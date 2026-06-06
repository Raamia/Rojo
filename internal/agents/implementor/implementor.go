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
	for i2, op := range ops {
		if err := i.applyOne(op); err != nil {
			return fmt.Errorf("op %d (%s %s): %w", i2, op.Kind, op.Path, err)
		}
	}
	return nil
}

func (i *Implementor) applyOne(op Operation) error {
	full, err := i.resolvePath(op.Path)
	if err != nil {
		return err
	}
	if len(op.Content) > MaxOpSizeBytes {
		return ErrOpTooLarge
	}
	switch op.Kind {
	case OpWrite:
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		return os.WriteFile(full, []byte(op.Content), 0o644)
	case OpAppend:
		f, err := os.OpenFile(full, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteString(op.Content)
		return err
	case OpDelete:
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidOp, op.Kind)
	}
}

func (i *Implementor) resolvePath(rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", ErrEmptyPath
	}
	cleaned := filepath.Clean(rel)
	if strings.HasPrefix(cleaned, "..") || filepath.IsAbs(cleaned) {
		return "", ErrPathEscape
	}
	for _, p := range protectedPaths {
		if cleaned == p || strings.HasPrefix(cleaned, p+string(filepath.Separator)) {
			return "", fmt.Errorf("%w: %s", ErrProtected, cleaned)
		}
	}
	full := filepath.Join(i.Workspace, cleaned)
	absWorkspace, err := filepath.Abs(i.Workspace)
	if err != nil {
		return "", err
	}
	absFull, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(absFull, absWorkspace+string(filepath.Separator)) && absFull != absWorkspace {
		return "", ErrPathEscape
	}
	return absFull, nil
}
