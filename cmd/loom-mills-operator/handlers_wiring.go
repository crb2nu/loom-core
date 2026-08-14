package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/council"
)

// The /wiring endpoint answers "which model/backend does every operationally-
// critical Mills surface actually use?" — the resolved (post env-precedence,
// post policy) routing the operator logs at startup but the HUD Mills Overview
// otherwise hides. The values are captured ONCE at wiring time into a
// WiringSnapshot (buildWiringSnapshot, called from main.go adjacent to the
// wiring log lines) and served verbatim: the routing only changes on restart,
// so a static snapshot can never disagree with the reconciler's live behavior.
//
// Contract invariants the HUD Overview builds against (buildWiringSnapshot must
// preserve them): every array field is non-nil ([] never null) and no secret,
// key, or credentialed URL ever appears — only backends, model ids, and booleans.

// judgeWiring is the resolved rubric-judge (LLM-gate) client wiring.
type judgeWiring struct {
	// Backend is "flexinfer" (FlexInfer proxy), "litellm" (LiteLLM gateway),
	// or "none" when no judge client is configured (LLM gates disabled).
	Backend string `json:"backend"`
	// Model is the effective primary judge model id (empty when disabled).
	Model string `json:"model"`
	// Fallbacks is the resolved ordered degrade chain (never null).
	Fallbacks []string `json:"fallbacks"`
	// MaxTokens is the effective rubric-judge completion budget.
	MaxTokens int `json:"max_tokens"`
	// RegistryFallbacksDisabled is true for a LiteLLM-gateway judge (its
	// fallbacks are backend-local env ids, not the aimodels role chain).
	RegistryFallbacksDisabled bool `json:"registry_fallbacks_disabled"`
}

// weaverWiring is the resolved research/weaver client wiring.
type weaverWiring struct {
	Backend   string   `json:"backend"`
	Model     string   `json:"model"`
	Fallbacks []string `json:"fallbacks"`
	// MaxTokens is the effective research/weaver completion budget
	// (FLEXINFER_WEAVER_MAX_TOKENS, default 1024; >=4096 for reasoning models).
	MaxTokens int `json:"max_tokens"`
}

// councilLensWiring is one reviewer lens as buildCouncilRunner bound it. A lens
// degraded to the deterministic fake fallback (FlexInfer unconfigured, or a
// litellm lens with no gateway) reports Backend "fake" honestly.
type councilLensWiring struct {
	Name    string `json:"name"`
	Backend string `json:"backend"`
	Model   string `json:"model"`
}

// councilWiring is the resolved council ensemble: the contradiction judge, the
// editor, and every reviewer lens.
type councilWiring struct {
	JudgeBackend  string              `json:"judge_backend"`
	JudgeModel    string              `json:"judge_model"`
	EditorBackend string              `json:"editor_backend"`
	EditorModel   string              `json:"editor_model"`
	Lenses        []councilLensWiring `json:"lenses"`
}

// stageWiring is the resolved spawn agent + model for one spawn-driven pipeline
// stage. Source names the precedence level that won the AGENT selection —
// "env" (LOOM_MILLS_SPAWN_AGENT break-glass), "policy" (pipeline.stage_agents),
// or "default" (mills.AgentDefault). Model follows the same precedence but is
// empty on the default path ("keep the vendor CLI default").
type stageWiring struct {
	Stage  string `json:"stage"`
	Agent  string `json:"agent"`
	Model  string `json:"model"`
	Source string `json:"source"`
}

// spawnWiring reports the global spawn defaults + whether the break-glass env
// overrides are engaged.
type spawnWiring struct {
	DefaultAgent     string `json:"default_agent"`
	EnvAgentOverride bool   `json:"env_agent_override"`
	EnvModelOverride bool   `json:"env_model_override"`
}

// gatesWiring reports LLM-gate enablement + the dissent tiebreaker backend.
type gatesWiring struct {
	LLMGatesEnabled bool   `json:"llm_gates_enabled"`
	Tiebreaker      string `json:"tiebreaker"`
}

// litellmWiring reports whether the LiteLLM gateway is configured (URL present;
// never the URL itself — it may carry credentials).
type litellmWiring struct {
	Configured bool `json:"configured"`
}

// policyWiring reports the autonomy kill-switch state + an optional cheap
// policy checksum (omitted when it cannot be computed).
type policyWiring struct {
	AutonomyEnabled bool   `json:"autonomy_enabled"`
	PolicyChecksum  string `json:"policy_checksum,omitempty"`
}

