package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/loomconcurrency"
	"github.com/crb2nu/loom/pkg/env"
	"github.com/crb2nu/loom/pkg/flexinfer"
	"github.com/crb2nu/loom/pkg/lifecycle"
	"github.com/crb2nu/loom/pkg/mcplog"
	"github.com/crb2nu/loom/pkg/mcpotel"
	"github.com/crb2nu/loom/pkg/openairesponses"
	"github.com/crb2nu/loom/pkg/weaver"
)

var version = "1.0.0"

func main() {
	if err := lifecycle.RunWithSignals(context.Background(), run); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	logger := mcplog.NewDefault()

	// Initialize OTel tracing (noop when OTEL_EXPORTER_OTLP_ENDPOINT is unset).
	tp, shutdownTracer, err := mcpotel.InitTracer(ctx, "mcp-weaver", logger)
	if err != nil {
		logger.Warn("OTel tracer init failed, continuing without tracing", "error", err)
	}
	defer shutdownTracer(ctx)

	// Load configuration.
	cfg := weaver.LoadConfigFromEnv()
	cfg.Enabled = true // standalone binary is always enabled

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid weaver config: %w", err)
	}

	// Require FLEXINFER_URL and MCP_HUB_URL.
	flexinferURL := env.String("FLEXINFER_URL", "")
	if flexinferURL == "" {
		return fmt.Errorf("FLEXINFER_URL is required")
	}
	hubURL := env.String("MCP_HUB_URL", "")
	if hubURL == "" {
		return fmt.Errorf("MCP_HUB_URL is required")
	}

	// Create FlexInfer client. When FLEXINFER_URL points at a keyed
	// LiteLLM gateway but no API key is configured, weaver's calls fail
	// with 401 ("No api key passed in"); fall back to the keyless
	// FlexInfer proxy when one is configured. See flexinfer.ResolveBaseURL.
	apiKey := env.String("FLEXINFER_API_KEY", "")
	proxyURL := env.String("FLEXINFER_PROXY_URL", "")
	if apiKey == "" && proxyURL != "" {
		logger.Info("no FlexInfer gateway key configured; routing through keyless FlexInfer proxy", "proxy_url", proxyURL)
	}
	flexinferURL = flexinfer.ResolveBaseURL(flexinferURL, apiKey, proxyURL)
	breaker := flexinfer.NewCircuitBreaker(5, 30*time.Second)
	flexClient := flexinfer.NewClient(flexinferURL, apiKey, cfg.Timeout, breaker, logger)

	// Load model behaviors from YAML (overrides defaults in cfg).
	// Mirrors the daemon-embedded path so the deployed standalone
	// server isn't a reduced-capability variant.
	if bs, err := weaver.LoadBehaviorsFromFile(weaver.DefaultBehaviorsPath()); err != nil {
		logger.Warn("failed to load behaviors YAML", "error", err)
	} else if bs != nil {
		for k, v := range bs {
			cfg.ModelBehaviors[k] = v
		}
		logger.Debug("loaded behaviors from YAML", "count", len(bs))
	}

	// Create hub-based tool lister and executor.
	lister := NewHubToolLister(hubURL)
	caller := NewHubToolCaller(hubURL)
	executor := weaver.NewDaemonToolExecutor(caller, cfg.Timeout)

	// Create the weaver router.
	router := weaver.NewRouter(cfg, flexClient, executor, lister, logger)
	router.SetMetrics(weaver.NewMetrics(nil))

	tracer := mcpotel.Tracer(tp, "mcp-weaver")
	router.SetTracer(tracer)

	// Load YAML domain overrides (same file the daemon path honors).
	yamlPath := weaver.DefaultDomainsPath()
	if err := weaver.MergeDomainsIntoRegistry(router.Registry(), yamlPath); err != nil {
		logger.Warn("failed to load YAML domains", "path", yamlPath, "error", err)
	}

	// Validate domain tool references against hub-advertised tools.
	if warnings := router.Registry().ValidateTools(lister); len(warnings) > 0 {
		for _, w := range warnings {
			logger.Warn(w)
		}
	}

	// Non-blocking model preflight: compare configured models against
	// the FlexInfer catalog so loom/weaver/status reports degraded
	// bindings instead of silent 404s on the first query. The deployed
	// standalone server previously had no preflight at all.
	preflight := weaver.RunPreflight(ctx, flexClient, cfg, router.Registry())
	if preflight.Degraded {
		logger.Warn("model preflight degraded",
			"missing_models", preflight.MissingModels,
			"catalog_size", preflight.CatalogSize,
			"catalog_error", preflight.CatalogError,
		)
	} else {
		logger.Info("model preflight ok",
			"ready_models", preflight.ReadyModels,
			"catalog_size", preflight.CatalogSize,
		)
	}

	logger.Info("starting server",
		"name", "mcp-weaver",
		"version", version,
		"flexinfer_url", flexinferURL,
		"hub_url", hubURL,
		"router_model", cfg.RouterModel,
		"subagent_model", cfg.SubagentModel,
	)

	server := mcp.NewServer("mcp-weaver", version)
	loomconcurrency.Apply(server)
	server.SetInstructions(`Orchestra MCP Server (standalone)

This server provides multi-tool orchestrated queries using local AI models
via FlexInfer. It routes queries to domain-specific subagents that use
tools from the MCP gateway, then synthesizes compressed answers.

Tools:
- weaver__query: Auto-classify and dispatch to relevant domains
- weaver__gather: Dispatch to specified domains (no auto-classification)
- weaver__cluster_status: Cluster health overview (pods, deployments, alerts)
- weaver__ci_status: CI/CD pipeline status and merge requests
- weaver__fleet_status: Agent fleet activity and session overview
- weaver__deploy_status: GitOps deploy/reconciliation status
- weaver__incident_triage: Cross-domain incident triage sweep
- weaver__codebase_overview: Codebase structure and index overview
- weaver__system_health: Comprehensive system health report
- loom/weaver/status: Show weaver configuration, model preflight, and available domains
- loom/weaver/history: Show recent orchestrated queries for HUD history
- loom/weaver/metrics: Show lifetime weaver metrics for HUD summaries

Environment:
- FLEXINFER_URL: FlexInfer proxy endpoint (required)
- MCP_HUB_URL: MCP gateway URL for tool routing (required)
- WEAVER_ROUTER_MODEL: Model for query classification
- WEAVER_SUBAGENT_MODEL: Model for domain subagents
- WEAVER_MAX_ITERATIONS: Max tool-call iterations per subagent
- WEAVER_MAX_CONCURRENT: Max parallel domain dispatches`)

	registerWeaverTools(server, router, preflight, logger)

	return server.Run(ctx)
}

