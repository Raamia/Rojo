package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Raamia/Rojo/internal/execution"
)

// argvRecorder captures every argv the manager issues.
type argvRecorder struct{ calls [][]string }

func (r *argvRecorder) Run(_ context.Context, _ string, command string, args ...string) (execution.CommandResult, error) {
	r.calls = append(r.calls, append([]string{command}, args...))
	return execution.CommandResult{}, nil
}

// repo_path comes from the API caller, and `git worktree add` performs a
// checkout — which runs that repository's post-checkout hook as the server
// user. Every git invocation must therefore carry the hardening flags, and they
// must precede the subcommand or git treats them as subcommand arguments.
func TestGitHardening_EveryInvocationDisablesHooksAndFsmonitor(t *testing.T) {
	rec := &argvRecorder{}
	base := t.TempDir()
	m := NewGitWorkspaceManager(rec, base)

	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	ws, err := m.Create(ctx, "job-1", repo)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := m.Diff(ctx, ws); err != nil {
		t.Fatalf("diff: %v", err)
	}
	if _, err := m.ListOrphans(ctx, repo); err != nil {
		t.Fatalf("list orphans: %v", err)
	}
	if err := m.Cleanup(ctx, ws); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	if len(rec.calls) == 0 {
		t.Fatal("no git commands were recorded")
	}
	for _, argv := range rec.calls {
		if argv[0] != "git" {
			t.Errorf("unexpected command %q", argv[0])
			continue
		}
		joined := strings.Join(argv, " ")
		for _, want := range []string{"-c core.hooksPath=/dev/null", "-c core.fsmonitor=false"} {
			if !strings.Contains(joined, want) {
				t.Errorf("argv %q is missing %q", joined, want)
			}
		}
		// The flags are only honoured before the subcommand.
		if len(argv) < 6 || argv[1] != "-c" {
			t.Errorf("argv %q must carry -c flags before the subcommand", joined)
		}
	}
}

// The real thing: a repository carrying a malicious post-checkout hook must not
// execute it when Rojo creates a worktree from it. Without the hardening this
// writes a file as the server user.
func TestGitHardening_PostCheckoutHookDoesNotRun(t *testing.T) {
	hasGit(t)

	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main", ".")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "initial")

	// Plant the hook. A real attacker ships this in the repository they ask
	// Rojo to work on.
	marker := filepath.Join(t.TempDir(), "PWNED.txt")
	hook := "#!/bin/sh\necho pwned > " + marker + "\n"
	if err := os.WriteFile(filepath.Join(repo, ".git", "hooks", "post-checkout"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	m := NewGitWorkspaceManager(execution.NewExecRunner(), t.TempDir())
	ws, err := m.Create(context.Background(), "hook-job", repo)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer m.Cleanup(context.Background(), ws)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the repository's post-checkout hook executed — arbitrary code ran as the server user")
	}

	// The worktree itself must still have been created properly.
	if _, err := os.Stat(filepath.Join(ws.Path, "README.md")); err != nil {
		t.Errorf("worktree checkout is incomplete: %v", err)
	}
}

// core.fsmonitor is the same hole without a hook file: git runs whatever command
// the repo-local config names, on ordinary operations.
func TestGitHardening_FsmonitorCommandDoesNotRun(t *testing.T) {
	hasGit(t)

	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main", ".")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "initial")

	marker := filepath.Join(t.TempDir(), "FSMON.txt")
	script := filepath.Join(repo, "evil.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho pwned > "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run("config", "core.fsmonitor", script)

	m := NewGitWorkspaceManager(execution.NewExecRunner(), t.TempDir())
	ws, err := m.Create(context.Background(), "fsmon-job", repo)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer m.Cleanup(context.Background(), ws)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("core.fsmonitor command executed — arbitrary code ran as the server user")
	}
}
