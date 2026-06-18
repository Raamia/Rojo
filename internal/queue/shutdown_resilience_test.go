package queue

// Resilience characterization tests for the queue's shutdown semantics.
// Assertions document ACTUAL behavior, including behavior that is a bug.

import (
	"testing"
)

// ---------------------------------------------------------------------------
// FAILURE MODE 1 — Close() races with late producers and panics
// ---------------------------------------------------------------------------

// cmd/api/main.go:104-110 calls srv.Shutdown, then q.Close(). Shutdown returns
// an error (rather than blocking) once ShutdownTimeout elapses, and main
// ignores that error and closes the queue anyway. Any HTTP handler still inside
// JobsHandler.Create then sends on a closed channel and panics. net/http
// recovers per-connection, so the symptom is a 500 plus a stack trace, and the
// job row has already been INSERTed as `queued` with nothing to run it.
func TestResilience_EnqueueAfterClosePanics(t *testing.T) {
	q := New(4)
	q.Close()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Enqueue after Close did not panic — Close is now guarded, re-baseline this test")
		}
		t.Logf("CONFIRMED: Enqueue after Close panics with %v", r)
	}()

	_ = q.Enqueue("late-job") // panic: send on closed channel
}

// Close is likewise not idempotent. Only one caller closes today, but any future
// second shutdown path (or a retry after a failed shutdown) crashes the process.
func TestResilience_DoubleClosePanics(t *testing.T) {
	q := New(1)
	q.Close()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("second Close did not panic — Close is now idempotent, re-baseline this test")
		}
	}()

	q.Close() // panic: close of closed channel
}

// ---------------------------------------------------------------------------
// FAILURE MODE 2 — buffered jobs vanish with the process
// ---------------------------------------------------------------------------

// Close() does not drain, persist, or report the IDs still buffered. They stay
// in the channel until the process exits and are then gone. The corresponding
// rows remain `queued` in postgres with no requeue path.
func TestResilience_CloseAbandonsBufferedJobsWithoutReporting(t *testing.T) {
	const buffered = 5
	q := New(64) // default ROJO_QUEUE_BUFFER

	for i := 0; i < buffered; i++ {
		if err := q.Enqueue("job"); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	q.Close()

	if got := q.Len(); got != buffered {
		t.Fatalf("Len after Close = %d, want %d — Close now drains, re-baseline this test", got, buffered)
	}
	t.Logf("CONFIRMED: %d job IDs remain buffered and unreported after Close; worst case is the full buffer (default 64)", buffered)
}
