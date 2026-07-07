package orchestration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Raamia/Rojo/internal/agents/implementor"
	"github.com/Raamia/Rojo/internal/agents/planner"
	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/verification"
	"github.com/Raamia/Rojo/internal/workspace"
)

// A stage that panics must not end the process. Without recovery the panic
// unwinds through the worker goroutine and kills the service, taking every
// other in-flight job with it — one malformed model response would be enough.
func TestProcess_PanicBecomesAFailedJob(t *testing.T) {
	stages := map[string]func(*Processor){
		"planner":     func(p *Processor) { p.Planner = panicPlanner{} },
		"implementor": func(p *Processor) { p.Implementor = panicImplementor{} },
		"verifier":    func(p *Processor) { p.Verifier = panicVerifier{} },
		"workspaces":  func(p *Processor) { p.Workspaces = panicWorkspaces{} },
	}
	for name, configure := range stages {
		t.Run(name, func(t *testing.T) {
			repo := jobs.NewInMemoryRepository()
			p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
			p.Workspaces = &diffingWorkspaces{base: t.TempDir(), diffs: map[int]string{0: sampleDiff("x")}}
			p.Implementor = &fakeImplementor{ops: []implementor.Operation{writeOp("a.go", "package a\n")}}
			configure(p)
			newQueuedJob(t, repo, "boom")

			err := p.Process(context.Background(), "boom")
			if !errors.Is(err, ErrPanic) {
				t.Fatalf("got %v, want ErrPanic", err)
			}
			if !strings.Contains(err.Error(), "deliberate") {
				t.Errorf("error %q should carry what the panic said", err)
			}
			got, _ := repo.Get(context.Background(), "boom")
			if got.Status != jobs.StatusFailed {
				t.Errorf("status = %q, want failed — a panicked job must not linger", got.Status)
			}
		})
	}
}

// The worktree still has to go. Cleanup is deferred after the recovery, so it
// runs first on the way out; a panic must not leak a checkout.
func TestProcess_PanicStillCleansUpTheWorktree(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	cleaned := make(chan string, 4)

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = &recordingCleanup{
		inner:   &diffingWorkspaces{base: t.TempDir(), diffs: map[int]string{0: ""}},
		cleaned: cleaned,
	}
	p.Implementor = panicImplementor{}
	newQueuedJob(t, repo, "leak")

	if err := p.Process(context.Background(), "leak"); !errors.Is(err, ErrPanic) {
		t.Fatalf("got %v, want ErrPanic", err)
	}
	select {
	case <-cleaned:
	default:
		t.Error("the worktree was never cleaned up after the panic")
	}
}

type recordingCleanup struct {
	inner   workspace.WorkspaceManager
	cleaned chan string
}

func (r *recordingCleanup) Create(ctx context.Context, id, repoPath string) (*workspace.Workspace, error) {
	return r.inner.Create(ctx, id, repoPath)
}
func (r *recordingCleanup) Cleanup(ctx context.Context, ws *workspace.Workspace) error {
	select {
	case r.cleaned <- ws.Path:
	default:
	}
	return r.inner.Cleanup(ctx, ws)
}
func (r *recordingCleanup) Diff(ctx context.Context, ws *workspace.Workspace) (string, error) {
	return r.inner.Diff(ctx, ws)
}

// Other jobs must be unaffected: recovery is what makes one bad job a bad job
// rather than an outage.
func TestProcess_PanicDoesNotAffectOtherJobs(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = &diffingWorkspaces{base: t.TempDir(), diffs: map[int]string{0: sampleDiff("x")}}
	p.Verifier = &panicOnce{}
	newQueuedJob(t, repo, "first")
	newQueuedJob(t, repo, "second")

	if err := p.Process(context.Background(), "first"); !errors.Is(err, ErrPanic) {
		t.Fatalf("first job: got %v, want ErrPanic", err)
	}
	if err := p.Process(context.Background(), "second"); err != nil {
		t.Fatalf("second job should be unaffected: %v", err)
	}
	if got, _ := repo.Get(context.Background(), "second"); got.Status != jobs.StatusCompleted {
		t.Errorf("second job status = %q, want completed", got.Status)
	}
}

type panicOnce struct {
	mu sync.Mutex
	n  int
}

func (p *panicOnce) Verify(context.Context, string) (verification.Report, error) {
	p.mu.Lock()
	p.n++
	first := p.n == 1
	p.mu.Unlock()
	if first {
		panic("deliberate: only the first job")
	}
	return verification.Report{Results: []verification.Result{{Check: "go test", Passed: true}}}, nil
}

type panicPlanner struct{}

func (panicPlanner) Plan(context.Context, planner.Request) (planner.Plan, error) {
	panic("deliberate: planner exploded")
}

type panicImplementor struct{}

func (panicImplementor) Propose(context.Context, implementor.Request) ([]implementor.Operation, error) {
	panic("deliberate: implementor exploded")
}

type panicVerifier struct{}

func (panicVerifier) Verify(context.Context, string) (verification.Report, error) {
	panic("deliberate: verifier exploded")
}

type panicWorkspaces struct{}

func (panicWorkspaces) Create(context.Context, string, string) (*workspace.Workspace, error) {
	panic("deliberate: workspace exploded")
}
func (panicWorkspaces) Cleanup(context.Context, *workspace.Workspace) error { return nil }
func (panicWorkspaces) Diff(context.Context, *workspace.Workspace) (string, error) {
	return "", nil
}
