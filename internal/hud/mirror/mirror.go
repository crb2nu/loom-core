// Package mirror federates the local daemon's active agent presence to
// a remote HUD by periodically posting /api/agent/heartbeat requests.
//
// Motivation: vendor CLI lifecycle hooks (Claude Code, Codex, etc.)
// always target a single HUD URL — either the local Mac daemon or the
// cluster HUD, never both. When operators run hud.flexinfer.ai as
// their "fleet view," their local laptop agents are invisible because
// their hooks point at localhost. This service closes that gap with a
// fire-and-forget mirror loop: while LOOM_HUD_MIRROR_URL is set, every
// LOOM_HUD_MIRROR_INTERVAL (default 15s) the local daemon reads its
// own presence list and forwards each non-offline agent as an
// ensure_session=true heartbeat to the remote HUD.
//
// The remote HUD's existing heartbeat handler auto-bootstraps a
// session for unknown agent_ids and surfaces the agent in its fleet
// snapshot on the next refresh. The kill-test for the design is
// captured in .loom/local/ralph-iteration-plan-hud-presence-federation-2026-05-25.md.
package mirror

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
	"github.com/crb2nu/loom/internal/visibility/contracts/presence"
)

const (
	defaultMirrorInterval = 15 * time.Second
	defaultMirrorTimeout  = 3 * time.Second
	// initialDelay gives the local daemon a moment to finish wiring its
	// MCP transports before we hit them with PresenceList.
	initialDelay = 2 * time.Second
)

// Doer is the subset of *http.Client we need; an interface seam for tests.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// PresenceReader is the subset of *bridge.AgentBridge the mirror reads
// from. The interface lets tests inject a fake without spinning up a
// real MCP transport.
type PresenceReader interface {
	PresenceList(includeOffline bool) ([]presence.PresenceInfo, error)
	// ActiveSessions returns the active-only, lightweight session list. The
	// mirror only enriches active agents, so it must NOT use the heavy
	// full-fidelity Sessions() (limit=1000, no light projection) — that call
	// blows the daemon's 3s tools/call recv budget on machines with a large
	// session history, starving the entire mirror (no heartbeats posted, so no
	// presence OR tool-call telemetry reaches the central HUD). See !579 and
	// project_hud_no_agents_session_list_timeout.
	ActiveSessions() ([]bridge.SessionInfo, error)
}

// ToolCallReader supplies recent per-session tool-call activity to forward to
// the remote HUD alongside the presence heartbeat. Implemented by the embedded
// HUD App over its EventLog. Returns the matching tool.call payloads with an
// event timestamp strictly greater than sinceUnixNano (up to limit), plus the
// max timestamp observed so the mirror can advance its per-session cursor and
// avoid re-forwarding the same calls every cycle.
type ToolCallReader interface {
	RecentToolCallsForSession(sessionID string, sinceUnixNano int64, limit int) (calls []map[string]any, maxTSUnixNano int64)
}

const mirrorToolCallLimit = 25

// Config controls the mirror loop. Zero-valued fields fall back to
// sane defaults.
type Config struct {
	// URL is the remote HUD base URL (e.g. "https://hud.flexinfer.ai").
	// When empty, the service is disabled.
	URL string
	// Interval between mirror cycles. Defaults to 15s.
	Interval time.Duration
	// Timeout per individual HTTP POST. Defaults to 3s.
	Timeout time.Duration
	// Optional bearer token sent as `Authorization: Bearer ...`.
	Token string
	// Optional Cloudflare Access service-token creds.
	CFAccessClientID     string
	CFAccessClientSecret string
}

// NewConfigFromEnv reads LOOM_HUD_MIRROR_* env vars into a Config.
// Returns the zero value when LOOM_HUD_MIRROR_URL is unset.
func NewConfigFromEnv() Config {
	cfg := Config{
		URL:                  strings.TrimSpace(os.Getenv("LOOM_HUD_MIRROR_URL")),
		Token:                strings.TrimSpace(os.Getenv("LOOM_HUD_MIRROR_TOKEN")),
		CFAccessClientID:     strings.TrimSpace(os.Getenv("LOOM_HUD_MIRROR_CF_ACCESS_CLIENT_ID")),
		CFAccessClientSecret: strings.TrimSpace(os.Getenv("LOOM_HUD_MIRROR_CF_ACCESS_CLIENT_SECRET")),
	}
	if v := strings.TrimSpace(os.Getenv("LOOM_HUD_MIRROR_INTERVAL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.Interval = d
		}
	}
	if v := strings.TrimSpace(os.Getenv("LOOM_HUD_MIRROR_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.Timeout = d
		}
	}
	return cfg
}

