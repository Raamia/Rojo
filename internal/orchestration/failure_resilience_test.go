package orchestration

// Resilience / failure-handling characterization tests.
//
// Every test here asserts the ACTUAL current behavior of the system, including
// behavior that is a bug. Test names describe the observed behavior so that a
// future fix will make the test fail loudly and force it to be re-baselined.
//
// stdlib + existing internal packages only; no new dependencies.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
)

// ---------------------------------------------------------------------------
// stubs
// ---------------------------------------------------------------------------

// resCtxRepo mimics a real database driver (pgx): every operation honours
// context cancellation. The in-memory repository ignores ctx entirely
// (internal/jobs/repository.go takes `_ context.Context`), which hides an
// entire class of shutdown bug from the existing test suite.
type resCtxRepo struct{ inner jobs.JobRepository }

func (r *resCtxRepo) Create(ctx context.Context, j *jobs.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.inner.Create(ctx, j)
}

func (r *resCtxRepo) Get(ctx context.Context, id string) (*jobs.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.inner.Get(ctx, id)
}

func (r *resCtxRepo) Update(ctx context.Context, j *jobs.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.inner.Update(ctx, j)
}

func (r *resCtxRepo) List(ctx context.Context) ([]*jobs.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.inner.List(ctx)
}

// resHookRepo runs a hook before delegating Update, so a test can inject a
// cancellation or a failure at an exact point in the status ladder.
type resHookRepo struct {
	jobs.JobRepository
	beforeUpdate func(status jobs.JobStatus) error
}

func (r *resHookRepo) Update(ctx context.Context, j *jobs.Job) error {
	if r.beforeUpdate != nil {
		if err := r.beforeUpdate(j.Status); err != nil {
			return err
		}
	}
	return r.JobRepository.Update(ctx, j)
}

// resBarrierRepo blocks Get until `want` callers have arrived, so two
// concurrent Process calls for the same job ID both observe the same starting
// status deterministically.
type resBarrierRepo struct {
	jobs.JobRepository
	mu      sync.Mutex
	arrived int
	want    int
	release chan struct{}
}

func newResBarrierRepo(inner jobs.JobRepository, want int) *resBarrierRepo {
	return &resBarrierRepo{JobRepository: inner, want: want, release: make(chan struct{})}
}

func (r *resBarrierRepo) Get(ctx context.Context, id string) (*jobs.Job, error) {
	// Read first, so every participant observes the same starting status, then
	// hold everyone at the barrier until all reads are done. Waiting before the
	// read would let the first caller persist a new status while the second is
	// still inside Get.
	job, err := r.JobRepository.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.arrived++
	if r.arrived == r.want {
		close(r.release)
	}
	r.mu.Unlock()

	select {
	case <-r.release:
	case <-time.After(5 * time.Second):
		return nil, errors.New("barrier timeout")
	}
	return job, nil
}

// resFailingStore is an events.Store whose writes always fail, standing in for
// a database outage during a job.
type resFailingStore struct {
	mu       sync.Mutex
	calls    int
	failWith error
}

func (s *resFailingStore) Append(_ context.Context, _ events.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.failWith
}

func (s *resFailingStore) History(_ context.Context, _ string) ([]events.Event, error) {
	return nil, nil
}

