package tests

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/execution"
	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/orchestration"
	"github.com/Raamia/Rojo/internal/queue"
	"github.com/Raamia/Rojo/internal/worker"
	"github.com/Raamia/Rojo/internal/workspace"
)

// A restart replaces the process — a fresh queue, a fresh worker pool — while
// the repository survives. These tests model that by building a new "run"
// around a repository that outlives it, which is exactly the split that made
// accepted work disappear: the channel holding pending job IDs is gone, the
// rows saying those jobs were accepted are not.
//
// With ROJO_DB_URL unset the repository is in-memory and dies with the process,
// so recovery is a no-op in that mode; these tests exercise the mechanism the
// postgres deployment relies on.

type run struct {
	queue *queue.Queue
	pool  *worker.Pool
	stop  context.CancelFunc
}

func startRun(t *testing.T, repo jobs.JobRepository, baseDir string) *run {
	t.Helper()

	q := queue.New(64)
	canceller := orchestration.NewCanceller()
	bus := events.NewInProcessBus()
	manager := workspace.NewGitWorkspaceManager(
		execution.NewSafeRunner(execution.NewExecRunner(), execution.NewAllowlist("git"), time.Minute),
		baseDir,
	)
	processor := orchestration.NewProcessor(repo, canceller, bus, manager, nil)
	pool := worker.NewPool(2, q, processor)

	ctx, cancel := context.WithCancel(context.Background())
	pool.Start(ctx)
	return &run{queue: q, pool: pool, stop: cancel}
}

// shutdown ends the run the way main.go does.
func (r *run) shutdown() {
	r.stop()
	r.queue.Close()
	r.pool.Wait()
}

func recoverInto(t *testing.T, repo jobs.JobRepository, r *run, baseDir string) orchestration.RecoveryReport {
	t.Helper()
	rec := &orchestration.Recoverer{
		Repo:  repo,
		Queue: r.queue,
		Bus:   events.NewInProcessBus(),
		Workspaces: workspace.NewGitWorkspaceManager(
			execution.NewSafeRunner(execution.NewExecRunner(), execution.NewAllowlist("git"), time.Minute),
			baseDir,
		),
		WorktreeBaseDir: baseDir,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	report, err := rec.Recover(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	return report
}

func waitForStatus(t *testing.T, repo jobs.JobRepository, id string, want jobs.JobStatus, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	var last jobs.JobStatus
	for time.Now().Before(deadline) {
		got, err := repo.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		last = got.Status
		if last == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s is %q after %s, want %q", id, last, within, want)
}

// Jobs accepted but never picked up must survive a restart. Before recovery
// existed these stayed `queued` forever: the client had a 201 for work that
// would never run.
func TestRestart_AcceptedButUnstartedJobsAreRecovered(t *testing.T) {
	hasGit(t)
	repoPath := initTestRepo(t)
	store := jobs.NewInMemoryRepository()
	baseDir := t.TempDir()

	// First run: accept three jobs, then die before any worker claims them.
	// Enqueueing is deliberately skipped to model IDs lost with the channel.
	ids := []string{"lost-1", "lost-2", "lost-3"}
	for _, id := range ids {
		job := &jobs.Job{
			ID: id, Task: "work that outlived its process", RepoPath: repoPath,
			Status: jobs.StatusQueued, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := store.Create(context.Background(), job); err != nil {
			t.Fatal(err)
		}
	}

	// Second run: fresh queue and pool, then recovery.
	second := startRun(t, store, baseDir)
	defer second.shutdown()

	report := recoverInto(t, store, second, baseDir)
	if report.Requeued != len(ids) {
		t.Fatalf("requeued %d, want %d", report.Requeued, len(ids))
	}

	// Recovery is only worth anything if the jobs actually run to completion.
	for _, id := range ids {
		waitForStatus(t, store, id, jobs.StatusCompleted, 10*time.Second)
	}
}

// A job that was mid-flight when the process died cannot be resumed: Process
// begins by transitioning to planning, which is illegal from implementing. It
// has to reach a terminal state instead of sitting non-terminal forever.
func TestRestart_InterruptedJobReachesATerminalState(t *testing.T) {
	hasGit(t)
	repoPath := initTestRepo(t)
	store := jobs.NewInMemoryRepository()
	baseDir := t.TempDir()

	job := &jobs.Job{
		ID: "died-mid-flight", Task: "interrupted work", RepoPath: repoPath,
		Status: jobs.StatusImplementing, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := store.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}

	// The crashed run had already created its worktree.
	orphan := filepath.Join(baseDir, "died-mid-flight")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}

	second := startRun(t, store, baseDir)
	defer second.shutdown()

	report := recoverInto(t, store, second, baseDir)
	if report.FailedInterrupted != 1 {
		t.Fatalf("failed %d interrupted jobs, want 1", report.FailedInterrupted)
	}

	got, err := store.Get(context.Background(), "died-mid-flight")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != jobs.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}

	// The worktree the dead process left behind must be reclaimed. Nothing
	// about it was persisted — its path is derived from the job ID.
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphaned worktree survived recovery: %v", err)
	}
}

// Recovery must not disturb work that already finished.
func TestRestart_TerminalJobsAreUntouched(t *testing.T) {
	hasGit(t)
	repoPath := initTestRepo(t)
	store := jobs.NewInMemoryRepository()
	baseDir := t.TempDir()

	for _, st := range []jobs.JobStatus{jobs.StatusCompleted, jobs.StatusFailed, jobs.StatusCancelled} {
		job := &jobs.Job{
			ID: string(st), Task: "already finished", RepoPath: repoPath,
			Status: st, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := store.Create(context.Background(), job); err != nil {
			t.Fatal(err)
		}
	}

	second := startRun(t, store, baseDir)
	defer second.shutdown()

	if report := recoverInto(t, store, second, baseDir); !report.IsZero() {
		t.Errorf("terminal jobs needed recovery: %+v", report)
	}
	for _, st := range []jobs.JobStatus{jobs.StatusCompleted, jobs.StatusFailed, jobs.StatusCancelled} {
		got, _ := store.Get(context.Background(), string(st))
		if got.Status != st {
			t.Errorf("job %q became %q", st, got.Status)
		}
	}
}
