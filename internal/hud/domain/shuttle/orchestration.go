// Package shuttle implements the shuttle domain -- it exposes the shuttle
// monitor snapshot (capacities, dispatch recommendations, pending tasks) over
// HTTP. Dispatch evaluation, policy management, and conflict preflight remain
// in-process concerns of internal/hud/shuttle.
package shuttle

import (
	"net/http"
)

// ShuttleDomain registers the shuttle status route.
type ShuttleDomain struct {
	deps Deps
}

// New creates a new ShuttleDomain backed by the given Deps interface.
func New(deps Deps) *ShuttleDomain {
	return &ShuttleDomain{deps: deps}
}

// Name returns "shuttle".
func (d *ShuttleDomain) Name() string { return "shuttle" }

// RegisterRoutes wires the shuttle endpoints to the ServeMux.
func (d *ShuttleDomain) RegisterRoutes(mux *http.ServeMux, mw func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/shuttle/status", mw(d.handleStatus))
}
