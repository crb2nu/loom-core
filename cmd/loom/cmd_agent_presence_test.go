package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

// TestAgentKeepaliveDeregisterUsesSharedEndpoint drives the real keepalive
// command to its max-lifetime deadline and captures the deregistration POST it
// fires on the way out.
//
// The path must be bridge.AgentDeregisterEndpoint, the same constant the fleet
// domain registers its route from (see internal/hud/domain/fleet:
// TestFleetDomainRegistersDeregisterEndpoint). Previously the CLI hardcoded
// "/api/agent/deregister" with no route behind it anywhere in the HUD, and the
// response was discarded — so every terminating agent's final deregister
// silently 404'd against the SPA catch-all.
func TestAgentKeepaliveDeregisterUsesSharedEndpoint(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LOOM_HUD_CF_ACCESS_ID", "")
	t.Setenv("LOOM_HUD_CF_ACCESS_SECRET", "")
	hudConfigOnce.loaded = false
	defer func() { hudConfigOnce.loaded = false }()

	var mu sync.Mutex
	type capture struct {
		method string
		path   string
		body   string
	}
	var seen []capture

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, capture{method: r.Method, path: r.URL.Path, body: string(raw)})
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	t.Setenv("LOOM_HUD_URL", ts.URL)

	cmd := newAgentKeepaliveCmd()
	cmd.SetArgs([]string{
		"--agent-id", "claude-code-test",
		"--interval", "1h",
		"--max-lifetime", "10ms",
		"--quiet",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("keepalive command error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	var dereg *capture
	for i := range seen {
		if seen[i].path == bridge.AgentDeregisterEndpoint {
			dereg = &seen[i]
			break
		}
	}
	if dereg == nil {
		t.Fatalf("keepalive never posted to %s; captured requests: %+v",
			bridge.AgentDeregisterEndpoint, seen)
	}
	if dereg.method != http.MethodPost {
		t.Errorf("deregister method = %s, want POST", dereg.method)
	}

	var body bridge.DeregisterRequest
	if err := json.Unmarshal([]byte(dereg.body), &body); err != nil {
		t.Fatalf("deregister body %q is not a DeregisterRequest: %v", dereg.body, err)
	}
	if body.AgentID != "claude-code-test" {
		t.Errorf("deregister agent_id = %q, want claude-code-test", body.AgentID)
	}
}

func TestNewAgentKeepaliveCmdHasKeepaliveWrapAlias(t *testing.T) {
	cmd := newAgentKeepaliveCmd()

	found := false
	for _, alias := range cmd.Aliases {
		if alias == "keepalive-wrap" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected keepalive command to expose keepalive-wrap alias, got %v", cmd.Aliases)
	}
}

func TestRunKeepaliveLoopSendsHeartbeatsWhileChildRuns(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	var heartbeats atomic.Int32
	var deregisters atomic.Int32
	childStarted := make(chan struct{})

	err := runKeepaliveLoop(context.Background(), keepaliveLoopOptions{
		agentID:     "codex-test",
		interval:    10 * time.Millisecond,
		maxLifetime: 0,
		quiet:       true,
	}, keepaliveLoopDeps{
		sendHeartbeat: func() error {
			heartbeats.Add(1)
			return nil
		},
		deregister: func() {
			deregisters.Add(1)
		},
		runChild: func(ctx context.Context) error {
			close(childStarted)
			select {
			case <-time.After(40 * time.Millisecond):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	if err != nil {
		t.Fatalf("runKeepaliveLoop() error: %v", err)
	}
	if got := heartbeats.Load(); got < 2 {
		t.Fatalf("expected at least 2 heartbeats while child ran, got %d", got)
	}
	if got := deregisters.Load(); got != 1 {
		t.Fatalf("expected one deregister on child exit, got %d", got)
	}
	select {
	case <-childStarted:
	default:
		t.Fatal("expected child runner to start")
	}
}

func TestRunKeepaliveLoopCancelsChildCleanly(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var deregisters atomic.Int32
	childStarted := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- runKeepaliveLoop(ctx, keepaliveLoopOptions{
			agentID:     "codex-test",
			interval:    10 * time.Millisecond,
			maxLifetime: 0,
			quiet:       true,
		}, keepaliveLoopDeps{
			sendHeartbeat: func() error { return nil },
			deregister: func() {
				deregisters.Add(1)
			},
			runChild: func(childCtx context.Context) error {
				close(childStarted)
				<-childCtx.Done()
				return nil
			},
		})
	}()

	select {
	case <-childStarted:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("child runner did not start")
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runKeepaliveLoop() after cancel = %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("keepalive loop did not exit after cancel")
	}

	if got := deregisters.Load(); got != 1 {
		t.Fatalf("expected one deregister on shutdown, got %d", got)
	}
}
