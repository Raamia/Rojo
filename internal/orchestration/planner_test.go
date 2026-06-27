package orchestration

import (
	"context"
	"errors"
	"testing"

	"github.com/Raamia/Rojo/internal/agents/planner"
	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
)

type fakePlanner struct {
	plan     planner.Plan
	err      error
	calls    int
	requests []planner.Request
}

func (f *fakePlanner) Plan(_ context.Context, req planner.Request) (planner.Plan, error) {
	f.calls++
	f.requests = append(f.requests, req)
	return f.plan, f.err
}

func samplePlan() planner.Plan {
	return planner.Plan{
		Summary: "add the endpoint",
		Steps: []planner.Step{
			{ID: "s1", Description: "write the handler", Files: []string{"main.go"}},
		},
	}
}

func TestProcessor_PlanningStepRunsAndEmits(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	bus := events.NewInProcessBus()
	sub := bus.Subscribe("plan-job", 64)
	defer bus.Unsubscribe(sub)

	p := NewProcessor(repo, NewCanceller(), bus)
	fp := &fakePlanner{plan: samplePlan()}
	p.Planner = fp
	newQueuedJob(t, repo, "plan-job")

	if err := p.Process(context.Background(), "plan-job"); err != nil {
		t.Fatalf("process: %v", err)
	}

	if fp.calls != 1 {
		t.Fatalf("Plan called %d times, want 1", fp.calls)
	}
	// The planner must receive the job's own task and repository, not defaults.
	got := fp.requests[0]
	if got.Task == "" || got.RepoPath == "" {
		t.Errorf("planner received %+v, want the job's task and repo path", got)
	}

	if counts := typesOf(drain(sub)); counts[events.TypePlanCreated] != 1 {
		t.Errorf("plan.created emitted %d times, want 1", counts[events.TypePlanCreated])
	}

	final, _ := repo.Get(context.Background(), "plan-job")
	if final.Status != jobs.StatusCompleted {
		t.Errorf("status = %q, want completed", final.Status)
	}
}

// A model that is unreachable, rate-limited, or returning unusable output is a
// real failure — the job must reach a terminal state rather than proceeding to
// implement work nobody planned.
func TestProcessor_PlannerFailureFailsTheJob(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	bus := events.NewInProcessBus()
	sub := bus.Subscribe("plan-fail", 64)
	defer bus.Unsubscribe(sub)

	boom := errors.New("anthropic rate limited after retries")
	p := NewProcessor(repo, NewCanceller(), bus)
	p.Planner = &fakePlanner{err: boom}
	newQueuedJob(t, repo, "plan-fail")

	err := p.Process(context.Background(), "plan-fail")
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the underlying cause", err)
	}

	final, _ := repo.Get(context.Background(), "plan-fail")
	if final.Status != jobs.StatusFailed {
		t.Errorf("status = %q, want failed", final.Status)
	}

	counts := typesOf(drain(sub))
	if counts[events.TypeJobFailed] != 1 {
		t.Errorf("job.failed emitted %d times, want 1", counts[events.TypeJobFailed])
	}
	// Nothing should proceed past a failed plan.
	if counts[events.TypeWorkspaceCreated] != 0 {
		t.Error("a workspace was created despite planning failing")
	}
	if counts[events.TypeJobCompleted] != 0 {
		t.Error("a job with no plan reported completion")
	}
}

// With no model configured the pipeline still isolates, verifies and reports —
// an unset key degrades the service rather than breaking it.
func TestProcessor_NoPlannerStillCompletes(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	bus := events.NewInProcessBus()
	sub := bus.Subscribe("no-plan", 64)
	defer bus.Unsubscribe(sub)

	p := NewProcessor(repo, NewCanceller(), bus) // Planner left nil
	newQueuedJob(t, repo, "no-plan")

	if err := p.Process(context.Background(), "no-plan"); err != nil {
		t.Fatalf("process: %v", err)
	}
	final, _ := repo.Get(context.Background(), "no-plan")
	if final.Status != jobs.StatusCompleted {
		t.Errorf("status = %q, want completed", final.Status)
	}
	if counts := typesOf(drain(sub)); counts[events.TypePlanCreated] != 0 {
		t.Error("plan.created emitted with no planner configured")
	}
}

// Planning happens before the workspace exists, so a failure there must not
// leave a worktree behind either.
func TestProcessor_PlannerFailureLeavesNoWorkspace(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	ws := newFakeWorkspaces()

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Planner = &fakePlanner{err: errors.New("model unavailable")}
	p.Workspaces = ws
	newQueuedJob(t, repo, "plan-before-ws")

	if err := p.Process(context.Background(), "plan-before-ws"); err == nil {
		t.Fatal("expected an error")
	}
	create, cleanup := ws.counts()
	if create != 0 || cleanup != 0 {
		t.Errorf("workspace create=%d cleanup=%d, want no workspace at all", create, cleanup)
	}
}
