package jobs

import (
	"errors"
	"path/filepath"
	"strings"
)

const (
	maxTaskLength = 4000
	minTaskLength = 4
)

type NewJobRequest struct {
	Task     string `json:"task"`
	RepoPath string `json:"repo_path"`
}

var (
	ErrTaskRequired    = errors.New("task is required")
	ErrTaskTooShort    = errors.New("task is too short")
	ErrTaskTooLong     = errors.New("task exceeds maximum length")
	ErrRepoPathMissing = errors.New("repo_path is required")
	ErrRepoPathRelative = errors.New("repo_path must be an absolute path")
)

func (r NewJobRequest) Validate() error {
	task := strings.TrimSpace(r.Task)
	if task == "" {
		return ErrTaskRequired
	}
	if len(task) < minTaskLength {
		return ErrTaskTooShort
	}
	if len(task) > maxTaskLength {
		return ErrTaskTooLong
	}

	if strings.TrimSpace(r.RepoPath) == "" {
		return ErrRepoPathMissing
	}
	if !filepath.IsAbs(r.RepoPath) {
		return ErrRepoPathRelative
	}
	return nil
}
