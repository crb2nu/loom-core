// api_patterns.go exposes the Pattern catalog front door to the HUD.
//
// Two endpoints power the Pattern Loom "stamp a pattern" surface (Slice B1):
//
//   - GET  /api/patterns?status=approved  — list the catalog (optionally
//     filtered by approval status) so the front-door page can offer patterns.
//   - POST /api/patterns/stamp            — stamp a pattern with materials,
//     expanding it into a Plan and returning plan_id + required tools.
//
// Both mirror the engram summary endpoint's bridge-required contract: when the
// agent bridge is unavailable (e.g. tests with a minimal App) the list endpoint
// returns an empty catalog instead of erroring, so the page can render a
// "no patterns yet" state. The stamp endpoint requires the bridge.
package hud

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/crb2nu/loom/internal/hud/bridge"
	domainmills "github.com/crb2nu/loom/internal/hud/domain/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// handlePatternInstances returns the stamp history for one pattern.
// A missing bridge returns {"instances":[],"degraded":true}; bridge failures
// return 502, preserving the distinction from a real empty history.
func (a *App) handlePatternInstances(w http.ResponseWriter, r *http.Request) {
	if a.agent == nil {
		a.writeJSON(w, http.StatusOK, map[string]any{"instances": []bridge.PatternInstanceInfo{}, "degraded": true})
		return
	}
	instances, err := a.agent.PatternInstances(r.PathValue("id"))
	if err != nil {
		a.writeError(w, http.StatusBadGateway, "pattern instances", err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"instances": instances, "degraded": false})
}

// handlePatternList returns the Pattern catalog, optionally filtered by the
// ?status= query (candidate|approved|deprecated).
//
// Response shape: {"patterns": [...], "count": N}.
//
// When the bridge is nil the endpoint returns an empty catalog (200) so the
// front-door page can render a "no daemon yet" state without a 500.
func (a *App) handlePatternList(w http.ResponseWriter, r *http.Request) {
	if a.agent == nil {
		a.writeJSON(w, http.StatusOK, map[string]any{"patterns": []any{}, "count": 0})
		return
	}

	status := strings.TrimSpace(r.URL.Query().Get("status"))
	patterns, err := a.agent.PatternList(status)
	if err != nil {
		a.writeError(w, http.StatusBadGateway, "list patterns", err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"patterns": patterns,
		"count":    len(patterns),
	})
}

