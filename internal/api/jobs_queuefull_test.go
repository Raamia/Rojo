package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Raamia/Rojo/internal/jobs"
)

// fullQueue always rejects, simulating a saturated queue.
type fullQueue struct{}

func (fullQueue) Enqueue(string) error { return errQueueFullTest }

type queueFullErr struct{}

func (queueFullErr) Error() string { return "queue is full" }

var errQueueFullTest = queueFullErr{}

// A submission rejected with 503 must not leave a job sitting in `queued`
// forever. The handler persists the job before enqueueing, so when the enqueue
// fails the record has to be moved out of the pending state — otherwise every
// 503 leaks a zombie job that no worker will ever claim. A load test against
// the unfixed code left 2062 such jobs stuck in `queued`.
func TestCreate_QueueFullLeavesNoPhantomQueuedJob(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	h := NewJobsHandler(repo, fullQueue{}, nil, nil)

	body := `{"task":"a job that cannot be queued","repo_path":"/tmp/repo"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.Create(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	all, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, j := range all {
		if j.Status == jobs.StatusQueued {
			t.Errorf("job %s left in %q after a 503 — nothing will ever process it",
				j.ID, j.Status)
		}
		if j.Status != jobs.StatusFailed {
			t.Errorf("job %s status = %q, want %q", j.ID, j.Status, jobs.StatusFailed)
		}
	}
}
