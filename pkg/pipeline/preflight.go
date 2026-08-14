// Package pipeline contains generic action-gating helpers for execution
// pipelines.
package pipeline

import "fmt"

// Action names a guarded operation.
type Action string

const (
	ActionCouncilPlan     Action = "council_plan"
	ActionPipelineExecute Action = "pipeline_execute"
	ActionMerge           Action = "merge"
)

// PreflightCheck is one prerequisite observation for an action.
type PreflightCheck struct {
	Name     string
	Passed   bool
	Required bool
	Message  string
}

// PreflightPolicy lists the required check names for each action.
type PreflightPolicy struct {
	Required map[Action][]string
}

// DefaultPreflightPolicy captures the minimum checks expected before the
// autonomous planning and execution path may mutate state.
func DefaultPreflightPolicy() PreflightPolicy {
	return PreflightPolicy{Required: map[Action][]string{
		ActionCouncilPlan:     {"roadmap_extracted", "policy_loaded"},
		ActionPipelineExecute: {"worktree_clean", "scope_loaded", "policy_loaded"},
		ActionMerge:           {"tests_green", "mr_ready", "policy_loaded"},
	}}
}

// PreflightResult is the gate decision for an action.
type PreflightResult struct {
	Allowed    bool
	FailClosed bool
	Reasons    []string
	Warnings   []string
}

// EvaluatePreflight applies policy to checks. Missing required policy, missing
// required checks, and failed required checks all block the action.
func EvaluatePreflight(action Action, policy PreflightPolicy, checks []PreflightCheck) PreflightResult {
	required, ok := policy.Required[action]
	if !ok || len(required) == 0 {
		return PreflightResult{
			Allowed:    false,
			FailClosed: true,
			Reasons:    []string{fmt.Sprintf("missing preflight policy for action %q", action)},
		}
	}

	byName := make(map[string]PreflightCheck, len(checks))
	var warnings []string
	for _, check := range checks {
		if check.Name == "" {
			continue
		}
		byName[check.Name] = check
		if !check.Required && !check.Passed {
			warnings = append(warnings, formatCheckFailure("optional preflight", check))
		}
	}

	var reasons []string
	failClosed := false
	for _, name := range required {
		check, exists := byName[name]
		if !exists {
			reasons = append(reasons, fmt.Sprintf("missing required preflight %q", name))
			failClosed = true
			continue
		}
		if !check.Passed {
			reasons = append(reasons, formatCheckFailure("required preflight", check))
		}
	}

	return PreflightResult{
		Allowed:    len(reasons) == 0,
		FailClosed: failClosed,
		Reasons:    reasons,
		Warnings:   warnings,
	}
}

func formatCheckFailure(prefix string, check PreflightCheck) string {
	if check.Message == "" {
		return fmt.Sprintf("%s %q failed", prefix, check.Name)
	}
	return fmt.Sprintf("%s %q failed: %s", prefix, check.Name, check.Message)
}
