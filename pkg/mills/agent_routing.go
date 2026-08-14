// Per-ITEM agent routing: the layer that lets one fleet run Claude Code and
// Codex implementers simultaneously, choosing the harness+model per backlog
// item instead of globally per stage.
//
// pipeline.stage_agents (policy.go) answers "which harness runs pr_self_review",
// a question about the STAGE. This file answers "which harness runs THIS item",
// a question about the WORK: UI/design/frontend slices belong on claude-code,
// backend/systems/infra slices on codex. Both maps feed the same SpawnRequest;
// this one wins where it applies.

package mills

import (
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// agentRoutingMaxRules bounds the rule list. First-match-wins means every
// dispatch walks the list until it hits, so an unbounded list would put an
// operator typo on the hot path of every spawn. No real routing table needs
// more than a handful of rules.
const agentRoutingMaxRules = 64

// AgentRoutingLabelPrefix marks the explicit per-item harness override. A
// backlog item labelled "agent/codex" runs on codex regardless of what the
// rules would have chosen — the operator's escape hatch for the item the
// heuristics get wrong. The suffix must be a member of StageAgentValuesValid;
// an unknown suffix is IGNORED (surfaced via AgentDecision.IgnoredLabels), never
// fatal, because a label is authored by humans and issue importers and must not
// be able to wedge dispatch.
const AgentRoutingLabelPrefix = "agent/"

// decided_by tokens recorded on every routing decision — the closed vocabulary,
// kept together even though the env rung is applied by the operator rather than
// by ResolveAgentRoute. An operator answering "why did this item go to codex?"
// reads exactly one of these off the dispatch event; AgentDecidedByRule carries
// the rule index so the answer names the specific policy line.
const (
	// AgentDecidedByEnv marks the LOOM_MILLS_SPAWN_AGENT break-glass. Applied
	// in cmd/loom-mills-operator.spawnRouteFor, above everything here.
	AgentDecidedByEnv         = "env"
	AgentDecidedByLabel       = "label"
	AgentDecidedByStageAgents = "stage_agents"
	AgentDecidedByDefault     = "default"
)

// AgentDecidedByRule renders the decided_by token for the rule at index i.
func AgentDecidedByRule(i int) string { return "rule:" + strconv.Itoa(i) }

// agentDecidedByRulePrefix is the AgentDecidedByRule token's stable prefix.
const agentDecidedByRulePrefix = "rule:"

// AgentRouted reports whether per-item routing — an agent/* label or an
// agent_routing rule — actually claimed this dispatch, as opposed to it falling
// through to the pre-routing stage_agents / default rungs. Callers use it to
// keep routing's side effects (dispatch events, stage artifacts) OFF for
// deployments that never opted in: those rungs always produce a non-empty
// DecidedBy, so "DecidedBy != \"\"" is not the inertness test it looks like.
func AgentRouted(d AgentDecision) bool {
	return d.DecidedBy == AgentDecidedByLabel ||
		strings.HasPrefix(d.DecidedBy, agentDecidedByRulePrefix)
}

// AgentDecision is the resolved harness+model for one stage dispatch of one
// item, plus the provenance that explains it. Agent is never empty (it bottoms
// out at AgentDefault); Model may be empty, which means "no override — let the
// vendor CLI pick its own default".
type AgentDecision struct {
	Agent     string
	Model     string
	DecidedBy string
	// IgnoredLabels holds the item's malformed agent/* labels. Populated so
	// the caller can warn once at dispatch; routing itself proceeds as if the
	// labels were absent.
	IgnoredLabels []string
}

// AgentRoutingPolicy routes individual backlog items to a harness+model by
// label, priority, and slice file paths.
//
//	pipeline:
//	  agent_routing:
//	    enabled: true
//	    rules:
//	      - match:
//	          path_globs: ["internal/hud/frontend/**", "**/*.svelte"]
//	        route: {agent: claude-code, model: claude-opus-5}
//	      - match:
//	          path_globs: ["pkg/**", "cmd/**"]
//	        route: {agent: codex, model: gpt-5.6-sol}
//
// The whole block is INERT when absent: no rules and no Enabled key means the
// agent/* label override is off too, so an operator who has not opted in gets
// byte-identical stage_agents behavior.
type AgentRoutingPolicy struct {
	// Enabled gates the block. *bool so an omitted key defaults to ON when
	// rules are present (a policy that bothered to write rules meant them);
	// opt out without deleting the table with `enabled: false`.
	Enabled *bool `yaml:"enabled,omitempty"`
	// Rules are evaluated in declaration order; the FIRST match wins. Order
	// is the operator's only tie-break, so a mixed-file item lands wherever
	// its earliest matching rule points.
	Rules []AgentRoutingRule `yaml:"rules,omitempty"`
}

// AgentRoutingRule pairs a predicate with the harness+model it selects.
type AgentRoutingRule struct {
	Match AgentRoutingMatch `yaml:"match"`
	Route AgentRoute        `yaml:"route"`
}

// AgentRoutingMatch predicates one rule. Every POPULATED criterion must hold
// (AND across criteria) and each is satisfied by any one of its entries (OR
// within a criterion), so `{labels: [ui], path_globs: ["**/*.svelte"]}` means
// "labelled ui AND touching a svelte file". An empty match is rejected at
// policy load rather than treated as match-everything.
type AgentRoutingMatch struct {
	// Labels match the item's labels case-insensitively.
	Labels []string `yaml:"labels,omitempty"`
	// Priority matches the item's priority band (P0..P3).
	Priority []string `yaml:"priority,omitempty"`
	// PathGlobs match against the union of the item's slice file paths using
	// doublestar (the same glob dialect as pipeline.protected_paths and squad
	// path classes), so "**" crosses directory separators.
	PathGlobs []string `yaml:"path_globs,omitempty"`
}

// AgentRoute is the destination half of a rule.
type AgentRoute struct {
	// Agent is required and must be a member of StageAgentValuesValid.
	Agent string `yaml:"agent"`
	// Model is the optional vendor-native LLM id (e.g. "gpt-5.6-sol") pinned
	// for items this rule claims. Empty means "vendor default" — see
	// Policy.ResolveAgentRoute for how it interacts with stage_models.
	Model string `yaml:"model,omitempty"`
}

// present reports whether the operator wrote the block at all. Used to keep an
// absent block fully inert rather than defaulting it on.
func (a AgentRoutingPolicy) present() bool {
	return a.Enabled != nil || len(a.Rules) > 0
}

// AgentRoutingEnabled resolves the *bool with default-ON-when-present
// semantics. Callers must use this instead of reading the field directly so the
// inert-when-absent rule stays in one place.
func (p PipelinePolicy) AgentRoutingEnabled() bool {
	if !p.AgentRouting.present() {
		return false
	}
	return p.AgentRouting.Enabled == nil || *p.AgentRouting.Enabled
}

// ResolveAgentRoute resolves the effective harness+model for one dispatch of
// item at stage. Precedence, highest first:
//
//  1. the item's `agent/<id>` label — the explicit per-item override.
//  2. the first matching pipeline.agent_routing rule.
//  3. pipeline.stage_agents[stage] — the per-stage map.
//  4. AgentDefault ("claude-code").
//
// The LOOM_MILLS_SPAWN_AGENT / LOOM_MILLS_SPAWN_MODEL env break-glass sits
// ABOVE all four and is deliberately not handled here: env is read once at
// operator start (pod env doesn't hot-reload) and applied by
// cmd/loom-mills-operator.spawnRouteFor, which owns the whole precedence chain.
//
// Model resolution follows the agent, because a stage_models pin names a
// vendor-native id that is meaningless to a different vendor:
//   - a route's own model always wins;
//   - a route that keeps the baseline agent inherits stage_models[stage];
//   - a route that RE-TARGETS the vendor drops stage_models and returns empty,
//     letting the new vendor's CLI apply its own default rather than handing
//     codex a claude-* id.
//
// Nil-safe on the receiver and on item; both yield the stage-level baseline.
func (p *Policy) ResolveAgentRoute(stage string, item *store.BacklogItem) AgentDecision {
	baseline := AgentDecision{Agent: AgentDefault, DecidedBy: AgentDecidedByDefault}
	if p == nil {
		return baseline
	}
	if a := p.AgentForStage(stage); a != "" {
		baseline.Agent = a
		baseline.DecidedBy = AgentDecidedByStageAgents
	}
	baseline.Model = p.ModelForStage(stage)
	if item == nil || !p.Pipeline.AgentRoutingEnabled() {
		return baseline
	}
	// Routing only applies to the SpawnWorker-driven stages — the same set
	// stage_agents configures. research (WeaverWorker) and tests
	// (DevboxWorker) consume no harness selection, and gate/judge model
	// choice is a separate system this must not reach into.
	if _, ok := StageAgentKeysValid[stage]; !ok {
		return baseline
	}

	labelAgent, ignored := agentLabelOverride(item.Labels)
	baseline.IgnoredLabels = ignored
	if labelAgent != "" {
		return routeOnto(baseline, AgentRoute{Agent: labelAgent}, AgentDecidedByLabel)
	}
	paths := slicePathUnion(item)
	for i, rule := range p.Pipeline.AgentRouting.Rules {
		if rule.Match.matches(item, paths) {
			return routeOnto(baseline, rule.Route, AgentDecidedByRule(i))
		}
	}
	return baseline
}

// routeOnto applies a route to the stage-level baseline, carrying the
// model-follows-agent rule documented on ResolveAgentRoute.
func routeOnto(baseline AgentDecision, route AgentRoute, decidedBy string) AgentDecision {
	d := baseline
	d.Agent = route.Agent
	d.DecidedBy = decidedBy
	switch {
	case route.Model != "":
		d.Model = route.Model
	case route.Agent != baseline.Agent:
		d.Model = ""
	}
	return d
}

// agentLabelOverride returns the harness named by the item's agent/* label,
// plus every agent/* label the override could not honour. An unusable label
// never fails the dispatch — resolution continues as if it were absent.
//
// Two labels naming DIFFERENT valid harnesses (agent/codex + agent/claude-code)
// are ambiguous: GitLab returns labels in its own order, so honouring the first
// would make the harness depend on label ordering the operator never chose.
// Both are ignored and the item falls through to the rules.
func agentLabelOverride(labels []string) (string, []string) {
	var chosen []string
	var ignored []string
	for _, raw := range labels {
		norm := strings.ToLower(strings.TrimSpace(raw))
		if !strings.HasPrefix(norm, AgentRoutingLabelPrefix) {
			continue
		}
		want := strings.TrimSpace(strings.TrimPrefix(norm, AgentRoutingLabelPrefix))
		if _, ok := StageAgentValuesValid[want]; !ok {
			ignored = append(ignored, raw)
			continue
		}
		if !containsString(chosen, want) {
			chosen = append(chosen, want)
		}
	}
	switch len(chosen) {
	case 0:
		return "", ignored
	case 1:
		return chosen[0], ignored
	default:
		// Ambiguous — surface every agent/* label so the warn log names them.
		for _, raw := range labels {
			norm := strings.ToLower(strings.TrimSpace(raw))
			if strings.HasPrefix(norm, AgentRoutingLabelPrefix) && !containsString(ignored, raw) {
				ignored = append(ignored, raw)
			}
		}
		return "", ignored
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// slicePathUnion flattens the item's slice file lists into a deduplicated set
// of repo-relative paths — the surface path_globs match against. A sliceless
// item yields nil, so its path rules simply never match and it falls through to
// stage_agents.
//
// Paths are normalized first: slice file lists are LLM-authored, so "./pkg/x.go"
// and "/pkg/x.go" both show up in practice and neither would match "pkg/**".
// Silently falling through on a leading "./" would look exactly like "no rule
// matched", which is the hardest routing bug to see.
func slicePathUnion(item *store.BacklogItem) []string {
	if item == nil || len(item.Slices) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	var paths []string
	for _, sl := range item.Slices {
		for _, f := range sl.Files {
			f = normalizeSlicePath(f)
			if f == "" {
				continue
			}
			if _, dup := seen[f]; dup {
				continue
			}
			seen[f] = struct{}{}
			paths = append(paths, f)
		}
	}
	return paths
}

// normalizeSlicePath reduces a slice file entry to the repo-relative form the
// glob patterns are written against. Returns "" for entries that carry no path.
func normalizeSlicePath(f string) string {
	f = strings.TrimSpace(f)
	if f == "" {
		return ""
	}
	f = path.Clean(f)
	f = strings.TrimPrefix(f, "/")
	if f == "." || f == ".." {
		return ""
	}
	return f
}

// matches evaluates the predicate. Criteria AND, entries within a criterion OR.
func (m AgentRoutingMatch) matches(item *store.BacklogItem, paths []string) bool {
	if m.isEmpty() || item == nil {
		return false
	}
	if len(m.Labels) > 0 && !anyLabelMatches(item.Labels, m.Labels) {
		return false
	}
	if len(m.Priority) > 0 && !anyPriorityMatches(item.Priority, m.Priority) {
		return false
	}
	if len(m.PathGlobs) > 0 && !anyPathMatches(paths, m.PathGlobs) {
		return false
	}
	return true
}

func (m AgentRoutingMatch) isEmpty() bool {
	return len(m.Labels) == 0 && len(m.Priority) == 0 && len(m.PathGlobs) == 0
}

func anyLabelMatches(have, want []string) bool {
	set := make(map[string]struct{}, len(have))
	for _, l := range have {
		set[strings.ToLower(strings.TrimSpace(l))] = struct{}{}
	}
	for _, w := range want {
		if _, ok := set[strings.ToLower(strings.TrimSpace(w))]; ok {
			return true
		}
	}
	return false
}

func anyPriorityMatches(have store.Priority, want []string) bool {
	got := strings.ToUpper(strings.TrimSpace(string(have)))
	for _, w := range want {
		if got == strings.ToUpper(strings.TrimSpace(w)) {
			return true
		}
	}
	return false
}

func anyPathMatches(paths, globs []string) bool {
	for _, path := range paths {
		for _, g := range globs {
			// Patterns are validated at policy load, so a Match error here
			// can only come from a pattern that slipped past validation;
			// treat it as no-match rather than routing on a broken glob.
			if ok, err := doublestar.Match(g, path); err == nil && ok {
				return true
			}
		}
	}
	return false
}

// validateAgentRouting enforces the rule contract at policy LOAD time — closed
// agent vocabulary, non-empty match, valid priority bands, and compilable
// globs — so a malformed table is rejected on startup/hot-reload instead of
// silently mis-routing (or panicking) on the dispatch path. Rules are validated
// even when the block is disabled: a disabled-but-broken table would otherwise
// blow up the moment someone flips enabled.
func validateAgentRouting(a AgentRoutingPolicy) error {
	if len(a.Rules) > agentRoutingMaxRules {
		return fmt.Errorf("pipeline.agent_routing.rules: %d rules exceeds the max of %d", len(a.Rules), agentRoutingMaxRules)
	}
	for i, r := range a.Rules {
		if r.Match.isEmpty() {
			return fmt.Errorf("pipeline.agent_routing.rules[%d].match must set at least one of labels, priority, path_globs", i)
		}
		for j, l := range r.Match.Labels {
			if strings.TrimSpace(l) == "" {
				return fmt.Errorf("pipeline.agent_routing.rules[%d].match.labels[%d] is empty", i, j)
			}
		}
		for j, pr := range r.Match.Priority {
			switch strings.ToUpper(strings.TrimSpace(pr)) {
			case string(store.P0), string(store.P1), string(store.P2), string(store.P3):
			default:
				return fmt.Errorf("pipeline.agent_routing.rules[%d].match.priority[%d]: must be one of P0..P3, got %q", i, j, pr)
			}
		}
		for j, g := range r.Match.PathGlobs {
			if strings.TrimSpace(g) == "" {
				return fmt.Errorf("pipeline.agent_routing.rules[%d].match.path_globs[%d] is empty", i, j)
			}
			if !doublestar.ValidatePattern(g) {
				return fmt.Errorf("pipeline.agent_routing.rules[%d].match.path_globs[%d] %q is not a valid glob", i, j, g)
			}
		}
		if _, ok := StageAgentValuesValid[r.Route.Agent]; !ok {
			return fmt.Errorf("pipeline.agent_routing.rules[%d].route.agent: %q is not a recognized agent (allowed: claude-code, codex, gemini)", i, r.Route.Agent)
		}
		if r.Route.Model != "" && !validModelToken(r.Route.Model) {
			return fmt.Errorf("pipeline.agent_routing.rules[%d].route.model: %q is not a valid model id (expect a vendor-native token like gpt-5.6-sol)", i, r.Route.Model)
		}
	}
	return nil
}
