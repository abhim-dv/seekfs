package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCompressedNameTrigramCandidatesMatchNameScan(t *testing.T) {
	idx := dottedPathBenchmarkIndex(5000)
	ti := buildNameTrigramIndex(idx)
	terms := []string{".nrrd", "nrrd", ".raw", ".pdf", ".opencode", "plain", "missing"}
	for _, term := range terms {
		t.Run(term, func(t *testing.T) {
			candidates, ok := ti.candidateIDs(term)
			if !ok {
				t.Fatalf("candidateIDs(%q) declined, want trigram candidates", term)
			}
			got := filterNameContains(idx, candidates, strings.ToLower(term))
			want := scanNameContains(idx, strings.ToLower(term))
			if !sameTrigramIntSet(got, want) {
				t.Fatalf("candidate matches = %v, scan matches = %v", namesForIDs(idx, got), namesForIDs(idx, want))
			}
		})
	}
}

func TestCompressedNameTrigramDeclinesShortTerms(t *testing.T) {
	ti := buildNameTrigramIndex(dottedPathBenchmarkIndex(100))
	for _, term := range []string{"", ".", "go"} {
		if _, ok := ti.candidateIDs(term); ok {
			t.Fatalf("candidateIDs(%q) accepted short term, want decline", term)
		}
	}
}

func TestSelectiveIntersectCandidateIDsCollapsesFusedNoHit(t *testing.T) {
	idx := dottedPathBenchmarkIndex(25_000)
	ti := buildNameTrigramIndex(idx)
	ids, ok, missing := ti.selectiveIntersectCandidateIDs("downloads.nrrd", serviceNameTrigramCandidateMaxIDs)
	if !ok {
		t.Fatal("selectiveIntersectCandidateIDs declined fused no-hit term")
	}
	if len(ids) != 0 {
		t.Fatalf("fused no-hit candidate count = %d missing=%v, want 0", len(ids), missing)
	}
}

func TestCompressedPathTrigramCandidatesMatchPathScan(t *testing.T) {
	idx := dottedPathBenchmarkIndex(5000)
	ti := buildPathTrigramIndex(idx)
	terms := []string{".nrrd", "nrrd-cache", ".raw", ".pdf", ".opencode", "workspace", "missing"}
	for _, term := range terms {
		t.Run(term, func(t *testing.T) {
			candidates, ok := ti.candidateIDs(term)
			if !ok {
				t.Fatalf("candidateIDs(%q) declined, want trigram candidates", term)
			}
			got := filterPathContains(idx, candidates, strings.ToLower(term))
			want := scanPathContains(idx, strings.ToLower(term))
			if !sameTrigramIntSet(got, want) {
				t.Fatalf("candidate paths = %v, scan paths = %v", pathsForIDs(idx, got), pathsForIDs(idx, want))
			}
		})
	}
}

func TestCompressedNameTrigramUsesLessThanRawPostings(t *testing.T) {
	ti := buildNameTrigramIndex(dottedPathBenchmarkIndex(10_000))
	rawBytes := 0
	for _, count := range ti.counts {
		rawBytes += count * 4
	}
	if ti.postingBytes == 0 || rawBytes == 0 {
		t.Fatalf("postingBytes=%d rawBytes=%d, want populated index", ti.postingBytes, rawBytes)
	}
	if ti.postingBytes >= rawBytes {
		t.Fatalf("compressed postings = %d bytes, raw postings = %d bytes; want compression win", ti.postingBytes, rawBytes)
	}
}

func TestCompressedPathTrigramUsesLessThanRawPostings(t *testing.T) {
	ti := buildPathTrigramIndex(dottedPathBenchmarkIndex(10_000))
	rawBytes := 0
	for _, count := range ti.counts {
		rawBytes += count * 4
	}
	if ti.postingBytes == 0 || rawBytes == 0 {
		t.Fatalf("postingBytes=%d rawBytes=%d, want populated index", ti.postingBytes, rawBytes)
	}
	if ti.postingBytes >= rawBytes {
		t.Fatalf("compressed postings = %d bytes, raw postings = %d bytes; want compression win", ti.postingBytes, rawBytes)
	}
}

