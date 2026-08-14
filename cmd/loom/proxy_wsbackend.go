// proxy_wsbackend.go implements `loom proxy --ws-backend <url>`: a thin,
// single-backend MCP pass-through that bridges the agent CLI's stdio MCP
// transport directly to ONE upstream MCP server spoken over WebSocket.
//
// Unlike the default proxy (runProxy), this mode does NOT talk to a loom
// daemon, does NOT aggregate multiple servers, and does NOT namespace tool
// names (server__tool). It forwards every standard JSON-RPC message verbatim
// in both directions. That is exactly what a Mills spawn pod needs to reach
// the in-cluster agent-context MCP server (ws://mcp-agent-context.loom-hub:8080/ws),
// which speaks plain MCP over WebSocket and exposes un-prefixed tool names
// (e.g. agent_plan_get). The default proxy's handlers speak daemon-only
// methods (loom/tools, loom/call) and would not work against a raw MCP server.
//
// Spawn pods have no local loom daemon and no Unix socket; the only reachable
// store endpoint is the cluster WebSocket service. This mode is the bridge.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"
)

// wsBackendDefaultServerNS is the namespace used when matching the profile
// priority patterns (which are written as "<server>__<tool>") against the
// backend's un-prefixed tool names. The only production ws-backend is
// mcp-agent-context, whose namespace in the daemon-aggregated proxy is
// "agent_context". Override with LOOM_PROXY_WS_SERVER for other backends.
const wsBackendDefaultServerNS = "agent_context"

// wsBackendFilter describes optional tools/list shaping for the ws-backend
// proxy. The zero value means "forward tools/list verbatim", which is the
// default and what Mills spawn pods depend on today.
type wsBackendFilter struct {
	profile  string // e.g. "llm-core"; empty disables filtering entirely
	serverNS string // namespace used only for pattern matching
	maxTools int    // 0 = profile default
}

// wsBackendFilterFromEnv reads the opt-in knobs. Filtering is OFF unless
// LOOM_PROXY_WS_PROFILE is set: the raw pass-through is the contract the
// in-cluster Mills spawn pods rely on, so it must stay the default.
func wsBackendFilterFromEnv() wsBackendFilter {
	f := wsBackendFilter{
		profile:  strings.TrimSpace(os.Getenv("LOOM_PROXY_WS_PROFILE")),
		serverNS: strings.TrimSpace(os.Getenv("LOOM_PROXY_WS_SERVER")),
	}
	if f.serverNS == "" {
		f.serverNS = wsBackendDefaultServerNS
	}
	return f
}

func (f wsBackendFilter) enabled() bool { return f.profile != "" }

