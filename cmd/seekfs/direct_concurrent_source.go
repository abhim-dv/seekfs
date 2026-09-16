package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	directConcurrentDefaultWorkers = 4
	directConcurrentDefaultQueue   = 2
)

// directConcurrentWalkOptions bounds the only in-flight filesystem work.
// Records are still ordered by the builder's final FRN sort, so worker
// completion order is deliberately not part of the output contract.
type directConcurrentWalkOptions struct {
	Workers int
	Queue   int
}

func (o directConcurrentWalkOptions) normalized() (directConcurrentWalkOptions, error) {
	if o.Workers < 0 || o.Queue < 0 {
		return directConcurrentWalkOptions{}, errors.New("direct concurrent walk limits must be non-negative")
	}
	if o.Workers == 0 {
		o.Workers = directConcurrentDefaultWorkers
	}
	if o.Queue == 0 {
		o.Queue = o.Workers * directConcurrentDefaultQueue
	}
	if o.Queue < 1 {
		return directConcurrentWalkOptions{}, errors.New("direct concurrent walk queue must be positive")
	}
	return o, nil
}

type directConcurrentWalkJob struct {
	path  string
	entry os.DirEntry
}

type directConcurrentDirBatch struct {
	root  string
	items []os.DirEntry
	done  bool
	err   error
}

type directFileInfoDirEntry struct{ info fs.FileInfo }

func (e directFileInfoDirEntry) Name() string               { return e.info.Name() }
func (e directFileInfoDirEntry) IsDir() bool                { return e.info.IsDir() }
func (e directFileInfoDirEntry) Type() fs.FileMode          { return e.info.Mode().Type() }
func (e directFileInfoDirEntry) Info() (fs.FileInfo, error) { return e.info, nil }

type directConcurrentWalkSource struct {
	records chan directRecord
	jobs    chan directConcurrentWalkJob
	done    chan struct{}
	finish  chan struct{}

	once       sync.Once
	workers    sync.WaitGroup
	reportMu   sync.Mutex
	errMu      sync.Mutex
	err        error
	report     *directWalkReport
	root       string
	dirWorkers int
}

// newDirectConcurrentWalkSource is the unfiltered convenience constructor.
func newDirectConcurrentWalkSource(root string, workers, queue int) (directRecordSource, error) {
	return newDirectConcurrentWalkSourceWithOptions(root, nil, nil, nil,
		directConcurrentWalkOptions{Workers: workers, Queue: queue})
}

// newDirectConcurrentWalkSourceWithOptions starts a bounded, read-only walk.
// The producer retains at most Queue jobs and each worker retains one record;
// results are likewise bounded by Queue.
func newDirectConcurrentWalkSourceWithOptions(root string, exclusionRoots, exclusionSuffixes []string, report *directWalkReport, options directConcurrentWalkOptions) (directRecordSource, error) {
	options, err := options.normalized()
	if err != nil {
		return nil, err
	}
	abs, canonicalExclusions, err := directConcurrentCanonicalPaths(root, exclusionRoots)
	if err != nil {
		return nil, err
	}
	if directPathIsReparse(abs) {
		return nil, fmt.Errorf("direct concurrent walk root is a reparse point: %s", abs)
	}
	if report != nil {
		report.Root = abs
		report.Exclusions = append([]string(nil), canonicalExclusions...)
		report.SourceComplete = true
	}

	s := &directConcurrentWalkSource{
		records:    make(chan directRecord, options.Queue),
		jobs:       make(chan directConcurrentWalkJob, options.Queue),
		done:       make(chan struct{}),
		finish:     make(chan struct{}),
		report:     report,
		root:       abs,
		dirWorkers: options.Workers,
	}
	s.workers.Add(options.Workers)
	for i := 0; i < options.Workers; i++ {
		go s.worker()
	}
	go s.runProducer(abs, canonicalExclusions, exclusionSuffixes)
	return s, nil
}

