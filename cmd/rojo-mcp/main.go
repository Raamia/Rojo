// Command rojo-mcp serves Rojo's verification gate over the Model Context
// Protocol, so any MCP client — Claude Code, Cursor, or anything else that
// speaks the protocol — can hand it a proposed change and find out whether the
// repository still passes its own checks.
//
// It is a third binary rather than a mode of cmd/api because it shares nothing
// with the server: no job store, no queue, no data directory and therefore no
// exclusive lock, no API key. It can run beside a running rojo-api, or with no
// rojo-api anywhere.
//
// Configure a client to launch it over stdio:
//
//	{"mcpServers": {"rojo": {"command": "/path/to/rojo-mcp"}}}
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Raamia/Rojo/internal/mcpserver"
)

func main() {
	// stderr, not stdout. The stdio transport *is* stdout: a single log line
	// written there is interleaved into the JSON-RPC stream and breaks the
	// session, which presents as a client that connects and then mysteriously
	// sees no tools. cmd/api logs to stdout; this one must not.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg := mcpserver.Config{
		WorktreeDir:    os.Getenv("ROJO_MCP_WORKTREE_DIR"),
		MaxCheckOutput: envInt("ROJO_MCP_MAX_CHECK_OUTPUT"),
		MaxDiff:        envInt("ROJO_MCP_MAX_DIFF"),
		Logger:         logger,
	}
	server := mcpserver.New(cfg)

	// A client that goes away closes the transport and Run returns; the signal
	// handler covers the other direction, where the user stops the process
	// directly. Cancelling the context closes the connection, and any
	// verification in flight unwinds through its own cleanup — the worktree is
	// removed on a context stripped of cancellation precisely so that this path
	// does not leave one behind.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("rojo mcp server starting",
		"version", mcpserver.Version, "transport", "stdio")

	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		// A cancelled context is the ordinary way this ends, not a failure.
		if ctx.Err() != nil {
			logger.Info("rojo mcp server stopped")
			return
		}
		logger.Error("rojo mcp server failed", "err", err)
		os.Exit(1)
	}
}

// envInt reads an optional integer setting, falling back to 0 — which every
// consumer reads as "use the default" — for both unset and unparseable values,
// consistent with how internal/config treats its own typed variables.
func envInt(key string) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
