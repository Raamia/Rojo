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
	"github.com/Raamia/Rojo/internal/repocontext"
	"github.com/Raamia/Rojo/internal/verification"
	"github.com/Raamia/Rojo/internal/workspace"
)

type fakeImplementor struct {
	mu       sync.Mutex
	ops      []implementor.Operation
	err      error
	failOn   int // fail the Nth call (1-indexed); 0 disables
	calls    int
	requests []implementor.Request
}

func (f *fakeImplementor) Propose(_ context.Context, req implementor.Request) ([]implementor.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, f.err
	}
	if f.failOn > 0 && f.calls == f.failOn {
		return nil, errors.New("model returned nonsense")
	}
	return f.ops, nil
}

func (f *fakeImplementor) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func writeOp(path, content string) implementor.Operation {
	return implementor.Operation{Kind: implementor.OpWrite, Path: path, Content: content}
}

// realWorkspaces gives the processor actual directories so applied changes can
// be observed on disk.
type realWorkspaces struct{ base string }

func (r *realWorkspaces) Create(_ context.Context, id, repoPath string) (*workspace.Workspace, error) {
	dir := filepath.Join(r.base, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &workspace.Workspace{JobID: id, Path: dir, RepoPath: repoPath, Branch: "rojo/job/" + id}, nil
}
func (r *realWorkspaces) Cleanup(context.Context, *workspace.Workspace) error { return nil }
func (r *realWorkspaces) Diff(context.Context, *workspace.Workspace) (string, error) {
	return "", nil
}

func TestProcessor_ImplementorWritesIntoTheWorktree(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	bus := events.NewInProcessBus()
	sub := bus.Subscribe("impl", 128)
	defer bus.Unsubscribe(sub)

	base := t.TempDir()
	impl := &fakeImplementor{ops: []implementor.Operation{writeOp("greet.go", "package main // written\n")}}

	p := NewProcessor(repo, NewCanceller(), bus)
	p.Workspaces = &realWorkspaces{base: base}
	p.Implementor = impl
	newQueuedJob(t, repo, "impl")

	if err := p.Process(context.Background(), "impl"); err != nil {
		t.Fatalf("process: %v", err)
	}
	if impl.count() != 1 {
		t.Fatalf("Propose called %d times, want 1", impl.count())
	}

	got, err := os.ReadFile(filepath.Join(base, "impl", "greet.go"))
	if err != nil {
		t.Fatalf("the change was not written: %v", err)
	}
	if !strings.Contains(string(got), "written") {
		t.Errorf("unexpected contents %q", got)
	}

	if counts := typesOf(drain(sub)); counts[events.TypeImplementationCompleted] != 1 {
		t.Errorf("implementation.completed emitted %d times, want 1", counts[events.TypeImplementationCompleted])
	}
}

// Each variant gets its own proposal — that is what makes fan-out produce
// genuinely different candidates rather than the same patch N times.
func TestProcessor_EachVariantGetsItsOwnProposal(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	base := t.TempDir()
	impl := &fakeImplementor{ops: []implementor.Operation{writeOp("a.go", "package a\n")}}

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = &realWorkspaces{base: base}
	p.Implementor = impl
	p.Variants = 3
	newQueuedJob(t, repo, "fanimpl")

	if err := p.Process(context.Background(), "fanimpl"); err != nil {
		t.Fatalf("process: %v", err)
	}
	if impl.count() != 3 {
		t.Errorf("Propose called %d times, want one per variant", impl.count())
	}
	for i := 0; i < 3; i++ {
		path := filepath.Join(base, "fanimpl-v"+string(rune('0'+i)), "a.go")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("variant %d was not modified: %v", i, err)
		}
	}
}

// One bad sample must not sink the attempt: the surviving variants still run.
func TestProcessor_OneFailedProposalDoesNotFailTheJob(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	base := t.TempDir()
	impl := &fakeImplementor{
		ops:    []implementor.Operation{writeOp("a.go", "package a\n")},
		failOn: 1, // the first variant's proposal fails
	}

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = &realWorkspaces{base: base}
	p.Implementor = impl
	p.Verifier = &alwaysPass{}
	p.Variants = 3
	newQueuedJob(t, repo, "partial")

	if err := p.Process(context.Background(), "partial"); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := repo.Get(context.Background(), "partial")
	if got.Status != jobs.StatusCompleted {
		t.Errorf("status = %q, want completed — two variants succeeded", got.Status)
	}
}

type alwaysPass struct{}

func (alwaysPass) Verify(context.Context, string) (verification.Report, error) {
	return verification.Report{Results: []verification.Result{{Check: "go test", Passed: true}}}, nil
}

