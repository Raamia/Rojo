package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// chain builds the production middleware order: recovery inside the logger, so
// a caught panic already has the request id in context.
func chain(h http.Handler, logTo *bytes.Buffer) http.Handler {
	logger := slog.New(slog.NewTextHandler(logTo, nil))
	return LoggerMiddleware(logger)(RecoverMiddleware()(h))
}

// net/http survives a handler panic by closing the connection with no status
// line, which reaches the client as a truncated response rather than an error.
// The middleware turns it into something a client can actually act on.
func TestRecover_PanicBecomesA500(t *testing.T) {
	var logs bytes.Buffer
	h := chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("handler exploded")
	}), &logs)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "internal server error") {
		t.Errorf("body = %q, want a JSON error", body)
	}
	// The panic message is an internal detail; the client gets a generic one.
	if strings.Contains(rec.Body.String(), "exploded") {
		t.Errorf("the panic message leaked to the client: %s", rec.Body)
	}
}

// A panic with no stack in the log is a panic nobody can debug.
func TestRecover_LogsThePanicWithAStack(t *testing.T) {
	var logs bytes.Buffer
	h := chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("handler exploded")
	}), &logs)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil))

	out := logs.String()
	for _, want := range []string{"panic serving request", "handler exploded", "request_id"} {
		if !strings.Contains(out, want) {
			t.Errorf("log is missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "recover_test.go") {
		t.Errorf("log has no stack trace pointing at the panic:\n%s", out)
	}
}

// One bad request must not affect the next.
func TestRecover_ServerKeepsServing(t *testing.T) {
	var logs bytes.Buffer
	h := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/boom" {
			panic("handler exploded")
		}
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}), &logs)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/boom", nil))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fine", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status %d after an earlier panic, want 200", rec.Code)
	}
}

func TestRecover_NormalRequestsAreUntouched(t *testing.T) {
	var logs bytes.Buffer
	h := chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusCreated, map[string]string{"id": "abc"})
	}), &logs)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/jobs", nil))
	if rec.Code != http.StatusCreated {
		t.Errorf("status %d, want 201", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "abc") {
		t.Errorf("body = %q", rec.Body)
	}
}

// http.ErrAbortHandler is how a handler deliberately drops a connection. The
// streaming endpoint relies on it, so converting it into a 500 would turn an
// intentional abort into a reported error.
func TestRecover_PassesThroughErrAbortHandler(t *testing.T) {
	var logs bytes.Buffer
	h := chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}), &logs)

	defer func() {
		if r := recover(); r != http.ErrAbortHandler {
			t.Errorf("recovered %v, want ErrAbortHandler to propagate", r)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil))
	t.Error("ErrAbortHandler was swallowed")
}

// Writing a response into a hijacked connection would panic inside the recovery
// handler itself, which is the one place a second panic cannot be caught.
func TestRecover_DoesNotWriteToAHijackedConnection(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	rec.hijacked = true

	h := RecoverMiddleware()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("upgrade then explode")
	}))
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs/x/stream", nil))

	if rec.status != http.StatusOK {
		t.Errorf("status was rewritten to %d on a hijacked connection", rec.status)
	}
	if body := rec.ResponseWriter.(*httptest.ResponseRecorder).Body.String(); body != "" {
		t.Errorf("wrote %q into a hijacked connection", body)
	}
}
