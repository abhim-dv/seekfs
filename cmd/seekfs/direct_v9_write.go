package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

func directV9WriteHeaderAndBase(ctx context.Context, cw *countingWriter, finalPath, tokenPath, frnPath string, opts directV9BuildOptions, recordCount int, nameBlobLen int64) (int, error) {
	if err := binary.Write(cw, binary.LittleEndian, diskHeader{
		Magic:       indexMagicV9,
		Version:     indexVersionV9,
		EntryCount:  uint64(recordCount),
		RootCount:   uint64(len(opts.Roots)),
		BuiltUnix:   opts.BuiltAt.UnixNano(),
		JournalID:   opts.JournalID,
		Checkpoint:  opts.Checkpoint,
		Compact:     compactDiskFlag | compactDiskAttrsFlag | directV9CompactFlags(recordCount),
		NameBlobLen: uint64(nameBlobLen),
		TokenCount:  uint64(recordCount),
	}); err != nil {
		return 0, err
	}
	// The section table offset is backpatched after all families are emitted.
	if err := binary.Write(cw, binary.LittleEndian, uint64(0)); err != nil {
		return 0, err
	}
	for _, value := range []string{opts.Source, opts.Volume, ""} {
		if err := writeString(cw, value); err != nil {
			return 0, err
		}
	}
	for _, root := range opts.Roots {
		if err := writeString(cw, root); err != nil {
			return 0, err
		}
	}
	nameFile, err := os.Open(finalPath)
	if err != nil {
		return 0, err
	}
	defer nameFile.Close()
	reader := bufio.NewReaderSize(nameFile, 256*1024)
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}
		rec, readErr := readDirectV9SpoolRecord(reader)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return 0, readErr
		}
		if _, err := io.WriteString(cw, rec.Name); err != nil {
			return 0, err
		}
	}
	tokens, err := os.Open(tokenPath)
	if err != nil {
		return 0, err
	}
	if _, err := io.Copy(cw, tokens); err != nil {
		_ = tokens.Close()
		return 0, err
	}
	if err := tokens.Close(); err != nil {
		return 0, err
	}
	// Stream records a second time.  The FRN file is mapped read-only for
	// in-memory parent lookups instead of issuing a ReadAt syscall per record.
	records, err := os.Open(finalPath)
	if err != nil {
		return 0, err
	}
	defer records.Close()
	frnMap, frns, err := directV9MapFRNs(frnPath, recordCount)
	if err != nil {
		return 0, err
	}
	if frnMap != nil {
		defer frnMap.close()
	}
	recordReader := bufio.NewReaderSize(records, 256*1024)
	wide := directV9CompactFlags(recordCount)&compactDiskWideRefsFlag != 0
	for id := 0; id < recordCount; id++ {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}
		rec, readErr := readDirectV9SpoolRecord(recordReader)
		if readErr != nil {
			return 0, readErr
		}
		parent := uint32(compactNarrowParentSentinel)
		if wide {
			parent = compactWideParentSentinel
		}
		if rec.ParentFRN != 0 {
			if parentID := directV9LookupIDMapped(frns, rec.ParentFRN); parentID >= 0 {
				parent = uint32(parentID)
			}
		}
		if err := binary.Write(cw, binary.LittleEndian, rec.FRN); err != nil {
			return 0, err
		}
		if err := binary.Write(cw, binary.LittleEndian, rec.ParentFRN); err != nil {
			return 0, err
		}
		if err := writeCompactRecordRefs(cw, parent, uint32(id), wide); err != nil {
			return 0, err
		}
		for _, value := range []any{rec.Mode, rec.Size, rec.ModUnix} {
			if err := binary.Write(cw, binary.LittleEndian, value); err != nil {
				return 0, err
			}
		}
		if err := binary.Write(cw, binary.LittleEndian, uint8(0)); err != nil {
			return 0, err
		}
	}
	return int(cw.n), nil
}

