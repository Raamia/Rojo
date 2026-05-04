package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
)

// Hostile-input tests for the HTTP layer. Every test asserts the ACTUAL behavior so
// the suite stays green; names prefixed "BUG_" flag divergence from the contract in
// the documented API and validation contracts.

// ---------------------------------------------------------------------------
// stubs (distinct names from jobs_test.go's noopQueue / noopCanceller)
// ---------------------------------------------------------------------------

type rbStubQueue struct {
	mu   sync.Mutex
	err  error
	seen []string
}

func (q *rbStubQueue) Enqueue(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seen = append(q.seen, id)
	return q.err
}

func (q *rbStubQueue) count() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.seen)
}

type rbStubCanceller struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (c *rbStubCanceller) Cancel(string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.err
}

type rbStubBus struct {
	mu        sync.Mutex
	published []events.Event
	err       error
	panicNow  bool
}

func (b *rbStubBus) Publish(_ context.Context, e events.Event) error {
	if b.panicNow {
		panic("bus exploded")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published = append(b.published, e)
	return b.err
}
func (b *rbStubBus) Subscribe(jobID string, buffer int) *events.Subscription { return nil }
func (b *rbStubBus) Unsubscribe(*events.Subscription)                        {}

func (b *rbStubBus) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.published)
}

// rbFailingRepo lets us exercise the 500 branches.
type rbFailingRepo struct {
	jobs.JobRepository
	createErr error
	getErr    error
	listErr   error
	updateErr error
}

func (r *rbFailingRepo) Update(ctx context.Context, j *jobs.Job) error {
	if r.updateErr != nil {
		return r.updateErr
	}
	return r.JobRepository.Update(ctx, j)
}

func (r *rbFailingRepo) Create(ctx context.Context, j *jobs.Job) error {
	if r.createErr != nil {
		return r.createErr
	}
	return r.JobRepository.Create(ctx, j)
}
func (r *rbFailingRepo) Get(ctx context.Context, id string) (*jobs.Job, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	return r.JobRepository.Get(ctx, id)
}
func (r *rbFailingRepo) List(ctx context.Context) ([]*jobs.Job, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.JobRepository.List(ctx)
}

func newRobustnessHandler() (*JobsHandler, *jobs.InMemoryRepository, *rbStubQueue, *rbStubCanceller, *rbStubBus) {
	repo := jobs.NewInMemoryRepository()
	q := &rbStubQueue{}
	c := &rbStubCanceller{}
	b := &rbStubBus{}
	return NewJobsHandler(repo, q, c, b), repo, q, c, b
}

// rbPostJSON drives Create with an arbitrary body reader, failing the test (rather
// than crashing the run) if the handler panics, and failing if it does not return
// within the timeout so the suite can never hang.
func rbPostJSON(t *testing.T, h *JobsHandler, body io.Reader, timeout time.Duration) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", body)

	done := make(chan struct{})
	var panicVal any
	go func() {
		defer func() {
			if p := recover(); p != nil {
				panicVal = p
			}
			close(done)
		}()
		h.Create(rec, req)
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("HANG: Create did not return within %s", timeout)
	}
	if panicVal != nil {
		t.Fatalf("PANIC in Create: %v", panicVal)
	}
	return rec
}

func rbDecodeErrorBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("error body is not JSON: %q (%v)", rec.Body.String(), err)
	}
	return m
}

// ---------------------------------------------------------------------------
// 1. Malformed request bodies
// ---------------------------------------------------------------------------

