package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stageIndexFile temporaries left behind by an interrupted persist must be
// reaped, while a temp that could still belong to a live persist survives.
func TestSweepStaleIndexTempFilesReapsOldTempsOnly(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "seekfs_c.gsi")
	if err := os.WriteFile(db, []byte("index"), 0o644); err != nil {
		t.Fatal(err)
	}
	abandoned := db + ".1817372077.tmp"
	live := db + ".3569482996.tmp"
	for _, p := range []string{abandoned, live} {
		if err := os.WriteFile(p, []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(abandoned, stale, stale); err != nil {
		t.Fatal(err)
	}

	s := &goSearchService{dbs: []string{db}}
	s.sweepStaleIndexTempFiles()

	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatalf("abandoned temp %s was not reaped (err=%v)", abandoned, err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("recent temp %s must survive: %v", live, err)
	}
}

// The periodic sweep must run on the interval, not on every loop tick.
func TestSweepStaleIndexTempFilesIfDueRunsOncePerInterval(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "seekfs_c.gsi")
	if err := os.WriteFile(db, []byte("index"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &goSearchService{dbs: []string{db}}
	now := time.Now()
	s.sweepStaleIndexTempFilesIfDue(now)

	abandoned := db + ".7.tmp"
	if err := os.WriteFile(abandoned, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(abandoned, stale, stale); err != nil {
		t.Fatal(err)
	}
	s.sweepStaleIndexTempFilesIfDue(now.Add(time.Minute))
	if _, err := os.Stat(abandoned); err != nil {
		t.Fatalf("sweep ran again inside the interval: %v", err)
	}
	s.sweepStaleIndexTempFilesIfDue(now.Add(staleTempSweepInterval + time.Second))
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatalf("sweep did not run after the interval (err=%v)", err)
	}
}
