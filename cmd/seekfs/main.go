package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const indexVersion = 7

var indexMagic = [8]byte{'G', 'O', 'S', 'R', 'C', 'H', '0', '7'}
var tokenSidecarMagic = [8]byte{'G', 'S', 'T', 'O', 'K', '0', '0', '1'}

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

const (
	fsctlEnumUSNData     = 0x000900b3
	fsctlQueryUSNJournal = 0x000900f4
	fsctlReadUSNJournal  = 0x000900bb
	fileAttributeDir     = 0x10
	serviceName          = "seekfs"
	defaultServicePipe   = `\\.\pipe\seekfs-service`
	defaultServiceSDDL   = `D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;IU)`
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

type jsonSearchResponse struct {
	OK      bool         `json:"ok"`
	Query   string       `json:"query"`
	Count   int          `json:"count"`
	Limit   int          `json:"limit,omitempty"`
	Results []jsonResult `json:"results,omitempty"`
}

type jsonInfoResponse struct {
	OK          bool     `json:"ok"`
	Version     int      `json:"version"`
	Source      string   `json:"source"`
	BuiltAt     string   `json:"built_at"`
	Entries     int      `json:"entries"`
	Roots       []string `json:"roots"`
	Volume      string   `json:"volume,omitempty"`
	JournalID   uint64   `json:"journal_id,omitempty"`
	Checkpoint  int64    `json:"checkpoint_usn,omitempty"`
	ContentHash string   `json:"content_hash,omitempty"`
}

type benchSummary struct {
	OK         bool               `json:"ok"`
	Mode       string             `json:"mode"`
	Iterations int                `json:"iterations"`
	Failures   int                `json:"failures"`
	Queries    int                `json:"queries"`
	Stats      map[string]float64 `json:"stats_ms"`
}

type Index struct {
	Version          int
	Roots            []string
	BuiltAt          time.Time
	Source           string
	Volume           string
	JournalID        uint64
	Checkpoint       int64
	ContentHash      string
	Entries          []Entry
	NameOrder        []int
	PathOrder        []int
	Compact          bool
	Records          []CompactRecord
	CompactNameOrder []int
	NameBlob         []byte
	PathTokenIndex   map[string][]int
	DBPath           string
}

type appConfig struct {
	DBs          []string
	Volumes      []string
	ServicePipe  string
	DefaultLimit int
}

type queryOptions struct {
	Query         string `json:"query"`
	MatchPath     bool   `json:"match_path"`
	Limit         int    `json:"limit"`
	Under         string `json:"under,omitempty"`
	Exists        bool   `json:"exists,omitempty"`
	CWDBias       string `json:"cwd_bias,omitempty"`
	RootBias      string `json:"root_bias,omitempty"`
	Recent        string `json:"recent,omitempty"`
	ModifiedAfter string `json:"modified_after,omitempty"`
	CaseSensitive bool   `json:"case_sensitive,omitempty"`
}

type parsedQuery struct {
	Raw           string
	Terms         []string
	CaseSensitive bool
	Exts          []string
	Dirs          []string
	Globs         []string
	Regexps       []*regexp.Regexp
	Type          string
	Under         string
	Exists        bool
	ModifiedAfter time.Time
	HasModAfter   bool
	CWDBias       string
	RootBias      string
}

type CompactRecord struct {
	Parent    int32
	Name      string
	LowerName string
	NameOff   uint32
	NameLen   uint16
	Mode      uint32
	Size      int64
	ModUnix   int64
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

type volumeMonitor struct {
	volume    string
	cancel    chan struct{}
	lastUSN   int64
	journalID uint64
	events    uint64
	lastError string
	running   bool
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
		return usage()
	}
	switch args[0] {
	case "index":
		return cmdIndex(args[1:])
	case "index-usn":
		return cmdIndexUSN(args[1:])
	case "service":
		return cmdService(args[1:])
	case "install-service":
		return cmdInstallService(args[1:])
	case "uninstall-service":
		return cmdUninstallService(args[1:])
	case "service-index-usn":
		return cmdServiceIndexUSN(args[1:])
	case "service-monitor-start":
		return cmdServiceSimple(args[1:], "monitor-start")
	case "service-monitor-stop":
		return cmdServiceSimple(args[1:], "monitor-stop")
	case "service-status":
		return cmdServiceSimple(args[1:], "status")
	case "service-info":
		return cmdServiceInfo(args[1:])
	case "token-sidecar":
		return cmdTokenSidecar(args[1:])
	case "bench-agent":
		return cmdBenchAgent(args[1:])
	case "compare-es":
		return cmdCompareES(args[1:])
	case "info":
		return cmdInfo(args[1:])
	case "search":
		return cmdSearch(args[1:], false)
	case "count":
		return cmdSearch(args[1:], true)
	case "serve":
		return cmdServe(args[1:])
	case "version":
		fmt.Printf("seekfs %s commit=%s date=%s\n", version, commit, date)
		return nil
	case "help", "-h", "--help":
		return usage()
	case "help-search":
		printSearchHelp()
		return nil
	default:
		return usage()
	}
}

func usage() error {
	fmt.Fprintln(os.Stderr, `Usage:
  seekfs index -root <path> [-root <path>...] [-db seekfs.db]
  seekfs index-usn -volume C: [-db seekfs.db]
  seekfs service [-pipe \\.\pipe\seekfs-service] [-sddl <sddl>] [-db index.gsi...]
  seekfs install-service [-pipe \\.\pipe\seekfs-service] [-sddl <sddl>] [-db index.gsi...]
  seekfs uninstall-service
  seekfs service-index-usn -volume C: -db seekfs.gsi [-pipe \\.\pipe\seekfs-service]
  seekfs service-monitor-start -volume C:
  seekfs service-monitor-stop -volume C:
  seekfs service-status [--json]
  seekfs service-info [--json]
  seekfs token-sidecar -db seekfs.gsi [-out seekfs.gsi.tok] [-query "term term"] [-term term]
  seekfs bench-agent [-db index.gsi...] [-service] [--json] [-iterations 100]
  seekfs compare-es -es path\to\es.exe [-instance 1.5a] [-db seekfs.gsi] [-n 100] <query>
  seekfs info [-db seekfs.gsi] [--json]
  seekfs help-search
  seekfs search [-db seekfs.db...] [-tokens seekfs.db.tok] [-addr 127.0.0.1:47832] [-service] [--json] [-n 100] [-path] <query>
  seekfs count [-db seekfs.db...] [-tokens seekfs.db.tok] [-addr 127.0.0.1:47832] [-service] [--json] [-path] <query>
  seekfs serve [-db seekfs.db] [-tokens seekfs.db.tok] [-addr 127.0.0.1:47832]
  seekfs version`)
	return errors.New("unknown or missing command")
}

func printSearchHelp() {
	fmt.Print(`seekfs search syntax

Supported today:
  plain text       Case-insensitive substring match against file name.
  multiple terms   Whitespace-separated terms are ANDed.
  -path            Match terms against the full path instead of just the name.
  -n <num>         Limit returned rows.
  count            Print the number of matches instead of result paths.

Examples:
  seekfs search -db index.gsi bench
  seekfs search -db index.gsi "bench py"
  seekfs search -db index.gsi -path "Codex 2026"
  seekfs count  -db index.gsi needle

Not implemented yet:
  Everything filters such as ext:, dm:, size:, attrib:, parent:
  wildcard operators such as *.go
  regex mode
  OR / NOT operators
  quoted phrase parsing beyond the shell's normal argument grouping
  date macros such as today or lastweek
  ranking compatible with Everything

Workarounds:
  Use ".py" as a substring to approximate ext:py for now.
  Use -path when you need folder/path matching.
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

func indexUSNVolume(volume string) (*Index, error) {
	vol := normalizeVolume(volume)
	handle, err := openVolume(vol)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(handle)

	var journal usnJournalDataV0
	var bytesReturned uint32
	if err := windows.DeviceIoControl(
		handle,
		fsctlQueryUSNJournal,
		nil,
		0,
		(*byte)(unsafe.Pointer(&journal)),
		uint32(unsafe.Sizeof(journal)),
		&bytesReturned,
		nil,
	); err != nil {
		return nil, fmt.Errorf("query USN journal for %s: %w; run elevated or use a service helper for raw volume access", vol, err)
	}

	nodes, err := enumUSN(handle, journal.NextUsn)
	if err != nil {
		return nil, err
	}
	serviceLog("enum-usn complete volume=%s nodes=%d next_usn=%d", vol, len(nodes), journal.NextUsn)

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
			Parent:    parent,
			Name:      node.name,
			LowerName: strings.ToLower(node.name),
			Mode:      modeFromAttrs(node.attr),
		})
	}
	serviceLog("compact records complete volume=%s entries=%d", vol, len(idx.Records))
	return idx, nil
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
				frn := binary.LittleEndian.Uint64(record[8:16])
				parent := binary.LittleEndian.Uint64(record[16:24])
				attr := binary.LittleEndian.Uint32(record[52:56])
				nameLen := binary.LittleEndian.Uint16(record[56:58])
				nameOff := binary.LittleEndian.Uint16(record[58:60])
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
	if attr&fileAttributeDir != 0 {
		return uint32(os.ModeDir)
	}
	return 0
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

func cmdSearch(args []string, countOnly bool) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	var dbs stringList
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	fs.Var(&dbs, "db", "index database path; repeatable")
	tokens := fs.String("tokens", "", "optional path-token sidecar; defaults to <db>.tok when present")
	addr := fs.String("addr", "", "resident server address")
	useService := fs.Bool("service", false, "query the installed seekfs service over its named pipe")
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
	if *limit == 100 && cfg.DefaultLimit > 0 {
		*limit = cfg.DefaultLimit
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return errors.New("query required")
	}
	opts := queryOptions{
		Query:         query,
		MatchPath:     *matchPath,
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
	if *addr != "" {
		return searchRemote(*addr, opts, countOnly, *jsonOut)
	}
	if *useService {
		return searchService(*pipeName, opts, countOnly, *jsonOut)
	}
	if len(dbs) == 0 {
		dbs = append(dbs, defaultDB())
	}
	indexes, err := loadIndexes(dbs, *tokens)
	if err != nil {
		return err
	}
	matches, err := searchAll(indexes, opts, countOnly)
	if err != nil {
		return err
	}
	if *jsonOut {
		resp := jsonSearchResponse{
			OK:    true,
			Query: query,
			Count: len(matches),
			Limit: *limit,
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
	idx, err := loadIndex(*db)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeJSON(os.Stdout, indexInfoToJSON(idx))
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
	return nil
}

func (idx *Index) entryCount() int {
	if idx.Compact {
		return len(idx.Records)
	}
	return len(idx.Entries)
}

func cmdCompareES(args []string) error {
	fs := flag.NewFlagSet("compare-es", flag.ContinueOnError)
	db := fs.String("db", defaultDB(), "index database path")
	es := fs.String("es", filepath.Join("extracted", "es.exe"), "path to es.exe")
	instance := fs.String("instance", "", "Everything instance")
	limit := fs.Int("n", 100, "maximum results")
	matchPath := fs.Bool("path", false, "match full path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return errors.New("query required")
	}
	idx, err := loadIndex(*db)
	if err != nil {
		return err
	}
	ours, err := search(idx, queryOptions{Query: query, MatchPath: *matchPath, Limit: *limit}, false)
	if err != nil {
		return err
	}
	esArgs := []string{}
	if *instance != "" {
		esArgs = append(esArgs, "-instance", *instance)
	}
	if *matchPath {
		esArgs = append(esArgs, "-match-path")
	}
	esArgs = append(esArgs, "-n", fmt.Sprint(*limit), query)
	out, err := exec.Command(*es, esArgs...).Output()
	if err != nil {
		return err
	}
	theirs := splitLines(string(out))
	ourPaths := make([]string, len(ours))
	for i, entry := range ours {
		ourPaths[i] = entry.Path
	}
	agreement := 0
	max := min(len(ourPaths), len(theirs))
	for i := 0; i < max; i++ {
		if strings.EqualFold(ourPaths[i], theirs[i]) {
			agreement++
		}
	}
	fmt.Printf("query: %s\n", query)
	fmt.Printf("mode: %s\n", map[bool]string{true: "path", false: "name"}[*matchPath])
	fmt.Printf("seekfs_results: %d\n", len(ourPaths))
	fmt.Printf("es_results: %d\n", len(theirs))
	fmt.Printf("same_position_top_%d: %d\n", max, agreement)
	if max > 0 && agreement != max {
		fmt.Println("first_difference:")
		for i := 0; i < max; i++ {
			if !strings.EqualFold(ourPaths[i], theirs[i]) {
				fmt.Printf("  rank: %d\n  seekfs: %s\n  es: %s\n", i+1, ourPaths[i], theirs[i])
				break
			}
		}
	}
	return nil
}

func cmdTokenSidecar(args []string) error {
	var terms stringList
	fs := flag.NewFlagSet("token-sidecar", flag.ContinueOnError)
	db := fs.String("db", defaultDB(), "index database path")
	out := fs.String("out", "", "sidecar output path")
	query := fs.String("query", "", "whitespace-separated terms to index")
	fs.Var(&terms, "term", "term to index; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	for _, term := range strings.Fields(strings.ToLower(*query)) {
		terms = append(terms, term)
	}
	if len(terms) == 0 {
		return errors.New("token-sidecar requires -query or at least one -term")
	}
	if *out == "" {
		*out = *db + ".tok"
	}
	idx, err := loadIndex(*db)
	if err != nil {
		return err
	}
	if !idx.Compact {
		return errors.New("token-sidecar currently requires a compact USN index")
	}
	start := time.Now()
	tokenSet := make(map[string]struct{}, len(terms))
	for _, term := range terms {
		term = strings.ToLower(strings.TrimSpace(term))
		if term != "" {
			tokenSet[term] = struct{}{}
		}
	}
	postings := make(map[string][]int, len(tokenSet))
	pathCache := make(map[int]string)
	for i := range idx.Records {
		path := strings.ToLower(idx.reconstructCompactPathCached(i, pathCache))
		for term := range tokenSet {
			if strings.Contains(path, term) {
				postings[term] = append(postings[term], i)
			}
		}
	}
	for token := range postings {
		sort.Ints(postings[token])
	}
	if err := saveTokenSidecar(*out, postings); err != nil {
		return err
	}
	info, _ := os.Stat(*out)
	size := int64(0)
	if info != nil {
		size = info.Size()
	}
	fmt.Printf("wrote %d token postings to %s (%d bytes) in %s\n", len(postings), *out, size, time.Since(start).Round(time.Millisecond))
	return nil
}

func cmdBenchAgent(args []string) error {
	fs := flag.NewFlagSet("bench-agent", flag.ContinueOnError)
	var dbs stringList
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	fs.Var(&dbs, "db", "index database path; repeatable")
	useService := fs.Bool("service", false, "query the installed seekfs service")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	jsonOut := fs.Bool("json", false, "write machine-readable JSON")
	iterations := fs.Int("iterations", 100, "number of benchmark iterations")
	limit := fs.Int("n", 20, "maximum results per query")
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
	queries := fs.Args()
	if len(queries) == 0 {
		queries = []string{"ext:go", "glob:*.md", "type:dir docs", "README", "main"}
	}
	if *iterations <= 0 {
		return errors.New("iterations must be positive")
	}
	var indexes []*Index
	if !*useService {
		if len(dbs) == 0 {
			dbs = append(dbs, defaultDB())
		}
		indexes, err = loadIndexes(dbs, "")
		if err != nil {
			return err
		}
	}
	timings := make([]float64, 0, *iterations)
	failures := 0
	for i := 0; i < *iterations; i++ {
		query := queries[i%len(queries)]
		opts := queryOptions{Query: query, MatchPath: true, Limit: *limit}
		start := time.Now()
		if *useService {
			err = benchServiceQuery(*pipeName, opts)
		} else {
			_, err = searchAll(indexes, opts, false)
		}
		elapsed := float64(time.Since(start).Microseconds()) / 1000
		timings = append(timings, elapsed)
		if err != nil {
			failures++
		}
	}
	summary := benchSummary{
		OK:         failures == 0,
		Mode:       map[bool]string{true: "service", false: "local"}[*useService],
		Iterations: *iterations,
		Failures:   failures,
		Queries:    len(queries),
		Stats:      latencyStats(timings),
	}
	if *jsonOut {
		return writeJSON(os.Stdout, summary)
	}
	fmt.Printf("mode: %s\niterations: %d\nqueries: %d\nfailures: %d\n", summary.Mode, summary.Iterations, summary.Queries, summary.Failures)
	for _, key := range []string{"min", "median", "p90", "p95", "max"} {
		fmt.Printf("%s_ms: %.3f\n", key, summary.Stats[key])
	}
	return nil
}

func benchServiceQuery(pipeName string, opts queryOptions) error {
	resp, err := callService(pipeName, serviceRequestFromOptions(opts, false))
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Message)
	}
	return nil
}

func latencyStats(values []float64) map[string]float64 {
	stats := map[string]float64{"min": 0, "median": 0, "p90": 0, "p95": 0, "max": 0}
	if len(values) == 0 {
		return stats
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	stats["min"] = sorted[0]
	stats["median"] = percentile(sorted, 0.50)
	stats["p90"] = percentile(sorted, 0.90)
	stats["p95"] = percentile(sorted, 0.95)
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

func splitLines(s string) []string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	out := lines[:0]
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

type request struct {
	Query         string `json:"query"`
	MatchPath     bool   `json:"match_path"`
	Limit         int    `json:"limit"`
	CountOnly     bool   `json:"count_only"`
	Under         string `json:"under,omitempty"`
	Exists        bool   `json:"exists,omitempty"`
	CWDBias       string `json:"cwd_bias,omitempty"`
	RootBias      string `json:"root_bias,omitempty"`
	Recent        string `json:"recent,omitempty"`
	ModifiedAfter string `json:"modified_after,omitempty"`
	CaseSensitive bool   `json:"case_sensitive,omitempty"`
}

type response struct {
	Count   int      `json:"count"`
	Results []string `json:"results,omitempty"`
	Error   string   `json:"error,omitempty"`
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
}

type serviceResponse struct {
	OK      bool     `json:"ok"`
	Message string   `json:"message,omitempty"`
	Entries int      `json:"entries,omitempty"`
	Count   int      `json:"count,omitempty"`
	Results []string `json:"results,omitempty"`
	DBs     []dbInfo `json:"dbs,omitempty"`
}

type dbInfo struct {
	Path       string `json:"path"`
	Entries    int    `json:"entries"`
	Source     string `json:"source"`
	BuiltAt    string `json:"built_at"`
	Volume     string `json:"volume,omitempty"`
	JournalID  uint64 `json:"journal_id,omitempty"`
	Checkpoint int64  `json:"checkpoint_usn,omitempty"`
}

type goSearchService struct {
	pipeName string
	sddl     string
	stop     chan struct{}
	monitors map[string]*volumeMonitor
	dbs      []string
	indexes  []*Index
	indexMu  sync.RWMutex
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
	isService, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	handler := &goSearchService{pipeName: *pipeName, sddl: *sddl, stop: make(chan struct{}), monitors: make(map[string]*volumeMonitor), dbs: dbs}
	if isService {
		return svc.Run(serviceName, handler)
	}
	return handler.runStandalone()
}

func cmdInstallService(args []string) error {
	var dbs stringList
	fs := flag.NewFlagSet("install-service", flag.ContinueOnError)
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

func cmdUninstallService(args []string) error {
	fs := flag.NewFlagSet("uninstall-service", flag.ContinueOnError)
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
	fs := flag.NewFlagSet("service-info", flag.ContinueOnError)
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

func callService(pipeName string, req serviceRequest) (serviceResponse, error) {
	handle, err := openPipeClient(pipeName)
	if err != nil {
		return serviceResponse{}, err
	}
	file := os.NewFile(uintptr(handle), pipeName)
	defer file.Close()
	if err := json.NewEncoder(file).Encode(req); err != nil {
		return serviceResponse{}, err
	}
	var resp serviceResponse
	if err := json.NewDecoder(file).Decode(&resp); err != nil {
		return serviceResponse{}, err
	}
	return resp, nil
}

func openPipeClient(pipeName string) (windows.Handle, error) {
	ptr, err := windows.UTF16PtrFromString(pipeName)
	if err != nil {
		return 0, err
	}
	deadline := time.Now().Add(5 * time.Second)
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
		time.Sleep(50 * time.Millisecond)
	}
}

func (s *goSearchService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}
	if err := s.loadConfiguredIndexes(); err != nil {
		serviceLog("startup index load error: %v", err)
		return false, 1
	}
	done := make(chan struct{})
	go func() {
		s.servePrivileged()
		close(done)
	}()
	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
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
	if err := s.loadConfiguredIndexes(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "seekfs privileged service listening on %s\n", s.pipeName)
	s.servePrivileged()
	return nil
}

func (s *goSearchService) loadConfiguredIndexes() error {
	if len(s.dbs) == 0 {
		serviceLog("service started without search databases")
		return nil
	}
	start := time.Now()
	indexes, err := loadIndexes(s.dbs, "")
	if err != nil {
		return err
	}
	total := 0
	for _, idx := range indexes {
		total += idx.entryCount()
	}
	s.indexMu.Lock()
	s.indexes = indexes
	s.indexMu.Unlock()
	serviceLog("loaded %d dbs entries=%d elapsed=%s", len(indexes), total, time.Since(start).Round(time.Millisecond))
	return nil
}

func (s *goSearchService) servePrivileged() {
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
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_NOWAIT,
		windows.PIPE_UNLIMITED_INSTANCES,
		64*1024,
		64*1024,
		0,
		sa,
	)
	if err != nil {
		return nil, err
	}
	for {
		err = windows.ConnectNamedPipe(handle, nil)
		if err == nil || err == windows.ERROR_PIPE_CONNECTED {
			return os.NewFile(uintptr(handle), s.pipeName), nil
		}
		if err != windows.ERROR_PIPE_LISTENING {
			windows.CloseHandle(handle)
			return nil, err
		}
		select {
		case <-s.stop:
			windows.CloseHandle(handle)
			return nil, windows.ERROR_OPERATION_ABORTED
		case <-time.After(50 * time.Millisecond):
		}
	}
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
		indexes := append([]*Index(nil), s.indexes...)
		dbs := append([]string(nil), s.dbs...)
		s.indexMu.RUnlock()
		infos := make([]dbInfo, 0, len(indexes))
		total := 0
		for i, idx := range indexes {
			path := ""
			if i < len(dbs) {
				path = dbs[i]
			}
			total += idx.entryCount()
			infos = append(infos, dbInfo{
				Path:       path,
				Entries:    idx.entryCount(),
				Source:     idx.Source,
				BuiltAt:    idx.BuiltAt.Format(time.RFC3339Nano),
				Volume:     idx.Volume,
				JournalID:  idx.JournalID,
				Checkpoint: idx.Checkpoint,
			})
		}
		_ = json.NewEncoder(conn).Encode(serviceResponse{OK: true, Entries: total, DBs: infos})
	case "search":
		s.indexMu.RLock()
		indexes := append([]*Index(nil), s.indexes...)
		s.indexMu.RUnlock()
		if len(indexes) == 0 {
			_ = json.NewEncoder(conn).Encode(serviceResponse{OK: false, Message: "service has no search indexes loaded"})
			return
		}
		matches, err := searchAll(indexes, requestToOptionsFromService(req), req.CountOnly)
		if err != nil {
			_ = json.NewEncoder(conn).Encode(serviceResponse{OK: false, Message: err.Error()})
			return
		}
		resp := serviceResponse{OK: true, Count: len(matches)}
		if !req.CountOnly {
			resp.Results = make([]string, len(matches))
			for i, entry := range matches {
				resp.Results[i] = entry.Path
			}
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
		if err := saveIndex(req.DB, idx); err != nil {
			serviceLog("index-usn save error volume=%s err=%v", req.Volume, err)
			_ = json.NewEncoder(conn).Encode(serviceResponse{OK: false, Message: err.Error()})
			return
		}
		serviceLog("index-usn complete volume=%s entries=%d", req.Volume, idx.entryCount())
		_ = json.NewEncoder(conn).Encode(serviceResponse{OK: true, Message: "indexed", Entries: idx.entryCount()})
	case "monitor-start":
		volume := normalizeVolume(req.Volume)
		if mon, ok := s.monitors[volume]; ok && mon.running {
			_ = json.NewEncoder(conn).Encode(serviceResponse{OK: true, Message: "monitor already running for " + volume})
			return
		}
		mon := &volumeMonitor{volume: volume, cancel: make(chan struct{}), running: true}
		s.monitors[volume] = mon
		go runUSNMonitor(mon)
		_ = json.NewEncoder(conn).Encode(serviceResponse{OK: true, Message: "monitor started for " + volume})
	case "monitor-stop":
		volume := normalizeVolume(req.Volume)
		if mon, ok := s.monitors[volume]; ok {
			close(mon.cancel)
			delete(s.monitors, volume)
		}
		_ = json.NewEncoder(conn).Encode(serviceResponse{OK: true, Message: "monitor stopped for " + volume})
	case "status":
		volumes := make([]string, 0, len(s.monitors))
		for volume, mon := range s.monitors {
			state := volume
			if mon.lastError != "" {
				state += "(error: " + mon.lastError + ")"
			} else {
				state += fmt.Sprintf("(usn:%d events:%d)", mon.lastUSN, mon.events)
			}
			volumes = append(volumes, state)
		}
		sort.Strings(volumes)
		msg := "service running"
		if len(volumes) > 0 {
			msg += "; monitors: " + strings.Join(volumes, ",")
		}
		_ = json.NewEncoder(conn).Encode(serviceResponse{OK: true, Message: msg})
	default:
		_ = json.NewEncoder(conn).Encode(serviceResponse{OK: false, Message: "unknown command"})
	}
}

func runUSNMonitor(mon *volumeMonitor) {
	handle, err := openVolume(mon.volume)
	if err != nil {
		mon.lastError = err.Error()
		mon.running = false
		return
	}
	defer windows.CloseHandle(handle)
	var journal usnJournalDataV0
	var bytesReturned uint32
	if err := windows.DeviceIoControl(
		handle,
		fsctlQueryUSNJournal,
		nil,
		0,
		(*byte)(unsafe.Pointer(&journal)),
		uint32(unsafe.Sizeof(journal)),
		&bytesReturned,
		nil,
	); err != nil {
		mon.lastError = err.Error()
		mon.running = false
		return
	}
	mon.journalID = journal.UsnJournalID
	mon.lastUSN = journal.NextUsn
	buffer := make([]byte, 1024*1024)
	for {
		select {
		case <-mon.cancel:
			mon.running = false
			return
		default:
		}
		req := readUSNJournalDataV0{
			StartUsn:       mon.lastUSN,
			ReasonMask:     0xffffffff,
			Timeout:        1,
			BytesToWaitFor: 0,
			UsnJournalID:   mon.journalID,
		}
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
			mon.lastError = err.Error()
			time.Sleep(time.Second)
			continue
		}
		mon.lastError = ""
		if bytesReturned <= 8 {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		mon.lastUSN = int64(binary.LittleEndian.Uint64(buffer[:8]))
		pos := uint32(8)
		for pos+60 <= bytesReturned {
			recordLen := binary.LittleEndian.Uint32(buffer[pos : pos+4])
			if recordLen < 60 || pos+recordLen > bytesReturned {
				break
			}
			mon.events++
			pos += recordLen
		}
	}
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var dbs stringList
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	fs.Var(&dbs, "db", "index database path; repeatable")
	tokens := fs.String("tokens", "", "optional path-token sidecar; defaults to <db>.tok when present")
	addr := fs.String("addr", "127.0.0.1:47832", "listen address")
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
		dbs = append(dbs, defaultDB())
	}
	indexes, err := loadIndexes(dbs, *tokens)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	total := 0
	for _, idx := range indexes {
		total += idx.entryCount()
	}
	fmt.Fprintf(os.Stderr, "seekfs server listening on %s with %d entries\n", ln.Addr(), total)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go handleConn(conn, indexes)
	}
}

func handleConn(conn net.Conn, indexes []*Index) {
	defer conn.Close()
	var req request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(response{Error: err.Error()})
		return
	}
	matches, err := searchAll(indexes, requestToOptions(req), req.CountOnly)
	if err != nil {
		_ = json.NewEncoder(conn).Encode(response{Error: err.Error()})
		return
	}
	resp := response{Count: len(matches)}
	if !req.CountOnly {
		resp.Results = make([]string, len(matches))
		for i, entry := range matches {
			resp.Results[i] = entry.Path
		}
	}
	_ = json.NewEncoder(conn).Encode(resp)
}

func searchRemote(addr string, opts queryOptions, countOnly bool, jsonOut bool) error {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	req := optionsToRequest(opts, countOnly)
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return err
	}
	var resp response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	if jsonOut {
		jsonResp := jsonSearchResponse{
			OK:    true,
			Query: opts.Query,
			Count: resp.Count,
			Limit: opts.Limit,
		}
		if !countOnly {
			jsonResp.Results = pathsToJSON(resp.Results)
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
			OK:    true,
			Query: opts.Query,
			Count: resp.Count,
			Limit: opts.Limit,
		}
		if !countOnly {
			jsonResp.Results = pathsToJSON(resp.Results)
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

func optionsToRequest(opts queryOptions, countOnly bool) request {
	return request{
		Query:         opts.Query,
		MatchPath:     opts.MatchPath,
		Limit:         opts.Limit,
		CountOnly:     countOnly,
		Under:         opts.Under,
		Exists:        opts.Exists,
		CWDBias:       opts.CWDBias,
		RootBias:      opts.RootBias,
		Recent:        opts.Recent,
		ModifiedAfter: opts.ModifiedAfter,
		CaseSensitive: opts.CaseSensitive,
	}
}

func requestToOptions(req request) queryOptions {
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
	}
}

func serviceRequestFromOptions(opts queryOptions, countOnly bool) serviceRequest {
	return serviceRequest{
		Command:       "search",
		Query:         opts.Query,
		MatchPath:     opts.MatchPath,
		Limit:         opts.Limit,
		CountOnly:     countOnly,
		Under:         opts.Under,
		Exists:        opts.Exists,
		CWDBias:       opts.CWDBias,
		RootBias:      opts.RootBias,
		Recent:        opts.Recent,
		ModifiedAfter: opts.ModifiedAfter,
		CaseSensitive: opts.CaseSensitive,
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
	order := idx.NameOrder
	if opts.MatchPath {
		order = idx.PathOrder
	}
	limit := normalizedLimit(opts.Limit, countOnly)
	if pq.RootBias != "" || pq.CWDBias != "" {
		order = biasOrderEntries(idx, order, firstNonEmpty(pq.CWDBias, pq.RootBias))
	}
	results := make([]Entry, 0, min(limit, 1024))
	for _, entryIndex := range order {
		entry := idx.Entries[entryIndex]
		entry.IndexSource = idx.Source
		if entryMatches(entry, pq, opts.MatchPath) {
			results = append(results, entry)
			if !countOnly && len(results) >= limit {
				break
			}
		}
	}
	return results, nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func indexInfoToJSON(idx *Index) jsonInfoResponse {
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
	}
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
	if entry.Size != 0 {
		size := entry.Size
		result.Size = &size
	}
	if entry.ModUnix != 0 {
		result.Modified = time.Unix(0, entry.ModUnix).Format(time.RFC3339Nano)
	}
	return result
}

func searchAll(indexes []*Index, opts queryOptions, countOnly bool) ([]Entry, error) {
	if len(indexes) == 1 {
		return search(indexes[0], opts, countOnly)
	}
	limit := normalizedLimit(opts.Limit, countOnly)
	results := make([]Entry, 0, min(limit, 1024))
	for _, idx := range indexes {
		perIndexLimit := limit
		if !countOnly && limit > 0 {
			perIndexLimit = limit - len(results)
			if perIndexLimit <= 0 {
				break
			}
		}
		childOpts := opts
		childOpts.Limit = perIndexLimit
		matches, err := search(idx, childOpts, countOnly)
		if err != nil {
			return nil, err
		}
		results = append(results, matches...)
	}
	return results, nil
}

func searchCompact(idx *Index, opts queryOptions, countOnly bool) ([]Entry, error) {
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, err
	}
	limit := normalizedLimit(opts.Limit, countOnly)
	order := idx.CompactNameOrder
	if opts.MatchPath && len(pq.Terms) > 0 {
		if candidates, ok := idx.pathCandidates(pq.Terms); ok {
			order = candidates
		}
	}
	if order == nil {
		order = make([]int, len(idx.Records))
		for i := range order {
			order[i] = i
		}
	}
	if pq.RootBias != "" || pq.CWDBias != "" {
		order = idx.biasOrderCompact(order, firstNonEmpty(pq.CWDBias, pq.RootBias))
	}
	results := make([]Entry, 0, min(limit, 1024))
	pathCache := make(map[int]string)
	for _, recIndex := range order {
		rec := idx.Records[recIndex]
		path := idx.reconstructCompactPathCached(recIndex, pathCache)
		entry := Entry{
			Path:        path,
			Name:        rec.Name,
			LowerPath:   strings.ToLower(path),
			LowerName:   rec.LowerName,
			Mode:        rec.Mode,
			Size:        rec.Size,
			ModUnix:     rec.ModUnix,
			IndexSource: idx.Source,
		}
		if entryMatches(entry, pq, opts.MatchPath) {
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
		CaseSensitive: opts.CaseSensitive,
		Under:         normalizeFilterPath(opts.Under),
		Exists:        opts.Exists,
		CWDBias:       normalizeFilterPath(opts.CWDBias),
		RootBias:      normalizeFilterPath(opts.RootBias),
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
		case strings.HasPrefix(raw, "glob:"):
			glob := strings.TrimPrefix(raw, "glob:")
			if glob != "" {
				pq.Globs = append(pq.Globs, normalizeCase(glob, pq.CaseSensitive))
			}
		case strings.HasPrefix(raw, "regex:"):
			pat := strings.TrimPrefix(raw, "regex:")
			if pat == "" {
				continue
			}
			if !pq.CaseSensitive {
				pat = "(?i)" + pat
			}
			re, err := regexp.Compile(pat)
			if err != nil {
				return pq, fmt.Errorf("invalid regex %q: %w", pat, err)
			}
			pq.Regexps = append(pq.Regexps, re)
		case raw == "case:" || raw == "case:true":
			pq.CaseSensitive = true
		case raw == "case:false":
			pq.CaseSensitive = false
		case raw == "type:file" || raw == "type:dir":
			pq.Type = strings.TrimPrefix(raw, "type:")
		default:
			pq.Terms = append(pq.Terms, normalizeCase(raw, pq.CaseSensitive))
		}
	}
	if len(pq.Terms) == 0 && len(pq.Exts) == 0 && len(pq.Dirs) == 0 && len(pq.Globs) == 0 && len(pq.Regexps) == 0 && pq.Type == "" && pq.Under == "" && !pq.HasModAfter {
		return pq, errors.New("query has no searchable terms or filters")
	}
	return pq, nil
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

func (idx *Index) pathCandidates(terms []string) ([]int, bool) {
	if len(idx.PathTokenIndex) == 0 || len(terms) == 0 {
		return nil, false
	}
	var lists [][]int
	for _, term := range terms {
		list, ok := idx.PathTokenIndex[term]
		if !ok {
			return []int{}, true
		}
		lists = append(lists, list)
	}
	sort.Slice(lists, func(i, j int) bool { return len(lists[i]) < len(lists[j]) })
	candidates := append([]int(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSorted(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	return candidates, true
}

func intersectSorted(a, b []int) []int {
	out := a[:0]
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] == b[j] {
			out = append(out, a[i])
			i++
			j++
		} else if a[i] < b[j] {
			i++
		} else {
			j++
		}
	}
	return out
}

func (idx *Index) compactPathContainsTerm(i int, term string) bool {
	if idx.Volume != "" && strings.Contains(strings.ToLower(idx.Volume), term) {
		return true
	}
	seen := make(map[int]struct{}, 16)
	cur := i
	for depth := 0; depth < 1024; depth++ {
		if cur < 0 || cur >= len(idx.Records) {
			return false
		}
		if _, ok := seen[cur]; ok {
			return false
		}
		seen[cur] = struct{}{}
		rec := idx.Records[cur]
		if strings.Contains(rec.LowerName, term) {
			return true
		}
		if rec.Parent < 0 {
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
	if i < 0 || i >= len(idx.Records) {
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
		if cur < 0 || cur >= len(idx.Records) {
			break
		}
		if _, ok := seen[cur]; ok {
			break
		}
		seen[cur] = struct{}{}
		rec := idx.Records[cur]
		parts = append(parts, rec.Name)
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

func buildPathTokenIndex(idx *Index) {
	idx.PathTokenIndex = make(map[string][]int, len(idx.Records)/4)
	pathCache := make(map[int]string)
	for i := range idx.Records {
		path := strings.ToLower(idx.reconstructCompactPathCached(i, pathCache))
		tokens := tokenizePath(path)
		for token := range tokens {
			idx.PathTokenIndex[token] = append(idx.PathTokenIndex[token], i)
		}
	}
	for token := range idx.PathTokenIndex {
		sort.Ints(idx.PathTokenIndex[token])
	}
}

func tokenizePath(path string) map[string]struct{} {
	out := make(map[string]struct{}, 8)
	start := -1
	for i, r := range path {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			addTokenParts(out, path[start:i])
			start = -1
		}
	}
	if start >= 0 {
		addTokenParts(out, path[start:])
	}
	return out
}

func addTokenParts(out map[string]struct{}, token string) {
	if len(token) >= 2 {
		out[token] = struct{}{}
	}
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
		header.Compact = 1
		header.EntryCount = uint64(len(idx.Records))
		nameIDs := make(map[string]uint32, len(idx.Records)/2)
		nameBlob := make([]byte, 0, len(idx.Records)*16)
		nameOffs = make([]uint32, 0, len(idx.Records))
		nameLens = make([]uint16, 0, len(idx.Records))
		for i := range idx.Records {
			rec := &idx.Records[i]
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
			rec.NameOff = id
			rec.NameLen = 0
		}
		idx.NameBlob = nameBlob
		header.NameBlobLen = uint64(len(nameBlob))
		header.TokenCount = uint64(len(nameOffs))
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
		for _, rec := range idx.Records {
			parent := uint32(0xFFFFFF)
			if rec.Parent >= 0 {
				parent = uint32(rec.Parent)
			}
			if parent > 0xFFFFFF || rec.NameOff > 0xFFFFFF {
				return errors.New("compact index too large for packed record format")
			}
			if err := writeUint24(w, parent); err != nil {
				return err
			}
			if err := writeUint24(w, rec.NameOff); err != nil {
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
	var header diskHeader
	if err := binary.Read(r, binary.LittleEndian, &header); err != nil {
		return nil, err
	}
	if header.Magic != indexMagic {
		return nil, errors.New("unsupported index format")
	}
	if header.Version != indexVersion {
		return nil, fmt.Errorf("unsupported index version %d", header.Version)
	}
	if header.EntryCount > uint64(^uint(0)>>1) || header.RootCount > uint64(^uint(0)>>1) {
		return nil, errors.New("index too large")
	}
	idx := &Index{
		Version:    indexVersion,
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
			parent, err := readUint24(r)
			if err != nil {
				return nil, err
			}
			if parent == 0xFFFFFF {
				rec.Parent = -1
			} else {
				rec.Parent = int32(parent)
			}
			if rec.NameOff, err = readUint24(r); err != nil {
				return nil, err
			}
			if int(rec.NameOff) >= len(nameOffs) {
				return nil, errors.New("invalid compact name id")
			}
			off := nameOffs[rec.NameOff]
			length := nameLens[rec.NameOff]
			end := int(off) + int(length)
			if end < int(off) || end > len(idx.NameBlob) {
				return nil, errors.New("invalid compact name reference")
			}
			rec.Name = string(idx.NameBlob[int(off):end])
			rec.LowerName = strings.ToLower(rec.Name)
		}
		idx.CompactNameOrder = make([]int, len(idx.Records))
		for i := range idx.CompactNameOrder {
			idx.CompactNameOrder[i] = i
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
	tmp := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 16*1024*1024)
	err = writeIndex(bw, idx)
	if flushErr := bw.Flush(); err == nil {
		err = flushErr
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
	_ = os.Remove(path)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func loadIndex(path string) (*Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readIndex(bufio.NewReaderSize(f, 16*1024*1024))
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
	if dir, err := os.UserConfigDir(); err == nil {
		candidates = append(candidates, filepath.Join(dir, "seekfs", "seekfs.toml"))
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
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

func loadIndexes(paths []string, tokenPath string) ([]*Index, error) {
	indexes := make([]*Index, 0, len(paths))
	for _, path := range paths {
		idx, err := loadIndex(path)
		if err != nil {
			return nil, err
		}
		idx.DBPath = path
		if err := loadOptionalTokenSidecar(idx, path, tokenPath); err != nil {
			return nil, err
		}
		indexes = append(indexes, idx)
	}
	return indexes, nil
}

func loadOptionalTokenSidecar(idx *Index, dbPath, tokenPath string) error {
	if tokenPath == "" {
		tokenPath = dbPath + ".tok"
		if _, err := os.Stat(tokenPath); errors.Is(err, os.ErrNotExist) {
			return nil
		}
	}
	postings, err := loadTokenSidecar(tokenPath)
	if err != nil {
		return err
	}
	idx.PathTokenIndex = postings
	return nil
}

func saveTokenSidecar(path string, postings map[string][]int) error {
	tmp := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 4*1024*1024)
	err = writeTokenSidecar(bw, postings)
	if flushErr := bw.Flush(); err == nil {
		err = flushErr
	}
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	_ = os.Remove(path)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func writeTokenSidecar(w io.Writer, postings map[string][]int) error {
	if err := binary.Write(w, binary.LittleEndian, tokenSidecarMagic); err != nil {
		return err
	}
	tokens := make([]string, 0, len(postings))
	for token := range postings {
		tokens = append(tokens, token)
	}
	sort.Strings(tokens)
	if err := binary.Write(w, binary.LittleEndian, uint32(len(tokens))); err != nil {
		return err
	}
	var buf [binary.MaxVarintLen64]byte
	for _, token := range tokens {
		if err := writeString(w, token); err != nil {
			return err
		}
		list := postings[token]
		if err := binary.Write(w, binary.LittleEndian, uint32(len(list))); err != nil {
			return err
		}
		prev := 0
		for i, value := range list {
			delta := value
			if i > 0 {
				delta = value - prev
			}
			n := binary.PutUvarint(buf[:], uint64(delta))
			if _, err := w.Write(buf[:n]); err != nil {
				return err
			}
			prev = value
		}
	}
	return nil
}

func loadTokenSidecar(path string) (map[string][]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 4*1024*1024)
	var magic [8]byte
	if err := binary.Read(br, binary.LittleEndian, &magic); err != nil {
		return nil, err
	}
	if magic != tokenSidecarMagic {
		return nil, errors.New("unsupported token sidecar format")
	}
	var count uint32
	if err := binary.Read(br, binary.LittleEndian, &count); err != nil {
		return nil, err
	}
	postings := make(map[string][]int, int(count))
	for i := uint32(0); i < count; i++ {
		token, err := readString(br)
		if err != nil {
			return nil, err
		}
		var n uint32
		if err := binary.Read(br, binary.LittleEndian, &n); err != nil {
			return nil, err
		}
		list := make([]int, int(n))
		prev := 0
		for j := range list {
			delta, err := binary.ReadUvarint(br)
			if err != nil {
				return nil, err
			}
			value := int(delta)
			if j > 0 {
				value += prev
			}
			list[j] = value
			prev = value
		}
		postings[token] = list
	}
	return postings, nil
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
