package main

import (
	"container/heap"
	"runtime"
	"sort"
	"sync"
)

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
