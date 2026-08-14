package bridge

import "strings"

// agent_patterns.go -- HUD bridge to the agent-context Pattern catalog (the
// "pattern library"). Patterns are vetted product archetypes that, given
// Materials, are STAMPED into a Plan that Mills executes.
//
// These call the agent_pattern_* MCP tools. Patterns live in the shared global
// store and are read cross-agent (NOT filtered by agent_id). If the daemon
// predates the pattern catalog the call errors and callers degrade to an empty
// view — mirrors the agent_plans.go contract.

// PatternMaterialField is the HUD view of one typed material input ("fabric").
type PatternMaterialField struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Required    bool     `json:"required,omitempty"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Default     string   `json:"default,omitempty"`
	Example     string   `json:"example,omitempty"`
}

// PatternPin is one closed architecture decision (mirrors the agent-context
// schema): the axes that make a pattern stampable rather than a suggestion.
type PatternPin struct {
	Axis  string `json:"axis"`
	Value string `json:"value"`
}

// PatternGauge is the pattern's acceptance swatch: commands that must exit 0
// plus black-box behavioral assertions.
type PatternGauge struct {
	Description string   `json:"description,omitempty"`
	Commands    []string `json:"commands,omitempty"`
	Assertions  []string `json:"assertions,omitempty"`
}

// PatternProvenance is the taste gate: who authored/approved the pattern and
// how many instances it has shipped green.
type PatternProvenance struct {
	Author                string `json:"author,omitempty"`
	ApprovedBy            string `json:"approved_by,omitempty"`
	InstancesShippedGreen int    `json:"instances_shipped_green,omitempty"`
	Notes                 string `json:"notes,omitempty"`
}

// PatternInfo is the HUD view of a Pattern in the catalog. It carries the
// listing-level fields plus materials_schema so the front-door page can render
// the stamp form without a second fetch, and the instruction-book detail
// (pins, gauge, engrams, provenance) so the page can show what a stamp pins.
type PatternInfo struct {
	ID              string                 `json:"id"`
	Slug            string                 `json:"slug"`
	Name            string                 `json:"name"`
	Makes           string                 `json:"makes"`
	Description     string                 `json:"description,omitempty"`
	Version         string                 `json:"version"`
	Status          string                 `json:"status"`
	MaterialsSchema []PatternMaterialField `json:"materials_schema,omitempty"`
	Pins            []PatternPin           `json:"pins,omitempty"`
	Gauge           *PatternGauge          `json:"gauge,omitempty"`
	Engrams         []string               `json:"engrams,omitempty"`
	DeployContract  string                 `json:"deploy_contract,omitempty"`
	Provenance      *PatternProvenance     `json:"provenance,omitempty"`
	Tags            []string               `json:"tags,omitempty"`
	CreatedBy       string                 `json:"created_by,omitempty"`
	CreatedAt       string                 `json:"created_at,omitempty"`
	UpdatedAt       string                 `json:"updated_at,omitempty"`
}

// PatternInstanceInfo is one stamped execution of a Pattern. Outcome fields
// are optional because in-flight and older stamps do not necessarily have
// run or merge-request results yet.
type PatternInstanceInfo struct {
	StampedAt     string `json:"stamped_at"`
	PlanID        string `json:"plan_id"`
	TargetProject string `json:"target_project"`
	RunID         string `json:"run_id,omitempty"`
	RunStatus     string `json:"run_status,omitempty"`
	RunOutcome    string `json:"run_outcome,omitempty"`
	MRRef         string `json:"mr_ref,omitempty"`
	MRURL         string `json:"mr_url,omitempty"`
	MRStatus      string `json:"mr_status,omitempty"`
	MROutcome     string `json:"mr_outcome,omitempty"`
}

// PatternInstances returns the stamp history embedded by agent_pattern_get.
func (a *AgentBridge) PatternInstances(patternID string) ([]PatternInstanceInfo, error) {
	var result struct {
		Pattern struct {
			Instances []PatternInstanceInfo `json:"instances"`
		} `json:"pattern"`
		Instances []PatternInstanceInfo `json:"instances"`
	}
	if err := a.callAgentTool("agent_pattern_get", map[string]any{"pattern_id": strings.TrimSpace(patternID)}, &result); err != nil {
		return nil, err
	}
	instances := result.Pattern.Instances
	if instances == nil {
		instances = result.Instances
	}
	if instances == nil {
		instances = []PatternInstanceInfo{}
	}
	return instances, nil
}

// PatternList returns patterns filtered by status (candidate|approved|
// deprecated). An empty status returns the full catalog. Read path is NOT
// agent-scoped (the store enforces that).
func (a *AgentBridge) PatternList(status string) ([]PatternInfo, error) {
	args := map[string]any{}
	if status != "" {
		args["status"] = status
	}
	var result struct {
		Patterns []PatternInfo `json:"patterns"`
	}
	if err := a.callAgentTool("agent_pattern_list", args, &result); err != nil {
		return nil, err
	}
	return result.Patterns, nil
}

// PatternStampResult is the agent_pattern_stamp response: it expands the
// pattern's slice_template into a concrete Plan and returns the new plan_id
// plus the required-tools manifest the caller should gate on.
type PatternStampResult struct {
	OK             bool             `json:"ok"`
	PlanID         string           `json:"plan_id"`
	PatternID      string           `json:"pattern_id"`
	PatternVersion string           `json:"pattern_version,omitempty"`
	Materials      map[string]any   `json:"materials,omitempty"`
	Slices         []map[string]any `json:"slices,omitempty"`
	SliceCount     int              `json:"slice_count"`
	ToolsRequired  []string         `json:"tools_required,omitempty"`
	DeployContract string           `json:"deploy_contract,omitempty"`
	Note           string           `json:"note,omitempty"`
}

// PatternStamp stamps a Pattern with Materials, expanding it into a Plan in the
// shared store. Returns the new plan_id, slice count, and the required tools.
func (a *AgentBridge) PatternStamp(patternID string, materials map[string]any, project string) (*PatternStampResult, error) {
	args := map[string]any{
		"pattern_id": patternID,
		"materials":  materials,
	}
	if project != "" {
		args["project"] = project
	}
	var result PatternStampResult
	if err := a.callAgentTool("agent_pattern_stamp", args, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
