package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// bodyStubClient returns a FlexInferClient whose transport always serves the
// given body/status, built from an explicit config so tests can exercise the
// LiteLLM-gateway shape (DisableRegistryFallbacks + a gateway-routable model).
func bodyStubClient(t *testing.T, cfg FlexInferConfig, body string) *FlexInferClient {
	t.Helper()
	if cfg.ProxyURL == "" {
		cfg.ProxyURL = "http://stub"
	}
	cli, err := NewFlexInferClient(cfg)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(bytes.NewBufferString(body)),
			Header:     make(http.Header),
		}, nil
	}))
	return cli
}

// TestNewFlexInferClient_LiteLLMSuppressesRegistryFallbacks pins design (a):
// a DisableRegistryFallbacks client (the LiteLLM-gateway judge/weaver) never
// inherits the aimodels-registry FlexInfer model ids as fallbacks — a frontier
// outage degrades backend-locally (env-listed litellm ids, or nothing), it does
// not walk to a proxy model id the gateway can't route. Contrasted against the
// default client, which DOES get the registry chain for the same pinned model.
func TestNewFlexInferClient_LiteLLMSuppressesRegistryFallbacks(t *testing.T) {
	t.Setenv("FLEXINFER_JUDGE_MODEL_FALLBACKS", "")
	t.Setenv("FLEXINFER_WEAVER_MODEL_FALLBACKS", "")

	lite, err := NewFlexInferClient(FlexInferConfig{
		ProxyURL:                 "http://litellm.test",
		JudgeModel:               "or/kimi-k3",
		DisableRegistryFallbacks: true,
	})
	if err != nil {
		t.Fatalf("litellm ctor: %v", err)
	}
	if got := lite.fallbacksFor("or/kimi-k3"); len(got) != 0 {
		t.Errorf("litellm judge fallbacks = %v, want none (registry suppressed)", got)
	}

	// A default (registry-enabled) client with the SAME pinned model WOULD
	// pick up the aimodels role chain — proving the suppression is what makes
	// the difference, not an empty registry.
	def, err := NewFlexInferClient(FlexInferConfig{
		ProxyURL:   "http://flexinfer.test",
		JudgeModel: "or/kimi-k3",
	})
	if err != nil {
		t.Fatalf("default ctor: %v", err)
	}
	if got := def.fallbacksFor("or/kimi-k3"); len(got) == 0 {
		t.Skip("aimodels registry returned no judge chain; suppression contrast not observable in this build")
	}
}

// TestNewFlexInferClient_LiteLLMEnvFallbacksAreBackendLocal confirms the
// LiteLLM client still honors an explicit FLEXINFER_JUDGE_MODEL_FALLBACKS env
// (interpreted as gateway-routable ids) — the operator-facing degrade knob —
// while the registry stays suppressed.
func TestNewFlexInferClient_LiteLLMEnvFallbacksAreBackendLocal(t *testing.T) {
	t.Setenv("FLEXINFER_JUDGE_MODEL_FALLBACKS", "or/kimi-k2-7-code, or/deepseek-v3")
	lite, err := NewFlexInferClient(FlexInferConfig{
		ProxyURL:                 "http://litellm.test",
		JudgeModel:               "or/kimi-k3",
		DisableRegistryFallbacks: true,
	})
	if err != nil {
		t.Fatalf("litellm ctor: %v", err)
	}
	got := lite.fallbacksFor("or/kimi-k3")
	want := []string{"or/kimi-k2-7-code", "or/deepseek-v3"}
	if !slices.Equal(got, want) {
		t.Errorf("litellm judge fallbacks = %v, want %v (env-listed litellm ids only)", got, want)
	}
}

