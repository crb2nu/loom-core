package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	mcp "gitlab.flexinfer.ai/libs/mcp-go"
)

const (
	generationBarrierSize  = 25
	generationSteadyCalls  = 1_000
	generationFaultTokens  = 100
	generationServerName   = "fleet_generation"
	generationToolName     = "echo"
	generationSingleReset  = "generation-reset-once"
	generationBarrierLimit = 15 * time.Second
)

// generationFakeHub is deliberately token-aware: every successful response
// echoes the exact request token, while selected tokens close their owning
// physical WebSocket with 1012 on the first attempt. This lets the black-box
// test distinguish a real retry on a fresh transport from a duplicated or
// misrouted response.
type generationFakeHub struct {
	server *httptest.Server

	opened    atomic.Int64
	closed    atomic.Int64
	active    atomic.Int64
	maxActive atomic.Int64
	resets    atomic.Int64

	mu              sync.Mutex
	attempts        map[string]int
	resetsByToken   map[string]int
	barrierTokens   map[string]struct{}
	barrierReleased chan struct{}
	barrierOnce     sync.Once
	handlerErrs     chan error
}

func newGenerationFakeHub(t *testing.T) *generationFakeHub {
	t.Helper()

	hub := &generationFakeHub{
		attempts:        make(map[string]int),
		resetsByToken:   make(map[string]int),
		barrierTokens:   make(map[string]struct{}),
		barrierReleased: make(chan struct{}),
		handlerErrs:     make(chan error, 256),
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("server"); got != generationServerName {
			hub.reportHandlerError(fmt.Errorf("websocket server query = %q, want %q", got, generationServerName))
			http.Error(w, "unexpected server", http.StatusBadRequest)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			hub.reportHandlerError(fmt.Errorf("upgrade: %w", err))
			return
		}
		hub.connectionOpened()
		defer func() {
			_ = conn.Close()
			hub.active.Add(-1)
			hub.closed.Add(1)
		}()

		for {
			var message mcp.Message
			if err := conn.ReadJSON(&message); err != nil {
				// Every pool eviction, injected reset, and daemon shutdown closes
				// a socket. A read-side close is therefore lifecycle, not a fake
				// handler failure; malformed messages still surface as errors.
				if !isGenerationDisconnect(err) {
					hub.reportHandlerError(fmt.Errorf("read websocket message: %w", err))
				}
				return
			}

			switch message.Method {
			case "initialize":
				response, err := mcp.NewResponse(message.ID, map[string]any{
					"protocolVersion": mcp.ProtocolVersion20250618,
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "fleet-generation-hub", "version": "1"},
				})
				if err != nil {
					hub.reportHandlerError(fmt.Errorf("initialize response: %w", err))
					return
				}
				if err := conn.WriteJSON(response); err != nil {
					hub.reportHandlerError(fmt.Errorf("write initialize response: %w", err))
					return
				}
			case "notifications/initialized":
				continue
			case "tools/call":
				if !hub.handleToolCall(conn, &message) {
					return
				}
			default:
				hub.reportHandlerError(fmt.Errorf("unexpected hub method %q", message.Method))
				if err := conn.WriteJSON(mcp.NewErrorResponse(message.ID, mcp.MethodNotFound, "method not found")); err != nil {
					hub.reportHandlerError(fmt.Errorf("write method-not-found response: %w", err))
				}
				return
			}
		}
	})

	hub.server = httptest.NewServer(mux)
	t.Cleanup(hub.server.Close)
	return hub
}

func (h *generationFakeHub) websocketURL() string {
	return "ws" + strings.TrimPrefix(h.server.URL, "http") + "/ws"
}

func isGenerationDisconnect(err error) bool {
	if err == nil {
		return false
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) || errors.Is(err, net.ErrClosed) {
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "use of closed network connection") ||
		strings.Contains(lower, "connection reset by peer") ||
		strings.Contains(lower, "broken pipe")
}

