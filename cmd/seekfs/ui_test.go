//go:build seekfs_ui && (production || dev)

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeUIQueryForServicePathFilter(t *testing.T) {
	query, matchPath := normalizeUIQueryForService("path:Downloads ext:png", false)
	if !matchPath {
		t.Fatal("path: did not enable path matching")
	}
	if query != "Downloads ext:png" {
		t.Fatalf("query = %q, want %q", query, "Downloads ext:png")
	}
}

func TestUIServiceArgsUseCurrentPipeAndDBs(t *testing.T) {
	args := uiServiceArgs(`\\.\pipe\seekfs-test`, []string{`C:\idx\c.gsi`, `F:\idx\f.gsi`})
	want := []string{
		"service",
		"-pipe", `\\.\pipe\seekfs-test`,
		"-sddl", defaultServiceSDDL,
		"-db", `C:\idx\c.gsi`,
		"-db", `F:\idx\f.gsi`,
	}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q; args=%v", i, args[i], want[i], args)
		}
	}
}

func TestNormalizeUIQueryForServiceEverythingAliases(t *testing.T) {
	cases := []struct {
		in        string
		wantQuery string
		wantPath  bool
	}{
		{"extension:.go", "ext:go", false},
		{"folder:", "type:dir", false},
		{"file:", "type:file", false},
		{"folder:regex:^src$", "type:dir regex:^src$", false},
		{"sz:>10mb", "size:>10mb", false},
		{"date-modified:today", "dm:today", false},
		{"location:Downloads", "dir:Downloads", false},
		{"name:main.go", "main.go", false},
	}
	for _, tc := range cases {
		gotQuery, gotPath := normalizeUIQueryForService(tc.in, false)
		if gotQuery != tc.wantQuery || gotPath != tc.wantPath {
			t.Fatalf("normalizeUIQueryForService(%q) = (%q, %v), want (%q, %v)", tc.in, gotQuery, gotPath, tc.wantQuery, tc.wantPath)
		}
	}
}

func TestNormalizeUIQueryKeepsDottedSubstringTerms(t *testing.T) {
	cases := []struct {
		in        string
		wantQuery string
		wantPath  bool
	}{
		{".opencode", ".opencode", false},
		{"path:C: .opencode", "C: .opencode", true},
		{"path:Downloads .nrrd", "Downloads .nrrd", true},
	}
	for _, tc := range cases {
		gotQuery, gotPath := normalizeUIQueryForService(tc.in, false)
		if gotQuery != tc.wantQuery || gotPath != tc.wantPath {
			t.Fatalf("normalizeUIQueryForService(%q) = (%q, %v), want (%q, %v)", tc.in, gotQuery, gotPath, tc.wantQuery, tc.wantPath)
		}
	}
}

func TestFrontendDoesNotRewriteDottedSubstringToExtension(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("ui_frontend", "main.js"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ext:${query.slice(1)}") {
		t.Fatal("frontend rewrites single dotted terms to ext:, breaking dotted substring search")
	}
}

func TestIncompleteUIQuerySuppressesTrailingDotAndFieldPrefix(t *testing.T) {
	cases := []string{
		"path:Downloads .",
		"ext:",
		"path:Downloads ext:",
	}
	for _, query := range cases {
		if !incompleteUIQuery(query) {
			t.Fatalf("incompleteUIQuery(%q) = false, want true", query)
		}
	}
	for _, query := range []string{"path:Downloads .nrrd", "path:.nrrd", "Downloads ext:nrrd"} {
		if incompleteUIQuery(query) {
			t.Fatalf("incompleteUIQuery(%q) = true, want false", query)
		}
	}
}

func TestUISearchDoesNotContactServiceForIncompleteQuery(t *testing.T) {
	app := &UIApp{pipeName: `\\.\pipe\seekfs-does-not-exist`, defaultLimit: 200}
	resp := app.search(UISearchRequest{Query: "path:Downloads .", Limit: 200}, 1)
	if !resp.OK || resp.Message != "Keep typing" || len(resp.Results) != 0 {
		t.Fatalf("response = %+v, want immediate keep-typing response", resp)
	}
}

func TestUIServiceSequenceIsSessionScoped(t *testing.T) {
	app := &UIApp{uiSeqBase: 10_000}
	if got := app.serviceUISeq(1); got != 10_001 {
		t.Fatalf("serviceUISeq(1) = %d, want 10001", got)
	}
	if got := app.serviceUISeq(2); got != 10_002 {
		t.Fatalf("serviceUISeq(2) = %d, want 10002", got)
	}
	if got := app.serviceUISeq(0); got != 0 {
		t.Fatalf("serviceUISeq(0) = %d, want 0 for synchronous calls", got)
	}
}