// If no variant could be implemented there is nothing to verify, so the job
// must fail rather than "passing" checks against an unmodified checkout.
func TestProcessor_AllProposalsFailingFailsTheJob(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	boom := errors.New("model unavailable")
	impl := &fakeImplementor{err: boom}

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = &realWorkspaces{base: t.TempDir()}
	p.Implementor = impl
	newQueuedJob(t, repo, "allfail")

	err := p.Process(context.Background(), "allfail")
	if err == nil {
		t.Fatal("expected an error when nothing could be implemented")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error %v should carry the underlying cause", err)
	}
	got, _ := repo.Get(context.Background(), "allfail")
	if got.Status != jobs.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
}

// The implementor must see the files as they exist in its own worktree — that
// is the copy it is editing.
func TestProcessor_ImplementorSeesWorktreeContents(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	base := t.TempDir()

	impl := &fakeImplementor{ops: []implementor.Operation{writeOp("out.go", "package main\n")}}
	seeded := &seedingWorkspaces{inner: &realWorkspaces{base: base}, file: "existing.go", body: "package main // ORIGINAL"}

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = seeded
	p.Implementor = impl
	p.Context = &fakeSelector{sel: contextWith("existing.go")}
	p.Planner = &fakePlanner{plan: samplePlan()}
	newQueuedJob(t, repo, "sees")

	if err := p.Process(context.Background(), "sees"); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(impl.requests) == 0 {
		t.Fatal("implementor was never called")
	}
	files := impl.requests[0].Files
	if len(files) != 1 || files[0].Path != "existing.go" {
		t.Fatalf("implementor saw %+v, want existing.go", files)
	}
	if !strings.Contains(files[0].Content, "ORIGINAL") {
		t.Errorf("implementor saw %q, want the worktree contents", files[0].Content)
	}
}

type seedingWorkspaces struct {
	inner      workspace.WorkspaceManager
	file, body string
}

func (s *seedingWorkspaces) Create(ctx context.Context, id, repoPath string) (*workspace.Workspace, error) {
	ws, err := s.inner.Create(ctx, id, repoPath)
	if err != nil {
		return nil, err
	}
	return ws, os.WriteFile(filepath.Join(ws.Path, s.file), []byte(s.body), 0o644)
}
func (s *seedingWorkspaces) Cleanup(ctx context.Context, ws *workspace.Workspace) error {
	return s.inner.Cleanup(ctx, ws)
}
func (s *seedingWorkspaces) Diff(ctx context.Context, ws *workspace.Workspace) (string, error) {
	return s.inner.Diff(ctx, ws)
}

func contextWith(files ...string) repocontext.Context {
	return repocontext.Context{Files: files, TotalTracked: len(files)}
}

// End to end with real git and the real toolchain: a change is written into
// the worktree, the gate runs against the modified code, and the source
// repository is left untouched.
func TestProcessor_ImplementorEndToEnd(t *testing.T) {
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
	write("go.mod", "module implprobe\n\ngo 1.25\n")
	write("main.go", "package main\n\nfunc main() {}\n")
	write("main_test.go", "package main\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif Greet() != \"hi\" {\n\t\tt.Fatal(\"bad greeting\")\n\t}\n}\n")
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

	// The repository does not compile yet: Greet is referenced but undefined.
	// The implementor supplies it, which is what makes the gate go green.
	impl := &fakeImplementor{ops: []implementor.Operation{
		writeOp("greet.go", "package main\n\nfunc Greet() string { return \"hi\" }\n"),
	}}

	base := t.TempDir()
	gitRunner := execution.NewSafeRunner(execution.NewExecRunner(), execution.NewAllowlist("git"), time.Minute)
	verifyRunner := execution.NewSafeRunner(execution.NewExecRunner(), execution.NewAllowlist("go", "gofmt"), 2*time.Minute)

	repo := jobs.NewInMemoryRepository()
	job := &jobs.Job{
		ID: "e2e-impl", Task: "add a Greet function", RepoPath: repoDir,
		Status: jobs.StatusQueued, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := repo.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = workspace.NewGitWorkspaceManager(gitRunner, base)
	p.Implementor = impl
	p.Verifier = verification.NewRunner(verifyRunner)

	if err := p.Process(context.Background(), "e2e-impl"); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := repo.Get(context.Background(), "e2e-impl")
	if got.Status != jobs.StatusCompleted {
		t.Fatalf("status = %q, want completed — the implementor's change should make the tests pass", got.Status)
	}

	// The source repository must be untouched: the change lived and died in
	// the worktree.
	if _, err := os.Stat(filepath.Join(repoDir, "greet.go")); !os.IsNotExist(err) {
		t.Error("the implementor's file leaked into the source repository")
	}
	if _, err := os.Stat(filepath.Join(base, "e2e-impl")); !os.IsNotExist(err) {
		t.Error("the worktree was not cleaned up")
	}
}
