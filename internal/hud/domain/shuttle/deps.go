package shuttle

import (
	"net/http"

	orch "github.com/crb2nu/loom/internal/hud/shuttle"
)

// Deps defines the dependencies the shuttle domain needs from the host App.
type Deps interface {
	WriteJSON(w http.ResponseWriter, status int, v any)
	WriteError(w http.ResponseWriter, status int, msg string, err error)
	ShuttleMonitor() *orch.ShuttleMonitor
}
