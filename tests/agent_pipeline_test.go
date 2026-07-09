package tests

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/agents/implementor"
	"github.com/Raamia/Rojo/internal/agents/model"
	"github.com/Raamia/Rojo/internal/agents/planner"
	"github.com/Raamia/Rojo/internal/agents/reviewer"
	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/execution"
	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/orchestration"
	"github.com/Raamia/Rojo/internal/repocontext"
	"github.com/Raamia/Rojo/internal/storage/filestore"
	"github.com/Raamia/Rojo/internal/verification"
	"github.com/Raamia/Rojo/internal/workspace"
)

// These exercise the whole pipeline with every real component — the real
// Anthropic SDK client, real git worktrees, the real Go toolchain, the real
// filesystem store — and fake only the model's HTTP endpoint. That is as close
// to production as it gets without an API key: everything except the model's
// judgement is the code that would actually run.

// fakeAnthropic serves canned Messages API replies in order, so a test can
// script what the planner, implementor and reviewer each "decide".
type fakeAnthropic struct {
	*httptest.Server
	mu      sync.Mutex
	replies []string
	prompts []string
}

func newFakeAnthropic(t *testing.T, replies ...string) *fakeAnthropic {
	t.Helper()
	f := &fakeAnthropic{replies: replies}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		f.mu.Lock()
		f.prompts = append(f.prompts, string(body))
		var reply string
		if n := len(f.prompts) - 1; n < len(f.replies) {
			reply = f.replies[n]
		} else {
			reply = f.replies[len(f.replies)-1] // the last one repeats
		}
		f.mu.Unlock()

		out, _ := json.Marshal(map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant",
			"model":       "claude-opus-4-8",
			"content":     []map[string]any{{"type": "text", "text": reply}},
			"stop_reason": "end_turn",
			"usage":       map[string]int{"input_tokens": 10, "output_tokens": 20},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAnthropic) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

func (f *fakeAnthropic) promptAt(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.prompts) {
		return ""
	}
	return f.prompts[i]
}

const (
	planReply = `{"summary":"add a Greet function",
		"steps":[{"id":"s1","description":"create greet.go with func Greet"}],
		"files_to_change":["greet.go"]}`

	implReply = `{"operations":[{"kind":"write","path":"greet.go",
		"content":"package main\n\nfunc Greet() string { return \"hi\" }\n"}]}`

	approveReply = `{"decision":"approve","notes":"does what was asked"}`
)

