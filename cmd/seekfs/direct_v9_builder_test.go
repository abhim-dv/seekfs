package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDirectV9BuilderIsSourceOrderIndependentAndResolvesParents(t *testing.T) {
	records := []directV9Record{
		{FRN: 40, ParentFRN: 99, Name: "orphan.txt"},
		{FRN: 30, ParentFRN: 20, Name: "child.txt"},
		{FRN: 10, Name: "root"},
		{FRN: 20, ParentFRN: 10, Name: "parent"},
	}
	build := func(name string, source []directV9Record, runRecords int) (string, directV9BuildStats) {
		dir := t.TempDir()
		path := filepath.Join(dir, name+".gsi")
		stats, err := buildDirectV9(context.Background(), directV9BuildOptions{
			OutputPath: path,
			SpoolDir:   filepath.Join(dir, "spool"),
			Roots:      []string{"X:\\"},
			Volume:     "X:",
			Source:     "direct-test",
			BuiltAt:    time.Unix(123, 0),
			Records:    newDirectV9SliceSource(source),
			RunRecords: runRecords,
			RunBytes:   4096,
		})
		if err != nil {
			t.Fatal(err)
		}
		return path, stats
	}
	first, firstStats := build("first", records, 2)
	secondRecords := []directV9Record{records[2], records[0], records[3], records[1]}
	second, secondStats := build("second", secondRecords, 3)
	firstHash := directV9FileHash(t, first)
	secondHash := directV9FileHash(t, second)
	if firstHash != secondHash {
		t.Fatalf("source order changed direct output: %s != %s", firstHash, secondHash)
	}
	if firstStats.FinalIDRule != "ascending-frn; duplicate-frn-rejected" || len(secondStats.Sections) != 16 || secondStats.Sections[0] != "RANK" {
		t.Fatalf("unexpected build stats: %#v / %#v", firstStats, secondStats)
	}
	idx, err := loadIndexMMap(first)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.MMapRecords.file.close() })
	if idx.Version != indexVersionV9 || idx.compactRecordCount() != 4 {
		t.Fatalf("loaded direct index = version %d records %d", idx.Version, idx.compactRecordCount())
	}
	wantParents := []int32{-1, 0, 1, -1}
	for id, want := range wantParents {
		if got := idx.compactRecord(id).Parent; got != want {
			t.Fatalf("record %d parent=%d, want %d", id, got, want)
		}
	}
	if len(idx.Derived.NameOrder) != 4 || len(idx.Derived.NameRank) != 4 || len(idx.Derived.SizeOrder) != 4 || len(idx.Derived.ModOrder) != 4 || len(idx.Derived.ExtOrder) != 4 || len(idx.Derived.TypeOrder) != 4 || len(idx.Derived.PathOrder) != 4 {
		t.Fatalf("rank families not decoded: name=%d size=%d mod=%d ext=%d type=%d path=%d", len(idx.Derived.NameOrder), len(idx.Derived.SizeOrder), len(idx.Derived.ModOrder), len(idx.Derived.ExtOrder), len(idx.Derived.TypeOrder), len(idx.Derived.PathOrder))
	}
	if got, want := idx.Derived.ChildOffsets, []uint32{0, 1, 2, 2, 2}; !equalUint32s(got, want) {
		t.Fatalf("child offsets=%v want=%v", got, want)
	}
	if got, want := idx.Derived.ChildIDs, []uint32{1, 2}; !equalUint32s(got, want) {
		t.Fatalf("child IDs=%v want=%v", got, want)
	}
	if got, want := idx.Derived.RootIDs, []uint32{0, 3}; !equalUint32s(got, want) {
		t.Fatalf("root IDs=%v want=%v", got, want)
	}
	if got, want := idx.Derived.FRNs, []uint64{10, 20, 30, 40}; !equalUint64s(got, want) || !equalUint32s(idx.Derived.FRNRecordIDs, []uint32{0, 1, 2, 3}) {
		t.Fatalf("FRNS mismatch frns=%v ids=%v", got, idx.Derived.FRNRecordIDs)
	}
}

