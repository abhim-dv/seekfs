package main

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

type deterministicOracleCase struct {
	name      string
	query     string
	matchPath bool
	limit     int
}

type deterministicOracleQuery struct {
	matchPath bool
	drive     string
	terms     []string
	exts      []string
	dirs      []string
	not       []deterministicOracleQuery
}

type deterministicOracleRecord struct {
	id        int
	path      string
	lowerPath string
	name      string
	lowerName string
	ext       string
}

func TestDeterministicOracleSubset(t *testing.T) {
	idx := deterministicOracleFixture()
	vol := newServiceVolumeIndex("deterministic-oracle.gsi", idx)
	cases := []deterministicOracleCase{
		{name: "downloads md", query: "Downloads md", matchPath: true, limit: 20},
		{name: "downloads md negation", query: "Downloads md !draft", matchPath: true, limit: 20},
		{name: "downloads nrrd", query: "Downloads nrrd", matchPath: true, limit: 20},
		{name: "downloads ext path", query: "ext:nrrd path:Downloads", limit: 20},
		{name: "downloads dir filter", query: "dir:Downloads nrrd", limit: 20},
		{name: "fixtureproj trainingdata path", query: `path:F:\fixtureproj trainingdata`, limit: 20},
		{name: "trainingdata dataset nrrd", query: "trainingdata Dataset nrrd", matchPath: true, limit: 20},
		{name: "node modules md", query: "path:node_modules md", limit: 20},
		{name: "no hit", query: "Downloads zzzz-nohit-md", matchPath: true, limit: 20},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := queryOptions{Query: tc.query, MatchPath: tc.matchPath, Limit: tc.limit}
			want, err := deterministicOracleSearch(idx, opts)
			if err != nil {
				t.Fatalf("oracle search: %v", err)
			}

			full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("full compact search: %v", err)
			}
			if got := deterministicOracleEntryPaths(full); !reflect.DeepEqual(got, want) {
				t.Fatalf("full compact paths = %v, want oracle %v", got, want)
			}

			planned, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
			if err != nil {
				t.Fatalf("planned compact search: %v", err)
			}
			if got := deterministicOracleEntryPaths(planned); !reflect.DeepEqual(got, want) {
				t.Fatalf("planned compact paths = %v, want oracle %v", got, want)
			}
		})
	}
}

func deterministicOracleFixture() *Index {
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
	}
	mod := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixNano()
	add := func(parent int32, name string, mode uint32) int32 {
		id := len(idx.Records)
		parentFRN := uint64(1)
		if parent >= 0 {
			parentFRN = idx.Records[parent].FRN
		}
		size := int64(0)
		if mode&uint32(os.ModeDir) == 0 {
			size = 1024
		}
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       uint64(id + 1),
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
			Mode:      mode,
			Size:      size,
			ModUnix:   mod,
		})
		return int32(id)
	}

	root := add(-1, ".", uint32(os.ModeDir))
	users := add(root, "Users", uint32(os.ModeDir))
	user := add(users, "exampleuser", uint32(os.ModeDir))
	downloads := add(user, "Downloads", uint32(os.ModeDir))
	add(downloads, "dataset-overview.md", 0)
	add(downloads, "draft-notes.md", 0)
	add(downloads, "scan-download.nrrd", 0)
	add(downloads, "labels.raw", 0)

	workspace := add(root, "workspace", uint32(os.ModeDir))
	trainingdata := add(workspace, "trainingdata", uint32(os.ModeDir))
	dataset := add(trainingdata, "Dataset", uint32(os.ModeDir))
	add(dataset, "heart-volume.nrrd", 0)
	add(dataset, "heart-labels.raw", 0)
	control := add(trainingdata, "control", uint32(os.ModeDir))
	add(control, "control-volume.nrrd", 0)

	fixtureproj := add(root, "fixtureproj", uint32(os.ModeDir))
	fixtureprojProject := add(fixtureproj, "projects", uint32(os.ModeDir))
	hadesModel := add(fixtureprojProject, "model", uint32(os.ModeDir))
	hadesTrainingdata := add(hadesModel, "trainingdata", uint32(os.ModeDir))
	hadesRaw := add(hadesTrainingdata, "raw files", uint32(os.ModeDir))
	add(hadesRaw, "volume-000.nrrd", 0)

	nodeModules := add(workspace, "node_modules", uint32(os.ModeDir))
	add(nodeModules, "README.md", 0)
	add(nodeModules, "package.json", 0)

	buildOrders(idx)
	return idx
}

func deterministicOracleSearch(idx *Index, opts queryOptions) ([]string, error) {
	query, err := deterministicOracleParse(opts)
	if err != nil {
		return nil, err
	}
	records := deterministicOracleRecords(idx)
	matches := make([]deterministicOracleRecord, 0, len(records))
	for _, record := range records {
		rec := idx.Records[record.id]
		if rec.Deleted {
			continue
		}
		if deterministicOracleMatches(record, query) {
			matches = append(matches, record)
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].lowerName == matches[j].lowerName {
			return matches[i].id < matches[j].id
		}
		return matches[i].lowerName < matches[j].lowerName
	})
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	if len(matches) > limit {
		matches = matches[:limit]
	}
	paths := make([]string, 0, len(matches))
	for _, match := range matches {
		paths = append(paths, match.path)
	}
	return paths, nil
}

func deterministicOracleParse(opts queryOptions) (deterministicOracleQuery, error) {
	query := deterministicOracleQuery{
		matchPath: opts.MatchPath || deterministicOracleLooksPathScoped(opts.Query),
	}
	for _, raw := range strings.Fields(opts.Query) {
		deterministicOracleApplyToken(&query, raw)
	}
	deterministicOraclePromoteDottedExts(&query)
	return query, nil
}

