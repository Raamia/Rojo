package orchestration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/workspace"
)

type recQueue struct {
	mu       sync.Mutex
	enqueued []string
	err      error
}

func (q *recQueue) Enqueue(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return q.err
	}
	q.enqueued = append(q.enqueued, id)
	return nil
}

func (q *recQueue) ids() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.enqueued...)
}

func seedJob(t *testing.T, repo jobs.JobRepository, id string, status jobs.JobStatus) {
	t.Helper()
	job := &jobs.Job{
		ID: id, Task: "a task for " + id, RepoPath: "/tmp/repo",
		Status: status, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := repo.Create(context.Background(), job); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func newRecoverer(repo jobs.JobRepository, q Enqueuer, ws workspace.WorkspaceManager) *Recoverer {
	return &Recoverer{
		Repo: repo, Queue: q, Bus: events.NewInProcessBus(), Workspaces: ws,
		WorktreeBaseDir: "/tmp/rojo-recovery",
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// A client that got a 201 was told its job was accepted. If the process
// restarts before a worker picks it up, the channel is gone but the row is not
// — without recovery that job is silently never run.
func TestRecover_RequeuesQueuedJobs(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	for _, id := range []string{"q1", "q2", "q3"} {
		seedJob(t, repo, id, jobs.StatusQueued)
	}
	q := &recQueue{}

	report, err := newRecoverer(repo, q, nil).Recover(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.Requeued != 3 {
		t.Errorf("requeued %d, want 3", report.Requeued)
	}
	if got := len(q.ids()); got != 3 {
		t.Errorf("enqueued %d ids, want 3: %v", got, q.ids())
	}

	// They stay queued: recovery hands them back to the workers, it does not
	// advance their state itself.
	for _, id := range q.ids() {
		got, _ := repo.Get(context.Background(), id)
		if got.Status != jobs.StatusQueued {
			t.Errorf("job %s status = %q, want queued", id, got.Status)
		}
	}
}

// A job that was mid-flight cannot simply be re-enqueued: Process starts by
// transitioning to planning, which is illegal from implementing or verifying.
func TestRecover_InterruptedJobsAreFailedNotRequeued(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	interrupted := []jobs.JobStatus{
		jobs.StatusPlanning,
		jobs.StatusPreparingWorkspace,
		jobs.StatusImplementing,
		jobs.StatusVerifying,
		jobs.StatusReviewing,
		jobs.StatusWaitingForRevision,
	}
	for i, st := range interrupted {
		seedJob(t, repo, string(st), st)
		_ = i
	}
	q := &recQueue{}

	report, err := newRecoverer(repo, q, nil).Recover(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.FailedInterrupted != len(interrupted) {
		t.Errorf("failed %d interrupted jobs, want %d", report.FailedInterrupted, len(interrupted))
	}
	if got := q.ids(); len(got) != 0 {
		t.Errorf("interrupted jobs must not be re-enqueued, got %v", got)
	}
	for _, st := range interrupted {
		got, _ := repo.Get(context.Background(), string(st))
		if got.Status != jobs.StatusFailed {
			t.Errorf("job interrupted in %q ended %q, want failed", st, got.Status)
		}
	}
}

// Terminal jobs are history; recovery must not touch them.
func TestRecover_LeavesTerminalJobsAlone(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	terminal := []jobs.JobStatus{jobs.StatusCompleted, jobs.StatusFailed, jobs.StatusCancelled}
	for _, st := range terminal {
		seedJob(t, repo, string(st), st)
	}
	q := &recQueue{}

	report, err := newRecoverer(repo, q, nil).Recover(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !report.IsZero() {
		t.Errorf("terminal jobs should need no recovery, got %+v", report)
	}
	for _, st := range terminal {
		got, _ := repo.Get(context.Background(), string(st))
		if got.Status != st {
			t.Errorf("terminal job %q was changed to %q", st, got.Status)
		}
	}
}

// A crashed job left a worktree behind. Nothing about it was persisted, but its
// path and branch derive from the job ID, so recovery can still name it.
func TestRecover_ReclaimsWorktreesOfInterruptedJobs(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	seedJob(t, repo, "crashed", jobs.StatusImplementing)
	ws := &fakeWorkspaces{}

	report, err := newRecoverer(repo, &recQueue{}, ws).Recover(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.WorktreesReclaimed != 1 {
		t.Errorf("reclaimed %d worktrees, want 1", report.WorktreesReclaimed)
	}
	if _, cleanup := ws.counts(); cleanup != 1 {
		t.Errorf("Cleanup called %d times, want 1", cleanup)
	}
}

// Reconstruct has to name exactly what the manager would have created,
// otherwise recovery reclaims nothing.
func TestReconstruct_MatchesWhatCreateWouldHaveMade(t *testing.T) {
	got := workspace.Reconstruct("/base", "job-42", "/src/repo")
	if want := filepath.Join("/base", "job-42"); got.Path != want {
		t.Errorf("path = %q, want %q", got.Path, want)
	}
	if want := "rojo/job/job-42"; got.Branch != want {
		t.Errorf("branch = %q, want %q", got.Branch, want)
	}
	if got.JobID != "job-42" || got.RepoPath != "/src/repo" {
		t.Errorf("unexpected workspace %+v", got)
	}
}

// A full queue must not lose or wrongly fail the job: leaving it queued keeps
// it recoverable on the next start.
func TestRecover_UnqueueableJobsStayQueued(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	seedJob(t, repo, "q1", jobs.StatusQueued)
	q := &recQueue{err: errors.New("queue is full")}

	report, err := newRecoverer(repo, q, nil).Recover(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.Unqueueable != 1 || report.Requeued != 0 {
		t.Errorf("report = %+v, want 1 unqueueable and 0 requeued", report)
	}
	got, _ := repo.Get(context.Background(), "q1")
	if got.Status != jobs.StatusQueued {
		t.Errorf("status = %q, want it left queued for the next attempt", got.Status)
	}
}

// A mixed database is the realistic case.
func TestRecover_MixedStateDatabase(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	seedJob(t, repo, "queued-1", jobs.StatusQueued)
	seedJob(t, repo, "queued-2", jobs.StatusQueued)
	seedJob(t, repo, "running", jobs.StatusVerifying)
	seedJob(t, repo, "done", jobs.StatusCompleted)
	seedJob(t, repo, "already-failed", jobs.StatusFailed)
	q := &recQueue{}

	report, err := newRecoverer(repo, q, &fakeWorkspaces{}).Recover(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.Requeued != 2 {
		t.Errorf("requeued %d, want 2", report.Requeued)
	}
	if report.FailedInterrupted != 1 {
		t.Errorf("failed %d, want 1", report.FailedInterrupted)
	}
	if got, _ := repo.Get(context.Background(), "done"); got.Status != jobs.StatusCompleted {
		t.Errorf("completed job became %q", got.Status)
	}
}

// Recovery is safe to run repeatedly: the second pass has nothing left to do.
func TestRecover_IsIdempotentAcrossRuns(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	seedJob(t, repo, "running", jobs.StatusImplementing)
	q := &recQueue{}
	r := newRecoverer(repo, q, &fakeWorkspaces{})

	first, err := r.Recover(context.Background())
	if err != nil {
		t.Fatalf("first recover: %v", err)
	}
	if first.FailedInterrupted != 1 {
		t.Fatalf("first pass failed %d jobs, want 1", first.FailedInterrupted)
	}

	second, err := r.Recover(context.Background())
	if err != nil {
		t.Fatalf("second recover: %v", err)
	}
	if !second.IsZero() {
		t.Errorf("second pass should be a no-op, got %+v", second)
	}
}

// A repository that cannot be read is a real startup problem, but one job that
// cannot be recovered must not abort the whole pass.
func TestRecover_ListFailureIsReported(t *testing.T) {
	boom := errors.New("connection refused")
	r := newRecoverer(brokenListRepo{err: boom}, &recQueue{}, nil)
	if _, err := r.Recover(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("got %v, want the underlying cause", err)
	}
}

type brokenListRepo struct {
	jobs.JobRepository
	err error
}

func (b brokenListRepo) List(context.Context) ([]*jobs.Job, error) { return nil, b.err }
