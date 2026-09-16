package main

import (
	"os"
	"sort"
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
