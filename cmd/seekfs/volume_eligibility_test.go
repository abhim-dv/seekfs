package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestAbsentExplicitVolumeTerminatesBeforePlannerRouting(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Roots:   []string{`C:\`},
		Compact: true,
		Records: []CompactRecord{{FRN: 1, Name: "fixture.nrrd"}},
	}
	vol := newServiceVolumeIndex("c-only.gsi", idx)

	shapes := []string{
		"F: nrrd",
		"path:F: .pdf",
		"F: ext:nrrd",
		"F: size:>1mb",
		"F: nrrd|raw",
		"F: nrrd !raw",
		"F: glob:*.nrrd",
		`F: regex:.*nrrd.*`,
	}
	sorts := []string{"", "sort:path", "sort:size", "sort:modified", "sort:extension", "sort:type"}
	limits := []int{1, 20, 100}
	for _, shape := range shapes {
		for _, sortTerm := range sorts {
			for _, limit := range limits {
				query := strings.TrimSpace(shape + " " + sortTerm)
				t.Run(fmt.Sprintf("search/%s/%s/%d", strings.ReplaceAll(shape, " ", "_"), strings.ReplaceAll(sortTerm, ":", "_"), limit), func(t *testing.T) {
					trace := &searchTrace{}
					got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
						Query: query, MatchPath: true, Limit: limit, Trace: trace,
					}, false)
					if err != nil {
						t.Fatal(err)
					}
					if len(got) != 0 || trace.Source != "volume-empty" || trace.PlannerMode != "volume-empty" {
						t.Fatalf("search query=%q got=%v trace=%+v; want exact empty volume route", query, got, trace)
					}
					if len(trace.EligibleVolumes) != 0 || trace.Candidates != 0 || trace.BlocksDecoded != 0 || trace.BlocksSkipped != 0 {
						t.Fatalf("search query=%q touched absent volume: trace=%+v", query, trace)
					}

					countTrace := &searchTrace{}
					count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
						Query: query, MatchPath: true, Limit: limit, Trace: countTrace,
					})
					if err != nil || !ok || count != 0 {
						t.Fatalf("count query=%q count=%d ok=%v err=%v", query, count, ok, err)
					}
					if countTrace.Source != "volume-empty" || countTrace.PlannerMode != "volume-empty" || len(countTrace.EligibleVolumes) != 0 || countTrace.Candidates != 0 || countTrace.BlocksDecoded != 0 || countTrace.BlocksSkipped != 0 {
						t.Fatalf("count query=%q touched absent volume: trace=%+v", query, countTrace)
					}
				})
			}
		}
	}

	underTrace := &searchTrace{}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
		Query: "nrrd", MatchPath: true, Under: `F:\`, Limit: 20, Trace: underTrace,
	}, false)
	if err != nil || len(got) != 0 || underTrace.Source != "volume-empty" || len(underTrace.EligibleVolumes) != 0 || underTrace.Candidates != 0 || underTrace.BlocksDecoded != 0 || underTrace.BlocksSkipped != 0 {
		t.Fatalf("under-only absent volume got=%v err=%v trace=%+v", got, err, underTrace)
	}
}

func TestBareVolumeAnchorScopesWithoutForcingPathMatching(t *testing.T) {
	bare := queryOptions{Query: "F: nrrd", Limit: 20}
	pq, err := parseQuery(bare)
	if err != nil {
		t.Fatal(err)
	}
	if pq.MatchPath {
		t.Fatal("bare volume anchor forced path matching")
	}
	if got := queryVolumeConstraint(bare); got != "F:" {
		t.Fatalf("volume constraint = %q, want F:", got)
	}
	dropSatisfiedVolumeTerms(&pq, "F:")
	if len(pq.Terms) != 1 || pq.Terms[0] != "nrrd" {
		t.Fatalf("terms after satisfying volume = %v, want [nrrd]", pq.Terms)
	}

	for _, explicit := range []queryOptions{
		{Query: "path:F: nrrd"},
		{Query: "F: nrrd", MatchPath: true},
	} {
		parsed, err := parseQuery(explicit)
		if err != nil {
			t.Fatal(err)
		}
		if !parsed.MatchPath {
			t.Fatalf("explicit path query %+v lost path matching", explicit)
		}
		if got := queryVolumeConstraint(explicit); got != "F:" {
			t.Fatalf("explicit path volume constraint = %q, want F:", got)
		}
	}
}
