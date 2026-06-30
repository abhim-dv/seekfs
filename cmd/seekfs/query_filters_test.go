package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseQueryInfersPathModeForPathSeparators(t *testing.T) {
	pq, err := parseQuery(queryOptions{Query: `reaper_base_new_workspaces_since_20260619\clean_surface_dead_time_outlier_metrics_since_20260619.json`})
	if err != nil {
		t.Fatal(err)
	}
	if !pq.MatchPath {
		t.Fatal("query with path separator did not enable path matching")
	}
	wantTerms := []string{"reaper_base_new_workspaces_since_20260619", "clean_surface_dead_time_outlier_metrics_since_20260619.json"}
	if !sameStringSet(pq.Terms, wantTerms) {
		t.Fatalf("terms = %v, want %v", pq.Terms, wantTerms)
	}
}

func TestParseSizeFilter(t *testing.T) {
	cases := []struct {
		spec    string
		op      string
		bytes   int64
		wantErr bool
	}{
		{">100mb", ">", 100 << 20, false},
		{">=1gb", ">=", 1 << 30, false},
		{"<4k", "<", 4 << 10, false},
		{"<=512", "<=", 512, false},
		{"1024", "=", 1024, false},
		{"=2048b", "=", 2048, false},
		{"10m", ">", 0, false}, // op defaults to "=" but value parses
		{"", "", 0, true},
		{">notanumber", "", 0, true},
	}
	for _, c := range cases {
		sf, err := parseSizeFilter(c.spec)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseSizeFilter(%q) expected error", c.spec)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSizeFilter(%q) unexpected error: %v", c.spec, err)
			continue
		}
		if c.spec != "10m" {
			if sf.op != c.op || sf.bytes != c.bytes {
				t.Errorf("parseSizeFilter(%q) = {%s %d}, want {%s %d}", c.spec, sf.op, sf.bytes, c.op, c.bytes)
			}
		}
	}
}

func TestSizeFilterMatches(t *testing.T) {
	sf := sizeFilter{op: ">", bytes: 100}
	if !sf.matches(101) || sf.matches(100) || sf.matches(99) {
		t.Fatal("size > 100 matched incorrectly")
	}
	sf = sizeFilter{op: "<=", bytes: 100}
	if !sf.matches(100) || !sf.matches(50) || sf.matches(101) {
		t.Fatal("size <= 100 matched incorrectly")
	}
}

func TestParseDateFilterRelativeAndMacros(t *testing.T) {
	for _, spec := range []string{"today", "yesterday", "thisweek", "lastweek", "24h", "7d", "2026-05-01"} {
		if _, err := parseDateFilter(spec); err != nil {
			t.Errorf("parseDateFilter(%q) unexpected error: %v", spec, err)
		}
	}
	if _, err := parseDateFilter("notadate"); err == nil {
		t.Error("parseDateFilter(notadate) expected error")
	}
}

func TestDateFilterMatchesToday(t *testing.T) {
	df, err := parseDateFilter("today")
	if err != nil {
		t.Fatal(err)
	}
	if !df.matches(time.Now().UnixNano()) {
		t.Fatal("dm:today did not match a record modified now")
	}
	if df.matches(time.Now().AddDate(0, 0, -2).UnixNano()) {
		t.Fatal("dm:today matched a record from two days ago")
	}
	if df.matches(0) {
		t.Fatal("dm:today matched a record with zero mtime")
	}
}

func TestUnknownFilterIsRejected(t *testing.T) {
	for _, q := range []string{"size2:>1mb", "attrib:H", "parent:foo", "color:red"} {
		if _, err := parseQuery(queryOptions{Query: q}); err == nil {
			t.Errorf("parseQuery(%q) should reject unknown filter, got nil error", q)
		}
	}
}

func TestKnownFiltersAndPathsAreNotRejected(t *testing.T) {
	// Drive-letter paths and supported filters must still parse.
	for _, q := range []string{`c:\windows main`, "ext:go", "size:>1mb", "dm:today", "type:file"} {
		if _, err := parseQuery(queryOptions{Query: q, MatchPath: true}); err != nil {
			t.Errorf("parseQuery(%q) unexpectedly rejected: %v", q, err)
		}
	}
}

func TestPathFilterEnablesPathMatching(t *testing.T) {
	pq, err := parseQuery(queryOptions{Query: "path:Downloads"})
	if err != nil {
		t.Fatalf("parseQuery: %v", err)
	}
	if !pq.MatchPath {
		t.Fatal("path: filter did not enable path matching")
	}
	if len(pq.Terms) != 1 || pq.Terms[0] != "downloads" {
		t.Fatalf("terms = %v, want [downloads]", pq.Terms)
	}
}

