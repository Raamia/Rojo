package worker

// Resilience / failure-handling characterization tests for the worker pool and
// its interaction with the in-memory queue during shutdown and restart.
// Every assertion documents ACTUAL behavior, including behavior that is a bug.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/queue"
)

// resGateProcessor hands the caller the exact context the worker passed in and
// blocks until released, so a test can observe an in-flight job's context.
type resGateProcessor struct {
	started chan context.Context
	release chan struct{}
}

func (p *resGateProcessor) Process(ctx context.Context, _ string) error {
	p.started <- ctx
	<-p.release
	return ctx.Err()
}

// resRecordingProcessor records which job IDs were seen and which actually ran
// to completion.
type resRecordingProcessor struct {
	mu        sync.Mutex
	seen      []string
	completed []string
	delay     time.Duration
}

func (p *resRecordingProcessor) Process(ctx context.Context, id string) error {
	p.mu.Lock()
	p.seen = append(p.seen, id)
	p.mu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(p.delay):
	}

	p.mu.Lock()
	p.completed = append(p.completed, id)
	p.mu.Unlock()
	return nil
}

func (p *resRecordingProcessor) counts() (seen, completed int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.seen), len(p.completed)
}

// ---------------------------------------------------------------------------
// FAILURE MODE 1 — shutdown kills in-flight jobs rather than draining them
// ---------------------------------------------------------------------------

// cmd/api/main.go:108-110 runs cancelWorkers() -> q.Close() -> pool.Wait().
// pool.go:49 passes the worker context straight into Process, so cancelWorkers
// immediately cancels every in-flight job's context. pool.Wait() therefore
// waits for jobs to ABORT, not to finish: there is no drain phase and no
// grace period for running work.
func TestResilience_ShutdownCancelsInFlightJobInsteadOfDraining(t *testing.T) {
	q := queue.New(8)
	p := &resGateProcessor{started: make(chan context.Context, 1), release: make(chan struct{})}
	pool := NewPool(1, q, p)

	ctx, cancel := context.WithCancel(context.Background())
	pool.Start(ctx)

	if err := q.Enqueue("job-inflight"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	var jobCtx context.Context
	select {
	case jobCtx = <-p.started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker never started the job")
	}
	if err := jobCtx.Err(); err != nil {
		t.Fatalf("job context already cancelled before shutdown: %v", err)
	}

	cancel() // == cancelWorkers() in main.go

	select {
	case <-jobCtx.Done():
		// Expected: the running job's context is cancelled instantly.
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight job context was NOT cancelled — a drain phase was added, re-baseline this test")
	}

	close(p.release)
	q.Close()

	done := make(chan struct{})
	go func() { pool.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pool.Wait did not return")
	}
}

// Jobs still sitting in the channel at shutdown are consumed and discarded
// without ever being executed, and nothing records that they were dropped.
func TestResilience_ShutdownSilentlyDropsRemainingQueuedJobs(t *testing.T) {
	const enqueued = 6

	q := queue.New(16)
	p := &resRecordingProcessor{delay: 300 * time.Millisecond}
	pool := NewPool(1, q, p)

	ctx, cancel := context.WithCancel(context.Background())
	pool.Start(ctx)

	for i := 0; i < enqueued; i++ {
		if err := q.Enqueue(fmt.Sprintf("job-%d", i)); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	// Wait until the worker has picked up the first job, then shut down.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if seen, _ := p.counts(); seen > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker never started any job")
		}
		time.Sleep(2 * time.Millisecond)
	}

	cancel()
	q.Close()

	done := make(chan struct{})
	go func() { pool.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pool.Wait did not return")
	}

	seen, completed := p.counts()
	if completed >= enqueued {
		t.Fatalf("completed = %d of %d — shutdown now drains the queue, re-baseline this test", completed, enqueued)
	}
	t.Logf("shutdown dropped %d of %d jobs (seen=%d, completed=%d); none were persisted as failed or requeued",
		enqueued-completed, enqueued, seen, completed)
}

// ---------------------------------------------------------------------------
// FAILURE MODE 2 — queued jobs are lost on restart, with no recovery
// ---------------------------------------------------------------------------

