package main

import (
	"bufio"
	"container/heap"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

type directV9ChildPair struct {
	Parent uint32
	Child  uint32
}

func directV9WriteChildRun(path string, pairs []directV9ChildPair) (int64, error) {
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Parent != pairs[j].Parent {
			return pairs[i].Parent < pairs[j].Parent
		}
		return pairs[i].Child < pairs[j].Child
	})
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	bw := bufio.NewWriterSize(f, 256*1024)
	var buf [8]byte
	var written int64
	for _, pair := range pairs {
		binary.LittleEndian.PutUint32(buf[0:4], pair.Parent)
		binary.LittleEndian.PutUint32(buf[4:8], pair.Child)
		if _, err := bw.Write(buf[:]); err != nil {
			_ = f.Close()
			return written, err
		}
		written += int64(len(buf))
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		return written, err
	}
	// Owned scratch; merged and removed within the same process.
	return written, f.Close()
}

type directV9ChildHead struct {
	Pair directV9ChildPair
	Run  int
}

type directV9ChildHeap struct{ items []directV9ChildHead }

func (h directV9ChildHeap) Len() int { return len(h.items) }
func (h directV9ChildHeap) Less(i, j int) bool {
	a, b := h.items[i].Pair, h.items[j].Pair
	if a.Parent != b.Parent {
		return a.Parent < b.Parent
	}
	return a.Child < b.Child
}
func (h directV9ChildHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *directV9ChildHeap) Push(x any)   { h.items = append(h.items, x.(directV9ChildHead)) }
func (h *directV9ChildHeap) Pop() any {
	old := h.items
	n := len(old)
	x := old[n-1]
	h.items = old[:n-1]
	return x
}

func directV9ReadChildPair(r *bufio.Reader) (directV9ChildPair, error) {
	var buf [8]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return directV9ChildPair{}, err
	}
	return directV9ChildPair{Parent: binary.LittleEndian.Uint32(buf[0:4]), Child: binary.LittleEndian.Uint32(buf[4:8])}, nil
}

func directV9CheckParentCycles(ctx context.Context, parentPath string, recordCount int) error {
	data, err := os.ReadFile(parentPath)
	if err != nil {
		return err
	}
	if len(data) < recordCount*4 {
		return errors.New("direct v9 topology parents file too short")
	}
	state := make([]byte, recordCount)
	stamp := make([]uint32, recordCount)
	var walkID uint32
	readParent := func(id int) (int32, error) {
		value := binary.LittleEndian.Uint32(data[id*4:])
		if value == ^uint32(0) {
			return -1, nil
		}
		if value >= uint32(recordCount) {
			return -1, errors.New("direct v9 topology parent ID out of range")
		}
		return int32(value), nil
	}
	for start := 0; start < recordCount; start++ {
		if state[start] != 0 {
			continue
		}
		walkID++
		if walkID == 0 {
			for i := range stamp {
				stamp[i] = 0
			}
			walkID = 1
		}
		cur := start
		for cur >= 0 && state[cur] == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			if stamp[cur] == walkID {
				return errors.New("direct v9 topology parent cycle")
			}
			stamp[cur] = walkID
			parent, err := readParent(cur)
			if err != nil {
				return err
			}
			cur = int(parent)
		}
		cur = start
		for cur >= 0 && state[cur] == 0 {
			state[cur] = 1
			parent, err := readParent(cur)
			if err != nil {
				return err
			}
			cur = int(parent)
		}
	}
	return nil
}

func directV9CopyFile(cw *countingWriter, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(cw, f)
	return err
}

