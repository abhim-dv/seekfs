package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Bounded external-merge construction of the filename gram index.
//
// The in-memory builder (buildSelectiveCompactNameGramIndex and its PNGC
// companion) holds every per-record posting map for the whole volume at once.
// This builder streams (gram, id) pairs into bounded per-worker buffers, spills
// delta-varint compressed sorted runs, and merges them with a bounded fan-in.
//
// Two properties matter at scale:
//   - Runs are delta-varint compressed (gram delta, count, id deltas), so the
//     spill footprint and the bytes read back are roughly half the fixed-width
//     encoding. This is the tgrep spill design.
//   - The merge is bounded fan-in: when the run count exceeds the fan-in, runs
//     are merged in parallel groups into intermediate runs until at most
//     fanIn remain, then a final k-way merge builds the postings. Reader memory
//     is therefore shards x fanIn x buffer, independent of volume size, instead
//     of one reader per run.
//
// The output is byte-for-byte equivalent to the in-memory path: postings are
// sorted-deduplicated by id and the section encoder re-blocks them identically.

// gramExternalSpillBytes bounds total spill buffering across all spill workers.
var gramExternalSpillBytes = 64 << 20

// Spill/merge/fan-in knobs, all env-overridable for tuning.
func gramSpillWorkers() int {
	return envWorkers("SEEKFS_GRAM_SPILL_WORKERS", defaultGramWorkers())
}

func gramMergeWorkers() int {
	return envWorkers("SEEKFS_GRAM_MERGE_WORKERS", defaultGramWorkers())
}

func gramMergeFanIn() int {
	n := envWorkers("SEEKFS_GRAM_MERGE_FANIN", 32)
	if n < 2 {
		n = 2
	}
	return n
}

