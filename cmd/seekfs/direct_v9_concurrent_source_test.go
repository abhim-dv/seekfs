package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
)

func TestDirectV9ConcurrentWalkWorkerCountsHaveStableRecords(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 96; i++ {
		dir := filepath.Join(root, "d", "nested")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, filepath.Base(t.Name())+"-"+itoaForDirectV9Test(i)+".txt")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var want []directV9Record
	for _, workers := range []int{1, 4, 8} {
		report := &directV9WalkReport{}
		source, err := newDirectV9ConcurrentWalkSourceWithOptions(root, nil, nil, report, directV9ConcurrentWalkOptions{Workers: workers, Queue: 3})
		if err != nil {
			t.Fatal(err)
		}
		got := collectDirectV9ConcurrentRecords(t, source)
		if report.Skipped != 0 || report.Inaccessible != 0 || !report.SourceComplete {
			t.Fatalf("workers=%d report=%+v", workers, report)
		}
		sortDirectV9Records(got)
		if want == nil {
			want = got
		} else if !sameDirectV9Records(want, got) {
			for i := range want {
				if !sameDirectV9Record(want[i], got[i]) {
					t.Fatalf("workers=%d changed record[%d]: want=%+v got=%+v", workers, i, want[i], got[i])
				}
			}
			t.Fatalf("workers=%d changed records", workers)
		}
	}
}