// newDirectConcurrentWalkSourceWithExclusions keeps the same positional
// shape as the existing walk constructor and adds worker/queue bounds.
func newDirectConcurrentWalkSourceWithExclusions(root string, exclusionRoots, exclusionSuffixes []string, report *directWalkReport, workers, queue int) (directRecordSource, error) {
	return newDirectConcurrentWalkSourceWithOptions(root, exclusionRoots, exclusionSuffixes, report,
		directConcurrentWalkOptions{Workers: workers, Queue: queue})
}

func directConcurrentCanonicalPaths(root string, exclusionRoots []string) (string, []string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", nil, err
	}
	abs = filepath.Clean(abs)

	canonical := make([]string, 0, len(exclusionRoots))
	for _, exclusion := range exclusionRoots {
		if exclusion == "" {
			continue
		}
		path, pathErr := filepath.Abs(exclusion)
		if pathErr != nil {
			return "", nil, pathErr
		}
		path, pathErr = filepath.EvalSymlinks(path)
		if pathErr != nil {
			// A fresh run directory may not exist yet. Canonicalize its
			// existing parent and retain the requested leaf.
			parent, parentErr := filepath.EvalSymlinks(filepath.Dir(path))
			if parentErr != nil {
				return "", nil, parentErr
			}
			path = filepath.Join(parent, filepath.Base(path))
		}
		canonical = append(canonical, filepath.Clean(path))
	}
	return abs, canonical, nil
}

func (s *directConcurrentWalkSource) runProducer(root string, exclusions, suffixes []string) {
	// Keep directory enumeration bounded as well as metadata reads. A worker
	// owns one open directory at a time and reports at most 256 entries per
	// batch; the coordinator is the only owner of the pending directory queue.
	// This avoids a producer-sized path slice while allowing independent
	// directories to be enumerated concurrently.
	dirWorkers := s.dirWorkers
	if dirWorkers < 1 {
		dirWorkers = 1
	}
	dirJobs := make(chan string, dirWorkers)
	dirResults := make(chan directConcurrentDirBatch, dirWorkers*2)
	var dirWG sync.WaitGroup
	for i := 0; i < dirWorkers; i++ {
		dirWG.Add(1)
		go func() {
			defer dirWG.Done()
			for {
				select {
				case <-s.done:
					return
				case dir, ok := <-dirJobs:
					if !ok {
						return
					}
					s.enumerateDirectory(dir, dirResults)
				}
			}
		}()
	}

	if info, err := os.Lstat(root); err != nil {
		s.note("inaccessible", root, true)
	} else if !sendDirectWalkJob(s.done, s.jobs, directConcurrentWalkJob{path: root, entry: directFileInfoDirEntry{info: info}}) {
		close(dirJobs)
		dirWG.Wait()
		close(s.jobs)
		s.workers.Wait()
		close(s.records)
		close(s.finish)
		return
	}
	pending := []string{root}
	inflight := 0
	cancelled := false
	for !cancelled && (len(pending) > 0 || inflight > 0) {
		var dispatch chan<- string
		var next string
		if len(pending) > 0 && inflight < dirWorkers {
			dispatch = dirJobs
			next = pending[0]
		}
		select {
		case dispatch <- next:
			pending = pending[1:]
			inflight++
		case batch := <-dirResults:
			if batch.done {
				inflight--
				if batch.err != nil && !errors.Is(batch.err, context.Canceled) {
					s.note("inaccessible", batch.root, true)
				}
				continue
			}
			for _, entry := range batch.items {
				path := filepath.Join(batch.root, entry.Name())
				if directPathUnderAny(path, exclusions) {
					s.note("excluded", path, false)
					continue
				}
				select {
				case <-s.done:
					break
				default:
				}
				reparse := directPathIsReparse(path)
				if reparse {
					// The entry itself is valid input; only its target is not.
					// Index it, but never enqueue a reparse directory for descent.
					s.note("reparse-not-followed", path, false)
				}
				if directHasExcludedSuffix(path, suffixes) {
					s.note("excluded", path, false)
					continue
				}
				if entry.IsDir() && !reparse {
					pending = append(pending, path)
				}
				select {
				case s.jobs <- directConcurrentWalkJob{path: path, entry: entry}:
				case <-s.done:
					pending = nil
				}
			}
		case <-s.done:
			pending = nil
			cancelled = true
		}
	}
	close(dirJobs)
	dirWG.Wait()
	close(s.jobs)
	s.workers.Wait()
	close(s.records)
	close(s.finish)
}

