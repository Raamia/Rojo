package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func getHealth(h *HealthHandler) (*httptest.ResponseRecorder, map[string]any) {
	rec := httptest.NewRecorder()
	h.Health(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec, body
}

func TestHealth_AllChecksPassing(t *testing.T) {
	h := NewHealthHandler()
	h.Register("store", func(context.Context) error { return nil })

	rec, body := getHealth(h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %v, want ok", body["status"])
	}
	checks, _ := body["checks"].(map[string]any)
	if checks["store"] != "ok" {
		t.Errorf("checks = %v", checks)
	}
}

// The whole point: an instance whose data directory has gone read-only still
// answers HTTP perfectly, so an endpoint that only proves the server is up
// would keep it in the load balancer while every job fails to persist.
func TestHealth_FailingCheckIsUnavailable(t *testing.T) {
	h := NewHealthHandler()
	h.Register("store", func(context.Context) error {
		return errors.New("data dir is not writable: read-only file system")
	})

	rec, body := getHealth(h)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 so a load balancer routes around this instance", rec.Code)
	}
	if body["status"] != "unhealthy" {
		t.Errorf("status = %v, want unhealthy", body["status"])
	}
	// An operator should not have to go read logs to find out what a probe
	// already knew.
	checks, _ := body["checks"].(map[string]any)
	if s, _ := checks["store"].(string); !strings.Contains(s, "read-only") {
		t.Errorf("checks = %v, want the reason", checks)
	}
}

// One broken dependency must not hide the state of the others.
func TestHealth_ReportsEveryCheck(t *testing.T) {
	h := NewHealthHandler()
	h.Register("store", func(context.Context) error { return errors.New("boom") })
	h.Register("other", func(context.Context) error { return nil })

	rec, body := getHealth(h)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	checks, _ := body["checks"].(map[string]any)
	if len(checks) != 2 || checks["other"] != "ok" {
		t.Errorf("checks = %v, want both reported", checks)
	}
}

// A probe that can hang is worse than none: an orchestrator waiting on it
// cannot tell "slow" from "wedged".
func TestHealth_SlowCheckIsBounded(t *testing.T) {
	h := NewHealthHandler()
	h.Register("wedged", func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
			return nil
		}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		rec, _ := getHealth(h)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status %d, want 503 for a check that timed out", rec.Code)
		}
	}()

	select {
	case <-done:
	case <-time.After(HealthCheckTimeout + 3*time.Second):
		t.Fatal("the health endpoint hung on a wedged check")
	}
}

// Queue depth and worker count are context, not a verdict — a full queue is
// busy, not broken.
func TestHealth_InfoDoesNotAffectStatus(t *testing.T) {
	h := NewHealthHandler()
	h.Register("store", func(context.Context) error { return nil })
	h.Info = func() map[string]any { return map[string]any{"queue_depth": 64, "workers": 4} }

	rec, body := getHealth(h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if body["queue_depth"] != float64(64) || body["workers"] != float64(4) {
		t.Errorf("info missing from body: %v", body)
	}
}

// With nothing registered the endpoint still answers, so a misconfigured
// instance does not look dead.
func TestHealth_NoChecksIsHealthy(t *testing.T) {
	if rec, _ := getHealth(NewHealthHandler()); rec.Code != http.StatusOK {
		t.Errorf("status %d, want 200", rec.Code)
	}
}
