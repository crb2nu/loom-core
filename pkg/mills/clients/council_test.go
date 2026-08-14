package clients

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/eval"
)

func TestFlexInferCouncilReviewer(t *testing.T) {
	cli := newStubClient(t, `{
		"model":"qwen3-8b",
		"choices":[{"message":{"role":"assistant","content":"# Security\n\nScope is tight."}}],
		"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}
	}`, 200)
	reviewer := &FlexInferCouncilReviewer{Client: cli}

	out, err := reviewer.Review(context.Background(), &council.Brief{Markdown: "Ship Mills."}, council.ReviewerLens{
		Name: "security", Model: "qwen3-8b", Backend: "flexinfer",
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if out.Lens.Name != "security" || !strings.Contains(out.Markdown, "Scope is tight") {
		t.Fatalf("unexpected review output: %+v", out)
	}
	if out.CostUSD <= 0 {
		t.Fatalf("CostUSD = %v, want positive estimate", out.CostUSD)
	}
}

func TestFlexInferCouncilReviewer_ZeroCostErrorOnlyMarksRemoteSpendUnpriced(t *testing.T) {
	for _, tc := range []struct {
		name         string
		backend      string
		wantUnpriced bool
	}{
		{name: "local outage", backend: "flexinfer", wantUnpriced: false},
		{name: "remote outage", backend: "litellm", wantUnpriced: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli := newStubClient(t, `{"error":"unavailable"}`, http.StatusServiceUnavailable)
			reviewer := &FlexInferCouncilReviewer{Client: cli}

			out, err := reviewer.Review(context.Background(), &council.Brief{Markdown: "Brief"}, council.ReviewerLens{
				Name: "security", Model: "qwen3-8b", Backend: tc.backend,
			})
			if err == nil {
				t.Fatal("expected provider error")
			}
			if out.CostUnpriced != tc.wantUnpriced {
				t.Fatalf("CostUnpriced = %v, want %v for backend %q", out.CostUnpriced, tc.wantUnpriced, tc.backend)
			}
		})
	}
}

func TestFlexInferCouncilReviewer_RemoteEstimateWithoutProviderCostMarksUnpriced(t *testing.T) {
	cli := newStubClient(t, `{
		"model":"remote-model",
		"choices":[{"message":{"role":"assistant","content":"review"}}],
		"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}
	}`, http.StatusOK)
	reviewer := &FlexInferCouncilReviewer{Client: cli}

	out, err := reviewer.Review(context.Background(), &council.Brief{Markdown: "Brief"}, council.ReviewerLens{
		Name: "security", Model: "remote-model", Backend: "litellm",
	})
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if out.CostUSD <= 0 {
		t.Fatalf("local-tier fallback estimate = %v, want positive fixture proof", out.CostUSD)
	}
	if !out.CostUnpriced {
		t.Fatal("remote response without usage.cost must not treat the local-tier estimate as authoritative")
	}
}

// Regression: Qwen's visible thinking can consume the reviewer's entire
// 30-second dispatch window before any critique is returned. Council reviews
// are short prose, so they should disable visible thinking and use a bounded
// default completion budget.
func TestFlexInferCouncilReviewerDisablesThinkingAndBoundsOutput(t *testing.T) {
	var captured map[string]json.RawMessage
	cli, err := NewFlexInferClient(FlexInferConfig{ProxyURL: "http://stub"})
	if err != nil {
		t.Fatal(err)
	}
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		body := `{"model":"qwen35","choices":[{"message":{"role":"assistant","content":"No material risks."}}]}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	}))

	reviewer := &FlexInferCouncilReviewer{Client: cli}
	if _, err := reviewer.Review(context.Background(), &council.Brief{Markdown: "Ship Mills."}, council.ReviewerLens{
		Name: "architecture", Model: "qwen35", Backend: "flexinfer",
	}); err != nil {
		t.Fatalf("Review: %v", err)
	}

	var kwargs struct {
		EnableThinking *bool `json:"enable_thinking"`
	}
	if err := json.Unmarshal(captured["chat_template_kwargs"], &kwargs); err != nil {
		t.Fatalf("decode chat_template_kwargs: %v", err)
	}
	if kwargs.EnableThinking == nil || *kwargs.EnableThinking {
		t.Fatalf("enable_thinking = %v, want false", kwargs.EnableThinking)
	}
	var maxTokens int
	if err := json.Unmarshal(captured["max_tokens"], &maxTokens); err != nil {
		t.Fatalf("decode max_tokens: %v", err)
	}
	if maxTokens != 384 {
		t.Fatalf("max_tokens = %d, want 384", maxTokens)
	}
}

func TestFlexInferCouncilEditorSplitsArtifacts(t *testing.T) {
	cli := newStubClient(t, `{
		"model":"qwen3-8b",
		"choices":[{"message":{"role":"assistant","content":"## Research\nR\n\n## Product Spec\nP\n\n## Implementation Plan\nI"}}],
		"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}
	}`, 200)
	editor := &FlexInferCouncilEditor{Client: cli, Model: "qwen3-8b"}

	out, err := editor.Edit(context.Background(), &council.Brief{Markdown: "Brief"}, []council.ReviewerOutput{
		{Lens: council.ReviewerLens{Name: "architecture", Model: "qwen3-8b", Backend: "flexinfer"}, CostUSD: 0.02},
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if len(out.Documents) != 3 {
		t.Fatalf("documents = %d, want 3", len(out.Documents))
	}
	if out.Documents[0].Body != "R" || out.Documents[1].Body != "P" || out.Documents[2].Body != "I" {
		t.Fatalf("documents not split correctly: %+v", out.Documents)
	}
	if out.Sidecar.CostUSD.Local != out.CostUSD || out.Sidecar.CostUSD.Local <= 0 {
		t.Fatalf("local cost = %v, want editor-only cost %v", out.Sidecar.CostUSD.Local, out.CostUSD)
	}
	const baseNote = "generated by FlexInfer-backed council participants"
	if !strings.HasPrefix(out.Sidecar.Notes, baseNote) {
		t.Fatalf("notes = %q, want prefix %q", out.Sidecar.Notes, baseNote)
	}
	// The stub output has no "## Backlog Proposals" section, so the run records
	// WHY it created zero backlog items (marker absent) instead of an opaque 0.
	if !strings.Contains(out.Sidecar.Notes, `omitted the "## Backlog Proposals" section`) {
		t.Errorf("notes should record the absent-proposals reason, got %q", out.Sidecar.Notes)
	}
	if out.Sidecar.BacklogDeltas.Created != 0 {
		t.Errorf("created = %d, want 0 (stub has no proposals block)", out.Sidecar.BacklogDeltas.Created)
	}
}

func TestFlexInferCouncilEditor_ZeroCostErrorOnlyMarksRemoteSpendUnpriced(t *testing.T) {
	for _, tc := range []struct {
		name         string
		backend      string
		wantUnpriced bool
	}{
		{name: "local outage", backend: "flexinfer", wantUnpriced: false},
		{name: "remote outage", backend: "litellm", wantUnpriced: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli := newStubClient(t, `{"error":"unavailable"}`, http.StatusServiceUnavailable)
			editor := &FlexInferCouncilEditor{Client: cli, Model: "qwen3-8b", Backend: tc.backend}

			out, err := editor.Edit(context.Background(), &council.Brief{Markdown: "Brief"}, nil)
			if err == nil {
				t.Fatal("expected provider error")
			}
			if out == nil {
				t.Fatal("expected cost-bearing error output")
			}
			if out.CostUnpriced != tc.wantUnpriced {
				t.Fatalf("CostUnpriced = %v, want %v for backend %q", out.CostUnpriced, tc.wantUnpriced, tc.backend)
			}
		})
	}
}

func TestFlexInferCouncilEditor_NoChoicesPreservesUsageCost(t *testing.T) {
	for _, tc := range []struct {
		name         string
		backend      string
		body         string
		wantCost     float64
		wantFrontier bool
	}{
		{
			name: "local token estimate", backend: "flexinfer",
			body:     `{"model":"local","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20}}`,
			wantCost: (100 + 20) * 0.0002 / 1000,
		},
		{
			name: "remote provider cost", backend: "litellm",
			body:     `{"model":"remote","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"cost":0.17}}`,
			wantCost: 0.17, wantFrontier: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli := newStubClient(t, tc.body, http.StatusOK)
			editor := &FlexInferCouncilEditor{Client: cli, Model: "model", Backend: tc.backend}

			out, err := editor.Edit(context.Background(), &council.Brief{Markdown: "Brief"}, nil)
			if err == nil {
				t.Fatal("expected no-choices error")
			}
			if out == nil || math.Abs(out.CostUSD-tc.wantCost) > 1e-12 {
				t.Fatalf("output = %+v, want cost %.8f", out, tc.wantCost)
			}
			if out.CostUnpriced {
				t.Fatal("available usage accounting was marked unpriced")
			}
			if tc.wantFrontier && out.Sidecar.CostUSD.Frontier != tc.wantCost {
				t.Fatalf("frontier sidecar = %+v, want %.8f", out.Sidecar.CostUSD, tc.wantCost)
			}
			if !tc.wantFrontier && out.Sidecar.CostUSD.Local != tc.wantCost {
				t.Fatalf("local sidecar = %+v, want %.8f", out.Sidecar.CostUSD, tc.wantCost)
			}
		})
	}
}

