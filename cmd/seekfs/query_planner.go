package main

import (
	"cmp"
	"container/heap"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type globalRecordID struct {
	volume int
	local  int
}

type globalQuerySnapshot struct {
	volumes    []*serviceVolumeIndex
	overlays   []*volumeSnapshot
	trace      *searchTrace
	overlaysOK bool
}

func newGlobalQuerySnapshot(volumes []*serviceVolumeIndex, trace *searchTrace) globalQuerySnapshot {
	overlays, ok := globalOverlaySnapshots(volumes)
	return globalQuerySnapshot{volumes: volumes, overlays: overlays, trace: trace, overlaysOK: ok}
}

type globalIDIterator interface {
	Next() (globalRecordID, bool)
	SeekGE(globalRecordID) (globalRecordID, bool)
	CountHint() int
}

func compareGlobalRecordID(a, b globalRecordID) int {
	if a.volume != b.volume {
		if a.volume < b.volume {
			return -1
		}
		return 1
	}
	if a.local != b.local {
		if a.local < b.local {
			return -1
		}
		return 1
	}
	return 0
}

type globalRecordIterator struct {
	volume int
	ids    []int
	pos    int
}

type globalPostingIterator struct {
	volume    int
	posting   postingCountCandidate
	trace     *searchTrace
	pos       int
	block     []uint32
	blockPos  int
	remaining int
}

type globalIDSliceIterator struct {
	ids []globalRecordID
	pos int
}

type globalHiddenIterator struct {
	base      globalIDIterator
	snapshots []*volumeSnapshot
}

type globalSubtreeFilterIterator struct {
	base    globalIDIterator
	volumes []*serviceVolumeIndex
	roots   map[int][]int
}

type globalPathTermFilterIterator struct {
	base    globalIDIterator
	volumes []*serviceVolumeIndex
	term    string
	roots   map[int][]globalSubtreeRange
	nameIDs map[int]map[int]struct{}
	fast    map[int]bool
}

type globalSubtreeRange struct {
	start uint32
	end   uint32
}

// globalMergeIterator merges already sorted global sources without copying
// their IDs. Sources are deliberately kept small: one source per volume or
// per boolean alternative, while the final verifier owns the only result
// materialization.
type globalMergeIterator struct {
	children []globalIDIterator
	current  []globalRecordID
	valid    []bool
}

type globalIntersectionIterator struct {
	a, b        globalIDIterator
	aID, bID    globalRecordID
	aOK, bOK    bool
	initialized bool
}

type globalExclusionIterator struct {
	include, exclude globalIDIterator
	iID, eID         globalRecordID
	iOK, eOK         bool
	initialized      bool
}

type globalSubtreeScanIterator struct {
	volume int
	vol    *serviceVolumeIndex
	roots  []int
	next   int
	end    int
}

type globalTypeFilterIterator struct {
	base    globalIDIterator
	volumes []*serviceVolumeIndex
	typ     string
}

type globalRankedEntry struct {
	entry   Entry
	rank    int
	volume  int
	tie     string
	overlay bool
}

func newGlobalRecordIterator(volume int, ids []int) globalRecordIterator {
	return globalRecordIterator{volume: volume, ids: ids}
}

func newGlobalIDSliceIterator(ids []globalRecordID) globalIDSliceIterator {
	return globalIDSliceIterator{ids: ids}
}

func newGlobalPostingIterator(volume int, posting postingCountCandidate) globalPostingIterator {
	return globalPostingIterator{volume: volume, posting: posting, remaining: posting.len()}
}

func newGlobalPostingIteratorWithTrace(volume int, posting postingCountCandidate, trace *searchTrace) globalPostingIterator {
	it := newGlobalPostingIterator(volume, posting)
	it.trace = trace
	return it
}

func newGlobalHiddenIterator(base globalIDIterator, snapshots []*volumeSnapshot) globalHiddenIterator {
	return globalHiddenIterator{base: base, snapshots: snapshots}
}

func newGlobalSubtreeFilterIterator(base globalIDIterator, volumes []*serviceVolumeIndex, roots []globalRecordID) globalSubtreeFilterIterator {
	byVolume := make(map[int][]int)
	for _, root := range roots {
		byVolume[root.volume] = append(byVolume[root.volume], root.local)
	}
	return globalSubtreeFilterIterator{base: base, volumes: volumes, roots: byVolume}
}

func newGlobalPathTermFilterIterator(base globalIDIterator, volumes []*serviceVolumeIndex, term string) globalPathTermFilterIterator {
	it := globalPathTermFilterIterator{base: base, volumes: volumes, term: term}
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil || len(vol.subtreeOrder) == 0 {
			continue
		}
		if vol.index.Derived.Postings != nil {
			if _, exists := vol.index.Derived.Postings[indexSectionPCMP]; exists {
				if it.fast == nil {
					it.fast = make(map[int]bool)
				}
				it.fast[volumeIndex] = true
			}
		}
		if !it.fast[volumeIndex] {
			if roots := vol.pathTermRootIDs(term); len(roots) > 0 {
				ranges := make([]globalSubtreeRange, 0, len(roots))
				for _, root := range roots {
					if root < 0 || root >= len(vol.subtreeStart) || root >= len(vol.subtreeEnd) {
						continue
					}
					start, end := vol.subtreeStart[root], vol.subtreeEnd[root]
					if start != ^uint32(0) && start < end {
						ranges = append(ranges, globalSubtreeRange{start: start, end: end})
					}
				}
				sort.Slice(ranges, func(i, j int) bool {
					if ranges[i].start == ranges[j].start {
						return ranges[i].end < ranges[j].end
					}
					return ranges[i].start < ranges[j].start
				})
				merged := ranges[:0]
				for _, current := range ranges {
					if len(merged) == 0 || current.start > merged[len(merged)-1].end {
						merged = append(merged, current)
						continue
					}
					if current.end > merged[len(merged)-1].end {
						merged[len(merged)-1].end = current.end
					}
				}
				if it.roots == nil {
					it.roots = make(map[int][]globalSubtreeRange)
				}
				it.roots[volumeIndex] = merged
			}
		}
		if !it.fast[volumeIndex] {
			if ids := vol.nameTermPosting(term); len(ids) > 0 {
				set := make(map[int]struct{}, len(ids))
				for _, id := range ids {
					set[id] = struct{}{}
				}
				if it.nameIDs == nil {
					it.nameIDs = make(map[int]map[int]struct{})
				}
				it.nameIDs[volumeIndex] = set
			}
		}
	}
	return it
}

func newGlobalMergeIterator(children ...globalIDIterator) *globalMergeIterator {
	filtered := make([]globalIDIterator, 0, len(children))
	for _, child := range children {
		if child != nil {
			filtered = append(filtered, child)
		}
	}
	it := &globalMergeIterator{
		children: filtered,
		current:  make([]globalRecordID, len(filtered)),
		valid:    make([]bool, len(filtered)),
	}
	for i, child := range filtered {
		it.current[i], it.valid[i] = child.Next()
	}
	return it
}

func (it *globalMergeIterator) CountHint() int {
	if it == nil {
		return 0
	}
	total := 0
	for i, child := range it.children {
		if it.valid[i] {
			total += child.CountHint()
		}
	}
	return total
}

func (it *globalMergeIterator) Next() (globalRecordID, bool) {
	if it == nil {
		return globalRecordID{}, false
	}
	best := -1
	for i, ok := range it.valid {
		if !ok || best >= 0 && compareGlobalRecordID(it.current[i], it.current[best]) >= 0 {
			continue
		}
		best = i
	}
	if best < 0 {
		return globalRecordID{}, false
	}
	out := it.current[best]
	for i, ok := range it.valid {
		if ok && compareGlobalRecordID(it.current[i], out) == 0 {
			it.current[i], it.valid[i] = it.children[i].Next()
		}
	}
	return out, true
}

func (it *globalMergeIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	if it == nil {
		return globalRecordID{}, false
	}
	for i, child := range it.children {
		if it.valid[i] && compareGlobalRecordID(it.current[i], target) < 0 {
			it.current[i], it.valid[i] = child.SeekGE(target)
		}
	}
	return it.Next()
}

func newGlobalIntersectionIterator(a, b globalIDIterator) *globalIntersectionIterator {
	return &globalIntersectionIterator{a: a, b: b}
}

func (it *globalIntersectionIterator) init() {
	if it.initialized {
		return
	}
	it.initialized = true
	if it.a != nil {
		it.aID, it.aOK = it.a.Next()
	}
	if it.b != nil {
		it.bID, it.bOK = it.b.Next()
	}
}

func (it *globalIntersectionIterator) CountHint() int {
	if it == nil || it.a == nil || it.b == nil {
		return 0
	}
	return minPositiveCountHint(it.a.CountHint(), it.b.CountHint())
}

func (it *globalIntersectionIterator) Next() (globalRecordID, bool) {
	if it == nil || it.a == nil || it.b == nil {
		return globalRecordID{}, false
	}
	it.init()
	for it.aOK && it.bOK {
		switch compareGlobalRecordID(it.aID, it.bID) {
		case 0:
			out := it.aID
			it.aID, it.aOK = it.a.Next()
			it.bID, it.bOK = it.b.Next()
			return out, true
		case -1:
			it.aID, it.aOK = it.a.SeekGE(it.bID)
		default:
			it.bID, it.bOK = it.b.SeekGE(it.aID)
		}
	}
	return globalRecordID{}, false
}

func (it *globalIntersectionIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	if it == nil || it.a == nil || it.b == nil {
		return globalRecordID{}, false
	}
	it.initialized = true
	it.aID, it.aOK = it.a.SeekGE(target)
	it.bID, it.bOK = it.b.SeekGE(target)
	return it.Next()
}

func newGlobalExclusionIterator(include, exclude globalIDIterator) *globalExclusionIterator {
	return &globalExclusionIterator{include: include, exclude: exclude}
}

func (it *globalExclusionIterator) init() {
	if it.initialized {
		return
	}
	it.initialized = true
	if it.include != nil {
		it.iID, it.iOK = it.include.Next()
	}
	if it.exclude != nil {
		it.eID, it.eOK = it.exclude.Next()
	}
}

func (it *globalExclusionIterator) CountHint() int {
	if it == nil || it.include == nil {
		return 0
	}
	return it.include.CountHint()
}

func (it *globalExclusionIterator) Next() (globalRecordID, bool) {
	if it == nil || it.include == nil {
		return globalRecordID{}, false
	}
	it.init()
	for it.iOK {
		for it.eOK && compareGlobalRecordID(it.eID, it.iID) < 0 {
			it.eID, it.eOK = it.exclude.SeekGE(it.iID)
		}
		out := it.iID
		if !it.eOK || compareGlobalRecordID(out, it.eID) != 0 {
			it.iID, it.iOK = it.include.Next()
			return out, true
		}
		it.iID, it.iOK = it.include.Next()
	}
	return globalRecordID{}, false
}

func (it *globalExclusionIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	if it == nil || it.include == nil {
		return globalRecordID{}, false
	}
	it.initialized = true
	it.iID, it.iOK = it.include.SeekGE(target)
	if it.exclude != nil {
		it.eID, it.eOK = it.exclude.SeekGE(target)
	}
	return it.Next()
}

func newGlobalSubtreeScanIterator(volume int, vol *serviceVolumeIndex, roots []int) *globalSubtreeScanIterator {
	end := 0
	if vol != nil && vol.index != nil {
		end = vol.index.compactRecordCount()
	}
	return &globalSubtreeScanIterator{volume: volume, vol: vol, roots: uniqueSortedInts(append([]int(nil), roots...)), end: end}
}

// newGlobalSubtreeIterator uses persisted SUBT intervals when available. The
// interval order is depth-first rather than local-ID order, so the bounded
// interval contents are sorted before being exposed to the ID-ordered set
// operators. This avoids a full compact-record scan; the old scan remains the
// compatibility fallback when SUBT is absent.
func newGlobalSubtreeIterator(volume int, vol *serviceVolumeIndex, roots []int) globalIDIterator {
	if vol == nil || vol.index == nil || len(vol.subtreeOrder) == 0 ||
		len(vol.subtreeStart) == 0 || len(vol.subtreeEnd) == 0 {
		return newGlobalSubtreeScanIterator(volume, vol, roots)
	}
	seen := make(map[int]struct{})
	for _, root := range uniqueSortedInts(append([]int(nil), roots...)) {
		if root < 0 || root >= len(vol.subtreeStart) || root >= len(vol.subtreeEnd) {
			return newGlobalSubtreeScanIterator(volume, vol, roots)
		}
		start, end := vol.subtreeStart[root], vol.subtreeEnd[root]
		if start == ^uint32(0) || start > end || int(end) > len(vol.subtreeOrder) {
			return newGlobalSubtreeScanIterator(volume, vol, roots)
		}
		for pos := start; pos < end; pos++ {
			id := int(vol.subtreeOrder[pos])
			if id < 0 || id >= vol.index.compactRecordCount() {
				return newGlobalSubtreeScanIterator(volume, vol, roots)
			}
			if !vol.index.compactRecord(id).Deleted {
				seen[id] = struct{}{}
			}
		}
	}
	ids := make([]int, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	it := newGlobalRecordIterator(volume, ids)
	return &it
}

func (it *globalSubtreeScanIterator) CountHint() int { return 0 }

func (it *globalSubtreeScanIterator) contains(local int) bool {
	if it == nil || it.vol == nil || it.vol.index == nil || local < 0 || local >= it.end {
		return false
	}
	for _, root := range it.roots {
		if root < 0 || root >= it.end {
			continue
		}
		if root < len(it.vol.subtreeStart) && local < len(it.vol.subtreeStart) &&
			len(it.vol.subtreeOrder) > 0 && root < len(it.vol.subtreeEnd) {
			pos := it.vol.subtreeStart[local]
			start, end := it.vol.subtreeStart[root], it.vol.subtreeEnd[root]
			if pos != ^uint32(0) && start != ^uint32(0) && pos >= start && pos < end {
				return true
			}
			continue
		}
		if it.vol.isDescendantOrSelf(local, root) {
			return true
		}
	}
	return false
}

func (it *globalSubtreeScanIterator) Next() (globalRecordID, bool) {
	if it == nil || it.vol == nil || it.vol.index == nil {
		return globalRecordID{}, false
	}
	for it.next < it.end {
		local := it.next
		it.next++
		rec := it.vol.index.compactRecord(local)
		if !rec.Deleted && it.contains(local) {
			return globalRecordID{volume: it.volume, local: local}, true
		}
	}
	return globalRecordID{}, false
}

func (it *globalSubtreeScanIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	if it == nil {
		return globalRecordID{}, false
	}
	if target.volume > it.volume {
		it.next = it.end
		return globalRecordID{}, false
	}
	if target.volume == it.volume && target.local > it.next {
		it.next = target.local
	}
	if target.volume < it.volume {
		it.next = 0
	}
	return it.Next()
}

func (it *globalTypeFilterIterator) CountHint() int {
	if it == nil || it.base == nil {
		return 0
	}
	return it.base.CountHint()
}

func (it *globalTypeFilterIterator) keep(id globalRecordID) bool {
	if id.volume < 0 || id.volume >= len(it.volumes) || id.local < 0 {
		return false
	}
	vol := it.volumes[id.volume]
	if vol == nil || vol.index == nil || id.local >= vol.index.compactRecordCount() {
		return false
	}
	isDir := vol.index.compactRecord(id.local).Mode&uint32(os.ModeDir) != 0
	return (it.typ == "dir" && isDir) || (it.typ == "file" && !isDir)
}

func (it *globalTypeFilterIterator) Next() (globalRecordID, bool) {
	if it == nil || it.base == nil {
		return globalRecordID{}, false
	}
	for id, ok := it.base.Next(); ok; id, ok = it.base.Next() {
		if it.keep(id) {
			return id, true
		}
	}
	return globalRecordID{}, false
}

func (it *globalTypeFilterIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	if it == nil || it.base == nil {
		return globalRecordID{}, false
	}
	if id, ok := it.base.SeekGE(target); ok && it.keep(id) {
		return id, true
	}
	return it.Next()
}

func globalPathTermIterator(volumes []*serviceVolumeIndex, term string) (globalIDIterator, bool) {
	if term == "" {
		return nil, false
	}
	children := make([]globalIDIterator, 0, len(volumes))
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil || !vol.pathComponentPostingAvailable(term) {
			return nil, false
		}
		if len(volumes) == 1 && vol.index.compactRecordCount() > serviceResidentChildRangeMaxRecords &&
			len(vol.subtreeOrder) == 0 && len(vol.childOffsets) > 0 {
			// Without persisted subtree order, estimating a broad component
			// would walk the entire child graph. Let the bounded single-volume
			// planner choose its indexed candidate source instead.
			return nil, false
		}
		// When persisted subtree order is absent, do not walk every compact
		// record and repeatedly test its parent chain. The exact path posting
		// traverses the child ranges once and is cached, preserving a global
		// set source without reintroducing a per-volume terminal search.
		if len(vol.subtreeOrder) == 0 && (len(vol.childOffsets) > 0 || vol.children != nil) {
			children = append(children, &globalRecordIterator{volume: volumeIndex, ids: vol.pathTermPosting(term)})
			continue
		}
		// Old indexes without child/subtree metadata retain the exact existing
		// scan semantics. They are a bounded compatibility fallback, not a
		// partially-correct posting source.
		if !vol.hasDescendantIndex() && vol.children == nil {
			children = append(children, &globalRecordIterator{volume: volumeIndex, ids: vol.pathTermPosting(term)})
			continue
		}
		nameIDs := vol.nameTermPosting(term)
		parts := make([]globalIDIterator, 0, 2)
		if len(nameIDs) > 0 {
			parts = append(parts, &globalRecordIterator{volume: volumeIndex, ids: nameIDs})
		}
		roots := vol.pathTermRootIDs(term)
		if len(roots) > 0 {
			if len(volumes) == 1 {
				for _, root := range roots {
					if vol.estimatedDescendantOrSelfCount(root) > serviceComponentTrigramExpansionMaxIDs {
						// Let the single-volume candidate planner choose its
						// bounded source; never eagerly materialize a huge
						// subtree just to return a small top-N page.
						return nil, false
					}
				}
			}
			parts = append(parts, newGlobalSubtreeIterator(volumeIndex, vol, roots))
		}
		if len(parts) == 0 {
			children = append(children, &globalRecordIterator{volume: volumeIndex, ids: nil})
		} else {
			children = append(children, newGlobalMergeIterator(parts...))
		}
	}
	return newGlobalMergeIterator(children...), true
}

func sortGlobalPathProbeTerms(volumes []*serviceVolumeIndex, terms []string) []string {
	probes := append([]string(nil), terms...)
	sort.SliceStable(probes, func(i, j int) bool {
		return estimateGlobalPathTerm(volumes, probes[i]) < estimateGlobalPathTerm(volumes, probes[j])
	})
	return probes
}

func estimateGlobalPathTerm(volumes []*serviceVolumeIndex, term string) int {
	if term == "" {
		return 0
	}
	total := 0
	for _, vol := range volumes {
		if vol == nil || vol.index == nil {
			continue
		}
		candidate, ok := vol.componentPostingCountCandidate(term)
		if ok && candidate.mapped {
			// Root postings can themselves be large (e.g. Users).  Use their
			// count as a conservative selectivity estimate instead of decoding
			// the entire root list merely to choose probe order.
			if candidate.count > 10_000 {
				total += candidate.count
				continue
			}
			for _, root32 := range candidate.materialize() {
				root := int(root32)
				if root >= 0 && root < len(vol.subtreeStart) && root < len(vol.subtreeEnd) {
					start, end := vol.subtreeStart[root], vol.subtreeEnd[root]
					if start != ^uint32(0) && start <= end {
						total += int(end - start)
					}
				}
			}
			continue
		}
		if ok {
			total += candidate.len()
			continue
		}
		total += len(vol.nameTermPosting(term))
	}
	return total
}

func globalUnderIterator(volumes []*serviceVolumeIndex, under string) (globalIDIterator, bool) {
	roots, ok := globalUnderRoots(volumes, under)
	if !ok {
		return nil, false
	}
	byVolume := make(map[int][]int)
	for _, root := range roots {
		byVolume[root.volume] = append(byVolume[root.volume], root.local)
	}
	children := make([]globalIDIterator, 0, len(byVolume))
	for volume, volumeRoots := range byVolume {
		children = append(children, newGlobalSubtreeIterator(volume, volumes[volume], volumeRoots))
	}
	return newGlobalMergeIterator(children...), true
}

func globalParentIterator(volumes []*serviceVolumeIndex, parent string) (globalIDIterator, bool) {
	if parent == "" || strings.ContainsAny(parent, `\/:*?[]`) {
		return nil, false
	}
	children := make([]globalIDIterator, 0, len(volumes))
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			return nil, false
		}
		children = append(children, &globalRecordIterator{volume: volumeIndex, ids: vol.parentIDs(parent)})
	}
	return newGlobalMergeIterator(children...), true
}

func globalAttributeIterator(volumes []*serviceVolumeIndex, filters []uint32) (globalIDIterator, bool) {
	if len(filters) == 0 {
		return nil, false
	}
	var current globalIDIterator
	for _, mask := range filters {
		children := make([]globalIDIterator, 0, len(volumes))
		for volumeIndex, vol := range volumes {
			if vol == nil || vol.index == nil {
				return nil, false
			}
			ids, ok := vol.attrIDsForMask(mask)
			if !ok {
				return nil, false
			}
			children = append(children, &globalRecordIterator{volume: volumeIndex, ids: ids})
		}
		next := newGlobalMergeIterator(children...)
		if current == nil {
			current = next
		} else {
			current = newGlobalIntersectionIterator(current, next)
		}
	}
	return current, current != nil
}

func globalExtensionIterator(volumes []*serviceVolumeIndex, ext string, trace *searchTrace) (globalIDIterator, bool) {
	if ext == "" {
		return nil, false
	}
	children := make([]globalIDIterator, 0, len(volumes))
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			return nil, false
		}
		posting, ok := vol.extPostingCountCandidate(ext)
		if !ok {
			trace.addDeclineForVolume("global-ext:missing-posting", vol.volume)
			return nil, false
		}
		postingIt := newGlobalPostingIteratorWithTrace(volumeIndex, posting, trace)
		children = append(children, &postingIt)
	}
	return newGlobalMergeIterator(children...), true
}

