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
	directV9ConcurrentDefaultWorkers = 4
	directV9ConcurrentDefaultQueue   = 2
)

// directV9ConcurrentWalkOptions bounds the only in-flight filesystem work.
// Records are still ordered by the builder's final FRN sort, so worker
// completion order is deliberately not part of the output contract.
type directV9ConcurrentWalkOptions struct {
	Workers int
	Queue   int
}

func (o directV9ConcurrentWalkOptions) normalized() (directV9ConcurrentWalkOptions, error) {
	if o.Workers < 0 || o.Queue < 0 {
		return directV9ConcurrentWalkOptions{}, errors.New("direct v9 concurrent walk limits must be non-negative")
	}
	if o.Workers == 0 {
		o.Workers = directV9ConcurrentDefaultWorkers
	}
	if o.Queue == 0 {
		o.Queue = o.Workers * directV9ConcurrentDefaultQueue
	}
	if o.Queue < 1 {
		return directV9ConcurrentWalkOptions{}, errors.New("direct v9 concurrent walk queue must be positive")
	}
	return o, nil
}

type directV9ConcurrentWalkJob struct {
	path  string
	entry os.DirEntry
}

type directV9ConcurrentDirBatch struct {
	root  string
	items []os.DirEntry
	done  bool
	err   error
}

type directV9FileInfoDirEntry struct{ info fs.FileInfo }

func (e directV9FileInfoDirEntry) Name() string               { return e.info.Name() }
func (e directV9FileInfoDirEntry) IsDir() bool                { return e.info.IsDir() }
func (e directV9FileInfoDirEntry) Type() fs.FileMode          { return e.info.Mode().Type() }
func (e directV9FileInfoDirEntry) Info() (fs.FileInfo, error) { return e.info, nil }

type directV9ConcurrentWalkSource struct {
	records chan directV9Record
	jobs    chan directV9ConcurrentWalkJob
	done    chan struct{}
	finish  chan struct{}

	once       sync.Once
	workers    sync.WaitGroup
	reportMu   sync.Mutex
	errMu      sync.Mutex
	err        error
	report     *directV9WalkReport
	root       string
	dirWorkers int
}

// newDirectV9ConcurrentWalkSource is the unfiltered convenience constructor.
func newDirectV9ConcurrentWalkSource(root string, workers, queue int) (directV9RecordSource, error) {
	return newDirectV9ConcurrentWalkSourceWithOptions(root, nil, nil, nil,
		directV9ConcurrentWalkOptions{Workers: workers, Queue: queue})
}

