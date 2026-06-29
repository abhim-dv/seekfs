package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPlannedCandidatesMatchFullSearchForStructuralFilters(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []queryOptions{
		{Query: "src ext:go", MatchPath: true, Limit: 20},
		{Query: "dir:src ext:go", MatchPath: true, Limit: 20},
		{Query: "glob:*test*.go", MatchPath: true, Limit: 20},
		{Query: `regex:Assets.*\.dat$`, MatchPath: true, Under: `C:\fixture\workspace`, Limit: 20},
		{Query: "type:file glob:*.go", MatchPath: true, Under: `C:\fixture\workspace`, Limit: 20},
	}
	for _, opts := range cases {
		t.Run(opts.Query, func(t *testing.T) {
			pq, err := parseQuery(opts)
			if err != nil {
				t.Fatal(err)
			}
			pq.Limit = normalizedLimit(opts.Limit, false)
			got, ok := vol.plannedCandidates(pq)
			if !ok {
				t.Fatal("plannedCandidates declined query")
			}
			full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("full search: %v", err)
			}
			fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, func(parsedQuery) ([]int, bool) {
				return got, true
			})
			if err != nil {
				t.Fatalf("planned search: %v", err)
			}
			if !sameStringSet(namesOf(fast), namesOf(full)) {
				t.Fatalf("planned names = %v, full names = %v", namesOf(fast), namesOf(full))
			}
		})
	}
}

func TestPlannedCountMatchesFullSearchCount(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []queryOptions{
		{Query: "type:file ext:go", MatchPath: true},
		{Query: "dir:src ext:go", MatchPath: true},
		{Query: "glob:*test*.go", MatchPath: true},
		{Query: "ext:dat", MatchPath: true, Under: `C:\fixture\workspace\Assets`},
		{Query: `regex:Assets.*\.(dat|txt)$`, MatchPath: true, Under: `C:\fixture\workspace`},
	}
	for _, opts := range cases {
		t.Run(opts.Query, func(t *testing.T) {
			pq, err := parseQuery(opts)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := vol.plannedCount(pq)
			if !ok {
				t.Fatal("plannedCount declined query")
			}
			full, err := searchCompactWithCache(idx, opts, true, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("full count search: %v", err)
			}
			if got != len(full) {
				t.Fatalf("planned count = %d, full count = %d", got, len(full))
			}
		})
	}
}

func TestCandidatePlanUsesCheapestUnderOrPostingSource(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	pq, err := parseQuery(queryOptions{Query: "type:file ext:go", MatchPath: true, Under: `C:\fixture\workspace`})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		t.Fatal("buildCandidatePlan declined query")
	}
	if len(plan.sources) == 0 || plan.sources[0].name != "ext:go" {
		t.Fatalf("plan sources = %+v, want extension posting before subtree materialization", plan.sources)
	}

	pq, err = parseQuery(queryOptions{Query: "type:file", MatchPath: true, Under: `C:\fixture\workspace\src`})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok = vol.buildCandidatePlan(pq)
	if !ok {
		t.Fatal("buildCandidatePlan declined under-only query")
	}
	if len(plan.sources) == 0 || plan.sources[0].name != "under" {
		t.Fatalf("plan sources = %+v, want under source for unposted subtree query", plan.sources)
	}
}