func globalComponentSubqueryIterator(volumes []*serviceVolumeIndex, pq parsedQuery, trace *searchTrace) (globalIDIterator, bool) {
	var current globalIDIterator
	intersect := func(next globalIDIterator) {
		if current == nil {
			current = next
		} else {
			current = newGlobalIntersectionIterator(current, next)
		}
	}
	if terms := nonVolumeTerms(pq.Terms); len(terms) > 0 {
		probes := sortGlobalPathProbeTerms(volumes, pathPlanProbeTerms(terms))
		if len(probes) == 0 {
			return nil, false
		}
		if estimateGlobalPathTerm(volumes, probes[0]) > serviceComponentMultiTermScanMaxIDs {
			return nil, false
		}
		it, ok := globalPathTermIterator(volumes, probes[0])
		if !ok {
			return nil, false
		}
		current = it
		for _, term := range probes[1:] {
			filtered := newGlobalPathTermFilterIterator(current, volumes, term)
			current = &filtered
		}
	}
	for _, dir := range pq.Dirs {
		it, ok := globalPathTermIterator(volumes, dir)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	if globalRegexLiteralSupported(pq) {
		it, ok := globalPathTermIterator(volumes, pq.RegexTerms[0])
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	for _, parent := range pq.Parents {
		it, ok := globalParentIterator(volumes, parent)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	if len(pq.AttrFilters) > 0 {
		it, ok := globalAttributeIterator(volumes, pq.AttrFilters)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	for _, extFilter := range mustGlobalExtFilters(pq) {
		it, ok := globalExtensionIterator(volumes, extFilter.ext, trace)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	if pq.Type != "" {
		if current == nil {
			return nil, false
		}
		current = &globalTypeFilterIterator{base: current, volumes: volumes, typ: pq.Type}
	}
	return current, current != nil
}

func mustGlobalExtFilters(pq parsedQuery) []globalExtFilter {
	filters, ok := globalExtPostingFilters(pq)
	if !ok {
		return nil
	}
	return filters
}

func globalComponentQueryIterator(volumes []*serviceVolumeIndex, pq parsedQuery, trace *searchTrace) (globalIDIterator, bool) {
	var current globalIDIterator
	intersect := func(next globalIDIterator) {
		if current == nil {
			current = next
		} else {
			current = newGlobalIntersectionIterator(current, next)
		}
	}
	if pq.Under != "" {
		under, ok := globalUnderIterator(volumes, pq.Under)
		if !ok {
			return nil, false
		}
		current = under
	}
	if terms := nonVolumeTerms(pq.Terms); len(terms) > 0 {
		probes := sortGlobalPathProbeTerms(volumes, pathPlanProbeTerms(terms))
		if len(probes) == 0 {
			return nil, false
		}
		if estimateGlobalPathTerm(volumes, probes[0]) > serviceComponentMultiTermScanMaxIDs {
			// A multi-term plan drives off the smallest probe.  If that probe
			// is already unbounded, materializing the intersection would build
			// a slice proportional to the whole volume.  Decline so the caller
			// routes to the bounded exhaustive scan instead.
			return nil, false
		}
		it, ok := globalPathTermIterator(volumes, probes[0])
		if !ok {
			return nil, false
		}
		intersect(it)
		for _, term := range probes[1:] {
			filtered := newGlobalPathTermFilterIterator(current, volumes, term)
			current = &filtered
		}
	}
	for _, group := range pq.OrGroups {
		alternatives := make([]globalIDIterator, 0, len(group))
		for _, alt := range group {
			it, ok := globalComponentSubqueryIterator(volumes, alt, trace)
			if !ok {
				return nil, false
			}
			alternatives = append(alternatives, it)
		}
		intersect(newGlobalMergeIterator(alternatives...))
	}
	for _, dir := range pq.Dirs {
		it, ok := globalPathTermIterator(volumes, dir)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	if globalRegexLiteralSupported(pq) {
		it, ok := globalPathTermIterator(volumes, pq.RegexTerms[0])
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	for _, parent := range pq.Parents {
		it, ok := globalParentIterator(volumes, parent)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	if len(pq.AttrFilters) > 0 {
		it, ok := globalAttributeIterator(volumes, pq.AttrFilters)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	for _, extFilter := range mustGlobalExtFilters(pq) {
		it, ok := globalExtensionIterator(volumes, extFilter.ext, trace)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	if pq.Type != "" {
		if current == nil {
			return nil, false
		}
		current = &globalTypeFilterIterator{base: current, volumes: volumes, typ: pq.Type}
	}
	for _, neg := range pq.NotGroups {
		if current == nil {
			all := make([]globalIDIterator, 0, len(volumes))
			for volumeIndex, vol := range volumes {
				if vol == nil || vol.index == nil {
					return nil, false
				}
				ids := make([]int, vol.index.compactRecordCount())
				for id := range ids {
					ids[id] = id
				}
				all = append(all, &globalRecordIterator{volume: volumeIndex, ids: ids})
			}
			current = newGlobalMergeIterator(all...)
		}
		excluded, ok := globalComponentSubqueryIterator(volumes, neg, trace)
		if !ok {
			return nil, false
		}
		current = newGlobalExclusionIterator(current, excluded)
	}
	return current, current != nil
}

func globalSimplePathORTerms(pq parsedQuery) ([]string, bool) {
	if !pq.MatchPath || len(pq.OrGroups) != 1 || len(pq.NotGroups) != 0 || pq.Type != "" ||
		pq.Under != "" || pq.Exists || pq.HasModAfter || len(pq.Exts) != 0 || len(pq.Dirs) != 0 ||
		len(pq.Globs) != 0 || len(pq.Regexps) != 0 || len(pq.RegexTerms) != 0 || len(pq.Parents) != 0 ||
		len(pq.SizeFilters) != 0 || len(pq.DateFilters) != 0 || len(pq.AttrFilters) != 0 ||
		pq.CaseSensitive || len(pq.OrGroups[0]) == 0 {
		return nil, false
	}
	if len(nonVolumeTerms(pq.Terms)) != 0 {
		return nil, false
	}
	terms := make([]string, 0, len(pq.OrGroups[0]))
	for _, alt := range pq.OrGroups[0] {
		altTerms := nonVolumeTerms(alt.Terms)
		if len(altTerms) != 1 || len(alt.OrGroups) != 0 || len(alt.NotGroups) != 0 ||
			len(alt.Dirs) != 0 || len(alt.Globs) != 0 || len(alt.Regexps) != 0 ||
			len(alt.RegexTerms) != 0 || len(alt.Parents) != 0 || len(alt.Exts) != 0 ||
			len(alt.SizeFilters) != 0 || len(alt.DateFilters) != 0 || len(alt.AttrFilters) != 0 ||
			alt.Type != "" || alt.Under != "" || alt.Exists || alt.HasModAfter {
			return nil, false
		}
		terms = append(terms, altTerms[0])
	}
	return terms, true
}

func globalSimplePathORTopIDs(volumes []*serviceVolumeIndex, pq parsedQuery, limit int) ([]globalRecordID, bool) {
	if pq.Cancel != nil {
		return nil, false
	}
	terms, ok := globalSimplePathORTerms(pq)
	if !ok || limit <= 0 {
		return nil, false
	}
	seen := make(map[globalRecordID]struct{}, len(volumes)*limit*len(terms))
	out := make([]globalRecordID, 0, len(volumes)*limit*len(terms))
	for volumeIndex, vol := range volumes {
		if queryCanceled(pq) {
			return nil, false
		}
		if vol == nil || vol.index == nil {
			return nil, false
		}
		ranks := vol.rankForQuery(pq)
		for _, term := range terms {
			if queryCanceled(pq) {
				return nil, false
			}
			if !vol.pathComponentPostingAvailable(term) {
				return nil, false
			}
			for _, local := range topCandidateIDsByRank(vol.pathTermPosting(term), limit, vol.index, ranks) {
				id := globalRecordID{volume: volumeIndex, local: local}
				if _, exists := seen[id]; exists {
					continue
				}
				seen[id] = struct{}{}
				out = append(out, id)
			}
		}
	}
	return out, true
}

func globalSimplePathORCount(volumes []*serviceVolumeIndex, pq parsedQuery) (int, bool) {
	if pq.Cancel != nil {
		return 0, false
	}
	terms, ok := globalSimplePathORTerms(pq)
	if !ok {
		return 0, false
	}
	children := make([]globalIDIterator, 0, len(volumes)*len(terms))
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			return 0, false
		}
		for _, term := range terms {
			if !vol.pathComponentPostingAvailable(term) {
				return 0, false
			}
			it := newGlobalRecordIterator(volumeIndex, vol.pathTermPosting(term))
			children = append(children, &it)
		}
	}
	merged := newGlobalMergeIterator(children...)
	count, _, err := countGlobalVerifiedIterator(merged, volumes, nil, pq)
	return count, err == nil
}

func globalComponentTopIDs(volumes []*serviceVolumeIndex, pq parsedQuery, limit int) ([]globalRecordID, bool) {
	term, ok := globalExactPathComponentTerm(pq)
	if limit <= 0 || !ok {
		return nil, false
	}
	out := make([]globalRecordID, 0, len(volumes)*limit)
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			return nil, false
		}
		var ids []int
		var ok bool
		if len(vol.subtreeOrder) == 0 && (len(vol.childOffsets) > 0 || vol.children != nil) {
			// v8-compatible low-memory indexes retain exact child ranges but
			// omit SUBT rank metadata. The cached exact path posting is still a
			// complete set source; rank only that set instead of materializing
			// every match through the generic component iterator.
			ids = topCandidateIDsByRank(vol.pathTermPosting(term), limit, vol.index, vol.rankForQuery(pq))
			ok = true
		} else {
			ids, ok = vol.mappedComponentTopPosting(term, limit, pq)
		}
		if !ok {
			return nil, false
		}
		for _, id := range ids {
			out = append(out, globalRecordID{volume: volumeIndex, local: id})
		}
	}
	return out, true
}

func globalExactPathComponentTerm(pq parsedQuery) (string, bool) {
	terms := nonVolumeTerms(pq.Terms)
	if !pq.MatchPath || len(terms) != 1 || pq.CaseSensitive ||
		pq.Type != "" || pq.Under != "" || pq.Exists || pq.HasModAfter ||
		len(pq.Exts) != 0 || len(pq.Dirs) != 0 || len(pq.Globs) != 0 ||
		len(pq.Regexps) != 0 || len(pq.RegexTerms) != 0 || len(pq.Parents) != 0 ||
		len(pq.SizeFilters) != 0 || len(pq.DateFilters) != 0 || len(pq.AttrFilters) != 0 ||
		len(pq.OrGroups) != 0 || len(pq.NotGroups) != 0 || pq.RootBias != "" || pq.CWDBias != "" {
		return "", false
	}
	return terms[0], true
}

func globalExtPostingIDs(volumes []*serviceVolumeIndex, ext string, limit int, trace *searchTrace) ([]globalRecordID, bool) {
	out := make([]globalRecordID, 0)
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			trace.addDeclineForVolume("global-ext:missing-volume", "")
			return nil, false
		}
		posting, ok := vol.extPostingCountCandidate(ext)
		if !ok {
			trace.addDeclineForVolume("global-ext:missing-posting", vol.volume)
			return nil, false
		}
		it := newGlobalPostingIteratorWithTrace(volumeIndex, posting, trace)
		remaining := 0
		if limit > 0 {
			remaining = limit - len(out)
			if remaining <= 0 {
				return out, true
			}
		}
		out = append(out, collectGlobalIterator(&it, remaining)...)
	}
	return out, true
}

func globalComponentRootIDs(volumes []*serviceVolumeIndex, component string, limit int) ([]globalRecordID, bool) {
	if component == "" {
		return nil, false
	}
	out := make([]globalRecordID, 0)
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil || !vol.pathComponentPostingAvailable(component) {
			return nil, false
		}
		for _, id := range vol.pathComponentRootIDs(component) {
			out = append(out, globalRecordID{volume: volumeIndex, local: id})
			if limit > 0 && len(out) >= limit {
				return out, true
			}
		}
	}
	sortGlobalRecordIDs(out)
	return out, true
}

func globalSubtreeIDs(volumes []*serviceVolumeIndex, roots []globalRecordID, limit int) ([]globalRecordID, bool) {
	if len(roots) == 0 {
		return nil, true
	}
	out := make([]globalRecordID, 0)
	seenByVolume := make(map[int]map[int]struct{})
	for _, root := range roots {
		if root.volume < 0 || root.volume >= len(volumes) {
			return nil, false
		}
		vol := volumes[root.volume]
		if vol == nil || vol.index == nil || root.local < 0 || root.local >= vol.index.compactRecordCount() {
			return nil, false
		}
		seen := seenByVolume[root.volume]
		if seen == nil {
			seen = make(map[int]struct{})
			seenByVolume[root.volume] = seen
		}
		var descendants []int
		if limit > 0 {
			descendants = vol.underDescendantsLimited(root.local, limit-len(out))
		} else {
			descendants = vol.underDescendants(root.local)
		}
		for _, id := range descendants {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, globalRecordID{volume: root.volume, local: id})
			if limit > 0 && len(out) >= limit {
				return out, true
			}
		}
	}
	sortGlobalRecordIDs(out)
	return out, true
}

func sortGlobalRecordIDs(ids []globalRecordID) {
	slices.SortFunc(ids, compareGlobalRecordID)
}

func searchServiceVolumesGlobalExtOnly(volumes []*serviceVolumeIndex, opts queryOptions, countOnly bool) ([]Entry, bool, error) {
	return searchServiceVolumesGlobalExtOnlySnapshot(newGlobalQuerySnapshot(volumes, opts.Trace), opts, countOnly)
}

func searchServiceVolumesGlobalExtOnlySnapshot(snapshot globalQuerySnapshot, opts queryOptions, countOnly bool) ([]Entry, bool, error) {
	volumes := snapshot.volumes
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, true, err
	}
	globalEnabled := globalPlannerEnabled()
	if !globalEnabled && !globalExtDefaultSupported(pq) {
		return nil, false, nil
	}
	if !globalExtOnlySupported(pq) {
		if globalEnabled {
			opts.Trace.replaceDecline("global-ext:unsupported-query")
		}
		return nil, false, nil
	}
	extFilters, _ := globalExtPostingFilters(pq)
	extFilter := extFilters[0]
	limit := normalizedLimit(opts.Limit, false)
	snapshots := snapshot.overlays
	if !snapshot.overlaysOK {
		opts.Trace.replaceDecline("global-ext:overlay-snapshot-missing")
		return nil, false, nil
	}
	if countOnly {
		ids, ok := globalExtPostingIDs(volumes, extFilter.ext, 0, opts.Trace)
		if !ok {
			return nil, false, nil
		}
		ids = filterGlobalIDsHidden(ids, snapshots)
		ranked, err := rankedEntriesFromGlobalIDs(volumes, ids, pq)
		if err != nil {
			return nil, true, err
		}
		results := globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, ranked, pq, 0))
		opts.Trace.setPlannerMode("global-ext")
		opts.Trace.addTerm(traceTerm{
			Term:      extFilter.ext,
			Kind:      "extension",
			Source:    "global:" + extFilter.source,
			CountHint: len(ids),
			Exact:     true,
		})
		opts.Trace.setSource("global:"+extFilter.source, len(ids))
		opts.Trace.setComplete(true)
		return results, true, nil
	}
	if pq.SortColumn != "" {
		ids, ok := globalExtPostingIDs(volumes, extFilter.ext, 0, opts.Trace)
		if !ok {
			return nil, false, nil
		}
		ids = filterGlobalIDsHidden(ids, snapshots)
		ranked, err := rankedEntriesFromGlobalIDs(volumes, ids, pq)
		if err != nil {
			return nil, true, err
		}
		results := globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, ranked, pq, limit))
		opts.Trace.setPlannerMode("global-ext")
		opts.Trace.addTerm(traceTerm{
			Term:      extFilter.ext,
			Kind:      "extension",
			Source:    "global:" + extFilter.source,
			CountHint: len(ids),
			Exact:     true,
		})
		opts.Trace.setSource("global:"+extFilter.source, len(ids))
		opts.Trace.setComplete(true)
		return results, true, nil
	}
	if !globalVolumesHaveRankForQuery(volumes, pq) {
		if globalSnapshotsHaveOverlayRecords(snapshots) {
			opts.Trace.replaceDecline("global-ext:rankless-overlay")
			return nil, false, nil
		}
		ids, ok := globalExtPostingIDs(volumes, extFilter.ext, 0, opts.Trace)
		if !ok {
			return nil, false, nil
		}
		ids = filterGlobalIDsHidden(ids, snapshots)
		results, err := entriesFromGlobalIDs(volumes, ids, pq)
		if err != nil {
			return nil, true, err
		}
		sortSearchAllEntries(results, pq)
		if limit > 0 && len(results) > limit {
			results = results[:limit]
		}
		opts.Trace.setPlannerMode("global-ext")
		opts.Trace.addTerm(traceTerm{
			Term:      extFilter.ext,
			Kind:      "extension",
			Source:    "global:" + extFilter.source,
			CountHint: len(ids),
			Exact:     true,
		})
		opts.Trace.setSource("global:"+extFilter.source, len(ids))
		opts.Trace.setComplete(true)
		return results, true, nil
	}

	ids := make([]globalRecordID, 0, limit*len(volumes))
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			opts.Trace.addDeclineForVolume("global-ext:missing-volume", "")
			return nil, false, nil
		}
		if err := checkQueryCapabilities(pq, vol.index); err != nil {
			return nil, true, err
		}
		posting, ok := vol.extPostingCountCandidate(extFilter.ext)
		if !ok {
			opts.Trace.addDeclineForVolume("global-ext:missing-posting", vol.volume)
			return nil, false, nil
		}
		if !globalSnapshotsHaveHidden(snapshots) {
			if localIDs, ok := vol.extTopPosting(extFilter.ext, limit, pq); ok {
				for _, local := range localIDs {
					ids = append(ids, globalRecordID{volume: volumeIndex, local: local})
				}
				continue
			}
		}
		it := newGlobalPostingIteratorWithTrace(volumeIndex, posting, opts.Trace)
		var source globalIDIterator = &it
		if globalSnapshotsHaveHidden(snapshots) {
			hiddenIt := newGlobalHiddenIterator(&it, snapshots)
			source = &hiddenIt
		}
		rankOf := candidateRanker(vol.index, vol.rankForQuery(pq))
		ids = append(ids, collectGlobalTopN([]globalIDIterator{source}, limit, func(id globalRecordID) int {
			return rankOf(id.local)
		})...)
	}
	ranked, err := rankedEntriesFromGlobalIDs(volumes, ids, pq)
	if err != nil {
		return nil, true, err
	}
	results := globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, ranked, pq, limit))
	opts.Trace.setPlannerMode("global-ext")
	opts.Trace.addTerm(traceTerm{
		Term:      extFilter.ext,
		Kind:      "extension",
		Source:    "global:" + extFilter.source,
		CountHint: len(ids),
		Exact:     true,
	})
	opts.Trace.setSource("global:"+extFilter.source, len(ids))
	opts.Trace.setComplete(true)
	return results, true, nil
}

func searchServiceVolumesGlobalComponentsOnly(volumes []*serviceVolumeIndex, opts queryOptions, countOnly bool) ([]Entry, bool, error) {
	return searchServiceVolumesGlobalComponentsOnlySnapshot(newGlobalQuerySnapshot(volumes, opts.Trace), opts, countOnly)
}

func searchServiceVolumesGlobalComponentsOnlySnapshot(snapshot globalQuerySnapshot, opts queryOptions, countOnly bool) ([]Entry, bool, error) {
	volumes := snapshot.volumes
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, true, err
	}
	terms := nonVolumeTerms(pq.Terms)
	globalEnabled := globalPlannerEnabled()
	if !globalEnabled && !globalComponentDefaultSupported(pq, terms) {
		return nil, false, nil
	}
	if !globalComponentQuerySupported(pq, terms) {
		opts.Trace.replaceDecline("global-components:unsupported-query")
		return nil, false, nil
	}
	snapshots := snapshot.overlays
	if !snapshot.overlaysOK {
		opts.Trace.replaceDecline("global-components:overlay-snapshot-missing")
		return nil, false, nil
	}
	for _, vol := range volumes {
		if vol == nil || vol.index == nil {
			opts.Trace.replaceDecline("global-components:missing-volume")
			return nil, false, nil
		}
		if err := checkQueryCapabilities(pq, vol.index); err != nil {
			return nil, true, err
		}
	}
	limit := normalizedLimit(opts.Limit, false)
	if countOnly {
		limit = 0
	}
	if !countOnly && (len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0) {
		if ids, ok := globalSimplePathORTopIDs(volumes, pq, limit); ok && !globalSnapshotsHaveHidden(snapshots) && !globalSnapshotsHaveOverlayRecords(snapshots) {
			topIt := newGlobalIDSliceIterator(ids)
			base, verified, err := collectGlobalVerifiedTopN(&topIt, volumes, snapshots, pq, limit)
			if err != nil {
				return nil, true, err
			}
			if opts.Trace != nil {
				opts.Trace.ComponentRecordsVerified += verified
				opts.Trace.setPlannerMode("global-components")
				opts.Trace.setSource("global:boolean-persisted-top", len(ids))
				opts.Trace.setComplete(true)
			}
			return globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, base, pq, limit)), true, nil
		}
		componentIt, ok := globalComponentQueryIterator(volumes, pq, opts.Trace)
		if !ok {
			if opts.Trace == nil || opts.Trace.Decline == "" {
				opts.Trace.replaceDecline("global-components:boolean-missing-source")
			}
			return nil, false, nil
		}
		base, verified, err := collectGlobalVerifiedTopN(componentIt, volumes, snapshots, pq, limit)
		if err != nil {
			return nil, true, err
		}
		if opts.Trace != nil {
			opts.Trace.ComponentRecordsVerified += verified
			opts.Trace.setPlannerMode("global-components")
			opts.Trace.setSource("global:boolean-iterator", len(base))
			opts.Trace.setComplete(true)
		}
		results := globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, base, pq, limit))
		return results, true, nil
	}
	var ids []globalRecordID
	topUsed := false
	if !countOnly && !globalSnapshotsHaveHidden(snapshots) {
		if topIDs, ok := globalComponentTopIDs(volumes, pq, limit); ok {
			ids = topIDs
			topUsed = true
		}
	}
	if !topUsed {
		componentIt, ok := globalComponentQueryIterator(volumes, pq, opts.Trace)
		if !ok {
			if opts.Trace == nil || opts.Trace.Decline == "" {
				opts.Trace.replaceDecline("global-components:missing-source")
			}
			return nil, false, nil
		}
		canceled := false
		if globalSnapshotsHaveHidden(snapshots) {
			hidden := newGlobalHiddenIterator(componentIt, snapshots)
			ids, canceled = collectGlobalIteratorCancelable(&hidden, 0, func() bool { return queryCanceled(pq) })
		} else {
			ids, canceled = collectGlobalIteratorCancelable(componentIt, 0, func() bool { return queryCanceled(pq) })
		}
		if canceled {
			return nil, true, errQueryCanceled
		}
	}
	ids = filterGlobalIDsByType(volumes, ids, pq.Type)
	ranked, err := rankedEntriesFromGlobalIDs(volumes, ids, pq)
	if err != nil {
		return nil, true, err
	}
	results := globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, ranked, pq, limit))
	opts.Trace.setPlannerMode("global-components")
	addGlobalComponentTraceTerms(opts.Trace, pq, len(ids))
	if topUsed {
		opts.Trace.setSource("global:component-top", len(ids))
	} else {
		opts.Trace.setSource("global:components", len(ids))
	}
	opts.Trace.setComplete(true)
	return results, true, nil
}

func searchServiceVolumesGlobalBoundedFallback(volumes []*serviceVolumeIndex, opts queryOptions, countOnly bool) ([]Entry, bool, error) {
	return searchServiceVolumesGlobalBoundedFallbackSnapshot(newGlobalQuerySnapshot(volumes, opts.Trace), opts, countOnly)
}

