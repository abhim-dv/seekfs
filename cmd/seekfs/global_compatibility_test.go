package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestGlobalPlannerMixedV8V9MultiVolumeParityAndTrace(t *testing.T) {
	cIndex := compatibilityIndexForVolume("C:")
	fIndex := compatibilityIndexForVolume("F:")
	v8 := roundTripCompatibilityIndex(t, cIndex, false, false)
	v9 := roundTripCompatibilityIndex(t, fIndex, true, false)
	t.Setenv("SEEKFS_ENGINE_V9", "")
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("mixed-v8.gsi", v8),
		newServiceVolumeIndex("mixed-v9.gsi", v9),
	}

	for _, tc := range []struct {
		query string
		mode  string
	}{
		{query: "ext:txt", mode: "global-ext"},
		{query: "path:workspace alpha", mode: "global-components"},
		{query: "dir:workspace ext:go", mode: "global-components"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			opts := queryOptions{Query: tc.query, MatchPath: true, Limit: 20}
			want, err := searchAll([]*Index{v8, v9}, opts, false)
			if err != nil {
				t.Fatalf("oracle search: %v", err)
			}
			trace := &searchTrace{}
			opts.Trace = trace
			got, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatalf("mixed service search: %v", err)
			}
			if !slices.Equal(pathsOf(got), pathsOf(want)) {
				t.Fatalf("mixed paths = %v, want %v", pathsOf(got), pathsOf(want))
			}
			assertCompleteGlobalTrace(t, trace, tc.mode)

			countOpts := opts
			countOpts.Limit = 0
			countTrace := &searchTrace{}
			countOpts.Trace = countTrace
			count, ok, err := countServiceVolumes(volumes, countOpts)
			if err != nil || !ok || count != len(want) {
				t.Fatalf("mixed count = %d, handled=%v, err=%v; want %d", count, ok, err, len(want))
			}
			assertCompleteGlobalTrace(t, countTrace, "global-count-"+globalTraceModeSuffix(tc.mode))
		})
	}
}

func TestGlobalPlannerMixedV8V9FallbackTrace(t *testing.T) {
	v8 := roundTripCompatibilityIndex(t, compatibilityIndexForVolume("C:"), false, false)
	v9 := roundTripCompatibilityIndex(t, compatibilityIndexForVolume("F:"), true, false)
	t.Setenv("SEEKFS_ENGINE_V9", "")
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("fallback-v8.gsi", v8),
		newServiceVolumeIndex("fallback-v9.gsi", v9),
	}
	volumes[0].queryIndex.ext = nil

	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "ext:txt", Limit: 20, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	want, err := searchAll([]*Index{v8, v9}, queryOptions{Query: "ext:txt", Limit: 20}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pathsOf(got), pathsOf(want)) {
		t.Fatalf("fallback paths = %v, want %v", pathsOf(got), pathsOf(want))
	}
	if trace.PlannerMode != "global-bounded-scan" || trace.Source != "global:bounded-scan" || trace.Decline != "global-ext:missing-posting" ||
		trace.Fallback != "global-bounded-scan" || trace.Complete == nil || !*trace.Complete {
		t.Fatalf("fallback trace = %+v, want complete sourced global bounded-scan decline", trace)
	}
	if len(trace.Declines) == 0 || trace.Declines[len(trace.Declines)-1] != (traceDecline{Source: "global-ext", Reason: "missing-posting", Volume: "C:"}) {
		t.Fatalf("fallback declines = %+v, want C: missing posting", trace.Declines)
	}

	countTrace := &searchTrace{}
	count, ok, err := countServiceVolumes(volumes, queryOptions{Query: "ext:txt", Trace: countTrace})
	if err != nil || !ok || count != len(want) {
		t.Fatalf("fallback count = %d, handled=%v, err=%v; want %d", count, ok, err, len(want))
	}
	if countTrace.PlannerMode != "global-bounded-scan" || countTrace.Source != "global:bounded-scan" ||
		countTrace.Decline != "global-ext:missing-posting" || countTrace.Fallback != "global-bounded-scan" ||
		countTrace.Complete == nil || !*countTrace.Complete {
		t.Fatalf("fallback count trace = %+v, want complete sourced global bounded-scan decline", countTrace)
	}
	if len(countTrace.Declines) == 0 || countTrace.Declines[len(countTrace.Declines)-1] != (traceDecline{Source: "global-ext", Reason: "missing-posting", Volume: "C:"}) {
		t.Fatalf("fallback count declines = %+v, want C: missing posting", countTrace.Declines)
	}
}

func TestGlobalPlannerQueryFamiliesNeverUsePerVolumeTerminal(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("families-c.gsi", compatibilityIndexForVolume("C:")),
		newServiceVolumeIndex("families-f.gsi", compatibilityIndexForVolume("F:")),
	}
	cases := []queryOptions{
		{Query: "needle", Limit: 3},
		{Query: "needle|workspace", Limit: 3},
		{Query: "needle !backup", MatchPath: true, Limit: 3},
		{Query: "case: regex:.*needle.*", Limit: 3},
		{Query: "glob:*.txt", Limit: 3},
		{Query: "size:>=1", Limit: 3},
		{Query: "type:file", Exists: true, Limit: 3},
		{Query: "needle", RootBias: `C:\`, Limit: 3},
		{Query: "ext:txt", Limit: 3},
	}
	for _, base := range cases {
		base := base
		t.Run(base.Query, func(t *testing.T) {
			trace := &searchTrace{}
			base.Trace = trace
			_, err := searchServiceVolumes(volumes, base, false)
			if err != nil && !errors.Is(err, errGlobalMultiVolumePlannerDeclined) {
				t.Fatalf("search error = %v", err)
			}
			if strings.Contains(trace.PlannerMode, "per-volume") || strings.Contains(trace.Fallback, "per-volume") {
				t.Fatalf("search used per-volume terminal: %+v", trace)
			}
			countTrace := &searchTrace{}
			base.Trace = countTrace
			_, handled, err := countServiceVolumes(volumes, base)
			if err != nil && !errors.Is(err, errGlobalMultiVolumePlannerDeclined) {
				t.Fatalf("count error = %v", err)
			}
			if !handled {
				t.Fatal("count was not handled")
			}
			if strings.Contains(countTrace.PlannerMode, "per-volume") || strings.Contains(countTrace.Fallback, "per-volume") {
				t.Fatalf("count used per-volume terminal: %+v", countTrace)
			}
		})
	}
}

func compatibilityIndexForVolume(volume string) *Index {
	idx := cloneCompactIndex(compatibilityMatrixIndex())
	idx.Volume = volume
	idx.Roots = []string{volume + `\`}
	return idx
}

func assertCompleteGlobalTrace(t *testing.T, trace *searchTrace, mode string) {
	t.Helper()
	if trace.PlannerMode != mode || trace.Source == "" || trace.Complete == nil || !*trace.Complete {
		t.Fatalf("trace = %+v, want complete sourced %s", trace, mode)
	}
	if trace.Decline != "" || len(trace.Declines) != 0 || trace.Fallback != "" {
		t.Fatalf("supported trace has decline/fallback: %+v", trace)
	}
	if !slices.Equal(trace.EligibleVolumes, []string{"C:", "F:"}) {
		t.Fatalf("eligible volumes = %v, want C:, F:", trace.EligibleVolumes)
	}
}

func globalTraceModeSuffix(mode string) string {
	if mode == "global-ext" {
		return "ext"
	}
	return "components"
}
