package clients

import (
	"strings"
	"sync"

	"github.com/crb2nu/loom/pkg/llmpricing"
)

// ModelPrice is a policy-supplied price row for one model on one provider.
// It lets the operator price a model the day it ships — a new frontier tier,
// a gateway alias — from the gitops policy instead of a code deploy. This
// matters more than it looks: every council/spin lookup below is EXACT-match,
// and an unpriced model does not merely report $0 — it loses the
// unpricedAttemptCeilingUSD guard and falls back to charging the whole run
// reservation (the $15 kimi-k3 incident, council.go). Pricing a model is the
// guard, so it must be reachable without shipping an image.
//
// Provider selects which table the row overlays: "anthropic" (Messages API,
// cache write/read rates), "openai" (Responses/LiteLLM oa/ rates), or
// "openrouter" (or/ ceilings). Keys are the exact model ids the policy
// routes — "claude-fable-5-1", "gpt-6-astra", "or/kimi-k3" — matched with the
// same normalisation as the static tables (oa/ stripped for openai only).
type ModelPrice struct {
	Provider              string
	InputPerMillion       float64
	CachedInputPerMillion float64
	CacheWritePerMillion  float64
	OutputPerMillion      float64
}

// Provider vocabulary accepted by RegisterModelPrices. Kept here (not in
// policy) so the price tables and the policy validator agree by construction.
const (
	ModelPriceProviderAnthropic  = "anthropic"
	ModelPriceProviderOpenAI     = "openai"
	ModelPriceProviderOpenRouter = "openrouter"
)

// ModelPriceProviders lists the accepted Provider values, for validation and
// error messages.
var ModelPriceProviders = []string{ModelPriceProviderAnthropic, ModelPriceProviderOpenAI, ModelPriceProviderOpenRouter}

// modelPriceOverlay holds the policy rows. Reads happen on every council
// turn from several goroutines; writes happen at boot and on policy reload,
// so the whole overlay is swapped under a lock rather than mutated in place.
var modelPriceOverlay struct {
	mu         sync.RWMutex
	anthropic  map[string]anthropicTokenPrice
	openai     map[string]openAITokenPrice
	openrouter map[string]openAITokenPrice
}

// RegisterModelPrices replaces the policy price overlay. A policy row wins
// over the compiled table for the same id so a corrected list price can be
// applied without a deploy; rows with an unknown provider are dropped (the
// policy validator rejects them earlier). Passing nil clears the overlay.
//
// Cache-rate conventions when a row omits them, chosen to stay conservative:
// a missing cached-input rate charges cache reads at the full input rate (no
// discount assumed), and a missing Anthropic cache-write rate uses the
// standard 1.25× input multiplier the whole first-party catalog follows.
func RegisterModelPrices(prices map[string]ModelPrice) {
	anth := make(map[string]anthropicTokenPrice)
	oa := make(map[string]openAITokenPrice)
	or := make(map[string]openAITokenPrice)
	for id, p := range prices {
		key := strings.TrimSpace(id)
		if key == "" {
			continue
		}
		cached := p.CachedInputPerMillion
		if cached <= 0 {
			cached = p.InputPerMillion
		}
		switch strings.ToLower(strings.TrimSpace(p.Provider)) {
		case ModelPriceProviderAnthropic:
			write := p.CacheWritePerMillion
			if write <= 0 {
				write = p.InputPerMillion * 1.25
			}
			anth[key] = anthropicTokenPrice{
				InputPerMillion:      p.InputPerMillion,
				CacheWritePerMillion: write,
				CacheReadPerMillion:  cached,
				OutputPerMillion:     p.OutputPerMillion,
			}
		case ModelPriceProviderOpenAI:
			oa[strings.TrimPrefix(key, "oa/")] = openAITokenPrice{
				InputPerMillion:       p.InputPerMillion,
				CachedInputPerMillion: cached,
				OutputPerMillion:      p.OutputPerMillion,
			}
		case ModelPriceProviderOpenRouter:
			or[key] = openAITokenPrice{
				InputPerMillion:       p.InputPerMillion,
				CachedInputPerMillion: cached,
				OutputPerMillion:      p.OutputPerMillion,
			}
		}
	}
	modelPriceOverlay.mu.Lock()
	modelPriceOverlay.anthropic = anth
	modelPriceOverlay.openai = oa
	modelPriceOverlay.openrouter = or
	modelPriceOverlay.mu.Unlock()
}

// lookupAnthropicPrice resolves a Messages-API model: policy overlay first,
// then the compiled table. Exact match on the trimmed id.
func lookupAnthropicPrice(model string) (anthropicTokenPrice, bool) {
	key := strings.TrimSpace(model)
	modelPriceOverlay.mu.RLock()
	p, ok := modelPriceOverlay.anthropic[key]
	modelPriceOverlay.mu.RUnlock()
	if ok {
		return p, true
	}
	p, ok = anthropicCouncilTokenPrices[key]
	return p, ok
}

// lookupOpenAIPrice resolves an OpenAI model. LiteLLM routes OpenAI models as
// oa/<model>; only that gateway prefix is removed so aliases never inherit
// another model's rate. Policy overlay first, then the compiled snapshot in
// pkg/llmpricing (shared with the HUD Codex spawn parser).
func lookupOpenAIPrice(model string) (openAITokenPrice, bool) {
	key := strings.TrimPrefix(strings.TrimSpace(model), "oa/")
	modelPriceOverlay.mu.RLock()
	p, ok := modelPriceOverlay.openai[key]
	modelPriceOverlay.mu.RUnlock()
	if ok {
		return p, true
	}
	list, ok := llmpricing.LookupOpenAI(key)
	if !ok {
		return openAITokenPrice{}, false
	}
	return openAITokenPrice{
		InputPerMillion:       list.InputPer1M,
		CachedInputPerMillion: list.CachedInputPer1M,
		OutputPerMillion:      list.OutputPer1M,
	}, true
}

// lookupOpenRouterPrice resolves direct native model IDs and explicit or/ gateway
// aliases. Exact match preserves provider isolation; no upstream identity is guessed.
func lookupOpenRouterPrice(model string) (openAITokenPrice, bool) {
	key := strings.TrimSpace(model)
	modelPriceOverlay.mu.RLock()
	p, ok := modelPriceOverlay.openrouter[key]
	modelPriceOverlay.mu.RUnlock()
	if ok {
		return p, true
	}
	p, ok = openRouterCouncilTokenPrices[key]
	return p, ok
}
