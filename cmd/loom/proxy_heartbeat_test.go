package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/hud"
)

func TestHeartbeatRateLimiting(t *testing.T) {
	// Reset state
	lastHeartbeat.Store(0)
	saved := heartbeatIntervalNanos
	defer func() { heartbeatIntervalNanos = saved }()

	// Use a long interval so second call is suppressed
	heartbeatIntervalNanos = int64(10 * time.Second)

	var count atomic.Int64

	// Simulate two rapid calls checking the rate limit
	for i := 0; i < 10; i++ {
		now := time.Now().UnixNano()
		prev := lastHeartbeat.Load()
		if now-prev >= heartbeatIntervalNanos {
			if lastHeartbeat.CompareAndSwap(prev, now) {
				count.Add(1)
			}
		}
	}

	if got := count.Load(); got != 1 {
		t.Errorf("expected 1 heartbeat within interval, got %d", got)
	}
}

func TestHeartbeatRateLimiting_AllowsAfterInterval(t *testing.T) {
	lastHeartbeat.Store(0)
	saved := heartbeatIntervalNanos
	defer func() { heartbeatIntervalNanos = saved }()

	// Very short interval
	heartbeatIntervalNanos = int64(1 * time.Millisecond)

	var count atomic.Int64

	// First call
	now := time.Now().UnixNano()
	prev := lastHeartbeat.Load()
	if now-prev >= heartbeatIntervalNanos {
		if lastHeartbeat.CompareAndSwap(prev, now) {
			count.Add(1)
		}
	}

	// Wait past interval
	time.Sleep(2 * time.Millisecond)

	// Second call should be allowed
	now = time.Now().UnixNano()
	prev = lastHeartbeat.Load()
	if now-prev >= heartbeatIntervalNanos {
		if lastHeartbeat.CompareAndSwap(prev, now) {
			count.Add(1)
		}
	}

	if got := count.Load(); got != 2 {
		t.Errorf("expected 2 heartbeats after interval, got %d", got)
	}
}

func TestNextSessionKeepaliveInterval_NoActivity(t *testing.T) {
	lastProxyCallTime.Store(0)
	savedActive := sessionKeepaliveActive
	savedIdle := sessionKeepaliveIdle
	savedThreshold := sessionIdleThreshold
	defer func() {
		sessionKeepaliveActive = savedActive
		sessionKeepaliveIdle = savedIdle
		sessionIdleThreshold = savedThreshold
	}()

	sessionKeepaliveActive = 5 * time.Second
	sessionKeepaliveIdle = 30 * time.Second
	sessionIdleThreshold = 30 * time.Second

	// No activity recorded → idle interval.
	got := nextSessionKeepaliveInterval()
	if got != 30*time.Second {
		t.Fatalf("expected 30s idle interval, got %v", got)
	}
}

func TestNextSessionKeepaliveInterval_RecentActivity(t *testing.T) {
	savedActive := sessionKeepaliveActive
	savedIdle := sessionKeepaliveIdle
	savedThreshold := sessionIdleThreshold
	defer func() {
		sessionKeepaliveActive = savedActive
		sessionKeepaliveIdle = savedIdle
		sessionIdleThreshold = savedThreshold
		lastProxyCallTime.Store(0)
	}()

	sessionKeepaliveActive = 5 * time.Second
	sessionKeepaliveIdle = 30 * time.Second
	sessionIdleThreshold = 30 * time.Second

	// Activity just now → active interval.
	lastProxyCallTime.Store(time.Now().UnixNano())
	got := nextSessionKeepaliveInterval()
	if got != 5*time.Second {
		t.Fatalf("expected 5s active interval, got %v", got)
	}
}

func TestNextSessionKeepaliveInterval_StaleActivity(t *testing.T) {
	savedActive := sessionKeepaliveActive
	savedIdle := sessionKeepaliveIdle
	savedThreshold := sessionIdleThreshold
	defer func() {
		sessionKeepaliveActive = savedActive
		sessionKeepaliveIdle = savedIdle
		sessionIdleThreshold = savedThreshold
		lastProxyCallTime.Store(0)
	}()

	sessionKeepaliveActive = 5 * time.Second
	sessionKeepaliveIdle = 30 * time.Second
	sessionIdleThreshold = 1 * time.Millisecond // tiny threshold for test

	// Activity in the past, beyond threshold → idle interval.
	lastProxyCallTime.Store(time.Now().Add(-10 * time.Millisecond).UnixNano())
	time.Sleep(2 * time.Millisecond) // ensure we're past threshold
	got := nextSessionKeepaliveInterval()
	if got != 30*time.Second {
		t.Fatalf("expected 30s idle interval for stale activity, got %v", got)
	}
}

