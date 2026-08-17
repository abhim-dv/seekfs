package main

import (
	"os"
	"slices"
	"testing"
	"time"
)

// TestTypeFileTermCountMatchesOracle verifies that a multi-volume `type:file
// <term>` count routes through the capped term-posting intersection (the
// type:dir mirror) instead of a full per-volume record scan, and stays in
// parity with both the search result set and the exhaustive oracle.
func TestTypeFileTermCountMatchesOracle(t *testing.T) {
	makeVolume := func(volume string, records []CompactRecord) *serviceVolumeIndex {
		idx := &Index{Source: "usn", Volume: volume, Compact: true, Records: records}
		buildOrders(idx)
		return newServiceVolumeIndex(volume+"-typefile.gsi", idx)
	}
	date := func(day int) int64 {
		return time.Date(2026, 7, day, 12, 0, 0, 0, time.UTC).UnixNano()
	}
	volumes := []*serviceVolumeIndex{
		makeVolume("C:", []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir), ModUnix: date(1)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "assets", Mode: uint32(os.ModeDir), ModUnix: date(1)},
			{FRN: 3, ParentFRN: 2, Parent: 1, Name: "assets-catalog.txt", ModUnix: date(1)},
			{FRN: 4, ParentFRN: 1, Parent: 0, Name: "notes-go.txt", ModUnix: date(1)},
		}),
		makeVolume("F:", []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir), ModUnix: date(1)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "assets", Mode: uint32(os.ModeDir), ModUnix: date(1)},
			{FRN: 3, ParentFRN: 2, Parent: 1, Name: "assets-manifest.json", ModUnix: date(1)},
			{FRN: 4, ParentFRN: 1, Parent: 0, Name: "go-build-cache", Mode: uint32(os.ModeDir), ModUnix: date(1)},
		}),
	}
	indexes := make([]*Index, len(volumes))
	for i, vol := range volumes {
		indexes[i] = vol.index
	}

	for _, query := range []string{"type:file assets", "type:file go"} {
		t.Run(query, func(t *testing.T) {
			want, err := searchAll(indexes, queryOptions{Query: query, Limit: 20}, false)
			if err != nil {
				t.Fatal(err)
			}
			searchOpts := queryOptions{Query: query, Limit: 20}
			got, err := searchServiceVolumes(volumes, searchOpts, false)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(pathsOf(got), pathsOf(want)) {
				t.Fatalf("search paths = %v, want %v", pathsOf(got), pathsOf(want))
			}
			countTrace := &searchTrace{}
			count, ok, err := countServiceVolumes(volumes, queryOptions{Query: query, Trace: countTrace})
			if err != nil || !ok {
				t.Fatalf("count handled=%v err=%v", ok, err)
			}
			if count != len(want) {
				t.Fatalf("count = %d, want %d (trace source %q)", count, len(want), countTrace.Source)
			}
			if countTrace.PlannerMode != "global-bounded-scan" || countTrace.Source != "global:bounded-scan" {
				t.Fatalf("count trace = %+v, want global bounded-scan", countTrace)
			}
		})
	}
}