package execution

import (
	"context"
	"fmt"
	"time"
)

// DefaultImage carries the toolchain jobs are verified with. Keep it in step
// with the go directive in go.mod.
const DefaultImage = "golang:1.25-alpine"

// DockerRunner is built but not wired into cmd/api. Running each job's checks
// in a disposable container is the Phase 10 goal; doing it needs either
// Docker-in-Docker or a mounted docker socket, and mounting the socket hands
// the container effective root on the host — which is a decision to make
// deliberately, not a default to drift into.
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
		// Must be at least the Go version in go.mod. An older toolchain refuses
		// the module outright ("go.mod requires go >= 1.25.0"), so every check
		// would fail before running — and it would look like the code was
		// broken rather than the image.
		opts.Image = DefaultImage
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
