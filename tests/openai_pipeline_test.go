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

// The point of the model.Client interface is that the pipeline does not care
// which provider answers. These prove it: the same jobs that run against the
// Anthropic client in agent_pipeline_test.go run here against the OpenAI one,
// through the real OpenAI SDK, with only the model's HTTP endpoint stubbed.
// Everything else — git worktrees, the Go toolchain, the filesystem store — is
// the code that actually ships.

// fakeOpenAI serves canned chat completions in order.
type fakeOpenAI struct {
	*httptest.Server
	mu      sync.Mutex
	replies []string
	prompts []string
}

func newFakeOpenAI(t *testing.T, replies ...string) *fakeOpenAI {
	t.Helper()
	f := &fakeOpenAI{replies: replies}
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
			"id": "chatcmpl-1", "object": "chat.completion", "created": 1,
			"model": "gpt-5.2",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": reply},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeOpenAI) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

func (f *fakeOpenAI) promptAt(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.prompts) {
		return ""
	}
	return f.prompts[i]
}

// buildOpenAIPipeline wires the processor exactly as cmd/api/main.go does when
// the resolved provider is OpenAI.
func buildOpenAIPipeline(t *testing.T, modelURL string) (*orchestration.Processor, *filestore.Store) {
	t.Helper()

	store, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	bus := &events.PersistingBus{Inner: events.NewInProcessBus(), Store: store}

	gitRunner := execution.NewSafeRunner(
		execution.NewExecRunner(), execution.NewAllowlist("git"), time.Minute)
	verifyRunner := execution.NewSafeRunner(
		execution.NewExecRunner(), execution.NewAllowlist(verification.AutoCommands()...), 3*time.Minute)

	mc := model.NewOpenAIClient(model.OpenAIOptions{
		APIKey: "test-key", BaseURL: modelURL, Timeout: time.Minute,
	})

	p := orchestration.NewProcessor(store, orchestration.NewCanceller(), bus)
	p.Workspaces = workspace.NewGitWorkspaceManager(gitRunner, t.TempDir())
	p.Verifier = verification.NewAutoRunner(verifyRunner)
	p.Artifacts = store
	p.Planner = planner.NewPlanner(mc)
	p.Context = repocontext.NewSelector(gitRunner)
	p.Implementor = implementor.NewAgent(mc)
	p.Reviewer = reviewer.New(mc)
	p.JobTimeout = 5 * time.Minute
	return p, store
}

// The whole pipeline through OpenAI: plan, implement, verify with the real Go
// toolchain, review, and hand back an applyable patch.
func TestOpenAIPipeline_ProducesAnApplyablePatch(t *testing.T) {
	repo := pipelineRepo(t)
	fake := newFakeOpenAI(t, planReply, implReply, approveReply)
	p, store := buildOpenAIPipeline(t, fake.URL)
	submit(t, store, "oai", "add a Greet function returning hi", repo)

	if err := p.Process(context.Background(), "oai"); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, err := store.Get(context.Background(), "oai")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != jobs.StatusCompleted {
		t.Fatalf("status = %q, want completed", got.Status)
	}
	if fake.calls() != 3 {
		t.Errorf("model called %d times, want 3 (plan, implement, review)", fake.calls())
	}

	patch, err := store.ReadArtifact("oai", orchestration.ArtifactDiff)
	if err != nil {
		t.Fatalf("no patch stored: %v", err)
	}
	for _, want := range []string{"greet.go", "new file mode", "func Greet()"} {
		if !strings.Contains(string(patch), want) {
			t.Errorf("patch missing %q:\n%s", want, patch)
		}
	}

	// Applying it to a clean copy is the difference between a patch and a
	// description of one.
	clone := pipelineRepo(t)
	cmd := exec.Command("git", "apply", "-")
	cmd.Dir = clone
	cmd.Stdin = strings.NewReader(string(patch))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git apply rejected the patch: %v\n%s", err, out)
	}
	check := exec.Command("go", "test", "./...")
	check.Dir = clone
	if out, err := check.CombinedOutput(); err != nil {
		t.Fatalf("the applied patch does not pass the tests: %v\n%s", err, out)
	}

	// The source repository is untouched.
	if _, err := os.Stat(filepath.Join(repo, "greet.go")); !os.IsNotExist(err) {
		t.Error("the change leaked into the source repository")
	}
}

