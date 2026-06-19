package main

import (
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

func TestParseUSNChangeBuffer(t *testing.T) {
	record := makeUSNRecordV2(0x000700000000002c, 0x0003000000000021, 1200, 0x00000100, fileAttributeDir, "created-dir")
	buffer := make([]byte, 8, 8+len(record))
	binary.LittleEndian.PutUint64(buffer[:8], 1300)
	buffer = append(buffer, record...)

	next, changes, err := parseUSNChangeBuffer(buffer)
	if err != nil {
		t.Fatalf("parseUSNChangeBuffer: %v", err)
	}
	if next != 1300 {
		t.Fatalf("next = %d, want 1300", next)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(changes))
	}
	got := changes[0]
	if got.FRN != 44 || got.ParentFRN != 33 || got.USN != 1200 || got.Reason != 0x00000100 || got.Attr != fileAttributeDir || got.Name != "created-dir" {
		t.Fatalf("change mismatch: %+v", got)
	}
}

func TestParseUSNChangeBufferV3(t *testing.T) {
	record := makeUSNRecordV3(0x000d0000008b7674, 0x00010000008b766c, 0x382a600ac0, usnReasonFileCreate, 0x20, "specific_fixture_tool.py")
	buffer := make([]byte, 8, 8+len(record))
	binary.LittleEndian.PutUint64(buffer[:8], 0x382a600ad0)
	buffer = append(buffer, record...)

	next, changes, err := parseUSNChangeBuffer(buffer)
	if err != nil {
		t.Fatalf("parseUSNChangeBuffer: %v", err)
	}
	if next != 0x382a600ad0 {
		t.Fatalf("next = %d, want %d", next, uint64(0x382a600ad0))
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(changes))
	}
	got := changes[0]
	if got.FRN != 0x8b7674 || got.ParentFRN != 0x8b766c || got.USN != 0x382a600ac0 ||
		got.Reason != usnReasonFileCreate || got.Attr != 0x20 || got.Name != "specific_fixture_tool.py" {
		t.Fatalf("change mismatch: %+v", got)
	}
}

func TestParseUSNRecordsRejectsInvalidLength(t *testing.T) {
	record := make([]byte, 60)
	binary.LittleEndian.PutUint32(record[0:4], 59)
	if _, err := parseUSNRecords(record); err == nil {
		t.Fatal("parseUSNRecords succeeded with invalid length")
	}
}

func TestMergeUSNNodesIntoMFTAddsMissingRecords(t *testing.T) {
	entries := map[uint64]mftEntry{
		10: {frn: 10, parentFRN: 5, name: "existing.txt", attr: 0x20, size: 99},
	}
	nodes := map[uint64]usnNode{
		10: {frn: 10, parentFRN: 5, name: "existing-renamed.txt", attr: 0x20},
		11: {frn: 11, parentFRN: 5, name: "missing-dir", attr: fileAttributeDir},
	}
	if got := mergeUSNNodesIntoMFT(entries, nodes); got != 1 {
		t.Fatalf("added = %d, want 1", got)
	}
	if entries[10].name != "existing.txt" || entries[10].size != 99 {
		t.Fatalf("existing MFT entry was overwritten: %+v", entries[10])
	}
	if e := entries[11]; e.name != "missing-dir" || e.parentFRN != 5 || !e.isDir || !e.inUse {
		t.Fatalf("merged entry = %+v, want USN-backed dir", e)
	}
}

func makeUSNRecordV2(frn, parent uint64, usn int64, reason, attr uint32, name string) []byte {
	nameUTF16 := utf16.Encode([]rune(name))
	nameBytes := make([]byte, len(nameUTF16)*2)
	for i, v := range nameUTF16 {
		binary.LittleEndian.PutUint16(nameBytes[i*2:], v)
	}
	recordLen := uint32(60 + len(nameBytes))
	record := make([]byte, recordLen)
	binary.LittleEndian.PutUint32(record[0:4], recordLen)
	binary.LittleEndian.PutUint16(record[4:6], 2)
	binary.LittleEndian.PutUint16(record[6:8], 0)
	binary.LittleEndian.PutUint64(record[8:16], frn)
	binary.LittleEndian.PutUint64(record[16:24], parent)
	binary.LittleEndian.PutUint64(record[24:32], uint64(usn))
	binary.LittleEndian.PutUint32(record[40:44], reason)
	binary.LittleEndian.PutUint32(record[52:56], attr)
	binary.LittleEndian.PutUint16(record[56:58], uint16(len(nameBytes)))
	binary.LittleEndian.PutUint16(record[58:60], 60)
	copy(record[60:], nameBytes)
	return record
}

func makeUSNRecordV3(frn, parent uint64, usn int64, reason, attr uint32, name string) []byte {
	nameUTF16 := utf16.Encode([]rune(name))
	nameBytes := make([]byte, len(nameUTF16)*2)
	for i, v := range nameUTF16 {
		binary.LittleEndian.PutUint16(nameBytes[i*2:], v)
	}
	recordLen := uint32(76 + len(nameBytes))
	record := make([]byte, recordLen)
	binary.LittleEndian.PutUint32(record[0:4], recordLen)
	binary.LittleEndian.PutUint16(record[4:6], 3)
	binary.LittleEndian.PutUint16(record[6:8], 0)
	binary.LittleEndian.PutUint64(record[8:16], frn)
	binary.LittleEndian.PutUint64(record[24:32], parent)
	binary.LittleEndian.PutUint64(record[40:48], uint64(usn))
	binary.LittleEndian.PutUint32(record[56:60], reason)
	binary.LittleEndian.PutUint32(record[68:72], attr)
	binary.LittleEndian.PutUint16(record[72:74], uint16(len(nameBytes)))
	binary.LittleEndian.PutUint16(record[74:76], 76)
	copy(record[76:], nameBytes)
	return record
}
