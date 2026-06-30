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

func TestPackedRecordsSetUnchangedNameDoesNotGrowNameBlob(t *testing.T) {
	packed := newPackedRecords([]CompactRecord{{FRN: 1, ParentFRN: 1, Parent: -1, Name: "same.txt"}})
	initialNameBlob := len(packed.NameBlob)
	initialLowerBlob := len(packed.LowerBlob)

	for i := 0; i < 100; i++ {
		packed.Set(0, CompactRecord{
			FRN:       1,
			ParentFRN: 1,
			Parent:    -1,
			Name:      "same.txt",
			Mode:      uint32(i),
			Size:      int64(i),
			ModUnix:   int64(i + 1),
		})
	}

	if len(packed.NameBlob) != initialNameBlob {
		t.Fatalf("NameBlob grew from %d to %d for unchanged names", initialNameBlob, len(packed.NameBlob))
	}
	if len(packed.LowerBlob) != initialLowerBlob {
		t.Fatalf("LowerBlob grew from %d to %d for unchanged names", initialLowerBlob, len(packed.LowerBlob))
	}
	if got := packed.At(0); got.Mode != 99 || got.Size != 99 || got.ModUnix != 100 {
		t.Fatalf("At(0) = %+v, want metadata updates preserved", got)
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

func TestPackedRecordsAvoidsRedundantOptionalArrays(t *testing.T) {
	packed := newPackedRecords([]CompactRecord{
		{FRN: 1, Name: "lower.txt"},
		{FRN: 2, Name: "also-lower.go"},
	})
	if len(packed.LowerBlob) != 0 {
		t.Fatalf("LowerBlob len = %d, want 0 for already-lowercase names", len(packed.LowerBlob))
	}
	if packed.Size32 != nil {
		t.Fatalf("Size32 allocated for zero sizes")
	}
	if packed.ModUnix != nil {
		t.Fatalf("ModUnix allocated for zero modified times")
	}
	packed.Set(0, CompactRecord{FRN: 1, Name: "Mixed.TXT", Size: 42, ModUnix: 99})
	if got := packed.At(0); got.Name != "Mixed.TXT" || got.Size != 42 || got.ModUnix != 99 {
		t.Fatalf("At(0) = %+v, want updated optional fields", got)
	}
	if got := packed.lowerNameAt(0); got != "mixed.txt" {
		t.Fatalf("lowerNameAt(0) = %q, want mixed.txt", got)
	}
}

func TestPackedRecordsDerivesParentFRN(t *testing.T) {
	packed := newPackedRecords([]CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "."},
		{FRN: 11, ParentFRN: 10, Parent: 0, Name: "child"},
		{FRN: 12, ParentFRN: 99, Parent: -1, Name: "pending-parent"},
	})
	if got := packed.At(1); got.ParentFRN != 10 {
		t.Fatalf("derived child ParentFRN = %d, want 10", got.ParentFRN)
	}
	if got := packed.At(2); got.ParentFRN != 99 {
		t.Fatalf("extra ParentFRN = %d, want 99", got.ParentFRN)
	}
	if len(packed.ParentFRNExtras) != 1 {
		t.Fatalf("ParentFRNExtras len = %d, want 1", len(packed.ParentFRNExtras))
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

func TestIndexRepackCompactRecordsDeduplicatesUpdatedNames(t *testing.T) {
	idx := &Index{
		Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: "."},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "old.txt"},
			{FRN: 3, ParentFRN: 1, Parent: 0, Name: "old.txt"},
		},
	}
	idx.packCompactRecords(true)
	idx.setCompactRecord(1, CompactRecord{FRN: 2, ParentFRN: 1, Parent: 0, Name: "new.txt"})
	idx.setCompactRecord(2, CompactRecord{FRN: 3, ParentFRN: 1, Parent: 0, Name: "new.txt"})
	if len(idx.PackedRecords.NameBlob) <= len(".old.txtnew.txt") {
		t.Fatalf("test setup did not create stale name blob bytes: %q", string(idx.PackedRecords.NameBlob))
	}

	idx.repackCompactRecords()

	if got, want := string(idx.PackedRecords.NameBlob), ".new.txt"; got != want {
		t.Fatalf("NameBlob after repack = %q, want %q", got, want)
	}
	if got := idx.compactRecord(2).Name; got != "new.txt" {
		t.Fatalf("record name after repack = %q, want new.txt", got)
	}
}
