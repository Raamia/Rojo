package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/orchestration"
)

// One Processor serves every worker, so anything per-job kept on it would be
// shared between unrelated jobs. These run several complete jobs through a
// single Processor at once and check that each one's plan, patch and review
// stayed its own — the failure mode being a job that quietly returns another
// job's work, which no single-job test can catch.

// perJobAnthropic answers according to which job the prompt belongs to, so a
// crossed wire produces the wrong patch rather than merely a race report.
type perJobAnthropic struct {
	*httptest.Server
	mu    sync.Mutex
	calls int
}

func newPerJobAnthropic(t *testing.T) *perJobAnthropic {
	t.Helper()
	f := &perJobAnthropic{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		prompt := string(body)

		f.mu.Lock()
		f.calls++
		f.mu.Unlock()

		// The task carries a marker like "widget-3"; every reply for that job
		// is stamped with it.
		marker := markerIn(prompt)

		var reply string
		switch {
		case strings.Contains(prompt, "Rojo implementor"):
			reply = fmt.Sprintf(
				`{"operations":[{"kind":"write","path":"%s.go","content":"package main\n\n// %s\n"}]}`,
				marker, marker)
		case strings.Contains(prompt, "Rojo reviewer"):
			reply = `{"decision":"approve","notes":"fine"}`
		default: // planner
			reply = fmt.Sprintf(
				`{"summary":"plan for %s","steps":[{"id":"s1","description":"write %s.go"}]}`,
				marker, marker)
		}

		out, _ := json.Marshal(map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant",
			"model":       "claude-opus-4-8",
			"content":     []map[string]any{{"type": "text", "text": reply}},
			"stop_reason": "end_turn",
			"usage":       map[string]int{"input_tokens": 5, "output_tokens": 5},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	t.Cleanup(f.Close)
	return f
}

// markerIn finds the "widget-N" token the task was submitted with.
func markerIn(prompt string) string {
	i := strings.Index(prompt, "widget-")
	if i < 0 {
		return "unknown"
	}
	end := i + len("widget-")
	for end < len(prompt) && prompt[end] >= '0' && prompt[end] <= '9' {
		end++
	}
	return prompt[i:end]
}

func TestConcurrent_JobsDoNotCrossWires(t *testing.T) {
	const jobCount = 6

	repo := pipelineRepoNoTest(t)
	fake := newPerJobAnthropic(t)
	p, store, _ := buildPipeline(t, fake.URL)
	// The probe repo compiles on its own, so the gate passes for every job and
	// each one reaches review — which is the point: all stages run at once.

	for i := 0; i < jobCount; i++ {
		submit(t, store, fmt.Sprintf("job%d", i),
			fmt.Sprintf("create the widget-%d component", i), repo)
	}

	var wg sync.WaitGroup
	errs := make([]error, jobCount)
	for i := 0; i < jobCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = p.Process(context.Background(), fmt.Sprintf("job%d", i))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("job%d: %v", i, err)
		}
	}

	for i := 0; i < jobCount; i++ {
		id := fmt.Sprintf("job%d", i)
		marker := fmt.Sprintf("widget-%d", i)

		got, err := store.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got.Status != jobs.StatusCompleted {
			t.Errorf("%s status = %q, want completed", id, got.Status)
		}

		patch, err := store.ReadArtifact(id, orchestration.ArtifactDiff)
		if err != nil {
			t.Errorf("%s has no patch: %v", id, err)
			continue
		}
		// The decisive assertion: this job's patch is this job's work.
		if !strings.Contains(string(patch), marker) {
			t.Errorf("%s got a patch that is not its own:\n%s", id, patch)
		}
		for k := 0; k < jobCount; k++ {
			if k == i {
				continue
			}
			if strings.Contains(string(patch), fmt.Sprintf("widget-%d.go", k)) {
				t.Errorf("%s's patch contains job%d's file — state crossed between jobs", id, k)
			}
		}
	}
}

// Each job's event log must contain only its own events, or a client watching
// one job would see another's progress.
func TestConcurrent_EventLogsStaySeparate(t *testing.T) {
	const jobCount = 4

	repo := pipelineRepoNoTest(t)
	fake := newPerJobAnthropic(t)
	p, store, _ := buildPipeline(t, fake.URL)

	for i := 0; i < jobCount; i++ {
		submit(t, store, fmt.Sprintf("ev%d", i),
			fmt.Sprintf("create the widget-%d component", i), repo)
	}

	var wg sync.WaitGroup
	for i := 0; i < jobCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = p.Process(context.Background(), fmt.Sprintf("ev%d", i))
		}(i)
	}
	wg.Wait()

	for i := 0; i < jobCount; i++ {
		id := fmt.Sprintf("ev%d", i)
		history, err := store.History(context.Background(), id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if len(history) == 0 {
			t.Errorf("%s has no events", id)
		}
		for _, e := range history {
			if e.JobID != id {
				t.Errorf("%s's log contains an event for %s", id, e.JobID)
			}
		}
	}
}

// Cancelling one job must not disturb the others sharing the Processor.
func TestConcurrent_CancellingOneJobLeavesTheRestAlone(t *testing.T) {
	repo := pipelineRepoNoTest(t)
	fake := newPerJobAnthropic(t)
	p, store, _ := buildPipeline(t, fake.URL)

	for i := 0; i < 4; i++ {
		submit(t, store, fmt.Sprintf("c%d", i),
			fmt.Sprintf("create the widget-%d component", i), repo)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = p.Process(context.Background(), fmt.Sprintf("c%d", i))
		}(i)
	}

	// Cancel one mid-flight. The canceller is keyed by job id, so only that one
	// should stop.
	go func() {
		for i := 0; i < 200; i++ {
			if err := p.Canceller.Cancel("c0"); err == nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	wg.Wait()

	for i := 1; i < 4; i++ {
		id := fmt.Sprintf("c%d", i)
		got, err := store.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got.Status == jobs.StatusCancelled {
			t.Errorf("%s was cancelled, but only c0 was asked to stop", id)
		}
	}
}
