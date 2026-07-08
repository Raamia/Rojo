package filestore

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
)

// The store has to satisfy both interfaces the rest of the codebase depends on.
var (
	_ jobs.JobRepository = (*Store)(nil)
	_ events.Store       = (*Store)(nil)
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s, dir
}

func newJob(id string) *jobs.Job {
	return &jobs.Job{
		ID: id, Task: "task for " + id, RepoPath: "/tmp/repo",
		Status: jobs.StatusQueued, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
}

func TestStore_CreateGetUpdate(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()

	if err := s.Create(ctx, newJob("j1")); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.Get(ctx, "j1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Task != "task for j1" || got.Status != jobs.StatusQueued {
		t.Fatalf("unexpected job %+v", got)
	}

	got.Status = jobs.StatusPlanning
	if err := s.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	again, _ := s.Get(ctx, "j1")
	if again.Status != jobs.StatusPlanning {
		t.Errorf("status = %q, want planning", again.Status)
	}
}

func TestStore_SentinelErrors(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()

	if _, err := s.Get(ctx, "missing"); !errors.Is(err, jobs.ErrJobNotFound) {
		t.Errorf("get missing = %v, want ErrJobNotFound", err)
	}
	if err := s.Update(ctx, newJob("missing")); !errors.Is(err, jobs.ErrJobNotFound) {
		t.Errorf("update missing = %v, want ErrJobNotFound", err)
	}
	if err := s.Create(ctx, newJob("dup")); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, newJob("dup")); !errors.Is(err, jobs.ErrJobAlreadyExists) {
		t.Errorf("duplicate create = %v, want ErrJobAlreadyExists", err)
	}
}

// The returned job must be a copy: a caller mutating it must not silently
// change stored state without going through Update.
func TestStore_ReturnsDefensiveCopies(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, newJob("j1")); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Get(ctx, "j1")
	got.Status = jobs.StatusFailed
	got.Task = "mutated"

	fresh, _ := s.Get(ctx, "j1")
	if fresh.Status == jobs.StatusFailed || fresh.Task == "mutated" {
		t.Error("mutating a returned job changed stored state")
	}
}

// The whole point of the store: state has to survive the process.
func TestStore_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	first, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := first.Create(ctx, newJob(id)); err != nil {
			t.Fatal(err)
		}
	}
	running := newJob("b")
	running.Status = jobs.StatusImplementing
	if err := first.Update(ctx, running); err != nil {
		t.Fatal(err)
	}

	// A new process, same directory. The old process is gone — which the
	// single-writer lock insists on — so its store is closed first.
	first.Close()
	second, err := New(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if second.Loaded() != 3 {
		t.Fatalf("loaded %d jobs, want 3", second.Loaded())
	}
	got, err := second.Get(ctx, "b")
	if err != nil {
		t.Fatalf("get after restart: %v", err)
	}
	if got.Status != jobs.StatusImplementing {
		t.Errorf("status = %q, want the persisted implementing", got.Status)
	}
}

func TestStore_ListIsNewestFirst(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()

	base := time.Now().UTC()
	for i, id := range []string{"old", "mid", "new"} {
		j := newJob(id)
		j.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		if err := s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("got %d jobs, want 3", len(list))
	}
	if list[0].ID != "new" || list[2].ID != "old" {
		t.Errorf("order = %s,%s,%s, want new,mid,old", list[0].ID, list[1].ID, list[2].ID)
	}
}

func TestStore_EventsAppendAndReadInOrder(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, newJob("j1")); err != nil {
		t.Fatal(err)
	}

	for _, typ := range []string{events.TypeJobStarted, events.TypePlanCreated, events.TypeJobCompleted} {
		if err := s.Append(ctx, events.Event{JobID: "j1", Type: typ, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("append %s: %v", typ, err)
		}
	}

	got, err := s.History(ctx, "j1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3", len(got))
	}
	if got[0].Type != events.TypeJobStarted || got[2].Type != events.TypeJobCompleted {
		t.Errorf("events out of order: %v", []string{got[0].Type, got[1].Type, got[2].Type})
	}
}

// A job that has emitted nothing has an empty history, not an error.
func TestStore_HistoryOfSilentJobIsEmpty(t *testing.T) {
	s, _ := newStore(t)
	got, err := s.History(context.Background(), "never-emitted")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d events, want none", len(got))
	}
}

