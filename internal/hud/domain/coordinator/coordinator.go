// Package coordinator implements the coordinator domain -- it exposes the
// on-demand LLM memory compression cycle over HTTP (consumed server-to-server
// by pkg/agentcontext) plus the coordinator Prometheus metrics handler.
// Summarization and workflow planning remain in-process concerns of
// internal/hud/coordinator.
package coordinator

import (
	"net/http"
)

// CoordinatorDomain registers the coordinator compression and metrics endpoints.
type CoordinatorDomain struct {
	deps Deps
}

// New creates a new CoordinatorDomain backed by the given Deps implementation.
func New(deps Deps) *CoordinatorDomain {
	return &CoordinatorDomain{deps: deps}
}

// Name returns "coordinator".
func (d *CoordinatorDomain) Name() string { return "coordinator" }

// RegisterRoutes wires the coordinator endpoints to the ServeMux.
func (d *CoordinatorDomain) RegisterRoutes(mux *http.ServeMux, mw func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("POST /api/coordinator/compress", mw(d.handleCoordinatorCompress))

	if m := d.deps.CoordinatorMetrics(); m != nil {
		mux.Handle("GET /api/coordinator/metrics", m.Handler())
	}
}
