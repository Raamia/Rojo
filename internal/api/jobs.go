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

type JobsHandler struct {
	Repo jobs.JobRepository
}

func NewJobsHandler(repo jobs.JobRepository) *JobsHandler {
	return &JobsHandler{Repo: repo}
}

func (h *JobsHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req jobs.NewJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
