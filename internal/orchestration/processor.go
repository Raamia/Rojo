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

	steps := []jobs.JobStatus{
		jobs.StatusPlanning,
		jobs.StatusPreparingWorkspace,
		jobs.StatusImplementing,
		jobs.StatusVerifying,
		jobs.StatusReviewing,
		jobs.StatusCompleted,
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
	return nil
}

func (p *Processor) markCancelled(job *jobs.Job) error {
	if err := job.Transition(jobs.StatusCancelled); err != nil {
		return err
	}
	return p.Repo.Update(context.Background(), job)
}
