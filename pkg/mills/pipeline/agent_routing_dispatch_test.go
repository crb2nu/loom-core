package pipeline

import (
	"context"
	"testing"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// routingDispatchPolicy is the operator's worked routing example, parsed through
// the real policy loader so this test exercises the same YAML shape the cluster
// ConfigMap carries.
const routingDispatchPolicy = `
version: 2
pipeline:
  stage_agents:
    implement: gemini
  agent_routing:
    enabled: true
    rules:
      - match:
          path_globs: ["internal/hud/frontend/**", "**/*.svelte"]
        route: {agent: claude-code, model: claude-opus-5}
      - match:
          path_globs: ["pkg/**", "cmd/**"]
        route: {agent: codex, model: gpt-5.6-sol}
`

// realRouteFor wires the SpawnWorker to the production resolver. The operator
// adds only the env break-glass and the dispatch event on top of this, so a
// green assertion here covers the policy→SpawnRequest path end to end.
func realRouteFor(t *testing.T) func(context.Context, string, *store.BacklogItem) mills.AgentDecision {
	t.Helper()
	pol, err := mills.ParsePolicy([]byte(routingDispatchPolicy))
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}
	if err := pol.Validate(); err != nil {
		t.Fatalf("validate policy: %v", err)
	}
	return func(_ context.Context, stage string, item *store.BacklogItem) mills.AgentDecision {
		return pol.ResolveAgentRoute(stage, item)
	}
}

// TestSpawnWorker_PerItemRouting is the dispatcher-integration contract for
// running claude-code and codex implementers simultaneously: two items dispatch
// through the SAME worker wiring and land on different harnesses, each carrying
// the provenance that explains the choice.
func TestSpawnWorker_PerItemRouting(t *testing.T) {
	cases := []struct {
		name          string
		item          *store.BacklogItem
		wantAgent     string
		wantModel     string
		wantDecidedBy string
	}{
		{
			name: "agent/codex label routes to codex",
			item: &store.BacklogItem{
				ID:     "BL-LABEL",
				Labels: []string{"agent/codex"},
				Slices: []store.Slice{{Name: "s1", Files: []string{"internal/hud/frontend/App.svelte"}}},
			},
			wantAgent: "codex",
			// The label re-targets the vendor away from the rule's
			// claude-opus-5, so no model pin survives.
			wantModel:     "",
			wantDecidedBy: mills.AgentDecidedByLabel,
		},
		{
			name: "frontend globs route to claude-code",
			item: &store.BacklogItem{
				ID:     "BL-FRONTEND",
				Slices: []store.Slice{{Name: "s1", Files: []string{"internal/hud/frontend/src/Panel.svelte"}}},
			},
			wantAgent:     "claude-code",
			wantModel:     "claude-opus-5",
			wantDecidedBy: mills.AgentDecidedByRule(0),
		},
		{
			name: "backend globs route to codex",
			item: &store.BacklogItem{
				ID:     "BL-BACKEND",
				Slices: []store.Slice{{Name: "s1", Files: []string{"pkg/mills/pipeline/dispatcher.go"}}},
			},
			wantAgent:     "codex",
			wantModel:     "gpt-5.6-sol",
			wantDecidedBy: mills.AgentDecidedByRule(1),
		},
		{
			name: "unmatched item keeps the stage_agents harness",
			item: &store.BacklogItem{
				ID:     "BL-DOCS",
				Slices: []store.Slice{{Name: "s1", Files: []string{"docs/MILLS.md"}}},
			},
			wantAgent:     "gemini",
			wantModel:     "",
			wantDecidedBy: mills.AgentDecidedByStageAgents,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spawn := &fakeSpawn{}
			w := &SpawnWorker{
				Client:    spawn,
				Model:     "claude-code",
				PromptFor: func(JobContext) string { return "noop" },
				RouteFor:  realRouteFor(t),
			}
			jc := sampleJobContext("implement", func(jc *JobContext) {
				jc.Item = tc.item
				jc.Run.BacklogID = tc.item.ID
			})
			out, err := w.Run(context.Background(), jc)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := len(spawn.calls); got != 1 {
				t.Fatalf("expected 1 spawn call, got %d", got)
			}
			if got := spawn.calls[0].Model; got != tc.wantAgent {
				t.Errorf("SpawnRequest.Model (agent): got %q want %q", got, tc.wantAgent)
			}
			if got := spawn.calls[0].AgentModel; got != tc.wantModel {
				t.Errorf("SpawnRequest.AgentModel: got %q want %q", got, tc.wantModel)
			}
			// Only a dispatch routing actually CLAIMED carries provenance; a
			// fall-through to stage_agents must leave the row untouched.
			if !mills.AgentRouted(mills.AgentDecision{DecidedBy: tc.wantDecidedBy}) {
				if _, present := out.Artifacts[agentRoutingArtifactKey]; present {
					t.Errorf("fall-through dispatch must not stamp %q: %+v", agentRoutingArtifactKey, out.Artifacts)
				}
				return
			}
			// Provenance must reach the persisted stage row so an operator can
			// answer "why codex?" without replaying policy.
			routing, ok := out.Artifacts[agentRoutingArtifactKey].(map[string]any)
			if !ok {
				t.Fatalf("stage artifacts missing %q: %+v", agentRoutingArtifactKey, out.Artifacts)
			}
			if got := routing["decided_by"]; got != tc.wantDecidedBy {
				t.Errorf("artifact decided_by: got %v want %q", got, tc.wantDecidedBy)
			}
			if got := routing["agent"]; got != tc.wantAgent {
				t.Errorf("artifact agent: got %v want %q", got, tc.wantAgent)
			}
			if got := routing["model"]; got != tc.wantModel {
				t.Errorf("artifact model: got %v want %q", got, tc.wantModel)
			}
			if out.Model != tc.wantAgent {
				t.Errorf("StageOutput.Model: got %q want %q", out.Model, tc.wantAgent)
			}
		})
	}
}

