package main

import "testing"

func TestNewPackedRecordsPreservesFields(t *testing.T) {
	records := []CompactRecord{
		{
			FRN:       101,
			ParentFRN: 100,
			Parent:    -1,
			Name:      ".",
			NameOff:   7,
			Mode:      1 << 31,
			Size:      4096,
			ModUnix:   123456789,
		},
		{
			FRN:       102,
			ParentFRN: 101,
			Parent:    0,
			Name:      "child.txt",
			NameOff:   11,
			Mode:      0,
			Size:      99,
			ModUnix:   987654321,
			Deleted:   true,
		},
	}

	packed := newPackedRecords(records)
	if packed == nil {
		t.Fatal("newPackedRecords returned nil for non-empty records")
	}
	if packed.Len() != len(records) {
		t.Fatalf("Len = %d, want %d", packed.Len(), len(records))
	}
	for i, want := range records {
		got := packed.At(i)
		if got.FRN != want.FRN ||
			got.ParentFRN != want.ParentFRN ||
			got.Parent != want.Parent ||
			got.Name != want.Name ||
			got.Mode != want.Mode ||
			got.Size != want.Size ||
			got.ModUnix != want.ModUnix ||
			got.Deleted != want.Deleted {
			t.Fatalf("At(%d) = %+v, want preserved fields from %+v", i, got, want)
		}
	}
}

func TestPackedRecordsSetAndAppendUseNameBlob(t *testing.T) {
	packed := newPackedRecords([]CompactRecord{{FRN: 1, Name: "one"}})
	initialBlob := len(packed.NameBlob)
	packed.Set(0, CompactRecord{FRN: 2, Name: "two"})
	packed.Append(CompactRecord{FRN: 3, Name: "three"})

	if packed.Len() != 2 {
		t.Fatalf("Len = %d, want 2", packed.Len())
	}
	if got := packed.At(0); got.FRN != 2 || got.Name != "two" {
		t.Fatalf("At(0) = %+v, want updated record", got)
	}
	if got := packed.At(1); got.FRN != 3 || got.Name != "three" {
		t.Fatalf("At(1) = %+v, want appended record", got)
	}
	if len(packed.NameBlob) <= initialBlob {
		t.Fatalf("NameBlob len = %d, want growth beyond %d", len(packed.NameBlob), initialBlob)
	}
}

func TestNewPackedRecordsDeduplicatesNameBlob(t *testing.T) {
	packed := newPackedRecords([]CompactRecord{
		{FRN: 1, Name: "same.txt"},
		{FRN: 2, Name: "same.txt"},
		{FRN: 3, Name: "other.txt"},
	})

	if got, want := string(packed.NameBlob), "same.txtother.txt"; got != want {
		t.Fatalf("NameBlob = %q, want %q", got, want)
	}
	if packed.NameOffs[0] != packed.NameOffs[1] || packed.NameLens[0] != packed.NameLens[1] {
		t.Fatalf("duplicate names did not share offsets: off=%v len=%v", packed.NameOffs, packed.NameLens)
	}
}

func TestPackedRecordsAtBounds(t *testing.T) {
	var nilPacked *PackedRecords
	if got := nilPacked.At(0); got != (CompactRecord{}) {
		t.Fatalf("nil At(0) = %+v, want zero CompactRecord", got)
	}

	packed := newPackedRecords([]CompactRecord{{FRN: 1, Name: "one"}})
	for _, i := range []int{-1, 1, 2} {
		if got := packed.At(i); got != (CompactRecord{}) {
			t.Fatalf("At(%d) = %+v, want zero CompactRecord", i, got)
		}
	}
	if got := packed.At(0); got.FRN != 1 || got.Name != "one" {
		t.Fatalf("At(0) = %+v, want populated record", got)
	}
}

func TestIndexPackCompactRecordsDropRecordsKeepsPackedAccess(t *testing.T) {
	idx := &Index{
		Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: 1 << 31},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "two.txt", Size: 22},
		},
		CompactNameOrder: []int{1, 0},
	}

	beforeCount := idx.entryCount()
	idx.packCompactRecords(true)

	if idx.Records != nil {
		t.Fatalf("Records = %+v, want nil after dropRecords", idx.Records)
	}
	if idx.CompactNameOrder != nil {
		t.Fatalf("CompactNameOrder = %+v, want nil after dropRecords", idx.CompactNameOrder)
	}
	if idx.PackedRecords == nil {
		t.Fatal("PackedRecords is nil after packing")
	}
	if got := idx.entryCount(); got != beforeCount {
		t.Fatalf("entryCount = %d, want %d", got, beforeCount)
	}
	if got := idx.compactRecordCount(); got != beforeCount {
		t.Fatalf("compactRecordCount = %d, want %d", got, beforeCount)
	}
	if got := idx.compactRecord(1); got.FRN != 2 || got.Name != "two.txt" || got.Size != 22 {
		t.Fatalf("compactRecord(1) = %+v, want packed second record", got)
	}
}
