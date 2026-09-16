// Package llmpricing is the single hard-coded snapshot of vendor list prices
// loom-core uses to turn token usage into an estimated USD cost.
//
// It exists because two consumers — the HUD Codex spawn parser
// (internal/hud/bridge) and the Mills council OpenAI editor/judges
// (pkg/mills/clients) — each carried their own OpenAI price table and the two
// drifted: the spawn table stopped at gpt-5 while production spawns ran
// gpt-5.6-*, so every Codex-run Mills stage was recorded at $0. One table,
// one snapshot date, one lookup rule.
//
// Rules:
//
//   - Lookup is EXACT. Gateway prefixes (LiteLLM's oa/) and aliases are the
//     caller's business; an unknown id must never inherit another model's
//     rate by accident.
//   - Unknown models are reported as unknown (ok=false). Whether to fall back
//     to a conservative default is a per-consumer budget decision, not a
//     pricing fact.
//   - Prices are the Standard tier. Flex/Batch discounts and Priority
//     surcharges are not modelled.
//
// Update on a release cadence; bump OpenAISnapshotDate when you do.
package llmpricing

import (
	"sort"
	"strings"
)

const (
	// OpenAISnapshotDate is the day the OpenAI table below was verified
	// against OpenAISource.
	OpenAISnapshotDate = "2026-09-04"
	// OpenAISource is the public price list the table is transcribed from.
	// (platform.openai.com/docs/pricing 301-redirects here.)
	OpenAISource = "https://developers.openai.com/api/docs/pricing"

	// OpenAILongContextThreshold is the prompt size above which OpenAI bills
	// the gpt-5.4+ families at the long-context rate: input doubles and
	// output is 1.5x (the price list shows e.g. gpt-5.5 $5/$30 → $10/$45,
	// gpt-5.6-sol $4/$20 → $8/$30). Applied to the TOTAL prompt (cached +
	// uncached), which is how OpenAI defines the context length.
	OpenAILongContextThreshold        = 272_000
	OpenAILongContextInputMultiplier  = 2.0
	OpenAILongContextOutputMultiplier = 1.5
)

// OpenAIPrice is the per-1M-token USD list price of one OpenAI model.
type OpenAIPrice struct {
	InputPer1M       float64 // uncached ("fresh") input
	CachedInputPer1M float64 // prompt-cache hits
	OutputPer1M      float64 // output, reasoning tokens included
}

