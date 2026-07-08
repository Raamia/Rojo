package api

// Security audit tests for the HTTP surface.
//
// Tests ASSERT CURRENT BEHAVIOUR. Names ending in _DocumentsGap pin a
// vulnerable behaviour so the suite stays green while `go test -v` prints the
// gap; when the gap is fixed those tests fail loudly and must be inverted.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
)

// ---------------------------------------------------------------------------
// Harness: reproduce the exact middleware composition from cmd/api/main.go:53-81
//   mux -> LoggerMiddleware -> RateLimiter -> TokenAuth (outermost)
// so ordering bugs are exercised the way production sees them.
// ---------------------------------------------------------------------------

type secAuditChain struct {
	handler http.Handler
	repo    *jobs.InMemoryRepository
	bus     *events.InProcessBus
	limiter *RateLimiter
}

type secAuditChainOpts struct {
	token          string
	rlBurst        int
	rlRefillPerSec float64
	cancelErr      error
}

type secErroringCanceller struct{ err error }

func (c secErroringCanceller) Cancel(string) error { return c.err }

func newSecAuditChain(t *testing.T, opts secAuditChainOpts) secAuditChain {
	t.Helper()
	if opts.rlBurst == 0 {
		opts.rlBurst = 1_000_000 // effectively disabled unless a test asks for it
		opts.rlRefillPerSec = 1_000_000
	}

	repo := jobs.NewInMemoryRepository()
	bus := events.NewInProcessBus()
	var canceller Canceller = noopCanceller{}
	if opts.cancelErr != nil {
		canceller = secErroringCanceller{err: opts.cancelErr}
	}
	h := NewJobsHandler(repo, noopQueue{}, canceller, bus)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /api/v1/jobs", h.Create)
	mux.HandleFunc("GET /api/v1/jobs", h.List)
	mux.HandleFunc("GET /api/v1/jobs/{jobID}", h.Get)
	mux.HandleFunc("POST /api/v1/jobs/{jobID}/cancel", h.Cancel)
	mux.HandleFunc("GET /api/v1/jobs/{jobID}/stream", NewStreamHandler(bus).Stream)

	limiter := NewRateLimiter(opts.rlBurst, opts.rlRefillPerSec)

	var chain http.Handler = mux
	chain = LoggerMiddleware(slog.New(slog.NewTextHandler(io.Discard, nil)))(chain)
	chain = limiter.Middleware()(chain)
	if opts.token != "" {
		chain = TokenAuth(opts.token)(chain)
	}
	return secAuditChain{handler: chain, repo: repo, bus: bus, limiter: limiter}
}

func (c secAuditChain) do(t *testing.T, method, target, token, remoteAddr string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	return rec
}

