package main

import (
	"encoding/binary"
	"runtime"
	"sort"
	"strings"
	"sync"
)

const trigramSegmentTargetRecords = 500_000
const trigramBuildMaxWorkers = 4
const trigramStoredPostingMaxCount = 25_000

type trigramSegment struct {
	start        int
	end          int
	postings     map[uint32]compressedPosting
	postingBytes int
}

type compressedPosting struct {
	count int
	data  []byte
}

type compressedTrigramIndex struct {
	segments     []trigramSegment
	counts       map[uint32]int
	recordCount  int
	postingBytes int
}

func buildNameTrigramIndex(idx *Index) *compressedTrigramIndex {
	return buildCompactTrigramIndex(idx, func(id int, cache map[int]string) string {
		return idx.compactLowerNameAt(id)
	})
}

func buildPathTrigramIndex(idx *Index) *compressedTrigramIndex {
	return buildCompactTrigramIndex(idx, func(id int, cache map[int]string) string {
		return strings.ToLower(idx.reconstructCompactPathCached(id, cache))
	})
}

func buildCompactTrigramIndex(idx *Index, text func(id int, cache map[int]string) string) *compressedTrigramIndex {
	recordCount := 0
	if idx != nil {
		recordCount = idx.compactRecordCount()
	}
	ti := &compressedTrigramIndex{
		counts:      make(map[uint32]int),
		recordCount: recordCount,
	}
	if idx == nil || recordCount == 0 {
		return ti
	}

	chunks := trigramBuildChunks(recordCount)
	workers := min(len(chunks), min(trigramBuildMaxWorkers, max(1, runtime.GOMAXPROCS(0))))
	results := make([]trigramSegment, len(chunks))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunkID := range jobs {
				start, end := chunks[chunkID][0], chunks[chunkID][1]
				results[chunkID] = buildCompactTrigramSegment(idx, start, end, text)
			}
		}()
	}
	for chunkID := range chunks {
		jobs <- chunkID
	}
	close(jobs)
	wg.Wait()

	ti.segments = make([]trigramSegment, 0, len(results))
	for _, segment := range results {
		if segment.end <= segment.start {
			continue
		}
		ti.segments = append(ti.segments, segment)
		ti.postingBytes += segment.postingBytes
		for gram, posting := range segment.postings {
			ti.counts[gram] += posting.count
		}
	}
	return ti
}

func trigramBuildChunks(recordCount int) [][2]int {
	if recordCount <= 0 {
		return nil
	}
	chunkCount := (recordCount + trigramSegmentTargetRecords - 1) / trigramSegmentTargetRecords
	chunks := make([][2]int, 0, chunkCount)
	for start := 0; start < recordCount; start += trigramSegmentTargetRecords {
		end := min(recordCount, start+trigramSegmentTargetRecords)
		chunks = append(chunks, [2]int{start, end})
	}
	return chunks
}

func buildCompactTrigramSegment(idx *Index, start, end int, text func(id int, cache map[int]string) string) trigramSegment {
	segment := trigramSegment{
		start:    start,
		end:      end,
		postings: make(map[uint32]compressedPosting),
	}
	raw := make(map[uint32][]uint32)
	cache := make(map[int]string)
	for id := start; id < end; id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		for _, gram := range uniqueTrigramKeys(text(id, cache)) {
			raw[gram] = append(raw[gram], uint32(id))
		}
	}
	for gram, ids := range raw {
		encoded := encodeDeltaUvarint32(ids)
		segment.postings[gram] = compressedPosting{count: len(ids), data: encoded}
		segment.postingBytes += len(encoded)
	}
	return segment
}

func (ti *compressedTrigramIndex) candidateIDs(term string) ([]int, bool) {
	term = strings.ToLower(term)
	grams := uniqueTrigramKeys(term)
	if len(grams) == 0 || ti == nil {
		return nil, false
	}
	sort.Slice(grams, func(i, j int) bool {
		return ti.counts[grams[i]] < ti.counts[grams[j]]
	})
	if !ti.hasAllGrams(grams) {
		return nil, true
	}
	out := make([]int, 0, ti.counts[grams[0]])
	for _, segment := range ti.segments {
		ids := ti.segmentCandidateIDs(segment, grams)
		out = append(out, uint32sToInts(ids)...)
	}
	return out, true
}

func (ti *compressedTrigramIndex) selectiveCandidateIDs(term string, maxIDs int) ([]int, bool, bool) {
	term = strings.ToLower(term)
	grams := uniqueTrigramKeys(term)
	if len(grams) == 0 || ti == nil {
		return nil, false, false
	}
	missing := false
	filtered := grams[:0]
	for _, gram := range grams {
		count, ok := ti.counts[gram]
		if !ok || count == 0 {
			missing = true
			continue
		}
		if maxIDs <= 0 || count <= maxIDs {
			filtered = append(filtered, gram)
		}
	}
	if missing {
		return nil, true, true
	}
	if len(filtered) == 0 {
		return nil, false, false
	}
	sort.Slice(filtered, func(i, j int) bool {
		return ti.counts[filtered[i]] < ti.counts[filtered[j]]
	})
	ids := ti.idsForGram(filtered[0])
	return uint32sToInts(ids), true, false
}