func deterministicOracleApplyToken(query *deterministicOracleQuery, raw string) {
	if raw == "" {
		return
	}
	if (strings.HasPrefix(raw, "!") || strings.HasPrefix(raw, "-")) && len(raw) > 1 {
		neg := deterministicOracleQuery{matchPath: query.matchPath}
		deterministicOracleApplyToken(&neg, raw[1:])
		deterministicOraclePromoteDottedExts(&neg)
		query.not = append(query.not, neg)
		return
	}

	lower := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lower, "path:"):
		query.matchPath = true
		value := raw[len("path:"):]
		for _, term := range deterministicOraclePlainTerms(value, true) {
			deterministicOracleAddTerm(query, term)
		}
	case strings.HasPrefix(lower, "ext:"):
		ext := strings.TrimPrefix(raw[len("ext:"):], ".")
		if ext != "" {
			query.exts = append(query.exts, strings.ToLower(ext))
		}
	case strings.HasPrefix(lower, "dir:"):
		dir := raw[len("dir:"):]
		if dir != "" {
			query.dirs = append(query.dirs, strings.ToLower(dir))
		}
	default:
		for _, term := range deterministicOraclePlainTerms(raw, query.matchPath) {
			deterministicOracleAddTerm(query, term)
		}
	}
}

func deterministicOraclePromoteDottedExts(query *deterministicOracleQuery) {
	if !query.matchPath && len(query.dirs) == 0 {
		return
	}
	terms := query.terms[:0]
	for _, term := range query.terms {
		if ext, ok := deterministicOracleDottedExt(term); ok {
			query.exts = append(query.exts, ext)
			continue
		}
		terms = append(terms, term)
	}
	query.terms = terms
}

func deterministicOracleAddTerm(query *deterministicOracleQuery, term string) {
	term = strings.ToLower(strings.TrimSpace(term))
	if term == "" || term == "." {
		return
	}
	if deterministicOracleIsDrive(term) {
		query.drive = strings.ToUpper(term)
		return
	}
	query.terms = append(query.terms, term)
}

func deterministicOraclePlainTerms(raw string, matchPath bool) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if !matchPath || !strings.ContainsAny(raw, `\/`) {
		return []string{strings.ToLower(raw)}
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == '\\' || r == '/' })
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || part == "." {
			continue
		}
		out = append(out, strings.ToLower(part))
	}
	if len(out) == 0 {
		return []string{strings.ToLower(raw)}
	}
	return out
}

func deterministicOracleRecords(idx *Index) []deterministicOracleRecord {
	records := make([]deterministicOracleRecord, 0, len(idx.Records))
	for id, rec := range idx.Records {
		path := deterministicOraclePath(idx, id)
		name := rec.Name
		records = append(records, deterministicOracleRecord{
			id:        id,
			path:      path,
			lowerPath: strings.ToLower(path),
			name:      name,
			lowerName: strings.ToLower(name),
			ext:       deterministicOracleExt(name),
		})
	}
	return records
}

func deterministicOraclePath(idx *Index, id int) string {
	parts := make([]string, 0, 8)
	for id >= 0 && id < len(idx.Records) {
		rec := idx.Records[id]
		if rec.Name != "" && rec.Name != "." {
			parts = append(parts, rec.Name)
		}
		if rec.Parent < 0 || int(rec.Parent) == id {
			break
		}
		id = int(rec.Parent)
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	if len(parts) == 0 {
		return idx.Volume + `\`
	}
	return idx.Volume + `\` + strings.Join(parts, `\`)
}

func deterministicOracleMatches(record deterministicOracleRecord, query deterministicOracleQuery) bool {
	if query.drive != "" && !strings.HasPrefix(strings.ToUpper(record.path), query.drive+`\`) {
		return false
	}
	haystack := record.lowerName
	if query.matchPath {
		haystack = record.lowerPath
	}
	for _, term := range query.terms {
		if !strings.Contains(haystack, term) {
			return false
		}
	}
	for _, ext := range query.exts {
		if record.ext != ext {
			return false
		}
	}
	for _, dir := range query.dirs {
		if !strings.Contains(record.lowerPath, dir) {
			return false
		}
	}
	for _, neg := range query.not {
		if deterministicOracleMatches(record, neg) {
			return false
		}
	}
	return true
}

func deterministicOracleLooksPathScoped(query string) bool {
	for _, field := range strings.Fields(query) {
		lower := strings.ToLower(field)
		if deterministicOracleIsDrive(lower) || strings.HasPrefix(lower, "path:") || strings.ContainsAny(field, `\/`) {
			return true
		}
	}
	return false
}

func deterministicOracleIsDrive(term string) bool {
	if len(term) != 2 || term[1] != ':' {
		return false
	}
	c := term[0]
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func deterministicOracleDottedExt(term string) (string, bool) {
	if len(term) < 2 || len(term) > 6 || term[0] != '.' || strings.ContainsAny(term, `\/*?[]:`) {
		return "", false
	}
	ext := term[1:]
	for _, r := range ext {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			continue
		}
		return "", false
	}
	return ext, true
}

func deterministicOracleExt(name string) string {
	name = strings.TrimRight(name, `\/`)
	lastSlash := strings.LastIndexAny(name, `\/`)
	if lastSlash >= 0 {
		name = name[lastSlash+1:]
	}
	dot := strings.LastIndexByte(name, '.')
	if dot < 0 || dot == len(name)-1 {
		return ""
	}
	return strings.ToLower(name[dot+1:])
}

func deterministicOracleEntryPaths(entries []Entry) []string {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}
	return paths
}
