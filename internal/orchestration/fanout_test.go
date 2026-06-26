package orchestration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/execution"
	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/verification"
	"github.com/Raamia/Rojo/internal/workspace"
)

// countingWorkspaces hands out a distinct workspace per requested id and
// records what it was asked for.
type countingWorkspaces struct {
	mu       sync.Mutex
	created  []string
	cleaned  []string
	failAt   int // create fails on this call index (-1 disables)
	liveMax  int
	liveNow  int
	failWith error
}

func newCountingWorkspaces() *countingWorkspaces {
	return &countingWorkspaces{failAt: -1}
}

func (w *countingWorkspaces) Create(_ context.Context, id, repoPath string) (*workspace.Workspace, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failAt >= 0 && len(w.created) == w.failAt {
		return nil, w.failWith
	}
	w.created = append(w.created, id)
	return workspace.Reconstruct("/tmp/fanout", id, repoPath), nil
}

func (w *countingWorkspaces) Cleanup(_ context.Context, ws *workspace.Workspace) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ws != nil {
		w.cleaned = append(w.cleaned, ws.JobID)
	}
	return nil
}

func (w *countingWorkspaces) Diff(context.Context, *workspace.Workspace) (string, error) {
	return "", nil
}
func (w *countingWorkspaces) ListOrphans(context.Context, string) ([]string, error) { return nil, nil }

func (w *countingWorkspaces) snapshot() (created, cleaned []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.created...), append([]string(nil), w.cleaned...)
}

// perDirVerifier returns a different verdict per workspace path, so a test can
// decide which attempt succeeds. It also records peak concurrency.
type perDirVerifier struct {
	mu       sync.Mutex
	passing  map[string]bool // keyed by workspace dir suffix
	failWith map[string]error
	liveNow  int
	livePeak int
	delay    time.Duration
}

func (v *perDirVerifier) Verify(ctx context.Context, dir string) (verification.Report, error) {
	v.mu.Lock()
	v.liveNow++
	if v.liveNow > v.livePeak {
		v.livePeak = v.liveNow
	}
	v.mu.Unlock()

	if v.delay > 0 {
		select {
		case <-time.After(v.delay):
		case <-ctx.Done():
		}
	}

	v.mu.Lock()
	defer func() { v.liveNow--; v.mu.Unlock() }()

	base := filepath.Base(dir)
	if err, ok := v.failWith[base]; ok {
		return verification.Report{}, err
	}
	if v.passing[base] {
		return passingReport(), nil
	}
	return failingReport(), nil
}

func (v *perDirVerifier) peak() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.livePeak
}

