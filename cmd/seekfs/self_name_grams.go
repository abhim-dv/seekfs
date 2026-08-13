package main

import (
	"os"
	"sort"
	"strings"
)

// optionalSelfNameGramIndex builds the companion PNGC payload for grams that
// the selective PNGR writer omitted.  It is deliberately opt-in while the
// format is being validated; normal v9 generation remains byte-compatible
// until the new section is enabled by the compaction owner.
func optionalSelfNameGramIndex(idx *Index, selective *compressedTrigramIndex) *compressedTrigramIndex {
	if idx == nil || selective == nil || len(selective.omitted) == 0 {
		return nil
	}
	full := buildNameTrigramIndex(idx)
	out := &compressedTrigramIndex{
		counts:             make(map[uint32]int, len(selective.omitted)),
		gramCountsComplete: true,
		gramSize:           3,
		recordCount:        idx.compactRecordCount(),
		segments:           []trigramSegment{{start: 0, end: idx.compactRecordCount(), postings: make(map[uint32]compressedPosting)}},
	}
	for gram := range selective.omitted {
		ids := trigramPostingIDs(full, gram)
		if len(ids) == 0 {
			continue
		}
		encoded := encodeDeltaUvarint32(ids)
		out.segments[0].postings[gram] = compressedPosting{count: len(ids), data: encoded}
		out.counts[gram] = len(ids)
		out.postingBytes += len(encoded)
		out.segments[0].postingBytes += len(encoded)
	}
	if len(out.counts) == 0 {
		return nil
	}
	return out
}

func optionalSelfNameGramSection(idx *Index, selective *compressedTrigramIndex, ranks []uint32) []byte {
	if raw := strings.TrimSpace(os.Getenv("SEEKFS_V9_SELF_NAME_GRAMS")); raw != "" && !envBool("SEEKFS_V9_SELF_NAME_GRAMS") {
		return nil
	}
	return encodeGramPostingSection(optionalSelfNameGramIndex(idx, selective), ranks)
}

// completeSelfNameGramIterators combines the original selective PNGR source
// with PNGC.  A term is complete only when every required gram is stored in
// one of those sources, or PNGR's completeness metadata proves an exact zero.
// Missing/omitted metadata remains a safe decline to the sequential route.
func completeSelfNameGramIterators(idx *Index, term string) ([]postingBlockIterator, []int, bool, bool) {
	if idx == nil || idx.Derived.NameTrigrams == nil || len(term) < 3 {
		return nil, nil, false, false
	}
	base := idx.Derived.NameTrigrams
	extra := idx.Derived.SelfNameTrigrams
	grams := uniqueFixedGramKeysFoldASCII(strings.ToLower(term), 3)
	if len(grams) == 0 {
		return nil, nil, false, false
	}
	type gramSource struct {
		gram  uint32
		count int
		it    postingBlockIterator
	}
	sources := make([]gramSource, 0, len(grams))
	for _, gram := range grams {
		found := false
		if base != nil && base.mappedGrams != nil && !base.isOmittedGram(gram) {
			if it, count, ok := base.mappedGrams.gramPostingIterator(gram); ok {
				sources = append(sources, gramSource{gram: gram, count: count, it: it})
				found = true
			}
		}
		if !found && extra != nil && extra.mappedGrams != nil {
			if it, count, ok := extra.mappedGrams.gramPostingIterator(gram); ok {
				sources = append(sources, gramSource{gram: gram, count: count, it: it})
				found = true
			}
		}
		if found {
			continue
		}
		if extra != nil && extra.gramUnionComplete && extra.gramCountsComplete && extra.countForGram(gram) == 0 {
			return nil, nil, true, true
		}
		if base != nil && base.gramCountsComplete && base.countForGram(gram) == 0 {
			return nil, nil, true, true
		}
		return nil, nil, false, false
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].count < sources[j].count })
	its := make([]postingBlockIterator, len(sources))
	counts := make([]int, len(sources))
	for i := range sources {
		its[i], counts[i] = sources[i].it, sources[i].count
	}
	return its, counts, false, true
}

