package main

// handlers_bootstrap.go -- plan→repo bootstrap endpoints.
//
// POST /api/mills/projects/bootstrap (admin-gated) mints a new GitLab project
// from a Spinning Room plan: creates the repo with the operator's group
// token, seeds an initial commit, records the project in the store registry
// (the emitter's dynamic demand source), and re-scopes the plan onto the new
// path. GET /api/mills/projects/bootstrapped is the open read of the
// registry, mirroring the other list endpoints.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/bootstrap"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// bootstrapRequestBudget bounds one mint end-to-end (namespace lookup +
// project create + seed commit + plan re-scope). Generous for four REST
// calls but small enough that a wedged GitLab can't pin the handler.
const bootstrapRequestBudget = 60 * time.Second

// projectBootstrapRequest is the admin POST body.
type projectBootstrapRequest struct {
	PlanID      string `json:"plan_id"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
	Visibility  string `json:"visibility,omitempty"`
}

// handleProjectBootstrap mints a repo from a plan. 503 when the wiring or the
// policy keys are missing (two-key: cross_repo.enabled AND
// cross_repo.allow_bootstrapped), 400/409 for caller errors, 502 when GitLab
// or the Plan Store fails mid-flight.
func (o *operator) handleProjectBootstrap(w http.ResponseWriter, r *http.Request) {
	if o.bootstrapper == nil {
		http.Error(w, "project bootstrap not configured (GitLab client + MCP hub required)", http.StatusServiceUnavailable)
		return
	}
	if !o.policy.Current().CrossRepoBootstrapEnabled() {
		http.Error(w, "project bootstrap disabled in policy (cross_repo.enabled + cross_repo.allow_bootstrapped)", http.StatusServiceUnavailable)
		return
	}
	var req projectBootstrapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), bootstrapRequestBudget)
	defer cancel()
	res, err := o.bootstrapper.Bootstrap(ctx, bootstrap.Request{
		PlanID:      strings.TrimSpace(req.PlanID),
		Path:        strings.TrimSpace(req.Path),
		Description: req.Description,
		Visibility:  req.Visibility,
	})
	if err != nil {
		writeBootstrapError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// handleBootstrappedList returns the runtime-minted project registry. Open
// read; an unconfigured store yields an empty list, not an error.
func (o *operator) handleBootstrappedList(w http.ResponseWriter, r *http.Request) {
	rows, err := o.store.Bootstrap.List(r.Context())
	if err != nil {
		http.Error(w, "list bootstrapped projects: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []*store.BootstrappedProject{} // render [] not null
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"projects": rows,
		"count":    len(rows),
		// enabled reflects whether a mint could run right now: wiring + both
		// policy keys. The HUD uses it to grey out the bootstrap action.
		"enabled": o.bootstrapper != nil && o.policy.Current().CrossRepoBootstrapEnabled(),
	})
}

// writeBootstrapError maps the bootstrap service's typed errors onto HTTP
// status codes.
func writeBootstrapError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, bootstrap.ErrInvalidRequest):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, bootstrap.ErrGroupNotAllowed):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, bootstrap.ErrPlanNotBootstrappable),
		errors.Is(err, bootstrap.ErrAlreadyBootstrapped):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusBadGateway)
	}
}