func (c secAuditChain) createJob(t *testing.T, token, remoteAddr, task, repoPath string) jobs.Job {
	t.Helper()
	body := fmt.Sprintf(`{"task":%q,"repo_path":%q}`, task, repoPath)
	rec := c.do(t, http.MethodPost, "/api/v1/jobs", token, remoteAddr, strings.NewReader(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create job: status %d body %s", rec.Code, rec.Body.String())
	}
	var j jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil {
		t.Fatalf("unmarshal job: %v", err)
	}
	return j
}

// ===========================================================================
// 1. AuthN / AuthZ
// ===========================================================================

// CRITICAL: ROJO_AUTH_TOKEN is optional (config.go:31, no Validate rule) and
// main.go:73 only installs TokenAuth when it is non-empty. A default
// deployment therefore serves the whole API with no credentials at all, and
// startup logs no warning about it.
func TestSecurity_NoTokenConfigured_EntireAPIIsUnauthenticated_DocumentsGap(t *testing.T) {
	c := newSecAuditChain(t, secAuditChainOpts{token: ""}) // mirrors ROJO_AUTH_TOKEN unset

	job := c.createJob(t, "", "203.0.113.1:1", "seeded task", "/tmp/repo")

	for _, tc := range []struct{ method, target string }{
		{http.MethodPost, "/api/v1/jobs"},
		{http.MethodGet, "/api/v1/jobs"},
		{http.MethodGet, "/api/v1/jobs/" + job.ID},
		{http.MethodPost, "/api/v1/jobs/" + job.ID + "/cancel"},
	} {
		var body io.Reader
		if tc.method == http.MethodPost && tc.target == "/api/v1/jobs" {
			body = strings.NewReader(`{"task":"anonymous task","repo_path":"/tmp/repo"}`)
		}
		rec := c.do(t, tc.method, tc.target, "", "198.51.100.66:9", body)
		if rec.Code == http.StatusUnauthorized {
			t.Fatalf("VULNERABILITY FIXED? %s %s now requires auth", tc.method, tc.target)
		}
		t.Logf("UNAUTHENTICATED %s %s -> %d", tc.method, tc.target, rec.Code)
	}
	t.Log("GAP: with ROJO_AUTH_TOKEN unset the API is fully open; config.Validate() does not require it")
}

// HIGH: there is no per-job authorization anywhere. jobs.Job (internal/jobs/job.go:8)
// has no owner/tenant field, and Get/Cancel/List/stream/events never consult
// caller identity — a single shared bearer token grants full access to every
// job created by every caller.
func TestSecurity_AnyCallerCanReadCancelAndListAnyJob_DocumentsMissingAuthz(t *testing.T) {
	const sharedToken = "shared-deployment-token"
	c := newSecAuditChain(t, secAuditChainOpts{token: sharedToken})

	// "alice" creates a job referencing a private path.
	alice := c.createJob(t, sharedToken, "203.0.113.10:1000",
		"refactor the billing service", "/Users/alice/private-monorepo")

	// "bob" — different IP, same shared token — reads it.
	rec := c.do(t, http.MethodGet, "/api/v1/jobs/"+alice.ID, sharedToken, "198.51.100.77:2000", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("VULNERABILITY FIXED? cross-caller read returned %d", rec.Code)
	}
	var leaked jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &leaked); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if leaked.RepoPath != "/Users/alice/private-monorepo" {
		t.Fatalf("unexpected body: %+v", leaked)
	}
	t.Logf("PROVEN: caller B read caller A's job, leaking task %q and host path %q",
		leaked.Task, leaked.RepoPath)

	// bob can cancel it too.
	rec = c.do(t, http.MethodPost, "/api/v1/jobs/"+alice.ID+"/cancel", sharedToken, "198.51.100.77:2000", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("VULNERABILITY FIXED? cross-caller cancel returned %d", rec.Code)
	}
	t.Log("PROVEN: caller B cancelled caller A's job (202 Accepted)")

	// And enumerate every job on the instance.
	rec = c.do(t, http.MethodGet, "/api/v1/jobs", sharedToken, "198.51.100.77:2000", nil)
	var all []jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected alice's job in the list, got %d", len(all))
	}
	t.Logf("PROVEN: GET /api/v1/jobs returns every caller's jobs (%d here), including repo_path", len(all))
}

// The WebSocket stream inherits the same gap: no per-job check, so any caller
// with the shared token can tail any job's event stream in real time.
func TestSecurity_AnyCallerCanStreamAnyJob_DocumentsMissingAuthz(t *testing.T) {
	c := newSecAuditChain(t, secAuditChainOpts{token: "shared-token"})
	srv := httptest.NewServer(c.handler)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	victimJobID := "victims-job-id"
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/jobs/" + victimJobID + "/stream"

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer shared-token"}},
	})
	if err != nil {
		t.Fatalf("VULNERABILITY FIXED? stream dial rejected: %v", err)
	}
	defer conn.CloseNow() // CloseNow: the server never reads, so a close handshake would stall 5s

	go func() {
		for i := 0; i < 40; i++ {
			_ = c.bus.Publish(context.Background(), events.Event{
				JobID:   victimJobID,
				Type:    events.TypeStepStarted,
				Payload: map[string]any{"status": "implementing", "secret_ish": "victim internals"},
			})
			time.Sleep(10 * time.Millisecond)
		}
	}()

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	t.Logf("PROVEN: a caller that never created job %q received its events: %s", victimJobID, data)
}

