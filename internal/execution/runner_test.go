package execution

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExecRunner_Success(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping unix-shell test on windows")
	}
	r := NewExecRunner()
	res, err := r.Run(context.Background(), "", "echo", "hello")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "hello") {
		t.Errorf("stdout %q missing 'hello'", res.Stdout)
	}
}

func TestExecRunner_NonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping unix-shell test on windows")
	}
	r := NewExecRunner()
	res, err := r.Run(context.Background(), "", "sh", "-c", "exit 3")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code %d, want 3", res.ExitCode)
	}
}

func TestSafeRunner_DisallowedCommand(t *testing.T) {
	safe := NewSafeRunner(NewExecRunner(), NewAllowlist("echo"), 5*time.Second)
	_, err := safe.Run(context.Background(), "", "rm", "-rf", "/")
	if !errors.Is(err, ErrCommandNotAllowed) {
		t.Fatalf("got %v, want ErrCommandNotAllowed", err)
	}
}

func TestSafeRunner_AllowedCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping unix-shell test on windows")
	}
	safe := NewSafeRunner(NewExecRunner(), NewAllowlist("echo"), 5*time.Second)
	res, err := safe.Run(context.Background(), "", "echo", "ok")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(res.Stdout, "ok") {
		t.Errorf("stdout %q missing 'ok'", res.Stdout)
	}
}

func TestSafeRunner_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping unix-shell test on windows")
	}
	safe := NewSafeRunner(NewExecRunner(), NewAllowlist("sleep"), 50*time.Millisecond)
	res, err := safe.Run(context.Background(), "", "sleep", "2")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v, want ErrTimeout", err)
	}
	if !res.TimedOut {
		t.Error("expected TimedOut to be true")
	}
}

func TestSafeRunner_CancelledByCaller(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping unix-shell test on windows")
	}
	safe := NewSafeRunner(NewExecRunner(), NewAllowlist("sleep"), 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	var runErr error
	go func() {
		_, runErr = safe.Run(ctx, "", "sleep", "2")
		close(done)
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("command did not stop after cancel")
	}
	if runErr == nil {
		t.Error("expected error after cancel, got nil")
	}
}
