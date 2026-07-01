package orchestration

import (
	"context"
	"errors"
	"testing"

	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/repocontext"
)

type fakeSelector struct {
	sel   repocontext.Context
	err   error
	calls int
}

func (f *fakeSelector) Select(_ context.Context, _, _ string) (repocontext.Context, error) {
	f.calls++
	return f.sel, f.err
}

// Selected files must actually reach the planner — that is the whole point of
// selecting them.
func TestProcessor_ContextReachesThePlanner(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	fp := &fakePlanner{plan: samplePlan()}
	sel := &fakeSelector{sel: repocontext.Context{
		Files:        []string{"ratelimit.go", "go.mod"},
		Keywords:     []string{"ratelimit"},
		TotalTracked: 12,
	}}

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Planner, p.Context = fp, sel
	newQueuedJob(t, repo, "ctx-job")

	if err := p.Process(context.Background(), "ctx-job"); err != nil {
		t.Fatalf("process: %v", err)
	}
	if sel.calls != 1 {
		t.Fatalf("Select called %d times, want 1", sel.calls)
	}
	got := fp.requests[0].Files
	if len(got) != 2 || got[0] != "ratelimit.go" {
		t.Errorf("planner received files %v, want the selected ones", got)
	}
}

// A repository that cannot be read should give the planner less to work with,
// not fail the job. The gate is what catches a plan that was too thin.
func TestProcessor_ContextFailureIsNotFatal(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	fp := &fakePlanner{plan: samplePlan()}

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Planner = fp
	p.Context = &fakeSelector{err: errors.New("not a git repository")}
	newQueuedJob(t, repo, "ctx-fail")

	if err := p.Process(context.Background(), "ctx-fail"); err != nil {
		t.Fatalf("a context failure should not fail the job: %v", err)
	}
	if fp.calls != 1 {
		t.Error("the planner should still have run")
	}
	if len(fp.requests[0].Files) != 0 {
		t.Errorf("planner got files %v despite selection failing", fp.requests[0].Files)
	}
	got, _ := repo.Get(context.Background(), "ctx-fail")
	if got.Status != jobs.StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
}

// With no selector configured the planner still runs, just without files.
func TestProcessor_NoSelectorStillPlans(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	fp := &fakePlanner{plan: samplePlan()}

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Planner = fp // Context left nil
	newQueuedJob(t, repo, "no-sel")

	if err := p.Process(context.Background(), "no-sel"); err != nil {
		t.Fatalf("process: %v", err)
	}
	if fp.calls != 1 || len(fp.requests[0].Files) != 0 {
		t.Errorf("planner calls=%d files=%v, want one call with no files", fp.calls, fp.requests[0].Files)
	}
}

// Selection costs a subprocess per keyword, so it must not run when there is
// no planner to consume it.
func TestProcessor_NoPlannerSkipsSelection(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	sel := &fakeSelector{}

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Context = sel // Planner left nil
	newQueuedJob(t, repo, "no-planner")

	if err := p.Process(context.Background(), "no-planner"); err != nil {
		t.Fatalf("process: %v", err)
	}
	if sel.calls != 0 {
		t.Errorf("Select called %d times with no planner, want 0", sel.calls)
	}
}