func (s *resFailingStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func resNewJob(t *testing.T, repo jobs.JobRepository, id string) {
	t.Helper()
	j := &jobs.Job{
		ID:        id,
		Task:      "resilience probe",
		RepoPath:  "/tmp/repo",
		Status:    jobs.StatusQueued,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := repo.Create(context.Background(), j); err != nil {
		t.Fatalf("create job: %v", err)
	}
}

func resStatus(t *testing.T, repo jobs.JobRepository, id string) jobs.JobStatus {
	t.Helper()
	got, err := repo.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get job %s: %v", id, err)
	}
	return got.Status
}

// ---------------------------------------------------------------------------
// FAILURE MODE 1 — shutdown with in-flight jobs
// ---------------------------------------------------------------------------

// main.go does cancelWorkers() -> q.Close() -> pool.Wait(). The worker passes
// the shutdown context straight into Process (internal/worker/pool.go:49), so
// an in-flight job is KILLED, not drained. With a ctx-honouring repository
// (i.e. postgres in production) the very first Repo.Get fails, Process returns
// an error the worker only logs, and the job is left in `queued` FOREVER while
// having already been consumed from the in-memory queue channel.
func TestResilience_ShutdownWithCancelledCtxLeavesJobStuckInQueued(t *testing.T) {
	inner := jobs.NewInMemoryRepository()
	repo := &resCtxRepo{inner: inner}
	resNewJob(t, inner, "job-shutdown-get")

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate cancelWorkers() firing before this job starts

	err := p.Process(ctx, "job-shutdown-get")
	if err == nil {
		t.Fatal("Process returned nil, want an error for a cancelled context")
	}
	if !strings.Contains(err.Error(), "load job") {
		t.Fatalf("Process error = %v, want a 'load job' failure", err)
	}

	got := resStatus(t, inner, "job-shutdown-get")
	if got != jobs.StatusQueued {
		t.Fatalf("status = %q, want %q", got, jobs.StatusQueued)
	}
	// Documented gap: the job is neither cancelled nor failed. It is stuck in a
	// non-terminal status with nothing left to pick it up.
	if got == jobs.StatusCancelled || got == jobs.StatusFailed {
		t.Fatalf("unexpected terminal status %q — behavior changed, re-baseline this test", got)
	}
}

// Shutdown lands between job.Transition and Repo.Update, so the status write
// fails. This used to return without ending the job, leaving it parked at the
// previous step with nothing left to pick it up; the transition and the persist
// now both route through markFailed, so the job ends terminal either way.
func TestResilience_CancelDuringPersistStillEndsTheJob(t *testing.T) {
	inner := jobs.NewInMemoryRepository()
	resNewJob(t, inner, "job-persist-cancel")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	base := &resCtxRepo{inner: inner}
	repo := &resHookRepo{
		JobRepository: base,
		beforeUpdate: func(status jobs.JobStatus) error {
			// Shutdown signal arrives exactly as `implementing` is being written.
			if status == jobs.StatusImplementing {
				cancel()
			}
			return nil
		},
	}

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	err := p.Process(ctx, "job-persist-cancel")
	if err == nil {
		t.Fatal("Process returned nil, want a persist failure")
	}
	if !strings.Contains(err.Error(), "persist status implementing") {
		t.Fatalf("Process error = %v, want 'persist status implementing'", err)
	}

	got := resStatus(t, inner, "job-persist-cancel")
	if got != jobs.StatusFailed {
		t.Fatalf("status = %q, want failed — a job whose status write fails must "+
			"not be abandoned mid-ladder", got)
	}
}

// Control case: with the ctx-ignoring in-memory repository, a mid-flight
// cancellation DOES reach markCancelled and the job ends terminal. This is why
// the existing suite looks healthy — the bugs above only surface against a
// repository that honours ctx (postgres).
func TestResilience_InMemoryRepoMasksShutdownBugAndReachesCancelled(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	resNewJob(t, repo, "job-inmem-cancel")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	done := make(chan error, 1)
	go func() { done <- p.Process(ctx, "job-inmem-cancel") }()

	time.Sleep(75 * time.Millisecond) // mid-ladder
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Process did not return after cancellation")
	}
	if got := resStatus(t, repo, "job-inmem-cancel"); got != jobs.StatusCancelled {
		t.Fatalf("status = %q, want %q", got, jobs.StatusCancelled)
	}
}

// ---------------------------------------------------------------------------
// FAILURE MODE 3 — no job-level timeout, no reaper
// ---------------------------------------------------------------------------

