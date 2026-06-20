package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Raamia/Rojo/internal/execution"
)

const branchPrefix = "rojo/job/"

type GitWorkspaceManager struct {
	runner  execution.CommandRunner
	baseDir string
}

func NewGitWorkspaceManager(runner execution.CommandRunner, baseDir string) *GitWorkspaceManager {
	return &GitWorkspaceManager{runner: runner, baseDir: baseDir}
}

func (m *GitWorkspaceManager) Create(ctx context.Context, jobID, repoPath string) (*Workspace, error) {
	if err := validateRepoPath(repoPath); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(m.baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("create base dir: %w", err)
	}

	branch := branchPrefix + jobID
	worktreePath := filepath.Join(m.baseDir, jobID)

	res, err := m.runner.Run(ctx, repoPath, "git", "worktree", "add", "-b", branch, worktreePath)
	if err != nil {
		return nil, fmt.Errorf("git worktree add: %w: %s", err, res.Stderr)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("git worktree add exited %d: %s", res.ExitCode, res.Stderr)
	}

	return &Workspace{
		JobID:    jobID,
		Branch:   branch,
		Path:     worktreePath,
		RepoPath: repoPath,
	}, nil
}

// Cleanup removes the job's worktree and its temporary branch.
//
// It deliberately does not inherit cancellation from ctx. Cleanup runs from a
// deferred call on paths where the job was cancelled or timed out, so the
// context it is handed is usually already dead; inheriting that would make
// every git command fail instantly and leak the worktree exactly when cleanup
// matters most. Values on ctx are preserved so logging and tracing still work,
// and the operation stays bounded by its own 30-second deadline.
func (m *GitWorkspaceManager) Cleanup(ctx context.Context, ws *Workspace) error {
	if ws == nil {
		return nil
	}
	deadline, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	var errs []error

	// A command that runs and exits non-zero comes back as (result, nil) — the
	// failure is in ExitCode, not err — so both have to be checked or a failed
	// removal looks like success.
	res, err := m.runner.Run(deadline, ws.RepoPath, "git", "worktree", "remove", "--force", ws.Path)
	if err != nil || res.ExitCode != 0 {
		if rmErr := os.RemoveAll(ws.Path); rmErr != nil {
			errs = append(errs, fmt.Errorf("remove worktree dir %s: %w", ws.Path, rmErr))
		}
		// Deleting the directory behind git's back leaves the worktree still
		// registered: `git worktree list` keeps reporting it and `git branch -D`
		// can refuse to delete a branch git believes is checked out. Prune the
		// stale registration so the fallback is actually equivalent.
		if pruneRes, pruneErr := m.runner.Run(deadline, ws.RepoPath, "git", "worktree", "prune"); pruneErr != nil {
			errs = append(errs, fmt.Errorf("prune worktrees: %w", pruneErr))
		} else if pruneRes.ExitCode != 0 {
			errs = append(errs, fmt.Errorf("git worktree prune exited %d: %s", pruneRes.ExitCode, pruneRes.Stderr))
		}
	}

	res, err = m.runner.Run(deadline, ws.RepoPath, "git", "branch", "-D", ws.Branch)
	if err != nil {
		errs = append(errs, fmt.Errorf("delete branch %s: %w", ws.Branch, err))
	} else if res.ExitCode != 0 {
		errs = append(errs, fmt.Errorf("delete branch %s exited %d: %s", ws.Branch, res.ExitCode, res.Stderr))
	}

	// Join reports every failure rather than the first: a worktree that was
	// removed but whose branch survived is still a leak the caller must see.
	return errors.Join(errs...)
}
