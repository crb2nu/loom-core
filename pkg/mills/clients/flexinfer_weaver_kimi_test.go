package clients

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// weaverKimiNotesBody is a kimi-k3-shaped LiteLLM→OpenRouter gateway response
// for a RESEARCH call: the research notes land in message.content, the
// chain-of-thought in the SEPARATE message.reasoning_content field, and the
// provider reports real usage (reasoning_tokens counted against the budget) plus
// the upstream USD cost. It mirrors the real captured judge fixture shape
// (testdata/kimi_k3_judge_live_2026-07-17.json) but carries prose notes instead
// of a score envelope, so it exercises the exact weaver attribution path.
const weaverKimiNotesBody = `{
  "model": "or/kimi-k3",
  "choices": [
    {"message": {"role": "assistant",
      "content": "Research: the api-call research path is WeaverClient.legacyResearch, which calls chatFallback against the configured WeaverModel. It runs when research_mode=off.",
      "reasoning_content": "Let me trace the research path. The operator wires the research stage to WeaverWorker, which calls WeaverClient.Research; in off mode that routes to legacyResearch..."},
     "finish_reason": "stop"}
  ],
  "usage": {"prompt_tokens": 1200, "completion_tokens": 900, "total_tokens": 2100, "cost": 0.0181,
    "completion_tokens_details": {"reasoning_tokens": 640}}
}`

// qwenEmptyStopBody is a NON-reasoning (local qwen) response that legitimately
// returned empty content with finish_reason=stop and no reasoning field. A
// larger budget would change nothing, so the weaver must NOT waste a retry on it
// — the byte-identical guard for the local research model.
const qwenEmptyStopBody = `{
  "model": "qwen3-32b",
  "choices": [
    {"message": {"role": "assistant", "content": ""}, "finish_reason": "stop"}
  ],
  "usage": {"prompt_tokens": 40, "completion_tokens": 0, "total_tokens": 40}
}`

// litellmKimiCfg is the LiteLLM-gateway weaver config: an explicit
// gateway-routable model + registry fallbacks suppressed (mirrors production
// MILLS_WEAVER_BACKEND=litellm wiring).
func litellmKimiCfg() FlexInferConfig {
	return FlexInferConfig{
		ProxyURL:                 "http://litellm.test",
		Token:                    "k",
		WeaverModel:              "or/kimi-k3",
		DisableRegistryFallbacks: true,
	}
}

// newWeaverMaxTokensCapture builds a litellm-gateway weaver client whose
// transport returns bodies[i] for the (i+1)-th call and records the max_tokens
// each request carried. Call count and the per-call budgets are returned via the
// pointers so a test can assert the first-call budget AND the boosted-retry
// budget.
func newWeaverMaxTokensCapture(t *testing.T, calls *int32, budgets *[]int, bodies ...string) *FlexInferClient {
	t.Helper()
	cli, err := NewFlexInferClient(litellmKimiCfg())
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		var body struct {
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		n := int(atomic.AddInt32(calls, 1))
		*budgets = append(*budgets, body.MaxTokens)
		idx := n - 1
		if idx >= len(bodies) {
			idx = len(bodies) - 1
		}
		return jsonResp(bodies[idx]), nil
	}))
	return cli
}

// TestWeaverClient_KimiNotesFromContent proves the research notes are extracted
// from message.content (not the reasoning field) for a kimi-shaped response, and
// that cost/model attribution flows from the provider-reported usage + id — the
// evidence that a reasoning-bearing response is decoded correctly on the weaver
// path. One call, no retry.
func TestWeaverClient_KimiNotesFromContent(t *testing.T) {
	t.Setenv("FLEXINFER_WEAVER_MAX_TOKENS", "") // default budget; notes present so no retry
	var calls int32
	cli := bodyStubClient(t, litellmKimiCfg(), weaverKimiNotesBody)
	// Wrap the transport to count calls without losing the body.
	cli.SetTransport(roundTripFn(func(*http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return jsonResp(weaverKimiNotesBody), nil
	}))

	out, err := NewWeaverClient(cli).Research(context.Background(), pipeline.WeaverRequest{
		BacklogID: "BL-K", Prompt: "trace the research path",
	})
	if err != nil {
		t.Fatalf("research: %v", err)
	}
	if !strings.Contains(out.Notes, "legacyResearch") {
		t.Errorf("notes should come from message.content, got %q", out.Notes)
	}
	if strings.Contains(out.Notes, "trace the research path") {
		// The reasoning_content prose is never surfaced as notes.
		t.Errorf("notes must not leak the reasoning field: %q", out.Notes)
	}
	if out.Model != "or/kimi-k3" {
		t.Errorf("weaver Model = %q, want or/kimi-k3", out.Model)
	}
	if out.CostUSD != 0.0181 {
		t.Errorf("weaver CostUSD = %v, want provider-reported 0.0181", out.CostUSD)
	}
	if out.Citation["model"] != "or/kimi-k3" {
		t.Errorf("citation.model = %v, want or/kimi-k3", out.Citation["model"])
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("call count = %d, want 1 (direct parse, no retry)", got)
	}
}

