// mcp-agent-loop exposes the F4-tool-loop-as-prefix ReAct engine as an MCP
// tool. It mirrors the flexinfer agent-loop CLI (slice 1) so the same
// append-only, prefix-cache-paying loop is available to any MCP client.
//
// Tools:
//   - agent_loop_run: drive a bounded ReAct session against an
//     OpenAI-compatible endpoint (the flexinfer proxy), pinning a session
//     cache key and running real read-only, path-jailed tools. Returns a
//     per-turn metrics report (the F4 signal: flat upstream_ms vs growing
//     prompt_tokens).
//   - agent_loop_self_check: run the offline self-check harness (no cluster
//     required) — the dev/ops gate that proves the engine wiring end-to-end.
package main

import (
	"context"
	"fmt"
	"os"

	"gitlab.flexinfer.ai/libs/mcp-go"

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
	srv, cleanup, err := mcpscaffold.NewServer(ctx, "mcp-agent-loop", version,
		mcpscaffold.WithInstructions("F4 append-only tool-loop ReAct engine. Tools: agent_loop_run (live loop vs an OpenAI-compatible endpoint with prefix-cache pinning + per-turn metrics), agent_loop_self_check (offline wiring gate)."),
	)
	if err != nil {
		return err
	}
	defer func() { _ = cleanup(ctx) }()

	srv.AddTracedTool(mcp.Tool{
		Name:        "agent_loop_run",
		Description: "Drive a bounded append-only ReAct loop against an OpenAI-compatible chat endpoint (the flexinfer proxy). The system prompt + fixed tool set form an immutable prefix; history grows by append only, and the session cache key pins prefix-consistent routing so the KV prefix cache pays off. Runs REAL read-only, path-jailed tools (read_file, list_dir) under workdir. Returns a per-turn report: each round's upstream_ms, prompt_tokens, finish_reason, and any tool calls, plus the final answer and stop reason. Flat upstream_ms while prompt_tokens grows is the prefix-cache-working signal.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"model": map[string]any{
					"type":        "string",
					"description": "Served model name to target (e.g. gemma4-26b-a4b-gptq-apc-canary).",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "The user task for the one-shot session.",
				},
				"workdir": map[string]any{
					"type":        "string",
					"description": "Root directory the read-only tools are jailed to. Defaults to the server's CWD. Absolute and '..' escapes are rejected.",
				},
				"endpoint": map[string]any{
					"type":        "string",
					"description": "Proxy base URL. Defaults to $FLEXINFER_PROXY_URL, else http://localhost:18080.",
				},
				"system": map[string]any{
					"type":        "string",
					"description": "System prompt (the immutable prefix). Has a sensible default.",
				},
				"session": map[string]any{
					"type":        "string",
					"description": "Session id; pins X-Flexinfer-Cache-Key. Generated if omitted.",
				},
				"max_model_len": map[string]any{
					"type":        "integer",
					"description": "Engine maxModelLen (the usable-context bound). Default 20480.",
				},
				"max_tokens": map[string]any{
					"type":        "integer",
					"description": "Max output tokens per turn (the budget's output reserve). Default 512.",
				},
				"max_rounds": map[string]any{
					"type":        "integer",
					"description": "Max ReAct rounds before stopping. Default 20.",
				},
				"system_tokens": map[string]any{
					"type":        "integer",
					"description": "Measured immutable-prefix token count. Default: estimated from system length.",
				},
				"temperature": map[string]any{
					"type":        "number",
					"description": "Sampling temperature. Default 0.",
				},
				"want_prefix_hit": map[string]any{
					"type":        "boolean",
					"description": "Ask the proxy for the engine prefix-cache hit rate (X-Flexinfer-Prefix-Cache-Hit-Rate) — the direct signal when the engine omits cached_tokens. Default true.",
				},
			},
			Required: []string{"model", "prompt"},
		},
	}, handleAgentLoopRun)

	srv.AddTracedTool(mcp.Tool{
		Name:        "agent_loop_self_check",
		Description: "Run the offline self-check harness: a canned chat server (with flexinfer instrumentation headers + a scripted tool-call→final dialogue), a real temp-file the read_file tool actually reads, and assertions on the append-only prefix invariant, header/metric parsing, real tool execution, the path-jail, and budget arithmetic. No cluster or model required. Returns a structured pass/fail report — the dev/ops wiring gate.",
		InputSchema: mcp.InputSchema{
			Type:       "object",
			Properties: map[string]any{},
		},
	}, handleAgentLoopSelfCheck)

	return srv.Run(ctx)
}
