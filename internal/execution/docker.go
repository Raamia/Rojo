package execution

import (
	"context"
	"fmt"
	"time"
)

type DockerRunner struct {
	inner   CommandRunner
	image   string
	memory  string
	cpus    string
	network string
	timeout time.Duration
}

type DockerOptions struct {
	Image   string
	Memory  string
	CPUs    string
	Network string
	Timeout time.Duration
}

func NewDockerRunner(inner CommandRunner, opts DockerOptions) *DockerRunner {
	if opts.Image == "" {
		opts.Image = "golang:1.23-alpine"
	}
	if opts.Memory == "" {
		opts.Memory = "512m"
	}
	if opts.CPUs == "" {
		opts.CPUs = "1"
	}
	if opts.Network == "" {
		opts.Network = "none"
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Minute
	}
	return &DockerRunner{
		inner:   inner,
		image:   opts.Image,
		memory:  opts.Memory,
		cpus:    opts.CPUs,
		network: opts.Network,
		timeout: opts.Timeout,
	}
}

func (d *DockerRunner) Run(ctx context.Context, workingDir string, command string, args ...string) (CommandResult, error) {
	dockerArgs := []string{
		"run", "--rm",
		"--memory", d.memory,
		"--cpus", d.cpus,
		"--network", d.network,
		"--workdir", "/workspace",
		"-v", fmt.Sprintf("%s:/workspace", workingDir),
		d.image,
		command,
	}
	dockerArgs = append(dockerArgs, args...)

	runCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	return d.inner.Run(runCtx, "", "docker", dockerArgs...)
}
