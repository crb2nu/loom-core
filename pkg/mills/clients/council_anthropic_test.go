package clients

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/llmusage"
	"github.com/crb2nu/loom/pkg/mills/council"
)

// fakeAnthropicMessenger is an anthropicMessenger double for editor tests.
type fakeAnthropicMessenger struct {
	res          anthropicMessageResult
	err          error
	gotModel     string
	gotSystem    string
	gotPrompt    string
	gotMax       int64
	gotDisabled  bool
	gotComponent string
	calls        int
}

func (f *fakeAnthropicMessenger) CreateMessage(ctx context.Context, req anthropicMessageRequest) (anthropicMessageResult, error) {
	f.calls++
	f.gotModel = req.Model
	f.gotSystem = req.System
	f.gotPrompt = req.Prompt
	f.gotMax = req.MaxTokens
	f.gotDisabled = req.DisableThinking
	// The component label travels on ctx exactly as the real client's Observe
	// reads it, so capturing it here exercises the same mechanism.
	f.gotComponent = llmusage.ComponentFrom(ctx)
	return f.res, f.err
}

func TestAnthropicEditor_ParsesDocsAndProposals(t *testing.T) {
	raw := "## Research\nr\n## Product Spec\ns\n## Implementation Plan\nplan body\n## Backlog Proposals\n```json\n" +
		`{"proposals":[{"title":"Do X","priority":"P2","slices":[{"name":"a","goal":"do a","files":["a.go"]}]}]}` +
		"\n```\n"
	fake := &fakeAnthropicMessenger{res: anthropicMessageResult{
		Text: raw, InputTokens: 11, OutputTokens: 22,
		CacheReadInputTokens: 500, CacheCreationInputTokens: 0,
	}}
	ed := &AnthropicCouncilEditor{Client: fake, Model: "claude-opus-4-8"}

	out, err := ed.Edit(context.Background(), &council.Brief{Markdown: "decompose X"}, nil)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if out.Empty {
		t.Error("Empty=true, want false")
	}
	if out.Model != "claude-opus-4-8" {
		t.Errorf("Model=%q", out.Model)
	}
	if out.Backend != "anthropic" {
		t.Errorf("Backend=%q, want anthropic", out.Backend)
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
	// The turn drives the configured model with a non-zero token budget.
	if fake.gotModel != "claude-opus-4-8" {
		t.Errorf("request model=%q", fake.gotModel)
	}
	if fake.gotMax <= 0 {
		t.Errorf("max tokens=%d, want the package default", fake.gotMax)
	}
	// The editor must keep adaptive thinking: claude-fable-5 rejects an
	// explicit disabled config with a 400, and plan decomposition wants it.
	if fake.gotDisabled {
		t.Error("editor request must not set DisableThinking")
	}
	// Prompt caching split: the STABLE instruction prefix rides the cached
	// system block; only the VOLATILE brief reaches the user message.
	if !strings.Contains(fake.gotSystem, "Loom Mills council editor") {
		t.Errorf("system prefix missing the stable instructions: %q", fake.gotSystem)
	}
	if strings.Contains(fake.gotSystem, "decompose X") {
		t.Error("system prefix leaked the volatile brief; it must stay in the user message for caching")
	}
	if !strings.Contains(fake.gotPrompt, "decompose X") {
		t.Errorf("user prompt missing the brief: %q", fake.gotPrompt)
	}
	// Token usage + cache activity flow into the sidecar notes for audit.
	if !strings.Contains(out.Sidecar.Notes, "in=11 out=22 cache_read=500 cache_write=0") {
		t.Errorf("notes missing token/cache usage: %q", out.Sidecar.Notes)
	}
	// Opus 4.8: input $5/M, cache hits $0.50/M, output $25/M.
	wantCost := (11*5.0 + 500*0.50 + 22*25.0) / 1_000_000
	if out.CostUSD != wantCost || out.Sidecar.CostUSD.Frontier != wantCost {
		t.Errorf("cost = %.8f sidecar = %.8f, want %.8f",
			out.CostUSD, out.Sidecar.CostUSD.Frontier, wantCost)
	}
	if out.CostUnpriced {
		t.Error("known Opus 4.8 price marked unpriced")
	}
}

func TestAnthropicEditor_UnknownModelMarksCostUnpriced(t *testing.T) {
	fake := &fakeAnthropicMessenger{res: anthropicMessageResult{
		Text:        "## Research\nr\n## Product Spec\ns\n## Implementation Plan\np",
		InputTokens: 100, OutputTokens: 50,
	}}
	ed := &AnthropicCouncilEditor{Client: fake, Model: "claude-future-unpriced"}

	out, err := ed.Edit(context.Background(), &council.Brief{Markdown: "x"}, nil)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !out.CostUnpriced {
		t.Fatal("unknown paid model should mark cost unpriced for conservative reservation accounting")
	}
}

func TestAnthropicEditor_FableTokenPricing(t *testing.T) {
	fake := &fakeAnthropicMessenger{res: anthropicMessageResult{
		Text:                     "## Research\nr\n## Product Spec\ns\n## Implementation Plan\np",
		InputTokens:              8_907,
		OutputTokens:             7_037,
		CacheCreationInputTokens: 3_821,
		CacheReadInputTokens:     1_000,
	}}
	ed := &AnthropicCouncilEditor{Client: fake, Model: "claude-fable-5"}

	out, err := ed.Edit(context.Background(), &council.Brief{Markdown: "x"}, nil)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	// Fable: input $10/M, cache write $12.50/M, cache read $1/M, output $50/M.
	want := (8_907*10.0 + 3_821*12.50 + 1_000*1.0 + 7_037*50.0) / 1_000_000
	if out.CostUnpriced || out.CostUSD != want || out.CostUSD >= 1 {
		t.Fatalf("cost, unpriced = %.8f, %v; want sub-$1 %.8f, false", out.CostUSD, out.CostUnpriced, want)
	}
}

func TestAnthropicEditor_KnownModelMissingUsageMarksCostUnpriced(t *testing.T) {
	fake := &fakeAnthropicMessenger{res: anthropicMessageResult{
		Text: "## Research\nr\n## Product Spec\ns\n## Implementation Plan\np",
	}}
	ed := &AnthropicCouncilEditor{Client: fake, Model: "claude-opus-4-8"}

	out, err := ed.Edit(context.Background(), &council.Brief{Markdown: "x"}, nil)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !out.CostUnpriced {
		t.Fatal("known paid model response without usage should remain conservatively unpriced")
	}
}

// TestCouncilEditorPromptParts_StableVolatileSplit pins the caching contract:
// the two halves concatenate to the original single-string prompt (so the
// flexinfer/OpenAI editors are unaffected), the stable half carries the
// repo-layout grounding, and the brief lives only in the volatile half.
func TestCouncilEditorPromptParts_StableVolatileSplit(t *testing.T) {
	brief := &council.Brief{Markdown: "UNIQUE-BRIEF-MARKER harden the importer"}
	repoTree := "pkg/\n  mills/\n    clients/"
	stable, volatile := buildCouncilEditorPromptParts(brief, nil, nil, repoTree, "")
	whole := buildCouncilEditorPrompt(brief, nil, nil, repoTree, "")

	if stable+volatile != whole {
		t.Fatal("stable+volatile != buildCouncilEditorPrompt (flexinfer/OpenAI would drift)")
	}
	if strings.Contains(stable, "UNIQUE-BRIEF-MARKER") {
		t.Error("stable prefix contains the brief — it must be brief-independent to cache")
	}
	if !strings.Contains(volatile, "UNIQUE-BRIEF-MARKER") {
		t.Error("volatile half is missing the brief")
	}
	if !strings.Contains(stable, "Repository layout") {
		t.Error("stable prefix should carry the repo-layout grounding")
	}
}

func TestAnthropicEditor_EmptyOutputMarksEmpty(t *testing.T) {
	fake := &fakeAnthropicMessenger{res: anthropicMessageResult{Text: "   \n  "}}
	ed := &AnthropicCouncilEditor{Client: fake, Model: "claude-opus-4-8"}
	out, err := ed.Edit(context.Background(), &council.Brief{Markdown: "x"}, nil)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !out.Empty {
		t.Error("Empty=false on whitespace-only output, want true")
	}
}

// A safety-classifier refusal is treated as an unusable run (Empty=true) so the
// FallbackCouncilEditor degrades to flexinfer, and the reason is auditable in
// the sidecar notes.
func TestAnthropicEditor_RefusalMarksEmpty(t *testing.T) {
	fake := &fakeAnthropicMessenger{res: anthropicMessageResult{Text: "", Refusal: true}}
	ed := &AnthropicCouncilEditor{Client: fake, Model: "claude-fable-5"}
	out, err := ed.Edit(context.Background(), &council.Brief{Markdown: "x"}, nil)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !out.Empty {
		t.Error("Empty=false on a refusal, want true")
	}
	if !strings.Contains(out.Sidecar.Notes, "refused") {
		t.Errorf("notes should record the refusal: %q", out.Sidecar.Notes)
	}
}

func TestAnthropicEditor_NilBriefErrors(t *testing.T) {
	ed := &AnthropicCouncilEditor{Client: &fakeAnthropicMessenger{}, Model: "claude-opus-4-8"}
	if _, err := ed.Edit(context.Background(), nil, nil); err == nil {
		t.Fatal("want error on nil brief")
	}
}

func TestNewAnthropicClient_RequiresKey(t *testing.T) {
	if _, err := NewAnthropicClient(AnthropicClientConfig{APIKey: "   "}); err == nil {
		t.Fatal("want error when API key is blank")
	}
	if _, err := NewAnthropicClient(AnthropicClientConfig{APIKey: "sk-ant-test"}); err != nil {
		t.Fatalf("unexpected error with a key: %v", err)
	}
}

// TestAnthropicEditor_Live exercises the real Messages API. Skipped unless
// ANTHROPIC_API_KEY (or LOOM_ANTHROPIC_API_KEY) is set, so CI stays offline.
// Run locally with the operator's key to validate that the configured Claude
// model decomposes a brief into a `## Backlog Proposals` block with slices.
func TestAnthropicEditor_Live(t *testing.T) {
	key := AnthropicAPIKeyFromEnv()
	if key == "" {
		t.Skip("set ANTHROPIC_API_KEY (or LOOM_ANTHROPIC_API_KEY) to run the live Claude council editor test")
	}
	model := os.Getenv("LOOM_COUNCIL_EDITOR_MODEL")
	if model == "" {
		model = "claude-opus-4-8"
	}
	client, err := NewAnthropicClient(AnthropicClientConfig{
		APIKey:  key,
		BaseURL: AnthropicBaseURLFromEnv(),
		Timeout: 180 * time.Second,
	})
	if err != nil {
		t.Fatalf("api client: %v", err)
	}
	ed := &AnthropicCouncilEditor{Client: client, Model: model}
	brief := &council.Brief{Markdown: strings.TrimSpace(`
# Council brief

We want to harden the Mills operator's HTTP surface. Three independent,
separately-mergeable improvements are in scope:
1. Add a GET /healthz liveness endpoint.
2. Add a GET /readyz readiness endpoint that checks the store + policy load.
3. Add structured request logging middleware.

Each can ship as its own MR. Produce the research / product-spec / implementation
docs, and a Backlog Proposals block decomposing this into independent slices.`)}

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	out, err := ed.Edit(ctx, brief, nil)
	if err != nil {
		t.Fatalf("live edit: %v", err)
	}
	t.Logf("live: model=%s docs=%d proposals=%d notes=%q", model, len(out.Documents), len(out.BacklogProposals), out.Sidecar.Notes)
	for i, p := range out.BacklogProposals {
		t.Logf("  proposal[%d] title=%q priority=%s slices=%d", i, p.Title, p.Priority, len(p.PlanSlices))
	}
	if out.Empty {
		t.Fatal("live editor returned empty output")
	}
	if len(out.Documents) != 3 {
		t.Fatalf("docs=%d, want 3", len(out.Documents))
	}
}
