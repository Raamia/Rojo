package jobs

import (
	"fmt"
	"time"
)

type Job struct {
	ID        string
	Task      string
	RepoPath  string
	Status    JobStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (j *Job) Transition(next JobStatus) error {
	if !canTransition(j.Status, next) {
		return fmt.Errorf("illegal transition for job %s: %s -> %s", j.ID, j.Status, next)
	}
	j.Status = next
	j.UpdatedAt = time.Now()
	return nil
}
