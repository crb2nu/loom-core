// schema_plan.go -- Plan entity: a first-class, worktree-resilient unit of
// planned work stored in the global agent-context Qdrant.
//
// Plans exist precisely because `.loom/*.md` files are git-tracked and therefore
// frozen per-worktree at checkout time: a plan written on `main` or in worktree
// A is invisible to a fresh agent in worktree B until committed AND merged. A
// Plan lives in the shared global store keyed by a stable plan_id, so any agent
// in any worktree/repo (Claude, Codex, or a Mills-spawned pod) retrieves the
// live record by id — never from a stale checkout.
//
// MVP (Slice 1): minimal fields + inline slices, non-semantic (zero) vector.
// The full schema, dedicated slice collection, semantic recall, lifecycle
// transitions, and the .md mirror arrive in Slice 2+.
package agentcontext

import (
	"encoding/json"
	"strings"
	"time"
)

// Plan lifecycle phases. A plan advances plan→implement→review→merge→deploy.
const (
	PlanPhaseDraft      = "draft"
	PlanPhasePlanned    = "planned"
	PlanPhaseInProgress = "in_progress"
	PlanPhaseInReview   = "in_review"
	PlanPhaseMerging    = "merging"
	PlanPhaseMerged     = "merged"
	PlanPhaseDeployed   = "deployed"
	PlanPhaseDone       = "done"
	PlanPhaseAbandoned  = "abandoned"
)

// Slice lifecycle phases (parallel-slice-ship work units).
const (
	SlicePhasePending      = "pending"
	SlicePhaseClaimed      = "claimed"
	SlicePhaseImplementing = "implementing"
	SlicePhaseImplemented  = "implemented"
	SlicePhaseInReview     = "in_review"
	SlicePhaseIntegrated   = "integrated"
	SlicePhaseMerged       = "merged"
)

// Plan is the authoritative record for a unit of planned work.
type Plan struct {
	ID            string      `json:"id"`
	Slug          string      `json:"slug"`
	Title         string      `json:"title"`
	Project       string      `json:"project"`
	Namespace     string      `json:"namespace,omitempty"`
	Phase         string      `json:"phase"`
	SpecDoc       string      `json:"spec_doc,omitempty"`
	Slices        []PlanSlice `json:"slices,omitempty"` // inline for MVP; own collection in Slice 2
	CreatedBy     string      `json:"created_by,omitempty"`
	SourceSession string      `json:"source_session_id,omitempty"`
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
}

// PlanSlice is an independently shippable slice of a Plan. A fresh
// slice-implementer looks itself up by ID rather than relying on the spawn
// prompt; its Files set is the basis for (Slice 4) claim enforcement.
type PlanSlice struct {
	ID    string   `json:"id"`
	Order int      `json:"order"`
	Name  string   `json:"name"`
	Goal  string   `json:"goal,omitempty"`
	Files []string `json:"files,omitempty"`
	Phase string   `json:"phase"`
}

// planSlug derives a stable, filesystem/id-safe kebab slug from a title.
func planSlug(title string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 60 {
		slug = strings.Trim(slug[:60], "-")
	}
	if slug == "" {
		slug = "plan"
	}
	return slug
}

// GeneratePlanID returns a stable, human-meaningful plan id of the form
// "plan-<slug>-<short>". The short suffix is deterministic in the inputs so the
// same (title, namespace, timestamp) always yields the same id.
func GeneratePlanID(title, namespace string, ts time.Time) string {
	short := GenerateID(namespace, "plan", title, ts)
	if len(short) > 6 {
		short = short[:6]
	}
	return "plan-" + planSlug(title) + "-" + short
}

// planToPayload flattens a Plan into a Qdrant payload. Indexed scalar fields
// (id, project, namespace, status, slug, created_by) live at the top level for
// keyword-filtered queries; slices round-trip via a JSON blob to avoid
// numeric/typing ambiguity when Qdrant rehydrates nested arrays.
func planToPayload(p *Plan) map[string]any {
	return map[string]any{
		"id":                p.ID,
		"slug":              p.Slug,
		"title":             p.Title,
		"project":           p.Project,
		"namespace":         p.Namespace,
		"status":            p.Phase, // indexed as "status" for consistency with other kinds
		"spec_doc":          p.SpecDoc,
		"slices_json":       planMarshalSlices(p.Slices),
		"created_by":        p.CreatedBy,
		"source_session_id": p.SourceSession,
		"created_at":        p.CreatedAt.Format(time.RFC3339),
		"updated_at":        p.UpdatedAt.Format(time.RFC3339),
		"_record_type":      "plan",
	}
}

// payloadToPlan rebuilds a Plan from a Qdrant payload. Returns nil if the
// payload is not a plan record.
func payloadToPlan(payload map[string]any) *Plan {
	if payload == nil {
		return nil
	}
	id := toString(payload["id"])
	if id == "" {
		return nil
	}
	p := &Plan{
		ID:            id,
		Slug:          toString(payload["slug"]),
		Title:         toString(payload["title"]),
		Project:       toString(payload["project"]),
		Namespace:     toString(payload["namespace"]),
		Phase:         toString(payload["status"]),
		SpecDoc:       toString(payload["spec_doc"]),
		CreatedBy:     toString(payload["created_by"]),
		SourceSession: toString(payload["source_session_id"]),
	}
	if raw := toString(payload["slices_json"]); raw != "" {
		_ = json.Unmarshal([]byte(raw), &p.Slices)
	}
	if t, err := time.Parse(time.RFC3339, toString(payload["created_at"])); err == nil {
		p.CreatedAt = t
	}
	if t, err := time.Parse(time.RFC3339, toString(payload["updated_at"])); err == nil {
		p.UpdatedAt = t
	}
	return p
}

// planMarshalSlices serializes slices to a JSON string for payload storage.
func planMarshalSlices(slices []PlanSlice) string {
	if len(slices) == 0 {
		return ""
	}
	b, err := json.Marshal(slices)
	if err != nil {
		return ""
	}
	return string(b)
}
