package main

import (
	"encoding/binary"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
)

const trigramSegmentTargetRecords = 500_000
const trigramBuildMaxWorkers = 4
const trigramStoredPostingMaxCount = 25_000
const trigramLowMemoryStoredPostingMaxCount = 25_000

type trigramSegment struct {
	start        int
	end          int
	postings     map[uint32]compressedPosting
	grams        []uint32
	postingList  []compressedPosting
	flatPostings []flatPosting
	postingBytes int
}

type compressedPosting struct {
	count  int
	data   []byte
	offset uint64
	size   uint32
}

type flatPosting struct {
	count  int
	offset uint64
	size   uint32
}

type compressedTrigramIndex struct {
	segments     []trigramSegment
	counts       map[uint32]int
	countKeys    []uint32
	countValues  []int
	postingBlob  *mappedIndexFile
	postingPath  string
	gramSize     int
	recordCount  int
	postingBytes int
}

func buildNameTrigramIndex(idx *Index) *compressedTrigramIndex {
	return buildCompactTrigramIndex(idx, func(id int, cache map[int]string) string {
		return idx.compactLowerNameAt(id)
	})
}

func buildSelectiveNameTrigramIndex(idx *Index, maxPostingCount int) *compressedTrigramIndex {
	return buildSelectiveCompactNameTrigramIndex(idx, maxPostingCount)
}

func buildSelectiveNameQuadgramIndex(idx *Index, maxPostingCount int) *compressedTrigramIndex {
	return buildSelectiveCompactNameGramIndex(idx, 4, maxPostingCount)
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
		gramSize:    3,
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

func buildSelectiveCompactTrigramIndex(idx *Index, maxPostingCount int, text func(id int, cache map[int]string) string) *compressedTrigramIndex {
	recordCount := 0
	if idx != nil {
		recordCount = idx.compactRecordCount()
	}
	ti := &compressedTrigramIndex{
		counts:      make(map[uint32]int),
		gramSize:    3,
		recordCount: recordCount,
	}
	if idx == nil || recordCount == 0 || maxPostingCount <= 0 {
		return ti
	}
	chunks := trigramBuildChunks(recordCount)
	workers := min(len(chunks), min(trigramBuildMaxWorkers, max(1, runtime.GOMAXPROCS(0))))
	localCounts := make([]map[uint32]int, len(chunks))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunkID := range jobs {
				start, end := chunks[chunkID][0], chunks[chunkID][1]
				localCounts[chunkID] = countCompactTrigramSegment(idx, start, end, text)
			}
		}()
	}
	for chunkID := range chunks {
		jobs <- chunkID
	}
	close(jobs)
	wg.Wait()
	for _, counts := range localCounts {
		for gram, count := range counts {
			ti.counts[gram] += count
		}
	}
	results := make([]trigramSegment, len(chunks))
	jobs = make(chan int)
	wg = sync.WaitGroup{}
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunkID := range jobs {
				start, end := chunks[chunkID][0], chunks[chunkID][1]
				results[chunkID] = buildSelectiveCompactTrigramSegment(idx, start, end, text, ti.counts, maxPostingCount)
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
	}
	return ti
}

func buildSelectiveCompactNameTrigramIndex(idx *Index, maxPostingCount int) *compressedTrigramIndex {
	return buildSelectiveCompactNameGramIndex(idx, 3, maxPostingCount)
}

