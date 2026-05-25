package main

import (
	"bytes"
	"testing"
	"time"
)

func TestCompactIndexV8RoundTripKeepsFRNMetadata(t *testing.T) {
	builtAt := time.Unix(0, 123456789)
	modified := time.Unix(0, 987654321)
	idx := &Index{
		Version:    indexVersion,
		Roots:      []string{`C:\`},
		BuiltAt:    builtAt,
		Source:     "usn",
		Volume:     "C:",
		JournalID:  42,
		Checkpoint: 99,
		Compact:    true,
		Records: []CompactRecord{
			{
				FRN:       10,
				ParentFRN: 10,
				Parent:    -1,
				Name:      ".",
				LowerName: ".",
				Mode:      uint32(1 << 31),
				Size:      0,
				ModUnix:   modified.UnixNano(),
			},
			{
				FRN:       11,
				ParentFRN: 10,
				Parent:    0,
				Name:      "main.go",
				LowerName: "main.go",
				Mode:      0,
				Size:      1234,
				ModUnix:   modified.Add(time.Second).UnixNano(),
				Deleted:   true,
			},
		},
	}

	var buf bytes.Buffer
	if err := writeIndex(&buf, idx); err != nil {
		t.Fatalf("writeIndex: %v", err)
	}
	got, err := readIndex(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("readIndex: %v", err)
	}

	if got.Version != indexVersion {
		t.Fatalf("Version = %d, want %d", got.Version, indexVersion)
	}
	if got.Source != "usn" || got.Volume != "C:" || got.JournalID != 42 || got.Checkpoint != 99 {
		t.Fatalf("index metadata was not preserved: %+v", got)
	}
	if !got.Compact || len(got.Records) != 2 {
		t.Fatalf("records = %d compact=%v, want 2 compact records", len(got.Records), got.Compact)
	}

	root := got.Records[0]
	if root.FRN != 10 || root.ParentFRN != 10 || root.Parent != -1 || root.Mode != uint32(1<<31) || root.ModUnix != modified.UnixNano() {
		t.Fatalf("root record metadata mismatch: %+v", root)
	}
	file := got.Records[1]
	if file.FRN != 11 || file.ParentFRN != 10 || file.Parent != 0 || file.Name != "main.go" || file.Size != 1234 || file.ModUnix != modified.Add(time.Second).UnixNano() || !file.Deleted {
		t.Fatalf("file record metadata mismatch: %+v", file)
	}
	if got.reconstructCompactPath(1) != `C:\.\main.go` {
		t.Fatalf("path = %q", got.reconstructCompactPath(1))
	}
}

func TestServiceVolumeIndexAppliesUSNMutations(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", LowerName: "."},
		},
	}
	vol := newServiceVolumeIndex(`F:\seekfs_f.gsi`, idx)

	vol.applyUSNChanges([]usnChange{{
		FRN:       101,
		ParentFRN: 100,
		USN:       10,
		Reason:    usnReasonFileCreate,
		Name:      "old.txt",
	}})
	if len(idx.Records) != 2 {
		t.Fatalf("records after create = %d, want 2", len(idx.Records))
	}
	if idx.Records[1].Name != "old.txt" || idx.Records[1].Parent != 0 || idx.Records[1].Deleted {
		t.Fatalf("created record mismatch: %+v", idx.Records[1])
	}

	vol.applyUSNChanges([]usnChange{{
		FRN:       101,
		ParentFRN: 100,
		USN:       11,
		Reason:    usnReasonRenameOld,
		Name:      "old.txt",
	}, {
		FRN:       101,
		ParentFRN: 100,
		USN:       12,
		Reason:    usnReasonRenameNew,
		Name:      "new.txt",
	}})
	if idx.Records[1].Name != "new.txt" || idx.Records[1].LowerName != "new.txt" || idx.Records[1].Deleted {
		t.Fatalf("renamed record mismatch: %+v", idx.Records[1])
	}

	vol.applyUSNChanges([]usnChange{{
		FRN:    101,
		USN:    13,
		Reason: usnReasonFileDelete,
	}})
	if !idx.Records[1].Deleted {
		t.Fatalf("deleted record was not tombstoned: %+v", idx.Records[1])
	}
	if vol.checkpoint != 13 || idx.Checkpoint != 13 {
		t.Fatalf("checkpoint = vol %d idx %d, want 13", vol.checkpoint, idx.Checkpoint)
	}
}

func TestValidateUSNCheckpoint(t *testing.T) {
	vol := &serviceVolumeIndex{journalID: 10, checkpoint: 50}
	journal := usnJournalDataV0{UsnJournalID: 10, FirstUsn: 20, LowestValidUsn: 30, NextUsn: 100}
	if err := validateUSNCheckpoint(vol, journal); err != nil {
		t.Fatalf("valid checkpoint rejected: %v", err)
	}

	vol.checkpoint = 29
	if err := validateUSNCheckpoint(vol, journal); err == nil {
		t.Fatal("checkpoint before lowest valid USN was accepted")
	}

	vol.checkpoint = 101
	if err := validateUSNCheckpoint(vol, journal); err == nil {
		t.Fatal("checkpoint after next USN was accepted")
	}

	vol.checkpoint = 50
	vol.journalID = 11
	if err := validateUSNCheckpoint(vol, journal); err == nil {
		t.Fatal("changed journal id was accepted")
	}
}

func TestSearchCompactSkipsDeletedRecords(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", LowerName: "."},
			{FRN: 101, ParentFRN: 100, Parent: 0, Name: "gone.txt", LowerName: "gone.txt", Deleted: true},
			{FRN: 102, ParentFRN: 100, Parent: 0, Name: "live.txt", LowerName: "live.txt"},
		},
	}
	buildOrders(idx)

	matches, err := searchCompact(idx, queryOptions{Query: "txt", MatchPath: false, Limit: 10}, false)
	if err != nil {
		t.Fatalf("searchCompact: %v", err)
	}
	if len(matches) != 1 || matches[0].Name != "live.txt" {
		t.Fatalf("matches = %+v, want only live.txt", matches)
	}
}

func TestServiceVolumeIndexBuildsFRNMap(t *testing.T) {
	idx := &Index{
		Source:     "usn",
		Volume:     "F:",
		JournalID:  7,
		Checkpoint: 12,
		Compact:    true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Name: ".", Parent: -1},
			{FRN: 101, ParentFRN: 100, Name: "child.txt", Parent: 0},
		},
	}

	vol := newServiceVolumeIndex(`F:\seekfs_f.gsi`, idx)
	if vol.dbPath != `F:\seekfs_f.gsi` || vol.volume != "F:" || vol.journalID != 7 || vol.checkpoint != 12 || vol.state != "ready" {
		t.Fatalf("volume metadata mismatch: %+v", vol)
	}
	if len(vol.frnToID) != 2 {
		t.Fatalf("frn map size = %d, want 2", len(vol.frnToID))
	}
	if got := vol.frnToID[101]; got != 1 {
		t.Fatalf("frnToID[101] = %d, want 1", got)
	}
}
