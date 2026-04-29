package jobs

import (
	"testing"
	"time"
)

// Timestamps stored on a Job must be UTC to match how they are created
// (api.jobs uses time.Now().UTC()) and how events are stamped. A local-time
// UpdatedAt would serialize with an offset while CreatedAt serialized as Z.
func TestTransition_UpdatedAtIsUTC(t *testing.T) {
	j := &Job{ID: "j1", Status: StatusQueued, CreatedAt: time.Now().UTC()}
	before := time.Now().UTC()

	if err := j.Transition(StatusPlanning); err != nil {
		t.Fatalf("transition: %v", err)
	}

	if loc := j.UpdatedAt.Location(); loc != time.UTC {
		t.Errorf("UpdatedAt location = %v, want UTC", loc)
	}
	if j.UpdatedAt.Before(before) {
		t.Errorf("UpdatedAt %v is before the transition time %v", j.UpdatedAt, before)
	}
}