// pipelineRepo is a repository whose tests fail until Greet exists, so the
// verification gate is genuinely load-bearing: nothing passes unless the
// implementor's change was really applied.
func pipelineRepo(t *testing.T) string {
	t.Helper()
	hasGit(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module pipelineprobe\n\ngo 1.25\n")
	write("main.go", "package main\n\nfunc main() {}\n")
	write("main_test.go",
		"package main\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n"+
			"\tif Greet() != \"hi\" {\n\t\tt.Fatal(\"bad greeting\")\n\t}\n}\n")

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

// buildPipeline wires the processor exactly as cmd/api/main.go does, but with
// the model client pointed at the given local server.
func buildPipeline(t *testing.T, modelURL string) (*orchestration.Processor, *filestore.Store, *events.PersistingBus) {
	t.Helper()

	store, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bus := &events.PersistingBus{Inner: events.NewInProcessBus(), Store: store}

	gitRunner := execution.NewSafeRunner(
		execution.NewExecRunner(), execution.NewAllowlist("git"), time.Minute)
	verifyRunner := execution.NewSafeRunner(
		execution.NewExecRunner(), execution.NewAllowlist("go", "gofmt"), 3*time.Minute)

	mc := model.NewAnthropicClient(model.AnthropicOptions{
		APIKey: "test-key", BaseURL: modelURL, Timeout: time.Minute,
	})

	p := orchestration.NewProcessor(store, orchestration.NewCanceller(), bus)
	p.Workspaces = workspace.NewGitWorkspaceManager(gitRunner, t.TempDir())
	p.Verifier = verification.NewRunner(verifyRunner)
	p.Artifacts = store
	p.Planner = planner.NewPlanner(mc)
	p.Context = repocontext.NewSelector(gitRunner)
	p.Implementor = implementor.NewAgent(mc)
	p.Reviewer = reviewer.New(mc)
	p.JobTimeout = 5 * time.Minute
	return p, store, bus
}

func submit(t *testing.T, store *filestore.Store, id, task, repoPath string) {
	t.Helper()
	now := time.Now().UTC()
	if err := store.Create(context.Background(), &jobs.Job{
		ID: id, Task: task, RepoPath: repoPath,
		Status: jobs.StatusQueued, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

// The whole thing: plan, select context, implement, verify with the real Go
// toolchain, review, and hand back a patch — with the source repository
// untouched at the end.
func TestPipeline_PlanImplementVerifyReviewProducesAPatch(t *testing.T) {
	repo := pipelineRepo(t)
	fake := newFakeAnthropic(t, planReply, implReply, approveReply)
	p, store, _ := buildPipeline(t, fake.URL)
	submit(t, store, "full", "add a Greet function returning hi", repo)

	if err := p.Process(context.Background(), "full"); err != nil {
		t.Fatalf("process: %v", err)
	}

	got, err := store.Get(context.Background(), "full")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != jobs.StatusCompleted {
		t.Fatalf("status = %q, want completed", got.Status)
	}
	if fake.calls() != 3 {
		t.Errorf("model called %d times, want 3 (plan, implement, review)", fake.calls())
	}

	// The patch is the deliverable, and it has to be a real one.
	patch, err := store.ReadArtifact("full", orchestration.ArtifactDiff)
	if err != nil {
		t.Fatalf("no patch stored: %v", err)
	}
	for _, want := range []string{"greet.go", "new file mode", "func Greet()"} {
		if !strings.Contains(string(patch), want) {
			t.Errorf("patch missing %q:\n%s", want, patch)
		}
	}

	// And applying it to a clean copy of the repository has to work — that is
	// the difference between a patch and a description of one.
	clone := pipelineRepo(t)
	cmd := exec.Command("git", "apply", "-")
	cmd.Dir = clone
	cmd.Stdin = strings.NewReader(string(patch))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git apply rejected the patch: %v\n%s\n%s", err, out, patch)
	}
	check := exec.Command("go", "test", "./...")
	check.Dir = clone
	if out, err := check.CombinedOutput(); err != nil {
		t.Fatalf("the applied patch does not pass the tests: %v\n%s", err, out)
	}

	// The source repository is exactly as it was.
	if _, err := os.Stat(filepath.Join(repo, "greet.go")); !os.IsNotExist(err) {
		t.Error("the change leaked into the source repository")
	}
	status := exec.Command("git", "status", "--porcelain")
	status.Dir = repo
	if out, _ := status.Output(); len(strings.TrimSpace(string(out))) != 0 {
		t.Errorf("source working tree is dirty:\n%s", out)
	}
	branches := exec.Command("git", "branch", "--list", "rojo/*")
	branches.Dir = repo
	if out, _ := branches.Output(); len(strings.TrimSpace(string(out))) != 0 {
		t.Errorf("job branch was left behind:\n%s", out)
	}
}

// The revision loop against the real toolchain: the first implementation does
// not compile, `go test` says so, and the failing output goes back to the model
// as feedback. This is the behaviour that makes Rojo more than a test runner.
func TestPipeline_FailingBuildTriggersARealRevision(t *testing.T) {
	repo := pipelineRepo(t)

	broken := `{"operations":[{"kind":"write","path":"greet.go",` +
		`"content":"package main\n\nfunc Greet() string { return 42 }\n"}]}`

	fake := newFakeAnthropic(t, planReply, broken, implReply, approveReply)
	p, store, _ := buildPipeline(t, fake.URL)
	submit(t, store, "revise", "add a Greet function returning hi", repo)

	if err := p.Process(context.Background(), "revise"); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := store.Get(context.Background(), "revise")
	if got.Status != jobs.StatusCompleted {
		t.Fatalf("status = %q, want completed after the revision fixed the build", got.Status)
	}

	// Call 2 is the second implementor request. It must carry the compiler's
	// complaint — without it the model is guessing at what broke.
	second := fake.promptAt(2)
	if !strings.Contains(second, "failed verification") {
		t.Errorf("the revision was not told the build failed:\n%s", truncateForLog(second))
	}
	if !strings.Contains(second, "cannot use 42") && !strings.Contains(second, "untyped int") {
		t.Errorf("the revision did not receive the compiler output:\n%s", truncateForLog(second))
	}

	patch, err := store.ReadArtifact("revise", orchestration.ArtifactDiff)
	if err != nil {
		t.Fatalf("no patch stored: %v", err)
	}
	if strings.Contains(string(patch), "return 42") {
		t.Errorf("the stored patch is the broken first attempt:\n%s", patch)
	}
}

// Deterministic checks outrank the model. A reviewer that approves a change
// which never compiled must not be able to complete the job.
func TestPipeline_ReviewerCannotApproveABrokenBuild(t *testing.T) {
	repo := pipelineRepo(t)

	broken := `{"operations":[{"kind":"write","path":"greet.go",` +
		`"content":"package main\n\nfunc Greet() string { return 42 }\n"}]}`

	// Every reply after the plan is "broken", so both passes fail the gate, and
	// the reviewer would approve if it were ever asked.
	fake := newFakeAnthropic(t, planReply, broken, broken, approveReply)
	p, store, _ := buildPipeline(t, fake.URL)
	submit(t, store, "nope", "add a Greet function", repo)

	if err := p.Process(context.Background(), "nope"); err == nil {
		t.Fatal("a job whose build never succeeded must not complete")
	}
	got, _ := store.Get(context.Background(), "nope")
	if got.Status != jobs.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}

	// The patch that failed is still there to look at.
	if _, err := store.ReadArtifact("nope", orchestration.ArtifactDiff); err != nil {
		t.Errorf("a failed job kept no patch: %v", err)
	}
}

// A model that returns prose instead of JSON must fail the job cleanly rather
// than crashing a worker or writing something arbitrary to disk.
func TestPipeline_MalformedModelOutputFailsSafely(t *testing.T) {
	repo := pipelineRepo(t)
	fake := newFakeAnthropic(t, "I have decided to write greet.go for you. Here is why...")
	p, store, _ := buildPipeline(t, fake.URL)
	submit(t, store, "garbage", "add a Greet function", repo)

	if err := p.Process(context.Background(), "garbage"); err == nil {
		t.Fatal("expected the job to fail on unparseable model output")
	}
	got, _ := store.Get(context.Background(), "garbage")
	if got.Status != jobs.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if _, err := os.Stat(filepath.Join(repo, "greet.go")); !os.IsNotExist(err) {
		t.Error("something was written despite the model returning nonsense")
	}
}

// The event log is what a client watching a job sees. It has to tell the whole
// story, and it has to survive in the store rather than only in memory.
func TestPipeline_EventLogTellsTheStory(t *testing.T) {
	repo := pipelineRepo(t)
	fake := newFakeAnthropic(t, planReply, implReply, approveReply)
	p, store, _ := buildPipeline(t, fake.URL)
	submit(t, store, "events", "add a Greet function", repo)

	if err := p.Process(context.Background(), "events"); err != nil {
		t.Fatalf("process: %v", err)
	}

	history, err := store.History(context.Background(), "events")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, e := range history {
		seen[e.Type]++
		if e.CreatedAt.IsZero() {
			t.Errorf("event %q has no timestamp", e.Type)
		}
	}
	for _, want := range []string{
		events.TypeJobStarted,
		events.TypePlanCreated,
		events.TypeWorkspaceCreated,
		events.TypeImplementationCompleted,
		events.TypeVerificationCompleted,
		events.TypeReviewCompleted,
		events.TypeDiffCaptured,
		events.TypeJobCompleted,
	} {
		if seen[want] == 0 {
			t.Errorf("no %s event in the job's history", want)
		}
	}
}

func truncateForLog(s string) string {
	if len(s) > 1200 {
		return s[:1200] + "..."
	}
	return s
}

// pipelineRepoNoTest is a repository that already compiles and passes, so the
// gate is green regardless of what the implementor writes. Used where the point
// is concurrency rather than the gate.
func pipelineRepoNoTest(t *testing.T) string {
	t.Helper()
	hasGit(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	dir := t.TempDir()
	for name, content := range map[string]string{
		"go.mod":  "module concurrentprobe\n\ngo 1.25\n",
		"main.go": "package main\n\nfunc main() {}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-b", "main", "."},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"add", "."},
		{"commit", "-m", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}
