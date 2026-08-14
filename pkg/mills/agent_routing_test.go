package mills

import (
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// agentRoutingFixture is the operator's worked example: frontend globs to
// claude-code, backend globs to codex, both fleets running at once.
const agentRoutingFixture = `
version: 2
pipeline:
  agent_routing:
    enabled: true
    rules:
      - match:
          path_globs:
            - "internal/hud/frontend/**"
            - "**/*.svelte"
            - "**/*.css"
        route:
          agent: claude-code
          model: claude-opus-5
      - match:
          labels: ["design", "ux"]
        route:
          agent: claude-code
      - match:
          path_globs:
            - "pkg/**"
            - "cmd/**"
            - "internal/**"
        route:
          agent: codex
          model: gpt-5.6-sol
      - match:
          priority: ["P0"]
        route:
          agent: codex
`

func parseRoutingFixture(t *testing.T) *Policy {
	t.Helper()
	p, err := ParsePolicy([]byte(agentRoutingFixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return p
}

func itemWith(labels []string, priority store.Priority, files ...string) *store.BacklogItem {
	return &store.BacklogItem{
		ID:       "MILLS-TEST-1",
		Labels:   labels,
		Priority: priority,
		Slices:   []store.Slice{{Name: "s1", Files: files}},
	}
}

func TestParsePolicy_AgentRouting(t *testing.T) {
	p := parseRoutingFixture(t)
	if !p.Pipeline.AgentRoutingEnabled() {
		t.Fatal("agent_routing should be enabled")
	}
	if got := len(p.Pipeline.AgentRouting.Rules); got != 4 {
		t.Fatalf("rules: got %d want 4", got)
	}
	r0 := p.Pipeline.AgentRouting.Rules[0]
	if r0.Route.Agent != "claude-code" || r0.Route.Model != "claude-opus-5" {
		t.Errorf("rules[0].route: got %+v", r0.Route)
	}
	if got := len(r0.Match.PathGlobs); got != 3 {
		t.Errorf("rules[0].match.path_globs: got %d want 3", got)
	}
}

// TestAgentRouting_InertWhenAbsent is the discipline guard: a policy with no
// agent_routing block must behave exactly like it did before routing shipped,
// including the agent/* label having no effect.
func TestAgentRouting_InertWhenAbsent(t *testing.T) {
	p := Default()
	if p.Pipeline.AgentRoutingEnabled() {
		t.Fatal("absent agent_routing block must be inert")
	}
	item := itemWith([]string{"agent/codex"}, store.P1, "internal/hud/frontend/App.svelte")
	got := p.ResolveAgentRoute("implement", item)
	if got.Agent != AgentDefault || got.DecidedBy != AgentDecidedByDefault {
		t.Errorf("inert routing: got %+v, want agent=%s decided_by=%s", got, AgentDefault, AgentDecidedByDefault)
	}
}

func TestAgentRouting_EnabledFalseDisablesBlock(t *testing.T) {
	p := parseRoutingFixture(t)
	p.Pipeline.AgentRouting.Enabled = boolPtr(false)
	if p.Pipeline.AgentRoutingEnabled() {
		t.Fatal("enabled: false must disable the block")
	}
	item := itemWith(nil, store.P1, "pkg/mills/policy.go")
	if got := p.ResolveAgentRoute("implement", item); got.DecidedBy != AgentDecidedByDefault {
		t.Errorf("decided_by: got %q want %q", got.DecidedBy, AgentDecidedByDefault)
	}
}

// TestAgentRouting_Precedence is the core contract: label > rule > stage_agents
// > default. The env break-glass sits above all of these and is asserted
// separately in cmd/loom-mills-operator (it lives in spawnRouteFor, not here).
func TestAgentRouting_Precedence(t *testing.T) {
	cases := []struct {
		name          string
		stageAgents   map[string]string
		stageModels   map[string]string
		item          *store.BacklogItem
		wantAgent     string
		wantModel     string
		wantDecidedBy string
	}{
		{
			name:          "label beats every rule",
			item:          itemWith([]string{"agent/codex"}, store.P1, "internal/hud/frontend/App.svelte"),
			wantAgent:     "codex",
			wantDecidedBy: AgentDecidedByLabel,
		},
		{
			name:          "rule beats stage_agents",
			stageAgents:   map[string]string{"implement": "gemini"},
			item:          itemWith(nil, store.P1, "internal/hud/frontend/App.svelte"),
			wantAgent:     "claude-code",
			wantModel:     "claude-opus-5",
			wantDecidedBy: AgentDecidedByRule(0),
		},
		{
			name:          "stage_agents beats default",
			stageAgents:   map[string]string{"implement": "gemini"},
			item:          itemWith(nil, store.P1, "docs/MILLS.md"),
			wantAgent:     "gemini",
			wantDecidedBy: AgentDecidedByStageAgents,
		},
		{
			name:          "default when nothing matches",
			item:          itemWith(nil, store.P1, "docs/MILLS.md"),
			wantAgent:     AgentDefault,
			wantDecidedBy: AgentDecidedByDefault,
		},
		{
			name:          "label wins over stage_agents too",
			stageAgents:   map[string]string{"implement": "gemini"},
			item:          itemWith([]string{"agent/claude-code"}, store.P1, "docs/MILLS.md"),
			wantAgent:     "claude-code",
			wantDecidedBy: AgentDecidedByLabel,
		},
		{
			name:          "modelless route on the same agent inherits stage_models",
			stageAgents:   map[string]string{"implement": "claude-code"},
			stageModels:   map[string]string{"implement": "claude-opus-5"},
			item:          itemWith([]string{"design"}, store.P1, "docs/MILLS.md"),
			wantAgent:     "claude-code",
			wantModel:     "claude-opus-5",
			wantDecidedBy: AgentDecidedByRule(1),
		},
		{
			name:          "modelless route that re-targets the vendor drops stage_models",
			stageAgents:   map[string]string{"implement": "claude-code"},
			stageModels:   map[string]string{"implement": "claude-opus-5"},
			item:          itemWith(nil, store.P0, "docs/MILLS.md"),
			wantAgent:     "codex",
			wantModel:     "",
			wantDecidedBy: AgentDecidedByRule(3),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := parseRoutingFixture(t)
			p.Pipeline.StageAgents = tc.stageAgents
			p.Pipeline.StageModels = tc.stageModels
			got := p.ResolveAgentRoute("implement", tc.item)
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

// TestAgentRouting_PathClassification covers the operator's stated heuristic:
// frontend-ish slice files reach claude-code, backend files reach codex, mixed
// items follow first-match-wins, and a sliceless item cannot panic or match.
func TestAgentRouting_PathClassification(t *testing.T) {
	cases := []struct {
		name          string
		item          *store.BacklogItem
		wantAgent     string
		wantModel     string
		wantDecidedBy string
	}{
		{
			name:          "frontend svelte",
			item:          itemWith(nil, store.P2, "internal/hud/frontend/src/lib/Panel.svelte"),
			wantAgent:     "claude-code",
			wantModel:     "claude-opus-5",
			wantDecidedBy: AgentDecidedByRule(0),
		},
		{
			name:          "backend go",
			item:          itemWith(nil, store.P2, "pkg/mills/pipeline/dispatcher.go", "cmd/loom-mills-operator/main.go"),
			wantAgent:     "codex",
			wantModel:     "gpt-5.6-sol",
			wantDecidedBy: AgentDecidedByRule(2),
		},
		{
			name: "mixed files follow first-match-wins",
			item: itemWith(nil, store.P2,
				"pkg/mills/agent_routing.go",
				"internal/hud/frontend/src/App.svelte"),
			wantAgent:     "claude-code",
			wantModel:     "claude-opus-5",
			wantDecidedBy: AgentDecidedByRule(0),
		},
		{
			name:          "sliceless item falls through to stage_agents",
			item:          &store.BacklogItem{ID: "MILLS-TEST-NOSLICE", Priority: store.P2},
			wantAgent:     "gemini",
			wantDecidedBy: AgentDecidedByStageAgents,
		},
		{
			name: "slice with empty file list falls through",
			item: &store.BacklogItem{
				ID:       "MILLS-TEST-EMPTYSLICE",
				Priority: store.P2,
				Slices:   []store.Slice{{Name: "s1"}, {Name: "s2", Files: []string{"", "  "}}},
			},
			wantAgent:     "gemini",
			wantDecidedBy: AgentDecidedByStageAgents,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := parseRoutingFixture(t)
			p.Pipeline.StageAgents = map[string]string{"implement": "gemini"}
			got := p.ResolveAgentRoute("implement", tc.item)
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

// TestAgentRouting_UnknownLabelIgnored guards the "never fail dispatch on a
// label" rule: an unrecognized agent/* suffix is surfaced for a warn log and
// resolution continues as if the label were absent.
func TestAgentRouting_UnknownLabelIgnored(t *testing.T) {
	p := parseRoutingFixture(t)
	item := itemWith([]string{"agent/gpt-9", "ui"}, store.P2, "internal/hud/frontend/App.svelte")
	got := p.ResolveAgentRoute("implement", item)
	if got.DecidedBy != AgentDecidedByRule(0) {
		t.Errorf("decided_by: got %q want %q", got.DecidedBy, AgentDecidedByRule(0))
	}
	if len(got.IgnoredLabels) != 1 || got.IgnoredLabels[0] != "agent/gpt-9" {
		t.Errorf("ignored labels: got %v want [agent/gpt-9]", got.IgnoredLabels)
	}
}

// TestAgentRouting_ConflictingLabelsAreAmbiguous: GitLab returns labels in its
// own order, so honouring the first of two conflicting agent/* labels would make
// the harness depend on ordering the operator never chose. Both are ignored and
// the item falls through to the rules.
func TestAgentRouting_ConflictingLabelsAreAmbiguous(t *testing.T) {
	p := parseRoutingFixture(t)
	p.Pipeline.StageAgents = map[string]string{"implement": "gemini"}
	item := itemWith([]string{"agent/codex", "agent/claude-code"}, store.P2, "pkg/mills/policy.go")
	got := p.ResolveAgentRoute("implement", item)
	if got.DecidedBy != AgentDecidedByRule(2) || got.Agent != "codex" {
		t.Errorf("ambiguous labels must fall through to the rules, got %+v", got)
	}
	if len(got.IgnoredLabels) != 2 {
		t.Errorf("ignored labels: got %v want both agent/* labels", got.IgnoredLabels)
	}
}

// TestAgentRouting_DuplicateSameLabelStillResolves: repeating the SAME harness
// is not a conflict.
func TestAgentRouting_DuplicateSameLabelStillResolves(t *testing.T) {
	p := parseRoutingFixture(t)
	item := itemWith([]string{"agent/codex", "AGENT/codex"}, store.P2, "docs/x.md")
	got := p.ResolveAgentRoute("implement", item)
	if got.Agent != "codex" || got.DecidedBy != AgentDecidedByLabel {
		t.Errorf("duplicate identical labels: got %+v want codex/label", got)
	}
	if len(got.IgnoredLabels) != 0 {
		t.Errorf("ignored labels: got %v want none", got.IgnoredLabels)
	}
}

// TestAgentRouting_PathNormalization: slice file lists are LLM-authored, so
// "./pkg/x.go" and "/pkg/x.go" must match "pkg/**" — a silent fall-through here
// is indistinguishable from "no rule matched", the hardest routing bug to see.
func TestAgentRouting_PathNormalization(t *testing.T) {
	for _, raw := range []string{"pkg/mills/policy.go", "./pkg/mills/policy.go", "/pkg/mills/policy.go", "pkg/./mills/policy.go"} {
		t.Run(raw, func(t *testing.T) {
			p := parseRoutingFixture(t)
			p.Pipeline.StageAgents = map[string]string{"implement": "gemini"}
			got := p.ResolveAgentRoute("implement", itemWith(nil, store.P2, raw))
			if got.Agent != "codex" || got.DecidedBy != AgentDecidedByRule(2) {
				t.Errorf("path %q: got %+v want codex via rule:2", raw, got)
			}
		})
	}
}

// TestAgentRouting_LabelOnlyMode: `enabled: true` with zero rules is legal and
// turns on the agent/* label override alone.
func TestAgentRouting_LabelOnlyMode(t *testing.T) {
	const body = `
version: 2
pipeline:
  stage_agents:
    implement: gemini
  agent_routing:
    enabled: true
`
	p, err := ParsePolicy([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !p.Pipeline.AgentRoutingEnabled() {
		t.Fatal("enabled: true with no rules must still enable label routing")
	}
	got := p.ResolveAgentRoute("implement", itemWith([]string{"agent/codex"}, store.P2, "pkg/x.go"))
	if got.Agent != "codex" || got.DecidedBy != AgentDecidedByLabel {
		t.Errorf("label-only mode: got %+v want codex/label", got)
	}
	// With no rules, an unlabelled item falls straight through.
	base := p.ResolveAgentRoute("implement", itemWith(nil, store.P2, "pkg/x.go"))
	if base.Agent != "gemini" || base.DecidedBy != AgentDecidedByStageAgents {
		t.Errorf("label-only mode fall-through: got %+v want gemini/stage_agents", base)
	}
}

// TestAgentRouted_OnlyClaimsLabelAndRule is the inertness predicate's contract:
// the stage_agents / default / env rungs must NOT read as "routed", or routing's
// side effects (events, artifacts) leak into deployments that never opted in.
func TestAgentRouted_OnlyClaimsLabelAndRule(t *testing.T) {
	cases := []struct {
		decidedBy string
		want      bool
	}{
		{AgentDecidedByLabel, true},
		{AgentDecidedByRule(0), true},
		{AgentDecidedByRule(17), true},
		{AgentDecidedByStageAgents, false},
		{AgentDecidedByDefault, false},
		{AgentDecidedByEnv, false},
		{"", false},
	}
	for _, tc := range cases {
		if got := AgentRouted(AgentDecision{DecidedBy: tc.decidedBy}); got != tc.want {
			t.Errorf("AgentRouted(%q): got %v want %v", tc.decidedBy, got, tc.want)
		}
	}
}

func TestAgentRouting_LabelCaseInsensitive(t *testing.T) {
	p := parseRoutingFixture(t)
	item := itemWith([]string{" Agent/CODEX "}, store.P2, "internal/hud/frontend/App.svelte")
	if got := p.ResolveAgentRoute("implement", item); got.Agent != "codex" {
		t.Errorf("agent: got %q want codex", got.Agent)
	}
}

// TestAgentRouting_OnlyAgentConfigurableStages keeps routing out of the stages
// that consume no harness selection (research/tests) and away from gate/judge
// model selection, which is a separate system.
func TestAgentRouting_OnlyAgentConfigurableStages(t *testing.T) {
	p := parseRoutingFixture(t)
	item := itemWith([]string{"agent/codex"}, store.P2, "pkg/mills/policy.go")
	for _, stage := range []string{"plan_slice", "implement", "pr_self_review"} {
		if got := p.ResolveAgentRoute(stage, item); got.Agent != "codex" {
			t.Errorf("stage %q: got %q want codex", stage, got.Agent)
		}
	}
	for _, stage := range []string{"research", "tests", "mr", "merge"} {
		got := p.ResolveAgentRoute(stage, item)
		if got.DecidedBy != AgentDecidedByDefault || got.Agent != AgentDefault {
			t.Errorf("stage %q: routing must not apply, got %+v", stage, got)
		}
	}
}

func TestAgentRouting_NilSafe(t *testing.T) {
	var p *Policy
	if got := p.ResolveAgentRoute("implement", nil); got.Agent != AgentDefault {
		t.Errorf("nil policy: got %+v", got)
	}
	p2 := parseRoutingFixture(t)
	if got := p2.ResolveAgentRoute("implement", nil); got.Agent != AgentDefault {
		t.Errorf("nil item: got %+v", got)
	}
}

func TestAgentRouting_Validate_Errors(t *testing.T) {
	cases := []struct {
		name  string
		patch func(*Policy)
		want  string
	}{
		{
			name: "unknown agent",
			patch: func(p *Policy) {
				p.Pipeline.AgentRouting.Rules[0].Route.Agent = "gpt-9"
			},
			want: "is not a recognized agent",
		},
		{
			name: "missing agent",
			patch: func(p *Policy) {
				p.Pipeline.AgentRouting.Rules[0].Route.Agent = ""
			},
			want: "is not a recognized agent",
		},
		{
			name: "empty match",
			patch: func(p *Policy) {
				p.Pipeline.AgentRouting.Rules[0].Match = AgentRoutingMatch{}
			},
			want: "must set at least one of labels, priority, path_globs",
		},
		{
			name: "invalid glob",
			patch: func(p *Policy) {
				p.Pipeline.AgentRouting.Rules[0].Match.PathGlobs = []string{"internal/[hud/**"}
			},
			want: "is not a valid glob",
		},
		{
			name: "empty glob",
			patch: func(p *Policy) {
				p.Pipeline.AgentRouting.Rules[0].Match.PathGlobs = []string{""}
			},
			want: "path_globs[0] is empty",
		},
		{
			name: "empty label",
			patch: func(p *Policy) {
				p.Pipeline.AgentRouting.Rules[1].Match.Labels = []string{"design", " "}
			},
			want: "labels[1] is empty",
		},
		{
			name: "bad priority band",
			patch: func(p *Policy) {
				p.Pipeline.AgentRouting.Rules[3].Match.Priority = []string{"P9"}
			},
			want: "must be one of P0..P3",
		},
		{
			name: "malformed model token",
			patch: func(p *Policy) {
				p.Pipeline.AgentRouting.Rules[0].Route.Model = "gpt 5.6 sol"
			},
			want: "is not a valid model id",
		},
		{
			name: "too many rules",
			patch: func(p *Policy) {
				rules := make([]AgentRoutingRule, 0, agentRoutingMaxRules+1)
				for i := 0; i <= agentRoutingMaxRules; i++ {
					rules = append(rules, AgentRoutingRule{
						Match: AgentRoutingMatch{Labels: []string{"x"}},
						Route: AgentRoute{Agent: "codex"},
					})
				}
				p.Pipeline.AgentRouting.Rules = rules
			},
			want: "exceeds the max of",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := parseRoutingFixture(t)
			tc.patch(p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// TestAgentRouting_ValidatesWhileDisabled: a broken table must be rejected at
// load even behind enabled: false, so flipping the flag can never be the moment
// an invalid glob first surfaces.
func TestAgentRouting_ValidatesWhileDisabled(t *testing.T) {
	p := parseRoutingFixture(t)
	p.Pipeline.AgentRouting.Enabled = boolPtr(false)
	p.Pipeline.AgentRouting.Rules[0].Match.PathGlobs = []string{"internal/[hud/**"}
	if err := p.Validate(); err == nil {
		t.Fatal("expected disabled-but-invalid routing table to fail validation")
	}
}
