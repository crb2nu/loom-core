package daemon

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// TestResolveEffectiveAgentID covers the fallback chain used to stamp audit,
// cost, and OTel records when a call omits an explicit per-call agent_id.
func TestResolveEffectiveAgentID(t *testing.T) {
	d := &Daemon{sessions: NewSessionManager(10, time.Minute, 1, slog.Default())}

	presenceSess := d.sessions.Open(SessionClientInfo{PresenceAgentID: "claude-code", AgentHint: "claude"}, "")
	hintOnlySess := d.sessions.Open(SessionClientInfo{AgentHint: "codex-cli"}, "")
	emptySess := d.sessions.Open(SessionClientInfo{}, "")

	tests := []struct {
		name   string
		params callParams
		want   string
	}{
		{
			name:   "explicit agent id wins",
			params: callParams{AgentID: "explicit-agent", SessionID: presenceSess.ID},
			want:   "explicit-agent",
		},
		{
			name:   "falls back to presence agent id",
			params: callParams{SessionID: presenceSess.ID},
			want:   "claude-code",
		},
		{
			name:   "falls back to agent hint when no presence id",
			params: callParams{SessionID: hintOnlySess.ID},
			want:   "codex-cli",
		},
		{
			name:   "empty when session carries no identity",
			params: callParams{SessionID: emptySess.ID},
			want:   "",
		},
		{
			name:   "empty when session unknown",
			params: callParams{SessionID: "does-not-exist"},
			want:   "",
		},
		{
			name:   "empty when no session id",
			params: callParams{},
			want:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := d.resolveEffectiveAgentID(tc.params); got != tc.want {
				t.Fatalf("resolveEffectiveAgentID() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEmitAuditResolvesProxySessionAgentID is the regression for the HUD
// "No captured activity" bug: a proxied agent call omits params.AgentID, so the
// audit entry must inherit the proxy session's presence agent id rather than
// recording an empty agent_id (which the per-session trace filter discards).
func TestEmitAuditResolvesProxySessionAgentID(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")
	audit, err := NewAuditLogger(AuditConfig{Enabled: true, LogPath: logPath}, slog.Default())
	if err != nil {
		t.Fatalf("NewAuditLogger: %v", err)
	}
	defer audit.Close()

	d := &Daemon{
		audit:    audit,
		sessions: NewSessionManager(10, time.Minute, 1, slog.Default()),
	}
	sess := d.sessions.Open(SessionClientInfo{PresenceAgentID: "claude-code"}, "")

	// Proxied agent call: no per-call AgentID, only the session lease.
	d.emitAudit(
		callParams{Method: "tools/call", SessionID: sess.ID},
		"git", "status", "local", time.Now(),
		"success", "", false, nil, "execute", 0, 0, auditTimings{},
	)

	entries := readAuditEntries(t, logPath)
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(entries))
	}
	if entries[0].AgentID != "claude-code" {
		t.Fatalf("audit agent_id = %q, want %q", entries[0].AgentID, "claude-code")
	}
}
