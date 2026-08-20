package main

import (
	"container/heap"
	"sort"
	"strings"
)

func (vol *serviceVolumeIndex) nameGramPosting(gram uint32) (index *compressedTrigramIndex, count int, stored bool, state string, exactEmpty bool) {
	if vol == nil || vol.index == nil {
		return nil, 0, false, "missing-section", false
	}
	base := vol.nameTrigramIndex()
	extra := vol.index.Derived.SelfNameTrigrams
	if base != nil && base.hasStoredPosting(gram) {
		return base, base.countForGram(gram), true, "present", false
	}
	if extra != nil && extra.hasStoredPosting(gram) {
		return extra, extra.countForGram(gram), true, "present", false
	}
	if extra != nil && extra.gramUnionComplete && extra.gramCountsComplete && extra.countForGram(gram) == 0 {
		return extra, 0, false, "exact-empty", true
	}
	if base != nil && base.gramCountsComplete && base.countForGram(gram) == 0 {
		return base, 0, false, "exact-empty", true
	}
	if base != nil && base.isOmittedGram(gram) {
		return base, base.countForGram(gram), false, "omitted-common", false
	}
	if base != nil && base.countForGram(gram) > 0 {
		return base, base.countForGram(gram), false, "omitted-common", false
	}
	return nil, 0, false, "missing-section", false
}

// nameGramUnionUsesExtra reports whether at least one required gram comes
// from the optional common-posting section.  It is metadata-only; no posting
// block is decoded here.
func (vol *serviceVolumeIndex) nameGramUnionUsesExtra(term string) bool {
	if vol == nil || vol.index == nil {
		return false
	}
	base := vol.nameTrigramIndex()
	extra := vol.index.Derived.SelfNameTrigrams
	if extra == nil || extra.mappedGrams == nil {
		return false
	}
	for _, gram := range uniqueFixedGramKeysFoldASCII(strings.ToLower(term), 3) {
		if base != nil && base.hasStoredPosting(gram) {
			continue
		}
		if extra.hasStoredPosting(gram) {
			return true
		}
	}
	return false
}

