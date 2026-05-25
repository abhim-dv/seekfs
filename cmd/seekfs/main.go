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
	"os"
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

const indexVersion = 8

var indexMagic = [8]byte{'G', 'O', 'S', 'R', 'C', 'H', '0', '8'}

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
	usnReasonFileCreate  = 0x00000100
	usnReasonFileDelete  = 0x00000200
	usnReasonRenameOld   = 0x00001000
	usnReasonRenameNew   = 0x00002000
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

type doctorResponse struct {
	OK            bool   `json:"ok"`
	ServiceName   string `json:"service_name"`
	Installed     bool   `json:"installed"`
	Running       bool   `json:"running"`
	PipeReachable bool   `json:"pipe_reachable"`
	Entries       int    `json:"entries,omitempty"`
	QueryOK       bool   `json:"query_ok"`
	Message       string `json:"message,omitempty"`
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
	DBPath           string
}

type appConfig struct {
	DBs          []string
	Volumes      []string
	ServicePipe  string
	DefaultLimit int
	OutputFormat string
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
	FRN       uint64
	ParentFRN uint64
	Parent    int32
	Name      string
	LowerName string
	NameOff   uint32
	NameLen   uint16
	Mode      uint32
	Size      int64
	ModUnix   int64
	Deleted   bool
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
	case "index-volumes":
		return cmdIndexVolumes(args[1:])
	case "service":
		return cmdService(args[1:])
	case "install":
		return cmdInstallService(args[1:])
	case "launch":
		return cmdLaunch(args[1:])
	case "start":
		return cmdControlService(args[1:], "start")
	case "stop":
		return cmdControlService(args[1:], "stop")
	case "restart":
		return cmdControlService(args[1:], "restart")
	case "status":
		return cmdServiceSimple(args[1:], "status")
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
	case "loaded":
		return cmdServiceInfo(args[1:])
	case "bench":
		return cmdBenchAgent(args[1:])
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
		return usage()
	}
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
  seekfs launch [-db index.gsi...] [--json]
  seekfs install [-pipe \\.\pipe\seekfs-service] [-sddl <sddl>] [-db index.gsi...]
  seekfs start|stop|restart
  seekfs uninstall
  seekfs doctor [--json]
  seekfs status [--json]
  seekfs loaded [--json]
  seekfs defaults [--json]
  seekfs config path|show|get|set
  seekfs service-index-usn -volume C: -db seekfs.gsi [-pipe \\.\pipe\seekfs-service]
  seekfs bench [-db index.gsi...] [-service] [--json] [-iterations 100]
  seekfs info [-db seekfs.gsi] [--json]
  seekfs syntax
  seekfs agent
  seekfs search [-db seekfs.db...] [-service] [--json] [-n 100] [-path] <query>
  seekfs count [-db seekfs.db...] [-service] [--json] [-path] <query>
  seekfs version

Agent starting points:
  seekfs agent
  seekfs search -service --json -path -n 20 "ext:go dir:cmd main"
  seekfs count  -service --json -path "type:file ext:go"`)
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
  glob:*.py         Match file name glob.
  regex:<pattern>   Match normalized full path regex.
  case:             Enable case-sensitive matching from the query.
  type:file         Only files.
  type:dir          Only directories.

Examples:
  seekfs search -service --json -path -n 20 "ext:go dir:cmd main"
  seekfs search -service --json -path --under F:\git\seekfs "type:file glob:*.md"
  seekfs search -service --json -path --exists --recent 24h "ext:go"
  seekfs count  -service --json -path "type:dir docs"

Not implemented yet:
  Everything filters such as dm:, size:, attrib:, parent:
  OR / NOT operators
  quoted phrase parsing beyond the shell's normal argument grouping
  date macros such as today or lastweek
  ranking compatible with Everything
`)
}

