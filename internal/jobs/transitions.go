package jobs

var legalTransitions = map[JobStatus][]JobStatus{
	StatusQueued:             {StatusPlanning, StatusCancelled, StatusFailed},
	StatusPlanning:           {StatusPreparingWorkspace, StatusFailed, StatusCancelled},
	StatusPreparingWorkspace: {StatusImplementing, StatusFailed, StatusCancelled},
	StatusImplementing:       {StatusVerifying, StatusFailed, StatusCancelled},
	StatusVerifying:          {StatusReviewing, StatusFailed, StatusCancelled},
	StatusReviewing:          {StatusCompleted, StatusWaitingForRevision, StatusFailed, StatusCancelled},
	StatusWaitingForRevision: {StatusImplementing, StatusFailed, StatusCancelled},
	StatusCompleted:          {},
	StatusFailed:             {},
	StatusCancelled:          {},
}

func canTransition(from, to JobStatus) bool {
	for _, allowed := range legalTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}