func searchServiceVolumesGlobalBoundedFallbackSnapshot(snapshot globalQuerySnapshot, opts queryOptions, countOnly bool) ([]Entry, bool, error) {
	volumes := snapshot.volumes
	if len(volumes) < 2 {
		return nil, false, nil
	}
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, true, err
	}
	terms := nonVolumeTerms(pq.Terms)
	if !globalPlannerEnabled() && !globalExtDefaultSupported(pq) && !globalComponentDefaultSupported(pq, terms) && !globalBoundedFallbackDefaultSupported(pq) {
		return nil, false, nil
	}
	snapshots := snapshot.overlays
	if !snapshot.overlaysOK {
		opts.Trace.replaceDecline("global-bounded-scan:overlay-snapshot-missing")
		return nil, false, nil
	}
	limit := normalizedLimit(opts.Limit, countOnly)
	if countOnly {
		limit = 0
	}
	ids := make([]globalRecordID, 0, 1024)
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			opts.Trace.replaceDecline("global-bounded-scan:missing-volume")
			return nil, false, nil
		}
		if err := checkQueryCapabilities(pq, vol.index); err != nil {
			return nil, true, err
		}
		volumePQ := pq
		volumePQ.Limit = limit
		volumePQ.CountOnly = countOnly
		dropSatisfiedVolumeTerms(&volumePQ, vol.index.Volume)
		var localIDs []int
		var ok bool
		orderReady := len(vol.orderForQuery(volumePQ)) >= vol.index.compactRecordCount()
		if countOnly || !orderReady || volumePQ.RootBias != "" || volumePQ.CWDBias != "" {
			volumePQ.Limit = 0
			volumePQ.CountOnly = true
			localIDs, ok = vol.boundedScanCandidates(volumePQ)
		} else {
			hidden := hiddenBaseIDs{}
			if volumeIndex < len(snapshots) && snapshots[volumeIndex] != nil {
				hidden = hiddenBaseIDs{tombstone: snapshots[volumeIndex].tombstoneIDs, shadowed: snapshots[volumeIndex].shadowedIDs}
			}
			localIDs, ok = vol.boundedScanCandidatesHiddenTop(volumePQ, hidden, limit)
		}
		if !ok {
			opts.Trace.replaceDecline("global-bounded-scan:canceled")
			return nil, false, nil
		}
		for _, local := range localIDs {
			ids = append(ids, globalRecordID{volume: volumeIndex, local: local})
		}
	}
	ids = filterGlobalIDsHidden(ids, snapshots)
	ranked, err := rankedEntriesFromGlobalIDs(volumes, ids, pq)
	if err != nil {
		return nil, true, err
	}
	results := globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, ranked, pq, limit))
	opts.Trace.setFallback("global-bounded-scan")
	opts.Trace.setPlannerMode("global-bounded-scan")
	opts.Trace.setSource("global:bounded-scan", len(ids))
	opts.Trace.setComplete(true)
	return results, true, nil
}

func countServiceVolumesGlobalOnly(volumes []*serviceVolumeIndex, opts queryOptions) (int, bool, error) {
	return countServiceVolumesGlobalOnlySnapshot(newGlobalQuerySnapshot(volumes, opts.Trace), opts)
}

// globalDirTermCountShape reports whether pq is a bare `type:dir <term>`
// query: exactly one non-volume term and no other filter.  These shapes used
// to force the global bounded fallback into a full per-volume record scan.
func globalDirTermCountShape(pq parsedQuery) (string, bool) {
	if pq.Type != "dir" {
		return "", false
	}
	terms := nonVolumeTerms(pq.Terms)
	if len(terms) != 1 || len(pq.Exts) != 0 || len(pq.Dirs) != 0 || len(pq.Globs) != 0 ||
		len(pq.Regexps) != 0 || len(pq.RegexTerms) != 0 || len(pq.Parents) != 0 ||
		pq.Under != "" || pq.Exists || pq.HasModAfter || len(pq.SizeFilters) != 0 ||
		len(pq.DateFilters) != 0 || len(pq.AttrFilters) != 0 || len(pq.OrGroups) != 0 ||
		len(pq.NotGroups) != 0 || pq.CaseSensitive || pq.RootBias != "" || pq.CWDBias != "" {
		return "", false
	}
	return terms[0], true
}

// boundedDirTermPosting returns the term's posting capped to
// serviceComponentMultiTermScanMaxIDs.  The path form reuses the ext-filter
// bounded builder (name matches plus capped descendant expansion); the name
// form uses the trigram-limited name posting, falling back to the scanned name
// posting only when its size is known to be within the cap.
func (vol *serviceVolumeIndex) boundedDirTermPosting(term string, matchPath bool) ([]int, bool) {
	if vol == nil || vol.index == nil {
		return nil, false
	}
	if matchPath {
		return vol.pathTermPostingForExtFilter(term, serviceComponentMultiTermScanMaxIDs)
	}
	if ids, ok := vol.completeNameTrigramNameTermPostingLimited(term, serviceComponentMultiTermScanMaxIDs); ok {
		return ids, true
	}
	ids := vol.nameTermPosting(term)
	if len(ids) > serviceComponentMultiTermScanMaxIDs {
		return nil, false
	}
	return ids, true
}

// countDirTermLive is an exact, bounded count for `type:dir <term>`.  The
// resident dir list is the type:dir posting; the term's bounded name/path
// posting drives an intersection, so a selective term never scans the whole
// volume.  Live recentIDs are reconciled the same way extTopPosting does, so
// the count stays complete while the legacy engine is live-updating base
// records in place.
func (vol *serviceVolumeIndex) countDirTermLive(term string, matchPath bool, hidden hiddenBaseIDs) (int, bool) {
	if vol == nil || vol.index == nil || vol.queryIndex == nil || !vol.queryIndex.dirsReady {
		return 0, false
	}
	dirs := vol.queryIndex.dirs
	ids, ok := vol.boundedDirTermPosting(term, matchPath)
	if !ok || len(ids) > serviceComponentMultiTermScanMaxIDs {
		return 0, false
	}
	var seen map[int]struct{}
	if len(vol.recentIDs) > 0 {
		seen = make(map[int]struct{}, len(ids))
	}
	count := 0
	for _, id := range ids {
		if id < 0 || id >= vol.index.compactRecordCount() || !hidden.empty() && hidden.contains(id) {
			continue
		}
		pos := sort.Search(len(dirs), func(i int) bool { return dirs[i] >= uint32(id) })
		if pos >= len(dirs) || dirs[pos] != uint32(id) {
			continue
		}
		count++
		if seen != nil {
			seen[id] = struct{}{}
		}
	}
	for id := range vol.recentIDs {
		if id < 0 || id >= vol.index.compactRecordCount() || !hidden.empty() && hidden.contains(id) {
			continue
		}
		if seen != nil {
			if _, exists := seen[id]; exists {
				continue
			}
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 {
			continue
		}
		if matchPath {
			if vol.index.compactPathContainsTerm(id, term) {
				count++
			}
		} else if strings.Contains(vol.index.compactLowerNameAt(id), term) {
			count++
		}
	}
	return count, true
}

func countServiceVolumesGlobalBoundedFallbackSnapshot(snapshot globalQuerySnapshot, opts queryOptions) (int, bool, error) {	if len(snapshot.volumes) < 2 {
		return 0, false, nil
	}
	pq, err := parseQuery(opts)
	if err != nil {
		return 0, true, err
	}
	terms := nonVolumeTerms(pq.Terms)
	if !globalPlannerEnabled() && !globalExtDefaultSupported(pq) && !globalComponentDefaultSupported(pq, terms) && !globalBoundedFallbackDefaultSupported(pq) {
		return 0, false, nil
	}
	if !snapshot.overlaysOK {
		opts.Trace.replaceDecline("global-bounded-scan:overlay-snapshot-missing")
		return 0, false, nil
	}
	total := 0
	for volumeIndex, vol := range snapshot.volumes {
		if vol == nil || vol.index == nil {
			opts.Trace.replaceDecline("global-bounded-scan:missing-volume")
			return 0, false, nil
		}
		if err := checkQueryCapabilities(pq, vol.index); err != nil {
			return 0, true, err
		}
		volumePQ := pq
		dropSatisfiedVolumeTerms(&volumePQ, vol.index.Volume)
		hidden := hiddenBaseIDs{}
		if volumeIndex < len(snapshot.overlays) && snapshot.overlays[volumeIndex] != nil {
			snap := snapshot.overlays[volumeIndex]
			hidden = hiddenBaseIDs{tombstone: snap.tombstoneIDs, shadowed: snap.shadowedIDs}
		}
		if term, ok := globalDirTermCountShape(volumePQ); ok {
			if count, ok := vol.countDirTermLive(term, volumePQ.MatchPath, hidden); ok {
				total += count
				continue
			}
		}
		cache := make(map[int]string)
		for id := 0; id < vol.index.compactRecordCount(); id++ {
			if id&1023 == 0 && queryCanceled(pq) {
				return 0, true, errQueryCanceled
			}
			if !hidden.empty() && hidden.contains(id) {
				continue
			}
			if _, ok := compactCandidateEntryIfMatch(vol.index, volumePQ, id, cache, true, false); ok {
				total++
			}
		}
	}
	total += globalOverlayMatchCount(snapshot.volumes, snapshot.overlays, pq)
	opts.Trace.setFallback("global-bounded-scan")
	opts.Trace.setPlannerMode("global-bounded-scan")
	opts.Trace.setSource("global:bounded-scan", total)
	opts.Trace.setComplete(true)
	return total, true, nil
}

func countServiceVolumesGlobalOnlySnapshot(snapshot globalQuerySnapshot, opts queryOptions) (int, bool, error) {
	volumes := snapshot.volumes
	pq, err := parseQuery(opts)
	if err != nil {
		return 0, true, err
	}
	globalEnabled := globalPlannerEnabled()
	extOnlySupported := globalExtOnlySupported(pq)
	terms := nonVolumeTerms(pq.Terms)
	if !globalEnabled && !globalExtDefaultSupported(pq) && !globalComponentDefaultSupported(pq, terms) {
		return 0, false, nil
	}
	snapshots := snapshot.overlays
	if !snapshot.overlaysOK {
		opts.Trace.replaceDecline("global-count:overlay-snapshot-missing")
		return 0, false, nil
	}
	for _, vol := range volumes {
		if vol == nil || vol.index == nil {
			opts.Trace.replaceDecline("global-count:missing-volume")
			return 0, false, nil
		}
		if err := checkQueryCapabilities(pq, vol.index); err != nil {
			return 0, true, err
		}
	}
	if extOnlySupported {
		extFilters, _ := globalExtPostingFilters(pq)
		extFilter := extFilters[0]
		baseCount := 0
		if globalSnapshotsHaveHidden(snapshots) {
			ids, ok := globalExtPostingIDs(volumes, extFilter.ext, 0, opts.Trace)
			if !ok {
				return 0, false, nil
			}
			baseCount = len(filterGlobalIDsHidden(ids, snapshots))
		} else {
			for _, vol := range volumes {
				count, ok := vol.countExtPostingWithRecent(extFilter.ext, pq)
				if !ok {
					opts.Trace.addDeclineForVolume("global-ext:missing-posting", vol.volume)
					return 0, false, nil
				}
				baseCount += count
			}
		}
		overlayCount := globalOverlayMatchCount(volumes, snapshots, pq)
		opts.Trace.setPlannerMode("global-count-ext")
		opts.Trace.addTerm(traceTerm{
			Term:      extFilter.ext,
			Kind:      "extension",
			Source:    "global:" + extFilter.source,
			CountHint: baseCount,
			Exact:     true,
		})
		opts.Trace.setSource("global:"+extFilter.source, baseCount)
		opts.Trace.setComplete(true)
		return baseCount + overlayCount, true, nil
	}
	if !globalEnabled && !globalComponentDefaultSupported(pq, terms) {
		return 0, false, nil
	}
	if !globalComponentQuerySupported(pq, terms) {
		opts.Trace.replaceDecline("global-count:unsupported-query")
		return 0, false, nil
	}
	if term, ok := globalExactPathComponentTerm(pq); ok {
		baseCount := 0
		for volumeIndex, vol := range volumes {
			if coverage, fastOK := vol.mappedComponentSubstringCoverageForTop(term); fastOK {
				var hidden func(int) bool
				if volumeIndex >= 0 && volumeIndex < len(snapshots) && snapshots[volumeIndex] != nil {
					h := hiddenBaseIDs{tombstone: snapshots[volumeIndex].tombstoneIDs, shadowed: snapshots[volumeIndex].shadowedIDs}
					hidden = h.contains
				}
				intervalCount, intervalVerified := coverage.countLive(vol, hidden)
				gramSelfCount, gramVisited, gramExact, gramOK := vol.countMappedComponentSelfNameGramHits(term, coverage, hidden, pq)
				if gramOK {
					baseCount += intervalCount + gramSelfCount
					driver := "mapped-pngc-self-gram"
					if gramExact {
						driver = "mapped-pngr-exact-zero"
					}
					opts.Trace.addComponentStats(driver, coverage.rootCount, len(coverage.intervals), coverage.cardinality+gramSelfCount, gramSelfCount, intervalVerified+gramVisited, false)
					continue
				}
				selfCount, selfVisited, scanOK := vol.countMappedComponentSelfHits(term, coverage, hidden, pq)
				if scanOK {
					baseCount += intervalCount + selfCount
					opts.Trace.addComponentStats("mapped-lowr-count", coverage.rootCount, len(coverage.intervals), coverage.cardinality+selfCount, selfCount, intervalVerified+selfVisited, false)
					continue
				}
			}
			coverage, fastOK := vol.mappedComponentCoverageForQuery(term, pq)
			if !fastOK {
				coverage, fastOK = vol.mappedComponentSubstringCoverage(term)
			}
			if fastOK {
				var hidden func(int) bool
				if volumeIndex >= 0 && volumeIndex < len(snapshots) && snapshots[volumeIndex] != nil {
					h := hiddenBaseIDs{tombstone: snapshots[volumeIndex].tombstoneIDs, shadowed: snapshots[volumeIndex].shadowedIDs}
					hidden = h.contains
				}
				count, verified := coverage.countLive(vol, hidden)
				baseCount += count
				opts.Trace.addComponentStats("interval-count", coverage.rootCount, len(coverage.intervals), coverage.cardinality, len(coverage.selfIDs), verified, false)
				continue
			}
			it := newGlobalRecordIterator(volumeIndex, vol.pathTermPosting(term))
			count, verified, err := countGlobalVerifiedIterator(&it, volumes, snapshots, pq)
			if err != nil {
				return 0, true, err
			}
			baseCount += count
			if opts.Trace != nil {
				opts.Trace.ComponentRecordsVerified += verified
			}
		}
		total := baseCount + globalOverlayMatchCount(volumes, snapshots, pq)
		opts.Trace.setPlannerMode("global-count-components")
		addGlobalComponentTraceTerms(opts.Trace, pq, baseCount)
		opts.Trace.setSource("global:components", baseCount)
		opts.Trace.setComplete(true)
		return total, true, nil
	}
	if len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 {
		if count, ok := globalSimplePathORCount(volumes, pq); ok && !globalSnapshotsHaveHidden(snapshots) && !globalSnapshotsHaveOverlayRecords(snapshots) {
			opts.Trace.setPlannerMode("global-count-components")
			opts.Trace.setSource("global:boolean-persisted-count", count)
			opts.Trace.setComplete(true)
			return count, true, nil
		}
		componentIt, ok := globalComponentQueryIterator(volumes, pq, opts.Trace)
		if !ok {
			if opts.Trace == nil || opts.Trace.Decline == "" {
				opts.Trace.replaceDecline("global-count-components:boolean-missing-source")
			}
			return 0, false, nil
		}
		baseCount, verified, err := countGlobalVerifiedIterator(componentIt, volumes, snapshots, pq)
		if err != nil {
			return 0, true, err
		}
		baseCount += globalOverlayMatchCount(volumes, snapshots, pq)
		if opts.Trace != nil {
			opts.Trace.ComponentRecordsVerified += verified
			opts.Trace.setPlannerMode("global-count-components")
			opts.Trace.setSource("global:boolean-iterator", baseCount)
			opts.Trace.setComplete(true)
		}
		return baseCount, true, nil
	}
	componentIt, ok := globalComponentQueryIterator(volumes, pq, opts.Trace)
	if !ok {
		if opts.Trace == nil || opts.Trace.Decline == "" {
			opts.Trace.replaceDecline("global-count-components:missing-source")
		}
		return 0, false, nil
	}
	var ids []globalRecordID
	canceled := false
	if globalSnapshotsHaveHidden(snapshots) {
		hidden := newGlobalHiddenIterator(componentIt, snapshots)
		ids, canceled = collectGlobalIteratorCancelable(&hidden, 0, func() bool { return queryCanceled(pq) })
	} else {
		ids, canceled = collectGlobalIteratorCancelable(componentIt, 0, func() bool { return queryCanceled(pq) })
	}
	if canceled {
		return 0, true, errQueryCanceled
	}
	ids = filterGlobalIDsByType(volumes, ids, pq.Type)
	baseCount, err := countVerifiedGlobalIDs(volumes, ids, pq)
	if err != nil {
		return 0, true, err
	}
	overlayCount := globalOverlayMatchCount(volumes, snapshots, pq)
	opts.Trace.setPlannerMode("global-count-components")
	addGlobalComponentTraceTerms(opts.Trace, pq, len(ids))
	opts.Trace.setSource("global:components", len(ids))
	opts.Trace.setComplete(true)
	return baseCount + overlayCount, true, nil
}

func addGlobalComponentTraceTerms(trace *searchTrace, pq parsedQuery, countHint int) {
	if trace == nil {
		return
	}
	for _, term := range nonVolumeTerms(pq.Terms) {
		trace.addTerm(traceTerm{Term: term, Kind: "path-substring", Source: "global:component-subtree", CountHint: countHint, Exact: false})
	}
	for _, dir := range pq.Dirs {
		trace.addTerm(traceTerm{Term: dir, Kind: "directory-component", Source: "global:dir", CountHint: countHint, Exact: false})
	}
	if globalRegexLiteralSupported(pq) {
		trace.addTerm(traceTerm{Term: pq.RegexTerms[0], Kind: "regex-literal", Source: "global:regex-literal", CountHint: countHint, Exact: false})
	}
	for _, parent := range pq.Parents {
		trace.addTerm(traceTerm{Term: parent, Kind: "parent", Source: "global:parent", CountHint: countHint, Exact: true})
	}
	if pq.Under != "" {
		trace.addTerm(traceTerm{Term: pq.Under, Kind: "under", Source: "global:under", CountHint: countHint, Exact: true})
	}
	if pq.Type != "" {
		trace.addTerm(traceTerm{Term: pq.Type, Kind: "type", Source: "global:type", CountHint: countHint, Exact: true})
	}
	for _, mask := range pq.AttrFilters {
		trace.addTerm(traceTerm{Term: attribMaskString(mask), Kind: "attribute", Source: "global:attribute", CountHint: countHint, Exact: true})
	}
	extFilters, _ := globalExtPostingFilters(pq)
	for _, extFilter := range extFilters {
		trace.addTerm(traceTerm{Term: extFilter.ext, Kind: "extension", Source: "global:" + extFilter.source, CountHint: countHint, Exact: true})
	}
}

func globalComponentQueryIDs(volumes []*serviceVolumeIndex, pq parsedQuery, trace *searchTrace) ([]globalRecordID, bool) {
	var ids []globalRecordID
	haveIDs := false
	var underRoots []globalRecordID
	underPending := false
	if pq.Under != "" {
		var ok bool
		underRoots, ok = globalUnderRoots(volumes, pq.Under)
		if !ok {
			return nil, false
		}
		underPending = true
	}
	intersect := func(next []globalRecordID) {
		if underPending {
			ids = filterGlobalIDsBySubtrees(volumes, underRoots, next)
			underPending = false
			haveIDs = true
			return
		}
		if !haveIDs {
			ids = next
			haveIDs = true
			return
		}
		left := newGlobalIDSliceIterator(ids)
		right := newGlobalIDSliceIterator(next)
		ids = intersectGlobalIterators(&left, &right, 0)
	}
	materializeUnder := func() bool {
		if !underPending {
			return true
		}
		var ok bool
		ids, ok = globalSubtreeIDs(volumes, underRoots, 0)
		underPending = false
		haveIDs = ok
		return ok
	}
	if terms := nonVolumeTerms(pq.Terms); len(terms) > 0 {
		var ok bool
		termIDs, ok := globalComponentPathIDs(volumes, terms)
		if !ok {
			return nil, false
		}
		intersect(termIDs)
	}
	for _, group := range pq.OrGroups {
		var groupIDs []globalRecordID
		for altIndex, alt := range group {
			altIDs, ok := globalComponentSubqueryIDs(volumes, alt, trace)
			if !ok {
				return nil, false
			}
			if altIndex == 0 {
				groupIDs = altIDs
				continue
			}
			left := newGlobalIDSliceIterator(groupIDs)
			right := newGlobalIDSliceIterator(altIDs)
			groupIDs = unionGlobalIterators(&left, &right, 0)
		}
		intersect(groupIDs)
		if len(ids) == 0 {
			break
		}
	}
	if len(pq.NotGroups) > 0 && !materializeUnder() {
		return nil, false
	}
	for _, neg := range pq.NotGroups {
		negIDs, ok := globalComponentSubqueryIDs(volumes, neg, trace)
		if !ok {
			return nil, false
		}
		if haveIDs {
			left := newGlobalIDSliceIterator(ids)
			right := newGlobalIDSliceIterator(negIDs)
			ids = excludeGlobalIterator(&left, &right, 0)
		}
		if len(ids) == 0 {
			break
		}
	}
	for _, dir := range pq.Dirs {
		dirIDs, ok := globalPathTermIDs(volumes, dir)
		if !ok {
			return nil, false
		}
		intersect(dirIDs)
		if len(ids) == 0 {
			break
		}
	}
	if globalRegexLiteralSupported(pq) {
		regexIDs, ok := globalRegexLiteralIDs(volumes, pq)
		if !ok {
			return nil, false
		}
		intersect(regexIDs)
	}
	for _, parent := range pq.Parents {
		parentIDs, ok := globalParentIDs(volumes, parent)
		if !ok {
			return nil, false
		}
		intersect(parentIDs)
		if len(ids) == 0 {
			break
		}
	}
	if len(pq.AttrFilters) > 0 {
		attrIDs, ok := globalAttrIDs(volumes, pq.AttrFilters)
		if !ok {
			return nil, false
		}
		intersect(attrIDs)
	}
	extFilters, _ := globalExtPostingFilters(pq)
	for _, extFilter := range extFilters {
		extIDs, ok := globalExtPostingIDs(volumes, extFilter.ext, 0, trace)
		if !ok {
			return nil, false
		}
		intersect(extIDs)
		if len(ids) == 0 {
			break
		}
	}
	if !materializeUnder() || !haveIDs {
		return nil, false
	}
	return ids, true
}

func globalComponentSubqueryIDs(volumes []*serviceVolumeIndex, pq parsedQuery, trace *searchTrace) ([]globalRecordID, bool) {
	var ids []globalRecordID
	haveIDs := false
	intersect := func(next []globalRecordID) {
		if !haveIDs {
			ids = next
			haveIDs = true
			return
		}
		left := newGlobalIDSliceIterator(ids)
		right := newGlobalIDSliceIterator(next)
		ids = intersectGlobalIterators(&left, &right, 0)
	}
	if terms := nonVolumeTerms(pq.Terms); len(terms) > 0 {
		termIDs, ok := globalComponentPathIDs(volumes, terms)
		if !ok {
			return nil, false
		}
		intersect(termIDs)
	}
	for _, dir := range pq.Dirs {
		dirIDs, ok := globalPathTermIDs(volumes, dir)
		if !ok {
			return nil, false
		}
		intersect(dirIDs)
		if len(ids) == 0 {
			break
		}
	}
	if globalRegexLiteralSupported(pq) {
		regexIDs, ok := globalRegexLiteralIDs(volumes, pq)
		if !ok {
			return nil, false
		}
		intersect(regexIDs)
	}
	for _, parent := range pq.Parents {
		parentIDs, ok := globalParentIDs(volumes, parent)
		if !ok {
			return nil, false
		}
		intersect(parentIDs)
		if len(ids) == 0 {
			break
		}
	}
	if len(pq.AttrFilters) > 0 {
		attrIDs, ok := globalAttrIDs(volumes, pq.AttrFilters)
		if !ok {
			return nil, false
		}
		intersect(attrIDs)
	}
	extFilters, _ := globalExtPostingFilters(pq)
	for _, extFilter := range extFilters {
		extIDs, ok := globalExtPostingIDs(volumes, extFilter.ext, 0, trace)
		if !ok {
			return nil, false
		}
		intersect(extIDs)
		if len(ids) == 0 {
			break
		}
	}
	if pq.Type != "" {
		if !haveIDs {
			return nil, false
		}
		ids = filterGlobalIDsByType(volumes, ids, pq.Type)
	}
	if !haveIDs {
		return nil, false
	}
	return ids, true
}

func globalUnderIDs(volumes []*serviceVolumeIndex, under string) ([]globalRecordID, bool) {
	roots, ok := globalUnderRoots(volumes, under)
	if !ok || len(roots) == 0 {
		return roots, ok
	}
	return globalSubtreeIDs(volumes, roots, 0)
}

func globalUnderRoots(volumes []*serviceVolumeIndex, under string) ([]globalRecordID, bool) {
	if under == "" {
		return nil, false
	}
	under = filepath.Clean(under)
	underVolume := strings.ToUpper(filepath.VolumeName(under))
	roots := make([]globalRecordID, 0, 1)
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			return nil, false
		}
		if underVolume != "" && vol.index.Volume != "" && !strings.EqualFold(vol.index.Volume, underVolume) {
			continue
		}
		for _, rootID := range vol.underRootIDs(under) {
			roots = append(roots, globalRecordID{volume: volumeIndex, local: rootID})
		}
	}
	if len(roots) == 0 {
		return []globalRecordID{}, true
	}
	return roots, true
}

