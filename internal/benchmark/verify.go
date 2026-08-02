package benchmark

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Raamia/Rojo/internal/execution"
)

// GroundTruth is the verdict on whether a patch actually did the task.
type GroundTruth struct {
	// Passed is the only authoritative signal in this package: the hidden
	// assertion compiled and ran green against a clean repository with the
	// patch applied.
	Passed bool
	// Stage names where it stopped, so a failure is attributable: "apply",
	// "assert", or "" when it passed.
	Stage string
	// Output is the failing command's output, truncated for the report.
	Output string
}

// MaxGroundTruthOutput bounds what a failure carries into the report. A
// compiler error names its cause in the first lines; the rest is repetition.
const MaxGroundTruthOutput = 4 << 10

// CheckPatch decides whether patch actually accomplished the case's task.
//
// It works on a *clean* copy of the starting repository rather than on the
// job's worktree, which the pipeline has already deleted by now. That is also
// the more honest test: it proves the patch alone carries the change, with no
// help from state left behind in a checkout nobody will ever have again.
//
// The order matters. The patch is applied first and the assertion overlaid
// second, so a patch that tries to weaken or delete the hidden test cannot: the
// overlay writes the real assertion back over whatever is there.
func CheckPatch(
	ctx context.Context, runner execution.CommandRunner, c Case, patch string,
) (GroundTruth, error) {
	dir, err := os.MkdirTemp("", "rojo-bench-verify-")
	if err != nil {
		return GroundTruth{}, fmt.Errorf("create verify dir: %w", err)
	}
	defer os.RemoveAll(dir)

	if err := c.MaterializeRepo(dir); err != nil {
		return GroundTruth{}, err
	}

	if strings.TrimSpace(patch) != "" {
		if out, err := applyPatch(ctx, runner, dir, patch); err != nil {
			// A patch that will not apply is a failed task, not a broken
			// harness: whatever the job produced cannot be used.
			return GroundTruth{Stage: "apply", Output: truncate(out, MaxGroundTruthOutput)}, nil
		}
	}

	if err := c.OverlayAssertions(dir); err != nil {
		return GroundTruth{}, err
	}

	res, err := runner.Run(ctx, dir, "go", "test", "./...")
	if err != nil {
		return GroundTruth{}, fmt.Errorf("run assertion tests: %w", err)
	}
	if res.ExitCode != 0 {
		return GroundTruth{
			Stage:  "assert",
			Output: truncate(strings.TrimSpace(res.Stdout+"\n"+res.Stderr), MaxGroundTruthOutput),
		}, nil
	}
	return GroundTruth{Passed: true}, nil
}

// applyPatch writes the patch to a file and applies it with git.
//
// `git apply` is used rather than `patch` because the pipeline produces a git
// diff, including its rename and mode metadata, and git is the only thing that
// reads all of it. --whitespace=nowarn keeps a trailing-newline nit from
// showing up as a failure in the report.
func applyPatch(
	ctx context.Context, runner execution.CommandRunner, dir, patch string,
) (string, error) {
	file := filepath.Join(dir, ".rojo-bench.patch")
	if err := os.WriteFile(file, []byte(patch), 0o644); err != nil {
		return "", fmt.Errorf("write patch: %w", err)
	}
	defer os.Remove(file)

	res, err := runner.Run(ctx, dir, "git", "apply", "--whitespace=nowarn", file)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return strings.TrimSpace(res.Stdout + "\n" + res.Stderr),
			fmt.Errorf("git apply exited %d", res.ExitCode)
	}
	return "", nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... (truncated)"
}
