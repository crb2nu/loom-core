package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/gates"
)

// kimiFixture is one real captured LiteLLM→OpenRouter kimi-k3 response
// (testdata/kimi_k3_judge_live_2026-07-17.json).
type kimiFixture struct {
	Variant  string          `json:"variant"`
	Note     string          `json:"note"`
	Status   int             `json:"http_status"`
	Response json.RawMessage `json:"response"`
}

// loadKimiFixtures returns the real captured kimi-k3 judge responses keyed by
// variant name. Each Response is the exact gateway JSON body a test can serve.
func loadKimiFixtures(t *testing.T) map[string]kimiFixture {
	t.Helper()
	path := filepath.Join("testdata", "kimi_k3_judge_live_2026-07-17.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read kimi fixture: %v", err)
	}
	var raw []kimiFixture
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode kimi fixture: %v", err)
	}
	out := make(map[string]kimiFixture, len(raw))
	for _, f := range raw {
		out[f.Variant] = f
	}
	return out
}

// kimiBody returns the raw gateway JSON body for a captured variant.
func kimiBody(t *testing.T, fx map[string]kimiFixture, variant string) string {
	t.Helper()
	f, ok := fx[variant]
	if !ok {
		t.Fatalf("kimi fixture %q not present", variant)
	}
	return string(f.Response)
}

// TestChatResponse_DecodesKimiReasoningFields proves the client now reads the
// reasoning-bearing message shape kimi-k3 returns: the chain-of-thought lands in
// message.reasoning_content, the score envelope in message.content, and the
// provider counts reasoning tokens against the completion budget. This is the
// evidence root of the bug — before this fix the struct dropped every reasoning
// field, so an empty-content squeeze looked like a bare unparseable failure.
func TestChatResponse_DecodesKimiReasoningFields(t *testing.T) {
	fx := loadKimiFixtures(t)

	// v4: reasoning + envelope both fit (max_tokens=4096).
	var ok chatResponse
	if err := json.Unmarshal([]byte(kimiBody(t, fx, "v4_4096_plain_no_kwargs")), &ok); err != nil {
		t.Fatalf("decode v4: %v", err)
	}
	if len(ok.Choices) == 0 {
		t.Fatal("v4: no choices")
	}
	if ok.Choices[0].Message.Content == "" {
		t.Error("v4: expected the score envelope in message.content")
	}
	if ok.Choices[0].Message.ReasoningContent == "" {
		t.Error("v4: expected chain-of-thought in message.reasoning_content")
	}
	if ok.Usage.CompletionTokensDetails.ReasoningTokens == 0 {
		t.Error("v4: expected usage.completion_tokens_details.reasoning_tokens > 0")
	}
	if _, _, perr := parseRubricEnvelope(ok.Choices[0].Message.Content); perr != nil {
		t.Errorf("v4: content should parse to a verdict: %v", perr)
	}

	// v5: reasoning consumed the whole budget (reasoning_tokens ~1021/1024) →
	// content empty, finish_reason=length. The failure the operator hit.
	var squeezed chatResponse
	if err := json.Unmarshal([]byte(kimiBody(t, fx, "v5_1024_reasoning_effort_low")), &squeezed); err != nil {
		t.Fatalf("decode v5: %v", err)
	}
	if got := squeezed.Choices[0].Message.Content; got != "" {
		t.Errorf("v5: expected empty content (the squeeze), got %q", got)
	}
	if squeezed.Choices[0].FinishReason != "length" {
		t.Errorf("v5: finish_reason = %q, want length", squeezed.Choices[0].FinishReason)
	}
	if squeezed.Usage.CompletionTokensDetails.ReasoningTokens == 0 {
		t.Error("v5: expected reasoning_tokens > 0 (reasoning ate the budget)")
	}
	if !responseHadReasoning(&squeezed) {
		t.Error("v5: responseHadReasoning must detect the reasoning-model squeeze")
	}
}

