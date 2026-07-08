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

	"github.com/Raamia/Rojo/internal/agents/implementor"
	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/execution"
	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/verification"
	"github.com/Raamia/Rojo/internal/workspace"
)

// memArtifacts is an in-memory ArtifactStore that also records the order of
// writes, so tests can assert *when* a diff was captured relative to cleanup.
type memArtifacts struct {
	mu    sync.Mutex
	files map[string][]byte
	err   error
	order *[]string
}

func newArtifacts() *memArtifacts { return &memArtifacts{files: map[string][]byte{}} }

func (m *memArtifacts) WriteArtifact(jobID, name string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.order != nil {
		*m.order = append(*m.order, "write:"+name)
	}
	if m.err != nil {
		return m.err
	}
	m.files[jobID+"/"+name] = data
	return nil
}

func (m *memArtifacts) get(jobID, name string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[jobID+"/"+name]
	return string(b), ok
}

// diffingWorkspaces returns a canned diff per variant, so tests can tell which
// attempt's patch was saved.
type diffingWorkspaces struct {
	base  string
	diffs map[int]string
	order *[]string
	mu    sync.Mutex
	made  map[string]int // path -> variant index
}

func (d *diffingWorkspaces) Create(_ context.Context, id, repoPath string) (*workspace.Workspace, error) {
	dir := filepath.Join(d.base, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	d.mu.Lock()
	if d.made == nil {
		d.made = map[string]int{}
	}
	d.made[dir] = len(d.made)
	d.mu.Unlock()
	return &workspace.Workspace{JobID: id, Path: dir, RepoPath: repoPath, Branch: "rojo/job/" + id}, nil
}

func (d *diffingWorkspaces) Cleanup(_ context.Context, ws *workspace.Workspace) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.order != nil {
		*d.order = append(*d.order, "cleanup:"+filepath.Base(ws.Path))
	}
	return os.RemoveAll(ws.Path)
}

func (d *diffingWorkspaces) Diff(_ context.Context, ws *workspace.Workspace) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.order != nil {
		*d.order = append(*d.order, "diff:"+filepath.Base(ws.Path))
	}
	// Reading the recorded creation order keeps the mapping stable even though
	// worktree paths are generated.
	return d.diffs[d.made[ws.Path]], nil
}

func sampleDiff(marker string) string {
	return "diff --git a/greet.go b/greet.go\n" +
		"--- a/greet.go\n+++ b/greet.go\n" +
		"@@ -1 +1,2 @@\n package main\n+// " + marker + "\n"
}

func withImplementor(p *Processor) {
	p.Implementor = &fakeImplementor{ops: []implementor.Operation{writeOp("greet.go", "package main\n")}}
}

func TestCaptureDiff_PassingJobStoresThePatch(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	arts := newArtifacts()
	bus := events.NewInProcessBus()
	sub := bus.Subscribe("patch", 128)
	defer bus.Unsubscribe(sub)

	p := NewProcessor(repo, NewCanceller(), bus)
	p.Workspaces = &diffingWorkspaces{base: t.TempDir(), diffs: map[int]string{0: sampleDiff("winner")}}
	p.Verifier = &alwaysPass{}
	p.Artifacts = arts
	withImplementor(p)
	newQueuedJob(t, repo, "patch")

	if err := p.Process(context.Background(), "patch"); err != nil {
		t.Fatalf("process: %v", err)
	}

	got, ok := arts.get("patch", ArtifactDiff)
	if !ok {
		t.Fatal("no patch was stored for a completed job")
	}
	if !strings.Contains(got, "winner") {
		t.Errorf("stored patch = %q, want the winning variant's diff", got)
	}
	if counts := typesOf(drain(sub)); counts[events.TypeDiffCaptured] != 1 {
		t.Errorf("diff.captured emitted %d times, want 1", counts[events.TypeDiffCaptured])
	}
}

// The most useful thing a failed job can hand back is the patch that failed.
// Reporting only "tests did not pass" leaves nothing to inspect or fix.
func TestCaptureDiff_FailedJobStillStoresThePatch(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	arts := newArtifacts()

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = &diffingWorkspaces{base: t.TempDir(), diffs: map[int]string{0: sampleDiff("rejected")}}
	p.Verifier = &alwaysFail{}
	p.Artifacts = arts
	withImplementor(p)
	newQueuedJob(t, repo, "bad")

	if err := p.Process(context.Background(), "bad"); err == nil {
		t.Fatal("expected the job to fail its gate")
	}
	got, ok := arts.get("bad", ArtifactDiff)
	if !ok {
		t.Fatal("a failed job stored no patch; there is nothing for a human to look at")
	}
	if !strings.Contains(got, "rejected") {
		t.Errorf("stored patch = %q", got)
	}
	if job, _ := repo.Get(context.Background(), "bad"); job.Status != jobs.StatusFailed {
		t.Errorf("status = %q, want failed — a saved patch must not imply approval", job.Status)
	}
}

