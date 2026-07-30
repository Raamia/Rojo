package mcpserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Raamia/Rojo/internal/verification"
	"github.com/Raamia/Rojo/internal/workspace"
)

// fakeWorkspaces stands in for the git manager. Create makes a real directory,
// because the sandbox writes through os.Root and needs somewhere that actually
// exists; everything else is recorded so a test can assert on it.
type fakeWorkspaces struct {
	mu sync.Mutex
	t  *testing.T

	createErr  error
	cleanupErr error
	diff       string
	diffErr    error

	createCalls  int
	cleanupCalls int
	// cleanupCtxErr records the state of the context cleanup was handed, which
	// is what proves cancellation was stripped before it ran.
	cleanupCtxErr error
	created       []string
}

func newFakeWorkspaces(t *testing.T) *fakeWorkspaces {
	return &fakeWorkspaces{t: t}
}

func (f *fakeWorkspaces) Create(_ context.Context, id, repoPath string) (*workspace.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	f.created = append(f.created, id)
	if f.createErr != nil {
		return nil, f.createErr
	}
	dir := f.t.TempDir()
	return &workspace.Workspace{
		JobID: id, Branch: "rojo/job/" + id, Path: dir, RepoPath: repoPath,
	}, nil
}

func (f *fakeWorkspaces) Cleanup(ctx context.Context, _ *workspace.Workspace) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanupCalls++
	f.cleanupCtxErr = ctx.Err()
	return f.cleanupErr
}

func (f *fakeWorkspaces) Diff(_ context.Context, _ *workspace.Workspace) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.diff, f.diffErr
}

func (f *fakeWorkspaces) cleanups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cleanupCalls
}

func (f *fakeWorkspaces) creates() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCalls
}

// fakeChecks returns a canned report without running anything.
type fakeChecks struct {
	report verification.Report
	err    error
	// dir records the directory it was asked to check, so a test can prove the
	// worktree was verified rather than the source repository.
	dir string
}

func (f *fakeChecks) Verify(_ context.Context, dir string) (verification.Report, error) {
	f.dir = dir
	return f.report, f.err
}

func passingReport() verification.Report {
	return verification.Report{Results: []verification.Result{
		{Check: "go test", Passed: true, Duration: 12},
	}}
}

func newGate(t *testing.T, ws *fakeWorkspaces, checks *fakeChecks) *Gate {
	t.Helper()
	return &Gate{Workspaces: ws, Checks: checks}
}

