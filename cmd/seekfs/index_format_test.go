package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
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
	if got.reconstructCompactPath(1) != `C:\main.go` {
		t.Fatalf("path = %q", got.reconstructCompactPath(1))
	}
}

func TestCompactIndexMMapRoundTripKeepsPathAndMetadata(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	modified := time.Unix(0, 987654321)
	idx := &Index{
		Version:    indexVersion,
		Roots:      []string{`F:\`},
		BuiltAt:    time.Unix(0, 123456789),
		Source:     "usn",
		Volume:     "F:",
		JournalID:  42,
		Checkpoint: 99,
		Compact:    true,
		Records: []CompactRecord{
			{FRN: 10, ParentFRN: 10, Parent: -1, Name: ".", Mode: uint32(os.ModeDir), ModUnix: modified.UnixNano()},
			{FRN: 11, ParentFRN: 10, Parent: 0, Name: "Projects", Mode: uint32(os.ModeDir), ModUnix: modified.Add(time.Second).UnixNano()},
			{FRN: 12, ParentFRN: 11, Parent: 1, Name: "Scan.NRRD", Size: 4096, ModUnix: modified.Add(2 * time.Second).UnixNano()},
		},
	}
	path := filepath.Join(t.TempDir(), "compact.gsi")
	if err := saveIndex(path, idx); err != nil {
		t.Fatalf("saveIndex: %v", err)
	}
	got, err := loadIndexMMap(path)
	if err != nil {
		t.Fatalf("loadIndexMMap: %v", err)
	}
	t.Cleanup(func() {
		if got.MMapRecords != nil {
			_ = got.MMapRecords.file.close()
		}
	})
	if got.MMapRecords == nil || got.PackedRecords != nil || len(got.Records) != 0 {
		t.Fatalf("mmap records not active: mmap=%v packed=%v records=%d", got.MMapRecords != nil, got.PackedRecords != nil, len(got.Records))
	}
	if got.compactRecordCount() != 3 || !got.compactHasSize() || !got.compactHasModTime() {
		t.Fatalf("mmap capabilities count=%d size=%v mod=%v", got.compactRecordCount(), got.compactHasSize(), got.compactHasModTime())
	}
	rec := got.compactRecord(2)
	if rec.Name != "Scan.NRRD" || rec.Size != 4096 || rec.Parent != 1 || rec.ParentFRN != 11 {
		t.Fatalf("mmap record mismatch: %+v", rec)
	}
	if lower := got.compactLowerNameAt(2); lower != "scan.nrrd" {
		t.Fatalf("lower name = %q", lower)
	}
	if path := got.reconstructCompactPath(2); path != `F:\Projects\Scan.NRRD` {
		t.Fatalf("path = %q", path)
	}
}

func TestCompactDiskRecordRefsUseWideEncodingPastNarrowLimit(t *testing.T) {
	var buf bytes.Buffer
	parent := uint32(compactNarrowParentSentinel)
	nameOff := uint32(compactNarrowParentSentinel + 7)
	if err := writeCompactRecordRefs(&buf, parent, nameOff, true); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 8 {
		t.Fatalf("wide ref bytes = %d, want 8", buf.Len())
	}
	gotParent, gotNameOff, err := readCompactRecordRefs(bytes.NewReader(buf.Bytes()), true)
	if err != nil {
		t.Fatal(err)
	}
	if gotParent != parent || gotNameOff != nameOff {
		t.Fatalf("refs = (%d, %d), want (%d, %d)", gotParent, gotNameOff, parent, nameOff)
	}
}

func TestCompactDiskRecordRefsRejectNarrowOverflow(t *testing.T) {
	var buf bytes.Buffer
	if err := writeCompactRecordRefs(&buf, compactNarrowParentSentinel+1, 1, false); err == nil {
		t.Fatal("expected narrow parent overflow error")
	}
	if err := writeCompactRecordRefs(&buf, 1, compactNarrowParentSentinel, false); err == nil {
		t.Fatal("expected narrow name ref overflow error")
	}
}

func TestCompactDiskRecordBytesSwitchesToWideAtNarrowLimit(t *testing.T) {
	narrowMaxCount := int(compactNarrowMaxRecordRef) + 1
	if got := compactDiskRecordBytesForCounts(narrowMaxCount, 1); got != compactDiskRecordBytes {
		t.Fatalf("narrow record bytes = %d, want %d", got, compactDiskRecordBytes)
	}
	if got := compactDiskRecordBytesForCounts(narrowMaxCount+1, 1); got != compactWideDiskRecordBytes {
		t.Fatalf("wide-by-record-count bytes = %d, want %d", got, compactWideDiskRecordBytes)
	}
	if got := compactDiskRecordBytesForCounts(1, narrowMaxCount+1); got != compactWideDiskRecordBytes {
		t.Fatalf("wide-by-name-count bytes = %d, want %d", got, compactWideDiskRecordBytes)
	}
}

func TestEngineV9WritesAndLoadsDerivedSections(t *testing.T) {
	t.Setenv("SEEKFS_ENGINE_V9", "1")
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`C:\`},
		BuiltAt: time.Unix(0, 123),
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 10, ParentFRN: 10, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 11, ParentFRN: 10, Parent: 0, Name: "docs", Mode: uint32(os.ModeDir)},
			{FRN: 12, ParentFRN: 11, Parent: 1, Name: "Alpha.TXT"},
			{FRN: 13, ParentFRN: 11, Parent: 1, Name: "Résumé.GO"},
			{FRN: 14, ParentFRN: 11, Parent: 1, Name: "Alpha.TXT"},
			{FRN: 15, ParentFRN: 11, Parent: 1, Name: "İ.TXT"},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "v9.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v9: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load v9 mmap: %v", err)
	}
	defer loaded.MMapRecords.file.close()
	if loaded.Version != indexVersionV9 {
		t.Fatalf("version = %d, want %d", loaded.Version, indexVersionV9)
	}
	if len(loaded.Derived.NameOrder) != loaded.compactRecordCount() || len(loaded.Derived.NameRank) != loaded.compactRecordCount() {
		t.Fatalf("rank section missing: %+v", loaded.Derived)
	}
	if len(loaded.Derived.ChildOffsets) == 0 || len(loaded.Derived.ChildIDs) == 0 || len(loaded.Derived.SubtreeOrder) == 0 {
		t.Fatalf("child/subtree sections missing: %+v", loaded.Derived)
	}
	if len(loaded.Derived.FRNs) != loaded.compactRecordCount() || len(loaded.Derived.FRNRecordIDs) != loaded.compactRecordCount() {
		t.Fatalf("frn section missing: %+v", loaded.Derived)
	}
	tokenCount := len(loaded.MMapRecords.tokenTable) / 6
	if len(loaded.Derived.LowerOffs) != tokenCount || len(loaded.Derived.LowerLens) != tokenCount || len(loaded.Derived.LowerBlob) == 0 {
		t.Fatalf("lowercase section missing: %+v", loaded.Derived)
	}
	if got, want := loaded.compactLowerNameAt(2), "alpha.txt"; got != want {
		t.Fatalf("mapped lower name = %q, want %q", got, want)
	}
	if got, want := loaded.compactLowerNameAt(3), "résumé.go"; got != want {
		t.Fatalf("mapped unicode lower name = %q, want %q", got, want)
	}
	if got, want := loaded.compactLowerNameAt(4), "alpha.txt"; got != want {
		t.Fatalf("mapped duplicate lower name = %q, want %q", got, want)
	}
	if got, want := loaded.compactLowerNameAt(5), strings.ToLower("İ.TXT"); got != want {
		t.Fatalf("mapped unicode variable lower name = %q, want %q", got, want)
	}
	sections, _ := derivedSectionInfo(loaded.Derived)
	for _, want := range []string{"LOWR", "PEXT", "PCMP", "PNGR"} {
		if !slices.Contains(sections, want) {
			t.Fatalf("sections = %v, missing %s", sections, want)
		}
	}
	vol := newServiceVolumeIndex(db, loaded)
	if vol.queryIndex == nil || len(vol.queryIndex.nameOrder) != loaded.compactRecordCount() {
		t.Fatalf("service did not wire mapped rank section")
	}
	if len(vol.queryIndex.ext) != 0 {
		t.Fatalf("mapped ext postings were eagerly materialized: %v", vol.queryIndex.ext)
	}
	if len(vol.queryIndex.components) != 0 {
		t.Fatalf("mapped component postings were eagerly materialized: %v", vol.queryIndex.components)
	}
	if vol.nameTrigramStateString() != "ready" || vol.nameTrigramIndex() == nil {
		t.Fatalf("mapped name trigram section was not marked ready")
	}
	if vol.nameTrigramIndex().mappedGrams == nil || len(vol.nameTrigramIndex().segments) != 0 {
		t.Fatalf("mapped name trigram section was materialized unexpectedly")
	}
	if ids, ok := vol.nameTrigramIndex().candidateIDs("alpha"); !ok || len(ids) == 0 {
		t.Fatalf("mapped name trigram lookup ok=%v ids=%v", ok, ids)
	}
	if got := vol.extPosting("txt"); !reflect.DeepEqual(got, []int{2, 4, 5}) {
		t.Fatalf("raw mapped ext lookup = %v", got)
	}
	if got := vol.pathComponentRootIDs("docs"); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("raw mapped component lookup = %v", got)
	}
	if len(vol.childOffsets) == 0 || len(vol.subtreeOrder) == 0 || len(vol.frns) == 0 {
		t.Fatalf("service did not wire mapped derived sections")
	}
}

func TestEngineV9LowmemMappedStartupSkipsResidentPathRebuilds(t *testing.T) {
	t.Setenv("SEEKFS_ENGINE_V9", "1")
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`C:\`},
		BuiltAt: time.Unix(0, 789),
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 10, ParentFRN: 10, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 11, ParentFRN: 10, Parent: 0, Name: "docs", Mode: uint32(os.ModeDir)},
			{FRN: 12, ParentFRN: 11, Parent: 1, Name: "Alpha.TXT"},
			{FRN: 13, ParentFRN: 11, Parent: 1, Name: "Beta.GO"},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "v9-lowmem.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v9: %v", err)
	}
	loaded, err := loadIndexForService(db)
	if err != nil {
		t.Fatalf("load for service: %v", err)
	}
	defer loaded.MMapRecords.file.close()
	vol := newServiceVolumeIndex(db, loaded)
	if vol.queryIndex == nil {
		t.Fatal("queryIndex was not initialized")
	}
	if vol.queryIndex.ext != nil || vol.queryIndex.components != nil || vol.queryIndex.pathGrams != nil || vol.queryIndex.dirsReady {
		t.Fatalf("mapped lowmem materialized resident postings: ext=%v components=%v pathGrams=%v dirsReady=%v",
			vol.queryIndex.ext != nil, vol.queryIndex.components != nil, vol.queryIndex.pathGrams != nil, vol.queryIndex.dirsReady)
	}
	if len(vol.queryIndex.nameOrder) != loaded.compactRecordCount() || len(vol.queryIndex.nameRank) != loaded.compactRecordCount() {
		t.Fatalf("mapped rank not wired: order=%d rank=%d records=%d", len(vol.queryIndex.nameOrder), len(vol.queryIndex.nameRank), loaded.compactRecordCount())
	}
	if got := vol.nameTrigramStateString(); got != "ready" {
		t.Fatalf("mapped trigram state = %q, want ready", got)
	}
	lazyPQ := mustParseQuery(t, queryOptions{Query: "ext:txt dir:docs alpha", MatchPath: true, Limit: 10})
	lazyPQ.Limit = normalizedLimit(10, false)
	plan, ok := vol.buildCandidatePlan(lazyPQ)
	if !ok {
		t.Fatal("buildCandidatePlan declined mapped ext+dir query")
	}
	if got, want := plan.sourceSummary(), "dir:docs+ext:txt"; got != want {
		t.Fatalf("source summary = %q, want %q", got, want)
	}
	if vol.queryIndex.ext != nil || vol.queryIndex.components != nil || vol.queryIndex.pathGrams != nil || vol.queryIndex.dirsReady {
		t.Fatalf("mapped lowmem materialized resident postings after buildCandidatePlan: ext=%v components=%v pathGrams=%v dirsReady=%v",
			vol.queryIndex.ext != nil, vol.queryIndex.components != nil, vol.queryIndex.pathGrams != nil, vol.queryIndex.dirsReady)
	}
	for _, tc := range []struct {
		name       string
		opts       queryOptions
		want       []string
		wantSource string
	}{
		{
			name: "type-dir-falls-to-bounded-scan",
			opts: queryOptions{Query: "type:dir", Limit: 10},
			want: []string{`C:`, `C:\docs`},
		},
		{
			name: "exact-dir-uses-mapped-component",
			opts: queryOptions{Query: "dir:docs alpha", MatchPath: true, Limit: 10},
			want: []string{`C:\docs\Alpha.TXT`},
		},
		{
			name: "substring-dir-declines-to-bounded-scan",
			opts: queryOptions{Query: "dir:doc alpha", MatchPath: true, Limit: 10},
			want: []string{`C:\docs\Alpha.TXT`},
		},
		{
			name:       "mapped-ext-top-uses-posting-blocks",
			opts:       queryOptions{Query: "ext:txt", Limit: 1},
			want:       []string{`C:\docs\Alpha.TXT`},
			wantSource: "planned:ext-top",
		},
		{
			name:       "mapped-ext-dir-plan-stays-lazy",
			opts:       queryOptions{Query: "ext:txt dir:docs alpha", MatchPath: true, Limit: 10},
			want:       []string{`C:\docs\Alpha.TXT`},
			wantSource: "planned:dir:docs+ext:txt",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := &searchTrace{}
			tc.opts.Trace = trace
			got, err := searchCompactWithCache(loaded, tc.opts, false, make(map[int]string), vol.nameTermCandidates)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(pathsOf(got), tc.want) {
				t.Fatalf("paths = %v, want %v (source=%s decline=%s candidates=%d)", pathsOf(got), tc.want, trace.Source, trace.Decline, trace.Candidates)
			}
			if trace.Source == "" {
				t.Fatal("query did not report a trace source")
			}
			if tc.wantSource != "" && trace.Source != tc.wantSource {
				t.Fatalf("trace source = %q, want %q", trace.Source, tc.wantSource)
			}
		})
	}
	if vol.queryIndex.ext != nil || vol.queryIndex.components != nil || vol.queryIndex.pathGrams != nil || vol.queryIndex.dirsReady {
		t.Fatalf("mapped lowmem materialized resident postings after queries: ext=%v components=%v pathGrams=%v dirsReady=%v",
			vol.queryIndex.ext != nil, vol.queryIndex.components != nil, vol.queryIndex.pathGrams != nil, vol.queryIndex.dirsReady)
	}
	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "ext:txt dir:docs alpha", MatchPath: true})
	if err != nil {
		t.Fatalf("count mapped ext+dir query: %v", err)
	}
	if !ok || count != 1 {
		t.Fatalf("count mapped ext+dir query = (%d, %v), want (1, true)", count, ok)
	}
}

func TestEngineV9LowmemMappedBoundedScanUsesMappedRankOrder(t *testing.T) {
	t.Setenv("SEEKFS_ENGINE_V9", "1")
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`C:\`},
		BuiltAt: time.Unix(0, 790),
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 10, ParentFRN: 10, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 11, ParentFRN: 10, Parent: 0, Name: "zeta", Mode: uint32(os.ModeDir)},
			{FRN: 12, ParentFRN: 10, Parent: 0, Name: "alpha", Mode: uint32(os.ModeDir)},
			{FRN: 13, ParentFRN: 12, Parent: 2, Name: "note.txt"},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "v9-lowmem-order.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v9: %v", err)
	}
	loaded, err := loadIndexForService(db)
	if err != nil {
		t.Fatalf("load for service: %v", err)
	}
	defer loaded.MMapRecords.file.close()
	vol := newServiceVolumeIndex(db, loaded)
	if vol.queryIndex == nil {
		t.Fatal("queryIndex was not initialized")
	}
	if len(vol.queryIndex.nameOrder) != loaded.compactRecordCount() {
		t.Fatalf("mapped rank not wired: order=%d records=%d", len(vol.queryIndex.nameOrder), loaded.compactRecordCount())
	}
	if vol.queryIndex.dirsReady {
		t.Fatal("mapped lowmem unexpectedly built resident directory postings")
	}

	opts := queryOptions{Query: "type:dir", Limit: 2, Trace: &searchTrace{}}
	got, err := searchCompactWithCache(loaded, opts, false, make(map[int]string), vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{`C:`, `C:\alpha`}; !reflect.DeepEqual(pathsOf(got), want) {
		t.Fatalf("paths = %v, want %v (source=%s candidates=%d)", pathsOf(got), want, opts.Trace.Source, opts.Trace.Candidates)
	}
	if opts.Trace.Source != "bounded-scan" {
		t.Fatalf("trace source = %q, want bounded-scan", opts.Trace.Source)
	}
}

func TestEngineV9UpgradeIndexCommand(t *testing.T) {
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`F:\`},
		BuiltAt: time.Unix(0, 456),
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 20, ParentFRN: 20, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 21, ParentFRN: 20, Parent: 0, Name: "src", Mode: uint32(os.ModeDir)},
			{FRN: 22, ParentFRN: 21, Parent: 1, Name: "main.go"},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "upgrade.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v8: %v", err)
	}
	t.Setenv("SEEKFS_ENGINE_V9", "1")
	if err := cmdUpgradeIndex([]string{"-db", db}); err != nil {
		t.Fatalf("upgrade-index: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load upgraded index: %v", err)
	}
	defer loaded.MMapRecords.file.close()
	if loaded.Version != indexVersionV9 {
		t.Fatalf("version = %d, want %d", loaded.Version, indexVersionV9)
	}
	sections, bytes := derivedSectionInfo(loaded.Derived)
	if !reflect.DeepEqual(sections, []string{"RANK", "CHLD", "SUBT", "FRNS", "LOWR", "PEXT", "PCMP", "PNGR"}) {
		t.Fatalf("sections = %v", sections)
	}
	if bytes == 0 {
		t.Fatal("derived byte count was not reported")
	}
}

func TestEngineV9DisabledDoesNotAllocateOverlay(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex(`F:\state.gsi`, idx)
	if vol.overlay != nil || vol.snap.Load() != nil {
		t.Fatal("engine v9 overlay scaffold was allocated while gate was disabled")
	}
	if err := cmdUpgradeIndex([]string{"-db", filepath.Join(t.TempDir(), "missing.gsi")}); err == nil {
		t.Fatal("upgrade-index succeeded without SEEKFS_ENGINE_V9")
	}
}

func TestEngineV9OverlaySnapshotScaffold(t *testing.T) {
	t.Setenv("SEEKFS_ENGINE_V9", "1")
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex(`F:\state.gsi`, idx)
	if vol.overlay == nil || vol.snap.Load() == nil {
		t.Fatal("engine v9 did not initialize overlay snapshot scaffold")
	}
	vol.applyUSNChanges([]usnChange{{FRN: 101, ParentFRN: 100, USN: 10, Reason: usnReasonFileCreate, Name: "alpha.txt"}})
	snap := vol.snap.Load()
	if snap == nil || snap.watermark != 1 || len(snap.records) < 1 {
		t.Fatalf("snapshot = %+v, want watermark 1", snap)
	}
	if got := vol.overlay.byFRN[101]; got != 0 {
		t.Fatalf("overlay slot for frn 101 = %d, want 0", got)
	}
	vol.applyUSNChanges([]usnChange{{FRN: 101, USN: 11, Reason: usnReasonFileDelete}})
	if len(vol.overlay.records) != 2 || !vol.overlay.records[1].Deleted {
		t.Fatalf("delete did not append overlay tombstone marker: %+v", vol.overlay.records)
	}
	if len(idx.Records) != 1 || idx.Records[0].Deleted {
		t.Fatalf("v9 overlay path mutated base records: %+v", idx.Records)
	}
}

func TestEngineV9OverlaySnapshotIsStableAcrossLaterChanges(t *testing.T) {
	t.Setenv("SEEKFS_ENGINE_V9", "1")
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex(`F:\state.gsi`, idx)
	vol.applyUSNChanges([]usnChange{{FRN: 101, ParentFRN: 100, USN: 10, Reason: usnReasonFileCreate, Name: "first.txt"}})
	first := vol.snap.Load()
	if first == nil || first.watermark != 1 || len(first.records) < 1 {
		t.Fatal("first snapshot missing overlay records")
	}
	vol.applyUSNChanges([]usnChange{{FRN: 101, ParentFRN: 100, USN: 11, Reason: usnReasonRenameNew, Name: "second.txt"}})

	firstRecords := first.records[:first.watermark]
	entry, ok := vol.overlayEntry(firstRecords, latestOverlaySlotsByFRN(firstRecords), 0, map[int32]struct{}{}, nil)
	if !ok {
		t.Fatal("first snapshot entry disappeared after later rename")
	}
	if entry.Name != "first.txt" {
		t.Fatalf("first snapshot entry name = %q, want first.txt", entry.Name)
	}
	latest := vol.snap.Load()
	latestRecords := latest.records[:latest.watermark]
	entry, ok = vol.overlayEntry(latestRecords, latestOverlaySlotsByFRN(latestRecords), 1, map[int32]struct{}{}, nil)
	if !ok || entry.Name != "second.txt" {
		t.Fatalf("latest snapshot entry = (%+v, %v), want second.txt", entry, ok)
	}
}

func TestEngineV9OverlayServiceSearchMergesCreatesAndDeletes(t *testing.T) {
	t.Setenv("SEEKFS_ENGINE_V9", "1")
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`F:\`},
		BuiltAt: time.Unix(0, 789),
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 101, ParentFRN: 100, Parent: 0, Name: "base.txt"},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "overlay.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v9: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load v9 mmap: %v", err)
	}
	defer loaded.MMapRecords.file.close()
	vol := newServiceVolumeIndex(db, loaded)
	vol.applyUSNChanges([]usnChange{{FRN: 102, ParentFRN: 100, USN: 20, Reason: usnReasonFileCreate, Name: "overlay.txt"}})
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "overlay", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search overlay: %v", err)
	}
	if !sameStringSet(pathsOf(got), []string{`F:\overlay.txt`}) {
		t.Fatalf("overlay search paths = %v", pathsOf(got))
	}
	vol.applyUSNChanges([]usnChange{{FRN: 101, USN: 21, Reason: usnReasonFileDelete}})
	got, err = searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "base", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search tombstoned base: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("tombstoned base leaked into search: %v", pathsOf(got))
	}
	if loaded.MMapRecords == nil || len(loaded.Records) != 0 {
		t.Fatalf("v9 overlay materialized base records: mmap=%v records=%d", loaded.MMapRecords != nil, len(loaded.Records))
	}
}

func TestEngineV9OverlayMergeFillsLimitAndRanksCreates(t *testing.T) {
	t.Setenv("SEEKFS_ENGINE_V9", "1")
	records := []CompactRecord{{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}}
	for i := 0; i < 40; i++ {
		records = append(records, CompactRecord{
			FRN:       uint64(200 + i),
			ParentFRN: 100,
			Parent:    0,
			Name:      fmt.Sprintf("needle-%02d.txt", i),
		})
	}
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`F:\`},
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: records,
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex(`F:\state.gsi`, idx)
	changes := make([]usnChange, 0, 11)
	for i := 0; i < 10; i++ {
		changes = append(changes, usnChange{FRN: uint64(200 + i), USN: int64(10 + i), Reason: usnReasonFileDelete})
	}
	changes = append(changes, usnChange{
		FRN:       999,
		ParentFRN: 100,
		USN:       30,
		Reason:    usnReasonFileCreate,
		Name:      "aaa-needle.txt",
	})
	vol.applyUSNChanges(changes)

	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "needle", Limit: 25}, false)
	if err != nil {
		t.Fatalf("search with overlay: %v", err)
	}
	paths := pathsOf(got)
	if len(paths) != 25 {
		t.Fatalf("overlay merge returned %d paths=%v, want filled limit 25", len(paths), paths)
	}
	if paths[0] != `F:\aaa-needle.txt` {
		t.Fatalf("first result = %q, want overlay create ranked first; paths=%v", paths[0], paths[:min(len(paths), 5)])
	}
	for i := 0; i < 10; i++ {
		deleted := fmt.Sprintf(`F:\needle-%02d.txt`, i)
		if slices.Contains(paths, deleted) {
			t.Fatalf("deleted base path %s leaked in %v", deleted, paths)
		}
	}
	if !slices.Contains(paths, `F:\needle-33.txt`) {
		t.Fatalf("later base matches did not backfill limit: %v", paths)
	}
}

func TestEngineV9OverlayCountOnlySearchIncludesCreatesAndExcludesTombstones(t *testing.T) {
	t.Setenv("SEEKFS_ENGINE_V9", "1")
	vol := engineV9OverlaySearchTestVolume(t)
	vol.applyUSNChanges([]usnChange{
		{FRN: 102, ParentFRN: 100, USN: 20, Reason: usnReasonFileCreate, Name: "needle-overlay.txt"},
		{FRN: 101, USN: 21, Reason: usnReasonFileDelete},
	})

	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "needle", MatchPath: true}, true)
	if err != nil {
		t.Fatalf("count-only overlay search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("count-only needle matches = %d paths=%v, want only overlay-created record", len(got), pathsOf(got))
	}
	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "needle", MatchPath: true})
	if err != nil {
		t.Fatalf("overlay-aware fast count: %v", err)
	}
	if !ok || count != 1 {
		t.Fatalf("overlay-aware fast count = (%d, %v), want (1, true)", count, ok)
	}
}

func TestEngineV9DirectoryDeleteTombstonesBaseDescendants(t *testing.T) {
	t.Setenv("SEEKFS_ENGINE_V9", "1")
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`F:\`},
		BuiltAt: time.Unix(0, 789),
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 200, ParentFRN: 100, Parent: 0, Name: "doomed", Mode: uint32(os.ModeDir)},
			{FRN: 201, ParentFRN: 200, Parent: 1, Name: "child.txt"},
			{FRN: 202, ParentFRN: 201, Parent: 2, Name: "grandchild.log"},
			{FRN: 300, ParentFRN: 100, Parent: 0, Name: "survivor.txt"},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "dir-delete.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save fixture: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	defer loaded.MMapRecords.file.close()
	vol := newServiceVolumeIndex(db, loaded)
	vol.applyUSNChanges([]usnChange{{FRN: 200, USN: 20, Reason: usnReasonFileDelete}})

	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "child", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search deleted child: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("deleted directory descendants leaked into search: %v", pathsOf(got))
	}
	got, err = searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "child", MatchPath: true}, true)
	if err != nil {
		t.Fatalf("count deleted child: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("deleted directory descendants leaked into count: %v", pathsOf(got))
	}
	got, err = searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "survivor", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search survivor: %v", err)
	}
	if !sameStringSet(pathsOf(got), []string{`F:\survivor.txt`}) {
		t.Fatalf("survivor paths = %v", pathsOf(got))
	}
}

func TestEngineV9OverlayRenameSearchHidesOldPathAndShowsNewPath(t *testing.T) {
	t.Setenv("SEEKFS_ENGINE_V9", "1")
	vol := engineV9OverlaySearchTestVolume(t)
	vol.applyUSNChanges([]usnChange{
		{FRN: 101, ParentFRN: 100, USN: 20, Reason: usnReasonRenameOld, Name: "needle-base.txt"},
		{FRN: 101, ParentFRN: 100, USN: 21, Reason: usnReasonRenameNew, Name: "needle-renamed.txt"},
	})

	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "needle", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search renamed overlay: %v", err)
	}
	if !sameStringSet(pathsOf(got), []string{`F:\needle-renamed.txt`}) {
		t.Fatalf("rename overlay paths = %v, want only renamed path", pathsOf(got))
	}
	got, err = searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "needle-base", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search old renamed path: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("old base path leaked after rename: %v", pathsOf(got))
	}
}

func TestEngineV9OverlaySearchReconstructsChildPathThroughBaseParent(t *testing.T) {
	t.Setenv("SEEKFS_ENGINE_V9", "1")
	vol := engineV9OverlaySearchTestVolume(t)
	vol.applyUSNChanges([]usnChange{{
		FRN:       201,
		ParentFRN: 200,
		USN:       20,
		Reason:    usnReasonFileCreate,
		Name:      "overlay-child.txt",
	}})

	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "overlay-child", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search overlay child: %v", err)
	}
	if !sameStringSet(pathsOf(got), []string{`F:\base-parent\overlay-child.txt`}) {
		t.Fatalf("overlay child paths = %v", pathsOf(got))
	}
}

func TestEngineV9CompactOverlayIndexMergesBaseAndOverlay(t *testing.T) {
	t.Setenv("SEEKFS_ENGINE_V9", "1")
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 101, ParentFRN: 100, Parent: 0, Name: "delete-me.txt"},
			{FRN: 102, ParentFRN: 100, Parent: 0, Name: "keep-me.txt"},
		},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex(`F:\state.gsi`, idx)
	vol.applyUSNChanges([]usnChange{
		{FRN: 101, USN: 30, Reason: usnReasonFileDelete},
		{FRN: 103, ParentFRN: 100, USN: 31, Reason: usnReasonFileCreate, Name: "created.txt"},
	})
	compacted := compactOverlayIndex(vol)
	if compacted.Checkpoint != 31 {
		t.Fatalf("checkpoint = %d, want 31", compacted.Checkpoint)
	}
	names := make([]string, 0, len(compacted.Records))
	for _, rec := range compacted.Records {
		names = append(names, rec.Name)
		if rec.Name == "delete-me.txt" {
			t.Fatalf("deleted base record persisted: %+v", compacted.Records)
		}
	}
	if !sameStringSet(names, []string{".", "keep-me.txt", "created.txt"}) {
		t.Fatalf("compacted names = %v", names)
	}
	created := compacted.Records[2]
	if created.Parent != 0 || created.ParentFRN != 100 {
		t.Fatalf("created parent = %+v", created)
	}
}

func TestEngineV9PersistVolumeCompactsOverlayToMappedBase(t *testing.T) {
	t.Setenv("SEEKFS_ENGINE_V9", "1")
	db := filepath.Join(t.TempDir(), "state.gsi")
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`F:\`},
		BuiltAt: time.Unix(0, 123),
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 101, ParentFRN: 100, Parent: 0, Name: "delete-me.txt"},
		},
	}
	buildOrders(idx)
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save fixture: %v", err)
	}
	loaded, err := loadIndexForService(db)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	vol := newServiceVolumeIndex(db, loaded)
	vol.applyUSNChanges([]usnChange{
		{FRN: 101, USN: 40, Reason: usnReasonFileDelete},
		{FRN: 102, ParentFRN: 100, USN: 41, Reason: usnReasonFileCreate, Name: "created.txt"},
	})
	vol.dirty = true
	svc := &goSearchService{
		indexes: []*Index{vol.index},
		volumes: []*serviceVolumeIndex{vol},
	}
	svc.persistVolumeIfDue(vol, true)
	if vol.dirty {
		t.Fatal("volume stayed dirty after forced compaction")
	}
	if vol.overlay != nil && vol.overlay.watermark.Load() != 0 {
		t.Fatalf("overlay survived compaction with watermark %d", vol.overlay.watermark.Load())
	}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "created", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search compacted overlay: %v", err)
	}
	if !sameStringSet(pathsOf(got), []string{`F:\created.txt`}) {
		t.Fatalf("compacted overlay paths = %v", pathsOf(got))
	}
	reloaded, err := loadIndexForService(db)
	if err != nil {
		t.Fatalf("reload compacted fixture: %v", err)
	}
	reloadedVol := newServiceVolumeIndex(db, reloaded)
	got, err = searchServiceVolumes([]*serviceVolumeIndex{reloadedVol}, queryOptions{Query: "created", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search reloaded compacted overlay: %v", err)
	}
	if !sameStringSet(pathsOf(got), []string{`F:\created.txt`}) {
		t.Fatalf("reloaded compacted overlay paths = %v", pathsOf(got))
	}
	if got, err := searchServiceVolumes([]*serviceVolumeIndex{reloadedVol}, queryOptions{Query: "delete-me", MatchPath: true, Limit: 10}, false); err != nil {
		t.Fatalf("search deleted compacted overlay: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("deleted path survived compaction: %v", pathsOf(got))
	}
}

func TestMappedPostingSectionUsesDecodedBlockCache(t *testing.T) {
	postings := map[string][]uint32{"go": make([]uint32, 0, 1500)}
	for i := 0; i < 1500; i++ {
		postings["go"] = append(postings["go"], uint32(i*2))
	}
	section := decodePostingSection(encodeStringPostingSection(postings, nil))
	if section.BlockCount < 2 {
		t.Fatalf("block count = %d, want at least 2", section.BlockCount)
	}

	servicePostingBlockCache = postingBlockLRU{}
	t.Setenv("SEEKFS_POSTING_CACHE_MB", "0")
	if got := section.stringPosting("go"); !reflect.DeepEqual(got, postings["go"]) {
		t.Fatalf("posting with disabled cache mismatch")
	}
	servicePostingBlockCache.mu.Lock()
	disabledItems := len(servicePostingBlockCache.items)
	servicePostingBlockCache.mu.Unlock()
	if disabledItems != 0 {
		t.Fatalf("disabled cache item count = %d, want 0", disabledItems)
	}

	servicePostingBlockCache = postingBlockLRU{}
	t.Setenv("SEEKFS_POSTING_CACHE_MB", "1")
	if got := section.stringPosting("go"); !reflect.DeepEqual(got, postings["go"]) {
		t.Fatalf("posting with enabled cache mismatch")
	}
	servicePostingBlockCache.mu.Lock()
	enabledItems := len(servicePostingBlockCache.items)
	enabledBytes := servicePostingBlockCache.bytes
	servicePostingBlockCache.mu.Unlock()
	if enabledItems != section.BlockCount {
		t.Fatalf("enabled cache item count = %d, want %d", enabledItems, section.BlockCount)
	}
	if enabledBytes <= 0 {
		t.Fatalf("enabled cache bytes = %d, want > 0", enabledBytes)
	}
}

func TestMappedPostingSectionIteratorParityAndIntersection(t *testing.T) {
	postings := map[string][]uint32{"go": make([]uint32, 0, 2500)}
	for i := 0; i < 2500; i++ {
		postings["go"] = append(postings["go"], uint32(i*2))
	}
	section := decodePostingSection(encodeStringPostingSection(postings, nil))
	it, count, ok := section.stringPostingIterator("go")
	if !ok {
		t.Fatal("stringPostingIterator declined mapped posting")
	}
	if count != len(postings["go"]) {
		t.Fatalf("iterator count = %d, want %d", count, len(postings["go"]))
	}
	if got := materializePostingBlockIterator(it, count); !reflect.DeepEqual(got, postings["go"]) {
		t.Fatalf("iterator materialization mismatch")
	}

	it, _, ok = section.stringPostingIterator("go")
	if !ok {
		t.Fatal("stringPostingIterator declined mapped posting for intersection")
	}
	seed := []uint32{1, 4, 8, 1998, 2000, 4998, 5001}
	got := intersectSortedUint32sWithPostingIterator(seed, it)
	want := []uint32{4, 8, 1998, 2000, 4998}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("block iterator intersection = %v, want %v", got, want)
	}
}

func TestWalkWatcherRebuildPathRefreshesWalkIndex(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "old.txt"), []byte("old"), 0o644); err != nil {
		t.Fatalf("write old fixture: %v", err)
	}
	db := filepath.Join(t.TempDir(), "walk.gsi")
	idx := &Index{Version: indexVersion, Roots: []string{root}, BuiltAt: time.Now(), Source: "walk"}
	if err := walkRoot(root, idx); err != nil {
		t.Fatalf("walk fixture: %v", err)
	}
	buildOrders(idx)
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save walk fixture: %v", err)
	}
	loaded, err := loadIndexForService(db)
	if err != nil {
		t.Fatalf("load walk fixture: %v", err)
	}
	vol := newServiceVolumeIndex(db, loaded)
	svc := &goSearchService{
		indexes: []*Index{vol.index},
		volumes: []*serviceVolumeIndex{vol},
	}
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatalf("write new fixture: %v", err)
	}
	if err := svc.rebuildWalkVolumeInPlace(vol, "test"); err != nil {
		t.Fatalf("rebuild walk index: %v", err)
	}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "new", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search rebuilt walk volume: %v", err)
	}
	if len(got) != 1 || got[0].Name != "new.txt" {
		t.Fatalf("rebuilt walk search results = %+v", got)
	}
	if len(svc.indexes) != 1 || svc.indexes[0] != vol.index {
		t.Fatal("service index slice was not updated to the rebuilt live index")
	}
}

func TestEngineV9OverlayCompactionDueTriggers(t *testing.T) {
	t.Setenv("SEEKFS_ENGINE_V9", "1")
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 101, ParentFRN: 100, Parent: 0, Name: "base.txt"},
		},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "state.gsi"), idx)
	now := time.Unix(1_000, 0)
	vol.lastPersist = now
	if vol.compactionDue(now.Add(time.Minute)) {
		t.Fatal("fresh empty overlay triggered compaction")
	}
	vol.overlay.watermark.Store(overlayCompactionMaxSlots)
	if !vol.compactionDue(now.Add(time.Minute)) {
		t.Fatal("overlay slot threshold did not trigger compaction")
	}

	vol.overlay = newOverlaySegment()
	vol.overlay.tombstone.add(1)
	vol.overlay.watermark.Store(1)
	if !vol.compactionDue(now.Add(time.Minute)) {
		t.Fatal("tombstone ratio threshold did not trigger compaction")
	}

	vol.overlay = newOverlaySegment()
	if !vol.compactionDue(now.Add(overlayCompactionDirtyAge)) {
		t.Fatal("dirty age threshold did not trigger compaction")
	}

	wal := walPath(vol.dbPath)
	f, err := os.Create(wal)
	if err != nil {
		t.Fatalf("create wal: %v", err)
	}
	if err := f.Truncate(overlayCompactionMaxWAL); err != nil {
		t.Fatalf("truncate wal: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close wal: %v", err)
	}
	vol.lastPersist = now
	if !vol.compactionDue(now.Add(time.Minute)) {
		t.Fatal("wal size threshold did not trigger compaction")
	}
}

func TestEngineV9LockVolumeSearchDoesNotSerializeOnSearchMu(t *testing.T) {
	t.Setenv("SEEKFS_ENGINE_V9", "1")
	vol := &serviceVolumeIndex{}
	vol.searchMu.Lock()
	defer vol.searchMu.Unlock()

	locked, ok := lockVolumeSearch(vol, queryOptions{})
	if !ok {
		t.Fatal("v9 lockVolumeSearch rejected uncanceled query")
	}
	if locked {
		t.Fatal("v9 lockVolumeSearch acquired searchMu")
	}
}

func TestEngineV9SnapshotServiceVolumeForSearchUsesStableBase(t *testing.T) {
	t.Setenv("SEEKFS_ENGINE_V9", "1")
	oldIdx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 101, ParentFRN: 100, Parent: 0, Name: "old-base.txt"},
		},
	}
	buildOrders(oldIdx)
	vol := newServiceVolumeIndex(`F:\state.gsi`, oldIdx)
	view := snapshotServiceVolumeForSearch(vol)

	newIdx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 200, ParentFRN: 200, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 201, ParentFRN: 200, Parent: 0, Name: "new-base.txt"},
		},
	}
	buildOrders(newIdx)
	replaceServiceVolumeContents(vol, newServiceVolumeIndex(`F:\state.gsi`, newIdx))

	got, err := searchServiceVolumes([]*serviceVolumeIndex{view}, queryOptions{Query: "old-base", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search read view: %v", err)
	}
	if !sameStringSet(pathsOf(got), []string{`F:\old-base.txt`}) {
		t.Fatalf("read view paths = %v, want old base", pathsOf(got))
	}
	got, err = searchServiceVolumes([]*serviceVolumeIndex{view}, queryOptions{Query: "new-base", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search read view new base: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("read view saw replaced live base: %v", pathsOf(got))
	}
}

func TestEngineV9ReadViewCachesUseSnapshotGeneration(t *testing.T) {
	t.Setenv("SEEKFS_ENGINE_V9", "1")
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 101, ParentFRN: 100, Parent: 0, Name: "dir", Mode: uint32(os.ModeDir)},
			{FRN: 102, ParentFRN: 101, Parent: 1, Name: "alpha-cache.txt"},
		},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex(`F:\state.gsi`, idx)
	view := snapshotServiceVolumeForSearch(vol)
	gen := view.cacheGeneration()
	if gen == 0 {
		t.Fatal("read view cache generation was zero")
	}

	got := view.nameTermPosting("alpha")
	if !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("nameTermPosting = %v, want [2]", got)
	}
	if stamp := view.termCache["alpha"].gen; stamp != gen {
		t.Fatalf("term cache stamp = %d, want snapshot gen %d", stamp, gen)
	}
	view.termCache["alpha"] = postingCacheEntry{ids: []int{99}, gen: gen - 1}
	if got := view.nameTermPosting("alpha"); !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("stale term cache was not ignored: %v", got)
	}
	view.underRootCache[strings.ToLower(filepath.Clean(`F:\dir`))] = postingCacheEntry{ids: []int{99}, gen: gen - 1}
	if got := view.underRootIDs(`F:\dir`); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("stale under-root cache was not ignored: %v", got)
	}

	vol.applyUSNChanges([]usnChange{{FRN: 102, ParentFRN: 100, USN: 10, Reason: usnReasonFileCreate, Name: "beta-cache.txt"}})
	next := snapshotServiceVolumeForSearch(vol)
	if next.cacheGeneration() == gen {
		t.Fatalf("cache generation did not advance after publish: %d", gen)
	}
	if _, ok := next.termCache["alpha"]; ok {
		t.Fatal("new read view reused prior query cache")
	}
}

func engineV9OverlaySearchTestVolume(t *testing.T) *serviceVolumeIndex {
	t.Helper()
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`F:\`},
		BuiltAt: time.Unix(0, 789),
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 101, ParentFRN: 100, Parent: 0, Name: "needle-base.txt"},
			{FRN: 200, ParentFRN: 100, Parent: 0, Name: "base-parent", Mode: uint32(os.ModeDir)},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "overlay-search.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v9 overlay search fixture: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load v9 overlay search fixture: %v", err)
	}
	t.Cleanup(func() {
		if loaded.MMapRecords != nil {
			_ = loaded.MMapRecords.file.close()
		}
	})
	return newServiceVolumeIndex(db, loaded)
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

func TestUSNMutationStateMachineMatchesFreshOracle(t *testing.T) {
	logical := map[uint64]CompactRecord{
		100: {FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir), Size: 0},
	}
	idx := freshIndexFromLogicalRecords("F:", logical)
	vol := newServiceVolumeIndex(`F:\seekfs_state.gsi`, idx)
	steps := [][]usnChange{
		{{FRN: 101, ParentFRN: 100, USN: 10, Reason: usnReasonFileCreate, Name: "alpha-needle.txt"}},
		{{FRN: 102, ParentFRN: 100, USN: 11, Reason: usnReasonFileCreate, Name: "beta-needle.go"}},
		{{FRN: 102, ParentFRN: 100, USN: 13, Reason: usnReasonRenameOld, Name: "beta-needle.go"}, {FRN: 102, ParentFRN: 100, USN: 14, Reason: usnReasonRenameNew, Name: "gamma-needle.go"}},
		{{FRN: 101, USN: 15, Reason: usnReasonFileDelete}},
		{{FRN: 101, ParentFRN: 100, USN: 16, Reason: usnReasonFileCreate, Name: "reused-needle.md"}},
	}
	queries := []queryOptions{
		{Query: "needle", MatchPath: true, Limit: 20},
		{Query: "ext:go", MatchPath: true, Limit: 20},
		{Query: "type:dir", MatchPath: true, Limit: 20},
	}
	for step, changes := range steps {
		vol.applyUSNChanges(changes)
		buildOrders(vol.index)
		vol.queryIndex = buildResidentQueryIndex(vol)
		vol.clearSearchCachesLocked()
		applyLogicalUSNChanges(logical, changes)
		fresh := freshIndexFromLogicalRecords("F:", logical)
		freshVol := newServiceVolumeIndex(`F:\fresh_state.gsi`, fresh)
		for _, opts := range queries {
			got, err := searchCompactWithCache(vol.index, opts, false, vol.pathCache, vol.nameTermCandidates)
			if err != nil {
				t.Fatalf("step %d query %q mutated search: %v", step, opts.Query, err)
			}
			want, err := searchCompactWithCache(fresh, opts, false, freshVol.pathCache, freshVol.nameTermCandidates)
			if err != nil {
				t.Fatalf("step %d query %q fresh search: %v", step, opts.Query, err)
			}
			if !sameOrderedStrings(pathsOf(got), pathsOf(want)) {
				t.Fatalf("step %d query %q paths=%v want=%v", step, opts.Query, pathsOf(got), pathsOf(want))
			}
		}
	}
}

func TestEngineV9OverlayMutationStateMachineMatchesFreshOracle(t *testing.T) {
	t.Setenv("SEEKFS_ENGINE_V9", "1")
	logical := map[uint64]CompactRecord{
		100: {FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir), Size: 0},
	}
	idx := freshIndexFromLogicalRecords("F:", logical)
	vol := newServiceVolumeIndex(`F:\seekfs_state.gsi`, idx)
	steps := [][]usnChange{
		{{FRN: 101, ParentFRN: 100, USN: 10, Reason: usnReasonFileCreate, Name: "alpha-needle.txt"}},
		{{FRN: 102, ParentFRN: 100, USN: 11, Reason: usnReasonFileCreate, Name: "beta-needle.go"}},
		{{FRN: 102, ParentFRN: 100, USN: 13, Reason: usnReasonRenameOld, Name: "beta-needle.go"}, {FRN: 102, ParentFRN: 100, USN: 14, Reason: usnReasonRenameNew, Name: "gamma-needle.go"}},
		{{FRN: 103, ParentFRN: 100, USN: 15, Reason: usnReasonFileCreate, Name: "subdir", Attr: fileAttributeDir}},
		{{FRN: 104, ParentFRN: 103, USN: 16, Reason: usnReasonFileCreate, Name: "delta-needle.txt"}},
		{{FRN: 103, USN: 17, Reason: usnReasonFileDelete}},
		{{FRN: 101, USN: 18, Reason: usnReasonFileDelete}},
		{{FRN: 101, ParentFRN: 100, USN: 19, Reason: usnReasonFileCreate, Name: "aaa-needle.md"}},
		// (a) directory rename to a NEW parent: create two sibling directories,
		// then move "movable-dir" (and its child) from under "parent-a" to
		// under "parent-b" via rename-old/rename-new carrying a different
		// ParentFRN, not just a name change in place.
		{{FRN: 105, ParentFRN: 100, USN: 20, Reason: usnReasonFileCreate, Name: "parent-a", Attr: fileAttributeDir}},
		{{FRN: 106, ParentFRN: 100, USN: 21, Reason: usnReasonFileCreate, Name: "parent-b", Attr: fileAttributeDir}},
		{{FRN: 107, ParentFRN: 105, USN: 22, Reason: usnReasonFileCreate, Name: "movable-dir", Attr: fileAttributeDir}},
		{{FRN: 108, ParentFRN: 107, USN: 23, Reason: usnReasonFileCreate, Name: "epsilon-needle.txt"}},
		{
			{FRN: 107, ParentFRN: 105, USN: 24, Reason: usnReasonRenameOld, Name: "movable-dir"},
			{FRN: 107, ParentFRN: 106, USN: 25, Reason: usnReasonRenameNew, Name: "movable-dir"},
		},
		// (c) delete-then-recreate at the SAME path (same name+parent): delete
		// "gamma-needle.go" (FRN 102, currently under the root) and recreate a
		// file at the identical name+parent with a brand-new FRN.
		{{FRN: 102, USN: 26, Reason: usnReasonFileDelete}},
		{{FRN: 109, ParentFRN: 100, USN: 27, Reason: usnReasonFileCreate, Name: "gamma-needle.go"}},
	}
	queries := []queryOptions{
		{Query: "needle", MatchPath: true, Limit: 20},
		{Query: "needle", Limit: 20},
		{Query: "ext:go", MatchPath: true, Limit: 20},
		{Query: "type:dir", MatchPath: true, Limit: 20},
	}
	for step, changes := range steps {
		vol.applyUSNChanges(changes)
		applyLogicalUSNChanges(logical, changes)
		fresh := freshIndexFromLogicalRecords("F:", logical)
		freshVol := newServiceVolumeIndex(`F:\fresh_state.gsi`, fresh)
		for _, opts := range queries {
			got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
			if err != nil {
				t.Fatalf("step %d query %q v9 overlay search: %v", step, opts.Query, err)
			}
			want, err := searchCompactWithCache(fresh, opts, false, freshVol.pathCache, freshVol.nameTermCandidates)
			if err != nil {
				t.Fatalf("step %d query %q fresh search: %v", step, opts.Query, err)
			}
			if !sameOrderedStrings(pathsOf(got), pathsOf(want)) {
				t.Fatalf("step %d query %q paths=%v want=%v", step, opts.Query, pathsOf(got), pathsOf(want))
			}
		}
	}
}

func TestEngineV9ConcurrentChurnQueryStress(t *testing.T) {
	t.Setenv("SEEKFS_ENGINE_V9", "1")
	idx := freshIndexFromLogicalRecords("F:", map[uint64]CompactRecord{
		100: {FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
	})
	vol := newServiceVolumeIndex(`F:\seekfs_churn_stress.gsi`, idx)
	queries := []queryOptions{
		{Query: "needle", MatchPath: true, Limit: 20},
		{Query: "ext:txt", MatchPath: true, Limit: 20},
		{Query: "type:dir", MatchPath: true, Limit: 20},
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 8)
	for worker := 0; worker < 2; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 80; i++ {
				for _, opts := range queries {
					if _, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false); err != nil {
						errs <- fmt.Errorf("search %q: %w", opts.Query, err)
						return
					}
					if _, _, err := countServiceVolumes([]*serviceVolumeIndex{vol}, opts); err != nil {
						errs <- fmt.Errorf("count %q: %w", opts.Query, err)
						return
					}
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 80; i++ {
			frn := uint64(1000 + i)
			vol.applyUSNChanges([]usnChange{{
				FRN:       frn,
				ParentFRN: 100,
				USN:       int64(10 + i*3),
				Reason:    usnReasonFileCreate,
				Name:      fmt.Sprintf("needle-%03d.txt", i),
			}})
			if i%3 == 0 {
				vol.applyUSNChanges([]usnChange{{
					FRN:    frn,
					USN:    int64(11 + i*3),
					Reason: usnReasonFileDelete,
				}})
			}
		}
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func applyLogicalUSNChanges(logical map[uint64]CompactRecord, changes []usnChange) {
	for _, change := range changes {
		rec := logical[change.FRN]
		switch {
		case change.Reason&usnReasonFileDelete != 0:
			markLogicalDeleted(logical, change.FRN)
		case change.Reason&usnReasonRenameNew != 0 || change.Reason&usnReasonFileCreate != 0:
			rec.FRN = change.FRN
			rec.ParentFRN = change.ParentFRN
			rec.Name = change.Name
			rec.Mode = modeFromAttrs(change.Attr)
			rec.Deleted = false
			rec.Size = 1024
			rec.ModUnix = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
			logical[change.FRN] = rec
		}
	}
}

func markLogicalDeleted(logical map[uint64]CompactRecord, frn uint64) {
	rec := logical[frn]
	rec.Deleted = true
	logical[frn] = rec
	for childFRN, child := range logical {
		if childFRN == frn || child.Deleted || child.ParentFRN != frn {
			continue
		}
		markLogicalDeleted(logical, childFRN)
	}
}

func freshIndexFromLogicalRecords(volume string, logical map[uint64]CompactRecord) *Index {
	frns := make([]uint64, 0, len(logical))
	for frn := range logical {
		frns = append(frns, frn)
	}
	sort.Slice(frns, func(i, j int) bool { return frns[i] < frns[j] })
	idx := &Index{Source: "usn", Volume: volume, Compact: true, Records: make([]CompactRecord, 0, len(frns))}
	idByFRN := make(map[uint64]int32, len(frns))
	for _, frn := range frns {
		rec := logical[frn]
		if rec.Deleted {
			continue
		}
		idByFRN[frn] = int32(len(idx.Records))
		idx.Records = append(idx.Records, rec)
	}
	for i := range idx.Records {
		parentFRN := idx.Records[i].ParentFRN
		if idx.Records[i].FRN == parentFRN {
			idx.Records[i].Parent = -1
			continue
		}
		parent, ok := idByFRN[parentFRN]
		if !ok {
			idx.Records[i].Parent = -1
			continue
		}
		idx.Records[i].Parent = parent
	}
	buildOrders(idx)
	return idx
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
	if got := idx.reconstructCompactPath(1); got != `F:\parent\child.txt` {
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

func TestServiceVolumeIndexReplaysBinaryWAL(t *testing.T) {
	t.Setenv("SEEKFS_ENGINE_V9", "1")
	dir := t.TempDir()
	db := filepath.Join(dir, "test.gsi")
	if err := appendWAL(db, 12, []usnChange{{
		FRN:       102,
		ParentFRN: 100,
		USN:       12,
		Reason:    usnReasonFileCreate,
		Name:      "binary-wal-created.txt",
	}}); err != nil {
		t.Fatalf("append binary WAL: %v", err)
	}
	data, err := os.ReadFile(walPath(db))
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	if !bytes.HasPrefix(data, walMagicV1) {
		t.Fatalf("wal prefix = %q, want %q", data[:min(len(data), len(walMagicV1))], walMagicV1)
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
		t.Fatalf("replay binary WAL: %v", err)
	}
	if reloaded.checkpoint != 12 || len(reloaded.index.Records) != 1 {
		t.Fatalf("binary wal mutated base unexpectedly: checkpoint=%d records=%+v", reloaded.checkpoint, reloaded.index.Records)
	}
	if reloaded.overlay == nil || len(reloaded.overlay.records) != 1 || reloaded.overlay.records[0].Name != "binary-wal-created.txt" {
		t.Fatalf("binary wal not replayed into overlay: %+v", reloaded.overlay)
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
	} else {
		var staleErr staleIndexError
		if !errors.As(err, &staleErr) || !staleErr.rebuild {
			t.Fatalf("error = %T %v, want rebuildable staleIndexError", err, err)
		}
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
			name:      "path term plus extension",
			opts:      queryOptions{Query: "workspace ext:go", MatchPath: true, Limit: 20},
			wantNames: []string{"main.go", "search_test.go"},
		},
		{
			name:      "path term plus dotted literal",
			opts:      queryOptions{Query: "src main.go", MatchPath: true, Limit: 20},
			wantNames: []string{"main.go"},
		},
		{
			name:      "path term plus extension and dotted literal",
			opts:      queryOptions{Query: "src ext:go search_test.go", MatchPath: true, Limit: 20},
			wantNames: []string{"search_test.go"},
		},
		{
			name:      "sibling path term plus extension",
			opts:      queryOptions{Query: "downstream ext:dat", MatchPath: true, Limit: 20},
			wantNames: []string{"sibling.dat"},
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
		{Query: "workspace ext:go", MatchPath: true, Limit: 20},
		{Query: "src main.go", MatchPath: true, Limit: 20},
		{Query: "src ext:go search_test.go", MatchPath: true, Limit: 20},
		{Query: "downstream ext:dat", MatchPath: true, Limit: 20},
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

func TestServiceCandidatesMatchFullSearchWithoutResidentSortedViews(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.queryIndex.nameOrder = nil
	vol.children = nil
	vol.childOffsets = nil
	vol.childIDs = nil

	cases := []queryOptions{
		{Query: "assets dat", MatchPath: true, Limit: 20},
		{Query: "ext:dat", MatchPath: true, Under: `C:\fixture\workspace\Assets`, Limit: 20},
		{Query: "type:file glob:*.go", MatchPath: true, Under: `C:\fixture\workspace`, Limit: 20},
		{Query: "glob:*test*.go", MatchPath: true, Under: `C:\fixture\workspace`, Limit: 20},
		{Query: "src go", MatchPath: true, Limit: 20},
		{Query: "workspace ext:go", MatchPath: true, Limit: 20},
		{Query: "src main.go", MatchPath: true, Limit: 20},
		{Query: "src ext:go search_test.go", MatchPath: true, Limit: 20},
		{Query: "downstream ext:dat", MatchPath: true, Limit: 20},
		{Query: `src\main.go`, MatchPath: true, Under: `C:\fixture\workspace`, Limit: 20},
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

func TestServiceVolumeIndexUnderRootIDsWithoutResidentChildRanges(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.queryIndex.nameOrder = nil
	vol.children = nil
	vol.childOffsets = nil
	vol.childIDs = nil

	got := vol.underRootIDs(`C:\fixture\workspace\Assets`)
	if len(got) != 1 {
		t.Fatalf("underRootIDs len = %d, ids = %v; want one root", len(got), got)
	}
	if name := idx.compactRecord(got[0]).Name; name != "Assets" {
		t.Fatalf("underRootIDs root name = %q, want Assets", name)
	}
	if got := vol.underRootIDs(`C:\fixture\workspace\Missing`); len(got) != 0 {
		t.Fatalf("missing underRootIDs = %v, want none", got)
	}
}

func TestServiceVolumeIndexUnderRootIDsUsesBasenameFallback(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.queryIndex.nameOrder = nil
	vol.children = nil
	vol.childOffsets = nil
	vol.childIDs = nil
	vol.termCache = make(map[string]postingCacheEntry)
	vol.termCache["assets"] = postingCacheEntry{ids: []int{4}, gen: vol.cacheGeneration()}

	got := vol.underRootIDs(`C:\fixture\workspace\Assets`)
	if len(got) != 1 || got[0] != 3 {
		t.Fatalf("basename fallback root IDs = %v, want [3]", got)
	}
}

func TestServiceVolumeIndexUnderDescendantsCachesWithoutChildRanges(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.queryIndex.nameOrder = nil
	vol.children = nil
	vol.childOffsets = nil
	vol.childIDs = nil
	root := vol.underRootIDs(`C:\fixture\workspace`)[0]

	first := vol.underDescendants(root)
	if len(first) == 0 {
		t.Fatal("underDescendants returned no descendants")
	}
	if len(vol.underCache[root].ids) == 0 {
		t.Fatal("underDescendants did not cache the root")
	}
	second := vol.underDescendants(root)
	if !sameIntSet(first, second) {
		t.Fatalf("cached descendants = %v, want %v", second, first)
	}
}

func TestParseQuerySplitsPathLikeTermsForPathSearch(t *testing.T) {
	pq, err := parseQuery(queryOptions{Query: `cmd\seekfs\main.go`, MatchPath: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cmd", "seekfs", "main.go"}
	if !sameStringSet(pq.Terms, want) {
		t.Fatalf("terms = %v, want %v", pq.Terms, want)
	}

	pq, err = parseQuery(queryOptions{Query: `cmd\seekfs\main.go`})
	if err != nil {
		t.Fatal(err)
	}
	if !pq.MatchPath {
		t.Fatal("path-like query did not infer path mode")
	}
	if !sameStringSet(pq.Terms, want) {
		t.Fatalf("inferred path-mode terms = %v, want %v", pq.Terms, want)
	}
}

func TestExtractUnderPathArgTreatsDriveTokenAsRoot(t *testing.T) {
	args, under := extractUnderPathArg([]string{"C:", "needle"})
	if under != `C:\` {
		t.Fatalf("under = %q, want C:\\", under)
	}
	if !sameStringSet(args, []string{"needle"}) {
		t.Fatalf("args = %v, want [needle]", args)
	}
}

func TestSortCandidateIDsPrefersExactFilename(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "WidgetObserver.cpp"},
			{FRN: 3, ParentFRN: 1, Parent: 0, Name: "Widget.cpp"},
		},
	}
	buildOrders(idx)
	pq, err := parseQuery(queryOptions{Query: "Widget.cpp"})
	if err != nil {
		t.Fatal(err)
	}
	ids := []int{1, 2}
	sortCandidateIDs(ids, pq, idx, nil)
	if ids[0] != 2 {
		t.Fatalf("sorted candidates = %v, want exact Widget.cpp first", ids)
	}
}

func TestSortCandidateIDsCachedRanksMatchesFallback(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "zeta.txt"},
			{FRN: 3, ParentFRN: 1, Parent: 0, Name: "alpha.txt"},
			{FRN: 4, ParentFRN: 1, Parent: 0, Name: "middle.txt"},
		},
	}
	buildOrders(idx)
	ranks := make([]uint32, idx.compactRecordCount())
	for i := range ranks {
		ranks[i] = uint32(i)
	}
	for pos := 0; pos < compactOrderLen(idx.CompactNameOrder, idx.compactRecordCount()); pos++ {
		id := compactOrderAt(idx.CompactNameOrder, pos)
		if id >= 0 && id < len(ranks) {
			ranks[id] = uint32(pos)
		}
	}
	pq, err := parseQuery(queryOptions{Query: "txt"})
	if err != nil {
		t.Fatal(err)
	}

	fallback := []int{1, 2, -1, 3, 99}
	cached := append([]int(nil), fallback...)
	sortCandidateIDs(fallback, pq, idx, nil)
	sortCandidateIDs(cached, pq, idx, ranks)
	if !reflect.DeepEqual(cached, fallback) {
		t.Fatalf("cached sort = %v, fallback = %v", cached, fallback)
	}
}

func TestServiceVolumeIndexExactNameIDsScansWhenNameOrderSkipped(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "WidgetObserver.cpp"},
			{FRN: 3, ParentFRN: 1, Parent: 0, Name: "Widget.cpp"},
		},
	}
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.queryIndex.nameOrder = nil
	vol.exactNames = nil

	got := vol.exactNameIDs("widget.cpp")
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("exactNameIDs = %v, want [2]", got)
	}
	if _, ok := vol.termCache["\x00exact:widget.cpp"]; !ok {
		t.Fatal("exactNameIDs did not cache scanned exact result")
	}
	if got := vol.exactNameIDs("widgetobserver.cpp"); len(got) != 1 || got[0] != 1 {
		t.Fatalf("cached extension exactNameIDs = %v, want [1]", got)
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
		// Give files a non-zero size so the index advertises size capability;
		// directories keep size 0. The exact value scales with the record index
		// so size: filters have something to discriminate on.
		size := int64(0)
		if mode&uint32(os.ModeDir) == 0 {
			size = int64(len(idx.Records)+1) * 1024
		}
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       frn,
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
			Mode:      mode,
			Size:      size,
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

func sameIntSet(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	a = append([]int(nil), a...)
	b = append([]int(nil), b...)
	sort.Ints(a)
	sort.Ints(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
