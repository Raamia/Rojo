package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Raamia/Rojo/internal/jobs"
)

type Enqueuer interface {
	Enqueue(jobID string) error
}

type JobsHandler struct {
	Repo  jobs.JobRepository
	Queue Enqueuer
}

func NewJobsHandler(repo jobs.JobRepository, q Enqueuer) *JobsHandler {
	return &JobsHandler{Repo: repo, Queue: q}
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
			WriteJSONError(w, http.StatusServiceUnavailable, "queue full, try again later")
			return
		}
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
