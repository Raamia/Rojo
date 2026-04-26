package jobs

type JobStatus string

const (
	StatusQueued             JobStatus = "queued"
	StatusPlanning           JobStatus = "planning"
	StatusPreparingWorkspace JobStatus = "preparing_workspace"
	StatusImplementing       JobStatus = "implementing"
	StatusVerifying          JobStatus = "verifying"
	StatusReviewing          JobStatus = "reviewing"
	StatusWaitingForRevision JobStatus = "waiting_for_revision"
	StatusCompleted          JobStatus = "completed"
	StatusFailed             JobStatus = "failed"
	StatusCancelled          JobStatus = "cancelled"
)
