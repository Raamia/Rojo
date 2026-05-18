package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (m *GitWorkspaceManager) Diff(ctx context.Context, ws *Workspace) (string, error) {
	if ws == nil {
		return "", ErrWorktreeNotFound
	}
	res, err := m.runner.Run(ctx, ws.Path, "git", "diff", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git diff: %w: %s", err, res.Stderr)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("git diff exited %d: %s", res.ExitCode, res.Stderr)
	}
	return res.Stdout, nil
}

func (m *GitWorkspaceManager) ListOrphans(ctx context.Context, repoPath string) ([]string, error) {
	if err := validateRepoPath(repoPath); err != nil {
		return nil, err
	}

	res, err := m.runner.Run(ctx, repoPath, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w", err)
	}

	tracked := make(map[string]struct{})
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			tracked[strings.TrimPrefix(line, "worktree ")] = struct{}{}
		}
	}

	entries, err := os.ReadDir(m.baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read base dir: %w", err)
	}

	var orphans []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(m.baseDir, entry.Name())
		if _, ok := tracked[path]; !ok {
			orphans = append(orphans, path)
		}
	}
	return orphans, nil
}