// Service runs the mirror loop.
type Service struct {
	cfg    Config
	reader PresenceReader
	client Doer
	logger *slog.Logger

	cancel context.CancelFunc
	done   chan struct{}

	// Optional per-session tool-call forwarding (nil = disabled).
	toolCalls  ToolCallReader
	tcCursorMu sync.Mutex
	tcCursor   map[string]int64 // session_id -> last forwarded event ts (unix nano)

	// Coalesce identical errors to one log line per failure streak.
	lastErr        atomic.Pointer[string]
	consecutiveErr atomic.Int64
}

// SetToolCalls wires an optional tool-call source whose recent per-session
// activity is forwarded alongside each heartbeat. Call once before Start.
func (s *Service) SetToolCalls(r ToolCallReader) { s.toolCalls = r }

// recentToolCallsSince returns this session's tool calls newer than the stored
// cursor and advances the cursor past them. Returns nil when none/disabled.
func (s *Service) recentToolCallsSince(sessionID string) []map[string]any {
	if s.toolCalls == nil || sessionID == "" {
		return nil
	}
	s.tcCursorMu.Lock()
	since := s.tcCursor[sessionID]
	s.tcCursorMu.Unlock()

	calls, maxTS := s.toolCalls.RecentToolCallsForSession(sessionID, since, mirrorToolCallLimit)
	if maxTS > since {
		s.tcCursorMu.Lock()
		s.tcCursor[sessionID] = maxTS
		s.tcCursorMu.Unlock()
	}
	if len(calls) == 0 {
		return nil
	}
	return calls
}

// New constructs a Service. cfg.URL must be non-empty; callers should
// gate construction on Config.Enabled().
func New(cfg Config, reader PresenceReader, client Doer, logger *slog.Logger) *Service {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultMirrorInterval
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultMirrorTimeout
	}
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout + time.Second}
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Service{
		cfg:      cfg,
		reader:   reader,
		client:   client,
		logger:   logger,
		done:     make(chan struct{}),
		tcCursor: make(map[string]int64),
	}
}

// Enabled reports whether mirroring is configured.
func (c Config) Enabled() bool { return strings.TrimSpace(c.URL) != "" }

