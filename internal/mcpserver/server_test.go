package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Raamia/Rojo/internal/verification"
)

// connect wires a client to the server over an in-memory transport, so the
// whole MCP surface — schema generation, argument validation, error packing —
// is exercised rather than assumed. Calling the handler directly would test
// none of it.
func connect(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	go func() {
		// Ends when the session closes; the error is expected on teardown.
		_ = server.Run(ctx, serverTransport)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func callVerify(t *testing.T, session *mcp.ClientSession, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "rojo_verify", Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	return res
}

// structured decodes the tool's typed output, which is what a client actually
// programs against.
func structured(t *testing.T, res *mcp.CallToolResult) VerifyResult {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out VerifyResult
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode structured content: %v", err)
	}
	return out
}

func textOf(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestServer_ToolIsAdvertised(t *testing.T) {
	ws := newFakeWorkspaces(t)
	server := NewWithGate(newGate(t, ws, &fakeChecks{report: passingReport()}), nil)
	session := connect(t, server)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools.Tools))
	}
	tool := tools.Tools[0]
	if tool.Name != "rojo_verify" {
		t.Errorf("tool name = %q, want rojo_verify", tool.Name)
	}
	// The description is the entire contract a model works from. These are the
	// two facts most likely to change its behaviour for the better.
	for _, want := range []string{"NEVER modified", "No language model is called"} {
		if !strings.Contains(tool.Description, want) {
			t.Errorf("description is missing %q", want)
		}
	}
	if tool.InputSchema == nil {
		t.Fatal("tool has no input schema; a model cannot call it unaided")
	}
}

