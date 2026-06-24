package verification

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Raamia/Rojo/internal/execution"
)

// AutoCommands is every command a detected check can run. The verification
// runner's allowlist has to cover exactly this set — detection chooses *which*
// of these run, never anything outside it, so a hostile repository can select a
// preset but cannot name a command.
func AutoCommands() []string {
	return []string{"go", "gofmt", "npm", "pytest", "cargo"}
}

// npmPlaceholder is the test script `npm init` writes into every new
// package.json. It is not a test suite, it is the absence of one — running it
// fails by design, which would fail every freshly-initialised Node repo.
const npmPlaceholder = `echo "Error: no test specified" && exit 1`

// AutoRunner detects what kind of repository a worktree is and runs the checks
// that fit it.
//
// Detection happens per Verify call, against the worktree, not once at
// startup: every job may point at a different repository, and the worktree is
// the copy whose manifests are authoritative — including any the implementor
// just created.
//
// The alternative — configurable check commands — was rejected on purpose. A
// per-repo config file would let the repository under test choose what the
// server executes, which breaks the allowlist rule; a server-side config knob
// is indirection for something detection answers better. Presets keep every
// runnable command a fixed string in this file.
type AutoRunner struct {
	runner execution.CommandRunner
	// lookPath is exec.LookPath, injectable so tests can decide which tools
	// "exist" without depending on the machine they run on.
	lookPath func(string) (string, error)
}

func NewAutoRunner(r execution.CommandRunner) *AutoRunner {
	return &AutoRunner{runner: r, lookPath: exec.LookPath}
}

// Verify detects the repository's stacks and runs their checks.
//
// A repository nothing was recognised in does not fail — a docs-only repo is
// legitimate — but it does not silently pass either: the report carries a
// result saying no checks ran, so "verified" is never quietly downgraded to
// "nothing was checked" without the record showing it. The reviewer sees that
// note, and so does anyone reading the job.
func (a *AutoRunner) Verify(ctx context.Context, dir string) (Report, error) {
	checks, notes := a.detect(dir)

	report := Report{Results: make([]Result, 0, len(checks)+len(notes))}
	report.Results = append(report.Results, notes...)
	for _, c := range checks {
		if err := ctx.Err(); err != nil {
			return report, fmt.Errorf("verification cancelled after %d checks: %w", len(report.Results), err)
		}
		report.Results = append(report.Results, runCheck(ctx, a.runner, dir, c))
	}
	return report, nil
}

// detect returns the checks worth running plus note-results for stacks that
// were recognised but cannot run here (tool not installed, no real test
// script). Notes are results rather than log lines so they survive into the
// event record and the reviewer's evidence.
func (a *AutoRunner) detect(dir string) ([]Check, []Result) {
	var (
		checks []Check
		notes  []Result
	)

	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	installed := func(tool string) bool {
		_, err := a.lookPath(tool)
		return err == nil
	}
	note := func(check, text string) {
		notes = append(notes, Result{Check: check, Passed: true, Note: text})
	}

	recognised := false

	if exists("go.mod") {
		recognised = true
		if installed("go") {
			checks = append(checks, DefaultChecks()...)
		} else {
			note("go", "go.mod found but the Go toolchain is not installed on this host; Go checks skipped")
		}
	}

	if exists("package.json") {
		recognised = true
		switch script, err := npmTestScript(filepath.Join(dir, "package.json")); {
		case err != nil:
			// An unparseable manifest is the repository's problem, not the
			// host's: surface it as a failure so it blocks, the same way a
			// syntax error in a test file would.
			notes = append(notes, Result{Check: "npm", Passed: false,
				Output: "package.json is not valid JSON: " + err.Error()})
		case script == "" || script == npmPlaceholder:
			note("npm", "package.json has no real test script; Node checks skipped")
		case !installed("npm"):
			note("npm", "package.json found but npm is not installed on this host; Node checks skipped")
		default:
			checks = append(checks, Check{
				Name: "npm test", Command: "npm", Args: []string{"test", "--silent"},
			})
		}
	}

	if exists("pyproject.toml") || exists("requirements.txt") || exists("setup.py") {
		recognised = true
		if installed("pytest") {
			checks = append(checks, Check{
				Name: "pytest", Command: "pytest", Args: []string{"-q"},
				// pytest exits 5 when it collected nothing to run. That is the
				// Python analogue of a Go module with no test files — an empty
				// gate, not a failed one — and failing on it would fail every
				// testless Python repo.
				AllowExit: map[int]string{5: "no tests collected: the gate ran nothing for Python"},
			})
		} else {
			note("pytest", "Python manifest found but pytest is not installed on this host; Python checks skipped")
		}
	}

	if exists("Cargo.toml") {
		recognised = true
		if installed("cargo") {
			checks = append(checks, Check{Name: "cargo test", Command: "cargo", Args: []string{"test"}})
		} else {
			note("cargo", "Cargo.toml found but cargo is not installed on this host; Rust checks skipped")
		}
	}

	if !recognised {
		note("detect", "no recognised toolchain (go.mod, package.json, pyproject.toml, requirements.txt, setup.py, Cargo.toml): no checks were run")
	}
	return checks, notes
}

// npmTestScript reads the "test" script out of a package.json, or "" if there
// is none.
func npmTestScript(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		return "", err
	}
	return strings.TrimSpace(pkg.Scripts["test"]), nil
}
