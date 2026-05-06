package worker

import (
	"context"
	"log/slog"
	"sync"

	"github.com/Raamia/Rojo/internal/queue"
)

type JobProcessor interface {
	Process(ctx context.Context, jobID string) error
}

type Pool struct {
	size      int
	queue     *queue.Queue
	processor JobProcessor
	wg        sync.WaitGroup
}

func NewPool(size int, q *queue.Queue, p JobProcessor) *Pool {
	if size <= 0 {
		size = 1
	}
	return &Pool{size: size, queue: q, processor: p}
}

func (p *Pool) Start(ctx context.Context) {
	for i := 0; i < p.size; i++ {
		p.wg.Add(1)
		go p.runWorker(ctx, i)
	}
}

func (p *Pool) runWorker(ctx context.Context, id int) {
	defer p.wg.Done()
	log := slog.With("worker", id)
	for {
		select {
		case <-ctx.Done():
			log.Info("worker stopping", "reason", ctx.Err())
			return
		case jobID, ok := <-p.queue.Dequeue():
			if !ok {
				log.Info("queue closed, worker stopping")
				return
			}
			if err := p.processor.Process(ctx, jobID); err != nil {
				log.Error("process job", "job_id", jobID, "err", err)
			}
		}
	}
}

func (p *Pool) Wait() {
	p.wg.Wait()
}
