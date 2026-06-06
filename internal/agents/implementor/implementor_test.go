package implementor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestApply_WriteAndDelete(t *testing.T) {
	dir := t.TempDir()
	i := New(dir)

	err := i.Apply([]Operation{
		{Kind: OpWrite, Path: "hello.txt", Content: "hi"},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil || string(body) != "hi" {
		t.Errorf("got body=%q err=%v", body, err)
	}

	err = i.Apply([]Operation{{Kind: OpDelete, Path: "hello.txt"}})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); !os.IsNotExist(err) {
		t.Errorf("expected file removed, got err=%v", err)
	}
}

func TestApply_PathEscape(t *testing.T) {
	i := New(t.TempDir())
	err := i.Apply([]Operation{{Kind: OpWrite, Path: "../evil.txt", Content: "x"}})
	if !errors.Is(err, ErrPathEscape) {
		t.Errorf("got %v, want ErrPathEscape", err)
	}
}

func TestApply_AbsolutePath(t *testing.T) {
	i := New(t.TempDir())
	err := i.Apply([]Operation{{Kind: OpWrite, Path: "/etc/passwd", Content: "x"}})
	if !errors.Is(err, ErrPathEscape) {
		t.Errorf("got %v, want ErrPathEscape", err)
	}
}

func TestApply_ProtectedPath(t *testing.T) {
	i := New(t.TempDir())
	err := i.Apply([]Operation{{Kind: OpWrite, Path: ".git/config", Content: "x"}})
	if !errors.Is(err, ErrProtected) {
		t.Errorf("got %v, want ErrProtected", err)
	}
}

func TestApply_TooManyOps(t *testing.T) {
	i := New(t.TempDir())
	ops := make([]Operation, MaxOpsPerAttempt+1)
	for k := range ops {
		ops[k] = Operation{Kind: OpWrite, Path: "x.txt", Content: "y"}
	}
	err := i.Apply(ops)
	if !errors.Is(err, ErrTooManyOps) {
		t.Errorf("got %v, want ErrTooManyOps", err)
	}
}

func TestApply_OpTooLarge(t *testing.T) {
	i := New(t.TempDir())
	big := make([]byte, MaxOpSizeBytes+1)
	err := i.Apply([]Operation{{Kind: OpWrite, Path: "big.txt", Content: string(big)}})
	if !errors.Is(err, ErrOpTooLarge) {
		t.Errorf("got %v, want ErrOpTooLarge", err)
	}
}

func TestApply_InvalidKind(t *testing.T) {
	i := New(t.TempDir())
	err := i.Apply([]Operation{{Kind: "yolo", Path: "x.txt"}})
	if !errors.Is(err, ErrInvalidOp) {
		t.Errorf("got %v, want ErrInvalidOp", err)
	}
}