// Payloads carry plans and verification reports, so they must round-trip.
func TestStore_EventPayloadsRoundTrip(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	payload := map[string]any{"summary": "2 of 3 checks failed", "passed": false}
	if err := s.Append(ctx, events.Event{JobID: "j1", Type: events.TypeVerificationCompleted, Payload: payload}); err != nil {
		t.Fatal(err)
	}

	got, _ := s.History(ctx, "j1")
	if len(got) != 1 {
		t.Fatalf("got %d events", len(got))
	}
	if got[0].Payload["summary"] != "2 of 3 checks failed" || got[0].Payload["passed"] != false {
		t.Errorf("payload did not round-trip: %+v", got[0].Payload)
	}
}

// A crash during an append leaves a truncated final line. That must cost one
// event, not the entire history.
func TestStore_TruncatedEventLineIsSkipped(t *testing.T) {
	s, dir := newStore(t)
	ctx := context.Background()
	if err := s.Append(ctx, events.Event{JobID: "j1", Type: events.TypeJobStarted}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, jobsDir, "j1", eventsFile)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"job_id":"j1","type":"job.compl`) // torn write
	f.Close()

	got, err := s.History(ctx, "j1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(got) != 1 || got[0].Type != events.TypeJobStarted {
		t.Errorf("got %+v, want the one complete event", got)
	}
}

func TestStore_Artifacts(t *testing.T) {
	s, dir := newStore(t)
	if err := s.WriteArtifact("j1", "diff.patch", []byte("--- a/main.go\n+++ b/main.go\n")); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	got, err := s.ReadArtifact("j1", "diff.patch")
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if !strings.Contains(string(got), "main.go") {
		t.Errorf("artifact = %q", got)
	}
	// A patch on disk is a patch — usable without going through the API.
	onDisk := filepath.Join(dir, jobsDir, "j1", "diff.patch")
	if _, err := os.Stat(onDisk); err != nil {
		t.Errorf("artifact should be a plain file at %s: %v", onDisk, err)
	}
}

// The API distinguishes "this job produced no patch yet" from "reading it
// failed" by testing errors.Is(err, fs.ErrNotExist). That branch is only
// correct if a missing artifact actually reports itself that way, so the
// contract is pinned here rather than left to os.ReadFile's implementation.
func TestStore_MissingArtifactIsNotExist(t *testing.T) {
	s, _ := newStore(t)

	for _, tt := range []struct{ name, job, artifact string }{
		{"no artifact for a known job", "j1", "patch.diff"},
		{"no job at all", "00000000000000000000000000000000", "patch.diff"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := s.WriteArtifact("j1", "other.txt", []byte("x")); err != nil {
				t.Fatal(err)
			}
			_, err := s.ReadArtifact(tt.job, tt.artifact)
			if !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("got %v, want an fs.ErrNotExist-compatible error", err)
			}
		})
	}
}

// A patch is the largest thing a job produces and the one most likely to be
// round-tripped wrong. Bytes in must equal bytes out, including the trailing
// newline git needs for `git apply` to accept the last hunk.
func TestStore_ArtifactRoundTripsExactly(t *testing.T) {
	s, _ := newStore(t)
	patch := "diff --git a/greet.go b/greet.go\nnew file mode 100644\n" +
		"--- /dev/null\n+++ b/greet.go\n@@ -0,0 +1 @@\n+package main\n"

	if err := s.WriteArtifact("j1", "patch.diff", []byte(patch)); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadArtifact("j1", "patch.diff")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != patch {
		t.Errorf("round trip altered the patch:\n got %q\nwant %q", got, patch)
	}
}

// Ids and artifact names reach a filesystem path, so traversal must be refused
// even though ids are generated from crypto/rand.
func TestStore_RejectsPathTraversal(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()

	for _, id := range []string{"../escape", "a/b", `a\b`, "", "with.dot", "nul\x00id"} {
		t.Run("id "+id, func(t *testing.T) {
			if err := s.Create(ctx, newJob(id)); !errors.Is(err, ErrInvalidJobID) {
				t.Errorf("create %q = %v, want ErrInvalidJobID", id, err)
			}
			if err := s.Append(ctx, events.Event{JobID: id}); !errors.Is(err, ErrInvalidJobID) {
				t.Errorf("append %q = %v, want ErrInvalidJobID", id, err)
			}
		})
	}
	for _, name := range []string{"../escape", "a/b", "..", ""} {
		if err := s.WriteArtifact("j1", name, []byte("x")); err == nil {
			t.Errorf("artifact name %q was accepted", name)
		}
	}
}