// Start launches the background loop. Safe to call once; subsequent
// calls are a no-op. Stop with Stop().
func (s *Service) Start(ctx context.Context) {
	if s.cancel != nil {
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	go s.run(loopCtx)
}

// Stop cancels the loop and waits for it to exit. Safe to call before Start.
func (s *Service) Stop() {
	if s.cancel == nil {
		return
	}
	s.cancel()
	<-s.done
	s.cancel = nil
}

// MirrorOnce runs a single mirror cycle synchronously. Exposed for
// tests and for the daemon's "kick on heartbeat" plumbing if we add it
// later.
func (s *Service) MirrorOnce(ctx context.Context) (posted, failed int) {
	return s.mirrorOnce(ctx)
}

func (s *Service) run(ctx context.Context) {
	defer close(s.done)

	// Short intervals (test fixtures) shouldn't wait the full 2s
	// settling delay; production keeps the headroom.
	startDelay := initialDelay
	if s.cfg.Interval < startDelay {
		startDelay = s.cfg.Interval
	}
	timer := time.NewTimer(startDelay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.mirrorOnce(ctx)
		timer.Reset(s.cfg.Interval)
	}
}

func (s *Service) mirrorOnce(ctx context.Context) (posted, failed int) {
	agents, err := s.reader.PresenceList(false)
	if err != nil {
		s.logErr("presence_list", err)
		return 0, 0
	}

	// Active sessions are optional context — they let us forward richer
	// namespace info. If the call fails we still mirror presence-only rows
	// (the cluster will bootstrap a session). Use the lightweight active-only
	// projection: it stays inside the 3s recv budget where full Sessions()
	// times out and silently kills the mirror.
	sessions, sessionsErr := s.reader.ActiveSessions()
	if sessionsErr != nil {
		s.logErr("sessions", sessionsErr)
	}
	sessionByAgent := make(map[string]bridge.SessionInfo, len(sessions))
	for _, sess := range sessions {
		if sess.Status != "active" {
			continue
		}
		if _, ok := sessionByAgent[sess.AgentID]; ok {
			continue
		}
		sessionByAgent[sess.AgentID] = sess
	}

	for _, a := range agents {
		if !shouldMirror(a) {
			continue
		}
		body := buildHeartbeatBody(a, sessionByAgent[a.AgentID])
		if sid, _ := body["session_id"].(string); sid != "" {
			if calls := s.recentToolCallsSince(sid); len(calls) > 0 {
				body["recent_tool_calls"] = calls
			}
		}
		if err := s.postHeartbeat(ctx, body); err != nil {
			failed++
			s.logErr("heartbeat", err)
			continue
		}
		posted++
	}
	if posted > 0 || failed > 0 {
		s.logger.Debug(
			"hud mirror cycle",
			"url", s.cfg.URL,
			"posted", posted,
			"failed", failed,
			"total", len(agents),
		)
	}
	if posted > 0 && failed == 0 {
		// Reset the failure-coalesce dedup on a clean cycle.
		s.consecutiveErr.Store(0)
		s.lastErr.Store(nil)
	}
	return posted, failed
}

// shouldMirror filters out rows the cluster shouldn't see — offline,
// expired, or empty agent_ids. Anything else (active, idle, unknown)
// is mirrored so the cluster can decide how to render it.
func shouldMirror(p presence.PresenceInfo) bool {
	if strings.TrimSpace(p.AgentID) == "" {
		return false
	}
	switch strings.ToLower(p.Status) {
	case "offline", "expired":
		return false
	}
	return true
}

// buildHeartbeatBody assembles the JSON payload for the remote HUD's
// /api/agent/heartbeat endpoint. ensure_session=true tells the cluster
// to auto-bootstrap a session row for first-sight agent_ids.
func buildHeartbeatBody(p presence.PresenceInfo, sess bridge.SessionInfo) map[string]any {
	description := firstNonEmpty(sess.Description, p.Description, "loom-core federation mirror")
	namespace := firstNonEmpty(sess.Namespace, "agents/"+p.AgentID)
	body := map[string]any{
		"agent_id":       p.AgentID,
		"agent_type":     firstNonEmpty(p.AgentType, "unknown"),
		"namespace":      namespace,
		"description":    description,
		"status":         firstNonEmpty(strings.ToLower(p.Status), "active"),
		"ensure_session": true,
	}
	if p.SessionID != "" {
		body["session_id"] = p.SessionID
	} else if sess.ID != "" {
		body["session_id"] = sess.ID
	}
	if p.Branch != "" {
		body["branch"] = p.Branch
	}
	if p.CurrentTask != "" {
		body["current_task"] = p.CurrentTask
	}
	if len(p.ActiveFiles) > 0 {
		body["active_files"] = p.ActiveFiles
	}
	return body
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (s *Service) postHeartbeat(ctx context.Context, body map[string]any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	url := strings.TrimRight(s.cfg.URL, "/") + "/api/agent/heartbeat"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	if s.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	}
	if s.cfg.CFAccessClientID != "" && s.cfg.CFAccessClientSecret != "" {
		req.Header.Set("CF-Access-Client-Id", s.cfg.CFAccessClientID)
		req.Header.Set("CF-Access-Client-Secret", s.cfg.CFAccessClientSecret)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		// Read a small slice of the body for diagnostics; the cluster's
		// failure messages are short. Bound to avoid huge log lines.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

// logErr emits a warn log on the first occurrence of an error string
// and stays silent for subsequent identical errors in the same streak.
// Resets when mirrorOnce sees a clean cycle.
func (s *Service) logErr(stage string, err error) {
	s.consecutiveErr.Add(1)
	cur := err.Error()
	last := s.lastErr.Load()
	if last != nil && *last == cur {
		return
	}
	s.lastErr.Store(&cur)
	s.logger.Warn("hud mirror error",
		"stage", stage,
		"url", s.cfg.URL,
		"error", cur,
	)
}
