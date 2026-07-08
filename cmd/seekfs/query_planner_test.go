package main

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
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

func TestNilCandidateProviderMeansEmptyCandidateSet(t *testing.T) {
	idx := commonSearchFixture()
	got, err := searchCompactWithCache(idx, queryOptions{Query: "main", Limit: 20}, false, make(map[int]string), func(parsedQuery) ([]int, bool) {
		return nil, true
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("nil candidate provider returned %d matches, want 0", len(got))
	}
}

func TestExactTopCandidatesFilterPathTerms(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	opts := queryOptions{Query: "src ext:go", MatchPath: true, Limit: 2}
	pq := mustParseQuery(t, opts)
	pq.Limit = normalizedLimit(opts.Limit, false)

	if candidates, ok := vol.exactTopPlannedCandidates(pq); ok {
		t.Fatalf("exactTopPlannedCandidates returned %d candidates for ext + path term query, want decline", len(candidates))
	}
	candidates, ok := vol.plannedCandidates(pq)
	if !ok {
		t.Fatal("plannedCandidates declined ext + path term query")
	}
	fast := entriesForIDs(idx, candidates)
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("exact top ext+path candidates = %v, full = %v", got, want)
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
	if len(plan.sources[0].ids) != 0 || len(plan.sources[0].roots) == 0 {
		t.Fatalf("under source = %+v, want lazy roots without materialized ids", plan.sources[0])
	}
	full, err := searchCompactWithCache(idx, queryOptions{Query: "type:file", MatchPath: true, Under: `C:\fixture\workspace\src`}, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	fast, err := searchCompactWithCache(idx, queryOptions{Query: "type:file", MatchPath: true, Under: `C:\fixture\workspace\src`}, false, make(map[int]string), func(parsedQuery) ([]int, bool) {
		return plan.execute(), true
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sameStringSet(pathsOf(fast), pathsOf(full)) {
		t.Fatalf("lazy under paths = %v, full paths = %v", pathsOf(fast), pathsOf(full))
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

func TestPathOnlyDottedExtensionUsesExtensionCandidate(t *testing.T) {
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
		t.Fatalf("path:.nrrd plan = %+v ok=%v, want extension candidate source", pathPlan.sources, ok)
	}
	if got, want := pathPlan.sources[0].name, "ext:nrrd"; got != want {
		t.Fatalf("path:.nrrd source = %q, want %q", got, want)
	}
	extPlan, ok := vol.buildCandidatePlan(extPQ)
	if !ok || len(extPlan.sources) == 0 {
		t.Fatalf("ext:.nrrd plan = %+v ok=%v, want extension candidate source", extPlan.sources, ok)
	}
	if got, want := extPlan.sources[0].name, "ext:nrrd"; got != want {
		t.Fatalf("ext:.nrrd source = %q, want %q", got, want)
	}
	if got, want := len(pathPlan.execute()), len(extPlan.execute()); got != want {
		t.Fatalf("candidate counts path:.nrrd=%d ext:.nrrd=%d, want same exact-extension set", got, want)
	}
	pathMatches, err := searchCompactWithCache(idx, queryOptions{Query: "path:.nrrd", Limit: 20}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(pathMatches); !sameStringSet(names, []string{"data.nrrd", "scan.nrrd"}) {
		t.Fatalf("path:.nrrd names = %v, want extension matches only", names)
	}
	extMatches, err := searchCompactWithCache(idx, queryOptions{Query: "ext:.nrrd", Limit: 20}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(extMatches); !sameStringSet(names, []string{"data.nrrd", "scan.nrrd"}) {
		t.Fatalf("ext:.nrrd names = %v, want extension matches only", names)
	}
}

func TestPathModeDottedExtensionNarrowsSubstringSemantics(t *testing.T) {
	idx := commonSearchFixture()
	idx.Records = append(idx.Records,
		CompactRecord{FRN: 17, ParentFRN: 3, Parent: 2, Name: "Downloads", Mode: uint32(os.ModeDir)},
		CompactRecord{FRN: 18, ParentFRN: 17, Parent: 16, Name: "scan.nrrd"},
		CompactRecord{FRN: 19, ParentFRN: 17, Parent: 16, Name: "backup.nrrd.bak"},
		CompactRecord{FRN: 20, ParentFRN: 3, Parent: 2, Name: "data.nrrd", Mode: uint32(os.ModeDir)},
		CompactRecord{FRN: 21, ParentFRN: 20, Parent: 19, Name: "metadata.json"},
	)
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	pathMode, err := searchCompactWithCache(idx, queryOptions{Query: "path:.nrrd", Limit: 20}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(pathMode); !sameStringSet(names, []string{"scan.nrrd", "data.nrrd"}) {
		t.Fatalf("path:.nrrd names = %v, want exact extension matches", names)
	}
	nameMode, err := searchCompactWithCache(idx, queryOptions{Query: ".nrrd", Limit: 20}, false, make(map[int]string), vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(nameMode); !sameStringSet(names, []string{"scan.nrrd", "backup.nrrd.bak", "data.nrrd"}) {
		t.Fatalf("name-mode .nrrd names = %v, want substring matches", names)
	}
}

func TestSinglePathTermCandidateCanSkipEntryMatches(t *testing.T) {
	pq := mustParseQuery(t, queryOptions{Query: "path:.opencode", Limit: 20})
	pq.Limit = normalizedLimit(20, false)
	if !compactCandidateCanSkipEntryMatches(pq, true) {
		t.Fatal("single path-term candidate query should skip redundant entryMatches")
	}
	if compactCandidateCanSkipEntryMatches(pq, false) {
		t.Fatal("non-candidate query skipped entryMatches")
	}
	withFilter := mustParseQuery(t, queryOptions{Query: "path:.opencode ext:json", Limit: 20})
	withFilter.Limit = normalizedLimit(20, false)
	if compactCandidateCanSkipEntryMatches(withFilter, true) {
		t.Fatal("candidate query with extra extension filter skipped entryMatches")
	}
	multiTerm := mustParseQuery(t, queryOptions{Query: "path:Downloads .nrrd", Limit: 20})
	multiTerm.Limit = normalizedLimit(20, false)
	if compactCandidateCanSkipEntryMatches(multiTerm, true) {
		t.Fatal("multi-term path query skipped entryMatches")
	}
}

func TestDottedExtensionMultiPathUsesExtensionPosting(t *testing.T) {
	idx := dottedPathBenchmarkIndex(25000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	opts := queryOptions{Query: "Downloads .nrrd", MatchPath: true, Limit: 20}
	trace := &searchTrace{}
	fast, err := searchCompactWithCache(idx, queryOptions{Query: opts.Query, MatchPath: opts.MatchPath, Limit: opts.Limit, Trace: trace}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if trace.Source != "planned:ext:nrrd" && trace.Source != "path-directory-term-top" {
		t.Fatalf("source = %q, want planned:ext:nrrd or path-directory-term-top", trace.Source)
	}
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
}

func TestBareExtensionMultiPathUsesExtensionPosting(t *testing.T) {
	idx := dottedPathBenchmarkIndex(25000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	opts := queryOptions{Query: "Downloads nrrd", MatchPath: true, Limit: 20}
	trace := &searchTrace{}
	fast, err := searchCompactWithCache(idx, queryOptions{Query: opts.Query, MatchPath: opts.MatchPath, Limit: opts.Limit, Trace: trace}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if trace.Source != "path-bare-extension-multi-top" && trace.Source != "path-directory-term-top" {
		t.Fatalf("source = %q, want path-bare-extension-multi-top or path-directory-term-top", trace.Source)
	}
	dotted, err := searchCompactWithCache(idx, queryOptions{Query: "Downloads .nrrd", MatchPath: true, Limit: 20}, false, make(map[int]string), vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOf(fast), pathsOf(dotted); !sameOrderedStrings(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
}

func TestOverlayAwareCandidatesKeepNonEmptyBaseAndDeclineEmpty(t *testing.T) {
	idx := dottedPathBenchmarkIndex(25000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)

	hit := mustParseQuery(t, queryOptions{Query: "Downloads .nrrd", MatchPath: true, Limit: 20})
	hit.Limit = normalizedLimit(20, false)
	candidates, ok := vol.overlayAwareNameTermCandidates(hit)
	if !ok || len(candidates) == 0 {
		t.Fatalf("overlayAwareNameTermCandidates hit = %d ok=%v, want non-empty candidates", len(candidates), ok)
	}

	miss := mustParseQuery(t, queryOptions{Query: "Downloads missing-nrrd-token", MatchPath: true, Limit: 20})
	miss.Limit = normalizedLimit(20, false)
	if candidates, ok := vol.overlayAwareNameTermCandidates(miss); ok {
		t.Fatalf("overlayAwareNameTermCandidates miss = %d ok=true, want decline", len(candidates))
	}
}

func TestMultiTermEmptyPathDeclinesDirectoryComponentQueries(t *testing.T) {
	idx := dottedPathBenchmarkIndex(25000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	pq := mustParseQuery(t, queryOptions{Query: "Downloads nrrd", MatchPath: true, Limit: 20})
	pq.Limit = normalizedLimit(20, false)
	if candidates, ok := vol.multiTermEmptyPathCandidates(pq); ok {
		t.Fatalf("multiTermEmptyPathCandidates returned %d candidates, want decline", len(candidates))
	}
}

func TestLimitedDottedPathScanDeclinesUnderfilledResults(t *testing.T) {
	idx := dottedPathBenchmarkIndex(25000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	opts := queryOptions{Query: "path:ai.opencode.desktop", Limit: 20}
	pq := mustParseQuery(t, opts)
	pq.Limit = normalizedLimit(opts.Limit, false)
	if candidates, ok := vol.limitedDottedPathScanCandidates(pq); ok {
		t.Fatalf("limitedDottedPathScanCandidates returned %d underfilled candidates, want decline", len(candidates))
	}
	filtered := mustParseQuery(t, queryOptions{Query: "path:ai.opencode.desktop ext:json", Limit: 20})
	filtered.Limit = normalizedLimit(20, false)
	if _, ok := vol.limitedDottedPathScanCandidates(filtered); ok {
		t.Fatal("limited dotted path scan accepted filtered query")
	}
}

func TestSearchTraceReportsCandidateSource(t *testing.T) {
	idx := dottedPathBenchmarkIndex(1200)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []struct {
		name       string
		opts       queryOptions
		wantSource string
	}{
		{
			name:       "dotted path extension",
			opts:       queryOptions{Query: "path:.nrrd", Limit: 20},
			wantSource: "planned:ext-top",
		},
		{
			name:       "extension planner",
			opts:       queryOptions{Query: "ext:.pdf", MatchPath: true, Limit: 20},
			wantSource: "planned:ext:pdf",
		},
		{
			name:       "limited missing term",
			opts:       queryOptions{Query: "zzzzzz-no-hit", Limit: 20},
			wantSource: "limited-single-term",
		},
		{
			name:       "broad path terms",
			opts:       queryOptions{Query: "workspace plain", MatchPath: true, Limit: 20},
			wantSource: "planned:path-term:plain+path-term:workspace",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trace := &searchTrace{}
			opts := tc.opts
			opts.Trace = trace
			if _, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates); err != nil {
				t.Fatal(err)
			}
			if trace.Source != tc.wantSource {
				t.Fatalf("trace source = %q, want %q", trace.Source, tc.wantSource)
			}
			if trace.Candidates < 0 {
				t.Fatalf("trace candidates = %d, want non-negative", trace.Candidates)
			}
		})
	}
}

func TestExtensionShapedPathTopCandidatesAvoidDottedScan(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := dottedPathBenchmarkIndex(25000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	opts := queryOptions{Query: "path:.pdf", Limit: 20}
	trace := &searchTrace{}
	fastOpts := opts
	fastOpts.Trace = trace

	fast, err := searchCompactWithCache(idx, fastOpts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("extension-shaped top paths = %v, full paths = %v", got, want)
	}
	if trace.Source != "planned:ext-top" {
		t.Fatalf("trace source = %q, want planned:ext-top", trace.Source)
	}
	if trace.Candidates != 20 {
		t.Fatalf("trace candidates = %d, want 20", trace.Candidates)
	}
}

func TestFusedDottedPathNoHitUsesIntersectedTrigramCandidates(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := dottedPathBenchmarkIndex(25000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	opts := queryOptions{Query: "path:Downloads.nrrd", Limit: 20}
	trace := &searchTrace{}
	fastOpts := opts
	fastOpts.Trace = trace

	fast, err := searchCompactWithCache(idx, fastOpts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(fast) != 0 {
		t.Fatalf("fused no-hit matches = %v, want none", pathsOf(fast))
	}
	if trace.Source != "path-component-trigram" {
		t.Fatalf("trace source = %q, want path-component-trigram", trace.Source)
	}
	if trace.Candidates != 0 {
		t.Fatalf("trace candidates = %d, want 0", trace.Candidates)
	}
}

func TestLongComponentTermUsesIntersectedTrigramDespiteCommonGrams(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	opts := queryOptions{Query: "path:opencode", Limit: 20}
	trace := &searchTrace{}
	fastOpts := opts
	fastOpts.Trace = trace

	fast, err := searchCompactWithCache(idx, fastOpts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("opencode trigram paths = %v, full paths = %v", got, want)
	}
	if trace.Source != "path-component-trigram" {
		t.Fatalf("trace source = %q, want path-component-trigram", trace.Source)
	}
}

func TestLimitedPathTermUsesTrigramNameMatches(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	opts := queryOptions{Query: "path:opencode", Limit: 20}
	pq := mustParseQuery(t, opts)
	pq.Limit = normalizedLimit(opts.Limit, false)

	candidates, ok := vol.pathPlanTermPostingLimited("opencode", pq)
	if !ok {
		t.Fatal("pathPlanTermPostingLimited declined opencode")
	}
	fast := entriesForIDs(idx, candidates)
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("limited opencode paths = %v, full paths = %v", got, want)
	}
}

func TestPathNameTrigramTopCandidatesForManyDirectMatches(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := manyDirectNameMatchIndex("opencode", 80)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	opts := queryOptions{Query: "path:opencode", Limit: 20}
	pq := mustParseQuery(t, opts)
	pq.Limit = normalizedLimit(opts.Limit, false)

	candidates, ok := vol.nameTrigramPathNameTopCandidates(pq)
	if !ok {
		t.Fatal("nameTrigramPathNameTopCandidates declined direct opencode matches")
	}
	if len(candidates) != 20 {
		t.Fatalf("candidate count = %d, want 20", len(candidates))
	}
	fast := entriesForIDs(idx, candidates)
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("top trigram paths = %v, full paths = %v", got, want)
	}
}

func TestPathNameTrigramTopCandidatesBoundLargeDirectory(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := broadComponentExpansionIndex(serviceComponentTrigramExpansionMaxIDs + 500)
	dir := idx.compactRecord(1)
	dir.Name = "opencode"
	idx.setCompactRecord(1, dir)
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	opts := queryOptions{Query: "path:opencode", Limit: 20}
	pq := mustParseQuery(t, opts)
	pq.Limit = normalizedLimit(opts.Limit, false)

	candidates, ok := vol.nameTrigramPathNameTopCandidates(pq)
	if !ok {
		t.Fatal("nameTrigramPathNameTopCandidates declined large matching directory")
	}
	if len(candidates) != 20 {
		t.Fatalf("candidate count = %d, want 20", len(candidates))
	}
	fast := entriesForIDs(idx, candidates)
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("large directory top paths = %v, full paths = %v", got, want)
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
			query:     "path:.nrrd",
			wantHas:   []string{"scan-000037.nrrd", "dataset-000000.nrrd"},
			wantLacks: []string{"backup-000053.nrrd.bak", "metadata-000000.json"},
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
			wantHas:   nil,
			wantLacks: []string{"metadata-000000.json", "scan-000037.nrrd", "backup-000053.nrrd.bak", "cache-000097.json"},
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
		{Query: "path:trainingdata .nrrd", Limit: 25},
		{Query: "path:trainingdata Dataset .nrrd", Limit: 25},
		{Query: "path:Dataset trainingdata .nrrd", Limit: 25},
		{Query: ".nrrd path:trainingdata Dataset", Limit: 25},
		{Query: "path:trainingdata path:Dataset .nrrd", Limit: 25},
		{Query: "path:workspace trainingdata Dataset .nrrd", Limit: 25},
		{Query: "path:trainingdata Dataset ext:nrrd", Limit: 25},
		{Query: "path:trainingdata Dataset glob:*.nrrd", Limit: 25},
		{Query: "path:trainingdata missing .nrrd", Limit: 25},
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
	vol.rebuildNameTrigramsLocked()
	pathTerms := []string{
		"path:.nrrd",
		"path:nrrd",
		"path:cache",
		"path:dataset",
		"path:workspace",
		"path:trainingdata",
		"path:trainingdata Dataset",
		"path:Dataset trainingdata",
		"path:workspace trainingdata Dataset",
		"path:missing Dataset",
	}
	filters := []string{"", "ext:nrrd", "ext:json", "type:file", "glob:*.json", "!backup", "!metadata", "ext:nrrd|json"}
	filters = append(filters, ".nrrd", "glob:*.nrrd")
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

func TestMultiPartPathQueriesUseBoundedCandidateSources(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	idx := dottedPathBenchmarkIndex(2000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.rebuildNameTrigramsLocked()
	queries := []queryOptions{
		{Query: "path:trainingdata Dataset .nrrd", Limit: 25},
		{Query: "path:Dataset trainingdata .nrrd", Limit: 25},
		{Query: ".nrrd path:trainingdata Dataset", Limit: 25},
		{Query: "path:trainingdata path:Dataset .nrrd", Limit: 25},
		{Query: "path:workspace trainingdata Dataset .nrrd", Limit: 25},
		{Query: "path:trainingdata Dataset ext:nrrd", Limit: 25},
		{Query: "path:trainingdata Dataset glob:*.nrrd", Limit: 25},
		{Query: "path:trainingdata missing .nrrd", Limit: 25},
	}
	for _, opts := range queries {
		t.Run(opts.Query, func(t *testing.T) {
			pq := mustParseQuery(t, opts)
			pq.Limit = normalizedLimit(opts.Limit, false)
			plan, ok := vol.buildCandidatePlan(pq)
			if !ok {
				t.Fatal("buildCandidatePlan declined multi-part path query")
			}
			summary := plan.sourceSummary()
			if !plan.empty && !strings.Contains(summary, "path-term:") &&
				!strings.Contains(summary, "ext:") && !strings.Contains(summary, "glob-ext:") {
				t.Fatalf("plan sources = %s, want a bounded path, extension, or glob source", summary)
			}
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

func TestExtensionBoundedPathTermsAvoidBroadPathReconstruction(t *testing.T) {
	idx := highExtensionFanoutPathIndex(3500)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	queries := []queryOptions{
		{Query: "path:trainingdata Dataset ext:nrrd", Limit: 20},
		{Query: "path:trainingdata Dataset .nrrd", Limit: 20},
		{Query: "path:trainingdata Dataset nrrd", Limit: 20},
		{Query: "path:absent Dataset ext:nrrd", Limit: 20},
		{Query: "path:absent Dataset .nrrd", Limit: 20},
		{Query: "path:absent Dataset nrrd", Limit: 20},
	}
	for _, opts := range queries {
		t.Run(opts.Query, func(t *testing.T) {
			pq := mustParseQuery(t, opts)
			dropSatisfiedVolumeTerms(&pq, idx.Volume)
			pq.Limit = normalizedLimit(opts.Limit, false)
			plan, ok := vol.buildCandidatePlan(pq)
			if !ok {
				t.Fatalf("plan = %+v, ok=%v, want bounded candidate plan", plan, ok)
			}
			summary := plan.sourceSummary()
			if !plan.empty && !strings.Contains(summary, "ext:nrrd") &&
				!strings.Contains(summary, "term:") && !strings.Contains(summary, "path-term:") {
				t.Fatalf("plan sources = %s, want extension or bounded term source", summary)
			}

			full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("full search: %v", err)
			}
			pathCache := make(map[int]string)
			fast, err := searchCompactWithCache(idx, opts, false, pathCache, vol.nameTermCandidates)
			if err != nil {
				t.Fatalf("candidate search: %v", err)
			}
			if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
				t.Fatalf("candidate paths = %v, full paths = %v", got, want)
			}
			if len(pathCache) > 32 {
				t.Fatalf("path cache grew to %d entries for %q; path terms should be verified before broad path reconstruction", len(pathCache), opts.Query)
			}
		})
	}
}

func TestDriveScopedBareSuffixPathTermsPreserveSubstringParity(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	idx := dottedPathBenchmarkIndex(5000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.rebuildNameTrigramsLocked()
	queries := []queryOptions{
		{Query: "path:C: nrrd", Limit: 25},
		{Query: "path:C: raw", Limit: 25},
		{Query: "path:C: pdf", Limit: 25},
		{Query: "path:C: pvsm", Limit: 25},
	}
	for _, opts := range queries {
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
				t.Fatalf("candidate paths = %v, full paths = %v", got, want)
			}
		})
	}
}

func TestGeneratedMultiPartPathSyntaxParityMatrix(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	idx := highExtensionFanoutPathIndex(1200)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.rebuildNameTrigramsLocked()
	pathPhrases := []string{
		"path:trainingdata Dataset",
		"path:Dataset trainingdata",
		"trainingdata path:Dataset",
		"path:trainingdata path:Dataset",
		"path:workspace trainingdata Dataset",
		"path:absent Dataset",
	}
	extForms := []string{"nrrd", ".nrrd", "ext:nrrd", "glob:*.nrrd"}
	volumeForms := []string{"", "path:C:"}
	negativeForms := []string{"", "!backup", "!metadata"}
	limits := []int{1, 5, 20}
	for _, volume := range volumeForms {
		for _, phrase := range pathPhrases {
			for _, ext := range extForms {
				for _, neg := range negativeForms {
					for _, limit := range limits {
						query := strings.Join(nonEmptyStrings(volume, phrase, ext, neg), " ")
						opts := queryOptions{Query: query, Limit: limit}
						t.Run(fmt.Sprintf("%s/limit:%d", query, limit), func(t *testing.T) {
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
}

func TestGeneratedLooseKeywordSyntaxParityMatrix(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	idx := dottedPathBenchmarkIndex(1600)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.rebuildNameTrigramsLocked()
	queries := []string{
		"nrrd",
		".nrrd",
		"raw",
		".raw",
		"pdf",
		".pdf",
		"pvsm",
		".pvsm",
		"F: nrrd",
		"F: .nrrd",
		"C: nrrd",
		"C: .nrrd",
		"C: pvsm",
		"C: .pvsm",
		"trainingdata Dataset nrrd",
		"Dataset trainingdata nrrd",
		"workspace trainingdata Dataset nrrd",
		"Downloads nrrd",
		"Users exampleuser",
		"exampleuser Users",
		"Users exampleuser Downloads",
		"Downloads exampleuser Users",
		"Downloads docx",
		"fixtureproj",
		"F: fixtureproj",
		"path:F: fixtureproj",
		"nrrd !backup",
		"nrrd type:file",
		"nrrd glob:*.json",
		"nrrd ext:json",
		`workspace\nrrd-cache cache`,
		`workspace/dataset-000000.nrrd metadata`,
	}
	limits := []int{1, 5, 25, 100}
	for _, query := range queries {
		for _, limit := range limits {
			opts := queryOptions{Query: query, Limit: limit}
			t.Run(fmt.Sprintf("%s/limit:%d", query, limit), func(t *testing.T) {
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

func TestLooseKeywordServiceVolumeMatrixUnderTarget(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	cIdx := dottedPathBenchmarkIndex(60_000)
	fIdx := dottedPathBenchmarkIndex(60_000)
	fIdx.Volume = "F:"
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("loose-c.gsi", cIdx),
		newServiceVolumeIndex("loose-f.gsi", fIdx),
	}
	for _, vol := range volumes {
		vol.rebuildNameTrigramsLocked()
	}
	queries := []string{
		"nrrd",
		".nrrd",
		"raw",
		".raw",
		"pdf",
		".pdf",
		"pvsm",
		".pvsm",
		"F: nrrd",
		"F: .nrrd",
		"F: raw",
		"F: .raw",
		"F: pdf",
		"F: .pdf",
		"C: pvsm",
		"C: .pvsm",
		"trainingdata Dataset nrrd",
		"Dataset trainingdata nrrd",
		"workspace trainingdata Dataset nrrd",
		"nrrd !backup",
		"nrrd type:file",
		"nrrd glob:*.json",
		`workspace\nrrd-cache cache`,
	}
	all := make([]float64, 0, len(queries)*3)
	perQuery := make(map[string][]float64, len(queries))
	for i := 0; i < 3; i++ {
		for _, query := range queries {
			start := time.Now()
			_, err := searchServiceVolumes(volumes, queryOptions{Query: query, Limit: 100}, false)
			if err != nil {
				t.Fatalf("query %q failed: %v", query, err)
			}
			ms := float64(time.Since(start).Microseconds()) / 1000
			all = append(all, ms)
			perQuery[query] = append(perQuery[query], ms)
		}
	}
	if p95 := percentile(append([]float64(nil), all...), 0.95); p95 > 100 {
		if envBool("SEEKFS_ENFORCE_LATENCY_TESTS") {
			t.Fatalf("loose keyword matrix p95 = %.3fms, want <= 100ms; per-query=%v", p95, perQuery)
		}
		t.Logf("loose keyword matrix p95 = %.3fms over soft 100ms budget; per-query=%v", p95, perQuery)
	}
}

func TestGeneratedLooseQueryFuzzMatrix(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	cIdx := dottedPathBenchmarkIndex(24_000)
	fIdx := dottedPathBenchmarkIndex(24_000)
	fIdx.Volume = "F:"
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("fuzz-c.gsi", cIdx),
		newServiceVolumeIndex("fuzz-f.gsi", fIdx),
	}
	for _, vol := range volumes {
		vol.rebuildNameTrigramsLocked()
	}

	queries := generatedLooseFuzzQueries()
	all := make([]float64, 0, len(queries)*2)
	perQuery := make(map[string][]float64, len(queries))
	for i := 0; i < 2; i++ {
		for _, query := range queries {
			matchPath := looseFuzzMatchPath(query)
			opts := queryOptions{Query: query, MatchPath: matchPath, Limit: 50}
			start := time.Now()
			got, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatalf("query %q failed: %v", query, err)
			}
			ms := float64(time.Since(start).Microseconds()) / 1000
			all = append(all, ms)
			perQuery[query] = append(perQuery[query], ms)

			full, err := searchAll([]*Index{cIdx, fIdx}, opts, false)
			if err != nil {
				t.Fatalf("full query %q failed: %v", query, err)
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(full); !sameOrderedStrings(gotPaths, wantPaths) {
				t.Fatalf("query %q paths = %v, full paths = %v", query, gotPaths, wantPaths)
			}
		}
	}
	if p95 := percentile(append([]float64(nil), all...), 0.95); p95 > 100 {
		if envBool("SEEKFS_ENFORCE_LATENCY_TESTS") {
			t.Fatalf("loose fuzz matrix p95 = %.3fms, want <= 100ms; per-query=%v", p95, perQuery)
		}
		t.Logf("loose fuzz matrix p95 = %.3fms over soft 100ms budget; per-query=%v", p95, perQuery)
	}
}

func TestRealIndexGeneratedPathSubstringMatrix(t *testing.T) {
	if !envBool("SEEKFS_REAL_INDEX_LOAD_MATRIX") {
		t.Skip("set SEEKFS_REAL_INDEX_LOAD_MATRIX=1 plus SEEKFS_REAL_INDEX_DBS or bench/real-indexes.local.txt to load real indexes in-process")
	}
	dbs := realIndexTestDBs(t)
	if len(dbs) == 0 {
		t.Skip("set SEEKFS_REAL_INDEX_DBS or create bench/real-indexes.local.txt to run real-index substring matrix")
	}
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	volumes := make([]*serviceVolumeIndex, 0, len(dbs))
	for _, db := range dbs {
		idx, err := loadIndexForService(db)
		if err != nil {
			t.Fatalf("load %s: %v", db, err)
		}
		vol := newServiceVolumeIndex(db, idx)
		if idx.Derived.Postings == nil || len(idx.Derived.NameOrder) == 0 || len(idx.Derived.NameRank) == 0 {
			vol.queryIndex = buildResidentQueryIndex(vol)
		}
		vol.resetNameTrigrams()
		if vol.needsCompactChildrenBuild() {
			vol.buildCompactChildren()
		}
		if vol.nameTrigramIndex() == nil {
			vol.rebuildNameTrigramsLocked()
		}
		volumes = append(volumes, vol)
	}
	queries := realIndexGeneratedPathQueries(volumes)
	if len(queries) == 0 {
		t.Fatal("no real-index queries generated")
	}
	if len(queries) > 96 {
		queries = queries[:96]
	}
	type slowQuery struct {
		query      string
		source     string
		decline    string
		candidates int
		ms         float64
		err        string
	}
	var slow []slowQuery
	for _, query := range queries {
		trace := &searchTrace{}
		opts := queryOptions{
			Query:        query,
			MatchPath:    true,
			Limit:        5,
			Trace:        trace,
			DeadlineUnix: time.Now().Add(750 * time.Millisecond).UnixNano(),
		}
		start := time.Now()
		_, err := searchServiceVolumes(volumes, opts, false)
		ms := float64(time.Since(start).Microseconds()) / 1000
		if err != nil {
			slow = append(slow, slowQuery{query: query, source: trace.Source, decline: trace.Decline, candidates: trace.Candidates, ms: ms, err: err.Error()})
			continue
		}
		if ms > 250 {
			slow = append(slow, slowQuery{query: query, source: trace.Source, decline: trace.Decline, candidates: trace.Candidates, ms: ms})
		}
	}
	if len(slow) > 0 {
		sort.Slice(slow, func(i, j int) bool { return slow[i].ms > slow[j].ms })
		limit := min(len(slow), 20)
		if envBool("SEEKFS_ENFORCE_LATENCY_TESTS") {
			t.Fatalf("real-index substring matrix had %d slow queries >250ms; slowest=%+v", len(slow), slow[:limit])
		}
		t.Logf("real-index substring matrix had %d slow queries over soft 250ms budget; slowest=%+v", len(slow), slow[:limit])
	}
}

func TestRealServiceGeneratedPathSubstringMatrix(t *testing.T) {
	if !envBool("SEEKFS_REAL_SERVICE_MATRIX") {
		t.Skip("set SEEKFS_REAL_SERVICE_MATRIX=1 to run against the resident service")
	}
	info, err := callService(defaultServicePipe, serviceRequest{Command: "info"})
	if err != nil {
		t.Fatalf("service info: %v", err)
	}
	if len(info.DBs) == 0 {
		t.Fatal("resident service has no loaded indexes")
	}
	queries := realServiceSeedQueries(info)
	queries = appendUniqueQueries(queries, realServiceSampledQueries(t, queries)...)
	if len(queries) > 128 {
		queries = queries[:128]
	}
	type slowQuery struct {
		query      string
		source     string
		decline    string
		candidates int
		searchMS   float64
		wallMS     float64
		err        string
	}
	var slow []slowQuery
	for i, query := range queries {
		opts := queryOptions{
			Query:        query,
			MatchPath:    true,
			Limit:        5,
			DeadlineUnix: time.Now().Add(750 * time.Millisecond).UnixNano(),
			RequestSeq:   time.Now().UnixNano() + int64(i),
		}
		start := time.Now()
		resp, err := benchServiceQuery(defaultServicePipe, opts)
		wallMS := float64(time.Since(start).Microseconds()) / 1000
		if err != nil {
			slow = append(slow, slowQuery{query: query, wallMS: wallMS, err: err.Error()})
			continue
		}
		if !resp.OK {
			slow = append(slow, slowQuery{query: query, source: resp.Source, decline: resp.Decline, candidates: resp.Candidates, searchMS: resp.SearchMS, wallMS: wallMS, err: resp.Message})
			continue
		}
		if wallMS > 250 || resp.SearchMS > 250 {
			slow = append(slow, slowQuery{query: query, source: resp.Source, decline: resp.Decline, candidates: resp.Candidates, searchMS: resp.SearchMS, wallMS: wallMS})
		}
	}
	if len(slow) > 0 {
		sort.Slice(slow, func(i, j int) bool { return slow[i].wallMS > slow[j].wallMS })
		if envBool("SEEKFS_ENFORCE_LATENCY_TESTS") {
			t.Fatalf("real-service substring matrix had %d slow/failing queries >250ms; slowest=%+v", len(slow), slow[:min(len(slow), 20)])
		}
		t.Logf("real-service substring matrix had %d slow/failing queries over soft 250ms budget; slowest=%+v", len(slow), slow[:min(len(slow), 20)])
	}
	t.Logf("checked %d resident-service real-index path substring queries", len(queries))
}

func realServiceSeedQueries(info serviceResponse) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, 128)
	add := func(query string) {
		if _, ok := seen[query]; ok {
			return
		}
		seen[query] = struct{}{}
		out = append(out, query)
	}
	for _, query := range []string{
		"Downloads docx",
		"fixtureproj",
		"F: fixtureproj",
		"path:F: fixtureproj",
		"Downloads raw",
		"Downloads nrrd",
		"path:C: raw",
		"path:C: docx",
		"path:F: pdf",
		"raw path:C:",
		"docx path:C:",
		"pdf path:F:",
		"path:Downloads ext:docx",
		"path:Downloads ext:raw",
		"path:Downloads ext:nrrd",
		"path:C: ext:raw",
		"path:C: ext:docx",
		"path:F: ext:pdf",
		"trainingdata Dataset nrrd",
		"Dataset trainingdata nrrd",
		"nrrd Dataset trainingdata",
		"path:trainingdata Dataset .nrrd",
		"path:Dataset trainingdata .nrrd",
	} {
		add(query)
	}
	terms := []string{"raw", "nrrd", "pdf", "pvsm", "json", "docx", "opencode", "downloads", "users", "appdata", "trainingdata"}
	for _, db := range info.DBs {
		volume := db.Volume
		if volume == "" {
			continue
		}
		for _, term := range terms {
			add(fmt.Sprintf("path:%s %s", volume, term))
			if realIndexTermLooksExtension(term) {
				add(fmt.Sprintf("path:%s .%s", volume, term))
			}
		}
	}
	return out
}

func realServiceSampledQueries(t *testing.T, seeds []string) []string {
	t.Helper()
	seen := make(map[string]struct{})
	var out []string
	add := func(query string) {
		query = strings.Join(strings.Fields(query), " ")
		if query == "" {
			return
		}
		if _, ok := seen[query]; ok {
			return
		}
		seen[query] = struct{}{}
		out = append(out, query)
	}
	for i, seed := range seeds {
		resp, err := benchServiceQuery(defaultServicePipe, queryOptions{
			Query:        seed,
			MatchPath:    true,
			Limit:        12,
			DeadlineUnix: time.Now().Add(750 * time.Millisecond).UnixNano(),
			RequestSeq:   time.Now().UnixNano() + int64(i),
		})
		if err != nil || !resp.OK {
			continue
		}
		for _, path := range resp.Results {
			terms := pathTermsForRealIndexQuery(path)
			if len(terms) < 2 {
				continue
			}
			volume := volumePrefixFromPath(path)
			for n := 2; n <= min(5, len(terms)); n++ {
				for start := 0; start+n <= len(terms) && start < 3; start++ {
					window := append([]string(nil), terms[start:start+n]...)
					add(strings.Join(window, " "))
					if volume != "" {
						add(fmt.Sprintf("path:%s %s", volume, strings.Join(window, " ")))
					}
					rev := append([]string(nil), window...)
					for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
						rev[i], rev[j] = rev[j], rev[i]
					}
					add(strings.Join(rev, " "))
					if volume != "" {
						add(fmt.Sprintf("path:%s %s", volume, strings.Join(rev, " ")))
					}
					if ext := extensionTermFromQueryTerms(window); ext != "" {
						add(fmt.Sprintf("path:%s ext:%s", window[0], ext))
						if volume != "" {
							add(fmt.Sprintf("path:%s %s ext:%s", volume, window[0], ext))
						}
					}
				}
			}
		}
	}
	return out
}

func appendUniqueQueries(base []string, more ...string) []string {
	seen := make(map[string]struct{}, len(base)+len(more))
	out := make([]string, 0, len(base)+len(more))
	for _, query := range append(append([]string(nil), base...), more...) {
		query = strings.Join(strings.Fields(query), " ")
		if query == "" {
			continue
		}
		if _, ok := seen[query]; ok {
			continue
		}
		seen[query] = struct{}{}
		out = append(out, query)
	}
	return out
}

func volumePrefixFromPath(path string) string {
	if len(path) >= 2 && path[1] == ':' {
		return strings.ToUpper(path[:2])
	}
	return ""
}

func extensionTermFromQueryTerms(terms []string) string {
	for i := len(terms) - 1; i >= 0; i-- {
		term := strings.TrimPrefix(terms[i], ".")
		if realIndexTermLooksExtension(term) {
			return term
		}
	}
	return ""
}

func realIndexTestDBs(t *testing.T) []string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("SEEKFS_REAL_INDEX_DBS"))
	var paths []string
	if raw != "" {
		for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ';' || r == '\n' || r == '\r' }) {
			if path := strings.TrimSpace(part); path != "" {
				paths = append(paths, path)
			}
		}
	}
	if len(paths) == 0 {
		path := filepath.Join("..", "..", "bench", "real-indexes.local.txt")
		data, err := os.ReadFile(path)
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				paths = append(paths, line)
			}
		}
	}
	out := paths[:0]
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			out = append(out, path)
		} else {
			t.Logf("skipping missing real index %s: %v", path, err)
		}
	}
	return out
}

func realIndexGeneratedPathQueries(volumes []*serviceVolumeIndex) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, 256)
	add := func(query string) {
		query = strings.TrimSpace(query)
		if query == "" {
			return
		}
		if _, ok := seen[query]; ok {
			return
		}
		seen[query] = struct{}{}
		out = append(out, query)
	}
	commonSuffixes := []string{"raw", "nrrd", "pdf", "pvsm", "json", "docx", "opencode", "downloads", "users", "appdata"}
	for _, vol := range volumes {
		if vol == nil || vol.index == nil {
			continue
		}
		volume := vol.index.Volume
		for _, term := range commonSuffixes {
			add(fmt.Sprintf("path:%s %s", volume, term))
			if realIndexTermLooksExtension(term) {
				add(fmt.Sprintf("path:%s .%s", volume, term))
			}
		}
		for _, terms := range sampleRealPathTerms(vol, 24) {
			if len(terms) == 0 {
				continue
			}
			add(fmt.Sprintf("path:%s %s", volume, strings.Join(terms, " ")))
			if len(terms) >= 2 {
				rev := append([]string(nil), terms...)
				for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
					rev[i], rev[j] = rev[j], rev[i]
				}
				add(fmt.Sprintf("path:%s %s", volume, strings.Join(rev, " ")))
				add(strings.Join(terms, " "))
			}
		}
	}
	return out
}

func realIndexTermLooksExtension(term string) bool {
	if len(term) < 2 || len(term) > 8 {
		return false
	}
	switch term {
	case "raw", "nrrd", "pdf", "pvsm", "json", "docx":
		return true
	default:
		return false
	}
}

func sampleRealPathTerms(vol *serviceVolumeIndex, limit int) [][]string {
	recordCount := vol.index.compactRecordCount()
	if recordCount == 0 || limit <= 0 {
		return nil
	}
	step := max(1, recordCount/limit)
	out := make([][]string, 0, limit)
	for id := 0; id < recordCount && len(out) < limit; id += step {
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Name == "" || rec.Name == "." {
			continue
		}
		path := vol.index.reconstructCompactPathCached(id, vol.pathCache)
		terms := pathTermsForRealIndexQuery(path)
		if len(terms) > 0 {
			out = append(out, terms)
		}
	}
	return out
}

func pathTermsForRealIndexQuery(path string) []string {
	fields := strings.FieldsFunc(strings.ToLower(path), func(r rune) bool {
		return r == '\\' || r == '/' || r == ':' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, 5)
	for _, field := range fields {
		field = strings.Trim(field, "._-()[]{}")
		if len(field) < 3 || len(field) > 32 {
			continue
		}
		if strings.IndexFunc(field, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-')
		}) >= 0 {
			continue
		}
		out = append(out, field)
		if ext := strings.TrimPrefix(filepath.Ext(field), "."); len(ext) >= 2 && len(ext) <= 8 {
			out = append(out, ext)
		}
		if len(out) >= 5 {
			break
		}
	}
	return out
}

type randomCorpusParams struct {
	Records int
}

func TestSeededQueryOracleMatrix(t *testing.T) {
	seeds := []int64{
		1, 2, 3, 4, 5, 6, 7, 8,
		11, 13, 17, 19, 23, 29, 31, 37,
		41, 43, 47, 53, 59, 61, 67, 71,
		73, 79, 83, 89, 97, 101, 103, 107,
	}
	if raw := strings.TrimSpace(os.Getenv("SEEKFS_QUERY_MATRIX_SEED")); raw != "" {
		var seed int64
		if _, err := fmt.Sscanf(raw, "%d", &seed); err != nil {
			t.Fatalf("invalid SEEKFS_QUERY_MATRIX_SEED %q: %v", raw, err)
		}
		seeds = []int64{seed}
	}
	for _, seed := range seeds {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			idx := randomCorpusIndex(seed, randomCorpusParams{Records: 240})
			variants := []struct {
				name  string
				setup func(*serviceVolumeIndex)
			}{
				{name: "resident", setup: func(vol *serviceVolumeIndex) {
					vol.rebuildNameTrigramsLocked()
				}},
				{name: "rebuilt", setup: func(vol *serviceVolumeIndex) {
					vol.rebuildNameTrigramsLocked()
					vol.resetNameTrigrams()
					vol.rebuildNameTrigramsLocked()
				}},
				{name: "no-query-index", setup: func(vol *serviceVolumeIndex) {
					vol.queryIndex = nil
				}},
				{name: "no-child-ranges", setup: func(vol *serviceVolumeIndex) {
					vol.children = nil
					vol.childOffsets = nil
					vol.childIDs = nil
					vol.subtreeStart = nil
					vol.subtreeEnd = nil
					vol.subtreeOrder = nil
				}},
			}
			queries := randomQueries(seed, idx)
			for _, variant := range variants {
				variant := variant
				t.Run(variant.name, func(t *testing.T) {
					for i, opts := range queries {
						opts := opts
						for _, countOnly := range []bool{false, i%7 == 0} {
							vol := newServiceVolumeIndex(fmt.Sprintf("seed-%d-%s.gsi", seed, variant.name), idx)
							variant.setup(vol)
							assertCompactOracleParity(t, seed, idx, vol, opts, countOnly)
							assertQueryMetamorphicProperties(t, seed, idx, vol, opts)
						}
					}
				})
			}
		})
	}
}

func TestSecondOracleSeededSubsample(t *testing.T) {
	for _, seed := range []int64{1, 6, 17, 31} {
		idx := randomCorpusIndex(seed, randomCorpusParams{Records: 96})
		queries := randomQueries(seed, idx)
		if len(queries) > 18 {
			queries = queries[:18]
		}
		for _, opts := range queries {
			opts := opts
			t.Run(fmt.Sprintf("seed-%d/%s", seed, opts.Query), func(t *testing.T) {
				if _, err := parseQuery(opts); err != nil {
					t.Skipf("generated invalid query: %v", err)
				}
				full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
				if err != nil {
					t.Fatalf("compact oracle: %v", err)
				}
				naive, err := secondOracleSearch(idx, opts)
				if err != nil {
					t.Fatalf("second oracle: %v", err)
				}
				if !sameOrderedStrings(pathsOf(full), pathsOf(naive)) {
					t.Fatalf("second oracle paths=%v compact=%v", pathsOf(naive), pathsOf(full))
				}
			})
		}
	}
}

func TestParserTokenProperties(t *testing.T) {
	tokens := []string{
		"workspace", "README.md", "Übersicht", "資料", "x", "xy",
		"ext:go", "ext:pdf|txt", "dir:src", "glob:*.go", "regex:.*go.*",
		"type:file", "type:dir", "case:", "!backup", "go|pdf", "path:src",
		"C:", "F", "bad:filter",
	}
	for seed := int64(1); seed <= 128; seed++ {
		rng := rand.New(rand.NewSource(seed))
		fields := make([]string, 0, 5)
		for i := 0; i < 1+rng.Intn(5); i++ {
			fields = append(fields, tokens[rng.Intn(len(tokens))])
		}
		opts := queryOptions{
			Query:     strings.Join(fields, " "),
			MatchPath: rng.Intn(2) == 0,
			Limit:     25,
		}
		pq, err := parseQueryNoPanic(t, opts)
		if err != nil {
			continue
		}
		if len(pq.Regexps) > 0 || len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 ||
			len(pq.NotGroups) > 0 || (pq.CaseSensitive && len(pq.OrGroups) > 0) {
			continue
		}
		rendered := renderParsedQueryForTest(pq)
		if rendered == "" {
			continue
		}
		round, err := parseQueryNoPanic(t, queryOptions{Query: rendered, MatchPath: pq.MatchPath, Limit: 25})
		if err != nil {
			t.Fatalf("seed=%d rendered query %q did not parse: %v", seed, rendered, err)
		}
		if got, want := parsedQuerySignature(round), parsedQuerySignature(pq); got != want {
			t.Fatalf("seed=%d query=%q rendered=%q signature=%q want=%q", seed, opts.Query, rendered, got, want)
		}
	}
}

func TestBroadPathScanCancellationPropagates(t *testing.T) {
	idx := dottedPathBenchmarkIndex(2000)
	vol := newServiceVolumeIndex("cancel-broad.gsi", idx)
	opts := queryOptions{
		Query:        "plain",
		MatchPath:    true,
		Limit:        20,
		DeadlineUnix: time.Now().Add(-time.Second).UnixNano(),
	}
	_, err := searchCompactWithCache(idx, opts, false, vol.pathCache, func(pq parsedQuery) ([]int, bool) {
		return vol.broadPathScanCandidates(pq)
	})
	if err != errQueryCanceled {
		t.Fatalf("broad scan cancellation err = %v, want %v", err, errQueryCanceled)
	}
}

func FuzzQueryOracleParity(f *testing.F) {
	f.Add(int64(1), "workspace")
	f.Add(int64(6), "Übersicht")
	f.Add(int64(17), "ext:pdf")
	f.Add(int64(29), "path:src type:file")
	f.Fuzz(func(t *testing.T, seed int64, queryBytes string) {
		idx := randomCorpusIndex(seed, randomCorpusParams{Records: 96})
		queries := randomQueries(seed, idx)
		if query := fuzzQueryString(queryBytes); query != "" {
			queries = append(queries, queryOptions{
				Query:     query,
				MatchPath: len(query)%2 == 0 || queryLooksPathScoped(query),
				Limit:     []int{1, 2, 25, 10000}[len(query)%4],
			})
		}
		vol := newServiceVolumeIndex(fmt.Sprintf("fuzz-%d.gsi", seed), idx)
		vol.rebuildNameTrigramsLocked()
		for _, opts := range queries {
			if _, err := parseQuery(opts); err != nil {
				continue
			}
			assertCompactOracleParity(t, seed, idx, vol, opts, false)
			if opts.Limit != 1 {
				countOpts := opts
				countOpts.Limit = 0
				assertCompactOracleParity(t, seed, idx, vol, countOpts, true)
			}
		}
	})
}

func randomCorpusIndex(seed int64, params randomCorpusParams) *Index {
	if params.Records <= 0 {
		params.Records = 200
	}
	rng := rand.New(rand.NewSource(seed))
	idx := &Index{
		Source:  "random",
		Volume:  "C:",
		Compact: true,
		Records: make([]CompactRecord, 0, params.Records+32),
	}
	nextFRN := uint64(1)
	add := func(parent int32, name string, mode uint32) int32 {
		parentFRN := nextFRN
		if parent >= 0 && int(parent) < len(idx.Records) {
			parentFRN = idx.Records[parent].FRN
		}
		rec := CompactRecord{
			FRN:       nextFRN,
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
			Mode:      mode,
			Size:      int64(rng.Intn(1_000_000)),
			ModUnix:   time.Date(2026, time.January, 1+rng.Intn(28), rng.Intn(24), 0, 0, 0, time.UTC).UnixNano(),
		}
		if rng.Intn(11) == 0 {
			rec.Size = 0
		}
		if rng.Intn(13) == 0 {
			rec.ModUnix = 0
		}
		idx.Records = append(idx.Records, rec)
		nextFRN++
		return int32(len(idx.Records) - 1)
	}
	root := add(-1, ".", uint32(os.ModeDir))
	workspace := add(root, "workspace", uint32(os.ModeDir))
	users := add(root, "Users", uint32(os.ModeDir))
	user := add(users, "exampleuser", uint32(os.ModeDir))
	downloads := add(user, "Downloads", uint32(os.ModeDir))
	src := add(workspace, "src", uint32(os.ModeDir))
	docs := add(workspace, "docs", uint32(os.ModeDir))
	unicodeDir := add(workspace, "Übersicht", uint32(os.ModeDir))
	parents := []int32{workspace, users, user, downloads, src, docs, unicodeDir}
	fixed := []struct {
		parent int32
		name   string
		mode   uint32
	}{
		{src, "main.go", 0},
		{src, "MAIN_test.go", 0},
		{docs, "README.md", 0},
		{docs, "Project Specification - v1.2.docx", 0},
		{unicodeDir, "Übersicht.pdf", 0},
		{unicodeDir, "ДОКУМЕНТ.txt", 0},
		{unicodeDir, "資料.csv", 0},
		{downloads, "a.b.c.d", 0},
		{downloads, ".leading-dot", 0},
		{downloads, "trailing-dot.", 0},
		{downloads, "glob-[set]-star*.txt", 0},
		{workspace, "ext", uint32(os.ModeDir)},
		{workspace, "path", uint32(os.ModeDir)},
		{workspace, "c:", uint32(os.ModeDir)},
		{workspace, "x", 0},
		{workspace, "xy", 0},
	}
	for _, item := range fixed {
		id := add(item.parent, item.name, item.mode)
		if item.mode&uint32(os.ModeDir) != 0 {
			parents = append(parents, id)
		}
	}
	stems := []string{"alpha", "Beta", "cache", "dataset", "needle", "report", "scan", "backup", "tmp", "control", "über", "東京"}
	exts := []string{"txt", "go", "pdf", "nrrd", "raw", "json", "md", "csv", "bin"}
	for len(idx.Records) < params.Records {
		parent := parents[rng.Intn(len(parents))]
		if rng.Intn(8) == 0 {
			dir := add(parent, fmt.Sprintf("%s-dir-%03d", stems[rng.Intn(len(stems))], len(idx.Records)), uint32(os.ModeDir))
			parents = append(parents, dir)
			continue
		}
		stem := stems[rng.Intn(len(stems))]
		name := fmt.Sprintf("%s-%03d.%s", stem, rng.Intn(60), exts[rng.Intn(len(exts))])
		switch rng.Intn(10) {
		case 0:
			name = fmt.Sprintf("%s.%s.%03d.%s", stem, stems[rng.Intn(len(stems))], rng.Intn(50), exts[rng.Intn(len(exts))])
		case 1:
			name = strings.Repeat("longname", 8) + fmt.Sprintf("-%03d.%s", rng.Intn(50), exts[rng.Intn(len(exts))])
		case 2:
			name = fmt.Sprintf("%s [%03d] ?.txt", stem, rng.Intn(50))
		case 3:
			name = strings.ToUpper(stem) + fmt.Sprintf("-%03d.%s", rng.Intn(50), exts[rng.Intn(len(exts))])
		}
		id := add(parent, name, 0)
		if rng.Intn(31) == 0 {
			idx.Records[id].Deleted = true
		}
	}
	buildOrders(idx)
	return idx
}

func randomQueries(seed int64, idx *Index) []queryOptions {
	rng := rand.New(rand.NewSource(seed ^ 0x5eed5eed))
	paths := randomCorpusPaths(idx)
	terms := randomCorpusTerms(paths)
	base := []queryOptions{
		{Query: "workspace", MatchPath: true, Limit: 25},
		{Query: "src main", MatchPath: true, Limit: 2},
		{Query: "Übersicht", MatchPath: true, Limit: 25},
		{Query: "übersicht", MatchPath: true, Limit: 25},
		{Query: "ДОКУМЕНТ", MatchPath: true, Limit: 25},
		{Query: "資料", MatchPath: true, Limit: 25},
		{Query: "x", MatchPath: true, Limit: 25},
		{Query: "xy", MatchPath: true, Limit: 25},
		{Query: "ext:go", MatchPath: true, Limit: 25},
		{Query: "ext:pdf|txt", MatchPath: true, Limit: 25},
		{Query: "glob:*.go", MatchPath: true, Limit: 25},
		{Query: "type:dir", MatchPath: true, Limit: 25},
		{Query: "type:file !backup", MatchPath: true, Limit: 25},
		{Query: "regex:.*Specification.*", MatchPath: true, Limit: 25},
		{Query: "dir:downloads", MatchPath: true, Limit: 25},
		{Query: "go|pdf", MatchPath: true, Limit: 25},
		{Query: "case: MAIN", MatchPath: true, Limit: 25},
	}
	limits := []int{0, 1, 2, 25, 10000}
	for i := 0; i < 48 && len(terms) > 0; i++ {
		a := terms[rng.Intn(len(terms))]
		b := terms[rng.Intn(len(terms))]
		query := a
		switch rng.Intn(9) {
		case 0:
			query = a + " " + b
		case 1:
			query = "path:" + a
		case 2:
			query = "!" + a + " " + b
		case 3:
			query = a + "|" + b
		case 4:
			query = "dir:" + a
		case 5:
			query = "regex:.*" + regexpSafeLiteral(a) + ".*"
		case 6:
			query = "type:file " + a
		case 7:
			query = "case: " + a
		}
		opt := queryOptions{
			Query:     query,
			MatchPath: rng.Intn(2) == 0 || strings.Contains(query, "path:") || strings.Contains(query, "dir:"),
			Limit:     limits[rng.Intn(len(limits))],
		}
		if rng.Intn(6) == 0 && len(paths) > 0 {
			opt.Under = parentPathForQuery(paths[rng.Intn(len(paths))])
			opt.MatchPath = true
		}
		if rng.Intn(10) == 0 && len(paths) > 0 {
			opt.CWDBias = parentPathForQuery(paths[rng.Intn(len(paths))])
		}
		if rng.Intn(10) == 0 && len(paths) > 0 {
			opt.RootBias = parentPathForQuery(paths[rng.Intn(len(paths))])
		}
		base = append(base, opt)
	}
	return base
}

func randomCorpusPaths(idx *Index) []string {
	paths := make([]string, 0, idx.compactRecordCount())
	cache := make(map[int]string)
	for i := 0; i < idx.compactRecordCount(); i++ {
		rec := idx.compactRecord(i)
		if rec.Deleted {
			continue
		}
		paths = append(paths, idx.reconstructCompactPathCached(i, cache))
	}
	return paths
}

func randomCorpusTerms(paths []string) []string {
	seen := make(map[string]struct{})
	for _, path := range paths {
		fields := strings.FieldsFunc(path, func(r rune) bool {
			return r == '\\' || r == '/' || r == ':' || r == ' ' || r == '\t' || r == '(' || r == ')'
		})
		for _, field := range fields {
			field = strings.Trim(field, "._-[]{}")
			if field == "" || strings.ContainsAny(field, `*?[]`) {
				continue
			}
			addTerm(seen, field)
			lower := strings.ToLower(field)
			addTerm(seen, lower)
			lowerRunes := []rune(lower)
			if len(lowerRunes) > 3 {
				addTerm(seen, string(lowerRunes[:3]))
				addTerm(seen, string(lowerRunes[1:min(len(lowerRunes), 5)]))
			}
			if ext := strings.TrimPrefix(filepath.Ext(field), "."); ext != "" {
				addTerm(seen, ext)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for term := range seen {
		out = append(out, term)
	}
	sort.Strings(out)
	return out
}

func addTerm(seen map[string]struct{}, term string) {
	term = strings.TrimSpace(term)
	if term == "" || strings.ContainsAny(term, `\/*?[]|`) {
		return
	}
	seen[term] = struct{}{}
}

func parentPathForQuery(path string) string {
	dir := filepath.Dir(path)
	if dir == "." || dir == "" {
		return path
	}
	return dir
}

func regexpSafeLiteral(term string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `.`, `\.`, `+`, `\+`, `*`, `\*`, `?`, `\?`, `[`, `\[`, `]`, `\]`, `(`, `\(`, `)`, `\)`, `^`, `\^`, `$`, `\$`)
	return replacer.Replace(term)
}

func fuzzQueryString(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	fields := strings.Fields(raw)
	out := make([]string, 0, min(len(fields), 4))
	for _, field := range fields {
		field = strings.Trim(field, "\"'")
		if field == "" || strings.ContainsAny(field, "\x00\r\n") {
			continue
		}
		if strings.Contains(field, ":") && !strings.HasPrefix(field, "ext:") &&
			!strings.HasPrefix(field, "path:") && !strings.HasPrefix(field, "dir:") &&
			!strings.HasPrefix(field, "glob:") && !strings.HasPrefix(field, "regex:") &&
			!strings.HasPrefix(field, "type:") && !strings.HasPrefix(field, "case:") {
			continue
		}
		out = append(out, field)
		if len(out) >= 4 {
			break
		}
	}
	return strings.Join(out, " ")
}

func assertCompactOracleParity(t *testing.T, seed int64, idx *Index, vol *serviceVolumeIndex, opts queryOptions, countOnly bool) {
	t.Helper()
	full, err := searchCompactWithCache(idx, opts, countOnly, make(map[int]string), nil)
	if err != nil {
		t.Fatalf("seed=%d query=%q count=%v full search: %v", seed, opts.Query, countOnly, err)
	}
	trace := &searchTrace{}
	fastOpts := opts
	fastOpts.Trace = trace
	fast, err := searchCompactWithCache(idx, fastOpts, countOnly, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatalf("seed=%d query=%q count=%v fast search: %v trace=%+v", seed, opts.Query, countOnly, err, *trace)
	}
	if gotPaths, wantPaths := pathsOf(fast), pathsOf(full); !sameOrderedStrings(gotPaths, wantPaths) {
		t.Fatalf("seed=%d query=%q count=%v source=%s candidates=%d paths=%v want=%v", seed, opts.Query, countOnly, trace.Source, trace.Candidates, gotPaths, wantPaths)
	}
	assertTraceBudget(t, seed, trace, opts, len(fast), idx.compactRecordCount())
}

func assertTraceBudget(t *testing.T, seed int64, trace *searchTrace, opts queryOptions, resultCount int, recordCount int) {
	t.Helper()
	if trace == nil || trace.Source == "" {
		t.Fatalf("seed=%d query=%q missing trace source", seed, opts.Query)
	}
	if trace.Source == "compact-name-order-scan" {
		t.Fatalf("seed=%d query=%q routed to unbounded compact scan", seed, opts.Query)
	}
	switch trace.Source {
	case "legacy-planner", "path-dir-filter", "filter", "path-root-limited", "path-term-subtree", "regex-literal", "exact-dir", "exact-name", "name-prefix", "cached-multi-name-term", "multi-name-term", "name-term-posting":
		t.Fatalf("seed=%d query=%q routed behind bounded floor to %s", seed, opts.Query, trace.Source)
	}
	budget := recordCount
	if opts.Limit > 0 && resultCount < opts.Limit && resultCount < budget {
		budget = recordCount
	}
	if trace.Candidates > budget {
		t.Fatalf("seed=%d query=%q source=%s candidates=%d budget=%d", seed, opts.Query, trace.Source, trace.Candidates, budget)
	}
}

func assertQueryMetamorphicProperties(t *testing.T, seed int64, idx *Index, vol *serviceVolumeIndex, opts queryOptions) {
	t.Helper()
	if _, err := parseQuery(opts); err != nil {
		t.Fatalf("seed=%d generated invalid query %q: %v", seed, opts.Query, err)
	}
	if strings.Fields(opts.Query)[0] == "case:" {
		return
	}
	base, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatalf("seed=%d query=%q metamorphic base: %v", seed, opts.Query, err)
	}
	fields := strings.Fields(opts.Query)
	if len(fields) > 1 && !strings.Contains(opts.Query, "regex:") {
		permuted := opts
		rev := append([]string(nil), fields...)
		for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
			rev[i], rev[j] = rev[j], rev[i]
		}
		permuted.Query = strings.Join(rev, " ")
		got, err := searchCompactWithCache(idx, permuted, false, vol.pathCache, vol.nameTermCandidates)
		if err != nil {
			t.Fatalf("seed=%d query=%q permuted=%q: %v", seed, opts.Query, permuted.Query, err)
		}
		if !sameOrderedStrings(pathsOf(got), pathsOf(base)) {
			t.Fatalf("seed=%d query=%q permuted=%q paths=%v want=%v", seed, opts.Query, permuted.Query, pathsOf(got), pathsOf(base))
		}
	}
	if opts.Limit != 1 {
		small := opts
		small.Limit = 1
		large := opts
		large.Limit = maxInt(opts.Limit, 25)
		gotSmall, err := searchCompactWithCache(idx, small, false, vol.pathCache, vol.nameTermCandidates)
		if err != nil {
			t.Fatalf("seed=%d query=%q small limit: %v", seed, opts.Query, err)
		}
		gotLarge, err := searchCompactWithCache(idx, large, false, vol.pathCache, vol.nameTermCandidates)
		if err != nil {
			t.Fatalf("seed=%d query=%q large limit: %v", seed, opts.Query, err)
		}
		if len(gotSmall) > len(gotLarge) || !sameOrderedStrings(pathsOf(gotSmall), pathsOf(gotLarge)[:len(gotSmall)]) {
			t.Fatalf("seed=%d query=%q limit monotonic small=%v large=%v", seed, opts.Query, pathsOf(gotSmall), pathsOf(gotLarge))
		}
	}
	negative := opts
	negative.Query = strings.TrimSpace(negative.Query + " !no-such-term-xq9")
	gotNegative, err := searchCompactWithCache(idx, negative, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatalf("seed=%d query=%q negative identity: %v", seed, opts.Query, err)
	}
	if !sameOrderedStrings(pathsOf(gotNegative), pathsOf(base)) {
		t.Fatalf("seed=%d query=%q negative identity paths=%v want=%v", seed, opts.Query, pathsOf(gotNegative), pathsOf(base))
	}
}

func secondOracleSearch(idx *Index, opts queryOptions) ([]Entry, error) {
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, err
	}
	dropSatisfiedVolumeTerms(&pq, idx.Volume)
	limit := normalizedLimit(opts.Limit, false)
	pq.Limit = limit
	order := idx.CompactNameOrder
	if pq.RootBias != "" || pq.CWDBias != "" {
		order = idx.biasOrderCompact(order, firstNonEmpty(pq.CWDBias, pq.RootBias))
	}
	cache := make(map[int]string)
	out := make([]Entry, 0, min(limit, 1024))
	for pos := 0; pos < compactOrderLen(order, idx.compactRecordCount()); pos++ {
		id := compactOrderAt(order, pos)
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		path := idx.reconstructCompactPathCached(id, cache)
		entry := Entry{
			Path:        path,
			Name:        rec.Name,
			LowerPath:   strings.ToLower(path),
			LowerName:   strings.ToLower(rec.Name),
			Mode:        rec.Mode,
			Size:        rec.Size,
			ModUnix:     rec.ModUnix,
			IndexSource: idx.Source,
		}
		if secondOracleEntryMatches(entry, pq, pq.MatchPath) {
			out = append(out, entry)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func secondOracleEntryMatches(entry Entry, pq parsedQuery, matchPath bool) bool {
	path := filepath.Clean(entry.Path)
	name := entry.Name
	if name == "" {
		name = filepath.Base(path)
	}
	cmpPath, cmpName := path, name
	if !pq.CaseSensitive {
		cmpPath = strings.ToLower(cmpPath)
		cmpName = strings.ToLower(cmpName)
	}
	haystack := cmpName
	if matchPath {
		haystack = cmpPath
	}
	if pq.Under != "" && !secondOraclePathUnder(path, pq.Under) {
		return false
	}
	if pq.HasModAfter {
		if entry.ModUnix == 0 || !time.Unix(0, entry.ModUnix).After(pq.ModifiedAfter) {
			return false
		}
	}
	if pq.Type == "file" && entry.Mode&uint32(os.ModeDir) != 0 {
		return false
	}
	if pq.Type == "dir" && entry.Mode&uint32(os.ModeDir) == 0 {
		return false
	}
	for _, term := range pq.Terms {
		if !strings.Contains(haystack, term) {
			return false
		}
	}
	for _, ext := range pq.Exts {
		actual := strings.TrimPrefix(filepath.Ext(name), ".")
		if !pq.CaseSensitive {
			actual = strings.ToLower(actual)
		}
		if actual != ext {
			return false
		}
	}
	for _, dir := range pq.Dirs {
		if !strings.Contains(cmpPath, dir) {
			return false
		}
	}
	for _, glob := range pq.Globs {
		ok, err := filepath.Match(glob, cmpName)
		if err != nil || !ok {
			return false
		}
	}
	for _, re := range pq.Regexps {
		if !re.MatchString(path) {
			return false
		}
	}
	for _, sf := range pq.SizeFilters {
		if !sf.matches(entry.Size) {
			return false
		}
	}
	for _, df := range pq.DateFilters {
		if !df.matches(entry.ModUnix) {
			return false
		}
	}
	for _, group := range pq.OrGroups {
		ok := false
		for _, alt := range group {
			if secondOracleEntryMatches(entry, alt, matchPath || alt.MatchPath) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for _, neg := range pq.NotGroups {
		if secondOracleEntryMatches(entry, neg, matchPath || neg.MatchPath) {
			return false
		}
	}
	return true
}

func secondOraclePathUnder(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if strings.EqualFold(path, root) {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	return true
}

func parseQueryNoPanic(t *testing.T, opts queryOptions) (pq parsedQuery, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("parseQuery panicked for %q: %v", opts.Query, r)
		}
	}()
	return parseQuery(opts)
}

func renderParsedQueryForTest(pq parsedQuery) string {
	fields := make([]string, 0, len(pq.Terms)+len(pq.Exts)+len(pq.Dirs)+len(pq.Globs)+len(pq.NotGroups)+2)
	if pq.CaseSensitive {
		fields = append(fields, "case:")
	}
	if pq.Type != "" {
		fields = append(fields, "type:"+pq.Type)
	}
	for _, term := range pq.Terms {
		fields = append(fields, term)
	}
	for _, ext := range pq.Exts {
		fields = append(fields, "ext:"+ext)
	}
	for _, dir := range pq.Dirs {
		fields = append(fields, "dir:"+dir)
	}
	for _, glob := range pq.Globs {
		fields = append(fields, "glob:"+glob)
	}
	for _, term := range pq.RegexTerms {
		fields = append(fields, term)
	}
	for _, group := range pq.OrGroups {
		parts := make([]string, 0, len(group))
		for _, alt := range group {
			if rendered := renderParsedQueryForTest(alt); rendered != "" && !strings.Contains(rendered, " ") {
				parts = append(parts, rendered)
			}
		}
		if len(parts) > 1 {
			fields = append(fields, strings.Join(parts, "|"))
		}
	}
	for _, neg := range pq.NotGroups {
		if rendered := renderParsedQueryForTest(neg); rendered != "" && !strings.Contains(rendered, " ") {
			fields = append(fields, "!"+rendered)
		}
	}
	return strings.Join(fields, " ")
}

func parsedQuerySignature(pq parsedQuery) string {
	parts := []string{
		fmt.Sprintf("path=%v", pq.MatchPath),
		fmt.Sprintf("case=%v", pq.CaseSensitive),
		"type=" + pq.Type,
		"terms=" + strings.Join(sortedCopy(pq.Terms), ","),
		"ext=" + strings.Join(sortedCopy(pq.Exts), ","),
		"dirs=" + strings.Join(sortedCopy(pq.Dirs), ","),
		"globs=" + strings.Join(sortedCopy(pq.Globs), ","),
		"regexTerms=" + strings.Join(sortedCopy(pq.RegexTerms), ","),
	}
	for _, group := range pq.OrGroups {
		groupParts := make([]string, 0, len(group))
		for _, alt := range group {
			groupParts = append(groupParts, parsedQuerySignature(alt))
		}
		sort.Strings(groupParts)
		parts = append(parts, "or="+strings.Join(groupParts, "|"))
	}
	for _, neg := range pq.NotGroups {
		parts = append(parts, "not="+parsedQuerySignature(neg))
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func generatedLooseFuzzQueries() []string {
	seed := []string{
		"nrrd",
		"NRRD",
		".nrrd",
		".NRRD",
		"raw",
		"pdf",
		"pvsm",
		"F: nrrd",
		"F nrrd",
		"C: pvsm",
		"C pvsm",
		"F: nrrd !backup",
		"F: nrrd !backup !tmp",
		"nrrd raw",
		".nrrd .raw",
		"pvsm json",
		"pdf docx",
		"Users nrrd",
		"Users exampleuser",
		"exampleuser Users",
		"Users exampleuser Downloads",
		"Downloads exampleuser Users",
		"AppData pvsm",
		"Downloads raw",
		"Downloads docx",
		"workspace pdf",
		"nrrd Dataset trainingdata",
		"trainingdata Dataset nrrd",
		"Dataset trainingdata nrrd",
		"workspace trainingdata Dataset nrrd",
		"nrrd nrrd",
		"F: nrrd nrrd",
		"nrrd missing",
		"trainingdata missing nrrd",
		"zzzz nrrd",
		"nrrd|raw",
		"F: nrrd|raw",
		`workspace\nrrd-cache cache`,
		`workspace/dataset-000000.nrrd metadata`,
		"path: C: nrrd",
		"path:F: nrrd",
		"path:F: .nrrd",
		"path:C:.nrrd",
		"path:Downloads.nrrd",
	}
	terms := [][]string{
		{"trainingdata", "Dataset", "nrrd"},
		{"Dataset", "trainingdata", "nrrd"},
		{"workspace", "trainingdata", "Dataset", "nrrd"},
		{"workspace", "nrrd", "metadata", "json"},
		{"Downloads", "raw"},
		{"Downloads", "docx"},
		{"Users", "nrrd"},
		{"Users", "exampleuser"},
		{"Users", "exampleuser", "Downloads"},
		{"fixtureproj", "projects"},
	}
	suffixes := []string{"nrrd", ".nrrd", "raw", ".raw", "pdf", ".pdf", "pvsm", ".pvsm"}
	drives := []string{"", "C:", "F:", "C", "F"}
	negatives := []string{"", "!backup", "!metadata"}

	seen := make(map[string]struct{}, 256)
	out := make([]string, 0, 180)
	add := func(query string) {
		query = strings.Join(strings.Fields(query), " ")
		if query == "" {
			return
		}
		if _, ok := seen[query]; ok {
			return
		}
		seen[query] = struct{}{}
		out = append(out, query)
	}
	for _, query := range seed {
		add(query)
	}
	for _, drive := range drives {
		for _, suffix := range suffixes {
			for _, neg := range negatives {
				add(strings.Join(nonEmptyStrings(drive, suffix, neg), " "))
			}
		}
	}
	for _, parts := range terms {
		for _, neg := range negatives {
			add(strings.Join(nonEmptyStrings(strings.Join(parts, " "), neg), " "))
			rev := append([]string(nil), parts...)
			for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
				rev[i], rev[j] = rev[j], rev[i]
			}
			add(strings.Join(nonEmptyStrings(strings.Join(rev, " "), neg), " "))
		}
	}
	if len(out) > 160 {
		out = out[:160]
	}
	return out
}

func looseFuzzMatchPath(query string) bool {
	fields := strings.Fields(query)
	plain := 0
	for _, field := range fields {
		raw := strings.TrimLeft(field, "!-")
		if raw == "" {
			continue
		}
		if isVolumeQueryTerm(raw) || strings.ContainsAny(raw, `\/`) {
			return true
		}
		key, value, hasPrefix := strings.Cut(raw, ":")
		if hasPrefix {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "path", "fullpath", "full-path", "full_path", "fullpathname", "full-path-name", "location":
				return true
			case "ext", "extension", "glob", "regex", "size", "sz", "dm", "date", "date-modified", "datemodified", "modified", "type", "case":
				continue
			}
			if value != "" {
				plain++
			}
			continue
		}
		plain++
	}
	return plain >= 2
}

func TestServiceVolumesHighFanoutMultiPartPathQueries(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	cIdx := highExtensionFanoutPathIndex(2500)
	fIdx := highExtensionFanoutPathIndex(2500)
	fIdx.Volume = "F:"
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("c-high-fanout.gsi", cIdx),
		newServiceVolumeIndex("f-high-fanout.gsi", fIdx),
	}
	for _, vol := range volumes {
		vol.rebuildNameTrigramsLocked()
	}
	queries := []queryOptions{
		{Query: "path:C: trainingdata Dataset .nrrd", Limit: 20},
		{Query: "path:F: trainingdata Dataset .nrrd", Limit: 20},
		{Query: "path:trainingdata Dataset .nrrd", Limit: 20},
		{Query: "path:C: absent Dataset .nrrd", Limit: 20},
		{Query: "path:F: absent Dataset .nrrd", Limit: 20},
	}
	for _, opts := range queries {
		t.Run(opts.Query, func(t *testing.T) {
			got, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatalf("service search: %v", err)
			}
			var full []Entry
			for _, vol := range volumes {
				if selected, err := serviceVolumesForQuery([]*serviceVolumeIndex{vol}, opts); err != nil || len(selected) == 0 {
					continue
				}
				matches, err := searchCompactWithCache(vol.index, opts, false, make(map[int]string), nil)
				if err != nil {
					t.Fatalf("full search %s: %v", vol.volume, err)
				}
				full = append(full, matches...)
			}
			if len(full) > opts.Limit {
				full = full[:opts.Limit]
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(full); !sameOrderedStrings(gotPaths, wantPaths) {
				t.Fatalf("service paths = %v, full paths = %v", gotPaths, wantPaths)
			}
			for _, entry := range got {
				if strings.Contains(opts.Query, "path:C:") && !strings.HasPrefix(strings.ToUpper(entry.Path), `C:\`) {
					t.Fatalf("C:-scoped query returned %q", entry.Path)
				}
				if strings.Contains(opts.Query, "path:F:") && !strings.HasPrefix(strings.ToUpper(entry.Path), `F:\`) {
					t.Fatalf("F:-scoped query returned %q", entry.Path)
				}
			}
		})
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
		{Query: "path:trainingdata Dataset .nrrd", Limit: 25},
		{Query: "path:Dataset trainingdata .nrrd", Limit: 25},
		{Query: "path:workspace trainingdata Dataset .nrrd", Limit: 25},
		{Query: "path:trainingdata Dataset ext:nrrd", Limit: 25},
		{Query: "path:trainingdata Dataset glob:*.nrrd", Limit: 25},
		{Query: "path:trainingdata missing .nrrd", Limit: 25},
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
		vol.rebuildNameTrigramsLocked()
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

func TestNameTrigramPathCandidatesIncludeDirectoryDescendants(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	opts := queryOptions{Query: "path:C: .opencode", Limit: 20}
	trigram, ok := vol.nameTrigramCandidates(mustParseQuery(t, opts))
	if !ok {
		t.Fatal("nameTrigramCandidates declined path dotted substring query")
	}
	gotNames := namesOf(entriesForIDs(idx, trigram))
	if !sameStringSet(gotNames, []string{"ai.opencode.desktop", "settings.json"}) {
		t.Fatalf("trigram candidate names = %v, want directory and descendant", gotNames)
	}
	fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := namesOf(fast), namesOf(full); !sameStringSet(got, want) {
		t.Fatalf("fast names = %v, full names = %v", got, want)
	}
}

func TestLimitedPathComponentPostingMatchesFullSearch(t *testing.T) {
	idx := dottedPathBenchmarkIndex(25000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	opts := queryOptions{Query: "path:C: .opencode", Limit: 20}
	pq := mustParseQuery(t, opts)
	dropSatisfiedVolumeTerms(&pq, idx.Volume)
	pq.Limit = normalizedLimit(opts.Limit, false)

	limited, ok := vol.pathPlanTermPostingLimited(".opencode", pq)
	if !ok {
		t.Fatal("pathPlanTermPostingLimited declined selective component query")
	}
	if len(limited) == 0 || len(limited) > opts.Limit {
		t.Fatalf("limited candidates = %d, want 1..%d", len(limited), opts.Limit)
	}
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok || len(plan.sources) != 1 ||
		(!strings.HasPrefix(plan.sources[0].name, "term-limited:") && !strings.HasPrefix(plan.sources[0].name, "path-term:")) {
		t.Fatalf("plan = %+v, ok=%v, want bounded path source", plan, ok)
	}
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatalf("full search: %v", err)
	}
	fast, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
	if err != nil {
		t.Fatalf("service search: %v", err)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("limited paths = %v, full paths = %v", got, want)
	}
}

func TestComponentMultiTermTopReturnsCompleteUnderLimitResults(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	idx := fixtureprojTrainingdataFixture(167)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.buildCompactChildren()
	vol.buildSubtreeRanges()
	for _, opts := range []queryOptions{
		{Query: "path:F: fixtureproj-dev-ff trainingdata", Limit: 200},
		{Query: `path:F:\fixtureproj trainingdata`, Limit: 20},
	} {
		t.Run(opts.Query, func(t *testing.T) {
			trace := &searchTrace{}
			fast, err := searchCompactWithCache(idx, queryOptions{Query: opts.Query, Limit: opts.Limit, MatchPath: true, Trace: trace}, false, vol.pathCache, vol.nameTermCandidates)
			if err != nil {
				t.Fatal(err)
			}
			full, err := searchCompactWithCache(idx, queryOptions{Query: opts.Query, Limit: opts.Limit, MatchPath: true}, false, make(map[int]string), nil)
			if err != nil {
				t.Fatal(err)
			}
			if trace.Source != "path-component-multi-top" {
				t.Fatalf("source = %q, want path-component-multi-top", trace.Source)
			}
			if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
				t.Fatalf("paths = %v, want %v", got, want)
			}
			if opts.Limit > 167 && len(fast) <= 167 {
				t.Fatalf("result count = %d, want fixture files plus matching directories", len(fast))
			}
		})
	}
}

func TestBroadDownloadsPathQueriesUseBoundedDirectoryTop(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	idx := broadDownloadsMarkdownFixture(35_000, 800, true)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.buildCompactChildren()
	vol.buildSubtreeRanges()
	vol.rebuildNameTrigramsLocked()
	for _, opts := range []queryOptions{
		{Query: "path:Downloads", Limit: 25},
		{Query: "path:Downloads ext:md", Limit: 25},
		{Query: "Downloads md", MatchPath: true, Limit: 25},
	} {
		t.Run(opts.Query, func(t *testing.T) {
			trace := &searchTrace{}
			fast, err := searchCompactWithCache(idx, queryOptions{Query: opts.Query, MatchPath: opts.MatchPath, Limit: opts.Limit, Trace: trace}, false, vol.pathCache, vol.nameTermCandidates)
			if err != nil {
				t.Fatal(err)
			}
			full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
				t.Fatalf("paths = %v, want %v", got, want)
			}
			switch opts.Query {
			case "path:Downloads":
				if trace.Source != "path-directory-term-top" && trace.Source != "path-component-direct-top" && trace.Source != "path-component-root-top" {
					t.Fatalf("source = %q, want bounded directory/component source", trace.Source)
				}
				if len(fast) != opts.Limit {
					t.Fatalf("result count = %d, want %d", len(fast), opts.Limit)
				}
			case "path:Downloads ext:md":
				if trace.Source != "path-directory-term-top" {
					t.Fatalf("source = %q, want path-directory-term-top", trace.Source)
				}
				if len(fast) != opts.Limit {
					t.Fatalf("result count = %d, want %d", len(fast), opts.Limit)
				}
			default:
				if len(fast) != opts.Limit {
					t.Fatalf("result count = %d, want %d", len(fast), opts.Limit)
				}
			}
		})
	}
}

func TestBareExtensionPathQueryUsesRealAnchorWhenAnchorLooksExtensionShaped(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	idx := clientDvarrayFixture(30_000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.buildCompactChildren()
	vol.buildSubtreeRanges()
	vol.rebuildNameTrigramsLocked()
	opts := queryOptions{Query: "path:F: client dvarray", MatchPath: true, Limit: 20}
	trace := &searchTrace{}
	fast, err := searchCompactWithCache(idx, queryOptions{Query: opts.Query, MatchPath: opts.MatchPath, Limit: opts.Limit, Trace: trace}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if trace.Source != "path-bare-extension-multi-top" {
		t.Fatalf("source = %q, want path-bare-extension-multi-top", trace.Source)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	if got := pathsOf(fast); !sameOrderedStrings(got, []string{`F:\Example Sync\Analysis\Projects\Example Client\PROJECT-2024-07-WELL-001\ml\876955601027075-top-thickness-ml-fixed.dvarray`}) {
		t.Fatalf("paths = %v, want example client dvarray hit", got)
	}
	if trace.Candidates > 128 {
		t.Fatalf("candidates = %d, want bounded anchor set", trace.Candidates)
	}
	pq := mustParseQuery(t, opts)
	dropSatisfiedVolumeTerms(&pq, idx.Volume)
	candidates, ok := vol.extPostingPathTermCandidates("dvarray", pq.Terms, opts.Limit)
	if !ok {
		t.Fatalf("extPostingPathTermCandidates declined %d dvarray postings; want bounded verifier route", len(vol.extPosting("dvarray")))
	}
	if got := pathsOf(entriesForIDs(idx, candidates)); !sameOrderedStrings(got, []string{`F:\Example Sync\Analysis\Projects\Example Client\PROJECT-2024-07-WELL-001\ml\876955601027075-top-thickness-ml-fixed.dvarray`}) {
		t.Fatalf("extension-posting verifier paths = %v, want example client dvarray hit", got)
	}
	countPQ := mustParseQuery(t, opts)
	countPQ.CountOnly = true
	count, ok := vol.fastPostingCount(countPQ)
	if !ok {
		t.Fatalf("fastPostingCount declined bare extension path query")
	}
	if count != 1 {
		t.Fatalf("fastPostingCount = %d, want 1", count)
	}
}

func TestComponentTrigramDeclinesBroadDirectoryExpansion(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := broadComponentExpansionIndex(serviceComponentTrigramExpansionMaxIDs + 500)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	opts := queryOptions{Query: "path:C: workspace", Limit: 20}
	pq := mustParseQuery(t, opts)
	if candidates, ok := vol.componentTrigramCandidates(pq); ok {
		t.Fatalf("componentTrigramCandidates returned %d broad workspace candidates, want fallback", len(candidates))
	}
	fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("fast paths = %v, full paths = %v", got, want)
	}
}

func TestLargePlainComponentRootUsesBoundedTopSource(t *testing.T) {
	idx := broadComponentExpansionIndex(100_100)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	opts := queryOptions{Query: "path:C: workspace", Limit: 20}
	pq := mustParseQuery(t, opts)
	dropSatisfiedVolumeTerms(&pq, idx.Volume)
	pq.Limit = opts.Limit

	candidates, ok := vol.componentRootTopCandidates(pq)
	if !ok {
		t.Fatal("componentRootTopCandidates declined large plain component root")
	}
	if len(candidates) != opts.Limit {
		t.Fatalf("candidate count = %d, want %d", len(candidates), opts.Limit)
	}
	if names := namesOf(entriesForIDs(idx, candidates)); !containsString(names, "workspace") {
		t.Fatalf("candidate names = %v, want workspace root included", names)
	}
}

func TestLowMemoryDeterministicQueryMatrixUnderTarget(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	cIdx := dottedPathBenchmarkIndex(100_000)
	fIdx := dottedPathBenchmarkIndex(100_000)
	fIdx.Volume = "F:"
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("lowmem-c.gsi", cIdx),
		newServiceVolumeIndex("lowmem-f.gsi", fIdx),
	}
	for _, vol := range volumes {
		vol.rebuildNameTrigramsLocked()
	}
	queries := []string{
		".nrrd",
		".raw",
		".pdf",
		".json",
		".dll",
		".exe",
		".opencode",
		"path:F: .nrrd",
		"path:F: nrrd",
		"path:F: .raw",
		"path:F: raw",
		"path:F: .pdf",
		"path:F: pdf",
		"path:C: pvsm",
		"path:C: .pvsm",
		"path:C: .exe",
		"path:C: Users",
		"path:C: AppData",
		"path:F: workspace",
		"path:Windows",
		"path:node_modules",
		"path:Downloads .nrrd",
		"path:F: Downloads .nrrd",
		"path:C: trainingdata Dataset .nrrd",
		"path:F: trainingdata Dataset .nrrd",
		"path:C: workspace trainingdata Dataset .nrrd",
		"path:F: workspace trainingdata Dataset .nrrd",
		"path:C: trainingdata Dataset ext:nrrd",
		"path:C: trainingdata Dataset glob:*.nrrd",
		"path:C: missing Dataset .nrrd",
		"path:F: missing Dataset .nrrd",
		"path:C:.nrrd",
		"zzzz-no-hit-seekfs",
	}
	const iterations = 5
	all := make([]float64, 0, len(queries)*iterations)
	perQuery := make(map[string][]float64, len(queries))
	for i := 0; i < iterations; i++ {
		for _, query := range queries {
			start := time.Now()
			_, err := searchServiceVolumes(volumes, queryOptions{Query: query, Limit: 100}, false)
			if err != nil {
				t.Fatalf("query %q failed: %v", query, err)
			}
			ms := float64(time.Since(start).Microseconds()) / 1000
			all = append(all, ms)
			perQuery[query] = append(perQuery[query], ms)
		}
	}
	if p95 := percentile(append([]float64(nil), all...), 0.95); p95 > 100 {
		if envBool("SEEKFS_ENFORCE_LATENCY_TESTS") {
			t.Fatalf("lowmem deterministic query matrix p95 = %.3fms, want <= 100ms; per-query=%v", p95, perQuery)
		}
		t.Logf("lowmem deterministic query matrix p95 = %.3fms over soft 100ms budget; per-query=%v", p95, perQuery)
	}
}

func TestNameTrigramRecentOverlayFindsCreateWithMissingBaseGram(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)

	id := idx.appendCompactRecord(CompactRecord{FRN: 30, ParentFRN: 2, Parent: 1, Name: "zzquark-note.txt", Size: 1, ModUnix: time.Now().UnixNano()})
	vol.addFRNID(30, id)
	vol.addExactName(id)
	vol.markNameTrigramRecent(id)

	opts := queryOptions{Query: "path:C: zzquark", Limit: 20}
	fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(fast); !sameStringSet(names, []string{"zzquark-note.txt"}) {
		t.Fatalf("recent create names = %v, want zzquark-note.txt", names)
	}
}

func TestNameTrigramPathPostingCachesAndKeepsRecentOverlay(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)

	first, ok := vol.nameTrigramPathTermPosting(".opencode")
	if !ok || len(first) == 0 {
		t.Fatalf("first trigram path posting = %v, %v; want candidates", first, ok)
	}
	cacheKey := "\x00trigrampath:.opencode"
	if vol.pathTermCache == nil || len(vol.pathTermCache[cacheKey].ids) == 0 {
		t.Fatal("trigram path posting was not cached")
	}

	dirID := 10 // ai.opencode.desktop in pathSyntaxFixture.
	id := idx.appendCompactRecord(CompactRecord{FRN: 40, ParentFRN: 10, Parent: int32(dirID), Name: "new-child.txt", Size: 1, ModUnix: time.Now().UnixNano()})
	vol.addFRNID(40, id)
	vol.addChild(10, id)
	vol.markNameTrigramRecent(id)
	if vol.recentIDs == nil {
		vol.recentIDs = make(map[int]struct{})
	}
	vol.recentIDs[id] = struct{}{}
	vol.recentSeq++

	second, ok := vol.nameTrigramPathTermPosting(".opencode")
	if !ok {
		t.Fatal("cached trigram path posting declined after recent update")
	}
	found := false
	for _, gotID := range second {
		if gotID == id {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("cached trigram path posting = %v, want recent descendant id %d", second, id)
	}
}

func TestFilenameTrigramCandidatesMatchFullSearch(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	opts := queryOptions{Query: ".opencode", Limit: 20}
	candidates, ok := vol.filenameTrigramCandidates(mustParseQuery(t, opts))
	if !ok {
		t.Fatal("filenameTrigramCandidates declined selective filename query")
	}
	if names := namesOf(entriesForIDs(idx, candidates)); !sameStringSet(names, []string{"ai.opencode.desktop"}) {
		t.Fatalf("filename trigram candidates = %v, want ai.opencode.desktop", names)
	}
	fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := namesOf(fast), namesOf(full); !sameStringSet(got, want) {
		t.Fatalf("fast names = %v, full names = %v", got, want)
	}
}

func TestFilenameTrigramRecentOverlayFindsMissingBaseGram(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	id := idx.appendCompactRecord(CompactRecord{FRN: 30, ParentFRN: 2, Parent: 1, Name: "zzquark-note.txt"})
	vol.addFRNID(30, id)
	vol.addExactName(id)
	vol.markNameTrigramRecent(id)

	opts := queryOptions{Query: "zzquark", Limit: 20}
	fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(fast); !sameStringSet(names, []string{"zzquark-note.txt"}) {
		t.Fatalf("recent filename names = %v, want zzquark-note.txt", names)
	}
}

func TestFilenameTrigramDeclinesUnicodeNameTerm(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	records := []CompactRecord{{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}}
	for i := 0; i < 30_000; i++ {
		name := fmt.Sprintf("plain-%05d.txt", i)
		if i == 20_000 {
			name = "Übersicht.pdf"
		}
		records = append(records, CompactRecord{
			FRN:       uint64(200 + i),
			ParentFRN: 100,
			Parent:    0,
			Name:      name,
		})
	}
	idx := &Index{Source: "usn", Volume: "F:", Compact: true, Records: records}
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	opts := queryOptions{Query: "übersicht", Limit: 10}
	if candidates, ok := vol.filenameTrigramCandidates(mustParseQuery(t, opts)); ok {
		t.Fatalf("filenameTrigramCandidates returned %d unicode candidates, want scan fallback", len(candidates))
	}
	trace := &searchTrace{}
	fast, err := searchCompactWithCache(idx, queryOptions{Query: "übersicht", Limit: 10, Trace: trace}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("unicode name query paths=%v want=%v source=%s", got, want, trace.Source)
	}
	if got := pathsOf(fast); !sameOrderedStrings(got, []string{`F:\Übersicht.pdf`}) {
		t.Fatalf("unicode name query paths=%v, want Übersicht.pdf source=%s", got, trace.Source)
	}
	if trace.Source == "name-trigram" {
		t.Fatalf("unicode name query used trigram source")
	}
}

func TestFilenameTrigramDeclinesCommonTerm(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := dottedPathBenchmarkIndex(100_000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	if candidates, ok := vol.filenameTrigramCandidates(mustParseQuery(t, queryOptions{Query: "plain", Limit: 20})); ok {
		t.Fatalf("filenameTrigramCandidates returned %d common candidates, want fallback", len(candidates))
	}
}

func TestNameTrigramRecentOverlayFindsRenamedDirectoryDescendants(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)

	dirID := 10 // ai.opencode.desktop in pathSyntaxFixture.
	rec := idx.compactRecord(dirID)
	vol.removeExactName(dirID)
	rec.Name = "zzquark-folder"
	idx.setCompactRecord(dirID, rec)
	vol.addExactName(dirID)
	vol.markNameTrigramRecent(dirID)

	opts := queryOptions{Query: "path:C: zzquark", Limit: 20}
	fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(fast); !sameStringSet(names, []string{"settings.json", "zzquark-folder"}) {
		t.Fatalf("recent renamed directory names = %v, want directory and descendant", names)
	}
}

func TestNameTrigramRecentOverlayExcludesDeletedBaseMatch(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := pathSyntaxFixture()
	id := idx.appendCompactRecord(CompactRecord{FRN: 30, ParentFRN: 2, Parent: 1, Name: "zzstable.txt", Size: 1, ModUnix: time.Now().UnixNano()})
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)

	rec := idx.compactRecord(id)
	rec.Deleted = true
	idx.setCompactRecord(id, rec)
	vol.markNameTrigramRecent(id)

	opts := queryOptions{Query: "path:C: zzstable", Limit: 20}
	fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(fast); len(names) != 0 {
		t.Fatalf("deleted base match names = %v, want none", names)
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
	if !sameStringSet(sourceNames, []string{"ext:raw"}) && !sameStringSet(sourceNames, []string{"ext:raw", "path-term:downloads"}) {
		t.Fatalf("plan sources = %v, want ext:raw plus optional bounded Downloads path source", sourceNames)
	}
	got, err := searchCompactWithCache(idx, queryOptions{Query: "Downloads ext:raw", MatchPath: true, Limit: 20}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(got); !sameStringSet(names, []string{"camera-001.raw", "camera-002.raw"}) {
		t.Fatalf("names = %v, want Downloads raw files only", names)
	}
}

func TestDropSatisfiedVolumeTermsOnlyForMatchingVolume(t *testing.T) {
	pq := mustParseQuery(t, queryOptions{Query: "path:F: .pdf", Limit: 20})
	dropSatisfiedVolumeTerms(&pq, "F:")
	if len(pq.Terms) != 0 {
		t.Fatalf("terms after matching F: drop = %v, want no terms", pq.Terms)
	}
	if len(pq.Exts) != 1 || pq.Exts[0] != "pdf" {
		t.Fatalf("exts after matching F: drop = %v, want [pdf]", pq.Exts)
	}
	if !compactCandidateCanSkipEntryMatches(pq, true) {
		t.Fatal("matching volume ext-only path query should skip full entryMatches after candidate selection")
	}

	pq = mustParseQuery(t, queryOptions{Query: "path:F: .pdf", Limit: 20})
	dropSatisfiedVolumeTerms(&pq, "C:")
	if len(pq.Terms) != 1 || pq.Terms[0] != "f:" {
		t.Fatalf("terms after mismatched C: drop = %v, want [f:]", pq.Terms)
	}
	if compactCandidateCanSkipEntryMatches(pq, true) {
		t.Fatal("mismatched volume term must still require full entryMatches")
	}

	pq = mustParseQuery(t, queryOptions{Query: "Downloads ext:raw", MatchPath: true, Limit: 20})
	if compactCandidateCanSkipEntryMatches(pq, true) {
		t.Fatal("path term query must not skip full entryMatches")
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
		{Query: "nrrd", Limit: 20},
		{Query: "raw", Limit: 20},
		{Query: "pdf", Limit: 20},
		{Query: "pvsm", Limit: 20},
		{Query: "F: nrrd", Limit: 20},
		{Query: "F: raw", Limit: 20},
		{Query: "F: pdf", Limit: 20},
		{Query: "C: pvsm", Limit: 20},
		{Query: "trainingdata Dataset nrrd", MatchPath: true, Limit: 20},
		{Query: "Dataset trainingdata nrrd", MatchPath: true, Limit: 20},
		{Query: "path:nrrd", Limit: 20},
		{Query: "path:C: nrrd", Limit: 20},
		{Query: "path:F: .nrrd", Limit: 20},
		{Query: "path:F: nrrd", Limit: 20},
		{Query: "path:F: .raw", Limit: 20},
		{Query: "path:F: raw", Limit: 20},
		{Query: "path:F: .pdf", Limit: 20},
		{Query: "path:F: pdf", Limit: 20},
		{Query: "path:C: pvsm", Limit: 20},
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
	trainingdata := add(5, 2, workspace, "trainingdata", uint32(os.ModeDir))
	dataset := add(6, 5, trainingdata, "Dataset", uint32(os.ModeDir))
	add(7, 6, dataset, "sample-volume.nrrd", 0)
	add(8, 6, dataset, "sample-labels.raw", 0)
	otherTrainingdata := add(9, 5, trainingdata, "control", uint32(os.ModeDir))
	add(10, 9, otherTrainingdata, "control-volume.nrrd", 0)
	datasetElsewhere := add(11, 2, workspace, "Dataset-archive", uint32(os.ModeDir))
	add(12, 11, datasetElsewhere, "archive-volume.nrrd", 0)
	users := add(13, 1, root, "Users", uint32(os.ModeDir))
	user := add(14, 13, users, "exampleuser", uint32(os.ModeDir))
	downloads := add(15, 14, user, "Downloads", uint32(os.ModeDir))
	add(16, 15, downloads, "Project Specification - v1.docx", 0)
	add(17, 15, downloads, "Project Specification - v1.2.docx", 0)
	add(18, 15, downloads, "labels-cleaned.nrrd", 0)
	add(19, 15, downloads, "filtered-volume-cleaned.nrrd", 0)
	fixtureproj := add(20, 1, root, "fixtureproj-dev-ff", uint32(os.ModeDir))
	fixtureprojProject := add(21, 20, fixtureproj, "projects", uint32(os.ModeDir))
	add(22, 21, fixtureprojProject, "best_model_1754265744.pth", 0)
	nextFRN := uint64(30)
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
		case i%127 == 0:
			name = fmt.Sprintf("state-%06d.pvsm", i)
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

func highExtensionFanoutPathIndex(n int) *Index {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: make([]CompactRecord, 0, n+16),
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
	bulk := add(3, 2, workspace, "bulk", uint32(os.ModeDir))
	trainingdata := add(4, 2, workspace, "trainingdata", uint32(os.ModeDir))
	dataset := add(5, 4, trainingdata, "Dataset", uint32(os.ModeDir))
	add(6, 5, dataset, "target-volume.nrrd", 0)
	add(7, 5, dataset, "target-metadata.json", 0)
	otherTrainingdata := add(8, 4, trainingdata, "control", uint32(os.ModeDir))
	add(9, 8, otherTrainingdata, "control-volume.nrrd", 0)
	nextFRN := uint64(20)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("unrelated-%06d.nrrd", i)
		if i%211 == 0 {
			name = fmt.Sprintf("backup-%06d.nrrd", i)
		}
		add(nextFRN, 3, bulk, name, 0)
		nextFRN++
	}
	buildOrders(idx)
	return idx
}

func nonEmptyStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
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

func broadComponentExpansionIndex(children int) *Index {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: make([]CompactRecord, 0, children+2),
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
	for i := 0; i < children; i++ {
		add(nextFRN, 2, workspace, fmt.Sprintf("file-%06d.txt", i), 0)
		nextFRN++
	}
	buildOrders(idx)
	return idx
}

func broadDownloadsMarkdownFixture(downloadChildren, markdownElsewhere int, markdownInDownloads bool) *Index {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: make([]CompactRecord, 0, downloadChildren+markdownElsewhere+8),
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
	users := add(2, 1, root, "Users", uint32(os.ModeDir))
	user := add(3, 2, users, "exampleuser", uint32(os.ModeDir))
	downloads := add(4, 3, user, "Downloads", uint32(os.ModeDir))
	archive := add(5, 1, root, "markdown-archive", uint32(os.ModeDir))
	nextFRN := uint64(10)
	for i := 0; i < downloadChildren; i++ {
		name := fmt.Sprintf("download-file-%06d.bin", i)
		if markdownInDownloads && i%97 == 0 {
			name = fmt.Sprintf("download-note-%06d.md", i)
		}
		add(nextFRN, 4, downloads, name, 0)
		nextFRN++
	}
	for i := 0; i < markdownElsewhere; i++ {
		add(nextFRN, 5, archive, fmt.Sprintf("note-%06d.md", i), 0)
		nextFRN++
	}
	buildOrders(idx)
	return idx
}

func clientDvarrayFixture(otherDvarray int) *Index {
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: make([]CompactRecord, 0, otherDvarray+16),
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
	syncRoot := add(2, 1, root, "Example Sync", uint32(os.ModeDir))
	analysis := add(3, 2, syncRoot, "Analysis", uint32(os.ModeDir))
	projects := add(4, 3, analysis, "Projects", uint32(os.ModeDir))
	client := add(5, 4, projects, "Example Client", uint32(os.ModeDir))
	well := add(6, 5, client, "PROJECT-2024-07-WELL-001", uint32(os.ModeDir))
	ml := add(7, 6, well, "ml", uint32(os.ModeDir))
	add(8, 7, ml, "876955601027075-top-thickness-ml-fixed.dvarray", 0)
	other := add(9, 1, root, "other-dvarray", uint32(os.ModeDir))
	nextFRN := uint64(10)
	for i := 0; i < otherDvarray; i++ {
		add(nextFRN, 9, other, fmt.Sprintf("other-%06d.dvarray", i), 0)
		nextFRN++
	}
	buildOrders(idx)
	return idx
}

func fixtureprojTrainingdataFixture(matches int) *Index {
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: make([]CompactRecord, 0, matches+1008),
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
	fixtureproj := add(2, 1, root, "fixtureproj-dev-ff", uint32(os.ModeDir))
	projects := add(3, 2, fixtureproj, "projects", uint32(os.ModeDir))
	model := add(4, 3, projects, "model", uint32(os.ModeDir))
	trainingdata := add(5, 4, model, "trainingdata", uint32(os.ModeDir))
	rawFiles := add(6, 5, trainingdata, "raw files", uint32(os.ModeDir))
	for i := 0; i < matches; i++ {
		add(uint64(1000+i), 6, rawFiles, fmt.Sprintf("volume-%03d.nrrd", i), 0)
	}
	for i := 0; i < 1000; i++ {
		add(uint64(10_000+i), 1, root, fmt.Sprintf("other-%03d.txt", i), 0)
	}
	buildOrders(idx)
	return idx
}

func manyDirectNameMatchIndex(term string, matches int) *Index {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: make([]CompactRecord, 0, matches+2),
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
	for i := 0; i < matches; i++ {
		add(nextFRN, 2, workspace, fmt.Sprintf("%s-%06d.txt", term, i), 0)
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

func entriesForIDs(idx *Index, ids []int) []Entry {
	entries := make([]Entry, 0, len(ids))
	cache := make(map[int]string)
	for _, id := range ids {
		if id < 0 || id >= idx.compactRecordCount() {
			continue
		}
		rec := idx.compactRecord(id)
		entries = append(entries, Entry{
			Path: idx.reconstructCompactPathCached(id, cache),
			Name: rec.Name,
			Mode: rec.Mode,
		})
	}
	return entries
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
