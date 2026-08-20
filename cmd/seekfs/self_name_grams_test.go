package main

import (
	"encoding/binary"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOptionalSelfNameGramRoundTripLegacyUnknownAndCorruption(t *testing.T) {
	idx := dottedPathBenchmarkIndex(2_000)
	selective := buildSelectiveNameTrigramIndex(idx, 1)
	if len(selective.omitted) == 0 {
		t.Fatal("fixture did not produce an omitted common gram")
	}
	extra := optionalSelfNameGramIndex(idx, selective)
	if extra == nil {
		t.Fatal("optional self-name source is empty")
	}
	section := encodeGramPostingSection(extra, nil)
	decoded := decodeGramPostingIndex(section, idx.compactRecordCount())
	if decoded == nil || !decoded.gramCountsComplete || decoded.mappedGrams == nil {
		t.Fatalf("optional section did not round-trip: %#v", decoded)
	}
	for gram, count := range extra.counts {
		if decoded.countForGram(gram) != count {
			t.Fatalf("gram %08x count=%d, want %d", gram, decoded.countForGram(gram), count)
		}
	}
	derived := indexDerivedSections{}
	decodeDerivedSection(&derived, indexSectionPNGC, section, idx.compactRecordCount())
	if derived.SelfNameTrigrams == nil {
		t.Fatal("derived decoder did not retain PNGC")
	}
	legacyDerived := indexDerivedSections{}
	decodeDerivedSection(&legacyDerived, indexSectionPNGR, encodeGramPostingSection(selective, nil), idx.compactRecordCount())
	if legacyDerived.SelfNameTrigrams != nil {
		t.Fatal("legacy PNGR unexpectedly created PNGC state")
	}
	unknown := indexDerivedSections{}
	decodeDerivedSection(&unknown, 0x554e4b4e, section, idx.compactRecordCount())
	if unknown.SelfNameTrigrams != nil || unknown.NameTrigrams != nil {
		t.Fatal("unknown derived section was not ignored")
	}
	bad := append([]byte(nil), section...)
	binary.LittleEndian.PutUint32(bad[0:], ^uint32(0))
	if decodeGramPostingIndex(bad, idx.compactRecordCount()) != nil {
		t.Fatal("corrupt PNGC entry count was accepted")
	}
}

func TestCompleteSelfNameGramIntersectionVisitsPostingNotRecordCount(t *testing.T) {
	idx := dottedPathBenchmarkIndex(50_000)
	selective := buildSelectiveNameTrigramIndex(idx, 1)
	extra := optionalSelfNameGramIndex(idx, selective)
	if extra == nil {
		t.Fatal("fixture did not produce a complete common-gram companion")
	}
	base := decodeGramPostingIndex(encodeGramPostingSection(selective, nil), idx.compactRecordCount())
	companion := decodeGramPostingIndex(encodeGramPostingSection(extra, nil), idx.compactRecordCount())
	idx.Derived.NameTrigrams = base
	idx.Derived.SelfNameTrigrams = companion
	vol := newServiceVolumeIndex("self-gram.gsi", idx)
	coverage := mappedComponentCoverage{}
	start := time.Now()
	count, visited, exactZero, ok := vol.countMappedComponentSelfNameGramHits("nrrd", coverage, nil, parsedQuery{})
	if !ok || exactZero {
		t.Fatalf("complete source declined: ok=%v exactZero=%v count=%d visited=%d", ok, exactZero, count, visited)
	}
	want := 0
	for id := 0; id < idx.compactRecordCount(); id++ {
		rec := idx.compactRecord(id)
		if !rec.Deleted && rec.Mode&uint32(os.ModeDir) == 0 && strings.Contains(idx.compactLowerNameAt(id), "nrrd") {
			want++
		}
	}
	if count != want {
		t.Fatalf("complete source count=%d, oracle=%d", count, want)
	}
	if visited <= 0 || visited >= idx.compactRecordCount() {
		t.Fatalf("candidate visits=%d records=%d; want a bounded posting intersection", visited, idx.compactRecordCount())
	}
	if got, ok := vol.completeSelfNameGramTop("nrrd", 20, parsedQuery{Limit: 20}); !ok || len(got) == 0 {
		t.Fatalf("complete source top search declined or returned no matches: ok=%v got=%d", ok, len(got))
	}
	for _, term := range []string{"nrrd", ".json", "zzzz-no-hit"} {
		searchTraceValue := &searchTrace{}
		pq := mustParseQuery(t, queryOptions{Query: term, Limit: 20, Trace: searchTraceValue})
		got, ok := vol.completeFilenameTopPosting(term, 20, pq)
		if !ok {
			t.Fatalf("filename top source declined term=%q", term)
		}
		countTrace := &searchTrace{}
		countPQ := mustParseQuery(t, queryOptions{Query: term, Trace: countTrace})
		count, ok := vol.completeFilenameCountPosting(term, countPQ)
		if !ok {
			t.Fatalf("filename count source declined term=%q", term)
		}
		want := 0
		for id := 0; id < idx.compactRecordCount(); id++ {
			if vol.nameTrigramCandidateMatches(id, term) {
				want++
			}
		}
		if term == "zzzz-no-hit" {
			if len(got) != 0 || count != 0 || searchTraceValue.Source != "exact-empty" || countTrace.Source != "exact-empty" {
				t.Fatalf("exact-empty term=%q search=%v/%+v count=%d/%+v", term, got, searchTraceValue, count, countTrace)
			}
			continue
		}
		if count != want || len(got) == 0 || searchTraceValue.FilenameDriver != "ranked-posting-pngc" || countTrace.FilenameDriver != "posting-intersection-pngc" {
			t.Fatalf("term=%q search=%v/%+v count=%d want=%d/%+v", term, got, searchTraceValue, count, want, countTrace)
		}
	}
	t.Logf("complete self-name gram term=nrrd records=%d candidates_verified=%d matches=%d companion_bytes=%d elapsed=%s", idx.compactRecordCount(), visited, count, len(encodeGramPostingSection(extra, nil)), time.Since(start).Round(time.Microsecond))
}

func TestCompleteSelfNameGramSyntheticMeasurements(t *testing.T) {
	for _, records := range []int{50_000, 200_000} {
		t.Run("records-"+strconv.Itoa(records), func(t *testing.T) {
			idx := dottedPathBenchmarkIndex(records)
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			buildStart := time.Now()
			selective := buildSelectiveNameTrigramIndex(idx, 1)
			extra := optionalSelfNameGramIndex(idx, selective)
			buildElapsed := time.Since(buildStart)
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			if extra == nil {
				t.Fatal("missing optional source")
			}
			idx.Derived.NameTrigrams = decodeGramPostingIndex(encodeGramPostingSection(selective, nil), records)
			idx.Derived.SelfNameTrigrams = decodeGramPostingIndex(encodeGramPostingSection(extra, nil), records)
			vol := newServiceVolumeIndex("synthetic.gsi", idx)
			measure := func(term string, search bool) []time.Duration {
				latencies := make([]time.Duration, 0, 12)
				for i := 0; i < 12; i++ {
					start := time.Now()
					if search {
						if _, ok := vol.completeSelfNameGramTop(term, 20, parsedQuery{Limit: 20}); !ok {
							t.Fatalf("search source declined term=%q", term)
						}
					} else {
						if _, _, _, ok := vol.countMappedComponentSelfNameGramHits(term, mappedComponentCoverage{}, nil, parsedQuery{}); !ok {
							t.Fatalf("count source declined term=%q", term)
						}
					}
					latencies = append(latencies, time.Since(start))
				}
				return latencies
			}
			percentile := func(values []time.Duration, p float64) time.Duration {
				sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
				return values[int(float64(len(values)-1)*p)]
			}
			for _, term := range []string{"nrrd", "opencode"} {
				for _, sample := range []struct {
					name   string
					values []time.Duration
				}{
					{name: "search", values: measure(term, true)},
					{name: "count", values: measure(term, false)},
				} {
					p50 := percentile(append([]time.Duration(nil), sample.values...), .50)
					p95 := percentile(append([]time.Duration(nil), sample.values...), .95)
					p99 := percentile(append([]time.Duration(nil), sample.values...), .99)
					t.Logf("records=%d term=%s mode=%s p50=%s p95=%s p99=%s", records, term, sample.name, p50, p95, p99)
					if p95 > 100*time.Millisecond || p99 > 200*time.Millisecond {
						t.Fatalf("synthetic %s %s tail exceeds gate: p95=%s p99=%s", term, sample.name, p95, p99)
					}
				}
			}
			allocs := testing.AllocsPerRun(3, func() {
				_, _, _, _ = vol.countMappedComponentSelfNameGramHits("nrrd", mappedComponentCoverage{}, nil, parsedQuery{})
			})
			its, _, _, complete := completeSelfNameGramIterators(idx, "nrrd")
			if !complete {
				t.Fatal("complete source iterator setup declined")
			}
			postingBlocks := 0
			for _, it := range its {
				postingBlocks += it.end - it.next
			}
			fallback := make([]time.Duration, 0, 3)
			for i := 0; i < 3; i++ {
				start := time.Now()
				_ = scanNameContains(idx, "nrrd")
				fallback = append(fallback, time.Since(start))
			}
			fallbackP95 := percentile(append([]time.Duration(nil), fallback...), .95)
			gramBytes := len(encodeGramPostingSection(selective, nil))
			commonBytes := len(encodeGramPostingSection(extra, nil))
			t.Logf("records=%d build=%s heap_inuse_delta=%d allocs_count=%.1f PNGR=%d PNGC=%d bytes_per_record=%.3f posting_blocks=%d blocks_skipped_by_intersection=%d fallback_count_p95=%s", records, buildElapsed.Round(time.Millisecond), int64(after.HeapInuse)-int64(before.HeapInuse), allocs, gramBytes, commonBytes, float64(gramBytes+commonBytes)/float64(records), postingBlocks, max(0, postingBlocks-1), fallbackP95)
		})
	}
}

func TestCompleteSelfNameGramSyntheticServiceBroadMatrix(t *testing.T) {
	idx := dottedPathBenchmarkIndex(50_000)
	selective := buildSelectiveNameTrigramIndex(idx, 1)
	extra := optionalSelfNameGramIndex(idx, selective)
	if extra == nil {
		t.Fatal("missing PNGC companion")
	}
	idx.Derived.NameTrigrams = decodeGramPostingIndex(encodeGramPostingSection(selective, nil), idx.compactRecordCount())
	idx.Derived.SelfNameTrigrams = decodeGramPostingIndex(encodeGramPostingSection(extra, nil), idx.compactRecordCount())
	vol := newServiceVolumeIndex("synthetic-pngc-service.gsi", idx)
	volumes := []*serviceVolumeIndex{vol}
	queries := []queryOptions{
		{Query: ".json", Limit: 20},
		{Query: "nrrd", Limit: 20},
		{Query: "opencode", Limit: 20},
		{Query: "zzzz-no-hit", Limit: 20},
		{Query: "path:workspace nrrd", MatchPath: true, Limit: 20},
		{Query: "nrrd|raw", Limit: 20},
		{Query: "nrrd !raw", Limit: 20},
		{Query: "ext:json", MatchPath: true, Limit: 20},
		{Query: "type:file nrrd", MatchPath: true, Limit: 20},
		{Query: "size:>1mb", Limit: 20},
	}
	percentile := func(values []time.Duration, p float64) time.Duration {
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		return values[int(float64(len(values)-1)*p)]
	}
	for _, opts := range queries {
		opts := opts
		t.Run(opts.Query, func(t *testing.T) {
			want, err := r5ExhaustivePlannerOracle(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			countOpts := opts
			countOpts.Limit = 0
			wantCount, err := r5ExhaustivePlannerOracle(volumes, countOpts, true)
			if err != nil {
				t.Fatal(err)
			}
			searchValues := make([]time.Duration, 0, 12)
			countValues := make([]time.Duration, 0, 12)
			for i := 0; i < 12; i++ {
				trace := &searchTrace{}
				searchOpts := opts
				searchOpts.Trace = trace
				start := time.Now()
				got, err := searchServiceVolumes(volumes, searchOpts, false)
				searchValues = append(searchValues, time.Since(start))
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(pathsOf(got), pathsOf(want)) {
					t.Fatalf("search paths=%v want=%v source=%s", pathsOf(got), pathsOf(want), trace.Source)
				}
				countTrace := &searchTrace{}
				countOpts.Trace = countTrace
				start = time.Now()
				gotCount, handled, err := countServiceVolumes(volumes, countOpts)
				countValues = append(countValues, time.Since(start))
				if err != nil || !handled {
					t.Fatalf("count handled=%v err=%v", handled, err)
				}
				if gotCount != len(wantCount) {
					t.Fatalf("count=%d want=%d source=%s", gotCount, len(wantCount), countTrace.Source)
				}
			}
			t.Logf("records=%d query=%q search_p50=%s search_p95=%s search_p99=%s count_p50=%s count_p95=%s count_p99=%s PNGC_bytes=%d", idx.compactRecordCount(), opts.Query, percentile(append([]time.Duration(nil), searchValues...), .50), percentile(append([]time.Duration(nil), searchValues...), .95), percentile(append([]time.Duration(nil), searchValues...), .99), percentile(append([]time.Duration(nil), countValues...), .50), percentile(append([]time.Duration(nil), countValues...), .95), percentile(append([]time.Duration(nil), countValues...), .99), len(encodeGramPostingSection(extra, nil)))
		})
	}
}

func BenchmarkCompleteSelfNameGramSource(b *testing.B) {
	idx := dottedPathBenchmarkIndex(50_000)
	selective := buildSelectiveNameTrigramIndex(idx, 1)
	if len(selective.omitted) == 0 {
		b.Fatal("fixture did not produce omitted grams")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		extra := optionalSelfNameGramIndex(idx, selective)
		data := encodeGramPostingSection(extra, nil)
		b.ReportMetric(float64(len(data)), "section_bytes")
		b.ReportMetric(float64(time.Since(start).Microseconds()), "build_us")
	}
}
