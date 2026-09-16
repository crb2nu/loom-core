package clients

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/spin"
	"github.com/crb2nu/loom/pkg/openairesponses"
)

// fakeCouncilEditor is a council.Editor double for FallbackCouncilEditor tests.
type fakeCouncilEditor struct {
	out   *council.EditorOutput
	err   error
	calls int
}

func (f *fakeCouncilEditor) Edit(_ context.Context, _ *council.Brief, _ []council.ReviewerOutput) (*council.EditorOutput, error) {
	f.calls++
	return f.out, f.err
}

func nonEmptyOutput() *council.EditorOutput {
	return &council.EditorOutput{Documents: []council.ArtifactDoc{{Kind: council.KindResearch}}}
}

func TestFallbackCouncilEditor_PrimaryErrorFallsBack(t *testing.T) {
	primary := &fakeCouncilEditor{err: errors.New("responses timeout")}
	fb := &fakeCouncilEditor{out: nonEmptyOutput()}
	e := &FallbackCouncilEditor{Primary: primary, Fallback: fb}
	out, err := e.Edit(context.Background(), &council.Brief{Markdown: "x"}, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if primary.calls != 1 || fb.calls != 1 {
		t.Fatalf("calls primary=%d fallback=%d, want 1/1", primary.calls, fb.calls)
	}
	if out == nil || out.Empty {
		t.Fatal("want non-empty fallback output")
	}
	if !out.CostUnpriced {
		t.Fatal("primary error without usage should preserve an unpriced-spend marker")
	}
}

func TestFallbackCouncilEditor_PrimaryEmptyFallsBack(t *testing.T) {
	primary := &fakeCouncilEditor{out: &council.EditorOutput{
		Empty: true, CostUSD: 0.30,
		Sidecar: council.Sidecar{CostUSD: council.SidecarCost{Frontier: 0.30}},
	}}
	fallbackOut := nonEmptyOutput()
	fallbackOut.CostUSD = 0.10
	fallbackOut.Sidecar.CostUSD.Local = 0.10
	fb := &fakeCouncilEditor{out: fallbackOut}
	e := &FallbackCouncilEditor{Primary: primary, Fallback: fb}
	out, err := e.Edit(context.Background(), &council.Brief{Markdown: "x"}, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if fb.calls != 1 {
		t.Fatalf("fallback calls=%d, want 1 (empty primary)", fb.calls)
	}
	if math.Abs(out.CostUSD-0.40) > 1e-12 {
		t.Fatalf("fallback total cost = %.2f, want paid primary + fallback 0.40", out.CostUSD)
	}
	if out.Sidecar.CostUSD.Frontier != 0.30 || out.Sidecar.CostUSD.Local != 0.10 {
		t.Fatalf("fallback cost split = %+v, want frontier/local 0.30/0.10", out.Sidecar.CostUSD)
	}
}

func TestFallbackCouncilEditor_PrimarySuccessNoFallback(t *testing.T) {
	primary := &fakeCouncilEditor{out: nonEmptyOutput()}
	fb := &fakeCouncilEditor{}
	e := &FallbackCouncilEditor{Primary: primary, Fallback: fb}
	if _, err := e.Edit(context.Background(), &council.Brief{Markdown: "x"}, nil); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(primary.out.FallbackHops) != 0 {
		t.Fatal("successful primary recorded a hop")
	}
	if fb.calls != 0 {
		t.Fatalf("fallback calls=%d, want 0 (primary succeeded)", fb.calls)
	}
}

type fakeResponsesClient struct {
	out    openairesponses.TurnResponse
	err    error
	gotReq openairesponses.TurnRequest
}

func (f *fakeResponsesClient) Create(_ context.Context, req openairesponses.TurnRequest) (openairesponses.TurnResponse, error) {
	f.gotReq = req
	return f.out, f.err
}

func TestOpenAIEditor_ParsesDocsAndProposals(t *testing.T) {
	raw := "## Research\nr\n## Product Spec\ns\n## Implementation Plan\nplan body\n## Backlog Proposals\n```json\n" +
		`{"proposals":[{"title":"Do X","priority":"P2","slices":[{"name":"a","goal":"do a","files":["a.go"]}]}]}` +
		"\n```\n"
	fake := &fakeResponsesClient{out: openairesponses.TurnResponse{OutputText: raw, PromptTokens: 10, CompletionTokens: 20, CachedTokens: 8}}
	ed := &OpenAIResponsesCouncilEditor{Client: fake, Model: "gpt-5.4"}

	out, err := ed.Edit(context.Background(), &council.Brief{Markdown: "decompose X"}, nil)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if out.Empty {
		t.Error("Empty=true, want false")
	}
	if out.Model != "gpt-5.4" {
		t.Errorf("Model=%q", out.Model)
	}
	if out.Backend != "openai-responses" {
		t.Errorf("Backend=%q, want openai-responses", out.Backend)
	}
	if len(out.Documents) != 3 {
		t.Fatalf("docs=%d, want 3", len(out.Documents))
	}
	if len(out.BacklogProposals) != 1 {
		t.Fatalf("proposals=%d, want 1", len(out.BacklogProposals))
	}
	if len(out.BacklogProposals[0].PlanSlices) != 1 || out.BacklogProposals[0].PlanSlices[0].Name != "a" {
		t.Errorf("plan slices=%+v", out.BacklogProposals[0].PlanSlices)
	}
	for _, d := range out.Documents {
		if d.Kind == council.KindImplementation && strings.Contains(d.Body, "## Backlog Proposals") {
			t.Error("implementation doc leaked the proposals JSON appendix")
		}
	}
	// The turn must be a single stateless call against the configured model.
	if fake.gotReq.Model != "gpt-5.4" {
		t.Errorf("request model=%q", fake.gotReq.Model)
	}
	if fake.gotReq.Context.Mode != openairesponses.ContextModeStateless {
		t.Errorf("request mode=%q, want stateless", fake.gotReq.Context.Mode)
	}
	// Prompt caching: the routing key is sent, and cache activity + real token
	// usage (previously unparsed, always 0) surface in the sidecar notes.
	if fake.gotReq.PromptCacheKey != openAICouncilPromptCacheKey {
		t.Errorf("prompt cache key=%q, want %q", fake.gotReq.PromptCacheKey, openAICouncilPromptCacheKey)
	}
	if !strings.Contains(out.Sidecar.Notes, "in=10 out=20 cached=8") {
		t.Errorf("notes missing token/cache usage: %q", out.Sidecar.Notes)
	}
}

func TestOpenAIEditor_EmptyOutputMarksEmpty(t *testing.T) {
	fake := &fakeResponsesClient{out: openairesponses.TurnResponse{OutputText: "   \n  "}}
	ed := &OpenAIResponsesCouncilEditor{Client: fake, Model: "gpt-5.4"}
	out, err := ed.Edit(context.Background(), &council.Brief{Markdown: "x"}, nil)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !out.Empty {
		t.Error("Empty=false on whitespace-only output, want true")
	}
}

func TestOpenAIEditor_UsesTokenPricingIncludingCachedInput(t *testing.T) {
	fake := &fakeResponsesClient{out: openairesponses.TurnResponse{
		OutputText:       "## Research\nr\n## Product Spec\ns\n## Implementation Plan\np",
		PromptTokens:     1_000,
		CachedTokens:     400,
		CompletionTokens: 100,
	}}
	ed := &OpenAIResponsesCouncilEditor{Client: fake, Model: "gpt-5.4"}
	reviews := []council.ReviewerOutput{{
		Lens:    council.ReviewerLens{Name: "security", Backend: "litellm", Model: "remote"},
		CostUSD: 0.02,
	}}

	out, err := ed.Edit(context.Background(), &council.Brief{Markdown: "x"}, reviews)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	// gpt-5.4: uncached input $2.50/M, cached input $0.25/M, output $15/M.
	want := 600*2.50/1_000_000 + 400*0.25/1_000_000 + 100*15.0/1_000_000
	if math.Abs(out.CostUSD-want) > 1e-12 {
		t.Fatalf("editor cost = %.8f, want %.8f", out.CostUSD, want)
	}
	if math.Abs(out.Sidecar.CostUSD.Frontier-want) > 1e-12 {
		t.Fatalf("sidecar frontier cost = %.8f, want editor-only %.8f", out.Sidecar.CostUSD.Frontier, want)
	}
	if out.CostUSD >= want+reviews[0].CostUSD {
		t.Fatalf("editor cost double-counted reviewer: got %.8f", out.CostUSD)
	}
}

func TestOpenAICouncilTokenPricing_GPT56GatewayAliases(t *testing.T) {
	tests := []struct {
		model string
		want  float64
	}{
		// oa/ is a LiteLLM gateway prefix, not part of the vendor model ID.
		// Rates are the pkg/llmpricing snapshot (Standard tier, 2026-09-04):
		// sol $4/$0.40/$20, terra $2/$0.20/$12, luna $0.20/$0.02/$1.20.
		{"oa/gpt-5.6-sol", (600*4.0 + 400*0.40 + 100*20.0) / 1_000_000},
		{"oa/gpt-5.6-terra", (600*2.0 + 400*0.20 + 100*12.0) / 1_000_000},
		{"oa/gpt-5.6-luna", (600*0.20 + 400*0.02 + 100*1.20) / 1_000_000},
	}
	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			got, ok := openAICouncilResponseCostUSD(tc.model, openairesponses.TurnResponse{
				PromptTokens: 1_000, CachedTokens: 400, CompletionTokens: 100,
			})
			if !ok || math.Abs(got-tc.want) > 1e-12 {
				t.Fatalf("cost, priced = %.8f, %v; want %.8f, true", got, ok, tc.want)
			}
		})
	}
}

