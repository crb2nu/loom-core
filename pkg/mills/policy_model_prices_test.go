package mills

import (
	"strings"
	"testing"
)

// budgets.model_prices is the day-one path for pricing a model the compiled
// tables do not know (a new frontier tier) without an image deploy. These
// pin the schema, the validator, and the accessor copy semantics.
func TestPolicyModelPrices_RoundTripAndAccessor(t *testing.T) {
	src := `
version: 2
budgets:
  council:  { max_usd_per_run: 15, max_usd_per_day: 50 }
  pipeline: { max_usd_per_run: 5,  max_usd_per_day: 75 }
  model_prices:
    gpt-6-astra:
      provider: openai
      input_per_million: 8
      cached_input_per_million: 0.8
      output_per_million: 40
    claude-fable-5-1:
      provider: anthropic
      input_per_million: 10
      cached_input_per_million: 1
      cache_write_per_million: 12.5
      output_per_million: 50
    or/new-lens:
      provider: openrouter
      input_per_million: 3
      output_per_million: 9
`
	p, err := ParsePolicy([]byte(src))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	rows := p.ModelPriceOverrides()
	if len(rows) != 3 {
		t.Fatalf("ModelPriceOverrides = %d rows, want 3", len(rows))
	}
	astra := rows["gpt-6-astra"]
	if astra.Provider != "openai" || astra.InputPerMillion != 8 || astra.CachedInputPerMillion != 0.8 || astra.OutputPerMillion != 40 {
		t.Fatalf("gpt-6-astra row = %+v", astra)
	}
	if rows["claude-fable-5-1"].CacheWritePerMillion != 12.5 {
		t.Fatalf("fable row = %+v", rows["claude-fable-5-1"])
	}
	// The accessor hands out a copy: mutating it must not touch policy.
	rows["gpt-6-astra"] = ModelPriceOverride{}
	if p.Budgets.ModelPrices["gpt-6-astra"].InputPerMillion != 8 {
		t.Fatal("ModelPriceOverrides leaked the policy map")
	}
	var nilPolicy *Policy
	if nilPolicy.ModelPriceOverrides() != nil {
		t.Fatal("nil policy must return nil overrides")
	}
}

func TestPolicyModelPrices_Validation(t *testing.T) {
	base := `
version: 2
budgets:
  council:  { max_usd_per_run: 15, max_usd_per_day: 50 }
  pipeline: { max_usd_per_run: 5,  max_usd_per_day: 75 }
  model_prices:
`
	cases := []struct {
		name string
		rows string
		want string
	}{
		{"unknown provider", "    gpt-6-astra: { provider: azure, input_per_million: 1, output_per_million: 1 }\n", "provider must be one of"},
		{"missing output", "    gpt-6-astra: { provider: openai, input_per_million: 1 }\n", "must be > 0"},
		{"negative cache", "    gpt-6-astra: { provider: openai, input_per_million: 1, output_per_million: 1, cached_input_per_million: -1 }\n", "cache rates must be >= 0"},
		{"bad id", "    \"gpt 6\": { provider: openai, input_per_million: 1, output_per_million: 1 }\n", "not a valid model id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePolicy([]byte(base + tc.rows))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParsePolicy error = %v, want containing %q", err, tc.want)
			}
		})
	}
	// An absent block is not an error and yields no overrides.
	p, err := ParsePolicy([]byte(strings.TrimSuffix(base, "  model_prices:\n")))
	if err != nil {
		t.Fatalf("ParsePolicy without model_prices: %v", err)
	}
	if p.ModelPriceOverrides() != nil {
		t.Fatal("expected nil overrides when the block is absent")
	}
}