func directV9MergeChildRuns(ctx context.Context, cw *countingWriter, runs []directV9RunFile, expected int) error {
	files := make([]*os.File, len(runs))
	readers := make([]*bufio.Reader, len(runs))
	h := &directV9ChildHeap{}
	heap.Init(h)
	for i, run := range runs {
		f, err := os.Open(run.path)
		if err != nil {
			for _, opened := range files {
				if opened != nil {
					_ = opened.Close()
				}
			}
			return err
		}
		files[i] = f
		readers[i] = bufio.NewReaderSize(f, 256*1024)
		pair, readErr := directV9ReadChildPair(readers[i])
		if readErr == nil {
			heap.Push(h, directV9ChildHead{Pair: pair, Run: i})
		} else if !errors.Is(readErr, io.EOF) {
			for _, opened := range files {
				if opened != nil {
					_ = opened.Close()
				}
			}
			return readErr
		}
	}
	defer func() {
		for _, f := range files {
			if f != nil {
				_ = f.Close()
			}
		}
	}()
	var written int
	var buf [4]byte
	for h.Len() > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		head := heap.Pop(h).(directV9ChildHead)
		binary.LittleEndian.PutUint32(buf[:], head.Pair.Child)
		if _, err := cw.Write(buf[:]); err != nil {
			return err
		}
		written++
		next, readErr := directV9ReadChildPair(readers[head.Run])
		if readErr == nil {
			heap.Push(h, directV9ChildHead{Pair: next, Run: head.Run})
		} else if !errors.Is(readErr, io.EOF) {
			return readErr
		}
	}
	if written != expected {
		return fmt.Errorf("direct v9 topology child count mismatch: wrote %d want %d", written, expected)
	}
	return nil
}

