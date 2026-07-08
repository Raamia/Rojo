package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/jobs"
)

// Route-level tests. The mux below mirrors the registrations in cmd/api/main.go so
// that ServeMux behavior (method matching, path cleaning, wildcard decoding) is
// exercised the same way it is in production.

func newRobustnessMux(t *testing.T) (http.Handler, *jobs.InMemoryRepository) {
	t.Helper()
	repo := jobs.NewInMemoryRepository()
	h := NewJobsHandler(repo, &rbStubQueue{}, &rbStubCanceller{}, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /api/v1/jobs", h.Create)
	mux.HandleFunc("GET /api/v1/jobs", h.List)
	mux.HandleFunc("GET /api/v1/jobs/{jobID}", h.Get)
	mux.HandleFunc("POST /api/v1/jobs/{jobID}/cancel", h.Cancel)
	return mux, repo
}

// rbServe runs one request through the mux, guarding against panics and hangs.
func rbServe(t *testing.T, mux http.Handler, method, target string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()

	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		mux.ServeHTTP(rec, r)
	}()

	select {
	case p := <-done:
		if p != nil {
			t.Fatalf("PANIC serving %s %s: %v", method, target, p)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("HANG serving %s %s", method, target)
	}
	return rec
}

// ---------------------------------------------------------------------------
// 1. Method mismatches
// ---------------------------------------------------------------------------

func TestRobustness_MethodMismatchReturns405(t *testing.T) {
	mux, _ := newRobustnessMux(t)
	cases := []struct {
		method, target string
	}{
		{http.MethodDelete, "/api/v1/jobs"},
		{http.MethodPut, "/api/v1/jobs"},
		{http.MethodPatch, "/api/v1/jobs"},
		{http.MethodPost, "/api/v1/jobs/abc"},   // POST on a GET-only route
		{http.MethodDelete, "/api/v1/jobs/abc"}, // no delete endpoint exists
		{http.MethodGet, "/api/v1/jobs/abc/cancel"},
		{http.MethodPost, "/healthz"},
		{http.MethodOptions, "/api/v1/jobs"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			rec := rbServe(t, mux, tc.method, tc.target, "")
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("got status %d, want 405", rec.Code)
			}
			if allow := rec.Header().Get("Allow"); allow == "" {
				t.Error("405 response has no Allow header")
			}
		})
	}
}

// The API contract lists the status codes the API returns. 405 is not among
// them, and unlike every handler-produced response its body is plain text.
func TestRobustness_BUG_405And404FromMuxAreNotJSONAndAreUndocumented(t *testing.T) {
	mux, _ := newRobustnessMux(t)

	t.Run("405 body", func(t *testing.T) {
		rec := rbServe(t, mux, http.MethodDelete, "/api/v1/jobs", "")
		if json.Valid(rec.Body.Bytes()) {
			t.Fatalf("expected non-JSON body, got %q", rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Fatalf("got Content-Type %q, want text/plain...", ct)
		}
		t.Logf("405 body=%q content-type=%q", strings.TrimSpace(rec.Body.String()),
			rec.Header().Get("Content-Type"))
	})

	t.Run("404 body for unknown path", func(t *testing.T) {
		rec := rbServe(t, mux, http.MethodGet, "/api/v1/nope", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("got status %d, want 404", rec.Code)
		}
		if json.Valid(rec.Body.Bytes()) {
			t.Fatalf("expected non-JSON body, got %q", rec.Body.String())
		}
		t.Logf("404 body=%q content-type=%q", strings.TrimSpace(rec.Body.String()),
			rec.Header().Get("Content-Type"))
	})

	t.Log("CONFIRMED GAP: a JSON API returns text/plain for 404/405. A client that " +
		"unconditionally parses the error envelope {\"error\":\"...\"} breaks on any " +
		"typo'd URL or wrong verb. 405 is also absent from the documented API contract")
}

// http.ServeMux treats a registered GET pattern as also matching HEAD.
func TestRobustness_HEADIsImplicitlyRoutedToGETHandlers(t *testing.T) {
	mux, repo := newRobustnessMux(t)
	_ = repo.Create(context.Background(), &jobs.Job{ID: "abc", Status: jobs.StatusQueued})

	rec := rbServe(t, mux, http.MethodHead, "/api/v1/jobs/abc", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", rec.Code)
	}
	t.Log("NOTE: HEAD is auto-routed to the GET handler by ServeMux; the handler " +
		"still does the full repository read. Undocumented but harmless here")
}

// ---------------------------------------------------------------------------
// 2. Unknown paths, slashes, and case
// ---------------------------------------------------------------------------

func TestRobustness_UnknownAndOddPathsAreRoutedPredictably(t *testing.T) {
	mux, repo := newRobustnessMux(t)
	_ = repo.Create(context.Background(), &jobs.Job{ID: "abc", Status: jobs.StatusQueued})

	cases := []struct {
		name, method, target string
		want                 int
	}{
		{"unknown top level", http.MethodGet, "/nope", 404},
		{"unknown api path", http.MethodGet, "/api/v1/nope", 404},
		{"wrong api version", http.MethodGet, "/api/v2/jobs", 404},
		{"uppercase path", http.MethodGet, "/API/V1/JOBS", 404},
		{"empty path segment mid-route", http.MethodGet, "/api//v1/jobs", 307},
		{"leading double slash", http.MethodGet, "//api/v1/jobs", 307},
		{"trailing slash on collection", http.MethodGet, "/api/v1/jobs/", 404},
		{"trailing slash on item", http.MethodGet, "/api/v1/jobs/abc/", 404},
		{"dot segment", http.MethodGet, "/api/v1/./jobs", 307},
		{"parent segment", http.MethodGet, "/api/v1/jobs/abc/..", 307},
		{"extra segment after item", http.MethodGet, "/api/v1/jobs/abc/extra", 404},
		{"cancel with no job id", http.MethodPost, "/api/v1/jobs//cancel", 307},
		{"query string junk", http.MethodGet, "/api/v1/jobs?a=1&a=2&%zz", 200},
		{"fragment-ish", http.MethodGet, "/api/v1/jobs%23frag", 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := rbServe(t, mux, tc.method, tc.target, "")
			if rec.Code != tc.want {
				t.Fatalf("%s %s: got status %d, want %d (body=%.60q, location=%q)",
					tc.method, tc.target, rec.Code, tc.want,
					rec.Body.String(), rec.Header().Get("Location"))
			}
		})
	}
	t.Log("OK: ServeMux answers unclean paths with 307 Temporary Redirect, which " +
		"preserves method and body, so a POST is not silently downgraded to GET")
}

