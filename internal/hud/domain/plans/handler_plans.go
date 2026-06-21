package plans

import (
	"net/http"
	"strings"
)

// handlePlanList returns plans for the lifecycle view, filtered by optional
// project / namespace / phase query params. If the agent-context daemon predates
// the plan store (the agent_plan_* tools are unknown), it degrades to an empty
// list with available=false rather than erroring, so the HUD shows a clean
// "no plans yet / deploy pending" state.
func (d *PlansDomain) handlePlanList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	plans, err := d.deps.Agent().Plans(q.Get("project"), q.Get("namespace"), q.Get("phase"))
	if err != nil {
		if planStoreUnavailable(err) {
			d.deps.WriteJSON(w, http.StatusOK, map[string]any{
				"available": false,
				"reason":    "plan store not available on this daemon yet",
				"plans":     []any{},
				"count":     0,
			})
			return
		}
		d.deps.WriteError(w, http.StatusBadGateway, "list plans failed", err)
		return
	}
	d.deps.WriteJSON(w, http.StatusOK, map[string]any{
		"available": true,
		"plans":     plans,
		"count":     len(plans),
	})
}

// handlePlanGet returns a single plan (with slices) by id.
func (d *PlansDomain) handlePlanGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		d.deps.WriteError(w, http.StatusBadRequest, "missing plan id", nil)
		return
	}
	plan, err := d.deps.Agent().Plan(id)
	if err != nil {
		if planStoreUnavailable(err) {
			d.deps.WriteJSON(w, http.StatusOK, map[string]any{
				"available": false,
				"reason":    "plan store not available on this daemon yet",
			})
			return
		}
		d.deps.WriteError(w, http.StatusNotFound, "plan not found", err)
		return
	}
	d.deps.WriteJSON(w, http.StatusOK, map[string]any{"available": true, "plan": plan})
}

// planStoreUnavailable reports whether the error means the daemon doesn't expose
// the plan-store tools yet (vs a genuine failure). Matches the MCP "unknown
// tool" / "method not found" shapes so an undeployed daemon degrades cleanly.
func planStoreUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unknown tool") ||
		strings.Contains(msg, "tool not found") ||
		strings.Contains(msg, "method not found") ||
		strings.Contains(msg, "not found: agent_plan") ||
		strings.Contains(msg, "no such tool")
}
