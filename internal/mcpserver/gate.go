// Package mcpserver exposes Rojo's isolation-and-verification gate over the
// Model Context Protocol.
//
// The tool it serves is deliberately the half of Rojo that never calls a model:
// take a set of proposed file changes, apply them in a throwaway git worktree,
// run the repository's own checks against them, and report what happened. A
// coding agent on the other end has already decided what to write — what it
// cannot do safely is try that change against a real repository without
// touching it. That is what this lends out.
//
// Consequences of that scope, all of them deliberate:
//
//   - No API key is needed. Nothing here talks to Anthropic or OpenAI, so the
//     caller is not billed twice for tokens it already spent deciding what to
//     change.
//   - No server is needed. There is no job store, no queue, no event bus, and
//     therefore no exclusive lock on a data directory — this runs in-process
//     and stateless, and several calls can be in flight at once.
//   - Nothing persists. A call is a question, not a job: the worktree exists
//     for the duration of the answer and is removed either way.
package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Raamia/Rojo/internal/agents/implementor"
	"github.com/Raamia/Rojo/internal/verification"
	"github.com/Raamia/Rojo/internal/workspace"
)

// Output caps. Both exist for the same reason: what this returns goes into an
// LLM's context window, which is a smaller and more expensive place than a log
// file. A failing test suite prints without limit, and a diff over a large
// change is unbounded too; sending either in full would evict whatever the
// caller was actually working on. The check output is where the cause of a
// failure lives, and the cause is at the top, so a head-truncation loses little.
const (
	DefaultMaxCheckOutput = 8 << 10 // per check
	DefaultMaxDiff        = 256 << 10
)

// cleanupTimeout bounds worktree removal. It runs on a context stripped of
// cancellation, so it needs a deadline of its own or a wedged git would hold
// the call open forever.
const cleanupTimeout = 30 * time.Second

// Operation is one proposed file change.
//
// It mirrors implementor.Operation rather than reusing it so the wire contract
// this tool publishes can carry jsonschema descriptions and evolve separately
// from an internal type. The schema the caller sees is generated from these
// tags, and for a tool an LLM has to use unaided, those descriptions are most
// of what makes it usable.
type Operation struct {
	Kind    string `json:"kind" jsonschema:"the change to make: write (replace the file's entire contents), append (add to the end), or delete"`
	Path    string `json:"path" jsonschema:"repository-relative path, e.g. internal/api/jobs.go. Must not be absolute and must not contain '..'"`
	Content string `json:"content,omitempty" jsonschema:"for write, the COMPLETE new contents of the file, not a patch or fragment; for append, the text to add; omit for delete"`
}

// VerifyRequest is one question: "if these changes were applied, would this
// repository still pass its own checks?"
type VerifyRequest struct {
	RepoPath string `json:"repo_path" jsonschema:"absolute path to the git repository to verify against. It is never modified"`
	// Operations may be empty, which asks the narrower and still useful
	// question of whether the repository passes its checks as it stands —
	// a baseline worth having before blaming a change for a pre-existing
	// failure.
	Operations []Operation `json:"operations,omitempty" jsonschema:"file changes to apply before running the checks. Omit to verify the repository unchanged"`
}

// CheckResult is one deterministic check's outcome.
type CheckResult struct {
	Check    string `json:"check"`
	Passed   bool   `json:"passed"`
	Output   string `json:"output,omitempty"`
	Duration int64  `json:"duration_ms"`
	// Note qualifies a pass that means less than it looks like it means — a
	// toolchain that is not installed, a suite that collected no tests. It is
	// carried through verbatim because the distinction between "verified" and
	// "nothing was checked" is the one a caller most needs and is least likely
	// to ask about.
	Note string `json:"note,omitempty"`
}

// VerifyResult is the answer.
type VerifyResult struct {
	Passed       bool          `json:"passed"`
	Summary      string        `json:"summary"`
	Checks       []CheckResult `json:"checks"`
	ChangedFiles []string      `json:"changed_files,omitempty"`
	Diff         string        `json:"diff,omitempty"`
	// Truncated names the fields that were shortened, so a caller never reads a
	// clipped diff as the whole change. Silent truncation here would be the
	// worst kind: the output still parses, still looks complete, and is wrong.
	Truncated []string `json:"truncated,omitempty"`
}

// CheckRunner is the deterministic gate. Declared here, near its consumer, so
// the gate can be tested without running real toolchains.
type CheckRunner interface {
	Verify(ctx context.Context, dir string) (verification.Report, error)
}

// Gate applies proposed changes in an isolated checkout and runs the
// repository's checks against them.
type Gate struct {
	Workspaces workspace.WorkspaceManager
	Checks     CheckRunner

	// MaxCheckOutput and MaxDiff default to the constants above when zero.
	MaxCheckOutput int
	MaxDiff        int

	Logger *slog.Logger
}

func (g *Gate) log() *slog.Logger {
	if g.Logger != nil {
		return g.Logger
	}
	return slog.Default()
}

func (g *Gate) maxCheckOutput() int {
	if g.MaxCheckOutput <= 0 {
		return DefaultMaxCheckOutput
	}
	return g.MaxCheckOutput
}

func (g *Gate) maxDiff() int {
	if g.MaxDiff <= 0 {
		return DefaultMaxDiff
	}
	return g.MaxDiff
}

var ErrNoRepoPath = errors.New("repo_path is required")

