package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Raamia/Rojo/internal/execution"
)

func TestGitWorkspaceManager_DiffShowsChanges(t *testing.T) {
	hasGit(t)
	repo := initTestRepo(t)
	base := t.TempDir()
	m := NewGitWorkspaceManager(execution.NewExecRunner(), base)

	ws, err := m.Create(context.Background(), "diff-job", repo)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer m.Cleanup(context.Background(), ws)

	// Modify a tracked file inside the isolated worktree.
	if err := os.WriteFile(filepath.Join(ws.Path, "README.md"), []byte("hello world changed"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, err := m.Diff(context.Background(), ws)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if diff == "" {
		t.Fatal("expected a non-empty diff after modifying README.md")
	}
	for _, want := range []string{"README.md", "+hello world changed", "-hello"} {
		if !contains(diff, want) {
			t.Errorf("diff missing %q:\n%s", want, diff)
		}
	}
}

func TestGitWorkspaceManager_DiffCleanWorktreeIsEmpty(t *testing.T) {
	hasGit(t)
	repo := initTestRepo(t)
	m := NewGitWorkspaceManager(execution.NewExecRunner(), t.TempDir())

	ws, err := m.Create(context.Background(), "clean-job", repo)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer m.Cleanup(context.Background(), ws)

	diff, err := m.Diff(context.Background(), ws)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if diff != "" {
		t.Errorf("expected empty diff on an unmodified worktree, got:\n%s", diff)
	}
}

func TestGitWorkspaceManager_DiffNilWorkspace(t *testing.T) {
	m := NewGitWorkspaceManager(execution.NewExecRunner(), t.TempDir())
	_, err := m.Diff(context.Background(), nil)
	if !errors.Is(err, ErrWorktreeNotFound) {
		t.Errorf("got %v, want ErrWorktreeNotFound", err)
	}
}

func TestGitWorkspaceManager_ListOrphans(t *testing.T) {
	hasGit(t)
	repo := initTestRepo(t)
	base := t.TempDir()
	m := NewGitWorkspaceManager(execution.NewExecRunner(), base)

	// A properly-created worktree is tracked by git and must NOT be an orphan.
	ws, err := m.Create(context.Background(), "tracked-job", repo)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer m.Cleanup(context.Background(), ws)

	// Simulate a crash-leftover: a stray dir under baseDir that git doesn't know.
	orphanPath := filepath.Join(base, "orphaned-job")
	if err := os.MkdirAll(orphanPath, 0o755); err != nil {
		t.Fatal(err)
	}

	orphans, err := m.ListOrphans(context.Background(), repo)
	if err != nil {
		t.Fatalf("list orphans: %v", err)
	}

	foundOrphan, foundTracked := false, false
	for _, o := range orphans {
		if o == orphanPath {
			foundOrphan = true
		}
		if o == ws.Path {
			foundTracked = true
		}
	}
	if !foundOrphan {
		t.Errorf("expected %s to be reported as an orphan; got %v", orphanPath, orphans)
	}
	if foundTracked {
		t.Errorf("tracked worktree %s was wrongly reported as an orphan", ws.Path)
	}
}

func TestGitWorkspaceManager_ListOrphansMissingBaseDir(t *testing.T) {
	hasGit(t)
	repo := initTestRepo(t)
	// baseDir that was never created.
	base := filepath.Join(t.TempDir(), "never-made")
	m := NewGitWorkspaceManager(execution.NewExecRunner(), base)

	orphans, err := m.ListOrphans(context.Background(), repo)
	if err != nil {
		t.Fatalf("expected nil error for a missing base dir, got %v", err)
	}
	if orphans != nil {
		t.Errorf("expected nil orphans for a missing base dir, got %v", orphans)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOfStr(haystack, needle) >= 0)
}

func indexOfStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
