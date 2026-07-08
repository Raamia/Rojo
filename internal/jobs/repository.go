package jobs

import (
	"context"
	"errors"
	"sort"
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

// List returns every job, newest first.
//
// The order is part of the interface, not an accident of storage. This used to
// return whatever order the map happened to iterate in, which Go deliberately
// randomises: the same request twice gave two different orderings, and paging
// over it made consecutive pages overlap and skip. The filesystem store has
// always sorted this way, so both implementations now answer the same question
// the same way.
func (r *InMemoryRepository) List(_ context.Context) ([]*Job, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Job, 0, len(r.jobs))
	for _, j := range r.jobs {
		out = append(out, cloneJob(j))
	}
	sort.Slice(out, func(i, k int) bool {
		if out[i].CreatedAt.Equal(out[k].CreatedAt) {
			return out[i].ID < out[k].ID // stable tie-break
		}
		return out[i].CreatedAt.After(out[k].CreatedAt)
	})
	return out, nil
}

func cloneJob(j *Job) *Job {
	c := *j
	return &c
}
