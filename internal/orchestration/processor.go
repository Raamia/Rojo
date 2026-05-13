package orchestration

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Raamia/Rojo/internal/jobs"
)

type Processor struct {
	Repo      jobs.JobRepository
	Canceller *Canceller
}

func NewProcessor(repo jobs.JobRepository, c *Canceller) *Processor {
	return &Processor{Repo: repo, Canceller: c}
}

func (p *Processor) Process(ctx context.Context, jobID string) error {
	log := slog.With("job_id", jobID)

	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if p.Canceller != nil {
		p.Canceller.Track(jobID, cancel)
		defer p.Canceller.Release(jobID)
	}

	job, err := p.Repo.Get(jobCtx, jobID)
	if err != nil {
		return fmt.Errorf("load job %s: %w", jobID, err)
	}

	// Work steps up to (but not including) the terminal completion. Each is
	// followed by a cancellation checkpoint.
	steps := []jobs.JobStatus{
		jobs.StatusPlanning,
		jobs.StatusPreparingWorkspace,
		jobs.StatusImplementing,
		jobs.StatusVerifying,
		jobs.StatusReviewing,
	}

	for _, next := range steps {
		if err := jobCtx.Err(); err != nil {
			return p.markCancelled(job)
		}
		if err := job.Transition(next); err != nil {
			return fmt.Errorf("transition to %s: %w", next, err)
		}
		if err := p.Repo.Update(jobCtx, job); err != nil {
			return fmt.Errorf("persist status %s: %w", next, err)
		}
		log.Info("step complete", "status", next)
		select {
		case <-jobCtx.Done():
			return p.markCancelled(job)
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Terminal transition. Check cancellation one last time *before* committing
	// completion — never after, because `completed` is terminal and a
	// subsequent markCancelled would attempt an illegal completed->cancelled
	// transition and return a misleading error for a job that actually finished.
	if err := jobCtx.Err(); err != nil {
		return p.markCancelled(job)
	}
	if err := job.Transition(jobs.StatusCompleted); err != nil {
		return fmt.Errorf("transition to %s: %w", jobs.StatusCompleted, err)
	}
	if err := p.Repo.Update(jobCtx, job); err != nil {
		return fmt.Errorf("persist status %s: %w", jobs.StatusCompleted, err)
	}
	log.Info("step complete", "status", jobs.StatusCompleted)
	return nil
}

func (p *Processor) markCancelled(job *jobs.Job) error {
	if err := job.Transition(jobs.StatusCancelled); err != nil {
		return err
	}
	return p.Repo.Update(context.Background(), job)
}