// TokenAuth exempts exactly one path string (auth.go:12). These are the
// classic bypass shapes. Result: no protected route is reachable — the exact
// == comparison plus Go's ServeMux matching on the same decoded r.URL.Path
// holds. Documented so a future switch to prefix matching is caught.
func TestSecurity_TokenAuth_PathBypassAttempts(t *testing.T) {
	c := newSecAuditChain(t, secAuditChainOpts{token: "s3cr3t"})
	job := c.createJob(t, "s3cr3t", "203.0.113.1:1", "seed task", "/tmp/repo")

	cases := []string{
		"/healthz",
		"/healthz/",
		"//healthz",
		"/healthz//",
		"/HEALTHZ",
		"/healthz?x=1",
		"/%68ealthz",
		"/healthz%2f",
		"/healthz/../api/v1/jobs",
		"/healthz/..;/api/v1/jobs",
		"/api/v1/jobs/../healthz",
		"/./healthz",
		"/healthz ",
		"/healthz\t",
		"/api/v1/jobs",
		"/api/v1/jobs/" + job.ID,
		"/api/v1/jobs%2f" + job.ID,
		"/./api/v1/jobs",
		"//api/v1/jobs",
		"/api/v1//jobs",
		"/API/V1/JOBS",
	}

	var bypassedProtected []string
	for _, target := range cases {
		req, err := http.NewRequest(http.MethodGet, "http://example.test"+target, nil)
		if err != nil {
			t.Logf("%-32q unparseable: %v", target, err)
			continue
		}
		req.RemoteAddr = "203.0.113.9:1234"
		rec := httptest.NewRecorder()
		c.handler.ServeHTTP(rec, req) // NO Authorization header

		body := strings.TrimSpace(rec.Body.String())
		if len(body) > 60 {
			body = body[:60] + "..."
		}
		t.Logf("unauthenticated %-34q -> %d %s", target, rec.Code, body)

		// A bypass = a 2xx from a route other than the intentionally-exempt
		// /healthz. Compare on the DECODED path, which is what both TokenAuth
		// and ServeMux match on.
		isHealth := req.URL.Path == "/healthz"
		if !isHealth && rec.Code >= 200 && rec.Code < 300 {
			bypassedProtected = append(bypassedProtected, target)
		}
	}

	if len(bypassedProtected) > 0 {
		t.Fatalf("AUTH BYPASS FOUND for %v", bypassedProtected)
	}
	t.Log("RESULT: no path trick reached a protected route unauthenticated; " +
		"the /healthz exemption is an exact r.URL.Path == comparison and ServeMux " +
		"matches on the same decoded path. Note /%68ealthz and /healthz?x=1 do hit " +
		"the exempt branch, but only the public health endpoint.")
}

// TokenAuth has no guard against an empty expected token: subtle.ConstantTimeCompare
// of two zero-length slices returns 1, so TokenAuth("") authenticates
// "Authorization: Bearer " (note the trailing space). main.go currently avoids
// this by skipping the middleware entirely, but the primitive fails open.
func TestSecurity_TokenAuth_EmptyExpectedTokenFailsOpen_DocumentsGap(t *testing.T) {
	var reached bool
	h := TokenAuth("")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !reached || rec.Code != http.StatusOK {
		t.Fatalf("VULNERABILITY FIXED? empty configured token now rejects (status %d, reached %v)", rec.Code, reached)
	}
	t.Log("GAP: TokenAuth(\"\") accepts `Authorization: Bearer ` — no guard against a blank configured token")
}

// Header parsing is strict (good news, pinned): scheme is case-sensitive and
// no whitespace normalisation happens.
func TestSecurity_TokenAuth_HeaderParsingIsStrict(t *testing.T) {
	const token = "s3cr3t"
	h := TokenAuth(token)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, header := range []string{
		"bearer s3cr3t",
		"BEARER s3cr3t",
		"Bearer  s3cr3t",
		"Bearer s3cr3t ",
		"Bearer\ts3cr3t",
		"s3cr3t",
		"Basic czNjcjN0",
		"Bearer s3cr3",
		"Bearer s3cr3tX",
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
		req.Header.Set("Authorization", header)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("header %q got %d, want 401", header, rec.Code)
		}
	}
	t.Log("RESULT: Bearer parsing is strict and uses constant-time comparison")
}

// ===========================================================================
// 2. Request limits / DoS
// ===========================================================================

