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
	// Tree is what the repository contains. Without it a plan has to guess at
	// the layout, and a confident guess lands the change in the wrong place.
	Tree []string `json:"repository_files,omitempty"`
}

type Step struct {
	// ID is a label, so it is read leniently: a model that numbers its steps
	// 1, 2, 3 rather than "1", "2", "3" has produced a perfectly good plan, and
	// failing the job over that would waste the call that produced it. The
	// first live GPT-5.2 run did exactly this.
	ID          model.LooseString `json:"id"`
	Description string            `json:"description"`
	Files       []string          `json:"files,omitempty"`
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

Return JSON only, with no prose outside it, in exactly this shape:
{"summary":"what the change does","steps":[{"id":"1","description":"what to do","files":["path/to/file.go"]}]}

Rules:
- "id" is a STRING, quoted, even when it is a number: "1", not 1.
- "description" is a string saying what that step changes.
- "files" is optional, and lists repository-relative paths the step touches.
- Keep the plan short: a handful of steps, not a task breakdown of every line.
- "repository_files" lists every file that exists. Plan changes to those paths.
  Only introduce a new file when the change genuinely belongs in one; putting it
  somewhere that merely compiles is a wrong answer that passes the build.`

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
		if strings.TrimSpace(step.ID.String()) == "" || strings.TrimSpace(step.Description) == "" {
			return Plan{}, fmt.Errorf("%w: step %d missing id or description", ErrInvalidPlan, i)
		}
	}
	return plan, nil
}
