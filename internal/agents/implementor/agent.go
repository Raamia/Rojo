package implementor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Raamia/Rojo/internal/agents/model"
	"github.com/Raamia/Rojo/internal/agents/planner"
)

var (
	ErrNoOperations      = errors.New("implementor proposed no operations")
	ErrInvalidOperations = errors.New("implementor returned invalid operations")
)

// SourceFile is one file the model is allowed to see when proposing changes.
type SourceFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Request is everything the model needs to propose a change: what was asked,
// how it was planned, and the current contents of the relevant files.
type Request struct {
	Task  string       `json:"task"`
	Plan  planner.Plan `json:"plan"`
	Files []SourceFile `json:"files,omitempty"`
	// Feedback carries a reviewer's notes from a previous attempt, so a
	// revision knows what was wrong rather than starting over blind.
	Feedback string `json:"prior_feedback,omitempty"`
}

// Agent turns a plan into concrete file operations.
//
// It is deliberately separate from Implementor: this one talks to a model and
// produces a proposal, while Implementor validates that proposal and writes it
// to disk inside a sandbox. Keeping them apart means the dangerous half — the
// part that touches the filesystem — never depends on a model being involved,
// and stays testable without one.
type Agent struct {
	Client model.Client
}

func NewAgent(c model.Client) *Agent { return &Agent{Client: c} }

const systemPrompt = `You are the Rojo implementor. You are given a task, a plan, and the current contents of the relevant files.

Return JSON only, with no prose, in exactly this shape:
{"operations":[{"kind":"write","path":"relative/path.go","content":"<full new file contents>"}]}

Rules:
- "kind" is one of "write", "append", or "delete".
- "path" is relative to the repository root. Never absolute, never containing "..".
- For "write", "content" must be the COMPLETE new contents of the file, not a patch or a fragment.
- For "delete", omit "content".
- Only include files you are actually changing.
- The result must compile and pass the existing tests.`

// Propose asks the model for the operations that would carry out the plan.
//
// The response is validated structurally here — shape, kinds, non-emptiness —
// and again for safety by Implementor.Apply, which is what enforces the
// sandbox. This layer catches "the model returned nonsense"; that one catches
// "the model tried to escape".
func (a *Agent) Propose(ctx context.Context, req Request) ([]Operation, error) {
	if strings.TrimSpace(req.Task) == "" {
		return nil, fmt.Errorf("%w: task is empty", ErrInvalidOperations)
	}

	prompt, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal implementor request: %w", err)
	}

	resp, err := a.Client.Generate(ctx, model.Request{
		System: systemPrompt,
		Prompt: string(prompt),
	})
	if err != nil {
		return nil, fmt.Errorf("model call: %w", err)
	}

	var out struct {
		Operations []Operation `json:"operations"`
	}
	if err := json.Unmarshal([]byte(unfence(resp.Text)), &out); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidOperations, err)
	}
	if len(out.Operations) == 0 {
		return nil, ErrNoOperations
	}

	for i, op := range out.Operations {
		switch op.Kind {
		case OpWrite, OpAppend, OpDelete:
		default:
			return nil, fmt.Errorf("%w: op %d has unknown kind %q", ErrInvalidOperations, i, op.Kind)
		}
		if strings.TrimSpace(op.Path) == "" {
			return nil, fmt.Errorf("%w: op %d has no path", ErrInvalidOperations, i)
		}
	}
	return out.Operations, nil
}

// unfence strips a markdown code fence from around a JSON body. Models wrap
// output in ```json even when told not to; that is a formatting habit rather
// than a bad proposal, and failing a job over punctuation would reject good
// work. Anything still invalid after unwrapping is rejected as before.
func unfence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