// TestSpawnWorker_RoutingAppliesToEverySpawnStage: routing must reach all three
// SpawnWorker-driven stages, not just implement.
func TestSpawnWorker_RoutingAppliesToEverySpawnStage(t *testing.T) {
	for _, stage := range []string{"plan_slice", "implement", "pr_self_review"} {
		t.Run(stage, func(t *testing.T) {
			spawn := &fakeSpawn{}
			w := &SpawnWorker{
				Client:    spawn,
				Model:     "claude-code",
				PromptFor: func(JobContext) string { return "noop" },
				RouteFor:  realRouteFor(t),
			}
			item := &store.BacklogItem{
				ID:     "BL-STAGE",
				Slices: []store.Slice{{Name: "s1", Files: []string{"pkg/mills/policy.go"}}},
			}
			jc := sampleJobContext(stage, func(jc *JobContext) { jc.Item = item })
			out, err := w.Run(context.Background(), jc)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := spawn.calls[0].Model; got != "codex" {
				t.Errorf("stage %q agent: got %q want codex", stage, got)
			}
			if got := spawn.calls[0].AgentModel; got != "gpt-5.6-sol" {
				t.Errorf("stage %q model: got %q want gpt-5.6-sol", stage, got)
			}
			if _, ok := out.Artifacts[agentRoutingArtifactKey]; !ok {
				t.Errorf("stage %q: missing routing artifact", stage)
			}
		})
	}
}

// TestSpawnWorker_NoRoutingLeavesArtifactsUntouched holds the inert guarantee at
// the dispatcher boundary. Both shapes matter: a nil RouteFor (pre-routing
// wiring) AND a wired resolver over a policy with no agent_routing block — the
// latter is what production actually looks like for an operator who never opted
// in, and it still returns a non-empty DecidedBy ("stage_agents"/"default").
func TestSpawnWorker_NoRoutingLeavesArtifactsUntouched(t *testing.T) {
	const noRoutingPolicy = `
version: 2
pipeline:
  stage_agents:
    implement: codex
  stage_models:
    implement: gpt-5.6-terra
`
	pol, err := mills.ParsePolicy([]byte(noRoutingPolicy))
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}
	if err := pol.Validate(); err != nil {
		t.Fatalf("validate policy: %v", err)
	}

	cases := []struct {
		name     string
		routeFor func(context.Context, string, *store.BacklogItem) mills.AgentDecision
	}{
		{name: "nil closure", routeFor: nil},
		{
			name: "wired resolver over a policy with no agent_routing block",
			routeFor: func(_ context.Context, stage string, item *store.BacklogItem) mills.AgentDecision {
				return pol.ResolveAgentRoute(stage, item)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &SpawnWorker{
				Client:    &fakeSpawn{},
				Model:     "claude-code",
				PromptFor: func(JobContext) string { return "noop" },
				RouteFor:  tc.routeFor,
			}
			jc := sampleJobContext("implement", func(jc *JobContext) {
				// An agent/* label must stay inert too — it is part of the
				// routing feature and switches off with it.
				jc.Item.Labels = []string{"agent/claude-code"}
				jc.Item.Slices = []store.Slice{{Name: "s1", Files: []string{"pkg/x.go"}}}
			})
			out, err := w.Run(context.Background(), jc)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if _, present := out.Artifacts[agentRoutingArtifactKey]; present {
				t.Errorf("unrouted dispatch must not stamp %q: %+v", agentRoutingArtifactKey, out.Artifacts)
			}
		})
	}
}