func buildSelectiveCompactNameGramIndex(idx *Index, gramSize int, maxPostingCount int) *compressedTrigramIndex {
	recordCount := 0
	if idx != nil {
		recordCount = idx.compactRecordCount()
	}
	ti := &compressedTrigramIndex{
		counts:      make(map[uint32]int),
		gramSize:    gramSize,
		recordCount: recordCount,
	}
	if idx == nil || recordCount == 0 || maxPostingCount <= 0 {
		return ti
	}
	chunks := trigramBuildChunks(recordCount)
	if serviceLowMemoryMode() {
		for _, chunk := range chunks {
			counts := countCompactNameGramSegment(idx, chunk[0], chunk[1], gramSize)
			for gram, count := range counts {
				ti.counts[gram] += count
			}
		}
		debug.FreeOSMemory()
		ti.segments = make([]trigramSegment, 0, len(chunks))
		for _, chunk := range chunks {
			segment := buildSelectiveCompactNameGramSegment(idx, chunk[0], chunk[1], gramSize, ti.counts, maxPostingCount)
			if segment.end <= segment.start {
				continue
			}
			ti.segments = append(ti.segments, segment)
			ti.postingBytes += segment.postingBytes
		}
		ti.compactForLowMemory()
		return ti
	}
	workers := min(len(chunks), min(trigramBuildMaxWorkers, max(1, runtime.GOMAXPROCS(0))))
	localCounts := make([]map[uint32]int, len(chunks))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunkID := range jobs {
				start, end := chunks[chunkID][0], chunks[chunkID][1]
				localCounts[chunkID] = countCompactNameGramSegment(idx, start, end, gramSize)
			}
		}()
	}
	for chunkID := range chunks {
		jobs <- chunkID
	}
	close(jobs)
	wg.Wait()
	for _, counts := range localCounts {
		for gram, count := range counts {
			ti.counts[gram] += count
		}
	}
	results := make([]trigramSegment, len(chunks))
	jobs = make(chan int)
	wg = sync.WaitGroup{}
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunkID := range jobs {
				start, end := chunks[chunkID][0], chunks[chunkID][1]
				results[chunkID] = buildSelectiveCompactNameGramSegment(idx, start, end, gramSize, ti.counts, maxPostingCount)
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

func countCompactTrigramSegment(idx *Index, start, end int, text func(id int, cache map[int]string) string) map[uint32]int {
	counts := make(map[uint32]int)
	cache := make(map[int]string)
	for id := start; id < end; id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		for _, gram := range uniqueTrigramKeys(text(id, cache)) {
			counts[gram]++
		}
	}
	return counts
}

func countCompactNameGramSegment(idx *Index, start, end int, gramSize int) map[uint32]int {
	counts := make(map[uint32]int)
	for id := start; id < end; id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		for _, gram := range uniqueFixedGramKeysFoldASCII(idx.compactNameAt(id), gramSize) {
			counts[gram]++
		}
	}
	return counts
}

