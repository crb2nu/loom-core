// schema_plan.go -- Plan + PlanSlice entities: first-class, worktree-resilient
// units of planned work stored in the global agent-context Qdrant.
//
// Plans exist precisely because `.loom/*.md` files are git-tracked and therefore
// frozen per-worktree at checkout time: a plan written on `main` or in worktree
// A is invisible to a fresh agent in worktree B until committed AND merged. A
// Plan lives in the shared global store keyed by a stable plan_id, so any agent
// in any worktree/repo (Claude, Codex, or a Mills-spawned pod) retrieves the
// live record by id — never from a stale checkout.
//
// Slice 2: full schema, slices promoted to their own collection (so parallel
// slice-implementers update them independently), validated lifecycle
// transitions with history, and best-effort semantic recall.
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

// planPhaseTransitions is the allowed plan phase DAG. abandoned is reachable
// from any non-terminal phase. Done/abandoned are terminal.
var planPhaseTransitions = map[string][]string{
	PlanPhaseDraft:      {PlanPhasePlanned, PlanPhaseInProgress, PlanPhaseAbandoned},
	PlanPhasePlanned:    {PlanPhaseInProgress, PlanPhaseAbandoned},
	PlanPhaseInProgress: {PlanPhaseInReview, PlanPhaseAbandoned},
	PlanPhaseInReview:   {PlanPhaseInProgress, PlanPhaseMerging, PlanPhaseAbandoned},
	PlanPhaseMerging:    {PlanPhaseMerged, PlanPhaseInReview, PlanPhaseAbandoned},
	PlanPhaseMerged:     {PlanPhaseDeployed, PlanPhaseDone, PlanPhaseAbandoned},
	PlanPhaseDeployed:   {PlanPhaseDone, PlanPhaseAbandoned},
	PlanPhaseDone:       {},
	PlanPhaseAbandoned:  {},
}

// planPhaseValid reports whether p is a known plan phase.
func planPhaseValid(p string) bool {
	_, ok := planPhaseTransitions[p]
	return ok
}

