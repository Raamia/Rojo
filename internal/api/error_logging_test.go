package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Raamia/Rojo/internal/jobs"
)

// brokenRepo fails every operation, standing in for a dead database.
type brokenRepo struct{ err error }

func (b brokenRepo) Create(context.Context, *jobs.Job) error        { return b.err }
func (b brokenRepo) Get(context.Context, string) (*jobs.Job, error) { return nil, b.err }
func (b brokenRepo) Update(context.Context, *jobs.Job) error        { return b.err }
func (b brokenRepo) List(context.Context) ([]*jobs.Job, error)      { return nil, b.err }

// A 500 that logs nothing is an incident with no root cause: the client gets a
// generic message and the only server-side trace is the access log's
// "status=500". The underlying error (bad credentials, missing relation,
// connection refused) has to reach the logs.
func TestErrorLogging_500sLogTheUnderlyingCause(t *testing.T) {
	cause := errors.New(`relation "jobs" does not exist (SQLSTATE 42P01)`)
	repo := brokenRepo{err: cause}

	tests := []struct {
		name    string
		request func() *http.Request
		invoke  func(h *JobsHandler, w http.ResponseWriter, r *http.Request)
	}{
		{
			name: "create",
			request: func() *http.Request {
				body := `{"task":"a valid task here","repo_path":"/tmp/repo"}`
				return httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(body))
			},
			invoke: func(h *JobsHandler, w http.ResponseWriter, r *http.Request) { h.Create(w, r) },
		},
		{
			name:    "get",
			request: func() *http.Request { return httptest.NewRequest(http.MethodGet, "/api/v1/jobs/abc", nil) },
			invoke:  func(h *JobsHandler, w http.ResponseWriter, r *http.Request) { h.Get(w, r) },
		},
		{
			name:    "list",
			request: func() *http.Request { return httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil) },
			invoke:  func(h *JobsHandler, w http.ResponseWriter, r *http.Request) { h.List(w, r) },
		},
		{
			name:    "cancel",
			request: func() *http.Request { return httptest.NewRequest(http.MethodPost, "/api/v1/jobs/abc/cancel", nil) },
			invoke:  func(h *JobsHandler, w http.ResponseWriter, r *http.Request) { h.Cancel(w, r) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

			h := NewJobsHandler(repo, nil, nil, nil)
			req := tt.request().WithContext(context.WithValue(context.Background(), loggerKey, logger))
			rec := httptest.NewRecorder()
			tt.invoke(h, rec, req)

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			if !strings.Contains(logs.String(), "SQLSTATE 42P01") {
				t.Errorf("500 did not log the underlying cause; logs were:\n%s", logs.String())
			}
		})
	}
}
