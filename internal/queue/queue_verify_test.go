package queue

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestNewBufferSize verifies that New(n) creates a queue whose channel has the
// requested buffer capacity, i.e. it can hold n IDs before Enqueue reports full.
func TestNewBufferSize(t *testing.T) {
	const size = 4
	q := New(size)

	for i := 0; i < size; i++ {
		if err := q.Enqueue("id"); err != nil {
			t.Fatalf("Enqueue #%d on buffer of %d returned unexpected error: %v", i, size, err)
		}
	}
	if err := q.Enqueue("overflow"); err != ErrQueueFull {
		t.Fatalf("Enqueue past capacity: got err=%v, want ErrQueueFull", err)
	}
}

// TestNewClampsNonPositive verifies the documented "clamped to >= 1" behavior:
// New(0) and New(-5) must yield a usable queue that accepts at least one item.
func TestNewClampsNonPositive(t *testing.T) {
	for _, size := range []int{0, -1, -5} {
		q := New(size)
		if err := q.Enqueue("x"); err != nil {
			t.Fatalf("New(%d): first Enqueue should succeed (clamped to >=1), got %v", size, err)
		}
		// Capacity was clamped to exactly 1, so the second must be full.
		if err := q.Enqueue("y"); err != ErrQueueFull {
			t.Fatalf("New(%d): second Enqueue should be ErrQueueFull (cap clamped to 1), got %v", size, err)
		}
	}
}

// TestEnqueueUntilFull verifies Enqueue succeeds up to capacity, then returns
// ErrQueueFull, and is non-blocking (the whole test completes without hanging).
func TestEnqueueUntilFull(t *testing.T) {
	q := New(2)
	if err := q.Enqueue("a"); err != nil {
		t.Fatalf("Enqueue a: %v", err)
	}
	if err := q.Enqueue("b"); err != nil {
		t.Fatalf("Enqueue b: %v", err)
	}
	if err := q.Enqueue("c"); err != ErrQueueFull {
		t.Fatalf("Enqueue c on full queue: got %v, want ErrQueueFull", err)
	}
}

// TestDequeueFIFO verifies that IDs come back out in the same order they went in.
func TestDequeueFIFO(t *testing.T) {
	q := New(3)
	want := []string{"job-1", "job-2", "job-3"}
	for _, id := range want {
		if err := q.Enqueue(id); err != nil {
			t.Fatalf("Enqueue %s: %v", id, err)
		}
	}
	ch := q.Dequeue()
	for i, w := range want {
		got := <-ch
		if got != w {
			t.Fatalf("Dequeue position %d: got %q, want %q (FIFO violated)", i, got, w)
		}
	}
}

// TestLen verifies Len reflects the current number of buffered IDs as items are
// added and removed.
func TestLen(t *testing.T) {
	q := New(5)
	if got := q.Len(); got != 0 {
		t.Fatalf("Len of fresh queue: got %d, want 0", got)
	}
	for i := 1; i <= 3; i++ {
		if err := q.Enqueue("id"); err != nil {
			t.Fatalf("Enqueue #%d: %v", i, err)
		}
		if got := q.Len(); got != i {
			t.Fatalf("Len after %d enqueues: got %d, want %d", i, got, i)
		}
	}
	ch := q.Dequeue()
	<-ch
	if got := q.Len(); got != 2 {
		t.Fatalf("Len after one dequeue: got %d, want 2", got)
	}
}

// TestCloseDrainThenClosed verifies that after Close, remaining buffered items
// are still received (ok=true), and once drained the receive yields ok=false.
func TestCloseDrainThenClosed(t *testing.T) {
	q := New(2)
	if err := q.Enqueue("only"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	q.Close()

	ch := q.Dequeue()

	// Buffered value survives Close.
	got, ok := <-ch
	if !ok {
		t.Fatalf("receive of buffered value after Close: ok=false, want true")
	}
	if got != "only" {
		t.Fatalf("buffered value after Close: got %q, want %q", got, "only")
	}

	// Drained + closed => ok=false.
	if _, ok := <-ch; ok {
		t.Fatalf("receive on drained closed channel: ok=true, want false")
	}
}

// TestConcurrentEnqueueDequeue runs many producers and consumers against a
// shared queue and asserts that every successfully enqueued ID is received
// exactly once, with no deadlock and (under -race) no data race.
func TestConcurrentEnqueueDequeue(t *testing.T) {
	const (
		producers   = 8
		perProducer = 1000
		total       = producers * perProducer
	)
	q := New(64)

	var enqueued int64 // count of successful Enqueues (non-full)
	var received int64

	// Consumers drain until the channel is closed.
	ch := q.Dequeue()
	var consumers sync.WaitGroup
	for i := 0; i < 4; i++ {
		consumers.Add(1)
		go func() {
			defer consumers.Done()
			for range ch {
				atomic.AddInt64(&received, 1)
			}
		}()
	}

	// Producers retry on ErrQueueFull so that the accounting is exact: every
	// job ID is eventually enqueued exactly once.
	var producersWG sync.WaitGroup
	for p := 0; p < producers; p++ {
		producersWG.Add(1)
		go func() {
			defer producersWG.Done()
			for i := 0; i < perProducer; i++ {
				for {
					if err := q.Enqueue("job"); err == nil {
						atomic.AddInt64(&enqueued, 1)
						break
					}
					// Full: yield and retry. Consumers will make room.
					// (No sleep needed; the scheduler will interleave.)
				}
			}
		}()
	}

	producersWG.Wait()
	// All producers done; close so consumers exit their range loop once drained.
	q.Close()
	consumers.Wait()

	if got := atomic.LoadInt64(&enqueued); got != total {
		t.Fatalf("enqueued count: got %d, want %d", got, total)
	}
	if got := atomic.LoadInt64(&received); got != total {
		t.Fatalf("received count: got %d, want %d (lost or duplicated items)", got, total)
	}
}
