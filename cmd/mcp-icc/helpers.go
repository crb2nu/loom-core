package main

import (
	"context"
	"encoding/json"
	"errors"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/iccclient"
)

// iccToolHandler is an alias for mcp.ToolHandler that makes it
// obvious which handlers close over the shared ICC client.
type iccToolHandler = mcp.ToolHandler

// jsonResult marshals v as JSON and returns it as the text content of
// an MCP call result. Kept terse so handlers stay readable.
func jsonResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return mcp.TextResult(string(data)), nil
}

// errWritesDisabled is returned by every write tool when
// ICC_MCP_WRITE_ENABLED is not exactly "1". Surface as a tool-level
// error rather than panicking so callers (and tests) can probe.
var errWritesDisabled = errors.New("writes_disabled: set ICC_MCP_WRITE_ENABLED=1 to enable mcp-icc write tools")

// withWriteGate wraps an inner handler with the writes-enabled check.
// The check happens at CALL time (not registration time) so toggling
// the env var doesn't require a server restart for tests that drive
// the handler factories directly. In production the gate is closed
// over the boolean from main.go's env read at startup.
func withWriteGate(enabled bool, inner iccToolHandler) iccToolHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		if !enabled {
			return mcp.ErrorResult(errWritesDisabled), nil
		}
		return inner(ctx, args)
	}
}

// postJSON re-exports iccclient.PostJSON as a package-local generic
// so handler files don't need to import iccclient directly.
func postJSON[T any](ctx context.Context, c *iccclient.Client, path string, body any) (int, T, error) {
	return iccclient.PostJSON[T](ctx, c, path, body)
}

// getRaw re-exports iccclient.GetRaw for handlers that hit bare-payload
// endpoints (no {"ok":..., "result":...} wrapper) — /api/state,
// /api/projects/overview, /api/needs-attention, etc.
func getRaw[T any](ctx context.Context, c *iccclient.Client, path string, query map[string]string) (int, T, error) {
	return iccclient.GetRaw[T](ctx, c, path, query)
}
