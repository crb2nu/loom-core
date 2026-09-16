package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/mcptest"
	"github.com/crb2nu/loom/pkg/lifecycle"
)

func TestUnavailableDockerBackendReturnsNotConfigured(t *testing.T) {
	b := unavailableDockerBackend{}
	_, err := b.Build(context.Background(), backend.BuildOpts{})
	if err == nil || !strings.Contains(err.Error(), "docker CLI") {
		t.Fatalf("expected docker CLI configuration error, got %v", err)
	}
	_, err = b.Exec(context.Background(), backend.ExecOpts{})
	if err == nil || !strings.Contains(err.Error(), "docker CLI") {
		t.Fatalf("expected docker CLI configuration error, got %v", err)
	}
}

// TestDevboxServesHandshakeWithoutDockerCLI pins the degraded-start contract
// at the server level: with no docker executable on PATH the server still
// answers initialize and tools/list, and a sandbox call returns an MCP error
// naming the docker CLI instead of the process dying with
// "init manager: docker CLI not found in PATH".
func TestDevboxServesHandshakeWithoutDockerCLI(t *testing.T) {
	workspace := t.TempDir()
	// A detectable project: the exec path runs language detection before it
	// touches the backend, so an empty directory would fail earlier for an
	// unrelated reason.
	if err := os.MkdirAll(filepath.Join(workspace, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "proj", "go.mod"), []byte("module example.com/proj\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // nothing resolvable, docker included
	t.Setenv("DEVBOX_BACKEND", "docker")
	t.Setenv("DEVBOX_WORKSPACE_ROOT", workspace)
	t.Setenv("DEVBOX_CACHE_DIR", t.TempDir())
	t.Setenv("DEVBOX_SYNC_MODE", "tar-pipe")
	t.Setenv("DEVBOX_BASE_REGISTRY_URL", "http://127.0.0.1:1") // refused at once, no DNS
	t.Setenv("DEVBOX_HUD_ADDR", "")
	t.Setenv("DEVBOX_WARM_PROJECTS", "")
	t.Setenv("DEVBOX_KUBECONFIG", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, cleanup, err := newDevboxServer(ctx)
	if err != nil {
		t.Fatalf("server must start without the docker CLI: %v", err)
	}
	t.Cleanup(func() { _ = cleanup(context.Background()) })

	res := mcptest.Drive(t, srv.Server, "devbox_exec", map[string]any{"project": "proj", "command": "true"})
	if names := mcptest.ToolNames(t, res.ToolsList); len(names) == 0 {
		t.Fatal("tools/list must not be empty while degraded")
	}
	result := mcptest.ToolResult(t, res.ToolCall)
	text := mcptest.Text(result)
	if !result.IsError || !strings.Contains(text, "docker CLI") {
		t.Fatalf("expected NotConfigured naming the docker CLI, got isError=%v text=%q", result.IsError, text)
	}
}

// Exercise the real SDK receive/dispatch/send boundary with a signal while the
// handler is blocked. A handler-return-only counter would release too early.
func TestSIGTERMDrainsThroughResponseDelivery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	signalCtx, stop, cleanupSignals := lifecycle.SetupSignalHandler(ctx)
	defer stop()
	defer cleanupSignals()
	client, raw := mcp.NewPipeTransport()
	defer client.Close()
	defer raw.Close()
	d := &lifecycle.Drain{}
	transport := &drainTransport{Transport: raw, drain: d, pending: make(map[string]int)}
	srv := mcp.NewServer("test", "1")
	srv.SetConcurrencyLimit(0)
	entered, release := make(chan struct{}), make(chan struct{})
	srv.AddTool(mcp.Tool{Name: "gate"}, func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
		close(entered)
		<-release
		return mcp.TextResult("gate completed"), nil
	})
	go func() { _ = srv.RunWithTransport(ctx, transport) }()
	if err := client.Send(ctx, &mcp.Message{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: json.RawMessage(`{"name":"gate"}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("handler never started")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-signalCtx.Done():
	case <-ctx.Done():
		t.Fatal("signal not received")
	}
	_, drained := d.Begin()
	if err := client.Send(ctx, &mcp.Message{JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: json.RawMessage(`{"name":"gate"}`)}); err != nil {
		t.Fatal(err)
	}
	rejected, err := client.Recv(ctx)
	if err != nil || rejected.Error == nil || !strings.Contains(rejected.Error.Message, "retry") {
		t.Fatalf("missing retryable rejection: %+v %v", rejected, err)
	}
	select {
	case <-drained:
		t.Fatal("drain completed before result")
	default:
	}
	close(release)
	resp, err := client.Recv(ctx)
	if err != nil || !strings.Contains(string(resp.Result), "gate completed") {
		t.Fatalf("result lost after SIGTERM: %+v %v", resp, err)
	}
	select {
	case <-drained:
	case <-ctx.Done():
		t.Fatal("drain never completed")
	}
}

func TestDevboxDrainTimeout(t *testing.T) {
	d := &lifecycle.Drain{}
	d.Admit()
	start := time.Now()
	waitDevboxDrain(d, 20*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond || elapsed > time.Second {
		t.Fatalf("unbounded wait: %v", elapsed)
	}
	d.Release()
}

func TestBuildTimeoutEnv(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"", 30 * time.Minute}, {"2h", 2 * time.Hour}, {"90m", 90 * time.Minute},
		{"invalid", 30 * time.Minute}, {"0", 30 * time.Minute}, {"-1m", 30 * time.Minute},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("DEVBOX_K8S_BUILD_TIMEOUT", tc.value)
			if got := buildTimeout(); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}