// Verify creates a throwaway worktree, applies the operations, runs the checks,
// and returns the result along with the diff of what was applied.
//
// The source repository is never modified: every write lands in the worktree,
// and the worktree and its branch are removed before this returns, on every
// path including cancellation and panic. A tool that leaves worktrees behind on
// a user's machine is worse than one that does not exist.
func (g *Gate) Verify(ctx context.Context, req VerifyRequest) (VerifyResult, error) {
	if strings.TrimSpace(req.RepoPath) == "" {
		return VerifyResult{}, ErrNoRepoPath
	}
	// Validate before creating anything. A malformed operation is the caller's
	// bug, and finding it after a worktree has been cut means a checkout and a
	// cleanup spent on an answer that was never going to arrive.
	ops, err := toOperations(req.Operations)
	if err != nil {
		return VerifyResult{}, err
	}

	id := newWorktreeID()
	ws, err := g.Workspaces.Create(ctx, id, req.RepoPath)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("create isolated worktree: %w", err)
	}
	defer func() {
		// Stripped of cancellation and separately bounded, for the same reason
		// the pipeline's own cleanup is: by the time a cancelled call reaches
		// here its context is already dead, and a removal that rides on it
		// would be refused — leaving the worktree and its branch behind in
		// exactly the case where nobody is watching for them.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if err := g.Workspaces.Cleanup(cleanupCtx, ws); err != nil {
			g.log().Error("cleanup worktree", "path", ws.Path, "branch", ws.Branch, "err", err)
		}
	}()

	if len(ops) > 0 {
		if err := implementor.New(ws.Path).Apply(ops); err != nil {
			// The sandbox refused something. That is an answer, not a crash:
			// the caller proposed a change that may not be applied, and the
			// reason names which operation and why.
			return VerifyResult{}, fmt.Errorf("apply operations: %w", err)
		}
	}

	report, verifyErr := g.Checks.Verify(ctx, ws.Path)

	result := VerifyResult{
		Passed:  verifyErr == nil && report.AllPassed(),
		Summary: report.Summary(),
		Checks:  make([]CheckResult, 0, len(report.Results)),
	}
	for _, r := range report.Results {
		out, clipped := truncate(r.Output, g.maxCheckOutput())
		if clipped {
			result.Truncated = appendOnce(result.Truncated, "checks["+r.Check+"].output")
		}
		result.Checks = append(result.Checks, CheckResult{
			Check: r.Check, Passed: r.Passed, Output: out,
			Duration: r.Duration, Note: r.Note,
		})
	}

	// A gate that could not be *run* is distinct from one that ran and failed,
	// and collapsing them would tell a caller its change broke the tests when
	// the truth is that no test ever executed. Surfaced as a summary line
	// rather than an error so the caller still receives the diff and whatever
	// partial results exist.
	if verifyErr != nil {
		result.Passed = false
		result.Summary = "verification could not be completed: " + verifyErr.Error()
	}

	// The diff is read while the worktree still exists — the deferred cleanup
	// above removes it on the way out. It is best effort: losing the diff makes
	// the answer less useful, but the check results are the answer.
	if diff, err := g.Workspaces.Diff(ctx, ws); err != nil {
		g.log().Error("read diff", "path", ws.Path, "err", err)
	} else if strings.TrimSpace(diff) != "" {
		result.ChangedFiles = changedFiles(diff)
		clipped := false
		result.Diff, clipped = truncate(diff, g.maxDiff())
		if clipped {
			result.Truncated = appendOnce(result.Truncated, "diff")
		}
	}

	return result, nil
}

// toOperations converts and structurally validates the wire type.
//
// Containment — path escapes, protected paths, size and count limits — is
// enforced by implementor.Apply at the point of writing, which is the only
// place it can be enforced honestly. This checks the shape so an obvious
// mistake is reported as such rather than as a filesystem error.
func toOperations(in []Operation) ([]implementor.Operation, error) {
	out := make([]implementor.Operation, 0, len(in))
	for i, op := range in {
		switch op.Kind {
		case implementor.OpWrite, implementor.OpAppend, implementor.OpDelete:
		default:
			return nil, fmt.Errorf("operation %d: kind must be %q, %q or %q, got %q",
				i, implementor.OpWrite, implementor.OpAppend, implementor.OpDelete, op.Kind)
		}
		if strings.TrimSpace(op.Path) == "" {
			return nil, fmt.Errorf("operation %d: path is required", i)
		}
		out = append(out, implementor.Operation{
			Kind: op.Kind, Path: op.Path, Content: op.Content,
		})
	}
	return out, nil
}

// truncate clips s to max bytes, reporting whether it did. The head is kept:
// for both a failing test suite and a diff, what a reader needs is at the top.
func truncate(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	return s[:max] + "\n... (truncated)", true
}

func appendOnce(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

// newWorktreeID names one call's checkout. The mcp- prefix keeps these
// distinguishable on disk from the server's job worktrees, which are named by
// job id and reclaimed by its startup recovery — these belong to nobody but the
// call in flight.
func newWorktreeID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "mcp-" + hex.EncodeToString(b)
}

// changedFiles lists the paths a unified diff touches, read from the "+++ b/"
// post-image lines with the "--- a/" line as the fallback a deletion needs.
func changedFiles(diff string) []string {
	var (
		out  []string
		seen = map[string]bool{}
		prev string
	)
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "--- "):
			prev = trimDiffPath(strings.TrimPrefix(line, "--- "))
		case strings.HasPrefix(line, "+++ "):
			path := trimDiffPath(strings.TrimPrefix(line, "+++ "))
			if path == "" {
				path = prev
			}
			if path != "" && !seen[path] {
				seen[path] = true
				out = append(out, path)
			}
		}
	}
	return out
}

func trimDiffPath(s string) string {
	s = strings.TrimSpace(s)
	if s == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(strings.TrimPrefix(s, "a/"), "b/")
}
