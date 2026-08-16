package main

import (
	"bufio"
	"bytes"
	"container/heap"
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const indexVersion = 8
const indexVersionV9 = 9
const servicePathCacheLimit = 25_000
const serviceStartupDefaultWorkers = 2
const serviceResidentNameOrderMaxRecords = 2_000_000
const serviceBackgroundNameOrderMaxRecords = 30_000_000
const serviceExtTopPostingLimit = 512
const serviceResidentChildRangeMaxRecords = 2_000_000
const serviceStartupWALRebuildBytes = 512 * 1024 * 1024
const defaultQueryPostingPrefetchBytes = 32 * 1024 * 1024
const serviceNameTrigramCandidateMaxIDs = 25_000
const servicePathNameTrigramCandidateMaxIDs = 250_000
const serviceComponentTrigramCandidateMaxIDs = 10_000
const serviceComponentTrigramExpansionMaxIDs = 25_000
const serviceComponentMultiTermScanMaxIDs = 500_000
const serviceCompleteFilenameOrderWalkMaxRecords = 2_000_000
const serviceTrigramParallelVerifyMinIDs = 4_096
const serviceRankParallelMinIDs = 500_000
const serviceNameTrigramDefaultMaxRecords = 20_000_000
const filesystemFallbackMaxVisited = 100_000
const filesystemFallbackMaxDuration = 2 * time.Second

const (
	nameTrigramStateDisabled int32 = iota
	nameTrigramStatePending
	nameTrigramStateBuilding
	nameTrigramStateReady
)

var indexMagic = [8]byte{'G', 'O', 'S', 'R', 'C', 'H', '0', '8'}
var indexMagicV9 = [8]byte{'G', 'O', 'S', 'R', 'C', 'H', '0', '9'}
var walMagicV1 = []byte{'S', 'W', 'A', 'L', '1'}

const (
	indexSectionRANK uint32 = 'R'<<24 | 'A'<<16 | 'N'<<8 | 'K'
	indexSectionSRNK uint32 = 'S'<<24 | 'R'<<16 | 'N'<<8 | 'K'
	indexSectionMRNK uint32 = 'M'<<24 | 'R'<<16 | 'N'<<8 | 'K'
	indexSectionERNK uint32 = 'E'<<24 | 'R'<<16 | 'N'<<8 | 'K'
	indexSectionTRNK uint32 = 'T'<<24 | 'R'<<16 | 'N'<<8 | 'K'
	indexSectionPRNK uint32 = 'P'<<24 | 'R'<<16 | 'N'<<8 | 'K'
	indexSectionSUBT uint32 = 'S'<<24 | 'U'<<16 | 'B'<<8 | 'T'
	indexSectionCHLD uint32 = 'C'<<24 | 'H'<<16 | 'L'<<8 | 'D'
	indexSectionFRNS uint32 = 'F'<<24 | 'R'<<16 | 'N'<<8 | 'S'
	indexSectionLOWR uint32 = 'L'<<24 | 'O'<<16 | 'W'<<8 | 'R'
	indexSectionPATR uint32 = 'P'<<24 | 'A'<<16 | 'T'<<8 | 'R'
	indexSectionPEXT uint32 = 'P'<<24 | 'E'<<16 | 'X'<<8 | 'T'
	indexSectionPXRB uint32 = 'P'<<24 | 'X'<<16 | 'R'<<8 | 'B'
	indexSectionPXRC uint32 = 'P'<<24 | 'X'<<16 | 'R'<<8 | 'C'
	indexSectionPCMP uint32 = 'P'<<24 | 'C'<<16 | 'M'<<8 | 'P'
	indexSectionPNGR uint32 = 'P'<<24 | 'N'<<16 | 'G'<<8 | 'R'
	// PNGC is an optional companion to PNGR containing postings for grams
	// omitted from the selective name index.  Older readers ignore it.
	indexSectionPNGC uint32 = 'P'<<24 | 'N'<<16 | 'G'<<8 | 'C'
)

const gramPostingUnionMetadataMagic uint32 = 0x47524d32 // "GRM2"

func engineV9Enabled() bool {
	return envBool("SEEKFS_ENGINE_V9")
}

func globalPlannerEnabled() bool {
	return envBool("SEEKFS_GLOBAL_PLANNER")
}

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

const packedLowerSameAsName = ^uint32(0)

const (
	fsctlEnumUSNData            = 0x000900b3
	fsctlQueryUSNJournal        = 0x000900f4
	fsctlReadUSNJournal         = 0x000900bb
	fileAttributeReadonly       = 0x01
	fileAttributeHidden         = 0x02
	fileAttributeSystem         = 0x04
	fileAttributeDir            = 0x10
	fileAttributeArchive        = 0x20
	usnReasonFileCreate         = 0x00000100
	usnReasonFileDelete         = 0x00000200
	usnReasonRenameOld          = 0x00001000
	usnReasonRenameNew          = 0x00002000
	serviceName                 = "seekfs"
	defaultServicePipe          = `\\.\pipe\seekfs-service`
	defaultServiceSDDL          = `D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;IU)`
	serviceQueryTimeout         = 30 * time.Second
	persistDebounce             = 5 * time.Minute
	overlayCompactionDirtyAge   = 30 * time.Minute
	overlayCompactionMaxSlots   = 64 * 1024
	overlayCompactionMaxWAL     = 64 * 1024 * 1024
	overlayCompactionTombstoneP = 5
	compactDiskRecordBytes      = 43
	compactWideDiskRecordBytes  = 45
	compactDiskFlag             = 1
	compactDiskWideRefsFlag     = 2
	compactDiskAttrsFlag        = 4
	compactNarrowParentSentinel = 0xFFFFFF
	compactNarrowMaxRecordRef   = compactNarrowParentSentinel - 1
	compactWideParentSentinel   = ^uint32(0)
	packedSize64Sentinel        = ^uint32(0)
)

type Entry struct {
	Path        string
	Name        string
	LowerPath   string
	LowerName   string
	Size        int64
	Mode        uint32
	ModUnix     int64
	IndexSource string
}

type jsonError struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

type jsonResult struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Volume      string `json:"volume,omitempty"`
	IsDir       bool   `json:"is_dir"`
	Size        *int64 `json:"size,omitempty"`
	Modified    string `json:"modified,omitempty"`
	IndexSource string `json:"index_source,omitempty"`
	Exists      *bool  `json:"exists,omitempty"`
}

func boolPtr(v bool) *bool {
	return &v
}

type jsonSearchResponse struct {
	OK                       bool           `json:"ok"`
	Query                    string         `json:"query"`
	Count                    int            `json:"count"`
	Limit                    int            `json:"limit,omitempty"`
	SearchMS                 float64        `json:"search_ms,omitempty"`
	Source                   string         `json:"source,omitempty"`
	Decline                  string         `json:"decline,omitempty"`
	Candidates               int            `json:"candidates,omitempty"`
	BlocksDecoded            int            `json:"blocks_decoded,omitempty"`
	BlocksSkipped            int            `json:"blocks_skipped,omitempty"`
	ScalarDriver             string         `json:"scalar_driver,omitempty"`
	ScalarInterval           int            `json:"scalar_interval,omitempty"`
	RecordsVerified          int            `json:"records_verified,omitempty"`
	ComponentDriver          string         `json:"component_driver,omitempty"`
	ComponentRoots           int            `json:"component_roots,omitempty"`
	ComponentIntervals       int            `json:"component_intervals,omitempty"`
	ComponentCardinality     int            `json:"component_cardinality,omitempty"`
	ComponentSelfHits        int            `json:"component_self_hits,omitempty"`
	ComponentBounds          string         `json:"component_bounds,omitempty"`
	ComponentRecordsVerified int            `json:"component_records_verified,omitempty"`
	FilenameDriver           string         `json:"filename_driver,omitempty"`
	FilenameRequiredGrams    int            `json:"filename_required_grams,omitempty"`
	FilenamePostingHint      int            `json:"filename_posting_hint,omitempty"`
	FilenameRecordsVerified  int            `json:"filename_records_verified,omitempty"`
	OverlayBaseWindow        int            `json:"overlay_base_window,omitempty"`
	PostingPrefetchBytes     int            `json:"posting_prefetch_bytes,omitempty"`
	PostingPrefetchRanges    int            `json:"posting_prefetch_ranges,omitempty"`
	PostingPrefetchPages     int            `json:"posting_prefetch_pages,omitempty"`
	PlannerMode              string         `json:"planner_mode,omitempty"`
	EligibleVolumes          []string       `json:"eligible_volumes,omitempty"`
	Terms                    []traceTerm    `json:"terms,omitempty"`
	Declines                 []traceDecline `json:"declines,omitempty"`
	Fallback                 string         `json:"fallback,omitempty"`
	Complete                 *bool          `json:"complete,omitempty"`
	Results                  []jsonResult   `json:"results,omitempty"`
}

type jsonInfoResponse struct {
	OK          bool         `json:"ok"`
	Version     int          `json:"version"`
	Source      string       `json:"source"`
	BuiltAt     string       `json:"built_at"`
	Entries     int          `json:"entries"`
	Roots       []string     `json:"roots"`
	Volume      string       `json:"volume,omitempty"`
	JournalID   uint64       `json:"journal_id,omitempty"`
	Checkpoint  int64        `json:"checkpoint_usn,omitempty"`
	ContentHash string       `json:"content_hash,omitempty"`
	Layout      *indexLayout `json:"layout,omitempty"`
}

type indexLayout struct {
	FileBytes      int64   `json:"file_bytes,omitempty"`
	RecordBytes    int64   `json:"record_bytes,omitempty"`
	NameBlobBytes  int64   `json:"name_blob_bytes,omitempty"`
	NameTableBytes int64   `json:"name_table_bytes,omitempty"`
	OtherBytes     int64   `json:"other_bytes,omitempty"`
	RecordCount    int     `json:"record_count,omitempty"`
	UniqueNames    int     `json:"unique_names,omitempty"`
	BytesPerRecord float64 `json:"bytes_per_record,omitempty"`
}

type doctorResponse struct {
	OK            bool               `json:"ok"`
	ServiceName   string             `json:"service_name"`
	Installed     bool               `json:"installed"`
	Running       bool               `json:"running"`
	ServiceError  string             `json:"service_error,omitempty"`
	PipeReachable bool               `json:"pipe_reachable"`
	Entries       int                `json:"entries,omitempty"`
	Loading       bool               `json:"loading,omitempty"`
	QueryOK       bool               `json:"query_ok"`
	Message       string             `json:"message,omitempty"`
	DBs           []dbInfo           `json:"dbs,omitempty"`
	Runtime       *runtimeMemoryInfo `json:"runtime,omitempty"`
}

type benchSummary struct {
	OK         bool                `json:"ok"`
	Mode       string              `json:"mode"`
	Iterations int                 `json:"iterations"`
	Failures   int                 `json:"failures"`
	Queries    int                 `json:"queries"`
	Stats      map[string]float64  `json:"stats_ms"`
	Backend    map[string]float64  `json:"backend_stats_ms,omitempty"`
	Sources    map[string]int      `json:"sources,omitempty"`
	Declines   map[string]int      `json:"declines,omitempty"`
	Candidates map[string]float64  `json:"candidate_stats,omitempty"`
	PerQuery   []benchQuerySummary `json:"per_query,omitempty"`
}

type benchQuerySummary struct {
	Query                 string             `json:"query"`
	Iterations            int                `json:"iterations"`
	Failures              int                `json:"failures"`
	Stats                 map[string]float64 `json:"stats_ms"`
	Backend               map[string]float64 `json:"backend_stats_ms,omitempty"`
	Sources               map[string]int     `json:"sources,omitempty"`
	Declines              map[string]int     `json:"declines,omitempty"`
	Candidates            map[string]float64 `json:"candidate_stats,omitempty"`
	ResultHash            string             `json:"result_hash,omitempty"`
	ResultCount           int                `json:"result_count,omitempty"`
	ResultConsistent      bool               `json:"result_consistent"`
	DiagnosticsConsistent bool               `json:"diagnostics_consistent"`
	Diagnostics           *benchDiagnostics  `json:"diagnostics,omitempty"`
}

type benchDiagnostics struct {
	Source          string `json:"source,omitempty"`
	Driver          string `json:"driver,omitempty"`
	Candidates      int    `json:"candidates,omitempty"`
	RecordsVerified int    `json:"records_verified,omitempty"`
	BlocksDecoded   int    `json:"blocks_decoded,omitempty"`
	BlocksSkipped   int    `json:"blocks_skipped,omitempty"`
	Complete        string `json:"complete,omitempty"`
}

type Index struct {
	Version                int
	Roots                  []string
	BuiltAt                time.Time
	Source                 string
	Volume                 string
	JournalID              uint64
	Checkpoint             int64
	ContentHash            string
	Entries                []Entry
	NameOrder              []int
	PathOrder              []int
	Compact                bool
	Records                []CompactRecord
	PackedRecords          *PackedRecords
	MMapRecords            *MMapRecords
	CompactAttrs           bool
	CompactNameOrder       []int
	NameBlob               []byte
	Derived                indexDerivedSections
	DBPath                 string
	baseDeletedState       atomic.Uint32 // 0 unknown, 1 no deleted records, 2 has deleted records
	componentCoverageMu    sync.Mutex
	componentCoverageCache map[string]mappedComponentCoverage
}

type indexDerivedSections struct {
	NameOrder        []uint32
	NameRank         []uint32
	SizeOrder        []uint32
	SizeRank         []uint32
	ModOrder         []uint32
	ModRank          []uint32
	ExtOrder         []uint32
	ExtRank          []uint32
	TypeOrder        []uint32
	TypeRank         []uint32
	PathOrder        []uint32
	PathRank         []uint32
	ChildOffsets     []uint32
	ChildIDs         []uint32
	RootIDs          []uint32
	SubtreeStart     []uint32
	SubtreeEnd       []uint32
	SubtreeOrder     []uint32
	SubtreeSizeRank  []uint32
	SubtreeModRank   []uint32
	SubtreeExtRank   []uint32
	SubtreeTypeRank  []uint32
	SubtreePathRank  []uint32
	FRNs             []uint64
	FRNRecordIDs     []uint32
	LowerBlob        []byte
	LowerOffs        []uint32
	LowerLens        []uint16
	AttrBits         map[uint32][]uint32
	Postings         map[uint32]mappedPostingSection
	PostingBounds    map[uint32]postingRankBounds
	NameTrigrams     *compressedTrigramIndex
	SelfNameTrigrams *compressedTrigramIndex
}

type mappedPostingSection struct {
	EntryCount int
	BlockCount int
	Bytes      int
	Data       []byte
	RankBounds postingRankBounds
}

type postingRankBounds struct {
	BlockCount int
	Name       []uint32
	Size       []uint32
	Modified   []uint32
	Extension  []uint32
	Type       []uint32
	Path       []uint32
}

type postingBlockCacheKey struct {
	base  uintptr
	bytes int
	block int
}

type postingBlockCacheEntry struct {
	key   postingBlockCacheKey
	ids   []uint32
	bytes int64
}

type postingBlockLRU struct {
	mu    sync.Mutex
	ll    list.List
	items map[postingBlockCacheKey]*list.Element
	bytes int64
}

var servicePostingBlockCache postingBlockLRU

type appConfig struct {
	DBs          []string
	Volumes      []string
	ServicePipe  string
	DefaultLimit int
	OutputFormat string
}

type queryOptions struct {
	Query         string       `json:"query"`
	MatchPath     bool         `json:"match_path"`
	Limit         int          `json:"limit"`
	Under         string       `json:"under,omitempty"`
	Exists        bool         `json:"exists,omitempty"`
	CWDBias       string       `json:"cwd_bias,omitempty"`
	RootBias      string       `json:"root_bias,omitempty"`
	Recent        string       `json:"recent,omitempty"`
	ModifiedAfter string       `json:"modified_after,omitempty"`
	CaseSensitive bool         `json:"case_sensitive,omitempty"`
	DeadlineUnix  int64        `json:"deadline_unix,omitempty"`
	RequestSeq    int64        `json:"request_seq,omitempty"`
	Cancel        func() bool  `json:"-"`
	Trace         *searchTrace `json:"-"`
}

type parsedQuery struct {
	Raw               string
	Terms             []string
	ImplicitPathTerms []string
	Impossible        bool
	MatchPath         bool
	CaseSensitive     bool
	Exts              []string
	Dirs              []string
	Globs             []string
	Regexps           []*regexp.Regexp
	RegexTerms        []string
	Type              string
	Parents           []string
	Under             string
	Exists            bool
	ModifiedAfter     time.Time
	HasModAfter       bool
	SizeFilters       []sizeFilter
	DateFilters       []dateFilter
	AttrFilters       []uint32
	SortColumn        string
	OrGroups          [][]parsedQuery
	NotGroups         []parsedQuery
	CWDBias           string
	RootBias          string
	Limit             int
	CountOnly         bool
	DeadlineUnix      int64
	Cancel            func() bool
	Trace             *searchTrace
}

type searchTrace struct {
	Source                   string
	Decline                  string
	Candidates               int
	PlannerMode              string
	EligibleVolumes          []string
	Terms                    []traceTerm
	Declines                 []traceDecline
	Fallback                 string
	BlocksDecoded            int
	BlocksSkipped            int
	ScalarDriver             string
	ScalarInterval           int
	ScalarRecordsVerified    int
	ComponentDriver          string
	ComponentRoots           int
	ComponentIntervals       int
	ComponentCardinality     int
	ComponentSelfHits        int
	ComponentBounds          string
	ComponentRecordsVerified int
	FilenameDriver           string `json:"filename_driver,omitempty"`
	FilenameRequiredGrams    int    `json:"filename_required_grams,omitempty"`
	FilenamePostingHint      int    `json:"filename_posting_hint,omitempty"`
	FilenameRecordsVerified  int    `json:"filename_records_verified,omitempty"`
	OverlayBaseWindow        int    `json:"overlay_base_window,omitempty"`
	PostingPrefetchBytes     int    `json:"posting_prefetch_bytes,omitempty"`
	PostingPrefetchRanges    int    `json:"posting_prefetch_ranges,omitempty"`
	PostingPrefetchPages     int    `json:"posting_prefetch_pages,omitempty"`
	Complete                 *bool
}

type traceTerm struct {
	Term      string `json:"term,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Source    string `json:"source,omitempty"`
	CountHint int    `json:"count_hint,omitempty"`
	Exact     bool   `json:"exact"`
	Volume    string `json:"volume,omitempty"`
}

type traceDecline struct {
	Source string `json:"source,omitempty"`
	Reason string `json:"reason,omitempty"`
	Volume string `json:"volume,omitempty"`
}

func (t *searchTrace) setSource(source string, candidates int) {
	if t == nil {
		return
	}
	if t.Source != "" && source != "exact-empty" {
		return
	}
	t.Source = source
	t.Candidates = candidates
}

func (t *searchTrace) setDecline(reason string) {
	if t == nil || t.Decline != "" {
		return
	}
	t.Decline = reason
	t.addDecline(reason)
}

func (t *searchTrace) setPlannerMode(mode string) {
	if t == nil || t.PlannerMode != "" {
		return
	}
	t.PlannerMode = mode
}

func (t *searchTrace) setEligibleVolumes(volumes []*serviceVolumeIndex) {
	if t == nil || t.EligibleVolumes != nil {
		return
	}
	t.EligibleVolumes = make([]string, 0, len(volumes))
	for _, vol := range volumes {
		if vol == nil || vol.volume == "" {
			continue
		}
		t.EligibleVolumes = append(t.EligibleVolumes, vol.volume)
	}
}

func (t *searchTrace) addPostingBlocks(decoded, skipped int) {
	if t == nil {
		return
	}
	t.BlocksDecoded += decoded
	t.BlocksSkipped += skipped
}

func (t *searchTrace) addComponentStats(driver string, roots, intervals, cardinality, selfHits, recordsVerified int, bounds bool) {
	if t == nil {
		return
	}
	if t.ComponentDriver == "" {
		t.ComponentDriver = driver
	} else if t.ComponentDriver != driver {
		t.ComponentDriver = "mixed"
	}
	t.ComponentRoots += roots
	t.ComponentIntervals += intervals
	t.ComponentCardinality += cardinality
	t.ComponentSelfHits += selfHits
	t.ComponentRecordsVerified += recordsVerified
	if t.ComponentBounds == "" {
		if bounds {
			t.ComponentBounds = "available"
		} else {
			t.ComponentBounds = "unavailable"
		}
	} else if !bounds {
		t.ComponentBounds = "mixed"
	}
}

func (t *searchTrace) replaceDecline(reason string) {
	if t == nil {
		return
	}
	t.Decline = reason
	t.addDecline(reason)
}

func (t *searchTrace) setFallback(route string) {
	if t == nil || t.Fallback != "" {
		return
	}
	t.Fallback = route
}

func (t *searchTrace) setComplete(complete bool) {
	if t == nil {
		return
	}
	t.Complete = boolPtr(complete)
}

func (t *searchTrace) completePtr() *bool {
	if t == nil || t.Complete == nil {
		return boolPtr(true)
	}
	return t.Complete
}

func (t *searchTrace) addTerm(term traceTerm) {
	if t == nil || term.Source == "" {
		return
	}
	t.Terms = append(t.Terms, term)
}

func (t *searchTrace) addTerms(terms []traceTerm) {
	if t == nil {
		return
	}
	for _, term := range terms {
		t.addTerm(term)
	}
}

func (t *searchTrace) addDecline(reason string) {
	if t == nil || reason == "" {
		return
	}
	decline := traceDecline{Reason: reason}
	if before, after, ok := strings.Cut(reason, ":"); ok {
		decline.Source = before
		decline.Reason = after
	}
	t.Declines = append(t.Declines, decline)
}

func (t *searchTrace) addDeclineForVolume(reason, volume string) {
	if t == nil || reason == "" {
		return
	}
	t.Decline = reason
	decline := traceDecline{Reason: reason, Volume: volume}
	if before, after, ok := strings.Cut(reason, ":"); ok {
		decline.Source = before
		decline.Reason = after
	}
	t.Declines = append(t.Declines, decline)
}

// sizeFilter expresses a size:<op><bytes> constraint, e.g. size:>100mb.
type sizeFilter struct {
	op    string // ">", ">=", "<", "<=", "="
	bytes int64
}

// dateFilter expresses a dm:<spec> constraint over modification time. The
// constraint is satisfied when ModUnix falls within [after, before).
type dateFilter struct {
	after  time.Time
	before time.Time
}

type CompactRecord struct {
	FRN       uint64
	ParentFRN uint64
	Parent    int32
	Name      string
	NameOff   uint32
	NameLen   uint16
	Mode      uint32
	Size      int64
	ModUnix   int64
	Deleted   bool
}

type PackedRecords struct {
	FRNs            []uint64
	ParentFRNExtras map[int]uint64
	Parents         []int32
	NameOffs        []uint32
	NameLens        []uint16
	LowerOffs       []uint32
	DirBits         []uint64
	ModeExtraIDs    []uint32
	ModeExtraValues []uint32
	Size32          []uint32
	Size64IDs       []uint32
	Size64Values    []int64
	ModUnix         []int64
	DeletedBits     []uint64
	NameBlob        []byte
	LowerBlob       []byte
}

type MMapRecords struct {
	file       *mappedIndexFile
	wideRefs   bool
	count      int
	nameBlob   []byte
	tokenTable []byte
	recordData []byte
	hasSize    bool
	hasModUnix bool
}

type usnJournalDataV0 struct {
	UsnJournalID    uint64
	FirstUsn        int64
	NextUsn         int64
	LowestValidUsn  int64
	MaxUsn          int64
	MaximumSize     uint64
	AllocationDelta uint64
}

type mftEnumDataV0 struct {
	StartFileReferenceNumber uint64
	LowUsn                   int64
	HighUsn                  int64
}

type readUSNJournalDataV0 struct {
	StartUsn          int64
	ReasonMask        uint32
	ReturnOnlyOnClose uint32
	Timeout           uint64
	BytesToWaitFor    uint64
	UsnJournalID      uint64
}

type usnNode struct {
	frn       uint64
	parentFRN uint64
	name      string
	attr      uint32
}

type usnChange struct {
	FRN       uint64
	ParentFRN uint64
	USN       int64
	Reason    uint32
	Attr      uint32
	Name      string
}

type walBatch struct {
	NextUSN int64       `json:"next_usn"`
	Changes []usnChange `json:"changes"`
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		if wantsJSON(os.Args[1:]) {
			_ = json.NewEncoder(os.Stderr).Encode(jsonError{OK: false, Error: err.Error()})
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func wantsJSON(args []string) bool {
	for _, arg := range args {
		if arg == "--json" || arg == "-json" {
			return true
		}
	}
	return false
}

func run(args []string) error {
	if len(args) == 0 {
		return cmdDefault(args)
	}
	switch args[0] {
	case "index":
		return cmdIndex(args[1:])
	case "index-usn":
		return cmdIndexUSN(args[1:])
	case "index-volumes":
		return cmdIndexVolumes(args[1:])
	case "upgrade-index":
		return cmdUpgradeIndex(args[1:])
	case "compact-index":
		return cmdCompactIndex(args[1:])
	case "augment-pngc":
		return cmdAugmentPNGC(args[1:])
	case "direct-v9":
		return cmdDirectV9(args[1:])
	case "service":
		return cmdService(args[1:])
	case "install":
		return cmdInstallService(args[1:])
	case "setup-service":
		return cmdSetupService(args[1:])
	case "launch":
		return cmdLaunch(args[1:])
	case "start":
		return cmdControlService(args[1:], "start")
	case "stop":
		return cmdControlService(args[1:], "stop")
	case "restart":
		return cmdControlService(args[1:], "restart")
	case "status":
		return cmdDoctor(args[1:])
	case "config":
		return cmdConfig(args[1:])
	case "defaults":
		return cmdDefaults(args[1:])
	case "uninstall":
		return cmdUninstallService(args[1:])
	case "doctor":
		return cmdDoctor(args[1:])
	case "service-index-usn":
		return cmdServiceIndexUSN(args[1:])
	case "loaded", "service-info":
		return cmdServiceInfo(args[1:])
	case "bench":
		return cmdBenchAgent(args[1:])
	case "ui":
		return cmdUI(args[1:])
	case "info":
		return cmdInfo(args[1:])
	case "search":
		return cmdSearch(args[1:], false)
	case "count":
		return cmdSearch(args[1:], true)
	case "version":
		fmt.Printf("seekfs %s commit=%s date=%s\n", version, commit, date)
		return nil
	case "help", "-h", "--help":
		printUsage(os.Stdout)
		return nil
	case "agent":
		printAgentHelp()
		return nil
	case "syntax":
		printSearchHelp()
		return nil
	default:
		if looksLikeSearchWithoutSubcommand(args) {
			return cmdSearch(normalizeSearchArgs(args), false)
		}
		return usage()
	}
}

func looksLikeSearchWithoutSubcommand(args []string) bool {
	for _, arg := range args {
		if arg == "--under" || strings.HasPrefix(arg, "--under=") ||
			arg == "-db" || arg == "--db" || strings.HasPrefix(arg, "-db=") || strings.HasPrefix(arg, "--db=") ||
			arg == "-path" || arg == "--path" || arg == "--json" || arg == "-json" ||
			arg == "-n" || arg == "--n" || strings.HasPrefix(arg, "-n=") || strings.HasPrefix(arg, "--n=") ||
			arg == "-service" || arg == "--service" || arg == "-local" || arg == "--local" ||
			arg == "--exists" || arg == "-exists" || arg == "--cwd-bias" || arg == "-cwd-bias" ||
			arg == "-root-bias" || arg == "--root-bias" || strings.HasPrefix(arg, "-root-bias=") || strings.HasPrefix(arg, "--root-bias=") ||
			arg == "-recent" || arg == "--recent" || strings.HasPrefix(arg, "-recent=") || strings.HasPrefix(arg, "--recent=") ||
			arg == "-modified-after" || arg == "--modified-after" || strings.HasPrefix(arg, "-modified-after=") || strings.HasPrefix(arg, "--modified-after=") ||
			arg == "-case" || arg == "--case" {
			return true
		}
	}
	if len(args) > 1 && !strings.HasPrefix(args[0], "-") {
		return true
	}
	if len(args) == 1 && looksLikeImplicitFilenameGlob(args[0]) {
		return true
	}
	return false
}

func normalizeSearchArgs(args []string) []string {
	valueFlags := map[string]bool{
		"--under": true, "-db": true, "--db": true, "-n": true, "--n": true,
		"-config": true, "--config": true, "-pipe": true, "--pipe": true,
		"-root-bias": true, "--root-bias": true, "-recent": true, "--recent": true,
		"-modified-after": true, "--modified-after": true,
	}
	boolFlags := map[string]bool{
		"-path": true, "--path": true, "--json": true, "-json": true,
		"-service": true, "--service": true, "-local": true, "--local": true,
		"--exists": true, "-exists": true, "--cwd-bias": true, "-cwd-bias": true,
		"-case": true, "--case": true,
	}
	flags := make([]string, 0, len(args))
	query := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if valueFlags[arg] && i+1 < len(args) {
			flags = append(flags, arg, args[i+1])
			i++
			continue
		}
		if boolFlags[arg] || strings.Contains(arg, "=") && strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			continue
		}
		query = append(query, arg)
	}
	return append(flags, query...)
}

func usage() error {
	printUsage(os.Stderr)
	return errors.New("unknown or missing command")
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage:
  seekfs index -root <path> [-root <path>...] [-db seekfs.db]
  seekfs index-usn -volume C: [-db seekfs.db]
  seekfs index-volumes [-volume C:] [-volume F:] [-index-dir path] [-launch]
  seekfs upgrade-index -db seekfs.gsi
  seekfs compact-index -db seekfs.gsi
  seekfs augment-pngc -db source.gsi -out target.gsi [-max-output-growth bytes] [-max-heap bytes] [-min-free-disk bytes]
  seekfs direct-v9 -out target.gsi (-root path | -records N) [-spool-dir path] [-run-records N] [-run-bytes bytes] [--json]
  seekfs launch [-db index.gsi...] [--json]
  seekfs install [-pipe \\.\pipe\seekfs-service] [-sddl <sddl>] [-db index.gsi...]
  seekfs setup-service [-pipe \\.\pipe\seekfs-service] [-sddl <sddl>] [-db index.gsi...] [-no-start]
  seekfs start|stop|restart
  seekfs uninstall
  seekfs doctor [--json]
  seekfs status [--json]
  seekfs loaded [--json]
  seekfs defaults [--json]
  seekfs config path|show|get|set
  seekfs service-index-usn -volume C: -db seekfs.gsi [-pipe \\.\pipe\seekfs-service]
  seekfs bench [-db index.gsi...] [-service] [-count] [--json] [-iterations 100]
  seekfs ui [-pipe \\.\pipe\seekfs-service] [-n 200]
  seekfs info [-db seekfs.gsi] [--json]
  seekfs syntax
  seekfs agent
  seekfs search [-db seekfs.db...] [--json] [-n 100] [-path] <query>
  seekfs count [-db seekfs.db...] [--json] [-path] <query>
  seekfs version

Agent use:
  seekfs is for indexed file-name and path discovery, preferably through the
  resident service. It is not a content/symbol search tool; use rg for text
  matches. Use --under <repo> for repo-scoped file discovery and run
  seekfs agent for automation guidance.

Agent starting points:
  seekfs agent
  F:\git\seekfs\seekfs.exe agent
  seekfs config set output_format json
  seekfs config set default_limit 20
  seekfs search "gh.exe"
  seekfs --under F:\git\seekfs "main.go"
  seekfs search --under F:\git\seekfs "main.go"
  seekfs search -path "ext:go dir:cmd main"
  seekfs count -path "type:file ext:go"`)
}

func printSearchHelp() {
	fmt.Print(`seekfs search syntax

Supported today:
  plain text        Case-insensitive substring match against file name.
  multiple terms    Whitespace-separated terms are ANDed.
  -path             Match terms against the full path instead of just the name.
  -n <num>          Limit returned rows; default is agent-safe 100.
  --json            Emit machine-readable JSON.
  --under <path>    Only return results under a workspace/project.
  --exists          Verify result paths still exist on disk.
  --cwd-bias        Rank paths under the current directory first.
  --root-bias path  Rank paths under a specific root first.
  --recent 24h      Only return entries modified within a duration.
  --modified-after  Only return entries modified after RFC3339 or YYYY-MM-DD.
  --case            Case-sensitive matching.
  count             Print the number of matches instead of result paths.

Query filters:
  ext:go            Match exact extension without leading dot.
  dir:src           Match a directory/path segment substring.
  parent:src        Match entries whose immediate parent directory is src.
  glob:*.py         Match file name glob.
  regex:<pattern>   Match normalized full path regex.
  size:>100mb       Match file sizes with >, >=, <, <=, = and k/m/g/t units.
  dm:today          Match modification date macros, durations, or YYYY-MM-DD.
  attrib:H          Match file attributes; flags R,H,S,D,A are supported.
  sort:size         Rank results by file size ascending.
  sort:modified     Rank results by modification time, newest first.
  sort:extension    Rank results by extension, then name.
  sort:type         Rank directories before files, then name.
  sort:path         Rank results by full path.
  case:             Enable case-sensitive matching from the query.
  type:file         Only files.
  type:dir          Only directories.
  a|b               OR alternatives within a term, such as ext:png|jpg.
  !term, -term      Exclude a term or filter.

Examples:
  seekfs search "gh.exe"
  seekfs --under F:\git\seekfs "main.go"
  seekfs search -path "ext:go dir:cmd main"
  seekfs search -path --under F:\git\seekfs "type:file glob:*.md"
  seekfs search -path --exists --recent 24h "ext:go"
  seekfs search "ext:png|jpg"
  seekfs search "report !draft"
  seekfs search "size:>100mb"
  seekfs count "ext:log dm:today"
  seekfs count -path "type:dir docs"

Not implemented yet:
  quoted phrase parsing beyond the shell's normal argument grouping
  Directory recursive-size semantics
  ranking compatible with Everything

Performance notes:
  Prefer filename-only search when you know the file name or executable name.
  Example: use "gh.exe" without -path to find the GitHub CLI binary.
  Use -path only when the query includes directory/path terms, dir:, --under,
  regex over full paths, or when path context is required. Full-path broad
  searches can be much slower on very large indexes.
`)
}

func printAgentHelp() {
	fmt.Print(`seekfs agent help

Purpose:
  Agent-first indexed file search for local filesystems. Prefer service mode
  for low latency; it avoids loading large indexes on each CLI invocation.

Recommended commands:
  seekfs loaded --json
  seekfs config set output_format json
  seekfs config set default_limit 20
  seekfs search "gh.exe"
  seekfs --under F:\git\seekfs "main.go"
  seekfs search --under F:\git\seekfs "main.go"
  seekfs search -path "ext:go dir:cmd main"
  seekfs count -path "type:file ext:go"
  seekfs launch -db F:\seekfs_c.gsi -db F:\seekfs_f.gsi
  seekfs config set output_format json
  seekfs bench -service --json -iterations 100
  seekfs bench -service -count --json -iterations 100

JSON result shape:
  {
    "ok": true,
    "query": "ext:go main",
    "count": 1,
    "limit": 20,
    "results": [{
      "path": "F:\\repo\\cmd\\seekfs\\main.go",
      "name": "main.go",
      "volume": "F:",
      "is_dir": false,
      "size": 123,
      "modified": "2026-05-22T12:00:00Z",
      "index_source": "walk"
    }]
  }

Useful search controls:
  --json              Required for robust automation.
  -service            Query the installed resident service; default when no -db is passed.
  -local              Do not auto-query the resident service.
  -path               Match full paths, not just names. Use only when needed.
  -n 20               Keep result sets bounded.
  --under <path>      Constrain search to a workspace.
  --exists            Filter stale index entries.
  --cwd-bias          Prefer current repo paths.
  --root-bias <path>  Prefer a specific repo/root.

Query filters:
  ext:go, parent:src, dir:src, glob:*.py, regex:<pattern>, case:, type:file, type:dir

Agent usage rules:
  seekfs searches indexed file names and paths, not file contents or symbols.
  Use rg for text-content search, definitions, import references, and line matches.
  For repo-local discovery, pass --under <repo> instead of relying on global ranking:
    seekfs search --under F:\git\seekfs "main.go"
    seekfs search -path --under F:\git\seekfs "ext:go dir:cmd main"
  Do not use -path with only a directory name when you want to list a tree.
  Add a file term/filter too, or use --under with a filename query.
  If seekfs is not on PATH, try the installed/repo binary directly:
    F:\git\seekfs\seekfs.exe search "gh.exe"

Performance guidance:
  Start with filename-only search for exact names and executables:
    seekfs search "gh.exe"
  Add -path only for path-aware queries:
    seekfs search -path "ext:go dir:cmd main"

Config:
  seekfs reads seekfs.toml from the current directory or user config dir.
  Supported keys: dbs, db, db_paths, db_path, volumes, volume, service_pipe,
  default_limit, output_format.

Errors:
  With --json, errors are written to stderr as:
  {"ok":false,"error":"message"}
  and the process exits nonzero.
`)
}

func cmdIndex(args []string) error {
	var roots stringList
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	db := fs.String("db", defaultDB(), "index database path")
	fs.Var(&roots, "root", "root to index; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(roots) == 0 {
		return errors.New("index requires at least one -root")
	}

	start := time.Now()
	idx := &Index{Version: indexVersion, Roots: roots, BuiltAt: time.Now(), Source: "walk"}
	for _, root := range roots {
		if err := walkRoot(root, idx); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s: %v\n", root, err)
		}
	}
	buildOrders(idx)
	if err := saveIndex(*db, idx); err != nil {
		return err
	}
	fmt.Printf("indexed %d entries in %s\n", len(idx.Entries), time.Since(start).Round(time.Millisecond))
	return nil
}

func cmdIndexUSN(args []string) error {
	fs := flag.NewFlagSet("index-usn", flag.ContinueOnError)
	db := fs.String("db", defaultDB(), "index database path")
	volume := fs.String("volume", "C:", "NTFS volume, for example C:")
	if err := fs.Parse(args); err != nil {
		return err
	}
	start := time.Now()
	idx, err := indexUSNVolume(*volume)
	if err != nil {
		return err
	}
	buildOrders(idx)
	if err := saveIndex(*db, idx); err != nil {
		return err
	}
	fmt.Printf("indexed %d entries from %s via USN in %s\n", len(idx.Entries), *volume, time.Since(start).Round(time.Millisecond))
	return nil
}

func cmdUpgradeIndex(args []string) error {
	fs := flag.NewFlagSet("upgrade-index", flag.ContinueOnError)
	db := fs.String("db", "", "index database path to upgrade")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *db == "" {
		return errors.New("upgrade-index requires -db")
	}
	if !engineV9Enabled() {
		return errors.New("upgrade-index writes the gated v9 format only when SEEKFS_ENGINE_V9=1")
	}
	idx, err := loadIndex(*db)
	if err != nil {
		return err
	}
	ensureCompactIndexForService(idx)
	if !idx.Compact {
		return errors.New("upgrade-index requires a compact-capable index")
	}
	idx.Version = indexVersionV9
	if err := saveIndex(*db, idx); err != nil {
		return err
	}
	fmt.Printf("upgraded %s to v9 derived-section format\n", *db)
	return nil
}

func cmdCompactIndex(args []string) error {
	fs := flag.NewFlagSet("compact-index", flag.ContinueOnError)
	db := fs.String("db", "", "index database path whose WAL overlay should be compacted")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *db == "" {
		return errors.New("compact-index requires -db")
	}
	if !engineV9Enabled() {
		return errors.New("compact-index writes the gated v9 format only when SEEKFS_ENGINE_V9=1")
	}
	idx, err := loadIndexForService(*db)
	if err != nil {
		return err
	}
	// Compaction only needs FRN and child topology to replay the WAL.  Building
	// the full resident query index here duplicates the expensive writer view
	// and was the dominant peak-memory source for large v8 inputs.
	vol := newCompactionVolumeIndex(*db, idx)
	if err := vol.replayWAL(); err != nil {
		return err
	}
	if err := compactOverlayToDisk(vol); err != nil {
		return err
	}
	if err := removeWAL(*db); err != nil {
		return err
	}
	fmt.Printf("compacted WAL overlay into %s\n", *db)
	return nil
}

// newCompactionVolumeIndex constructs only the state required to apply a WAL
// and produce a compacted index.  It deliberately does not build resident
// postings, path caches, or packed records; the v9 writer builds derived data
// once, from the compacted output.
func newCompactionVolumeIndex(dbPath string, idx *Index) *serviceVolumeIndex {
	vol := &serviceVolumeIndex{
		dbPath:      dbPath,
		index:       idx,
		volume:      idx.Volume,
		journalID:   idx.JournalID,
		checkpoint:  idx.Checkpoint,
		state:       "ready",
		pathCache:   make(map[int]string),
		lastPersist: time.Now(),
	}
	if idx.Compact && idx.Source == "usn" {
		recordCount := idx.compactRecordCount()
		if len(idx.Derived.FRNs) == recordCount && len(idx.Derived.FRNRecordIDs) == recordCount {
			vol.frns = idx.Derived.FRNs
			vol.frnRecordIDs = idx.Derived.FRNRecordIDs
		} else {
			vol.frns = make([]uint64, 0, recordCount)
			vol.frnRecordIDs = make([]uint32, 0, recordCount)
			for id := 0; id < recordCount; id++ {
				rec := idx.compactRecord(id)
				if rec.FRN != 0 {
					vol.frns = append(vol.frns, rec.FRN)
					vol.frnRecordIDs = append(vol.frnRecordIDs, uint32(id))
				}
			}
			sortFRNIndexEntries(vol.frns, vol.frnRecordIDs)
		}
		vol.childOffsets = idx.Derived.ChildOffsets
		vol.childIDs = idx.Derived.ChildIDs
		vol.rootIDs = idx.Derived.RootIDs
		vol.subtreeOrder = idx.Derived.SubtreeOrder
		vol.subtreeStart = idx.Derived.SubtreeStart
		vol.subtreeEnd = idx.Derived.SubtreeEnd
		if len(vol.childOffsets) == 0 || len(vol.childIDs) == 0 {
			// The fallback subtree walk only needs the packed child graph.  Do
			// not build the optional DFS interval arrays during WAL replay.
			old := os.Getenv("SEEKFS_SUBTREE_INTERVALS")
			_ = os.Setenv("SEEKFS_SUBTREE_INTERVALS", "0")
			vol.buildCompactChildren()
			if old == "" {
				_ = os.Unsetenv("SEEKFS_SUBTREE_INTERVALS")
			} else {
				_ = os.Setenv("SEEKFS_SUBTREE_INTERVALS", old)
			}
		}
	}
	if engineV9Enabled() {
		vol.overlay = newOverlaySegment()
		vol.publishSnapshot()
	}
	return vol
}

func cmdIndexVolumes(args []string) error {
	var volumes stringList
	fs := flag.NewFlagSet("index-volumes", flag.ContinueOnError)
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	indexDir := fs.String("index-dir", defaultIndexDir(), "directory for generated .gsi files")
	launch := fs.Bool("launch", false, "launch resident service with the built indexes")
	dryRun := fs.Bool("dry-run", false, "show planned index paths without indexing")
	jsonOut := fs.Bool("json", false, "write machine-readable JSON")
	fs.Var(&volumes, "volume", "NTFS volume to index; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if len(volumes) == 0 && len(cfg.Volumes) > 0 {
		volumes = append(volumes, cfg.Volumes...)
	}
	if len(volumes) == 0 {
		volumes = defaultIndexVolumes()
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	if cfg.OutputFormat == "json" {
		*jsonOut = true
	}
	if !*dryRun {
		if _, err := callService(*pipeName, serviceRequest{Command: "status"}); err != nil {
			if setupErr := cmdSetupService([]string{"-pipe", *pipeName, "-no-start"}); setupErr != nil {
				return fmt.Errorf("service unavailable and setup failed: %w", setupErr)
			}
			if startErr := startWindowsService(); startErr != nil {
				return fmt.Errorf("service unavailable and start failed: %w", startErr)
			}
		}
	}
	type result struct {
		Volume  string `json:"volume"`
		DB      string `json:"db"`
		Entries int    `json:"entries"`
		Error   string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(volumes))
	dbs := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		vol := normalizeVolume(volume)
		db := defaultVolumeDB(*indexDir, vol)
		if *dryRun {
			results = append(results, result{Volume: vol, DB: db})
			dbs = append(dbs, db)
			continue
		}
		if err := os.MkdirAll(*indexDir, 0o755); err != nil {
			return err
		}
		resp, err := callService(*pipeName, serviceRequest{Command: "index-usn", Volume: vol, DB: db})
		r := result{Volume: vol, DB: db}
		if err != nil {
			r.Error = err.Error()
			results = append(results, r)
			continue
		}
		if !resp.OK {
			r.Error = resp.Message
			results = append(results, r)
			continue
		}
		r.Entries = resp.Entries
		results = append(results, r)
		dbs = append(dbs, db)
	}
	if *launch && len(dbs) > 0 && !*dryRun {
		launchArgs := []string{"-pipe", *pipeName}
		for _, db := range dbs {
			launchArgs = append(launchArgs, "-db", db)
		}
		if err := cmdLaunch(launchArgs); err != nil {
			return err
		}
	}
	if *jsonOut {
		return writeJSON(os.Stdout, struct {
			OK      bool     `json:"ok"`
			Results []result `json:"results"`
		}{OK: len(dbs) == len(volumes), Results: results})
	}
	for _, r := range results {
		if r.Error != "" {
			fmt.Printf("%s -> %s error=%s\n", r.Volume, r.DB, r.Error)
		} else {
			fmt.Printf("%s -> %s entries=%d\n", r.Volume, r.DB, r.Entries)
		}
	}
	if len(dbs) != len(volumes) {
		return errors.New("one or more volumes failed to index")
	}
	return nil
}

func indexUSNVolume(volume string) (*Index, error) {
	vol := normalizeVolume(volume)
	handle, err := openVolume(vol)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(handle)

	journal, err := queryUSNJournal(handle)
	if err != nil {
		return nil, fmt.Errorf("query USN journal for %s: %w; run elevated or use a service helper for raw volume access", vol, err)
	}

	idx := &Index{
		Version:    indexVersion,
		Roots:      []string{vol + `\`},
		BuiltAt:    time.Now(),
		Source:     "usn",
		Volume:     vol,
		JournalID:  journal.UsnJournalID,
		Checkpoint: journal.NextUsn,
	}
	idx.Compact = true
	idx.CompactAttrs = true

	// Prefer the MFT for the initial build: it carries name, parent, size, and
	// modification time in a single bulk read (the same source Everything uses).
	// Fall back to FSCTL_ENUM_USN_DATA (names only) if the raw MFT read fails.
	if entries, err := enumMFT(handle); err == nil && len(entries) > 0 {
		serviceLog("enum-mft complete volume=%s entries=%d", vol, len(entries))
		if nodes, usnErr := enumUSN(handle, journal.NextUsn); usnErr == nil {
			if added := mergeUSNNodesIntoMFT(entries, nodes); added > 0 {
				serviceLog("enum-usn merge complete volume=%s added=%d total=%d", vol, added, len(entries))
			}
		} else {
			serviceLog("enum-usn merge skipped volume=%s err=%v", vol, usnErr)
		}
		buildRecordsFromMFT(idx, entries)
		serviceLog("compact records complete volume=%s entries=%d source=mft", vol, len(idx.Records))
		return idx, nil
	} else if err != nil {
		serviceLog("enum-mft failed volume=%s err=%v; falling back to USN enum", vol, err)
	}

	nodes, err := enumUSN(handle, journal.NextUsn)
	if err != nil {
		return nil, err
	}
	serviceLog("enum-usn complete volume=%s nodes=%d next_usn=%d", vol, len(nodes), journal.NextUsn)

	frns := make([]uint64, 0, len(nodes))
	for frn := range nodes {
		frns = append(frns, frn)
	}
	sort.Slice(frns, func(i, j int) bool { return frns[i] < frns[j] })
	frnToIndex := make(map[uint64]int, len(frns))
	for i, frn := range frns {
		frnToIndex[frn] = i
	}
	idx.Records = make([]CompactRecord, 0, len(frns))
	for _, frn := range frns {
		node := nodes[frn]
		parent := int32(-1)
		if p, ok := frnToIndex[node.parentFRN]; ok && p != frnToIndex[frn] {
			parent = int32(p)
		}
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       frn,
			ParentFRN: node.parentFRN,
			Parent:    parent,
			Name:      node.name,
			Mode:      modeFromAttrs(node.attr),
		})
	}
	serviceLog("compact records complete volume=%s entries=%d source=usn", vol, len(idx.Records))
	return idx, nil
}

func mergeUSNNodesIntoMFT(entries map[uint64]mftEntry, nodes map[uint64]usnNode) int {
	if len(entries) == 0 || len(nodes) == 0 {
		return 0
	}
	added := 0
	for frn, node := range nodes {
		if frn == 0 || node.name == "" {
			continue
		}
		if _, ok := entries[frn]; ok {
			continue
		}
		entries[frn] = mftEntry{
			frn:       frn,
			parentFRN: node.parentFRN,
			name:      node.name,
			attr:      node.attr,
			isDir:     node.attr&fileAttributeDir != 0,
			inUse:     true,
		}
		added++
	}
	return added
}

// buildRecordsFromMFT converts MFT entries into compact records with stable
// parent indexes, populating size and modification time.
func buildRecordsFromMFT(idx *Index, entries map[uint64]mftEntry) {
	idx.CompactAttrs = true
	frns := make([]uint64, 0, len(entries))
	for frn := range entries {
		frns = append(frns, frn)
	}
	sort.Slice(frns, func(i, j int) bool { return frns[i] < frns[j] })
	frnToIndex := make(map[uint64]int, len(frns))
	for i, frn := range frns {
		frnToIndex[frn] = i
	}
	idx.Records = make([]CompactRecord, 0, len(frns))
	for _, frn := range frns {
		e := entries[frn]
		parent := int32(-1)
		if p, ok := frnToIndex[e.parentFRN]; ok && p != frnToIndex[frn] {
			parent = int32(p)
		}
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       frn,
			ParentFRN: e.parentFRN,
			Parent:    parent,
			Name:      e.name,
			Mode:      modeFromAttrs(e.attr),
			Size:      e.size,
			ModUnix:   e.modUnix,
		})
	}
}

func normalizeVolume(volume string) string {
	volume = strings.TrimSpace(volume)
	volume = strings.TrimRight(volume, `\`)
	if len(volume) == 1 && ((volume[0] >= 'A' && volume[0] <= 'Z') || (volume[0] >= 'a' && volume[0] <= 'z')) {
		volume += ":"
	}
	return strings.ToUpper(volume[:1]) + volume[1:]
}

func openVolume(volume string) (windows.Handle, error) {
	path := `\\.\` + strings.TrimRight(volume, `\`)
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(
		ptr,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
}

func enumUSN(handle windows.Handle, highUSN int64) (map[uint64]usnNode, error) {
	nodes := make(map[uint64]usnNode, 1<<20)
	enumData := mftEnumDataV0{LowUsn: 0, HighUsn: highUSN}
	inSize := uint32(unsafe.Sizeof(enumData))
	buffer := make([]byte, 4*1024*1024)
	for {
		var bytesReturned uint32
		err := windows.DeviceIoControl(
			handle,
			fsctlEnumUSNData,
			(*byte)(unsafe.Pointer(&enumData)),
			inSize,
			&buffer[0],
			uint32(len(buffer)),
			&bytesReturned,
			nil,
		)
		if err != nil {
			if err == windows.ERROR_HANDLE_EOF {
				break
			}
			return nil, fmt.Errorf("enumerate USN data: %w", err)
		}
		if bytesReturned <= 8 {
			break
		}
		enumData.StartFileReferenceNumber = binary.LittleEndian.Uint64(buffer[:8])
		pos := uint32(8)
		for pos+60 <= bytesReturned {
			record := buffer[pos:bytesReturned]
			recordLen := binary.LittleEndian.Uint32(record[0:4])
			if recordLen < 60 || pos+recordLen > bytesReturned {
				break
			}
			major := binary.LittleEndian.Uint16(record[4:6])
			if major == 2 || major == 3 {
				frn, parent, attr, nameLen, nameOff, ok := parseUSNRecordFields(record, major)
				if !ok {
					pos += recordLen
					continue
				}
				if uint32(nameOff)+uint32(nameLen) <= recordLen {
					nameBytes := record[nameOff : uint32(nameOff)+uint32(nameLen)]
					name := windows.UTF16ToString(bytesToUTF16(nameBytes))
					if name != "" {
						nodes[frn] = usnNode{frn: frn, parentFRN: parent, name: name, attr: attr}
					}
				}
			}
			pos += recordLen
		}
	}
	return nodes, nil
}

func readUSNChanges(handle windows.Handle, journalID uint64, startUSN int64, buffer []byte) (int64, []usnChange, error) {
	return readUSNChangesWait(handle, journalID, startUSN, buffer, 0, 0)
}

func readUSNChangesWait(handle windows.Handle, journalID uint64, startUSN int64, buffer []byte, timeout time.Duration, bytesToWaitFor uint64) (int64, []usnChange, error) {
	if len(buffer) < 4096 {
		buffer = make([]byte, 4096)
	}
	req := makeReadUSNJournalRequest(journalID, startUSN, timeout, bytesToWaitFor)
	var bytesReturned uint32
	err := windows.DeviceIoControl(
		handle,
		fsctlReadUSNJournal,
		(*byte)(unsafe.Pointer(&req)),
		uint32(unsafe.Sizeof(req)),
		&buffer[0],
		uint32(len(buffer)),
		&bytesReturned,
		nil,
	)
	if err != nil {
		if err == windows.ERROR_HANDLE_EOF {
			return startUSN, nil, nil
		}
		return startUSN, nil, err
	}
	return parseUSNChangeBuffer(buffer[:bytesReturned])
}

func makeReadUSNJournalRequest(journalID uint64, startUSN int64, timeout time.Duration, bytesToWaitFor uint64) readUSNJournalDataV0 {
	timeoutSeconds := uint64(0)
	if timeout > 0 {
		timeoutSeconds = uint64(timeout.Round(time.Second) / time.Second)
		if timeoutSeconds == 0 {
			timeoutSeconds = 1
		}
	}
	return readUSNJournalDataV0{
		StartUsn:          startUSN,
		ReasonMask:        0xffffffff,
		ReturnOnlyOnClose: 0,
		Timeout:           timeoutSeconds,
		BytesToWaitFor:    bytesToWaitFor,
		UsnJournalID:      journalID,
	}
}

func parseUSNChangeBuffer(buffer []byte) (int64, []usnChange, error) {
	if len(buffer) < 8 {
		return 0, nil, errors.New("USN change buffer too small")
	}
	nextUSN := int64(binary.LittleEndian.Uint64(buffer[:8]))
	changes, err := parseUSNRecords(buffer[8:])
	return nextUSN, changes, err
}

func parseUSNRecords(buffer []byte) ([]usnChange, error) {
	changes := make([]usnChange, 0, 128)
	for pos := uint32(0); pos < uint32(len(buffer)); {
		if uint32(len(buffer))-pos < 60 {
			break
		}
		record := buffer[pos:]
		recordLen := binary.LittleEndian.Uint32(record[0:4])
		if recordLen < 60 || pos+recordLen > uint32(len(buffer)) {
			return nil, errors.New("invalid USN record length")
		}
		major := binary.LittleEndian.Uint16(record[4:6])
		if major == 2 || major == 3 {
			frn, parent, attr, nameLen, nameOff, ok := parseUSNRecordFields(record, major)
			if !ok {
				pos += recordLen
				continue
			}
			if uint32(nameOff)+uint32(nameLen) > recordLen {
				return nil, errors.New("invalid USN record name")
			}
			nameBytes := record[nameOff : uint32(nameOff)+uint32(nameLen)]
			changes = append(changes, usnChange{
				FRN:       frn,
				ParentFRN: parent,
				USN:       parseUSNRecordUSN(record, major),
				Reason:    parseUSNRecordReason(record, major),
				Attr:      attr,
				Name:      windows.UTF16ToString(bytesToUTF16(nameBytes)),
			})
		}
		pos += recordLen
	}
	return changes, nil
}

func parseUSNRecordFields(record []byte, major uint16) (frn, parent uint64, attr uint32, nameLen, nameOff uint16, ok bool) {
	switch major {
	case 2:
		if len(record) < 60 {
			return 0, 0, 0, 0, 0, false
		}
		return fileReferenceRecordNumber(binary.LittleEndian.Uint64(record[8:16])),
			fileReferenceRecordNumber(binary.LittleEndian.Uint64(record[16:24])),
			binary.LittleEndian.Uint32(record[52:56]),
			binary.LittleEndian.Uint16(record[56:58]),
			binary.LittleEndian.Uint16(record[58:60]),
			true
	case 3:
		if len(record) < 76 {
			return 0, 0, 0, 0, 0, false
		}
		return fileReferenceRecordNumber(binary.LittleEndian.Uint64(record[8:16])),
			fileReferenceRecordNumber(binary.LittleEndian.Uint64(record[24:32])),
			binary.LittleEndian.Uint32(record[68:72]),
			binary.LittleEndian.Uint16(record[72:74]),
			binary.LittleEndian.Uint16(record[74:76]),
			true
	default:
		return 0, 0, 0, 0, 0, false
	}
}

func parseUSNRecordUSN(record []byte, major uint16) int64 {
	if major == 3 {
		return int64(binary.LittleEndian.Uint64(record[40:48]))
	}
	return int64(binary.LittleEndian.Uint64(record[24:32]))
}

func parseUSNRecordReason(record []byte, major uint16) uint32 {
	if major == 3 {
		return binary.LittleEndian.Uint32(record[56:60])
	}
	return binary.LittleEndian.Uint32(record[40:44])
}

func fileReferenceRecordNumber(ref uint64) uint64 {
	return ref & 0x0000FFFFFFFFFFFF
}

func bytesToUTF16(b []byte) []uint16 {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return u
}

func buildUSNPath(frn uint64, nodes map[uint64]usnNode, cache map[uint64]string, volume string) string {
	if path, ok := cache[frn]; ok {
		return path
	}
	chain := make([]uint64, 0, 16)
	seen := make(map[uint64]struct{}, 16)
	cur := frn
	var prefix string
	for depth := 0; depth < 1024; depth++ {
		if path, ok := cache[cur]; ok {
			prefix = path
			break
		}
		node, ok := nodes[cur]
		if !ok {
			break
		}
		if _, ok := seen[cur]; ok {
			return ""
		}
		seen[cur] = struct{}{}
		chain = append(chain, cur)
		if node.parentFRN == cur || node.parentFRN == 0 {
			prefix = volume + `\` + node.name
			cache[cur] = prefix
			chain = chain[:len(chain)-1]
			break
		}
		cur = node.parentFRN
	}
	if prefix == "" {
		if len(chain) == 0 {
			return ""
		}
		root := chain[len(chain)-1]
		node := nodes[root]
		prefix = volume + `\` + node.name
		cache[root] = prefix
		chain = chain[:len(chain)-1]
	}
	for i := len(chain) - 1; i >= 0; i-- {
		node := nodes[chain[i]]
		prefix += `\` + node.name
		cache[chain[i]] = prefix
	}
	return cache[frn]
}

func modeFromAttrs(attr uint32) uint32 {
	mode := attr
	if attr&fileAttributeDir != 0 {
		mode |= uint32(os.ModeDir)
	}
	return mode
}

func walkRoot(root string, idx *Index) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	return filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		name := d.Name()
		idx.Entries = append(idx.Entries, Entry{
			Path:      path,
			Name:      name,
			LowerPath: strings.ToLower(path),
			LowerName: strings.ToLower(name),
			Size:      info.Size(),
			Mode:      uint32(info.Mode()),
			ModUnix:   info.ModTime().UnixNano(),
		})
		return nil
	})
}

func buildOrders(idx *Index) {
	if idx.Compact {
		idx.CompactNameOrder = make([]int, len(idx.Records))
		for i := range idx.Records {
			idx.CompactNameOrder[i] = i
		}
		sort.Slice(idx.CompactNameOrder, func(i, j int) bool {
			a, b := idx.Records[idx.CompactNameOrder[i]], idx.Records[idx.CompactNameOrder[j]]
			aName, bName := compactLowerName(a), compactLowerName(b)
			if aName == bName {
				return idx.CompactNameOrder[i] < idx.CompactNameOrder[j]
			}
			return aName < bName
		})
		return
	}
	idx.NameOrder = make([]int, len(idx.Entries))
	idx.PathOrder = make([]int, len(idx.Entries))
	for i := range idx.Entries {
		idx.NameOrder[i] = i
		idx.PathOrder[i] = i
	}
	sort.Slice(idx.NameOrder, func(i, j int) bool {
		a, b := idx.Entries[idx.NameOrder[i]], idx.Entries[idx.NameOrder[j]]
		if a.LowerName == b.LowerName {
			return a.LowerPath < b.LowerPath
		}
		return a.LowerName < b.LowerName
	})
	sort.Slice(idx.PathOrder, func(i, j int) bool {
		return idx.Entries[idx.PathOrder[i]].LowerPath < idx.Entries[idx.PathOrder[j]].LowerPath
	})
}

func ensureCompactNameOrderSorted(idx *Index) {
	if idx == nil || !idx.Compact {
		return
	}
	if len(idx.CompactNameOrder) != len(idx.Records) {
		idx.CompactNameOrder = make([]int, len(idx.Records))
		for i := range idx.CompactNameOrder {
			idx.CompactNameOrder[i] = i
		}
	}
	sort.Slice(idx.CompactNameOrder, func(i, j int) bool {
		a, b := idx.Records[idx.CompactNameOrder[i]], idx.Records[idx.CompactNameOrder[j]]
		aName, bName := compactLowerName(a), compactLowerName(b)
		if aName == bName {
			return idx.CompactNameOrder[i] < idx.CompactNameOrder[j]
		}
		return aName < bName
	})
}

func cmdSearch(args []string, countOnly bool) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	var dbs stringList
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	fs.Var(&dbs, "db", "index database path; repeatable")
	useService := fs.Bool("service", false, "query the installed seekfs service over its named pipe")
	forceLocal := fs.Bool("local", false, "do not auto-query the resident service")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	jsonOut := fs.Bool("json", false, "write machine-readable JSON")
	limit := fs.Int("n", 100, "maximum results")
	matchPath := fs.Bool("path", false, "match full path")
	under := fs.String("under", "", "only return results under this path")
	exists := fs.Bool("exists", false, "verify result paths still exist")
	cwdBias := fs.Bool("cwd-bias", false, "rank paths under the current working directory first")
	rootBias := fs.String("root-bias", "", "rank paths under this root first")
	recent := fs.String("recent", "", "only return results modified within this duration, for example 24h")
	modifiedAfter := fs.String("modified-after", "", "only return results modified after RFC3339 time or YYYY-MM-DD")
	caseSensitive := fs.Bool("case", false, "case-sensitive query matching")
	if err := fs.Parse(normalizeSearchArgs(args)); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if len(dbs) == 0 && len(cfg.DBs) > 0 {
		dbs = append(dbs, cfg.DBs...)
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	if cfg.OutputFormat == "json" {
		*jsonOut = true
	}
	if *limit == 100 && cfg.DefaultLimit > 0 {
		*limit = cfg.DefaultLimit
	}
	if len(dbs) == 0 && !*forceLocal {
		*useService = true
	}
	queryArgs := append([]string(nil), fs.Args()...)
	if *matchPath && *under == "" {
		queryArgs, *under = extractUnderPathArg(queryArgs)
	}
	query := strings.TrimSpace(strings.Join(queryArgs, " "))
	if query == "" {
		return errors.New("query required")
	}
	opts := queryOptions{
		Query:         query,
		MatchPath:     *matchPath || queryLooksLoosePathScoped(query),
		Limit:         *limit,
		Under:         *under,
		Exists:        *exists,
		RootBias:      *rootBias,
		Recent:        *recent,
		ModifiedAfter: *modifiedAfter,
		CaseSensitive: *caseSensitive,
	}
	if *cwdBias {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		opts.CWDBias = cwd
	}
	if *useService {
		return searchService(*pipeName, opts, countOnly, *jsonOut)
	}
	if len(dbs) == 0 {
		dbs = append(dbs, defaultDB())
	}
	indexes, err := loadIndexes(dbs)
	if err != nil {
		return err
	}
	matches, err := searchAll(indexes, opts, countOnly)
	if err != nil {
		return err
	}
	if *jsonOut {
		resp := jsonSearchResponse{
			OK:       true,
			Query:    query,
			Count:    len(matches),
			Limit:    *limit,
			Complete: boolPtr(true),
		}
		if !countOnly {
			resp.Results = entriesToJSON(matches)
		}
		return writeJSON(os.Stdout, resp)
	}
	if countOnly {
		fmt.Println(len(matches))
		return nil
	}
	w := bufio.NewWriter(os.Stdout)
	for _, entry := range matches {
		fmt.Fprintln(w, entry.Path)
	}
	return w.Flush()
}

func cmdInfo(args []string) error {
	fs := flag.NewFlagSet("info", flag.ContinueOnError)
	db := fs.String("db", defaultDB(), "index database path")
	jsonOut := fs.Bool("json", false, "write machine-readable JSON")
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *db == defaultDB() && len(cfg.DBs) > 0 {
		*db = cfg.DBs[0]
	}
	if cfg.OutputFormat == "json" {
		*jsonOut = true
	}
	idx, err := loadIndex(*db)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeJSON(os.Stdout, indexInfoToJSON(idx, *db))
	}
	fmt.Printf("version: %d\n", idx.Version)
	fmt.Printf("source: %s\n", idx.Source)
	fmt.Printf("built_at: %s\n", idx.BuiltAt.Format(time.RFC3339Nano))
	fmt.Printf("entries: %d\n", idx.entryCount())
	fmt.Printf("roots: %s\n", strings.Join(idx.Roots, "; "))
	if idx.Volume != "" {
		fmt.Printf("volume: %s\n", idx.Volume)
		fmt.Printf("journal_id: %d\n", idx.JournalID)
		fmt.Printf("checkpoint_usn: %d\n", idx.Checkpoint)
	}
	if idx.ContentHash != "" {
		fmt.Printf("content_hash: %s\n", idx.ContentHash)
	}
	if layout := estimateIndexLayout(idx, *db); layout != nil {
		fmt.Printf("file_bytes: %d\n", layout.FileBytes)
		fmt.Printf("record_bytes: %d\n", layout.RecordBytes)
		fmt.Printf("name_blob_bytes: %d\n", layout.NameBlobBytes)
		fmt.Printf("name_table_bytes: %d\n", layout.NameTableBytes)
		fmt.Printf("bytes_per_record: %.2f\n", layout.BytesPerRecord)
	}
	return nil
}

func extractUnderPathArg(args []string) ([]string, string) {
	for i, arg := range args {
		under := ""
		if isDriveToken(arg) {
			under = strings.ToUpper(arg[:1]) + `:\`
		} else if filepath.IsAbs(arg) {
			under = filepath.Clean(arg)
		}
		if under == "" {
			continue
		}
		rest := append([]string{}, args[:i]...)
		rest = append(rest, args[i+1:]...)
		return rest, under
	}
	return args, ""
}

func isDriveToken(arg string) bool {
	return len(arg) == 2 && arg[1] == ':' && ((arg[0] >= 'A' && arg[0] <= 'Z') || (arg[0] >= 'a' && arg[0] <= 'z'))
}

func (idx *Index) entryCount() int {
	if idx.Compact {
		if idx.MMapRecords != nil {
			return idx.MMapRecords.Len()
		}
		if idx.PackedRecords != nil {
			return idx.PackedRecords.Len()
		}
		return len(idx.Records)
	}
	return len(idx.Entries)
}

func cmdBenchAgent(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	var dbs stringList
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	fs.Var(&dbs, "db", "index database path; repeatable")
	useService := fs.Bool("service", false, "query the installed seekfs service")
	countOnly := fs.Bool("count", false, "benchmark count-only service queries")
	useResident := fs.Bool("resident", false, "query resident service-volume indexes in-process")
	residentWait := fs.Duration("resident-wait", 2*time.Minute, "max time to wait for resident background indexes")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	jsonOut := fs.Bool("json", false, "write machine-readable JSON")
	iterations := fs.Int("iterations", 100, "number of benchmark iterations")
	warmup := fs.Int("warmup", 0, "untimed warmup iterations")
	limit := fs.Int("n", 20, "maximum results per query")
	queryFile := fs.String("query-file", "", "file containing benchmark queries, one per line")
	matchPath := fs.Bool("path", false, "match full path and file name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if len(dbs) == 0 && len(cfg.DBs) > 0 {
		dbs = append(dbs, cfg.DBs...)
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	if cfg.OutputFormat == "json" {
		*jsonOut = true
	}
	queries := fs.Args()
	if *queryFile != "" {
		fileQueries, err := readBenchQueries(*queryFile)
		if err != nil {
			return err
		}
		queries = append(queries, fileQueries...)
	}
	if len(queries) == 0 {
		queries = []string{"ext:go", "glob:*.md", "type:dir docs", "README", "main"}
	}
	if *iterations <= 0 {
		return errors.New("iterations must be positive")
	}
	if *warmup < 0 {
		return errors.New("warmup must be non-negative")
	}
	if *countOnly && !*useService {
		return errors.New("count benchmarking requires -service")
	}
	var indexes []*Index
	var residentVolumes []*serviceVolumeIndex
	if !*useService {
		if len(dbs) == 0 {
			dbs = append(dbs, defaultDB())
		}
		if *useResident {
			indexes, residentVolumes, _, err = loadConfiguredVolumes(dbs)
			if err != nil {
				return err
			}
			_ = indexes
			svc := &goSearchService{}
			svc.startBackgroundNameOrderBuilds(residentVolumes)
			svc.startBackgroundNameTrigramBuilds(residentVolumes)
			waitResidentBackgroundIndexes(residentVolumes, *residentWait)
		} else {
			indexes, err = loadIndexes(dbs)
			if err != nil {
				return err
			}
		}
	}
	timings := make([]float64, 0, *iterations)
	backendTimings := make([]float64, 0, *iterations)
	queryTimings := make(map[string][]float64, len(queries))
	queryBackendTimings := make(map[string][]float64, len(queries))
	sourceCounts := make(map[string]int)
	querySourceCounts := make(map[string]map[string]int, len(queries))
	declineCounts := make(map[string]int)
	queryDeclineCounts := make(map[string]map[string]int, len(queries))
	candidateCounts := make([]float64, 0, *iterations)
	queryCandidateCounts := make(map[string][]float64, len(queries))
	queryFailures := make(map[string]int, len(queries))
	queryResultHashes := make(map[string]string, len(queries))
	queryResultCounts := make(map[string]int, len(queries))
	queryResultConsistent := make(map[string]bool, len(queries))
	queryDiagnostics := make(map[string]benchDiagnostics, len(queries))
	queryDiagnosticsConsistent := make(map[string]bool, len(queries))
	queryResponseSeen := make(map[string]bool, len(queries))
	failures := 0
	for i := 0; i < *warmup; i++ {
		query := queries[i%len(queries)]
		opts := queryOptions{Query: query, MatchPath: *matchPath, Limit: *limit}
		if err := runBenchQuery(*useService, *useResident, *countOnly, *pipeName, indexes, residentVolumes, opts, nil); err != nil {
			return fmt.Errorf("warmup query %q failed: %w", query, err)
		}
	}
	for i := 0; i < *iterations; i++ {
		query := queries[i%len(queries)]
		opts := queryOptions{Query: query, MatchPath: *matchPath, Limit: *limit}
		start := time.Now()
		if *useService {
			var resp serviceResponse
			resp, err = benchServiceRequest(*pipeName, opts, *countOnly)
			if err == nil {
				resultHash := benchResultHash(resp, *countOnly)
				diagnostics := benchDiagnosticsFromResponse(resp)
				if !queryResponseSeen[query] {
					queryResultHashes[query] = resultHash
					queryResultCounts[query] = resp.Count
					queryResultConsistent[query] = true
					queryDiagnostics[query] = diagnostics
					queryDiagnosticsConsistent[query] = true
					queryResponseSeen[query] = true
				} else {
					if queryResultHashes[query] != resultHash || queryResultCounts[query] != resp.Count {
						queryResultConsistent[query] = false
					}
					if !sameBenchDiagnostics(queryDiagnostics[query], diagnostics) {
						queryDiagnosticsConsistent[query] = false
					}
				}
				backendTimings = append(backendTimings, resp.SearchMS)
				queryBackendTimings[query] = append(queryBackendTimings[query], resp.SearchMS)
				recordBenchSource(sourceCounts, querySourceCounts, query, resp.Source)
				recordBenchOptional(declineCounts, queryDeclineCounts, query, resp.Decline)
				candidateCounts = append(candidateCounts, float64(resp.Candidates))
				queryCandidateCounts[query] = append(queryCandidateCounts[query], float64(resp.Candidates))
			}
		} else if *useResident {
			trace := &searchTrace{}
			opts.Trace = trace
			_, err = searchServiceVolumes(residentVolumes, opts, false)
			if err == nil {
				recordBenchSource(sourceCounts, querySourceCounts, query, trace.Source)
				recordBenchOptional(declineCounts, queryDeclineCounts, query, trace.Decline)
				candidateCounts = append(candidateCounts, float64(trace.Candidates))
				queryCandidateCounts[query] = append(queryCandidateCounts[query], float64(trace.Candidates))
			}
		} else {
			_, err = searchAll(indexes, opts, false)
		}
		elapsed := float64(time.Since(start).Nanoseconds()) / 1_000_000
		timings = append(timings, elapsed)
		queryTimings[query] = append(queryTimings[query], elapsed)
		if err != nil {
			failures++
			queryFailures[query]++
		}
	}
	perQuery := make([]benchQuerySummary, 0, len(queries))
	seenQueries := make(map[string]struct{}, len(queries))
	for _, query := range queries {
		if _, seen := seenQueries[query]; seen {
			continue
		}
		seenQueries[query] = struct{}{}
		item := benchQuerySummary{
			Query:                 query,
			Iterations:            len(queryTimings[query]),
			Failures:              queryFailures[query],
			Stats:                 latencyStats(queryTimings[query]),
			Backend:               latencyStats(queryBackendTimings[query]),
			Sources:               querySourceCounts[query],
			Declines:              queryDeclineCounts[query],
			Candidates:            latencyStats(queryCandidateCounts[query]),
			ResultHash:            queryResultHashes[query],
			ResultCount:           queryResultCounts[query],
			ResultConsistent:      queryResponseSeen[query] && queryResultConsistent[query],
			DiagnosticsConsistent: queryResponseSeen[query] && queryDiagnosticsConsistent[query],
		}
		if queryResponseSeen[query] {
			diagnostics := queryDiagnostics[query]
			item.Diagnostics = &diagnostics
		}
		perQuery = append(perQuery, item)
		if len(queryBackendTimings[query]) == 0 {
			perQuery[len(perQuery)-1].Backend = nil
		}
		if len(querySourceCounts[query]) == 0 {
			perQuery[len(perQuery)-1].Sources = nil
		}
		if len(queryDeclineCounts[query]) == 0 {
			perQuery[len(perQuery)-1].Declines = nil
		}
		if len(queryCandidateCounts[query]) == 0 {
			perQuery[len(perQuery)-1].Candidates = nil
		}
	}
	summary := benchSummary{
		OK:         failures == 0,
		Mode:       benchModeName(*useService, *useResident, *countOnly),
		Iterations: *iterations,
		Failures:   failures,
		Queries:    len(queries),
		Stats:      latencyStats(timings),
		Backend:    latencyStats(backendTimings),
		Sources:    sourceCounts,
		Declines:   declineCounts,
		Candidates: latencyStats(candidateCounts),
		PerQuery:   perQuery,
	}
	if len(backendTimings) == 0 {
		summary.Backend = nil
	}
	if len(sourceCounts) == 0 {
		summary.Sources = nil
	}
	if len(declineCounts) == 0 {
		summary.Declines = nil
	}
	if len(candidateCounts) == 0 {
		summary.Candidates = nil
	}
	if *jsonOut {
		return writeJSON(os.Stdout, summary)
	}
	fmt.Printf("mode: %s\niterations: %d\nqueries: %d\nfailures: %d\n", summary.Mode, summary.Iterations, summary.Queries, summary.Failures)
	for _, key := range []string{"min", "median", "p90", "p95", "p99", "max"} {
		fmt.Printf("%s_ms: %.3f\n", key, summary.Stats[key])
	}
	for _, item := range summary.PerQuery {
		if item.Backend != nil {
			fmt.Printf("query=%q iterations=%d failures=%d median_ms=%.3f backend_median_ms=%.3f p90_ms=%.3f p99_ms=%.3f max_ms=%.3f candidates_median=%.0f sources=%v\n", item.Query, item.Iterations, item.Failures, item.Stats["median"], item.Backend["median"], item.Stats["p90"], item.Stats["p99"], item.Stats["max"], item.Candidates["median"], item.Sources)
		} else {
			fmt.Printf("query=%q iterations=%d failures=%d median_ms=%.3f p90_ms=%.3f p99_ms=%.3f max_ms=%.3f candidates_median=%.0f sources=%v\n", item.Query, item.Iterations, item.Failures, item.Stats["median"], item.Stats["p90"], item.Stats["p99"], item.Stats["max"], item.Candidates["median"], item.Sources)
		}
	}
	return nil
}

func benchResultHash(resp serviceResponse, countOnly bool) string {
	h := sha256.New()
	if countOnly {
		fmt.Fprintf(h, "count:%d\n", resp.Count)
	} else {
		for _, path := range resp.Results {
			h.Write([]byte(path))
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func benchDiagnosticsFromResponse(resp serviceResponse) benchDiagnostics {
	complete := ""
	if resp.Complete != nil {
		complete = strconv.FormatBool(*resp.Complete)
	}
	return benchDiagnostics{
		Source:          resp.Source,
		Driver:          benchResponseDriver(resp),
		Candidates:      resp.Candidates,
		RecordsVerified: resp.RecordsVerified + resp.ComponentRecordsVerified + resp.FilenameRecordsVerified,
		BlocksDecoded:   resp.BlocksDecoded,
		BlocksSkipped:   resp.BlocksSkipped,
		Complete:        complete,
	}
}

func benchResponseDriver(resp serviceResponse) string {
	if resp.FilenameDriver != "" {
		return resp.FilenameDriver
	}
	if resp.ComponentDriver != "" {
		return resp.ComponentDriver
	}
	if resp.ScalarDriver != "" {
		return resp.ScalarDriver
	}
	return resp.PlannerMode
}

func sameBenchDiagnostics(a, b benchDiagnostics) bool {
	return a == b
}

func readBenchQueries(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

func benchModeName(useService, useResident, countOnly bool) string {
	if useService {
		if countOnly {
			return "service-count"
		}
		return "service"
	}
	if useResident {
		return "resident-local"
	}
	return "local"
}

func recordBenchSource(global map[string]int, perQuery map[string]map[string]int, query string, source string) {
	if source == "" {
		source = "unknown"
	}
	global[source]++
	if perQuery[query] == nil {
		perQuery[query] = make(map[string]int)
	}
	perQuery[query][source]++
}

func recordBenchOptional(global map[string]int, perQuery map[string]map[string]int, query string, value string) {
	if value == "" {
		return
	}
	global[value]++
	if perQuery[query] == nil {
		perQuery[query] = make(map[string]int)
	}
	perQuery[query][value]++
}

func runBenchQuery(useService bool, useResident bool, countOnly bool, pipeName string, indexes []*Index, volumes []*serviceVolumeIndex, opts queryOptions, trace *searchTrace) error {
	if useService {
		_, err := benchServiceRequest(pipeName, opts, countOnly)
		return err
	}
	if useResident {
		opts.Trace = trace
		_, err := searchServiceVolumes(volumes, opts, false)
		return err
	}
	_, err := searchAll(indexes, opts, false)
	return err
}

func waitResidentBackgroundIndexes(volumes []*serviceVolumeIndex, maxWait time.Duration) {
	if maxWait <= 0 || len(volumes) == 0 {
		return
	}
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		allDone := true
		for _, vol := range volumes {
			if vol == nil {
				continue
			}
			nameOrderState := vol.nameOrderStateString()
			nameTrigramState := vol.nameTrigramStateString()
			if nameOrderState == "pending" || nameOrderState == "building" ||
				nameTrigramState == "pending" || nameTrigramState == "building" {
				allDone = false
				break
			}
		}
		if allDone {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func benchServiceQuery(pipeName string, opts queryOptions) (serviceResponse, error) {
	return benchServiceRequest(pipeName, opts, false)
}

func benchServiceRequest(pipeName string, opts queryOptions, countOnly bool) (serviceResponse, error) {
	resp, err := callService(pipeName, serviceRequestFromOptions(opts, countOnly))
	if err != nil {
		return serviceResponse{}, err
	}
	if !resp.OK {
		return resp, errors.New(resp.Message)
	}
	return resp, nil
}

func latencyStats(values []float64) map[string]float64 {
	stats := map[string]float64{"min": 0, "median": 0, "p90": 0, "p95": 0, "p99": 0, "max": 0}
	if len(values) == 0 {
		return stats
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	stats["min"] = sorted[0]
	stats["median"] = percentile(sorted, 0.50)
	stats["p90"] = percentile(sorted, 0.90)
	stats["p95"] = percentile(sorted, 0.95)
	stats["p99"] = percentile(sorted, 0.99)
	stats["max"] = sorted[len(sorted)-1]
	return stats
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

type serviceRequest struct {
	Command       string `json:"command"`
	Volume        string `json:"volume,omitempty"`
	DB            string `json:"db,omitempty"`
	Query         string `json:"query,omitempty"`
	MatchPath     bool   `json:"match_path,omitempty"`
	Limit         int    `json:"limit,omitempty"`
	CountOnly     bool   `json:"count_only,omitempty"`
	Under         string `json:"under,omitempty"`
	Exists        bool   `json:"exists,omitempty"`
	CWDBias       string `json:"cwd_bias,omitempty"`
	RootBias      string `json:"root_bias,omitempty"`
	Recent        string `json:"recent,omitempty"`
	ModifiedAfter string `json:"modified_after,omitempty"`
	CaseSensitive bool   `json:"case_sensitive,omitempty"`
	DeadlineUnix  int64  `json:"deadline_unix,omitempty"`
	RequestSeq    int64  `json:"request_seq,omitempty"`
}

type serviceResponse struct {
	OK                       bool               `json:"ok"`
	Message                  string             `json:"message,omitempty"`
	PID                      int                `json:"pid,omitempty"`
	Executable               string             `json:"executable,omitempty"`
	ExecutableHash           string             `json:"executable_hash,omitempty"`
	Version                  string             `json:"version,omitempty"`
	Commit                   string             `json:"commit,omitempty"`
	Date                     string             `json:"date,omitempty"`
	BuildFlavor              string             `json:"build_flavor,omitempty"`
	PipeName                 string             `json:"pipe_name,omitempty"`
	ProcessMode              string             `json:"process_mode,omitempty"`
	Entries                  int                `json:"entries,omitempty"`
	Loading                  bool               `json:"loading,omitempty"`
	Count                    int                `json:"count,omitempty"`
	SearchMS                 float64            `json:"search_ms,omitempty"`
	Source                   string             `json:"source,omitempty"`
	Decline                  string             `json:"decline,omitempty"`
	Candidates               int                `json:"candidates,omitempty"`
	PlannerMode              string             `json:"planner_mode,omitempty"`
	EligibleVolumes          []string           `json:"eligible_volumes,omitempty"`
	BlocksDecoded            int                `json:"blocks_decoded,omitempty"`
	BlocksSkipped            int                `json:"blocks_skipped,omitempty"`
	ScalarDriver             string             `json:"scalar_driver,omitempty"`
	ScalarInterval           int                `json:"scalar_interval,omitempty"`
	RecordsVerified          int                `json:"records_verified,omitempty"`
	ComponentDriver          string             `json:"component_driver,omitempty"`
	ComponentRoots           int                `json:"component_roots,omitempty"`
	ComponentIntervals       int                `json:"component_intervals,omitempty"`
	ComponentCardinality     int                `json:"component_cardinality,omitempty"`
	ComponentSelfHits        int                `json:"component_self_hits,omitempty"`
	ComponentBounds          string             `json:"component_bounds,omitempty"`
	ComponentRecordsVerified int                `json:"component_records_verified,omitempty"`
	FilenameDriver           string             `json:"filename_driver,omitempty"`
	FilenameRequiredGrams    int                `json:"filename_required_grams,omitempty"`
	FilenamePostingHint      int                `json:"filename_posting_hint,omitempty"`
	FilenameRecordsVerified  int                `json:"filename_records_verified,omitempty"`
	OverlayBaseWindow        int                `json:"overlay_base_window,omitempty"`
	PostingPrefetchBytes     int                `json:"posting_prefetch_bytes,omitempty"`
	PostingPrefetchRanges    int                `json:"posting_prefetch_ranges,omitempty"`
	PostingPrefetchPages     int                `json:"posting_prefetch_pages,omitempty"`
	Terms                    []traceTerm        `json:"terms,omitempty"`
	Declines                 []traceDecline     `json:"declines,omitempty"`
	Fallback                 string             `json:"fallback,omitempty"`
	Complete                 *bool              `json:"complete,omitempty"`
	Results                  []string           `json:"results,omitempty"`
	Rows                     []jsonResult       `json:"rows,omitempty"`
	DBs                      []dbInfo           `json:"dbs,omitempty"`
	Runtime                  *runtimeMemoryInfo `json:"runtime,omitempty"`
}

func serviceInfoResponse(resp serviceResponse) serviceResponse {
	return serviceInfoResponseFor(resp, "", "")
}

func serviceInfoResponseFor(resp serviceResponse, pipeName, processMode string) serviceResponse {
	resp.PID = os.Getpid()
	resp.Version = version
	resp.Commit = commit
	resp.Date = date
	resp.BuildFlavor = serviceBuildFlavor()
	resp.PipeName = pipeName
	resp.ProcessMode = processMode
	if exe, err := os.Executable(); err == nil {
		resp.Executable = exe
		resp.ExecutableHash = executableContentHash(exe)
	}
	return resp
}

func executableContentHash(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func serviceBuildFlavor() string {
	return serviceBuildFlavorForMemoryMode(os.Getenv("SEEKFS_MEMORY_MODE"))
}

func serviceBuildFlavorForMemoryMode(memoryMode string) string {
	parts := []string{"cli", "service"}
	switch strings.ToLower(strings.TrimSpace(memoryMode)) {
	case "lowmem", "mmap", "low-memory":
		parts = append(parts, "lowmem")
	}
	return strings.Join(parts, ",")
}

type dbInfo struct {
	Path              string              `json:"path"`
	Entries           int                 `json:"entries"`
	Source            string              `json:"source"`
	BuiltAt           string              `json:"built_at"`
	Volume            string              `json:"volume,omitempty"`
	JournalID         uint64              `json:"journal_id,omitempty"`
	Checkpoint        int64               `json:"checkpoint_usn,omitempty"`
	State             string              `json:"state,omitempty"`
	StaleReason       string              `json:"stale_reason,omitempty"`
	FRNRecords        int                 `json:"frn_records,omitempty"`
	Recent            int                 `json:"recent,omitempty"`
	PathCache         int                 `json:"path_cache,omitempty"`
	TermCache         int                 `json:"term_cache,omitempty"`
	PathTerms         int                 `json:"path_term_cache,omitempty"`
	ExtCache          int                 `json:"ext_cache,omitempty"`
	RecentSeq         uint64              `json:"recent_seq,omitempty"`
	Dirty             bool                `json:"dirty,omitempty"`
	LastPersist       string              `json:"last_persist,omitempty"`
	PersistFailures   int                 `json:"persist_failures,omitempty"`
	PersistRetryAfter string              `json:"persist_retry_after,omitempty"`
	LastPersistError  string              `json:"last_persist_error,omitempty"`
	QueryExtKeys      int                 `json:"query_ext_keys,omitempty"`
	QueryDirs         int                 `json:"query_dirs,omitempty"`
	NameOrderState    string              `json:"name_order_state,omitempty"`
	NameOrderMillis   int64               `json:"name_order_build_ms,omitempty"`
	NameTrigramState  string              `json:"name_trigram_state,omitempty"`
	NameTrigramMillis int64               `json:"name_trigram_build_ms,omitempty"`
	DerivedSections   []string            `json:"derived_sections,omitempty"`
	DerivedBytes      int                 `json:"derived_bytes,omitempty"`
	Memory            *residentMemoryInfo `json:"memory,omitempty"`
}

type residentMemoryInfo struct {
	Records           int   `json:"records"`
	MMapRecordBytes   int64 `json:"mmap_record_bytes,omitempty"`
	NameBlobBytes     int   `json:"name_blob_bytes,omitempty"`
	LowerBlobBytes    int   `json:"lower_blob_bytes,omitempty"`
	RecordBytes       int64 `json:"record_bytes,omitempty"`
	NameOrderBytes    int   `json:"name_order_bytes,omitempty"`
	ExtPostBytes      int   `json:"ext_posting_bytes,omitempty"`
	NameTrigramBytes  int   `json:"name_trigram_bytes,omitempty"`
	NameTrigramKeys   int   `json:"name_trigram_keys,omitempty"`
	TypePostBytes     int   `json:"type_posting_bytes,omitempty"`
	ChildBytes        int   `json:"child_bytes,omitempty"`
	FRNIndexBytes     int   `json:"frn_index_bytes,omitempty"`
	FRNOverlayEntries int   `json:"frn_overlay_entries,omitempty"`
	KnownBytes        int64 `json:"known_bytes,omitempty"`
}

type runtimeMemoryInfo struct {
	HeapAllocBytes    uint64  `json:"heap_alloc_bytes"`
	HeapInuseBytes    uint64  `json:"heap_inuse_bytes"`
	HeapIdleBytes     uint64  `json:"heap_idle_bytes"`
	HeapReleasedBytes uint64  `json:"heap_released_bytes"`
	HeapSysBytes      uint64  `json:"heap_sys_bytes"`
	StackInuseBytes   uint64  `json:"stack_inuse_bytes"`
	SysBytes          uint64  `json:"sys_bytes"`
	NumGC             uint32  `json:"num_gc"`
	GCCPUFraction     float64 `json:"gc_cpu_fraction,omitempty"`
}

type goSearchService struct {
	pipeName    string
	sddl        string
	processMode string
	stop        chan struct{}
	dbs         []string
	indexes     []*Index
	volumes     []*serviceVolumeIndex
	loading     bool
	loadErr     string
	indexMu     sync.RWMutex
	requestSeq  atomic.Int64
}

type serviceVolumeIndex struct {
	dbPath            string
	index             *Index
	volume            string
	journalID         uint64
	checkpoint        int64
	state             string
	staleReason       string
	frnToID           map[uint64]int
	frns              []uint64
	frnRecordIDs      []uint32
	children          map[uint64]map[int]struct{}
	childOffsets      []uint32
	childIDs          []uint32
	rootIDs           []uint32
	subtreeOrder      []uint32
	subtreeStart      []uint32
	subtreeEnd        []uint32
	subtreeSizeRank   []uint32
	subtreeModRank    []uint32
	subtreeExtRank    []uint32
	subtreeTypeRank   []uint32
	subtreePathRank   []uint32
	exactNames        map[string][]int
	pathCache         map[int]string
	queryIndex        *residentQueryIndex
	nameOrderState    atomic.Int32
	nameOrderMillis   atomic.Int64
	nameTrigrams      atomic.Pointer[compressedTrigramIndex]
	nameQuadgrams     atomic.Pointer[compressedTrigramIndex]
	nameTrigramState  atomic.Int32
	nameTrigramMillis atomic.Int64
	searchMu          sync.Mutex
	termMu            sync.Mutex
	walkMu            sync.Mutex
	termCache         map[string]postingCacheEntry
	pathTermCache     map[string]postingCacheEntry
	extCache          map[string]postingCacheEntry
	recentIDs         map[int]struct{}
	nameTrigramRecent map[int]struct{}
	recentSeq         uint64
	underCache        map[int]postingCacheEntry
	underRootCache    map[string]postingCacheEntry
	dirty             bool
	lastPersist       time.Time
	persistFailures   int
	persistRetryAfter time.Time
	lastPersistErr    string
	searchCount       uint64
	overlay           *overlaySegment
	snap              atomic.Pointer[volumeSnapshot]
	snapshotGen       atomic.Uint64
}

type overlaySegment struct {
	records   []CompactRecord
	byFRN     map[uint64]int32
	tombstone overlayBaseIDSet
	shadowed  overlayBaseIDSet
	watermark atomic.Int32
}

type overlayBaseIDSet struct {
	bits  []uint64
	ids   []int32
	count int
}

type volumeSnapshot struct {
	base         *Index
	records      []CompactRecord
	tombstoneIDs []int32
	shadowedIDs  []int32
	watermark    int32
	gen          uint64
}

type residentQueryIndex struct {
	ext        map[string][]uint32
	extTop     map[string][]uint32
	pathGrams  map[string][]uint32
	components map[string][]uint32
	attrBits   map[uint32][]uint32
	nameOrder  []uint32
	nameRank   []uint32
	sizeOrder  []uint32
	sizeRank   []uint32
	modOrder   []uint32
	modRank    []uint32
	extOrder   []uint32
	extRank    []uint32
	typeOrder  []uint32
	typeRank   []uint32
	pathOrder  []uint32
	pathRank   []uint32
	dirs       []uint32
	dirsReady  bool
}

type postingCacheEntry struct {
	ids []int
	gen uint64
}

func serviceLog(format string, args ...any) {
	dir := filepath.Join(os.Getenv("ProgramData"), "seekfs")
	if dir == "" || dir == "seekfs" {
		dir = filepath.Join(os.TempDir(), "seekfs")
	}
	_ = os.MkdirAll(dir, 0o755)
	f, err := os.OpenFile(filepath.Join(dir, "service.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, time.Now().Format(time.RFC3339Nano)+" "+format+"\n", args...)
}

func cmdService(args []string) error {
	var dbs stringList
	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	sddl := fs.String("sddl", defaultServiceSDDL, "pipe security descriptor SDDL")
	lowMemory := fs.Bool("lowmem", false, "run service in low-memory mmap mode")
	skipStartupSync := fs.Bool("skip-startup-sync", false, "deprecated no-op; startup replay and catch-up always run")
	fs.Var(&dbs, "db", "index database path to load for service search; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *lowMemory {
		_ = os.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	}
	_ = *skipStartupSync
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if len(dbs) == 0 && len(cfg.DBs) > 0 {
		dbs = append(dbs, cfg.DBs...)
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	isService, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	processMode := "standalone"
	if isService {
		processMode = "windows-service"
	}
	handler := &goSearchService{pipeName: *pipeName, sddl: *sddl, processMode: processMode, stop: make(chan struct{}), dbs: dbs}
	if isService {
		return svc.Run(serviceName, handler)
	}
	return handler.runStandalone()
}

func cmdInstallService(args []string) error {
	var dbs stringList
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	sddl := fs.String("sddl", defaultServiceSDDL, "pipe security descriptor SDDL")
	fs.Var(&dbs, "db", "index database path to load for service search; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if len(dbs) == 0 && len(cfg.DBs) > 0 {
		dbs = append(dbs, cfg.DBs...)
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err == nil {
		s.Close()
		return fmt.Errorf("service %s already exists", serviceName)
	}
	serviceArgs := []string{"service", "-pipe", *pipeName, "-sddl", *sddl}
	for _, db := range dbs {
		serviceArgs = append(serviceArgs, "-db", db)
	}
	s, err = m.CreateService(serviceName, exe, mgr.Config{
		DisplayName: "seekfs indexing service",
		StartType:   mgr.StartManual,
	}, serviceArgs...)
	if err != nil {
		return err
	}
	defer s.Close()
	fmt.Println("installed service", serviceName)
	return nil
}

func cmdSetupService(args []string) error {
	var dbs stringList
	fs := flag.NewFlagSet("setup-service", flag.ContinueOnError)
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	sddl := fs.String("sddl", defaultServiceSDDL, "pipe security descriptor SDDL")
	noStart := fs.Bool("no-start", false, "install but do not start the service")
	fs.Var(&dbs, "db", "index database path to load for service search; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if len(dbs) == 0 && len(cfg.DBs) > 0 {
		dbs = append(dbs, cfg.DBs...)
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	if err := stopServiceIfExists(); err != nil {
		return err
	}
	if err := deleteServiceIfExists(); err != nil {
		return err
	}
	installArgs := []string{"-pipe", *pipeName, "-sddl", *sddl}
	for _, db := range dbs {
		installArgs = append(installArgs, "-db", db)
	}
	if err := cmdInstallService(installArgs); err != nil {
		return err
	}
	if !*noStart {
		if err := startWindowsService(); err != nil {
			return err
		}
		fmt.Println("started service", serviceName)
	}
	return nil
}

func cmdLaunch(args []string) error {
	var dbs stringList
	fs := flag.NewFlagSet("launch", flag.ContinueOnError)
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	sddl := fs.String("sddl", defaultServiceSDDL, "pipe security descriptor SDDL")
	jsonOut := fs.Bool("json", false, "write machine-readable JSON")
	timeout := fs.Duration("timeout", 3*time.Minute, "maximum time to wait for service health")
	fs.Var(&dbs, "db", "index database path to load for service search; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if len(dbs) == 0 && len(cfg.DBs) > 0 {
		dbs = append(dbs, cfg.DBs...)
	}
	if len(dbs) == 0 {
		return errors.New("launch requires at least one -db or configured dbs")
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	if cfg.OutputFormat == "json" {
		*jsonOut = true
	}
	setupArgs := []string{"-pipe", *pipeName, "-sddl", *sddl}
	for _, db := range dbs {
		setupArgs = append(setupArgs, "-db", db)
	}
	if err := cmdSetupService(setupArgs); err != nil {
		return err
	}
	resp := waitForDoctor(*pipeName, *timeout)
	if *jsonOut {
		if err := writeJSON(os.Stdout, resp); err != nil {
			return err
		}
		if !resp.OK {
			return errors.New(resp.Message)
		}
		return nil
	}
	fmt.Printf("installed: %t\nrunning: %t\npipe_reachable: %t\nentries: %d\nquery_ok: %t\n", resp.Installed, resp.Running, resp.PipeReachable, resp.Entries, resp.QueryOK)
	if !resp.OK {
		return errors.New(resp.Message)
	}
	return nil
}

func cmdControlService(args []string, action string) error {
	fs := flag.NewFlagSet(action+"-service", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch action {
	case "start":
		return startWindowsService()
	case "stop":
		return stopWindowsService()
	case "restart":
		if err := stopServiceIfExists(); err != nil {
			return err
		}
		return startWindowsService()
	default:
		return errors.New("unknown service action")
	}
}

func cmdUninstallService(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Delete(); err != nil {
		return err
	}
	fmt.Println("uninstalled service", serviceName)
	return nil
}

func startWindowsService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Start()
}

func stopWindowsService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	defer s.Close()
	status, err := s.Control(svc.Stop)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(15 * time.Second)
	for status.State != svc.Stopped && time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		status, err = s.Query()
		if err != nil {
			return err
		}
	}
	return nil
}

func stopServiceIfExists() error {
	err := stopWindowsService()
	if err == nil || strings.Contains(strings.ToLower(err.Error()), "does not exist") {
		return nil
	}
	return nil
}

func deleteServiceIfExists() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return nil
	}
	defer s.Close()
	return s.Delete()
}

func cmdServiceIndexUSN(args []string) error {
	fs := flag.NewFlagSet("service-index-usn", flag.ContinueOnError)
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	db := fs.String("db", defaultDB(), "index database path")
	volume := fs.String("volume", "C:", "NTFS volume")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	req := serviceRequest{Command: "index-usn", Volume: *volume, DB: *db}
	resp, err := callService(*pipeName, req)
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Message)
	}
	fmt.Printf("%s %d entries\n", resp.Message, resp.Entries)
	return nil
}

func cmdServiceSimple(args []string, command string) error {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	volume := fs.String("volume", "C:", "NTFS volume")
	jsonOut := fs.Bool("json", false, "write machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	if cfg.OutputFormat == "json" {
		*jsonOut = true
	}
	resp, err := callService(*pipeName, serviceRequest{Command: command, Volume: *volume})
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Message)
	}
	if *jsonOut {
		return writeJSON(os.Stdout, resp)
	}
	fmt.Println(resp.Message)
	return nil
}

func cmdServiceInfo(args []string) error {
	fs := flag.NewFlagSet("loaded", flag.ContinueOnError)
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	jsonOut := fs.Bool("json", false, "write machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	if cfg.OutputFormat == "json" {
		*jsonOut = true
	}
	resp, err := callService(*pipeName, serviceRequest{Command: "info"})
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Message)
	}
	if *jsonOut {
		return writeJSON(os.Stdout, resp)
	}
	fmt.Printf("entries: %d\n", resp.Entries)
	for _, db := range resp.DBs {
		fmt.Printf("%s entries=%d source=%s volume=%s built_at=%s checkpoint=%d\n", db.Path, db.Entries, db.Source, db.Volume, db.BuiltAt, db.Checkpoint)
	}
	return nil
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	jsonOut := fs.Bool("json", false, "write machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	if cfg.OutputFormat == "json" {
		*jsonOut = true
	}
	resp := probeDoctor(*pipeName)
	if *jsonOut {
		if err := writeJSON(os.Stdout, resp); err != nil {
			return err
		}
		if !resp.OK {
			return errors.New(resp.Message)
		}
		return nil
	}
	fmt.Printf("installed: %t\nrunning: %t\npipe_reachable: %t\nentries: %d\nquery_ok: %t\n", resp.Installed, resp.Running, resp.PipeReachable, resp.Entries, resp.QueryOK)
	if !resp.OK {
		return errors.New(resp.Message)
	}
	return nil
}

func cmdDefaults(args []string) error {
	fs := flag.NewFlagSet("defaults", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "write machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	type dbDefault struct {
		Volume string `json:"volume"`
		DB     string `json:"db"`
		Exists bool   `json:"exists"`
	}
	indexDir := defaultIndexDir()
	volumes := defaultIndexVolumes()
	dbs := make([]dbDefault, 0, len(volumes))
	for _, volume := range volumes {
		db := defaultVolumeDB(indexDir, normalizeVolume(volume))
		_, err := os.Stat(db)
		dbs = append(dbs, dbDefault{Volume: normalizeVolume(volume), DB: db, Exists: err == nil})
	}
	resp := struct {
		OK          bool        `json:"ok"`
		ConfigPath  string      `json:"config_path"`
		IndexDir    string      `json:"index_dir"`
		ServicePipe string      `json:"service_pipe"`
		Volumes     []string    `json:"volumes"`
		DBs         []dbDefault `json:"dbs"`
	}{OK: true, ConfigPath: defaultConfigPath(), IndexDir: indexDir, ServicePipe: defaultServicePipe, Volumes: volumes, DBs: dbs}
	cfg, _ := loadConfig("")
	if cfg.OutputFormat == "json" {
		*jsonOut = true
	}
	if *jsonOut {
		return writeJSON(os.Stdout, resp)
	}
	fmt.Printf("config: %s\nindex_dir: %s\nservice_pipe: %s\n", resp.ConfigPath, resp.IndexDir, resp.ServicePipe)
	for _, db := range dbs {
		fmt.Printf("%s -> %s exists=%t\n", db.Volume, db.DB, db.Exists)
	}
	return nil
}

func cmdConfig(args []string) error {
	if len(args) == 0 {
		return cmdConfig([]string{"show"})
	}
	switch args[0] {
	case "path":
		fmt.Println(defaultConfigPath())
		return nil
	case "show":
		path := defaultConfigPath()
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Print(string(data))
		return nil
	case "get":
		if len(args) < 2 {
			return errors.New("config get requires a key")
		}
		cfg, err := loadConfig(defaultConfigPath())
		if err != nil {
			return err
		}
		return printConfigKey(cfg, args[1])
	case "set":
		if len(args) < 3 {
			return errors.New(`config set requires a key and value, for example: seekfs config set output_format json`)
		}
		key := args[1]
		value := strings.TrimSpace(strings.Join(args[2:], " "))
		value = strings.TrimSpace(strings.TrimPrefix(value, "="))
		return setConfigKey(defaultConfigPath(), key, value)
	default:
		return errors.New("unknown config command; use path, show, get, or set")
	}
}

func printConfigKey(cfg appConfig, key string) error {
	switch key {
	case "dbs", "db_paths":
		fmt.Println(formatStringArray(cfg.DBs))
	case "volumes":
		fmt.Println(formatStringArray(cfg.Volumes))
	case "service_pipe":
		fmt.Println(cfg.ServicePipe)
	case "default_limit":
		fmt.Println(cfg.DefaultLimit)
	case "output_format":
		fmt.Println(cfg.OutputFormat)
	default:
		return fmt.Errorf("unknown config key %q", key)
	}
	return nil
}

func setConfigKey(path, key, value string) error {
	allowed := map[string]bool{"dbs": true, "db_paths": true, "volumes": true, "service_pipe": true, "default_limit": true, "output_format": true}
	if !allowed[key] {
		return fmt.Errorf("unknown config key %q", key)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lines := []string{}
	if data, err := os.ReadFile(path); err == nil {
		lines = strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	}
	formatted := formatConfigValue(key, value)
	replaced := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, key+" ") || strings.HasPrefix(trimmed, key+"=") {
			lines[i] = key + " = " + formatted
			replaced = true
		}
	}
	if !replaced {
		lines = append(lines, key+" = "+formatted)
	}
	out := strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
	return os.WriteFile(path, []byte(out), 0o644)
}

func formatConfigValue(key, value string) string {
	value = strings.TrimSpace(value)
	if key == "dbs" || key == "db_paths" || key == "volumes" {
		if strings.HasPrefix(value, "[") {
			return formatStringArray(parseTOMLStringArray(value))
		}
		parts := strings.Split(value, ",")
		items := make([]string, 0, len(parts))
		for _, part := range parts {
			part = strings.Trim(strings.TrimSpace(part), `"'`)
			if part != "" {
				items = append(items, strconvQuote(part))
			}
		}
		return "[" + strings.Join(items, ", ") + "]"
	}
	if key == "default_limit" {
		return value
	}
	return strconvQuote(strings.Trim(value, `"'`))
}

func formatStringArray(values []string) string {
	items := make([]string, len(values))
	for i, value := range values {
		items[i] = strconvQuote(value)
	}
	return "[" + strings.Join(items, ", ") + "]"
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func probeDoctor(pipeName string) doctorResponse {
	resp := doctorResponse{ServiceName: serviceName}
	if status, err := queryWindowsService(); err == nil {
		resp.Installed = true
		resp.Running = status.State == svc.Running
	} else {
		resp.ServiceError = err.Error()
		if installed, running := serviceStatusBySC(serviceName); installed {
			resp.Installed = true
			resp.Running = running
		}
	}
	info, err := callService(pipeName, serviceRequest{Command: "info"})
	if err == nil && info.OK {
		resp.PipeReachable = true
		resp.Entries = info.Entries
		resp.Loading = info.Loading
		resp.DBs = info.DBs
		resp.Runtime = info.Runtime
	}
	if err == nil && info.Loading {
		resp.PipeReachable = true
		resp.Loading = true
		resp.DBs = info.DBs
		resp.Runtime = info.Runtime
		resp.Message = "seekfs service is loading indexes"
	}
	searchResp, searchErr := callService(pipeName, serviceRequestFromOptions(queryOptions{Query: "ext:go", MatchPath: true, Limit: 1}, false))
	if searchErr == nil && searchResp.OK {
		resp.QueryOK = true
	}
	if resp.Running && resp.PipeReachable {
		resp.ServiceError = ""
	}
	resp.OK = resp.PipeReachable && resp.Entries > 0 && resp.QueryOK
	if !resp.OK && resp.Message == "" {
		if resp.PipeReachable && !resp.QueryOK {
			resp.Message = "seekfs service pipe is reachable but search is not healthy"
		} else if !resp.PipeReachable && resp.ServiceError != "" && strings.Contains(strings.ToLower(resp.ServiceError), "access is denied") {
			resp.Message = "seekfs service pipe denied access; run seekfs launch/setup-service from an elevated shell to refresh the service ACL"
		} else {
			resp.Message = "seekfs service is not fully healthy"
		}
	}
	return resp
}

func serviceStatusBySC(name string) (bool, bool) {
	cmd := exec.Command("sc.exe", "query", name)
	out, err := cmd.Output()
	if err != nil {
		return false, false
	}
	text := strings.ToUpper(string(out))
	return true, strings.Contains(text, "RUNNING")
}

func waitForDoctor(pipeName string, timeout time.Duration) doctorResponse {
	deadline := time.Now().Add(timeout)
	var resp doctorResponse
	for {
		resp = probeDoctor(pipeName)
		if resp.OK || time.Now().After(deadline) {
			return resp
		}
		if !engineV9Enabled() {
			time.Sleep(500 * time.Millisecond)
		}
	}
}

func queryWindowsService() (svc.Status, error) {
	m, err := mgr.Connect()
	if err != nil {
		return svc.Status{}, err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return svc.Status{}, err
	}
	defer s.Close()
	return s.Query()
}

func callService(pipeName string, req serviceRequest) (serviceResponse, error) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := callServiceOnce(pipeName, req)
		if err == nil {
			return resp, nil
		}
		if !isTransientPipeError(err) || time.Now().After(deadline) {
			if strings.Contains(strings.ToLower(err.Error()), "access is denied") {
				return serviceResponse{}, fmt.Errorf("access denied opening seekfs service pipe %s; run seekfs launch/setup-service from an elevated shell to refresh the service ACL: %w", pipeName, err)
			}
			return serviceResponse{}, err
		}
		time.Sleep(75 * time.Millisecond)
	}
}

func callServiceOnce(pipeName string, req serviceRequest) (serviceResponse, error) {
	handle, err := openPipeClient(pipeName)
	if err != nil {
		return serviceResponse{}, err
	}
	file := os.NewFile(uintptr(handle), pipeName)
	defer file.Close()
	return exchangeServiceJSON(file, req, serviceCallTimeout(req))
}

func isTransientPipeError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, windows.ERROR_PIPE_BUSY) ||
		errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED) ||
		errors.Is(err, windows.ERROR_BROKEN_PIPE) ||
		errors.Is(err, windows.ERROR_NO_DATA) ||
		errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "pipe is being closed") ||
		strings.Contains(text, "broken pipe") ||
		strings.Contains(text, "no process is on the other end")
}

func serviceCallTimeout(req serviceRequest) time.Duration {
	switch req.Command {
	case "search", "status", "info":
		return serviceQueryTimeout
	default:
		return 0
	}
}

func exchangeServiceJSON(conn io.ReadWriteCloser, req serviceRequest, timeout time.Duration) (serviceResponse, error) {
	if timeout <= 0 {
		return exchangeServiceJSONBlocking(conn, req)
	}
	type result struct {
		resp serviceResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := exchangeServiceJSONBlocking(conn, req)
		done <- result{resp: resp, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-done:
		return res.resp, res.err
	case <-timer.C:
		_ = conn.Close()
		return serviceResponse{}, fmt.Errorf("seekfs service %q request timed out after %s", req.Command, timeout)
	}
}

func exchangeServiceJSONBlocking(conn io.ReadWriteCloser, req serviceRequest) (serviceResponse, error) {
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return serviceResponse{}, err
	}
	var resp serviceResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return serviceResponse{}, err
	}
	return resp, nil
}

func openPipeClient(pipeName string) (windows.Handle, error) {
	return openPipeClientWithTimeout(pipeName, 5*time.Second)
}

func openPipeClientWithTimeout(pipeName string, timeout time.Duration) (windows.Handle, error) {
	ptr, err := windows.UTF16PtrFromString(pipeName)
	if err != nil {
		return 0, err
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		handle, err := windows.CreateFile(
			ptr,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_ATTRIBUTE_NORMAL,
			0,
		)
		if err == nil {
			return handle, nil
		}
		if err != windows.ERROR_PIPE_BUSY || time.Now().After(deadline) {
			return 0, err
		}
		sleep := 50 * time.Millisecond
		if remaining := time.Until(deadline); remaining < sleep {
			sleep = remaining
		}
		if sleep <= 0 {
			return 0, err
		}
		time.Sleep(sleep)
	}
}

func (s *goSearchService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	if s.processMode == "" {
		s.processMode = "windows-service"
	}
	changes <- svc.Status{State: svc.StartPending}
	done := make(chan struct{})
	go func() {
		s.servePrivileged()
		close(done)
	}()
	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				serviceLog("startup index load panic: %v\n%s", r, string(debug.Stack()))
			}
		}()
		if err := s.loadConfiguredIndexes(); err != nil {
			serviceLog("startup index load error: %v", err)
		}
	}()
	for req := range r {
		switch req.Cmd {
		case svc.Interrogate:
			changes <- req.CurrentStatus
		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending}
			close(s.stop)
			<-done
			return false, 0
		}
	}
	return false, 0
}

func (s *goSearchService) runStandalone() error {
	if s.processMode == "" {
		s.processMode = "standalone"
	}
	fmt.Fprintf(os.Stderr, "seekfs privileged service listening on %s\n", s.pipeName)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				serviceLog("startup index load panic: %v\n%s", r, string(debug.Stack()))
			}
		}()
		if err := s.loadConfiguredIndexes(); err != nil {
			serviceLog("startup index load error: %v", err)
		}
	}()
	s.servePrivileged()
	return nil
}

func (s *goSearchService) loadConfiguredIndexes() error {
	s.indexMu.Lock()
	s.loading = true
	s.loadErr = ""
	s.indexMu.Unlock()
	defer func() {
		s.indexMu.Lock()
		s.loading = false
		s.indexMu.Unlock()
	}()
	if len(s.dbs) == 0 {
		serviceLog("service started without search databases")
		return nil
	}
	start := time.Now()
	indexes, volumes, total, err := loadConfiguredVolumes(s.dbs)
	if err != nil {
		s.indexMu.Lock()
		s.loadErr = err.Error()
		s.indexMu.Unlock()
		return err
	}
	s.indexMu.Lock()
	s.indexes = indexes
	s.volumes = volumes
	s.loadErr = ""
	s.indexMu.Unlock()
	debug.FreeOSMemory()
	s.startBackgroundNameOrderBuilds(volumes)
	s.startBackgroundNameTrigramBuilds(volumes)
	for _, vol := range volumes {
		if vol.state == "ready" && vol.index.Compact && vol.index.Source == "usn" {
			go s.replayVolumeLoop(vol)
			go s.persistVolumeLoop(vol)
		} else if vol.state == "ready" && vol.index.Source == "walk" {
			s.startWalkWatchers(vol)
		}
	}
	serviceLog("loaded %d dbs entries=%d elapsed=%s", len(indexes), total, time.Since(start).Round(time.Millisecond))
	return nil
}

type startupVolumeResult struct {
	idx *Index
	vol *serviceVolumeIndex
	err error
}

func loadConfiguredVolumes(dbs []string) ([]*Index, []*serviceVolumeIndex, int, error) {
	results := make([]startupVolumeResult, len(dbs))
	workers := serviceStartupWorkerCount(len(dbs))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, dbPath := range dbs {
		i, dbPath := i, dbPath
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			idx, vol, err := loadConfiguredVolume(dbPath)
			results[i] = startupVolumeResult{idx: idx, vol: vol, err: err}
		}()
	}
	wg.Wait()
	indexes := make([]*Index, 0, len(results))
	volumes := make([]*serviceVolumeIndex, 0, len(results))
	total := 0
	for _, result := range results {
		if result.err != nil {
			return nil, nil, 0, result.err
		}
		indexes = append(indexes, result.idx)
		volumes = append(volumes, result.vol)
		total += result.idx.entryCount()
	}
	return indexes, volumes, total, nil
}

func serviceStartupWorkerCount(dbCount int) int {
	if dbCount <= 1 {
		return 1
	}
	raw := strings.TrimSpace(os.Getenv("SEEKFS_STARTUP_WORKERS"))
	if raw != "" {
		n, err := strconv.Atoi(raw)
		if err == nil && n > 0 {
			return min(n, dbCount)
		}
	}
	return min(serviceStartupDefaultWorkers, dbCount)
}

func loadConfiguredVolume(dbPath string) (*Index, *serviceVolumeIndex, error) {
	idx, err := loadIndexForService(dbPath)
	if err != nil {
		return nil, nil, err
	}
	idx.DBPath = dbPath
	vol := newServiceVolumeIndex(dbPath, idx)
	if err := vol.replayWAL(); err != nil {
		serviceLog("startup wal replay skipped volume=%s db=%s err=%v", vol.volume, vol.dbPath, err)
		vol.state = "stale"
		vol.staleReason = err.Error()
		if shouldRebuildStaleIndex(err) {
			if rebuilt, rebuildErr := rebuildServiceVolumeIndex(vol); rebuildErr == nil {
				serviceLog("startup wal rebuild complete volume=%s db=%s entries=%d", rebuilt.volume, rebuilt.dbPath, rebuilt.index.entryCount())
				vol = rebuilt
				idx = rebuilt.index
			} else {
				serviceLog("startup wal rebuild failed volume=%s db=%s err=%v", vol.volume, vol.dbPath, rebuildErr)
			}
		}
	}
	if err := catchUpServiceVolume(vol); err != nil {
		serviceLog("startup catch-up skipped volume=%s db=%s err=%v", vol.volume, vol.dbPath, err)
		if shouldRebuildStaleIndex(err) {
			if rebuilt, rebuildErr := rebuildServiceVolumeIndex(vol); rebuildErr == nil {
				serviceLog("startup stale rebuild complete volume=%s db=%s entries=%d", rebuilt.volume, rebuilt.dbPath, rebuilt.index.entryCount())
				vol = rebuilt
				idx = rebuilt.index
			} else {
				serviceLog("startup stale rebuild failed volume=%s db=%s err=%v", vol.volume, vol.dbPath, rebuildErr)
			}
		}
	}
	if engineV9Enabled() && serviceLowMemoryMode() && idx.Compact && idx.MMapRecords == nil {
		if mmapIdx, mmapErr := loadIndexMMap(dbPath); mmapErr == nil {
			idx = mmapIdx
			vol = newServiceVolumeIndex(dbPath, idx)
		} else {
			serviceLog("lowmem mmap load fallback volume=%s db=%s err=%v", vol.volume, dbPath, mmapErr)
		}
		debug.FreeOSMemory()
	}
	vol.queryIndex = buildResidentQueryIndex(vol)
	vol.resetNameOrderBuild()
	vol.resetNameTrigrams()
	if vol.needsCompactChildrenBuild() {
		vol.buildCompactChildren()
	}
	return idx, vol, nil
}

func loadIndexForService(dbPath string) (*Index, error) {
	if !serviceLowMemoryMode() {
		idx, err := loadIndex(dbPath)
		if err != nil {
			return nil, err
		}
		ensureCompactIndexForService(idx)
		return idx, nil
	}
	if !engineV9Enabled() {
		idx, err := loadIndex(dbPath)
		if err != nil {
			return nil, err
		}
		ensureCompactIndexForService(idx)
		return idx, nil
	}
	idx, err := loadIndexMMap(dbPath)
	if err == nil {
		ensureCompactIndexForService(idx)
		return idx, nil
	}
	serviceLog("lowmem mmap initial load fallback db=%s err=%v", dbPath, err)
	idx, err = loadIndex(dbPath)
	if err != nil {
		return nil, err
	}
	ensureCompactIndexForService(idx)
	return idx, nil
}

func ensureCompactIndexForService(idx *Index) {
	if idx == nil || idx.Compact || len(idx.Entries) == 0 {
		return
	}
	records := make([]CompactRecord, 0, len(idx.Entries))
	idByPath := make(map[string]int32, len(idx.Entries))
	if idx.Volume == "" {
		for _, entry := range idx.Entries {
			if vol := filepath.VolumeName(entry.Path); vol != "" {
				idx.Volume = vol
				break
			}
		}
	}
	for i, entry := range idx.Entries {
		path := filepath.Clean(entry.Path)
		name := entry.Name
		if name == "" {
			name = filepath.Base(path)
		}
		if name == "." || name == string(filepath.Separator) || name == "" {
			name = filepath.VolumeName(path)
			if name == "" {
				name = "."
			}
		}
		rec := CompactRecord{
			FRN:       uint64(i + 1),
			ParentFRN: uint64(i + 1),
			Parent:    -1,
			Name:      name,
			Mode:      entry.Mode,
			Size:      entry.Size,
			ModUnix:   entry.ModUnix,
		}
		records = append(records, rec)
		idByPath[strings.ToLower(path)] = int32(i)
	}
	for i, entry := range idx.Entries {
		path := filepath.Clean(entry.Path)
		parentPath := filepath.Dir(path)
		if parentPath == path || parentPath == "." {
			continue
		}
		parent, ok := idByPath[strings.ToLower(parentPath)]
		if !ok {
			continue
		}
		records[i].Parent = parent
		records[i].ParentFRN = records[parent].FRN
	}
	idx.Records = records
	idx.Entries = nil
	idx.NameOrder = nil
	idx.PathOrder = nil
	idx.Compact = true
	buildOrders(idx)
	if serviceLowMemoryMode() {
		idx.packCompactRecords(true)
	}
}

func newServiceVolumeIndex(dbPath string, idx *Index) *serviceVolumeIndex {
	vol := &serviceVolumeIndex{
		dbPath:      dbPath,
		index:       idx,
		volume:      idx.Volume,
		journalID:   idx.JournalID,
		checkpoint:  idx.Checkpoint,
		state:       "ready",
		pathCache:   make(map[int]string),
		lastPersist: time.Now(),
	}
	if idx.Compact && idx.Source == "usn" {
		recordCount := idx.compactRecordCount()
		largeResident := recordCount >= 1_000_000 || serviceLowMemoryMode()
		if !largeResident {
			ensureCompactNameOrderSorted(idx)
		} else if idx.MMapRecords == nil {
			idx.CompactNameOrder = nil
			idx.packCompactRecords(true)
		} else {
			idx.CompactNameOrder = nil
		}
		recordCount = idx.compactRecordCount()
		if !serviceLowMemoryMode() {
			vol.frns = make([]uint64, 0, recordCount)
			vol.frnRecordIDs = make([]uint32, 0, recordCount)
		}
		if !largeResident {
			vol.children = make(map[uint64]map[int]struct{}, recordCount)
			vol.exactNames = make(map[string][]int, recordCount/2)
		}
		if vol.frns != nil || vol.children != nil || vol.exactNames != nil {
			for i := 0; i < recordCount; i++ {
				rec := idx.compactRecord(i)
				if vol.frns != nil && rec.FRN != 0 {
					vol.frns = append(vol.frns, rec.FRN)
					vol.frnRecordIDs = append(vol.frnRecordIDs, uint32(i))
				}
				if vol.children != nil && rec.ParentFRN != 0 && rec.ParentFRN != rec.FRN {
					vol.addChild(rec.ParentFRN, i)
				}
				if name := idx.compactLowerNameAt(i); vol.exactNames != nil && !rec.Deleted && name != "" {
					vol.exactNames[name] = append(vol.exactNames[name], i)
				}
			}
		}
		if vol.frns != nil {
			sortFRNIndexEntries(vol.frns, vol.frnRecordIDs)
		}
		vol.queryIndex = buildResidentQueryIndex(vol)
		vol.applyDerivedSections()
		vol.resetNameOrderBuild()
		vol.resetNameTrigrams()
		if vol.needsCompactChildrenBuild() {
			vol.buildCompactChildren()
		}
	}
	if engineV9Enabled() {
		vol.overlay = newOverlaySegment()
		vol.publishSnapshot()
	}
	return vol
}

func newOverlaySegment() *overlaySegment {
	return &overlaySegment{
		byFRN: make(map[uint64]int32),
	}
}

func (set *overlayBaseIDSet) add(id int32) {
	if id < 0 {
		return
	}
	word := int(id) / 64
	bit := uint(id) % 64
	if word >= len(set.bits) {
		grown := make([]uint64, word+1)
		copy(grown, set.bits)
		set.bits = grown
	}
	mask := uint64(1) << bit
	if set.bits[word]&mask != 0 {
		return
	}
	set.bits[word] |= mask
	set.ids = append(set.ids, id)
	set.count++
}

func (set *overlayBaseIDSet) contains(id int32) bool {
	if id < 0 {
		return false
	}
	word := int(id) / 64
	if word >= len(set.bits) {
		return false
	}
	return set.bits[word]&(uint64(1)<<(uint(id)%64)) != 0
}

func (set *overlayBaseIDSet) len() int {
	if set == nil {
		return 0
	}
	return set.count
}

func (vol *serviceVolumeIndex) publishSnapshot() {
	if vol == nil || !engineV9Enabled() {
		return
	}
	gen := vol.snapshotGen.Add(1)
	watermark := int32(0)
	var records []CompactRecord
	var tombstoneIDs []int32
	var shadowedIDs []int32
	if vol.overlay != nil {
		watermark = vol.overlay.watermark.Load()
		records = vol.overlay.records
		tombstoneIDs = sortedInt32Snapshot(vol.overlay.tombstone.ids)
		shadowedIDs = sortedInt32Snapshot(vol.overlay.shadowed.ids)
	}
	vol.snap.Store(&volumeSnapshot{base: vol.index, records: records, tombstoneIDs: tombstoneIDs, shadowedIDs: shadowedIDs, watermark: watermark, gen: gen})
}

func sortedInt32Snapshot(ids []int32) []int32 {
	if len(ids) == 0 {
		return nil
	}
	out := append([]int32(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (vol *serviceVolumeIndex) applyDerivedSections() {
	if vol == nil || vol.index == nil {
		return
	}
	derived := vol.index.Derived
	if len(derived.NameOrder) > 0 && len(derived.NameRank) > 0 {
		if vol.queryIndex == nil {
			vol.queryIndex = &residentQueryIndex{}
		}
		vol.queryIndex.nameOrder = derived.NameOrder
		vol.queryIndex.nameRank = derived.NameRank
	}
	if len(derived.SizeOrder) > 0 && len(derived.SizeRank) > 0 {
		if vol.queryIndex == nil {
			vol.queryIndex = &residentQueryIndex{}
		}
		vol.queryIndex.sizeOrder = derived.SizeOrder
		vol.queryIndex.sizeRank = derived.SizeRank
	}
	if len(derived.ModOrder) > 0 && len(derived.ModRank) > 0 {
		if vol.queryIndex == nil {
			vol.queryIndex = &residentQueryIndex{}
		}
		vol.queryIndex.modOrder = derived.ModOrder
		vol.queryIndex.modRank = derived.ModRank
	}
	if len(derived.ExtOrder) > 0 && len(derived.ExtRank) > 0 {
		if vol.queryIndex == nil {
			vol.queryIndex = &residentQueryIndex{}
		}
		vol.queryIndex.extOrder = derived.ExtOrder
		vol.queryIndex.extRank = derived.ExtRank
	}
	if len(derived.TypeOrder) > 0 && len(derived.TypeRank) > 0 {
		if vol.queryIndex == nil {
			vol.queryIndex = &residentQueryIndex{}
		}
		vol.queryIndex.typeOrder = derived.TypeOrder
		vol.queryIndex.typeRank = derived.TypeRank
	}
	if len(derived.PathOrder) > 0 && len(derived.PathRank) > 0 {
		if vol.queryIndex == nil {
			vol.queryIndex = &residentQueryIndex{}
		}
		vol.queryIndex.pathOrder = derived.PathOrder
		vol.queryIndex.pathRank = derived.PathRank
	}
	if len(derived.ChildOffsets) > 0 && len(derived.ChildIDs) > 0 {
		vol.childOffsets = derived.ChildOffsets
		vol.childIDs = derived.ChildIDs
		vol.rootIDs = derived.RootIDs
	}
	if len(derived.SubtreeStart) > 0 && len(derived.SubtreeEnd) > 0 && len(derived.SubtreeOrder) > 0 {
		vol.subtreeStart = derived.SubtreeStart
		vol.subtreeEnd = derived.SubtreeEnd
		vol.subtreeOrder = derived.SubtreeOrder
		vol.subtreeSizeRank = derived.SubtreeSizeRank
		vol.subtreeModRank = derived.SubtreeModRank
		vol.subtreeExtRank = derived.SubtreeExtRank
		vol.subtreeTypeRank = derived.SubtreeTypeRank
		vol.subtreePathRank = derived.SubtreePathRank
	}
	if len(derived.FRNs) > 0 && len(derived.FRNRecordIDs) == len(derived.FRNs) {
		vol.frns = derived.FRNs
		vol.frnRecordIDs = derived.FRNRecordIDs
	}
}

func sortFRNIndexEntries(frns []uint64, ids []uint32) {
	if len(frns) <= 1 || len(frns) != len(ids) || sort.SliceIsSorted(frns, func(i, j int) bool {
		return frns[i] < frns[j]
	}) {
		return
	}
	sort.Sort(frnIndexPairs{frns: frns, ids: ids})
}

type frnIndexPairs struct {
	frns []uint64
	ids  []uint32
}

func (p frnIndexPairs) Len() int { return len(p.frns) }

func (p frnIndexPairs) Less(i, j int) bool {
	if p.frns[i] == p.frns[j] {
		return p.ids[i] < p.ids[j]
	}
	return p.frns[i] < p.frns[j]
}

func (p frnIndexPairs) Swap(i, j int) {
	p.frns[i], p.frns[j] = p.frns[j], p.frns[i]
	p.ids[i], p.ids[j] = p.ids[j], p.ids[i]
}

func catchUpServiceVolume(vol *serviceVolumeIndex) error {
	if vol.index == nil || !vol.index.Compact || vol.index.Source != "usn" || vol.volume == "" {
		return nil
	}
	handle, err := openVolume(vol.volume)
	if err != nil {
		vol.state = "stale"
		vol.staleReason = err.Error()
		return err
	}
	defer windows.CloseHandle(handle)

	journal, err := queryUSNJournal(handle)
	if err != nil {
		vol.state = "stale"
		vol.staleReason = err.Error()
		return err
	}
	if err := validateUSNCheckpoint(vol, journal); err != nil {
		vol.state = "stale"
		vol.staleReason = err.Error()
		return err
	}
	if vol.checkpoint >= journal.NextUsn {
		vol.state = "ready"
		return nil
	}
	vol.state = "replaying"
	buffer := make([]byte, 4*1024*1024)
	for vol.checkpoint < journal.NextUsn {
		nextUSN, changes, err := readUSNChanges(handle, journal.UsnJournalID, vol.checkpoint, buffer)
		if err != nil {
			vol.state = "stale"
			vol.staleReason = err.Error()
			return err
		}
		if nextUSN <= vol.checkpoint {
			break
		}
		if engineV9Enabled() {
			if err := appendWAL(vol.dbPath, nextUSN, changes); err != nil {
				vol.state = "stale"
				vol.staleReason = err.Error()
				return err
			}
		}
		vol.applyUSNChanges(changes)
		vol.checkpoint = nextUSN
		vol.index.Checkpoint = nextUSN
	}
	if vol.dbPath != "" {
		if engineV9Enabled() {
			vol.dirty = true
		} else {
			if err := saveIndex(vol.dbPath, vol.index); err != nil {
				vol.state = "stale"
				vol.staleReason = err.Error()
				return err
			}
			vol.repackResidentRecordsIfBloated()
			releaseServiceMemoryAfterSave()
			if err := removeWAL(vol.dbPath); err != nil {
				serviceLog("wal cleanup error volume=%s db=%s err=%v", vol.volume, vol.dbPath, err)
			}
		}
	}
	vol.state = "ready"
	vol.staleReason = ""
	return nil
}

func (vol *serviceVolumeIndex) idForFRN(frn uint64) (int, bool) {
	if frn == 0 {
		return 0, false
	}
	if vol.frnToID != nil {
		if id, ok := vol.frnToID[frn]; ok {
			return id, true
		}
	}
	i := sort.Search(len(vol.frns), func(i int) bool { return vol.frns[i] >= frn })
	if i < len(vol.frns) && i < len(vol.frnRecordIDs) && vol.frns[i] == frn {
		return int(vol.frnRecordIDs[i]), true
	}
	return 0, false
}

func (vol *serviceVolumeIndex) addFRNID(frn uint64, id int) {
	if frn == 0 {
		return
	}
	if _, ok := vol.idForFRN(frn); ok {
		return
	}
	if vol.frnToID == nil {
		vol.frnToID = make(map[uint64]int)
	}
	vol.frnToID[frn] = id
}

func (vol *serviceVolumeIndex) frnRecordCount() int {
	return len(vol.frns) + len(vol.frnToID)
}

func (s *goSearchService) replayVolumeLoop(vol *serviceVolumeIndex) {
	buffer := make([]byte, 4*1024*1024)
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		if err := s.replayVolumeOnce(vol, buffer); err != nil {
			serviceLog("background replay error volume=%s db=%s err=%v", vol.volume, vol.dbPath, err)
			if shouldRebuildStaleIndex(err) {
				rebuildErr := s.rebuildVolumeInPlace(vol)
				if rebuildErr == nil {
					time.Sleep(500 * time.Millisecond)
					continue
				}
				serviceLog("background stale rebuild failed volume=%s db=%s err=%v", vol.volume, vol.dbPath, rebuildErr)
			}
			s.indexMu.Lock()
			vol.state = "stale"
			vol.staleReason = err.Error()
			s.indexMu.Unlock()
			time.Sleep(5 * time.Second)
			continue
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (s *goSearchService) replayVolumeOnce(vol *serviceVolumeIndex, buffer []byte) error {
	handle, err := openVolume(vol.volume)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)

	s.indexMu.RLock()
	startUSN := vol.checkpoint
	journalID := vol.journalID
	s.indexMu.RUnlock()
	if journal, err := queryUSNJournal(handle); err != nil {
		return err
	} else if err := validateUSNCheckpoint(vol, journal); err != nil {
		return err
	}

	var nextUSN int64
	var changes []usnChange
	err = nil
	if engineV9Enabled() {
		nextUSN, changes, err = readUSNChangesWait(handle, journalID, startUSN, buffer, 5*time.Second, 1)
	} else {
		nextUSN, changes, err = readUSNChanges(handle, journalID, startUSN, buffer)
	}
	if err != nil {
		return err
	}
	if nextUSN <= startUSN {
		return nil
	}
	if err := appendWAL(vol.dbPath, nextUSN, changes); err != nil {
		s.indexMu.Lock()
		vol.state = "stale"
		vol.staleReason = err.Error()
		s.indexMu.Unlock()
		return err
	}
	s.indexMu.Lock()
	if vol.checkpoint != startUSN {
		s.indexMu.Unlock()
		return nil
	}
	vol.applyUSNChanges(changes)
	vol.checkpoint = nextUSN
	vol.index.Checkpoint = nextUSN
	vol.state = "ready"
	vol.staleReason = ""
	vol.dirty = true
	s.indexMu.Unlock()
	return nil
}

func (s *goSearchService) persistVolumeLoop(vol *serviceVolumeIndex) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			s.persistVolumeIfDue(vol, true)
			return
		case <-ticker.C:
			s.persistVolumeIfDue(vol, false)
		}
	}
}

func (s *goSearchService) persistVolumeIfDue(vol *serviceVolumeIndex, force bool) {
	if vol.dbPath == "" {
		return
	}
	if !force && envBool("SEEKFS_DISABLE_BACKGROUND_PERSIST") {
		return
	}
	now := time.Now()
	s.indexMu.RLock()
	dirty := vol.dirty
	retryAfter := vol.persistRetryAfter
	due := force || vol.compactionDue(now)
	if !engineV9Enabled() {
		due = force || (now.Sub(vol.lastPersist) >= persistDebounce && (retryAfter.IsZero() || !now.Before(retryAfter)))
	} else if !retryAfter.IsZero() && now.Before(retryAfter) {
		due = false
	}
	s.indexMu.RUnlock()
	if !dirty || !due {
		return
	}
	s.indexMu.Lock()
	now = time.Now()
	due = force || vol.compactionDue(now)
	if !engineV9Enabled() {
		due = force || (now.Sub(vol.lastPersist) >= persistDebounce && (vol.persistRetryAfter.IsZero() || !now.Before(vol.persistRetryAfter)))
	} else if !vol.persistRetryAfter.IsZero() && now.Before(vol.persistRetryAfter) {
		due = false
	}
	if !vol.dirty || !due {
		s.indexMu.Unlock()
		return
	}
	saved := false
	var err error
	if engineV9Enabled() && vol.overlay != nil {
		err = s.compactOverlayVolumeLocked(vol)
	} else {
		err = saveIndex(vol.dbPath, vol.index)
	}
	if err != nil {
		vol.notePersistFailureLocked(err, now)
		serviceLog("background persist error volume=%s db=%s failures=%d retry_after=%s err=%v", vol.volume, vol.dbPath, vol.persistFailures, vol.persistRetryAfter.Format(time.RFC3339Nano), err)
	} else {
		if !engineV9Enabled() {
			vol.repackResidentRecordsIfBloated()
		}
		if err := removeWAL(vol.dbPath); err != nil {
			serviceLog("wal cleanup error volume=%s db=%s err=%v", vol.volume, vol.dbPath, err)
		}
		vol.dirty = false
		vol.lastPersist = time.Now()
		vol.persistFailures = 0
		vol.persistRetryAfter = time.Time{}
		vol.lastPersistErr = ""
		if !engineV9Enabled() {
			vol.afterPersist()
		}
		saved = true
	}
	s.indexMu.Unlock()
	if saved {
		releaseServiceMemoryAfterSave()
		s.startBackgroundNameOrderBuilds([]*serviceVolumeIndex{vol})
		s.startBackgroundNameTrigramBuilds([]*serviceVolumeIndex{vol})
	}
}

func (vol *serviceVolumeIndex) compactionDue(now time.Time) bool {
	if vol == nil || !engineV9Enabled() {
		return false
	}
	if vol.overlay != nil {
		watermark := int(vol.overlay.watermark.Load())
		if watermark >= overlayCompactionMaxSlots {
			return true
		}
		baseCount := 0
		if vol.index != nil {
			baseCount = vol.index.compactRecordCount()
		}
		if baseCount > 0 && vol.overlay.tombstone.len()*100 >= baseCount*overlayCompactionTombstoneP {
			return true
		}
	}
	if vol.dbPath != "" {
		if info, err := os.Stat(walPath(vol.dbPath)); err == nil && info.Size() >= overlayCompactionMaxWAL {
			return true
		}
	}
	return !vol.lastPersist.IsZero() && now.Sub(vol.lastPersist) >= overlayCompactionDirtyAge
}

func (vol *serviceVolumeIndex) notePersistFailureLocked(err error, now time.Time) {
	if vol == nil || err == nil {
		return
	}
	vol.persistFailures++
	vol.persistRetryAfter = now.Add(persistFailureBackoff(vol.persistFailures))
	vol.lastPersistErr = err.Error()
}

func (s *goSearchService) compactOverlayVolumeLocked(vol *serviceVolumeIndex) error {
	if vol == nil || vol.index == nil || vol.dbPath == "" {
		return nil
	}
	if err := vol.compactOverlayLocked(); err != nil {
		return err
	}
	for i, existing := range s.volumes {
		if existing == vol {
			s.indexes[i] = vol.index
			break
		}
	}
	return nil
}

func (vol *serviceVolumeIndex) compactOverlayLocked() error {
	if vol == nil || vol.index == nil || vol.dbPath == "" {
		return nil
	}
	if err := compactOverlayToDisk(vol); err != nil {
		return err
	}
	loaded, err := loadIndexForService(vol.dbPath)
	if err != nil {
		return err
	}
	replacement := newServiceVolumeIndex(vol.dbPath, loaded)
	replacement.state = "ready"
	replacement.staleReason = ""
	replacement.dirty = false
	replacement.lastPersist = time.Now()
	replaceServiceVolumeContents(vol, replacement)
	return nil
}

func compactOverlayToDisk(vol *serviceVolumeIndex) error {
	if vol == nil || vol.index == nil || vol.dbPath == "" {
		return nil
	}
	compacted := compactOverlayIndex(vol)
	if err := closeIndexMMapRecords(vol.index); err != nil {
		return err
	}
	return saveIndex(vol.dbPath, compacted)
}

func closeIndexMMapRecords(idx *Index) error {
	if idx == nil || idx.MMapRecords == nil || idx.MMapRecords.file == nil {
		return nil
	}
	if err := idx.MMapRecords.file.close(); err != nil {
		return err
	}
	idx.MMapRecords = nil
	return nil
}

func compactOverlayIndex(vol *serviceVolumeIndex) *Index {
	base := vol.index
	out := &Index{
		Version:      indexVersion,
		Roots:        append([]string(nil), base.Roots...),
		BuiltAt:      time.Now(),
		Source:       base.Source,
		Volume:       base.Volume,
		JournalID:    base.JournalID,
		Checkpoint:   vol.checkpoint,
		ContentHash:  base.ContentHash,
		Compact:      true,
		CompactAttrs: base.CompactAttrs,
	}
	if engineV9Enabled() {
		out.Version = indexVersionV9
	}
	if out.Volume == "" {
		out.Volume = vol.volume
	}
	records := make([]CompactRecord, 0, base.compactRecordCount()+len(vol.overlay.records))
	if vol.overlay == nil {
		for id := 0; id < base.compactRecordCount(); id++ {
			rec := base.compactRecord(id)
			if !rec.Deleted {
				rec.Parent = -1
				rec.Name = strings.Clone(rec.Name)
				records = append(records, rec)
			}
		}
	} else {
		for id := 0; id < base.compactRecordCount(); id++ {
			if vol.overlay.tombstone.contains(int32(id)) {
				continue
			}
			if vol.overlay.shadowed.contains(int32(id)) {
				continue
			}
			rec := base.compactRecord(id)
			if rec.Deleted {
				continue
			}
			rec.Parent = -1
			rec.Name = strings.Clone(rec.Name)
			records = append(records, rec)
		}
		watermark := int(vol.overlay.watermark.Load())
		if watermark > len(vol.overlay.records) {
			watermark = len(vol.overlay.records)
		}
		for slot := 0; slot < watermark; slot++ {
			rec := vol.overlay.records[slot]
			if current, ok := vol.overlay.byFRN[rec.FRN]; ok && current != int32(slot) {
				continue
			}
			if rec.Deleted {
				continue
			}
			rec.Parent = -1
			rec.Name = strings.Clone(rec.Name)
			records = append(records, rec)
		}
	}
	idByFRN := make(map[uint64]int32, len(records))
	for i, rec := range records {
		idByFRN[rec.FRN] = int32(i)
	}
	for i := range records {
		parentFRN := records[i].ParentFRN
		if parentFRN == 0 || parentFRN == records[i].FRN {
			records[i].Parent = -1
			continue
		}
		if parent, ok := idByFRN[parentFRN]; ok {
			records[i].Parent = parent
		}
	}
	out.Records = records
	buildOrders(out)
	return out
}

func persistFailureBackoff(failures int) time.Duration {
	if failures <= 0 {
		return time.Minute
	}
	if failures > 6 {
		failures = 6
	}
	return time.Duration(1<<(failures-1)) * time.Minute
}

func releaseServiceMemoryAfterSave() {
	debug.FreeOSMemory()
}

func (vol *serviceVolumeIndex) afterPersist() {
	vol.queryIndex = buildResidentQueryIndex(vol)
	vol.resetNameOrderBuild()
	vol.resetNameTrigrams()
	if vol.needsCompactChildrenBuild() {
		vol.buildCompactChildren()
	}
	vol.recentIDs = nil
	vol.recentSeq++
	vol.clearSearchCachesLocked()
	debug.FreeOSMemory()
}

func (vol *serviceVolumeIndex) repackResidentRecordsIfBloated() {
	if vol == nil || vol.index == nil || !vol.index.packedNameBlobLooksBloated() {
		return
	}
	before := len(vol.index.PackedRecords.NameBlob)
	start := time.Now()
	vol.index.repackCompactRecords()
	debug.FreeOSMemory()
	after := 0
	if vol.index.PackedRecords != nil {
		after = len(vol.index.PackedRecords.NameBlob)
	}
	serviceLog("repacked resident name blob volume=%s before=%d after=%d elapsed=%s", vol.volume, before, after, time.Since(start).Round(time.Millisecond))
}

func queryUSNJournal(handle windows.Handle) (usnJournalDataV0, error) {
	var journal usnJournalDataV0
	var bytesReturned uint32
	err := windows.DeviceIoControl(
		handle,
		fsctlQueryUSNJournal,
		nil,
		0,
		(*byte)(unsafe.Pointer(&journal)),
		uint32(unsafe.Sizeof(journal)),
		&bytesReturned,
		nil,
	)
	return journal, err
}

func validateUSNCheckpoint(vol *serviceVolumeIndex, journal usnJournalDataV0) error {
	if vol.journalID != 0 && vol.journalID != journal.UsnJournalID {
		return staleIndexError{reason: fmt.Sprintf("journal id changed from %d to %d", vol.journalID, journal.UsnJournalID), rebuild: true}
	}
	firstValid := journal.FirstUsn
	if journal.LowestValidUsn > firstValid {
		firstValid = journal.LowestValidUsn
	}
	if vol.checkpoint < firstValid {
		return staleIndexError{reason: fmt.Sprintf("checkpoint %d is before first valid USN %d", vol.checkpoint, firstValid), rebuild: true}
	}
	if vol.checkpoint > journal.NextUsn {
		return staleIndexError{reason: fmt.Sprintf("checkpoint %d is after journal next USN %d", vol.checkpoint, journal.NextUsn), rebuild: true}
	}
	return nil
}

type staleIndexError struct {
	reason  string
	rebuild bool
}

func (e staleIndexError) Error() string {
	return e.reason
}

func shouldRebuildStaleIndex(err error) bool {
	var staleErr staleIndexError
	if errors.As(err, &staleErr) {
		return staleErr.rebuild
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "journal id changed") ||
		strings.Contains(text, "before first valid usn") ||
		strings.Contains(text, "after journal next usn")
}

func rebuildServiceVolumeIndex(vol *serviceVolumeIndex) (*serviceVolumeIndex, error) {
	if vol == nil || vol.volume == "" || vol.dbPath == "" {
		return nil, errors.New("stale index rebuild requires a volume and db path")
	}
	idx, err := indexUSNVolume(vol.volume)
	if err != nil {
		return nil, err
	}
	buildOrders(idx)
	if err := closeIndexMMapRecords(vol.index); err != nil {
		return nil, err
	}
	if err := saveIndex(vol.dbPath, idx); err != nil {
		return nil, err
	}
	releaseServiceMemoryAfterSave()
	if err := removeWAL(vol.dbPath); err != nil {
		serviceLog("wal cleanup error volume=%s db=%s err=%v", vol.volume, vol.dbPath, err)
	}
	rebuilt := newServiceVolumeIndex(vol.dbPath, idx)
	rebuilt.state = "ready"
	rebuilt.staleReason = ""
	return rebuilt, nil
}

func rebuildWalkIndex(vol *serviceVolumeIndex) (*Index, error) {
	if vol == nil || vol.index == nil || len(vol.index.Roots) == 0 {
		return nil, errors.New("walk index rebuild requires roots")
	}
	idx := &Index{
		Version: vol.index.Version,
		Roots:   append([]string(nil), vol.index.Roots...),
		BuiltAt: time.Now(),
		Source:  "walk",
	}
	if idx.Version == 0 {
		idx.Version = indexVersion
	}
	for _, root := range idx.Roots {
		if err := walkRoot(root, idx); err != nil {
			serviceLog("walk watcher rebuild root error root=%s db=%s err=%v", root, vol.dbPath, err)
		}
	}
	buildOrders(idx)
	return idx, nil
}

func (s *goSearchService) rebuildWalkVolumeInPlace(vol *serviceVolumeIndex, reason string) error {
	if vol == nil || vol.dbPath == "" {
		return errors.New("walk index rebuild requires a db path")
	}
	vol.walkMu.Lock()
	defer vol.walkMu.Unlock()
	idx, err := rebuildWalkIndex(vol)
	if err != nil {
		return err
	}
	s.indexMu.Lock()
	if err := closeIndexMMapRecords(vol.index); err != nil {
		s.indexMu.Unlock()
		return err
	}
	if err := saveIndex(vol.dbPath, idx); err != nil {
		s.indexMu.Unlock()
		return err
	}
	loaded, err := loadIndexForService(vol.dbPath)
	if err != nil {
		s.indexMu.Unlock()
		return err
	}
	rebuilt := newServiceVolumeIndex(vol.dbPath, loaded)
	rebuilt.state = "ready"
	rebuilt.staleReason = ""
	replaceServiceVolumeContents(vol, rebuilt)
	for i, existing := range s.volumes {
		if existing == vol {
			s.indexes[i] = vol.index
			break
		}
	}
	s.indexMu.Unlock()
	releaseServiceMemoryAfterSave()
	s.startBackgroundNameTrigramBuilds([]*serviceVolumeIndex{vol})
	serviceLog("rebuilt walk index db=%s entries=%d reason=%s", vol.dbPath, loaded.entryCount(), reason)
	return nil
}

func (s *goSearchService) rebuildVolumeInPlace(vol *serviceVolumeIndex) error {
	if vol == nil || vol.volume == "" || vol.dbPath == "" {
		return errors.New("stale index rebuild requires a volume and db path")
	}
	idx, err := indexUSNVolume(vol.volume)
	if err != nil {
		return err
	}
	buildOrders(idx)

	s.indexMu.Lock()
	if err := closeIndexMMapRecords(vol.index); err != nil {
		s.indexMu.Unlock()
		return err
	}
	if err := saveIndex(vol.dbPath, idx); err != nil {
		s.indexMu.Unlock()
		return err
	}
	if err := removeWAL(vol.dbPath); err != nil {
		serviceLog("wal cleanup error volume=%s db=%s err=%v", vol.volume, vol.dbPath, err)
	}
	rebuilt := newServiceVolumeIndex(vol.dbPath, idx)
	rebuilt.state = "ready"
	rebuilt.staleReason = ""
	replaceServiceVolumeContents(vol, rebuilt)
	for i, existing := range s.volumes {
		if existing == vol {
			s.indexes[i] = vol.index
			break
		}
	}
	s.indexMu.Unlock()
	releaseServiceMemoryAfterSave()
	s.startBackgroundNameTrigramBuilds([]*serviceVolumeIndex{vol})
	serviceLog("rebuilt stale index volume=%s db=%s entries=%d", rebuilt.volume, rebuilt.dbPath, rebuilt.index.entryCount())
	return nil
}

func (s *goSearchService) startWalkWatchers(vol *serviceVolumeIndex) {
	if vol == nil || vol.index == nil || vol.index.Source != "walk" {
		return
	}
	for _, root := range vol.index.Roots {
		root := root
		go s.watchWalkRoot(vol, root)
	}
}

func (s *goSearchService) watchWalkRoot(vol *serviceVolumeIndex, root string) {
	handle, err := openDirectoryForChanges(root)
	if err != nil {
		serviceLog("walk watcher disabled root=%s db=%s err=%v", root, vol.dbPath, err)
		return
	}
	defer windows.CloseHandle(handle)
	buffer := make([]byte, 64*1024)
	mask := uint32(windows.FILE_NOTIFY_CHANGE_FILE_NAME |
		windows.FILE_NOTIFY_CHANGE_DIR_NAME |
		windows.FILE_NOTIFY_CHANGE_ATTRIBUTES |
		windows.FILE_NOTIFY_CHANGE_SIZE |
		windows.FILE_NOTIFY_CHANGE_LAST_WRITE |
		windows.FILE_NOTIFY_CHANGE_CREATION)
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		var bytesReturned uint32
		err := windows.ReadDirectoryChanges(handle, &buffer[0], uint32(len(buffer)), true, mask, &bytesReturned, nil, 0)
		if err != nil {
			if errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
				return
			}
			if errors.Is(err, windows.ERROR_NOTIFY_ENUM_DIR) {
				_ = s.rebuildWalkVolumeInPlace(vol, "watch-overflow")
				continue
			}
			serviceLog("walk watcher read error root=%s db=%s err=%v", root, vol.dbPath, err)
			select {
			case <-s.stop:
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}
		reason := "watch-change"
		if bytesReturned == 0 {
			reason = "watch-overflow"
		}
		select {
		case <-s.stop:
			return
		case <-time.After(250 * time.Millisecond):
		}
		if err := s.rebuildWalkVolumeInPlace(vol, reason); err != nil {
			serviceLog("walk watcher rebuild error root=%s db=%s err=%v", root, vol.dbPath, err)
		}
	}
}

func openDirectoryForChanges(root string) (windows.Handle, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return windows.InvalidHandle, err
	}
	ptr, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return windows.CreateFile(
		ptr,
		windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
}

func (vol *serviceVolumeIndex) applyUSNChanges(changes []usnChange) {
	if engineV9Enabled() {
		for _, change := range changes {
			if change.FRN == 0 {
				continue
			}
			vol.recordOverlayChange(change)
			if change.USN > vol.checkpoint {
				vol.checkpoint = change.USN
			}
		}
		vol.index.Checkpoint = vol.checkpoint
		vol.dirty = true
		vol.publishSnapshot()
		return
	}
	for _, change := range changes {
		if change.FRN == 0 {
			continue
		}
		vol.recordOverlayChange(change)
		if change.Reason&usnReasonRenameOld != 0 && change.Reason&usnReasonRenameNew == 0 {
			continue
		}
		if change.Reason&usnReasonFileDelete != 0 {
			if id, ok := vol.idForFRN(change.FRN); ok {
				vol.markNameTrigramRecent(id)
				vol.removeExactName(id)
				vol.markDeleted(id)
			}
			if change.USN > vol.checkpoint {
				vol.checkpoint = change.USN
			}
			continue
		}
		id, ok := vol.idForFRN(change.FRN)
		if !ok {
			id = vol.index.appendCompactRecord(CompactRecord{FRN: change.FRN})
			vol.addFRNID(change.FRN, id)
		}
		if vol.recentIDs == nil {
			vol.recentIDs = make(map[int]struct{})
		}
		vol.recentIDs[id] = struct{}{}
		vol.markNameTrigramRecent(id)
		vol.recentSeq++
		rec := vol.index.compactRecord(id)
		if rec.ParentFRN != 0 && rec.ParentFRN != change.ParentFRN {
			vol.removeChild(rec.ParentFRN, id)
		}
		vol.removeExactName(id)
		rec.FRN = change.FRN
		rec.ParentFRN = change.ParentFRN
		rec.Parent = -1
		if parentID, ok := vol.idForFRN(change.ParentFRN); ok && parentID != id {
			rec.Parent = int32(parentID)
		}
		if change.ParentFRN != 0 && change.ParentFRN != change.FRN {
			vol.addChild(change.ParentFRN, id)
		}
		vol.repairChildren(change.FRN, id)
		if change.Name != "" {
			rec.Name = change.Name
		}
		rec.Mode = modeFromAttrs(change.Attr)
		vol.index.CompactAttrs = true
		rec.Deleted = false
		vol.index.setCompactRecord(id, rec)
		vol.addExactName(id)
		if change.USN > vol.checkpoint {
			vol.checkpoint = change.USN
		}
	}
	vol.index.Checkpoint = vol.checkpoint
	vol.pathCache = make(map[int]string)
	vol.publishSnapshot()
}

func (vol *serviceVolumeIndex) recordOverlayChange(change usnChange) {
	if vol == nil || !engineV9Enabled() {
		return
	}
	if vol.overlay == nil {
		vol.overlay = newOverlaySegment()
	}
	overlay := vol.overlay
	if change.Reason&usnReasonRenameOld != 0 && change.Reason&usnReasonRenameNew == 0 {
		return
	}
	if change.Reason&usnReasonFileDelete != 0 {
		rec := CompactRecord{FRN: change.FRN, ParentFRN: change.ParentFRN, Name: change.Name, Deleted: true}
		// Only directories can have descendants, so the O(overlay) cascade
		// below is skipped when the deleted FRN is PROVABLY a plain file:
		// some record for it exists (base or prior live overlay slot) and
		// no available record says directory. The overlay slot is fresher
		// than the base record (it shadows it), so a live overlay dir
		// create on a reused FRN must veto stale base file evidence. If
		// dir-ness cannot be determined at all, cascade conservatively:
		// correctness wins. This keeps mass file-delete churn (e.g. a
		// node_modules removal WITH per-child USN records) at O(deletes)
		// on the apply path instead of O(deletes x overlay).
		baseID, hasBase := vol.idForFRN(change.FRN)
		provenFile := hasBase && vol.index.compactRecord(baseID).Mode&uint32(os.ModeDir) == 0
		if slot, ok := overlay.byFRN[change.FRN]; ok && slot >= 0 && int(slot) < len(overlay.records) {
			prev := overlay.records[slot]
			if rec.Name == "" {
				rec.Name = prev.Name
			}
			if rec.ParentFRN == 0 {
				rec.ParentFRN = prev.ParentFRN
			}
			if !prev.Deleted {
				if prev.Mode&uint32(os.ModeDir) != 0 {
					provenFile = false
				} else if !hasBase {
					provenFile = true
				}
			}
		}
		slot := int32(len(overlay.records))
		overlay.byFRN[change.FRN] = slot
		overlay.records = append(overlay.records, rec)
		if hasBase {
			vol.tombstoneBaseSubtree(baseID)
		}
		if !provenFile {
			vol.cascadeOverlayDelete(change.FRN)
		}
		overlay.watermark.Store(int32(len(overlay.records)))
		return
	}
	rec := CompactRecord{
		FRN:       change.FRN,
		ParentFRN: change.ParentFRN,
		Parent:    -1,
		Name:      change.Name,
		Mode:      modeFromAttrs(change.Attr),
	}
	vol.index.CompactAttrs = true
	if baseID, ok := vol.idForFRN(change.FRN); ok {
		overlay.shadowed.add(int32(baseID))
		base := vol.index.compactRecord(baseID)
		if rec.Name == "" {
			rec.Name = base.Name
		}
		if rec.ParentFRN == 0 {
			rec.ParentFRN = base.ParentFRN
		}
	}
	slot := int32(len(overlay.records))
	overlay.byFRN[change.FRN] = slot
	overlay.records = append(overlay.records, rec)
	overlay.watermark.Store(int32(len(overlay.records)))
}

func (vol *serviceVolumeIndex) tombstoneBaseSubtree(rootID int) {
	if vol == nil || vol.overlay == nil || vol.index == nil || rootID < 0 || rootID >= vol.index.compactRecordCount() {
		return
	}
	if rootID < len(vol.subtreeStart) && rootID < len(vol.subtreeEnd) && len(vol.subtreeOrder) > 0 {
		start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start != ^uint32(0) && start <= end && int(end) <= len(vol.subtreeOrder) {
			for _, id32 := range vol.subtreeOrder[start:end] {
				vol.overlay.tombstone.add(int32(id32))
			}
			return
		}
	}
	stack := []int{rootID}
	for len(stack) > 0 {
		last := len(stack) - 1
		id := stack[last]
		stack = stack[:last]
		if id < 0 || id >= vol.index.compactRecordCount() || vol.overlay.tombstone.contains(int32(id)) {
			continue
		}
		vol.overlay.tombstone.add(int32(id))
		for _, childID := range vol.childIDsForRecord(id) {
			stack = append(stack, int(childID))
		}
	}
}

// cascadeOverlayDelete handles the case a plain base-subtree tombstone
// cannot: overlay-only descendants (created purely via USN, never
// persisted) whose parent chain passes through deletedFRN or through a
// base id that tombstoneBaseSubtree just marked. Those descendants have
// no overlay slot of their own that got tombstoned by the delete branch
// above (only deletedFRN's own slot did), and overlayRecordPath falls
// back to the base index for any ancestor FRN without a live overlay
// slot — which does not consult vol.overlay.tombstone. So without this
// cascade an overlay-only child parented (directly or transitively)
// under a deleted base directory stays visible forever.
//
// This runs at apply time, once per delete, and is append-only: for
// every live overlay slot whose ancestor chain is doomed, it appends a
// new Deleted record for that FRN (mirroring the manual delete branch),
// never mutating overlay.records in place. Cost is O(overlay slots x
// chain depth) with per-FRN memoization, which is acceptable because the
// overlay is bounded (~64k slots before compaction) and directory
// deletes are rare; this trades a bounded amount of apply-time work for
// avoiding any read-path (per-query) traversal, which is the failure
// mode reviews F1/G1 flagged.
func (vol *serviceVolumeIndex) cascadeOverlayDelete(deletedFRN uint64) {
	if vol == nil || vol.overlay == nil || vol.index == nil {
		return
	}
	overlay := vol.overlay
	if len(overlay.records) == 0 {
		return
	}
	latest := latestOverlaySlotsByFRN(overlay.records)

	// doomed memoizes, per FRN visited during this cascade, whether that
	// FRN's own identity (i.e. treating it as an ancestor) is dead: either
	// it IS deletedFRN, or it resolves (via live overlay slot, else base
	// id) to a base id in vol.overlay.tombstone, or its own parent chain
	// is doomed.
	doomed := make(map[uint64]bool, 8)
	var resolve func(frn uint64, seen map[uint64]struct{}) bool
	resolve = func(frn uint64, seen map[uint64]struct{}) bool {
		if frn == 0 {
			return false
		}
		if frn == deletedFRN {
			return true
		}
		if v, ok := doomed[frn]; ok {
			return v
		}
		if _, cyc := seen[frn]; cyc {
			// Cycle in parent chain (shouldn't happen); treat as not doomed
			// rather than infinite-looping.
			return false
		}
		seen[frn] = struct{}{}

		result := false
		if slot, ok := latest[frn]; ok && slot >= 0 && int(slot) < len(overlay.records) {
			rec := overlay.records[slot]
			if rec.Deleted {
				result = true
			} else if rec.ParentFRN != 0 && rec.ParentFRN != frn {
				result = resolve(rec.ParentFRN, seen)
			}
		} else if baseID, ok := vol.idForFRN(frn); ok {
			if overlay.tombstone.contains(int32(baseID)) {
				result = true
			} else if baseID >= 0 && baseID < vol.index.compactRecordCount() {
				rec := vol.index.compactRecord(baseID)
				if rec.ParentFRN != 0 && rec.ParentFRN != frn {
					result = resolve(rec.ParentFRN, seen)
				}
			}
		}
		doomed[frn] = result
		return result
	}

	type victim struct {
		frn  uint64
		name string
	}
	var victims []victim
	for frn, slot := range latest {
		if frn == 0 || frn == deletedFRN {
			continue
		}
		rec := overlay.records[slot]
		if rec.Deleted {
			continue
		}
		if rec.ParentFRN == 0 || rec.ParentFRN == frn {
			continue
		}
		if resolve(rec.ParentFRN, map[uint64]struct{}{frn: {}}) {
			victims = append(victims, victim{frn: frn, name: rec.Name})
		}
	}
	if len(victims) == 0 {
		return
	}
	// Deterministic append order so replay from WAL is reproducible.
	sort.Slice(victims, func(i, j int) bool { return victims[i].frn < victims[j].frn })
	for _, v := range victims {
		slot := latest[v.frn]
		prev := overlay.records[slot]
		newSlot := int32(len(overlay.records))
		overlay.byFRN[v.frn] = newSlot
		overlay.records = append(overlay.records, CompactRecord{
			FRN:       v.frn,
			ParentFRN: prev.ParentFRN,
			Name:      v.name,
			Deleted:   true,
		})
	}
}

func (vol *serviceVolumeIndex) replayWAL() error {
	return vol.replayWALWithLimit(serviceStartupWALRebuildBytes)
}

func (vol *serviceVolumeIndex) replayWALWithLimit(maxBytes int64) error {
	if vol == nil || vol.dbPath == "" {
		return nil
	}
	f, err := os.Open(walPath(vol.dbPath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	if maxBytes > 0 {
		if info, err := f.Stat(); err == nil && info.Size() > maxBytes {
			return staleIndexError{
				reason:  fmt.Sprintf("wal %s is %d bytes; rebuilding instead of replaying", walPath(vol.dbPath), info.Size()),
				rebuild: true,
			}
		}
	}
	br := bufio.NewReaderSize(f, 1024*1024)
	prefix, err := br.Peek(len(walMagicV1))
	if err == nil && bytes.Equal(prefix, walMagicV1) {
		_, _ = br.Discard(len(walMagicV1))
		return vol.replayBinaryWAL(br)
	}
	dec := json.NewDecoder(br)
	applied := 0
	for {
		var batch walBatch
		if err := dec.Decode(&batch); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		if batch.NextUSN <= vol.checkpoint {
			continue
		}
		vol.applyUSNChanges(batch.Changes)
		vol.checkpoint = batch.NextUSN
		vol.index.Checkpoint = batch.NextUSN
		vol.dirty = true
		applied++
	}
	if applied > 0 {
		serviceLog("replayed wal volume=%s db=%s batches=%d checkpoint=%d", vol.volume, vol.dbPath, applied, vol.checkpoint)
	}
	return nil
}

func (vol *serviceVolumeIndex) replayBinaryWAL(r io.Reader) error {
	applied := 0
	for {
		var header [8]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) && applied == 0 {
				return nil
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return err
			}
			return err
		}
		length := binary.LittleEndian.Uint32(header[0:4])
		wantCRC := binary.LittleEndian.Uint32(header[4:8])
		if length == 0 || length > 64*1024*1024 {
			return fmt.Errorf("invalid wal frame length %d", length)
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			return err
		}
		if got := crc32.ChecksumIEEE(payload); got != wantCRC {
			return fmt.Errorf("wal frame crc mismatch got=%08x want=%08x", got, wantCRC)
		}
		batch, err := decodeBinaryWALBatch(payload)
		if err != nil {
			return err
		}
		if batch.NextUSN <= vol.checkpoint {
			continue
		}
		vol.applyUSNChanges(batch.Changes)
		vol.checkpoint = batch.NextUSN
		vol.index.Checkpoint = batch.NextUSN
		vol.dirty = true
		applied++
	}
}

func walPath(dbPath string) string {
	return dbPath + ".wal"
}

func appendWAL(dbPath string, nextUSN int64, changes []usnChange) error {
	if dbPath == "" || len(changes) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(walPath(dbPath), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	err = nil
	if engineV9Enabled() {
		err = appendBinaryWALFrame(f, nextUSN, changes)
	} else {
		enc := json.NewEncoder(f)
		err = enc.Encode(walBatch{NextUSN: nextUSN, Changes: changes})
	}
	if syncErr := f.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

func appendBinaryWALFrame(f *os.File, nextUSN int64, changes []usnChange) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		if _, err := f.Write(walMagicV1); err != nil {
			return err
		}
	}
	payload, err := encodeBinaryWALBatch(nextUSN, changes)
	if err != nil {
		return err
	}
	var header [8]byte
	binary.LittleEndian.PutUint32(header[0:4], uint32(len(payload)))
	binary.LittleEndian.PutUint32(header[4:8], crc32.ChecksumIEEE(payload))
	if _, err := f.Write(header[:]); err != nil {
		return err
	}
	_, err = f.Write(payload)
	return err
}

func encodeBinaryWALBatch(nextUSN int64, changes []usnChange) ([]byte, error) {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, nextUSN)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(changes)))
	for _, change := range changes {
		if len(change.Name) > int(^uint16(0)) {
			return nil, errors.New("wal change name too large")
		}
		_ = binary.Write(&buf, binary.LittleEndian, change.FRN)
		_ = binary.Write(&buf, binary.LittleEndian, change.ParentFRN)
		_ = binary.Write(&buf, binary.LittleEndian, change.USN)
		_ = binary.Write(&buf, binary.LittleEndian, change.Reason)
		_ = binary.Write(&buf, binary.LittleEndian, change.Attr)
		_ = binary.Write(&buf, binary.LittleEndian, uint16(len(change.Name)))
		_, _ = buf.WriteString(change.Name)
	}
	return buf.Bytes(), nil
}

func decodeBinaryWALBatch(payload []byte) (walBatch, error) {
	var batch walBatch
	if len(payload) < 12 {
		return batch, errors.New("wal frame too small")
	}
	off := 0
	batch.NextUSN = int64(binary.LittleEndian.Uint64(payload[off:]))
	off += 8
	count := int(binary.LittleEndian.Uint32(payload[off:]))
	off += 4
	if count < 0 {
		return batch, errors.New("invalid wal change count")
	}
	batch.Changes = make([]usnChange, 0, count)
	for i := 0; i < count; i++ {
		if off+34 > len(payload) {
			return batch, errors.New("truncated wal change")
		}
		change := usnChange{}
		change.FRN = binary.LittleEndian.Uint64(payload[off:])
		off += 8
		change.ParentFRN = binary.LittleEndian.Uint64(payload[off:])
		off += 8
		change.USN = int64(binary.LittleEndian.Uint64(payload[off:]))
		off += 8
		change.Reason = binary.LittleEndian.Uint32(payload[off:])
		off += 4
		change.Attr = binary.LittleEndian.Uint32(payload[off:])
		off += 4
		nameLen := int(binary.LittleEndian.Uint16(payload[off:]))
		off += 2
		if off+nameLen < off || off+nameLen > len(payload) {
			return batch, errors.New("truncated wal change name")
		}
		change.Name = string(payload[off : off+nameLen])
		off += nameLen
		batch.Changes = append(batch.Changes, change)
	}
	if off != len(payload) {
		return batch, errors.New("wal frame has trailing bytes")
	}
	return batch, nil
}

func removeWAL(dbPath string) error {
	if dbPath == "" {
		return nil
	}
	err := os.Remove(walPath(dbPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (vol *serviceVolumeIndex) rebuildChildren() {
	recordCount := vol.index.compactRecordCount()
	vol.children = make(map[uint64]map[int]struct{}, recordCount)
	for i := 0; i < recordCount; i++ {
		rec := vol.index.compactRecord(i)
		if rec.ParentFRN != 0 && rec.ParentFRN != rec.FRN {
			vol.addChild(rec.ParentFRN, i)
		}
	}
}

func (vol *serviceVolumeIndex) addExactName(id int) {
	if id < 0 || id >= vol.index.compactRecordCount() {
		return
	}
	rec := vol.index.compactRecord(id)
	name := vol.index.compactLowerNameAt(id)
	if rec.Deleted || name == "" {
		return
	}
	if vol.exactNames == nil {
		vol.exactNames = make(map[string][]int)
	}
	vol.exactNames[name] = append(vol.exactNames[name], id)
}

func (vol *serviceVolumeIndex) removeExactName(id int) {
	if id < 0 || id >= vol.index.compactRecordCount() || vol.exactNames == nil {
		return
	}
	name := vol.index.compactLowerNameAt(id)
	if name == "" {
		return
	}
	list := vol.exactNames[name]
	for i, value := range list {
		if value == id {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(vol.exactNames, name)
	} else {
		vol.exactNames[name] = list
	}
}

func (vol *serviceVolumeIndex) addChild(parentFRN uint64, id int) {
	if parentFRN == 0 {
		return
	}
	vol.childOffsets = nil
	vol.childIDs = nil
	if vol.children == nil {
		return
	}
	kids := vol.children[parentFRN]
	if kids == nil {
		kids = make(map[int]struct{})
		vol.children[parentFRN] = kids
	}
	kids[id] = struct{}{}
}

func (vol *serviceVolumeIndex) removeChild(parentFRN uint64, id int) {
	if parentFRN == 0 {
		return
	}
	vol.childOffsets = nil
	vol.childIDs = nil
	if vol.children == nil {
		return
	}
	kids := vol.children[parentFRN]
	if kids == nil {
		return
	}
	delete(kids, id)
	if len(kids) == 0 {
		delete(vol.children, parentFRN)
	}
}

func (vol *serviceVolumeIndex) repairChildren(parentFRN uint64, parentID int) {
	if vol.children == nil {
		return
	}
	for childID := range vol.children[parentFRN] {
		if childID == parentID || childID < 0 || childID >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(childID)
		rec.Parent = int32(parentID)
		vol.index.setCompactRecord(childID, rec)
	}
}

func (vol *serviceVolumeIndex) buildCompactChildren() {
	if vol == nil || vol.index == nil {
		return
	}
	recordCount := vol.index.compactRecordCount()
	counts := make([]uint32, recordCount+1)
	roots := make([]uint32, 0, 16)
	total := 0
	for id := 0; id < recordCount; id++ {
		rec := vol.index.compactRecord(id)
		parent := int(rec.Parent)
		if parent < 0 || parent >= recordCount || parent == id {
			if !rec.Deleted {
				roots = append(roots, uint32(id))
			}
			continue
		}
		counts[parent+1]++
		total++
	}
	vol.rootIDs = roots
	if total == 0 {
		return
	}
	for i := 1; i < len(counts); i++ {
		counts[i] += counts[i-1]
	}
	childIDs := make([]uint32, total)
	next := append([]uint32(nil), counts[:recordCount]...)
	for id := 0; id < recordCount; id++ {
		rec := vol.index.compactRecord(id)
		parent := int(rec.Parent)
		if parent < 0 || parent >= recordCount || parent == id {
			continue
		}
		pos := next[parent]
		childIDs[pos] = uint32(id)
		next[parent]++
	}
	vol.childOffsets = counts
	vol.childIDs = childIDs
	if serviceSubtreeIntervalsEnabled() {
		vol.buildSubtreeRanges()
	} else {
		vol.subtreeOrder = nil
		vol.subtreeStart = nil
		vol.subtreeEnd = nil
	}
}

func (vol *serviceVolumeIndex) needsCompactChildrenBuild() bool {
	return vol != nil &&
		vol.index != nil &&
		vol.index.Compact &&
		vol.index.Source == "usn" &&
		len(vol.childOffsets) == 0 &&
		len(vol.childIDs) == 0 &&
		len(vol.index.Derived.ChildOffsets) == 0
}

func serviceSubtreeIntervalsEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SEEKFS_SUBTREE_INTERVALS")))
	if serviceLowMemoryMode() && v == "" {
		return false
	}
	return v != "0" && v != "false" && v != "no" && v != "off"
}

func servicePathGramsEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SEEKFS_PATH_GRAMS")))
	if serviceLowMemoryMode() && v == "" {
		return true
	}
	return v != "0" && v != "false" && v != "no" && v != "off"
}

func serviceNameOrderEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SEEKFS_NAME_ORDER")))
	if serviceLowMemoryMode() && v == "" {
		return false
	}
	return v != "0" && v != "false" && v != "no" && v != "off"
}

func serviceNameOrderEnabledForIndex(idx *Index) bool {
	if idx == nil || !idx.Compact || !serviceNameOrderEnabled() {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SEEKFS_NAME_ORDER")))
	if v == "1" || v == "true" || v == "yes" || v == "on" {
		return true
	}
	return idx.compactRecordCount() <= serviceBackgroundNameOrderMaxRecords
}

func serviceNameTrigramsEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SEEKFS_NAME_TRIGRAMS")))
	if serviceLowMemoryMode() && v == "" {
		return true
	}
	return v != "0" && v != "false" && v != "no" && v != "off"
}

func serviceLowMemoryMode() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SEEKFS_MEMORY_MODE")))
	return v == "lowmem" || v == "mmap" || v == "low-memory"
}

func envBool(name string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func serviceNameTrigramsEnabledForIndex(idx *Index) bool {
	if idx == nil || !idx.Compact || !serviceNameTrigramsEnabled() {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SEEKFS_NAME_TRIGRAMS")))
	if v == "1" || v == "true" || v == "yes" || v == "on" {
		return true
	}
	if serviceLowMemoryMode() {
		return true
	}
	return idx.compactRecordCount() <= serviceNameTrigramMaxRecords()
}

func serviceNameTrigramMaxRecords() int {
	raw := strings.TrimSpace(os.Getenv("SEEKFS_NAME_TRIGRAM_MAX_RECORDS"))
	if raw == "" {
		return serviceNameTrigramDefaultMaxRecords
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return serviceNameTrigramDefaultMaxRecords
	}
	return n
}

func serviceLowMemoryTrigramStoredPostingMax() int {
	raw := strings.TrimSpace(os.Getenv("SEEKFS_LOW_MEMORY_TRIGRAM_MAX_POSTING"))
	if raw == "" {
		return trigramLowMemoryStoredPostingMaxCount
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return trigramLowMemoryStoredPostingMaxCount
	}
	return n
}

func (s *goSearchService) startBackgroundNameTrigramBuilds(volumes []*serviceVolumeIndex) {
	if !serviceNameTrigramsEnabled() || len(volumes) == 0 {
		return
	}
	go func() {
		for _, vol := range volumes {
			if vol == nil || !serviceNameTrigramsEnabledForIndex(vol.index) {
				continue
			}
			s.rebuildNameTrigramsInBackground(vol)
		}
		debug.FreeOSMemory()
	}()
}

func (s *goSearchService) startBackgroundNameOrderBuilds(volumes []*serviceVolumeIndex) {
	if !serviceNameOrderEnabled() || len(volumes) == 0 {
		return
	}
	go func() {
		for _, vol := range volumes {
			if vol == nil || !serviceNameOrderEnabledForIndex(vol.index) {
				continue
			}
			s.rebuildNameOrderInBackground(vol)
		}
		debug.FreeOSMemory()
	}()
}

func (s *goSearchService) rebuildNameOrderInBackground(vol *serviceVolumeIndex) {
	if vol == nil || !vol.nameOrderState.CompareAndSwap(nameTrigramStatePending, nameTrigramStateBuilding) {
		return
	}
	vol.rebuildNameOrderLocked()
}

func (s *goSearchService) rebuildNameTrigramsInBackground(vol *serviceVolumeIndex) {
	if vol == nil || !vol.nameTrigramState.CompareAndSwap(nameTrigramStatePending, nameTrigramStateBuilding) {
		return
	}
	vol.rebuildNameTrigramsLocked()
}

func (vol *serviceVolumeIndex) resetNameOrderBuild() {
	if vol == nil {
		return
	}
	vol.nameOrderMillis.Store(0)
	if vol.queryIndex != nil && len(vol.queryIndex.nameOrder) > 0 {
		vol.nameOrderState.Store(nameTrigramStateReady)
		return
	}
	if serviceNameOrderEnabledForIndex(vol.index) {
		vol.nameOrderState.Store(nameTrigramStatePending)
	} else {
		vol.nameOrderState.Store(nameTrigramStateDisabled)
	}
}

func (vol *serviceVolumeIndex) rebuildNameOrderLocked() {
	if vol == nil || vol.index == nil || !serviceNameOrderEnabledForIndex(vol.index) {
		vol.resetNameOrderBuild()
		return
	}
	start := time.Now()
	order, ranks := buildCompactNameOrderRank(vol.index)
	vol.searchMu.Lock()
	if vol.queryIndex == nil {
		vol.queryIndex = &residentQueryIndex{}
	}
	vol.queryIndex.nameOrder = order
	vol.queryIndex.nameRank = ranks
	vol.queryIndex.extTop = buildExtTopPostings(vol.queryIndex.ext, ranks, serviceExtTopPostingLimit)
	vol.nameOrderMillis.Store(time.Since(start).Milliseconds())
	vol.nameOrderState.Store(nameTrigramStateReady)
	vol.searchMu.Unlock()
	serviceLog("built resident name order volume=%s records=%d bytes=%d elapsed=%s",
		vol.volume, vol.index.compactRecordCount(), (len(order)+len(ranks))*4, time.Since(start).Round(time.Millisecond))
}

func buildCompactNameOrderRank(idx *Index) ([]uint32, []uint32) {
	recordCount := 0
	if idx != nil {
		recordCount = idx.compactRecordCount()
	}
	if recordCount == 0 {
		return nil, nil
	}
	order := make([]uint32, 0, recordCount)
	for id := 0; id < recordCount; id++ {
		rec := idx.compactRecord(id)
		if !rec.Deleted {
			order = append(order, uint32(id))
		}
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := int(order[i]), int(order[j])
		an, bn := idx.compactLowerNameAt(a), idx.compactLowerNameAt(b)
		if an == bn {
			return a < b
		}
		return an < bn
	})
	ranks := make([]uint32, recordCount)
	for i := range ranks {
		ranks[i] = uint32(i)
	}
	for pos, id32 := range order {
		id := int(id32)
		if id >= 0 && id < recordCount {
			ranks[id] = uint32(pos)
		}
	}
	return order, ranks
}

func buildCompactSizeOrderRank(idx *Index) ([]uint32, []uint32) {
	recordCount := 0
	if idx != nil {
		recordCount = idx.compactRecordCount()
	}
	if recordCount == 0 {
		return nil, nil
	}
	order := make([]uint32, 0, recordCount)
	for id := 0; id < recordCount; id++ {
		rec := idx.compactRecord(id)
		if !rec.Deleted {
			order = append(order, uint32(id))
		}
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := int(order[i]), int(order[j])
		ar, br := idx.compactRecord(a), idx.compactRecord(b)
		if ar.Size != br.Size {
			return ar.Size < br.Size
		}
		an, bn := idx.compactLowerNameAt(a), idx.compactLowerNameAt(b)
		if an != bn {
			return an < bn
		}
		return a < b
	})
	ranks := make([]uint32, recordCount)
	for i := range ranks {
		ranks[i] = uint32(i)
	}
	for pos, id32 := range order {
		id := int(id32)
		if id >= 0 && id < recordCount {
			ranks[id] = uint32(pos)
		}
	}
	return order, ranks
}

func buildCompactModifiedOrderRank(idx *Index) ([]uint32, []uint32) {
	recordCount := 0
	if idx != nil {
		recordCount = idx.compactRecordCount()
	}
	if recordCount == 0 {
		return nil, nil
	}
	order := make([]uint32, 0, recordCount)
	for id := 0; id < recordCount; id++ {
		rec := idx.compactRecord(id)
		if !rec.Deleted {
			order = append(order, uint32(id))
		}
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := int(order[i]), int(order[j])
		ar, br := idx.compactRecord(a), idx.compactRecord(b)
		if ar.ModUnix != br.ModUnix {
			if ar.ModUnix == 0 {
				return false
			}
			if br.ModUnix == 0 {
				return true
			}
			return ar.ModUnix > br.ModUnix
		}
		an, bn := idx.compactLowerNameAt(a), idx.compactLowerNameAt(b)
		if an != bn {
			return an < bn
		}
		return a < b
	})
	ranks := make([]uint32, recordCount)
	for i := range ranks {
		ranks[i] = uint32(i)
	}
	for pos, id32 := range order {
		id := int(id32)
		if id >= 0 && id < recordCount {
			ranks[id] = uint32(pos)
		}
	}
	return order, ranks
}

func buildCompactExtensionOrderRank(idx *Index) ([]uint32, []uint32) {
	recordCount := 0
	if idx != nil {
		recordCount = idx.compactRecordCount()
	}
	if recordCount == 0 {
		return nil, nil
	}
	order := make([]uint32, 0, recordCount)
	for id := 0; id < recordCount; id++ {
		rec := idx.compactRecord(id)
		if !rec.Deleted {
			order = append(order, uint32(id))
		}
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := int(order[i]), int(order[j])
		ae, be := compactRecordLowerExt(idx.compactRecord(a)), compactRecordLowerExt(idx.compactRecord(b))
		if ae != be {
			return ae < be
		}
		an, bn := idx.compactLowerNameAt(a), idx.compactLowerNameAt(b)
		if an != bn {
			return an < bn
		}
		return a < b
	})
	ranks := make([]uint32, recordCount)
	for i := range ranks {
		ranks[i] = uint32(i)
	}
	for pos, id32 := range order {
		id := int(id32)
		if id >= 0 && id < recordCount {
			ranks[id] = uint32(pos)
		}
	}
	return order, ranks
}

func buildCompactTypeOrderRank(idx *Index) ([]uint32, []uint32) {
	recordCount := 0
	if idx != nil {
		recordCount = idx.compactRecordCount()
	}
	if recordCount == 0 {
		return nil, nil
	}
	order := make([]uint32, 0, recordCount)
	for id := 0; id < recordCount; id++ {
		rec := idx.compactRecord(id)
		if !rec.Deleted {
			order = append(order, uint32(id))
		}
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := int(order[i]), int(order[j])
		ar, br := idx.compactRecord(a), idx.compactRecord(b)
		at, bt := compactRecordTypeRank(ar), compactRecordTypeRank(br)
		if at != bt {
			return at < bt
		}
		an, bn := idx.compactLowerNameAt(a), idx.compactLowerNameAt(b)
		if an != bn {
			return an < bn
		}
		return a < b
	})
	ranks := make([]uint32, recordCount)
	for i := range ranks {
		ranks[i] = uint32(i)
	}
	for pos, id32 := range order {
		id := int(id32)
		if id >= 0 && id < recordCount {
			ranks[id] = uint32(pos)
		}
	}
	return order, ranks
}

func buildCompactPathOrderRank(idx *Index) ([]uint32, []uint32) {
	recordCount := 0
	if idx != nil {
		recordCount = idx.compactRecordCount()
	}
	if recordCount == 0 {
		return nil, nil
	}
	order := make([]uint32, 0, recordCount)
	for id := 0; id < recordCount; id++ {
		rec := idx.compactRecord(id)
		if !rec.Deleted {
			order = append(order, uint32(id))
		}
	}
	cache := make(map[int]string)
	sort.Slice(order, func(i, j int) bool {
		a, b := int(order[i]), int(order[j])
		ap := strings.ToLower(idx.reconstructCompactPathCached(a, cache))
		bp := strings.ToLower(idx.reconstructCompactPathCached(b, cache))
		if ap != bp {
			return ap < bp
		}
		return a < b
	})
	ranks := make([]uint32, recordCount)
	for i := range ranks {
		ranks[i] = uint32(i)
	}
	for pos, id32 := range order {
		id := int(id32)
		if id >= 0 && id < recordCount {
			ranks[id] = uint32(pos)
		}
	}
	return order, ranks
}

func compactRecordTypeRank(rec CompactRecord) int {
	if rec.Mode&uint32(os.ModeDir) != 0 {
		return 0
	}
	return 1
}

func compactRecordLowerExt(rec CompactRecord) string {
	ext := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
	if ext == "" {
		return ""
	}
	return strings.ToLower(ext)
}

func buildExtTopPostings(ext map[string][]uint32, ranks []uint32, limit int) map[string][]uint32 {
	return buildExtTopPostingsMin(ext, ranks, limit, 1)
}

func buildExtTopPostingsMin(ext map[string][]uint32, ranks []uint32, limit int, minIDs int) map[string][]uint32 {
	if len(ext) == 0 || limit <= 0 {
		return nil
	}
	if minIDs <= 0 {
		minIDs = 1
	}
	out := make(map[string][]uint32, len(ext))
	for key, ids := range ext {
		if len(ids) < minIDs {
			continue
		}
		if len(ids) <= limit {
			top := append([]uint32(nil), ids...)
			sortExtTopByRank(top, ranks)
			out[key] = top
			continue
		}
		h := make(extRankMaxHeap, 0, limit)
		for _, id := range ids {
			item := extRankItem{id: id, rank: extRankOf(id, ranks)}
			if len(h) < limit {
				heap.Push(&h, item)
				continue
			}
			if extRankLess(item, h[0]) {
				h[0] = item
				heap.Fix(&h, 0)
			}
		}
		top := make([]uint32, len(h))
		for i := range h {
			top[i] = h[i].id
		}
		sortExtTopByRank(top, ranks)
		out[key] = top
	}
	return out
}

func sortExtTopByRank(ids []uint32, ranks []uint32) {
	sort.Slice(ids, func(i, j int) bool {
		a, b := ids[i], ids[j]
		ra, rb := extRankOf(a, ranks), extRankOf(b, ranks)
		if ra == rb {
			return a < b
		}
		return ra < rb
	})
}

func extRankOf(id uint32, ranks []uint32) uint32 {
	if int(id) >= len(ranks) {
		return id
	}
	return ranks[id]
}

func extRankLess(a, b extRankItem) bool {
	if a.rank == b.rank {
		return a.id < b.id
	}
	return a.rank < b.rank
}

type extRankItem struct {
	id   uint32
	rank uint32
}

type extRankMaxHeap []extRankItem

func (h extRankMaxHeap) Len() int { return len(h) }

func (h extRankMaxHeap) Less(i, j int) bool {
	if h[i].rank == h[j].rank {
		return h[i].id > h[j].id
	}
	return h[i].rank > h[j].rank
}

func (h extRankMaxHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *extRankMaxHeap) Push(x any) {
	*h = append(*h, x.(extRankItem))
}

func (h *extRankMaxHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

func (vol *serviceVolumeIndex) nameOrderStateString() string {
	if vol == nil {
		return ""
	}
	switch vol.nameOrderState.Load() {
	case nameTrigramStatePending:
		return "pending"
	case nameTrigramStateBuilding:
		return "building"
	case nameTrigramStateReady:
		return "ready"
	default:
		if serviceNameOrderEnabled() {
			return "disabled"
		}
		return ""
	}
}

func (vol *serviceVolumeIndex) nameTrigramIndex() *compressedTrigramIndex {
	if vol == nil {
		return nil
	}
	return vol.nameTrigrams.Load()
}

func (vol *serviceVolumeIndex) nameQuadgramIndex() *compressedTrigramIndex {
	if vol == nil {
		return nil
	}
	return vol.nameQuadgrams.Load()
}

func (vol *serviceVolumeIndex) markNameTrigramRecent(id int) {
	if vol == nil || id < 0 || !serviceNameTrigramsEnabledForIndex(vol.index) {
		return
	}
	if vol.nameTrigramRecent == nil {
		vol.nameTrigramRecent = make(map[int]struct{})
	}
	vol.nameTrigramRecent[id] = struct{}{}
}

func (vol *serviceVolumeIndex) resetNameTrigrams() {
	if vol == nil {
		return
	}
	if vol.index != nil && vol.index.Derived.NameTrigrams != nil {
		vol.nameTrigrams.Store(vol.index.Derived.NameTrigrams)
		vol.nameQuadgrams.Store(nil)
		vol.nameTrigramMillis.Store(0)
		vol.nameTrigramRecent = nil
		vol.nameTrigramState.Store(nameTrigramStateReady)
		return
	}
	vol.nameTrigrams.Store(nil)
	vol.nameQuadgrams.Store(nil)
	vol.nameTrigramMillis.Store(0)
	vol.nameTrigramRecent = nil
	if serviceNameTrigramsEnabledForIndex(vol.index) {
		vol.nameTrigramState.Store(nameTrigramStatePending)
	} else {
		vol.nameTrigramState.Store(nameTrigramStateDisabled)
	}
}

func (vol *serviceVolumeIndex) rebuildNameTrigramsLocked() {
	if vol == nil || vol.index == nil || !serviceNameTrigramsEnabledForIndex(vol.index) {
		vol.resetNameTrigrams()
		return
	}
	start := time.Now()
	var ti *compressedTrigramIndex
	if serviceLowMemoryMode() {
		ti = buildSelectiveNameTrigramIndex(vol.index, serviceLowMemoryTrigramStoredPostingMax())
	} else {
		ti = buildNameTrigramIndex(vol.index)
		ti.dropCommonPostings(trigramStoredPostingMaxCount)
	}
	vol.searchMu.Lock()
	vol.nameTrigrams.Store(ti)
	vol.nameQuadgrams.Store(nil)
	vol.nameTrigramRecent = nil
	vol.nameTrigramMillis.Store(time.Since(start).Milliseconds())
	vol.nameTrigramState.Store(nameTrigramStateReady)
	vol.searchMu.Unlock()
	serviceLog("built name trigram index volume=%s records=%d keys=%d bytes=%d elapsed=%s",
		vol.volume, vol.index.compactRecordCount(), ti.keyCount(), ti.postingBytes, time.Since(start).Round(time.Millisecond))
}

func (vol *serviceVolumeIndex) nameTrigramStateString() string {
	if vol == nil {
		return ""
	}
	switch vol.nameTrigramState.Load() {
	case nameTrigramStatePending:
		return "pending"
	case nameTrigramStateBuilding:
		return "building"
	case nameTrigramStateReady:
		return "ready"
	default:
		if serviceNameTrigramsEnabled() {
			return "disabled"
		}
		return ""
	}
}

func (vol *serviceVolumeIndex) childIDsForRecord(id int) []uint32 {
	if vol == nil || id < 0 {
		return nil
	}
	if len(vol.childOffsets) > id+1 {
		start, end := vol.childOffsets[id], vol.childOffsets[id+1]
		if start <= end && int(end) <= len(vol.childIDs) {
			return vol.childIDs[start:end]
		}
	}
	if vol.children == nil || vol.index == nil || id >= vol.index.compactRecordCount() {
		return nil
	}
	rec := vol.index.compactRecord(id)
	kids := vol.children[rec.FRN]
	if len(kids) == 0 {
		return nil
	}
	out := make([]uint32, 0, len(kids))
	for childID := range kids {
		if childID >= 0 {
			out = append(out, uint32(childID))
		}
	}
	return out
}

func buildResidentQueryIndex(vol *serviceVolumeIndex) *residentQueryIndex {
	return buildResidentQueryIndexMode(vol, false)
}

func buildResidentQueryIndexForPersistence(vol *serviceVolumeIndex) *residentQueryIndex {
	return buildResidentQueryIndexMode(vol, true)
}

func buildResidentQueryIndexMode(vol *serviceVolumeIndex, persistence bool) *residentQueryIndex {
	recordCount := vol.index.compactRecordCount()
	hasMappedExt := false
	hasMappedComponents := false
	if postings := vol.index.Derived.Postings; postings != nil {
		if section := postings[indexSectionPEXT]; len(section.Data) > 0 {
			hasMappedExt = true
		}
		if section := postings[indexSectionPCMP]; len(section.Data) > 0 {
			hasMappedComponents = true
		}
	}
	mappedLowmem := serviceLowMemoryMode() && vol.index.MMapRecords != nil &&
		len(vol.index.Derived.NameOrder) > 0 && len(vol.index.Derived.NameRank) > 0 &&
		hasMappedExt && hasMappedComponents
	qi := &residentQueryIndex{}
	if !hasMappedExt {
		qi.ext = make(map[string][]uint32)
	}
	if !hasMappedComponents {
		qi.components = make(map[string][]uint32)
	}
	if !mappedLowmem && !persistence {
		qi.dirs = make([]uint32, 0, recordCount/8)
		qi.dirsReady = true
	}
	sortAttrBits := false
	if vol.index.compactHasAttrs() && !mappedLowmem {
		qi.attrBits = make(map[uint32][]uint32, 5)
		sortAttrBits = true
	}
	if vol.index.compactHasAttrs() && mappedLowmem && len(vol.index.Derived.AttrBits) > 0 {
		qi.attrBits = vol.index.Derived.AttrBits
	}
	if servicePathGramsEnabled() && !mappedLowmem && !persistence {
		qi.pathGrams = make(map[string][]uint32)
	}
	if !mappedLowmem {
		for id := 0; id < recordCount; id++ {
			rec := vol.index.compactRecord(id)
			if rec.Deleted {
				continue
			}
			name := vol.index.compactLowerNameAt(id)
			if rec.Mode&uint32(os.ModeDir) != 0 {
				qi.dirs = append(qi.dirs, uint32(id))
				if !hasMappedComponents && name != "" && name != "." {
					qi.components[name] = append(qi.components[name], uint32(id))
				}
				if qi.pathGrams != nil && name != "" && name != "." {
					for _, gram := range componentGrams(name) {
						qi.pathGrams[gram] = append(qi.pathGrams[gram], uint32(id))
					}
				}
			}
			if !hasMappedExt {
				actualExt := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
				if actualExt != "" {
					ext := strings.ToLower(actualExt)
					qi.ext[ext] = append(qi.ext[ext], uint32(id))
				}
			}
			if qi.attrBits != nil {
				for _, bit := range queryAttrBits() {
					if rec.Mode&bit == bit {
						qi.attrBits[bit] = append(qi.attrBits[bit], uint32(id))
					}
				}
			}
		}
	}
	sortResidentPostings(qi.ext)
	if qi.pathGrams != nil {
		sortResidentPostings(qi.pathGrams)
	}
	sortResidentPostings(qi.components)
	if sortAttrBits {
		sortResidentAttrPostings(qi.attrBits)
	}
	sortUint32s(qi.dirs)
	if len(vol.index.Derived.NameOrder) > 0 && len(vol.index.Derived.NameRank) > 0 {
		qi.nameOrder, qi.nameRank = vol.index.Derived.NameOrder, vol.index.Derived.NameRank
		qi.sizeOrder, qi.sizeRank = vol.index.Derived.SizeOrder, vol.index.Derived.SizeRank
		qi.modOrder, qi.modRank = vol.index.Derived.ModOrder, vol.index.Derived.ModRank
		qi.extOrder, qi.extRank = vol.index.Derived.ExtOrder, vol.index.Derived.ExtRank
		qi.typeOrder, qi.typeRank = vol.index.Derived.TypeOrder, vol.index.Derived.TypeRank
		qi.pathOrder, qi.pathRank = vol.index.Derived.PathOrder, vol.index.Derived.PathRank
		if !persistence {
			qi.extTop = buildExtTopPostings(qi.ext, qi.nameRank, serviceExtTopPostingLimit)
		}
	} else if !persistence && serviceNameOrderEnabled() && recordCount <= serviceResidentNameOrderMaxRecords {
		qi.nameOrder, qi.nameRank = buildCompactNameOrderRank(vol.index)
		if vol.index.compactHasSize() {
			qi.sizeOrder, qi.sizeRank = buildCompactSizeOrderRank(vol.index)
		}
		if vol.index.compactHasModTime() {
			qi.modOrder, qi.modRank = buildCompactModifiedOrderRank(vol.index)
		}
		qi.extOrder, qi.extRank = buildCompactExtensionOrderRank(vol.index)
		qi.typeOrder, qi.typeRank = buildCompactTypeOrderRank(vol.index)
		qi.pathOrder, qi.pathRank = buildCompactPathOrderRank(vol.index)
		if !persistence {
			qi.extTop = buildExtTopPostings(qi.ext, qi.nameRank, serviceExtTopPostingLimit)
		}
	} else if !persistence {
		qi.extTop = buildExtTopPostingsMin(qi.ext, nil, serviceExtTopPostingLimit, serviceExtTopPostingLimit)
	}
	return qi
}

func (vol *serviceVolumeIndex) clearSearchCachesLocked() {
	vol.pathCache = make(map[int]string)
	vol.termCache = nil
	vol.pathTermCache = nil
	vol.extCache = nil
	vol.underCache = nil
	vol.underRootCache = nil
}

func (vol *serviceVolumeIndex) trimSearchCachesLocked() {
	if len(vol.pathCache) > servicePathCacheLimit {
		vol.pathCache = make(map[int]string)
	}
	vol.termMu.Lock()
	defer vol.termMu.Unlock()
	if vol.postingListCacheBytesLocked() > postingListCacheMaxBytes() {
		vol.termCache = nil
		vol.pathTermCache = nil
		vol.extCache = nil
		vol.underCache = nil
		vol.underRootCache = nil
	}
	vol.searchCount++
}

func (vol *serviceVolumeIndex) residentMemoryInfo() *residentMemoryInfo {
	if vol == nil || vol.index == nil {
		return nil
	}
	recordCount := vol.index.compactRecordCount()
	info := &residentMemoryInfo{Records: recordCount}
	if p := vol.index.PackedRecords; p != nil {
		info.NameBlobBytes = len(p.NameBlob)
		info.LowerBlobBytes = len(p.LowerBlob)
		info.RecordBytes = int64(len(p.FRNs))*8 +
			int64(len(p.ParentFRNExtras))*16 +
			int64(len(p.Parents))*4 +
			int64(len(p.NameOffs))*4 +
			int64(len(p.NameLens))*2 +
			int64(len(p.LowerOffs))*4 +
			int64(len(p.DirBits))*8 +
			int64(len(p.ModeExtraIDs))*4 +
			int64(len(p.ModeExtraValues))*4 +
			int64(len(p.Size32))*4 +
			int64(len(p.Size64IDs))*4 +
			int64(len(p.Size64Values))*8 +
			int64(len(p.ModUnix))*8 +
			int64(len(p.DeletedBits))*8
	}
	if m := vol.index.MMapRecords; m != nil {
		info.MMapRecordBytes = int64(len(m.nameBlob)) + int64(len(m.tokenTable)) + int64(len(m.recordData))
	}
	if vol.queryIndex != nil {
		info.NameOrderBytes = (len(vol.queryIndex.nameOrder) + len(vol.queryIndex.nameRank)) * 4
		info.TypePostBytes = len(vol.queryIndex.dirs) * 4
		for _, list := range vol.queryIndex.ext {
			info.ExtPostBytes += len(list) * 4
		}
		for _, list := range vol.queryIndex.extTop {
			info.ExtPostBytes += len(list) * 4
		}
		for _, list := range vol.queryIndex.pathGrams {
			info.ExtPostBytes += len(list) * 4
		}
		for _, list := range vol.queryIndex.components {
			info.TypePostBytes += len(list) * 4
		}
	}
	if trigrams := vol.nameTrigramIndex(); trigrams != nil {
		info.NameTrigramBytes = trigrams.postingBytes
		info.NameTrigramKeys = trigrams.keyCount()
	}
	info.ChildBytes = (len(vol.childOffsets) + len(vol.childIDs) + len(vol.rootIDs) + len(vol.subtreeOrder) + len(vol.subtreeStart) + len(vol.subtreeEnd)) * 4
	info.FRNIndexBytes = len(vol.frns)*8 + len(vol.frnRecordIDs)*4
	info.FRNOverlayEntries = len(vol.frnToID)
	info.KnownBytes = int64(info.NameBlobBytes) +
		int64(info.LowerBlobBytes) +
		info.RecordBytes +
		info.MMapRecordBytes +
		int64(info.NameOrderBytes) +
		int64(info.ExtPostBytes) +
		int64(info.NameTrigramBytes) +
		int64(info.TypePostBytes) +
		int64(info.ChildBytes) +
		int64(info.FRNIndexBytes)
	return info
}

func runtimeMemorySnapshot() *runtimeMemoryInfo {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return &runtimeMemoryInfo{
		HeapAllocBytes:    m.HeapAlloc,
		HeapInuseBytes:    m.HeapInuse,
		HeapIdleBytes:     m.HeapIdle,
		HeapReleasedBytes: m.HeapReleased,
		HeapSysBytes:      m.HeapSys,
		StackInuseBytes:   m.StackInuse,
		SysBytes:          m.Sys,
		NumGC:             m.NumGC,
		GCCPUFraction:     m.GCCPUFraction,
	}
}

func sortResidentPostings(postings map[string][]uint32) {
	for key, list := range postings {
		sortUint32s(list)
		postings[key] = uniqueSortedUint32s(list)
	}
}

func sortResidentAttrPostings(postings map[uint32][]uint32) {
	for key, list := range postings {
		sortUint32s(list)
		postings[key] = uniqueSortedUint32s(list)
	}
}

func queryAttrBits() []uint32 {
	return []uint32{
		fileAttributeReadonly,
		fileAttributeHidden,
		fileAttributeSystem,
		fileAttributeDir,
		fileAttributeArchive,
	}
}

func componentGrams(s string) []string {
	return fixedGrams(s, 3)
}

func fixedGrams(s string, n int) []string {
	if n <= 0 {
		return nil
	}
	if len(s) < n {
		return nil
	}
	seen := make(map[string]struct{}, len(s))
	out := make([]string, 0, len(s)-n+1)
	for i := 0; i+n <= len(s); i++ {
		gram := s[i : i+n]
		if _, ok := seen[gram]; ok {
			continue
		}
		seen[gram] = struct{}{}
		out = append(out, gram)
	}
	return out
}

func compactLowerName(rec CompactRecord) string {
	return strings.ToLower(rec.Name)
}

func newPackedRecords(records []CompactRecord) *PackedRecords {
	if len(records) == 0 {
		return nil
	}
	hasSize := false
	hasModUnix := false
	for _, rec := range records {
		if rec.Size != 0 {
			hasSize = true
		}
		if rec.ModUnix != 0 {
			hasModUnix = true
		}
	}
	p := &PackedRecords{
		FRNs:        make([]uint64, len(records)),
		Parents:     make([]int32, len(records)),
		NameOffs:    make([]uint32, len(records)),
		NameLens:    make([]uint16, len(records)),
		LowerOffs:   make([]uint32, len(records)),
		DirBits:     make([]uint64, (len(records)+63)/64),
		DeletedBits: make([]uint64, (len(records)+63)/64),
		NameBlob:    make([]byte, 0, len(records)*16),
		LowerBlob:   make([]byte, 0, len(records)*16),
	}
	if hasSize {
		p.Size32 = make([]uint32, len(records))
	}
	if hasModUnix {
		p.ModUnix = make([]int64, len(records))
	}
	nameRefs := make(map[string]struct {
		off uint32
		len uint16
	}, len(records)/2)
	lowerRefs := make(map[string]struct {
		off uint32
		len uint16
	}, len(records)/2)
	for i, rec := range records {
		p.FRNs[i] = rec.FRN
		p.Parents[i] = rec.Parent
		p.setNameDedup(i, rec.Name, nameRefs)
		p.setLowerNameDedup(i, rec.Name, lowerRefs)
		p.setMode(i, rec.Mode)
		if p.Size32 != nil {
			p.setSize(i, rec.Size)
		}
		if p.ModUnix != nil {
			p.ModUnix[i] = rec.ModUnix
		}
		p.setDeleted(i, rec.Deleted)
	}
	for i, rec := range records {
		p.setParentFRN(i, rec.ParentFRN)
	}
	return p
}

func (idx *Index) packCompactRecords(dropRecords bool) {
	if idx == nil || !idx.Compact || idx.PackedRecords != nil || idx.MMapRecords != nil {
		return
	}
	idx.PackedRecords = newPackedRecords(idx.Records)
	if dropRecords {
		idx.Records = nil
		idx.CompactNameOrder = nil
	}
}

func (idx *Index) repackCompactRecords() {
	if idx == nil || !idx.Compact || idx.PackedRecords == nil {
		return
	}
	count := idx.PackedRecords.Len()
	records := make([]CompactRecord, count)
	for i := range records {
		records[i] = idx.PackedRecords.At(i)
	}
	idx.PackedRecords = newPackedRecords(records)
	idx.Records = nil
	idx.CompactNameOrder = nil
}

func (idx *Index) packedNameBlobLooksBloated() bool {
	if idx == nil || idx.PackedRecords == nil {
		return false
	}
	recordCount := idx.PackedRecords.Len()
	if recordCount == 0 {
		return false
	}
	limit := recordCount * 64
	if limit < 512*1024*1024 {
		limit = 512 * 1024 * 1024
	}
	return len(idx.PackedRecords.NameBlob) > limit
}

func (p *PackedRecords) Len() int {
	if p == nil {
		return 0
	}
	return len(p.FRNs)
}

func (m *MMapRecords) Len() int {
	if m == nil {
		return 0
	}
	return m.count
}

func (m *MMapRecords) At(i int) CompactRecord {
	if m == nil || i < 0 || i >= m.count {
		return CompactRecord{}
	}
	base, ok := m.recordOffset(i)
	if !ok {
		return CompactRecord{}
	}
	refBytes := 6
	if m.wideRefs {
		refBytes = 8
	}
	parent, nameID := m.recordRefs(base + 16)
	modeOff := base + 16 + refBytes
	sizeOff := modeOff + 4
	modOff := sizeOff + 8
	delOff := modOff + 8
	rec := CompactRecord{
		FRN:       binary.LittleEndian.Uint64(m.recordData[base:]),
		ParentFRN: binary.LittleEndian.Uint64(m.recordData[base+8:]),
		NameOff:   nameID,
		Mode:      binary.LittleEndian.Uint32(m.recordData[modeOff:]),
		Size:      int64(binary.LittleEndian.Uint64(m.recordData[sizeOff:])),
		ModUnix:   int64(binary.LittleEndian.Uint64(m.recordData[modOff:])),
		Deleted:   m.recordData[delOff] != 0,
	}
	if (!m.wideRefs && parent == compactNarrowParentSentinel) || (m.wideRefs && parent == compactWideParentSentinel) {
		rec.Parent = -1
	} else {
		rec.Parent = int32(parent)
	}
	rec.Name, rec.NameLen = m.nameByID(nameID)
	return rec
}

func (m *MMapRecords) lowerNameAt(i int) string {
	if m != nil && i >= 0 && i < m.count {
		derived := m.fileDerived()
		if len(derived.LowerOffs) > 0 && len(derived.LowerLens) == len(derived.LowerOffs) {
			base, ok := m.recordOffset(i)
			if ok {
				_, nameID := m.recordRefs(base + 16)
				token := int(nameID)
				if token >= 0 && token < len(derived.LowerOffs) {
					off := derived.LowerOffs[token]
					if off == packedLowerSameAsName {
						return m.nameAtRecord(i)
					}
					length := derived.LowerLens[token]
					end := int(off) + int(length)
					if end >= int(off) && end <= len(derived.LowerBlob) {
						return stringView(derived.LowerBlob[int(off):end])
					}
				}
			}
		}
	}
	name := m.nameAtRecord(i)
	if name == "" {
		return ""
	}
	return strings.ToLower(name)
}

func (m *MMapRecords) nameAtRecord(i int) string {
	name, _ := m.nameAtRecordWithLen(i)
	return name
}

func (m *MMapRecords) nameAtRecordWithLen(i int) (string, uint16) {
	if m == nil || i < 0 || i >= m.count {
		return "", 0
	}
	base, ok := m.recordOffset(i)
	if !ok {
		return "", 0
	}
	_, nameID := m.recordRefs(base + 16)
	return m.nameByID(nameID)
}

func (m *MMapRecords) fileDerived() indexDerivedSections {
	if m == nil || m.file == nil {
		return indexDerivedSections{}
	}
	return m.file.derived
}

func (m *MMapRecords) recordOffset(i int) (int, bool) {
	if m == nil || i < 0 || i >= m.count {
		return 0, false
	}
	size := compactDiskRecordBytes
	if m.wideRefs {
		size = compactWideDiskRecordBytes
	}
	base := i * size
	if base < 0 || base+size > len(m.recordData) {
		return 0, false
	}
	return base, true
}

func (m *MMapRecords) recordRefs(off int) (uint32, uint32) {
	if m.wideRefs {
		if off+8 > len(m.recordData) {
			return compactWideParentSentinel, 0
		}
		return binary.LittleEndian.Uint32(m.recordData[off:]), binary.LittleEndian.Uint32(m.recordData[off+4:])
	}
	if off+6 > len(m.recordData) {
		return compactNarrowParentSentinel, 0
	}
	parent := uint32(m.recordData[off]) | uint32(m.recordData[off+1])<<8 | uint32(m.recordData[off+2])<<16
	nameID := uint32(m.recordData[off+3]) | uint32(m.recordData[off+4])<<8 | uint32(m.recordData[off+5])<<16
	return parent, nameID
}

func (m *MMapRecords) nameByID(nameID uint32) (string, uint16) {
	if m == nil {
		return "", 0
	}
	token := int(nameID) * 6
	if token < 0 || token+6 > len(m.tokenTable) {
		return "", 0
	}
	off := binary.LittleEndian.Uint32(m.tokenTable[token:])
	length := binary.LittleEndian.Uint16(m.tokenTable[token+4:])
	end := int(off) + int(length)
	if end < int(off) || end > len(m.nameBlob) {
		return "", 0
	}
	return stringView(m.nameBlob[int(off):end]), length
}

func (m *MMapRecords) scanCapabilities() {
	if m == nil {
		return
	}
	refBytes := 6
	if m.wideRefs {
		refBytes = 8
	}
	for i := 0; i < m.count; i++ {
		base, ok := m.recordOffset(i)
		if !ok {
			return
		}
		sizeOff := base + 16 + refBytes + 4
		modOff := sizeOff + 8
		if binary.LittleEndian.Uint64(m.recordData[sizeOff:]) != 0 {
			m.hasSize = true
		}
		if binary.LittleEndian.Uint64(m.recordData[modOff:]) != 0 {
			m.hasModUnix = true
		}
		if m.hasSize && m.hasModUnix {
			return
		}
	}
}

func (p *PackedRecords) At(i int) CompactRecord {
	if p == nil || i < 0 || i >= len(p.FRNs) {
		return CompactRecord{}
	}
	name := p.nameAt(i)
	return CompactRecord{
		FRN:       p.FRNs[i],
		ParentFRN: p.parentFRNAt(i),
		Parent:    p.Parents[i],
		Name:      name,
		NameOff:   p.NameOffs[i],
		NameLen:   p.NameLens[i],
		Mode:      p.modeAt(i),
		Size:      p.sizeAt(i),
		ModUnix:   p.modUnixAt(i),
		Deleted:   p.deletedAt(i),
	}
}

func (p *PackedRecords) Set(i int, rec CompactRecord) {
	if p == nil || i < 0 || i >= len(p.FRNs) {
		return
	}
	p.FRNs[i] = rec.FRN
	p.Parents[i] = rec.Parent
	p.setParentFRN(i, rec.ParentFRN)
	if rec.Name != p.nameAt(i) {
		p.setName(i, rec.Name)
		p.setLowerName(i, rec.Name)
	}
	p.setMode(i, rec.Mode)
	p.setSize(i, rec.Size)
	p.setModUnix(i, rec.ModUnix)
	p.setDeleted(i, rec.Deleted)
}

func (p *PackedRecords) Append(rec CompactRecord) {
	if p == nil {
		return
	}
	p.FRNs = append(p.FRNs, rec.FRN)
	p.Parents = append(p.Parents, rec.Parent)
	p.NameOffs = append(p.NameOffs, 0)
	p.NameLens = append(p.NameLens, 0)
	p.LowerOffs = append(p.LowerOffs, 0)
	if need := (len(p.FRNs) + 63) / 64; len(p.DirBits) < need {
		p.DirBits = append(p.DirBits, 0)
	}
	if p.Size32 != nil {
		p.Size32 = append(p.Size32, 0)
		p.setSize(len(p.FRNs)-1, rec.Size)
	} else if rec.Size != 0 {
		p.Size32 = make([]uint32, len(p.FRNs))
		p.setSize(len(p.FRNs)-1, rec.Size)
	}
	if p.ModUnix != nil {
		p.ModUnix = append(p.ModUnix, rec.ModUnix)
	} else if rec.ModUnix != 0 {
		p.ModUnix = make([]int64, len(p.FRNs))
		p.ModUnix[len(p.ModUnix)-1] = rec.ModUnix
	}
	if need := (len(p.FRNs) + 63) / 64; len(p.DeletedBits) < need {
		p.DeletedBits = append(p.DeletedBits, 0)
	}
	p.setName(len(p.FRNs)-1, rec.Name)
	p.setLowerName(len(p.FRNs)-1, rec.Name)
	p.setParentFRN(len(p.FRNs)-1, rec.ParentFRN)
	p.setDeleted(len(p.FRNs)-1, rec.Deleted)
	p.setMode(len(p.FRNs)-1, rec.Mode)
}

func (p *PackedRecords) parentFRNAt(i int) uint64 {
	if p == nil || i < 0 || i >= len(p.FRNs) {
		return 0
	}
	if value, ok := p.ParentFRNExtras[i]; ok {
		return value
	}
	parent := int(p.Parents[i])
	if parent >= 0 && parent < len(p.FRNs) {
		return p.FRNs[parent]
	}
	return p.FRNs[i]
}

func (p *PackedRecords) setParentFRN(i int, parentFRN uint64) {
	if p == nil || i < 0 || i >= len(p.FRNs) {
		return
	}
	parent := int(p.Parents[i])
	derived := uint64(0)
	if parent >= 0 && parent < len(p.FRNs) {
		derived = p.FRNs[parent]
	} else {
		derived = p.FRNs[i]
	}
	if parentFRN == derived {
		if p.ParentFRNExtras != nil {
			delete(p.ParentFRNExtras, i)
		}
		return
	}
	if p.ParentFRNExtras == nil {
		p.ParentFRNExtras = make(map[int]uint64, 4)
	}
	p.ParentFRNExtras[i] = parentFRN
}

func (p *PackedRecords) sizeAt(i int) int64 {
	if p == nil || i < 0 || i >= len(p.Size32) {
		return 0
	}
	value := p.Size32[i]
	if value != packedSize64Sentinel {
		return int64(value)
	}
	j := sort.Search(len(p.Size64IDs), func(j int) bool { return p.Size64IDs[j] >= uint32(i) })
	if j < len(p.Size64IDs) && p.Size64IDs[j] == uint32(i) && j < len(p.Size64Values) {
		return p.Size64Values[j]
	}
	return 0
}

func (p *PackedRecords) modUnixAt(i int) int64 {
	if p == nil || i < 0 || i >= len(p.ModUnix) {
		return 0
	}
	return p.ModUnix[i]
}

func (p *PackedRecords) deletedAt(i int) bool {
	if p == nil || i < 0 {
		return false
	}
	word := i >> 6
	if word >= len(p.DeletedBits) {
		return false
	}
	return p.DeletedBits[word]&(uint64(1)<<uint(i&63)) != 0
}

func (p *PackedRecords) setDeleted(i int, value bool) {
	if p == nil || i < 0 || i >= len(p.FRNs) {
		return
	}
	word := i >> 6
	if word >= len(p.DeletedBits) {
		next := make([]uint64, word+1)
		copy(next, p.DeletedBits)
		p.DeletedBits = next
	}
	mask := uint64(1) << uint(i&63)
	if value {
		p.DeletedBits[word] |= mask
		return
	}
	p.DeletedBits[word] &^= mask
}

func (p *PackedRecords) modeAt(i int) uint32 {
	if p == nil || i < 0 {
		return 0
	}
	id := uint32(i)
	j := sort.Search(len(p.ModeExtraIDs), func(j int) bool { return p.ModeExtraIDs[j] >= id })
	if j < len(p.ModeExtraIDs) && p.ModeExtraIDs[j] == id && j < len(p.ModeExtraValues) {
		return p.ModeExtraValues[j]
	}
	word := i >> 6
	if word >= len(p.DirBits) {
		return 0
	}
	if p.DirBits[word]&(uint64(1)<<uint(i&63)) != 0 {
		return uint32(os.ModeDir)
	}
	return 0
}

func (p *PackedRecords) setMode(i int, mode uint32) {
	if p == nil || i < 0 || i >= len(p.FRNs) {
		return
	}
	word := i >> 6
	if word >= len(p.DirBits) {
		next := make([]uint64, word+1)
		copy(next, p.DirBits)
		p.DirBits = next
	}
	mask := uint64(1) << uint(i&63)
	if mode&uint32(os.ModeDir) != 0 {
		p.DirBits[word] |= mask
	} else {
		p.DirBits[word] &^= mask
	}
	if mode == 0 || mode == uint32(os.ModeDir) {
		p.removeModeExtra(i)
		return
	}
	p.upsertModeExtra(i, mode)
}

func (p *PackedRecords) upsertModeExtra(i int, value uint32) {
	id := uint32(i)
	j := sort.Search(len(p.ModeExtraIDs), func(j int) bool { return p.ModeExtraIDs[j] >= id })
	if j < len(p.ModeExtraIDs) && p.ModeExtraIDs[j] == id {
		p.ModeExtraValues[j] = value
		return
	}
	p.ModeExtraIDs = append(p.ModeExtraIDs, 0)
	copy(p.ModeExtraIDs[j+1:], p.ModeExtraIDs[j:])
	p.ModeExtraIDs[j] = id
	p.ModeExtraValues = append(p.ModeExtraValues, 0)
	copy(p.ModeExtraValues[j+1:], p.ModeExtraValues[j:])
	p.ModeExtraValues[j] = value
}

func (p *PackedRecords) removeModeExtra(i int) {
	id := uint32(i)
	j := sort.Search(len(p.ModeExtraIDs), func(j int) bool { return p.ModeExtraIDs[j] >= id })
	if j >= len(p.ModeExtraIDs) || p.ModeExtraIDs[j] != id {
		return
	}
	copy(p.ModeExtraIDs[j:], p.ModeExtraIDs[j+1:])
	p.ModeExtraIDs = p.ModeExtraIDs[:len(p.ModeExtraIDs)-1]
	copy(p.ModeExtraValues[j:], p.ModeExtraValues[j+1:])
	p.ModeExtraValues = p.ModeExtraValues[:len(p.ModeExtraValues)-1]
}

func (p *PackedRecords) setSize(i int, value int64) {
	if p == nil || i < 0 || i >= len(p.FRNs) {
		return
	}
	if p.Size32 == nil {
		if value == 0 {
			return
		}
		p.Size32 = make([]uint32, len(p.FRNs))
	}
	if value >= 0 && value < int64(packedSize64Sentinel) {
		p.Size32[i] = uint32(value)
		return
	}
	p.Size32[i] = packedSize64Sentinel
	p.upsertSize64(i, value)
}

func (p *PackedRecords) upsertSize64(i int, value int64) {
	id := uint32(i)
	j := sort.Search(len(p.Size64IDs), func(j int) bool { return p.Size64IDs[j] >= id })
	if j < len(p.Size64IDs) && p.Size64IDs[j] == id {
		p.Size64Values[j] = value
		return
	}
	p.Size64IDs = append(p.Size64IDs, 0)
	copy(p.Size64IDs[j+1:], p.Size64IDs[j:])
	p.Size64IDs[j] = id
	p.Size64Values = append(p.Size64Values, 0)
	copy(p.Size64Values[j+1:], p.Size64Values[j:])
	p.Size64Values[j] = value
}

func (p *PackedRecords) setModUnix(i int, value int64) {
	if p == nil || i < 0 || i >= len(p.FRNs) {
		return
	}
	if p.ModUnix == nil {
		if value == 0 {
			return
		}
		p.ModUnix = make([]int64, len(p.FRNs))
	}
	p.ModUnix[i] = value
}

func (p *PackedRecords) nameAt(i int) string {
	if p == nil || i < 0 || i >= len(p.NameOffs) {
		return ""
	}
	off := p.NameOffs[i]
	length := p.NameLens[i]
	end := int(off) + int(length)
	if end < int(off) || end > len(p.NameBlob) {
		return ""
	}
	return stringView(p.NameBlob[int(off):end])
}

func (p *PackedRecords) lowerNameAt(i int) string {
	if p == nil || i < 0 || i >= len(p.LowerOffs) {
		return ""
	}
	off := p.LowerOffs[i]
	if off == packedLowerSameAsName {
		return p.nameAt(i)
	}
	length := p.NameLens[i]
	end := int(off) + int(length)
	if end < int(off) || end > len(p.LowerBlob) {
		return strings.ToLower(p.nameAt(i))
	}
	return stringView(p.LowerBlob[int(off):end])
}

func (p *PackedRecords) setName(i int, name string) {
	if p == nil || i < 0 || i >= len(p.NameOffs) {
		return
	}
	if len(name) > int(^uint16(0)) {
		name = name[:int(^uint16(0))]
	}
	p.NameOffs[i] = uint32(len(p.NameBlob))
	p.NameLens[i] = uint16(len(name))
	p.NameBlob = append(p.NameBlob, name...)
}

func (p *PackedRecords) setLowerName(i int, name string) {
	if p == nil || i < 0 || i >= len(p.LowerOffs) {
		return
	}
	lower := strings.ToLower(name)
	if len(lower) > int(^uint16(0)) {
		lower = lower[:int(^uint16(0))]
	}
	if lower == name {
		p.LowerOffs[i] = packedLowerSameAsName
		return
	}
	p.LowerOffs[i] = uint32(len(p.LowerBlob))
	p.LowerBlob = append(p.LowerBlob, lower...)
}

func (p *PackedRecords) setNameDedup(i int, name string, refs map[string]struct {
	off uint32
	len uint16
}) {
	if p == nil || i < 0 || i >= len(p.NameOffs) {
		return
	}
	if len(name) > int(^uint16(0)) {
		name = name[:int(^uint16(0))]
	}
	if ref, ok := refs[name]; ok {
		p.NameOffs[i] = ref.off
		p.NameLens[i] = ref.len
		return
	}
	ref := struct {
		off uint32
		len uint16
	}{off: uint32(len(p.NameBlob)), len: uint16(len(name))}
	refs[name] = ref
	p.NameOffs[i] = ref.off
	p.NameLens[i] = ref.len
	p.NameBlob = append(p.NameBlob, name...)
}

func (p *PackedRecords) setLowerNameDedup(i int, name string, refs map[string]struct {
	off uint32
	len uint16
}) {
	if p == nil || i < 0 || i >= len(p.LowerOffs) {
		return
	}
	lower := strings.ToLower(name)
	if len(lower) > int(^uint16(0)) {
		lower = lower[:int(^uint16(0))]
	}
	if lower == name {
		p.LowerOffs[i] = packedLowerSameAsName
		return
	}
	if ref, ok := refs[lower]; ok {
		p.LowerOffs[i] = ref.off
		return
	}
	ref := struct {
		off uint32
		len uint16
	}{off: uint32(len(p.LowerBlob)), len: uint16(len(lower))}
	refs[lower] = ref
	p.LowerOffs[i] = ref.off
	p.LowerBlob = append(p.LowerBlob, lower...)
}

func stringView(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}

func (vol *serviceVolumeIndex) markDeleted(id int) {
	if id < 0 || id >= vol.index.compactRecordCount() {
		return
	}
	if vol.children == nil {
		rec := vol.index.compactRecord(id)
		rec.Deleted = true
		vol.index.setCompactRecord(id, rec)
		return
	}
	stack := []int{id}
	for len(stack) > 0 {
		last := len(stack) - 1
		cur := stack[last]
		stack = stack[:last]
		if cur < 0 || cur >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(cur)
		if rec.Deleted {
			continue
		}
		rec.Deleted = true
		vol.index.setCompactRecord(cur, rec)
		for _, childID := range vol.childIDsForRecord(cur) {
			stack = append(stack, int(childID))
		}
	}
}

func (s *goSearchService) servePrivileged() {
	var wg sync.WaitGroup
	const listeners = 8
	for i := 0; i < listeners; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.servePipeListener()
		}()
	}
	<-s.stop
	wg.Wait()
}

func (s *goSearchService) servePipeListener() {
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		file, err := s.createPipeInstance()
		if err != nil {
			return
		}
		go handleServiceConn(file, s)
	}
}

func (s *goSearchService) createPipeInstance() (*os.File, error) {
	ptr, err := windows.UTF16PtrFromString(s.pipeName)
	if err != nil {
		return nil, err
	}
	sa, err := securityAttributesFromSDDL(s.sddl)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateNamedPipe(
		ptr,
		windows.PIPE_ACCESS_DUPLEX,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT,
		windows.PIPE_UNLIMITED_INSTANCES,
		64*1024,
		64*1024,
		0,
		sa,
	)
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-s.stop:
			_ = windows.CloseHandle(handle)
		case <-done:
		}
	}()
	err = windows.ConnectNamedPipe(handle, nil)
	close(done)
	if err == nil || err == windows.ERROR_PIPE_CONNECTED {
		return os.NewFile(uintptr(handle), s.pipeName), nil
	}
	windows.CloseHandle(handle)
	return nil, err
}

func securityAttributesFromSDDL(sddl string) (*windows.SecurityAttributes, error) {
	if strings.TrimSpace(sddl) == "" {
		return nil, nil
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
		InheritHandle:      0,
	}, nil
}

func handleServiceConn(conn *os.File, s *goSearchService) {
	defer conn.Close()
	defer func() {
		if r := recover(); r != nil {
			serviceLog("panic: %v\n%s", r, string(debug.Stack()))
			_ = json.NewEncoder(conn).Encode(serviceResponse{OK: false, Message: fmt.Sprintf("service panic: %v", r)})
		}
	}()
	var req serviceRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(serviceResponse{OK: false, Message: err.Error()})
		return
	}
	switch req.Command {
	case "info":
		s.indexMu.RLock()
		volumes := append([]*serviceVolumeIndex(nil), s.volumes...)
		loading := s.loading
		loadErr := s.loadErr
		s.indexMu.RUnlock()
		infos := make([]dbInfo, 0, len(volumes))
		total := 0
		for _, vol := range volumes {
			idx := vol.index
			total += idx.entryCount()
			info := dbInfo{
				Path:             vol.dbPath,
				Entries:          idx.entryCount(),
				Source:           idx.Source,
				BuiltAt:          idx.BuiltAt.Format(time.RFC3339Nano),
				Volume:           vol.volume,
				JournalID:        vol.journalID,
				Checkpoint:       vol.checkpoint,
				State:            vol.state,
				StaleReason:      vol.staleReason,
				FRNRecords:       vol.frnRecordCount(),
				Recent:           len(vol.recentIDs),
				PathCache:        len(vol.pathCache),
				TermCache:        len(vol.termCache),
				PathTerms:        len(vol.pathTermCache),
				ExtCache:         len(vol.extCache),
				RecentSeq:        vol.recentSeq,
				Dirty:            vol.dirty,
				PersistFailures:  vol.persistFailures,
				LastPersistError: vol.lastPersistErr,
			}
			if !vol.lastPersist.IsZero() {
				info.LastPersist = vol.lastPersist.Format(time.RFC3339Nano)
			}
			if !vol.persistRetryAfter.IsZero() {
				info.PersistRetryAfter = vol.persistRetryAfter.Format(time.RFC3339Nano)
			}
			if vol.queryIndex != nil {
				info.QueryExtKeys = len(vol.queryIndex.ext)
				info.QueryDirs = len(vol.queryIndex.dirs)
			}
			info.NameOrderState = vol.nameOrderStateString()
			info.NameOrderMillis = vol.nameOrderMillis.Load()
			info.NameTrigramState = vol.nameTrigramStateString()
			info.NameTrigramMillis = vol.nameTrigramMillis.Load()
			info.DerivedSections, info.DerivedBytes = derivedSectionInfo(idx.Derived)
			info.Memory = vol.residentMemoryInfo()
			infos = append(infos, info)
		}
		if !loading && serviceResidentBackgroundLoading(volumes) {
			loading = true
		}
		message := ""
		if loading {
			message = "loading indexes"
		} else if loadErr != "" {
			message = loadErr
		}
		_ = json.NewEncoder(conn).Encode(serviceInfoResponseFor(serviceResponse{OK: loadErr == "", Message: message, Entries: total, Loading: loading, DBs: infos, Runtime: runtimeMemorySnapshot()}, s.pipeName, s.processMode))
	case "search":
		s.indexMu.RLock()
		if len(s.indexes) == 0 {
			loading := s.loading
			loadErr := s.loadErr
			s.indexMu.RUnlock()
			message := "service has no search indexes loaded"
			if loading {
				message = "loading indexes"
			} else if loadErr != "" {
				message = loadErr
			}
			_ = json.NewEncoder(conn).Encode(serviceResponse{OK: false, Message: message, Loading: loading})
			return
		}
		opts := requestToOptionsFromService(req)
		if opts.DeadlineUnix == 0 {
			opts.DeadlineUnix = time.Now().Add(serviceQueryTimeout - 250*time.Millisecond).UnixNano()
		}
		trace := &searchTrace{}
		opts.Trace = trace
		if req.RequestSeq > 0 {
			for {
				current := s.requestSeq.Load()
				if req.RequestSeq <= current || s.requestSeq.CompareAndSwap(current, req.RequestSeq) {
					break
				}
			}
			opts.Cancel = func() bool {
				return req.RequestSeq < s.requestSeq.Load()
			}
		}
		var matches []Entry
		var err error
		searchStart := time.Now()
		if len(s.volumes) == len(s.indexes) {
			volumes := s.volumes
			unlockedForSearch := false
			if engineV9Enabled() {
				volumes = snapshotServiceVolumesForSearch(s.volumes)
				s.indexMu.RUnlock()
				unlockedForSearch = true
			}
			if req.CountOnly {
				if count, ok, countErr := countServiceVolumes(volumes, opts); ok {
					if !unlockedForSearch {
						s.indexMu.RUnlock()
					}
					searchMS := float64(time.Since(searchStart).Nanoseconds()) / 1_000_000
					if countErr != nil {
						_ = json.NewEncoder(conn).Encode(serviceResponse{OK: false, Message: countErr.Error()})
					} else {
						trace.setSource("count-fast-posting", count)
						_ = json.NewEncoder(conn).Encode(serviceResponse{OK: true, Count: count, SearchMS: searchMS, Source: trace.Source, Decline: trace.Decline, Candidates: trace.Candidates, PlannerMode: trace.PlannerMode, EligibleVolumes: trace.EligibleVolumes, BlocksDecoded: trace.BlocksDecoded, BlocksSkipped: trace.BlocksSkipped, ScalarDriver: trace.ScalarDriver, ScalarInterval: trace.ScalarInterval, RecordsVerified: trace.ScalarRecordsVerified, ComponentDriver: trace.ComponentDriver, ComponentRoots: trace.ComponentRoots, ComponentIntervals: trace.ComponentIntervals, ComponentCardinality: trace.ComponentCardinality, ComponentSelfHits: trace.ComponentSelfHits, ComponentBounds: trace.ComponentBounds, ComponentRecordsVerified: trace.ComponentRecordsVerified, FilenameDriver: trace.FilenameDriver, FilenameRequiredGrams: trace.FilenameRequiredGrams, FilenamePostingHint: trace.FilenamePostingHint, FilenameRecordsVerified: trace.FilenameRecordsVerified, OverlayBaseWindow: trace.OverlayBaseWindow, PostingPrefetchBytes: trace.PostingPrefetchBytes, PostingPrefetchRanges: trace.PostingPrefetchRanges, PostingPrefetchPages: trace.PostingPrefetchPages, Terms: trace.Terms, Declines: trace.Declines, Fallback: trace.Fallback, Complete: trace.completePtr()})
					}
					return
				}
			}
			matches, err = searchServiceVolumes(volumes, opts, req.CountOnly)
			if !unlockedForSearch {
				s.indexMu.RUnlock()
			}
		} else {
			matches, err = searchAll(s.indexes, opts, req.CountOnly)
			s.indexMu.RUnlock()
		}
		searchMS := float64(time.Since(searchStart).Nanoseconds()) / 1_000_000
		if err != nil {
			_ = json.NewEncoder(conn).Encode(serviceResponse{OK: false, Message: err.Error()})
			return
		}
		resp := serviceResponse{OK: true, Count: len(matches), SearchMS: searchMS, Source: trace.Source, Decline: trace.Decline, Candidates: trace.Candidates, PlannerMode: trace.PlannerMode, EligibleVolumes: trace.EligibleVolumes, BlocksDecoded: trace.BlocksDecoded, BlocksSkipped: trace.BlocksSkipped, ScalarDriver: trace.ScalarDriver, ScalarInterval: trace.ScalarInterval, RecordsVerified: trace.ScalarRecordsVerified, ComponentDriver: trace.ComponentDriver, ComponentRoots: trace.ComponentRoots, ComponentIntervals: trace.ComponentIntervals, ComponentCardinality: trace.ComponentCardinality, ComponentSelfHits: trace.ComponentSelfHits, ComponentBounds: trace.ComponentBounds, ComponentRecordsVerified: trace.ComponentRecordsVerified, FilenameDriver: trace.FilenameDriver, FilenameRequiredGrams: trace.FilenameRequiredGrams, FilenamePostingHint: trace.FilenamePostingHint, FilenameRecordsVerified: trace.FilenameRecordsVerified, OverlayBaseWindow: trace.OverlayBaseWindow, PostingPrefetchBytes: trace.PostingPrefetchBytes, PostingPrefetchRanges: trace.PostingPrefetchRanges, PostingPrefetchPages: trace.PostingPrefetchPages, Terms: trace.Terms, Declines: trace.Declines, Fallback: trace.Fallback, Complete: trace.completePtr()}
		if !req.CountOnly {
			resp.Results = make([]string, len(matches))
			for i, entry := range matches {
				resp.Results[i] = entry.Path
			}
			resp.Rows = entriesToJSON(matches)
		}
		_ = json.NewEncoder(conn).Encode(resp)
	case "index-usn":
		serviceLog("index-usn start volume=%s db=%s", req.Volume, req.DB)
		idx, err := indexUSNVolume(req.Volume)
		if err != nil {
			serviceLog("index-usn error volume=%s err=%v", req.Volume, err)
			_ = json.NewEncoder(conn).Encode(serviceResponse{OK: false, Message: err.Error()})
			return
		}
		serviceLog("index-usn built volume=%s entries=%d", req.Volume, idx.entryCount())
		buildOrders(idx)
		serviceLog("index-usn orders volume=%s", req.Volume)
		s.indexMu.Lock()
		if err := saveIndex(req.DB, idx); err != nil {
			s.indexMu.Unlock()
			serviceLog("index-usn save error volume=%s err=%v", req.Volume, err)
			_ = json.NewEncoder(conn).Encode(serviceResponse{OK: false, Message: err.Error()})
			return
		}
		if err := removeWAL(req.DB); err != nil {
			serviceLog("wal cleanup error volume=%s db=%s err=%v", req.Volume, req.DB, err)
		}
		vol := s.replaceLoadedVolumeLocked(req.DB, idx)
		s.indexMu.Unlock()
		releaseServiceMemoryAfterSave()
		s.startBackgroundNameOrderBuilds([]*serviceVolumeIndex{vol})
		s.startBackgroundNameTrigramBuilds([]*serviceVolumeIndex{vol})
		serviceLog("index-usn complete volume=%s entries=%d", req.Volume, idx.entryCount())
		_ = json.NewEncoder(conn).Encode(serviceResponse{OK: true, Message: "indexed", Entries: idx.entryCount()})
	case "status":
		_ = json.NewEncoder(conn).Encode(serviceInfoResponseFor(serviceResponse{OK: true, Message: "service running"}, s.pipeName, s.processMode))
	default:
		_ = json.NewEncoder(conn).Encode(serviceResponse{OK: false, Message: "unknown command"})
	}
}

func serviceResidentBackgroundLoading(volumes []*serviceVolumeIndex) bool {
	for _, vol := range volumes {
		if vol == nil {
			continue
		}
		nameOrderState := vol.nameOrderStateString()
		if nameOrderState == "pending" || nameOrderState == "building" {
			return true
		}
		nameTrigramState := vol.nameTrigramStateString()
		if nameTrigramState == "pending" || nameTrigramState == "building" {
			return true
		}
	}
	return false
}

func (s *goSearchService) replaceLoadedVolume(dbPath string, idx *Index) {
	if s == nil || idx == nil || dbPath == "" {
		return
	}
	s.indexMu.Lock()
	vol := s.replaceLoadedVolumeLocked(dbPath, idx)
	s.indexMu.Unlock()
	s.startBackgroundNameOrderBuilds([]*serviceVolumeIndex{vol})
	s.startBackgroundNameTrigramBuilds([]*serviceVolumeIndex{vol})
}

func (s *goSearchService) replaceLoadedVolumeLocked(dbPath string, idx *Index) *serviceVolumeIndex {
	if s == nil || idx == nil || dbPath == "" {
		return nil
	}
	vol := newServiceVolumeIndex(dbPath, idx)
	for i, existing := range s.volumes {
		if existing != nil && (samePath(existing.dbPath, dbPath) || strings.EqualFold(existing.volume, idx.Volume)) {
			replaceServiceVolumeContents(existing, vol)
			if i < len(s.indexes) {
				s.indexes[i] = existing.index
			}
			return existing
		}
	}
	s.volumes = append(s.volumes, vol)
	s.indexes = append(s.indexes, idx)
	return vol
}

func replaceServiceVolumeContents(dst, src *serviceVolumeIndex) {
	if dst == nil || src == nil {
		return
	}
	dst.dbPath = src.dbPath
	dst.index = src.index
	dst.volume = src.volume
	dst.journalID = src.journalID
	dst.checkpoint = src.checkpoint
	dst.state = src.state
	dst.staleReason = src.staleReason
	dst.frnToID = src.frnToID
	dst.frns = src.frns
	dst.frnRecordIDs = src.frnRecordIDs
	dst.children = src.children
	dst.childOffsets = src.childOffsets
	dst.childIDs = src.childIDs
	dst.rootIDs = src.rootIDs
	dst.subtreeOrder = src.subtreeOrder
	dst.subtreeStart = src.subtreeStart
	dst.subtreeEnd = src.subtreeEnd
	dst.subtreeSizeRank = src.subtreeSizeRank
	dst.subtreeModRank = src.subtreeModRank
	dst.subtreeExtRank = src.subtreeExtRank
	dst.subtreeTypeRank = src.subtreeTypeRank
	dst.subtreePathRank = src.subtreePathRank
	dst.exactNames = src.exactNames
	dst.pathCache = src.pathCache
	dst.queryIndex = src.queryIndex
	dst.nameOrderState.Store(src.nameOrderState.Load())
	dst.nameOrderMillis.Store(src.nameOrderMillis.Load())
	dst.nameTrigrams.Store(src.nameTrigramIndex())
	dst.nameQuadgrams.Store(src.nameQuadgramIndex())
	dst.nameTrigramState.Store(src.nameTrigramState.Load())
	dst.nameTrigramMillis.Store(src.nameTrigramMillis.Load())
	dst.termCache = src.termCache
	dst.pathTermCache = src.pathTermCache
	dst.extCache = src.extCache
	dst.recentIDs = src.recentIDs
	dst.nameTrigramRecent = src.nameTrigramRecent
	dst.recentSeq = src.recentSeq
	dst.underCache = src.underCache
	dst.underRootCache = src.underRootCache
	dst.overlay = src.overlay
	dst.snap.Store(src.snap.Load())
	dst.snapshotGen.Store(src.snapshotGen.Load())
	dst.dirty = src.dirty
	dst.lastPersist = src.lastPersist
	dst.searchCount = src.searchCount
}

func snapshotServiceVolumesForSearch(volumes []*serviceVolumeIndex) []*serviceVolumeIndex {
	out := make([]*serviceVolumeIndex, 0, len(volumes))
	for _, vol := range volumes {
		out = append(out, snapshotServiceVolumeForSearch(vol))
	}
	return out
}

func snapshotServiceVolumeForSearch(vol *serviceVolumeIndex) *serviceVolumeIndex {
	if vol == nil {
		return nil
	}
	snap := vol.snap.Load()
	idx := vol.index
	if snap != nil && snap.base != nil {
		idx = snap.base
	}
	view := &serviceVolumeIndex{
		dbPath:            vol.dbPath,
		index:             idx,
		volume:            vol.volume,
		journalID:         vol.journalID,
		checkpoint:        vol.checkpoint,
		state:             vol.state,
		staleReason:       vol.staleReason,
		frnToID:           vol.frnToID,
		frns:              vol.frns,
		frnRecordIDs:      vol.frnRecordIDs,
		children:          vol.children,
		childOffsets:      vol.childOffsets,
		childIDs:          vol.childIDs,
		rootIDs:           vol.rootIDs,
		subtreeOrder:      vol.subtreeOrder,
		subtreeStart:      vol.subtreeStart,
		subtreeEnd:        vol.subtreeEnd,
		subtreeSizeRank:   vol.subtreeSizeRank,
		subtreeModRank:    vol.subtreeModRank,
		subtreeExtRank:    vol.subtreeExtRank,
		subtreeTypeRank:   vol.subtreeTypeRank,
		subtreePathRank:   vol.subtreePathRank,
		exactNames:        vol.exactNames,
		pathCache:         make(map[int]string),
		queryIndex:        vol.queryIndex,
		termCache:         make(map[string]postingCacheEntry),
		pathTermCache:     make(map[string]postingCacheEntry),
		extCache:          make(map[string]postingCacheEntry),
		underCache:        make(map[int]postingCacheEntry),
		underRootCache:    make(map[string]postingCacheEntry),
		dirty:             vol.dirty,
		lastPersist:       vol.lastPersist,
		persistFailures:   vol.persistFailures,
		persistRetryAfter: vol.persistRetryAfter,
		lastPersistErr:    vol.lastPersistErr,
	}
	if snap != nil {
		view.recentSeq = snap.gen
		view.snap.Store(snap)
	} else {
		view.recentSeq = vol.cacheGeneration()
	}
	if trigrams := vol.nameTrigramIndex(); trigrams != nil {
		view.nameTrigrams.Store(trigrams)
	}
	if quadgrams := vol.nameQuadgramIndex(); quadgrams != nil {
		view.nameQuadgrams.Store(quadgrams)
	}
	view.nameOrderState.Store(vol.nameOrderState.Load())
	view.nameOrderMillis.Store(vol.nameOrderMillis.Load())
	view.nameTrigramState.Store(vol.nameTrigramState.Load())
	view.nameTrigramMillis.Store(vol.nameTrigramMillis.Load())
	return view
}

func searchService(pipeName string, opts queryOptions, countOnly bool, jsonOut bool) error {
	resp, err := callService(pipeName, serviceRequestFromOptions(opts, countOnly))
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Message)
	}
	if jsonOut {
		jsonResp := jsonSearchResponse{
			OK:                       true,
			Query:                    opts.Query,
			Count:                    resp.Count,
			Limit:                    opts.Limit,
			SearchMS:                 resp.SearchMS,
			Source:                   resp.Source,
			Decline:                  resp.Decline,
			Candidates:               resp.Candidates,
			BlocksDecoded:            resp.BlocksDecoded,
			BlocksSkipped:            resp.BlocksSkipped,
			ScalarDriver:             resp.ScalarDriver,
			ScalarInterval:           resp.ScalarInterval,
			RecordsVerified:          resp.RecordsVerified,
			ComponentDriver:          resp.ComponentDriver,
			ComponentRoots:           resp.ComponentRoots,
			ComponentIntervals:       resp.ComponentIntervals,
			ComponentCardinality:     resp.ComponentCardinality,
			ComponentSelfHits:        resp.ComponentSelfHits,
			ComponentBounds:          resp.ComponentBounds,
			ComponentRecordsVerified: resp.ComponentRecordsVerified,
			FilenameDriver:           resp.FilenameDriver,
			FilenameRequiredGrams:    resp.FilenameRequiredGrams,
			FilenamePostingHint:      resp.FilenamePostingHint,
			FilenameRecordsVerified:  resp.FilenameRecordsVerified,
			OverlayBaseWindow:        resp.OverlayBaseWindow,
			PostingPrefetchBytes:     resp.PostingPrefetchBytes,
			PostingPrefetchRanges:    resp.PostingPrefetchRanges,
			PostingPrefetchPages:     resp.PostingPrefetchPages,
			PlannerMode:              resp.PlannerMode,
			EligibleVolumes:          resp.EligibleVolumes,
			Terms:                    resp.Terms,
			Declines:                 resp.Declines,
			Fallback:                 resp.Fallback,
			Complete:                 resp.Complete,
		}
		if !countOnly {
			if len(resp.Rows) > 0 {
				jsonResp.Results = resp.Rows
			} else {
				jsonResp.Results = pathsToJSON(resp.Results)
			}
		}
		return writeJSON(os.Stdout, jsonResp)
	}
	if countOnly {
		fmt.Println(resp.Count)
		return nil
	}
	w := bufio.NewWriter(os.Stdout)
	for _, result := range resp.Results {
		fmt.Fprintln(w, result)
	}
	return w.Flush()
}

func serviceRequestFromOptions(opts queryOptions, countOnly bool) serviceRequest {
	return serviceRequest{
		Command:       "search",
		Query:         opts.Query,
		MatchPath:     opts.MatchPath || queryLooksLoosePathScoped(opts.Query),
		Limit:         opts.Limit,
		CountOnly:     countOnly,
		Under:         opts.Under,
		Exists:        opts.Exists,
		CWDBias:       opts.CWDBias,
		RootBias:      opts.RootBias,
		Recent:        opts.Recent,
		ModifiedAfter: opts.ModifiedAfter,
		CaseSensitive: opts.CaseSensitive,
		DeadlineUnix:  opts.DeadlineUnix,
		RequestSeq:    opts.RequestSeq,
	}
}

func requestToOptionsFromService(req serviceRequest) queryOptions {
	return queryOptions{
		Query:         req.Query,
		MatchPath:     req.MatchPath,
		Limit:         req.Limit,
		Under:         req.Under,
		Exists:        req.Exists,
		CWDBias:       req.CWDBias,
		RootBias:      req.RootBias,
		Recent:        req.Recent,
		ModifiedAfter: req.ModifiedAfter,
		CaseSensitive: req.CaseSensitive,
		DeadlineUnix:  req.DeadlineUnix,
		RequestSeq:    req.RequestSeq,
	}
}

func search(idx *Index, opts queryOptions, countOnly bool) ([]Entry, error) {
	if idx.Compact {
		return searchCompact(idx, opts, countOnly)
	}
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, err
	}
	if pq.Impossible {
		opts.Trace.setPlannerMode("impossible-query")
		opts.Trace.setSource("impossible-query", 0)
		return []Entry{}, nil
	}
	if queryNeedsAttrs(pq) {
		return nil, errors.New("attrib: filters require a compact NTFS attribute-capable index")
	}
	order := idx.NameOrder
	if pq.MatchPath {
		order = idx.PathOrder
	}
	limit := normalizedLimit(opts.Limit, countOnly)
	if pq.RootBias != "" || pq.CWDBias != "" {
		order = biasOrderEntries(idx, order, firstNonEmpty(pq.CWDBias, pq.RootBias))
	}
	results := make([]Entry, 0, min(limit, 1024))
	for pos, entryIndex := range order {
		if pos&1023 == 0 && queryCanceled(pq) {
			return nil, errQueryCanceled
		}
		entry := idx.Entries[entryIndex]
		entry.IndexSource = idx.Source
		if entryMatches(entry, pq, pq.MatchPath) {
			results = append(results, entry)
			if !countOnly && len(results) >= limit {
				break
			}
		}
	}
	return filterImplicitUnderExisting(results, opts, countOnly), nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func indexInfoToJSON(idx *Index, path string) jsonInfoResponse {
	return jsonInfoResponse{
		OK:          true,
		Version:     idx.Version,
		Source:      idx.Source,
		BuiltAt:     idx.BuiltAt.Format(time.RFC3339Nano),
		Entries:     idx.entryCount(),
		Roots:       append([]string(nil), idx.Roots...),
		Volume:      idx.Volume,
		JournalID:   idx.JournalID,
		Checkpoint:  idx.Checkpoint,
		ContentHash: idx.ContentHash,
		Layout:      estimateIndexLayout(idx, path),
	}
}

func estimateIndexLayout(idx *Index, path string) *indexLayout {
	if idx == nil || !idx.Compact {
		return nil
	}
	recordCount := idx.compactRecordCount()
	layout := &indexLayout{RecordCount: recordCount}
	if info, err := os.Stat(path); err == nil {
		layout.FileBytes = info.Size()
	}
	unique := make(map[string]struct{}, recordCount/2)
	var nameBlobBytes int64
	for i := 0; i < recordCount; i++ {
		rec := idx.compactRecord(i)
		if _, ok := unique[rec.Name]; ok {
			continue
		}
		unique[rec.Name] = struct{}{}
		nameBlobBytes += int64(len(rec.Name))
	}
	layout.UniqueNames = len(unique)
	layout.NameBlobBytes = nameBlobBytes
	layout.NameTableBytes = int64(layout.UniqueNames * 6)
	layout.RecordBytes = int64(layout.RecordCount * compactDiskRecordBytesForCounts(layout.RecordCount, layout.UniqueNames))
	if layout.FileBytes > 0 {
		layout.OtherBytes = layout.FileBytes - layout.RecordBytes - layout.NameBlobBytes - layout.NameTableBytes
		layout.BytesPerRecord = float64(layout.FileBytes) / float64(max(layout.RecordCount, 1))
	}
	return layout
}

func compactNeedsWideDiskRecords(recordCount, tokenCount int) bool {
	return recordCount > int(compactNarrowMaxRecordRef)+1 || tokenCount > int(compactNarrowMaxRecordRef)+1
}

func compactDiskRecordBytesForCounts(recordCount, tokenCount int) int {
	if compactNeedsWideDiskRecords(recordCount, tokenCount) {
		return compactWideDiskRecordBytes
	}
	return compactDiskRecordBytes
}

func entriesToJSON(entries []Entry) []jsonResult {
	out := make([]jsonResult, len(entries))
	for i, entry := range entries {
		out[i] = entryToJSON(entry)
	}
	return out
}

func pathsToJSON(paths []string) []jsonResult {
	out := make([]jsonResult, len(paths))
	for i, path := range paths {
		out[i] = jsonResult{
			Path:        filepath.Clean(path),
			Name:        filepath.Base(path),
			Volume:      filepath.VolumeName(path),
			IndexSource: "service",
		}
	}
	return out
}

func entryToJSON(entry Entry) jsonResult {
	result := jsonResult{
		Path:        filepath.Clean(entry.Path),
		Name:        entry.Name,
		Volume:      filepath.VolumeName(entry.Path),
		IsDir:       entry.Mode&uint32(os.ModeDir) != 0,
		IndexSource: entry.IndexSource,
	}
	if result.Name == "" {
		result.Name = filepath.Base(result.Path)
	}
	size := entry.Size
	result.Size = &size
	if entry.ModUnix != 0 {
		result.Modified = time.Unix(0, entry.ModUnix).Format(time.RFC3339Nano)
	}
	return result
}

func searchAll(indexes []*Index, opts queryOptions, countOnly bool) ([]Entry, error) {
	if len(indexes) == 1 {
		matches, err := search(indexes[0], opts, countOnly)
		if err != nil {
			return nil, err
		}
		return filterImplicitUnderExisting(matches, opts, countOnly), nil
	}
	limit := normalizedLimit(opts.Limit, countOnly)
	results := make([]Entry, 0, min(limit, 1024))
	for _, idx := range indexes {
		childOpts := opts
		childOpts.Limit = limit
		if !countOnly {
			childOpts.Limit = max(limit, idx.compactRecordCount())
		}
		matches, err := search(idx, childOpts, countOnly)
		if err != nil {
			return nil, err
		}
		results = append(results, matches...)
	}
	results = filterImplicitUnderExisting(results, opts, countOnly)
	if !countOnly && entriesSpanMultipleVolumes(results) {
		pq, err := parseQuery(opts)
		if err != nil {
			return nil, err
		}
		sortSearchAllEntries(results, pq)
	}
	if !countOnly && limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func entriesSpanMultipleVolumes(entries []Entry) bool {
	first := ""
	for _, entry := range entries {
		vol := filepath.VolumeName(entry.Path)
		if first == "" {
			first = vol
			continue
		}
		if vol != first {
			return true
		}
	}
	return false
}

func sortSearchAllEntries(entries []Entry, pq parsedQuery) {
	sort.SliceStable(entries, func(i, j int) bool {
		return compareSearchAllEntries(entries[i], entries[j], pq) < 0
	})
}

func compareSearchAllEntries(a, b Entry, pq parsedQuery) int {
	if pq.SortColumn == "size" {
		if a.Size != b.Size {
			if a.Size < b.Size {
				return -1
			}
			return 1
		}
		return compareSearchAllEntryNamePath(a, b)
	}
	if pq.SortColumn == "modified" {
		if a.ModUnix != b.ModUnix {
			if a.ModUnix == 0 {
				return 1
			}
			if b.ModUnix == 0 {
				return -1
			}
			if a.ModUnix > b.ModUnix {
				return -1
			}
			return 1
		}
		return compareSearchAllEntryNamePath(a, b)
	}
	if pq.SortColumn == "extension" {
		ae, be := entryLowerExt(a), entryLowerExt(b)
		if ae != be {
			if ae < be {
				return -1
			}
			return 1
		}
		return compareSearchAllEntryNamePath(a, b)
	}
	if pq.SortColumn == "type" {
		at, bt := entryTypeRank(a), entryTypeRank(b)
		if at != bt {
			return at - bt
		}
		return compareSearchAllEntryNamePath(a, b)
	}
	if pq.SortColumn == "path" {
		return compareSearchAllEntryPath(a, b)
	}
	return compareSearchAllEntryNamePath(a, b)
}

func compareSearchAllEntryNamePath(a, b Entry) int {
	an, bn := entryLowerName(a), entryLowerName(b)
	if an != bn {
		if an < bn {
			return -1
		}
		return 1
	}
	return compareSearchAllEntryPath(a, b)
}

func compareSearchAllEntryPath(a, b Entry) int {
	ap, bp := entryLowerPath(a), entryLowerPath(b)
	if ap != bp {
		if ap < bp {
			return -1
		}
		return 1
	}
	if a.Path != b.Path {
		if a.Path < b.Path {
			return -1
		}
		return 1
	}
	return strings.Compare(a.Name, b.Name)
}

func entryLowerName(entry Entry) string {
	if entry.LowerName != "" {
		return entry.LowerName
	}
	if entry.Name != "" {
		return strings.ToLower(entry.Name)
	}
	return strings.ToLower(filepath.Base(entry.Path))
}

func entryLowerPath(entry Entry) string {
	if entry.LowerPath != "" {
		return entry.LowerPath
	}
	return strings.ToLower(entry.Path)
}

func entryLowerExt(entry Entry) string {
	return strings.ToLower(strings.TrimPrefix(filepath.Ext(entry.Name), "."))
}

func entryTypeRank(entry Entry) int {
	if entry.Mode&uint32(os.ModeDir) != 0 {
		return 0
	}
	return 1
}

func searchServiceVolumes(volumes []*serviceVolumeIndex, opts queryOptions, countOnly bool) ([]Entry, error) {
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, err
	}
	if pq.Impossible {
		opts.Trace.setPlannerMode("impossible-query")
		opts.Trace.setSource("impossible-query", 0)
		return []Entry{}, nil
	}
	volumes, err = serviceVolumesForQuery(volumes, opts)
	if err != nil {
		return nil, err
	}
	if len(volumes) == 0 {
		opts.Trace.setEligibleVolumes(volumes)
		opts.Trace.setPlannerMode("volume-empty")
		opts.Trace.setSource("volume-empty", 0)
		return []Entry{}, nil
	}
	volumes = prioritizeServiceVolumesForPathTerms(volumes, opts)
	opts.Trace.setEligibleVolumes(volumes)
	snapshot := newGlobalQuerySnapshot(volumes, opts.Trace)
	if matches, handled, err := searchServiceVolumesGlobalExtOnlySnapshot(snapshot, opts, countOnly); handled {
		return matches, err
	}
	if matches, handled, err := searchServiceVolumesGlobalComponentsOnlySnapshot(snapshot, opts, countOnly); handled {
		return matches, err
	}
	if matches, handled, err := searchServiceVolumesGlobalNameSnapshot(snapshot, opts, countOnly); handled {
		return matches, err
	}
	if matches, handled, err := searchServiceVolumesGlobalScalarSnapshot(snapshot, opts, countOnly); handled {
		return matches, err
	}
	if matches, handled, err := searchServiceVolumesGlobalBoundedFallbackSnapshot(snapshot, opts, countOnly); handled {
		return matches, err
	}
	if len(volumes) > 1 {
		return nil, globalMultiVolumePlannerDeclineError(opts)
	}
	if len(volumes) == 1 {
		if opts.Trace != nil && strings.HasPrefix(opts.Trace.Decline, "global-") {
			opts.Trace.setFallback("service-single-volume")
		}
		opts.Trace.setPlannerMode("service-single-volume")
		vol := volumes[0]
		locked, ok := lockVolumeSearch(vol, opts)
		if !ok {
			return nil, errQueryCanceled
		}
		pathCache := make(map[int]string)
		hidden := vol.snapshotHiddenBaseIDs()
		candidateFn := vol.nameTermCandidates
		if vol.hasActiveOverlay() && !countOnly {
			if hidden.empty() {
				candidateFn = vol.overlayAwareNameTermCandidates
			} else {
				candidateFn = nil
			}
		}
		baseOpts := opts
		if vol.hasActiveOverlay() && !countOnly {
			// The overlay may outrank a base entry that was just outside the
			// base top-N. Retain one bounded base window per live overlay slot;
			// mergeOverlayMatches applies the actual Entry comparator before the
			// caller's final limit.
			overlayMatches := 0
			if snap := vol.snap.Load(); snap != nil {
				pq, parseErr := parseQuery(opts)
				if parseErr != nil {
					return nil, parseErr
				}
				var countErr error
				overlayMatches, countErr = vol.overlayLiveMatchCountCancellable(snap, pq)
				if countErr != nil {
					return nil, countErr
				}
			}
			baseOpts.Limit = normalizedLimit(opts.Limit, false) + overlayMatches
			if opts.Trace != nil {
				opts.Trace.OverlayBaseWindow = baseOpts.Limit
			}
		}
		matches, err := searchCompactWithCacheHidden(vol.index, baseOpts, countOnly, pathCache, candidateFn, hidden)
		matches = vol.mergeOverlayMatches(matches, opts, countOnly, pathCache)
		vol.trimSearchCachesLocked()
		if locked {
			vol.searchMu.Unlock()
		}
		matches = filterImplicitUnderExisting(matches, opts, countOnly)
		if err == nil && len(matches) == 0 {
			if fallback, ok, complete := filesystemUnderFallbackSearch(opts, countOnly); ok {
				opts.Trace.setFallback("filesystem-under-fallback")
				opts.Trace.setSource("filesystem-under-fallback", len(fallback))
				opts.Trace.setComplete(complete)
				return fallback, nil
			}
		}
		return matches, err
	}
	return nil, nil
}

func queryTermPromotedToExtension(term string, exts []string) bool {
	for _, ext := range exts {
		if strings.TrimPrefix(ext, ".") == term {
			return true
		}
	}
	return false
}

func (vol *serviceVolumeIndex) mergeOverlayMatches(base []Entry, opts queryOptions, countOnly bool, pathCache map[int]string) []Entry {
	if vol == nil || !engineV9Enabled() {
		return base
	}
	snap := vol.snap.Load()
	if snap == nil || len(snap.records) == 0 {
		return base
	}
	pq, err := parseQuery(opts)
	if err != nil {
		return base
	}
	limit := normalizedLimit(opts.Limit, countOnly)
	overlay := vol.overlayRankedMatches(snap, pq, pathCache)
	if !countOnly {
		merged := make([]Entry, 0, len(base)+len(overlay))
		merged = append(merged, base...)
		for _, item := range overlay {
			merged = append(merged, item.entry)
		}
		sort.SliceStable(merged, func(i, j int) bool {
			return compareSearchAllEntries(merged[i], merged[j], pq) < 0
		})
		if limit > 0 && len(merged) > limit {
			merged = merged[:limit]
		}
		return merged
	}
	out := vol.mergeRankedOverlayEntries(base, overlay, limit, countOnly, pq)
	return out
}

func (vol *serviceVolumeIndex) overlayRankedMatches(snap *volumeSnapshot, pq parsedQuery, pathCache map[int]string) []rankedOverlayEntry {
	if vol == nil || snap == nil || len(snap.records) == 0 {
		return nil
	}
	watermark := int(snap.watermark)
	records := snap.records
	if watermark > len(records) {
		watermark = len(records)
	}
	records = records[:watermark]
	if len(records) == 0 {
		return nil
	}
	latest := latestOverlaySlotsByFRN(records)
	overlay := make([]rankedOverlayEntry, 0, len(records))
	for slot := 0; slot < len(records); slot++ {
		entry, ok := vol.overlayEntry(records, latest, slot, map[int32]struct{}{}, pathCache)
		if !ok {
			continue
		}
		if entryMatches(entry, pq, pq.MatchPath) {
			overlay = append(overlay, rankedOverlayEntry{entry: entry, rank: vol.overlayEntryRank(entry, pq)})
		}
	}
	sort.SliceStable(overlay, func(i, j int) bool {
		if overlay[i].rank == overlay[j].rank {
			if pq.SortColumn == "path" {
				ip, jp := overlay[i].entry.LowerPath, overlay[j].entry.LowerPath
				if ip == "" {
					ip = strings.ToLower(overlay[i].entry.Path)
				}
				if jp == "" {
					jp = strings.ToLower(overlay[j].entry.Path)
				}
				if ip == jp {
					return overlay[i].entry.Path < overlay[j].entry.Path
				}
				return ip < jp
			}
			in, jn := overlay[i].entry.LowerName, overlay[j].entry.LowerName
			if in == "" {
				in = strings.ToLower(overlay[i].entry.Name)
			}
			if jn == "" {
				jn = strings.ToLower(overlay[j].entry.Name)
			}
			if in == jn {
				return overlay[i].entry.Path < overlay[j].entry.Path
			}
			return in < jn
		}
		return overlay[i].rank < overlay[j].rank
	})
	return overlay
}

func (vol *serviceVolumeIndex) snapshotHiddenBaseIDs() hiddenBaseIDs {
	if vol == nil || !engineV9Enabled() {
		return hiddenBaseIDs{}
	}
	snap := vol.snap.Load()
	if snap == nil {
		return hiddenBaseIDs{}
	}
	return hiddenBaseIDs{tombstone: snap.tombstoneIDs, shadowed: snap.shadowedIDs}
}

// overlayLiveMatchCount counts live (non-deleted, latest-slot-per-FRN) overlay
// records matching pq, reading only through the given snapshot's records
// slice up to watermark — the same walk mergeOverlayMatches performs for a
// full search, but without allocating/ranking Entry results since callers
// only need len(). It reuses latestOverlaySlotsByFRN/overlayEntry/
// entryMatches so overlay entry construction and match semantics never
// diverge from the search path (review G6: snapshot slices only, never
// vol.overlay maps, on this read path).
func (vol *serviceVolumeIndex) overlayLiveMatchCount(snap *volumeSnapshot, pq parsedQuery) int {
	count, _ := vol.overlayLiveMatchCountCancellable(snap, pq)
	return count
}

func (vol *serviceVolumeIndex) overlayLiveMatchCountCancellable(snap *volumeSnapshot, pq parsedQuery) (int, error) {
	if vol == nil || snap == nil || len(snap.records) == 0 {
		return 0, nil
	}
	watermark := int(snap.watermark)
	records := snap.records
	if watermark > len(records) {
		watermark = len(records)
	}
	records = records[:watermark]
	if len(records) == 0 {
		return 0, nil
	}
	latest := latestOverlaySlotsByFRN(records)
	pathCache := make(map[int]string)
	count := 0
	for slot := 0; slot < len(records); slot++ {
		if queryCanceled(pq) {
			return 0, errQueryCanceled
		}
		entry, ok := vol.overlayEntry(records, latest, slot, map[int32]struct{}{}, pathCache)
		if !ok {
			continue
		}
		if entryMatches(entry, pq, pq.MatchPath) {
			count++
		}
	}
	return count, nil
}

type rankedOverlayEntry struct {
	entry Entry
	rank  int
}

func (vol *serviceVolumeIndex) mergeRankedOverlayEntries(base []Entry, overlay []rankedOverlayEntry, limit int, countOnly bool, pq parsedQuery) []Entry {
	if len(overlay) == 0 {
		return base
	}
	out := make([]Entry, 0, len(base)+len(overlay))
	overlayPos := 0
	for _, entry := range base {
		baseRank := vol.baseEntryRank(entry, pq)
		for overlayPos < len(overlay) && overlay[overlayPos].rank <= baseRank {
			out = append(out, overlay[overlayPos].entry)
			overlayPos++
			if !countOnly && limit > 0 && len(out) >= limit {
				return out
			}
		}
		out = append(out, entry)
		if !countOnly && limit > 0 && len(out) >= limit {
			return out
		}
	}
	for overlayPos < len(overlay) {
		out = append(out, overlay[overlayPos].entry)
		overlayPos++
		if !countOnly && limit > 0 && len(out) >= limit {
			return out
		}
	}
	return out
}

func (vol *serviceVolumeIndex) overlayEntryRank(entry Entry, pq parsedQuery) int {
	if pq.SortColumn == "size" {
		return vol.entrySizeRank(entry)*2 + 1
	}
	if pq.SortColumn == "modified" {
		return vol.entryModifiedRank(entry)*2 + 1
	}
	if pq.SortColumn == "extension" {
		return vol.entryExtensionRank(entry)*2 + 1
	}
	if pq.SortColumn == "type" {
		return vol.entryTypeRank(entry)*2 + 1
	}
	if pq.SortColumn == "path" {
		return vol.overlayEntryPathRank(entry)
	}
	return vol.overlayEntryNameRank(entry)
}

func (vol *serviceVolumeIndex) baseEntryRank(entry Entry, pq parsedQuery) int {
	if pq.SortColumn == "size" {
		return vol.entrySizeRank(entry) * 2
	}
	if pq.SortColumn == "modified" {
		return vol.entryModifiedRank(entry) * 2
	}
	if pq.SortColumn == "extension" {
		return vol.entryExtensionRank(entry) * 2
	}
	if pq.SortColumn == "type" {
		return vol.entryTypeRank(entry) * 2
	}
	if pq.SortColumn == "path" {
		return vol.entryPathRank(entry) * 2
	}
	return vol.baseEntryNameRank(entry)
}

func (vol *serviceVolumeIndex) entrySizeRank(entry Entry) int {
	if vol == nil || vol.index == nil {
		return 0
	}
	order := vol.sizeOrderForRank()
	recordCount := vol.index.compactRecordCount()
	pos := sort.Search(len(order), func(i int) bool {
		id := int(order[i])
		if id < 0 || id >= recordCount {
			return false
		}
		return vol.index.compactRecord(id).Size >= entry.Size
	})
	return pos
}

func (vol *serviceVolumeIndex) entryModifiedRank(entry Entry) int {
	if vol == nil || vol.index == nil {
		return 0
	}
	order := vol.modifiedOrderForRank()
	recordCount := vol.index.compactRecordCount()
	pos := sort.Search(len(order), func(i int) bool {
		id := int(order[i])
		if id < 0 || id >= recordCount {
			return false
		}
		mod := vol.index.compactRecord(id).ModUnix
		if entry.ModUnix == 0 {
			return mod == 0
		}
		return mod <= entry.ModUnix
	})
	return pos
}

func (vol *serviceVolumeIndex) entryExtensionRank(entry Entry) int {
	if vol == nil || vol.index == nil {
		return 0
	}
	ext := strings.TrimPrefix(filepath.Ext(entry.Name), ".")
	ext = strings.ToLower(ext)
	name := entry.LowerName
	if name == "" {
		name = strings.ToLower(entry.Name)
	}
	order := vol.extensionOrderForRank()
	recordCount := vol.index.compactRecordCount()
	pos := sort.Search(len(order), func(i int) bool {
		id := int(order[i])
		if id < 0 || id >= recordCount {
			return false
		}
		recExt := compactRecordLowerExt(vol.index.compactRecord(id))
		if recExt != ext {
			return recExt >= ext
		}
		return vol.index.compactLowerNameAt(id) >= name
	})
	return pos
}

func (vol *serviceVolumeIndex) entryTypeRank(entry Entry) int {
	if vol == nil || vol.index == nil {
		return 0
	}
	class := 1
	if entry.Mode&uint32(os.ModeDir) != 0 {
		class = 0
	}
	name := entry.LowerName
	if name == "" {
		name = strings.ToLower(entry.Name)
	}
	order := vol.typeOrderForRank()
	recordCount := vol.index.compactRecordCount()
	pos := sort.Search(len(order), func(i int) bool {
		id := int(order[i])
		if id < 0 || id >= recordCount {
			return false
		}
		rec := vol.index.compactRecord(id)
		recClass := compactRecordTypeRank(rec)
		if recClass != class {
			return recClass >= class
		}
		return vol.index.compactLowerNameAt(id) >= name
	})
	return pos
}

func (vol *serviceVolumeIndex) entryPathRank(entry Entry) int {
	if vol == nil || vol.index == nil {
		return 0
	}
	path := entry.LowerPath
	if path == "" {
		path = strings.ToLower(entry.Path)
	}
	order := vol.pathOrderForRank()
	recordCount := vol.index.compactRecordCount()
	cache := make(map[int]string)
	pos := sort.Search(len(order), func(i int) bool {
		id := int(order[i])
		if id < 0 || id >= recordCount {
			return false
		}
		return strings.ToLower(vol.index.reconstructCompactPathCached(id, cache)) >= path
	})
	return pos
}

func (vol *serviceVolumeIndex) overlayEntryPathRank(entry Entry) int {
	if vol == nil || vol.index == nil {
		return 0
	}
	path := entry.LowerPath
	if path == "" {
		path = strings.ToLower(entry.Path)
	}
	order := vol.pathOrderForRank()
	recordCount := vol.index.compactRecordCount()
	cache := make(map[int]string)
	pos := sort.Search(len(order), func(i int) bool {
		id := int(order[i])
		if id < 0 || id >= recordCount {
			return false
		}
		return strings.ToLower(vol.index.reconstructCompactPathCached(id, cache)) >= path
	})
	if pos < len(order) {
		id := int(order[pos])
		if id >= 0 && id < recordCount && strings.ToLower(vol.index.reconstructCompactPathCached(id, cache)) == path {
			return pos*2 + 1
		}
	}
	return pos*2 - 1
}

func (vol *serviceVolumeIndex) overlayEntryNameRank(entry Entry) int {
	name := entry.LowerName
	if name == "" {
		name = strings.ToLower(entry.Name)
	}
	if vol == nil || vol.index == nil {
		return 0
	}
	order := vol.mappedOrCompactNameOrder()
	recordCount := vol.index.compactRecordCount()
	pos := sort.Search(len(order), func(i int) bool {
		id := int(order[i])
		if id < 0 || id >= recordCount {
			return false
		}
		return vol.index.compactLowerNameAt(id) >= name
	})
	if pos < len(order) {
		id := int(order[pos])
		if id >= 0 && id < recordCount && vol.index.compactLowerNameAt(id) == name {
			return pos*2 + 1
		}
	}
	return pos*2 - 1
}

func (vol *serviceVolumeIndex) baseEntryNameRank(entry Entry) int {
	name := entry.LowerName
	if name == "" {
		name = strings.ToLower(entry.Name)
	}
	if vol == nil || vol.index == nil {
		return 0
	}
	order := vol.mappedOrCompactNameOrder()
	recordCount := vol.index.compactRecordCount()
	pos := sort.Search(len(order), func(i int) bool {
		id := int(order[i])
		if id < 0 || id >= recordCount {
			return false
		}
		return vol.index.compactLowerNameAt(id) >= name
	})
	return pos * 2
}

func (vol *serviceVolumeIndex) mappedOrCompactNameOrder() []uint32 {
	if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.nameOrder) > 0 {
		return vol.queryIndex.nameOrder
	}
	if vol == nil || vol.index == nil {
		return nil
	}
	if len(vol.index.CompactNameOrder) == 0 {
		return nil
	}
	out := make([]uint32, len(vol.index.CompactNameOrder))
	for i, id := range vol.index.CompactNameOrder {
		out[i] = uint32(id)
	}
	return out
}

func (vol *serviceVolumeIndex) sizeOrderForRank() []uint32 {
	if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.sizeOrder) > 0 {
		return vol.queryIndex.sizeOrder
	}
	if vol != nil && vol.index != nil && len(vol.index.Derived.SizeOrder) > 0 {
		return vol.index.Derived.SizeOrder
	}
	if vol == nil || vol.index == nil || !vol.index.compactHasSize() {
		return nil
	}
	order, _ := buildCompactSizeOrderRank(vol.index)
	return order
}

func (vol *serviceVolumeIndex) modifiedOrderForRank() []uint32 {
	if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.modOrder) > 0 {
		return vol.queryIndex.modOrder
	}
	if vol != nil && vol.index != nil && len(vol.index.Derived.ModOrder) > 0 {
		return vol.index.Derived.ModOrder
	}
	if vol == nil || vol.index == nil || !vol.index.compactHasModTime() {
		return nil
	}
	order, _ := buildCompactModifiedOrderRank(vol.index)
	return order
}

func (vol *serviceVolumeIndex) extensionOrderForRank() []uint32 {
	if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.extOrder) > 0 {
		return vol.queryIndex.extOrder
	}
	if vol != nil && vol.index != nil && len(vol.index.Derived.ExtOrder) > 0 {
		return vol.index.Derived.ExtOrder
	}
	if vol == nil || vol.index == nil {
		return nil
	}
	order, _ := buildCompactExtensionOrderRank(vol.index)
	return order
}

func (vol *serviceVolumeIndex) typeOrderForRank() []uint32 {
	if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.typeOrder) > 0 {
		return vol.queryIndex.typeOrder
	}
	if vol != nil && vol.index != nil && len(vol.index.Derived.TypeOrder) > 0 {
		return vol.index.Derived.TypeOrder
	}
	if vol == nil || vol.index == nil {
		return nil
	}
	order, _ := buildCompactTypeOrderRank(vol.index)
	return order
}

func (vol *serviceVolumeIndex) pathOrderForRank() []uint32 {
	if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.pathOrder) > 0 {
		return vol.queryIndex.pathOrder
	}
	if vol != nil && vol.index != nil && len(vol.index.Derived.PathOrder) > 0 {
		return vol.index.Derived.PathOrder
	}
	if vol == nil || vol.index == nil {
		return nil
	}
	order, _ := buildCompactPathOrderRank(vol.index)
	return order
}

func latestOverlaySlotsByFRN(records []CompactRecord) map[uint64]int32 {
	latest := make(map[uint64]int32, len(records))
	for i := len(records) - 1; i >= 0; i-- {
		frn := records[i].FRN
		if frn == 0 {
			continue
		}
		if _, exists := latest[frn]; !exists {
			latest[frn] = int32(i)
		}
	}
	return latest
}

func (vol *serviceVolumeIndex) overlayEntry(records []CompactRecord, latest map[uint64]int32, slot int, seen map[int32]struct{}, pathCache map[int]string) (Entry, bool) {
	if vol == nil || slot < 0 || slot >= len(records) {
		return Entry{}, false
	}
	if _, ok := seen[int32(slot)]; ok {
		return Entry{}, false
	}
	seen[int32(slot)] = struct{}{}
	rec := records[slot]
	if latestSlot := latest[rec.FRN]; latestSlot != int32(slot) {
		return Entry{}, false
	}
	if rec.Deleted {
		return Entry{}, false
	}
	path := vol.overlayRecordPath(records, latest, slot, seen, pathCache)
	if path == "" {
		return Entry{}, false
	}
	return Entry{
		Path:        path,
		Name:        rec.Name,
		LowerName:   strings.ToLower(rec.Name),
		LowerPath:   strings.ToLower(path),
		Mode:        rec.Mode,
		Size:        rec.Size,
		ModUnix:     rec.ModUnix,
		IndexSource: vol.index.Source,
	}, true
}

func (vol *serviceVolumeIndex) overlayRecordPath(records []CompactRecord, latest map[uint64]int32, slot int, seen map[int32]struct{}, pathCache map[int]string) string {
	if pathCache == nil {
		pathCache = make(map[int]string)
	}
	if slot < 0 || slot >= len(records) {
		return ""
	}
	rec := records[slot]
	if rec.Deleted {
		return ""
	}
	name := rec.Name
	if name == "" {
		name = "."
	}
	if rec.ParentFRN == 0 || rec.ParentFRN == rec.FRN {
		if vol.volume != "" && name != "." {
			return vol.volume + `\` + name
		}
		if vol.volume != "" {
			return vol.volume + `\`
		}
		return name
	}
	if parentSlot, ok := latest[rec.ParentFRN]; ok && parentSlot >= 0 {
		if _, ok := seen[parentSlot]; ok {
			return ""
		}
		parentPath := vol.overlayRecordPath(records, latest, int(parentSlot), seen, pathCache)
		if parentPath != "" {
			return joinOverlayPath(parentPath, name)
		}
		return ""
	}
	if parentID, ok := vol.idForFRN(rec.ParentFRN); ok {
		parentPath := vol.index.reconstructCompactPathCached(parentID, pathCache)
		if parentPath != "" {
			return joinOverlayPath(parentPath, name)
		}
	}
	if vol.volume != "" {
		return vol.volume + `\` + name
	}
	return name
}

func joinOverlayPath(parentPath, name string) string {
	if parentPath == "" {
		return name
	}
	if strings.HasSuffix(parentPath, `\`) || strings.HasSuffix(parentPath, `/`) {
		return parentPath + name
	}
	if vol := filepath.VolumeName(parentPath); vol != "" && strings.EqualFold(parentPath, vol) {
		return parentPath + `\` + name
	}
	return filepath.Join(parentPath, name)
}

func prioritizeServiceVolumesForPathTerms(volumes []*serviceVolumeIndex, opts queryOptions) []*serviceVolumeIndex {
	if len(volumes) < 2 || opts.Under != "" {
		return volumes
	}
	pq, err := parseQuery(opts)
	if err != nil || !pq.MatchPath {
		return volumes
	}
	type scoredVolume struct {
		vol          *serviceVolumeIndex
		matchedTerms int
		score        int
		pos          int
	}
	scored := make([]scoredVolume, 0, len(volumes))
	hasScore := false
	for i, vol := range volumes {
		score := 0
		matchedTerms := 0
		if vol != nil && vol.queryIndex != nil {
			for _, term := range pq.Terms {
				if len(term) < 3 || isVolumeQueryTerm(term) || strings.ContainsAny(term, `\/*?[]:`) {
					continue
				}
				if hits := vol.componentPostingCount(term); hits > 0 {
					matchedTerms++
					score += 1_000_000 / max(1, hits)
				}
				if ext := strings.TrimPrefix(term, "."); ext != "" && bareExtensionCandidateTerm(ext) {
					if hits := vol.extPostingCount(ext); hits > 0 {
						score += 10_000_000 + 2_000_000/max(1, hits)
					}
				}
			}
		}
		if score > 0 {
			hasScore = true
		}
		scored = append(scored, scoredVolume{vol: vol, matchedTerms: matchedTerms, score: score, pos: i})
	}
	if !hasScore {
		return volumes
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].matchedTerms != scored[j].matchedTerms {
			return scored[i].matchedTerms > scored[j].matchedTerms
		}
		if scored[i].score == scored[j].score {
			return scored[i].pos < scored[j].pos
		}
		return scored[i].score > scored[j].score
	})
	out := make([]*serviceVolumeIndex, len(scored))
	for i, item := range scored {
		out[i] = item.vol
	}
	return out
}

func lockVolumeSearch(vol *serviceVolumeIndex, opts queryOptions) (bool, bool) {
	if vol == nil {
		return false, false
	}
	if engineV9Enabled() {
		pq := parsedQuery{DeadlineUnix: opts.DeadlineUnix, Cancel: opts.Cancel}
		return false, !queryCanceled(pq)
	}
	if opts.Cancel == nil && opts.DeadlineUnix == 0 {
		vol.searchMu.Lock()
		return true, true
	}
	pq := parsedQuery{DeadlineUnix: opts.DeadlineUnix, Cancel: opts.Cancel}
	for {
		if queryCanceled(pq) {
			return false, false
		}
		if vol.searchMu.TryLock() {
			if queryCanceled(pq) {
				vol.searchMu.Unlock()
				return false, false
			}
			return true, true
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func filterImplicitUnderExisting(matches []Entry, opts queryOptions, countOnly bool) []Entry {
	if countOnly || opts.Exists || opts.Under == "" || len(matches) == 0 {
		return matches
	}
	info, err := os.Stat(opts.Under)
	if err != nil || !info.IsDir() {
		return matches
	}
	out := matches[:0]
	for _, entry := range matches {
		if _, err := os.Stat(entry.Path); err == nil {
			out = append(out, entry)
		}
	}
	return out
}

func filesystemUnderFallbackSearch(opts queryOptions, countOnly bool) ([]Entry, bool, bool) {
	return filesystemUnderFallbackSearchLimited(opts, countOnly, filesystemFallbackMaxVisited, filesystemFallbackMaxDuration)
}

func filesystemUnderFallbackSearchLimited(opts queryOptions, countOnly bool, maxVisited int, maxDuration time.Duration) ([]Entry, bool, bool) {
	if countOnly || opts.Under == "" || isVolumeRoot(opts.Under) {
		return nil, false, true
	}
	root := normalizeFilterPath(opts.Under)
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, false, true
	}
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, false, true
	}
	limit := normalizedLimit(opts.Limit, false)
	pq.Limit = limit
	matches := make([]Entry, 0, min(limit, 128))
	deadline := time.Time{}
	if maxDuration > 0 {
		deadline = time.Now().Add(maxDuration)
	}
	visited := 0
	complete := true
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		visited++
		if maxVisited > 0 && visited > maxVisited {
			complete = false
			return filepath.SkipAll
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			complete = false
			return filepath.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		entry := Entry{
			Path:      path,
			Name:      d.Name(),
			LowerPath: strings.ToLower(path),
			LowerName: strings.ToLower(d.Name()),
			Size:      info.Size(),
			Mode:      uint32(info.Mode()),
			ModUnix:   info.ModTime().UnixNano(),
		}
		if entryMatches(entry, pq, pq.MatchPath) {
			matches = append(matches, entry)
			if limit > 0 && len(matches) >= limit {
				return filepath.SkipAll
			}
		}
		return nil
	})
	if len(matches) == 0 {
		return nil, false, complete
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].LowerName == matches[j].LowerName {
			return matches[i].LowerPath < matches[j].LowerPath
		}
		return matches[i].LowerName < matches[j].LowerName
	})
	return matches, true, complete
}

func isVolumeRoot(path string) bool {
	clean := normalizeFilterPath(path)
	vol := filepath.VolumeName(clean)
	if vol == "" {
		return false
	}
	rest := strings.TrimPrefix(clean, vol)
	return rest == `\` || rest == `/` || rest == ""
}

func countServiceVolumes(volumes []*serviceVolumeIndex, opts queryOptions) (int, bool, error) {
	pq, err := parseQuery(opts)
	if err != nil {
		return 0, true, err
	}
	if pq.Impossible {
		opts.Trace.setPlannerMode("impossible-query")
		opts.Trace.setSource("impossible-query", 0)
		return 0, true, nil
	}
	volumes, err = serviceVolumesForQuery(volumes, opts)
	if err != nil {
		return 0, true, err
	}
	if len(volumes) == 0 {
		opts.Trace.setEligibleVolumes(volumes)
		opts.Trace.setPlannerMode("volume-empty")
		opts.Trace.setSource("volume-empty", 0)
		return 0, true, nil
	}
	volumes = prioritizeServiceVolumesForPathTerms(volumes, opts)
	opts.Trace.setEligibleVolumes(volumes)
	snapshot := newGlobalQuerySnapshot(volumes, opts.Trace)
	if count, handled, err := countServiceVolumesGlobalOnlySnapshot(snapshot, opts); handled {
		if count == 0 && opts.Trace != nil && opts.Trace.PlannerMode == "global-count-name" {
			opts.Trace.setSource("exact-empty", 0)
		}
		return count, true, err
	}
	if count, handled, err := countServiceVolumesGlobalNameSnapshot(snapshot, opts); handled {
		if count == 0 && opts.Trace != nil && opts.Trace.PlannerMode == "global-count-name" {
			opts.Trace.setSource("exact-empty", 0)
		}
		return count, true, err
	}
	if count, handled, err := countServiceVolumesGlobalScalarSnapshot(snapshot, opts); handled {
		return count, true, err
	}
	if count, handled, err := countServiceVolumesGlobalBoundedFallbackSnapshot(snapshot, opts); handled {
		return count, true, err
	}
	if len(volumes) > 1 {
		return 0, true, globalMultiVolumePlannerDeclineError(opts)
	}
	if opts.Trace != nil && strings.HasPrefix(opts.Trace.Decline, "global-") {
		opts.Trace.setFallback("service-count-single-volume")
	}
	opts.Trace.setPlannerMode("service-count-single-volume")
	pq, err = parseQuery(opts)
	if err != nil {
		return 0, true, err
	}
	vol := volumes[0]
	dropSatisfiedVolumeTerms(&pq, vol.index.Volume)
	if err := checkQueryCapabilities(pq, vol.index); err != nil {
		return 0, true, err
	}
	if vol.hasActiveOverlay() {
		if count, ok := vol.overlayAwareFastCount(pq); ok {
			return count, true, nil
		}
		pathCache := make(map[int]string)
		matches, err := searchCompactWithCacheHidden(vol.index, opts, true, pathCache, vol.nameTermCandidates, vol.snapshotHiddenBaseIDs())
		if err != nil {
			return 0, true, err
		}
		matches = vol.mergeOverlayMatches(matches, opts, true, pathCache)
		return len(matches), true, nil
	}
	locked := false
	if !engineV9Enabled() {
		vol.searchMu.Lock()
		locked = true
	}
	count, ok := vol.fastPostingCount(pq)
	if locked {
		vol.searchMu.Unlock()
	}
	if !ok {
		return 0, false, nil
	}
	return count, true, nil
}

// overlayAwareFastCount is the review-G7 / plan-R2.6 sanctioned stopgap: it
// answers a count query exactly while a v9 overlay is active, without
// falling back to the full search+merge path. It is
//
//	(base fast posting count, filtered against tombstoned/shadowed base ids)
//	+ (linear count of live overlay records matching pq)
//
// It reads the volume's snapshot exactly once and only touches snapshot
// slices (records[:watermark], tombstoneIDs, shadowedIDs) — never
// vol.overlay's live maps, which the apply goroutine mutates concurrently
// (review G6). If the base fast-count route cannot evaluate pq (same decline
// conditions as fastPostingCount today), this declines too (ok=false) rather
// than guess, per the R2.6 invariant: any route that can't see the overlay
// exactly must decline, not answer stale/wrong.
func (vol *serviceVolumeIndex) overlayAwareFastCount(pq parsedQuery) (int, bool) {
	if vol == nil || vol.index == nil {
		return 0, false
	}
	snap := vol.snap.Load()
	if snap == nil {
		// No snapshot published yet even though hasActiveOverlay() said the
		// overlay was active (e.g. legacy overlay.watermark path without a
		// published snapshot) -- decline rather than risk reading the live
		// overlay maps outside the snapshot.
		return 0, false
	}
	hidden := hiddenBaseIDs{tombstone: snap.tombstoneIDs, shadowed: snap.shadowedIDs}
	baseCount, ok := vol.fastPostingCountHidden(pq, hidden)
	if !ok {
		return 0, false
	}
	overlayCount := vol.overlayLiveMatchCount(snap, pq)
	return baseCount + overlayCount, true
}

func (vol *serviceVolumeIndex) hasActiveOverlay() bool {
	if !engineV9Enabled() || vol == nil {
		return false
	}
	if snap := vol.snap.Load(); snap != nil {
		return snap.watermark > 0
	}
	return vol.overlay != nil && vol.overlay.watermark.Load() > 0
}

func serviceVolumesForQuery(volumes []*serviceVolumeIndex, opts queryOptions) ([]*serviceVolumeIndex, error) {
	if len(volumes) == 0 {
		return nil, errors.New("service has no search indexes loaded")
	}
	wantVolume := queryVolumeConstraint(opts)
	ready := make([]*serviceVolumeIndex, 0, len(volumes))
	stale := make([]string, 0, len(volumes))
	for _, vol := range volumes {
		if vol == nil || vol.index == nil {
			continue
		}
		if wantVolume != "" && !serviceVolumeMatchesConstraint(vol, wantVolume) {
			continue
		}
		if vol.state != "" && vol.state != "ready" {
			stale = append(stale, fmt.Sprintf("%s: %s", vol.index.Volume, vol.staleReason))
		}
		ready = append(ready, vol)
	}
	if len(ready) > 0 {
		return ready, nil
	}
	if len(stale) > 0 {
		return nil, fmt.Errorf("matching search index is stale: %s", strings.Join(stale, "; "))
	}
	if wantVolume != "" {
		// An explicit volume anchor that is absent from this snapshot is an
		// exact empty federated scope. Return an empty eligible set so the
		// caller can terminate before planner-family routing, posting decode,
		// record verification, or filesystem fallback.
		return []*serviceVolumeIndex{}, nil
	}
	return nil, errors.New("service has no ready search indexes loaded")
}

func serviceVolumeMatchesConstraint(vol *serviceVolumeIndex, wantVolume string) bool {
	if vol == nil || vol.index == nil || wantVolume == "" {
		return true
	}
	if vol.index.Volume != "" {
		return strings.EqualFold(vol.index.Volume, wantVolume)
	}
	for _, root := range vol.index.Roots {
		if strings.EqualFold(filepath.VolumeName(filepath.Clean(root)), wantVolume) {
			return true
		}
	}
	return false
}

func queryVolumeConstraint(opts queryOptions) string {
	if underVolume := strings.ToUpper(filepath.VolumeName(filepath.Clean(opts.Under))); underVolume != "" {
		return underVolume
	}
	pq, err := parseQuery(opts)
	if err != nil {
		return ""
	}
	volume := ""
	for _, term := range pq.Terms {
		if !isVolumeQueryTerm(term) {
			continue
		}
		normalized := strings.ToUpper(term)
		if volume == "" {
			volume = normalized
			continue
		}
		if !strings.EqualFold(volume, normalized) {
			return ""
		}
	}
	return volume
}

func (vol *serviceVolumeIndex) fastPostingCount(pq parsedQuery) (int, bool) {
	return vol.fastPostingCountHidden(pq, hiddenBaseIDs{})
}

// fastPostingCountHidden is fastPostingCount plus an id-level exclusion set
// for base records tombstoned/shadowed by the active v9 overlay. Every
// candidate-id loop below checks hidden before counting, so the result stays
// exact while an overlay is active instead of being a stale base-only count
// (review G7 / plan R2.6).
func (vol *serviceVolumeIndex) fastPostingCountHidden(pq parsedQuery, hidden hiddenBaseIDs) (int, bool) {
	if count, ok := vol.plannedCountHidden(pq, hidden); ok {
		return count, true
	}
	if count, ok := vol.fastBareExtensionPathCountHidden(pq, hidden); ok {
		return count, true
	}
	globExts, globsOK := simpleGlobExts(pq.Globs)
	if vol == nil || vol.index == nil || vol.queryIndex == nil || pq.CaseSensitive || pq.Under != "" || pq.Exists || pq.HasModAfter || len(pq.AttrFilters) > 0 || len(pq.Regexps) > 0 || !globsOK || len(pq.Dirs) > 0 || len(pq.Parents) > 0 {
		return 0, false
	}
	lists := make([]postingCountCandidate, 0, len(pq.Exts)+len(globExts)+len(pq.Terms)+1)
	for _, ext := range pq.Exts {
		list, ok := vol.extPostingCountCandidate(ext)
		if !ok || list.len() == 0 {
			return vol.countRecentOnlyHidden(pq, hidden), true
		}
		lists = append(lists, list)
	}
	for _, ext := range globExts {
		list, ok := vol.extPostingCountCandidate(ext)
		if !ok || list.len() == 0 {
			return vol.countRecentOnlyHidden(pq, hidden), true
		}
		lists = append(lists, list)
	}
	switch pq.Type {
	case "":
	case "file":
		if len(lists) == 0 {
			return 0, false
		}
	case "dir":
		lists = append(lists, postingCountCandidate{ids: vol.queryIndex.dirs})
	default:
		return 0, false
	}
	for _, term := range pq.Terms {
		if !pq.MatchPath || !isVolumeQueryTerm(term) {
			return 0, false
		}
		if !strings.EqualFold(term, vol.volume) {
			return vol.countRecentOnlyHidden(pq, hidden), true
		}
	}
	if len(lists) == 0 {
		return 0, false
	}
	sortPostingCountCandidatesByLen(lists)
	candidates := lists[0].materialize()
	for _, list := range lists[1:] {
		if list.mapped {
			candidates = intersectSortedUint32sWithPostingIterator(candidates, list.it)
		} else {
			candidates = intersectSortedUint32s(candidates, list.ids)
		}
		if len(candidates) == 0 {
			break
		}
	}
	recent := vol.recentIDs
	count := 0
	for _, id := range candidates {
		if _, ok := recent[int(id)]; ok {
			continue
		}
		if !hidden.empty() && hidden.contains(int(id)) {
			continue
		}
		count++
	}
	for id := range recent {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		if !hidden.empty() && hidden.contains(id) {
			continue
		}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted && compactRecordPrecheck(rec, pq, true) {
			count++
		}
	}
	return count, true
}

func (vol *serviceVolumeIndex) fastBareExtensionPathCountHidden(pq parsedQuery, hidden hiddenBaseIDs) (int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CaseSensitive ||
		pq.Under != "" || pq.Exists || pq.HasModAfter ||
		len(pq.Exts) > 0 || len(pq.Globs) > 0 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.Parents) > 0 || len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		pq.Type == "dir" || countNonVolumeTerms(pq.Terms) < 2 {
		return 0, false
	}
	hasAnchor := false
	var bestExt string
	var best postingCountCandidate
	bestSet := false
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) {
			continue
		}
		if strings.ContainsAny(term, `\/*?[]:`) {
			return 0, false
		}
		if ext, ok := pathExtensionCandidateTerm(term); ok && vol.pathTermIsUsableExtensionCandidate(term) {
			candidate, ok := vol.extPostingCountCandidate(ext)
			if !ok {
				continue
			}
			if candidate.len() == 0 {
				return vol.countRecentOnlyHidden(pq, hidden), true
			}
			if candidate.len() > serviceComponentMultiTermScanMaxIDs {
				continue
			}
			if !bestSet || candidate.len() < best.len() {
				bestExt = ext
				best = candidate
				bestSet = true
			}
			continue
		}
		if len(term) >= 4 {
			hasAnchor = true
		}
	}
	if !hasAnchor || !bestSet {
		return 0, false
	}
	matchesExt := func(rec CompactRecord) bool {
		actual := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
		return strings.EqualFold(actual, bestExt)
	}
	pathCache := make(map[int]string)
	recent := vol.recentIDs
	count := 0
	for _, id32 := range best.materialize() {
		id := int(id32)
		if _, ok := recent[id]; ok {
			continue
		}
		if !hidden.empty() && hidden.contains(id) {
			continue
		}
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !matchesExt(rec) {
			continue
		}
		if _, ok := compactCandidateEntryIfMatch(vol.index, pq, id, pathCache, true, false); ok {
			count++
		}
	}
	for id := range recent {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		if !hidden.empty() && hidden.contains(id) {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !matchesExt(rec) {
			continue
		}
		if _, ok := compactCandidateEntryIfMatch(vol.index, pq, id, pathCache, true, false); ok {
			count++
		}
	}
	return count, true
}

type postingCountCandidate struct {
	ids    []uint32
	it     postingBlockIterator
	count  int
	mapped bool
}

func (candidate postingCountCandidate) len() int {
	if candidate.mapped {
		return candidate.count
	}
	return len(candidate.ids)
}

func (candidate postingCountCandidate) materialize() []uint32 {
	if candidate.mapped {
		return materializePostingBlockIterator(candidate.it, candidate.count)
	}
	return append([]uint32(nil), candidate.ids...)
}

func isVolumeQueryTerm(term string) bool {
	if len(term) != 2 || term[1] != ':' {
		return false
	}
	c := term[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func (vol *serviceVolumeIndex) countRecentOnly(pq parsedQuery) int {
	return vol.countRecentOnlyHidden(pq, hiddenBaseIDs{})
}

func (vol *serviceVolumeIndex) countRecentOnlyHidden(pq parsedQuery, hidden hiddenBaseIDs) int {
	count := 0
	for id := range vol.recentIDs {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		if !hidden.empty() && hidden.contains(id) {
			continue
		}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted && compactRecordPrecheck(rec, pq, true) {
			count++
		}
	}
	return count
}

func searchCompact(idx *Index, opts queryOptions, countOnly bool) ([]Entry, error) {
	return searchCompactWithCache(idx, opts, countOnly, nil, nil)
}

func compactOrderLen(order []int, recordCount int) int {
	if order == nil {
		return recordCount
	}
	return len(order)
}

func compactOrderAt(order []int, pos int) int {
	if order == nil {
		return pos
	}
	return order[pos]
}

func uint32OrderToInts(order []uint32) []int {
	if len(order) == 0 {
		return nil
	}
	out := make([]int, len(order))
	for i, id := range order {
		out[i] = int(id)
	}
	return out
}

func (idx *Index) compactRecordCount() int {
	if idx.MMapRecords != nil {
		return idx.MMapRecords.Len()
	}
	if idx.PackedRecords != nil {
		return idx.PackedRecords.Len()
	}
	return len(idx.Records)
}

// compactHasSize reports whether the index carries per-record file sizes.
// Older USN-built indexes may not capture sizes; size: filters must error
// against those indexes rather than silently match nothing.
func (idx *Index) compactHasSize() bool {
	if idx.MMapRecords != nil {
		return idx.MMapRecords.hasSize
	}
	if idx.PackedRecords != nil {
		return idx.PackedRecords.Size32 != nil
	}
	for i := range idx.Records {
		if idx.Records[i].Size != 0 {
			return true
		}
	}
	return false
}

// compactHasModTime reports whether the index carries per-record modification
// times. Used to gate dm:, --recent, and --modified-after.
func (idx *Index) compactHasModTime() bool {
	if idx.MMapRecords != nil {
		return idx.MMapRecords.hasModUnix
	}
	if idx.PackedRecords != nil {
		return idx.PackedRecords.ModUnix != nil
	}
	for i := range idx.Records {
		if idx.Records[i].ModUnix != 0 {
			return true
		}
	}
	return false
}

func (idx *Index) compactHasAttrs() bool {
	return idx != nil && idx.CompactAttrs
}

func (idx *Index) compactRecord(i int) CompactRecord {
	if idx.MMapRecords != nil {
		return idx.MMapRecords.At(i)
	}
	if idx.PackedRecords != nil {
		return idx.PackedRecords.At(i)
	}
	if i < 0 || i >= len(idx.Records) {
		return CompactRecord{}
	}
	return idx.Records[i]
}

func (idx *Index) compactNameAt(i int) string {
	if idx.MMapRecords != nil {
		return idx.MMapRecords.nameAtRecord(i)
	}
	if idx.PackedRecords != nil {
		return idx.PackedRecords.nameAt(i)
	}
	if i < 0 || i >= len(idx.Records) {
		return ""
	}
	return idx.Records[i].Name
}

func (idx *Index) compactLowerNameAt(i int) string {
	if idx.MMapRecords != nil {
		return idx.MMapRecords.lowerNameAt(i)
	}
	if idx.PackedRecords != nil {
		return idx.PackedRecords.lowerNameAt(i)
	}
	return compactLowerName(idx.compactRecord(i))
}

func (idx *Index) setCompactRecord(i int, rec CompactRecord) {
	if idx.MMapRecords != nil {
		return
	}
	if idx.PackedRecords != nil {
		idx.PackedRecords.Set(i, rec)
	}
	if i >= 0 && i < len(idx.Records) {
		idx.Records[i] = rec
	}
}

func (idx *Index) appendCompactRecord(rec CompactRecord) int {
	id := idx.compactRecordCount()
	if idx.MMapRecords != nil {
		return -1
	}
	if idx.PackedRecords != nil {
		idx.PackedRecords.Append(rec)
	}
	if idx.PackedRecords == nil || idx.Records != nil {
		idx.Records = append(idx.Records, rec)
	}
	return id
}

func searchCompactWithCache(idx *Index, opts queryOptions, countOnly bool, pathCache map[int]string, candidateFn func(parsedQuery) ([]int, bool)) ([]Entry, error) {
	return searchCompactWithCacheHidden(idx, opts, countOnly, pathCache, candidateFn, hiddenBaseIDs{})
}

type hiddenBaseIDs struct {
	tombstone []int32
	shadowed  []int32
}

func (h hiddenBaseIDs) empty() bool {
	return len(h.tombstone) == 0 && len(h.shadowed) == 0
}

func (h hiddenBaseIDs) contains(id int) bool {
	if id < 0 {
		return false
	}
	id32 := int32(id)
	if pos := sort.Search(len(h.tombstone), func(i int) bool { return h.tombstone[i] >= id32 }); pos < len(h.tombstone) && h.tombstone[pos] == id32 {
		return true
	}
	if pos := sort.Search(len(h.shadowed), func(i int) bool { return h.shadowed[i] >= id32 }); pos < len(h.shadowed) && h.shadowed[pos] == id32 {
		return true
	}
	return false
}

func searchCompactWithCacheHidden(idx *Index, opts queryOptions, countOnly bool, pathCache map[int]string, candidateFn func(parsedQuery) ([]int, bool), hidden hiddenBaseIDs) ([]Entry, error) {
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, err
	}
	if pq.Impossible {
		pq.Trace.setPlannerMode("impossible-query")
		pq.Trace.setSource("impossible-query", 0)
		return []Entry{}, nil
	}
	if err := checkQueryCapabilities(pq, idx); err != nil {
		return nil, err
	}
	dropSatisfiedVolumeTerms(&pq, idx.Volume)
	limit := normalizedLimit(opts.Limit, countOnly)
	pq.Limit = limit
	pq.CountOnly = countOnly
	order := idx.CompactNameOrder
	if pq.SortColumn == "size" {
		order = uint32OrderToInts((&serviceVolumeIndex{index: idx}).sizeOrderForRank())
	} else if pq.SortColumn == "modified" {
		order = uint32OrderToInts((&serviceVolumeIndex{index: idx}).modifiedOrderForRank())
	} else if pq.SortColumn == "extension" {
		order = uint32OrderToInts((&serviceVolumeIndex{index: idx}).extensionOrderForRank())
	} else if pq.SortColumn == "type" {
		order = uint32OrderToInts((&serviceVolumeIndex{index: idx}).typeOrderForRank())
	} else if pq.SortColumn == "path" {
		order = uint32OrderToInts((&serviceVolumeIndex{index: idx}).pathOrderForRank())
	}
	usedCandidates := false
	if candidateFn != nil {
		if candidates, ok := candidateFn(pq); ok {
			if candidates == nil {
				candidates = []int{}
			}
			order = candidates
			usedCandidates = true
		}
	}
	if !usedCandidates && queryCanceled(pq) {
		return nil, errQueryCanceled
	}
	if !usedCandidates {
		pq.Trace.setSource("compact-name-order-scan", compactOrderLen(order, idx.compactRecordCount()))
	}
	if pq.RootBias != "" || pq.CWDBias != "" {
		order = idx.biasOrderCompact(order, firstNonEmpty(pq.CWDBias, pq.RootBias))
	}
	results := make([]Entry, 0, min(limit, 1024))
	if pathCache == nil {
		pathCache = make(map[int]string)
	}
	skipEntryMatches := compactCandidateCanSkipEntryMatches(pq, usedCandidates)
	if usedCandidates && !countOnly && len(order) >= serviceTrigramParallelVerifyMinIDs {
		return verifyCompactCandidateOrderParallel(idx, pq, order, pathCache, limit, skipEntryMatches, hidden)
	}
	for pos := 0; pos < compactOrderLen(order, idx.compactRecordCount()); pos++ {
		if pos&1023 == 0 && queryCanceled(pq) {
			return nil, errQueryCanceled
		}
		recIndex := compactOrderAt(order, pos)
		if hidden.contains(recIndex) {
			continue
		}
		rec := idx.compactRecord(recIndex)
		if rec.Deleted {
			continue
		}
		if !compactRecordPrecheck(rec, pq, pq.MatchPath) {
			continue
		}
		if queryPathTermPrecheckSafe(pq) && !idx.compactPathContainsAll(recIndex, pq.Terms) {
			continue
		}
		if skipEntryMatches {
			results = append(results, compactEntryFromRecord(idx, recIndex, rec, pathCache, false))
			if !countOnly && len(results) >= limit {
				break
			}
			continue
		}
		path := idx.reconstructCompactPathCached(recIndex, pathCache)
		entry := Entry{
			Path:        path,
			Name:        rec.Name,
			LowerPath:   strings.ToLower(path),
			LowerName:   idx.compactLowerNameAt(recIndex),
			Mode:        rec.Mode,
			Size:        rec.Size,
			ModUnix:     rec.ModUnix,
			IndexSource: idx.Source,
		}
		if entryMatches(entry, pq, pq.MatchPath) {
			results = append(results, Entry{
				Path:        entry.Path,
				Name:        entry.Name,
				LowerPath:   entry.LowerPath,
				LowerName:   entry.LowerName,
				Mode:        entry.Mode,
				Size:        entry.Size,
				ModUnix:     entry.ModUnix,
				IndexSource: entry.IndexSource,
			})
			if !countOnly && len(results) >= limit {
				break
			}
		}
	}
	return results, nil
}

func compactCandidateCanSkipEntryMatches(pq parsedQuery, usedCandidates bool) bool {
	if !usedCandidates {
		return false
	}
	if pq.Under != "" ||
		pq.Exists ||
		len(pq.Dirs) > 0 ||
		len(pq.Regexps) > 0 ||
		len(pq.Parents) > 0 ||
		len(pq.SizeFilters) > 0 ||
		len(pq.DateFilters) > 0 ||
		len(pq.AttrFilters) > 0 ||
		len(pq.OrGroups) > 0 ||
		len(pq.NotGroups) > 0 {
		return false
	}
	if len(pq.Terms) == 0 {
		return len(pq.Globs) == 0 &&
			pq.Type == "" &&
			!pq.HasModAfter &&
			pq.CWDBias == "" &&
			pq.RootBias == ""
	}
	return pq.MatchPath &&
		countNonVolumeTerms(pq.Terms) == 1 &&
		len(pq.Exts) == 0 &&
		len(pq.Globs) == 0 &&
		pq.Type == "" &&
		!pq.HasModAfter &&
		pq.CWDBias == "" &&
		pq.RootBias == ""
}

func compactEntryFromRecord(idx *Index, recIndex int, rec CompactRecord, pathCache map[int]string, withLower bool) Entry {
	path := idx.reconstructCompactPathCached(recIndex, pathCache)
	entry := Entry{
		Path:        path,
		Name:        rec.Name,
		Mode:        rec.Mode,
		Size:        rec.Size,
		ModUnix:     rec.ModUnix,
		IndexSource: idx.Source,
	}
	if withLower {
		entry.LowerPath = strings.ToLower(path)
		entry.LowerName = idx.compactLowerNameAt(recIndex)
	}
	return entry
}

func verifyCompactCandidateOrderParallel(idx *Index, pq parsedQuery, order []int, pathCache map[int]string, limit int, skipEntryMatches bool, hidden hiddenBaseIDs) ([]Entry, error) {
	if len(order) == 0 || limit <= 0 {
		return nil, nil
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, len(order)/serviceTrigramParallelVerifyMinIDs))
	if workers <= 1 {
		return verifyCompactCandidateOrderRange(idx, pq, order, pathCache, limit, skipEntryMatches, hidden)
	}
	parts := make([][]Entry, workers)
	var canceled atomic.Bool
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := worker * len(order) / workers
		end := (worker + 1) * len(order) / workers
		wg.Add(1)
		go func(worker, start, end int) {
			defer wg.Done()
			localCache := make(map[int]string)
			local := make([]Entry, 0, min(limit, end-start))
			for pos := start; pos < end; pos++ {
				if pos&1023 == 0 && queryCanceled(pq) {
					canceled.Store(true)
					return
				}
				recIndex := order[pos]
				if hidden.contains(recIndex) {
					continue
				}
				if entry, ok := compactCandidateEntryIfMatch(idx, pq, recIndex, localCache, true, skipEntryMatches); ok {
					local = append(local, entry)
					if len(local) >= limit {
						break
					}
				}
			}
			parts[worker] = local
		}(worker, start, end)
	}
	wg.Wait()
	if canceled.Load() {
		return nil, errQueryCanceled
	}
	out := make([]Entry, 0, min(limit, 1024))
	for _, part := range parts {
		for _, entry := range part {
			out = append(out, entry)
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

func verifyCompactCandidateOrderRange(idx *Index, pq parsedQuery, order []int, pathCache map[int]string, limit int, skipEntryMatches bool, hidden hiddenBaseIDs) ([]Entry, error) {
	out := make([]Entry, 0, min(limit, 1024))
	for pos, recIndex := range order {
		if pos&1023 == 0 && queryCanceled(pq) {
			return nil, errQueryCanceled
		}
		if hidden.contains(recIndex) {
			continue
		}
		if entry, ok := compactCandidateEntryIfMatch(idx, pq, recIndex, pathCache, true, skipEntryMatches); ok {
			out = append(out, entry)
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

func compactCandidateEntryIfMatch(idx *Index, pq parsedQuery, recIndex int, pathCache map[int]string, usedCandidates bool, skipEntryMatches bool) (Entry, bool) {
	rec := idx.compactRecord(recIndex)
	if rec.Deleted {
		return Entry{}, false
	}
	if !compactRecordPrecheck(rec, pq, pq.MatchPath) {
		return Entry{}, false
	}
	if queryPathTermPrecheckSafe(pq) && !idx.compactPathContainsAll(recIndex, pq.Terms) {
		return Entry{}, false
	}
	if skipEntryMatches {
		return compactEntryFromRecord(idx, recIndex, rec, pathCache, false), true
	}
	entry := compactEntryFromRecord(idx, recIndex, rec, pathCache, true)
	if !entryMatches(entry, pq, pq.MatchPath) {
		return Entry{}, false
	}
	return entry, true
}

func dropSatisfiedVolumeTerms(pq *parsedQuery, volume string) {
	if pq == nil || volume == "" || len(pq.Terms) == 0 {
		return
	}
	out := pq.Terms[:0]
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) && strings.EqualFold(term, volume) {
			continue
		}
		out = append(out, term)
	}
	pq.Terms = out
}

var errQueryCanceled = errors.New("query superseded")

var errGlobalMultiVolumePlannerDeclined = errors.New("global planner declined multi-volume query")

func globalMultiVolumePlannerDeclineError(opts queryOptions) error {
	if queryCanceled(parsedQuery{DeadlineUnix: opts.DeadlineUnix, Cancel: opts.Cancel}) {
		return errQueryCanceled
	}
	if opts.Trace != nil && opts.Trace.Decline != "" {
		return fmt.Errorf("%w: %s", errGlobalMultiVolumePlannerDeclined, opts.Trace.Decline)
	}
	return errGlobalMultiVolumePlannerDeclined
}

func queryCanceled(pq parsedQuery) bool {
	if pq.Cancel != nil && pq.Cancel() {
		return true
	}
	return pq.DeadlineUnix > 0 && time.Now().UnixNano() > pq.DeadlineUnix
}

// checkQueryCapabilities rejects queries whose filters need data the index does
// not carry. Older indexes can lack file sizes or modification times, so size:,
// dm:, --recent, and --modified-after would otherwise silently match nothing.
// Failing loudly is consistent with rejecting unknown filters at parse time.
func checkQueryCapabilities(pq parsedQuery, idx *Index) error {
	needsSize, needsMod := queryNeedsSizeOrMod(pq)
	if needsSize && !idx.compactHasSize() {
		return errors.New("size: filters require an index with file sizes; the current index has none (rebuild with a size-capable indexer)")
	}
	if needsMod && !idx.compactHasModTime() {
		return errors.New("dm:/--recent/--modified-after require an index with modification times; the current index has none")
	}
	if queryNeedsAttrs(pq) && !idx.compactHasAttrs() {
		return errors.New("attrib: filters require an index with file attributes; rebuild the index with a current seekfs version")
	}
	return nil
}

func queryNeedsSizeOrMod(pq parsedQuery) (size bool, mod bool) {
	if len(pq.SizeFilters) > 0 {
		size = true
	}
	if pq.SortColumn == "size" {
		size = true
	}
	if pq.SortColumn == "modified" {
		mod = true
	}
	if len(pq.DateFilters) > 0 || pq.HasModAfter {
		mod = true
	}
	for _, group := range pq.OrGroups {
		for _, alt := range group {
			s, m := queryNeedsSizeOrMod(alt)
			size = size || s
			mod = mod || m
		}
	}
	for _, neg := range pq.NotGroups {
		s, m := queryNeedsSizeOrMod(neg)
		size = size || s
		mod = mod || m
	}
	return size, mod
}

func queryNeedsAttrs(pq parsedQuery) bool {
	if len(pq.AttrFilters) > 0 {
		return true
	}
	for _, group := range pq.OrGroups {
		for _, alt := range group {
			if queryNeedsAttrs(alt) {
				return true
			}
		}
	}
	for _, neg := range pq.NotGroups {
		if queryNeedsAttrs(neg) {
			return true
		}
	}
	return false
}

func compactRecordPrecheck(rec CompactRecord, pq parsedQuery, matchPath bool) bool {
	name := rec.Name
	cmpName := normalizeCase(name, pq.CaseSensitive)
	if !matchPath && !containsAll(cmpName, pq.Terms) {
		return false
	}
	if pq.Type == "file" && rec.Mode&uint32(os.ModeDir) != 0 {
		return false
	}
	if pq.Type == "dir" && rec.Mode&uint32(os.ModeDir) == 0 {
		return false
	}
	if !attrFiltersMatch(rec.Mode, pq.AttrFilters) {
		return false
	}
	if pq.HasModAfter {
		if rec.ModUnix == 0 || !time.Unix(0, rec.ModUnix).After(pq.ModifiedAfter) {
			return false
		}
	}
	for _, ext := range pq.Exts {
		actual := strings.TrimPrefix(filepath.Ext(name), ".")
		if normalizeCase(actual, pq.CaseSensitive) != ext {
			return false
		}
	}
	for _, glob := range pq.Globs {
		ok, err := filepath.Match(glob, cmpName)
		if err != nil || !ok {
			return false
		}
	}
	return true
}

func (vol *serviceVolumeIndex) nameTermCandidates(pq parsedQuery) ([]int, bool) {
	if !pq.CountOnly && !pq.MatchPath && (len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0) {
		if candidates, ok := vol.plannedCandidates(pq); ok {
			pq.Trace.setSource("planned:boolean", len(candidates))
			return candidates, true
		}
	}
	if pq.CountOnly {
		if candidates, ok := vol.boundedScanCandidates(pq); ok {
			pq.Trace.setSource("bounded-scan", len(candidates))
			return candidates, true
		}
	}
	if len(pq.Terms) == 0 && len(pq.Exts) == 0 && len(pq.Globs) == 0 && len(pq.OrGroups) == 0 {
		if candidates, ok := vol.boundedScanCandidates(pq); ok {
			pq.Trace.setSource("bounded-scan", len(candidates))
			return candidates, true
		}
	}
	if queryHasNonASCIIPlainTerm(pq) {
		if candidates, ok := vol.boundedScanCandidates(pq); ok {
			pq.Trace.setSource("bounded-scan", len(candidates))
			return candidates, true
		}
	}
	if pq.MatchPath {
		if candidates, ok := vol.extensionShapedPathTopCandidates(pq); ok {
			pq.Trace.setSource("path-extension-top", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.bareExtensionMultiPathTopCandidates(pq); ok {
			pq.Trace.setSource("path-bare-extension-multi-top", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.selectiveNamePathTermCandidates(pq); ok {
			pq.Trace.setSource("path-selective-name-term", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.pathDirectoryTermTopCandidates(pq); ok {
			pq.Trace.setSource("path-directory-term-top", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.componentDirectTopCandidates(pq); ok {
			pq.Trace.setSource("path-component-direct-top", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.componentRootTopCandidates(pq); ok {
			pq.Trace.setSource("path-component-root-top", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.componentMultiTermTopCandidates(pq); ok {
			pq.Trace.setSource("path-component-multi-top", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.multiTermEmptyPathCandidates(pq); ok {
			pq.Trace.setSource("path-multi-empty", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.nameTrigramCandidates(pq); ok {
			if compactCandidateCanSkipEntryMatches(pq, true) && pq.Limit > 0 {
				candidates = topCandidateIDsByRank(candidates, pq.Limit, vol.index, vol.rankForQuery(pq))
			} else {
				sortCandidateIDs(candidates, pq, vol.index, vol.rankForQuery(pq))
			}
			pq.Trace.setSource("path-component-trigram", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.nameTrigramPathNameTopCandidates(pq); ok {
			pq.Trace.setSource("path-name-trigram-top", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.limitedPathTermCandidates(pq); ok {
			pq.Trace.setSource("path-term-limited", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.limitedDottedPathScanCandidates(pq); ok {
			pq.Trace.setSource("path-dotted-limited-scan", len(candidates))
			return candidates, true
		}
	} else {
		if candidates, ok := vol.nameTrigramCandidates(pq); ok {
			sortCandidateIDs(candidates, pq, vol.index, vol.rankForQuery(pq))
			pq.Trace.setSource("name-trigram", len(candidates))
			return candidates, true
		}
	}
	if candidates, ok := vol.limitedSingleTermCandidates(pq); ok {
		pq.Trace.setSource("limited-single-term", len(candidates))
		return candidates, true
	}
	if !pq.MatchPath {
		if candidates, ok := vol.plannedCandidates(pq); ok {
			pq.Trace.setSource("planned", len(candidates))
			return candidates, true
		}
	}
	if pq.MatchPath {
		if candidates, ok := vol.nameTrigramCandidates(pq); ok {
			if compactCandidateCanSkipEntryMatches(pq, true) && pq.Limit > 0 {
				candidates = topCandidateIDsByRank(candidates, pq.Limit, vol.index, vol.rankForQuery(pq))
			} else {
				sortCandidateIDs(candidates, pq, vol.index, vol.rankForQuery(pq))
			}
			pq.Trace.setSource("path-component-trigram", len(candidates))
			return candidates, true
		}
	}
	if candidates, ok := vol.plannedCandidates(pq); ok {
		pq.Trace.setSource("planned", len(candidates))
		return candidates, true
	}
	if pq.MatchPath {
		if candidates, ok := vol.componentTrigramCandidates(pq); ok {
			pq.Trace.setSource("component-trigram", len(candidates))
			return candidates, true
		}
	}
	if candidates, ok := vol.underCandidates(pq); ok {
		pq.Trace.setSource("under", len(candidates))
		return candidates, true
	}
	if candidates, ok := vol.boundedScanCandidates(pq); ok {
		pq.Trace.setSource("bounded-scan", len(candidates))
		return candidates, true
	}
	if candidates, ok := vol.plannerCandidates(pq); ok {
		pq.Trace.setSource("legacy-planner", len(candidates))
		return candidates, true
	}
	if candidates, ok := vol.pathDirFilterCandidates(pq); ok {
		pq.Trace.setSource("path-dir-filter", len(candidates))
		return candidates, true
	}
	if candidates, ok := vol.filterCandidates(pq); ok {
		pq.Trace.setSource("filter", len(candidates))
		return candidates, true
	}
	if candidates, ok := vol.pathRootLimitedCandidates(pq); ok {
		pq.Trace.setSource("path-root-limited", len(candidates))
		return candidates, true
	}
	if candidates, ok := vol.pathTermSubtreeCandidates(pq); ok {
		pq.Trace.setSource("path-term-subtree", len(candidates))
		return candidates, true
	}
	if candidates, ok := vol.regexLiteralCandidates(pq); ok {
		pq.Trace.setSource("regex-literal", len(candidates))
		return candidates, true
	}
	if candidates, ok := vol.exactDirCandidates(pq); ok {
		pq.Trace.setSource("exact-dir", len(candidates))
		return candidates, true
	}
	if candidates, ok := vol.exactNameCandidates(pq); ok {
		pq.Trace.setSource("exact-name", len(candidates))
		return candidates, true
	}
	if candidates, ok := vol.namePrefixCandidates(pq); ok {
		pq.Trace.setSource("name-prefix", len(candidates))
		return candidates, true
	}
	if vol == nil || vol.index == nil || len(pq.Terms) == 0 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || pq.Under != "" {
		return nil, false
	}
	if len(pq.Terms) > 1 && !pq.CaseSensitive && !pq.MatchPath {
		if candidates, ok := vol.cachedMultiNameTermCandidates(pq.Terms); ok {
			pq.Trace.setSource("cached-multi-name-term", len(candidates))
			return candidates, true
		}
		candidates := vol.multiNameTermCandidates(pq.Terms)
		pq.Trace.setSource("multi-name-term", len(candidates))
		return candidates, true
	}
	lists := make([][]int, 0, len(pq.Terms))
	for _, term := range pq.Terms {
		list := vol.nameTermPosting(term)
		if len(list) == 0 {
			return []int{}, true
		}
		lists = append(lists, list)
	}
	sortIntListsByLen(lists)
	candidates := append([]int(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSortedInts(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	pq.Trace.setSource("name-term-posting", len(candidates))
	return candidates, true
}

func (vol *serviceVolumeIndex) overlayAwareNameTermCandidates(pq parsedQuery) ([]int, bool) {
	candidates, ok := vol.nameTermCandidates(pq)
	if !ok {
		return nil, false
	}
	if len(candidates) == 0 {
		return nil, false
	}
	return candidates, true
}

func queryHasNonASCIIPlainTerm(pq parsedQuery) bool {
	for _, term := range pq.Terms {
		for _, r := range term {
			if r > 127 {
				return true
			}
		}
	}
	for _, group := range pq.OrGroups {
		for _, alt := range group {
			if queryHasNonASCIIPlainTerm(alt) {
				return true
			}
		}
	}
	for _, neg := range pq.NotGroups {
		if queryHasNonASCIIPlainTerm(neg) {
			return true
		}
	}
	return false
}

func queryPathTermPrecheckSafe(pq parsedQuery) bool {
	return pq.MatchPath && len(pq.Terms) > 0 && !queryHasNonASCIIPlainTerm(pq)
}

func (vol *serviceVolumeIndex) componentRootTopCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CountOnly || pq.Limit <= 0 ||
		pq.CaseSensitive || pq.Under != "" || pq.Type != "" || len(pq.Exts) > 0 ||
		len(pq.Globs) > 0 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 || pq.HasModAfter || pq.Exists ||
		pq.CWDBias != "" || pq.RootBias != "" || countNonVolumeTerms(pq.Terms) != 1 {
		return nil, false
	}
	term := ""
	for _, candidate := range pq.Terms {
		if isVolumeQueryTerm(candidate) {
			continue
		}
		term = candidate
		break
	}
	if term == "" || strings.ContainsAny(term, `\/*?[]:.-`) {
		return nil, false
	}
	var roots []int
	if it, _, ok := vol.componentPostingBlockIterator(term); ok {
		ordinal := 0
		for it.next < it.end {
			block, _, ok := it.nextBlock()
			if !ok {
				return nil, false
			}
			for _, id32 := range block {
				if ordinal&1023 == 0 && queryCanceled(pq) {
					return nil, false
				}
				ordinal++
				rootID := int(id32)
				if vol.estimatedDescendantOrSelfCount(rootID) >= 100_000 {
					roots = append(roots, rootID)
					if len(roots) >= pq.Limit*4 {
						break
					}
				}
			}
			if len(roots) >= pq.Limit*4 {
				break
			}
		}
	} else {
		for i, id32 := range vol.componentPosting32(term) {
			if i&1023 == 0 && queryCanceled(pq) {
				return nil, false
			}
			rootID := int(id32)
			if vol.estimatedDescendantOrSelfCount(rootID) >= 100_000 {
				roots = append(roots, rootID)
				if len(roots) >= pq.Limit*4 {
					break
				}
			}
		}
	}
	if len(roots) == 0 && vol.queryIndex == nil {
		for _, rootID := range vol.pathComponentRootIDs(term) {
			if vol.estimatedDescendantOrSelfCount(rootID) >= 100_000 {
				roots = append(roots, rootID)
			}
		}
	}
	if len(roots) == 0 {
		return nil, false
	}
	if serviceLowMemoryMode() && (len(vol.subtreeStart) == 0 || len(vol.subtreeEnd) == 0 || len(vol.subtreeOrder) == 0) {
		return topCandidateIDsByRank(roots, pq.Limit, vol.index, vol.rankForQuery(pq)), true
	}
	recordCount := vol.index.compactRecordCount()
	out := make([]int, 0, pq.Limit)
	seen := make(map[int]struct{}, pq.Limit)
	add := func(id int) bool {
		if id < 0 || id >= recordCount {
			return false
		}
		if _, ok := seen[id]; ok {
			return false
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			return false
		}
		seen[id] = struct{}{}
		out = append(out, id)
		return len(out) >= pq.Limit
	}
	for rootPos, rootID := range roots {
		if rootPos&127 == 0 && queryCanceled(pq) {
			return nil, false
		}
		if add(rootID) {
			return out, true
		}
		if rootID < 0 || rootID >= len(vol.subtreeStart) || rootID >= len(vol.subtreeEnd) {
			continue
		}
		start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start == ^uint32(0) || start >= end || int(end) > len(vol.subtreeOrder) {
			continue
		}
		for pos := start; pos < end; pos++ {
			if pos&4095 == 0 && queryCanceled(pq) {
				return nil, false
			}
			if add(int(vol.subtreeOrder[pos])) {
				return out, true
			}
		}
	}
	return out, len(out) > 0
}

func (vol *serviceVolumeIndex) componentDirectTopCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || vol.queryIndex == nil ||
		!serviceLowMemoryMode() || !pq.MatchPath || pq.CountOnly || pq.Limit <= 0 || pq.CaseSensitive ||
		pq.Under != "" || pq.Type != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || len(pq.SizeFilters) > 0 ||
		len(pq.DateFilters) > 0 || len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		pq.HasModAfter || pq.Exists || pq.CWDBias != "" || pq.RootBias != "" ||
		countNonVolumeTerms(pq.Terms) != 1 {
		return nil, false
	}
	term := ""
	for _, candidate := range pq.Terms {
		if isVolumeQueryTerm(candidate) {
			continue
		}
		term = candidate
		break
	}
	if len(term) < 3 || strings.ContainsAny(term, `\/*?[]:`) {
		return nil, false
	}
	candidates := vol.componentPosting32(term)
	if len(candidates) == 0 {
		if len(vol.extPosting32(term)) > 0 {
			return nil, false
		}
		if len(vol.queryIndex.pathGrams) == 0 {
			return nil, false
		}
		grams := uniqueTrigramKeys(term)
		if len(grams) == 0 {
			return nil, false
		}
		for _, gram := range grams {
			list := vol.queryIndex.pathGrams[trigramStringFromKey(gram)]
			if len(list) == 0 {
				return nil, false
			}
			if candidates == nil || len(list) < len(candidates) {
				candidates = list
			}
		}
		for _, gram := range grams {
			list := vol.queryIndex.pathGrams[trigramStringFromKey(gram)]
			if len(list) == 0 || sameUint32Slice(list, candidates) {
				continue
			}
			candidates = intersectSortedUint32s(candidates, list)
			if len(candidates) == 0 {
				return nil, false
			}
		}
	}
	recordCount := vol.index.compactRecordCount()
	out := make([]int, 0, pq.Limit)
	seen := make(map[int]struct{}, pq.Limit)
	add := func(id int) bool {
		if id < 0 || id >= recordCount {
			return false
		}
		if _, ok := seen[id]; ok {
			return false
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			return false
		}
		seen[id] = struct{}{}
		out = append(out, id)
		return len(out) >= pq.Limit
	}
	for _, id32 := range candidates {
		id := int(id32)
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 || !containsFoldASCII(vol.index.compactNameAt(id), term) {
			continue
		}
		if add(id) {
			return out, true
		}
		if id >= 0 && id < len(vol.subtreeStart) && id < len(vol.subtreeEnd) && len(vol.subtreeOrder) > 0 {
			start, end := vol.subtreeStart[id], vol.subtreeEnd[id]
			if start != ^uint32(0) && start <= end && int(end) <= len(vol.subtreeOrder) {
				for pos := start; pos < end; pos++ {
					if add(int(vol.subtreeOrder[pos])) {
						return out, true
					}
				}
			}
		}
	}
	if len(out) > 0 && len(out) < pq.Limit && len(vol.subtreeOrder) == 0 {
		roots := append([]int(nil), out...)
		scanned := vol.scanOrderedLimited(pq, pq.Limit-len(out), func(id int) bool {
			if _, ok := seen[id]; ok {
				return false
			}
			return vol.isDescendantOrSelfAnyFast(id, roots) && vol.index.compactPathContainsTerm(id, term)
		})
		for _, id := range scanned {
			if add(id) {
				return out, true
			}
		}
	}
	return out, len(out) > 0
}

func (vol *serviceVolumeIndex) componentMultiTermTopCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || vol.queryIndex == nil || !vol.hasDescendantIndex() ||
		!serviceLowMemoryMode() || !pq.MatchPath || pq.CountOnly || pq.Limit <= 0 || pq.CaseSensitive ||
		pq.Under != "" || pq.Type != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || len(pq.SizeFilters) > 0 ||
		len(pq.DateFilters) > 0 || len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		pq.HasModAfter || pq.Exists || pq.CWDBias != "" || pq.RootBias != "" ||
		countNonVolumeTerms(pq.Terms) < 2 {
		return nil, false
	}
	var best []int
	bestEstimate := int(^uint(0) >> 1)
	for _, term := range pq.Terms {
		if len(term) < 3 || isVolumeQueryTerm(term) || strings.ContainsAny(term, `\/*?[]:.`) ||
			vol.pathTermIsUsableExtensionCandidate(term) {
			continue
		}
		nameMatches, roots, complete := vol.pathDirectoryTermSource(term)
		if complete && len(nameMatches) == 0 {
			return []int{}, true
		}
		if len(roots) == 0 {
			for _, id32 := range vol.componentPosting32(term) {
				roots = append(roots, int(id32))
			}
		}
		if len(roots) == 0 {
			continue
		}
		estimate := 0
		for _, id := range roots {
			if id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			if len(vol.subtreeOrder) == 0 && (len(vol.childOffsets) > 0 || vol.children != nil) {
				estimate += len(vol.underDescendantsLimited(id, serviceComponentMultiTermScanMaxIDs+1))
			} else {
				estimate += vol.estimatedDescendantOrSelfCount(id)
			}
			if estimate > bestEstimate {
				break
			}
		}
		if estimate > 0 && estimate < bestEstimate {
			best = roots
			bestEstimate = estimate
		}
	}
	if len(best) == 0 {
		return nil, false
	}
	if len(vol.subtreeOrder) == 0 {
		var bestTerm string
		for _, term := range pq.Terms {
			if len(term) < 3 || isVolumeQueryTerm(term) || strings.ContainsAny(term, `\/*?[]:.`) ||
				vol.pathTermIsUsableExtensionCandidate(term) {
				continue
			}
			_, roots, _ := vol.pathDirectoryTermSource(term)
			if len(roots) == 0 {
				for _, id32 := range vol.componentPosting32(term) {
					roots = append(roots, int(id32))
				}
			}
			if len(roots) == len(best) {
				same := true
				for i := range roots {
					if roots[i] != best[i] {
						same = false
						break
					}
				}
				if same {
					bestTerm = term
					break
				}
			}
		}
		if bestTerm != "" && bestEstimate <= serviceComponentMultiTermScanMaxIDs {
			seenCandidates := make(map[int]struct{}, len(best))
			candidates := make([]int, 0, min(bestEstimate, serviceComponentMultiTermScanMaxIDs))
			for _, id := range best {
				for _, candidate := range vol.underDescendantsLimited(id, serviceComponentMultiTermScanMaxIDs+1) {
					if _, ok := seenCandidates[candidate]; ok {
						continue
					}
					seenCandidates[candidate] = struct{}{}
					candidates = append(candidates, candidate)
				}
			}
			out := make([]int, 0, min(len(candidates), pq.Limit))
			for _, id := range candidates {
				if id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				rec := vol.index.compactRecord(id)
				if rec.Deleted || !vol.index.compactPathContainsAll(id, pq.Terms) {
					continue
				}
				out = append(out, id)
			}
			if len(out) > 0 {
				sortCandidateIDs(out, pq, vol.index, vol.rankForQuery(pq))
				return out, true
			}
		}
		roots := make([]int, 0, min(len(best), 16))
		for _, id := range best {
			if id >= 0 && id < vol.index.compactRecordCount() {
				roots = append(roots, id)
				if len(roots) >= 16 {
					break
				}
			}
		}
		if len(roots) == 0 {
			return nil, false
		}
		out := vol.scanOrderedLimited(pq, pq.Limit, func(id int) bool {
			if !vol.isDescendantOrSelfAnyFast(id, roots) {
				return false
			}
			rec := vol.index.compactRecord(id)
			return !rec.Deleted && vol.index.compactPathContainsAll(id, pq.Terms)
		})
		if len(out) >= pq.Limit {
			return out, true
		}
		return nil, false
	}
	recordCount := vol.index.compactRecordCount()
	out := make([]int, 0, pq.Limit)
	seen := make(map[int]struct{}, pq.Limit)
	scanned := 0
	add := func(id int) bool {
		if id < 0 || id >= recordCount {
			return false
		}
		scanned++
		if _, ok := seen[id]; ok {
			return false
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !vol.index.compactPathContainsAll(id, pq.Terms) {
			return false
		}
		seen[id] = struct{}{}
		out = append(out, id)
		return len(out) >= pq.Limit
	}
	for rootIndex, id := range best {
		if rootIndex&127 == 0 && queryCanceled(pq) {
			return nil, false
		}
		if add(id) {
			return out, true
		}
		if id < 0 || id >= len(vol.subtreeStart) || id >= len(vol.subtreeEnd) || len(vol.subtreeOrder) == 0 {
			continue
		}
		start, end := vol.subtreeStart[id], vol.subtreeEnd[id]
		if start == ^uint32(0) || start > end || int(end) > len(vol.subtreeOrder) {
			continue
		}
		for pos := start; pos < end; pos++ {
			if bestEstimate > serviceComponentTrigramExpansionMaxIDs && scanned >= serviceComponentMultiTermScanMaxIDs {
				if len(out) >= pq.Limit {
					return out, true
				}
				return nil, false
			}
			if pos&4095 == 0 && queryCanceled(pq) {
				return nil, false
			}
			if add(int(vol.subtreeOrder[pos])) {
				return out, true
			}
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func (vol *serviceVolumeIndex) selectiveNamePathTermCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CountOnly || pq.Limit <= 0 || pq.CaseSensitive ||
		pq.Under != "" || pq.Type != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || len(pq.SizeFilters) > 0 ||
		len(pq.DateFilters) > 0 || len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		pq.HasModAfter || pq.Exists || pq.CWDBias != "" || pq.RootBias != "" ||
		countNonVolumeTerms(pq.Terms) < 2 {
		return nil, false
	}
	bestTerm := ""
	bestIDs := []int(nil)
	for _, term := range pq.Terms {
		if len(term) < 4 || isVolumeQueryTerm(term) || strings.ContainsAny(term, `\/*?[]:`) || !filenameLikePathTerm(term) {
			continue
		}
		ids, ok := vol.nameTrigramNameTermPostingLimited(term, servicePathNameTrigramCandidateMaxIDs)
		if !ok || len(ids) > serviceComponentTrigramExpansionMaxIDs {
			continue
		}
		if vol.hasDirectoryCandidate(ids) {
			continue
		}
		if bestTerm == "" || len(ids) < len(bestIDs) {
			bestTerm = term
			bestIDs = ids
		}
	}
	if bestTerm == "" {
		return nil, false
	}
	out := make([]int, 0, min(pq.Limit, len(bestIDs)))
	for _, id := range bestIDs {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !vol.index.compactPathContainsAll(id, pq.Terms) {
			continue
		}
		out = append(out, id)
		if len(out) >= pq.Limit {
			break
		}
	}
	if len(out) == 0 {
		return []int{}, true
	}
	return topCandidateIDsByRank(out, pq.Limit, vol.index, vol.rankForQuery(pq)), true
}

func filenameLikePathTerm(term string) bool {
	return strings.ContainsAny(term, "._-")
}

func asciiOnlyString(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func (vol *serviceVolumeIndex) multiTermEmptyPathCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CountOnly || pq.CaseSensitive ||
		pq.Under != "" || pq.Type != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || len(pq.SizeFilters) > 0 ||
		len(pq.DateFilters) > 0 || len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		pq.HasModAfter || pq.Exists || pq.CWDBias != "" || pq.RootBias != "" ||
		countNonVolumeTerms(pq.Terms) < 2 {
		return nil, false
	}
	for _, term := range pq.Terms {
		if len(term) < 3 || isVolumeQueryTerm(term) || strings.ContainsAny(term, `\/*?[]:`) {
			continue
		}
		if !asciiOnlyString(term) {
			return nil, false
		}
		if len(term) >= 6 && vol.queryIndex != nil && len(vol.componentPosting32(term)) == 0 {
			if trigrams := vol.nameTrigramIndex(); trigrams != nil {
				_, ok, missing := trigrams.selectiveIntersectCandidateIDs(term, servicePathNameTrigramCandidateMaxIDs)
				if ok && missing {
					return []int{}, true
				}
			}
		}
	}
	return nil, false
}

func (vol *serviceVolumeIndex) nameTrigramCandidates(pq parsedQuery) ([]int, bool) {
	if pq.MatchPath {
		return vol.componentTrigramCandidates(pq)
	}
	return vol.filenameTrigramCandidates(pq)
}

func (vol *serviceVolumeIndex) filenameTrigramCandidates(pq parsedQuery) ([]int, bool) {
	trigrams := vol.nameTrigramIndex()
	if vol == nil || vol.index == nil || (trigrams == nil && vol.index.Derived.SelfNameTrigrams == nil) || pq.CaseSensitive ||
		pq.MatchPath || len(pq.Terms) == 0 || pq.Under != "" || pq.Type != "" ||
		len(pq.Exts) > 0 || len(pq.Globs) > 0 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 || pq.HasModAfter || pq.Exists ||
		pq.CWDBias != "" || pq.RootBias != "" {
		return nil, false
	}
	return vol.filenameNgramCandidates(pq, trigrams, serviceNameTrigramCandidateMaxIDs)
}

func (vol *serviceVolumeIndex) filenameNgramCandidates(pq parsedQuery, trigrams *compressedTrigramIndex, maxIDs int) ([]int, bool) {
	if vol == nil {
		return nil, false
	}
	if trigrams == nil {
		trigrams = vol.index.Derived.SelfNameTrigrams
	}
	if trigrams == nil {
		return nil, false
	}
	bestTerm := ""
	bestCount := maxIDs + 1
	exactEmpty := false
	exactEmptyTerm := ""
	for _, term := range pq.Terms {
		if len(term) < max(3, trigrams.gramSize) {
			continue
		}
		if !asciiOnlyString(term) {
			return nil, false
		}
		termBest := maxIDs + 1
		termMissing := false
		termExactEmpty := false
		for _, gram := range trigrams.termGramKeys(strings.ToLower(term)) {
			_, count, stored, state, isExactEmpty := vol.nameGramPosting(gram)
			if isExactEmpty {
				termExactEmpty = true
				break
			}
			if !stored {
				termMissing = true
				if state == "omitted-common" || state == "missing-section" {
					break
				}
				continue
			}
			if count < termBest {
				termBest = count
			}
		}
		if termExactEmpty {
			exactEmpty = true
			exactEmptyTerm = term
			continue
		}
		if termMissing || termBest > maxIDs {
			continue
		}
		if termBest < bestCount {
			bestTerm = term
			bestCount = termBest
		}
	}
	if exactEmpty {
		// A complete PNGR count table proves that at least one required gram
		// has no base records.  Do not turn that fact into a recent-match or
		// bounded-scan fallback: overlays are merged by the caller, while the
		// persisted base candidate set is exactly empty.
		recent := vol.nameTrigramRecentMatches(exactEmptyTerm)
		pq.Trace.setSource("exact-empty", len(recent))
		return recent, true
	}
	if bestTerm == "" {
		for _, term := range pq.Terms {
			state := ""
			for _, gram := range trigrams.termGramKeys(strings.ToLower(term)) {
				_, _, _, gramState, _ := vol.nameGramPosting(gram)
				if gramState == "omitted-common" || gramState == "missing-section" {
					state = gramState
					break
				}
			}
			if state != "" {
				pq.Trace.setDecline("name-trigram:" + state)
				break
			}
		}
		return nil, false
	}
	candidates, ok := vol.nameNgramNameTermPosting(bestTerm, trigrams, maxIDs)
	if !ok {
		pq.Trace.setDecline("name-trigram:" + trigrams.lookupState(bestTerm))
		return nil, false
	}
	if len(candidates) > maxIDs {
		return nil, false
	}
	return candidates, true
}

func (vol *serviceVolumeIndex) componentTrigramCandidates(pq parsedQuery) ([]int, bool) {
	trigrams := vol.nameTrigramIndex()
	if vol == nil || vol.index == nil {
		pq.Trace.setDecline("component-trigram:no-volume")
		return nil, false
	}
	if trigrams == nil {
		pq.Trace.setDecline("component-trigram:not-ready")
		return nil, false
	}
	if pq.CaseSensitive || !pq.MatchPath || len(pq.Terms) == 0 || pq.Under != "" || pq.Type != "" ||
		len(pq.Exts) > 0 || len(pq.Globs) > 0 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 || pq.HasModAfter || pq.Exists ||
		pq.CWDBias != "" || pq.RootBias != "" {
		pq.Trace.setDecline("component-trigram:unsupported-query")
		return nil, false
	}
	bestTerm := ""
	bestCount := serviceComponentTrigramCandidateMaxIDs + 1
	missingTerm := ""
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) {
			continue
		}
		if len(term) < 3 {
			continue
		}
		if !asciiOnlyString(term) {
			pq.Trace.setDecline("component-trigram:non-ascii-term")
			return nil, false
		}
		count, ok := trigrams.postingCount(term)
		if !ok {
			pq.Trace.setDecline("component-trigram:no-posting-count")
			continue
		}
		if count == 0 {
			missingTerm = term
			continue
		}
		if count > serviceComponentTrigramCandidateMaxIDs {
			if len(term) < 6 {
				continue
			}
			count = serviceComponentTrigramCandidateMaxIDs
		}
		if count < bestCount {
			bestTerm = term
			bestCount = count
		}
	}
	if bestTerm == "" {
		if missingTerm != "" {
			candidates, ok := vol.nameTrigramPathTermPosting(missingTerm)
			if !ok {
				pq.Trace.setDecline("component-trigram:" + trigrams.lookupState(missingTerm))
				return nil, false
			}
			if len(candidates) > serviceComponentTrigramExpansionMaxIDs {
				pq.Trace.setDecline("component-trigram:missing-term-expanded-too-large")
				return nil, false
			}
			return candidates, true
		}
		for _, term := range pq.Terms {
			if state := trigrams.lookupState(term); state == "omitted-common" || state == "missing-section" {
				pq.Trace.setDecline("component-trigram:" + state)
				break
			}
		}
		pq.Trace.setDecline("component-trigram:no-selective-term")
		return nil, false
	}
	candidates, ok := vol.nameTrigramPathTermPosting(bestTerm)
	if !ok {
		pq.Trace.setDecline("component-trigram:" + trigrams.lookupState(bestTerm))
		return nil, false
	}
	if len(candidates) > serviceComponentTrigramExpansionMaxIDs {
		pq.Trace.setDecline("component-trigram:expanded-too-large")
		return nil, false
	}
	return candidates, true
}

func (vol *serviceVolumeIndex) nameTrigramPathNameTopCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || pq.CaseSensitive ||
		!pq.MatchPath || pq.CountOnly || pq.Limit <= 0 || len(pq.Terms) == 0 ||
		pq.Under != "" || pq.Type != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || len(pq.OrGroups) > 0 ||
		len(pq.NotGroups) > 0 || pq.HasModAfter || pq.Exists ||
		pq.CWDBias != "" || pq.RootBias != "" || countNonVolumeTerms(pq.Terms) != 1 {
		pq.Trace.replaceDecline("path-name-trigram-top:unsupported-query")
		return nil, false
	}
	term := ""
	for _, candidate := range pq.Terms {
		if isVolumeQueryTerm(candidate) {
			continue
		}
		term = candidate
		break
	}
	if len(term) < 6 || strings.ContainsAny(term, `\/*?[]:`) {
		pq.Trace.replaceDecline("path-name-trigram-top:bad-term")
		return nil, false
	}
	nameMatches, ok := vol.nameTrigramNameTermTopPosting(term, servicePathNameTrigramCandidateMaxIDs, servicePathNameTrigramCandidateMaxIDs)
	if !ok || len(nameMatches) == 0 {
		pq.Trace.replaceDecline("path-name-trigram-top:" + vol.nameTrigramIndex().lookupState(term))
		return nil, false
	}
	direct := make([]int, 0, len(nameMatches))
	seen := make(map[int]struct{}, len(nameMatches)+pq.Limit)
	sawUnexpandedDir := false
	for _, id := range nameMatches {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			continue
		}
		if _, exists := seen[id]; !exists {
			seen[id] = struct{}{}
			direct = append(direct, id)
		}
		if rec.Mode&uint32(os.ModeDir) != 0 && vol.estimatedDescendantOrSelfCount(id) > serviceComponentTrigramExpansionMaxIDs {
			direct = vol.appendTopSubtreeCandidatesByRank(direct, seen, id, pq.Limit*4)
		} else if rec.Mode&uint32(os.ModeDir) != 0 {
			sawUnexpandedDir = true
		}
	}
	if sawUnexpandedDir {
		pq.Trace.replaceDecline("path-name-trigram-top:directory-needs-expansion")
		return nil, false
	}
	if len(direct) < pq.Limit {
		pq.Trace.replaceDecline("path-name-trigram-top:too-few-direct")
		return nil, false
	}
	return topCandidateIDsByRank(direct, pq.Limit, vol.index, vol.rankForQuery(pq)), true
}

func (vol *serviceVolumeIndex) nameTrigramNameTermTopPosting(term string, maxIDs, limit int) ([]int, bool) {
	trigrams := vol.nameTrigramIndex()
	if vol == nil || trigrams == nil || limit <= 0 || !asciiOnlyString(term) {
		return nil, false
	}
	ids, ok, missing := trigrams.selectiveCandidateIDs(term, maxIDs)
	if len(term) >= 6 {
		ids, ok, missing = trigrams.selectiveIntersectCandidateIDs(term, maxIDs)
	}
	if !ok {
		return nil, false
	}
	if missing {
		return vol.nameTrigramRecentMatches(term), true
	}
	out := make([]int, 0, min(limit, len(ids)))
	seen := make(map[int]struct{}, min(limit, len(ids)))
	for _, id := range ids {
		if _, exists := seen[id]; exists {
			continue
		}
		if vol.nameTrigramCandidateMatches(id, term) {
			seen[id] = struct{}{}
			out = append(out, id)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, true
}

func (vol *serviceVolumeIndex) appendTopSubtreeCandidatesByRank(out []int, seen map[int]struct{}, rootID int, limit int) []int {
	if vol == nil || vol.index == nil || limit <= 0 || rootID < 0 ||
		rootID >= len(vol.subtreeStart) || rootID >= len(vol.subtreeEnd) ||
		len(vol.subtreeOrder) == 0 {
		return out
	}
	start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
	if start == ^uint32(0) || start >= end {
		return out
	}
	recordCount := vol.index.compactRecordCount()
	orderLen := recordCount
	useResidentOrder := vol.queryIndex != nil && len(vol.queryIndex.nameOrder) > 0
	if useResidentOrder {
		orderLen = len(vol.queryIndex.nameOrder)
	} else {
		orderLen = compactOrderLen(vol.index.CompactNameOrder, recordCount)
	}
	for pos := 0; pos < orderLen && len(out) < limit; pos++ {
		id := pos
		if useResidentOrder {
			id = int(vol.queryIndex.nameOrder[pos])
		} else {
			id = compactOrderAt(vol.index.CompactNameOrder, pos)
		}
		if id < 0 || id >= recordCount || id >= len(vol.subtreeStart) {
			continue
		}
		treePos := vol.subtreeStart[id]
		if treePos < start || treePos >= end {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func (vol *serviceVolumeIndex) nameTrigramNameTermPosting(term string) ([]int, bool) {
	return vol.nameTrigramNameTermPostingLimited(term, serviceNameTrigramCandidateMaxIDs)
}

func (vol *serviceVolumeIndex) nameTrigramNameTermPostingLimited(term string, maxIDs int) ([]int, bool) {
	trigrams := vol.nameTrigramIndex()
	return vol.nameNgramNameTermPosting(term, trigrams, maxIDs)
}

func (vol *serviceVolumeIndex) nameNgramNameTermPosting(term string, trigrams *compressedTrigramIndex, maxIDs int) ([]int, bool) {
	if vol == nil || !asciiOnlyString(term) {
		return nil, false
	}
	if trigrams == nil {
		trigrams = vol.nameTrigramIndex()
		if trigrams == nil && vol.index != nil {
			trigrams = vol.index.Derived.SelfNameTrigrams
		}
	}
	if trigrams == nil {
		return nil, false
	}
	extra := vol.index != nil && vol.index.Derived.SelfNameTrigrams != nil && vol.index.Derived.SelfNameTrigrams.mappedGrams != nil
	if extra {
		its, counts, exactZero, complete := completeSelfNameGramIterators(vol.index, term)
		if !complete {
			return nil, false
		}
		if exactZero {
			return vol.nameTrigramRecentMatches(term), true
		}
		if len(its) == 0 || (maxIDs > 0 && counts[0] > maxIDs) {
			return nil, false
		}
		ids := materializePostingBlockIterator(its[0], counts[0])
		for i := 1; i < len(its) && len(ids) > 0; i++ {
			ids = intersectSortedUint32sWithPostingIterator(ids, its[i])
		}
		if maxIDs > 0 && len(ids) > maxIDs {
			return nil, false
		}
		out := uniqueSortedInts(vol.verifyNameTrigramCandidateIDs(uint32sToInts(ids), term))
		return vol.withNameTrigramRecentCandidates(out, term), true
	}
	cacheKey := fmt.Sprintf("\x00ngram%dname:%s", trigrams.gramSize, term)
	vol.termMu.Lock()
	if vol.termCache != nil {
		if entry, ok := vol.termCache[cacheKey]; ok {
			if vol.cacheStampValid(entry.gen) {
				vol.termMu.Unlock()
				return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
					id, ok := vol.idForFRN(rec.FRN)
					return ok && vol.nameTrigramCandidateMatches(id, term)
				}), true
			}
		}
	}
	vol.termMu.Unlock()
	ids, ok, missing := trigrams.selectiveCandidateIDs(term, maxIDs)
	if len(term) >= 6 {
		ids, ok, missing = trigrams.selectiveIntersectCandidateIDs(term, maxIDs)
	}
	if !ok {
		return nil, false
	}
	if missing {
		return vol.nameTrigramRecentMatches(term), true
	}
	out := vol.verifyNameTrigramCandidateIDs(ids, term)
	out = uniqueSortedInts(out)
	vol.cacheNamePosting(cacheKey, out)
	return vol.withNameTrigramRecentCandidates(out, term), true
}

func (vol *serviceVolumeIndex) nameTrigramRecentMatches(term string) []int {
	if vol == nil || len(vol.nameTrigramRecent) == 0 {
		return nil
	}
	out := make([]int, 0, min(len(vol.nameTrigramRecent), 64))
	for id := range vol.nameTrigramRecent {
		if vol.nameTrigramCandidateMatches(id, term) {
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out
}

func (vol *serviceVolumeIndex) withNameTrigramRecentCandidates(base []int, term string) []int {
	if vol == nil || len(vol.nameTrigramRecent) == 0 {
		return base
	}
	out := append([]int(nil), base...)
	seen := make(map[int]struct{}, len(out))
	for _, id := range out {
		seen[id] = struct{}{}
	}
	for id := range vol.nameTrigramRecent {
		if _, ok := seen[id]; ok {
			continue
		}
		if vol.nameTrigramCandidateMatches(id, term) {
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out
}

func (vol *serviceVolumeIndex) verifyNameTrigramCandidateIDs(ids []int, term string) []int {
	if len(ids) == 0 || vol == nil || vol.index == nil {
		return nil
	}
	if len(ids) < serviceTrigramParallelVerifyMinIDs {
		out := make([]int, 0, len(ids))
		for _, id := range ids {
			if vol.nameTrigramCandidateMatches(id, term) {
				out = append(out, id)
			}
		}
		return out
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, len(ids)/serviceTrigramParallelVerifyMinIDs))
	if workers <= 1 {
		out := make([]int, 0, len(ids))
		for _, id := range ids {
			if vol.nameTrigramCandidateMatches(id, term) {
				out = append(out, id)
			}
		}
		return out
	}
	parts := make([][]int, workers)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := worker * len(ids) / workers
		end := (worker + 1) * len(ids) / workers
		wg.Add(1)
		go func(worker, start, end int) {
			defer wg.Done()
			local := make([]int, 0, end-start)
			for _, id := range ids[start:end] {
				if vol.nameTrigramCandidateMatches(id, term) {
					local = append(local, id)
				}
			}
			parts[worker] = local
		}(worker, start, end)
	}
	wg.Wait()
	total := 0
	for _, part := range parts {
		total += len(part)
	}
	out := make([]int, 0, total)
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

func (vol *serviceVolumeIndex) nameTrigramCandidateMatches(id int, term string) bool {
	if id < 0 || id >= vol.index.compactRecordCount() {
		return false
	}
	rec := vol.index.compactRecord(id)
	if rec.Deleted {
		return false
	}
	return containsFoldASCII(vol.index.compactNameAt(id), term)
}

func (vol *serviceVolumeIndex) nameTrigramPathTermPosting(term string) ([]int, bool) {
	cacheKey := "\x00trigrampath:" + term
	vol.termMu.Lock()
	if vol.pathTermCache != nil {
		if entry, ok := vol.pathTermCache[cacheKey]; ok {
			if vol.cacheStampValid(entry.gen) {
				vol.termMu.Unlock()
				return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
					id, ok := vol.idForFRN(rec.FRN)
					return ok && vol.index.compactPathContainsTerm(id, term)
				}), true
			}
		}
	}
	vol.termMu.Unlock()
	ids, ok := vol.nameTrigramNameTermPostingLimited(term, servicePathNameTrigramCandidateMaxIDs)
	if !ok {
		return nil, false
	}
	seen := make(map[int]struct{}, len(ids))
	out := make([]int, 0, len(ids))
	estimated := 0
	for _, id := range ids {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			continue
		}
		if rec.Mode&uint32(os.ModeDir) == 0 {
			estimated++
		} else {
			if !vol.hasDescendantIndex() {
				return nil, false
			}
			estimated += vol.estimatedDescendantOrSelfCount(id)
		}
		if estimated > serviceComponentTrigramExpansionMaxIDs {
			return nil, false
		}
	}
	for _, id := range ids {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		if _, exists := seen[id]; !exists {
			seen[id] = struct{}{}
			out = append(out, id)
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 {
			continue
		}
		if !vol.hasDescendantIndex() {
			return nil, false
		}
		for _, childID := range vol.underDescendants(id) {
			child := int(childID)
			if _, exists := seen[child]; exists {
				continue
			}
			seen[child] = struct{}{}
			out = append(out, child)
			if len(out) > serviceComponentTrigramExpansionMaxIDs {
				return nil, false
			}
		}
	}
	sort.Ints(out)
	vol.cachePathPosting(cacheKey, out)
	return out, true
}

func (vol *serviceVolumeIndex) hasDescendantIndex() bool {
	return vol != nil && (len(vol.subtreeOrder) > 0 || len(vol.childOffsets) > 0 || vol.children != nil)
}

func (vol *serviceVolumeIndex) estimatedDescendantOrSelfCount(rootID int) int {
	if vol == nil || vol.index == nil || rootID < 0 || rootID >= vol.index.compactRecordCount() {
		return 0
	}
	if rootID < len(vol.subtreeStart) && rootID < len(vol.subtreeEnd) && len(vol.subtreeOrder) > 0 {
		start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start != ^uint32(0) && start <= end && int(end) <= len(vol.subtreeOrder) {
			return int(end - start)
		}
	}
	vol.termMu.Lock()
	if vol.underCache != nil {
		if cached, ok := vol.underCache[rootID]; ok {
			vol.termMu.Unlock()
			return len(cached.ids)
		}
	}
	vol.termMu.Unlock()
	return len(vol.underDescendantsLimited(rootID, serviceComponentTrigramExpansionMaxIDs+1))
}

func (vol *serviceVolumeIndex) limitedSingleTermCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || pq.CountOnly || pq.Limit <= 0 || pq.CaseSensitive ||
		pq.Under != "" || pq.Type != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.Regexps) > 0 || len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 || pq.HasModAfter || pq.Exists ||
		pq.CWDBias != "" || pq.RootBias != "" || pq.SortColumn != "" {
		return nil, false
	}
	if !pq.MatchPath && len(pq.Terms) == 1 && len(pq.Dirs) == 0 {
		return vol.scanNameTermLimited(pq, pq.Terms[0], pq.Limit), true
	}
	if len(pq.Terms) == 0 && len(pq.Dirs) == 1 {
		return vol.scanPathTermLimited(pq, pq.Dirs[0], pq.Limit), true
	}
	return nil, false
}

func (vol *serviceVolumeIndex) scanNameTermLimited(pq parsedQuery, term string, limit int) []int {
	if term == "" || limit <= 0 {
		return nil
	}
	return vol.scanOrderedLimited(pq, limit, func(i int) bool {
		return containsFoldASCII(vol.index.compactNameAt(i), term)
	})
}

func (vol *serviceVolumeIndex) scanPathTermLimited(pq parsedQuery, term string, limit int) []int {
	if term == "" || limit <= 0 {
		return nil
	}
	return vol.scanOrderedLimited(pq, limit, func(i int) bool {
		return vol.index.compactPathContainsTerm(i, term)
	})
}

func (vol *serviceVolumeIndex) scanPathTermPrefixLimited(pq parsedQuery, term string, limit int, maxScan int) []int {
	if term == "" || limit <= 0 || maxScan <= 0 {
		return nil
	}
	orderLen := vol.index.compactRecordCount()
	if vol.queryIndex != nil && len(vol.queryIndex.nameOrder) > 0 {
		orderLen = len(vol.queryIndex.nameOrder)
	} else {
		orderLen = compactOrderLen(vol.index.CompactNameOrder, vol.index.compactRecordCount())
	}
	end := min(orderLen, maxScan)
	return vol.scanOrderedLimitedRange(pq, 0, end, limit, func(i int) bool {
		return vol.index.compactPathContainsTerm(i, term)
	})
}

func (vol *serviceVolumeIndex) scanOrderedLimited(pq parsedQuery, limit int, match func(int) bool) []int {
	recordCount := vol.index.compactRecordCount()
	orderLen := recordCount
	useResidentOrder := vol.queryIndex != nil && len(vol.queryIndex.nameOrder) > 0
	if useResidentOrder {
		orderLen = len(vol.queryIndex.nameOrder)
	} else {
		orderLen = compactOrderLen(vol.index.CompactNameOrder, recordCount)
	}
	prefixEnd := min(orderLen, 4_096)
	out := vol.scanOrderedLimitedRange(pq, 0, prefixEnd, limit, match)
	if len(out) >= limit || prefixEnd >= orderLen {
		return out
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, orderLen/25_000))
	if workers <= 1 || orderLen < 50_000 {
		tail := vol.scanOrderedLimitedRange(pq, prefixEnd, orderLen, limit-len(out), match)
		return append(out, tail...)
	}
	parts := make([][]int, workers)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := prefixEnd + worker*(orderLen-prefixEnd)/workers
		end := prefixEnd + (worker+1)*(orderLen-prefixEnd)/workers
		wg.Add(1)
		go func(worker, start, end int) {
			defer wg.Done()
			parts[worker] = vol.scanOrderedLimitedRange(pq, start, end, limit-len(out), match)
		}(worker, start, end)
	}
	wg.Wait()
	for _, part := range parts {
		for _, id := range part {
			out = append(out, id)
			if len(out) >= limit {
				return out
			}
		}
	}
	return out
}

func (vol *serviceVolumeIndex) scanOrderedLimitedRange(pq parsedQuery, start, end, limit int, match func(int) bool) []int {
	out := make([]int, 0, limit)
	recordCount := vol.index.compactRecordCount()
	useResidentOrder := vol.queryIndex != nil && len(vol.queryIndex.nameOrder) > 0
	for pos := start; pos < end; pos++ {
		if pos&4095 == 0 && queryCanceled(pq) {
			return out
		}
		i := pos
		if useResidentOrder {
			i = int(vol.queryIndex.nameOrder[pos])
		} else {
			i = compactOrderAt(vol.index.CompactNameOrder, pos)
		}
		if i < 0 || i >= recordCount {
			continue
		}
		rec := vol.index.compactRecord(i)
		if rec.Deleted {
			continue
		}
		if match(i) {
			out = append(out, i)
			if len(out) >= limit {
				return out
			}
		}
	}
	return out
}

func (vol *serviceVolumeIndex) cachedMultiNameTermCandidates(terms []string) ([]int, bool) {
	vol.termMu.Lock()
	if vol.termCache == nil {
		vol.termMu.Unlock()
		return nil, false
	}
	lists := make([][]int, 0, len(terms))
	seqs := make([]uint64, 0, len(terms))
	for _, term := range terms {
		entry, ok := vol.termCache[term]
		if !ok {
			vol.termMu.Unlock()
			return nil, false
		}
		if !vol.cacheStampValid(entry.gen) {
			vol.termMu.Unlock()
			return nil, false
		}
		lists = append(lists, append([]int(nil), entry.ids...))
		seqs = append(seqs, entry.gen)
	}
	vol.termMu.Unlock()
	for i, term := range terms {
		lists[i] = vol.withRecentCandidates(lists[i], seqs[i], func(rec CompactRecord) bool {
			id, ok := vol.idForFRN(rec.FRN)
			return ok && strings.Contains(vol.index.compactLowerNameAt(id), term)
		})
	}
	sortIntListsByLen(lists)
	candidates := append([]int(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSortedInts(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	return candidates, true
}

func (vol *serviceVolumeIndex) multiNameTermCandidates(terms []string) []int {
	lists := make([][]int, len(terms))
	for termIndex, term := range terms {
		lists[termIndex] = vol.nameTermPosting(term)
	}
	sortIntListsByLen(lists)
	candidates := append([]int(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSortedInts(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	return candidates
}

func (vol *serviceVolumeIndex) plannerCandidates(pq parsedQuery) ([]int, bool) {
	globExts, globsOK := simpleGlobExts(pq.Globs)
	if vol == nil || vol.index == nil || vol.queryIndex == nil || pq.CaseSensitive || pq.Under != "" || pq.Exists || pq.HasModAfter || !globsOK {
		return nil, false
	}
	strong := make([][]uint32, 0, len(pq.Terms)+len(pq.Exts)+len(globExts)+2)
	addStrong := func(list []uint32) bool {
		if len(list) == 0 {
			return false
		}
		strong = append(strong, list)
		return true
	}
	qi := vol.queryIndex
	lastBareExt := []uint32(nil)
	for _, ext := range pq.Exts {
		if !addStrong(qi.ext[ext]) {
			return []int{}, true
		}
	}
	for _, ext := range globExts {
		if !addStrong(qi.ext[ext]) {
			return []int{}, true
		}
	}
	for _, term := range pq.RegexTerms {
		if list := qi.ext[term]; len(list) > 0 {
			addStrong(list)
		}
	}
	if pq.Type == "dir" {
		addStrong(qi.dirs)
	}
	for _, term := range pq.Terms {
		if pq.MatchPath {
			if strings.HasSuffix(term, ":") {
				if !strings.EqualFold(term, vol.volume) {
					return []int{}, true
				}
				continue
			}
			if strings.HasPrefix(term, ".") && len(term) > 1 {
				if list := qi.ext[strings.TrimPrefix(term, ".")]; len(list) > 0 {
					addStrong(list)
					continue
				}
			}
			if list := qi.ext[term]; len(list) > 0 {
				lastBareExt = list
			}
			if ext := strings.TrimPrefix(filepath.Ext(term), "."); ext != "" {
				if list := qi.ext[ext]; len(list) > 0 {
					lastBareExt = list
				}
			}
			continue
		}
		if strings.HasPrefix(term, ".") && len(term) > 1 {
			if list := qi.ext[strings.TrimPrefix(term, ".")]; len(list) > 0 {
				addStrong(list)
				continue
			}
		}
		if list := qi.ext[term]; len(list) > 0 {
			lastBareExt = list
		}
		if ext := strings.TrimPrefix(filepath.Ext(term), "."); ext != "" {
			if list := qi.ext[ext]; len(list) > 0 {
				lastBareExt = list
			}
		}
	}
	if len(strong) == 0 && len(lastBareExt) > 0 {
		addStrong(lastBareExt)
	}
	if len(strong) == 0 {
		return nil, false
	}
	lists := strong
	sortUint32ListsByLen(lists)
	candidates := append([]uint32(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSortedUint32s(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	out := uint32sToInts(candidates)
	if len(vol.recentIDs) > 0 {
		out = append(out, mapKeys(vol.recentIDs)...)
		sort.Ints(out)
		out = uniqueSortedInts(out)
	}
	return out, true
}

func simpleGlobExts(globs []string) ([]string, bool) {
	if len(globs) == 0 {
		return nil, true
	}
	exts := make([]string, 0, len(globs))
	for _, glob := range globs {
		if !strings.HasPrefix(glob, "*.") || strings.Count(glob, "*") != 1 || strings.ContainsAny(strings.TrimPrefix(glob, "*."), `\/*?[]:`) {
			return nil, false
		}
		ext := strings.ToLower(strings.TrimPrefix(glob, "*."))
		if ext == "" {
			return nil, false
		}
		exts = append(exts, ext)
	}
	return exts, true
}

func (vol *serviceVolumeIndex) underCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || pq.Under == "" {
		return nil, false
	}
	under := filepath.Clean(pq.Under)
	if vol.index.Volume != "" && !strings.EqualFold(filepath.VolumeName(under), vol.index.Volume) {
		return []int{}, true
	}
	base := strings.ToLower(filepath.Base(under))
	if base == "." || base == string(filepath.Separator) || base == "" {
		return nil, false
	}
	roots := vol.underRootIDs(under)
	if len(roots) == 0 {
		return []int{}, true
	}
	if candidates, ok := vol.underLimitedTermCandidates(roots, pq); ok {
		return candidates, true
	}
	out := make([]int, 0, 256)
	prefilter := vol.underPrefilter(pq)
	for _, rootID := range roots {
		if rootID < 0 || rootID >= vol.index.compactRecordCount() || vol.index.compactRecord(rootID).Deleted {
			continue
		}
		if len(vol.childOffsets) == 0 && vol.children == nil {
			if prefilter != nil {
				prefilterIDs := make([]int, 0, len(prefilter))
				for id := range prefilter {
					prefilterIDs = append(prefilterIDs, id)
				}
				sort.Ints(prefilterIDs)
				for _, id := range prefilterIDs {
					if id < 0 || id >= vol.index.compactRecordCount() {
						continue
					}
					rec := vol.index.compactRecord(id)
					if vol.isDescendantOrSelf(id, rootID) && !rec.Deleted && compactRecordPrecheck(rec, pq, true) {
						out = append(out, id)
					}
				}
				continue
			}
			descendants := vol.underDescendants(rootID)
			for _, id := range descendants {
				if id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				rec := vol.index.compactRecord(id)
				if !rec.Deleted && compactRecordPrecheck(rec, pq, true) {
					out = append(out, id)
				}
			}
			continue
		}
		if prefilter != nil {
			for id := range prefilter {
				if id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				rec := vol.index.compactRecord(id)
				if vol.isDescendantOrSelf(id, rootID) && !rec.Deleted && compactRecordPrecheck(rec, pq, true) {
					out = append(out, id)
				}
			}
			continue
		}
		seen := make(map[int]struct{}, 256)
		stack := []int{rootID}
		for len(stack) > 0 {
			last := len(stack) - 1
			id := stack[last]
			stack = stack[:last]
			if _, ok := seen[id]; ok || id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			seen[id] = struct{}{}
			rec := vol.index.compactRecord(id)
			if !rec.Deleted && compactRecordPrecheck(rec, pq, true) {
				out = append(out, id)
			}
			for _, childID := range vol.childIDsForRecord(id) {
				stack = append(stack, int(childID))
			}
		}
	}
	sortCandidateIDs(out, pq, vol.index, vol.rankForQuery(pq))
	return out, true
}

func (vol *serviceVolumeIndex) underRootIDs(under string) []int {
	if vol == nil || vol.index == nil {
		return nil
	}
	cacheKey := strings.ToLower(filepath.Clean(under))
	vol.termMu.Lock()
	if vol.underRootCache != nil {
		if entry, ok := vol.underRootCache[cacheKey]; ok {
			if vol.cacheStampValid(entry.gen) {
				vol.termMu.Unlock()
				return append([]int(nil), entry.ids...)
			}
		}
	}
	vol.termMu.Unlock()
	var roots []int
	volume := filepath.VolumeName(under)
	rest := strings.TrimPrefix(under, volume)
	rest = strings.Trim(rest, `\/`)
	if rest == "" {
		if len(vol.rootIDs) > 0 {
			roots = make([]int, 0, len(vol.rootIDs))
			for _, id := range vol.rootIDs {
				roots = append(roots, int(id))
			}
		} else {
			roots = []int{0}
		}
		vol.cacheUnderRoots(cacheKey, roots)
		return append([]int(nil), roots...)
	}
	parts := strings.FieldsFunc(rest, func(r rune) bool { return r == '\\' || r == '/' })
	candidates := make([]int, 0, 4)
	recordCount := vol.index.compactRecordCount()
	if len(vol.rootIDs) > 0 {
		for _, id := range vol.rootIDs {
			if int(id) < recordCount {
				candidates = append(candidates, int(id))
			}
		}
	} else {
		for id := 0; id < recordCount; id++ {
			rec := vol.index.compactRecord(id)
			if rec.Parent < 0 && !rec.Deleted {
				candidates = append(candidates, id)
			}
		}
	}
	if len(vol.childOffsets) == 0 && vol.children == nil {
		if roots := vol.underRootIDsByBasename(under); len(roots) > 0 {
			vol.cacheUnderRoots(cacheKey, roots)
			return append([]int(nil), roots...)
		}
		roots = vol.underRootIDsByParentScans(candidates, parts)
		vol.cacheUnderRoots(cacheKey, roots)
		return append([]int(nil), roots...)
	}
	for _, part := range parts {
		want := strings.ToLower(part)
		next := make([]int, 0, 4)
		for _, parentID := range candidates {
			for _, childID32 := range vol.childIDsForRecord(parentID) {
				childID := int(childID32)
				if childID < 0 || childID >= recordCount {
					continue
				}
				rec := vol.index.compactRecord(childID)
				if !rec.Deleted && strings.EqualFold(vol.index.compactLowerNameAt(childID), want) {
					next = append(next, childID)
				}
			}
		}
		if len(next) == 0 {
			if roots := vol.underRootIDsByBasename(under); len(roots) > 0 {
				vol.cacheUnderRoots(cacheKey, roots)
				return append([]int(nil), roots...)
			}
			vol.cacheUnderRoots(cacheKey, nil)
			return nil
		}
		candidates = next
	}
	vol.cacheUnderRoots(cacheKey, candidates)
	return append([]int(nil), candidates...)
}

func (vol *serviceVolumeIndex) cacheUnderRoots(key string, roots []int) {
	if key == "" {
		return
	}
	vol.termMu.Lock()
	defer vol.termMu.Unlock()
	if vol.underRootCache == nil {
		vol.underRootCache = make(map[string]postingCacheEntry)
	}
	vol.underRootCache[key] = postingCacheEntry{ids: append([]int(nil), roots...), gen: vol.cacheGeneration()}
}

func (vol *serviceVolumeIndex) underLimitedTermCandidates(roots []int, pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || pq.CountOnly || pq.Limit <= 0 || len(roots) == 0 || len(pq.Terms) != 1 || len(pq.Exts) > 0 || len(pq.Dirs) > 0 || len(pq.Globs) > 0 || len(pq.Regexps) > 0 || pq.Type != "" || pq.HasModAfter || pq.Exists || pq.CaseSensitive {
		return nil, false
	}
	term := pq.Terms[0]
	if term == "" || strings.ContainsAny(term, `\/*?[]:`) {
		return nil, false
	}
	if out, ok := vol.scanUnderRootsTermLimited(roots, term, pq.Limit); ok {
		return out, true
	}
	out := make([]int, 0, pq.Limit)
	seen := make(map[int]struct{}, pq.Limit)
	for _, rootID := range roots {
		for _, id := range vol.subtreeIDsInOrder(rootID) {
			if len(out) >= pq.Limit {
				sort.Ints(out)
				return out, true
			}
			if _, ok := seen[id]; ok || id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			rec := vol.index.compactRecord(id)
			if rec.Deleted {
				continue
			}
			if strings.Contains(vol.index.compactLowerNameAt(id), term) {
				seen[id] = struct{}{}
				out = append(out, id)
			}
		}
	}
	sort.Ints(out)
	return out, true
}

func (vol *serviceVolumeIndex) scanUnderRootsTermLimited(roots []int, term string, limit int) ([]int, bool) {
	if len(roots) == 0 {
		return nil, false
	}
	intervals := make([]interval, 0, len(roots))
	for _, rootID := range roots {
		if rootID < 0 || rootID >= vol.index.compactRecordCount() || rootID >= len(vol.subtreeStart) || rootID >= len(vol.subtreeEnd) {
			continue
		}
		start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start == ^uint32(0) || start > end || int(end) > len(vol.subtreeOrder) {
			continue
		}
		intervals = append(intervals, interval{start: int(start), end: int(end)})
	}
	if len(intervals) == 0 {
		return nil, false
	}
	return vol.scanIntervalsTermLimited(intervals, term, limit), true
}

func (vol *serviceVolumeIndex) scanUnderTermLimited(rootID int, term string, limit int) ([]int, bool) {
	if vol == nil || vol.index == nil || limit <= 0 || rootID < 0 || rootID >= vol.index.compactRecordCount() {
		return nil, false
	}
	if rootID >= len(vol.subtreeStart) || rootID >= len(vol.subtreeEnd) || len(vol.subtreeOrder) == 0 {
		return nil, false
	}
	start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
	if start == ^uint32(0) || start > end || int(end) > len(vol.subtreeOrder) {
		return nil, false
	}
	return vol.scanIntervalsTermLimited([]interval{{start: int(start), end: int(end)}}, term, limit), true
}

type interval struct {
	start int
	end   int
}

func (vol *serviceVolumeIndex) scanIntervalsTermLimited(intervals []interval, term string, limit int) []int {
	total := 0
	for _, iv := range intervals {
		if iv.end > iv.start {
			total += iv.end - iv.start
		}
	}
	n := total
	if n == 0 {
		return nil
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, n/100_000))
	out := make([]int, 0, limit)
	var mu sync.Mutex
	var found atomic.Int32
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		a := worker * n / workers
		b := (worker + 1) * n / workers
		wg.Add(1)
		go func(a, b int) {
			defer wg.Done()
			local := make([]int, 0, 8)
			for logical := a; logical < b && int(found.Load()) < limit; logical++ {
				pos := intervalPosition(intervals, logical)
				if pos < 0 {
					continue
				}
				id := int(vol.subtreeOrder[pos])
				if id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				rec := vol.index.compactRecord(id)
				if rec.Deleted {
					continue
				}
				if strings.Contains(vol.index.compactLowerNameAt(id), term) {
					if found.Add(1) <= int32(limit) {
						local = append(local, id)
					}
				}
			}
			if len(local) > 0 {
				mu.Lock()
				out = append(out, local...)
				mu.Unlock()
			}
		}(a, b)
	}
	wg.Wait()
	sort.Ints(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func intervalPosition(intervals []interval, logical int) int {
	for _, iv := range intervals {
		n := iv.end - iv.start
		if logical < n {
			return iv.start + logical
		}
		logical -= n
	}
	return -1
}

func (vol *serviceVolumeIndex) underRootIDsByBasename(under string) []int {
	base := strings.ToLower(filepath.Base(under))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return nil
	}
	cleanUnder := filepath.Clean(under)
	candidates := vol.exactNameIDs(base)
	out := vol.filterUnderRootCandidates(candidates, base, cleanUnder)
	if len(out) == 0 {
		out = vol.filterUnderRootCandidates(vol.nameTermPosting(base), base, cleanUnder)
	}
	sort.Ints(out)
	return uniqueSortedInts(out)
}

func (vol *serviceVolumeIndex) filterUnderRootCandidates(candidates []int, base, cleanUnder string) []int {
	out := make([]int, 0, 1)
	pathCache := make(map[int]string)
	for _, id := range candidates {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 || vol.index.compactLowerNameAt(id) != base {
			continue
		}
		path := vol.index.reconstructCompactPathCached(id, pathCache)
		if strings.EqualFold(filepath.Clean(path), cleanUnder) {
			out = append(out, id)
		}
	}
	return out
}

func (vol *serviceVolumeIndex) underRootIDsByParentScans(candidates []int, parts []string) []int {
	if len(candidates) == 0 {
		return nil
	}
	recordCount := vol.index.compactRecordCount()
	for _, part := range parts {
		want := strings.ToLower(part)
		parentFRNs := make(map[uint64]struct{}, len(candidates))
		for _, id := range candidates {
			if id < 0 || id >= recordCount {
				continue
			}
			frn := vol.index.compactRecord(id).FRN
			if frn != 0 {
				parentFRNs[frn] = struct{}{}
			}
		}
		if len(parentFRNs) == 0 {
			return nil
		}
		next := make([]int, 0, 4)
		for id := 0; id < recordCount; id++ {
			rec := vol.index.compactRecord(id)
			if rec.Deleted {
				continue
			}
			if _, ok := parentFRNs[rec.ParentFRN]; !ok {
				continue
			}
			if vol.index.compactLowerNameAt(id) == want {
				next = append(next, id)
			}
		}
		if len(next) == 0 {
			return nil
		}
		candidates = next
	}
	return candidates
}

func (vol *serviceVolumeIndex) isDescendantOrSelf(id, rootID int) bool {
	if vol != nil && id >= 0 && rootID >= 0 && id < len(vol.subtreeStart) && rootID < len(vol.subtreeStart) {
		pos, start, end := vol.subtreeStart[id], vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start != ^uint32(0) && pos != ^uint32(0) {
			return pos >= start && pos < end
		}
	}
	seen := make(map[int]struct{}, 16)
	cur := id
	for depth := 0; depth < 1024; depth++ {
		if cur == rootID {
			return true
		}
		if cur < 0 || cur >= vol.index.compactRecordCount() {
			return false
		}
		if _, ok := seen[cur]; ok {
			return false
		}
		seen[cur] = struct{}{}
		parent := vol.index.compactRecord(cur).Parent
		if parent < 0 {
			return false
		}
		cur = int(parent)
	}
	return false
}

func (vol *serviceVolumeIndex) isDescendantOrSelfAnyFast(id int, roots []int) bool {
	if vol == nil || vol.index == nil || id < 0 || len(roots) == 0 {
		return false
	}
	recordCount := vol.index.compactRecordCount()
	cur := id
	for depth := 0; depth < 1024; depth++ {
		if cur < 0 || cur >= recordCount {
			return false
		}
		for _, rootID := range roots {
			if cur == rootID {
				return true
			}
		}
		parent := int(vol.index.compactRecord(cur).Parent)
		if parent < 0 || parent == cur {
			return false
		}
		cur = parent
	}
	return false
}

func (vol *serviceVolumeIndex) underDescendants(rootID int) []int {
	if vol == nil || vol.index == nil || rootID < 0 || rootID >= vol.index.compactRecordCount() {
		return nil
	}
	vol.termMu.Lock()
	if vol.underCache != nil {
		if entry, ok := vol.underCache[rootID]; ok {
			if vol.cacheStampValid(entry.gen) {
				vol.termMu.Unlock()
				return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
					id, ok := vol.idForFRN(rec.FRN)
					return ok && vol.isDescendantOrSelf(id, rootID)
				})
			}
		}
	}
	vol.termMu.Unlock()
	recordCount := vol.index.compactRecordCount()
	out := make([]int, 0, 256)
	if rootID < len(vol.subtreeStart) && rootID < len(vol.subtreeEnd) && len(vol.subtreeOrder) > 0 {
		start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start != ^uint32(0) && start <= end && int(end) <= len(vol.subtreeOrder) {
			out = make([]int, 0, int(end-start))
			for _, id32 := range vol.subtreeOrder[start:end] {
				id := int(id32)
				if id < 0 || id >= recordCount {
					continue
				}
				if !vol.index.compactRecord(id).Deleted {
					out = append(out, id)
				}
			}
		}
	} else if len(vol.childOffsets) > 0 || vol.children != nil {
		stack := []int{rootID}
		seen := make(map[int]struct{}, 256)
		for len(stack) > 0 {
			last := len(stack) - 1
			id := stack[last]
			stack = stack[:last]
			if _, ok := seen[id]; ok || id < 0 || id >= recordCount {
				continue
			}
			seen[id] = struct{}{}
			rec := vol.index.compactRecord(id)
			if !rec.Deleted {
				out = append(out, id)
			}
			for _, childID := range vol.childIDsForRecord(id) {
				stack = append(stack, int(childID))
			}
		}
	} else {
		for id := 0; id < recordCount; id++ {
			rec := vol.index.compactRecord(id)
			if rec.Deleted {
				continue
			}
			if vol.isDescendantOrSelf(id, rootID) {
				out = append(out, id)
			}
		}
	}
	if len(out) == 0 {
		return out
	}
	sort.Ints(out)
	if vol.shouldCachePosting(out) {
		vol.termMu.Lock()
		if vol.underCache == nil {
			vol.underCache = make(map[int]postingCacheEntry)
		}
		vol.underCache[rootID] = postingCacheEntry{ids: out, gen: vol.cacheGeneration()}
		vol.termMu.Unlock()
	}
	return out
}

func (vol *serviceVolumeIndex) underDescendantsLimited(rootID, limit int) []int {
	if vol == nil || vol.index == nil || rootID < 0 || rootID >= vol.index.compactRecordCount() {
		return nil
	}
	if limit <= 0 {
		return vol.underDescendants(rootID)
	}
	recordCount := vol.index.compactRecordCount()
	if rootID < len(vol.subtreeStart) && rootID < len(vol.subtreeEnd) && len(vol.subtreeOrder) > 0 {
		start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start != ^uint32(0) && start <= end && int(end) <= len(vol.subtreeOrder) {
			out := make([]int, 0, min(limit, int(end-start)))
			for _, id32 := range vol.subtreeOrder[start:end] {
				if len(out) >= limit {
					break
				}
				id := int(id32)
				if id < 0 || id >= recordCount {
					continue
				}
				if !vol.index.compactRecord(id).Deleted {
					out = append(out, id)
				}
			}
			sort.Ints(out)
			return out
		}
	}
	out := make([]int, 0, min(limit, 256))
	stack := []int{rootID}
	seen := make(map[int]struct{}, 256)
	for len(stack) > 0 && len(out) < limit {
		last := len(stack) - 1
		id := stack[last]
		stack = stack[:last]
		if _, ok := seen[id]; ok || id < 0 || id >= recordCount {
			continue
		}
		seen[id] = struct{}{}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted {
			out = append(out, id)
		}
		for _, childID := range vol.childIDsForRecord(id) {
			stack = append(stack, int(childID))
		}
	}
	sort.Ints(out)
	return out
}

func (vol *serviceVolumeIndex) underPrefilter(pq parsedQuery) map[int]struct{} {
	if vol == nil || len(pq.Regexps) > 0 || pq.CaseSensitive || pq.HasModAfter || pq.Exists {
		return nil
	}
	lists := make([][]int, 0, len(pq.Exts)+len(pq.Dirs)+len(pq.Terms)+len(pq.Globs))
	for _, ext := range pq.Exts {
		list := vol.extPosting(ext)
		if len(list) == 0 {
			return map[int]struct{}{}
		}
		lists = append(lists, list)
	}
	globExts, globsOK := simpleGlobExts(pq.Globs)
	if globsOK {
		for _, ext := range globExts {
			list := vol.extPosting(ext)
			if len(list) == 0 {
				return map[int]struct{}{}
			}
			lists = append(lists, list)
		}
	} else {
		for _, ext := range complexGlobExts(pq.Globs) {
			list := vol.extPosting(ext)
			if len(list) == 0 {
				return map[int]struct{}{}
			}
			lists = append(lists, list)
		}
		for _, globTerm := range globLiteralTerms(pq.Globs, pq.CaseSensitive) {
			list := vol.nameTermPosting(globTerm)
			if len(list) == 0 {
				continue
			}
			lists = append(lists, list)
		}
	}
	for _, dir := range pq.Dirs {
		list := vol.pathComponentPosting(dir)
		if len(list) == 0 {
			return map[int]struct{}{}
		}
		lists = append(lists, list)
	}
	hasDottedTerm := false
	for _, term := range pq.Terms {
		if strings.Contains(term, ".") {
			hasDottedTerm = true
			break
		}
	}
	for _, term := range pq.Terms {
		if pq.MatchPath && hasDottedTerm && !strings.Contains(term, ".") {
			continue
		}
		list := []int(nil)
		if ext, ok := dottedExtensionTerm(term); ok {
			list = vol.extPosting(ext)
		} else if strings.Contains(term, ".") {
			list = vol.exactNameIDs(term)
		}
		if len(list) == 0 {
			list = vol.nameTermPosting(term)
		}
		if pq.MatchPath && len(list) == 0 {
			list = vol.pathTermPosting(term)
		}
		if len(list) == 0 {
			return map[int]struct{}{}
		}
		lists = append(lists, list)
	}
	if len(lists) == 0 {
		return nil
	}
	sortIntListsByLen(lists)
	candidates := append([]int(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSortedInts(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	out := make(map[int]struct{}, len(candidates))
	for _, id := range candidates {
		out[id] = struct{}{}
	}
	return out
}

func sortCandidateIDs(ids []int, pq parsedQuery, idx *Index, cachedRanks []uint32) {
	if idx == nil {
		sort.Ints(ids)
		return
	}
	rankOf := candidateRanker(idx, cachedRanks)
	sort.SliceStable(ids, func(i, j int) bool {
		return rankOf(ids[i]) < rankOf(ids[j])
	})
}

func topCandidateIDsByRank(ids []int, limit int, idx *Index, cachedRanks []uint32) []int {
	if limit <= 0 || len(ids) <= limit {
		sortCandidateIDs(ids, parsedQuery{}, idx, cachedRanks)
		return ids
	}
	if idx == nil {
		sort.Ints(ids)
		return ids[:limit]
	}
	rankOf := candidateRanker(idx, cachedRanks)
	if len(ids) >= serviceRankParallelMinIDs && limit <= 256 && runtime.GOMAXPROCS(0) > 1 {
		return topCandidateIDsByRankParallel(ids, limit, rankOf)
	}
	h := make(candidateRankMaxHeap, 0, limit)
	for _, id := range ids {
		item := candidateRankItem{id: id, rank: rankOf(id)}
		if len(h) < limit {
			heap.Push(&h, item)
			continue
		}
		if item.rank < h[0].rank {
			h[0] = item
			heap.Fix(&h, 0)
		}
	}
	out := make([]int, len(h))
	for i := range h {
		out[i] = h[i].id
	}
	sortIDsByRank(out, rankOf)
	return out
}

func topCandidateIDsByRankParallel(ids []int, limit int, rankOf func(int) int) []int {
	workers := min(runtime.GOMAXPROCS(0), max(2, len(ids)/serviceRankParallelMinIDs))
	if workers <= 1 {
		return topCandidateIDsByRankSerial(ids, limit, rankOf)
	}
	chunk := (len(ids) + workers - 1) / workers
	partials := make([][]candidateRankItem, workers)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := worker * chunk
		end := min(len(ids), start+chunk)
		if start >= end {
			partials = partials[:worker]
			break
		}
		wg.Add(1)
		go func(worker, start, end int) {
			defer wg.Done()
			partials[worker] = topCandidateRankItems(ids[start:end], limit, rankOf)
		}(worker, start, end)
	}
	wg.Wait()
	h := make(candidateRankMaxHeap, 0, limit)
	for _, partial := range partials {
		for _, item := range partial {
			if len(h) < limit {
				heap.Push(&h, item)
				continue
			}
			if item.rank < h[0].rank {
				h[0] = item
				heap.Fix(&h, 0)
			}
		}
	}
	out := make([]int, len(h))
	for i := range h {
		out[i] = h[i].id
	}
	sortIDsByRank(out, rankOf)
	return out
}

func topCandidateIDsByRankSerial(ids []int, limit int, rankOf func(int) int) []int {
	items := topCandidateRankItems(ids, limit, rankOf)
	out := make([]int, len(items))
	for i := range items {
		out[i] = items[i].id
	}
	sortIDsByRank(out, rankOf)
	return out
}

func topCandidateRankItems(ids []int, limit int, rankOf func(int) int) []candidateRankItem {
	h := make(candidateRankMaxHeap, 0, limit)
	for _, id := range ids {
		item := candidateRankItem{id: id, rank: rankOf(id)}
		if len(h) < limit {
			heap.Push(&h, item)
			continue
		}
		if item.rank < h[0].rank {
			h[0] = item
			heap.Fix(&h, 0)
		}
	}
	out := make([]candidateRankItem, len(h))
	copy(out, h)
	return out
}

type candidateRankItem struct {
	id   int
	rank int
}

type candidateRankMaxHeap []candidateRankItem

func (h candidateRankMaxHeap) Len() int { return len(h) }

func (h candidateRankMaxHeap) Less(i, j int) bool { return h[i].rank > h[j].rank }

func (h candidateRankMaxHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *candidateRankMaxHeap) Push(x any) {
	*h = append(*h, x.(candidateRankItem))
}

func (h *candidateRankMaxHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

func candidateRanker(idx *Index, cachedRanks []uint32) func(int) int {
	recordCount := idx.compactRecordCount()
	var ranks []int
	if len(cachedRanks) < recordCount && len(idx.CompactNameOrder) > 0 {
		ranks = make([]int, recordCount)
		for i := range ranks {
			ranks[i] = i
		}
		order := idx.CompactNameOrder
		for pos := 0; pos < compactOrderLen(order, recordCount); pos++ {
			id := compactOrderAt(order, pos)
			if id >= 0 && id < recordCount {
				ranks[id] = pos
			}
		}
	}
	return func(id int) int {
		rank := recordCount + id
		if id < 0 || id >= recordCount {
			return rank
		}
		if len(cachedRanks) >= recordCount {
			return int(cachedRanks[id])
		}
		if len(ranks) == 0 {
			return id
		}
		return ranks[id]
	}
}

func (vol *serviceVolumeIndex) nameOrderRanks() []uint32 {
	if vol == nil || vol.queryIndex == nil || len(vol.queryIndex.nameRank) == 0 {
		return nil
	}
	return vol.queryIndex.nameRank
}

func (vol *serviceVolumeIndex) rankForQuery(pq parsedQuery) []uint32 {
	if pq.SortColumn == "size" {
		if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.sizeRank) > 0 {
			return vol.queryIndex.sizeRank
		}
		if vol != nil && vol.index != nil && len(vol.index.Derived.SizeRank) > 0 {
			return vol.index.Derived.SizeRank
		}
		if vol != nil && vol.index != nil && vol.index.compactHasSize() {
			_, ranks := buildCompactSizeOrderRank(vol.index)
			return ranks
		}
	}
	if pq.SortColumn == "modified" {
		if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.modRank) > 0 {
			return vol.queryIndex.modRank
		}
		if vol != nil && vol.index != nil && len(vol.index.Derived.ModRank) > 0 {
			return vol.index.Derived.ModRank
		}
		if vol != nil && vol.index != nil && vol.index.compactHasModTime() {
			_, ranks := buildCompactModifiedOrderRank(vol.index)
			return ranks
		}
	}
	if pq.SortColumn == "extension" {
		if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.extRank) > 0 {
			return vol.queryIndex.extRank
		}
		if vol != nil && vol.index != nil && len(vol.index.Derived.ExtRank) > 0 {
			return vol.index.Derived.ExtRank
		}
		if vol != nil && vol.index != nil {
			_, ranks := buildCompactExtensionOrderRank(vol.index)
			return ranks
		}
	}
	if pq.SortColumn == "type" {
		if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.typeRank) > 0 {
			return vol.queryIndex.typeRank
		}
		if vol != nil && vol.index != nil && len(vol.index.Derived.TypeRank) > 0 {
			return vol.index.Derived.TypeRank
		}
		if vol != nil && vol.index != nil {
			_, ranks := buildCompactTypeOrderRank(vol.index)
			return ranks
		}
	}
	if pq.SortColumn == "path" {
		if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.pathRank) > 0 {
			return vol.queryIndex.pathRank
		}
		if vol != nil && vol.index != nil && len(vol.index.Derived.PathRank) > 0 {
			return vol.index.Derived.PathRank
		}
		if vol != nil && vol.index != nil {
			_, ranks := buildCompactPathOrderRank(vol.index)
			return ranks
		}
	}
	if ranks := vol.nameOrderRanks(); len(ranks) > 0 {
		return ranks
	}
	if vol != nil && vol.index != nil && len(vol.index.Derived.NameRank) > 0 {
		return vol.index.Derived.NameRank
	}
	return nil
}

func (vol *serviceVolumeIndex) orderForQuery(pq parsedQuery) []uint32 {
	if pq.SortColumn == "size" {
		return vol.sizeOrderForRank()
	}
	if pq.SortColumn == "modified" {
		return vol.modifiedOrderForRank()
	}
	if pq.SortColumn == "extension" {
		return vol.extensionOrderForRank()
	}
	if pq.SortColumn == "type" {
		return vol.typeOrderForRank()
	}
	if pq.SortColumn == "path" {
		return vol.pathOrderForRank()
	}
	return vol.mappedOrCompactNameOrder()
}

func (vol *serviceVolumeIndex) pathDirFilterCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || len(pq.Terms) == 0 || len(pq.Regexps) > 0 || pq.Under != "" || pq.CaseSensitive {
		return nil, false
	}
	if len(pq.Exts) == 0 && len(pq.Dirs) == 0 && len(pq.Globs) == 0 && pq.Type == "" && !pq.HasModAfter {
		return nil, false
	}
	type rootTerm struct {
		id   int
		term string
	}
	roots := make([]rootTerm, 0, 4)
	for _, term := range pq.Terms {
		if strings.ContainsAny(term, `\/*?[]:`) {
			continue
		}
		for _, id := range vol.exactNameIDs(term) {
			if id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			rec := vol.index.compactRecord(id)
			if !rec.Deleted && rec.Mode&uint32(os.ModeDir) != 0 {
				roots = append(roots, rootTerm{id: id, term: term})
			}
		}
	}
	if len(roots) == 0 {
		return nil, false
	}
	prefilter := vol.underPrefilter(pq)
	if prefilter == nil {
		return nil, false
	}
	out := make([]int, 0, 64)
	seen := make(map[int]struct{}, 64)
	for _, root := range roots {
		for id := range prefilter {
			if _, ok := seen[id]; ok || id < 0 || id >= vol.index.compactRecordCount() || !vol.isDescendantOrSelf(id, root.id) {
				continue
			}
			rec := vol.index.compactRecord(id)
			if rec.Deleted || !compactRecordPrecheck(rec, pq, true) || !vol.recordPathContainsRemainingTerms(id, pq.Terms, root.term) {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out, true
}

func (vol *serviceVolumeIndex) recordPathContainsRemainingTerms(id int, terms []string, rootTerm string) bool {
	for _, term := range terms {
		if term == rootTerm {
			continue
		}
		if !vol.index.compactPathContainsTerm(id, term) {
			return false
		}
	}
	return true
}

func (vol *serviceVolumeIndex) filterCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || len(pq.Regexps) > 0 || pq.Under != "" || pq.CaseSensitive {
		return nil, false
	}
	if len(pq.Exts) == 0 && len(pq.Dirs) == 0 {
		return nil, false
	}
	lists := make([][]int, 0, len(pq.Exts)+len(pq.Dirs)+len(pq.Terms))
	for _, ext := range pq.Exts {
		list := vol.extPosting(ext)
		if len(list) == 0 {
			return []int{}, true
		}
		lists = append(lists, list)
	}
	for _, dir := range pq.Dirs {
		list := vol.pathComponentPosting(dir)
		if len(list) == 0 {
			return []int{}, true
		}
		lists = append(lists, list)
	}
	for _, term := range pq.Terms {
		list := vol.nameTermPosting(term)
		if pq.MatchPath {
			list = vol.pathTermPosting(term)
		}
		if len(list) == 0 {
			return []int{}, true
		}
		lists = append(lists, list)
	}
	if len(lists) == 0 {
		return nil, false
	}
	sortIntListsByLen(lists)
	candidates := append([]int(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSortedInts(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	return candidates, true
}

func (vol *serviceVolumeIndex) regexLiteralCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || len(pq.Regexps) == 0 || len(pq.RegexTerms) != 1 || pq.CaseSensitive {
		return nil, false
	}
	lists := make([][]int, 0, len(pq.RegexTerms))
	for _, term := range pq.RegexTerms {
		list := vol.pathTermPosting(term)
		if len(list) == 0 {
			return []int{}, true
		}
		lists = append(lists, list)
	}
	sortIntListsByLen(lists)
	candidates := append([]int(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSortedInts(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	return candidates, true
}

func (vol *serviceVolumeIndex) pathRootLimitedCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CountOnly || len(pq.Terms) < 2 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || pq.Under != "" || pq.CaseSensitive || pq.Limit <= 0 {
		return nil, false
	}
	type rootTerm struct {
		id   int
		term string
	}
	roots := make([]rootTerm, 0, 4)
	for _, term := range pq.Terms {
		if strings.ContainsAny(term, `\/*?[]:`) {
			continue
		}
		for _, id := range vol.pathComponentRootIDs(term) {
			if id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			rec := vol.index.compactRecord(id)
			if !rec.Deleted && rec.Mode&uint32(os.ModeDir) != 0 {
				roots = append(roots, rootTerm{id: id, term: term})
			}
		}
	}
	if len(roots) == 0 {
		return nil, false
	}
	out := make([]int, 0, pq.Limit)
	seen := make(map[int]struct{}, pq.Limit)
	for _, root := range roots {
		for _, id := range vol.subtreeIDsInOrder(root.id) {
			if len(out) >= pq.Limit {
				sort.Ints(out)
				return out, true
			}
			if _, ok := seen[id]; ok || id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			rec := vol.index.compactRecord(id)
			if rec.Deleted || !compactRecordPrecheck(rec, pq, true) || !vol.recordPathContainsRemainingTerms(id, pq.Terms, root.term) {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out, true
}

func (vol *serviceVolumeIndex) subtreeIDsInOrder(rootID int) []int {
	if vol == nil || vol.index == nil || rootID < 0 || rootID >= vol.index.compactRecordCount() {
		return nil
	}
	if rootID < len(vol.subtreeStart) && rootID < len(vol.subtreeEnd) && len(vol.subtreeOrder) > 0 {
		start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start != ^uint32(0) && start <= end && int(end) <= len(vol.subtreeOrder) {
			out := make([]int, 0, int(end-start))
			for _, id32 := range vol.subtreeOrder[start:end] {
				out = append(out, int(id32))
			}
			return out
		}
	}
	return vol.underDescendants(rootID)
}

func (vol *serviceVolumeIndex) exactDirCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || len(pq.Terms) != 1 || pq.Type != "dir" || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || pq.Under != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 || pq.HasModAfter || pq.Exists || pq.CaseSensitive {
		return nil, false
	}
	list := vol.exactNameIDs(pq.Terms[0])
	out := make([]int, 0, len(list))
	for _, id := range list {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted && rec.Mode&uint32(os.ModeDir) != 0 {
			out = append(out, id)
		}
	}
	return out, true
}

func (vol *serviceVolumeIndex) pathTermSubtreeCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || len(pq.Terms) < 2 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || pq.Under != "" || pq.CaseSensitive {
		return nil, false
	}
	lists := make([][]int, 0, len(pq.Terms))
	for _, term := range pq.Terms {
		list := vol.pathPlanTermPosting(term)
		if len(list) == 0 {
			return []int{}, true
		}
		lists = append(lists, list)
	}
	sortIntListsByLen(lists)
	candidates := append([]int(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSortedInts(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	if len(candidates) > 4096 {
		if nameList, ok := vol.unionNamePostings(pq.Terms); ok {
			candidates = intersectSortedInts(candidates, nameList)
		}
	}
	return candidates, true
}

func (vol *serviceVolumeIndex) unionNamePostings(terms []string) ([]int, bool) {
	seen := make(map[int]struct{}, 64)
	out := make([]int, 0, 64)
	for _, term := range terms {
		for _, id := range vol.nameTermPosting(term) {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out, len(out) > 0
}

func (vol *serviceVolumeIndex) exactNameCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || len(pq.Terms) != 1 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || pq.Under != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 || pq.Type != "" || pq.HasModAfter || pq.Exists || pq.CaseSensitive {
		return nil, false
	}
	term := pq.Terms[0]
	if !strings.Contains(term, ".") {
		return nil, false
	}
	list := vol.exactNameIDs(term)
	out := make([]int, 0, len(list))
	for _, id := range list {
		if id >= 0 && id < vol.index.compactRecordCount() && !vol.index.compactRecord(id).Deleted {
			out = append(out, id)
		}
	}
	return out, len(out) > 0
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA == nil {
		a = aa
	}
	if errB == nil {
		b = bb
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func (vol *serviceVolumeIndex) namePrefixCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || len(pq.Terms) != 1 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || pq.Under != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 || pq.Type != "" || pq.HasModAfter || pq.Exists || pq.CaseSensitive {
		return nil, false
	}
	term := pq.Terms[0]
	if len(term) < 8 || strings.ContainsAny(term, `\/*?[]:`) {
		return nil, false
	}
	var order []uint32
	if vol.queryIndex != nil && len(vol.queryIndex.nameOrder) > 0 {
		order = vol.queryIndex.nameOrder
	} else if len(vol.index.CompactNameOrder) > 0 {
		order = make([]uint32, len(vol.index.CompactNameOrder))
		for i, id := range vol.index.CompactNameOrder {
			order[i] = uint32(id)
		}
	}
	if len(order) == 0 {
		return nil, false
	}
	start := sort.Search(len(order), func(i int) bool {
		return vol.index.compactLowerNameAt(int(order[i])) >= term
	})
	out := make([]int, 0, 8)
	seen := make(map[int]struct{})
	for i := start; i < len(order); i++ {
		id := int(order[i])
		rec := vol.index.compactRecord(id)
		if !strings.HasPrefix(vol.index.compactLowerNameAt(id), term) {
			break
		}
		if !rec.Deleted {
			out = append(out, id)
			seen[id] = struct{}{}
		}
	}
	for id := range vol.recentIDs {
		if _, ok := seen[id]; ok || id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted && strings.HasPrefix(vol.index.compactLowerNameAt(id), term) {
			out = append(out, id)
		}
	}
	return out, len(out) > 0
}

func (vol *serviceVolumeIndex) exactNameIDs(name string) []int {
	if vol == nil || vol.index == nil || name == "" {
		return nil
	}
	if vol.exactNames != nil {
		return append([]int(nil), vol.exactNames[name]...)
	}
	cacheKey := "\x00exact:" + name
	vol.termMu.Lock()
	if vol.termCache != nil {
		if entry, ok := vol.termCache[cacheKey]; ok {
			if vol.cacheStampValid(entry.gen) {
				vol.termMu.Unlock()
				return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
					id, ok := vol.idForFRN(rec.FRN)
					return ok && vol.index.compactLowerNameAt(id) == name
				})
			}
		}
	}
	vol.termMu.Unlock()
	if vol.queryIndex == nil || len(vol.queryIndex.nameOrder) == 0 {
		out := vol.scanExactNameIDs(name)
		vol.cacheNamePosting(cacheKey, out)
		return out
	}
	order := vol.queryIndex.nameOrder
	start := sort.Search(len(order), func(i int) bool {
		return vol.index.compactLowerNameAt(int(order[i])) >= name
	})
	if start >= len(order) || vol.index.compactLowerNameAt(int(order[start])) != name {
		return nil
	}
	out := make([]int, 0, 4)
	for i := start; i < len(order); i++ {
		id := int(order[i])
		if vol.index.compactLowerNameAt(id) != name {
			break
		}
		out = append(out, id)
	}
	for id := range vol.recentIDs {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted && vol.index.compactLowerNameAt(id) == name {
			out = append(out, id)
		}
	}
	sort.Ints(out)
	out = uniqueSortedInts(out)
	return out
}

func (vol *serviceVolumeIndex) scanExactNameIDs(name string) []int {
	if ext := strings.TrimPrefix(filepath.Ext(name), "."); ext != "" && vol.queryIndex != nil {
		if extIDs := vol.extPosting32(ext); len(extIDs) > 0 {
			out := make([]int, 0, 4)
			for _, id32 := range extIDs {
				id := int(id32)
				if id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				rec := vol.index.compactRecord(id)
				if !rec.Deleted && vol.index.compactLowerNameAt(id) == name {
					out = append(out, id)
				}
			}
			for id := range vol.recentIDs {
				if id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				rec := vol.index.compactRecord(id)
				if !rec.Deleted && vol.index.compactLowerNameAt(id) == name {
					out = append(out, id)
				}
			}
			sort.Ints(out)
			return uniqueSortedInts(out)
		}
	}
	out := make([]int, 0, 4)
	recordCount := vol.index.compactRecordCount()
	for id := 0; id < recordCount; id++ {
		rec := vol.index.compactRecord(id)
		if !rec.Deleted && vol.index.compactLowerNameAt(id) == name {
			out = append(out, id)
		}
	}
	return out
}

func (vol *serviceVolumeIndex) nameTermPosting(term string) []int {
	vol.termMu.Lock()
	if vol.termCache == nil {
		vol.termCache = make(map[string]postingCacheEntry)
	}
	if entry, ok := vol.termCache[term]; ok {
		if vol.cacheStampValid(entry.gen) {
			vol.termMu.Unlock()
			return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
				id, ok := vol.idForFRN(rec.FRN)
				return ok && strings.Contains(vol.index.compactLowerNameAt(id), term)
			})
		}
	}
	vol.termMu.Unlock()
	list := vol.scanNameTermPosting(term)
	vol.cacheNamePosting(term, list)
	return list
}

func postingListCacheMaxBytes() int64 {
	maxBytes := postingBlockCacheMaxBytes()
	if maxBytes <= 0 {
		return 0
	}
	return maxBytes / 4
}

func postingListBytes(list []int) int64 {
	return int64(len(list)) * int64(unsafe.Sizeof(int(0)))
}

func (vol *serviceVolumeIndex) shouldCachePosting(list []int) bool {
	maxBytes := postingListCacheMaxBytes()
	return maxBytes > 0 && postingListBytes(list) <= maxBytes
}

func postingEntryCacheBytes(cache map[string]postingCacheEntry) int64 {
	var total int64
	for _, entry := range cache {
		total += postingListBytes(entry.ids)
	}
	return total
}

func postingRootCacheBytes(cache map[int]postingCacheEntry) int64 {
	var total int64
	for _, entry := range cache {
		total += postingListBytes(entry.ids)
	}
	return total
}

func (vol *serviceVolumeIndex) postingListCacheBytesLocked() int64 {
	if vol == nil {
		return 0
	}
	return postingEntryCacheBytes(vol.termCache) +
		postingEntryCacheBytes(vol.pathTermCache) +
		postingEntryCacheBytes(vol.extCache) +
		postingEntryCacheBytes(vol.underRootCache) +
		postingRootCacheBytes(vol.underCache)
}

func (vol *serviceVolumeIndex) cacheNamePosting(term string, list []int) {
	if !vol.shouldCachePosting(list) {
		return
	}
	vol.termMu.Lock()
	if vol.termCache == nil {
		vol.termCache = make(map[string]postingCacheEntry)
	}
	vol.termCache[term] = postingCacheEntry{ids: list, gen: vol.cacheGeneration()}
	vol.termMu.Unlock()
}

func (vol *serviceVolumeIndex) cachePathPosting(term string, list []int) {
	if !vol.shouldCachePosting(list) {
		return
	}
	vol.termMu.Lock()
	if vol.pathTermCache == nil {
		vol.pathTermCache = make(map[string]postingCacheEntry)
	}
	vol.pathTermCache[term] = postingCacheEntry{ids: list, gen: vol.cacheGeneration()}
	vol.termMu.Unlock()
}

func (vol *serviceVolumeIndex) cacheExtPosting(ext string, list []int) {
	if !vol.shouldCachePosting(list) {
		return
	}
	vol.termMu.Lock()
	if vol.extCache == nil {
		vol.extCache = make(map[string]postingCacheEntry)
	}
	vol.extCache[ext] = postingCacheEntry{ids: list, gen: vol.cacheGeneration()}
	vol.termMu.Unlock()
}

func (vol *serviceVolumeIndex) scanNameTermPosting(term string) []int {
	if vol != nil && vol.index != nil {
		if ids, ok := vol.index.scanCompactLowerNameTerm(term); ok {
			return ids
		}
	}
	recordCount := vol.index.compactRecordCount()
	workers := min(runtime.GOMAXPROCS(0), max(1, recordCount/250_000))
	if workers <= 1 {
		out := make([]int, 0, 64)
		for i := 0; i < recordCount; i++ {
			rec := vol.index.compactRecord(i)
			if rec.Deleted {
				continue
			}
			if strings.Contains(vol.index.compactLowerNameAt(i), term) {
				out = append(out, i)
			}
		}
		return out	}
	parts := make([][]int, workers)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := worker * recordCount / workers
		end := (worker + 1) * recordCount / workers
		wg.Add(1)
		go func(worker, start, end int) {
			defer wg.Done()
			local := make([]int, 0, 64)
			for i := start; i < end; i++ {
				rec := vol.index.compactRecord(i)
				if rec.Deleted {
					continue
				}
				if strings.Contains(vol.index.compactLowerNameAt(i), term) {
					local = append(local, i)
				}
			}
			parts[worker] = local
		}(worker, start, end)
	}
	wg.Wait()
	total := 0
	for _, part := range parts {
		total += len(part)
	}
	out := make([]int, 0, total)
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

// countBareTermParallel counts records whose lower name contains the term, in
// parallel, without materializing the matching ID slice.  It returns ok=false
// when a mapped bulk scan is available so the caller keeps using the
// slice-based posting (which is cached and reused by search), and only pays the
// count-only walk when a full materialization would be required anyway.
func (vol *serviceVolumeIndex) countBareTermParallel(term string, hidden hiddenBaseIDs, pq parsedQuery) (int, bool) {
	if vol == nil || vol.index == nil || term == "" {
		return 0, false
	}
	if vol.index.MMapRecords != nil {
		// The mapped fast path returns the slice cheaply; prefer it so search
		// and count share the same cached posting.
		if _, ok := vol.index.scanCompactLowerNameTerm(term); ok {
			return 0, false
		}
	}
	recordCount := vol.index.compactRecordCount()
	workers := min(runtime.GOMAXPROCS(0), max(1, recordCount/250_000))
	count := 0
	if workers <= 1 {
		for i := 0; i < recordCount; i++ {
			if i&1023 == 0 && queryCanceled(pq) {
				return 0, false
			}
			rec := vol.index.compactRecord(i)
			if rec.Deleted {
				continue
			}
			if !hidden.empty() && hidden.contains(i) {
				continue
			}
			if strings.Contains(vol.index.compactLowerNameAt(i), term) {
				count++
			}
		}
		return count, true
	}
	parts := make([]int, workers)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := worker * recordCount / workers
		end := (worker + 1) * recordCount / workers
		wg.Add(1)
		go func(worker, start, end int) {
			defer wg.Done()
			local := 0
			for i := start; i < end; i++ {
				if i&1023 == 0 && queryCanceled(pq) {
					return
				}
				rec := vol.index.compactRecord(i)
				if rec.Deleted {
					continue
				}
				if !hidden.empty() && hidden.contains(i) {
					continue
				}
				if strings.Contains(vol.index.compactLowerNameAt(i), term) {
					local++
				}
			}
			parts[worker] = local
		}(worker, start, end)
	}
	wg.Wait()
	if queryCanceled(pq) {
		return 0, false
	}
	for _, part := range parts {
		count += part
	}
	return count, true
}

// scanCompactLowerNameTerm is the complete mapped-name fallback used when a
// persisted trigram posting is selective/incomplete.  The normal compact
// accessors intentionally reconstruct a CompactRecord on every visit; that
// is needlessly expensive for this exact, read-only name-table scan.  Read
// the lower-name token and deleted bit directly from the mapped record table
// while preserving the same record/substring semantics.
func (idx *Index) scanCompactLowerNameTerm(term string) ([]int, bool) {
	if idx == nil || term == "" {
		return nil, false
	}
	if m := idx.MMapRecords; m != nil {
		derived := m.fileDerived()
		if len(derived.LowerOffs) == 0 || len(derived.LowerLens) != len(derived.LowerOffs) {
			return nil, false
		}
		size := compactDiskRecordBytes
		if m.wideRefs {
			size = compactWideDiskRecordBytes
		}
		if size <= 0 || len(m.recordData) < m.count*size {
			return nil, false
		}
		termBytes := []byte(term)
		nameBytesForToken := func(token uint32) []byte {
			if token >= uint32(len(derived.LowerOffs)) {
				return nil
			}
			off := derived.LowerOffs[token]
			var nameBytes []byte
			if off == packedLowerSameAsName {
				nameOff := int(token) * 6
				if nameOff >= 0 && nameOff+6 <= len(m.tokenTable) {
					nameStart := binary.LittleEndian.Uint32(m.tokenTable[nameOff:])
					nameLen := binary.LittleEndian.Uint16(m.tokenTable[nameOff+4:])
					nameEnd := int(nameStart) + int(nameLen)
					if nameEnd >= int(nameStart) && nameEnd <= len(m.nameBlob) {
						nameBytes = m.nameBlob[int(nameStart):nameEnd]
					}
				}
			} else {
				end := int(off) + int(derived.LowerLens[token])
				if end >= int(off) && end <= len(derived.LowerBlob) {
					nameBytes = derived.LowerBlob[int(off):end]
				}
			}
			return nameBytes
		}
		tokenMatches := make([]bool, len(derived.LowerOffs))
		matchWorkers := min(runtime.GOMAXPROCS(0), max(1, len(derived.LowerOffs)/250_000))
		var matchWG sync.WaitGroup
		for worker := 0; worker < matchWorkers; worker++ {
			start := worker * len(derived.LowerOffs) / matchWorkers
			end := (worker + 1) * len(derived.LowerOffs) / matchWorkers
			matchWG.Add(1)
			go func(start, end int) {
				defer matchWG.Done()
				for nameID := start; nameID < end; nameID++ {
					tokenMatches[nameID] = bytes.Contains(nameBytesForToken(uint32(nameID)), termBytes)
				}
			}(start, end)
		}
		matchWG.Wait()
		workers := min(runtime.GOMAXPROCS(0), max(1, m.count/250_000))
		parts := make([][]int, workers)
		var wg sync.WaitGroup
		for worker := 0; worker < workers; worker++ {
			start := worker * m.count / workers
			end := (worker + 1) * m.count / workers
			wg.Add(1)
			go func(worker, start, end int) {
				defer wg.Done()
				local := make([]int, 0, 64)
				for i := start; i < end; i++ {
					base := i * size
					if m.recordData[base+size-1] != 0 {
						continue
					}
					_, nameID := m.recordRefs(base + 16)
					if nameID >= uint32(len(derived.LowerOffs)) {
						continue
					}
					if tokenMatches[nameID] {
						local = append(local, i)
					}
				}
				parts[worker] = local
			}(worker, start, end)
		}
		wg.Wait()
		total := 0
		for _, part := range parts {
			total += len(part)
		}
		out := make([]int, 0, total)
		for _, part := range parts {
			out = append(out, part...)
		}
		return out, true
	}
	if p := idx.PackedRecords; p != nil && len(p.LowerOffs) == p.Len() {
		workers := min(runtime.GOMAXPROCS(0), max(1, p.Len()/250_000))
		parts := make([][]int, workers)
		var wg sync.WaitGroup
		for worker := 0; worker < workers; worker++ {
			start := worker * p.Len() / workers
			end := (worker + 1) * p.Len() / workers
			wg.Add(1)
			go func(worker, start, end int) {
				defer wg.Done()
				local := make([]int, 0, 64)
				for i := start; i < end; i++ {
					rec := p.At(i)
					if rec.Deleted || !strings.Contains(p.lowerNameAt(i), term) {
						continue
					}
					local = append(local, i)
				}
				parts[worker] = local
			}(worker, start, end)
		}
		wg.Wait()
		total := 0
		for _, part := range parts {
			total += len(part)
		}
		out := make([]int, 0, total)
		for _, part := range parts {
			out = append(out, part...)
		}
		return out, true
	}
	return nil, false
}

func (vol *serviceVolumeIndex) pathTermPosting(term string) []int {
	vol.termMu.Lock()
	if vol.pathTermCache == nil {
		vol.pathTermCache = make(map[string]postingCacheEntry)
	}
	if entry, ok := vol.pathTermCache[term]; ok {
		if vol.cacheStampValid(entry.gen) {
			vol.termMu.Unlock()
			return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
				id, ok := vol.idForFRN(rec.FRN)
				return ok && vol.index.compactPathContainsTerm(id, term)
			})
		}
	}
	vol.termMu.Unlock()

	seen := make(map[int]struct{}, 64)
	out := make([]int, 0, 64)
	for _, id := range vol.nameTermPosting(term) {
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if !strings.ContainsAny(term, `\/*?[]:`) && (len(vol.childOffsets) > 0 || vol.children != nil) {
		traversed := make(map[int]struct{}, 64)
		for _, rootID := range vol.pathTermRootIDs(term) {
			if rootID < 0 || rootID >= vol.index.compactRecordCount() {
				continue
			}
			root := vol.index.compactRecord(rootID)
			if root.Deleted || root.Mode&uint32(os.ModeDir) == 0 {
				continue
			}
			stack := []int{rootID}
			for len(stack) > 0 {
				last := len(stack) - 1
				id := stack[last]
				stack = stack[:last]
				if _, ok := traversed[id]; ok || id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				traversed[id] = struct{}{}
				rec := vol.index.compactRecord(id)
				if !rec.Deleted {
					if _, ok := seen[id]; !ok {
						seen[id] = struct{}{}
						out = append(out, id)
					}
				}
				for _, childID := range vol.childIDsForRecord(id) {
					stack = append(stack, int(childID))
				}
			}
		}
	} else if !strings.ContainsAny(term, `\/*?[]:`) {
		for _, id := range vol.scanPathTermPosting(term) {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Ints(out)
	vol.cachePathPosting(term, out)
	return out
}

// countPathTermPostingLive is the count-only analogue of pathTermPosting.  It
// counts live records whose name contains the term plus live descendants of
// directory roots whose name contains the term, without materializing the ID
// slice.  It is exact for the same input as pathTermPosting (bare term, no
// path separators) and is intended for the count terminal where only the total
// is needed.  `hidden` applies overlay tombstone/shadow filtering to match the
// verified iterator path.  Every counted record is verified with
// compactPathContainsTerm so a malformed/legacy component posting that is a
// superset (rather than an exact path predicate) cannot over-count.
func (vol *serviceVolumeIndex) countPathTermPostingLive(term string, hidden func(int) bool) int {
	if vol == nil || vol.index == nil || term == "" {
		return 0
	}
	recordCount := vol.index.compactRecordCount()
	count := 0
	seen := make(map[int]struct{}, 64)
	// Name self-hits: records whose name contains the term.
	for _, id := range vol.nameTermPosting(term) {
		if id < 0 || id >= recordCount {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if hidden != nil && hidden(id) {
			continue
		}
		if !vol.index.compactPathContainsTerm(id, term) {
			continue
		}
		count++
	}
	if strings.ContainsAny(term, `\/*?[]:`) {
		return count
	}
	if len(vol.childOffsets) > 0 || vol.children != nil {
		traversed := make(map[int]struct{}, 64)
		for _, rootID := range vol.pathTermRootIDs(term) {
			if rootID < 0 || rootID >= recordCount {
				continue
			}
			root := vol.index.compactRecord(rootID)
			if root.Deleted || root.Mode&uint32(os.ModeDir) == 0 {
				continue
			}
			// A legitimate directory root whose own name contains the term
			// guarantees every descendant's path contains the term, so the
			// descendant walk needs no per-record path check.  A malformed or
			// legacy component posting can supply a superset root whose name
			// does NOT contain the term; those descendants must be verified
			// individually so the count stays exact.
			rootExact := strings.Contains(vol.index.compactLowerNameAt(rootID), term) ||
				(vol.index.Volume != "" && containsFoldASCII(vol.index.Volume, term))
			stack := []int{rootID}
			for len(stack) > 0 {
				last := len(stack) - 1
				id := stack[last]
				stack = stack[:last]
				if _, ok := traversed[id]; ok || id < 0 || id >= recordCount {
					continue
				}
				traversed[id] = struct{}{}
				rec := vol.index.compactRecord(id)
				if !rec.Deleted {
					if _, ok := seen[id]; !ok {
						seen[id] = struct{}{}
						if hidden == nil || !hidden(id) {
							if rootExact || vol.index.compactPathContainsTerm(id, term) {
								count++
							}
						}
					}
				}
				for _, childID := range vol.childIDsForRecord(id) {
					stack = append(stack, int(childID))
				}
			}
		}
		return count
	}
	// No child graph: scan paths directly.
	for _, id := range vol.scanPathTermPosting(term) {
		if id < 0 || id >= recordCount {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if hidden != nil && hidden(id) {
			continue
		}
		if !vol.index.compactPathContainsTerm(id, term) {
			continue
		}
		count++
	}
	return count
}

func (vol *serviceVolumeIndex) pathComponentPosting(term string) []int {
	if vol == nil || vol.index == nil || term == "" || strings.ContainsAny(term, `\/*?[]:`) {
		return vol.pathTermPosting(term)
	}
	cacheKey := "\x00pathcomponent:" + term
	vol.termMu.Lock()
	if vol.pathTermCache != nil {
		if entry, ok := vol.pathTermCache[cacheKey]; ok {
			if vol.cacheStampValid(entry.gen) {
				vol.termMu.Unlock()
				return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
					id, ok := vol.idForFRN(rec.FRN)
					return ok && vol.index.compactPathContainsTerm(id, term)
				})
			}
		}
	}
	vol.termMu.Unlock()

	roots := vol.pathComponentRootIDs(term)
	if len(roots) == 0 {
		vol.cachePathPosting(cacheKey, nil)
		return nil
	}
	seen := make(map[int]struct{}, 256)
	out := make([]int, 0, 256)
	for _, rootID := range roots {
		for _, id := range vol.underDescendants(rootID) {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Ints(out)
	vol.cachePathPosting(cacheKey, out)
	return out
}

func (vol *serviceVolumeIndex) pathComponentPostingAvailable(term string) bool {
	if vol == nil || vol.index == nil || term == "" {
		return false
	}
	if strings.ContainsAny(term, `\/*?[]:`) {
		return true
	}
	if len(vol.componentPosting32(term)) > 0 {
		return true
	}
	// exactNameIDs is only a cheap membership probe when the resident name
	// order (or exact-names map) is available.  In lowmem/mapped mode that
	// would fall back to a full-record scan just to answer "is this a
	// component?", which is far more expensive than any posting this function
	// gates.  The pathGrams check below is the actual lowmem posting source.
	if vol.exactNames != nil || (vol.queryIndex != nil && len(vol.queryIndex.nameOrder) > 0) {
		if len(vol.exactNameIDs(strings.ToLower(term))) > 0 {
			return true
		}
	}
	return vol.queryIndex != nil && vol.queryIndex.pathGrams != nil
}

type componentCoverageInterval struct {
	start uint32
	end   uint32
}

// mappedComponentCoverage is the exact base-record union for one component:
// every PCMP directory root contributes its SUBT interval, and filename
// self-hits contribute records outside those intervals.  The intervals are
// merged before either counting or walking so nested roots cannot multiply
// work or cardinality.
type mappedComponentCoverage struct {
	rootCount       int
	intervals       []componentCoverageInterval
	selfIDs         []int
	selfIDsComplete bool
	cardinality     int
	membership      []uint64
}

func (vol *serviceVolumeIndex) baseHasDeletedRecords() bool {
	if vol == nil || vol.index == nil {
		return false
	}
	if state := vol.index.baseDeletedState.Load(); state != 0 {
		return state == 2
	}
	hasDeleted := false
	idx := vol.index
	if idx.MMapRecords != nil {
		m := idx.MMapRecords
		refBytes := 6
		stride := compactDiskRecordBytes
		if m.wideRefs {
			refBytes = 8
			stride = compactWideDiskRecordBytes
		}
		deletedOffset := 16 + refBytes + 4 + 8 + 8
		for base := deletedOffset; base < len(m.recordData); base += stride {
			if m.recordData[base] != 0 {
				hasDeleted = true
				break
			}
		}
	} else if idx.PackedRecords != nil {
		for _, word := range idx.PackedRecords.DeletedBits {
			if word != 0 {
				hasDeleted = true
				break
			}
		}
	} else {
		for _, rec := range idx.Records {
			if rec.Deleted {
				hasDeleted = true
				break
			}
		}
	}
	if hasDeleted {
		vol.index.baseDeletedState.Store(2)
	} else {
		vol.index.baseDeletedState.Store(1)
	}
	return hasDeleted
}

func mergeComponentCoverageIntervals(intervals []componentCoverageInterval) []componentCoverageInterval {
	if len(intervals) < 2 {
		return intervals
	}
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].start == intervals[j].start {
			return intervals[i].end > intervals[j].end
		}
		return intervals[i].start < intervals[j].start
	})
	merged := intervals[:0]
	for _, current := range intervals {
		if len(merged) == 0 || current.start > merged[len(merged)-1].end {
			merged = append(merged, current)
			continue
		}
		if current.end > merged[len(merged)-1].end {
			merged[len(merged)-1].end = current.end
		}
	}
	return merged
}

func subtractComponentCoverageIntervals(incoming, covered []componentCoverageInterval) []componentCoverageInterval {
	if len(incoming) == 0 {
		return nil
	}
	if len(covered) == 0 {
		return append([]componentCoverageInterval(nil), incoming...)
	}
	newIntervals := make([]componentCoverageInterval, 0, len(incoming))
	for _, current := range incoming {
		start := current.start
		for _, existing := range covered {
			if existing.end <= start {
				continue
			}
			if existing.start >= current.end {
				break
			}
			if existing.start > start {
				end := existing.start
				if end > current.end {
					end = current.end
				}
				newIntervals = append(newIntervals, componentCoverageInterval{start: start, end: end})
			}
			if existing.end > start {
				start = existing.end
			}
			if start >= current.end {
				break
			}
		}
		if start < current.end {
			newIntervals = append(newIntervals, componentCoverageInterval{start: start, end: current.end})
		}
	}
	return newIntervals
}

func (coverage mappedComponentCoverage) containsInterval(vol *serviceVolumeIndex, id int) bool {
	if vol == nil || id < 0 || id >= vol.index.compactRecordCount() {
		return false
	}
	if id >= len(vol.subtreeStart) {
		return false
	}
	position := vol.subtreeStart[id]
	if position == ^uint32(0) {
		return false
	}
	interval := sort.Search(len(coverage.intervals), func(i int) bool {
		return coverage.intervals[i].start > position
	}) - 1
	return interval >= 0 && position < coverage.intervals[interval].end
}

func (coverage mappedComponentCoverage) contains(vol *serviceVolumeIndex, id int) bool {
	if id >= 0 && id/64 < len(coverage.membership) {
		return coverage.membership[id/64]&(uint64(1)<<uint(id%64)) != 0
	}
	if coverage.containsInterval(vol, id) {
		return true
	}
	pos := sort.SearchInts(coverage.selfIDs, id)
	return pos < len(coverage.selfIDs) && coverage.selfIDs[pos] == id
}

// buildMembership creates a compact transient membership filter for the
// persisted-order top-N driver. It is bounded by one bit per record and avoids
// repeating a binary search over merged SUBT intervals for every rank entry;
// it does not materialize CompactRecord values or alter the persisted index.
func (coverage *mappedComponentCoverage) buildMembership(vol *serviceVolumeIndex) {
	if coverage == nil || vol == nil || vol.index == nil || coverage.cardinality <= 0 {
		return
	}
	recordCount := vol.index.compactRecordCount()
	if recordCount <= 0 {
		return
	}
	coverage.membership = make([]uint64, (recordCount+63)/64)
	for _, current := range coverage.intervals {
		for pos := current.start; pos < current.end && int(pos) < len(vol.subtreeOrder); pos++ {
			id := vol.subtreeOrder[pos]
			if int(id) < recordCount {
				coverage.membership[id/64] |= uint64(1) << uint(id%64)
			}
		}
	}
	for _, id := range coverage.selfIDs {
		if id >= 0 && id < recordCount {
			coverage.membership[id/64] |= uint64(1) << uint(id%64)
		}
	}
}

func (vol *serviceVolumeIndex) buildMappedComponentCoverage(component string, selfIDs []int) (mappedComponentCoverage, bool) {
	coverage := mappedComponentCoverage{selfIDs: uniqueSortedInts(append([]int(nil), selfIDs...)), selfIDsComplete: true}
	if vol == nil || vol.index == nil || component == "" ||
		len(vol.subtreeOrder) == 0 || len(vol.subtreeStart) == 0 || len(vol.subtreeEnd) == 0 {
		return coverage, false
	}
	candidate, ok := vol.componentPostingCountCandidate(component)
	if !ok || (!candidate.mapped && candidate.ids == nil) {
		return coverage, false
	}
	return vol.buildMappedComponentCoverageFromRoots(candidate.materialize(), coverage.selfIDs)
}

func (vol *serviceVolumeIndex) buildMappedComponentCoverageFromRoots(roots []uint32, selfIDs []int) (mappedComponentCoverage, bool) {
	coverage := mappedComponentCoverage{selfIDs: uniqueSortedInts(append([]int(nil), selfIDs...)), selfIDsComplete: true}
	if vol == nil || vol.index == nil || len(vol.subtreeOrder) == 0 || len(vol.subtreeStart) == 0 || len(vol.subtreeEnd) == 0 {
		return coverage, false
	}
	uniqueRoots := make(map[uint32]struct{}, len(roots))
	for _, root := range roots {
		uniqueRoots[root] = struct{}{}
	}
	roots = roots[:0]
	for root := range uniqueRoots {
		roots = append(roots, root)
	}
	coverage.rootCount = len(roots)
	intervals := make([]componentCoverageInterval, 0, len(roots))
	for _, root32 := range roots {
		root := int(root32)
		if root < 0 || root >= len(vol.subtreeStart) || root >= len(vol.subtreeEnd) {
			return mappedComponentCoverage{}, false
		}
		start, end := vol.subtreeStart[root], vol.subtreeEnd[root]
		if start == ^uint32(0) || start > end || int(end) > len(vol.subtreeOrder) {
			return mappedComponentCoverage{}, false
		}
		intervals = append(intervals, componentCoverageInterval{start: start, end: end})
	}
	coverage.intervals = mergeComponentCoverageIntervals(intervals)
	if !vol.baseHasDeletedRecords() {
		for _, current := range coverage.intervals {
			coverage.cardinality += int(current.end - current.start)
		}
	} else {
		for _, current := range coverage.intervals {
			for pos := current.start; pos < current.end; pos++ {
				id := int(vol.subtreeOrder[pos])
				if id >= 0 && id < vol.index.compactRecordCount() && !vol.index.compactRecord(id).Deleted {
					coverage.cardinality++
				}
			}
		}
	}
	for _, id := range coverage.selfIDs {
		if id < 0 || id >= vol.index.compactRecordCount() || vol.index.compactRecord(id).Deleted ||
			vol.index.compactRecord(id).Mode&uint32(os.ModeDir) != 0 || coverage.containsInterval(vol, id) {
			continue
		}
		coverage.cardinality++
	}
	return coverage, true
}

func (coverage mappedComponentCoverage) countLive(vol *serviceVolumeIndex, hidden func(int) bool) (count, verified int) {
	if vol == nil || vol.index == nil {
		return 0, 0
	}
	if hidden == nil && !vol.baseHasDeletedRecords() {
		count = coverage.cardinality
		return count, 0
	}
	for _, current := range coverage.intervals {
		for pos := current.start; pos < current.end; pos++ {
			id := int(vol.subtreeOrder[pos])
			if id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			verified++
			if vol.index.compactRecord(id).Deleted || (hidden != nil && hidden(id)) {
				continue
			}
			count++
		}
	}
	for _, id := range coverage.selfIDs {
		if id < 0 || id >= vol.index.compactRecordCount() || vol.index.compactRecord(id).Deleted ||
			vol.index.compactRecord(id).Mode&uint32(os.ModeDir) != 0 || coverage.containsInterval(vol, id) {
			continue
		}
		verified++
		if hidden == nil || !hidden(id) {
			count++
		}
	}
	return count, verified
}

// countMappedComponentSelfHits walks the persisted SUBT order and advances
// over merged descendant intervals monotonically.  It reads folded LOWR bytes
// and record flags directly from the mapped tables; no paths, strings, or ID
// slices are materialized.  The caller adds hidden/overlay corrections once
// at the terminal count.
func (vol *serviceVolumeIndex) countMappedComponentSelfHits(term string, coverage mappedComponentCoverage, hidden func(int) bool, pq parsedQuery) (count, visited int, ok bool) {
	if vol == nil || vol.index == nil || term == "" || len(vol.subtreeOrder) < vol.index.compactRecordCount() {
		return 0, 0, false
	}
	termBytes := []byte(term)
	intervalPos := 0
	order := vol.subtreeOrder
	if m := vol.index.MMapRecords; m != nil {
		derived := m.fileDerived()
		if len(derived.LowerOffs) == 0 || len(derived.LowerLens) != len(derived.LowerOffs) {
			return 0, 0, false
		}
		size := compactDiskRecordBytes
		refBytes := 6
		if m.wideRefs {
			size = compactWideDiskRecordBytes
			refBytes = 8
		}
		lowerBytes := func(nameID uint32) []byte {
			if nameID >= uint32(len(derived.LowerOffs)) {
				return nil
			}
			off := derived.LowerOffs[nameID]
			if off == packedLowerSameAsName {
				nameOff := int(nameID) * 6
				if nameOff < 0 || nameOff+6 > len(m.tokenTable) {
					return nil
				}
				start := binary.LittleEndian.Uint32(m.tokenTable[nameOff:])
				length := binary.LittleEndian.Uint16(m.tokenTable[nameOff+4:])
				end := int(start) + int(length)
				if end < int(start) || end > len(m.nameBlob) {
					return nil
				}
				return m.nameBlob[int(start):end]
			}
			end := int(off) + int(derived.LowerLens[nameID])
			if end < int(off) || end > len(derived.LowerBlob) {
				return nil
			}
			return derived.LowerBlob[int(off):end]
		}
		for pos := 0; pos < len(order); {
			if pos&1023 == 0 && queryCanceled(pq) {
				return 0, visited, false
			}
			for intervalPos < len(coverage.intervals) && uint32(pos) >= coverage.intervals[intervalPos].end {
				intervalPos++
			}
			if intervalPos < len(coverage.intervals) && uint32(pos) >= coverage.intervals[intervalPos].start {
				pos = int(coverage.intervals[intervalPos].end)
				continue
			}
			id := int(order[pos])
			pos++
			if id < 0 || id >= m.count {
				continue
			}
			visited++
			base, valid := m.recordOffset(id)
			if !valid || m.recordData[base+size-1] != 0 {
				continue
			}
			modeOff := base + 16 + refBytes
			if binary.LittleEndian.Uint32(m.recordData[modeOff:])&uint32(os.ModeDir) != 0 {
				continue
			}
			_, nameID := m.recordRefs(base + 16)
			if bytes.Contains(lowerBytes(nameID), termBytes) && (hidden == nil || !hidden(id)) {
				count++
			}
		}
		return count, visited, true
	}
	if p := vol.index.PackedRecords; p != nil && p.Len() >= len(order) {
		for pos := 0; pos < len(order); {
			if pos&1023 == 0 && queryCanceled(pq) {
				return 0, visited, false
			}
			for intervalPos < len(coverage.intervals) && uint32(pos) >= coverage.intervals[intervalPos].end {
				intervalPos++
			}
			if intervalPos < len(coverage.intervals) && uint32(pos) >= coverage.intervals[intervalPos].start {
				pos = int(coverage.intervals[intervalPos].end)
				continue
			}
			id := int(order[pos])
			pos++
			if id < 0 || id >= p.Len() {
				continue
			}
			visited++
			rec := p.At(id)
			if rec.Deleted || rec.Mode&uint32(os.ModeDir) != 0 || hidden != nil && hidden(id) {
				continue
			}
			if strings.Contains(p.lowerNameAt(id), term) {
				count++
			}
		}
		return count, visited, true
	}
	return 0, visited, false
}

func (vol *serviceVolumeIndex) componentSelfHits(component string, pq parsedQuery) ([]int, bool) {
	namePQ := pq
	namePQ.MatchPath = false
	namePQ.Terms = []string{component}
	if vol != nil && vol.nameTrigramIndex() != nil {
		ids, ok := vol.filenameTrigramCandidates(namePQ)
		if !ok {
			return nil, false
		}
		out := make([]int, 0, len(ids))
		for _, id := range ids {
			if id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			rec := vol.index.compactRecord(id)
			if !rec.Deleted && rec.Mode&uint32(os.ModeDir) == 0 && strings.Contains(vol.index.compactLowerNameAt(id), strings.ToLower(component)) {
				out = append(out, id)
			}
		}
		return uniqueSortedInts(out), true
	}
	ids := vol.nameTermPosting(strings.ToLower(component))
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted && rec.Mode&uint32(os.ModeDir) == 0 {
			out = append(out, id)
		}
	}
	return uniqueSortedInts(out), true
}

func (vol *serviceVolumeIndex) mappedComponentCoverageForQuery(component string, pq parsedQuery) (mappedComponentCoverage, bool) {
	if candidate, candidateOK := vol.componentPostingCountCandidate(component); candidateOK && candidate.mapped && candidate.len() == 0 {
		return mappedComponentCoverage{}, false
	}
	selfIDs, ok := vol.componentSelfHits(component, pq)
	if !ok {
		return mappedComponentCoverage{}, false
	}
	coverage, ok := vol.buildMappedComponentCoverage(component, selfIDs)
	if ok {
		if candidate, candidateOK := vol.componentPostingCountCandidate(component); candidateOK && candidate.mapped && candidate.len() == 0 {
			// A missing exact key does not prove that no longer directory name
			// contains this substring; let the complete PCMP dictionary route
			// enumerate supersets instead.
			return mappedComponentCoverage{}, false
		}
	}
	return coverage, ok
}

// mappedComponentSubstringCoverage is the complete fallback when PNGR cannot
// prove filename self-hit coverage.  PCMP's key dictionary is complete for
// persisted directory components, so enumerate only keys containing the
// query and decode their root postings.  LOWR/name order supplies the complete
// base filename verification without reconstructing paths.
func (vol *serviceVolumeIndex) mappedComponentSubstringCoverage(component string) (mappedComponentCoverage, bool) {
	return vol.mappedComponentSubstringCoverageMode(component, true)
}

func (vol *serviceVolumeIndex) mappedComponentSubstringCoverageForTop(component string) (mappedComponentCoverage, bool) {
	return vol.mappedComponentSubstringCoverageMode(component, false)
}

func (vol *serviceVolumeIndex) mappedComponentSubstringCoverageMode(component string, includeSelfIDs bool) (mappedComponentCoverage, bool) {
	if vol == nil || vol.index == nil || component == "" || vol.index.Derived.Postings == nil {
		return mappedComponentCoverage{}, false
	}
	key := strings.ToLower(component)
	if includeSelfIDs {
		vol.index.componentCoverageMu.Lock()
		if vol.index.componentCoverageCache != nil {
			if coverage, ok := vol.index.componentCoverageCache[key]; ok {
				vol.index.componentCoverageMu.Unlock()
				return coverage, true
			}
		}
		vol.index.componentCoverageMu.Unlock()
	}

	section, exists := vol.index.Derived.Postings[indexSectionPCMP]
	if !exists || len(section.Data) == 0 {
		return mappedComponentCoverage{}, false
	}
	keys, complete := section.matchingStringPostingKeys(key)
	if !complete {
		return mappedComponentCoverage{}, false
	}
	roots := make([]uint32, 0, len(keys))
	for _, postingKey := range keys {
		it, _, ok := section.stringPostingIterator(postingKey)
		if !ok {
			return mappedComponentCoverage{}, false
		}
		for {
			ids, _, ok := it.nextBlock()
			if !ok {
				break
			}
			roots = append(roots, ids...)
		}
	}
	order := vol.mappedOrCompactNameOrder()
	recordCount := vol.index.compactRecordCount()
	if len(order) < recordCount {
		return mappedComponentCoverage{}, false
	}
	selfIDs := []int(nil)
	if includeSelfIDs {
		selfIDs = make([]int, 0, 32)
		for _, id32 := range order {
			id := int(id32)
			if id < 0 || id >= recordCount {
				return mappedComponentCoverage{}, false
			}
			rec := vol.index.compactRecord(id)
			if !rec.Deleted && rec.Mode&uint32(os.ModeDir) == 0 && strings.Contains(vol.index.compactLowerNameAt(id), key) {
				selfIDs = append(selfIDs, id)
			}
		}
	}
	coverage, ok := vol.buildMappedComponentCoverageFromRoots(roots, selfIDs)
	if !ok {
		return mappedComponentCoverage{}, false
	}
	coverage.selfIDsComplete = includeSelfIDs
	if includeSelfIDs {
		vol.index.componentCoverageMu.Lock()
		if vol.index.componentCoverageCache == nil {
			vol.index.componentCoverageCache = make(map[string]mappedComponentCoverage)
		}
		vol.index.componentCoverageCache[key] = coverage
		vol.index.componentCoverageMu.Unlock()
	}
	return coverage, true
}

// mappedComponentCount counts the exact mapped union without materializing
// every descendant ID as a separate posting.  It remains as a small wrapper
// for callers/tests that do not have a parsed query and therefore use the
// complete legacy name scan for file self-hits.
func (vol *serviceVolumeIndex) mappedComponentCount(component string) (int, bool) {
	if vol == nil || vol.index == nil || component == "" ||
		len(vol.subtreeOrder) == 0 || len(vol.subtreeStart) == 0 || len(vol.subtreeEnd) == 0 {
		return 0, false
	}
	selfIDs := vol.nameTermPosting(strings.ToLower(component))
	coverage, ok := vol.buildMappedComponentCoverage(component, selfIDs)
	if !ok {
		return 0, false
	}
	count, _ := coverage.countLive(vol, nil)
	return count, true
}

func (vol *serviceVolumeIndex) pathComponentRootIDs(term string) []int {
	if vol == nil || vol.index == nil || term == "" {
		return nil
	}
	term = strings.ToLower(term)
	if ids := vol.componentPosting32(term); len(ids) > 0 {
		out := make([]int, 0, len(ids))
		for _, id := range ids {
			out = append(out, int(id))
		}
		return out
	}
	if vol.queryIndex == nil {
		return vol.exactNameIDs(term)
	}
	candidates := make([]uint32, 0, 8)
	for _, id := range vol.exactNameIDs(term) {
		if id >= 0 && id < vol.index.compactRecordCount() {
			rec := vol.index.compactRecord(id)
			if !rec.Deleted && rec.Mode&uint32(os.ModeDir) != 0 {
				candidates = append(candidates, uint32(id))
			}
		}
	}
	if len(candidates) == 0 && len(term) >= 3 && vol.queryIndex.pathGrams != nil {
		grams := componentGrams(term)
		lists := make([][]uint32, 0, len(grams))
		for _, gram := range grams {
			list := vol.queryIndex.pathGrams[gram]
			if len(list) == 0 {
				return nil
			}
			lists = append(lists, list)
		}
		sortUint32ListsByLen(lists)
		if len(lists) > 0 {
			candidates = append([]uint32(nil), lists[0]...)
			for _, list := range lists[1:] {
				candidates = intersectSortedUint32s(candidates, list)
				if len(candidates) == 0 {
					return nil
				}
			}
		}
	}
	out := make([]int, 0, len(candidates))
	for _, id32 := range candidates {
		id := int(id32)
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 {
			continue
		}
		if strings.Contains(vol.index.compactLowerNameAt(id), term) {
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return uniqueSortedInts(out)
}

func (vol *serviceVolumeIndex) scanPathTermPosting(term string) []int {
	recordCount := vol.index.compactRecordCount()
	workers := min(runtime.GOMAXPROCS(0), max(1, recordCount/250_000))
	if workers <= 1 {
		out := make([]int, 0, 64)
		for i := 0; i < recordCount; i++ {
			rec := vol.index.compactRecord(i)
			if !rec.Deleted && vol.index.compactPathContainsTerm(i, term) {
				out = append(out, i)
			}
		}
		return out
	}
	parts := make([][]int, workers)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := worker * recordCount / workers
		end := (worker + 1) * recordCount / workers
		wg.Add(1)
		go func(worker, start, end int) {
			defer wg.Done()
			local := make([]int, 0, 64)
			for i := start; i < end; i++ {
				rec := vol.index.compactRecord(i)
				if !rec.Deleted && vol.index.compactPathContainsTerm(i, term) {
					local = append(local, i)
				}
			}
			parts[worker] = local
		}(worker, start, end)
	}
	wg.Wait()
	total := 0
	for _, part := range parts {
		total += len(part)
	}
	out := make([]int, 0, total)
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

func (vol *serviceVolumeIndex) pathTermRootIDs(term string) []int {
	roots := vol.pathComponentRootIDs(term)
	if len(roots) == 0 {
		roots = vol.exactNameIDs(term)
	}
	for _, id := range vol.nameTermPosting(term) {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 || vol.index.compactLowerNameAt(id) == term {
			continue
		}
		roots = append(roots, id)
	}
	return roots
}

func (vol *serviceVolumeIndex) extPosting(ext string) []int {
	if ids32 := vol.extPosting32(ext); ids32 != nil {
		list := make([]int, 0, len(ids32))
		for _, id := range ids32 {
			list = append(list, int(id))
		}
		return vol.withRecentCandidates(list, 0, func(rec CompactRecord) bool {
			actual := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
			return strings.EqualFold(actual, ext)
		})
	}
	vol.termMu.Lock()
	if vol.extCache == nil {
		vol.extCache = make(map[string]postingCacheEntry)
	}
	if entry, ok := vol.extCache[ext]; ok {
		if vol.cacheStampValid(entry.gen) {
			vol.termMu.Unlock()
			return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
				actual := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
				return strings.EqualFold(actual, ext)
			})
		}
	}
	vol.termMu.Unlock()

	list := make([]int, 0, 64)
	for i := 0; i < vol.index.compactRecordCount(); i++ {
		rec := vol.index.compactRecord(i)
		if rec.Deleted {
			continue
		}
		actual := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
		if strings.EqualFold(actual, ext) {
			list = append(list, i)
		}
	}
	vol.cacheExtPosting(ext, list)
	return list
}

func (vol *serviceVolumeIndex) extPosting32(ext string) []uint32 {
	if vol == nil {
		return nil
	}
	key := strings.ToLower(ext)
	if vol.index != nil && vol.index.Derived.Postings != nil {
		if ids := vol.index.Derived.Postings[indexSectionPEXT].stringPosting(key); ids != nil {
			return ids
		}
	}
	if vol.queryIndex != nil && vol.queryIndex.ext != nil {
		if ids, ok := vol.queryIndex.ext[key]; ok {
			return ids
		}
	}
	return nil
}

func (vol *serviceVolumeIndex) extPostingCountCandidate(ext string) (postingCountCandidate, bool) {
	if vol == nil {
		return postingCountCandidate{}, false
	}
	key := strings.ToLower(ext)
	if vol.index != nil && vol.index.Derived.Postings != nil {
		if section, exists := vol.index.Derived.Postings[indexSectionPEXT]; exists {
			if it, count, ok := section.stringPostingIterator(key); ok {
				return postingCountCandidate{it: it, count: count, mapped: true}, true
			}
			return postingCountCandidate{mapped: true}, true
		}
	}
	if vol.queryIndex != nil && vol.queryIndex.ext != nil {
		if ids, ok := vol.queryIndex.ext[key]; ok {
			return postingCountCandidate{ids: ids}, true
		}
		return postingCountCandidate{}, true
	}
	return postingCountCandidate{}, false
}

func (vol *serviceVolumeIndex) extPostingCount(ext string) int {
	candidate, ok := vol.extPostingCountCandidate(ext)
	if !ok {
		return 0
	}
	return candidate.len()
}

func (vol *serviceVolumeIndex) componentPosting32(component string) []uint32 {
	if vol == nil {
		return nil
	}
	key := strings.ToLower(component)
	if vol.index != nil && vol.index.Derived.Postings != nil {
		if ids := vol.index.Derived.Postings[indexSectionPCMP].stringPosting(key); ids != nil {
			return ids
		}
	}
	if vol.queryIndex != nil && vol.queryIndex.components != nil {
		if ids, ok := vol.queryIndex.components[key]; ok {
			return ids
		}
	}
	return nil
}

func (vol *serviceVolumeIndex) componentPostingBlockIterator(component string) (postingBlockIterator, int, bool) {
	if vol == nil || vol.index == nil || vol.index.Derived.Postings == nil {
		return postingBlockIterator{}, 0, false
	}
	key := strings.ToLower(component)
	return vol.index.Derived.Postings[indexSectionPCMP].stringPostingIterator(key)
}

func (vol *serviceVolumeIndex) componentPostingCount(component string) int {
	candidate, ok := vol.componentPostingCountCandidate(component)
	if !ok {
		return 0
	}
	return candidate.len()
}

func (vol *serviceVolumeIndex) componentPostingCountCandidate(component string) (postingCountCandidate, bool) {
	if vol == nil {
		return postingCountCandidate{}, false
	}
	key := strings.ToLower(component)
	if vol.index != nil && vol.index.Derived.Postings != nil {
		if section, exists := vol.index.Derived.Postings[indexSectionPCMP]; exists {
			if it, count, ok := section.stringPostingIterator(key); ok {
				return postingCountCandidate{it: it, count: count, mapped: true}, true
			}
			return postingCountCandidate{mapped: true}, true
		}
	}
	if vol.queryIndex != nil && vol.queryIndex.components != nil {
		if ids, ok := vol.queryIndex.components[key]; ok {
			return postingCountCandidate{ids: ids}, true
		}
		return postingCountCandidate{}, true
	}
	return postingCountCandidate{}, false
}

func (vol *serviceVolumeIndex) extTopPosting(ext string, limit int, pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.queryIndex == nil || limit <= 0 {
		return nil, false
	}
	if vol.queryIndex.extTop != nil && pq.SortColumn == "" {
		ids32, ok := vol.queryIndex.extTop[strings.ToLower(ext)]
		if !ok || len(ids32) < limit {
			return nil, false
		}
		list := make([]int, 0, len(ids32)+len(vol.recentIDs))
		seen := make(map[int]struct{}, len(ids32)+len(vol.recentIDs))
		for _, id32 := range ids32 {
			id := int(id32)
			list = append(list, id)
			seen[id] = struct{}{}
		}
		for id := range vol.recentIDs {
			if _, exists := seen[id]; exists || id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			rec := vol.index.compactRecord(id)
			actual := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
			if strings.EqualFold(actual, ext) {
				list = append(list, id)
			}
		}
		return topCandidateIDsByRank(list, limit, vol.index, vol.nameOrderRanks()), true
	}
	if ids, ok := vol.mappedExtTopPosting(ext, limit, pq); ok {
		return ids, true
	}
	return nil, false
}

// countExtPostingWithRecent is the count-side twin of extTopPosting's recentID
// merge.  The legacy engine mutates base records in place and only tracks live
// changes in vol.recentIDs (no overlay snapshot is published), so the persisted
// posting count must be reconciled against recent records whose actual
// extension matches.  It mirrors extTopPosting's merge gate (resident extTop
// and default sort) so search and count stay in parity.
func (vol *serviceVolumeIndex) countExtPostingWithRecent(ext string, pq parsedQuery) (int, bool) {
	if vol == nil {
		return 0, false
	}
	posting, ok := vol.extPostingCountCandidate(ext)
	if !ok {
		return 0, false
	}
	if pq.SortColumn != "" || vol.queryIndex == nil || vol.queryIndex.extTop == nil || len(vol.recentIDs) == 0 {
		return posting.len(), true
	}
	ids := posting.materialize()
	seen := make(map[int]struct{}, len(ids))
	for _, id32 := range ids {
		seen[int(id32)] = struct{}{}
	}
	count := len(ids)
	for id := range vol.recentIDs {
		if _, exists := seen[id]; exists || id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		actual := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
		if strings.EqualFold(actual, ext) {
			count++
		}
	}
	return count, true
}

func (vol *serviceVolumeIndex) mappedExtTopPosting(ext string, limit int, pq parsedQuery) ([]int, bool) {
	if limit <= 0 || limit > serviceExtTopPostingLimit {
		return nil, false
	}
	candidate, ok := vol.extPostingCountCandidate(ext)
	if !ok || !candidate.mapped || candidate.len() == 0 {
		return nil, false
	}
	ranks := vol.rankForQuery(pq)
	boundSort := pq.SortColumn
	if boundSort == "" && pq.MatchPath {
		boundSort = "type"
	}
	blocks, canSkipByBlockRank := candidate.it.rankOrderedBlockRefsForSort(boundSort)
	if len(blocks) == 0 {
		return nil, false
	}
	h := make(extRankMaxHeap, 0, limit)
	recordCount := vol.index.compactRecordCount()
	decodedBlocks := 0
	skippedBlocks := 0
	for blockPos, ref := range blocks {
		if canSkipByBlockRank && len(h) >= limit && ref.meta.minRank > h[0].rank {
			skippedBlocks = len(blocks) - blockPos
			break
		}
		ids, _, ok := candidate.it.blockAt(ref.index)
		if !ok {
			return nil, false
		}
		decodedBlocks++
		for _, id := range ids {
			if int(id) < 0 || int(id) >= recordCount {
				continue
			}
			rec := vol.index.compactRecord(int(id))
			if rec.Deleted {
				continue
			}
			item := extRankItem{id: id, rank: extRankOf(id, ranks)}
			if len(h) < limit {
				heap.Push(&h, item)
				continue
			}
			if extRankLess(item, h[0]) {
				h[0] = item
				heap.Fix(&h, 0)
			}
		}
	}
	pq.Trace.addPostingBlocks(decodedBlocks, skippedBlocks)
	if len(h) == 0 {
		return []int{}, true
	}
	top := make([]uint32, len(h), len(h)+len(vol.recentIDs))
	seen := make(map[uint32]struct{}, len(h)+len(vol.recentIDs))
	for i := range h {
		top[i] = h[i].id
		seen[h[i].id] = struct{}{}
	}
	for id := range vol.recentIDs {
		if id < 0 || id >= recordCount {
			continue
		}
		id32 := uint32(id)
		if _, exists := seen[id32]; exists {
			continue
		}
		rec := vol.index.compactRecord(id)
		actual := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
		if rec.Deleted || !strings.EqualFold(actual, ext) {
			continue
		}
		top = append(top, id32)
	}
	sortExtTopByRank(top, ranks)
	if len(top) > limit {
		top = top[:limit]
	}
	out := make([]int, len(top))
	for i, id := range top {
		out[i] = int(id)
	}
	pq.Trace.addTerm(traceTerm{
		Term:      strings.ToLower(ext),
		Kind:      "extension",
		Source:    "mapped-ext-top",
		CountHint: candidate.len(),
		Exact:     true,
		Volume:    vol.index.Volume,
	})
	return out, true
}

// mappedComponentTopPosting returns top-ranked records from component
// subtrees. PCMP block bounds are computed from descendant SUBT minima, so a
// skipped block cannot contain a better descendant than the current heap
// threshold. This route is intentionally narrow; filtered/overlay queries use
// the complete component iterator instead of risking a truncated result.
func (vol *serviceVolumeIndex) mappedComponentTopPosting(component string, limit int, pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || limit <= 0 || len(vol.recentIDs) > 0 ||
		len(vol.subtreeSizeRank) == 0 || len(vol.subtreeModRank) == 0 || len(vol.subtreeExtRank) == 0 ||
		len(vol.subtreeTypeRank) == 0 || len(vol.subtreePathRank) == 0 {
		return nil, false
	}
	rankPQ := pq
	if rankPQ.MatchPath && rankPQ.SortColumn == "" {
		rankPQ.SortColumn = "type"
	}
	// A complete PCMP dictionary is sufficient to supply directory roots;
	// defer LOWR self-hit verification to the persisted-order driver so top-N
	// searches do not materialize the full self-hit set first.
	if substringCoverage, substringOK := vol.mappedComponentSubstringCoverageForTop(component); substringOK {
		return vol.mappedComponentTopFromCoverage(component, substringCoverage, limit, pq)
	}
	coverage, exactOK := vol.mappedComponentCoverageForQuery(component, pq)
	candidate, candidateOK := vol.componentPostingCountCandidate(component)
	if !exactOK || !candidateOK || !candidate.mapped {
		if substringCoverage, substringOK := vol.mappedComponentSubstringCoverageForTop(component); substringOK {
			return vol.mappedComponentTopFromCoverage(component, substringCoverage, limit, pq)
		}
		return nil, false
	}
	if candidate.len() == 0 {
		if pq.Trace != nil {
			pq.Trace.addComponentStats("self-only", coverage.rootCount, len(coverage.intervals), coverage.cardinality, len(coverage.selfIDs), len(coverage.selfIDs), false)
			pq.Trace.addTerm(traceTerm{Term: strings.ToLower(component), Kind: "path-subtree", Source: "mapped-component-top", CountHint: 0, Exact: true, Volume: vol.index.Volume})
		}
		return topCandidateIDsByRank(coverage.selfIDs, limit, vol.index, vol.rankForQuery(rankPQ)), true
	}
	blocks, canSkipByBlockRank := candidate.it.rankOrderedBlockRefsForSort(rankPQ.SortColumn)
	if len(blocks) == 0 {
		return nil, false
	}
	ranks := vol.rankForQuery(rankPQ)
	h := make(extRankMaxHeap, 0, limit)
	addID := func(id32 uint32) {
		item := extRankItem{id: id32, rank: extRankOf(id32, ranks)}
		if len(h) < limit {
			heap.Push(&h, item)
		} else if extRankLess(item, h[0]) {
			h[0] = item
			heap.Fix(&h, 0)
		}
	}
	verified := 0
	driver := "interval-bounds"
	decodedBlocks := 0
	skippedBlocks := 0
	// Once the exact coverage is broad, the persisted global order is the
	// cheaper driver: membership checks stop at the requested top-N instead of
	// walking every descendant in a large SUBT interval.  The final global
	// merge still compares actual entries and tie-breaks across volumes.
	order := vol.orderForQuery(rankPQ)
	if coverage.cardinality >= max(limit*8, 4096) && len(order) >= vol.index.compactRecordCount() {
		driver = "persisted-order"
		coverage.buildMembership(vol)
		for _, id32 := range order {
			if queryCanceled(pq) {
				return nil, false
			}
			id := int(id32)
			if !coverage.contains(vol, id) || id < 0 || id >= vol.index.compactRecordCount() || vol.index.compactRecord(id).Deleted {
				continue
			}
			verified++
			addID(id32)
			if len(h) >= limit {
				break
			}
		}
		skippedBlocks = len(blocks)
	} else {
		processedIntervals := make([]componentCoverageInterval, 0, candidate.len()/1024+1)
		for blockPos, ref := range blocks {
			if canSkipByBlockRank && len(h) >= limit && ref.meta.minRank > h[0].rank {
				skippedBlocks = len(blocks) - blockPos
				break
			}
			roots, _, ok := candidate.it.blockAt(ref.index)
			if !ok {
				return nil, false
			}
			decodedBlocks++
			blockIntervals := make([]componentCoverageInterval, 0, len(roots))
			for _, root32 := range roots {
				root := int(root32)
				if root < 0 || root >= len(vol.subtreeStart) || root >= len(vol.subtreeEnd) {
					return nil, false
				}
				start, end := vol.subtreeStart[root], vol.subtreeEnd[root]
				if start == ^uint32(0) || start > end || int(end) > len(vol.subtreeOrder) {
					return nil, false
				}
				blockIntervals = append(blockIntervals, componentCoverageInterval{start: start, end: end})
			}
			blockIntervals = mergeComponentCoverageIntervals(blockIntervals)
			for _, current := range subtractComponentCoverageIntervals(blockIntervals, processedIntervals) {
				for pos := current.start; pos < current.end; pos++ {
					id32 := vol.subtreeOrder[pos]
					id := int(id32)
					if id < 0 || id >= vol.index.compactRecordCount() || vol.index.compactRecord(id).Deleted {
						continue
					}
					verified++
					addID(id32)
				}
			}
			processedIntervals = mergeComponentCoverageIntervals(append(processedIntervals, blockIntervals...))
		}
		for _, id := range coverage.selfIDs {
			if id >= 0 && id < vol.index.compactRecordCount() && !vol.index.compactRecord(id).Deleted && !coverage.containsInterval(vol, id) {
				verified++
				addID(uint32(id))
			}
		}
	}
	if pq.Trace != nil {
		pq.Trace.addPostingBlocks(decodedBlocks, skippedBlocks)
		pq.Trace.addComponentStats(driver, coverage.rootCount, len(coverage.intervals), coverage.cardinality, len(coverage.selfIDs), verified, canSkipByBlockRank)
		pq.Trace.addTerm(traceTerm{Term: strings.ToLower(component), Kind: "path-subtree", Source: "mapped-component-top", CountHint: candidate.len(), Exact: true, Volume: vol.index.Volume})
	}
	out := make([]uint32, len(h))
	for i := range h {
		out[i] = h[i].id
	}
	sortExtTopByRank(out, ranks)
	if len(out) > limit {
		out = out[:limit]
	}
	ids := make([]int, len(out))
	for i, id := range out {
		ids[i] = int(id)
	}
	return ids, true
}

func (vol *serviceVolumeIndex) mappedComponentTopFromCoverage(component string, coverage mappedComponentCoverage, limit int, pq parsedQuery) ([]int, bool) {
	if len(coverage.intervals) == 0 {
		if ids, ok := vol.completeSelfNameGramTop(component, limit, pq); ok {
			return ids, true
		}
	}
	if vol == nil || vol.index == nil || limit <= 0 {
		return nil, false
	}
	term := strings.ToLower(component)
	rankPQ := pq
	if rankPQ.MatchPath && rankPQ.SortColumn == "" {
		rankPQ.SortColumn = "type"
	}
	ranks := vol.rankForQuery(rankPQ)
	h := make(extRankMaxHeap, 0, limit)
	addID := func(id int) {
		item := extRankItem{id: uint32(id), rank: extRankOf(uint32(id), ranks)}
		if len(h) < limit {
			heap.Push(&h, item)
		} else if extRankLess(item, h[0]) {
			h[0] = item
			heap.Fix(&h, 0)
		}
	}
	verified := 0
	driver := "interval-substring"
	selfHitCount := len(coverage.selfIDs)
	order := vol.orderForQuery(rankPQ)
	if coverage.cardinality >= max(limit*8, 4096) && len(order) >= vol.index.compactRecordCount() {
		selfPQ := rankPQ
		// Component default order promotes directories by type; complete
		// self-name postings only contribute file self-hits, so name order is
		// the exact tie-breaker for this subset.
		if selfPQ.SortColumn == "type" {
			selfPQ.SortColumn = ""
		}
		selfTop, selfOK := vol.completeSelfNameGramTop(term, limit, selfPQ)
		if selfOK {
			driver = "persisted-order-pngc-self"
			selfHitCount = len(selfTop)
			seen := make(map[int]struct{}, len(selfTop)+limit)
			for _, id := range selfTop {
				if id < 0 || id >= vol.index.compactRecordCount() || vol.index.compactRecord(id).Deleted {
					continue
				}
				seen[id] = struct{}{}
				verified++
				addID(id)
			}
			coverage.buildMembership(vol)
			for _, id32 := range order {
				if queryCanceled(pq) {
					return nil, false
				}
				if len(h) >= limit && extRankOf(id32, ranks) > h[0].rank {
					break
				}
				id := int(id32)
				if id < 0 || id >= vol.index.compactRecordCount() || vol.index.compactRecord(id).Deleted || !coverage.contains(vol, id) {
					continue
				}
				if _, already := seen[id]; already {
					continue
				}
				seen[id] = struct{}{}
				verified++
				addID(id)
			}
		} else {
			driver = "persisted-order-substring"
			for _, id32 := range order {
				if queryCanceled(pq) {
					return nil, false
				}
				id := int(id32)
				if id < 0 || id >= vol.index.compactRecordCount() || vol.index.compactRecord(id).Deleted {
					continue
				}
				matched := coverage.contains(vol, id)
				if !matched {
					rec := vol.index.compactRecord(id)
					matched = rec.Mode&uint32(os.ModeDir) == 0 && strings.Contains(vol.index.compactLowerNameAt(id), term)
				}
				if !matched {
					continue
				}
				verified++
				addID(id)
				if len(h) >= limit {
					break
				}
			}
		}
	} else {
		if !coverage.selfIDsComplete {
			return nil, false
		}
		for _, current := range coverage.intervals {
			for pos := current.start; pos < current.end; pos++ {
				id := int(vol.subtreeOrder[pos])
				if id < 0 || id >= vol.index.compactRecordCount() || vol.index.compactRecord(id).Deleted {
					continue
				}
				verified++
				addID(id)
			}
		}
		for _, id := range coverage.selfIDs {
			if id >= 0 && id < vol.index.compactRecordCount() && !vol.index.compactRecord(id).Deleted && !coverage.containsInterval(vol, id) {
				verified++
				addID(id)
			}
		}
	}
	if pq.Trace != nil {
		if driver == "persisted-order-substring" {
			pq.Trace.addPostingBlocks(0, max(0, len(order)-verified))
		}
		pq.Trace.addComponentStats(driver, coverage.rootCount, len(coverage.intervals), coverage.cardinality, selfHitCount, verified, false)
		pq.Trace.addTerm(traceTerm{Term: strings.ToLower(component), Kind: "path-substring", Source: "mapped-component-substring", CountHint: coverage.cardinality, Exact: true, Volume: vol.index.Volume})
	}
	out := make([]uint32, len(h))
	for i := range h {
		out[i] = h[i].id
	}
	sortExtTopByRank(out, ranks)
	if len(out) > limit {
		out = out[:limit]
	}
	ids := make([]int, len(out))
	for i, id := range out {
		ids[i] = int(id)
	}
	return ids, true
}

func (vol *serviceVolumeIndex) extTopPathTermCandidates(ext string, terms []string, limit int) ([]int, bool) {
	if vol == nil || vol.queryIndex == nil || vol.queryIndex.extTop == nil || limit <= 0 || len(terms) == 0 {
		return nil, false
	}
	ids32, ok := vol.queryIndex.extTop[strings.ToLower(ext)]
	if !ok {
		return nil, false
	}
	out := make([]int, 0, limit)
	seen := make(map[int]struct{}, min(len(ids32)+len(vol.recentIDs), serviceExtTopPostingLimit))
	for _, id32 := range ids32 {
		id := int(id32)
		seen[id] = struct{}{}
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !vol.index.compactPathContainsAll(id, terms) {
			continue
		}
		out = append(out, id)
		if len(out) >= limit {
			return out, true
		}
	}
	for id := range vol.recentIDs {
		if _, exists := seen[id]; exists || id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		actual := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
		if rec.Deleted || !strings.EqualFold(actual, ext) || !vol.index.compactPathContainsAll(id, terms) {
			continue
		}
		out = append(out, id)
	}
	if len(out) < limit && len(ids32) >= serviceExtTopPostingLimit {
		return nil, false
	}
	return topCandidateIDsByRank(out, limit, vol.index, vol.nameOrderRanks()), true
}

func (vol *serviceVolumeIndex) extPostingPathTermCandidates(ext string, terms []string, limit int) ([]int, bool) {
	if vol == nil || vol.index == nil || limit <= 0 || len(terms) == 0 {
		return nil, false
	}
	ids := vol.extPosting(ext)
	if len(ids) == 0 || len(ids) > serviceComponentMultiTermScanMaxIDs {
		return nil, false
	}
	out := make([]int, 0, min(limit, len(ids)))
	for _, id := range ids {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !vol.index.compactPathContainsAll(id, terms) {
			continue
		}
		out = append(out, id)
	}
	if len(out) == 0 {
		return []int{}, true
	}
	return topCandidateIDsByRank(out, limit, vol.index, vol.nameOrderRanks()), true
}

func (vol *serviceVolumeIndex) withRecentCandidates(base []int, seq uint64, keep func(CompactRecord) bool) []int {
	if vol == nil {
		return base
	}
	if engineV9Enabled() {
		if seq == 0 || seq == vol.cacheGeneration() {
			return base
		}
		return base
	}
	if len(vol.recentIDs) == 0 || seq == vol.recentSeq {
		return base
	}
	out := append([]int(nil), base...)
	seen := make(map[int]struct{}, len(out))
	for _, id := range out {
		seen[id] = struct{}{}
	}
	for id := range vol.recentIDs {
		if _, ok := seen[id]; ok || id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted && keep(rec) {
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out
}

func (vol *serviceVolumeIndex) cacheGeneration() uint64 {
	if vol == nil {
		return 0
	}
	if engineV9Enabled() {
		if snap := vol.snap.Load(); snap != nil {
			return snap.gen
		}
		return vol.snapshotGen.Load()
	}
	return vol.recentSeq
}

func (vol *serviceVolumeIndex) cacheStampValid(stamp uint64) bool {
	if vol == nil || !engineV9Enabled() {
		return true
	}
	return stamp == vol.cacheGeneration()
}

func intersectSortedInts(a, b []int) []int {
	out := a[:0]
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return out
}

func intersectSortedUint32s(a, b []uint32) []uint32 {
	out := a[:0]
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return out
}

func materializePostingBlockIterator(it postingBlockIterator, count int) []uint32 {
	if count <= 0 {
		return nil
	}
	out := make([]uint32, 0, count)
	for it.next < it.end {
		ids, _, ok := it.nextBlock()
		if !ok {
			return nil
		}
		out = append(out, ids...)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func intersectSortedUint32sWithPostingIterator(a []uint32, it postingBlockIterator) []uint32 {
	out := a[:0]
	cursor := 0
	for cursor < len(a) && it.next < it.end {
		block, meta, ok := it.nextBlock()
		if !ok {
			return nil
		}
		if len(block) == 0 {
			continue
		}
		for cursor < len(a) && a[cursor] < meta.minID {
			cursor++
		}
		if cursor >= len(a) {
			break
		}
		if a[cursor] > meta.maxID {
			continue
		}
		j := 0
		for cursor < len(a) && j < len(block) {
			av := a[cursor]
			if av > meta.maxID {
				break
			}
			bv := block[j]
			switch {
			case av == bv:
				out = append(out, av)
				cursor++
				j++
			case av < bv:
				cursor++
			default:
				j++
			}
		}
		for cursor < len(a) && a[cursor] <= meta.maxID {
			cursor++
		}
	}
	return out
}

func sortUint32s(values []uint32) {
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
}

func uniqueSortedInts(in []int) []int {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	last := in[0]
	for _, v := range in[1:] {
		if v == last {
			continue
		}
		out = append(out, v)
		last = v
	}
	return out
}

func uniqueSortedUint32s(in []uint32) []uint32 {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	last := in[0]
	for _, v := range in[1:] {
		if v == last {
			continue
		}
		out = append(out, v)
		last = v
	}
	return out
}

func uint32sToInts(in []uint32) []int {
	out := make([]int, len(in))
	for i, v := range in {
		out[i] = int(v)
	}
	return out
}

func mapKeys(m map[int]struct{}) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func (idx *Index) compactPathContainsAll(i int, terms []string) bool {
	for _, term := range terms {
		if !idx.compactPathContainsTerm(i, term) {
			return false
		}
	}
	return true
}

func parseQuery(opts queryOptions) (parsedQuery, error) {
	pq := parsedQuery{
		Raw:           opts.Query,
		MatchPath:     opts.MatchPath || queryLooksPathScoped(opts.Query),
		CaseSensitive: opts.CaseSensitive,
		Under:         normalizeFilterPath(opts.Under),
		Exists:        opts.Exists,
		CWDBias:       normalizeFilterPath(opts.CWDBias),
		RootBias:      normalizeFilterPath(opts.RootBias),
		DeadlineUnix:  opts.DeadlineUnix,
		Cancel:        opts.Cancel,
		Trace:         opts.Trace,
	}
	if opts.ModifiedAfter != "" {
		t, err := parseTimeValue(opts.ModifiedAfter)
		if err != nil {
			return pq, err
		}
		pq.ModifiedAfter = t
		pq.HasModAfter = true
	}
	if opts.Recent != "" {
		d, err := time.ParseDuration(opts.Recent)
		if err != nil {
			return pq, fmt.Errorf("invalid --recent duration: %w", err)
		}
		pq.ModifiedAfter = time.Now().Add(-d)
		pq.HasModAfter = true
	}
	for _, raw := range strings.Fields(opts.Query) {
		if implicitPathSeparatorToken(raw) {
			pq.ImplicitPathTerms = append(pq.ImplicitPathTerms, queryPlainTerms(raw, pq.CaseSensitive, true)...)
		}
		if err := applyQueryToken(&pq, raw); err != nil {
			return pq, err
		}
	}
	promotePathExtensionTerms(&pq)
	if pq.isEmpty() {
		return pq, errors.New("query has no searchable terms or filters")
	}
	return pq, nil
}

func implicitPathSeparatorToken(raw string) bool {
	if raw == "" || strings.HasPrefix(raw, "!") || strings.HasPrefix(raw, "-") ||
		strings.Contains(raw, "|") || !strings.ContainsAny(raw, `\/`) {
		return false
	}
	colon := strings.IndexByte(raw, ':')
	return colon < 0 || (colon == 1 && ((raw[0] >= 'a' && raw[0] <= 'z') || (raw[0] >= 'A' && raw[0] <= 'Z')))
}

func promotePathExtensionTerms(pq *parsedQuery) {
	if pq == nil || (!pq.MatchPath && len(pq.Dirs) == 0 && pq.Under == "") {
		return
	}
	terms := pq.Terms[:0]
	for _, term := range pq.Terms {
		if ext, ok := dottedExtensionTerm(term); ok {
			pq.Exts = append(pq.Exts, ext)
			continue
		}
		terms = append(terms, term)
	}
	pq.Terms = terms
	promotePathBareExtensionTerms(pq)
}

func promotePathBareExtensionTerms(pq *parsedQuery) {
	if pq == nil || (!pq.MatchPath && len(pq.Dirs) == 0 && pq.Under == "") {
		return
	}
	hasPathAnchor := pq.Under != "" || len(pq.Dirs) > 0
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) || commonPathBareExtensionTerm(term) || strings.ContainsAny(term, `\/*?[]:.`) {
			continue
		}
		if len(term) >= 3 {
			hasPathAnchor = true
			break
		}
	}
	if !hasPathAnchor {
		return
	}
	terms := pq.Terms[:0]
	for _, term := range pq.Terms {
		if commonPathBareExtensionTerm(term) {
			pq.Exts = append(pq.Exts, term)
			continue
		}
		terms = append(terms, term)
	}
	pq.Terms = terms
}

func commonPathBareExtensionTerm(term string) bool {
	switch strings.ToLower(term) {
	case "md", "nrrd", "raw", "pdf", "json", "go", "py", "txt", "csv", "tsv",
		"doc", "docx", "xls", "xlsx", "ppt", "pptx", "png", "jpg", "jpeg",
		"zip", "whl", "toml", "yaml", "yml", "xml", "html", "css", "js", "ts":
		return true
	default:
		return false
	}
}

func queryLooksPathScoped(query string) bool {
	for _, field := range strings.Fields(query) {
		if strings.ContainsAny(field, `\/`) {
			return true
		}
	}
	return false
}

func queryLooksLoosePathScoped(query string) bool {
	fields := strings.Fields(query)
	plain := 0
	for _, field := range fields {
		if strings.HasPrefix(field, "!") || strings.HasPrefix(field, "-") {
			continue
		}
		raw := strings.TrimLeft(field, "!-")
		if raw == "" {
			continue
		}
		key, value, hasPrefix := strings.Cut(raw, ":")
		if hasPrefix {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "path", "fullpath", "full-path", "full_path", "fullpathname", "full-path-name", "location":
				return true
			case "regex", "regexp", "re":
				// A regex is matched against the full path, so it is inherently
				// path-scoped.  Treat a bare regex: query as path-scoped so the
				// regex-literal planner can serve it instead of declining to an
				// exhaustive scan.
				return true
			case "ext", "extension", "glob", "size", "sz", "dm", "date", "date-modified", "datemodified", "modified", "type", "case", "attrib", "sort":
				continue
			}
			if value != "" {
				plain++
			}
			continue
		}
		plain++
	}
	return plain >= 2
}

func dottedExtensionTerm(term string) (string, bool) {
	// Bare dotted tokens are only an extension shorthand for common short
	// extensions. Longer dotted strings, such as ".opencode", are ordinary
	// substrings unless the user explicitly writes ext:opencode.
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

// isEmpty reports whether a parsed query carries no searchable constraints.
func (pq parsedQuery) isEmpty() bool {
	return len(pq.Terms) == 0 && len(pq.Exts) == 0 && len(pq.Dirs) == 0 &&
		len(pq.Globs) == 0 && len(pq.Regexps) == 0 && pq.Type == "" &&
		len(pq.Parents) == 0 && pq.Under == "" && !pq.HasModAfter && len(pq.SizeFilters) == 0 &&
		len(pq.DateFilters) == 0 && len(pq.AttrFilters) == 0 && len(pq.OrGroups) == 0 && len(pq.NotGroups) == 0
}

// applyQueryToken parses a single whitespace-delimited token and folds it into
// pq. It handles OR groups (a|b), negation (!term or -term), structured filters
// (ext:, dir:, glob:, regex:, type:, case:, size:, dm:), and plain terms.
// Unknown "name:" style prefixes are rejected so the tool never silently
// degrades an unsupported filter into a literal substring match.
func applyQueryToken(pq *parsedQuery, raw string) error {
	if raw == "" {
		return nil
	}

	// Negation: !term or -term excludes records matching the inner token.
	if (strings.HasPrefix(raw, "!") || strings.HasPrefix(raw, "-")) && len(raw) > 1 {
		inner := raw[1:]
		sub := parsedQuery{MatchPath: pq.MatchPath, CaseSensitive: pq.CaseSensitive}
		if err := applyQueryToken(&sub, inner); err != nil {
			return err
		}
		if !sub.isEmpty() {
			pq.NotGroups = append(pq.NotGroups, sub)
		}
		return nil
	}

	// OR group: a|b|c. Each alternative is parsed as its own subquery; a record
	// matches the group if it matches any alternative. We only treat '|' as an
	// operator when it joins token-like alternatives (not inside a regex, which
	// uses the regex: prefix and is handled before this point).
	if strings.Contains(raw, "|") && !strings.HasPrefix(raw, "regex:") {
		parts := strings.Split(raw, "|")
		group := make([]parsedQuery, 0, len(parts))
		for _, part := range parts {
			if part == "" {
				continue
			}
			sub := parsedQuery{MatchPath: pq.MatchPath, CaseSensitive: pq.CaseSensitive}
			if err := applyQueryToken(&sub, part); err != nil {
				return err
			}
			if !sub.isEmpty() {
				if sub.MatchPath {
					pq.MatchPath = true
				}
				group = append(group, sub)
			}
		}
		if len(group) == 1 {
			// A degenerate "a|" collapses to a plain token.
			mergeSubquery(pq, group[0])
		} else if len(group) > 1 {
			pq.OrGroups = append(pq.OrGroups, group)
		}
		return nil
	}

	switch {
	case strings.HasPrefix(raw, "ext:"):
		ext := strings.TrimPrefix(raw, "ext:")
		ext = strings.TrimPrefix(ext, ".")
		if ext != "" {
			pq.Exts = append(pq.Exts, normalizeCase(ext, pq.CaseSensitive))
		}
	case strings.HasPrefix(raw, "dir:"):
		dir := strings.TrimPrefix(raw, "dir:")
		if dir != "" {
			pq.Dirs = append(pq.Dirs, normalizeCase(dir, pq.CaseSensitive))
		}
	case strings.HasPrefix(raw, "path:"):
		term := strings.TrimPrefix(raw, "path:")
		pq.MatchPath = true
		if term != "" {
			if isDriveRelativePathTerm(term) {
				// The documented drive-scoped form is `path:C: term`.
				// A fused `path:C:.nrrd` is a drive-relative literal, not
				// an extension filter; indexed seekfs paths are absolute and
				// cannot contain this colon form.  Mark it impossible so the
				// strict parser semantics do not trigger a full-volume scan.
				pq.Impossible = true
			}
			for _, part := range queryPlainTerms(term, pq.CaseSensitive, true) {
				pq.Terms = append(pq.Terms, part)
			}
		}
	case strings.HasPrefix(raw, "parent:"):
		parent := strings.TrimPrefix(raw, "parent:")
		if parent != "" {
			if strings.ContainsAny(parent, `\/:*?[]`) {
				return fmt.Errorf("invalid parent filter %q; parent: matches one directory name, not a path or glob", raw)
			}
			pq.Parents = append(pq.Parents, normalizeCase(parent, pq.CaseSensitive))
		}
	case strings.HasPrefix(raw, "glob:"):
		glob := strings.TrimPrefix(raw, "glob:")
		if glob != "" {
			pq.Globs = append(pq.Globs, normalizeCase(glob, pq.CaseSensitive))
		}
	case strings.HasPrefix(raw, "regex:"):
		pat := strings.TrimPrefix(raw, "regex:")
		if pat == "" {
			return nil
		}
		pq.RegexTerms = appendRegexLiteralTerms(pq.RegexTerms, pat, pq.CaseSensitive)
		if !pq.CaseSensitive {
			pat = "(?i)" + pat
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return fmt.Errorf("invalid regex %q: %w", pat, err)
		}
		pq.Regexps = append(pq.Regexps, re)
	case strings.HasPrefix(raw, "size:"):
		sf, err := parseSizeFilter(strings.TrimPrefix(raw, "size:"))
		if err != nil {
			return err
		}
		pq.SizeFilters = append(pq.SizeFilters, sf)
	case strings.HasPrefix(raw, "dm:"):
		df, err := parseDateFilter(strings.TrimPrefix(raw, "dm:"))
		if err != nil {
			return err
		}
		pq.DateFilters = append(pq.DateFilters, df)
	case strings.HasPrefix(raw, "attrib:"):
		mask, err := parseAttribFilter(strings.TrimPrefix(raw, "attrib:"))
		if err != nil {
			return err
		}
		pq.AttrFilters = append(pq.AttrFilters, mask)
	case strings.HasPrefix(raw, "sort:"):
		sortColumn := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(raw, "sort:")))
		switch sortColumn {
		case "size", "modified", "extension", "type", "path":
			pq.SortColumn = sortColumn
		default:
			return fmt.Errorf("unsupported sort %q; supported: sort:size, sort:modified, sort:extension, sort:type, sort:path", raw)
		}
	case raw == "case:" || raw == "case:true":
		pq.CaseSensitive = true
	case raw == "case:false":
		pq.CaseSensitive = false
	case raw == "type:file" || raw == "type:dir":
		pq.Type = strings.TrimPrefix(raw, "type:")
	case isUnknownFilterToken(raw):
		return fmt.Errorf("unsupported filter %q; supported: path: parent: ext: dir: glob: regex: type: case: size: dm: attrib: sort:size sort:modified sort:extension sort:type sort:path (and !term, a|b)", raw)
	case looksLikeImplicitFilenameGlob(raw):
		pq.Globs = append(pq.Globs, normalizeCase(raw, pq.CaseSensitive))
	default:
		for _, term := range queryPlainTerms(raw, pq.CaseSensitive, pq.MatchPath) {
			pq.Terms = append(pq.Terms, term)
		}
	}
	return nil
}

func isDriveRelativePathTerm(term string) bool {
	if len(term) < 3 || term[1] != ':' {
		return false
	}
	c := term[0]
	if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
		return false
	}
	return term[2] != '\\' && term[2] != '/'
}

// mergeSubquery folds the constraints of src into dst. Used when an OR group
// collapses to a single alternative.
func mergeSubquery(dst *parsedQuery, src parsedQuery) {
	dst.Terms = append(dst.Terms, src.Terms...)
	dst.Exts = append(dst.Exts, src.Exts...)
	dst.Dirs = append(dst.Dirs, src.Dirs...)
	dst.Globs = append(dst.Globs, src.Globs...)
	dst.Regexps = append(dst.Regexps, src.Regexps...)
	dst.RegexTerms = append(dst.RegexTerms, src.RegexTerms...)
	dst.Parents = append(dst.Parents, src.Parents...)
	dst.SizeFilters = append(dst.SizeFilters, src.SizeFilters...)
	dst.DateFilters = append(dst.DateFilters, src.DateFilters...)
	dst.AttrFilters = append(dst.AttrFilters, src.AttrFilters...)
	if src.Type != "" {
		dst.Type = src.Type
	}
	if src.MatchPath {
		dst.MatchPath = true
	}
}

// isUnknownFilterToken reports whether raw looks like an unsupported "name:"
// filter prefix (a short alphabetic prefix followed by ':') rather than a plain
// term that merely contains a colon (e.g. a Windows drive path "c:\foo").
func isUnknownFilterToken(raw string) bool {
	idx := strings.IndexByte(raw, ':')
	if idx <= 0 {
		return false
	}
	prefix := raw[:idx]
	// Windows drive letters ("c", "d") are single-char; treat single-char
	// prefixes as path-like, not filters.
	if len(prefix) < 2 {
		return false
	}
	// A filter prefix starts with a letter and is otherwise alphanumeric
	// (e.g. "size2", "attrib"). Anything else (digits-first, punctuation) is a
	// plain term that merely contains a colon.
	for i, r := range prefix {
		isLetter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		isDigit := r >= '0' && r <= '9'
		if i == 0 && !isLetter {
			return false
		}
		if !isLetter && !isDigit {
			return false
		}
	}
	return true
}

func queryPlainTerms(raw string, caseSensitive, matchPath bool) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if !matchPath || !strings.ContainsAny(raw, `\/`) {
		return []string{normalizeCase(raw, caseSensitive)}
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == '\\' || r == '/' })
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || part == "." {
			continue
		}
		out = append(out, normalizeCase(part, caseSensitive))
	}
	if len(out) == 0 {
		return []string{normalizeCase(raw, caseSensitive)}
	}
	return out
}

func looksLikeImplicitFilenameGlob(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, `\/`) {
		return false
	}
	return strings.ContainsAny(raw, "*?[")
}

func globLiteralTerms(globs []string, caseSensitive bool) []string {
	seen := make(map[string]struct{}, len(globs))
	out := make([]string, 0, len(globs))
	for _, glob := range globs {
		for _, term := range splitGlobLiteralTerms(glob, caseSensitive) {
			if _, ok := seen[term]; ok {
				continue
			}
			seen[term] = struct{}{}
			out = append(out, term)
		}
	}
	return out
}

func complexGlobExts(globs []string) []string {
	seen := make(map[string]struct{}, len(globs))
	out := make([]string, 0, len(globs))
	for _, glob := range globs {
		ext := strings.TrimPrefix(filepath.Ext(glob), ".")
		if ext == "" || strings.ContainsAny(ext, `\/*?[]:`) {
			continue
		}
		ext = strings.ToLower(ext)
		if _, ok := seen[ext]; ok {
			continue
		}
		seen[ext] = struct{}{}
		out = append(out, ext)
	}
	return out
}

func splitGlobLiteralTerms(glob string, caseSensitive bool) []string {
	var b strings.Builder
	out := make([]string, 0, 2)
	flush := func() {
		if b.Len() >= 3 {
			out = append(out, normalizeCase(b.String(), caseSensitive))
		}
		b.Reset()
	}
	for _, r := range glob {
		switch r {
		case '*', '?', '[', ']', '\\', '/', ':':
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return out
}

func appendRegexLiteralTerms(out []string, pattern string, caseSensitive bool) []string {
	var b strings.Builder
	flush := func() {
		// Two-character runs are kept too: dropping them can hide an
		// alternation such as `.*\.(md|txt)$`, whose only >=3 run ("txt")
		// would then look like a required literal even though "md" matches
		// the regex without it.  Keeping short runs makes the planner treat
		// the query as ambiguous and decline to the exhaustive scan.
		if b.Len() >= 2 {
			out = append(out, normalizeCase(b.String(), caseSensitive))
		}
		b.Reset()
	}
	escaped := false
	for _, r := range pattern {
		if escaped {
			if isRegexLiteralRune(r) {
				b.WriteRune(r)
			} else {
				flush()
			}
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if isRegexLiteralRune(r) {
			b.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

func isRegexLiteralRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
}

func entryMatches(entry Entry, pq parsedQuery, matchPath bool) bool {
	path := filepath.Clean(entry.Path)
	name := entry.Name
	if name == "" {
		name = filepath.Base(path)
	}
	cmpPath := normalizeCase(path, pq.CaseSensitive)
	cmpName := normalizeCase(name, pq.CaseSensitive)
	haystack := cmpName
	if matchPath {
		haystack = cmpPath
	}
	if pq.Under != "" && !pathUnder(path, pq.Under) {
		return false
	}
	if pq.Exists {
		if _, err := os.Stat(path); err != nil {
			return false
		}
	}
	if pq.HasModAfter {
		if entry.ModUnix == 0 || !time.Unix(0, entry.ModUnix).After(pq.ModifiedAfter) {
			return false
		}
	}
	if pq.Type == "file" && entry.Mode&uint32(os.ModeDir) != 0 {
		return false
	}
	if pq.Type == "dir" && entry.Mode&uint32(os.ModeDir) == 0 {
		return false
	}
	if len(pq.Parents) > 0 {
		parentName := normalizeCase(filepath.Base(filepath.Dir(path)), pq.CaseSensitive)
		for _, parent := range pq.Parents {
			if parentName != parent {
				return false
			}
		}
	}
	if !containsAll(haystack, pq.Terms) {
		return false
	}
	for _, ext := range pq.Exts {
		actual := strings.TrimPrefix(filepath.Ext(name), ".")
		if normalizeCase(actual, pq.CaseSensitive) != ext {
			return false
		}
	}
	for _, dir := range pq.Dirs {
		if !strings.Contains(cmpPath, dir) {
			return false
		}
	}
	for _, glob := range pq.Globs {
		ok, err := filepath.Match(glob, cmpName)
		if err != nil || !ok {
			return false
		}
	}
	for _, re := range pq.Regexps {
		if !re.MatchString(path) {
			return false
		}
	}
	for _, sf := range pq.SizeFilters {
		if !sf.matches(entry.Size) {
			return false
		}
	}
	for _, df := range pq.DateFilters {
		if !df.matches(entry.ModUnix) {
			return false
		}
	}
	if !attrFiltersMatch(entry.Mode, pq.AttrFilters) {
		return false
	}
	for _, group := range pq.OrGroups {
		matched := false
		for _, alt := range group {
			if entryMatches(entry, alt, matchPath || alt.MatchPath) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, neg := range pq.NotGroups {
		if entryMatches(entry, neg, matchPath || neg.MatchPath) {
			return false
		}
	}
	return true
}

func normalizedLimit(limit int, countOnly bool) int {
	if limit <= 0 && !countOnly {
		return 100
	}
	if limit <= 0 && countOnly {
		return 0
	}
	return limit
}

func normalizeCase(s string, caseSensitive bool) string {
	if caseSensitive {
		return s
	}
	return strings.ToLower(s)
}

func normalizeFilterPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}

func pathUnder(path, root string) bool {
	path = normalizeFilterPath(path)
	root = normalizeFilterPath(root)
	if strings.EqualFold(path, root) {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != "" && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

func parseTimeValue(value string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", value); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid time %q; use RFC3339 or YYYY-MM-DD", value)
}

// parseSizeFilter parses an Everything-style size constraint such as ">100mb",
// ">=1gb", "<4k", or "1024". A bare number is treated as an exact match.
func parseSizeFilter(spec string) (sizeFilter, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return sizeFilter{}, errors.New("empty size: filter")
	}
	op := "="
	switch {
	case strings.HasPrefix(spec, ">="):
		op, spec = ">=", spec[2:]
	case strings.HasPrefix(spec, "<="):
		op, spec = "<=", spec[2:]
	case strings.HasPrefix(spec, ">"):
		op, spec = ">", spec[1:]
	case strings.HasPrefix(spec, "<"):
		op, spec = "<", spec[1:]
	case strings.HasPrefix(spec, "="):
		op, spec = "=", spec[1:]
	}
	bytes, err := parseByteSize(strings.TrimSpace(spec))
	if err != nil {
		return sizeFilter{}, fmt.Errorf("invalid size: filter %q: %w", spec, err)
	}
	return sizeFilter{op: op, bytes: bytes}, nil
}

// parseByteSize parses a number with an optional unit suffix (b, kb, mb, gb,
// tb; the trailing 'b' is optional, e.g. "100mb" or "100m"). Units are 1024-based.
func parseByteSize(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, errors.New("empty size")
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "tb"):
		mult, s = 1<<40, s[:len(s)-2]
	case strings.HasSuffix(s, "gb"):
		mult, s = 1<<30, s[:len(s)-2]
	case strings.HasSuffix(s, "mb"):
		mult, s = 1<<20, s[:len(s)-2]
	case strings.HasSuffix(s, "kb"):
		mult, s = 1<<10, s[:len(s)-2]
	case strings.HasSuffix(s, "t"):
		mult, s = 1<<40, s[:len(s)-1]
	case strings.HasSuffix(s, "g"):
		mult, s = 1<<30, s[:len(s)-1]
	case strings.HasSuffix(s, "m"):
		mult, s = 1<<20, s[:len(s)-1]
	case strings.HasSuffix(s, "k"):
		mult, s = 1<<10, s[:len(s)-1]
	case strings.HasSuffix(s, "b"):
		mult, s = 1, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("not a valid byte count")
	}
	return n * mult, nil
}

func parseAttribFilter(spec string) (uint32, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0, errors.New("empty attrib: filter")
	}
	var mask uint32
	for _, r := range spec {
		switch r {
		case 'r', 'R':
			mask |= fileAttributeReadonly
		case 'h', 'H':
			mask |= fileAttributeHidden
		case 's', 'S':
			mask |= fileAttributeSystem
		case 'd', 'D':
			mask |= fileAttributeDir
		case 'a', 'A':
			mask |= fileAttributeArchive
		default:
			return 0, fmt.Errorf("invalid attrib: flag %q; supported flags are R,H,S,D,A", r)
		}
	}
	return mask, nil
}

// parseDateFilter parses an Everything-style dm: constraint. Supported specs:
// "today", "yesterday", "thisweek", "lastweek", a relative duration like "24h"
// or "7d", or an absolute date (YYYY-MM-DD) / RFC3339 timestamp meaning
// "modified on or after".
func parseDateFilter(spec string) (dateFilter, error) {
	spec = strings.ToLower(strings.TrimSpace(spec))
	if spec == "" {
		return dateFilter{}, errors.New("empty dm: filter")
	}
	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch spec {
	case "today":
		return dateFilter{after: startOfDay, before: startOfDay.AddDate(0, 0, 1)}, nil
	case "yesterday":
		return dateFilter{after: startOfDay.AddDate(0, 0, -1), before: startOfDay}, nil
	case "thisweek":
		weekday := int(startOfDay.Weekday())
		weekStart := startOfDay.AddDate(0, 0, -weekday)
		return dateFilter{after: weekStart, before: weekStart.AddDate(0, 0, 7)}, nil
	case "lastweek":
		weekday := int(startOfDay.Weekday())
		weekStart := startOfDay.AddDate(0, 0, -weekday-7)
		return dateFilter{after: weekStart, before: weekStart.AddDate(0, 0, 7)}, nil
	}
	// Relative duration: support a "d" (days) suffix in addition to Go durations.
	if strings.HasSuffix(spec, "d") {
		if days, err := strconv.Atoi(strings.TrimSuffix(spec, "d")); err == nil {
			return dateFilter{after: now.AddDate(0, 0, -days)}, nil
		}
	}
	if d, err := time.ParseDuration(spec); err == nil {
		return dateFilter{after: now.Add(-d)}, nil
	}
	if t, err := parseTimeValue(spec); err == nil {
		return dateFilter{after: t}, nil
	}
	return dateFilter{}, fmt.Errorf("invalid dm: filter %q; use today|yesterday|thisweek|lastweek, a duration (24h, 7d), or a date", spec)
}

// matchesSize reports whether size satisfies the filter.
func (sf sizeFilter) matches(size int64) bool {
	switch sf.op {
	case ">":
		return size > sf.bytes
	case ">=":
		return size >= sf.bytes
	case "<":
		return size < sf.bytes
	case "<=":
		return size <= sf.bytes
	default:
		return size == sf.bytes
	}
}

// matches reports whether the modification time (unix nanoseconds) satisfies the
// filter. A zero before bound means "no upper bound".
func (df dateFilter) matches(modUnixNanos int64) bool {
	if modUnixNanos == 0 {
		return false
	}
	t := time.Unix(0, modUnixNanos)
	if !df.after.IsZero() && t.Before(df.after) {
		return false
	}
	if !df.before.IsZero() && !t.Before(df.before) {
		return false
	}
	return true
}

func attrFiltersMatch(mode uint32, filters []uint32) bool {
	for _, mask := range filters {
		if mode&mask != mask {
			return false
		}
	}
	return true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func biasOrderEntries(idx *Index, order []int, root string) []int {
	if root == "" || len(order) == 0 {
		return order
	}
	out := append([]int(nil), order...)
	sort.SliceStable(out, func(i, j int) bool {
		a := pathUnder(idx.Entries[out[i]].Path, root)
		b := pathUnder(idx.Entries[out[j]].Path, root)
		return a && !b
	})
	return out
}

func (idx *Index) biasOrderCompact(order []int, root string) []int {
	if root == "" || len(order) == 0 {
		return order
	}
	cache := make(map[int]string)
	out := append([]int(nil), order...)
	sort.SliceStable(out, func(i, j int) bool {
		a := pathUnder(idx.reconstructCompactPathCached(out[i], cache), root)
		b := pathUnder(idx.reconstructCompactPathCached(out[j], cache), root)
		return a && !b
	})
	return out
}

func (idx *Index) compactPathContainsTerm(i int, term string) bool {
	if idx.Volume != "" && containsFoldASCII(idx.Volume, term) {
		return true
	}
	// Walk the parent chain without a per-call cycle-detection map: this is on
	// the broad-scan hot path (called per term per record across tens of millions
	// of records), and the depth cap already bounds a malformed chain. A cycle
	// simply exhausts the cap and returns false, the same result the map gave.
	cur := i
	for depth := 0; depth < 1024; depth++ {
		if cur < 0 || cur >= idx.compactRecordCount() {
			return false
		}
		rec := idx.compactRecord(cur)
		if containsFoldASCII(idx.compactNameAt(cur), term) {
			return true
		}
		if rec.Parent < 0 || int(rec.Parent) == cur {
			return false
		}
		cur = int(rec.Parent)
	}
	return false
}

func (idx *Index) reconstructCompactPath(i int) string {
	return idx.reconstructCompactPathCached(i, make(map[int]string))
}

func (idx *Index) reconstructCompactPathCached(i int, cache map[int]string) string {
	if path, ok := cache[i]; ok {
		return path
	}
	if i < 0 || i >= idx.compactRecordCount() {
		return ""
	}
	parts := make([]string, 0, 16)
	seen := make(map[int]struct{}, 16)
	cur := i
	for depth := 0; depth < 1024; depth++ {
		if path, ok := cache[cur]; ok {
			for p := len(parts) - 1; p >= 0; p-- {
				path += `\` + parts[p]
			}
			cache[i] = path
			return path
		}
		if cur < 0 || cur >= idx.compactRecordCount() {
			break
		}
		if _, ok := seen[cur]; ok {
			break
		}
		seen[cur] = struct{}{}
		rec := idx.compactRecord(cur)
		if rec.Name != "." {
			parts = append(parts, rec.Name)
		}
		if rec.Parent < 0 {
			break
		}
		cur = int(rec.Parent)
	}
	root := idx.Volume
	if root == "" && len(idx.Roots) > 0 {
		root = strings.TrimRight(idx.Roots[0], `\`)
	}
	path := root
	for p := len(parts) - 1; p >= 0; p-- {
		if path == "" {
			path = parts[p]
		} else {
			path += `\` + parts[p]
		}
	}
	cache[i] = path
	return path
}

func containsAll(s string, terms []string) bool {
	for _, term := range terms {
		if !strings.Contains(s, term) {
			return false
		}
	}
	return true
}

func containsFoldASCII(s, term string) bool {
	if term == "" {
		return true
	}
	if len(term) > len(s) {
		return false
	}
	first := foldASCII(term[0])
	last := len(s) - len(term)
	for i := 0; i <= last; i++ {
		if foldASCII(s[i]) != first {
			continue
		}
		matched := true
		for j := 1; j < len(term); j++ {
			if foldASCII(s[i+j]) != foldASCII(term[j]) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func sameUint32Slice(a, b []uint32) bool {
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

type diskHeader struct {
	Magic       [8]byte
	Version     uint32
	EntryCount  uint64
	RootCount   uint64
	BuiltUnix   int64
	JournalID   uint64
	Checkpoint  int64
	Compact     uint32
	NameBlobLen uint64
	TokenCount  uint64
}

func writeIndex(w io.Writer, idx *Index) error {
	var nameOffs []uint32
	var nameLens []uint16
	var nameIDForRecord []uint32
	recordCount := idx.compactRecordCount()
	header := diskHeader{
		Magic:      indexMagic,
		Version:    indexVersion,
		EntryCount: uint64(len(idx.Entries)),
		RootCount:  uint64(len(idx.Roots)),
		BuiltUnix:  idx.BuiltAt.UnixNano(),
		JournalID:  idx.JournalID,
		Checkpoint: idx.Checkpoint,
	}
	if idx.Compact {
		header.EntryCount = uint64(recordCount)
		nameIDs := make(map[string]uint32, recordCount/2)
		nameBlob := make([]byte, 0, recordCount*16)
		nameOffs = make([]uint32, 0, recordCount)
		nameLens = make([]uint16, 0, recordCount)
		nameIDForRecord = make([]uint32, recordCount)
		for i := 0; i < recordCount; i++ {
			rec := idx.compactRecord(i)
			if len(rec.Name) > int(^uint16(0)) {
				return errors.New("compact name too large")
			}
			id, ok := nameIDs[rec.Name]
			if !ok {
				id = uint32(len(nameOffs))
				nameIDs[rec.Name] = id
				nameOffs = append(nameOffs, uint32(len(nameBlob)))
				nameLens = append(nameLens, uint16(len(rec.Name)))
				nameBlob = append(nameBlob, rec.Name...)
			}
			nameIDForRecord[i] = id
		}
		idx.NameBlob = nameBlob
		header.NameBlobLen = uint64(len(nameBlob))
		header.TokenCount = uint64(len(nameOffs))
		header.Compact = compactDiskFlag
		if compactNeedsWideDiskRecords(recordCount, len(nameOffs)) {
			header.Compact |= compactDiskWideRefsFlag
		}
		if idx.CompactAttrs {
			header.Compact |= compactDiskAttrsFlag
		}
	}
	if err := binary.Write(w, binary.LittleEndian, header); err != nil {
		return err
	}
	for _, s := range []string{idx.Source, idx.Volume, idx.ContentHash} {
		if err := writeString(w, s); err != nil {
			return err
		}
	}
	for _, root := range idx.Roots {
		if err := writeString(w, root); err != nil {
			return err
		}
	}
	if idx.Compact {
		wideRefs := header.Compact&compactDiskWideRefsFlag != 0
		if _, err := w.Write(idx.NameBlob); err != nil {
			return err
		}
		for i := range nameOffs {
			if err := binary.Write(w, binary.LittleEndian, nameOffs[i]); err != nil {
				return err
			}
			if err := binary.Write(w, binary.LittleEndian, nameLens[i]); err != nil {
				return err
			}
		}
		for i := 0; i < recordCount; i++ {
			rec := idx.compactRecord(i)
			rec.NameOff = nameIDForRecord[i]
			parent := uint32(compactNarrowParentSentinel)
			if wideRefs {
				parent = compactWideParentSentinel
			}
			if rec.Parent >= 0 {
				if !wideRefs && uint32(rec.Parent) >= compactNarrowParentSentinel {
					return errors.New("compact index too large for packed record format")
				}
				parent = uint32(rec.Parent)
			}
			if err := binary.Write(w, binary.LittleEndian, rec.FRN); err != nil {
				return err
			}
			if err := binary.Write(w, binary.LittleEndian, rec.ParentFRN); err != nil {
				return err
			}
			if err := writeCompactRecordRefs(w, parent, rec.NameOff, wideRefs); err != nil {
				return err
			}
			if err := binary.Write(w, binary.LittleEndian, rec.Mode); err != nil {
				return err
			}
			if err := binary.Write(w, binary.LittleEndian, rec.Size); err != nil {
				return err
			}
			if err := binary.Write(w, binary.LittleEndian, rec.ModUnix); err != nil {
				return err
			}
			deleted := uint8(0)
			if rec.Deleted {
				deleted = 1
			}
			if err := binary.Write(w, binary.LittleEndian, deleted); err != nil {
				return err
			}
		}
		return nil
	}
	for _, entry := range idx.Entries {
		for _, s := range []string{entry.Path, entry.Name, entry.LowerPath, entry.LowerName} {
			if err := writeString(w, s); err != nil {
				return err
			}
		}
		for _, v := range []any{entry.Size, entry.Mode, entry.ModUnix} {
			if err := binary.Write(w, binary.LittleEndian, v); err != nil {
				return err
			}
		}
	}
	if err := writeUint32Slice(w, idx.NameOrder); err != nil {
		return err
	}
	return writeUint32Slice(w, idx.PathOrder)
}

func readIndex(r io.Reader) (*Index, error) {
	return readIndexWithReaderAt(r, nil, 0)
}

func readIndexWithReaderAt(r io.Reader, ra io.ReaderAt, size int64) (*Index, error) {
	var header diskHeader
	if err := binary.Read(r, binary.LittleEndian, &header); err != nil {
		return nil, err
	}
	sectionTableOffset := uint64(0)
	if header.Magic == indexMagicV9 {
		if err := binary.Read(r, binary.LittleEndian, &sectionTableOffset); err != nil {
			return nil, err
		}
	} else if header.Magic != indexMagic {
		return nil, errors.New("unsupported index format")
	}
	if header.Version != indexVersion && header.Version != indexVersionV9 {
		return nil, fmt.Errorf("unsupported index version %d", header.Version)
	}
	if header.EntryCount > uint64(^uint(0)>>1) || header.RootCount > uint64(^uint(0)>>1) {
		return nil, errors.New("index too large")
	}
	idx := &Index{
		Version:    int(header.Version),
		BuiltAt:    time.Unix(0, header.BuiltUnix),
		Roots:      make([]string, int(header.RootCount)),
		JournalID:  header.JournalID,
		Checkpoint: header.Checkpoint,
		Compact:    header.Compact != 0,
	}
	var err error
	if idx.Source, err = readString(r); err != nil {
		return nil, err
	}
	if idx.Volume, err = readString(r); err != nil {
		return nil, err
	}
	if idx.ContentHash, err = readString(r); err != nil {
		return nil, err
	}
	for i := range idx.Roots {
		if idx.Roots[i], err = readString(r); err != nil {
			return nil, err
		}
	}
	if idx.Compact {
		wideRefs := header.Compact&compactDiskWideRefsFlag != 0
		idx.CompactAttrs = header.Compact&compactDiskAttrsFlag != 0
		if header.NameBlobLen > uint64(^uint(0)>>1) {
			return nil, errors.New("name blob too large")
		}
		idx.NameBlob = make([]byte, int(header.NameBlobLen))
		if _, err := io.ReadFull(r, idx.NameBlob); err != nil {
			return nil, err
		}
		if header.TokenCount > uint64(^uint(0)>>1) {
			return nil, errors.New("name table too large")
		}
		nameOffs := make([]uint32, int(header.TokenCount))
		nameLens := make([]uint16, int(header.TokenCount))
		for i := range nameOffs {
			if err := binary.Read(r, binary.LittleEndian, &nameOffs[i]); err != nil {
				return nil, err
			}
			if err := binary.Read(r, binary.LittleEndian, &nameLens[i]); err != nil {
				return nil, err
			}
		}
		idx.Records = make([]CompactRecord, int(header.EntryCount))
		for i := range idx.Records {
			rec := &idx.Records[i]
			if err := binary.Read(r, binary.LittleEndian, &rec.FRN); err != nil {
				return nil, err
			}
			if err := binary.Read(r, binary.LittleEndian, &rec.ParentFRN); err != nil {
				return nil, err
			}
			parent, nameOff, err := readCompactRecordRefs(r, wideRefs)
			if err != nil {
				return nil, err
			}
			if (!wideRefs && parent == compactNarrowParentSentinel) || (wideRefs && parent == compactWideParentSentinel) {
				rec.Parent = -1
			} else {
				rec.Parent = int32(parent)
			}
			rec.NameOff = nameOff
			if int(rec.NameOff) >= len(nameOffs) {
				return nil, errors.New("invalid compact name id")
			}
			off := nameOffs[rec.NameOff]
			length := nameLens[rec.NameOff]
			end := int(off) + int(length)
			if end < int(off) || end > len(idx.NameBlob) {
				return nil, errors.New("invalid compact name reference")
			}
			rec.Name = stringView(idx.NameBlob[int(off):end])
			if err := binary.Read(r, binary.LittleEndian, &rec.Mode); err != nil {
				return nil, err
			}
			if err := binary.Read(r, binary.LittleEndian, &rec.Size); err != nil {
				return nil, err
			}
			if err := binary.Read(r, binary.LittleEndian, &rec.ModUnix); err != nil {
				return nil, err
			}
			var deleted uint8
			if err := binary.Read(r, binary.LittleEndian, &deleted); err != nil {
				return nil, err
			}
			rec.Deleted = deleted != 0
		}
		idx.CompactNameOrder = make([]int, len(idx.Records))
		for i := range idx.CompactNameOrder {
			idx.CompactNameOrder[i] = i
		}
		if sectionTableOffset != 0 {
			idx.Derived = readDerivedSectionsFromReaderAt(ra, size, sectionTableOffset, int(header.EntryCount))
		}
		return idx, nil
	}
	idx.Entries = make([]Entry, int(header.EntryCount))
	for i := range idx.Entries {
		entry := &idx.Entries[i]
		if entry.Path, err = readString(r); err != nil {
			return nil, err
		}
		if entry.Name, err = readString(r); err != nil {
			return nil, err
		}
		if entry.LowerPath, err = readString(r); err != nil {
			return nil, err
		}
		if entry.LowerName, err = readString(r); err != nil {
			return nil, err
		}
		if err := binary.Read(r, binary.LittleEndian, &entry.Size); err != nil {
			return nil, err
		}
		if err := binary.Read(r, binary.LittleEndian, &entry.Mode); err != nil {
			return nil, err
		}
		if err := binary.Read(r, binary.LittleEndian, &entry.ModUnix); err != nil {
			return nil, err
		}
	}
	if idx.NameOrder, err = readUint32Slice(r, int(header.EntryCount)); err != nil {
		return nil, err
	}
	if idx.PathOrder, err = readUint32Slice(r, int(header.EntryCount)); err != nil {
		return nil, err
	}
	return idx, nil
}

func writeString(w io.Writer, s string) error {
	if len(s) > int(^uint32(0)) {
		return errors.New("string too large")
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(len(s))); err != nil {
		return err
	}
	_, err := io.WriteString(w, s)
	return err
}

func readString(r io.Reader) (string, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return "", err
	}
	buf := make([]byte, int(n))
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func writeUint32Slice(w io.Writer, values []int) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(values))); err != nil {
		return err
	}
	tmp := make([]uint32, len(values))
	for i, value := range values {
		tmp[i] = uint32(value)
	}
	return binary.Write(w, binary.LittleEndian, tmp)
}

func readUint32Slice(r io.Reader, n int) ([]int, error) {
	validateAsPermutation := n >= 0
	if n < 0 {
		var count uint32
		if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
			return nil, err
		}
		n = int(count)
	} else {
		var count uint32
		if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
			return nil, err
		}
		if int(count) != n {
			return nil, errors.New("invalid order length")
		}
	}
	tmp := make([]uint32, n)
	if err := binary.Read(r, binary.LittleEndian, tmp); err != nil {
		return nil, err
	}
	values := make([]int, n)
	for i, value := range tmp {
		if validateAsPermutation && (int(value) < 0 || int(value) >= n) {
			return nil, errors.New("invalid order index")
		}
		values[i] = int(value)
	}
	return values, nil
}

func writeCompactRecordRefs(w io.Writer, parent, nameOff uint32, wide bool) error {
	if wide {
		if err := binary.Write(w, binary.LittleEndian, parent); err != nil {
			return err
		}
		return binary.Write(w, binary.LittleEndian, nameOff)
	}
	if parent > compactNarrowParentSentinel || nameOff >= compactNarrowParentSentinel {
		return errors.New("compact index too large for packed record format")
	}
	if err := writeUint24(w, parent); err != nil {
		return err
	}
	return writeUint24(w, nameOff)
}

func readCompactRecordRefs(r io.Reader, wide bool) (uint32, uint32, error) {
	if wide {
		var parent, nameOff uint32
		if err := binary.Read(r, binary.LittleEndian, &parent); err != nil {
			return 0, 0, err
		}
		if err := binary.Read(r, binary.LittleEndian, &nameOff); err != nil {
			return 0, 0, err
		}
		return parent, nameOff, nil
	}
	parent, err := readUint24(r)
	if err != nil {
		return 0, 0, err
	}
	nameOff, err := readUint24(r)
	if err != nil {
		return 0, 0, err
	}
	return parent, nameOff, nil
}

func writeUint24(w io.Writer, value uint32) error {
	var buf [3]byte
	buf[0] = byte(value)
	buf[1] = byte(value >> 8)
	buf[2] = byte(value >> 16)
	_, err := w.Write(buf[:])
	return err
}

func readUint24(r io.Reader) (uint32, error) {
	var buf [3]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, err
	}
	return uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16, nil
}

func blobString(blob []byte, off uint64, length uint32) (string, error) {
	end := off + uint64(length)
	if end < off || end > uint64(len(blob)) {
		return "", errors.New("invalid string reference")
	}
	return string(blob[off:end]), nil
}

func saveIndex(path string, idx *Index) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if engineV9Enabled() && idx.Compact {
		err = writeIndexV9File(f, idx)
	} else {
		bw := bufio.NewWriterSize(f, 16*1024*1024)
		err = writeIndex(bw, idx)
		if flushErr := bw.Flush(); err == nil {
			err = flushErr
		}
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if syncErr != nil {
		_ = os.Remove(tmp)
		return syncErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if dir, err := os.Open(dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n += int64(n)
	return n, err
}

type indexSectionBlob struct {
	tag     uint32
	data    []byte
	subtree *subtreeSectionBlob
	flags   uint32
}

const subtreeSectionScratchBytes = 64 * 1024

type subtreeSectionBlob struct {
	parts [][]uint32
}

func writeSubtreeSection(w io.Writer, section *subtreeSectionBlob) (int64, error) {
	if section == nil {
		return 0, nil
	}
	var scratch [subtreeSectionScratchBytes]byte
	var written int64
	for _, part := range section.parts {
		var count [4]byte
		binary.LittleEndian.PutUint32(count[:], uint32(len(part)))
		n, err := w.Write(count[:])
		written += int64(n)
		if err != nil {
			return written, err
		}
		if n != len(count) {
			return written, io.ErrShortWrite
		}
		for start := 0; start < len(part); {
			chunk := min(len(part)-start, len(scratch)/4)
			for i := 0; i < chunk; i++ {
				binary.LittleEndian.PutUint32(scratch[i*4:], part[start+i])
			}
			bytes := scratch[:chunk*4]
			n, err := w.Write(bytes)
			written += int64(n)
			if err != nil {
				return written, err
			}
			if n != len(bytes) {
				return written, io.ErrShortWrite
			}
			start += chunk
		}
	}
	return written, nil
}

type indexSectionTableEntry struct {
	tag    uint32
	offset uint64
	length uint64
	flags  uint32
}

func writeIndexV9File(f *os.File, idx *Index) error {
	bw := bufio.NewWriterSize(f, 16*1024*1024)
	cw := &countingWriter{w: bw}
	sectionOffsetPatch := int64(binary.Size(diskHeader{}))
	header := diskHeader{
		Magic:      indexMagicV9,
		Version:    indexVersionV9,
		EntryCount: uint64(idx.compactRecordCount()),
		RootCount:  uint64(len(idx.Roots)),
		BuiltUnix:  idx.BuiltAt.UnixNano(),
		JournalID:  idx.JournalID,
		Checkpoint: idx.Checkpoint,
		Compact:    compactDiskFlag,
	}
	recordCount := idx.compactRecordCount()
	nameIDs := make(map[string]uint32, max(1, recordCount/2))
	nameBlob := make([]byte, 0, recordCount*16)
	nameOffs := make([]uint32, 0, recordCount)
	nameLens := make([]uint16, 0, recordCount)
	nameTokens := make([]string, 0, recordCount)
	nameIDForRecord := make([]uint32, recordCount)
	for i := 0; i < recordCount; i++ {
		rec := idx.compactRecord(i)
		if len(rec.Name) > int(^uint16(0)) {
			return errors.New("compact name too large")
		}
		id, ok := nameIDs[rec.Name]
		if !ok {
			id = uint32(len(nameOffs))
			nameIDs[rec.Name] = id
			nameOffs = append(nameOffs, uint32(len(nameBlob)))
			nameLens = append(nameLens, uint16(len(rec.Name)))
			nameTokens = append(nameTokens, rec.Name)
			nameBlob = append(nameBlob, rec.Name...)
		}
		nameIDForRecord[i] = id
	}
	// The deduplication map is only needed while assigning record references.
	// Drop it before derived-section generation; retaining millions of string
	// keys here was the second large peak in v8->v9 conversion.
	nameIDs = nil
	debug.FreeOSMemory()
	header.NameBlobLen = uint64(len(nameBlob))
	header.TokenCount = uint64(len(nameOffs))
	if compactNeedsWideDiskRecords(recordCount, len(nameOffs)) {
		header.Compact |= compactDiskWideRefsFlag
	}
	if idx.CompactAttrs {
		header.Compact |= compactDiskAttrsFlag
	}
	if err := binary.Write(cw, binary.LittleEndian, header); err != nil {
		return err
	}
	if err := binary.Write(cw, binary.LittleEndian, uint64(0)); err != nil {
		return err
	}
	for _, s := range []string{idx.Source, idx.Volume, idx.ContentHash} {
		if err := writeString(cw, s); err != nil {
			return err
		}
	}
	for _, root := range idx.Roots {
		if err := writeString(cw, root); err != nil {
			return err
		}
	}
	wideRefs := header.Compact&compactDiskWideRefsFlag != 0
	if _, err := cw.Write(nameBlob); err != nil {
		return err
	}
	for i := range nameOffs {
		if err := binary.Write(cw, binary.LittleEndian, nameOffs[i]); err != nil {
			return err
		}
		if err := binary.Write(cw, binary.LittleEndian, nameLens[i]); err != nil {
			return err
		}
	}
	for i := 0; i < recordCount; i++ {
		rec := idx.compactRecord(i)
		rec.NameOff = nameIDForRecord[i]
		parent := uint32(compactNarrowParentSentinel)
		if wideRefs {
			parent = compactWideParentSentinel
		}
		if rec.Parent >= 0 {
			if !wideRefs && uint32(rec.Parent) >= compactNarrowParentSentinel {
				return errors.New("compact index too large for packed record format")
			}
			parent = uint32(rec.Parent)
		}
		if err := binary.Write(cw, binary.LittleEndian, rec.FRN); err != nil {
			return err
		}
		if err := binary.Write(cw, binary.LittleEndian, rec.ParentFRN); err != nil {
			return err
		}
		if err := writeCompactRecordRefs(cw, parent, rec.NameOff, wideRefs); err != nil {
			return err
		}
		if err := binary.Write(cw, binary.LittleEndian, rec.Mode); err != nil {
			return err
		}
		if err := binary.Write(cw, binary.LittleEndian, rec.Size); err != nil {
			return err
		}
		if err := binary.Write(cw, binary.LittleEndian, rec.ModUnix); err != nil {
			return err
		}
		deleted := uint8(0)
		if rec.Deleted {
			deleted = 1
		}
		if err := binary.Write(cw, binary.LittleEndian, deleted); err != nil {
			return err
		}
	}
	// The record table is now durable in the output stream.  Keep only
	// nameTokens for LOWR/PNGR generation and release the other name-table
	// working buffers before building postings.
	nameIDForRecord = nil
	nameOffs = nil
	nameLens = nil
	nameBlob = nil
	debug.FreeOSMemory()
	v9PersistTrace("record-table")
	table, err := writeDerivedSectionStream(cw, idx, nameTokens)
	if err != nil {
		return err
	}
	v9PersistTrace("derived-complete")
	if err := writeAlignment(cw, 8); err != nil {
		return err
	}
	sectionTableOffset := uint64(cw.n)
	if err := binary.Write(cw, binary.LittleEndian, uint32(len(table))); err != nil {
		return err
	}
	for _, entry := range table {
		if err := binary.Write(cw, binary.LittleEndian, entry.tag); err != nil {
			return err
		}
		if err := binary.Write(cw, binary.LittleEndian, entry.offset); err != nil {
			return err
		}
		if err := binary.Write(cw, binary.LittleEndian, entry.length); err != nil {
			return err
		}
		if err := binary.Write(cw, binary.LittleEndian, entry.flags); err != nil {
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	if _, err := f.Seek(sectionOffsetPatch, io.SeekStart); err != nil {
		return err
	}
	var patch [8]byte
	binary.LittleEndian.PutUint64(patch[:], sectionTableOffset)
	if _, err := f.Write(patch[:]); err != nil {
		return err
	}
	_, err = f.Seek(0, io.SeekEnd)
	return err
}

var v9PersistStageObserver func(string, runtime.MemStats)

func releaseV9PersistStage() {
	if serviceLowMemoryMode() {
		runtime.GC()
		debug.FreeOSMemory()
	}
}

func v9PersistTrace(stage string) {
	if os.Getenv("SEEKFS_V9_PERSIST_TRACE") != "1" {
		return
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	if v9PersistStageObserver != nil {
		v9PersistStageObserver(stage, mem)
	}
	fmt.Fprintf(os.Stderr, "v9-persist stage=%s time=%s heap_alloc=%d heap_inuse=%d heap_objects=%d\n",
		stage, time.Now().Format(time.RFC3339Nano), mem.HeapAlloc, mem.HeapInuse, mem.HeapObjects)
	if dir := os.Getenv("SEEKFS_V9_PERSIST_PROFILE_DIR"); dir != "" {
		if os.MkdirAll(dir, 0o755) == nil {
			name := strings.NewReplacer("\\", "_", "/", "_", ":", "_").Replace(stage)
			if f, err := os.Create(filepath.Join(dir, name+".pprof")); err == nil {
				_ = pprof.Lookup("heap").WriteTo(f, 0)
				_ = f.Close()
			}
		}
	}
}

func writeAlignment(w io.Writer, align int64) error {
	if align <= 1 {
		return nil
	}
	if cw, ok := w.(*countingWriter); ok {
		pad := int((align - (cw.n % align)) % align)
		if pad == 0 {
			return nil
		}
		_, err := cw.Write(make([]byte, pad))
		return err
	}
	return nil
}

func newDerivedSectionVolumeIndex(idx *Index) *serviceVolumeIndex {
	return newDerivedSectionVolumeIndexMode(idx, false)
}

func newStagedDerivedSectionVolumeIndex(idx *Index) *serviceVolumeIndex {
	return newDerivedSectionVolumeIndexMode(idx, true)
}

func newDerivedSectionVolumeIndexMode(idx *Index, staged bool) *serviceVolumeIndex {
	vol := &serviceVolumeIndex{
		index:       idx,
		volume:      idx.Volume,
		state:       "ready",
		pathCache:   make(map[int]string),
		lastPersist: time.Now(),
	}
	// Reuse already-persisted topology without copying it.  v8 inputs have no
	// derived topology and are filled by buildDerivedSectionBlobs below.
	vol.childOffsets = idx.Derived.ChildOffsets
	vol.childIDs = idx.Derived.ChildIDs
	vol.rootIDs = idx.Derived.RootIDs
	vol.subtreeOrder = idx.Derived.SubtreeOrder
	vol.subtreeStart = idx.Derived.SubtreeStart
	vol.subtreeEnd = idx.Derived.SubtreeEnd
	if staged {
		vol.queryIndex = &residentQueryIndex{}
	} else {
		vol.queryIndex = buildResidentQueryIndexForPersistence(vol)
	}
	return vol
}

func buildPersistencePostingFamily(vol *serviceVolumeIndex, family string) {
	if vol == nil || vol.index == nil || vol.queryIndex == nil {
		return
	}
	recordCount := vol.index.compactRecordCount()
	switch family {
	case "ext":
		if postings := vol.index.Derived.Postings; postings != nil && len(postings[indexSectionPEXT].Data) > 0 {
			return
		}
		vol.queryIndex.ext = make(map[string][]uint32)
		for id := 0; id < recordCount; id++ {
			rec := vol.index.compactRecord(id)
			if rec.Deleted {
				continue
			}
			ext := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
			if ext != "" {
				vol.queryIndex.ext[strings.ToLower(ext)] = append(vol.queryIndex.ext[strings.ToLower(ext)], uint32(id))
			}
		}
		sortResidentPostings(vol.queryIndex.ext)
	case "components":
		if postings := vol.index.Derived.Postings; postings != nil && len(postings[indexSectionPCMP].Data) > 0 {
			return
		}
		vol.queryIndex.components = make(map[string][]uint32)
		for id := 0; id < recordCount; id++ {
			rec := vol.index.compactRecord(id)
			if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 {
				continue
			}
			name := vol.index.compactLowerNameAt(id)
			if name != "" && name != "." {
				vol.queryIndex.components[name] = append(vol.queryIndex.components[name], uint32(id))
			}
		}
		sortResidentPostings(vol.queryIndex.components)
	case "attrs":
		if len(vol.index.Derived.AttrBits) > 0 {
			vol.queryIndex.attrBits = vol.index.Derived.AttrBits
			return
		}
		vol.queryIndex.attrBits = make(map[uint32][]uint32, 5)
		for id := 0; id < recordCount; id++ {
			rec := vol.index.compactRecord(id)
			if rec.Deleted {
				continue
			}
			for _, bit := range queryAttrBits() {
				if rec.Mode&bit == bit {
					vol.queryIndex.attrBits[bit] = append(vol.queryIndex.attrBits[bit], uint32(id))
				}
			}
		}
		sortResidentAttrPostings(vol.queryIndex.attrBits)
	}
}

func populatePersistenceFRNs(vol *serviceVolumeIndex, idx *Index) {
	if vol == nil || idx == nil || !idx.Compact || idx.Source != "usn" {
		return
	}
	recordCount := idx.compactRecordCount()
	if len(idx.Derived.FRNs) == recordCount && len(idx.Derived.FRNRecordIDs) == recordCount {
		vol.frns = idx.Derived.FRNs
		vol.frnRecordIDs = idx.Derived.FRNRecordIDs
		return
	}
	vol.frns = make([]uint64, 0, recordCount)
	vol.frnRecordIDs = make([]uint32, 0, recordCount)
	for id := 0; id < recordCount; id++ {
		rec := idx.compactRecord(id)
		if rec.FRN != 0 {
			vol.frns = append(vol.frns, rec.FRN)
			vol.frnRecordIDs = append(vol.frnRecordIDs, uint32(id))
		}
	}
	sortFRNIndexEntries(vol.frns, vol.frnRecordIDs)
}

func prepareDerivedSectionVolume(idx *Index) *serviceVolumeIndex {
	if idx == nil || !idx.Compact {
		return nil
	}
	// The writer needs query postings and rank metadata, but not the resident
	// service wrapper, FRN maps, or packed record copy.  Keeping this view
	// separate prevents v8->v9 compaction from materializing the same large
	// index twice.
	vol := newDerivedSectionVolumeIndex(idx)
	populatePersistenceFRNs(vol, idx)
	if vol.queryIndex == nil {
		vol.queryIndex = buildResidentQueryIndexForPersistence(vol)
	}
	v9PersistTrace("resident-prepared")
	if len(vol.queryIndex.nameOrder) == 0 || len(vol.queryIndex.nameRank) == 0 {
		vol.queryIndex.nameOrder, vol.queryIndex.nameRank = buildCompactNameOrderRank(idx)
	}
	v9PersistTrace("name-rank-ready")
	if idx.compactHasSize() && (len(vol.queryIndex.sizeOrder) == 0 || len(vol.queryIndex.sizeRank) == 0) {
		vol.queryIndex.sizeOrder, vol.queryIndex.sizeRank = buildCompactSizeOrderRank(idx)
	}
	v9PersistTrace("size-rank-ready")
	if idx.compactHasModTime() && (len(vol.queryIndex.modOrder) == 0 || len(vol.queryIndex.modRank) == 0) {
		vol.queryIndex.modOrder, vol.queryIndex.modRank = buildCompactModifiedOrderRank(idx)
	}
	v9PersistTrace("modified-rank-ready")
	if len(vol.queryIndex.extOrder) == 0 || len(vol.queryIndex.extRank) == 0 {
		vol.queryIndex.extOrder, vol.queryIndex.extRank = buildCompactExtensionOrderRank(idx)
	}
	v9PersistTrace("extension-rank-ready")
	if len(vol.queryIndex.typeOrder) == 0 || len(vol.queryIndex.typeRank) == 0 {
		vol.queryIndex.typeOrder, vol.queryIndex.typeRank = buildCompactTypeOrderRank(idx)
	}
	v9PersistTrace("type-rank-ready")
	if len(vol.queryIndex.pathOrder) == 0 || len(vol.queryIndex.pathRank) == 0 {
		vol.queryIndex.pathOrder, vol.queryIndex.pathRank = buildCompactPathOrderRank(idx)
	}
	v9PersistTrace("path-rank-ready")
	if len(vol.childOffsets) == 0 || len(vol.childIDs) == 0 {
		vol.buildCompactChildren()
	}
	v9PersistTrace("children-ready")
	// Persisted v9 generation must not inherit the low-memory serving toggle:
	// SUBT is the source of truth for mapped component bounds and subtree
	// execution after restart. The serving path may still decline to build this
	// metadata when it is absent, but a writer should include it whenever the
	// child graph is available.
	if len(vol.subtreeOrder) == 0 && len(vol.childOffsets) > 0 {
		vol.buildSubtreeRanges()
	}
	v9PersistTrace("subtree-ready")
	// Size/modification rank sections are legitimately omitted when a compact
	// index has no non-zero values. Bounds still need a safe rank for every
	// persisted sort column; the default name rank is the same fallback used by
	// rankForQuery for those absent columns.
	sizeRank := vol.queryIndex.sizeRank
	if len(sizeRank) == 0 {
		sizeRank = vol.queryIndex.nameRank
	}
	modRank := vol.queryIndex.modRank
	if len(modRank) == 0 {
		modRank = vol.queryIndex.nameRank
	}
	vol.subtreeSizeRank = vol.buildSubtreeMinRanks(sizeRank)
	vol.subtreeModRank = vol.buildSubtreeMinRanks(modRank)
	vol.subtreeExtRank = vol.buildSubtreeMinRanks(vol.queryIndex.extRank)
	vol.subtreeTypeRank = vol.buildSubtreeMinRanks(vol.queryIndex.typeRank)
	vol.subtreePathRank = vol.buildSubtreeMinRanks(vol.queryIndex.pathRank)
	v9PersistTrace("derived-prepared")
	return vol
}

func buildDerivedSectionBlobs(idx *Index, nameTokens []string) []indexSectionBlob {
	out := make([]indexSectionBlob, 0, 16)
	_ = forEachDerivedSection(idx, nameTokens, func(section indexSectionBlob) error {
		if section.subtree != nil {
			section.data = encodeUint32Section(section.subtree.parts...)
			section.subtree = nil
		}
		out = append(out, section)
		return nil
	})
	return out
}

// forEachDerivedSection builds independent persistence families in output
// order.  Rank orders are emitted as soon as they are built; only the rank
// vectors needed by later rank-bound sections remain live.  This keeps the
// offline writer from preparing all serving maps, order arrays, and subtree
// workspaces at the same time.
func forEachDerivedSection(idx *Index, nameTokens []string, emit func(indexSectionBlob) error) error {
	vol := newStagedDerivedSectionVolumeIndex(idx)
	if vol == nil {
		return nil
	}
	v9PersistTrace("resident-prepared")
	emitSection := func(tag uint32, data []byte) error {
		return emit(indexSectionBlob{tag: tag, data: data})
	}
	emitSubtree := func(parts ...[]uint32) error {
		return emit(indexSectionBlob{tag: indexSectionSUBT, subtree: &subtreeSectionBlob{parts: parts}})
	}

	nameOrder, nameRank := vol.queryIndex.nameOrder, vol.queryIndex.nameRank
	if len(nameOrder) == 0 || len(nameRank) == 0 {
		nameOrder, nameRank = buildCompactNameOrderRank(idx)
	}
	vol.queryIndex.nameOrder, vol.queryIndex.nameRank = nameOrder, nameRank
	v9PersistTrace("name-rank-ready")
	if err := emitSection(indexSectionRANK, encodeUint32Section(nameOrder, nameRank)); err != nil {
		return err
	}
	vol.queryIndex.nameOrder = nil
	nameOrder = nil

	var sizeRank, modRank, extRank, typeRank, pathRank []uint32
	if idx.compactHasSize() {
		order, rank := vol.queryIndex.sizeOrder, vol.queryIndex.sizeRank
		if len(order) == 0 || len(rank) == 0 {
			order, rank = buildCompactSizeOrderRank(idx)
		}
		vol.queryIndex.sizeOrder, vol.queryIndex.sizeRank = order, rank
		sizeRank = rank
		v9PersistTrace("size-rank-ready")
		if err := emitSection(indexSectionSRNK, encodeUint32Section(order, rank)); err != nil {
			return err
		}
		vol.queryIndex.sizeOrder = nil
		vol.queryIndex.sizeRank = nil
	}
	if idx.compactHasModTime() {
		order, rank := vol.queryIndex.modOrder, vol.queryIndex.modRank
		if len(order) == 0 || len(rank) == 0 {
			order, rank = buildCompactModifiedOrderRank(idx)
		}
		vol.queryIndex.modOrder, vol.queryIndex.modRank = order, rank
		modRank = rank
		v9PersistTrace("modified-rank-ready")
		if err := emitSection(indexSectionMRNK, encodeUint32Section(order, rank)); err != nil {
			return err
		}
		vol.queryIndex.modOrder = nil
		vol.queryIndex.modRank = nil
	}
	{
		order, rank := vol.queryIndex.extOrder, vol.queryIndex.extRank
		if len(order) == 0 || len(rank) == 0 {
			order, rank = buildCompactExtensionOrderRank(idx)
		}
		vol.queryIndex.extOrder, vol.queryIndex.extRank = order, rank
		extRank = rank
		v9PersistTrace("extension-rank-ready")
		if err := emitSection(indexSectionERNK, encodeUint32Section(order, rank)); err != nil {
			return err
		}
		vol.queryIndex.extOrder = nil
	}
	{
		order, rank := vol.queryIndex.typeOrder, vol.queryIndex.typeRank
		if len(order) == 0 || len(rank) == 0 {
			order, rank = buildCompactTypeOrderRank(idx)
		}
		vol.queryIndex.typeOrder, vol.queryIndex.typeRank = order, rank
		typeRank = rank
		v9PersistTrace("type-rank-ready")
		if err := emitSection(indexSectionTRNK, encodeUint32Section(order, rank)); err != nil {
			return err
		}
		vol.queryIndex.typeOrder = nil
	}
	{
		order, rank := vol.queryIndex.pathOrder, vol.queryIndex.pathRank
		if len(order) == 0 || len(rank) == 0 {
			order, rank = buildCompactPathOrderRank(idx)
		}
		vol.queryIndex.pathOrder, vol.queryIndex.pathRank = order, rank
		pathRank = rank
		v9PersistTrace("path-rank-ready")
		if err := emitSection(indexSectionPRNK, encodeUint32Section(order, rank)); err != nil {
			return err
		}
		vol.queryIndex.pathOrder = nil
	}
	// Extension postings only need the raw rank vectors. Emit this family
	// before subtree minima are prepared, so those raw vectors can be released
	// as each corresponding subtree-minimum vector is built.
	buildPersistencePostingFamily(vol, "ext")
	if err := emitSection(indexSectionPEXT, encodeStringPostingSection(vol.queryIndex.ext, nameRank)); err != nil {
		return err
	}
	if err := emitSection(indexSectionPXRB, encodePostingRankBounds(buildStringPostingRankBounds(
		vol.queryIndex.ext, nameRank, sizeRank, modRank, extRank, typeRank, pathRank,
	))); err != nil {
		return err
	}
	vol.queryIndex.ext = nil

	if len(vol.childOffsets) == 0 || len(vol.childIDs) == 0 {
		vol.buildCompactChildren()
	}
	v9PersistTrace("children-ready")
	if len(vol.subtreeOrder) == 0 && len(vol.childOffsets) > 0 {
		vol.buildSubtreeRanges()
	}
	v9PersistTrace("subtree-ready")
	if len(sizeRank) == 0 {
		sizeRank = nameRank
	}
	if len(modRank) == 0 {
		modRank = nameRank
	}
	vol.subtreeSizeRank = vol.buildSubtreeMinRanks(sizeRank)
	sizeRank = nil
	vol.subtreeModRank = vol.buildSubtreeMinRanks(modRank)
	modRank = nil
	vol.subtreeExtRank = vol.buildSubtreeMinRanks(extRank)
	extRank = nil
	vol.queryIndex.extRank = nil
	vol.subtreeTypeRank = vol.buildSubtreeMinRanks(typeRank)
	typeRank = nil
	vol.queryIndex.typeRank = nil
	vol.subtreePathRank = vol.buildSubtreeMinRanks(pathRank)
	pathRank = nil
	vol.queryIndex.pathRank = nil
	releaseV9PersistStage()
	v9PersistTrace("derived-prepared")
	if err := emitSection(indexSectionCHLD, encodeUint32Section(vol.childOffsets, vol.childIDs, vol.rootIDs)); err != nil {
		return err
	}
	if err := emitSubtree(
		vol.subtreeStart, vol.subtreeEnd, vol.subtreeOrder,
		vol.subtreeSizeRank, vol.subtreeModRank, vol.subtreeExtRank,
		vol.subtreeTypeRank, vol.subtreePathRank,
	); err != nil {
		return err
	}
	populatePersistenceFRNs(vol, idx)
	if err := emitSection(indexSectionFRNS, encodeFRNSection(vol.frns, vol.frnRecordIDs)); err != nil {
		return err
	}
	vol.frns, vol.frnRecordIDs = nil, nil
	if err := emitSection(indexSectionLOWR, encodeLowerSection(nameTokens)); err != nil {
		return err
	}
	buildPersistencePostingFamily(vol, "attrs")
	if err := emitSection(indexSectionPATR, encodeAttrPostingSection(vol.queryIndex.attrBits)); err != nil {
		return err
	}
	vol.queryIndex.attrBits = nil

	buildPersistencePostingFamily(vol, "components")
	if err := emitSection(indexSectionPCMP, encodeStringPostingSection(vol.queryIndex.components, nameRank)); err != nil {
		return err
	}
	if err := emitSection(indexSectionPXRC, encodePostingRankBounds(buildComponentPostingRankBounds(vol.queryIndex.components, vol))); err != nil {
		return err
	}
	vol.queryIndex.components = nil
	vol.childOffsets, vol.childIDs, vol.rootIDs = nil, nil, nil
	vol.subtreeStart, vol.subtreeEnd, vol.subtreeOrder = nil, nil, nil
	vol.subtreeSizeRank, vol.subtreeModRank, vol.subtreeExtRank = nil, nil, nil
	vol.subtreeTypeRank, vol.subtreePathRank = nil, nil

	nameGrams := buildSelectiveNameTrigramIndex(idx, serviceLowMemoryTrigramStoredPostingMax())
	gramData := encodeGramPostingSection(nameGrams, nameRank)
	selfNameGramData := optionalSelfNameGramSection(idx, nameGrams, nameRank)
	nameGrams = nil
	if err := emitSection(indexSectionPNGR, gramData); err != nil {
		return err
	}
	if len(selfNameGramData) > 0 {
		if err := emitSection(indexSectionPNGC, selfNameGramData); err != nil {
			return err
		}
	}
	return nil
}

func forEachDerivedSectionLegacy(idx *Index, nameTokens []string, emit func(indexSectionBlob) error) error {
	vol := prepareDerivedSectionVolume(idx)
	if vol == nil {
		return nil
	}
	// These serving-only structures are not needed by the v9 writer.  Drop
	// them before encoding large persisted postings to shorten their lifetime.
	vol.queryIndex.dirs = nil
	vol.queryIndex.pathGrams = nil
	vol.queryIndex.extTop = nil
	sizeRank := vol.queryIndex.sizeRank
	if len(sizeRank) == 0 {
		sizeRank = vol.queryIndex.nameRank
	}
	modRank := vol.queryIndex.modRank
	if len(modRank) == 0 {
		modRank = vol.queryIndex.nameRank
	}
	if err := emit(indexSectionBlob{tag: indexSectionRANK, data: encodeUint32Section(vol.queryIndex.nameOrder, vol.queryIndex.nameRank)}); err != nil {
		return err
	}
	if len(vol.queryIndex.sizeOrder) > 0 && len(vol.queryIndex.sizeRank) > 0 {
		if err := emit(indexSectionBlob{tag: indexSectionSRNK, data: encodeUint32Section(vol.queryIndex.sizeOrder, vol.queryIndex.sizeRank)}); err != nil {
			return err
		}
	}
	if len(vol.queryIndex.modOrder) > 0 && len(vol.queryIndex.modRank) > 0 {
		if err := emit(indexSectionBlob{tag: indexSectionMRNK, data: encodeUint32Section(vol.queryIndex.modOrder, vol.queryIndex.modRank)}); err != nil {
			return err
		}
	}
	if len(vol.queryIndex.extOrder) > 0 && len(vol.queryIndex.extRank) > 0 {
		if err := emit(indexSectionBlob{tag: indexSectionERNK, data: encodeUint32Section(vol.queryIndex.extOrder, vol.queryIndex.extRank)}); err != nil {
			return err
		}
	}
	if len(vol.queryIndex.typeOrder) > 0 && len(vol.queryIndex.typeRank) > 0 {
		if err := emit(indexSectionBlob{tag: indexSectionTRNK, data: encodeUint32Section(vol.queryIndex.typeOrder, vol.queryIndex.typeRank)}); err != nil {
			return err
		}
	}
	if len(vol.queryIndex.pathOrder) > 0 && len(vol.queryIndex.pathRank) > 0 {
		if err := emit(indexSectionBlob{tag: indexSectionPRNK, data: encodeUint32Section(vol.queryIndex.pathOrder, vol.queryIndex.pathRank)}); err != nil {
			return err
		}
	}
	if err := emit(indexSectionBlob{tag: indexSectionCHLD, data: encodeUint32Section(vol.childOffsets, vol.childIDs, vol.rootIDs)}); err != nil {
		return err
	}
	if err := emit(indexSectionBlob{tag: indexSectionSUBT, data: encodeUint32Section(
		vol.subtreeStart, vol.subtreeEnd, vol.subtreeOrder,
		vol.subtreeSizeRank, vol.subtreeModRank, vol.subtreeExtRank,
		vol.subtreeTypeRank, vol.subtreePathRank,
	)}); err != nil {
		return err
	}
	if err := emit(indexSectionBlob{tag: indexSectionFRNS, data: encodeFRNSection(vol.frns, vol.frnRecordIDs)}); err != nil {
		return err
	}
	vol.frns = nil
	vol.frnRecordIDs = nil
	if err := emit(indexSectionBlob{tag: indexSectionLOWR, data: encodeLowerSection(nameTokens)}); err != nil {
		return err
	}
	if err := emit(indexSectionBlob{tag: indexSectionPATR, data: encodeAttrPostingSection(vol.queryIndex.attrBits)}); err != nil {
		return err
	}
	vol.queryIndex.attrBits = nil
	if err := emit(indexSectionBlob{tag: indexSectionPEXT, data: encodeStringPostingSection(vol.queryIndex.ext, vol.queryIndex.nameRank)}); err != nil {
		return err
	}
	if err := emit(indexSectionBlob{tag: indexSectionPXRB, data: encodePostingRankBounds(buildStringPostingRankBounds(vol.queryIndex.ext, vol.queryIndex.nameRank, sizeRank, modRank, vol.queryIndex.extRank, vol.queryIndex.typeRank, vol.queryIndex.pathRank))}); err != nil {
		return err
	}
	vol.queryIndex.ext = nil
	releaseV9PersistStage()
	if err := emit(indexSectionBlob{tag: indexSectionPCMP, data: encodeStringPostingSection(vol.queryIndex.components, vol.queryIndex.nameRank)}); err != nil {
		return err
	}
	if err := emit(indexSectionBlob{tag: indexSectionPXRC, data: encodePostingRankBounds(buildComponentPostingRankBounds(vol.queryIndex.components, vol))}); err != nil {
		return err
	}
	vol.queryIndex.components = nil
	releaseV9PersistStage()
	vol.childOffsets = nil
	vol.childIDs = nil
	vol.rootIDs = nil
	vol.subtreeStart = nil
	vol.subtreeEnd = nil
	vol.subtreeOrder = nil
	vol.subtreeSizeRank = nil
	vol.subtreeModRank = nil
	vol.subtreeExtRank = nil
	vol.subtreeTypeRank = nil
	vol.subtreePathRank = nil
	nameGrams := buildSelectiveNameTrigramIndex(idx, serviceLowMemoryTrigramStoredPostingMax())
	return emit(indexSectionBlob{tag: indexSectionPNGR, data: encodeGramPostingSection(nameGrams, vol.queryIndex.nameRank)})
}

func writeDerivedSectionStream(cw *countingWriter, idx *Index, nameTokens []string) ([]indexSectionTableEntry, error) {
	return writeDerivedSectionStreamObserved(cw, idx, nameTokens, nil)
}

// writeDerivedSectionStreamObserved is also used by the bounded writer
// benchmark.  The callback observes the one section buffer currently held by
// the stream; it must return to zero before the next section is built.
func writeDerivedSectionStreamObserved(cw *countingWriter, idx *Index, nameTokens []string, observe func(int)) ([]indexSectionTableEntry, error) {
	table := make([]indexSectionTableEntry, 0, 16)
	err := forEachDerivedSection(idx, nameTokens, func(section indexSectionBlob) error {
		if len(section.data) == 0 && section.subtree == nil {
			return nil
		}
		v9PersistTrace(fmt.Sprintf("section-%08x", section.tag))
		if err := writeAlignment(cw, 8); err != nil {
			return err
		}
		offset := uint64(cw.n)
		before := cw.n
		if section.subtree != nil {
			if _, err := writeSubtreeSection(cw, section.subtree); err != nil {
				return err
			}
		} else {
			if observe != nil {
				observe(len(section.data))
			}
			if _, err := cw.Write(section.data); err != nil {
				return err
			}
		}
		length := uint64(cw.n - before)
		if observe != nil {
			observe(int(length))
		}
		table = append(table, indexSectionTableEntry{tag: section.tag, offset: offset, length: length, flags: section.flags})
		v9PersistTrace(fmt.Sprintf("after-%08x", section.tag))
		if observe != nil {
			observe(0)
		}
		return nil
	})
	return table, err
}

func derivedSectionInfo(derived indexDerivedSections) ([]string, int) {
	sections := make([]string, 0, 4)
	bytes := 0
	if len(derived.NameOrder) > 0 || len(derived.NameRank) > 0 {
		sections = append(sections, "RANK")
		bytes += 4 * (len(derived.NameOrder) + len(derived.NameRank))
	}
	if len(derived.SizeOrder) > 0 || len(derived.SizeRank) > 0 {
		sections = append(sections, "SRNK")
		bytes += 4 * (len(derived.SizeOrder) + len(derived.SizeRank))
	}
	if len(derived.ModOrder) > 0 || len(derived.ModRank) > 0 {
		sections = append(sections, "MRNK")
		bytes += 4 * (len(derived.ModOrder) + len(derived.ModRank))
	}
	if len(derived.ExtOrder) > 0 || len(derived.ExtRank) > 0 {
		sections = append(sections, "ERNK")
		bytes += 4 * (len(derived.ExtOrder) + len(derived.ExtRank))
	}
	if len(derived.TypeOrder) > 0 || len(derived.TypeRank) > 0 {
		sections = append(sections, "TRNK")
		bytes += 4 * (len(derived.TypeOrder) + len(derived.TypeRank))
	}
	if len(derived.PathOrder) > 0 || len(derived.PathRank) > 0 {
		sections = append(sections, "PRNK")
		bytes += 4 * (len(derived.PathOrder) + len(derived.PathRank))
	}
	if len(derived.ChildOffsets) > 0 || len(derived.ChildIDs) > 0 || len(derived.RootIDs) > 0 {
		sections = append(sections, "CHLD")
		bytes += 4 * (len(derived.ChildOffsets) + len(derived.ChildIDs) + len(derived.RootIDs))
	}
	if len(derived.SubtreeStart) > 0 || len(derived.SubtreeEnd) > 0 || len(derived.SubtreeOrder) > 0 {
		sections = append(sections, "SUBT")
		bytes += 4 * (len(derived.SubtreeStart) + len(derived.SubtreeEnd) + len(derived.SubtreeOrder) +
			len(derived.SubtreeSizeRank) + len(derived.SubtreeModRank) + len(derived.SubtreeExtRank) +
			len(derived.SubtreeTypeRank) + len(derived.SubtreePathRank))
	}
	if len(derived.FRNs) > 0 || len(derived.FRNRecordIDs) > 0 {
		sections = append(sections, "FRNS")
		bytes += 8*len(derived.FRNs) + 4*len(derived.FRNRecordIDs)
	}
	if len(derived.LowerOffs) > 0 || len(derived.LowerBlob) > 0 {
		sections = append(sections, "LOWR")
		bytes += 6*len(derived.LowerOffs) + len(derived.LowerBlob)
	}
	for _, item := range []struct {
		tag  uint32
		name string
	}{
		{indexSectionPATR, "PATR"},
		{indexSectionPEXT, "PEXT"},
		{indexSectionPXRB, "PXRB"},
		{indexSectionPXRC, "PXRC"},
		{indexSectionPCMP, "PCMP"},
		{indexSectionPNGR, "PNGR"},
	} {
		if item.tag == indexSectionPATR {
			if len(derived.AttrBits) > 0 {
				count := 0
				for _, ids := range derived.AttrBits {
					count += len(ids)
				}
				sections = append(sections, item.name)
				bytes += 4 * (len(queryAttrBits()) + count)
			}
			continue
		}
		if item.tag == indexSectionPXRB || item.tag == indexSectionPXRC {
			boundsTag := indexSectionPEXT
			if item.tag == indexSectionPXRC {
				boundsTag = indexSectionPCMP
			}
			if bounds, ok := derived.PostingBounds[boundsTag]; ok && bounds.BlockCount > 0 {
				sections = append(sections, item.name)
				bytes += 4 * (5 + len(bounds.Size) + len(bounds.Modified) + len(bounds.Extension) + len(bounds.Type) + len(bounds.Path))
			}
			continue
		}
		if posting, ok := derived.Postings[item.tag]; ok && posting.EntryCount > 0 {
			sections = append(sections, item.name)
			bytes += posting.Bytes
		}
	}
	return sections, bytes
}

func encodeUint32Section(parts ...[]uint32) []byte {
	var buf bytes.Buffer
	for _, part := range parts {
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(part)))
		_ = binary.Write(&buf, binary.LittleEndian, part)
	}
	return buf.Bytes()
}

func encodeFRNSection(frns []uint64, ids []uint32) []byte {
	if len(frns) == 0 || len(frns) != len(ids) {
		return nil
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(frns)))
	_ = binary.Write(&buf, binary.LittleEndian, frns)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(ids)))
	_ = binary.Write(&buf, binary.LittleEndian, ids)
	return buf.Bytes()
}

func encodeLowerSection(nameTokens []string) []byte {
	if len(nameTokens) == 0 {
		return nil
	}
	offs := make([]uint32, len(nameTokens))
	lens := make([]uint16, len(nameTokens))
	blob := make([]byte, 0, len(nameTokens)*8)
	refs := make(map[string]uint32, max(1, len(nameTokens)/2))
	lengths := make(map[string]uint16, max(1, len(nameTokens)/2))
	for i, name := range nameTokens {
		lower := strings.ToLower(name)
		if len(lower) > int(^uint16(0)) {
			lower = lower[:int(^uint16(0))]
		}
		lens[i] = uint16(len(lower))
		if lower == name {
			offs[i] = packedLowerSameAsName
			continue
		}
		if off, ok := refs[lower]; ok {
			offs[i] = off
			lens[i] = lengths[lower]
			continue
		}
		off := uint32(len(blob))
		refs[lower] = off
		lengths[lower] = uint16(len(lower))
		offs[i] = off
		blob = append(blob, lower...)
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(nameTokens)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(blob)))
	_ = binary.Write(&buf, binary.LittleEndian, offs)
	_ = binary.Write(&buf, binary.LittleEndian, lens)
	_, _ = buf.Write(blob)
	return buf.Bytes()
}

func encodeAttrPostingSection(attrBits map[uint32][]uint32) []byte {
	if len(attrBits) == 0 {
		return nil
	}
	parts := make([][]uint32, 0, len(queryAttrBits()))
	hasAny := false
	for _, bit := range queryAttrBits() {
		ids := uniqueSortedUint32s(append([]uint32(nil), attrBits[bit]...))
		if len(ids) > 0 {
			hasAny = true
		}
		parts = append(parts, ids)
	}
	if !hasAny {
		return nil
	}
	return encodeUint32Section(parts...)
}

func decodeAttrPostingSection(data []byte) map[uint32][]uint32 {
	parts := decodeUint32Section(data, len(queryAttrBits()))
	if len(parts) != len(queryAttrBits()) {
		return nil
	}
	out := make(map[uint32][]uint32, len(parts))
	for i, bit := range queryAttrBits() {
		if len(parts[i]) > 0 {
			out[bit] = parts[i]
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func buildStringPostingRankBounds(postings map[string][]uint32, nameRank, sizeRank, modRank, extRank, typeRank, pathRank []uint32) postingRankBounds {
	if len(sizeRank) == 0 {
		sizeRank = nameRank
	}
	if len(modRank) == 0 {
		modRank = nameRank
	}
	if len(extRank) == 0 {
		extRank = nameRank
	}
	if len(typeRank) == 0 {
		typeRank = nameRank
	}
	if len(pathRank) == 0 {
		pathRank = nameRank
	}
	if len(postings) == 0 || len(nameRank) == 0 || len(sizeRank) == 0 || len(modRank) == 0 || len(extRank) == 0 || len(typeRank) == 0 || len(pathRank) == 0 {
		return postingRankBounds{}
	}
	keys := make([]string, 0, len(postings))
	for key, ids := range postings {
		if key != "" && len(ids) > 0 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var bounds postingRankBounds
	for _, key := range keys {
		if len(key) > int(^uint16(0)) {
			continue
		}
		ids := uniqueSortedUint32s(append([]uint32(nil), postings[key]...))
		if len(ids) == 0 {
			continue
		}
		appendPostingRankBounds(&bounds, ids, nameRank, sizeRank, modRank, extRank, typeRank, pathRank)
	}
	bounds.BlockCount = len(bounds.Size)
	if bounds.BlockCount == 0 {
		return postingRankBounds{}
	}
	return bounds
}

func appendPostingRankBounds(bounds *postingRankBounds, ids []uint32, nameRank, sizeRank, modRank, extRank, typeRank, pathRank []uint32) {
	const blockSize = 1024
	for start := 0; start < len(ids); start += blockSize {
		end := min(len(ids), start+blockSize)
		chunk := ids[start:end]
		bounds.Name = append(bounds.Name, minRankForIDs(chunk, nameRank))
		bounds.Size = append(bounds.Size, minRankForIDs(chunk, sizeRank))
		bounds.Modified = append(bounds.Modified, minRankForIDs(chunk, modRank))
		bounds.Extension = append(bounds.Extension, minRankForIDs(chunk, extRank))
		bounds.Type = append(bounds.Type, minRankForIDs(chunk, typeRank))
		bounds.Path = append(bounds.Path, minRankForIDs(chunk, pathRank))
	}
}

// buildComponentPostingRankBounds stores the best possible descendant rank
// for each PCMP block. A component posting contains directory roots, so using
// the root's own rank would be unsound; the subtree minima computed from SUBT
// are the conservative bounds required for safe block skipping.
func buildComponentPostingRankBounds(postings map[string][]uint32, vol *serviceVolumeIndex) postingRankBounds {
	if vol == nil || len(postings) == 0 || len(vol.subtreeSizeRank) == 0 || len(vol.subtreeModRank) == 0 ||
		len(vol.subtreeExtRank) == 0 || len(vol.subtreeTypeRank) == 0 || len(vol.subtreePathRank) == 0 {
		return postingRankBounds{}
	}
	keys := make([]string, 0, len(postings))
	for key, ids := range postings {
		if key != "" && len(ids) > 0 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var bounds postingRankBounds
	for _, key := range keys {
		ids := uniqueSortedUint32s(append([]uint32(nil), postings[key]...))
		for start := 0; start < len(ids); start += 1024 {
			end := min(len(ids), start+1024)
			chunk := ids[start:end]
			bounds.Name = append(bounds.Name, vol.minSubtreeRankForRoots(chunk, vol.queryIndex.nameRank))
			bounds.Size = append(bounds.Size, minRankForIDs(chunk, vol.subtreeSizeRank))
			bounds.Modified = append(bounds.Modified, minRankForIDs(chunk, vol.subtreeModRank))
			bounds.Extension = append(bounds.Extension, minRankForIDs(chunk, vol.subtreeExtRank))
			bounds.Type = append(bounds.Type, minRankForIDs(chunk, vol.subtreeTypeRank))
			bounds.Path = append(bounds.Path, minRankForIDs(chunk, vol.subtreePathRank))
		}
	}
	bounds.BlockCount = len(bounds.Size)
	if bounds.BlockCount == 0 {
		return postingRankBounds{}
	}
	return bounds
}

func (vol *serviceVolumeIndex) minSubtreeRankForRoots(roots []uint32, ranks []uint32) uint32 {
	best := uint32(^uint32(0))
	if vol == nil || len(vol.subtreeOrder) == 0 {
		return best
	}
	for _, root := range roots {
		if int(root) >= len(vol.subtreeStart) || int(root) >= len(vol.subtreeEnd) {
			continue
		}
		start, end := vol.subtreeStart[root], vol.subtreeEnd[root]
		if start == ^uint32(0) || start > end || int(end) > len(vol.subtreeOrder) {
			continue
		}
		if rank := minRankForIDs(vol.subtreeOrder[start:end], ranks); rank < best {
			best = rank
		}
	}
	if best == uint32(^uint32(0)) {
		return 0
	}
	return best
}

func minRankForIDs(ids []uint32, ranks []uint32) uint32 {
	minRank := uint32(^uint32(0))
	for _, id := range ids {
		rank := extRankOf(id, ranks)
		if rank < minRank {
			minRank = rank
		}
	}
	if minRank == uint32(^uint32(0)) {
		return 0
	}
	return minRank
}

func encodePostingRankBounds(bounds postingRankBounds) []byte {
	if bounds.BlockCount <= 0 || len(bounds.Name) != bounds.BlockCount || len(bounds.Size) != bounds.BlockCount || len(bounds.Modified) != bounds.BlockCount ||
		len(bounds.Extension) != bounds.BlockCount || len(bounds.Type) != bounds.BlockCount || len(bounds.Path) != bounds.BlockCount {
		return nil
	}
	return encodeUint32Section(bounds.Name, bounds.Size, bounds.Modified, bounds.Extension, bounds.Type, bounds.Path)
}

func decodePostingRankBounds(data []byte) postingRankBounds {
	parts := decodeUint32Section(data, 6)
	legacy := false
	if len(parts) != 6 {
		parts = decodeUint32Section(data, 5)
		legacy = true
	}
	if (len(parts) != 6 && len(parts) != 5) || len(parts[0]) == 0 {
		return postingRankBounds{}
	}
	blockCount := len(parts[0])
	for _, part := range parts[1:] {
		if len(part) != blockCount {
			return postingRankBounds{}
		}
	}
	if legacy {
		return postingRankBounds{BlockCount: blockCount, Size: parts[0], Modified: parts[1], Extension: parts[2], Type: parts[3], Path: parts[4]}
	}
	return postingRankBounds{BlockCount: blockCount, Name: parts[0], Size: parts[1], Modified: parts[2], Extension: parts[3], Type: parts[4], Path: parts[5]}
}

type postingBlockMeta struct {
	offset  uint64
	length  uint32
	count   uint32
	minID   uint32
	maxID   uint32
	minRank uint32
}

type stringPostingEntry struct {
	keyOff     uint32
	keyLen     uint16
	count      uint32
	firstBlock uint32
	blockCount uint32
}

type gramPostingEntry struct {
	key        uint32
	count      uint32
	firstBlock uint32
	blockCount uint32
}

func encodeStringPostingSection(postings map[string][]uint32, ranks []uint32) []byte {
	if len(postings) == 0 {
		return nil
	}
	keys := make([]string, 0, len(postings))
	for key, ids := range postings {
		if key != "" && len(ids) > 0 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	entries := make([]stringPostingEntry, 0, len(keys))
	var keyBlob bytes.Buffer
	var blockBlob bytes.Buffer
	blocks := make([]postingBlockMeta, 0, len(keys))
	for _, key := range keys {
		if len(key) > int(^uint16(0)) {
			continue
		}
		ids := uniqueSortedUint32s(append([]uint32(nil), postings[key]...))
		if len(ids) == 0 {
			continue
		}
		entry := stringPostingEntry{
			keyOff:     uint32(keyBlob.Len()),
			keyLen:     uint16(len(key)),
			count:      uint32(len(ids)),
			firstBlock: uint32(len(blocks)),
		}
		_, _ = keyBlob.WriteString(key)
		blocks = appendPostingBlocks(blocks, &blockBlob, ids, ranks)
		entry.blockCount = uint32(len(blocks)) - entry.firstBlock
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return nil
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(entries)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(keyBlob.Len()))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(blocks)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(blockBlob.Len()))
	for _, entry := range entries {
		_ = binary.Write(&buf, binary.LittleEndian, entry.keyOff)
		_ = binary.Write(&buf, binary.LittleEndian, entry.keyLen)
		_ = binary.Write(&buf, binary.LittleEndian, uint16(0))
		_ = binary.Write(&buf, binary.LittleEndian, entry.count)
		_ = binary.Write(&buf, binary.LittleEndian, entry.firstBlock)
		_ = binary.Write(&buf, binary.LittleEndian, entry.blockCount)
	}
	writePostingBlockMetas(&buf, blocks)
	_, _ = buf.Write(keyBlob.Bytes())
	_, _ = buf.Write(blockBlob.Bytes())
	return buf.Bytes()
}

func encodeGramPostingSection(ti *compressedTrigramIndex, ranks []uint32) []byte {
	return encodeGramPostingSectionWithMetadata(ti, ranks, gramPostingMetadataMagic)
}

func encodeGramPostingSectionWithMetadata(ti *compressedTrigramIndex, ranks []uint32, metadataMagic uint32) []byte {
	if ti == nil {
		return nil
	}
	keys := make([]uint32, 0)
	ti.forEachCount(func(gram uint32, count int) {
		if count > 0 {
			keys = append(keys, gram)
		}
	})
	sortUint32s(keys)
	entries := make([]gramPostingEntry, 0, len(keys))
	var blockBlob bytes.Buffer
	blocks := make([]postingBlockMeta, 0, len(keys))
	for _, key := range keys {
		ids := trigramPostingIDs(ti, key)
		if len(ids) == 0 {
			continue
		}
		entry := gramPostingEntry{
			key:        key,
			count:      uint32(len(ids)),
			firstBlock: uint32(len(blocks)),
		}
		blocks = appendPostingBlocks(blocks, &blockBlob, ids, ranks)
		entry.blockCount = uint32(len(blocks)) - entry.firstBlock
		entries = append(entries, entry)
	}
	omitted := make([]uint32, 0, len(ti.omitted))
	for gram := range ti.omitted {
		omitted = append(omitted, gram)
	}
	sortUint32s(omitted)
	if len(entries) == 0 && len(omitted) == 0 {
		return nil
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(entries)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(blocks)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(blockBlob.Len()))
	for _, entry := range entries {
		_ = binary.Write(&buf, binary.LittleEndian, entry.key)
		_ = binary.Write(&buf, binary.LittleEndian, entry.count)
		_ = binary.Write(&buf, binary.LittleEndian, entry.firstBlock)
		_ = binary.Write(&buf, binary.LittleEndian, entry.blockCount)
	}
	writePostingBlockMetas(&buf, blocks)
	_, _ = buf.Write(blockBlob.Bytes())
	if ti.gramCountsComplete {
		_ = binary.Write(&buf, binary.LittleEndian, metadataMagic)
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(omitted)))
		for _, gram := range omitted {
			_ = binary.Write(&buf, binary.LittleEndian, gram)
			_ = binary.Write(&buf, binary.LittleEndian, uint32(ti.countForGram(gram)))
		}
	}
	return buf.Bytes()
}

func appendPostingBlocks(blocks []postingBlockMeta, blob *bytes.Buffer, ids []uint32, ranks []uint32) []postingBlockMeta {
	const blockSize = 1024
	for start := 0; start < len(ids); start += blockSize {
		end := min(len(ids), start+blockSize)
		chunk := ids[start:end]
		encoded := encodeDeltaUvarint32(chunk)
		offset := uint64(blob.Len())
		_, _ = blob.Write(encoded)
		minRank := uint32(^uint32(0))
		for _, id := range chunk {
			rank := extRankOf(id, ranks)
			if rank < minRank {
				minRank = rank
			}
		}
		if minRank == uint32(^uint32(0)) {
			minRank = 0
		}
		blocks = append(blocks, postingBlockMeta{
			offset:  offset,
			length:  uint32(len(encoded)),
			count:   uint32(len(chunk)),
			minID:   chunk[0],
			maxID:   chunk[len(chunk)-1],
			minRank: minRank,
		})
	}
	return blocks
}

func writePostingBlockMetas(buf *bytes.Buffer, blocks []postingBlockMeta) {
	for _, block := range blocks {
		_ = binary.Write(buf, binary.LittleEndian, block.offset)
		_ = binary.Write(buf, binary.LittleEndian, block.length)
		_ = binary.Write(buf, binary.LittleEndian, block.count)
		_ = binary.Write(buf, binary.LittleEndian, block.minID)
		_ = binary.Write(buf, binary.LittleEndian, block.maxID)
		_ = binary.Write(buf, binary.LittleEndian, block.minRank)
	}
}

func trigramPostingIDs(ti *compressedTrigramIndex, gram uint32) []uint32 {
	if ti == nil {
		return nil
	}
	var out []uint32
	for _, segment := range ti.segments {
		posting := segment.postingForGram(gram)
		if posting.count == 0 {
			continue
		}
		out = append(out, decodeDeltaUvarint32(ti.postingData(posting), posting.count)...)
	}
	if len(out) == 0 {
		return nil
	}
	sortUint32s(out)
	return uniqueSortedUint32s(out)
}

func (vol *serviceVolumeIndex) buildSubtreeRanges() {
	if vol == nil || vol.index == nil || len(vol.childOffsets) == 0 {
		return
	}
	recordCount := vol.index.compactRecordCount()
	start := make([]uint32, recordCount)
	end := make([]uint32, recordCount)
	for i := range start {
		start[i] = ^uint32(0)
	}
	order := make([]uint32, 0, recordCount)
	seen := make([]bool, recordCount)
	var walk func(int)
	walk = func(id int) {
		if id < 0 || id >= recordCount || seen[id] {
			return
		}
		seen[id] = true
		start[id] = uint32(len(order))
		order = append(order, uint32(id))
		for _, childID := range vol.childIDsForRecord(id) {
			walk(int(childID))
		}
		end[id] = uint32(len(order))
	}
	for _, rootID := range vol.rootIDs {
		walk(int(rootID))
	}
	for id := 0; id < recordCount; id++ {
		if !seen[id] {
			walk(id)
		}
	}
	vol.subtreeOrder = order
	vol.subtreeStart = start
	vol.subtreeEnd = end
}

// buildSubtreeMinRanks computes a conservative best rank for every subtree.
// The persisted subtree order is depth-first, so children are visited before
// their parent when walking it backwards. Deleted records contribute no rank;
// this keeps block-max skips safe when a subtree contains tombstoned records.
func (vol *serviceVolumeIndex) buildSubtreeMinRanks(ranks []uint32) []uint32 {
	if vol == nil || vol.index == nil || len(ranks) < vol.index.compactRecordCount() ||
		len(vol.subtreeOrder) == 0 {
		return nil
	}
	best := make([]uint32, vol.index.compactRecordCount())
	for i := range best {
		best[i] = ^uint32(0)
	}
	for pos := len(vol.subtreeOrder) - 1; pos >= 0; pos-- {
		id := int(vol.subtreeOrder[pos])
		if id < 0 || id >= len(best) {
			continue
		}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted {
			best[id] = ranks[id]
		}
		for _, childID32 := range vol.childIDsForRecord(id) {
			childID := int(childID32)
			if childID >= 0 && childID < len(best) && best[childID] < best[id] {
				best[id] = best[childID]
			}
		}
	}
	return best
}

func loadIndex(path string) (*Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return readIndexWithReaderAt(bufio.NewReaderSize(f, 16*1024*1024), f, info.Size())
}

func loadIndexMMap(path string) (*Index, error) {
	mapped, err := mapIndexFile(path)
	if err != nil {
		return nil, err
	}
	idx, err := readIndexMMap(mapped)
	if err != nil {
		_ = mapped.close()
		return nil, err
	}
	idx.DBPath = path
	return idx, nil
}

func readIndexMMap(mapped *mappedIndexFile) (*Index, error) {
	if mapped == nil || len(mapped.data) < binary.Size(diskHeader{}) {
		return nil, errors.New("invalid mapped index")
	}
	data := mapped.data
	headerSize := binary.Size(diskHeader{})
	var magic [8]byte
	copy(magic[:], data[:8])
	header := diskHeader{
		Magic:       magic,
		Version:     binary.LittleEndian.Uint32(data[8:]),
		EntryCount:  binary.LittleEndian.Uint64(data[12:]),
		RootCount:   binary.LittleEndian.Uint64(data[20:]),
		BuiltUnix:   int64(binary.LittleEndian.Uint64(data[28:])),
		JournalID:   binary.LittleEndian.Uint64(data[36:]),
		Checkpoint:  int64(binary.LittleEndian.Uint64(data[44:])),
		Compact:     binary.LittleEndian.Uint32(data[52:]),
		NameBlobLen: binary.LittleEndian.Uint64(data[56:]),
		TokenCount:  binary.LittleEndian.Uint64(data[64:]),
	}
	sectionTableOffset := uint64(0)
	if header.Magic == indexMagicV9 {
		if len(data) < headerSize+8 {
			return nil, errors.New("invalid mapped v9 index")
		}
		sectionTableOffset = binary.LittleEndian.Uint64(data[headerSize:])
		headerSize += 8
	} else if header.Magic != indexMagic {
		return nil, errors.New("unsupported index format")
	}
	if header.Version != indexVersion && header.Version != indexVersionV9 {
		return nil, fmt.Errorf("unsupported index version %d", header.Version)
	}
	if header.Compact == 0 {
		return nil, errors.New("mmap low-memory mode requires compact index")
	}
	if header.EntryCount > uint64(^uint(0)>>1) || header.RootCount > uint64(^uint(0)>>1) ||
		header.NameBlobLen > uint64(^uint(0)>>1) || header.TokenCount > uint64(^uint(0)>>1) {
		return nil, errors.New("index too large")
	}
	off := headerSize
	idx := &Index{
		Version:    int(header.Version),
		BuiltAt:    time.Unix(0, header.BuiltUnix),
		Roots:      make([]string, int(header.RootCount)),
		JournalID:  header.JournalID,
		Checkpoint: header.Checkpoint,
		Compact:    true,
	}
	var err error
	if idx.Source, off, err = mappedReadString(data, off); err != nil {
		return nil, err
	}
	if idx.Volume, off, err = mappedReadString(data, off); err != nil {
		return nil, err
	}
	if idx.ContentHash, off, err = mappedReadString(data, off); err != nil {
		return nil, err
	}
	for i := range idx.Roots {
		if idx.Roots[i], off, err = mappedReadString(data, off); err != nil {
			return nil, err
		}
	}
	nameBlobLen := int(header.NameBlobLen)
	if off+nameBlobLen < off || off+nameBlobLen > len(data) {
		return nil, errors.New("invalid mapped name blob")
	}
	nameBlob := data[off : off+nameBlobLen]
	off += nameBlobLen
	tokenBytes := int(header.TokenCount) * 6
	if tokenBytes/6 != int(header.TokenCount) || off+tokenBytes < off || off+tokenBytes > len(data) {
		return nil, errors.New("invalid mapped name table")
	}
	tokenTable := data[off : off+tokenBytes]
	off += tokenBytes
	recordCount := int(header.EntryCount)
	wideRefs := header.Compact&compactDiskWideRefsFlag != 0
	idx.CompactAttrs = header.Compact&compactDiskAttrsFlag != 0
	recordBytes := compactDiskRecordBytesForCounts(recordCount, int(header.TokenCount))
	needRecordBytes := recordCount * recordBytes
	if recordBytes <= 0 || needRecordBytes/recordBytes != recordCount ||
		off+needRecordBytes < off || off+needRecordBytes > len(data) {
		return nil, errors.New("invalid mapped compact records")
	}
	mappedRecords := &MMapRecords{
		file:       mapped,
		wideRefs:   wideRefs,
		count:      recordCount,
		nameBlob:   nameBlob,
		tokenTable: tokenTable,
		recordData: data[off : off+needRecordBytes],
	}
	mappedRecords.scanCapabilities()
	idx.MMapRecords = mappedRecords
	if sectionTableOffset != 0 {
		idx.Derived = parseMappedDerivedSections(data, sectionTableOffset, int(header.EntryCount))
		mapped.derived = idx.Derived
	}
	return idx, nil
}

func parseMappedDerivedSections(data []byte, sectionTableOffset uint64, recordCount int) indexDerivedSections {
	var out indexDerivedSections
	if sectionTableOffset > uint64(len(data)) || sectionTableOffset+4 > uint64(len(data)) {
		return out
	}
	off := int(sectionTableOffset)
	count := int(binary.LittleEndian.Uint32(data[off:]))
	off += 4
	const entrySize = 24
	if count < 0 || off+count*entrySize < off || off+count*entrySize > len(data) {
		return out
	}
	for i := 0; i < count; i++ {
		tag := binary.LittleEndian.Uint32(data[off:])
		sectionOff := binary.LittleEndian.Uint64(data[off+4:])
		length := binary.LittleEndian.Uint64(data[off+12:])
		off += entrySize
		if sectionOff > uint64(len(data)) || length > uint64(len(data)) || sectionOff+length < sectionOff || sectionOff+length > uint64(len(data)) {
			continue
		}
		section := data[int(sectionOff):int(sectionOff+length)]
		decodeDerivedSection(&out, tag, section, recordCount)
	}
	finalizeDerivedSections(&out)
	return out
}

func readDerivedSectionsFromReaderAt(ra io.ReaderAt, size int64, sectionTableOffset uint64, recordCount int) indexDerivedSections {
	var out indexDerivedSections
	if ra == nil || size < 0 || sectionTableOffset == 0 {
		return out
	}
	fileSize := uint64(size)
	if sectionTableOffset > fileSize || sectionTableOffset+4 < sectionTableOffset || sectionTableOffset+4 > fileSize {
		return out
	}
	var countBuf [4]byte
	if _, err := ra.ReadAt(countBuf[:], int64(sectionTableOffset)); err != nil {
		return out
	}
	count := int(binary.LittleEndian.Uint32(countBuf[:]))
	const entrySize = 24
	tableBytes := count * entrySize
	tableOff := sectionTableOffset + 4
	if count < 0 || tableBytes/entrySize != count || tableOff+uint64(tableBytes) < tableOff || tableOff+uint64(tableBytes) > fileSize {
		return out
	}
	table := make([]byte, tableBytes)
	if _, err := ra.ReadAt(table, int64(tableOff)); err != nil {
		return out
	}
	for off := 0; off < len(table); off += entrySize {
		tag := binary.LittleEndian.Uint32(table[off:])
		sectionOff := binary.LittleEndian.Uint64(table[off+4:])
		length := binary.LittleEndian.Uint64(table[off+12:])
		if sectionOff > fileSize || length > fileSize || sectionOff+length < sectionOff || sectionOff+length > fileSize {
			continue
		}
		if length > uint64(^uint(0)>>1) {
			continue
		}
		section := make([]byte, int(length))
		if _, err := ra.ReadAt(section, int64(sectionOff)); err != nil {
			continue
		}
		decodeDerivedSection(&out, tag, section, recordCount)
	}
	finalizeDerivedSections(&out)
	return out
}

func decodeDerivedSection(out *indexDerivedSections, tag uint32, section []byte, recordCount int) {
	if out == nil {
		return
	}
	switch tag {
	case indexSectionRANK:
		parts := decodeUint32Section(section, 2)
		if len(parts) == 2 {
			out.NameOrder, out.NameRank = parts[0], parts[1]
		}
	case indexSectionSRNK:
		parts := decodeUint32Section(section, 2)
		if len(parts) == 2 {
			out.SizeOrder, out.SizeRank = parts[0], parts[1]
		}
	case indexSectionMRNK:
		parts := decodeUint32Section(section, 2)
		if len(parts) == 2 {
			out.ModOrder, out.ModRank = parts[0], parts[1]
		}
	case indexSectionERNK:
		parts := decodeUint32Section(section, 2)
		if len(parts) == 2 {
			out.ExtOrder, out.ExtRank = parts[0], parts[1]
		}
	case indexSectionTRNK:
		parts := decodeUint32Section(section, 2)
		if len(parts) == 2 {
			out.TypeOrder, out.TypeRank = parts[0], parts[1]
		}
	case indexSectionPRNK:
		parts := decodeUint32Section(section, 2)
		if len(parts) == 2 {
			out.PathOrder, out.PathRank = parts[0], parts[1]
		}
	case indexSectionCHLD:
		parts := decodeUint32Section(section, 3)
		if len(parts) == 3 {
			out.ChildOffsets, out.ChildIDs, out.RootIDs = parts[0], parts[1], parts[2]
		}
	case indexSectionSUBT:
		parts := decodeUint32Section(section, 8)
		if len(parts) == 8 {
			out.SubtreeStart, out.SubtreeEnd, out.SubtreeOrder = parts[0], parts[1], parts[2]
			out.SubtreeSizeRank, out.SubtreeModRank = parts[3], parts[4]
			out.SubtreeExtRank, out.SubtreeTypeRank = parts[5], parts[6]
			out.SubtreePathRank = parts[7]
			break
		}
		parts = decodeUint32Section(section, 3)
		if len(parts) == 3 {
			out.SubtreeStart, out.SubtreeEnd, out.SubtreeOrder = parts[0], parts[1], parts[2]
		}
	case indexSectionFRNS:
		frns, ids := decodeFRNSection(section)
		out.FRNs, out.FRNRecordIDs = frns, ids
	case indexSectionLOWR:
		out.LowerBlob, out.LowerOffs, out.LowerLens = decodeLowerSection(section)
	case indexSectionPATR:
		out.AttrBits = decodeAttrPostingSection(section)
	case indexSectionPEXT, indexSectionPCMP, indexSectionPNGR, indexSectionPNGC:
		if out.Postings == nil {
			out.Postings = make(map[uint32]mappedPostingSection)
		}
		out.Postings[tag] = decodePostingSection(section)
		if tag == indexSectionPNGR {
			out.NameTrigrams = decodeGramPostingIndex(section, recordCount)
		} else if tag == indexSectionPNGC {
			out.SelfNameTrigrams = decodeGramPostingIndex(section, recordCount)
		}
	case indexSectionPXRB:
		if out.PostingBounds == nil {
			out.PostingBounds = make(map[uint32]postingRankBounds)
		}
		out.PostingBounds[indexSectionPEXT] = decodePostingRankBounds(section)
	case indexSectionPXRC:
		if out.PostingBounds == nil {
			out.PostingBounds = make(map[uint32]postingRankBounds)
		}
		out.PostingBounds[indexSectionPCMP] = decodePostingRankBounds(section)
	}
}

func finalizeDerivedSections(out *indexDerivedSections) {
	if out == nil {
		return
	}
	for tag, bounds := range out.PostingBounds {
		if bounds.BlockCount <= 0 || out.Postings == nil {
			continue
		}
		if posting, ok := out.Postings[tag]; ok {
			posting.RankBounds = bounds
			out.Postings[tag] = posting
		}
	}
}

func decodeUint32Section(data []byte, parts int) [][]uint32 {
	out := make([][]uint32, 0, parts)
	off := 0
	for i := 0; i < parts; i++ {
		if off+4 > len(data) {
			return nil
		}
		n := int(binary.LittleEndian.Uint32(data[off:]))
		off += 4
		bytesLen := n * 4
		if n < 0 || bytesLen/4 != n || off+bytesLen < off || off+bytesLen > len(data) {
			return nil
		}
		values := mappedUint32Slice(data[off : off+bytesLen])
		off += bytesLen
		out = append(out, values)
	}
	if off != len(data) {
		return nil
	}
	return out
}

func decodeFRNSection(data []byte) ([]uint64, []uint32) {
	off := 0
	if off+4 > len(data) {
		return nil, nil
	}
	frnCount := int(binary.LittleEndian.Uint32(data[off:]))
	off += 4
	frnBytes := frnCount * 8
	if frnCount < 0 || frnBytes/8 != frnCount || off+frnBytes < off || off+frnBytes > len(data) {
		return nil, nil
	}
	frns := mappedUint64Slice(data[off : off+frnBytes])
	off += frnBytes
	if off+4 > len(data) {
		return nil, nil
	}
	idCount := int(binary.LittleEndian.Uint32(data[off:]))
	off += 4
	idBytes := idCount * 4
	if idCount < 0 || idBytes/4 != idCount || idCount != frnCount || off+idBytes < off || off+idBytes > len(data) {
		return nil, nil
	}
	ids := mappedUint32Slice(data[off : off+idBytes])
	return frns, ids
}

func decodeLowerSection(data []byte) ([]byte, []uint32, []uint16) {
	if len(data) < 8 {
		return nil, nil, nil
	}
	count := int(binary.LittleEndian.Uint32(data[0:]))
	blobLen := int(binary.LittleEndian.Uint32(data[4:]))
	off := 8
	offsBytes := count * 4
	if count < 0 || blobLen < 0 || offsBytes/4 != count || off+offsBytes < off || off+offsBytes > len(data) {
		return nil, nil, nil
	}
	offs := mappedUint32Slice(data[off : off+offsBytes])
	off += offsBytes
	lensBytes := count * 2
	if lensBytes/2 != count || off+lensBytes < off || off+lensBytes > len(data) {
		return nil, nil, nil
	}
	lens := mappedUint16Slice(data[off : off+lensBytes])
	off += lensBytes
	if off+blobLen < off || off+blobLen > len(data) {
		return nil, nil, nil
	}
	return data[off : off+blobLen], offs, lens
}

func mappedUint16Slice(data []byte) []uint16 {
	if len(data) == 0 {
		return nil
	}
	return unsafe.Slice((*uint16)(unsafe.Pointer(&data[0])), len(data)/2)
}

func mappedUint32Slice(data []byte) []uint32 {
	if len(data) == 0 {
		return nil
	}
	return unsafe.Slice((*uint32)(unsafe.Pointer(&data[0])), len(data)/4)
}

func mappedUint64Slice(data []byte) []uint64 {
	if len(data) == 0 {
		return nil
	}
	return unsafe.Slice((*uint64)(unsafe.Pointer(&data[0])), len(data)/8)
}

func decodePostingSection(data []byte) mappedPostingSection {
	if len(data) < 16 {
		return mappedPostingSection{}
	}
	entryCount := int(binary.LittleEndian.Uint32(data[0:]))
	keyBlobLen := int(binary.LittleEndian.Uint32(data[4:]))
	blockCount := int(binary.LittleEndian.Uint32(data[8:]))
	blockBlobLen := int(binary.LittleEndian.Uint32(data[12:]))
	if entryCount < 0 || keyBlobLen < 0 || blockCount < 0 || blockBlobLen < 0 {
		return mappedPostingSection{}
	}
	stringEntrySize := 20
	gramEntrySize := 16
	entrySize := stringEntrySize
	if keyBlobLen == 0 {
		entrySize = gramEntrySize
	}
	blockMetaSize := 28
	off := 16
	entriesBytes := entryCount * entrySize
	if entriesBytes/entrySize != entryCount || off+entriesBytes < off || off+entriesBytes > len(data) {
		return mappedPostingSection{}
	}
	off += entriesBytes
	blockMetaBytes := blockCount * blockMetaSize
	if blockMetaBytes/blockMetaSize != blockCount || off+blockMetaBytes < off || off+blockMetaBytes > len(data) {
		return mappedPostingSection{}
	}
	off += blockMetaBytes
	if off+keyBlobLen < off || off+keyBlobLen > len(data) {
		return mappedPostingSection{}
	}
	off += keyBlobLen
	if off+blockBlobLen < off || off+blockBlobLen > len(data) {
		return mappedPostingSection{}
	}
	section := mappedPostingSection{EntryCount: entryCount, BlockCount: blockCount, Bytes: len(data), Data: data}
	if keyBlobLen == 0 {
		return section
	}
	for i := 0; i < entryCount; i++ {
		entryOff := 16 + i*stringEntrySize
		keyOff := int(binary.LittleEndian.Uint32(data[entryOff:]))
		keyLen := int(binary.LittleEndian.Uint16(data[entryOff+4:]))
		firstBlock := int(binary.LittleEndian.Uint32(data[entryOff+12:]))
		entryBlockCount := int(binary.LittleEndian.Uint32(data[entryOff+16:]))
		if keyOff < 0 || keyLen < 0 || keyOff+keyLen < keyOff || keyOff+keyLen > keyBlobLen {
			return mappedPostingSection{}
		}
		if firstBlock < 0 || entryBlockCount < 0 || firstBlock+entryBlockCount < firstBlock || firstBlock+entryBlockCount > blockCount {
			return mappedPostingSection{}
		}
	}
	return section
}

func postingBlockCacheMaxBytes() int64 {
	mb := int64(128)
	if raw := strings.TrimSpace(os.Getenv("SEEKFS_POSTING_CACHE_MB")); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
			mb = parsed
		}
	}
	if mb <= 0 {
		return 0
	}
	const maxReasonableMB = 16 * 1024
	if mb > maxReasonableMB {
		mb = maxReasonableMB
	}
	return mb * 1024 * 1024
}

func (c *postingBlockLRU) get(key postingBlockCacheKey) ([]uint32, bool) {
	maxBytes := postingBlockCacheMaxBytes()
	if maxBytes <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		return nil, false
	}
	elem := c.items[key]
	if elem == nil {
		return nil, false
	}
	c.ll.MoveToFront(elem)
	entry := elem.Value.(*postingBlockCacheEntry)
	return entry.ids, true
}

func (c *postingBlockLRU) add(key postingBlockCacheKey, ids []uint32) {
	maxBytes := postingBlockCacheMaxBytes()
	if maxBytes <= 0 || len(ids) == 0 {
		return
	}
	entryBytes := int64(len(ids)) * int64(unsafe.Sizeof(uint32(0)))
	if entryBytes > maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = make(map[postingBlockCacheKey]*list.Element)
	}
	if elem := c.items[key]; elem != nil {
		c.ll.MoveToFront(elem)
		entry := elem.Value.(*postingBlockCacheEntry)
		c.bytes += entryBytes - entry.bytes
		entry.ids = ids
		entry.bytes = entryBytes
	} else {
		elem := c.ll.PushFront(&postingBlockCacheEntry{key: key, ids: ids, bytes: entryBytes})
		c.items[key] = elem
		c.bytes += entryBytes
	}
	for c.bytes > maxBytes {
		elem := c.ll.Back()
		if elem == nil {
			break
		}
		entry := elem.Value.(*postingBlockCacheEntry)
		delete(c.items, entry.key)
		c.bytes -= entry.bytes
		c.ll.Remove(elem)
	}
}

func (section mappedPostingSection) postingBlockCacheKey(blockIndex int) (postingBlockCacheKey, bool) {
	if len(section.Data) == 0 || blockIndex < 0 {
		return postingBlockCacheKey{}, false
	}
	return postingBlockCacheKey{
		base:  uintptr(unsafe.Pointer(unsafe.SliceData(section.Data))),
		bytes: len(section.Data),
		block: blockIndex,
	}, true
}

func (section mappedPostingSection) decodePostingBlock(blockIndex, blockMetaStart, blockBlobStart, blockBlobLen int) ([]uint32, bool) {
	const blockMetaSize = 28
	data := section.Data
	metaOff := blockMetaStart + blockIndex*blockMetaSize
	if metaOff < blockMetaStart || metaOff+blockMetaSize < metaOff || metaOff+blockMetaSize > len(data) {
		return nil, false
	}
	blockOffset := int(binary.LittleEndian.Uint64(data[metaOff:]))
	blockLength := int(binary.LittleEndian.Uint32(data[metaOff+8:]))
	blockCountValue := int(binary.LittleEndian.Uint32(data[metaOff+12:]))
	if blockOffset < 0 || blockLength < 0 || blockOffset+blockLength < blockOffset || blockOffset+blockLength > blockBlobLen {
		return nil, false
	}
	if key, ok := section.postingBlockCacheKey(blockIndex); ok {
		if cached, ok := servicePostingBlockCache.get(key); ok {
			return cached, true
		}
		encoded := data[blockBlobStart+blockOffset : blockBlobStart+blockOffset+blockLength]
		decoded := decodeDeltaUvarint32(encoded, blockCountValue)
		servicePostingBlockCache.add(key, decoded)
		return decoded, true
	}
	encoded := data[blockBlobStart+blockOffset : blockBlobStart+blockOffset+blockLength]
	return decodeDeltaUvarint32(encoded, blockCountValue), true
}

type postingBlockIterator struct {
	section        mappedPostingSection
	next           int
	end            int
	blockMetaStart int
	blockBlobStart int
	blockBlobLen   int
}

type postingBlockRankRef struct {
	index int
	meta  postingBlockMeta
}

func (it *postingBlockIterator) nextBlock() ([]uint32, postingBlockMeta, bool) {
	if it == nil || it.next >= it.end {
		return nil, postingBlockMeta{}, false
	}
	blockIndex := it.next
	it.next++
	return it.blockAt(blockIndex)
}

func (it postingBlockIterator) blockAt(blockIndex int) ([]uint32, postingBlockMeta, bool) {
	meta, ok := it.blockMetaAt(blockIndex)
	if !ok {
		return nil, postingBlockMeta{}, false
	}
	ids, ok := it.section.decodePostingBlock(blockIndex, it.blockMetaStart, it.blockBlobStart, it.blockBlobLen)
	return ids, meta, ok
}

func (it postingBlockIterator) blockMetaAt(blockIndex int) (postingBlockMeta, bool) {
	const blockMetaSize = 28
	if blockIndex < 0 || blockIndex >= it.end {
		return postingBlockMeta{}, false
	}
	metaOff := it.blockMetaStart + blockIndex*blockMetaSize
	data := it.section.Data
	if metaOff < it.blockMetaStart || metaOff+blockMetaSize < metaOff || metaOff+blockMetaSize > len(data) {
		return postingBlockMeta{}, false
	}
	meta := postingBlockMeta{
		offset:  binary.LittleEndian.Uint64(data[metaOff:]),
		length:  binary.LittleEndian.Uint32(data[metaOff+8:]),
		count:   binary.LittleEndian.Uint32(data[metaOff+12:]),
		minID:   binary.LittleEndian.Uint32(data[metaOff+16:]),
		maxID:   binary.LittleEndian.Uint32(data[metaOff+20:]),
		minRank: binary.LittleEndian.Uint32(data[metaOff+24:]),
	}
	return meta, true
}

// containsID is a bounded SeekGE-style membership check.  Gram postings are
// stored in record-ID order, so binary-searching block bounds decodes at most
// one block and never materializes the posting or its intersection.
func (it postingBlockIterator) containsID(target uint32) (found, valid bool, blockIndex int, decoded bool) {
	if it.next >= it.end {
		return false, true, -1, false
	}
	lo, hi := it.next, it.end
	for lo < hi {
		mid := lo + (hi-lo)/2
		meta, ok := it.blockMetaAt(mid)
		if !ok {
			return false, false, -1, false
		}
		if target <= meta.maxID {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	if lo >= it.end {
		return false, true, -1, false
	}
	meta, ok := it.blockMetaAt(lo)
	if !ok {
		return false, false, -1, false
	}
	if target < meta.minID || target > meta.maxID {
		return false, true, lo, false
	}
	ids, _, ok := it.blockAt(lo)
	if !ok {
		return false, false, lo, false
	}
	pos := sort.Search(len(ids), func(i int) bool { return ids[i] >= target })
	return pos < len(ids) && ids[pos] == target, true, lo, true
}

type postingPrefetchRange struct {
	start int
	end   int
}

func queryPostingPrefetchBytes() int64 {
	raw := strings.TrimSpace(os.Getenv("SEEKFS_QUERY_POSTING_PREFETCH_BYTES"))
	if raw == "" {
		return defaultQueryPostingPrefetchBytes
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return defaultQueryPostingPrefetchBytes
	}
	return value
}

// prefetchPostingBlockRefs touches only the selected posting payload ranges,
// in physical order, before rank-ordered evaluation.  It intentionally has a
// hard byte cap and no allocation proportional to the mapped section.
func prefetchPostingBlockRefs(it postingBlockIterator, refs []postingBlockRankRef, maxBytes int64, canceled func() bool) (bytes, ranges, pages int, stopped bool) {
	if maxBytes <= 0 || len(refs) == 0 || len(it.section.Data) == 0 {
		return 0, 0, 0, false
	}
	physical := make([]postingPrefetchRange, 0, len(refs))
	for _, ref := range refs {
		meta, ok := it.blockMetaAt(ref.index)
		if !ok || meta.length == 0 {
			continue
		}
		if meta.offset > uint64(it.blockBlobLen) {
			return 0, 0, 0, true
		}
		start := it.blockBlobStart + int(meta.offset)
		end := start + int(meta.length)
		if start < it.blockBlobStart || end < start || end > it.blockBlobStart+it.blockBlobLen || end > len(it.section.Data) {
			return 0, 0, 0, true
		}
		physical = append(physical, postingPrefetchRange{start: start, end: end})
	}
	sort.Slice(physical, func(i, j int) bool {
		if physical[i].start == physical[j].start {
			return physical[i].end < physical[j].end
		}
		return physical[i].start < physical[j].start
	})
	for _, r := range physical {
		if canceled != nil && canceled() {
			return bytes, ranges, pages, true
		}
		if r.end <= r.start || int64(bytes) >= maxBytes {
			break
		}
		end := r.end
		remaining := maxBytes - int64(bytes)
		if int64(end-r.start) > remaining {
			end = r.start + int(remaining)
		}
		if end <= r.start {
			break
		}
		ranges++
		for off, touched := r.start, 0; off < end; off, touched = off+4096, touched+1 {
			_ = it.section.Data[off]
			pages++
			if touched&255 == 255 && canceled != nil && canceled() {
				return bytes + end - r.start, ranges, pages, true
			}
		}
		bytes += end - r.start
	}
	return bytes, ranges, pages, false
}

func (it postingBlockIterator) rankOrderedBlockRefs() []postingBlockRankRef {
	if it.next >= it.end {
		return nil
	}
	refs := make([]postingBlockRankRef, 0, it.end-it.next)
	for blockIndex := it.next; blockIndex < it.end; blockIndex++ {
		meta, ok := it.blockMetaAt(blockIndex)
		if !ok {
			return nil
		}
		refs = append(refs, postingBlockRankRef{index: blockIndex, meta: meta})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].meta.minRank == refs[j].meta.minRank {
			return refs[i].meta.minID < refs[j].meta.minID
		}
		return refs[i].meta.minRank < refs[j].meta.minRank
	})
	return refs
}

func (it postingBlockIterator) rankOrderedBlockRefsForSort(sortColumn string) ([]postingBlockRankRef, bool) {
	if sortColumn == "" {
		bounds := it.section.RankBounds.ranksForSort("")
		if len(bounds) >= it.end {
			refs := make([]postingBlockRankRef, 0, it.end-it.next)
			for blockIndex := it.next; blockIndex < it.end; blockIndex++ {
				meta, ok := it.blockMetaAt(blockIndex)
				if !ok {
					return nil, false
				}
				meta.minRank = bounds[blockIndex]
				refs = append(refs, postingBlockRankRef{index: blockIndex, meta: meta})
			}
			sort.Slice(refs, func(i, j int) bool {
				if refs[i].meta.minRank == refs[j].meta.minRank {
					return refs[i].meta.minID < refs[j].meta.minID
				}
				return refs[i].meta.minRank < refs[j].meta.minRank
			})
			return refs, true
		}
		return it.rankOrderedBlockRefs(), true
	}
	bounds := it.section.RankBounds.ranksForSort(sortColumn)
	if len(bounds) < it.end {
		return it.rankOrderedBlockRefs(), false
	}
	refs := make([]postingBlockRankRef, 0, it.end-it.next)
	for blockIndex := it.next; blockIndex < it.end; blockIndex++ {
		meta, ok := it.blockMetaAt(blockIndex)
		if !ok {
			return nil, false
		}
		meta.minRank = bounds[blockIndex]
		refs = append(refs, postingBlockRankRef{index: blockIndex, meta: meta})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].meta.minRank == refs[j].meta.minRank {
			return refs[i].meta.minID < refs[j].meta.minID
		}
		return refs[i].meta.minRank < refs[j].meta.minRank
	})
	return refs, true
}

func (bounds postingRankBounds) ranksForSort(sortColumn string) []uint32 {
	if bounds.BlockCount <= 0 {
		return nil
	}
	switch sortColumn {
	case "size":
		return bounds.Size
	case "modified":
		return bounds.Modified
	case "extension":
		return bounds.Extension
	case "type":
		return bounds.Type
	case "path":
		return bounds.Path
	default:
		return bounds.Name
	}
}

func (section mappedPostingSection) stringPosting(key string) []uint32 {
	it, count, ok := section.stringPostingIterator(key)
	if !ok {
		return nil
	}
	return materializePostingBlockIterator(it, count)
}

// matchingStringPostingKeys scans the complete sorted key dictionary without
// touching any posting blocks.  Callers can then decode only the postings for
// keys whose component names contain the requested substring.
func (section mappedPostingSection) matchingStringPostingKeys(term string) ([]string, bool) {
	data := section.Data
	if len(data) < 16 || term == "" {
		return nil, false
	}
	entryCount := int(binary.LittleEndian.Uint32(data[0:]))
	keyBlobLen := int(binary.LittleEndian.Uint32(data[4:]))
	blockCount := int(binary.LittleEndian.Uint32(data[8:]))
	blockBlobLen := int(binary.LittleEndian.Uint32(data[12:]))
	const stringEntrySize = 20
	const blockMetaSize = 28
	if entryCount <= 0 || keyBlobLen <= 0 || blockCount < 0 || blockBlobLen < 0 {
		return nil, false
	}
	entriesStart := 16
	entriesBytes := entryCount * stringEntrySize
	if entriesBytes/stringEntrySize != entryCount || entriesStart+entriesBytes < entriesStart || entriesStart+entriesBytes > len(data) {
		return nil, false
	}
	blockMetaStart := entriesStart + entriesBytes
	blockMetaBytes := blockCount * blockMetaSize
	if blockMetaBytes/blockMetaSize != blockCount || blockMetaStart+blockMetaBytes < blockMetaStart || blockMetaStart+blockMetaBytes > len(data) {
		return nil, false
	}
	keyBlobStart := blockMetaStart + blockMetaBytes
	blockBlobStart := keyBlobStart + keyBlobLen
	if blockBlobStart < keyBlobStart || blockBlobStart > len(data) || blockBlobStart+blockBlobLen < blockBlobStart || blockBlobStart+blockBlobLen > len(data) {
		return nil, false
	}
	term = strings.ToLower(term)
	keys := make([]string, 0, 8)
	for i := 0; i < entryCount; i++ {
		entryOff := entriesStart + i*stringEntrySize
		keyOff := int(binary.LittleEndian.Uint32(data[entryOff:]))
		keyLen := int(binary.LittleEndian.Uint16(data[entryOff+4:]))
		if keyOff < 0 || keyLen < 0 || keyOff+keyLen < keyOff || keyOff+keyLen > keyBlobLen {
			return nil, false
		}
		key := stringView(data[keyBlobStart+keyOff : keyBlobStart+keyOff+keyLen])
		if !strings.Contains(key, term) {
			continue
		}
		if _, _, ok := section.stringPostingIterator(key); !ok {
			return nil, false
		}
		keys = append(keys, key)
	}
	return keys, true
}

func (section mappedPostingSection) stringPostingIterator(key string) (postingBlockIterator, int, bool) {
	data := section.Data
	if key == "" || len(data) < 16 {
		return postingBlockIterator{}, 0, false
	}
	entryCount := int(binary.LittleEndian.Uint32(data[0:]))
	keyBlobLen := int(binary.LittleEndian.Uint32(data[4:]))
	blockCount := int(binary.LittleEndian.Uint32(data[8:]))
	blockBlobLen := int(binary.LittleEndian.Uint32(data[12:]))
	if entryCount <= 0 || keyBlobLen <= 0 || blockCount < 0 || blockBlobLen < 0 {
		return postingBlockIterator{}, 0, false
	}
	const stringEntrySize = 20
	const blockMetaSize = 28
	entriesStart := 16
	entriesBytes := entryCount * stringEntrySize
	if entriesBytes/stringEntrySize != entryCount || entriesStart+entriesBytes < entriesStart || entriesStart+entriesBytes > len(data) {
		return postingBlockIterator{}, 0, false
	}
	blockMetaStart := entriesStart + entriesBytes
	blockMetaBytes := blockCount * blockMetaSize
	if blockMetaBytes/blockMetaSize != blockCount || blockMetaStart+blockMetaBytes < blockMetaStart || blockMetaStart+blockMetaBytes > len(data) {
		return postingBlockIterator{}, 0, false
	}
	keyBlobStart := blockMetaStart + blockMetaBytes
	blockBlobStart := keyBlobStart + keyBlobLen
	if blockBlobStart < keyBlobStart || blockBlobStart > len(data) || blockBlobStart+blockBlobLen < blockBlobStart || blockBlobStart+blockBlobLen > len(data) {
		return postingBlockIterator{}, 0, false
	}
	i := sort.Search(entryCount, func(i int) bool {
		entryOff := entriesStart + i*stringEntrySize
		keyOff := int(binary.LittleEndian.Uint32(data[entryOff:]))
		keyLen := int(binary.LittleEndian.Uint16(data[entryOff+4:]))
		if keyOff < 0 || keyLen < 0 || keyOff+keyLen < keyOff || keyOff+keyLen > keyBlobLen {
			return true
		}
		entryKey := stringView(data[keyBlobStart+keyOff : keyBlobStart+keyOff+keyLen])
		return entryKey >= key
	})
	if i >= entryCount {
		return postingBlockIterator{}, 0, false
	}
	entryOff := entriesStart + i*stringEntrySize
	keyOff := int(binary.LittleEndian.Uint32(data[entryOff:]))
	keyLen := int(binary.LittleEndian.Uint16(data[entryOff+4:]))
	if keyOff < 0 || keyLen < 0 || keyOff+keyLen < keyOff || keyOff+keyLen > keyBlobLen {
		return postingBlockIterator{}, 0, false
	}
	if stringView(data[keyBlobStart+keyOff:keyBlobStart+keyOff+keyLen]) != key {
		return postingBlockIterator{}, 0, false
	}
	count := int(binary.LittleEndian.Uint32(data[entryOff+8:]))
	firstBlock := int(binary.LittleEndian.Uint32(data[entryOff+12:]))
	entryBlockCount := int(binary.LittleEndian.Uint32(data[entryOff+16:]))
	if count <= 0 || firstBlock < 0 || entryBlockCount <= 0 || firstBlock+entryBlockCount < firstBlock || firstBlock+entryBlockCount > blockCount {
		return postingBlockIterator{}, 0, false
	}
	it := postingBlockIterator{
		section:        section,
		next:           firstBlock,
		end:            firstBlock + entryBlockCount,
		blockMetaStart: blockMetaStart,
		blockBlobStart: blockBlobStart,
		blockBlobLen:   blockBlobLen,
	}
	return it, count, true
}

func (section mappedPostingSection) gramPosting(gram uint32) []uint32 {
	it, count, ok := section.gramPostingIterator(gram)
	if !ok {
		return nil
	}
	return materializePostingBlockIterator(it, count)
}

func (section mappedPostingSection) gramPostingIterator(gram uint32) (postingBlockIterator, int, bool) {
	data := section.Data
	if len(data) < 16 {
		return postingBlockIterator{}, 0, false
	}
	entryCount := int(binary.LittleEndian.Uint32(data[0:]))
	keyBlobLen := int(binary.LittleEndian.Uint32(data[4:]))
	blockCount := int(binary.LittleEndian.Uint32(data[8:]))
	blockBlobLen := int(binary.LittleEndian.Uint32(data[12:]))
	if entryCount <= 0 || keyBlobLen != 0 || blockCount < 0 || blockBlobLen < 0 {
		return postingBlockIterator{}, 0, false
	}
	const entrySize = 16
	const blockMetaSize = 28
	entriesStart := 16
	entriesBytes := entryCount * entrySize
	if entriesBytes/entrySize != entryCount || entriesStart+entriesBytes < entriesStart || entriesStart+entriesBytes > len(data) {
		return postingBlockIterator{}, 0, false
	}
	blockMetaStart := entriesStart + entriesBytes
	blockMetaBytes := blockCount * blockMetaSize
	if blockMetaBytes/blockMetaSize != blockCount || blockMetaStart+blockMetaBytes < blockMetaStart || blockMetaStart+blockMetaBytes > len(data) {
		return postingBlockIterator{}, 0, false
	}
	blockBlobStart := blockMetaStart + blockMetaBytes
	if blockBlobStart < blockMetaStart || blockBlobStart > len(data) || blockBlobStart+blockBlobLen < blockBlobStart || blockBlobStart+blockBlobLen > len(data) {
		return postingBlockIterator{}, 0, false
	}
	i := sort.Search(entryCount, func(i int) bool {
		entryOff := entriesStart + i*entrySize
		return binary.LittleEndian.Uint32(data[entryOff:]) >= gram
	})
	if i >= entryCount {
		return postingBlockIterator{}, 0, false
	}
	entryOff := entriesStart + i*entrySize
	if binary.LittleEndian.Uint32(data[entryOff:]) != gram {
		return postingBlockIterator{}, 0, false
	}
	count := int(binary.LittleEndian.Uint32(data[entryOff+4:]))
	firstBlock := int(binary.LittleEndian.Uint32(data[entryOff+8:]))
	entryBlockCount := int(binary.LittleEndian.Uint32(data[entryOff+12:]))
	if count <= 0 || firstBlock < 0 || entryBlockCount <= 0 || firstBlock+entryBlockCount < firstBlock || firstBlock+entryBlockCount > blockCount {
		return postingBlockIterator{}, 0, false
	}
	it := postingBlockIterator{
		section:        section,
		next:           firstBlock,
		end:            firstBlock + entryBlockCount,
		blockMetaStart: blockMetaStart,
		blockBlobStart: blockBlobStart,
		blockBlobLen:   blockBlobLen,
	}
	return it, count, true
}

func decodeGramPostingIndex(data []byte, recordCount int) *compressedTrigramIndex {
	if len(data) < 16 {
		return nil
	}
	entryCount := int(binary.LittleEndian.Uint32(data[0:]))
	rawKeyBlobLen := binary.LittleEndian.Uint32(data[4:])
	omittedCount := 0
	keyBlobLen := int(rawKeyBlobLen)
	hasMetadata := false
	unionComplete := false
	blockCount := int(binary.LittleEndian.Uint32(data[8:]))
	blockBlobLen := int(binary.LittleEndian.Uint32(data[12:]))
	if keyBlobLen != 0 || blockCount < 0 || blockBlobLen < 0 {
		return nil
	}
	const entrySize = 16
	const blockMetaSize = 28
	entriesStart := 16
	entriesBytes := entryCount * entrySize
	if entriesBytes/entrySize != entryCount || entriesStart+entriesBytes > len(data) {
		return nil
	}
	blockMetaStart := entriesStart + entriesBytes
	blockMetaBytes := blockCount * blockMetaSize
	if blockMetaBytes/blockMetaSize != blockCount || blockMetaStart+blockMetaBytes < blockMetaStart || blockMetaStart+blockMetaBytes > len(data) {
		return nil
	}
	blockBlobStart := blockMetaStart + blockMetaBytes
	if blockBlobStart+blockBlobLen < blockBlobStart || blockBlobStart+blockBlobLen > len(data) {
		return nil
	}
	metadataStart := blockBlobStart + blockBlobLen
	if len(data)-metadataStart >= 8 && (binary.LittleEndian.Uint32(data[metadataStart:]) == gramPostingMetadataMagic || binary.LittleEndian.Uint32(data[metadataStart:]) == gramPostingUnionMetadataMagic) {
		hasMetadata = true
		unionComplete = binary.LittleEndian.Uint32(data[metadataStart:]) == gramPostingUnionMetadataMagic
		omittedCount = int(binary.LittleEndian.Uint32(data[metadataStart+4:]))
	}
	metadataBytes := 0
	if hasMetadata {
		if omittedCount < 0 || omittedCount > (len(data)-metadataStart-8)/8 {
			return nil
		}
		metadataBytes = 8 + omittedCount*8
	}
	if !hasMetadata && entryCount <= 0 {
		return nil
	}
	if metadataStart+metadataBytes < metadataStart || metadataStart+metadataBytes > len(data) || (hasMetadata && metadataStart+metadataBytes != len(data)) {
		return nil
	}
	ti := &compressedTrigramIndex{
		counts:             make(map[uint32]int, entryCount),
		gramCountsComplete: hasMetadata,
		gramUnionComplete:  unionComplete,
		gramSize:           3,
		recordCount:        recordCount,
		mappedGrams:        &mappedPostingSection{EntryCount: entryCount, BlockCount: blockCount, Bytes: len(data), Data: data},
	}
	if omittedCount > 0 {
		ti.omitted = make(map[uint32]struct{}, omittedCount)
		for i := 0; i < omittedCount; i++ {
			off := metadataStart + 8 + i*8
			gram := binary.LittleEndian.Uint32(data[off:])
			if i > 0 && gram <= binary.LittleEndian.Uint32(data[off-8:]) {
				return nil
			}
			ti.omitted[gram] = struct{}{}
			ti.counts[gram] = int(binary.LittleEndian.Uint32(data[off+4:]))
		}
	}
	for i := 0; i < entryCount; i++ {
		entryOff := entriesStart + i*entrySize
		gram := binary.LittleEndian.Uint32(data[entryOff:])
		count := int(binary.LittleEndian.Uint32(data[entryOff+4:]))
		firstBlock := int(binary.LittleEndian.Uint32(data[entryOff+8:]))
		entryBlockCount := int(binary.LittleEndian.Uint32(data[entryOff+12:]))
		if count <= 0 || firstBlock < 0 || entryBlockCount <= 0 || firstBlock+entryBlockCount < firstBlock || firstBlock+entryBlockCount > blockCount {
			continue
		}
		for blockIndex := firstBlock; blockIndex < firstBlock+entryBlockCount; blockIndex++ {
			metaOff := blockMetaStart + blockIndex*blockMetaSize
			blockOffset := int(binary.LittleEndian.Uint64(data[metaOff:]))
			blockLength := int(binary.LittleEndian.Uint32(data[metaOff+8:]))
			if blockOffset < 0 || blockLength < 0 || blockOffset+blockLength < blockOffset || blockOffset+blockLength > blockBlobLen {
				return nil
			}
		}
		ti.counts[gram] = count
	}
	if len(ti.counts) == 0 && len(ti.omitted) == 0 {
		return nil
	}
	return ti
}

func mappedReadString(data []byte, off int) (string, int, error) {
	if off < 0 || off+4 < off || off+4 > len(data) {
		return "", off, errors.New("invalid mapped string length")
	}
	n := int(binary.LittleEndian.Uint32(data[off:]))
	off += 4
	if off+n < off || off+n > len(data) {
		return "", off, errors.New("invalid mapped string")
	}
	return string(data[off : off+n]), off + n, nil
}

func loadConfig(path string) (appConfig, error) {
	if path == "" {
		path = findDefaultConfig()
	}
	if path == "" {
		return appConfig{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return appConfig{}, err
	}
	cfg := appConfig{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		switch key {
		case "db", "db_path":
			if s := parseTOMLString(value); s != "" {
				cfg.DBs = append(cfg.DBs, s)
			}
		case "dbs", "db_paths":
			cfg.DBs = append(cfg.DBs, parseTOMLStringArray(value)...)
		case "volume":
			if s := parseTOMLString(value); s != "" {
				cfg.Volumes = append(cfg.Volumes, s)
			}
		case "volumes":
			cfg.Volumes = append(cfg.Volumes, parseTOMLStringArray(value)...)
		case "service_pipe":
			cfg.ServicePipe = parseTOMLString(value)
		case "output_format":
			cfg.OutputFormat = strings.ToLower(parseTOMLString(value))
		case "default_limit":
			var n int
			if _, err := fmt.Sscanf(value, "%d", &n); err == nil {
				cfg.DefaultLimit = n
			}
		}
	}
	return cfg, nil
}

func findDefaultConfig() string {
	candidates := []string{"seekfs.toml"}
	candidates = append(candidates, defaultConfigPath())
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

func defaultConfigPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "seekfs", "seekfs.toml")
	}
	return "seekfs.toml"
}

func parseTOMLString(value string) string {
	value = strings.TrimSpace(strings.TrimSuffix(value, ","))
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
		return value[1 : len(value)-1]
	}
	return value
}

func parseTOMLStringArray(value string) []string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "[")
	value = strings.TrimSuffix(value, "]")
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if s := parseTOMLString(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func loadIndexes(paths []string) ([]*Index, error) {
	indexes := make([]*Index, 0, len(paths))
	for _, path := range paths {
		idx, err := loadIndex(path)
		if err != nil {
			return nil, err
		}
		idx.DBPath = path
		indexes = append(indexes, idx)
	}
	return indexes, nil
}

func defaultDB() string {
	if v := os.Getenv("SEEKFS_DB"); v != "" {
		return v
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "seekfs.db"
	}
	return filepath.Join(dir, "seekfs", "index.gsi")
}

func defaultIndexDir() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		if dir, err := os.UserCacheDir(); err == nil {
			base = dir
		}
	}
	if base == "" {
		return "."
	}
	return filepath.Join(base, "seekfs", "indexes")
}

func defaultVolumeDB(indexDir, volume string) string {
	letter := strings.ToLower(strings.TrimSuffix(strings.TrimRight(volume, `\`), ":"))
	if letter == "" {
		letter = "volume"
	}
	return filepath.Join(indexDir, "seekfs_"+letter+".gsi")
}

func defaultIndexVolumes() []string {
	var volumes []string
	for letter := 'C'; letter <= 'Z'; letter++ {
		root := fmt.Sprintf("%c:\\", letter)
		driveType := windows.GetDriveType(windows.StringToUTF16Ptr(root))
		if driveType == windows.DRIVE_FIXED {
			volumes = append(volumes, fmt.Sprintf("%c:", letter))
		}
	}
	if len(volumes) == 0 {
		return []string{"C:"}
	}
	return volumes
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
