package metrics

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/agents/model"
)

func section(t *testing.T, snap map[string]any, key string) map[string]any {
	t.Helper()
	m, ok := snap[key].(map[string]any)
	if !ok {
		t.Fatalf("snapshot has no %q section: %v", key, snap)
	}
	return m
}

func TestJobLifecycleCounts(t *testing.T) {
	r := New()

	done := r.JobStarted()
	if got := section(t, r.Snapshot(), "jobs")["active"]; got != int64(1) {
		t.Errorf("active = %v, want 1 while the job runs", got)
	}
	done("completed", 250*time.Millisecond)

	jobs := section(t, r.Snapshot(), "jobs")
	if jobs["active"] != int64(0) || jobs["completed"] != int64(1) || jobs["total"] != int64(1) {
		t.Errorf("after completion: %v", jobs)
	}
	dur := jobs["duration"].(map[string]any)
	if dur["count"] != int64(1) || dur["max_ms"] != int64(250) {
		t.Errorf("duration = %v", dur)
	}
}

func TestOutcomesLandInTheirOwnCounters(t *testing.T) {
	r := New()
	r.JobStarted()("completed", time.Millisecond)
	r.JobStarted()("failed", time.Millisecond)
	r.JobStarted()("failed", time.Millisecond)
	r.JobStarted()("cancelled", time.Millisecond)

	jobs := section(t, r.Snapshot(), "jobs")
	if jobs["completed"] != int64(1) || jobs["failed"] != int64(2) || jobs["cancelled"] != int64(1) {
		t.Errorf("outcomes miscounted: %v", jobs)
	}
}

func TestModelCallsAndErrors(t *testing.T) {
	r := New()
	r.ModelCall(100*time.Millisecond, nil)
	r.ModelCall(300*time.Millisecond, errors.New("rate limited"))

	m := section(t, r.Snapshot(), "model")
	if m["calls"] != int64(2) || m["errors"] != int64(1) {
		t.Errorf("model = %v", m)
	}
	lat := m["latency"].(map[string]any)
	if lat["mean_ms"] != int64(200) || lat["max_ms"] != int64(300) {
		t.Errorf("latency = %v", lat)
	}
}

// Clock skew between the timestamps a wait is derived from must not corrupt
// the totals silently.
func TestNegativeDurationIsDropped(t *testing.T) {
	r := New()
	r.QueueWait(-5 * time.Second)
	r.QueueWait(2 * time.Second)

	wait := section(t, r.Snapshot(), "queue")["wait"].(map[string]any)
	if wait["count"] != int64(1) || wait["total_ms"] != int64(2000) {
		t.Errorf("wait = %v", wait)
	}
}

// Every recording site is on the worker pool's hot path, so a nil registry —
// the unconfigured case — must be a no-op, not a panic.
func TestNilRegistryIsSafe(t *testing.T) {
	var r *Registry
	r.JobStarted()("completed", time.Second)
	r.QueueWait(time.Second)
	r.ModelCall(time.Second, nil)
	r.RevisionRequested()
	if got := r.Snapshot(); len(got) != 0 {
		t.Errorf("nil snapshot = %v", got)
	}
}

// The registry is written by every worker at once; the race detector is the
// real assertion here.
func TestConcurrentRecording(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				done := r.JobStarted()
				r.QueueWait(time.Millisecond)
				r.ModelCall(time.Millisecond, nil)
				done("completed", time.Millisecond)
			}
		}()
	}
	wg.Wait()

	jobs := section(t, r.Snapshot(), "jobs")
	if jobs["completed"] != int64(800) || jobs["active"] != int64(0) {
		t.Errorf("after 800 jobs: %v", jobs)
	}
}

type fakeModel struct {
	resp model.Response
	err  error
}

func (f fakeModel) Generate(context.Context, model.Request) (model.Response, error) {
	return f.resp, f.err
}

func TestInstrumentModel(t *testing.T) {
	r := New()
	ok := InstrumentModel(fakeModel{resp: model.Response{Text: "hi"}}, r)
	bad := InstrumentModel(fakeModel{err: errors.New("boom")}, r)

	if resp, err := ok.Generate(context.Background(), model.Request{}); err != nil || resp.Text != "hi" {
		t.Fatalf("the decorator altered the result: %v %v", resp, err)
	}
	if _, err := bad.Generate(context.Background(), model.Request{}); err == nil {
		t.Fatal("the decorator swallowed the error")
	}

	m := section(t, r.Snapshot(), "model")
	if m["calls"] != int64(2) || m["errors"] != int64(1) {
		t.Errorf("model = %v", m)
	}
}
