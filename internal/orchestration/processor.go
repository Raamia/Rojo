package orchestration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/verification"
	"github.com/Raamia/Rojo/internal/workspace"
)

// Verifier runs the deterministic quality gate against a checked-out
// workspace. Declared here rather than imported as a concrete type so the
// processor can be tested without running real commands.
type Verifier interface {
	Verify(ctx context.Context, dir string) (verification.Report, error)
}

type Processor struct {
	Repo      jobs.JobRepository
	Canceller *Canceller
	Bus       events.Bus
	// Workspaces is optional. When nil the job walks its states without an
	// isolated checkout, which is what the pure state-machine tests want.
	Workspaces workspace.WorkspaceManager
	// Verifier is optional and only runs when a workspace exists — there is
	// nothing meaningful to check without a checkout.
	Verifier Verifier
	// JobTimeout bounds one job's total execution. A zero value falls back to
	// DefaultJobTimeout rather than meaning "unlimited": a job that can run
	// forever holds its worker slot forever, and with the default four workers
	// four such jobs stall the whole service. There is deliberately no way to
	// configure an unbounded job.
	JobTimeout time.Duration
}

// DefaultJobTimeout applies when a Processor is built without an explicit one.
const DefaultJobTimeout = 30 * time.Minute

func (p *Processor) jobTimeout() time.Duration {
	if p.JobTimeout <= 0 {
		return DefaultJobTimeout
	}
	return p.JobTimeout
}

func NewProcessor(repo jobs.JobRepository, c *Canceller, bus events.Bus, ws workspace.WorkspaceManager, v Verifier) *Processor {
	return &Processor{Repo: repo, Canceller: c, Bus: bus, Workspaces: ws, Verifier: v}
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

	// Two layers: an outer cancel the Canceller can fire on request, and an
	// inner deadline. Cancelling the outer propagates inward, so the Canceller
	// still tracks a single func that stops everything, while the deadline
	// remains distinguishable — jobCtx.Err() reports DeadlineExceeded for a
	// timeout and Canceled for a request, and those are different outcomes.
	outerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobCtx, cancelTimeout := context.WithTimeout(outerCtx, p.jobTimeout())
	defer cancelTimeout()

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
			return p.endInterrupted(job, err)
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

		if next == jobs.StatusVerifying && p.Verifier != nil && ws != nil {
			report, vErr := p.Verifier.Verify(jobCtx, ws.Path)
			if vErr != nil {
				return p.markFailed(job, fmt.Errorf("run verification for job %s: %w", jobID, vErr))
			}

			// Emitting the report persists it: PersistingBus writes event
			// payloads to the events table, so the results survive without a
			// dedicated schema.
			p.emit(jobCtx, jobID, events.TypeVerificationCompleted, map[string]any{
				"passed":  report.AllPassed(),
				"summary": report.Summary(),
				"results": report.Results,
			})
			log.Info("verification complete", "passed", report.AllPassed(), "summary", report.Summary())

			// Deterministic checks outrank everything downstream: a job whose
			// gate failed must not reach completed. Once the reviewer exists
			// this becomes a revision cycle rather than a terminal failure.
			if !report.AllPassed() {
				return p.markFailed(job, fmt.Errorf("verification failed: %s", report.Summary()))
			}
		}

		log.Info("step complete", "status", next)
		p.emit(jobCtx, jobID, events.TypeStepCompleted, map[string]any{"status": string(next)})
		select {
		case <-jobCtx.Done():
			return p.endInterrupted(job, jobCtx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Terminal transition. Check cancellation one last time *before* committing
	// completion — never after, because `completed` is terminal and a
	// subsequent markCancelled would attempt an illegal completed->cancelled
	// transition and return a misleading error for a job that actually finished.
	if err := jobCtx.Err(); err != nil {
		return p.endInterrupted(job, err)
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

// endInterrupted decides how a job that stopped early should end.
//
// A cancellation is somebody deliberately stopping the work, and `cancelled`
// records that faithfully. A deadline is the system failing to finish, which is
// not the same thing and should not be reported as though a user asked for it —
// it ends `failed`, with the deadline named so the cause is obvious from the
// job alone.
func (p *Processor) endInterrupted(job *jobs.Job, cause error) error {
	if errors.Is(cause, context.DeadlineExceeded) {
		return p.markFailed(job, fmt.Errorf("job %s exceeded its %s deadline", job.ID, p.jobTimeout()))
	}
	return p.markCancelled(job)
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
