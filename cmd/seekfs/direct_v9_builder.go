package main

// The direct builder is intentionally independent from Index and
// buildOrders.  It is the bounded ingestion seam for the direct filesystem
// to v9 path: sources write records into owned external-sort runs, final IDs
// are assigned once by FRN, and the existing v9 reader contract is emitted
// directly from the final spool.

import (
	"bufio"
	"container/heap"
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
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	directV9DefaultRunRecords = 64 * 1024
	directV9DefaultRunBytes   = 64 * 1024 * 1024
	directV9SpoolHeaderBytes  = 8 + 8 + 4 + 8 + 8 + 4 + 4

	// directV9DefaultMaxInaccessible allows a handful of transient permission
	// or race failures during a large filesystem walk without discarding hours
	// of work.  The build reports SourceDegraded when the bound is hit so the
	// operator knows the published index skipped a few paths.
	directV9DefaultMaxInaccessible = 64
)

var errDirectV9DuplicateFRN = errors.New("direct v9 source contains duplicate FRN")

// directV9Record is the only source-to-spool representation.  It is fixed
// width apart from Name, so a source can be read incrementally and no Index
// or v8 object is needed between ingestion and v9 output.
type directV9Record struct {
	FRN       uint64
	ParentFRN uint64
	Mode      uint32
	Size      int64
	ModUnix   int64
	Name      string
	// Path is the canonical source path when the source can provide it.  A
	// missing path is deliberate for the elevation-free MFT/USN adapters; the
	// path rank then uses the folded name as its conservative fallback.
	Path string
}

type directV9RecordSource interface {
	Next(context.Context) (directV9Record, error)
}

type directV9BuildOptions struct {
	OutputPath string
	SpoolDir   string
	Roots      []string
	Volume     string
	Source     string
	BuiltAt    time.Time
	JournalID  uint64
	Checkpoint int64
	Records    directV9RecordSource

	// RunRecords and RunBytes are hard bounds for each external-sort run.
	// The builder never allocates a source-sized slice.
	RunRecords  int
	RunBytes    int64
	RankWorkers int
	WalkWorkers int
	WalkQueue   int

	// WalkReport is populated by the read-only filesystem walk.  It is kept
	// outside the record spool so skipped/inaccessible paths remain bounded.
	WalkReport *directV9WalkReport

	// MaxInaccessible bounds how many inaccessible paths may be skipped before
	// the source-completeness gate refuses to publish an index.  A transient
	// permission/race must not turn a multi-hour build into a wasted run, but a
	// grossly incomplete walk must not be silently persisted.
	MaxInaccessible int
}

type directV9BuildStats struct {
	Records                  int                     `json:"records"`
	Runs                     int                     `json:"runs"`
	SpoolBytes               int64                   `json:"spool_bytes"`
	ScratchBytes             int64                   `json:"scratch_bytes"`
	MaxRunBytes              int64                   `json:"max_run_bytes"`
	NameBlobBytes            int64                   `json:"name_blob_bytes"`
	TokenBytes               int64                   `json:"token_table_bytes"`
	RecordBytes              int64                   `json:"record_table_bytes"`
	RankBytes                int64                   `json:"rank_section_bytes"`
	OutputBytes              int64                   `json:"output_bytes"`
	RuntimeHeap              uint64                  `json:"runtime_heap_bytes"`
	Duration                 time.Duration           `json:"duration"`
	RankRecords              int                     `json:"rank_records"`
	ParentMisses             int                     `json:"parent_misses"`
	Sections                 []string                `json:"sections"`
	RankFamilies             []directV9RankReport    `json:"rank_families,omitempty"`
	SectionReports           []directV9SectionReport `json:"section_reports,omitempty"`
	FinalIDRule              string                  `json:"final_id_rule"`
	SpoolSchema              string                  `json:"spool_schema"`
	SourceComplete           bool                    `json:"source_complete,omitempty"`
	SourceSkipped            int                     `json:"source_skipped,omitempty"`
	SourceInaccessible       int                     `json:"source_inaccessible,omitempty"`
	SourceExcluded           int                     `json:"source_excluded,omitempty"`
	SourceReparseSkipped     int                     `json:"source_reparse_skipped,omitempty"`
	SourceReparseNotFollowed int                     `json:"source_reparse_not_followed,omitempty"`
	SourceExclusions         []string                `json:"source_exclusions,omitempty"`
	SourceSkipExamples       []string                `json:"source_skip_examples,omitempty"`
	SourceDegraded           bool                    `json:"source_degraded,omitempty"`
	SourceMaxInaccessible    int                     `json:"source_max_inaccessible,omitempty"`
}

type directV9RunFile struct {
	path  string
	bytes int64
}

type directV9RankItem struct {
	Key string
	ID  uint32
}

type directV9RankSpec struct {
	Tag  uint32
	Name string
	Key  func(directV9Record) string
}

type directV9RankReport struct {
	Name        string `json:"name"`
	Tag         uint32 `json:"tag"`
	Runs        int    `json:"runs"`
	Bytes       int64  `json:"section_bytes"`
	RunBytes    int64  `json:"run_bytes"`
	MaxRunBytes int64  `json:"max_run_bytes"`
}

type directV9SectionReport struct {
	Name         string `json:"name"`
	Tag          uint32 `json:"tag"`
	Runs         int    `json:"runs"`
	Bytes        int64  `json:"section_bytes"`
	ScratchBytes int64  `json:"scratch_bytes"`
}

type directV9RankHeap struct {
	items []itemWithRun
}

func (h directV9RankHeap) Len() int           { return len(h.items) }
func (h directV9RankHeap) Less(i, j int) bool { return h.items[i].less(h.items[j]) }
func (h directV9RankHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *directV9RankHeap) Push(x any)        { h.items = append(h.items, x.(itemWithRun)) }
func (h *directV9RankHeap) Pop() any {
	old := h.items
	n := len(old)
	x := old[n-1]
	h.items = old[:n-1]
	return x
}

type directV9RunHead struct {
	rec directV9Record
	run int
}

type directV9RunHeap struct {
	items []directV9RunHead
}