// registerWeaverTools registers all weaver tools on the MCP server.
func registerWeaverTools(server *mcp.Server, router *weaver.Router, preflight weaver.Preflight, logger *slog.Logger) {
	// weaver__query
	server.AddTool(mcp.Tool{
		Name:        "weaver__query",
		Description: "Execute a multi-tool orchestrated query using local AI models. Routes the query to domain-specific subagents that use 5-10 tools in parallel, then synthesizes a compressed answer.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "The natural language query to answer using multiple tools.",
				},
				"domains": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional domain filter. If empty, the router auto-classifies. Available: cluster-ops, codebase, ci-pipeline, observability.",
				},
				"max_tokens": map[string]any{
					"type":        "integer",
					"description": "Optional max tokens for the synthesized response.",
				},
			},
			Required: []string{"query"},
		},
	}, handleQuery(router, logger))

	// weaver__gather
	server.AddTool(mcp.Tool{
		Name:        "weaver__gather",
		Description: "Execute an orchestrated query against specific domains (no auto-classification). Useful when you know which domains to query.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "The natural language query to answer.",
				},
				"domains": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Domains to query. Required. Available: cluster-ops, codebase, ci-pipeline, observability.",
				},
			},
			Required: []string{"query", "domains"},
		},
	}, handleGather(router, logger))

	// Compound tools.
	for _, ct := range weaver.DefaultCompoundTools() {
		ct := ct // capture loop variable
		server.AddTool(mcp.Tool{
			Name:        ct.Name,
			Description: ct.Description,
			InputSchema: mcp.InputSchema{
				Type: "object",
				Properties: map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Optional custom query to override the default.",
					},
				},
			},
		}, handleCompound(router, ct, logger))
	}

	// loom/weaver/status
	server.AddTool(mcp.Tool{
		Name:        "loom/weaver/status",
		Description: "Show weaver configuration and available domains.",
		InputSchema: mcp.InputSchema{
			Type:       "object",
			Properties: map[string]any{},
		},
	}, handleStatus(router, preflight))

	server.AddTool(mcp.Tool{
		Name:        "loom/weaver/history",
		Description: "Show recent orchestrated query history for HUD display.",
		InputSchema: mcp.InputSchema{
			Type:       "object",
			Properties: map[string]any{},
		},
	}, handleHistory(router))

	server.AddTool(mcp.Tool{
		Name:        "loom/weaver/metrics",
		Description: "Show lifetime weaver metrics for HUD summaries.",
		InputSchema: mcp.InputSchema{
			Type:       "object",
			Properties: map[string]any{},
		},
	}, handleMetrics(router))
}

