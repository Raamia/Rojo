package execution

import (
	"context"
	"strings"
	"testing"
	"time"
)

// recordingRunner captures the command + args it is asked to run instead of
// executing anything, so DockerRunner's argv construction can be asserted
// without a docker daemon.
type recordingRunner struct {
	workingDir string
	command    string
	args       []string
}

func (r *recordingRunner) Run(_ context.Context, workingDir, command string, args ...string) (CommandResult, error) {
	r.workingDir = workingDir
	r.command = command
	r.args = args
	return CommandResult{ExitCode: 0}, nil
}

func TestDockerRunner_BuildsExpectedArgv(t *testing.T) {
	rec := &recordingRunner{}
	d := NewDockerRunner(rec, DockerOptions{}) // all defaults

	if _, err := d.Run(context.Background(), "/work/job-1", "go", "test", "./..."); err != nil {
		t.Fatalf("run: %v", err)
	}

	if rec.command != "docker" {
		t.Errorf("inner command = %q, want docker", rec.command)
	}
	if rec.workingDir != "" {
		t.Errorf("inner workingDir = %q, want empty (docker manages the mount)", rec.workingDir)
	}

	got := strings.Join(rec.args, " ")
	wantContains := []string{
		"run --rm",
		"--memory 512m",
		"--cpus 1",
		"--network none",
		"--workdir /workspace",
		"-v /work/job-1:/workspace",
		"golang:1.23-alpine go test ./...",
	}
	for _, w := range wantContains {
		if !strings.Contains(got, w) {
			t.Errorf("argv %q missing %q", got, w)
		}
	}

	// The user's command + args must come AFTER the image, not before.
	imageIdx := indexOf(rec.args, "golang:1.23-alpine")
	cmdIdx := indexOf(rec.args, "go")
	if imageIdx < 0 || cmdIdx < 0 || cmdIdx != imageIdx+1 {
		t.Errorf("expected user command immediately after image; args=%v", rec.args)
	}
}

func TestDockerRunner_HonorsCustomOptions(t *testing.T) {
	rec := &recordingRunner{}
	d := NewDockerRunner(rec, DockerOptions{
		Image:   "alpine:3.20",
		Memory:  "256m",
		CPUs:    "2",
		Network: "bridge",
		Timeout: 30 * time.Second,
	})

	if _, err := d.Run(context.Background(), "/w", "echo", "hi"); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := strings.Join(rec.args, " ")
	for _, w := range []string{"--memory 256m", "--cpus 2", "--network bridge", "alpine:3.20 echo hi"} {
		if !strings.Contains(got, w) {
			t.Errorf("argv %q missing %q", got, w)
		}
	}
}

func indexOf(ss []string, target string) int {
	for i, s := range ss {
		if s == target {
			return i
		}
	}
	return -1
}
