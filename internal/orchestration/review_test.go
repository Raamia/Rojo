package orchestration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Raamia/Rojo/internal/agents/implementor"
	"github.com/Raamia/Rojo/internal/agents/reviewer"
	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/verification"
)

type fakeReviewer struct {
	mu        sync.Mutex
	decisions []reviewer.Review // consumed in order; the last one repeats
	err       error
	requests  []reviewer.Request
}

func (f *fakeReviewer) Review(_ context.Context, req reviewer.Request) (reviewer.Review, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if f.err != nil {
		return reviewer.Review{}, f.err
	}
	i := len(f.requests) - 1
	if i >= len(f.decisions) {
		i = len(f.decisions) - 1
	}
	return f.decisions[i], nil
}

func (f *fakeReviewer) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func approve() reviewer.Review {
	return reviewer.Review{Decision: reviewer.DecisionApprove, Notes: "looks right"}
}
func requestChanges(notes string) reviewer.Review {
	return reviewer.Review{Decision: reviewer.DecisionRequestChanges, Notes: notes}
}

// reviewProcessor wires the full plan-free pipeline: workspaces, an
// implementor, a verifier and a reviewer.
func reviewProcessor(t *testing.T, repo jobs.JobRepository, bus events.Bus) (*Processor, *fakeImplementor) {
	t.Helper()
	impl := &fakeImplementor{ops: []implementor.Operation{writeOp("greet.go", "package main\n")}}
	p := NewProcessor(repo, NewCanceller(), bus)
	p.Workspaces = &diffingWorkspaces{base: t.TempDir(), diffs: map[int]string{0: sampleDiff("x")}}
	p.Implementor = impl
	p.Verifier = &alwaysPass{}
	return p, impl
}

func TestReview_ApprovalCompletesTheJob(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	bus := events.NewInProcessBus()
	sub := bus.Subscribe("ok", 128)
	defer bus.Unsubscribe(sub)

	p, impl := reviewProcessor(t, repo, bus)
	rev := &fakeReviewer{decisions: []reviewer.Review{approve()}}
	p.Reviewer = rev
	newQueuedJob(t, repo, "ok")

	if err := p.Process(context.Background(), "ok"); err != nil {
		t.Fatalf("process: %v", err)
	}
	if got, _ := repo.Get(context.Background(), "ok"); got.Status != jobs.StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
	if rev.calls() != 1 || impl.count() != 1 {
		t.Errorf("review calls = %d, implement calls = %d; want one pass each", rev.calls(), impl.count())
	}
	counts := typesOf(drain(sub))
	if counts[events.TypeReviewCompleted] != 1 {
		t.Errorf("review.completed = %d, want 1", counts[events.TypeReviewCompleted])
	}
	if counts[events.TypeRevisionRequested] != 0 {
		t.Errorf("an approved job requested a revision")
	}
}

// The point of the loop: a reviewer that wants changes sends the job back
// through implement, verify and review once more.
func TestReview_RequestChangesRunsASecondPass(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	bus := events.NewInProcessBus()
	sub := bus.Subscribe("revise", 256)
	defer bus.Unsubscribe(sub)

	p, impl := reviewProcessor(t, repo, bus)
	rev := &fakeReviewer{decisions: []reviewer.Review{
		requestChanges("you forgot the error case"),
		approve(),
	}}
	p.Reviewer = rev
	newQueuedJob(t, repo, "revise")

	if err := p.Process(context.Background(), "revise"); err != nil {
		t.Fatalf("process: %v", err)
	}
	if got, _ := repo.Get(context.Background(), "revise"); got.Status != jobs.StatusCompleted {
		t.Errorf("status = %q, want completed on the second pass", got.Status)
	}
	if impl.count() != 2 {
		t.Errorf("implementor ran %d times, want 2 — the revision must redo the work", impl.count())
	}
	if rev.calls() != 2 {
		t.Errorf("reviewer ran %d times, want 2", rev.calls())
	}

	// The second attempt has to know what it was asked to fix.
	impl.mu.Lock()
	second := impl.requests[1]
	impl.mu.Unlock()
	if !strings.Contains(second.Feedback, "forgot the error case") {
		t.Errorf("revision feedback = %q, want the reviewer's notes", second.Feedback)
	}

	counts := typesOf(drain(sub))
	if counts[events.TypeRevisionRequested] != 1 {
		t.Errorf("revision.requested = %d, want 1", counts[events.TypeRevisionRequested])
	}
	// The exact sequence of states the loop walks is asserted in
	// TestReview_RevisionWalksTheStateMachine.
}

