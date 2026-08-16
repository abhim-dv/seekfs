package main

import (
	"os"
	"testing"
)

// TestBareRegexIsPathScopedAndRoutesLiteral verifies that a bare `regex:`
// query (no explicit path:/dir:) is treated as path-scoped so the regex-literal
// planner serves it rather than declining to an exhaustive scan.  A regex
// evaluates against the full path, so a lone regex token implies path mode.
func TestBareRegexIsPathScopedAndRoutesLiteral(t *testing.T) {
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
	add(4, 2, dir, "notes.txt", 0)
	add(5, 2, dir, "report.go", 0)
	buildOrders(idx)
	vol := newServiceVolumeIndex("regex-literal.gsi", idx)

	trace := &searchTrace{}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
		Query: "regex:.*scan.*", Limit: 20, Trace: trace,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "scan-000.nrrd" {
		t.Fatalf("regex scan result = %v, want the matching file", pathsOf(got))
	}
}