// Path cleaning can change which route a request lands on. POST /api/v1/jobs//cancel
// is redirected to /api/v1/jobs/cancel, i.e. the Get route with jobID="cancel" --
// which then rejects POST with 405. 307 is not part of the documented API contract.
func TestRobustness_PathCleaningRedirectsCancelOntoTheGetRoute(t *testing.T) {
	mux, _ := newRobustnessMux(t)

	rec := rbServe(t, mux, http.MethodPost, "/api/v1/jobs//cancel", "")
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("got status %d, want 307", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if loc != "/api/v1/jobs/cancel" {
		t.Fatalf("got Location %q, want /api/v1/jobs/cancel", loc)
	}

	// Follow the redirect by hand: the target is a different route entirely.
	rec2 := rbServe(t, mux, http.MethodPost, loc, "")
	if rec2.Code != http.StatusMethodNotAllowed {
		t.Fatalf("following the redirect: got status %d, want 405", rec2.Code)
	}
	t.Log("NOTE: a doubled slash rewrites POST .../{id}/cancel into the GET item " +
		"route with jobID=\"cancel\", ending in 405. 307/405 are both absent from " +
		"the documented API status-code list")
}

// ---------------------------------------------------------------------------
// 3. Hostile {jobID} values
// ---------------------------------------------------------------------------

// The jobID wildcard is passed straight to the repository. Nothing validates its
// shape, so traversal-looking IDs reach storage as opaque lookup keys.
func TestRobustness_HostileJobIDsAreTreatedAsOpaqueKeysAnd404(t *testing.T) {
	mux, repo := newRobustnessMux(t)
	_ = repo.Create(context.Background(), &jobs.Job{ID: "abc", Status: jobs.StatusQueued})

	cases := []struct {
		name, target string
		want         int
	}{
		{"encoded traversal", "/api/v1/jobs/..%2f..%2fetc%2fpasswd", 404},
		{"double encoded traversal", "/api/v1/jobs/%252e%252e%252fetc", 404},
		{"encoded dot segments", "/api/v1/jobs/%2e%2e", 404},
		{"absolute path as id", "/api/v1/jobs/%2Fetc%2Fpasswd", 404},
		{"encoded NUL", "/api/v1/jobs/abc%00", 404},
		{"encoded newline", "/api/v1/jobs/abc%0d%0aX-Injected:%20yes", 404},
		{"sql-ish", "/api/v1/jobs/abc%27%20OR%201%3D1--", 404},
		{"unicode", "/api/v1/jobs/%F0%9F%98%80", 404},
		{"space", "/api/v1/jobs/a%20b%20c", 404},
		{"semicolon params", "/api/v1/jobs/abc;jsessionid=1", 404},
		{"exact match still works", "/api/v1/jobs/abc", 200},
		{"case-different id", "/api/v1/jobs/ABC", 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := rbServe(t, mux, http.MethodGet, tc.target, "")
			if rec.Code != tc.want {
				t.Fatalf("%s: got status %d, want %d (body=%.80q)",
					tc.target, rec.Code, tc.want, rec.Body.String())
			}
			// No header injection may leak through the decoded path value.
			if rec.Header().Get("X-Injected") != "" {
				t.Fatal("SECURITY: header injection via path value")
			}
		})
	}
}