// FIXED: Process now derives its job context with context.WithTimeout, so every
// job carries a deadline. Without one a wedged step (a hung go test, a
// deadlocked build) occupied its worker slot indefinitely; with the default
// four workers, four such jobs stalled the service entirely.
func TestResilience_JobContextCarriesADeadline(t *testing.T) {
	inner := jobs.NewInMemoryRepository()
	resNewJob(t, inner, "job-deadline-probe")

	var (
		mu          sync.Mutex
		sawDeadline bool
		probed      bool
	)
	repo := &resDeadlineRepo{
		JobRepository: inner,
		onCtx: func(ctx context.Context) {
			mu.Lock()
			defer mu.Unlock()
			if !probed {
				probed = true
				_, ok := ctx.Deadline()
				sawDeadline = ok
			}
		},
	}

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	if err := p.Process(context.Background(), "job-deadline-probe"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !probed {
		t.Fatal("repository was never called; probe did not run")
	}
	if !sawDeadline {
		t.Fatal("job context has no deadline — a wedged step would hold its worker slot forever")
	}
}

type resDeadlineRepo struct {
	jobs.JobRepository
	onCtx func(ctx context.Context)
}

func (r *resDeadlineRepo) Update(ctx context.Context, j *jobs.Job) error {
	if r.onCtx != nil {
		r.onCtx(ctx)
	}
	return r.JobRepository.Update(ctx, j)
}

// A step that hangs (a wedged model call, a stalled DB) blocks Process forever
// and burns a worker slot permanently. There is no reaper and no deadline, so
// the job sits in a non-terminal status indefinitely.
func TestResilience_HungStepBlocksWorkerForeverInNonTerminalStatus(t *testing.T) {
	inner := jobs.NewInMemoryRepository()
	resNewJob(t, inner, "job-hang")

	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })

	repo := &resHookRepo{
		JobRepository: inner,
		beforeUpdate: func(status jobs.JobStatus) error {
			if status == jobs.StatusImplementing {
				<-release // wedge forever
			}
			return nil
		},
	}

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	done := make(chan error, 1)
	go func() { done <- p.Process(context.Background(), "job-hang") }()

	select {
	case err := <-done:
		t.Fatalf("Process returned %v — a job timeout now exists, re-baseline this test", err)
	case <-time.After(600 * time.Millisecond):
		// Still wedged, as expected.
	}

	if got := resStatus(t, inner, "job-hang"); got != jobs.StatusPreparingWorkspace {
		t.Fatalf("status while hung = %q, want %q (non-terminal, no reaper)",
			got, jobs.StatusPreparingWorkspace)
	}

	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Process did not finish after release")
	}
}

// ---------------------------------------------------------------------------
// FAILURE MODE 6 — repository failure mid-pipeline
// ---------------------------------------------------------------------------

// A repository error mid-pipeline used to leave the job wedged in whatever
// non-terminal status it last persisted, with the worker logging the error and
// moving on. The job now ends failed and says so on the event stream, which is
// what an operator watching a job needs to see.
func TestResilience_RepoFailureMidPipelineMarksTheJobFailed(t *testing.T) {
	inner := jobs.NewInMemoryRepository()
	resNewJob(t, inner, "job-db-down")

	dbDown := errors.New("db down")
	repo := &resHookRepo{
		JobRepository: inner,
		beforeUpdate: func(status jobs.JobStatus) error {
			if status == jobs.StatusVerifying {
				return dbDown
			}
			return nil
		},
	}

	bus := events.NewInProcessBus()
	sub := bus.Subscribe("job-db-down", 256)
	defer bus.Unsubscribe(sub)

	p := NewProcessor(repo, NewCanceller(), bus)
	err := p.Process(context.Background(), "job-db-down")
	if err == nil {
		t.Fatal("Process returned nil, want the repository error")
	}
	if !errors.Is(err, dbDown) {
		t.Fatalf("Process error = %v, want it to wrap %v", err, dbDown)
	}

	got := resStatus(t, inner, "job-db-down")
	if got != jobs.StatusFailed {
		t.Fatalf("status = %q, want failed", got)
	}

	// A job that stops mid-ladder with no event looks identical to a job that
	// is still working.
	var sawFailed bool
	for _, e := range resDrain(sub) {
		if e.Type == events.TypeJobFailed {
			sawFailed = true
		}
	}
	if !sawFailed {
		t.Error("no job.failed event: a watching operator sees the job simply stop")
	}
}

func resDrain(sub *events.Subscription) []events.Event {
	var out []events.Event
	for {
		select {
		case e, ok := <-sub.C:
			if !ok {
				return out
			}
			out = append(out, e)
		default:
			return out
		}
	}
}

// ---------------------------------------------------------------------------
// FAILURE MODE 7 — duplicate execution
// ---------------------------------------------------------------------------