// WiringSnapshot is the full resolved model-wiring config the /wiring endpoint
// serves. Built once at startup by buildWiringSnapshot.
type WiringSnapshot struct {
	GeneratedAt time.Time     `json:"generated_at"`
	Judge       judgeWiring   `json:"judge"`
	Weaver      weaverWiring  `json:"weaver"`
	Council     councilWiring `json:"council"`
	Stages      []stageWiring `json:"stages"`
	Spawn       spawnWiring   `json:"spawn"`
	Gates       gatesWiring   `json:"gates"`
	LiteLLM     litellmWiring `json:"litellm"`
	Policy      policyWiring  `json:"policy"`
}

// spawnStages is the ordered set of spawn-driven pipeline stages the wiring log
// line (buildDispatcher) enumerates. Kept in lockstep with the SpawnWorker
// routes so /wiring reports exactly the stages the operator spawns.
var spawnStages = []string{"plan_slice", "implement", "pr_self_review"}

// wiringInputs carries the already-resolved wiring the operator computed at
// startup. main.go fills it from the SAME variables that feed the wiring log
// lines, so the snapshot can never drift from what is logged; buildWiringSnapshot
// is otherwise pure (only os-independent — env is read in main.go and passed in
// as booleans) so the population logic is unit-testable.
type wiringInputs struct {
	cfg    Config
	policy *mills.Policy

	// Resolved LLM clients (any may be nil). flexClient nil ⇒ FlexInfer
	// unconfigured (council degrades to fakes; judge/weaver stages stub).
	flexClient   *clients.FlexInferClient
	judgeClient  *clients.FlexInferClient // resolveMillsJudgeClient result; nil ⇒ gates off
	weaverClient *clients.FlexInferClient // resolveMillsWeaverClient result

	// litellmConfigured is (buildLiteLLMClient != nil): drives council-lens
	// degradation AND the top-level litellm.configured flag.
	litellmConfigured bool

	// councilJudgeModel is the explicit gateway model the council contradiction
	// judge dials on the litellm backend ("" on the flexinfer default).
	councilJudgeModel string

	// agentFor/modelFor are the effective per-stage resolvers (env > policy >
	// default). Never nil in production; buildWiringSnapshot guards nil.
	agentFor func(stage string) string
	modelFor func(stage string) string

	// spawnEnvAgent/spawnEnvModel report whether the break-glass env overrides
	// are set (LOOM_MILLS_SPAWN_AGENT / LOOM_MILLS_SPAWN_MODEL). Read in main.go
	// so the builder stays pure; also decide stage source attribution.
	spawnDefaultAgent string
	spawnEnvAgent     bool
	spawnEnvModel     bool

	// gateTiebreaker is "anthropic" (dissent tiebreaker wired) or "none".
	gateTiebreaker string

	now time.Time
}

// buildWiringSnapshot assembles the resolved WiringSnapshot from the operator's
// startup wiring. Pure: no env or clock reads (main.go passes now + env booleans
// in) so it is deterministic under test. Every array is initialized non-nil to
// honor the never-null wire contract.
func buildWiringSnapshot(in wiringInputs) WiringSnapshot {
	flexConfigured := in.flexClient != nil

	// --- judge (rubric / LLM gates) ---
	judgeBackend := "none"
	llmGatesEnabled := in.judgeClient != nil
	if in.judgeClient != nil {
		if in.judgeClient == in.flexClient {
			judgeBackend = "flexinfer"
		} else {
			judgeBackend = "litellm"
		}
	}
	judge := judgeWiring{
		Backend:                   judgeBackend,
		Model:                     in.judgeClient.JudgeModel(),
		Fallbacks:                 normalizeStrings(in.judgeClient.JudgeModelFallbacks()),
		MaxTokens:                 clients.JudgeMaxTokensFromEnv(),
		RegistryFallbacksDisabled: in.judgeClient.RegistryFallbacksDisabled(),
	}

	// --- weaver (research) ---
	weaver := weaverWiring{
		Backend:   weaverBackendLabel(in.cfg),
		Model:     in.weaverClient.WeaverModel(),
		Fallbacks: normalizeStrings(in.weaverClient.WeaverModelFallbacks()),
		MaxTokens: clients.WeaverMaxTokensFromEnv(),
	}

	// --- council ensemble ---
	councilW := buildCouncilWiring(in, flexConfigured)

	// --- spawn stages ---
	stages := make([]stageWiring, 0, len(spawnStages))
	for _, s := range spawnStages {
		agent := in.spawnDefaultAgent
		if in.agentFor != nil {
			agent = in.agentFor(s)
		}
		model := ""
		if in.modelFor != nil {
			model = in.modelFor(s)
		}
		stages = append(stages, stageWiring{
			Stage:  s,
			Agent:  agent,
			Model:  model,
			Source: stageSource(in, s),
		})
	}

	return WiringSnapshot{
		GeneratedAt: in.now.UTC(),
		Judge:       judge,
		Weaver:      weaver,
		Council:     councilW,
		Stages:      stages,
		Spawn: spawnWiring{
			DefaultAgent:     in.spawnDefaultAgent,
			EnvAgentOverride: in.spawnEnvAgent,
			EnvModelOverride: in.spawnEnvModel,
		},
		Gates: gatesWiring{
			LLMGatesEnabled: llmGatesEnabled,
			Tiebreaker:      orString(in.gateTiebreaker, "none"),
		},
		LiteLLM: litellmWiring{Configured: in.litellmConfigured},
		Policy: policyWiring{
			AutonomyEnabled: in.policy.IsEnabled(),
			PolicyChecksum:  policyChecksum(in.policy),
		},
	}
}

