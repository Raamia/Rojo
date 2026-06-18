package jobs

// Resilience / failure-handling characterization tests for the job repository's
// concurrent-update semantics. Assertions document ACTUAL behavior.

import (
	"context"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// FAILURE MODE 5 — concurrent updates: last-write-wins, no optimistic locking
// ---------------------------------------------------------------------------

// Neither repository implementation carries a version/revision column and
// neither guards on the previous status, so two writers that both read the same
// job and then write silently clobber each other. Job.Transition validates
// against the writer's own in-memory copy only — it cannot see that another
// goroutine already advanced the stored row.
func TestResilience_ConcurrentUpdateIsLastWriteWinsLostUpdate(t *testing.T) {
	repo := NewInMemoryRepository()
	ctx := context.Background()
	if err := repo.Create(ctx, &Job{ID: "j1", Status: StatusQueued}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Two independent readers of the same row.
	a, err := repo.Get(ctx, "j1")
	if err != nil {
		t.Fatalf("get a: %v", err)
	}
	b, err := repo.Get(ctx, "j1")
	if err != nil {
		t.Fatalf("get b: %v", err)
	}

	// Writer A advances the pipeline.
	if err := a.Transition(StatusPlanning); err != nil {
		t.Fatalf("a transition: %v", err)
	}
	if err := repo.Update(ctx, a); err != nil {
		t.Fatalf("update a: %v", err)
	}

	// Writer B still holds the stale `queued` copy and cancels. Its write is
	// accepted with no conflict error, discarding A's update entirely.
	if err := b.Transition(StatusCancelled); err != nil {
		t.Fatalf("b transition: %v", err)
	}
	if err := repo.Update(ctx, b); err != nil {
		t.Fatalf("stale update b returned %v, want nil — a conflict check was added, re-baseline this test", err)
	}

	got, err := repo.Get(ctx, "j1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != StatusCancelled {
		t.Fatalf("status = %q, want %q (last write wins)", got.Status, StatusCancelled)
	}
	t.Log("LOST UPDATE: writer A's `planning` transition was silently discarded by a stale writer")
}

// Worse: a stale writer can drag a job OUT of a terminal status. The transition
// table forbids completed->planning, but the check runs against the writer's own
// stale copy (which still says `queued`), and the repository performs an
// unconditional overwrite. A job an operator already saw as `completed` can flip
// back to a non-terminal status.
func TestResilience_StaleWriterResurrectsTerminalJob(t *testing.T) {
	repo := NewInMemoryRepository()
	ctx := context.Background()
	if err := repo.Create(ctx, &Job{ID: "j2", Status: StatusQueued}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Stale reader grabs the row before the pipeline runs.
	stale, err := repo.Get(ctx, "j2")
	if err != nil {
		t.Fatalf("get stale: %v", err)
	}

	// The real pipeline drives the job all the way to completed.
	live, _ := repo.Get(ctx, "j2")
	for _, s := range []JobStatus{
		StatusPlanning, StatusPreparingWorkspace, StatusImplementing,
		StatusVerifying, StatusReviewing, StatusCompleted,
	} {
		if err := live.Transition(s); err != nil {
			t.Fatalf("transition to %s: %v", s, err)
		}
		if err := repo.Update(ctx, live); err != nil {
			t.Fatalf("update %s: %v", s, err)
		}
	}
	if got, _ := repo.Get(ctx, "j2"); got.Status != StatusCompleted {
		t.Fatalf("setup: status = %q, want completed", got.Status)
	}

	// Stale writer wakes up and writes. queued->planning is legal on ITS copy.
	if err := stale.Transition(StatusPlanning); err != nil {
		t.Fatalf("stale transition: %v", err)
	}
	if err := repo.Update(ctx, stale); err != nil {
		t.Fatalf("stale update returned %v, want nil — a guard was added, re-baseline this test", err)
	}

	got, _ := repo.Get(ctx, "j2")
	if got.Status != StatusPlanning {
		t.Fatalf("status = %q, want %q — terminal state is now protected, re-baseline this test",
			got.Status, StatusPlanning)
	}
	t.Log("TERMINAL RESURRECTION: a completed job was rolled back to `planning` by a stale writer with no error")
}

// Under contention the damage compounds: N writers all read `queued`, all
// transition, all write, and every Update reports success even though only one
// of them can possibly be the final state.
func TestResilience_AllConcurrentUpdatesReportSuccessDespiteConflict(t *testing.T) {
	repo := NewInMemoryRepository()
	ctx := context.Background()
	if err := repo.Create(ctx, &Job{ID: "j3", Status: StatusQueued}); err != nil {
		t.Fatalf("create: %v", err)
	}

	const writers = 16

	// Phase 1: every writer reads the same `queued` row before anybody writes.
	copies := make([]*Job, writers)
	var readWG sync.WaitGroup
	for i := 0; i < writers; i++ {
		readWG.Add(1)
		go func(i int) {
			defer readWG.Done()
			j, err := repo.Get(ctx, "j3")
			if err != nil {
				return
			}
			copies[i] = j
		}(i)
	}
	readWG.Wait()

	// Phase 2: all of them write concurrently.
	var (
		writeWG  sync.WaitGroup
		mu       sync.Mutex
		accepted int
	)
	for i := 0; i < writers; i++ {
		j := copies[i]
		if j == nil {
			t.Fatalf("writer %d failed to read the job", i)
		}
		if err := j.Transition(StatusPlanning); err != nil {
			t.Fatalf("writer %d transition: %v", i, err)
		}
		writeWG.Add(1)
		go func(j *Job) {
			defer writeWG.Done()
			if err := repo.Update(ctx, j); err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}(j)
	}
	writeWG.Wait()

	if accepted != writers {
		t.Fatalf("%d of %d writes accepted — conflict detection was added, re-baseline this test", accepted, writers)
	}
	t.Logf("all %d concurrent writers holding the same stale read had their Update accepted; no version check exists", writers)
}