func TestGate_Verify_PassingChecks(t *testing.T) {
	ws := newFakeWorkspaces(t)
	ws.diff = "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n"
	checks := &fakeChecks{report: passingReport()}
	g := newGate(t, ws, checks)

	res, err := g.Verify(context.Background(), VerifyRequest{
		RepoPath:   "/repo",
		Operations: []Operation{{Kind: "write", Path: "main.go", Content: "package main\n"}},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.Passed {
		t.Errorf("Passed = false, want true")
	}
	if len(res.Checks) != 1 || res.Checks[0].Check != "go test" {
		t.Errorf("Checks = %+v, want the one report result", res.Checks)
	}
	if got := res.ChangedFiles; len(got) != 1 || got[0] != "main.go" {
		t.Errorf("ChangedFiles = %v, want [main.go]", got)
	}
}

// The checks must run against the isolated checkout, never the source
// repository — that is the entire isolation guarantee.
func TestGate_Verify_ChecksTheWorktreeNotTheRepo(t *testing.T) {
	ws := newFakeWorkspaces(t)
	checks := &fakeChecks{report: passingReport()}
	g := newGate(t, ws, checks)

	if _, err := g.Verify(context.Background(), VerifyRequest{RepoPath: "/source/repo"}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if checks.dir == "/source/repo" {
		t.Fatal("checks ran against the source repository, not the worktree")
	}
	if checks.dir == "" {
		t.Fatal("checks never ran")
	}
}

// The operations must actually reach the worktree's filesystem. Without this a
// gate could report a confident pass on an unmodified checkout.
func TestGate_Verify_AppliesOperationsToTheWorktree(t *testing.T) {
	ws := newFakeWorkspaces(t)
	checks := &fakeChecks{report: passingReport()}
	g := newGate(t, ws, checks)

	_, err := g.Verify(context.Background(), VerifyRequest{
		RepoPath: "/repo",
		Operations: []Operation{
			{Kind: "write", Path: "pkg/thing.go", Content: "package pkg\n"},
		},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	written := filepath.Join(checks.dir, "pkg", "thing.go")
	b, err := os.ReadFile(written)
	if err != nil {
		t.Fatalf("operation was not applied to the worktree: %v", err)
	}
	if string(b) != "package pkg\n" {
		t.Errorf("content = %q, want %q", b, "package pkg\n")
	}
}

func TestGate_Verify_FailingChecksAreNotAPass(t *testing.T) {
	ws := newFakeWorkspaces(t)
	checks := &fakeChecks{report: verification.Report{Results: []verification.Result{
		{Check: "go test", Passed: false, Output: "FAIL: TestThing"},
	}}}
	g := newGate(t, ws, checks)

	res, err := g.Verify(context.Background(), VerifyRequest{RepoPath: "/repo"})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Passed {
		t.Error("Passed = true for a failing report")
	}
	if !strings.Contains(res.Checks[0].Output, "FAIL: TestThing") {
		t.Errorf("failing output was not carried through: %q", res.Checks[0].Output)
	}
}

// A gate that could not be *run* must never read as a pass. An empty report
// satisfies AllPassed, so the verifier's error is the only thing standing
// between "nothing was checked" and "everything passed".
func TestGate_Verify_VerifierErrorIsNotAPass(t *testing.T) {
	ws := newFakeWorkspaces(t)
	checks := &fakeChecks{err: errors.New("go: toolchain not found")}
	g := newGate(t, ws, checks)

	res, err := g.Verify(context.Background(), VerifyRequest{RepoPath: "/repo"})
	if err != nil {
		t.Fatalf("Verify returned an error, want a result: %v", err)
	}
	if res.Passed {
		t.Fatal("Passed = true when verification could not run")
	}
	if !strings.Contains(res.Summary, "could not be completed") {
		t.Errorf("Summary = %q, want it to say verification could not be completed", res.Summary)
	}
	if !strings.Contains(res.Summary, "toolchain not found") {
		t.Errorf("Summary = %q, want the underlying cause named", res.Summary)
	}
}

func TestGate_Verify_CleansUpWorktree(t *testing.T) {
	ws := newFakeWorkspaces(t)
	g := newGate(t, ws, &fakeChecks{report: passingReport()})

	if _, err := g.Verify(context.Background(), VerifyRequest{RepoPath: "/repo"}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ws.cleanups() != 1 {
		t.Errorf("cleanupCalls = %d, want 1", ws.cleanups())
	}
}

// A refused operation must still reclaim the checkout that was already made.
func TestGate_Verify_CleansUpAfterApplyFailure(t *testing.T) {
	ws := newFakeWorkspaces(t)
	g := newGate(t, ws, &fakeChecks{report: passingReport()})

	_, err := g.Verify(context.Background(), VerifyRequest{
		RepoPath: "/repo",
		// The sandbox refuses .git in any segment.
		Operations: []Operation{{Kind: "write", Path: ".git/hooks/pre-commit", Content: "x"}},
	})
	if err == nil {
		t.Fatal("Verify accepted a write into .git")
	}
	if ws.cleanups() != 1 {
		t.Errorf("cleanupCalls = %d, want 1 — a refused operation must not leak the worktree", ws.cleanups())
	}
}

// The worktree must be reclaimed even when the caller's context is already
// dead, which is exactly when nobody is left watching for the leak.
func TestGate_Verify_CleanupSurvivesCancelledContext(t *testing.T) {
	ws := newFakeWorkspaces(t)
	checks := &fakeChecks{report: passingReport()}
	g := newGate(t, ws, checks)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // dead before the call even starts

	// Create is handed the cancelled context and the fake ignores it, which is
	// the point: the assertion is about what cleanup receives.
	if _, err := g.Verify(ctx, VerifyRequest{RepoPath: "/repo"}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ws.cleanups() != 1 {
		t.Fatalf("cleanupCalls = %d, want 1", ws.cleanups())
	}
	if ws.cleanupCtxErr != nil {
		t.Errorf("cleanup context error = %v, want nil — cancellation must be stripped "+
			"or a cancelled call leaves the worktree behind", ws.cleanupCtxErr)
	}
}

func TestGate_Verify_RejectsEmptyRepoPath(t *testing.T) {
	ws := newFakeWorkspaces(t)
	g := newGate(t, ws, &fakeChecks{report: passingReport()})

	for _, path := range []string{"", "   "} {
		if _, err := g.Verify(context.Background(), VerifyRequest{RepoPath: path}); !errors.Is(err, ErrNoRepoPath) {
			t.Errorf("repo_path %q: err = %v, want ErrNoRepoPath", path, err)
		}
	}
	if ws.creates() != 0 {
		t.Error("a worktree was created for a request with no repo path")
	}
}

// Malformed operations are rejected before any checkout is made, so a caller's
// mistake does not cost a worktree.
func TestGate_Verify_RejectsBadOperationsBeforeCreatingAWorktree(t *testing.T) {
	tests := []struct {
		name string
		op   Operation
		want string
	}{
		{"unknown kind", Operation{Kind: "patch", Path: "a.go"}, "kind must be"},
		{"empty kind", Operation{Kind: "", Path: "a.go"}, "kind must be"},
		{"empty path", Operation{Kind: "write", Path: ""}, "path is required"},
		{"blank path", Operation{Kind: "write", Path: "   "}, "path is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := newFakeWorkspaces(t)
			g := newGate(t, ws, &fakeChecks{report: passingReport()})

			_, err := g.Verify(context.Background(), VerifyRequest{
				RepoPath: "/repo", Operations: []Operation{tt.op},
			})
			if err == nil {
				t.Fatalf("Verify accepted %+v", tt.op)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %q, want it to contain %q", err, tt.want)
			}
			if ws.creates() != 0 {
				t.Error("a worktree was created before the operations were validated")
			}
		})
	}
}

// No operations is a legitimate question — "does this repository pass as it
// stands" — and must run the checks rather than short-circuit.
func TestGate_Verify_NoOperationsStillRunsChecks(t *testing.T) {
	ws := newFakeWorkspaces(t)
	checks := &fakeChecks{report: passingReport()}
	g := newGate(t, ws, checks)

	res, err := g.Verify(context.Background(), VerifyRequest{RepoPath: "/repo"})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if checks.dir == "" {
		t.Error("checks did not run for an empty operation list")
	}
	if !res.Passed {
		t.Error("Passed = false for a clean repository with no operations")
	}
}

func TestGate_Verify_TruncatesLargeCheckOutputAndSaysSo(t *testing.T) {
	ws := newFakeWorkspaces(t)
	huge := strings.Repeat("x", 5000)
	checks := &fakeChecks{report: verification.Report{Results: []verification.Result{
		{Check: "go test", Passed: false, Output: huge},
	}}}
	g := &Gate{Workspaces: ws, Checks: checks, MaxCheckOutput: 100}

	res, err := g.Verify(context.Background(), VerifyRequest{RepoPath: "/repo"})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(res.Checks[0].Output) >= len(huge) {
		t.Errorf("output was not truncated: %d bytes", len(res.Checks[0].Output))
	}
	if !strings.Contains(res.Checks[0].Output, "truncated") {
		t.Error("truncated output does not say it was truncated")
	}
	if len(res.Truncated) == 0 {
		t.Error("Truncated is empty; silent truncation is the failure this field prevents")
	}
}

func TestGate_Verify_TruncatesLargeDiffAndSaysSo(t *testing.T) {
	ws := newFakeWorkspaces(t)
	ws.diff = "+++ b/main.go\n" + strings.Repeat("y", 5000)
	g := &Gate{Workspaces: ws, Checks: &fakeChecks{report: passingReport()}, MaxDiff: 100}

	res, err := g.Verify(context.Background(), VerifyRequest{RepoPath: "/repo"})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(res.Diff) >= len(ws.diff) {
		t.Errorf("diff was not truncated: %d bytes", len(res.Diff))
	}
	found := false
	for _, f := range res.Truncated {
		if f == "diff" {
			found = true
		}
	}
	if !found {
		t.Errorf("Truncated = %v, want it to name the diff", res.Truncated)
	}
}

// Notes are the difference between "verified" and "nothing was checked", so
// they must survive into the result rather than being dropped as decoration.
func TestGate_Verify_CarriesNotes(t *testing.T) {
	ws := newFakeWorkspaces(t)
	checks := &fakeChecks{report: verification.Report{Results: []verification.Result{
		{Check: "pytest", Passed: true, Note: "pytest is not installed on this host; Python checks skipped"},
	}}}
	g := newGate(t, ws, checks)

	res, err := g.Verify(context.Background(), VerifyRequest{RepoPath: "/repo"})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !strings.Contains(res.Checks[0].Note, "not installed") {
		t.Errorf("Note = %q, want the skipped-check caveat carried through", res.Checks[0].Note)
	}
}

// Losing the diff makes the answer less useful; it must not make the answer
// disappear, because the check results are what was actually asked for.
func TestGate_Verify_DiffFailureDoesNotFailTheCall(t *testing.T) {
	ws := newFakeWorkspaces(t)
	ws.diffErr = errors.New("git exploded")
	g := newGate(t, ws, &fakeChecks{report: passingReport()})

	res, err := g.Verify(context.Background(), VerifyRequest{RepoPath: "/repo"})
	if err != nil {
		t.Fatalf("Verify failed because the diff could not be read: %v", err)
	}
	if !res.Passed {
		t.Error("Passed = false; a lost diff must not change the verdict")
	}
	if res.Diff != "" {
		t.Errorf("Diff = %q, want empty", res.Diff)
	}
}

func TestGate_Verify_CreateFailureIsReported(t *testing.T) {
	ws := newFakeWorkspaces(t)
	ws.createErr = errors.New("not a git repository")
	g := newGate(t, ws, &fakeChecks{report: passingReport()})

	_, err := g.Verify(context.Background(), VerifyRequest{RepoPath: "/not/a/repo"})
	if err == nil {
		t.Fatal("Verify succeeded despite the worktree not being created")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("err = %q, want the underlying cause", err)
	}
	if ws.cleanups() != 0 {
		t.Error("cleanup ran for a worktree that was never created")
	}
}

// Worktree ids must be unique per call, or two concurrent verifications would
// collide on the same path and branch.
func TestNewWorktreeID_IsUniqueAndPrefixed(t *testing.T) {
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		id := newWorktreeID()
		if !strings.HasPrefix(id, "mcp-") {
			t.Fatalf("id %q lacks the mcp- prefix that keeps these apart from job worktrees", id)
		}
		if seen[id] {
			t.Fatalf("duplicate worktree id %q", id)
		}
		seen[id] = true
	}
}

func TestChangedFiles(t *testing.T) {
	tests := []struct {
		name string
		diff string
		want []string
	}{
		{"modification", "--- a/main.go\n+++ b/main.go\n", []string{"main.go"}},
		{"deletion", "--- a/gone.go\n+++ /dev/null\n", []string{"gone.go"}},
		{"creation", "--- /dev/null\n+++ b/new.go\n", []string{"new.go"}},
		{"empty", "", nil},
		{
			"two files",
			"--- a/a.go\n+++ b/a.go\n--- a/b.go\n+++ b/b.go\n",
			[]string{"a.go", "b.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := changedFiles(tt.diff)
			if len(got) != len(tt.want) {
				t.Fatalf("changedFiles = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("changedFiles[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	if s, clipped := truncate("short", 100); clipped || s != "short" {
		t.Errorf("truncate(short) = %q, %v; want unchanged", s, clipped)
	}
	s, clipped := truncate(strings.Repeat("a", 200), 10)
	if !clipped {
		t.Error("truncate did not report clipping")
	}
	if !strings.HasPrefix(s, strings.Repeat("a", 10)) {
		t.Error("truncate did not keep the head, where the cause of a failure lives")
	}
}
