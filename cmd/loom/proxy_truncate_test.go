package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/daemon"
)

func TestTruncateCallToolResult_NoChangeWhenUnderLimit(t *testing.T) {
	r := &mcp.CallToolResult{
		Content: []mcp.Content{{Type: "text", Text: "hello"}},
	}
	if truncateCallToolResult(r, 1024, 1024) {
		t.Fatalf("expected no truncation")
	}
	if got := r.Content[0].Text; got != "hello" {
		t.Fatalf("unexpected text: %q", got)
	}
}

func TestTruncateCallToolResult_TruncatesAndAddsSuffix(t *testing.T) {
	maxBytes := 2000
	huge := strings.Repeat("x", maxBytes*2)
	r := &mcp.CallToolResult{
		Content: []mcp.Content{{Type: "text", Text: huge}},
	}

	if !truncateCallToolResult(r, maxBytes, 1024) {
		t.Fatalf("expected truncation")
	}
	if len(r.Content) != 1 {
		t.Fatalf("expected 1 content item, got %d", len(r.Content))
	}
	if len(r.Content[0].Text) > maxBytes {
		t.Fatalf("expected truncated text <= %d bytes, got %d", maxBytes, len(r.Content[0].Text))
	}
	if !strings.Contains(r.Content[0].Text, "[loom] output truncated") {
		t.Fatalf("expected truncation suffix, got: %q", r.Content[0].Text)
	}
}

func TestTruncateCallToolResult_DropsExtraContentAndHonorsBudget(t *testing.T) {
	maxBytes := 1024
	r := &mcp.CallToolResult{
		Content: []mcp.Content{
			{Type: "text", Text: strings.Repeat("a", maxBytes-10)},
			{Type: "text", Text: strings.Repeat("b", maxBytes)},
		},
	}

	if !truncateCallToolResult(r, maxBytes, 1024) {
		t.Fatalf("expected truncation")
	}

	total := 0
	for _, c := range r.Content {
		total += len(c.Text) + len(c.Data)
	}
	if total > maxBytes {
		t.Fatalf("expected total <= %d bytes, got %d", maxBytes, total)
	}
	if len(r.Content) != 2 {
		t.Fatalf("expected second content item to remain (truncated), got %d items", len(r.Content))
	}
}

func TestTruncateCallToolResult_ImageNeverTruncatesBase64(t *testing.T) {
	maxText := 512
	maxImg := 1024
	hugeImg := "data:image/png;base64," + strings.Repeat("A", maxImg*2)

	r := &mcp.CallToolResult{
		Content: []mcp.Content{
			{Type: "image", MimeType: "image/png", Data: hugeImg},
			{Type: "text", Text: "after"},
		},
	}

	if !truncateCallToolResult(r, maxText, maxImg) {
		t.Fatalf("expected truncation due to oversized image")
	}
	// Image should be dropped, not truncated.
	for _, c := range r.Content {
		if c.Type == "image" {
			t.Fatalf("expected image to be omitted, got image content")
		}
		if strings.Contains(c.Data, "data:image") {
			t.Fatalf("expected no image data URL remnants in truncated output")
		}
	}
	// Ensure we still keep the trailing text content.
	foundAfter := false
	for _, c := range r.Content {
		if c.Type == "text" && c.Text == "after" {
			foundAfter = true
		}
	}
	if !foundAfter {
		t.Fatalf("expected to keep trailing text content")
	}
}

func TestResolveProxyLimit_EnvOverridesConfig(t *testing.T) {
	os.Setenv("TEST_PROXY_LIMIT", "99999")
	defer os.Unsetenv("TEST_PROXY_LIMIT")

	got := resolveProxyLimit("TEST_PROXY_LIMIT", 50000, 48000, 1024)
	if got != 99999 {
		t.Errorf("resolveProxyLimit = %d, want 99999 (env)", got)
	}
}

func TestResolveProxyLimit_ConfigFallback(t *testing.T) {
	os.Unsetenv("TEST_PROXY_LIMIT_2")

	got := resolveProxyLimit("TEST_PROXY_LIMIT_2", 60000, 48000, 1024)
	if got != 60000 {
		t.Errorf("resolveProxyLimit = %d, want 60000 (config)", got)
	}
}