func (h directV9RunHeap) Len() int { return len(h.items) }
func (h directV9RunHeap) Less(i, j int) bool {
	a, b := h.items[i].rec, h.items[j].rec
	if a.FRN != b.FRN {
		return a.FRN < b.FRN
	}
	if a.ParentFRN != b.ParentFRN {
		return a.ParentFRN < b.ParentFRN
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	if a.Mode != b.Mode {
		return a.Mode < b.Mode
	}
	if a.Size != b.Size {
		return a.Size < b.Size
	}
	return a.ModUnix < b.ModUnix
}
func (h directV9RunHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *directV9RunHeap) Push(x any)   { h.items = append(h.items, x.(directV9RunHead)) }
func (h *directV9RunHeap) Pop() any {
	old := h.items
	n := len(old)
	x := old[n-1]
	h.items = old[:n-1]
	return x
}

type directV9SliceSource struct {
	records []directV9Record
	pos     int
}

type directV9SyntheticSource struct {
	next, remaining int
}

type directV9MFTSource struct {
	entries map[uint64]mftEntry
	frns    []uint64
	pos     int
}

func newDirectV9MFTSource(entries map[uint64]mftEntry) directV9RecordSource {
	frns := make([]uint64, 0, len(entries))
	for frn := range entries {
		frns = append(frns, frn)
	}
	sort.Slice(frns, func(i, j int) bool { return frns[i] < frns[j] })
	return &directV9MFTSource{entries: entries, frns: frns}
}

func (s *directV9MFTSource) Next(ctx context.Context) (directV9Record, error) {
	select {
	case <-ctx.Done():
		return directV9Record{}, ctx.Err()
	default:
	}
	if s.pos >= len(s.frns) {
		return directV9Record{}, io.EOF
	}
	e := s.entries[s.frns[s.pos]]
	s.pos++
	return directV9Record{
		FRN:       e.frn,
		ParentFRN: e.parentFRN,
		Mode:      modeFromAttrs(e.attr),
		Size:      e.size,
		ModUnix:   e.modUnix,
		Name:      e.name,
	}, nil
}

type directV9USNSource struct {
	nodes map[uint64]usnNode
	frns  []uint64
	pos   int
}

func newDirectV9USNSource(nodes map[uint64]usnNode) directV9RecordSource {
	frns := make([]uint64, 0, len(nodes))
	for frn := range nodes {
		frns = append(frns, frn)
	}
	sort.Slice(frns, func(i, j int) bool { return frns[i] < frns[j] })
	return &directV9USNSource{nodes: nodes, frns: frns}
}

func (s *directV9USNSource) Next(ctx context.Context) (directV9Record, error) {
	select {
	case <-ctx.Done():
		return directV9Record{}, ctx.Err()
	default:
	}
	if s.pos >= len(s.frns) {
		return directV9Record{}, io.EOF
	}
	n := s.nodes[s.frns[s.pos]]
	s.pos++
	return directV9Record{FRN: n.frn, ParentFRN: n.parentFRN, Mode: modeFromAttrs(n.attr), Name: n.name}, nil
}

func (s *directV9SyntheticSource) Next(ctx context.Context) (directV9Record, error) {
	select {
	case <-ctx.Done():
		return directV9Record{}, ctx.Err()
	default:
	}
	if s.remaining == 0 {
		return directV9Record{}, io.EOF
	}
	id := s.next
	s.next++
	s.remaining--
	return directV9Record{
		FRN:       uint64(id + 1),
		ParentFRN: uint64(id),
		Size:      int64(id % 100000),
		ModUnix:   int64(1_700_000_000_000_000_000 + id),
		Name:      fmt.Sprintf("prototype-%05d.txt", id%100000),
		Path:      fmt.Sprintf(`synthetic:\prototype-%05d.txt`, id%100000),
	}, nil
}

func (s *directV9SliceSource) Next(ctx context.Context) (directV9Record, error) {
	select {
	case <-ctx.Done():
		return directV9Record{}, ctx.Err()
	default:
	}
	if s.pos >= len(s.records) {
		return directV9Record{}, io.EOF
	}
	rec := s.records[s.pos]
	s.pos++
	return rec, nil
}

// newDirectV9SliceSource is used by deterministic fixtures and by the
// source-order invariance tests.  Production sources implement the same
// small interface and may stream from USN/MFT or a walk.
func newDirectV9SliceSource(records []directV9Record) directV9RecordSource {
	return &directV9SliceSource{records: records}
}

type directV9WalkSource struct {
	records chan directV9Record
	errs    chan error
	done    chan struct{}
	once    sync.Once
	report  *directV9WalkReport
}

func newDirectV9WalkSource(root string) (directV9RecordSource, error) {
	return newDirectV9WalkSourceWithExclusions(root, nil, nil, nil)
}

// directV9WalkReport is intentionally counter-based.  A full list of skipped
// paths can be larger than the builder's memory budget, so only a small sample
// is retained for diagnostics while the complete counts are emitted in the
// build report.
type directV9WalkReport struct {
	Root               string
	Exclusions         []string
	Skipped            int
	Inaccessible       int
	Excluded           int
	ReparseSkipped     int
	ReparseNotFollowed int
	SkipExamples       []string
	SourceComplete     bool
}

func (r *directV9WalkReport) note(kind string, path string) {
	if r == nil {
		return
	}
	switch kind {
	case "skipped":
		r.Skipped++
	case "inaccessible":
		r.Inaccessible++
	case "excluded":
		r.Excluded++
	case "reparse":
		r.ReparseSkipped++
	case "reparse-not-followed":
		r.ReparseNotFollowed++
	}
	if len(r.SkipExamples) < 64 && path != "" {
		r.SkipExamples = append(r.SkipExamples, kind+":"+path)
	}
}

func newDirectV9WalkSourceWithExclusions(root string, exclusionRoots, exclusionSuffixes []string, report *directV9WalkReport) (directV9RecordSource, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	abs = filepath.Clean(abs)
	if directV9PathIsReparse(abs) {
		return nil, fmt.Errorf("direct v9 walk root is a reparse point: %s", abs)
	}
	canonicalExclusions := make([]string, 0, len(exclusionRoots))
	for _, exclusion := range exclusionRoots {
		if exclusion == "" {
			continue
		}
		canonical, canonicalErr := filepath.Abs(exclusion)
		if canonicalErr != nil {
			return nil, canonicalErr
		}
		canonical, canonicalErr = filepath.EvalSymlinks(canonical)
		if canonicalErr != nil {
			// A not-yet-created builder directory is still a valid exclusion;
			// canonicalize its existing parent and append the requested leaf.
			parent := filepath.Dir(canonical)
			parent, canonicalErr = filepath.EvalSymlinks(parent)
			if canonicalErr != nil {
				return nil, canonicalErr
			}
			canonical = filepath.Join(parent, filepath.Base(canonical))
		}
		canonicalExclusions = append(canonicalExclusions, filepath.Clean(canonical))
	}
	if report != nil {
		report.Root = abs
		report.Exclusions = append([]string(nil), canonicalExclusions...)
		report.SourceComplete = true
	}
	s := &directV9WalkSource{
		records: make(chan directV9Record, 256),
		errs:    make(chan error, 1),
		done:    make(chan struct{}),
		report:  report,
	}
	go func() {
		walkErr := filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if report != nil {
					report.SourceComplete = false
					report.note("inaccessible", path)
				}
				return nil
			}
			if directV9PathUnderAny(path, canonicalExclusions) {
				if report != nil {
					report.note("excluded", path)
				}
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			select {
			case <-s.done:
				return context.Canceled
			default:
			}
			if directV9PathIsReparse(path) {
				if report != nil {
					report.SourceComplete = false
					report.note("reparse", path)
				}
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			lowerPath := strings.ToLower(path)
			for _, suffix := range exclusionSuffixes {
				if strings.HasSuffix(lowerPath, strings.ToLower(suffix)) {
					if report != nil {
						report.note("excluded", path)
					}
					return nil
				}
			}
			info, infoErr := d.Info()
			if infoErr != nil {
				if report != nil {
					report.SourceComplete = false
					report.note("inaccessible", path)
				}
				return nil
			}
			frn := directV9StablePathID(path)
			parent := uint64(0)
			clean := filepath.Clean(path)
			parentPath := filepath.Dir(clean)
			if filepath.Clean(abs) != clean {
				parent = directV9StablePathID(parentPath)
			}
			rec := directV9Record{
				FRN:       frn,
				ParentFRN: parent,
				Mode:      uint32(info.Mode()),
				Size:      info.Size(),
				ModUnix:   directV9WalkModUnix(info),
				Name:      d.Name(),
				Path:      clean,
			}
			select {
			case s.records <- rec:
				return nil
			case <-s.done:
				return context.Canceled
			}
		})
		close(s.records)
		s.errs <- walkErr
		close(s.errs)
	}()
	return s, nil
}

