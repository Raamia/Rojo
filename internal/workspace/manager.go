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
