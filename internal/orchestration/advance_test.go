package orchestration

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
)

// failingUpdates rejects the Nth Update, simulating a store that goes away
// mid-job.
type failingUpdates struct {
	jobs.JobRepository
	mu       sync.Mutex
	failOn   int
	n        int
	rejected error
}

func (f *failingUpdates) Update(ctx context.Context, j *jobs.Job) error {
	f.mu.Lock()
	f.n++
	hit := f.n == f.failOn
	f.mu.Unlock()
	if hit {
		return f.rejected
	}
	return f.JobRepository.Update(ctx, j)
}

// A status write that fails used to return without ending the job, leaving it
// parked in a non-terminal state that nothing would ever revisit — a job stuck
// at "planning" forever, looking like it was still working.
func TestProcess_FailedStatusWriteStillEndsTheJob(t *testing.T) {
	inner := jobs.NewInMemoryRepository()
	boom := errors.New("data dir is read-only")
	repo := &failingUpdates{JobRepository: inner, failOn: 2, rejected: boom}

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	newQueuedJob(t, inner, "stuck")

	err := p.Process(context.Background(), "stuck")
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the underlying store error", err)
	}
	got, _ := inner.Get(context.Background(), "stuck")
	if got.Status != jobs.StatusFailed {
		t.Errorf("status = %q, want failed — a job must never be abandoned mid-flight", got.Status)
	}
}

// When the store itself is what is broken, markFailed's own write fails too.
// The job cannot be marked, but the caller must still learn both reasons rather
// than one silently replacing the other.
func TestProcess_TotallyBrokenStoreStillReportsBothCauses(t *testing.T) {
	inner := jobs.NewInMemoryRepository()
	boom := errors.New("disk gone")
	repo := &alwaysFailingUpdates{JobRepository: inner, rejected: boom}

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	newQueuedJob(t, inner, "broken")

	err := p.Process(context.Background(), "broken")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error %v should carry the store failure", err)
	}
}

type alwaysFailingUpdates struct {
	jobs.JobRepository
	rejected error
}

func (a *alwaysFailingUpdates) Update(context.Context, *jobs.Job) error { return a.rejected }