// buildCouncilWiring mirrors the per-participant backend binding buildCouncilRunner
// performs so /wiring reports the SAME effective backends. Kept adjacent in
// intent to that function: any change to the lens/editor/judge binding rule
// there must update the mirror here (both are covered by tests).
func buildCouncilWiring(in wiringInputs, flexConfigured bool) councilWiring {
	lenses := council.LensesFromPolicy(in.policy)
	out := councilWiring{Lenses: make([]councilLensWiring, 0, len(lenses))}
	for _, l := range lenses {
		backend := l.Backend
		switch {
		case !flexConfigured:
			// FlexInfer unconfigured ⇒ every reviewer is a FakeReviewer.
			backend = "fake"
		case l.Backend == "litellm" && !in.litellmConfigured:
			// A litellm lens with no gateway degrades to the fake reviewer
			// (visible-misconfiguration contract) instead of 404ing.
			backend = "fake"
		}
		out.Lenses = append(out.Lenses, councilLensWiring{
			Name:    l.Name,
			Backend: backend,
			Model:   l.Model,
		})
	}

	// Editor: the policy-configured backend/model when a real backend exists;
	// "fake" (FakeEditor) when FlexInfer is unconfigured. Runtime per-call
	// degradation (a missing OpenAI/Anthropic key falling back to flexinfer) is
	// a logged operational detail, not part of the static wiring.
	var editor mills.CouncilAgent
	if in.policy != nil {
		editor = in.policy.Council.Ensemble.Editor
	}
	out.EditorModel = editor.Model
	if flexConfigured {
		out.EditorBackend = editor.Backend
	} else {
		out.EditorBackend = "fake"
	}

	// Contradiction judge: shares the rubric-judge backend selection.
	switch {
	case !flexConfigured:
		out.JudgeBackend = "fake"
		out.JudgeModel = ""
	case in.judgeClient != nil && in.judgeClient != in.flexClient:
		out.JudgeBackend = "litellm"
		out.JudgeModel = in.councilJudgeModel
	default:
		out.JudgeBackend = "flexinfer"
		out.JudgeModel = ""
	}
	return out
}

// stageSource attributes the winning precedence level for a stage's spawn
// agent: env break-glass > policy stage_agents > built-in default.
func stageSource(in wiringInputs, stage string) string {
	if in.spawnEnvAgent {
		return "env"
	}
	if in.policy != nil && in.policy.AgentForStage(stage) != "" {
		return "policy"
	}
	return "default"
}

// policyChecksum returns a short, stable checksum of the current policy so an
// operator can tell at a glance whether two operators run the same policy.
// Cheap (a one-time hash at startup). Returns "" (field omitted) when the
// policy cannot be marshaled.
func policyChecksum(p *mills.Policy) string {
	if p == nil {
		return ""
	}
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// normalizeStrings guarantees a non-nil slice so JSON encodes [] not null.
func normalizeStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// orString returns v, or def when v is empty.
func orString(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// handleWiring serves the static resolved model-wiring snapshot captured at
// startup. Read-only, non-secret config — no auth, GET only. Returns 503 only
// in the (production-impossible) case that the snapshot was never populated,
// which surfaces a wiring bug rather than serving a misleading zero value.
func (o *operator) handleWiring(w http.ResponseWriter, _ *http.Request) {
	o.wiringMu.RLock()
	snap := o.wiring
	o.wiringMu.RUnlock()
	if snap == nil {
		http.Error(w, "wiring snapshot not populated", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}
