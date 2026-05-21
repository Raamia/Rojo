package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Raamia/Rojo/internal/jobs"
)

type JobRepository struct {
	pool *pgxpool.Pool
}

func NewJobRepository(pool *pgxpool.Pool) *JobRepository {
	return &JobRepository{pool: pool}
}

const insertJobSQL = `
INSERT INTO jobs (id, task, repo_path, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6)
`

func (r *JobRepository) Create(ctx context.Context, job *jobs.Job) error {
	_, err := r.pool.Exec(ctx, insertJobSQL,
		job.ID, job.Task, job.RepoPath, string(job.Status), job.CreatedAt, job.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert job %s: %w", job.ID, err)
	}
	return nil
}

const selectJobSQL = `
SELECT id, task, repo_path, status, created_at, updated_at
FROM jobs
WHERE id = $1
`

func (r *JobRepository) Get(ctx context.Context, id string) (*jobs.Job, error) {
	row := r.pool.QueryRow(ctx, selectJobSQL, id)
	var (
		job    jobs.Job
		status string
	)
	if err := row.Scan(&job.ID, &job.Task, &job.RepoPath, &status, &job.CreatedAt, &job.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, jobs.ErrJobNotFound
		}
		return nil, fmt.Errorf("select job %s: %w", id, err)
	}
	job.Status = jobs.JobStatus(status)
	return &job, nil
}

const updateJobSQL = `
UPDATE jobs
SET task = $2, repo_path = $3, status = $4, updated_at = $5
WHERE id = $1
`

func (r *JobRepository) Update(ctx context.Context, job *jobs.Job) error {
	tag, err := r.pool.Exec(ctx, updateJobSQL,
		job.ID, job.Task, job.RepoPath, string(job.Status), job.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("update job %s: %w", job.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return jobs.ErrJobNotFound
	}
	return nil
}

const listJobsSQL = `
SELECT id, task, repo_path, status, created_at, updated_at
FROM jobs
ORDER BY created_at DESC
`

func (r *JobRepository) List(ctx context.Context) ([]*jobs.Job, error) {
	rows, err := r.pool.Query(ctx, listJobsSQL)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	var out []*jobs.Job
	for rows.Next() {
		var (
			job    jobs.Job
			status string
		)
		if err := rows.Scan(&job.ID, &job.Task, &job.RepoPath, &status, &job.CreatedAt, &job.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		job.Status = jobs.JobStatus(status)
		out = append(out, &job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate jobs: %w", err)
	}
	return out, nil
}
