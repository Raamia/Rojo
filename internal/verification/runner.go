package verification

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Raamia/Rojo/internal/execution"
)

// ErrNoChecks guards the most dangerous failure mode this package has. An empty
// Report satisfies AllPassed, so a runner configured with no checks would
// certify every job as verified while running nothing at all. Refuse instead.
var ErrNoChecks = errors.New("no verification checks configured")

// MaxOutputBytes caps how much of a check's output is retained. Test output is
// unbounded and ends up in an event payload and a database row, so a runaway
// log must not be able to consume memory or bloat storage.
const MaxOutputBytes = 64 << 10

// Check is one deterministic quality gate.
type Check struct {
	Name    string
	Command string
	Args    []string

	// FailOnOutput marks a check that reports problems by printing them while
	// still exiting zero. `gofmt -l` is the canonical example: it lists every
	// unformatted file and exits 0, so judging it by exit code alone would let
	// all formatting violations pass.
	FailOnOutput bool

	// AllowExit maps non-zero exit codes that do not mean failure to the note
	// explaining them. pytest is the motivating case: it exits 5 when no tests
	// were collected, which is "there was nothing to check", not "the checks
	// failed" — treating it as failure would fail every testless Python repo.
	AllowExit map[int]string

	// NoteWhen inspects a passing check's output and returns a caveat, or "".
	// It lets a preset flag things like `go test` passing because there were
	// no test files: exit 0, technically green, but "verified" then means
	// only "compiled", and the record should say so.
	NoteWhen func(output string) string
}

// DefaultChecks is the Go gate: formatting, static analysis, tests.
//
// gofmt runs against "." rather than "./..." because it takes paths, not
// package patterns. go vet and go test take patterns.
func DefaultChecks() []Check {
	return []Check{
		{Name: "gofmt", Command: "gofmt", Args: []string{"-l", "."}, FailOnOutput: true},
		{Name: "go vet", Command: "go", Args: []string{"vet", "./..."}},
		{Name: "go test", Command: "go", Args: []string{"test", "./..."}, NoteWhen: goTestNote},
	}
}

// goTestNote flags the pass that means less than it looks like it means:
// `go test ./...` exits 0 for a module with no test files at all, so the gate
// proved compilation and nothing else. The reviewer and the user both need
// that distinction — "verified" and "compiled" are different claims.
func goTestNote(output string) string {
	if !strings.Contains(output, "[no test files]") {
		return ""
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "ok") {
			return "" // at least one package genuinely ran tests
		}
	}
	return "no test files found: the gate proves compilation only"
}

// Runner executes checks in a directory and reports the outcome.
//
// It does not decide what happens next — that is the orchestrator's job — but
// it does guarantee that a check which could not be run is reported as failed,
// never as passed.
type Runner struct {
	runner execution.CommandRunner
	checks []Check
}

func NewRunner(r execution.CommandRunner, checks ...Check) *Runner {
	if len(checks) == 0 {
		checks = DefaultChecks()
	}
	return &Runner{runner: r, checks: checks}
}

// Verify runs every configured check against dir and returns their results.
//
// Checks all run even after one fails: a caller fixing a change wants the whole
// list of what is wrong, not just the first problem. An error is returned only
// when the run could not be performed meaningfully — no checks configured, or
// the context ended — because "verification did not happen" must be
// distinguishable from "verification found nothing wrong".
func (v *Runner) Verify(ctx context.Context, dir string) (Report, error) {
	if len(v.checks) == 0 {
		return Report{}, ErrNoChecks
	}

	report := Report{Results: make([]Result, 0, len(v.checks))}
	for _, c := range v.checks {
		if err := ctx.Err(); err != nil {
			return report, fmt.Errorf("verification cancelled after %d checks: %w", len(report.Results), err)
		}
		report.Results = append(report.Results, runCheck(ctx, v.runner, dir, c))
	}
	return report, nil
}

// runCheck is shared by Runner (fixed checks) and AutoRunner (detected checks).
func runCheck(ctx context.Context, runner execution.CommandRunner, dir string, c Check) Result {
	start := time.Now()
	res, err := runner.Run(ctx, dir, c.Command, c.Args...)

	result := Result{
		Check:    c.Name,
		Duration: time.Since(start).Milliseconds(),
		Output:   truncate(combineOutput(res)),
	}

	switch {
	case err != nil:
		// A check that could not run at all — missing binary, timeout,
		// disallowed command — is a failed check, not a passed one. Treating it
		// as anything else would make "we could not verify this" and "this
		// verified clean" indistinguishable to the reviewer.
		result.Passed = false
		result.Output = truncate(joinNonEmpty(result.Output, err.Error()))
	case res.ExitCode != 0:
		if note, tolerated := c.AllowExit[res.ExitCode]; tolerated {
			result.Passed = true
			result.Note = note
		} else {
			result.Passed = false
		}
	case c.FailOnOutput && strings.TrimSpace(res.Stdout) != "":
		result.Passed = false
	default:
		result.Passed = true
	}
	if result.Passed && result.Note == "" && c.NoteWhen != nil {
		result.Note = c.NoteWhen(result.Output)
	}
	return result
}

func combineOutput(res execution.CommandResult) string {
	return joinNonEmpty(strings.TrimSpace(res.Stdout), strings.TrimSpace(res.Stderr))
}

func joinNonEmpty(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n")
}

func truncate(s string) string {
	if len(s) <= MaxOutputBytes {
		return s
	}
	return s[:MaxOutputBytes] + "\n... output truncated"
}

// Summary renders the failed checks for a log line or an error message.
func (r Report) Summary() string {
	var failed []string
	for _, res := range r.Results {
		if !res.Passed {
			failed = append(failed, res.Check)
		}
	}
	if len(failed) == 0 {
		return fmt.Sprintf("all %d checks passed", len(r.Results))
	}
	return fmt.Sprintf("%d of %d checks failed: %s",
		len(failed), len(r.Results), strings.Join(failed, ", "))
}
