package planner

import (
	"context"
	"errors"
	"testing"

	"github.com/Raamia/Rojo/internal/agents/model"
)

func TestPlanner_ValidPlan(t *testing.T) {
	fake := &model.FakeClient{Reply: `{"summary":"do it","steps":[{"id":"1","description":"add file"}]}`}
	p := NewPlanner(fake)

	plan, err := p.Plan(context.Background(), Request{Task: "add feature", RepoPath: "/tmp/r"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Summary != "do it" || len(plan.Steps) != 1 {
		t.Errorf("unexpected plan %+v", plan)
	}
}

func TestPlanner_EmptyResponse(t *testing.T) {
	p := NewPlanner(&model.FakeClient{Reply: ""})
	_, err := p.Plan(context.Background(), Request{Task: "x", RepoPath: "/r"})
	if !errors.Is(err, ErrEmptyPlan) {
		t.Errorf("got %v, want ErrEmptyPlan", err)
	}
}

func TestPlanner_MalformedJSON(t *testing.T) {
	p := NewPlanner(&model.FakeClient{Reply: "not json"})
	_, err := p.Plan(context.Background(), Request{Task: "x", RepoPath: "/r"})
	if !errors.Is(err, ErrInvalidPlan) {
		t.Errorf("got %v, want ErrInvalidPlan", err)
	}
}

func TestPlanner_NoSteps(t *testing.T) {
	p := NewPlanner(&model.FakeClient{Reply: `{"summary":"nothing","steps":[]}`})
	_, err := p.Plan(context.Background(), Request{Task: "x", RepoPath: "/r"})
	if !errors.Is(err, ErrInvalidPlan) {
		t.Errorf("got %v, want ErrInvalidPlan", err)
	}
}

func TestPlanner_EmptyTask(t *testing.T) {
	p := NewPlanner(&model.FakeClient{Reply: `{"summary":"x","steps":[{"id":"1","description":"d"}]}`})
	_, err := p.Plan(context.Background(), Request{Task: "", RepoPath: "/r"})
	if !errors.Is(err, ErrInvalidPlan) {
		t.Errorf("got %v, want ErrInvalidPlan", err)
	}
}

// The first live GPT-5.2 run returned a plan whose step ids were numbers, and
// the job died in the planning step with "cannot unmarshal number into Go
// struct field Step.steps.id of type string". A numbered plan is a good plan;
// failing the job over the quoting throws away the call that produced it.
func TestPlan_AcceptsNumericStepIDs(t *testing.T) {
	reply := `{"summary":"add a greeting","steps":[
		{"id":1,"description":"add Greet to greet.go"},
		{"id":2,"description":"call it from main"}
	]}`
	p := NewPlanner(&model.FakeClient{Reply: reply})

	got, err := p.Plan(context.Background(), Request{Task: "add a greeting", RepoPath: "/tmp/r"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(got.Steps) != 2 {
		t.Fatalf("got %d steps, want 2", len(got.Steps))
	}
	if got.Steps[0].ID.String() != "1" || got.Steps[1].ID.String() != "2" {
		t.Errorf("ids = %q, %q; want \"1\", \"2\"", got.Steps[0].ID, got.Steps[1].ID)
	}
}
