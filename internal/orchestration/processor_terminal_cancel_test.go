package orchestration

import (
	"context"
	"testing"

	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
)

// cancelOnCompleteRepo cancels the supplied context the instant the terminal
// `completed` status is persisted, deterministically reproducing the race
// where cancellation and completion collide at the very end of Process.
type cancelOnCompleteRepo struct {
	jobs.JobRepository
	cancel context.CancelFunc
}

func (r *cancelOnCompleteRepo) Update(ctx context.Context, job *jobs.Job) error {
	err := r.JobRepository.Update(ctx, job)
	if job.Status == jobs.StatusCompleted {
		r.cancel()
	}
	return err
}

// Regression: a context cancellation landing exactly as the job completes must
// not drive Process into an illegal completed->cancelled transition. Before the
// fix, the post-step select still ran after the terminal transition, so this
// deterministically returned a misleading "illegal transition" error for a job
// that had actually finished.
func TestProcessor_CancelAtCompletionIsNotAnError(t *testing.T) {
	base := jobs.NewInMemoryRepository()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	repo := &cancelOnCompleteRepo{JobRepository: base, cancel: cancel}

	canc := NewCanceller()
	jobID := "job-terminal-race"
	job := &jobs.Job{
		ID:       jobID,
		Task:     "test terminal cancellation",
		RepoPath: "/tmp/repo",
		Status:   jobs.StatusQueued,
	}
	if err := repo.Create(context.Background(), job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	p := NewProcessor(repo, canc, events.NewInProcessBus())
	if err := p.Process(ctx, jobID); err != nil {
		t.Fatalf("Process returned %v, want nil for a job that reached completion", err)
	}

	got, err := base.Get(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Status != jobs.StatusCompleted {
		t.Fatalf("final status = %q, want %q", got.Status, jobs.StatusCompleted)
	}
}
