package main

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestValidateUSNCheckpointTable(t *testing.T) {
	base := usnJournalDataV0{UsnJournalID: 42, FirstUsn: 1000, LowestValidUsn: 1200, NextUsn: 9000}
	cases := []struct {
		name      string
		vol       *serviceVolumeIndex
		journal   usnJournalDataV0
		wantStale bool
		wantMsg   string
	}{
		{
			name:    "healthy checkpoint in range",
			vol:     &serviceVolumeIndex{journalID: 42, checkpoint: 5000},
			journal: base,
		},
		{
			name:    "zero journal id skips id comparison",
			vol:     &serviceVolumeIndex{journalID: 0, checkpoint: 5000},
			journal: base,
		},
		{
			name:      "journal id changed means recreated journal",
			vol:       &serviceVolumeIndex{journalID: 41, checkpoint: 5000},
			journal:   base,
			wantStale: true,
			wantMsg:   "journal id changed",
		},
		{
			name:      "checkpoint before first usn means wrapped journal",
			vol:       &serviceVolumeIndex{journalID: 42, checkpoint: 999},
			journal:   base,
			wantStale: true,
			wantMsg:   "before first valid USN",
		},
		{
			name:    "checkpoint at lowest valid usn is acceptable",
			vol:     &serviceVolumeIndex{journalID: 42, checkpoint: 1200},
			journal: base,
		},
		{
			name:      "checkpoint after next usn means foreign journal state",
			vol:       &serviceVolumeIndex{journalID: 42, checkpoint: 9001},
			journal:   base,
			wantStale: true,
			wantMsg:   "after journal next USN",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateUSNCheckpoint(tc.vol, tc.journal)
			if !tc.wantStale {
				if err != nil {
					t.Fatalf("validateUSNCheckpoint() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("validateUSNCheckpoint() = nil, want stale error")
			}
			if !shouldRebuildStaleIndex(err) {
				t.Fatalf("error %q should trigger rebuild", err)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error %q should contain %q", err, tc.wantMsg)
			}
		})
	}
}

func TestClassifyServiceHealth(t *testing.T) {
	cases := []struct {
		name        string
		loading     bool
		loadErr     string
		infos       []dbInfo
		wantHealth  string
		wantMessage string
	}{
		{
			name:       "all ready volumes are ok",
			infos:      []dbInfo{{Volume: "C:", State: "ready"}},
			wantHealth: serviceHealthOK,
		},
		{
			name:        "stale volume is an error",
			infos:       []dbInfo{{Volume: "C:", State: "ready"}, {Volume: "F:", State: "stale", StaleReason: "journal wrapped"}},
			wantHealth:  serviceHealthError,
			wantMessage: "journal wrapped",
		},
		{
			name:        "load error is surfaced as error",
			loadErr:     "db unreadable",
			infos:       []dbInfo{{Volume: "C:", State: "ready"}},
			wantHealth:  serviceHealthError,
			wantMessage: "db unreadable",
		},
		{
			name:        "persist failures degrade health",
			infos:       []dbInfo{{Volume: "C:", State: "ready", PersistFailures: 2}},
			wantHealth:  serviceHealthDegraded,
			wantMessage: "persist failing on volume C:",
		},
		{
			name:        "replaying volume degrades health",
			infos:       []dbInfo{{Volume: "F:", State: "replaying"}},
			wantHealth:  serviceHealthDegraded,
			wantMessage: "catching up volume F:",
		},
		{
			name:        "startup loading degrades health",
			loading:     true,
			infos:       []dbInfo{{Volume: "C:", State: "ready"}},
			wantHealth:  serviceHealthDegraded,
			wantMessage: "loading indexes",
		},
		{
			name:        "no indexes loaded degrades health",
			wantHealth:  serviceHealthDegraded,
			wantMessage: "no indexes loaded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			health, message := classifyServiceHealth(tc.loading, tc.loadErr, tc.infos)
			if health != tc.wantHealth {
				t.Fatalf("health = %q, want %q (message %q)", health, tc.wantHealth, message)
			}
			if tc.wantMessage != "" && message != tc.wantMessage {
				t.Fatalf("message = %q, want %q", message, tc.wantMessage)
			}
		})
	}
}

func TestReplayLoopMarksUnreachableVolumeStaleAndStopsCleanly(t *testing.T) {
	origIdle, origErr := replayIdleDelay, replayErrorDelay
	replayIdleDelay, replayErrorDelay = time.Millisecond, time.Millisecond
	defer func() { replayIdleDelay, replayErrorDelay = origIdle, origErr }()

	svc := &goSearchService{stop: make(chan struct{})}
	idx := &Index{Volume: "\\\\.\\seekfs-test-nonexistent-volume", Source: "usn", Compact: true}
	vol := newServiceVolumeIndex("", idx)
	vol.state = "ready"

	before := runtime.NumGoroutine()
	done := make(chan struct{})
	go func() {
		svc.replayVolumeLoop(vol)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		svc.indexMu.RLock()
		state := vol.state
		svc.indexMu.RUnlock()
		if state == "stale" && vol.staleReason != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	svc.indexMu.RLock()
	state, reason := vol.state, vol.staleReason
	svc.indexMu.RUnlock()
	if state != "stale" || reason == "" {
		t.Fatalf("volume state=%q reason=%q; want stale with reason", state, reason)
	}

	close(svc.stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("replay loop did not exit after stop")
	}
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines before=%d after=%d; possible leak", before, after)
	}
}

func TestServiceInfoResponseCarriesHealth(t *testing.T) {
	resp := serviceInfoResponse(serviceResponse{OK: true, Entries: 10, Health: serviceHealthOK})
	if resp.Health != serviceHealthOK {
		t.Fatalf("health = %q after identity decoration; want preserved", resp.Health)
	}
}