// TestJudge_KimiSuccessCaseParsesDirect feeds the real v4 body (4096 budget,
// full envelope) and asserts the judge grades it on the first call — no retry,
// score parsed straight from message.content.
func TestJudge_KimiSuccessCaseParsesDirect(t *testing.T) {
	fx := loadKimiFixtures(t)
	var calls int32
	cli := newCountingStub(t, &calls, kimiBody(t, fx, "v4_4096_plain_no_kwargs"))

	v, err := NewRubricJudge(cli).Judge(context.Background(), gates.SpecConformanceRubricName, gates.StageInput{})
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	if v.Score != 1.0 {
		t.Errorf("score = %v, want 1.0 (v4 envelope)", v.Score)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("call count = %d, want 1 (direct parse, no retry)", got)
	}
}

// TestJudge_KimiEmptyContentSqueeze_BoostedRetryRecovers is the core regression:
// the real v5 squeeze (empty content, finish_reason=length, reasoning ate the
// 1024 budget) must trigger exactly ONE boosted retry, and — because the first
// response showed reasoning activity — the retry budget is floored to 4096 so
// the chain-of-thought and the envelope both fit. The retry returns a clean
// envelope and the gate recovers instead of false-failing on raw="".
func TestJudge_KimiEmptyContentSqueeze_BoostedRetryRecovers(t *testing.T) {
	t.Setenv("FLEXINFER_JUDGE_MAX_TOKENS", "") // default 1024, mirrors production
	fx := loadKimiFixtures(t)
	first := kimiBody(t, fx, "v5_1024_reasoning_effort_low")
	second := kimiBody(t, fx, "v4_4096_plain_no_kwargs") // clean envelope on retry

	var calls int32
	var retryMaxTokens int
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
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		n := int(atomic.AddInt32(&calls, 1))
		if n == 1 {
			return jsonResp(first), nil
		}
		retryMaxTokens = body.MaxTokens
		return jsonResp(second), nil
	}))

	v, err := NewRubricJudge(cli).Judge(context.Background(), gates.SpecConformanceRubricName, gates.StageInput{})
	if err != nil {
		t.Fatalf("judge recovered nothing: %v", err)
	}
	if v.Score != 1.0 {
		t.Errorf("recovered score = %v, want 1.0", v.Score)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("call count = %d, want exactly 2 (one boosted retry)", got)
	}
	if retryMaxTokens != judgeReasoningRetryFloorTokens {
		t.Errorf("retry max_tokens = %d, want floored to %d for a reasoning-model squeeze",
			retryMaxTokens, judgeReasoningRetryFloorTokens)
	}
}

// TestJudge_KimiEmptyContentNeverFailsWithoutRetry_BothBackends is the mandated
// guard: a gate must NEVER false-fail on an empty completion (raw="") without at
// least one boosted retry — on BOTH the flexinfer proxy and the litellm gateway
// client. Here the retry ALSO returns empty (bounded to one), so the judge does
// surface the unparseable sentinel — but only after the guard tried, and its
// diagnostic names the reasoning squeeze.
func TestJudge_KimiEmptyContentNeverFailsWithoutRetry_BothBackends(t *testing.T) {
	fx := loadKimiFixtures(t)
	squeeze := kimiBody(t, fx, "v5_1024_reasoning_effort_low")

	backends := []struct {
		name string
		cfg  FlexInferConfig
	}{
		{"flexinfer", FlexInferConfig{ProxyURL: "http://flexinfer.test", JudgeModel: "qwen35"}},
		{"litellm", FlexInferConfig{ProxyURL: "http://litellm.test", Token: "k", JudgeModel: "or/kimi-k3", DisableRegistryFallbacks: true}},
	}
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			var calls int32
			cli, err := NewFlexInferClient(b.cfg)
			if err != nil {
				t.Fatal(err)
			}
			cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				return jsonResp(squeeze), nil // both calls squeeze
			}))
			_, err = NewRubricJudge(cli).Judge(context.Background(), gates.SpecConformanceRubricName, gates.StageInput{})
			if err == nil {
				t.Fatal("expected unparseable error when every attempt is empty")
			}
			// The guard must have tried a boosted retry before giving up.
			if got := atomic.LoadInt32(&calls); got != 2 {
				t.Errorf("call count = %d, want 2 (guard's one boosted retry) before surfacing the sentinel", got)
			}
		})
	}
}

