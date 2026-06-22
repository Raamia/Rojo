package tests

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/execution"
	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/orchestration"
	"github.com/Raamia/Rojo/internal/queue"
	"github.com/Raamia/Rojo/internal/worker"
	"github.com/Raamia/Rojo/internal/workspace"
)

func hasGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed, skipping e2e test")
	}
}

func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	run("commit", "--allow-empty", "-m", "initial")
	return dir
}

func TestEndToEnd_JobFlowsThroughStates(t *testing.T) {
	hasGit(t)

	repo := jobs.NewInMemoryRepository()
	q := queue.New(4)
	canceller := orchestration.NewCanceller()
	processor := orchestration.NewProcessor(repo, canceller, nil, nil)

	pool := worker.NewPool(1, q, processor)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)

	repoPath := initTestRepo(t)
	wsMgr := workspace.NewGitWorkspaceManager(execution.NewExecRunner(), filepath.Join(t.TempDir(), "worktrees"))

	ws, err := wsMgr.Create(ctx, "e2e-job", repoPath)
	if err != nil {
		t.Fatalf("workspace create: %v", err)
	}
	defer wsMgr.Cleanup(context.Background(), ws)

	now := time.Now().UTC()
	job := &jobs.Job{
		ID:        "e2e-job",
		Task:      "run tests",
		RepoPath:  repoPath,
		Status:    jobs.StatusQueued,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := repo.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(job.ID); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := repo.Get(ctx, "e2e-job")
		if got != nil && got.Status == jobs.StatusCompleted {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := repo.Get(ctx, "e2e-job")
	t.Fatalf("job did not reach completed; last status = %s", got.Status)
}
