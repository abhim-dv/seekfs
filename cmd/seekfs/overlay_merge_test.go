package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// buildOverlayLimitFixture creates a v9 mmap-backed base index with `matchCount`
// matching records named needle-NN.txt plus a handful of non-matching noise
// records, all persisted as base (not overlay) records. Records are named so
// that base name-order sorts needle-00.txt < needle-01.txt < ... which is also
// the order searchCompactWithCache returns them in (ascending name rank).
func buildOverlayLimitFixture(t *testing.T, matchCount int) *serviceVolumeIndex {
	t.Helper()
	records := []CompactRecord{{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}}
	for i := 0; i < matchCount; i++ {
		records = append(records, CompactRecord{
			FRN:       uint64(200 + i),
			ParentFRN: 100,
			Parent:    0,
			Name:      fmt.Sprintf("needle-%02d.txt", i),
		})
	}
	for i := 0; i < 5; i++ {
		records = append(records, CompactRecord{
			FRN:       uint64(900 + i),
			ParentFRN: 100,
			Parent:    0,
			Name:      fmt.Sprintf("noise-%02d.log", i),
		})
	}
	idx := &Index{
		Version: indexVersionV9,
		Roots:   []string{`F:\`},
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: records,
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "overlay-limit.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v9 overlay limit fixture: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load v9 overlay limit fixture: %v", err)
	}
	t.Cleanup(func() {
		if loaded.MMapRecords != nil {
			_ = loaded.MMapRecords.file.close()
		}
	})
	return newServiceVolumeIndex(db, loaded)
}

// TestOverlayMergeUnderfillBackfillsFromLaterBaseMatches covers review G2 /
// plan R2.3a: seed more matches than the limit, tombstone some of the first
// `limit` base matches via overlay deletes (not persisted), and assert the
// response still returns a full `limit` results (backfilled from later base
// matches) rather than `limit - k`. Result set is compared against a fresh
// rebuild oracle for both membership and order.
func TestOverlayMergeUnderfillBackfillsFromLaterBaseMatches(t *testing.T) {
	const matchCount = 40
	const limit = 25
	const deleteCount = 10
	vol := buildOverlayLimitFixture(t, matchCount)

	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "needle", MatchPath: true, Limit: limit}, false)
	if err != nil {
		t.Fatalf("initial search: %v", err)
	}
	if len(pathsOf(got)) != limit {
		t.Fatalf("initial search returned %d paths, want %d: %v", len(pathsOf(got)), limit, pathsOf(got))
	}

	changes := make([]usnChange, 0, deleteCount)
	logical := make(map[uint64]CompactRecord, matchCount+6)
	logical[100] = CompactRecord{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}
	for i := 0; i < matchCount; i++ {
		logical[uint64(200+i)] = CompactRecord{FRN: uint64(200 + i), ParentFRN: 100, Name: fmt.Sprintf("needle-%02d.txt", i)}
	}
	for i := 0; i < 5; i++ {
		logical[uint64(900+i)] = CompactRecord{FRN: uint64(900 + i), ParentFRN: 100, Name: fmt.Sprintf("noise-%02d.log", i)}
	}
	for i := 0; i < deleteCount; i++ {
		frn := uint64(200 + i)
		changes = append(changes, usnChange{FRN: frn, USN: int64(10 + i), Reason: usnReasonFileDelete})
	}
	vol.applyUSNChanges(changes)
	applyLogicalUSNChanges(logical, changes)

	got, err = searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "needle", MatchPath: true, Limit: limit}, false)
	if err != nil {
		t.Fatalf("post-delete search: %v", err)
	}
	gotPaths := pathsOf(got)
	if len(gotPaths) != limit {
		t.Fatalf("post-delete search returned %d paths, want %d (underfill): %v", len(gotPaths), limit, gotPaths)
	}
	for i := 0; i < deleteCount; i++ {
		deleted := fmt.Sprintf(`F:\needle-%02d.txt`, i)
		for _, p := range gotPaths {
			if p == deleted {
				t.Fatalf("tombstoned path %s leaked into results: %v", deleted, gotPaths)
			}
		}
	}
	backfilled := false
	for _, p := range gotPaths {
		if p == `F:\needle-30.txt` {
			backfilled = true
			break
		}
	}
	if !backfilled {
		t.Fatalf("later base match beyond original limit did not backfill the gap: %v", gotPaths)
	}

	fresh := freshIndexFromLogicalRecords("F:", logical)
	freshVol := newServiceVolumeIndex(`F:\fresh-overlay-limit.gsi`, fresh)
	want, err := searchCompactWithCache(fresh, queryOptions{Query: "needle", MatchPath: true, Limit: limit}, false, freshVol.pathCache, freshVol.nameTermCandidates)
	if err != nil {
		t.Fatalf("fresh oracle search: %v", err)
	}
	if !sameOrderedStrings(gotPaths, pathsOf(want)) {
		t.Fatalf("post-delete paths=%v want=%v", gotPaths, pathsOf(want))
	}
}

// TestOverlayMergeRanksNewCreateFirstWithinFilledLimit covers review G3: when
// base matches already fill the query limit, a newly created overlay file
// whose name ranks first among all matches must appear at its correctly
// ranked position (first) in the limited response, not appended at the end
// or dropped because the limit was already full.
func TestOverlayMergeRanksNewCreateFirstWithinFilledLimit(t *testing.T) {
	const matchCount = 30
	const limit = 25
	vol := buildOverlayLimitFixture(t, matchCount)

	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "needle", MatchPath: true, Limit: limit}, false)
	if err != nil {
		t.Fatalf("initial search: %v", err)
	}
	if len(pathsOf(got)) != limit {
		t.Fatalf("initial search returned %d paths, want %d: %v", len(pathsOf(got)), limit, pathsOf(got))
	}

	vol.applyUSNChanges([]usnChange{{
		FRN:       999,
		ParentFRN: 100,
		USN:       500,
		Reason:    usnReasonFileCreate,
		Name:      "aaa-needle.txt",
	}})

	got, err = searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "needle", MatchPath: true, Limit: limit}, false)
	if err != nil {
		t.Fatalf("post-create search: %v", err)
	}
	paths := pathsOf(got)
	if len(paths) != limit {
		t.Fatalf("post-create search returned %d paths, want filled limit %d: %v", len(paths), limit, paths)
	}
	if paths[0] != `F:\aaa-needle.txt` {
		t.Fatalf("newly created overlay entry not ranked first: paths[0]=%q paths=%v", paths[0], paths)
	}
}

// TestOverlayMergeCountMatchesFreshOracleUnderTombstones covers count-only
// parity for the same underfill/tombstone shape as
// TestOverlayMergeUnderfillBackfillsFromLaterBaseMatches: the count-only
// search path and the overlay-aware fast count path must both match a fresh
// rebuild oracle's count after deletes land only in the overlay.
func TestOverlayMergeCountMatchesFreshOracleUnderTombstones(t *testing.T) {
	const matchCount = 30
	const deleteCount = 10
	vol := buildOverlayLimitFixture(t, matchCount)

	logical := make(map[uint64]CompactRecord, matchCount+6)
	logical[100] = CompactRecord{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}
	for i := 0; i < matchCount; i++ {
		logical[uint64(200+i)] = CompactRecord{FRN: uint64(200 + i), ParentFRN: 100, Name: fmt.Sprintf("needle-%02d.txt", i)}
	}
	for i := 0; i < 5; i++ {
		logical[uint64(900+i)] = CompactRecord{FRN: uint64(900 + i), ParentFRN: 100, Name: fmt.Sprintf("noise-%02d.log", i)}
	}
	changes := make([]usnChange, 0, deleteCount)
	for i := 0; i < deleteCount; i++ {
		changes = append(changes, usnChange{FRN: uint64(200 + i), USN: int64(10 + i), Reason: usnReasonFileDelete})
	}
	vol.applyUSNChanges(changes)
	applyLogicalUSNChanges(logical, changes)

	fresh := freshIndexFromLogicalRecords("F:", logical)
	freshVol := newServiceVolumeIndex(`F:\fresh-overlay-count.gsi`, fresh)
	wantEntries, err := searchCompactWithCache(fresh, queryOptions{Query: "needle", MatchPath: true}, true, freshVol.pathCache, freshVol.nameTermCandidates)
	if err != nil {
		t.Fatalf("fresh oracle count search: %v", err)
	}
	wantCount := len(wantEntries)
	if wantCount != matchCount-deleteCount {
		t.Fatalf("sanity: fresh oracle count = %d, want %d", wantCount, matchCount-deleteCount)
	}

	gotEntries, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "needle", MatchPath: true}, true)
	if err != nil {
		t.Fatalf("count-only overlay search: %v", err)
	}
	if len(gotEntries) != wantCount {
		t.Fatalf("count-only overlay search = %d, want %d matching fresh oracle", len(gotEntries), wantCount)
	}

	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "needle", MatchPath: true})
	if err != nil {
		t.Fatalf("overlay-aware fast count: %v", err)
	}
	if !ok || count != wantCount {
		t.Fatalf("overlay-aware fast count = (%d, %v), want (%d, true)", count, ok, wantCount)
	}
}

// TestOverlayDirectoryDeleteWithoutPerChildUSNLeavesOverlayOnlyChildVisible
// probes review F4's residual concern precisely: a directory subtree is
// created and persisted as BASE records (grandparent "doomed-dir" containing
// base child "mid-dir"), then a NEW grandchild file is created under the
// base child ONLY via overlay (USN create, never persisted). The top-level
// grandparent then receives a single USN delete with NO accompanying
// per-child delete USN record for "mid-dir" or the overlay grandchild
// (simulating a journal gap where only the top-level deleted path is
// reported).
//
// tombstoneBaseSubtree walks base parent/child links (subtreeOrder /
// vol.children) and correctly marks "mid-dir" itself tombstoned by id, but it
// never visits or creates any overlay record for "mid-dir" (it has no
// overlay slot of its own — only the grandparent FRN gets one from
// recordOverlayChange). overlayRecordPath, when reconstructing the overlay
// grandchild's path, looks up its ParentFRN ("mid-dir") in the overlay
// `latest` map first; finding no overlay slot there, it falls through to
// idForFRN + reconstructCompactPathCached against the base index, which
// does not consult vol.overlay.tombstone/Deleted at all. So the overlay-only
// grandchild is expected to remain visible after the parent delete: this
// documents a real, reproducible engine gap rather than a passing invariant.
func TestOverlayDirectoryDeleteWithoutPerChildUSNLeavesOverlayOnlyChildVisible(t *testing.T) {
	idx := &Index{
		Version: indexVersionV9,
		Roots:   []string{`F:\`},
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 200, ParentFRN: 100, Parent: 0, Name: "doomed-dir", Mode: uint32(os.ModeDir)},
			{FRN: 210, ParentFRN: 200, Parent: 1, Name: "mid-dir", Mode: uint32(os.ModeDir)},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "f4-residual.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save fixture: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	t.Cleanup(func() {
		if loaded.MMapRecords != nil {
			_ = loaded.MMapRecords.file.close()
		}
	})
	vol := newServiceVolumeIndex(db, loaded)

	// Overlay-only grandchild created under the base "mid-dir"; never persisted.
	vol.applyUSNChanges([]usnChange{{
		FRN:       211,
		ParentFRN: 210,
		USN:       20,
		Reason:    usnReasonFileCreate,
		Name:      "overlay-only-child.txt",
	}})

	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "overlay-only-child", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search before parent delete: %v", err)
	}
	if !sameStringSet(pathsOf(got), []string{`F:\doomed-dir\mid-dir\overlay-only-child.txt`}) {
		t.Fatalf("overlay child not visible before parent delete: %v", pathsOf(got))
	}

	// Top-level grandparent directory delete arrives with NO per-child USN
	// record for "mid-dir" or the overlay grandchild (journal gap).
	vol.applyUSNChanges([]usnChange{{FRN: 200, USN: 21, Reason: usnReasonFileDelete}})

	got, err = searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "overlay-only-child", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search after parent delete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("overlay-only grandchild should be tombstoned when an ancestor subtree is deleted, got: %v", pathsOf(got))
	}
}

// TestOverlayFileDeleteBurstMatchesFreshOracle covers the performance fix for
// recordOverlayChange's delete branch: cascadeOverlayDelete is a full reverse
// pass over overlay.records plus an ancestor-chain walk, which is only
// necessary because directories can have overlay-only descendants whose
// visibility depends on the deleted ancestor. Plain file deletes can never
// have descendants, so the delete branch now skips the cascade call whenever
// the deleted FRN is provably a file (via its base record's Mode, or, if it
// has no base record, its prior overlay slot's Mode). This burst applies
// ~500 plain file deletes (mixed base-record and overlay-created files) and
// asserts search/count parity against a fresh-rebuild oracle, then deletes
// one directory in the same burst and asserts its overlay-only descendant
// disappears, proving the guard still cascades for directories.
func TestOverlayFileDeleteBurstMatchesFreshOracle(t *testing.T) {
	const baseFileCount = 300
	const overlayFileCount = 300
	const deleteBaseCount = 250
	const deleteOverlayCount = 250

	logical := map[uint64]CompactRecord{
		100: {FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		101: {FRN: 101, ParentFRN: 100, Parent: 0, Name: "dir-a", Mode: uint32(os.ModeDir)},
		102: {FRN: 102, ParentFRN: 100, Parent: 0, Name: "dir-b", Mode: uint32(os.ModeDir)},
	}
	records := make([]CompactRecord, 0, len(logical)+baseFileCount)
	for _, frn := range []uint64{100, 101, 102} {
		records = append(records, logical[frn])
	}
	for i := 0; i < baseFileCount; i++ {
		parent := uint64(101)
		parentIdx := int32(1)
		if i%2 == 1 {
			parent = 102
			parentIdx = 2
		}
		rec := CompactRecord{FRN: uint64(200 + i), ParentFRN: parent, Parent: parentIdx, Name: fmt.Sprintf("needle-base-%03d.txt", i)}
		records = append(records, rec)
		logical[rec.FRN] = rec
	}

	idx := &Index{
		Version: indexVersionV9,
		Roots:   []string{`F:\`},
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: records,
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "delete-burst.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v9 delete burst fixture: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load v9 delete burst fixture: %v", err)
	}
	t.Cleanup(func() {
		if loaded.MMapRecords != nil {
			_ = loaded.MMapRecords.file.close()
		}
	})
	vol := newServiceVolumeIndex(db, loaded)

	// Create overlay-only files (never persisted to base) under the same
	// two directories, mixed in with the base files for query purposes.
	createChanges := make([]usnChange, 0, overlayFileCount)
	usn := int64(1000)
	for i := 0; i < overlayFileCount; i++ {
		parent := uint64(101)
		if i%2 == 1 {
			parent = 102
		}
		frn := uint64(5000 + i)
		createChanges = append(createChanges, usnChange{
			FRN:       frn,
			ParentFRN: parent,
			USN:       usn,
			Reason:    usnReasonFileCreate,
			Name:      fmt.Sprintf("needle-overlay-%03d.txt", i),
		})
		usn++
	}
	vol.applyUSNChanges(createChanges)
	applyLogicalUSNChanges(logical, createChanges)

	// One overlay-only descendant under a directory that will be deleted in
	// the same burst below, used to prove the cascade still runs for dirs.
	dirDescendantChange := []usnChange{{
		FRN:       6000,
		ParentFRN: 101,
		USN:       usn,
		Reason:    usnReasonFileCreate,
		Name:      "doomed-dir-child.txt",
	}}
	usn++
	vol.applyUSNChanges(dirDescendantChange)
	applyLogicalUSNChanges(logical, dirDescendantChange)

	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "doomed-dir-child", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search before dir delete: %v", err)
	}
	if !sameStringSet(pathsOf(got), []string{`F:\dir-a\doomed-dir-child.txt`}) {
		t.Fatalf("overlay dir-descendant not visible before delete burst: %v", pathsOf(got))
	}

	// Burst: ~500 plain file deletes (mix of base and overlay-created files),
	// plus one directory delete (dir-a), all in a single apply batch.
	deleteChanges := make([]usnChange, 0, deleteBaseCount+deleteOverlayCount+1)
	for i := 0; i < deleteBaseCount; i++ {
		deleteChanges = append(deleteChanges, usnChange{FRN: uint64(200 + i), USN: usn, Reason: usnReasonFileDelete})
		usn++
	}
	for i := 0; i < deleteOverlayCount; i++ {
		deleteChanges = append(deleteChanges, usnChange{FRN: uint64(5000 + i), USN: usn, Reason: usnReasonFileDelete})
		usn++
	}
	deleteChanges = append(deleteChanges, usnChange{FRN: 101, USN: usn, Reason: usnReasonFileDelete})
	usn++
	vol.applyUSNChanges(deleteChanges)
	applyLogicalUSNChanges(logical, deleteChanges)

	fresh := freshIndexFromLogicalRecords("F:", logical)
	freshVol := newServiceVolumeIndex(`F:\fresh-delete-burst.gsi`, fresh)

	got, err = searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "needle", MatchPath: true, Limit: 1000}, false)
	if err != nil {
		t.Fatalf("post-burst search: %v", err)
	}
	want, err := searchCompactWithCache(fresh, queryOptions{Query: "needle", MatchPath: true, Limit: 1000}, false, freshVol.pathCache, freshVol.nameTermCandidates)
	if err != nil {
		t.Fatalf("fresh oracle search: %v", err)
	}
	if !sameOrderedStrings(pathsOf(got), pathsOf(want)) {
		t.Fatalf("post-burst paths=%v want=%v", pathsOf(got), pathsOf(want))
	}

	gotCount, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "needle", MatchPath: true}, true)
	if err != nil {
		t.Fatalf("post-burst count search: %v", err)
	}
	wantCount, err := searchCompactWithCache(fresh, queryOptions{Query: "needle", MatchPath: true}, true, freshVol.pathCache, freshVol.nameTermCandidates)
	if err != nil {
		t.Fatalf("fresh oracle count search: %v", err)
	}
	if len(gotCount) != len(wantCount) {
		t.Fatalf("post-burst count = %d, want %d matching fresh oracle", len(gotCount), len(wantCount))
	}

	gotDoomed, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "doomed-dir-child", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search after dir delete: %v", err)
	}
	if len(gotDoomed) != 0 {
		t.Fatalf("overlay-only descendant of deleted directory should be gone, got: %v", pathsOf(gotDoomed))
	}
}
