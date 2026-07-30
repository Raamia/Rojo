package mcpserver

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Raamia/Rojo/internal/execution"
	"github.com/Raamia/Rojo/internal/verification"
	"github.com/Raamia/Rojo/internal/workspace"
)

// Timeouts for the two kinds of subprocess this server runs. They mirror the
// API server's, and for the same reasons: git operations are fast and a slow
// one is a wedged one, while a test suite legitimately takes minutes.
const (
	gitTimeout    = 5 * time.Minute
	verifyTimeout = 10 * time.Minute
)

// DefaultWorktreeDir is deliberately not the API server's /tmp/rojo-worktrees.
// A running server reclaims orphaned worktrees under its own base directory by
// deriving paths from job ids; these checkouts have no job and no store, so
// keeping them somewhere separate means neither process is ever picking through
// the other's working state.
const DefaultWorktreeDir = "/tmp/rojo-mcp-worktrees"

// Config configures the server. The zero value is usable.
type Config struct {
	// WorktreeDir is where throwaway checkouts are made. Empty uses
	// DefaultWorktreeDir.
	WorktreeDir string
	// MaxCheckOutput and MaxDiff bound what is returned to the caller. Zero
	// uses the package defaults.
	MaxCheckOutput int
	MaxDiff        int
	Logger         *slog.Logger
}

// Version is reported to clients in the MCP handshake.
const Version = "0.1.0"

// toolDescription is the entire contract a model has to work from, so it says
// what the tool guarantees, what it costs, and — the part a description usually
// omits and a model most needs — when *not* to reach for it.
const toolDescription = `Verify proposed file changes against a real git repository, in isolation.

Applies the given file operations inside a throwaway git worktree cut from the
repository, runs that repository's own checks against the result, and reports
what passed, what failed, and the exact diff that was applied.

The checks are chosen by what the repository is: go.mod runs gofmt, go vet and
go test; package.json with a real test script runs npm test; a Python manifest
runs pytest; Cargo.toml runs cargo test. A toolchain that is not installed is
reported as a skipped check rather than a false pass.

Guarantees:
- The repository's working tree and tracked files are NEVER modified. Every
  change lands in a worktree that is deleted before this returns.
- Writes cannot escape the worktree, and cannot touch .git or .env.
- No language model is called, so this costs nothing beyond running the checks.

Use this to find out whether a change actually works before proposing it to the
user. Call it with an empty "operations" list first if you need to know whether
the repository already passes its checks — a pre-existing failure is worth
telling apart from one you introduced.

Do not use this to read files, explore a repository, or apply a change
permanently: it reports on a change and then throws the checkout away.`

// New builds the MCP server with the verification tool registered.
func New(cfg Config) *mcp.Server {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	worktreeDir := cfg.WorktreeDir
	if worktreeDir == "" {
		worktreeDir = DefaultWorktreeDir
	}

	// Two runners with two allowlists, exactly as the API server does it: the
	// workspace half may only ever invoke git, and the verification half only
	// the toolchains detection can choose from. Neither can invoke the other's
	// binaries, so a bug in one cannot borrow the other's reach.
	gitRunner := execution.NewSafeRunner(
		execution.NewExecRunner(),
		execution.NewAllowlist("git"),
		gitTimeout,
	)
	verifyRunner := execution.NewSafeRunner(
		execution.NewExecRunner(),
		execution.NewAllowlist(verification.AutoCommands()...),
		verifyTimeout,
	)

	gate := &Gate{
		Workspaces:     workspace.NewGitWorkspaceManager(gitRunner, worktreeDir),
		Checks:         verification.NewAutoRunner(verifyRunner),
		MaxCheckOutput: cfg.MaxCheckOutput,
		MaxDiff:        cfg.MaxDiff,
		Logger:         log,
	}

	return NewWithGate(gate, log)
}

// NewWithGate registers the tool against a caller-supplied gate. It exists so
// tests can drive the whole MCP surface without running git or a toolchain.
func NewWithGate(gate *Gate, log *slog.Logger) *mcp.Server {
	if log == nil {
		log = slog.Default()
	}
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "rojo",
		Version: Version,
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "rojo_verify",
		Description: toolDescription,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in VerifyRequest) (*mcp.CallToolResult, VerifyResult, error) {
		start := time.Now()
		log.Info("rojo_verify", "repo", in.RepoPath, "operations", len(in.Operations))

		res, err := gate.Verify(ctx, in)
		if err != nil {
			// A usage error — an unreadable repo path, an operation the
			// sandbox refused. Returned as a tool error rather than a protocol
			// one, so the model reads the reason and can correct itself
			// instead of the whole call failing opaquely.
			log.Warn("rojo_verify failed", "repo", in.RepoPath, "err", err)
			return nil, VerifyResult{}, err
		}

		log.Info("rojo_verify complete",
			"repo", in.RepoPath, "passed", res.Passed,
			"checks", len(res.Checks), "took", time.Since(start))

		// Content carries a readable summary and StructuredOutput the full
		// detail. Left to the SDK, Content would be the raw JSON of the result
		// — correct but wasteful, since the caller's first question is only
		// ever "did it pass, and if not, why".
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: summarize(res)}},
		}, res, nil
	})

	return server
}

// summarize renders the result as the few lines a reader actually needs: the
// verdict, what changed, and the output of whatever failed. Passing checks are
// counted rather than listed — their output is noise once the verdict is known.
func summarize(res VerifyResult) string {
	var b strings.Builder

	if res.Passed {
		b.WriteString("PASSED — ")
	} else {
		b.WriteString("FAILED — ")
	}
	b.WriteString(res.Summary)

	if len(res.ChangedFiles) > 0 {
		fmt.Fprintf(&b, "\nChanged %d file(s): %s",
			len(res.ChangedFiles), strings.Join(res.ChangedFiles, ", "))
	} else {
		b.WriteString("\nNo files were changed.")
	}

	// Notes qualify a pass that means less than it looks like it means, so they
	// are surfaced whether or not anything failed.
	for _, c := range res.Checks {
		if c.Note != "" {
			fmt.Fprintf(&b, "\nNote (%s): %s", c.Check, c.Note)
		}
	}

	for _, c := range res.Checks {
		if c.Passed {
			continue
		}
		fmt.Fprintf(&b, "\n\n--- %s FAILED ---\n%s", c.Check, c.Output)
	}

	if len(res.Truncated) > 0 {
		fmt.Fprintf(&b, "\n\n(truncated for length: %s — full values are in the structured output)",
			strings.Join(res.Truncated, ", "))
	}

	return b.String()
}