func TestPathExtensionSyntaxMatrixMatchesAcrossSearchPaths(t *testing.T) {
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []struct {
		name      string
		opts      queryOptions
		wantNames []string
	}{
		{
			name:      "path filter and explicit extension",
			opts:      queryOptions{Query: "path:Downloads ext:raw", Limit: 20},
			wantNames: []string{"camera.raw", "draft.raw"},
		},
		{
			name:      "path filter and dotted extension",
			opts:      queryOptions{Query: "path:Downloads .raw", Limit: 20},
			wantNames: []string{"camera.raw", "draft.raw"},
		},
		{
			name:      "absolute path filter and dotted extension",
			opts:      queryOptions{Query: `path:C:\fixture\Downloads .raw`, Limit: 20},
			wantNames: []string{"camera.raw", "draft.raw"},
		},
		{
			name:      "bare path-like term and dotted extension",
			opts:      queryOptions{Query: `C:\fixture\Downloads .raw`, MatchPath: true, Limit: 20},
			wantNames: []string{"camera.raw", "draft.raw"},
		},
		{
			name:      "dir filter and dotted ext filter",
			opts:      queryOptions{Query: "dir:Downloads ext:.raw", Limit: 20},
			wantNames: []string{"camera.raw", "draft.raw"},
		},
		{
			name:      "path filter and glob",
			opts:      queryOptions{Query: "path:Downloads glob:*.raw", Limit: 20},
			wantNames: []string{"camera.raw", "draft.raw"},
		},
		{
			name:      "path filter and regex",
			opts:      queryOptions{Query: `path:Downloads regex:Downloads.*\.raw$`, Limit: 20},
			wantNames: []string{"camera.raw", "draft.raw"},
		},
		{
			name:      "path filter extension type and not",
			opts:      queryOptions{Query: "path:Downloads ext:raw type:file !draft", Limit: 20},
			wantNames: []string{"camera.raw"},
		},
		{
			name:      "path filter and extension or",
			opts:      queryOptions{Query: "path:Downloads ext:raw|nrrd", Limit: 20},
			wantNames: []string{"camera.raw", "draft.raw", "scan.nrrd"},
		},
		{
			name:      "path filter inside or alternatives",
			opts:      queryOptions{Query: "path:Downloads|path:Assets ext:raw", Limit: 20},
			wantNames: []string{"asset.raw", "camera.raw", "draft.raw"},
		},
		{
			name:      "path filter inside not",
			opts:      queryOptions{Query: "ext:raw !path:Assets", Limit: 20},
			wantNames: []string{"camera.raw", "draft.raw", "lab.raw"},
		},
		{
			name:      "middle dotted substring is not extension",
			opts:      queryOptions{Query: ".opencode", Limit: 20},
			wantNames: []string{"ai.opencode.desktop"},
		},
		{
			name:      "drive scoped middle dotted substring",
			opts:      queryOptions{Query: "path:C: .opencode", Limit: 20},
			wantNames: []string{"ai.opencode.desktop", "settings.json"},
		},
		{
			name:      "explicit extension remains exact",
			opts:      queryOptions{Query: "ext:opencode", Limit: 20},
			wantNames: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full, err := searchCompactWithCache(idx, tc.opts, false, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("full search: %v", err)
			}
			if got := namesOf(full); !sameStringSet(got, tc.wantNames) {
				t.Fatalf("full names = %v, want %v", got, tc.wantNames)
			}
			fast, err := searchCompactWithCache(idx, tc.opts, false, vol.pathCache, vol.nameTermCandidates)
			if err != nil {
				t.Fatalf("candidate search: %v", err)
			}
			if got := namesOf(fast); !sameStringSet(got, tc.wantNames) {
				t.Fatalf("candidate names = %v, want %v", got, tc.wantNames)
			}
		})
	}
}

func TestStrictSpaceSplitDoesNotInferFusedPathExtensions(t *testing.T) {
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []queryOptions{
		{Query: "path:C:.nrrd", Limit: 20},
		{Query: "path:C:.NRRD", Limit: 20},
		{Query: "path:Downloads.raw", Limit: 20},
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
			if len(full) != 0 || len(fast) != 0 {
				t.Fatalf("strict fused query matched full=%v fast=%v, want no inferred extension matches", namesOf(full), namesOf(fast))
			}
		})
	}
}