type selfNameGramCursor struct {
	it    postingBlockIterator
	block []uint32
	pos   int
	done  bool
}

func (c *selfNameGramCursor) next() (uint32, bool) {
	for {
		if c.pos < len(c.block) {
			id := c.block[c.pos]
			c.pos++
			return id, true
		}
		if c.done {
			return 0, false
		}
		block, _, ok := c.it.nextBlock()
		if !ok {
			c.done = true
			return 0, false
		}
		c.block, c.pos = block, 0
	}
}

// contains advances monotonically, retaining only one decoded block per gram.
func (c *selfNameGramCursor) contains(target uint32) bool {
	for {
		if c.pos < len(c.block) {
			if c.block[len(c.block)-1] < target {
				c.pos = len(c.block)
				continue
			}
			i := sort.Search(len(c.block), func(i int) bool { return c.block[i] >= target })
			c.pos = i
			return i < len(c.block) && c.block[i] == target
		}
		block, _, ok := c.it.nextBlock()
		if !ok {
			c.done = true
			return false
		}
		c.block, c.pos = block, 0
	}
}

// countMappedComponentSelfNameGramHits performs lazy posting intersection and
// verifies only candidates from the smallest required gram.  It never builds
// an ID slice for the intersection.
func (vol *serviceVolumeIndex) countMappedComponentSelfNameGramHits(term string, coverage mappedComponentCoverage, hidden func(int) bool, pq parsedQuery) (count, visited int, exactZero, ok bool) {
	if vol == nil || vol.index == nil {
		return 0, 0, false, false
	}
	its, _, exactZero, complete := completeSelfNameGramIterators(vol.index, term)
	if !complete {
		return 0, 0, false, false
	}
	if exactZero {
		return 0, 0, true, true
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
			return 0, visited, false, false
		}
	}
	for {
		if visited&1023 == 0 && queryCanceled(pq) {
			return 0, visited, false, false
		}
		id32, exists := cursors[0].next()
		if !exists {
			break
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
		visited++
		id := int(id32)
		if id < 0 || id >= vol.index.compactRecordCount() || coverage.containsInterval(vol, id) {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) != 0 || hidden != nil && hidden(id) {
			continue
		}
		if strings.Contains(vol.index.compactLowerNameAt(id), strings.ToLower(term)) {
			count++
		}
	}
	return count, visited, false, true
}

// completeSelfNameGramTop returns self-hit candidates in persisted rank order.
// It is used when the component source has no descendant interval; the
// interval route remains responsible for merging directory coverage.
func (vol *serviceVolumeIndex) completeSelfNameGramTop(term string, limit int, pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || limit <= 0 {
		return nil, false
	}
	its, _, exactZero, complete := completeSelfNameGramIterators(vol.index, term)
	if !complete {
		return nil, false
	}
	if exactZero {
		return []int{}, true
	}
	refs, ok := its[0].rankOrderedBlockRefsForSort(pq.SortColumn)
	if !ok {
		return nil, false
	}
	prefetched, ranges, pages, stopped := prefetchPostingBlockRefs(its[0], refs, int64(queryPostingPrefetchBytes()), func() bool { return queryCanceled(pq) })
	if pq.Trace != nil {
		pq.Trace.PostingPrefetchBytes += prefetched
		pq.Trace.PostingPrefetchRanges += ranges
		pq.Trace.PostingPrefetchPages += pages
	}
	if stopped {
		return nil, false
	}
	cursors := make([]selfNameGramCursor, len(its))
	for i := 1; i < len(its); i++ {
		cursors[i].it = its[i]
	}
	termLower := strings.ToLower(term)
	out := make([]int, 0, limit)
	for _, ref := range refs {
		block, _, ok := its[0].blockAt(ref.index)
		if !ok {
			return nil, false
		}
		for _, id32 := range block {
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
			rec := vol.index.compactRecord(id)
			if rec.Deleted || rec.Mode&uint32(os.ModeDir) != 0 || !strings.Contains(vol.index.compactLowerNameAt(id), termLower) {
				continue
			}
			out = append(out, id)
			if len(out) >= limit {
				return out, true
			}
		}
	}
	return out, true
}
