package implementor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrPathEscape   = errors.New("path escapes workspace")
	ErrProtected    = errors.New("path is protected")
	ErrTooManyOps   = errors.New("too many operations in one attempt")
	ErrOpTooLarge   = errors.New("operation exceeds max size")
	ErrEmptyPath    = errors.New("path is empty")
	ErrInvalidOp    = errors.New("invalid operation kind")
)

const (
	OpWrite  = "write"
	OpAppend = "append"
	OpDelete = "delete"

	MaxOpsPerAttempt = 50
	MaxOpSizeBytes   = 1 << 20 // 1 MiB per file
)

var protectedPaths = []string{
	".git",
	".env",
}

type Operation struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
}

type Implementor struct {
	Workspace string
}

func New(workspace string) *Implementor {
	return &Implementor{Workspace: workspace}
}

func (i *Implementor) Apply(ops []Operation) error {
	if len(ops) == 0 {
		return nil
	}
	if len(ops) > MaxOpsPerAttempt {
		return fmt.Errorf("%w: %d ops (max %d)", ErrTooManyOps, len(ops), MaxOpsPerAttempt)
	}
	// os.Root confines every subsequent operation to the workspace directory:
	// the traversal is performed by the OS relative to this root, so a symlink
	// inside the workspace cannot redirect a write outside it. Lexical checks
	// alone (Clean/Abs comparisons) cannot provide that guarantee, because they
	// inspect the path string and never resolve what is actually on disk.
	root, err := os.OpenRoot(i.Workspace)
	if err != nil {
		return fmt.Errorf("open workspace root: %w", err)
	}
	defer root.Close()

	for i2, op := range ops {
		if err := i.applyOne(root, op); err != nil {
			return fmt.Errorf("op %d (%s %s): %w", i2, op.Kind, op.Path, err)
		}
	}
	return nil
}

func (i *Implementor) applyOne(root *os.Root, op Operation) error {
	rel, err := i.resolvePath(op.Path)
	if err != nil {
		return err
	}
	if len(op.Content) > MaxOpSizeBytes {
		return ErrOpTooLarge
	}
	switch op.Kind {
	case OpWrite:
		if dir := filepath.Dir(rel); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return escapeErr(err)
			}
		}
		if err := root.WriteFile(rel, []byte(op.Content), 0o644); err != nil {
			return escapeErr(err)
		}
		return nil
	case OpAppend:
		f, err := root.OpenFile(rel, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return escapeErr(err)
		}
		defer f.Close()
		if _, err := f.WriteString(op.Content); err != nil {
			return err
		}
		return nil
	case OpDelete:
		if err := root.Remove(rel); err != nil && !os.IsNotExist(err) {
			return escapeErr(err)
		}
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidOp, op.Kind)
	}
}

// escapeErr maps os.Root's containment refusals onto ErrPathEscape so callers
// keep seeing a single sentinel for "this operation tried to leave the
// workspace", however it tried to do it.
func escapeErr(err error) error {
	if errors.Is(err, os.ErrInvalid) || isInsecurePath(err) {
		return fmt.Errorf("%w: %v", ErrPathEscape, err)
	}
	return err
}

func isInsecurePath(err error) bool {
	// os.Root reports escape attempts (including via symlink) as a *PathError
	// whose message notes the path escapes the root; match defensively on both
	// the sentinel and the text so this holds across Go versions.
	return err != nil && strings.Contains(err.Error(), "escapes from parent")
}

// resolvePath validates the requested path and returns it cleaned and relative
// to the workspace. Containment itself is enforced by os.Root at the point of
// use; the checks here reject obviously hostile input early and enforce the
// protected-path policy, which os.Root knows nothing about.
func (i *Implementor) resolvePath(rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", ErrEmptyPath
	}
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("%w: path contains a NUL byte", ErrPathEscape)
	}
	cleaned := filepath.Clean(rel)
	if strings.HasPrefix(cleaned, "..") || filepath.IsAbs(cleaned) {
		return "", ErrPathEscape
	}
	if err := checkProtected(cleaned); err != nil {
		return "", err
	}
	return cleaned, nil
}

// checkProtected rejects protected names in ANY path segment, case-folded.
// Anchoring at the root only would let `submodule/.git/hooks/pre-commit`
// through, and a case-sensitive compare would let `.GIT/hooks/pre-commit`
// through — which on a case-insensitive filesystem (APFS, HFS+, NTFS) is the
// very same file, turning the check into no protection at all where most
// developers run.
func checkProtected(cleaned string) error {
	for _, seg := range strings.Split(cleaned, string(filepath.Separator)) {
		lower := strings.ToLower(seg)
		for _, p := range protectedPaths {
			if lower == p || strings.HasPrefix(lower, p+".") {
				return fmt.Errorf("%w: %s", ErrProtected, cleaned)
			}
		}
	}
	return nil
}