// TestBoostedRetryTokens_FloorsForReasoningModels pins the budget math: a plain
// (non-reasoning) miss doubles the budget (unchanged behavior for the flexinfer
// qwen judge), while a reasoning-model squeeze floors the retry so the
// chain-of-thought can complete.
func TestBoostedRetryTokens_FloorsForReasoningModels(t *testing.T) {
	// Non-reasoning length stop: double, no floor.
	plain := &chatResponse{}
	plain.Choices = []chatChoice{{FinishReason: "length"}}
	if got := boostedRetryTokens(1024, plain); got != 2048 {
		t.Errorf("plain retry budget = %d, want 2048 (double)", got)
	}
	// Reasoning squeeze: floored to 4096.
	reasoning := &chatResponse{}
	reasoning.Choices = []chatChoice{{Message: chatMessage{ReasoningContent: "…"}, FinishReason: "length"}}
	reasoning.Usage.CompletionTokensDetails.ReasoningTokens = 1021
	if got := boostedRetryTokens(1024, reasoning); got != judgeReasoningRetryFloorTokens {
		t.Errorf("reasoning retry budget = %d, want %d (floored)", got, judgeReasoningRetryFloorTokens)
	}
	// A large configured budget already clears the floor: keep the double.
	if got := boostedRetryTokens(4096, reasoning); got != 8192 {
		t.Errorf("large-budget reasoning retry = %d, want 8192 (double already clears floor)", got)
	}
}

// TestLiveKimiJudge_ParsesThroughFixedClient drives the REAL RubricJudge
// against the live LiteLLM gateway (or/kimi-k3) through the fixed client path
// and asserts a parseable score envelope comes back. Skipped unless
// MILLS_LIVE_CAPTURE=1. This is the kill-test for the re-flip: it exercises the
// exact production shape (structuredChatFallback → LiteLLM gateway) plus the
// recommended FLEXINFER_JUDGE_MAX_TOKENS budget.
//
//	MILLS_LIVE_CAPTURE=1 MILLS_LITELLM_URL=http://127.0.0.1:18000 \
//	MILLS_LITELLM_KEY=sk-... FLEXINFER_JUDGE_MAX_TOKENS=4096 \
//	go test ./pkg/mills/clients -run TestLiveKimiJudge_ParsesThroughFixedClient -v
func TestLiveKimiJudge_ParsesThroughFixedClient(t *testing.T) {
	if os.Getenv("MILLS_LIVE_CAPTURE") == "" {
		t.Skip("set MILLS_LIVE_CAPTURE=1 to hit the live gateway")
	}
	base := os.Getenv("MILLS_LITELLM_URL")
	key := os.Getenv("MILLS_LITELLM_KEY")
	if base == "" || key == "" {
		t.Fatal("MILLS_LITELLM_URL and MILLS_LITELLM_KEY required")
	}
	cli, err := NewFlexInferClient(FlexInferConfig{
		ProxyURL:                 base,
		Token:                    key,
		JudgeModel:               "or/kimi-k3",
		DisableRegistryFallbacks: true,
		Timeout:                  180 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	in := gates.StageInput{
		FilesChanged: []string{"pkg/mills/clients/flexinfer.go"},
		LinesAdded:   18,
		LinesRemoved: 2,
		TestsPassed:  true,
		DiffPatch: []byte("diff --git a/pkg/mills/clients/flexinfer.go b/pkg/mills/clients/flexinfer.go\n" +
			"@@ func responseHadReasoning @@\n" +
			"+func responseHadReasoning(resp *chatResponse) bool { return resp != nil }\n"),
	}
	v, err := NewRubricJudge(cli).Judge(context.Background(), gates.SpecConformanceRubricName, in)
	if err != nil {
		t.Fatalf("live kimi judge failed: %v", err)
	}
	if v.Score < 0 || v.Score > 1 {
		t.Fatalf("live kimi judge score out of range: %v", v.Score)
	}
	t.Logf("LIVE kimi-k3 verdict: score=%v model=%q reasons=%d", v.Score, v.Model, len(v.Reasons))
}

// --- shared helpers ---

func jsonResp(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Header:     make(http.Header),
	}
}

func newCountingStub(t *testing.T, calls *int32, body string) *FlexInferClient {
	t.Helper()
	cli, err := NewFlexInferClient(FlexInferConfig{ProxyURL: "http://stub", JudgeModel: "qwen35"})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(calls, 1)
		return jsonResp(body), nil
	}))
	return cli
}
