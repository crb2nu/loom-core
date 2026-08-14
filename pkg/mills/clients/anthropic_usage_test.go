package clients

import (
	"context"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/gates"
)

// TestAnthropicUsageMapsToLLMUsage pins the Anthropic → llmusage field mapping.
// A swap here would be invisible at compile time (all four SDK fields are
// int64) and would silently misreport the council's warm share — the exact
// number this instrumentation exists to make trustworthy.
func TestAnthropicUsageMapsToLLMUsage(t *testing.T) {
	got := anthropicLLMUsage(anthropic.Usage{
		InputTokens:              210,
		OutputTokens:             30,
		CacheReadInputTokens:     3968,
		CacheCreationInputTokens: 512,
	})
	if got.PromptTokens != 210 {
		t.Errorf("prompt tokens = %d, want 210 (input_tokens — Anthropic's uncached remainder, NOT the total prompt)", got.PromptTokens)
	}
	if got.CachedPromptTokens != 3968 {
		t.Errorf("cached prompt tokens = %d, want 3968 (cache_read_input_tokens)", got.CachedPromptTokens)
	}
	if got.CompletionTokens != 30 {
		t.Errorf("completion tokens = %d, want 30", got.CompletionTokens)
	}
	// The 512 cache_creation_input_tokens above appear in NO field: cache
	// writes are deliberately absent from the mapping — see the comment on
	// anthropicLLMUsage. The exact-value asserts above are what catch a write
	// count being folded into one of the three counters.
}

// TestAnthropicEditorTagsItsOwnComponent mirrors TestJudgeTagsItsOwnComponent
// for the Anthropic backend: without per-caller attribution, editor and judge
// traffic through the shared Anthropic client would be averaged into one
// indistinguishable series.
func TestAnthropicEditorTagsItsOwnComponent(t *testing.T) {
	fake := &fakeAnthropicMessenger{res: anthropicMessageResult{Text: "## Research\nr\n## Product Spec\ns\n## Implementation Plan\np\n"}}
	ed := &AnthropicCouncilEditor{Client: fake, Model: "claude-opus-4-8"}
	if _, err := ed.Edit(context.Background(), &council.Brief{Markdown: "decompose X"}, nil); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if fake.gotComponent != ComponentCouncilEditor {
		t.Errorf("component = %q, want %q", fake.gotComponent, ComponentCouncilEditor)
	}
}

// TestAnthropicJudgeTagsItsOwnComponent: the dissent-tiebreaker judge must
// land in the same mills-judge series as the FlexInfer primary so the two are
// comparable in the warm-share queries.
func TestAnthropicJudgeTagsItsOwnComponent(t *testing.T) {
	fake := &fakeAnthropicMessenger{res: anthropicMessageResult{
		Text: "```json\n{\"score\": 0.9, \"reasons\": [\"ok\"]}\n```",
	}}
	j := &AnthropicRubricJudge{Client: fake, Model: "claude-sonnet-5"}
	if _, err := j.Judge(context.Background(), gates.PRSelfReviewRubricName, gates.StageInput{}); err != nil {
		t.Fatalf("judge: %v", err)
	}
	if fake.gotComponent != ComponentJudge {
		t.Errorf("component = %q, want %q", fake.gotComponent, ComponentJudge)
	}
}