// Regression: a model that returns an empty (or whitespace-only) string
// must surface as EditorOutput.Empty=true so the runner can demote the
// council run's outcome to error instead of silently writing a placeholder
// and marking the run success.
func TestFlexInferCouncilEditorMarksEmptyResponse(t *testing.T) {
	cli := newStubClient(t, `{
		"model":"gemma4-26b-a4b-gptq",
		"choices":[{"message":{"role":"assistant","content":"   \n  "}}],
		"usage":{"prompt_tokens":40,"completion_tokens":0,"total_tokens":40}
	}`, 200)
	editor := &FlexInferCouncilEditor{Client: cli, Model: "gemma4-26b-a4b-gptq"}

	out, err := editor.Edit(context.Background(), &council.Brief{Markdown: "Brief"}, nil)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !out.Empty {
		t.Fatalf("Empty = false, want true for whitespace-only response")
	}
	if !strings.Contains(out.Sidecar.Notes, "no content") {
		t.Fatalf("Sidecar.Notes = %q, want explanation of empty response", out.Sidecar.Notes)
	}
}

func TestFlexInferCouncilEditorNonEmptyResponseNotMarkedEmpty(t *testing.T) {
	cli := newStubClient(t, `{
		"model":"gemma4-26b-a4b-gptq",
		"choices":[{"message":{"role":"assistant","content":"## Research\nFindings."}}],
		"usage":{"prompt_tokens":40,"completion_tokens":10,"total_tokens":50}
	}`, 200)
	editor := &FlexInferCouncilEditor{Client: cli, Model: "gemma4-26b-a4b-gptq"}

	out, err := editor.Edit(context.Background(), &council.Brief{Markdown: "Brief"}, nil)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if out.Empty {
		t.Fatalf("Empty = true, want false for non-empty response")
	}
}