type alwaysFail struct{}

func (alwaysFail) Verify(context.Context, string) (verification.Report, error) {
	return verification.Report{Results: []verification.Result{
		{Check: "go test", Passed: false, Output: "FAIL"},
	}}, nil
}

// The patch has to be read while the worktree is alive: cleanup deletes it.
func TestCaptureDiff_HappensBeforeCleanup(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	var order []string
	arts := newArtifacts()
	arts.order = &order

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = &diffingWorkspaces{
		base: t.TempDir(), diffs: map[int]string{0: sampleDiff("x")}, order: &order,
	}
	p.Verifier = &alwaysPass{}
	p.Artifacts = arts
	withImplementor(p)
	newQueuedJob(t, repo, "order")

	if err := p.Process(context.Background(), "order"); err != nil {
		t.Fatalf("process: %v", err)
	}

	joined := strings.Join(order, ",")
	diffAt, cleanupAt := indexOfPrefix(order, "diff:"), indexOfPrefix(order, "cleanup:")
	if diffAt < 0 || cleanupAt < 0 {
		t.Fatalf("expected both a diff and a cleanup, got %s", joined)
	}
	if diffAt > cleanupAt {
		t.Errorf("diff was read after cleanup removed the worktree: %s", joined)
	}
}

func indexOfPrefix(all []string, prefix string) int {
	for i, s := range all {
		if strings.HasPrefix(s, prefix) {
			return i
		}
	}
	return -1
}

// Event payloads are appended to the log and fanned out to every subscriber
// through a bounded buffer. Putting a patch in one would evict other events and
// disconnect slow clients, so the event carries the diff's shape only.
func TestCaptureDiff_EventCarriesShapeNotBody(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	bus := events.NewInProcessBus()
	sub := bus.Subscribe("shape", 128)
	defer bus.Unsubscribe(sub)

	body := sampleDiff("SECRET_MARKER")
	p := NewProcessor(repo, NewCanceller(), bus)
	p.Workspaces = &diffingWorkspaces{base: t.TempDir(), diffs: map[int]string{0: body}}
	p.Verifier = &alwaysPass{}
	p.Artifacts = newArtifacts()
	withImplementor(p)
	newQueuedJob(t, repo, "shape")

	if err := p.Process(context.Background(), "shape"); err != nil {
		t.Fatalf("process: %v", err)
	}

	var found bool
	for _, e := range drain(sub) {
		if e.Type != events.TypeDiffCaptured {
			continue
		}
		found = true
		if n, _ := e.Payload["bytes"].(int); n != len(body) {
			t.Errorf("bytes = %v, want %d", e.Payload["bytes"], len(body))
		}
		files, _ := e.Payload["files"].([]string)
		if len(files) != 1 || files[0] != "greet.go" {
			t.Errorf("files = %v, want [greet.go]", e.Payload["files"])
		}
		for k, v := range e.Payload {
			if s, ok := v.(string); ok && strings.Contains(s, "SECRET_MARKER") {
				t.Errorf("payload field %q carries the diff body", k)
			}
		}
	}
	if !found {
		t.Error("no diff.captured event")
	}
}

// "The model produced no change" and "the diff was lost" look identical from
// outside unless the empty case is recorded on purpose.
func TestCaptureDiff_EmptyDiffWritesNoArtifactButIsReported(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	arts := newArtifacts()
	bus := events.NewInProcessBus()
	sub := bus.Subscribe("nochange", 64)
	defer bus.Unsubscribe(sub)

	p := NewProcessor(repo, NewCanceller(), bus)
	p.Workspaces = &diffingWorkspaces{base: t.TempDir(), diffs: map[int]string{0: "  \n"}}
	p.Verifier = &alwaysPass{}
	p.Artifacts = arts
	withImplementor(p)
	newQueuedJob(t, repo, "nochange")

	if err := p.Process(context.Background(), "nochange"); err != nil {
		t.Fatalf("process: %v", err)
	}
	if _, ok := arts.get("nochange", ArtifactDiff); ok {
		t.Error("an empty diff should not be stored as a patch")
	}
	var reported bool
	for _, e := range drain(sub) {
		if e.Type == events.TypeDiffCaptured && e.Payload["empty"] == true {
			reported = true
		}
	}
	if !reported {
		t.Error("an empty diff was not reported")
	}
}

