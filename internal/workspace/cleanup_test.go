package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Raamia/Rojo/internal/execution"
)

// scriptedRunner returns a canned result per git subcommand and records the
// order commands were issued in, so cleanup's fallback behavior can be driven
// without a real repository.
type scriptedRunner struct {
	results map[string]execution.CommandResult
	errs    map[string]error
	calls   []string
}

func (r *scriptedRunner) Run(_ context.Context, _ string, command string, args ...string) (execution.CommandResult, error) {
	args = stripGitConfigFlags(args)
	key := command
	if len(args) > 0 {
		key = command + " " + args[0]
		if args[0] == "worktree" && len(args) > 1 {
			key = command + " worktree " + args[1]
		}
	}
	r.calls = append(r.calls, key)
	return r.results[key], r.errs[key]
}

// stripGitConfigFlags drops the leading `-c key=value` pairs that gitArgs
// prepends, so tests can key on the subcommand.
func stripGitConfigFlags(args []string) []string {
	for len(args) >= 2 && args[0] == "-c" {
		args = args[2:]
	}
	return args
}

func (r *scriptedRunner) called(prefix string) bool {
	for _, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func tempWorkspace(t *testing.T) *Workspace {
	t.Helper()
	dir := t.TempDir()
	wt := filepath.Join(dir, "worktree")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	return &Workspace{JobID: "job-1", Branch: "rojo/job/job-1", Path: wt, RepoPath: dir}
}

// A command that runs and exits non-zero is reported as (result, nil): the
// failure lives in ExitCode, not err. Checking only err made a failed
// `git worktree remove` look like success, so the os.RemoveAll fallback never
// ran and Cleanup returned nil while the directory was still on disk.
func TestCleanup_NonZeroExitTriggersFallbackAndIsReported(t *testing.T) {
	ws := tempWorkspace(t)
	r := &scriptedRunner{
		results: map[string]execution.CommandResult{
			"git worktree remove": {ExitCode: 1, Stderr: "fatal: cannot remove"},
		},
	}
	m := NewGitWorkspaceManager(r, filepath.Dir(ws.Path))

	err := m.Cleanup(context.Background(), ws)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, statErr := os.Stat(ws.Path); !os.IsNotExist(statErr) {
		t.Error("worktree directory still exists; the fallback did not run")
	}
	if !r.called("git worktree prune") {
		t.Error("expected a prune after removing the directory behind git's back")
	}
}

// Removing the directory ourselves leaves the worktree registered with git, so
// the stale entry has to be pruned or `git worktree list` keeps reporting it.
func TestCleanup_PruneFailureIsReported(t *testing.T) {
	ws := tempWorkspace(t)
	r := &scriptedRunner{
		results: map[string]execution.CommandResult{
			"git worktree remove": {ExitCode: 1},
			"git worktree prune":  {ExitCode: 128, Stderr: "fatal: not a git repository"},
		},
	}
	m := NewGitWorkspaceManager(r, filepath.Dir(ws.Path))

	err := m.Cleanup(context.Background(), ws)
	if err == nil {
		t.Fatal("expected an error when prune fails")
	}
	if !strings.Contains(err.Error(), "prune") {
		t.Errorf("error %q should mention prune", err)
	}
}

// A worktree that was removed but whose branch survived is still a leak.
func TestCleanup_BranchDeleteFailureIsReported(t *testing.T) {
	ws := tempWorkspace(t)
	r := &scriptedRunner{
		results: map[string]execution.CommandResult{
			"git branch": {ExitCode: 1, Stderr: "error: branch not found"},
		},
	}
	m := NewGitWorkspaceManager(r, filepath.Dir(ws.Path))

	err := m.Cleanup(context.Background(), ws)
	if err == nil {
		t.Fatal("expected an error when the branch delete fails")
	}
	if !strings.Contains(err.Error(), ws.Branch) {
		t.Errorf("error %q should name the branch", err)
	}
}

// Cleanup runs from a deferred call on cancelled and timed-out jobs, so the
// context it receives is usually already dead. Inheriting that cancellation
// would make every git command fail instantly and leak the worktree exactly
// when cleanup matters most.
func TestCleanup_RunsEvenWithAnAlreadyCancelledContext(t *testing.T) {
	ws := tempWorkspace(t)
	r := &scriptedRunner{}
	m := NewGitWorkspaceManager(r, filepath.Dir(ws.Path))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // dead before cleanup is even called

	if err := m.Cleanup(ctx, ws); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if !r.called("git worktree remove") {
		t.Error("worktree remove was never attempted on a cancelled context")
	}
	if !r.called("git branch") {
		t.Error("branch delete was never attempted on a cancelled context")
	}
}

// Every failure should be reported, not just the first one encountered.
func TestCleanup_ReportsBothWorktreeAndBranchFailures(t *testing.T) {
	ws := tempWorkspace(t)
	r := &scriptedRunner{
		results: map[string]execution.CommandResult{
			"git worktree remove": {ExitCode: 1},
			"git worktree prune":  {ExitCode: 1, Stderr: "prune failed"},
			"git branch":          {ExitCode: 1, Stderr: "branch failed"},
		},
	}
	m := NewGitWorkspaceManager(r, filepath.Dir(ws.Path))

	err := m.Cleanup(context.Background(), ws)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"prune", ws.Branch} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("joined error %q is missing %q", err, want)
		}
	}
}

func TestCleanup_NilWorkspaceIsANoop(t *testing.T) {
	r := &scriptedRunner{}
	m := NewGitWorkspaceManager(r, t.TempDir())
	if err := m.Cleanup(context.Background(), nil); err != nil {
		t.Fatalf("cleanup(nil): %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("expected no commands, got %v", r.calls)
	}
}

// The happy path must stay quiet: a clean removal reports no error and does not
// reach for the fallback.
func TestCleanup_SuccessSkipsFallback(t *testing.T) {
	ws := tempWorkspace(t)
	r := &scriptedRunner{} // all zero-value results == exit 0
	m := NewGitWorkspaceManager(r, filepath.Dir(ws.Path))

	if err := m.Cleanup(context.Background(), ws); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if r.called("git worktree prune") {
		t.Error("prune should only run when the normal removal failed")
	}
}
