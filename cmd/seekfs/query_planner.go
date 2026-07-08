package main

import (
	"container/heap"
	"os"
	"path/filepath"
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
		pq.Trace.setSource("planned:ext-top", len(out))
		return out, true
	}
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		return nil, false
	}
	out := plan.execute()
	if compactCandidateCanSkipEntryMatches(pq, true) && pq.Limit > 0 {
		out = topCandidateIDsByRank(out, pq.Limit, vol.index, vol.nameOrderRanks())
	} else {
		sortCandidateIDs(out, pq, vol.index, vol.nameOrderRanks())
	}
	pq.Trace.setSource("planned:"+plan.sourceSummary(), len(out))
	return out, true
}

func (vol *serviceVolumeIndex) exactTopPlannedCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.queryIndex == nil || pq.Limit <= 0 ||
		len(pq.Exts) != 1 || len(pq.Globs) > 0 || len(pq.Dirs) > 0 ||
		pq.Type != "" || pq.Under != "" || pq.HasModAfter || pq.Exists ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 {
		return nil, false
	}
	terms := nonVolumeTerms(pq.Terms)
	if len(terms) > 0 {
		return nil, false
	}
	ids, ok := vol.extTopPosting(pq.Exts[0], pq.Limit)
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
	ids := plan.execute()
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
	if len(pq.Dirs) > 0 || len(pq.Regexps) > 0 {
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
	if len(plan.sources) == 0 && pq.MatchPath && hasNonVolumeTerm(pq.Terms) {
		for _, term := range pathPlanProbeTerms(pq.Terms) {
			ids, ok := vol.boundedPathTermPlanSource(term)
			if !ok {
				continue
			}
			if !addRequired("path-term:"+term, ids) {
				return plan, true
			}
		}
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
	// Only build broad term postings when no narrower source exists.
	if len(plan.sources) == 0 {
		if pq.MatchPath && hasNonVolumeTerm(pq.Terms) {
			// Path mode: build a path posting for only the single most selective
			// term and verify the rest in entryMatches. Building a path posting
			// for a broad term (e.g. "src") materializes millions of ids that
			// exceed the posting cache cap and are rebuilt on every call. If no
			// term is selective enough AND there is no other source (under /
			// regex literals) to bound the query, decline so the search uses the
			// streaming name-order scan instead — how Everything scans columns.
			if len(underRoots) == 0 {
				return plan, false
			}
		} else if !pq.MatchPath {
			for _, term := range pq.Terms {
				if !addRequired("term:"+term, vol.namePlanTermPosting(term)) {
					return plan, true
				}
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
	ids, ok := vol.completeNameTrigramPathTermPosting(term)
	if ok && len(ids) <= serviceComponentTrigramExpansionMaxIDs {
		return ids, true
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
		sortCandidateIDs(out, pq, vol.index, vol.nameOrderRanks())
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
	sortCandidateIDs(out, pq, vol.index, vol.nameOrderRanks())
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
	order := vol.mappedOrCompactNameOrder()
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
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 ||
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
			return exact
		}
	}
	return vol.pathTermPosting(term)
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
		sortCandidateIDs(candidates, pq, vol.index, vol.nameOrderRanks())
		return candidates, true
	}
	return nil, false
}

func (vol *serviceVolumeIndex) limitedDottedPathScanCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CountOnly || pq.Limit <= 0 ||
		pq.CaseSensitive || pq.Under != "" || pq.Type != "" || pq.HasModAfter || pq.Exists ||
		pq.CWDBias != "" || pq.RootBias != "" ||
		len(pq.Exts) > 0 || len(pq.Dirs) > 0 || len(pq.Globs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 ||
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
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 ||
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
	rankOf := candidateRanker(vol.index, vol.nameOrderRanks())
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
		if ids, ok := vol.extTopPosting(ext, maxInt(pq.Limit*8, pq.Limit)); ok {
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
	nameMatches, ok := vol.completeNameTrigramNameTermPostingLimited(term, servicePathNameTrigramCandidateMaxIDs)
	if !ok {
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
	sort.Slice(out, func(i, j int) bool {
		return rankOf(out[i]) < rankOf(out[j])
	})
	return out
}

func (vol *serviceVolumeIndex) extensionShapedPathTermCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CaseSensitive ||
		pq.Under != "" || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		pq.Type != "" || pq.HasModAfter || pq.Exists ||
		len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 ||
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
		len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 ||
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
		return topCandidateIDsByRank(append([]int(nil), nameMatches...), pq.Limit, vol.index, vol.nameOrderRanks()), true
	}
	ids, _ := vol.extTopPosting(ext, pq.Limit)
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
	return topCandidateIDsByRank(out, pq.Limit, vol.index, vol.nameOrderRanks()), true
}

func (vol *serviceVolumeIndex) bareExtensionMultiPathTopCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CountOnly || pq.Limit <= 0 ||
		pq.CaseSensitive || pq.Under != "" || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		pq.Type != "" || pq.HasModAfter || pq.Exists ||
		len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 ||
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
	rankOf := candidateRanker(vol.index, vol.nameOrderRanks())
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
	sort.Slice(plan.sources, func(i, j int) bool {
		if plan.sources[i].len() == plan.sources[j].len() {
			return plan.sources[i].name < plan.sources[j].name
		}
		return plan.sources[i].len() < plan.sources[j].len()
	})
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
	union := make([]int, 0, 64)
	parts := make([]candidatePlanSource, 0, len(group))
	for _, alt := range group {
		source, ok := vol.altPlanSource(alt, matchPath)
		if !ok {
			return candidatePlanSource{}, false
		}
		if source.hasPosting || len(source.union) > 0 {
			parts = append(parts, source)
			continue
		}
		union = append(union, source.ids...)
	}
	if len(parts) > 0 {
		if len(union) > 0 {
			sort.Ints(union)
			parts = append(parts, candidatePlanSource{name: "or-group-ids", ids: uniqueSortedInts(union)})
		}
		return candidatePlanSource{name: "or-group", union: parts}, true
	}
	sort.Ints(union)
	return candidatePlanSource{name: "or-group", ids: uniqueSortedInts(union)}, true
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
	default:
		return candidatePlanSource{}, false
	}
}

// isOnly reports whether the alternative carries exactly one kind of constraint
// (named by kind) and nothing else, so it can be turned into a single posting.
func (alt parsedQuery) isOnly(kind string) bool {
	counts := map[string]int{
		"ext":  len(alt.Exts),
		"glob": len(alt.Globs),
		"term": len(alt.Terms),
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
