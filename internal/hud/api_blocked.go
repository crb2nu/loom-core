package hud

import (
	"net/http"

	"github.com/crb2nu/loom/internal/hud/domain/mobile"
)

// handleBlocked serves the sessions currently waiting on a human — a
// flightdeck-derived permission/idle stall — folded from the
// flightdeck-hud-bridge's agent.blocked / agent.unblocked daemon events into the
// blocked store (see IngestDaemonEvent + blockedStore). It is the
// desktop-readable counterpart of the mobile dashboard's "blocked" array
// (GET /api/mobile/v1/dashboard): the same data, served without a
// mobile-operator token, so the Loom VS Code extension's Flightdeck view can
// poll it directly.
//
// Response: {"blocked": [BlockedSessionInfo...], "count": N}, longest wait
// first. "blocked" is always an array (never null) so clients can iterate
// unconditionally. The mobile-operator token guard in withCORS still rejects
// that restricted token here (it is scoped to /api/mobile/v1); ordinary desktop
// clients with no token are served normally.
func (a *App) handleBlocked(w http.ResponseWriter, _ *http.Request) {
	blocked := a.BlockedSessions()
	if blocked == nil {
		blocked = []mobile.BlockedSessionInfo{}
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"blocked": blocked,
		"count":   len(blocked),
	})
}