func TestOpenAIEditor_UnknownModelMarksCostUnpriced(t *testing.T) {
	fake := &fakeResponsesClient{out: openairesponses.TurnResponse{
		OutputText:   "## Research\nr\n## Product Spec\ns\n## Implementation Plan\np",
		PromptTokens: 100, CompletionTokens: 50,
	}}
	ed := &OpenAIResponsesCouncilEditor{Client: fake, Model: "future-unpriced-model"}

	out, err := ed.Edit(context.Background(), &council.Brief{Markdown: "x"}, nil)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !out.CostUnpriced {
		t.Fatal("unknown paid model should mark cost unpriced for conservative reservation accounting")
	}
}

func TestOpenAIEditor_KnownModelMissingUsageMarksCostUnpriced(t *testing.T) {
	fake := &fakeResponsesClient{out: openairesponses.TurnResponse{
		OutputText: "## Research\nr\n## Product Spec\ns\n## Implementation Plan\np",
	}}
	ed := &OpenAIResponsesCouncilEditor{Client: fake, Model: "gpt-5.4"}

	out, err := ed.Edit(context.Background(), &council.Brief{Markdown: "x"}, nil)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !out.CostUnpriced {
		t.Fatal("known paid model response without usage should remain conservatively unpriced")
	}
}