func TestResolveProxyIdentity_UsesEnvOverride(t *testing.T) {
	t.Setenv("LOOM_PROXY_AGENT_ID", "custom-proxy-id")
	proxyIdentityOnce = sync.Once{}
	proxyAgentID = ""

	id, typ := resolveProxyIdentity("codex")
	if typ != "codex" {
		t.Fatalf("agentType = %q, want codex", typ)
	}
	if id != "custom-proxy-id" {
		t.Fatalf("agentID = %q, want custom-proxy-id", id)
	}
}

func TestResolveProxyIdentity_GeneratesStableWorkspaceScopedID(t *testing.T) {
	t.Setenv("LOOM_PROXY_AGENT_ID", "")
	proxyIdentityOnce = sync.Once{}
	proxyAgentID = ""

	id1, typ1 := resolveProxyIdentity("claude-code")
	id2, typ2 := resolveProxyIdentity("claude-code")

	if typ1 != "claude-code" || typ2 != "claude-code" {
		t.Fatalf("unexpected agent type values: %q %q", typ1, typ2)
	}
	if id1 == "" || id2 == "" {
		t.Fatalf("expected non-empty generated IDs, got %q and %q", id1, id2)
	}
	if id1 != id2 {
		t.Fatalf("expected stable ID within process, got %q != %q", id1, id2)
	}

	// claude-code (like every platform now) resolves to the workspace-scoped
	// key <type>-<cksum(workspace root)> so its tool calls correlate to the
	// hook-registered HUD session — NOT a process-scoped host-pid id.
	prefix := "claude-code-"
	if !strings.HasPrefix(id1, prefix) {
		t.Fatalf("expected id %q to start with %q", id1, prefix)
	}
	if want, ok := stableWorkspaceProxyAgentID("claude-code"); !ok || id1 != want {
		t.Fatalf("expected workspace-scoped id %q, got %q (ok=%v)", want, id1, ok)
	}
	pidFragment := fmt.Sprintf("-%d", os.Getpid())
	if strings.Contains(id1, pidFragment) {
		t.Fatalf("id %q must NOT include the pid fragment %q (workspace-scoped, not process-scoped)", id1, pidFragment)
	}
}

func TestResolveProxyIdentity_CodexMatchesKeepaliveWorkspaceID(t *testing.T) {
	t.Setenv("LOOM_PROXY_AGENT_ID", "")
	t.Chdir(t.TempDir())
	proxyIdentityOnce = sync.Once{}
	proxyAgentID = ""

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	id, typ := resolveProxyIdentity("codex")

	if typ != "codex" {
		t.Fatalf("agentType = %q, want codex", typ)
	}
	want := fmt.Sprintf("codex-%d", posixCKSumString(wd))
	if id != want {
		t.Fatalf("agentID = %q, want %q", id, want)
	}
	pidFragment := fmt.Sprintf("-%d", os.Getpid())
	if strings.Contains(id, pidFragment) {
		t.Fatalf("expected codex identity %q not to include pid fragment %q", id, pidFragment)
	}
}

