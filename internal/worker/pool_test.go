package worker

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/queue"
)

type countingProcessor struct {
	count atomic.Int64
	delay time.Duration
}

func (p *countingProcessor) Process(ctx context.Context, _ string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(p.delay):
		p.count.Add(1)
		return nil
	}
}

func TestPool_ProcessesEnqueuedJobs(t *testing.T) {
	q := queue.New(16)
	p := &countingProcessor{delay: time.Millisecond}
	pool := NewPool(4, q, p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)

	const n = 20
	for i := 0; i < n; i++ {
		if err := q.Enqueue(fmt.Sprintf("job-%d", i)); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.count.Load() == n {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := p.count.Load(); got != n {
		t.Fatalf("processed %d jobs, want %d", got, n)
	}
}

func TestPool_ShutdownDrainsInFlight(t *testing.T) {
	q := queue.New(16)
	p := &countingProcessor{delay: 20 * time.Millisecond}
	pool := NewPool(2, q, p)

	ctx, cancel := context.WithCancel(context.Background())
	pool.Start(ctx)

	for i := 0; i < 4; i++ {
		_ = q.Enqueue(fmt.Sprintf("job-%d", i))
	}
	time.Sleep(5 * time.Millisecond)

	cancel()
	q.Close()

	done := make(chan struct{})
	go func() {
		pool.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("workers did not stop after context cancel")
	}
}

func TestPool_ConcurrentEnqueueAndProcess(t *testing.T) {
	q := queue.New(64)
	p := &countingProcessor{delay: 0}
	pool := NewPool(8, q, p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)

	var wg sync.WaitGroup
	const producers = 4
	const each = 50
	for i := 0; i < producers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < each; j++ {
				for {
					if err := q.Enqueue(fmt.Sprintf("%d-%d", id, j)); err == nil {
						break
					}
					time.Sleep(time.Millisecond)
				}
			}
		}(i)
	}
	wg.Wait()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p.count.Load() == producers*each {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("got %d processed, want %d", p.count.Load(), producers*each)
}
