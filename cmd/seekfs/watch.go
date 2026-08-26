package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
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

func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	interval := fs.Duration("interval", 2*time.Second, "poll cadence")
	limit := fs.Int("n", 10000, "maximum results per tick")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	configPath := fs.String("config", "", "optional seekfs.toml config path")
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
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return errors.New("query required")
	}
	if *interval <= 0 {
		return errors.New("-interval must be positive")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	enc := json.NewEncoder(os.Stdout)
	snapshot := make(map[string]watchEntry)
	baselined := false
	inOutage := false

	poll := func() (map[string]watchEntry, error) {
		opts := queryOptions{
			Query: query,
			Limit: normalizedLimit(*limit, false),
		}
		req := serviceRequestFromOptions(opts, false)
		resp, err := callService(*pipeName, req)
		if err != nil {
			return nil, err
		}
		if !resp.OK {
			return nil, errors.New(resp.Message)
		}
		return snapshotFromResponse(resp), nil
	}

	emit := func(event string, entries map[string]watchEntry, paths []string) {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		for _, p := range paths {
			ev := watchEvent{Ts: now, Event: event, Path: p}
			if e, ok := entries[p]; ok {
				size := e.size
				ev.Size = &size
				if !e.mtime.IsZero() {
					ev.Mtime = e.mtime.Format(time.RFC3339)
				}
			}
			if err := enc.Encode(ev); err != nil {
				fmt.Fprintf(os.Stderr, "watch: write event: %v\n", err)
				return
			}
		}
	}

	tick := func() {
		next, err := poll()
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
			// Re-baseline silently: skip the tick so an outage never
			// surfaces as a deletion storm.
			snapshot = next
			baselined = true
			return
		}
		if !baselined {
			snapshot = next
			baselined = true
			fmt.Fprintf(os.Stderr, "watching %d files (poll %s) query=%q pipe=%s\n",
				len(snapshot), *interval, query, *pipeName)
			return
		}
		created, modified, deleted := diffWatchSnapshots(snapshot, next)
		snapshot = next
		emit("created", next, created)
		emit("modified", next, modified)
		emit("deleted", next, deleted)
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
