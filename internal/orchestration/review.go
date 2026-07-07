package orchestration

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Raamia/Rojo/internal/agents/planner"
	"github.com/Raamia/Rojo/internal/agents/reviewer"
	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/verification"
)

// Reviewer judges whether a change actually did what was asked. Declared here,
// near its consumer, so the processor can be tested without a model.
type Reviewer interface {
	Review(ctx context.Context, req reviewer.Request) (reviewer.Review, error)
}

// MaxFeedbackOutput bounds how much check output is quoted back to the model on
// a revision. A failing test suite can print megabytes; the first part of it is
// where the cause is, and the rest is repetition that only spends tokens.
const MaxFeedbackOutput = 4 << 10

// outcome is what the reviewing step decided to do with a job.
type outcome int

const (
	outcomeApprove outcome = iota
	outcomeRevise
	outcomeFail
)

// reviewInput is one job's state at the reviewing step.
//
// It is a parameter rather than Processor fields because a single Processor
// serves every worker concurrently: per-job state kept on it would be shared
// between unrelated jobs, which is both a race and a correctness bug.
type reviewInput struct {
	task string
	plan planner.Plan
	best *candidate
	// gateErr is set when no attempt passed the deterministic checks.
	gateErr error
	// feedback is what the previous round was told to fix, so the reviewer can
	// see whether it was addressed.
	feedback  string
	canRevise bool
}

// review decides a job's fate after the gate has run.
//
// Deterministic checks come first and are not negotiable: a failing gate never
// reaches the model, because there is nothing for a reviewer to weigh in on
// when the change does not build. Only a change that already passed is worth
// asking whether it did the right thing.
func (p *Processor) review(
	ctx context.Context, log *slog.Logger, jobID string, in reviewInput,
) (outcome, string, error) {
	best, gateErr, canRevise := in.best, in.gateErr, in.canRevise

	if gateErr != nil {
		if !canRevise {
			return outcomeFail, "", gateErr
		}
		// The check output is the most actionable feedback there is — it names
		// the failing test and the line. Handing it back is what lets the
		// implementor correct itself rather than guess.
		log.Info("gate failed; requesting a revision", "summary", best.report.Summary())
		return outcomeRevise, gateFeedback(best.report), nil
	}

	if p.Reviewer == nil {
		return outcomeApprove, "", nil
	}

	p.emit(ctx, jobID, events.TypeReviewStarted, map[string]any{"variant": best.index})

	diff, err := p.diffOf(ctx, best)
	if err != nil {
		// Without a diff the reviewer would be judging a change it cannot see,
		// and a confident opinion formed on no evidence is worse than none. The
		// gate already passed, so the honest outcome is to ship it unreviewed
		// rather than to fail work that met every objective check.
		log.Error("read diff for review; approving on the gate alone", "err", err)
		return outcomeApprove, "", nil
	}

	rev, err := p.Reviewer.Review(ctx, reviewer.Request{
		Task:          in.task,
		Plan:          in.plan,
		Diff:          diff,
		Verification:  best.report,
		ChangedFiles:  changedFiles(diff),
		PriorFeedback: in.feedback,
	})
	if err != nil {
		return outcomeFail, "", fmt.Errorf("review job %s: %w", jobID, err)
	}

	log.Info("review complete", "decision", rev.Decision, "notes", rev.Notes)
	p.emit(ctx, jobID, events.TypeReviewCompleted, map[string]any{
		"variant":  best.index,
		"decision": string(rev.Decision),
		"notes":    rev.Notes,
		"reasons":  rev.Reasons,
	})

	switch rev.Decision {
	case reviewer.DecisionApprove:
		return outcomeApprove, "", nil
	case reviewer.DecisionReject:
		// Reject means the approach is wrong, not that it needs another pass.
		// Retrying a rejected approach just spends a second model call to
		// arrive somewhere just as wrong.
		return outcomeFail, "", fmt.Errorf("review rejected job %s: %s", jobID, rev.Notes)
	default: // request_changes
		if !canRevise {
			return outcomeFail, "", fmt.Errorf("%w for job %s: %s",
				ErrMaxRevisionsExceeded, jobID, rev.Notes)
		}
		return outcomeRevise, rev.Notes, nil
	}
}

// diffOf reads an attempt's current patch. The reviewer needs to see the change
// itself; the verification report says the change is safe, not that it is right.
func (p *Processor) diffOf(ctx context.Context, c *candidate) (string, error) {
	if p.Workspaces == nil || c == nil || c.ws == nil {
		return "", fmt.Errorf("no workspace to diff")
	}
	return p.Workspaces.Diff(ctx, c.ws)
}

// gateFeedback turns a failed report into instructions the implementor can act
// on: which checks failed, and what they printed.
func gateFeedback(r verification.Report) string {
	var b strings.Builder
	b.WriteString("The previous attempt failed verification: ")
	b.WriteString(r.Summary())
	b.WriteString("\n\nFix the cause. Output from the failing checks:\n")
	for _, res := range r.Results {
		if res.Passed {
			continue
		}
		fmt.Fprintf(&b, "\n--- %s ---\n%s\n", res.Check, truncate(res.Output, MaxFeedbackOutput))
	}
	return b.String()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... (truncated)"
}
