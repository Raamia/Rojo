package events

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestBus_PublishAndSubscribe(t *testing.T) {
	bus := NewInProcessBus()
	sub := bus.Subscribe("job-1", 4)
	defer bus.Unsubscribe(sub)

	if err := bus.Publish(context.Background(), Event{JobID: "job-1", Type: TypeJobStarted}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case evt := <-sub.C:
		if evt.Type != TypeJobStarted {
			t.Errorf("got type %s, want %s", evt.Type, TypeJobStarted)
		}
	case <-time.After(time.Second):
		t.Fatal("no event received")
	}
}

func TestBus_SubscribersScopedToJob(t *testing.T) {
	bus := NewInProcessBus()
	subA := bus.Subscribe("a", 4)
	subB := bus.Subscribe("b", 4)
	defer bus.Unsubscribe(subA)
	defer bus.Unsubscribe(subB)

	_ = bus.Publish(context.Background(), Event{JobID: "a", Type: TypeJobStarted})

	select {
	case <-subA.C:
	case <-time.After(time.Second):
		t.Fatal("subA did not receive event")
	}
	select {
	case evt := <-subB.C:
		t.Errorf("subB should not have received %v", evt)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBus_SlowSubscriberDoesNotBlockPublisher(t *testing.T) {
	bus := NewInProcessBus()
	var dropped atomic.Int64
	bus.OnDrop(func(string) { dropped.Add(1) })

	sub := bus.Subscribe("job-slow", 2)
	defer bus.Unsubscribe(sub)

	publishStart := time.Now()
	for i := 0; i < 20; i++ {
		if err := bus.Publish(context.Background(), Event{JobID: "job-slow", Type: TypeStepCompleted}); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	elapsed := time.Since(publishStart)
	if elapsed > 500*time.Millisecond {
		t.Errorf("publisher blocked for %v (expected fast fail on full subscriber)", elapsed)
	}
	if dropped.Load() == 0 {
		t.Error("expected some events dropped for slow subscriber")
	}
}