// HIGH: JobsHandler.Create (jobs.go:36) decodes straight from r.Body with no
// http.MaxBytesReader. The task-length validation (4000 chars) runs only AFTER
// the whole body has been read and parsed into memory.
func TestSecurity_CreateJob_RequestBodyIsSizeLimited(t *testing.T) {
	c := newSecAuditChain(t, secAuditChainOpts{token: "tok"})

	const padBytes = 16 << 20 // 16 MiB of ignored JSON; nothing caps this
	counted := &secCountingReader{
		r: io.MultiReader(
			strings.NewReader(`{"task":"a legitimate short task","repo_path":"/tmp/repo","pad":"`),
			&secRepeatReader{b: 'A', n: padBytes},
			strings.NewReader(`"}`),
		),
	}

	rec := c.do(t, http.MethodPost, "/api/v1/jobs", "tok", "203.0.113.5:1", counted)

	// FIXED: http.MaxBytesReader caps the body, so the server rejects with 413
	// and — critically — stops reading long before the 16 MiB is buffered.
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413: %s", rec.Code, rec.Body.String())
	}
	if counted.n > MaxRequestBodyBytes+(1<<16) {
		t.Fatalf("server read %d bytes; it should stop near the %d-byte cap", counted.n, MaxRequestBodyBytes)
	}
}

// Even a body that will be REJECTED is fully read first: validation happens
// after decoding, so an attacker pays nothing to make the server allocate.
func TestSecurity_CreateJob_OversizedBodyRejectedBeforeBuffering(t *testing.T) {
	c := newSecAuditChain(t, secAuditChainOpts{token: "tok"})

	const padBytes = 8 << 20
	counted := &secCountingReader{
		r: io.MultiReader(
			strings.NewReader(`{"task":"`),
			&secRepeatReader{b: 'B', n: padBytes},
			strings.NewReader(`","repo_path":"/tmp/repo"}`),
		),
	}
	rec := c.do(t, http.MethodPost, "/api/v1/jobs", "tok", "203.0.113.5:1", counted)

	// FIXED: an oversized body is refused at the transport layer, so the server
	// no longer materialises megabytes into a Go string just to reject them.
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413", rec.Code)
	}
	if counted.n > MaxRequestBodyBytes+(1<<16) {
		t.Fatalf("server read %d bytes before rejecting; want it to stop near %d", counted.n, MaxRequestBodyBytes)
	}
}

// FIXED: List used to serialise every job in storage into one response, with
// ?limit silently ignored — a free way for any client to make the server
// allocate, growing without bound as the service is used.
func TestSecurity_ListJobs_IsPaginated(t *testing.T) {
	c := newSecAuditChain(t, secAuditChainOpts{token: "tok"})

	const n = 2000
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if err := c.repo.Create(ctx, &jobs.Job{
			ID:       fmt.Sprintf("job-%04d", i),
			Task:     strings.Repeat("x", 400),
			RepoPath: "/srv/repos/private",
			Status:   jobs.StatusQueued,
		}); err != nil {
			t.Fatal(err)
		}
	}

	rec := c.do(t, http.MethodGet, "/api/v1/jobs?limit=10", "tok", "203.0.113.5:1", nil)
	var got []jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("got %d jobs for ?limit=10, want 10 of %d", len(got), n)
	}

	// And a client that asks for nothing in particular is still bounded.
	rec = c.do(t, http.MethodGet, "/api/v1/jobs", "tok", "203.0.113.5:1", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) > MaxListLimit {
		t.Fatalf("an unparameterised request returned %d of %d jobs", len(got), n)
	}
}