func filterGlobalIDsBySubtrees(volumes []*serviceVolumeIndex, roots, ids []globalRecordID) []globalRecordID {
	base := newGlobalIDSliceIterator(ids)
	filtered := newGlobalSubtreeFilterIterator(&base, volumes, roots)
	return collectGlobalIterator(&filtered, 0)
}

func filterGlobalIDsByType(volumes []*serviceVolumeIndex, ids []globalRecordID, typ string) []globalRecordID {
	if typ == "" {
		return ids
	}
	out := ids[:0]
	for _, id := range ids {
		if id.volume < 0 || id.volume >= len(volumes) {
			continue
		}
		vol := volumes[id.volume]
		if vol == nil || vol.index == nil || id.local < 0 || id.local >= vol.index.compactRecordCount() {
			continue
		}
		isDir := vol.index.compactRecord(id.local).Mode&uint32(os.ModeDir) != 0
		if (typ == "file" && !isDir) || (typ == "dir" && isDir) {
			out = append(out, id)
		}
	}
	return out
}

func globalComponentPathIDs(volumes []*serviceVolumeIndex, terms []string) ([]globalRecordID, bool) {
	probes := pathPlanProbeTerms(terms)
	if len(probes) == 0 {
		return nil, true
	}
	ids, ok := globalPathTermIDs(volumes, probes[0])
	if !ok {
		return nil, false
	}
	for _, term := range probes[1:] {
		base := newGlobalIDSliceIterator(ids)
		filtered := newGlobalPathTermFilterIterator(&base, volumes, term)
		ids = collectGlobalIterator(&filtered, 0)
		if len(ids) == 0 {
			break
		}
	}
	return ids, true
}

func globalPathTermIDs(volumes []*serviceVolumeIndex, term string) ([]globalRecordID, bool) {
	if term == "" {
		return nil, false
	}
	out := make([]globalRecordID, 0)
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil || !vol.pathComponentPostingAvailable(term) {
			return nil, false
		}
		for _, id := range vol.pathPlanTermPosting(term) {
			out = append(out, globalRecordID{volume: volumeIndex, local: id})
		}
	}
	sortGlobalRecordIDs(out)
	return out, true
}

func globalRegexLiteralIDs(volumes []*serviceVolumeIndex, pq parsedQuery) ([]globalRecordID, bool) {
	if !globalRegexLiteralSupported(pq) {
		return nil, false
	}
	return globalPathTermIDs(volumes, pq.RegexTerms[0])
}

func globalParentIDs(volumes []*serviceVolumeIndex, parent string) ([]globalRecordID, bool) {
	if parent == "" || strings.ContainsAny(parent, `\/:*?[]`) {
		return nil, false
	}
	out := make([]globalRecordID, 0)
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			return nil, false
		}
		for _, id := range vol.parentIDs(parent) {
			out = append(out, globalRecordID{volume: volumeIndex, local: id})
		}
	}
	sortGlobalRecordIDs(out)
	return out, true
}

func globalAttrIDs(volumes []*serviceVolumeIndex, filters []uint32) ([]globalRecordID, bool) {
	if len(filters) == 0 {
		return nil, false
	}
	var ids []globalRecordID
	haveIDs := false
	for _, mask := range filters {
		maskIDs := make([]globalRecordID, 0)
		for volumeIndex, vol := range volumes {
			if vol == nil || vol.index == nil {
				return nil, false
			}
			localIDs, ok := vol.attrIDsForMask(mask)
			if !ok {
				return nil, false
			}
			for _, id := range localIDs {
				maskIDs = append(maskIDs, globalRecordID{volume: volumeIndex, local: id})
			}
		}
		sortGlobalRecordIDs(maskIDs)
		if !haveIDs {
			ids = maskIDs
			haveIDs = true
			continue
		}
		left := newGlobalIDSliceIterator(ids)
		right := newGlobalIDSliceIterator(maskIDs)
		ids = intersectGlobalIterators(&left, &right, 0)
		if len(ids) == 0 {
			break
		}
	}
	if !haveIDs {
		return nil, false
	}
	return ids, true
}

func entriesFromGlobalIDs(volumes []*serviceVolumeIndex, ids []globalRecordID, pq parsedQuery) ([]Entry, error) {
	ranked, err := rankedEntriesFromGlobalIDs(volumes, ids, pq)
	if err != nil {
		return nil, err
	}
	return globalRankedEntriesToEntries(ranked), nil
}

func countVerifiedGlobalIDs(volumes []*serviceVolumeIndex, ids []globalRecordID, pq parsedQuery) (int, error) {
	pathCaches := make([]map[int]string, len(volumes))
	count := 0
	for _, id := range ids {
		if queryCanceled(pq) {
			return 0, errQueryCanceled
		}
		if id.volume < 0 || id.volume >= len(volumes) {
			continue
		}
		vol := volumes[id.volume]
		if vol == nil || vol.index == nil || id.local < 0 || id.local >= vol.index.compactRecordCount() {
			continue
		}
		if pathCaches[id.volume] == nil {
			pathCaches[id.volume] = make(map[int]string)
		}
		volumePQ := pq
		dropSatisfiedVolumeTerms(&volumePQ, vol.index.Volume)
		if _, ok := compactCandidateEntryIfMatch(vol.index, volumePQ, id.local, pathCaches[id.volume], true, false); ok {
			count++
		}
	}
	return count, nil
}

func rankedEntriesFromGlobalIDs(volumes []*serviceVolumeIndex, ids []globalRecordID, pq parsedQuery) ([]globalRankedEntry, error) {
	pathCaches := make([]map[int]string, len(volumes))
	rankers := make([]func(int) int, len(volumes))
	for i, vol := range volumes {
		if vol != nil && vol.index != nil {
			rankers[i] = candidateRanker(vol.index, vol.rankForQuery(pq))
		}
	}
	results := make([]globalRankedEntry, 0, len(ids))
	for _, id := range ids {
		if queryCanceled(pq) {
			return nil, errQueryCanceled
		}
		if id.volume < 0 || id.volume >= len(volumes) {
			continue
		}
		vol := volumes[id.volume]
		if vol == nil || vol.index == nil {
			continue
		}
		if pathCaches[id.volume] == nil {
			pathCaches[id.volume] = make(map[int]string)
		}
		volumePQ := pq
		dropSatisfiedVolumeTerms(&volumePQ, vol.index.Volume)
		entry, ok := compactCandidateEntryIfMatch(vol.index, volumePQ, id.local, pathCaches[id.volume], true, compactCandidateCanSkipEntryMatches(volumePQ, true))
		if ok {
			rank := int(^uint(0) >> 1)
			if rankers[id.volume] != nil {
				rank = rankers[id.volume](id.local)
			}
			results = append(results, globalRankedEntry{entry: entry, rank: rank, volume: id.volume, tie: entry.Path})
		}
	}
	return results, nil
}

func globalRankedEntriesToEntries(ranked []globalRankedEntry) []Entry {
	out := make([]Entry, len(ranked))
	for i, item := range ranked {
		out[i] = item.entry
	}
	return out
}

func mergeGlobalOverlayEntries(volumes []*serviceVolumeIndex, snapshots []*volumeSnapshot, base []globalRankedEntry, pq parsedQuery, limit int) []globalRankedEntry {
	if len(snapshots) == 0 {
		sortGlobalRankedEntries(base, pq)
		if limit > 0 && len(base) > limit {
			return base[:limit]
		}
		return base
	}
	out := append([]globalRankedEntry(nil), base...)
	for volumeIndex, snap := range snapshots {
		if snap == nil || volumeIndex < 0 || volumeIndex >= len(volumes) {
			continue
		}
		vol := volumes[volumeIndex]
		if vol == nil {
			continue
		}
		pathCache := make(map[int]string)
		for _, overlay := range vol.overlayRankedMatches(snap, pq, pathCache) {
			out = append(out, globalRankedEntry{entry: overlay.entry, rank: overlay.rank, volume: volumeIndex, tie: overlay.entry.Path, overlay: true})
		}
	}
	sortGlobalRankedEntries(out, pq)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func sortGlobalRankedEntries(entries []globalRankedEntry, pq parsedQuery) {
	global := globalRankedEntriesSpanMultipleVolumes(entries)
	for _, entry := range entries {
		if entry.overlay {
			global = true
			break
		}
	}
	slices.SortStableFunc(entries, func(a, b globalRankedEntry) int {
		if global {
			// Once multiple volumes participate, persisted ranks are local to a
			// volume.  Compare actual entries before rank/tie fallbacks so an
			// overlay or equal local rank cannot change global deterministic order.
			if n := compareSearchAllEntries(a.entry, b.entry, pq); n != 0 {
				return n
			}
		}
		if n := cmp.Compare(a.rank, b.rank); n != 0 {
			return n
		}
		if n := cmp.Compare(a.volume, b.volume); n != 0 {
			return n
		}
		return strings.Compare(a.tie, b.tie)
	})
}

func globalRankedEntriesSpanMultipleVolumes(entries []globalRankedEntry) bool {
	first := -1
	for _, entry := range entries {
		if first < 0 {
			first = entry.volume
			continue
		}
		if entry.volume != first {
			return true
		}
	}
	return false
}

func globalOverlayMatchCount(volumes []*serviceVolumeIndex, snapshots []*volumeSnapshot, pq parsedQuery) int {
	total := 0
	for i, snap := range snapshots {
		if snap == nil || i < 0 || i >= len(volumes) || volumes[i] == nil {
			continue
		}
		total += volumes[i].overlayLiveMatchCount(snap, pq)
	}
	return total
}

func globalOverlaySnapshots(volumes []*serviceVolumeIndex) ([]*volumeSnapshot, bool) {
	snapshots := make([]*volumeSnapshot, len(volumes))
	for i, vol := range volumes {
		if vol == nil || !vol.hasActiveOverlay() {
			continue
		}
		snap := vol.snap.Load()
		if snap == nil {
			return nil, false
		}
		snapshots[i] = snap
	}
	return snapshots, true
}

func globalSnapshotsHaveHidden(snapshots []*volumeSnapshot) bool {
	for _, snap := range snapshots {
		if snap != nil && (len(snap.tombstoneIDs) > 0 || len(snap.shadowedIDs) > 0) {
			return true
		}
	}
	return false
}

func globalSnapshotsHaveOverlayRecords(snapshots []*volumeSnapshot) bool {
	for _, snap := range snapshots {
		if snap != nil && len(snap.records) > 0 {
			return true
		}
	}
	return false
}

func globalVolumesHaveRankForQuery(volumes []*serviceVolumeIndex, pq parsedQuery) bool {
	for _, vol := range volumes {
		if vol == nil || vol.index == nil {
			return false
		}
		recordCount := vol.index.compactRecordCount()
		if len(vol.rankForQuery(pq)) >= recordCount {
			continue
		}
		if pq.SortColumn == "" && len(vol.index.CompactNameOrder) >= recordCount {
			continue
		}
		return false
	}
	return true
}

func globalHiddenContains(snapshots []*volumeSnapshot, id globalRecordID) bool {
	if id.volume < 0 || id.volume >= len(snapshots) || snapshots[id.volume] == nil {
		return false
	}
	hidden := hiddenBaseIDs{tombstone: snapshots[id.volume].tombstoneIDs, shadowed: snapshots[id.volume].shadowedIDs}
	return hidden.contains(id.local)
}

func filterGlobalIDsHidden(ids []globalRecordID, snapshots []*volumeSnapshot) []globalRecordID {
	if !globalSnapshotsHaveHidden(snapshots) {
		return ids
	}
	out := ids[:0]
	for _, id := range ids {
		if !globalHiddenContains(snapshots, id) {
			out = append(out, id)
		}
	}
	return out
}

func globalRankerForVolumes(volumes []*serviceVolumeIndex, pq parsedQuery) func(globalRecordID) int {
	rankers := make([]func(int) int, len(volumes))
	for i, vol := range volumes {
		if vol != nil && vol.index != nil {
			rankers[i] = candidateRanker(vol.index, vol.rankForQuery(pq))
		}
	}
	return func(id globalRecordID) int {
		if id.volume < 0 || id.volume >= len(rankers) || rankers[id.volume] == nil {
			return int(^uint(0) >> 1)
		}
		return rankers[id.volume](id.local)
	}
}

func (it *globalIDSliceIterator) CountHint() int {
	if it == nil || it.pos >= len(it.ids) {
		return 0
	}
	return len(it.ids) - it.pos
}

func (it *globalIDSliceIterator) Next() (globalRecordID, bool) {
	if it == nil || it.pos >= len(it.ids) {
		return globalRecordID{}, false
	}
	id := it.ids[it.pos]
	it.pos++
	return id, true
}

func (it *globalIDSliceIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	if it == nil {
		return globalRecordID{}, false
	}
	for it.pos < len(it.ids) && compareGlobalRecordID(it.ids[it.pos], target) < 0 {
		it.pos++
	}
	return it.Next()
}

func (it *globalHiddenIterator) CountHint() int {
	if it == nil || it.base == nil {
		return 0
	}
	return it.base.CountHint()
}

func (it *globalHiddenIterator) Next() (globalRecordID, bool) {
	if it == nil || it.base == nil {
		return globalRecordID{}, false
	}
	for {
		id, ok := it.base.Next()
		if !ok {
			return globalRecordID{}, false
		}
		if !globalHiddenContains(it.snapshots, id) {
			return id, true
		}
	}
}

func (it *globalHiddenIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	if it == nil || it.base == nil {
		return globalRecordID{}, false
	}
	id, ok := it.base.SeekGE(target)
	for ok && globalHiddenContains(it.snapshots, id) {
		id, ok = it.base.Next()
	}
	return id, ok
}

func (it *globalSubtreeFilterIterator) CountHint() int {
	if it == nil || it.base == nil {
		return 0
	}
	return it.base.CountHint()
}

func (it *globalSubtreeFilterIterator) contains(id globalRecordID) bool {
	if id.volume < 0 || id.volume >= len(it.volumes) {
		return false
	}
	vol := it.volumes[id.volume]
	if vol == nil || vol.index == nil || id.local < 0 || id.local >= vol.index.compactRecordCount() {
		return false
	}
	for _, root := range it.roots[id.volume] {
		if vol.isDescendantOrSelf(id.local, root) {
			return true
		}
	}
	return false
}

func (it *globalSubtreeFilterIterator) Next() (globalRecordID, bool) {
	if it == nil || it.base == nil {
		return globalRecordID{}, false
	}
	for {
		id, ok := it.base.Next()
		if !ok {
			return globalRecordID{}, false
		}
		if it.contains(id) {
			return id, true
		}
	}
}

func (it *globalSubtreeFilterIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	if it == nil || it.base == nil {
		return globalRecordID{}, false
	}
	id, ok := it.base.SeekGE(target)
	if ok && it.contains(id) {
		return id, true
	}
	return it.Next()
}

func (it *globalPathTermFilterIterator) CountHint() int {
	if it == nil || it.base == nil {
		return 0
	}
	return it.base.CountHint()
}

func (it *globalPathTermFilterIterator) contains(id globalRecordID) bool {
	if id.volume < 0 || id.volume >= len(it.volumes) {
		return false
	}
	vol := it.volumes[id.volume]
	if vol == nil || vol.index == nil || id.local < 0 || id.local >= vol.index.compactRecordCount() {
		return false
	}
	if set := it.nameIDs[id.volume]; len(set) > 0 {
		if _, ok := set[id.local]; ok {
			return true
		}
	}
	if it.fast[id.volume] {
		// PCMP represents exact directory components.  For a filtered
		// candidate, walk only its parent chain instead of copying the full
		// root posting (Users can have millions of descendants).
		if strings.Contains(vol.index.compactLowerNameAt(id.local), it.term) {
			return true
		}
		for current := id.local; current >= 0 && current < vol.index.compactRecordCount(); {
			if current != id.local && strings.EqualFold(vol.index.compactLowerNameAt(current), it.term) {
				return true
			}
			rec := vol.index.compactRecord(current)
			if rec.Parent < 0 || int(rec.Parent) == current {
				break
			}
			current = int(rec.Parent)
		}
		return false
	}
	if roots := it.roots[id.volume]; len(roots) > 0 {
		if id.local < len(vol.subtreeStart) {
			pos := vol.subtreeStart[id.local]
			if pos == ^uint32(0) {
				return false
			}
			idx := sort.Search(len(roots), func(i int) bool { return roots[i].start > pos }) - 1
			if idx >= 0 && pos < roots[idx].end {
				return true
			}
		}
		return false
	}
	return vol.index.compactPathContainsTerm(id.local, it.term)
}

func (it *globalPathTermFilterIterator) Next() (globalRecordID, bool) {
	if it == nil || it.base == nil {
		return globalRecordID{}, false
	}
	for {
		id, ok := it.base.Next()
		if !ok {
			return globalRecordID{}, false
		}
		if it.contains(id) {
			return id, true
		}
	}
}

func (it *globalPathTermFilterIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	if it == nil || it.base == nil {
		return globalRecordID{}, false
	}
	id, ok := it.base.SeekGE(target)
	if ok && it.contains(id) {
		return id, true
	}
	return it.Next()
}

func globalExtOnlySupported(pq parsedQuery) bool {
	extFilters, ok := globalExtPostingFilters(pq)
	if !ok || len(extFilters) != 1 || len(nonVolumeTerms(pq.Terms)) != 0 || len(pq.Dirs) != 0 ||
		len(pq.Regexps) != 0 || len(pq.RegexTerms) != 0 ||
		len(pq.Parents) != 0 || pq.Under != "" || pq.Exists || pq.HasModAfter || len(pq.SizeFilters) != 0 ||
		len(pq.DateFilters) != 0 || len(pq.AttrFilters) != 0 || len(pq.OrGroups) != 0 || len(pq.NotGroups) != 0 ||
		pq.CaseSensitive {
		return false
	}
	return pq.Type == ""
}

func globalExtDefaultSupported(pq parsedQuery) bool {
	return globalExtOnlySupported(pq) && (pq.SortColumn == "" || pq.SortColumn == "path" ||
		pq.SortColumn == "size" || pq.SortColumn == "modified" || pq.SortColumn == "extension" || pq.SortColumn == "type") &&
		(!pq.MatchPath || len(pq.Terms) == 0)
}

func globalBoundedFallbackDefaultSupported(pq parsedQuery) bool {
	return !pq.isEmpty()
}

func globalComponentDefaultSupported(pq parsedQuery, terms []string) bool {
	if pq.SortColumn != "" && pq.SortColumn != "path" && pq.SortColumn != "size" &&
		pq.SortColumn != "modified" && pq.SortColumn != "extension" && pq.SortColumn != "type" {
		return false
	}
	if !globalComponentDefaultHasRoot(pq) {
		return false
	}
	if !globalComponentDefaultTermsLong(pq, terms) {
		return false
	}
	return globalComponentQuerySupported(pq, terms)
}

func globalComponentDefaultHasRoot(pq parsedQuery) bool {
	extFilters, extOK := globalExtPostingFilters(pq)
	if (pq.MatchPath && (queryHasExplicitPathTerm(pq.Raw) || globalComponentDefaultImplicitPathRoot(pq))) ||
		globalComponentVolumeAnchored(pq) ||
		len(pq.Dirs) != 0 || len(pq.Parents) != 0 || pq.Under != "" || len(pq.AttrFilters) != 0 ||
		globalRegexLiteralSupported(pq) ||
		(extOK && len(extFilters) != 0 &&
			(pq.Type != "" || pq.Exists || pq.HasModAfter || len(pq.SizeFilters) != 0 || len(pq.DateFilters) != 0)) {
		return true
	}
	for _, group := range pq.OrGroups {
		if len(group) == 0 {
			return false
		}
		for _, alt := range group {
			if !globalComponentDefaultHasRoot(alt) {
				return false
			}
		}
		return true
	}
	return false
}

func globalComponentVolumeAnchored(pq parsedQuery) bool {
	if !pq.MatchPath {
		return false
	}
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) {
			return true
		}
	}
	return false
}

func globalComponentDefaultImplicitPathRoot(pq parsedQuery) bool {
	if !pq.MatchPath {
		return false
	}
	terms := nonVolumeTerms(pq.Terms)
	if len(terms) >= 2 {
		return true
	}
	extFilters, ok := globalExtPostingFilters(pq)
	return ok && len(terms) >= 1 && len(extFilters) > 0
}

func globalComponentDefaultTermsLong(pq parsedQuery, terms []string) bool {
	for _, term := range terms {
		if len(term) < 3 && !slices.Contains(pq.ImplicitPathTerms, term) {
			return false
		}
	}
	for _, dir := range pq.Dirs {
		if len(dir) < 3 || strings.ContainsAny(dir, `\/*?[]:`) {
			return false
		}
	}
	for _, group := range pq.OrGroups {
		for _, alt := range group {
			if !globalComponentDefaultTermsLong(alt, nonVolumeTerms(alt.Terms)) {
				return false
			}
		}
	}
	for _, neg := range pq.NotGroups {
		if !globalComponentDefaultTermsLong(neg, nonVolumeTerms(neg.Terms)) {
			return false
		}
	}
	return true
}

func queryHasExplicitPathTerm(raw string) bool {
	for _, field := range strings.Fields(raw) {
		lower := strings.ToLower(strings.TrimLeft(field, "!-"))
		if strings.HasPrefix(lower, "path:") || strings.HasPrefix(lower, "fullpath:") ||
			strings.HasPrefix(lower, "full-path:") || strings.HasPrefix(lower, "full_path:") ||
			strings.HasPrefix(lower, "location:") {
			return true
		}
	}
	return false
}

type globalExtFilter struct {
	ext    string
	source string
}

