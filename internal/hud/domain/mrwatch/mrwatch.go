// Package mrwatch (HUD domain) exposes the branch→MR status registry over the
// HUD REST surface. It is the read-only view layer for the registry maintained
// by internal/hud/mrwatch; slice M1 registers two endpoints:
//
//	GET /api/agent/mr-status?branch=<b>[&repo=<p>]  — MRs for one branch.
//	GET /api/mrwatch/summary                        — full snapshot + counts.
//	GET /api/mrwatch/actions                         — shepherd (M4) audit log.
package mrwatch

import (
	"log/slog"
	"net/http"

	registry "github.com/crb2nu/loom/internal/hud/mrwatch"
)

// Deps is what the domain needs from the host App: JSON writers, a logger, and
// access to the current registry snapshot.
type Deps interface {
	WriteJSON(w http.ResponseWriter, status int, v any)
	WriteError(w http.ResponseWriter, status int, msg string, err error)
	Logger() *slog.Logger
	// MRWatchSnapshot returns the current registry snapshot. It must return a
	// zero-value-but-non-nil snapshot (arrays as [], counts as {}) when the
	// poller is disabled so the endpoints always emit valid JSON.
	MRWatchSnapshot() registry.Snapshot
	// MRWatchActions returns the shepherd's bounded audit log (newest last).
	// It must return a non-nil slice ([] when empty / shepherd disabled) so the
	// endpoint always emits a JSON array, never null.
	MRWatchActions() []registry.ActionRecord
	// MRWatchShepherdEnabled reports whether the independent auto-action loop
	// can take actions. An empty audit log alone is not evidence it is enabled.
	MRWatchShepherdEnabled() bool
}

// Domain registers the mrwatch REST endpoints.
type Domain struct {
	deps Deps
}

// New creates a Domain backed by deps.
func New(deps Deps) *Domain { return &Domain{deps: deps} }

// Name returns "mrwatch".
func (d *Domain) Name() string { return "mrwatch" }

// RegisterRoutes wires the mrwatch endpoints onto the mux.
func (d *Domain) RegisterRoutes(mux *http.ServeMux, mw func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/agent/mr-status", mw(d.handleBranchStatus))
	mux.HandleFunc("GET /api/mrwatch/summary", mw(d.handleSummary))
	mux.HandleFunc("GET /api/mrwatch/actions", mw(d.handleActions))
}