func TestStrictSpaceSplitTokenPermutationsMatchAcrossSearchPaths(t *testing.T) {
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []struct {
		name      string
		tokens    []string
		wantNames []string
	}{
		{
			name:      "path ext type",
			tokens:    []string{"path:Downloads", "ext:raw", "type:file"},
			wantNames: []string{"camera.raw", "draft.raw"},
		},
		{
			name:      "path dotted type",
			tokens:    []string{"path:Downloads", ".raw", "type:file"},
			wantNames: []string{"camera.raw", "draft.raw"},
		},
		{
			name:      "path ext not",
			tokens:    []string{"path:Downloads", "ext:raw", "!draft"},
			wantNames: []string{"camera.raw"},
		},
		{
			name:      "path ext or",
			tokens:    []string{"path:Downloads", "ext:raw|nrrd"},
			wantNames: []string{"camera.raw", "draft.raw", "scan.nrrd"},
		},
	}
	for _, tc := range cases {
		for _, tokens := range permutations(tc.tokens) {
			query := strings.Join(tokens, " ")
			t.Run(tc.name+"/"+query, func(t *testing.T) {
				full, err := searchCompactWithCache(idx, queryOptions{Query: query, Limit: 20}, false, make(map[int]string), nil)
				if err != nil {
					t.Fatalf("full search: %v", err)
				}
				fast, err := searchCompactWithCache(idx, queryOptions{Query: query, Limit: 20}, false, vol.pathCache, vol.nameTermCandidates)
				if err != nil {
					t.Fatalf("candidate search: %v", err)
				}
				if got := namesOf(full); !sameStringSet(got, tc.wantNames) {
					t.Fatalf("full names = %v, want %v", got, tc.wantNames)
				}
				if got := namesOf(fast); !sameStringSet(got, tc.wantNames) {
					t.Fatalf("candidate names = %v, want %v", got, tc.wantNames)
				}
			})
		}
	}
}

func TestPathFilterParsingPropagatesThroughNestedSyntax(t *testing.T) {
	cases := []struct {
		query     string
		wantPath  bool
		wantTerms []string
		wantExts  []string
	}{
		{
			query:     "path:Downloads .raw",
			wantPath:  true,
			wantTerms: []string{"downloads"},
			wantExts:  []string{"raw"},
		},
		{
			query:     `path:C:\fixture\Downloads ext:.raw`,
			wantPath:  true,
			wantTerms: []string{"c:", "fixture", "downloads"},
			wantExts:  []string{"raw"},
		},
		{
			query:     "path:C:.nrrd",
			wantPath:  true,
			wantTerms: []string{"c:.nrrd"},
			wantExts:  nil,
		},
		{
			query:     "path:.nrrd",
			wantPath:  true,
			wantTerms: []string{".nrrd"},
			wantExts:  nil,
		},
		{
			query:    "path:Downloads|path:Assets ext:raw",
			wantPath: true,
			wantExts: []string{"raw"},
		},
		{
			query:    "ext:raw !path:Assets",
			wantPath: false,
			wantExts: []string{"raw"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			pq, err := parseQuery(queryOptions{Query: tc.query})
			if err != nil {
				t.Fatalf("parseQuery: %v", err)
			}
			if pq.MatchPath != tc.wantPath {
				t.Fatalf("MatchPath = %v, want %v", pq.MatchPath, tc.wantPath)
			}
			if !sameStringSet(pq.Terms, tc.wantTerms) {
				t.Fatalf("terms = %v, want %v", pq.Terms, tc.wantTerms)
			}
			if !sameStringSet(pq.Exts, tc.wantExts) {
				t.Fatalf("exts = %v, want %v", pq.Exts, tc.wantExts)
			}
		})
	}
}

func permutations(values []string) [][]string {
	out := make([][]string, 0)
	var walk func(int)
	items := append([]string(nil), values...)
	walk = func(pos int) {
		if pos == len(items) {
			out = append(out, append([]string(nil), items...))
			return
		}
		for i := pos; i < len(items); i++ {
			items[pos], items[i] = items[i], items[pos]
			walk(pos + 1)
			items[pos], items[i] = items[i], items[pos]
		}
	}
	walk(0)
	return out
}

func TestSearchCompactHonorsQueryDeadlineAndCancel(t *testing.T) {
	idx := syntheticCompactIndex(5000)
	if _, err := searchCompactWithCache(idx, queryOptions{
		Query:        "file",
		MatchPath:    true,
		Limit:        20,
		DeadlineUnix: time.Now().Add(-time.Millisecond).UnixNano(),
	}, false, make(map[int]string), nil); !errors.Is(err, errQueryCanceled) {
		t.Fatalf("deadline error = %v, want %v", err, errQueryCanceled)
	}
	if _, err := searchCompactWithCache(idx, queryOptions{
		Query:     "file",
		MatchPath: true,
		Limit:     20,
		Cancel:    func() bool { return true },
	}, false, make(map[int]string), nil); !errors.Is(err, errQueryCanceled) {
		t.Fatalf("cancel error = %v, want %v", err, errQueryCanceled)
	}
}

