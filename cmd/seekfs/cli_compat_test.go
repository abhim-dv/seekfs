package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunTreatsCommandlessSearchAsSearch(t *testing.T) {
	root := t.TempDir()
	subdir := filepath.Join(root, "subdir")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(filepath.Join(root, "alpha-needle.txt"), "one")
	writeTestFile(filepath.Join(subdir, "beta-needle.log"), "two")

	idx := &Index{Source: "walk"}
	if err := walkRoot(root, idx); err != nil {
		t.Fatal(err)
	}
	buildOrders(idx)
	db := filepath.Join(root, "test.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatal(err)
	}

	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = stdout }()

	runErr := run([]string{"-db", db, "--under", subdir, "needle"})
	_ = w.Close()
	out, readErr := io.ReadAll(r)
	_ = r.Close()
	if runErr != nil {
		t.Fatalf("run returned error: %v", runErr)
	}
	if readErr != nil {
		t.Fatalf("read stdout: %v", readErr)
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) != 1 || !strings.Contains(lines[0], "beta-needle.log") {
		t.Fatalf("stdout = %q, want one beta-needle.log result", string(out))
	}
}

func TestRunSearchAcceptsFlagsAfterQuery(t *testing.T) {
	root := t.TempDir()
	subdir := filepath.Join(root, "subdir")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(filepath.Join(root, "alpha-needle.txt"), "one")
	writeTestFile(filepath.Join(subdir, "beta-needle.log"), "two")

	idx := &Index{Source: "walk"}
	if err := walkRoot(root, idx); err != nil {
		t.Fatal(err)
	}
	buildOrders(idx)
	db := filepath.Join(root, "test.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatal(err)
	}

	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = stdout }()

	runErr := run([]string{"search", "needle", "--under", subdir, "-n", "1", "-db", db})
	_ = w.Close()
	out, readErr := io.ReadAll(r)
	_ = r.Close()
	if runErr != nil {
		t.Fatalf("run returned error: %v", runErr)
	}
	if readErr != nil {
		t.Fatalf("read stdout: %v", readErr)
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) != 1 || !strings.Contains(lines[0], "beta-needle.log") {
		t.Fatalf("stdout = %q, want one beta-needle.log result", string(out))
	}
}

func TestRunTreatsSingleCommandlessQueryAsSearch(t *testing.T) {
	root := t.TempDir()
	writeTestFile := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(filepath.Join(root, "alpha_test.go"), "one")
	writeTestFile(filepath.Join(root, "alpha.txt"), "two")

	idx := &Index{Source: "walk"}
	if err := walkRoot(root, idx); err != nil {
		t.Fatal(err)
	}
	buildOrders(idx)
	db := filepath.Join(root, "test.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatal(err)
	}

	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = stdout }()

	runErr := run([]string{"-db", db, "*_test.go"})
	_ = w.Close()
	out, readErr := io.ReadAll(r)
	_ = r.Close()
	if runErr != nil {
		t.Fatalf("run returned error: %v", runErr)
	}
	if readErr != nil {
		t.Fatalf("read stdout: %v", readErr)
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) != 1 || !strings.Contains(lines[0], "alpha_test.go") {
		t.Fatalf("stdout = %q, want one alpha_test.go result", string(out))
	}
}
