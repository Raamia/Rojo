// Package filestore persists jobs, their event history, and their artifacts as
// plain files on disk.
//
// What Rojo stores is a key-value map of jobs plus an append-only log per job —
// no joins, no aggregations, no queries. A directory per job covers that with
// the standard library alone, and it makes the data inspectable with ls and
// cat, which matters more at 3am than query flexibility this system never uses.
//
// Layout:
//
//	<dir>/jobs/<job-id>/job.json      current state, atomically replaced
//	<dir>/jobs/<job-id>/events.jsonl  append-only, one JSON object per line
//	<dir>/jobs/<job-id>/<artifact>    plan.json, report.json, diff.patch, ...
//
// Reads are served from an in-memory index built at startup, so listing and
// lookups cost no I/O. The store assumes a single process — the documented
// deployment model — and guards that index with a mutex.
package filestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
)

const (
	jobsDir    = "jobs"
	jobFile    = "job.json"
	eventsFile = "events.jsonl"
)

// ErrInvalidJobID rejects an id that would escape the data directory. Job ids
// are generated from crypto/rand hex so this should be unreachable, but the id
// reaches a filesystem path and defence in depth is cheap.
var ErrInvalidJobID = errors.New("invalid job id")

type Store struct {
	dir string
	// unlock releases the data directory's single-writer lock; see Close.
	unlock func()

	mu    sync.RWMutex
	index map[string]*jobs.Job
}

// New opens (creating if needed) a store rooted at dir and loads every job it
// finds into memory. A job directory that cannot be read is skipped rather than
// failing startup: one corrupt job must not stop the service from serving the
// rest.
func New(dir string) (*Store, error) {
	root := filepath.Join(dir, jobsDir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", root, err)
	}

	// Exclusive ownership before anything is read: an index built while
	// another process is writing is already wrong.
	unlock, err := acquireLock(dir)
	if err != nil {
		return nil, err
	}

	s := &Store{dir: dir, unlock: unlock, index: make(map[string]*jobs.Job)}
	entries, err := os.ReadDir(root)
	if err != nil {
		unlock()
		return nil, fmt.Errorf("read data dir %s: %w", root, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		job, err := s.readJob(e.Name())
		if err != nil {
			continue
		}
		s.index[job.ID] = job
	}
	return s, nil
}

// Close releases the data directory so another process may take it over. The
// kernel would release the lock at exit anyway; calling this makes handover
// immediate rather than whenever the OS gets around to it.
func (s *Store) Close() {
	if s.unlock != nil {
		s.unlock()
	}
}

// Loaded reports how many jobs were recovered from disk at startup.
func (s *Store) Loaded() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.index)
}

func validJobID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	// Anything that is not a plain identifier could traverse or collide.
	return !strings.ContainsAny(id, `/\.`) && !strings.ContainsRune(id, 0)
}

func (s *Store) jobDir(id string) string { return filepath.Join(s.dir, jobsDir, id) }

func (s *Store) readJob(id string) (*jobs.Job, error) {
	b, err := os.ReadFile(filepath.Join(s.jobDir(id), jobFile))
	if err != nil {
		return nil, err
	}
	var job jobs.Job
	if err := json.Unmarshal(b, &job); err != nil {
		return nil, fmt.Errorf("parse job %s: %w", id, err)
	}
	return &job, nil
}

// writeJob replaces job.json atomically.
//
// The temp file is fsynced before the rename so a crash cannot leave a
// half-written record: rename is atomic on POSIX, but only guarantees you see
// either the old file or a *durable* new one if the new one hit the disk first.
// This is the recovery-critical write, so it is worth the sync.
func (s *Store) writeJob(job *jobs.Job) error {
	dir := s.jobDir(job.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create job dir: %w", err)
	}

	b, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return fmt.Errorf("encode job %s: %w", job.ID, err)
	}

	tmp, err := os.CreateTemp(dir, jobFile+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp job file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write job %s: %w", job.ID, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync job %s: %w", job.ID, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close job %s: %w", job.ID, err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, jobFile)); err != nil {
		return fmt.Errorf("commit job %s: %w", job.ID, err)
	}
	return nil
}

func cloneJob(j *jobs.Job) *jobs.Job {
	c := *j
	return &c
}

// --- jobs.JobRepository ---