func buildSelectiveCompactTrigramSegment(idx *Index, start, end int, text func(id int, cache map[int]string) string, counts map[uint32]int, maxPostingCount int) trigramSegment {
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
			if counts[gram] > maxPostingCount {
				continue
			}
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

func buildSelectiveCompactNameGramSegment(idx *Index, start, end int, gramSize int, counts map[uint32]int, maxPostingCount int) trigramSegment {
	segment := trigramSegment{
		start:    start,
		end:      end,
		postings: make(map[uint32]compressedPosting),
	}
	raw := make(map[uint32][]uint32)
	for id := start; id < end; id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		for _, gram := range uniqueFixedGramKeysFoldASCII(idx.compactNameAt(id), gramSize) {
			if counts[gram] > maxPostingCount {
				continue
			}
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
	grams := ti.termGramKeys(term)
	if len(grams) == 0 || ti == nil {
		return nil, false
	}
	sort.Slice(grams, func(i, j int) bool {
		return ti.countForGram(grams[i]) < ti.countForGram(grams[j])
	})
	if !ti.hasAllGrams(grams) {
		return nil, true
	}
	out := make([]int, 0, ti.countForGram(grams[0]))
	for _, segment := range ti.segments {
		ids := ti.segmentCandidateIDs(segment, grams)
		out = append(out, uint32sToInts(ids)...)
	}
	return out, true
}

func (ti *compressedTrigramIndex) selectiveCandidateIDs(term string, maxIDs int) ([]int, bool, bool) {
	term = strings.ToLower(term)
	grams := ti.termGramKeys(term)
	if len(grams) == 0 || ti == nil {
		return nil, false, false
	}
	missing := false
	filtered := grams[:0]
	for _, gram := range grams {
		count := ti.countForGram(gram)
		if count == 0 {
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
		return ti.countForGram(filtered[i]) < ti.countForGram(filtered[j])
	})
	ids := ti.idsForGram(filtered[0])
	return uint32sToInts(ids), true, false
}

func (ti *compressedTrigramIndex) selectiveIntersectCandidateIDs(term string, maxIDs int) ([]int, bool, bool) {
	term = strings.ToLower(term)
	grams := ti.termGramKeys(term)
	if len(grams) == 0 || ti == nil {
		return nil, false, false
	}
	for _, gram := range grams {
		if ti.countForGram(gram) == 0 {
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
		return ti.countForGram(stored[i]) < ti.countForGram(stored[j])
	})
	if maxIDs > 0 && ti.countForGram(stored[0]) > maxIDs {
		return nil, false, false
	}
	out := make([]int, 0, ti.countForGram(stored[0]))
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
		if segment.postingForGram(gram).count > 0 {
			return true
		}
	}
	return false
}

func (ti *compressedTrigramIndex) postingCount(term string) (int, bool) {
	grams := ti.termGramKeys(strings.ToLower(term))
	if len(grams) == 0 || ti == nil {
		return 0, false
	}
	minCount := -1
	for _, gram := range grams {
		count := ti.countForGram(gram)
		if count == 0 {
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
	if ti.counts != nil {
		return len(ti.counts)
	}
	return len(ti.countKeys)
}

func (ti *compressedTrigramIndex) dropCommonPostings(maxCount int) {
	if ti == nil || maxCount <= 0 {
		return
	}
	ti.forEachCount(func(gram uint32, count int) {
		if count <= maxCount {
			return
		}
		for i := range ti.segments {
			posting := ti.segments[i].postingForGram(gram)
			if posting.count == 0 {
				continue
			}
			ti.postingBytes -= len(posting.data)
			ti.segments[i].postingBytes -= len(posting.data)
			ti.segments[i].deletePosting(gram)
		}
	})
}

func (ti *compressedTrigramIndex) hasAllGrams(grams []uint32) bool {
	for _, gram := range grams {
		if ti.countForGram(gram) == 0 {
			return false
		}
	}
	return true
}

func (ti *compressedTrigramIndex) termGramKeys(term string) []uint32 {
	if ti == nil {
		return nil
	}
	gramSize := ti.gramSize
	if gramSize == 0 {
		gramSize = 3
	}
	return uniqueFixedGramKeys(term, gramSize)
}

func (ti *compressedTrigramIndex) countForGram(gram uint32) int {
	if ti == nil {
		return 0
	}
	if ti.counts != nil {
		return ti.counts[gram]
	}
	i := sort.Search(len(ti.countKeys), func(i int) bool { return ti.countKeys[i] >= gram })
	if i < len(ti.countKeys) && ti.countKeys[i] == gram {
		return ti.countValues[i]
	}
	return 0
}

func (ti *compressedTrigramIndex) forEachCount(fn func(uint32, int)) {
	if ti == nil || fn == nil {
		return
	}
	if ti.counts != nil {
		for gram, count := range ti.counts {
			fn(gram, count)
		}
		return
	}
	for i, gram := range ti.countKeys {
		fn(gram, ti.countValues[i])
	}
}

func (segment *trigramSegment) postingForGram(gram uint32) compressedPosting {
	if segment == nil {
		return compressedPosting{}
	}
	if segment.postings != nil {
		return segment.postings[gram]
	}
	i := sort.Search(len(segment.grams), func(i int) bool { return segment.grams[i] >= gram })
	if i < len(segment.grams) && segment.grams[i] == gram && len(segment.flatPostings) > 0 {
		posting := segment.flatPostings[i]
		return compressedPosting{count: posting.count, offset: posting.offset, size: posting.size}
	}
	if i < len(segment.grams) && segment.grams[i] == gram && len(segment.postingList) > 0 {
		return segment.postingList[i]
	}
	return compressedPosting{}
}

func (segment *trigramSegment) deletePosting(gram uint32) {
	if segment == nil {
		return
	}
	if segment.postings != nil {
		delete(segment.postings, gram)
		return
	}
	i := sort.Search(len(segment.grams), func(i int) bool { return segment.grams[i] >= gram })
	if i >= len(segment.grams) || segment.grams[i] != gram {
		return
	}
	segment.grams = append(segment.grams[:i], segment.grams[i+1:]...)
	if len(segment.flatPostings) > 0 {
		segment.flatPostings = append(segment.flatPostings[:i], segment.flatPostings[i+1:]...)
	}
	if len(segment.postingList) > 0 {
		segment.postingList = append(segment.postingList[:i], segment.postingList[i+1:]...)
	}
}

func (ti *compressedTrigramIndex) compactForLowMemory() {
	if ti == nil {
		return
	}
	if ti.counts != nil {
		ti.countKeys = make([]uint32, 0, len(ti.counts))
		for gram := range ti.counts {
			ti.countKeys = append(ti.countKeys, gram)
		}
		sort.Slice(ti.countKeys, func(i, j int) bool { return ti.countKeys[i] < ti.countKeys[j] })
		ti.countValues = make([]int, len(ti.countKeys))
		for i, gram := range ti.countKeys {
			ti.countValues[i] = ti.counts[gram]
		}
		ti.counts = nil
	}
	for i := range ti.segments {
		segment := &ti.segments[i]
		if segment.postings == nil {
			continue
		}
		segment.grams = make([]uint32, 0, len(segment.postings))
		for gram := range segment.postings {
			segment.grams = append(segment.grams, gram)
		}
		sort.Slice(segment.grams, func(i, j int) bool { return segment.grams[i] < segment.grams[j] })
		segment.postingList = make([]compressedPosting, len(segment.grams))
		for j, gram := range segment.grams {
			segment.postingList[j] = segment.postings[gram]
		}
		segment.postings = nil
	}
	ti.mmapPostingData()
	debug.FreeOSMemory()
}

func (ti *compressedTrigramIndex) mmapPostingData() {
	if ti == nil || ti.postingBytes <= 0 {
		return
	}
	tmp, err := os.CreateTemp("", "seekfs-name-trigrams-*.bin")
	if err != nil {
		return
	}
	path := tmp.Name()
	var offset uint64
	var writeErr error
	for i := range ti.segments {
		segment := &ti.segments[i]
		for j := range segment.postingList {
			posting := &segment.postingList[j]
			if len(posting.data) == 0 {
				continue
			}
			posting.offset = offset
			posting.size = uint32(len(posting.data))
			if _, writeErr = tmp.Write(posting.data); writeErr != nil {
				break
			}
			offset += uint64(len(posting.data))
		}
		if writeErr != nil {
			break
		}
	}
	if closeErr := tmp.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		_ = os.Remove(path)
		return
	}
	mapped, err := mapIndexFile(path)
	if err != nil {
		_ = os.Remove(path)
		return
	}
	ti.postingBlob = mapped
	ti.postingPath = path
	for i := range ti.segments {
		segment := &ti.segments[i]
		segment.flatPostings = make([]flatPosting, len(segment.postingList))
		for j, posting := range segment.postingList {
			segment.flatPostings[j] = flatPosting{
				count:  posting.count,
				offset: posting.offset,
				size:   posting.size,
			}
		}
		segment.postingList = nil
	}
	runtime.SetFinalizer(ti, func(ti *compressedTrigramIndex) {
		if ti.postingBlob != nil {
			_ = ti.postingBlob.close()
		}
		if ti.postingPath != "" {
			_ = os.Remove(ti.postingPath)
		}
	})
}

func (ti *compressedTrigramIndex) postingData(posting compressedPosting) []byte {
	if len(posting.data) > 0 {
		return posting.data
	}
	if ti == nil || ti.postingBlob == nil || posting.size == 0 {
		return nil
	}
	start := int(posting.offset)
	end := start + int(posting.size)
	if start < 0 || end < start || end > len(ti.postingBlob.data) {
		return nil
	}
	return ti.postingBlob.data[start:end]
}

func (ti *compressedTrigramIndex) segmentCandidateIDs(segment trigramSegment, grams []uint32) []uint32 {
	var candidates []uint32
	for _, gram := range grams {
		posting := segment.postingForGram(gram)
		if posting.count == 0 {
			return nil
		}
	}
	for i, gram := range grams {
		posting := segment.postingForGram(gram)
		ids := decodeDeltaUvarint32(ti.postingData(posting), posting.count)
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
	total := ti.countForGram(gram)
	if total == 0 {
		return nil
	}
	out := make([]uint32, 0, total)
	for _, segment := range ti.segments {
		posting := segment.postingForGram(gram)
		if posting.count == 0 {
			continue
		}
		out = append(out, decodeDeltaUvarint32(ti.postingData(posting), posting.count)...)
	}
	return out
}

func uniqueTrigramKeys(s string) []uint32 {
	return uniqueFixedGramKeys(s, 3)
}

func uniqueFixedGramKeys(s string, n int) []uint32 {
	if len(s) < 3 {
		return nil
	}
	if n != 3 && n != 4 {
		return nil
	}
	if len(s) < n {
		return nil
	}
	seen := make(map[uint32]struct{}, len(s)-n+1)
	out := make([]uint32, 0, len(s)-n+1)
	for i := 0; i+n <= len(s); i++ {
		key := fixedGramKey(s, i, n)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

func uniqueTrigramKeysFoldASCII(s string) []uint32 {
	return uniqueFixedGramKeysFoldASCII(s, 3)
}

func uniqueFixedGramKeysFoldASCII(s string, n int) []uint32 {
	if n != 3 && n != 4 {
		return nil
	}
	if len(s) < n {
		return nil
	}
	seen := make(map[uint32]struct{}, len(s)-n+1)
	out := make([]uint32, 0, len(s)-n+1)
	for i := 0; i+n <= len(s); i++ {
		key := fixedGramKeyFoldASCII(s, i, n)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

func fixedGramKey(s string, start int, n int) uint32 {
	if n == 4 {
		return uint32(s[start])<<24 | uint32(s[start+1])<<16 | uint32(s[start+2])<<8 | uint32(s[start+3])
	}
	return uint32(s[start])<<16 | uint32(s[start+1])<<8 | uint32(s[start+2])
}

func fixedGramKeyFoldASCII(s string, start int, n int) uint32 {
	if n == 4 {
		return uint32(foldASCII(s[start]))<<24 | uint32(foldASCII(s[start+1]))<<16 | uint32(foldASCII(s[start+2]))<<8 | uint32(foldASCII(s[start+3]))
	}
	return uint32(foldASCII(s[start]))<<16 | uint32(foldASCII(s[start+1]))<<8 | uint32(foldASCII(s[start+2]))
}

func trigramKey(s string) uint32 {
	if len(s) != 3 {
		return 0
	}
	return uint32(s[0])<<16 | uint32(s[1])<<8 | uint32(s[2])
}

func trigramStringFromKey(key uint32) string {
	var buf [3]byte
	buf[0] = byte(key >> 16)
	buf[1] = byte(key >> 8)
	buf[2] = byte(key)
	return string(buf[:])
}

func trigramKeyFoldASCII(a, b, c byte) uint32 {
	return uint32(foldASCII(a))<<16 | uint32(foldASCII(b))<<8 | uint32(foldASCII(c))
}

func foldASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
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