// Work that was done and verified stays done. Losing the artifact is a storage
// problem, not grounds for retracting a passing result.
func TestCaptureDiff_StoreFailureDoesNotFailTheJob(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	arts := newArtifacts()
	arts.err = errors.New("disk full")

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = &diffingWorkspaces{base: t.TempDir(), diffs: map[int]string{0: sampleDiff("x")}}
	p.Verifier = &alwaysPass{}
	p.Artifacts = arts
	withImplementor(p)
	newQueuedJob(t, repo, "diskfull")

	if err := p.Process(context.Background(), "diskfull"); err != nil {
		t.Fatalf("a failed artifact write must not fail the job: %v", err)
	}
	if job, _ := repo.Get(context.Background(), "diskfull"); job.Status != jobs.StatusCompleted {
		t.Errorf("status = %q, want completed", job.Status)
	}
}

// With no store configured the pipeline still runs end to end; the patch is
// simply discarded along with the worktree.
func TestCaptureDiff_NoStoreConfiguredIsHarmless(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = &diffingWorkspaces{base: t.TempDir(), diffs: map[int]string{0: sampleDiff("x")}}
	p.Verifier = &alwaysPass{}
	withImplementor(p)
	newQueuedJob(t, repo, "nostore")

	if err := p.Process(context.Background(), "nostore"); err != nil {
		t.Fatalf("process: %v", err)
	}
}

// The winning variant's patch is the one to keep — saving a losing attempt's
// diff would hand back code that does not pass the checks.
func TestCaptureDiff_StoresTheWinningVariant(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	arts := newArtifacts()

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = &diffingWorkspaces{
		base:  t.TempDir(),
		diffs: map[int]string{0: sampleDiff("loser"), 1: sampleDiff("champion")},
	}
	p.Verifier = &passOnlyVariant{pass: 1}
	p.Artifacts = arts
	p.Variants = 2
	withImplementor(p)
	newQueuedJob(t, repo, "fan")

	if err := p.Process(context.Background(), "fan"); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, ok := arts.get("fan", ArtifactDiff)
	if !ok {
		t.Fatal("no patch stored")
	}
	if !strings.Contains(got, "champion") {
		t.Errorf("stored the wrong variant's patch: %q", got)
	}
}

// passOnlyVariant passes exactly one attempt, identified by the "-v<N>" suffix
// the fan-out gives its worktrees.
type passOnlyVariant struct{ pass int }

func (p *passOnlyVariant) Verify(_ context.Context, dir string) (verification.Report, error) {
	ok := strings.HasSuffix(dir, "-v"+string(rune('0'+p.pass)))
	return verification.Report{Results: []verification.Result{{Check: "go test", Passed: ok}}}, nil
}

// newGitRepo builds a committed repository so a worktree can be cut from it.
func newGitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-b", "main", "."},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"add", "."},
		{"commit", "-m", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestChangedFiles(t *testing.T) {
	tests := map[string]struct {
		diff string
		want []string
	}{
		"single file": {sampleDiff("x"), []string{"greet.go"}},
		"two files": {
			"--- a/one.go\n+++ b/one.go\n@@\n--- a/two.go\n+++ b/two.go\n@@\n",
			[]string{"one.go", "two.go"},
		},
		"deletion falls back to the pre-image": {
			"--- a/gone.go\n+++ /dev/null\n@@\n",
			[]string{"gone.go"},
		},
		"new file": {
			"--- /dev/null\n+++ b/fresh.go\n@@\n",
			[]string{"fresh.go"},
		},
		"deduped":  {"--- a/x.go\n+++ b/x.go\n--- a/x.go\n+++ b/x.go\n", []string{"x.go"}},
		"no diff":  {"", nil},
		"no paths": {"just some prose\n", nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := changedFiles(tt.diff); strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("changedFiles = %v, want %v", got, tt.want)
			}
		})
	}
}

