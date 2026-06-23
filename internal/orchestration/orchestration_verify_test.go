package orchestration

// Characterization tests for internal/orchestration.
// These document the ACTUAL, observed behavior of the Processor, Canceller,
// and revision loop against the real in-memory JobRepository and InProcessBus.
// They add no new dependencies (stdlib + existing internal packages only).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/agents/reviewer"
	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// drain reads every buffered event off the subscription without blocking.
func drain(sub *events.Subscription) []events.Event {
	var out []events.Event
	for {
		select {
		case e, ok := <-sub.C:
			if !ok {
				return out
			}
			out = append(out, e)
		default:
			return out
		}
	}
}

func typesOf(evs []events.Event) map[string]int {
	counts := make(map[string]int)
	for _, e := range evs {
		counts[e.Type]++
	}
	return counts
}

func newQueuedJob(t *testing.T, repo jobs.JobRepository, id string) *jobs.Job {
	t.Helper()
	j := &jobs.Job{
		ID:        id,
		Task:      "do the thing",
		RepoPath:  "/tmp/repo",
		Status:    jobs.StatusQueued,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := repo.Create(context.Background(), j); err != nil {
		t.Fatalf("create job: %v", err)
	}
	return j
}

// ---------------------------------------------------------------------------
// Canceller
// ---------------------------------------------------------------------------

func TestCanceller_TrackThenCancelFires(t *testing.T) {
	c := NewCanceller()
	fired := make(chan struct{}, 1)
	c.Track("job-1", func() { fired <- struct{}{} })

	if err := c.Cancel("job-1"); err != nil {
		t.Fatalf("Cancel returned %v, want nil", err)
	}
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("cancel func was not fired")
	}

	// After a successful Cancel the entry is removed; a second Cancel fails.
	if err := c.Cancel("job-1"); !errors.Is(err, ErrJobNotRunning) {
		t.Fatalf("second Cancel returned %v, want ErrJobNotRunning", err)
	}
}

func TestCanceller_CancelUntracked(t *testing.T) {
	c := NewCanceller()
	if err := c.Cancel("nope"); !errors.Is(err, ErrJobNotRunning) {
		t.Fatalf("Cancel untracked returned %v, want ErrJobNotRunning", err)
	}
}

func TestCanceller_ReleaseRemovesWithoutFiring(t *testing.T) {
	c := NewCanceller()
	fired := make(chan struct{}, 1)
	c.Track("job-2", func() { fired <- struct{}{} })
	c.Release("job-2")

	if err := c.Cancel("job-2"); !errors.Is(err, ErrJobNotRunning) {
		t.Fatalf("Cancel after Release returned %v, want ErrJobNotRunning", err)
	}
	select {
	case <-fired:
		t.Fatal("cancel func fired even though Release should not fire it")
	case <-time.After(50 * time.Millisecond):
		// expected: nothing fired
	}
}

// ---------------------------------------------------------------------------
// Processor happy path
// ---------------------------------------------------------------------------