// The state machine has to record the loop honestly: reviewing ->
// waiting_for_revision -> implementing is the sequence that says "it is going
// round again".
func TestReview_RevisionWalksTheStateMachine(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	bus := events.NewInProcessBus()
	sub := bus.Subscribe("states", 256)
	defer bus.Unsubscribe(sub)

	p, _ := reviewProcessor(t, repo, bus)
	p.Reviewer = &fakeReviewer{decisions: []reviewer.Review{requestChanges("again"), approve()}}
	newQueuedJob(t, repo, "states")

	if err := p.Process(context.Background(), "states"); err != nil {
		t.Fatalf("process: %v", err)
	}

	var seen []string
	for _, e := range drain(sub) {
		if e.Type == events.TypeStepStarted {
			seen = append(seen, e.Payload["status"].(string))
		}
	}
	want := []string{
		"planning", "preparing_workspace", "implementing", "verifying", "reviewing",
		"waiting_for_revision", "implementing", "verifying", "reviewing", "completed",
	}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("state sequence:\n got %v\nwant %v", seen, want)
	}
}

// One revision, not an unbounded retry loop. A model that always wants changes
// would otherwise spend the job's entire budget going in circles.
func TestReview_RevisionsAreBounded(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	p, impl := reviewProcessor(t, repo, events.NewInProcessBus())
	rev := &fakeReviewer{decisions: []reviewer.Review{requestChanges("never happy")}}
	p.Reviewer = rev
	newQueuedJob(t, repo, "loop")

	err := p.Process(context.Background(), "loop")
	if !errors.Is(err, ErrMaxRevisionsExceeded) {
		t.Fatalf("got %v, want ErrMaxRevisionsExceeded", err)
	}
	if got, _ := repo.Get(context.Background(), "loop"); got.Status != jobs.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if want := MaxRevisions + 1; rev.calls() != want || impl.count() != want {
		t.Errorf("review=%d implement=%d, want %d each", rev.calls(), impl.count(), want)
	}
}

// Reject means the approach is wrong, not that it needs another pass. Retrying
// it would spend a second model call arriving somewhere just as wrong.
func TestReview_RejectFailsWithoutRetrying(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	p, impl := reviewProcessor(t, repo, events.NewInProcessBus())
	p.Reviewer = &fakeReviewer{decisions: []reviewer.Review{
		{Decision: reviewer.DecisionReject, Notes: "this rewrites the wrong package"},
	}}
	newQueuedJob(t, repo, "reject")

	err := p.Process(context.Background(), "reject")
	if err == nil || !strings.Contains(err.Error(), "wrong package") {
		t.Fatalf("got %v, want a failure carrying the reviewer's reason", err)
	}
	if got, _ := repo.Get(context.Background(), "reject"); got.Status != jobs.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if impl.count() != 1 {
		t.Errorf("implementor ran %d times, want 1 — a rejection is not retried", impl.count())
	}
}

// The most valuable use of the loop: failing tests become the feedback, so the
// implementor can correct itself instead of the job just dying.
func TestReview_FailedGateTriggersARevision(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	p, impl := reviewProcessor(t, repo, events.NewInProcessBus())
	p.Verifier = &failThenPass{}
	p.Reviewer = &fakeReviewer{decisions: []reviewer.Review{approve()}}
	newQueuedJob(t, repo, "selfheal")

	if err := p.Process(context.Background(), "selfheal"); err != nil {
		t.Fatalf("process: %v", err)
	}
	if got, _ := repo.Get(context.Background(), "selfheal"); got.Status != jobs.StatusCompleted {
		t.Errorf("status = %q, want completed after the fix", got.Status)
	}
	if impl.count() != 2 {
		t.Fatalf("implementor ran %d times, want 2", impl.count())
	}

	impl.mu.Lock()
	second := impl.requests[1]
	impl.mu.Unlock()
	// The check output is the actionable part — without it the second attempt
	// is guessing at what broke.
	for _, want := range []string{"failed verification", "go test", "undefined: Greet"} {
		if !strings.Contains(second.Feedback, want) {
			t.Errorf("feedback is missing %q:\n%s", want, second.Feedback)
		}
	}
}

// failThenPass fails the first verification and passes every one after, which
// is what a successful self-correction looks like.
type failThenPass struct {
	mu sync.Mutex
	n  int
}