func TestUIEverythingQueriesMatchServiceSearchFixture(t *testing.T) {
	idx := commonSearchFixture()
	idx.Records = append(idx.Records,
		CompactRecord{FRN: 17, ParentFRN: 3, Parent: 2, Name: "Downloads", Mode: uint32(os.ModeDir)},
		CompactRecord{FRN: 18, ParentFRN: 17, Parent: 16, Name: "scan.nrrd"},
		CompactRecord{FRN: 19, ParentFRN: 17, Parent: 16, Name: "notes.txt"},
		CompactRecord{FRN: 20, ParentFRN: 2, Parent: 1, Name: "ai.opencode.desktop", Mode: uint32(os.ModeDir)},
	)
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []struct {
		raw       string
		wantQuery string
		wantPath  bool
		wantNames []string
	}{
		{
			raw:       "path:workspace extension:.go",
			wantQuery: "workspace ext:go",
			wantPath:  true,
			wantNames: []string{"main.go", "search_test.go"},
		},
		{
			raw:       "location:Assets extension:.dat",
			wantQuery: "dir:Assets ext:dat",
			wantNames: []string{"sample.dat"},
		},
		{
			raw:       "folder:Assets",
			wantQuery: "type:dir Assets",
			wantNames: []string{"Assets"},
		},
		{
			raw:       "file:extension:.go",
			wantQuery: "type:file ext:go",
			wantNames: []string{"main.go", "search_test.go"},
		},
		{
			raw:       "full-path-name:Downstream ext:dat",
			wantQuery: "Downstream ext:dat",
			wantPath:  true,
			wantNames: []string{"sibling.dat"},
		},
		{
			raw:       "path:Downloads .nrrd",
			wantQuery: "Downloads .nrrd",
			wantPath:  true,
			wantNames: []string{"scan.nrrd"},
		},
		{
			raw:       "path:Downloads",
			wantQuery: "Downloads",
			wantPath:  true,
			wantNames: []string{"Downloads", "notes.txt", "scan.nrrd"},
		},
		{
			raw:       ".opencode",
			wantQuery: ".opencode",
			wantNames: []string{"ai.opencode.desktop"},
		},
		{
			raw:       "path:C: .opencode",
			wantQuery: "C: .opencode",
			wantPath:  true,
			wantNames: []string{"ai.opencode.desktop"},
		},
		{
			raw:       "ext:opencode",
			wantQuery: "ext:opencode",
			wantNames: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			query, matchPath := normalizeUIQueryForService(tc.raw, false)
			if query != tc.wantQuery || matchPath != tc.wantPath {
				t.Fatalf("normalizeUIQueryForService(%q) = (%q, %v), want (%q, %v)", tc.raw, query, matchPath, tc.wantQuery, tc.wantPath)
			}
			opts := queryOptions{Query: query, MatchPath: matchPath, Limit: 20}
			got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
			if err != nil {
				t.Fatalf("searchServiceVolumes: %v", err)
			}
			if !sameStringSet(namesOf(got), tc.wantNames) {
				t.Fatalf("names = %v, want %v", namesOf(got), tc.wantNames)
			}
		})
	}
}

func TestUIStrictSpaceSplitDoesNotInferFusedPathExtensions(t *testing.T) {
	cases := []struct {
		raw       string
		wantQuery string
		wantPath  bool
	}{
		{"path:C:.nrrd", "C:.nrrd", true},
		{"path:C:.NRRD", "C:.NRRD", true},
		{"path:.nrrd", "path:.nrrd", true},
		{"path:Downloads.nrrd", "Downloads.nrrd", true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			gotQuery, gotPath := normalizeUIQueryForService(tc.raw, false)
			if gotQuery != tc.wantQuery || gotPath != tc.wantPath {
				t.Fatalf("normalizeUIQueryForService(%q) = (%q, %v), want (%q, %v)", tc.raw, gotQuery, gotPath, tc.wantQuery, tc.wantPath)
			}
		})
	}
}

func TestUIResultsPreferServiceRowsOverPathOnlyFallback(t *testing.T) {
	size := int64(42 * 1024)
	resp := serviceResponse{
		Results: []string{`C:\Downloads\scan.nrrd`},
		Rows: []jsonResult{{
			Path:     `C:\Downloads\scan.nrrd`,
			Name:     "scan.nrrd",
			Size:     &size,
			Modified: "2026-06-29T10:09:00Z",
		}},
	}
	got := uiResultsFromServiceResponse(resp)
	if len(got) != 1 {
		t.Fatalf("results = %d, want 1", len(got))
	}
	if got[0].Path != `C:\Downloads\scan.nrrd` || got[0].Name != "scan.nrrd" || got[0].Size != size || !got[0].Exists {
		t.Fatalf("result = %+v, want indexed row metadata", got[0])
	}
	if got[0].Dir != `C:\Downloads` {
		t.Fatalf("dir = %q, want C:\\Downloads", got[0].Dir)
	}
}

func TestEntryToJSONUsesIndexedSizeAndModifiedTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.json")
	mod := time.Date(2026, 6, 29, 13, 10, 47, 0, time.Local)
	row := entryToJSON(Entry{Path: path, Name: "metrics.json", Size: 6, ModUnix: mod.UnixNano()})
	if row.Size == nil || *row.Size != 6 {
		t.Fatalf("size = %v, want indexed size 6", row.Size)
	}
	if row.Modified == "" {
		t.Fatal("indexed modified time was not serialized")
	}
}
