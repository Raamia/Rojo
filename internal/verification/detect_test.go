package verification

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/execution"
)

// newAutoRunner builds an AutoRunner whose view of "what is installed" is under
// the test's control, so results do not depend on the machine running them.
func newAutoRunner(r execution.CommandRunner, tools ...string) *AutoRunner {
	installed := map[string]bool{}
	for _, t := range tools {
		installed[t] = true
	}
	return &AutoRunner{runner: r, lookPath: func(name string) (string, error) {
		if installed[name] {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}}
}

func repoWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func resultNamed(rep Report, name string) (Result, bool) {
	for _, r := range rep.Results {
		if r.Check == name {
			return r, true
		}
	}
	return Result{}, false
}

func TestAuto_GoRepoRunsTheGoGate(t *testing.T) {
	dir := repoWith(t, map[string]string{"go.mod": "module m\n"})
	s := &stubRunner{}

	rep, err := newAutoRunner(s, "go", "gofmt").Verify(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(rep.Results) != 3 {
		t.Fatalf("got %d results, want the Go trio: %+v", len(rep.Results), rep.Results)
	}
	for _, want := range []string{"gofmt -l .", "go vet ./...", "go test ./..."} {
		if !contains(s.cmds, want) {
			t.Errorf("did not run %q; ran %v", want, s.cmds)
		}
	}
}

func TestAuto_NodeRepoRunsNpmTest(t *testing.T) {
	dir := repoWith(t, map[string]string{
		"package.json": `{"scripts":{"test":"vitest run"}}`,
	})
	s := &stubRunner{}

	rep, err := newAutoRunner(s, "npm").Verify(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !contains(s.cmds, "npm test --silent") {
		t.Errorf("npm test did not run; ran %v", s.cmds)
	}
	if _, ok := resultNamed(rep, "npm test"); !ok {
		t.Errorf("no npm result: %+v", rep.Results)
	}
}

// `npm init` writes a placeholder test script that fails by design. Running it
// would fail every freshly-initialised Node repo for not having tests yet.
func TestAuto_NpmPlaceholderScriptIsSkippedWithANote(t *testing.T) {
	dir := repoWith(t, map[string]string{
		"package.json": `{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`,
	})
	s := &stubRunner{}

	rep, err := newAutoRunner(s, "npm").Verify(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(s.cmds) != 0 {
		t.Errorf("ran %v, want nothing", s.cmds)
	}
	got, ok := resultNamed(rep, "npm")
	if !ok || !got.Passed || !strings.Contains(got.Note, "no real test script") {
		t.Errorf("want a passing note about the placeholder, got %+v", rep.Results)
	}
}

// A stack the host cannot run is a note, not a failure: the repository did
// nothing wrong, and failing it would make the job's outcome depend invisibly
// on what happens to be installed.
func TestAuto_MissingToolIsANoteNotAFailure(t *testing.T) {
	tests := map[string]struct {
		files map[string]string
		check string
	}{
		"npm absent":    {map[string]string{"package.json": `{"scripts":{"test":"jest"}}`}, "npm"},
		"pytest absent": {map[string]string{"requirements.txt": "flask\n"}, "pytest"},
		"cargo absent":  {map[string]string{"Cargo.toml": "[package]\n"}, "cargo"},
		"go absent":     {map[string]string{"go.mod": "module m\n"}, "go"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := repoWith(t, tt.files)
			s := &stubRunner{}
			rep, err := newAutoRunner(s /* nothing installed */).Verify(context.Background(), dir)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if len(s.cmds) != 0 {
				t.Errorf("ran %v with no tools installed", s.cmds)
			}
			got, ok := resultNamed(rep, tt.check)
			if !ok || !got.Passed || got.Note == "" {
				t.Errorf("want a passing note for %s, got %+v", tt.check, rep.Results)
			}
			if !rep.AllPassed() {
				t.Errorf("a missing tool must not fail the gate: %s", rep.Summary())
			}
		})
	}
}

// A repo nothing was recognised in must not silently pass as "verified". The
// record has to say no checks ran.
func TestAuto_UnknownStackSaysNoChecksRan(t *testing.T) {
	dir := repoWith(t, map[string]string{"README.md": "# docs only\n"})
	s := &stubRunner{}

	rep, err := newAutoRunner(s, "go", "npm", "pytest", "cargo").Verify(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(s.cmds) != 0 {
		t.Errorf("ran %v in an unrecognised repo", s.cmds)
	}
	got, ok := resultNamed(rep, "detect")
	if !ok || !strings.Contains(got.Note, "no checks were run") {
		t.Errorf("the record does not say nothing ran: %+v", rep.Results)
	}
}

// pytest exits 5 for "no tests collected" — the Python analogue of a Go module
// with no test files. An empty gate, not a failed one.
func TestAuto_PytestNoTestsCollectedPassesWithANote(t *testing.T) {
	dir := repoWith(t, map[string]string{"pyproject.toml": "[project]\n"})
	s := &stubRunner{results: map[string]execution.CommandResult{
		"pytest": {ExitCode: 5, Stdout: "no tests ran in 0.01s"},
	}}

	rep, err := newAutoRunner(s, "pytest").Verify(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	got, ok := resultNamed(rep, "pytest")
	if !ok {
		t.Fatalf("pytest did not run: %+v", rep.Results)
	}
	if !got.Passed || !strings.Contains(got.Note, "no tests collected") {
		t.Errorf("exit 5 should pass with a note, got %+v", got)
	}
}

// Exit 1 is real failure — only 5 is tolerated.
func TestAuto_PytestRealFailureStillFails(t *testing.T) {
	dir := repoWith(t, map[string]string{"pyproject.toml": "[project]\n"})
	s := &stubRunner{results: map[string]execution.CommandResult{
		"pytest": {ExitCode: 1, Stdout: "FAILED test_app.py::test_add"},
	}}

	rep, err := newAutoRunner(s, "pytest").Verify(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.AllPassed() {
		t.Errorf("a failing pytest run passed the gate: %+v", rep.Results)
	}
}

// A monorepo with several stacks runs every applicable gate.
func TestAuto_MultiStackRunsTheUnion(t *testing.T) {
	dir := repoWith(t, map[string]string{
		"go.mod":       "module m\n",
		"package.json": `{"scripts":{"test":"jest"}}`,
	})
	s := &stubRunner{}

	rep, err := newAutoRunner(s, "go", "gofmt", "npm").Verify(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(rep.Results) != 4 {
		t.Errorf("got %d results, want Go trio + npm: %+v", len(rep.Results), rep.Results)
	}
}

// A repository choosing a manifest must not be able to choose a command. Every
// command detection can produce has to be inside the published set, because
// that set is what the server's allowlist is built from.
func TestAuto_EveryDetectableCommandIsInAutoCommands(t *testing.T) {
	dir := repoWith(t, map[string]string{
		"go.mod":         "module m\n",
		"package.json":   `{"scripts":{"test":"jest"}}`,
		"pyproject.toml": "[project]\n",
		"Cargo.toml":     "[package]\n",
	})
	a := newAutoRunner(&stubRunner{}, "go", "gofmt", "npm", "pytest", "cargo")
	checks, _ := a.detect(dir)

	allowed := map[string]bool{}
	for _, c := range AutoCommands() {
		allowed[c] = true
	}
	for _, c := range checks {
		if !allowed[c.Command] {
			t.Errorf("detection produced command %q outside AutoCommands()", c.Command)
		}
	}
	if len(checks) == 0 {
		t.Fatal("nothing detected in a four-stack repo")
	}
}

// A broken manifest is the repository's fault and must block, exactly as a
// syntax error in a test file would.
func TestAuto_MalformedPackageJSONFailsTheGate(t *testing.T) {
	dir := repoWith(t, map[string]string{"package.json": `{"scripts": not json`})
	s := &stubRunner{}

	rep, err := newAutoRunner(s, "npm").Verify(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.AllPassed() {
		t.Errorf("a malformed package.json passed the gate: %+v", rep.Results)
	}
}

// --- integration: the real Go toolchain through the real detector ---

func TestAuto_Integration_RealGoRepo(t *testing.T) {
	for _, tool := range []string{"go", "gofmt", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	dir := repoWith(t, map[string]string{
		"go.mod":  "module autoprobe\n\ngo 1.25\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})

	runner := execution.NewSafeRunner(
		execution.NewExecRunner(), execution.NewAllowlist(AutoCommands()...), 3*time.Minute)
	rep, err := NewAutoRunner(runner).Verify(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !rep.AllPassed() {
		t.Fatalf("a clean Go repo failed: %s\n%+v", rep.Summary(), rep.Results)
	}
	// This repo has no tests, and the record must say so — that is the
	// difference between "verified" and "compiled".
	got, ok := resultNamed(rep, "go test")
	if !ok || !strings.Contains(got.Note, "no test files") {
		t.Errorf("no note for a testless module: %+v", got)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