func TestRobustness_MalformedBodiesNeverPanicAndAlwaysReturn400(t *testing.T) {
	deep := func(n int) string {
		return strings.Repeat(`{"task":`, n) + `"x"` + strings.Repeat(`}`, n)
	}
	cases := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"whitespace only body", "   \n\t  "},
		{"json null", "null"},
		{"json true", "true"},
		{"json number", "42"},
		{"json string", `"just a string"`},
		{"top-level array", "[]"},
		{"array of objects", `[{"task":"aaaa","repo_path":"/tmp"}]`},
		{"empty object", "{}"},
		{"task is a number", `{"task":123,"repo_path":"/tmp/repo"}`},
		{"task is null", `{"task":null,"repo_path":"/tmp/repo"}`},
		{"task is an object", `{"task":{"a":1},"repo_path":"/tmp/repo"}`},
		{"repo_path is an array", `{"task":"a valid task","repo_path":[]}`},
		{"repo_path is a bool", `{"task":"a valid task","repo_path":true}`},
		{"unterminated object", `{"task":"a valid task"`},
		{"unterminated string", `{"task":"a valid`},
		{"single quotes", `{'task':'aaaa','repo_path':'/tmp'}`},
		{"trailing comma", `{"task":"aaaa","repo_path":"/tmp",}`},
		{"nan literal", `{"task":NaN,"repo_path":"/tmp"}`},
		{"raw NUL inside string", "{\"task\":\"aa\x00aa\",\"repo_path\":\"/tmp/repo\"}"},
		{"raw tab inside string", "{\"task\":\"aa\taa\",\"repo_path\":\"/tmp/repo\"}"},
		{"UTF-8 BOM prefix", "\xef\xbb\xbf{\"task\":\"a valid task\",\"repo_path\":\"/tmp/repo\"}"},
		{"UTF-16 encoded body", "\xff\xfe{\x00\"\x00t\x00"},
		{"form encoded body", "task=aaaa&repo_path=/tmp"},
		{"xml body", `<job><task>aaaa</task></job>`},
		{"nesting depth 1000", deep(1000)},
		{"nesting depth 10001 (stdlib cap)", deep(10001)},
		{"nesting depth 200000", deep(200000)},
		{"missing repo_path", `{"task":"a valid task"}`},
		{"misspelled repo_path key", `{"task":"a valid task","repopath":"/tmp/repo"}`},
		{"missing task", `{"repo_path":"/tmp/repo"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, repo, q, _, bus := newRobustnessHandler()
			rec := rbPostJSON(t, h, strings.NewReader(tc.body), 10*time.Second)

			// Oversized inputs (e.g. very deep nesting) are now refused by the
			// body cap before the decoder ever sees them, which is a 413 rather
			// than a 400. Either is a clean rejection; neither may panic.
			if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("got status %d, want 400 or 413 (body=%.80q)", rec.Code, tc.body)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("got Content-Type %q, want application/json", ct)
			}
			m := rbDecodeErrorBody(t, rec)
			if len(m) != 1 {
				t.Errorf("error body has %d keys, want exactly 1: %v", len(m), m)
			}
			if _, ok := m["error"].(string); !ok {
				t.Errorf(`error body missing string "error" key: %v`, m)
			}
			// A rejected request must leave no trace.
			list, _ := repo.List(context.Background())
			if len(list) != 0 {
				t.Errorf("rejected request created %d jobs", len(list))
			}
			if q.count() != 0 {
				t.Errorf("rejected request enqueued %d jobs", q.count())
			}
			if bus.count() != 0 {
				t.Errorf("rejected request published %d events", bus.count())
			}
		})
	}
}

// Deeply nested input must not blow the stack or hang. The stdlib scanner caps
// nesting at 10000 and returns an error, so this is safe -- pinned here because it
// is a property the handler relies on without stating it.
func TestRobustness_DeeplyNestedJSONIsBoundedByStdlibDepthLimit(t *testing.T) {
	h, _, _, _, _ := newRobustnessHandler()
	body := strings.Repeat(`[`, 1_000_000) + strings.Repeat(`]`, 1_000_000)

	start := time.Now()
	rec := rbPostJSON(t, h, strings.NewReader(body), 15*time.Second)
	elapsed := time.Since(start)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400", rec.Code)
	}
	t.Logf("1,000,000-level nesting rejected in %s (stdlib max depth 10000)", elapsed)
}

// ---------------------------------------------------------------------------
// 2. Silently accepted malformed-ish input
// ---------------------------------------------------------------------------

// json.Decoder.Decode stops at the end of the first JSON value and never checks for
// trailing data, so anything after the object is silently discarded.
func TestRobustness_BUG_TrailingGarbageAfterValidJSONIsSilentlyIgnored(t *testing.T) {
	cases := map[string]string{
		"trailing text":        `{"task":"a valid task","repo_path":"/tmp/repo"} DROP TABLE jobs`,
		"trailing binary":      "{\"task\":\"a valid task\",\"repo_path\":\"/tmp/repo\"}\xff\xff\xff",
		"second json object":   `{"task":"first task","repo_path":"/a"}{"task":"second task","repo_path":"/b"}`,
		"trailing null bytes":  "{\"task\":\"a valid task\",\"repo_path\":\"/tmp/repo\"}\x00\x00",
		"megabyte of trailing": `{"task":"a valid task","repo_path":"/tmp/repo"}` + strings.Repeat("A", 1<<20),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			h, _, _, _, _ := newRobustnessHandler()
			rec := rbPostJSON(t, h, strings.NewReader(body), 10*time.Second)
			if rec.Code != http.StatusCreated {
				t.Fatalf("got status %d, want 201 (documenting actual behavior)", rec.Code)
			}
		})
	}
	t.Log("CONFIRMED BUG: trailing bytes after the JSON object are accepted; a body " +
		"containing two objects silently creates a job from the first and drops the second")
}

func TestRobustness_BUG_DuplicateJSONKeysSilentlyTakeTheLastValue(t *testing.T) {
	h, repo, _, _, _ := newRobustnessHandler()
	body := `{"task":"benign task text","repo_path":"/tmp/safe","task":"malicious task text","repo_path":"/tmp/evil"}`
	rec := rbPostJSON(t, h, strings.NewReader(body), 5*time.Second)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got status %d, want 201", rec.Code)
	}
	list, _ := repo.List(context.Background())
	if len(list) != 1 {
		t.Fatalf("got %d jobs, want 1", len(list))
	}
	if list[0].Task != "malicious task text" || list[0].RepoPath != "/tmp/evil" {
		t.Fatalf("got task=%q repo=%q, want the LAST duplicate to win",
			list[0].Task, list[0].RepoPath)
	}
	t.Log("CONFIRMED BUG: duplicate keys are accepted and the last value wins; a " +
		"validating proxy reading the first value would disagree with the server")
}

func TestRobustness_BUG_UnknownFieldsAreSilentlyIgnored(t *testing.T) {
	h, repo, _, _, _ := newRobustnessHandler()
	body := `{"task":"a valid task","repo_path":"/tmp/repo","status":"completed","ID":"attacker-chosen-id","CreatedAt":"1999-01-01T00:00:00Z"}`
	rec := rbPostJSON(t, h, strings.NewReader(body), 5*time.Second)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got status %d, want 201", rec.Code)
	}
	list, _ := repo.List(context.Background())
	if list[0].ID == "attacker-chosen-id" {
		t.Fatal("SECURITY: client controlled the job ID")
	}
	if list[0].Status != jobs.StatusQueued {
		t.Fatalf("SECURITY: client controlled status, got %q", list[0].Status)
	}
	t.Log("OK: mass assignment is not possible (NewJobRequest only has task/repo_path), " +
		"but unknown fields are accepted without error, so client typos fail silently")
}

// The JSON escape \u0000 is legal JSON and decodes to a real NUL byte, which
// survives validation and is persisted.
func TestRobustness_EscapedNULByteIsRejected(t *testing.T) {
	h, repo, _, _, _ := newRobustnessHandler()
	body := `{"task":"aa\u0000aa","repo_path":"/tmp/re\u0000po"}`
	rec := rbPostJSON(t, h, strings.NewReader(body), 5*time.Second)
	// FIXED: rejected at the edge as a 400. Previously the NUL reached the job
	// record, where postgres would refuse the insert (invalid byte sequence for
	// encoding UTF8: 0x00) and any syscall on repo_path would fail EINVAL --
	// surfacing as a 500 for what is really a bad request.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400", rec.Code)
	}
	list, _ := repo.List(context.Background())
	if len(list) != 0 {
		t.Fatalf("expected no job to be persisted, got %d", len(list))
	}
}

// encoding/json replaces invalid UTF-8 with U+FFFD instead of erroring, so the task
// the server stores is not the task the client sent.
func TestRobustness_BUG_InvalidUTF8IsSilentlyCorruptedNotRejected(t *testing.T) {
	h, repo, _, _, _ := newRobustnessHandler()
	sent := "fix the \xff\xfe parser"
	body := fmt.Sprintf("{\"task\":\"%s\",\"repo_path\":\"/tmp/repo\"}", sent)

	rec := rbPostJSON(t, h, strings.NewReader(body), 5*time.Second)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got status %d, want 201 (documenting actual behavior)", rec.Code)
	}
	list, _ := repo.List(context.Background())
	if list[0].Task == sent {
		t.Fatal("expected the invalid UTF-8 to have been rewritten")
	}
	if !strings.Contains(list[0].Task, "�") {
		t.Fatalf("expected U+FFFD replacement chars, got %q", list[0].Task)
	}
	t.Logf("CONFIRMED BUG: sent %q, stored %q -- silent data corruption, no error",
		sent, list[0].Task)
}

// Unpaired UTF-16 surrogate escapes are also rewritten to U+FFFD rather than
// rejected, so a body that is not valid JSON text still produces a 201.
func TestRobustness_LoneSurrogateEscapesAreRewrittenThenLengthChecked(t *testing.T) {
	h, repo, _, _, _ := newRobustnessHandler()
	// Two unpaired high surrogates: invalid JSON string content by RFC 8259.
	// encoding/json rewrites each to U+FFFD rather than erroring, so the task
	// is 2 characters -- which the rune-based minimum now rejects. (Before the
	// rune fix this was 6 bytes and sneaked past a byte-based minimum of 4.)
	rec := rbPostJSON(t, h, strings.NewReader(`{"task":"\ud800\ud800","repo_path":"/tmp/repo"}`), 5*time.Second)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400", rec.Code)
	}
	list, _ := repo.List(context.Background())
	if len(list) != 0 {
		t.Fatalf("expected no job to be persisted, got %d", len(list))
	}
}

func TestRobustness_BUG_ContentTypeHeaderIsNeverValidated(t *testing.T) {
	for _, ct := range []string{"text/plain", "text/html", "application/x-www-form-urlencoded", ""} {
		t.Run("content-type="+ct, func(t *testing.T) {
			h, _, _, _, _ := newRobustnessHandler()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs",
				strings.NewReader(`{"task":"a valid task","repo_path":"/tmp/repo"}`))
			if ct != "" {
				req.Header.Set("Content-Type", ct)
			}
			rec := httptest.NewRecorder()
			h.Create(rec, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("got status %d, want 201 (documenting actual behavior)", rec.Code)
			}
		})
	}
	t.Log("CONFIRMED GAP: no Content-Type enforcement. A cross-origin HTML form can " +
		"POST text/plain and create jobs (simple request, no CORS preflight)")
}

// ---------------------------------------------------------------------------
// 3. Unbounded request bodies (memory exhaustion)
// ---------------------------------------------------------------------------

// rbFillReader streams n copies of b without allocating n bytes up front.
type rbFillReader struct {
	b    byte
	left int
}

func (r *rbFillReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.left {
		n = r.left
	}
	for i := 0; i < n; i++ {
		p[i] = r.b
	}
	r.left -= n
	return n, nil
}

type rbTallyReader struct {
	r io.Reader
	n int64
}

func (c *rbTallyReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func rbHugeTaskBody(size int) *rbTallyReader {
	return &rbTallyReader{r: io.MultiReader(
		strings.NewReader(`{"task":"`),
		&rbFillReader{b: 'a', left: size},
		strings.NewReader(`","repo_path":"/tmp/repo"}`),
	)}
}

