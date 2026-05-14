package execution

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	ErrCommandNotAllowed = errors.New("command not in allowlist")
	ErrTimeout           = errors.New("command timed out")
)

type Allowlist struct {
	commands map[string]struct{}
}

func NewAllowlist(commands ...string) *Allowlist {
	set := make(map[string]struct{}, len(commands))
	for _, c := range commands {
		set[c] = struct{}{}
	}
	return &Allowlist{commands: set}
}

func (a *Allowlist) Contains(command string) bool {
	_, ok := a.commands[command]
	return ok
}

type SafeRunner struct {
	inner     CommandRunner
	allowlist *Allowlist
	timeout   time.Duration
}

func NewSafeRunner(inner CommandRunner, allowlist *Allowlist, timeout time.Duration) *SafeRunner {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &SafeRunner{inner: inner, allowlist: allowlist, timeout: timeout}
}

func (s *SafeRunner) Run(ctx context.Context, workingDir string, command string, args ...string) (CommandResult, error) {
	if !s.allowlist.Contains(command) {
		return CommandResult{}, fmt.Errorf("%w: %s", ErrCommandNotAllowed, command)
	}

	runCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	result, err := s.inner.Run(runCtx, workingDir, command, args...)
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		result.TimedOut = true
		return result, ErrTimeout
	}
	return result, err
}
