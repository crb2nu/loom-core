package bridge

// agent_plans.go -- HUD bridge to the agent-context Plan store (plan store S6).
// Read-only: lists plans and fetches a plan (+ slices) for the lifecycle view.
// These call the agent_plan_* MCP tools; if the daemon predates the plan store
// the call errors and callers degrade to an empty view.

// PlanSliceInfo is the HUD view of one slice.
type PlanSliceInfo struct {
	ID              string   `json:"id"`
	PlanID          string   `json:"plan_id"`
	Order           int      `json:"order"`
	Name            string   `json:"name"`
	Goal            string   `json:"goal,omitempty"`
	Phase           string   `json:"phase"`
	Files           []string `json:"files,omitempty"`
	BranchName      string   `json:"branch_name,omitempty"`
	AssignedAgentID string   `json:"assigned_agent_id,omitempty"`
	MRRef           string   `json:"mr_ref,omitempty"`
	Decisions       []string `json:"decisions,omitempty"`
	// Connective tissue — the slice DAG edges + contracts, rendered inline in the
	// plan detail drawer. DependsOn holds resolved slice_ids the store minted.
	DependsOn          []string `json:"depends_on,omitempty"`
	InterfaceContracts string   `json:"interface_contracts,omitempty"`
	AcceptanceCriteria string   `json:"acceptance_criteria,omitempty"`
}

// PlanPhaseTransition mirrors the store's phase_history entries.
type PlanPhaseTransition struct {
	From  string `json:"from"`
	To    string `json:"to"`
	At    string `json:"at"`
	Actor string `json:"actor,omitempty"`
	Note  string `json:"note,omitempty"`
}

// PlanInfo is the HUD view of a plan and its lifecycle.
type PlanInfo struct {
	ID             string                `json:"id"`
	Slug           string                `json:"slug"`
	Title          string                `json:"title"`
	Project        string                `json:"project,omitempty"`
	Namespace      string                `json:"namespace,omitempty"`
	Phase          string                `json:"phase"`
	Priority       string                `json:"priority,omitempty"`
	Objective      string                `json:"objective,omitempty"`
	RespunFrom     string                `json:"respun_from,omitempty"`
	CreatedBy      string                `json:"created_by,omitempty"`
	SourceSession  string                `json:"source_session_id,omitempty"`
	MRRefs         []string              `json:"mr_refs,omitempty"`
	PipelineRefs   []string              `json:"pipeline_refs,omitempty"`
	DeployRefs     []string              `json:"deploy_refs,omitempty"`
	MirrorPath     string                `json:"mirror_path,omitempty"`
	MillsBacklogID string                `json:"mills_backlog_id,omitempty"`
	KillTestStatus string                `json:"kill_test_status,omitempty"`
	PhaseHistory   []PlanPhaseTransition `json:"phase_history,omitempty"`
	Slices         []PlanSliceInfo       `json:"slices,omitempty"`
	// SliceSummary is a phase->count rollup the plan store computes on list so
	// the board can show per-plan slice progress without a detail fetch.
	SliceSummary map[string]int `json:"slice_summary,omitempty"`
	CreatedAt    string         `json:"created_at,omitempty"`
	UpdatedAt    string         `json:"updated_at,omitempty"`
}

// Plans lists plans, optionally filtered by project / namespace / phase.
// Read path is deliberately NOT agent-scoped (the store enforces that).
func (a *AgentBridge) Plans(project, namespace, phase string) ([]PlanInfo, error) {
	args := map[string]any{}
	if project != "" {
		args["project"] = project
	}
	if namespace != "" {
		args["namespace"] = namespace
	}
	if phase != "" {
		args["phase"] = phase
	}
	var result struct {
		Plans []PlanInfo `json:"plans"`
	}
	if err := a.callAgentTool("agent_plan_list", args, &result); err != nil {
		return nil, err
	}
	return result.Plans, nil
}

