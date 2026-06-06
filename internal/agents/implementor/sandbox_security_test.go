package implementor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The implementor is a security boundary: it applies file operations proposed
// by a model, so "the write cannot leave the workspace" has to hold against a
// hostile repository, not just against hostile path strings. These tests cover
// the escape vectors that lexical validation alone (Clean/Abs comparisons)
// cannot stop, because they depend on what is actually on disk.

func newWorkspace(t *testing.T) (workspace, outside string) {
	t.Helper()
	base := t.TempDir()
	workspace = filepath.Join(base, "workspace")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{workspace, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return workspace, outside
}

// A symlinked *directory* checked into the repo must not redirect writes.
// git tracks symlinks, so an attacker-authored repo can ship one.
func TestSandbox_SymlinkedDirectoryCannotRedirectWrite(t *testing.T) {
	ws, outside := newWorkspace(t)
	if err := os.Symlink(outside, filepath.Join(ws, "docs")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	err := New(ws).Apply([]Operation{{Kind: OpWrite, Path: "docs/pwned.txt", Content: "escaped"}})
	if err == nil {
		t.Fatal("write through a symlinked directory succeeded; the sandbox was escaped")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "pwned.txt")); statErr == nil {
		t.Fatalf("file was created outside the workspace: %v", err)
	}
}

// A symlinked *file* must not allow overwriting a host file.
func TestSandbox_SymlinkedFileCannotOverwriteOutsideTarget(t *testing.T) {
	ws, outside := newWorkspace(t)
	target := filepath.Join(outside, "authorized_keys")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(ws, "README.md")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	err := New(ws).Apply([]Operation{{Kind: OpWrite, Path: "README.md", Content: "ATTACKER-KEY"}})
	if !errors.Is(err, ErrPathEscape) {
		t.Errorf("got %v, want ErrPathEscape", err)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "original" {
		t.Fatalf("file outside the workspace was overwritten: %q", got)
	}
}

// Append must be contained too — it opens with O_CREATE and would otherwise
// follow the same symlink.
func TestSandbox_AppendCannotFollowSymlinkOutside(t *testing.T) {
	ws, outside := newWorkspace(t)
	target := filepath.Join(outside, "secrets.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(ws, "link.txt")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	err := New(ws).Apply([]Operation{{Kind: OpAppend, Path: "link.txt", Content: "appended"}})
	if !errors.Is(err, ErrPathEscape) {
		t.Errorf("got %v, want ErrPathEscape", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "original" {
		t.Fatalf("appended through a symlink to outside the workspace: %q", got)
	}
}

// On APFS/HFS+/NTFS ".GIT" and ".git" are the same directory, so a
// case-sensitive protected-path check is no protection at all on the platforms
// most developers use.
func TestSandbox_ProtectedPathsAreCaseInsensitive(t *testing.T) {
	ws, _ := newWorkspace(t)
	imp := New(ws)

	for _, p := range []string{
		".GIT/config", ".Git/hooks/pre-commit", ".gIt/hooks/post-checkout",
		".git/config", ".ENV", ".Env", ".eNv", ".env",
	} {
		t.Run(p, func(t *testing.T) {
			err := imp.Apply([]Operation{{Kind: OpWrite, Path: p, Content: "payload"}})
			if !errors.Is(err, ErrProtected) {
				t.Errorf("path %q: got %v, want ErrProtected", p, err)
			}
		})
	}
}

// Anchoring the check at the repo root only would leave a submodule's git dir
// and non-root env files writable.
func TestSandbox_ProtectedPathsMatchAnySegment(t *testing.T) {
	ws, _ := newWorkspace(t)
	imp := New(ws)

	for _, p := range []string{
		"submodule/.git/hooks/pre-commit",
		"testdata/repo/.git/config",
		"deploy/.env",
		"config/.env.production",
	} {
		t.Run(p, func(t *testing.T) {
			err := imp.Apply([]Operation{{Kind: OpWrite, Path: p, Content: "payload"}})
			if !errors.Is(err, ErrProtected) {
				t.Errorf("path %q: got %v, want ErrProtected", p, err)
			}
		})
	}
}

// A real .git directory must survive a case-variant write attempt.
func TestSandbox_CaseVariantCannotWriteIntoRealGitDir(t *testing.T) {
	ws, _ := newWorkspace(t)
	hooks := filepath.Join(ws, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}

	err := New(ws).Apply([]Operation{{Kind: OpWrite, Path: ".GIT/hooks/pre-commit", Content: "#!/bin/sh\nid\n"}})
	if !errors.Is(err, ErrProtected) {
		t.Errorf("got %v, want ErrProtected", err)
	}
	if _, statErr := os.Stat(filepath.Join(hooks, "pre-commit")); statErr == nil {
		t.Fatal("a git hook was written via a case variant — this is an RCE primitive")
	}
}

// Lexical traversal remains blocked (this was already correct; pinned here so a
// refactor of resolvePath cannot regress it).
func TestSandbox_LexicalTraversalStillBlocked(t *testing.T) {
	ws, _ := newWorkspace(t)
	imp := New(ws)

	for _, p := range []string{
		"../escape.txt", "../../escape.txt", "a/../../escape.txt",
		"/etc/passwd", "/tmp/escape.txt",
	} {
		t.Run(p, func(t *testing.T) {
			if err := imp.Apply([]Operation{{Kind: OpWrite, Path: p, Content: "x"}}); !errors.Is(err, ErrPathEscape) {
				t.Errorf("path %q: got %v, want ErrPathEscape", p, err)
			}
		})
	}
}

// A NUL byte must be rejected explicitly rather than reaching a syscall.
func TestSandbox_NullByteInPathRejected(t *testing.T) {
	ws, _ := newWorkspace(t)
	err := New(ws).Apply([]Operation{{Kind: OpWrite, Path: "ok\x00/evil.txt", Content: "x"}})
	if !errors.Is(err, ErrPathEscape) {
		t.Errorf("got %v, want ErrPathEscape", err)
	}
}

// Ordinary nested writes must still work — containment must not break the
// feature it protects.
func TestSandbox_LegitimateNestedWriteStillWorks(t *testing.T) {
	ws, _ := newWorkspace(t)
	imp := New(ws)

	if err := imp.Apply([]Operation{
		{Kind: OpWrite, Path: "cmd/api/main.go", Content: "package main\n"},
		{Kind: OpAppend, Path: "cmd/api/main.go", Content: "// appended\n"},
	}); err != nil {
		t.Fatalf("legitimate nested write failed: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(ws, "cmd", "api", "main.go"))
	if err != nil {
		t.Fatalf("expected file to exist: %v", err)
	}
	if !strings.Contains(string(got), "package main") || !strings.Contains(string(got), "// appended") {
		t.Errorf("unexpected content: %q", got)
	}

	if err := imp.Apply([]Operation{{Kind: OpDelete, Path: "cmd/api/main.go"}}); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "cmd", "api", "main.go")); !os.IsNotExist(err) {
		t.Error("expected file to be deleted")
	}
}
