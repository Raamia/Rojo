package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var (
	ErrRepoNotFound     = errors.New("repository path does not exist")
	ErrNotAGitRepo      = errors.New("path is not a git repository")
	ErrPathTraversal    = errors.New("path escapes repository root")
	ErrWorktreeNotFound = errors.New("worktree not found")
)

type Workspace struct {
	JobID    string
	Branch   string
	Path     string
	RepoPath string
}

// Reconstruct returns the Workspace a run would have created for this job.
//
// Both the path and the branch derive from the job ID alone, which is what
// makes crash recovery possible without a schema: a process that died
// mid-job persisted nothing about its worktree, but a later run can still
// name it exactly and reclaim it.
func Reconstruct(baseDir, jobID, repoPath string) *Workspace {
	return &Workspace{
		JobID:    jobID,
		Branch:   branchPrefix + jobID,
		Path:     filepath.Join(baseDir, jobID),
		RepoPath: repoPath,
	}
}

type WorkspaceManager interface {
	Create(ctx context.Context, jobID, repoPath string) (*Workspace, error)
	Cleanup(ctx context.Context, ws *Workspace) error
	Diff(ctx context.Context, ws *Workspace) (string, error)
	ListOrphans(ctx context.Context, repoPath string) ([]string, error)
}

func validateRepoPath(repoPath string) error {
	if !filepath.IsAbs(repoPath) {
		return fmt.Errorf("%w: %s", ErrPathTraversal, repoPath)
	}
	info, err := os.Stat(repoPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", ErrRepoNotFound, repoPath)
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: not a directory", ErrRepoNotFound)
	}
	gitDir := filepath.Join(repoPath, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		return fmt.Errorf("%w: %s", ErrNotAGitRepo, repoPath)
	}
	return nil
}
