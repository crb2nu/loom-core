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
	Phase           string   `json:"phase"`
	Files           []string `json:"files,omitempty"`
	BranchName      string   `json:"branch_name,omitempty"`
	AssignedAgentID string   `json:"assigned_agent_id,omitempty"`
	MRRef           string   `json:"mr_ref,omitempty"`
	Decisions       []string `json:"decisions,omitempty"`
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
	CreatedAt      string                `json:"created_at,omitempty"`
	UpdatedAt      string                `json:"updated_at,omitempty"`
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
