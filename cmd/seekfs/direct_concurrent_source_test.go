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

func TestDirectConcurrentWalkWorkerCountsHaveStableRecords(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 96; i++ {
		dir := filepath.Join(root, "d", "nested")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, filepath.Base(t.Name())+"-"+itoaForDirectTest(i)+".txt")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var want []directRecord
	for _, workers := range []int{1, 4, 8} {
		report := &directWalkReport{}
		source, err := newDirectConcurrentWalkSourceWithOptions(root, nil, nil, report, directConcurrentWalkOptions{Workers: workers, Queue: 3})
		if err != nil {
			t.Fatal(err)
		}
		got := collectDirectConcurrentRecords(t, source)
		if report.Skipped != 0 || report.Inaccessible != 0 || !report.SourceComplete {
			t.Fatalf("workers=%d report=%+v", workers, report)
		}
		sortDirectRecords(got)
		if want == nil {
			want = got
		} else if !sameDirectRecords(want, got) {
			for i := range want {
				if !sameDirectRecord(want[i], got[i]) {
					t.Fatalf("workers=%d changed record[%d]: want=%+v got=%+v", workers, i, want[i], got[i])
				}
			}
			t.Fatalf("workers=%d changed records", workers)
		}
	}
}

func TestDirectConcurrentWalkHonorsExclusionsAndSuffixes(t *testing.T) {
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
	report := &directWalkReport{}
	source, err := newDirectConcurrentWalkSourceWithExclusions(root, []string{owned}, []string{".gsi"}, report, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	got := collectDirectConcurrentRecords(t, source)
	if len(got) != 2 || report.Excluded < 2 || !report.SourceComplete {
		t.Fatalf("records/report=%d/%+v", len(got), report)
	}
}

func TestDirectConcurrentWalkCancellationClosesBoundedPipeline(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 512; i++ {
		path := filepath.Join(root, "file-"+itoaForDirectTest(i))
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	source, err := newDirectConcurrentWalkSourceWithOptions(root, nil, nil, nil, directConcurrentWalkOptions{Workers: 8, Queue: 1})
	if err != nil {
		t.Fatal(err)
	}
	walk := source.(*directConcurrentWalkSource)
	if _, err := source.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	walk.Close()
	if _, err := source.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("after Close Next error=%v, want EOF", err)
	}
}

func TestDirectConcurrentWalkCloseDiscardsBufferedRoot(t *testing.T) {
	source, err := newDirectConcurrentWalkSourceWithOptions(t.TempDir(), nil, nil, nil, directConcurrentWalkOptions{Workers: 1, Queue: 1})
	if err != nil {
		t.Fatal(err)
	}
	walk := source.(*directConcurrentWalkSource)
	<-walk.finish
	if got := len(walk.records); got != 1 {
		t.Fatalf("buffered records=%d, want root record", got)
	}
	walk.Close()
	if _, err := walk.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("after Close Next error=%v, want EOF", err)
	}
}

func TestDirectConcurrentWalkIndexesReparseEntriesWithoutFollowingTargets(t *testing.T) {
	// The walk canonicalizes its root (EvalSymlinks), so build and compare paths
	// in canonical space; a symlinked or short-named temp root would otherwise
	// make the walk's record paths alias the raw t.TempDir() paths.
	root, err := directCanonicalPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	external, err := directCanonicalPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
	if err := makeDirectJunction(junction, filepath.Join(external, "target-dir")); err != nil {
		t.Skipf("directory junction unavailable: %v", err)
	}
	if err := makeDirectJunction(cycle, root); err != nil {
		_ = os.Remove(junction)
		t.Skipf("cycle junction unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(cycle); _ = os.Remove(junction) })

	wantHash := ""
	for _, workers := range []int{1, 4, 8} {
		report := &directWalkReport{}
		source, err := newDirectConcurrentWalkSourceWithOptions(root, nil, nil, report, directConcurrentWalkOptions{Workers: workers, Queue: workers * 2})
		if err != nil {
			t.Fatal(err)
		}
		records := collectDirectConcurrentRecords(t, source)
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
		report = &directWalkReport{}
		source, err = newDirectConcurrentWalkSourceWithOptions(root, nil, nil, report, directConcurrentWalkOptions{Workers: workers, Queue: workers * 2})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := buildDirect(context.Background(), directBuildOptions{
			OutputPath: filepath.Join(dir, "walk.gsi"), SpoolDir: filepath.Join(dir, "spool"), Records: source,
			RunRecords: 8, RankWorkers: workers, WalkReport: report, Source: "direct-reparse-test",
		}); err != nil {
			t.Fatal(err)
		}
		gotHash := directFileHash(t, filepath.Join(dir, "walk.gsi"))
		if wantHash == "" {
			wantHash = gotHash
		} else if gotHash != wantHash {
			t.Fatalf("workers=%d changed deterministic output: got=%s want=%s", workers, gotHash, wantHash)
		}
	}
}

func makeDirectJunction(path, target string) error {
	if filepath.Separator != '\\' {
		return errors.New("directory junctions require Windows")
	}
	cmd := exec.Command("cmd.exe", "/c", "mklink", "/J", path, target)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mklink /J failed: %w: %s", err, output)
	}
	return nil
}

func collectDirectConcurrentRecords(t *testing.T, source directRecordSource) []directRecord {
	t.Helper()
	defer func() {
		if closeable, ok := source.(interface{ Close() }); ok {
			closeable.Close()
		}
	}()
	var records []directRecord
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

func sortDirectRecords(records []directRecord) {
	sort.Slice(records, func(i, j int) bool {
		if records[i].FRN != records[j].FRN {
			return records[i].FRN < records[j].FRN
		}
		return records[i].Path < records[j].Path
	})
}

func sameDirectRecords(a, b []directRecord) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameDirectRecord(a[i], b[i]) {
			return false
		}
	}
	return true
}

func sameDirectRecord(a, b directRecord) bool {
	return a.FRN == b.FRN && a.ParentFRN == b.ParentFRN && a.Mode == b.Mode &&
		a.Size == b.Size && a.ModUnix == b.ModUnix && a.Name == b.Name && a.Path == b.Path
}

func itoaForDirectTest(n int) string {
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
