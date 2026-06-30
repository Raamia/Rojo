package events

import (
	"context"
	"fmt"
)

// Store is durable event storage. The implementation lives in
// internal/storage/filestore, which appends to a per-job log on disk.
type Store interface {
	Append(ctx context.Context, e Event) error
	History(ctx context.Context, jobID string) ([]Event, error)
}

// PersistingBus writes each event to the store before fanning it out.
//
// The ordering is deliberate: a live WebSocket subscriber must never see an
// event that is missing from the replayable history, because a client that
// reconnects would then be unable to catch up on what it saw. The trade is that
// a failed write stops the fan-out too — the error is returned so the caller
// can log it rather than losing it silently.
type PersistingBus struct {
	Inner Bus
	Store Store
}

func NewPersistingBus(inner Bus, store Store) *PersistingBus {
	return &PersistingBus{Inner: inner, Store: store}
}

func (b *PersistingBus) Publish(ctx context.Context, e Event) error {
	// Stamp before storing, not after: the inner bus would otherwise be the
	// only thing that sets the time, and the durable copy — the one the history
	// endpoint serves — would keep a zero timestamp.
	e = stamp(e)
	if err := b.Store.Append(ctx, e); err != nil {
		return fmt.Errorf("persist event %s for job %s: %w", e.Type, e.JobID, err)
	}
	return b.Inner.Publish(ctx, e)
}

func (b *PersistingBus) Subscribe(jobID string, buffer int) *Subscription {
	return b.Inner.Subscribe(jobID, buffer)
}

func (b *PersistingBus) Unsubscribe(sub *Subscription) {
	b.Inner.Unsubscribe(sub)
}
