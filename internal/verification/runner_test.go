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

// stubRunner returns a canned result per command name.
type stubRunner struct {
	results map[string]execution.CommandResult
	errs    map[string]error
	dirs    []string
	cmds    []string
}

func (s *stubRunner) Run(_ context.Context, dir string, command string, args ...string) (execution.CommandResult, error) {
	s.dirs = append(s.dirs, dir)
	s.cmds = append(s.cmds, strings.TrimSpace(command+" "+strings.Join(args, " ")))
	return s.results[command], s.errs[command]
}

func TestVerify_AllChecksPass(t *testing.T) {
	s := &stubRunner{}
	report, err := NewRunner(s).Verify(context.Background(), "/work")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(report.Results) != 3 {
		t.Fatalf("got %d results, want 3", len(report.Results))
	}
	if !report.AllPassed() {
		t.Errorf("expected all checks to pass: %s", report.Summary())
	}
	for _, d := range s.dirs {
		if d != "/work" {
			t.Errorf("check ran in %q, want /work", d)
		}
	}
}

// gofmt -l lists unformatted files and exits 0. A check judged only by exit
// code would let every formatting violation through.
func TestVerify_GofmtFailsOnOutputDespiteZeroExit(t *testing.T) {
	s := &stubRunner{
		results: map[string]execution.CommandResult{
			"gofmt": {ExitCode: 0, Stdout: "internal/api/jobs.go\ninternal/jobs/job.go\n"},
		},
	}
	report, err := NewRunner(s).Verify(context.Background(), "/work")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if report.AllPassed() {
		t.Fatal("gofmt listed unformatted files but the report passed")
	}
	got := report.Results[0]
	if got.Check != "gofmt" || got.Passed {
		t.Errorf("gofmt result = %+v, want failed", got)
	}
	if !strings.Contains(got.Output, "jobs.go") {
		t.Errorf("output %q should name the offending files", got.Output)
	}
}

// A check that exits zero and prints nothing is a pass, even with FailOnOutput.
func TestVerify_GofmtPassesWhenSilent(t *testing.T) {
	s := &stubRunner{}
	report, _ := NewRunner(s).Verify(context.Background(), "/work")
	if !report.Results[0].Passed {
		t.Errorf("silent gofmt should pass, got %+v", report.Results[0])
	}
}

func TestVerify_NonZeroExitFails(t *testing.T) {
	s := &stubRunner{
		results: map[string]execution.CommandResult{
			"go": {ExitCode: 1, Stdout: "--- FAIL: TestThing", Stderr: "FAIL\tpkg\t0.1s"},
		},
	}
	report, err := NewRunner(s).Verify(context.Background(), "/work")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if report.AllPassed() {
		t.Fatal("a non-zero exit must fail the report")
	}
	// Both go vet and go test use the "go" command, so both fail here.
	for _, res := range report.Results[1:] {
		if res.Passed {
			t.Errorf("%s passed despite a non-zero exit", res.Check)
		}
		if !strings.Contains(res.Output, "FAIL") {
			t.Errorf("%s output %q should include the failure text", res.Check, res.Output)
		}
	}
}

// "We could not verify this" must never be reported as "this verified clean".
func TestVerify_UnrunnableCheckIsAFailureNotAPass(t *testing.T) {
	s := &stubRunner{
		errs: map[string]error{
			"go": execution.ErrCommandNotAllowed,
		},
	}
	report, err := NewRunner(s).Verify(context.Background(), "/work")
	if err != nil {
		t.Fatalf("verify returned an error rather than a failed check: %v", err)
	}
	if report.AllPassed() {
		t.Fatal("a check that could not run must not count as passed")
	}
	if !strings.Contains(report.Results[1].Output, "allowlist") {
		t.Errorf("output %q should explain why the check could not run", report.Results[1].Output)
	}
}

func TestVerify_TimeoutIsAFailedCheck(t *testing.T) {
	s := &stubRunner{errs: map[string]error{"go": execution.ErrTimeout}}
	report, err := NewRunner(s).Verify(context.Background(), "/work")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if report.AllPassed() {
		t.Fatal("a timed-out check must fail the report")
	}
}