// HIGH: cmd/api/main.go:77-81 sets only ReadHeaderTimeout. ReadTimeout,
// WriteTimeout and IdleTimeout are all zero (= no limit), so a client can send
// complete headers and then dribble the body forever, holding a goroutine and
// a connection each. This test reproduces main.go's exact server config.
func TestSecurity_HTTPServer_NoReadTimeout_SlowBodyHoldsConnection_DocumentsGap(t *testing.T) {
	c := newSecAuditChain(t, secAuditChainOpts{token: "tok"})

	srv := httptest.NewUnstartedServer(c.handler)
	// Exactly what cmd/api/main.go builds.
	srv.Config.ReadHeaderTimeout = 5 * time.Second
	srv.Config.ReadTimeout = 0
	srv.Config.WriteTimeout = 0
	srv.Config.IdleTimeout = 0
	srv.Start()
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Complete headers (so ReadHeaderTimeout is satisfied), then an incomplete body.
	head := "POST /api/v1/jobs HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Authorization: Bearer tok\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: 100000\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(head)); err != nil {
		t.Fatalf("write head: %v", err)
	}

	const dribbleFor = 1200 * time.Millisecond
	deadline := time.Now().Add(dribbleFor)
	for time.Now().Before(deadline) {
		if _, err := conn.Write([]byte("a")); err != nil {
			t.Fatalf("VULNERABILITY FIXED? server closed the slow connection after %v: %v",
				dribbleFor-time.Until(deadline), err)
		}
		time.Sleep(150 * time.Millisecond)
	}

	// The server must not have responded or hung up yet.
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 64)
	n, readErr := conn.Read(buf)
	var netErr net.Error
	if errors.As(readErr, &netErr) && netErr.Timeout() {
		t.Logf("PROVEN: connection still open and the server is still waiting for the body after %v "+
			"of 1-byte-per-150ms dribbling. With ReadTimeout=0 and IdleTimeout=0 this holds a goroutine "+
			"+ conn indefinitely; N such sockets = N goroutines. Slowloris is unmitigated.", dribbleFor)
		return
	}
	t.Fatalf("VULNERABILITY FIXED? server responded/closed early: n=%d err=%v body=%q", n, readErr, buf[:n])
}

// ===========================================================================
// 3. Rate limiter
// ===========================================================================

// HIGH: clientKey (ratelimit.go:78-94) trusts X-Forwarded-For unconditionally.
// There is no trusted-proxy list, and main.go binds the server directly (no
// reverse proxy is assumed), so any client can mint a fresh bucket per request.
func TestSecurity_RateLimiter_XForwardedForSpoofBypassesLimit_DocumentsGap(t *testing.T) {
	const burst = 3
	rl := NewRateLimiter(burst, 0) // no refill: burst is the hard cap per key
	h := rl.Middleware()(okHandler())

	const attempts = 500
	allowed, limited := 0, 0
	for i := 0; i < attempts; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
		req.RemoteAddr = "203.0.113.66:5555" // ONE real attacker IP for all of them
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.%d.%d.%d", i/65536%256, i/256%256, i%256))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		switch rec.Code {
		case http.StatusOK:
			allowed++
		case http.StatusTooManyRequests:
			limited++
		}
	}

	if allowed != attempts {
		t.Fatalf("VULNERABILITY FIXED? only %d/%d spoofed requests allowed (%d limited)", allowed, attempts, limited)
	}

	rl.mu.Lock()
	buckets := len(rl.buckets)
	rl.mu.Unlock()

	t.Logf("PROVEN: one source IP sent %d requests with spoofed X-Forwarded-For; ALL %d were allowed "+
		"against a burst of %d (%.0fx the limit). The limiter is fully bypassable by any client.",
		attempts, allowed, burst, float64(allowed)/float64(burst))
	t.Logf("PROVEN MEMORY GROWTH: rl.buckets now holds %d entries — one per forged IP. "+
		"There is no TTL, no eviction and no cap, so buckets grows unbounded for the process lifetime.", buckets)
	if buckets != attempts {
		t.Fatalf("expected %d buckets, got %d", attempts, buckets)
	}
}

// MEDIUM: even without spoofing, buckets are never reclaimed. One entry per
// distinct client IP, retained forever.
func TestSecurity_RateLimiter_BucketsNeverEvicted_DocumentsGap(t *testing.T) {
	rl := NewRateLimiter(10, 100) // fast refill: buckets go idle immediately
	h := rl.Middleware()(okHandler())

	for i := 0; i < 1000; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
		req.RemoteAddr = fmt.Sprintf("192.0.2.%d:%d", i%256, 1000+i)
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("172.16.%d.%d", i/256, i%256))
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	time.Sleep(20 * time.Millisecond) // every bucket is now full/idle

	rl.mu.Lock()
	n := len(rl.buckets)
	rl.mu.Unlock()

	if n != 1000 {
		t.Fatalf("VULNERABILITY FIXED? %d buckets retained, expected 1000", n)
	}
	t.Logf("PROVEN: %d fully-refilled (idle) buckets still resident. RateLimiter has no janitor "+
		"goroutine and no max size; memory is monotonically increasing.", n)
}

