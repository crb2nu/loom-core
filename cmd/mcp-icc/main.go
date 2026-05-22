// mcp-icc is the broad MCP server for the Integration Command Center
// (ICC). It exposes the full entity surface (~20 read + ~30 write
// tools) so MCP callers can drive ICC end-to-end without resorting
// to raw HTTP. The narrow mcp-icc-capture server remains for the
// note-capture workflow; mcp-icc covers everything else (projects,
// artifacts, action items, decisions, risks, milestones,
// deliverables, dependencies, code refs, session links).
//
// Write tools are gated behind ICC_MCP_WRITE_ENABLED=1 — when the env
// var is unset (or any value other than "1"), every write tool is
// registered but returns a "writes_disabled" error immediately without
// touching the network. Read tools are always enabled.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/crb2nu/loom/internal/iccclient"
	"github.com/crb2nu/loom/pkg/env"
	"github.com/crb2nu/loom/pkg/lifecycle"
	"github.com/crb2nu/loom/pkg/mcpscaffold"
)

var version = "0.1.0"

func main() {
	if err := lifecycle.RunWithSignals(context.Background(), run); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	srv, cleanup, err := mcpscaffold.NewServer(ctx, "mcp-icc", version,
		mcpscaffold.WithInstructions(`ICC Workbench MCP Server

Tools for the broad ICC entity surface: projects, artifacts, action
items, decisions, risks, milestones, deliverables, dependencies,
code refs, session links, plus convenience read endpoints
(project brief / kanban / calendar / gantt / status / blocked,
needs-attention, search).

Read tools are always enabled. Write tools are gated behind
ICC_MCP_WRITE_ENABLED=1; when unset they return a "writes_disabled"
error so callers can probe without side effects.

Configure ICC_BASE_URL to point at the ICC backend (default port
8765). The trusted-context handshake (Origin + X-Requested-With
headers) is applied automatically — no token plumbing is required
in this slice; HMAC is a future hardening step.`),
	)
	if err != nil {
		return err
	}
	defer func() { _ = cleanup(ctx) }()

	icc := iccclient.New(srv.Logger)
	writesEnabled := env.String("ICC_MCP_WRITE_ENABLED", "") == "1"

	registerTools(srv, icc, writesEnabled)

	return srv.Run(ctx)
}