func TestDirectV9ConcurrentWalkHonorsExclusionsAndSuffixes(t *testing.T) {
	root := t.TempDir()
	owned := filepath.Join(root, ".r5tmp")
	if err := os.MkdirAll(filepath.Join(owned, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{
		filepath.Join(root, "keep.txt"):               "keep",
		filepath.Join(root, "skip.gsi"):               "skip",
		filepath.Join(owned, "nested", "builder.tmp"): "owned",
	} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	report := &directV9WalkReport{}
	source, err := newDirectV9ConcurrentWalkSourceWithExclusions(root, []string{owned}, []string{".gsi"}, report, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	got := collectDirectV9ConcurrentRecords(t, source)
	if len(got) != 2 || report.Excluded < 2 || !report.SourceComplete {
		t.Fatalf("records/report=%d/%+v", len(got), report)
	}
}

func TestDirectV9ConcurrentWalkCancellationClosesBoundedPipeline(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 512; i++ {
		path := filepath.Join(root, "file-"+itoaForDirectV9Test(i))
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	source, err := newDirectV9ConcurrentWalkSourceWithOptions(root, nil, nil, nil, directV9ConcurrentWalkOptions{Workers: 8, Queue: 1})
	if err != nil {
		t.Fatal(err)
	}
	walk := source.(*directV9ConcurrentWalkSource)
	if _, err := source.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	walk.Close()
	if _, err := source.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("after Close Next error=%v, want EOF", err)
	}
}

func TestDirectV9ConcurrentWalkCloseDiscardsBufferedRoot(t *testing.T) {
	source, err := newDirectV9ConcurrentWalkSourceWithOptions(t.TempDir(), nil, nil, nil, directV9ConcurrentWalkOptions{Workers: 1, Queue: 1})
	if err != nil {
		t.Fatal(err)
	}
	walk := source.(*directV9ConcurrentWalkSource)
	<-walk.finish
	if got := len(walk.records); got != 1 {
		t.Fatalf("buffered records=%d, want root record", got)
	}
	walk.Close()
	if _, err := walk.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("after Close Next error=%v, want EOF", err)
	}
}

func TestDirectV9ConcurrentWalkIndexesReparseEntriesWithoutFollowingTargets(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "outside.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(external, "target-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, "target-dir", "child.txt"), []byte("child"), 0o600); err != nil {
		t.Fatal(err)
	}

	fileLink := filepath.Join(root, "file-link")
	if err := os.Symlink(filepath.Join(external, "outside.txt"), fileLink); err != nil {
		t.Logf("file symlink unavailable; continuing with junction coverage: %v", err)
		fileLink = ""
	} else {
		t.Cleanup(func() { _ = os.Remove(fileLink) })
	}
	junction := filepath.Join(root, "junction")
	cycle := filepath.Join(root, "cycle")
	if err := makeDirectV9Junction(junction, filepath.Join(external, "target-dir")); err != nil {
		t.Skipf("directory junction unavailable: %v", err)
	}
	if err := makeDirectV9Junction(cycle, root); err != nil {
		_ = os.Remove(junction)
		t.Skipf("cycle junction unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(cycle); _ = os.Remove(junction) })

	wantHash := ""
	for _, workers := range []int{1, 4, 8} {
		report := &directV9WalkReport{}
		source, err := newDirectV9ConcurrentWalkSourceWithOptions(root, nil, nil, report, directV9ConcurrentWalkOptions{Workers: workers, Queue: workers * 2})
		if err != nil {
			t.Fatal(err)
		}
		records := collectDirectV9ConcurrentRecords(t, source)
		if !report.SourceComplete || report.Inaccessible != 0 || report.Skipped != 0 || report.ReparseNotFollowed < 2 {
			t.Fatalf("workers=%d report=%+v", workers, report)
		}
		paths := make(map[string]bool, len(records))
		for _, record := range records {
			paths[filepath.Clean(record.Path)] = true
			if filepath.Clean(record.Path) == filepath.Clean(filepath.Join(external, "target-dir", "child.txt")) || filepath.Clean(record.Path) == filepath.Clean(filepath.Join(external, "outside.txt")) {
				t.Fatalf("workers=%d followed external target: %s", workers, record.Path)
			}
		}
		if !paths[filepath.Clean(junction)] || !paths[filepath.Clean(cycle)] {
			t.Fatalf("workers=%d omitted reparse entries: paths=%v report=%+v", workers, paths, report)
		}
		if fileLink != "" && !paths[filepath.Clean(fileLink)] {
			t.Fatalf("workers=%d omitted file symlink", workers)
		}

		dir := t.TempDir()
		report = &directV9WalkReport{}
		source, err = newDirectV9ConcurrentWalkSourceWithOptions(root, nil, nil, report, directV9ConcurrentWalkOptions{Workers: workers, Queue: workers * 2})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := buildDirectV9(context.Background(), directV9BuildOptions{
			OutputPath: filepath.Join(dir, "walk.gsi"), SpoolDir: filepath.Join(dir, "spool"), Records: source,
			RunRecords: 8, RankWorkers: workers, WalkReport: report, Source: "direct-reparse-test",
		}); err != nil {
			t.Fatal(err)
		}
		gotHash := directV9FileHash(t, filepath.Join(dir, "walk.gsi"))
		if wantHash == "" {
			wantHash = gotHash
		} else if gotHash != wantHash {
			t.Fatalf("workers=%d changed deterministic output: got=%s want=%s", workers, gotHash, wantHash)
		}
	}
}

func makeDirectV9Junction(path, target string) error {
	if filepath.Separator != '\\' {
		return errors.New("directory junctions require Windows")
	}
	cmd := exec.Command("cmd.exe", "/c", "mklink", "/J", path, target)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mklink /J failed: %w: %s", err, output)
	}
	return nil
}

func collectDirectV9ConcurrentRecords(t *testing.T, source directV9RecordSource) []directV9Record {
	t.Helper()
	defer func() {
		if closeable, ok := source.(interface{ Close() }); ok {
			closeable.Close()
		}
	}()
	var records []directV9Record
	for {
		record, err := source.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return records
		}
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
}

func sortDirectV9Records(records []directV9Record) {
	sort.Slice(records, func(i, j int) bool {
		if records[i].FRN != records[j].FRN {
			return records[i].FRN < records[j].FRN
		}
		return records[i].Path < records[j].Path
	})
}

func sameDirectV9Records(a, b []directV9Record) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameDirectV9Record(a[i], b[i]) {
			return false
		}
	}
	return true
}

func sameDirectV9Record(a, b directV9Record) bool {
	return a.FRN == b.FRN && a.ParentFRN == b.ParentFRN && a.Mode == b.Mode &&
		a.Size == b.Size && a.ModUnix == b.ModUnix && a.Name == b.Name && a.Path == b.Path
}

func itoaForDirectV9Test(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf[i:])
}
