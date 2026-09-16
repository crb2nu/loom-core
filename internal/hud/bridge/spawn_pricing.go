package bridge

import "github.com/crb2nu/loom/pkg/llmpricing"

// CodexModelPrice is the per-1M-token USD price of an OpenAI model. It is the
// shared llmpricing row: the spawn parser and the Mills council price from
// the same table (snapshot llmpricing.OpenAISnapshotDate) so the two can no
// longer drift apart.
type CodexModelPrice = llmpricing.OpenAIPrice

// DefaultCodexModel is the model a Codex spawn is billed as when the SDK
// stream never names one, and the rate an UNKNOWN model is billed at.
//
// It must equal the model `codex exec --model` is pinned to when nothing
// overrides it (internal/hud/spawn_config.go aliases this constant), so the
// no-metadata case is priced at the model that actually ran. It is also the
// most expensive non-pro model Codex can run under ChatGPT sign-in, which
// makes it the conservative choice for the unknown-model fallback: a model
// that is newer than the price snapshot over-estimates rather than reporting
// $0, and an over-estimate is the safe direction for a Mills budget cap.
const DefaultCodexModel = "gpt-5.5"

// EstimateCodexCost returns an estimated USD cost for a single Codex turn
// from fresh-input, cached-input and output token counts (Codex's
// usage.input_tokens INCLUDES the cached share; callers subtract first).
//
// known=false means model is not in the price snapshot and the turn was
// priced at DefaultCodexModel's rate instead. The cost is never 0 for
// non-zero usage: Mills stage records and budget accounting consume this
// figure, and an unknown model silently recorded as free was exactly how
// every gpt-5.6 spawn ran at $0 while the table stopped at gpt-5.
func EstimateCodexCost(model string, freshInput, cachedInput, output int) (usd float64, known bool) {
	if price, ok := llmpricing.LookupOpenAI(model); ok {
		return price.CostUSD(freshInput, cachedInput, output), true
	}
	price, ok := llmpricing.LookupOpenAI(DefaultCodexModel)
	if !ok {
		// Programming error: the default must always be priced. Guarded by
		// TestLookupCodexPrice_DefaultModelExists.
		return 0, false
	}
	return price.CostUSD(freshInput, cachedInput, output), false
}

// LookupCodexPrice returns the price entry for the given model, falling back
// to DefaultCodexModel if unknown. ok=false means even the default is
// missing (programming error).
func LookupCodexPrice(model string) (CodexModelPrice, bool) {
	if p, ok := llmpricing.LookupOpenAI(model); ok {
		return p, true
	}
	return llmpricing.LookupOpenAI(DefaultCodexModel)
}