// FIXED: the task is trimmed before it is length-checked AND before it is
// stored, so padding can no longer smuggle an oversized value past the cap.
// A multi-megabyte pad is now refused by the body limit first.
func TestRobustness_WhitespacePaddedTaskCannotBypassTheLimit(t *testing.T) {
	h, repo, _, _, _ := newRobustnessHandler()
	pad := strings.Repeat(" ", 8<<20)
	rec := rbPostJSON(t, h, strings.NewReader(`{"task":"`+pad+`abcd","repo_path":"/tmp/repo"}`), 30*time.Second)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got status %d, want 413", rec.Code)
	}

	// A pad small enough to fit under the body cap is trimmed, not persisted.
	rec = rbPostJSON(t, h, strings.NewReader(`{"task":"`+strings.Repeat(" ", 1024)+`abcd","repo_path":"/tmp/repo"}`), 10*time.Second)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got status %d, want 201", rec.Code)
	}
	list, _ := repo.List(context.Background())
	if list[0].Task != "abcd" {
		t.Errorf("stored task is %d bytes, want the trimmed 4-character value", len(list[0].Task))
	}
}

// FIXED: repo_path is capped at PATH_MAX, and an oversized body is refused
// before it is even decoded.
func TestRobustness_UnboundedRepoPathIsRejected(t *testing.T) {
	h, _, _, _, _ := newRobustnessHandler()
	huge := "/" + strings.Repeat("a", 4<<20)
	rec := rbPostJSON(t, h, strings.NewReader(`{"task":"a valid task","repo_path":"`+huge+`"}`), 30*time.Second)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got status %d, want 413", rec.Code)
	}

	// Under the body cap but still longer than PATH_MAX: rejected by validation.
	long := "/" + strings.Repeat("a", 5000)
	rec = rbPostJSON(t, h, strings.NewReader(`{"task":"a valid task","repo_path":"`+long+`"}`), 10*time.Second)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400", rec.Code)
	}
}
func TestRobustness_QueueFullMarksThePersistedJobFailedAndReturns503(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	q := &rbStubQueue{err: errors.New("queue is full")}
	bus := &rbStubBus{}
	h := NewJobsHandler(repo, q, &rbStubCanceller{}, bus)

	rec := rbPostJSON(t, h, strings.NewReader(`{"task":"a valid task","repo_path":"/tmp/repo"}`), 5*time.Second)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want 503", rec.Code)
	}

	list, _ := repo.List(context.Background())
	if len(list) != 1 {
		t.Fatalf("got %d jobs, want 1 (documenting actual behavior)", len(list))
	}
	if list[0].Status != jobs.StatusFailed {
		t.Fatalf("got status %q, want failed -- an unqueueable job must not linger as queued",
			list[0].Status)
	}
	if bus.count() != 0 {
		t.Errorf("job.created was published for a request that returned 503")
	}
	t.Logf("OK: job %s was persisted then transitioned to %q. NOTE: the row is still "+
		"retained, so a client retry loop against a full queue grows the jobs table "+
		"by one failed row per attempt, and no job.failed event is published",
		list[0].ID, list[0].Status)
}

