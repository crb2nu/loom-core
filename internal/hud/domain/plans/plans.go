package plans

import "net/http"

// PlansDomain exposes a read view of the agent-context Plan store.
type PlansDomain struct {
	deps Deps
}

// New creates a PlansDomain.
func New(deps Deps) *PlansDomain { return &PlansDomain{deps: deps} }

// Name returns "plans".
func (d *PlansDomain) Name() string { return "plans" }

// RegisterRoutes wires the plan lifecycle endpoints (read + management).
func (d *PlansDomain) RegisterRoutes(mux *http.ServeMux, mw func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/plans", mw(d.handlePlanList))
	mux.HandleFunc("GET /api/plans/{id}", mw(d.handlePlanGet))
	mux.HandleFunc("POST /api/plans", mw(d.handlePlanCreate))
	mux.HandleFunc("POST /api/plans/compose", mw(d.handlePlanCompose))
	mux.HandleFunc("POST /api/plans/{id}/advance", mw(d.handlePlanAdvance))
	mux.HandleFunc("POST /api/plans/{id}/priority", mw(d.handlePlanSetPriority))
}
