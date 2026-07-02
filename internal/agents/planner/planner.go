package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Raamia/Rojo/internal/agents/model"
)

var (
	ErrEmptyPlan   = errors.New("planner returned empty plan")
	ErrInvalidPlan = errors.New("planner returned invalid plan")
)

type Request struct {
	Task     string   `json:"task"`
	RepoPath string   `json:"repo_path"`
	Files    []string `json:"context_files,omitempty"`
}

type Step struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Files       []string `json:"files,omitempty"`
}

type Plan struct {
	Summary string `json:"summary"`
	Steps   []Step `json:"steps"`
}

type Planner struct {
	Client model.Client
}

func NewPlanner(c model.Client) *Planner {
	return &Planner{Client: c}
}

const systemPrompt = `You are the Rojo planner. Given a task, return a JSON plan with fields "summary" and "steps".
Each step must have "id", "description", and optionally "files". Do not include prose outside the JSON.`

func (p *Planner) Plan(ctx context.Context, req Request) (Plan, error) {
	if strings.TrimSpace(req.Task) == "" {
		return Plan{}, fmt.Errorf("%w: task is empty", ErrInvalidPlan)
	}

	prompt, err := json.Marshal(req)
	if err != nil {
		return Plan{}, fmt.Errorf("marshal planner request: %w", err)
	}

	resp, err := p.Client.Generate(ctx, model.Request{
		System: systemPrompt,
		Prompt: string(prompt),
	})
	if err != nil {
		return Plan{}, fmt.Errorf("model call: %w", err)
	}
	if strings.TrimSpace(resp.Text) == "" {
		return Plan{}, ErrEmptyPlan
	}

	var plan Plan
	if err := json.Unmarshal([]byte(model.Unfence(resp.Text)), &plan); err != nil {
		return Plan{}, fmt.Errorf("%w: %v", ErrInvalidPlan, err)
	}
	if len(plan.Steps) == 0 {
		return Plan{}, fmt.Errorf("%w: no steps", ErrInvalidPlan)
	}
	for i, step := range plan.Steps {
		if strings.TrimSpace(step.ID) == "" || strings.TrimSpace(step.Description) == "" {
			return Plan{}, fmt.Errorf("%w: step %d missing id or description", ErrInvalidPlan, i)
		}
	}
	return plan, nil
}
