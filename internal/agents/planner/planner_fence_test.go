package planner

import (
	"context"
	"testing"

	"github.com/Raamia/Rojo/internal/agents/model"
)

// Models routinely wrap JSON in a markdown fence even when told not to. That is
// a formatting habit, not a malformed plan — failing the job over it would be
// rejecting good work on punctuation.
func TestPlan_AcceptsFencedJSON(t *testing.T) {
	valid := `{"summary":"do the thing","steps":[{"id":"s1","description":"edit main.go"}]}`

	for name, reply := range map[string]string{
		"bare":            valid,
		"fenced":          "```\n" + valid + "\n```",
		"fenced with tag": "```json\n" + valid + "\n```",
		"fenced padded":   "  ```json\n" + valid + "\n```  ",
	} {
		t.Run(name, func(t *testing.T) {
			p := NewPlanner(&model.FakeClient{Reply: reply})
			plan, err := p.Plan(context.Background(), Request{Task: "do the thing"})
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if plan.Summary != "do the thing" || len(plan.Steps) != 1 {
				t.Errorf("unexpected plan %+v", plan)
			}
		})
	}
}

// Unwrapping a fence must not turn genuinely malformed output into a pass.
func TestPlan_StillRejectsMalformedFencedJSON(t *testing.T) {
	p := NewPlanner(&model.FakeClient{Reply: "```json\nnot json at all\n```"})
	if _, err := p.Plan(context.Background(), Request{Task: "x"}); err == nil {
		t.Fatal("expected malformed JSON inside a fence to be rejected")
	}
}