func directV9PathUnderAny(path string, roots []string) bool {
	path = filepath.Clean(path)
	for _, root := range roots {
		root = filepath.Clean(root)
		rel, err := filepath.Rel(root, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func directV9PathIsReparse(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && data.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func (s *directV9WalkSource) Next(ctx context.Context) (directV9Record, error) {
	select {
	case <-ctx.Done():
		return directV9Record{}, ctx.Err()
	case rec, ok := <-s.records:
		if ok {
			return rec, nil
		}
		if err := <-s.errs; err != nil && !errors.Is(err, context.Canceled) {
			return directV9Record{}, err
		}
		return directV9Record{}, io.EOF
	}
}

func (s *directV9WalkSource) Close() { s.once.Do(func() { close(s.done) }) }

func directV9StablePathID(path string) uint64 {
	// FNV-1a is stable across processes and adequate for the walk adapter.
	// A collision is rejected by the final-ID merge rather than silently
	// producing ambiguous parent links.
	normal := strings.ToLower(filepath.Clean(path))
	var hash uint64 = 14695981039346656037
	for i := 0; i < len(normal); i++ {
		hash ^= uint64(normal[i])
		hash *= 1099511628211
	}
	if hash == 0 {
		return 1
	}
	return hash
}

func directV9WalkModUnix(info os.FileInfo) int64 {
	// Windows directory timestamps can change as directory metadata is read.
	// Keep walk output deterministic; authoritative USN/MFT sources retain
	// directory timestamps when they are available.
	if info.IsDir() {
		return 0
	}
	return info.ModTime().UnixNano()
}

func writeDirectV9SpoolRecord(w io.Writer, rec directV9Record) (int64, error) {
	if uint64(len(rec.Name)) > uint64(^uint32(0)) || uint64(len(rec.Path)) > uint64(^uint32(0)) {
		return 0, errors.New("direct v9 record name or path too large")
	}
	var header [directV9SpoolHeaderBytes]byte
	binary.LittleEndian.PutUint64(header[0:8], rec.FRN)
	binary.LittleEndian.PutUint64(header[8:16], rec.ParentFRN)
	binary.LittleEndian.PutUint32(header[16:20], rec.Mode)
	binary.LittleEndian.PutUint64(header[20:28], uint64(rec.Size))
	binary.LittleEndian.PutUint64(header[28:36], uint64(rec.ModUnix))
	binary.LittleEndian.PutUint32(header[36:40], uint32(len(rec.Name)))
	binary.LittleEndian.PutUint32(header[40:44], uint32(len(rec.Path)))
	n, err := w.Write(header[:])
	if err != nil {
		return int64(n), err
	}
	if n != len(header) {
		return int64(n), io.ErrShortWrite
	}
	m, err := io.WriteString(w, rec.Name)
	if err != nil {
		return int64(n + m), err
	}
	if m != len(rec.Name) {
		return int64(n + m), io.ErrShortWrite
	}
	p, err := io.WriteString(w, rec.Path)
	if err != nil {
		return int64(n + m + p), err
	}
	if p != len(rec.Path) {
		return int64(n + m + p), io.ErrShortWrite
	}
	return int64(n + m + p), nil
}

func readDirectV9SpoolRecord(r *bufio.Reader) (directV9Record, error) {
	var header [directV9SpoolHeaderBytes]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return directV9Record{}, err
	}
	nameLen := binary.LittleEndian.Uint32(header[36:40])
	pathLen := binary.LittleEndian.Uint32(header[40:44])
	if uint64(nameLen) > uint64(^uint(0)>>1) {
		return directV9Record{}, errors.New("direct v9 spool name too large")
	}
	name := make([]byte, int(nameLen))
	if _, err := io.ReadFull(r, name); err != nil {
		return directV9Record{}, err
	}
	if uint64(pathLen) > uint64(^uint(0)>>1) {
		return directV9Record{}, errors.New("direct v9 spool path too large")
	}
	path := make([]byte, int(pathLen))
	if _, err := io.ReadFull(r, path); err != nil {
		return directV9Record{}, err
	}
	return directV9Record{
		FRN:       binary.LittleEndian.Uint64(header[0:8]),
		ParentFRN: binary.LittleEndian.Uint64(header[8:16]),
		Mode:      binary.LittleEndian.Uint32(header[16:20]),
		Size:      int64(binary.LittleEndian.Uint64(header[20:28])),
		ModUnix:   int64(binary.LittleEndian.Uint64(header[28:36])),
		Name:      string(name),
		Path:      string(path),
	}, nil
}

func directV9RecordLess(a, b directV9Record) bool {
	if a.FRN != b.FRN {
		return a.FRN < b.FRN
	}
	if a.ParentFRN != b.ParentFRN {
		return a.ParentFRN < b.ParentFRN
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	if a.Mode != b.Mode {
		return a.Mode < b.Mode
	}
	if a.Size != b.Size {
		return a.Size < b.Size
	}
	return a.ModUnix < b.ModUnix
}

func directV9WriteRun(path string, records []directV9Record) (int64, error) {
	sort.Slice(records, func(i, j int) bool { return directV9RecordLess(records[i], records[j]) })
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	bw := bufio.NewWriterSize(f, 256*1024)
	var written int64
	for _, rec := range records {
		n, err := writeDirectV9SpoolRecord(bw, rec)
		written += n
		if err != nil {
			return written, err
		}
	}
	if err := bw.Flush(); err != nil {
		return written, err
	}
	// The run file is owned scratch: it is merged and removed within the same
	// process, never published, so no per-run filesystem flush is needed.
	return written, f.Close()
}

func directV9BuildRuns(ctx context.Context, source directV9RecordSource, spoolDir string, maxRecords int, maxBytes int64, owned *[]string) ([]directV9RunFile, int64, error) {
	if maxRecords <= 0 {
		maxRecords = directV9DefaultRunRecords
	}
	if maxBytes <= 0 {
		maxBytes = directV9DefaultRunBytes
	}
	var runs []directV9RunFile
	chunk := make([]directV9Record, 0, min(maxRecords, 4096))
	var chunkBytes int64
	var maxChunkBytes int64
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		maxChunkBytes = max(maxChunkBytes, chunkBytes)
		path := filepath.Join(spoolDir, fmt.Sprintf("direct-v9-run-%06d.tmp", len(runs)))
		bytes, err := directV9WriteRun(path, chunk)
		if err != nil {
			return err
		}
		*owned = append(*owned, path)
		runs = append(runs, directV9RunFile{path: path, bytes: bytes})
		chunk = make([]directV9Record, 0, min(maxRecords, 4096))
		chunkBytes = 0
		return nil
	}
	for {
		rec, err := source.Next(ctx)
		if err == io.EOF {
			if err := flush(); err != nil {
				return nil, 0, err
			}
			return runs, maxChunkBytes, nil
		}
		if err != nil {
			return nil, 0, err
		}
		if len(rec.Name) > int(^uint16(0)) {
			return nil, 0, errors.New("direct v9 record name exceeds v9 token limit")
		}
		chunk = append(chunk, rec)
		chunkBytes += int64(directV9SpoolHeaderBytes + len(rec.Name) + len(rec.Path))
		if len(chunk) >= maxRecords || chunkBytes >= maxBytes {
			if err := flush(); err != nil {
				return nil, 0, err
			}
		}
	}
}

func directV9MergeRuns(ctx context.Context, runs []directV9RunFile, finalPath, frnPath string, owned *[]string) (int, int64, error) {
	if len(runs) == 0 {
		f, err := os.Create(finalPath)
		if err != nil {
			return 0, 0, err
		}
		_ = f.Close()
		f, err = os.Create(frnPath)
		if err == nil {
			_ = f.Close()
		}
		*owned = append(*owned, finalPath, frnPath)
		return 0, 0, err
	}
	files := make([]*os.File, len(runs))
	readers := make([]*bufio.Reader, len(runs))
	for i, run := range runs {
		f, err := os.Open(run.path)
		if err != nil {
			for _, opened := range files {
				if opened != nil {
					_ = opened.Close()
				}
			}
			return 0, 0, err
		}
		files[i] = f
		readers[i] = bufio.NewReaderSize(f, 256*1024)
	}
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()

	final, err := os.Create(finalPath)
	if err != nil {
		return 0, 0, err
	}
	frns, err := os.Create(frnPath)
	if err != nil {
		_ = final.Close()
		return 0, 0, err
	}
	*owned = append(*owned, finalPath, frnPath)
	bw := bufio.NewWriterSize(final, 256*1024)
	frnWriter := bufio.NewWriterSize(frns, 256*1024)
	h := &directV9RunHeap{}
	heap.Init(h)
	for i, reader := range readers {
		rec, readErr := readDirectV9SpoolRecord(reader)
		if readErr == nil {
			heap.Push(h, directV9RunHead{rec: rec, run: i})
			continue
		}
		if !errors.Is(readErr, io.EOF) {
			_ = final.Close()
			_ = frns.Close()
			return 0, 0, readErr
		}
	}
	var lastFRN uint64
	var haveLast bool
	var count int
	var written int64
	for h.Len() > 0 {
		select {
		case <-ctx.Done():
			_ = final.Close()
			_ = frns.Close()
			return 0, 0, ctx.Err()
		default:
		}
		head := heap.Pop(h).(directV9RunHead)
		if haveLast && head.rec.FRN == lastFRN {
			_ = final.Close()
			_ = frns.Close()
			return 0, 0, errDirectV9DuplicateFRN
		}
		haveLast = true
		lastFRN = head.rec.FRN
		n, writeErr := writeDirectV9SpoolRecord(bw, head.rec)
		written += n
		if writeErr != nil {
			_ = final.Close()
			_ = frns.Close()
			return 0, 0, writeErr
		}
		var frnBytes [8]byte
		binary.LittleEndian.PutUint64(frnBytes[:], head.rec.FRN)
		if _, writeErr = frnWriter.Write(frnBytes[:]); writeErr != nil {
			_ = final.Close()
			_ = frns.Close()
			return 0, 0, writeErr
		}
		count++
		next, readErr := readDirectV9SpoolRecord(readers[head.run])
		if readErr == nil {
			heap.Push(h, directV9RunHead{rec: next, run: head.run})
		} else if !errors.Is(readErr, io.EOF) {
			_ = final.Close()
			_ = frns.Close()
			return 0, 0, readErr
		}
	}
	if err := bw.Flush(); err != nil {
		_ = final.Close()
		_ = frns.Close()
		return 0, 0, err
	}
	if err := frnWriter.Flush(); err != nil {
		_ = final.Close()
		_ = frns.Close()
		return 0, 0, err
	}
	if err := final.Close(); err != nil {
		_ = frns.Close()
		return 0, 0, err
	}
	if err := frns.Close(); err != nil {
		return 0, 0, err
	}
	return count, written, nil
}

func directV9LookupIDMapped(frns []uint64, frn uint64) int32 {
	if frn == 0 || len(frns) == 0 {
		return -1
	}
	lo, hi := 0, len(frns)
	for lo < hi {
		mid := lo + (hi-lo)/2
		if frns[mid] < frn {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo >= len(frns) || frns[lo] != frn {
		return -1
	}
	return int32(lo)
}

// directV9MapFRNs maps the merged FRN file for in-memory parent lookups.  The
// caller must close the returned mapped file.  A nil mapping is returned when
// the file is empty (no records).
func directV9MapFRNs(path string, count int) (*mappedIndexFile, []uint64, error) {
	if count == 0 {
		return nil, nil, nil
	}
	m, err := mapIndexFile(path)
	if err != nil {
		return nil, nil, err
	}
	frns := mappedUint64Slice(m.data)
	if len(frns) < count {
		_ = m.close()
		return nil, nil, fmt.Errorf("direct v9 FRN map bytes=%d want=%d", len(frns), count)
	}
	frns = frns[:count]
	return m, frns, nil
}

type directV9ChildPair struct {
	Parent uint32
	Child  uint32
}

func directV9WriteChildRun(path string, pairs []directV9ChildPair) (int64, error) {
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Parent != pairs[j].Parent {
			return pairs[i].Parent < pairs[j].Parent
		}
		return pairs[i].Child < pairs[j].Child
	})
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	bw := bufio.NewWriterSize(f, 256*1024)
	var buf [8]byte
	var written int64
	for _, pair := range pairs {
		binary.LittleEndian.PutUint32(buf[0:4], pair.Parent)
		binary.LittleEndian.PutUint32(buf[4:8], pair.Child)
		if _, err := bw.Write(buf[:]); err != nil {
			_ = f.Close()
			return written, err
		}
		written += int64(len(buf))
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		return written, err
	}
	// Owned scratch; merged and removed within the same process.
	return written, f.Close()
}

type directV9ChildHead struct {
	Pair directV9ChildPair
	Run  int
}

type directV9ChildHeap struct{ items []directV9ChildHead }

func (h directV9ChildHeap) Len() int { return len(h.items) }
func (h directV9ChildHeap) Less(i, j int) bool {
	a, b := h.items[i].Pair, h.items[j].Pair
	if a.Parent != b.Parent {
		return a.Parent < b.Parent
	}
	return a.Child < b.Child
}
func (h directV9ChildHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *directV9ChildHeap) Push(x any)   { h.items = append(h.items, x.(directV9ChildHead)) }
func (h *directV9ChildHeap) Pop() any {
	old := h.items
	n := len(old)
	x := old[n-1]
	h.items = old[:n-1]
	return x
}

func directV9ReadChildPair(r *bufio.Reader) (directV9ChildPair, error) {
	var buf [8]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return directV9ChildPair{}, err
	}
	return directV9ChildPair{Parent: binary.LittleEndian.Uint32(buf[0:4]), Child: binary.LittleEndian.Uint32(buf[4:8])}, nil
}

func directV9CheckParentCycles(ctx context.Context, parentPath string, recordCount int) error {
	f, err := os.Open(parentPath)
	if err != nil {
		return err
	}
	defer f.Close()
	state := make([]byte, recordCount)
	stamp := make([]uint32, recordCount)
	var walkID uint32
	readParent := func(id int) (int32, error) {
		var b [4]byte
		if _, err := f.ReadAt(b[:], int64(id)*4); err != nil {
			return -1, err
		}
		value := binary.LittleEndian.Uint32(b[:])
		if value == ^uint32(0) {
			return -1, nil
		}
		if value >= uint32(recordCount) {
			return -1, errors.New("direct v9 topology parent ID out of range")
		}
		return int32(value), nil
	}
	for start := 0; start < recordCount; start++ {
		if state[start] != 0 {
			continue
		}
		walkID++
		if walkID == 0 {
			for i := range stamp {
				stamp[i] = 0
			}
			walkID = 1
		}
		cur := start
		for cur >= 0 && state[cur] == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			if stamp[cur] == walkID {
				return errors.New("direct v9 topology parent cycle")
			}
			stamp[cur] = walkID
			parent, err := readParent(cur)
			if err != nil {
				return err
			}
			cur = int(parent)
		}
		cur = start
		for cur >= 0 && state[cur] == 0 {
			state[cur] = 1
			parent, err := readParent(cur)
			if err != nil {
				return err
			}
			cur = int(parent)
		}
	}
	return nil
}

func directV9CopyFile(cw *countingWriter, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(cw, f)
	return err
}

func directV9MergeChildRuns(ctx context.Context, cw *countingWriter, runs []directV9RunFile, expected int) error {
	files := make([]*os.File, len(runs))
	readers := make([]*bufio.Reader, len(runs))
	h := &directV9ChildHeap{}
	heap.Init(h)
	for i, run := range runs {
		f, err := os.Open(run.path)
		if err != nil {
			for _, opened := range files {
				if opened != nil {
					_ = opened.Close()
				}
			}
			return err
		}
		files[i] = f
		readers[i] = bufio.NewReaderSize(f, 256*1024)
		pair, readErr := directV9ReadChildPair(readers[i])
		if readErr == nil {
			heap.Push(h, directV9ChildHead{Pair: pair, Run: i})
		} else if !errors.Is(readErr, io.EOF) {
			for _, opened := range files {
				if opened != nil {
					_ = opened.Close()
				}
			}
			return readErr
		}
	}
	defer func() {
		for _, f := range files {
			if f != nil {
				_ = f.Close()
			}
		}
	}()
	var written int
	var buf [4]byte
	for h.Len() > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		head := heap.Pop(h).(directV9ChildHead)
		binary.LittleEndian.PutUint32(buf[:], head.Pair.Child)
		if _, err := cw.Write(buf[:]); err != nil {
			return err
		}
		written++
		next, readErr := directV9ReadChildPair(readers[head.Run])
		if readErr == nil {
			heap.Push(h, directV9ChildHead{Pair: next, Run: head.Run})
		} else if !errors.Is(readErr, io.EOF) {
			return readErr
		}
	}
	if written != expected {
		return fmt.Errorf("direct v9 topology child count mismatch: wrote %d want %d", written, expected)
	}
	return nil
}