// HIGH: middleware order in main.go:70-75 puts TokenAuth OUTSIDE the rate
// limiter, so requests that fail authentication never reach it. Token
// guessing is therefore completely unthrottled.
func TestSecurity_UnauthenticatedRequestsBypassRateLimiter_DocumentsGap(t *testing.T) {
	c := newSecAuditChain(t, secAuditChainOpts{token: "the-real-token", rlBurst: 2, rlRefillPerSec: 0})

	const attempts = 300
	unauthorized, throttled := 0, 0
	for i := 0; i < attempts; i++ {
		rec := c.do(t, http.MethodGet, "/api/v1/jobs", fmt.Sprintf("guess-%d", i), "203.0.113.99:4444", nil)
		switch rec.Code {
		case http.StatusUnauthorized:
			unauthorized++
		case http.StatusTooManyRequests:
			throttled++
		}
	}

	if throttled != 0 {
		t.Fatalf("VULNERABILITY FIXED? %d/%d guesses were rate limited", throttled, attempts)
	}
	if unauthorized != attempts {
		t.Fatalf("unexpected mix: %d unauthorized, %d throttled", unauthorized, throttled)
	}

	c.limiter.mu.Lock()
	buckets := len(c.limiter.buckets)
	c.limiter.mu.Unlock()

	t.Logf("PROVEN: %d wrong-token attempts from one IP, ALL returned 401 and ZERO were rate limited "+
		"(burst was 2, refill 0). The limiter recorded %d buckets — it never saw the traffic because "+
		"TokenAuth is the outermost middleware.", attempts, buckets)
	t.Log("IMPACT: unlimited offline-speed token brute force, and unauthenticated request flooding " +
		"costs the attacker nothing.")
}

// ===========================================================================
// 5. WebSocket / CSWSH
// ===========================================================================

// stream.go:36 sets InsecureSkipVerify: true, which disables coder/websocket's
// Origin check entirely. Proven here: a handshake claiming an attacker Origin
// is accepted.
func TestSecurity_WebSocket_AcceptsAnyOrigin_CSWSH_DocumentsGap(t *testing.T) {
	// Unauthenticated deployment (the default, see finding 1) — this is the
	// case where CSWSH is directly exploitable from a victim's browser.
	c := newSecAuditChain(t, secAuditChainOpts{token: ""})
	srv := httptest.NewServer(c.handler)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	jobID := "target-job"
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/jobs/" + jobID + "/stream"

	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://attacker.example"}},
	})
	if err != nil {
		t.Fatalf("VULNERABILITY FIXED? cross-origin handshake rejected: %v", err)
	}
	defer conn.CloseNow() // CloseNow: the server never reads, so a close handshake would stall 5s
	if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("unexpected handshake status %d", resp.StatusCode)
	}

	go func() {
		for i := 0; i < 40; i++ {
			_ = c.bus.Publish(context.Background(), events.Event{JobID: jobID, Type: events.TypeJobStarted})
			time.Sleep(10 * time.Millisecond)
		}
	}()
	if _, data, err := conn.Read(ctx); err != nil {
		t.Fatalf("read: %v", err)
	} else {
		t.Logf("PROVEN CSWSH: handshake with Origin: https://attacker.example accepted and streamed %s", data)
	}
	t.Log("IMPACT: any web page the operator visits can open ws://<rojo-host>/api/v1/jobs/<id>/stream " +
		"and read job events (task text, repo paths, step status) cross-origin. Also enables " +
		"DNS-rebinding against a localhost deployment.")
}

// With a token configured the CSWSH impact is limited, but only incidentally:
// browsers cannot set an Authorization header on a WebSocket handshake, so the
// same attack yields 401 — and legitimate browser clients cannot connect
// either. The endpoint has no cookie/subprotocol/query auth path.
func TestSecurity_WebSocket_BrowserCannotAuthenticate(t *testing.T) {
	c := newSecAuditChain(t, secAuditChainOpts{token: "tok"})
	srv := httptest.NewServer(c.handler)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/jobs/j1/stream"

	// What a browser can do: set Origin, but not Authorization.
	_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://attacker.example"}},
	})
	if err == nil {
		t.Fatal("VULNERABILITY: cross-origin unauthenticated WebSocket succeeded with a token configured")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got resp=%v err=%v", resp, err)
	}
	t.Log("RESULT: with ROJO_AUTH_TOKEN set, browser-origin CSWSH gets 401 — but only because the " +
		"bearer token is not an ambient credential. Origin verification is still OFF, and there is no " +
		"supported way for a browser to authenticate to /stream (no subprotocol/ticket/cookie support).")
}

