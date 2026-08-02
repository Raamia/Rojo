package benchmark

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func ctxFor(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestClient_CreateJob(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Job{ID: "job-1", Status: "queued"})
	}))
	defer srv.Close()

	job, err := NewClient(srv.URL, "").CreateJob(ctxFor(t), "do it", "/repo")
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if job.ID != "job-1" {
		t.Errorf("ID = %q, want job-1", job.ID)
	}
	if gotBody["task"] != "do it" || gotBody["repo_path"] != "/repo" {
		t.Errorf("request body = %v", gotBody)
	}
}

func TestClient_SendsBearerToken(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(Job{ID: "x"})
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL, "secret").GetJob(ctxFor(t), "x"); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer secret" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer secret")
	}
}

func TestJob_Terminal(t *testing.T) {
	terminal := []string{"completed", "failed", "cancelled"}
	running := []string{"queued", "planning", "implementing", "verifying", "reviewing", "waiting_for_revision"}
	for _, s := range terminal {
		if !(Job{Status: s}).Terminal() {
			t.Errorf("%q should be terminal", s)
		}
	}
	for _, s := range running {
		if (Job{Status: s}).Terminal() {
			t.Errorf("%q should not be terminal", s)
		}
	}
}

func TestClient_WaitForTerminalPollsUntilDone(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		status := "implementing"
		if n >= 3 {
			status = "completed"
		}
		_ = json.NewEncoder(w).Encode(Job{ID: "x", Status: status})
	}))
	defer srv.Close()

	job, err := NewClient(srv.URL, "").WaitForTerminal(ctxFor(t), "x", time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForTerminal: %v", err)
	}
	if job.Status != "completed" {
		t.Errorf("Status = %q, want completed", job.Status)
	}
	if calls.Load() < 3 {
		t.Errorf("polled %d times, want at least 3", calls.Load())
	}
}

// A job that never finishes must surface as a timeout naming the status it was
// stuck in, not as a silent hang.
func TestClient_WaitForTerminalGivesUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Job{ID: "x", Status: "verifying"})
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := NewClient(srv.URL, "").WaitForTerminal(ctx, "x", time.Millisecond)
	if err == nil {
		t.Fatal("WaitForTerminal returned nil for a job that never finished")
	}
	if !strings.Contains(err.Error(), "verifying") {
		t.Errorf("err = %v, want it to name the status the job was stuck in", err)
	}
}

// A job with no patch is a real outcome — the model may have proposed nothing —
// so it must not read as an error.
func TestClient_DiffMissingIsEmptyNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no diff for job in status failed"}`))
	}))
	defer srv.Close()

	diff, err := NewClient(srv.URL, "").Diff(ctxFor(t), "x")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if diff != "" {
		t.Errorf("Diff = %q, want empty", diff)
	}
}

func TestClient_DiffReturnsPatchVerbatim(t *testing.T) {
	patch := "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/x-diff")
		_, _ = w.Write([]byte(patch))
	}))
	defer srv.Close()

	got, err := NewClient(srv.URL, "").Diff(ctxFor(t), "x")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if got != patch {
		t.Errorf("Diff = %q, want it byte-for-byte", got)
	}
}

// The token fields are new; a benchmark that silently read zeros would report
// every run as free.
func TestClient_MetricsParsesTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"jobs": {"completed": 3},
			"model": {"calls": 9, "errors": 1, "tokens": {"input": 1200, "output": 340, "total": 1540}},
			"revisions_requested": 2
		}`))
	}))
	defer srv.Close()

	m, err := NewClient(srv.URL, "").Metrics(ctxFor(t))
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if m.Model.Calls != 9 || m.Model.Errors != 1 {
		t.Errorf("calls/errors = %d/%d, want 9/1", m.Model.Calls, m.Model.Errors)
	}
	if m.Model.Tokens.Input != 1200 || m.Model.Tokens.Output != 340 {
		t.Errorf("tokens = %d/%d, want 1200/340", m.Model.Tokens.Input, m.Model.Tokens.Output)
	}
	if m.RevisionsRequested != 2 {
		t.Errorf("RevisionsRequested = %d, want 2", m.RevisionsRequested)
	}
}

// A wrong address is the commonest way to start a run that was never going to
// work, so the error has to say what to do about it.
func TestClient_UnreachableServerExplainsItself(t *testing.T) {
	// Port 1 on loopback: reserved, and nothing will be listening.
	err := NewClient("http://127.0.0.1:1", "").Health(ctxFor(t))
	if err == nil {
		t.Fatal("Health succeeded against a dead address")
	}
	if !strings.Contains(err.Error(), "rojo-api") {
		t.Errorf("err = %v, want it to say how to start a server", err)
	}
}

func TestClient_ErrorStatusIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"queue full, try again later"}`))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "").CreateJob(ctxFor(t), "t", "/r")
	if err == nil {
		t.Fatal("CreateJob succeeded on a 503")
	}
	if !strings.Contains(err.Error(), "queue full") {
		t.Errorf("err = %v, want the server's own message", err)
	}
}

func TestCountRevisions(t *testing.T) {
	if got := countRevisions(nil); got != 0 {
		t.Errorf("countRevisions(nil) = %d, want 0", got)
	}
}