// Nothing deduplicates job IDs: the queue is a plain chan string and Process
// takes no lease. Two workers can run the same job end-to-end concurrently,
// doubling every DB write and every event.
func TestResilience_SameJobIDRunsTwiceConcurrentlyWithNoLease(t *testing.T) {
	inner := jobs.NewInMemoryRepository()
	resNewJob(t, inner, "job-dupe")
	repo := newResBarrierRepo(inner, 2)

	bus := events.NewInProcessBus()
	sub := bus.Subscribe("job-dupe", 256)
	// Defers run LIFO: wg.Wait() must run BEFORE Unsubscribe, because
	// unsubscribing while a publisher is mid-Publish panics (see
	// TestResilience_UnsubscribeDuringPublishPanics in internal/events).
	defer bus.Unsubscribe(sub)
	var wg sync.WaitGroup
	wg.Add(2)
	defer wg.Wait()

	p := NewProcessor(repo, NewCanceller(), bus)

	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			errs <- p.Process(context.Background(), "job-dupe")
		}()
	}
	results := make([]error, 0, 2)
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			results = append(results, err)
		case <-time.After(10 * time.Second):
			t.Fatal("duplicate Process calls did not finish")
		}
	}
	for i, err := range results {
		if err != nil {
			t.Fatalf("duplicate Process #%d returned %v, want nil (no dedup guard exists)", i, err)
		}
	}

	counts := map[string]int{}
	for _, e := range resDrain(sub) {
		counts[e.Type]++
	}
	if counts[events.TypeJobStarted] != 2 {
		t.Fatalf("job.started = %d, want 2 (the same job ran twice)", counts[events.TypeJobStarted])
	}
	if counts[events.TypeJobCompleted] != 2 {
		t.Fatalf("job.completed = %d, want 2 (the same job completed twice)", counts[events.TypeJobCompleted])
	}
	if counts[events.TypeStepCompleted] != 12 {
		t.Fatalf("step.completed = %d, want 12 (6 steps x 2 concurrent runs)", counts[events.TypeStepCompleted])
	}
}

// Consequence of duplicate execution: Canceller.Track overwrites the map entry
// keyed by jobID (canceller.go:20-24). The first run's cancel func is dropped
// on the floor, so POST /cancel can only ever stop one of the two runs and the
// other becomes permanently uncancellable.
func TestResilience_CancellerTrackSilentlyOverwritesPriorCancelFunc(t *testing.T) {
	c := NewCanceller()
	firstFired := make(chan struct{}, 1)
	secondFired := make(chan struct{}, 1)

	c.Track("job-dupe", func() { firstFired <- struct{}{} })
	c.Track("job-dupe", func() { secondFired <- struct{}{} })

	if err := c.Cancel("job-dupe"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case <-secondFired:
	case <-time.After(time.Second):
		t.Fatal("second cancel func did not fire")
	}
	select {
	case <-firstFired:
		t.Fatal("first cancel func fired — Track no longer clobbers, re-baseline this test")
	case <-time.After(50 * time.Millisecond):
		// Expected: the first registration was silently lost.
	}

	// And the map entry is gone, so the surviving run cannot be cancelled at all.
	if err := c.Cancel("job-dupe"); !errors.Is(err, ErrJobNotRunning) {
		t.Fatalf("second Cancel = %v, want ErrJobNotRunning", err)
	}
}

// ---------------------------------------------------------------------------
// FAILURE MODE 8 — event bus failures are silent
// ---------------------------------------------------------------------------

// PersistingBus.Publish (events/store.go:85-90) returns early when the DB write
// fails, so the event is never fanned out to the in-process bus either. And
// Processor.emit discards the error entirely (`_ = p.Bus.Publish(...)`,
// processor.go:27). Net effect: a DB hiccup silently blinds every WebSocket
// client AND leaves no trace in the event history, while the job reports
// success.
func TestResilience_EventStoreFailureSilentlyDropsEveryEventAndStream(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	resNewJob(t, repo, "job-events-down")

	inner := events.NewInProcessBus()
	store := &resFailingStore{failWith: errors.New("events table unavailable")}
	bus := events.NewPersistingBus(inner, store)

	sub := bus.Subscribe("job-events-down", 256)
	defer bus.Unsubscribe(sub)

	p := NewProcessor(repo, NewCanceller(), bus)
	if err := p.Process(context.Background(), "job-events-down"); err != nil {
		t.Fatalf("Process returned %v, want nil — event failures do not fail the job", err)
	}
	if got := resStatus(t, repo, "job-events-down"); got != jobs.StatusCompleted {
		t.Fatalf("status = %q, want %q", got, jobs.StatusCompleted)
	}

	if store.callCount() == 0 {
		t.Fatal("event store was never called; test is not exercising the path")
	}
	if got := resDrain(sub); len(got) != 0 {
		t.Fatalf("subscriber received %d events, want 0 — fan-out on store failure was added, re-baseline this test", len(got))
	}
}