// The repository Update that records the failed status is best-effort: if it errors
// the handler only logs, and the job is left as `queued` with no worker to run it.
func TestRobustness_BUG_QueueFullPlusUpdateFailureStillLeavesAQueuedOrphan(t *testing.T) {
	repo := &rbFailingRepo{
		JobRepository: jobs.NewInMemoryRepository(),
		updateErr:     errors.New("database is down"),
	}
	q := &rbStubQueue{err: errors.New("queue is full")}
	h := NewJobsHandler(repo, q, &rbStubCanceller{}, nil)

	rec := rbPostJSON(t, h, strings.NewReader(`{"task":"a valid task","repo_path":"/tmp/repo"}`), 5*time.Second)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want 503", rec.Code)
	}
	list, _ := repo.List(context.Background())
	if len(list) != 1 {
		t.Fatalf("got %d jobs, want 1", len(list))
	}
	if list[0].Status != jobs.StatusQueued {
		t.Fatalf("got status %q, want queued (documenting actual behavior)", list[0].Status)
	}
	t.Log("CONFIRMED GAP (internal/api/jobs.go:64-69): the compensating Update is " +
		"best-effort. If it fails the phantom `queued` job survives and only a log " +
		"line records it -- there is no reconciler that sweeps stale queued jobs")
}

