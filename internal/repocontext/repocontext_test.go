package repocontext

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/execution"
)

func TestKeywords(t *testing.T) {
	tests := []struct {
		name, task string
		want       []string
	}{
		{
			name: "drops stopwords and short tokens",
			task: "add a new endpoint to the api",
			want: []string{"endpoint", "api"},
		},
		{
			name: "keeps identifiers with underscores and hyphens",
			task: "fix rate_limit and job-status handling",
			want: []string{"rate_limit", "job-status", "handling"},
		},
		{
			name: "splits on punctuation",
			task: "the RateLimiter.Middleware() drops requests",
			want: []string{"ratelimiter", "middleware", "drops", "requests"},
		},
		{
			name: "dedupes",
			task: "cache the cache in the cache layer",
			want: []string{"cache", "layer"},
		},
		{
			name: "empty task yields nothing",
			task: "   ",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Keywords(tt.task)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("Keywords(%q) = %v, want %v", tt.task, got, tt.want)
			}
		})
	}
}

// A task made entirely of filler must not produce a keyword that matches every
// file — that is the same as no signal at all, but far more expensive.
func TestKeywords_AllStopwordsYieldsNothing(t *testing.T) {
	if got := Keywords("please can you make the change and update it"); len(got) != 0 {
		t.Errorf("got %v, want no keywords from an all-filler task", got)
	}
}

func TestKeywords_BoundedCount(t *testing.T) {
	task := "alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo"
	if got := Keywords(task); len(got) > DefaultMaxKeywords {
		t.Errorf("got %d keywords, want at most %d", len(got), DefaultMaxKeywords)
	}
}

// --- integration against a real repository ---

func newRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main", ".")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	run("add", ".")
	run("commit", "-m", "initial")
	return dir
}

func newSelector() *Selector {
	return NewSelector(execution.NewSafeRunner(
		execution.NewExecRunner(), execution.NewAllowlist("git"), time.Minute))
}

func TestSelect_RanksFilesMatchingTheTask(t *testing.T) {
	repo := newRepo(t, map[string]string{
		"go.mod":                 "module probe\n\ngo 1.25\n",
		"ratelimit.go":           "package main\n\nfunc rateLimit() {}\n",
		"internal/api/jobs.go":   "package api\n\n// jobs handler\n",
		"docs/unrelated.md":      "nothing to do with anything\n",
		"internal/api/auth.go":   "package api\n\nfunc auth() {}\n",
		"vendor/dep/vendored.go": "package dep\n",
	})

	got, err := newSelector().Select(context.Background(), repo, "fix the ratelimit middleware")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(got.Files) == 0 {
		t.Fatal("no files selected")
	}
	// The filename match is the strongest signal, so it should lead.
	if got.Files[0] != "ratelimit.go" {
		t.Errorf("first file = %q, want ratelimit.go; full list %v", got.Files[0], got.Files)
	}
	// A file with no connection to the task should not crowd out one that has.
	for _, f := range got.Files {
		if f == "docs/unrelated.md" {
			t.Errorf("unrelated file was selected: %v", got.Files)
		}
	}
}

