package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDirectV9DirectorySubtreeBytes builds a small tree with known file sizes
// and checks the persisted recursive directory sizes, both through the decoded
// derived section and through the Entry a search would report.
func TestDirectV9DirectorySubtreeBytes(t *testing.T) {
	records := []directV9Record{
		{FRN: 1, ParentFRN: 0, Mode: uint32(os.ModeDir), Name: "root"},
		{FRN: 2, ParentFRN: 1, Mode: uint32(os.ModeDir), Name: "A"},
		{FRN: 3, ParentFRN: 2, Mode: uint32(os.ModeDir), Name: "B"},
		{FRN: 4, ParentFRN: 3, Name: "f1", Size: 100},
		{FRN: 5, ParentFRN: 2, Name: "f2", Size: 50},
		{FRN: 6, ParentFRN: 1, Name: "f3", Size: 7},
		{FRN: 7, ParentFRN: 1, Mode: uint32(os.ModeDir), Name: "C"},
		{FRN: 8, ParentFRN: 1, Name: "f4", Size: 1000},
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "dirsizes.gsi")
	if _, err := buildDirectV9(context.Background(), directV9BuildOptions{
		OutputPath: path,
		SpoolDir:   filepath.Join(dir, "spool"),
		Roots:      []string{"X:\\"},
		Volume:     "X:",
		Source:     "direct-test",
		BuiltAt:    time.Unix(123, 0),
		Records:    newDirectV9SliceSource(records),
		RunRecords: 4,
		RunBytes:   4096,
	}); err != nil {
		t.Fatal(err)
	}
	idx, err := loadIndexMMap(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.MMapRecords.file.close() })

	// ids are assigned by ascending FRN, so id i is FRN i+1.
	want := []uint64{1157, 150, 100, 100, 50, 7, 0, 1000}
	if !equalUint64s(idx.Derived.SubtreeBytes, want) {
		t.Fatalf("subtree bytes = %v, want %v", idx.Derived.SubtreeBytes, want)
	}

	// A directory entry reports the aggregate; a file keeps its own size.
	cache := map[int]string{}
	if got := compactEntryFromRecord(idx, 1, idx.compactRecord(1), cache, false).Size; got != 150 {
		t.Fatalf("directory A entry size = %d, want 150", got)
	}
	if got := compactEntryFromRecord(idx, 0, idx.compactRecord(0), cache, false).Size; got != 1157 {
		t.Fatalf("root entry size = %d, want 1157", got)
	}
	if got := compactEntryFromRecord(idx, 3, idx.compactRecord(3), cache, false).Size; got != 100 {
		t.Fatalf("file f1 entry size = %d, want 100", got)
	}
	if got := compactEntryFromRecord(idx, 6, idx.compactRecord(6), cache, false).Size; got != 0 {
		t.Fatalf("empty directory C entry size = %d, want 0", got)
	}

	// A size: filter must see the directory aggregate, not 0.
	pq := parsedQuery{SizeFilters: []sizeFilter{{op: ">", bytes: 200}}}
	dirEntry := compactEntryFromRecord(idx, 0, idx.compactRecord(0), cache, true)
	if !entryMatches(dirEntry, pq, false) {
		t.Fatal("root dir should match size:>200 via its aggregate size")
	}
	emptyEntry := compactEntryFromRecord(idx, 6, idx.compactRecord(6), cache, true)
	if entryMatches(emptyEntry, pq, false) {
		t.Fatal("empty dir should not match size:>200")
	}
}
