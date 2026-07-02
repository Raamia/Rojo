package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Raamia/Rojo/internal/agents/model"
	"github.com/Raamia/Rojo/internal/agents/planner"
	"github.com/Raamia/Rojo/internal/verification"
)

type Decision string

const (
	DecisionApprove        Decision = "approve"
	DecisionRequestChanges Decision = "request_changes"
	DecisionReject         Decision = "reject"
)

var ErrInvalidDecision = errors.New("reviewer returned invalid decision")

type Request struct {
	Task          string              `json:"task"`
	Plan          planner.Plan        `json:"plan"`
	Diff          string              `json:"diff"`
	Verification  verification.Report `json:"verification"`
	PriorFeedback string              `json:"prior_feedback,omitempty"`
	ChangedFiles  []string            `json:"changed_files,omitempty"`
	Extras        map[string]any      `json:"extras,omitempty"`
}

type Review struct {
	Decision Decision `json:"decision"`
	Notes    string   `json:"notes"`
	Reasons  []string `json:"reasons,omitempty"`
}

type Reviewer struct {
	Client model.Client
}

func New(c model.Client) *Reviewer {
	return &Reviewer{Client: c}
}

const systemPrompt = `You are the Rojo reviewer. Given a task, plan, diff, and verification report, respond with a JSON object with "decision" (approve|request_changes|reject), "notes", and optionally "reasons". You may not override failed deterministic checks — if verification.results contain any failed check, decision must be "request_changes" or "reject".`

func (r *Reviewer) Review(ctx context.Context, req Request) (Review, error) {
	if !req.Verification.AllPassed() {
		return Review{
			Decision: DecisionRequestChanges,
			Notes:    "deterministic checks failed",
		}, nil
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Review{}, fmt.Errorf("marshal review request: %w", err)
	}
	resp, err := r.Client.Generate(ctx, model.Request{
		System: systemPrompt,
		Prompt: string(body),
	})
	if err != nil {
		return Review{}, fmt.Errorf("model call: %w", err)
	}
	var review Review
	if err := json.Unmarshal([]byte(model.Unfence(resp.Text)), &review); err != nil {
		return Review{}, fmt.Errorf("%w: %v", ErrInvalidDecision, err)
	}
	switch review.Decision {
	case DecisionApprove, DecisionRequestChanges, DecisionReject:
		return review, nil
	default:
		return Review{}, fmt.Errorf("%w: %q", ErrInvalidDecision, review.Decision)
	}
}
