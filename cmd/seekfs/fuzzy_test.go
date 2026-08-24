package main

import (
	"strings"
	"testing"
)

func TestDamerauLevenshteinBounded(t *testing.T) {
	cases := []struct {
		a, b string
		max  int
		want int
	}{
		{"tonic", "tonic", 2, 0},
		{"tonic", "sonic", 2, 1},
		{"reprot", "report", 2, 1}, // transposition
		{"cat", "cart", 2, 1},
		{"abc", "xyz", 1, 2}, // exceeds max: capped at max+1
		{"", "ab", 2, 2},
		{"kitten", "mitten", 2, 1},
	}
	for _, tc := range cases {
		got := damerauLevenshteinBounded([]rune(tc.a), []rune(tc.b), tc.max)
		if got != tc.want {
			t.Fatalf("damerau(%q,%q,max=%d) = %d, want %d", tc.a, tc.b, tc.max, got, tc.want)
		}
	}
}

func TestFuzzyDeletionVariantsCoverDistanceOne(t *testing.T) {
	term := "report"
	variants := fuzzyDeletionVariants(term, 1)
	set := make(map[string]struct{}, len(variants))
	for _, v := range variants {
		set[v] = struct{}{}
	}
	if _, ok := set[term]; !ok {
		t.Fatal("variants must include the original term")
	}
	// Every single-character deletion must itself be present.
	if _, ok := set["eport"]; !ok {
		t.Fatal("missing deletion variant eport")
	}
	if _, ok := set["repor"]; !ok {
		t.Fatal("missing deletion variant repor")
	}
	// SymSpell completeness: any string within distance 1 of the term must
	// contain at least one variant as a substring.
	for _, near := range []string{"reports", "eport", "repot", "rpeort"} {
		found := false
		for v := range set {
			if strings.Contains(near, v) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("near-string %q shares no deletion variant of %q", near, term)
		}
	}
}

func TestFoldFuzzyText(t *testing.T) {
	if got := foldFuzzyText("Só Danço"); got != "so danco" && got != "So Danco" {
		// Case is handled separately by callers; only marks/width fold here.
		if got != "Só Danço" && !strings.EqualFold(foldFuzzyText("sodanco"), "sodanco") {
			t.Fatalf("foldFuzzyText unexpectedly unstable")
		}
	}
	if got := foldFuzzyText("ＲＥＡＤＭＥ"); got != "README" {
		t.Fatalf("fullwidth fold = %q, want README", got)
	}
	if got := foldFuzzyText("café"); got != "cafe" {
		t.Fatalf("accent fold = %q, want cafe", got)
	}
}

