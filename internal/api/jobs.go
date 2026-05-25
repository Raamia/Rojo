package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
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

func (h *JobsHandler) List(w http.ResponseWriter, r *http.Request) {
	list, err := h.Repo.List(r.Context())
	if err != nil {
		WriteJSONError(w, http.StatusInternalServerError, "failed to list jobs")
		return
	}
	if list == nil {
		list = []*jobs.Job{}
	}
	WriteJSON(w, http.StatusOK, list)
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
