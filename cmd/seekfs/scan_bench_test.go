package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var benchmarkPathArenaSink int

// buildLargeScanFixture creates a synthetic index with a deep directory tree and
// many files so broad path-query scan behavior can be benchmarked without the service.
// Roughly nDirs directories each holding filesPerDir files.
func buildLargeScanFixture(nDirs, filesPerDir int) *Index {
	idx := &Index{Source: "usn", Volume: "C:", Compact: true}
	add := func(frn, parentFRN uint64, parent int32, name string, mode uint32) int {
		idx.Records = append(idx.Records, CompactRecord{
			FRN: frn, ParentFRN: parentFRN, Parent: parent, Name: name, Mode: mode,
		})
		return len(idx.Records) - 1
	}
	root := add(1, 1, -1, "C:", uint32(os.ModeDir))
	var frn uint64 = 2
	for d := 0; d < nDirs; d++ {
		// A handful of directories contain "src" in the name; most do not, so a
		// broad "src" path query must scan widely to find descendants.
		dirName := fmt.Sprintf("module%d", d)
		if d%50 == 0 {
			dirName = fmt.Sprintf("src%d", d)
		}
		dirIdx := add(frn, 1, int32(root), dirName, uint32(os.ModeDir))
		frn++
		for f := 0; f < filesPerDir; f++ {
			name := fmt.Sprintf("file%d.go", f)
			if f%7 == 0 {
				name = fmt.Sprintf("main%d.go", f)
			}
			add(frn, uint64(dirIdx+1), int32(dirIdx), name, 0)
			frn++
		}
	}
	buildOrders(idx)
	return idx
}