func TestRobustness_RepositoryFailuresMapTo500WithJSONBody(t *testing.T) {
	boom := errors.New("connection refused")

	t.Run("create", func(t *testing.T) {
		h := NewJobsHandler(&rbFailingRepo{JobRepository: jobs.NewInMemoryRepository(), createErr: boom},
			&rbStubQueue{}, &rbStubCanceller{}, nil)
		rec := rbPostJSON(t, h, strings.NewReader(`{"task":"a valid task","repo_path":"/tmp/repo"}`), 5*time.Second)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("got status %d, want 500", rec.Code)
		}
		if m := rbDecodeErrorBody(t, rec); m["error"] != "failed to create job" {
			t.Errorf("got error %q", m["error"])
		}
	})

	t.Run("get", func(t *testing.T) {
		h := NewJobsHandler(&rbFailingRepo{JobRepository: jobs.NewInMemoryRepository(), getErr: boom},
			&rbStubQueue{}, &rbStubCanceller{}, nil)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/x", nil)
		req.SetPathValue("jobID", "x")
		rec := httptest.NewRecorder()
		h.Get(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("got status %d, want 500", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "connection refused") {
			t.Error("SECURITY: internal error text leaked to the client")
		}
	})

	t.Run("list", func(t *testing.T) {
		h := NewJobsHandler(&rbFailingRepo{JobRepository: jobs.NewInMemoryRepository(), listErr: boom},
			&rbStubQueue{}, &rbStubCanceller{}, nil)
		rec := httptest.NewRecorder()
		h.List(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("got status %d, want 500", rec.Code)
		}
	})
}

