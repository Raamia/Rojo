package orchestration

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/workspace"
)

// Enqueuer is the slice of the queue recovery needs. Declared here rather than
// imported so the recoverer can be tested without a real queue.
type Enqueuer interface {
	Enqueue(jobID string) error
}

// Recoverer reconciles persisted job state with an empty in-process queue at
// startup.
//
// The queue is a Go channel, so it does not survive a restart while the
// database does. Without this, every job a client was told had been accepted
// (201) but that had not yet been picked up simply vanished, and every job
// interrupted mid-flight sat in a non-terminal status forever.
type Recoverer struct {
	Repo       jobs.JobRepository
	Queue      Enqueuer
	Bus        events.Bus
	Workspaces workspace.WorkspaceManager
	// WorktreeBaseDir lets recovery name the worktree a crashed job would have
	// created, so it can be reclaimed.
	WorktreeBaseDir string
	Logger          *slog.Logger
}

// RecoveryReport summarises what a recovery pass did.
type RecoveryReport struct {
	Requeued           int
	FailedInterrupted  int
	WorktreesReclaimed int
	Unqueueable        int
}

func (r RecoveryReport) IsZero() bool {
	return r.Requeued == 0 && r.FailedInterrupted == 0 &&
		r.WorktreesReclaimed == 0 && r.Unqueueable == 0
}

func (r *Recoverer) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// Recover puts persisted jobs back into a coherent state.
//
//   - `queued` jobs never started, so they are safe to run and are re-enqueued.
//   - Jobs in any other non-terminal status were interrupted by whatever ended
//     the previous process. They cannot simply be re-enqueued: Process begins by
//     transitioning to `planning`, which is an illegal move from `implementing`
//     or `verifying`. They are marked `failed` instead, and their worktree is
//     reclaimed. Resuming them safely needs work claiming and idempotent steps,
//     neither of which exists yet.
//
// Terminal jobs are left alone. A failure to recover one job never aborts the
// pass — the goal is to salvage as much as possible — but the error is logged
// and reflected in the report.
func (r *Recoverer) Recover(ctx context.Context) (RecoveryReport, error) {
	var report RecoveryReport

	all, err := r.Repo.List(ctx)
	if err != nil {
		return report, fmt.Errorf("list jobs for recovery: %w", err)
	}

	for _, job := range all {
		switch {
		case job.Status == jobs.StatusQueued:
			if r.Queue == nil {
				continue
			}
			if err := r.Queue.Enqueue(job.ID); err != nil {
				// Almost certainly a full queue. Leaving the job `queued` is
				// the right outcome: it stays recoverable on the next start
				// rather than being lost or wrongly failed.
				report.Unqueueable++
				continue
			}
			report.Requeued++

		case isInterrupted(job.Status):
			if r.reclaimWorktree(ctx, job) {
				report.WorktreesReclaimed++
			}
			if err := r.failInterrupted(ctx, job); err != nil {
				r.logger().Error("recover interrupted job", "job_id", job.ID, "err", err)
				continue
			}
			report.FailedInterrupted++
		}
	}

	return report, nil
}

// isInterrupted reports whether a status means "this job was running when the
// process stopped". Terminal statuses are excluded; `queued` is handled
// separately because it is the one non-terminal state that is safe to re-run.
func isInterrupted(s jobs.JobStatus) bool {
	switch s {
	case jobs.StatusPlanning,
		jobs.StatusPreparingWorkspace,
		jobs.StatusImplementing,
		jobs.StatusVerifying,
		jobs.StatusReviewing,
		jobs.StatusWaitingForRevision:
		return true
	default:
		return false
	}
}

// reclaimWorktree removes the worktree the crashed job would have created.
//
// Nothing about the worktree was persisted, but its path and branch derive from
// the job ID, so it can be named exactly. Cleanup tolerates a worktree that was
// never created or already removed, so this is safe to attempt for any
// interrupted job.
func (r *Recoverer) reclaimWorktree(ctx context.Context, job *jobs.Job) bool {
	if r.Workspaces == nil || r.WorktreeBaseDir == "" || job.RepoPath == "" {
		return false
	}
	ws := workspace.Reconstruct(r.WorktreeBaseDir, job.ID, job.RepoPath)
	if err := r.Workspaces.Cleanup(ctx, ws); err != nil {
		r.logger().Warn("reclaim worktree for interrupted job",
			"job_id", job.ID, "path", ws.Path, "err", err)
		return false
	}
	return true
}

func (r *Recoverer) failInterrupted(ctx context.Context, job *jobs.Job) error {
	previous := job.Status
	if err := job.Transition(jobs.StatusFailed); err != nil {
		return fmt.Errorf("transition %s -> failed: %w", previous, err)
	}
	if r.Bus != nil {
		_ = r.Bus.Publish(ctx, events.Event{
			JobID: job.ID,
			Type:  events.TypeJobFailed,
			Payload: map[string]any{
				"error":  fmt.Sprintf("interrupted in %s by a restart", previous),
				"reason": "recovery",
			},
		})
	}
	if err := r.Repo.Update(ctx, job); err != nil {
		return fmt.Errorf("persist failed status: %w", err)
	}
	r.logger().Info("failed interrupted job", "job_id", job.ID, "interrupted_in", string(previous))
	return nil
}