func TestResolveProxyLimit_HardcodedDefault(t *testing.T) {
	os.Unsetenv("TEST_PROXY_LIMIT_3")

	got := resolveProxyLimit("TEST_PROXY_LIMIT_3", 0, 48000, 1024)
	if got != 48000 {
		t.Errorf("resolveProxyLimit = %d, want 48000 (default)", got)
	}
}

func TestResolveProxyLimit_MinBound(t *testing.T) {
	os.Setenv("TEST_PROXY_LIMIT_4", "500")
	defer os.Unsetenv("TEST_PROXY_LIMIT_4")

	got := resolveProxyLimit("TEST_PROXY_LIMIT_4", 0, 48000, 1024)
	if got != 1024 {
		t.Errorf("resolveProxyLimit = %d, want 1024 (min bound)", got)
	}
}

func TestProxyMaxToolResultBytes_ConfigFallback(t *testing.T) {
	os.Unsetenv(loomProxyMaxToolResultBytesEnv)
	saved := proxyConfigGlobal
	defer func() { proxyConfigGlobal = saved }()

	proxyConfigGlobal = daemon.ProxyConfig{MaxToolResultBytes: 70000}
	got := proxyMaxToolResultBytes()
	if got != 70000 {
		t.Errorf("proxyMaxToolResultBytes() = %d, want 70000", got)
	}
}

func TestProxyMaxToolResultBytes_EnvOverridesConfig(t *testing.T) {
	os.Setenv(loomProxyMaxToolResultBytesEnv, "80000")
	defer os.Unsetenv(loomProxyMaxToolResultBytesEnv)
	saved := proxyConfigGlobal
	defer func() { proxyConfigGlobal = saved }()

	proxyConfigGlobal = daemon.ProxyConfig{MaxToolResultBytes: 70000}
	got := proxyMaxToolResultBytes()
	if got != 80000 {
		t.Errorf("proxyMaxToolResultBytes() = %d, want 80000 (env)", got)
	}
}

func TestLookupToolCap_Precedence(t *testing.T) {
	caps := []daemon.ToolCap{
		{Server: "gitlab", MaxBytes: 16000},                            // server-wide
		{Server: "gitlab", Tool: "list_pipeline_jobs", MaxBytes: 8000}, // exact
		{Server: "k8s_apps_k3s", Tool: "*", MaxBytes: 12000},           // server-wide via "*"
		{Server: "flux", Tool: "flux_get_kustomizations", MaxBytes: 0}, // ignored (<=0)
	}

	tests := []struct {
		name      string
		server    string
		tool      string
		wantBytes int
		wantOK    bool
	}{
		{"exact wins over server-wide", "gitlab", "list_pipeline_jobs", 8000, true},
		{"server-wide applies when no exact", "gitlab", "other_tool", 16000, true},
		{"server-wide via star", "k8s_apps_k3s", "k8s_get", 12000, true},
		{"zero-byte entry ignored", "flux", "flux_get_kustomizations", 0, false},
		{"unknown server", "prometheus", "query", 0, false},
		{"empty server", "", "x", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := lookupToolCap(caps, tc.server, tc.tool)
			if ok != tc.wantOK || got != tc.wantBytes {
				t.Errorf("lookupToolCap(%q,%q) = (%d,%v), want (%d,%v)",
					tc.server, tc.tool, got, ok, tc.wantBytes, tc.wantOK)
			}
		})
	}
}