func TestVariantCount_IsClamped(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{-5, 1}, {0, 1}, {1, 1}, {3, 3}, {MaxVariants, MaxVariants}, {MaxVariants + 10, MaxVariants},
	} {
		if got := variantCount(tc.in); got != tc.want {
			t.Errorf("variantCount(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// A single-variant job must keep the plain job ID, so its worktree path and
// branch are exactly what they have always been — startup recovery derives
// paths from the job ID and would otherwise stop matching.
func TestVariantID_SingleVariantKeepsThePlainJobID(t *testing.T) {
	if got := variantID("job-1", 0, 1); got != "job-1" {
		t.Errorf("variantID for a single variant = %q, want the plain job ID", got)
	}
	if got := variantID("job-1", 2, 5); got != "job-1-v2" {
		t.Errorf("variantID = %q, want job-1-v2", got)
	}
}

// Fan-out gives each attempt its own checkout: nothing is shared but the source
// repository, which none of them modify.
func TestProcessor_FanoutCreatesOneWorkspacePerVariant(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	ws := newCountingWorkspaces()
	v := &perDirVerifier{passing: map[string]bool{"fan-v0": true, "fan-v1": true, "fan-v2": true}}
	newQueuedJob(t, repo, "fan")

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = ws
	p.Verifier = v
	p.Variants = 3
	if err := p.Process(context.Background(), "fan"); err != nil {
		t.Fatalf("process: %v", err)
	}

	created, cleaned := ws.snapshot()
	want := []string{"fan-v0", "fan-v1", "fan-v2"}
	if len(created) != 3 {
		t.Fatalf("created %v, want %v", created, want)
	}
	for i, id := range want {
		if created[i] != id {
			t.Errorf("workspace %d = %q, want %q", i, created[i], id)
		}
	}
	if len(cleaned) != 3 {
		t.Errorf("cleaned %v, want all three reclaimed", cleaned)
	}
}

// The checks are the slow part and the attempts are independent, so they must
// actually run at the same time — otherwise fanning out just multiplies the
// wall clock.
func TestProcessor_VariantsAreVerifiedConcurrently(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	ws := newCountingWorkspaces()
	v := &perDirVerifier{
		passing: map[string]bool{"conc-v0": true, "conc-v1": true, "conc-v2": true, "conc-v3": true},
		delay:   150 * time.Millisecond,
	}
	newQueuedJob(t, repo, "conc")

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = ws
	p.Verifier = v
	p.Variants = 4

	start := time.Now()
	if err := p.Process(context.Background(), "conc"); err != nil {
		t.Fatalf("process: %v", err)
	}
	elapsed := time.Since(start)

	if peak := v.peak(); peak != 4 {
		t.Errorf("peak concurrent verifications = %d, want 4", peak)
	}
	// Sequential would be at least 4 x 150ms.
	if elapsed > 500*time.Millisecond {
		t.Errorf("four 150ms verifications took %s — they ran sequentially", elapsed)
	}
}

// Selection is by objective results, and deterministic: the lowest-indexed
// passing attempt always wins.
func TestSelectWinner_PrefersTheLowestPassingIndex(t *testing.T) {
	cands := []*candidate{
		{index: 0, ws: &workspace.Workspace{}, report: failingReport()},
		{index: 1, ws: &workspace.Workspace{}, report: passingReport()},
		{index: 2, ws: &workspace.Workspace{}, report: passingReport()},
	}
	got, ok := selectWinner(cands)
	if !ok || got.index != 1 {
		t.Fatalf("winner = %+v ok=%v, want index 1", got, ok)
	}

	if _, ok := selectWinner([]*candidate{{index: 0, report: failingReport()}}); ok {
		t.Error("a failing attempt must not be selected")
	}
	// An attempt that errored is not a viable answer even if its report is empty
	// (an empty report satisfies AllPassed).
	errored := []*candidate{{index: 0, ws: &workspace.Workspace{}, err: errors.New("boom")}}
	if _, ok := selectWinner(errored); ok {
		t.Error("an attempt that could not be verified must not be selected")
	}
}

// One passing attempt is enough for the job to succeed, even when others fail.
func TestProcessor_JobSucceedsIfAnyVariantPasses(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	bus := events.NewInProcessBus()
	sub := bus.Subscribe("mixed", 128)
	defer bus.Unsubscribe(sub)

	ws := newCountingWorkspaces()
	v := &perDirVerifier{passing: map[string]bool{"mixed-v2": true}} // only the third passes
	newQueuedJob(t, repo, "mixed")

	p := NewProcessor(repo, NewCanceller(), bus)
	p.Workspaces = ws
	p.Verifier = v
	p.Variants = 3
	if err := p.Process(context.Background(), "mixed"); err != nil {
		t.Fatalf("process: %v", err)
	}

	got, _ := repo.Get(context.Background(), "mixed")
	if got.Status != jobs.StatusCompleted {
		t.Errorf("status = %q, want completed — one passing attempt is a success", got.Status)
	}

	counts := typesOf(drain(sub))
	if counts[events.TypeVerificationCompleted] != 3 {
		t.Errorf("verification.completed emitted %d times, want one per variant", counts[events.TypeVerificationCompleted])
	}
	if counts[events.TypeFanoutCompleted] != 1 {
		t.Errorf("fanout.completed emitted %d times, want 1", counts[events.TypeFanoutCompleted])
	}
}

// If nothing passes, the gate still holds.
func TestProcessor_JobFailsWhenNoVariantPasses(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	ws := newCountingWorkspaces()
	v := &perDirVerifier{passing: map[string]bool{}}
	newQueuedJob(t, repo, "allbad")

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = ws
	p.Verifier = v
	p.Variants = 3
	err := p.Process(context.Background(), "allbad")
	if err == nil {
		t.Fatal("expected an error when no variant passes")
	}

	got, _ := repo.Get(context.Background(), "allbad")
	if got.Status != jobs.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if _, cleaned := ws.snapshot(); len(cleaned) != 3 {
		t.Errorf("cleaned %v worktrees, want all 3 reclaimed on the failure path", len(cleaned))
	}
}

// A verifier that errors on one attempt must not sink the others.
func TestProcessor_OneVariantErroringDoesNotStopTheRest(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	ws := newCountingWorkspaces()
	v := &perDirVerifier{
		passing:  map[string]bool{"partial-v1": true},
		failWith: map[string]error{"partial-v0": errors.New("toolchain missing")},
	}
	newQueuedJob(t, repo, "partial")

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = ws
	p.Verifier = v
	p.Variants = 2
	if err := p.Process(context.Background(), "partial"); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := repo.Get(context.Background(), "partial")
	if got.Status != jobs.StatusCompleted {
		t.Errorf("status = %q, want completed — variant 1 passed", got.Status)
	}
}

// A checkout that fails partway must still leave the earlier ones reclaimable.
func TestProcessor_PartialWorkspaceFailureStillCleansUp(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	ws := newCountingWorkspaces()
	ws.failAt = 2 // first two succeed, third fails
	ws.failWith = errors.New("disk full")
	newQueuedJob(t, repo, "partial-ws")

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = ws
	p.Verifier = &perDirVerifier{}
	p.Variants = 4
	if err := p.Process(context.Background(), "partial-ws"); err == nil {
		t.Fatal("expected an error when a workspace cannot be created")
	}

	created, cleaned := ws.snapshot()
	if len(created) != 2 {
		t.Fatalf("created %v, want 2 before the failure", created)
	}
	if len(cleaned) != 2 {
		t.Errorf("cleaned %v, want the 2 already-created worktrees reclaimed", cleaned)
	}
	got, _ := repo.Get(context.Background(), "partial-ws")
	if got.Status != jobs.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
}

// Default behavior is unchanged: one variant, plain job ID, one worktree.
func TestProcessor_DefaultIsASingleVariant(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	ws := newCountingWorkspaces()
	bus := events.NewInProcessBus()
	sub := bus.Subscribe("single", 64)
	defer bus.Unsubscribe(sub)

	v := &perDirVerifier{passing: map[string]bool{"single": true}}
	newQueuedJob(t, repo, "single")

	p := NewProcessor(repo, NewCanceller(), bus) // Variants left at 0
	p.Workspaces = ws
	p.Verifier = v
	if err := p.Process(context.Background(), "single"); err != nil {
		t.Fatalf("process: %v", err)
	}

	created, _ := ws.snapshot()
	if len(created) != 1 || created[0] != "single" {
		t.Fatalf("created %v, want exactly [single]", created)
	}
	// No fan-out event for an un-fanned job.
	if counts := typesOf(drain(sub)); counts[events.TypeFanoutCompleted] != 0 {
		t.Error("a single-variant job should not emit fanout.completed")
	}
}

// End to end with real git and the real toolchain.
func TestProcessor_FanoutRealGitAndToolchain(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	repoDir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module fanoutprobe\n\ngo 1.25\n")
	write("main.go", "package main\n\nfunc main() {}\n")
	write("main_test.go", "package main\n\nimport \"testing\"\n\nfunc TestOK(t *testing.T) {}\n")
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main", ".")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	run("add", ".")
	run("commit", "-m", "initial")

	base := t.TempDir()
	manager := workspace.NewGitWorkspaceManager(
		execution.NewSafeRunner(execution.NewExecRunner(), execution.NewAllowlist("git"), time.Minute), base)
	verifier := verification.NewRunner(
		execution.NewSafeRunner(execution.NewExecRunner(), execution.NewAllowlist("go", "gofmt"), 2*time.Minute))

	repo := jobs.NewInMemoryRepository()
	job := &jobs.Job{
		ID: "real-fanout", Task: "fan this out", RepoPath: repoDir,
		Status: jobs.StatusQueued, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := repo.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = manager
	p.Verifier = verifier
	p.Variants = 3
	if err := p.Process(context.Background(), "real-fanout"); err != nil {
		t.Fatalf("process: %v", err)
	}

	got, _ := repo.Get(context.Background(), "real-fanout")
	if got.Status != jobs.StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
	// Every variant worktree must be gone, and the source repo untouched.
	for i := 0; i < 3; i++ {
		path := filepath.Join(base, fmt.Sprintf("real-fanout-v%d", i))
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("variant worktree %s survived: %v", path, err)
		}
	}
	out, err := exec.Command("git", "-C", repoDir, "branch", "--list", "rojo/job/*").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Errorf("temporary branches left behind:\n%s", out)
	}
}
