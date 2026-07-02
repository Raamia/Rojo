package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/orchestration"
)

type stubArtifacts struct {
	data map[string][]byte
	err  error
}

func (s stubArtifacts) ReadArtifact(jobID, name string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	b, ok := s.data[jobID+"/"+name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return b, nil
}

const testPatch = "diff --git a/greet.go b/greet.go\nnew file mode 100644\n--- /dev/null\n+++ b/greet.go\n@@ -0,0 +1 @@\n+package main\n"

func newDiffTest(t *testing.T, status jobs.JobStatus, arts ArtifactReader) *DiffHandler {
	t.Helper()
	repo := jobs.NewInMemoryRepository()
	job := &jobs.Job{
		ID: "job-1", Task: "t", RepoPath: "/tmp/repo", Status: jobs.StatusQueued,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := repo.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	job.Status = status // set directly: this is about the handler, not transitions
	if err := repo.Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	return NewDiffHandler(repo, arts)
}

func getDiff(h *DiffHandler, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+id+"/diff", nil)
	req.SetPathValue("jobID", id)
	rec := httptest.NewRecorder()
	h.Diff(rec, req)
	return rec
}

// The patch is served as text so it can be piped straight into `git apply`.
// Wrapping it in JSON would force every consumer to unescape it first.
func TestDiff_ServesRawPatch(t *testing.T) {
	h := newDiffTest(t, jobs.StatusCompleted, stubArtifacts{
		data: map[string][]byte{"job-1/" + orchestration.ArtifactDiff: []byte(testPatch)},
	})

	rec := getDiff(h, "job-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec.Body.String() != testPatch {
		t.Errorf("body was altered:\n%q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/x-diff") {
		t.Errorf("Content-Type = %q, want text/x-diff", ct)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(testPatch)) {
		t.Errorf("Content-Length = %q, want %d", got, len(testPatch))
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "job-1.diff") {
		t.Errorf("Content-Disposition = %q, want the job-named filename", cd)
	}
}

// "Your ID is wrong" and "it is still running" are different problems and need
// different answers, so the job is looked up before the artifact.
func TestDiff_UnknownJobIsNotFound(t *testing.T) {
	h := newDiffTest(t, jobs.StatusCompleted, stubArtifacts{})

	rec := getDiff(h, "nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	if msg := errorMessage(t, rec.Body.Bytes()); !strings.Contains(msg, "job not found") {
		t.Errorf("message = %q, want it to name the missing job", msg)
	}
}

func TestDiff_RunningJobSaysWhy(t *testing.T) {
	h := newDiffTest(t, jobs.StatusVerifying, stubArtifacts{})

	rec := getDiff(h, "job-1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	// Naming the status is what turns a dead end into "check back shortly".
	if msg := errorMessage(t, rec.Body.Bytes()); !strings.Contains(msg, string(jobs.StatusVerifying)) {
		t.Errorf("message = %q, want it to name the job's status", msg)
	}
}

// A failed job's patch is the whole point of keeping it: it is what a human
// reads to find out what went wrong.
func TestDiff_FailedJobStillServesItsPatch(t *testing.T) {
	h := newDiffTest(t, jobs.StatusFailed, stubArtifacts{
		data: map[string][]byte{"job-1/" + orchestration.ArtifactDiff: []byte(testPatch)},
	})

	rec := getDiff(h, "job-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 — a failed job's patch is still readable", rec.Code)
	}
}

func TestDiff_StoreErrorIsServerError(t *testing.T) {
	h := newDiffTest(t, jobs.StatusCompleted, stubArtifacts{err: errors.New("i/o error")})

	rec := getDiff(h, "job-1")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	// The underlying error stays in the log; the client gets a generic message.
	if msg := errorMessage(t, rec.Body.Bytes()); strings.Contains(msg, "i/o error") {
		t.Errorf("internal error leaked to the client: %q", msg)
	}
}

func TestDiff_NoStoreConfigured(t *testing.T) {
	h := newDiffTest(t, jobs.StatusCompleted, nil)

	if rec := getDiff(h, "job-1"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
}

func errorMessage(t *testing.T, body []byte) string {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("error body is not JSON: %s", body)
	}
	s, _ := out["error"].(string)
	return s
}