func TestRealGSITrigramSizing(t *testing.T) {
	dbs := strings.Fields(os.Getenv("SEEKFS_TRIGRAM_GSI"))
	if len(dbs) == 0 {
		t.Skip("set SEEKFS_TRIGRAM_GSI to one or more .gsi paths")
	}
	limits := []int{100_000, 500_000, 1_000_000}
	for _, db := range dbs {
		idx, err := loadIndex(db)
		if err != nil {
			t.Fatalf("loadIndex(%q): %v", db, err)
		}
		total := idx.compactRecordCount()
		t.Logf("db=%s volume=%s records=%d", db, idx.Volume, total)
		for _, limit := range limits {
			if limit > total {
				continue
			}
			t.Run(fmt.Sprintf("%s/%d", idx.Volume, limit), func(t *testing.T) {
				name := measureLimitedTrigramIndex(t, idx, limit, func(id int, cache map[int]string) string {
					return idx.compactLowerNameAt(id)
				})
				path := measureLimitedTrigramIndex(t, idx, limit, func(id int, cache map[int]string) string {
					return strings.ToLower(idx.reconstructCompactPathCached(id, cache))
				})
				scale := float64(total) / float64(limit)
				t.Logf("limit=%d name_keys=%d name_posting_bytes=%d name_raw_bytes=%d name_build=%s name_projected_full=%.1fMB",
					limit, name.keys, name.postingBytes, name.rawBytes, name.elapsed.Round(time.Millisecond), float64(name.postingBytes)*scale/1024/1024)
				t.Logf("limit=%d path_keys=%d path_posting_bytes=%d path_raw_bytes=%d path_build=%s path_projected_full=%.1fMB",
					limit, path.keys, path.postingBytes, path.rawBytes, path.elapsed.Round(time.Millisecond), float64(path.postingBytes)*scale/1024/1024)
				t.Logf("limit=%d heap_delta_name=%.1fMB heap_delta_path=%.1fMB",
					limit, float64(name.heapDelta)/1024/1024, float64(path.heapDelta)/1024/1024)
			})
		}
	}
}

func BenchmarkCompressedNameTrigramBuild(b *testing.B) {
	idx := dottedPathBenchmarkIndex(100_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ti := buildNameTrigramIndex(idx)
		if ti.keyCount() == 0 {
			b.Fatal("empty trigram index")
		}
		b.ReportMetric(float64(ti.postingBytes), "posting_bytes")
	}
}

func BenchmarkCompressedPathTrigramBuild(b *testing.B) {
	idx := dottedPathBenchmarkIndex(100_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ti := buildPathTrigramIndex(idx)
		if ti.keyCount() == 0 {
			b.Fatal("empty trigram index")
		}
		b.ReportMetric(float64(ti.postingBytes), "posting_bytes")
	}
}

func BenchmarkCompressedNameTrigramCandidates(b *testing.B) {
	idx := dottedPathBenchmarkIndex(100_000)
	ti := buildNameTrigramIndex(idx)
	vol := newServiceVolumeIndex("bench.gsi", idx)
	cases := []string{".nrrd", "nrrd", ".raw", ".pdf", ".opencode", "plain"}
	for _, term := range cases {
		b.Run("trigram/"+term, func(b *testing.B) {
			b.ReportMetric(float64(ti.postingBytes), "posting_bytes")
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				candidates, ok := ti.candidateIDs(term)
				if !ok {
					b.Fatalf("candidateIDs(%q) declined", term)
				}
				matches := filterNameContains(idx, candidates, strings.ToLower(term))
				if term != ".opencode" && term != "missing" && len(matches) == 0 {
					b.Fatalf("no matches for %q", term)
				}
			}
		})
		b.Run("scan/"+term, func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				matches := vol.scanNameTermPosting(strings.ToLower(term))
				if term != ".opencode" && len(matches) == 0 {
					b.Fatalf("no matches for %q", term)
				}
			}
		})
	}
}

