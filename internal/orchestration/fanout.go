package orchestration

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Raamia/Rojo/internal/verification"
	"github.com/Raamia/Rojo/internal/workspace"
)

// MaxVariants caps how many ways a single job may be attempted. Each variant
// costs a worktree and a full verification run, so an unbounded count would let
// one submission monopolise the host.
const MaxVariants = 8

// candidate is one attempt at a job: its own checkout, its own verification
// result. Attempts are independent by construction — nothing is shared but the
// source repository, which none of them modify.
type candidate struct {
	index  int
	ws     *workspace.Workspace
	report verification.Report
	// err is why verification could not be run or completed.
	err error
	// implErr is why no change could be made to this checkout.
	//
	// It is kept apart from err because verification assigns err unconditionally
	// and would erase an implementation failure. An attempt that was never
	// modified would then be checked against a pristine tree, pass — of course
	// it passes, it is the base commit — and win the job, which would report
	// success while handing back an empty patch.
	implErr error
}

// failure reports why this attempt is unusable, or nil if it is fine.
func (c *candidate) failure() error {
	if c.implErr != nil {
		return c.implErr
	}
	return c.err
}

// implemented reports whether this attempt has changes worth checking.
func (c *candidate) implemented() bool { return c.ws != nil && c.implErr == nil }

// passed reports whether this attempt is a viable answer.
func (c *candidate) passed() bool {
	return c.failure() == nil && c.ws != nil && c.report.AllPassed()
}

// variantID names the worktree for one attempt.
//
// A single-variant job keeps the plain job ID, so the common case produces
// exactly the paths and branches it always has — and startup recovery, which
// derives worktree paths from the job ID, keeps working unchanged.
func variantID(jobID string, index, total int) string {
	if total <= 1 {
		return jobID
	}
	return fmt.Sprintf("%s-v%d", jobID, index)
}

// variantCount clamps a requested fan-out to something the host can survive.
func variantCount(requested int) int {
	switch {
	case requested < 1:
		return 1
	case requested > MaxVariants:
		return MaxVariants
	default:
		return requested
	}
}

// createCandidates prepares one isolated checkout per attempt.
//
// Creation is sequential rather than concurrent: `git worktree add` takes a
// lock on the source repository, so running them in parallel mostly contends on
// that lock, and doing it in order keeps failures easy to attribute. The
// expensive part — verification — is where the parallelism actually pays.
//
// On any failure the checkouts already made are returned alongside the error so
// the caller can still clean them up.
func (p *Processor) createCandidates(ctx context.Context, jobID, repoPath string, total int) ([]*candidate, error) {
	cands := make([]*candidate, 0, total)
	for i := 0; i < total; i++ {
		ws, err := p.Workspaces.Create(ctx, variantID(jobID, i, total), repoPath)
		if err != nil {
			return cands, fmt.Errorf("create workspace for variant %d: %w", i, err)
		}
		cands = append(cands, &candidate{index: i, ws: ws})
	}
	return cands, nil
}

// verifyCandidates runs the deterministic gate against every attempt at once.
//
// This is the point of fanning out: the checks are the slow part, the attempts
// are independent, and a failure in one must not prevent the others from being
// judged. Each candidate records its own outcome; the function itself does not
// fail.
func (p *Processor) verifyCandidates(ctx context.Context, cands []*candidate) {
	var wg sync.WaitGroup
	for _, c := range cands {
		// An attempt with no changes has nothing to check. Running the gate
		// against its untouched checkout would pass and make it look viable.
		if !c.implemented() {
			continue
		}
		wg.Add(1)
		go func(c *candidate) {
			defer wg.Done()
			report, err := p.Verifier.Verify(ctx, c.ws.Path)
			c.report = report
			c.err = err
		}(c)
	}
	wg.Wait()
}

// selectWinner picks the attempt to keep.
//
// Selection is by objective check results, not by judgement: the lowest-indexed
// attempt that passed everything wins. Preferring the lowest index makes the
// choice deterministic, so the same set of results always yields the same
// answer regardless of which goroutine happened to finish first.
func selectWinner(cands []*candidate) (*candidate, bool) {
	for _, c := range cands {
		if c.passed() {
			return c, true
		}
	}
	return nil, false
}

// fanoutSummary describes the outcome of a multi-variant attempt for logging
// and for the event payload.
type fanoutSummary struct {
	Total   int              `json:"total"`
	Passed  int              `json:"passed"`
	Winner  int              `json:"winner"`
	Results []variantOutcome `json:"results"`
}

type variantOutcome struct {
	Variant int    `json:"variant"`
	Passed  bool   `json:"passed"`
	Summary string `json:"summary"`
	Error   string `json:"error,omitempty"`
}

// fanoutFailure builds the error for a job where nothing passed.
//
// The aggregate message says how many attempts failed, but the underlying
// causes are joined in rather than summarised away: a verifier that could not
// run at all ("go: no such tool") is a very different problem from tests that
// legitimately failed, and errors.Is must still reach it.
func fanoutFailure(cands []*candidate, summary fanoutSummary) error {
	errs := []error{fmt.Errorf("no variant passed verification: %d of %d attempts failed",
		summary.Total-summary.Passed, summary.Total)}
	for _, c := range cands {
		if err := c.failure(); err != nil {
			errs = append(errs, fmt.Errorf("variant %d: %w", c.index, err))
		}
	}
	return errors.Join(errs...)
}

func summarise(cands []*candidate, winner *candidate) fanoutSummary {
	sum := fanoutSummary{Total: len(cands), Winner: -1}
	if winner != nil {
		sum.Winner = winner.index
	}
	for _, c := range cands {
		out := variantOutcome{Variant: c.index, Passed: c.passed(), Summary: c.report.Summary()}
		if err := c.failure(); err != nil {
			out.Error = err.Error()
		}
		if out.Passed {
			sum.Passed++
		}
		sum.Results = append(sum.Results, out)
	}
	return sum
}

// representative picks the attempt that best stands for the job when no attempt
// passed. An implemented one is preferred: its diff shows what was tried, where
// an untouched checkout would only ever yield an empty patch.
func representative(cands []*candidate) *candidate {
	for _, c := range cands {
		if c.implemented() {
			return c
		}
	}
	if len(cands) > 0 {
		return cands[0]
	}
	return nil
}
