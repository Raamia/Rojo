package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// A client hitting the API over several TCP connections arrives with the same
// IP but a different ephemeral port each time. The limiter must key on the IP
// so those connections share one bucket, not get a fresh bucket per port.
func TestRateLimiter_KeysByIPNotPort(t *testing.T) {
	const burst = 3
	rl := NewRateLimiter(burst, 0) // no refill, so the burst is the hard cap
	h := rl.Middleware()(okHandler())

	ports := []string{"1111", "2222", "3333", "4444", "5555"}
	var allowed, limited int
	for _, p := range ports {
		// Deliberately not the health path: that one is exempt from limiting.
		req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
		req.RemoteAddr = "203.0.113.7:" + p
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		switch rec.Code {
		case http.StatusOK:
			allowed++
		case http.StatusTooManyRequests:
			limited++
		default:
			t.Fatalf("port %s: unexpected status %d", p, rec.Code)
		}
	}
	if allowed != burst {
		t.Errorf("allowed %d requests, want %d (burst) — limiter is not keying by IP", allowed, burst)
	}
	if limited != len(ports)-burst {
		t.Errorf("limited %d requests, want %d", limited, len(ports)-burst)
	}
}

// Distinct client IPs must not share a bucket.
func TestRateLimiter_DistinctIPsIndependent(t *testing.T) {
	rl := NewRateLimiter(1, 0)
	h := rl.Middleware()(okHandler())

	for _, ip := range []string{"10.0.0.1:5000", "10.0.0.2:5000", "10.0.0.3:5000"} {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.RemoteAddr = ip
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("first request from %s got %d, want 200", ip, rec.Code)
		}
	}
}

func TestClientKey(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		trustProxy bool
		want       string
	}{
		{"strips port from remote addr", "192.0.2.5:54321", "", false, "192.0.2.5"},
		{"ipv6 remote addr", "[2001:db8::1]:443", "", false, "2001:db8::1"},
		// XFF is attacker-writable, so by default it is ignored entirely.
		{"xff ignored by default", "10.0.0.9:80", "198.51.100.2", false, "10.0.0.9"},
		{"xff honoured when trusted", "10.0.0.9:80", "198.51.100.2", true, "198.51.100.2"},
		// The rightmost entry is the one the trusted proxy appended; everything
		// to its left is client-supplied and forgeable.
		{"trusted xff list takes rightmost, trimmed", "10.0.0.9:80", "spoofed, 70.1.2.3", true, "70.1.2.3"},
		{"malformed remote addr falls back whole", "not-an-addr", "", false, "not-an-addr"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rl := NewRateLimiter(1, 1)
			if tt.trustProxy {
				rl.TrustProxyHeader()
			}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				req.Header.Set("X-Forwarded-For", tt.xff)
			}
			if got := rl.clientKey(req); got != tt.want {
				t.Errorf("clientKey = %q, want %q", got, tt.want)
			}
		})
	}
}

// With a trusted appending proxy, the right-most XFF entry is the one the proxy
// wrote and the only one a client cannot forge — the client owns everything to
// its left. Keying on the left-most entry let an attacker mint a bucket per
// request by varying a field they control; the limiter must resist that.
func TestRateLimiter_TrustedProxyKeysOnTheRightmostXFF(t *testing.T) {
	rl := NewRateLimiter(1, 0).TrustProxyHeader() // burst 1, no refill
	h := rl.Middleware()(okHandler())

	allowed := 0
	for i := 0; i < 50; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
		req.RemoteAddr = "10.0.0.9:5555" // the proxy
		// Attacker varies the left part; the proxy appended the real client IP.
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("spoof-%d, 203.0.113.7", i))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			allowed++
		}
	}
	if allowed != 1 {
		t.Errorf("%d requests allowed, want exactly the burst of 1 — the rightmost key held", allowed)
	}
}

func TestClientKey_RightmostWhenTrusted(t *testing.T) {
	rl := NewRateLimiter(1, 1).TrustProxyHeader()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.9:80"
	req.Header.Set("X-Forwarded-For", "attacker, middle, 198.51.100.2")
	if got := rl.clientKey(req); got != "198.51.100.2" {
		t.Errorf("clientKey = %q, want the rightmost 198.51.100.2", got)
	}
}

// Idle buckets must be reclaimed by elapsed time, not only by a fresh flood of
// distinct IPs. Once traffic settles to a recurring set, no new buckets are
// inserted, so an insert-only trigger would never sweep again.
func TestRateLimiter_IdleBucketsEvictedByTime(t *testing.T) {
	rl := NewRateLimiter(10, 100)
	h := rl.Middleware()(okHandler())

	// A burst of one-shot clients that then never return.
	for i := 0; i < 20; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
		req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1000", i)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	// Force them all idle and push the last sweep past the TTL, without any new
	// distinct IPs arriving.
	rl.mu.Lock()
	past := time.Now().Add(-2 * bucketIdleTTL)
	for _, b := range rl.buckets {
		b.lastSeen = past
	}
	rl.lastSwept = past
	before := len(rl.buckets)
	rl.mu.Unlock()

	// A single request from an already-known client — no insert — must still
	// trigger the time-based sweep.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	req.RemoteAddr = "192.0.2.0:1000"
	h.ServeHTTP(httptest.NewRecorder(), req)

	rl.mu.Lock()
	after := len(rl.buckets)
	rl.mu.Unlock()
	if before < 20 {
		t.Fatalf("setup wrong: %d buckets", before)
	}
	if after > 1 {
		t.Errorf("%d buckets after a time-based sweep, want the idle ones gone", after)
	}
}
