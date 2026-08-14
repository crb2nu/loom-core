package main

import (
	"regexp"
	"testing"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/store"
)

const provenancePolicyWithModels = `
version: 2
budgets:
  council:  { max_usd_per_run: 15, max_usd_per_day: 50 }
  pipeline: { max_usd_per_run: 5, max_usd_per_day: 75 }
pipeline:
  retry: { max_attempts: 3, cooldown_seconds: 300 }
  stage_models:
    implement: gpt-5.6-terra
    plan_slice: gpt-5.6-sol
`

// TestProvenancePromptHashes_CoversWiredTemplates: every stage prompt template
// and judge rubric this binary can run under gets a digest, so a per-prompt-
// version rollup can join on the stamp instead of on deploy timestamps.
func TestProvenancePromptHashes_CoversWiredTemplates(t *testing.T) {
	hashes := provenancePromptHashes()
	normalized := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for stage, tmpl := range stagePromptTemplates {
		got := hashes["stage_prompt:"+stage]
		if got != mills.ProvenanceDigest([]byte(tmpl)) {
			t.Errorf("stage_prompt:%s = %q, want the template digest", stage, got)
		}
		if !normalized.MatchString(got) {
			t.Errorf("stage_prompt:%s = %q, want 64 lowercase hex chars", stage, got)
		}
	}
	rubrics := map[string]string{
		"judge_rubric:" + gates.SpecConformanceRubricName: gates.SpecConformanceRubric,
		"judge_rubric:" + gates.PRSelfReviewRubricName:    gates.PRSelfReviewRubric,
	}
	for key, body := range rubrics {
		if got := hashes[key]; got != mills.ProvenanceDigest([]byte(body)) {
			t.Errorf("%s = %q, want the rubric digest", key, got)
		}
	}
	if len(hashes) != len(stagePromptTemplates)+len(rubrics) {
		t.Errorf("prompt hashes = %v, want exactly the wired templates and rubrics", hashes)
	}
}

// TestProvenanceStageModels_ResolvesThroughSpawnPrecedence: the stamp reads
// models through the same chain the SpawnWorker uses. Reading policy directly
// would miss the break-glass env, which is precisely the deploy where knowing
// what a run ran on matters.
func TestProvenanceStageModels_ResolvesThroughSpawnPrecedence(t *testing.T) {
	item := &store.BacklogItem{ID: "MILLS-PROV-1", Title: "provenance"}

	t.Run("policy_pins", func(t *testing.T) {
		t.Setenv("LOOM_MILLS_SPAWN_AGENT", "")
		t.Setenv("LOOM_MILLS_SPAWN_MODEL", "")
		pm := newAgentPolicyManager(t, provenancePolicyWithModels)
		models := provenanceStageModels(resolveSpawnRoute(pm))(item)
		if models["implement"] != "gpt-5.6-terra" || models["plan_slice"] != "gpt-5.6-sol" {
			t.Fatalf("models = %v, want the policy pins", models)
		}
		if _, ok := models["pr_self_review"]; ok {
			t.Fatalf("models = %v, want unpinned stages omitted", models)
		}
	})

	t.Run("env_break_glass_wins", func(t *testing.T) {
		t.Setenv("LOOM_MILLS_SPAWN_AGENT", "")
		t.Setenv("LOOM_MILLS_SPAWN_MODEL", "gpt-5.6-sol")
		pm := newAgentPolicyManager(t, provenancePolicyWithModels)
		models := provenanceStageModels(resolveSpawnRoute(pm))(item)
		for stage := range mills.StageModelKeysValid {
			if models[stage] != "gpt-5.6-sol" {
				t.Fatalf("models[%s] = %q, want the break-glass model", stage, models[stage])
			}
		}
	})
}

// TestResolveSpawnRoute_ResolvesRoutingWithoutAStore: provenance resolves
// routing at run start, before any stage dispatch, so it must not go through
// the event-appending closure — that would attribute a dispatch that never
// happened. resolveSpawnRoute takes no store and still reaches the routed
// rungs of the chain.
func TestResolveSpawnRoute_ResolvesRoutingWithoutAStore(t *testing.T) {
	const policyWithRouting = provenancePolicyWithModels + `  agent_routing:
    enabled: true
    rules:
      - match: { labels: [ui] }
        route: { agent: gemini }
`
	t.Setenv("LOOM_MILLS_SPAWN_AGENT", "")
	t.Setenv("LOOM_MILLS_SPAWN_MODEL", "")
	pm := newAgentPolicyManager(t, policyWithRouting)
	item := &store.BacklogItem{ID: "MILLS-PROV-2", Title: "labelled", Labels: []string{"agent/codex"}}

	// A label-routed decision is exactly what recordAgentRoute would persist;
	// resolveSpawnRoute must produce it with no store at all.
	d := resolveSpawnRoute(pm)("implement", item)
	if !mills.AgentRouted(d) {
		t.Fatalf("decision = %+v, want a routed decision to make this assertion meaningful", d)
	}
	if d.Agent != "codex" {
		t.Fatalf("agent = %q, want codex from the agent/* label", d.Agent)
	}
}