func TestCandidatePlanUsesNameTermBeforeUnderSubtree(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	pq, err := parseQuery(queryOptions{Query: "main.go", Under: `C:\fixture\workspace`, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		t.Fatal("buildCandidatePlan declined query")
	}
	if len(plan.sources) == 0 {
		t.Fatal("buildCandidatePlan returned no sources")
	}
	for _, source := range plan.sources {
		if source.name == "under" {
			t.Fatalf("plan sources = %+v, should filter selective filename candidates by --under instead of materializing subtree", plan.sources)
		}
	}
	if plan.sources[0].name != "term:main.go" {
		t.Fatalf("first source = %q, want term:main.go", plan.sources[0].name)
	}
}

func TestCandidatePlanUsesExactKnownFileUnderRepo(t *testing.T) {
	idx := commonSearchFixture()
	idx.Records = append(idx.Records, CompactRecord{
		FRN:       17,
		ParentFRN: 3,
		Parent:    2,
		Name:      ".seekfs-agent-log.jsonl",
	})
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	pq, err := parseQuery(queryOptions{Query: ".seekfs-agent-log.jsonl", Under: `C:\fixture\workspace`, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		t.Fatal("buildCandidatePlan declined query")
	}
	got := plan.execute()
	if len(got) != 1 || got[0] != 16 {
		t.Fatalf("candidate ids = %v, want exact agent log file only", got)
	}
}

func TestPathDottedExtensionTermUsesExtensionPosting(t *testing.T) {
	idx := commonSearchFixture()
	idx.Records = append(idx.Records,
		CompactRecord{FRN: 17, ParentFRN: 3, Parent: 2, Name: "Reports", Mode: uint32(os.ModeDir)},
		CompactRecord{FRN: 18, ParentFRN: 17, Parent: 16, Name: "annual-report.docx"},
		CompactRecord{FRN: 19, ParentFRN: 17, Parent: 16, Name: "notes.txt"},
	)
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	opts := queryOptions{Query: "Reports .docx", MatchPath: true, Limit: 20}
	pq, err := parseQuery(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(pq.Exts) != 1 || pq.Exts[0] != "docx" || len(pq.Terms) != 1 || pq.Terms[0] != "reports" {
		t.Fatalf("parsed query terms=%v exts=%v, want reports + docx extension", pq.Terms, pq.Exts)
	}
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		t.Fatal("buildCandidatePlan declined query")
	}
	if len(plan.sources) == 0 || plan.sources[0].name != "ext:docx" {
		t.Fatalf("plan sources = %+v, want ext:docx source", plan.sources)
	}
	got, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(got); len(names) != 1 || names[0] != "annual-report.docx" {
		t.Fatalf("matches = %v, want annual-report.docx", names)
	}
}

func TestPathOnlyDottedExtensionUsesPathPostingCandidate(t *testing.T) {
	idx := commonSearchFixture()
	idx.Records = append(idx.Records,
		CompactRecord{FRN: 17, ParentFRN: 3, Parent: 2, Name: "Downloads", Mode: uint32(os.ModeDir)},
		CompactRecord{FRN: 18, ParentFRN: 17, Parent: 16, Name: "scan.nrrd"},
		CompactRecord{FRN: 19, ParentFRN: 17, Parent: 16, Name: "notes.txt"},
		CompactRecord{FRN: 20, ParentFRN: 3, Parent: 2, Name: "data.nrrd", Mode: uint32(os.ModeDir)},
		CompactRecord{FRN: 21, ParentFRN: 20, Parent: 19, Name: "metadata.json"},
	)
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)

	pathPQ, err := parseQuery(queryOptions{Query: "path:.nrrd", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	extPQ, err := parseQuery(queryOptions{Query: "ext:.nrrd", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	pathPlan, ok := vol.buildCandidatePlan(pathPQ)
	if !ok || len(pathPlan.sources) == 0 {
		t.Fatalf("path:.nrrd plan = %+v ok=%v, want path-term candidate source", pathPlan.sources, ok)
	}
	if pathPlan.sources[0].name != "term:.nrrd" {
		t.Fatalf("path:.nrrd source = %q, want path term source", pathPlan.sources[0].name)
	}
	extPlan, ok := vol.buildCandidatePlan(extPQ)
	if !ok || len(extPlan.sources) == 0 {
		t.Fatalf("ext:.nrrd plan = %+v ok=%v, want extension candidate source", extPlan.sources, ok)
	}
	if got, want := extPlan.sources[0].name, "ext:nrrd"; got != want {
		t.Fatalf("ext:.nrrd source = %q, want %q", got, want)
	}
	if got, want := len(pathPlan.execute()), len(extPlan.execute()); got <= want {
		t.Fatalf("candidate counts path:.nrrd=%d ext:.nrrd=%d, want path substring candidate set to include extension matches plus path-only matches", got, want)
	}
	pathMatches, err := searchCompactWithCache(idx, queryOptions{Query: "path:.nrrd", Limit: 20}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(pathMatches); !sameStringSet(names, []string{"data.nrrd", "metadata.json", "scan.nrrd"}) {
		t.Fatalf("path:.nrrd names = %v, want full-path substring matches", names)
	}
	extMatches, err := searchCompactWithCache(idx, queryOptions{Query: "ext:.nrrd", Limit: 20}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(extMatches); !sameStringSet(names, []string{"data.nrrd", "scan.nrrd"}) {
		t.Fatalf("ext:.nrrd names = %v, want extension matches only", names)
	}
}

func TestDottedPathSubstringAndExtensionSemanticsMatrix(t *testing.T) {
	idx := dottedPathBenchmarkIndex(200)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []struct {
		query     string
		wantHas   []string
		wantLacks []string
	}{
		{
			query:   "path:.nrrd",
			wantHas: []string{"scan-000037.nrrd", "backup-000053.nrrd.bak", "dataset-000000.nrrd", "metadata-000000.json"},
		},
		{
			query:     "ext:.nrrd",
			wantHas:   []string{"scan-000037.nrrd", "dataset-000000.nrrd"},
			wantLacks: []string{"backup-000053.nrrd.bak", "metadata-000000.json"},
		},
		{
			query:     "type:file ext:.nrrd",
			wantHas:   []string{"scan-000037.nrrd"},
			wantLacks: []string{"dataset-000000.nrrd", "backup-000053.nrrd.bak", "metadata-000000.json"},
		},
		{
			query:     "path:nrrd",
			wantHas:   []string{"scan-000037.nrrd", "nrrd-cache", "cache-000097.json"},
			wantLacks: []string{"plain-000001.txt"},
		},
		{
			query:     "path:.nrrd ext:json",
			wantHas:   []string{"metadata-000000.json"},
			wantLacks: []string{"scan-000037.nrrd", "backup-000053.nrrd.bak", "cache-000097.json"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			got, err := searchCompactWithCache(idx, queryOptions{Query: tc.query, Limit: 500}, false, vol.pathCache, vol.nameTermCandidates)
			if err != nil {
				t.Fatal(err)
			}
			names := namesOf(got)
			for _, want := range tc.wantHas {
				if !containsString(names, want) {
					t.Fatalf("%q names missing %q: %v", tc.query, want, names)
				}
			}
			for _, unwanted := range tc.wantLacks {
				if containsString(names, unwanted) {
					t.Fatalf("%q names unexpectedly included %q: %v", tc.query, unwanted, names)
				}
			}
		})
	}
}

func TestLimitedBroadSubstringCandidatesPreserveFullSearchFirstPage(t *testing.T) {
	idx := broadSubstringOrderingFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []queryOptions{
		{Query: "path:nrrd", Limit: 5},
		{Query: "nrrd", Limit: 5},
		{Query: "path:.nrrd", Limit: 5},
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
			if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
				t.Fatalf("candidate first page = %v, full first page = %v", got, want)
			}
		})
	}
}

func TestBroadPathSearchAndCountParityMatrix(t *testing.T) {
	idx := dottedPathBenchmarkIndex(600)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	queries := []queryOptions{
		{Query: "path:.nrrd", Limit: 25},
		{Query: "path:.nrrd", Limit: 200},
		{Query: "path:nrrd", Limit: 25},
		{Query: "path:.nrrd ext:json", Limit: 25},
		{Query: "path:.nrrd type:file", Limit: 25},
		{Query: "path:nrrd !backup", Limit: 25},
		{Query: "path:nrrd type:file", Limit: 25},
		{Query: "path:nrrd ext:txt", Limit: 25},
		{Query: "path:nrrd size:>0", Limit: 25},
		{Query: "path:nrrd dm:2026-05-01", Limit: 25},
		{Query: "path:nrrd glob:*.json", Limit: 25},
		{Query: "path:cache ext:json", Limit: 25},
		{Query: "path:workspace .nrrd", Limit: 25},
		{Query: "path:workspace ext:nrrd|json", Limit: 25},
		{Query: "path:dataset ext:json", Limit: 25},
		{Query: "path:.nrrd ext:nrrd|json", Limit: 50},
		{Query: "path:.nrrd !metadata", Limit: 50},
		{Query: "path:.nrrd", Under: `C:\workspace\dataset-000000.nrrd`, Limit: 25},
		{Query: "ext:json", Under: `C:\workspace\dataset-000000.nrrd`, Limit: 25},
		{Query: "path:.nrrd ext:json", Under: `C:\workspace`, Limit: 25},
	}
	for _, opts := range queries {
		t.Run(opts.Query+"/search", func(t *testing.T) {
			full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("full search: %v", err)
			}
			fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
			if err != nil {
				t.Fatalf("candidate search: %v", err)
			}
			if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
				t.Fatalf("candidate paths = %v, full paths = %v", got, want)
			}
		})
		t.Run(opts.Query+"/count", func(t *testing.T) {
			countOpts := opts
			countOpts.Limit = 0
			full, err := searchCompactWithCache(idx, countOpts, true, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("full count search: %v", err)
			}
			count, ok := vol.plannedCount(mustParseQuery(t, countOpts))
			if !ok {
				t.Skip("plannedCount declined query")
			}
			if count != len(full) {
				t.Fatalf("planned count = %d, full count = %d", count, len(full))
			}
		})
	}
}

func TestGeneratedBroadPathQueryParity(t *testing.T) {
	idx := dottedPathBenchmarkIndex(800)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	pathTerms := []string{"path:.nrrd", "path:nrrd", "path:cache", "path:dataset", "path:workspace"}
	filters := []string{"", "ext:nrrd", "ext:json", "type:file", "glob:*.json", "!backup", "!metadata", "ext:nrrd|json"}
	limits := []int{1, 5, 25, 100}
	unders := []string{"", `C:\workspace`, `C:\workspace\nrrd-cache`, `C:\workspace\dataset-000000.nrrd`}
	for _, term := range pathTerms {
		for _, filter := range filters {
			for _, limit := range limits {
				for _, under := range unders {
					query := strings.TrimSpace(term + " " + filter)
					opts := queryOptions{Query: query, Limit: limit, Under: under}
					t.Run(fmt.Sprintf("%s/limit:%d/under:%s", query, limit, under), func(t *testing.T) {
						full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
						if err != nil {
							t.Fatalf("full search: %v", err)
						}
						fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
						if err != nil {
							t.Fatalf("candidate search: %v", err)
						}
						if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
							t.Fatalf("candidate paths = %v, full paths = %v", got, want)
						}
					})
				}
			}
		}
	}
}

func TestGeneratedImplicitPathSeparatorQueryParity(t *testing.T) {
	idx := dottedPathBenchmarkIndex(800)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []struct {
		implicit string
		explicit string
	}{
		{
			implicit: `workspace\dataset-000000.nrrd\metadata-000000.json`,
			explicit: `path:workspace path:dataset-000000.nrrd metadata-000000.json`,
		},
		{
			implicit: `workspace/nrrd-cache/cache-000097.json`,
			explicit: `path:workspace path:nrrd-cache cache-000097.json`,
		},
		{
			implicit: `nrrd-cache\cache-000097.json`,
			explicit: `path:nrrd-cache cache-000097.json`,
		},
	}
	filters := []string{"", "ext:json", "type:file", "!backup", "size:>0", "dm:2026-05-01"}
	limits := []int{1, 5, 25}
	for _, tc := range cases {
		for _, filter := range filters {
			for _, limit := range limits {
				implicit := strings.TrimSpace(tc.implicit + " " + filter)
				explicit := strings.TrimSpace(tc.explicit + " " + filter)
				t.Run(fmt.Sprintf("%s/filter:%s/limit:%d", tc.implicit, filter, limit), func(t *testing.T) {
					implicitOpts := queryOptions{Query: implicit, Limit: limit}
					explicitOpts := queryOptions{Query: explicit, Limit: limit}
					pq := mustParseQuery(t, implicitOpts)
					if !pq.MatchPath {
						t.Fatalf("implicit path query %q did not infer MatchPath", implicit)
					}
					implicitFast, err := searchCompactWithCache(idx, implicitOpts, false, vol.pathCache, vol.nameTermCandidates)
					if err != nil {
						t.Fatalf("implicit search: %v", err)
					}
					explicitFast, err := searchCompactWithCache(idx, explicitOpts, false, vol.pathCache, vol.nameTermCandidates)
					if err != nil {
						t.Fatalf("explicit search: %v", err)
					}
					if got, want := pathsOf(implicitFast), pathsOf(explicitFast); !sameOrderedStrings(got, want) {
						t.Fatalf("implicit paths = %v, explicit paths = %v", got, want)
					}
					full, err := searchCompactWithCache(idx, implicitOpts, false, make(map[int]string), nil)
					if err != nil {
						t.Fatalf("full implicit search: %v", err)
					}
					if got, want := pathsOf(implicitFast), pathsOf(full); !sameOrderedStrings(got, want) {
						t.Fatalf("candidate paths = %v, full paths = %v", got, want)
					}
				})
			}
		}
	}
}

func TestDriveScopedBroadExtensionSearchRoutesToRequestedVolume(t *testing.T) {
	cIdx := dottedPathBenchmarkIndex(1000)
	cIdx.Volume = "C:"
	fIdx := dottedPathBenchmarkIndex(1000)
	fIdx.Volume = "F:"
	cVol := newServiceVolumeIndex("c.gsi", cIdx)
	fVol := newServiceVolumeIndex("f.gsi", fIdx)
	for _, ext := range []string{"nrrd", "raw", "pdf"} {
		query := "path:F: ." + ext
		t.Run(query, func(t *testing.T) {
			volumes, err := serviceVolumesForQuery([]*serviceVolumeIndex{cVol, fVol}, queryOptions{Query: query, MatchPath: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(volumes) != 1 || volumes[0] != fVol {
				t.Fatalf("volumes = %+v, want only F:", volumes)
			}
			got, err := searchServiceVolumes([]*serviceVolumeIndex{cVol, fVol}, queryOptions{Query: query, MatchPath: true, Limit: 25}, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range got {
				if !strings.HasPrefix(strings.ToUpper(entry.Path), `F:\`) {
					t.Fatalf("result %q is not on F:", entry.Path)
				}
			}
		})
	}
}

func TestGeneratedBroadPathQueryParityAcrossResidentVariants(t *testing.T) {
	idx := dottedPathBenchmarkIndex(800)
	queries := []queryOptions{
		{Query: "path:.nrrd", Limit: 25},
		{Query: "path:.nrrd ext:json", Limit: 25},
		{Query: "path:nrrd glob:*.json", Limit: 25},
		{Query: "path:dataset ext:json", Limit: 25},
		{Query: "path:.nrrd", Under: `C:\workspace\dataset-000000.nrrd`, Limit: 25},
		{Query: "path:nrrd !backup", Limit: 25},
		{Query: "path:workspace ext:nrrd|json", Limit: 25},
		{Query: `workspace\dataset-000000.nrrd\metadata-000000.json`, Limit: 25},
		{Query: `workspace/nrrd-cache/cache-000097.json`, Limit: 25},
	}
	variants := []struct {
		name   string
		mutate func(*serviceVolumeIndex)
	}{
		{name: "normal"},
		{name: "no-child-ranges", mutate: func(vol *serviceVolumeIndex) {
			vol.childOffsets = nil
			vol.childIDs = nil
			vol.rootIDs = nil
			vol.subtreeOrder = nil
			vol.subtreeStart = nil
			vol.subtreeEnd = nil
		}},
		{name: "no-children-map", mutate: func(vol *serviceVolumeIndex) {
			vol.children = nil
			vol.childOffsets = nil
			vol.childIDs = nil
			vol.rootIDs = nil
			vol.subtreeOrder = nil
			vol.subtreeStart = nil
			vol.subtreeEnd = nil
		}},
		{name: "no-query-index", mutate: func(vol *serviceVolumeIndex) {
			vol.queryIndex = nil
		}},
		{name: "no-exact-names", mutate: func(vol *serviceVolumeIndex) {
			vol.exactNames = nil
		}},
	}
	for _, variant := range variants {
		vol := newServiceVolumeIndex("fixture.gsi", idx)
		if variant.mutate != nil {
			variant.mutate(vol)
		}
		for _, opts := range queries {
			t.Run(variant.name+"/"+opts.Query, func(t *testing.T) {
				full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
				if err != nil {
					t.Fatalf("full search: %v", err)
				}
				fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
				if err != nil {
					t.Fatalf("candidate search: %v", err)
				}
				if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
					t.Fatalf("candidate paths = %v, full paths = %v", got, want)
				}
			})
		}
	}
}

func TestLiveDottedExtensionQueryMatchesExtensionFilter(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	dotted, err := searchCompactWithCache(idx, queryOptions{Query: ".go", MatchPath: true, Limit: 20}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatalf("dotted extension search: %v", err)
	}
	filtered, err := searchCompactWithCache(idx, queryOptions{Query: "ext:go", MatchPath: true, Limit: 20}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatalf("extension filter search: %v", err)
	}
	if !sameStringSet(namesOf(dotted), namesOf(filtered)) {
		t.Fatalf(".go names = %v, ext:go names = %v", namesOf(dotted), namesOf(filtered))
	}
}

func TestLimitedSingleTermCandidatesMatchFullFirstPage(t *testing.T) {
	idx := syntheticCompactIndex(5000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	opts := queryOptions{Query: "source", MatchPath: true, Limit: 25}
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatalf("full search: %v", err)
	}
	fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatalf("candidate search: %v", err)
	}
	if got, want := namesOf(fast), namesOf(full); !sameStringSet(got, want) {
		t.Fatalf("fast first page = %v, full first page = %v", got, want)
	}
}

func TestPlannerUsesSelectiveExtensionBeforePathVerification(t *testing.T) {
	idx := &Index{Source: "usn", Volume: "C:", Compact: true}
	add := func(frn, parentFRN uint64, parent int32, name string, mode uint32) {
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       frn,
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
			Mode:      mode,
		})
	}
	add(1, 1, -1, ".", uint32(os.ModeDir))
	add(2, 1, 0, "Downloads", uint32(os.ModeDir))
	add(3, 2, 1, "camera-001.raw", 0)
	add(4, 2, 1, "camera-002.raw", 0)
	add(5, 1, 0, "Lab", uint32(os.ModeDir))
	for i := 0; i < 2000; i++ {
		add(uint64(i+10), 5, 4, fmt.Sprintf("sample-%04d.raw", i), 0)
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	pq, err := parseQuery(queryOptions{Query: "Downloads ext:raw", MatchPath: true, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		t.Fatal("buildCandidatePlan declined Downloads ext:raw")
	}
	sourceNames := make([]string, 0, len(plan.sources))
	for _, source := range plan.sources {
		sourceNames = append(sourceNames, source.name)
	}
	if !sameStringSet(sourceNames, []string{"ext:raw"}) {
		t.Fatalf("plan sources = %v, want only ext:raw with Downloads verified after candidate selection", sourceNames)
	}
	got, err := searchCompactWithCache(idx, queryOptions{Query: "Downloads ext:raw", MatchPath: true, Limit: 20}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(got); !sameStringSet(names, []string{"camera-001.raw", "camera-002.raw"}) {
		t.Fatalf("names = %v, want Downloads raw files only", names)
	}
}

func TestCandidatePlanSkipsSingleCharacterPathTermWhenSelectiveTermExists(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	pq, err := parseQuery(queryOptions{Query: "c main.go", MatchPath: true})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		t.Fatal("buildCandidatePlan declined query")
	}
	for _, source := range plan.sources {
		if source.name == "term:c" {
			t.Fatalf("plan sources = %+v, should not build broad single-character path term posting", plan.sources)
		}
	}
	got := plan.execute()
	if len(got) == 0 {
		t.Fatal("plan returned no candidates from the selective term")
	}
}

func TestPathPlanProbeTermsPreferSpecificFragments(t *testing.T) {
	got := pathPlanProbeTerms([]string{"f:", "repo", "tools", "fixtures", "reports", "specific_fixture_tool.py"})
	want := []string{"specific_fixture_tool.py", "fixtures", "reports", "tools", "repo"}
	if !sameStringSet(got, want) {
		t.Fatalf("probe terms = %v, want same terms as %v", got, want)
	}
	for i, term := range want {
		if got[i] != term {
			t.Fatalf("probe terms = %v, want ordered prefix %v", got, want)
		}
	}
}

func TestCandidatePlanDeclinesCaseSensitivePostings(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	pq, err := parseQuery(queryOptions{Query: "case: README", MatchPath: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := vol.plannedCandidates(pq); ok {
		t.Fatal("plannedCandidates accepted case-sensitive query")
	}
}

func TestRegexLiteralCandidatesDeclinesAlternationLiterals(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	pq, err := parseQuery(queryOptions{Query: `regex:Assets.*\.(dat|txt)$`, MatchPath: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := vol.regexLiteralCandidates(pq); ok {
		t.Fatal("regexLiteralCandidates accepted ambiguous alternation literals")
	}
}

func BenchmarkDottedPathSubstringVsExtension(b *testing.B) {
	idx := dottedPathBenchmarkIndex(100_000)
	vol := newServiceVolumeIndex("bench.gsi", idx)
	cases := []queryOptions{
		{Query: "ext:.nrrd", Limit: 50},
		{Query: "type:file ext:.nrrd", Limit: 50},
		{Query: "path:.nrrd", Limit: 50},
		{Query: "path:nrrd", Limit: 50},
		{Query: "path:.nrrd ext:json", Limit: 50},
	}
	for _, opts := range cases {
		b.Run(opts.Query, func(b *testing.B) {
			if _, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				matches, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
				if err != nil {
					b.Fatal(err)
				}
				if len(matches) == 0 {
					b.Fatalf("no matches for %q", opts.Query)
				}
			}
		})
	}
}

func BenchmarkDottedPathSubstringCount(b *testing.B) {
	idx := dottedPathBenchmarkIndex(100_000)
	vol := newServiceVolumeIndex("bench.gsi", idx)
	cases := []queryOptions{
		{Query: "path:.nrrd"},
		{Query: "path:nrrd"},
		{Query: "path:.nrrd ext:json"},
		{Query: "path:nrrd glob:*.json"},
	}
	for _, opts := range cases {
		b.Run(opts.Query, func(b *testing.B) {
			pq := mustParseQueryB(b, opts)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				count, ok := vol.plannedCount(pq)
				if !ok {
					b.Fatalf("plannedCount declined %q", opts.Query)
				}
				if count == 0 {
					b.Fatalf("count = 0 for %q", opts.Query)
				}
			}
		})
	}
}

func BenchmarkDottedPathSubstringUnder(b *testing.B) {
	idx := dottedPathBenchmarkIndex(100_000)
	vol := newServiceVolumeIndex("bench.gsi", idx)
	cases := []queryOptions{
		{Query: "path:.nrrd ext:json", Under: `C:\workspace`, Limit: 50},
		{Query: "path:nrrd glob:*.json", Under: `C:\workspace\nrrd-cache`, Limit: 50},
		{Query: "ext:json", Under: `C:\workspace\dataset-000000.nrrd`, Limit: 50},
	}
	for _, opts := range cases {
		b.Run(opts.Query+"/under:"+opts.Under, func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				matches, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
				if err != nil {
					b.Fatal(err)
				}
				if len(matches) == 0 {
					b.Fatalf("no matches for %+v", opts)
				}
			}
		})
	}
}

func BenchmarkDottedPathSubstringColdWarm(b *testing.B) {
	idx := dottedPathBenchmarkIndex(100_000)
	cases := []struct {
		name string
		cold bool
		opts queryOptions
	}{
		{name: "warm-path-dot", opts: queryOptions{Query: "path:.nrrd", Limit: 50}},
		{name: "cold-path-dot", cold: true, opts: queryOptions{Query: "path:.nrrd", Limit: 50}},
		{name: "warm-path-json", opts: queryOptions{Query: "path:.nrrd ext:json", Limit: 50}},
		{name: "cold-path-json", cold: true, opts: queryOptions{Query: "path:.nrrd ext:json", Limit: 50}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			vol := newServiceVolumeIndex("bench.gsi", idx)
			if !tc.cold {
				if _, err := searchCompactWithCache(idx, tc.opts, false, vol.pathCache, vol.nameTermCandidates); err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if tc.cold {
					vol = newServiceVolumeIndex("bench.gsi", idx)
				}
				matches, err := searchCompactWithCache(idx, tc.opts, false, vol.pathCache, vol.nameTermCandidates)
				if err != nil {
					b.Fatal(err)
				}
				if len(matches) == 0 {
					b.Fatalf("no matches for %q", tc.opts.Query)
				}
			}
		})
	}
}

func BenchmarkSearchServiceVolumesSynthetic(b *testing.B) {
	volumes := make([]*serviceVolumeIndex, 0, 4)
	for i, volume := range []string{"C:", "D:", "E:", "F:"} {
		idx := dottedPathBenchmarkIndex(25_000)
		idx.Volume = volume
		volumes = append(volumes, newServiceVolumeIndex(fmt.Sprintf("bench-%d.gsi", i), idx))
	}
	cases := []queryOptions{
		{Query: "path:nrrd", Limit: 20},
		{Query: "path:F: .nrrd", Limit: 20},
		{Query: "path:F: .raw", Limit: 20},
		{Query: "path:F: .pdf", Limit: 20},
		{Query: "path:C: .opencode", Limit: 20},
		{Query: "path:.nrrd ext:json", Limit: 20},
		{Query: "ext:nrrd", Limit: 20},
		{Query: "ext:raw", Limit: 20},
		{Query: "ext:pdf", Limit: 20},
	}
	for _, opts := range cases {
		b.Run(opts.Query, func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				matches, err := searchServiceVolumes(volumes, opts, false)
				if err != nil {
					b.Fatal(err)
				}
				if len(matches) == 0 {
					b.Fatalf("no matches for %q", opts.Query)
				}
			}
		})
	}
}

func BenchmarkOrderedLimitedSubstringScans(b *testing.B) {
	idx := orderedLimitedBenchmarkIndex(200_000)
	vol := newServiceVolumeIndex("bench.gsi", idx)
	cases := []queryOptions{
		{Query: "aaneedle", Limit: 20},
		{Query: "zzneedle", Limit: 20},
		{Query: "zzneedle", MatchPath: true, Limit: 20},
		{Query: "missingneedle", MatchPath: true, Limit: 20},
	}
	for _, opts := range cases {
		b.Run(fmt.Sprintf("%s/path:%v", opts.Query, opts.MatchPath), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				matches, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
				if err != nil {
					b.Fatal(err)
				}
				if opts.Query != "missingneedle" && len(matches) != 20 {
					b.Fatalf("matches = %d, want 20", len(matches))
				}
				if opts.Query == "missingneedle" && len(matches) != 0 {
					b.Fatalf("matches = %d, want 0", len(matches))
				}
			}
		})
	}
}

func dottedPathBenchmarkIndex(n int) *Index {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: make([]CompactRecord, 0, n+n/50+4),
	}
	add := func(frn, parentFRN uint64, parent int32, name string, mode uint32) int32 {
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       frn,
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
			Mode:      mode,
			Size:      1024,
			ModUnix:   time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixNano(),
		})
		return int32(len(idx.Records) - 1)
	}
	root := add(1, 1, -1, ".", uint32(os.ModeDir))
	workspace := add(2, 1, root, "workspace", uint32(os.ModeDir))
	cacheDir := add(3, 2, workspace, "nrrd-cache", uint32(os.ModeDir))
	add(4, 2, workspace, "ai.opencode.desktop", uint32(os.ModeDir))
	nextFRN := uint64(10)
	for i := 0; i < n; i++ {
		parent := workspace
		parentFRN := uint64(2)
		name := fmt.Sprintf("plain-%06d.txt", i)
		switch {
		case i%5000 == 0:
			dirFRN := nextFRN
			dir := add(dirFRN, 2, workspace, fmt.Sprintf("dataset-%06d.nrrd", i), uint32(os.ModeDir))
			nextFRN++
			add(nextFRN, dirFRN, dir, fmt.Sprintf("metadata-%06d.json", i), 0)
			nextFRN++
			continue
		case i%37 == 0:
			name = fmt.Sprintf("scan-%06d.nrrd", i)
		case i%53 == 0:
			name = fmt.Sprintf("backup-%06d.nrrd.bak", i)
		case i%89 == 0:
			name = fmt.Sprintf("capture-%06d.raw", i)
		case i%113 == 0:
			name = fmt.Sprintf("report-%06d.pdf", i)
		case i%97 == 0:
			parent = cacheDir
			parentFRN = 3
			name = fmt.Sprintf("cache-%06d.json", i)
		}
		add(nextFRN, parentFRN, parent, name, 0)
		nextFRN++
	}
	buildOrders(idx)
	return idx
}

func orderedLimitedBenchmarkIndex(n int) *Index {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: make([]CompactRecord, 0, n+2),
	}
	add := func(frn, parentFRN uint64, parent int32, name string, mode uint32) int32 {
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       frn,
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
			Mode:      mode,
			Size:      1024,
			ModUnix:   time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixNano(),
		})
		return int32(len(idx.Records) - 1)
	}
	root := add(1, 1, -1, ".", uint32(os.ModeDir))
	workspace := add(2, 1, root, "workspace", uint32(os.ModeDir))
	nextFRN := uint64(10)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("mm-file-%06d.txt", i)
		switch {
		case i < 100:
			name = fmt.Sprintf("aaneedle-%06d.txt", i)
		case i >= n-100:
			name = fmt.Sprintf("zzneedle-%06d.txt", i)
		}
		add(nextFRN, 2, workspace, name, 0)
		nextFRN++
	}
	buildOrders(idx)
	return idx
}

func broadSubstringOrderingFixture() *Index {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
	}
	add := func(frn, parentFRN uint64, parent int32, name string, mode uint32) int32 {
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       frn,
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
			Mode:      mode,
			Size:      1024,
			ModUnix:   time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixNano(),
		})
		return int32(len(idx.Records) - 1)
	}
	root := add(1, 1, -1, ".", uint32(os.ModeDir))
	folder := add(2, 1, root, "workspace", uint32(os.ModeDir))
	frn := uint64(10)
	for i := 0; i < 20; i++ {
		add(frn, 2, folder, fmt.Sprintf("zz-nrrd-%02d.txt", i), 0)
		frn++
	}
	for i := 0; i < 20; i++ {
		add(frn, 2, folder, fmt.Sprintf("aa-nrrd-%02d.txt", i), 0)
		frn++
	}
	for i := 0; i < 20; i++ {
		add(frn, 2, folder, fmt.Sprintf("mm-%02d.nrrd", i), 0)
		frn++
	}
	buildOrders(idx)
	return idx
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func pathsOf(entries []Entry) []string {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}
	return paths
}

func sameOrderedStrings(a, b []string) bool {
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

func mustParseQuery(t *testing.T, opts queryOptions) parsedQuery {
	t.Helper()
	pq, err := parseQuery(opts)
	if err != nil {
		t.Fatal(err)
	}
	return pq
}

func mustParseQueryB(b *testing.B, opts queryOptions) parsedQuery {
	b.Helper()
	pq, err := parseQuery(opts)
	if err != nil {
		b.Fatal(err)
	}
	return pq
}