func TestRobustness_ExtremelyLongJobIDDoesNotPanic(t *testing.T) {
	mux, _ := newRobustnessMux(t)
	for _, n := range []int{4096, 100_000, 1_000_000} {
		t.Run(fmt.Sprintf("%d chars", n), func(t *testing.T) {
			start := time.Now()
			rec := rbServe(t, mux, http.MethodGet, "/api/v1/jobs/"+strings.Repeat("a", n), "")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("got status %d, want 404", rec.Code)
			}
			t.Logf("%d-char jobID handled in %s", n, time.Since(start))
		})
	}
	t.Log("NOTE: handler-level there is no jobID length check; the only protection is " +
		"net/http's 1 MiB MaxHeaderBytes on a real listener (see the live-server test)")
}

// A real listener enforces http.DefaultMaxHeaderBytes; verify the oversized request
// line is refused by the server rather than reaching the repository.
func TestRobustness_LiveServerRejectsOversizedRequestLine(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind a local listener in this environment: %v", err)
	}
	mux, _ := newRobustnessMux(t)
	srv := httptest.NewUnstartedServer(mux)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	client := &http.Client{Timeout: 20 * time.Second}

	t.Run("normal request works", func(t *testing.T) {
		resp, err := client.Get(srv.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("got status %d, want 200", resp.StatusCode)
		}
	})

	t.Run("2MB request line", func(t *testing.T) {
		resp, err := client.Get(srv.URL + "/api/v1/jobs/" + strings.Repeat("a", 2<<20))
		if err != nil {
			t.Logf("connection error (server refused the oversized request line): %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("2 MiB URL reached the handler and returned 404; expected the " +
				"server to reject it with 431")
		}
		t.Logf("2 MiB URL -> status %d", resp.StatusCode)
	})

	t.Run("unbounded POST body over the wire", func(t *testing.T) {
		const size = 20 << 20
		body := rbHugeTaskBody(size)
		start := time.Now()
		resp, err := client.Post(srv.URL+"/api/v1/jobs", "application/json", body)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		// FIXED: over a real connection the body cap now rejects with 413 and
		// the server stops reading instead of buffering the whole payload.
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("got status %d, want 413", resp.StatusCode)
		}
		// Deliberately not reading body.n here: with the cap in place the server
		// answers while the client is still writing, so the counter is still
		// being mutated by the request goroutine.
		t.Logf("a %d MiB body was rejected with 413 in %s", size>>20, time.Since(start))
	})
}

// ---------------------------------------------------------------------------
// 4. Requests that carry unexpected extras
// ---------------------------------------------------------------------------

func TestRobustness_BodyOnGETAndQueryParamsAreIgnored(t *testing.T) {
	mux, repo := newRobustnessMux(t)
	_ = repo.Create(context.Background(), &jobs.Job{ID: "abc", Status: jobs.StatusQueued})

	rec := rbServe(t, mux, http.MethodGet, "/api/v1/jobs/abc?limit=-1&offset=evil", `{"task":"ignored"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", rec.Code)
	}
	t.Log("NOTE: List/Get accept and ignore all query parameters -- there is no " +
		"pagination, so GET /api/v1/jobs always serializes every job in storage")
}

// List is bounded: the response no longer grows with the number of jobs.
func TestRobustness_ListIsBounded(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	h := NewJobsHandler(repo, &rbStubQueue{}, &rbStubCanceller{}, nil)
	for i := 0; i < 5000; i++ {
		_ = repo.Create(context.Background(), &jobs.Job{
			ID:       fmt.Sprintf("job-%d", i),
			Task:     strings.Repeat("x", 500),
			RepoPath: "/tmp/repo",
			Status:   jobs.StatusQueued,
		})
	}
	rec := httptest.NewRecorder()
	start := time.Now()
	h.List(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil))
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", rec.Code)
	}
	var got []jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != DefaultListLimit {
		t.Fatalf("got %d jobs of 5000, want the %d default page", len(got), DefaultListLimit)
	}
	if total := rec.Header().Get("X-Total-Count"); total != "5000" {
		t.Errorf("X-Total-Count = %q, want 5000 so a client can page", total)
	}
	// The read is still O(total jobs) — the store materialises and sorts
	// everything to page it. What is bounded is the response.
	t.Logf("bounded: %d of 5000 jobs, %d bytes, %s", len(got), rec.Body.Len(), elapsed)
}

// List order used to be Go map-iteration order, which changes between identical
// requests — and any paging built on that is unsound, because consecutive pages
// overlap and skip. It is now newest-first, and that is part of the interface.
func TestRobustness_ListOrderIsDeterministic(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	h := NewJobsHandler(repo, &rbStubQueue{}, &rbStubCanceller{}, nil)
	for i := 0; i < 50; i++ {
		_ = repo.Create(context.Background(), &jobs.Job{
			ID: fmt.Sprintf("job-%02d", i), Status: jobs.StatusQueued})
	}

	order := func() string {
		rec := httptest.NewRecorder()
		h.List(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil))
		var got []jobs.Job
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		ids := make([]string, len(got))
		for i, j := range got {
			ids[i] = j.ID
		}
		return strings.Join(ids, ",")
	}

	first := order()
	for i := 0; i < 20; i++ {
		if got := order(); got != first {
			t.Fatalf("run %d returned a different order:\n %s\n %s", i, first, got)
		}
	}
}