func globalExtPostingFilters(pq parsedQuery) ([]globalExtFilter, bool) {
	if len(pq.Exts) > 0 && len(pq.Globs) > 0 {
		return nil, false
	}
	if len(pq.Exts) > 1 {
		return nil, false
	}
	if len(pq.Exts) == 1 {
		ext := pq.Exts[0]
		return []globalExtFilter{{ext: ext, source: "ext:" + ext}}, true
	}
	if len(pq.Globs) == 0 {
		return nil, true
	}
	exts, ok := simpleGlobExts(pq.Globs)
	if !ok || len(exts) != 1 {
		return nil, false
	}
	ext := exts[0]
	return []globalExtFilter{{ext: ext, source: "glob-ext:" + ext}}, true
}

func globalComponentQuerySupported(pq parsedQuery, terms []string) bool {
	minTerms := 2
	if queryHasExplicitPathTerm(pq.Raw) || globalComponentVolumeAnchored(pq) {
		minTerms = 1
	}
	if len(pq.OrGroups) != 0 || len(pq.NotGroups) != 0 {
		minTerms = 1
	}
	if len(pq.OrGroups) != 0 {
		minTerms = 0
	}
	if len(pq.Dirs) != 0 || len(pq.Parents) != 0 || pq.Under != "" || len(pq.AttrFilters) != 0 {
		minTerms = 0
	}
	if !globalComponentTermsSupported(pq, terms, minTerms, true) {
		return false
	}
	for _, group := range pq.OrGroups {
		if len(group) == 0 {
			return false
		}
		for _, alt := range group {
			if len(alt.OrGroups) != 0 || len(alt.NotGroups) != 0 {
				return false
			}
			if !globalComponentTermsSupported(alt, nonVolumeTerms(alt.Terms), 1, true) {
				return false
			}
		}
	}
	for _, neg := range pq.NotGroups {
		if len(neg.OrGroups) != 0 || len(neg.NotGroups) != 0 {
			return false
		}
		if !globalComponentTermsSupported(neg, nonVolumeTerms(neg.Terms), 1, true) {
			return false
		}
	}
	return true
}

func globalComponentTermsSupported(pq parsedQuery, terms []string, minTerms int, allowExtFilters bool) bool {
	extFilters, extFiltersOK := globalExtPostingFilters(pq)
	if !extFiltersOK || (!allowExtFilters && len(extFilters) != 0) {
		return false
	}
	if len(pq.Dirs) != 0 || len(pq.Parents) != 0 || pq.Under != "" || len(pq.AttrFilters) != 0 || len(extFilters) != 0 {
		minTerms = 0
	}
	if globalRegexLiteralSupported(pq) {
		minTerms = 0
	}
	if (len(terms) > 0 && !pq.MatchPath) || (!pq.MatchPath && len(pq.Dirs) == 0 && len(pq.Parents) == 0 && pq.Under == "" && len(extFilters) == 0 && len(pq.AttrFilters) == 0 && len(pq.OrGroups) == 0 && !globalRegexLiteralSupported(pq)) ||
		len(terms) < minTerms || (pq.Type != "" && pq.Type != "file" && pq.Type != "dir") ||
		(len(pq.Regexps) != 0 && !globalRegexLiteralSupported(pq)) ||
		(len(pq.RegexTerms) != 0 && !globalRegexLiteralSupported(pq)) {
		return false
	}
	for _, dir := range pq.Dirs {
		if dir == "" || strings.ContainsAny(dir, `\/*?[]:`) {
			return false
		}
	}
	for _, term := range terms {
		if term == "" || strings.ContainsAny(term, `\/*?[]:`) {
			return false
		}
	}
	return true
}

func globalRegexLiteralSupported(pq parsedQuery) bool {
	return pq.MatchPath && !pq.CaseSensitive && len(pq.Regexps) > 0 && len(pq.RegexTerms) == 1 && len(pq.RegexTerms[0]) >= 3 && !strings.ContainsAny(pq.RegexTerms[0], `\/*?[]:`)
}

func collectGlobalTopN(iterators []globalIDIterator, limit int, rankOf func(globalRecordID) int) []globalRecordID {
	if limit <= 0 || rankOf == nil {
		return nil
	}
	h := make(globalRankMaxHeap, 0, limit)
	for _, it := range iterators {
		if it == nil {
			continue
		}
		for {
			id, ok := it.Next()
			if !ok {
				break
			}
			item := globalRankItem{id: id, rank: rankOf(id)}
			if len(h) < limit {
				heap.Push(&h, item)
				continue
			}
			if globalRankItemBetter(item, h[0]) {
				h[0] = item
				heap.Fix(&h, 0)
			}
		}
	}
	out := make([]globalRankItem, len(h))
	copy(out, h)
	slices.SortFunc(out, func(a, b globalRankItem) int {
		if n := cmp.Compare(a.rank, b.rank); n != 0 {
			return n
		}
		return compareGlobalRecordID(a.id, b.id)
	})
	ids := make([]globalRecordID, len(out))
	for i, item := range out {
		ids[i] = item.id
	}
	return ids
}

type globalVerifiedTopHeap struct {
	items  []globalRankedEntry
	pq     parsedQuery
	global bool
}

func (h globalVerifiedTopHeap) Len() int { return len(h.items) }

func (h globalVerifiedTopHeap) Less(i, j int) bool {
	return compareGlobalVerifiedEntries(h.items[i], h.items[j], h.pq, h.global) > 0
}

func (h globalVerifiedTopHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }

func (h *globalVerifiedTopHeap) Push(x any) {
	h.items = append(h.items, x.(globalRankedEntry))
}

func (h *globalVerifiedTopHeap) Pop() any {
	old := h.items
	n := len(old)
	x := old[n-1]
	h.items = old[:n-1]
	return x
}

func compareGlobalVerifiedEntries(a, b globalRankedEntry, pq parsedQuery, global bool) int {
	if global {
		if n := compareSearchAllEntries(a.entry, b.entry, pq); n != 0 {
			return n
		}
	}
	if n := cmp.Compare(a.rank, b.rank); n != 0 {
		return n
	}
	if n := cmp.Compare(a.volume, b.volume); n != 0 {
		return n
	}
	return strings.Compare(a.tie, b.tie)
}

// collectGlobalVerifiedTopN consumes the set iterator once and retains only
// the requested top-N entries. Boolean branches stay lazy: OR sources merge
// their posting iterators and NOT advances the exclusion iterator with
// SeekGE, while this verifier owns the only bounded result heap.
func collectGlobalVerifiedTopN(it globalIDIterator, volumes []*serviceVolumeIndex, snapshots []*volumeSnapshot, pq parsedQuery, limit int) ([]globalRankedEntry, int, error) {
	if it == nil || limit <= 0 {
		return nil, 0, nil
	}
	global := len(volumes) > 1 || globalSnapshotsHaveOverlayRecords(snapshots)
	h := &globalVerifiedTopHeap{pq: pq, global: global}
	heap.Init(h)
	pathCaches := make([]map[int]string, len(volumes))
	rankers := make([]func(int) int, len(volumes))
	for i, vol := range volumes {
		if vol != nil && vol.index != nil {
			rankers[i] = candidateRanker(vol.index, vol.rankForQuery(pq))
		}
	}
	verified := 0
	for {
		id, ok := it.Next()
		if !ok {
			break
		}
		if verified&1023 == 0 && queryCanceled(pq) {
			return nil, verified, errQueryCanceled
		}
		if globalHiddenContains(snapshots, id) || id.volume < 0 || id.volume >= len(volumes) {
			continue
		}
		vol := volumes[id.volume]
		if vol == nil || vol.index == nil || id.local < 0 || id.local >= vol.index.compactRecordCount() {
			continue
		}
		if pathCaches[id.volume] == nil {
			pathCaches[id.volume] = make(map[int]string)
		}
		volumePQ := pq
		dropSatisfiedVolumeTerms(&volumePQ, vol.index.Volume)
		entry, ok := compactCandidateEntryIfMatch(vol.index, volumePQ, id.local, pathCaches[id.volume], true, false)
		verified++
		if !ok {
			continue
		}
		rank := int(^uint(0) >> 1)
		if rankers[id.volume] != nil {
			rank = rankers[id.volume](id.local)
		}
		item := globalRankedEntry{entry: entry, rank: rank, volume: id.volume, tie: entry.Path}
		if h.Len() < limit {
			heap.Push(h, item)
			continue
		}
		if compareGlobalVerifiedEntries(item, h.items[0], pq, global) < 0 {
			h.items[0] = item
			heap.Fix(h, 0)
		}
	}
	out := append([]globalRankedEntry(nil), h.items...)
	sortGlobalRankedEntries(out, pq)
	return out, verified, nil
}

func countGlobalVerifiedIterator(it globalIDIterator, volumes []*serviceVolumeIndex, snapshots []*volumeSnapshot, pq parsedQuery) (int, int, error) {
	if it == nil {
		return 0, 0, nil
	}
	pathCaches := make([]map[int]string, len(volumes))
	count, verified := 0, 0
	for {
		id, ok := it.Next()
		if !ok {
			break
		}
		if verified&1023 == 0 && queryCanceled(pq) {
			return 0, verified, errQueryCanceled
		}
		if globalHiddenContains(snapshots, id) || id.volume < 0 || id.volume >= len(volumes) {
			continue
		}
		vol := volumes[id.volume]
		if vol == nil || vol.index == nil || id.local < 0 || id.local >= vol.index.compactRecordCount() {
			continue
		}
		if pathCaches[id.volume] == nil {
			pathCaches[id.volume] = make(map[int]string)
		}
		volumePQ := pq
		dropSatisfiedVolumeTerms(&volumePQ, vol.index.Volume)
		_, ok = compactCandidateEntryIfMatch(vol.index, volumePQ, id.local, pathCaches[id.volume], true, false)
		verified++
		if ok {
			count++
		}
	}
	return count, verified, nil
}

func globalRankItemBetter(a, b globalRankItem) bool {
	if a.rank != b.rank {
		return a.rank < b.rank
	}
	return compareGlobalRecordID(a.id, b.id) < 0
}

func globalRankItemWorse(a, b globalRankItem) bool {
	if a.rank != b.rank {
		return a.rank > b.rank
	}
	return compareGlobalRecordID(a.id, b.id) > 0
}

type globalRankItem struct {
	id   globalRecordID
	rank int
}

type globalRankMaxHeap []globalRankItem

func (h globalRankMaxHeap) Len() int { return len(h) }

func (h globalRankMaxHeap) Less(i, j int) bool { return globalRankItemWorse(h[i], h[j]) }

func (h globalRankMaxHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *globalRankMaxHeap) Push(x any) {
	*h = append(*h, x.(globalRankItem))
}

func (h *globalRankMaxHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

func (it *globalRecordIterator) CountHint() int {
	if it == nil || it.pos >= len(it.ids) {
		return 0
	}
	return len(it.ids) - it.pos
}

func (it *globalRecordIterator) Next() (globalRecordID, bool) {
	if it == nil || it.pos >= len(it.ids) {
		return globalRecordID{}, false
	}
	id := globalRecordID{volume: it.volume, local: it.ids[it.pos]}
	it.pos++
	return id, true
}

func (it *globalRecordIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	if it == nil {
		return globalRecordID{}, false
	}
	if target.volume > it.volume {
		it.pos = len(it.ids)
		return globalRecordID{}, false
	}
	minLocal := target.local
	if target.volume < it.volume {
		minLocal = 0
	}
	for it.pos < len(it.ids) && it.ids[it.pos] < minLocal {
		it.pos++
	}
	return it.Next()
}

func (it *globalPostingIterator) CountHint() int {
	if it == nil {
		return 0
	}
	return it.remaining
}

func (it *globalPostingIterator) Next() (globalRecordID, bool) {
	if it == nil || it.remaining <= 0 {
		return globalRecordID{}, false
	}
	if !it.posting.mapped {
		if it.pos >= len(it.posting.ids) {
			it.remaining = 0
			return globalRecordID{}, false
		}
		id := it.posting.ids[it.pos]
		it.pos++
		it.remaining--
		return globalRecordID{volume: it.volume, local: int(id)}, true
	}
	for it.blockPos >= len(it.block) {
		block, _, ok := it.posting.it.nextBlock()
		if !ok {
			it.remaining = 0
			return globalRecordID{}, false
		}
		it.trace.addPostingBlocks(1, 0)
		it.block = block
		it.blockPos = 0
	}
	id := it.block[it.blockPos]
	it.blockPos++
	it.remaining--
	return globalRecordID{volume: it.volume, local: int(id)}, true
}

func (it *globalPostingIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	if it == nil || it.remaining <= 0 {
		return globalRecordID{}, false
	}
	if target.volume > it.volume {
		it.remaining = 0
		return globalRecordID{}, false
	}
	minLocal := target.local
	if target.volume < it.volume || minLocal < 0 {
		minLocal = 0
	}
	if !it.posting.mapped {
		skipped := sort.Search(len(it.posting.ids)-it.pos, func(i int) bool {
			return int(it.posting.ids[it.pos+i]) >= minLocal
		})
		it.pos += skipped
		it.remaining -= skipped
		return it.Next()
	}
	if it.blockPos < len(it.block) {
		skipped := sort.Search(len(it.block)-it.blockPos, func(i int) bool {
			return int(it.block[it.blockPos+i]) >= minLocal
		})
		it.blockPos += skipped
		it.remaining -= skipped
		if it.blockPos < len(it.block) {
			return it.Next()
		}
	}
	for it.posting.it.next < it.posting.it.end {
		meta, ok := it.posting.it.blockMetaAt(it.posting.it.next)
		if !ok {
			it.remaining = 0
			return globalRecordID{}, false
		}
		if int(meta.maxID) < minLocal {
			it.posting.it.next++
			it.remaining -= int(meta.count)
			it.trace.addPostingBlocks(0, 1)
			continue
		}
		block, _, ok := it.posting.it.nextBlock()
		if !ok {
			it.remaining = 0
			return globalRecordID{}, false
		}
		it.block = block
		it.trace.addPostingBlocks(1, 0)
		it.blockPos = sort.Search(len(block), func(i int) bool { return int(block[i]) >= minLocal })
		it.remaining -= it.blockPos
		return it.Next()
	}
	it.remaining = 0
	return globalRecordID{}, false
}

func intersectGlobalIterators(a, b globalIDIterator, limit int) []globalRecordID {
	if a == nil || b == nil {
		return nil
	}
	av, aok := a.Next()
	bv, bok := b.Next()
	out := make([]globalRecordID, 0, minPositiveCountHint(a.CountHint(), b.CountHint()))
	for aok && bok {
		switch compareGlobalRecordID(av, bv) {
		case 0:
			out = append(out, av)
			if limit > 0 && len(out) >= limit {
				return out
			}
			av, aok = a.Next()
			bv, bok = b.Next()
		case -1:
			av, aok = a.SeekGE(bv)
		default:
			bv, bok = b.SeekGE(av)
		}
	}
	return out
}

func unionGlobalIterators(a, b globalIDIterator, limit int) []globalRecordID {
	if a == nil {
		return collectGlobalIterator(b, limit)
	}
	if b == nil {
		return collectGlobalIterator(a, limit)
	}
	av, aok := a.Next()
	bv, bok := b.Next()
	out := make([]globalRecordID, 0, positiveCountHint(a.CountHint())+positiveCountHint(b.CountHint()))
	for aok || bok {
		var next globalRecordID
		switch {
		case !bok || (aok && compareGlobalRecordID(av, bv) < 0):
			next = av
			av, aok = a.Next()
		case !aok || compareGlobalRecordID(av, bv) > 0:
			next = bv
			bv, bok = b.Next()
		default:
			next = av
			av, aok = a.Next()
			bv, bok = b.Next()
		}
		out = append(out, next)
		if limit > 0 && len(out) >= limit {
			return out
		}
	}
	return out
}

func excludeGlobalIterator(include, exclude globalIDIterator, limit int) []globalRecordID {
	if include == nil {
		return nil
	}
	if exclude == nil {
		return collectGlobalIterator(include, limit)
	}
	iv, iok := include.Next()
	ev, eok := exclude.Next()
	out := make([]globalRecordID, 0, positiveCountHint(include.CountHint()))
	for iok {
		for eok && compareGlobalRecordID(ev, iv) < 0 {
			ev, eok = exclude.SeekGE(iv)
		}
		if !eok || compareGlobalRecordID(iv, ev) != 0 {
			out = append(out, iv)
			if limit > 0 && len(out) >= limit {
				return out
			}
		}
		iv, iok = include.Next()
	}
	return out
}

func collectGlobalIterator(it globalIDIterator, limit int) []globalRecordID {
	if it == nil {
		return nil
	}
	out := make([]globalRecordID, 0, positiveCountHint(it.CountHint()))
	for {
		id, ok := it.Next()
		if !ok {
			return out
		}
		out = append(out, id)
		if limit > 0 && len(out) >= limit {
			return out
		}
	}
}

// collectGlobalIteratorCancelable is collectGlobalIterator with a per-yield
// cancellation probe.  Materializing a broad multi-term component can walk
// millions of IDs; the caller bails with errQueryCanceled instead of letting
// a superseded query run to completion.
func collectGlobalIteratorCancelable(it globalIDIterator, limit int, canceled func() bool) ([]globalRecordID, bool) {
	if it == nil {
		return nil, false
	}
	out := make([]globalRecordID, 0, positiveCountHint(it.CountHint()))
	for {
		if canceled != nil && canceled() {
			return nil, true
		}
		id, ok := it.Next()
		if !ok {
			return out, false
		}
		out = append(out, id)
		if limit > 0 && len(out) >= limit {
			return out, false
		}
	}
}

func minPositiveCountHint(a, b int) int {
	a = positiveCountHint(a)
	b = positiveCountHint(b)
	if a == 0 {
		return b
	}
	if b == 0 {
		return a
	}
	return min(a, b)
}

func positiveCountHint(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

func sortIntListsByLen(lists [][]int) {
	slices.SortFunc(lists, func(a, b []int) int {
		return cmp.Compare(len(a), len(b))
	})
}

func sortUint32ListsByLen(lists [][]uint32) {
	slices.SortFunc(lists, func(a, b []uint32) int {
		return cmp.Compare(len(a), len(b))
	})
}

func sortPostingCountCandidatesByLen(lists []postingCountCandidate) {
	slices.SortFunc(lists, func(a, b postingCountCandidate) int {
		return cmp.Compare(a.len(), b.len())
	})
}

func sortIDsByRank(ids []int, rankOf func(int) int) {
	slices.SortFunc(ids, func(a, b int) int {
		if n := cmp.Compare(rankOf(a), rankOf(b)); n != 0 {
			return n
		}
		return cmp.Compare(a, b)
	})
}

func sortCandidatePlanSourcesByLen(sources []candidatePlanSource) {
	slices.SortFunc(sources, func(a, b candidatePlanSource) int {
		if n := cmp.Compare(a.len(), b.len()); n != 0 {
			return n
		}
		return cmp.Compare(a.name, b.name)
	})
}

type candidatePlan struct {
	vol               *serviceVolumeIndex
	pq                parsedQuery
	sources           []candidatePlanSource
	empty             bool
	underPathFallback string
}

type candidatePlanSource struct {
	name       string
	ids        []int
	posting    postingCountCandidate
	hasPosting bool
	union      []candidatePlanSource
	vol        *serviceVolumeIndex
	roots      []int
}

func traceTermForCandidateSource(source candidatePlanSource, volume string) traceTerm {
	term := traceTerm{
		Source:    source.name,
		CountHint: source.len(),
		Exact:     true,
		Volume:    volume,
	}
	switch {
	case strings.HasPrefix(source.name, "ext:"):
		term.Kind = "extension"
		term.Term = strings.TrimPrefix(source.name, "ext:")
	case strings.HasPrefix(source.name, "glob-ext:"):
		term.Kind = "glob-extension"
		term.Term = strings.TrimPrefix(source.name, "glob-ext:")
	case strings.HasPrefix(source.name, "parent:"):
		term.Kind = "parent"
		term.Term = strings.TrimPrefix(source.name, "parent:")
	case strings.HasPrefix(source.name, "attrib:"):
		term.Kind = "attribute"
		term.Term = strings.TrimPrefix(source.name, "attrib:")
	case strings.HasPrefix(source.name, "dir:"):
		term.Kind = "directory-component"
		term.Term = strings.TrimPrefix(source.name, "dir:")
	case strings.HasPrefix(source.name, "path-term:"):
		term.Kind = "path-substring"
		term.Term = strings.TrimPrefix(source.name, "path-term:")
		term.Exact = false
	case strings.HasPrefix(source.name, "term:"):
		term.Kind = "name-substring"
		term.Term = strings.TrimPrefix(source.name, "term:")
		term.Exact = false
	case source.name == "type:dir":
		term.Kind = "type"
		term.Term = "dir"
	case source.name == "under":
		term.Kind = "under"
		term.Term = "under"
	case strings.HasPrefix(source.name, "or-group"):
		term.Kind = "or"
		term.Term = "or-group"
	default:
		term.Kind = "source"
		term.Term = source.name
	}
	return term
}

func (source candidatePlanSource) len() int {
	switch {
	case source.hasPosting:
		return source.posting.len()
	case len(source.union) > 0:
		total := 0
		for _, part := range source.union {
			total += part.len()
		}
		return total
	case len(source.roots) > 0:
		if source.vol != nil {
			if estimate := source.vol.estimateUnderDescendantCount(source.roots); estimate >= 0 {
				return estimate
			}
		}
		return len(source.roots)
	default:
		return len(source.ids)
	}
}

func (source candidatePlanSource) materialize() []int {
	switch {
	case source.hasPosting:
		return uint32sToInts(source.posting.materialize())
	case len(source.union) > 0:
		total := 0
		for _, part := range source.union {
			total += part.len()
		}
		out := make([]int, 0, total)
		for _, part := range source.union {
			out = append(out, part.materialize()...)
		}
		sort.Ints(out)
		return uniqueSortedInts(out)
	case len(source.roots) > 0:
		seen := make(map[int]struct{}, 256)
		out := make([]int, 0, source.len())
		if source.vol == nil {
			return nil
		}
		for _, rootID := range source.roots {
			for _, id := range source.vol.underDescendants(rootID) {
				if _, ok := seen[id]; ok {
					continue
				}
				seen[id] = struct{}{}
				out = append(out, id)
			}
		}
		sort.Ints(out)
		return out
	default:
		return append([]int(nil), source.ids...)
	}
}

func (source candidatePlanSource) intersect(out []int) []int {
	if len(out) == 0 {
		return out
	}
	if source.hasPosting {
		if source.posting.mapped {
			return intersectSortedIntsWithPostingIterator(out, source.posting.it)
		}
		return intersectSortedIntsWithUint32s(out, source.posting.ids)
	}
	return intersectSortedInts(out, source.materialize())
}

func intersectSortedIntsWithUint32s(a []int, b []uint32) []int {
	out := a[:0]
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		av := a[i]
		bv := int(b[j])
		switch {
		case av == bv:
			out = append(out, av)
			i++
			j++
		case av < bv:
			i++
		default:
			j++
		}
	}
	return out
}

func intersectSortedIntsWithPostingIterator(a []int, it postingBlockIterator) []int {
	out := a[:0]
	cursor := 0
	for cursor < len(a) && it.next < it.end {
		block, meta, ok := it.nextBlock()
		if !ok {
			return nil
		}
		if len(block) == 0 {
			continue
		}
		for cursor < len(a) && a[cursor] < int(meta.minID) {
			cursor++
		}
		if cursor >= len(a) {
			break
		}
		if a[cursor] > int(meta.maxID) {
			continue
		}
		j := 0
		for cursor < len(a) && j < len(block) {
			av := a[cursor]
			if av > int(meta.maxID) {
				break
			}
			bv := int(block[j])
			switch {
			case av == bv:
				out = append(out, av)
				cursor++
				j++
			case av < bv:
				cursor++
			default:
				j++
			}
		}
	}
	return out
}

func (vol *serviceVolumeIndex) plannedCandidates(pq parsedQuery) ([]int, bool) {
	if out, ok := vol.exactTopPlannedCandidates(pq); ok {
		if len(pq.Exts) == 1 {
			volume := ""
			if vol != nil && vol.index != nil {
				volume = vol.index.Volume
			}
			pq.Trace.addTerm(traceTerm{Term: pq.Exts[0], Kind: "extension", Source: "planned:ext-top", CountHint: len(out), Exact: true, Volume: volume})
		}
		pq.Trace.setSource("planned:ext-top", len(out))
		return out, true
	}
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		return nil, false
	}
	if out, scanned, ok := plan.executeTop(pq); ok {
		pq.Trace.addTerms(plan.traceTerms())
		pq.Trace.setSource("planned:or-group-lazy-top", scanned)
		return out, true
	}
	out := plan.execute()
	if compactCandidateCanSkipEntryMatches(pq, true) && pq.Limit > 0 {
		out = topCandidateIDsByRank(out, pq.Limit, vol.index, vol.rankForQuery(pq))
	}
	// topCandidateIDsByRank intentionally only knows persisted ranks.  Apply
	// the requested Entry comparator afterward so equal/rankless candidates
	// retain deterministic path/name tie ordering.
	sortCandidateIDs(out, pq, vol.index, vol.rankForQuery(pq))
	pq.Trace.addTerms(plan.traceTerms())
	pq.Trace.setSource("planned:"+plan.sourceSummary(), len(out))
	return out, true
}

