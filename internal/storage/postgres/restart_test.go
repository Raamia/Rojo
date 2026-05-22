package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/jobs"
)

func TestPostgres_JobSurvivesReconnect(t *testing.T) {
	url := os.Getenv("ROJO_TEST_DB_URL")
	if url == "" {
		t.Skip("ROJO_TEST_DB_URL not set, skipping postgres integration test")
	}
	ctx := context.Background()

	first, err := NewPool(ctx, Config{URL: url, MaxConns: 4})
	if err != nil {
		t.Fatalf("connect first: %v", err)
	}
	if _, err := first.Exec(ctx, "TRUNCATE jobs CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	repo := NewJobRepository(first)

	now := time.Now().UTC().Truncate(time.Millisecond)
	job := &jobs.Job{
		ID:        "persist-1",
		Task:      "persist across reconnect",
		RepoPath:  "/tmp/repo",
		Status:    jobs.StatusQueued,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := repo.Create(ctx, job); err != nil {
		t.Fatalf("create: %v", err)
	}
	first.Close()

	second, err := NewPool(ctx, Config{URL: url, MaxConns: 4})
	if err != nil {
		t.Fatalf("connect second: %v", err)
	}
	defer second.Close()

	got, err := NewJobRepository(second).Get(ctx, "persist-1")
	if err != nil {
		t.Fatalf("get after reconnect: %v", err)
	}
	if got.Task != "persist across reconnect" {
		t.Errorf("got task %q, want persistence", got.Task)
	}
}
