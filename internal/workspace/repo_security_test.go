package workspace

// Security audit tests for repository validation and worktree isolation.
//
// Tests ASSERT CURRENT BEHAVIOUR. Names ending in _DocumentsGap pin a
// vulnerable behaviour so the suite stays green while `go test -v` prints the
// gap.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/execution"
)

type secWSRecorder struct {
	workingDir string
	command    string
	args       []string
}

func (r *secWSRecorder) Run(_ context.Context, workingDir, command string, args ...string) (execution.CommandResult, error) {
	r.workingDir = workingDir
	r.command = command
	r.args = append([]string(nil), args...)
	return execution.CommandResult{ExitCode: 0}, nil
}

// ===========================================================================
// repo_path validation
// ===========================================================================

// validateRepoPath (manager.go:32-51) accepts ANY absolute directory that has
// a `.git` entry. There is no allowlist of permitted repositories and no
// confinement to a configured root. Combined with the API accepting an
// arbitrary absolute repo_path (jobs/request.go:42), a caller chooses which
// directory on the host the server operates in.
func TestSecurity_ValidateRepoPath_AcceptsAnyAbsoluteDirWithDotGit_DocumentsGap(t *testing.T) {
	// (a) An ordinary repo anywhere on the filesystem.
	realish := filepath.Join(t.TempDir(), "someone-elses-repo")
	if err := os.MkdirAll(filepath.Join(realish, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validateRepoPath(realish); err != nil {
		t.Fatalf("VULNERABILITY FIXED? %s rejected: %v", realish, err)
	}
	t.Logf("ACCEPTED arbitrary path: %s (no allowlist, no root confinement)", realish)

	// (b) A directory containing only an EMPTY FILE named .git. The check is
	// os.Stat, which succeeds for files too, so "is this a git repository?" is
	// answered by the presence of a name, not by git.
	fake := filepath.Join(t.TempDir(), "not-really-a-repo")
	if err := os.MkdirAll(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fake, ".git"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateRepoPath(fake); err != nil {
		t.Fatalf("VULNERABILITY FIXED? empty .git file rejected: %v", err)
	}
	t.Logf("ACCEPTED %s whose .git is an EMPTY FILE — the repo check is a name lookup, not `git rev-parse`", fake)

	// (c) A symlink pointing at a repository elsewhere. os.Stat follows it.
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "innocent-looking")
	if err := os.Symlink(realish, link); err != nil {
		t.Fatal(err)
	}
	if err := validateRepoPath(link); err != nil {
		t.Fatalf("VULNERABILITY FIXED? symlinked repo path rejected: %v", err)
	}
	t.Logf("ACCEPTED symlink %s -> %s; paths are never canonicalised before use", link, realish)

	t.Log("GAP: no ROJO_ALLOWED_REPOS / root-prefix check exists. Any absolute path the caller " +
		"names becomes the working directory for git commands and the source of the worktree.")
}

// Controls that do hold.
func TestSecurity_ValidateRepoPath_RejectsRelativeAndMissing(t *testing.T) {
	for _, p := range []string{"relative/path", "./x", "..", ""} {
		if err := validateRepoPath(p); !errors.Is(err, ErrPathTraversal) {
			t.Errorf("relative path %q: got %v, want ErrPathTraversal", p, err)
		}
	}
	if err := validateRepoPath("/definitely/not/here/rojo-audit"); !errors.Is(err, ErrRepoNotFound) {
		t.Errorf("missing path: got %v, want ErrRepoNotFound", err)
	}
	plain := t.TempDir()
	if err := validateRepoPath(plain); !errors.Is(err, ErrNotAGitRepo) {
		t.Errorf("non-repo dir: got %v, want ErrNotAGitRepo", err)
	}
	t.Log("RESULT: relative paths, missing paths and non-repo directories are correctly rejected")
}

// ===========================================================================
// Arbitrary code execution via a caller-chosen repository
// ===========================================================================

// CRITICAL (once the workspace layer is wired into the orchestrator):
// GitWorkspaceManager.Create runs `git worktree add` with cmd.Dir set to the
// caller-supplied repoPath (git.go:35). `git worktree add` performs a checkout,
// which fires that repository's post-checkout hook. The hook is an executable
// file inside the repository the CALLER named — so naming a repository is
// equivalent to running its code as the API server user.
func TestSecurity_GitWorktreeAdd_ExecutesRepoSuppliedHook_DocumentsGap(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not available: %v", err)
	}

	root := t.TempDir()
	repo := filepath.Join(root, "attacker-repo")
	marker := filepath.Join(root, "PWNED")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main", ".")
	run("-c", "user.email=a@b.test", "-c", "user.name=audit", "commit", "-q", "--allow-empty", "-m", "init")

	// The payload: an ordinary executable file inside the repository.
	hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := "#!/bin/sh\nid > '" + marker + "'\n"
	if err := os.WriteFile(hook, []byte(payload), 0o755); err != nil {
		t.Fatal(err)
	}

	base := filepath.Join(root, "worktrees")
	m := NewGitWorkspaceManager(execution.NewExecRunner(), base)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ws, err := m.Create(ctx, "audit-job-1", repo)
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	t.Cleanup(func() { _ = m.Cleanup(context.Background(), ws) })

	body, readErr := os.ReadFile(marker)
	if readErr != nil {
		t.Skipf("post-checkout hook did not fire in this environment (%v); "+
			"the code path is still unguarded — see the argv assertion test", readErr)
	}
	t.Logf("PROVEN ARBITRARY CODE EXECUTION: `git worktree add` ran the repository's "+
		"post-checkout hook as the server user. Hook output (`id`): %s", strings.TrimSpace(string(body)))
	t.Log("REPRO: POST /api/v1/jobs {\"task\":\"...\",\"repo_path\":\"/path/to/attacker/repo\"} where " +
		"that repo contains .git/hooks/post-checkout. No allowlist blocks the path, and " +
		"`git worktree add` is run with the repo as cwd. Same applies to core.fsmonitor / " +
		"core.sshCommand set in that repo's .git/config.")
}

// The argv/cwd shape that makes the above possible, asserted without needing
// git — so the finding stays visible even where hooks are disabled.
func TestSecurity_WorktreeCreate_RunsGitInsideCallerSuppliedRepo_DocumentsGap(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "caller-chosen")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &secWSRecorder{}
	m := NewGitWorkspaceManager(rec, t.TempDir())

	if _, err := m.Create(context.Background(), "job-1", repo); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rec.workingDir != repo {
		t.Fatalf("VULNERABILITY FIXED? git no longer runs in the caller's repo (cwd=%q)", rec.workingDir)
	}
	t.Logf("PROVEN: cwd=%s  argv=git %s — the caller's directory is the git working directory, "+
		"so that repository's .git/config and .git/hooks govern what executes.",
		rec.workingDir, strings.Join(rec.args, " "))
}

