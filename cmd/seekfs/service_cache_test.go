package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServiceVolumeIndexAfterPersistClearsRecentAndSearchCaches(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	vol.recentIDs = map[int]struct{}{1: {}, 2: {}}
	vol.pathCache = map[int]string{1: "cached-path"}
	vol.termCache = map[string][]int{"one": {1}}
	vol.pathTermCache = map[string][]int{"fixture": {1}}
	vol.extCache = map[string][]int{".txt": {1}}
	vol.termSeq = map[string]uint64{"one": 1}
	vol.pathTermSeq = map[string]uint64{"fixture": 1}
	vol.extSeq = map[string]uint64{".txt": 1}

	vol.afterPersist()

	if vol.recentIDs != nil {
		t.Fatalf("recentIDs = %#v, want nil", vol.recentIDs)
	}
	if len(vol.pathCache) != 0 {
		t.Fatalf("pathCache len = %d, want 0", len(vol.pathCache))
	}
	if vol.termCache != nil || vol.pathTermCache != nil || vol.extCache != nil {
		t.Fatalf("term caches were not cleared: term=%v pathTerm=%v ext=%v", vol.termCache, vol.pathTermCache, vol.extCache)
	}
	if vol.termSeq != nil || vol.pathTermSeq != nil || vol.extSeq != nil {
		t.Fatalf("term cache sequences were not cleared: term=%v pathTerm=%v ext=%v", vol.termSeq, vol.pathTermSeq, vol.extSeq)
	}
	if vol.recentSeq != 1 {
		t.Fatalf("recentSeq = %d, want 1", vol.recentSeq)
	}
}

func TestServiceVolumeIndexTrimSearchCachesLockedClearsOversizedCaches(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	for i := 0; i <= servicePathCacheLimit; i++ {
		vol.pathCache[i] = "cached-path"
	}
	for i := 0; i <= serviceTermCacheLimit; i++ {
		key := string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
		vol.termCache[key] = []int{i}
	}
	vol.pathTermCache = map[string][]int{"path": {1}}
	vol.extCache = map[string][]int{".txt": {1}}
	vol.termSeq = map[string]uint64{"term": 1}
	vol.pathTermSeq = map[string]uint64{"path": 1}
	vol.extSeq = map[string]uint64{".txt": 1}

	vol.trimSearchCachesLocked()

	if len(vol.pathCache) != 0 {
		t.Fatalf("pathCache len = %d, want 0", len(vol.pathCache))
	}
	if vol.termCache != nil || vol.pathTermCache != nil || vol.extCache != nil {
		t.Fatalf("term caches were not cleared: term=%v pathTerm=%v ext=%v", vol.termCache, vol.pathTermCache, vol.extCache)
	}
	if vol.termSeq != nil || vol.pathTermSeq != nil || vol.extSeq != nil {
		t.Fatalf("term cache sequences were not cleared: term=%v pathTerm=%v ext=%v", vol.termSeq, vol.pathTermSeq, vol.extSeq)
	}
}

