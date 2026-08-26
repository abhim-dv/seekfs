package main

import (
	"reflect"
	"testing"
	"time"
)

func watchTestEntry(path string, size int64, mtime time.Time) watchEntry {
	return watchEntry{path: path, size: size, mtime: mtime}
}

func TestDiffWatchSnapshots(t *testing.T) {
	t0 := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(2 * time.Second)

	tests := []struct {
		name            string
		prev, next      map[string]watchEntry
		created         []string
		modified        []string
		deleted         []string
		wantNochangeNil bool
	}{
		{
			name: "created only",
			prev: map[string]watchEntry{},
			next: map[string]watchEntry{
				`F:\a.txt`: watchTestEntry(`F:\a.txt`, 10, t0),
				`F:\b.txt`: watchTestEntry(`F:\b.txt`, 20, t0),
			},
			created: []string{`F:\a.txt`, `F:\b.txt`},
		},
		{
			name: "modified size change",
			prev: map[string]watchEntry{
				`F:\a.txt`: watchTestEntry(`F:\a.txt`, 10, t0),
			},
			next: map[string]watchEntry{
				`F:\a.txt`: watchTestEntry(`F:\a.txt`, 99, t0),
			},
			modified: []string{`F:\a.txt`},
		},
		{
			name: "modified mtime-only change",
			prev: map[string]watchEntry{
				`F:\a.txt`: watchTestEntry(`F:\a.txt`, 10, t0),
			},
			next: map[string]watchEntry{
				`F:\a.txt`: watchTestEntry(`F:\a.txt`, 10, t1),
			},
			modified: []string{`F:\a.txt`},
		},
		{
			name: "unchanged is not modified",
			prev: map[string]watchEntry{
				`F:\a.txt`: watchTestEntry(`F:\a.txt`, 10, t0),
			},
			next: map[string]watchEntry{
				`F:\a.txt`: watchTestEntry(`F:\a.txt`, 10, t0),
			},
			created: nil, modified: nil, deleted: nil,
		},
		{
			name: "deleted only",
			prev: map[string]watchEntry{
				`F:\gone.txt`: watchTestEntry(`F:\gone.txt`, 5, t0),
			},
			next:    map[string]watchEntry{},
			deleted: []string{`F:\gone.txt`},
		},
		{
			name: "mixed",
			prev: map[string]watchEntry{
				`F:\kept.txt`:    watchTestEntry(`F:\kept.txt`, 1, t0),
				`F:\changed.txt`: watchTestEntry(`F:\changed.txt`, 2, t0),
				`F:\removed.txt`: watchTestEntry(`F:\removed.txt`, 3, t0),
			},
			next: map[string]watchEntry{
				`F:\kept.txt`:    watchTestEntry(`F:\kept.txt`, 1, t0),
				`F:\changed.txt`: watchTestEntry(`F:\changed.txt`, 4, t1),
				`F:\added.txt`:   watchTestEntry(`F:\added.txt`, 5, t1),
			},
			created:  []string{`F:\added.txt`},
			modified: []string{`F:\changed.txt`},
			deleted:  []string{`F:\removed.txt`},
		},
		{
			name: "empty prev means all created",
			prev: nil,
			next: map[string]watchEntry{
				`F:\z.txt`: watchTestEntry(`F:\z.txt`, 1, t0),
				`F:\a.txt`: watchTestEntry(`F:\a.txt`, 2, t0),
			},
			created: []string{`F:\a.txt`, `F:\z.txt`},
		},
		{
			name: "empty next means all deleted",
			prev: map[string]watchEntry{
				`F:\z.txt`: watchTestEntry(`F:\z.txt`, 1, t0),
				`F:\a.txt`: watchTestEntry(`F:\a.txt`, 2, t0),
			},
			next:    nil,
			deleted: []string{`F:\a.txt`, `F:\z.txt`},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			gotCreated, gotModified, gotDeleted := diffWatchSnapshots(tc.prev, tc.next)
			if !reflect.DeepEqual(gotCreated, tc.created) {
				t.Errorf("created = %v, want %v", gotCreated, tc.created)
			}
			if !reflect.DeepEqual(gotModified, tc.modified) {
				t.Errorf("modified = %v, want %v", gotModified, tc.modified)
			}
			if !reflect.DeepEqual(gotDeleted, tc.deleted) {
				t.Errorf("deleted = %v, want %v", gotDeleted, tc.deleted)
			}
		})
	}
}

func TestDiffWatchSnapshotsDeterminism(t *testing.T) {
	t0 := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	next := make(map[string]watchEntry)
	for _, p := range []string{`F:\c.txt`, `F:\a.txt`, `F:\b.txt`} {
		next[p] = watchTestEntry(p, 1, t0)
	}
	first, _, _ := diffWatchSnapshots(nil, next)
	for i := 0; i < 50; i++ {
		again, _, _ := diffWatchSnapshots(nil, next)
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("diff not deterministic: %v vs %v", first, again)
		}
	}
	for i := 1; i < len(first); i++ {
		if first[i-1] >= first[i] {
			t.Fatalf("created not sorted: %v", first)
		}
	}
}