// The point of all of this: the stored artifact must be a patch that `git apply`
// accepts. Anything less is a report, not a deliverable.
func TestCaptureDiff_ArtifactIsApplyable(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	files := map[string]string{
		"go.mod":  "module probe\n\ngo 1.25\n",
		"main.go": "package main\n\nfunc main() {}\n",
	}
	source := newGitRepo(t, files)

	repo := jobs.NewInMemoryRepository()
	arts := newArtifacts()
	gitRunner := execution.NewSafeRunner(
		execution.NewExecRunner(), execution.NewAllowlist("git"), time.Minute)

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = workspace.NewGitWorkspaceManager(gitRunner, t.TempDir())
	p.Artifacts = arts
	p.Implementor = &fakeImplementor{ops: []implementor.Operation{
		writeOp("greet.go", "package main\n\nfunc Greet() string { return \"hi\" }\n"),
	}}
	job := newQueuedJob(t, repo, "applyable")
	job.RepoPath = source
	if err := repo.Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}

	if err := p.Process(context.Background(), "applyable"); err != nil {
		t.Fatalf("process: %v", err)
	}
	patch, ok := arts.get("applyable", ArtifactDiff)
	if !ok {
		t.Fatal("no patch stored")
	}

	// Apply it to a clean copy of the source. If it applies there, it is a
	// genuine patch rather than a formatted summary.
	clone := newGitRepo(t, files)
	cmd := exec.Command("git", "apply", "-")
	cmd.Dir = clone
	cmd.Stdin = strings.NewReader(patch)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git apply rejected the stored patch: %v\n%s\n--- patch ---\n%s", err, out, patch)
	}
	got, err := os.ReadFile(filepath.Join(clone, "greet.go"))
	if err != nil {
		t.Fatalf("the patch did not create the file: %v", err)
	}
	if !strings.Contains(string(got), "func Greet()") {
		t.Errorf("applied file = %q", got)
	}
}

// A variant whose proposal failed was never modified. Verifying it means
// running the checks against a pristine checkout, which of course passes — and
// if it can then win, the job reports success while handing back nothing.
func TestVerify_UnimplementedVariantCannotWin(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	arts := newArtifacts()

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = &diffingWorkspaces{
		base: t.TempDir(),
		// Variant 0 is never implemented, so its worktree is unchanged.
		diffs: map[int]string{0: "", 1: sampleDiff("real work"), 2: sampleDiff("real work")},
	}
	p.Verifier = &alwaysPass{} // a pristine checkout passes too
	p.Artifacts = arts
	p.Variants = 3
	p.Implementor = &fakeImplementor{
		ops:    []implementor.Operation{writeOp("greet.go", "package main\n")},
		failOn: 1, // variant 0's proposal fails
	}
	newQueuedJob(t, repo, "unimpl")

	if err := p.Process(context.Background(), "unimpl"); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, ok := arts.get("unimpl", ArtifactDiff)
	if !ok || !strings.Contains(got, "real work") {
		t.Errorf("job completed with patch %q; an unimplemented variant won", got)
	}
}

// The patch is most worth having when a job did not finish cleanly. A job that
// runs out of time reaches capture with its context already dead, so reading
// the diff through that context would lose the artifact in exactly the case
// where seeing what the job was doing matters most.
func TestCaptureDiff_SurvivesATimedOutJob(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	arts := newArtifacts()

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = &ctxCheckingWorkspaces{
		inner: &diffingWorkspaces{base: t.TempDir(), diffs: map[int]string{0: sampleDiff("interrupted")}},
	}
	p.Implementor = &fakeImplementor{ops: []implementor.Operation{writeOp("a.go", "package a\n")}}
	// The deadline expires while the checks are running, which is the realistic
	// shape: the work happened, the clock ran out mid-verification.
	p.Verifier = &blockingVerifier{}
	p.JobTimeout = 300 * time.Millisecond
	p.Artifacts = arts
	newQueuedJob(t, repo, "timedout")

	if err := p.Process(context.Background(), "timedout"); err == nil {
		t.Fatal("expected the job to fail on its deadline")
	}
	got, ok := arts.get("timedout", ArtifactDiff)
	if !ok || !strings.Contains(got, "interrupted") {
		t.Errorf("the patch was lost because the job's context was dead: got %q", got)
	}
}

// ctxCheckingWorkspaces refuses to diff on a cancelled context, the way a real
// command runner does.
type ctxCheckingWorkspaces struct{ inner workspace.WorkspaceManager }

func (c *ctxCheckingWorkspaces) Create(ctx context.Context, id, repoPath string) (*workspace.Workspace, error) {
	return c.inner.Create(ctx, id, repoPath)
}
func (c *ctxCheckingWorkspaces) Cleanup(ctx context.Context, ws *workspace.Workspace) error {
	return c.inner.Cleanup(ctx, ws)
}
func (c *ctxCheckingWorkspaces) Diff(ctx context.Context, ws *workspace.Workspace) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return c.inner.Diff(ctx, ws)
}
