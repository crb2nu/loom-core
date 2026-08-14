package plans

import (
	"encoding/json"
	"fmt"
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
		Priority  string `json:"priority"`
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
	body.Priority = strings.ToUpper(strings.TrimSpace(body.Priority))
	if !validPlanPriority(body.Priority) {
		d.deps.WriteError(w, http.StatusBadRequest, "priority must be P0..P3 or empty", nil)
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
		Priority:  body.Priority,
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

// composeSliceInput is one cherry-picked slice from a competing draft. `source`
// (an optional frame/plan label) is provenance for the spec_doc, not passed to
// the store.
type composeSliceInput struct {
	Name   string   `json:"name"`
	Goal   string   `json:"goal"`
	Files  []string `json:"files"`
	Source string   `json:"source"`
}

// handlePlanCompose authors ONE new draft plan from slices cherry-picked in the
// compare/merge editor. It mirrors handlePlanCreate (plan mutations are open,
// not admin-gated) but requires >=2 source plan ids: composing is only
// meaningful across competing drafts. The two (or more) source drafts are left
// untouched as provenance — the operator advances the merged plan and can then
// abandon the losers. The spec_doc opens with a provenance note naming the
// sources so a reviewer sees where the merged plan came from.
func (d *PlansDomain) handlePlanCompose(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title        string              `json:"title"`
		SourcePlanID []string            `json:"source_plan_ids"`
		Slices       []composeSliceInput `json:"slices"`
		Project      string              `json:"project"`
		Namespace    string              `json:"namespace"`
		Priority     string              `json:"priority"`
		AgentID      string              `json:"agent_id"`
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

	// Composing is a cross-draft merge: it needs at least two sources to be
	// meaningful (a single source is a plain create/respin).
	sources := make([]string, 0, len(body.SourcePlanID))
	for _, id := range body.SourcePlanID {
		if s := strings.TrimSpace(id); s != "" {
			sources = append(sources, s)
		}
	}
	if len(sources) < 2 {
		d.deps.WriteError(w, http.StatusBadRequest, "compose needs at least two source_plan_ids", nil)
		return
	}

	// Build the store slices + collect names for the provenance spec_doc.
	slices := make([]bridge.PlanSliceInput, 0, len(body.Slices))
	for _, s := range body.Slices {
		name := strings.TrimSpace(s.Name)
		if name == "" {
			continue
		}
		slices = append(slices, bridge.PlanSliceInput{
			Name:  name,
			Goal:  strings.TrimSpace(s.Goal),
			Files: s.Files,
		})
	}
	if len(slices) == 0 {
		d.deps.WriteError(w, http.StatusBadRequest, "compose needs at least one named slice", nil)
		return
	}

	body.Priority = strings.ToUpper(strings.TrimSpace(body.Priority))
	if !validPlanPriority(body.Priority) {
		d.deps.WriteError(w, http.StatusBadRequest, "priority must be P0..P3 or empty", nil)
		return
	}

	agentID := strings.TrimSpace(body.AgentID)
	if agentID == "" {
		agentID = "hud:plan-merge-editor"
	}

	res, err := d.deps.Agent().PlanCreate(bridge.PlanCreateParams{
		Title:     body.Title,
		Project:   strings.TrimSpace(body.Project),
		Namespace: strings.TrimSpace(body.Namespace),
		Phase:     "draft",
		Priority:  body.Priority,
		SpecDoc:   composeSpecDoc(body.Title, sources, body.Slices),
		AgentID:   agentID,
		Slices:    slices,
	})
	if err != nil {
		if planStoreUnavailable(err) {
			d.deps.WriteError(w, http.StatusServiceUnavailable, "plan store not available on this daemon yet", err)
			return
		}
		d.deps.WriteError(w, http.StatusBadGateway, "compose plan failed", err)
		return
	}
	d.deps.WriteJSON(w, http.StatusCreated, map[string]any{
		"status":          "composed",
		"plan_id":         res.PlanID,
		"source_plan_ids": sources,
		"slice_count":     len(slices),
	})
}

// composeSpecDoc renders the merged plan's spec_doc: a provenance note naming
// the source drafts, then the merged slice list (with each slice's source frame
// when known) so a reviewer can trace every slice back to the draft it came
// from.
func composeSpecDoc(title string, sources []string, slices []composeSliceInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", title)
	fmt.Fprintf(&b, "Composed in the plan compare/merge editor from drafts: %s.\n\n", strings.Join(sources, ", "))
	b.WriteString("_Draft — review before advancing. The source drafts are kept as provenance; abandon them once this is advanced._\n\n")
	b.WriteString("## Merged slices\n\n")
	for _, s := range slices {
		name := strings.TrimSpace(s.Name)
		if name == "" {
			continue
		}
		if src := strings.TrimSpace(s.Source); src != "" {
			fmt.Fprintf(&b, "- **%s** _(from %s)_", name, src)
		} else {
			fmt.Fprintf(&b, "- **%s**", name)
		}
		if goal := strings.TrimSpace(s.Goal); goal != "" {
			fmt.Fprintf(&b, " — %s", goal)
		}
		b.WriteString("\n")
		if len(s.Files) > 0 {
			fmt.Fprintf(&b, "  - files: %s\n", strings.Join(s.Files, ", "))
		}
	}
	return b.String()
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

// handlePlanSetPriority sets or clears a plan's warp-beam priority bucket.
// Management action: this is the HUD's reorder knob — the plan-slice emitter
// resyncs still-queued Mills backlog items to the new bucket on its next tick,
// so a change here re-orders autonomous dispatch without re-emitting.
func (d *PlansDomain) handlePlanSetPriority(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		d.deps.WriteError(w, http.StatusBadRequest, "missing plan id", nil)
		return
	}
	var body struct {
		Priority string `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		d.deps.WriteError(w, http.StatusBadRequest, "invalid request body", err)
		return
	}
	body.Priority = strings.ToUpper(strings.TrimSpace(body.Priority))
	if !validPlanPriority(body.Priority) {
		d.deps.WriteError(w, http.StatusBadRequest, "priority must be P0..P3 or empty (empty clears)", nil)
		return
	}
	res, err := d.deps.Agent().PlanSetPriority(id, body.Priority)
	if err != nil {
		if planStoreUnavailable(err) {
			d.deps.WriteError(w, http.StatusServiceUnavailable, "plan store not available on this daemon yet", err)
			return
		}
		d.deps.WriteError(w, http.StatusUnprocessableEntity, "set priority failed", err)
		return
	}
	d.deps.WriteJSON(w, http.StatusOK, map[string]any{
		"status":   "priority_set",
		"plan_id":  res.PlanID,
		"priority": body.Priority,
	})
}

// validPlanPriority reports whether p is a settable priority: a bucket P0..P3
// or the empty string (clear).
func validPlanPriority(p string) bool {
	switch p {
	case "", "P0", "P1", "P2", "P3":
		return true
	}
	return false
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
