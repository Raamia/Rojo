package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Raamia/Rojo/internal/events"
)

// fakeAPI is a scripted Rojo server. Each poll of the job advances it through
// its statuses, so the watch loop sees a job progress and finish.
type fakeAPI struct {
	*httptest.Server
	mu       sync.Mutex
	statuses []string // consumed one per GET /jobs/{id}; last repeats
	polls    int
	events   []events.Event
	patch    string
	sawAuth  []string
	created  map[string]string // task -> repo_path
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{statuses: []string{"completed"}, created: map[string]string{}}
	mux := http.NewServeMux()

	record := func(r *http.Request) {
		f.mu.Lock()
		f.sawAuth = append(f.sawAuth, r.Header.Get("Authorization"))
		f.mu.Unlock()
	}

	mux.HandleFunc("POST /api/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		var req struct{ Task, Repo string }
		var body struct {
			Task     string `json:"task"`
			RepoPath string `json:"repo_path"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		req.Task, req.Repo = body.Task, body.RepoPath
		f.mu.Lock()
		f.created[req.Task] = req.Repo
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ID": "job-1", "Task": req.Task, "RepoPath": req.Repo, "Status": "queued",
			"CreatedAt": time.Now(), "UpdatedAt": time.Now(),
		})
	})
	mux.HandleFunc("GET /api/v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		f.mu.Lock()
		i := f.polls
		if i >= len(f.statuses) {
			i = len(f.statuses) - 1
		}
		status := f.statuses[i]
		f.polls++
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ID": r.PathValue("id"), "Status": status})
	})
	mux.HandleFunc("GET /api/v1/jobs/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		f.mu.Lock()
		evs := append([]events.Event(nil), f.events...)
		f.mu.Unlock()
		// Honour the ?since= cursor the way the real server does, so the watch
		// loop's incremental fetch is actually exercised.
		if raw := r.URL.Query().Get("since"); raw != "" {
			if since, err := strconv.Atoi(raw); err == nil {
				if since > len(evs) {
					since = len(evs)
				}
				evs = evs[since:]
			}
		}
		_ = json.NewEncoder(w).Encode(evs)
	})
	mux.HandleFunc("GET /api/v1/jobs/{id}/diff", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if f.patch == "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no diff for job in status failed"}`))
			return
		}
		w.Header().Set("Content-Type", "text/x-diff")
		_, _ = w.Write([]byte(f.patch))
	})
	mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"cancel_requested"}`))
	})
	mux.HandleFunc("GET /api/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("X-Total-Count", "2")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"ID": "job-2", "Status": "completed", "Task": "newer", "CreatedAt": time.Now()},
			{"ID": "job-1", "Status": "failed", "Task": "older", "CreatedAt": time.Now()},
		})
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// gitRepo makes a real repository so resolveRepo's .git check passes.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main", "."},
		{"config", "user.email", "t@e.com"}, {"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := exec.Command("git", "add", ".")
	run.Dir = dir
	_ = run.Run()
	run = exec.Command("git", "commit", "-qm", "init")
	run.Dir = dir
	_ = run.Run()
	return dir
}

const testPatch = "diff --git a/greet.go b/greet.go\nnew file mode 100644\nindex 0000000..9524232\n--- /dev/null\n+++ b/greet.go\n@@ -0,0 +1 @@\n+package main\n"

func TestRun_HappyPath(t *testing.T) {
	api := newFakeAPI(t)
	api.statuses = []string{"implementing", "completed"}
	api.patch = testPatch
	api.events = []events.Event{
		{Type: events.TypeStepStarted, Payload: map[string]any{"status": "planning"}},
		{Type: events.TypeJobCompleted},
	}
	repo := gitRepo(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"-server", api.URL, "run", "-repo", repo, "add a greeting"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit %d, want 0\nstderr: %s", code, stderr.String())
	}
	// The contract that makes `rojo run ... > fix.patch` work: patch on
	// stdout, everything else on stderr.
	if stdout.String() != testPatch {
		t.Errorf("stdout = %q, want exactly the patch", stdout.String())
	}
	if !strings.Contains(stderr.String(), "planning") {
		t.Errorf("stderr does not show progress: %s", stderr.String())
	}
	api.mu.Lock()
	sent := api.created["add a greeting"]
	api.mu.Unlock()
	if sent != repo {
		t.Errorf("server got repo %q, want %q", sent, repo)
	}
}

// A failed job's patch is what the user reads to see what went wrong — it must
// still be delivered, with a failing exit code.
func TestRun_FailedJobStillPrintsThePatch(t *testing.T) {
	api := newFakeAPI(t)
	api.statuses = []string{"failed"}
	api.patch = testPatch
	repo := gitRepo(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"-server", api.URL, "run", "-repo", repo, "break something"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if stdout.String() != testPatch {
		t.Errorf("the failed job's patch was not delivered: %q", stdout.String())
	}
}

func TestRun_ApplyWritesTheChangeIntoTheRepo(t *testing.T) {
	api := newFakeAPI(t)
	api.patch = testPatch
	repo := gitRepo(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"-server", api.URL, "run", "-repo", repo, "-apply", "add greet"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(repo, "greet.go")); err != nil {
		t.Errorf("the patch was not applied: %v", err)
	}
	// Applied means not also dumped to stdout — the file is the delivery.
	if stdout.Len() != 0 {
		t.Errorf("stdout should be empty when applying, got %q", stdout.String())
	}
}

// -apply on a failed job must not touch the repository: the patch failed its
// checks, and silently applying it would hand the user broken code as if it
// were the deliverable.
func TestRun_ApplyRefusesAFailedJobsPatch(t *testing.T) {
	api := newFakeAPI(t)
	api.statuses = []string{"failed"}
	api.patch = testPatch
	repo := gitRepo(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"-server", api.URL, "run", "-repo", repo, "-apply", "x"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if _, err := os.Stat(filepath.Join(repo, "greet.go")); err == nil {
		t.Error("a failed job's patch was applied to the repository")
	}
	if stdout.String() != testPatch {
		t.Errorf("the patch should be printed instead: %q", stdout.String())
	}
}

func TestRun_RefusesANonRepository(t *testing.T) {
	api := newFakeAPI(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-server", api.URL, "run", "-repo", t.TempDir(), "task"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not the root of a git repository") {
		t.Errorf("stderr = %q, want a clear non-repo error", stderr.String())
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.created) != 0 {
		t.Error("a job was submitted for a directory that is not a repository")
	}
}

func TestRun_TokenIsSent(t *testing.T) {
	api := newFakeAPI(t)
	repo := gitRepo(t)
	var stdout, stderr bytes.Buffer
	run([]string{"-server", api.URL, "-token", "sekrit", "run", "-repo", repo, "x"}, &stdout, &stderr)

	api.mu.Lock()
	defer api.mu.Unlock()
	for _, h := range api.sawAuth {
		if h != "Bearer sekrit" {
			t.Fatalf("a request went out with Authorization %q", h)
		}
	}
	if len(api.sawAuth) == 0 {
		t.Fatal("no requests recorded")
	}
}

func TestRun_ServerDownIsAHelpfulError(t *testing.T) {
	repo := gitRepo(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-server", "http://127.0.0.1:1", "run", "-repo", repo, "x"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "cannot reach the rojo server") {
		t.Errorf("stderr = %q, want a pointer to starting the server", stderr.String())
	}
}

func TestList(t *testing.T) {
	api := newFakeAPI(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-server", api.URL, "list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"job-2", "completed", "newer", "job-1", "failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
}

func TestDiff_NoPatchYet(t *testing.T) {
	api := newFakeAPI(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-server", api.URL, "diff", "job-1"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no patch") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestCancel(t *testing.T) {
	api := newFakeAPI(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-server", api.URL, "cancel", "job-1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "cancel requested") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"frobnicate"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Usage") {
		t.Errorf("stderr should show usage:\n%s", stderr.String())
	}
}

func TestRenderEvent(t *testing.T) {
	at := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		event events.Event
		want  string // substring; "" means the event renders to nothing
	}{
		{events.Event{Type: events.TypeStepStarted, CreatedAt: at, Payload: map[string]any{"status": "implementing"}}, "→ implementing"},
		{events.Event{Type: events.TypePlanCreated, CreatedAt: at, Payload: map[string]any{"summary": "add Greet"}}, "plan: add Greet"},
		{events.Event{Type: events.TypeJobFailed, CreatedAt: at, Payload: map[string]any{"error": "boom"}}, "✗ failed: boom"},
		{events.Event{Type: events.TypeDiffCaptured, CreatedAt: at, Payload: map[string]any{"empty": true}}, "no changes"},
		{events.Event{Type: events.TypeStepCompleted, CreatedAt: at}, ""},
		{events.Event{Type: "job.created", CreatedAt: at}, ""},
		{
			events.Event{Type: events.TypeVerificationCompleted, CreatedAt: at, Payload: map[string]any{
				"summary": "all 1 checks passed",
				"results": []any{map[string]any{"check": "go test", "note": "no test files found: the gate proves compilation only"}},
			}},
			"no test files found",
		},
	}
	for _, tt := range tests {
		got := renderEvent(tt.event)
		if tt.want == "" {
			if got != "" {
				t.Errorf("%s rendered %q, want silence", tt.event.Type, got)
			}
			continue
		}
		if !strings.Contains(got, tt.want) {
			t.Errorf("%s rendered %q, want it to contain %q", tt.event.Type, got, tt.want)
		}
	}
}

func TestOneLine_RuneSafe(t *testing.T) {
	// A cut that lands mid-rune must not emit invalid UTF-8.
	got := oneLine(strings.Repeat("世", 100), 60)
	if !utf8.ValidString(got) {
		t.Errorf("oneLine produced invalid UTF-8: %q", got)
	}
	if r := []rune(got); len(r) != 60 { // 59 kept + the ellipsis
		t.Errorf("got %d runes, want 60", len(r))
	}
	// Short strings pass through untouched.
	if oneLine("hello", 60) != "hello" {
		t.Error("short string was altered")
	}
}