func (vol *serviceVolumeIndex) exactTopPlannedCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.queryIndex == nil || pq.Limit <= 0 ||
		len(pq.Exts) != 1 || len(pq.Globs) > 0 || len(pq.Dirs) > 0 ||
		pq.Type != "" || pq.Under != "" || pq.HasModAfter || pq.Exists ||
		(pq.SortColumn != "" && pq.SortColumn != "size" && pq.SortColumn != "modified" && pq.SortColumn != "extension" && pq.SortColumn != "type" && pq.SortColumn != "path") ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 {
		return nil, false
	}
	terms := nonVolumeTerms(pq.Terms)
	if len(terms) > 0 {
		return nil, false
	}
	ids, ok := vol.extTopPosting(pq.Exts[0], pq.Limit, pq)
	if !ok {
		return nil, false
	}
	return ids, true
}

func nonVolumeTerms(terms []string) []string {
	out := make([]string, 0, len(terms))
	for _, term := range terms {
		if isVolumeQueryTerm(term) {
			continue
		}
		out = append(out, term)
	}
	return out
}

func (vol *serviceVolumeIndex) plannedCount(pq parsedQuery) (int, bool) {
	return vol.plannedCountHidden(pq, hiddenBaseIDs{})
}

// plannedCountHidden is plannedCount plus an id-level exclusion set (base
// tombstoned/shadowed ids from the active v9 overlay snapshot). It never
// materializes an Entry unless path reconstruction is unavoidable, and it
// filters candidate ids against hidden before evaluating them so counts
// stay exact while an overlay is active (review G7 / plan R2.6).
func (vol *serviceVolumeIndex) plannedCountHidden(pq parsedQuery, hidden hiddenBaseIDs) (int, bool) {
	pq.CountOnly = true
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		return 0, false
	}
	if plan.empty {
		return 0, true
	}
	if count, scanned, ok := plan.executeUnionCount(pq, hidden); ok {
		pq.Trace.addTerms(plan.traceTerms())
		pq.Trace.setSource("planned:or-group-lazy-count", scanned)
		return count, true
	}
	ids := plan.execute()
	pq.Trace.addTerms(plan.traceTerms())
	count := 0

	// Fast path: when the query can be decided from the record alone (no path
	// substring matching, no path-scoped filters), count without reconstructing
	// the full path or allocating an Entry per candidate. This is the common
	// case for `count ext:md`, `count type:file ext:go`, etc., and is where
	// Everything's -get-result-count was beating us.
	if !queryNeedsPath(pq) {
		for _, id := range ids {
			if id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			if !hidden.empty() && hidden.contains(id) {
				continue
			}
			rec := vol.index.compactRecord(id)
			if rec.Deleted {
				continue
			}
			if vol.recordMatchesNonPath(id, rec, pq) {
				count++
			}
		}
		return count, true
	}

	pathCache := make(map[int]string)
	for _, id := range ids {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		if !hidden.empty() && hidden.contains(id) {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !compactRecordPrecheck(rec, pq, pq.MatchPath) {
			continue
		}
		path := vol.index.reconstructCompactPathCached(id, pathCache)
		entry := Entry{
			Path:        path,
			Name:        rec.Name,
			LowerPath:   strings.ToLower(path),
			LowerName:   vol.index.compactLowerNameAt(id),
			Mode:        rec.Mode,
			Size:        rec.Size,
			ModUnix:     rec.ModUnix,
			IndexSource: vol.index.Source,
		}
		if entryMatches(entry, pq, pq.MatchPath) {
			count++
		}
	}
	return count, true
}

// queryNeedsPath reports whether deciding a match requires the reconstructed
// full path rather than just the record's own fields.
func queryNeedsPath(pq parsedQuery) bool {
	if pq.MatchPath && len(pq.Terms) > 0 {
		return true
	}
	if len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || len(pq.Parents) > 0 {
		return true
	}
	if pq.Under != "" || pq.Exists {
		return true
	}
	for _, group := range pq.OrGroups {
		for _, alt := range group {
			if queryNeedsPath(alt) {
				return true
			}
		}
	}
	for _, neg := range pq.NotGroups {
		if queryNeedsPath(neg) {
			return true
		}
	}
	return false
}

// recordMatchesNonPath verifies a record against a query that does not require
// path reconstruction. It mirrors entryMatches but operates on the compact
// record's own name/size/mtime/mode fields.
func (vol *serviceVolumeIndex) recordMatchesNonPath(id int, rec CompactRecord, pq parsedQuery) bool {
	cmpName := normalizeCase(rec.Name, pq.CaseSensitive)
	if !pq.MatchPath && !containsAll(cmpName, pq.Terms) {
		return false
	}
	if pq.Type == "file" && rec.Mode&uint32(os.ModeDir) != 0 {
		return false
	}
	if pq.Type == "dir" && rec.Mode&uint32(os.ModeDir) == 0 {
		return false
	}
	if !attrFiltersMatch(rec.Mode, pq.AttrFilters) {
		return false
	}
	if pq.HasModAfter {
		if rec.ModUnix == 0 || !time.Unix(0, rec.ModUnix).After(pq.ModifiedAfter) {
			return false
		}
	}
	for _, ext := range pq.Exts {
		actual := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
		if normalizeCase(actual, pq.CaseSensitive) != ext {
			return false
		}
	}
	for _, glob := range pq.Globs {
		ok, err := filepath.Match(glob, cmpName)
		if err != nil || !ok {
			return false
		}
	}
	for _, sf := range pq.SizeFilters {
		if !sf.matches(rec.Size) {
			return false
		}
	}
	for _, df := range pq.DateFilters {
		if !df.matches(rec.ModUnix) {
			return false
		}
	}
	for _, group := range pq.OrGroups {
		matched := false
		for _, alt := range group {
			if vol.recordMatchesNonPath(id, rec, alt) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, neg := range pq.NotGroups {
		if vol.recordMatchesNonPath(id, rec, neg) {
			return false
		}
	}
	return true
}

func (vol *serviceVolumeIndex) buildCandidatePlan(pq parsedQuery) (candidatePlan, bool) {
	plan := candidatePlan{vol: vol, pq: pq}
	if vol == nil || vol.index == nil || pq.CaseSensitive {
		return plan, false
	}
	var underRoots []int
	underEstimatedSize := -1
	addRequired := func(name string, ids []int) bool {
		if len(ids) == 0 {
			plan.empty = true
			return false
		}
		plan.sources = append(plan.sources, candidatePlanSource{
			name: name,
			ids:  uniqueSortedInts(append([]int(nil), ids...)),
		})
		return true
	}
	addPostingRequired := func(name string, candidate postingCountCandidate) bool {
		if candidate.len() == 0 {
			plan.empty = true
			return false
		}
		plan.sources = append(plan.sources, candidatePlanSource{
			name:       name,
			posting:    candidate,
			hasPosting: true,
		})
		return true
	}

	if pq.Under != "" {
		under := filepath.Clean(pq.Under)
		if vol.index.Volume != "" && !strings.EqualFold(filepath.VolumeName(under), vol.index.Volume) {
			plan.empty = true
			return plan, true
		}
		underRoots = vol.underRootIDs(under)
		if len(underRoots) == 0 {
			plan.underPathFallback = under
		}
		if len(underRoots) > 0 {
			underEstimatedSize = vol.estimateUnderDescendantCount(underRoots)
		}
	}

	for _, ext := range pq.Exts {
		if candidate, ok := vol.extPostingCountCandidate(ext); ok {
			if !addPostingRequired("ext:"+ext, candidate) {
				return plan, true
			}
			continue
		}
		if !addRequired("ext:"+ext, vol.extPosting(ext)) {
			return plan, true
		}
	}
	globExts, globsOK := simpleGlobExts(pq.Globs)
	if globsOK {
		for _, ext := range globExts {
			if candidate, ok := vol.extPostingCountCandidate(ext); ok {
				if !addPostingRequired("glob-ext:"+ext, candidate) {
					return plan, true
				}
				continue
			}
			if !addRequired("glob-ext:"+ext, vol.extPosting(ext)) {
				return plan, true
			}
		}
	} else {
		for _, ext := range complexGlobExts(pq.Globs) {
			if candidate, ok := vol.extPostingCountCandidate(ext); ok {
				if !addPostingRequired("glob-ext:"+ext, candidate) {
					return plan, true
				}
				continue
			}
			if !addRequired("glob-ext:"+ext, vol.extPosting(ext)) {
				return plan, true
			}
		}
	}
	if pq.Type == "dir" {
		if vol.queryIndex != nil && vol.queryIndex.dirsReady {
			if !addPostingRequired("type:dir", postingCountCandidate{ids: vol.queryIndex.dirs}) {
				return plan, true
			}
		}
	}
	for _, parent := range pq.Parents {
		if !addRequired("parent:"+parent, vol.parentIDs(parent)) {
			return plan, true
		}
	}
	for _, mask := range pq.AttrFilters {
		ids, ok := vol.attrIDsForMask(mask)
		if !ok {
			continue
		}
		if !addRequired("attrib:"+attribMaskString(mask), ids) {
			return plan, true
		}
	}
	for _, dir := range pq.Dirs {
		if !vol.pathComponentPostingAvailable(dir) {
			continue
		}
		roots := vol.pathComponentRootIDs(dir)
		if len(roots) == 0 {
			plan.empty = true
			return plan, true
		}
		plan.sources = append(plan.sources, candidatePlanSource{
			name:  "dir:" + dir,
			vol:   vol,
			roots: uniqueSortedInts(roots),
		})
	}
	// OR groups: a record must match at least one alternative, so the candidate
	// source is the union of each alternative's posting. We only build a posting
	// source when every alternative is cheaply postable (ext/glob-ext/term);
	// otherwise the group is verified later against the full candidate set.
	for _, group := range pq.OrGroups {
		source, ok := vol.orGroupPlanSource(group, pq.MatchPath)
		if !ok {
			continue
		}
		if source.len() == 0 {
			plan.empty = true
			return plan, true
		}
		plan.sources = append(plan.sources, source)
	}

	// Cheap structural filters above are verified against the full query later.
	// Add bounded path/name term postings for remaining terms so the plan drives
	// off the smallest source and intersects the rest lazily.  This keeps a
	// loose multi-term query like `Dataset trainingdata nrrd` from materializing
	// a huge promoted extension posting and verifying every other term against
	// it.  Only selective (bounded) postings are added; a broad term that cannot
	// be bounded stays verification-only so correctness never depends on a cap.
	if pq.MatchPath && hasNonVolumeTerm(pq.Terms) {
		for _, term := range pathPlanProbeTerms(pq.Terms) {
			ids, ok := vol.boundedPathTermPlanSource(term)
			if !ok {
				continue
			}
			if !addRequired("path-term:"+term, ids) {
				return plan, true
			}
		}
		if len(plan.sources) == 0 && len(underRoots) == 0 {
			// Path mode with no usable source at all: decline so the search
			// uses the streaming name-order scan instead of materializing a
			// broad posting on every call.
			return plan, false
		}
	} else if !pq.MatchPath {
		for _, term := range pq.Terms {
			if !addRequired("term:"+term, vol.namePlanTermPosting(term)) {
				return plan, true
			}
		}
		if !globsOK {
			for _, term := range globLiteralTerms(pq.Globs, pq.CaseSensitive) {
				if list := vol.nameTermPosting(term); len(list) > 0 {
					if !addRequired("glob-literal:"+term, list) {
						return plan, true
					}
				}
			}
		}
	}

	if len(underRoots) > 0 && shouldUseUnderPlanSource(underEstimatedSize, plan.sources) {
		plan.sources = append(plan.sources, candidatePlanSource{
			name:  "under",
			vol:   vol,
			roots: uniqueSortedInts(underRoots),
		})
	}

	if len(plan.sources) == 0 {
		return plan, false
	}
	return plan, true
}

func (vol *serviceVolumeIndex) boundedPathTermPlanSource(term string) ([]int, bool) {
	if vol == nil || vol.index == nil || term == "" || isVolumeQueryTerm(term) ||
		strings.ContainsAny(term, `\/*?[]:`) || len(term) < 3 {
		return nil, false
	}
	// A term whose required filename grams are proven absent by the complete
	// name-gram metadata (PNGR counts complete, or the PNGC companion) cannot
	// match anything: return a proven empty source so the plan marks itself
	// empty instead of materializing a broad extension/scan.  This is the same
	// exact-zero proof the fast count path uses.
	if _, _, exactZero, complete := completeSelfNameGramIterators(vol.index, term); complete && exactZero {
		return []int{}, true
	}
	// First try the posting path; it is cheaper than a scan for a selective
	// term that has persisted gram or component postings.
	ids, ok := vol.completeNameTrigramPathTermPosting(term)
	if ok && len(ids) <= serviceComponentTrigramExpansionMaxIDs {
		return ids, true
	}
	// The posting path declined (for example an omitted-common gram, a subtree
	// estimate above the expansion cap, or a term with zero name matches).  For
	// a loose multi-term query the plan still needs a bounded source so it does
	// not drive off a huge promoted extension posting and verify every other
	// term against it.  A bounded parallel name scan proves a zero-match term
	// empty immediately, and a small match set becomes a path posting source
	// (name self-hits expanded to descendants) for the driving term.
	if scanned := vol.scanNameTermBounded(term, serviceComponentTrigramExpansionMaxIDs); scanned != nil {
		if len(scanned) == 0 {
			return []int{}, true
		}
		if ids, ok := vol.expandNameMatchesToPathTermPosting("", term, scanned); ok && len(ids) <= serviceComponentTrigramExpansionMaxIDs {
			return ids, true
		}
	}
	if vol.index.compactRecordCount() > serviceResidentChildRangeMaxRecords {
		return nil, false
	}
	ids, ok = vol.scannedNamePathTermPosting(term)
	if !ok || len(ids) > serviceComponentTrigramExpansionMaxIDs {
		return nil, false
	}
	return ids, true
}

// scanNameTermBounded scans the compact records in parallel up to maxMatches
// and returns the matched record IDs.  It returns nil when the match set
// exceeds the bound, so the caller declines to the exhaustive scan rather than
// materializing a huge posting.  A term with provably zero matches returns an
// empty non-nil slice.  This works in every mode (resident, lowmem, mapped)
// because it scans record IDs directly, mirroring scanNameTermPosting.
func (vol *serviceVolumeIndex) scanNameTermBounded(term string, maxMatches int) []int {
	if vol == nil || vol.index == nil || term == "" || maxMatches <= 0 {
		return nil
	}
	recordCount := vol.index.compactRecordCount()
	if recordCount == 0 {
		return []int{}
	}
	// The mapped fast path scans the persisted lower-name blob in bulk instead
	// of reconstructing every CompactRecord; reuse it when available.
	if ids, ok := vol.index.scanCompactLowerNameTerm(term); ok {
		if len(ids) > maxMatches {
			return nil
		}
		return ids
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, recordCount/250_000))
	if workers <= 1 {
		out := make([]int, 0, 64)
		for i := 0; i < recordCount; i++ {
			rec := vol.index.compactRecord(i)
			if rec.Deleted {
				continue
			}
			if strings.Contains(vol.index.compactLowerNameAt(i), term) {
				out = append(out, i)
				if len(out) > maxMatches {
					return nil
				}
			}
		}
		return out
	}
	parts := make([][]int, workers)
	exceeded := make([]bool, workers)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := worker * recordCount / workers
		end := (worker + 1) * recordCount / workers
		wg.Add(1)
		go func(worker, start, end int) {
			defer wg.Done()
			local := make([]int, 0, 64)
			for i := start; i < end; i++ {
				rec := vol.index.compactRecord(i)
				if rec.Deleted {
					continue
				}
				if strings.Contains(vol.index.compactLowerNameAt(i), term) {
					local = append(local, i)
					if len(local) > maxMatches {
						exceeded[worker] = true
						return
					}
				}
			}
			parts[worker] = local
		}(worker, start, end)
	}
	wg.Wait()
	total := 0
	for _, ex := range exceeded {
		if ex {
			return nil
		}
	}
	for _, part := range parts {
		total += len(part)
	}
	out := make([]int, 0, total)
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

func (vol *serviceVolumeIndex) completeNameTrigramNameTermPostingLimited(term string, maxIDs int) ([]int, bool) {
	trigrams := vol.nameTrigramIndex()
	if vol == nil || trigrams == nil {
		return nil, false
	}
	cacheKey := "\x00complete-ngram-name:" + term
	vol.termMu.Lock()
	if vol.termCache != nil {
		if entry, ok := vol.termCache[cacheKey]; ok {
			if vol.cacheStampValid(entry.gen) {
				vol.termMu.Unlock()
				return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
					id, ok := vol.idForFRN(rec.FRN)
					return ok && vol.nameTrigramCandidateMatches(id, term)
				}), true
			}
		}
	}
	vol.termMu.Unlock()

	ids, ok, missing := trigrams.selectiveCandidateIDs(term, maxIDs)
	if len(term) >= 6 {
		ids, ok, missing = trigrams.selectiveIntersectCandidateIDs(term, maxIDs)
	}
	if !ok {
		return nil, false
	}
	if missing {
		return vol.nameTrigramRecentMatches(term), true
	}
	out := uniqueSortedInts(vol.verifyNameTrigramCandidateIDs(ids, term))
	vol.cacheNamePosting(cacheKey, out)
	return vol.withNameTrigramRecentCandidates(out, term), true
}

func (vol *serviceVolumeIndex) completeNameTrigramPathTermPosting(term string) ([]int, bool) {
	if vol == nil || vol.index == nil {
		return nil, false
	}
	cacheKey := "\x00complete-trigram-path:" + term
	vol.termMu.Lock()
	if vol.pathTermCache != nil {
		if entry, ok := vol.pathTermCache[cacheKey]; ok {
			if vol.cacheStampValid(entry.gen) {
				vol.termMu.Unlock()
				return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
					id, ok := vol.idForFRN(rec.FRN)
					return ok && vol.index.compactPathContainsTerm(id, term)
				}), true
			}
		}
	}
	vol.termMu.Unlock()

	nameMatches, ok := vol.completeNameTrigramNameTermPostingLimited(term, servicePathNameTrigramCandidateMaxIDs)
	if !ok {
		return nil, false
	}
	return vol.expandNameMatchesToPathTermPosting(cacheKey, term, nameMatches)
}

func (vol *serviceVolumeIndex) scannedNamePathTermPosting(term string) ([]int, bool) {
	if vol == nil || vol.index == nil {
		return nil, false
	}
	cacheKey := "\x00scan-name-path:" + term
	vol.termMu.Lock()
	if vol.pathTermCache != nil {
		if entry, ok := vol.pathTermCache[cacheKey]; ok {
			if vol.cacheStampValid(entry.gen) {
				vol.termMu.Unlock()
				return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
					id, ok := vol.idForFRN(rec.FRN)
					return ok && vol.index.compactPathContainsTerm(id, term)
				}), true
			}
		}
	}
	vol.termMu.Unlock()

	nameMatches := vol.nameTermPosting(term)
	if len(nameMatches) > servicePathNameTrigramCandidateMaxIDs {
		return nil, false
	}
	return vol.expandNameMatchesToPathTermPosting(cacheKey, term, nameMatches)
}

func (vol *serviceVolumeIndex) expandNameMatchesToPathTermPosting(cacheKey, term string, nameMatches []int) ([]int, bool) {
	if vol == nil || vol.index == nil {
		return nil, false
	}
	seen := make(map[int]struct{}, len(nameMatches))
	out := make([]int, 0, len(nameMatches))
	estimated := 0
	for _, id := range nameMatches {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			continue
		}
		if rec.Mode&uint32(os.ModeDir) == 0 {
			estimated++
		} else {
			if !vol.hasDescendantIndex() {
				return nil, false
			}
			estimated += vol.estimatedDescendantOrSelfCount(id)
		}
		if estimated > serviceComponentTrigramExpansionMaxIDs {
			return nil, false
		}
	}
	for _, id := range nameMatches {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		if _, exists := seen[id]; !exists {
			seen[id] = struct{}{}
			out = append(out, id)
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 {
			continue
		}
		if !vol.hasDescendantIndex() {
			return nil, false
		}
		for _, childID := range vol.underDescendants(id) {
			child := int(childID)
			if _, exists := seen[child]; exists {
				continue
			}
			seen[child] = struct{}{}
			out = append(out, child)
			if len(out) > serviceComponentTrigramExpansionMaxIDs {
				return nil, false
			}
		}
	}
	sort.Ints(out)
	if cacheKey != "" {
		vol.cachePathPosting(cacheKey, out)
	}
	return out, true
}

// broadPathScanCandidates is retained for direct benchmark/test coverage of
// the old broad path scanner. The live route now uses boundedScanCandidates for
// this family.
//
// It only engages when the query is purely plain terms in path mode with no
// other constraints that an earlier, cheaper strategy already covers.
func (vol *serviceVolumeIndex) broadPathScanCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || pq.CaseSensitive || !pq.MatchPath {
		return nil, false
	}
	if pq.Under != "" || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || len(pq.OrGroups) > 0 {
		return nil, false
	}
	terms := make([]string, 0, len(pq.Terms))
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) {
			continue
		}
		terms = append(terms, term)
	}
	if len(terms) == 0 {
		return nil, false
	}

	recordCount := vol.index.compactRecordCount()
	workers := minInt(maxInt(1, recordCountWorkers(recordCount)), 16)
	if workers <= 1 {
		out := make([]int, 0, 256)
		for i := 0; i < recordCount; i++ {
			if i&1023 == 0 && queryCanceled(pq) {
				return nil, false
			}
			rec := vol.index.compactRecord(i)
			if rec.Deleted {
				continue
			}
			if vol.index.compactPathContainsAll(i, terms) {
				out = append(out, i)
			}
		}
		out = vol.withRecentCandidates(out, 0, func(rec CompactRecord) bool {
			id, ok := vol.idForFRN(rec.FRN)
			return ok && vol.index.compactPathContainsAll(id, terms)
		})
		sortCandidateIDs(out, pq, vol.index, vol.rankForQuery(pq))
		return capBroadCandidates(out, pq), true
	}

	parts := make([][]int, workers)
	var canceled atomic.Bool
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		start := w * recordCount / workers
		end := (w + 1) * recordCount / workers
		wg.Add(1)
		go func(w, start, end int) {
			defer wg.Done()
			local := make([]int, 0, 256)
			for i := start; i < end; i++ {
				if i&1023 == 0 && queryCanceled(pq) {
					canceled.Store(true)
					return
				}
				rec := vol.index.compactRecord(i)
				if rec.Deleted {
					continue
				}
				if vol.index.compactPathContainsAll(i, terms) {
					local = append(local, i)
				}
			}
			parts[w] = local
		}(w, start, end)
	}
	wg.Wait()
	if canceled.Load() {
		return nil, false
	}

	total := 0
	for _, p := range parts {
		total += len(p)
	}
	out := make([]int, 0, total)
	for _, p := range parts {
		out = append(out, p...)
	}
	out = vol.withRecentCandidates(out, 0, func(rec CompactRecord) bool {
		id, ok := vol.idForFRN(rec.FRN)
		return ok && vol.index.compactPathContainsAll(id, terms)
	})
	sortCandidateIDs(out, pq, vol.index, vol.rankForQuery(pq))
	return capBroadCandidates(out, pq), true
}

