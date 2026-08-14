package vendorsessions

import (
	"net/http"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

// Deps is the slice of *hud.App the vendor-sessions domain needs.
// Decouples the domain from hud.App for testability.
type Deps interface {
	WriteJSON(w http.ResponseWriter, status int, v any)
	WriteError(w http.ResponseWriter, status int, msg string, err error)

	// VendorSessions returns the bridge-backed transcript reader, or nil
	// when the agent bridge is unavailable (minimal test App). Handlers
	// must degrade to an empty payload with "degraded": true rather than
	// erroring, matching the engram-summary precedent.
	VendorSessions() Ops
}

// Ops is the slice of bridge.AgentBridge the domain consumes.
type Ops interface {
	VendorSessionList(p bridge.VendorSessionListParams) ([]bridge.VendorSessionInfo, error)
	VendorSessionSearch(p bridge.VendorSessionSearchParams) ([]bridge.VendorSessionMatch, error)
	VendorSessionTail(vendor, id string, maxLines int) (*bridge.VendorSessionTailResult, error)
}
