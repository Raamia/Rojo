package orchestration

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/execution"
	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/verification"
	"github.com/Raamia/Rojo/internal/workspace"
)

type fakeVerifier struct {
	report verification.Report
	err    error
	calls  int
	dirs   []string
}

func (f *fakeVerifier) Verify(_ context.Context, dir string) (verification.Report, error) {
	f.calls++
	f.dirs = append(f.dirs, dir)
	return f.report, f.err
}

func passingReport() verification.Report {
	return verification.Report{Results: []verification.Result{
		{Check: "gofmt", Passed: true},
		{Check: "go vet", Passed: true},
		{Check: "go test", Passed: true},
	}}
}

func failingReport() verification.Report {
	return verification.Report{Results: []verification.Result{
		{Check: "gofmt", Passed: true},
		{Check: "go vet", Passed: true},
		{Check: "go test", Passed: false, Output: "--- FAIL: TestThing"},
	}}
}

func TestProcessor_VerificationRunsInTheWorkspaceAndCompletes(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	bus := events.NewInProcessBus()
	sub := bus.Subscribe("verify-ok", 64)
	defer bus.Unsubscribe(sub)

	fakeWs := &fakeWorkspaces{}
	verifier := &fakeVerifier{report: passingReport()}
	newQueuedJob(t, repo, "verify-ok")

	p := NewProcessor(repo, NewCanceller(), bus, fakeWs, verifier)
	if err := p.Process(context.Background(), "verify-ok"); err != nil {
		t.Fatalf("process: %v", err)
	}

	if verifier.calls != 1 {
		t.Fatalf("Verify called %d times, want 1", verifier.calls)
	}
	// The checks must run against the isolated worktree, never the source repo.
	if want := filepath.Join("/tmp/rojo-test", "verify-ok"); verifier.dirs[0] != want {
		t.Errorf("verified %q, want the worktree %q", verifier.dirs[0], want)
	}

	got, err := repo.Get(context.Background(), "verify-ok")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != jobs.StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}

	counts := typesOf(drain(sub))
	if counts[events.TypeVerificationCompleted] != 1 {
		t.Errorf("verification.completed emitted %d times, want 1", counts[events.TypeVerificationCompleted])
	}
}

// The whole point of a deterministic gate: a job whose checks failed must not
// reach completed, whatever anything downstream thinks.
func TestProcessor_FailedChecksBlockCompletion(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	bus := events.NewInProcessBus()
	sub := bus.Subscribe("verify-fail", 64)
	defer bus.Unsubscribe(sub)

	fakeWs := &fakeWorkspaces{}
	verifier := &fakeVerifier{report: failingReport()}
	newQueuedJob(t, repo, "verify-fail")

	p := NewProcessor(repo, NewCanceller(), bus, fakeWs, verifier)
	err := p.Process(context.Background(), "verify-fail")
	if err == nil {
		t.Fatal("expected an error when verification fails")
	}

	got, getErr := repo.Get(context.Background(), "verify-fail")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status != jobs.StatusFailed {
		t.Fatalf("status = %q, want failed — failed checks must not produce a completed job", got.Status)
	}

	counts := typesOf(drain(sub))
	if counts[events.TypeVerificationCompleted] != 1 {
		t.Errorf("the report should still be emitted on failure, got %d", counts[events.TypeVerificationCompleted])
	}
	if counts[events.TypeJobCompleted] != 0 {
		t.Error("a job with failing checks emitted job.completed")
	}
	if counts[events.TypeJobFailed] != 1 {
		t.Errorf("job.failed emitted %d times, want 1", counts[events.TypeJobFailed])
	}

	// The worktree still has to be cleaned up on the failure path.
	if _, cleanup := fakeWs.counts(); cleanup != 1 {
		t.Errorf("Cleanup called %d times after a failed verification, want 1", cleanup)
	}
}

// A verifier that cannot run at all is a failure, not a silent skip.
func TestProcessor_VerifierErrorFailsTheJob(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	boom := errors.New("no verification checks configured")
	verifier := &fakeVerifier{err: boom}
	newQueuedJob(t, repo, "verify-err")

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus(), &fakeWorkspaces{}, verifier)
	err := p.Process(context.Background(), "verify-err")
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the underlying cause", err)
	}

	got, _ := repo.Get(context.Background(), "verify-err")
	if got.Status != jobs.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
}

// Without a workspace there is nothing to check, so verification is skipped
// rather than run against some unrelated directory.
func TestProcessor_VerificationSkippedWithoutAWorkspace(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	verifier := &fakeVerifier{report: failingReport()}
	newQueuedJob(t, repo, "no-ws-verify")

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus(), nil, verifier)
	if err := p.Process(context.Background(), "no-ws-verify"); err != nil {
		t.Fatalf("process: %v", err)
	}
	if verifier.calls != 0 {
		t.Errorf("Verify called %d times without a workspace, want 0", verifier.calls)
	}
	got, _ := repo.Get(context.Background(), "no-ws-verify")
	if got.Status != jobs.StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
}

// End to end with real git and the real toolchain: a clean module passes, and
// the same repository with a failing test fails the job.
func TestProcessor_RealVerificationEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	newRepo := func(t *testing.T, testBody string) string {
		t.Helper()
		dir := t.TempDir()
		write := func(name, content string) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write("go.mod", "module verifyjob\n\ngo 1.25\n")
		write("main.go", "package main\n\nfunc main() {}\n")
		write("main_test.go", "package main\n\nimport \"testing\"\n\nfunc TestIt(t *testing.T) {\n"+testBody+"\n}\n")

		run := func(args ...string) {
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		run("init", "-b", "main", ".")
		run("config", "user.email", "test@example.com")
		run("config", "user.name", "test")
		run("add", ".")
		run("commit", "-m", "initial")
		return dir
	}

	gitRunner := execution.NewSafeRunner(execution.NewExecRunner(), execution.NewAllowlist("git"), time.Minute)
	verifyRunner := execution.NewSafeRunner(execution.NewExecRunner(), execution.NewAllowlist("go", "gofmt"), 2*time.Minute)

	runJob := func(t *testing.T, id, repoPath string) jobs.JobStatus {
		t.Helper()
		base := t.TempDir()
		repo := jobs.NewInMemoryRepository()
		job := &jobs.Job{
			ID: id, Task: "verify this repo", RepoPath: repoPath,
			Status: jobs.StatusQueued, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := repo.Create(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		p := NewProcessor(
			repo, NewCanceller(), events.NewInProcessBus(),
			workspace.NewGitWorkspaceManager(gitRunner, base),
			verification.NewRunner(verifyRunner),
		)
		_ = p.Process(context.Background(), id)

		// Whatever the outcome, the worktree must be gone.
		if _, err := os.Stat(filepath.Join(base, id)); !os.IsNotExist(err) {
			t.Errorf("worktree left behind: %v", err)
		}
		got, err := repo.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return got.Status
	}

	if got := runJob(t, "clean-repo", newRepo(t, "")); got != jobs.StatusCompleted {
		t.Errorf("a repo that passes its checks ended %q, want completed", got)
	}
	if got := runJob(t, "failing-repo", newRepo(t, "\tt.Fatal(\"boom\")")); got != jobs.StatusFailed {
		t.Errorf("a repo with a failing test ended %q, want failed", got)
	}
}