// TestRubricJudge_LiteLLMHardeningParityLiveFixtures replays the 7 REAL
// production judge responses (testdata/judge_unparseable_live_2026-07-16.json)
// through the FULL RubricJudge end-to-end on BOTH a FlexInfer-configured client
// and a LiteLLM-gateway-configured client (DisableRegistryFallbacks + or/kimi-
// k3). Identical bytes in ⇒ identical verdict/error-class out proves the judge
// hardening (truncation recovery, echo-stripping, unparseable sentinel) lives
// in the shared client + parser, not the proxy — so it rides through unchanged
// on the litellm path.
func TestRubricJudge_LiteLLMHardeningParityLiveFixtures(t *testing.T) {
	path := filepath.Join("testdata", "judge_unparseable_live_2026-07-16.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixtures []struct {
		Run    string `json:"run"`
		Gate   string `json:"gate"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(fixtures) != 7 {
		t.Fatalf("expected 7 live fixtures, got %d", len(fixtures))
	}

	var recovered, sentinels int
	for i, f := range fixtures {
		raw := extractRawPayload(t, f.Reason)
		if raw == "" {
			t.Fatalf("sample %d (%s/%s): no raw= payload extracted", i, f.Run, f.Gate)
		}
		body := cleanEnvelopeBody(raw)

		flex := bodyStubClient(t, FlexInferConfig{ProxyURL: "http://flexinfer.test", JudgeModel: "qwen35"}, body)
		lite := bodyStubClient(t, FlexInferConfig{
			ProxyURL:                 "http://litellm.test",
			Token:                    "k",
			JudgeModel:               "or/kimi-k3",
			DisableRegistryFallbacks: true,
		}, body)

		fv, ferr := NewRubricJudge(flex).Judge(context.Background(), gates.SpecConformanceRubricName, gates.StageInput{})
		lv, lerr := NewRubricJudge(lite).Judge(context.Background(), gates.SpecConformanceRubricName, gates.StageInput{})

		// Error classification must match exactly across backends.
		if (ferr == nil) != (lerr == nil) {
			t.Errorf("sample %d (%s/%s): err mismatch flex=%v litellm=%v", i, f.Run, f.Gate, ferr, lerr)
			continue
		}
		if ferr != nil {
			if errors.Is(ferr, ErrRubricUnparseable) != errors.Is(lerr, ErrRubricUnparseable) {
				t.Errorf("sample %d (%s/%s): unparseable-class mismatch flex=%v litellm=%v", i, f.Run, f.Gate, ferr, lerr)
			}
			if errors.Is(ferr, ErrRubricUnparseable) {
				sentinels++
			}
			continue
		}
		if fv.Score != lv.Score {
			t.Errorf("sample %d (%s/%s): score mismatch flex=%v litellm=%v", i, f.Run, f.Gate, fv.Score, lv.Score)
		}
		recovered++
	}
	if recovered == 0 {
		t.Error("no fixture recovered a score on the litellm path; recovery arm not exercised")
	}
	if sentinels == 0 {
		t.Error("no fixture hit the unparseable sentinel on the litellm path; negative arm not exercised")
	}
}

// TestRubricJudge_LiteLLMLengthRetryParity confirms the finish_reason=length
// double-budget retry fires identically on the litellm path: a truncated
// preamble on the first call, a clean envelope on the retry, recovered score.
func TestRubricJudge_LiteLLMLengthRetryParity(t *testing.T) {
	t.Setenv("FLEXINFER_JUDGE_MAX_TOKENS", "")
	var calls int32
	first := lengthTruncatedBody("Thinking Process:\n1. Analyze the request and consider the")
	second := cleanEnvelopeBody(`{"score": 0.91, "reasons": ["frontier ok"]}`)

	cli, err := NewFlexInferClient(FlexInferConfig{
		ProxyURL:                 "http://litellm.test",
		Token:                    "k",
		JudgeModel:               "or/kimi-k3",
		DisableRegistryFallbacks: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		var body struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Model != "or/kimi-k3" {
			t.Errorf("litellm judge dialed model %q, want or/kimi-k3", body.Model)
		}
		n := int(atomic.AddInt32(&calls, 1))
		if n == 1 {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(first)), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(second)), Header: make(http.Header)}, nil
	}))

	v, err := NewRubricJudge(cli).Judge(context.Background(), gates.SpecConformanceRubricName, gates.StageInput{})
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	if v.Score != 0.91 {
		t.Errorf("score = %v, want 0.91 from length retry", v.Score)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("call count = %d, want exactly 2 (one retry) on litellm path", got)
	}
}

// TestWeaverClient_LiteLLMCostPassthrough proves provider-reported usage.cost
// flows into the research stage's CostUSD on the litellm path (the frontier
// charge, not the flat local-tier estimate) — cost accounting parity with the
// council lens path.
func TestWeaverClient_LiteLLMCostPassthrough(t *testing.T) {
	const providerCost = 0.0172
	resp := chatResponse{Model: "or/kimi-k3"}
	resp.Choices = append(resp.Choices, chatChoice{
		Message:      chatMessage{Role: "assistant", Content: "research notes"},
		FinishReason: "stop",
	})
	resp.Usage.PromptTokens = 1200
	resp.Usage.CompletionTokens = 800
	resp.Usage.Cost = providerCost
	body, _ := json.Marshal(resp)

	cli := bodyStubClient(t, FlexInferConfig{
		ProxyURL:                 "http://litellm.test",
		Token:                    "k",
		WeaverModel:              "or/kimi-k3",
		DisableRegistryFallbacks: true,
	}, string(body))

	wc := NewWeaverClient(cli) // Mode defaults to off (legacy chat path)
	out, err := wc.Research(context.Background(), pipeline.WeaverRequest{Prompt: "research this"})
	if err != nil {
		t.Fatalf("research: %v", err)
	}
	if out.CostUSD != providerCost {
		t.Errorf("weaver CostUSD = %v, want provider-reported %v", out.CostUSD, providerCost)
	}
	if out.Model != "or/kimi-k3" {
		t.Errorf("weaver Model = %q, want or/kimi-k3", out.Model)
	}
}