// The generated schema must name the fields a caller has to supply, and mark
// repo_path required — an optional repo path would fail at runtime instead of
// being caught by the client's own validation.
func TestServer_InputSchemaDescribesTheArguments(t *testing.T) {
	ws := newFakeWorkspaces(t)
	server := NewWithGate(newGate(t, ws, &fakeChecks{report: passingReport()}), nil)
	session := connect(t, server)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	raw, err := json.Marshal(tools.Tools[0].InputSchema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	schema := string(raw)
	for _, want := range []string{"repo_path", "operations", "kind", "path", "content"} {
		if !strings.Contains(schema, want) {
			t.Errorf("input schema does not mention %q:\n%s", want, schema)
		}
	}
	if !strings.Contains(schema, `"required"`) || !strings.Contains(schema, `"repo_path"`) {
		t.Errorf("repo_path is not marked required:\n%s", schema)
	}
}

func TestServer_VerifyRoundTrip(t *testing.T) {
	ws := newFakeWorkspaces(t)
	ws.diff = "--- a/main.go\n+++ b/main.go\n"
	server := NewWithGate(newGate(t, ws, &fakeChecks{report: passingReport()}), nil)
	session := connect(t, server)

	res := callVerify(t, session, map[string]any{
		"repo_path": "/repo",
		"operations": []map[string]any{
			{"kind": "write", "path": "main.go", "content": "package main\n"},
		},
	})
	if res.IsError {
		t.Fatalf("tool reported an error: %s", textOf(res))
	}

	out := structured(t, res)
	if !out.Passed {
		t.Errorf("Passed = false, want true")
	}
	if len(out.ChangedFiles) != 1 || out.ChangedFiles[0] != "main.go" {
		t.Errorf("ChangedFiles = %v, want [main.go]", out.ChangedFiles)
	}
	if text := textOf(res); !strings.Contains(text, "PASSED") {
		t.Errorf("text content = %q, want a readable PASSED summary", text)
	}
}

// A failing gate is a successful call with a negative answer, not a tool error
// — the caller asked a question and got one.
func TestServer_FailingChecksAreNotAToolError(t *testing.T) {
	ws := newFakeWorkspaces(t)
	checks := &fakeChecks{report: verification.Report{Results: []verification.Result{
		{Check: "go test", Passed: false, Output: "--- FAIL: TestThing"},
	}}}
	server := NewWithGate(newGate(t, ws, checks), nil)
	session := connect(t, server)

	res := callVerify(t, session, map[string]any{"repo_path": "/repo"})
	if res.IsError {
		t.Error("IsError = true for a change that merely failed its checks")
	}
	out := structured(t, res)
	if out.Passed {
		t.Error("Passed = true for a failing report")
	}
	text := textOf(res)
	if !strings.Contains(text, "FAILED") {
		t.Errorf("text = %q, want FAILED", text)
	}
	if !strings.Contains(text, "TestThing") {
		t.Errorf("text = %q, want the failing check's output quoted", text)
	}
}

// A usage error must come back as a tool error so the model reads it and
// corrects itself, rather than as a protocol failure it cannot see.
func TestServer_UsageErrorIsReadableByTheModel(t *testing.T) {
	ws := newFakeWorkspaces(t)
	server := NewWithGate(newGate(t, ws, &fakeChecks{report: passingReport()}), nil)
	session := connect(t, server)

	res := callVerify(t, session, map[string]any{
		"repo_path":  "/repo",
		"operations": []map[string]any{{"kind": "patch", "path": "a.go"}},
	})
	if !res.IsError {
		t.Fatal("IsError = false for an invalid operation kind")
	}
	if text := textOf(res); !strings.Contains(text, "kind must be") {
		t.Errorf("error text = %q, want it to say what a valid kind is", text)
	}
}

func TestSummarize(t *testing.T) {
	t.Run("passing with changes", func(t *testing.T) {
		got := summarize(VerifyResult{
			Passed: true, Summary: "all 3 checks passed",
			ChangedFiles: []string{"a.go", "b.go"},
			Checks:       []CheckResult{{Check: "go test", Passed: true}},
		})
		for _, want := range []string{"PASSED", "all 3 checks passed", "a.go, b.go"} {
			if !strings.Contains(got, want) {
				t.Errorf("summary missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("failing quotes only the failure", func(t *testing.T) {
		got := summarize(VerifyResult{
			Passed: false, Summary: "1 of 2 checks failed",
			Checks: []CheckResult{
				{Check: "gofmt", Passed: true, Output: "noise that should not appear"},
				{Check: "go test", Passed: false, Output: "FAIL: TestX"},
			},
		})
		if !strings.Contains(got, "FAIL: TestX") {
			t.Errorf("summary omits the failing output:\n%s", got)
		}
		if strings.Contains(got, "noise that should not appear") {
			t.Errorf("summary includes a passing check's output:\n%s", got)
		}
	})

	t.Run("no changes is stated", func(t *testing.T) {
		got := summarize(VerifyResult{Passed: true, Summary: "ok"})
		if !strings.Contains(got, "No files were changed") {
			t.Errorf("summary does not say nothing changed:\n%s", got)
		}
	})

	t.Run("notes surface even on a pass", func(t *testing.T) {
		got := summarize(VerifyResult{
			Passed: true, Summary: "all checks passed",
			Checks: []CheckResult{{Check: "pytest", Passed: true, Note: "pytest is not installed"}},
		})
		if !strings.Contains(got, "pytest is not installed") {
			t.Errorf("summary drops the caveat on a qualified pass:\n%s", got)
		}
	})

	t.Run("truncation is disclosed", func(t *testing.T) {
		got := summarize(VerifyResult{Passed: true, Summary: "ok", Truncated: []string{"diff"}})
		if !strings.Contains(got, "truncated") {
			t.Errorf("summary hides that a value was truncated:\n%s", got)
		}
	})
}

// --- end to end, against a real repository and a real toolchain ---

func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed, skipping end-to-end test", tool)
		}
	}
}

// initGoRepo builds a committed Go repository whose tests pass.
func initGoRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("go.mod", "module gatetest\n\ngo 1.25\n")
	write("main.go", "package main\n\nfunc main() {}\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n")
	write("main_test.go", "package main\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n")

	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	run("add", "-A")
	run("commit", "-m", "initial")
	return dir
}

