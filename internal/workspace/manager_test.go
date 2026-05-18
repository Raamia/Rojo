package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Raamia/Rojo/internal/execution"
)

func hasGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed, skipping workspace integration test")
	}
}

func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "initial")
	return dir
}

func TestGitWorkspaceManager_CreateAndCleanup(t *testing.T) {
	hasGit(t)
	repo := initTestRepo(t)
	base := t.TempDir()
	m := NewGitWorkspaceManager(execution.NewExecRunner(), base)

	ws, err := m.Create(context.Background(), "job-1", repo)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := os.Stat(ws.Path); err != nil {
		t.Fatalf("worktree path missing: %v", err)
	}

	if err := m.Cleanup(context.Background(), ws); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(ws.Path); !os.IsNotExist(err) {
		t.Errorf("expected worktree removed, got err=%v", err)
	}
}

func TestGitWorkspaceManager_ValidationErrors(t *testing.T) {
	m := NewGitWorkspaceManager(execution.NewExecRunner(), t.TempDir())

	_, err := m.Create(context.Background(), "j", "relative/path")
	if !errors.Is(err, ErrPathTraversal) {
		t.Errorf("got %v, want ErrPathTraversal", err)
	}

	_, err = m.Create(context.Background(), "j", "/nonexistent/path/xyz")
	if !errors.Is(err, ErrRepoNotFound) {
		t.Errorf("got %v, want ErrRepoNotFound", err)
	}

	notARepo := t.TempDir()
	_, err = m.Create(context.Background(), "j", notARepo)
	if !errors.Is(err, ErrNotAGitRepo) {
		t.Errorf("got %v, want ErrNotAGitRepo", err)
	}
}

func TestGitWorkspaceManager_ConcurrentJobsIsolated(t *testing.T) {
	hasGit(t)
	repo := initTestRepo(t)
	base := t.TempDir()
	m := NewGitWorkspaceManager(execution.NewExecRunner(), base)

	var wg sync.WaitGroup
	paths := make([]string, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ws, err := m.Create(context.Background(), strings.Repeat("a", i+1), repo)
			if err != nil {
				t.Errorf("create %d: %v", i, err)
				return
			}
			paths[i] = ws.Path
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool)
	for _, p := range paths {
		if p == "" {
			continue
		}
		if seen[p] {
			t.Errorf("duplicate worktree path %s", p)
		}
		seen[p] = true
	}
}
