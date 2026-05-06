package orchestration

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Raamia/Rojo/internal/jobs"
)

type Processor struct {
	Repo jobs.JobRepository
}

func NewProcessor(repo jobs.JobRepository) *Processor {
	return &Processor{Repo: repo}
}

func (p *Processor) Process(ctx context.Context, jobID string) error {
	log := slog.With("job_id", jobID)
	job, err := p.Repo.Get(ctx, jobID)
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
		if err := ctx.Err(); err != nil {
			return p.markCancelled(context.Background(), job)
		}
		if err := job.Transition(next); err != nil {
			return fmt.Errorf("transition to %s: %w", next, err)
		}
		if err := p.Repo.Update(ctx, job); err != nil {
			return fmt.Errorf("persist status %s: %w", next, err)
		}
		log.Info("step complete", "status", next)
		select {
		case <-ctx.Done():
			return p.markCancelled(context.Background(), job)
		case <-time.After(50 * time.Millisecond):
		}
	}
	return nil
}

func (p *Processor) markCancelled(ctx context.Context, job *jobs.Job) error {
	if err := job.Transition(jobs.StatusCancelled); err != nil {
		return err
	}
	return p.Repo.Update(ctx, job)
}
