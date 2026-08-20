package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestGlobalRecordIDOrdering(t *testing.T) {
	cases := []struct {
		a, b globalRecordID
		want int
	}{
		{globalRecordID{volume: 0, local: 1}, globalRecordID{volume: 0, local: 1}, 0},
		{globalRecordID{volume: 0, local: 2}, globalRecordID{volume: 0, local: 3}, -1},
		{globalRecordID{volume: 0, local: 4}, globalRecordID{volume: 0, local: 3}, 1},
		{globalRecordID{volume: 0, local: 99}, globalRecordID{volume: 1, local: 0}, -1},
		{globalRecordID{volume: 2, local: 0}, globalRecordID{volume: 1, local: 99}, 1},
	}
	for _, tc := range cases {
		if got := compareGlobalRecordID(tc.a, tc.b); got != tc.want {
			t.Fatalf("compareGlobalRecordID(%+v, %+v) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestGlobalPostingIteratorDecodesMappedBlocksLazily(t *testing.T) {
	t.Setenv("SEEKFS_POSTING_CACHE_MB", "1")
	servicePostingBlockCache = postingBlockLRU{}
	t.Cleanup(func() { servicePostingBlockCache = postingBlockLRU{} })

	ids := make([]uint32, 2050)
	for i := range ids {
		ids[i] = uint32(i)
	}
	section := decodePostingSection(encodeStringPostingSection(map[string][]uint32{"txt": ids}, nil))
	posting, count, ok := section.stringPostingIterator("txt")
	if !ok || count != len(ids) {
		t.Fatalf("posting iterator = (%v, %d), want (true, %d)", ok, count, len(ids))
	}
	it := newGlobalPostingIterator(1, postingCountCandidate{it: posting, count: count, mapped: true})
	if got := len(servicePostingBlockCache.items); got != 0 {
		t.Fatalf("constructor decoded %d blocks, want 0", got)
	}
	if got, ok := it.Next(); !ok || got != (globalRecordID{volume: 1, local: 0}) {
		t.Fatalf("first id = (%+v, %v), want volume 1 local 0", got, ok)
	}
	if got := len(servicePostingBlockCache.items); got != 1 {
		t.Fatalf("first Next decoded %d blocks, want 1", got)
	}

	servicePostingBlockCache = postingBlockLRU{}
	posting, count, _ = section.stringPostingIterator("txt")
	it = newGlobalPostingIterator(1, postingCountCandidate{it: posting, count: count, mapped: true})
	got, ok := it.SeekGE(globalRecordID{volume: 1, local: 2048})
	if !ok || got != (globalRecordID{volume: 1, local: 2048}) {
		t.Fatalf("SeekGE = (%+v, %v), want volume 1 local 2048", got, ok)
	}
	if got := len(servicePostingBlockCache.items); got != 1 {
		t.Fatalf("SeekGE decoded %d blocks, want only the target block", got)
	}
	if got := it.CountHint(); got != 1 {
		t.Fatalf("CountHint after SeekGE = %d, want 1", got)
	}
}

func TestGlobalSubtreeIteratorNestedRootsDedupesAndOrders(t *testing.T) {
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, Records: []CompactRecord{
		{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		{FRN: 2, ParentFRN: 1, Parent: 0, Name: "workspace", Mode: uint32(os.ModeDir)},
		{FRN: 3, ParentFRN: 2, Parent: 1, Name: "nested", Mode: uint32(os.ModeDir)},
		{FRN: 4, ParentFRN: 3, Parent: 2, Name: "model_v2.txt"},
		{FRN: 5, ParentFRN: 1, Parent: 0, Name: "sibling.txt"},
	}}
	buildOrders(idx)
	vol := newServiceVolumeIndex("nested-subtree.gsi", idx)
	it := newGlobalSubtreeScanIterator(0, vol, []int{1, 2})
	got := collectGlobalIterator(it, 0)
	want := []globalRecordID{{volume: 0, local: 1}, {volume: 0, local: 2}, {volume: 0, local: 3}}
	if !slices.Equal(got, want) {
		t.Fatalf("nested subtree IDs = %v, want %v", got, want)
	}
	it = newGlobalSubtreeScanIterator(0, vol, []int{1})
	if got, ok := it.SeekGE(globalRecordID{volume: 0, local: 3}); !ok || got.local != 3 {
		t.Fatalf("subtree SeekGE = (%+v, %v), want local 3", got, ok)
	}
}

func TestGlobalSubtreeIntervalIteratorUsesSubtreeMetadata(t *testing.T) {
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, Records: []CompactRecord{
		{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		{FRN: 2, ParentFRN: 1, Parent: 0, Name: "workspace", Mode: uint32(os.ModeDir)},
		{FRN: 3, ParentFRN: 2, Parent: 1, Name: "nested", Mode: uint32(os.ModeDir)},
		{FRN: 4, ParentFRN: 3, Parent: 2, Name: "model_v2.txt"},
		{FRN: 5, ParentFRN: 1, Parent: 0, Name: "sibling.txt"},
	}}
	buildOrders(idx)
	vol := newServiceVolumeIndex("interval-subtree.gsi", idx)
	vol.buildCompactChildren()
	vol.buildSubtreeRanges()
	it := newGlobalSubtreeIterator(0, vol, []int{1, 2})
	got := collectGlobalIterator(it, 0)
	want := []globalRecordID{{volume: 0, local: 1}, {volume: 0, local: 2}, {volume: 0, local: 3}}
	if !slices.Equal(got, want) {
		t.Fatalf("interval subtree IDs = %v, want %v", got, want)
	}
	if _, ok := it.SeekGE(globalRecordID{volume: 0, local: 2}); ok {
		t.Fatal("exhausted interval iterator unexpectedly returned an ID")
	}
}

func TestMappedComponentCoverageMergesNestedRootsAndHiddenSelfHits(t *testing.T) {
	idx := &Index{
		Version: indexVersionV9, Source: "usn", Volume: "C:", Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "needle", Mode: uint32(os.ModeDir)},
			{FRN: 3, ParentFRN: 2, Parent: 1, Name: "needle", Mode: uint32(os.ModeDir)},
			{FRN: 4, ParentFRN: 3, Parent: 2, Name: "inside.txt"},
			{FRN: 5, ParentFRN: 2, Parent: 1, Name: "other.txt"},
			{FRN: 6, ParentFRN: 1, Parent: 0, Name: "needle-report.txt"},
		},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("component-coverage.gsi", idx)
	vol.buildCompactChildren()
	vol.buildSubtreeRanges()
	vol.queryIndex = &residentQueryIndex{components: map[string][]uint32{"needle": {1, 2}}}

	coverage, ok := vol.buildMappedComponentCoverage("needle", []int{3, 5})
	if !ok {
		t.Fatal("mapped component coverage declined")
	}
	if coverage.rootCount != 2 || len(coverage.intervals) != 1 {
		t.Fatalf("coverage roots/intervals = %d/%d, want 2/1", coverage.rootCount, len(coverage.intervals))
	}
	if coverage.cardinality != 5 {
		t.Fatalf("coverage cardinality = %d, want 5 (nested roots deduped and one file self-hit)", coverage.cardinality)
	}
	count, verified := coverage.countLive(vol, func(id int) bool { return id == 3 })
	if count != 4 || verified != 5 {
		t.Fatalf("hidden count/verified = %d/%d, want 4/5", count, verified)
	}
}

func TestMappedComponentSubstringCoverageUsesCompletePCMPDictionary(t *testing.T) {
	idx := &Index{
		Version: indexVersionV9, Source: "usn", Volume: "C:", Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "Windows", Mode: uint32(os.ModeDir)},
			{FRN: 3, ParentFRN: 1, Parent: 0, Name: "Windows.old", Mode: uint32(os.ModeDir)},
			{FRN: 4, ParentFRN: 2, Parent: 1, Name: "inside.txt"},
			{FRN: 5, ParentFRN: 3, Parent: 2, Name: "old.txt"},
			{FRN: 6, ParentFRN: 1, Parent: 0, Name: "WindowsReport.txt"},
			{FRN: 7, ParentFRN: 1, Parent: 0, Name: "UsersBackup", Mode: uint32(os.ModeDir)},
		},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("component-substring.gsi", idx)
	vol.buildCompactChildren()
	vol.buildSubtreeRanges()
	vol.index.Derived.Postings = map[uint32]mappedPostingSection{
		indexSectionPCMP: {Data: encodeStringPostingSection(map[string][]uint32{
			"windows":     {1},
			"windows.old": {2},
			"usersbackup": {6},
		}, nil)},
	}
	coverage, ok := vol.mappedComponentSubstringCoverage("windows")
	if !ok {
		t.Fatal("substring PCMP coverage declined")
	}
	if coverage.rootCount != 2 || coverage.cardinality != 5 {
		t.Fatalf("substring coverage roots/cardinality = %d/%d, want 2/5", coverage.rootCount, coverage.cardinality)
	}
	if len(coverage.selfIDs) != 1 || coverage.selfIDs[0] != 5 {
		t.Fatalf("substring self hits = %v, want [5]", coverage.selfIDs)
	}
}

func TestCompleteLowerNameTermScanMatchesPackedOracle(t *testing.T) {
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, Records: []CompactRecord{
		{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		{FRN: 2, ParentFRN: 1, Parent: 0, Name: "Users", Mode: uint32(os.ModeDir)},
		{FRN: 3, ParentFRN: 1, Parent: 0, Name: "UsersBackup.txt"},
		{FRN: 4, ParentFRN: 1, Parent: 0, Name: "unrelated.txt"},
	}}
	buildOrders(idx)
	idx.packCompactRecords(true)
	got, ok := idx.scanCompactLowerNameTerm("users")
	if !ok {
		t.Fatal("packed lower-name scan declined")
	}
	want := make([]int, 0, len(got))
	for id := 0; id < idx.compactRecordCount(); id++ {
		if strings.Contains(idx.compactLowerNameAt(id), "users") && !idx.compactRecord(id).Deleted {
			want = append(want, id)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("packed lower-name scan = %v, want %v", got, want)
	}
}

// TestR5HolisticPlannerGeneratedCoverageMatrix keeps the component and boolean
// planner work honest across the dimensions called out by the R5 exit gate.
// This is deliberately a selected cross-product: it exercises every family
// and limit/sort pair while keeping the exhaustive oracle bounded and the
// seed stable for reproducible failures.
func TestR5HolisticPlannerGeneratedCoverageMatrix(t *testing.T) {
	const seed int64 = 0x5eedf5
	rng := rand.New(rand.NewSource(seed))
	terms := []string{"workspace", "Users", "src", "nrrd", "pdf", "raw", "x", "zzzz-nohit"}
	altTerms := []string{"needle", "backup", "dataset", "report"}
	families := []string{"bare", "implicit-path", "explicit-path", "dir", "ext", "scalar", "date", "parent", "attrib", "glob", "regex", "or", "not", "under"}
	sorts := []string{"", "sort:path", "sort:size", "sort:modified", "sort:extension", "sort:type"}
	limits := []int{1, 20, 100}
	volumeStates := []string{"single", "both", "absent", "early-miss-later-hit", "mixed-v8", "missing-derived"}
	resultStates := []string{"self-hit", "directory-root", "nested-root", "tie"}
	overlayStates := []string{"none", "create", "hide", "delete", "rename"}
	pairCounts := make(map[string]int)
	cases := make([]struct {
		family, volume, result, overlay, sort string
		query, under                          string
		matchPath                             bool
		limit                                 int
	}, 0, 48)
	for i := 0; i < 48; i++ {
		family := families[i%len(families)]
		term := terms[rng.Intn(len(terms))]
		alt := altTerms[rng.Intn(len(altTerms))]
		query := term
		matchPath := false
		under := ""
		switch family {
		case "implicit-path":
			matchPath = true
		case "explicit-path":
			query, matchPath = "path:"+term, true
		case "dir":
			query, matchPath = "dir:"+term, true
		case "ext":
			query = "ext:" + map[string]string{"nrrd": "nrrd", "pdf": "pdf", "raw": "raw"}[term]
			if strings.HasSuffix(query, ":") {
				query = "ext:nrrd"
			}
		case "scalar":
			query = "size:>=0 " + term
		case "date":
			query = "dm:2026-01-10 " + term
		case "parent":
			query = "parent:workspace"
		case "attrib":
			query = "type:file " + term
		case "glob":
			query = "glob:*" + term + "*"
		case "regex":
			query = "regex:.*" + regexpSafeLiteral(term) + ".*"
		case "or":
			query, matchPath = term+"|"+alt, true
		case "not":
			query, matchPath = term+" !backup", true
		case "under":
			query, matchPath, under = term, true, `C:\workspace`
		}
		sortToken := sorts[rng.Intn(len(sorts))]
		if sortToken != "" {
			query += " " + sortToken
		}
		volumeState := volumeStates[rng.Intn(len(volumeStates))]
		resultState := resultStates[rng.Intn(len(resultStates))]
		overlayState := overlayStates[rng.Intn(len(overlayStates))]
		limit := limits[rng.Intn(len(limits))]
		cases = append(cases, struct {
			family, volume, result, overlay, sort string
			query, under                          string
			matchPath                             bool
			limit                                 int
		}{family, volumeState, resultState, overlayState, sortToken, query, under, matchPath, limit})
		pairCounts[family+"/"+sortToken]++
	}

	for i, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("%02d-%s-%s-%s", i, tc.family, tc.volume, tc.overlay), func(t *testing.T) {
			baseC := randomCorpusIndex(seed+int64(i), randomCorpusParams{Records: 96})
			baseC.Source, baseC.Volume, baseC.Roots = "usn", "C:", []string{`C:\`}
			baseF := cloneCompactIndex(baseC)
			baseF.Volume, baseF.Roots = "F:", []string{`F:\`}
			if tc.volume == "mixed-v8" {
				baseF.Version = indexVersionV9
				baseF.Derived = indexDerivedSections{}
			}
			if tc.volume == "missing-derived" {
				baseF.Version = indexVersionV9
				baseF.Derived = indexDerivedSections{}
			}
			volC := newServiceVolumeIndex(fmt.Sprintf("r5-matrix-c-%d.gsi", i), baseC)
			volF := newServiceVolumeIndex(fmt.Sprintf("r5-matrix-f-%d.gsi", i), baseF)
			volumes := []*serviceVolumeIndex{volC, volF}
			switch tc.volume {
			case "single", "absent":
				volumes = []*serviceVolumeIndex{volC}
			case "early-miss-later-hit":
				volumes = []*serviceVolumeIndex{volF, volC}
			}
			query := tc.query
			under := tc.under
			if tc.volume == "absent" {
				query = "path:F: " + query
				// Keep the absent-volume case orthogonal to an explicit C:-root
				// under constraint; that combination is intentionally covered by
				// the separate under/volume adversarial tests.
				under = ""
			}
			opts := queryOptions{Query: query, MatchPath: tc.matchPath, Under: under, Limit: tc.limit}
			want, err := r5ExhaustivePlannerOracle(volumes, opts, false)
			if err != nil {
				t.Fatalf("exhaustive search oracle: %v", err)
			}
			trace := &searchTrace{}
			fast, err := searchServiceVolumes(volumes, queryOptions{Query: query, MatchPath: tc.matchPath, Under: under, Limit: tc.limit, Trace: trace}, false)
			if err != nil {
				t.Fatalf("planned search: %v trace=%+v", err, *trace)
			}
			if got, expected := pathsOf(fast), pathsOf(want); !sameOrderedStrings(got, expected) {
				t.Fatalf("search parity query=%q family=%s result=%s sort=%s got=%v want=%v trace=%+v", query, tc.family, tc.result, tc.sort, got, expected, *trace)
			}
			wantCount, err := r5ExhaustivePlannerOracle(volumes, opts, true)
			if err != nil {
				t.Fatalf("exhaustive count oracle: %v", err)
			}
			gotCount, err := searchServiceVolumes(volumes, queryOptions{Query: query, MatchPath: tc.matchPath, Under: under, Limit: tc.limit, Trace: &searchTrace{}}, true)
			if err != nil {
				t.Fatalf("planned count: %v", err)
			}
			if len(gotCount) != len(wantCount) {
				t.Fatalf("count parity query=%q family=%s got=%d want=%d", query, tc.family, len(gotCount), len(wantCount))
			}
			if tc.volume == "absent" && (trace.Candidates != 0 || trace.BlocksDecoded != 0) {
				t.Fatalf("absent volume did work: candidates=%d decoded=%d skipped=%d trace=%+v", trace.Candidates, trace.BlocksDecoded, trace.BlocksSkipped, *trace)
			}
		})
	}
	pairs := make([]string, 0, len(pairCounts))
	for pair, count := range pairCounts {
		pairs = append(pairs, fmt.Sprintf("%s=%d", pair, count))
	}
	sort.Strings(pairs)
	t.Logf("R5 matrix seed=%d cases=%d dimension-pairs=%s; oracle=exhaustive search/count, absent-volume zero-work asserted", seed, len(cases), strings.Join(pairs, ","))
}

func r5ExhaustivePlannerOracle(volumes []*serviceVolumeIndex, opts queryOptions, countOnly bool) ([]Entry, error) {
	selected, err := serviceVolumesForQuery(volumes, opts)
	if err != nil {
		return nil, err
	}
	selected = prioritizeServiceVolumesForPathTerms(selected, opts)
	out := make([]Entry, 0)
	for _, vol := range selected {
		child := opts
		if !countOnly {
			child.Limit = max(normalizedLimit(opts.Limit, false), vol.index.compactRecordCount())
		}
		got, err := searchCompactWithCache(vol.index, child, countOnly, make(map[int]string), nil)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	if !countOnly && len(selected) > 1 {
		pq, err := parseQuery(opts)
		if err != nil {
			return nil, err
		}
		sortSearchAllEntries(out, pq)
	}
	if !countOnly {
		limit := normalizedLimit(opts.Limit, false)
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
	}
	return out, nil
}

func TestR5GeneratedOverlayCoverageMatrix(t *testing.T) {
	const seed int64 = 0x0a11ce
	states := []struct {
		name    string
		changes []usnChange
	}{
		{name: "none"},
		{name: "create", changes: []usnChange{{FRN: 301, ParentFRN: 100, USN: 10, Reason: usnReasonFileCreate, Name: "matrix-needle.txt"}}},
		{name: "hide", changes: []usnChange{{FRN: 302, ParentFRN: 100, USN: 11, Reason: usnReasonFileCreate, Name: "matrix-hidden.txt", Attr: fileAttributeHidden}}},
		{name: "delete", changes: []usnChange{{FRN: 101, USN: 12, Reason: usnReasonFileDelete}}},
		{name: "rename", changes: []usnChange{{FRN: 101, ParentFRN: 100, USN: 13, Reason: usnReasonRenameOld, Name: "needle-base.txt"}, {FRN: 101, ParentFRN: 100, USN: 14, Reason: usnReasonRenameNew, Name: "renamed-away.txt"}}},
	}
	queries := []queryOptions{
		{Query: "needle", MatchPath: true, Limit: 1},
		{Query: "path:base-parent needle", Limit: 20},
		{Query: "needle|base", MatchPath: true, Limit: 100},
		{Query: "ext:txt sort:path", Limit: 20},
		{Query: "type:file needle", MatchPath: true, Limit: 20},
		{Query: "path:F: needle|base", MatchPath: true, Limit: 20},
		{Query: "path:F: needle !base", MatchPath: true, Limit: 20},
	}
	caseCount := 0
	for _, state := range states {
		state := state
		t.Run(state.name, func(t *testing.T) {
			vol := engineV9OverlaySearchTestVolume(t)
			logical := make(map[uint64]CompactRecord, vol.index.compactRecordCount())
			for i := 0; i < vol.index.compactRecordCount(); i++ {
				rec := vol.index.compactRecord(i)
				logical[rec.FRN] = rec
			}
			if len(state.changes) > 0 {
				vol.applyUSNChanges(state.changes)
				applyLogicalUSNChanges(logical, state.changes)
			}
			fresh := freshIndexFromLogicalRecords("F:", logical)
			fresh.Roots = []string{`F:\`}
			freshVol := newServiceVolumeIndex(fmt.Sprintf("r5-overlay-oracle-%d.gsi", seed+int64(len(state.name))), fresh)
			for _, baseOpts := range queries {
				caseCount++
				opts := baseOpts
				want, err := r5ExhaustivePlannerOracle([]*serviceVolumeIndex{freshVol}, opts, false)
				if err != nil {
					t.Fatalf("state=%s query=%q oracle search: %v", state.name, opts.Query, err)
				}
				got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
				if err != nil {
					t.Fatalf("state=%s query=%q overlay search: %v", state.name, opts.Query, err)
				}
				if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !sameOrderedStrings(gotPaths, wantPaths) {
					t.Fatalf("state=%s query=%q paths=%v want=%v fresh-volume=%q fresh-records=%d fresh-state=%q", state.name, opts.Query, gotPaths, wantPaths, fresh.Volume, len(fresh.Records), freshVol.state)
				}
				wantCount, err := r5ExhaustivePlannerOracle([]*serviceVolumeIndex{freshVol}, opts, true)
				if err != nil {
					t.Fatalf("state=%s query=%q oracle count: %v", state.name, opts.Query, err)
				}
				gotCount, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, true)
				if err != nil {
					t.Fatalf("state=%s query=%q overlay count: %v", state.name, opts.Query, err)
				}
				if len(gotCount) != len(wantCount) {
					t.Fatalf("state=%s query=%q count=%d want=%d", state.name, opts.Query, len(gotCount), len(wantCount))
				}
			}
		})
	}
	t.Logf("R5 overlay matrix seed=%d cases=%d states=none/create/hide/delete/rename; volume+overlay+boolean triples included; oracle=clean rebuilt index search/count", seed, caseCount)
}

// TestR5RequiredAdversarialCoverageMatrix makes the exit-gate dimensions that
// are easy to miss explicit.  It uses one stable fixture and checks both
// search ordering and exact count against the exhaustive oracle for every
// case, including the v8 fallback volume.
func TestR5RequiredAdversarialCoverageMatrix(t *testing.T) {
	const seed int64 = 0x71e5eed
	baseC := randomCorpusIndex(seed, randomCorpusParams{Records: 144})
	baseC.Source, baseC.Volume, baseC.Roots, baseC.CompactAttrs = "usn", "C:", []string{`C:\`}, true
	for i := range baseC.Records {
		if baseC.Records[i].Mode&uint32(os.ModeDir) == 0 && i%9 == 0 {
			baseC.Records[i].Mode = modeFromAttrs(fileAttributeHidden | fileAttributeArchive)
		}
		if i >= 8 && i <= 11 {
			baseC.Records[i].Size = 4096
			baseC.Records[i].ModUnix = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC).UnixNano()
		}
	}
	buildOrders(baseC)
	baseF := cloneCompactIndex(baseC)
	baseF.Volume, baseF.Roots = "F:", []string{`F:\`}
	baseF.Version, baseF.Derived = indexVersionV9, indexDerivedSections{}
	volC := newServiceVolumeIndex("r5-adversarial-c.gsi", baseC)
	volF := newServiceVolumeIndex("r5-adversarial-f-v8.gsi", baseF)

	cases := []struct {
		name, query, volume, under string
		matchPath                  bool
		limit                      int
	}{
		{"attrib-hidden", "attrib:H", "C", "", false, 20},
		{"exact-name", "workspace", "C", "", true, 1},
		{"substring-superset", "work", "C", "", true, 20},
		{"common-term", "a", "C", "", true, 100},
		{"rare-term", "Ã¼ber", "C", "", true, 20},
		{"short-term", "x", "C", "", true, 1},
		{"dotted-term", ".leading-dot", "C", "", true, 20},
		{"case-varied", "WORKSPACE", "C", "", true, 20},
		{"no-hit", "zzzz-nohit", "C", "", true, 20},
		{"separator-derived", `path:workspace\src`, "C", "", true, 20},
		{"file-self-hit", "path:README.md", "C", "", true, 20},
		{"directory-self-hit", "path:workspace", "C", "", true, 20},
		{"nested-overlap", "path:workspace|path:src", "C", "", true, 100},
		{"under-root", "workspace", "C", `C:\workspace`, true, 20},
		{"size-ascending-ties", "type:file sort:size", "C", "", true, 20},
		{"modified-descending-ties", "type:file sort:modified", "C", "", true, 20},
		{"path-order", "path:workspace sort:path", "both", "", true, 20},
		{"type-order", "path:workspace sort:type", "both", "", true, 20},
		{"extension-order", "ext:nrrd sort:extension", "both", "", true, 20},
		{"v8-fallback", "nrrd sort:size", "F", "", true, 20},
		{"mixed-metadata-ties", "nrrd sort:modified", "both", "", true, 100},
		{"absent-volume", "path:F: .pdf", "C", "", true, 20},
		{"volume-boolean", "path:F: nrrd|raw", "F", "", true, 20},
		{"volume-not", "path:F: nrrd !backup", "F", "", true, 20},
	}
	pairs := make(map[string]struct{})
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			volumes := []*serviceVolumeIndex{volC}
			switch tc.volume {
			case "F":
				volumes = []*serviceVolumeIndex{volF}
			case "both":
				volumes = []*serviceVolumeIndex{volC, volF}
			}
			opts := queryOptions{Query: tc.query, MatchPath: tc.matchPath, Under: tc.under, Limit: tc.limit}
			want, err := r5ExhaustivePlannerOracle(volumes, opts, false)
			if err != nil {
				t.Fatalf("oracle search: %v", err)
			}
			trace := &searchTrace{}
			got, err := searchServiceVolumes(volumes, queryOptions{Query: tc.query, MatchPath: tc.matchPath, Under: tc.under, Limit: tc.limit, Trace: trace}, false)
			if err != nil {
				t.Fatalf("planned search: %v trace=%+v", err, *trace)
			}
			if !sameOrderedStrings(pathsOf(got), pathsOf(want)) {
				t.Fatalf("search parity got=%v want=%v trace=%+v", pathsOf(got), pathsOf(want), *trace)
			}
			wantCount, err := r5ExhaustivePlannerOracle(volumes, opts, true)
			if err != nil {
				t.Fatalf("oracle count: %v", err)
			}
			gotCount, err := searchServiceVolumes(volumes, queryOptions{Query: tc.query, MatchPath: tc.matchPath, Under: tc.under, Limit: tc.limit, Trace: &searchTrace{}}, true)
			if err != nil {
				t.Fatalf("planned count: %v", err)
			}
			if len(gotCount) != len(wantCount) {
				t.Fatalf("count parity got=%d want=%d", len(gotCount), len(wantCount))
			}
			if tc.name == "absent-volume" && (trace.Candidates != 0 || trace.BlocksDecoded != 0) {
				t.Fatalf("absent volume did work: trace=%+v", *trace)
			}
			pairs[tc.name+"/"+tc.volume] = struct{}{}
		})
	}
	keys := make([]string, 0, len(pairs))
	for pair := range pairs {
		keys = append(keys, pair)
	}
	sort.Strings(keys)
	t.Logf("R5 required adversarial matrix seed=%d cases=%d dimension-pairs=%s; search/count exhaustive parity, zero-work absent volume, global tie order asserted", seed, len(cases), strings.Join(keys, ","))
}

func TestMappedComponentTopUsesDescendantRankBounds(t *testing.T) {
	const roots = 2048
	idx := &Index{
		Version: indexVersionV9, Source: "usn", Volume: "C:", Compact: true,
		Roots: []string{`C:\`}, Records: make([]CompactRecord, 0, 1+roots*3),
	}
	idx.Records = append(idx.Records, CompactRecord{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)})
	for i := 0; i < roots; i++ {
		bucketID := len(idx.Records)
		bucketFRN := uint64(bucketID + 1)
		idx.Records = append(idx.Records, CompactRecord{
			FRN: bucketFRN, ParentFRN: 1, Parent: 0,
			Name: fmt.Sprintf("bucket-%04d", i), Mode: uint32(os.ModeDir),
		})
		workspaceID := len(idx.Records)
		workspaceFRN := uint64(workspaceID + 1)
		idx.Records = append(idx.Records, CompactRecord{
			FRN: workspaceFRN, ParentFRN: bucketFRN, Parent: int32(bucketID),
			Name: "workspace", Mode: uint32(os.ModeDir),
		})
		childName := fmt.Sprintf("z-%04d.txt", i)
		if i < roots/2 {
			childName = fmt.Sprintf("a-%04d.txt", i)
		}
		idx.Records = append(idx.Records, CompactRecord{
			FRN: uint64(len(idx.Records) + 1), ParentFRN: workspaceFRN, Parent: int32(workspaceID),
			Name: childName, Size: int64(i + 1), ModUnix: int64(i + 1),
		})
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "component-top.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save index: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load mapped index: %v", err)
	}
	if loaded.MMapRecords == nil {
		t.Fatal("expected mapped records")
	}
	t.Cleanup(func() { _ = loaded.MMapRecords.file.close() })
	vol := newServiceVolumeIndex(db, loaded)
	for _, sortColumn := range []string{"", "path", "size", "modified", "extension", "type"} {
		query := "path:workspace"
		if sortColumn != "" {
			query += " sort:" + sortColumn
		}
		opts := queryOptions{Query: query, Limit: 1}
		var want []Entry
		if sortColumn == "" {
			want, err = searchCompactWithCache(loaded, opts, false, make(map[int]string), nil)
		} else {
			want, err = searchAll([]*Index{loaded}, opts, false)
		}
		if err != nil {
			t.Fatalf("%s oracle search: %v", sortColumn, err)
		}
		trace := &searchTrace{}
		opts.Trace = trace
		got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
		if err != nil {
			t.Fatalf("%s mapped component search: %v", sortColumn, err)
		}
		if !slices.Equal(pathsOf(got), pathsOf(want)) {
			t.Fatalf("%s paths = %v, want %v", sortColumn, pathsOf(got), pathsOf(want))
		}
		if trace.Source != "global:component-top" {
			t.Fatalf("%s trace = %+v, want component-top", sortColumn, trace)
		}
		if trace.ComponentDriver == "persisted-order-pngc-self" {
			if trace.ComponentRecordsVerified == 0 {
				t.Fatalf("%s complete self-name route did no bounded verification: %+v", sortColumn, trace)
			}
		} else if trace.BlocksSkipped == 0 {
			t.Fatalf("%s trace = %+v, want component-top with skipped blocks or complete self-name route", sortColumn, trace)
		}
	}
}

func TestGlobalBooleanIteratorsDeduplicateAndExcludeLazily(t *testing.T) {
	a := newGlobalIDSliceIterator([]globalRecordID{{volume: 0, local: 1}, {volume: 0, local: 3}, {volume: 1, local: 1}})
	b := newGlobalIDSliceIterator([]globalRecordID{{volume: 0, local: 3}, {volume: 1, local: 0}, {volume: 1, local: 1}})
	union := newGlobalMergeIterator(&a, &b)
	if got := collectGlobalIterator(union, 0); !slices.Equal(got, []globalRecordID{{volume: 0, local: 1}, {volume: 0, local: 3}, {volume: 1, local: 0}, {volume: 1, local: 1}}) {
		t.Fatalf("union = %v", got)
	}
	include := newGlobalIDSliceIterator([]globalRecordID{{volume: 0, local: 1}, {volume: 0, local: 3}, {volume: 1, local: 1}})
	exclude := newGlobalIDSliceIterator([]globalRecordID{{volume: 0, local: 1}, {volume: 1, local: 1}})
	countingExclude := &countingSeekGlobalIterator{globalIDIterator: &exclude}
	without := newGlobalExclusionIterator(&include, countingExclude)
	if got := collectGlobalIterator(without, 0); !slices.Equal(got, []globalRecordID{{volume: 0, local: 3}}) {
		t.Fatalf("exclusion = %v", got)
	}
	if countingExclude.seeks == 0 {
		t.Fatal("exclusion did not use SeekGE on the broad negative iterator")
	}
}

type countingSeekGlobalIterator struct {
	globalIDIterator
	seeks int
}

func (it *countingSeekGlobalIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	it.seeks++
	return it.globalIDIterator.SeekGE(target)
}

func TestBooleanLocalPlanUsesBoundedTopN(t *testing.T) {
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, Records: []CompactRecord{
		{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		{FRN: 2, ParentFRN: 1, Parent: 0, Name: "nrrd-z.txt"},
		{FRN: 3, ParentFRN: 1, Parent: 0, Name: "raw-a.txt"},
		{FRN: 4, ParentFRN: 1, Parent: 0, Name: "nrrd-a.txt"},
		{FRN: 5, ParentFRN: 1, Parent: 0, Name: "other.txt"},
	}}
	buildOrders(idx)
	idx.packCompactRecords(true)
	vol := newServiceVolumeIndex("boolean-local.gsi", idx)
	pq, err := parseQuery(queryOptions{Query: "nrrd|raw", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	pq.Limit = 1
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok || len(plan.sources) != 1 || len(plan.sources[0].union) != 2 {
		t.Fatalf("plan = %+v, want one two-way union", plan)
	}
	got, scanned, ok := plan.executeTop(pq)
	if !ok || scanned == 0 || scanned > 2 || len(got) != 1 {
		t.Fatalf("bounded top = ids=%v scanned=%d ok=%v, want bounded two-branch top", got, scanned, ok)
	}
}

func TestGlobalBooleanPersistedTopAndCountMatchOracle(t *testing.T) {
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	volumes := deterministicMultiVolumeCorpus(t, 0x0b001)
	queries := []string{
		"path:C: nrrd|raw",
		"path:C: raw|nrrd",
		"path:C: nrrd|nrrd",
		"path:C: zzzz-no-hit|no-such-term",
		"path:C: a|raw",
		"path:C: zzzz-no-hit|raw sort:path",
		"path:C: nrrd|raw sort:modified",
		"path:C: nrrd|raw sort:extension",
		"path:C: nrrd|raw sort:type",
		"path:F: nrrd|raw",
		"path:C: nrrd !raw sort:size",
	}
	for _, query := range queries {
		for _, limit := range []int{1, 20, 100} {
			t.Run(fmt.Sprintf("%s-limit-%d", query, limit), func(t *testing.T) {
				opts := queryOptions{Query: query, Limit: limit}
				want, err := r5ExhaustivePlannerOracle(volumes, opts, false)
				if err != nil {
					t.Fatal(err)
				}
				trace := &searchTrace{}
				got, err := searchServiceVolumes(volumes, queryOptions{Query: query, Limit: limit, Trace: trace}, false)
				if err != nil {
					t.Fatalf("search: %v trace=%+v", err, *trace)
				}
				if !slices.Equal(pathsOf(got), pathsOf(want)) {
					t.Fatalf("search paths=%v want=%v trace=%+v", pathsOf(got), pathsOf(want), *trace)
				}
				wantCount, err := r5ExhaustivePlannerOracle(volumes, opts, true)
				if err != nil {
					t.Fatal(err)
				}
				countTrace := &searchTrace{}
				gotCount, _, err := countServiceVolumes(volumes, queryOptions{Query: query, Limit: limit, Trace: countTrace})
				if err != nil {
					t.Fatalf("count: %v trace=%+v", err, *countTrace)
				}
				if gotCount != len(wantCount) {
					t.Fatalf("count=%d want=%d trace=%+v", gotCount, len(wantCount), *countTrace)
				}
				if strings.Contains(query, "!") {
					if trace.Source != "global:boolean-iterator" || countTrace.Source != "global:boolean-iterator" {
						t.Fatalf("NOT did not use lazy iterator: search=%+v count=%+v", *trace, *countTrace)
					}
				} else if trace.Source != "global:boolean-persisted-top" || countTrace.Source != "global:boolean-persisted-count" {
					t.Fatalf("OR did not use persisted top/count: search=%+v count=%+v", *trace, *countTrace)
				}
			})
		}
	}
}

func TestGlobalBooleanCancellationUsesSafeFallback(t *testing.T) {
	volumes := deterministicMultiVolumeCorpus(t, 0x0b002)
	canceled := func() bool { return true }
	for _, countOnly := range []bool{false, true} {
		trace := &searchTrace{}
		opts := queryOptions{Query: "path:C: nrrd|raw", Limit: 20, Cancel: canceled, Trace: trace}
		if countOnly {
			_, _, err := countServiceVolumes(volumes, opts)
			if !errors.Is(err, errQueryCanceled) {
				t.Fatalf("count cancellation error=%v trace=%+v, want %v", err, *trace, errQueryCanceled)
			}
			continue
		}
		_, err := searchServiceVolumes(volumes, opts, false)
		if !errors.Is(err, errQueryCanceled) {
			t.Fatalf("search cancellation error=%v trace=%+v, want %v", err, *trace, errQueryCanceled)
		}
	}
}

func TestSearchAllMultiIndexLimitDoesNotHideLaterBetterMatch(t *testing.T) {
	cIdx := singleFileCompactIndex("C:", "z-match.txt")
	fIdx := singleFileCompactIndex("F:", "a-match.txt")

	got, err := searchAll([]*Index{cIdx, fIdx}, queryOptions{Query: "match", Limit: 1}, false)
	if err != nil {
		t.Fatalf("searchAll: %v", err)
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`F:\workspace\a-match.txt`}) {
		t.Fatalf("paths = %v, want F volume global best match", gotPaths)
	}
}

func TestSearchAllPromotedShortExtensionDoesNotWaterfall(t *testing.T) {
	cIdx := singleDownloadFileCompactIndex("C:", "z-notes.md")
	fIdx := singleDownloadFileCompactIndex("F:", "a-notes.md")

	got, err := searchAll([]*Index{cIdx, fIdx}, queryOptions{Query: "path:Downloads md", Limit: 1}, false)
	if err != nil {
		t.Fatalf("searchAll: %v", err)
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`F:\Downloads\a-notes.md`}) {
		t.Fatalf("paths = %v, want F volume global best promoted extension match", gotPaths)
	}
}

func TestServiceVolumesPromotedShortExtensionDoesNotWaterfall(t *testing.T) {
	for _, lowmem := range []bool{false, true} {
		t.Run(fmt.Sprintf("lowmem=%v", lowmem), func(t *testing.T) {
			if lowmem {
				t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
			}
			volumes := []*serviceVolumeIndex{
				newServiceVolumeIndex("c-short-ext.gsi", singleDownloadFileCompactIndex("C:", "z-notes.md")),
				newServiceVolumeIndex("f-short-ext.gsi", singleDownloadFileCompactIndex("F:", "a-notes.md")),
			}
			got, err := searchServiceVolumes(volumes, queryOptions{Query: "path:Downloads md", Limit: 1}, false)
			if err != nil {
				t.Fatalf("searchServiceVolumes: %v", err)
			}
			if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`F:\Downloads\a-notes.md`}) {
				t.Fatalf("paths = %v, want F volume global best promoted extension match", gotPaths)
			}
		})
	}
}

func TestMultiVolumePlannerDeclineDoesNotUsePerVolumeTerminal(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("c-declined.gsi", singleFileCompactIndex("C:", "needle.txt")),
		newServiceVolumeIndex("f-declined.gsi", singleFileCompactIndex("F:", "needle.txt")),
	}
	// Model the only unsafe global-planner decline that must not fall through:
	// an active overlay whose snapshot has not been published yet.
	volumes[0].overlay.watermark.Store(1)
	volumes[0].snap.Store(nil)

	trace := &searchTrace{}
	_, err := searchServiceVolumes(volumes, queryOptions{Query: "needle", Trace: trace}, false)
	if !errors.Is(err, errGlobalMultiVolumePlannerDeclined) {
		t.Fatalf("search error = %v, want global multi-volume planner decline", err)
	}
	if trace.PlannerMode == "service-per-volume" || trace.Fallback == "service-per-volume" {
		t.Fatalf("search trace used removed per-volume terminal: %+v", trace)
	}

	countTrace := &searchTrace{}
	_, handled, err := countServiceVolumes(volumes, queryOptions{Query: "needle", Trace: countTrace})
	if !handled || !errors.Is(err, errGlobalMultiVolumePlannerDeclined) {
		t.Fatalf("count = (handled=%v, err=%v), want handled decline error", handled, err)
	}
	if countTrace.PlannerMode == "service-count-per-volume" || countTrace.Fallback == "service-count-per-volume" {
		t.Fatalf("count trace used removed per-volume terminal: %+v", countTrace)
	}
}

func TestBareShortNameSubstringKeepsSubstringSemantics(t *testing.T) {
	idx := shortSubstringCompactIndex()
	got, err := searchCompactWithCache(idx, queryOptions{Query: "md", Limit: 10}, false, make(map[int]string), nil)
	if err != nil {
		t.Fatalf("search md: %v", err)
	}
	if names := namesOf(got); !sameStringSet(names, []string{"cmd.exe", "readme.md"}) {
		t.Fatalf("names = %v, want substring match not extension rewrite", names)
	}
}

func TestExplicitTwoCharExtUsesGlobalMerge(t *testing.T) {
	for _, query := range []string{"ext:md", "glob:*.md"} {
		t.Run(query, func(t *testing.T) {
			volumes := []*serviceVolumeIndex{
				newServiceVolumeIndex("c-explicit-md.gsi", singleDownloadFileCompactIndex("C:", "z-notes.md")),
				newServiceVolumeIndex("f-explicit-md.gsi", singleDownloadFileCompactIndex("F:", "a-notes.md")),
			}
			got, err := searchServiceVolumes(volumes, queryOptions{Query: query, Limit: 1}, false)
			if err != nil {
				t.Fatalf("searchServiceVolumes: %v", err)
			}
			if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`F:\Downloads\a-notes.md`}) {
				t.Fatalf("paths = %v, want F volume global best explicit extension match", gotPaths)
			}
		})
	}
}

func singleFileCompactIndex(volume, name string) *Index {
	idx := &Index{
		Source:  "usn",
		Volume:  volume,
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
	workspace := add(2, 1, root, "workspace", uint32(os.ModeDir))
	add(3, 2, workspace, name, 0)
	buildOrders(idx)
	return idx
}

func singleDownloadFileCompactIndex(volume, name string) *Index {
	idx := &Index{
		Source:  "usn",
		Volume:  volume,
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
	downloads := add(2, 1, root, "Downloads", uint32(os.ModeDir))
	add(3, 2, downloads, name, 0)
	buildOrders(idx)
	return idx
}

func shortSubstringCompactIndex() *Index {
	idx := singleDownloadFileCompactIndex("C:", "readme.md")
	idx.Records = append(idx.Records, CompactRecord{
		FRN:       4,
		ParentFRN: 2,
		Parent:    1,
		Name:      "cmd.exe",
		Mode:      0,
		Size:      1024,
		ModUnix:   time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixNano(),
	})
	buildOrders(idx)
	return idx
}

func TestGlobalRecordIteratorNextSeekGE(t *testing.T) {
	it := newGlobalRecordIterator(2, []int{1, 4, 9, 15})
	var _ globalIDIterator = &it
	if got := it.CountHint(); got != 4 {
		t.Fatalf("initial CountHint = %d, want 4", got)
	}
	if got, ok := it.Next(); !ok || got != (globalRecordID{volume: 2, local: 1}) {
		t.Fatalf("Next = %+v/%v, want volume 2 local 1", got, ok)
	}
	if got := it.CountHint(); got != 3 {
		t.Fatalf("CountHint after Next = %d, want 3", got)
	}
	if got, ok := it.SeekGE(globalRecordID{volume: 2, local: 8}); !ok || got != (globalRecordID{volume: 2, local: 9}) {
		t.Fatalf("SeekGE local 8 = %+v/%v, want volume 2 local 9", got, ok)
	}
	if got, ok := it.SeekGE(globalRecordID{volume: 1, local: 99}); !ok || got != (globalRecordID{volume: 2, local: 15}) {
		t.Fatalf("SeekGE prior volume = %+v/%v, want volume 2 local 15", got, ok)
	}
	if got, ok := it.SeekGE(globalRecordID{volume: 3, local: 0}); ok {
		t.Fatalf("SeekGE later volume = %+v/%v, want exhausted", got, ok)
	}
	if got, ok := it.Next(); ok {
		t.Fatalf("Next after exhausted = %+v/%v, want exhausted", got, ok)
	}
	if got := it.CountHint(); got != 0 {
		t.Fatalf("CountHint after exhausted = %d, want 0", got)
	}
}

func TestGlobalPostingIteratorAdaptsPostingCandidate(t *testing.T) {
	it := newGlobalPostingIterator(3, postingCountCandidate{ids: []uint32{2, 5, 11}})
	if got := it.CountHint(); got != 3 {
		t.Fatalf("posting CountHint = %d, want 3", got)
	}
	if got, ok := it.SeekGE(globalRecordID{volume: 3, local: 4}); !ok || got != (globalRecordID{volume: 3, local: 5}) {
		t.Fatalf("posting SeekGE = %+v/%v, want volume 3 local 5", got, ok)
	}
	if got, ok := it.Next(); !ok || got != (globalRecordID{volume: 3, local: 11}) {
		t.Fatalf("posting Next = %+v/%v, want volume 3 local 11", got, ok)
	}
}

func TestGlobalExtPostingIDsMatchPerVolumePostings(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	got, ok := globalExtPostingIDs(volumes, "bin", 0, nil)
	if !ok {
		t.Fatal("globalExtPostingIDs declined ext source")
	}
	want := make([]globalRecordID, 0, 8)
	for _, id := range volumes[1].extPosting("bin") {
		want = append(want, globalRecordID{volume: 1, local: id})
	}
	if !slices.Equal(got, want) {
		t.Fatalf("global ext ids = %+v, want %+v", got, want)
	}
}

func TestGlobalExtPostingIDsRespectsLimit(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	got, ok := globalExtPostingIDs(volumes, "bin", 3, nil)
	if !ok {
		t.Fatal("globalExtPostingIDs declined ext source")
	}
	want := []globalRecordID{
		{volume: 1, local: volumes[1].extPosting("bin")[0]},
		{volume: 1, local: volumes[1].extPosting("bin")[1]},
		{volume: 1, local: volumes[1].extPosting("bin")[2]},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("limited global ext ids = %+v, want %+v", got, want)
	}
}

func TestGlobalExtPostingIDsTraceMissingPostingVolume(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	if volumes[0].queryIndex == nil {
		volumes[0].queryIndex = &residentQueryIndex{}
	}
	volumes[0].queryIndex.ext = nil
	trace := &searchTrace{}
	if _, ok := globalExtPostingIDs(volumes, "bin", 0, trace); ok {
		t.Fatal("globalExtPostingIDs unexpectedly handled missing ext source")
	}
	if trace.Decline != "global-ext:missing-posting" {
		t.Fatalf("decline = %q, want global-ext:missing-posting", trace.Decline)
	}
	if len(trace.Declines) != 1 || trace.Declines[0].Volume != "C:" || trace.Declines[0].Source != "global-ext" || trace.Declines[0].Reason != "missing-posting" {
		t.Fatalf("declines = %+v, want C: missing-posting", trace.Declines)
	}
}

func TestGlobalPlannerExtRankedPathTraceMissingPostingVolume(t *testing.T) {
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	if volumes[0].queryIndex == nil {
		volumes[0].queryIndex = &residentQueryIndex{}
	}
	volumes[0].queryIndex.ext = nil
	trace := &searchTrace{}
	opts := queryOptions{Query: "ext:bin", Limit: 5, Trace: trace}
	if got, handled, err := searchServiceVolumesGlobalExtOnly(volumes, opts, false); err != nil {
		t.Fatal(err)
	} else if handled {
		t.Fatalf("global ext unexpectedly handled missing posting source with results=%v", got)
	}
	if trace.Decline != "global-ext:missing-posting" {
		t.Fatalf("decline = %q, want global-ext:missing-posting", trace.Decline)
	}
	if len(trace.Declines) != 1 || trace.Declines[0].Volume != "C:" || trace.Declines[0].Source != "global-ext" || trace.Declines[0].Reason != "missing-posting" {
		t.Fatalf("declines = %+v, want C: missing-posting", trace.Declines)
	}
}

func TestGlobalComponentRootIDsMatchPerVolumeRoots(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	got, ok := globalComponentRootIDs(volumes, "workspace-alpha", 0)
	if !ok {
		t.Fatal("globalComponentRootIDs declined component source")
	}
	want := make([]globalRecordID, 0, 9)
	for volumeIndex, vol := range volumes {
		for _, id := range vol.pathComponentRootIDs("workspace-alpha") {
			want = append(want, globalRecordID{volume: volumeIndex, local: id})
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("global component roots = %+v, want %+v", got, want)
	}
}

func TestGlobalComponentRootIDsRespectsLimit(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	got, ok := globalComponentRootIDs(volumes, "workspace-alpha", 2)
	if !ok {
		t.Fatal("globalComponentRootIDs declined component source")
	}
	want := []globalRecordID{
		{volume: 0, local: volumes[0].pathComponentRootIDs("workspace-alpha")[0]},
		{volume: 1, local: volumes[1].pathComponentRootIDs("workspace-alpha")[0]},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("limited global component roots = %+v, want %+v", got, want)
	}
}

func TestGlobalSubtreeIDsExpandsAndDedupesRoots(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	roots, ok := globalComponentRootIDs(volumes, "workspace-alpha", 0)
	if !ok {
		t.Fatal("globalComponentRootIDs declined component source")
	}
	roots = append(roots, roots[0])
	got, ok := globalSubtreeIDs(volumes, roots, 0)
	if !ok {
		t.Fatal("globalSubtreeIDs declined subtree source")
	}
	want := make([]globalRecordID, 0)
	seen := make(map[globalRecordID]struct{})
	for _, root := range roots {
		for _, id := range volumes[root.volume].underDescendants(root.local) {
			globalID := globalRecordID{volume: root.volume, local: id}
			if _, exists := seen[globalID]; exists {
				continue
			}
			seen[globalID] = struct{}{}
			want = append(want, globalID)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("global subtree ids = %+v, want %+v", got, want)
	}
}

func TestGlobalSubtreeIDsRespectsLimit(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	roots, ok := globalComponentRootIDs(volumes, "workspace-alpha", 0)
	if !ok {
		t.Fatal("globalComponentRootIDs declined component source")
	}
	got, ok := globalSubtreeIDs(volumes, roots, 3)
	if !ok {
		t.Fatal("globalSubtreeIDs declined subtree source")
	}
	if len(got) != 3 {
		t.Fatalf("limited global subtree ids len = %d, want 3; ids=%+v", len(got), got)
	}
}

func TestGlobalSubtreeLimitDoesNotMaterializeBroadRoot(t *testing.T) {
	vol := broadUnderTestVolume(2050)
	roots := vol.underRootIDs(`C:\broad`)
	if len(roots) != 1 {
		t.Fatalf("under roots = %v, want one broad root", roots)
	}
	got, ok := globalSubtreeIDs([]*serviceVolumeIndex{vol}, []globalRecordID{{volume: 0, local: roots[0]}}, 3)
	if !ok || len(got) != 3 {
		t.Fatalf("limited subtree = (%+v, %v), want three ids", got, ok)
	}
	if _, cached := vol.underCache[roots[0]]; cached {
		t.Fatal("limited global subtree materialized and cached the full broad root")
	}
}

func TestGlobalUnderIntersectionFiltersSelectivePostingFirst(t *testing.T) {
	vol := broadUnderTestVolume(2050)
	roots := vol.underRootIDs(`C:\broad`)
	if len(roots) != 1 {
		t.Fatalf("under roots = %v, want one broad root", roots)
	}
	pq := mustParseQuery(t, queryOptions{Query: "path:target.bin", MatchPath: true, Under: `C:\broad`, Limit: 10})
	ids, ok := globalComponentQueryIDs([]*serviceVolumeIndex{vol}, pq, &searchTrace{})
	if !ok || len(ids) != 1 || vol.index.compactRecord(ids[0].local).Name != "target.bin" {
		t.Fatalf("global under ids = (%+v, %v), want target.bin", ids, ok)
	}
	if _, cached := vol.underCache[roots[0]]; cached {
		t.Fatal("global under intersection materialized the broad subtree before filtering the selective posting")
	}
}

func TestGlobalComponentPathIDsMaterializesOnlySelectiveProbe(t *testing.T) {
	vol := broadUnderTestVolume(2050)
	ids, ok := globalComponentPathIDs([]*serviceVolumeIndex{vol}, []string{"broad", "target.bin"})
	if !ok || len(ids) != 1 || vol.index.compactRecord(ids[0].local).Name != "target.bin" {
		t.Fatalf("component ids = (%+v, %v), want target.bin", ids, ok)
	}
	if _, cached := vol.pathTermCache["broad"]; cached {
		t.Fatal("multi-term global component query materialized the broad path posting before intersection")
	}
	if _, cached := vol.underCache[1]; cached {
		t.Fatal("multi-term global component query materialized the broad directory subtree")
	}
}

func broadUnderTestVolume(children int) *serviceVolumeIndex {
	idx := &Index{Source: "usn", Volume: "C:", Compact: true}
	idx.Records = append(idx.Records,
		CompactRecord{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		CompactRecord{FRN: 2, ParentFRN: 1, Parent: 0, Name: "broad", Mode: uint32(os.ModeDir)},
	)
	for i := 0; i < children; i++ {
		name := fmt.Sprintf("noise-%04d.dat", i)
		if i == children-1 {
			name = "target.bin"
		}
		idx.Records = append(idx.Records, CompactRecord{FRN: uint64(i + 3), ParentFRN: 2, Parent: 1, Name: name})
	}
	buildOrders(idx)
	return newServiceVolumeIndex("broad-under.gsi", idx)
}

func TestGlobalIteratorSetOperations(t *testing.T) {
	left := newGlobalRecordIterator(0, []int{1, 3, 5, 9})
	right := newGlobalRecordIterator(0, []int{3, 4, 5, 10})
	if got, want := intersectGlobalIterators(&left, &right, 0), []globalRecordID{
		{volume: 0, local: 3},
		{volume: 0, local: 5},
	}; !slices.Equal(got, want) {
		t.Fatalf("intersect = %+v, want %+v", got, want)
	}

	left = newGlobalRecordIterator(0, []int{1, 3, 5})
	right = newGlobalRecordIterator(0, []int{3, 4, 5})
	if got, want := unionGlobalIterators(&left, &right, 0), []globalRecordID{
		{volume: 0, local: 1},
		{volume: 0, local: 3},
		{volume: 0, local: 4},
		{volume: 0, local: 5},
	}; !slices.Equal(got, want) {
		t.Fatalf("union = %+v, want %+v", got, want)
	}

	include := newGlobalRecordIterator(1, []int{1, 2, 3, 4})
	exclude := newGlobalRecordIterator(1, []int{2, 4})
	if got, want := excludeGlobalIterator(&include, &exclude, 0), []globalRecordID{
		{volume: 1, local: 1},
		{volume: 1, local: 3},
	}; !slices.Equal(got, want) {
		t.Fatalf("exclude = %+v, want %+v", got, want)
	}
}

func TestGlobalIteratorSetOperationsRespectVolumeAndLimit(t *testing.T) {
	left := newGlobalRecordIterator(0, []int{1, 2, 3})
	right := newGlobalRecordIterator(1, []int{1, 2, 3})
	if got := intersectGlobalIterators(&left, &right, 0); len(got) != 0 {
		t.Fatalf("cross-volume intersect = %+v, want empty", got)
	}

	left = newGlobalRecordIterator(0, []int{1, 2, 3})
	right = newGlobalRecordIterator(1, []int{1, 2, 3})
	if got, want := unionGlobalIterators(&left, &right, 4), []globalRecordID{
		{volume: 0, local: 1},
		{volume: 0, local: 2},
		{volume: 0, local: 3},
		{volume: 1, local: 1},
	}; !slices.Equal(got, want) {
		t.Fatalf("limited cross-volume union = %+v, want %+v", got, want)
	}
}

func TestCollectGlobalTopNOrdersByRankThenGlobalID(t *testing.T) {
	left := newGlobalRecordIterator(0, []int{1, 2, 3})
	right := newGlobalRecordIterator(1, []int{1, 2, 3})
	ranks := map[globalRecordID]int{
		{volume: 0, local: 1}: 50,
		{volume: 0, local: 2}: 10,
		{volume: 0, local: 3}: 20,
		{volume: 1, local: 1}: 10,
		{volume: 1, local: 2}: 40,
		{volume: 1, local: 3}: 20,
	}
	got := collectGlobalTopN([]globalIDIterator{&left, &right}, 4, func(id globalRecordID) int {
		return ranks[id]
	})
	want := []globalRecordID{
		{volume: 0, local: 2},
		{volume: 1, local: 1},
		{volume: 0, local: 3},
		{volume: 1, local: 3},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("global top-n = %+v, want %+v", got, want)
	}
}

func TestCollectGlobalTopNHandlesEmptyInputs(t *testing.T) {
	got := collectGlobalTopN(nil, 10, func(id globalRecordID) int { return 0 })
	if len(got) != 0 {
		t.Fatalf("nil iterator top-n = %+v, want empty", got)
	}
	it := newGlobalRecordIterator(0, []int{1})
	got = collectGlobalTopN([]globalIDIterator{&it}, 0, func(id globalRecordID) int { return 0 })
	if len(got) != 0 {
		t.Fatalf("zero limit top-n = %+v, want empty", got)
	}
}

func TestSortIDsByRankOrdersByRankThenID(t *testing.T) {
	ids := []int{9, 2, 8, 5, 3, 1}
	ranks := map[int]int{
		1: 10,
		2: 20,
		3: 10,
		5: 30,
		8: 20,
		9: 10,
	}
	sortIDsByRank(ids, func(id int) int {
		return ranks[id]
	})
	want := []int{1, 3, 9, 2, 8, 5}
	if !slices.Equal(ids, want) {
		t.Fatalf("sorted ids = %v, want %v", ids, want)
	}
}

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

// TestCandidatePlanMultiTermIncludesBoundedTermSources reproduces the R5
// performance gap where a loose multi-term path query like
// `Dataset trainingdata nrrd` promoted `nrrd` to an extension and drove the
// whole plan off the (huge) extension posting, verifying every other term
// against it.  The plan must instead add bounded sources for the remaining
// terms so the intersection drives off the smallest (a zero-match term makes
// the plan empty immediately).
func TestCandidatePlanMultiTermIncludesBoundedTermSources(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
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
	dataset := add(2, 1, root, "Dataset", uint32(os.ModeDir))
	datasetFRN := idx.Records[dataset].FRN
	notes := add(3, 2, dataset, "notes.txt", 0)
	_ = notes
	for i := 0; i < 40; i++ {
		add(100+uint64(i), datasetFRN, dataset, fmt.Sprintf("scan-%02d.nrrd", i), 0)
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)

	pq, err := parseQuery(queryOptions{Query: "Dataset notes nrrd", MatchPath: true, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if !pq.MatchPath {
		t.Fatal("parseQuery lost MatchPath")
	}
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		t.Fatal("buildCandidatePlan declined multi-term path query")
	}
	sawTerm := false
	for _, source := range plan.sources {
		if strings.HasPrefix(source.name, "term:") || strings.HasPrefix(source.name, "path-term:") {
			sawTerm = true
		}
	}
	if !sawTerm {
		t.Fatalf("plan sources = %+v, want a bounded term source alongside the extension posting", plan.sources)
	}
	// A zero-match required term must make the plan empty rather than
	// materialize the extension posting and verify every candidate against it.
	pqZero, err := parseQuery(queryOptions{Query: "Dataset zzz-missing-keyword nrrd", MatchPath: true, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	planZero, okZero := vol.buildCandidatePlan(pqZero)
	if !okZero {
		t.Fatal("buildCandidatePlan declined multi-term query with a zero-match term")
	}
	if !planZero.empty {
		t.Fatalf("plan sources = %+v, want empty plan because a required term has zero matches", planZero.sources)
	}
	if got := planZero.execute(); len(got) != 0 {
		t.Fatalf("empty plan executed to %v, want zero candidates", got)
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
	if trace.Source != "planned:ext:nrrd" && trace.Source != "path-directory-term-top" &&
		trace.Source != "planned:ext:nrrd+path-term:downloads" && trace.Source != "planned:empty" {
		t.Fatalf("source = %q, want planned:ext:nrrd, path-directory-term-top, or the bounded plan route", trace.Source)
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
	if trace.Source != "path-bare-extension-multi-top" && trace.Source != "path-directory-term-top" &&
		trace.Source != "planned:ext:nrrd+path-term:downloads" && trace.Source != "planned:empty" {
		t.Fatalf("source = %q, want path-bare-extension-multi-top, path-directory-term-top, or the bounded plan route", trace.Source)
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
		wantTerms  []traceTerm
	}{
		{
			name:       "dotted path extension",
			opts:       queryOptions{Query: "path:.nrrd", Limit: 20},
			wantSource: "planned:ext-top",
			wantTerms:  []traceTerm{{Term: "nrrd", Kind: "extension", Source: "planned:ext-top", Exact: true}},
		},
		{
			name:       "extension planner",
			opts:       queryOptions{Query: "ext:.pdf", MatchPath: true, Limit: 20},
			wantSource: "planned:ext:pdf",
			wantTerms:  []traceTerm{{Term: "pdf", Kind: "extension", Source: "ext:pdf", Exact: true}},
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
			wantTerms: []traceTerm{
				{Term: "plain", Kind: "path-substring", Source: "path-term:plain", Exact: false},
				{Term: "workspace", Kind: "path-substring", Source: "path-term:workspace", Exact: false},
			},
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
			for _, want := range tc.wantTerms {
				if !traceHasTerm(trace.Terms, want) {
					t.Fatalf("trace terms = %+v, missing %+v", trace.Terms, want)
				}
			}
		})
	}
}

func traceHasTerm(terms []traceTerm, want traceTerm) bool {
	for _, term := range terms {
		if term.Term == want.Term && term.Kind == want.Kind && term.Source == want.Source && term.Exact == want.Exact {
			return true
		}
	}
	return false
}

func TestSearchTraceRecordsDeclineList(t *testing.T) {
	trace := &searchTrace{}
	trace.setDecline("component-trigram:unsupported-query")
	trace.replaceDecline("global-components:missing-source")
	trace.addDeclineForVolume("global-ext:missing-posting", "F:")

	if trace.Decline != "global-ext:missing-posting" {
		t.Fatalf("decline = %q, want latest decline", trace.Decline)
	}
	if len(trace.Declines) != 3 {
		t.Fatalf("declines = %+v, want three entries", trace.Declines)
	}
	if trace.Declines[0].Source != "component-trigram" || trace.Declines[0].Reason != "unsupported-query" {
		t.Fatalf("first decline = %+v", trace.Declines[0])
	}
	if trace.Declines[1].Source != "global-components" || trace.Declines[1].Reason != "missing-source" {
		t.Fatalf("second decline = %+v", trace.Declines[1])
	}
	if trace.Declines[2].Source != "global-ext" || trace.Declines[2].Reason != "missing-posting" || trace.Declines[2].Volume != "F:" {
		t.Fatalf("third decline = %+v", trace.Declines[2])
	}
}

func TestServiceResponseJSONIncludesStructuredTraceFields(t *testing.T) {
	complete := true
	resp := serviceResponse{
		OK:              true,
		Count:           2,
		SearchMS:        1.25,
		Source:          "global:components",
		PlannerMode:     "global-components",
		EligibleVolumes: []string{"C:", "F:"},
		Terms: []traceTerm{
			{Term: "workspace-alpha", Kind: "path-substring", Source: "global:component-subtree", CountHint: 16},
			{Term: "file", Kind: "type", Source: "global:type", CountHint: 16, Exact: true},
		},
		Declines: []traceDecline{
			{Source: "global-ext", Reason: "missing-posting", Volume: "F:"},
		},
		Fallback: "global-bounded-scan",
		Complete: &complete,
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var decoded serviceResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.PlannerMode != resp.PlannerMode || decoded.Source != resp.Source || decoded.Fallback != resp.Fallback {
		t.Fatalf("decoded trace route fields = %+v", decoded)
	}
	if !slices.Equal(decoded.EligibleVolumes, resp.EligibleVolumes) {
		t.Fatalf("eligible volumes = %v, want %v", decoded.EligibleVolumes, resp.EligibleVolumes)
	}
	if decoded.Complete == nil || !*decoded.Complete {
		t.Fatalf("complete = %v, want true pointer", decoded.Complete)
	}
	if !traceHasTerm(decoded.Terms, traceTerm{Term: "workspace-alpha", Kind: "path-substring", Source: "global:component-subtree"}) {
		t.Fatalf("decoded terms = %+v, missing component-subtree term", decoded.Terms)
	}
	if len(decoded.Declines) != 1 || decoded.Declines[0].Source != "global-ext" || decoded.Declines[0].Reason != "missing-posting" || decoded.Declines[0].Volume != "F:" {
		t.Fatalf("decoded declines = %+v", decoded.Declines)
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
	for _, parent := range pq.Parents {
		fields = append(fields, "parent:"+parent)
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

func TestServiceVolumesPathModePrioritizesFullComponentVolume(t *testing.T) {
	cVol := workspaceAlphaModelVolume("C:", false)
	fVol := workspaceAlphaModelVolume("F:", true)
	opts := queryOptions{Query: "workspace-alpha model_v2 type:file", MatchPath: true, Limit: 10}

	volumes := prioritizeServiceVolumesForPathTerms([]*serviceVolumeIndex{cVol, fVol}, opts)
	if len(volumes) != 2 || volumes[0] != fVol {
		t.Fatalf("prioritized volumes = [%s %s], want F: before partial C:", volumes[0].volume, volumes[1].volume)
	}
}

func TestServiceVolumesPathModeLaterFullVolumeSearchCountParity(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	trace := &searchTrace{}
	opts := queryOptions{Query: "path:workspace-alpha model_v2 type:file", Limit: 10, Trace: trace}

	got, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components", trace.PlannerMode)
	}
	if want := []string{"F:", "C:"}; !slices.Equal(trace.EligibleVolumes, want) {
		t.Fatalf("eligible volumes = %v, want %v", trace.EligibleVolumes, want)
	}
	want := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		want = append(want, fmt.Sprintf(`F:\project-%02d\workspace-alpha\model_v2\target-model-%02d.bin`, i, i))
	}
	if paths := pathsOf(got); !sameOrderedStrings(paths, want) {
		t.Fatalf("search paths = %v, want %v", paths, want)
	}

	countTrace := &searchTrace{}
	countOpts := opts
	countOpts.Trace = countTrace
	countMatches, err := searchServiceVolumes(volumes, countOpts, true)
	if err != nil {
		t.Fatal(err)
	}
	if countTrace.PlannerMode != "global-components" {
		t.Fatalf("count planner mode = %q, want global-components", countTrace.PlannerMode)
	}
	if want := []string{"F:", "C:"}; !slices.Equal(countTrace.EligibleVolumes, want) {
		t.Fatalf("count eligible volumes = %v, want %v", countTrace.EligibleVolumes, want)
	}
	if gotCount := len(countMatches); gotCount != len(got) {
		t.Fatalf("count/search parity = %d/%d, want equal", gotCount, len(got))
	}

	fastCountTrace := &searchTrace{}
	fastCountOpts := opts
	fastCountOpts.Trace = fastCountTrace
	fastCount, ok, err := countServiceVolumes(volumes, fastCountOpts)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("countServiceVolumes declined exact fast count")
	}
	if fastCount != len(got) {
		t.Fatalf("fast count/search parity = %d/%d, want equal", fastCount, len(got))
	}
	if fastCountTrace.PlannerMode != "global-count-components" {
		t.Fatalf("fast count planner mode = %q, want global-count-components", fastCountTrace.PlannerMode)
	}
	if want := []string{"F:", "C:"}; !slices.Equal(fastCountTrace.EligibleVolumes, want) {
		t.Fatalf("fast count eligible volumes = %v, want %v", fastCountTrace.EligibleVolumes, want)
	}
}

func TestServiceVolumesPathModeExpiredDeadlineDoesNotReturnCompleteZero(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	opts := queryOptions{
		Query:        "workspace-alpha model_v2 type:file",
		MatchPath:    true,
		Limit:        10,
		DeadlineUnix: time.Now().Add(-time.Second).UnixNano(),
	}

	got, err := searchServiceVolumes(volumes, opts, false)
	if !errors.Is(err, errQueryCanceled) {
		t.Fatalf("searchServiceVolumes err = %v, want %v; results=%v", err, errQueryCanceled, pathsOf(got))
	}
}

func TestGlobalPlannerExtOnlyMatchesServiceVolumes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		query  string
		source string
	}{
		{name: "ext", query: "ext:bin", source: "global:ext:bin"},
		{name: "simple-glob", query: "glob:*.bin", source: "global:glob-ext:bin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			volumes := []*serviceVolumeIndex{
				workspaceAlphaModelVolume("C:", false),
				workspaceAlphaModelVolume("F:", true),
			}
			opts := queryOptions{Query: tc.query, Limit: 10}

			want, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			wantCount, ok, err := countServiceVolumes(volumes, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("legacy ext count declined")
			}
			trace := &searchTrace{}
			globalOpts := opts
			globalOpts.Trace = trace
			got, err := searchServiceVolumes(volumes, globalOpts, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != "global-ext" {
				t.Fatalf("planner mode = %q, want global-ext", trace.PlannerMode)
			}
			if trace.Source != tc.source {
				t.Fatalf("trace source = %q, want %s", trace.Source, tc.source)
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
				t.Fatalf("global ext paths = %v, want %v", gotPaths, wantPaths)
			}
			countSearchTrace := &searchTrace{}
			countSearchOpts := opts
			countSearchOpts.Trace = countSearchTrace
			countMatches, err := searchServiceVolumes(volumes, countSearchOpts, true)
			if err != nil {
				t.Fatal(err)
			}
			if countSearchTrace.PlannerMode != "global-ext" {
				t.Fatalf("count-search planner mode = %q, want global-ext", countSearchTrace.PlannerMode)
			}
			if len(countMatches) != wantCount {
				t.Fatalf("global ext count-search len = %d, want %d", len(countMatches), wantCount)
			}
			countTrace := &searchTrace{}
			countOpts := opts
			countOpts.Trace = countTrace
			count, ok, err := countServiceVolumes(volumes, countOpts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("global ext count declined")
			}
			if countTrace.PlannerMode != "global-count-ext" {
				t.Fatalf("count planner mode = %q, want global-count-ext", countTrace.PlannerMode)
			}
			if count != wantCount {
				t.Fatalf("global ext count = %d, want %d", count, wantCount)
			}
		})
	}
}

func TestGlobalPlannerExtTypeFileDeclinesUnsafeTopN(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("c-ext-type.gsi", singleEntryKindCompactIndex("C:", "foo.bin", uint32(os.ModeDir))),
		newServiceVolumeIndex("f-ext-type.gsi", singleEntryKindCompactIndex("F:", "bar.bin", 0)),
	}
	trace := &searchTrace{}
	opts := queryOptions{Query: "type:file ext:bin", Limit: 1, Trace: trace}
	got, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode == "global-ext" || trace.PlannerMode == "global-count-ext" {
		t.Fatalf("planner mode = %q, want safe fallback for type:file ext", trace.PlannerMode)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`F:\workspace\bar.bin`}) {
		t.Fatalf("paths = %v, want only file result", gotPaths)
	}
	countTrace := &searchTrace{}
	count, ok, err := countServiceVolumes(volumes, queryOptions{Query: "type:file ext:bin", Trace: countTrace})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("countServiceVolumes declined type:file ext:bin")
	}
	if countTrace.PlannerMode != "global-count-components" {
		t.Fatalf("count planner mode = %q, want global-count-components; decline=%s", countTrace.PlannerMode, countTrace.Decline)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestGlobalPlannerExtDeclinesCaseSensitiveShortcut(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("c-case-ext.gsi", singleFileCompactIndex("C:", "case.txt")),
		newServiceVolumeIndex("f-case-ext.gsi", singleFileCompactIndex("F:", "case.TXT")),
	}
	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "case: ext:TXT", Limit: 10, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode == "global-ext" || trace.PlannerMode == "global-count-ext" {
		t.Fatalf("planner mode = %q, want verified fallback for case-sensitive ext", trace.PlannerMode)
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`F:\workspace\case.TXT`}) {
		t.Fatalf("paths = %v, want only exact-case TXT extension", gotPaths)
	}
}

func TestGlobalPlannerExtSortPathUsesGlobalPathOrder(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("F:", true),
		workspaceAlphaModelVolume("C:", true),
	}
	trace := &searchTrace{}
	opts := queryOptions{Query: "ext:bin sort:path", Limit: 3, Trace: trace}
	got, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-ext" {
		t.Fatalf("planner mode = %q, want global-ext; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
	}
	paths := pathsOf(got)
	if len(paths) != 3 {
		t.Fatalf("paths = %v, want 3 results", paths)
	}
	for _, path := range paths {
		if !strings.HasPrefix(path, `C:\`) {
			t.Fatalf("global sort:path paths = %v, want C: paths before F: despite reversed volume order", paths)
		}
	}
}

func TestGlobalPlannerExtDefaultTopNUsesGlobalNameOrder(t *testing.T) {
	makeVolume := func(volume, match string, earlier int) *serviceVolumeIndex {
		idx := &Index{
			Source:  "usn",
			Volume:  volume,
			Compact: true,
			Records: []CompactRecord{{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}},
		}
		for i := 0; i < earlier; i++ {
			idx.Records = append(idx.Records, CompactRecord{FRN: uint64(2 + i), ParentFRN: 1, Parent: 0, Name: fmt.Sprintf("a-%03d.go", i)})
		}
		idx.Records = append(idx.Records, CompactRecord{FRN: uint64(2 + earlier), ParentFRN: 1, Parent: 0, Name: match})
		buildOrders(idx)
		return newServiceVolumeIndex(volume+"-ext-global-rank.gsi", idx)
	}
	volumes := []*serviceVolumeIndex{
		makeVolume("C:", "b-target.txt", 100),
		makeVolume("F:", "z-target.txt", 0),
	}
	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "ext:txt", Limit: 1, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-ext" {
		t.Fatalf("planner mode = %q, want global-ext; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`C:\b-target.txt`}) {
		t.Fatalf("paths = %v, want globally first name despite worse local rank", gotPaths)
	}
}

func TestGlobalPlannerExtNonPathSortsUseGlobalOrder(t *testing.T) {
	makeVolume := func(volume, firstName, secondName string, firstMode, secondMode uint32, firstSize, secondSize int64, firstMod, secondMod int64) *serviceVolumeIndex {
		idx := &Index{
			Source:  "usn",
			Volume:  volume,
			Compact: true,
		}
		add := func(frn, parentFRN uint64, parent int32, name string, mode uint32, size int64, mod int64) int32 {
			idx.Records = append(idx.Records, CompactRecord{
				FRN:       frn,
				ParentFRN: parentFRN,
				Parent:    parent,
				Name:      name,
				Mode:      mode,
				Size:      size,
				ModUnix:   mod,
			})
			return int32(len(idx.Records) - 1)
		}
		root := add(1, 1, -1, ".", uint32(os.ModeDir), 0, 1)
		add(2, 1, root, firstName, firstMode, firstSize, firstMod)
		add(3, 1, root, secondName, secondMode, secondSize, secondMod)
		buildOrders(idx)
		return newServiceVolumeIndex(strings.ToLower(strings.TrimSuffix(volume, ":"))+"-sort.gsi", idx)
	}
	volumes := []*serviceVolumeIndex{
		makeVolume("C:", "z-large.txt", "folder.txt", 0, uint32(os.ModeDir), 900, 200, 20, 10),
		makeVolume("F:", "a-small.txt", "newest.txt", 0, 0, 5, 100, 30, 100),
	}
	cases := []struct {
		query string
		want  string
	}{
		{query: "ext:txt sort:size", want: `F:\a-small.txt`},
		{query: "ext:txt sort:modified", want: `F:\newest.txt`},
		{query: "ext:txt sort:extension", want: `F:\a-small.txt`},
		{query: "ext:txt sort:type", want: `C:\folder.txt`},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			trace := &searchTrace{}
			got, err := searchServiceVolumes(volumes, queryOptions{Query: tc.query, Limit: 1, Trace: trace}, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != "global-ext" {
				t.Fatalf("planner mode = %q, want global-ext; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
			}
			if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{tc.want}) {
				t.Fatalf("paths = %v, want %s", gotPaths, tc.want)
			}
		})
	}
}

func TestGlobalPlannerComponentNonPathSortsUseGlobalOrder(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		componentSortVolume("C:", []componentSortFixtureEntry{
			{name: "target-large.zzz", size: 900, modUnix: 100},
			{name: "target-dir", mode: uint32(os.ModeDir), size: 0, modUnix: 50},
			{name: "target-new.bin", size: 100, modUnix: 300},
		}),
		componentSortVolume("F:", []componentSortFixtureEntry{
			{name: "target-small.bin", size: 5, modUnix: 200},
			{name: "target-alpha.aaa", size: 20, modUnix: 150},
		}),
	}
	cases := []struct {
		query string
		want  string
		count int
	}{
		{query: "path:workspace-alpha target type:file sort:size", want: `F:\project\workspace-alpha\model_v2\target-small.bin`, count: 4},
		{query: "path:workspace-alpha target type:file sort:modified", want: `C:\project\workspace-alpha\model_v2\target-new.bin`, count: 4},
		{query: "path:workspace-alpha target type:file sort:extension", want: `F:\project\workspace-alpha\model_v2\target-alpha.aaa`, count: 4},
		{query: "path:workspace-alpha target sort:type", want: `C:\project\workspace-alpha\model_v2\target-dir`, count: 5},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			trace := &searchTrace{}
			got, err := searchServiceVolumes(volumes, queryOptions{Query: tc.query, Limit: 1, Trace: trace}, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != "global-components" {
				t.Fatalf("planner mode = %q, want global-components; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
			}
			if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{tc.want}) {
				t.Fatalf("paths = %v, want %s", gotPaths, tc.want)
			}
			countTrace := &searchTrace{}
			count, ok, err := countServiceVolumes(volumes, queryOptions{Query: tc.query, Trace: countTrace})
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("countServiceVolumes declined component sort query")
			}
			if countTrace.PlannerMode != "global-count-components" {
				t.Fatalf("count planner mode = %q, want global-count-components; decline=%s", countTrace.PlannerMode, countTrace.Decline)
			}
			if count != tc.count {
				t.Fatalf("count = %d, want %d", count, tc.count)
			}
		})
	}
}

func singleEntryKindCompactIndex(volume, name string, mode uint32) *Index {
	idx := singleFileCompactIndex(volume, name)
	idx.Records[len(idx.Records)-1].Mode = mode
	buildOrders(idx)
	return idx
}

func TestGlobalPlannerOverlayExtMatchesServiceVolumes(t *testing.T) {
	vol := engineV9OverlaySearchTestVolume(t)
	vol.applyUSNChanges([]usnChange{
		{FRN: 101, USN: 10, Reason: usnReasonFileDelete},
		{FRN: 301, ParentFRN: 100, USN: 11, Reason: usnReasonFileCreate, Name: "aaa-overlay.txt"},
	})
	opts := queryOptions{Query: "ext:txt", Limit: 10}
	want, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	wantCount, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("legacy overlay ext count declined")
	}

	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	trace := &searchTrace{}
	globalOpts := opts
	globalOpts.Trace = trace
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, globalOpts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-ext" {
		t.Fatalf("planner mode = %q, want global-ext; decline=%s", trace.PlannerMode, trace.Decline)
	}
	if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
		t.Fatalf("global overlay ext paths = %v, want %v", gotPaths, wantPaths)
	}
	countTrace := &searchTrace{}
	countOpts := opts
	countOpts.Trace = countTrace
	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, countOpts)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("global overlay ext count declined")
	}
	if countTrace.PlannerMode != "global-count-ext" {
		t.Fatalf("count planner mode = %q, want global-count-ext; decline=%s", countTrace.PlannerMode, countTrace.Decline)
	}
	if count != wantCount {
		t.Fatalf("global overlay ext count = %d, want %d", count, wantCount)
	}
}

func TestGlobalPlannerComplexGlobFallsBack(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	opts := queryOptions{Query: "glob:target-*.bin", Limit: 10}
	wantPaths := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		wantPaths = append(wantPaths, fmt.Sprintf(`F:\project-%02d\workspace-alpha\model_v2\target-model-%02d.bin`, i, i))
	}
	trace := &searchTrace{}
	globalOpts := opts
	globalOpts.Trace = trace
	got, err := searchServiceVolumes(volumes, globalOpts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-bounded-scan" {
		t.Fatalf("planner mode = %q, want global-bounded-scan; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
	}
	if trace.Fallback != "global-bounded-scan" {
		t.Fatalf("fallback = %q, want global-bounded-scan; decline=%s declines=%+v", trace.Fallback, trace.Decline, trace.Declines)
	}
	if gotPaths := pathsOf(got); !slices.Equal(gotPaths, wantPaths) {
		t.Fatalf("fallback glob paths = %v, want %v", gotPaths, wantPaths)
	}
	countTrace := &searchTrace{}
	countOpts := opts
	countOpts.Trace = countTrace
	count, ok, err := countServiceVolumes(volumes, countOpts)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("global bounded fallback count declined")
	}
	if countTrace.PlannerMode != "global-bounded-scan" {
		t.Fatalf("count planner mode = %q, want global-bounded-scan", countTrace.PlannerMode)
	}
	if countTrace.Source != "global:bounded-scan" {
		t.Fatalf("count source = %q, want global:bounded-scan", countTrace.Source)
	}
	if countTrace.Fallback != "global-bounded-scan" {
		t.Fatalf("count fallback = %q, want global-bounded-scan", countTrace.Fallback)
	}
	if countTrace.Complete == nil || !*countTrace.Complete {
		t.Fatalf("count complete = %v, want true", countTrace.Complete)
	}
	if countTrace.Candidates == 0 {
		t.Fatalf("count candidates = %d, want populated", countTrace.Candidates)
	}
	if count != len(wantPaths) {
		t.Fatalf("global bounded fallback count = %d, want %d", count, len(wantPaths))
	}
}

func TestGlobalPlannerCompleteFallbackDefaultsForRemainingFamilies(t *testing.T) {
	makeVolume := func(volume, match string, earlier int) *serviceVolumeIndex {
		idx := &Index{
			Source:  "usn",
			Volume:  volume,
			Compact: true,
			Records: []CompactRecord{{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}},
		}
		for i := 0; i < earlier; i++ {
			idx.Records = append(idx.Records, CompactRecord{FRN: uint64(2 + i), ParentFRN: 1, Parent: 0, Name: fmt.Sprintf("a-%03d.go", i)})
		}
		idx.Records = append(idx.Records, CompactRecord{FRN: uint64(2 + earlier), ParentFRN: 1, Parent: 0, Name: match})
		buildOrders(idx)
		return newServiceVolumeIndex(volume+"-complete-fallback.gsi", idx)
	}
	volumes := []*serviceVolumeIndex{
		makeVolume("C:", "b-needle.txt", 100),
		makeVolume("F:", "z-needle.txt", 0),
	}

	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "needle", Limit: 1, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-bounded-scan" || trace.Complete == nil || !*trace.Complete {
		t.Fatalf("trace = %+v, want complete global fallback", trace)
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`C:\b-needle.txt`}) {
		t.Fatalf("paths = %v, want globally first remaining-family match", gotPaths)
	}
	countTrace := &searchTrace{}
	count, ok, err := countServiceVolumes(volumes, queryOptions{Query: "needle", Trace: countTrace})
	if err != nil || !ok || count != 2 {
		t.Fatalf("count = %d handled=%v err=%v, want 2 true nil", count, ok, err)
	}
	if countTrace.PlannerMode != "global-bounded-scan" {
		t.Fatalf("count planner mode = %q, want global-bounded-scan", countTrace.PlannerMode)
	}

	for _, opts := range []queryOptions{
		{Query: "x|needle", Limit: 1},
		{Query: "case: regex:.*needle.*", Limit: 1},
		{Query: "type:file", Exists: true, Limit: 1},
	} {
		routeTrace := &searchTrace{}
		opts.Trace = routeTrace
		if _, err := searchServiceVolumes(volumes, opts, false); err != nil {
			t.Fatalf("query %q: %v", opts.Query, err)
		}
		if routeTrace.PlannerMode != "global-bounded-scan" {
			t.Fatalf("query %q planner mode = %q, want global-bounded-scan; decline=%s", opts.Query, routeTrace.PlannerMode, routeTrace.Decline)
		}
	}
}

func TestGlobalPlannerComponentPathMatchesServiceVolumes(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	opts := queryOptions{Query: "path:workspace-alpha model_v2", Limit: 10}

	wantPaths := make([]string, 0, 16)
	for i := 0; i < 8; i++ {
		wantPaths = append(wantPaths, fmt.Sprintf(`F:\project-%02d\workspace-alpha\model_v2`, i))
	}
	for i := 0; i < 8; i++ {
		wantPaths = append(wantPaths, fmt.Sprintf(`F:\project-%02d\workspace-alpha\model_v2\target-model-%02d.bin`, i, i))
	}
	trace := &searchTrace{}
	globalOpts := opts
	globalOpts.Trace = trace
	got, err := searchServiceVolumes(volumes, globalOpts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components", trace.PlannerMode)
	}
	if trace.Source != "global:components" {
		t.Fatalf("trace source = %q, want global:components", trace.Source)
	}
	for _, want := range []traceTerm{
		{Term: "workspace-alpha", Kind: "path-substring", Source: "global:component-subtree", Exact: false},
		{Term: "model_v2", Kind: "path-substring", Source: "global:component-subtree", Exact: false},
	} {
		if !traceHasTerm(trace.Terms, want) {
			t.Fatalf("trace terms = %+v, missing %+v", trace.Terms, want)
		}
	}
	if gotPaths := pathsOf(got); !slices.Equal(gotPaths, wantPaths[:len(gotPaths)]) {
		t.Fatalf("global component paths = %v, want prefix %v", gotPaths, wantPaths[:len(gotPaths)])
	}
	countSearchTrace := &searchTrace{}
	countSearchOpts := opts
	countSearchOpts.Trace = countSearchTrace
	countMatches, err := searchServiceVolumes(volumes, countSearchOpts, true)
	if err != nil {
		t.Fatal(err)
	}
	if countSearchTrace.PlannerMode != "global-components" {
		t.Fatalf("count-search planner mode = %q, want global-components", countSearchTrace.PlannerMode)
	}
	if len(countMatches) != len(wantPaths) {
		t.Fatalf("global component count-search len = %d, want %d", len(countMatches), len(wantPaths))
	}
	countTrace := &searchTrace{}
	countOpts := opts
	countOpts.Trace = countTrace
	count, ok, err := countServiceVolumes(volumes, countOpts)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("global component count declined")
	}
	if countTrace.PlannerMode != "global-count-components" {
		t.Fatalf("count planner mode = %q, want global-count-components", countTrace.PlannerMode)
	}
	if count != len(wantPaths) {
		t.Fatalf("global component count = %d, want %d", count, len(wantPaths))
	}
}

func TestGlobalPlannerComponentSortPathUsesGlobalPathOrder(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("F:", true),
		workspaceAlphaModelVolume("C:", true),
	}
	trace := &searchTrace{}
	opts := queryOptions{Query: "path:workspace-alpha model_v2 sort:path", Limit: 3, Trace: trace}
	got, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
	}
	paths := pathsOf(got)
	if len(paths) != 3 {
		t.Fatalf("paths = %v, want 3 results", paths)
	}
	for _, path := range paths {
		if !strings.HasPrefix(path, `C:\`) {
			t.Fatalf("global component sort:path paths = %v, want C: paths before F: despite reversed volume order", paths)
		}
	}
}

func TestGlobalPlannerOverlayComponentMatchesServiceVolumes(t *testing.T) {
	vol := engineV9OverlaySearchTestVolume(t)
	vol.applyUSNChanges([]usnChange{{
		FRN:       301,
		ParentFRN: 200,
		USN:       11,
		Reason:    usnReasonFileCreate,
		Name:      "needle-overlay.bin",
	}})
	opts := queryOptions{Query: "path:base-parent needle-overlay", MatchPath: true, Limit: 10}
	want, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	wantCount, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		countMatches, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, true)
		if err != nil {
			t.Fatal(err)
		}
		wantCount = len(countMatches)
	}

	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	trace := &searchTrace{}
	globalOpts := opts
	globalOpts.Trace = trace
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, globalOpts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components; decline=%s", trace.PlannerMode, trace.Decline)
	}
	if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
		t.Fatalf("global overlay component paths = %v, want %v", gotPaths, wantPaths)
	}
	countTrace := &searchTrace{}
	countOpts := opts
	countOpts.Trace = countTrace
	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, countOpts)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("global overlay component count declined")
	}
	if countTrace.PlannerMode != "global-count-components" {
		t.Fatalf("count planner mode = %q, want global-count-components; decline=%s", countTrace.PlannerMode, countTrace.Decline)
	}
	if count != wantCount {
		t.Fatalf("global overlay component count = %d, want %d", count, wantCount)
	}
}

func TestGlobalPlannerOverlayDirectoryRenameUpdatesComponentDescendants(t *testing.T) {
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	vol := engineV9OverlaySearchTestVolume(t)
	vol.applyUSNChanges([]usnChange{
		{FRN: 301, ParentFRN: 100, USN: 11, Reason: usnReasonFileCreate, Name: "staging", Attr: fileAttributeDir},
		{FRN: 302, ParentFRN: 301, USN: 12, Reason: usnReasonFileCreate, Name: "model_v2", Attr: fileAttributeDir},
		{FRN: 303, ParentFRN: 302, USN: 13, Reason: usnReasonFileCreate, Name: "result.bin"},
		{FRN: 301, ParentFRN: 100, USN: 14, Reason: usnReasonRenameOld, Name: "staging", Attr: fileAttributeDir},
		{FRN: 301, ParentFRN: 100, USN: 15, Reason: usnReasonRenameNew, Name: "workspace-alpha", Attr: fileAttributeDir},
	})

	assertMatches := func(want []string) {
		t.Helper()
		opts := queryOptions{Query: "path:workspace-alpha model_v2 type:file", Limit: 10}
		trace := &searchTrace{}
		opts.Trace = trace
		got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
		if err != nil {
			t.Fatal(err)
		}
		if trace.PlannerMode != "global-components" {
			t.Fatalf("planner mode = %q, want global-components; decline=%s", trace.PlannerMode, trace.Decline)
		}
		if gotPaths := pathsOf(got); !slices.Equal(gotPaths, want) {
			t.Fatalf("renamed overlay component paths = %v, want %v", gotPaths, want)
		}
		countTrace := &searchTrace{}
		opts.Trace = countTrace
		count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, opts)
		if err != nil || !ok {
			t.Fatalf("renamed overlay component count handled=%v err=%v", ok, err)
		}
		if countTrace.PlannerMode != "global-count-components" {
			t.Fatalf("count planner mode = %q, want global-count-components; decline=%s", countTrace.PlannerMode, countTrace.Decline)
		}
		if count != len(want) {
			t.Fatalf("renamed overlay component count = %d, want %d", count, len(want))
		}
	}

	assertMatches([]string{`F:\workspace-alpha\model_v2\result.bin`})
	vol.applyUSNChanges([]usnChange{
		{FRN: 301, ParentFRN: 100, USN: 16, Reason: usnReasonRenameOld, Name: "workspace-alpha", Attr: fileAttributeDir},
		{FRN: 301, ParentFRN: 100, USN: 17, Reason: usnReasonRenameNew, Name: "archive", Attr: fileAttributeDir},
	})
	assertMatches(nil)
}

func TestGlobalPlannerOverlayParentMatchesServiceVolumes(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		changes []usnChange
	}{
		{
			name:  "overlay-child-under-base-parent",
			query: "parent:base-parent",
			changes: []usnChange{{
				FRN:       301,
				ParentFRN: 200,
				USN:       11,
				Reason:    usnReasonFileCreate,
				Name:      "overlay-child.txt",
			}},
		},
		{
			name:  "overlay-child-under-overlay-parent",
			query: "parent:overlay-parent",
			changes: []usnChange{
				{FRN: 301, ParentFRN: 100, USN: 11, Reason: usnReasonFileCreate, Name: "overlay-parent", Attr: fileAttributeDir},
				{FRN: 302, ParentFRN: 301, USN: 12, Reason: usnReasonFileCreate, Name: "overlay-child.txt"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vol := engineV9OverlaySearchTestVolume(t)
			vol.applyUSNChanges(tc.changes)
			opts := queryOptions{Query: tc.query, Limit: 10}
			want, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			wantCount, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				countMatches, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, true)
				if err != nil {
					t.Fatal(err)
				}
				wantCount = len(countMatches)
			}

			t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
			trace := &searchTrace{}
			globalOpts := opts
			globalOpts.Trace = trace
			got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, globalOpts, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != "global-components" {
				t.Fatalf("planner mode = %q, want global-components; decline=%s", trace.PlannerMode, trace.Decline)
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
				t.Fatalf("global overlay parent paths = %v, want %v", gotPaths, wantPaths)
			}
			countTrace := &searchTrace{}
			countOpts := opts
			countOpts.Trace = countTrace
			count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, countOpts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("global overlay parent count declined")
			}
			if countTrace.PlannerMode != "global-count-components" {
				t.Fatalf("count planner mode = %q, want global-count-components; decline=%s", countTrace.PlannerMode, countTrace.Decline)
			}
			if count != wantCount {
				t.Fatalf("global overlay parent count = %d, want %d", count, wantCount)
			}
		})
	}
}

func TestGlobalPlannerComponentOrNotMatchesServiceVolumes(t *testing.T) {
	cases := []string{
		"path:workspace-alpha model_v2|alpha-notes",
		"path:workspace-alpha !model_v2",
		"path:workspace-alpha parent:model_v2|parent:workspace-alpha sort:path",
		"path:workspace-alpha !parent:model_v2 sort:path",
	}
	for _, query := range cases {
		t.Run(query, func(t *testing.T) {
			volumes := []*serviceVolumeIndex{
				workspaceAlphaModelVolume("C:", false),
				workspaceAlphaModelVolume("F:", true),
			}
			opts := queryOptions{Query: query, Limit: 20}
			want, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			wantCount, ok, err := countServiceVolumes(volumes, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("legacy component count declined")
			}

			t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
			trace := &searchTrace{}
			globalOpts := opts
			globalOpts.Trace = trace
			got, err := searchServiceVolumes(volumes, globalOpts, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != "global-components" {
				t.Fatalf("planner mode = %q, want global-components", trace.PlannerMode)
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
				t.Fatalf("global component paths = %v, want %v", gotPaths, wantPaths)
			}

			countTrace := &searchTrace{}
			countOpts := opts
			countOpts.Trace = countTrace
			count, ok, err := countServiceVolumes(volumes, countOpts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("global component count declined")
			}
			if countTrace.PlannerMode != "global-count-components" {
				t.Fatalf("count planner mode = %q, want global-count-components", countTrace.PlannerMode)
			}
			if count != wantCount {
				t.Fatalf("global component count = %d, want %d", count, wantCount)
			}
		})
	}
}

func TestGlobalPlannerComponentTypeFilterMatchesServiceVolumes(t *testing.T) {
	cases := []string{
		"path:workspace-alpha model_v2 type:file",
		"path:workspace-alpha model_v2 type:dir",
	}
	for _, query := range cases {
		t.Run(query, func(t *testing.T) {
			volumes := []*serviceVolumeIndex{
				workspaceAlphaModelVolume("C:", false),
				workspaceAlphaModelVolume("F:", true),
			}
			opts := queryOptions{Query: query, Limit: 5}
			want, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			wantCount, ok, err := countServiceVolumes(volumes, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("legacy component count declined")
			}

			t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
			trace := &searchTrace{}
			globalOpts := opts
			globalOpts.Trace = trace
			got, err := searchServiceVolumes(volumes, globalOpts, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != "global-components" {
				t.Fatalf("planner mode = %q, want global-components", trace.PlannerMode)
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
				t.Fatalf("global component paths = %v, want %v", gotPaths, wantPaths)
			}

			countTrace := &searchTrace{}
			countOpts := opts
			countOpts.Trace = countTrace
			count, ok, err := countServiceVolumes(volumes, countOpts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("global component count declined")
			}
			if countTrace.PlannerMode != "global-count-components" {
				t.Fatalf("count planner mode = %q, want global-count-components", countTrace.PlannerMode)
			}
			if count != wantCount {
				t.Fatalf("global component count = %d, want %d", count, wantCount)
			}
		})
	}
}

func TestGlobalPlannerComponentExtMatchesServiceVolumes(t *testing.T) {
	for _, query := range []string{"path:workspace-alpha model_v2 ext:bin", "path:workspace-alpha model_v2 glob:*.bin"} {
		t.Run(query, func(t *testing.T) {
			volumes := []*serviceVolumeIndex{
				workspaceAlphaModelVolume("C:", false),
				workspaceAlphaModelVolume("F:", true),
			}
			opts := queryOptions{Query: query, Limit: 5}
			want, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			wantCount, ok, err := countServiceVolumes(volumes, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				countMatches, err := searchServiceVolumes(volumes, opts, true)
				if err != nil {
					t.Fatal(err)
				}
				wantCount = len(countMatches)
			}

			t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
			trace := &searchTrace{}
			globalOpts := opts
			globalOpts.Trace = trace
			got, err := searchServiceVolumes(volumes, globalOpts, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != "global-components" {
				t.Fatalf("planner mode = %q, want global-components", trace.PlannerMode)
			}
			extSource := "global:ext:bin"
			if strings.Contains(query, "glob:") {
				extSource = "global:glob-ext:bin"
			}
			for _, want := range []traceTerm{
				{Term: "workspace-alpha", Kind: "path-substring", Source: "global:component-subtree", Exact: false},
				{Term: "model_v2", Kind: "path-substring", Source: "global:component-subtree", Exact: false},
				{Term: "bin", Kind: "extension", Source: extSource, Exact: true},
			} {
				if !traceHasTerm(trace.Terms, want) {
					t.Fatalf("trace terms = %+v, missing %+v for query %q", trace.Terms, want, query)
				}
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
				t.Fatalf("global component+ext paths = %v, want %v", gotPaths, wantPaths)
			}
			countTrace := &searchTrace{}
			countOpts := opts
			countOpts.Trace = countTrace
			count, ok, err := countServiceVolumes(volumes, countOpts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("global component+ext count declined")
			}
			if countTrace.PlannerMode != "global-count-components" {
				t.Fatalf("count planner mode = %q, want global-count-components", countTrace.PlannerMode)
			}
			if count != wantCount {
				t.Fatalf("global component+ext count = %d, want %d", count, wantCount)
			}
		})
	}
}

func TestGlobalPlannerComponentExtTraceMissingPostingVolume(t *testing.T) {
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	if volumes[0].queryIndex == nil {
		volumes[0].queryIndex = &residentQueryIndex{}
	}
	volumes[0].queryIndex.ext = nil
	trace := &searchTrace{}
	opts := queryOptions{Query: "path:workspace-alpha model_v2 ext:bin", Limit: 5, Trace: trace}
	if got, handled, err := searchServiceVolumesGlobalComponentsOnly(volumes, opts, false); err != nil {
		t.Fatal(err)
	} else if handled {
		t.Fatalf("global components unexpectedly handled missing ext posting source with results=%v", got)
	}
	if trace.Decline != "global-ext:missing-posting" {
		t.Fatalf("decline = %q, want global-ext:missing-posting", trace.Decline)
	}
	if len(trace.Declines) == 0 {
		t.Fatalf("declines = %+v, want missing posting decline", trace.Declines)
	}
	last := trace.Declines[len(trace.Declines)-1]
	if last.Volume != "C:" || last.Source != "global-ext" || last.Reason != "missing-posting" {
		t.Fatalf("last decline = %+v, want C: global-ext missing-posting", last)
	}
}

func TestGlobalPlannerSupportedComponentsDefaultWithoutEnv(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	cases := []struct {
		opts queryOptions
		mode string
	}{
		{opts: queryOptions{Query: "path:workspace-alpha model_v2 ext:bin", Limit: 20}, mode: "global-components"},
		{opts: queryOptions{Query: "workspace-alpha model_v2 ext:bin", MatchPath: true, Limit: 20}, mode: "global-components"},
		{opts: queryOptions{Query: "workspace-alpha model_v2", MatchPath: true, Limit: 20}, mode: "global-components"},
		{opts: queryOptions{Query: "path:workspace-alpha model_v2|alpha-notes", Limit: 20}, mode: "global-components"},
		{opts: queryOptions{Query: "path:workspace-alpha !model_v2", Limit: 20}, mode: "global-components"},
		{opts: queryOptions{Query: "parent:model_v2 type:file", Limit: 20}, mode: "global-components"},
		{opts: queryOptions{Query: "dir:workspace-alpha ext:bin type:file", Limit: 20}, mode: "global-components"},
	}
	for _, tc := range cases {
		t.Run(tc.opts.Query, func(t *testing.T) {
			trace := &searchTrace{}
			opts := tc.opts
			opts.Trace = trace
			got, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != tc.mode {
				t.Fatalf("planner mode = %q, want %q; decline=%s fallback=%s results=%v", trace.PlannerMode, tc.mode, trace.Decline, trace.Fallback, pathsOf(got))
			}
			if strings.Contains(opts.Query, "dir:workspace-alpha") && !traceHasTerm(trace.Terms, traceTerm{Term: "workspace-alpha", Kind: "directory-component", Source: "global:dir"}) {
				t.Fatalf("trace terms = %+v, missing dir source", trace.Terms)
			}
			countTrace := &searchTrace{}
			countOpts := opts
			countOpts.Trace = countTrace
			count, ok, err := countServiceVolumes(volumes, countOpts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("default global component count declined")
			}
			if countTrace.PlannerMode != "global-count-components" {
				t.Fatalf("count planner mode = %q, want global-count-components; decline=%s", countTrace.PlannerMode, countTrace.Decline)
			}
			if count != len(got) {
				t.Fatalf("count = %d, search matches = %d (%v)", count, len(got), pathsOf(got))
			}
		})
	}
}

func TestGlobalPlannerVolumeAnchoredAndExplicitSinglePathTermsDefault(t *testing.T) {
	cIndex := dottedPathBenchmarkIndex(80)
	fIndex := dottedPathBenchmarkIndex(80)
	fIndex.Volume = "F:"
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("single-path-c.gsi", cIndex),
		newServiceVolumeIndex("single-path-f.gsi", fIndex),
	}
	cases := []struct {
		query      string
		searchMode string
		countMode  string
	}{
		{query: "F: fixtureproj", searchMode: "service-single-volume", countMode: "service-count-single-volume"},
		{query: "path:F: raw", searchMode: "global-components", countMode: "global-count-components"},
		{query: "path:trainingdata", searchMode: "global-components", countMode: "global-count-components"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			query := tc.query
			opts := queryOptions{Query: query, Limit: 20}
			want, err := searchAll([]*Index{cIndex, fIndex}, opts, false)
			if err != nil {
				t.Fatalf("oracle search: %v", err)
			}
			trace := &searchTrace{}
			opts.Trace = trace
			got, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != tc.searchMode {
				t.Fatalf("planner mode = %q, want %s; source=%s decline=%s fallback=%s", trace.PlannerMode, tc.searchMode, trace.Source, trace.Decline, trace.Fallback)
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
				t.Fatalf("paths = %v, want %v", gotPaths, wantPaths)
			}
			countTrace := &searchTrace{}
			countWant, err := searchAll([]*Index{cIndex, fIndex}, queryOptions{Query: query}, true)
			if err != nil {
				t.Fatalf("oracle count: %v", err)
			}
			count, ok, err := countServiceVolumes(volumes, queryOptions{Query: query, Trace: countTrace})
			if err != nil || !ok || count != len(countWant) {
				t.Fatalf("count = %d, handled=%v, err=%v; want %d", count, ok, err, len(countWant))
			}
			if countTrace.PlannerMode != tc.countMode {
				t.Fatalf("count planner mode = %q, want %s; source=%s decline=%s fallback=%s", countTrace.PlannerMode, tc.countMode, countTrace.Source, countTrace.Decline, countTrace.Fallback)
			}
		})
	}

	trace := &searchTrace{}
	if _, err := searchServiceVolumes(volumes, queryOptions{Query: "fixtureproj", Limit: 20, Trace: trace}, false); err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode == "global-components" {
		t.Fatal("unanchored bare single term unexpectedly used global components")
	}
}

func TestGlobalPlannerCountVerifiesMalformedLegacyComponentCandidates(t *testing.T) {
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	cases := []struct {
		name    string
		query   string
		corrupt []string
	}{
		{name: "volume-anchor", query: "path:F: workspace-alpha", corrupt: []string{"workspace-alpha"}},
		{name: "common", query: "path:workspace-alpha", corrupt: []string{"workspace-alpha"}},
		{name: "rare", query: "path:target-model-00", corrupt: []string{"target-model-00"}},
		{name: "no-hit", query: "path:missingneedle", corrupt: []string{"missingneedle"}},
		{name: "or", query: "path:workspace-alpha|missingneedle", corrupt: []string{"workspace-alpha", "missingneedle"}},
		{name: "not", query: "path:workspace-alpha !model_v2", corrupt: []string{"workspace-alpha"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			volumes := []*serviceVolumeIndex{
				workspaceAlphaModelVolume("C:", false),
				workspaceAlphaModelVolume("F:", true),
			}
			indexes := []*Index{volumes[0].index, volumes[1].index}
			want, err := searchAll(indexes, queryOptions{Query: tc.query}, true)
			if err != nil {
				t.Fatalf("oracle count: %v", err)
			}
			for _, vol := range volumes {
				// Legacy indexes can have component candidates without the
				// canonical subtree metadata needed for exact interval coverage.
				vol.subtreeOrder = nil
				vol.subtreeStart = nil
				vol.subtreeEnd = nil
				if vol.queryIndex == nil {
					vol.queryIndex = &residentQueryIndex{}
				}
				if vol.queryIndex.components == nil {
					vol.queryIndex.components = make(map[string][]uint32)
				}
				for _, term := range tc.corrupt {
					// Simulate a malformed legacy derived posting: root zero is a
					// complete candidate superset, but not an exact path predicate.
					vol.queryIndex.components[term] = []uint32{0}
				}
			}

			trace := &searchTrace{}
			got, handled, err := countServiceVolumes(volumes, queryOptions{Query: tc.query, Trace: trace})
			if err != nil || !handled {
				t.Fatalf("count handled=%v err=%v; trace=%+v", handled, err, trace)
			}
			if got != len(want) {
				t.Fatalf("count = %d, want oracle %d; source=%s", got, len(want), trace.Source)
			}
		})
	}
}

func TestGlobalPlannerComponentShortExtensionDefaultKeepsGlobalNameOrder(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("c-short-ext.gsi", singleDownloadFileCompactIndex("C:", "z-notes.md")),
		newServiceVolumeIndex("f-short-ext.gsi", singleDownloadFileCompactIndex("F:", "a-notes.md")),
	}
	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "path:Downloads md", Limit: 1, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`F:\Downloads\a-notes.md`}) {
		t.Fatalf("paths = %v, want F volume global best promoted extension match", gotPaths)
	}
}

func TestGlobalPlannerImplicitPathSeparatorDefault(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	opts := queryOptions{Query: `project-03\workspace-alpha\model_v2 type:file`, Limit: 20}
	trace := &searchTrace{}
	searchOpts := opts
	searchOpts.Trace = trace
	got, err := searchServiceVolumes(volumes, searchOpts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`F:\project-03\workspace-alpha\model_v2\target-model-03.bin`}) {
		t.Fatalf("paths = %v, want F project-03 model result", gotPaths)
	}
	countTrace := &searchTrace{}
	countOpts := opts
	countOpts.Trace = countTrace
	count, ok, err := countServiceVolumes(volumes, countOpts)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("countServiceVolumes declined implicit path separator query")
	}
	if countTrace.PlannerMode != "global-count-components" {
		t.Fatalf("count planner mode = %q, want global-count-components; decline=%s", countTrace.PlannerMode, countTrace.Decline)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestGlobalPlannerImplicitPathSeparatorShortComponentsDefault(t *testing.T) {
	makeVolume := func(volume, leaf string) *serviceVolumeIndex {
		idx := &Index{
			Source:  "usn",
			Volume:  volume,
			Compact: true,
			Records: []CompactRecord{
				{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
				{FRN: 2, ParentFRN: 1, Parent: 0, Name: "x", Mode: uint32(os.ModeDir)},
				{FRN: 3, ParentFRN: 2, Parent: 1, Name: "y", Mode: uint32(os.ModeDir)},
				{FRN: 4, ParentFRN: 3, Parent: 2, Name: leaf},
			},
		}
		buildOrders(idx)
		return newServiceVolumeIndex(volume+"-short-path.gsi", idx)
	}
	volumes := []*serviceVolumeIndex{makeVolume("C:", "other.bin"), makeVolume("F:", "target.bin")}
	opts := queryOptions{Query: `x\y\target.bin type:file`, Limit: 10}
	pq := mustParseQuery(t, opts)
	if !slices.Equal(pq.ImplicitPathTerms, []string{"x", "y", "target.bin"}) {
		t.Fatalf("implicit path terms = %v", pq.ImplicitPathTerms)
	}
	bare := mustParseQuery(t, queryOptions{Query: "x y target.bin", MatchPath: true})
	if globalComponentDefaultSupported(bare, nonVolumeTerms(bare.Terms)) {
		t.Fatal("bare short terms unexpectedly enabled the default global planner")
	}

	trace := &searchTrace{}
	opts.Trace = trace
	got, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`F:\x\y\target.bin`}) {
		t.Fatalf("paths = %v, want short separator-derived match", gotPaths)
	}
	countTrace := &searchTrace{}
	opts.Trace = countTrace
	count, ok, err := countServiceVolumes(volumes, opts)
	if err != nil || !ok || count != 1 {
		t.Fatalf("count = %d, handled=%v, err=%v; want 1, true, nil", count, ok, err)
	}
	if countTrace.PlannerMode != "global-count-components" {
		t.Fatalf("count planner mode = %q, want global-count-components; decline=%s", countTrace.PlannerMode, countTrace.Decline)
	}
}

func TestGlobalPlannerUnderMatchesServiceVolumes(t *testing.T) {
	cases := []queryOptions{
		{Query: "ext:bin", Under: `F:\project-03\workspace-alpha`, Limit: 20},
		{Query: "path:workspace-alpha model_v2", MatchPath: true, Under: `F:\project-03`, Limit: 20},
	}
	for _, opts := range cases {
		t.Run(opts.Query+"/under", func(t *testing.T) {
			volumes := []*serviceVolumeIndex{
				workspaceAlphaModelVolume("C:", false),
				workspaceAlphaModelVolume("F:", true),
			}
			want, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			wantCount, ok, err := countServiceVolumes(volumes, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				countMatches, err := searchServiceVolumes(volumes, opts, true)
				if err != nil {
					t.Fatal(err)
				}
				wantCount = len(countMatches)
			}

			t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
			trace := &searchTrace{}
			globalOpts := opts
			globalOpts.Trace = trace
			got, err := searchServiceVolumes(volumes, globalOpts, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != "global-components" {
				t.Fatalf("planner mode = %q, want global-components", trace.PlannerMode)
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
				t.Fatalf("global under paths = %v, want %v", gotPaths, wantPaths)
			}
			countTrace := &searchTrace{}
			countOpts := opts
			countOpts.Trace = countTrace
			count, ok, err := countServiceVolumes(volumes, countOpts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("global under count declined")
			}
			if countTrace.PlannerMode != "global-count-components" {
				t.Fatalf("count planner mode = %q, want global-count-components", countTrace.PlannerMode)
			}
			if count != wantCount {
				t.Fatalf("global under count = %d, want %d", count, wantCount)
			}
		})
	}
}

func TestParentFilterSearchCountParity(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	cases := []struct {
		query string
		want  []string
	}{
		{
			query: "parent:model_v2 type:file",
			want: []string{
				`F:\project-00\workspace-alpha\model_v2\target-model-00.bin`,
				`F:\project-01\workspace-alpha\model_v2\target-model-01.bin`,
				`F:\project-02\workspace-alpha\model_v2\target-model-02.bin`,
				`F:\project-03\workspace-alpha\model_v2\target-model-03.bin`,
				`F:\project-04\workspace-alpha\model_v2\target-model-04.bin`,
				`F:\project-05\workspace-alpha\model_v2\target-model-05.bin`,
				`F:\project-06\workspace-alpha\model_v2\target-model-06.bin`,
				`F:\project-07\workspace-alpha\model_v2\target-model-07.bin`,
			},
		},
		{
			query: "parent:workspace-alpha type:dir",
			want: []string{
				`F:\project-00\workspace-alpha\model_v2`,
				`F:\project-01\workspace-alpha\model_v2`,
				`F:\project-02\workspace-alpha\model_v2`,
				`F:\project-03\workspace-alpha\model_v2`,
				`F:\project-04\workspace-alpha\model_v2`,
				`F:\project-05\workspace-alpha\model_v2`,
				`F:\project-06\workspace-alpha\model_v2`,
				`F:\project-07\workspace-alpha\model_v2`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			opts := queryOptions{Query: tc.query, Limit: 20}
			got, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			if paths := pathsOf(got); !slices.Equal(paths, tc.want) {
				t.Fatalf("paths = %v, want %v", paths, tc.want)
			}
			count, ok, err := countServiceVolumes(volumes, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				countMatches, err := searchServiceVolumes(volumes, opts, true)
				if err != nil {
					t.Fatal(err)
				}
				count = len(countMatches)
			}
			if count != len(tc.want) {
				t.Fatalf("count = %d, want %d", count, len(tc.want))
			}
		})
	}
}

func TestParentFilterBuildsCandidateSource(t *testing.T) {
	vol := workspaceAlphaModelVolume("F:", true)
	pq := mustParseQuery(t, queryOptions{Query: "parent:model_v2 type:file"})
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		t.Fatal("buildCandidatePlan declined parent filter")
	}
	if len(plan.sources) == 0 || plan.sources[0].name != "parent:model_v2" {
		t.Fatalf("plan sources = %+v, want parent:model_v2 source", plan.sources)
	}
	if got := plan.execute(); len(got) != 8 {
		t.Fatalf("parent candidate count = %d, want 8; ids=%v", len(got), got)
	}
}

func TestGlobalPlannerParentFilterMatchesServiceVolumes(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	opts := queryOptions{Query: "path:workspace-alpha model_v2 parent:workspace-alpha", Limit: 20}
	want, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	wantCount, ok, err := countServiceVolumes(volumes, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		countMatches, err := searchServiceVolumes(volumes, opts, true)
		if err != nil {
			t.Fatal(err)
		}
		wantCount = len(countMatches)
	}

	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	trace := &searchTrace{}
	globalOpts := opts
	globalOpts.Trace = trace
	got, err := searchServiceVolumes(volumes, globalOpts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components", trace.PlannerMode)
	}
	if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
		t.Fatalf("global parent paths = %v, want %v", gotPaths, wantPaths)
	}
	countTrace := &searchTrace{}
	countOpts := opts
	countOpts.Trace = countTrace
	count, ok, err := countServiceVolumes(volumes, countOpts)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("global parent count declined")
	}
	if countTrace.PlannerMode != "global-count-components" {
		t.Fatalf("count planner mode = %q, want global-count-components", countTrace.PlannerMode)
	}
	if count != wantCount {
		t.Fatalf("global parent count = %d, want %d", count, wantCount)
	}
}

func TestGlobalPlannerParentOnlyMatchesServiceVolumes(t *testing.T) {
	for _, query := range []string{"parent:model_v2", "parent:model_v2 ext:bin"} {
		t.Run(query, func(t *testing.T) {
			volumes := []*serviceVolumeIndex{
				workspaceAlphaModelVolume("C:", false),
				workspaceAlphaModelVolume("F:", true),
			}
			opts := queryOptions{Query: query, Limit: 20}
			want, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			wantCount, ok, err := countServiceVolumes(volumes, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				countMatches, err := searchServiceVolumes(volumes, opts, true)
				if err != nil {
					t.Fatal(err)
				}
				wantCount = len(countMatches)
			}

			t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
			trace := &searchTrace{}
			globalOpts := opts
			globalOpts.Trace = trace
			got, err := searchServiceVolumes(volumes, globalOpts, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != "global-components" {
				t.Fatalf("planner mode = %q, want global-components", trace.PlannerMode)
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
				t.Fatalf("global parent paths = %v, want %v", gotPaths, wantPaths)
			}
			countTrace := &searchTrace{}
			countOpts := opts
			countOpts.Trace = countTrace
			count, ok, err := countServiceVolumes(volumes, countOpts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("global parent count declined")
			}
			if countTrace.PlannerMode != "global-count-components" {
				t.Fatalf("count planner mode = %q, want global-count-components", countTrace.PlannerMode)
			}
			if count != wantCount {
				t.Fatalf("global parent count = %d, want %d", count, wantCount)
			}
		})
	}
}

func TestGlobalPlannerRequestSeqDoesNotStopAfterFirstHit(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", true),
		workspaceAlphaModelVolume("F:", true),
	}
	opts := queryOptions{
		Query:      "path:workspace-alpha model_v2 type:file sort:size",
		Limit:      20,
		RequestSeq: time.Now().UnixNano(),
	}
	trace := &searchTrace{}
	opts.Trace = trace
	got, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components", trace.PlannerMode)
	}
	paths := pathsOf(got)
	var cHits, fHits int
	for _, path := range paths {
		if strings.HasPrefix(path, `C:\`) {
			cHits++
		}
		if strings.HasPrefix(path, `F:\`) {
			fHits++
		}
	}
	if cHits == 0 || fHits == 0 {
		t.Fatalf("paths = %v, want hits from both C: and F:", paths)
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

func TestFilenameTrigramCompletePNGRExactEmptyDeclinesNoFallback(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	trigrams := buildNameTrigramIndex(idx)
	trigrams.gramCountsComplete = true
	vol.nameTrigrams.Store(trigrams)

	trace := &searchTrace{}
	pq := mustParseQuery(t, queryOptions{Query: "zzzz-no-hit", Limit: 20, Trace: trace})
	candidates, ok := vol.filenameTrigramCandidates(pq)
	if !ok || len(candidates) != 0 {
		t.Fatalf("complete PNGR candidates=%v ok=%v, want exact empty", candidates, ok)
	}
	if trace.Source != "exact-empty" || trace.Candidates != 0 || trace.Decline != "" {
		t.Fatalf("exact-empty trace=%+v, want terminal exact-empty without decline", trace)
	}

	searchRunTrace := &searchTrace{}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
		Query: "zzzz-no-hit", Limit: 20, Trace: searchRunTrace,
	}, false)
	if err != nil || len(got) != 0 {
		t.Fatalf("complete PNGR search got=%v err=%v, want zero", got, err)
	}
	if searchRunTrace.Source != "exact-empty" || searchRunTrace.Candidates != 0 {
		t.Fatalf("complete PNGR search trace=%+v, want exact-empty zero", searchRunTrace)
	}

	countTrace := &searchTrace{}
	count, handled, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
		Query: "zzzz-no-hit", Trace: countTrace,
	})
	if err != nil || !handled || count != 0 {
		t.Fatalf("complete PNGR count=%d handled=%v err=%v, want zero handled", count, handled, err)
	}
	if countTrace.Source != "exact-empty" || countTrace.Candidates != 0 {
		t.Fatalf("complete PNGR count trace=%+v, want exact-empty zero", countTrace)
	}

	legacy := *trigrams
	legacy.gramCountsComplete = false
	vol.nameTrigrams.Store(&legacy)
	legacyTrace := &searchTrace{}
	legacyPQ := mustParseQuery(t, queryOptions{Query: "zzzz-no-hit", Limit: 20, Trace: legacyTrace})
	if candidates, ok := vol.filenameTrigramCandidates(legacyPQ); ok {
		t.Fatalf("legacy PNGR returned %d candidates, want safe decline", len(candidates))
	}
	if legacyTrace.Decline != "name-trigram:missing-section" {
		t.Fatalf("legacy PNGR trace=%+v, want missing-section decline", legacyTrace)
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

func TestGlobalPlannerRegexLiteralUsesGlobalSource(t *testing.T) {
	idx := dottedPathBenchmarkIndex(200)
	vol := newServiceVolumeIndex("regex.gsi", idx)
	trace := &searchTrace{}
	opts := queryOptions{Query: `regex:.*filtered-volume-cleaned.*`, MatchPath: true, Limit: 10, Trace: trace}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
	}
	if !traceHasTerm(trace.Terms, traceTerm{Term: "filtered-volume-cleaned", Kind: "regex-literal", Source: "global:regex-literal"}) {
		t.Fatalf("trace terms = %+v, missing regex literal source", trace.Terms)
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`C:\Users\exampleuser\Downloads\filtered-volume-cleaned.nrrd`}) {
		t.Fatalf("paths = %v, want filtered-volume-cleaned.nrrd only", gotPaths)
	}
	countTrace := &searchTrace{}
	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: opts.Query, MatchPath: true, Trace: countTrace})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("countServiceVolumes declined regex literal")
	}
	if countTrace.PlannerMode != "global-count-components" {
		t.Fatalf("count planner mode = %q, want global-count-components; decline=%s", countTrace.PlannerMode, countTrace.Decline)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestGlobalPlannerRegexLiteralDeclinesAmbiguousAlternation(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	trace := &searchTrace{}
	opts := queryOptions{Query: `regex:Assets.*\.(dat|txt)$`, MatchPath: true, Limit: 10, Trace: trace}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode == "global-components" {
		t.Fatalf("planner mode = %q, want legacy fallback for ambiguous regex; terms=%+v", trace.PlannerMode, trace.Terms)
	}
	if len(got) == 0 {
		t.Fatal("ambiguous regex fallback returned no matches")
	}
}

func TestGlobalPlannerRegexLiteralIntersectsExtensionSource(t *testing.T) {
	idx := dottedPathBenchmarkIndex(200)
	vol := newServiceVolumeIndex("regex-ext.gsi", idx)
	trace := &searchTrace{}
	opts := queryOptions{Query: `ext:nrrd regex:.*filtered-volume-cleaned.*`, MatchPath: true, Limit: 10, Trace: trace}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
	}
	for _, want := range []traceTerm{
		{Term: "filtered-volume-cleaned", Kind: "regex-literal", Source: "global:regex-literal"},
		{Term: "nrrd", Kind: "extension", Source: "global:ext:nrrd", Exact: true},
	} {
		if !traceHasTerm(trace.Terms, want) {
			t.Fatalf("trace terms = %+v, missing %+v", trace.Terms, want)
		}
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`C:\Users\exampleuser\Downloads\filtered-volume-cleaned.nrrd`}) {
		t.Fatalf("paths = %v, want filtered-volume-cleaned.nrrd only", gotPaths)
	}
}

func TestGlobalPlannerRegexLiteralDeclinesCaseSensitive(t *testing.T) {
	idx := dottedPathBenchmarkIndex(200)
	vol := newServiceVolumeIndex("regex-case.gsi", idx)
	trace := &searchTrace{}
	opts := queryOptions{Query: `case: regex:.*Filtered.*`, MatchPath: true, Limit: 10, Trace: trace}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode == "global-components" {
		t.Fatalf("planner mode = %q, want verified fallback for case-sensitive regex; terms=%+v", trace.PlannerMode, trace.Terms)
	}
	if len(got) != 0 {
		t.Fatalf("case-sensitive regex matches = %v, want none for lowercase path", pathsOf(got))
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

func workspaceAlphaModelVolume(volume string, includeModel bool) *serviceVolumeIndex {
	idx := &Index{
		Source:  "usn",
		Volume:  volume,
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
	if !includeModel {
		workspace := add(2, 1, root, "workspace-alpha", uint32(os.ModeDir))
		add(3, 2, workspace, "alpha-notes.txt", 0)
	} else {
		nextFRN := uint64(2)
		for i := 0; i < 8; i++ {
			project := add(nextFRN, 1, root, fmt.Sprintf("project-%02d", i), uint32(os.ModeDir))
			projectFRN := nextFRN
			nextFRN++
			workspace := add(nextFRN, projectFRN, project, "workspace-alpha", uint32(os.ModeDir))
			workspaceFRN := nextFRN
			nextFRN++
			model := add(nextFRN, workspaceFRN, workspace, "model_v2", uint32(os.ModeDir))
			modelFRN := nextFRN
			nextFRN++
			add(nextFRN, modelFRN, model, fmt.Sprintf("target-model-%02d.bin", i), 0)
			nextFRN++
		}
	}
	buildOrders(idx)
	return newServiceVolumeIndex(strings.ToLower(strings.TrimSuffix(volume, ":"))+"-workspace-alpha.gsi", idx)
}

type componentSortFixtureEntry struct {
	name    string
	mode    uint32
	size    int64
	modUnix int64
}

func componentSortVolume(volume string, entries []componentSortFixtureEntry) *serviceVolumeIndex {
	idx := &Index{
		Source:  "usn",
		Volume:  volume,
		Compact: true,
	}
	add := func(frn, parentFRN uint64, parent int32, name string, mode uint32, size int64, modUnix int64) int32 {
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       frn,
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
			Mode:      mode,
			Size:      size,
			ModUnix:   modUnix,
		})
		return int32(len(idx.Records) - 1)
	}
	root := add(1, 1, -1, ".", uint32(os.ModeDir), 0, 1)
	project := add(2, 1, root, "project", uint32(os.ModeDir), 0, 2)
	workspace := add(3, 2, project, "workspace-alpha", uint32(os.ModeDir), 0, 3)
	model := add(4, 3, workspace, "model_v2", uint32(os.ModeDir), 0, 4)
	nextFRN := uint64(5)
	for _, entry := range entries {
		add(nextFRN, 4, model, entry.name, entry.mode, entry.size, entry.modUnix)
		nextFRN++
	}
	buildOrders(idx)
	return newServiceVolumeIndex(strings.ToLower(strings.TrimSuffix(volume, ":"))+"-component-sort.gsi", idx)
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