func envWorkers(name string, def int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func gramSpillBytes() int {
	if v := strings.TrimSpace(os.Getenv("SEEKFS_GRAM_SPILL_BYTES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return gramExternalSpillBytes
}

func defaultGramWorkers() int {
	n := runtime.GOMAXPROCS(0)
	if n < 1 {
		n = 1
	}
	if n > 8 {
		n = 8
	}
	return n
}

// shouldUseExternalNameGram chooses the builder for a volume.  External is the
// default for any real volume: its peak is bounded and flat where the one-pass
// in-memory builder's grows into multiple GiB, at comparable build time.  The
// floor below nameGramExternalMinRecords keeps tiny indexes (and the test
// suite) on the in-memory builder, which avoids the spill temp-dir and file I/O
// overhead that only pays off at scale.  SEEKFS_NAME_GRAM_EXTERNAL=0 forces the
// in-memory builder; SEEKFS_NAME_GRAM_EXTERNAL_MIN_RECORDS moves the floor.
func shouldUseExternalNameGram(recordCount int) bool {
	if raw := strings.TrimSpace(os.Getenv("SEEKFS_NAME_GRAM_EXTERNAL")); raw != "" {
		return envBool("SEEKFS_NAME_GRAM_EXTERNAL")
	}
	if serviceLowMemoryMode() {
		return true
	}
	return recordCount >= nameGramExternalMinRecords()
}

func nameGramExternalMinRecords() int {
	if v := strings.TrimSpace(os.Getenv("SEEKFS_NAME_GRAM_EXTERNAL_MIN_RECORDS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 250_000
}

func nameGramSpoolDir() string {
	if dir := strings.TrimSpace(os.Getenv("SEEKFS_NAME_GRAM_SPOOL_DIR")); dir != "" {
		return dir
	}
	return os.TempDir()
}

// buildNameGramIndexExternal returns the selective PNGR index and the optional
// PNGC companion for idx.  PNGC is nil when no gram exceeds maxPostingCount.
func buildNameGramIndexExternal(ctx context.Context, idx *Index, gramSize, maxPostingCount int, scratchDir string) (pngr, pngc *compressedTrigramIndex, err error) {
	// gramSpillTable stores gram+1 as its empty-slot sentinel, which is only
	// collision-free while a gram fits in 24 bits, and fixedGramKeyFoldASCII
	// always reads three bytes.  Only trigrams are supported.
	if gramSize != 3 {
		return nil, nil, fmt.Errorf("external name gram builder supports gramSize=3, got %d", gramSize)
	}
	recordCount := 0
	if idx != nil {
		recordCount = idx.compactRecordCount()
	}
	newIndex := func() *compressedTrigramIndex {
		return &compressedTrigramIndex{
			counts:             make(map[uint32]int),
			gramCountsComplete: true,
			gramSize:           gramSize,
			recordCount:        recordCount,
		}
	}
	pngr = newIndex()
	if idx == nil || recordCount == 0 || maxPostingCount <= 0 {
		return pngr, nil, nil
	}

	owned, err := os.MkdirTemp(scratchDir, "seekfs-name-gram-")
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = os.RemoveAll(owned) }()

	runs, err := buildNameGramExternalRuns(ctx, idx, gramSize, owned)
	if err != nil {
		return nil, nil, err
	}
	// Release spill buffers before the merge allocates posting maps.
	runtime.GC()

	stored := make(map[uint32]compressedPosting)
	omitted := make(map[uint32]compressedPosting)
	storedBytes, omittedBytes, err := mergeGramRunsBounded(ctx, runs, maxPostingCount, pngr.counts, stored, omitted, owned)
	if err != nil {
		return nil, nil, err
	}
	pngr.segments = []trigramSegment{{start: 0, end: recordCount, postings: stored}}
	pngr.postingBytes = storedBytes
	pngr.markOmittedPostings(maxPostingCount)

	if len(omitted) > 0 {
		pngc = newIndex()
		pngc.counts = make(map[uint32]int, len(omitted))
		for gram, posting := range omitted {
			pngc.counts[gram] = posting.count
		}
		pngc.segments = []trigramSegment{{start: 0, end: recordCount, postings: omitted}}
		pngc.postingBytes = omittedBytes
	}
	return pngr, pngc, nil
}

// gramRunFile is one sorted spill segment on scratch.
type gramRunFile struct {
	path  string
	count int
}

// buildNameGramExternalRuns extracts (gram, id) keys over disjoint record ranges
// in parallel, each worker spilling compressed sorted runs when its buffer fills.
func buildNameGramExternalRuns(ctx context.Context, idx *Index, gramSize int, scratchDir string) ([]gramRunFile, error) {
	recordCount := idx.compactRecordCount()
	if recordCount == 0 {
		return nil, nil
	}
	workers := gramSpillWorkers()
	if workers > recordCount {
		workers = recordCount
	}
	if workers < 1 {
		workers = 1
	}
	chunk := (recordCount + workers - 1) / workers
	perWorker := make([][]gramRunFile, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		start := w * chunk
		end := min(recordCount, start+chunk)
		if start >= end {
			continue
		}
		wg.Add(1)
		go func(w, start, end int) {
			defer wg.Done()
			perWorker[w], errs[w] = spillGramRange(ctx, idx, gramSize, scratchDir, w, start, end)
		}(w, start, end)
	}
	wg.Wait()
	var runs []gramRunFile
	for w := range perWorker {
		if errs[w] != nil {
			return nil, errs[w]
		}
		runs = append(runs, perWorker[w]...)
	}
	return runs, nil
}

func spillGramRange(ctx context.Context, idx *Index, gramSize int, scratchDir string, worker, start, end int) ([]gramRunFile, error) {
	budget := (gramSpillBytes() / (max(1, gramSpillWorkers()) * 8))
	if budget < 1024 {
		budget = 1024
	}
	table := newGramSpillTable(budget / 8)
	grams := make([]uint32, 0, 1024)
	pairs := 0
	var runs []gramRunFile
	seq := 0
	flush := func() error {
		if pairs == 0 {
			return nil
		}
		grams = table.sortedGrams(grams)
		path := filepath.Join(scratchDir, fmt.Sprintf("spill-w%d-%06d.tmp", worker, seq))
		seq++
		if err := writeGramRunGrouped(path, grams, table); err != nil {
			return err
		}
		runs = append(runs, gramRunFile{path: path, count: pairs})
		table.reset()
		pairs = 0
		return nil
	}
	for id := start; id < end; id++ {
		if id&0xffff == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		// Group by gram in a custom open-addressed multimap instead of a Go map:
		// records are visited in id order, so each gram's ids append already
		// sorted, and only the distinct grams need a sort.  The custom table
		// avoids a Go map's bucket indirection, keep-alive hazards, and the
		// read-modify-write needed to append through a map value.
		name := rec.Name
		if len(name) >= gramSize {
			id32 := uint32(id)
			for i := 0; i+gramSize <= len(name); i++ {
				gram := fixedGramKeyFoldASCII(name, i, gramSize)
				table.add(gram, id32)
				pairs++
			}
		}
		if pairs >= budget {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return runs, nil
}

// gramSpillTable is an open-addressed multimap from gram to a growing id list.
// keys stores gram+1 so 0 marks an empty slot; trigram keys are <= 0xFFFFFF, so
// the +1 sentinel can never collide with a real key.  Ids within a slot append
// in increasing record order (the spill visits records in id order).
type gramSpillTable struct {
	mask  uint32
	keys  []uint32
	vals  [][]uint32
	count int
}

func newGramSpillTable(hint int) *gramSpillTable {
	size := 1024
	for size < hint {
		size <<= 1
	}
	return &gramSpillTable{
		mask: uint32(size - 1),
		keys: make([]uint32, size),
		vals: make([][]uint32, size),
	}
}

func (t *gramSpillTable) add(gram, id uint32) {
	h := (gram * 2654435761) & t.mask
	for {
		k := t.keys[h]
		if k == 0 {
			t.keys[h] = gram + 1
			t.vals[h] = append(t.vals[h][:0], id)
			t.count++
			if t.count*2 >= len(t.keys) {
				t.grow()
			}
			return
		}
		if k-1 == gram {
			// Records are visited in id order, so a gram that repeats within one
			// name is appended twice with the same id; skip the repeat so each
			// record contributes each gram once, matching the in-memory builder
			// (and the old key-level merge, which deduplicated adjacent keys).
			if n := len(t.vals[h]); n == 0 || t.vals[h][n-1] != id {
				t.vals[h] = append(t.vals[h], id)
			}
			return
		}
		h = (h + 1) & t.mask
	}
}

func (t *gramSpillTable) get(gram uint32) []uint32 {
	h := (gram * 2654435761) & t.mask
	for {
		k := t.keys[h]
		if k == 0 {
			return nil
		}
		if k-1 == gram {
			return t.vals[h]
		}
		h = (h + 1) & t.mask
	}
}

func (t *gramSpillTable) grow() {
	oldKeys, oldVals := t.keys, t.vals
	size := len(oldKeys) * 2
	t.keys = make([]uint32, size)
	t.vals = make([][]uint32, size)
	t.mask = uint32(size - 1)
	t.count = 0
	for i, k := range oldKeys {
		if k == 0 {
			continue
		}
		gram := k - 1
		h := (gram * 2654435761) & t.mask
		for t.keys[h] != 0 {
			h = (h + 1) & t.mask
		}
		t.keys[h] = k
		t.vals[h] = oldVals[i]
		t.count++
	}
}

// sortedGrams collects the grams that have ids in the current buffer and sorts
// them ascending.  Grams retained from earlier buffers have empty id lists and
// are skipped, so a flush never emits a zero-count group for a stale gram.
func (t *gramSpillTable) sortedGrams(dst []uint32) []uint32 {
	dst = dst[:0]
	for i, k := range t.keys {
		if k != 0 && len(t.vals[i]) > 0 {
			dst = append(dst, k-1)
		}
	}
	slices.Sort(dst)
	return dst
}

// reset empties each id list but keeps its slot and backing array (a gram seen
// again reuses the capacity) rather than freeing them, which would make every
// flush re-grow every slice and inflate the build's GC peak.
func (t *gramSpillTable) reset() {
	for i, k := range t.keys {
		if k != 0 {
			t.vals[i] = t.vals[i][:0]
		}
	}
}

// gramRunWriter buffers a run's varints in one reusable byte slice and writes
// them in large blocks, avoiding a bufio WriteByte call per encoded byte.
type gramRunWriter struct {
	f   *os.File
	buf []byte
}

func newGramRunWriter(path string) (*gramRunWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &gramRunWriter{f: f, buf: make([]byte, 0, 1<<20)}, nil
}

func (w *gramRunWriter) uvarint(v uint64) {
	for v >= 0x80 {
		w.buf = append(w.buf, byte(v)|0x80)
		v >>= 7
	}
	w.buf = append(w.buf, byte(v))
}

func (w *gramRunWriter) maybeFlush() error {
	if len(w.buf) < 1<<20 {
		return nil
	}
	return w.flush()
}

func (w *gramRunWriter) flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	if _, err := w.f.Write(w.buf); err != nil {
		return err
	}
	w.buf = w.buf[:0]
	return nil
}

func (w *gramRunWriter) close() error {
	if err := w.flush(); err != nil {
		_ = w.f.Close()
		return err
	}
	return w.f.Close()
}

// writeGramRunGrouped writes one spill run as delta-varint groups:
// varint(gram delta), varint(count), count x varint(id delta).  grams is sorted;
// raw[gram] holds that gram's ids in increasing order.  First gram and first id
// of each group are absolute.
func writeGramRunGrouped(path string, grams []uint32, table *gramSpillTable) error {
	w, err := newGramRunWriter(path)
	if err != nil {
		return err
	}
	var prevGram uint32
	for gi, gram := range grams {
		ids := table.get(gram)
		if gi == 0 {
			w.uvarint(uint64(gram))
		} else {
			w.uvarint(uint64(gram - prevGram))
		}
		prevGram = gram
		w.uvarint(uint64(len(ids)))
		var prevID uint32
		for k, id := range ids {
			if k == 0 {
				w.uvarint(uint64(id))
			} else {
				w.uvarint(uint64(id - prevID))
			}
			prevID = id
		}
		if err := w.maybeFlush(); err != nil {
			_ = w.close()
			return err
		}
	}
	return w.close()
}

// gramRunReader yields (gram, id) keys from a compressed run.  It decodes
// varints from a raw block buffer rather than bufio+binary.ReadUvarint, which
// saves a bounds-checked interface call per byte; a varint that straddles a
// block boundary is carried across the refill.
type gramRunReader struct {
	f         *os.File
	buf       []byte
	pos       int
	end       int
	eof       bool
	gram      uint32
	remaining uint32
	prevID    uint32
	haveGram  bool
	done      bool
}

const gramRunReadBytes = 64 * 1024

func newGramRunReader(path string) (*gramRunReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &gramRunReader{f: f, buf: make([]byte, gramRunReadBytes)}, nil
}

func (r *gramRunReader) readUvarint() (uint64, bool) {
	var x uint64
	var s uint
	for {
		if r.pos >= r.end {
			if !r.refill() {
				return 0, false
			}
		}
		b := r.buf[r.pos]
		r.pos++
		if b < 0x80 {
			if s > 63 {
				return 0, false
			}
			return x | uint64(b)<<s, true
		}
		x |= uint64(b&0x7f) << s
		s += 7
	}
}

func (r *gramRunReader) refill() bool {
	if r.eof {
		return false
	}
	n, err := r.f.Read(r.buf)
	if n <= 0 {
		r.eof = true
		if err != nil && !errors.Is(err, io.EOF) {
			r.done = true
		}
		return false
	}
	r.pos = 0
	r.end = n
	return true
}

// ensureGram loads the next gram header and leaves the reader positioned at the
// start of that gram's ids.  It returns false once the run is exhausted.  The
// merge peeks each run's gram before consuming ids so it can merge a whole gram
// at a time instead of pushing one (gram,id) key per posting through the tree.
func (r *gramRunReader) ensureGram() bool {
	if r.done {
		return false
	}
	for r.remaining == 0 {
		delta, ok := r.readUvarint()
		if !ok {
			r.done = true
			return false
		}
		count, ok := r.readUvarint()
		if !ok {
			r.done = true
			return false
		}
		if r.haveGram {
			r.gram += uint32(delta)
		} else {
			r.gram = uint32(delta)
			r.haveGram = true
		}
		r.remaining = uint32(count)
		r.prevID = 0
	}
	return true
}

// nextID consumes one id from the current gram.  ensureGram must have returned
// true and the caller must not read past r.remaining.
func (r *gramRunReader) nextID() (uint32, bool) {
	d, ok := r.readUvarint()
	if !ok {
		r.done = true
		return 0, false
	}
	r.prevID += uint32(d)
	r.remaining--
	return r.prevID, true
}

func (r *gramRunReader) close() {
	if r.f != nil {
		_ = r.f.Close()
	}
}

// mergeTree is a winner tree over runs keyed by each run's current gram.  The
// earlier design pushed one (gram,id) key per posting through the tree, so every
// id paid O(log k); keying on the gram and consuming a whole gram at a time pays
// O(log k) only once per (run,gram) pair.
type mergeTree struct {
	grams []uint32
	size  int
	tree  []int
}

func newMergeTree(readers []*gramRunReader) *mergeTree {
	k := len(readers)
	size := 1
	for size < k {
		size <<= 1
	}
	mt := &mergeTree{
		grams: make([]uint32, k),
		size:  size,
		tree:  make([]int, 2*size),
	}
	for i := range mt.tree {
		mt.tree[i] = -1
	}
	for i, r := range readers {
		if r.ensureGram() {
			mt.grams[i] = r.gram
			mt.tree[size+i] = i
		}
	}
	for i := size - 1; i >= 1; i-- {
		mt.tree[i] = mt.better(mt.tree[2*i], mt.tree[2*i+1])
	}
	return mt
}

func (mt *mergeTree) better(a, b int) int {
	if a < 0 {
		return b
	}
	if b < 0 {
		return a
	}
	if mt.grams[a] <= mt.grams[b] {
		return a
	}
	return b
}

// winner returns the smallest current gram and the run that owns it.
func (mt *mergeTree) winner() (uint32, bool) {
	run := mt.tree[1]
	if run < 0 {
		return 0, false
	}
	return mt.grams[run], true
}

// setGram records that run advanced to gram g and repairs the tree path.
func (mt *mergeTree) setGram(run int, g uint32) {
	mt.grams[run] = g
	mt.tree[mt.size+run] = run
	mt.repair(run)
}

// retire removes an exhausted run from the tree.
func (mt *mergeTree) retire(run int) {
	mt.tree[mt.size+run] = -1
	mt.repair(run)
}

func (mt *mergeTree) repair(run int) {
	for i := (mt.size + run) / 2; i >= 1; i /= 2 {
		mt.tree[i] = mt.better(mt.tree[2*i], mt.tree[2*i+1])
	}
}

// sortUniqueUint32 sorts ids ascending and removes duplicates.  It is only
// needed when a gram appears in more than one run; a single run already stores
// that gram's ids sorted and unique.
func sortUniqueUint32(ids []uint32) []uint32 {
	if len(ids) < 2 {
		return ids
	}
	slices.Sort(ids)
	out := ids[:1]
	for _, v := range ids[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

// appendDeltaUvarint32 appends the delta-uvarint encoding of ids to dst.
func appendDeltaUvarint32(dst []byte, ids []uint32) []byte {
	var prev uint32
	var buf [binary.MaxVarintLen32]byte
	for _, id := range ids {
		delta := id - prev
		n := binary.PutUvarint(buf[:], uint64(delta))
		dst = append(dst, buf[:n]...)
		prev = id
	}
	return dst
}

// mergeRunKeys k-way merges the given runs, calling emit once per gram with its
// sorted, deduplicated ids.  At most fanIn readers are open at once.
func mergeRunKeys(ctx context.Context, paths []string, emit func(gram uint32, ids []uint32) error) error {
	readers := make([]*gramRunReader, len(paths))
	for i, path := range paths {
		r, err := newGramRunReader(path)
		if err != nil {
			for _, rr := range readers {
				if rr != nil {
					rr.close()
				}
			}
			return err
		}
		readers[i] = r
	}
	defer func() {
		for _, r := range readers {
			if r != nil {
				r.close()
			}
		}
	}()
	mt := newMergeTree(readers)
	var ids []uint32
	for {
		gram, ok := mt.winner()
		if !ok {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		ids = ids[:0]
		sources := 0
		for i, r := range readers {
			if r.remaining == 0 || r.gram != gram {
				continue
			}
			sources++
			for r.remaining > 0 {
				id, ok := r.nextID()
				if !ok {
					break
				}
				ids = append(ids, id)
			}
			if r.ensureGram() {
				mt.setGram(i, r.gram)
			} else {
				mt.retire(i)
			}
		}
		if sources == 0 {
			continue
		}
		if sources > 1 {
			ids = sortUniqueUint32(ids)
		}
		if err := emit(gram, ids); err != nil {
			return err
		}
	}
	return nil
}

// mergeGramRunsBounded reduces the run count to at most the fan-in with parallel
// intermediate merges, then does a final merge into the posting maps.
func mergeGramRunsBounded(ctx context.Context, runs []gramRunFile, maxPostingCount int, counts map[uint32]int, stored, omitted map[uint32]compressedPosting, scratchDir string) (storedBytes, omittedBytes int, err error) {
	if len(runs) == 0 {
		return 0, 0, nil
	}
	fanIn := gramMergeFanIn()
	current := runs
	for level := 0; len(current) > fanIn; level++ {
		next, err := mergeRunLevel(ctx, current, fanIn, scratchDir, level)
		if err != nil {
			return 0, 0, err
		}
		for _, r := range current {
			_ = os.Remove(r.path)
		}
		current = next
	}
	paths := make([]string, len(current))
	for i, r := range current {
		paths[i] = r.path
	}
	err = mergeRunKeys(ctx, paths, func(gram uint32, ids []uint32) error {
		counts[gram] = len(ids)
		data := make([]byte, 0, len(ids)+len(ids)/2+8)
		data = appendDeltaUvarint32(data, ids)
		if len(ids) > maxPostingCount {
			omitted[gram] = compressedPosting{count: len(ids), data: data}
			omittedBytes += len(data)
			return nil
		}
		stored[gram] = compressedPosting{count: len(ids), data: data}
		storedBytes += len(data)
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return storedBytes, omittedBytes, nil
}

// mergeRunLevel batches runs into fanIn-sized groups and merges each group into
// one intermediate compressed run, in parallel.
func mergeRunLevel(ctx context.Context, runs []gramRunFile, fanIn int, scratchDir string, level int) ([]gramRunFile, error) {
	numBatches := (len(runs) + fanIn - 1) / fanIn
	results := make([][]gramRunFile, numBatches)
	errs := make([]error, numBatches)
	jobs := make(chan int)
	workers := min(gramMergeWorkers(), numBatches)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range jobs {
				start := b * fanIn
				end := min(len(runs), start+fanIn)
				path := filepath.Join(scratchDir, fmt.Sprintf("merge-L%d-%04d.tmp", level, b))
				results[b], errs[b] = mergeRunBatchToFile(ctx, runs[start:end], path)
			}
		}()
	}
	for b := 0; b < numBatches; b++ {
		jobs <- b
	}
	close(jobs)
	wg.Wait()
	var out []gramRunFile
	for b := range results {
		if errs[b] != nil {
			return nil, errs[b]
		}
		out = append(out, results[b]...)
	}
	return out, nil
}

func mergeRunBatchToFile(ctx context.Context, batch []gramRunFile, path string) ([]gramRunFile, error) {
	w, err := newGramRunWriter(path)
	if err != nil {
		return nil, err
	}
	var prevGram uint32
	first := true
	err = mergeRunKeys(ctx, runPaths(batch), func(gram uint32, ids []uint32) error {
		if first {
			w.uvarint(uint64(gram))
			first = false
		} else {
			w.uvarint(uint64(gram - prevGram))
		}
		prevGram = gram
		w.uvarint(uint64(len(ids)))
		var prevID uint32
		firstID := true
		for _, id := range ids {
			if firstID {
				w.uvarint(uint64(id))
				firstID = false
			} else {
				w.uvarint(uint64(id - prevID))
			}
			prevID = id
		}
		return w.maybeFlush()
	})
	if err != nil {
		_ = w.close()
		return nil, err
	}
	if err := w.close(); err != nil {
		return nil, err
	}
	return []gramRunFile{{path: path}}, nil
}

func runPaths(runs []gramRunFile) []string {
	paths := make([]string, len(runs))
	for i, r := range runs {
		paths[i] = r.path
	}
	return paths
}
