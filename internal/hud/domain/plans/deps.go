// Package plans implements the plan-store lifecycle domain — a read view of the
// agent-context Plan store so the HUD can show each plan/slice across
// plan→implement→review→merge→deploy.
package plans

import (
	"log/slog"
	"net/http"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

// Deps is what the plans domain needs from the host App.
type Deps interface {
	WriteJSON(w http.ResponseWriter, status int, v any)
	WriteError(w http.ResponseWriter, status int, msg string, err error)
	Logger() *slog.Logger
	Agent() *bridge.AgentBridge
}
