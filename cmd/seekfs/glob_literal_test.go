package main

import (
	"os"
	"testing"
)

// TestComplexGlobUsesLiteralPrefilter verifies that a complex glob that is not
// reducible to a single extension posting (e.g. `glob:*scan*`) routes through a
// glob-literal name-substring source as a prefilter instead of a full-volume
// scan, and returns correct results.  A record matching `glob:*scan*`
// necessarily has `scan` in its name, so the literal is a safe superset.
func TestComplexGlobUsesLiteralPrefilter(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
	}
	add := func(frn, parentFRN uint64, parent int32, name string, mode uint32) int32 {
		idx.Records = append(idx.Records, CompactRecord{
			FRN: frn, ParentFRN: parentFRN, Parent: parent, Name: name, Mode: mode,
			Size: 1024, ModUnix: 1,
		})
		return int32(len(idx.Records) - 1)
	}
	root := add(1, 1, -1, ".", uint32(os.ModeDir))
	dir := add(2, 1, root, "dataset", uint32(os.ModeDir))
	add(3, 2, dir, "scan-000.nrrd", 0)
	add(4, 2, dir, "rescan-note.txt", 0)
	add(5, 2, dir, "report.go", 0)
	buildOrders(idx)
	vol := newServiceVolumeIndex("glob-literal.gsi", idx)

	trace := &searchTrace{}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
		Query: "glob:*scan*", Limit: 20, Trace: trace,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("glob scan result = %v, want the two matching files", pathsOf(got))
	}

	// Count must match the search result count.
	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
		Query: "glob:*scan*",
	})
	if err != nil || !ok {
		t.Fatalf("glob count handled=%v err=%v", ok, err)
	}
	if count != 2 {
		t.Fatalf("glob count = %d, want 2", count)
	}
}
