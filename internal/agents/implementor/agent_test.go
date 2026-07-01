package implementor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Raamia/Rojo/internal/agents/model"
	"github.com/Raamia/Rojo/internal/agents/planner"
)

func samplePlan() planner.Plan {
	return planner.Plan{
		Summary: "add a greeting",
		Steps:   []planner.Step{{ID: "s1", Description: "write greet.go"}},
	}
}

func TestPropose_ParsesOperations(t *testing.T) {
	reply := `{"operations":[
		{"kind":"write","path":"greet.go","content":"package main\n"},
		{"kind":"delete","path":"old.go"}
	]}`
	a := NewAgent(&model.FakeClient{Reply: reply})

	ops, err := a.Propose(context.Background(), Request{Task: "add a greeting", Plan: samplePlan()})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("got %d ops, want 2", len(ops))
	}
	if ops[0].Kind != OpWrite || ops[0].Path != "greet.go" {
		t.Errorf("unexpected first op %+v", ops[0])
	}
	if ops[1].Kind != OpDelete {
		t.Errorf("unexpected second op %+v", ops[1])
	}
}

func TestPropose_AcceptsFencedJSON(t *testing.T) {
	body := `{"operations":[{"kind":"write","path":"a.go","content":"package a\n"}]}`
	for name, reply := range map[string]string{
		"fenced":     "```\n" + body + "\n```",
		"tagged":     "```json\n" + body + "\n```",
		"whitespace": "  ```json\n" + body + "\n```  ",
	} {
		t.Run(name, func(t *testing.T) {
			a := NewAgent(&model.FakeClient{Reply: reply})
			ops, err := a.Propose(context.Background(), Request{Task: "t", Plan: samplePlan()})
			if err != nil {
				t.Fatalf("propose: %v", err)
			}
			if len(ops) != 1 {
				t.Errorf("got %d ops, want 1", len(ops))
			}
		})
	}
}

// A model that proposes nothing has not done the task. Treating that as success
// would produce an empty diff and a job that claims to have worked.
func TestPropose_EmptyOperationsIsAnError(t *testing.T) {
	for name, reply := range map[string]string{
		"empty array": `{"operations":[]}`,
		"no field":    `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			a := NewAgent(&model.FakeClient{Reply: reply})
			if _, err := a.Propose(context.Background(), Request{Task: "t", Plan: samplePlan()}); !errors.Is(err, ErrNoOperations) {
				t.Errorf("got %v, want ErrNoOperations", err)
			}
		})
	}
}

func TestPropose_RejectsMalformedOutput(t *testing.T) {
	tests := map[string]string{
		"not json":     "I have decided to write greet.go for you.",
		"unknown kind": `{"operations":[{"kind":"chmod","path":"a.go"}]}`,
		"missing path": `{"operations":[{"kind":"write","content":"x"}]}`,
		"blank path":   `{"operations":[{"kind":"write","path":"   ","content":"x"}]}`,
	}
	for name, reply := range tests {
		t.Run(name, func(t *testing.T) {
			a := NewAgent(&model.FakeClient{Reply: reply})
			if _, err := a.Propose(context.Background(), Request{Task: "t", Plan: samplePlan()}); !errors.Is(err, ErrInvalidOperations) {
				t.Errorf("got %v, want ErrInvalidOperations", err)
			}
		})
	}
}

func TestPropose_ModelErrorPropagates(t *testing.T) {
	boom := errors.New("anthropic rate limited after retries")
	a := NewAgent(&model.FakeClient{Err: boom})
	if _, err := a.Propose(context.Background(), Request{Task: "t", Plan: samplePlan()}); !errors.Is(err, boom) {
		t.Errorf("got %v, want the underlying cause", err)
	}
}

func TestPropose_RejectsEmptyTask(t *testing.T) {
	a := NewAgent(&model.FakeClient{Reply: `{"operations":[{"kind":"write","path":"a.go"}]}`})
	if _, err := a.Propose(context.Background(), Request{Task: "  ", Plan: samplePlan()}); !errors.Is(err, ErrInvalidOperations) {
		t.Errorf("got %v, want ErrInvalidOperations", err)
	}
}

// The prompt must actually carry the task, plan, file contents and any prior
// feedback — a model cannot act on context it was never sent.
func TestPropose_PromptCarriesTheContext(t *testing.T) {
	fake := &promptCapturingClient{reply: `{"operations":[{"kind":"write","path":"a.go","content":"x"}]}`}
	a := NewAgent(fake)

	_, err := a.Propose(context.Background(), Request{
		Task:     "add a greeting",
		Plan:     samplePlan(),
		Files:    []SourceFile{{Path: "greet.go", Content: "package main // existing"}},
		Feedback: "the previous attempt broke the build",
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	for _, want := range []string{
		"add a greeting",           // task
		"write greet.go",           // plan step
		"package main // existing", // file contents
		"broke the build",          // reviewer feedback
	} {
		if !strings.Contains(fake.prompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
}

type promptCapturingClient struct {
	prompt string
	reply  string
}

func (c *promptCapturingClient) Generate(_ context.Context, req model.Request) (model.Response, error) {
	c.prompt = req.Prompt
	return model.Response{Text: c.reply, Model: "fake"}, nil
}

// The two halves compose: what the agent proposes, the sandbox applies — and
// the sandbox is still what refuses anything hostile.
func TestProposeThenApply(t *testing.T) {
	ws := t.TempDir()
	reply := `{"operations":[{"kind":"write","path":"pkg/greet.go","content":"package pkg\n\nfunc Greet() string { return \"hi\" }\n"}]}`

	ops, err := NewAgent(&model.FakeClient{Reply: reply}).
		Propose(context.Background(), Request{Task: "add greet", Plan: samplePlan()})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if err := New(ws).Apply(ops); err != nil {
		t.Fatalf("apply: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(ws, "pkg", "greet.go"))
	if err != nil {
		t.Fatalf("expected the file to be written: %v", err)
	}
	if !strings.Contains(string(got), "func Greet()") {
		t.Errorf("unexpected contents %q", got)
	}
}

// A proposal that tries to escape the workspace is refused at the write layer,
// no matter how convincing the model was.
func TestProposeThenApply_SandboxStillRefusesEscape(t *testing.T) {
	ws := t.TempDir()
	reply := `{"operations":[{"kind":"write","path":"../escaped.go","content":"package x"}]}`

	ops, err := NewAgent(&model.FakeClient{Reply: reply}).
		Propose(context.Background(), Request{Task: "escape", Plan: samplePlan()})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if err := New(ws).Apply(ops); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("apply = %v, want ErrPathEscape", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(ws), "escaped.go")); err == nil {
		t.Fatal("a file was written outside the workspace")
	}
}