// boundedScanCandidates is the universal candidate floor. It accepts any query
// shape by scanning a bounded compact-record order and evaluating the same
// predicate the shared verifier uses. Specialized postings can beat this, but
// no query should need to fall through to older per-term reconstruction routes.
func (vol *serviceVolumeIndex) boundedScanCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil {
		return nil, false
	}
	recordCount := vol.index.compactRecordCount()
	if recordCount == 0 {
		return []int{}, true
	}
	order := vol.orderForQuery(pq)
	limit := pq.Limit
	canStopAtLimit := !pq.CountOnly && limit > 0 && pq.RootBias == "" && pq.CWDBias == ""
	if canStopAtLimit {
		out := make([]int, 0, min(limit, 1024))
		cache := make(map[int]string)
		for pos := 0; pos < compactUint32OrderLen(order, recordCount); pos++ {
			if pos&1023 == 0 && queryCanceled(pq) {
				return nil, false
			}
			id := compactUint32OrderAt(order, pos)
			if _, ok := compactCandidateEntryIfMatch(vol.index, pq, id, cache, true, false); !ok {
				continue
			}
			out = append(out, id)
			if len(out) >= limit {
				return out, true
			}
		}
		return out, true
	}

	workers := minInt(maxInt(1, recordCountWorkers(recordCount)), 16)
	if workers <= 1 || recordCount < 8192 {
		out := make([]int, 0, min(recordCount, 1024))
		cache := make(map[int]string)
		for pos := 0; pos < compactUint32OrderLen(order, recordCount); pos++ {
			if pos&1023 == 0 && queryCanceled(pq) {
				return nil, false
			}
			id := compactUint32OrderAt(order, pos)
			if _, ok := compactCandidateEntryIfMatch(vol.index, pq, id, cache, true, false); ok {
				out = append(out, id)
			}
		}
		return out, true
	}

	orderLen := compactUint32OrderLen(order, recordCount)
	parts := make([][]int, workers)
	var canceled atomic.Bool
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		start := w * orderLen / workers
		end := (w + 1) * orderLen / workers
		wg.Add(1)
		go func(w, start, end int) {
			defer wg.Done()
			local := make([]int, 0, 256)
			cache := make(map[int]string)
			for pos := start; pos < end; pos++ {
				if pos&1023 == 0 && queryCanceled(pq) {
					canceled.Store(true)
					return
				}
				id := compactUint32OrderAt(order, pos)
				if _, ok := compactCandidateEntryIfMatch(vol.index, pq, id, cache, true, false); ok {
					local = append(local, id)
				}
			}
			parts[w] = local
		}(w, start, end)
	}
	wg.Wait()
	if canceled.Load() {
		return nil, false
	}
	total := 0
	for _, part := range parts {
		total += len(part)
	}
	out := make([]int, 0, total)
	for _, part := range parts {
		out = append(out, part...)
	}
	return out, true
}

func (vol *serviceVolumeIndex) boundedScanCandidatesHiddenTop(pq parsedQuery, hidden hiddenBaseIDs, limit int) ([]int, bool) {
	if vol == nil || vol.index == nil || limit <= 0 {
		return nil, false
	}
	recordCount := vol.index.compactRecordCount()
	order := vol.orderForQuery(pq)
	out := make([]int, 0, min(limit, 1024))
	cache := make(map[int]string)
	for pos := 0; pos < compactUint32OrderLen(order, recordCount); pos++ {
		if pos&1023 == 0 && queryCanceled(pq) {
			return nil, false
		}
		id := compactUint32OrderAt(order, pos)
		if !hidden.empty() && hidden.contains(id) {
			continue
		}
		if _, ok := compactCandidateEntryIfMatch(vol.index, pq, id, cache, true, false); !ok {
			continue
		}
		out = append(out, id)
		if len(out) >= limit {
			break
		}
	}
	return out, true
}

func compactUint32OrderLen(order []uint32, recordCount int) int {
	if len(order) == 0 {
		return recordCount
	}
	return len(order)
}

func compactUint32OrderAt(order []uint32, pos int) int {
	if len(order) == 0 {
		return pos
	}
	return int(order[pos])
}

// capBroadCandidates trims a fully-path-verified, name-order-sorted candidate
// list to just enough entries to satisfy a search limit. The broad scan has
// already confirmed the path predicate, so when the query carries no record-level
// filter that could still reject a candidate (type/ext/glob/size/dm/NOT), the
// downstream loop will accept candidates in order until it hits the limit;
// returning more than that is wasted reconstruction. For count queries, or when
// a rejecting filter is present, the full set is returned so the count/limit
// remains exact.
func capBroadCandidates(ids []int, pq parsedQuery) []int {
	if pq.CountOnly || pq.Limit <= 0 {
		return ids
	}
	if pq.RootBias != "" || pq.CWDBias != "" {
		// Bias re-ranks downstream; capping by name order would drop preferred
		// results before they can be promoted.
		return ids
	}
	if pq.Type != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		len(pq.NotGroups) > 0 || pq.HasModAfter || pq.Exists {
		return ids
	}
	if len(ids) <= pq.Limit {
		return ids
	}
	return ids[:pq.Limit]
}

func recordCountWorkers(recordCount int) int {
	return maxInt(1, recordCount/250_000)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// hasNonVolumeTerm reports whether terms contains at least one plain term that
// is not a bare volume/drive token.
func hasNonVolumeTerm(terms []string) bool {
	for _, term := range terms {
		if !isVolumeQueryTerm(term) {
			return true
		}
	}
	return false
}

func pathPlanProbeTerms(terms []string) []string {
	out := make([]string, 0, len(terms))
	for _, term := range terms {
		if term == "" || isVolumeQueryTerm(term) {
			continue
		}
		out = append(out, term)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		aDot, bDot := strings.Contains(a, "."), strings.Contains(b, ".")
		if aDot != bDot {
			return aDot
		}
		if len(a) != len(b) {
			return len(a) > len(b)
		}
		return a < b
	})
	return out
}

func (vol *serviceVolumeIndex) namePlanTermPosting(term string) []int {
	if strings.Contains(term, ".") {
		if exact := vol.exactNameIDs(term); len(exact) > 0 {
			return exact
		}
	}
	return vol.nameTermPosting(term)
}

func (vol *serviceVolumeIndex) pathPlanTermPosting(term string) []int {
	if strings.Contains(term, ".") {
		if exact := vol.exactNameIDs(term); len(exact) > 0 {
			out := make([]int, 0, len(exact))
			for _, id := range exact {
				if id < 0 || vol == nil || vol.index == nil || id >= vol.index.compactRecordCount() {
					continue
				}
				rec := vol.index.compactRecord(id)
				if rec.Deleted {
					continue
				}
				if rec.Mode&uint32(os.ModeDir) == 0 {
					out = append(out, id)
					continue
				}
				out = append(out, vol.underDescendants(id)...)
			}
			sort.Ints(out)
			return uniqueSortedInts(out)
		}
	}
	return vol.pathTermPosting(term)
}

func (vol *serviceVolumeIndex) parentIDs(parent string) []int {
	if vol == nil || vol.index == nil || parent == "" || strings.ContainsAny(parent, `\/:*?[]`) {
		return nil
	}
	roots := vol.pathComponentRootIDs(parent)
	if len(roots) == 0 {
		return nil
	}
	out := make([]int, 0, 64)
	for _, root := range roots {
		if root < 0 || root >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(root)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 || !strings.EqualFold(vol.index.compactLowerNameAt(root), parent) {
			continue
		}
		for _, childID := range vol.childIDsForRecord(root) {
			id := int(childID)
			if id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			if !vol.index.compactRecord(id).Deleted {
				out = append(out, id)
			}
		}
	}
	sort.Ints(out)
	return uniqueSortedInts(out)
}

func (vol *serviceVolumeIndex) attrIDsForMask(mask uint32) ([]int, bool) {
	if vol == nil || vol.queryIndex == nil || vol.queryIndex.attrBits == nil || mask == 0 {
		return nil, false
	}
	var ids []uint32
	haveIDs := false
	for _, bit := range queryAttrBits() {
		if mask&bit != bit {
			continue
		}
		bitIDs := vol.queryIndex.attrBits[bit]
		if len(bitIDs) == 0 {
			return []int{}, true
		}
		if !haveIDs {
			ids = append([]uint32(nil), bitIDs...)
			haveIDs = true
			continue
		}
		ids = intersectSortedUint32s(ids, bitIDs)
		if len(ids) == 0 {
			break
		}
	}
	if !haveIDs {
		return nil, false
	}
	return uint32sToInts(ids), true
}

func attribMaskString(mask uint32) string {
	var b strings.Builder
	for _, item := range []struct {
		bit uint32
		ch  byte
	}{
		{fileAttributeReadonly, 'R'},
		{fileAttributeHidden, 'H'},
		{fileAttributeSystem, 'S'},
		{fileAttributeDir, 'D'},
		{fileAttributeArchive, 'A'},
	} {
		if mask&item.bit == item.bit {
			b.WriteByte(item.ch)
		}
	}
	if b.Len() == 0 {
		return "0"
	}
	return b.String()
}

func (vol *serviceVolumeIndex) limitedPathTermCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || countNonVolumeTerms(pq.Terms) != 1 {
		return nil, false
	}
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) {
			continue
		}
		candidates, ok := vol.pathPlanTermPostingLimited(term, pq)
		if !ok {
			return nil, false
		}
		sortCandidateIDs(candidates, pq, vol.index, vol.rankForQuery(pq))
		return candidates, true
	}
	return nil, false
}

func (vol *serviceVolumeIndex) limitedDottedPathScanCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CountOnly || pq.Limit <= 0 ||
		pq.CaseSensitive || pq.Under != "" || pq.Type != "" || pq.HasModAfter || pq.Exists ||
		pq.CWDBias != "" || pq.RootBias != "" ||
		len(pq.Exts) > 0 || len(pq.Dirs) > 0 || len(pq.Globs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		countNonVolumeTerms(pq.Terms) != 1 {
		return nil, false
	}
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) {
			continue
		}
		if !strings.Contains(term, ".") || strings.ContainsAny(term, `\/*?[]:`) {
			return nil, false
		}
		out := vol.scanPathTermPrefixLimited(pq, term, pq.Limit, 16_384)
		if len(out) >= pq.Limit {
			return out, true
		}
		out = vol.scanPathTermLimited(pq, term, pq.Limit)
		if len(out) >= pq.Limit {
			return out, true
		}
		return nil, false
	}
	return nil, false
}

