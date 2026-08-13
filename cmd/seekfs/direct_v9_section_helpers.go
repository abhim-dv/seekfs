package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// directV9WriteLOWRSectionFromSpool writes the existing LOWR wire format
// directly from the canonical record spool.  It reads the spool once, writing
// the folded-name blob to a bounded temp file and the per-record offset/length
// table to a second temp file; the section header is emitted from the final
// blob size and both temps are streamed to the output.
//
// The caller owns alignment and section-table insertion. The returned entry
// starts at the writer's current offset.
func directV9WriteLOWRSectionFromSpool(ctx context.Context, cw *countingWriter, finalPath string, recordCount int) (indexSectionTableEntry, error) {
	if cw == nil {
		return indexSectionTableEntry{}, errors.New("direct v9 LOWR writer is nil")
	}
	if recordCount < 0 || uint64(recordCount) > uint64(^uint32(0)) {
		return indexSectionTableEntry{}, errors.New("direct v9 LOWR record count out of range")
	}
	if recordCount == 0 {
		return indexSectionTableEntry{}, nil
	}

	blobPath := filepath.Join(filepath.Dir(finalPath), "direct-v9-lowr-blob.tmp")
	tablePath := filepath.Join(filepath.Dir(finalPath), "direct-v9-lowr-table.tmp")
	defer os.Remove(blobPath)
	defer os.Remove(tablePath)
	blob, err := os.Create(blobPath)
	if err != nil {
		return indexSectionTableEntry{}, err
	}
	blobWriter := bufio.NewWriterSize(blob, 256*1024)
	table, err := os.Create(tablePath)
	if err != nil {
		_ = blob.Close()
		return indexSectionTableEntry{}, err
	}
	tableWriter := bufio.NewWriterSize(table, 256*1024)

	var blobBytes uint64
	count, err := directV9ForEachSpoolName(ctx, finalPath, recordCount, func(_ int, name string) error {
		lower := directV9LOWRName(name)
		var off uint32 = packedLowerSameAsName
		if lower != name {
			if blobBytes > uint64(^uint32(0)) {
				return errors.New("direct v9 LOWR offset exceeds on-disk limits")
			}
			off = uint32(blobBytes)
			blobBytes += uint64(len(lower))
			if blobBytes > uint64(^uint32(0)) {
				return errors.New("direct v9 LOWR blob exceeds on-disk limits")
			}
			if _, err := blobWriter.WriteString(lower); err != nil {
				return err
			}
		}
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], off)
		if _, err := tableWriter.Write(b[:]); err != nil {
			return err
		}
		var lens [2]byte
		binary.LittleEndian.PutUint16(lens[:], uint16(len(lower)))
		if _, err := tableWriter.Write(lens[:]); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		_ = blob.Close()
		_ = table.Close()
		return indexSectionTableEntry{}, err
	}
	if count != recordCount {
		_ = blob.Close()
		_ = table.Close()
		return indexSectionTableEntry{}, errors.New("direct v9 LOWR record count mismatch")
	}
	if err := blobWriter.Flush(); err != nil {
		_ = blob.Close()
		_ = table.Close()
		return indexSectionTableEntry{}, err
	}
	if err := tableWriter.Flush(); err != nil {
		_ = blob.Close()
		_ = table.Close()
		return indexSectionTableEntry{}, err
	}
	if err := blob.Close(); err != nil {
		_ = table.Close()
		return indexSectionTableEntry{}, err
	}
	if err := table.Close(); err != nil {
		return indexSectionTableEntry{}, err
	}
	blobInfo, err := os.Stat(blobPath)
	if err != nil {
		return indexSectionTableEntry{}, err
	}
	if uint64(blobInfo.Size()) != blobBytes {
		return indexSectionTableEntry{}, errors.New("direct v9 LOWR blob length mismatch")
	}

	offset := uint64(cw.n)
	var header [8]byte
	binary.LittleEndian.PutUint32(header[0:4], uint32(recordCount))
	binary.LittleEndian.PutUint32(header[4:8], uint32(blobBytes))
	if err := directV9WriteBytes(cw, header[:]); err != nil {
		return indexSectionTableEntry{}, err
	}
	if err := copyFileToWriter(cw, tablePath); err != nil {
		return indexSectionTableEntry{}, err
	}
	if err := copyFileToWriter(cw, blobPath); err != nil {
		return indexSectionTableEntry{}, err
	}
	return indexSectionTableEntry{tag: indexSectionLOWR, offset: offset, length: uint64(cw.n) - offset}, nil
}

func copyFileToWriter(cw *countingWriter, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(cw, f)
	return err
}

func directV9LOWRName(name string) string {
	lower := strings.ToLower(name)
	if len(lower) > int(^uint16(0)) {
		lower = lower[:int(^uint16(0))]
	}
	return lower
}

func directV9ForEachSpoolName(ctx context.Context, finalPath string, expected int, fn func(int, string) error) (int, error) {
	f, err := os.Open(finalPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 256*1024)
	count := 0
	for {
		select {
		case <-ctx.Done():
			return count, ctx.Err()
		default:
		}
		rec, readErr := readDirectV9SpoolRecord(r)
		if errors.Is(readErr, io.EOF) {
			if count != expected {
				return count, errors.New("direct v9 spool record count mismatch")
			}
			return count, nil
		}
		if readErr != nil {
			return count, readErr
		}
		if count >= expected {
			return count, errors.New("direct v9 spool contains extra records")
		}
		if err := fn(count, rec.Name); err != nil {
			return count, err
		}
		count++
	}
}

func directV9WriteBytes(cw *countingWriter, p []byte) error {
	n, err := cw.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

// These bounded-fixture encoders delegate to the canonical v9 encoders. They
// deliberately keep the existing sorting, deduplication, block metadata, and
// decode contracts in one place until the direct builder has its streaming
// posting pipeline.
func directV9EncodePATRSection(attrBits map[uint32][]uint32) []byte {
	return encodeAttrPostingSection(attrBits)
}

func directV9EncodePEXTSection(postings map[string][]uint32, ranks []uint32) []byte {
	return encodeStringPostingSection(postings, ranks)
}

func directV9EncodePCMPSection(postings map[string][]uint32, ranks []uint32) []byte {
	return encodeStringPostingSection(postings, ranks)
}
