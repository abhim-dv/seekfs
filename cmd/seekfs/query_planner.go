package main

import (
	"container/heap"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
	name string
	ids  []int
}

func (vol *serviceVolumeIndex) plannedCandidates(pq parsedQuery) ([]int, bool) {
	if compactCandidateCanSkipEntryMatches(pq, true) && pq.Limit > 0 {
		if out, ok := vol.exactTopPlannedCandidates(pq); ok {
			pq.Trace.setSource("planned:ext-top", len(out))
			return out, true
		}
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
		return vol.extTopPathTermCandidates(pq.Exts[0], terms, pq.Limit)
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

	if vol.pathCache == nil {
		vol.pathCache = make(map[int]string)
	}
	for _, id := range ids {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !compactRecordPrecheck(rec, pq, pq.MatchPath) {
			continue
		}
		path := vol.index.reconstructCompactPathCached(id, vol.pathCache)
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
		if !addRequired("ext:"+ext, vol.extPosting(ext)) {
			return plan, true
		}
	}
	globExts, globsOK := simpleGlobExts(pq.Globs)
	if globsOK {
		for _, ext := range globExts {
			if !addRequired("glob-ext:"+ext, vol.extPosting(ext)) {
				return plan, true
			}
		}
	} else {
		for _, ext := range complexGlobExts(pq.Globs) {
			if !addRequired("glob-ext:"+ext, vol.extPosting(ext)) {
				return plan, true
			}
		}
	}
	if pq.Type == "dir" {
		dirs := []int(nil)
		if vol.queryIndex != nil {
			dirs = uint32sToInts(vol.queryIndex.dirs)
			dirs = vol.withRecentCandidates(dirs, 0, func(rec CompactRecord) bool {
				return rec.Mode&uint32(os.ModeDir) != 0
			})
		}
		if !addRequired("type:dir", dirs) {
			return plan, true
		}
	}
	for _, dir := range pq.Dirs {
		if !addRequired("dir:"+dir, vol.pathComponentPosting(dir)) {
			return plan, true
		}
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
		ids, ok := vol.orGroupPosting(group, pq.MatchPath)
		if !ok {
			continue
		}
		if !addRequired("or-group", ids) {
			return plan, true
		}
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
			term, ok := vol.mostSelectivePathTerm(pq)
			if !ok {
				if len(underRoots) == 0 {
					return plan, false
				}
			} else if limited, ok := vol.pathPlanTermPostingLimited(term, pq); ok {
				plan.sources = append(plan.sources, candidatePlanSource{name: "term-limited:" + term, ids: limited})
			} else if !addRequired("term:"+term, vol.pathPlanTermPosting(term)) {
				return plan, true
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
		if !addRequired("under", vol.unionUnderDescendants(underRoots)) {
			return plan, true
		}
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
		if list, ok := vol.termCache[cacheKey]; ok {
			seq := vol.termSeq[cacheKey]
			vol.termMu.Unlock()
			return vol.withRecentCandidates(list, seq, func(rec CompactRecord) bool {
				id, ok := vol.idForFRN(rec.FRN)
				return ok && vol.nameTrigramCandidateMatches(id, term)
			}), true
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
		if list, ok := vol.pathTermCache[cacheKey]; ok {
			seq := vol.pathTermSeq[cacheKey]
			vol.termMu.Unlock()
			return vol.withRecentCandidates(list, seq, func(rec CompactRecord) bool {
				id, ok := vol.idForFRN(rec.FRN)
				return ok && vol.index.compactPathContainsTerm(id, term)
			}), true
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
		if list, ok := vol.pathTermCache[cacheKey]; ok {
			seq := vol.pathTermSeq[cacheKey]
			vol.termMu.Unlock()
			return vol.withRecentCandidates(list, seq, func(rec CompactRecord) bool {
				id, ok := vol.idForFRN(rec.FRN)
				return ok && vol.index.compactPathContainsTerm(id, term)
			}), true
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

// broadPathScanCandidates handles broad path-substring queries (e.g.
// `-path "src"` or `-path "src main"`) where no term is selective enough to
// build a bounded posting. Instead of materializing a per-term path posting that
// exceeds the cache cap and is rebuilt every call, it scans all records in
// parallel and returns the ids whose full path contains every plain term. This
// mirrors how Everything scans its packed columns. The final ranking, limit, and
// full verification still happen in the shared search loop.
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
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		start := w * recordCount / workers
		end := (w + 1) * recordCount / workers
		wg.Add(1)
		go func(w, start, end int) {
			defer wg.Done()
			local := make([]int, 0, 256)
			for i := start; i < end; i++ {
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

func (vol *serviceVolumeIndex) mostSelectivePathTerm(pq parsedQuery) (string, bool) {
	best := ""
	bestSize := -1
	for _, term := range pathPlanProbeTerms(pq.Terms) {
		size := len(vol.pathPlanTermPosting(term))
		if bestSize < 0 || size < bestSize {
			best, bestSize = term, size
		}
		if bestSize <= serviceCachedPostingMaxIDs {
			break
		}
	}
	if bestSize < 0 {
		return "", false
	}
	if bestSize > serviceCachedPostingMaxIDs {
		return "", false
	}
	return best, true
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
		if len(plan.sources[i].ids) == len(plan.sources[j].ids) {
			return plan.sources[i].name < plan.sources[j].name
		}
		return len(plan.sources[i].ids) < len(plan.sources[j].ids)
	})
	out := append([]int(nil), plan.sources[0].ids...)
	for _, source := range plan.sources[1:] {
		out = intersectSortedInts(out, source.ids)
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
	if plan.vol.pathCache == nil {
		plan.vol.pathCache = make(map[int]string)
	}
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		if id < 0 || id >= plan.vol.index.compactRecordCount() {
			continue
		}
		path := plan.vol.index.reconstructCompactPathCached(id, plan.vol.pathCache)
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
	union := make([]int, 0, 64)
	for _, alt := range group {
		ids, ok := vol.altPosting(alt, matchPath)
		if !ok {
			return nil, false
		}
		union = append(union, ids...)
	}
	sort.Ints(union)
	return uniqueSortedInts(union), true
}

// altPosting returns a posting for a single OR alternative if it is a lone
// ext:, simple glob extension, or plain term. Returns ok=false otherwise.
func (vol *serviceVolumeIndex) altPosting(alt parsedQuery, matchPath bool) ([]int, bool) {
	switch {
	case len(alt.Exts) == 1 && alt.isOnly("ext"):
		return vol.extPosting(alt.Exts[0]), true
	case len(alt.Globs) == 1 && alt.isOnly("glob"):
		if exts, ok := simpleGlobExts(alt.Globs); ok && len(exts) == 1 {
			return vol.extPosting(exts[0]), true
		}
		return nil, false
	case len(alt.Terms) == 1 && alt.isOnly("term"):
		if matchPath {
			return vol.pathTermPosting(alt.Terms[0]), true
		}
		return vol.nameTermPosting(alt.Terms[0]), true
	default:
		return nil, false
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
	smallest := len(sources[0].ids)
	for _, source := range sources[1:] {
		if len(source.ids) < smallest {
			smallest = len(source.ids)
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