func (h *generationFakeHub) connectionOpened() {
	h.opened.Add(1)
	active := h.active.Add(1)
	for {
		observed := h.maxActive.Load()
		if active <= observed || h.maxActive.CompareAndSwap(observed, active) {
			return
		}
	}
}

func (h *generationFakeHub) handleToolCall(conn *websocket.Conn, message *mcp.Message) bool {
	var params struct {
		Name      string `json:"name"`
		Arguments struct {
			Token string `json:"token"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(message.Params, &params); err != nil {
		h.reportHandlerError(fmt.Errorf("decode tools/call params: %w", err))
		_ = conn.WriteJSON(mcp.NewErrorResponse(message.ID, mcp.InvalidParams, "invalid params"))
		return false
	}
	if params.Name != generationToolName || params.Arguments.Token == "" {
		h.reportHandlerError(fmt.Errorf("tools/call name=%q token=%q", params.Name, params.Arguments.Token))
		_ = conn.WriteJSON(mcp.NewErrorResponse(message.ID, mcp.InvalidParams, "missing generation token"))
		return false
	}

	token := params.Arguments.Token
	attempt := h.recordAttempt(token)
	if strings.HasPrefix(token, "generation-barrier-") {
		if attempt != 1 {
			h.reportHandlerError(fmt.Errorf("barrier token %q attempt = %d, want 1", token, attempt))
			_ = conn.WriteJSON(mcp.NewErrorResponse(message.ID, mcp.InternalError, "duplicate barrier token"))
			return false
		}
		if err := h.awaitBarrier(token); err != nil {
			h.reportHandlerError(err)
			_ = conn.WriteJSON(mcp.NewErrorResponse(message.ID, mcp.InternalError, err.Error()))
			return false
		}
	}

	if shouldResetGenerationToken(token) && attempt == 1 {
		h.recordReset(token)
		deadline := time.Now().Add(time.Second)
		if err := conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseServiceRestart, "injected generation reset"),
			deadline,
		); err != nil {
			h.reportHandlerError(fmt.Errorf("write reset for %q: %w", token, err))
		}
		return false
	}

	response, err := mcp.NewResponse(message.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": token}},
		"token":   token,
	})
	if err != nil {
		h.reportHandlerError(fmt.Errorf("build token response for %q: %w", token, err))
		return false
	}
	if err := conn.WriteJSON(response); err != nil {
		h.reportHandlerError(fmt.Errorf("write token response for %q: %w", token, err))
		return false
	}
	return true
}

func shouldResetGenerationToken(token string) bool {
	return token == generationSingleReset || strings.HasPrefix(token, "generation-fault-")
}

func (h *generationFakeHub) recordAttempt(token string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.attempts[token]++
	return h.attempts[token]
}

func (h *generationFakeHub) recordReset(token string) {
	h.mu.Lock()
	h.resetsByToken[token]++
	h.mu.Unlock()
	h.resets.Add(1)
}

func (h *generationFakeHub) awaitBarrier(token string) error {
	h.mu.Lock()
	if _, exists := h.barrierTokens[token]; exists {
		h.mu.Unlock()
		return fmt.Errorf("duplicate barrier token %q", token)
	}
	h.barrierTokens[token] = struct{}{}
	arrivals := len(h.barrierTokens)
	released := h.barrierReleased
	if arrivals == generationBarrierSize {
		h.barrierOnce.Do(func() { close(h.barrierReleased) })
	}
	h.mu.Unlock()

	select {
	case <-released:
		return nil
	case <-time.After(generationBarrierLimit):
		return fmt.Errorf("barrier token %q timed out at %d/%d arrivals", token, arrivals, generationBarrierSize)
	}
}

func (h *generationFakeHub) tokenCounts(token string) (attempts, resets int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.attempts[token], h.resetsByToken[token]
}

func (h *generationFakeHub) reportHandlerError(err error) {
	select {
	case h.handlerErrs <- err:
	default:
	}
}

func (h *generationFakeHub) collectedHandlerErrors() []error {
	var errs []error
	for {
		select {
		case err := <-h.handlerErrs:
			errs = append(errs, err)
		default:
			return errs
		}
	}
}

// generationUnixClient is one independent raw Unix MCP transport. It talks
// directly to loomd, avoiding MCPClient.Send's process-level serialization and
// proving that the daemon can fan 25 callers out to 25 physical hub sockets.
type generationUnixClient struct {
	conn      net.Conn
	transport mcp.Transport
	nextID    int64
	mu        sync.Mutex
}

func newGenerationUnixClient(ctx context.Context, socketPath string, ordinal int) (*generationUnixClient, error) {
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("dial daemon socket: %w", err)
	}
	client := &generationUnixClient{
		conn:      conn,
		transport: mcp.NewStdioTransport(conn, conn),
		nextID:    1,
	}

	response, err := client.roundTrip(ctx, "initialize", map[string]any{
		"protocolVersion": mcp.ProtocolVersion20250618,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    fmt.Sprintf("fleet-generation-%02d", ordinal),
			"version": "1",
		},
	})
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if response.Error != nil {
		_ = client.Close()
		return nil, fmt.Errorf("initialize error: %s", response.Error.Message)
	}
	if err := client.transport.Send(ctx, &mcp.Message{
		JSONRPC: mcp.JSONRPCVersion,
		Method:  "notifications/initialized",
	}); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("send initialized notification: %w", err)
	}
	return client, nil
}

func (c *generationUnixClient) callToken(ctx context.Context, token string) error {
	response, err := c.roundTrip(ctx, "tools/call", map[string]any{
		"name": generationServerName + "__" + generationToolName,
		"arguments": map[string]any{
			"token": token,
		},
	})
	if err != nil {
		return err
	}
	if response.Error != nil {
		return fmt.Errorf("daemon tools/call error for %q: %s", token, response.Error.Message)
	}
	var result struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return fmt.Errorf("decode result for %q: %w (result=%s)", token, err, response.Result)
	}
	if result.Token != token {
		return fmt.Errorf("result token = %q, want %q", result.Token, token)
	}
	return nil
}

func (c *generationUnixClient) roundTrip(ctx context.Context, method string, params any) (*mcp.Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID
	c.nextID++
	request, err := mcp.NewRequest(id, method, params)
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", method, err)
	}
	if err := c.transport.Send(ctx, request); err != nil {
		return nil, fmt.Errorf("send %s id=%d: %w", method, id, err)
	}

	for {
		response, err := c.transport.Recv(ctx)
		if err != nil {
			return nil, fmt.Errorf("recv %s id=%d: %w", method, id, err)
		}
		if response == nil || response.ID == nil {
			continue
		}
		if fmt.Sprint(response.ID) != fmt.Sprint(id) {
			return nil, fmt.Errorf("response id = %v, want %d", response.ID, id)
		}
		return response, nil
	}
}

func (c *generationUnixClient) Close() error {
	if c == nil {
		return nil
	}
	if c.transport != nil {
		return c.transport.Close()
	}
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

func waitForGenerationCondition(ctx context.Context, description string, condition func() bool) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for %s: %w", description, ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestIntegration_ProxyDaemon_FakeHubTransportGenerationSoak(t *testing.T) {
	if os.Getenv("LOOM_RUN_INTEGRATION") != "1" {
		t.Skip("set LOOM_RUN_INTEGRATION=1 to run the transport-generation soak")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 85*time.Second)
	defer cancel()
	startedAt := time.Now()
	hub := newGenerationFakeHub(t)
	loomdBin := requireReliabilityBinary(t, "loomd")

	tempDir := t.TempDir()
	configDir := filepath.Join(tempDir, ".config", "loom")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("create HOME config directory: %v", err)
	}
	config := []byte(`resources:
  hub_pool_max_open: 25
  hub_pool_max_idle: 1
routing:
  preferences:
    fleet_generation: hub-only
`)
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), config, 0o600); err != nil {
		t.Fatalf("write HOME config: %v", err)
	}

	registryPath := filepath.Join(tempDir, "registry.yaml")
	if err := os.WriteFile(registryPath, []byte("version: 1\nservers: []\n"), 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("loom-generation-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() {
		_ = os.Remove(socketPath)
		_ = os.Remove(socketPath + ".lock")
	})

	daemonLogs := &lockedBuffer{}
	daemonCmd := exec.CommandContext(ctx, loomdBin,
		"--socket", socketPath,
		"--registry", registryPath,
		"--hub-url", hub.websocketURL(),
		"--hub-fallback=true",
		"--hub-prefer=true",
		"--metrics-addr=",
	)
	daemonCmd.Stdout = daemonLogs
	daemonCmd.Stderr = daemonLogs
	daemonCmd.Env = append(os.Environ(),
		"HOME="+tempDir,
		"LOOM_MASTER_KEY=fleet-reliability-test-master-key",
		"LOOM_METRICS_ADDR=",
		"OTEL_EXPORTER_OTLP_ENDPOINT=",
	)
	if err := daemonCmd.Start(); err != nil {
		t.Fatalf("start loomd: %v", err)
	}
	var stopOnce sync.Once
	stopDaemon := func() {
		stopOnce.Do(func() { stopReliabilityProcess(daemonCmd) })
	}
	t.Cleanup(stopDaemon)
	if err := waitForReliabilitySocket(ctx, socketPath); err != nil {
		t.Fatalf("loomd socket did not become ready: %v\nlogs:\n%s", err, daemonLogs.String())
	}

	clients := make([]*generationUnixClient, 0, generationBarrierSize)
	t.Cleanup(func() {
		for _, client := range clients {
			_ = client.Close()
		}
	})
	for i := 0; i < generationBarrierSize; i++ {
		client, err := newGenerationUnixClient(ctx, socketPath, i)
		if err != nil {
			t.Fatalf("create raw Unix MCP transport %d: %v\nlogs:\n%s", i, err, daemonLogs.String())
		}
		clients = append(clients, client)
	}

	// All callers enter the fake-hub barrier before any response is released.
	// Since each caller owns an independent Unix transport and the hub pool has
	// max_open=25, this proves the daemon opened 25 independent WebSockets.
	barrierErrs := make(chan error, generationBarrierSize)
	var barrierWG sync.WaitGroup
	for i, client := range clients {
		barrierWG.Add(1)
		go func(i int, client *generationUnixClient) {
			defer barrierWG.Done()
			token := fmt.Sprintf("generation-barrier-%02d", i)
			barrierErrs <- client.callToken(ctx, token)
		}(i, client)
	}
	barrierWG.Wait()
	close(barrierErrs)
	for err := range barrierErrs {
		if err != nil {
			t.Fatalf("25-way barrier call: %v\nlogs:\n%s", err, daemonLogs.String())
		}
	}
	if got := hub.opened.Load(); got != generationBarrierSize {
		t.Fatalf("physical connections after barrier = %d, want %d", got, generationBarrierSize)
	}
	if got := hub.maxActive.Load(); got != generationBarrierSize {
		t.Fatalf("max active physical connections = %d, want %d", got, generationBarrierSize)
	}

	idleCtx, idleCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := waitForGenerationCondition(idleCtx, "hub pool to retain exactly one idle connection", func() bool {
		return hub.active.Load() == 1 && hub.closed.Load() == generationBarrierSize-1
	}); err != nil {
		idleCancel()
		t.Fatalf("%v (opened=%d active=%d closed=%d)\nlogs:\n%s",
			err, hub.opened.Load(), hub.active.Load(), hub.closed.Load(), daemonLogs.String())
	}
	idleCancel()

	openedBeforeReset := hub.opened.Load()
	if err := clients[0].callToken(ctx, generationSingleReset); err != nil {
		t.Fatalf("single reset recovery: %v\nlogs:\n%s", err, daemonLogs.String())
	}
	if attempts, resets := hub.tokenCounts(generationSingleReset); attempts != 2 || resets != 1 {
		t.Fatalf("single reset counts = attempts:%d resets:%d, want 2/1", attempts, resets)
	}
	if got := hub.opened.Load(); got != openedBeforeReset+1 {
		t.Fatalf("connections after one reset = %d, want exactly one replacement from %d", got, openedBeforeReset)
	}

	openedBeforeSteady := hub.opened.Load()
	for i := 0; i < generationSteadyCalls; i++ {
		token := fmt.Sprintf("generation-steady-%04d", i)
		if err := clients[0].callToken(ctx, token); err != nil {
			t.Fatalf("steady call %d: %v\nlogs:\n%s", i, err, daemonLogs.String())
		}
		if attempts, resets := hub.tokenCounts(token); attempts != 1 || resets != 0 {
			t.Fatalf("steady token %q counts = attempts:%d resets:%d, want 1/0", token, attempts, resets)
		}
	}
	if got := hub.opened.Load(); got != openedBeforeSteady {
		t.Fatalf("steady calls opened %d extra connections (before=%d after=%d)", got-openedBeforeSteady, openedBeforeSteady, got)
	}

	openedBeforeFaults := hub.opened.Load()
	for i := 0; i < generationFaultTokens; i++ {
		token := fmt.Sprintf("generation-fault-%03d", i)
		if err := clients[0].callToken(ctx, token); err != nil {
			t.Fatalf("fault token %d: %v\nlogs:\n%s", i, err, daemonLogs.String())
		}
		if attempts, resets := hub.tokenCounts(token); attempts != 2 || resets != 1 {
			t.Fatalf("fault token %q counts = attempts:%d resets:%d, want 2/1", token, attempts, resets)
		}
	}
	if got := hub.opened.Load(); got != openedBeforeFaults+generationFaultTokens {
		t.Fatalf("fault replacements = %d, want %d (before=%d after=%d)",
			got-openedBeforeFaults, generationFaultTokens, openedBeforeFaults, got)
	}
	if got := hub.resets.Load(); got != generationFaultTokens+1 {
		t.Fatalf("total resets = %d, want %d", got, generationFaultTokens+1)
	}

	// Keep the raw clients connected while explicitly stopping loomd. Shutdown
	// must close the final idle hub generation as well as every Unix client.
	stopDaemon()
	if state := daemonCmd.ProcessState; state == nil || !state.Success() {
		t.Fatalf("loomd did not stop cleanly: state=%v\nlogs:\n%s", state, daemonLogs.String())
	}
	closedCtx, closedCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := waitForGenerationCondition(closedCtx, "all physical hub connections to close", func() bool {
		return hub.active.Load() == 0 && hub.opened.Load() == hub.closed.Load()
	}); err != nil {
		closedCancel()
		t.Fatalf("%v (opened=%d active=%d closed=%d)\nlogs:\n%s",
			err, hub.opened.Load(), hub.active.Load(), hub.closed.Load(), daemonLogs.String())
	}
	closedCancel()

	if errs := hub.collectedHandlerErrors(); len(errs) > 0 {
		t.Fatalf("fake hub handler errors: %v", errs)
	}
	if elapsed := time.Since(startedAt); elapsed >= 90*time.Second {
		t.Fatalf("generation soak runtime = %s, must stay below 90s", elapsed)
	}

	snapshot, err := json.Marshal(map[string]any{
		"active":           hub.active.Load(),
		"closed":           hub.closed.Load(),
		"fault_tokens":     generationFaultTokens,
		"max_active":       hub.maxActive.Load(),
		"opened":           hub.opened.Load(),
		"resets":           hub.resets.Load(),
		"runtime_ms":       time.Since(startedAt).Milliseconds(),
		"sequential_calls": generationSteadyCalls,
	})
	if err != nil {
		t.Fatalf("marshal generation snapshot: %v", err)
	}
	t.Logf("RELIABILITY_SCENARIO %s", snapshot)
}