// handleQuery returns a tool handler for weaver__query.
func handleQuery(router *weaver.Router, logger *slog.Logger) mcp.ToolHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		query, _ := args["query"].(string)
		if query == "" {
			return mcp.ErrorResult(fmt.Errorf("query is required")), nil
		}

		domains := parseStringSlice(args["domains"])

		var maxTokens int
		if mt, ok := args["max_tokens"].(float64); ok {
			maxTokens = int(mt)
		}

		req := weaver.QueryRequest{
			Query:     query,
			Domains:   domains,
			MaxTokens: maxTokens,
		}

		result, err := router.Query(ctx, req)
		if err != nil {
			logger.Warn("weaver query failed", "error", err)
			return mcp.ErrorResult(fmt.Errorf("weaver query failed: %w", err)), nil
		}

		return mcp.JSONResult(result)
	}
}

// handleGather returns a tool handler for weaver__gather.
func handleGather(router *weaver.Router, logger *slog.Logger) mcp.ToolHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		query, _ := args["query"].(string)
		if query == "" {
			return mcp.ErrorResult(fmt.Errorf("query is required")), nil
		}

		domains := parseStringSlice(args["domains"])
		if len(domains) == 0 {
			return mcp.ErrorResult(fmt.Errorf("domains is required for gather")), nil
		}

		result, err := router.Gather(ctx, domains, query, openairesponses.ExecutionIdentity{})
		if err != nil {
			logger.Warn("weaver gather failed", "error", err)
			return mcp.ErrorResult(fmt.Errorf("weaver gather failed: %w", err)), nil
		}

		return mcp.JSONResult(result)
	}
}

// handleCompound returns a tool handler for a compound weaver tool.
func handleCompound(router *weaver.Router, ct weaver.CompoundTool, logger *slog.Logger) mcp.ToolHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		raw, _ := json.Marshal(args)
		result, ok := weaver.HandleCompoundTool(ctx, router, ct.Name, raw, openairesponses.ExecutionIdentity{}, logger)
		if !ok {
			return mcp.ErrorResult(fmt.Errorf("unknown compound tool: %s", ct.Name)), nil
		}
		return mcp.TextResult(string(result)), nil
	}
}

// handleStatus returns a tool handler for loom/weaver/status. The boot-time
// model preflight is merged in with the same field names the daemon path
// uses, so HUD/iOS/extension render the degraded banner regardless of
// which deployment shape served the call.
func handleStatus(router *weaver.Router, preflight weaver.Preflight) mcp.ToolHandler {
	return func(_ context.Context, _ map[string]any) (*mcp.CallToolResult, error) {
		status := router.Status()
		status["degraded"] = preflight.Degraded
		status["missing_models"] = preflight.MissingModels
		status["ready_models"] = preflight.ReadyModels
		status["catalog_size"] = preflight.CatalogSize
		if preflight.CatalogError != "" {
			status["catalog_error"] = preflight.CatalogError
		}
		status["preflight_at"] = preflight.CheckedAt.Format(time.RFC3339)
		return mcp.JSONResult(status)
	}
}

func handleHistory(router *weaver.Router) mcp.ToolHandler {
	return func(_ context.Context, _ map[string]any) (*mcp.CallToolResult, error) {
		history := router.History()
		// Present newest entries first for HUD consumption.
		for left, right := 0, len(history)-1; left < right; left, right = left+1, right-1 {
			history[left], history[right] = history[right], history[left]
		}
		return mcp.JSONResult(map[string]any{
			"entries": history,
		})
	}
}

func handleMetrics(router *weaver.Router) mcp.ToolHandler {
	return func(_ context.Context, _ map[string]any) (*mcp.CallToolResult, error) {
		metrics := router.MetricsSummary()
		if metrics == nil {
			metrics = map[string]any{
				"total_queries":  0,
				"avg_latency_ms": 0,
				"error_rate":     0,
				"total_tokens":   0,
				"error_count":    0,
			}
		}
		return mcp.JSONResult(metrics)
	}
}

// parseStringSlice extracts a []string from a JSON-decoded interface value
// (which is typically []any after JSON unmarshaling).
func parseStringSlice(v any) []string {
	if v == nil {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
