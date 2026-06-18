package workspace

// Resilience characterization tests for worktree cleanup and orphan recovery.
// Assertions document ACTUAL behavior, including behavior that is a bug.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Raamia/Rojo/internal/execution"
)

// resStubRunner returns a canned CommandResult, letting a test simulate git
// failing the way it actually fails: a non-zero exit code with a nil error.
// ExecRunner.Run (internal/execution/runner.go:46-51) converts *exec.ExitError
// into (result{ExitCode: n}, nil) — a non-nil error means the binary could not
// be started at all.
type resStubRunner struct {
	mu       sync.Mutex
	calls    [][]string
	exitCode int
	err      error
}

func (r *resStubRunner) Run(_ context.Context, _ string, command string, args ...string) (execution.CommandResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string{command}, args...))
	return execution.CommandResult{ExitCode: r.exitCode, Stderr: "fatal: simulated git failure"}, r.err
}

func (r *resStubRunner) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.calls))
	for _, c := range r.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

// ---------------------------------------------------------------------------
// FAILURE MODE 4 — cleanup reports success while leaking the worktree
// ---------------------------------------------------------------------------

// GitWorkspaceManager.Cleanup (git.go:58-63) checks only the error return of
// runner.Run and ignores CommandResult.ExitCode. Because a failing git command
// yields (ExitCode: 1, nil), every ordinary git failure is invisible: the
// os.RemoveAll fallback never runs, `git branch -D` is never retried, and
// Cleanup returns nil. The caller believes the workspace is gone.
func TestResilience_CleanupIgnoresGitExitCodeAndLeaksWorktreeSilently(t *testing.T) {
	baseDir := t.TempDir()
	wtPath := filepath.Join(baseDir, "job-leak")
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtPath, "leftover.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	runner := &resStubRunner{exitCode: 1} // git fails, but err == nil
	m := NewGitWorkspaceManager(runner, baseDir)

	err := m.Cleanup(context.Background(), &Workspace{
		JobID:    "job-leak",
		Branch:   branchPrefix + "job-leak",
		Path:     wtPath,
		RepoPath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Cleanup returned %v, want nil — exit codes are now checked, re-baseline this test", err)
	}

	if _, statErr := os.Stat(wtPath); statErr != nil {
		t.Fatalf("worktree directory was removed (%v) — the RemoveAll fallback now runs, re-baseline this test", statErr)
	}
	t.Logf("CONFIRMED: Cleanup returned nil while %s still exists; commands issued: %v", wtPath, runner.commands())
}

// The error branch that does work — os.RemoveAll — only fires when the git
// binary itself cannot be executed. That is the rare case; the common case
// (git runs and refuses) is the one that leaks.
func TestResilience_CleanupFallbackOnlyRunsWhenGitCannotBeExecuted(t *testing.T) {
	baseDir := t.TempDir()
	wtPath := filepath.Join(baseDir, "job-exec-fail")
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	runner := &resStubRunner{err: os.ErrNotExist} // git binary missing
	m := NewGitWorkspaceManager(runner, baseDir)

	err := m.Cleanup(context.Background(), &Workspace{
		JobID:    "job-exec-fail",
		Branch:   branchPrefix + "job-exec-fail",
		Path:     wtPath,
		RepoPath: t.TempDir(),
	})
	// The worktree dir is force-removed, but the branch delete then fails and
	// Cleanup surfaces that error — so the caller cannot distinguish "nothing
	// cleaned up" from "partially cleaned up".
	if err == nil {
		t.Fatal("Cleanup returned nil despite an exec failure — re-baseline this test")
	}
	if !strings.Contains(err.Error(), "delete branch") {
		t.Fatalf("Cleanup error = %v, want a 'delete branch' failure", err)
	}
	if _, statErr := os.Stat(wtPath); !os.IsNotExist(statErr) {
		t.Fatalf("worktree directory still exists after the RemoveAll fallback: %v", statErr)
	}
}

// ---------------------------------------------------------------------------
// FAILURE MODE 4 — orphan recovery exists but is never invoked
// ---------------------------------------------------------------------------

// ListOrphans is implemented and unit-tested, but no production code path calls
// it. cmd/api/main.go performs no startup sweep, so worktrees left behind by a
// crash accumulate in ROJO_WORKTREE_DIR forever. (Today the orchestrator never
// creates a worktree at all — see the companion assertion below — so nothing
// leaks yet; the moment the workspace manager is wired in, it will.)
func TestResilience_NoProductionCallerInvokesListOrphans(t *testing.T) {
	allowed := map[string]bool{
		filepath.Join("internal", "workspace", "diff.go"):    true, // definition
		filepath.Join("internal", "workspace", "manager.go"): true, // interface method
	}

	callers := resGrepGoSources(t, "ListOrphans", allowed)
	if len(callers) != 0 {
		t.Fatalf("ListOrphans is now called from %v — startup orphan recovery was added, re-baseline this test", callers)
	}
	t.Log("CONFIRMED: no startup or periodic sweep calls ListOrphans; orphaned worktrees are never reclaimed")
}

// The orchestration pipeline does not create workspaces yet: nothing outside
// the package's own tests even imports internal/workspace. Failure mode
// "crash mid-job leaks a worktree" is therefore currently vacuous — and so is
// the entire preparing_workspace step, which only sleeps 50ms.
func TestResilience_OrchestrationDoesNotCreateWorkspacesYet(t *testing.T) {
	importers := resGrepGoSources(t, "Rojo/internal/workspace", nil)
	for _, f := range importers {
		if strings.HasPrefix(f, filepath.Join("internal", "workspace")) {
			continue
		}
		t.Fatalf("%s now imports internal/workspace — the pipeline creates worktrees, re-baseline this test and audit every cleanup path", f)
	}
	t.Log("CONFIRMED: no production package imports internal/workspace; Processor.Process creates no worktree and so cannot leak one")
}

// resGrepGoSources walks the module root and returns every non-test .go file
// containing needle, excluding the allowed set. Paths are module-relative.
func resGrepGoSources(t *testing.T, needle string, allowed map[string]bool) []string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}

	var hits []string
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !strings.Contains(string(body), needle) {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if allowed[rel] {
			return nil
		}
		hits = append(hits, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk module root: %v", err)
	}
	return hits
}