// planPhaseCanTransition reports whether from→to is an allowed plan transition.
// A no-op (from==to) is always allowed.
func planPhaseCanTransition(from, to string) bool {
	if from == to {
		return planPhaseValid(from)
	}
	for _, t := range planPhaseTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// PhaseTransition records one lifecycle hop for audit/HUD rendering.
type PhaseTransition struct {
	From  string    `json:"from"`
	To    string    `json:"to"`
	At    time.Time `json:"at"`
	Actor string    `json:"actor,omitempty"`
	Note  string    `json:"note,omitempty"`
}

// SuccessCriteria mirrors the Mills BacklogItem success contract so the two
// converge in Slice 7.
type SuccessCriteria struct {
	Tests       []string `json:"tests,omitempty"`
	Metrics     []string `json:"metrics,omitempty"`
	ManualCheck string   `json:"manual_check,omitempty"`
}

// Plan is the authoritative record for a unit of planned work.
type Plan struct {
	ID            string `json:"id"`
	Slug          string `json:"slug"`
	Title         string `json:"title"`
	Project       string `json:"project"`
	Namespace     string `json:"namespace,omitempty"`
	Phase         string `json:"phase"`
	SpecDoc       string `json:"spec_doc,omitempty"`
	SpecAnchor    string `json:"spec_anchor,omitempty"`
	CreatedBy     string `json:"created_by,omitempty"`
	SourceSession string `json:"source_session_id,omitempty"`

	// Planning contract (aligns with Mills BacklogItem for Slice 7).
	Success            *SuccessCriteria `json:"success,omitempty"`
	Budget             string           `json:"budget,omitempty"`
	RiskiestAssumption string           `json:"riskiest_assumption,omitempty"`
	KillTest           string           `json:"kill_test,omitempty"`
	KillTestStatus     string           `json:"kill_test_status,omitempty"`
	Dependencies       []string         `json:"dependencies,omitempty"` // other plan_ids

	// Lifecycle pointers (plan→merge→deploy review).
	MRRefs       []string `json:"mr_refs,omitempty"`
	PipelineRefs []string `json:"pipeline_refs,omitempty"`
	DeployRefs   []string `json:"deploy_refs,omitempty"`

	// Cross-system links + mirror.
	MirrorPath     string `json:"mirror_path,omitempty"`
	MillsBacklogID string `json:"mills_backlog_id,omitempty"`
	GitLabIssueIID int    `json:"gitlab_issue_iid,omitempty"`

	PhaseHistory []PhaseTransition `json:"phase_history,omitempty"`

	// Slices is populated on read by aggregating the slice collection; it is
	// NOT stored on the plan payload (slices are their own records).
	Slices []PlanSlice `json:"slices,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PlanSlice is an independently shippable slice of a Plan, stored as its own
// record so parallel slice-implementers update it without racing on the plan.
// A fresh implementer looks itself up by ID rather than relying on the spawn
// prompt; Files is the basis for (Slice 4) claim enforcement.
type PlanSlice struct {
	ID                 string    `json:"id"`
	PlanID             string    `json:"plan_id"`
	Order              int       `json:"order"`
	Name               string    `json:"name"`
	Goal               string    `json:"goal,omitempty"`
	Files              []string  `json:"files,omitempty"`
	AcceptanceCriteria string    `json:"acceptance_criteria,omitempty"`
	TestStrategy       string    `json:"test_strategy,omitempty"`
	InterfaceContracts string    `json:"interface_contracts,omitempty"`
	BranchName         string    `json:"branch_name,omitempty"`
	DependsOn          []string  `json:"depends_on,omitempty"` // other slice_ids
	Phase              string    `json:"phase"`
	AssignedAgentID    string    `json:"assigned_agent_id,omitempty"`
	WorktreeID         string    `json:"worktree_id,omitempty"`
	SessionID          string    `json:"session_id,omitempty"`
	CommitRefs         []string  `json:"commit_refs,omitempty"`
	MRRef              string    `json:"mr_ref,omitempty"`
	Decisions          []string  `json:"decisions,omitempty"` // blockers/decisions anchored here
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
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

// GeneratePlanID returns a stable "plan-<slug>-<short>" id, deterministic in
// (title, namespace, timestamp).
func GeneratePlanID(title, namespace string, ts time.Time) string {
	short := GenerateID(namespace, "plan", title, ts)
	if len(short) > 6 {
		short = short[:6]
	}
	return "plan-" + planSlug(title) + "-" + short
}

// planToPayload flattens a Plan into a Qdrant payload. Indexed scalar fields
// live at the top level for keyword-filtered queries; structured sub-objects
// round-trip via JSON blobs. Slices are NOT stored here (own collection).
func planToPayload(p *Plan) map[string]any {
	return map[string]any{
		"id":                  p.ID,
		"slug":                p.Slug,
		"title":               p.Title,
		"project":             p.Project,
		"namespace":           p.Namespace,
		"status":              p.Phase, // indexed as "status" for consistency
		"spec_doc":            p.SpecDoc,
		"spec_anchor":         p.SpecAnchor,
		"created_by":          p.CreatedBy,
		"source_session_id":   p.SourceSession,
		"success_json":        marshalJSON(p.Success),
		"budget":              p.Budget,
		"riskiest_assumption": p.RiskiestAssumption,
		"kill_test":           p.KillTest,
		"kill_test_status":    p.KillTestStatus,
		"dependencies":        p.Dependencies,
		"mr_refs":             p.MRRefs,
		"pipeline_refs":       p.PipelineRefs,
		"deploy_refs":         p.DeployRefs,
		"mirror_path":         p.MirrorPath,
		"mills_backlog_id":    p.MillsBacklogID,
		"gitlab_issue_iid":    p.GitLabIssueIID,
		"phase_history_json":  marshalJSON(p.PhaseHistory),
		"created_at":          p.CreatedAt.Format(time.RFC3339),
		"updated_at":          p.UpdatedAt.Format(time.RFC3339),
		"_record_type":        "plan",
	}
}

// payloadToPlan rebuilds a Plan from a Qdrant payload (slices not included).
func payloadToPlan(payload map[string]any) *Plan {
	if payload == nil {
		return nil
	}
	id := toString(payload["id"])
	if id == "" {
		return nil
	}
	p := &Plan{
		ID:                 id,
		Slug:               toString(payload["slug"]),
		Title:              toString(payload["title"]),
		Project:            toString(payload["project"]),
		Namespace:          toString(payload["namespace"]),
		Phase:              toString(payload["status"]),
		SpecDoc:            toString(payload["spec_doc"]),
		SpecAnchor:         toString(payload["spec_anchor"]),
		CreatedBy:          toString(payload["created_by"]),
		SourceSession:      toString(payload["source_session_id"]),
		Budget:             toString(payload["budget"]),
		RiskiestAssumption: toString(payload["riskiest_assumption"]),
		KillTest:           toString(payload["kill_test"]),
		KillTestStatus:     toString(payload["kill_test_status"]),
		Dependencies:       toStringSlice(payload["dependencies"]),
		MRRefs:             toStringSlice(payload["mr_refs"]),
		PipelineRefs:       toStringSlice(payload["pipeline_refs"]),
		DeployRefs:         toStringSlice(payload["deploy_refs"]),
		MirrorPath:         toString(payload["mirror_path"]),
		MillsBacklogID:     toString(payload["mills_backlog_id"]),
		GitLabIssueIID:     toInt(payload["gitlab_issue_iid"]),
	}
	if raw := toString(payload["success_json"]); raw != "" {
		var sc SuccessCriteria
		if json.Unmarshal([]byte(raw), &sc) == nil {
			p.Success = &sc
		}
	}
	if raw := toString(payload["phase_history_json"]); raw != "" {
		_ = json.Unmarshal([]byte(raw), &p.PhaseHistory)
	}
	if t, err := time.Parse(time.RFC3339, toString(payload["created_at"])); err == nil {
		p.CreatedAt = t
	}
	if t, err := time.Parse(time.RFC3339, toString(payload["updated_at"])); err == nil {
		p.UpdatedAt = t
	}
	return p
}

// sliceToPayload flattens a PlanSlice into a Qdrant payload.
func sliceToPayload(s *PlanSlice) map[string]any {
	return map[string]any{
		"id":                  s.ID,
		"plan_id":             s.PlanID,
		"order":               s.Order,
		"name":                s.Name,
		"goal":                s.Goal,
		"files":               s.Files,
		"acceptance_criteria": s.AcceptanceCriteria,
		"test_strategy":       s.TestStrategy,
		"interface_contracts": s.InterfaceContracts,
		"branch_name":         s.BranchName,
		"depends_on":          s.DependsOn,
		"status":              s.Phase, // indexed as "status"
		"assigned_agent_id":   s.AssignedAgentID,
		"worktree_id":         s.WorktreeID,
		"session_id":          s.SessionID,
		"commit_refs":         s.CommitRefs,
		"mr_ref":              s.MRRef,
		"decisions":           s.Decisions,
		"created_at":          s.CreatedAt.Format(time.RFC3339),
		"updated_at":          s.UpdatedAt.Format(time.RFC3339),
		"_record_type":        "plan_slice",
	}
}

// payloadToSlice rebuilds a PlanSlice from a Qdrant payload.
func payloadToSlice(payload map[string]any) *PlanSlice {
	if payload == nil {
		return nil
	}
	id := toString(payload["id"])
	if id == "" {
		return nil
	}
	s := &PlanSlice{
		ID:                 id,
		PlanID:             toString(payload["plan_id"]),
		Order:              toInt(payload["order"]),
		Name:               toString(payload["name"]),
		Goal:               toString(payload["goal"]),
		Files:              toStringSlice(payload["files"]),
		AcceptanceCriteria: toString(payload["acceptance_criteria"]),
		TestStrategy:       toString(payload["test_strategy"]),
		InterfaceContracts: toString(payload["interface_contracts"]),
		BranchName:         toString(payload["branch_name"]),
		DependsOn:          toStringSlice(payload["depends_on"]),
		Phase:              toString(payload["status"]),
		AssignedAgentID:    toString(payload["assigned_agent_id"]),
		WorktreeID:         toString(payload["worktree_id"]),
		SessionID:          toString(payload["session_id"]),
		CommitRefs:         toStringSlice(payload["commit_refs"]),
		MRRef:              toString(payload["mr_ref"]),
		Decisions:          toStringSlice(payload["decisions"]),
	}
	if t, err := time.Parse(time.RFC3339, toString(payload["created_at"])); err == nil {
		s.CreatedAt = t
	}
	if t, err := time.Parse(time.RFC3339, toString(payload["updated_at"])); err == nil {
		s.UpdatedAt = t
	}
	return s
}

// marshalJSON serializes v to a compact JSON string, "" on nil/empty/error.
func marshalJSON(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return ""
	}
	return string(b)
}
