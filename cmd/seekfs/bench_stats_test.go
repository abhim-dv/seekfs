package main

import (
	"encoding/binary"
	"testing"
)

func TestLatencyStatsReportsP99(t *testing.T) {
	stats := latencyStats([]float64{100, 1, 2, 3, 4, 5, 6, 7, 8, 9})
	want := map[string]float64{
		"min":    1,
		"median": 5,
		"p90":    9,
		"p95":    9,
		"p99":    9,
		"max":    100,
	}
	for key, expected := range want {
		if got := stats[key]; got != expected {
			t.Fatalf("%s = %v, want %v (stats=%v)", key, got, expected, stats)
		}
	}
}

func TestBenchCountModeRequiresService(t *testing.T) {
	if err := cmdBenchAgent([]string{"-count", "-iterations", "1"}); err == nil {
		t.Fatal("bench -count without -service succeeded")
	}
}

func TestBenchCountModeAndRequest(t *testing.T) {
	if got := benchModeName(true, false, true); got != "service-count" {
		t.Fatalf("bench mode = %q, want service-count", got)
	}
	req := serviceRequestFromOptions(queryOptions{Query: "ext:go", Limit: 20}, true)
	if !req.CountOnly || req.Command != "search" || req.Query != "ext:go" {
		t.Fatalf("count request = %+v", req)
	}
}

func TestBenchResultHashAndDiagnostics(t *testing.T) {
	complete := true
	resp := serviceResponse{
		Count:          2,
		Results:        []string{"C:\\a.txt", "C:\\b.txt"},
		Source:         "global:filename-pngc",
		FilenameDriver: "ranked-posting-pngc",
		Candidates:     2,
		BlocksDecoded:  1,
		BlocksSkipped:  3,
		Complete:       &complete,
	}
	if got := benchResultHash(resp, false); got == "" {
		t.Fatal("search result hash is empty")
	}
	if benchResultHash(resp, false) != benchResultHash(resp, false) {
		t.Fatal("search result hash is not deterministic")
	}
	if got := benchResultHash(resp, true); got == "" {
		t.Fatal("count result hash is empty")
	}
	diagnostics := benchDiagnosticsFromResponse(resp)
	if diagnostics.Driver != "ranked-posting-pngc" || diagnostics.Source != resp.Source || diagnostics.Complete != "true" {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}
	if !sameBenchDiagnostics(diagnostics, benchDiagnosticsFromResponse(resp)) {
		t.Fatal("identical diagnostics did not compare equal")
	}
}

func TestPostingPrefetchHonorsBoundAndCancellation(t *testing.T) {
	data := make([]byte, 16+16+2*28+64)
	it := postingBlockIterator{
		section:        mappedPostingSection{Data: data},
		next:           0,
		end:            2,
		blockMetaStart: 32,
		blockBlobStart: 88,
		blockBlobLen:   64,
	}
	refs := []postingBlockRankRef{{index: 0}, {index: 1}}
	putMeta := func(off, length int) {
		metaOff := it.blockMetaStart + (off/32)*28
		binary.LittleEndian.PutUint64(data[metaOff:], uint64(length))
		binary.LittleEndian.PutUint32(data[metaOff+8:], 32)
	}
	putMeta(0, 0)
	putMeta(32, 32)
	gotBytes, gotRanges, gotPages, stopped := prefetchPostingBlockRefs(it, refs, 33, nil)
	if stopped || gotBytes != 33 || gotRanges != 2 || gotPages != 2 {
		t.Fatalf("prefetch = bytes %d ranges %d pages %d stopped %v", gotBytes, gotRanges, gotPages, stopped)
	}
	_, _, _, stopped = prefetchPostingBlockRefs(it, refs, 64, func() bool { return true })
	if !stopped {
		t.Fatal("prefetch ignored cancellation")
	}
}
