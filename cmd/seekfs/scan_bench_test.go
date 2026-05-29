package main

import (
	"fmt"
	"os"
	"testing"
)

// buildLargeScanFixture creates a synthetic index with a deep directory tree and
// many files so broad-path-scan behavior can be benchmarked without the service.
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
