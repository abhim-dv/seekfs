package main

import (
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

func TestParseUSNChangeBuffer(t *testing.T) {
	record := makeUSNRecordV2(44, 33, 1200, 0x00000100, fileAttributeDir, "created-dir")
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

func TestParseUSNRecordsRejectsInvalidLength(t *testing.T) {
	record := make([]byte, 60)
	binary.LittleEndian.PutUint32(record[0:4], 59)
	if _, err := parseUSNRecords(record); err == nil {
		t.Fatal("parseUSNRecords succeeded with invalid length")
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