// openAIPrices is the OpenAI price table. Standard tier, per 1M tokens.
//
// Snapshot: OpenAISnapshotDate, from OpenAISource. Every row was read off the
// live page that day; a model that is no longer listed there (gpt-5-codex)
// was dropped rather than carried at a stale rate.
var openAIPrices = map[string]OpenAIPrice{
	// gpt-6 (Codex default for eligible accounts)
	"gpt-6-astra": {InputPer1M: 10.00, CachedInputPer1M: 1.00, OutputPer1M: 50.00},

	// gpt-5.6 family (launched 2026-07-09; Codex CLI defaults, Mills
	// pipeline.stage_models pins terra for implement and sol for plan_slice)
	"gpt-5.6-sol":   {InputPer1M: 4.00, CachedInputPer1M: 0.40, OutputPer1M: 20.00},
	"gpt-5.6-terra": {InputPer1M: 2.00, CachedInputPer1M: 0.20, OutputPer1M: 12.00},
	"gpt-5.6-luna":  {InputPer1M: 0.20, CachedInputPer1M: 0.02, OutputPer1M: 1.20},

	// gpt-5.5 family (Codex ChatGPT-sign-in safe default; see
	// internal/hud/spawn_config.go). -pro has no cached rate on the price
	// list ("-"): cached input is charged at the full input rate.
	"gpt-5.5":     {InputPer1M: 5.00, CachedInputPer1M: 0.50, OutputPer1M: 30.00},
	"gpt-5.5-pro": {InputPer1M: 30.00, CachedInputPer1M: 30.00, OutputPer1M: 180.00},

	// gpt-5.4 family (council editor; ChatGPT-sign-in retires 5.4/5.4-mini
	// 2026-08-31, API access unaffected)
	"gpt-5.4":            {InputPer1M: 2.50, CachedInputPer1M: 0.25, OutputPer1M: 15.00},
	"gpt-5.4-2026-03-05": {InputPer1M: 2.50, CachedInputPer1M: 0.25, OutputPer1M: 15.00},
	"gpt-5.4-mini":       {InputPer1M: 0.75, CachedInputPer1M: 0.075, OutputPer1M: 4.50},
	"gpt-5.4-nano":       {InputPer1M: 0.20, CachedInputPer1M: 0.02, OutputPer1M: 1.25},
	"gpt-5.4-pro":        {InputPer1M: 30.00, CachedInputPer1M: 30.00, OutputPer1M: 180.00},

	// older gpt-5.x (gpt-5.3-codex and gpt-5.2 are deprecated for ChatGPT
	// sign-in but still priced for API keys)
	"gpt-5.3-codex": {InputPer1M: 1.75, CachedInputPer1M: 0.175, OutputPer1M: 14.00},
	"gpt-5.2":       {InputPer1M: 1.75, CachedInputPer1M: 0.175, OutputPer1M: 14.00},
	"gpt-5.1":       {InputPer1M: 1.25, CachedInputPer1M: 0.125, OutputPer1M: 10.00},
	"gpt-5":         {InputPer1M: 1.25, CachedInputPer1M: 0.125, OutputPer1M: 10.00},
	"gpt-5-mini":    {InputPer1M: 0.25, CachedInputPer1M: 0.025, OutputPer1M: 2.00},
	"gpt-5-nano":    {InputPer1M: 0.05, CachedInputPer1M: 0.005, OutputPer1M: 0.40},

	// gpt-4.1 family
	"gpt-4.1":      {InputPer1M: 2.00, CachedInputPer1M: 0.50, OutputPer1M: 8.00},
	"gpt-4.1-mini": {InputPer1M: 0.40, CachedInputPer1M: 0.10, OutputPer1M: 1.60},
	"gpt-4.1-nano": {InputPer1M: 0.10, CachedInputPer1M: 0.025, OutputPer1M: 0.40},

	// o-series
	"o3":      {InputPer1M: 2.00, CachedInputPer1M: 0.50, OutputPer1M: 8.00},
	"o3-mini": {InputPer1M: 1.10, CachedInputPer1M: 0.55, OutputPer1M: 4.40},
	"o4-mini": {InputPer1M: 1.10, CachedInputPer1M: 0.275, OutputPer1M: 4.40},
}

// LookupOpenAI returns the list price for an exact OpenAI model id.
// Surrounding whitespace is ignored; nothing else is normalised. ok=false
// means the model is not in the snapshot — the caller decides what that
// means for its budget.
func LookupOpenAI(model string) (OpenAIPrice, bool) {
	p, ok := openAIPrices[strings.TrimSpace(model)]
	return p, ok
}

// OpenAIModels lists every model id in the snapshot, sorted, for tests and
// diagnostics.
func OpenAIModels() []string {
	out := make([]string, 0, len(openAIPrices))
	for m := range openAIPrices {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// CostUSD prices one completion. uncachedInput and cachedInput are the two
// disjoint halves of the prompt (OpenAI's usage.input_tokens is their SUM;
// callers on that convention subtract before calling). Negative counts are
// treated as zero. The long-context multipliers apply when the whole prompt
// exceeds OpenAILongContextThreshold.
func (p OpenAIPrice) CostUSD(uncachedInput, cachedInput, output int) float64 {
	uncached := max(uncachedInput, 0)
	cached := max(cachedInput, 0)
	out := max(output, 0)
	inMul, outMul := 1.0, 1.0
	if uncached+cached > OpenAILongContextThreshold {
		inMul, outMul = OpenAILongContextInputMultiplier, OpenAILongContextOutputMultiplier
	}
	return inMul*(float64(uncached)*p.InputPer1M+float64(cached)*p.CachedInputPer1M)/1_000_000 +
		outMul*float64(out)*p.OutputPer1M/1_000_000
}

// OpenAICostUSD is LookupOpenAI followed by CostUSD. ok=false (and a zero
// cost) means the model is not in the snapshot.
func OpenAICostUSD(model string, uncachedInput, cachedInput, output int) (float64, bool) {
	p, ok := LookupOpenAI(model)
	if !ok {
		return 0, false
	}
	return p.CostUSD(uncachedInput, cachedInput, output), true
}
