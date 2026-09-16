package main

import (
	"bytes"
	"container/heap"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

func queryHasNonASCIIPlainTerm(pq parsedQuery) bool {
	for _, term := range pq.Terms {
		for _, r := range term {
			if r > 127 {
				return true
			}
		}
	}
	for _, group := range pq.OrGroups {
		for _, alt := range group {
			if queryHasNonASCIIPlainTerm(alt) {
				return true
			}
		}
	}
	for _, neg := range pq.NotGroups {
		if queryHasNonASCIIPlainTerm(neg) {
			return true
		}
	}
	return false
}

func queryPathTermPrecheckSafe(pq parsedQuery) bool {
	return pq.MatchPath && len(pq.Terms) > 0 && !queryHasNonASCIIPlainTerm(pq)
}

func (vol *serviceVolumeIndex) componentRootTopCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CountOnly || pq.Limit <= 0 ||
		pq.CaseSensitive || pq.Under != "" || pq.Type != "" || len(pq.Exts) > 0 ||
		len(pq.Globs) > 0 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 || pq.HasModAfter || pq.Exists ||
		pq.CWDBias != "" || pq.RootBias != "" || countNonVolumeTerms(pq.Terms) != 1 {
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
	if term == "" || strings.ContainsAny(term, `\/*?[]:.-`) {
		return nil, false
	}
	var roots []int
	if it, _, ok := vol.componentPostingBlockIterator(term); ok {
		ordinal := 0
		for it.next < it.end {
			block, _, ok := it.nextBlock()
			if !ok {
				return nil, false
			}
			for _, id32 := range block {
				if ordinal&1023 == 0 && queryCanceled(pq) {
					return nil, false
				}
				ordinal++
				rootID := int(id32)
				if vol.estimatedDescendantOrSelfCount(rootID) >= 100_000 {
					roots = append(roots, rootID)
					if len(roots) >= pq.Limit*4 {
						break
					}
				}
			}
			if len(roots) >= pq.Limit*4 {
				break
			}
		}
	} else {
		for i, id32 := range vol.componentPosting32(term) {
			if i&1023 == 0 && queryCanceled(pq) {
				return nil, false
			}
			rootID := int(id32)
			if vol.estimatedDescendantOrSelfCount(rootID) >= 100_000 {
				roots = append(roots, rootID)
				if len(roots) >= pq.Limit*4 {
					break
				}
			}
		}
	}
	if len(roots) == 0 && vol.queryIndex == nil {
		for _, rootID := range vol.pathComponentRootIDs(term) {
			if vol.estimatedDescendantOrSelfCount(rootID) >= 100_000 {
				roots = append(roots, rootID)
			}
		}
	}
	if len(roots) == 0 {
		return nil, false
	}
	if serviceLowMemoryMode() && (len(vol.subtreeStart) == 0 || len(vol.subtreeEnd) == 0 || len(vol.subtreeOrder) == 0) {
		return topCandidateIDsByRank(roots, pq.Limit, vol.index, vol.rankForQuery(pq)), true
	}
	recordCount := vol.index.compactRecordCount()
	out := make([]int, 0, pq.Limit)
	seen := make(map[int]struct{}, pq.Limit)
	add := func(id int) bool {
		if id < 0 || id >= recordCount {
			return false
		}
		if _, ok := seen[id]; ok {
			return false
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			return false
		}
		seen[id] = struct{}{}
		out = append(out, id)
		return len(out) >= pq.Limit
	}
	for rootPos, rootID := range roots {
		if rootPos&127 == 0 && queryCanceled(pq) {
			return nil, false
		}
		if add(rootID) {
			return out, true
		}
		if rootID < 0 || rootID >= len(vol.subtreeStart) || rootID >= len(vol.subtreeEnd) {
			continue
		}
		start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start == ^uint32(0) || start >= end || int(end) > len(vol.subtreeOrder) {
			continue
		}
		for pos := start; pos < end; pos++ {
			if pos&4095 == 0 && queryCanceled(pq) {
				return nil, false
			}
			if add(int(vol.subtreeOrder[pos])) {
				return out, true
			}
		}
	}
	return out, len(out) > 0
}

func (vol *serviceVolumeIndex) componentDirectTopCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || vol.queryIndex == nil ||
		!serviceLowMemoryMode() || !pq.MatchPath || pq.CountOnly || pq.Limit <= 0 || pq.CaseSensitive ||
		pq.Under != "" || pq.Type != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || len(pq.SizeFilters) > 0 ||
		len(pq.DateFilters) > 0 || len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		pq.HasModAfter || pq.Exists || pq.CWDBias != "" || pq.RootBias != "" ||
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
	if len(term) < 3 || strings.ContainsAny(term, `\/*?[]:`) {
		return nil, false
	}
	candidates := vol.componentPosting32(term)
	if len(candidates) == 0 {
		if len(vol.extPosting32(term)) > 0 {
			return nil, false
		}
		if len(vol.queryIndex.pathGrams) == 0 {
			return nil, false
		}
		grams := uniqueTrigramKeys(term)
		if len(grams) == 0 {
			return nil, false
		}
		for _, gram := range grams {
			list := vol.queryIndex.pathGrams[trigramStringFromKey(gram)]
			if len(list) == 0 {
				return nil, false
			}
			if candidates == nil || len(list) < len(candidates) {
				candidates = list
			}
		}
		for _, gram := range grams {
			list := vol.queryIndex.pathGrams[trigramStringFromKey(gram)]
			if len(list) == 0 || sameUint32Slice(list, candidates) {
				continue
			}
			candidates = intersectSortedUint32s(candidates, list)
			if len(candidates) == 0 {
				return nil, false
			}
		}
	}
	recordCount := vol.index.compactRecordCount()
	out := make([]int, 0, pq.Limit)
	seen := make(map[int]struct{}, pq.Limit)
	add := func(id int) bool {
		if id < 0 || id >= recordCount {
			return false
		}
		if _, ok := seen[id]; ok {
			return false
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			return false
		}
		seen[id] = struct{}{}
		out = append(out, id)
		return len(out) >= pq.Limit
	}
	for _, id32 := range candidates {
		id := int(id32)
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 || !containsFoldASCII(vol.index.compactNameAt(id), term) {
			continue
		}
		if add(id) {
			return out, true
		}
		if id >= 0 && id < len(vol.subtreeStart) && id < len(vol.subtreeEnd) && len(vol.subtreeOrder) > 0 {
			start, end := vol.subtreeStart[id], vol.subtreeEnd[id]
			if start != ^uint32(0) && start <= end && int(end) <= len(vol.subtreeOrder) {
				for pos := start; pos < end; pos++ {
					if add(int(vol.subtreeOrder[pos])) {
						return out, true
					}
				}
			}
		}
	}
	if len(out) > 0 && len(out) < pq.Limit && len(vol.subtreeOrder) == 0 {
		roots := append([]int(nil), out...)
		scanned := vol.scanOrderedLimited(pq, pq.Limit-len(out), func(id int) bool {
			if _, ok := seen[id]; ok {
				return false
			}
			return vol.isDescendantOrSelfAnyFast(id, roots) && vol.index.compactPathContainsTerm(id, term)
		})
		for _, id := range scanned {
			if add(id) {
				return out, true
			}
		}
	}
	return out, len(out) > 0
}