func directV9WriteTopologySections(ctx context.Context, cw *countingWriter, finalPath, frnPath, spoolDir string, recordCount, maxRecords int, owned *[]string, scratchHigh *int64) ([]indexSectionTableEntry, []directV9SectionReport, error) {
	parentPath := filepath.Join(spoolDir, "direct-v9-parents.tmp")
	offsetsPath := filepath.Join(spoolDir, "direct-v9-child-offsets.tmp")
	rootsPath := filepath.Join(spoolDir, "direct-v9-roots.tmp")
	*owned = append(*owned, parentPath, offsetsPath, rootsPath)
	parents, err := os.Create(parentPath)
	if err != nil {
		return nil, nil, err
	}
	roots, err := os.Create(rootsPath)
	if err != nil {
		_ = parents.Close()
		return nil, nil, err
	}
	parentWriter := bufio.NewWriterSize(parents, 256*1024)
	rootWriter := bufio.NewWriterSize(roots, 256*1024)
	counts := make([]uint32, recordCount)
	final, err := os.Open(finalPath)
	if err != nil {
		_ = parents.Close()
		_ = roots.Close()
		return nil, nil, err
	}
	frnMap, frns, err := directV9MapFRNs(frnPath, recordCount)
	if err != nil {
		_ = final.Close()
		_ = parents.Close()
		_ = roots.Close()
		return nil, nil, err
	}
	if frnMap != nil {
		defer frnMap.close()
	}
	r := bufio.NewReaderSize(final, 256*1024)
	for id := 0; id < recordCount; id++ {
		select {
		case <-ctx.Done():
			_ = final.Close()
			_ = parents.Close()
			_ = roots.Close()
			return nil, nil, ctx.Err()
		default:
		}
		rec, readErr := readDirectV9SpoolRecord(r)
		if readErr != nil {
			_ = final.Close()
			_ = parents.Close()
			_ = roots.Close()
			return nil, nil, readErr
		}
		parentID := int32(-1)
		if rec.ParentFRN != 0 {
			parentID = directV9LookupIDMapped(frns, rec.ParentFRN)
		}
		if parentID == int32(id) {
			_ = final.Close()
			_ = parents.Close()
			_ = roots.Close()
			return nil, nil, errors.New("direct v9 topology self-parent")
		}
		var b [4]byte
		if parentID < 0 {
			binary.LittleEndian.PutUint32(b[:], ^uint32(0))
			if _, err := rootWriter.Write(func() []byte { var v [4]byte; binary.LittleEndian.PutUint32(v[:], uint32(id)); return v[:] }()); err != nil {
				_ = final.Close()
				_ = parents.Close()
				_ = roots.Close()
				return nil, nil, err
			}
		} else {
			binary.LittleEndian.PutUint32(b[:], uint32(parentID))
			counts[parentID]++
		}
		if _, err := parentWriter.Write(b[:]); err != nil {
			_ = final.Close()
			_ = parents.Close()
			_ = roots.Close()
			return nil, nil, err
		}
	}
	_ = final.Close()
	if err := parentWriter.Flush(); err != nil {
		_ = parents.Close()
		_ = roots.Close()
		return nil, nil, err
	}
	if err := rootWriter.Flush(); err != nil {
		_ = parents.Close()
		_ = roots.Close()
		return nil, nil, err
	}
	_ = parents.Close()
	_ = roots.Close()
	if err := directV9CheckParentCycles(ctx, parentPath, recordCount); err != nil {
		return nil, nil, err
	}
	offsets, err := os.Create(offsetsPath)
	if err != nil {
		return nil, nil, err
	}
	var childCount uint64
	var off [4]byte
	for _, count := range counts {
		childCount += uint64(count)
	}
	var running uint32
	for i := 0; i <= recordCount; i++ {
		if i > 0 {
			running += counts[i-1]
		}
		binary.LittleEndian.PutUint32(off[:], running)
		if _, err := offsets.Write(off[:]); err != nil {
			_ = offsets.Close()
			return nil, nil, err
		}
	}
	_ = offsets.Close()
	if childCount > uint64(^uint32(0)) {
		return nil, nil, errors.New("direct v9 topology child count exceeds format")
	}
	if maxRecords <= 0 {
		maxRecords = directV9DefaultRunRecords
	}
	pfile, err := os.Open(parentPath)
	if err != nil {
		return nil, nil, err
	}
	reader := bufio.NewReaderSize(pfile, 256*1024)
	pairs := make([]directV9ChildPair, 0, min(maxRecords, 4096))
	runs := make([]directV9RunFile, 0)
	flush := func() error {
		if len(pairs) == 0 {
			return nil
		}
		path := filepath.Join(spoolDir, fmt.Sprintf("direct-v9-child-%06d.tmp", len(runs)))
		bytes, err := directV9WriteChildRun(path, pairs)
		if err != nil {
			return err
		}
		*owned = append(*owned, path)
		runs = append(runs, directV9RunFile{path: path, bytes: bytes})
		pairs = make([]directV9ChildPair, 0, min(maxRecords, 4096))
		return nil
	}
	for id := 0; id < recordCount; id++ {
		var b [4]byte
		if _, err := io.ReadFull(reader, b[:]); err != nil {
			_ = pfile.Close()
			return nil, nil, err
		}
		parent := binary.LittleEndian.Uint32(b[:])
		if parent != ^uint32(0) {
			pairs = append(pairs, directV9ChildPair{Parent: parent, Child: uint32(id)})
		}
		if len(pairs) >= maxRecords {
			if err := flush(); err != nil {
				_ = pfile.Close()
				return nil, nil, err
			}
		}
	}
	if err := flush(); err != nil {
		_ = pfile.Close()
		return nil, nil, err
	}
	_ = pfile.Close()
	var runBytes int64
	for _, run := range runs {
		runBytes += run.bytes
	}
	parentInfo, _ := os.Stat(parentPath)
	offsetInfo, _ := os.Stat(offsetsPath)
	rootInfo, _ := os.Stat(rootsPath)
	topoScratch := runBytes
	if parentInfo != nil {
		topoScratch += parentInfo.Size()
	}
	if offsetInfo != nil {
		topoScratch += offsetInfo.Size()
	}
	if rootInfo != nil {
		topoScratch += rootInfo.Size()
	}
	if base := topoScratch; base > *scratchHigh {
		*scratchHigh = base
	}
	entries := make([]indexSectionTableEntry, 0, 2)
	reports := make([]directV9SectionReport, 0, 2)
	if err := writeAlignment(cw, 8); err != nil {
		return nil, nil, err
	}
	offset := uint64(cw.n)
	if err := binary.Write(cw, binary.LittleEndian, uint32(recordCount+1)); err != nil {
		return nil, nil, err
	}
	if err := directV9CopyFile(cw, offsetsPath); err != nil {
		return nil, nil, err
	}
	if err := binary.Write(cw, binary.LittleEndian, uint32(childCount)); err != nil {
		return nil, nil, err
	}
	if err := directV9MergeChildRuns(ctx, cw, runs, int(childCount)); err != nil {
		return nil, nil, err
	}
	rootCount := int(rootInfo.Size() / 4)
	if err := binary.Write(cw, binary.LittleEndian, uint32(rootCount)); err != nil {
		return nil, nil, err
	}
	if err := directV9CopyFile(cw, rootsPath); err != nil {
		return nil, nil, err
	}
	entries = append(entries, indexSectionTableEntry{tag: indexSectionCHLD, offset: offset, length: uint64(cw.n) - offset})
	reports = append(reports, directV9SectionReport{Name: "CHLD", Tag: indexSectionCHLD, Runs: len(runs), Bytes: int64(cw.n) - int64(offset), ScratchBytes: topoScratch})
	for _, run := range runs {
		_ = os.Remove(run.path)
	}
	if err := writeAlignment(cw, 8); err != nil {
		return nil, nil, err
	}
	offset = uint64(cw.n)
	if err := binary.Write(cw, binary.LittleEndian, uint32(recordCount)); err != nil {
		return nil, nil, err
	}
	if err := directV9CopyFile(cw, frnPath); err != nil {
		return nil, nil, err
	}
	if err := binary.Write(cw, binary.LittleEndian, uint32(recordCount)); err != nil {
		return nil, nil, err
	}
	var ids [256]byte
	for start := 0; start < recordCount; {
		count := min(recordCount-start, len(ids)/4)
		for i := 0; i < count; i++ {
			binary.LittleEndian.PutUint32(ids[i*4:], uint32(start+i))
		}
		if _, err := cw.Write(ids[:count*4]); err != nil {
			return nil, nil, err
		}
		start += count
	}
	entries = append(entries, indexSectionTableEntry{tag: indexSectionFRNS, offset: offset, length: uint64(cw.n) - offset})
	reports = append(reports, directV9SectionReport{Name: "FRNS", Tag: indexSectionFRNS, Runs: 0, Bytes: int64(cw.n) - int64(offset), ScratchBytes: 0})
	return entries, reports, nil
}

