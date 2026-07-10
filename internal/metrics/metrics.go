// Package metrics counts what the system actually does, so claims about it can
// be read off an endpoint instead of invented.
//
// It is deliberately the standard library and nothing else: atomic counters, a
// mutex where a max is tracked, one JSON snapshot. A metrics vendor becomes
// worth its operational weight when there is a fleet to aggregate across;
// one process serving JSON is enough for one process's truth.
package metrics

import (
	"sync"
	"sync/atomic"
	"time"
)

// Registry is the process's single metrics store. All methods are safe for
// concurrent use; the zero value is not usable — construct with New.
type Registry struct {
	// Job outcomes. Terminal states only: a job increments exactly one.
	jobsCompleted atomic.Int64
	jobsFailed    atomic.Int64
	jobsCancelled atomic.Int64

	// activeJobs counts jobs currently inside Process — the live gauge that,
	// against the worker count, says whether the pool is saturated.
	activeJobs atomic.Int64

	// Model traffic. Latency is summed so mean = total/calls; the histogram a
	// fleet would want is a vendor feature this stage does not need.
	modelCalls   atomic.Int64
	modelErrors  atomic.Int64
	modelLatency durationStat

	jobDuration durationStat
	queueWait   durationStat

	revisionsRequested atomic.Int64
}

// durationStat tracks total, count and max for one duration series. Mean is
// derived at snapshot time. A mutex rather than atomics because max needs a
// compare-and-set loop anyway, and these record a handful of times per job —
// nothing hot enough to care.
type durationStat struct {
	mu    sync.Mutex
	total time.Duration
	count int64
	max   time.Duration
}

func (d *durationStat) record(v time.Duration) {
	if v < 0 {
		// A negative duration means clock skew between the timestamps it was
		// derived from (job created on one write, observed on another).
		// Recording it would corrupt the totals silently.
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.total += v
	d.count++
	if v > d.max {
		d.max = v
	}
}

func (d *durationStat) snapshot() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]any{
		"count":    d.count,
		"total_ms": d.total.Milliseconds(),
		"max_ms":   d.max.Milliseconds(),
	}
	if d.count > 0 {
		out["mean_ms"] = (d.total / time.Duration(d.count)).Milliseconds()
	}
	return out
}

func New() *Registry { return &Registry{} }

// JobStarted marks a job entering Process. The returned func marks it leaving
// with its terminal status — defer it, so every exit path decrements the gauge.
func (r *Registry) JobStarted() func(status string, took time.Duration) {
	if r == nil {
		return func(string, time.Duration) {}
	}
	r.activeJobs.Add(1)
	return func(status string, took time.Duration) {
		r.activeJobs.Add(-1)
		r.jobDuration.record(took)
		switch status {
		case "completed":
			r.jobsCompleted.Add(1)
		case "cancelled":
			r.jobsCancelled.Add(1)
		default:
			r.jobsFailed.Add(1)
		}
	}
}

// QueueWait records how long a job sat between creation and a worker picking
// it up. Derived from the job's CreatedAt, so a restart-requeued job reports
// its whole wait — which is the honest number for "how long did I wait".
func (r *Registry) QueueWait(v time.Duration) {
	if r != nil {
		r.queueWait.record(v)
	}
}

func (r *Registry) RevisionRequested() {
	if r != nil {
		r.revisionsRequested.Add(1)
	}
}

// ModelCall records one round trip to the model.
func (r *Registry) ModelCall(took time.Duration, err error) {
	if r == nil {
		return
	}
	r.modelCalls.Add(1)
	r.modelLatency.record(took)
	if err != nil {
		r.modelErrors.Add(1)
	}
}

// Snapshot renders every metric as JSON-ready data. Rates (success rate, error
// rate) are left to the reader: raw counters compose across time windows,
// ratios do not.
func (r *Registry) Snapshot() map[string]any {
	if r == nil {
		return map[string]any{}
	}
	completed := r.jobsCompleted.Load()
	failed := r.jobsFailed.Load()
	cancelled := r.jobsCancelled.Load()
	return map[string]any{
		"jobs": map[string]any{
			"active":    r.activeJobs.Load(),
			"completed": completed,
			"failed":    failed,
			"cancelled": cancelled,
			"total":     completed + failed + cancelled,
			"duration":  r.jobDuration.snapshot(),
		},
		"queue": map[string]any{
			"wait": r.queueWait.snapshot(),
		},
		"model": map[string]any{
			"calls":   r.modelCalls.Load(),
			"errors":  r.modelErrors.Load(),
			"latency": r.modelLatency.snapshot(),
		},
		"revisions_requested": r.revisionsRequested.Load(),
	}
}
