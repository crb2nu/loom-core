// Package vendorsessions implements the vendor-sessions domain — a
// read-only HUD surface over the cross-vendor session bridge
// (pkg/vendorsessions via the agent-context tools agent_vendor_session_list
// / agent_vendor_session_search). It lets the Operator Deck and the iOS
// companion list and search the on-disk transcripts of the vendor CLIs
// (Claude Code, Codex) running on the workstation, which the fleet's
// presence/session views cannot see.
//
// The transcripts live on the host running the agent-context MCP server, so
// the daemon's agent_context routing must prefer the local server
// (`agent_context: prefer-local`) — a hub-delegated agent-context pod has
// empty vendor session roots and returns empty lists.
//
// Hosts that aren't this one federate their transcripts here instead: the
// HUD mirror (internal/hud/mirror, vendorsync.go) POSTs bounded transcript
// snapshots to /api/vendor-sessions/mirror, and list/search merge that
// per-host store with the local bridge's results, tagging federated rows
// with `host`. This is what lets the cluster HUD (and the phone paired to
// it) browse a workstation's claude/codex sessions.
package vendorsessions

import (
	"net/http"
)

// Domain registers the /api/vendor-sessions endpoints.
type Domain struct {
	deps   Deps
	mirror *MirrorStore
}

// New creates a new vendor-sessions Domain.
func New(deps Deps) *Domain {
	return &Domain{deps: deps, mirror: NewMirrorStore()}
}

// Name returns "vendorsessions".
func (d *Domain) Name() string { return "vendorsessions" }

// RegisterRoutes mounts the transcript list/search endpoints and the
// federation ingest. The ingest rides the same middleware tier as
// POST /api/agent/heartbeat — the mirror authenticates the same way for
// both (LAN-open; CF Access service token at the edge off-LAN).
func (d *Domain) RegisterRoutes(mux *http.ServeMux, mw func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/vendor-sessions", mw(d.handleList))
	mux.HandleFunc("GET /api/vendor-sessions/search", mw(d.handleSearch))
	mux.HandleFunc("GET /api/vendor-sessions/tail", mw(d.handleTail))
	mux.HandleFunc("POST /api/vendor-sessions/mirror", mw(d.handleMirrorIngest))
}
