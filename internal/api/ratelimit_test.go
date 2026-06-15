package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
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
		want       string
	}{
		{"strips port from remote addr", "192.0.2.5:54321", "", "192.0.2.5"},
		{"ipv6 remote addr", "[2001:db8::1]:443", "", "2001:db8::1"},
		{"xff single", "10.0.0.9:80", "198.51.100.2", "198.51.100.2"},
		{"xff list takes first, trimmed", "10.0.0.9:80", "198.51.100.2, 70.1.2.3, 70.1.2.4", "198.51.100.2"},
		{"malformed remote addr falls back whole", "not-an-addr", "", "not-an-addr"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				req.Header.Set("X-Forwarded-For", tt.xff)
			}
			if got := clientKey(req); got != tt.want {
				t.Errorf("clientKey = %q, want %q", got, tt.want)
			}
		})
	}
}
