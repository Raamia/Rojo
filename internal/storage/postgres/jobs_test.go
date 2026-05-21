package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/jobs"
)

func testPool(t *testing.T) *JobRepository {
	t.Helper()
	url := os.Getenv("ROJO_TEST_DB_URL")
	if url == "" {
		t.Skip("ROJO_TEST_DB_URL not set, skipping postgres integration test")
	}
	pool, err := NewPool(context.Background(), Config{URL: url, MaxConns: 4})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	if _, err := pool.Exec(context.Background(), "TRUNCATE jobs CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return NewJobRepository(pool)
}

func TestPostgres_CreateAndGet(t *testing.T) {
	repo := testPool(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Millisecond)
	job := &jobs.Job{
		ID:        "job-1",
		Task:      "do the thing",
		RepoPath:  "/tmp/repo",
		Status:    jobs.StatusQueued,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := repo.Create(ctx, job); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := repo.Get(ctx, "job-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Task != job.Task || got.Status != job.Status {
		t.Errorf("got %+v, want task=%s status=%s", got, job.Task, job.Status)
	}
}

func TestPostgres_GetMissing(t *testing.T) {
	repo := testPool(t)
	_, err := repo.Get(context.Background(), "missing")
	if !errors.Is(err, jobs.ErrJobNotFound) {
		t.Fatalf("got %v, want ErrJobNotFound", err)
	}
}

func TestPostgres_UpdateMissing(t *testing.T) {
	repo := testPool(t)
	err := repo.Update(context.Background(), &jobs.Job{ID: "nope", Status: jobs.StatusQueued})
	if !errors.Is(err, jobs.ErrJobNotFound) {
		t.Fatalf("got %v, want ErrJobNotFound", err)
	}
}

func TestPostgres_List(t *testing.T) {
	repo := testPool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i, id := range []string{"a", "b", "c"} {
		job := &jobs.Job{
			ID:        id,
			Task:      "t",
			RepoPath:  "/r",
			Status:    jobs.StatusQueued,
			CreatedAt: now.Add(time.Duration(i) * time.Second),
			UpdatedAt: now.Add(time.Duration(i) * time.Second),
		}
		if err := repo.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	got, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	if got[0].ID != "c" {
		t.Errorf("expected newest first, got %s", got[0].ID)
	}
}
