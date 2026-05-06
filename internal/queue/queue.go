package queue

import "errors"

var ErrQueueFull = errors.New("queue is full")

type Queue struct {
	ch chan string
}

func New(bufferSize int) *Queue {
	if bufferSize <= 0 {
		bufferSize = 1
	}
	return &Queue{ch: make(chan string, bufferSize)}
}

func (q *Queue) Enqueue(jobID string) error {
	select {
	case q.ch <- jobID:
		return nil
	default:
		return ErrQueueFull
	}
}

func (q *Queue) Dequeue() <-chan string {
	return q.ch
}

func (q *Queue) Close() {
	close(q.ch)
}

func (q *Queue) Len() int {
	return len(q.ch)
}