// Manifests orient a reader in an unfamiliar repository, so they are always
// worth the tokens even with no keyword match.
func TestSelect_AlwaysIncludesManifests(t *testing.T) {
	repo := newRepo(t, map[string]string{
		"go.mod":  "module probe\n\ngo 1.25\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})

	got, err := newSelector().Select(context.Background(), repo, "something completely unrelated zzz")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	var found bool
	for _, f := range got.Files {
		if f == "go.mod" {
			found = true
		}
	}
	if !found {
		t.Errorf("go.mod not selected: %v", got.Files)
	}
}

// Using git rather than walking the filesystem means .gitignore is honoured
// for free — no node_modules, no build output.
func TestSelect_SkipsUntrackedAndIgnoredFiles(t *testing.T) {
	repo := newRepo(t, map[string]string{
		".gitignore": "ignored.go\n",
		"main.go":    "package main\n",
	})
	// Written after the commit, so tracked by nothing.
	for _, name := range []string{"ignored.go", "untracked.go"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("package main // secret\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := newSelector().Select(context.Background(), repo, "main secret")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	for _, f := range got.Files {
		if f == "ignored.go" || f == "untracked.go" {
			t.Errorf("selected a file git does not track: %v", got.Files)
		}
	}
}

func TestSelect_RespectsMaxFiles(t *testing.T) {
	files := map[string]string{"go.mod": "module probe\n"}
	for i := 0; i < 50; i++ {
		files[filepath.Join("pkg", "handler"+string(rune('a'+i%26))+string(rune('a'+i/26))+".go")] =
			"package pkg\n// handler\n"
	}
	repo := newRepo(t, files)

	s := newSelector()
	s.MaxFiles = 5
	got, err := s.Select(context.Background(), repo, "handler")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(got.Files) != 5 {
		t.Errorf("got %d files, want the 5 cap", len(got.Files))
	}
	// The caller needs to know the selection was a sample, not the whole repo.
	if got.TotalTracked <= 5 {
		t.Errorf("TotalTracked = %d, want the real repository size", got.TotalTracked)
	}
}

// The same repository and task must always produce the same context, or a
// re-run of an identical job would send the model something different.
func TestSelect_IsDeterministic(t *testing.T) {
	repo := newRepo(t, map[string]string{
		"go.mod": "module probe\n", "a.go": "package main // handler\n",
		"b.go": "package main // handler\n", "c.go": "package main // handler\n",
	})
	s := newSelector()

	first, err := s.Select(context.Background(), repo, "handler")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		again, err := s.Select(context.Background(), repo, "handler")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(again.Files, ",") != strings.Join(first.Files, ",") {
			t.Fatalf("run %d differed:\n%v\n%v", i, first.Files, again.Files)
		}
	}
}

// Thin context should produce a worse plan, not no plan.
func TestSelect_EmptyRepoIsNotAnError(t *testing.T) {
	repo := newRepo(t, map[string]string{".keep": ""})
	got, err := newSelector().Select(context.Background(), repo, "do something")
	if err != nil {
		t.Fatalf("an empty repo should not fail selection: %v", err)
	}
	_ = got
}

func TestSelect_NonRepoIsAnError(t *testing.T) {
	if _, err := newSelector().Select(context.Background(), t.TempDir(), "task"); err == nil {
		t.Fatal("expected an error for a directory that is not a git repository")
	}
}

// Generic programming vocabulary matches every file in a code repository, so
// it is worth what "the" is worth — and worse, because the keyword budget is
// finite and noise crowds out signal. A live run lost "main" this way: the
// task "add a Greet function that takes a name string and returns a greeting,
// and call it from main" spent every slot on function/takes/string/returns/call
// and never searched for the one word that located the file to edit.
func TestKeywords_DropsGenericCodeWords(t *testing.T) {
	got := Keywords("add a Greet function that takes a name string and returns a greeting, and call it from main")

	joined := strings.Join(got, ",")
	for _, noise := range []string{"function", "takes", "string", "returns", "call"} {
		if strings.Contains(joined, noise) {
			t.Errorf("generic word %q survived: %v", noise, got)
		}
	}
	// The two words that actually locate the change must be present.
	for _, want := range []string{"greet", "main"} {
		if !strings.Contains(joined, want) {
			t.Errorf("keyword %q is missing, which is how the wrong file gets edited: %v", want, got)
		}
	}
}

// Ranked files say what is worth reading; the tree says what exists. Without
// it a model cannot tell ./main.go from ./cmd/tutorial/main.go, and a confident
// guess lands the change somewhere that compiles but is wrong.
func TestSelect_IncludesTheRepositoryTree(t *testing.T) {
	repo := newRepo(t, map[string]string{
		"go.mod":               "module probe\n\ngo 1.25\n",
		"cmd/tutorial/main.go": "package main\n\nfunc main() {}\n",
		"internal/thing.go":    "package internal\n",
	})

	got, err := newSelector().Select(context.Background(), repo, "something unrelated zzz")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(got.Tree) != 3 {
		t.Fatalf("tree has %d paths, want every tracked file: %v", len(got.Tree), got.Tree)
	}
	var found bool
	for _, p := range got.Tree {
		if p == "cmd/tutorial/main.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("the tree does not show where main lives: %v", got.Tree)
	}
}

// A large repository must not spend the whole context window on paths.
func TestSelect_TreeIsCapped(t *testing.T) {
	files := map[string]string{"go.mod": "module probe\n"}
	for i := 0; i < DefaultMaxTreePaths+50; i++ {
		files[filepath.Join("pkg", "f"+strconv.Itoa(i)+".go")] = "package pkg\n"
	}
	repo := newRepo(t, files)

	got, err := newSelector().Select(context.Background(), repo, "anything")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(got.Tree) > DefaultMaxTreePaths {
		t.Errorf("tree has %d paths, cap is %d", len(got.Tree), DefaultMaxTreePaths)
	}
	// TotalTracked still tells the truth about the repository's real size.
	if got.TotalTracked <= DefaultMaxTreePaths {
		t.Errorf("TotalTracked = %d, want the real count", got.TotalTracked)
	}
}
