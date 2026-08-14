package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/crb2nu/loom/pkg/mills"
)

// newAgentPolicyManager builds a SkipWatch PolicyManager from an inline policy
// body so the precedence test can point agentForStage at a real policy with a
// pipeline.stage_agents override.
func newAgentPolicyManager(t *testing.T, body string) *mills.PolicyManager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	pm, err := mills.NewPolicyManager(context.Background(), path, mills.PolicyManagerOptions{SkipWatch: true})
	if err != nil {
		t.Fatalf("policy manager: %v", err)
	}
	t.Cleanup(func() { _ = pm.Close() })
	return pm
}

// TestAgentForStage_Precedence pins the W2 contract: the effective spawn agent
// resolves LOOM_MILLS_SPAWN_AGENT env (break-glass, wins) > policy
// pipeline.stage_agents > mills.AgentDefault. The env is read once when the
// closure is built, so each sub-case rebuilds it after setting env.
func TestAgentForStage_Precedence(t *testing.T) {
	const policyWithReviewOverride = `
version: 2
budgets:
  council:  { max_usd_per_run: 15, max_usd_per_day: 50 }
  pipeline: { max_usd_per_run: 5, max_usd_per_day: 75 }
pipeline:
  retry: { max_attempts: 3, cooldown_seconds: 300 }
  stage_agents:
    pr_self_review: gemini
`
	const policyNoOverride = `
version: 2
budgets:
  council:  { max_usd_per_run: 15, max_usd_per_day: 50 }
  pipeline: { max_usd_per_run: 5, max_usd_per_day: 75 }
pipeline:
  retry: { max_attempts: 3, cooldown_seconds: 300 }
`

	t.Run("policy_over_default", func(t *testing.T) {
		t.Setenv("LOOM_MILLS_SPAWN_AGENT", "")
		pm := newAgentPolicyManager(t, policyWithReviewOverride)
		agentFor := agentForStage(pm)
		if got := agentFor("pr_self_review"); got != "gemini" {
			t.Errorf("pr_self_review: got %q want gemini (policy override)", got)
		}
		if got := agentFor("implement"); got != mills.AgentDefault {
			t.Errorf("implement: got %q want %q (built-in default)", got, mills.AgentDefault)
		}
	})

	t.Run("default_when_no_override", func(t *testing.T) {
		t.Setenv("LOOM_MILLS_SPAWN_AGENT", "")
		pm := newAgentPolicyManager(t, policyNoOverride)
		agentFor := agentForStage(pm)
		for _, stage := range []string{"plan_slice", "implement", "pr_self_review"} {
			if got := agentFor(stage); got != mills.AgentDefault {
				t.Errorf("%s: got %q want %q (built-in default)", stage, got, mills.AgentDefault)
			}
		}
	})

	t.Run("env_break_glass_wins_over_policy", func(t *testing.T) {
		t.Setenv("LOOM_MILLS_SPAWN_AGENT", "codex")
		pm := newAgentPolicyManager(t, policyWithReviewOverride)
		agentFor := agentForStage(pm)
		// The break-glass flips EVERY stage, including one with a policy entry.
		for _, stage := range []string{"plan_slice", "implement", "pr_self_review"} {
			if got := agentFor(stage); got != "codex" {
				t.Errorf("%s: got %q want codex (env break-glass overrides policy)", stage, got)
			}
		}
	})
}

// TestModelForStage_Precedence pins the stage_models contract, mirroring
// TestAgentForStage_Precedence: LOOM_MILLS_SPAWN_MODEL env (break-glass, wins) >
// policy pipeline.stage_models > empty ("vendor default"). Unlike the agent
// closure the fallback is the empty string — each vendor CLI owns its own
// default model, so there is no operator-side default to substitute.
func TestModelForStage_Precedence(t *testing.T) {
	const policyWithModels = `
version: 2
budgets:
  council:  { max_usd_per_run: 15, max_usd_per_day: 50 }
  pipeline: { max_usd_per_run: 5, max_usd_per_day: 75 }
pipeline:
  retry: { max_attempts: 3, cooldown_seconds: 300 }
  stage_models:
    implement:  gpt-5.6-terra
    plan_slice: gpt-5.6-sol
`
	const policyNoModels = `
version: 2
budgets:
  council:  { max_usd_per_run: 15, max_usd_per_day: 50 }
  pipeline: { max_usd_per_run: 5, max_usd_per_day: 75 }
pipeline:
  retry: { max_attempts: 3, cooldown_seconds: 300 }
`

	t.Run("policy_sets_per_stage_model", func(t *testing.T) {
		t.Setenv("LOOM_MILLS_SPAWN_MODEL", "")
		pm := newAgentPolicyManager(t, policyWithModels)
		modelFor := modelForStage(pm)
		if got := modelFor("implement"); got != "gpt-5.6-terra" {
			t.Errorf("implement: got %q want gpt-5.6-terra", got)
		}
		if got := modelFor("plan_slice"); got != "gpt-5.6-sol" {
			t.Errorf("plan_slice: got %q want gpt-5.6-sol", got)
		}
		// A stage with no entry falls through to empty (vendor default).
		if got := modelFor("pr_self_review"); got != "" {
			t.Errorf("pr_self_review: got %q want empty (vendor default)", got)
		}
	})

	t.Run("empty_when_no_policy", func(t *testing.T) {
		t.Setenv("LOOM_MILLS_SPAWN_MODEL", "")
		pm := newAgentPolicyManager(t, policyNoModels)
		modelFor := modelForStage(pm)
		for _, stage := range []string{"plan_slice", "implement", "pr_self_review"} {
			if got := modelFor(stage); got != "" {
				t.Errorf("%s: got %q want empty (vendor default)", stage, got)
			}
		}
	})

	t.Run("env_break_glass_wins_over_policy", func(t *testing.T) {
		t.Setenv("LOOM_MILLS_SPAWN_MODEL", "gpt-5.6-sol")
		pm := newAgentPolicyManager(t, policyWithModels)
		modelFor := modelForStage(pm)
		// The break-glass flips EVERY stage, including ones with a policy entry.
		for _, stage := range []string{"plan_slice", "implement", "pr_self_review"} {
			if got := modelFor(stage); got != "gpt-5.6-sol" {
				t.Errorf("%s: got %q want gpt-5.6-sol (env break-glass overrides policy)", stage, got)
			}
		}
	})
}
