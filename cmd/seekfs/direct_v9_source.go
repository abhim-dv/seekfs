package main

// The direct builder is intentionally independent from Index and
// buildOrders.  It is the bounded ingestion seam for the direct filesystem
// to v9 path: sources write records into owned external-sort runs, final IDs
// are assigned once by FRN, and the existing v9 reader contract is emitted
// directly from the final spool.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

	// PhaseReporter receives per-phase build durations; it may be called from
	// worker goroutines during the rank-family phase, so implementations must be
	// safe for concurrent use.
	PhaseReporter func(name string, d time.Duration)
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
	PhaseMillis              []string                `json:"phase_millis,omitempty"`
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
	// KeyWithID, when set, overrides Key and receives the record id so the
	// size rank can order directories by their recursive subtree total.
	KeyWithID func(uint32, directV9Record) string
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
	parentFRN := e.parentFRN
	if parentFRN == e.frn {
		parentFRN = 0
	}
	return directV9Record{
		FRN:       e.frn,
		ParentFRN: parentFRN,
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