func directV9WriteSubtreeSection(ctx context.Context, cw *countingWriter, parentPath string, recordCount int, rankPaths []string, owned *[]string, scratchHigh *int64) (indexSectionTableEntry, directV9SectionReport, []string, error) {
	parents := make([]int32, recordCount)
	f, err := os.Open(parentPath)
	if err != nil {
		return indexSectionTableEntry{}, directV9SectionReport{}, nil, err
	}
	for id := range parents {
		var b [4]byte
		if _, err := f.ReadAt(b[:], int64(id)*4); err != nil {
			_ = f.Close()
			return indexSectionTableEntry{}, directV9SectionReport{}, nil, err
		}
		value := binary.LittleEndian.Uint32(b[:])
		if value == ^uint32(0) {
			parents[id] = -1
		} else if value >= uint32(recordCount) {
			_ = f.Close()
			return indexSectionTableEntry{}, directV9SectionReport{}, nil, errors.New("direct v9 subtree parent out of range")
		} else {
			parents[id] = int32(value)
		}
	}
	_ = f.Close()
	counts := make([]uint32, recordCount)
	roots := make([]uint32, 0, 16)
	for id, parent := range parents {
		if parent < 0 {
			roots = append(roots, uint32(id))
		} else {
			counts[parent]++
		}
	}
	offsets := make([]uint32, recordCount+1)
	for i := 0; i < recordCount; i++ {
		offsets[i+1] = offsets[i] + counts[i]
	}
	children := make([]uint32, offsets[recordCount])
	next := append([]uint32(nil), offsets[:recordCount]...)
	for id, parent := range parents {
		if parent >= 0 {
			children[next[parent]] = uint32(id)
			next[parent]++
		}
	}
	start := make([]uint32, recordCount)
	end := make([]uint32, recordCount)
	for i := range start {
		start[i] = ^uint32(0)
		end[i] = ^uint32(0)
	}
	order := make([]uint32, 0, recordCount)
	type frame struct {
		id   uint32
		next uint32
	}
	visit := func(root uint32) {
		if int(root) >= recordCount || start[root] != ^uint32(0) {
			return
		}
		start[root] = uint32(len(order))
		order = append(order, root)
		stack := []frame{{id: root}}
		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			childStart, childEnd := offsets[top.id], offsets[top.id+1]
			if childStart+top.next < childEnd {
				child := children[childStart+top.next]
				top.next++
				if start[child] == ^uint32(0) {
					start[child] = uint32(len(order))
					order = append(order, child)
					stack = append(stack, frame{id: child})
				}
				continue
			}
			end[top.id] = uint32(len(order))
			stack = stack[:len(stack)-1]
		}
	}
	for _, root := range roots {
		visit(root)
	}
	for id := 0; id < recordCount; id++ {
		visit(uint32(id))
	}
	if len(order) != recordCount {
		return indexSectionTableEntry{}, directV9SectionReport{}, nil, errors.New("direct v9 subtree traversal did not cover all records")
	}
	if err := writeAlignment(cw, 8); err != nil {
		return indexSectionTableEntry{}, directV9SectionReport{}, nil, err
	}
	offset := uint64(cw.n)
	writePart := func(values []uint32) error {
		if err := binary.Write(cw, binary.LittleEndian, uint32(len(values))); err != nil {
			return err
		}
		if len(values) == 0 {
			return nil
		}
		_, err := cw.Write(uint32SliceBytes(values))
		return err
	}
	if err := writePart(start); err != nil {
		return indexSectionTableEntry{}, directV9SectionReport{}, nil, err
	}
	if err := writePart(end); err != nil {
		return indexSectionTableEntry{}, directV9SectionReport{}, nil, err
	}
	if err := writePart(order); err != nil {
		return indexSectionTableEntry{}, directV9SectionReport{}, nil, err
	}
	subtreeRankPaths := make([]string, 0, len(rankPaths))
	for rankIndex, rankPath := range rankPaths {
		rankFile, openErr := os.Open(rankPath)
		if openErr != nil {
			return indexSectionTableEntry{}, directV9SectionReport{}, nil, openErr
		}
		ranks := make([]uint32, recordCount)
		best := make([]uint32, recordCount)
		for id := range best {
			best[id] = ^uint32(0)
		}
		var buf [4]byte
		for id := range ranks {
			if _, err := rankFile.ReadAt(buf[:], int64(id)*4); err != nil {
				_ = rankFile.Close()
				return indexSectionTableEntry{}, directV9SectionReport{}, nil, err
			}
			ranks[id] = binary.LittleEndian.Uint32(buf[:])
		}
		_ = rankFile.Close()
		for pos := len(order) - 1; pos >= 0; pos-- {
			id := order[pos]
			best[id] = ranks[id]
			for childPos := offsets[id]; childPos < offsets[id+1]; childPos++ {
				if best[children[childPos]] < best[id] {
					best[id] = best[children[childPos]]
				}
			}
		}
		bestPath := filepath.Join(filepath.Dir(parentPath), fmt.Sprintf("direct-v9-subtree-rank-%d.tmp", rankIndex))
		if err := os.WriteFile(bestPath, uint32SliceBytes(best), 0o600); err != nil {
			return indexSectionTableEntry{}, directV9SectionReport{}, nil, err
		}
		*owned = append(*owned, bestPath)
		subtreeRankPaths = append(subtreeRankPaths, bestPath)
		// Canonical SUBT stores no name-rank part: start/end/order are followed
		// by size, modified, extension, type, and path subtree minima.
		if rankIndex > 0 {
			if err := writePart(best); err != nil {
				return indexSectionTableEntry{}, directV9SectionReport{}, nil, err
			}
		}
	}
	entry := indexSectionTableEntry{tag: indexSectionSUBT, offset: offset, length: uint64(cw.n) - offset}
	scratch := int64(recordCount) * 4 * 7
	if info, statErr := os.Stat(parentPath); statErr == nil {
		scratch += info.Size()
	}
	if scratch > *scratchHigh {
		*scratchHigh = scratch
	}
	return entry, directV9SectionReport{Name: "SUBT", Tag: indexSectionSUBT, Runs: 0, Bytes: int64(entry.length), ScratchBytes: scratch}, subtreeRankPaths, nil
}