// The revision loop works the same through OpenAI: a broken first attempt, the
// real compiler's complaint as feedback, a fixed second attempt.
func TestOpenAIPipeline_RevisionLoop(t *testing.T) {
	repo := pipelineRepo(t)
	broken := `{"operations":[{"kind":"write","path":"greet.go",` +
		`"content":"package main\n\nfunc Greet() string { return 42 }\n"}]}`

	fake := newFakeOpenAI(t, planReply, broken, implReply, approveReply)
	p, store := buildOpenAIPipeline(t, fake.URL)
	submit(t, store, "oairev", "add a Greet function returning hi", repo)

	if err := p.Process(context.Background(), "oairev"); err != nil {
		t.Fatalf("process: %v", err)
	}
	if got, _ := store.Get(context.Background(), "oairev"); got.Status != jobs.StatusCompleted {
		t.Fatalf("status = %q, want completed after the revision", got.Status)
	}

	second := fake.promptAt(2)
	if !strings.Contains(second, "failed verification") {
		t.Errorf("the revision was not told the build failed")
	}
	if !strings.Contains(second, "cannot use 42") && !strings.Contains(second, "untyped int") {
		t.Errorf("the revision did not receive the compiler output")
	}
}

// Deterministic checks outrank the model here too: a reviewer that would
// approve is never consulted while the build is broken.
func TestOpenAIPipeline_ReviewerCannotApproveABrokenBuild(t *testing.T) {
	repo := pipelineRepo(t)
	broken := `{"operations":[{"kind":"write","path":"greet.go",` +
		`"content":"package main\n\nfunc Greet() string { return 42 }\n"}]}`

	fake := newFakeOpenAI(t, planReply, broken, broken, approveReply)
	p, store := buildOpenAIPipeline(t, fake.URL)
	submit(t, store, "oaibad", "add a Greet function", repo)

	if err := p.Process(context.Background(), "oaibad"); err == nil {
		t.Fatal("a job whose build never succeeded must not complete")
	}
	if got, _ := store.Get(context.Background(), "oaibad"); got.Status != jobs.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	// The failed patch is still there to read.
	if _, err := store.ReadArtifact("oaibad", orchestration.ArtifactDiff); err != nil {
		t.Errorf("a failed job kept no patch: %v", err)
	}
}

// Prose instead of JSON must fail the job cleanly rather than writing anything.
func TestOpenAIPipeline_MalformedOutputFailsSafely(t *testing.T) {
	repo := pipelineRepo(t)
	fake := newFakeOpenAI(t, "I have decided to write greet.go for you. Here is why...")
	p, store := buildOpenAIPipeline(t, fake.URL)
	submit(t, store, "oaijunk", "add a Greet function", repo)

	if err := p.Process(context.Background(), "oaijunk"); err == nil {
		t.Fatal("expected the job to fail on unparseable model output")
	}
	if got, _ := store.Get(context.Background(), "oaijunk"); got.Status != jobs.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if _, err := os.Stat(filepath.Join(repo, "greet.go")); !os.IsNotExist(err) {
		t.Error("something was written despite the model returning nonsense")
	}
}

// The agents must be genuinely provider-agnostic: the same prompts, parsers and
// pipeline produce the same outcome whichever backend answered. If a provider
// ever needed its own prompt or parser, that coupling would show up here.
func TestPipeline_SameOutcomeAcrossProviders(t *testing.T) {
	patchOf := func(t *testing.T, p *orchestration.Processor, store *filestore.Store, id, repo string) string {
		t.Helper()
		if err := p.Process(context.Background(), id); err != nil {
			t.Fatalf("process %s: %v", id, err)
		}
		got, _ := store.Get(context.Background(), id)
		if got.Status != jobs.StatusCompleted {
			t.Fatalf("%s status = %q, want completed", id, got.Status)
		}
		b, err := store.ReadArtifact(id, orchestration.ArtifactDiff)
		if err != nil {
			t.Fatalf("%s has no patch: %v", id, err)
		}
		return string(b)
	}

	anthropicRepo := pipelineRepo(t)
	ap, astore, _ := buildPipeline(t, newFakeAnthropic(t, planReply, implReply, approveReply).URL)
	submit(t, astore, "prov-a", "add a Greet function returning hi", anthropicRepo)
	anthropicPatch := patchOf(t, ap, astore, "prov-a", anthropicRepo)

	openaiRepo := pipelineRepo(t)
	op, ostore := buildOpenAIPipeline(t, newFakeOpenAI(t, planReply, implReply, approveReply).URL)
	submit(t, ostore, "prov-o", "add a Greet function returning hi", openaiRepo)
	openaiPatch := patchOf(t, op, ostore, "prov-o", openaiRepo)

	// Same instructions in, same change out. Index-line hashes are identical
	// too since the content is, so the patches should match outright.
	if anthropicPatch != openaiPatch {
		t.Errorf("providers produced different patches:\n--- anthropic ---\n%s\n--- openai ---\n%s",
			anthropicPatch, openaiPatch)
	}
}
