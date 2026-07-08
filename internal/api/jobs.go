package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
)

type Enqueuer interface {
	Enqueue(jobID string) error
}

type Canceller interface {
	Cancel(jobID string) error
}

type JobsHandler struct {
	Repo      jobs.JobRepository
	Queue     Enqueuer
	Canceller Canceller
	Publisher events.Bus
}

func NewJobsHandler(repo jobs.JobRepository, q Enqueuer, c Canceller, pub events.Bus) *JobsHandler {
	return &JobsHandler{Repo: repo, Queue: q, Canceller: c, Publisher: pub}
}

// MaxRequestBodyBytes bounds a job submission. The largest legal request is a
// 4000-character task plus a path, so 64 KiB is generous. Without this the
// decoder buffers the whole body into memory before validation can reject it,
// so any unauthenticated client could pin arbitrary heap by streaming a large
// body — measured at 16 MiB accepted, and 50 MiB read in full.
const MaxRequestBodyBytes = 64 << 10

func (h *JobsHandler) Create(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBodyBytes)

	var req jobs.NewJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			WriteJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		WriteJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if err := req.Validate(); err != nil {
		WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	now := time.Now().UTC()
	job := &jobs.Job{
		ID:        newID(),
		Task:      req.Task,
		RepoPath:  req.RepoPath,
		Status:    jobs.StatusQueued,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := h.Repo.Create(r.Context(), job); err != nil {
		LoggerFrom(r.Context()).Error("create job", "job_id", job.ID, "err", err)
		WriteJSONError(w, http.StatusInternalServerError, "failed to create job")
		return
	}
	if h.Queue != nil {
		if err := h.Queue.Enqueue(job.ID); err != nil {
			// The job is already persisted, but nothing will ever pick it up.
			// Move it to the terminal `failed` status so it does not linger as
			// a phantom `queued` job that looks pending forever; a rejected
			// submission must not leave work that no worker will claim.
			if txErr := job.Transition(jobs.StatusFailed); txErr == nil {
				if updErr := h.Repo.Update(r.Context(), job); updErr != nil {
					LoggerFrom(r.Context()).Error("mark unqueueable job failed",
						"job_id", job.ID, "err", updErr)
				}
			}
			WriteJSONError(w, http.StatusServiceUnavailable, "queue full, try again later")
			return
		}
	}
	if h.Publisher != nil {
		_ = h.Publisher.Publish(r.Context(), events.Event{
			JobID: job.ID,
			Type:  events.TypeJobCreated,
		})
	}
	WriteJSON(w, http.StatusCreated, job)
}

func (h *JobsHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("jobID")
	job, err := h.Repo.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, jobs.ErrJobNotFound) {
			WriteJSONError(w, http.StatusNotFound, "job not found")
			return
		}
		LoggerFrom(r.Context()).Error("load job", "job_id", id, "err", err)
		WriteJSONError(w, http.StatusInternalServerError, "failed to load job")
		return
	}
	WriteJSON(w, http.StatusOK, job)
}

func (h *JobsHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("jobID")
	if _, err := h.Repo.Get(r.Context(), id); err != nil {
		if errors.Is(err, jobs.ErrJobNotFound) {
			WriteJSONError(w, http.StatusNotFound, "job not found")
			return
		}
		LoggerFrom(r.Context()).Error("load job for cancel", "job_id", id, "err", err)
		WriteJSONError(w, http.StatusInternalServerError, "failed to load job")
		return
	}
	if h.Canceller == nil {
		WriteJSONError(w, http.StatusServiceUnavailable, "cancellation not available")
		return
	}
	if err := h.Canceller.Cancel(id); err != nil {
		WriteJSONError(w, http.StatusConflict, err.Error())
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]string{"job_id": id, "status": "cancel_requested"})
}

// Pagination bounds for GET /api/v1/jobs. Without a cap the endpoint serialises
// every job the service has ever run into one response, which grows without
// limit and is a free way for any client to make the server allocate.
const (
	DefaultListLimit = 50
	MaxListLimit     = 200
)

// List returns a page of jobs, newest first.
//
// The page is a slice of the full ordered list: the store still materialises
// every record to sort them, so this bounds the response rather than the read.
// That is the right trade for a single-node service with a filesystem store —
// the response is what crosses the network and what the client has to hold —
// and the place to fix it later is the repository, not here.
func (h *JobsHandler) List(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := pageParams(r)
	if err != nil {
		WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	list, err := h.Repo.List(r.Context())
	if err != nil {
		LoggerFrom(r.Context()).Error("list jobs", "err", err)
		WriteJSONError(w, http.StatusInternalServerError, "failed to list jobs")
		return
	}

	// The total goes in a header so the body stays a plain array — clients that
	// predate paging keep working, and one that wants to page has what it needs.
	w.Header().Set("X-Total-Count", strconv.Itoa(len(list)))

	if offset > len(list) {
		offset = len(list)
	}
	end := offset + limit
	if end > len(list) {
		end = len(list)
	}
	page := list[offset:end]
	if page == nil {
		page = []*jobs.Job{}
	}
	WriteJSON(w, http.StatusOK, page)
}

// pageParams reads limit and offset, rejecting nonsense rather than silently
// substituting a default: a client that asked for limit=-1 has a bug, and
// quietly returning 50 hides it.
func pageParams(r *http.Request) (limit, offset int, err error) {
	limit, offset = DefaultListLimit, 0
	q := r.URL.Query()

	if raw := q.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 {
			return 0, 0, fmt.Errorf("limit must be a positive integer, got %q", raw)
		}
		if limit > MaxListLimit {
			limit = MaxListLimit
		}
	}
	if raw := q.Get("offset"); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 {
			return 0, 0, fmt.Errorf("offset must be a non-negative integer, got %q", raw)
		}
	}
	return limit, offset, nil
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
