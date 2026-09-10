package main

import (
	"os"
	"path/filepath"
	"testing"
)

func liveMetadataVolume(t *testing.T, root string) *serviceVolumeIndex {
	t.Helper()
	idx := &Index{
		Version: indexVersionV9,
		Compact: true,
		Volume:  "C:",
		Roots:   []string{root},
		Records: []CompactRecord{
			{FRN: 1, Parent: -1, ParentFRN: 1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, Parent: 0, ParentFRN: 1, Name: "probe.bin"},
		},
	}
	vol := newServiceVolumeIndex("", idx)
	vol.volume = "C:"
	vol.frnToID = map[uint64]int{1: 0, 2: 1}
	return vol
}

// A rename/modify USN record carries no size, so the overlay must preserve the
// base record's size rather than reporting zero.
func TestOverlayChangePreservesBaseSizeWhenNoLiveStat(t *testing.T) {
	idx := &Index{
		Version: indexVersionV9,
		Compact: true,
		Volume:  "C:",
		Records: []CompactRecord{
			{FRN: 1, Parent: -1, ParentFRN: 1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, Parent: 0, ParentFRN: 1, Name: "keep.bin", Size: 4321, ModUnix: 999},
		},
	}
	vol := newServiceVolumeIndex("", idx)
	vol.frnToID = map[uint64]int{1: 0, 2: 1}
	// volume stays empty, so the live refresh is a no-op.
	vol.applyUSNChanges([]usnChange{{FRN: 2, ParentFRN: 1, USN: 10, Reason: usnReasonRenameNew, Name: "keep.bin"}})

	slot, ok := vol.overlay.byFRN[2]
	if !ok {
		t.Fatal("overlay slot missing for renamed file")
	}
	rec := vol.overlay.records[slot]
	if rec.Size != 4321 || rec.ModUnix != 999 {
		t.Fatalf("overlay size=%d mod=%d; want preserved 4321/999", rec.Size, rec.ModUnix)
	}
}

// A data change to a file must move the ancestor directory's recursive size by
// the file's size delta, and the reported directory entry must reflect it.
func TestLiveFolderSizeDeltaTracksFileChange(t *testing.T) {
	root := t.TempDir()
	probe := filepath.Join(root, "probe.bin")
	if err := os.WriteFile(probe, make([]byte, 777), 0o644); err != nil {
		t.Fatal(err)
	}
	idx := &Index{
		Version: indexVersionV9,
		Compact: true,
		Volume:  "C:",
		Roots:   []string{root},
		Records: []CompactRecord{
			{FRN: 1, Parent: -1, ParentFRN: 0, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, Parent: 0, ParentFRN: 1, Name: "probe.bin", Size: 100},
		},
	}
	// Base recursive sizes: root includes the file's original 100 bytes.
	idx.Derived.SubtreeBytes = []uint64{100, 100}
	vol := newServiceVolumeIndex("", idx)
	vol.volume = "C:"
	vol.frnToID = map[uint64]int{1: 0, 2: 1}
	vol.subtreeBytes = idx.Derived.SubtreeBytes

	vol.applyUSNChanges([]usnChange{{FRN: 2, ParentFRN: 1, USN: 10, Reason: usnReasonDataExtend, Name: "probe.bin"}})

	if got := vol.dirSizeDelta[0]; got != 677 {
		t.Fatalf("dir delta = %d, want 677 (777 live - 100 base)", got)
	}
	dirEntry := compactEntryFromRecord(idx, 0, idx.compactRecord(0), map[int]string{}, false)
	if dirEntry.Size != 777 {
		t.Fatalf("directory entry size = %d, want 777", dirEntry.Size)
	}
}

// Deleting a file must subtract its size from ancestor directories.
func TestLiveFolderSizeDeltaTracksDelete(t *testing.T) {
	idx := &Index{
		Version: indexVersionV9,
		Compact: true,
		Volume:  "C:",
		Records: []CompactRecord{
			{FRN: 1, Parent: -1, ParentFRN: 0, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, Parent: 0, ParentFRN: 1, Name: "gone.bin", Size: 512},
		},
	}
	idx.Derived.SubtreeBytes = []uint64{512, 512}
	vol := newServiceVolumeIndex("", idx)
	vol.frnToID = map[uint64]int{1: 0, 2: 1}
	vol.subtreeBytes = idx.Derived.SubtreeBytes

	vol.applyUSNChanges([]usnChange{{FRN: 2, ParentFRN: 1, USN: 10, Reason: usnReasonFileDelete, Name: "gone.bin"}})

	if got := vol.dirSizeDelta[0]; got != -512 {
		t.Fatalf("dir delta = %d, want -512", got)
	}
	dirEntry := compactEntryFromRecord(idx, 0, idx.compactRecord(0), map[int]string{}, false)
	if dirEntry.Size != 0 {
		t.Fatalf("directory entry size = %d, want 0 after delete", dirEntry.Size)
	}
}

func TestOverlayChangeRefreshesLiveFileSize(t *testing.T) {
	root := t.TempDir()
	probe := filepath.Join(root, "probe.bin")
	if err := os.WriteFile(probe, make([]byte, 777), 0o644); err != nil {
		t.Fatal(err)
	}
	vol := liveMetadataVolume(t, root)
	vol.applyUSNChanges([]usnChange{{FRN: 2, ParentFRN: 1, USN: 10, Reason: usnReasonDataExtend, Name: "probe.bin"}})

	slot, ok := vol.overlay.byFRN[2]
	if !ok {
		t.Fatal("overlay slot missing for modified file")
	}
	rec := vol.overlay.records[slot]
	if rec.Size != 777 {
		t.Fatalf("overlay size=%d; want live size 777", rec.Size)
	}
	if rec.ModUnix == 0 {
		t.Fatal("overlay mod time was not refreshed from the live file")
	}
}
