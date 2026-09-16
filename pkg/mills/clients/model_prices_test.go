package clients

import (
	"math"
	"testing"
)

func TestModelPriceOverlay_PolicyRowsPriceUnknownModels(t *testing.T) {
	t.Cleanup(func() { RegisterModelPrices(nil) })
	RegisterModelPrices(map[string]ModelPrice{
		// The GPT-6 tier: unknown to the compiled table, priced from policy
		// the day it ships. cached_input omitted → charged at full input.
		"gpt-6-astra": {Provider: "openai", InputPerMillion: 8, OutputPerMillion: 40},
		// Anthropic row with the cache-write rate omitted → 1.25× input.
		"claude-fable-5-2": {Provider: "anthropic", InputPerMillion: 10, CachedInputPerMillion: 1, OutputPerMillion: 50},
		// OpenRouter ceiling keeps its or/ prefix.
		"or/some-new-lens": {Provider: "openrouter", InputPerMillion: 3, OutputPerMillion: 9},
	})

	oa, ok := lookupOpenAIPrice("oa/gpt-6-astra")
	if !ok || oa.InputPerMillion != 8 || oa.CachedInputPerMillion != 8 || oa.OutputPerMillion != 40 {
		t.Fatalf("openai overlay = %+v, %v", oa, ok)
	}
	anth, ok := lookupAnthropicPrice("claude-fable-5-2")
	if !ok || anth.CacheWritePerMillion != 12.5 || anth.CacheReadPerMillion != 1 {
		t.Fatalf("anthropic overlay = %+v, %v", anth, ok)
	}
	or, ok := lookupOpenRouterPrice("or/some-new-lens")
	if !ok || or.InputPerMillion != 3 {
		t.Fatalf("openrouter overlay = %+v, %v", or, ok)
	}
	// The overlay feeds the same cost paths the compiled table does.
	if got, ok := openAICouncilTokenCostUSD("oa/gpt-6-astra", 1_000, 0, 100); !ok || math.Abs(got-(1000*8.0+100*40.0)/1_000_000) > 1e-12 {
		t.Fatalf("overlay cost = %.8f, %v", got, ok)
	}
}

func TestModelPriceOverlay_PolicyRowWinsOverCompiledTable(t *testing.T) {
	t.Cleanup(func() { RegisterModelPrices(nil) })
	static, _ := lookupOpenAIPrice("gpt-5.6-sol")
	RegisterModelPrices(map[string]ModelPrice{
		"gpt-5.6-sol": {Provider: "openai", InputPerMillion: static.InputPerMillion * 2, OutputPerMillion: static.OutputPerMillion},
	})
	got, ok := lookupOpenAIPrice("gpt-5.6-sol")
	if !ok || got.InputPerMillion != static.InputPerMillion*2 {
		t.Fatalf("policy row did not win: %+v", got)
	}
	RegisterModelPrices(nil)
	got, _ = lookupOpenAIPrice("gpt-5.6-sol")
	if got.InputPerMillion != static.InputPerMillion {
		t.Fatalf("clearing the overlay did not restore the compiled rate: %+v", got)
	}
}

func TestModelPriceOverlay_UnknownProviderAndBlankIdIgnored(t *testing.T) {
	t.Cleanup(func() { RegisterModelPrices(nil) })
	RegisterModelPrices(map[string]ModelPrice{
		"gpt-7-nova": {Provider: "azure", InputPerMillion: 1, OutputPerMillion: 1},
		"   ":        {Provider: "openai", InputPerMillion: 1, OutputPerMillion: 1},
	})
	if _, ok := lookupOpenAIPrice("gpt-7-nova"); ok {
		t.Fatal("unknown provider row must not price anything")
	}
}

func TestAnthropicPrices_Fable51IsPriced(t *testing.T) {
	// claude-fable-5-1 must never fall into the unpriced whole-reservation
	// path; the row is provisional (Fable 5 rates) until the list price is
	// confirmed, and a policy model_prices row overrides it either way.
	p, ok := lookupAnthropicPrice("claude-fable-5-1")
	if !ok || p.OutputPerMillion <= 0 {
		t.Fatalf("claude-fable-5-1 unpriced: %+v, %v", p, ok)
	}
	if _, ok := anthropicAttemptCeilingUSD("claude-fable-5-1", 3000, 4096); !ok {
		t.Fatal("attempt ceiling must be computable for claude-fable-5-1")
	}
}
