package api

import (
	"errors"
	"io/fs"
	"net/http"
	"strconv"

	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/orchestration"
)

// ArtifactReader fetches a stored job output. Declared here, near its consumer,
// so the handler does not depend on which store produced the artifact.
type ArtifactReader interface {
	ReadArtifact(jobID, name string) ([]byte, error)
}

type DiffHandler struct {
	Repo      jobs.JobRepository
	Artifacts ArtifactReader
}

func NewDiffHandler(repo jobs.JobRepository, a ArtifactReader) *DiffHandler {
	return &DiffHandler{Repo: repo, Artifacts: a}
}

// Diff serves a job's patch as text, so it can be piped straight into
// `git apply` without unwrapping it from JSON first. Errors stay JSON, matching
// every other endpoint — only the success body is special.
func (h *DiffHandler) Diff(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("jobID")

	// The job is looked up first so a nonexistent job and a job that has not
	// produced a patch are distinguishable. Both would otherwise be a bare 404,
	// and "your ID is wrong" needs a different response from "wait, it is still
	// running".
	job, err := h.Repo.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, jobs.ErrJobNotFound) {
			WriteJSONError(w, http.StatusNotFound, "job not found")
			return
		}
		LoggerFrom(r.Context()).Error("load job for diff", "job_id", id, "err", err)
		WriteJSONError(w, http.StatusInternalServerError, "failed to load job")
		return
	}
	if h.Artifacts == nil {
		WriteJSONError(w, http.StatusServiceUnavailable, "artifacts not available")
		return
	}

	diff, err := h.Artifacts.ReadArtifact(id, orchestration.ArtifactDiff)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			WriteJSONError(w, http.StatusNotFound,
				"no diff for job in status "+string(job.Status))
			return
		}
		LoggerFrom(r.Context()).Error("read diff artifact", "job_id", id, "err", err)
		WriteJSONError(w, http.StatusInternalServerError, "failed to read diff")
		return
	}

	w.Header().Set("Content-Type", "text/x-diff; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(diff)))
	// A patch is something you save, not something a browser should try to
	// render — and naming it after the job makes several downloads distinct.
	w.Header().Set("Content-Disposition", `attachment; filename="`+id+`.diff"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(diff)
}
