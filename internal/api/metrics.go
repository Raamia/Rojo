package api

import (
	"net/http"

	"github.com/Raamia/Rojo/internal/metrics"
)

// MetricsHandler serves the process's counters as JSON.
//
// It sits under /api/v1 and therefore behind the auth token: the numbers are
// not secrets exactly, but job counts and failure rates describe what a
// machine is being used for, and there is no reason to hand that to anyone
// who can merely reach the port.
type MetricsHandler struct {
	Registry *metrics.Registry
	// Live reads gauges that belong to other components — queue depth, worker
	// count — so the registry does not have to reach into them.
	Live func() map[string]any
}

func NewMetricsHandler(r *metrics.Registry, live func() map[string]any) *MetricsHandler {
	return &MetricsHandler{Registry: r, Live: live}
}

func (h *MetricsHandler) Metrics(w http.ResponseWriter, r *http.Request) {
	snap := h.Registry.Snapshot()
	if h.Live != nil {
		for k, v := range h.Live() {
			snap[k] = v
		}
	}
	WriteJSON(w, http.StatusOK, snap)
}