func (ti *compressedTrigramIndex) selectiveIntersectCandidateIDs(term string, maxIDs int) ([]int, bool, bool) {
	term = strings.ToLower(term)
	grams := uniqueTrigramKeys(term)
	if len(grams) == 0 || ti == nil {
		return nil, false, false
	}
	for _, gram := range grams {
		if ti.counts[gram] == 0 {
			return nil, true, true
		}
	}
	stored := grams[:0]
	for _, gram := range grams {
		if ti.hasStoredPosting(gram) {
			stored = append(stored, gram)
		}
	}
	if len(stored) == 0 {
		return nil, false, false
	}
	sort.Slice(stored, func(i, j int) bool {
		return ti.counts[stored[i]] < ti.counts[stored[j]]
	})
	if maxIDs > 0 && ti.counts[stored[0]] > maxIDs {
		return nil, false, false
	}
	out := make([]int, 0, ti.counts[stored[0]])
	for _, segment := range ti.segments {
		ids := ti.segmentCandidateIDs(segment, stored)
		if len(ids) == 0 {
			continue
		}
		out = append(out, uint32sToInts(ids)...)
		if maxIDs > 0 && len(out) > maxIDs {
			return nil, false, false
		}
	}
	return out, true, false
}

func (ti *compressedTrigramIndex) hasStoredPosting(gram uint32) bool {
	if ti == nil {
		return false
	}
	for _, segment := range ti.segments {
		if segment.postings[gram].count > 0 {
			return true
		}
	}
	return false
}

func (ti *compressedTrigramIndex) postingCount(term string) (int, bool) {
	grams := uniqueTrigramKeys(strings.ToLower(term))
	if len(grams) == 0 || ti == nil {
		return 0, false
	}
	minCount := -1
	for _, gram := range grams {
		count, ok := ti.counts[gram]
		if !ok {
			return 0, true
		}
		if minCount < 0 || count < minCount {
			minCount = count
		}
	}
	return minCount, true
}

func (ti *compressedTrigramIndex) keyCount() int {
	if ti == nil {
		return 0
	}
	return len(ti.counts)
}

func (ti *compressedTrigramIndex) dropCommonPostings(maxCount int) {
	if ti == nil || maxCount <= 0 {
		return
	}
	for gram, count := range ti.counts {
		if count <= maxCount {
			continue
		}
		for i := range ti.segments {
			posting := ti.segments[i].postings[gram]
			if posting.count == 0 {
				continue
			}
			ti.postingBytes -= len(posting.data)
			ti.segments[i].postingBytes -= len(posting.data)
			delete(ti.segments[i].postings, gram)
		}
	}
}

func (ti *compressedTrigramIndex) hasAllGrams(grams []uint32) bool {
	for _, gram := range grams {
		if ti.counts[gram] == 0 {
			return false
		}
	}
	return true
}

func (ti *compressedTrigramIndex) segmentCandidateIDs(segment trigramSegment, grams []uint32) []uint32 {
	var candidates []uint32
	for i, gram := range grams {
		posting := segment.postings[gram]
		if posting.count == 0 {
			return nil
		}
		ids := decodeDeltaUvarint32(posting.data, posting.count)
		if i == 0 {
			candidates = ids
			continue
		}
		candidates = intersectSortedUint32s(candidates, ids)
		if len(candidates) == 0 {
			return nil
		}
	}
	return candidates
}

func (ti *compressedTrigramIndex) idsForGram(gram uint32) []uint32 {
	total := ti.counts[gram]
	if total == 0 {
		return nil
	}
	out := make([]uint32, 0, total)
	for _, segment := range ti.segments {
		posting := segment.postings[gram]
		if posting.count == 0 {
			continue
		}
		out = append(out, decodeDeltaUvarint32(posting.data, posting.count)...)
	}
	return out
}

func uniqueTrigramKeys(s string) []uint32 {
	if len(s) < 3 {
		return nil
	}
	seen := make(map[uint32]struct{}, len(s)-2)
	out := make([]uint32, 0, len(s)-2)
	for i := 0; i+3 <= len(s); i++ {
		key := trigramKey(s[i : i+3])
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

func trigramKey(s string) uint32 {
	if len(s) != 3 {
		return 0
	}
	return uint32(s[0])<<16 | uint32(s[1])<<8 | uint32(s[2])
}

func encodeDeltaUvarint32(ids []uint32) []byte {
	if len(ids) == 0 {
		return nil
	}
	out := make([]byte, 0, len(ids)*2)
	var prev uint32
	var buf [binary.MaxVarintLen32]byte
	for _, id := range ids {
		delta := id - prev
		n := binary.PutUvarint(buf[:], uint64(delta))
		out = append(out, buf[:n]...)
		prev = id
	}
	return out
}

func decodeDeltaUvarint32(encoded []byte, count int) []uint32 {
	out := make([]uint32, 0, count)
	var prev uint32
	for len(encoded) > 0 {
		value, n := binary.Uvarint(encoded)
		if n <= 0 {
			return out
		}
		id := prev + uint32(value)
		out = append(out, id)
		prev = id
		encoded = encoded[n:]
	}
	return out
}
