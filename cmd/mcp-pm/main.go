// Command mcp-pm is a minimal project-management MCP server. Slice A owns the
// RISKS domain: a single Qdrant-backed collection (pm_risks) exposed via four
// tools (pm_risk_create / pm_risk_list / pm_risk_update / pm_risk_link).
package main

import (
	"context"
	"fmt"
	"os"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/loomconcurrency"
	"github.com/crb2nu/loom/pkg/lifecycle"
	"github.com/crb2nu/loom/pkg/mcplog"
	"github.com/crb2nu/loom/pkg/pm"
)

var version = "0.1.0"

func main() {
	if err := lifecycle.RunWithSignals(context.Background(), run); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	logger := mcplog.NewDefault()

	svc := pm.NewServiceFromEnv(logger)

	logger.Info("starting server", "name", "mcp-pm", "version", version)

	server := mcp.NewServer("mcp-pm", version)
	loomconcurrency.Apply(server)
	server.SetInstructions(`Project Management MCP Server (risks domain)

This server owns the RISKS store for unified project tracking. Risks are keyed
by a canonical project identifier — the GitLab path_with_namespace, e.g.
"services/flexdeck".

Tools:
- pm_risk_create : create a risk (project, title, likelihood, impact, mitigation, owner, status)
- pm_risk_list   : list risks, optionally filtered by project and/or status
- pm_risk_update : update mutable fields of an existing risk by id
- pm_risk_link   : append a reference (gitlab issue path or task id) to a risk's links

Likelihood/impact: low | medium | high. Status: identified | mitigating | accepted | closed.

Writes are decoupled from embedding: a risk always persists even when the
shared embedder is unavailable (a fallback vector keeps the point filterable).`)

	registerTools(server, svc)

	return server.Run(ctx)
}