// TestOpenAIEditor_Live exercises the real Responses API. It is skipped unless
// OPENAI_API_KEY is set, so CI stays offline. Run locally with the operator's
// key to validate that the configured model actually decomposes a brief into a
// `## Backlog Proposals` block with slices (the S3 decomposition kill-test).
func TestOpenAIEditor_Live(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("set OPENAI_API_KEY to run the live gpt-5.4 council editor test")
	}
	model := os.Getenv("LOOM_COUNCIL_EDITOR_MODEL")
	if model == "" {
		model = "gpt-5.4"
	}
	client, err := openairesponses.NewAPIClient(openairesponses.APIClientConfig{
		APIKey:  key,
		BaseURL: openairesponses.BaseURLFromEnv(),
	})
	if err != nil {
		t.Fatalf("api client: %v", err)
	}
	ed := &OpenAIResponsesCouncilEditor{Client: client, Model: model}
	brief := &council.Brief{Markdown: strings.TrimSpace(`
# Council brief

We want to harden the Mills operator's HTTP surface. Three independent,
separately-mergeable improvements are in scope:
1. Add a GET /healthz liveness endpoint.
2. Add a GET /readyz readiness endpoint that checks the store + policy load.
3. Add structured request logging middleware.

Each can ship as its own MR. Produce the research / product-spec / implementation
docs, and a Backlog Proposals block decomposing this into independent slices.`)}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	out, err := ed.Edit(ctx, brief, nil)
	if err != nil {
		t.Fatalf("live edit: %v", err)
	}
	t.Logf("live: model=%s docs=%d proposals=%d notes=%q", model, len(out.Documents), len(out.BacklogProposals), out.Sidecar.Notes)
	for i, p := range out.BacklogProposals {
		t.Logf("  proposal[%d] title=%q priority=%s slices=%d", i, p.Title, p.Priority, len(p.PlanSlices))
		for j, s := range p.PlanSlices {
			t.Logf("    slice[%d] name=%q goal=%q files=%v", j, s.Name, s.Goal, s.Files)
		}
	}
	if out.Empty {
		t.Fatal("live editor returned empty output")
	}
	if len(out.Documents) != 3 {
		t.Fatalf("docs=%d, want 3", len(out.Documents))
	}
}

func TestFallbackCouncilEditorHopEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"billing", &VendorError{Vendor: "anthropic", Kind: VendorBilling, Err: errors.New("hold")}},
		{"auth", &VendorError{Vendor: "anthropic", Kind: VendorAuth, Err: errors.New("key")}},
		{"breaker_open", &VendorError{Vendor: "anthropic", Kind: VendorBilling, Err: ErrVendorBreakerOpen}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := nonEmptyOutput()
			out.Backend = "openai"
			out.Model = "gpt-5.5"
			out.CostUSD = 2
			ed := &FallbackCouncilEditor{Primary: &fakeCouncilEditor{out: &council.EditorOutput{CostUSD: 1}, err: tc.err}, Fallback: &fakeCouncilEditor{out: out}, PrimaryLabel: "anthropic:opus"}
			got, err := ed.Edit(context.Background(), &council.Brief{}, nil)
			if err != nil || got.CostUSD != 3 || len(got.FallbackHops) != 1 {
				t.Fatalf("out=%+v err=%v", got, err)
			}
			want := council.EditorFallbackHop{Primary: "anthropic:opus", Destination: "openai:gpt-5.5", Kind: tc.name}
			if got.FallbackHops[0] != want {
				t.Fatalf("hop=%+v", got.FallbackHops)
			}
			if len(out.FallbackHops) != 0 {
				t.Fatal("mutated shared output")
			}
		})
	}
}

func TestFallbackCouncilEditorNestedHopEvidence(t *testing.T) {
	out := nonEmptyOutput()
	out.Backend = "flexinfer"
	out.Model = "local"
	errVendor := &VendorError{Kind: VendorAuth, Err: errors.New("key")}
	ed := &FallbackCouncilEditor{PrimaryLabel: "anthropic:opus", Primary: &fakeCouncilEditor{err: errVendor}, Fallback: &FallbackCouncilEditor{PrimaryLabel: "openai:gpt", Primary: &fakeCouncilEditor{err: errVendor}, Fallback: &fakeCouncilEditor{out: out}}}
	got, err := ed.Edit(context.Background(), &council.Brief{}, nil)
	if err != nil || len(got.FallbackHops) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	for _, hop := range got.FallbackHops {
		if hop.Destination != "flexinfer:local" {
			t.Fatalf("%+v", hop)
		}
	}
}

// Exercise the real fallback wrapper through SpinAll and the persisted spec renderer.
func TestSpinAllFrameFallbackIsolation(t *testing.T) {
	failure := &VendorError{Vendor: "anthropic", Kind: VendorBilling, Err: errors.New("billing hold")}
	out := &council.EditorOutput{Backend: "openai", Model: "gpt-5.5", CostUSD: 2, BacklogProposals: []council.BacklogProposal{{Title: "Retry", PlanSlices: []council.PlanSliceSpec{{Name: "retry", Goal: "retry failures"}}}}}
	author := &fallbackDraftAuthor{}
	s := &spin.Spinner{
		Enabled: func() bool { return true },
		Frame: func(name string) (spin.Frame, bool) {
			return spin.Frame{Name: name, Backend: "anthropic", Model: "opus"}, true
		},
		NewEditor: func(frame spin.Frame) (council.Editor, error) {
			fb := &fakeCouncilEditor{out: out}
			if frame.Name != "jacquard" {
				fb = &fakeCouncilEditor{err: failure}
			}
			return &FallbackCouncilEditor{PrimaryLabel: "anthropic:opus", Primary: &fakeCouncilEditor{err: failure}, Fallback: fb}, nil
		},
		Author: author,
	}
	got, err := s.SpinAll(context.Background(), spin.Request{Brief: "Add retries", Frames: []string{"jacquard", "down"}})
	if err != nil || len(got.Results) != 1 || len(got.Failures) != 1 || got.Failures[0].Frame != "down" {
		t.Fatalf("%+v %v", got, err)
	}
	if got.Results[0].Model != "gpt-5.5" || got.Results[0].CostUSD != 2 {
		t.Fatalf("%+v", got.Results[0])
	}
	if !strings.Contains(author.spec, "frame=jacquard primary=anthropic:opus fell back to openai:gpt-5.5: billing") {
		t.Fatal(author.spec)
	}
	failed, err := s.SpinAll(context.Background(), spin.Request{Brief: "Add retries", Frames: []string{"down", "down-too"}})
	if !errors.Is(err, failure) || len(failed.Results) != 0 || len(failed.Failures) != 2 {
		t.Fatalf("all down: %+v %v", failed, err)
	}
}

type fallbackDraftAuthor struct{ spec string }

func (a *fallbackDraftAuthor) AuthorDraftPlan(_ context.Context, in spin.DraftPlanInput) (string, error) {
	a.spec = draftSpecDoc(in)
	return "draft", nil
}

func TestFallbackCouncilEditorExhaustedDoesNotReuseFailedOutput(t *testing.T) {
	prior := nonEmptyOutput()
	prior.CostUSD = 1
	ed := &FallbackCouncilEditor{Primary: &fakeCouncilEditor{out: prior, err: errors.New("failed")}, Fallback: &fakeCouncilEditor{}}
	got, err := ed.Edit(context.Background(), &council.Brief{}, nil)
	if err != nil || got == nil || !got.Empty || got.CostUSD != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	if prior.Empty {
		t.Fatal("mutated primary output")
	}
}
