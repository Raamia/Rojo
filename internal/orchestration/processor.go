package orchestration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Raamia/Rojo/internal/agents/planner"
	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/repocontext"
	"github.com/Raamia/Rojo/internal/verification"
	"github.com/Raamia/Rojo/internal/workspace"
)

// Verifier runs the deterministic quality gate against a checked-out
// workspace. Declared here rather than imported as a concrete type so the
// processor can be tested without running real commands.
type Verifier interface {
	Verify(ctx context.Context, dir string) (verification.Report, error)
}

// Planner turns a task into a structured plan. Declared here, near its
// consumer, so the processor can be tested without calling a model.
type Planner interface {
	Plan(ctx context.Context, req planner.Request) (planner.Plan, error)
}

// ContextSelector picks the files worth showing the planner. Optional: with
// none configured the planner sees the task and repo path only.
type ContextSelector interface {
	Select(ctx context.Context, repoPath, task string) (repocontext.Context, error)
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
	// Planner is optional. When nil the planning step is a no-op, which is how
	// the pipeline runs with no model configured.
	Planner Planner
	// Context is optional and only consulted when a Planner is set.
	Context ContextSelector
	// Variants is how many independent attempts each job gets. 1 (the default)
	// runs a job once. Higher values fan the job out across that many isolated
	// checkouts, verify them all, and keep the first that passes.
	Variants int
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

// NewProcessor builds a processor from the three collaborators it cannot work
// without. The pipeline stages — Workspaces, Verifier, and the planner,
// implementor and reviewer still to come — are set as fields afterwards.
//
// They are fields rather than parameters because they are genuinely optional
// (a processor with none of them is a working state machine, which is what the
// transition tests exercise) and because the list only grows: threading each
// new stage through a constructor meant editing every call site in the suite
// for a dependency most of those tests do not use.
func NewProcessor(repo jobs.JobRepository, c *Canceller, bus events.Bus) *Processor {
	return &Processor{Repo: repo, Canceller: c, Bus: bus}
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
	// The plan is produced by the planning step and consumed by the steps after
	// it; it is declared here so it outlives one loop iteration.
	var plan planner.Plan
	_ = plan // consumed by the implementor once that is wired in

	var cands []*candidate
	defer func() {
		if p.Workspaces == nil {
			return
		}
		for _, c := range cands {
			if c.ws == nil {
				continue
			}
			if err := p.Workspaces.Cleanup(jobCtx, c.ws); err != nil {
				log.Error("cleanup workspace", "variant", c.index,
					"path", c.ws.Path, "branch", c.ws.Branch, "err", err)
			}
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
		if next == jobs.StatusPlanning && p.Planner != nil {
			req := planner.Request{Task: job.Task, RepoPath: job.RepoPath}

			// Context selection is best effort. Failing to read the repository
			// should give the planner less to work with, not fail the job —
			// the verification gate is what catches a plan that was too thin.
			if p.Context != nil {
				sel, ctxErr := p.Context.Select(jobCtx, job.RepoPath, job.Task)
				if ctxErr != nil {
					log.Warn("select repo context", "err", ctxErr)
				} else {
					req.Files = sel.Files
					log.Info("repo context selected",
						"files", len(sel.Files), "tracked", sel.TotalTracked, "keywords", sel.Keywords)
				}
			}

			created, planErr := p.Planner.Plan(jobCtx, req)
			if planErr != nil {
				return p.markFailed(job, fmt.Errorf("plan job %s: %w", jobID, planErr))
			}
			plan = created
			log.Info("plan created", "summary", plan.Summary, "steps", len(plan.Steps))
			p.emit(jobCtx, jobID, events.TypePlanCreated, map[string]any{
				"summary": plan.Summary,
				"steps":   plan.Steps,
			})
		}

		if next == jobs.StatusPreparingWorkspace && p.Workspaces != nil {
			total := variantCount(p.Variants)
			created, wsErr := p.createCandidates(jobCtx, jobID, job.RepoPath, total)
			cands = created // assign even on failure so the defer cleans up partial work
			if wsErr != nil {
				return p.markFailed(job, fmt.Errorf("prepare workspaces for job %s: %w", jobID, wsErr))
			}
			for _, c := range cands {
				log.Info("workspace created", "variant", c.index, "path", c.ws.Path, "branch", c.ws.Branch)
				p.emit(jobCtx, jobID, events.TypeWorkspaceCreated, map[string]any{
					"variant": c.index,
					"path":    c.ws.Path,
					"branch":  c.ws.Branch,
				})
			}
		}

		if next == jobs.StatusVerifying && p.Verifier != nil && len(cands) > 0 {
			// Every attempt is checked, concurrently: the checks are the slow
			// part and the attempts are independent, so one failing must not
			// stop the others from being judged.
			p.verifyCandidates(jobCtx, cands)

			for _, c := range cands {
				// Emitting each report persists it: PersistingBus writes event
				// payloads to the events table, so results survive without a
				// dedicated schema.
				payload := map[string]any{
					"variant": c.index,
					"passed":  c.passed(),
					"summary": c.report.Summary(),
					"results": c.report.Results,
				}
				if c.err != nil {
					payload["error"] = c.err.Error()
				}
				p.emit(jobCtx, jobID, events.TypeVerificationCompleted, payload)
			}

			winner, ok := selectWinner(cands)
			summary := summarise(cands, winner)
			if len(cands) > 1 {
				p.emit(jobCtx, jobID, events.TypeFanoutCompleted, map[string]any{
					"total": summary.Total, "passed": summary.Passed,
					"winner": summary.Winner, "results": summary.Results,
				})
				log.Info("fan-out complete", "total", summary.Total,
					"passed", summary.Passed, "winner", summary.Winner)
			}

			// Deterministic checks outrank everything downstream: a job with no
			// passing attempt must not reach completed. Once the reviewer exists
			// this becomes a revision cycle rather than a terminal failure.
			if !ok {
				return p.markFailed(job, fanoutFailure(cands, summary))
			}
			log.Info("verification complete", "winner", winner.index, "summary", winner.report.Summary())
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