func (vol *serviceVolumeIndex) pathDirectoryTermTopCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !vol.hasDescendantIndex() ||
		!pq.MatchPath || pq.CountOnly || pq.Limit <= 0 ||
		pq.CaseSensitive || pq.Under != "" || pq.Type != "" || pq.HasModAfter || pq.Exists ||
		pq.CWDBias != "" || pq.RootBias != "" ||
		len(pq.Exts) != 1 || len(pq.Dirs) > 0 || len(pq.Globs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		countNonVolumeTerms(pq.Terms) != 1 {
		return nil, false
	}
	term := ""
	for _, candidate := range pq.Terms {
		if !isVolumeQueryTerm(candidate) {
			term = candidate
			break
		}
	}
	if len(term) < 3 || strings.ContainsAny(term, `\/*?[]:`) {
		return nil, false
	}
	if vol.pathTermIsUsableExtensionCandidate(term) {
		return nil, false
	}
	nameMatches, roots, complete := vol.pathDirectoryTermSource(term)
	if complete && len(nameMatches) == 0 {
		return []int{}, true
	}
	if len(roots) == 0 {
		return nil, false
	}
	recordCount := vol.index.compactRecordCount()
	rankOf := candidateRanker(vol.index, vol.rankForQuery(pq))
	seen := make(map[int]struct{}, pq.Limit*4)
	h := make(candidateRankMaxHeap, 0, pq.Limit)
	add := func(id int) {
		if id < 0 || id >= recordCount {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !compactRecordPrecheck(rec, pq, true) || !vol.index.compactPathContainsTerm(id, term) {
			return
		}
		seen[id] = struct{}{}
		item := candidateRankItem{id: id, rank: rankOf(id)}
		if len(h) < pq.Limit {
			heap.Push(&h, item)
			return
		}
		if item.rank < h[0].rank {
			h[0] = item
			heap.Fix(&h, 0)
		}
	}
	for _, id := range nameMatches {
		add(id)
	}
	if ext := pq.Exts[0]; ext != "" {
		if ids, ok := vol.extTopPosting(ext, maxInt(pq.Limit*8, pq.Limit), pq); ok {
			for _, id := range ids {
				add(id)
			}
		}
	}
	scanned := 0
	for rootIndex, rootID := range roots {
		if rootIndex&127 == 0 && queryCanceled(pq) {
			return nil, false
		}
		if len(vol.subtreeOrder) > 0 && rootID >= 0 && rootID < len(vol.subtreeStart) && rootID < len(vol.subtreeEnd) {
			start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
			if start != ^uint32(0) && start <= end && int(end) <= len(vol.subtreeOrder) {
				for pos := start; pos < end; pos++ {
					if scanned >= serviceComponentMultiTermScanMaxIDs {
						return heapIDsByRank(h, rankOf), len(h) > 0
					}
					scanned++
					if pos&4095 == 0 && queryCanceled(pq) {
						return nil, false
					}
					add(int(vol.subtreeOrder[pos]))
				}
				continue
			}
		}
		for _, childID := range vol.underDescendantsLimited(rootID, serviceComponentMultiTermScanMaxIDs-scanned+1) {
			if scanned >= serviceComponentMultiTermScanMaxIDs {
				return heapIDsByRank(h, rankOf), len(h) > 0
			}
			scanned++
			add(int(childID))
		}
	}
	out := heapIDsByRank(h, rankOf)
	return out, len(out) > 0
}

func (vol *serviceVolumeIndex) pathDirectoryTermRoots(term string) ([]int, bool) {
	_, roots, complete := vol.pathDirectoryTermSource(term)
	return roots, complete
}

func (vol *serviceVolumeIndex) pathDirectoryTermSource(term string) (nameMatches []int, roots []int, complete bool) {
	if vol == nil || vol.index == nil || len(term) < 3 || isVolumeQueryTerm(term) ||
		strings.ContainsAny(term, `\/*?[]:`) {
		return nil, nil, false
	}
	// A term whose required filename grams are proven absent cannot match any
	// directory: report a complete empty source instead of running an expensive
	// trigram intersect that will only decline.
	if _, _, exactZero, completeGrams := completeSelfNameGramIterators(vol.index, term); completeGrams && exactZero {
		return nil, nil, true
	}
	nameMatches, ok := vol.completeNameTrigramNameTermPostingLimited(term, servicePathNameTrigramCandidateMaxIDs)
	if !ok {
		// The trigram posting is unavailable (omitted-common gram, incomplete
		// metadata).  Use the bounded name scan instead of pathComponentRootIDs,
		// which on a large volume without a resident name order falls back to a
		// full-record scan just to answer a membership question.
		if scanned := vol.scanNameTermBounded(term, servicePathNameTrigramCandidateMaxIDs); scanned != nil {
			if len(scanned) == 0 {
				return nil, nil, true
			}
			return scanned, nil, true
		}
		roots = vol.pathComponentRootIDs(term)
		return nil, roots, false
	}
	seen := make(map[int]struct{}, len(nameMatches))
	for _, id := range nameMatches {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 {
			continue
		}
		if !strings.Contains(vol.index.compactLowerNameAt(id), term) {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		roots = append(roots, id)
	}
	if len(roots) == 0 {
		roots = vol.pathComponentRootIDs(term)
	}
	sortCandidateIDs(roots, parsedQuery{}, vol.index, vol.nameOrderRanks())
	return nameMatches, roots, true
}

func heapIDsByRank(h candidateRankMaxHeap, rankOf func(int) int) []int {
	if len(h) == 0 {
		return nil
	}
	out := make([]int, len(h))
	for i := range h {
		out[i] = h[i].id
	}
	sortIDsByRank(out, rankOf)
	return out
}

func (vol *serviceVolumeIndex) extensionShapedPathTermCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CaseSensitive ||
		pq.Under != "" || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		pq.Type != "" || pq.HasModAfter || pq.Exists ||
		len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		pq.CWDBias != "" || pq.RootBias != "" ||
		countNonVolumeTerms(pq.Terms) != 1 {
		return nil, false
	}
	term := ""
	for _, candidate := range pq.Terms {
		if isVolumeQueryTerm(candidate) {
			continue
		}
		term = candidate
		break
	}
	ext, ok := extensionShapedPathTerm(term)
	if !ok {
		return nil, false
	}
	base := vol.extPosting(ext)
	if len(base) == 0 {
		return nil, false
	}
	nameMatches, ok := vol.nameTrigramNameTermPosting(term)
	if !ok {
		nameMatches = vol.nameTermPosting(term)
	}
	threshold := maxInt(4096, pq.Limit*64)
	if len(base) > threshold || len(nameMatches) > threshold {
		return nil, false
	}
	estimated := len(base) + len(nameMatches)
	for _, id := range nameMatches {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 {
			continue
		}
		estimated += vol.estimatedDescendantOrSelfCount(id)
		if estimated > serviceComponentTrigramExpansionMaxIDs {
			return nil, false
		}
	}
	seen := make(map[int]struct{}, estimated)
	out := make([]int, 0, estimated)
	add := func(id int) {
		if id < 0 || id >= vol.index.compactRecordCount() {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, id := range base {
		add(id)
	}
	for _, id := range nameMatches {
		add(id)
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 {
			continue
		}
		for _, childID := range vol.underDescendants(id) {
			add(int(childID))
			if len(out) > serviceComponentTrigramExpansionMaxIDs {
				return nil, false
			}
		}
	}
	sort.Ints(out)
	return out, true
}

func (vol *serviceVolumeIndex) extensionShapedPathTopCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CountOnly || pq.Limit <= 0 ||
		pq.CaseSensitive || pq.Under != "" || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		pq.Type != "" || pq.HasModAfter || pq.Exists ||
		pq.SortColumn != "" ||
		len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		pq.CWDBias != "" || pq.RootBias != "" ||
		countNonVolumeTerms(pq.Terms) != 1 {
		return nil, false
	}
	term := ""
	for _, candidate := range pq.Terms {
		if isVolumeQueryTerm(candidate) {
			continue
		}
		term = candidate
		break
	}
	ext, ok := extensionShapedPathTerm(term)
	if !ok {
		return nil, false
	}
	nameMatches, ok := vol.nameTrigramNameTermPosting(term)
	if !ok {
		return nil, false
	}
	if len(nameMatches) >= pq.Limit && !vol.hasDirectoryCandidate(nameMatches) {
		return topCandidateIDsByRank(append([]int(nil), nameMatches...), pq.Limit, vol.index, vol.rankForQuery(pq)), true
	}
	ids, _ := vol.extTopPosting(ext, pq.Limit, pq)
	seen := make(map[int]struct{}, len(nameMatches)+len(ids))
	out := make([]int, 0, len(nameMatches)+len(ids))
	addNameMatch := func(id int) {
		if id < 0 || id >= vol.index.compactRecordCount() {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	addPathMatch := func(id int) {
		if id < 0 || id >= vol.index.compactRecordCount() {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !vol.index.compactPathContainsTerm(id, term) {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, id := range nameMatches {
		addNameMatch(id)
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 {
			continue
		}
		for _, childID := range vol.underDescendants(id) {
			addPathMatch(int(childID))
			if len(out) > serviceComponentTrigramExpansionMaxIDs {
				return nil, false
			}
		}
	}
	for _, id := range ids {
		addPathMatch(id)
	}
	if len(out) < pq.Limit {
		return nil, false
	}
	return topCandidateIDsByRank(out, pq.Limit, vol.index, vol.rankForQuery(pq)), true
}

func (vol *serviceVolumeIndex) bareExtensionMultiPathTopCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CountOnly || pq.Limit <= 0 ||
		pq.CaseSensitive || pq.Under != "" || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		pq.Type != "" || pq.HasModAfter || pq.Exists ||
		pq.SortColumn != "" ||
		len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		pq.CWDBias != "" || pq.RootBias != "" ||
		countNonVolumeTerms(pq.Terms) < 2 {
		return nil, false
	}
	hasAnchor := false
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) || strings.ContainsAny(term, `\/*?[]:`) {
			continue
		}
		if vol.pathTermIsUsableExtensionCandidate(term) {
			continue
		}
		if len(term) >= 4 {
			hasAnchor = true
			break
		}
	}
	if !hasAnchor {
		return nil, false
	}
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) {
			continue
		}
		ext, ok := pathExtensionCandidateTerm(term)
		if !ok {
			continue
		}
		if !strings.HasPrefix(term, ".") {
			candidate, ok := vol.extPostingCountCandidate(ext)
			if !ok || candidate.len() == 0 {
				continue
			}
		}
		if candidates, ok := vol.extTopPathTermCandidates(ext, pq.Terms, pq.Limit); ok {
			return candidates, true
		}
		if candidates, ok := vol.extPathTermPostingCandidates(ext, pq.Terms, pq.Limit); ok {
			return candidates, true
		}
		if candidates, ok := vol.extPostingPathTermCandidates(ext, pq.Terms, pq.Limit); ok {
			return candidates, true
		}
	}
	return nil, false
}

func (vol *serviceVolumeIndex) pathTermIsUsableExtensionCandidate(term string) bool {
	ext, ok := pathExtensionCandidateTerm(term)
	if !ok {
		return false
	}
	if strings.HasPrefix(term, ".") {
		return true
	}
	candidate, ok := vol.extPostingCountCandidate(ext)
	return ok && candidate.len() > 0
}

func (vol *serviceVolumeIndex) hasDirectoryCandidate(ids []int) bool {
	if vol == nil || vol.index == nil {
		return true
	}
	recordCount := vol.index.compactRecordCount()
	for _, id := range ids {
		if id < 0 || id >= recordCount {
			continue
		}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted && rec.Mode&uint32(os.ModeDir) != 0 {
			return true
		}
	}
	return false
}

func extensionShapedPathTerm(term string) (string, bool) {
	if len(term) < 2 || term[0] != '.' || strings.ContainsAny(term, `\/*?[]:`) {
		return "", false
	}
	ext := strings.TrimPrefix(term, ".")
	if ext == "" || strings.Contains(ext, ".") {
		return "", false
	}
	return ext, true
}

func bareExtensionCandidateTerm(term string) bool {
	if len(term) < 2 || len(term) > 8 || strings.ContainsAny(term, `.\/*?[]:`) {
		return false
	}
	for _, r := range term {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

func pathExtensionCandidateTerm(term string) (string, bool) {
	if ext, ok := extensionShapedPathTerm(term); ok {
		return ext, true
	}
	if bareExtensionCandidateTerm(term) {
		return term, true
	}
	return "", false
}

func (vol *serviceVolumeIndex) extPathTermPostingCandidates(ext string, terms []string, limit int) ([]int, bool) {
	if vol == nil || vol.index == nil || ext == "" || limit <= 0 || len(terms) == 0 {
		return nil, false
	}
	best := []int(nil)
	bestSet := false
	checkedAnchor := false
	for _, term := range terms {
		if isVolumeQueryTerm(term) {
			continue
		}
		if vol.pathTermIsUsableExtensionCandidate(term) {
			continue
		}
		if strings.ContainsAny(term, `\/*?[]:`) {
			continue
		}
		checkedAnchor = true
		ids, ok := vol.boundedPathTermPlanSource(term)
		if !ok {
			if expanded, expandedOK := vol.pathTermPostingForExtFilter(term, serviceComponentMultiTermScanMaxIDs); expandedOK {
				ids = expanded
				ok = true
			}
		}
		if !ok {
			if vol.pathTermDefinitelyEmpty(term) {
				return []int{}, true
			}
			continue
		}
		if !bestSet || len(ids) < len(best) {
			best = ids
			bestSet = true
		}
	}
	if checkedAnchor && !bestSet {
		return nil, false
	}
	if !bestSet {
		return nil, false
	}
	out := make([]int, 0, min(limit, len(best)))
	for _, id := range best {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			continue
		}
		actual := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
		if !strings.EqualFold(actual, ext) || !vol.index.compactPathContainsAll(id, terms) {
			continue
		}
		out = append(out, id)
	}
	return topCandidateIDsByRank(out, limit, vol.index, vol.nameOrderRanks()), true
}

func (vol *serviceVolumeIndex) pathTermDefinitelyEmpty(term string) bool {
	if vol == nil || vol.index == nil || term == "" || isVolumeQueryTerm(term) {
		return false
	}
	if len(vol.pathComponentRootIDs(term)) > 0 {
		return false
	}
	if ids, ok := vol.nameTrigramNameTermPosting(term); ok {
		return len(ids) == 0
	}
	return len(vol.nameTermPosting(term)) == 0
}

func (vol *serviceVolumeIndex) pathTermPostingForExtFilter(term string, maxIDs int) ([]int, bool) {
	if vol == nil || vol.index == nil || term == "" || maxIDs <= 0 {
		return nil, false
	}
	seen := make(map[int]struct{})
	out := make([]int, 0, 256)
	add := func(id int) bool {
		if id < 0 || id >= vol.index.compactRecordCount() {
			return true
		}
		if _, exists := seen[id]; exists {
			return true
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			return true
		}
		seen[id] = struct{}{}
		out = append(out, id)
		return len(out) <= maxIDs
	}
	for _, root := range vol.pathComponentRootIDs(term) {
		if root < 0 || root >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(root)
		if rec.Deleted {
			continue
		}
		if rec.Mode&uint32(os.ModeDir) == 0 {
			if !add(root) {
				return nil, false
			}
			continue
		}
		if !vol.hasDescendantIndex() || vol.estimatedDescendantOrSelfCount(root) > maxIDs {
			return nil, false
		}
		for _, childID := range vol.underDescendantsLimited(root, maxIDs+1) {
			if !add(int(childID)) {
				return nil, false
			}
		}
	}
	nameMatches, ok := vol.completeNameTrigramNameTermPostingLimited(term, servicePathNameTrigramCandidateMaxIDs)
	if !ok {
		nameMatches = vol.nameTermPosting(term)
		if len(nameMatches) > servicePathNameTrigramCandidateMaxIDs {
			return nil, false
		}
	}
	if len(nameMatches) == 0 && len(out) == 0 {
		return []int{}, true
	}
	estimated := 0
	for _, id := range nameMatches {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			continue
		}
		if rec.Mode&uint32(os.ModeDir) == 0 {
			estimated++
		} else {
			if !vol.hasDescendantIndex() {
				return nil, false
			}
			estimated += vol.estimatedDescendantOrSelfCount(id)
		}
		if estimated > maxIDs {
			return nil, false
		}
	}
	for _, id := range nameMatches {
		if !add(id) {
			return nil, false
		}
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 {
			continue
		}
		if !vol.hasDescendantIndex() {
			return nil, false
		}
		for _, childID := range vol.underDescendantsLimited(id, maxIDs+1) {
			if !add(int(childID)) {
				return nil, false
			}
		}
	}
	sort.Ints(out)
	return out, true
}

func (vol *serviceVolumeIndex) pathPlanTermPostingLimited(term string, pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || pq.CountOnly || pq.Limit <= 0 || !pq.MatchPath ||
		pq.Under != "" || pq.Type != "" || pq.CaseSensitive || pq.CWDBias != "" || pq.RootBias != "" ||
		len(pq.Exts) > 0 || len(pq.Dirs) > 0 || len(pq.Globs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 || pq.HasModAfter || pq.Exists ||
		countNonVolumeTerms(pq.Terms) != 1 || term == "" || strings.ContainsAny(term, `\/*?[]:`) {
		return nil, false
	}
	roots := vol.pathTermRootIDs(term)
	if len(roots) == 0 || len(vol.subtreeStart) == 0 || len(vol.subtreeEnd) == 0 || len(vol.subtreeOrder) == 0 {
		return nil, false
	}
	nameMatches, ok := vol.nameTrigramNameTermPostingLimited(term, servicePathNameTrigramCandidateMaxIDs)
	if !ok {
		nameMatches = vol.nameTermPosting(term)
	}
	if len(nameMatches) > maxInt(128, pq.Limit*4) {
		return nil, false
	}
	intervals := make([]interval, 0, len(roots))
	recordCount := vol.index.compactRecordCount()
	for _, rootID := range roots {
		if rootID < 0 || rootID >= recordCount || rootID >= len(vol.subtreeStart) || rootID >= len(vol.subtreeEnd) {
			continue
		}
		start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start == ^uint32(0) || start > end || int(end) > len(vol.subtreeOrder) {
			continue
		}
		intervals = append(intervals, interval{start: int(start), end: int(end)})
	}
	if len(intervals) == 0 {
		return nil, false
	}
	intervals = mergeIntervals(intervals)
	if out, ok := vol.smallPathComponentExpansion(term, pq, intervals, nameMatches); ok {
		return out, true
	}
	return vol.topPathComponentExpansion(term, pq, intervals, nameMatches)
}

func (vol *serviceVolumeIndex) smallPathComponentExpansion(term string, pq parsedQuery, intervals []interval, nameMatches []int) ([]int, bool) {
	total := 0
	for _, iv := range intervals {
		if iv.end > iv.start {
			total += iv.end - iv.start
		}
	}
	threshold := maxInt(4096, pq.Limit*64)
	if total > threshold || len(nameMatches) > threshold {
		return nil, false
	}
	seen := make(map[int]struct{}, total+len(nameMatches))
	out := make([]int, 0, total+len(nameMatches))
	add := func(id int) {
		if id < 0 || id >= vol.index.compactRecordCount() {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !compactRecordPrecheck(rec, pq, true) {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, id := range nameMatches {
		add(id)
	}
	for _, iv := range intervals {
		for pos := iv.start; pos < iv.end; pos++ {
			if pos < 0 || pos >= len(vol.subtreeOrder) {
				continue
			}
			add(int(vol.subtreeOrder[pos]))
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	sort.Ints(out)
	return out, true
}

func (vol *serviceVolumeIndex) topPathComponentExpansion(term string, pq parsedQuery, intervals []interval, nameMatches []int) ([]int, bool) {
	if vol == nil || vol.index == nil || pq.Limit <= 0 {
		return nil, false
	}
	rankOf := candidateRanker(vol.index, vol.rankForQuery(pq))
	seen := make(map[int]struct{}, pq.Limit*4)
	h := make(candidateRankMaxHeap, 0, pq.Limit)
	add := func(id int) {
		if id < 0 || id >= vol.index.compactRecordCount() {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !compactRecordPrecheck(rec, pq, true) {
			return
		}
		seen[id] = struct{}{}
		item := candidateRankItem{id: id, rank: rankOf(id)}
		if len(h) < pq.Limit {
			heap.Push(&h, item)
			return
		}
		if item.rank < h[0].rank {
			h[0] = item
			heap.Fix(&h, 0)
		}
	}
	for _, id := range nameMatches {
		if id >= 0 && id < vol.index.compactRecordCount() && strings.Contains(vol.index.compactLowerNameAt(id), term) {
			add(id)
		}
	}
	for _, iv := range intervals {
		for pos := iv.start; pos < iv.end; pos++ {
			if pos < 0 || pos >= len(vol.subtreeOrder) {
				continue
			}
			add(int(vol.subtreeOrder[pos]))
		}
	}
	if len(h) == 0 {
		return nil, false
	}
	out := make([]int, len(h))
	for i := range h {
		out[i] = h[i].id
	}
	sort.Slice(out, func(i, j int) bool {
		return rankOf(out[i]) < rankOf(out[j])
	})
	return out, true
}

func countNonVolumeTerms(terms []string) int {
	count := 0
	for _, term := range terms {
		if isVolumeQueryTerm(term) {
			continue
		}
		count++
	}
	return count
}

func mergeIntervals(intervals []interval) []interval {
	if len(intervals) <= 1 {
		return intervals
	}
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].start == intervals[j].start {
			return intervals[i].end < intervals[j].end
		}
		return intervals[i].start < intervals[j].start
	})
	out := intervals[:1]
	for _, iv := range intervals[1:] {
		last := &out[len(out)-1]
		if iv.start <= last.end {
			if iv.end > last.end {
				last.end = iv.end
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

func intervalContainsPosition(intervals []interval, pos int) bool {
	if pos < 0 {
		return false
	}
	i := sort.Search(len(intervals), func(i int) bool {
		return intervals[i].end > pos
	})
	return i < len(intervals) && intervals[i].start <= pos && pos < intervals[i].end
}

func (plan candidatePlan) execute() []int {
	if plan.empty {
		return []int{}
	}
	sortCandidatePlanSourcesByLen(plan.sources)
	out := plan.sources[0].materialize()
	for _, source := range plan.sources[1:] {
		out = source.intersect(out)
		if len(out) == 0 {
			break
		}
	}
	if plan.vol != nil && len(plan.vol.recentIDs) > 0 {
		out = append(out, mapKeys(plan.vol.recentIDs)...)
		sort.Ints(out)
		out = uniqueSortedInts(out)
	}
	if plan.underPathFallback != "" {
		out = plan.filterUnderPath(out)
	}
	return out
}

type localVerifiedTopItem struct {
	id    int
	entry Entry
}

type localVerifiedTopHeap struct {
	items []localVerifiedTopItem
	pq    parsedQuery
}

func (h localVerifiedTopHeap) Len() int { return len(h.items) }

func (h localVerifiedTopHeap) Less(i, j int) bool {
	if n := compareSearchAllEntries(h.items[i].entry, h.items[j].entry, h.pq); n != 0 {
		return n > 0
	}
	return h.items[i].id > h.items[j].id
}

func (h localVerifiedTopHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }

func (h *localVerifiedTopHeap) Push(x any) { h.items = append(h.items, x.(localVerifiedTopItem)) }

func (h *localVerifiedTopHeap) Pop() any {
	old := h.items
	n := len(old)
	x := old[n-1]
	h.items = old[:n-1]
	return x
}

// executeTop is the bounded local equivalent of the global boolean iterator.
// It only handles a single OR union made from already sorted posting slices;
// all other plans retain the exact materializing/fallback path.
func (plan candidatePlan) executeTop(pq parsedQuery) ([]int, int, bool) {
	if plan.vol == nil || plan.vol.index == nil || pq.CountOnly || pq.Limit <= 0 ||
		len(plan.sources) != 1 || len(plan.sources[0].union) == 0 ||
		len(plan.vol.recentIDs) != 0 || plan.underPathFallback != "" ||
		pq.RootBias != "" || pq.CWDBias != "" {
		return nil, 0, false
	}
	parts := plan.sources[0].union
	for _, part := range parts {
		if part.hasPosting || len(part.union) != 0 || len(part.roots) != 0 {
			return nil, 0, false
		}
	}
	h := &localVerifiedTopHeap{pq: pq}
	heap.Init(h)
	orderedParts := make([][]int, len(parts))
	rankRanks := plan.vol.rankForQuery(pq)
	rankOf := candidateRanker(plan.vol.index, rankRanks)
	for i, part := range parts {
		// A posting is a candidate source, not necessarily an exact predicate
		// source (name trigrams in particular can contain false positives). Keep
		// the complete ordered stream and stop only after the next rank cannot
		// beat the current verified threshold. Truncating before verification can
		// hide a real top-N match behind false positives.
		orderedParts[i] = append([]int(nil), part.ids...)
		sortCandidateIDs(orderedParts[i], pq, plan.vol.index, rankRanks)
	}
	positions := make([]int, len(orderedParts))
	pathCache := make(map[int]string)
	last, haveLast := 0, false
	scanned := 0
	for {
		best := -1
		for i, ids := range orderedParts {
			if positions[i] >= len(ids) {
				continue
			}
			id := ids[positions[i]]
			if best < 0 || compareCandidateRank(id, orderedParts[best][positions[best]], rankOf) < 0 {
				best = i
			}
		}
		if best < 0 {
			break
		}
		id := orderedParts[best][positions[best]]
		if h.Len() == pq.Limit && compareCandidateRank(id, h.items[0].id, rankOf) > 0 {
			break
		}
		positions[best]++
		if haveLast && id == last {
			continue
		}
		last, haveLast = id, true
		scanned++
		if scanned&1023 == 0 && queryCanceled(pq) {
			return nil, scanned, false
		}
		entry, ok := compactCandidateEntryIfMatch(plan.vol.index, pq, id, pathCache, true, false)
		if !ok {
			continue
		}
		item := localVerifiedTopItem{id: id, entry: entry}
		if h.Len() < pq.Limit {
			heap.Push(h, item)
			continue
		}
		if compareSearchAllEntries(item.entry, h.items[0].entry, pq) < 0 ||
			(compareSearchAllEntries(item.entry, h.items[0].entry, pq) == 0 && item.id < h.items[0].id) {
			h.items[0] = item
			heap.Fix(h, 0)
		}
	}
	out := make([]int, len(h.items))
	for i, item := range h.items {
		out[i] = item.id
	}
	sortCandidateIDs(out, pq, plan.vol.index, plan.vol.rankForQuery(pq))
	return out, scanned, true
}

func compareCandidateRank(a, b int, rankOf func(int) int) int {
	if n := cmp.Compare(rankOf(a), rankOf(b)); n != 0 {
		return n
	}
	return cmp.Compare(a, b)
}

func (plan candidatePlan) executeUnionCount(pq parsedQuery, hidden hiddenBaseIDs) (int, int, bool) {
	if plan.vol == nil || plan.vol.index == nil || len(plan.sources) != 1 ||
		len(plan.sources[0].union) == 0 || len(plan.vol.recentIDs) != 0 ||
		plan.underPathFallback != "" || pq.RootBias != "" || pq.CWDBias != "" {
		return 0, 0, false
	}
	parts := plan.sources[0].union
	for _, part := range parts {
		if part.hasPosting || len(part.union) != 0 || len(part.roots) != 0 {
			return 0, 0, false
		}
	}
	positions := make([]int, len(parts))
	last, haveLast := 0, false
	pathCache := make(map[int]string)
	count, scanned := 0, 0
	for {
		best := -1
		for i, part := range parts {
			if positions[i] >= len(part.ids) {
				continue
			}
			id := part.ids[positions[i]]
			if best < 0 || id < parts[best].ids[positions[best]] {
				best = i
			}
		}
		if best < 0 {
			break
		}
		id := parts[best].ids[positions[best]]
		positions[best]++
		if haveLast && id == last {
			continue
		}
		last, haveLast = id, true
		scanned++
		if scanned&1023 == 0 && queryCanceled(pq) {
			return 0, scanned, false
		}
		if id < 0 || id >= plan.vol.index.compactRecordCount() || (!hidden.empty() && hidden.contains(id)) {
			continue
		}
		matched := false
		if queryNeedsPath(pq) {
			_, matched = compactCandidateEntryIfMatch(plan.vol.index, pq, id, pathCache, true, false)
		} else {
			rec := plan.vol.index.compactRecord(id)
			matched = !rec.Deleted && plan.vol.recordMatchesNonPath(id, rec, pq)
		}
		if matched {
			count++
		}
	}
	return count, scanned, true
}

func (plan candidatePlan) sourceSummary() string {
	if plan.empty {
		return "empty"
	}
	if len(plan.sources) == 0 {
		return "none"
	}
	names := make([]string, 0, len(plan.sources))
	for _, source := range plan.sources {
		names = append(names, source.name)
	}
	sort.Strings(names)
	return strings.Join(names, "+")
}

func (plan candidatePlan) traceTerms() []traceTerm {
	if len(plan.sources) == 0 {
		return nil
	}
	volume := ""
	if plan.vol != nil && plan.vol.index != nil {
		volume = plan.vol.index.Volume
	}
	out := make([]traceTerm, 0, len(plan.sources))
	for _, source := range plan.sources {
		if len(source.union) > 0 {
			for _, part := range source.union {
				out = append(out, traceTermForCandidateSource(part, volume))
			}
			continue
		}
		out = append(out, traceTermForCandidateSource(source, volume))
	}
	return out
}

func (plan candidatePlan) filterUnderPath(ids []int) []int {
	if plan.vol == nil || plan.vol.index == nil || plan.underPathFallback == "" || len(ids) == 0 {
		return ids
	}
	pathCache := make(map[int]string)
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		if id < 0 || id >= plan.vol.index.compactRecordCount() {
			continue
		}
		path := plan.vol.index.reconstructCompactPathCached(id, pathCache)
		if pathUnder(path, plan.underPathFallback) {
			out = append(out, id)
		}
	}
	return out
}

// orGroupPosting returns the union of candidate postings for an OR group when
// every alternative is cheaply postable. The bool is false when at least one
// alternative cannot be turned into a posting, in which case the caller should
// let the group be verified against the broader candidate set instead.
func (vol *serviceVolumeIndex) orGroupPosting(group []parsedQuery, matchPath bool) ([]int, bool) {
	source, ok := vol.orGroupPlanSource(group, matchPath)
	if !ok {
		return nil, false
	}
	return source.materialize(), true
}

func (vol *serviceVolumeIndex) orGroupPlanSource(group []parsedQuery, matchPath bool) (candidatePlanSource, bool) {
	parts := make([]candidatePlanSource, 0, len(group))
	for _, alt := range group {
		source, ok := vol.altPlanSource(alt, matchPath)
		if !ok {
			return candidatePlanSource{}, false
		}
		parts = append(parts, source)
	}
	return candidatePlanSource{name: "or-group", union: parts}, true
}

// altPosting returns a posting for a single OR alternative if it is a lone
// ext:, simple glob extension, or plain term. Returns ok=false otherwise.
func (vol *serviceVolumeIndex) altPosting(alt parsedQuery, matchPath bool) ([]int, bool) {
	source, ok := vol.altPlanSource(alt, matchPath)
	if !ok {
		return nil, false
	}
	return source.materialize(), true
}

func (vol *serviceVolumeIndex) altPlanSource(alt parsedQuery, matchPath bool) (candidatePlanSource, bool) {
	switch {
	case len(alt.Exts) == 1 && alt.isOnly("ext"):
		ext := alt.Exts[0]
		if candidate, ok := vol.extPostingCountCandidate(ext); ok {
			return candidatePlanSource{name: "ext:" + ext, posting: candidate, hasPosting: true}, true
		}
		return candidatePlanSource{name: "ext:" + ext, ids: uniqueSortedInts(vol.extPosting(ext))}, true
	case len(alt.Globs) == 1 && alt.isOnly("glob"):
		if exts, ok := simpleGlobExts(alt.Globs); ok && len(exts) == 1 {
			ext := exts[0]
			if candidate, ok := vol.extPostingCountCandidate(ext); ok {
				return candidatePlanSource{name: "glob-ext:" + ext, posting: candidate, hasPosting: true}, true
			}
			return candidatePlanSource{name: "glob-ext:" + ext, ids: uniqueSortedInts(vol.extPosting(ext))}, true
		}
		return candidatePlanSource{}, false
	case len(alt.Terms) == 1 && alt.isOnly("term"):
		if matchPath {
			term := alt.Terms[0]
			return candidatePlanSource{name: "path-term:" + term, ids: uniqueSortedInts(vol.pathTermPosting(term))}, true
		}
		term := alt.Terms[0]
		return candidatePlanSource{name: "term:" + term, ids: uniqueSortedInts(vol.nameTermPosting(term))}, true
	case len(alt.Parents) == 1 && alt.isOnly("parent"):
		parent := alt.Parents[0]
		return candidatePlanSource{name: "parent:" + parent, ids: uniqueSortedInts(vol.parentIDs(parent))}, true
	case len(alt.AttrFilters) == 1 && alt.isOnly("attrib"):
		mask := alt.AttrFilters[0]
		ids, ok := vol.attrIDsForMask(mask)
		if !ok {
			return candidatePlanSource{}, false
		}
		return candidatePlanSource{name: "attrib:" + attribMaskString(mask), ids: uniqueSortedInts(ids)}, true
	default:
		return candidatePlanSource{}, false
	}
}

// isOnly reports whether the alternative carries exactly one kind of constraint
// (named by kind) and nothing else, so it can be turned into a single posting.
func (alt parsedQuery) isOnly(kind string) bool {
	counts := map[string]int{
		"ext":    len(alt.Exts),
		"glob":   len(alt.Globs),
		"term":   len(alt.Terms),
		"parent": len(alt.Parents),
		"attrib": len(alt.AttrFilters),
	}
	other := len(alt.Dirs) + len(alt.Regexps) + len(alt.SizeFilters) +
		len(alt.DateFilters) + len(alt.OrGroups) + len(alt.NotGroups)
	if alt.Type != "" {
		other++
	}
	if other != 0 {
		return false
	}
	for k, v := range counts {
		if k == kind {
			continue
		}
		if v != 0 {
			return false
		}
	}
	return true
}

func (vol *serviceVolumeIndex) unionUnderDescendants(roots []int) []int {
	if len(roots) == 0 {
		return nil
	}
	seen := make(map[int]struct{}, 256)
	out := make([]int, 0, 256)
	for _, root := range roots {
		for _, id := range vol.underDescendants(root) {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out
}

func shouldUseUnderPlanSource(underEstimatedSize int, sources []candidatePlanSource) bool {
	if len(sources) == 0 {
		return true
	}
	if underEstimatedSize < 0 {
		return false
	}
	smallest := sources[0].len()
	for _, source := range sources[1:] {
		if source.len() < smallest {
			smallest = source.len()
		}
	}
	return underEstimatedSize <= smallest
}

func (vol *serviceVolumeIndex) estimateUnderDescendantCount(roots []int) int {
	if len(roots) == 0 {
		return 0
	}
	total := 0
	for _, root := range roots {
		if root < 0 || root >= len(vol.subtreeStart) || root >= len(vol.subtreeEnd) || len(vol.subtreeOrder) == 0 {
			return -1
		}
		start, end := vol.subtreeStart[root], vol.subtreeEnd[root]
		if start == ^uint32(0) || start > end {
			return -1
		}
		total += int(end - start)
	}
	return total
}
