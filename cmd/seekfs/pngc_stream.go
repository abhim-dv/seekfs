package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

const (
	pngcStreamBufferBytes = 256 * 1024
	pngcStreamBlockIDs    = 1024
)

type pngcStreamGram struct {
	gram   uint32
	count  uint64
	offset int64
	cursor uint64
}

type pngcStreamPending struct {
	ids []uint32
}

type pngcStreamBuilder struct {
	file          *os.File
	path          string
	grams         []pngcStreamGram
	byGram        map[uint32]int
	scratch       int64
	bufferCap     int
	bufferUsed    int
	unionComplete bool
}

type pngcStreamEntry struct {
	key        uint32
	count      uint32
	firstBlock uint32
	blockCount uint32
}

type pngcStreamBlock struct {
	offset  uint64
	length  uint32
	count   uint32
	minID   uint32
	maxID   uint32
	minRank uint32
}

func newPNGCStreamBuilder(ctx context.Context, idx *Index, selective *compressedTrigramIndex, scratchDir string, maxScratch int64) (*pngcStreamBuilder, error) {
	if idx == nil || selective == nil {
		return nil, errors.New("PNGC stream source is incomplete")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	counts := make(map[uint32]uint64)
	if len(selective.omitted) > 0 {
		for gram := range selective.omitted {
			count := selective.countForGram(gram)
			if count > 0 {
				counts[gram] = uint64(count)
			}
		}
	} else {
		// Legacy v9 PNGR files may predate omission metadata.  Count only
		// grams absent from the mapped PNGR table; posting IDs are never kept
		// in this map.  The subsequent pass writes IDs directly to the spool.
		if err := countMissingNameGrams(ctx, idx, selective, counts); err != nil {
			return nil, err
		}
	}
	if len(counts) == 0 {
		return nil, errors.New("source has no missing common grams to augment")
	}
	grams := make([]uint32, 0, len(counts))
	var totalIDs uint64
	for gram, count := range counts {
		grams = append(grams, gram)
		if totalIDs > ^uint64(0)-count {
			return nil, errors.New("PNGC spool size overflows")
		}
		totalIDs += count
	}
	sort.Slice(grams, func(i, j int) bool { return grams[i] < grams[j] })
	spoolBytes := totalIDs * 4
	if spoolBytes > uint64(maxInt64Value()) || (maxScratch > 0 && spoolBytes > uint64(maxScratch)) {
		return nil, fmt.Errorf("PNGC scratch size %d exceeds limit %d", spoolBytes, maxScratch)
	}
	if err := os.MkdirAll(scratchDir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(scratchDir, "pngc-postings-*.spool.tmp")
	if err != nil {
		return nil, err
	}
	b := &pngcStreamBuilder{file: file, path: file.Name(), bufferCap: pngcStreamBufferBytes, byGram: make(map[uint32]int, len(grams)), unionComplete: len(selective.omitted) == 0}
	b.grams = make([]pngcStreamGram, len(grams))
	var offset int64
	for i, gram := range grams {
		count := counts[gram]
		if count > uint64(maxInt64Value()/4) || offset > maxInt64Value()-int64(count*4) {
			_ = file.Close()
			_ = os.Remove(b.path)
			return nil, errors.New("PNGC spool offset overflows")
		}
		b.grams[i] = pngcStreamGram{gram: gram, count: count, offset: offset}
		b.byGram[gram] = i
		offset += int64(count * 4)
	}
	b.scratch = offset
	if err := file.Truncate(offset); err != nil {
		_ = file.Close()
		_ = os.Remove(b.path)
		return nil, err
	}
	if err := b.scanNames(ctx, idx); err != nil {
		_ = b.cleanup()
		return nil, err
	}
	if err := b.file.Sync(); err != nil {
		_ = b.cleanup()
		return nil, err
	}
	for _, gram := range b.grams {
		if gram.cursor != gram.count {
			_ = b.cleanup()
			return nil, fmt.Errorf("PNGC gram %06x count mismatch: wrote %d want %d", gram.gram, gram.cursor, gram.count)
		}
	}
	return b, nil
}

func countMissingNameGrams(ctx context.Context, idx *Index, selective *compressedTrigramIndex, counts map[uint32]uint64) error {
	for id := 0; id < idx.compactRecordCount(); id++ {
		if id&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		name := idx.compactLowerNameAt(id)
		seen := make([]uint32, 0, len(name))
		for pos := 0; pos+3 <= len(name); pos++ {
			gram := fixedNameGram(name[pos:])
			if selective.hasStoredPosting(gram) || containsUint32(seen, gram) {
				continue
			}
			seen = append(seen, gram)
			counts[gram]++
		}
	}
	return nil
}

func fixedNameGram(name string) uint32 {
	return uint32(name[0])<<16 | uint32(name[1])<<8 | uint32(name[2])
}

func containsUint32(values []uint32, value uint32) bool {
	for _, current := range values {
		if current == value {
			return true
		}
	}
	return false
}

func (b *pngcStreamBuilder) scanNames(ctx context.Context, idx *Index) error {
	pending := make(map[int][]uint32)
	flush := func() error {
		buf := make([]byte, min(pngcStreamBufferBytes, max(4, b.bufferUsed)))
		for region, ids := range pending {
			gram := &b.grams[region]
			start := gram.cursor - uint64(len(ids))
			for pos := 0; pos < len(ids); {
				chunk := min(len(ids)-pos, len(buf)/4)
				for i := 0; i < chunk; i++ {
					binary.LittleEndian.PutUint32(buf[i*4:], ids[pos+i])
				}
				if _, err := b.file.WriteAt(buf[:chunk*4], gram.offset+int64(start+uint64(pos))*4); err != nil {
					return err
				}
				pos += chunk
			}
		}
		pending = make(map[int][]uint32)
		b.bufferUsed = 0
		return nil
	}
	for id := 0; id < idx.compactRecordCount(); id++ {
		if id&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		name := idx.compactLowerNameAt(id)
		seen := make([]uint32, 0, len(name))
		for pos := 0; pos+3 <= len(name); pos++ {
			gram := fixedNameGram(name[pos:])
			region, ok := b.byGram[gram]
			if !ok || containsUint32(seen, gram) {
				continue
			}
			seen = append(seen, gram)
			entry := &b.grams[region]
			if entry.cursor >= entry.count {
				return fmt.Errorf("PNGC gram %06x exceeded declared count", gram)
			}
			pending[region] = append(pending[region], uint32(id))
			entry.cursor++
			b.bufferUsed += 4
			if b.bufferUsed >= b.bufferCap {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
	return flush()
}

func (b *pngcStreamBuilder) measure(ranks []uint32) ([]pngcStreamEntry, []pngcStreamBlock, uint64, error) {
	entries := make([]pngcStreamEntry, 0, len(b.grams))
	blocks := make([]pngcStreamBlock, 0)
	var blobBytes uint64
	ids := make([]uint32, pngcStreamBlockIDs)
	for _, gram := range b.grams {
		entry := pngcStreamEntry{key: gram.gram, count: uint32(gram.count), firstBlock: uint32(len(blocks))}
		var read uint64
		for read < gram.count {
			chunk := min(int(gram.count-read), len(ids))
			if err := b.readIDs(gram, read, ids[:chunk]); err != nil {
				return nil, nil, 0, err
			}
			encoded := encodeDeltaUvarint32(ids[:chunk])
			minRank, err := streamBlockMinRank(ids[:chunk], ranks)
			if err != nil {
				return nil, nil, 0, err
			}
			if blobBytes > ^uint64(0)-uint64(len(encoded)) {
				return nil, nil, 0, errors.New("PNGC payload size overflows")
			}
			blocks = append(blocks, pngcStreamBlock{offset: blobBytes, length: uint32(len(encoded)), count: uint32(chunk), minID: ids[0], maxID: ids[chunk-1], minRank: minRank})
			blobBytes += uint64(len(encoded))
			read += uint64(chunk)
		}
		entry.blockCount = uint32(len(blocks)) - entry.firstBlock
		entries = append(entries, entry)
	}
	payloadBytes := uint64(16) + uint64(len(entries))*16 + uint64(len(blocks))*28 + blobBytes + 8
	return entries, blocks, payloadBytes, nil
}

func streamBlockMinRank(ids []uint32, ranks []uint32) (uint32, error) {
	if len(ids) == 0 {
		return 0, errors.New("empty PNGC block")
	}
	minRank := ^uint32(0)
	for _, id := range ids {
		rank := extRankOf(id, ranks)
		if rank < minRank {
			minRank = rank
		}
	}
	if minRank == ^uint32(0) {
		minRank = 0
	}
	return minRank, nil
}

func (b *pngcStreamBuilder) writePayload(ctx context.Context, dst io.Writer, ranks []uint32, entries []pngcStreamEntry, blocks []pngcStreamBlock) (int64, error) {
	write := func(value any) error { return binary.Write(dst, binary.LittleEndian, value) }
	if err := write(uint32(len(entries))); err != nil {
		return 0, err
	}
	if err := write(uint32(0)); err != nil {
		return 0, err
	}
	if err := write(uint32(len(blocks))); err != nil {
		return 0, err
	}
	var blobBytes uint64
	for _, block := range blocks {
		blobBytes += uint64(block.length)
	}
	if err := write(uint32(blobBytes)); err != nil {
		return 0, err
	}
	for _, entry := range entries {
		if err := write(entry.key); err != nil {
			return 0, err
		}
		if err := write(entry.count); err != nil {
			return 0, err
		}
		if err := write(entry.firstBlock); err != nil {
			return 0, err
		}
		if err := write(entry.blockCount); err != nil {
			return 0, err
		}
	}
	for _, block := range blocks {
		if err := write(block.offset); err != nil {
			return 0, err
		}
		if err := write(block.length); err != nil {
			return 0, err
		}
		if err := write(block.count); err != nil {
			return 0, err
		}
		if err := write(block.minID); err != nil {
			return 0, err
		}
		if err := write(block.maxID); err != nil {
			return 0, err
		}
		if err := write(block.minRank); err != nil {
			return 0, err
		}
	}
	ids := make([]uint32, pngcStreamBlockIDs)
	for _, gram := range b.grams {
		var read uint64
		for read < gram.count {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			chunk := min(int(gram.count-read), len(ids))
			if err := b.readIDs(gram, read, ids[:chunk]); err != nil {
				return 0, err
			}
			encoded := encodeDeltaUvarint32(ids[:chunk])
			if _, err := dst.Write(encoded); err != nil {
				return 0, err
			}
			read += uint64(chunk)
		}
	}
	metadataMagic := uint32(gramPostingMetadataMagic)
	if b.unionComplete {
		metadataMagic = gramPostingUnionMetadataMagic
	}
	if err := write(metadataMagic); err != nil {
		return 0, err
	}
	if err := write(uint32(0)); err != nil {
		return 0, err
	}
	return int64(uint64(16) + uint64(len(entries))*16 + uint64(len(blocks))*28 + blobBytes + 8), nil
}

func (b *pngcStreamBuilder) readIDs(gram pngcStreamGram, start uint64, dst []uint32) error {
	if start+uint64(len(dst)) > gram.count {
		return errors.New("PNGC spool read exceeds declared count")
	}
	buf := make([]byte, len(dst)*4)
	if _, err := b.file.ReadAt(buf, gram.offset+int64(start)*4); err != nil {
		return err
	}
	for i := range dst {
		dst[i] = binary.LittleEndian.Uint32(buf[i*4:])
		if i > 0 && dst[i] <= dst[i-1] {
			return errors.New("PNGC spool IDs are not strictly increasing")
		}
	}
	return nil
}

func (b *pngcStreamBuilder) cleanup() error {
	if b == nil {
		return nil
	}
	var first error
	if b.file != nil {
		if err := b.file.Close(); err != nil {
			first = err
		}
		b.file = nil
	}
	if b.path != "" {
		if err := os.Remove(b.path); err != nil && !errors.Is(err, os.ErrNotExist) && first == nil {
			first = err
		}
	}
	return first
}

func maxInt64Value() int64 { return int64(^uint64(0) >> 1) }

func pngcScratchPath(targetPath string) string {
	return filepath.Dir(targetPath)
}
