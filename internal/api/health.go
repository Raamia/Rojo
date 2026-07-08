package api

import (
	"context"
	"net/http"
	"time"
)

// HealthCheckTimeout bounds one probe. A health endpoint that can hang is worse
// than none: an orchestrator waiting on it cannot tell "slow" from "wedged", and
// hanging probes pile up connections on an already struggling process.
const HealthCheckTimeout = 2 * time.Second

// Check is one named thing that can be broken.
type Check func(ctx context.Context) error

// HealthHandler answers whether this instance can actually do its job.
//
// The previous endpoint returned {"status":"ok"} unconditionally, which only
// proved the HTTP server was accepting connections — it would happily report
// healthy with a read-only data directory and every job failing to persist.
// That is exactly the outage a load balancer needs to route around, so the
// checks probe the parts that genuinely fail.
type HealthHandler struct {
	Checks map[string]Check
	// Info is static context reported alongside the checks — queue depth,
	// worker count. Never load-bearing for the status.
	Info func() map[string]any
}

func NewHealthHandler() *HealthHandler {
	return &HealthHandler{Checks: map[string]Check{}}
}

func (h *HealthHandler) Register(name string, c Check) {
	if h.Checks == nil {
		h.Checks = map[string]Check{}
	}
	h.Checks[name] = c
}

func (h *HealthHandler) Health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), HealthCheckTimeout)
	defer cancel()

	results := make(map[string]string, len(h.Checks))
	healthy := true
	for name, check := range h.Checks {
		if err := check(ctx); err != nil {
			// The reason goes in the body: this endpoint is for operators, and
			// "unhealthy" without a cause means reading logs to learn what a
			// probe already knew. It is not an authenticated endpoint, so the
			// error text must not carry anything a check would not print to a
			// log anyway.
			results[name] = "failed: " + err.Error()
			healthy = false
			continue
		}
		results[name] = "ok"
	}

	body := map[string]any{"status": "ok", "checks": results}
	if h.Info != nil {
		for k, v := range h.Info() {
			body[k] = v
		}
	}

	status := http.StatusOK
	if !healthy {
		body["status"] = "unhealthy"
		// 503 rather than 500: the instance is temporarily unable to serve, and
		// that is what tells a load balancer to stop sending traffic here.
		status = http.StatusServiceUnavailable
	}
	WriteJSON(w, status, body)
}
