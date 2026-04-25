package jobs

import "time"

type Job struct {
	ID        string
	Task      string
	RepoPath  string
	CreatedAt time.Time
	UpdatedAt time.Time
}