// The queue is a plain in-process `chan string` (internal/queue/queue.go:8).
// Nothing in cmd/api/main.go re-reads jobs whose persisted status is `queued`
// at startup, so every job that was buffered at crash time is orphaned: it
// stays `queued` in postgres forever and is never handed to a worker again.
func TestResilience_QueuedJobsAreLostOnRestartWithNoRecovery(t *testing.T) {
	const pending = 10

	repo := jobs.NewInMemoryRepository()
	ctx := context.Background()

	// --- process instance #1: accept jobs, persist them as queued, buffer them.
	q1 := queue.New(64) // default ROJO_QUEUE_BUFFER
	for i := 0; i < pending; i++ {
		id := fmt.Sprintf("job-%d", i)
		if err := repo.Create(ctx, &jobs.Job{
			ID: id, Task: "t", RepoPath: "/tmp/repo", Status: jobs.StatusQueued,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		if err := q1.Enqueue(id); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
	}
	buffered := q1.Len()
	if buffered != pending {
		t.Fatalf("queue holds %d, want %d", buffered, pending)
	}

	// --- crash: the process dies. The channel and everything in it evaporate.
	q1 = nil
	_ = q1

	// --- process instance #2: exactly what main.go does on startup — build a
	// fresh empty queue and a pool. No requeue step exists anywhere.
	q2 := queue.New(64)
	p := &resRecordingProcessor{delay: time.Millisecond}
	pool := NewPool(4, q2, p)
	restartCtx, cancel := context.WithCancel(context.Background())
	pool.Start(restartCtx)

	time.Sleep(250 * time.Millisecond)

	cancel()
	q2.Close()
	pool.Wait()

	seen, _ := p.counts()
	if seen != 0 {
		t.Fatalf("restart processed %d jobs — startup recovery was added, re-baseline this test", seen)
	}

	all, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	stillQueued := 0
	for _, j := range all {
		if j.Status == jobs.StatusQueued {
			stillQueued++
		}
	}
	if stillQueued != pending {
		t.Fatalf("%d jobs still queued, want %d", stillQueued, pending)
	}
	t.Logf("DATA LOSS: %d jobs persisted as %q are unreachable after restart (queue buffer default 64 = worst-case loss per crash)",
		stillQueued, jobs.StatusQueued)
}

// ---------------------------------------------------------------------------
// FAILURE MODE 1/8 — one panicking job takes down the whole service
// ---------------------------------------------------------------------------

type resPanicProcessor struct{}

func (resPanicProcessor) Process(context.Context, string) error {
	panic("simulated panic inside a job step")
}

// runWorker (pool.go:36-54) calls processor.Process with no recover, and no
// other layer wraps it either. A panic anywhere in the pipeline — including the
// send-on-closed-channel panic in InProcessBus.Publish proven by
// internal/events/persistence_resilience_test.go — therefore kills the entire
// API process, dropping every other in-flight job and everything still buffered
// in the queue.
//
// Proven by re-executing this test binary as a child process: the child starts
// a pool with a panicking processor and must die.
func TestResilience_PanicInAJobCrashesTheEntireProcess(t *testing.T) {
	if os.Getenv("ROJO_RESILIENCE_PANIC_CHILD") == "1" {
		q := queue.New(1)
		pool := NewPool(1, q, resPanicProcessor{})
		pool.Start(context.Background())
		if err := q.Enqueue("boom"); err != nil {
			panic("enqueue failed: " + err.Error())
		}
		time.Sleep(10 * time.Second) // the panic should kill us long before this
		return
	}

	cmd := exec.Command(os.Args[0],
		"-test.run=^TestResilience_PanicInAJobCrashesTheEntireProcess$",
		"-test.timeout=60s",
	)
	cmd.Env = append(os.Environ(), "ROJO_RESILIENCE_PANIC_CHILD=1")
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatal("child process exited cleanly — the worker pool now recovers panics, re-baseline this test")
	}
	if !strings.Contains(string(out), "panic: simulated panic inside a job step") {
		t.Fatalf("child died without the expected panic. exit=%v output:\n%s", err, out)
	}
	t.Logf("CONFIRMED: a panic in one job killed the whole process (exit: %v); every other in-flight and queued job dies with it", err)
}