// Regression: the editor must stamp Sidecar.StartedAt before the chat
// call so the artifact writer preserves a real start time. Without
// this, both StartedAt and EndedAt would get the same post-write
// timestamp and the Council tab would render every run as 0ms /
// 'instant' even for runs that took multiple seconds.
func TestFlexInferCouncilEditorStampsStartedAt(t *testing.T) {
	cli := newStubClient(t, `{
		"model":"gemma4-26b-a4b-gptq",
		"choices":[{"message":{"role":"assistant","content":"## Research\nFindings."}}],
		"usage":{"prompt_tokens":40,"completion_tokens":10,"total_tokens":50}
	}`, 200)
	editor := &FlexInferCouncilEditor{Client: cli, Model: "gemma4-26b-a4b-gptq"}

	before := time.Now().UTC().Add(-time.Second)
	out, err := editor.Edit(context.Background(), &council.Brief{Markdown: "Brief"}, nil)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	if out.Sidecar.StartedAt.IsZero() {
		t.Fatalf("Sidecar.StartedAt was zero; runner can't tell start from end")
	}
	if out.Sidecar.StartedAt.Before(before) || out.Sidecar.StartedAt.After(after) {
		t.Errorf("Sidecar.StartedAt = %v, want within [%v, %v]", out.Sidecar.StartedAt, before, after)
	}
}

func TestFlexInferEvalJudgeParsesContradictionVerdict(t *testing.T) {
	cli := newStubClient(t, `{
		"model":"qwen3-8b",
		"choices":[{"message":{"role":"assistant","content":"verdict: {\"score\":0.91,\"findings\":[\"consistent\"]}"}}],
		"usage":{"prompt_tokens":80,"completion_tokens":20,"total_tokens":100}
	}`, 200)
	judge := &FlexInferEvalJudge{Client: cli}

	result, err := judge.JudgeContradiction(context.Background(), eval.Input{
		Sidecar:      &council.Sidecar{Models: []string{"qwen3-8b"}},
		EditorOutput: &council.EditorOutput{Documents: []council.ArtifactDoc{{Kind: council.KindImplementation, Body: "Plan"}}},
	})
	if err != nil {
		t.Fatalf("JudgeContradiction: %v", err)
	}
	if result.Score != 0.91 {
		t.Fatalf("score = %v, want 0.91", result.Score)
	}
	if len(result.Findings) != 1 || result.Findings[0] != "consistent" {
		t.Fatalf("findings = %#v", result.Findings)
	}
	if result.CostUSD <= 0 {
		t.Fatalf("cost = %v, want provider cost", result.CostUSD)
	}
}