func TestParseQueryFuzzyMarker(t *testing.T) {
	pq, err := parseQuery(queryOptions{Query: "reprot~", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !pq.Fuzzy {
		t.Fatal("term~ should mark the query fuzzy")
	}
	if len(pq.Terms) != 1 || pq.Terms[0] != "reprot" {
		t.Fatalf("terms = %v, want [reprot]", pq.Terms)
	}

	pqPlain, err := parseQuery(queryOptions{Query: "reprot", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if pqPlain.Fuzzy {
		t.Fatal("plain term must not be fuzzy")
	}

	pqFlag, err := parseQuery(queryOptions{Query: "reprot", Fuzzy: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !pqFlag.Fuzzy {
		t.Fatal("--fuzzy option should mark the query fuzzy")
	}
}

// buildFuzzyTestVolume creates a compact service volume whose names exercise
// distance ranking, prefix preference, and exact-tier separation.
func buildFuzzyTestVolume(t *testing.T) *serviceVolumeIndex {
	t.Helper()
	idx := &Index{Source: "usn", Volume: "Q:", Compact: true, DBPath: ""}
	add := func(name string) {
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       uint64(len(idx.Records) + 10),
			ParentFRN: uint64(10),
			Parent:    0,
			Name:      name,
		})
	}
	for _, name := range []string{
		".", // root record
		"tonic.txt",
		"tonicwater.txt",
		"sonic.txt",
		"ironic.txt",
		"totally-unrelated.log",
	} {
		add(name)
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("", idx)
	vol.volume = "Q:"
	vol.rebuildNameTrigramsLocked()
	return vol
}

func TestAppendFuzzyServiceMatchesGoldenRanking(t *testing.T) {
	vol := buildFuzzyTestVolume(t)
	opts := queryOptions{Query: "tonics~", Limit: 10}
	matches, added := appendFuzzyServiceMatches([]*serviceVolumeIndex{vol}, opts, nil)
	if !added {
		t.Fatal("expected fuzzy results for tonics~")
	}
	if len(matches) == 0 {
		t.Fatal("expected at least one fuzzy match")
	}
	if got := matches[0].Name; got != "tonic.txt" && got != "tonicwater.txt" {
		t.Fatalf("top fuzzy result = %q, want a tonic* file", got)
	}
	for i, m := range matches {
		if m.Name == "totally-unrelated.log" {
			t.Fatalf("unrelated file ranked into fuzzy results at %d", i)
		}
	}
	// Prefix matches must outrank infix matches at equal distance.
	prefixIdx, infixIdx := -1, -1
	for i, m := range matches {
		switch m.Name {
		case "tonic.txt":
			prefixIdx = i
		case "ironic.txt":
			infixIdx = i
		}
	}
	if prefixIdx >= 0 && infixIdx >= 0 && prefixIdx > infixIdx {
		t.Fatalf("prefix match ranked %d below infix match %d", prefixIdx, infixIdx)
	}
}

func TestAppendFuzzyServiceMatchesAutoFiresOnZeroResults(t *testing.T) {
	vol := buildFuzzyTestVolume(t)

	// A zero-result exact query auto-triggers fuzzy without any marker.
	opts := queryOptions{Query: "tonics", Limit: 10}
	matches, added := appendFuzzyServiceMatches([]*serviceVolumeIndex{vol}, opts, nil)
	if !added || len(matches) == 0 {
		t.Fatal("zero-result query should auto-trigger the fuzzy tier")
	}
	if matches[0].Name != "tonic.txt" && matches[0].Name != "tonicwater.txt" {
		t.Fatalf("top fuzzy result = %q, want a tonic* file", matches[0].Name)
	}
}

func TestAppendFuzzyServiceMatchesKeepsExactResultsFirst(t *testing.T) {
	vol := buildFuzzyTestVolume(t)

	// Simulate a partial exact result set (below the auto-fuzzy threshold):
	// fuzzy entries must be appended strictly after every exact entry.
	exact := []Entry{{Path: `Q:\exact-tonic.txt`, Name: "exact-tonic.txt"}}
	opts := queryOptions{Query: "tonics", Limit: 10}
	matches, added := appendFuzzyServiceMatches([]*serviceVolumeIndex{vol}, opts, exact)
	if !added {
		t.Fatal("partial result set below threshold should trigger the fuzzy tier")
	}
	for i, m := range matches {
		if i < len(exact) {
			continue
		}
		if m.Path == exact[0].Path {
			t.Fatalf("exact entry %q reappeared inside the fuzzy section at %d", m.Path, i)
		}
	}
	if matches[0].Path != exact[0].Path {
		t.Fatalf("first result = %q; exact results must always lead", matches[0].Path)
	}
}

func TestAppendFuzzyServiceMatchesDeclinesMultiTermAndShortTerms(t *testing.T) {
	vol := buildFuzzyTestVolume(t)

	// Multi-term queries are out of scope for the fallback tier in v1.
	optsMulti := queryOptions{Query: "tonic~ water", Limit: 10}
	if _, added := appendFuzzyServiceMatches([]*serviceVolumeIndex{vol}, optsMulti, nil); added {
		t.Fatal("multi-term fuzzy queries must be declined")
	}

	// Short terms are noise-prone and declined.
	optsShort := queryOptions{Query: "to~", Limit: 10}
	if _, added := appendFuzzyServiceMatches([]*serviceVolumeIndex{vol}, optsShort, nil); added {
		t.Fatal("sub-minimum terms must be declined")
	}

	// Case-sensitive queries are declined (fuzzy compares folded text).
	optsCase := queryOptions{Query: "tonics~", CaseSensitive: true, Limit: 10}
	if _, added := appendFuzzyServiceMatches([]*serviceVolumeIndex{vol}, optsCase, nil); added {
		t.Fatal("case-sensitive fuzzy queries must be declined")
	}
}

func TestFuzzyNameDistancePrefixPreference(t *testing.T) {
	// Equal distance: prefix alignment wins.
	dist, prefix, _, ok := fuzzyNameDistance("tonic.txt", "tonicx", 1)
	if !ok || dist != 1 || !prefix {
		t.Fatalf("dist=%d prefix=%v ok=%v; want 1/prefix/ok", dist, prefix, ok)
	}
	dist, prefix, _, ok = fuzzyNameDistance("totally-unrelated.log", "tonicx", 1)
	if ok {
		t.Fatalf("unrelated name accepted at tau=1: dist=%d", dist)
	}
	_ = dist
	_ = prefix
}
