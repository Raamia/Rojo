package orchestration

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/execution"
	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/workspace"
)

// fakeWorkspaces records how the processor drives the manager, including the
// liveness of the context cleanup is handed — the detail that decides whether
// cancelled jobs actually get cleaned up.
type fakeWorkspaces struct {
	mu sync.Mutex

	createErr    error
	createCalls  int
	cleanupCalls int
	cleanupErr   error

	cleanupCtxErr    error
	cleanupCtxWasSet bool
	created          *workspace.Workspace

	// afterCreate runs once the workspace exists, letting a test act inside the
	// exact window where a worktree is live and must not be leaked.
	afterCreate func()
}

func (f *fakeWorkspaces) Create(_ context.Context, jobID, repoPath string) (*workspace.Workspace, error) {
	f.mu.Lock()
	f.createCalls++
	if f.createErr != nil {
		err := f.createErr
		f.mu.Unlock()
		return nil, err
	}
	ws := &workspace.Workspace{
		JobID:    jobID,
		Branch:   "rojo/job/" + jobID,
		Path:     filepath.Join("/tmp/rojo-test", jobID),
		RepoPath: repoPath,
	}
	f.created = ws
	hook := f.afterCreate
	f.mu.Unlock()

	if hook != nil {
		hook()
	}
	return ws, nil
}

func (f *fakeWorkspaces) Cleanup(ctx context.Context, _ *workspace.Workspace) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanupCalls++
	f.cleanupCtxWasSet = true
	f.cleanupCtxErr = ctx.Err()
	return f.cleanupErr
}

func (f *fakeWorkspaces) Diff(context.Context, *workspace.Workspace) (string, error) { return "", nil }
func (f *fakeWorkspaces) ListOrphans(context.Context, string) ([]string, error)      { return nil, nil }

func (f *fakeWorkspaces) counts() (create, cleanup int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCalls, f.cleanupCalls
}

func TestProcessor_CreatesAndCleansUpWorkspaceOnSuccess(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	bus := events.NewInProcessBus()
	sub := bus.Subscribe("ws-job", 64)
	defer bus.Unsubscribe(sub)

	fake := &fakeWorkspaces{}
	newQueuedJob(t, repo, "ws-job")

	p := NewProcessor(repo, NewCanceller(), bus, fake)
	if err := p.Process(context.Background(), "ws-job"); err != nil {
		t.Fatalf("process: %v", err)
	}

	create, cleanup := fake.counts()
	if create != 1 {
		t.Errorf("Create called %d times, want 1", create)
	}
	if cleanup != 1 {
		t.Errorf("Cleanup called %d times, want 1 — a finished job must not leave a worktree behind", cleanup)
	}

	got, err := repo.Get(context.Background(), "ws-job")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != jobs.StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}

	counts := typesOf(drain(sub))
	if counts[events.TypeWorkspaceCreated] != 1 {
		t.Errorf("workspace.created emitted %d times, want 1", counts[events.TypeWorkspaceCreated])
	}
}

// Cancellation is the path where cleanup matters most and is easiest to get
// wrong, because the job's context is dead by the time the deferred call runs.
// Cancelling from inside Create lands the cancellation in exactly the window
// where a worktree is live — cancelling earlier would prove nothing, since
// there would be no workspace to clean up.
func TestProcessor_CleansUpWorkspaceOnCancellation(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	canc := NewCanceller()
	fake := &fakeWorkspaces{}
	fake.afterCreate = func() { _ = canc.Cancel("cancel-ws") }
	newQueuedJob(t, repo, "cancel-ws")

	p := NewProcessor(repo, canc, events.NewInProcessBus(), fake)

	done := make(chan error, 1)
	go func() { done <- p.Process(context.Background(), "cancel-ws") }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Process did not return after cancellation")
	}

	create, cleanup := fake.counts()
	if create != 1 {
		t.Fatalf("Create called %d times, want 1", create)
	}
	if cleanup != 1 {
		t.Errorf("Cleanup called %d times after cancellation, want 1 — "+
			"a cancelled job must not leak its worktree", cleanup)
	}

	got, err := repo.Get(context.Background(), "cancel-ws")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != jobs.StatusCancelled {
		t.Errorf("status = %q, want cancelled", got.Status)
	}
}

// The processor hands cleanup the job context, which by then is cancelled.
// GitWorkspaceManager.Cleanup strips that cancellation internally; this pins
// the contract so a future change cannot quietly reintroduce "cleanup is
// skipped exactly when the job was cancelled".
func TestProcessor_CleanupIsAttemptedEvenWhenContextIsDead(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	canc := NewCanceller()
	fake := &fakeWorkspaces{}
	fake.afterCreate = func() { _ = canc.Cancel("dead-ctx") }
	newQueuedJob(t, repo, "dead-ctx")

	p := NewProcessor(repo, canc, events.NewInProcessBus(), fake)

	done := make(chan error, 1)
	go func() { done <- p.Process(context.Background(), "dead-ctx") }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Process did not return")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !fake.cleanupCtxWasSet {
		t.Fatal("Cleanup was never called on a cancelled job")
	}
	if fake.cleanupCtxErr == nil {
		t.Log("cleanup context was still live")
	} else {
		t.Logf("cleanup ran despite ctx.Err() = %v", fake.cleanupCtxErr)
	}
}

