package events

import (
	"context"
	"sync"
	"testing"
)

// Publish must not send on a subscriber channel that Unsubscribe may be closing
// concurrently. A WebSocket client disconnecting while a job emits an event is
// an entirely ordinary occurrence, and the publisher here is the worker
// goroutine — which has no recover() — so a "send on closed channel" panic
// takes down the whole API process along with every in-flight job.
func TestPublish_ConcurrentUnsubscribeDoesNotPanic(t *testing.T) {
	const rounds = 300

	var wg sync.WaitGroup
	panics := make(chan any, rounds*2)

	guard := func(fn func()) {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				panics <- r
			}
		}()
		fn()
	}

	for i := 0; i < rounds; i++ {
		bus := NewInProcessBus()

		// Many subscribers make the fan-out loop long enough that an
		// Unsubscribe landing partway through it closes a channel Publish has
		// snapshotted but not yet sent to.
		const subscribers = 64
		subs := make([]*Subscription, subscribers)
		for j := range subs {
			subs[j] = bus.Subscribe("job", 8)
		}

		start := make(chan struct{})
		wg.Add(2)
		go guard(func() {
			<-start
			_ = bus.Publish(context.Background(), Event{JobID: "job", Type: TypeStepCompleted})
		})
		go guard(func() {
			<-start
			for _, s := range subs {
				bus.Unsubscribe(s)
			}
		})
		close(start)
	}

	wg.Wait()
	close(panics)

	if n := len(panics); n > 0 {
		for p := range panics {
			t.Errorf("publish/unsubscribe race panicked: %v", p)
			break
		}
		t.Fatalf("%d of %d rounds panicked — a client disconnect can crash the process", n, rounds)
	}
}