// newDirectV9ConcurrentWalkSourceWithOptions starts a bounded, read-only walk.
// The producer retains at most Queue jobs and each worker retains one record;
// results are likewise bounded by Queue.
func newDirectV9ConcurrentWalkSourceWithOptions(root string, exclusionRoots, exclusionSuffixes []string, report *directV9WalkReport, options directV9ConcurrentWalkOptions) (directV9RecordSource, error) {
	options, err := options.normalized()
	if err != nil {
		return nil, err
	}
	abs, canonicalExclusions, err := directV9ConcurrentCanonicalPaths(root, exclusionRoots)
	if err != nil {
		return nil, err
	}
	if directV9PathIsReparse(abs) {
		return nil, fmt.Errorf("direct v9 concurrent walk root is a reparse point: %s", abs)
	}
	if report != nil {
		report.Root = abs
		report.Exclusions = append([]string(nil), canonicalExclusions...)
		report.SourceComplete = true
	}

	s := &directV9ConcurrentWalkSource{
		records:    make(chan directV9Record, options.Queue),
		jobs:       make(chan directV9ConcurrentWalkJob, options.Queue),
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

// newDirectV9ConcurrentWalkSourceWithExclusions keeps the same positional
// shape as the existing walk constructor and adds worker/queue bounds.
func newDirectV9ConcurrentWalkSourceWithExclusions(root string, exclusionRoots, exclusionSuffixes []string, report *directV9WalkReport, workers, queue int) (directV9RecordSource, error) {
	return newDirectV9ConcurrentWalkSourceWithOptions(root, exclusionRoots, exclusionSuffixes, report,
		directV9ConcurrentWalkOptions{Workers: workers, Queue: queue})
}

func directV9ConcurrentCanonicalPaths(root string, exclusionRoots []string) (string, []string, error) {
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

func (s *directV9ConcurrentWalkSource) runProducer(root string, exclusions, suffixes []string) {
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
	dirResults := make(chan directV9ConcurrentDirBatch, dirWorkers*2)
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
	} else if !sendDirectV9WalkJob(s.done, s.jobs, directV9ConcurrentWalkJob{path: root, entry: directV9FileInfoDirEntry{info: info}}) {
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
				if directV9PathUnderAny(path, exclusions) {
					s.note("excluded", path, false)
					continue
				}
				select {
				case <-s.done:
					break
				default:
				}
				reparse := directV9PathIsReparse(path)
				if reparse {
					// The entry itself is valid input; only its target is not.
					// Index it, but never enqueue a reparse directory for descent.
					s.note("reparse-not-followed", path, false)
				}
				if directV9HasExcludedSuffix(path, suffixes) {
					s.note("excluded", path, false)
					continue
				}
				if entry.IsDir() && !reparse {
					pending = append(pending, path)
				}
				select {
				case s.jobs <- directV9ConcurrentWalkJob{path: path, entry: entry}:
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

func sendDirectV9WalkJob(done <-chan struct{}, jobs chan<- directV9ConcurrentWalkJob, job directV9ConcurrentWalkJob) bool {
	select {
	case jobs <- job:
		return true
	case <-done:
		return false
	}
}

func directV9HasExcludedSuffix(path string, suffixes []string) bool {
	lowerPath := strings.ToLower(path)
	for _, suffix := range suffixes {
		if strings.HasSuffix(lowerPath, strings.ToLower(suffix)) {
			return true
		}
	}
	return false
}

func (s *directV9ConcurrentWalkSource) enumerateDirectory(root string, results chan<- directV9ConcurrentDirBatch) {
	f, err := os.Open(root)
	if err != nil {
		sendDirectV9DirBatch(s.done, results, directV9ConcurrentDirBatch{root: root, done: true, err: err})
		return
	}
	defer f.Close()
	for {
		entries, readErr := f.ReadDir(256)
		if len(entries) > 0 {
			if !sendDirectV9DirBatch(s.done, results, directV9ConcurrentDirBatch{root: root, items: entries}) {
				return
			}
		}
		if errors.Is(readErr, io.EOF) || len(entries) == 0 {
			sendDirectV9DirBatch(s.done, results, directV9ConcurrentDirBatch{root: root, done: true, err: nil})
			return
		}
		if readErr != nil {
			sendDirectV9DirBatch(s.done, results, directV9ConcurrentDirBatch{root: root, done: true, err: readErr})
			return
		}
	}
}

func sendDirectV9DirBatch(done <-chan struct{}, results chan<- directV9ConcurrentDirBatch, batch directV9ConcurrentDirBatch) bool {
	select {
	case results <- batch:
		return true
	case <-done:
		return false
	}
}

func (s *directV9ConcurrentWalkSource) worker() {
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

func (s *directV9ConcurrentWalkSource) emit(job directV9ConcurrentWalkJob) {
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
		parent = directV9StablePathID(filepath.Dir(clean))
	}
	record := directV9Record{
		FRN:       directV9StablePathID(clean),
		ParentFRN: parent,
		Mode:      uint32(info.Mode()),
		Size:      info.Size(),
		ModUnix:   directV9WalkModUnix(info),
		Name:      job.entry.Name(),
		Path:      clean,
	}
	select {
	case s.records <- record:
	case <-s.done:
	}
}

func (s *directV9ConcurrentWalkSource) note(kind, path string, incomplete bool) {
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

func (s *directV9ConcurrentWalkSource) setErr(err error) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

func (s *directV9ConcurrentWalkSource) sourceErr() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *directV9ConcurrentWalkSource) Next(ctx context.Context) (directV9Record, error) {
	select {
	case <-s.done:
		return directV9Record{}, io.EOF
	default:
	}
	select {
	case <-ctx.Done():
		return directV9Record{}, ctx.Err()
	case <-s.done:
		return directV9Record{}, io.EOF
	case record, ok := <-s.records:
		if ok {
			return record, nil
		}
		if err := s.sourceErr(); err != nil {
			return directV9Record{}, err
		}
		return directV9Record{}, io.EOF
	}
}

func (s *directV9ConcurrentWalkSource) Close() {
	s.once.Do(func() { close(s.done) })
	<-s.finish
}