func (vol *serviceVolumeIndex) componentMultiTermTopCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || vol.queryIndex == nil || !vol.hasDescendantIndex() ||
		!serviceLowMemoryMode() || !pq.MatchPath || pq.CountOnly || pq.Limit <= 0 || pq.CaseSensitive ||
		pq.Under != "" || pq.Type != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || len(pq.SizeFilters) > 0 ||
		len(pq.DateFilters) > 0 || len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		pq.HasModAfter || pq.Exists || pq.CWDBias != "" || pq.RootBias != "" ||
		countNonVolumeTerms(pq.Terms) < 2 {
		return nil, false
	}
	var best []int
	bestEstimate := int(^uint(0) >> 1)
	for _, term := range pq.Terms {
		if len(term) < 3 || isVolumeQueryTerm(term) || strings.ContainsAny(term, `\/*?[]:.`) ||
			vol.pathTermIsUsableExtensionCandidate(term) {
			continue
		}
		nameMatches, roots, complete := vol.pathDirectoryTermSource(term)
		if complete && len(nameMatches) == 0 {
			return []int{}, true
		}
		if len(roots) == 0 {
			for _, id32 := range vol.componentPosting32(term) {
				roots = append(roots, int(id32))
			}
		}
		if len(roots) == 0 {
			continue
		}
		estimate := 0
		for _, id := range roots {
			if id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			if len(vol.subtreeOrder) == 0 && (len(vol.childOffsets) > 0 || vol.children != nil) {
				estimate += len(vol.underDescendantsLimited(id, serviceComponentMultiTermScanMaxIDs+1))
			} else {
				estimate += vol.estimatedDescendantOrSelfCount(id)
			}
			if estimate > bestEstimate {
				break
			}
		}
		if estimate > 0 && estimate < bestEstimate {
			best = roots
			bestEstimate = estimate
		}
	}
	if len(best) == 0 {
		return nil, false
	}
	if len(vol.subtreeOrder) == 0 {
		var bestTerm string
		for _, term := range pq.Terms {
			if len(term) < 3 || isVolumeQueryTerm(term) || strings.ContainsAny(term, `\/*?[]:.`) ||
				vol.pathTermIsUsableExtensionCandidate(term) {
				continue
			}
			_, roots, _ := vol.pathDirectoryTermSource(term)
			if len(roots) == 0 {
				for _, id32 := range vol.componentPosting32(term) {
					roots = append(roots, int(id32))
				}
			}
			if len(roots) == len(best) {
				same := true
				for i := range roots {
					if roots[i] != best[i] {
						same = false
						break
					}
				}
				if same {
					bestTerm = term
					break
				}
			}
		}
		if bestTerm != "" && bestEstimate <= serviceComponentMultiTermScanMaxIDs {
			seenCandidates := make(map[int]struct{}, len(best))
			candidates := make([]int, 0, min(bestEstimate, serviceComponentMultiTermScanMaxIDs))
			for _, id := range best {
				for _, candidate := range vol.underDescendantsLimited(id, serviceComponentMultiTermScanMaxIDs+1) {
					if _, ok := seenCandidates[candidate]; ok {
						continue
					}
					seenCandidates[candidate] = struct{}{}
					candidates = append(candidates, candidate)
				}
			}
			out := make([]int, 0, min(len(candidates), pq.Limit))
			for _, id := range candidates {
				if id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				rec := vol.index.compactRecord(id)
				if rec.Deleted || !vol.index.compactPathContainsAll(id, pq.Terms) {
					continue
				}
				out = append(out, id)
			}
			if len(out) > 0 {
				sortCandidateIDs(out, pq, vol.index, vol.rankForQuery(pq))
				return out, true
			}
		}
		roots := make([]int, 0, min(len(best), 16))
		for _, id := range best {
			if id >= 0 && id < vol.index.compactRecordCount() {
				roots = append(roots, id)
				if len(roots) >= 16 {
					break
				}
			}
		}
		if len(roots) == 0 {
			return nil, false
		}
		out := vol.scanOrderedLimited(pq, pq.Limit, func(id int) bool {
			if !vol.isDescendantOrSelfAnyFast(id, roots) {
				return false
			}
			rec := vol.index.compactRecord(id)
			return !rec.Deleted && vol.index.compactPathContainsAll(id, pq.Terms)
		})
		if len(out) >= pq.Limit {
			return out, true
		}
		return nil, false
	}
	recordCount := vol.index.compactRecordCount()
	out := make([]int, 0, pq.Limit)
	seen := make(map[int]struct{}, pq.Limit)
	scanned := 0
	add := func(id int) bool {
		if id < 0 || id >= recordCount {
			return false
		}
		scanned++
		if _, ok := seen[id]; ok {
			return false
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !vol.index.compactPathContainsAll(id, pq.Terms) {
			return false
		}
		seen[id] = struct{}{}
		out = append(out, id)
		return len(out) >= pq.Limit
	}
	for rootIndex, id := range best {
		if rootIndex&127 == 0 && queryCanceled(pq) {
			return nil, false
		}
		if add(id) {
			return out, true
		}
		if id < 0 || id >= len(vol.subtreeStart) || id >= len(vol.subtreeEnd) || len(vol.subtreeOrder) == 0 {
			continue
		}
		start, end := vol.subtreeStart[id], vol.subtreeEnd[id]
		if start == ^uint32(0) || start > end || int(end) > len(vol.subtreeOrder) {
			continue
		}
		for pos := start; pos < end; pos++ {
			if bestEstimate > serviceComponentTrigramExpansionMaxIDs && scanned >= serviceComponentMultiTermScanMaxIDs {
				if len(out) >= pq.Limit {
					return out, true
				}
				return nil, false
			}
			if pos&4095 == 0 && queryCanceled(pq) {
				return nil, false
			}
			if add(int(vol.subtreeOrder[pos])) {
				return out, true
			}
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func (vol *serviceVolumeIndex) selectiveNamePathTermCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CountOnly || pq.Limit <= 0 || pq.CaseSensitive ||
		pq.Under != "" || pq.Type != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || len(pq.SizeFilters) > 0 ||
		len(pq.DateFilters) > 0 || len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		pq.HasModAfter || pq.Exists || pq.CWDBias != "" || pq.RootBias != "" ||
		countNonVolumeTerms(pq.Terms) < 2 {
		return nil, false
	}
	bestTerm := ""
	bestIDs := []int(nil)
	for _, term := range pq.Terms {
		if len(term) < 4 || isVolumeQueryTerm(term) || strings.ContainsAny(term, `\/*?[]:`) || !filenameLikePathTerm(term) {
			continue
		}
		ids, ok := vol.nameTrigramNameTermPostingLimited(term, servicePathNameTrigramCandidateMaxIDs)
		if !ok || len(ids) > serviceComponentTrigramExpansionMaxIDs {
			continue
		}
		if vol.hasDirectoryCandidate(ids) {
			continue
		}
		if bestTerm == "" || len(ids) < len(bestIDs) {
			bestTerm = term
			bestIDs = ids
		}
	}
	if bestTerm == "" {
		return nil, false
	}
	out := make([]int, 0, min(pq.Limit, len(bestIDs)))
	for _, id := range bestIDs {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !vol.index.compactPathContainsAll(id, pq.Terms) {
			continue
		}
		out = append(out, id)
		if len(out) >= pq.Limit {
			break
		}
	}
	if len(out) == 0 {
		return []int{}, true
	}
	return topCandidateIDsByRank(out, pq.Limit, vol.index, vol.rankForQuery(pq)), true
}

func filenameLikePathTerm(term string) bool {
	return strings.ContainsAny(term, "._-")
}

func asciiOnlyString(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func (vol *serviceVolumeIndex) multiTermEmptyPathCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CountOnly || pq.CaseSensitive ||
		pq.Under != "" || pq.Type != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || len(pq.SizeFilters) > 0 ||
		len(pq.DateFilters) > 0 || len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		pq.HasModAfter || pq.Exists || pq.CWDBias != "" || pq.RootBias != "" ||
		countNonVolumeTerms(pq.Terms) < 2 {
		return nil, false
	}
	for _, term := range pq.Terms {
		if len(term) < 3 || isVolumeQueryTerm(term) || strings.ContainsAny(term, `\/*?[]:`) {
			continue
		}
		if !asciiOnlyString(term) {
			return nil, false
		}
		if len(term) >= 6 && vol.queryIndex != nil && len(vol.componentPosting32(term)) == 0 {
			if trigrams := vol.nameTrigramIndex(); trigrams != nil {
				_, ok, missing := trigrams.selectiveIntersectCandidateIDs(term, servicePathNameTrigramCandidateMaxIDs)
				if ok && missing {
					return []int{}, true
				}
			}
		}
	}
	return nil, false
}

func (vol *serviceVolumeIndex) nameTrigramCandidates(pq parsedQuery) ([]int, bool) {
	if pq.MatchPath {
		return vol.componentTrigramCandidates(pq)
	}
	return vol.filenameTrigramCandidates(pq)
}

func (vol *serviceVolumeIndex) filenameTrigramCandidates(pq parsedQuery) ([]int, bool) {
	trigrams := vol.nameTrigramIndex()
	if vol == nil || vol.index == nil || (trigrams == nil && vol.index.Derived.SelfNameTrigrams == nil) || pq.CaseSensitive ||
		pq.MatchPath || pq.Under != "" || pq.Type != "" ||
		len(pq.Dirs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 || pq.HasModAfter || pq.Exists ||
		pq.CWDBias != "" || pq.RootBias != "" {
		return nil, false
	}
	// Glob literals can supply the driving terms when the query has no plain
	// terms of its own (glob:*acme*), or join them.  Without this a
	// glob-only query is all-orphan and drops to the bounded scan.
	if len(pq.Terms) == 0 && len(pq.Globs) > 0 && !pqGramsCouldDriveGlobs(pq) {
		return nil, false
	}
	return vol.filenameNgramCandidates(pq, trigrams, serviceNameTrigramCandidateMaxIDs)
}

// pqGramsCouldDriveGlobs reports whether the query's globs can be driven by
// gram candidate generation: every glob must yield a literal run of at least
// three runes so a substring of it can select candidates, and the globs must
// be verifiable on the record name in the final fold.  A leading wildcard
// glob like "glob:*acme*" is a substring match, which the trigram lane
// answers exactly; the blanket "no globs in the fast lane" rule is what made
// it fall to a 22s bounded scan.
func pqGramsCouldDriveGlobs(pq parsedQuery) bool {
	literals := globLiteralTerms(pq.Globs, pq.CaseSensitive)
	if len(literals) == 0 {
		return false
	}
	for _, glob := range pq.Globs {
		if strings.ContainsAny(glob, `[]`) || strings.Contains(glob, "?") {
			// Character classes and single-char wildcards cannot be reduced
			// to a plain substring literal.
			return false
		}
	}
	return true
}

func (vol *serviceVolumeIndex) filenameNgramCandidates(pq parsedQuery, trigrams *compressedTrigramIndex, maxIDs int) ([]int, bool) {
	if vol == nil {
		return nil, false
	}
	if trigrams == nil {
		trigrams = vol.index.Derived.SelfNameTrigrams
	}
	if trigrams == nil {
		return nil, false
	}
	// Glob literals ride into the driving term set when the glob is a plain
	// substring pattern (glob:*acme*).  pq.Globs stays intact for the
	// final fold, so this only widens candidate selection, never weakens the
	// verification.  Without it, any query carrying a glob was forced to the
	// bounded scan even though a literal run inside it is exactly the kind of
	// substring the gram lane answers.
	if len(pq.Globs) > 0 && pqGramsCouldDriveGlobs(pq) {
		combined := append([]string(nil), pq.Terms...)
		seen := make(map[string]struct{}, len(combined))
		for _, t := range combined {
			seen[t] = struct{}{}
		}
		for _, gl := range globLiteralTerms(pq.Globs, pq.CaseSensitive) {
			if _, ok := seen[gl]; !ok {
				combined = append(combined, gl)
				seen[gl] = struct{}{}
			}
		}
		pq.Terms = combined
	}
	// Multi-term queries: the selective single-best-term lane verifies only
	// the best term, admitting false positives for the other terms.  When the
	// companion PNGC section is present, answer exactly by intersecting
	// postings across every term.  This is also the only exact lane for
	// common-gram queries whose grams are omitted from PNGR.
	if len(pq.Terms) >= 2 && vol.index != nil && vol.index.Derived.SelfNameTrigrams != nil &&
		vol.index.Derived.SelfNameTrigrams.mappedGrams != nil {
		if candidates, ok := vol.completeMultiTermNameGramCandidates(pq.Terms, maxIDs, pq); ok {
			return candidates, true
		}
	}
	bestTerm := ""
	bestCount := maxIDs + 1
	exactEmpty := false
	exactEmptyTerm := ""
	for _, term := range pq.Terms {
		if len(term) < max(3, trigrams.gramSize) {
			continue
		}
		if !asciiOnlyString(term) {
			return nil, false
		}
		termBest := maxIDs + 1
		termMissing := false
		termExactEmpty := false
		for _, gram := range trigrams.termGramKeys(strings.ToLower(term)) {
			_, count, stored, state, isExactEmpty := vol.nameGramPosting(gram)
			if isExactEmpty {
				termExactEmpty = true
				break
			}
			if !stored {
				termMissing = true
				if state == "omitted-common" || state == "missing-section" {
					break
				}
				continue
			}
			if count < termBest {
				termBest = count
			}
		}
		if termExactEmpty {
			exactEmpty = true
			exactEmptyTerm = term
			continue
		}
		if termMissing || termBest > maxIDs {
			continue
		}
		if termBest < bestCount {
			bestTerm = term
			bestCount = termBest
		}
	}
	if exactEmpty {
		// A complete PNGR count table proves that at least one required gram
		// has no base records.  Do not turn that fact into a recent-match or
		// bounded-scan fallback: overlays are merged by the caller, while the
		// persisted base candidate set is exactly empty.
		recent := vol.nameTrigramRecentMatches(exactEmptyTerm)
		pq.Trace.setSource("exact-empty", len(recent))
		return recent, true
	}
	if bestTerm == "" {
		for _, term := range pq.Terms {
			state := ""
			for _, gram := range trigrams.termGramKeys(strings.ToLower(term)) {
				_, _, _, gramState, _ := vol.nameGramPosting(gram)
				if gramState == "omitted-common" || gramState == "missing-section" {
					state = gramState
					break
				}
			}
			if state != "" {
				pq.Trace.setDecline("name-trigram:" + state)
				break
			}
		}
		// Selective lane declined (every gram over cap or omitted-common).
		if rescued, ok := vol.rescueWithCompleteGramLane(pq, maxIDs); ok {
			return rescued, true
		}
		return nil, false
	}
	candidates, ok := vol.nameNgramNameTermPosting(bestTerm, trigrams, maxIDs)
	if !ok {
		pq.Trace.setDecline("name-trigram:" + trigrams.lookupState(bestTerm))
		if rescued, rescuedOK := vol.rescueWithCompleteGramLane(pq, maxIDs); rescuedOK {
			return rescued, true
		}
		return nil, false
	}
	if len(candidates) > maxIDs {
		if rescued, rescuedOK := vol.rescueWithCompleteGramLane(pq, maxIDs); rescuedOK {
			return rescued, true
		}
		return nil, false
	}
	return candidates, true
}

// rescueWithCompleteGramLane retries a declined selective-trigram query
// through the complete PNGC intersection lane, which answers broad terms
// exactly instead of falling to the bounded scan.
func (vol *serviceVolumeIndex) rescueWithCompleteGramLane(pq parsedQuery, maxIDs int) ([]int, bool) {
	if vol == nil || vol.index == nil || vol.index.Derived.SelfNameTrigrams == nil ||
		vol.index.Derived.SelfNameTrigrams.mappedGrams == nil {
		return nil, false
	}
	out, ok := vol.completeMultiTermNameGramCandidates(pq.Terms, maxIDs, pq)
	return out, ok
}

func (vol *serviceVolumeIndex) componentTrigramCandidates(pq parsedQuery) ([]int, bool) {
	trigrams := vol.nameTrigramIndex()
	if vol == nil || vol.index == nil {
		pq.Trace.setDecline("component-trigram:no-volume")
		return nil, false
	}
	if trigrams == nil {
		pq.Trace.setDecline("component-trigram:not-ready")
		return nil, false
	}
	if pq.CaseSensitive || !pq.MatchPath || len(pq.Terms) == 0 || pq.Under != "" || pq.Type != "" ||
		len(pq.Exts) > 0 || len(pq.Globs) > 0 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 || pq.HasModAfter || pq.Exists ||
		pq.CWDBias != "" || pq.RootBias != "" {
		pq.Trace.setDecline("component-trigram:unsupported-query")
		return nil, false
	}
	bestTerm := ""
	bestCount := serviceComponentTrigramCandidateMaxIDs + 1
	missingTerm := ""
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) {
			continue
		}
		if len(term) < 3 {
			continue
		}
		if !asciiOnlyString(term) {
			pq.Trace.setDecline("component-trigram:non-ascii-term")
			return nil, false
		}
		count, ok := trigrams.postingCount(term)
		if !ok {
			pq.Trace.setDecline("component-trigram:no-posting-count")
			continue
		}
		if count == 0 {
			missingTerm = term
			continue
		}
		if count > serviceComponentTrigramCandidateMaxIDs {
			if len(term) < 6 {
				continue
			}
			count = serviceComponentTrigramCandidateMaxIDs
		}
		if count < bestCount {
			bestTerm = term
			bestCount = count
		}
	}
	if bestTerm == "" {
		if missingTerm != "" {
			candidates, ok := vol.nameTrigramPathTermPosting(missingTerm)
			if !ok {
				pq.Trace.setDecline("component-trigram:" + trigrams.lookupState(missingTerm))
				return nil, false
			}
			if len(candidates) > serviceComponentTrigramExpansionMaxIDs {
				pq.Trace.setDecline("component-trigram:missing-term-expanded-too-large")
				return nil, false
			}
			return candidates, true
		}
		for _, term := range pq.Terms {
			if state := trigrams.lookupState(term); state == "omitted-common" || state == "missing-section" {
				pq.Trace.setDecline("component-trigram:" + state)
				break
			}
		}
		pq.Trace.setDecline("component-trigram:no-selective-term")
		return nil, false
	}
	candidates, ok := vol.nameTrigramPathTermPosting(bestTerm)
	if !ok {
		pq.Trace.setDecline("component-trigram:" + trigrams.lookupState(bestTerm))
		return nil, false
	}
	if len(candidates) > serviceComponentTrigramExpansionMaxIDs {
		pq.Trace.setDecline("component-trigram:expanded-too-large")
		return nil, false
	}
	return candidates, true
}

func (vol *serviceVolumeIndex) nameTrigramPathNameTopCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || pq.CaseSensitive ||
		!pq.MatchPath || pq.CountOnly || pq.Limit <= 0 || len(pq.Terms) == 0 ||
		pq.Under != "" || pq.Type != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || len(pq.OrGroups) > 0 ||
		len(pq.NotGroups) > 0 || pq.HasModAfter || pq.Exists ||
		pq.CWDBias != "" || pq.RootBias != "" || countNonVolumeTerms(pq.Terms) != 1 {
		pq.Trace.replaceDecline("path-name-trigram-top:unsupported-query")
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
	if len(term) < 6 || strings.ContainsAny(term, `\/*?[]:`) {
		pq.Trace.replaceDecline("path-name-trigram-top:bad-term")
		return nil, false
	}
	nameMatches, ok := vol.nameTrigramNameTermTopPosting(term, servicePathNameTrigramCandidateMaxIDs, servicePathNameTrigramCandidateMaxIDs)
	if !ok || len(nameMatches) == 0 {
		pq.Trace.replaceDecline("path-name-trigram-top:" + vol.nameTrigramIndex().lookupState(term))
		return nil, false
	}
	direct := make([]int, 0, len(nameMatches))
	seen := make(map[int]struct{}, len(nameMatches)+pq.Limit)
	sawUnexpandedDir := false
	for _, id := range nameMatches {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			continue
		}
		if _, exists := seen[id]; !exists {
			seen[id] = struct{}{}
			direct = append(direct, id)
		}
		if rec.Mode&uint32(os.ModeDir) != 0 && vol.estimatedDescendantOrSelfCount(id) > serviceComponentTrigramExpansionMaxIDs {
			direct = vol.appendTopSubtreeCandidatesByRank(direct, seen, id, pq.Limit*4)
		} else if rec.Mode&uint32(os.ModeDir) != 0 {
			sawUnexpandedDir = true
		}
	}
	if sawUnexpandedDir {
		pq.Trace.replaceDecline("path-name-trigram-top:directory-needs-expansion")
		return nil, false
	}
	if len(direct) < pq.Limit {
		pq.Trace.replaceDecline("path-name-trigram-top:too-few-direct")
		return nil, false
	}
	return topCandidateIDsByRank(direct, pq.Limit, vol.index, vol.rankForQuery(pq)), true
}