func sendDirectWalkJob(done <-chan struct{}, jobs chan<- directConcurrentWalkJob, job directConcurrentWalkJob) bool {
	select {
	case jobs <- job:
		return true
	case <-done:
		return false
	}
}

func directHasExcludedSuffix(path string, suffixes []string) bool {
	lowerPath := strings.ToLower(path)
	for _, suffix := range suffixes {
		if strings.HasSuffix(lowerPath, strings.ToLower(suffix)) {
			return true
		}
	}
	return false
}

func (s *directConcurrentWalkSource) enumerateDirectory(root string, results chan<- directConcurrentDirBatch) {
	f, err := os.Open(root)
	if err != nil {
		sendDirectDirBatch(s.done, results, directConcurrentDirBatch{root: root, done: true, err: err})
		return
	}
	defer f.Close()
	for {
		entries, readErr := f.ReadDir(256)
		if len(entries) > 0 {
			if !sendDirectDirBatch(s.done, results, directConcurrentDirBatch{root: root, items: entries}) {
				return
			}
		}
		if errors.Is(readErr, io.EOF) || len(entries) == 0 {
			sendDirectDirBatch(s.done, results, directConcurrentDirBatch{root: root, done: true, err: nil})
			return
		}
		if readErr != nil {
			sendDirectDirBatch(s.done, results, directConcurrentDirBatch{root: root, done: true, err: readErr})
			return
		}
	}
}

func sendDirectDirBatch(done <-chan struct{}, results chan<- directConcurrentDirBatch, batch directConcurrentDirBatch) bool {
	select {
	case results <- batch:
		return true
	case <-done:
		return false
	}
}

func (s *directConcurrentWalkSource) worker() {
	defer s.workers.Done()
	for {
		select {
		case <-s.done:
			return
		case job, ok := <-s.jobs:
			if !ok {
				return
			}
			s.emit(job)
		}
	}
}

func (s *directConcurrentWalkSource) emit(job directConcurrentWalkJob) {
	select {
	case <-s.done:
		return
	default:
	}
	info, err := job.entry.Info()
	if err != nil {
		s.note("inaccessible", job.path, true)
		return
	}
	clean := filepath.Clean(job.path)
	parent := uint64(0)
	if filepath.Clean(s.root) != clean {
		parent = directStablePathID(filepath.Dir(clean))
	}
	record := directRecord{
		FRN:       directStablePathID(clean),
		ParentFRN: parent,
		Mode:      uint32(info.Mode()),
		Size:      info.Size(),
		ModUnix:   directWalkModUnix(info),
		Name:      job.entry.Name(),
		Path:      clean,
	}
	select {
	case s.records <- record:
	case <-s.done:
	}
}

func (s *directConcurrentWalkSource) note(kind, path string, incomplete bool) {
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
	if s.report == nil {
		return
	}
	s.report.note(kind, path)
	if incomplete {
		s.report.SourceComplete = false
	}
}

func (s *directConcurrentWalkSource) setErr(err error) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

func (s *directConcurrentWalkSource) sourceErr() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *directConcurrentWalkSource) Next(ctx context.Context) (directRecord, error) {
	select {
	case <-s.done:
		return directRecord{}, io.EOF
	default:
	}
	select {
	case <-ctx.Done():
		return directRecord{}, ctx.Err()
	case <-s.done:
		return directRecord{}, io.EOF
	case record, ok := <-s.records:
		if ok {
			return record, nil
		}
		if err := s.sourceErr(); err != nil {
			return directRecord{}, err
		}
		return directRecord{}, io.EOF
	}
}

func (s *directConcurrentWalkSource) Close() {
	s.once.Do(func() { close(s.done) })
	<-s.finish
}