// applyToToolsList rewrites a tools/list response in place, applying the
// configured profile. The backend speaks un-namespaced tool names while the
// profile priority lists are namespaced, so names are temporarily prefixed for
// matching and restored before the response goes back to the client. Any
// decode failure leaves the response untouched — shaping must never be able to
// break the bridge.
func (f wsBackendFilter) applyToToolsList(resp *mcp.Message) {
	if !f.enabled() || resp == nil || len(resp.Result) == 0 {
		return
	}

	var payload struct {
		Tools []mcp.Tool `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &payload); err != nil || len(payload.Tools) == 0 {
		return
	}

	prefix := f.serverNS + "__"
	namespaced := make([]mcp.Tool, len(payload.Tools))
	original := make(map[string]string, len(payload.Tools))
	for i, tool := range payload.Tools {
		namespaced[i] = tool
		if !strings.Contains(tool.Name, "__") {
			namespaced[i].Name = prefix + tool.Name
			original[namespaced[i].Name] = tool.Name
		}
	}

	kept := filterProxyTools(namespaced, "", f.profile, f.maxTools)
	for i := range kept {
		if name, ok := original[kept[i].Name]; ok {
			kept[i].Name = name
		}
	}

	// Preserve any sibling fields (nextCursor, _meta, ...) the backend sent.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(resp.Result, &raw); err != nil {
		return
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return
	}
	raw["tools"] = encoded
	result, err := json.Marshal(raw)
	if err != nil {
		return
	}
	resp.Result = result
}

// runWSBackendProxy runs loom as a stdio MCP server that forwards every
// request/notification to a single upstream MCP server over WebSocket and
// streams responses (and server-initiated notifications) back to the client.
//
// The upstream connection is dialed lazily on the first client message and
// re-dialed if the WebSocket drops, so the proxy answers `initialize`
// promptly even if the backend is briefly unreachable at startup.
func runWSBackendProxy(wsURL string) error {
	wsURL = strings.TrimSpace(wsURL)
	if wsURL == "" {
		return fmt.Errorf("--ws-backend requires a non-empty WebSocket URL")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case sig := <-sigCh:
			fmt.Fprintf(os.Stderr, "loom proxy: received %s, shutting down\n", sig)
			cancel()
		case <-ctx.Done():
		}
	}()
	defer signal.Stop(sigCh)

	stdio := mcp.NewStdioTransport(os.Stdin, os.Stdout)

	// Optional Cloudflare Access / bearer credentials for off-LAN backends.
	// Harmless when unset (in-cluster the service is reached directly).
	wsHeaders := map[string]string{}
	if tok := strings.TrimSpace(os.Getenv("LOOM_WS_BEARER_TOKEN")); tok != "" {
		wsHeaders["Authorization"] = "Bearer " + tok
	}
	cfID := strings.TrimSpace(os.Getenv("LOOM_WS_CF_ACCESS_CLIENT_ID"))
	cfSecret := strings.TrimSpace(os.Getenv("LOOM_WS_CF_ACCESS_CLIENT_SECRET"))

	dial := func(ctx context.Context) (mcp.Transport, error) {
		return mcp.NewWebSocketTransport(ctx, mcp.WebSocketConfig{
			URL:                  wsURL,
			Headers:              wsHeaders,
			CFAccessClientID:     cfID,
			CFAccessClientSecret: cfSecret,
			ConnectTimeout:       10 * time.Second,
			ClientInfo:           mcp.ClientInfo{Name: "loom-ws-proxy", Version: version},
		}, "")
	}

	return wsBackendPump(ctx, stdio, dial, wsBackendFilterFromEnv())
}

// wsBackendPump is the transport-agnostic core: it reads client messages,
// forwards each to a lazily-dialed (and reconnect-on-error) backend, and
// relays responses + server-initiated notifications back to the client.
// Split out from runWSBackendProxy so tests can inject in-memory transports.
// filter is the zero value (no shaping) unless LOOM_PROXY_WS_PROFILE is set.
func wsBackendPump(ctx context.Context, client mcp.Transport, dial func(context.Context) (mcp.Transport, error), filter wsBackendFilter) error {
	var (
		backend   mcp.Transport
		backendMu sync.Mutex
	)
	ensureBackend := func() (mcp.Transport, error) {
		backendMu.Lock()
		defer backendMu.Unlock()
		if backend != nil {
			return backend, nil
		}
		t, err := dial(ctx)
		if err != nil {
			return nil, err
		}
		backend = t
		return backend, nil
	}
	resetBackend := func() {
		backendMu.Lock()
		defer backendMu.Unlock()
		if backend != nil {
			backend.Close()
			backend = nil
		}
	}
	defer resetBackend()

	// forward sends the client message upstream and, for requests (those with
	// an ID), reads upstream messages until the matching response arrives —
	// relaying any server-initiated notifications to the client meanwhile.
	// Client notifications (no ID) are fire-and-forget. On a transport error
	// it resets the backend and retries once.
	forward := func(msg *mcp.Message) (*mcp.Message, error) {
		attempt := func() (*mcp.Message, error) {
			be, err := ensureBackend()
			if err != nil {
				return nil, &proxyTransportError{err: err}
			}
			if err := be.Send(ctx, msg); err != nil {
				return nil, &proxyTransportError{err: err}
			}
			if msg.ID == nil { // notification: no response expected
				return nil, nil
			}
			for {
				resp, err := be.Recv(ctx)
				if err != nil {
					return nil, &proxyTransportError{err: err}
				}
				if resp.ID == nil {
					// Server-initiated notification: relay and keep waiting.
					_ = client.Send(ctx, resp)
					continue
				}
				if jsonRPCIDEqual(resp.ID, msg.ID) {
					return resp, nil
				}
				// Out-of-band response id (defensive; single-flighted here).
				_ = client.Send(ctx, resp)
			}
		}

		resp, err := attempt()
		var te *proxyTransportError
		if err != nil && errors.As(err, &te) {
			resetBackend()
			if ctx.Err() != nil {
				return nil, err
			}
			resp, err = attempt()
		}
		return resp, err
	}

	for {
		msg, err := client.Recv(ctx)
		if err != nil {
			return nil // client disconnected or shutdown
		}

		resp, ferr := forward(msg)
		if msg.ID == nil {
			continue // client notification — nothing to send back
		}
		if ferr != nil {
			_ = client.Send(ctx, mcp.NewErrorResponse(msg.ID, mcp.InternalError,
				"ws-backend forward failed: "+ferr.Error()))
			continue
		}
		if resp != nil {
			if msg.Method == "tools/list" {
				filter.applyToToolsList(resp)
			}
			_ = client.Send(ctx, resp)
		}
	}
}

// jsonRPCIDEqual compares two JSON-RPC ids that arrive as decoded JSON values
// (float64 for numbers, string for strings). It tolerates the number/string
// representational mismatch by falling back to a stringified compare.
func jsonRPCIDEqual(a, b any) bool {
	if a == b {
		return true
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}
