package events

// Resilience characterization tests for PersistingBus failure behavior.
// Assertions document ACTUAL behavior, including behavior that is a bug.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type resFlakyStore struct {
	mu       sync.Mutex
	appended []Event
	failWith error
}

func (s *resFlakyStore) Append(_ context.Context, e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failWith != nil {
		return s.failWith
	}
	s.appended = append(s.appended, e)
	return nil
}

func (s *resFlakyStore) History(_ context.Context, _ string) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.appended))
	copy(out, s.appended)
	return out, nil
}

func (s *resFlakyStore) setFailure(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failWith = err
}

// ---------------------------------------------------------------------------
// FAILURE MODE 8 — a failed event write also kills the live fan-out
// ---------------------------------------------------------------------------

// PersistingBus.Publish (store.go:85-90) returns as soon as Store.Append fails
// and never reaches Inner.Publish. Persistence and delivery are coupled: one
// bad DB write means the live WebSocket stream misses that event too. The
// connection stays open, so the client sees a silent gap, not an error.
func TestResilience_PersistingBusStoreFailureAlsoBlocksLiveFanout(t *testing.T) {
	inner := NewInProcessBus()
	store := &resFlakyStore{failWith: errors.New("events table unavailable")}
	bus := NewPersistingBus(inner, store)

	sub := bus.Subscribe("job-1", 8)
	defer bus.Unsubscribe(sub)

	err := bus.Publish(context.Background(), Event{JobID: "job-1", Type: TypeJobStarted})
	if err == nil {
		t.Fatal("Publish returned nil despite a store failure — re-baseline this test")
	}

	select {
	case e := <-sub.C:
		t.Fatalf("subscriber received %q — fan-out now survives store failure, re-baseline this test", e.Type)
	case <-time.After(100 * time.Millisecond):
		// Expected: nothing was delivered.
	}
}

// The failure is not merely delayed — the event is gone from history as well,
// so the /events replay endpoint cannot fill the gap for a reconnecting client.
func TestResilience_EventLostFromBothStreamAndHistory(t *testing.T) {
	inner := NewInProcessBus()
	store := &resFlakyStore{}
	bus := NewPersistingBus(inner, store)

	sub := bus.Subscribe("job-2", 8)
	defer bus.Unsubscribe(sub)

	if err := bus.Publish(context.Background(), Event{JobID: "job-2", Type: TypeJobStarted}); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	// Transient DB failure for one event only.
	store.setFailure(errors.New("deadlock detected"))
	_ = bus.Publish(context.Background(), Event{JobID: "job-2", Type: TypeStepStarted})
	store.setFailure(nil)

	if err := bus.Publish(context.Background(), Event{JobID: "job-2", Type: TypeJobCompleted}); err != nil {
		t.Fatalf("third publish: %v", err)
	}

	history, err := store.History(context.Background(), "job-2")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("history has %d events, want 2 — a retry was added, re-baseline this test", len(history))
	}
	for _, e := range history {
		if e.Type == TypeStepStarted {
			t.Fatal("the failed event made it into history — re-baseline this test")
		}
	}

	var delivered []string
	for {
		select {
		case e := <-sub.C:
			delivered = append(delivered, e.Type)
			continue
		default:
		}
		break
	}
	if len(delivered) != 2 {
		t.Fatalf("subscriber got %v, want 2 events (the middle one is lost)", delivered)
	}
	t.Log("CONFIRMED: a single failed DB write permanently loses the event from BOTH the live stream and the replay history, with no retry and no error surfaced to the client")
}

// ---------------------------------------------------------------------------
// FAILURE MODE 8 (critical) — Unsubscribe races Publish and panics the publisher
// ---------------------------------------------------------------------------

// InProcessBus.Publish snapshots its subscriber list under b.mu, RELEASES the
// lock (events.go:64), and only then sends on sub.C (events.go:68).
// Unsubscribe closes sub.C (events.go:102). A subscriber that goes away inside
// that window makes the publisher send on a closed channel and panic.
//
// In production the publisher is the worker goroutine (Processor.emit ->
// Bus.Publish) and the unsubscriber is an HTTP handler
// (api/stream.go:45 `defer h.Bus.Unsubscribe(sub)`), so a WebSocket client
// disconnecting while its job is emitting an event panics a goroutine that
// nothing recovers — taking the entire API process down.
//
// FIXED: Publish now fans out while holding b.mu, so Unsubscribe cannot close a
// channel mid-send, and the drop callback is invoked only after the lock is
// released so a callback that re-enters the bus cannot deadlock it.
//
// This test keeps the original adversarial setup — an OnDrop callback that
// unsubscribes another subscriber, i.e. re-entrant use of the bus from inside a
// publish — and asserts it neither panics nor hangs.
func TestResilience_UnsubscribeDuringPublishIsSafe(t *testing.T) {
	for attempt := 0; attempt < 64; attempt++ {
		bus := NewInProcessBus()

		victim := bus.Subscribe("job-1", 8)  // still open when Publish reaches it
		trigger := bus.Subscribe("job-1", 1) // buffer pre-filled so its send hits `default`
		trigger.C <- Event{}

		bus.OnDrop(func(string) { bus.Unsubscribe(victim) })

		done := make(chan any, 1)
		go func() {
			defer func() { done <- recover() }()
			_ = bus.Publish(context.Background(), Event{JobID: "job-1", Type: TypeStepStarted})
		}()

		select {
		case r := <-done:
			if r != nil {
				t.Fatalf("attempt %d: Publish panicked with %v; a subscriber going away "+
					"mid-fan-out must not crash the publisher", attempt, r)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("attempt %d: Publish deadlocked — a drop callback that re-enters "+
				"the bus must not be invoked while the lock is held", attempt)
		}
	}
}

// There is no dead-letter path or drop counter on the persistence side: the
// only drop signal the bus offers is InProcessBus.OnDrop, which fires for slow
// subscribers and is never wired up in cmd/api/main.go.
func TestResilience_NoDropSignalForFailedPersistence(t *testing.T) {
	inner := NewInProcessBus()
	dropped := 0
	inner.OnDrop(func(string) { dropped++ })

	bus := NewPersistingBus(inner, &resFlakyStore{failWith: errors.New("db down")})
	_ = bus.Publish(context.Background(), Event{JobID: "job-3", Type: TypeJobStarted})

	if dropped != 0 {
		t.Fatalf("OnDrop fired %d times — persistence drops are now reported, re-baseline this test", dropped)
	}
	t.Log("CONFIRMED: events dropped by a store failure are invisible to OnDrop and to every caller that discards Publish's error")
}