func TestProcessor_HappyPathReachesCompleted(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	bus := events.NewInProcessBus()
	canc := NewCanceller()
	jobID := "job-happy"
	newQueuedJob(t, repo, jobID)

	// Subscribe BEFORE running. 6 steps -> 1 job.started + 12 step.* + 1 job.completed = 14 events.
	sub := bus.Subscribe(jobID, 128)
	defer bus.Unsubscribe(sub)

	p := NewProcessor(repo, canc, bus, nil, nil)

	done := make(chan error, 1)
	go func() { done <- p.Process(context.Background(), jobID) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Process returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Process did not finish within timeout")
	}

	got, err := repo.Get(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Status != jobs.StatusCompleted {
		t.Fatalf("final status = %q, want %q", got.Status, jobs.StatusCompleted)
	}

	counts := typesOf(drain(sub))
	if counts[events.TypeJobStarted] != 1 {
		t.Errorf("job.started count = %d, want 1", counts[events.TypeJobStarted])
	}
	if counts[events.TypeJobCompleted] != 1 {
		t.Errorf("job.completed count = %d, want 1", counts[events.TypeJobCompleted])
	}
	if counts[events.TypeStepStarted] != 6 {
		t.Errorf("step.started count = %d, want 6", counts[events.TypeStepStarted])
	}
	if counts[events.TypeStepCompleted] != 6 {
		t.Errorf("step.completed count = %d, want 6", counts[events.TypeStepCompleted])
	}
	if counts[events.TypeJobCancelled] != 0 {
		t.Errorf("job.cancelled count = %d, want 0", counts[events.TypeJobCancelled])
	}
}

// ---------------------------------------------------------------------------
// Processor cancellation
// ---------------------------------------------------------------------------

func TestProcessor_CancelMidFlightReachesCancelled(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	bus := events.NewInProcessBus()
	canc := NewCanceller()
	jobID := "job-cancel"
	newQueuedJob(t, repo, jobID)

	sub := bus.Subscribe(jobID, 128)
	defer bus.Unsubscribe(sub)

	p := NewProcessor(repo, canc, bus, nil, nil)

	done := make(chan error, 1)
	go func() { done <- p.Process(context.Background(), jobID) }()

	// Wait until the processor has registered itself with the Canceller,
	// then cancel. Poll rather than sleep-and-hope.
	deadline := time.After(3 * time.Second)
	for {
		if err := canc.Cancel(jobID); err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("processor never registered with canceller")
		case <-time.After(2 * time.Millisecond):
		}
	}

	select {
	case <-done:
		// Process returned (error or nil both acceptable here; we assert on status).
	case <-time.After(5 * time.Second):
		t.Fatal("Process did not finish after cancellation")
	}

	got, err := repo.Get(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Status != jobs.StatusCancelled {
		t.Fatalf("final status = %q, want %q (cancelled)", got.Status, jobs.StatusCancelled)
	}

	counts := typesOf(drain(sub))
	if counts[events.TypeJobCancelled] != 1 {
		t.Errorf("job.cancelled count = %d, want 1", counts[events.TypeJobCancelled])
	}
	if counts[events.TypeJobCompleted] != 0 {
		t.Errorf("job.completed count = %d, want 0 (job was cancelled)", counts[events.TypeJobCompleted])
	}
}

// ---------------------------------------------------------------------------
// RunWithRevisionLoop
// ---------------------------------------------------------------------------

func TestRunWithRevisionLoop_ApproveFirstTry(t *testing.T) {
	calls := 0
	got, err := RunWithRevisionLoop(context.Background(),
		func(_ context.Context, feedback string) (reviewer.Review, error) {
			calls++
			if feedback != "" {
				t.Errorf("first call feedback = %q, want empty", feedback)
			}
			return reviewer.Review{Decision: reviewer.DecisionApprove}, nil
		})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got.Decision != reviewer.DecisionApprove {
		t.Errorf("decision = %q, want approve", got.Decision)
	}
	if calls != 1 {
		t.Errorf("attempt called %d times, want 1", calls)
	}
}

func TestRunWithRevisionLoop_RequestChangesThenApprove(t *testing.T) {
	calls := 0
	var seenFeedback []string
	got, err := RunWithRevisionLoop(context.Background(),
		func(_ context.Context, feedback string) (reviewer.Review, error) {
			calls++
			seenFeedback = append(seenFeedback, feedback)
			if calls == 1 {
				return reviewer.Review{Decision: reviewer.DecisionRequestChanges, Notes: "please fix X"}, nil
			}
			return reviewer.Review{Decision: reviewer.DecisionApprove}, nil
		})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got.Decision != reviewer.DecisionApprove {
		t.Errorf("final decision = %q, want approve", got.Decision)
	}
	if calls != 2 {
		t.Fatalf("attempt called %d times, want 2", calls)
	}
	// Feedback from the first request_changes must be threaded into the retry.
	if seenFeedback[0] != "" {
		t.Errorf("first feedback = %q, want empty", seenFeedback[0])
	}
	if seenFeedback[1] != "please fix X" {
		t.Errorf("second feedback = %q, want %q", seenFeedback[1], "please fix X")
	}
}

func TestRunWithRevisionLoop_AlwaysRequestChangesExceedsMax(t *testing.T) {
	calls := 0
	_, err := RunWithRevisionLoop(context.Background(),
		func(_ context.Context, _ string) (reviewer.Review, error) {
			calls++
			return reviewer.Review{Decision: reviewer.DecisionRequestChanges, Notes: "still bad"}, nil
		})
	if !errors.Is(err, ErrMaxRevisionsExceeded) {
		t.Fatalf("err = %v, want ErrMaxRevisionsExceeded", err)
	}
	// MaxRevisions == 1 => loop runs i = 0 and i = 1 => 2 attempts before giving up.
	if calls != MaxRevisions+1 {
		t.Errorf("attempt called %d times, want %d", calls, MaxRevisions+1)
	}
}