// An empty Report satisfies AllPassed, so a runner with nothing configured
// would certify every job while running nothing.
func TestVerify_NoChecksIsRefusedRatherThanAVacuousPass(t *testing.T) {
	v := &Runner{runner: &stubRunner{}} // deliberately bypasses NewRunner's default
	report, err := v.Verify(context.Background(), "/work")
	if !errors.Is(err, ErrNoChecks) {
		t.Fatalf("got %v, want ErrNoChecks", err)
	}
	if report.AllPassed() && len(report.Results) == 0 {
		t.Log("confirmed: an empty report would otherwise have passed")
	}
}

func TestVerify_CancelledContextStopsAndReports(t *testing.T) {
	s := &stubRunner{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewRunner(s).Verify(ctx, "/work")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if len(s.cmds) != 0 {
		t.Errorf("no checks should have run, got %v", s.cmds)
	}
}

// Every check runs even after one fails: the caller wants the whole list.
func TestVerify_AllChecksRunEvenAfterAFailure(t *testing.T) {
	s := &stubRunner{
		results: map[string]execution.CommandResult{
			"gofmt": {Stdout: "bad.go"},
		},
	}
	report, _ := NewRunner(s).Verify(context.Background(), "/work")
	if len(report.Results) != 3 {
		t.Fatalf("got %d results, want all 3 checks to run", len(report.Results))
	}
	if len(s.cmds) != 3 {
		t.Errorf("commands run = %v, want 3", s.cmds)
	}
}

func TestVerify_OutputIsTruncated(t *testing.T) {
	s := &stubRunner{
		results: map[string]execution.CommandResult{
			"go": {ExitCode: 1, Stdout: strings.Repeat("x", MaxOutputBytes*2)},
		},
	}
	report, _ := NewRunner(s).Verify(context.Background(), "/work")
	for _, res := range report.Results[1:] {
		if len(res.Output) > MaxOutputBytes+64 {
			t.Errorf("%s output is %d bytes, want it capped near %d", res.Check, len(res.Output), MaxOutputBytes)
		}
		if !strings.Contains(res.Output, "truncated") {
			t.Errorf("%s truncated output should say so", res.Check)
		}
	}
}

func TestReport_Summary(t *testing.T) {
	pass := Report{Results: []Result{{Check: "gofmt", Passed: true}, {Check: "go test", Passed: true}}}
	if got := pass.Summary(); !strings.Contains(got, "all 2 checks passed") {
		t.Errorf("summary = %q", got)
	}
	fail := Report{Results: []Result{{Check: "gofmt", Passed: true}, {Check: "go test"}}}
	if got := fail.Summary(); !strings.Contains(got, "go test") || !strings.Contains(got, "1 of 2") {
		t.Errorf("summary = %q", got)
	}
}

// End to end against the real toolchain in a real module.
func TestVerify_RealGoModule(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module verifyprobe\n\ngo 1.25\n")
	write("main.go", "package main\n\nfunc main() {}\n")
	write("main_test.go", "package main\n\nimport \"testing\"\n\nfunc TestOK(t *testing.T) {}\n")

	runner := execution.NewSafeRunner(
		execution.NewExecRunner(),
		execution.NewAllowlist("go", "gofmt"),
		2*time.Minute,
	)

	report, err := NewRunner(runner).Verify(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !report.AllPassed() {
		for _, r := range report.Results {
			t.Logf("%s passed=%v output=%q", r.Check, r.Passed, r.Output)
		}
		t.Fatalf("clean module should pass: %s", report.Summary())
	}

	// Now break the formatting and confirm gofmt catches it.
	write("bad.go", "package main\nfunc  badlyFormatted( ) {}\n")
	report, err = NewRunner(runner).Verify(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if report.AllPassed() {
		t.Fatal("an unformatted file should fail gofmt")
	}
	if report.Results[0].Check != "gofmt" || report.Results[0].Passed {
		t.Errorf("expected gofmt to fail, got %+v", report.Results[0])
	}

	// And a failing test must fail the report.
	write("bad.go", "package main\n")
	write("fail_test.go", "package main\n\nimport \"testing\"\n\nfunc TestFails(t *testing.T) { t.Fatal(\"boom\") }\n")
	report, err = NewRunner(runner).Verify(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if report.AllPassed() {
		t.Fatal("a failing test should fail the report")
	}
}