func (s *Store) Create(_ context.Context, job *jobs.Job) error {
	if job == nil {
		return errors.New("create: nil job")
	}
	if !validJobID(job.ID) {
		return fmt.Errorf("%w: %q", ErrInvalidJobID, job.ID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.index[job.ID]; exists {
		return jobs.ErrJobAlreadyExists
	}
	if err := s.writeJob(job); err != nil {
		return err
	}
	// Store a copy so a later mutation by the caller cannot change what is
	// indexed without also going through Update.
	s.index[job.ID] = cloneJob(job)
	return nil
}

func (s *Store) Get(_ context.Context, id string) (*jobs.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.index[id]
	if !ok {
		return nil, jobs.ErrJobNotFound
	}
	return cloneJob(job), nil
}

func (s *Store) Update(_ context.Context, job *jobs.Job) error {
	if job == nil {
		return errors.New("update: nil job")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.index[job.ID]; !ok {
		return jobs.ErrJobNotFound
	}
	if err := s.writeJob(job); err != nil {
		return err
	}
	s.index[job.ID] = cloneJob(job)
	return nil
}

// List returns every job, newest first — the order the API has always used.
func (s *Store) List(_ context.Context) ([]*jobs.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*jobs.Job, 0, len(s.index))
	for _, j := range s.index {
		out = append(out, cloneJob(j))
	}
	sort.Slice(out, func(i, k int) bool {
		if out[i].CreatedAt.Equal(out[k].CreatedAt) {
			return out[i].ID < out[k].ID // stable tie-break
		}
		return out[i].CreatedAt.After(out[k].CreatedAt)
	})
	return out, nil
}

// --- events.Store ---

// Append writes one event to the job's log.
//
// Unlike job.json this is not fsynced: events are an observability record, the
// write happens on the job's hot path, and losing the last line to a hard crash
// costs a log entry rather than correctness. Job state, which recovery depends
// on, is the durable one.
func (s *Store) Append(_ context.Context, e events.Event) error {
	if !validJobID(e.JobID) {
		return fmt.Errorf("%w: %q", ErrInvalidJobID, e.JobID)
	}
	dir := s.jobDir(e.JobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create job dir: %w", err)
	}

	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(dir, eventsFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open event log: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}

// History returns a job's events in the order they were appended. A job with no
// events yet returns an empty slice rather than an error — that is a job that
// has not emitted anything, not a missing one.
func (s *Store) History(_ context.Context, jobID string) ([]events.Event, error) {
	if !validJobID(jobID) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidJobID, jobID)
	}
	b, err := os.ReadFile(filepath.Join(s.jobDir(jobID), eventsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return []events.Event{}, nil
		}
		return nil, fmt.Errorf("read event log for %s: %w", jobID, err)
	}

	out := []events.Event{}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e events.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			// A truncated final line is the expected shape of a crash during
			// append; skip it rather than failing the whole history.
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// --- artifacts ---

// WriteArtifact stores a job's output — a plan, a verification report, a diff.
// Artifacts are files because that is what they are: a patch written here can
// be fed straight to `git apply`.
func (s *Store) WriteArtifact(jobID, name string, data []byte) error {
	if !validJobID(jobID) {
		return fmt.Errorf("%w: %q", ErrInvalidJobID, jobID)
	}
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return fmt.Errorf("invalid artifact name %q", name)
	}
	dir := s.jobDir(jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create job dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		return fmt.Errorf("write artifact %s for %s: %w", name, jobID, err)
	}
	return nil
}

// ReadArtifact returns a previously written artifact.
func (s *Store) ReadArtifact(jobID, name string) ([]byte, error) {
	if !validJobID(jobID) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidJobID, jobID)
	}
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return nil, fmt.Errorf("invalid artifact name %q", name)
	}
	b, err := os.ReadFile(filepath.Join(s.jobDir(jobID), name))
	if err != nil {
		return nil, err
	}
	return b, nil
}

// Health reports whether the store can still do the one thing a job needs from
// it: write. A data directory that has gone read-only or run out of space stays
// perfectly readable, so a check that only stats the directory would report
// healthy while every job fails to persist.
//
// The probe writes and removes a small file rather than reusing WriteArtifact,
// so it cannot collide with a real job's artifacts.
//
// The returned errors deliberately name the condition and not the path. This
// reaches an unauthenticated /healthz, and the data directory layout is not
// something to hand to anyone who can reach the port; the full error, path
// included, goes to the log where an operator will look for it.
func (s *Store) Health() error {
	f, err := os.CreateTemp(s.dir, ".health*")
	if err != nil {
		slog.Error("health probe: create file in data dir", "dir", s.dir, "err", err)
		return errors.New("data dir is not writable")
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.WriteString("ok"); err != nil {
		f.Close()
		slog.Error("health probe: write to data dir", "dir", s.dir, "err", err)
		return errors.New("data dir is not writable")
	}
	if err := f.Close(); err != nil {
		slog.Error("health probe: close", "dir", s.dir, "err", err)
		return errors.New("data dir write could not be completed")
	}
	return nil
}
