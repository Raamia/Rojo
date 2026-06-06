package orchestration

import (
	"context"
	"errors"

	"github.com/Raamia/Rojo/internal/agents/reviewer"
)

var ErrMaxRevisionsExceeded = errors.New("max revisions exceeded")

const MaxRevisions = 1

type RevisionAttempt func(ctx context.Context, priorFeedback string) (reviewer.Review, error)

func RunWithRevisionLoop(ctx context.Context, attempt RevisionAttempt) (reviewer.Review, error) {
	var feedback string
	for i := 0; i <= MaxRevisions; i++ {
		review, err := attempt(ctx, feedback)
		if err != nil {
			return reviewer.Review{}, err
		}
		if review.Decision == reviewer.DecisionApprove || review.Decision == reviewer.DecisionReject {
			return review, nil
		}
		feedback = review.Notes
	}
	return reviewer.Review{}, ErrMaxRevisionsExceeded
}
