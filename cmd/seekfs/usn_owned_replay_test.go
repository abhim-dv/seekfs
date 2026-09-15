package main

import (
	"os"
	"path/filepath"
	"testing"
)

// newOwnedReplayTestVolume returns a volume index whose owned set is one
// artifact directory (FRN 5000) and one unrelated user directory (FRN 6000),
// so the filter can be exercised without touching the real seekfs dir.
func newOwnedReplayTestVolume(t *testing.T) *serviceVolumeIndex {
	t.Helper()
	idx := &Index{
		Version: indexVersionV9,
		Compact: true,
		Volume:  "C:",
		Records: []CompactRecord{
			{FRN: 1, Parent: -1, ParentFRN: 0, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 6000, Parent: 0, ParentFRN: 1, Name: "users", Mode: uint32(os.ModeDir)},
			{FRN: 6001, Parent: 1, ParentFRN: 6000, Name: "notes.txt"},
		},
	}
	vol := newServiceVolumeIndex("", idx)
	vol.ownedDirFRNs = map[uint64]struct{}{5000: {}}
	return vol
}

// A change whose parent is an owned artifact directory must never reach the
// overlay, while an ordinary change must, and the checkpoint must advance over
// both so replay does not re-read the skipped records.
func TestApplyUSNChangesSkipsOwnedArtifactDirs(t *testing.T) {
	vol := newOwnedReplayTestVolume(t)
	vol.applyUSNChanges([]usnChange{
		{FRN: 5001, ParentFRN: 5000, USN: 10, Reason: usnReasonFileCreate, Attr: fileAttributeArchive, Name: "seekfs_c.gsi.tmp"},
		{FRN: 6002, ParentFRN: 6000, USN: 11, Reason: usnReasonFileCreate, Attr: fileAttributeArchive, Name: "fresh.txt"},
	})
	if vol.checkpoint != 11 {
		t.Fatalf("checkpoint = %d, want 11 (skipped records must still advance it)", vol.checkpoint)
	}
	if vol.overlay == nil {
		t.Fatal("overlay is nil; the user change should have created it")
	}
	if _, ok := vol.overlay.byFRN[5001]; ok {
		t.Fatal("artifact change entered the overlay")
	}
	if _, ok := vol.overlay.byFRN[6002]; !ok {
		t.Fatal("user change did not enter the overlay")
	}
}

// The filter drops owned changes before the WAL/apply split and covers
// directories created under an owned directory at runtime: the builder's
// MkdirTemp scratch dir, and therefore its spill files, are excluded too.
func TestFilterOwnedReplayChangesTracksScratchDirs(t *testing.T) {
	vol := newOwnedReplayTestVolume(t)
	spool := usnChange{FRN: 5001, ParentFRN: 5000, USN: 2, Reason: usnReasonFileCreate, Attr: fileAttributeDir, Name: "gram-spool"}
	scratch := usnChange{FRN: 5002, ParentFRN: 5001, USN: 3, Reason: usnReasonFileCreate, Attr: fileAttributeDir, Name: "seekfs-name-gram-123"}
	spill := usnChange{FRN: 5003, ParentFRN: 5002, USN: 4, Reason: usnReasonFileCreate, Attr: fileAttributeArchive, Name: "run-000.bin"}
	user := usnChange{FRN: 6003, ParentFRN: 6000, USN: 5, Reason: usnReasonFileCreate, Attr: fileAttributeArchive, Name: "notes2.txt"}

	// The seed for the dynamic set: the spool dir directly under the owned
	// seekfs dir.  The scratch dir then arrives under the spool, and the spill
	// file under the scratch dir.
	kept := vol.filterOwnedReplayChanges([]usnChange{spool, scratch, spill, user})
	if len(kept) != 1 || kept[0].FRN != 6003 {
		t.Fatalf("kept %d changes (%v), want only the user file", len(kept), kept)
	}
	if !vol.ownedReplayParent(5002) {
		t.Fatal("scratch dir was not tracked, so its spill files would be indexed")
	}

	// The builder removes the scratch dir; its reference must retire so a
	// reused reference cannot hide real files.
	gone := usnChange{FRN: 5002, ParentFRN: 5001, USN: 6, Reason: usnReasonFileDelete, Attr: fileAttributeDir, Name: "seekfs-name-gram-123"}
	if kept := vol.filterOwnedReplayChanges([]usnChange{gone}); len(kept) != 0 {
		t.Fatalf("scratch dir delete was not consumed: %v", kept)
	}
	if vol.ownedReplayParent(5002) {
		t.Fatal("removed scratch dir still filters its reference")
	}
	reused := usnChange{FRN: 5002, ParentFRN: 6000, USN: 7, Reason: usnReasonFileCreate, Attr: fileAttributeDir, Name: "reused"}
	inside := usnChange{FRN: 6004, ParentFRN: 5002, USN: 8, Reason: usnReasonFileCreate, Attr: fileAttributeArchive, Name: "real.txt"}
	if kept := vol.filterOwnedReplayChanges([]usnChange{reused, inside}); len(kept) != 2 {
		t.Fatalf("changes under a reused reference were filtered: %v", kept)
	}
}

