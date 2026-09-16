package main

import (
	"cmp"
	"container/heap"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

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
	// Single bare term with no path scope and no other filters: count directly
	// over records in parallel without materializing the candidate slice.  A
	// short/broad term like `x` matches millions of names, so building the full
	// name posting just to count it is wasteful.
	if terms := nonVolumeTerms(pq.Terms); len(terms) == 1 && !pq.MatchPath &&
		pq.Type == "" && pq.Under == "" && !pq.Exists && !pq.HasModAfter &&
		len(pq.Exts) == 0 && len(pq.Dirs) == 0 && len(pq.Globs) == 0 &&
		len(pq.Regexps) == 0 && len(pq.RegexTerms) == 0 && len(pq.Parents) == 0 &&
		len(pq.SizeFilters) == 0 && len(pq.DateFilters) == 0 && len(pq.AttrFilters) == 0 &&
		len(pq.OrGroups) == 0 && len(pq.NotGroups) == 0 &&
		pq.CWDBias == "" && pq.RootBias == "" && !pq.CaseSensitive {
		if count, ok := vol.countBareTermParallel(terms[0], hidden, pq); ok {
			pq.Trace.addTerm(traceTerm{Term: terms[0], Kind: "name-substring", Source: "parallel-name-count", CountHint: count, Exact: true})
			pq.Trace.setSource("parallel-name-count", count)
			pq.Trace.setComplete(true)
			return count, true
		}
	}
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
	}
	// Glob literals are a safe name-substring prefilter for complex globs (a
	// record matching `glob:*foo*` necessarily has `foo` in its name), so add
	// them as sources regardless of path scope.  This narrows the candidate set
	// before the full glob is verified, avoiding a full-volume scan for broad
	// globs that are not reducible to a single extension posting.
	if !globsOK {
		for _, term := range globLiteralTerms(pq.Globs, pq.CaseSensitive) {
			if list := vol.nameTermPosting(term); len(list) > 0 {
				if !addRequired("glob-literal:"+term, list) {
					return plan, true
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
	workers := min(max(1, recordCountWorkers(recordCount)), 16)
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
	return vol.boundedScanCandidatesFiltered(pq, nil)
}

// boundedScanCandidatesFiltered is boundedScanCandidates with an optional cheap
// posting membership pre-filter.  Records outside the filter are skipped before
// the full entry verification, so a count or non-order-ready scan for a query
// with a cheap exact superset (ext:, a bounded type:dir subtree, or a required
// regex literal run) touches only the candidate subset instead of every record.
func (vol *serviceVolumeIndex) boundedScanCandidatesFiltered(pq parsedQuery, filter *boundedScanMembershipFilter) ([]int, bool) {
	if vol == nil || vol.index == nil {
		return nil, false
	}
	if filter != nil && filter.members == nil && filter.source.hasPosting {
		ids := filter.source.posting.materialize()
		filter.members = make(map[int]struct{}, len(ids))
		for _, id := range ids {
			filter.members[int(id)] = struct{}{}
		}
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
			if filter != nil && !filter.contains(id) {
				continue
			}
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

	workers := min(max(1, recordCountWorkers(recordCount)), 16)
	if workers <= 1 || recordCount < 8192 {
		out := make([]int, 0, min(recordCount, 1024))
		cache := make(map[int]string)
		for pos := 0; pos < compactUint32OrderLen(order, recordCount); pos++ {
			if pos&1023 == 0 && queryCanceled(pq) {
				return nil, false
			}
			id := compactUint32OrderAt(order, pos)
			if filter != nil && !filter.contains(id) {
				continue
			}
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
				if filter != nil && !filter.contains(id) {
					continue
				}
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

// boundedScanMembershipFilter pre-filters the bounded name/id-order scan to a
// selective ext/glob-ext posting so broad queries with a cheap extension filter
// do not walk every record.  The scan order is unchanged (id order when the
// resident name order is absent, name order otherwise), so the top-N semantics
// of boundedScanCandidatesHiddenTop are preserved exactly.
type boundedScanMembershipFilter struct {
	source  candidatePlanSource
	members map[int]struct{}
}

func (f *boundedScanMembershipFilter) contains(id int) bool {
	if f == nil || f.members == nil {
		return true
	}
	_, ok := f.members[id]
	return ok
}

// boundedScanCandidatesHiddenTopFiltered is boundedScanCandidatesHiddenTop with
// an optional cheap posting membership pre-filter.  Records outside the filter
// are skipped without verifying the full entry, so a query like "test ext:py"
// scans only the .py subset of the volume while preserving the exact id/name
// order of the unfiltered bounded scan.
func (vol *serviceVolumeIndex) boundedScanCandidatesHiddenTopFiltered(pq parsedQuery, hidden hiddenBaseIDs, limit int, filter *boundedScanMembershipFilter) ([]int, bool) {
	if vol == nil || vol.index == nil || limit <= 0 {
		return nil, false
	}
	if filter != nil && filter.members == nil && filter.source.hasPosting {
		ids := filter.source.posting.materialize()
		filter.members = make(map[int]struct{}, len(ids))
		for _, id := range ids {
			filter.members[int(id)] = struct{}{}
		}
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
		if filter != nil && !filter.contains(id) {
			continue
		}
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

// boundedScanPrefilter tries the cheap exact superset pre-filters for the
// bounded scan in priority order: ext:/glob-ext:, a bounded type:dir subtree,
// and a required regex literal run.  It returns (exactEmpty, filterOK):
// exactEmpty means the query provably matches nothing on this volume (caller
// short-circuits), filterOK means a membership filter was set, and neither
// means no filter applies and the unfiltered scan runs.
func (vol *serviceVolumeIndex) boundedScanPrefilter(pq parsedQuery, filter **boundedScanMembershipFilter) (exactEmpty, filterOK bool) {
	if vol == nil || vol.index == nil || pq.CaseSensitive {
		return false, false
	}
	if source, hasSource := vol.planExtFilterSource(pq); hasSource {
		*filter = &boundedScanMembershipFilter{source: source}
		return false, true
	}
	if dirFilter, hasDirFilter := vol.planDirSubtreeFilter(pq); hasDirFilter {
		*filter = dirFilter
		return false, true
	}
	if regexFilter, regexEmpty, hasRegexFilter := vol.planRegexLiteralFilter(pq); hasRegexFilter {
		if regexEmpty {
			return true, true
		}
		*filter = regexFilter
		return false, true
	}
	return false, false
}

// planExtFilterSource returns a candidatePlanSource whose posting is a cheap,
// exact superset pre-filter (ext: or glob-ext:) for the query, when one exists.
func (vol *serviceVolumeIndex) planExtFilterSource(pq parsedQuery) (candidatePlanSource, bool) {
	if vol == nil || vol.index == nil || pq.CaseSensitive {
		return candidatePlanSource{}, false
	}
	for _, ext := range pq.Exts {
		if candidate, ok := vol.extPostingCountCandidate(ext); ok {
			// Cap the posting so a very common extension (e.g. .dll on a huge
			// index) does not materialize a multi-hundred-MB membership map.
			// Above the cap the unfiltered scan is no worse, and skipping the
			// filter avoids a pathological memory spike.
			if candidate.len() > serviceComponentMultiTermScanMaxIDs {
				continue
			}
			return candidatePlanSource{posting: candidate, hasPosting: true}, true
		}
	}
	if globExts, ok := simpleGlobExts(pq.Globs); ok && len(globExts) == 1 {
		if candidate, ok := vol.extPostingCountCandidate(globExts[0]); ok {
			if candidate.len() > serviceComponentMultiTermScanMaxIDs {
				return candidatePlanSource{}, false
			}
			return candidatePlanSource{posting: candidate, hasPosting: true}, true
		}
	}
	return candidatePlanSource{}, false
}

// regexRequiredLiteral finds a literal run that is guaranteed to appear in any
// match of the regex: a depth-0 run not inside a top-level alternation and not
// made optional by a trailing `?`/`*`/`{` quantifier.  Such a run is a safe
// superset pre-filter for the bounded scan (every matching record's path must
// contain it).  Returns "" when no required literal can be proven.
func regexRequiredLiteral(pat string) string {
	pat = strings.TrimPrefix(pat, "(?i)")
	depth := 0
	required := ""
	var run []byte
	escaped := false
	inClass := false
	flush := func() {
		if depth == 0 && len(run) >= 2 {
			required = string(run)
		}
		run = run[:0]
	}
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		if escaped {
			escaped = false
			// An escaped literal rune (e.g. \_) continues the run; any other
			// escape ends it.  Note this branch is reached with c == the
			// escaped character.
			if isRegexLiteralRune(rune(c)) && depth == 0 {
				run = append(run, c)
			} else {
				flush()
			}
			continue
		}
		switch c {
		case '\\':
			escaped = true
		case '[':
			flush()
			inClass = true
		case ']':
			inClass = false
		case '(', '|':
			flush()
			if c == '(' {
				depth++
			}
		case ')':
			flush()
			if depth > 0 {
				depth--
			}
		case '?', '*', '+', '{':
			// A quantifier makes the preceding run optional/repeatable; a run
			// with a preceding quantifier is not provably required.
			flush()
		case '.':
			if i+1 < len(pat) && (pat[i+1] == '*' || pat[i+1] == '+') {
				flush()
				i++
				continue
			}
			flush()
		default:
			if !inClass && isRegexLiteralRune(rune(c)) {
				if depth == 0 {
					run = append(run, c)
				} else {
					flush()
				}
			} else {
				flush()
			}
		}
	}
	flush()
	return required
}

// planRegexLiteralFilter builds a bounded-scan membership pre-filter from a
// required regex literal run.  A record matching the regex must contain the
// run in its path, so the run's path posting is an exact superset of the match
// set; the bounded scan then only regex-verifies those records.  This turns
// rare-match regexes like `regex:README\.(md|txt)$` from a full-volume scan
// (minutes) into a scan of just the README-containing records (sub-second).
// empty reports that no record contains the required literal, so the query is
// provably a zero-match on this volume and the caller can short-circuit.
func (vol *serviceVolumeIndex) planRegexLiteralFilter(pq parsedQuery) (filter *boundedScanMembershipFilter, empty bool, ok bool) {
	if vol == nil || vol.index == nil || pq.CaseSensitive || len(pq.Regexps) != 1 ||
		len(pq.RegexTerms) == 0 || !pq.MatchPath {
		return nil, false, false
	}
	disjuncts := regexRequiredLiteralAlternatives(pq.Regexps[0].String())
	if len(disjuncts) == 0 {
		return nil, false, false
	}
	members := make(map[int]struct{}, 64)
	kept := 0
	for _, runs := range disjuncts {
		hasUsable := false
		var best []int
		for _, run := range runs {
			if len(run) < 3 || strings.ContainsAny(run, `\/*?[]:`) {
				continue
			}
			hasUsable = true
			ids := vol.pathTermPosting(run)
			if len(ids) == 0 {
				// This run is absent from the volume; another run (or another
				// disjunct) may still narrow the scan, so do not treat it as an
				// empty query here.
				continue
			}
			if best == nil || len(ids) < len(best) {
				best = ids
			}
		}
		if !hasUsable {
			// This disjunct has no run we can prove required.  Skipping it
			// would let a match through with none of the chosen literals, so
			// no sound filter can be built from the pattern.
			return nil, false, false
		}
		if best == nil {
			// Every usable run of this disjunct is absent, so this branch can
			// never match.  The union of the remaining branches is still an
			// exact superset of the match set.
			continue
		}
		if len(members)+len(best) > serviceComponentMultiTermScanMaxIDs {
			return nil, false, false
		}
		for _, id := range best {
			members[id] = struct{}{}
		}
		kept++
	}
	if kept == 0 {
		// Every disjunct was proven impossible, so the query matches nothing on
		// this volume.
		return nil, true, true
	}
	return &boundedScanMembershipFilter{members: members}, false, true
}

// planDirSubtreeFilter builds a membership pre-filter for `type:dir <term>`
// path queries: every matching directory is either a `term`-named directory or
// a descendant of one, so the union of each term root's descendant interval is
// an exact superset of the match set.  The filter is only built when the total
// descendant count is bounded, so a huge `term` subtree (e.g. a root directory)
// falls back to the unfiltered scan rather than materializing millions of ids.
// The scan order is unchanged, preserving bounded-scan top-N semantics.
func (vol *serviceVolumeIndex) planDirSubtreeFilter(pq parsedQuery) (*boundedScanMembershipFilter, bool) {
	if vol == nil || vol.index == nil || pq.CaseSensitive || pq.Type != "dir" ||
		len(pq.Terms) != 1 || len(pq.Exts) != 0 || len(pq.Globs) != 0 ||
		len(pq.Dirs) != 0 || len(pq.Regexps) != 0 || len(pq.OrGroups) != 0 || len(pq.NotGroups) != 0 ||
		len(pq.SizeFilters) != 0 || len(pq.DateFilters) != 0 || len(pq.AttrFilters) != 0 ||
		pq.Under != "" || pq.HasModAfter || pq.Exists {
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
	roots := vol.pathTermRootIDs(term)
	if len(roots) == 0 {
		return nil, false
	}
	if len(vol.subtreeOrder) == 0 {
		return nil, false
	}
	total := 0
	for _, root := range roots {
		if root < 0 || root >= len(vol.subtreeStart) || root >= len(vol.subtreeEnd) {
			continue
		}
		start, end := vol.subtreeStart[root], vol.subtreeEnd[root]
		if start != ^uint32(0) && start <= end {
			total += int(end - start)
			if total > serviceComponentTrigramExpansionMaxIDs {
				return nil, false
			}
		}
	}
	if total == 0 {
		return nil, false
	}
	members := make(map[int]struct{}, total)
	for _, root := range roots {
		if root < 0 || root >= len(vol.subtreeStart) || root >= len(vol.subtreeEnd) {
			continue
		}
		start, end := vol.subtreeStart[root], vol.subtreeEnd[root]
		if start == ^uint32(0) || start > end || int(end) > len(vol.subtreeOrder) {
			continue
		}
		for _, id32 := range vol.subtreeOrder[start:end] {
			members[int(id32)] = struct{}{}
		}
	}
	return &boundedScanMembershipFilter{members: members}, true
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
	return max(1, recordCount/250_000)
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
		if ids, ok := vol.extTopPosting(ext, max(pq.Limit*8, pq.Limit), pq); ok {
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
	threshold := max(4096, pq.Limit*64)
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
	if len(nameMatches) > max(128, pq.Limit*4) {
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
	threshold := max(4096, pq.Limit*64)
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
