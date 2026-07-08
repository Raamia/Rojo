package orchestration

import (
	"context"
	"strings"
	"testing"

	"github.com/Raamia/Rojo/internal/agents/implementor"
	"github.com/Raamia/Rojo/internal/agents/reviewer"
	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
)

// Fan-out and the reviewer are both production features, and this is the first
// test that turns them on together — which is the whole shipping configuration
// once ROJO_FANOUT_VARIANTS is raised and a key is set.
//
// The reviewer must judge the attempt that *won*. Handing it a losing variant
// means judging a diff the job is not returning, against a failing report, and
// the reviewer's own guard then refuses on deterministic grounds — failing a
// job in which every check passed.
func TestFanout_ReviewerJudgesTheWinner(t *testing.T) {
	repo := jobs.NewInMemoryRepository()
	arts := newArtifacts()
	rev := &fakeReviewer{decisions: []reviewer.Review{approve()}}

	p := NewProcessor(repo, NewCanceller(), events.NewInProcessBus())
	p.Workspaces = &diffingWorkspaces{
		base:  t.TempDir(),
		diffs: map[int]string{0: sampleDiff("loser"), 1: sampleDiff("champion")},
	}
	p.Implementor = &fakeImplementor{ops: []implementor.Operation{writeOp("a.go", "package a\n")}}
	p.Verifier = &passOnlyVariant{pass: 1} // variant 0 fails the gate, variant 1 passes
	p.Reviewer = rev
	p.Artifacts = arts
	p.Variants = 2
	newQueuedJob(t, repo, "fanrev")

	if err := p.Process(context.Background(), "fanrev"); err != nil {
		t.Fatalf("a job with a passing variant must not fail: %v", err)
	}
	got, _ := repo.Get(context.Background(), "fanrev")
	if got.Status != jobs.StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}

	rev.mu.Lock()
	defer rev.mu.Unlock()
	if len(rev.requests) == 0 {
		t.Fatal("the reviewer was never consulted")
	}
	seen := rev.requests[0]
	if !strings.Contains(seen.Diff, "champion") {
		t.Errorf("the reviewer judged the wrong variant's diff:\n%s", seen.Diff)
	}
	if !seen.Verification.AllPassed() {
		t.Errorf("the reviewer was shown a failing report for a job that passed: %+v", seen.Verification)
	}
}