// A duplicate ID collision (or any Create error) is reported as 500. Included to
// show the ErrJobAlreadyExists path is not distinguished.
func TestRobustness_DuplicateJobIDCollisionSurfacesAs500(t *testing.T) {
	h := NewJobsHandler(&rbFailingRepo{JobRepository: jobs.NewInMemoryRepository(),
		createErr: jobs.ErrJobAlreadyExists}, &rbStubQueue{}, &rbStubCanceller{}, nil)
	rec := rbPostJSON(t, h, strings.NewReader(`{"task":"a valid task","repo_path":"/tmp/repo"}`), 5*time.Second)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// 5. Cancel contract
// ---------------------------------------------------------------------------

// API contract: "409 Conflict -- cancel requested for a job that isn't
// running (Canceller returns ErrJobNotRunning)". In practice EVERY Canceller error
// becomes 409 and its text is echoed to the client verbatim.
func TestRobustness_BUG_AllCancellerErrorsBecome409AndLeakTheirText(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	if err := repo.Create(context.Background(), &jobs.Job{ID: "job-1", Task: "t",
		RepoPath: "/tmp/r", Status: jobs.StatusQueued}); err != nil {
		t.Fatal(err)
	}
	secret := "dial tcp 10.0.0.5:5432: connect: connection refused (user=rojo password=hunter2)"
	h := NewJobsHandler(repo, &rbStubQueue{}, &rbStubCanceller{err: errors.New(secret)}, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/job-1/cancel", nil)
	req.SetPathValue("jobID", "job-1")
	rec := httptest.NewRecorder()
	h.Cancel(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("got status %d, want 409 (documenting actual behavior)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatalf("expected the raw error text to be echoed, got %q", rec.Body.String())
	}
	t.Log("CONFIRMED BUG (internal/api/jobs.go:101-103): any Canceller error -- not " +
		"just ErrJobNotRunning -- returns 409 with the raw error string in the body, " +
		"leaking internal detail and mislabelling infrastructure failures as conflicts")
}

func TestRobustness_CancelWithoutCancellerReturns503(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	_ = repo.Create(context.Background(), &jobs.Job{ID: "job-1", Status: jobs.StatusQueued})
	h := NewJobsHandler(repo, &rbStubQueue{}, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/job-1/cancel", nil)
	req.SetPathValue("jobID", "job-1")
	rec := httptest.NewRecorder()
	h.Cancel(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want 503", rec.Code)
	}
}

func TestRobustness_CancelIsNotIdempotentAcrossRepeatedCalls(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	_ = repo.Create(context.Background(), &jobs.Job{ID: "job-1", Status: jobs.StatusQueued})
	c := &rbStubCanceller{}
	h := NewJobsHandler(repo, &rbStubQueue{}, c, nil)

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/job-1/cancel", nil)
		req.SetPathValue("jobID", "job-1")
		rec := httptest.NewRecorder()
		h.Cancel(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("call %d: got status %d, want 202", i, rec.Code)
		}
	}
	if c.calls != 3 {
		t.Fatalf("got %d Cancel calls, want 3", c.calls)
	}
	t.Log("NOTE: Cancel is unauthenticated per-job and unthrottled; each POST reaches " +
		"the Canceller. Also note it does NOT transition job state -- the 202 body " +
		"claims cancel_requested while the stored job status is unchanged")
}

// The 202 response asserts "cancel_requested" but nothing is written to the job.
func TestRobustness_BUG_CancelDoesNotPersistAnyStateChange(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	_ = repo.Create(context.Background(), &jobs.Job{ID: "job-1", Status: jobs.StatusQueued})
	h := NewJobsHandler(repo, &rbStubQueue{}, &rbStubCanceller{}, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/job-1/cancel", nil)
	req.SetPathValue("jobID", "job-1")
	rec := httptest.NewRecorder()
	h.Cancel(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want 202", rec.Code)
	}
	got, _ := repo.Get(context.Background(), "job-1")
	if got.Status != jobs.StatusQueued {
		t.Fatalf("got status %q, want queued (documenting actual behavior)", got.Status)
	}
	t.Log("NOTE: a queued-but-not-yet-running job is not tracked by the Canceller, so " +
		"the real Canceller returns ErrJobNotRunning -> 409. A queued job therefore " +
		"cannot be cancelled at all until a worker picks it up")
}

// ---------------------------------------------------------------------------
// 6. Response contract
// ---------------------------------------------------------------------------

func TestRobustness_ContentTypeIsSetOnEverySuccessAndErrorResponse(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	_ = repo.Create(context.Background(), &jobs.Job{ID: "job-1", Status: jobs.StatusQueued})
	h := NewJobsHandler(repo, &rbStubQueue{}, &rbStubCanceller{}, nil)

	checks := []struct {
		name string
		want int
		run  func() *httptest.ResponseRecorder
	}{
		{"create 201", 201, func() *httptest.ResponseRecorder {
			return rbPostJSON(t, h, strings.NewReader(`{"task":"a valid task","repo_path":"/tmp/repo"}`), 5*time.Second)
		}},
		{"create 400", 400, func() *httptest.ResponseRecorder {
			return rbPostJSON(t, h, strings.NewReader(`{}`), 5*time.Second)
		}},
		{"get 200", 200, func() *httptest.ResponseRecorder {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/job-1", nil)
			r.SetPathValue("jobID", "job-1")
			rec := httptest.NewRecorder()
			h.Get(rec, r)
			return rec
		}},
		{"get 404", 404, func() *httptest.ResponseRecorder {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/nope", nil)
			r.SetPathValue("jobID", "nope")
			rec := httptest.NewRecorder()
			h.Get(rec, r)
			return rec
		}},
		{"list 200", 200, func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			h.List(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil))
			return rec
		}},
		{"cancel 202", 202, func() *httptest.ResponseRecorder {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/job-1/cancel", nil)
			r.SetPathValue("jobID", "job-1")
			rec := httptest.NewRecorder()
			h.Cancel(rec, r)
			return rec
		}},
		{"cancel 404", 404, func() *httptest.ResponseRecorder {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/nope/cancel", nil)
			r.SetPathValue("jobID", "nope")
			rec := httptest.NewRecorder()
			h.Cancel(rec, r)
			return rec
		}},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			rec := c.run()
			if rec.Code != c.want {
				t.Fatalf("got status %d, want %d", rec.Code, c.want)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("got Content-Type %q, want application/json", ct)
			}
			if !json.Valid(rec.Body.Bytes()) {
				t.Errorf("body is not valid JSON: %q", rec.Body.String())
			}
		})
	}
}