func TestFlexInferEvalJudgeParseErrorPreservesProviderCost(t *testing.T) {
	cli := newStubClient(t, `{
		"model":"qwen3-8b",
		"choices":[{"message":{"role":"assistant","content":"not-json"}}],
		"usage":{"prompt_tokens":80,"completion_tokens":20,"total_tokens":100}
	}`, 200)
	judge := &FlexInferEvalJudge{Client: cli, Backend: "litellm"}

	result, err := judge.JudgeContradiction(context.Background(), eval.Input{})
	if err == nil {
		t.Fatal("expected parse error")
	}
	if result.CostUSD <= 0 {
		t.Fatalf("cost = %v, want provider cost on parse failure", result.CostUSD)
	}
	if result.Backend != "litellm" {
		t.Fatalf("backend = %q, want litellm", result.Backend)
	}
}

func TestFlexInferEvalJudgeRetriesEmptyReasoningResponseWithBoostedBudget(t *testing.T) {
	var requests []chatRequest
	cli, err := NewFlexInferClient(FlexInferConfig{ProxyURL: "http://stub"})
	if err != nil {
		t.Fatal(err)
	}
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		var captured chatRequest
		if err := json.NewDecoder(req.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requests = append(requests, captured)

		body := `{
			"model":"or/kimi-k3",
			"choices":[{"message":{"role":"assistant","content":null,"reasoning_content":"thinking"},"finish_reason":"length"}],
			"usage":{"prompt_tokens":100,"completion_tokens":1024,"total_tokens":1124,"cost":0.01,"completion_tokens_details":{"reasoning_tokens":1024}}
		}`
		if len(requests) == 2 {
			body = `{
				"model":"or/kimi-k3",
				"choices":[{"message":{"role":"assistant","content":"{\"score\":0.93,\"findings\":[\"consistent\"]}"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":110,"completion_tokens":40,"total_tokens":150,"cost":0.02}
			}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	}))

	judge := &FlexInferEvalJudge{
		Client: cli, Model: "or/kimi-k3", Backend: "litellm", MaxTokens: 1024,
	}
	result, err := judge.JudgeContradiction(context.Background(), eval.Input{})
	if err != nil {
		t.Fatalf("JudgeContradiction: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want one bounded retry", len(requests))
	}
	if requests[0].MaxTokens != 1024 || requests[1].MaxTokens != judgeReasoningRetryFloorTokens {
		t.Fatalf("max_tokens = [%d, %d], want [1024, %d]",
			requests[0].MaxTokens, requests[1].MaxTokens, judgeReasoningRetryFloorTokens)
	}
	if !strings.Contains(requests[1].Messages[0].Content, `"findings"`) {
		t.Fatalf("retry prompt missing contradiction envelope: %q", requests[1].Messages[0].Content)
	}
	if result.Score != 0.93 || len(result.Findings) != 1 || result.Findings[0] != "consistent" {
		t.Fatalf("result = %+v, want recovered contradiction verdict", result)
	}
	if math.Abs(result.CostUSD-0.03) > 1e-9 {
		t.Fatalf("cost = %v, want both attempts (0.03)", result.CostUSD)
	}
	if result.CostUnpriced {
		t.Fatal("provider reported both attempt costs; want priced result")
	}
}

func TestFlexInferEvalJudge_RemoteEstimateWithoutProviderCostMarksUnpriced(t *testing.T) {
	cli := newStubClient(t, `{
		"model":"remote-model",
		"choices":[{"message":{"role":"assistant","content":"{\"score\":0.9,\"findings\":[]}"}}],
		"usage":{"prompt_tokens":80,"completion_tokens":20,"total_tokens":100}
	}`, http.StatusOK)
	judge := &FlexInferEvalJudge{Client: cli, Backend: "litellm"}

	result, err := judge.JudgeContradiction(context.Background(), eval.Input{})
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	if result.CostUSD <= 0 || !result.CostUnpriced {
		t.Fatalf("result = %+v, want positive fallback estimate plus unpriced marker", result)
	}
}

func TestFlexInferEvalJudge_PricesKnownOpenAIGatewayModelWithoutProviderCost(t *testing.T) {
	cli := newStubClient(t, `{
		"model":"oa/gpt-5.6-terra",
		"choices":[{"message":{"role":"assistant","content":"{\"score\":0.9,\"findings\":[]}"}}],
		"usage":{"prompt_tokens":1000,"completion_tokens":100,"total_tokens":1100,"prompt_tokens_details":{"cached_tokens":400}}
	}`, http.StatusOK)
	judge := &FlexInferEvalJudge{Client: cli, Model: "oa/gpt-5.6-terra", Backend: "litellm"}

	result, err := judge.JudgeContradiction(context.Background(), eval.Input{})
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	want := (600*2.50 + 400*0.25 + 100*15.0) / 1_000_000
	if math.Abs(result.CostUSD-want) > 1e-12 || result.CostUnpriced {
		t.Fatalf("result = %+v; want priced cost %.8f", result, want)
	}
}

func TestFlexInferEvalJudge_KnownGatewayModelWithoutUsageMarksCostUnpriced(t *testing.T) {
	cli := newStubClient(t, `{
		"model":"oa/gpt-5.6-terra",
		"choices":[{"message":{"role":"assistant","content":"{\"score\":0.9,\"findings\":[]}"}}]
	}`, http.StatusOK)
	judge := &FlexInferEvalJudge{Client: cli, Model: "oa/gpt-5.6-terra", Backend: "litellm"}

	result, err := judge.JudgeContradiction(context.Background(), eval.Input{})
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	if !result.CostUnpriced {
		t.Fatal("known gateway model without usage must preserve conservative reservation accounting")
	}
}

func TestFlexInferEvalJudgeDisablesThinking(t *testing.T) {
	t.Setenv("FLEXINFER_JUDGE_MAX_TOKENS", "4096")
	var captured map[string]json.RawMessage
	cli, err := NewFlexInferClient(FlexInferConfig{ProxyURL: "http://stub"})
	if err != nil {
		t.Fatal(err)
	}
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		body := `{"model":"qwen35","choices":[{"message":{"role":"assistant","content":"{\"score\":0.95,\"findings\":[]}"}}]}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	}))

	judge := &FlexInferEvalJudge{Client: cli, Model: "qwen35"}
	if _, err := judge.JudgeContradiction(context.Background(), eval.Input{}); err != nil {
		t.Fatalf("JudgeContradiction: %v", err)
	}
	var kwargs struct {
		EnableThinking *bool `json:"enable_thinking"`
	}
	if err := json.Unmarshal(captured["chat_template_kwargs"], &kwargs); err != nil {
		t.Fatalf("decode chat_template_kwargs: %v", err)
	}
	if kwargs.EnableThinking == nil || *kwargs.EnableThinking {
		t.Fatalf("enable_thinking = %v, want false", kwargs.EnableThinking)
	}
	var maxTokens int
	if err := json.Unmarshal(captured["max_tokens"], &maxTokens); err != nil {
		t.Fatalf("decode max_tokens: %v", err)
	}
	if maxTokens != 4096 {
		t.Fatalf("max_tokens = %d, want FLEXINFER_JUDGE_MAX_TOKENS value 4096", maxTokens)
	}
}

