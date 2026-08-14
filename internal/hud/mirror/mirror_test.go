package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
	"github.com/crb2nu/loom/internal/visibility/contracts/presence"
)

// fakeReader is a deterministic PresenceReader for tests.
type fakeReader struct {
	mu          sync.Mutex
	agents      []presence.PresenceInfo
	sessions    []bridge.SessionInfo
	presenceErr error
	sessionsErr error
	calls       atomic.Int32
}

func (f *fakeReader) PresenceList(_ bool) ([]presence.PresenceInfo, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.agents, f.presenceErr
}

func (f *fakeReader) ActiveSessions() ([]bridge.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions, f.sessionsErr
}

func TestConfigFromEnv_DisabledWhenURLEmpty(t *testing.T) {
	t.Setenv("LOOM_HUD_MIRROR_URL", "")
	t.Setenv("LOOM_HUD_MIRROR_INTERVAL", "")
	cfg := NewConfigFromEnv()
	if cfg.Enabled() {
		t.Fatalf("expected disabled, got URL=%q", cfg.URL)
	}
}

func TestConfigFromEnv_ParsesAll(t *testing.T) {
	t.Setenv("LOOM_HUD_MIRROR_URL", "https://hud.example/")
	t.Setenv("LOOM_HUD_MIRROR_INTERVAL", "7s")
	t.Setenv("LOOM_HUD_MIRROR_TIMEOUT", "1s")
	t.Setenv("LOOM_HUD_MIRROR_TOKEN", "abc")
	t.Setenv("LOOM_HUD_MIRROR_CF_ACCESS_CLIENT_ID", "id")
	t.Setenv("LOOM_HUD_MIRROR_CF_ACCESS_CLIENT_SECRET", "secret")
	cfg := NewConfigFromEnv()
	if cfg.URL != "https://hud.example/" {
		t.Errorf("URL = %q", cfg.URL)
	}
	if cfg.Interval != 7*time.Second {
		t.Errorf("Interval = %v", cfg.Interval)
	}
	if cfg.Timeout != time.Second {
		t.Errorf("Timeout = %v", cfg.Timeout)
	}
	if cfg.Token != "abc" || cfg.CFAccessClientID != "id" || cfg.CFAccessClientSecret != "secret" {
		t.Errorf("creds not populated: %+v", cfg)
	}
}