func TestReconstructCompactPathSkipsSyntheticDotRoot(t *testing.T) {
	idx := &Index{
		Compact: true,
		Volume:  "F:",
		Records: []CompactRecord{
			{FRN: 5, ParentFRN: 5, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 10, ParentFRN: 5, Parent: 0, Name: "git", Mode: uint32(os.ModeDir)},
			{FRN: 11, ParentFRN: 10, Parent: 1, Name: "seekfs", Mode: uint32(os.ModeDir)},
			{FRN: 12, ParentFRN: 11, Parent: 2, Name: "main.go"},
		},
	}
	if got, want := idx.reconstructCompactPath(3), `F:\git\seekfs\main.go`; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	if !pathUnder(idx.reconstructCompactPath(3), `F:\git\seekfs`) {
		t.Fatal("reconstructed path should be under F:\\git\\seekfs")
	}
}

func TestUnderSearchFiltersStaleFilesystemEntriesByDefault(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "stale-indexed-path.txt")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx := &Index{}
	if err := walkRoot(root, idx); err != nil {
		t.Fatal(err)
	}
	buildOrders(idx)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	got, err := search(idx, queryOptions{Query: "stale-indexed-path.txt", Under: root, Limit: 20}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("matches = %+v, want stale indexed path filtered", got)
	}
	countMatches, err := search(idx, queryOptions{Query: "stale-indexed-path.txt", Under: root, Limit: 20}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(countMatches) != 1 {
		t.Fatalf("count matches = %+v, want indexed count behavior unchanged", countMatches)
	}
}

