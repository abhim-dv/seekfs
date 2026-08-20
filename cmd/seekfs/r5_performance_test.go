package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestR5MappedScalarRangeCountUsesQualifyingInterval(t *testing.T) {
	const files = 4096
	idx := &Index{
		Version: indexVersionV9,
		Source:  "usn",
		Volume:  "C:",
		Roots:   []string{`C:\`},
		Compact: true,
		Records: make([]CompactRecord, 0, files+1),
	}
	idx.Records = append(idx.Records, CompactRecord{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)})
	for i := 0; i < files; i++ {
		idx.Records = append(idx.Records, CompactRecord{
			FRN: uint64(i + 2), ParentFRN: 1, Parent: 0,
			Name: fmt.Sprintf("file-%04d.bin", i), Size: int64(i), ModUnix: int64(i + 1),
		})
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "mapped-scalar-range.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save index: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load mapped index: %v", err)
	}
	if loaded.MMapRecords == nil {
		t.Fatal("expected mapped records")
	}
	t.Cleanup(func() { _ = loaded.MMapRecords.file.close() })
	vol := newServiceVolumeIndex(db, loaded)
	trace := &searchTrace{}
	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
		Query: "size:>=4095", Trace: trace,
	})
	if err != nil || !ok || count != 1 {
		t.Fatalf("count = %d handled=%v err=%v, want 1 true nil", count, ok, err)
	}
	if trace.Source != "global:scalar-range" {
		t.Fatalf("trace source = %q, want global:scalar-range", trace.Source)
	}
	scalar, ok := scalarRangeForVolume(vol, mustParseQuery(t, queryOptions{Query: "size:>=4095"}))
	if !ok || scalar.end-scalar.start != 1 {
		t.Fatalf("scalar interval = %+v ok=%v, want one qualifying record", scalar, ok)
	}
}

func TestR5SyntheticSearchAndCountP95P99(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")

	cIdx := dottedPathBenchmarkIndex(25_000)
	fIdx := dottedPathBenchmarkIndex(25_000)
	fIdx.Volume = "F:"
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("r5-perf-c.gsi", cIdx),
		newServiceVolumeIndex("r5-perf-f.gsi", fIdx),
	}
	queries := []queryOptions{
		{Query: "ext:nrrd", Limit: 20},
		{Query: "path:workspace", Limit: 20},
		{Query: "path:workspace ext:nrrd", Limit: 20},
		{Query: "trainingdata Dataset nrrd", MatchPath: true, Limit: 20},
		{Query: "path:missing-r5-component", Limit: 20},
	}

	runR5LatencyState(t, "warm-base", volumes, queries)
	volumes[1].applyUSNChanges([]usnChange{{
		FRN:       9_000_001,
		ParentFRN: 2,
		USN:       1,
		Reason:    usnReasonFileCreate,
		Name:      "active-overlay-r5.nrrd",
	}})
	runR5LatencyState(t, "active-overlay", volumes, queries)
}

func runR5LatencyState(t *testing.T, state string, volumes []*serviceVolumeIndex, queries []queryOptions) {
	t.Helper()
	const iterations = 12
	searchMS := make([]float64, 0, len(queries)*iterations)
	countMS := make([]float64, 0, len(queries)*iterations)
	for i := 0; i < iterations; i++ {
		for _, opts := range queries {
			start := time.Now()
			if _, err := searchServiceVolumes(volumes, opts, false); err != nil {
				t.Fatalf("%s search %q: %v", state, opts.Query, err)
			}
			searchMS = append(searchMS, float64(time.Since(start).Microseconds())/1000)

			start = time.Now()
			if _, _, err := countServiceVolumes(volumes, opts); err != nil {
				t.Fatalf("%s count %q: %v", state, opts.Query, err)
			}
			countMS = append(countMS, float64(time.Since(start).Microseconds())/1000)
		}
	}

	for operation, samples := range map[string][]float64{"search": searchMS, "count": countMS} {
		stats := latencyStats(samples)
		t.Logf("R5 synthetic %s %s latency_ms=%v", state, operation, stats)
		if stats["p95"] <= 150 && stats["p99"] <= 250 {
			continue
		}
		message := fmt.Sprintf("R5 synthetic %s %s latency p95=%.3fms p99=%.3fms, want <=150ms/250ms", state, operation, stats["p95"], stats["p99"])
		if envBool("SEEKFS_ENFORCE_LATENCY_TESTS") {
			t.Fatal(message)
		}
		t.Log(message)
	}
}
