package jobs

import "testing"

func TestJobTransition_Valid(t *testing.T) {
	cases := []struct {
		name string
		from JobStatus
		to   JobStatus
	}{
		{"queued to planning", StatusQueued, StatusPlanning},
		{"planning to preparing", StatusPlanning, StatusPreparingWorkspace},
		{"preparing to implementing", StatusPreparingWorkspace, StatusImplementing},
		{"implementing to verifying", StatusImplementing, StatusVerifying},
		{"verifying to reviewing", StatusVerifying, StatusReviewing},
		{"reviewing to completed", StatusReviewing, StatusCompleted},
		{"reviewing to waiting for revision", StatusReviewing, StatusWaitingForRevision},
		{"waiting for revision back to implementing", StatusWaitingForRevision, StatusImplementing},
		{"queued to cancelled", StatusQueued, StatusCancelled},
		{"implementing to failed", StatusImplementing, StatusFailed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := &Job{ID: "j1", Status: tc.from}
			if err := j.Transition(tc.to); err != nil {
				t.Fatalf("expected valid transition, got error: %v", err)
			}
			if j.Status != tc.to {
				t.Fatalf("expected status %s, got %s", tc.to, j.Status)
			}
			if j.UpdatedAt.IsZero() {
				t.Fatal("expected UpdatedAt to be set")
			}
		})
	}
}

func TestJobTransition_Invalid(t *testing.T) {
	cases := []struct {
		name string
		from JobStatus
		to   JobStatus
	}{
		{"queued directly to completed", StatusQueued, StatusCompleted},
		{"queued to implementing", StatusQueued, StatusImplementing},
		{"completed to anything", StatusCompleted, StatusPlanning},
		{"failed to queued", StatusFailed, StatusQueued},
		{"cancelled to planning", StatusCancelled, StatusPlanning},
		{"reviewing back to queued", StatusReviewing, StatusQueued},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := &Job{ID: "j1", Status: tc.from}
			if err := j.Transition(tc.to); err == nil {
				t.Fatalf("expected error for illegal transition %s -> %s", tc.from, tc.to)
			}
			if j.Status != tc.from {
				t.Fatalf("status should not change on failed transition, got %s", j.Status)
			}
		})
	}
}
