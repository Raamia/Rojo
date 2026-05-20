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

	// git reports worktree paths in canonical (symlink-resolved) form, e.g.
	// /private/tmp/... where the configured baseDir is /tmp/... . Canonicalize
	// both sides before comparing, otherwise a legitimately-tracked worktree is
	// misclassified as an orphan whenever baseDir contains a symlink component
	// (the default /tmp/rojo-worktrees on macOS, where /tmp -> /private/tmp).
	tracked := make(map[string]struct{})
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			p := strings.TrimPrefix(line, "worktree ")
			tracked[realPath(p)] = struct{}{}
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
		if _, ok := tracked[realPath(path)]; !ok {
			// Return the path under the configured baseDir (not the resolved
			// form) so callers clean up using the path they know.
			orphans = append(orphans, path)
		}
	}
	return orphans, nil
}

// realPath resolves symlinks so paths from different sources (git's output vs.
// a baseDir join) can be compared. Falls back to the input if resolution fails
// (e.g. the path no longer exists).
func realPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}
