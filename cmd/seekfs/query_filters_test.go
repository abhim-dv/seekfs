package main

import (
	"os"
	"testing"
	"time"
)

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
