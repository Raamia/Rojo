package orchestration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/Raamia/Rojo/internal/agents/implementor"
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

// Implementor proposes the file operations that carry out a plan. Declared
// here, near its consumer, so the processor can be tested without a model.
type Implementor interface {
	Propose(ctx context.Context, req implementor.Request) ([]implementor.Operation, error)
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
	// Implementor is optional and only runs when there is both a plan to carry
	// out and a workspace to carry it out in.
	Implementor Implementor
	// Artifacts is optional. Without it a job still runs and still reports its
	// status, but the patch it produced is discarded with the worktree.
	Artifacts ArtifactStore
	// Reviewer is optional. Without one a change that passes the deterministic
	// gate is approved on that evidence alone.
	Reviewer Reviewer
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

// ErrPanic marks a job that failed because a stage panicked.
var ErrPanic = errors.New("panic while processing job")

func (p *Processor) Process(ctx context.Context, jobID string) (err error) {
	log := slog.With("job_id", jobID)

	// A panic in any stage — a nil map from a malformed model response, a bad
	// index, an unexpected type — would otherwise unwind through the worker
	// goroutine and end the process, taking every other in-flight job with it.
	// Recovering here turns it into an ordinary job failure instead: this job
	// ends terminal, its worktree is removed by the cleanup deferred below
	// (defers run last-in-first-out, so cleanup happens before this), and the
	// rest of the service carries on.
	var job *jobs.Job
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		log.Error("panic processing job", "panic", r, "stack", string(debug.Stack()))
		cause := fmt.Errorf("%w: %v", ErrPanic, r)
		if job == nil {
			// The panic beat the job load, so there is no record to fail.
			err = cause
			return
		}
		err = p.markFailed(job, cause)
	}()

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
	var (
		plan         planner.Plan
		contextFiles []string
	)

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

	job, err = p.Repo.Get(jobCtx, jobID)
	if err != nil {
		return fmt.Errorf("load job %s: %w", jobID, err)
	}

	p.emit(jobCtx, jobID, events.TypeJobStarted, nil)

	// Work steps up to (but not including) the terminal completion. Each is
	// followed by a cancellation checkpoint.
	//
	// This is a work list rather than a fixed sequence because a revision
	// appends another pass to it: reviewing a change may conclude that it needs
	// redoing, which sends the job back through implement, verify and review.
	// The state machine validates every transition, so an impossible sequence
	// fails loudly instead of quietly running the wrong step.
	steps := []jobs.JobStatus{
		jobs.StatusPlanning,
		jobs.StatusPreparingWorkspace,
		jobs.StatusImplementing,
		jobs.StatusVerifying,
		jobs.StatusReviewing,
	}

	// State carried between passes: what the gate concluded, and what the
	// previous round was told to fix.
	var (
		gateErr   error
		feedback  string
		revisions int
	)

	for i := 0; i < len(steps); i++ {
		next := steps[i]
		if err := jobCtx.Err(); err != nil {
			return p.endInterrupted(job, err)
		}
		p.emit(jobCtx, jobID, events.TypeStepStarted, map[string]any{"status": string(next)})
		if err := p.advance(jobCtx, job, next); err != nil {
			return p.markFailed(job, err)
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
					contextFiles = sel.Files
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

		if next == jobs.StatusImplementing && p.Implementor != nil && len(cands) > 0 {
			if err := p.implement(jobCtx, log, jobID, job, plan, contextFiles, cands, feedback); err != nil {
				return p.markFailed(job, err)
			}
		}

		if next == jobs.StatusVerifying && len(cands) > 0 {
			var best *candidate
			best, gateErr = p.verify(jobCtx, log, jobID, cands)

			// Capture before the deferred cleanup removes the worktree, and
			// before the reviewing step is allowed to end the job. A rejected
			// attempt's diff is exactly what a human needs in order to see what
			// went wrong, so a failed job still returns a patch — it just does
			// not claim the patch is good.
			p.captureDiff(jobCtx, log, jobID, best)
		}

		if next == jobs.StatusReviewing && len(cands) > 0 {
			// A revision is only worth attempting when something can actually
			// change between passes. With no implementor the second attempt
			// would be byte-for-byte the first, so the job fails now rather
			// than after spending another full verification run proving it.
			canRevise := p.Implementor != nil && revisions < MaxRevisions

			decision, notes, err := p.review(jobCtx, log, jobID, reviewInput{
				task: job.Task, plan: plan, best: representative(cands),
				gateErr: gateErr, feedback: feedback, canRevise: canRevise,
			})
			switch {
			case err != nil:
				return p.markFailed(job, err)
			case decision == outcomeRevise:
				revisions++
				feedback = notes
				gateErr = nil
				log.Info("revision requested", "attempt", revisions, "notes", notes)
				p.emit(jobCtx, jobID, events.TypeRevisionRequested, map[string]any{
					"attempt": revisions, "notes": notes,
				})
				steps = append(steps,
					jobs.StatusWaitingForRevision,
					jobs.StatusImplementing,
					jobs.StatusVerifying,
					jobs.StatusReviewing,
				)
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
	// Deterministic checks outrank every judgement made above this line. The
	// reviewing step is supposed to have failed the job already if the gate did
	// not pass, so reaching here with gateErr set means a bug in that logic —
	// and the failure mode of such a bug is a job claiming success for a change
	// that does not build, which is the one outcome worth an extra guard.
	if gateErr != nil {
		return p.markFailed(job, gateErr)
	}
	p.emit(jobCtx, jobID, events.TypeStepStarted, map[string]any{"status": string(jobs.StatusCompleted)})
	if err := p.advance(jobCtx, job, jobs.StatusCompleted); err != nil {
		return p.markFailed(job, err)
	}
	log.Info("step complete", "status", jobs.StatusCompleted)
	p.emit(jobCtx, jobID, events.TypeStepCompleted, map[string]any{"status": string(jobs.StatusCompleted)})
	p.emit(jobCtx, jobID, events.TypeJobCompleted, nil)
	return nil
}

// implement asks the model for changes and writes them into every candidate
// worktree.
//
// Each variant gets its own proposal, which is what makes fan-out meaningful:
// the same task attempted several ways produces genuinely different patches,
// and the verification gate picks between them on evidence.
//
// A variant whose proposal fails is left unmodified and marked so it cannot
// win; the job only fails if no variant could be implemented at all. One
// unlucky sample must not sink the whole attempt.
func (p *Processor) implement(
	ctx context.Context, log *slog.Logger, jobID string, job *jobs.Job,
	plan planner.Plan, contextFiles []string, cands []*candidate, feedback string,
) error {
	var lastErr error
	implemented := 0

	for _, c := range cands {
		if c.ws == nil {
			continue
		}
		// On a revision, attempts that already failed to produce anything stay
		// dead. The feedback describes a diff they never made, so handing it to
		// them would ask for a correction to work that does not exist.
		if feedback != "" && c.implErr != nil {
			continue
		}
		req := implementor.Request{
			Task: job.Task,
			Plan: plan,
			// The files it touched last round are included alongside the
			// planner's selection: a revision has to see the code it is being
			// asked to correct, and a file it created is in neither the
			// original selection nor the source repository.
			Files:    readSources(c.ws.Path, mergePaths(contextFiles, c.touched)),
			Feedback: feedback,
		}
		ops, err := p.Implementor.Propose(ctx, req)
		if err != nil {
			c.implErr = fmt.Errorf("propose changes: %w", err)
			lastErr = c.implErr
			log.Warn("implementor proposal failed", "variant", c.index, "err", err)
			continue
		}
		if err := implementor.New(c.ws.Path).Apply(ops); err != nil {
			c.implErr = fmt.Errorf("apply changes: %w", err)
			lastErr = c.implErr
			log.Warn("applying changes failed", "variant", c.index, "err", err)
			continue
		}

		implemented++
		c.touched = mergePaths(c.touched, pathsOf(ops))
		log.Info("changes applied", "variant", c.index, "operations", len(ops))
		p.emit(ctx, jobID, events.TypeImplementationCompleted, map[string]any{
			"variant":    c.index,
			"operations": len(ops),
			"paths":      pathsOf(ops),
		})
	}

	if implemented == 0 {
		return fmt.Errorf("no variant could be implemented: %w", lastErr)
	}
	return nil
}

// verify runs the deterministic gate and returns the attempt that represents
// the job, along with the reason the job should fail if none of them passed.
//
// It always names an attempt, even when the gate rejected every one of them:
// the caller still wants that attempt's diff saved, because "here is the patch
// that failed the tests" is a far more useful failure report than "it failed".
func (p *Processor) verify(
	ctx context.Context, log *slog.Logger, jobID string, cands []*candidate,
) (*candidate, error) {
	if p.Verifier == nil {
		// Nothing to check against, so the first usable attempt stands. Its diff
		// is still worth keeping: the user asked for a change, not a test run.
		return representative(cands), nil
	}

	// Every attempt is checked, concurrently: the checks are the slow part and
	// the attempts are independent, so one failing must not stop the others
	// from being judged.
	p.verifyCandidates(ctx, cands)

	for _, c := range cands {
		// Emitting each report persists it: PersistingBus writes event payloads
		// to the event log, so results survive without a dedicated schema.
		payload := map[string]any{
			"variant": c.index,
			"passed":  c.passed(),
			"summary": c.report.Summary(),
			"results": c.report.Results,
		}
		if err := c.failure(); err != nil {
			payload["error"] = err.Error()
		}
		p.emit(ctx, jobID, events.TypeVerificationCompleted, payload)
	}

	winner, ok := selectWinner(cands)
	summary := summarise(cands, winner)
	if len(cands) > 1 {
		p.emit(ctx, jobID, events.TypeFanoutCompleted, map[string]any{
			"total": summary.Total, "passed": summary.Passed,
			"winner": summary.Winner, "results": summary.Results,
		})
		log.Info("fan-out complete", "total", summary.Total,
			"passed", summary.Passed, "winner", summary.Winner)
	}

	// Deterministic checks outrank everything downstream: a job with no passing
	// attempt must not reach completed. Once the reviewer exists this becomes a
	// revision cycle rather than a terminal failure.
	if !ok {
		// Nothing passed, but the closest attempt is still named so its diff is
		// saved with the failure — a patch that failed the checks is what a
		// human reads to find out why.
		return representative(cands), fanoutFailure(cands, summary)
	}
	log.Info("verification complete", "winner", winner.index, "summary", winner.report.Summary())
	return winner, nil
}

// mergePaths unions two path lists, preserving order and dropping duplicates.
// It always allocates: appending to a caller's slice in a loop would let one
// variant's writes reach into another's backing array.
func mergePaths(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	seen := make(map[string]bool, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, p := range list {
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

func pathsOf(ops []implementor.Operation) []string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, op.Path)
	}
	return out
}

// advance moves a job to its next status and persists it.
//
// Both halves are failure paths that used to return without ending the job,
// which left it sitting in a non-terminal status that nothing would ever
// revisit — a job stuck at "verifying" forever, looking like it was still
// working. Callers route the error through markFailed instead. If the store is
// what is broken then markFailed's own write fails too, and it joins that on;
// the outcome is at least reported rather than silently abandoned.
func (p *Processor) advance(ctx context.Context, job *jobs.Job, next jobs.JobStatus) error {
	if err := job.Transition(next); err != nil {
		return fmt.Errorf("transition to %s: %w", next, err)
	}
	if err := p.Repo.Update(ctx, job); err != nil {
		return fmt.Errorf("persist status %s: %w", next, err)
	}
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
