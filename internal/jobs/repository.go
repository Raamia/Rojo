package jobs

import (
	"context"
	"errors"
	"sync"
)

var (
	ErrJobNotFound      = errors.New("job not found")
	ErrJobAlreadyExists = errors.New("job already exists")
)

type JobRepository interface {
	Create(ctx context.Context, job *Job) error
	Get(ctx context.Context, id string) (*Job, error)
	Update(ctx context.Context, job *Job) error
	List(ctx context.Context) ([]*Job, error)
}

type InMemoryRepository struct {
	mu   sync.RWMutex
	jobs map[string]*Job
}

func NewInMemoryRepository() *InMemoryRepository {
	return &InMemoryRepository{jobs: make(map[string]*Job)}
}

func (r *InMemoryRepository) Create(_ context.Context, job *Job) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.jobs[job.ID]; ok {
		return ErrJobAlreadyExists
	}
	r.jobs[job.ID] = cloneJob(job)
	return nil
}

func (r *InMemoryRepository) Get(_ context.Context, id string) (*Job, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	j, ok := r.jobs[id]
	if !ok {
		return nil, ErrJobNotFound
	}
	return cloneJob(j), nil
}

func (r *InMemoryRepository) Update(_ context.Context, job *Job) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.jobs[job.ID]; !ok {
		return ErrJobNotFound
	}
	r.jobs[job.ID] = cloneJob(job)
	return nil
}

func (r *InMemoryRepository) List(_ context.Context) ([]*Job, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Job, 0, len(r.jobs))
	for _, j := range r.jobs {
		out = append(out, cloneJob(j))
	}
	return out, nil
}

func cloneJob(j *Job) *Job {
	c := *j
	return &c
}
