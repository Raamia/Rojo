package jobs

import (
	"errors"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	// Task limits are counted in runes, not bytes: a 4000-character task is
	// 4000 characters whether it is ASCII or CJK. Counting bytes made the
	// effective limit 1000 characters for emoji and let a 1-character emoji
	// satisfy a 4-character minimum.
	maxTaskLength = 4000
	minTaskLength = 4

	// PATH_MAX is 4096 on Linux; anything longer cannot name a real path.
	maxRepoPathLength = 4096
)

type NewJobRequest struct {
	Task     string `json:"task"`
	RepoPath string `json:"repo_path"`
}

var (
	ErrTaskRequired     = errors.New("task is required")
	ErrTaskTooShort     = errors.New("task is too short")
	ErrTaskTooLong      = errors.New("task exceeds maximum length")
	ErrRepoPathMissing  = errors.New("repo_path is required")
	ErrRepoPathRelative = errors.New("repo_path must be an absolute path")
	ErrRepoPathTooLong  = errors.New("repo_path exceeds maximum length")
	ErrNullByte         = errors.New("field contains a NUL byte")
)

// Validate normalizes the request in place and then checks it.
//
// Normalizing here rather than in the caller keeps validation and storage
// honest about the same value. Previously the length check ran against the
// trimmed task while the handler persisted the raw one, so padding a short task
// with megabytes of leading whitespace passed a 4000-character limit and was
// stored in full — the cap bounded nothing.
func (r *NewJobRequest) Validate() error {
	r.Task = strings.TrimSpace(r.Task)
	r.RepoPath = strings.TrimSpace(r.RepoPath)

	if r.Task == "" {
		return ErrTaskRequired
	}
	// A NUL byte cannot survive a postgres text column or a path syscall, so
	// reject it here as a 400 rather than failing later as a 500.
	if strings.ContainsRune(r.Task, 0) || strings.ContainsRune(r.RepoPath, 0) {
		return ErrNullByte
	}
	if n := utf8.RuneCountInString(r.Task); n < minTaskLength {
		return ErrTaskTooShort
	} else if n > maxTaskLength {
		return ErrTaskTooLong
	}

	if r.RepoPath == "" {
		return ErrRepoPathMissing
	}
	if len(r.RepoPath) > maxRepoPathLength {
		return ErrRepoPathTooLong
	}
	if !filepath.IsAbs(r.RepoPath) {
		return ErrRepoPathRelative
	}
	return nil
}
