package worker

import (
	"context"
	"log/slog"
	"runtime/debug"
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
			p.process(ctx, log, jobID)
		}
	}
}

// process runs one job and survives whatever it does.
//
// Processor.Process already turns a panic into an ordinary job failure; this is
// the backstop for anything that escapes it, including a panic raised inside
// that recovery path. Without it an unrecovered panic unwinds through this
// goroutine and takes the whole process down, killing every other in-flight job
// and every worker — one malformed model response would end the service.
//
// The job may be left in a non-terminal state when the inner recovery is what
// failed; startup recovery is what reconciles that.
func (p *Pool) process(ctx context.Context, log *slog.Logger, jobID string) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("worker recovered from panic", "job_id", jobID,
				"panic", r, "stack", string(debug.Stack()))
		}
	}()
	if err := p.processor.Process(ctx, jobID); err != nil {
		log.Error("process job", "job_id", jobID, "err", err)
	}
}

func (p *Pool) Wait() {
	p.wg.Wait()
}