func uint32SliceBytes(values []uint32) []byte {
	if len(values) == 0 {
		return nil
	}
	bytes := make([]byte, len(values)*4)
	for i, value := range values {
		binary.LittleEndian.PutUint32(bytes[i*4:], value)
	}
	return bytes
}

func directV9ScanNames(finalPath, tokenPath string) (int64, int, error) {
	f, err := os.Open(finalPath)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	var tokens *os.File
	if tokenPath != "" {
		tokens, err = os.Create(tokenPath)
		if err != nil {
			return 0, 0, err
		}
		defer tokens.Close()
	}
	r := bufio.NewReaderSize(f, 256*1024)
	var offset uint64
	var count int
	for {
		rec, readErr := readDirectV9SpoolRecord(r)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return 0, 0, readErr
		}
		if len(rec.Name) > int(^uint16(0)) || offset > uint64(^uint32(0))-uint64(len(rec.Name)) {
			return 0, 0, errors.New("direct v9 name table exceeds on-disk limits")
		}
		if tokens != nil {
			var entry [6]byte
			binary.LittleEndian.PutUint32(entry[:4], uint32(offset))
			binary.LittleEndian.PutUint16(entry[4:], uint16(len(rec.Name)))
			if _, err := tokens.Write(entry[:]); err != nil {
				return 0, 0, err
			}
		}
		offset += uint64(len(rec.Name))
		count++
	}
	// The name-table temp is owned scratch consumed later in this process.
	return int64(offset), count, nil
}