// Plan fetches a single plan (with aggregated slices) by id.
func (a *AgentBridge) Plan(planID string) (*PlanInfo, error) {
	var result struct {
		Plan PlanInfo `json:"plan"`
	}
	if err := a.callAgentTool("agent_plan_get", map[string]any{"plan_id": planID}, &result); err != nil {
		return nil, err
	}
	return &result.Plan, nil
}

// PlanSliceInput is one seed slice for a created/composed plan. Mirrors the
// agent_plan_create `slices[]` item shape (name/goal/files); the compose editor
// carries these from the cherry-picked draft slices.
type PlanSliceInput struct {
	Name  string
	Goal  string
	Files []string
}

// PlanCreateParams holds the fields for creating a plan from the HUD.
type PlanCreateParams struct {
	Title     string
	Project   string
	Namespace string
	Phase     string
	Priority  string
	SpecDoc   string
	AgentID   string
	// Slices optionally seeds the plan's decomposition. When non-empty each
	// slice is passed to agent_plan_create as {name, goal?, files?}.
	Slices []PlanSliceInput
}

// PlanCreateResult is the agent_plan_create response.
type PlanCreateResult struct {
	PlanID     string `json:"plan_id"`
	Slug       string `json:"slug"`
	Phase      string `json:"phase"`
	SliceCount int    `json:"slice_count"`
}

// PlanCreate creates a plan via the store.
func (a *AgentBridge) PlanCreate(p PlanCreateParams) (*PlanCreateResult, error) {
	args := map[string]any{"title": p.Title}
	if p.Project != "" {
		args["project"] = p.Project
	}
	if p.Namespace != "" {
		args["namespace"] = p.Namespace
	}
	if p.Phase != "" {
		args["phase"] = p.Phase
	}
	if p.Priority != "" {
		args["priority"] = p.Priority
	}
	if p.SpecDoc != "" {
		args["spec_doc"] = p.SpecDoc
	}
	if p.AgentID != "" {
		args["agent_id"] = p.AgentID
	}
	if len(p.Slices) > 0 {
		slices := make([]map[string]any, 0, len(p.Slices))
		for _, s := range p.Slices {
			if s.Name == "" {
				continue
			}
			sl := map[string]any{"name": s.Name}
			if s.Goal != "" {
				sl["goal"] = s.Goal
			}
			if len(s.Files) > 0 {
				sl["files"] = s.Files
			}
			slices = append(slices, sl)
		}
		if len(slices) > 0 {
			args["slices"] = slices
		}
	}
	var res PlanCreateResult
	if err := a.callAgentTool("agent_plan_create", args, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// PlanSetPriorityResult is the agent_plan_update response projection for a
// priority change.
type PlanSetPriorityResult struct {
	PlanID string `json:"plan_id"`
}

// PlanSetPriority sets (or clears, with "") a plan's warp-beam priority bucket
// via agent_plan_update. The priority key is always sent — an empty string is
// the explicit "clear" signal, not an omitted field.
func (a *AgentBridge) PlanSetPriority(planID, priority string) (*PlanSetPriorityResult, error) {
	args := map[string]any{"plan_id": planID, "priority": priority}
	var res PlanSetPriorityResult
	if err := a.callAgentTool("agent_plan_update", args, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// PlanAdvanceResult is the agent_plan_lifecycle_advance response.
type PlanAdvanceResult struct {
	PlanID    string `json:"plan_id"`
	FromPhase string `json:"from_phase"`
	ToPhase   string `json:"to_phase"`
}

// PlanAdvance transitions a plan to a new lifecycle phase.
func (a *AgentBridge) PlanAdvance(planID, toPhase, agentID, note string) (*PlanAdvanceResult, error) {
	args := map[string]any{"plan_id": planID, "to_phase": toPhase}
	if agentID != "" {
		args["agent_id"] = agentID
	}
	if note != "" {
		args["note"] = note
	}
	var res PlanAdvanceResult
	if err := a.callAgentTool("agent_plan_lifecycle_advance", args, &res); err != nil {
		return nil, err
	}
	return &res, nil
}
