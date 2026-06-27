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

// unfence strips a markdown code fence from around a JSON body.
//
// The system prompt asks for JSON only, but models routinely wrap it in
// ```json ... ``` anyway, and that is a formatting habit rather than a
// malformed plan — rejecting it would fail jobs over punctuation. Anything
// still not valid JSON after unwrapping is rejected as before.
func unfence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:] // drop the opening fence and any language tag
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

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
	if err := json.Unmarshal([]byte(unfence(resp.Text)), &plan); err != nil {
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