// A workspace that cannot be created is a real failure, and the job has to end
// somewhere terminal rather than sitting in preparing_workspace forever.
func TestProcessor_WorkspaceCreateFailureMarksJobFailed(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	bus := events.NewInProcessBus()
	sub := bus.Subscribe("bad-repo", 64)
	defer bus.Unsubscribe(sub)

	boom := errors.New("repository path does not exist")
	fake := &fakeWorkspaces{createErr: boom}
	newQueuedJob(t, repo, "bad-repo")

	p := NewProcessor(repo, NewCanceller(), bus, fake)
	err := p.Process(context.Background(), "bad-repo")
	if err == nil {
		t.Fatal("expected an error when the workspace cannot be created")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error %v should wrap the underlying cause", err)
	}

	got, getErr := repo.Get(context.Background(), "bad-repo")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status != jobs.StatusFailed {
		t.Fatalf("status = %q, want failed — a job whose workspace failed must reach a terminal state", got.Status)
	}

	if counts := typesOf(drain(sub)); counts[events.TypeJobFailed] != 1 {
		t.Errorf("job.failed emitted %d times, want 1", counts[events.TypeJobFailed])
	}

	// Nothing was created, so nothing should be cleaned up.
	if _, cleanup := fake.counts(); cleanup != 0 {
		t.Errorf("Cleanup called %d times after a failed create, want 0", cleanup)
	}
}

// A nil manager keeps the pure state-machine behavior, so existing callers and
// tests are unaffected.
func TestProcessor_NilWorkspaceManagerStillCompletes(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	newQueuedJob(t, repo, "no-ws")

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus(), nil)
	if err := p.Process(context.Background(), "no-ws"); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, err := repo.Get(context.Background(), "no-ws")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != jobs.StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
}

// End to end against real git: the worktree must exist while the job runs and
// be gone once it finishes, leaving the original repository untouched.
func TestProcessor_RealGitWorktreeLifecycle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	repoDir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main", ".")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "initial")

	base := t.TempDir()
	manager := workspace.NewGitWorkspaceManager(
		execution.NewSafeRunner(execution.NewExecRunner(), execution.NewAllowlist("git"), time.Minute),
		base,
	)

	// Observe the worktree while the job is still in flight.
	var sawWorktree bool
	observer := &observingWorkspaces{inner: manager, onCreate: func(ws *workspace.Workspace) {
		if _, err := os.Stat(filepath.Join(ws.Path, "README.md")); err == nil {
			sawWorktree = true
		}
	}}

	repo := jobs.NewInMemoryRepository()
	job := &jobs.Job{
		ID: "real-git", Task: "do a thing", RepoPath: repoDir,
		Status: jobs.StatusQueued, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := repo.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus(), observer)
	if err := p.Process(context.Background(), "real-git"); err != nil {
		t.Fatalf("process: %v", err)
	}

	if !sawWorktree {
		t.Error("the worktree was never populated during the job")
	}
	if _, err := os.Stat(filepath.Join(base, "real-git")); !os.IsNotExist(err) {
		t.Errorf("worktree still present after the job finished: %v", err)
	}

	// The original repository must be untouched: no stray branch, no worktree.
	out, err := exec.Command("git", "-C", repoDir, "worktree", "list").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(splitLines(string(out))); n != 1 {
		t.Errorf("expected only the main worktree to remain, got:\n%s", out)
	}
	branches, err := exec.Command("git", "-C", repoDir, "branch", "--list", "rojo/job/*").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if len(splitLines(string(branches))) != 0 {
		t.Errorf("temporary branch was left behind:\n%s", branches)
	}
}

// observingWorkspaces lets a test look at the worktree at the moment it exists.
type observingWorkspaces struct {
	inner    workspace.WorkspaceManager
	onCreate func(*workspace.Workspace)
}

func (o *observingWorkspaces) Create(ctx context.Context, jobID, repoPath string) (*workspace.Workspace, error) {
	ws, err := o.inner.Create(ctx, jobID, repoPath)
	if err == nil && o.onCreate != nil {
		o.onCreate(ws)
	}
	return ws, err
}
func (o *observingWorkspaces) Cleanup(ctx context.Context, ws *workspace.Workspace) error {
	return o.inner.Cleanup(ctx, ws)
}
func (o *observingWorkspaces) Diff(ctx context.Context, ws *workspace.Workspace) (string, error) {
	return o.inner.Diff(ctx, ws)
}
func (o *observingWorkspaces) ListOrphans(ctx context.Context, repoPath string) ([]string, error) {
	return o.inner.ListOrphans(ctx, repoPath)
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
