package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"
)

// watchEntry is one indexed file snapshot: path plus the metadata used to
// detect modification between polls.
type watchEntry struct {
	path  string
	size  int64
	mtime time.Time
}

// watchEvent is one JSON line emitted on stdout.
type watchEvent struct {
	Ts    string `json:"ts"`
	Event string `json:"event"`
	Path  string `json:"path"`
	Size  *int64 `json:"size,omitempty"`
	Mtime string `json:"mtime,omitempty"`
}

// watchVolumeCursor is a per-volume delta cursor: the overlay watermark
// (len of overlay.records at snapshot time) for one volume. The watch client
// tracks one cursor per volume and sends them back so each volume's deltas
// resume exactly where the previous tick stopped. Reset indicates the volume's
// overlay was rebuilt (background persist) and the client must re-baseline.
type watchVolumeCursor struct {
	Volume string `json:"volume,omitempty"`
	Seq    uint64 `json:"seq"`
	Reset  bool   `json:"reset,omitempty"`
}

// watchDeltaEvent is one change reported by the service's watch-delta
// endpoint. It is produced server-side from the overlay change stream, so
// the watch client only pays for changed records instead of re-running the
// full query every tick.
type watchDeltaEvent struct {
	Volume string `json:"volume,omitempty"`
	Event  string `json:"event"`
	Path   string `json:"path"`
	Size   int64  `json:"size,omitempty"`
	Mtime  string `json:"mtime,omitempty"`
}

func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	interval := fs.Duration("interval", 2*time.Second, "poll cadence")
	limit := fs.Int("n", 10000, "maximum events per tick")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	matchPath := fs.Bool("path", false, "match full path")
	under := fs.String("under", "", "only report changes under this path")
	exists := fs.Bool("exists", false, "verify result paths still exist")
	cwdBias := fs.Bool("cwd-bias", false, "rank paths under the current working directory first")
	rootBias := fs.String("root-bias", "", "rank paths under this root first")
	recent := fs.String("recent", "", "only report changes modified within this duration, for example 24h")
	modifiedAfter := fs.String("modified-after", "", "only report changes modified after RFC3339 time or YYYY-MM-DD")
	caseSensitive := fs.Bool("case", false, "case-sensitive query matching")
	fuzzy := fs.Bool("fuzzy", false, "append close matches (edit distance) below exact results")
	execCmd := fs.String("exec", "", "run this command for each event; {} is replaced by the path")
	execOn := fs.String("exec-on", "created,modified,deleted", "comma-separated events that trigger -exec")
	execShell := fs.Bool("exec-shell", false, "run -exec through cmd /C")
	if err := fs.Parse(normalizeSearchArgs(args)); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	queryArgs := append([]string(nil), fs.Args()...)
	if *matchPath && *under == "" {
		queryArgs, *under = extractUnderPathArg(queryArgs)
	}
	query := strings.TrimSpace(strings.Join(queryArgs, " "))
	if query == "" {
		return errors.New("query required")
	}
	if *interval <= 0 {
		return errors.New("-interval must be positive")
	}
	var execEvents map[string]bool
	if *execCmd != "" {
		execEvents = make(map[string]bool)
		for _, e := range strings.Split(*execOn, ",") {
			execEvents[strings.TrimSpace(e)] = true
		}
		if len(execEvents) == 0 {
			return errors.New("-exec-on must list at least one event")
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	enc := json.NewEncoder(os.Stdout)
	var cursors []watchVolumeCursor
	baselined := false
	inOutage := false

	mkOpts := func() queryOptions {
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
			Fuzzy:         *fuzzy,
		}
		if *cwdBias {
			if cwd, err := os.Getwd(); err == nil {
				opts.CWDBias = cwd
			}
		}
		return opts
	}

	deltaReq := func(since []watchVolumeCursor, baseline bool) (serviceResponse, error) {
		req := serviceRequestFromOptions(mkOpts(), false)
		req.Command = "watch-delta"
		req.SinceVolumes = since
		req.Baseline = baseline
		resp, err := callService(*pipeName, req)
		if err != nil {
			return resp, err
		}
		if !resp.OK {
			return resp, errors.New(resp.Message)
		}
		return resp, nil
	}

	emit := func(ev watchDeltaEvent) {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		out := watchEvent{Ts: now, Event: ev.Event, Path: ev.Path}
		if ev.Size != 0 {
			size := ev.Size
			out.Size = &size
		}
		if ev.Mtime != "" {
			out.Mtime = ev.Mtime
		}
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(os.Stderr, "watch: write event: %v\n", err)
			return
		}
		if *execCmd != "" && execEvents[ev.Event] {
			runWatchExec(*execCmd, ev.Path, *execShell)
		}
	}

	baseline := func() error {
		resp, err := deltaReq(nil, true)
		if err != nil {
			return err
		}
		cursors = resp.WatchVolumes
		baselined = true
		fmt.Fprintf(os.Stderr, "watching (delta) query=%q poll=%s pipe=%s\n", query, *interval, *pipeName)
		return nil
	}

	tick := func() {
		if !baselined {
			if err := baseline(); err != nil {
				if !inOutage {
					fmt.Fprintln(os.Stderr, "service unreachable, retrying...")
					inOutage = true
				}
				return
			}
			inOutage = false
			return
		}
		resp, err := deltaReq(cursors, false)
		if err != nil {
			if !inOutage {
				fmt.Fprintln(os.Stderr, "service unreachable, retrying...")
				inOutage = true
			}
			return
		}
		if inOutage {
			fmt.Fprintln(os.Stderr, "service connection restored")
			inOutage = false
		}
		// Any volume whose overlay was rebuilt needs a fresh baseline; a
		// changed volume reappears with a new cursor in the next tick.
		needRebaseline := false
		for _, c := range resp.WatchVolumes {
			if c.Reset {
				needRebaseline = true
				break
			}
		}
		if needRebaseline {
			cursors = nil
			baselined = false
			return
		}
		for _, ev := range resp.WatchEvents {
			emit(ev)
		}
		cursors = resp.WatchVolumes
	}

	tick()
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			tick()
		}
	}
}