// handlePatternStamp stamps a Pattern with Materials, expanding it into a Plan,
// and OPTIONALLY projects that Plan into a queued Mills BacklogItem (the S1
// Mills e2e seam) when the request sets "enqueue": true.
//
// Request body: {"pattern_id": str, "materials": {...}, "project": str,
// "enqueue": bool}.
// Response: the stamp result (plan_id, slice_count, tools_required, slices);
// when enqueued, it additionally carries enqueued/backlog_id/backlog_state.
//
// pattern_id and a non-empty materials object are required. Unlike the list
// endpoint, stamping requires a live bridge — there is no empty fallback.
// Enqueuing kicks off the autonomous Mills loop, so it is gated behind the HUD
// admin token (the bare stamp, which only writes a Plan, is not).
func (a *App) handlePatternStamp(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PatternID string         `json:"pattern_id"`
		Materials map[string]any `json:"materials"`
		Project   string         `json:"project"`
		Enqueue   bool           `json:"enqueue"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeError(w, http.StatusBadRequest, "invalid request body", err)
		return
	}
	body.PatternID = strings.TrimSpace(body.PatternID)
	if body.PatternID == "" {
		a.writeError(w, http.StatusBadRequest, "pattern_id is required", nil)
		return
	}
	if len(body.Materials) == 0 {
		a.writeError(w, http.StatusBadRequest, "materials object is required", nil)
		return
	}
	// Gate the privileged enqueue path BEFORE stamping, so an unauthorized
	// caller can't even leave an orphan Plan behind.
	if body.Enqueue && !a.RequireAdminToken(w, r) {
		return
	}
	if a.agent == nil {
		a.writeError(w, http.StatusServiceUnavailable, "agent bridge unavailable", nil)
		return
	}

	result, err := a.agent.PatternStamp(body.PatternID, body.Materials, strings.TrimSpace(body.Project))
	if err != nil {
		a.writeError(w, http.StatusBadGateway, "stamp pattern", err)
		return
	}
	a.broadcastAgentEvent("hud.pattern.stamp", map[string]any{
		"pattern_id": body.PatternID,
		"plan_id":    result.PlanID,
	})

	if !body.Enqueue {
		a.writeJSON(w, http.StatusOK, result)
		return
	}

	// --- Projection: stamp -> queued Mills BacklogItem (S1 e2e seam) ---
	if strings.TrimSpace(a.config.MillsOperatorURL) == "" {
		a.writeError(w, http.StatusServiceUnavailable,
			"mills operator not configured; cannot enqueue (the Plan was still created)", nil)
		return
	}
	item := buildStampBacklogItem(result)
	cfg := domainmills.Config{BaseURL: a.config.MillsOperatorURL, AdminToken: a.config.MillsOperatorToken}
	created, err := domainmills.EnqueueBacklogItem(r.Context(), cfg, item, false)
	if err != nil {
		// The Plan exists; only the enqueue failed. Preserve plan_id so the
		// caller can retry the enqueue (or run the plan another way).
		a.writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok":            false,
			"plan_id":       result.PlanID,
			"pattern_id":    result.PatternID,
			"enqueued":      false,
			"enqueue_error": err.Error(),
		})
		return
	}
	a.broadcastAgentEvent("hud.pattern.stamp.enqueued", map[string]any{
		"pattern_id": body.PatternID,
		"plan_id":    result.PlanID,
		"backlog_id": created.ID,
	})
	resp := toFlatMap(result)
	resp["enqueued"] = true
	resp["backlog_id"] = created.ID
	resp["backlog_state"] = string(created.State)
	a.writeJSON(w, http.StatusOK, resp)
}

// buildStampBacklogItem projects a stamp result into a queued Mills BacklogItem.
// PlanID carries the canonical link (the Mills agent resolves the live plan via
// agent_plan_get), but Slices MUST also be materialized here: the operator's
// post-implement scope gate reads item.Slices directly and fails closed on a
// slice-less item ("backlog item has no slices; no scope to enforce") — the
// live 2026-07-01 widget stamp escalated exactly this way. Every other intake
// path (council mutator, importer, plan-slice emitter) materializes slices at
// creation time; the stamp projection follows the same convention. The id is
// derived from the plan id so re-stamping identical materials is idempotent
// (the operator upserts by id). The mills-pattern-stamp label marks the item's
// provenance for the operator and the HUD backlog view.
func buildStampBacklogItem(result *bridge.PatternStampResult) store.BacklogItem {
	id := "pattern-stamp-" + strings.TrimPrefix(result.PlanID, "plan-stamp-")
	if id == "pattern-stamp-" {
		id = "pattern-stamp-" + result.PatternID
	}
	title := "Pattern stamp: " + result.PatternID
	if sn := firstMaterialString(result.Materials, "service_name", "tool_name"); sn != "" {
		title += " — " + sn
	}
	return store.BacklogItem{
		ID:        id,
		Title:     title,
		PlanID:    result.PlanID,
		Slices:    stampSlices(result.Slices),
		Labels:    []string{"mills-pattern-stamp"},
		CreatedBy: "pattern-stamp",
	}
}

// stampSlices converts the stamp result's expanded slices (name/goal/files
// maps) into the store's Slice form so the scope gate has a file allowlist to
// enforce. Slices without files are kept (by name) — the gate unions files
// across slices, and an all-empty union still fails closed as before.
func stampSlices(raw []map[string]any) []store.Slice {
	slices := make([]store.Slice, 0, len(raw))
	for _, m := range raw {
		s := store.Slice{Name: materialString(m, "name")}
		if files, ok := m["files"].([]any); ok {
			for _, f := range files {
				if fs, ok := f.(string); ok && strings.TrimSpace(fs) != "" {
					s.Files = append(s.Files, fs)
				}
			}
		}
		slices = append(slices, s)
	}
	return slices
}

// firstMaterialString returns the first non-empty string material among keys.
// Patterns name their primary material by what they make (service_name for
// services, tool_name for CLIs).
func firstMaterialString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := materialString(m, k); s != "" {
			return s
		}
	}
	return ""
}

// materialString returns a string material value, or "" if absent / non-string.
func materialString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// toFlatMap round-trips a value through JSON into a map so enqueue metadata can
// be added alongside the stamp result without dropping any of its fields.
func toFlatMap(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}
	}
	return m
}