func directV9WriteTopologySections(ctx context.Context, cw *countingWriter, finalPath, frnPath, spoolDir string, recordCount, maxRecords int, owned *[]string, scratchHigh *int64) ([]indexSectionTableEntry, []directV9SectionReport, error) {
	parentPath := filepath.Join(spoolDir, "direct-v9-parents.tmp")
	offsetsPath := filepath.Join(spoolDir, "direct-v9-child-offsets.tmp")
	rootsPath := filepath.Join(spoolDir, "direct-v9-roots.tmp")
	sizesPath := filepath.Join(spoolDir, "direct-v9-sizes.tmp")
	*owned = append(*owned, parentPath, offsetsPath, rootsPath, sizesPath)
	parents, err := os.Create(parentPath)
	if err != nil {
		return nil, nil, err
	}
	roots, err := os.Create(rootsPath)
	if err != nil {
		_ = parents.Close()
		return nil, nil, err
	}
	sizes, err := os.Create(sizesPath)
	if err != nil {
		_ = parents.Close()
		_ = roots.Close()
		return nil, nil, err
	}
	parentWriter := bufio.NewWriterSize(parents, 256*1024)
	rootWriter := bufio.NewWriterSize(roots, 256*1024)
	sizeWriter := bufio.NewWriterSize(sizes, 256*1024)
	counts := make([]uint32, recordCount)
	final, err := os.Open(finalPath)
	if err != nil {
		_ = parents.Close()
		_ = roots.Close()
		_ = sizes.Close()
		return nil, nil, err
	}
	frnMap, frns, err := directV9MapFRNs(frnPath, recordCount)
	if err != nil {
		_ = final.Close()
		_ = parents.Close()
		_ = roots.Close()
		_ = sizes.Close()
		return nil, nil, err
	}
	if frnMap != nil {
		defer frnMap.close()
	}
	r := bufio.NewReaderSize(final, 256*1024)
	for id := 0; id < recordCount; id++ {
		select {
		case <-ctx.Done():
			_ = final.Close()
			_ = parents.Close()
			_ = roots.Close()
			_ = sizes.Close()
			return nil, nil, ctx.Err()
		default:
		}
		rec, readErr := readDirectV9SpoolRecord(r)
		if readErr != nil {
			_ = final.Close()
			_ = parents.Close()
			_ = roots.Close()
			_ = sizes.Close()
			return nil, nil, readErr
		}
		// Directory aggregate sizes sum descendant file sizes; a directory's
		// own entry contributes nothing so the subtree post-order sum is exact.
		var size uint64
		if rec.Mode&uint32(os.ModeDir) == 0 && rec.Size > 0 {
			size = uint64(rec.Size)
		}
		var sz [8]byte
		binary.LittleEndian.PutUint64(sz[:], size)
		if _, err := sizeWriter.Write(sz[:]); err != nil {
			_ = final.Close()
			_ = parents.Close()
			_ = roots.Close()
			_ = sizes.Close()
			return nil, nil, err
		}
		parentID := int32(-1)
		if rec.ParentFRN != 0 {
			parentID = directV9LookupIDMapped(frns, rec.ParentFRN)
		}
		if parentID == int32(id) {
			_ = final.Close()
			_ = parents.Close()
			_ = roots.Close()
			_ = sizes.Close()
			return nil, nil, errors.New("direct v9 topology self-parent")
		}
		var b [4]byte
		if parentID < 0 {
			binary.LittleEndian.PutUint32(b[:], ^uint32(0))
			if _, err := rootWriter.Write(func() []byte { var v [4]byte; binary.LittleEndian.PutUint32(v[:], uint32(id)); return v[:] }()); err != nil {
				_ = final.Close()
				_ = parents.Close()
				_ = roots.Close()
				_ = sizes.Close()
				return nil, nil, err
			}
		} else {
			binary.LittleEndian.PutUint32(b[:], uint32(parentID))
			counts[parentID]++
		}
		if _, err := parentWriter.Write(b[:]); err != nil {
			_ = final.Close()
			_ = parents.Close()
			_ = roots.Close()
			_ = sizes.Close()
			return nil, nil, err
		}
	}
	_ = final.Close()
	if err := parentWriter.Flush(); err != nil {
		_ = parents.Close()
		_ = roots.Close()
		_ = sizes.Close()
		return nil, nil, err
	}
	if err := rootWriter.Flush(); err != nil {
		_ = parents.Close()
		_ = roots.Close()
		_ = sizes.Close()
		return nil, nil, err
	}
	if err := sizeWriter.Flush(); err != nil {
		_ = parents.Close()
		_ = roots.Close()
		_ = sizes.Close()
		return nil, nil, err
	}
	_ = parents.Close()
	_ = roots.Close()
	_ = sizes.Close()
	if err := directV9CheckParentCycles(ctx, parentPath, recordCount); err != nil {
		return nil, nil, err
	}
	offsets, err := os.Create(offsetsPath)
	if err != nil {
		return nil, nil, err
	}
	offsetWriter := bufio.NewWriterSize(offsets, 256*1024)
	var childCount uint64
	var off [4]byte
	for _, count := range counts {
		childCount += uint64(count)
	}
	var running uint32
	for i := 0; i <= recordCount; i++ {
		if i > 0 {
			running += counts[i-1]
		}
		binary.LittleEndian.PutUint32(off[:], running)
		if _, err := offsetWriter.Write(off[:]); err != nil {
			_ = offsets.Close()
			return nil, nil, err
		}
	}
	if err := offsetWriter.Flush(); err != nil {
		_ = offsets.Close()
		return nil, nil, err
	}
	_ = offsets.Close()
	if childCount > uint64(^uint32(0)) {
		return nil, nil, errors.New("direct v9 topology child count exceeds format")
	}
	if maxRecords <= 0 {
		maxRecords = directV9DefaultRunRecords
	}
	pfile, err := os.Open(parentPath)
	if err != nil {
		return nil, nil, err
	}
	reader := bufio.NewReaderSize(pfile, 256*1024)
	pairs := make([]directV9ChildPair, 0, min(maxRecords, 4096))
	runs := make([]directV9RunFile, 0)
	flush := func() error {
		if len(pairs) == 0 {
			return nil
		}
		path := filepath.Join(spoolDir, fmt.Sprintf("direct-v9-child-%06d.tmp", len(runs)))
		bytes, err := directV9WriteChildRun(path, pairs)
		if err != nil {
			return err
		}
		*owned = append(*owned, path)
		runs = append(runs, directV9RunFile{path: path, bytes: bytes})
		pairs = make([]directV9ChildPair, 0, min(maxRecords, 4096))
		return nil
	}
	for id := 0; id < recordCount; id++ {
		var b [4]byte
		if _, err := io.ReadFull(reader, b[:]); err != nil {
			_ = pfile.Close()
			return nil, nil, err
		}
		parent := binary.LittleEndian.Uint32(b[:])
		if parent != ^uint32(0) {
			pairs = append(pairs, directV9ChildPair{Parent: parent, Child: uint32(id)})
		}
		if len(pairs) >= maxRecords {
			if err := flush(); err != nil {
				_ = pfile.Close()
				return nil, nil, err
			}
		}
	}
	if err := flush(); err != nil {
		_ = pfile.Close()
		return nil, nil, err
	}
	_ = pfile.Close()
	var runBytes int64
	for _, run := range runs {
		runBytes += run.bytes
	}
	parentInfo, _ := os.Stat(parentPath)
	offsetInfo, _ := os.Stat(offsetsPath)
	rootInfo, _ := os.Stat(rootsPath)
	topoScratch := runBytes
	if parentInfo != nil {
		topoScratch += parentInfo.Size()
	}
	if offsetInfo != nil {
		topoScratch += offsetInfo.Size()
	}
	if rootInfo != nil {
		topoScratch += rootInfo.Size()
	}
	if base := topoScratch; base > *scratchHigh {
		*scratchHigh = base
	}
	entries := make([]indexSectionTableEntry, 0, 2)
	reports := make([]directV9SectionReport, 0, 2)
	if err := writeAlignment(cw, 8); err != nil {
		return nil, nil, err
	}
	offset := uint64(cw.n)
	if err := binary.Write(cw, binary.LittleEndian, uint32(recordCount+1)); err != nil {
		return nil, nil, err
	}
	if err := directV9CopyFile(cw, offsetsPath); err != nil {
		return nil, nil, err
	}
	if err := binary.Write(cw, binary.LittleEndian, uint32(childCount)); err != nil {
		return nil, nil, err
	}
	if err := directV9MergeChildRuns(ctx, cw, runs, int(childCount)); err != nil {
		return nil, nil, err
	}
	rootCount := int(rootInfo.Size() / 4)
	if err := binary.Write(cw, binary.LittleEndian, uint32(rootCount)); err != nil {
		return nil, nil, err
	}
	if err := directV9CopyFile(cw, rootsPath); err != nil {
		return nil, nil, err
	}
	entries = append(entries, indexSectionTableEntry{tag: indexSectionCHLD, offset: offset, length: uint64(cw.n) - offset})
	reports = append(reports, directV9SectionReport{Name: "CHLD", Tag: indexSectionCHLD, Runs: len(runs), Bytes: int64(cw.n) - int64(offset), ScratchBytes: topoScratch})
	for _, run := range runs {
		_ = os.Remove(run.path)
	}
	if err := writeAlignment(cw, 8); err != nil {
		return nil, nil, err
	}
	offset = uint64(cw.n)
	if err := binary.Write(cw, binary.LittleEndian, uint32(recordCount)); err != nil {
		return nil, nil, err
	}
	if err := directV9CopyFile(cw, frnPath); err != nil {
		return nil, nil, err
	}
	if err := binary.Write(cw, binary.LittleEndian, uint32(recordCount)); err != nil {
		return nil, nil, err
	}
	var ids [256]byte
	for start := 0; start < recordCount; {
		count := min(recordCount-start, len(ids)/4)
		for i := 0; i < count; i++ {
			binary.LittleEndian.PutUint32(ids[i*4:], uint32(start+i))
		}
		if _, err := cw.Write(ids[:count*4]); err != nil {
			return nil, nil, err
		}
		start += count
	}
	entries = append(entries, indexSectionTableEntry{tag: indexSectionFRNS, offset: offset, length: uint64(cw.n) - offset})
	reports = append(reports, directV9SectionReport{Name: "FRNS", Tag: indexSectionFRNS, Runs: 0, Bytes: int64(cw.n) - int64(offset), ScratchBytes: 0})
	return entries, reports, nil
}
