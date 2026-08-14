package main

import (
	"context"
	"testing"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// policyWithAgentRouting is the operator's worked example: frontend globs to
// claude-code + claude-opus-5, backend globs to codex + gpt-5.6-sol, with a
// stage_agents entry underneath so the fall-through rung is observable.
const policyWithAgentRouting = `
version: 2
budgets:
  council:  { max_usd_per_run: 15, max_usd_per_day: 50 }
  pipeline: { max_usd_per_run: 5, max_usd_per_day: 75 }
pipeline:
  retry: { max_attempts: 3, cooldown_seconds: 300 }
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

func routedItem(id string, labels []string, files ...string) *store.BacklogItem {
	return &store.BacklogItem{
		ID:       id,
		Labels:   labels,
		Priority: store.P2,
		Slices:   []store.Slice{{Name: "s1", Files: files}},
	}
}

// TestSpawnRouteFor_Precedence pins the full effective chain the SpawnWorker
// consults: LOOM_MILLS_SPAWN_AGENT env (break-glass) > item agent/* label >
// first matching agent_routing rule > pipeline.stage_agents > AgentDefault. The
// env is read once when the closure is built, so each sub-case rebuilds it.
func TestSpawnRouteFor_Precedence(t *testing.T) {
	cases := []struct {
		name          string
		envAgent      string
		envModel      string
		item          *store.BacklogItem
		wantAgent     string
		wantModel     string
		wantDecidedBy string
	}{
		{
			name:          "env break-glass beats the label",
			envAgent:      "gemini",
			item:          routedItem("BL-ENV", []string{"agent/codex"}, "pkg/mills/policy.go"),
			wantAgent:     "gemini",
			wantDecidedBy: mills.AgentDecidedByEnv,
		},
		{
			name:          "env model break-glass pins the model",
			envAgent:      "codex",
			envModel:      "gpt-5.6-terra",
			item:          routedItem("BL-ENVMODEL", nil, "internal/hud/frontend/App.svelte"),
			wantAgent:     "codex",
			wantModel:     "gpt-5.6-terra",
			wantDecidedBy: mills.AgentDecidedByEnv,
		},
		{
			name:          "label beats the matching rule",
			item:          routedItem("BL-LABEL", []string{"agent/codex"}, "internal/hud/frontend/App.svelte"),
			wantAgent:     "codex",
			wantDecidedBy: mills.AgentDecidedByLabel,
		},
		{
			name:          "frontend rule beats stage_agents",
			item:          routedItem("BL-FE", nil, "internal/hud/frontend/src/Panel.svelte"),
			wantAgent:     "claude-code",
			wantModel:     "claude-opus-5",
			wantDecidedBy: mills.AgentDecidedByRule(0),
		},
		{
			name:          "backend rule beats stage_agents",
			item:          routedItem("BL-BE", nil, "pkg/mills/pipeline/dispatcher.go"),
			wantAgent:     "codex",
			wantModel:     "gpt-5.6-sol",
			wantDecidedBy: mills.AgentDecidedByRule(1),
		},
		{
			name:          "stage_agents beats the default",
			item:          routedItem("BL-DOCS", nil, "docs/MILLS.md"),
			wantAgent:     "gemini",
			wantDecidedBy: mills.AgentDecidedByStageAgents,
		},
		{
			name:          "sliceless item falls through without panicking",
			item:          &store.BacklogItem{ID: "BL-NOSLICE", Priority: store.P2},
			wantAgent:     "gemini",
			wantDecidedBy: mills.AgentDecidedByStageAgents,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOOM_MILLS_SPAWN_AGENT", tc.envAgent)
			t.Setenv("LOOM_MILLS_SPAWN_MODEL", tc.envModel)
			pm := newAgentPolicyManager(t, policyWithAgentRouting)
			st := openTestStore(t)
			routeFor := spawnRouteFor(pm, st, discardLogger())

			got := routeFor(context.Background(), "implement", tc.item)
			if got.Agent != tc.wantAgent {
				t.Errorf("agent: got %q want %q", got.Agent, tc.wantAgent)
			}
			if got.Model != tc.wantModel {
				t.Errorf("model: got %q want %q", got.Model, tc.wantModel)
			}
			if got.DecidedBy != tc.wantDecidedBy {
				t.Errorf("decided_by: got %q want %q", got.DecidedBy, tc.wantDecidedBy)
			}
		})
	}
}

// policyNoAgentRouting is the pre-routing shape: stage_agents + stage_models
// and no agent_routing block at all.
const policyNoAgentRouting = `
version: 2
budgets:
  council:  { max_usd_per_run: 15, max_usd_per_day: 50 }
  pipeline: { max_usd_per_run: 5, max_usd_per_day: 75 }
pipeline:
  retry: { max_attempts: 3, cooldown_seconds: 300 }
  stage_agents:
    implement: codex
  stage_models:
    implement: gpt-5.6-terra
`

// TestSpawnRouteFor_InertWithoutRoutingBlock is the hard compatibility guard: a
// deployment with no agent_routing block must resolve exactly what
// agentForStage + modelForStage resolved before routing shipped — including
// under the env break-glass, and including for an item carrying an agent/*
// label (the label is part of the routing feature and stays off with it).
func TestSpawnRouteFor_InertWithoutRoutingBlock(t *testing.T) {
	cases := []struct {
		name      string
		envAgent  string
		envModel  string
		item      *store.BacklogItem
		wantAgent string
		wantModel string
	}{
		{
			name:      "policy only",
			item:      routedItem("BL-INERT", nil, "internal/hud/frontend/App.svelte"),
			wantAgent: "codex",
			wantModel: "gpt-5.6-terra",
		},
		{
			name:      "agent label has no effect",
			item:      routedItem("BL-INERT-LABEL", []string{"agent/claude-code"}, "pkg/x.go"),
			wantAgent: "codex",
			wantModel: "gpt-5.6-terra",
		},
		{
			// Same vendor as the pin was authored for: the model survives, so
			// a break-glass that just re-asserts the configured harness is a
			// no-op on the model.
			name:      "env agent matching the policy agent keeps the model",
			envAgent:  "codex",
			item:      routedItem("BL-INERT-ENV", nil, "pkg/x.go"),
			wantAgent: "codex",
			wantModel: "gpt-5.6-terra",
		},
		{
			// Cross-vendor break-glass: `gpt-5.6-terra` means nothing to
			// claude-code, so the pin must drop rather than reach the CLI.
			name:      "env agent re-targeting the vendor drops the model",
			envAgent:  "claude-code",
			item:      routedItem("BL-INERT-ENVX", nil, "pkg/x.go"),
			wantAgent: "claude-code",
			wantModel: "",
		},
		{
			name:      "env model still wins",
			envModel:  "gpt-5.6-sol",
			item:      routedItem("BL-INERT-ENVMODEL", nil, "pkg/x.go"),
			wantAgent: "codex",
			wantModel: "gpt-5.6-sol",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOOM_MILLS_SPAWN_AGENT", tc.envAgent)
			t.Setenv("LOOM_MILLS_SPAWN_MODEL", tc.envModel)
			pm := newAgentPolicyManager(t, policyNoAgentRouting)
			st := openTestStore(t)
			routeFor := spawnRouteFor(pm, st, discardLogger())

			got := routeFor(context.Background(), "implement", tc.item)
			if got.Agent != tc.wantAgent {
				t.Errorf("agent: got %q want %q", got.Agent, tc.wantAgent)
			}
			if got.Model != tc.wantModel {
				t.Errorf("model: got %q want %q", got.Model, tc.wantModel)
			}
			if mills.AgentRouted(got) {
				t.Errorf("decided_by %q: routing must not claim an unrouted dispatch", got.DecidedBy)
			}
			// No agent_routing block ⇒ no rows in the (unpruned) events table.
			events, err := st.Events.ListBySubject(context.Background(), "backlog_item", tc.item.ID, 10)
			if err != nil {
				t.Fatalf("list events: %v", err)
			}
			for _, ev := range events {
				if ev.Kind == agentRoutedEventKind {
					t.Errorf("unrouted dispatch wrote a %s event", agentRoutedEventKind)
				}
			}
			// With no env override the legacy stage-only resolvers must agree
			// with dispatch, so /wiring never drifts from what actually ran.
			if tc.envAgent == "" && tc.envModel == "" {
				if want := agentForStage(pm)("implement"); got.Agent != want {
					t.Errorf("agent drift vs agentForStage: got %q want %q", got.Agent, want)
				}
				if want := modelForStage(pm)("implement"); got.Model != want {
					t.Errorf("model drift vs modelForStage: got %q want %q", got.Model, want)
				}
			}
		})
	}
}

// TestSpawnRouteFor_EmitsDispatchEvent is the observability contract: every
// routed dispatch leaves an event an operator can read to answer "why did this
// item go to codex?" without replaying policy.
func TestSpawnRouteFor_EmitsDispatchEvent(t *testing.T) {
	t.Setenv("LOOM_MILLS_SPAWN_AGENT", "")
	t.Setenv("LOOM_MILLS_SPAWN_MODEL", "")
	pm := newAgentPolicyManager(t, policyWithAgentRouting)
	st := openTestStore(t)
	routeFor := spawnRouteFor(pm, st, discardLogger())

	item := routedItem("BL-EVENT", nil, "pkg/mills/pipeline/dispatcher.go")
	routeFor(context.Background(), "implement", item)

	events, err := st.Events.ListBySubject(context.Background(), "backlog_item", item.ID, 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var routed *store.Event
	for _, ev := range events {
		if ev.Kind == agentRoutedEventKind {
			routed = ev
			break
		}
	}
	if routed == nil {
		t.Fatalf("no %s event recorded (got %d events)", agentRoutedEventKind, len(events))
	}
	want := map[string]any{
		"item":       item.ID,
		"stage":      "implement",
		"agent":      "codex",
		"model":      "gpt-5.6-sol",
		"decided_by": mills.AgentDecidedByRule(1),
	}
	for k, v := range want {
		if got := routed.Payload[k]; got != v {
			t.Errorf("payload[%s]: got %v want %v", k, got, v)
		}
	}
}

// TestSpawnRouteFor_UnknownLabelDoesNotBlockDispatch: a malformed agent/* label
// is authored by humans and issue importers, so it must degrade to "ignored"
// rather than wedge the item.
func TestSpawnRouteFor_UnknownLabelDoesNotBlockDispatch(t *testing.T) {
	t.Setenv("LOOM_MILLS_SPAWN_AGENT", "")
	t.Setenv("LOOM_MILLS_SPAWN_MODEL", "")
	pm := newAgentPolicyManager(t, policyWithAgentRouting)
	routeFor := spawnRouteFor(pm, openTestStore(t), discardLogger())

	item := routedItem("BL-BADLABEL", []string{"agent/gpt-9"}, "pkg/mills/policy.go")
	got := routeFor(context.Background(), "implement", item)
	if got.Agent != "codex" || got.DecidedBy != mills.AgentDecidedByRule(1) {
		t.Errorf("routing: got %+v, want the backend rule to still apply", got)
	}
	if len(got.IgnoredLabels) != 1 || got.IgnoredLabels[0] != "agent/gpt-9" {
		t.Errorf("ignored labels: got %v want [agent/gpt-9]", got.IgnoredLabels)
	}
}

// TestSpawnRouteFor_NilStoreAndItemlessBaseline: routing must survive an
// unwired store (test/dev operators) and must not write a subject-less event
// for the startup wiring log's item-less baseline probe.
func TestSpawnRouteFor_NilStoreAndItemlessBaseline(t *testing.T) {
	t.Setenv("LOOM_MILLS_SPAWN_AGENT", "")
	t.Setenv("LOOM_MILLS_SPAWN_MODEL", "")
	pm := newAgentPolicyManager(t, policyWithAgentRouting)

	if got := spawnRouteFor(pm, nil, discardLogger())(context.Background(), "implement", routedItem("BL-NOSTORE", nil, "pkg/x.go")); got.Agent != "codex" {
		t.Errorf("nil store: got %+v want codex", got)
	}

	st := openTestStore(t)
	baseline := spawnRouteFor(pm, st, discardLogger())(context.Background(), "implement", nil)
	if baseline.Agent != "gemini" || baseline.DecidedBy != mills.AgentDecidedByStageAgents {
		t.Errorf("item-less baseline: got %+v want gemini/stage_agents", baseline)
	}
	events, err := st.Events.ListBySubject(context.Background(), "backlog_item", "", 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("item-less probe wrote %d event(s); expected none", len(events))
	}
}
