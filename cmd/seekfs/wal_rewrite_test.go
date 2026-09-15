package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A corrupt tail must not block the post-fold rewrite: the readable prefix is
// what has to survive, and rewriting it drops the WAL back under its size
// trigger.  Skipping the rewrite instead leaves the WAL oversized, so the next
// fold is due immediately and folds run back to back.
func TestReadWALFramesAfterToleratesCorruptTail(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "seekfs_c.gsi")
	if err := appendWAL(db, 100, []usnChange{
		{FRN: 10, ParentFRN: 1, USN: 100, Reason: usnReasonFileCreate, Name: "a.txt"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := appendWAL(db, 200, []usnChange{
		{FRN: 11, ParentFRN: 1, USN: 200, Reason: usnReasonFileCreate, Name: "b.txt"},
	}); err != nil {
		t.Fatal(err)
	}

	path := walPath(db)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xFF
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	frames, err := readWALFramesAfter(db, 0)
	if err == nil {
		t.Fatal("expected a read error for the corrupt final frame")
	}
	if len(frames) != 1 || frames[0].NextUSN != 100 {
		t.Fatalf("readable frames = %v, want only the good frame (nextUSN=100)", frames)
	}

	if err := rewriteWAL(db, frames); err != nil {
		t.Fatalf("rewriteWAL: %v", err)
	}
	after, err := readWALFramesAfter(db, 0)
	if err != nil {
		t.Fatalf("rewritten wal must read cleanly: %v", err)
	}
	if len(after) != 1 || after[0].NextUSN != 100 {
		t.Fatalf("rewritten frames = %v, want only nextUSN=100", after)
	}
}

// No readable frames at all still resets the WAL instead of leaving an
// unreadable file above the fold trigger.
func TestRewriteWALWithNoFramesResetsTheWAL(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "seekfs_c.gsi")
	if err := appendWAL(db, 100, []usnChange{
		{FRN: 10, ParentFRN: 1, USN: 100, Reason: usnReasonFileCreate, Name: "a.txt"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := rewriteWAL(db, nil); err != nil {
		t.Fatalf("rewriteWAL: %v", err)
	}
	frames, err := readWALFramesAfter(db, 0)
	if err != nil {
		t.Fatalf("reset wal must read cleanly: %v", err)
	}
	if len(frames) != 0 {
		t.Fatalf("reset wal frames = %v, want none", frames)
	}
}
