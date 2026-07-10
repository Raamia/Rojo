package orchestration

import (
	"context"
	"testing"

	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/metrics"
)

// The registry only earns its place if the pipeline actually feeds it: a job
// that runs must land in the counters with its real outcome and a plausible
// duration, and the active gauge must return to zero.
func TestProcess_FeedsTheMetricsRegistry(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	reg := metrics.New()

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Metrics = reg
	newQueuedJob(t, repo, "counted")

	if err := p.Process(context.Background(), "counted"); err != nil {
		t.Fatalf("process: %v", err)
	}

	snap := reg.Snapshot()
	jobsSec := snap["jobs"].(map[string]any)
	if jobsSec["completed"] != int64(1) {
		t.Errorf("completed = %v, want 1", jobsSec["completed"])
	}
	if jobsSec["active"] != int64(0) {
		t.Errorf("active = %v, want 0 after the job ends", jobsSec["active"])
	}
	dur := jobsSec["duration"].(map[string]any)
	if dur["count"] != int64(1) {
		t.Errorf("duration count = %v", dur["count"])
	}
	wait := snap["queue"].(map[string]any)["wait"].(map[string]any)
	if wait["count"] != int64(1) {
		t.Errorf("queue wait was not recorded: %v", wait)
	}
}

// A failed job counts as failed — and the gauge still comes back down.
func TestProcess_FailureIsCountedAsFailure(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	reg := metrics.New()

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Metrics = reg
	p.Workspaces = &diffingWorkspaces{base: t.TempDir()}
	p.Implementor = panicImplementor{} // ends the job via the recovery path
	newQueuedJob(t, repo, "boomjob")

	if err := p.Process(context.Background(), "boomjob"); err == nil {
		t.Fatal("expected the job to fail")
	}
	jobsSec := reg.Snapshot()["jobs"].(map[string]any)
	if jobsSec["failed"] != int64(1) || jobsSec["active"] != int64(0) {
		t.Errorf("after a panic-failed job: %v", jobsSec)
	}
}
