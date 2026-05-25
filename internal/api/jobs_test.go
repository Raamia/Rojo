package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Raamia/Rojo/internal/jobs"
)

type noopQueue struct{}

func (noopQueue) Enqueue(string) error { return nil }

type noopCanceller struct{}

func (noopCanceller) Cancel(string) error { return nil }

func newTestHandler() (*JobsHandler, *jobs.InMemoryRepository) {
	repo := jobs.NewInMemoryRepository()
	return NewJobsHandler(repo, noopQueue{}, noopCanceller{}, nil), repo
}

func TestCreateJob_Success(t *testing.T) {
	h, _ := newTestHandler()
	body := `{"task":"add a new endpoint","repo_path":"/tmp/repo"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusCreated)
	}
	var got jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID == "" {
		t.Error("expected ID to be set")
	}
	if got.Status != jobs.StatusQueued {
		t.Errorf("expected status queued, got %s", got.Status)
	}
	if got.CreatedAt.IsZero() {
		t.Error("expected CreatedAt to be set")
	}
}

func TestCreateJob_InvalidJSON(t *testing.T) {
	h, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader("not json"))
	rec := httptest.NewRecorder()
	h.Create(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestCreateJob_ValidationError(t *testing.T) {
	h, _ := newTestHandler()
	body := `{"task":"","repo_path":"/tmp/repo"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.Create(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestGetJob_Success(t *testing.T) {
	h, repo := newTestHandler()
	seed := &jobs.Job{ID: "abc", Task: "x", RepoPath: "/tmp/r", Status: jobs.StatusQueued}
	if err := repo.Create(context.Background(), seed); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/abc", nil)
	req.SetPathValue("jobID", "abc")
	rec := httptest.NewRecorder()
	h.Get(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusOK)
	}
	var got jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != "abc" {
		t.Errorf("got id %s, want abc", got.ID)
	}
}

func TestGetJob_NotFound(t *testing.T) {
	h, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/missing", nil)
	req.SetPathValue("jobID", "missing")
	rec := httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestListJobs(t *testing.T) {
	h, repo := newTestHandler()
	ctx := context.Background()
	_ = repo.Create(ctx, &jobs.Job{ID: "1", Task: "a", RepoPath: "/tmp/r", Status: jobs.StatusQueued})
	_ = repo.Create(ctx, &jobs.Job{ID: "2", Task: "b", RepoPath: "/tmp/r", Status: jobs.StatusQueued})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	rec := httptest.NewRecorder()
	h.List(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusOK)
	}
	var got []jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d jobs, want 2", len(got))
	}
}

func TestListJobs_Empty(t *testing.T) {
	h, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	rec := httptest.NewRecorder()
	h.List(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusOK)
	}
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("expected empty JSON array, got %q", rec.Body.String())
	}
}