// The subscriber buffer is bounded (stream.go:16) and InProcessBus drops on a
// full channel, so a slow WebSocket client cannot block job execution. Pinned
// as a control that holds.
func TestSecurity_SlowWebSocketSubscriberDoesNotBlockPublisher(t *testing.T) {
	bus := events.NewInProcessBus()
	sub := bus.Subscribe("j", 4)
	defer bus.Unsubscribe(sub)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10_000; i++ { // never drained
			_ = bus.Publish(context.Background(), events.Event{JobID: "j", Type: events.TypeStepStarted})
		}
	}()
	select {
	case <-done:
		t.Log("RESULT: publisher never blocks on a full subscriber buffer (events are dropped instead)")
	case <-time.After(5 * time.Second):
		t.Fatal("publisher blocked on a slow subscriber")
	}
}

// ===========================================================================
// 6. Secrets in logs
// ===========================================================================

// LoggerMiddleware logs only request_id/method/path/status/duration
// (middleware.go:19-40). Verified: no Authorization header, no query string.
func TestSecurity_LoggerMiddleware_DoesNotLogCredentials(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	h := LoggerMiddleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	const secret = "SUPER-SECRET-TOKEN-VALUE"
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs?access_token="+secret+"&db=postgres://u:p@h/db", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Cookie", "session="+secret)
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	for _, needle := range []string{secret, "postgres://", "Bearer", "Cookie"} {
		if strings.Contains(out, needle) {
			t.Errorf("LEAK: request log contains %q\nlog line: %s", needle, out)
		}
	}
	t.Logf("RESULT: no credential material in the access log. Logged line: %s", strings.TrimSpace(out))
	t.Log("NOTE: only r.URL.Path is logged, so a secret placed in a query string would not leak here — " +
		"but nothing enforces that, and job IDs are logged in the path.")
}

// ===========================================================================
// Misc hardening gaps (Low)
// ===========================================================================

// Cancel forwards the Canceller's raw error text to the client (jobs.go:102).
func TestSecurity_CancelHandler_LeaksInternalErrorText_DocumentsGap(t *testing.T) {
	internal := errors.New("job f00d is not tracked by worker pool node-3 (pid 8123)")
	c := newSecAuditChain(t, secAuditChainOpts{token: "tok", cancelErr: internal})
	job := c.createJob(t, "tok", "203.0.113.1:1", "seed task", "/tmp/repo")

	rec := c.do(t, http.MethodPost, "/api/v1/jobs/"+job.ID+"/cancel", "tok", "203.0.113.1:1", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "node-3") {
		t.Fatalf("VULNERABILITY FIXED? internal detail no longer echoed: %s", rec.Body.String())
	}
	t.Logf("GAP: internal error string echoed verbatim to the client: %s", strings.TrimSpace(rec.Body.String()))
}

// No baseline security response headers are set (response.go:13-22).
func TestSecurity_ResponsesMissingSecurityHeaders_DocumentsGap(t *testing.T) {
	c := newSecAuditChain(t, secAuditChainOpts{token: "tok"})
	rec := c.do(t, http.MethodGet, "/api/v1/jobs", "tok", "203.0.113.1:1", nil)

	var missing []string
	for _, h := range []string{
		"X-Content-Type-Options",
		"Cache-Control",
		"Strict-Transport-Security",
		"Referrer-Policy",
	} {
		if rec.Header().Get(h) == "" {
			missing = append(missing, h)
		}
	}
	if len(missing) == 0 {
		t.Fatal("VULNERABILITY FIXED? security headers are now set")
	}
	t.Logf("GAP: job responses omit %v (job data is cacheable by intermediaries by default)", missing)
}

