package events

import (
	"context"
	"testing"
	"time"
)

type capturingStore struct{ got []Event }

func (c *capturingStore) Append(_ context.Context, e Event) error {
	c.got = append(c.got, e)
	return nil
}
func (c *capturingStore) History(context.Context, string) ([]Event, error) { return nil, nil }

// The persisted copy is what the history endpoint serves and what any future
// duration or queue-wait metric is computed from. It must carry a real time —
// storing before the inner bus stamped it left every stored event at the zero
// time, which looks fine until something tries to measure with it.
func TestPersistingBus_StampsBeforeStoring(t *testing.T) {
	store := &capturingStore{}
	bus := NewPersistingBus(NewInProcessBus(), store)

	before := time.Now().UTC()
	if err := bus.Publish(context.Background(), Event{JobID: "j1", Type: TypeJobStarted}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if len(store.got) != 1 {
		t.Fatalf("stored %d events, want 1", len(store.got))
	}
	stored := store.got[0]
	if stored.CreatedAt.IsZero() {
		t.Fatal("stored event has a zero timestamp")
	}
	if stored.CreatedAt.Before(before) {
		t.Errorf("stored timestamp %v predates the publish", stored.CreatedAt)
	}
}

// A caller that sets its own time keeps it — stamping only fills a gap.
func TestPersistingBus_PreservesAnExplicitTimestamp(t *testing.T) {
	store := &capturingStore{}
	bus := NewPersistingBus(NewInProcessBus(), store)
	want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	if err := bus.Publish(context.Background(), Event{JobID: "j1", Type: TypeJobStarted, CreatedAt: want}); err != nil {
		t.Fatal(err)
	}
	if !store.got[0].CreatedAt.Equal(want) {
		t.Errorf("timestamp = %v, want the caller's %v", store.got[0].CreatedAt, want)
	}
}

func TestInProcessBus_StampsSubscriberCopy(t *testing.T) {
	bus := NewInProcessBus()
	sub := bus.Subscribe("j1", 4)
	defer bus.Unsubscribe(sub)

	if err := bus.Publish(context.Background(), Event{JobID: "j1", Type: TypeJobStarted}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-sub.C:
		if got.CreatedAt.IsZero() {
			t.Error("subscriber received a zero timestamp")
		}
	case <-time.After(time.Second):
		t.Fatal("no event delivered")
	}
}
