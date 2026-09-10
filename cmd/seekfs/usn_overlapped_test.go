package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestUSNOverlappedReaderLive exercises the cancelable overlapped reader
// against the live system volume.  Raw volume access needs an elevated
// process, so the test is opt-in via SEEKFS_LIVE_USN_TEST=1 and skips (rather
// than fails) when the volume cannot be opened.  Run it from an elevated shell:
//
//	$env:SEEKFS_LIVE_USN_TEST = "1"; go test ./cmd/seekfs -run TestUSNOverlappedReaderLive -v
//
// SEEKFS_LIVE_USN_VOLUME selects the volume (default C:); the probe file is
// created in the process temp directory, which must live on that volume.
func TestUSNOverlappedReaderLive(t *testing.T) {
	if os.Getenv("SEEKFS_LIVE_USN_TEST") != "1" {
		t.Skip("set SEEKFS_LIVE_USN_TEST=1 (elevated) to run the live USN test")
	}
	volume := os.Getenv("SEEKFS_LIVE_USN_VOLUME")
	if volume == "" {
		volume = "C:"
	}
	queryHandle, err := openVolume(volume)
	if err != nil {
		t.Skipf("cannot open %s (need elevation): %v", volume, err)
	}
	defer windows.CloseHandle(queryHandle)
	journal, err := queryUSNJournal(queryHandle)
	if err != nil {
		t.Fatalf("queryUSNJournal(%s): %v", volume, err)
	}

	reader, err := openUSNOverlappedReader(volume)
	if err != nil {
		t.Fatalf("openUSNOverlappedReader(%s): %v", volume, err)
	}
	defer reader.close()

	// Cancellation must not wait for a filesystem change.  On a quiet journal
	// the probe returns canceled; on a busy one it may return data first, so
	// only the promptness bound is asserted.
	start := time.Now()
	_, _, canceled, err := reader.read(journal.UsnJournalID, journal.NextUsn, make([]byte, 1<<20), func() bool { return true })
	if err != nil {
		t.Fatalf("cancel probe error: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("cancel probe took %s; cancellation is not prompt", elapsed)
	}
	t.Logf("cancel probe: canceled=%v elapsed=%s", canceled, elapsed)

	// A created file must surface through the blocking read within the
	// deadline.  Start from the journal head captured before the write.
	const name = "seekfs-usn-live-probe.txt"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("seekfs"), 0o644); err != nil {
		t.Fatalf("write probe file: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	from := journal.NextUsn
	buffer := make([]byte, 4<<20)
	for time.Now().Before(deadline) {
		next, changes, canceled, err := reader.read(journal.UsnJournalID, from, buffer, func() bool { return time.Now().After(deadline) })
		if err != nil {
			t.Fatalf("live read error: %v", err)
		}
		if canceled {
			break
		}
		for _, change := range changes {
			if change.Name == name {
				t.Logf("observed %s after %s (next_usn=%d)", name, time.Since(start), next)
				return
			}
		}
		if next > from {
			from = next
		}
	}
	t.Fatalf("did not observe %s within %s", name, time.Since(start))
}
