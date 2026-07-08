package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

type deterministicQueryCase struct {
	Name         string
	Query        string
	Limit        int
	MatchPath    bool
	Under        string
	WantNonEmpty bool
	Family       string
}

func TestDeterministicV2QueryMatrixAcrossEngineVariants(t *testing.T) {
	cases := deterministicQueryCases()
	normalVolumes := deterministicMultiVolumeCorpus(t, 42)
	oracle := func(tc deterministicQueryCase) []Entry {
		t.Helper()
		return deterministicFullScanAcrossVolumes(t, normalVolumes, tc)
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			want := oracle(tc)
			if tc.WantNonEmpty && len(want) == 0 {
				t.Fatalf("oracle returned no results for non-empty case %q", tc.Query)
			}
			if strings.Contains(tc.Query, "path:C:") || strings.HasPrefix(strings.ToLower(tc.Under), `c:\`) {
				assertAllPathsOnVolume(t, want, "C:")
			}
			if strings.Contains(tc.Query, "path:F:") || strings.HasPrefix(strings.ToLower(tc.Under), `f:\`) {
				assertAllPathsOnVolume(t, want, "F:")
			}

			t.Run("normal-service", func(t *testing.T) {
				trace := &searchTrace{}
				got := deterministicServiceSearch(t, normalVolumes, tc, trace, false)
				assertSameOrderedPaths(t, got, want)
				assertPlannerBudget(t, tc, *trace, deterministicRecordCount(normalVolumes))
			})

			t.Run("normal-count", func(t *testing.T) {
				got := deterministicServiceSearch(t, normalVolumes, tc, &searchTrace{}, true)
				wantCount := deterministicFullScanCountAcrossVolumes(t, normalVolumes, tc)
				if len(got) != wantCount {
					t.Fatalf("count-only result count = %d, want %d", len(got), wantCount)
				}
			})

			t.Run("lowmem-cold", func(t *testing.T) {
				t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
				volumes := deterministicMultiVolumeCorpus(t, 42)
				want := deterministicFullScanAcrossVolumes(t, volumes, tc)
				trace := &searchTrace{}
				got := deterministicServiceSearch(t, volumes, tc, trace, false)
				assertSameOrderedPaths(t, got, want)
				assertPlannerBudget(t, tc, *trace, deterministicRecordCount(volumes))
			})

			t.Run("lowmem-warm-trigram", func(t *testing.T) {
				t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
				volumes := deterministicMultiVolumeCorpus(t, 42)
				for _, vol := range volumes {
					vol.rebuildNameTrigramsLocked()
				}
				want := deterministicFullScanAcrossVolumes(t, volumes, tc)
				trace := &searchTrace{}
				got := deterministicServiceSearch(t, volumes, tc, trace, false)
				assertSameOrderedPaths(t, got, want)
				assertPlannerBudget(t, tc, *trace, deterministicRecordCount(volumes))
			})
		})
	}
}

func TestDeterministicV2LooseAndExplicitEquivalence(t *testing.T) {
	volumes := deterministicMultiVolumeCorpus(t, 42)
	for _, pair := range []struct {
		name     string
		loose    deterministicQueryCase
		explicit deterministicQueryCase
	}{
		{
			name:     "downloads-nrrd",
			loose:    deterministicQueryCase{Query: "Downloads nrrd", MatchPath: true, Limit: 20},
			explicit: deterministicQueryCase{Query: "path:Downloads .nrrd", Limit: 20},
		},
		{
			name:     "fixtureproj-trainingdata",
			loose:    deterministicQueryCase{Query: "fixtureproj trainingdata", MatchPath: true, Limit: 20},
			explicit: deterministicQueryCase{Query: `path:F:\fixtureproj trainingdata`, Limit: 20},
		},
	} {
		t.Run(pair.name, func(t *testing.T) {
			loose := deterministicFullScanAcrossVolumes(t, volumes, pair.loose)
			explicit := deterministicFullScanAcrossVolumes(t, volumes, pair.explicit)
			assertSameOrderedPaths(t, loose, explicit)
		})
	}
}

func deterministicCorpus(t *testing.T, seed int) *Index {
	t.Helper()
	return deterministicVolumeCorpus(t, "C:", seed)
}

func deterministicMultiVolumeCorpus(t *testing.T, seed int) []*serviceVolumeIndex {
	t.Helper()
	return []*serviceVolumeIndex{
		newServiceVolumeIndex("deterministic-c.gsi", deterministicVolumeCorpus(t, "C:", seed)),
		newServiceVolumeIndex("deterministic-f.gsi", deterministicVolumeCorpus(t, "F:", seed+1)),
	}
}

func deterministicQueryCases() []deterministicQueryCase {
	return []deterministicQueryCase{
		{Name: "bare-md", Query: "md", Limit: 20, WantNonEmpty: true, Family: "bare-extension"},
		{Name: "bare-nrrd", Query: "nrrd", Limit: 20, WantNonEmpty: true, Family: "bare-extension"},
		{Name: "bare-pdf", Query: "pdf", Limit: 20, WantNonEmpty: true, Family: "bare-extension"},
		{Name: "dotted-md", Query: ".md", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "dotted-extension"},
		{Name: "dotted-nrrd", Query: ".nrrd", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "dotted-extension"},
		{Name: "dotted-pdf", Query: ".pdf", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "dotted-extension"},
		{Name: "downloads-md", Query: "Downloads md", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "loose-path"},
		{Name: "downloads-nrrd", Query: "Downloads nrrd", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "loose-path"},
		{Name: "downloads-dotted-nrrd", Query: "Downloads .nrrd", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "loose-path"},
		{Name: "fixtureproj-trainingdata", Query: "fixtureproj trainingdata", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "loose-path"},
		{Name: "path-downloads-md", Query: "path:Downloads md", Limit: 20, WantNonEmpty: true, Family: "explicit-path"},
		{Name: "path-downloads-dotted-nrrd", Query: "path:Downloads .nrrd", Limit: 20, WantNonEmpty: true, Family: "explicit-path"},
		{Name: "path-fixtureproj-trainingdata", Query: `path:F:\fixtureproj trainingdata`, Limit: 20, WantNonEmpty: true, Family: "explicit-path"},
		{Name: "path-f-fixtureproj-trainingdata", Query: "path:F: fixtureproj trainingdata", Limit: 20, WantNonEmpty: true, Family: "drive-scoped"},
		{Name: "path-c-trainingdata-dataset-nrrd", Query: "path:C: trainingdata Dataset .nrrd", Limit: 20, WantNonEmpty: true, Family: "drive-scoped"},
		{Name: "path-f-trainingdata-dataset-nrrd", Query: "path:F: trainingdata Dataset .nrrd", Limit: 20, WantNonEmpty: true, Family: "drive-scoped"},
		{Name: "trainingdata-dataset-nrrd", Query: "trainingdata Dataset nrrd", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "reordered"},
		{Name: "dataset-trainingdata-nrrd", Query: "Dataset trainingdata nrrd", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "reordered"},
		{Name: "nrrd-dataset-trainingdata", Query: "nrrd Dataset trainingdata", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "reordered"},
		{Name: "ext-nrrd-path-downloads", Query: "ext:nrrd path:Downloads", Limit: 20, WantNonEmpty: true, Family: "mixed-filter"},
		{Name: "dir-downloads-nrrd", Query: "dir:Downloads nrrd", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "mixed-filter"},
		{Name: "downloads-md-not-draft", Query: "Downloads md !draft", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "negation"},
		{Name: "trainingdata-nrrd-not-backup", Query: "trainingdata nrrd !backup", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "negation"},
		{Name: "downloads-nohit-selective", Query: "Downloads zzzz-nohit-md", MatchPath: true, Limit: 20, Family: "no-hit-selective"},
		{Name: "nohit-broad-grams", Query: "path:node_modules commonishzz", Limit: 20, Family: "no-hit-broad"},
		{Name: "path-node-modules-md", Query: "path:node_modules md", Limit: 20, WantNonEmpty: true, Family: "broad-common"},
		{Name: "path-appdata-json", Query: "path:AppData json", Limit: 20, WantNonEmpty: true, Family: "broad-common"},
		{Name: "under-downloads-nrrd", Query: "nrrd", MatchPath: true, Under: `C:\Users\exampleuser\Downloads`, Limit: 20, WantNonEmpty: true, Family: "under"},
	}
}

func deterministicVolumeCorpus(t *testing.T, volume string, seed int) *Index {
	t.Helper()
	idx := &Index{
		Source:  "usn",
		Volume:  volume,
		Compact: true,
		Records: make([]CompactRecord, 0, 7000),
	}
	nextFRN := uint64(seed * 100_000)
	if nextFRN == 0 {
		nextFRN = 100_000
	}
	add := func(parent int32, parentFRN uint64, name string, mode uint32) int32 {
		nextFRN++
		rec := CompactRecord{
			FRN:       nextFRN,
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
			Mode:      mode,
			Size:      int64(100 + len(name)),
			ModUnix:   time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC).Add(time.Duration(len(idx.Records)) * time.Second).UnixNano(),
		}
		idx.Records = append(idx.Records, rec)
		return int32(len(idx.Records) - 1)
	}
	rootFRN := nextFRN + 1
	idx.Records = append(idx.Records, CompactRecord{FRN: rootFRN, ParentFRN: rootFRN, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)})
	nextFRN = rootFRN
	root := int32(0)
	users := add(root, rootFRN, "Users", uint32(os.ModeDir))
	user := add(users, idx.Records[users].FRN, "exampleuser", uint32(os.ModeDir))
	downloads := add(user, idx.Records[user].FRN, "Downloads", uint32(os.ModeDir))
	appData := add(user, idx.Records[user].FRN, "AppData", uint32(os.ModeDir))
	workspace := add(root, rootFRN, "workspace", uint32(os.ModeDir))
	trainingdata := add(workspace, idx.Records[workspace].FRN, "trainingdata", uint32(os.ModeDir))
	dataset := add(trainingdata, idx.Records[trainingdata].FRN, "Dataset", uint32(os.ModeDir))
	nodeModules := add(workspace, idx.Records[workspace].FRN, "node_modules", uint32(os.ModeDir))
	target := add(workspace, idx.Records[workspace].FRN, "target", uint32(os.ModeDir))
	gitDir := add(workspace, idx.Records[workspace].FRN, ".git", uint32(os.ModeDir))
	unicodeDir := add(workspace, idx.Records[workspace].FRN, "Ünicode", uint32(os.ModeDir))

	add(downloads, idx.Records[downloads].FRN, "product_summary.md", 0)
	add(downloads, idx.Records[downloads].FRN, "scenario_region_summary.md", 0)
	add(downloads, idx.Records[downloads].FRN, "labels-cleaned.nrrd", 0)
	add(downloads, idx.Records[downloads].FRN, "filtered-volume-cleaned.nrrd", 0)
	add(downloads, idx.Records[downloads].FRN, "fixturemetrics_surface_stats.csv", 0)
	add(downloads, idx.Records[downloads].FRN, "draft-notes.md", 0)
	add(appData, idx.Records[appData].FRN, "settings.json", 0)
	add(appData, idx.Records[appData].FRN, "profile-cache.json", 0)
	add(dataset, idx.Records[dataset].FRN, "training-volume.nrrd", 0)
	add(dataset, idx.Records[dataset].FRN, "validation-volume.nrrd", 0)
	add(dataset, idx.Records[dataset].FRN, "backup-volume.nrrd", 0)
	add(nodeModules, idx.Records[nodeModules].FRN, "readme.md", 0)
	add(nodeModules, idx.Records[nodeModules].FRN, "package.json", 0)
	add(target, idx.Records[target].FRN, "artifact.raw", 0)
	add(gitDir, idx.Records[gitDir].FRN, "config", 0)
	add(unicodeDir, idx.Records[unicodeDir].FRN, "Résumé.pdf", 0)

	if strings.EqualFold(volume, "F:") {
		fixtureproj := add(root, rootFRN, "fixtureproj-dev", uint32(os.ModeDir))
		projects := add(fixtureproj, idx.Records[fixtureproj].FRN, "projects", uint32(os.ModeDir))
		model := add(projects, idx.Records[projects].FRN, "model", uint32(os.ModeDir))
		acmeTrainingdata := add(model, idx.Records[model].FRN, "trainingdata", uint32(os.ModeDir))
		rawFiles := add(acmeTrainingdata, idx.Records[acmeTrainingdata].FRN, "raw files", uint32(os.ModeDir))
		for i := 0; i < 120; i++ {
			add(rawFiles, idx.Records[rawFiles].FRN, fmt.Sprintf("volume-%03d.nrrd", i), 0)
		}
	} else {
		add(root, rootFRN, "fixtureproj-old-empty", uint32(os.ModeDir))
	}

	for i := 0; i < 900; i++ {
		add(downloads, idx.Records[downloads].FRN, fmt.Sprintf("download-noise-%04d.bin", i), 0)
	}
	for i := 0; i < 900; i++ {
		add(appData, idx.Records[appData].FRN, fmt.Sprintf("cache-entry-%04d.json", i), 0)
	}
	for i := 0; i < 900; i++ {
		add(nodeModules, idx.Records[nodeModules].FRN, fmt.Sprintf("package-readme-%04d.md", i), 0)
	}
	for i := 0; i < 900; i++ {
		add(target, idx.Records[target].FRN, fmt.Sprintf("build-output-%04d.raw", i), 0)
	}
	for i := 0; i < 600; i++ {
		parent := workspace
		parentFRN := idx.Records[workspace].FRN
		ext := "txt"
		switch i % 7 {
		case 0:
			ext = "md"
		case 1:
			ext = "nrrd"
		case 2:
			ext = "raw"
		case 3:
			ext = "pdf"
		case 4:
			ext = "json"
		case 5:
			ext = "go"
		case 6:
			ext = "py"
		}
		add(parent, parentFRN, fmt.Sprintf("common-file-%04d.%s", i, ext), 0)
	}
	buildOrders(idx)
	return idx
}

func runDeterministicQueryAcrossVariants(t *testing.T, volumes []*serviceVolumeIndex, tc deterministicQueryCase) {
	t.Helper()
	want := deterministicFullScanAcrossVolumes(t, volumes, tc)
	got := deterministicServiceSearch(t, volumes, tc, &searchTrace{}, false)
	assertSameOrderedPaths(t, got, want)
}

func deterministicFullScanAcrossVolumes(t *testing.T, volumes []*serviceVolumeIndex, tc deterministicQueryCase) []Entry {
	t.Helper()
	opts := deterministicOptions(tc, nil)
	selected, err := serviceVolumesForQuery(volumes, opts)
	if err != nil {
		t.Fatal(err)
	}
	selected = prioritizeServiceVolumesForPathTerms(selected, opts)
	limit := normalizedLimit(opts.Limit, false)
	out := make([]Entry, 0, limit)
	for _, vol := range selected {
		childOpts := opts
		childOpts.Limit = limit - len(out)
		if childOpts.Limit <= 0 {
			break
		}
		got, err := searchCompactWithCache(vol.index, childOpts, false, make(map[int]string), nil)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, got...)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func deterministicFullScanCountAcrossVolumes(t *testing.T, volumes []*serviceVolumeIndex, tc deterministicQueryCase) int {
	t.Helper()
	opts := deterministicOptions(tc, nil)
	selected, err := serviceVolumesForQuery(volumes, opts)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, vol := range selected {
		got, err := searchCompactWithCache(vol.index, opts, true, make(map[int]string), nil)
		if err != nil {
			t.Fatal(err)
		}
		total += len(got)
	}
	return total
}

func deterministicServiceSearch(t *testing.T, volumes []*serviceVolumeIndex, tc deterministicQueryCase, trace *searchTrace, countOnly bool) []Entry {
	t.Helper()
	opts := deterministicOptions(tc, trace)
	got, err := searchServiceVolumes(volumes, opts, countOnly)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func deterministicOptions(tc deterministicQueryCase, trace *searchTrace) queryOptions {
	return queryOptions{
		Query:     tc.Query,
		MatchPath: tc.MatchPath || queryLooksLoosePathScoped(tc.Query),
		Limit:     tc.Limit,
		Under:     tc.Under,
		Trace:     trace,
	}
}

func assertSameOrderedPaths(t *testing.T, got, want []Entry) {
	t.Helper()
	if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !sameOrderedStrings(gotPaths, wantPaths) {
		t.Fatalf("paths = %v, want %v", gotPaths, wantPaths)
	}
}

func assertPlannerBudget(t *testing.T, tc deterministicQueryCase, trace searchTrace, recordCount int) {
	t.Helper()
	if trace.Source == "" {
		t.Fatalf("query %q did not report planner source", tc.Query)
	}
	if trace.Source == "filesystem-under-fallback" {
		t.Fatalf("query %q used filesystem fallback", tc.Query)
	}
	budget := deterministicCandidateBudget(tc, tc.Limit, trace.Candidates, recordCount)
	if trace.Candidates > budget {
		t.Fatalf("query %q source=%s candidates=%d budget=%d", tc.Query, trace.Source, trace.Candidates, budget)
	}
	if trace.Source == "compact-name-order-scan" && trace.Candidates > maxInt(512, tc.Limit*4) {
		t.Fatalf("query %q used broad compact-name-order-scan candidates=%d", tc.Query, trace.Candidates)
	}
}

func deterministicCandidateBudget(tc deterministicQueryCase, limit, resultCount, recordCount int) int {
	budget := maxInt(4*limit, 512)
	switch tc.Family {
	case "broad-common", "cold-trigram", "mixed-filter", "negation", "under":
		return min(recordCount, maxInt(budget, 50_000))
	default:
		return min(recordCount, budget)
	}
}

func deterministicRecordCount(volumes []*serviceVolumeIndex) int {
	total := 0
	for _, vol := range volumes {
		if vol != nil && vol.index != nil {
			total += vol.index.compactRecordCount()
		}
	}
	return total
}

func assertAllPathsOnVolume(t *testing.T, entries []Entry, volume string) {
	t.Helper()
	for _, entry := range entries {
		if !strings.HasPrefix(strings.ToLower(entry.Path), strings.ToLower(volume+`\`)) {
			t.Fatalf("path %q is not on volume %s", entry.Path, volume)
		}
	}
}
