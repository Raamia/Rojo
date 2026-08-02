package benchmark

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed, skipping", tool)
		}
	}
}

// verifyCase is a self-contained case: Greet exists, Farewell does not, and the
// hidden assertion demands Farewell.
func verifyCase() Case {
	return Case{
		Name: "verify-fixture",
		Task: "add Farewell",
		repoFiles: map[string][]byte{
			"go.mod.txt": []byte("module benchcase\n\ngo 1.25\n"),
			"greet.go":   []byte("package benchcase\n\nfunc Greet(n string) string { return \"hello \" + n }\n"),
		},
		assertFiles: map[string][]byte{
			"zz_assert_test.go": []byte("package benchcase\n\nimport \"testing\"\n\n" +
				"func TestFarewell(t *testing.T) {\n" +
				"\tif Farewell(\"x\") != \"goodbye x\" {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n"),
		},
	}
}

// A correct patch must be recognised as having done the task.
func TestCheckPatch_CorrectPatchPasses(t *testing.T) {
	requireTools(t, "git", "go")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	patch := `diff --git a/greet.go b/greet.go
--- a/greet.go
+++ b/greet.go
@@ -1,3 +1,5 @@
 package benchcase

 func Greet(n string) string { return "hello " + n }
+
+func Farewell(n string) string { return "goodbye " + n }
`
	got, err := CheckPatch(ctx, GitAndGoRunner(2*time.Minute), verifyCase(), patch)
	if err != nil {
		t.Fatalf("CheckPatch: %v", err)
	}
	if !got.Passed {
		t.Fatalf("a correct patch was not recognised: stage=%s\n%s", got.Stage, got.Output)
	}
}

// An empty patch is the "the model changed nothing" case. The pipeline may well
// report that as a success — the repository still passes its own tests — so the
// hidden assertion is the only thing that catches it.
func TestCheckPatch_EmptyPatchFails(t *testing.T) {
	requireTools(t, "git", "go")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	got, err := CheckPatch(ctx, GitAndGoRunner(2*time.Minute), verifyCase(), "")
	if err != nil {
		t.Fatalf("CheckPatch: %v", err)
	}
	if got.Passed {
		t.Fatal("an empty patch was reported as having done the task")
	}
	if got.Stage != "assert" {
		t.Errorf("Stage = %q, want assert", got.Stage)
	}
}

func TestCheckPatch_UnapplyablePatchIsAttributedToApply(t *testing.T) {
	requireTools(t, "git", "go")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	patch := `diff --git a/nonexistent.go b/nonexistent.go
--- a/nonexistent.go
+++ b/nonexistent.go
@@ -1,3 +1,4 @@
 this context does not match anything
+added
`
	got, err := CheckPatch(ctx, GitAndGoRunner(2*time.Minute), verifyCase(), patch)
	if err != nil {
		t.Fatalf("CheckPatch: %v", err)
	}
	if got.Passed {
		t.Fatal("a patch that cannot be applied was reported as a pass")
	}
	if got.Stage != "apply" {
		t.Errorf("Stage = %q, want apply so the failure is attributable", got.Stage)
	}
}

// The load-bearing ordering property: assertions are overlaid AFTER the patch,
// so a patch that deletes or weakens the hidden test cannot make itself pass.
func TestCheckPatch_PatchCannotDisableTheHiddenAssertion(t *testing.T) {
	requireTools(t, "git", "go")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// This patch writes a neutered version of the assertion file and does not
	// implement Farewell at all. If overlay ordering were wrong, it would pass.
	patch := `diff --git a/zz_assert_test.go b/zz_assert_test.go
new file mode 100644
--- /dev/null
+++ b/zz_assert_test.go
@@ -0,0 +1,5 @@
+package benchcase
+
+import "testing"
+
+func TestFarewell(t *testing.T) {}
`
	got, err := CheckPatch(ctx, GitAndGoRunner(2*time.Minute), verifyCase(), patch)
	if err != nil {
		t.Fatalf("CheckPatch: %v", err)
	}
	if got.Passed {
		t.Fatal("a patch that overwrote the hidden assertion was able to pass; " +
			"the real assertion must be written back over it")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 100); got != "short" {
		t.Errorf("truncate did not leave a short string alone: %q", got)
	}
	got := truncate(strings.Repeat("x", 100), 10)
	if !strings.HasPrefix(got, strings.Repeat("x", 10)) {
		t.Error("truncate did not keep the head, where a compiler error names its cause")
	}
	if !strings.Contains(got, "truncated") {
		t.Error("truncate did not disclose that it clipped")
	}
}
