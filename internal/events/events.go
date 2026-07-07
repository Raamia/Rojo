package events

import (
	"context"
	"sync"
	"time"
)

type Event struct {
	JobID     string         `json:"job_id"`
	Type      string         `json:"type"`
	Payload   map[string]any `json:"payload,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

const (
	TypeJobCreated    = "job.created"
	TypeJobStarted    = "job.started"
	TypeJobCompleted  = "job.completed"
	TypeJobFailed     = "job.failed"
	TypeJobCancelled  = "job.cancelled"
	TypeStepStarted   = "step.started"
	TypeStepCompleted = "step.completed"

	TypePlanCreated             = "plan.created"
	TypeWorkspaceCreated        = "workspace.created"
	TypeImplementationCompleted = "implementation.completed"
	TypeVerificationCompleted   = "verification.completed"
	TypeFanoutCompleted         = "fanout.completed"
	TypeDiffCaptured            = "diff.captured"
	TypeReviewStarted           = "review.started"
	TypeReviewCompleted         = "review.completed"
	TypeRevisionRequested       = "revision.requested"
)

type Subscription struct {
	JobID  string
	C      chan Event
	closed bool
}

type Bus interface {
	Publish(ctx context.Context, e Event) error
	Subscribe(jobID string, buffer int) *Subscription
	Unsubscribe(sub *Subscription)
}

type InProcessBus struct {
	mu     sync.Mutex
	subs   map[string]map[*Subscription]struct{}
	dropCB func(jobID string)
}

func NewInProcessBus() *InProcessBus {
	return &InProcessBus{subs: make(map[string]map[*Subscription]struct{})}
}

func (b *InProcessBus) OnDrop(cb func(jobID string)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dropCB = cb
}

// stamp fills in the emission time if the caller left it unset. Every path
// that publishes an event calls this, so a stored event always carries a
// timestamp — job duration and queue wait are derived from these, and a zero
// time silently poisons both.
func stamp(e Event) Event {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	return e
}

func (b *InProcessBus) Publish(_ context.Context, e Event) error {
	e = stamp(e)
	// Fan out while still holding the lock. Unsubscribe closes sub.C, so
	// snapshotting the subscribers and sending after releasing the lock races
	// any concurrent disconnect and panics with "send on closed channel" —
	// and the publisher is usually the worker goroutine, which has no
	// recover(), so an ordinary client disconnect would take down the process.
	// Every send here is non-blocking, so the critical section stays bounded
	// and a slow subscriber still cannot stall the publisher.
	b.mu.Lock()
	cb := b.dropCB
	drops := 0
	for sub := range b.subs[e.JobID] {
		select {
		case sub.C <- e:
		default:
			drops++
		}
	}
	b.mu.Unlock()

	// Notify outside the lock: the callback is caller-supplied and must not be
	// able to deadlock the bus by re-entering it.
	if cb != nil {
		for i := 0; i < drops; i++ {
			cb(e.JobID)
		}
	}
	return nil
}

func (b *InProcessBus) Subscribe(jobID string, buffer int) *Subscription {
	if buffer <= 0 {
		buffer = 16
	}
	sub := &Subscription{JobID: jobID, C: make(chan Event, buffer)}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[jobID]; !ok {
		b.subs[jobID] = make(map[*Subscription]struct{})
	}
	b.subs[jobID][sub] = struct{}{}
	return sub
}

func (b *InProcessBus) Unsubscribe(sub *Subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if set, ok := b.subs[sub.JobID]; ok {
		delete(set, sub)
		if len(set) == 0 {
			delete(b.subs, sub.JobID)
		}
	}
	if !sub.closed {
		close(sub.C)
		sub.closed = true
	}
}