// TestWorkspaceProxyAgentIDFor_CodexFoldsWorktree proves the proxy side of the
// codex cross-worktree fix: codex's `codex-<WS_HASH>` folds all worktrees of one
// repo to the same id (matching the canonicalized notify mint), while
// conversation-scoped vendors stay worktree-specific so they keep matching their
// unchanged shell bootstrap.
func TestWorkspaceProxyAgentIDFor_CodexFoldsWorktree(t *testing.T) {
	const main = "/home/u/workspace/services/loom-core"
	mainID := workspaceProxyAgentIDFor("codex", main)
	for _, wt := range []string{
		main + "/.worktrees/feat-x",
		main + "/.claude/worktrees/agent-7",
		main + "/.worktrees/fix/nested-branch",
	} {
		if got := workspaceProxyAgentIDFor("codex", wt); got != mainID {
			t.Errorf("codex worktree %q → %q, want %q (must fold to main repo)", wt, got, mainID)
		}
	}
	// A different repo must not collide.
	if other := workspaceProxyAgentIDFor("codex", "/home/u/workspace/services/flexinfer"); other == mainID {
		t.Errorf("distinct repo collided with codex main id %q", mainID)
	}
	// Conversation-scoped vendors must NOT fold worktrees.
	for _, typ := range []string{"claude-code", "gemini-cli"} {
		base := workspaceProxyAgentIDFor(typ, main)
		wt := workspaceProxyAgentIDFor(typ, main+"/.worktrees/feat-x")
		if base == wt {
			t.Errorf("%s must NOT fold worktrees: main=%q wt=%q", typ, base, wt)
		}
	}
}

func TestPosixCKSumStringMatchesSystemCKSumExamples(t *testing.T) {
	tests := []struct {
		input string
		want  uint32
	}{
		{input: "", want: 4294967295},
		{input: "abc", want: 1219131554},
		{input: "/Users/cblevins/workspace/services/loom-core", want: 552019522},
	}

	for _, tt := range tests {
		if got := posixCKSumString(tt.input); got != tt.want {
			t.Fatalf("posixCKSumString(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

// TestProxyHeartbeat_ReadsCanonicalPortFile verifies that proxyHeartbeat reads
// the port from hud.PortFilePath() (the canonical ~/.config/loom/hud.port)
// instead of the old hardcoded /tmp/loom-hud.port path.
func TestProxyHeartbeat_ReadsCanonicalPortFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// Start a test HTTP server that records heartbeat requests.
	var received atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/agent/heartbeat" {
			received.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Extract the port from the test server URL.
	parts := strings.SplitN(srv.URL, ":", 3)
	port := parts[2] // "PORT" from "http://127.0.0.1:PORT"

	// Write the port to the canonical location.
	_, err := hud.WritePortFile(mustAtoi(port))
	if err != nil {
		t.Fatalf("WritePortFile: %v", err)
	}
	defer hud.RemovePortFile()

	// Reset heartbeat state so the call goes through.
	lastHeartbeat.Store(0)
	proxyIdentityOnce = sync.Once{}
	proxyAgentID = ""
	proxyNamespaceOnce = sync.Once{}

	// proxyHeartbeat should discover the test server via the port file.
	proxyHeartbeat("test-agent")

	// Give a moment for the HTTP request to complete.
	time.Sleep(100 * time.Millisecond)

	if !received.Load() {
		t.Fatal("proxyHeartbeat did not send request to the server discovered via hud.PortFilePath()")
	}
}

func mustAtoi(s string) int {
	var n int
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// TestStableWorkspaceProxyAgentID_AllPlatforms verifies the workspace-scoped
// proxy id now applies to every agent type (not just codex) so interactive
// agents' tool calls carry a HUD-correlatable <type>-<WS_HASH> id. Mirrors the
// hook's cksum-of-git-toplevel derivation.
func TestStableWorkspaceProxyAgentID_AllPlatforms(t *testing.T) {
	for _, typ := range []string{"claude-code", "codex", "gemini-cli", "kilocode"} {
		id, ok := stableWorkspaceProxyAgentID(typ)
		if !ok {
			t.Fatalf("stableWorkspaceProxyAgentID(%q) ok=false, want true (gate should be removed for all types)", typ)
		}
		if !strings.HasPrefix(id, typ+"-") {
			t.Fatalf("id %q does not start with %q-", id, typ)
		}
		// suffix must be the numeric cksum of the workspace root
		suffix := strings.TrimPrefix(id, typ+"-")
		if suffix == "" {
			t.Fatalf("id %q has empty cksum suffix", id)
		}
		for _, r := range suffix {
			if r < '0' || r > '9' {
				t.Fatalf("id %q cksum suffix %q is not all digits", id, suffix)
			}
		}
	}
	if _, ok := stableWorkspaceProxyAgentID(""); ok {
		t.Fatal("empty agent type should not yield a stable id")
	}
}
