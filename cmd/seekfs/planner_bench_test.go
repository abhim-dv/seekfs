package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

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
	fixtureproj := add(20, 1, root, "fixtureproj-dev", uint32(os.ModeDir))
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
	fixtureproj := add(2, 1, root, "fixtureproj-dev", uint32(os.ModeDir))
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
