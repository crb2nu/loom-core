// api_agent.go provides shared utilities for the agent lifecycle REST API.
//
// Domain handlers are split across:
//   - api_agent_session.go — Session lifecycle + heartbeat
//   - api_agent_task.go    — Task update + workflow management
//   - api_agent_context.go — Context add/inspect + knowledge recall
//   - api_agent_nudge.go   — Nudge queue + sandbox policy helpers
//
// All routes are registered in app.go under /api/agent/*.
package hud

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

// mustMarshal marshals v to JSON, returning nil on error.
func mustMarshal(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return data
}

// broadcastAgentEvent sends a granular agent SSE event to all browser clients
// and appends it to the timeline event log.
func (a *App) broadcastAgentEvent(eventType string, payload any) {
	data := mustMarshal(payload)
	if data == nil {
		return
	}

	now := time.Now()

	// Append to timeline event log for the unified activity feed.
	if a.eventLog != nil {
		agentID, _ := extractStringField(payload, "agent_id")
		agentType, _ := extractStringField(payload, "agent_type")
		a.eventLog.Append(TimelineEntry{
			Timestamp: now,
			EventType: eventType,
			AgentID:   agentID,
			AgentType: agentType,
			Data:      data,
		})
	}

	// Broadcast via SSE.
	if a.sseHub == nil {
		return
	}
	a.sseHub.Broadcast(bridge.SSEEvent{
		ID:        fmt.Sprintf("%s-%d", eventType, now.UnixMilli()),
		Type:      eventType,
		Timestamp: now,
		Data:      data,
	})
}

// extractStringField safely extracts a string field from a payload (map or struct).
func extractStringField(payload any, key string) (string, bool) {
	if m, ok := payload.(map[string]any); ok {
		if v, ok := m[key].(string); ok {
			return v, true
		}
	}
	return "", false
}

// requireAdminToken authorizes an admin request via EITHER a verified
// Cloudflare Access SSO identity (an allowlisted email — no token needed) OR a
// matching admin token. It writes the appropriate error and returns false when
// neither authorizes.
func (a *App) requireAdminToken(w http.ResponseWriter, r *http.Request) bool {
	if _, _, ok := a.adminIdentity(r); ok {
		return true
	}
	// Not authorized. Preserve the original "not configured" hint only when NO
	// admin path is configured at all (no token, no Access SSO, no trusted net).
	if strings.TrimSpace(a.AdminToken()) == "" && !a.accessVerifier.Enabled() && len(a.adminTrustedNets) == 0 {
		a.writeError(w, http.StatusForbidden, "admin token is not configured; set HUD_ADMIN_TOKEN", nil)
		return false
	}
	a.writeError(w, http.StatusUnauthorized, "invalid admin token", nil)
	return false
}

// adminIdentity reports the authenticated admin identity for a request, if any:
// via="access" with the verified email for a Cloudflare Access SSO admin, or
// via="token" for a valid admin token. ok=false when neither authorizes. It is
// read-only (never writes to w) so both the gate and /api/labs/auth-check can
// use it. Access SSO is checked first so an SSO admin never needs a token.
func (a *App) adminIdentity(r *http.Request) (email, via string, ok bool) {
	if e, isAccess := a.accessVerifier.AdminEmail(r); isAccess {
		return e, "access", true
	}
	if a.trustedNetworkAdmin(r) {
		return "", "network", true
	}
	expected := strings.TrimSpace(a.AdminToken())
	if expected != "" && subtle.ConstantTimeCompare([]byte(expected), []byte(extractAdminToken(r))) == 1 {
		return "", "token", true
	}
	return "", "", false
}

func extractAdminToken(r *http.Request) string {
	if r == nil {
		return ""
	}
	if token := strings.TrimSpace(r.Header.Get("X-Admin-Token")); token != "" {
		return token
	}
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if authHeader == "" {
		return ""
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}