func TestBuildContradictionPromptGroundsPrivateStateAndTime(t *testing.T) {
	now := time.Date(2026, time.July, 16, 15, 37, 16, 0, time.UTC)
	prompt := buildContradictionPrompt(eval.Input{
		Now: func() time.Time { return now },
		Sidecar: &council.Sidecar{
			CouncilRunID: "COUNCIL-2026-07-16-153716",
			Models:       []string{"qwen35-internal"},
		},
	})

	for _, want := range []string{
		"2026-07-16T15:37:16Z",
		"authoritative private system state",
		"training cutoff",
		"Dates, model names, run IDs, and internal paths",
		"Do not infer a contradiction merely because no comparison evidence was supplied",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestParseContradictionVerdictSkipsExampleBeforeFinalVerdict(t *testing.T) {
	raw := "Thinking Process:\nSchema example: {\"score\": \"0..1\", \"findings\": []}\n" +
		"Final: {\"score\": 0.88, \"findings\": [\"consistent with recent state\"]}"
	score, findings, err := parseContradictionVerdict(raw)
	if err != nil {
		t.Fatalf("parseContradictionVerdict: %v", err)
	}
	if score != 0.88 || len(findings) != 1 {
		t.Fatalf("score=%v findings=%v, want 0.88 and one finding", score, findings)
	}
}
