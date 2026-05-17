package workspace

import (
	"context"
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

func (m *GitWorkspaceManager) Cleanup(ctx context.Context, ws *Workspace) error {
	if ws == nil {
		return nil
	}
	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if _, err := m.runner.Run(deadline, ws.RepoPath, "git", "worktree", "remove", "--force", ws.Path); err != nil {
		_ = os.RemoveAll(ws.Path)
	}
	if _, err := m.runner.Run(deadline, ws.RepoPath, "git", "branch", "-D", ws.Branch); err != nil {
		return fmt.Errorf("delete branch %s: %w", ws.Branch, err)
	}
	return nil
}
