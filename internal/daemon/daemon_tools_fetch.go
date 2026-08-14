package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/registry"
)

// fetchServerToolsTimeout is the default budget for a single-process
// tools/list probe. Used by cache warming, which wants to fail fast.
const fetchServerToolsTimeout = 10 * time.Second

// Bound Cmd.Wait when a probe child (or one of its descendants) inherits a
// stdio pipe. Without WaitDelay, refresh cancellation can leave cleanup stuck
// waiting for EOF after the direct child has already been killed.
const fetchServerToolsWaitDelay = 2 * time.Second

// fetchServerToolsWithTimeout gets tools from a single server using its own
// dedicated process, bounding the probe with the given timeout. The health
// monitor passes a longer deep-probe timeout so a slow subprocess start (under
// retry-storm load) is not mistaken for an unhealthy server and restarted.
func (d *Daemon) fetchServerToolsWithTimeout(ctx context.Context, serverName string, timeout time.Duration) ([]mcp.Tool, error) {
	reg := d.currentRegistry()
	if reg == nil {
		return nil, fmt.Errorf("get server spec: registry not loaded")
	}
	return d.fetchServerToolsFromRegistry(ctx, serverName, timeout, reg)
}

// fetchServerToolsFromRegistry uses one captured registry for both the target
// spec and variable expansion. Reload cannot mix a new spec with old aliases
// (or the reverse) inside a probe.
func (d *Daemon) fetchServerToolsFromRegistry(
	ctx context.Context,
	serverName string,
	timeout time.Duration,
	reg *registry.Registry,
) ([]mcp.Tool, error) {
	if reg == nil {
		return nil, fmt.Errorf("get server spec: registry not loaded")
	}
	spec, err := reg.GetServerSpec(serverName, d.cfg.Target)
	if err != nil {
		return nil, fmt.Errorf("get server spec: %w", err)
	}

	if spec.Command == "" {
		return nil, fmt.Errorf("no command defined")
	}

	if timeout <= 0 {
		timeout = fetchServerToolsTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Expand variables in command
	command := expandVarsWithRegistry(spec.Command, d.repoRoot, reg)

	// Build command
	args := make([]string, len(spec.Args))
	for i, arg := range spec.Args {
		args[i] = expandVarsWithRegistry(fmt.Sprint(arg), d.repoRoot, reg)
	}

	cmd := exec.CommandContext(ctx, command, args...)
	cmd.WaitDelay = fetchServerToolsWaitDelay
	cmd.Env = os.Environ()
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, expandVarsWithRegistry(v, d.repoRoot, reg)))
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("start: %w", err)
	}
	defer func() {
		stdin.Close()
		stdout.Close()
		cmd.Process.Kill()
		cmd.Wait()
	}()

	transport := mcp.NewStdioTransport(stdout, stdin)

	if err := initializeMCPTransport(ctx, transport); err != nil {
		return nil, err
	}

	// Get tools
	toolsReq, _ := mcp.NewRequest(2, "tools/list", nil)
	if err := transport.Send(ctx, toolsReq); err != nil {
		return nil, fmt.Errorf("send tools/list: %w", err)
	}
	toolsResp, err := transport.Recv(ctx)
	if err != nil {
		return nil, fmt.Errorf("recv tools/list: %w", err)
	}
	if toolsResp.Error != nil {
		return nil, fmt.Errorf("server error: %s", toolsResp.Error.Message)
	}

	var toolsList struct {
		Tools []mcp.Tool `json:"tools"`
	}
	if err := json.Unmarshal(toolsResp.Result, &toolsList); err != nil {
		return nil, fmt.Errorf("unmarshal tools: %w", err)
	}

	return toolsList.Tools, nil
}

// fetchServerToolsViaPool performs a tools/list health probe using the connection
// pool, reusing an existing idle connection when available. This avoids spawning a
// fresh process for every health check interval.
func (d *Daemon) fetchServerToolsViaPool(ctx context.Context, serverName string) ([]mcp.Tool, error) {
	if d.pool == nil {
		return nil, fmt.Errorf("local pool not configured")
	}

	// Acquire callLock BEFORE pool.Get to match callPipeline.routeAndConnect
	// ordering. Reversed ordering (pool->lock) can deadlock against the
	// callPipeline path (lock->pool) when the pool is at capacity.
	mu, _, err := d.acquireCallLock(ctx, serverName)
	if err != nil {
		return nil, fmt.Errorf("acquire call lock: %w", err)
	}
	defer mu.Unlock()

	checkout, err := d.checkoutLocalConnection(ctx, serverName)
	if err != nil {
		return nil, fmt.Errorf("pool connect: %w", err)
	}
	defer checkout.close()
	conn := checkout.conn

	req, _ := mcp.NewRequest(1, "tools/list", nil)
	if err := conn.Transport.Send(ctx, req); err != nil {
		checkout.failObservedGeneration(err)
		return nil, fmt.Errorf("send tools/list: %w", err)
	}

	resp, err := conn.Transport.Recv(ctx)
	if err != nil {
		checkout.failObservedGeneration(err)
		return nil, fmt.Errorf("recv tools/list: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("server error: %s", resp.Error.Message)
	}

	var toolsList struct {
		Tools []mcp.Tool `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &toolsList); err != nil {
		return nil, fmt.Errorf("unmarshal tools: %w", err)
	}

	return toolsList.Tools, nil
}

// hubInitTimeout bounds the MCP initialize handshake on a fresh hub
// transport. The caller's request context may carry no deadline (a plain
// tools/call forwarded by the proxy), and a wedged tunnel stream can accept
// the WebSocket upgrade yet never deliver the init response — without an
// independent bound the dial, and every tool call queued on it, hangs until
// the client aborts.
const hubInitTimeout = 30 * time.Second

// initializeMCPTransportWithTimeout runs the initialize handshake under its
// own deadline, layered on top of whatever deadline the caller context
// already carries.
func initializeMCPTransportWithTimeout(ctx context.Context, transport mcp.Transport, timeout time.Duration) error {
	initCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return initializeMCPTransport(initCtx, transport)
}

// initializeMCPTransport performs the MCP initialize handshake on a fresh transport.
func initializeMCPTransport(ctx context.Context, transport mcp.Transport) error {
	versions := []string{
		mcp.ProtocolVersion20250618,
		mcp.ProtocolVersion,
	}
	var lastErr error
	for _, protocolVersion := range versions {
		initReq, _ := mcp.NewRequest(1, "initialize", mcp.InitializeParams{
			ProtocolVersion: protocolVersion,
			Capabilities:    mcp.Capabilities{},
			ClientInfo:      mcp.ClientInfo{Name: "loom-daemon", Version: "0.1.0"},
		})
		if err := transport.Send(ctx, initReq); err != nil {
			lastErr = fmt.Errorf("send init (%s): %w", protocolVersion, err)
			continue
		}
		initResp, err := transport.Recv(ctx)
		if err != nil {
			lastErr = fmt.Errorf("recv init (%s): %w", protocolVersion, err)
			continue
		}
		if initResp != nil && initResp.Error != nil {
			lastErr = fmt.Errorf("init error (%s): %s", protocolVersion, initResp.Error.Message)
			continue
		}

		initNotif := &mcp.Message{JSONRPC: "2.0", Method: "notifications/initialized"}
		if err := transport.Send(ctx, initNotif); err != nil {
			return fmt.Errorf("send initialized (%s): %w", protocolVersion, err)
		}
		return nil
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("initialize failed: no protocol versions attempted")
}
