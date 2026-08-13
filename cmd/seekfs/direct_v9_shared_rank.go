package main

// Shared rank construction is the first production-throughput slice.  It
// reads the canonical record spool once, computes all rank keys in that pass,
// and sorts bounded run chunks with a capped worker pool.  The output merge
// remains in deterministic section order; worker scheduling affects only
// when an already-named run becomes available.

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const directV9SharedRankChunkBytes = 2 * 1024 * 1024

type directV9SharedRankRuns struct {
	Runs       map[string][]directV9RunFile
	LiveCounts map[string]int
	MaxBytes   map[string]int64
}

type directV9RankSortJob struct {
	specIndex int
	runIndex  int
	path      string
	items     []directV9RankItem
}

func directV9WriteRankRunItems(path string, items []directV9RankItem) (int64, error) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Key != items[j].Key {
			return items[i].Key < items[j].Key
		}
		return items[i].ID < items[j].ID
	})
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	bw := bufio.NewWriterSize(f, 256*1024)
	var written int64
	for _, item := range items {
		var id [4]byte
		binary.LittleEndian.PutUint32(id[:], item.ID)
		if _, err := bw.Write(id[:]); err != nil {
			_ = f.Close()
			return written, err
		}
		if uint64(len(item.Key)) > uint64(^uint32(0)) {
			_ = f.Close()
			return written, errors.New("direct v9 rank key too large")
		}
		var n [4]byte
		binary.LittleEndian.PutUint32(n[:], uint32(len(item.Key)))
		if _, err := bw.Write(n[:]); err != nil {
			_ = f.Close()
			return written, err
		}
		if _, err := io.WriteString(bw, item.Key); err != nil {
			_ = f.Close()
			return written, err
		}
		written += int64(8 + len(item.Key))
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		return written, err
	}
	// Owned scratch; merged and removed within the same process.
	return written, f.Close()
}

func directV9BuildRankRunsShared(ctx context.Context, finalPath, spoolDir string, maxRecords, workers int, specs []directV9RankSpec, owned *[]string) (directV9SharedRankRuns, error) {
	if maxRecords <= 0 {
		maxRecords = directV9DefaultRunRecords
	}
	if workers <= 0 {
		workers = 1
	}
	if workers > 16 {
		workers = 16
	}
	// Six small bounded chunks keep key-generation memory independent of the
	// record count and avoid holding one full rank vector per family.
	chunkRecords := min(maxRecords, max(4096, 65536/workers))
	result := directV9SharedRankRuns{
		Runs:       make(map[string][]directV9RunFile, len(specs)),
		LiveCounts: make(map[string]int, len(specs)),
		MaxBytes:   make(map[string]int64, len(specs)),
	}
	chunks := make([][]directV9RankItem, len(specs))
	chunkBytes := make([]int64, len(specs))
	for i, spec := range specs {
		chunks[i] = make([]directV9RankItem, 0, chunkRecords)
		result.Runs[spec.Name] = nil
	}

	internalCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan directV9RankSortJob, workers*2)
	var workerWG sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	var resultMu sync.Mutex
	recordErr := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		errMu.Unlock()
	}
	for worker := 0; worker < workers; worker++ {
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			for job := range jobs {
				if err := internalCtx.Err(); err != nil {
					return
				}
				bytes, err := directV9WriteRankRunItems(job.path, job.items)
				if err != nil {
					recordErr(err)
					return
				}
				resultMu.Lock()
				result.Runs[specs[job.specIndex].Name][job.runIndex].bytes = bytes
				if bytes > result.MaxBytes[specs[job.specIndex].Name] {
					result.MaxBytes[specs[job.specIndex].Name] = bytes
				}
				resultMu.Unlock()
			}
		}()
	}
	flush := func(specIndex int) error {
		items := chunks[specIndex]
		if len(items) == 0 {
			return nil
		}
		spec := specs[specIndex]
		resultMu.Lock()
		runIndex := len(result.Runs[spec.Name])
		path := filepath.Join(spoolDir, fmt.Sprintf("direct-v9-rank-%s-%06d.tmp", spec.Name, runIndex))
		*owned = append(*owned, path)
		result.Runs[spec.Name] = append(result.Runs[spec.Name], directV9RunFile{path: path})
		resultMu.Unlock()
		job := directV9RankSortJob{specIndex: specIndex, runIndex: runIndex, path: path, items: items}
		select {
		case jobs <- job:
			chunks[specIndex] = make([]directV9RankItem, 0, chunkRecords)
			chunkBytes[specIndex] = 0
			return nil
		case <-internalCtx.Done():
			return internalCtx.Err()
		}
	}
	f, err := os.Open(finalPath)
	if err != nil {
		close(jobs)
		workerWG.Wait()
		return result, err
	}
	r := bufio.NewReaderSize(f, 256*1024)
	var id uint32
	for {
		select {
		case <-internalCtx.Done():
			_ = f.Close()
			close(jobs)
			workerWG.Wait()
			return result, internalCtx.Err()
		default:
		}
		rec, readErr := readDirectV9SpoolRecord(r)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			recordErr(readErr)
			break
		}
		if !rec.Deleted() {
			for i, spec := range specs {
				key := spec.Key(rec)
				chunks[i] = append(chunks[i], directV9RankItem{Key: key, ID: id})
				chunkBytes[i] += int64(len(key))
				result.LiveCounts[spec.Name]++
				if len(chunks[i]) >= chunkRecords || chunkBytes[i] >= directV9SharedRankChunkBytes {
					if err := flush(i); err != nil {
						recordErr(err)
						break
					}
				}
			}
		}
		id++
	}
	_ = f.Close()
	errMu.Lock()
	haveErr := firstErr != nil
	errMu.Unlock()
	if !haveErr && internalCtx.Err() == nil {
		for i := range specs {
			if err := flush(i); err != nil {
				recordErr(err)
			}
		}
	}
	close(jobs)
	workerWG.Wait()
	errMu.Lock()
	err = firstErr
	errMu.Unlock()
	if err == nil {
		err = internalCtx.Err()
	}
	if err != nil {
		return result, err
	}
	return result, nil
}