func BenchmarkCompressedPathTrigramCandidates(b *testing.B) {
	idx := dottedPathBenchmarkIndex(100_000)
	ti := buildPathTrigramIndex(idx)
	vol := newServiceVolumeIndex("bench.gsi", idx)
	cases := []string{".nrrd", "nrrd-cache", ".raw", ".pdf", ".opencode", "workspace", "plain"}
	for _, term := range cases {
		b.Run("trigram/"+term, func(b *testing.B) {
			b.ReportMetric(float64(ti.postingBytes), "posting_bytes")
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				candidates, ok := ti.candidateIDs(term)
				if !ok {
					b.Fatalf("candidateIDs(%q) declined", term)
				}
				matches := filterPathContains(idx, candidates, strings.ToLower(term))
				if len(matches) == 0 {
					b.Fatalf("no matches for %q", term)
				}
			}
		})
		b.Run("scan/"+term, func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				matches := vol.scanPathTermPosting(strings.ToLower(term))
				if len(matches) == 0 {
					b.Fatalf("no matches for %q", term)
				}
			}
		})
	}
}

type trigramSizing struct {
	keys         int
	postingBytes int
	rawBytes     int
	heapDelta    uint64
	elapsed      time.Duration
}

func measureLimitedTrigramIndex(t *testing.T, idx *Index, limit int, text func(id int, cache map[int]string) string) trigramSizing {
	t.Helper()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	ti := buildLimitedCompactTrigramIndex(idx, limit, text)
	elapsed := time.Since(start)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	rawBytes := 0
	for _, count := range ti.counts {
		rawBytes += count * 4
	}
	heapDelta := uint64(0)
	if after.HeapAlloc > before.HeapAlloc {
		heapDelta = after.HeapAlloc - before.HeapAlloc
	}
	return trigramSizing{
		keys:         ti.keyCount(),
		postingBytes: ti.postingBytes,
		rawBytes:     rawBytes,
		heapDelta:    heapDelta,
		elapsed:      elapsed,
	}
}

func buildLimitedCompactTrigramIndex(idx *Index, limit int, text func(id int, cache map[int]string) string) *compressedTrigramIndex {
	if idx == nil || limit <= 0 {
		return &compressedTrigramIndex{counts: make(map[uint32]int)}
	}
	recordCount := min(limit, idx.compactRecordCount())
	ti := &compressedTrigramIndex{
		counts:      make(map[uint32]int),
		recordCount: recordCount,
	}
	segment := buildCompactTrigramSegment(idx, 0, recordCount, text)
	ti.segments = []trigramSegment{segment}
	ti.postingBytes = segment.postingBytes
	for gram, posting := range segment.postings {
		ti.counts[gram] = posting.count
	}
	return ti
}

func scanNameContains(idx *Index, term string) []int {
	out := make([]int, 0)
	for id := 0; id < idx.compactRecordCount(); id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		if strings.Contains(idx.compactLowerNameAt(id), term) {
			out = append(out, id)
		}
	}
	return out
}

func scanPathContains(idx *Index, term string) []int {
	out := make([]int, 0)
	for id := 0; id < idx.compactRecordCount(); id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		if idx.compactPathContainsTerm(id, term) {
			out = append(out, id)
		}
	}
	return out
}

func filterNameContains(idx *Index, candidates []int, term string) []int {
	out := make([]int, 0, len(candidates))
	for _, id := range candidates {
		if id < 0 || id >= idx.compactRecordCount() {
			continue
		}
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		if strings.Contains(idx.compactLowerNameAt(id), term) {
			out = append(out, id)
		}
	}
	return out
}

func filterPathContains(idx *Index, candidates []int, term string) []int {
	out := make([]int, 0, len(candidates))
	for _, id := range candidates {
		if id < 0 || id >= idx.compactRecordCount() {
			continue
		}
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		if idx.compactPathContainsTerm(id, term) {
			out = append(out, id)
		}
	}
	return out
}

func namesForIDs(idx *Index, ids []int) []string {
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		names = append(names, fmt.Sprintf("%d:%s", id, idx.compactRecord(id).Name))
	}
	return names
}

func pathsForIDs(idx *Index, ids []int) []string {
	paths := make([]string, 0, len(ids))
	cache := make(map[int]string)
	for _, id := range ids {
		paths = append(paths, fmt.Sprintf("%d:%s", id, idx.reconstructCompactPathCached(id, cache)))
	}
	return paths
}

func sameTrigramIntSet(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[int]int, len(a))
	for _, value := range a {
		seen[value]++
	}
	for _, value := range b {
		seen[value]--
		if seen[value] < 0 {
			return false
		}
	}
	return true
}
