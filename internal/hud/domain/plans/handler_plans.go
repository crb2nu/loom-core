package plans

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/crb2nu/loom/internal/hud/bridge"
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

// handlePlanCreate creates a plan from the HUD. Management action.
func (d *PlansDomain) handlePlanCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title     string `json:"title"`
		Project   string `json:"project"`
		Namespace string `json:"namespace"`
		Phase     string `json:"phase"`
		SpecDoc   string `json:"spec_doc"`
		AgentID   string `json:"agent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		d.deps.WriteError(w, http.StatusBadRequest, "invalid request body", err)
		return
	}
	body.Title = strings.TrimSpace(body.Title)
	if body.Title == "" {
		d.deps.WriteError(w, http.StatusBadRequest, "title is required", nil)
		return
	}
	if body.AgentID == "" {
		body.AgentID = "hud-user"
	}
	res, err := d.deps.Agent().PlanCreate(bridge.PlanCreateParams{
		Title:     body.Title,
		Project:   strings.TrimSpace(body.Project),
		Namespace: strings.TrimSpace(body.Namespace),
		Phase:     strings.TrimSpace(body.Phase),
		SpecDoc:   body.SpecDoc,
		AgentID:   body.AgentID,
	})
	if err != nil {
		if planStoreUnavailable(err) {
			d.deps.WriteError(w, http.StatusServiceUnavailable, "plan store not available on this daemon yet", err)
			return
		}
		d.deps.WriteError(w, http.StatusBadGateway, "create plan failed", err)
		return
	}
	d.deps.WriteJSON(w, http.StatusCreated, map[string]any{"status": "created", "plan_id": res.PlanID, "phase": res.Phase})
}

// handlePlanAdvance transitions a plan to a new lifecycle phase. Management action.
func (d *PlansDomain) handlePlanAdvance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		d.deps.WriteError(w, http.StatusBadRequest, "missing plan id", nil)
		return
	}
	var body struct {
		ToPhase string `json:"to_phase"`
		AgentID string `json:"agent_id"`
		Note    string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		d.deps.WriteError(w, http.StatusBadRequest, "invalid request body", err)
		return
	}
	body.ToPhase = strings.TrimSpace(body.ToPhase)
	if body.ToPhase == "" {
		d.deps.WriteError(w, http.StatusBadRequest, "to_phase is required", nil)
		return
	}
	if body.AgentID == "" {
		body.AgentID = "hud-user"
	}
	res, err := d.deps.Agent().PlanAdvance(id, body.ToPhase, body.AgentID, strings.TrimSpace(body.Note))
	if err != nil {
		if planStoreUnavailable(err) {
			d.deps.WriteError(w, http.StatusServiceUnavailable, "plan store not available on this daemon yet", err)
			return
		}
		// Illegal transitions / not-found come back as tool errors → 422.
		d.deps.WriteError(w, http.StatusUnprocessableEntity, "advance failed", err)
		return
	}
	d.deps.WriteJSON(w, http.StatusOK, map[string]any{
		"status":     "advanced",
		"plan_id":    res.PlanID,
		"from_phase": res.FromPhase,
		"to_phase":   res.ToPhase,
	})
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
