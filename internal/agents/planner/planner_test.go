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
