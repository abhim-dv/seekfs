package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestCompactIndexV8RoundTripKeepsFRNMetadata(t *testing.T) {
	builtAt := time.Unix(0, 123456789)
	modified := time.Unix(0, 987654321)
	idx := &Index{
		Version:    indexVersion,
		Roots:      []string{`C:\`},
		BuiltAt:    builtAt,
		Source:     "usn",
		Volume:     "C:",
		JournalID:  42,
		Checkpoint: 99,
		Compact:    true,
		Records: []CompactRecord{
			{
				FRN:       10,
				ParentFRN: 10,
				Parent:    -1,
				Name:      ".",
				Mode:      uint32(1 << 31),
				Size:      0,
				ModUnix:   modified.UnixNano(),
			},
			{
				FRN:       11,
				ParentFRN: 10,
				Parent:    0,
				Name:      "main.go",
				Mode:      0,
				Size:      1234,
				ModUnix:   modified.Add(time.Second).UnixNano(),
				Deleted:   true,
			},
		},
	}

	var buf bytes.Buffer
	if err := writeIndex(&buf, idx); err != nil {
		t.Fatalf("writeIndex: %v", err)
	}
	got, err := readIndex(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("readIndex: %v", err)
	}

	if got.Version != indexVersion {
		t.Fatalf("Version = %d, want %d", got.Version, indexVersion)
	}
	if got.Source != "usn" || got.Volume != "C:" || got.JournalID != 42 || got.Checkpoint != 99 {
		t.Fatalf("index metadata was not preserved: %+v", got)
	}
	if !got.Compact || len(got.Records) != 2 {
		t.Fatalf("records = %d compact=%v, want 2 compact records", len(got.Records), got.Compact)
	}

	root := got.Records[0]
	if root.FRN != 10 || root.ParentFRN != 10 || root.Parent != -1 || root.Mode != uint32(1<<31) || root.ModUnix != modified.UnixNano() {
		t.Fatalf("root record metadata mismatch: %+v", root)
	}
	file := got.Records[1]
	if file.FRN != 11 || file.ParentFRN != 10 || file.Parent != 0 || file.Name != "main.go" || file.Size != 1234 || file.ModUnix != modified.Add(time.Second).UnixNano() || !file.Deleted {
		t.Fatalf("file record metadata mismatch: %+v", file)
	}
	if got.reconstructCompactPath(1) != `C:\.\main.go` {
		t.Fatalf("path = %q", got.reconstructCompactPath(1))
	}
}

func TestServiceVolumeIndexAppliesUSNMutations(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: "."},
		},
	}
	vol := newServiceVolumeIndex(`F:\seekfs_f.gsi`, idx)

	vol.applyUSNChanges([]usnChange{{
		FRN:       101,
		ParentFRN: 100,
		USN:       10,
		Reason:    usnReasonFileCreate,
		Name:      "old.txt",
	}})
	if len(idx.Records) != 2 {
		t.Fatalf("records after create = %d, want 2", len(idx.Records))
	}
	if idx.Records[1].Name != "old.txt" || idx.Records[1].Parent != 0 || idx.Records[1].Deleted {
		t.Fatalf("created record mismatch: %+v", idx.Records[1])
	}

	vol.applyUSNChanges([]usnChange{{
		FRN:       101,
		ParentFRN: 100,
		USN:       11,
		Reason:    usnReasonRenameOld,
		Name:      "old.txt",
	}, {
		FRN:       101,
		ParentFRN: 100,
		USN:       12,
		Reason:    usnReasonRenameNew,
		Name:      "new.txt",
	}})
	if idx.Records[1].Name != "new.txt" || idx.Records[1].Deleted {
		t.Fatalf("renamed record mismatch: %+v", idx.Records[1])
	}

	vol.applyUSNChanges([]usnChange{{
		FRN:    101,
		USN:    13,
		Reason: usnReasonFileDelete,
	}})
	if !idx.Records[1].Deleted {
		t.Fatalf("deleted record was not tombstoned: %+v", idx.Records[1])
	}
	if vol.checkpoint != 13 || idx.Checkpoint != 13 {
		t.Fatalf("checkpoint = vol %d idx %d, want 13", vol.checkpoint, idx.Checkpoint)
	}
}

func TestServiceVolumeIndexRepairsOutOfOrderParents(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: "."},
		},
	}
	vol := newServiceVolumeIndex(`F:\seekfs_f.gsi`, idx)

	vol.applyUSNChanges([]usnChange{{
		FRN:       201,
		ParentFRN: 200,
		USN:       10,
		Reason:    usnReasonFileCreate,
		Name:      "child.txt",
	}, {
		FRN:       200,
		ParentFRN: 100,
		USN:       11,
		Reason:    usnReasonFileCreate,
		Attr:      fileAttributeDir,
		Name:      "parent",
	}})

	if len(idx.Records) != 3 {
		t.Fatalf("records = %d, want 3", len(idx.Records))
	}
	child := idx.Records[1]
	if child.Parent != 2 {
		t.Fatalf("child parent = %d, want parent record 2: %+v", child.Parent, child)
	}
	if got := idx.reconstructCompactPath(1); got != `F:\.\parent\child.txt` {
		t.Fatalf("path = %q", got)
	}
}

func TestServiceVolumeIndexDeletesDirectorySubtree(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: "."},
			{FRN: 200, ParentFRN: 100, Parent: 0, Name: "dir"},
			{FRN: 201, ParentFRN: 200, Parent: 1, Name: "child.txt"},
		},
	}
	vol := newServiceVolumeIndex(`F:\seekfs_f.gsi`, idx)

	vol.applyUSNChanges([]usnChange{{
		FRN:    200,
		USN:    12,
		Reason: usnReasonFileDelete,
	}})

	if !idx.Records[1].Deleted || !idx.Records[2].Deleted {
		t.Fatalf("directory subtree was not tombstoned: dir=%+v child=%+v", idx.Records[1], idx.Records[2])
	}
}

func TestServiceVolumeIndexReplaysWAL(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "test.gsi")
	if err := appendWAL(db, 11, []usnChange{{
		FRN:       101,
		ParentFRN: 100,
		USN:       11,
		Reason:    usnReasonFileCreate,
		Name:      "wal-created.txt",
	}}); err != nil {
		t.Fatalf("appendWAL: %v", err)
	}

	reloaded := newServiceVolumeIndex(db, &Index{
		Source:     "usn",
		Volume:     "F:",
		Compact:    true,
		Checkpoint: 10,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: "."},
		},
	})
	if err := reloaded.replayWAL(); err != nil {
		t.Fatalf("replayWAL: %v", err)
	}
	if reloaded.checkpoint != 11 || reloaded.index.Checkpoint != 11 || !reloaded.dirty {
		t.Fatalf("wal checkpoint/dirty mismatch: vol=%d idx=%d dirty=%v", reloaded.checkpoint, reloaded.index.Checkpoint, reloaded.dirty)
	}
	if len(reloaded.index.Records) != 2 || reloaded.index.Records[1].Name != "wal-created.txt" {
		t.Fatalf("wal record not replayed: %+v", reloaded.index.Records)
	}
	if _, err := os.Stat(walPath(db)); err != nil {
		t.Fatalf("wal missing before cleanup: %v", err)
	}
	if err := removeWAL(db); err != nil {
		t.Fatalf("removeWAL: %v", err)
	}
	if _, err := os.Stat(walPath(db)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wal still exists after cleanup: %v", err)
	}
}

func TestValidateUSNCheckpoint(t *testing.T) {
	vol := &serviceVolumeIndex{journalID: 10, checkpoint: 50}
	journal := usnJournalDataV0{UsnJournalID: 10, FirstUsn: 20, LowestValidUsn: 30, NextUsn: 100}
	if err := validateUSNCheckpoint(vol, journal); err != nil {
		t.Fatalf("valid checkpoint rejected: %v", err)
	}

	vol.checkpoint = 29
	if err := validateUSNCheckpoint(vol, journal); err == nil {
		t.Fatal("checkpoint before lowest valid USN was accepted")
	}

	vol.checkpoint = 101
	if err := validateUSNCheckpoint(vol, journal); err == nil {
		t.Fatal("checkpoint after next USN was accepted")
	}

	vol.checkpoint = 50
	vol.journalID = 11
	if err := validateUSNCheckpoint(vol, journal); err == nil {
		t.Fatal("changed journal id was accepted")
	}
}

func TestSearchCompactSkipsDeletedRecords(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: "."},
			{FRN: 101, ParentFRN: 100, Parent: 0, Name: "gone.txt", Deleted: true},
			{FRN: 102, ParentFRN: 100, Parent: 0, Name: "live.txt"},
		},
	}
	buildOrders(idx)

	matches, err := searchCompact(idx, queryOptions{Query: "txt", MatchPath: false, Limit: 10}, false)
	if err != nil {
		t.Fatalf("searchCompact: %v", err)
	}
	if len(matches) != 1 || matches[0].Name != "live.txt" {
		t.Fatalf("matches = %+v, want only live.txt", matches)
	}
}

func TestServiceVolumeIndexBuildsFRNMap(t *testing.T) {
	idx := &Index{
		Source:     "usn",
		Volume:     "F:",
		JournalID:  7,
		Checkpoint: 12,
		Compact:    true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Name: ".", Parent: -1},
			{FRN: 101, ParentFRN: 100, Name: "child.txt", Parent: 0},
		},
	}

	vol := newServiceVolumeIndex(`F:\seekfs_f.gsi`, idx)
	if vol.dbPath != `F:\seekfs_f.gsi` || vol.volume != "F:" || vol.journalID != 7 || vol.checkpoint != 12 || vol.state != "ready" {
		t.Fatalf("volume metadata mismatch: %+v", vol)
	}
	if vol.frnRecordCount() != 2 {
		t.Fatalf("frn record count = %d, want 2", vol.frnRecordCount())
	}
	if got, ok := vol.idForFRN(101); !ok || got != 1 {
		t.Fatalf("idForFRN(101) = %d, %v; want 1, true", got, ok)
	}
}

func TestCommonSearchQuerySemantics(t *testing.T) {
	idx := commonSearchFixture()
	cases := []struct {
		name      string
		opts      queryOptions
		wantNames []string
	}{
		{
			name:      "filename term",
			opts:      queryOptions{Query: "main", Limit: 20},
			wantNames: []string{"main.go"},
		},
		{
			name:      "full path terms",
			opts:      queryOptions{Query: "assets dat", MatchPath: true, Limit: 20},
			wantNames: []string{"sample.dat"},
		},
		{
			name:      "path substring and dotted filename substring",
			opts:      queryOptions{Query: "project .bin", MatchPath: true, Limit: 20},
			wantNames: []string{"volume.bin"},
		},
		{
			name:      "drive token",
			opts:      queryOptions{Query: "c: .bin", MatchPath: true, Limit: 20},
			wantNames: []string{"volume.bin"},
		},
		{
			name:      "compound dotted suffix",
			opts:      queryOptions{Query: ".tar.gz", MatchPath: true, Limit: 20},
			wantNames: []string{"archive.tar.gz"},
		},
		{
			name:      "extension filter",
			opts:      queryOptions{Query: "ext:go", MatchPath: true, Limit: 20},
			wantNames: []string{"main.go", "search_test.go"},
		},
		{
			name:      "directory filter",
			opts:      queryOptions{Query: "dir:src ext:go", MatchPath: true, Limit: 20},
			wantNames: []string{"main.go", "search_test.go"},
		},
		{
			name:      "glob filter",
			opts:      queryOptions{Query: "glob:*_test.go", MatchPath: true, Limit: 20},
			wantNames: []string{"search_test.go"},
		},
		{
			name:      "regex filter",
			opts:      queryOptions{Query: `regex:Assets.*\.dat$`, MatchPath: true, Limit: 20},
			wantNames: []string{"sample.dat"},
		},
		{
			name:      "type file",
			opts:      queryOptions{Query: "type:file assets", MatchPath: true, Limit: 20},
			wantNames: []string{"sample.dat", "notes.txt"},
		},
		{
			name:      "type directory",
			opts:      queryOptions{Query: "type:dir assets", MatchPath: true, Limit: 20},
			wantNames: []string{"Assets"},
		},
		{
			name:      "under filter",
			opts:      queryOptions{Query: "ext:dat", MatchPath: true, Under: `C:\fixture\workspace\Assets`, Limit: 20},
			wantNames: []string{"sample.dat"},
		},
		{
			name:      "under excludes sibling prefix",
			opts:      queryOptions{Query: "ext:dat", MatchPath: true, Under: `C:\fixture\workspace\Down`, Limit: 20},
			wantNames: nil,
		},
		{
			name:      "case sensitive",
			opts:      queryOptions{Query: "case: README", Limit: 20},
			wantNames: []string{"README.md"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matches, err := searchCompact(idx, tc.opts, false)
			if err != nil {
				t.Fatalf("searchCompact: %v", err)
			}
			got := namesOf(matches)
			if !sameStringSet(got, tc.wantNames) {
				t.Fatalf("names = %v, want %v", got, tc.wantNames)
			}
		})
	}
}

func TestServiceCandidatesMatchFullCompactSearchForCommonQueries(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []queryOptions{
		{Query: "assets dat", MatchPath: true, Limit: 20},
		{Query: "project .bin", MatchPath: true, Limit: 20},
		{Query: "c: .bin", MatchPath: true, Limit: 20},
		{Query: ".tar.gz", MatchPath: true, Limit: 20},
		{Query: "ext:dat", MatchPath: true, Under: `C:\fixture\workspace\Assets`, Limit: 20},
		{Query: "src go", MatchPath: true, Limit: 20},
		{Query: "ext:go dir:src", MatchPath: true, Limit: 20},
		{Query: "glob:*_test.go", MatchPath: true, Limit: 20},
		{Query: `regex:Assets.*\.dat$`, MatchPath: true, Limit: 20},
		{Query: "type:dir Assets", MatchPath: true, Limit: 20},
	}
	for _, opts := range cases {
		t.Run(opts.Query, func(t *testing.T) {
			full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("full search: %v", err)
			}
			fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
			if err != nil {
				t.Fatalf("candidate search: %v", err)
			}
			if !sameStringSet(namesOf(fast), namesOf(full)) {
				t.Fatalf("candidate names = %v, full names = %v", namesOf(fast), namesOf(full))
			}
		})
	}
}

func BenchmarkSearchCompactBroadPathQuery(b *testing.B) {
	idx := syntheticCompactIndex(100_000)
	opts := queryOptions{Query: "needle", MatchPath: true, Limit: 20}
	cache := make(map[int]string)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		matches, err := searchCompactWithCache(idx, opts, false, cache, nil)
		if err != nil {
			b.Fatal(err)
		}
		if len(matches) != 20 {
			b.Fatalf("matches = %d, want 20", len(matches))
		}
	}
}

func BenchmarkSearchCompactExtPrecheck(b *testing.B) {
	idx := syntheticCompactIndex(100_000)
	opts := queryOptions{Query: "ext:go", MatchPath: true, Limit: 20}
	cache := make(map[int]string)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		matches, err := searchCompactWithCache(idx, opts, false, cache, nil)
		if err != nil {
			b.Fatal(err)
		}
		if len(matches) != 20 {
			b.Fatalf("matches = %d, want 20", len(matches))
		}
	}
}

func BenchmarkSearchCompactNameTokenPathQuery(b *testing.B) {
	idx := syntheticCompactIndex(100_000)
	vol := newServiceVolumeIndex("bench.gsi", idx)
	opts := queryOptions{Query: "source", MatchPath: true, Limit: 20}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		matches, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
		if err != nil {
			b.Fatal(err)
		}
		if len(matches) != 20 {
			b.Fatalf("matches = %d, want 20", len(matches))
		}
	}
}

func syntheticCompactIndex(n int) *Index {
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: make([]CompactRecord, 0, n+2),
	}
	idx.Records = append(idx.Records,
		CompactRecord{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		CompactRecord{FRN: 2, ParentFRN: 1, Parent: 0, Name: "needle-root", Mode: uint32(os.ModeDir)},
	)
	for i := 0; i < n; i++ {
		parent := int32(0)
		parentFRN := uint64(1)
		if i%10 == 0 {
			parent = 1
			parentFRN = 2
		}
		name := fmt.Sprintf("file-%06d.txt", i)
		if i%37 == 0 {
			name = fmt.Sprintf("source-%06d.go", i)
		}
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       uint64(i + 10),
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
		})
	}
	buildOrders(idx)
	return idx
}

func commonSearchFixture() *Index {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
	}
	add := func(frn, parentFRN uint64, parent int32, name string, mode uint32) {
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       frn,
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
			Mode:      mode,
			ModUnix:   time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixNano(),
		})
	}
	add(1, 1, -1, ".", uint32(os.ModeDir))
	add(2, 1, 0, "fixture", uint32(os.ModeDir))
	add(3, 2, 1, "workspace", uint32(os.ModeDir))
	add(4, 3, 2, "Assets", uint32(os.ModeDir))
	add(5, 4, 3, "sample.dat", 0)
	add(6, 4, 3, "notes.txt", 0)
	add(7, 3, 2, "src", uint32(os.ModeDir))
	add(8, 7, 6, "main.go", 0)
	add(9, 7, 6, "search_test.go", 0)
	add(10, 3, 2, "README.md", 0)
	add(11, 3, 2, "readme.md", 0)
	add(12, 3, 2, "project-data-worktree", uint32(os.ModeDir))
	add(13, 12, 11, "volume.bin", 0)
	add(14, 3, 2, "archive.tar.gz", 0)
	add(15, 3, 2, "Downstream", uint32(os.ModeDir))
	add(16, 15, 14, "sibling.dat", 0)
	buildOrders(idx)
	return idx
}

func namesOf(entries []Entry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	sort.Strings(names)
	return names
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	a = append([]string(nil), a...)
	b = append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