func directV9FoldName(rec directV9Record) string { return strings.ToLower(rec.Name) }

func directV9SignedOrderKey(value int64) string {
	var b [8]byte
	u := uint64(value) ^ (uint64(1) << 63)
	binary.BigEndian.PutUint64(b[:], u)
	return string(b[:])
}

func directV9LowerExt(rec directV9Record) string {
	ext := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
	return strings.ToLower(ext)
}

func directV9LowerPath(rec directV9Record) string {
	if rec.Path != "" {
		return strings.ToLower(filepath.Clean(rec.Path))
	}
	return strings.ToLower(rec.Name)
}

func directV9RankSpecs() []directV9RankSpec {
	return []directV9RankSpec{
		{Tag: indexSectionRANK, Name: "RANK", Key: directV9FoldName},
		{Tag: indexSectionSRNK, Name: "SRNK", Key: func(rec directV9Record) string {
			return directV9SignedOrderKey(rec.Size) + "\x00" + strings.ToLower(rec.Name)
		}},
		{Tag: indexSectionMRNK, Name: "MRNK", Key: func(rec directV9Record) string {
			if rec.ModUnix == 0 {
				return "\x01" + strings.ToLower(rec.Name)
			}
			var b [8]byte
			u := uint64(rec.ModUnix) ^ (uint64(1) << 63)
			binary.BigEndian.PutUint64(b[:], ^u)
			return "\x00" + string(b[:]) + "\x00" + strings.ToLower(rec.Name)
		}},
		{Tag: indexSectionERNK, Name: "ERNK", Key: func(rec directV9Record) string {
			return directV9LowerExt(rec) + "\x00" + strings.ToLower(rec.Name)
		}},
		{Tag: indexSectionTRNK, Name: "TRNK", Key: func(rec directV9Record) string {
			kind := byte(1)
			if rec.Mode&uint32(os.ModeDir) != 0 {
				kind = 0
			}
			return string([]byte{kind}) + "\x00" + strings.ToLower(rec.Name)
		}},
		{Tag: indexSectionPRNK, Name: "PRNK", Key: func(rec directV9Record) string {
			return directV9LowerPath(rec)
		}},
	}
}

func directV9BuildRankRuns(ctx context.Context, finalPath, spoolDir string, maxRecords int, spec directV9RankSpec, owned *[]string) ([]directV9RunFile, int, int64, error) {
	f, err := os.Open(finalPath)
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()
	if maxRecords <= 0 {
		maxRecords = directV9DefaultRunRecords
	}
	r := bufio.NewReaderSize(f, 256*1024)
	chunk := make([]directV9RankItem, 0, min(maxRecords, 4096))
	var runs []directV9RunFile
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		sort.Slice(chunk, func(i, j int) bool {
			if chunk[i].Key != chunk[j].Key {
				return chunk[i].Key < chunk[j].Key
			}
			return chunk[i].ID < chunk[j].ID
		})
		path := filepath.Join(spoolDir, fmt.Sprintf("direct-v9-rank-%s-%06d.tmp", spec.Name, len(runs)))
		run, err := os.Create(path)
		if err != nil {
			return err
		}
		bw := bufio.NewWriterSize(run, 256*1024)
		var bytesWritten int64
		for _, item := range chunk {
			var id [4]byte
			binary.LittleEndian.PutUint32(id[:], item.ID)
			if _, err := bw.Write(id[:]); err != nil {
				_ = run.Close()
				return err
			}
			bytesWritten += 4
			if uint64(len(item.Key)) > uint64(^uint32(0)) {
				_ = run.Close()
				return errors.New("direct v9 rank key too large")
			}
			var n [4]byte
			binary.LittleEndian.PutUint32(n[:], uint32(len(item.Key)))
			if _, err := bw.Write(n[:]); err != nil {
				_ = run.Close()
				return err
			}
			if _, err := io.WriteString(bw, item.Key); err != nil {
				_ = run.Close()
				return err
			}
			bytesWritten += int64(4 + len(item.Key))
		}
		if err := bw.Flush(); err != nil {
			_ = run.Close()
			return err
		}
		if err := run.Close(); err != nil {
			return err
		}
		*owned = append(*owned, path)
		runs = append(runs, directV9RunFile{path: path, bytes: bytesWritten})
		chunk = make([]directV9RankItem, 0, min(maxRecords, 4096))
		return nil
	}
	var id uint32
	var count int
	for {
		select {
		case <-ctx.Done():
			return nil, 0, 0, ctx.Err()
		default:
		}
		rec, readErr := readDirectV9SpoolRecord(r)
		if errors.Is(readErr, io.EOF) {
			if err := flush(); err != nil {
				return nil, 0, 0, err
			}
			var maxBytes int64
			for _, run := range runs {
				maxBytes = max(maxBytes, run.bytes)
			}
			return runs, count, maxBytes, nil
		}
		if readErr != nil {
			return nil, 0, 0, readErr
		}
		if !rec.Deleted() {
			chunk = append(chunk, directV9RankItem{Key: spec.Key(rec), ID: id})
			count++
		}
		id++
		if len(chunk) >= maxRecords {
			if err := flush(); err != nil {
				return nil, 0, 0, err
			}
		}
	}
}

// directV9Record has no deletion field in the first direct slice.  Keeping
// this method makes the rank writer's contract explicit for the next source
// slice, which will carry tombstones from USN/MFT.
func (r directV9Record) Deleted() bool { return false }

func directV9ReadRankItem(r *bufio.Reader) (directV9RankItem, error) {
	var idBytes [4]byte
	if _, err := io.ReadFull(r, idBytes[:]); err != nil {
		return directV9RankItem{}, err
	}
	var lenBytes [4]byte
	if _, err := io.ReadFull(r, lenBytes[:]); err != nil {
		return directV9RankItem{}, err
	}
	keyLen := binary.LittleEndian.Uint32(lenBytes[:])
	if uint64(keyLen) > uint64(^uint(0)>>1) {
		return directV9RankItem{}, errors.New("direct v9 rank key too large")
	}
	key := make([]byte, int(keyLen))
	if _, err := io.ReadFull(r, key); err != nil {
		return directV9RankItem{}, err
	}
	return directV9RankItem{ID: binary.LittleEndian.Uint32(idBytes[:]), Key: string(key)}, nil
}

// directV9ComputeRankFamily merges one rank family's external-sort runs into
// its section bytes plus a rank-by-id array, writing both to private temp
// files.  It is safe to run for multiple families concurrently: each family
// touches only its own runs and its own temp paths, and the output file order
// is preserved by the caller's serial emission phase.
func directV9ComputeRankFamily(ctx context.Context, tag uint32, runs []directV9RunFile, recordCount, liveCount int, sectionPath, rankPath string, owned *[]string) (int64, error) {
	section, err := os.Create(sectionPath)
	if err != nil {
		return 0, err
	}
	*owned = append(*owned, sectionPath)
	sw := bufio.NewWriterSize(section, 256*1024)
	written := int64(0)
	if err := binary.Write(sw, binary.LittleEndian, uint32(liveCount)); err != nil {
		_ = section.Close()
		return 0, err
	}
	written += 4
	// The rank-by-id array is filled in memory during the merge and written
	// once per family; the array is bounded by recordCount*4 and released when
	// the family completes.  Multiple families may be in flight, so the caller
	// bounds the worker pool.
	ranks := make([]byte, recordCount*4)
	readers := make([]*bufio.Reader, len(runs))
	files := make([]*os.File, len(runs))
	h := &directV9RankHeap{}
	heap.Init(h)
	for i, run := range runs {
		f, openErr := os.Open(run.path)
		if openErr != nil {
			_ = section.Close()
			return 0, openErr
		}
		files[i] = f
		readers[i] = bufio.NewReaderSize(f, 256*1024)
		item, readErr := directV9ReadRankItem(readers[i])
		if readErr == nil {
			heap.Push(h, itemWithRun{item: item, run: i})
		} else if !errors.Is(readErr, io.EOF) {
			_ = section.Close()
			return 0, readErr
		}
	}
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	for rank := 0; h.Len() > 0; rank++ {
		select {
		case <-ctx.Done():
			_ = section.Close()
			return 0, ctx.Err()
		default:
		}
		head := heap.Pop(h).(itemWithRun)
		if err := binary.Write(sw, binary.LittleEndian, head.item.ID); err != nil {
			_ = section.Close()
			return 0, err
		}
		written += 4
		binary.LittleEndian.PutUint32(ranks[head.item.ID*4:head.item.ID*4+4], uint32(rank))
		next, readErr := directV9ReadRankItem(readers[head.run])
		if readErr == nil {
			heap.Push(h, itemWithRun{item: next, run: head.run})
		} else if !errors.Is(readErr, io.EOF) {
			_ = section.Close()
			return 0, readErr
		}
	}
	if err := binary.Write(sw, binary.LittleEndian, uint32(recordCount)); err != nil {
		_ = section.Close()
		return 0, err
	}
	written += 4
	if _, err := sw.Write(ranks); err != nil {
		_ = section.Close()
		return 0, err
	}
	written += int64(len(ranks))
	if err := sw.Flush(); err != nil {
		_ = section.Close()
		return 0, err
	}
	if err := section.Close(); err != nil {
		return 0, err
	}
	// Persist the rank-by-id array for the retained copy the caller needs for
	// subtree/bounds.  One sequential write replaces N random WriteAt calls.
	rankFile, err := os.Create(rankPath)
	if err != nil {
		return 0, err
	}
	*owned = append(*owned, rankPath)
	if _, err := rankFile.Write(ranks); err != nil {
		_ = rankFile.Close()
		return 0, err
	}
	if err := rankFile.Close(); err != nil {
		return 0, err
	}
	ranks = nil
	return written, nil
}