func (f *failThenPass) Verify(context.Context, string) (verification.Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	if f.n == 1 {
		return verification.Report{Results: []verification.Result{
			{Check: "go test", Passed: false, Output: "./main.go:7:2: undefined: Greet"},
		}}, nil
	}
	return verification.Report{Results: []verification.Result{{Check: "go test", Passed: true}}}, nil
}

// A reviewer must never rescue a change that does not build. Deterministic
// checks outrank model judgement, so a failing gate is not even shown to it.
func TestReview_ReviewerNeverSeesAFailedGate(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	p, _ := reviewProcessor(t, repo, events.NewInProcessBus())
	p.Verifier = &alwaysFail{}
	rev := &fakeReviewer{decisions: []reviewer.Review{approve()}}
	p.Reviewer = rev
	newQueuedJob(t, repo, "nogate")

	if err := p.Process(context.Background(), "nogate"); err == nil {
		t.Fatal("a job whose checks never passed must not succeed")
	}
	if got, _ := repo.Get(context.Background(), "nogate"); got.Status != jobs.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if rev.calls() != 0 {
		t.Errorf("reviewer was asked to judge %d unbuildable changes, want 0", rev.calls())
	}
}

// Without an implementor nothing can differ between passes, so a revision would
// re-run the identical work. The job fails immediately instead.
func TestReview_NoImplementorMeansNoRevision(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	p, _ := reviewProcessor(t, repo, events.NewInProcessBus())
	p.Implementor = nil
	p.Verifier = &alwaysFail{}
	newQueuedJob(t, repo, "norev")

	if err := p.Process(context.Background(), "norev"); err == nil {
		t.Fatal("expected the failed gate to fail the job")
	}
	if got, _ := repo.Get(context.Background(), "norev"); got.Status != jobs.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
}

// With no reviewer configured, passing the gate is the whole bar.
func TestReview_NoReviewerApprovesOnTheGate(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	p, _ := reviewProcessor(t, repo, events.NewInProcessBus())
	newQueuedJob(t, repo, "gateonly")

	if err := p.Process(context.Background(), "gateonly"); err != nil {
		t.Fatalf("process: %v", err)
	}
	if got, _ := repo.Get(context.Background(), "gateonly"); got.Status != jobs.StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
}

func TestReview_ModelErrorFailsTheJob(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	boom := errors.New("anthropic unavailable after retries")
	p, _ := reviewProcessor(t, repo, events.NewInProcessBus())
	p.Reviewer = &fakeReviewer{err: boom}
	newQueuedJob(t, repo, "revboom")

	err := p.Process(context.Background(), "revboom")
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the underlying cause", err)
	}
	if got, _ := repo.Get(context.Background(), "revboom"); got.Status != jobs.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
}

// The reviewer judges the change, so it has to be handed the change.
func TestReview_RequestCarriesTheEvidence(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	rev := &fakeReviewer{decisions: []reviewer.Review{approve()}}

	p, _ := reviewProcessor(t, repo, events.NewInProcessBus())
	p.Workspaces = &diffingWorkspaces{
		base: t.TempDir(), diffs: map[int]string{0: sampleDiff("the actual change")},
	}
	p.Planner = &fakePlanner{plan: samplePlan()}
	p.Reviewer = rev
	newQueuedJob(t, repo, "evidence")

	if err := p.Process(context.Background(), "evidence"); err != nil {
		t.Fatalf("process: %v", err)
	}
	rev.mu.Lock()
	got := rev.requests[0]
	rev.mu.Unlock()

	if !strings.Contains(got.Diff, "the actual change") {
		t.Errorf("reviewer saw diff %q", got.Diff)
	}
	if got.Task == "" {
		t.Error("reviewer was not told the task")
	}
	if len(got.ChangedFiles) == 0 {
		t.Error("reviewer was not told which files changed")
	}
	if !got.Verification.AllPassed() || len(got.Verification.Results) == 0 {
		t.Errorf("reviewer was not shown the verification report: %+v", got.Verification)
	}
	if got.Plan.Summary == "" {
		t.Error("reviewer was not shown the plan")
	}
}

