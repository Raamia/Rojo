package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/jobs"
)

func seedJobs(t *testing.T, n int) *JobsHandler {
	t.Helper()
	repo := jobs.NewInMemoryRepository()
	base := time.Now().UTC()
	for i := 0; i < n; i++ {
		j := &jobs.Job{
			ID: fmt.Sprintf("job-%03d", i), Task: "t", RepoPath: "/tmp/repo",
			Status:    jobs.StatusQueued,
			CreatedAt: base.Add(time.Duration(i) * time.Second),
			UpdatedAt: base.Add(time.Duration(i) * time.Second),
		}
		if err := repo.Create(context.Background(), j); err != nil {
			t.Fatal(err)
		}
	}
	return NewJobsHandler(repo, noopQueue{}, noopCanceller{}, nil)
}

func listJobs(t *testing.T, h *JobsHandler, query string) (*httptest.ResponseRecorder, []*jobs.Job) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.List(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs"+query, nil))
	var got []*jobs.Job
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal: %v — body %s", err, rec.Body)
		}
	}
	return rec, got
}

// Without a cap, one request serialises every job the service has ever run.
func TestList_DefaultsToABoundedPage(t *testing.T) {
	h := seedJobs(t, DefaultListLimit+20)

	rec, got := listJobs(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if len(got) != DefaultListLimit {
		t.Errorf("got %d jobs, want the %d default", len(got), DefaultListLimit)
	}
	// A client cannot page without knowing how many there are.
	if total := rec.Header().Get("X-Total-Count"); total != strconv.Itoa(DefaultListLimit+20) {
		t.Errorf("X-Total-Count = %q, want %d", total, DefaultListLimit+20)
	}
}

func TestList_LimitAndOffset(t *testing.T) {
	h := seedJobs(t, 10)

	_, first := listJobs(t, h, "?limit=3")
	if len(first) != 3 {
		t.Fatalf("got %d, want 3", len(first))
	}
	_, second := listJobs(t, h, "?limit=3&offset=3")
	if len(second) != 3 {
		t.Fatalf("got %d, want 3", len(second))
	}
	// Pages must not overlap, or a client walking them sees duplicates.
	for _, a := range first {
		for _, b := range second {
			if a.ID == b.ID {
				t.Errorf("job %s appears on both pages", a.ID)
			}
		}
	}
}

// A client asking for a million rows is still capped.
func TestList_LimitIsClamped(t *testing.T) {
	h := seedJobs(t, MaxListLimit+50)

	_, got := listJobs(t, h, "?limit=100000")
	if len(got) != MaxListLimit {
		t.Errorf("got %d jobs, want the %d cap", len(got), MaxListLimit)
	}
}

// A client that sent limit=-1 has a bug; quietly returning the default hides it.
func TestList_RejectsNonsenseParams(t *testing.T) {
	h := seedJobs(t, 5)
	for _, q := range []string{"?limit=-1", "?limit=0", "?limit=abc", "?offset=-5", "?offset=x"} {
		t.Run(q, func(t *testing.T) {
			if rec, _ := listJobs(t, h, q); rec.Code != http.StatusBadRequest {
				t.Errorf("status %d for %s, want 400", rec.Code, q)
			}
		})
	}
}

// Paging past the end is an empty page, not an error and not a panic.
func TestList_OffsetBeyondTheEnd(t *testing.T) {
	h := seedJobs(t, 3)

	rec, got := listJobs(t, h, "?offset=999")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if len(got) != 0 {
		t.Errorf("got %d jobs, want none", len(got))
	}
	if rec.Body.String() == "null\n" || rec.Body.String() == "null" {
		t.Error("body is null; an empty page must serialise as []")
	}
}

// The body stays a plain array so a client written before paging still works.
func TestList_BodyIsStillAnArray(t *testing.T) {
	h := seedJobs(t, 2)
	rec, _ := listJobs(t, h, "")
	if b := rec.Body.Bytes(); len(b) == 0 || b[0] != '[' {
		t.Errorf("body is not a JSON array: %s", rec.Body)
	}
}

func TestList_EmptyRepository(t *testing.T) {
	h := seedJobs(t, 0)
	rec, got := listJobs(t, h, "")
	if rec.Code != http.StatusOK || len(got) != 0 {
		t.Errorf("status %d, %d jobs", rec.Code, len(got))
	}
	if rec.Header().Get("X-Total-Count") != "0" {
		t.Errorf("X-Total-Count = %q", rec.Header().Get("X-Total-Count"))
	}
}
