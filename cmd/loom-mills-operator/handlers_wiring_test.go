package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/clients"
)

// TestHandleWiring_ZeroSnapshotContractShape drives the operator mux end-to-end
// against a ZERO-VALUE snapshot and pins the exact JSON contract the HUD Mills
// Overview builds against: every field present, correct snake_case keys, and —
// the load-bearing invariant — every array encodes as [] never null.
func TestHandleWiring_ZeroSnapshotContractShape(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	// A zero-value snapshot is the worst case for the never-null contract:
	// no clients, no lenses, no stages. buildWiringSnapshot must still emit
	// [] for every array. Populate via the same setter main.go uses.
	op.withWiringSnapshot(buildWiringSnapshot(wiringInputs{
		policy: op.policy.Current(),
		now:    time.Unix(1_700_000_000, 0),
	}))

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/wiring", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	for _, key := range []string{
		"generated_at", "judge", "weaver", "council", "stages",
		"spawn", "gates", "litellm", "policy",
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("response missing top-level key %q; body=%s", key, rec.Body.String())
		}
	}

	// Never-null arrays: judge.fallbacks, weaver.fallbacks, council.lenses,
	// stages. Decode loosely and assert each is a JSON array (not null).
	var loose struct {
		Judge struct {
			Fallbacks []any `json:"fallbacks"`
		} `json:"judge"`
		Weaver struct {
			Fallbacks []any `json:"fallbacks"`
		} `json:"weaver"`
		Council struct {
			Lenses []any `json:"lenses"`
		} `json:"council"`
		Stages *[]any `json:"stages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &loose); err != nil {
		t.Fatalf("loose decode: %v", err)
	}
	if loose.Judge.Fallbacks == nil {
		t.Error("judge.fallbacks must be [] not null")
	}
	if loose.Weaver.Fallbacks == nil {
		t.Error("weaver.fallbacks must be [] not null")
	}
	if loose.Council.Lenses == nil {
		t.Error("council.lenses must be [] not null")
	}
	if loose.Stages == nil || *loose.Stages == nil {
		t.Error("stages must be [] not null")
	}

	// Exact field names + zero-value semantics for a fully-unconfigured op.
	var resp WiringSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("typed decode: %v", err)
	}
	if resp.Judge.Backend != "none" {
		t.Errorf("judge.backend = %q, want none (no client)", resp.Judge.Backend)
	}
	if resp.Judge.MaxTokens != clients.JudgeMaxTokensFromEnv() {
		t.Errorf("judge.max_tokens = %d, want %d", resp.Judge.MaxTokens, clients.JudgeMaxTokensFromEnv())
	}
	if resp.Weaver.MaxTokens != clients.WeaverMaxTokensFromEnv() {
		t.Errorf("weaver.max_tokens = %d, want %d", resp.Weaver.MaxTokens, clients.WeaverMaxTokensFromEnv())
	}
	if resp.Gates.LLMGatesEnabled {
		t.Error("gates.llm_gates_enabled must be false with no judge client")
	}
	if resp.Gates.Tiebreaker != "none" {
		t.Errorf("gates.tiebreaker = %q, want none", resp.Gates.Tiebreaker)
	}
	// The seeded test policy has no `enabled:` key ⇒ autonomy defaults on.
	if !resp.Policy.AutonomyEnabled {
		t.Error("policy.autonomy_enabled must default true")
	}
	if len(resp.Stages) != len(spawnStages) {
		t.Errorf("stages len = %d, want %d", len(resp.Stages), len(spawnStages))
	}
}

// TestHandleWiring_503WhenUnpopulated pins the guard: an operator that never
// captured a snapshot returns 503, not a misleading zero-value body.
func TestHandleWiring_503WhenUnpopulated(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/wiring", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unpopulated snapshot: got %d want 503", rec.Code)
	}
}

// TestBuildWiringSnapshot_JudgeBackendPrecedence pins the EFFECTIVE judge wiring
// across the three env-precedence outcomes resolveMillsJudgeClient produces:
// default flexinfer, litellm-configured, and misconfigured-loud-fallback. The
// snapshot must reflect the resolved outcome, not the raw env request.
func TestBuildWiringSnapshot_JudgeBackendPrecedence(t *testing.T) {
	flex, err := clients.NewFlexInferClient(clients.FlexInferConfig{
		ProxyURL: "http://flexinfer.test", JudgeModel: "gemma4-26b",
	})
	if err != nil {
		t.Fatalf("flex client: %v", err)
	}
	policy := loadPolicyFromBody(t, validPolicy)

	t.Run("default_flexinfer", func(t *testing.T) {
		cfg := Config{FlexInferProxyURL: "http://flexinfer.test"}
		judge, model := resolveMillsJudgeClient(cfg, flex, discardLogger())
		snap := buildWiringSnapshot(wiringInputs{
			cfg: cfg, policy: policy, flexClient: flex,
			judgeClient: judge, weaverClient: flex, councilJudgeModel: model,
			now: time.Now(),
		})
		if snap.Judge.Backend != "flexinfer" {
			t.Errorf("judge.backend = %q, want flexinfer", snap.Judge.Backend)
		}
		if snap.Judge.Model != "gemma4-26b" {
			t.Errorf("judge.model = %q, want gemma4-26b", snap.Judge.Model)
		}
		if snap.Judge.RegistryFallbacksDisabled {
			t.Error("registry_fallbacks_disabled must be false for the flexinfer judge")
		}
		if !snap.Gates.LLMGatesEnabled {
			t.Error("llm_gates_enabled must be true when a judge client resolves")
		}
	})

	t.Run("litellm_configured", func(t *testing.T) {
		cfg := Config{
			FlexInferProxyURL:   "http://flexinfer.test",
			JudgeBackend:        "litellm",
			LiteLLMProxyURL:     "http://litellm.test",
			LiteLLMToken:        "k",
			FlexInferJudgeModel: "or/kimi-k3",
		}
		t.Setenv("FLEXINFER_JUDGE_MODEL_FALLBACKS", "or/kimi-k2.7-code")
		judge, model := resolveMillsJudgeClient(cfg, flex, discardLogger())
		if judge == flex {
			t.Fatal("expected a distinct litellm judge client")
		}
		snap := buildWiringSnapshot(wiringInputs{
			cfg: cfg, policy: policy, flexClient: flex,
			judgeClient: judge, weaverClient: flex, councilJudgeModel: model,
			litellmConfigured: true, now: time.Now(),
		})
		if snap.Judge.Backend != "litellm" {
			t.Errorf("judge.backend = %q, want litellm", snap.Judge.Backend)
		}
		if snap.Judge.Model != "or/kimi-k3" {
			t.Errorf("judge.model = %q, want or/kimi-k3", snap.Judge.Model)
		}
		if !snap.Judge.RegistryFallbacksDisabled {
			t.Error("registry_fallbacks_disabled must be true for a litellm judge")
		}
		if len(snap.Judge.Fallbacks) != 1 || snap.Judge.Fallbacks[0] != "or/kimi-k2.7-code" {
			t.Errorf("judge.fallbacks = %v, want [or/kimi-k2.7-code]", snap.Judge.Fallbacks)
		}
		// Council contradiction judge shares the litellm selection.
		if snap.Council.JudgeBackend != "litellm" || snap.Council.JudgeModel != "or/kimi-k3" {
			t.Errorf("council judge = %s/%s, want litellm/or/kimi-k3", snap.Council.JudgeBackend, snap.Council.JudgeModel)
		}
	})

	t.Run("misconfigured_falls_back_to_flexinfer", func(t *testing.T) {
		// litellm requested but no gateway URL ⇒ resolveMillsJudgeClient degrades
		// LOUD to the FlexInfer client. The snapshot must reflect the EFFECTIVE
		// flexinfer outcome, not the litellm request.
		cfg := Config{
			FlexInferProxyURL:   "http://flexinfer.test",
			JudgeBackend:        "litellm",
			FlexInferJudgeModel: "or/kimi-k3",
			// LiteLLMProxyURL deliberately empty.
		}
		judge, model := resolveMillsJudgeClient(cfg, flex, discardLogger())
		if judge != flex {
			t.Fatalf("misconfigured litellm judge must degrade to flex, got %p", judge)
		}
		snap := buildWiringSnapshot(wiringInputs{
			cfg: cfg, policy: policy, flexClient: flex,
			judgeClient: judge, weaverClient: flex, councilJudgeModel: model,
			now: time.Now(),
		})
		if snap.Judge.Backend != "flexinfer" {
			t.Errorf("judge.backend = %q, want flexinfer (degraded)", snap.Judge.Backend)
		}
		if snap.Council.JudgeBackend != "flexinfer" || snap.Council.JudgeModel != "" {
			t.Errorf("council judge = %s/%s, want flexinfer/'' (degraded)", snap.Council.JudgeBackend, snap.Council.JudgeModel)
		}
	})
}

// TestBuildWiringSnapshot_CouncilLensBackends pins the lens list mirroring
// buildCouncilRunner: a litellm lens with no gateway degrades to "fake"; with a
// gateway it stays "litellm"; a flexinfer lens stays "flexinfer".
func TestBuildWiringSnapshot_CouncilLensBackends(t *testing.T) {
	flex, err := clients.NewFlexInferClient(clients.FlexInferConfig{ProxyURL: "http://flexinfer.test"})
	if err != nil {
		t.Fatalf("flex client: %v", err)
	}
	policy := loadPolicyFromBody(t, litellmPolicy)

	byName := func(snap WiringSnapshot) map[string]councilLensWiring {
		m := map[string]councilLensWiring{}
		for _, l := range snap.Council.Lenses {
			m[l.Name] = l
		}
		return m
	}

	t.Run("gateway_present", func(t *testing.T) {
		snap := buildWiringSnapshot(wiringInputs{
			policy: policy, flexClient: flex, judgeClient: flex, weaverClient: flex,
			litellmConfigured: true, now: time.Now(),
		})
		lenses := byName(snap)
		if lenses["frontier"].Backend != "litellm" || lenses["frontier"].Model != "or/kimi-k3" {
			t.Errorf("frontier lens = %+v, want litellm/or/kimi-k3", lenses["frontier"])
		}
		if lenses["architecture"].Backend != "flexinfer" {
			t.Errorf("architecture lens backend = %q, want flexinfer", lenses["architecture"].Backend)
		}
	})

	t.Run("gateway_absent_degrades_to_fake", func(t *testing.T) {
		snap := buildWiringSnapshot(wiringInputs{
			policy: policy, flexClient: flex, judgeClient: flex, weaverClient: flex,
			litellmConfigured: false, now: time.Now(),
		})
		lenses := byName(snap)
		if lenses["frontier"].Backend != "fake" {
			t.Errorf("frontier lens backend = %q, want fake (no gateway)", lenses["frontier"].Backend)
		}
		if lenses["architecture"].Backend != "flexinfer" {
			t.Errorf("architecture lens backend = %q, want flexinfer", lenses["architecture"].Backend)
		}
	})

	t.Run("flexinfer_unconfigured_all_fake", func(t *testing.T) {
		snap := buildWiringSnapshot(wiringInputs{
			policy: policy, flexClient: nil, judgeClient: nil, weaverClient: nil,
			now: time.Now(),
		})
		for _, l := range snap.Council.Lenses {
			if l.Backend != "fake" {
				t.Errorf("lens %q backend = %q, want fake (flexinfer unconfigured)", l.Name, l.Backend)
			}
		}
		if snap.Council.EditorBackend != "fake" {
			t.Errorf("editor backend = %q, want fake", snap.Council.EditorBackend)
		}
		if snap.Council.JudgeBackend != "fake" {
			t.Errorf("council judge backend = %q, want fake", snap.Council.JudgeBackend)
		}
	})
}

// TestBuildWiringSnapshot_StageSourceAttribution pins the per-stage source: env
// break-glass > policy stage_agents > default.
func TestBuildWiringSnapshot_StageSourceAttribution(t *testing.T) {
	const policyWithReviewOverride = `
version: 2
budgets:
  council:  { max_usd_per_run: 15, max_usd_per_day: 50 }
  pipeline: { max_usd_per_run: 5, max_usd_per_day: 75 }
pipeline:
  retry: { max_attempts: 3, cooldown_seconds: 300 }
  stage_agents:
    pr_self_review: gemini
  stage_models:
    implement: gpt-5.6-terra
`
	stageByName := func(snap WiringSnapshot) map[string]stageWiring {
		m := map[string]stageWiring{}
		for _, s := range snap.Stages {
			m[s.Stage] = s
		}
		return m
	}

	t.Run("policy_and_default", func(t *testing.T) {
		t.Setenv("LOOM_MILLS_SPAWN_AGENT", "")
		t.Setenv("LOOM_MILLS_SPAWN_MODEL", "")
		pm := newAgentPolicyManager(t, policyWithReviewOverride)
		snap := buildWiringSnapshot(wiringInputs{
			policy: pm.Current(), agentFor: agentForStage(pm), modelFor: modelForStage(pm),
			spawnDefaultAgent: mills.AgentDefault, now: time.Now(),
		})
		stages := stageByName(snap)
		if stages["pr_self_review"].Agent != "gemini" || stages["pr_self_review"].Source != "policy" {
			t.Errorf("pr_self_review = %+v, want gemini/policy", stages["pr_self_review"])
		}
		if stages["implement"].Agent != mills.AgentDefault || stages["implement"].Source != "default" {
			t.Errorf("implement agent = %+v, want %s/default", stages["implement"], mills.AgentDefault)
		}
		// implement has a stage_models override; model resolves even though the
		// agent stays the default.
		if stages["implement"].Model != "gpt-5.6-terra" {
			t.Errorf("implement model = %q, want gpt-5.6-terra", stages["implement"].Model)
		}
		if stages["plan_slice"].Source != "default" {
			t.Errorf("plan_slice source = %q, want default", stages["plan_slice"].Source)
		}
	})

	t.Run("env_break_glass_wins", func(t *testing.T) {
		t.Setenv("LOOM_MILLS_SPAWN_AGENT", "codex")
		t.Setenv("LOOM_MILLS_SPAWN_MODEL", "")
		pm := newAgentPolicyManager(t, policyWithReviewOverride)
		snap := buildWiringSnapshot(wiringInputs{
			policy: pm.Current(), agentFor: agentForStage(pm), modelFor: modelForStage(pm),
			spawnDefaultAgent: "codex", spawnEnvAgent: true, now: time.Now(),
		})
		for _, s := range snap.Stages {
			if s.Agent != "codex" || s.Source != "env" {
				t.Errorf("stage %s = %+v, want codex/env (break-glass)", s.Stage, s)
			}
		}
		if snap.Spawn.DefaultAgent != "codex" || !snap.Spawn.EnvAgentOverride {
			t.Errorf("spawn = %+v, want default codex + env override", snap.Spawn)
		}
	})
}

// TestBuildWiringSnapshot_WeaverAndLiteLLM pins the weaver backend + litellm
// configured flag independent of the judge selection.
func TestBuildWiringSnapshot_WeaverAndLiteLLM(t *testing.T) {
	flex, err := clients.NewFlexInferClient(clients.FlexInferConfig{
		ProxyURL: "http://flexinfer.test", WeaverModel: "qwen3-32b",
	})
	if err != nil {
		t.Fatalf("flex client: %v", err)
	}
	policy := loadPolicyFromBody(t, validPolicy)

	cfg := Config{FlexInferProxyURL: "http://flexinfer.test"} // weaver default flexinfer
	snap := buildWiringSnapshot(wiringInputs{
		cfg: cfg, policy: policy, flexClient: flex, judgeClient: flex, weaverClient: flex,
		litellmConfigured: false, now: time.Now(),
	})
	if snap.Weaver.Backend != "flexinfer" {
		t.Errorf("weaver.backend = %q, want flexinfer", snap.Weaver.Backend)
	}
	if snap.Weaver.Model != "qwen3-32b" {
		t.Errorf("weaver.model = %q, want qwen3-32b", snap.Weaver.Model)
	}
	if snap.LiteLLM.Configured {
		t.Error("litellm.configured must be false")
	}
}

// loadPolicyFromBody writes body to a temp file and loads it as a *mills.Policy.
func loadPolicyFromBody(t *testing.T, body string) *mills.Policy {
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
	return pm.Current()
}