// runWatchExec launches the user's -exec command for one watch event. The
// placeholder {} in the command is replaced by the event path. Without
// -exec-shell the command is split on whitespace and executed directly;
// with it, the command is passed to cmd /C for full shell semantics.
func runWatchExec(cmdTemplate, path string, shell bool) {
	cmdText := strings.ReplaceAll(cmdTemplate, "{}", strconv.Quote(path))
	if shell {
		c := exec.Command("cmd", "/C", cmdText)
		c.Stdout = os.Stderr
		c.Stderr = os.Stderr
		if err := c.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "watch: exec start: %v\n", err)
			return
		}
		return
	}
	parts := strings.Fields(cmdText)
	if len(parts) == 0 {
		return
	}
	c := exec.Command(parts[0], parts[1:]...)
	c.Stdout = os.Stderr
	c.Stderr = os.Stderr
	if err := c.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "watch: exec start: %v\n", err)
		return
	}
}

// snapshotFromResponse collects the current result set keyed by path,
// preferring per-entry row metadata over bare paths.
func snapshotFromResponse(resp serviceResponse) map[string]watchEntry {
	out := make(map[string]watchEntry, max(len(resp.Rows), len(resp.Results)))
	if len(resp.Rows) > 0 {
		for _, row := range resp.Rows {
			if row.Path == "" {
				continue
			}
			entry := watchEntry{path: row.Path}
			if row.Size != nil {
				entry.size = *row.Size
			}
			if row.Modified != "" {
				if t, err := time.Parse(time.RFC3339Nano, row.Modified); err == nil {
					entry.mtime = t
				}
			}
			out[row.Path] = entry
		}
		return out
	}
	for _, path := range resp.Results {
		out[path] = watchEntry{path: path}
	}
	return out
}

// diffWatchSnapshots compares two snapshots and returns paths grouped by
// change kind, each slice sorted alphabetically. A file counts as modified
// when its size or mtime differs between ticks.
func diffWatchSnapshots(prev, next map[string]watchEntry) (created, modified, deleted []string) {
	for path, cur := range next {
		old, ok := prev[path]
		if !ok {
			created = append(created, path)
			continue
		}
		if old.size != cur.size || !old.mtime.Equal(cur.mtime) {
			modified = append(modified, path)
		}
	}
	for path := range prev {
		if _, ok := next[path]; !ok {
			deleted = append(deleted, path)
		}
	}
	sort.Strings(created)
	sort.Strings(modified)
	sort.Strings(deleted)
	return created, modified, deleted
}
