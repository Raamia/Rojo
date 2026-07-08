package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInMemoryRepository_CreateAndGet(t *testing.T) {
	repo := NewInMemoryRepository()
	ctx := context.Background()

	job := &Job{ID: "1", Task: "do the thing", RepoPath: "/r", Status: StatusQueued}
	if err := repo.Create(ctx, job); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.Get(ctx, "1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Task != "do the thing" {
		t.Errorf("got task %s, want 'do the thing'", got.Task)
	}
}

func TestInMemoryRepository_GetMissing(t *testing.T) {
	repo := NewInMemoryRepository()
	_, err := repo.Get(context.Background(), "nope")
	if !errors.Is(err, ErrJobNotFound) {
		t.Errorf("got %v, want ErrJobNotFound", err)
	}
}

func TestInMemoryRepository_DuplicateCreate(t *testing.T) {
	repo := NewInMemoryRepository()
	ctx := context.Background()
	job := &Job{ID: "1"}
	if err := repo.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	err := repo.Create(ctx, job)
	if !errors.Is(err, ErrJobAlreadyExists) {
		t.Errorf("got %v, want ErrJobAlreadyExists", err)
	}
}

func TestInMemoryRepository_Update(t *testing.T) {
	repo := NewInMemoryRepository()
	ctx := context.Background()
	job := &Job{ID: "1", Status: StatusQueued}
	_ = repo.Create(ctx, job)

	job.Status = StatusPlanning
	if err := repo.Update(ctx, job); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := repo.Get(ctx, "1")
	if got.Status != StatusPlanning {
		t.Errorf("got %s, want planning", got.Status)
	}
}

func TestInMemoryRepository_UpdateMissing(t *testing.T) {
	repo := NewInMemoryRepository()
	err := repo.Update(context.Background(), &Job{ID: "nope"})
	if !errors.Is(err, ErrJobNotFound) {
		t.Errorf("got %v, want ErrJobNotFound", err)
	}
}

func TestInMemoryRepository_ClonesOnGet(t *testing.T) {
	repo := NewInMemoryRepository()
	ctx := context.Background()
	_ = repo.Create(ctx, &Job{ID: "1", Task: "original"})

	got, _ := repo.Get(ctx, "1")
	got.Task = "mutated"

	fresh, _ := repo.Get(ctx, "1")
	if fresh.Task != "original" {
		t.Errorf("expected clone isolation, got %s", fresh.Task)
	}
}

func TestInMemoryRepository_ConcurrentAccess(t *testing.T) {
	repo := NewInMemoryRepository()
	ctx := context.Background()
	const writers = 20
	const perWriter = 50

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				job := &Job{ID: fmt.Sprintf("%d-%d", id, i), Status: StatusQueued}
				if err := repo.Create(ctx, job); err != nil {
					t.Errorf("create: %v", err)
				}
				if _, err := repo.Get(ctx, job.ID); err != nil {
					t.Errorf("get: %v", err)
				}
			}
		}(w)
	}

	for r := 0; r < writers/2; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				_, _ = repo.List(ctx)
			}
		}()
	}

	wg.Wait()

	all, _ := repo.List(ctx)
	if len(all) != writers*perWriter {
		t.Errorf("got %d jobs, want %d", len(all), writers*perWriter)
	}
}

// The order List returns is part of the interface, not an accident of storage.
// Map iteration in Go is deliberately randomised, so a repository that just
// ranges over its map answers the same request differently every time — and
// paging over that makes consecutive pages overlap and skip records.
func TestInMemoryRepository_ListIsNewestFirstAndStable(t *testing.T) {
	repo := NewInMemoryRepository()
	ctx := context.Background()
	base := time.Now().UTC()

	for i, id := range []string{"oldest", "middle", "newest"} {
		j := &Job{
			ID: id, Task: "t", RepoPath: "/tmp/repo", Status: StatusQueued,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
			UpdatedAt: base,
		}
		if err := repo.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}

	want := "newest,middle,oldest"
	// Several times over: one lucky ordering from a map range would otherwise
	// pass by chance.
	for i := 0; i < 20; i++ {
		list, err := repo.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, j := range list {
			ids = append(ids, j.ID)
		}
		if got := strings.Join(ids, ","); got != want {
			t.Fatalf("run %d: order = %s, want %s", i, got, want)
		}
	}
}

// Jobs created in the same instant still need one definitive order.
func TestInMemoryRepository_ListTieBreaksOnID(t *testing.T) {
	repo := NewInMemoryRepository()
	ctx := context.Background()
	at := time.Now().UTC()

	for _, id := range []string{"c", "a", "b"} {
		if err := repo.Create(ctx, &Job{
			ID: id, Task: "t", RepoPath: "/tmp/repo", Status: StatusQueued,
			CreatedAt: at, UpdatedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 20; i++ {
		list, _ := repo.List(ctx)
		var ids []string
		for _, j := range list {
			ids = append(ids, j.ID)
		}
		if got := strings.Join(ids, ","); got != "a,b,c" {
			t.Fatalf("run %d: order = %s, want a,b,c", i, got)
		}
	}
}
