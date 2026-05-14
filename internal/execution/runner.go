package execution

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

type CommandResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
	TimedOut bool
}

type CommandRunner interface {
	Run(ctx context.Context, workingDir string, command string, args ...string) (CommandResult, error)
}

type ExecRunner struct{}

func NewExecRunner() *ExecRunner {
	return &ExecRunner{}
}

func (r *ExecRunner) Run(ctx context.Context, workingDir string, command string, args ...string) (CommandResult, error) {
	start := time.Now()

	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = workingDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	result := CommandResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		return result, err
	}
	return result, nil
}
