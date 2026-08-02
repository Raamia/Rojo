package main

import (
	"context"
	"os/exec"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Raamia/Rojo/internal/benchmark"
)

func loadFixtures(t *testing.T) []benchmark.Case {
	t.Helper()
	cases, err := benchmark.LoadCases(fixtures, "testdata/cases")
	if err != nil {
		t.Fatalf("load embedded cases: %v", err)
	}
	return cases
}

func TestFixturesLoad(t *testing.T) {
	cases := loadFixtures(t)
	if len(cases) == 0 {
		t.Fatal("no cases embedded")
	}
	for _, c := range cases {
		if c.Name == "" || c.Task == "" {
			t.Errorf("case %+v is missing a name or task", c)
		}
	}
	t.Logf("%d cases embedded", len(cases))
}

func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed, skipping fixture integrity checks", tool)
		}
	}
}

// goTestPasses runs the repository's tests in dir.
func goTestPasses(t *testing.T, dir string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	res, err := benchmark.GitAndGoRunner(2*time.Minute).Run(ctx, dir, "go", "test", "./...")
	if err != nil {
		t.Fatalf("run go test in %s: %v", dir, err)
	}
	return res.ExitCode == 0
}

// TestFixtureIntegrity is the check that keeps this benchmark meaningful.
//
// Two properties per case, and a case that loses either one silently degrades
// every number the suite reports:
//
//   - the starting repository is in the state the case claims (green, or red
//     for a case that exists to be fixed), so a run measures the task and not a
//     broken fixture;
//   - the hidden assertion FAILS against that starting repository. An assertion
//     that already passes proves nothing — it would score every run as a
//     success including one where the model changed nothing at all.
//
// The second is the one worth having a test for. It is invisible by
// inspection, it breaks silently, and when it breaks the benchmark reports
// 100% and looks like good news.
func TestFixtureIntegrity(t *testing.T) {
	requireTools(t, "go", "git")
	if testing.Short() {
		t.Skip("compiles and runs every fixture; skipped under -short")
	}

	for _, c := range loadFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()

			// (a) the repository alone, as the job will first see it
			repoDir := t.TempDir()
			if err := c.MaterializeRepo(repoDir); err != nil {
				t.Fatalf("materialize repo: %v", err)
			}
			green := goTestPasses(t, repoDir)
			if c.StartsRed && green {
				t.Errorf("case is marked starts_red but its tests pass; " +
					"it would not exercise the revision loop")
			}
			if !c.StartsRed && !green {
				t.Errorf("the starting repository does not pass its own tests; " +
					"a run would measure a broken fixture rather than the task")
			}

			// (b) the repository plus the hidden assertion, unchanged
			assertDir := t.TempDir()
			if err := c.MaterializeRepo(assertDir); err != nil {
				t.Fatalf("materialize repo: %v", err)
			}
			if err := c.OverlayAssertions(assertDir); err != nil {
				t.Fatalf("overlay assertions: %v", err)
			}
			if goTestPasses(t, assertDir) {
				t.Fatal("the hidden assertion PASSES against the unmodified repository. " +
					"It does not discriminate, so this case would score any patch — " +
					"including an empty one — as a success.")
			}
		})
	}
}

// Case names reach the -only flag and the report's rows, so they need to be
// distinct and free of the separator that flag splits on.
func TestFixtureNamesAreUsable(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range loadFixtures(t) {
		if seen[c.Name] {
			t.Errorf("duplicate case name %q", c.Name)
		}
		seen[c.Name] = true
		for _, bad := range []string{",", " ", "\t", "\n"} {
			if contains(c.Name, bad) {
				t.Errorf("case name %q contains %q, which breaks -only parsing", c.Name, bad)
			}
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("one\ntwo"); got != "one" {
		t.Errorf("firstLine = %q, want %q", got, "one")
	}
	long := make([]byte, 200)
	for i := range long {
		long[i] = 'x'
	}
	// Counted in runes, not bytes: the ellipsis is one character but three
	// bytes, and this bounds a display width.
	if got := utf8.RuneCountInString(firstLine(string(long))); got > 70 {
		t.Errorf("firstLine returned %d characters, want it clipped to 70", got)
	}
}