func TestRobustness_ErrorBodyShapeIsAlwaysASingleErrorKey(t *testing.T) {
	h, _, _, _, _ := newRobustnessHandler()
	for _, body := range []string{"", "{}", `{"task":"a"}`, `{"task":"a valid task","repo_path":"rel"}`, "garbage"} {
		rec := rbPostJSON(t, h, strings.NewReader(body), 5*time.Second)
		m := rbDecodeErrorBody(t, rec)
		if len(m) != 1 {
			t.Errorf("body %q: got %d keys, want 1", body, len(m))
		}
		if s, ok := m["error"].(string); !ok || s == "" {
			t.Errorf("body %q: got error field %v, want a non-empty string", body, m["error"])
		}
	}
}

func TestRobustness_ListReturnsEmptyArrayNotNullWhenNoJobsExist(t *testing.T) {
	h, _, _, _, _ := newRobustnessHandler()
	rec := httptest.NewRecorder()
	h.List(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil))

	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Fatalf("got %q, want []", got)
	}
}

// The API contract documents Go field names for the Job response. Pin it so a
// later `json:"..."` tag addition is a conscious, breaking change.
func TestRobustness_JobResponseUsesGoFieldNamesNotSnakeCase(t *testing.T) {
	h, _, _, _, _ := newRobustnessHandler()
	rec := rbPostJSON(t, h, strings.NewReader(`{"task":"a valid task","repo_path":"/tmp/repo"}`), 5*time.Second)

	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ID", "Task", "RepoPath", "Status", "CreatedAt", "UpdatedAt"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing field %q in %v", k, m)
		}
	}
	if _, ok := m["repo_path"]; ok {
		t.Error("unexpected snake_case field repo_path")
	}
	t.Log("NOTE: the API is asymmetric -- requests use snake_case (repo_path) but " +
		"responses use Go names (RepoPath). Documented, but a client footgun")
}

