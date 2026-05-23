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
	TypeJobCreated             = "job.created"
	TypeJobStarted             = "job.started"
	TypeJobCompleted           = "job.completed"
	TypeJobFailed              = "job.failed"
	TypeJobCancelled           = "job.cancelled"
	TypeStepStarted            = "step.started"
	TypeStepCompleted          = "step.completed"
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

func (b *InProcessBus) Publish(_ context.Context, e Event) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	b.mu.Lock()
	targets := make([]*Subscription, 0, len(b.subs[e.JobID]))
	for sub := range b.subs[e.JobID] {
		targets = append(targets, sub)
	}
	cb := b.dropCB
	b.mu.Unlock()

	for _, sub := range targets {
		select {
		case sub.C <- e:
		default:
			if cb != nil {
				cb(e.JobID)
			}
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