func TestDirectV9TopologyRejectsParentCycleAndCleans(t *testing.T) {
	dir := t.TempDir()
	_, err := buildDirectV9(context.Background(), directV9BuildOptions{
		OutputPath: filepath.Join(dir, "cycle.gsi"),
		SpoolDir:   filepath.Join(dir, "spool"),
		Records: newDirectV9SliceSource([]directV9Record{
			{FRN: 1, ParentFRN: 2, Name: "a"},
			{FRN: 2, ParentFRN: 1, Name: "b"},
		}),
		RunRecords: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "parent cycle") {
		t.Fatalf("cycle error=%v", err)
	}
	if leftovers, globErr := filepath.Glob(filepath.Join(dir, "spool", "direct-v9-*.tmp")); globErr != nil || len(leftovers) != 0 {
		t.Fatalf("cycle scratch survived: %v (%v)", leftovers, globErr)
	}
}

func equalUint32s(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalUint64s(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDirectV9RankFamiliesMatchComparator(t *testing.T) {
	records := []directV9Record{
		{FRN: 40, Name: "zeta.TXT", Path: `X:\zeta.TXT`, Size: 0, ModUnix: 0},
		{FRN: 10, Name: "dir", Path: `X:\dir`, Mode: uint32(os.ModeDir), Size: 5, ModUnix: 20},
		{FRN: 30, Name: "a.go", Path: `X:\dir\a.go`, Size: 5, ModUnix: 10},
		{FRN: 20, Name: ".profile", Path: `X:\.profile`, Size: 2, ModUnix: 30},
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "ranks.gsi")
	stats, err := buildDirectV9(context.Background(), directV9BuildOptions{OutputPath: out, SpoolDir: filepath.Join(dir, "spool"), Records: newDirectV9SliceSource(records), RunRecords: 2, RunBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.RankFamilies) != 6 {
		t.Fatalf("rank family reports=%d: %#v", len(stats.RankFamilies), stats.RankFamilies)
	}
	idx, err := loadIndexMMap(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.MMapRecords.file.close() })
	ordered := append([]directV9Record(nil), records...)
	sort.Slice(ordered, func(i, j int) bool { return directV9RecordLess(ordered[i], ordered[j]) })
	for _, spec := range directV9RankSpecs() {
		want := make([]uint32, len(ordered))
		for i := range want {
			want[i] = uint32(i)
		}
		sort.Slice(want, func(i, j int) bool {
			ki, kj := spec.Key(ordered[want[i]]), spec.Key(ordered[want[j]])
			if ki != kj {
				return ki < kj
			}
			return want[i] < want[j]
		})
		gotOrder, gotRank := directV9DerivedRank(idx, spec.Tag)
		if len(gotOrder) != len(want) || len(gotRank) != len(want) {
			t.Fatalf("%s lengths=%d/%d want=%d", spec.Name, len(gotOrder), len(gotRank), len(want))
		}
		for pos, id := range want {
			if gotOrder[pos] != id || gotRank[id] != uint32(pos) {
				t.Fatalf("%s mismatch at pos=%d got=%d/%d want=%d", spec.Name, pos, gotOrder[pos], gotRank[id], id)
			}
		}
	}
}

func TestDirectV9RankWorkerCountsPreserveOutput(t *testing.T) {
	records := make([]directV9Record, 0, 256)
	for i := 0; i < 256; i++ {
		records = append(records, directV9SyntheticRecord(i))
	}
	var wantHash string
	for _, workers := range []int{1, 4, 8, 16} {
		dir := t.TempDir()
		out := filepath.Join(dir, fmt.Sprintf("workers-%d.gsi", workers))
		stats, err := buildDirectV9(context.Background(), directV9BuildOptions{
			OutputPath:  out,
			SpoolDir:    filepath.Join(dir, "spool"),
			Records:     newDirectV9SliceSource(records),
			RunRecords:  32,
			RankWorkers: workers,
		})
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		gotHash := directV9FileHash(t, out)
		if workers == 1 {
			wantHash = gotHash
		} else if gotHash != wantHash {
			t.Fatalf("workers=%d output hash=%s want=%s stats=%+v", workers, gotHash, wantHash, stats)
		}
	}
}

func TestDirectV9SharedRankPassCancellationCleansOwnedRuns(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "records.final.tmp")
	f, err := os.Create(finalPath)
	if err != nil {
		t.Fatal(err)
	}
	bw := bufio.NewWriter(f)
	if _, err := writeDirectV9SpoolRecord(bw, directV9SyntheticRecord(0)); err != nil {
		t.Fatal(err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	owned := make([]string, 0)
	_, err = directV9BuildRankRunsShared(ctx, finalPath, dir, 32, 4, directV9RankSpecs(), &owned)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("shared rank cancellation error=%v", err)
	}
	directV9RemoveOwned(owned)
	if leftovers, globErr := filepath.Glob(filepath.Join(dir, "direct-v9-rank-*.tmp")); globErr != nil || len(leftovers) != 0 {
		t.Fatalf("shared rank cancellation leftovers=%v err=%v", leftovers, globErr)
	}
}

func directV9DerivedRank(idx *Index, tag uint32) ([]uint32, []uint32) {
	switch tag {
	case indexSectionRANK:
		return idx.Derived.NameOrder, idx.Derived.NameRank
	case indexSectionSRNK:
		return idx.Derived.SizeOrder, idx.Derived.SizeRank
	case indexSectionMRNK:
		return idx.Derived.ModOrder, idx.Derived.ModRank
	case indexSectionERNK:
		return idx.Derived.ExtOrder, idx.Derived.ExtRank
	case indexSectionTRNK:
		return idx.Derived.TypeOrder, idx.Derived.TypeRank
	case indexSectionPRNK:
		return idx.Derived.PathOrder, idx.Derived.PathRank
	default:
		return nil, nil
	}
}

func TestDirectV9BuilderRejectsDuplicateFRNAndCleansOwnedScratch(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "duplicate.gsi")
	_, err := buildDirectV9(context.Background(), directV9BuildOptions{
		OutputPath: out,
		SpoolDir:   filepath.Join(dir, "owned-spool"),
		BuiltAt:    time.Unix(123, 0),
		Records: newDirectV9SliceSource([]directV9Record{
			{FRN: 1, Name: "a"},
			{FRN: 1, Name: "b"},
		}),
		RunRecords: 1,
	})
	if !errors.Is(err, errDirectV9DuplicateFRN) {
		t.Fatalf("duplicate error = %v", err)
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("duplicate build left output: %v", statErr)
	}
	if leftovers, globErr := filepath.Glob(filepath.Join(dir, "owned-spool", "direct-v9-*.tmp")); globErr != nil || len(leftovers) != 0 {
		t.Fatalf("owned scratch survived cleanup: %v (%v)", leftovers, globErr)
	}
}

func TestDirectV9MFTAndUSNAdaptersAreElevationFree(t *testing.T) {
	mft := newDirectV9MFTSource(map[uint64]mftEntry{
		20: {frn: 20, parentFRN: 10, name: "child.bin", size: 7},
		10: {frn: 10, name: "root", attr: fileAttributeDir, isDir: true},
	})
	first, err := mft.Next(context.Background())
	if err != nil || first.FRN != 10 || first.Name != "root" {
		t.Fatalf("MFT adapter first=%+v err=%v", first, err)
	}
	second, err := mft.Next(context.Background())
	if err != nil || second.FRN != 20 || second.ParentFRN != 10 || second.Size != 7 {
		t.Fatalf("MFT adapter second=%+v err=%v", second, err)
	}
}

func TestDirectV9MFTSourceNormalizesRootSelfParent(t *testing.T) {
	mft := newDirectV9MFTSource(map[uint64]mftEntry{
		5:  {frn: 5, parentFRN: 5, name: "root", attr: fileAttributeDir, isDir: true},
		10: {frn: 10, parentFRN: 5, name: "child.bin", size: 3},
	})
	root, err := mft.Next(context.Background())
	if err != nil || root.FRN != 5 || root.ParentFRN != 0 {
		t.Fatalf("MFT root self-parent not normalized: root=%+v err=%v", root, err)
	}
	child, err := mft.Next(context.Background())
	if err != nil || child.FRN != 10 || child.ParentFRN != 5 {
		t.Fatalf("MFT child=%+v err=%v", child, err)
	}
}

func TestDirectV9USNAdapterIsElevationFree(t *testing.T) {
	usn := newDirectV9USNSource(map[uint64]usnNode{
		2: {frn: 2, parentFRN: 1, name: "child.bin"},
		1: {frn: 1, name: "root", attr: fileAttributeDir},
	})
	first, err := usn.Next(context.Background())
	if err != nil || first.FRN != 1 || first.Mode&uint32(os.ModeDir) == 0 {
		t.Fatalf("USN adapter first=%+v err=%v", first, err)
	}
	second, err := usn.Next(context.Background())
	if err != nil || second.FRN != 2 || second.ParentFRN != 1 {
		t.Fatalf("USN adapter second=%+v err=%v", second, err)
	}
}

func TestDirectV9WalkSourceFeedsTheSameBuilder(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "alpha.txt"), []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "beta.bin"), []byte("beta"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := newDirectV9WalkSource(root)
	if err != nil {
		t.Fatal(err)
	}
	defer source.(*directV9WalkSource).Close()
	dir := t.TempDir()
	out := filepath.Join(dir, "walk.gsi")
	stats, err := buildDirectV9(context.Background(), directV9BuildOptions{
		OutputPath: out,
		SpoolDir:   filepath.Join(dir, "spool"),
		Roots:      []string{root},
		Source:     "direct-walk",
		BuiltAt:    time.Unix(123, 0),
		Records:    source,
		RunRecords: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Records != 4 || stats.Sections[0] != "RANK" {
		t.Fatalf("walk build stats=%+v", stats)
	}
	idx, err := loadIndexMMap(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.MMapRecords.file.close() })
	if idx.compactRecordCount() != 4 || idx.Source != "direct-walk" {
		t.Fatalf("walk index records/source=%d/%q", idx.compactRecordCount(), idx.Source)
	}
}

func TestDirectV9WalkSourceExcludesOwnedArtifactsBeforeTraversal(t *testing.T) {
	root := t.TempDir()
	owned := filepath.Join(root, "owned-run")
	if err := os.MkdirAll(filepath.Join(root, "included"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(owned, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		filepath.Join(root, "included", "ok.txt"): "ok",
		filepath.Join(root, "ignored.gsi"):        "database",
		filepath.Join(owned, "nested", "new.tmp"): "builder output",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	report := &directV9WalkReport{}
	source, err := newDirectV9WalkSourceWithExclusions(root, []string{owned}, []string{".gsi"}, report)
	if err != nil {
		t.Fatal(err)
	}
	walk := source.(*directV9WalkSource)
	defer walk.Close()
	count := 0
	for {
		_, nextErr := source.Next(context.Background())
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		count++
	}
	// Root, included directory, and included file are the only records.
	if count != 3 || report.Excluded < 2 || !report.SourceComplete {
		t.Fatalf("walk count/report=%d/%+v", count, report)
	}
}

func TestDirectV9ConcurrentWalkBuilderOutputIsWorkerIndependent(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 32; i++ {
		dir := filepath.Join(root, fmt.Sprintf("d-%02d", i%4))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%02d.txt", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var want string
	for _, workers := range []int{1, 4, 8, 16} {
		dir := t.TempDir()
		report := &directV9WalkReport{}
		source, err := newDirectV9ConcurrentWalkSourceWithOptions(root, nil, nil, report, directV9ConcurrentWalkOptions{Workers: workers, Queue: workers * 2})
		if err != nil {
			t.Fatal(err)
		}
		stats, err := buildDirectV9(context.Background(), directV9BuildOptions{
			OutputPath:  filepath.Join(dir, "walk.gsi"),
			SpoolDir:    filepath.Join(dir, "spool"),
			Records:     source,
			RunRecords:  8,
			RankWorkers: workers,
			WalkReport:  report,
			Source:      "direct-walk",
		})
		if err != nil {
			t.Fatal(err)
		}
		got := directV9FileHash(t, filepath.Join(dir, "walk.gsi"))
		if workers == 1 {
			want = got
		} else if got != want {
			idx, loadErr := loadIndexMMap(filepath.Join(dir, "walk.gsi"))
			if loadErr != nil {
				t.Fatalf("workers=%d hash=%s want=%s load=%v stats=%+v", workers, got, want, loadErr, stats)
			}
			idxHash := directV9OrderHash(idx.Derived.NameOrder)
			_ = idx.MMapRecords.file.close()
			t.Fatalf("workers=%d hash=%s want=%s order_hash=%s stats=%+v", workers, got, want, idxHash, stats)
		}
	}
}

func TestDirectV9WalkPreflightUsesStableArtifactRootAcrossFreshRuns(t *testing.T) {
	root := t.TempDir()
	stable := filepath.Join(root, ".r5tmp")
	if err := os.MkdirAll(stable, 0o700); err != nil {
		t.Fatal(err)
	}
	runOne := filepath.Join(stable, "run-a-7f6e")
	runTwo := filepath.Join(stable, "run-b-92c1")
	for _, run := range []string{runOne, runTwo} {
		if err := os.MkdirAll(filepath.Join(run, "spool"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	preOne, err := directV9WalkPreflightFor(root, filepath.Join(runOne, "one.gsi"), filepath.Join(runOne, "spool"))
	if err != nil {
		t.Fatal(err)
	}
	preTwo, err := directV9WalkPreflightFor(root, filepath.Join(runTwo, "two.gsi"), filepath.Join(runTwo, "spool"))
	if err != nil {
		t.Fatal(err)
	}
	if !directV9PathUnderAny(preOne.Target, preOne.ExclusionRoots) || !directV9PathUnderAny(preOne.Spool, preOne.ExclusionRoots) ||
		!directV9PathUnderAny(preTwo.Target, preTwo.ExclusionRoots) || !directV9PathUnderAny(preTwo.Spool, preTwo.ExclusionRoots) {
		t.Fatalf("target/spool escaped stable exclusion: one=%+v two=%+v", preOne, preTwo)
	}
	if !directV9PathUnderAny(stable, preTwo.ExclusionRoots) || directV9PathUnderAny(root, preTwo.ExclusionRoots) {
		t.Fatalf("stable root/source alias check failed: %+v", preTwo.ExclusionRoots)
	}
	if directV9PathUnderAny(preTwo.Target, []string{runOne}) {
		t.Fatalf("fresh run target was covered only by stale prior-run root")
	}
	if !directV9PathUnderAny(preTwo.Target, preOne.ExclusionRoots) {
		t.Fatalf("stable root should cover a future run without stale variables")
	}
}

func TestDirectV9BuilderCancellationDoesNotPublishTarget(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := buildDirectV9(ctx, directV9BuildOptions{
		OutputPath: filepath.Join(dir, "cancelled.gsi"),
		SpoolDir:   filepath.Join(dir, "owned-spool"),
		Records:    newDirectV9SliceSource([]directV9Record{{FRN: 1, Name: "a"}}),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "cancelled.gsi")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("cancelled build published target: %v", statErr)
	}
}

func TestDirectV9InaccessibleBoundDegradesWithinLimit(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "degraded.gsi")
	report := &directV9WalkReport{SourceComplete: false}
	report.note("inaccessible", "X:\\protected\\dir-a")
	report.note("inaccessible", "X:\\protected\\dir-b")
	stats, err := buildDirectV9(context.Background(), directV9BuildOptions{
		OutputPath:     out,
		SpoolDir:       filepath.Join(dir, "spool"),
		Roots:          []string{`X:\`},
		Volume:         "X:",
		Source:         "direct-walk",
		BuiltAt:        time.Unix(0, 0),
		Records:        newDirectV9SliceSource([]directV9Record{{FRN: 1, Name: "."}}),
		WalkReport:     report,
		MaxInaccessible: 64,
	})
	if err != nil {
		t.Fatalf("bounded inaccessible build failed: %v", err)
	}
	if !stats.SourceDegraded {
		t.Fatalf("bounded inaccessible build did not report degraded: %+v", stats)
	}
	if stats.SourceComplete {
		t.Fatalf("degraded build must not report source complete")
	}
	if stats.SourceInaccessible != 2 {
		t.Fatalf("source inaccessible = %d, want 2", stats.SourceInaccessible)
	}
	if _, statErr := os.Stat(out); statErr != nil {
		t.Fatalf("degraded build did not publish target: %v", statErr)
	}
}

func TestDirectV9InaccessibleBoundRefusesAboveLimit(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "refused.gsi")
	report := &directV9WalkReport{SourceComplete: false, Inaccessible: 2}
	_, err := buildDirectV9(context.Background(), directV9BuildOptions{
		OutputPath:     out,
		SpoolDir:       filepath.Join(dir, "spool"),
		Roots:          []string{`X:\`},
		Volume:         "X:",
		Source:         "direct-walk",
		BuiltAt:        time.Unix(0, 0),
		Records:        newDirectV9SliceSource([]directV9Record{{FRN: 1, Name: "."}}),
		WalkReport:     report,
		MaxInaccessible: 1,
	})
	if err == nil {
		t.Fatal("build with inaccessible above bound must fail")
	}
	if !strings.Contains(err.Error(), "source incomplete") {
		t.Fatalf("error = %v, want source-incomplete refusal", err)
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("refused build published target: %v", statErr)
	}
}

func TestDirectV9InaccessibleZeroRequiresCleanSource(t *testing.T) {
	dir := t.TempDir()
	report := &directV9WalkReport{SourceComplete: true, Inaccessible: 0}
	out := filepath.Join(dir, "clean.gsi")
	if _, err := buildDirectV9(context.Background(), directV9BuildOptions{
		OutputPath:     out,
		SpoolDir:       filepath.Join(dir, "spool"),
		Roots:          []string{`X:\`},
		Volume:         "X:",
		Source:         "direct-walk",
		BuiltAt:        time.Unix(0, 0),
		Records:        newDirectV9SliceSource([]directV9Record{{FRN: 1, Name: "."}}),
		WalkReport:     report,
		MaxInaccessible: 0,
	}); err != nil {
		t.Fatalf("clean build failed: %v", err)
	}
	if _, statErr := os.Stat(out); statErr != nil {
		t.Fatalf("clean build did not publish target: %v", statErr)
	}
}

func TestDirectV9CanonicalDerivedParity(t *testing.T) {
	t.Setenv("SEEKFS_LOW_MEMORY_TRIGRAM_MAX_POSTING", "2")
	t.Setenv("SEEKFS_V9_SELF_NAME_GRAMS", "1")
	records := []directV9Record{
		{FRN: 1, Name: ".", Path: `X:\`, Mode: uint32(os.ModeDir)},
		{FRN: 2, ParentFRN: 1, Name: "common", Path: `X:\common`, Mode: uint32(os.ModeDir)},
		{FRN: 3, ParentFRN: 1, Name: "rare", Path: `X:\rare`, Mode: uint32(os.ModeDir)},
		{FRN: 4, ParentFRN: 2, Name: "common-alpha-nrrd.txt", Path: `X:\common\common-alpha-nrrd.txt`, Size: 30, ModUnix: 300},
		{FRN: 5, ParentFRN: 2, Name: "common-beta.txt", Path: `X:\common\common-beta.txt`, Size: 10, ModUnix: 100},
		{FRN: 6, ParentFRN: 2, Name: "common-gamma.go", Path: `X:\common\common-gamma.go`, Size: 20, ModUnix: 200},
		{FRN: 7, ParentFRN: 3, Name: "zqx-rare-nrrd.bin", Path: `X:\rare\zqx-rare-nrrd.bin`, Size: 40, ModUnix: 400},
		{FRN: 8, ParentFRN: 3, Name: "neutral.txt", Path: `X:\rare\neutral.txt`, Size: 50, ModUnix: 500},
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "canonical-parity.gsi")
	if _, err := buildDirectV9(context.Background(), directV9BuildOptions{
		OutputPath: out,
		SpoolDir:   filepath.Join(dir, "spool"),
		Roots:      []string{`X:\`},
		Volume:     "X:",
		Source:     "usn",
		BuiltAt:    time.Unix(123, 0),
		Records:    newDirectV9SliceSource(records),
		RunRecords: 2,
		RunBytes:   4096,
	}); err != nil {
		t.Fatal(err)
	}
	idx, err := loadIndexMMap(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.MMapRecords.file.close() })
	sizeRank := append([]uint32(nil), idx.Derived.SizeRank...)
	modRank := append([]uint32(nil), idx.Derived.ModRank...)
	extRank := append([]uint32(nil), idx.Derived.ExtRank...)
	typeRank := append([]uint32(nil), idx.Derived.TypeRank...)
	pathRank := append([]uint32(nil), idx.Derived.PathRank...)
	vol := newServiceVolumeIndex(out, idx)

	subt := directV9TestSectionData(t, out, indexSectionSUBT)
	if parts := directV9TestUint32Parts(t, subt); len(parts) != 8 {
		t.Fatalf("SUBT parts=%d, want canonical 8", len(parts))
	}
	for _, tc := range []struct {
		name string
		got  []uint32
		rank []uint32
	}{
		{name: "size", got: vol.subtreeSizeRank, rank: sizeRank},
		{name: "modified", got: vol.subtreeModRank, rank: modRank},
		{name: "extension", got: vol.subtreeExtRank, rank: extRank},
		{name: "type", got: vol.subtreeTypeRank, rank: typeRank},
		{name: "path", got: vol.subtreePathRank, rank: pathRank},
	} {
		if want := vol.buildSubtreeMinRanks(tc.rank); !equalUint32s(tc.got, want) {
			t.Fatalf("SUBT %s minima=%v, want %v", tc.name, tc.got, want)
		}
	}

	pcmp := idx.Derived.Postings[indexSectionPCMP]
	componentPostings := map[string][]uint32{"common": {1}, "rare": {2}}
	for key, want := range componentPostings {
		if got := pcmp.stringPosting(key); !equalUint32s(got, want) {
			t.Fatalf("PCMP %q=%v, want own-directory roots %v", key, got, want)
		}
	}
	pcmpData := directV9TestSectionData(t, out, indexSectionPCMP)
	firstBlock, blockCount := directV9TestStringPostingBlocks(t, pcmpData, "common")
	if blockCount != 1 {
		t.Fatalf("PCMP common blocks=%d, want 1", blockCount)
	}
	if got, want := directV9TestBlockMinRank(pcmpData, firstBlock), vol.queryIndex.nameRank[1]; got != want {
		t.Fatalf("PCMP common minRank=%d, want name rank %d", got, want)
	}
	if got, want := pcmp.RankBounds, buildComponentPostingRankBounds(componentPostings, vol); !reflect.DeepEqual(got, want) {
		t.Fatalf("PXRC bounds=%+v, want canonical descendant bounds %+v", got, want)
	}

	commonGram, rareGram := trigramKey("com"), trigramKey("zqx")
	if got := idx.Derived.NameTrigrams.countForGram(commonGram); got != 4 {
		t.Fatalf("PNGR common count=%d, want omitted count 4", got)
	}
	if _, omitted := idx.Derived.NameTrigrams.omitted[commonGram]; !omitted {
		t.Fatal("PNGR did not mark common filename gram omitted")
	}
	if got := idx.Derived.NameTrigrams.countForGram(rareGram); got != 1 {
		t.Fatalf("PNGR rare count=%d, want stored count 1", got)
	}
	if idx.Derived.SelfNameTrigrams == nil || !idx.Derived.SelfNameTrigrams.gramUnionComplete {
		t.Fatal("PNGC missing complete-union metadata")
	}
	if got := idx.Derived.SelfNameTrigrams.countForGram(commonGram); got != 4 {
		t.Fatalf("PNGC common count=%d, want omitted companion count 4", got)
	}
	if got := idx.Derived.SelfNameTrigrams.countForGram(rareGram); got != 0 {
		t.Fatalf("PNGC rare count=%d, want selective PNGR to own it", got)
	}
	pngcData := directV9TestSectionData(t, out, indexSectionPNGC)
	firstBlock, blockCount = directV9TestGramPostingBlocks(t, pngcData, commonGram)
	if blockCount != 1 {
		t.Fatalf("PNGC common blocks=%d, want 1", blockCount)
	}
	wantCommonRank := minRankForIDs([]uint32{1, 3, 4, 5}, vol.queryIndex.nameRank)
	if got := directV9TestBlockMinRank(pngcData, firstBlock); got != wantCommonRank {
		t.Fatalf("PNGC common minRank=%d, want name rank %d", got, wantCommonRank)
	}

	queries := []string{
		"path:common", "path:rare", "path:no-such-component",
		"nrrd", "zqx", "path:common|rare", "path:common !beta",
		"path:common sort:size", "path:common sort:modified",
		"path:common sort:extension", "path:common sort:type", "path:common sort:path",
	}
	fastCount := map[string]bool{"path:common": true, "path:rare": true, "path:no-such-component": true, "nrrd": true, "zqx": true}
	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			opts := queryOptions{Query: query, Limit: 100, Trace: &searchTrace{}}
			want, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("oracle search: %v", err)
			}
			got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
			if err != nil {
				t.Fatalf("mapped search: %v", err)
			}
			gotPaths, wantPaths := directV9TestPaths(got), directV9TestPaths(want)
			if !strings.Contains(query, "sort:") {
				sort.Strings(gotPaths)
				sort.Strings(wantPaths)
			}
			if !reflect.DeepEqual(gotPaths, wantPaths) {
				t.Fatalf("mapped paths=%v, want exhaustive %v", gotPaths, wantPaths)
			}
			countOpts := queryOptions{Query: query, Trace: &searchTrace{}}
			wantCount, err := searchCompactWithCache(idx, countOpts, true, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("oracle count: %v", err)
			}
			gotCount, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, countOpts, true)
			if err != nil || len(gotCount) != len(wantCount) {
				t.Fatalf("mapped count-search=%d err=%v, want %d", len(gotCount), err, len(wantCount))
			}
			count, handled, err := countServiceVolumes([]*serviceVolumeIndex{vol}, countOpts)
			if err != nil {
				t.Fatalf("fast count: %v", err)
			}
			if fastCount[query] && !handled {
				t.Fatal("fast count declined canonical direct fixture")
			}
			if handled && count != len(wantCount) {
				t.Fatalf("fast count=%d, want %d", count, len(wantCount))
			}
		})
	}
}

func TestDecodeUint32SectionRejectsTrailingSUBTPart(t *testing.T) {
	parts := make([][]uint32, 9)
	for i := range parts {
		parts[i] = []uint32{uint32(i)}
	}
	if got := decodeUint32Section(encodeUint32Section(parts...), 8); got != nil {
		t.Fatalf("decoded %d SUBT parts from a nine-part payload; want strict rejection", len(got))
	}
}

func directV9TestSectionData(t *testing.T, path string, tag uint32) []byte {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	table, err := readRawV9SectionTable(path, info.Size())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range table.Entries {
		if entry.tag == tag {
			return data[int(entry.offset):int(entry.offset+entry.length)]
		}
	}
	t.Fatalf("section %08x not found", tag)
	return nil
}

func directV9TestUint32Parts(t *testing.T, data []byte) [][]uint32 {
	t.Helper()
	parts := make([][]uint32, 0, 8)
	for off := 0; off < len(data); {
		if off+4 > len(data) {
			t.Fatalf("truncated uint32 part header at %d", off)
		}
		n := int(binary.LittleEndian.Uint32(data[off:]))
		off += 4
		bytesLen := n * 4
		if n < 0 || bytesLen/4 != n || off+bytesLen < off || off+bytesLen > len(data) {
			t.Fatalf("invalid uint32 part at %d count=%d", off-4, n)
		}
		part := make([]uint32, n)
		for i := range part {
			part[i] = binary.LittleEndian.Uint32(data[off+i*4:])
		}
		parts = append(parts, part)
		off += bytesLen
	}
	return parts
}

func directV9TestStringPostingBlocks(t *testing.T, data []byte, key string) (int, int) {
	t.Helper()
	entryCount := int(binary.LittleEndian.Uint32(data[0:]))
	keyBlobLen := int(binary.LittleEndian.Uint32(data[4:]))
	blockCount := int(binary.LittleEndian.Uint32(data[8:]))
	keyStart := 16 + entryCount*20 + blockCount*28
	if keyStart+keyBlobLen > len(data) {
		t.Fatal("invalid string posting layout")
	}
	for i := 0; i < entryCount; i++ {
		off := 16 + i*20
		keyOff := int(binary.LittleEndian.Uint32(data[off:]))
		keyLen := int(binary.LittleEndian.Uint16(data[off+4:]))
		if keyOff+keyLen <= keyBlobLen && string(data[keyStart+keyOff:keyStart+keyOff+keyLen]) == key {
			return int(binary.LittleEndian.Uint32(data[off+12:])), int(binary.LittleEndian.Uint32(data[off+16:]))
		}
	}
	t.Fatalf("string posting %q not found", key)
	return 0, 0
}

func directV9TestGramPostingBlocks(t *testing.T, data []byte, gram uint32) (int, int) {
	t.Helper()
	entryCount := int(binary.LittleEndian.Uint32(data[0:]))
	for i := 0; i < entryCount; i++ {
		off := 16 + i*16
		if binary.LittleEndian.Uint32(data[off:]) == gram {
			return int(binary.LittleEndian.Uint32(data[off+8:])), int(binary.LittleEndian.Uint32(data[off+12:]))
		}
	}
	t.Fatalf("gram posting %08x not found", gram)
	return 0, 0
}

func directV9TestBlockMinRank(data []byte, block int) uint32 {
	entryCount := int(binary.LittleEndian.Uint32(data[0:]))
	entrySize := 20
	if binary.LittleEndian.Uint32(data[4:]) == 0 {
		entrySize = 16
	}
	return binary.LittleEndian.Uint32(data[16+entryCount*entrySize+block*28+24:])
}

func directV9TestPaths(entries []Entry) []string {
	paths := make([]string, len(entries))
	for i := range entries {
		paths[i] = entries[i].Path
	}
	return paths
}

func TestDirectV9PrototypeGate(t *testing.T) {
	if os.Getenv("SEEKFS_DIRECT_V9_PROTOTYPE") != "1" {
		t.Skip("set SEEKFS_DIRECT_V9_PROTOTYPE=1 to run the owned 500k/1M prototype gate")
	}
	records := 500_000
	if value := os.Getenv("SEEKFS_DIRECT_V9_RECORDS"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			t.Fatalf("invalid SEEKFS_DIRECT_V9_RECORDS=%q", value)
		}
		records = parsed
	}
	dir := t.TempDir()
	stats, err := buildDirectV9(context.Background(), directV9BuildOptions{
		OutputPath: filepath.Join(dir, "prototype.gsi"),
		SpoolDir:   filepath.Join(dir, "spool"),
		Roots:      []string{"X:\\"},
		Volume:     "X:",
		Source:     "synthetic-direct",
		BuiltAt:    time.Unix(123, 0),
		Records:    &directV9SyntheticSource{remaining: records},
		RunRecords: 64 * 1024,
		RunBytes:   64 * 1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := loadIndexMMap(filepath.Join(dir, "prototype.gsi"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = loaded.MMapRecords.file.close() })
	if loaded.compactRecordCount() != records || len(loaded.Derived.NameOrder) != records {
		t.Fatalf("prototype loaded records/rank = %d/%d, want %d/%d", loaded.compactRecordCount(), len(loaded.Derived.NameOrder), records, records)
	}
	encoded, _ := json.Marshal(stats)
	t.Logf("direct-v9 prototype stats=%s output_sha256=%s", encoded, directV9FileHash(t, filepath.Join(dir, "prototype.gsi")))
}

func TestDirectV9ExternalPrototypeValidation(t *testing.T) {
	path := os.Getenv("SEEKFS_DIRECT_V9_VALIDATE_PATH")
	if path == "" {
		t.Skip("set SEEKFS_DIRECT_V9_VALIDATE_PATH to validate an owned prototype target")
	}
	idx, err := loadIndexMMap(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.MMapRecords.file.close() })
	n := idx.compactRecordCount()
	if idx.Version != indexVersionV9 || n == 0 || len(idx.Derived.NameOrder) != n || len(idx.Derived.NameRank) != n {
		t.Fatalf("invalid direct prototype reader state: version=%d records=%d order=%d rank=%d", idx.Version, n, len(idx.Derived.NameOrder), len(idx.Derived.NameRank))
	}
	want := make([]uint32, n)
	for i := range want {
		want[i] = uint32(i)
	}
	sort.Slice(want, func(i, j int) bool {
		a, b := int(want[i]), int(want[j])
		ak, bk := a%100000, b%100000
		if ak != bk {
			return ak < bk
		}
		return a < b
	})
	for pos, id := range want {
		if got := idx.Derived.NameOrder[pos]; got != id {
			t.Fatalf("default-order oracle mismatch at %d: got %d want %d", pos, got, id)
		}
		if got := idx.Derived.NameRank[id]; int(got) != pos {
			t.Fatalf("rank oracle mismatch for id %d: got %d want %d", id, got, pos)
		}
		if pos < 3 || pos == len(want)-1 {
			name := idx.compactRecord(int(id)).Name
			if name != fmt.Sprintf("prototype-%05d.txt", int(id)%100000) {
				t.Fatalf("record name mismatch for id %d: %q", id, name)
			}
		}
	}
	for id := 0; id < min(n, 4096); id++ {
		wantParent := int32(id - 1)
		if id == 0 {
			wantParent = -1
		}
		if got := idx.compactRecord(id).Parent; got != wantParent {
			t.Fatalf("parent oracle mismatch for id %d: got %d want %d", id, got, wantParent)
		}
	}
	wantHash := directV9OrderHash(want)
	gotHash := directV9OrderHash(idx.Derived.NameOrder)
	if gotHash != wantHash {
		t.Fatalf("default-order search hash mismatch: got %s want %s", gotHash, wantHash)
	}
	for _, spec := range directV9RankSpecs() {
		if spec.Tag == indexSectionRANK {
			continue
		}
		wantFamily := make([]uint32, n)
		for i := range wantFamily {
			wantFamily[i] = uint32(i)
		}
		sort.Slice(wantFamily, func(i, j int) bool {
			ki := spec.Key(directV9SyntheticRecord(int(wantFamily[i])))
			kj := spec.Key(directV9SyntheticRecord(int(wantFamily[j])))
			if ki != kj {
				return ki < kj
			}
			return wantFamily[i] < wantFamily[j]
		})
		gotOrder, gotRank := directV9DerivedRank(idx, spec.Tag)
		if len(gotOrder) != n || len(gotRank) != n {
			t.Fatalf("%s lengths=%d/%d want=%d", spec.Name, len(gotOrder), len(gotRank), n)
		}
		for pos, id := range wantFamily {
			if gotOrder[pos] != id || gotRank[id] != uint32(pos) {
				t.Fatalf("%s oracle mismatch at %d: got id/rank=%d/%d want=%d", spec.Name, pos, gotOrder[pos], gotRank[id], id)
			}
		}
		t.Logf("direct-v9 %s hash=%s", spec.Name, directV9OrderHash(gotOrder))
	}
	t.Logf("direct-v9 reader/oracle valid records=%d default_order_hash=%s top20_hash=%s", n, gotHash, directV9OrderHash(idx.Derived.NameOrder[:min(20, n)]))
}

func directV9SyntheticRecord(id int) directV9Record {
	return directV9Record{
		FRN:       uint64(id + 1),
		ParentFRN: uint64(id),
		Size:      int64(id % 100000),
		ModUnix:   int64(1_700_000_000_000_000_000 + id),
		Name:      fmt.Sprintf("prototype-%05d.txt", id%100000),
		Path:      fmt.Sprintf(`synthetic:\prototype-%05d.txt`, id%100000),
	}
}

func directV9OrderHash(order []uint32) string {
	h := sha256.New()
	var buf [4]byte
	for _, id := range order {
		binary.LittleEndian.PutUint32(buf[:], id)
		_, _ = h.Write(buf[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func directV9FileHash(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}