// TestMirrorOnce_FiltersOfflineAndEmpty proves we don't forward rows
// the cluster shouldn't see and we always set ensure_session=true so
// the cluster can bootstrap unknown agent_ids.
func TestMirrorOnce_FiltersOfflineAndEmpty(t *testing.T) {
	reader := &fakeReader{
		agents: []presence.PresenceInfo{
			{AgentID: "claude-code-1", AgentType: "claude-code", Status: "active", SessionID: "sess-1"},
			{AgentID: "claude-code-2", AgentType: "claude-code", Status: "offline"}, // dropped
			{AgentID: "", AgentType: "ghost", Status: "active"},                     // dropped
			{AgentID: "codex-1", AgentType: "codex", Status: "idle"},                // mirrored
			{AgentID: "expired-1", Status: "expired"},                               // dropped
		},
		sessions: []bridge.SessionInfo{
			{ID: "sess-1", AgentID: "claude-code-1", Status: "active", Namespace: "ns-1", Description: "desc-1"},
		},
	}

	var (
		mu   sync.Mutex
		hits []map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/heartbeat" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("body unmarshal: %v", err)
		}
		mu.Lock()
		hits = append(hits, got)
		mu.Unlock()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	s := New(Config{URL: srv.URL, Interval: time.Hour, Timeout: time.Second}, reader, srv.Client(), nil)
	posted, failed := s.MirrorOnce(context.Background())
	if posted != 2 || failed != 0 {
		t.Fatalf("posted=%d failed=%d", posted, failed)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	gotAgents := map[string]bool{}
	for _, h := range hits {
		gotAgents[h["agent_id"].(string)] = true
		if h["ensure_session"] != true {
			t.Errorf("ensure_session missing on %v", h)
		}
	}
	if !gotAgents["claude-code-1"] || !gotAgents["codex-1"] {
		t.Errorf("missing mirrored agent: %v", gotAgents)
	}
	// claude-code-1 should pick up the session-derived namespace.
	for _, h := range hits {
		if h["agent_id"] == "claude-code-1" {
			if h["namespace"] != "ns-1" {
				t.Errorf("namespace not derived from session: %v", h)
			}
			if h["description"] != "desc-1" {
				t.Errorf("description not derived from session: %v", h)
			}
		}
		if h["agent_id"] == "codex-1" {
			if h["namespace"] != "agents/codex-1" {
				t.Errorf("namespace fallback wrong: %v", h)
			}
		}
	}
}

// TestMirrorOnce_HeadersPropagated proves bearer + CF Access headers
// are forwarded to the remote HUD.
func TestMirrorOnce_HeadersPropagated(t *testing.T) {
	reader := &fakeReader{
		agents: []presence.PresenceInfo{
			{AgentID: "claude-code-1", Status: "active"},
		},
	}

	var (
		mu    sync.Mutex
		gotID string
		gotSc string
		gotBT string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotID = r.Header.Get("CF-Access-Client-Id")
		gotSc = r.Header.Get("CF-Access-Client-Secret")
		gotBT = r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := New(Config{
		URL:                  srv.URL,
		Interval:             time.Hour,
		Timeout:              time.Second,
		Token:                "token-xyz",
		CFAccessClientID:     "id-abc",
		CFAccessClientSecret: "secret-def",
	}, reader, srv.Client(), nil)
	posted, failed := s.MirrorOnce(context.Background())
	if posted != 1 || failed != 0 {
		t.Fatalf("posted=%d failed=%d", posted, failed)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotID != "id-abc" || gotSc != "secret-def" || gotBT != "Bearer token-xyz" {
		t.Errorf("headers not propagated: id=%q sec=%q bearer=%q", gotID, gotSc, gotBT)
	}
}

// TestMirrorOnce_HTTPErrorCountsAsFailed proves a 5xx response is
// counted as a failed forward and surfaced via the result, not
// silently swallowed.
func TestMirrorOnce_HTTPErrorCountsAsFailed(t *testing.T) {
	reader := &fakeReader{
		agents: []presence.PresenceInfo{
			{AgentID: "claude-code-1", Status: "active"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	s := New(Config{URL: srv.URL, Interval: time.Hour, Timeout: time.Second}, reader, srv.Client(), nil)
	posted, failed := s.MirrorOnce(context.Background())
	if posted != 0 || failed != 1 {
		t.Fatalf("posted=%d failed=%d", posted, failed)
	}
}

// TestMirrorOnce_PresenceErrorReturnsZero verifies that a presence-list
// error short-circuits the cycle without panicking.
func TestMirrorOnce_PresenceErrorReturnsZero(t *testing.T) {
	reader := &fakeReader{presenceErr: errors.New("mcp gone")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatalf("should not be called")
	}))
	defer srv.Close()

	s := New(Config{URL: srv.URL, Interval: time.Hour, Timeout: time.Second}, reader, srv.Client(), nil)
	posted, failed := s.MirrorOnce(context.Background())
	if posted != 0 || failed != 0 {
		t.Fatalf("posted=%d failed=%d, want both 0", posted, failed)
	}
}

// TestMirrorOnce_SessionsErrorStillForwardsPresence proves the mirror
// degrades to presence-only when the sessions call fails: we still
// post heartbeats, just with the agents/<id> namespace fallback.
func TestMirrorOnce_SessionsErrorStillForwardsPresence(t *testing.T) {
	reader := &fakeReader{
		agents: []presence.PresenceInfo{
			{AgentID: "claude-code-1", Status: "active"},
		},
		sessionsErr: errors.New("sessions mcp timeout"),
	}
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := New(Config{URL: srv.URL, Interval: time.Hour, Timeout: time.Second}, reader, srv.Client(), nil)
	posted, failed := s.MirrorOnce(context.Background())
	if posted != 1 || failed != 0 {
		t.Fatalf("posted=%d failed=%d", posted, failed)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("hits=%d, want 1", hits)
	}
}

// TestStartStop_CleanShutdown proves Start/Stop don't leak the loop
// goroutine even when no mirror cycle has produced a result yet.
func TestStartStop_CleanShutdown(t *testing.T) {
	reader := &fakeReader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	s := New(Config{URL: srv.URL, Interval: 50 * time.Millisecond, Timeout: time.Second}, reader, srv.Client(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	s.Start(ctx)
	// Wait for at least one cycle.
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) && reader.calls.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if reader.calls.Load() == 0 {
		t.Fatalf("background loop never ran a cycle")
	}
	s.Stop()
}

// TestLogErr_CoalescesIdenticalErrors guards against the loop spamming
// logs when the cluster has been unreachable for hours.
func TestLogErr_CoalescesIdenticalErrors(t *testing.T) {
	reader := &fakeReader{presenceErr: errors.New("mcp gone")}
	var calls atomic.Int32
	logger := slog.New(slogHandlerFunc(func() { calls.Add(1) }))
	srv := httptest.NewServer(http.HandlerFunc(nil))
	defer srv.Close()

	s := New(Config{URL: srv.URL, Interval: time.Hour, Timeout: time.Second}, reader, srv.Client(), logger)
	for i := 0; i < 5; i++ {
		s.MirrorOnce(context.Background())
	}
	got := calls.Load()
	if got != 1 {
		t.Fatalf("expected 1 log line, got %d", got)
	}
}

// slogHandlerFunc counts log invocations without producing output.
type slogHandlerFunc func()

func (h slogHandlerFunc) Enabled(_ context.Context, _ slog.Level) bool  { return true }
func (h slogHandlerFunc) WithAttrs(_ []slog.Attr) slog.Handler          { return h }
func (h slogHandlerFunc) WithGroup(_ string) slog.Handler               { return h }
func (h slogHandlerFunc) Handle(_ context.Context, _ slog.Record) error { h(); return nil }

// "Hidden" import dance: slog is referenced via the handler above; the
// import block is materialised by go imports / vet via the type's use.
var _ = func() string { return strings.ToLower("MIRROR") }