func TestImplicitFilenameGlobQuery(t *testing.T) {
	pq, err := parseQuery(queryOptions{Query: "*_test.go", MatchPath: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(pq.Globs) != 1 || pq.Globs[0] != "*_test.go" {
		t.Fatalf("expected implicit glob for *_test.go, got globs=%v", pq.Globs)
	}
	if len(pq.Terms) != 0 {
		t.Fatalf("expected no plain terms for implicit glob, got %v", pq.Terms)
	}
}

func TestImplicitFilenameGlobMatchesFixture(t *testing.T) {
	idx := commonSearchFixture()
	got := searchFixtureNames(t, idx, queryOptions{Query: "*_test.go", MatchPath: true, Limit: 20})
	if len(got) != 1 || got[0] != "search_test.go" {
		t.Fatalf("implicit *_test.go glob = %v, want [search_test.go]", got)
	}
}

func TestImplicitFilenameGlobMatchesFixtureByName(t *testing.T) {
	idx := commonSearchFixture()
	got := searchFixtureNames(t, idx, queryOptions{Query: "*_test.go", Limit: 20})
	if len(got) != 1 || got[0] != "search_test.go" {
		t.Fatalf("implicit *_test.go filename glob = %v, want [search_test.go]", got)
	}
}

func TestParseOrGroup(t *testing.T) {
	pq, err := parseQuery(queryOptions{Query: "ext:png|jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pq.OrGroups) != 1 {
		t.Fatalf("expected 1 OR group, got %d", len(pq.OrGroups))
	}
	if len(pq.OrGroups[0]) != 2 {
		t.Fatalf("expected 2 alternatives, got %d", len(pq.OrGroups[0]))
	}
}

func TestParseNotGroup(t *testing.T) {
	pq, err := parseQuery(queryOptions{Query: "main !test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pq.NotGroups) != 1 {
		t.Fatalf("expected 1 NOT group, got %d", len(pq.NotGroups))
	}
	if len(pq.Terms) != 1 || pq.Terms[0] != "main" {
		t.Fatalf("expected term 'main', got %v", pq.Terms)
	}
}

func TestOrNotSizeOnFixture(t *testing.T) {
	idx := commonSearchFixture()

	// OR over extensions: .dat OR .txt files.
	orFiles := searchFixtureNames(t, idx, queryOptions{Query: "ext:dat|txt", MatchPath: true, Limit: 50})
	wantOr := map[string]bool{"sample.dat": true, "notes.txt": true, "sibling.dat": true}
	for _, n := range orFiles {
		if !wantOr[n] {
			t.Errorf("ext:dat|txt returned unexpected %q", n)
		}
		delete(wantOr, n)
	}
	if len(wantOr) != 0 {
		t.Errorf("ext:dat|txt missed %v", wantOr)
	}

	// NOT: .go files excluding *test*.
	goNoTest := searchFixtureNames(t, idx, queryOptions{Query: "ext:go !test", MatchPath: true, Limit: 50})
	for _, n := range goNoTest {
		if n == "search_test.go" {
			t.Errorf("ext:go !test should have excluded search_test.go")
		}
	}
	foundMain := false
	for _, n := range goNoTest {
		if n == "main.go" {
			foundMain = true
		}
	}
	if !foundMain {
		t.Error("ext:go !test should include main.go")
	}
}

func TestSizeAndModFiltersRequireCapableIndex(t *testing.T) {
	// An index with no sizes and no mtimes must reject size:/dm:/recent filters
	// rather than silently returning nothing.
	bare := &Index{Source: "usn", Volume: "C:", Compact: true}
	bare.Records = []CompactRecord{{FRN: 1, Parent: -1, Name: "C:", Mode: uint32(os.ModeDir)}}
	buildOrders(bare)

	for _, q := range []string{"size:>1mb", "dm:today"} {
		opts := queryOptions{Query: q}
		if _, err := searchCompactWithCache(bare, opts, false, make(map[int]string), nil); err == nil {
			t.Errorf("query %q on a size/mtime-less index should error", q)
		}
	}
	if _, err := searchCompactWithCache(bare, queryOptions{Query: "ext:go", Recent: "24h"}, false, make(map[int]string), nil); err == nil {
		t.Error("--recent on an mtime-less index should error")
	}

	// The standard fixture carries sizes and mtimes, so the same filters work.
	idx := commonSearchFixture()
	if !idx.compactHasSize() {
		t.Fatal("fixture should advertise size capability")
	}
	if !idx.compactHasModTime() {
		t.Fatal("fixture should advertise mtime capability")
	}
	if _, err := searchCompactWithCache(idx, queryOptions{Query: "size:>1kb"}, false, make(map[int]string), nil); err != nil {
		t.Errorf("size: on a capable index should not error: %v", err)
	}
}

func TestPlannedCountFastPathMatchesFullForFilters(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []queryOptions{
		{Query: "ext:dat"},
		{Query: "ext:dat|txt"},
		{Query: "type:file ext:go"},
		{Query: "ext:go !test"},
		{Query: "size:>=0"}, // every record qualifies
	}
	for _, opts := range cases {
		t.Run(opts.Query, func(t *testing.T) {
			pq, err := parseQuery(opts)
			if err != nil {
				t.Fatal(err)
			}
			// Confirm these queries take the no-path fast path.
			if queryNeedsPath(pq) {
				t.Fatalf("query %q unexpectedly needs path reconstruction", opts.Query)
			}
			got, ok := vol.plannedCount(pq)
			if !ok {
				// Some pure-filter queries decline the planner; fall back to full.
				full, ferr := searchCompactWithCache(idx, opts, true, make(map[int]string), nil)
				if ferr != nil {
					t.Fatal(ferr)
				}
				if len(full) == 0 {
					t.Skipf("query %q produced no candidates and declined planner", opts.Query)
				}
				return
			}
			full, err := searchCompactWithCache(idx, opts, true, make(map[int]string), nil)
			if err != nil {
				t.Fatal(err)
			}
			if got != len(full) {
				t.Fatalf("planned count = %d, full count = %d for %q", got, len(full), opts.Query)
			}
		})
	}
}

// searchFixtureNames runs a query against the fixture through the resident
// volume planner path and returns matched names.
func searchFixtureNames(t *testing.T, idx *Index, opts queryOptions) []string {
	t.Helper()
	entries, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatalf("search %q: %v", opts.Query, err)
	}
	return namesOf(entries)
}

func pathSyntaxFixture() *Index {
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
			Size:      1024,
			ModUnix:   time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixNano(),
		})
	}
	add(1, 1, -1, ".", uint32(os.ModeDir))
	add(2, 1, 0, "fixture", uint32(os.ModeDir))
	add(3, 2, 1, "Downloads", uint32(os.ModeDir))
	add(4, 3, 2, "camera.raw", 0)
	add(5, 3, 2, "scan.nrrd", 0)
	add(6, 3, 2, "draft.raw", 0)
	add(7, 2, 1, "Assets", uint32(os.ModeDir))
	add(8, 7, 6, "asset.raw", 0)
	add(9, 2, 1, "Lab", uint32(os.ModeDir))
	add(10, 9, 8, "lab.raw", 0)
	add(11, 2, 1, "ai.opencode.desktop", uint32(os.ModeDir))
	add(12, 11, 10, "settings.json", 0)
	buildOrders(idx)
	return idx
}
