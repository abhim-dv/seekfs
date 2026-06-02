package main

import (
	"strings"
	"testing"
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
	got, err := serviceVolumesForQuery([]*serviceVolumeIndex{c, f}, queryOptions{Under: `F:\git\seekfs`})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != f {
		t.Fatalf("volumes = %+v, want only ready F: volume", got)
	}
}

func TestServiceVolumesForQueryErrorsForMatchingStaleVolume(t *testing.T) {
	c := &serviceVolumeIndex{index: &Index{Volume: "C:"}, state: "stale", staleReason: "old checkpoint"}
	if _, err := serviceVolumesForQuery([]*serviceVolumeIndex{c}, queryOptions{Under: `C:\Data`}); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("error = %v, want stale index error", err)
	}
}

func TestLooksLikeSearchWithoutSubcommand(t *testing.T) {
	if !looksLikeSearchWithoutSubcommand([]string{"ScenePropertyPanel.cpp", "--under", `F:\git\DVCode`}) {
		t.Fatal("expected omitted search command to be recognized")
	}
	if looksLikeSearchWithoutSubcommand([]string{"bogus"}) {
		t.Fatal("single unknown command should not be treated as omitted search")
	}
}

func TestNormalizeCommandlessSearchArgsMovesFlagsBeforeQuery(t *testing.T) {
	got := normalizeCommandlessSearchArgs([]string{"ScenePropertyPanel.cpp", "--under", `F:\git\DVCode`, "-path"})
	want := []string{"--under", `F:\git\DVCode`, "-path", "ScenePropertyPanel.cpp"}
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