func TestServiceVolumeIndexFastPostingCount(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	pq, err := parseQuery(queryOptions{Query: "type:file ext:go c:", MatchPath: true})
	if err != nil {
		t.Fatal(err)
	}
	count, ok := vol.fastPostingCount(pq)
	if !ok {
		t.Fatal("fastPostingCount declined safe posting query")
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestServiceVolumeIndexFastPostingCountUsesSimpleExtensionGlob(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	pq, err := parseQuery(queryOptions{Query: "type:file glob:*.go"})
	if err != nil {
		t.Fatal(err)
	}
	count, ok := vol.fastPostingCount(pq)
	if !ok {
		t.Fatal("fastPostingCount declined simple extension glob")
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestSimpleGlobExtsDeclinesComplexGlobs(t *testing.T) {
	if got, ok := simpleGlobExts([]string{"*.go", "*.md"}); !ok || len(got) != 2 || got[0] != "go" || got[1] != "md" {
		t.Fatalf("simpleGlobExts = %v, %v; want [go md], true", got, ok)
	}
	if _, ok := simpleGlobExts([]string{"*test*.go"}); ok {
		t.Fatal("simpleGlobExts accepted complex wildcard glob")
	}
	if got := complexGlobExts([]string{"*test*.go", "README.*"}); len(got) != 1 || got[0] != "go" {
		t.Fatalf("complexGlobExts = %v, want [go]", got)
	}
}

func TestServiceSubtreeIntervalsDisabledByDefault(t *testing.T) {
	t.Setenv("SEEKFS_SUBTREE_INTERVALS", "")
	if serviceSubtreeIntervalsEnabled() {
		t.Fatal("subtree intervals should be disabled by default")
	}
	t.Setenv("SEEKFS_SUBTREE_INTERVALS", "1")
	if !serviceSubtreeIntervalsEnabled() {
		t.Fatal("subtree intervals should be enabled by explicit opt-in")
	}
}

func TestServicePathGramsDisabledByDefault(t *testing.T) {
	t.Setenv("SEEKFS_PATH_GRAMS", "")
	if servicePathGramsEnabled() {
		t.Fatal("path grams should be disabled by default")
	}
	t.Setenv("SEEKFS_PATH_GRAMS", "true")
	if !servicePathGramsEnabled() {
		t.Fatal("path grams should be enabled by explicit opt-in")
	}
}

func TestServiceVolumesForQuerySkipsStaleNonMatchingUnderVolume(t *testing.T) {
	c := &serviceVolumeIndex{index: &Index{Volume: "C:"}, state: "stale", staleReason: "old checkpoint"}
	f := &serviceVolumeIndex{index: &Index{Volume: "F:"}, state: "ready"}
	got, err := serviceVolumesForQuery([]*serviceVolumeIndex{c, f}, queryOptions{Under: `F:\workspace\project`})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != f {
		t.Fatalf("volumes = %+v, want only ready F: volume", got)
	}
}

func TestServiceVolumesForQueryUsesLoadedStaleVolume(t *testing.T) {
	c := &serviceVolumeIndex{index: &Index{Volume: "C:"}, state: "stale", staleReason: "old checkpoint"}
	got, err := serviceVolumesForQuery([]*serviceVolumeIndex{c}, queryOptions{Under: `C:\Data`})
	if err != nil {
		t.Fatalf("error = %v, want loaded stale volume to remain searchable", err)
	}
	if len(got) != 1 || got[0] != c {
		t.Fatalf("volumes = %+v, want stale C: volume", got)
	}
}

func TestServiceVolumesForQueryRoutesPathVolumeTerm(t *testing.T) {
	c := &serviceVolumeIndex{index: &Index{Volume: "C:"}, state: "ready"}
	f := &serviceVolumeIndex{index: &Index{Volume: "F:"}, state: "ready"}
	cases := []string{
		"path:F: .nrrd",
		"path:F: .raw",
		"path:F: .pdf",
		"F: ext:pdf",
	}
	for _, query := range cases {
		t.Run(query, func(t *testing.T) {
			got, err := serviceVolumesForQuery([]*serviceVolumeIndex{c, f}, queryOptions{Query: query, MatchPath: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0] != f {
				t.Fatalf("volumes = %+v, want only F: volume", got)
			}
		})
	}
}

func TestServiceVolumesForQueryKeepsAllVolumesWithoutUniqueVolumeTerm(t *testing.T) {
	c := &serviceVolumeIndex{index: &Index{Volume: "C:"}, state: "ready"}
	f := &serviceVolumeIndex{index: &Index{Volume: "F:"}, state: "ready"}
	got, err := serviceVolumesForQuery([]*serviceVolumeIndex{c, f}, queryOptions{Query: "path:C:|F: .pdf", MatchPath: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("volumes = %+v, want both volumes for ambiguous volume query", got)
	}
}

func TestLockVolumeSearchCancelsWhileWaiting(t *testing.T) {
	vol := &serviceVolumeIndex{}
	vol.searchMu.Lock()
	defer vol.searchMu.Unlock()
	start := time.Now()
	locked := lockVolumeSearch(vol, queryOptions{
		Cancel: func() bool { return true },
	})
	if locked {
		t.Fatal("lockVolumeSearch acquired lock for canceled query")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("lockVolumeSearch took %s for canceled query, want quick return", elapsed)
	}
}

func TestLooksLikeSearchWithoutSubcommand(t *testing.T) {
	if !looksLikeSearchWithoutSubcommand([]string{"ExamplePanel.cpp", "--under", `F:\workspace\app`}) {
		t.Fatal("expected omitted search command to be recognized")
	}
	if looksLikeSearchWithoutSubcommand([]string{"bogus"}) {
		t.Fatal("single unknown command should not be treated as omitted search")
	}
}

func TestNormalizeSearchArgsMovesFlagsBeforeQuery(t *testing.T) {
	got := normalizeSearchArgs([]string{"ExamplePanel.cpp", "--under", `F:\workspace\app`, "-path"})
	want := []string{"--under", `F:\workspace\app`, "-path", "ExamplePanel.cpp"}
	if !sameStringSet(got, want) || strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("normalized args = %v, want %v", got, want)
	}
}

func TestServiceVolumeIndexFastPostingCountIncludesRecentAndDeleted(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	vol.recentIDs = map[int]struct{}{1: {}, 3: {}}
	rec := vol.index.compactRecord(1)
	rec.Deleted = true
	vol.index.setCompactRecord(1, rec)
	vol.index.appendCompactRecord(CompactRecord{FRN: 4, ParentFRN: 1, Parent: 0, Name: "new.go"})

	pq, err := parseQuery(queryOptions{Query: "type:file ext:go c:", MatchPath: true})
	if err != nil {
		t.Fatal(err)
	}
	count, ok := vol.fastPostingCount(pq)
	if !ok {
		t.Fatal("fastPostingCount declined safe posting query")
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}
}

func TestServiceVolumeIndexFastPostingCountHandlesDirQueryWithPlanner(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	pq, err := parseQuery(queryOptions{Query: "dir:src ext:go", MatchPath: true})
	if err != nil {
		t.Fatal(err)
	}
	count, ok := vol.fastPostingCount(pq)
	if !ok {
		t.Fatal("fastPostingCount declined planned dir query")
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}
}

func TestServiceVolumeIndexFastPostingCountDeclinesBareFileType(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	pq, err := parseQuery(queryOptions{Query: "type:file"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := vol.fastPostingCount(pq); ok {
		t.Fatal("fastPostingCount accepted bare type:file without a narrowing posting")
	}
}

func TestServiceVolumeIndexMultiNameTermCandidatesWarmsEachTerm(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	vol.index.appendCompactRecord(CompactRecord{FRN: 4, ParentFRN: 1, Parent: 0, Name: "alpha_report.txt"})
	vol.queryIndex = buildResidentQueryIndex(vol)

	got := vol.multiNameTermCandidates([]string{"alpha", "report"})
	if len(got) != 1 || got[0] != 3 {
		t.Fatalf("candidates = %v, want [3]", got)
	}
	if len(vol.termCache["alpha"]) != 1 || len(vol.termCache["report"]) != 1 {
		t.Fatalf("term cache not warmed for both terms: %#v", vol.termCache)
	}
	cached, ok := vol.cachedMultiNameTermCandidates([]string{"alpha", "report"})
	if !ok {
		t.Fatal("cachedMultiNameTermCandidates missed warmed terms")
	}
	if len(cached) != 1 || cached[0] != 3 {
		t.Fatalf("cached candidates = %v, want [3]", cached)
	}
}

func TestServiceVolumeIndexPathTermPostingFallsBackWithoutChildRanges(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	vol.children = nil
	vol.childOffsets = nil
	vol.childIDs = nil

	got := vol.pathTermPosting("main")
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("pathTermPosting fallback = %v, want [2]", got)
	}
}

func TestServiceVolumeIndexResidentMemoryInfoReflectsSkippedViews(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	vol.index.packCompactRecords(true)
	vol.queryIndex.nameOrder = nil
	vol.children = nil
	vol.childOffsets = nil
	vol.childIDs = nil

	info := vol.residentMemoryInfo()
	if info == nil {
		t.Fatal("residentMemoryInfo returned nil")
	}
	if info.Records != vol.index.compactRecordCount() {
		t.Fatalf("records = %d, want %d", info.Records, vol.index.compactRecordCount())
	}
	if info.NameBlobBytes == 0 || info.RecordBytes == 0 {
		t.Fatalf("expected record memory fields to be populated: %+v", info)
	}
	if info.NameOrderBytes != 0 || info.ChildBytes != 0 {
		t.Fatalf("skipped resident views still reported memory: %+v", info)
	}
	if info.ExtPostBytes == 0 {
		t.Fatalf("expected extension posting memory field: %+v", info)
	}
	if info.FRNIndexBytes == 0 || info.KnownBytes == 0 {
		t.Fatalf("expected FRN and known memory fields: %+v", info)
	}
}

func TestServiceVolumeIndexPersistFailureBackoffRetainsQueryableState(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	vol.state = "ready"
	vol.staleReason = ""
	now := time.Unix(100, 0)

	vol.notePersistFailureLocked(errors.New("not enough space on the disk"), now)

	if vol.state != "ready" || vol.staleReason != "" {
		t.Fatalf("persist failure changed query state: state=%q stale=%q", vol.state, vol.staleReason)
	}
	if vol.persistFailures != 1 {
		t.Fatalf("persistFailures = %d, want 1", vol.persistFailures)
	}
	if got, want := vol.persistRetryAfter.Sub(now), time.Minute; got != want {
		t.Fatalf("retry delay = %s, want %s", got, want)
	}
	if vol.lastPersistErr == "" {
		t.Fatal("lastPersistErr was not recorded")
	}
}

func TestPersistFailureBackoffCaps(t *testing.T) {
	if got := persistFailureBackoff(1); got != time.Minute {
		t.Fatalf("first backoff = %s, want 1m", got)
	}
	if got := persistFailureBackoff(9); got != 32*time.Minute {
		t.Fatalf("capped backoff = %s, want 32m", got)
	}
}

func TestServiceWildcardUnderQueryMatchesWithoutPathMode(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)

	got, err := searchCompactWithCache(idx, queryOptions{
		Query: "*_test.go",
		Under: `C:\fixture\workspace\src`,
		Limit: 20,
	}, false, make(map[int]string), vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "search_test.go" {
		t.Fatalf("matches = %+v, want [search_test.go]", got)
	}
}

func TestUnderQueryFallsBackToPathFilterWhenRootRecordIsDeleted(t *testing.T) {
	idx := commonSearchFixture()
	root := idx.compactRecord(2) // C:\fixture\workspace
	root.Deleted = true
	idx.setCompactRecord(2, root)
	vol := newServiceVolumeIndex("fixture.gsi", idx)

	got, err := searchCompactWithCache(idx, queryOptions{
		Query: "readme",
		Under: `C:\fixture\workspace`,
		Limit: 20,
	}, false, make(map[int]string), vol.plannedCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(got); !sameStringSet(names, []string{"README.md", "readme.md"}) {
		t.Fatalf("matches = %v, want readme files under deleted root record", names)
	}
}

func TestServiceSearchFallsBackToFilesystemForScopedIndexMiss(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "specific_fixture_tool.py"), []byte("pass"), 0o644); err != nil {
		t.Fatal(err)
	}
	volume := filepath.VolumeName(root)
	idx := &Index{
		Source:  "usn",
		Volume:  volume,
		Compact: true,
		Records: []CompactRecord{{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)

	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
		Query: "specific_fixture_tool.py",
		Under: root,
		Limit: 20,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "specific_fixture_tool.py" {
		t.Fatalf("matches = %+v, want filesystem fallback result", got)
	}
}

func TestFilesystemUnderFallbackIsBounded(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 12; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("item-%02d.txt", i)), []byte("pass"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := filesystemUnderFallbackSearchLimited(queryOptions{
		Query: "item-11.txt",
		Under: root,
		Limit: 20,
	}, false, 3, time.Minute)
	if ok {
		t.Fatalf("fallback returned %v after maxVisited cap; want no result", got)
	}
}

func TestExchangeServiceJSONTimesOutHungQuery(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		defer server.Close()
		var req serviceRequest
		_ = json.NewDecoder(server).Decode(&req)
		time.Sleep(200 * time.Millisecond)
	}()

	start := time.Now()
	_, err := exchangeServiceJSON(client, serviceRequest{Command: "search"}, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("timeout elapsed = %s, want under 150ms", elapsed)
	}
}

func TestReplayWALWithLimitMarksOversizedWALRebuildable(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.gsi")
	if err := os.WriteFile(walPath(db), []byte("oversized wal"), 0o644); err != nil {
		t.Fatal(err)
	}
	vol := &serviceVolumeIndex{dbPath: db, volume: "F:"}
	err := vol.replayWALWithLimit(4)
	if err == nil {
		t.Fatal("expected oversized wal error")
	}
	if !shouldRebuildStaleIndex(err) {
		t.Fatalf("err = %v, want rebuildable stale index error", err)
	}
}

func TestServiceCallTimeoutLeavesIndexRequestsUnlimited(t *testing.T) {
	if got := serviceCallTimeout(serviceRequest{Command: "search"}); got != serviceQueryTimeout {
		t.Fatalf("search timeout = %s, want %s", got, serviceQueryTimeout)
	}
	if got := serviceCallTimeout(serviceRequest{Command: "index-usn"}); got != 0 {
		t.Fatalf("index-usn timeout = %s, want 0", got)
	}
}

func TestServiceRequestRoundTripPreservesControlFields(t *testing.T) {
	opts := queryOptions{
		Query:        "ext:nrrd",
		MatchPath:    true,
		Limit:        200,
		DeadlineUnix: 123456789,
		RequestSeq:   42,
	}
	req := serviceRequestFromOptions(opts, false)
	if req.DeadlineUnix != opts.DeadlineUnix || req.RequestSeq != opts.RequestSeq {
		t.Fatalf("request deadline/seq = (%d, %d), want (%d, %d)", req.DeadlineUnix, req.RequestSeq, opts.DeadlineUnix, opts.RequestSeq)
	}
	got := requestToOptionsFromService(req)
	if got.DeadlineUnix != opts.DeadlineUnix || got.RequestSeq != opts.RequestSeq || got.Query != opts.Query || !got.MatchPath || got.Limit != opts.Limit {
		t.Fatalf("round trip options = %+v, want query/deadline/seq preserved", got)
	}
}

func syntheticServiceVolumeIndexForCacheTests() *serviceVolumeIndex {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: "."},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "one.txt"},
			{FRN: 3, ParentFRN: 1, Parent: 0, Name: "main.go"},
		},
	}
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.termCache = make(map[string][]int)
	vol.pathTermCache = make(map[string][]int)
	vol.extCache = make(map[string][]int)
	return vol
}