func directV9CompactFlags(recordCount int) uint32 {
	flags := uint32(0)
	if compactNeedsWideDiskRecords(recordCount, recordCount) {
		flags |= compactDiskWideRefsFlag
	}
	return flags
}

func directV9WriteAtomic(ctx context.Context, opts directV9BuildOptions, finalPath, frnPath string, recordCount int, nameBlobLen int64, tokenPath string, rankSpecs []directV9RankSpec, baseScratch int64, reports *[]directV9RankReport, sectionReports *[]directV9SectionReport, scratchHigh *int64, owned *[]string) (int64, error) {
	reportPhase := func(name string, d time.Duration) {
		if opts.PhaseReporter != nil {
			opts.PhaseReporter(name, d)
		}
	}
	dir := filepath.Dir(opts.OutputPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	f, err := os.CreateTemp(dir, filepath.Base(opts.OutputPath)+".direct-*.tmp")
	if err != nil {
		return 0, err
	}
	tmp := f.Name()
	*owned = append(*owned, tmp)
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	bw := bufio.NewWriterSize(f, 4*1024*1024)
	cw := &countingWriter{w: bw}
	t0 := time.Now()
	if _, err := directV9WriteHeaderAndBase(ctx, cw, finalPath, tokenPath, frnPath, opts, recordCount, nameBlobLen); err != nil {
		cleanup()
		return 0, err
	}
	reportPhase("header-base", time.Since(t0))
	entries := make([]indexSectionTableEntry, 0, len(rankSpecs))
	rankScratchPaths := make([]string, 0, len(rankSpecs))
	t0 = time.Now()
	sharedRankRuns, sharedRankErr := directV9BuildRankRunsShared(ctx, finalPath, filepath.Dir(finalPath), opts.RunRecords, opts.RankWorkers, rankSpecs, owned)
	if sharedRankErr != nil {
		cleanup()
		return 0, sharedRankErr
	}
	reportPhase("rank-runs", time.Since(t0))
	// Compute every rank family concurrently with a bounded worker pool, then
	// emit the finished sections serially in tag order.  The section bytes are
	// written to private temp files during the merge so the parallel phase is
	// memory-bounded and the serial phase stays byte-deterministic.
	rankWorkers := opts.RankWorkers
	if rankWorkers < 1 {
		rankWorkers = 1
	}
	if rankWorkers > 16 {
		rankWorkers = 16
	}
	type rankFamilyResult struct {
		spec     directV9RankSpec
		entry    indexSectionTableEntry
		runBytes int64
		report   directV9RankReport
		section  directV9SectionReport
		rankPath string
	}
	results := make([]*rankFamilyResult, len(rankSpecs))
	ctxRanks, cancelRanks := context.WithCancel(ctx)
	defer cancelRanks()
	t0 = time.Now()
	jobs := make(chan int, len(rankSpecs))
	var workerWG sync.WaitGroup
	var firstRankErr error
	var rankErrMu sync.Mutex
	for worker := 0; worker < rankWorkers; worker++ {
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			for index := range jobs {
				spec := rankSpecs[index]
				runs := sharedRankRuns.Runs[spec.Name]
				liveCount := sharedRankRuns.LiveCounts[spec.Name]
				maxRunBytes := sharedRankRuns.MaxBytes[spec.Name]
				var runBytes int64
				for _, run := range runs {
					runBytes += run.bytes
				}
				sectionPath := filepath.Join(filepath.Dir(finalPath), fmt.Sprintf("direct-v9-rank-section-%s.tmp", spec.Name))
				rankPath := filepath.Join(filepath.Dir(finalPath), fmt.Sprintf("direct-v9-rank-by-id-%s.tmp", spec.Name))
				if _, err := directV9ComputeRankFamily(ctxRanks, spec.Tag, runs, recordCount, liveCount, sectionPath, rankPath, owned); err != nil {
					rankErrMu.Lock()
					if firstRankErr == nil {
						firstRankErr = err
						cancelRanks()
					}
					rankErrMu.Unlock()
					continue
				}
				results[index] = &rankFamilyResult{
					spec:     spec,
					runBytes: runBytes,
					report:   directV9RankReport{Name: spec.Name, Tag: spec.Tag, Runs: len(runs), RunBytes: runBytes, MaxRunBytes: maxRunBytes},
					rankPath: rankPath,
				}
			}
		}()
	}
	for index := range rankSpecs {
		select {
		case jobs <- index:
		case <-ctxRanks.Done():
			break
		}
	}
	close(jobs)
	workerWG.Wait()
	reportPhase("rank-families", time.Since(t0))
	if firstRankErr != nil {
		cleanup()
		return 0, firstRankErr
	}
	for _, result := range results {
		if result == nil {
			cleanup()
			return 0, errors.New("direct v9 rank family was not computed")
		}
		spec := result.spec
		rankRuns := sharedRankRuns.Runs[spec.Name]
		sectionPath := filepath.Join(filepath.Dir(finalPath), fmt.Sprintf("direct-v9-rank-section-%s.tmp", spec.Name))
		entry, emitErr := directV9EmitRankSection(cw, spec.Tag, sectionPath)
		if emitErr != nil {
			cleanup()
			return 0, emitErr
		}
		result.entry = entry
		if high := baseScratch + result.runBytes + int64(recordCount)*4; high > *scratchHigh {
			*scratchHigh = high
		}
		entries = append(entries, entry)
		retainedPath := result.rankPath
		*owned = append(*owned, retainedPath)
		rankScratchPaths = append(rankScratchPaths, retainedPath)
		if high := baseScratch + result.runBytes + int64(recordCount)*4*int64(len(rankScratchPaths)+1); high > *scratchHigh {
			*scratchHigh = high
		}
		if reports != nil {
			result.report.Bytes = int64(entry.length)
			*reports = append(*reports, result.report)
		}
		if sectionReports != nil {
			*sectionReports = append(*sectionReports, directV9SectionReport{Name: spec.Name, Tag: spec.Tag, Runs: len(rankRuns), Bytes: int64(entry.length), ScratchBytes: baseScratch + result.runBytes + int64(recordCount)*4})
		}
		for _, run := range rankRuns {
			_ = os.Remove(run.path)
		}
		_ = os.Remove(sectionPath)
	}
	t0 = time.Now()
	topologyEntries, topologyReports, topologyErr := directV9WriteTopologySections(ctx, cw, finalPath, frnPath, filepath.Dir(finalPath), recordCount, opts.RunRecords, owned, scratchHigh)
	if topologyErr != nil {
		cleanup()
		return 0, topologyErr
	}
	reportPhase("topology", time.Since(t0))
	entries = append(entries, topologyEntries...)
	if sectionReports != nil {
		*sectionReports = append(*sectionReports, topologyReports...)
	}
	parentPath := filepath.Join(filepath.Dir(finalPath), "direct-v9-parents.tmp")
	sizesPath := filepath.Join(filepath.Dir(finalPath), "direct-v9-sizes.tmp")
	t0 = time.Now()
	subtreeEntries, subtreeReports, subtreeRankPaths, subtreeErr := directV9WriteSubtreeSection(ctx, cw, parentPath, sizesPath, recordCount, rankScratchPaths, owned, scratchHigh)
	if subtreeErr != nil {
		cleanup()
		return 0, subtreeErr
	}
	reportPhase("subtree", time.Since(t0))
	entries = append(entries, subtreeEntries...)
	if sectionReports != nil {
		*sectionReports = append(*sectionReports, subtreeReports...)
	}
	t0 = time.Now()
	auxEntries, auxReports, auxErr := directV9WriteAuxiliarySections(ctx, cw, finalPath, recordCount, opts.RunRecords, rankScratchPaths[0], subtreeRankPaths, filepath.Dir(finalPath), owned, scratchHigh)
	if auxErr != nil {
		cleanup()
		return 0, auxErr
	}
	reportPhase("aux", time.Since(t0))
	entries = append(entries, auxEntries...)
	if sectionReports != nil {
		*sectionReports = append(*sectionReports, auxReports...)
	}
	for _, rankPath := range rankScratchPaths {
		_ = os.Remove(rankPath)
	}
	for _, rankPath := range subtreeRankPaths {
		_ = os.Remove(rankPath)
	}
	t0 = time.Now()
	if err := writeAlignment(cw, 8); err != nil {
		cleanup()
		return 0, err
	}
	tableOffset := uint64(cw.n)
	if err := binary.Write(cw, binary.LittleEndian, uint32(len(entries))); err != nil {
		cleanup()
		return 0, err
	}
	for _, entry := range entries {
		for _, value := range []any{entry.tag, entry.offset, entry.length, entry.flags} {
			if err := binary.Write(cw, binary.LittleEndian, value); err != nil {
				cleanup()
				return 0, err
			}
		}
	}
	if err := bw.Flush(); err != nil {
		cleanup()
		return 0, err
	}
	if _, err := f.Seek(int64(binary.Size(diskHeader{})), io.SeekStart); err != nil {
		cleanup()
		return 0, err
	}
	var patch [8]byte
	binary.LittleEndian.PutUint64(patch[:], tableOffset)
	if _, err := f.Write(patch[:]); err != nil {
		cleanup()
		return 0, err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return 0, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, opts.OutputPath); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if dirFile, openErr := os.Open(dir); openErr == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	reportPhase("finalize", time.Since(t0))
	return fileSize(opts.OutputPath)
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func directV9RemoveOwned(paths []string) {
	for i := len(paths) - 1; i >= 0; i-- {
		_ = os.Remove(paths[i])
	}
}

func buildDirectV9(ctx context.Context, opts directV9BuildOptions) (stats directV9BuildStats, err error) {
	start := time.Now()
	report := func(name string, d time.Duration) {
		if opts.PhaseReporter != nil {
			opts.PhaseReporter(name, d)
		}
	}
	if opts.OutputPath == "" || opts.Records == nil {
		return stats, errors.New("direct v9 build requires output path and record source")
	}
	if opts.BuiltAt.IsZero() {
		opts.BuiltAt = time.Unix(0, 0)
	}
	if opts.Source == "" {
		opts.Source = "direct"
	}
	if opts.SpoolDir == "" {
		opts.SpoolDir = filepath.Join(filepath.Dir(opts.OutputPath), ".direct-v9-spool")
	}
	if opts.RunRecords <= 0 {
		opts.RunRecords = directV9DefaultRunRecords
	}
	if opts.RunBytes <= 0 {
		opts.RunBytes = directV9DefaultRunBytes
	}
	if opts.RankWorkers <= 0 {
		opts.RankWorkers = 1
	}
	if err := os.MkdirAll(opts.SpoolDir, 0o700); err != nil {
		return stats, err
	}
	owned := make([]string, 0, 32)
	defer func() {
		directV9RemoveOwned(owned)
		stats.Duration = time.Since(start)
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		stats.RuntimeHeap = mem.HeapAlloc
	}()

	if opts.MaxInaccessible < 0 {
		return stats, errors.New("direct v9 max inaccessible must be non-negative")
	}
	t0 := time.Now()
	runs, maxRunBytes, err := directV9BuildRuns(ctx, opts.Records, opts.SpoolDir, opts.RunRecords, opts.RunBytes, &owned)
	if err != nil {
		return stats, err
	}
	report("runs", time.Since(t0))
	stats.Runs = len(runs)
	stats.MaxRunBytes = maxRunBytes
	var runBytes int64
	for _, run := range runs {
		runBytes += run.bytes
	}
	if opts.WalkReport != nil && !opts.WalkReport.SourceComplete {
		if opts.WalkReport.Inaccessible > 0 && opts.WalkReport.Inaccessible <= opts.MaxInaccessible {
			stats.SourceDegraded = true
		} else {
			return stats, fmt.Errorf("direct v9 source incomplete: skipped=%d inaccessible=%d reparse=%d max-inaccessible=%d examples=%v", opts.WalkReport.Skipped, opts.WalkReport.Inaccessible, opts.WalkReport.ReparseSkipped, opts.MaxInaccessible, opts.WalkReport.SkipExamples)
		}
	}
	t0 = time.Now()
	finalPath := filepath.Join(opts.SpoolDir, "direct-v9-records.final.tmp")
	frnPath := filepath.Join(opts.SpoolDir, "direct-v9-frns.tmp")
	recordCount, spoolBytes, err := directV9MergeRuns(ctx, runs, finalPath, frnPath, &owned)
	if err != nil {
		return stats, err
	}
	report("merge-runs", time.Since(t0))
	stats.Records = recordCount
	stats.SpoolBytes = spoolBytes
	stats.FinalIDRule = "ascending-frn; duplicate-frn-rejected"
	stats.SpoolSchema = "u64 frn,parent_frn; u32 mode; i64 size,mod_unix; u32 name_bytes,path_bytes; utf8 name,path"
	tokenPath := filepath.Join(opts.SpoolDir, "direct-v9-name-table.tmp")
	owned = append(owned, tokenPath)
	t0 = time.Now()
	nameBlobLen, tokenCount, err := directV9ScanNames(finalPath, tokenPath)
	if err != nil {
		return stats, err
	}
	report("scan-names", time.Since(t0))
	if tokenCount != recordCount {
		return stats, errors.New("direct v9 token count mismatch")
	}
	stats.NameBlobBytes = nameBlobLen
	stats.TokenBytes = int64(tokenCount) * 6
	if directV9CompactFlags(recordCount)&compactDiskWideRefsFlag != 0 {
		stats.RecordBytes = int64(recordCount) * compactWideDiskRecordBytes
	} else {
		stats.RecordBytes = int64(recordCount) * compactDiskRecordBytes
	}
	rankSpecs := directV9RankSpecs()
	// Give the size rank the recursive directory totals so a directory is
	// ordered by its subtree size, matching the persisted SUBS column.
	if dirBytes, err := directV9ComputeDirBytes(ctx, finalPath, frnPath, recordCount); err != nil {
		return stats, err
	} else if len(dirBytes) == recordCount {
		for i := range rankSpecs {
			if rankSpecs[i].Tag != indexSectionSRNK {
				continue
			}
			bytes := dirBytes
			rankSpecs[i].KeyWithID = func(id uint32, rec directV9Record) string {
				if rec.Mode&uint32(os.ModeDir) != 0 && int(id) < len(bytes) {
					return directV9SignedOrderKey(int64(bytes[id])) + "\x00" + strings.ToLower(rec.Name)
				}
				return directV9SignedOrderKey(rec.Size) + "\x00" + strings.ToLower(rec.Name)
			}
		}
	}
	stats.RankRecords = recordCount
	stats.RankBytes = int64(len(rankSpecs)) * (8 + int64(recordCount)*8)
	finalSpoolBytes := spoolBytes
	frnInfo, _ := os.Stat(frnPath)
	finalInfo, _ := os.Stat(finalPath)
	frnBytes := int64(0)
	if frnInfo != nil {
		frnBytes = frnInfo.Size()
	}
	if finalInfo != nil {
		finalSpoolBytes = finalInfo.Size()
	}
	// Each rank family is built, emitted, and released before the next one.
	// The initial merge peak and the one-family output peak are tracked
	// separately; no full rank vector is retained in Go memory.
	stats.ScratchBytes = runBytes + finalSpoolBytes + frnBytes
	baseRankScratch := finalSpoolBytes + frnBytes + stats.TokenBytes
	if baseRankScratch > stats.ScratchBytes {
		stats.ScratchBytes = baseRankScratch
	}
	stats.RankFamilies = nil
	stats.SectionReports = nil
	t0 = time.Now()
	outputBytes, err := directV9WriteAtomic(ctx, opts, finalPath, frnPath, recordCount, nameBlobLen, tokenPath, rankSpecs, baseRankScratch, &stats.RankFamilies, &stats.SectionReports, &stats.ScratchBytes, &owned)
	if err != nil {
		return stats, err
	}
	report("write-atomic", time.Since(t0))
	stats.OutputBytes = outputBytes
	stats.Sections = make([]string, 0, len(stats.SectionReports))
	for _, section := range stats.SectionReports {
		stats.Sections = append(stats.Sections, section.Name)
	}
	if opts.WalkReport != nil {
		stats.SourceComplete = opts.WalkReport.SourceComplete
		stats.SourceSkipped = opts.WalkReport.Skipped
		stats.SourceInaccessible = opts.WalkReport.Inaccessible
		stats.SourceExcluded = opts.WalkReport.Excluded
		stats.SourceReparseSkipped = opts.WalkReport.ReparseSkipped
		stats.SourceReparseNotFollowed = opts.WalkReport.ReparseNotFollowed
		stats.SourceExclusions = append([]string(nil), opts.WalkReport.Exclusions...)
		stats.SourceSkipExamples = append([]string(nil), opts.WalkReport.SkipExamples...)
		stats.SourceMaxInaccessible = opts.MaxInaccessible
		if stats.SourceDegraded && stats.SourceComplete {
			stats.SourceComplete = false
		}
	}
	return stats, nil
}

type directV9WalkPreflight struct {
	SourceRoot      string   `json:"source_root"`
	Target          string   `json:"target"`
	Spool           string   `json:"spool"`
	ExclusionRoots  []string `json:"effective_exclusion_roots"`
	ExclusionSuffix []string `json:"effective_exclusion_suffixes"`
}

var directV9ArtifactSuffixes = []string{
	".gsi", ".gsi.tok", ".tok", ".seekfs-dogfood.jsonl", ".seekfs-agent-findings.jsonl",
}

func directV9CanonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if canonical, canonicalErr := filepath.EvalSymlinks(abs); canonicalErr == nil {
		return filepath.Clean(canonical), nil
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

func directV9WalkPreflightFor(root, output, spool string) (directV9WalkPreflight, error) {
	var preflight directV9WalkPreflight
	rootAbs, err := directV9CanonicalPath(root)
	if err != nil {
		return preflight, err
	}
	outAbs, err := directV9CanonicalPath(output)
	if err != nil {
		return preflight, err
	}
	if spool == "" {
		spool = filepath.Join(filepath.Dir(outAbs), ".direct-v9-spool")
	}
	spoolAbs, err := directV9CanonicalPath(spool)
	if err != nil {
		return preflight, err
	}
	runRoot := filepath.Dir(outAbs)
	exclusions := []string{
		runRoot,
		filepath.Join(rootAbs, ".r5tmp"),
		filepath.Join(rootAbs, ".seekfs"),
		filepath.Join(rootAbs, ".seekfs-db"),
		filepath.Join(rootAbs, ".seekfs-data"),
		filepath.Join(rootAbs, ".seekfs-cache"),
		filepath.Join(rootAbs, "$Recycle.Bin"),
		filepath.Join(rootAbs, "System Volume Information"),
	}
	exclusions = append(exclusions, seekFSExclusionDirsUnder(rootAbs)...)
	for parent := runRoot; parent != filepath.Dir(parent); parent = filepath.Dir(parent) {
		if strings.EqualFold(filepath.Base(parent), ".r5tmp") {
			exclusions = append(exclusions, parent)
		}
	}
	canonicalExclusions := make([]string, 0, len(exclusions))
	seen := make(map[string]struct{}, len(exclusions))
	for _, exclusion := range exclusions {
		canonical, canonicalErr := directV9CanonicalPath(exclusion)
		if canonicalErr != nil {
			return preflight, canonicalErr
		}
		if strings.EqualFold(canonical, rootAbs) || !directV9PathUnderAny(canonical, []string{rootAbs}) {
			return preflight, fmt.Errorf("direct v9 exclusion escapes or aliases source root: %s", canonical)
		}
		key := strings.ToLower(canonical)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		canonicalExclusions = append(canonicalExclusions, canonical)
	}
	if !directV9PathUnderAny(outAbs, canonicalExclusions) || !directV9PathUnderAny(spoolAbs, canonicalExclusions) {
		return preflight, errors.New("direct v9 target/spool is not covered by the effective exclusions")
	}
	preflight = directV9WalkPreflight{
		SourceRoot:      rootAbs,
		Target:          outAbs,
		Spool:           spoolAbs,
		ExclusionRoots:  canonicalExclusions,
		ExclusionSuffix: append([]string(nil), directV9ArtifactSuffixes...),
	}
	return preflight, nil
}

func directV9WriteWalkPreflight(runRoot string, preflight directV9WalkPreflight) error {
	data, err := json.MarshalIndent(preflight, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(runRoot, "direct-v9-walk-preflight.json"), append(data, '\n'), 0o600); err != nil {
		return err
	}
	var log strings.Builder
	log.WriteString("preflight_complete=true\nsource_root=" + preflight.SourceRoot + "\nexclusion_roots:\n")
	for _, root := range preflight.ExclusionRoots {
		log.WriteString("  " + root + "\n")
	}
	log.WriteString("exclusion_suffixes:\n")
	for _, suffix := range preflight.ExclusionSuffix {
		log.WriteString("  " + suffix + "\n")
	}
	return os.WriteFile(filepath.Join(runRoot, "direct-v9-startup.log"), []byte(log.String()), 0o600)
}

// directV9VolumeExclusions returns the paths on vol that a raw-MFT build must
// drop: the seekfs dir plus the standard system folders the walk preflight also
// excludes. Paths are left as canonical absolute paths; filterMFTExclusions
// resolves them to FRN subtrees and skips any not present on the volume.
func directV9VolumeExclusions(vol string) []string {
	volRoot := strings.ToUpper(strings.TrimRight(vol, `\`)) + `\`
	exclusions := seekFSExclusionDirsUnder(volRoot)
	exclusions = append(exclusions,
		filepath.Join(volRoot, "$Recycle.Bin"),
		filepath.Join(volRoot, "System Volume Information"),
	)
	return exclusions
}

// cmdDirectV9 exposes only the bounded prototype sources.  It deliberately
// has no service, elevation, compactor, or existing-index input path.
func cmdDirectV9(args []string) error {
	fs := flag.NewFlagSet("direct-v9", flag.ContinueOnError)
	out := fs.String("out", "", "new v9 output path")
	root := fs.String("root", "", "read-only filesystem root for the walk fallback")
	volume := fs.String("volume", "", "NTFS volume to build from the raw MFT/USN journal (elevated, Everything-style fast build)")
	records := fs.Int("records", 0, "deterministic synthetic record count")
	spool := fs.String("spool-dir", "", "owned scratch directory")
	runRecords := fs.Int("run-records", directV9DefaultRunRecords, "records per external-sort run")
	runBytes := fs.Int64("run-bytes", directV9DefaultRunBytes, "bytes per external-sort run")
	rankWorkers := fs.Int("rank-workers", 1, "bounded parallel rank-run sort workers (1-16)")
	walkWorkers := fs.Int("walk-workers", directV9ConcurrentDefaultWorkers, "bounded filesystem metadata workers (1-16)")
	walkQueue := fs.Int("walk-queue", 0, "bounded filesystem walk queue (default workers*2)")
	maxInaccessible := fs.Int("max-inaccessible", directV9DefaultMaxInaccessible, "maximum inaccessible paths to skip before refusing to publish")
	timeout := fs.Duration("timeout", 30*time.Minute, "prototype timeout")
	jsonOut := fs.Bool("json", false, "write JSON stats")
	dryRun := fs.Bool("dry-run", false, "validate and write the filesystem-walk preflight without traversing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("direct-v9 requires -out")
	}
	provided := 0
	if *root != "" {
		provided++
	}
	if *volume != "" {
		provided++
	}
	if *records != 0 {
		provided++
	}
	if provided != 1 {
		return errors.New("direct-v9 requires exactly one of -root, -volume, or -records")
	}
	if *dryRun {
		if *root == "" {
			return errors.New("direct-v9 -dry-run requires -root")
		}
		preflight, err := directV9WalkPreflightFor(*root, *out, *spool)
		if err != nil {
			return err
		}
		if err := directV9WriteWalkPreflight(filepath.Dir(preflight.Target), preflight); err != nil {
			return err
		}
		if *jsonOut {
			return json.NewEncoder(os.Stdout).Encode(preflight)
		}
		fmt.Printf("direct v9 walk preflight source=%s target=%s spool=%s exclusions=%d\n", preflight.SourceRoot, preflight.Target, preflight.Spool, len(preflight.ExclusionRoots))
		return nil
	}
	var source directV9RecordSource
	var roots []string
	var sourceName, volName string
	var closeSource func()
	var walkReport *directV9WalkReport
	var journalID uint64
	var checkpoint int64
	builtAt := time.Unix(0, 0)
	if *volume != "" {
		src, vols, jid, cp, err := directV9VolumeSource(*volume, directV9VolumeExclusions(normalizeVolume(*volume)))
		if err != nil {
			return err
		}
		source = src
		roots = vols
		sourceName = "usn"
		volName = normalizeVolume(*volume)
		journalID = jid
		checkpoint = cp
		builtAt = time.Now()
	} else if *root != "" {
		preflight, err := directV9WalkPreflightFor(*root, *out, *spool)
		if err != nil {
			return err
		}
		if err := directV9WriteWalkPreflight(filepath.Dir(preflight.Target), preflight); err != nil {
			return err
		}
		rootAbs := preflight.SourceRoot
		walkReport = &directV9WalkReport{}
		walk, err := newDirectV9ConcurrentWalkSourceWithExclusions(rootAbs, preflight.ExclusionRoots, preflight.ExclusionSuffix, walkReport, *walkWorkers, *walkQueue)
		if err != nil {
			return err
		}
		source = walk
		roots = []string{rootAbs}
		sourceName = "direct-walk"
		volName = filepath.VolumeName(rootAbs)
		closeSource = walk.(interface{ Close() }).Close
	} else {
		if *records < 0 {
			return errors.New("direct-v9 -records must be non-negative")
		}
		source = &directV9SyntheticSource{remaining: *records}
		roots = []string{"synthetic:\\"}
		sourceName = "direct-synthetic"
	}
	if closeSource != nil {
		defer closeSource()
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var phaseMillis []string
	var phaseMu sync.Mutex
	stats, err := buildDirectV9(ctx, directV9BuildOptions{
		OutputPath:      *out,
		SpoolDir:        *spool,
		Roots:           roots,
		Volume:          volName,
		Source:          sourceName,
		BuiltAt:         builtAt,
		JournalID:       journalID,
		Checkpoint:      checkpoint,
		Records:         source,
		RunRecords:      *runRecords,
		RunBytes:        *runBytes,
		RankWorkers:     *rankWorkers,
		WalkWorkers:     *walkWorkers,
		WalkQueue:       *walkQueue,
		WalkReport:      walkReport,
		MaxInaccessible: *maxInaccessible,
		PhaseReporter: func(name string, d time.Duration) {
			phaseMu.Lock()
			phaseMillis = append(phaseMillis, fmt.Sprintf("%s=%dms", name, d.Milliseconds()))
			phaseMu.Unlock()
		},
	})
	if err != nil {
		return err
	}
	stats.PhaseMillis = phaseMillis
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(os.Stdout).Encode(struct {
			OK bool `json:"ok"`
			directV9BuildStats
		}{OK: true, directV9BuildStats: stats})
	}
	fmt.Printf("direct v9 records=%d runs=%d output=%d scratch=%d duration=%s\n", stats.Records, stats.Runs, stats.OutputBytes, stats.ScratchBytes, stats.Duration.Round(time.Millisecond))
	return nil
}