// directV9EmitRankSection streams a computed rank-family section into the
// output writer with alignment, preserving the section byte layout exactly.
func directV9EmitRankSection(cw *countingWriter, tag uint32, sectionPath string) (indexSectionTableEntry, error) {
	if err := writeAlignment(cw, 8); err != nil {
		return indexSectionTableEntry{}, err
	}
	offset := uint64(cw.n)
	if err := copyFileToWriter(cw, sectionPath); err != nil {
		return indexSectionTableEntry{}, err
	}
	return indexSectionTableEntry{tag: tag, offset: offset, length: uint64(cw.n) - offset}, nil
}

type itemWithRun struct {
	item directV9RankItem
	run  int
}

func (h itemWithRun) less(other itemWithRun) bool {
	if h.item.Key != other.item.Key {
		return h.item.Key < other.item.Key
	}
	return h.item.ID < other.item.ID
}

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
	if _, err := directV9WriteHeaderAndBase(ctx, cw, finalPath, tokenPath, frnPath, opts, recordCount, nameBlobLen); err != nil {
		cleanup()
		return 0, err
	}
	entries := make([]indexSectionTableEntry, 0, len(rankSpecs))
	rankScratchPaths := make([]string, 0, len(rankSpecs))
	sharedRankRuns, sharedRankErr := directV9BuildRankRunsShared(ctx, finalPath, filepath.Dir(finalPath), opts.RunRecords, opts.RankWorkers, rankSpecs, owned)
	if sharedRankErr != nil {
		cleanup()
		return 0, sharedRankErr
	}
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
	topologyEntries, topologyReports, topologyErr := directV9WriteTopologySections(ctx, cw, finalPath, frnPath, filepath.Dir(finalPath), recordCount, opts.RunRecords, owned, scratchHigh)
	if topologyErr != nil {
		cleanup()
		return 0, topologyErr
	}
	entries = append(entries, topologyEntries...)
	if sectionReports != nil {
		*sectionReports = append(*sectionReports, topologyReports...)
	}
	parentPath := filepath.Join(filepath.Dir(finalPath), "direct-v9-parents.tmp")
	subtreeEntry, subtreeReport, subtreeRankPaths, subtreeErr := directV9WriteSubtreeSection(ctx, cw, parentPath, recordCount, rankScratchPaths, owned, scratchHigh)
	if subtreeErr != nil {
		cleanup()
		return 0, subtreeErr
	}
	entries = append(entries, subtreeEntry)
	if sectionReports != nil {
		*sectionReports = append(*sectionReports, subtreeReport)
	}
	auxEntries, auxReports, auxErr := directV9WriteAuxiliarySections(ctx, cw, finalPath, recordCount, opts.RunRecords, rankScratchPaths[0], subtreeRankPaths, filepath.Dir(finalPath), owned, scratchHigh)
	if auxErr != nil {
		cleanup()
		return 0, auxErr
	}
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
	runs, maxRunBytes, err := directV9BuildRuns(ctx, opts.Records, opts.SpoolDir, opts.RunRecords, opts.RunBytes, &owned)
	if err != nil {
		return stats, err
	}
	if opts.WalkReport != nil && !opts.WalkReport.SourceComplete {
		if opts.WalkReport.Inaccessible > 0 && opts.WalkReport.Inaccessible <= opts.MaxInaccessible {
			stats.SourceDegraded = true
		} else {
			return stats, fmt.Errorf("direct v9 source incomplete: skipped=%d inaccessible=%d reparse=%d max-inaccessible=%d examples=%v", opts.WalkReport.Skipped, opts.WalkReport.Inaccessible, opts.WalkReport.ReparseSkipped, opts.MaxInaccessible, opts.WalkReport.SkipExamples)
		}
	}
	stats.Runs = len(runs)
	stats.MaxRunBytes = maxRunBytes
	var runBytes int64
	for _, run := range runs {
		runBytes += run.bytes
	}
	finalPath := filepath.Join(opts.SpoolDir, "direct-v9-records.final.tmp")
	frnPath := filepath.Join(opts.SpoolDir, "direct-v9-frns.tmp")
	recordCount, spoolBytes, err := directV9MergeRuns(ctx, runs, finalPath, frnPath, &owned)
	if err != nil {
		return stats, err
	}
	stats.Records = recordCount
	stats.SpoolBytes = spoolBytes
	stats.FinalIDRule = "ascending-frn; duplicate-frn-rejected"
	stats.SpoolSchema = "u64 frn,parent_frn; u32 mode; i64 size,mod_unix; u32 name_bytes,path_bytes; utf8 name,path"
	tokenPath := filepath.Join(opts.SpoolDir, "direct-v9-name-table.tmp")
	owned = append(owned, tokenPath)
	nameBlobLen, tokenCount, err := directV9ScanNames(finalPath, tokenPath)
	if err != nil {
		return stats, err
	}
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
	outputBytes, err := directV9WriteAtomic(ctx, opts, finalPath, frnPath, recordCount, nameBlobLen, tokenPath, rankSpecs, baseRankScratch, &stats.RankFamilies, &stats.SectionReports, &stats.ScratchBytes, &owned)
	if err != nil {
		return stats, err
	}
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

// cmdDirectV9 exposes only the bounded prototype sources.  It deliberately
// has no service, elevation, compactor, or existing-index input path.
func cmdDirectV9(args []string) error {
	fs := flag.NewFlagSet("direct-v9", flag.ContinueOnError)
	out := fs.String("out", "", "new v9 output path")
	root := fs.String("root", "", "read-only filesystem root for the walk fallback")
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
	if (*root == "") == (*records == 0) {
		return errors.New("direct-v9 requires exactly one of -root or -records")
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
	var sourceName, volume string
	var closeSource func()
	var walkReport *directV9WalkReport
	if *root != "" {
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
		volume = filepath.VolumeName(rootAbs)
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
	stats, err := buildDirectV9(ctx, directV9BuildOptions{
		OutputPath:  *out,
		SpoolDir:    *spool,
		Roots:       roots,
		Volume:      volume,
		Source:      sourceName,
		BuiltAt:     time.Unix(0, 0),
		Records:     source,
		RunRecords:  *runRecords,
		RunBytes:    *runBytes,
		RankWorkers: *rankWorkers,
		WalkWorkers: *walkWorkers,
		WalkQueue:   *walkQueue,
		WalkReport:  walkReport,
		MaxInaccessible: *maxInaccessible,
	})
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