func TestRobustness_CreateGeneratesUnpredictable128BitIDs(t *testing.T) {
	h, _, _, _, _ := newRobustnessHandler()
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		rec := rbPostJSON(t, h, strings.NewReader(`{"task":"a valid task","repo_path":"/tmp/repo"}`), 5*time.Second)
		var j jobs.Job
		if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil {
			t.Fatal(err)
		}
		if len(j.ID) != 32 {
			t.Fatalf("got ID %q (len %d), want 32 hex chars", j.ID, len(j.ID))
		}
		if seen[j.ID] {
			t.Fatalf("duplicate ID %s", j.ID)
		}
		seen[j.ID] = true
	}
	t.Log("NOTE: newID() discards the crypto/rand error (internal/api/jobs.go:122). " +
		"If the entropy source ever failed it would mint the all-zero ID repeatedly " +
		"and Create would then 500 on ErrJobAlreadyExists")
}

// ---------------------------------------------------------------------------
// 7. Concurrency (meaningful under -race)
// ---------------------------------------------------------------------------

func TestRobustness_HundredConcurrentCreatesProduceUniqueJobs(t *testing.T) {
	h, repo, q, _, bus := newRobustnessHandler()

	const n = 100
	var wg sync.WaitGroup
	codes := make([]int, n)
	ids := make([]string, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("PANIC in concurrent Create: %v", p)
				}
			}()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs",
				strings.NewReader(fmt.Sprintf(`{"task":"task number %d","repo_path":"/tmp/repo"}`, i)))
			rec := httptest.NewRecorder()
			h.Create(rec, req)
			codes[i] = rec.Code
			var j jobs.Job
			_ = json.Unmarshal(rec.Body.Bytes(), &j)
			ids[i] = j.ID
		}(i)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("HANG: concurrent creates did not finish in 30s")
	}

	unique := make(map[string]bool)
	for i, c := range codes {
		if c != http.StatusCreated {
			t.Errorf("request %d: got status %d, want 201", i, c)
		}
		if ids[i] == "" {
			t.Errorf("request %d: empty ID", i)
		}
		unique[ids[i]] = true
	}
	if len(unique) != n {
		t.Errorf("got %d unique IDs, want %d", len(unique), n)
	}
	list, _ := repo.List(context.Background())
	if len(list) != n {
		t.Errorf("repository holds %d jobs, want %d", len(list), n)
	}
	if q.count() != n {
		t.Errorf("queue saw %d enqueues, want %d", q.count(), n)
	}
	if bus.count() != n {
		t.Errorf("bus saw %d events, want %d", bus.count(), n)
	}
}

func TestRobustness_ConcurrentGetListAndCancelOnSameJobDoNotRace(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	_ = repo.Create(context.Background(), &jobs.Job{ID: "shared", Task: "t",
		RepoPath: "/tmp/r", Status: jobs.StatusQueued})
	h := NewJobsHandler(repo, &rbStubQueue{}, &rbStubCanceller{}, &rbStubBus{})

	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/shared", nil)
			r.SetPathValue("jobID", "shared")
			rec := httptest.NewRecorder()
			h.Get(rec, r)
			if rec.Code != http.StatusOK {
				t.Errorf("Get: got status %d, want 200", rec.Code)
			}
		}()
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/shared/cancel", nil)
			r.SetPathValue("jobID", "shared")
			rec := httptest.NewRecorder()
			h.Cancel(rec, r)
			if rec.Code != http.StatusAccepted {
				t.Errorf("Cancel: got status %d, want 202", rec.Code)
			}
		}()
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.List(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil))
			if rec.Code != http.StatusOK {
				t.Errorf("List: got status %d, want 200", rec.Code)
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("HANG: concurrent Get/Cancel/List did not finish in 30s")
	}
}

// The handler chain has no panic-recovery middleware, so a panic in any dependency
// (here the event bus) escapes the handler. net/http recovers per connection, but
// the client gets a truncated/empty response and the request is lost.
func TestRobustness_BUG_NoPanicRecoveryMiddlewareInTheChain(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	h := NewJobsHandler(repo, &rbStubQueue{}, &rbStubCanceller{}, &rbStubBus{panicNow: true})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs",
		strings.NewReader(`{"task":"a valid task","repo_path":"/tmp/repo"}`))

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		h.Create(rec, req)
	}()

	if recovered == nil {
		t.Fatal("expected the panic to escape the handler")
	}
	// The job was already written before the panic: partial side effects survive.
	list, _ := repo.List(context.Background())
	if len(list) != 1 {
		t.Fatalf("got %d jobs, want 1", len(list))
	}
	t.Log("CONFIRMED GAP: cmd/api/main.go composes TokenAuth -> RateLimiter -> " +
		"LoggerMiddleware -> mux with no recovery middleware. A panic anywhere in a " +
		"handler drops the connection with no response body and no structured log line")
}