func TestProxyMaxToolResultBytesFor(t *testing.T) {
	os.Unsetenv(loomProxyMaxToolResultBytesEnv)
	saved := proxyConfigGlobal
	defer func() { proxyConfigGlobal = saved }()

	proxyConfigGlobal = daemon.ProxyConfig{
		MaxToolResultBytes: 48000,
		ToolCaps: []daemon.ToolCap{
			{Server: "gitlab", Tool: "list_pipeline_jobs", MaxBytes: 8192},
			{Server: "tiny", Tool: "t", MaxBytes: 10}, // below floor
		},
	}

	if got := proxyMaxToolResultBytesFor("gitlab", "list_pipeline_jobs"); got != 8192 {
		t.Errorf("capped tool = %d, want 8192", got)
	}
	if got := proxyMaxToolResultBytesFor("gitlab", "uncapped"); got != 48000 {
		t.Errorf("uncapped tool on capped server = %d, want global 48000", got)
	}
	if got := proxyMaxToolResultBytesFor("prometheus", "query"); got != 48000 {
		t.Errorf("uncapped server = %d, want global 48000", got)
	}
	if got := proxyMaxToolResultBytesFor("tiny", "t"); got != minToolCapBytes {
		t.Errorf("below-floor cap = %d, want floor %d", got, minToolCapBytes)
	}
}

func TestProxyToolPageSize_DefaultAndClamp(t *testing.T) {
	os.Unsetenv(loomProxyToolPageSizeEnv)
	if got := proxyToolPageSize(); got != defaultToolPageSize {
		t.Errorf("proxyToolPageSize() = %d, want %d", got, defaultToolPageSize)
	}

	os.Setenv(loomProxyToolPageSizeEnv, "1")
	if got := proxyToolPageSize(); got != minToolPageSize {
		t.Errorf("proxyToolPageSize() = %d, want %d", got, minToolPageSize)
	}

	os.Setenv(loomProxyToolPageSizeEnv, "9999")
	if got := proxyToolPageSize(); got != maxToolPageSize {
		t.Errorf("proxyToolPageSize() = %d, want %d", got, maxToolPageSize)
	}
	os.Unsetenv(loomProxyToolPageSizeEnv)
}

func TestProxyIdleExitTimeout_ConfigFallback(t *testing.T) {
	os.Unsetenv(loomProxyIdleExitSecondsEnv)
	saved := proxyConfigGlobal
	defer func() { proxyConfigGlobal = saved }()

	proxyConfigGlobal = daemon.ProxyConfig{IdleExitSeconds: 600}
	got := proxyIdleExitTimeout()
	if got != 10*time.Minute {
		t.Errorf("proxyIdleExitTimeout() = %v, want 10m", got)
	}
}

func TestProxyIdleExitTimeout_EnvOverridesConfig(t *testing.T) {
	os.Setenv(loomProxyIdleExitSecondsEnv, "120")
	defer os.Unsetenv(loomProxyIdleExitSecondsEnv)
	saved := proxyConfigGlobal
	defer func() { proxyConfigGlobal = saved }()

	proxyConfigGlobal = daemon.ProxyConfig{IdleExitSeconds: 600}
	got := proxyIdleExitTimeout()
	if got != 2*time.Minute {
		t.Errorf("proxyIdleExitTimeout() = %v, want 2m", got)
	}
}

func TestProxyIdleExitTimeout_EnvZeroDisables(t *testing.T) {
	os.Setenv(loomProxyIdleExitSecondsEnv, "0")
	defer os.Unsetenv(loomProxyIdleExitSecondsEnv)
	saved := proxyConfigGlobal
	defer func() { proxyConfigGlobal = saved }()

	proxyConfigGlobal = daemon.ProxyConfig{IdleExitSeconds: 600}
	got := proxyIdleExitTimeout()
	if got != 0 {
		t.Errorf("proxyIdleExitTimeout() = %v, want disabled", got)
	}
}

func TestProxyIdleExitTimeout_MinBound(t *testing.T) {
	os.Setenv(loomProxyIdleExitSecondsEnv, "5")
	defer os.Unsetenv(loomProxyIdleExitSecondsEnv)
	saved := proxyConfigGlobal
	defer func() { proxyConfigGlobal = saved }()

	proxyConfigGlobal = daemon.ProxyConfig{IdleExitSeconds: 0}
	got := proxyIdleExitTimeout()
	if got != 30*time.Second {
		t.Errorf("proxyIdleExitTimeout() = %v, want 30s min bound", got)
	}
}