// A revision has to be judged against what it was told to fix, or the reviewer
// cannot tell a correction from a repeat of the same mistake.
func TestReview_SecondReviewSeesThePriorFeedback(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	rev := &fakeReviewer{decisions: []reviewer.Review{requestChanges("handle nil input"), approve()}}
	p, _ := reviewProcessor(t, repo, events.NewInProcessBus())
	p.Reviewer = rev
	newQueuedJob(t, repo, "prior")

	if err := p.Process(context.Background(), "prior"); err != nil {
		t.Fatalf("process: %v", err)
	}
	rev.mu.Lock()
	defer rev.mu.Unlock()
	if len(rev.requests) != 2 {
		t.Fatalf("got %d reviews, want 2", len(rev.requests))
	}
	if rev.requests[0].PriorFeedback != "" {
		t.Errorf("first review had prior feedback %q", rev.requests[0].PriorFeedback)
	}
	if !strings.Contains(rev.requests[1].PriorFeedback, "handle nil input") {
		t.Errorf("second review's prior feedback = %q", rev.requests[1].PriorFeedback)
	}
}

func TestMergePaths(t *testing.T) {
	tests := map[string]struct{ a, b, want []string }{
		"union":       {[]string{"a"}, []string{"b"}, []string{"a", "b"}},
		"dedupes":     {[]string{"a", "b"}, []string{"b", "c"}, []string{"a", "b", "c"}},
		"drops empty": {[]string{"", "a"}, nil, []string{"a"}},
		"both nil":    {nil, nil, nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := mergePaths(tt.a, tt.b); strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("mergePaths(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// Appending to a caller's slice in a loop would let one variant's writes reach
// into another's backing array.
func TestMergePaths_DoesNotAliasItsInput(t *testing.T) {
	base := make([]string, 1, 8) // spare capacity: append would write in place
	base[0] = "shared.go"

	first := mergePaths(base, []string{"one.go"})
	second := mergePaths(base, []string{"two.go"})

	if len(base) != 1 || base[0] != "shared.go" {
		t.Errorf("input was mutated: %v", base)
	}
	if strings.Join(first, ",") != "shared.go,one.go" {
		t.Errorf("first = %v", first)
	}
	if strings.Join(second, ",") != "shared.go,two.go" {
		t.Errorf("second = %v, want it unaffected by the first call", second)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 100); got != "short" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("x", 200)
	got := truncate(long, 50)
	if len(got) <= 50 || !strings.HasSuffix(got, "(truncated)") {
		t.Errorf("got %q", got)
	}
	if !strings.HasPrefix(got, strings.Repeat("x", 50)) {
		t.Error("truncation dropped the beginning, which is where the cause is")
	}
}

// A gate that could not be *run* is not something a model can fix. The
// toolchain is missing, or a check timed out — the report is empty, so feedback
// built from it would read "all 0 checks passed" and quote no output, asking
// the model to fix an infrastructure problem with no information. Failing now
// beats spending a model call and a whole verification round to fail anyway.
func TestReview_UnrunnableGateFailsWithoutARevision(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	p, impl := reviewProcessor(t, repo, events.NewInProcessBus())
	boom := errors.New(`exec: "go": executable file not found in $PATH`)
	p.Verifier = &erroringVerifier{err: boom}
	rev := &fakeReviewer{decisions: []reviewer.Review{approve()}}
	p.Reviewer = rev

	newQueuedJob(t, repo, "notoolchain")

	err := p.Process(context.Background(), "notoolchain")
	if err == nil {
		t.Fatal("expected the job to fail")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error %v should carry the real cause", err)
	}
	// The decisive part: no second attempt, and the model is never consulted.
	if impl.count() != 1 {
		t.Errorf("implementor ran %d times, want 1 — a revision cannot install a toolchain", impl.count())
	}
	if rev.calls() != 0 {
		t.Errorf("the reviewer was consulted %d times about checks that never ran", rev.calls())
	}
	if got, _ := repo.Get(context.Background(), "notoolchain"); got.Status != jobs.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
}

type erroringVerifier struct{ err error }

func (e *erroringVerifier) Verify(context.Context, string) (verification.Report, error) {
	return verification.Report{}, e.err
}

// Feedback built from an empty report would tell the model everything passed.
func TestGateFeedback_ReportsAVerifierThatCouldNotRun(t *testing.T) {
	c := &candidate{index: 0, err: errors.New(`exec: "go": executable file not found`)}
	got := gateFeedback(c)
	if !strings.Contains(got, "executable file not found") {
		t.Errorf("feedback hides why verification failed:\n%s", got)
	}
	if strings.Contains(got, "all 0 checks passed") && !strings.Contains(got, "verifier reported") {
		t.Errorf("feedback claims the checks passed:\n%s", got)
	}
}
