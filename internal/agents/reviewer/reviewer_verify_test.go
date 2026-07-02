package reviewer

import (
	"context"
	"errors"
	"testing"

	"github.com/Raamia/Rojo/internal/agents/model"
	"github.com/Raamia/Rojo/internal/verification"
)

// failingReport returns a Report where at least one deterministic check failed.
func failingReport() verification.Report {
	return verification.Report{Results: []verification.Result{
		{Check: "go test", Passed: true},
		{Check: "go vet", Passed: false, Output: "vet failed"},
	}}
}

// passingReport returns a Report where every deterministic check passed.
func passingReport() verification.Report {
	return verification.Report{Results: []verification.Result{
		{Check: "go test", Passed: true},
		{Check: "go vet", Passed: true},
	}}
}

// TestDeterministicOverrideShortCircuits verifies the project security
// invariant: when a deterministic check fails, the reviewer must return
// request_changes WITHOUT consulting the model. The FakeClient is configured
// to return "approve" and to record whether it was called; if the override
// holds, the model is never consulted and the result is request_changes.
func TestDeterministicOverrideShortCircuits(t *testing.T) {
	spy := &spyClient{reply: `{"decision":"approve","notes":"looks good"}`}
	r := New(spy)

	rev, err := r.Review(context.Background(), Request{
		Task:         "do the thing",
		Diff:         "some diff",
		Verification: failingReport(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spy.calls != 0 {
		t.Fatalf("model was consulted %d time(s); deterministic override must short-circuit before any model call", spy.calls)
	}
	if rev.Decision != DecisionRequestChanges {
		t.Fatalf("decision = %q, want %q", rev.Decision, DecisionRequestChanges)
	}
}

// TestValidModelDecisions covers the happy path: when checks pass and the model
// returns a valid decision JSON, the reviewer returns that decision with
// notes/reasons parsed.
func TestValidModelDecisions(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		want  Decision
	}{
		{"approve", `{"decision":"approve","notes":"ship it","reasons":["tests pass","clean diff"]}`, DecisionApprove},
		{"request_changes", `{"decision":"request_changes","notes":"tweak naming","reasons":["style"]}`, DecisionRequestChanges},
		{"reject", `{"decision":"reject","notes":"wrong approach"}`, DecisionReject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New(&model.FakeClient{Reply: tc.reply})
			rev, err := r.Review(context.Background(), Request{
				Task:         "task",
				Verification: passingReport(),
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rev.Decision != tc.want {
				t.Fatalf("decision = %q, want %q", rev.Decision, tc.want)
			}
			if rev.Notes == "" {
				t.Errorf("notes not parsed, got empty string")
			}
		})
	}
}

// TestReasonsParsed confirms the Reasons slice is populated from model JSON.
func TestReasonsParsed(t *testing.T) {
	r := New(&model.FakeClient{Reply: `{"decision":"approve","notes":"ok","reasons":["a","b"]}`})
	rev, err := r.Review(context.Background(), Request{Verification: passingReport()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rev.Reasons) != 2 || rev.Reasons[0] != "a" || rev.Reasons[1] != "b" {
		t.Fatalf("reasons = %v, want [a b]", rev.Reasons)
	}
}

// TestUnknownDecision verifies an unknown/unsupported decision string yields
// ErrInvalidDecision (even though the JSON itself is well-formed).
func TestUnknownDecision(t *testing.T) {
	r := New(&model.FakeClient{Reply: `{"decision":"maybe","notes":"unsure"}`})
	_, err := r.Review(context.Background(), Request{Verification: passingReport()})
	if !errors.Is(err, ErrInvalidDecision) {
		t.Fatalf("err = %v, want ErrInvalidDecision", err)
	}
}

// TestMalformedJSON verifies malformed model output is rejected safely.
// The reviewer documents wrapping the unmarshal failure with ErrInvalidDecision.
func TestMalformedJSON(t *testing.T) {
	r := New(&model.FakeClient{Reply: `{not valid json`})
	_, err := r.Review(context.Background(), Request{Verification: passingReport()})
	if !errors.Is(err, ErrInvalidDecision) {
		t.Fatalf("err = %v, want ErrInvalidDecision", err)
	}
}

// TestModelError verifies a transport-level model error propagates (not
// silently swallowed) and does not produce a spurious approval.
func TestModelError(t *testing.T) {
	sentinel := errors.New("boom")
	r := New(&model.FakeClient{Err: sentinel})
	_, err := r.Review(context.Background(), Request{Verification: passingReport()})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped sentinel", err)
	}
}

// spyClient records how many times Generate is called so tests can prove the
// deterministic override never reaches the model.
type spyClient struct {
	reply string
	calls int
}

func (s *spyClient) Generate(_ context.Context, _ model.Request) (model.Response, error) {
	s.calls++
	return model.Response{Text: s.reply, Model: "spy"}, nil
}

// The reviewer was the one agent that did not unwrap a fenced reply, and it is
// the worst place to lack it: the reviewer runs last, so a model wrapping its
// answer in ```json discarded a completed plan, implementation and
// verification — the most expensive possible moment to fail over punctuation.
func TestReview_AcceptsFencedJSON(t *testing.T) {
	body := `{"decision":"approve","notes":"looks right"}`
	for name, reply := range map[string]string{
		"fenced":     "```\n" + body + "\n```",
		"tagged":     "```json\n" + body + "\n```",
		"whitespace": "  ```json\n" + body + "\n```  ",
	} {
		t.Run(name, func(t *testing.T) {
			r := New(&model.FakeClient{Reply: reply})
			got, err := r.Review(context.Background(), passingRequest())
			if err != nil {
				t.Fatalf("review: %v", err)
			}
			if got.Decision != DecisionApprove {
				t.Errorf("decision = %q, want approve", got.Decision)
			}
			if got.Notes != "looks right" {
				t.Errorf("notes = %q", got.Notes)
			}
		})
	}
}

// passingRequest is a review request whose checks passed, so Review reaches the
// model instead of short-circuiting on the deterministic gate.
func passingRequest() Request {
	return Request{
		Task: "add a greeting",
		Verification: verification.Report{
			Results: []verification.Result{{Check: "go test", Passed: true}},
		},
	}
}
