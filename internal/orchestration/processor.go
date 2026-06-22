package orchestration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/workspace"
)

type Processor struct {
	Repo      jobs.JobRepository
	Canceller *Canceller
	Bus       events.Bus
	// Workspaces is optional. When nil the job walks its states without an
	// isolated checkout, which is what the pure state-machine tests want.
	Workspaces workspace.WorkspaceManager
}

func NewProcessor(repo jobs.JobRepository, c *Canceller, bus events.Bus, ws workspace.WorkspaceManager) *Processor {
	return &Processor{Repo: repo, Canceller: c, Bus: bus, Workspaces: ws}
}

func (p *Processor) emit(ctx context.Context, jobID, eventType string, payload map[string]any) {
	if p.Bus == nil {
		return
	}
	_ = p.Bus.Publish(ctx, events.Event{
		JobID:   jobID,
		Type:    eventType,
		Payload: payload,
	})
}

func (p *Processor) Process(ctx context.Context, jobID string) error {
	log := slog.With("job_id", jobID)

	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if p.Canceller != nil {
		p.Canceller.Track(jobID, cancel)
		defer p.Canceller.Release(jobID)
	}

	// Declared before the deferred cleanup so the closure observes whatever the
	// preparing_workspace step later assigns. defer fires on every exit path —
	// success, error, cancellation, panic — which is exactly the guarantee this
	// needs: a job must never leave a worktree behind, however it ends.
	//
	// Cleanup is handed jobCtx even though it is often already cancelled here;
	// it deliberately strips cancellation internally so that a cancelled job
	// still gets cleaned up.
	var ws *workspace.Workspace
	defer func() {
		if p.Workspaces == nil || ws == nil {
			return
		}
		if err := p.Workspaces.Cleanup(jobCtx, ws); err != nil {
			log.Error("cleanup workspace", "path", ws.Path, "branch", ws.Branch, "err", err)
		}
	}()

	job, err := p.Repo.Get(jobCtx, jobID)
	if err != nil {
		return fmt.Errorf("load job %s: %w", jobID, err)
	}

	p.emit(jobCtx, jobID, events.TypeJobStarted, nil)

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
		p.emit(jobCtx, jobID, events.TypeStepStarted, map[string]any{"status": string(next)})
		if err := job.Transition(next); err != nil {
			return fmt.Errorf("transition to %s: %w", next, err)
		}
		if err := p.Repo.Update(jobCtx, job); err != nil {
			return fmt.Errorf("persist status %s: %w", next, err)
		}

		// The step's actual work happens between started and completed, so
		// step.completed means the work finished rather than just the status
		// write. Today only preparing_workspace does anything; the planner,
		// implementor, verifier and reviewer slot in beside it.
		if next == jobs.StatusPreparingWorkspace && p.Workspaces != nil {
			created, wsErr := p.Workspaces.Create(jobCtx, jobID, job.RepoPath)
			if wsErr != nil {
				return p.markFailed(job, fmt.Errorf("create workspace for job %s: %w", jobID, wsErr))
			}
			ws = created
			log.Info("workspace created", "path", ws.Path, "branch", ws.Branch)
			p.emit(jobCtx, jobID, events.TypeWorkspaceCreated, map[string]any{
				"path":   ws.Path,
				"branch": ws.Branch,
			})
		}

		log.Info("step complete", "status", next)
		p.emit(jobCtx, jobID, events.TypeStepCompleted, map[string]any{"status": string(next)})
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
	p.emit(jobCtx, jobID, events.TypeStepStarted, map[string]any{"status": string(jobs.StatusCompleted)})
	if err := job.Transition(jobs.StatusCompleted); err != nil {
		return fmt.Errorf("transition to %s: %w", jobs.StatusCompleted, err)
	}
	if err := p.Repo.Update(jobCtx, job); err != nil {
		return fmt.Errorf("persist status %s: %w", jobs.StatusCompleted, err)
	}
	log.Info("step complete", "status", jobs.StatusCompleted)
	p.emit(jobCtx, jobID, events.TypeStepCompleted, map[string]any{"status": string(jobs.StatusCompleted)})
	p.emit(jobCtx, jobID, events.TypeJobCompleted, nil)
	return nil
}

// markFailed drives a job to the terminal failed status and returns the
// original cause, so the worker still logs why while the job stops looking
// like it is mid-flight forever.
//
// Like markCancelled, the terminal write uses context.Background(): by the time
// a step has failed the job's own context is frequently dead or dying, and a
// status write that rides on it would be refused — leaving the job wedged in a
// non-terminal state, which is the exact outcome this exists to prevent.
func (p *Processor) markFailed(job *jobs.Job, cause error) error {
	if err := job.Transition(jobs.StatusFailed); err != nil {
		return errors.Join(cause, err)
	}
	p.emit(context.Background(), job.ID, events.TypeJobFailed, map[string]any{"error": cause.Error()})
	if err := p.Repo.Update(context.Background(), job); err != nil {
		return errors.Join(cause, fmt.Errorf("persist failed status: %w", err))
	}
	return cause
}

func (p *Processor) markCancelled(job *jobs.Job) error {
	if err := job.Transition(jobs.StatusCancelled); err != nil {
		return err
	}
	p.emit(context.Background(), job.ID, events.TypeJobCancelled, nil)
	return p.Repo.Update(context.Background(), job)
}