// MEDIUM (latent): jobID is interpolated into both the branch name and the
// worktree path with no validation (git.go:32-33). It is server-generated hex
// today, so this is not currently reachable — but any future path that lets a
// caller choose a job/workspace identifier escapes the configured base dir.
func TestSecurity_WorktreePath_JobIDIsNotValidated_DocumentsGap(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(t.TempDir(), "worktrees")

	rec := &secWSRecorder{}
	m := NewGitWorkspaceManager(rec, base)

	hostileID := "../../../../tmp/rojo-escape"
	ws, err := m.Create(context.Background(), hostileID, repo)
	if err != nil {
		t.Fatalf("VULNERABILITY FIXED? hostile jobID rejected: %v", err)
	}

	absBase, _ := filepath.Abs(base)
	absWS, _ := filepath.Abs(ws.Path)
	if strings.HasPrefix(absWS, absBase+string(filepath.Separator)) {
		t.Fatalf("VULNERABILITY FIXED? worktree stayed under base: %s", absWS)
	}
	t.Logf("PROVEN ESCAPE OF baseDir: jobID %q produced worktree path %s (baseDir was %s) "+
		"and branch %q. filepath.Join collapses the traversal; nothing validates jobID.",
		hostileID, ws.Path, base, ws.Branch)
	t.Log("NOTE: not currently reachable — cmd/api/main.go generates job IDs with crypto/rand hex " +
		"and the workspace layer is not yet wired into the orchestrator. Validate the ID anyway; " +
		"Cleanup() calls os.RemoveAll(ws.Path) on this same unvalidated path.")
}

// Cleanup falls back to os.RemoveAll(ws.Path) when `git worktree remove` fails
// (git.go:58-60). Pinned so the recursive delete is never pointed at an
// unvalidated path.
func TestSecurity_Cleanup_RecursivelyDeletesWorkspacePath(t *testing.T) {
	base := t.TempDir()
	victim := filepath.Join(base, "job-x")
	if err := os.MkdirAll(filepath.Join(victim, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "nested", "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A runner that always errors, forcing the RemoveAll fallback.
	m := NewGitWorkspaceManager(alwaysFailRunner{}, base)
	ws := &Workspace{JobID: "job-x", Branch: "rojo/job/job-x", Path: victim, RepoPath: base}
	_ = m.Cleanup(context.Background(), ws)

	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Fatalf("expected recursive delete, got err=%v", err)
	}
	t.Logf("CONFIRMED: Cleanup falls back to os.RemoveAll(%q). Safe only because ws.Path is "+
		"constructed from baseDir + a server-generated job ID — see the jobID test above.", victim)
}

type alwaysFailRunner struct{}

func (alwaysFailRunner) Run(context.Context, string, string, ...string) (execution.CommandResult, error) {
	return execution.CommandResult{}, errors.New("simulated git failure")
}