func (vol *serviceVolumeIndex) nameTrigramNameTermTopPosting(term string, maxIDs, limit int) ([]int, bool) {
	trigrams := vol.nameTrigramIndex()
	if vol == nil || trigrams == nil || limit <= 0 || !asciiOnlyString(term) {
		return nil, false
	}
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
	out := make([]int, 0, min(limit, len(ids)))
	seen := make(map[int]struct{}, min(limit, len(ids)))
	for _, id := range ids {
		if _, exists := seen[id]; exists {
			continue
		}
		if vol.nameTrigramCandidateMatches(id, term) {
			seen[id] = struct{}{}
			out = append(out, id)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, true
}

func (vol *serviceVolumeIndex) appendTopSubtreeCandidatesByRank(out []int, seen map[int]struct{}, rootID int, limit int) []int {
	if vol == nil || vol.index == nil || limit <= 0 || rootID < 0 ||
		rootID >= len(vol.subtreeStart) || rootID >= len(vol.subtreeEnd) ||
		len(vol.subtreeOrder) == 0 {
		return out
	}
	start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
	if start == ^uint32(0) || start >= end {
		return out
	}
	recordCount := vol.index.compactRecordCount()
	orderLen := recordCount
	useResidentOrder := vol.queryIndex != nil && len(vol.queryIndex.nameOrder) > 0
	if useResidentOrder {
		orderLen = len(vol.queryIndex.nameOrder)
	} else {
		orderLen = compactOrderLen(vol.index.CompactNameOrder, recordCount)
	}
	for pos := 0; pos < orderLen && len(out) < limit; pos++ {
		id := pos
		if useResidentOrder {
			id = int(vol.queryIndex.nameOrder[pos])
		} else {
			id = compactOrderAt(vol.index.CompactNameOrder, pos)
		}
		if id < 0 || id >= recordCount || id >= len(vol.subtreeStart) {
			continue
		}
		treePos := vol.subtreeStart[id]
		if treePos < start || treePos >= end {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func (vol *serviceVolumeIndex) nameTrigramNameTermPosting(term string) ([]int, bool) {
	return vol.nameTrigramNameTermPostingLimited(term, serviceNameTrigramCandidateMaxIDs)
}

func (vol *serviceVolumeIndex) nameTrigramNameTermPostingLimited(term string, maxIDs int) ([]int, bool) {
	trigrams := vol.nameTrigramIndex()
	return vol.nameNgramNameTermPosting(term, trigrams, maxIDs)
}

func (vol *serviceVolumeIndex) nameNgramNameTermPosting(term string, trigrams *compressedTrigramIndex, maxIDs int) ([]int, bool) {
	if vol == nil || !asciiOnlyString(term) {
		return nil, false
	}
	if trigrams == nil {
		trigrams = vol.nameTrigramIndex()
		if trigrams == nil && vol.index != nil {
			trigrams = vol.index.Derived.SelfNameTrigrams
		}
	}
	if trigrams == nil {
		return nil, false
	}
	extra := vol.index != nil && vol.index.Derived.SelfNameTrigrams != nil && vol.index.Derived.SelfNameTrigrams.mappedGrams != nil
	if extra {
		its, counts, exactZero, complete := completeSelfNameGramIterators(vol.index, term)
		if !complete {
			return nil, false
		}
		if exactZero {
			return vol.nameTrigramRecentMatches(term), true
		}
		if len(its) == 0 || (maxIDs > 0 && counts[0] > maxIDs) {
			return nil, false
		}
		ids := materializePostingBlockIterator(its[0], counts[0])
		for i := 1; i < len(its) && len(ids) > 0; i++ {
			ids = intersectSortedUint32sWithPostingIterator(ids, its[i])
		}
		if maxIDs > 0 && len(ids) > maxIDs {
			return nil, false
		}
		out := uniqueSortedInts(vol.verifyNameTrigramCandidateIDs(uint32sToInts(ids), term))
		return vol.withNameTrigramRecentCandidates(out, term), true
	}
	cacheKey := fmt.Sprintf("\x00ngram%dname:%s", trigrams.gramSize, term)
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
	out := vol.verifyNameTrigramCandidateIDs(ids, term)
	out = uniqueSortedInts(out)
	vol.cacheNamePosting(cacheKey, out)
	return vol.withNameTrigramRecentCandidates(out, term), true
}

func (vol *serviceVolumeIndex) nameTrigramRecentMatches(term string) []int {
	if vol == nil || len(vol.nameTrigramRecent) == 0 {
		return nil
	}
	out := make([]int, 0, min(len(vol.nameTrigramRecent), 64))
	for id := range vol.nameTrigramRecent {
		if vol.nameTrigramCandidateMatches(id, term) {
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out
}

func (vol *serviceVolumeIndex) withNameTrigramRecentCandidates(base []int, term string) []int {
	if vol == nil || len(vol.nameTrigramRecent) == 0 {
		return base
	}
	out := append([]int(nil), base...)
	seen := make(map[int]struct{}, len(out))
	for _, id := range out {
		seen[id] = struct{}{}
	}
	for id := range vol.nameTrigramRecent {
		if _, ok := seen[id]; ok {
			continue
		}
		if vol.nameTrigramCandidateMatches(id, term) {
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out
}

func (vol *serviceVolumeIndex) verifyNameTrigramCandidateIDs(ids []int, term string) []int {
	if len(ids) == 0 || vol == nil || vol.index == nil {
		return nil
	}
	if len(ids) < serviceTrigramParallelVerifyMinIDs {
		out := make([]int, 0, len(ids))
		for _, id := range ids {
			if vol.nameTrigramCandidateMatches(id, term) {
				out = append(out, id)
			}
		}
		return out
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, len(ids)/serviceTrigramParallelVerifyMinIDs))
	if workers <= 1 {
		out := make([]int, 0, len(ids))
		for _, id := range ids {
			if vol.nameTrigramCandidateMatches(id, term) {
				out = append(out, id)
			}
		}
		return out
	}
	parts := make([][]int, workers)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := worker * len(ids) / workers
		end := (worker + 1) * len(ids) / workers
		wg.Add(1)
		go func(worker, start, end int) {
			defer wg.Done()
			local := make([]int, 0, end-start)
			for _, id := range ids[start:end] {
				if vol.nameTrigramCandidateMatches(id, term) {
					local = append(local, id)
				}
			}
			parts[worker] = local
		}(worker, start, end)
	}
	wg.Wait()
	total := 0
	for _, part := range parts {
		total += len(part)
	}
	out := make([]int, 0, total)
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

// verifyMultiTermCandidateIDs runs the multi-term substring check over a
// candidate set in parallel, mirroring verifyNameTrigramCandidateIDs.
func (vol *serviceVolumeIndex) verifyMultiTermCandidateIDs(ids []int, matches func(int) bool) []int {
	if len(ids) == 0 || vol == nil || vol.index == nil {
		return nil
	}
	if len(ids) < serviceTrigramParallelVerifyMinIDs {
		out := make([]int, 0, len(ids))
		for _, id := range ids {
			if matches(id) {
				out = append(out, id)
			}
		}
		return uniqueSortedInts(out)
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, len(ids)/serviceTrigramParallelVerifyMinIDs))
	if workers <= 1 {
		out := make([]int, 0, len(ids))
		for _, id := range ids {
			if matches(id) {
				out = append(out, id)
			}
		}
		return uniqueSortedInts(out)
	}
	parts := make([][]int, workers)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := worker * len(ids) / workers
		end := (worker + 1) * len(ids) / workers
		wg.Add(1)
		go func(worker, start, end int) {
			defer wg.Done()
			local := make([]int, 0, end-start)
			for _, id := range ids[start:end] {
				if matches(id) {
					local = append(local, id)
				}
			}
			parts[worker] = local
		}(worker, start, end)
	}
	wg.Wait()
	total := 0
	for _, part := range parts {
		total += len(part)
	}
	out := make([]int, 0, total)
	for _, part := range parts {
		out = append(out, part...)
	}
	return uniqueSortedInts(out)
}

func (vol *serviceVolumeIndex) nameTrigramCandidateMatches(id int, term string) bool {
	if id < 0 || id >= vol.index.compactRecordCount() {
		return false
	}
	rec := vol.index.compactRecord(id)
	if rec.Deleted {
		return false
	}
	return containsFoldASCII(vol.index.compactNameAt(id), term)
}

func (vol *serviceVolumeIndex) nameTrigramPathTermPosting(term string) ([]int, bool) {
	cacheKey := "\x00trigrampath:" + term
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
	ids, ok := vol.nameTrigramNameTermPostingLimited(term, servicePathNameTrigramCandidateMaxIDs)
	if !ok {
		return nil, false
	}
	seen := make(map[int]struct{}, len(ids))
	out := make([]int, 0, len(ids))
	estimated := 0
	for _, id := range ids {
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
	for _, id := range ids {
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
	vol.cachePathPosting(cacheKey, out)
	return out, true
}

func (vol *serviceVolumeIndex) hasDescendantIndex() bool {
	return vol != nil && (len(vol.subtreeOrder) > 0 || len(vol.childOffsets) > 0 || vol.children != nil)
}

func (vol *serviceVolumeIndex) estimatedDescendantOrSelfCount(rootID int) int {
	if vol == nil || vol.index == nil || rootID < 0 || rootID >= vol.index.compactRecordCount() {
		return 0
	}
	if rootID < len(vol.subtreeStart) && rootID < len(vol.subtreeEnd) && len(vol.subtreeOrder) > 0 {
		start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start != ^uint32(0) && start <= end && int(end) <= len(vol.subtreeOrder) {
			return int(end - start)
		}
	}
	vol.termMu.Lock()
	if vol.underCache != nil {
		if cached, ok := vol.underCache[rootID]; ok {
			vol.termMu.Unlock()
			return len(cached.ids)
		}
	}
	vol.termMu.Unlock()
	return len(vol.underDescendantsLimited(rootID, serviceComponentTrigramExpansionMaxIDs+1))
}

func (vol *serviceVolumeIndex) limitedSingleTermCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || pq.CountOnly || pq.Limit <= 0 || pq.CaseSensitive ||
		pq.Under != "" || pq.Type != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.Regexps) > 0 || len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 || pq.HasModAfter || pq.Exists ||
		pq.CWDBias != "" || pq.RootBias != "" || pq.SortColumn != "" {
		return nil, false
	}
	if !pq.MatchPath && len(pq.Terms) == 1 && len(pq.Dirs) == 0 {
		return vol.scanNameTermLimited(pq, pq.Terms[0], pq.Limit), true
	}
	if len(pq.Terms) == 0 && len(pq.Dirs) == 1 {
		return vol.scanPathTermLimited(pq, pq.Dirs[0], pq.Limit), true
	}
	return nil, false
}

func (vol *serviceVolumeIndex) scanNameTermLimited(pq parsedQuery, term string, limit int) []int {
	if term == "" || limit <= 0 {
		return nil
	}
	return vol.scanOrderedLimited(pq, limit, func(i int) bool {
		return containsFoldASCII(vol.index.compactNameAt(i), term)
	})
}

func (vol *serviceVolumeIndex) scanPathTermLimited(pq parsedQuery, term string, limit int) []int {
	if term == "" || limit <= 0 {
		return nil
	}
	return vol.scanOrderedLimited(pq, limit, func(i int) bool {
		return vol.index.compactPathContainsTerm(i, term)
	})
}

func (vol *serviceVolumeIndex) scanPathTermPrefixLimited(pq parsedQuery, term string, limit int, maxScan int) []int {
	if term == "" || limit <= 0 || maxScan <= 0 {
		return nil
	}
	orderLen := vol.index.compactRecordCount()
	if vol.queryIndex != nil && len(vol.queryIndex.nameOrder) > 0 {
		orderLen = len(vol.queryIndex.nameOrder)
	} else {
		orderLen = compactOrderLen(vol.index.CompactNameOrder, vol.index.compactRecordCount())
	}
	end := min(orderLen, maxScan)
	return vol.scanOrderedLimitedRange(pq, 0, end, limit, func(i int) bool {
		return vol.index.compactPathContainsTerm(i, term)
	})
}

func (vol *serviceVolumeIndex) scanOrderedLimited(pq parsedQuery, limit int, match func(int) bool) []int {
	recordCount := vol.index.compactRecordCount()
	orderLen := recordCount
	useResidentOrder := vol.queryIndex != nil && len(vol.queryIndex.nameOrder) > 0
	if useResidentOrder {
		orderLen = len(vol.queryIndex.nameOrder)
	} else {
		orderLen = compactOrderLen(vol.index.CompactNameOrder, recordCount)
	}
	prefixEnd := min(orderLen, 4_096)
	out := vol.scanOrderedLimitedRange(pq, 0, prefixEnd, limit, match)
	if len(out) >= limit || prefixEnd >= orderLen {
		return out
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, orderLen/25_000))
	if workers <= 1 || orderLen < 50_000 {
		tail := vol.scanOrderedLimitedRange(pq, prefixEnd, orderLen, limit-len(out), match)
		return append(out, tail...)
	}
	parts := make([][]int, workers)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := prefixEnd + worker*(orderLen-prefixEnd)/workers
		end := prefixEnd + (worker+1)*(orderLen-prefixEnd)/workers
		wg.Add(1)
		go func(worker, start, end int) {
			defer wg.Done()
			parts[worker] = vol.scanOrderedLimitedRange(pq, start, end, limit-len(out), match)
		}(worker, start, end)
	}
	wg.Wait()
	for _, part := range parts {
		for _, id := range part {
			out = append(out, id)
			if len(out) >= limit {
				return out
			}
		}
	}
	return out
}

func (vol *serviceVolumeIndex) scanOrderedLimitedRange(pq parsedQuery, start, end, limit int, match func(int) bool) []int {
	out := make([]int, 0, limit)
	recordCount := vol.index.compactRecordCount()
	useResidentOrder := vol.queryIndex != nil && len(vol.queryIndex.nameOrder) > 0
	for pos := start; pos < end; pos++ {
		if pos&4095 == 0 && queryCanceled(pq) {
			return out
		}
		i := pos
		if useResidentOrder {
			i = int(vol.queryIndex.nameOrder[pos])
		} else {
			i = compactOrderAt(vol.index.CompactNameOrder, pos)
		}
		if i < 0 || i >= recordCount {
			continue
		}
		rec := vol.index.compactRecord(i)
		if rec.Deleted {
			continue
		}
		if match(i) {
			out = append(out, i)
			if len(out) >= limit {
				return out
			}
		}
	}
	return out
}

func (vol *serviceVolumeIndex) cachedMultiNameTermCandidates(terms []string) ([]int, bool) {
	vol.termMu.Lock()
	if vol.termCache == nil {
		vol.termMu.Unlock()
		return nil, false
	}
	lists := make([][]int, 0, len(terms))
	seqs := make([]uint64, 0, len(terms))
	for _, term := range terms {
		entry, ok := vol.termCache[term]
		if !ok {
			vol.termMu.Unlock()
			return nil, false
		}
		if !vol.cacheStampValid(entry.gen) {
			vol.termMu.Unlock()
			return nil, false
		}
		lists = append(lists, append([]int(nil), entry.ids...))
		seqs = append(seqs, entry.gen)
	}
	vol.termMu.Unlock()
	for i, term := range terms {
		lists[i] = vol.withRecentCandidates(lists[i], seqs[i], func(rec CompactRecord) bool {
			id, ok := vol.idForFRN(rec.FRN)
			return ok && strings.Contains(vol.index.compactLowerNameAt(id), term)
		})
	}
	sortIntListsByLen(lists)
	candidates := append([]int(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSortedInts(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	return candidates, true
}

func (vol *serviceVolumeIndex) multiNameTermCandidates(terms []string) []int {
	lists := make([][]int, len(terms))
	for termIndex, term := range terms {
		lists[termIndex] = vol.nameTermPosting(term)
	}
	sortIntListsByLen(lists)
	candidates := append([]int(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSortedInts(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	return candidates
}

func (vol *serviceVolumeIndex) plannerCandidates(pq parsedQuery) ([]int, bool) {
	globExts, globsOK := simpleGlobExts(pq.Globs)
	if vol == nil || vol.index == nil || vol.queryIndex == nil || pq.CaseSensitive || pq.Under != "" || pq.Exists || pq.HasModAfter || !globsOK {
		return nil, false
	}
	strong := make([][]uint32, 0, len(pq.Terms)+len(pq.Exts)+len(globExts)+2)
	addStrong := func(list []uint32) bool {
		if len(list) == 0 {
			return false
		}
		strong = append(strong, list)
		return true
	}
	qi := vol.queryIndex
	lastBareExt := []uint32(nil)
	for _, ext := range pq.Exts {
		if !addStrong(qi.ext[ext]) {
			return []int{}, true
		}
	}
	for _, ext := range globExts {
		if !addStrong(qi.ext[ext]) {
			return []int{}, true
		}
	}
	for _, term := range pq.RegexTerms {
		if list := qi.ext[term]; len(list) > 0 {
			addStrong(list)
		}
	}
	if pq.Type == "dir" {
		addStrong(qi.dirs)
	}
	for _, term := range pq.Terms {
		if pq.MatchPath {
			if strings.HasSuffix(term, ":") {
				if !strings.EqualFold(term, vol.volume) {
					return []int{}, true
				}
				continue
			}
			if strings.HasPrefix(term, ".") && len(term) > 1 {
				if list := qi.ext[strings.TrimPrefix(term, ".")]; len(list) > 0 {
					addStrong(list)
					continue
				}
			}
			if list := qi.ext[term]; len(list) > 0 {
				lastBareExt = list
			}
			if ext := strings.TrimPrefix(filepath.Ext(term), "."); ext != "" {
				if list := qi.ext[ext]; len(list) > 0 {
					lastBareExt = list
				}
			}
			continue
		}
		if strings.HasPrefix(term, ".") && len(term) > 1 {
			if list := qi.ext[strings.TrimPrefix(term, ".")]; len(list) > 0 {
				addStrong(list)
				continue
			}
		}
		if list := qi.ext[term]; len(list) > 0 {
			lastBareExt = list
		}
		if ext := strings.TrimPrefix(filepath.Ext(term), "."); ext != "" {
			if list := qi.ext[ext]; len(list) > 0 {
				lastBareExt = list
			}
		}
	}
	if len(strong) == 0 && len(lastBareExt) > 0 {
		addStrong(lastBareExt)
	}
	if len(strong) == 0 {
		return nil, false
	}
	lists := strong
	sortUint32ListsByLen(lists)
	candidates := append([]uint32(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSortedUint32s(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	out := uint32sToInts(candidates)
	if len(vol.recentIDs) > 0 {
		out = append(out, mapKeys(vol.recentIDs)...)
		sort.Ints(out)
		out = uniqueSortedInts(out)
	}
	return out, true
}

func simpleGlobExts(globs []string) ([]string, bool) {
	if len(globs) == 0 {
		return nil, true
	}
	exts := make([]string, 0, len(globs))
	for _, glob := range globs {
		if !strings.HasPrefix(glob, "*.") || strings.Count(glob, "*") != 1 || strings.ContainsAny(strings.TrimPrefix(glob, "*."), `\/*?[]:`) {
			return nil, false
		}
		ext := strings.ToLower(strings.TrimPrefix(glob, "*."))
		if ext == "" {
			return nil, false
		}
		exts = append(exts, ext)
	}
	return exts, true
}

func (vol *serviceVolumeIndex) underCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || pq.Under == "" {
		return nil, false
	}
	under := filepath.Clean(pq.Under)
	if vol.index.Volume != "" && !strings.EqualFold(filepath.VolumeName(under), vol.index.Volume) {
		return []int{}, true
	}
	base := strings.ToLower(filepath.Base(under))
	if base == "." || base == string(filepath.Separator) || base == "" {
		return nil, false
	}
	roots := vol.underRootIDs(under)
	if len(roots) == 0 {
		return []int{}, true
	}
	if candidates, ok := vol.underLimitedTermCandidates(roots, pq); ok {
		return candidates, true
	}
	out := make([]int, 0, 256)
	prefilter := vol.underPrefilter(pq)
	for _, rootID := range roots {
		if rootID < 0 || rootID >= vol.index.compactRecordCount() || vol.index.compactRecord(rootID).Deleted {
			continue
		}
		if len(vol.childOffsets) == 0 && vol.children == nil {
			if prefilter != nil {
				prefilterIDs := make([]int, 0, len(prefilter))
				for id := range prefilter {
					prefilterIDs = append(prefilterIDs, id)
				}
				sort.Ints(prefilterIDs)
				for _, id := range prefilterIDs {
					if id < 0 || id >= vol.index.compactRecordCount() {
						continue
					}
					rec := vol.index.compactRecord(id)
					if vol.isDescendantOrSelf(id, rootID) && !rec.Deleted && compactRecordPrecheck(rec, pq, true) {
						out = append(out, id)
					}
				}
				continue
			}
			descendants := vol.underDescendants(rootID)
			for _, id := range descendants {
				if id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				rec := vol.index.compactRecord(id)
				if !rec.Deleted && compactRecordPrecheck(rec, pq, true) {
					out = append(out, id)
				}
			}
			continue
		}
		if prefilter != nil {
			for id := range prefilter {
				if id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				rec := vol.index.compactRecord(id)
				if vol.isDescendantOrSelf(id, rootID) && !rec.Deleted && compactRecordPrecheck(rec, pq, true) {
					out = append(out, id)
				}
			}
			continue
		}
		seen := make(map[int]struct{}, 256)
		stack := []int{rootID}
		for len(stack) > 0 {
			last := len(stack) - 1
			id := stack[last]
			stack = stack[:last]
			if _, ok := seen[id]; ok || id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			seen[id] = struct{}{}
			rec := vol.index.compactRecord(id)
			if !rec.Deleted && compactRecordPrecheck(rec, pq, true) {
				out = append(out, id)
			}
			for _, childID := range vol.childIDsForRecord(id) {
				stack = append(stack, int(childID))
			}
		}
	}
	sortCandidateIDs(out, pq, vol.index, vol.rankForQuery(pq))
	return out, true
}

func (vol *serviceVolumeIndex) underRootIDs(under string) []int {
	if vol == nil || vol.index == nil {
		return nil
	}
	cacheKey := strings.ToLower(filepath.Clean(under))
	vol.termMu.Lock()
	if vol.underRootCache != nil {
		if entry, ok := vol.underRootCache[cacheKey]; ok {
			if vol.cacheStampValid(entry.gen) {
				vol.termMu.Unlock()
				return append([]int(nil), entry.ids...)
			}
		}
	}
	vol.termMu.Unlock()
	var roots []int
	volume := filepath.VolumeName(under)
	rest := strings.TrimPrefix(under, volume)
	rest = strings.Trim(rest, `\/`)
	if rest == "" {
		if len(vol.rootIDs) > 0 {
			roots = make([]int, 0, len(vol.rootIDs))
			for _, id := range vol.rootIDs {
				roots = append(roots, int(id))
			}
		} else {
			roots = []int{0}
		}
		vol.cacheUnderRoots(cacheKey, roots)
		return append([]int(nil), roots...)
	}
	parts := strings.FieldsFunc(rest, func(r rune) bool { return r == '\\' || r == '/' })
	candidates := make([]int, 0, 4)
	recordCount := vol.index.compactRecordCount()
	if len(vol.rootIDs) > 0 {
		for _, id := range vol.rootIDs {
			if int(id) < recordCount {
				candidates = append(candidates, int(id))
			}
		}
	} else {
		for id := 0; id < recordCount; id++ {
			rec := vol.index.compactRecord(id)
			if rec.Parent < 0 && !rec.Deleted {
				candidates = append(candidates, id)
			}
		}
	}
	if len(vol.childOffsets) == 0 && vol.children == nil {
		if roots := vol.underRootIDsByBasename(under); len(roots) > 0 {
			vol.cacheUnderRoots(cacheKey, roots)
			return append([]int(nil), roots...)
		}
		roots = vol.underRootIDsByParentScans(candidates, parts)
		vol.cacheUnderRoots(cacheKey, roots)
		return append([]int(nil), roots...)
	}
	for _, part := range parts {
		want := strings.ToLower(part)
		next := make([]int, 0, 4)
		for _, parentID := range candidates {
			for _, childID32 := range vol.childIDsForRecord(parentID) {
				childID := int(childID32)
				if childID < 0 || childID >= recordCount {
					continue
				}
				rec := vol.index.compactRecord(childID)
				if !rec.Deleted && strings.EqualFold(vol.index.compactLowerNameAt(childID), want) {
					next = append(next, childID)
				}
			}
		}
		if len(next) == 0 {
			if roots := vol.underRootIDsByBasename(under); len(roots) > 0 {
				vol.cacheUnderRoots(cacheKey, roots)
				return append([]int(nil), roots...)
			}
			vol.cacheUnderRoots(cacheKey, nil)
			return nil
		}
		candidates = next
	}
	vol.cacheUnderRoots(cacheKey, candidates)
	return append([]int(nil), candidates...)
}

func (vol *serviceVolumeIndex) cacheUnderRoots(key string, roots []int) {
	if key == "" {
		return
	}
	vol.termMu.Lock()
	defer vol.termMu.Unlock()
	if vol.underRootCache == nil {
		vol.underRootCache = make(map[string]postingCacheEntry)
	}
	vol.underRootCache[key] = postingCacheEntry{ids: append([]int(nil), roots...), gen: vol.cacheGeneration()}
}

func (vol *serviceVolumeIndex) underLimitedTermCandidates(roots []int, pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || pq.CountOnly || pq.Limit <= 0 || len(roots) == 0 || len(pq.Terms) != 1 || len(pq.Exts) > 0 || len(pq.Dirs) > 0 || len(pq.Globs) > 0 || len(pq.Regexps) > 0 || pq.Type != "" || pq.HasModAfter || pq.Exists || pq.CaseSensitive {
		return nil, false
	}
	term := pq.Terms[0]
	if term == "" || strings.ContainsAny(term, `\/*?[]:`) {
		return nil, false
	}
	if out, ok := vol.scanUnderRootsTermLimited(roots, term, pq.Limit); ok {
		return out, true
	}
	out := make([]int, 0, pq.Limit)
	seen := make(map[int]struct{}, pq.Limit)
	for _, rootID := range roots {
		for _, id := range vol.subtreeIDsInOrder(rootID) {
			if len(out) >= pq.Limit {
				sort.Ints(out)
				return out, true
			}
			if _, ok := seen[id]; ok || id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			rec := vol.index.compactRecord(id)
			if rec.Deleted {
				continue
			}
			if strings.Contains(vol.index.compactLowerNameAt(id), term) {
				seen[id] = struct{}{}
				out = append(out, id)
			}
		}
	}
	sort.Ints(out)
	return out, true
}

func (vol *serviceVolumeIndex) scanUnderRootsTermLimited(roots []int, term string, limit int) ([]int, bool) {
	if len(roots) == 0 {
		return nil, false
	}
	intervals := make([]interval, 0, len(roots))
	for _, rootID := range roots {
		if rootID < 0 || rootID >= vol.index.compactRecordCount() || rootID >= len(vol.subtreeStart) || rootID >= len(vol.subtreeEnd) {
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
	return vol.scanIntervalsTermLimited(intervals, term, limit), true
}

func (vol *serviceVolumeIndex) scanUnderTermLimited(rootID int, term string, limit int) ([]int, bool) {
	if vol == nil || vol.index == nil || limit <= 0 || rootID < 0 || rootID >= vol.index.compactRecordCount() {
		return nil, false
	}
	if rootID >= len(vol.subtreeStart) || rootID >= len(vol.subtreeEnd) || len(vol.subtreeOrder) == 0 {
		return nil, false
	}
	start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
	if start == ^uint32(0) || start > end || int(end) > len(vol.subtreeOrder) {
		return nil, false
	}
	return vol.scanIntervalsTermLimited([]interval{{start: int(start), end: int(end)}}, term, limit), true
}

type interval struct {
	start int
	end   int
}

func (vol *serviceVolumeIndex) scanIntervalsTermLimited(intervals []interval, term string, limit int) []int {
	total := 0
	for _, iv := range intervals {
		if iv.end > iv.start {
			total += iv.end - iv.start
		}
	}
	n := total
	if n == 0 {
		return nil
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, n/100_000))
	out := make([]int, 0, limit)
	var mu sync.Mutex
	var found atomic.Int32
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		a := worker * n / workers
		b := (worker + 1) * n / workers
		wg.Add(1)
		go func(a, b int) {
			defer wg.Done()
			local := make([]int, 0, 8)
			for logical := a; logical < b && int(found.Load()) < limit; logical++ {
				pos := intervalPosition(intervals, logical)
				if pos < 0 {
					continue
				}
				id := int(vol.subtreeOrder[pos])
				if id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				rec := vol.index.compactRecord(id)
				if rec.Deleted {
					continue
				}
				if strings.Contains(vol.index.compactLowerNameAt(id), term) {
					if found.Add(1) <= int32(limit) {
						local = append(local, id)
					}
				}
			}
			if len(local) > 0 {
				mu.Lock()
				out = append(out, local...)
				mu.Unlock()
			}
		}(a, b)
	}
	wg.Wait()
	sort.Ints(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func intervalPosition(intervals []interval, logical int) int {
	for _, iv := range intervals {
		n := iv.end - iv.start
		if logical < n {
			return iv.start + logical
		}
		logical -= n
	}
	return -1
}

func (vol *serviceVolumeIndex) underRootIDsByBasename(under string) []int {
	base := strings.ToLower(filepath.Base(under))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return nil
	}
	cleanUnder := filepath.Clean(under)
	candidates := vol.exactNameIDs(base)
	out := vol.filterUnderRootCandidates(candidates, base, cleanUnder)
	if len(out) == 0 {
		out = vol.filterUnderRootCandidates(vol.nameTermPosting(base), base, cleanUnder)
	}
	sort.Ints(out)
	return uniqueSortedInts(out)
}

func (vol *serviceVolumeIndex) filterUnderRootCandidates(candidates []int, base, cleanUnder string) []int {
	out := make([]int, 0, 1)
	pathCache := make(map[int]string)
	for _, id := range candidates {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 || vol.index.compactLowerNameAt(id) != base {
			continue
		}
		path := vol.index.reconstructCompactPathCached(id, pathCache)
		if strings.EqualFold(filepath.Clean(path), cleanUnder) {
			out = append(out, id)
		}
	}
	return out
}

func (vol *serviceVolumeIndex) underRootIDsByParentScans(candidates []int, parts []string) []int {
	if len(candidates) == 0 {
		return nil
	}
	recordCount := vol.index.compactRecordCount()
	for _, part := range parts {
		want := strings.ToLower(part)
		parentFRNs := make(map[uint64]struct{}, len(candidates))
		for _, id := range candidates {
			if id < 0 || id >= recordCount {
				continue
			}
			frn := vol.index.compactRecord(id).FRN
			if frn != 0 {
				parentFRNs[frn] = struct{}{}
			}
		}
		if len(parentFRNs) == 0 {
			return nil
		}
		next := make([]int, 0, 4)
		for id := 0; id < recordCount; id++ {
			rec := vol.index.compactRecord(id)
			if rec.Deleted {
				continue
			}
			if _, ok := parentFRNs[rec.ParentFRN]; !ok {
				continue
			}
			if vol.index.compactLowerNameAt(id) == want {
				next = append(next, id)
			}
		}
		if len(next) == 0 {
			return nil
		}
		candidates = next
	}
	return candidates
}

func (vol *serviceVolumeIndex) isDescendantOrSelf(id, rootID int) bool {
	if vol != nil && id >= 0 && rootID >= 0 && id < len(vol.subtreeStart) && rootID < len(vol.subtreeStart) {
		pos, start, end := vol.subtreeStart[id], vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start != ^uint32(0) && pos != ^uint32(0) {
			return pos >= start && pos < end
		}
	}
	seen := make(map[int]struct{}, 16)
	cur := id
	for depth := 0; depth < 1024; depth++ {
		if cur == rootID {
			return true
		}
		if cur < 0 || cur >= vol.index.compactRecordCount() {
			return false
		}
		if _, ok := seen[cur]; ok {
			return false
		}
		seen[cur] = struct{}{}
		parent := vol.index.compactRecord(cur).Parent
		if parent < 0 {
			return false
		}
		cur = int(parent)
	}
	return false
}

func (vol *serviceVolumeIndex) isDescendantOrSelfAnyFast(id int, roots []int) bool {
	if vol == nil || vol.index == nil || id < 0 || len(roots) == 0 {
		return false
	}
	recordCount := vol.index.compactRecordCount()
	cur := id
	for depth := 0; depth < 1024; depth++ {
		if cur < 0 || cur >= recordCount {
			return false
		}
		for _, rootID := range roots {
			if cur == rootID {
				return true
			}
		}
		parent := int(vol.index.compactRecord(cur).Parent)
		if parent < 0 || parent == cur {
			return false
		}
		cur = parent
	}
	return false
}

func (vol *serviceVolumeIndex) underDescendants(rootID int) []int {
	if vol == nil || vol.index == nil || rootID < 0 || rootID >= vol.index.compactRecordCount() {
		return nil
	}
	vol.termMu.Lock()
	if vol.underCache != nil {
		if entry, ok := vol.underCache[rootID]; ok {
			if vol.cacheStampValid(entry.gen) {
				vol.termMu.Unlock()
				return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
					id, ok := vol.idForFRN(rec.FRN)
					return ok && vol.isDescendantOrSelf(id, rootID)
				})
			}
		}
	}
	vol.termMu.Unlock()
	recordCount := vol.index.compactRecordCount()
	out := make([]int, 0, 256)
	if rootID < len(vol.subtreeStart) && rootID < len(vol.subtreeEnd) && len(vol.subtreeOrder) > 0 {
		start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start != ^uint32(0) && start <= end && int(end) <= len(vol.subtreeOrder) {
			out = make([]int, 0, int(end-start))
			for _, id32 := range vol.subtreeOrder[start:end] {
				id := int(id32)
				if id < 0 || id >= recordCount {
					continue
				}
				if !vol.index.compactRecord(id).Deleted {
					out = append(out, id)
				}
			}
		}
	} else if len(vol.childOffsets) > 0 || vol.children != nil {
		stack := []int{rootID}
		seen := make(map[int]struct{}, 256)
		for len(stack) > 0 {
			last := len(stack) - 1
			id := stack[last]
			stack = stack[:last]
			if _, ok := seen[id]; ok || id < 0 || id >= recordCount {
				continue
			}
			seen[id] = struct{}{}
			rec := vol.index.compactRecord(id)
			if !rec.Deleted {
				out = append(out, id)
			}
			for _, childID := range vol.childIDsForRecord(id) {
				stack = append(stack, int(childID))
			}
		}
	} else {
		for id := 0; id < recordCount; id++ {
			rec := vol.index.compactRecord(id)
			if rec.Deleted {
				continue
			}
			if vol.isDescendantOrSelf(id, rootID) {
				out = append(out, id)
			}
		}
	}
	if len(out) == 0 {
		return out
	}
	sort.Ints(out)
	if vol.shouldCachePosting(out) {
		vol.termMu.Lock()
		if vol.underCache == nil {
			vol.underCache = make(map[int]postingCacheEntry)
		}
		vol.underCache[rootID] = postingCacheEntry{ids: out, gen: vol.cacheGeneration()}
		vol.termMu.Unlock()
	}
	return out
}

func (vol *serviceVolumeIndex) underDescendantsLimited(rootID, limit int) []int {
	if vol == nil || vol.index == nil || rootID < 0 || rootID >= vol.index.compactRecordCount() {
		return nil
	}
	if limit <= 0 {
		return vol.underDescendants(rootID)
	}
	recordCount := vol.index.compactRecordCount()
	if rootID < len(vol.subtreeStart) && rootID < len(vol.subtreeEnd) && len(vol.subtreeOrder) > 0 {
		start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start != ^uint32(0) && start <= end && int(end) <= len(vol.subtreeOrder) {
			out := make([]int, 0, min(limit, int(end-start)))
			for _, id32 := range vol.subtreeOrder[start:end] {
				if len(out) >= limit {
					break
				}
				id := int(id32)
				if id < 0 || id >= recordCount {
					continue
				}
				if !vol.index.compactRecord(id).Deleted {
					out = append(out, id)
				}
			}
			sort.Ints(out)
			return out
		}
	}
	out := make([]int, 0, min(limit, 256))
	stack := []int{rootID}
	seen := make(map[int]struct{}, 256)
	for len(stack) > 0 && len(out) < limit {
		last := len(stack) - 1
		id := stack[last]
		stack = stack[:last]
		if _, ok := seen[id]; ok || id < 0 || id >= recordCount {
			continue
		}
		seen[id] = struct{}{}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted {
			out = append(out, id)
		}
		for _, childID := range vol.childIDsForRecord(id) {
			stack = append(stack, int(childID))
		}
	}
	sort.Ints(out)
	return out
}

func (vol *serviceVolumeIndex) underPrefilter(pq parsedQuery) map[int]struct{} {
	if vol == nil || len(pq.Regexps) > 0 || pq.CaseSensitive || pq.HasModAfter || pq.Exists {
		return nil
	}
	lists := make([][]int, 0, len(pq.Exts)+len(pq.Dirs)+len(pq.Terms)+len(pq.Globs))
	for _, ext := range pq.Exts {
		list := vol.extPosting(ext)
		if len(list) == 0 {
			return map[int]struct{}{}
		}
		lists = append(lists, list)
	}
	globExts, globsOK := simpleGlobExts(pq.Globs)
	if globsOK {
		for _, ext := range globExts {
			list := vol.extPosting(ext)
			if len(list) == 0 {
				return map[int]struct{}{}
			}
			lists = append(lists, list)
		}
	} else {
		for _, ext := range complexGlobExts(pq.Globs) {
			list := vol.extPosting(ext)
			if len(list) == 0 {
				return map[int]struct{}{}
			}
			lists = append(lists, list)
		}
		for _, globTerm := range globLiteralTerms(pq.Globs, pq.CaseSensitive) {
			list := vol.nameTermPosting(globTerm)
			if len(list) == 0 {
				continue
			}
			lists = append(lists, list)
		}
	}
	for _, dir := range pq.Dirs {
		list := vol.pathComponentPosting(dir)
		if len(list) == 0 {
			return map[int]struct{}{}
		}
		lists = append(lists, list)
	}
	hasDottedTerm := false
	for _, term := range pq.Terms {
		if strings.Contains(term, ".") {
			hasDottedTerm = true
			break
		}
	}
	for _, term := range pq.Terms {
		if pq.MatchPath && hasDottedTerm && !strings.Contains(term, ".") {
			continue
		}
		list := []int(nil)
		if ext, ok := dottedExtensionTerm(term); ok {
			list = vol.extPosting(ext)
		} else if strings.Contains(term, ".") {
			list = vol.exactNameIDs(term)
		}
		if len(list) == 0 {
			list = vol.nameTermPosting(term)
		}
		if pq.MatchPath && len(list) == 0 {
			list = vol.pathTermPosting(term)
		}
		if len(list) == 0 {
			return map[int]struct{}{}
		}
		lists = append(lists, list)
	}
	if len(lists) == 0 {
		return nil
	}
	sortIntListsByLen(lists)
	candidates := append([]int(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSortedInts(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	out := make(map[int]struct{}, len(candidates))
	for _, id := range candidates {
		out[id] = struct{}{}
	}
	return out
}

func sortCandidateIDs(ids []int, pq parsedQuery, idx *Index, cachedRanks []uint32) {
	if idx == nil {
		sort.Ints(ids)
		return
	}
	rankOf := candidateRanker(idx, cachedRanks)
	sort.SliceStable(ids, func(i, j int) bool {
		return rankOf(ids[i]) < rankOf(ids[j])
	})
}

func topCandidateIDsByRank(ids []int, limit int, idx *Index, cachedRanks []uint32) []int {
	if limit <= 0 || len(ids) <= limit {
		sortCandidateIDs(ids, parsedQuery{}, idx, cachedRanks)
		return ids
	}
	if idx == nil {
		sort.Ints(ids)
		return ids[:limit]
	}
	rankOf := candidateRanker(idx, cachedRanks)
	if len(ids) >= serviceRankParallelMinIDs && limit <= 256 && runtime.GOMAXPROCS(0) > 1 {
		return topCandidateIDsByRankParallel(ids, limit, rankOf)
	}
	h := make(candidateRankMaxHeap, 0, limit)
	for _, id := range ids {
		item := candidateRankItem{id: id, rank: rankOf(id)}
		if len(h) < limit {
			heap.Push(&h, item)
			continue
		}
		if item.rank < h[0].rank {
			h[0] = item
			heap.Fix(&h, 0)
		}
	}
	out := make([]int, len(h))
	for i := range h {
		out[i] = h[i].id
	}
	sortIDsByRank(out, rankOf)
	return out
}

func topCandidateIDsByRankParallel(ids []int, limit int, rankOf func(int) int) []int {
	workers := min(runtime.GOMAXPROCS(0), max(2, len(ids)/serviceRankParallelMinIDs))
	if workers <= 1 {
		return topCandidateIDsByRankSerial(ids, limit, rankOf)
	}
	chunk := (len(ids) + workers - 1) / workers
	partials := make([][]candidateRankItem, workers)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := worker * chunk
		end := min(len(ids), start+chunk)
		if start >= end {
			partials = partials[:worker]
			break
		}
		wg.Add(1)
		go func(worker, start, end int) {
			defer wg.Done()
			partials[worker] = topCandidateRankItems(ids[start:end], limit, rankOf)
		}(worker, start, end)
	}
	wg.Wait()
	h := make(candidateRankMaxHeap, 0, limit)
	for _, partial := range partials {
		for _, item := range partial {
			if len(h) < limit {
				heap.Push(&h, item)
				continue
			}
			if item.rank < h[0].rank {
				h[0] = item
				heap.Fix(&h, 0)
			}
		}
	}
	out := make([]int, len(h))
	for i := range h {
		out[i] = h[i].id
	}
	sortIDsByRank(out, rankOf)
	return out
}

func topCandidateIDsByRankSerial(ids []int, limit int, rankOf func(int) int) []int {
	items := topCandidateRankItems(ids, limit, rankOf)
	out := make([]int, len(items))
	for i := range items {
		out[i] = items[i].id
	}
	sortIDsByRank(out, rankOf)
	return out
}

func topCandidateRankItems(ids []int, limit int, rankOf func(int) int) []candidateRankItem {
	h := make(candidateRankMaxHeap, 0, limit)
	for _, id := range ids {
		item := candidateRankItem{id: id, rank: rankOf(id)}
		if len(h) < limit {
			heap.Push(&h, item)
			continue
		}
		if item.rank < h[0].rank {
			h[0] = item
			heap.Fix(&h, 0)
		}
	}
	out := make([]candidateRankItem, len(h))
	copy(out, h)
	return out
}

type candidateRankItem struct {
	id   int
	rank int
}

type candidateRankMaxHeap []candidateRankItem

func (h candidateRankMaxHeap) Len() int { return len(h) }

func (h candidateRankMaxHeap) Less(i, j int) bool { return h[i].rank > h[j].rank }

func (h candidateRankMaxHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *candidateRankMaxHeap) Push(x any) {
	*h = append(*h, x.(candidateRankItem))
}

func (h *candidateRankMaxHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

func candidateRanker(idx *Index, cachedRanks []uint32) func(int) int {
	recordCount := idx.compactRecordCount()
	var ranks []int
	if len(cachedRanks) < recordCount && len(idx.CompactNameOrder) > 0 {
		ranks = make([]int, recordCount)
		for i := range ranks {
			ranks[i] = i
		}
		order := idx.CompactNameOrder
		for pos := 0; pos < compactOrderLen(order, recordCount); pos++ {
			id := compactOrderAt(order, pos)
			if id >= 0 && id < recordCount {
				ranks[id] = pos
			}
		}
	}
	return func(id int) int {
		rank := recordCount + id
		if id < 0 || id >= recordCount {
			return rank
		}
		if len(cachedRanks) >= recordCount {
			return int(cachedRanks[id])
		}
		if len(ranks) == 0 {
			return id
		}
		return ranks[id]
	}
}

func (vol *serviceVolumeIndex) nameOrderRanks() []uint32 {
	if vol == nil || vol.queryIndex == nil || len(vol.queryIndex.nameRank) == 0 {
		return nil
	}
	return vol.queryIndex.nameRank
}

func (vol *serviceVolumeIndex) rankForQuery(pq parsedQuery) []uint32 {
	if pq.SortColumn == "size" {
		if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.sizeRank) > 0 {
			return vol.queryIndex.sizeRank
		}
		if vol != nil && vol.index != nil && len(vol.index.Derived.SizeRank) > 0 {
			return vol.index.Derived.SizeRank
		}
		if vol != nil && vol.index != nil && vol.index.compactHasSize() {
			_, ranks := buildCompactSizeOrderRank(vol.index)
			return ranks
		}
	}
	if pq.SortColumn == "modified" {
		if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.modRank) > 0 {
			return vol.queryIndex.modRank
		}
		if vol != nil && vol.index != nil && len(vol.index.Derived.ModRank) > 0 {
			return vol.index.Derived.ModRank
		}
		if vol != nil && vol.index != nil && vol.index.compactHasModTime() {
			_, ranks := buildCompactModifiedOrderRank(vol.index)
			return ranks
		}
	}
	if pq.SortColumn == "extension" {
		if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.extRank) > 0 {
			return vol.queryIndex.extRank
		}
		if vol != nil && vol.index != nil && len(vol.index.Derived.ExtRank) > 0 {
			return vol.index.Derived.ExtRank
		}
		if vol != nil && vol.index != nil {
			_, ranks := buildCompactExtensionOrderRank(vol.index)
			return ranks
		}
	}
	if pq.SortColumn == "type" {
		if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.typeRank) > 0 {
			return vol.queryIndex.typeRank
		}
		if vol != nil && vol.index != nil && len(vol.index.Derived.TypeRank) > 0 {
			return vol.index.Derived.TypeRank
		}
		if vol != nil && vol.index != nil {
			_, ranks := buildCompactTypeOrderRank(vol.index)
			return ranks
		}
	}
	if pq.SortColumn == "path" {
		if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.pathRank) > 0 {
			return vol.queryIndex.pathRank
		}
		if vol != nil && vol.index != nil && len(vol.index.Derived.PathRank) > 0 {
			return vol.index.Derived.PathRank
		}
		if vol != nil && vol.index != nil {
			_, ranks := buildCompactPathOrderRank(vol.index)
			return ranks
		}
	}
	if ranks := vol.nameOrderRanks(); len(ranks) > 0 {
		return ranks
	}
	if vol != nil && vol.index != nil && len(vol.index.Derived.NameRank) > 0 {
		return vol.index.Derived.NameRank
	}
	return nil
}

func (vol *serviceVolumeIndex) orderForQuery(pq parsedQuery) []uint32 {
	if pq.SortColumn == "size" {
		return vol.sizeOrderForRank()
	}
	if pq.SortColumn == "modified" {
		return vol.modifiedOrderForRank()
	}
	if pq.SortColumn == "extension" {
		return vol.extensionOrderForRank()
	}
	if pq.SortColumn == "type" {
		return vol.typeOrderForRank()
	}
	if pq.SortColumn == "path" {
		return vol.pathOrderForRank()
	}
	return vol.mappedOrCompactNameOrder()
}

func (vol *serviceVolumeIndex) pathDirFilterCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || len(pq.Terms) == 0 || len(pq.Regexps) > 0 || pq.Under != "" || pq.CaseSensitive {
		return nil, false
	}
	if len(pq.Exts) == 0 && len(pq.Dirs) == 0 && len(pq.Globs) == 0 && pq.Type == "" && !pq.HasModAfter {
		return nil, false
	}
	type rootTerm struct {
		id   int
		term string
	}
	roots := make([]rootTerm, 0, 4)
	for _, term := range pq.Terms {
		if strings.ContainsAny(term, `\/*?[]:`) {
			continue
		}
		for _, id := range vol.exactNameIDs(term) {
			if id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			rec := vol.index.compactRecord(id)
			if !rec.Deleted && rec.Mode&uint32(os.ModeDir) != 0 {
				roots = append(roots, rootTerm{id: id, term: term})
			}
		}
	}
	if len(roots) == 0 {
		return nil, false
	}
	prefilter := vol.underPrefilter(pq)
	if prefilter == nil {
		return nil, false
	}
	out := make([]int, 0, 64)
	seen := make(map[int]struct{}, 64)
	for _, root := range roots {
		for id := range prefilter {
			if _, ok := seen[id]; ok || id < 0 || id >= vol.index.compactRecordCount() || !vol.isDescendantOrSelf(id, root.id) {
				continue
			}
			rec := vol.index.compactRecord(id)
			if rec.Deleted || !compactRecordPrecheck(rec, pq, true) || !vol.recordPathContainsRemainingTerms(id, pq.Terms, root.term) {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out, true
}

func (vol *serviceVolumeIndex) recordPathContainsRemainingTerms(id int, terms []string, rootTerm string) bool {
	for _, term := range terms {
		if term == rootTerm {
			continue
		}
		if !vol.index.compactPathContainsTerm(id, term) {
			return false
		}
	}
	return true
}

func (vol *serviceVolumeIndex) filterCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || len(pq.Regexps) > 0 || pq.Under != "" || pq.CaseSensitive {
		return nil, false
	}
	if len(pq.Exts) == 0 && len(pq.Dirs) == 0 {
		return nil, false
	}
	lists := make([][]int, 0, len(pq.Exts)+len(pq.Dirs)+len(pq.Terms))
	for _, ext := range pq.Exts {
		list := vol.extPosting(ext)
		if len(list) == 0 {
			return []int{}, true
		}
		lists = append(lists, list)
	}
	for _, dir := range pq.Dirs {
		list := vol.pathComponentPosting(dir)
		if len(list) == 0 {
			return []int{}, true
		}
		lists = append(lists, list)
	}
	for _, term := range pq.Terms {
		list := vol.nameTermPosting(term)
		if pq.MatchPath {
			list = vol.pathTermPosting(term)
		}
		if len(list) == 0 {
			return []int{}, true
		}
		lists = append(lists, list)
	}
	if len(lists) == 0 {
		return nil, false
	}
	sortIntListsByLen(lists)
	candidates := append([]int(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSortedInts(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	return candidates, true
}

func (vol *serviceVolumeIndex) regexLiteralCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || len(pq.Regexps) == 0 || len(pq.RegexTerms) != 1 || pq.CaseSensitive {
		return nil, false
	}
	lists := make([][]int, 0, len(pq.RegexTerms))
	for _, term := range pq.RegexTerms {
		list := vol.pathTermPosting(term)
		if len(list) == 0 {
			return []int{}, true
		}
		lists = append(lists, list)
	}
	sortIntListsByLen(lists)
	candidates := append([]int(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSortedInts(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	return candidates, true
}

func (vol *serviceVolumeIndex) pathRootLimitedCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CountOnly || len(pq.Terms) < 2 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || pq.Under != "" || pq.CaseSensitive || pq.Limit <= 0 {
		return nil, false
	}
	type rootTerm struct {
		id   int
		term string
	}
	roots := make([]rootTerm, 0, 4)
	for _, term := range pq.Terms {
		if strings.ContainsAny(term, `\/*?[]:`) {
			continue
		}
		for _, id := range vol.pathComponentRootIDs(term) {
			if id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			rec := vol.index.compactRecord(id)
			if !rec.Deleted && rec.Mode&uint32(os.ModeDir) != 0 {
				roots = append(roots, rootTerm{id: id, term: term})
			}
		}
	}
	if len(roots) == 0 {
		return nil, false
	}
	out := make([]int, 0, pq.Limit)
	seen := make(map[int]struct{}, pq.Limit)
	for _, root := range roots {
		for _, id := range vol.subtreeIDsInOrder(root.id) {
			if len(out) >= pq.Limit {
				sort.Ints(out)
				return out, true
			}
			if _, ok := seen[id]; ok || id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			rec := vol.index.compactRecord(id)
			if rec.Deleted || !compactRecordPrecheck(rec, pq, true) || !vol.recordPathContainsRemainingTerms(id, pq.Terms, root.term) {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out, true
}

func (vol *serviceVolumeIndex) subtreeIDsInOrder(rootID int) []int {
	if vol == nil || vol.index == nil || rootID < 0 || rootID >= vol.index.compactRecordCount() {
		return nil
	}
	if rootID < len(vol.subtreeStart) && rootID < len(vol.subtreeEnd) && len(vol.subtreeOrder) > 0 {
		start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start != ^uint32(0) && start <= end && int(end) <= len(vol.subtreeOrder) {
			out := make([]int, 0, int(end-start))
			for _, id32 := range vol.subtreeOrder[start:end] {
				out = append(out, int(id32))
			}
			return out
		}
	}
	return vol.underDescendants(rootID)
}

func (vol *serviceVolumeIndex) exactDirCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || len(pq.Terms) != 1 || pq.Type != "dir" || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || pq.Under != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 || pq.HasModAfter || pq.Exists || pq.CaseSensitive {
		return nil, false
	}
	list := vol.exactNameIDs(pq.Terms[0])
	out := make([]int, 0, len(list))
	for _, id := range list {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted && rec.Mode&uint32(os.ModeDir) != 0 {
			out = append(out, id)
		}
	}
	return out, true
}

func (vol *serviceVolumeIndex) pathTermSubtreeCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || len(pq.Terms) < 2 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || pq.Under != "" || pq.CaseSensitive {
		return nil, false
	}
	lists := make([][]int, 0, len(pq.Terms))
	for _, term := range pq.Terms {
		list := vol.pathPlanTermPosting(term)
		if len(list) == 0 {
			return []int{}, true
		}
		lists = append(lists, list)
	}
	sortIntListsByLen(lists)
	candidates := append([]int(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSortedInts(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	if len(candidates) > 4096 {
		if nameList, ok := vol.unionNamePostings(pq.Terms); ok {
			candidates = intersectSortedInts(candidates, nameList)
		}
	}
	return candidates, true
}

func (vol *serviceVolumeIndex) unionNamePostings(terms []string) ([]int, bool) {
	seen := make(map[int]struct{}, 64)
	out := make([]int, 0, 64)
	for _, term := range terms {
		for _, id := range vol.nameTermPosting(term) {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out, len(out) > 0
}

func (vol *serviceVolumeIndex) exactNameCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || len(pq.Terms) != 1 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || pq.Under != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 || pq.Type != "" || pq.HasModAfter || pq.Exists || pq.CaseSensitive {
		return nil, false
	}
	term := pq.Terms[0]
	if !strings.Contains(term, ".") {
		return nil, false
	}
	list := vol.exactNameIDs(term)
	out := make([]int, 0, len(list))
	for _, id := range list {
		if id >= 0 && id < vol.index.compactRecordCount() && !vol.index.compactRecord(id).Deleted {
			out = append(out, id)
		}
	}
	return out, len(out) > 0
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA == nil {
		a = aa
	}
	if errB == nil {
		b = bb
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func (vol *serviceVolumeIndex) namePrefixCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || len(pq.Terms) != 1 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || pq.Under != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 || pq.Type != "" || pq.HasModAfter || pq.Exists || pq.CaseSensitive {
		return nil, false
	}
	term := pq.Terms[0]
	if len(term) < 8 || strings.ContainsAny(term, `\/*?[]:`) {
		return nil, false
	}
	var order []uint32
	if vol.queryIndex != nil && len(vol.queryIndex.nameOrder) > 0 {
		order = vol.queryIndex.nameOrder
	} else if len(vol.index.CompactNameOrder) > 0 {
		order = make([]uint32, len(vol.index.CompactNameOrder))
		for i, id := range vol.index.CompactNameOrder {
			order[i] = uint32(id)
		}
	}
	if len(order) == 0 {
		return nil, false
	}
	start := sort.Search(len(order), func(i int) bool {
		return vol.index.compactLowerNameAt(int(order[i])) >= term
	})
	out := make([]int, 0, 8)
	seen := make(map[int]struct{})
	for i := start; i < len(order); i++ {
		id := int(order[i])
		rec := vol.index.compactRecord(id)
		if !strings.HasPrefix(vol.index.compactLowerNameAt(id), term) {
			break
		}
		if !rec.Deleted {
			out = append(out, id)
			seen[id] = struct{}{}
		}
	}
	for id := range vol.recentIDs {
		if _, ok := seen[id]; ok || id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted && strings.HasPrefix(vol.index.compactLowerNameAt(id), term) {
			out = append(out, id)
		}
	}
	return out, len(out) > 0
}

func (vol *serviceVolumeIndex) exactNameIDs(name string) []int {
	if vol == nil || vol.index == nil || name == "" {
		return nil
	}
	if vol.exactNames != nil {
		return append([]int(nil), vol.exactNames[name]...)
	}
	cacheKey := "\x00exact:" + name
	vol.termMu.Lock()
	if vol.termCache != nil {
		if entry, ok := vol.termCache[cacheKey]; ok {
			if vol.cacheStampValid(entry.gen) {
				vol.termMu.Unlock()
				return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
					id, ok := vol.idForFRN(rec.FRN)
					return ok && vol.index.compactLowerNameAt(id) == name
				})
			}
		}
	}
	vol.termMu.Unlock()
	if vol.queryIndex == nil || len(vol.queryIndex.nameOrder) == 0 {
		out := vol.scanExactNameIDs(name)
		vol.cacheNamePosting(cacheKey, out)
		return out
	}
	order := vol.queryIndex.nameOrder
	start := sort.Search(len(order), func(i int) bool {
		return vol.index.compactLowerNameAt(int(order[i])) >= name
	})
	if start >= len(order) || vol.index.compactLowerNameAt(int(order[start])) != name {
		return nil
	}
	out := make([]int, 0, 4)
	for i := start; i < len(order); i++ {
		id := int(order[i])
		if vol.index.compactLowerNameAt(id) != name {
			break
		}
		out = append(out, id)
	}
	for id := range vol.recentIDs {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted && vol.index.compactLowerNameAt(id) == name {
			out = append(out, id)
		}
	}
	sort.Ints(out)
	out = uniqueSortedInts(out)
	return out
}

func (vol *serviceVolumeIndex) scanExactNameIDs(name string) []int {
	if ext := strings.TrimPrefix(filepath.Ext(name), "."); ext != "" && vol.queryIndex != nil {
		if extIDs := vol.extPosting32(ext); len(extIDs) > 0 {
			out := make([]int, 0, 4)
			for _, id32 := range extIDs {
				id := int(id32)
				if id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				rec := vol.index.compactRecord(id)
				if !rec.Deleted && vol.index.compactLowerNameAt(id) == name {
					out = append(out, id)
				}
			}
			for id := range vol.recentIDs {
				if id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				rec := vol.index.compactRecord(id)
				if !rec.Deleted && vol.index.compactLowerNameAt(id) == name {
					out = append(out, id)
				}
			}
			sort.Ints(out)
			return uniqueSortedInts(out)
		}
	}
	out := make([]int, 0, 4)
	recordCount := vol.index.compactRecordCount()
	for id := 0; id < recordCount; id++ {
		rec := vol.index.compactRecord(id)
		if !rec.Deleted && vol.index.compactLowerNameAt(id) == name {
			out = append(out, id)
		}
	}
	return out
}

func (vol *serviceVolumeIndex) nameTermPosting(term string) []int {
	vol.termMu.Lock()
	if vol.termCache == nil {
		vol.termCache = make(map[string]postingCacheEntry)
	}
	if entry, ok := vol.termCache[term]; ok {
		if vol.cacheStampValid(entry.gen) {
			vol.termMu.Unlock()
			return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
				id, ok := vol.idForFRN(rec.FRN)
				return ok && strings.Contains(vol.index.compactLowerNameAt(id), term)
			})
		}
	}
	vol.termMu.Unlock()
	list := vol.scanNameTermPosting(term)
	vol.cacheNamePosting(term, list)
	return list
}

func postingListCacheMaxBytes() int64 {
	maxBytes := postingBlockCacheMaxBytes()
	if maxBytes <= 0 {
		return 0
	}
	return maxBytes / 4
}

func postingListBytes(list []int) int64 {
	return int64(len(list)) * int64(unsafe.Sizeof(int(0)))
}

func (vol *serviceVolumeIndex) shouldCachePosting(list []int) bool {
	maxBytes := postingListCacheMaxBytes()
	return maxBytes > 0 && postingListBytes(list) <= maxBytes
}

func postingEntryCacheBytes(cache map[string]postingCacheEntry) int64 {
	var total int64
	for _, entry := range cache {
		total += postingListBytes(entry.ids)
	}
	return total
}

func postingRootCacheBytes(cache map[int]postingCacheEntry) int64 {
	var total int64
	for _, entry := range cache {
		total += postingListBytes(entry.ids)
	}
	return total
}

func (vol *serviceVolumeIndex) postingListCacheBytesLocked() int64 {
	if vol == nil {
		return 0
	}
	return postingEntryCacheBytes(vol.termCache) +
		postingEntryCacheBytes(vol.pathTermCache) +
		postingEntryCacheBytes(vol.extCache) +
		postingEntryCacheBytes(vol.underRootCache) +
		postingRootCacheBytes(vol.underCache)
}

func (vol *serviceVolumeIndex) cacheNamePosting(term string, list []int) {
	if !vol.shouldCachePosting(list) {
		return
	}
	vol.termMu.Lock()
	if vol.termCache == nil {
		vol.termCache = make(map[string]postingCacheEntry)
	}
	vol.termCache[term] = postingCacheEntry{ids: list, gen: vol.cacheGeneration()}
	vol.termMu.Unlock()
}

func (vol *serviceVolumeIndex) cachePathPosting(term string, list []int) {
	if !vol.shouldCachePosting(list) {
		return
	}
	vol.termMu.Lock()
	if vol.pathTermCache == nil {
		vol.pathTermCache = make(map[string]postingCacheEntry)
	}
	vol.pathTermCache[term] = postingCacheEntry{ids: list, gen: vol.cacheGeneration()}
	vol.termMu.Unlock()
}

func (vol *serviceVolumeIndex) cacheExtPosting(ext string, list []int) {
	if !vol.shouldCachePosting(list) {
		return
	}
	vol.termMu.Lock()
	if vol.extCache == nil {
		vol.extCache = make(map[string]postingCacheEntry)
	}
	vol.extCache[ext] = postingCacheEntry{ids: list, gen: vol.cacheGeneration()}
	vol.termMu.Unlock()
}

func (vol *serviceVolumeIndex) scanNameTermPosting(term string) []int {
	if vol != nil && vol.index != nil {
		if ids, ok := vol.index.scanCompactLowerNameTerm(term); ok {
			return ids
		}
	}
	recordCount := vol.index.compactRecordCount()
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
			}
		}
		return out
	}
	parts := make([][]int, workers)
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
				}
			}
			parts[worker] = local
		}(worker, start, end)
	}
	wg.Wait()
	total := 0
	for _, part := range parts {
		total += len(part)
	}
	out := make([]int, 0, total)
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

// countBareTermParallel counts records whose lower name contains the term, in
// parallel, without materializing the matching ID slice.  It returns ok=false
// when a mapped bulk scan is available so the caller keeps using the
// slice-based posting (which is cached and reused by search), and only pays the
// count-only walk when a full materialization would be required anyway.
func (vol *serviceVolumeIndex) countBareTermParallel(term string, hidden hiddenBaseIDs, pq parsedQuery) (int, bool) {
	if vol == nil || vol.index == nil || term == "" {
		return 0, false
	}
	if vol.index.MMapRecords != nil {
		// The mapped fast path returns the slice cheaply; prefer it so search
		// and count share the same cached posting.
		if _, ok := vol.index.scanCompactLowerNameTerm(term); ok {
			return 0, false
		}
	}
	recordCount := vol.index.compactRecordCount()
	workers := min(runtime.GOMAXPROCS(0), max(1, recordCount/250_000))
	count := 0
	if workers <= 1 {
		for i := 0; i < recordCount; i++ {
			if i&1023 == 0 && queryCanceled(pq) {
				return 0, false
			}
			rec := vol.index.compactRecord(i)
			if rec.Deleted {
				continue
			}
			if !hidden.empty() && hidden.contains(i) {
				continue
			}
			if strings.Contains(vol.index.compactLowerNameAt(i), term) {
				count++
			}
		}
		return count, true
	}
	parts := make([]int, workers)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := worker * recordCount / workers
		end := (worker + 1) * recordCount / workers
		wg.Add(1)
		go func(worker, start, end int) {
			defer wg.Done()
			local := 0
			for i := start; i < end; i++ {
				if i&1023 == 0 && queryCanceled(pq) {
					return
				}
				rec := vol.index.compactRecord(i)
				if rec.Deleted {
					continue
				}
				if !hidden.empty() && hidden.contains(i) {
					continue
				}
				if strings.Contains(vol.index.compactLowerNameAt(i), term) {
					local++
				}
			}
			parts[worker] = local
		}(worker, start, end)
	}
	wg.Wait()
	if queryCanceled(pq) {
		return 0, false
	}
	for _, part := range parts {
		count += part
	}
	return count, true
}

// scanCompactLowerNameTerm is the complete mapped-name fallback used when a
// persisted trigram posting is selective/incomplete.  The normal compact
// accessors intentionally reconstruct a CompactRecord on every visit; that
// is needlessly expensive for this exact, read-only name-table scan.  Read
// the lower-name token and deleted bit directly from the mapped record table
// while preserving the same record/substring semantics.
func (idx *Index) scanCompactLowerNameTerm(term string) ([]int, bool) {
	if idx == nil || term == "" {
		return nil, false
	}
	if m := idx.MMapRecords; m != nil {
		derived := m.fileDerived()
		if len(derived.LowerOffs) == 0 || len(derived.LowerLens) != len(derived.LowerOffs) {
			return nil, false
		}
		size := compactDiskRecordBytes
		if m.wideRefs {
			size = compactWideDiskRecordBytes
		}
		if size <= 0 || len(m.recordData) < m.count*size {
			return nil, false
		}
		termBytes := []byte(term)
		nameBytesForToken := func(token uint32) []byte {
			if token >= uint32(len(derived.LowerOffs)) {
				return nil
			}
			off := derived.LowerOffs[token]
			var nameBytes []byte
			if off == packedLowerSameAsName {
				nameOff := int(token) * 6
				if nameOff >= 0 && nameOff+6 <= len(m.tokenTable) {
					nameStart := binary.LittleEndian.Uint32(m.tokenTable[nameOff:])
					nameLen := binary.LittleEndian.Uint16(m.tokenTable[nameOff+4:])
					nameEnd := int(nameStart) + int(nameLen)
					if nameEnd >= int(nameStart) && nameEnd <= len(m.nameBlob) {
						nameBytes = m.nameBlob[int(nameStart):nameEnd]
					}
				}
			} else {
				end := int(off) + int(derived.LowerLens[token])
				if end >= int(off) && end <= len(derived.LowerBlob) {
					nameBytes = derived.LowerBlob[int(off):end]
				}
			}
			return nameBytes
		}
		tokenMatches := make([]bool, len(derived.LowerOffs))
		matchWorkers := min(runtime.GOMAXPROCS(0), max(1, len(derived.LowerOffs)/250_000))
		var matchWG sync.WaitGroup
		for worker := 0; worker < matchWorkers; worker++ {
			start := worker * len(derived.LowerOffs) / matchWorkers
			end := (worker + 1) * len(derived.LowerOffs) / matchWorkers
			matchWG.Add(1)
			go func(start, end int) {
				defer matchWG.Done()
				for nameID := start; nameID < end; nameID++ {
					tokenMatches[nameID] = bytes.Contains(nameBytesForToken(uint32(nameID)), termBytes)
				}
			}(start, end)
		}
		matchWG.Wait()
		workers := min(runtime.GOMAXPROCS(0), max(1, m.count/250_000))
		parts := make([][]int, workers)
		var wg sync.WaitGroup
		for worker := 0; worker < workers; worker++ {
			start := worker * m.count / workers
			end := (worker + 1) * m.count / workers
			wg.Add(1)
			go func(worker, start, end int) {
				defer wg.Done()
				local := make([]int, 0, 64)
				for i := start; i < end; i++ {
					base := i * size
					if m.recordData[base+size-1] != 0 {
						continue
					}
					_, nameID := m.recordRefs(base + 16)
					if nameID >= uint32(len(derived.LowerOffs)) {
						continue
					}
					if tokenMatches[nameID] {
						local = append(local, i)
					}
				}
				parts[worker] = local
			}(worker, start, end)
		}
		wg.Wait()
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
	if p := idx.PackedRecords; p != nil && len(p.LowerOffs) == p.Len() {
		workers := min(runtime.GOMAXPROCS(0), max(1, p.Len()/250_000))
		parts := make([][]int, workers)
		var wg sync.WaitGroup
		for worker := 0; worker < workers; worker++ {
			start := worker * p.Len() / workers
			end := (worker + 1) * p.Len() / workers
			wg.Add(1)
			go func(worker, start, end int) {
				defer wg.Done()
				local := make([]int, 0, 64)
				for i := start; i < end; i++ {
					rec := p.At(i)
					if rec.Deleted || !strings.Contains(p.lowerNameAt(i), term) {
						continue
					}
					local = append(local, i)
				}
				parts[worker] = local
			}(worker, start, end)
		}
		wg.Wait()
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
	return nil, false
}

func (vol *serviceVolumeIndex) pathTermPosting(term string) []int {
	vol.termMu.Lock()
	if vol.pathTermCache == nil {
		vol.pathTermCache = make(map[string]postingCacheEntry)
	}
	if entry, ok := vol.pathTermCache[term]; ok {
		if vol.cacheStampValid(entry.gen) {
			vol.termMu.Unlock()
			return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
				id, ok := vol.idForFRN(rec.FRN)
				return ok && vol.index.compactPathContainsTerm(id, term)
			})
		}
	}
	vol.termMu.Unlock()

	seen := make(map[int]struct{}, 64)
	out := make([]int, 0, 64)
	for _, id := range vol.nameTermPosting(term) {
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if !strings.ContainsAny(term, `\/*?[]:`) && (len(vol.childOffsets) > 0 || vol.children != nil) {
		traversed := make(map[int]struct{}, 64)
		for _, rootID := range vol.pathTermRootIDs(term) {
			if rootID < 0 || rootID >= vol.index.compactRecordCount() {
				continue
			}
			root := vol.index.compactRecord(rootID)
			if root.Deleted || root.Mode&uint32(os.ModeDir) == 0 {
				continue
			}
			stack := []int{rootID}
			for len(stack) > 0 {
				last := len(stack) - 1
				id := stack[last]
				stack = stack[:last]
				if _, ok := traversed[id]; ok || id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				traversed[id] = struct{}{}
				rec := vol.index.compactRecord(id)
				if !rec.Deleted {
					if _, ok := seen[id]; !ok {
						seen[id] = struct{}{}
						out = append(out, id)
					}
				}
				for _, childID := range vol.childIDsForRecord(id) {
					stack = append(stack, int(childID))
				}
			}
		}
	} else if !strings.ContainsAny(term, `\/*?[]:`) {
		for _, id := range vol.scanPathTermPosting(term) {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Ints(out)
	vol.cachePathPosting(term, out)
	return out
}