// The whole point of the tool, proven against real git and a real toolchain: a
// good change passes, a bad one is caught, and the repository is untouched
// either way.
func TestEndToEnd_RealRepository(t *testing.T) {
	requireTools(t, "git", "go", "gofmt")

	repo := initGoRepo(t)
	server := New(Config{WorktreeDir: filepath.Join(t.TempDir(), "worktrees")})
	session := connect(t, server)

	t.Run("baseline passes with no operations", func(t *testing.T) {
		res := callVerify(t, session, map[string]any{"repo_path": repo})
		if res.IsError {
			t.Fatalf("tool error: %s", textOf(res))
		}
		out := structured(t, res)
		if !out.Passed {
			t.Fatalf("a clean repository did not pass:\n%s", textOf(res))
		}
		if len(out.Checks) == 0 {
			t.Error("no checks ran against a Go repository")
		}
	})

	t.Run("a good change passes and returns its diff", func(t *testing.T) {
		res := callVerify(t, session, map[string]any{
			"repo_path": repo,
			"operations": []map[string]any{{
				"kind": "append", "path": "main.go",
				"content": "\nfunc Sub(a, b int) int {\n\treturn a - b\n}\n",
			}},
		})
		if res.IsError {
			t.Fatalf("tool error: %s", textOf(res))
		}
		out := structured(t, res)
		if !out.Passed {
			t.Fatalf("a valid change failed the gate:\n%s", textOf(res))
		}
		if !strings.Contains(out.Diff, "func Sub") {
			t.Errorf("diff does not contain the change:\n%s", out.Diff)
		}
		if len(out.ChangedFiles) != 1 || out.ChangedFiles[0] != "main.go" {
			t.Errorf("ChangedFiles = %v, want [main.go]", out.ChangedFiles)
		}
	})

	t.Run("a change that breaks the tests is caught", func(t *testing.T) {
		res := callVerify(t, session, map[string]any{
			"repo_path": repo,
			"operations": []map[string]any{{
				"kind": "write", "path": "main.go",
				"content": "package main\n\nfunc main() {}\n\nfunc Add(a, b int) int {\n\treturn a * b\n}\n",
			}},
		})
		if res.IsError {
			t.Fatalf("tool error: %s", textOf(res))
		}
		out := structured(t, res)
		if out.Passed {
			t.Fatal("a change that breaks TestAdd was reported as passing")
		}
		if !strings.Contains(textOf(res), "TestAdd") {
			t.Errorf("the failing test is not named in the summary:\n%s", textOf(res))
		}
	})

	t.Run("a change that does not compile is caught", func(t *testing.T) {
		res := callVerify(t, session, map[string]any{
			"repo_path": repo,
			"operations": []map[string]any{{
				"kind": "write", "path": "main.go", "content": "package main\nthis is not go\n",
			}},
		})
		if res.IsError {
			t.Fatalf("tool error: %s", textOf(res))
		}
		if structured(t, res).Passed {
			t.Fatal("a file that does not compile was reported as passing")
		}
	})

	t.Run("writing outside the workspace is refused", func(t *testing.T) {
		for _, path := range []string{"../escaped.go", "/etc/passwd", ".git/hooks/pre-commit"} {
			res := callVerify(t, session, map[string]any{
				"repo_path":  repo,
				"operations": []map[string]any{{"kind": "write", "path": path, "content": "x"}},
			})
			if !res.IsError {
				t.Errorf("path %q was accepted; the sandbox must refuse it", path)
			}
		}
	})

	// The guarantee the tool description makes, checked rather than asserted.
	t.Run("the source repository was never modified", func(t *testing.T) {
		cmd := exec.Command("git", "status", "--porcelain")
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git status: %v\n%s", err, out)
		}
		if len(strings.TrimSpace(string(out))) != 0 {
			t.Errorf("the source repository was modified:\n%s", out)
		}

		cmd = exec.Command("git", "branch", "--list")
		cmd.Dir = repo
		branches, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git branch: %v\n%s", err, branches)
		}
		if strings.Contains(string(branches), "rojo/job/") {
			t.Errorf("a job branch was left behind:\n%s", branches)
		}
	})
}

// Concurrent calls must not collide: each gets its own worktree id, path and
// branch, and `git worktree add` is serialised inside the manager.
func TestEndToEnd_ConcurrentCallsDoNotCollide(t *testing.T) {
	requireTools(t, "git", "go", "gofmt")

	repo := initGoRepo(t)
	server := New(Config{WorktreeDir: filepath.Join(t.TempDir(), "worktrees")})
	session := connect(t, server)

	const n = 4
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			res, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name: "rojo_verify",
				Arguments: map[string]any{
					"repo_path": repo,
					"operations": []map[string]any{{
						"kind": "write", "path": "gen.go",
						"content": "package main\n\nvar Generated = " + string(rune('0'+i)) + "\n",
					}},
				},
			})
			if err != nil {
				errs <- err
				return
			}
			if res.IsError {
				errs <- errInvalid(textOf(res))
				return
			}
			errs <- nil
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent call %d: %v", i, err)
		}
	}
}

type errInvalid string

func (e errInvalid) Error() string { return string(e) }
