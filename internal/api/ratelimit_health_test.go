package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Health probes share a source IP with real traffic (and with each other behind
// a NAT or ingress that sets no X-Forwarded-For). If the limiter counted them,
// a load spike would start failing liveness probes and the orchestrator would
// restart an otherwise-healthy process, shedding capacity mid-spike. A live run
// with the default burst of 30 returned 429 on probes 32-40 before this fix.
func TestRateLimiter_HealthCheckIsNeverLimited(t *testing.T) {
	rl := NewRateLimiter(3, 0) // tiny burst, no refill
	h := rl.Middleware()(okHandler())

	for i := 0; i < 50; i++ {
		req := httptest.NewRequest(http.MethodGet, healthPath, nil)
		req.RemoteAddr = "10.0.0.5:4000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("probe %d got %d, want 200 — health checks must not be rate limited", i+1, rec.Code)
		}
	}
}

// The bypass must be specific to the health path: ordinary traffic from the
// same client is still limited.
func TestRateLimiter_HealthBypassDoesNotExemptOtherPaths(t *testing.T) {
	rl := NewRateLimiter(2, 0)
	h := rl.Middleware()(okHandler())

	var limited bool
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
		req.RemoteAddr = "10.0.0.5:4000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Error("expected non-health traffic to still be rate limited")
	}
}