// completeFilenameTopPosting is the broad complete-source driver.  It walks
// the requested persisted order and performs bounded posting membership
// checks, so a common gram never becomes a full ID slice.  The final folded
// name check keeps the posting source conservative.
func (vol *serviceVolumeIndex) completeFilenameTopPosting(term string, limit int, pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || limit <= 0 || pq.CountOnly || len(vol.recentIDs) > 0 {
		return nil, false
	}
	if pq.SortColumn == "" {
		if out, ok := vol.completeFilenameRankedPosting(term, limit, pq); ok {
			return out, true
		}
	}
	its, _, exactZero, complete := completeSelfNameGramIterators(vol.index, term)
	if !complete {
		return nil, false
	}
	if exactZero {
		if pq.Trace != nil {
			pq.Trace.FilenameDriver = "exact-empty"
			pq.Trace.FilenameRequiredGrams = len(uniqueFixedGramKeysFoldASCII(strings.ToLower(term), 3))
			pq.Trace.setSource("exact-empty", 0)
			pq.Trace.setComplete(true)
		}
		return []int{}, true
	}
	order := vol.orderForQuery(pq)
	recordCount := vol.index.compactRecordCount()
	if len(its) == 0 || len(order) < recordCount {
		return nil, false
	}
	out := make([]int, 0, min(limit, 64))
	verified, decoded := 0, 0
	decodedBlocks := make(map[[2]int]struct{}, len(its))
	walked := 0
	for _, id32 := range order {
		walked++
		if walked > serviceCompleteFilenameOrderWalkMaxRecords {
			// A broad common gram must never force a walk of the entire
			// persisted order for a small top-N page.  Declining keeps the
			// result complete: the caller falls through to another exhaustive
			// source instead of returning a partial page.
			return nil, false
		}
		if queryCanceled(pq) {
			return nil, false
		}
		matched := true
		for i := range its {
			found, valid, blockIndex, didDecode := its[i].containsID(id32)
			if didDecode {
				key := [2]int{i, blockIndex}
				if _, seen := decodedBlocks[key]; !seen {
					decodedBlocks[key] = struct{}{}
					decoded++
				}
			}
			if !valid {
				return nil, false
			}
			if !found {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		id := int(id32)
		if id < 0 || id >= recordCount {
			continue
		}
		verified++
		if !vol.nameTrigramCandidateMatches(id, term) {
			continue
		}
		out = append(out, id)
		if len(out) >= limit {
			break
		}
	}
	if pq.Trace != nil {
		if vol.nameGramUnionUsesExtra(term) {
			pq.Trace.FilenameDriver = "persisted-order-pngc"
			pq.Trace.setSource("filename-pngc-persisted-order", len(out))
		} else {
			pq.Trace.FilenameDriver = "persisted-order-pngr"
			pq.Trace.setSource("filename-pngr-persisted-order", len(out))
		}
		pq.Trace.FilenameRequiredGrams = len(its)
		pq.Trace.FilenameRecordsVerified = verified
		pq.Trace.addPostingBlocks(decoded, 0)
		pq.Trace.setComplete(true)
	}
	return out, true
}

// completeFilenameRankedPosting uses the primary gram's persisted rank
// bounds for the default/name order.  It decodes posting blocks in rank order
// and stops once the next block cannot beat the bounded heap, avoiding a scan
// of the entire persisted order for common terms.
func (vol *serviceVolumeIndex) completeFilenameRankedPosting(term string, limit int, pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || limit <= 0 || len(vol.recentIDs) > 0 || pq.SortColumn != "" {
		return nil, false
	}
	its, counts, exactZero, complete := completeSelfNameGramIterators(vol.index, term)
	if !complete {
		return nil, false
	}
	if exactZero {
		if pq.Trace != nil {
			pq.Trace.FilenameDriver = "exact-empty"
			pq.Trace.FilenameRequiredGrams = len(uniqueFixedGramKeysFoldASCII(strings.ToLower(term), 3))
			pq.Trace.setSource("exact-empty", 0)
			pq.Trace.setComplete(true)
		}
		return []int{}, true
	}
	if len(its) == 0 || len(counts) != len(its) {
		return nil, false
	}
	refs, canSkip := its[0].rankOrderedBlockRefsForSort("")
	if len(refs) == 0 {
		return nil, false
	}
	ranks := vol.rankForQuery(pq)
	h := make(extRankMaxHeap, 0, limit)
	decoded, skipped, verified := 0, 0, 0
	decodedBlocks := make(map[[2]int]struct{}, len(its))
	prefetchBytes, prefetchRanges, prefetchPages, prefetchStopped := prefetchPostingBlockRefs(its[0], refs, queryPostingPrefetchBytes(), func() bool { return queryCanceled(pq) })
	if prefetchStopped {
		return nil, false
	}
	if pq.Trace != nil {
		pq.Trace.PostingPrefetchBytes += prefetchBytes
		pq.Trace.PostingPrefetchRanges += prefetchRanges
		pq.Trace.PostingPrefetchPages += prefetchPages
	}
	add := func(id32 uint32) {
		item := extRankItem{id: id32, rank: extRankOf(id32, ranks)}
		if len(h) < limit {
			heap.Push(&h, item)
		} else if extRankLess(item, h[0]) {
			h[0] = item
			heap.Fix(&h, 0)
		}
	}
	for blockPos, ref := range refs {
		if canSkip && len(h) >= limit && ref.meta.minRank > h[0].rank {
			skipped = len(refs) - blockPos
			break
		}
		ids, _, ok := its[0].blockAt(ref.index)
		if !ok {
			return nil, false
		}
		if _, seen := decodedBlocks[[2]int{0, ref.index}]; !seen {
			decodedBlocks[[2]int{0, ref.index}] = struct{}{}
			decoded++
		}
		for _, id32 := range ids {
			matched := true
			for i := 1; i < len(its); i++ {
				found, valid, blockIndex, didDecode := its[i].containsID(id32)
				if didDecode {
					key := [2]int{i, blockIndex}
					if _, seen := decodedBlocks[key]; !seen {
						decodedBlocks[key] = struct{}{}
						decoded++
					}
				}
				if !valid {
					return nil, false
				}
				if !found {
					matched = false
					break
				}
			}
			if !matched {
				continue
			}
			id := int(id32)
			if id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			verified++
			if vol.nameTrigramCandidateMatches(id, term) {
				add(id32)
			}
		}
	}
	out := make([]int, len(h))
	for i := range h {
		out[i] = int(h[i].id)
	}
	sort.Slice(out, func(i, j int) bool {
		return extRankLess(extRankItem{id: uint32(out[i]), rank: extRankOf(uint32(out[i]), ranks)}, extRankItem{id: uint32(out[j]), rank: extRankOf(uint32(out[j]), ranks)})
	})
	if pq.Trace != nil {
		if vol.nameGramUnionUsesExtra(term) {
			pq.Trace.FilenameDriver = "ranked-posting-pngc"
			pq.Trace.setSource("filename-pngc-ranked-posting", len(out))
		} else {
			pq.Trace.FilenameDriver = "ranked-posting-pngr"
			pq.Trace.setSource("filename-pngr-ranked-posting", len(out))
		}
		pq.Trace.FilenameRequiredGrams = len(its)
		pq.Trace.FilenamePostingHint = counts[0]
		pq.Trace.FilenameRecordsVerified = verified
		pq.Trace.addPostingBlocks(decoded, skipped)
		pq.Trace.setComplete(true)
	}
	return out, true
}

// completeMultiTermNameGramCandidates intersects PNGR/PNGC postings across
// all query terms.  It picks the term whose smallest complete gram has the
// fewest postings as the driver, materializes those IDs, then probes the
// remaining terms' gram iterators with bounded membership checks.  Unlike the
// single-term fast paths, this handles multi-word name queries such as
// "aker log" whose grams are all common (>25k) and thus live in PNGC instead
// of the selective PNGR section.
func (vol *serviceVolumeIndex) completeMultiTermNameGramCandidates(terms []string, maxIDs int, pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || len(terms) < 2 || maxIDs <= 0 {
		return nil, false
	}
	// Collect complete gram iterators for every term.  A term is complete
	// only when each required gram is stored in PNGR or PNGC, or the
	// completeness metadata proves an exact empty.
	type termIters struct {
		term   string
		its    []postingBlockIterator
		counts []int
	}
	perTerm := make([]termIters, 0, len(terms))
	exactEmpty := false
	for _, term := range terms {
		if len(term) < 3 {
			return nil, false
		}
		its, counts, exactZero, complete := completeSelfNameGramIterators(vol.index, term)
		if !complete {
			return nil, false
		}
		if exactZero {
			exactEmpty = true
			continue
		}
		if len(its) == 0 || len(counts) != len(its) || counts[0] <= 0 {
			return nil, false
		}
		perTerm = append(perTerm, termIters{term: term, its: its, counts: counts})
	}
	if len(perTerm) == 0 {
		if exactEmpty {
			if pq.Trace != nil {
				pq.Trace.FilenameDriver = "exact-empty"
				pq.Trace.setSource("exact-empty", 0)
				pq.Trace.setComplete(true)
			}
			return []int{}, true
		}
		return nil, false
	}
	// Driver term = smallest first-gram posting count.  The driver's posting
	// count may exceed maxIDs for common grams in PNGC; the cap applies to the
	// intersected result, not the intermediate posting, so we do not reject
	// here.
	sort.Slice(perTerm, func(i, j int) bool { return perTerm[i].counts[0] < perTerm[j].counts[0] })
	driver := perTerm[0]
	ids := materializePostingBlockIterator(driver.its[0], driver.counts[0])
	if ids == nil {
		return nil, false
	}
	// Intersect the driver's remaining grams for its own term.
	for i := 1; i < len(driver.its) && len(ids) > 0; i++ {
		ids = intersectSortedUint32sWithPostingIterator(ids, driver.its[i])
	}
	// Probe every other term's grams.
	for t := 1; t < len(perTerm) && len(ids) > 0; t++ {
		pt := perTerm[t]
		for g := 0; g < len(pt.its) && len(ids) > 0; g++ {
			ids = intersectSortedUint32sWithPostingIterator(ids, pt.its[g])
		}
	}
	if len(ids) == 0 {
		return []int{}, true
	}
	// The intersected result must fit within maxIDs; intermediate posting
	// counts for common PNGC grams are expected to exceed it.
	if len(ids) > maxIDs {
		if pq.Trace != nil {
			pq.Trace.setDecline("name-trigram:multi-term-result-too-large")
		}
		return nil, false
	}
	// Merge recent (overlay) record IDs into the candidate pool.  The final
	// fold verifies every term substring, so recent additions are admitted
	// exactly when they match the full query.
	if len(vol.recentIDs) > 0 {
		seen := make(map[int]struct{}, len(ids)+len(vol.recentIDs))
		for _, id := range ids {
			seen[int(id)] = struct{}{}
		}
		for id := range vol.recentIDs {
			seen[id] = struct{}{}
		}
		ids = ids[:0]
		for id := range seen {
			ids = append(ids, uint32(id))
		}
		sortUint32s(ids)
	}
	// Final fold: each candidate must contain every query term as a
	// substring of its name.
	ints := uint32sToInts(ids)
	if len(ints) < serviceTrigramParallelVerifyMinIDs {
		out := make([]int, 0, len(ints))
		for _, id := range ints {
			matched := true
			for _, term := range terms {
				if !vol.nameTrigramCandidateMatches(id, term) {
					matched = false
					break
				}
			}
			if matched {
				out = append(out, id)
			}
		}
		out = uniqueSortedInts(out)
		if pq.Trace != nil {
			usedPNGC := false
			for _, pt := range perTerm {
				if vol.nameGramUnionUsesExtra(pt.term) {
					usedPNGC = true
					break
				}
			}
			if usedPNGC {
				pq.Trace.FilenameDriver = "posting-intersection-pngc-multi"
				pq.Trace.setSource("filename-pngc-multi-term", len(out))
			} else {
				pq.Trace.FilenameDriver = "posting-intersection-pngr-multi"
				pq.Trace.setSource("filename-pngr-multi-term", len(out))
			}
			pq.Trace.FilenameRequiredGrams = len(driver.its)
			pq.Trace.setComplete(true)
		}
		return out, true
	}
	// Parallel fold for large candidate sets.
	verifyMultiTerm := func(id int) bool {
		for _, term := range terms {
			if !vol.nameTrigramCandidateMatches(id, term) {
				return false
			}
		}
		return true
	}
	verified := vol.verifyMultiTermCandidateIDs(ints, verifyMultiTerm)
	if pq.Trace != nil {
		usedPNGC := false
		for _, pt := range perTerm {
			if vol.nameGramUnionUsesExtra(pt.term) {
				usedPNGC = true
				break
			}
		}
		if usedPNGC {
			pq.Trace.FilenameDriver = "posting-intersection-pngc-multi"
			pq.Trace.setSource("filename-pngc-multi-term", len(verified))
		} else {
			pq.Trace.FilenameDriver = "posting-intersection-pngr-multi"
			pq.Trace.setSource("filename-pngr-multi-term", len(verified))
		}
		pq.Trace.FilenameRequiredGrams = len(driver.its)
		pq.Trace.setComplete(true)
	}
	return verified, true
}

// completeFilenameCountPosting intersects the smallest complete gram in
// record-ID order and probes the other grams with bounded membership checks.
// It retains only one decoded posting block at a time.
func (vol *serviceVolumeIndex) completeFilenameCountPosting(term string, pq parsedQuery) (int, bool) {
	if vol == nil || vol.index == nil || len(vol.recentIDs) > 0 {
		return 0, false
	}
	its, counts, exactZero, complete := completeSelfNameGramIterators(vol.index, term)
	if !complete {
		return 0, false
	}
	if exactZero {
		if pq.Trace != nil {
			pq.Trace.FilenameDriver = "exact-empty"
			pq.Trace.FilenameRequiredGrams = len(uniqueFixedGramKeysFoldASCII(strings.ToLower(term), 3))
			pq.Trace.setSource("exact-empty", 0)
			pq.Trace.setComplete(true)
		}
		return 0, true
	}
	if len(its) == 0 {
		return 0, false
	}
	if len(counts) != len(its) {
		return 0, false
	}
	cursors := make([]selfNameGramCursor, len(its))
	for i := range its {
		cursors[i].it = its[i]
	}
	remainingPrefetch := int64(queryPostingPrefetchBytes())
	for i := range its {
		if remainingPrefetch <= 0 {
			break
		}
		refs := its[i].rankOrderedBlockRefs()
		prefetched, ranges, pages, stopped := prefetchPostingBlockRefs(its[i], refs, remainingPrefetch, func() bool { return queryCanceled(pq) })
		if pq.Trace != nil {
			pq.Trace.PostingPrefetchBytes += prefetched
			pq.Trace.PostingPrefetchRanges += ranges
			pq.Trace.PostingPrefetchPages += pages
		}
		remainingPrefetch -= int64(prefetched)
		if stopped {
			return 0, false
		}
	}
	count, verified, decoded := 0, 0, 0
	for {
		id32, ok := cursors[0].next()
		if !ok {
			break
		}
		if verified&1023 == 0 && queryCanceled(pq) {
			return 0, false
		}
		matched := true
		for i := 1; i < len(cursors); i++ {
			if !cursors[i].contains(id32) {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		id := int(id32)
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		verified++
		if vol.nameTrigramCandidateMatches(id, term) {
			count++
		}
	}
	for i := range cursors {
		decoded += cursors[i].it.next
	}
	if pq.Trace != nil {
		if vol.nameGramUnionUsesExtra(term) {
			pq.Trace.FilenameDriver = "posting-intersection-pngc"
			pq.Trace.setSource("count-fast-pngc", count)
		} else {
			pq.Trace.FilenameDriver = "posting-intersection-pngr"
			pq.Trace.setSource("count-fast-pngr", count)
		}
		pq.Trace.FilenameRequiredGrams = len(its)
		pq.Trace.FilenamePostingHint = counts[0]
		pq.Trace.FilenameRecordsVerified = verified
		pq.Trace.addPostingBlocks(decoded, 0)
		pq.Trace.setComplete(true)
	}
	return count, true
}