// TestWeaverClient_KimiEmptyContentSqueeze_BoostedRetryRecovers is the core
// regression: the REAL v5 squeeze (empty content, finish_reason=length,
// reasoning ate the 1024 budget) must trigger exactly ONE boosted retry, floored
// to 4096 because the first response showed reasoning activity, and the retry's
// clean notes recover instead of the pipeline seeing empty research.
func TestWeaverClient_KimiEmptyContentSqueeze_BoostedRetryRecovers(t *testing.T) {
	t.Setenv("FLEXINFER_WEAVER_MAX_TOKENS", "") // default 1024, mirrors a misconfigured production budget
	fx := loadKimiFixtures(t)
	squeeze := kimiBody(t, fx, "v5_1024_reasoning_effort_low")

	var calls int32
	var budgets []int
	cli := newWeaverMaxTokensCapture(t, &calls, &budgets, squeeze, weaverKimiNotesBody)

	out, err := NewWeaverClient(cli).Research(context.Background(), pipeline.WeaverRequest{Prompt: "research this"})
	if err != nil {
		t.Fatalf("research recovered nothing: %v", err)
	}
	if strings.TrimSpace(out.Notes) == "" {
		t.Fatal("expected recovered non-empty notes after the boosted retry")
	}
	if !strings.Contains(out.Notes, "legacyResearch") {
		t.Errorf("recovered notes = %q, want the retry body", out.Notes)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("call count = %d, want exactly 2 (one boosted retry)", got)
	}
	if len(budgets) != 2 {
		t.Fatalf("captured %d budgets, want 2", len(budgets))
	}
	if budgets[0] != defaultWeaverMaxTokens {
		t.Errorf("first-call max_tokens = %d, want default %d", budgets[0], defaultWeaverMaxTokens)
	}
	if budgets[1] != judgeReasoningRetryFloorTokens {
		t.Errorf("retry max_tokens = %d, want floored to %d for a reasoning-model squeeze",
			budgets[1], judgeReasoningRetryFloorTokens)
	}
	// Attribution follows the recovered response (the litellm/kimi id + cost).
	if out.Model != "or/kimi-k3" {
		t.Errorf("recovered Model = %q, want or/kimi-k3", out.Model)
	}
	if out.CostUSD != 0.0181 {
		t.Errorf("recovered CostUSD = %v, want 0.0181 (retry response cost)", out.CostUSD)
	}
}

// TestWeaverClient_KimiBothSqueeze_BoundedToOneRetry pins the mandated guard: a
// weaver call must NEVER return empty notes on a recoverable reasoning-starve
// without at least one boosted retry — but the retry is bounded to ONE. When the
// retry ALSO squeezes, the call returns the (empty) notes without erroring, so
// the grounding guard + downstream handle it; it does not spin.
func TestWeaverClient_KimiBothSqueeze_BoundedToOneRetry(t *testing.T) {
	t.Setenv("FLEXINFER_WEAVER_MAX_TOKENS", "")
	fx := loadKimiFixtures(t)
	squeeze := kimiBody(t, fx, "v5_1024_reasoning_effort_low")

	var calls int32
	var budgets []int
	cli := newWeaverMaxTokensCapture(t, &calls, &budgets, squeeze, squeeze)

	out, err := NewWeaverClient(cli).Research(context.Background(), pipeline.WeaverRequest{Prompt: "research this"})
	if err != nil {
		t.Fatalf("weaver should not error on empty notes: %v", err)
	}
	if strings.TrimSpace(out.Notes) != "" {
		t.Errorf("expected empty notes when every attempt squeezes, got %q", out.Notes)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("call count = %d, want exactly 2 (guard's one boosted retry, then stop)", got)
	}
}

