package integration

import (
	"bytes"
	"context"
	"encoding/json"
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
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	mcp "gitlab.flexinfer.ai/libs/mcp-go"
)

type reliabilityFakeHub struct {
	server        *httptest.Server
	connections   atomic.Int64
	toolCalls     atomic.Int64
	resets        atomic.Int64
	notifications atomic.Int64
	handlerErrs   chan error
}

func newReliabilityFakeHub(t *testing.T) *reliabilityFakeHub {
	t.Helper()
	hub := &reliabilityFakeHub{handlerErrs: make(chan error, 8)}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			hub.reportHandlerError(fmt.Errorf("upgrade: %w", err))
			return
		}
		hub.connections.Add(1)
		defer conn.Close()

		for {
			var message mcp.Message
			if err := conn.ReadJSON(&message); err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					hub.reportHandlerError(fmt.Errorf("read: %w", err))
				}
				return
			}

			switch message.Method {
			case "initialize":
				hub.notifications.Add(1)
				if err := conn.WriteJSON(&mcp.Message{
					JSONRPC: mcp.JSONRPCVersion,
					Method:  "notifications/tools/list_changed",
				}); err != nil {
					hub.reportHandlerError(fmt.Errorf("pre-initialize notification: %w", err))
					return
				}
				response := &mcp.Message{
					JSONRPC: mcp.JSONRPCVersion,
					ID:      message.ID,
					Result: mustMarshal(map[string]any{
						"protocolVersion": mcp.ProtocolVersion20250618,
						"capabilities":    map[string]any{"tools": map[string]any{}},
						"serverInfo":      map[string]any{"name": "fleet-fake-hub", "version": "1"},
					}),
				}
				if err := conn.WriteJSON(response); err != nil {
					hub.reportHandlerError(fmt.Errorf("initialize response: %w", err))
					return
				}
			case "notifications/initialized":
				continue
			case "tools/call":
				attempt := hub.toolCalls.Add(1)
				if attempt == 1 {
					hub.resets.Add(1)
					deadline := time.Now().Add(time.Second)
					_ = conn.WriteControl(websocket.CloseMessage,
						websocket.FormatCloseMessage(websocket.CloseServiceRestart, "injected reliability reset"), deadline)
					return
				}
				hub.notifications.Add(1)
				if err := conn.WriteJSON(&mcp.Message{
					JSONRPC: mcp.JSONRPCVersion,
					Method:  "notifications/tools/list_changed",
				}); err != nil {
					hub.reportHandlerError(fmt.Errorf("tool notification: %w", err))
					return
				}
				response := &mcp.Message{
					JSONRPC: mcp.JSONRPCVersion,
					ID:      message.ID,
					Result: mustMarshal(map[string]any{
						"content": []map[string]any{{"type": "text", "text": "recovered"}},
					}),
				}
				if err := conn.WriteJSON(response); err != nil {
					hub.reportHandlerError(fmt.Errorf("tool response: %w", err))
					return
				}
			default:
				response := &mcp.Message{
					JSONRPC: mcp.JSONRPCVersion,
					ID:      message.ID,
					Error:   &mcp.Error{Code: mcp.MethodNotFound, Message: "method not found"},
				}
				if err := conn.WriteJSON(response); err != nil {
					hub.reportHandlerError(fmt.Errorf("error response: %w", err))
					return
				}
			}
		}
	})
	hub.server = httptest.NewServer(mux)
	t.Cleanup(hub.server.Close)
	return hub
}

func (h *reliabilityFakeHub) websocketURL() string {
	return "ws" + strings.TrimPrefix(h.server.URL, "http") + "/ws"
}