func printAgentHelp() {
	fmt.Print(`seekfs agent help

Purpose:
  Agent-first indexed file search for local filesystems. Prefer service mode
  for low latency; it avoids loading large indexes on each CLI invocation.

Recommended commands:
  seekfs loaded --json
  seekfs search -service --json -path -n 20 "ext:go dir:cmd main"
  seekfs count  -service --json -path "type:file ext:go"
  seekfs launch -db F:\seekfs_c.gsi -db F:\seekfs_f.gsi
  seekfs config set output_format json
  seekfs bench -service --json -iterations 100

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
  -service            Query the installed resident service.
  -path               Match full paths, not just names.
  -n 20               Keep result sets bounded.
  --under <path>      Constrain search to a workspace.
  --exists            Filter stale index entries.
  --cwd-bias          Prefer current repo paths.
  --root-bias <path>  Prefer a specific repo/root.

Query filters:
  ext:go, dir:src, glob:*.py, regex:<pattern>, case:, type:file, type:dir

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
			FRN:       frn,
			ParentFRN: node.parentFRN,
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

func readUSNChanges(handle windows.Handle, journalID uint64, startUSN int64, buffer []byte) (int64, []usnChange, error) {
	if len(buffer) < 4096 {
		buffer = make([]byte, 4096)
	}
	req := readUSNJournalDataV0{
		StartUsn:          startUSN,
		ReasonMask:        0xffffffff,
		ReturnOnlyOnClose: 0,
		Timeout:           0,
		BytesToWaitFor:    0,
		UsnJournalID:      journalID,
	}
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
			nameLen := binary.LittleEndian.Uint16(record[56:58])
			nameOff := binary.LittleEndian.Uint16(record[58:60])
			if uint32(nameOff)+uint32(nameLen) > recordLen {
				return nil, errors.New("invalid USN record name")
			}
			nameBytes := record[nameOff : uint32(nameOff)+uint32(nameLen)]
			changes = append(changes, usnChange{
				FRN:       binary.LittleEndian.Uint64(record[8:16]),
				ParentFRN: binary.LittleEndian.Uint64(record[16:24]),
				USN:       int64(binary.LittleEndian.Uint64(record[24:32])),
				Reason:    binary.LittleEndian.Uint32(record[40:44]),
				Attr:      binary.LittleEndian.Uint32(record[52:56]),
				Name:      windows.UTF16ToString(bytesToUTF16(nameBytes)),
			})
		}
		pos += recordLen
	}
	return changes, nil
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
	if cfg.OutputFormat == "json" {
		*jsonOut = true
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
	if cfg.OutputFormat == "json" {
		*jsonOut = true
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

func cmdBenchAgent(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
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
	if cfg.OutputFormat == "json" {
		*jsonOut = true
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
		indexes, err = loadIndexes(dbs)
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
	Loading bool     `json:"loading,omitempty"`
	Count   int      `json:"count,omitempty"`
	Results []string `json:"results,omitempty"`
	DBs     []dbInfo `json:"dbs,omitempty"`
}

type dbInfo struct {
	Path        string `json:"path"`
	Entries     int    `json:"entries"`
	Source      string `json:"source"`
	BuiltAt     string `json:"built_at"`
	Volume      string `json:"volume,omitempty"`
	JournalID   uint64 `json:"journal_id,omitempty"`
	Checkpoint  int64  `json:"checkpoint_usn,omitempty"`
	State       string `json:"state,omitempty"`
	StaleReason string `json:"stale_reason,omitempty"`
	FRNRecords  int    `json:"frn_records,omitempty"`
}

type goSearchService struct {
	pipeName string
	sddl     string
	stop     chan struct{}
	dbs      []string
	indexes  []*Index
	volumes  []*serviceVolumeIndex
	loading  bool
	loadErr  string
	indexMu  sync.RWMutex
}

type serviceVolumeIndex struct {
	dbPath      string
	index       *Index
	volume      string
	journalID   uint64
	checkpoint  int64
	state       string
	staleReason string
	frnToID     map[uint64]int
	pathCache   map[int]string
	termMu      sync.Mutex
	termCache   map[string][]int
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
	handler := &goSearchService{pipeName: *pipeName, sddl: *sddl, stop: make(chan struct{}), dbs: dbs}
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
	}
	info, err := callService(pipeName, serviceRequest{Command: "info"})
	if err == nil && info.OK {
		resp.PipeReachable = true
		resp.Entries = info.Entries
	}
	if err == nil && info.Loading {
		resp.PipeReachable = true
		resp.Message = "seekfs service is loading indexes"
	}
	searchResp, searchErr := callService(pipeName, serviceRequestFromOptions(queryOptions{Query: "ext:go", MatchPath: true, Limit: 1}, false))
	if searchErr == nil && searchResp.OK {
		resp.QueryOK = true
	}
	resp.OK = resp.Installed && resp.Running && resp.PipeReachable && resp.Entries > 0 && resp.QueryOK
	if !resp.OK && resp.Message == "" {
		resp.Message = "seekfs service is not fully healthy"
	}
	return resp
}

func waitForDoctor(pipeName string, timeout time.Duration) doctorResponse {
	deadline := time.Now().Add(timeout)
	var resp doctorResponse
	for {
		resp = probeDoctor(pipeName)
		if resp.OK || time.Now().After(deadline) {
			return resp
		}
		time.Sleep(500 * time.Millisecond)
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
	done := make(chan struct{})
	go func() {
		s.servePrivileged()
		close(done)
	}()
	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	go func() {
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
	if err := s.loadConfiguredIndexes(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "seekfs privileged service listening on %s\n", s.pipeName)
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
	indexes, err := loadIndexes(s.dbs)
	if err != nil {
		s.indexMu.Lock()
		s.loadErr = err.Error()
		s.indexMu.Unlock()
		return err
	}
	volumes := make([]*serviceVolumeIndex, 0, len(indexes))
	total := 0
	for i, idx := range indexes {
		total += idx.entryCount()
		dbPath := ""
		if i < len(s.dbs) {
			dbPath = s.dbs[i]
		}
		vol := newServiceVolumeIndex(dbPath, idx)
		if err := catchUpServiceVolume(vol); err != nil {
			serviceLog("startup catch-up skipped volume=%s db=%s err=%v", vol.volume, vol.dbPath, err)
		}
		volumes = append(volumes, vol)
	}
	s.indexMu.Lock()
	s.indexes = indexes
	s.volumes = volumes
	s.loadErr = ""
	s.indexMu.Unlock()
	for _, vol := range volumes {
		if vol.state == "ready" && vol.index.Compact && vol.index.Source == "usn" {
			go s.replayVolumeLoop(vol)
		}
	}
	serviceLog("loaded %d dbs entries=%d elapsed=%s", len(indexes), total, time.Since(start).Round(time.Millisecond))
	return nil
}

func newServiceVolumeIndex(dbPath string, idx *Index) *serviceVolumeIndex {
	vol := &serviceVolumeIndex{
		dbPath:     dbPath,
		index:      idx,
		volume:     idx.Volume,
		journalID:  idx.JournalID,
		checkpoint: idx.Checkpoint,
		state:      "ready",
		pathCache:  make(map[int]string),
	}
	if idx.Compact && idx.Source == "usn" {
		vol.frnToID = make(map[uint64]int, len(idx.Records))
		for i, rec := range idx.Records {
			if rec.FRN != 0 {
				vol.frnToID[rec.FRN] = i
			}
		}
	}
	return vol
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
		vol.applyUSNChanges(changes)
		vol.checkpoint = nextUSN
		vol.index.Checkpoint = nextUSN
	}
	if vol.dbPath != "" {
		if err := saveIndex(vol.dbPath, vol.index); err != nil {
			vol.state = "stale"
			vol.staleReason = err.Error()
			return err
		}
	}
	vol.state = "ready"
	vol.staleReason = ""
	return nil
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

	nextUSN, changes, err := readUSNChanges(handle, journalID, startUSN, buffer)
	if err != nil {
		return err
	}
	if nextUSN <= startUSN {
		return nil
	}
	s.indexMu.Lock()
	vol.applyUSNChanges(changes)
	vol.checkpoint = nextUSN
	vol.index.Checkpoint = nextUSN
	vol.state = "ready"
	vol.staleReason = ""
	err = saveIndex(vol.dbPath, vol.index)
	s.indexMu.Unlock()
	return err
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
		return fmt.Errorf("journal id changed from %d to %d", vol.journalID, journal.UsnJournalID)
	}
	firstValid := journal.FirstUsn
	if journal.LowestValidUsn > firstValid {
		firstValid = journal.LowestValidUsn
	}
	if vol.checkpoint < firstValid {
		return fmt.Errorf("checkpoint %d is before first valid USN %d", vol.checkpoint, firstValid)
	}
	if vol.checkpoint > journal.NextUsn {
		return fmt.Errorf("checkpoint %d is after journal next USN %d", vol.checkpoint, journal.NextUsn)
	}
	return nil
}

func (vol *serviceVolumeIndex) applyUSNChanges(changes []usnChange) {
	if vol.frnToID == nil {
		vol.frnToID = make(map[uint64]int)
	}
	for _, change := range changes {
		if change.FRN == 0 {
			continue
		}
		if change.Reason&usnReasonRenameOld != 0 && change.Reason&usnReasonRenameNew == 0 {
			continue
		}
		if change.Reason&usnReasonFileDelete != 0 {
			if id, ok := vol.frnToID[change.FRN]; ok {
				vol.index.Records[id].Deleted = true
			}
			if change.USN > vol.checkpoint {
				vol.checkpoint = change.USN
			}
			continue
		}
		id, ok := vol.frnToID[change.FRN]
		if !ok {
			id = len(vol.index.Records)
			vol.frnToID[change.FRN] = id
			vol.index.Records = append(vol.index.Records, CompactRecord{FRN: change.FRN})
		}
		rec := &vol.index.Records[id]
		rec.FRN = change.FRN
		rec.ParentFRN = change.ParentFRN
		rec.Parent = -1
		if parentID, ok := vol.frnToID[change.ParentFRN]; ok && parentID != id {
			rec.Parent = int32(parentID)
		}
		if change.Name != "" {
			rec.Name = change.Name
			rec.LowerName = strings.ToLower(change.Name)
		}
		rec.Mode = modeFromAttrs(change.Attr)
		rec.Deleted = false
		if change.USN > vol.checkpoint {
			vol.checkpoint = change.USN
		}
	}
	vol.index.Checkpoint = vol.checkpoint
	vol.pathCache = make(map[int]string)
	vol.termMu.Lock()
	vol.termCache = nil
	vol.termMu.Unlock()
	vol.index.CompactNameOrder = make([]int, len(vol.index.Records))
	for i := range vol.index.CompactNameOrder {
		vol.index.CompactNameOrder[i] = i
	}
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
		volumes := append([]*serviceVolumeIndex(nil), s.volumes...)
		loading := s.loading
		loadErr := s.loadErr
		s.indexMu.RUnlock()
		infos := make([]dbInfo, 0, len(volumes))
		total := 0
		for _, vol := range volumes {
			idx := vol.index
			total += idx.entryCount()
			infos = append(infos, dbInfo{
				Path:        vol.dbPath,
				Entries:     idx.entryCount(),
				Source:      idx.Source,
				BuiltAt:     idx.BuiltAt.Format(time.RFC3339Nano),
				Volume:      vol.volume,
				JournalID:   vol.journalID,
				Checkpoint:  vol.checkpoint,
				State:       vol.state,
				StaleReason: vol.staleReason,
				FRNRecords:  len(vol.frnToID),
			})
		}
		message := ""
		if loading {
			message = "loading indexes"
		} else if loadErr != "" {
			message = loadErr
		}
		_ = json.NewEncoder(conn).Encode(serviceResponse{OK: loadErr == "", Message: message, Entries: total, Loading: loading, DBs: infos})
	case "search":
		s.indexMu.RLock()
		if len(s.indexes) == 0 {
			s.indexMu.RUnlock()
			_ = json.NewEncoder(conn).Encode(serviceResponse{OK: false, Message: "service has no search indexes loaded"})
			return
		}
		opts := requestToOptionsFromService(req)
		var matches []Entry
		var err error
		if len(s.volumes) == len(s.indexes) {
			matches, err = searchServiceVolumes(s.volumes, opts, req.CountOnly)
		} else {
			matches, err = searchAll(s.indexes, opts, req.CountOnly)
		}
		s.indexMu.RUnlock()
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
	case "status":
		_ = json.NewEncoder(conn).Encode(serviceResponse{OK: true, Message: "service running"})
	default:
		_ = json.NewEncoder(conn).Encode(serviceResponse{OK: false, Message: "unknown command"})
	}
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

func searchServiceVolumes(volumes []*serviceVolumeIndex, opts queryOptions, countOnly bool) ([]Entry, error) {
	if len(volumes) == 1 {
		return searchCompactWithCache(volumes[0].index, opts, countOnly, volumes[0].pathCache, volumes[0].nameTermCandidates)
	}
	limit := normalizedLimit(opts.Limit, countOnly)
	results := make([]Entry, 0, min(limit, 1024))
	for _, vol := range volumes {
		perIndexLimit := limit
		if !countOnly && limit > 0 {
			perIndexLimit = limit - len(results)
			if perIndexLimit <= 0 {
				break
			}
		}
		childOpts := opts
		childOpts.Limit = perIndexLimit
		matches, err := searchCompactWithCache(vol.index, childOpts, countOnly, vol.pathCache, vol.nameTermCandidates)
		if err != nil {
			return nil, err
		}
		results = append(results, matches...)
	}
	return results, nil
}

func searchCompact(idx *Index, opts queryOptions, countOnly bool) ([]Entry, error) {
	return searchCompactWithCache(idx, opts, countOnly, nil, nil)
}

func searchCompactWithCache(idx *Index, opts queryOptions, countOnly bool, pathCache map[int]string, candidateFn func(parsedQuery) ([]int, bool)) ([]Entry, error) {
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, err
	}
	limit := normalizedLimit(opts.Limit, countOnly)
	order := idx.CompactNameOrder
	if order == nil {
		order = make([]int, len(idx.Records))
		for i := range order {
			order[i] = i
		}
	}
	if opts.MatchPath && !countOnly {
		if candidateFn != nil {
			if candidates, ok := candidateFn(pq); ok {
				order = candidates
			}
		}
	}
	if pq.RootBias != "" || pq.CWDBias != "" {
		order = idx.biasOrderCompact(order, firstNonEmpty(pq.CWDBias, pq.RootBias))
	}
	results := make([]Entry, 0, min(limit, 1024))
	if pathCache == nil {
		pathCache = make(map[int]string)
	}
	for _, recIndex := range order {
		rec := idx.Records[recIndex]
		if rec.Deleted {
			continue
		}
		if !compactRecordPrecheck(rec, pq, opts.MatchPath) {
			continue
		}
		if opts.MatchPath && len(pq.Terms) > 0 && !idx.compactPathContainsAll(recIndex, pq.Terms) {
			continue
		}
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
	if vol == nil || vol.index == nil || len(pq.Terms) == 0 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || pq.Under != "" {
		return nil, false
	}
	lists := make([][]int, 0, len(pq.Terms))
	for _, term := range pq.Terms {
		list := vol.nameTermPosting(term)
		if len(list) == 0 {
			return nil, false
		}
		lists = append(lists, list)
	}
	sort.Slice(lists, func(i, j int) bool { return len(lists[i]) < len(lists[j]) })
	candidates := append([]int(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSortedInts(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	return candidates, len(candidates) > 0
}

func (vol *serviceVolumeIndex) nameTermPosting(term string) []int {
	vol.termMu.Lock()
	defer vol.termMu.Unlock()
	if vol.termCache == nil {
		vol.termCache = make(map[string][]int)
	}
	if list, ok := vol.termCache[term]; ok {
		return list
	}
	list := make([]int, 0, 64)
	for i, rec := range vol.index.Records {
		if rec.Deleted {
			continue
		}
		if strings.Contains(rec.LowerName, term) {
			list = append(list, i)
		}
	}
	vol.termCache[term] = list
	return list
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
			if err := binary.Write(w, binary.LittleEndian, rec.FRN); err != nil {
				return err
			}
			if err := binary.Write(w, binary.LittleEndian, rec.ParentFRN); err != nil {
				return err
			}
			if err := writeUint24(w, parent); err != nil {
				return err
			}
			if err := writeUint24(w, rec.NameOff); err != nil {
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
			if err := binary.Read(r, binary.LittleEndian, &rec.FRN); err != nil {
				return nil, err
			}
			if err := binary.Read(r, binary.LittleEndian, &rec.ParentFRN); err != nil {
				return nil, err
			}
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
