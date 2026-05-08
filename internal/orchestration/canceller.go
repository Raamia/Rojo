package orchestration

import (
	"context"
	"errors"
	"sync"
)

var ErrJobNotRunning = errors.New("job is not currently running")

type Canceller struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func NewCanceller() *Canceller {
	return &Canceller{cancels: make(map[string]context.CancelFunc)}
}

func (c *Canceller) Track(jobID string, cancel context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancels[jobID] = cancel
}

func (c *Canceller) Release(jobID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cancels, jobID)
}

func (c *Canceller) Cancel(jobID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cancel, ok := c.cancels[jobID]
	if !ok {
		return ErrJobNotRunning
	}
	cancel()
	delete(c.cancels, jobID)
	return nil
}