// A directory that moves out of the owned tree retires on its rename-out
// record, so the file now living under it is indexed.
func TestFilterOwnedReplayChangesRetiresMovedOutDir(t *testing.T) {
	vol := newOwnedReplayTestVolume(t)
	scratch := usnChange{FRN: 5002, ParentFRN: 5000, USN: 1, Reason: usnReasonFileCreate, Attr: fileAttributeDir, Name: "seekfs-name-gram-9"}
	if kept := vol.filterOwnedReplayChanges([]usnChange{scratch}); len(kept) != 0 {
		t.Fatalf("scratch dir create was not consumed: %v", kept)
	}
	moveAway := usnChange{FRN: 5002, ParentFRN: 6000, USN: 2, Reason: usnReasonRenameNew, Attr: fileAttributeDir, Name: "kept"}
	inside := usnChange{FRN: 6005, ParentFRN: 5002, USN: 3, Reason: usnReasonFileCreate, Attr: fileAttributeArchive, Name: "real.txt"}
	if kept := vol.filterOwnedReplayChanges([]usnChange{moveAway, inside}); len(kept) != 2 {
		t.Fatalf("dir moved out of the owned tree still filters: %v", kept)
	}
}

// A volume with no artifacts on it must stay on the zero-cost path: the
// filter returns the batch unchanged so ordinary volumes pay only a map check.
func TestFilterOwnedReplayChangesNoOwnedDirsPreservesBatch(t *testing.T) {
	vol := newOwnedReplayTestVolume(t)
	vol.ownedDirFRNs = nil
	changes := []usnChange{
		{FRN: 6002, ParentFRN: 6000, USN: 10, Reason: usnReasonFileCreate, Name: "a.txt"},
		{FRN: 6003, ParentFRN: 6000, USN: 11, Reason: usnReasonFileCreate, Name: "b.txt"},
	}
	kept := vol.filterOwnedReplayChanges(changes)
	if len(kept) != len(changes) {
		t.Fatalf("kept %d of %d changes on a volume without owned dirs", len(kept), len(changes))
	}
}

func TestOverlayCompactionSlotLimitScalesWithRecords(t *testing.T) {
	if got := overlayCompactionSlotLimitFor(0); got != overlayCompactionMaxSlots {
		t.Fatalf("empty index limit = %d, want floor %d", got, overlayCompactionMaxSlots)
	}
	if got := overlayCompactionSlotLimitFor(overlayCompactionMaxSlots); got != overlayCompactionMaxSlots {
		t.Fatalf("small index limit = %d, want floor %d", got, overlayCompactionMaxSlots)
	}
	records := overlayCompactionMaxSlots * overlayCompactionSlotFraction * 2
	if got, want := overlayCompactionSlotLimitFor(records), records/overlayCompactionSlotFraction; got != want {
		t.Fatalf("large index limit = %d, want %d", got, want)
	}
}

// ownedReplayDirFRNs resolves the seekfs dir tree to live NTFS references,
// including the name-gram spool, and leaves unrelated directories alone.
func TestOwnedReplayDirFRNsCollectsSeekFSDirTree(t *testing.T) {
	root := t.TempDir()
	seekfsDir := filepath.Join(root, "seekfs")
	nested := filepath.Join(seekfsDir, "indexes", "leaf")
	spool := filepath.Join(seekfsDir, "gram-spool")
	other := filepath.Join(root, "documents")
	for _, dir := range []string{nested, spool, other} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("SEEKFS_DIR", seekfsDir)
	t.Setenv("SEEKFS_NAME_GRAM_SPOOL_DIR", spool)

	volume := normalizeVolume(filepath.VolumeName(root))
	if len(volume) != 2 {
		t.Skipf("temp dir %q is not on a local volume", root)
	}
	owned := ownedReplayDirFRNs(volume)
	if len(owned) == 0 {
		t.Fatalf("no owned dirs collected for %s (volume %s)", seekfsDir, volume)
	}
	for _, dir := range []string{seekfsDir, nested, spool} {
		frn, err := ntfsFileReference(dir)
		if err != nil {
			t.Fatalf("ntfsFileReference(%s): %v", dir, err)
		}
		if _, ok := owned[frn]; !ok {
			t.Fatalf("missing owned dir %s (frn %d)", dir, frn)
		}
	}
	if frn, err := ntfsFileReference(other); err == nil {
		if _, ok := owned[frn]; ok {
			t.Fatalf("unrelated dir %s was collected as owned", other)
		}
	}
}
