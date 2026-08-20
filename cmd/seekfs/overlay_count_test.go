package main

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestOverlayAwareFastCountExcludesTombstonesIncludesOverlayCreates covers
// review G7 / plan R2.6: with an active v9 overlay, countServiceVolumes must
// not decline to the full search+merge path. It must instead exclude
// tombstoned base matches and include live overlay-created matches, and the
// result must equal a fresh-rebuild oracle's count. It also asserts the new
// overlayAwareFastCount helper itself returns ok=true and the exact count,
// so the fast route (not merely "some route") is exercised.
func TestOverlayAwareFastCountExcludesTombstonesIncludesOverlayCreates(t *testing.T) {
	vol := engineV9OverlaySearchTestVolume(t)

	logical := map[uint64]CompactRecord{
		100: {FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		101: {FRN: 101, ParentFRN: 100, Name: "needle-base.txt"},
		200: {FRN: 200, ParentFRN: 100, Name: "base-parent", Mode: uint32(os.ModeDir)},
	}

	// Tombstone the pre-existing base match, and create two new overlay
	// matches (one fresh FRN, one that reuses parent linkage) — the exact
	// shape review G7 says a naive base-only fast count would get wrong.
	changes := []usnChange{
		{FRN: 101, USN: 10, Reason: usnReasonFileDelete},
		{FRN: 301, ParentFRN: 100, USN: 11, Reason: usnReasonFileCreate, Name: "needle-created-a.txt"},
		{FRN: 302, ParentFRN: 100, USN: 12, Reason: usnReasonFileCreate, Name: "needle-created-b.txt"},
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
	if wantCount != 2 {
		t.Fatalf("sanity: fresh oracle count = %d, want 2 (base tombstoned, two overlay creates)", wantCount)
	}

	if !vol.hasActiveOverlay() {
		t.Fatalf("expected active overlay after applying USN changes")
	}

	opts := queryOptions{Query: "needle", MatchPath: true}
	pq, err := parseQuery(opts)
	if err != nil {
		t.Fatalf("parseQuery: %v", err)
	}
	fastCount, ok := vol.overlayAwareFastCount(pq)
	if !ok {
		t.Fatalf("overlayAwareFastCount declined for a query it should be able to answer exactly")
	}
	if fastCount != wantCount {
		t.Fatalf("overlayAwareFastCount = %d, want %d", fastCount, wantCount)
	}

	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, opts)
	if err != nil {
		t.Fatalf("countServiceVolumes: %v", err)
	}
	if !ok || count != wantCount {
		t.Fatalf("countServiceVolumes = (%d, %v), want (%d, true)", count, ok, wantCount)
	}
}

// TestOverlayAwareFastCountDeduplicatesReModifiedFRN covers the "overlay
// re-modify" shape: the same FRN is updated twice while the overlay is
// active (e.g. two renames of the same file). Only the latest slot for that
// FRN may count as a live match, never once per overlay slot.
func TestOverlayAwareFastCountDeduplicatesReModifiedFRN(t *testing.T) {
	vol := engineV9OverlaySearchTestVolume(t)

	logical := map[uint64]CompactRecord{
		100: {FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		101: {FRN: 101, ParentFRN: 100, Name: "needle-base.txt"},
		200: {FRN: 200, ParentFRN: 100, Name: "base-parent", Mode: uint32(os.ModeDir)},
	}

	// FRN 101 (already a "needle" match in base) is renamed twice within the
	// overlay: first to a non-matching name, then to a second matching name.
	// It must be counted exactly once, under its final name, not zero times
	// (base copy shadowed and one overlay slot skipped) and not twice (two
	// overlay slots both counted).
	changes := []usnChange{
		{FRN: 101, ParentFRN: 100, USN: 10, Reason: usnReasonRenameOld, Name: "needle-base.txt"},
		{FRN: 101, ParentFRN: 100, USN: 11, Reason: usnReasonRenameNew, Name: "renamed-away.txt"},
		{FRN: 101, ParentFRN: 100, USN: 12, Reason: usnReasonRenameOld, Name: "renamed-away.txt"},
		{FRN: 101, ParentFRN: 100, USN: 13, Reason: usnReasonRenameNew, Name: "needle-final.txt"},
	}
	vol.applyUSNChanges(changes)
	applyLogicalUSNChanges(logical, changes)

	fresh := freshIndexFromLogicalRecords("F:", logical)
	freshVol := newServiceVolumeIndex(`F:\fresh-overlay-remodify.gsi`, fresh)
	wantEntries, err := searchCompactWithCache(fresh, queryOptions{Query: "needle", MatchPath: true}, true, freshVol.pathCache, freshVol.nameTermCandidates)
	if err != nil {
		t.Fatalf("fresh oracle count search: %v", err)
	}
	wantCount := len(wantEntries)
	if wantCount != 1 {
		t.Fatalf("sanity: fresh oracle count = %d, want 1 (single re-modified FRN under final name)", wantCount)
	}

	opts := queryOptions{Query: "needle", MatchPath: true}
	pq, err := parseQuery(opts)
	if err != nil {
		t.Fatalf("parseQuery: %v", err)
	}
	fastCount, ok := vol.overlayAwareFastCount(pq)
	if !ok {
		t.Fatalf("overlayAwareFastCount declined unexpectedly")
	}
	if fastCount != wantCount {
		t.Fatalf("overlayAwareFastCount = %d, want %d (re-modified FRN must count once)", fastCount, wantCount)
	}

	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, opts)
	if err != nil {
		t.Fatalf("countServiceVolumes: %v", err)
	}
	if !ok || count != wantCount {
		t.Fatalf("countServiceVolumes = (%d, %v), want (%d, true)", count, ok, wantCount)
	}
}

// TestOverlayAwareFastCountDeclinesForRegexDeclinesToFallback covers the
// R2.6 invariant: a query shape the posting route cannot evaluate (a bare
// regex, with no ext/dir/under source to bound it) must make
// overlayAwareFastCount decline (ok=false) rather than guess, and
// countServiceVolumes must still return the oracle-correct count via the
// full search+merge fallback path.
func TestOverlayAwareFastCountDeclinesForRegexDeclinesToFallback(t *testing.T) {
	vol := engineV9OverlaySearchTestVolume(t)

	logical := map[uint64]CompactRecord{
		100: {FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		101: {FRN: 101, ParentFRN: 100, Name: "needle-base.txt"},
		200: {FRN: 200, ParentFRN: 100, Name: "base-parent", Mode: uint32(os.ModeDir)},
	}
	changes := []usnChange{
		{FRN: 101, USN: 10, Reason: usnReasonFileDelete},
		{FRN: 301, ParentFRN: 100, USN: 11, Reason: usnReasonFileCreate, Name: "needle-created.txt"},
	}
	vol.applyUSNChanges(changes)
	applyLogicalUSNChanges(logical, changes)

	fresh := freshIndexFromLogicalRecords("F:", logical)
	freshVol := newServiceVolumeIndex(`F:\fresh-overlay-regex.gsi`, fresh)
	opts := queryOptions{Query: "regex:needle", MatchPath: true}
	wantEntries, err := searchCompactWithCache(fresh, opts, true, freshVol.pathCache, freshVol.nameTermCandidates)
	if err != nil {
		t.Fatalf("fresh oracle count search: %v", err)
	}
	wantCount := len(wantEntries)
	if wantCount != 1 {
		t.Fatalf("sanity: fresh oracle count = %d, want 1", wantCount)
	}

	pq, err := parseQuery(opts)
	if err != nil {
		t.Fatalf("parseQuery: %v", err)
	}
	if _, ok := vol.overlayAwareFastCount(pq); ok {
		t.Fatalf("overlayAwareFastCount should decline for a regex query with no bounding source")
	}

	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, opts)
	if err != nil {
		t.Fatalf("countServiceVolumes: %v", err)
	}
	if !ok {
		t.Fatalf("countServiceVolumes must still resolve via the fallback path, got ok=false")
	}
	if count != wantCount {
		t.Fatalf("countServiceVolumes fallback = %d, want %d", count, wantCount)
	}
}

// TestOverlayAwareFastCountMatchesFreshOracleAcrossSeededMutations is a small
// seeded stress: apply a batch of create/delete/rename USN changes to an
// active overlay and assert overlayAwareFastCount matches the fresh-rebuild
// oracle for several query shapes the fast route is expected to handle
// (ext:, plain term, type:dir).
func TestOverlayAwareFastCountMatchesFreshOracleAcrossSeededMutations(t *testing.T) {
	vol := engineV9OverlaySearchTestVolume(t)

	logical := map[uint64]CompactRecord{
		100: {FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		101: {FRN: 101, ParentFRN: 100, Name: "needle-base.txt"},
		200: {FRN: 200, ParentFRN: 100, Name: "base-parent", Mode: uint32(os.ModeDir)},
	}

	var changes []usnChange
	nextFRN := uint64(400)
	for i := 0; i < 15; i++ {
		changes = append(changes, usnChange{
			FRN:       nextFRN,
			ParentFRN: 100,
			USN:       int64(20 + i),
			Reason:    usnReasonFileCreate,
			Name:      fmt.Sprintf("gen-%02d.dat", i),
		})
		nextFRN++
	}
	// Tombstone the original base file and a couple of the newly created ones.
	changes = append(changes,
		usnChange{FRN: 101, USN: 40, Reason: usnReasonFileDelete},
		usnChange{FRN: 400, USN: 41, Reason: usnReasonFileDelete},
		usnChange{FRN: 401, USN: 42, Reason: usnReasonFileDelete},
	)
	// New directory plus a child file under it.
	changes = append(changes,
		usnChange{FRN: 500, ParentFRN: 100, USN: 43, Reason: usnReasonFileCreate, Name: "gen-dir", Attr: fileAttributeDir},
		usnChange{FRN: 501, ParentFRN: 500, USN: 44, Reason: usnReasonFileCreate, Name: "gen-child.dat"},
	)

	vol.applyUSNChanges(changes)
	applyLogicalUSNChanges(logical, changes)

	fresh := freshIndexFromLogicalRecords("F:", logical)
	freshVol := newServiceVolumeIndex(`F:\fresh-overlay-seeded.gsi`, fresh)

	queries := []queryOptions{
		{Query: "ext:dat", MatchPath: true},
		{Query: "gen", MatchPath: true},
		{Query: "type:dir", MatchPath: true},
	}
	for _, opts := range queries {
		wantEntries, err := searchCompactWithCache(fresh, opts, true, freshVol.pathCache, freshVol.nameTermCandidates)
		if err != nil {
			t.Fatalf("query %q fresh oracle count: %v", opts.Query, err)
		}
		wantCount := len(wantEntries)

		pq, err := parseQuery(opts)
		if err != nil {
			t.Fatalf("query %q parseQuery: %v", opts.Query, err)
		}
		fastCount, ok := vol.overlayAwareFastCount(pq)
		if !ok {
			t.Fatalf("query %q: overlayAwareFastCount declined", opts.Query)
		}
		if fastCount != wantCount {
			t.Fatalf("query %q: overlayAwareFastCount = %d, want %d", opts.Query, fastCount, wantCount)
		}

		count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, opts)
		if err != nil {
			t.Fatalf("query %q countServiceVolumes: %v", opts.Query, err)
		}
		if !ok || count != wantCount {
			t.Fatalf("query %q countServiceVolumes = (%d, %v), want (%d, true)", opts.Query, count, ok, wantCount)
		}
	}
}

func BenchmarkOverlayAwareFastCountVsSearchFallback(b *testing.B) {

	const baseFiles = 200_000
	const overlayCreates = 10_000
	records := make([]CompactRecord, 0, baseFiles+2)
	records = append(records,
		CompactRecord{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		CompactRecord{FRN: 200, ParentFRN: 100, Parent: 0, Name: "bench-parent", Mode: uint32(os.ModeDir)},
	)
	for i := 0; i < baseFiles; i++ {
		records = append(records, CompactRecord{
			FRN:       uint64(1_000 + i),
			ParentFRN: 200,
			Parent:    1,
			Name:      fmt.Sprintf("bench-count-%06d.dat", i),
		})
	}
	idx := &Index{
		Version: indexVersionV9,
		Roots:   []string{`F:\`},
		BuiltAt: time.Unix(0, 123),
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: records,
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex(`F:\overlay-count-bench.gsi`, idx)

	changes := make([]usnChange, 0, overlayCreates+baseFiles/20)
	for i := 0; i < overlayCreates; i++ {
		changes = append(changes, usnChange{
			FRN:       uint64(1_000_000 + i),
			ParentFRN: 200,
			USN:       int64(10 + i),
			Reason:    usnReasonFileCreate,
			Name:      fmt.Sprintf("bench-count-created-%06d.dat", i),
		})
	}
	for i := 0; i < baseFiles; i += 20 {
		changes = append(changes, usnChange{
			FRN:    uint64(1_000 + i),
			USN:    int64(20_000 + i),
			Reason: usnReasonFileDelete,
		})
	}
	vol.applyUSNChanges(changes)
	if !vol.hasActiveOverlay() {
		b.Fatal("benchmark fixture did not create an active overlay")
	}

	opts := queryOptions{Query: "ext:dat", MatchPath: true, Limit: baseFiles + overlayCreates}
	pq, err := parseQuery(opts)
	if err != nil {
		b.Fatalf("parseQuery: %v", err)
	}
	want, ok := vol.overlayAwareFastCount(pq)
	if !ok {
		b.Fatal("overlayAwareFastCount declined benchmark query")
	}
	fallbackEntries, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
	if err != nil {
		b.Fatalf("search fallback: %v", err)
	}
	if len(fallbackEntries) != want {
		b.Fatalf("fixture mismatch: overlayAwareFastCount=%d fallback=%d", want, len(fallbackEntries))
	}

	b.ReportAllocs()
	b.Run("overlay_aware_count", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			got, ok := vol.overlayAwareFastCount(pq)
			if !ok || got != want {
				b.Fatalf("overlayAwareFastCount=(%d,%v), want (%d,true)", got, ok, want)
			}
		}
	})
	b.Run("search_merge_fallback", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
			if err != nil {
				b.Fatalf("search fallback: %v", err)
			}
			if len(got) != want {
				b.Fatalf("search fallback len=%d, want %d", len(got), want)
			}
		}
	})
}