// The API accepts any absolute filesystem path as repo_path
// (jobs/request.go:42) — no allowlist, no root confinement. Once the
// workspace/execution layer is wired into the orchestrator this becomes host
// filesystem + git-hook reach; see workspace/repo_security_test.go.
func TestSecurity_CreateJob_ArbitraryAbsoluteRepoPathAccepted_DocumentsGap(t *testing.T) {
	c := newSecAuditChain(t, secAuditChainOpts{token: "tok"})
	for _, p := range []string{
		"/etc",
		"/root",
		"/Users/someone-else/private-repo",
		"/var/lib/rojo/../../etc",
		"/proc/self/cwd",
	} {
		body := fmt.Sprintf(`{"task":"a valid task string","repo_path":%q}`, p)
		rec := c.do(t, http.MethodPost, "/api/v1/jobs", "tok", "203.0.113.1:1", strings.NewReader(body))
		if rec.Code != http.StatusCreated {
			t.Fatalf("VULNERABILITY FIXED? repo_path %q rejected with %d", p, rec.Code)
		}
		t.Logf("ACCEPTED repo_path=%q -> 201 Created", p)
	}
	t.Log("GAP: Validate() only checks filepath.IsAbs. No allowlist of permitted repositories, " +
		"no confinement to a configured root, and \"..\" segments are not rejected.")
}

// ---------------------------------------------------------------------------
// test helpers
// ---------------------------------------------------------------------------

type secCountingReader struct {
	r io.Reader
	n int64
}

func (c *secCountingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

type secRepeatReader struct {
	b byte
	n int
}

func (r *secRepeatReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.n {
		n = r.n
	}
	for i := 0; i < n; i++ {
		p[i] = r.b
	}
	r.n -= n
	return n, nil
}

// ===========================================================================
// WebSocket lifecycle (availability)
// ===========================================================================

// REGRESSION GUARD. The connection is hijacked, so r.Context() is never
// cancelled by the http server; if StreamHandler stops reading the socket
// (conn.CloseRead) it can no longer observe a vanished client, and every
// dropped connection parks a goroutine plus a live *events.Subscription in
// InProcessBus.subs forever. That is a remote memory-exhaustion primitive,
// reachable unauthenticated whenever ROJO_AUTH_TOKEN is unset.
//
// This test measured a 50-goroutine leak for 25 clients before stream.go
// adopted conn.CloseRead; it now measures 0. Keep it strict.
func TestSecurity_StreamHandler_DoesNotLeakGoroutinePerDisconnectedClient(t *testing.T) {
	c := newSecAuditChain(t, secAuditChainOpts{token: ""})
	srv := httptest.NewServer(c.handler)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	dialAndDrop := func(jobID string) {
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+
			"/api/v1/jobs/"+jobID+"/stream", nil)
		if err != nil {
			t.Fatalf("dial %s: %v", jobID, err)
		}
		_ = conn.CloseNow() // abrupt client disconnect, like a closed browser tab
	}

	// Warm up so one-off runtime goroutines are not counted.
	dialAndDrop("warmup")
	settleGoroutines()
	before := runtime.NumGoroutine()

	const clients = 25
	for i := 0; i < clients; i++ {
		dialAndDrop(fmt.Sprintf("idle-job-%d", i))
	}
	settleGoroutines()
	leaked := runtime.NumGoroutine() - before

	t.Logf("goroutines: before=%d after=%d delta=%d for %d connect/disconnect cycles",
		before, runtime.NumGoroutine(), leaked, clients)

	if leaked >= clients/2 {
		t.Fatalf("REGRESSION — LEAK: ~%d goroutines survived %d disconnected clients. Each also "+
			"retains a *events.Subscription in InProcessBus.subs, so an attacker can open and drop "+
			"/stream connections in a loop to exhaust memory. Restore conn.CloseRead (or add a read "+
			"loop / ping deadline) in StreamHandler.Stream.", leaked, clients)
	}
	t.Logf("RESULT: no leak (delta=%d). conn.CloseRead in StreamHandler.Stream cancels the handler "+
		"context on client disconnect. Note there is still no read deadline or ping/pong keepalive, "+
		"so a client that goes silent without closing the TCP connection is not detected.", leaked)
}

func settleGoroutines() {
	for i := 0; i < 40; i++ {
		runtime.Gosched()
		time.Sleep(25 * time.Millisecond)
	}
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
}