// TestWeaverClient_EnvBudgetOverrideRespected proves FLEXINFER_WEAVER_MAX_TOKENS
// is read at construction (mirrors FLEXINFER_JUDGE_MAX_TOKENS) and used as the
// first-call completion budget — the durable fix that makes the kimi env flip
// safe. At 4096 the reasoning + notes fit on the FIRST call, so there is no
// retry.
func TestWeaverClient_EnvBudgetOverrideRespected(t *testing.T) {
	t.Setenv("FLEXINFER_WEAVER_MAX_TOKENS", "4096")
	var calls int32
	var budgets []int
	cli := newWeaverMaxTokensCapture(t, &calls, &budgets, weaverKimiNotesBody)

	wc := NewWeaverClient(cli)
	if wc.MaxTokens != 4096 {
		t.Fatalf("WeaverClient.MaxTokens = %d, want 4096 from env", wc.MaxTokens)
	}
	out, err := wc.Research(context.Background(), pipeline.WeaverRequest{Prompt: "research this"})
	if err != nil {
		t.Fatalf("research: %v", err)
	}
	if strings.TrimSpace(out.Notes) == "" {
		t.Error("expected non-empty notes at the 4096 budget")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("call count = %d, want 1 (4096 budget fits reasoning + notes, no retry)", got)
	}
	if budgets[0] != 4096 {
		t.Errorf("first-call max_tokens = %d, want 4096 from FLEXINFER_WEAVER_MAX_TOKENS", budgets[0])
	}
}

// TestWeaverMaxTokensFromEnv covers the resolver: a positive override wins, a
// blank/invalid/non-positive value falls back to the 1024 default (the local
// qwen research model is unchanged).
func TestWeaverMaxTokensFromEnv(t *testing.T) {
	cases := map[string]int{
		"":      defaultWeaverMaxTokens,
		"  ":    defaultWeaverMaxTokens,
		"0":     defaultWeaverMaxTokens,
		"-5":    defaultWeaverMaxTokens,
		"bogus": defaultWeaverMaxTokens,
		"4096":  4096,
		"2048":  2048,
	}
	for in, want := range cases {
		t.Setenv("FLEXINFER_WEAVER_MAX_TOKENS", in)
		if got := WeaverMaxTokensFromEnv(); got != want {
			t.Errorf("WeaverMaxTokensFromEnv(%q) = %d, want %d", in, got, want)
		}
	}
}

// TestWeaverClient_QwenPathUnchanged is the byte-identical guard for the local
// (non-reasoning) research model: a non-empty answer returns on the first call,
// and a GENUINE empty answer (finish_reason=stop, no reasoning) is returned
// as-is WITHOUT a wasted retry — only a recoverable reasoning-starve retries.
func TestWeaverClient_QwenPathUnchanged(t *testing.T) {
	t.Setenv("FLEXINFER_WEAVER_MAX_TOKENS", "")

	t.Run("non_empty_success_single_call", func(t *testing.T) {
		var calls int32
		cli, err := NewFlexInferClient(FlexInferConfig{ProxyURL: "http://stub", WeaverModel: "qwen3-32b"})
		if err != nil {
			t.Fatal(err)
		}
		cli.SetTransport(roundTripFn(func(*http.Request) (*http.Response, error) {
			atomic.AddInt32(&calls, 1)
			return jsonResp(successBody), nil
		}))
		out, err := NewWeaverClient(cli).Research(context.Background(), pipeline.WeaverRequest{Prompt: "p"})
		if err != nil {
			t.Fatalf("research: %v", err)
		}
		if !strings.Contains(out.Notes, "verdict") {
			t.Errorf("notes = %q, want the qwen content", out.Notes)
		}
		if got := atomic.LoadInt32(&calls); got != 1 {
			t.Errorf("call count = %d, want 1 (qwen success, no retry)", got)
		}
	})

	t.Run("genuine_empty_no_retry", func(t *testing.T) {
		var calls int32
		cli, err := NewFlexInferClient(FlexInferConfig{ProxyURL: "http://stub", WeaverModel: "qwen3-32b"})
		if err != nil {
			t.Fatal(err)
		}
		cli.SetTransport(roundTripFn(func(*http.Request) (*http.Response, error) {
			atomic.AddInt32(&calls, 1)
			return jsonResp(qwenEmptyStopBody), nil
		}))
		out, err := NewWeaverClient(cli).Research(context.Background(), pipeline.WeaverRequest{Prompt: "p"})
		if err != nil {
			t.Fatalf("research: %v", err)
		}
		if strings.TrimSpace(out.Notes) != "" {
			t.Errorf("notes = %q, want empty (genuine empty answer)", out.Notes)
		}
		if got := atomic.LoadInt32(&calls); got != 1 {
			t.Errorf("call count = %d, want 1 (no reasoning + finish=stop is NOT retried)", got)
		}
	})
}