// job.json is replaced via a temp file and rename, so a reader never sees a
// half-written record and no temp files are left behind.
func TestStore_WritesAreAtomic(t *testing.T) {
	s, dir := newStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, newJob("j1")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		j, _ := s.Get(ctx, "j1")
		j.Status = jobs.StatusPlanning
		if err := s.Update(ctx, j); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(dir, jobsDir, "j1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}

	// The committed file must always be complete JSON.
	b, err := os.ReadFile(filepath.Join(dir, jobsDir, "j1", jobFile))
	if err != nil {
		t.Fatal(err)
	}
	var parsed jobs.Job
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("job.json is not valid JSON: %v", err)
	}
}

// A corrupt job directory must not stop the service from serving the rest.
func TestStore_CorruptJobIsSkippedAtStartup(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, newJob("good")); err != nil {
		t.Fatal(err)
	}

	bad := filepath.Join(dir, jobsDir, "bad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, jobFile), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	s.Close() // the "crashed" process's lock is gone with it
	reopened, err := New(dir)
	if err != nil {
		t.Fatalf("a corrupt job should not fail startup: %v", err)
	}
	if reopened.Loaded() != 1 {
		t.Errorf("loaded %d, want just the good job", reopened.Loaded())
	}
	if _, err := reopened.Get(ctx, "good"); err != nil {
		t.Errorf("the readable job should still be served: %v", err)
	}
}

func TestStore_ConcurrentAccess(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('a' + i))
			if err := s.Create(ctx, newJob(id)); err != nil {
				t.Errorf("create %s: %v", id, err)
				return
			}
			for k := 0; k < 5; k++ {
				j, err := s.Get(ctx, id)
				if err != nil {
					t.Errorf("get %s: %v", id, err)
					return
				}
				j.Status = jobs.StatusPlanning
				if err := s.Update(ctx, j); err != nil {
					t.Errorf("update %s: %v", id, err)
					return
				}
				_ = s.Append(ctx, events.Event{JobID: id, Type: events.TypeStepCompleted})
			}
		}(i)
	}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 10; k++ {
				if _, err := s.List(ctx); err != nil {
					t.Errorf("list: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	list, _ := s.List(ctx)
	if len(list) != 20 {
		t.Errorf("got %d jobs, want 20", len(list))
	}
}

// Two processes on one data directory would rebuild independent in-memory
// indexes and overwrite job.json behind each other's backs — silent corruption
// where every individual write succeeds. The classic cause is a second
// `make run` in another terminal, and it has to fail at startup with a clear
// message, not corrupt quietly. flock scopes per open file description, so a
// second open in the same process conflicts too, which is what makes this
// testable without spawning a child.
func TestStore_SecondOpenOfTheSameDataDirIsRefused(t *testing.T) {
	dir := t.TempDir()
	first, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := New(dir); err == nil {
		t.Fatal("a second store opened the same data dir")
	} else if !strings.Contains(err.Error(), "already in use") {
		t.Errorf("error %q should say the dir is in use and how to fix it", err)
	}

	// Close hands the directory over; a successor must be able to start.
	first.Close()
	second, err := New(dir)
	if err != nil {
		t.Fatalf("reopen after Close: %v", err)
	}
	second.Close()
}

// A store that answers reads perfectly while every write fails is exactly the
// outage a health check exists to catch: the process looks fine and every job
// fails to persist.
func TestStore_HealthDetectsAnUnwritableDataDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions do not restrict writes")
	}
	s, dir := newStore(t)
	if err := s.Health(); err != nil {
		t.Fatalf("a fresh store should be healthy: %v", err)
	}

	if err := os.Chmod(dir, 0o500); err != nil { // read+execute, no write
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	err := s.Health()
	if err == nil {
		t.Fatal("a read-only data dir reported healthy")
	}
	// /healthz is unauthenticated, so the message must not hand out the
	// deployment's filesystem layout. The detail belongs in the log.
	if strings.Contains(err.Error(), dir) {
		t.Errorf("health error leaks the data dir path: %v", err)
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Health(); err != nil {
		t.Errorf("should recover once writable again: %v", err)
	}
}
