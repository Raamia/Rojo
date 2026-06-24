package orchestration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/verification"
)

// blockingVerifier parks until its context ends, standing in for a wedged step
// — a hung `go test`, an unreachable dependency, a deadlocked build.
type blockingVerifier struct{ entered chan struct{} }

func (b *blockingVerifier) Verify(ctx context.Context, _ string) (verification.Report, error) {
	if b.entered != nil {
		close(b.entered)
	}
	<-ctx.Done()
	return verification.Report{}, ctx.Err()
}

// Without a deadline a wedged step holds its worker slot forever; with the
// default worker count, four such jobs stall the service entirely.
func TestProcessor_WedgedStepIsCutOffByTheJobTimeout(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	bus := events.NewInProcessBus()
	sub := bus.Subscribe("wedged", 64)
	defer bus.Unsubscribe(sub)

	newQueuedJob(t, repo, "wedged")
	p := NewProcessor(repo, NewCanceller(), bus, &fakeWorkspaces{}, &blockingVerifier{})
	p.JobTimeout = 250 * time.Millisecond

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- p.Process(context.Background(), "wedged") }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Process never returned — the job timeout did not take effect")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Process took %s to give up on a 250ms deadline", elapsed)
	}

	got, err := repo.Get(context.Background(), "wedged")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != jobs.StatusFailed {
		t.Fatalf("status = %q, want failed — a job that ran out of time must reach a terminal state", got.Status)
	}
}

// A deadline is the system failing to finish; a cancellation is somebody
// deliberately stopping the work. Reporting a timeout as `cancelled` would
// misattribute it, so the two outcomes stay distinct.
func TestProcessor_TimeoutFailsWhileCancellationCancels(t *testing.T) {
	t.Run("timeout ends failed", func(t *testing.T) {
		repo := jobs.NewInMemoryRepository()
		newQueuedJob(t, repo, "t-out")
		p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus(), &fakeWorkspaces{}, &blockingVerifier{})
		p.JobTimeout = 150 * time.Millisecond

		err := p.Process(context.Background(), "t-out")
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "deadline") {
			t.Errorf("error %q should name the deadline as the cause", err)
		}
		got, _ := repo.Get(context.Background(), "t-out")
		if got.Status != jobs.StatusFailed {
			t.Errorf("status = %q, want failed", got.Status)
		}
	})

	t.Run("cancellation ends cancelled", func(t *testing.T) {
		repo := jobs.NewInMemoryRepository()
		canc := NewCanceller()
		fake := &fakeWorkspaces{}
		fake.afterCreate = func() { _ = canc.Cancel("t-cancel") }
		newQueuedJob(t, repo, "t-cancel")

		p := NewProcessor(repo, canc, events.NewInProcessBus(), fake, nil)
		p.JobTimeout = time.Minute // generous, so only the cancel can end it

		done := make(chan error, 1)
		go func() { done <- p.Process(context.Background(), "t-cancel") }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Process did not return")
		}

		got, _ := repo.Get(context.Background(), "t-cancel")
		if got.Status != jobs.StatusCancelled {
			t.Errorf("status = %q, want cancelled — a user cancel is not a timeout", got.Status)
		}
	})
}

// The worktree still has to be reclaimed when a job is cut off mid-flight.
func TestProcessor_TimeoutStillCleansUpTheWorkspace(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	fake := &fakeWorkspaces{}
	newQueuedJob(t, repo, "timeout-cleanup")

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus(), fake, &blockingVerifier{})
	p.JobTimeout = 150 * time.Millisecond

	_ = p.Process(context.Background(), "timeout-cleanup")

	create, cleanup := fake.counts()
	if create != 1 {
		t.Fatalf("Create called %d times, want 1", create)
	}
	if cleanup != 1 {
		t.Errorf("Cleanup called %d times after a timeout, want 1", cleanup)
	}
}

// A Processor built without an explicit timeout must not run unbounded.
func TestProcessor_ZeroTimeoutFallsBackToTheDefault(t *testing.T) {
	p := &Processor{}
	if got := p.jobTimeout(); got != DefaultJobTimeout {
		t.Errorf("jobTimeout() = %s, want the %s default rather than unlimited", got, DefaultJobTimeout)
	}
	p.JobTimeout = -1
	if got := p.jobTimeout(); got != DefaultJobTimeout {
		t.Errorf("negative timeout gave %s, want the default", got)
	}
	p.JobTimeout = 5 * time.Second
	if got := p.jobTimeout(); got != 5*time.Second {
		t.Errorf("explicit timeout gave %s, want 5s", got)
	}
}

// A job that finishes well inside its deadline is unaffected.
func TestProcessor_GenerousTimeoutDoesNotDisturbNormalJobs(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	newQueuedJob(t, repo, "fast")

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus(), nil, nil)
	p.JobTimeout = time.Minute

	if err := p.Process(context.Background(), "fast"); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := repo.Get(context.Background(), "fast")
	if got.Status != jobs.StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
}

// The worker's own context still wins: shutting the pool down cancels jobs
// regardless of how much of their deadline is left.
func TestProcessor_ParentCancellationStillPropagates(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	newQueuedJob(t, repo, "parent-cancel")

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus(), nil, nil)
	p.JobTimeout = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := p.Process(ctx, "parent-cancel"); err != nil {
		t.Logf("process returned %v", err)
	}
	got, _ := repo.Get(context.Background(), "parent-cancel")
	if got.Status != jobs.StatusCancelled {
		t.Errorf("status = %q, want cancelled", got.Status)
	}
}
