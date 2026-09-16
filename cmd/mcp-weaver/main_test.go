package main

import (
	"context"
	"strings"
	"testing"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/mcptest"
	"github.com/crb2nu/loom/pkg/mcperror"
	"github.com/crb2nu/loom/pkg/mcplog"
	"github.com/crb2nu/loom/pkg/mcpotel"
	"github.com/crb2nu/loom/pkg/weaver"
)

func TestRequireConfiguredEvaluatesPerCall(t *testing.T) {
	var current error = mcperror.NotConfigured("FLEXINFER_URL", "set it")
	calls := 0
	h := requireConfigured(func() error { return current }, func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
		calls++
		return mcp.TextResult("ok"), nil
	})
	result, err := h(context.Background(), map[string]any{"query": "status"})
	if err != nil || result == nil || !result.IsError || !strings.Contains(result.Content[0].Text, "FLEXINFER_URL") {
		t.Fatalf("expected missing-config MCP error, result=%+v err=%v", result, err)
	}
	if calls != 0 {
		t.Fatal("backend handler must not run while unconfigured")
	}
	// The check is lazy: once the environment is fixed the same handler
	// reaches the backend without a restart.
	current = nil
	if result, err := h(context.Background(), nil); err != nil || result.IsError || calls != 1 {
		t.Fatalf("configured call should reach the backend, result=%+v err=%v calls=%d", result, err, calls)
	}
}

func unconfiguredWeaverEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"FLEXINFER_URL", "MCP_HUB_URL", "FLEXINFER_PROXY_URL", "FLEXINFER_API_KEY", "OTEL_EXPORTER_OTLP_ENDPOINT"} {
		t.Setenv(name, "")
	}
}

func newTestWeaverServer(t *testing.T) *mcp.Server {
	t.Helper()
	ctx := context.Background()
	logger := mcplog.NewDefault()
	tp, shutdown, err := mcpotel.InitTracer(ctx, "mcp-weaver-test", logger)
	if err != nil {
		t.Fatalf("tracer: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(ctx) })
	server, err := newWeaverServer(ctx, logger, mcpotel.Tracer(tp, "mcp-weaver-test"))
	if err != nil {
		t.Fatalf("server must start without backend configuration: %v", err)
	}
	return server
}

// TestWeaverServesHandshakeWithoutBackends pins the degraded-start contract
// at the server level: with neither backend variable set the server answers
// initialize and tools/list, and a backend-bound tool call returns an MCP
// error naming BOTH missing variables, without exiting the process.
func TestWeaverServesHandshakeWithoutBackends(t *testing.T) {
	unconfiguredWeaverEnv(t)
	server := newTestWeaverServer(t)

	res := mcptest.Drive(t, server, "weaver__query", map[string]any{"query": "cluster status"})
	names := mcptest.ToolNames(t, res.ToolsList)
	if len(names) == 0 {
		t.Fatal("tools/list must not be empty while unconfigured")
	}
	result := mcptest.ToolResult(t, res.ToolCall)
	text := mcptest.Text(result)
	if !result.IsError || !strings.Contains(text, "FLEXINFER_URL") || !strings.Contains(text, "MCP_HUB_URL") {
		t.Fatalf("expected NotConfigured naming both variables, got isError=%v text=%q", result.IsError, text)
	}
}

// TestWeaverNotConfiguredNamesOnlyTheMissingVariable pins the per-variable
// verdict: with FLEXINFER_URL set and MCP_HUB_URL missing, the error names
// MCP_HUB_URL and not FLEXINFER_URL, and loom/weaver/metrics is gated too.
func TestWeaverNotConfiguredNamesOnlyTheMissingVariable(t *testing.T) {
	unconfiguredWeaverEnv(t)
	t.Setenv("FLEXINFER_URL", "http://127.0.0.1:1")
	server := newTestWeaverServer(t)

	res := mcptest.Drive(t, server, "loom/weaver/metrics", map[string]any{})
	result := mcptest.ToolResult(t, res.ToolCall)
	text := mcptest.Text(result)
	if !result.IsError || !strings.Contains(text, "MCP_HUB_URL") || strings.Contains(text, "FLEXINFER_URL") {
		t.Fatalf("expected NotConfigured naming only MCP_HUB_URL, got isError=%v text=%q", result.IsError, text)
	}
}

// TestWeaverStatusAnswersWhileUnconfigured pins that the status tool stays
// reachable: it is how an operator sees the degraded state.
func TestWeaverStatusAnswersWhileUnconfigured(t *testing.T) {
	unconfiguredWeaverEnv(t)
	server := newTestWeaverServer(t)

	res := mcptest.Drive(t, server, "loom/weaver/status", map[string]any{})
	if result := mcptest.ToolResult(t, res.ToolCall); result.IsError {
		t.Fatalf("status must answer while unconfigured, got %q", mcptest.Text(result))
	}
}

// TestWeaverRejectsInvalidConfigEvenWithoutBackends pins that cfg.Validate
// is not swallowed: an unrelated bad value refuses to start regardless of
// whether the backends are configured.
func TestWeaverRejectsInvalidConfigEvenWithoutBackends(t *testing.T) {
	unconfiguredWeaverEnv(t)
	// LoadConfigFromEnv clamps out-of-range env values to defaults, so the
	// invalid case Validate guards can only be reached programmatically.
	cfg := weaver.LoadConfigFromEnv()
	cfg.Enabled = true
	cfg.MaxIterations = 0
	ctx := context.Background()
	logger := mcplog.NewDefault()
	tp, shutdown, _ := mcpotel.InitTracer(ctx, "mcp-weaver-test", logger)
	t.Cleanup(func() { _ = shutdown(ctx) })
	if _, err := newWeaverServerWithConfig(ctx, cfg, logger, mcpotel.Tracer(tp, "mcp-weaver-test")); err == nil {
		t.Fatal("invalid weaver config must refuse to start even when the backends are unconfigured")
	}
}