func benchScan(b *testing.B, query string, matchPath bool) {
	idx := buildLargeScanFixture(4000, 250) // ~1M records
	vol := newServiceVolumeIndex("bench.gsi", idx)
	opts := queryOptions{Query: query, MatchPath: matchPath, Limit: 20}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScanBroadPathSingle(b *testing.B) { benchScan(b, "src", true) }
func BenchmarkScanBroadPathTwo(b *testing.B)    { benchScan(b, "src main", true) }
func BenchmarkScanSelectivePath(b *testing.B)   { benchScan(b, "src main100.go", true) }

type benchPathArena struct {
	blob    []byte
	offsets []uint32
	lengths []uint32
}

func buildBenchPathArena(idx *Index) benchPathArena {
	return buildBenchPathArenaCase(idx, true)
}

func buildBenchPathArenaCase(idx *Index, lower bool) benchPathArena {
	recordCount := idx.compactRecordCount()
	arena := benchPathArena{
		offsets: make([]uint32, recordCount),
		lengths: make([]uint32, recordCount),
	}
	stack := make([]string, 0, 32)
	for id := 0; id < recordCount; id++ {
		arena.offsets[id] = uint32(len(arena.blob))
		before := len(arena.blob)
		arena.blob = appendCompactPathASCII(arena.blob, idx, id, stack[:0], lower)
		arena.lengths[id] = uint32(len(arena.blob) - before)
	}
	return arena
}

func appendCompactPathASCII(dst []byte, idx *Index, id int, stack []string, lower bool) []byte {
	cur := id
	for depth := 0; depth < 1024; depth++ {
		if cur < 0 || cur >= idx.compactRecordCount() {
			break
		}
		rec := idx.compactRecord(cur)
		if rec.Name != "." {
			stack = append(stack, rec.Name)
		}
		if rec.Parent < 0 || int(rec.Parent) == cur {
			break
		}
		cur = int(rec.Parent)
	}
	root := idx.Volume
	if root == "" && len(idx.Roots) > 0 {
		root = strings.TrimRight(idx.Roots[0], `\`)
	}
	dst = appendMaybeLowerASCII(dst, root, lower)
	for p := len(stack) - 1; p >= 0; p-- {
		if len(dst) > 0 {
			dst = append(dst, '\\')
		}
		dst = appendMaybeLowerASCII(dst, stack[p], lower)
	}
	return dst
}

func appendMaybeLowerASCII(dst []byte, s string, lower bool) []byte {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if lower && c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		dst = append(dst, c)
	}
	return dst
}

func (arena benchPathArena) pathBytes(id int) []byte {
	if id < 0 || id >= len(arena.offsets) {
		return nil
	}
	off := arena.offsets[id]
	end := off + arena.lengths[id]
	if end < off || int(end) > len(arena.blob) {
		return nil
	}
	return arena.blob[off:end]
}

func BenchmarkPathArenaBuildSynthetic1M(b *testing.B) {
	idx := buildLargeScanFixture(4000, 250)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		arena := buildBenchPathArena(idx)
		benchmarkPathArenaSink = len(arena.blob)
		b.ReportMetric(float64(len(arena.blob))/(1024*1024), "fullpath_mib")
		b.ReportMetric(float64(8*len(arena.offsets))/(1024*1024), "u32_refs_mib")
	}
}

func BenchmarkPathArenaRealIndexesBuildAndScan(b *testing.B) {
	if !envBool("SEEKFS_REAL_INDEX_LOAD_MATRIX") {
		b.Skip("set SEEKFS_REAL_INDEX_LOAD_MATRIX=1 plus SEEKFS_REAL_INDEX_DBS or bench/real-indexes.local.txt")
	}
	dbs := realIndexBenchmarkDBs(b)
	if len(dbs) == 0 {
		b.Skip("no real indexes configured")
	}
	oldMode := os.Getenv("SEEKFS_MEMORY_MODE")
	_ = os.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	b.Cleanup(func() {
		if oldMode == "" {
			_ = os.Unsetenv("SEEKFS_MEMORY_MODE")
		} else {
			_ = os.Setenv("SEEKFS_MEMORY_MODE", oldMode)
		}
	})
	for _, db := range dbs {
		db := db
		b.Run(filepath.Base(db), func(b *testing.B) {
			idx, err := loadIndexForService(db)
			if err != nil {
				b.Fatalf("load %s: %v", db, err)
			}
			start := time.Now()
			arena := buildBenchPathArena(idx)
			buildMS := float64(time.Since(start).Microseconds()) / 1000
			recordCount := idx.compactRecordCount()
			countStart := time.Now()
			srcCount := scanBenchPathArenaCount(idx, arena, []byte("src"))
			srcCountMS := float64(time.Since(countStart).Microseconds()) / 1000
			b.ReportAllocs()
			needles := [][]byte{[]byte("src"), []byte("users"), []byte("appdata"), []byte("downloads"), []byte("workspace")}
			order := idx.CompactNameOrder
			total := 0
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				total = 0
				for _, needle := range needles {
					total += scanBenchPathArenaLimit(idx, arena, order, needle, 20)
				}
				benchmarkPathArenaSink = total
			}
			b.StopTimer()
			b.ReportMetric(float64(recordCount), "records")
			b.ReportMetric(float64(len(arena.blob))/(1024*1024), "fullpath_mib")
			b.ReportMetric(float64(8*len(arena.offsets))/(1024*1024), "u32_refs_mib")
			b.ReportMetric(buildMS, "build_ms")
			b.ReportMetric(srcCountMS, "count_src_ms")
			b.ReportMetric(float64(srcCount), "src_hits")
			b.ReportMetric(float64(total), "hits")
			if err := closeIndexMMapRecords(idx); err != nil {
				b.Fatalf("close mmap: %v", err)
			}
			runtime.GC()
		})
	}
}

func BenchmarkPathArenaRealIndexesMixedCaseFoldScan(b *testing.B) {
	if !envBool("SEEKFS_REAL_INDEX_LOAD_MATRIX") {
		b.Skip("set SEEKFS_REAL_INDEX_LOAD_MATRIX=1 plus SEEKFS_REAL_INDEX_DBS or bench/real-indexes.local.txt")
	}
	dbs := realIndexBenchmarkDBs(b)
	if len(dbs) == 0 {
		b.Skip("no real indexes configured")
	}
	oldMode := os.Getenv("SEEKFS_MEMORY_MODE")
	_ = os.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	b.Cleanup(func() {
		if oldMode == "" {
			_ = os.Unsetenv("SEEKFS_MEMORY_MODE")
		} else {
			_ = os.Setenv("SEEKFS_MEMORY_MODE", oldMode)
		}
	})
	for _, db := range dbs {
		db := db
		b.Run(filepath.Base(db), func(b *testing.B) {
			idx, err := loadIndexForService(db)
			if err != nil {
				b.Fatalf("load %s: %v", db, err)
			}
			start := time.Now()
			arena := buildBenchPathArenaCase(idx, false)
			buildMS := float64(time.Since(start).Microseconds()) / 1000
			recordCount := idx.compactRecordCount()
			countStart := time.Now()
			srcCount := scanBenchPathArenaCountFold(idx, arena, []byte("src"))
			srcCountMS := float64(time.Since(countStart).Microseconds()) / 1000
			b.ReportAllocs()
			needles := [][]byte{[]byte("src"), []byte("users"), []byte("appdata"), []byte("downloads"), []byte("workspace")}
			order := idx.CompactNameOrder
			total := 0
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				total = 0
				for _, needle := range needles {
					total += scanBenchPathArenaLimitFold(idx, arena, order, needle, 20)
				}
				benchmarkPathArenaSink = total
			}
			b.StopTimer()
			b.ReportMetric(float64(recordCount), "records")
			b.ReportMetric(float64(len(arena.blob))/(1024*1024), "fullpath_mib")
			b.ReportMetric(float64(8*len(arena.offsets))/(1024*1024), "u32_refs_mib")
			b.ReportMetric(buildMS, "build_ms")
			b.ReportMetric(srcCountMS, "count_src_ms")
			b.ReportMetric(float64(srcCount), "src_hits")
			b.ReportMetric(float64(total), "hits")
			if err := closeIndexMMapRecords(idx); err != nil {
				b.Fatalf("close mmap: %v", err)
			}
			runtime.GC()
		})
	}
}

func scanBenchPathArenaLimit(idx *Index, arena benchPathArena, order []int, needle []byte, limit int) int {
	count := 0
	for pos := 0; pos < compactOrderLen(order, idx.compactRecordCount()); pos++ {
		id := compactOrderAt(order, pos)
		if bytes.Contains(arena.pathBytes(id), needle) {
			count++
			if count >= limit {
				break
			}
		}
	}
	return count
}

func scanBenchPathArenaCount(idx *Index, arena benchPathArena, needle []byte) int {
	count := 0
	for id := 0; id < idx.compactRecordCount(); id++ {
		if bytes.Contains(arena.pathBytes(id), needle) {
			count++
		}
	}
	return count
}

func scanBenchPathArenaLimitFold(idx *Index, arena benchPathArena, order []int, needle []byte, limit int) int {
	count := 0
	for pos := 0; pos < compactOrderLen(order, idx.compactRecordCount()); pos++ {
		id := compactOrderAt(order, pos)
		if containsFoldASCIIBytes(arena.pathBytes(id), needle) {
			count++
			if count >= limit {
				break
			}
		}
	}
	return count
}

func scanBenchPathArenaCountFold(idx *Index, arena benchPathArena, needle []byte) int {
	count := 0
	for id := 0; id < idx.compactRecordCount(); id++ {
		if containsFoldASCIIBytes(arena.pathBytes(id), needle) {
			count++
		}
	}
	return count
}

func containsFoldASCIIBytes(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	if len(needle) > len(haystack) {
		return false
	}
	first := lowerASCIIByte(needle[0])
	limit := len(haystack) - len(needle)
	for i := 0; i <= limit; i++ {
		if lowerASCIIByte(haystack[i]) != first {
			continue
		}
		match := true
		for j := 1; j < len(needle); j++ {
			if lowerASCIIByte(haystack[i+j]) != lowerASCIIByte(needle[j]) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func lowerASCIIByte(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

func realIndexBenchmarkDBs(b *testing.B) []string {
	b.Helper()
	raw := strings.TrimSpace(os.Getenv("SEEKFS_REAL_INDEX_DBS"))
	var paths []string
	if raw != "" {
		for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ';' || r == '\n' || r == '\r' }) {
			if path := strings.TrimSpace(part); path != "" {
				paths = append(paths, path)
			}
		}
	}
	if len(paths) == 0 {
		data, err := os.ReadFile(filepath.Join("..", "..", "bench", "real-indexes.local.txt"))
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				paths = append(paths, line)
			}
		}
	}
	out := paths[:0]
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			out = append(out, path)
		} else {
			b.Logf("skipping missing real index %s: %v", path, err)
		}
	}
	return out
}

func BenchmarkPathArenaBroadPathScalarCountSynthetic1M(b *testing.B) {
	idx := buildLargeScanFixture(4000, 250)
	arena := buildBenchPathArena(idx)
	needle := []byte("src")
	b.ReportMetric(float64(len(arena.blob))/(1024*1024), "fullpath_mib")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		count := 0
		for id := 0; id < idx.compactRecordCount(); id++ {
			if bytes.Contains(arena.pathBytes(id), needle) {
				count++
			}
		}
		benchmarkPathArenaSink = count
	}
}

func BenchmarkPathArenaBroadPathScalarLimitSynthetic1M(b *testing.B) {
	idx := buildLargeScanFixture(4000, 250)
	arena := buildBenchPathArena(idx)
	needle := []byte("src")
	order := idx.CompactNameOrder
	b.ReportMetric(float64(len(arena.blob))/(1024*1024), "fullpath_mib")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		count := 0
		for pos := 0; pos < compactOrderLen(order, idx.compactRecordCount()); pos++ {
			id := compactOrderAt(order, pos)
			if bytes.Contains(arena.pathBytes(id), needle) {
				count++
				if count >= 20 {
					break
				}
			}
		}
		benchmarkPathArenaSink = count
	}
}