func (h *reliabilityFakeHub) reportHandlerError(err error) {
	select {
	case h.handlerErrs <- err:
	default:
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func requireReliabilityBinary(t *testing.T, name string) string {
	t.Helper()
	repoRoot := os.Getenv("LOOM_REPO_ROOT")
	if repoRoot == "" {
		var err error
		repoRoot, err = filepath.Abs("../..")
		if err != nil {
			t.Fatalf("resolve repo root: %v", err)
		}
	}
	path := filepath.Join(repoRoot, "bin", name)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		t.Fatalf("required reliability binary missing or not executable: %s", path)
	}
	return path
}

func waitForReliabilitySocket(ctx context.Context, socketPath string) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(ctx, "unix", socketPath)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func stopReliabilityProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

func TestIntegration_ProxyDaemon_FakeHubCloseReset(t *testing.T) {
	if os.Getenv("LOOM_RUN_INTEGRATION") != "1" {
		t.Skip("set LOOM_RUN_INTEGRATION=1 to run the strict fleet reliability integration")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	hub := newReliabilityFakeHub(t)
	loomBin := requireReliabilityBinary(t, "loom")
	loomdBin := requireReliabilityBinary(t, "loomd")

	tempDir := t.TempDir()
	// Darwin caps Unix socket paths at roughly 104 bytes; t.TempDir paths are
	// intentionally descriptive and can exceed that bound.
	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("loom-fleet-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() {
		_ = os.Remove(socketPath)
		_ = os.Remove(socketPath + ".lock")
	})
	registryPath := filepath.Join(tempDir, "registry.yaml")
	if err := os.WriteFile(registryPath, []byte("version: 1\nservers: []\n"), 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}

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
	t.Cleanup(func() { stopReliabilityProcess(daemonCmd) })
	if err := waitForReliabilitySocket(ctx, socketPath); err != nil {
		t.Fatalf("loomd socket did not become ready: %v\nlogs:\n%s", err, daemonLogs.String())
	}

	client, err := NewMCPClientWithEnv(ctx, map[string]string{
		"HOME":                        tempDir,
		"LOOM_SOCKET":                 socketPath,
		"LOOM_PROXY_SESSION_DISABLE":  "1",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "",
	}, loomBin, "proxy")
	if err != nil {
		t.Fatalf("start loom proxy: %v", err)
	}
	defer client.Close()

	initialize, err := client.Send("initialize", map[string]any{
		"protocolVersion": mcp.ProtocolVersion20250618,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "fleet-reliability", "version": "1"},
	})
	if err != nil {
		t.Fatalf("initialize proxy: %v", err)
	}
	if initialize.Error != nil {
		t.Fatalf("initialize proxy error: %s", mcpErrorStr(initialize.Error))
	}

	response, err := client.Send("tools/call", map[string]any{
		"name":      "fleet_fake__echo",
		"arguments": map[string]any{"message": "recover"},
	})
	if err != nil {
		t.Fatalf("tools/call after injected reset: %v\nlogs:\n%s", err, daemonLogs.String())
	}
	if response.Error != nil {
		t.Fatalf("tools/call returned error after injected reset: %s\nlogs:\n%s",
			mcpErrorStr(response.Error), daemonLogs.String())
	}
	if !bytes.Contains(response.Result, []byte(`"recovered"`)) {
		t.Fatalf("tools/call result = %s, want recovered fake-hub response", response.Result)
	}

	if got := hub.resets.Load(); got != 1 {
		t.Fatalf("injected reset count = %d, want 1", got)
	}
	if got := hub.toolCalls.Load(); got != 2 {
		t.Fatalf("hub tool attempts = %d, want 2", got)
	}
	if got := hub.connections.Load(); got != 2 {
		t.Fatalf("hub physical connections = %d, want 2", got)
	}
	if got := hub.notifications.Load(); got != 3 {
		t.Fatalf("hub notifications = %d, want 3", got)
	}
	select {
	case handlerErr := <-hub.handlerErrs:
		t.Fatalf("fake hub handler error: %v", handlerErr)
	default:
	}

	snapshot, err := json.Marshal(map[string]int64{
		"connections":   hub.connections.Load(),
		"notifications": hub.notifications.Load(),
		"tool_calls":    hub.toolCalls.Load(),
		"resets":        hub.resets.Load(),
	})
	if err != nil {
		t.Fatalf("marshal scenario snapshot: %v", err)
	}
	t.Logf("RELIABILITY_SCENARIO %s", snapshot)
}
