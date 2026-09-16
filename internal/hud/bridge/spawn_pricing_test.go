package bridge

import (
	"math"
	"testing"

	"github.com/crb2nu/loom/pkg/llmpricing"
)

const floatTolerance = 1e-9

func floatsApproxEqual(a, b float64) bool {
	return math.Abs(a-b) < floatTolerance
}

func TestEstimateCodexCost_DefaultModel(t *testing.T) {
	// gpt-5.5: input $5.00/M, cached $0.50/M, output $30.00/M.
	// 1000 fresh + 500 cached + 200 output =
	//   (1000 * 5.00 + 500 * 0.50 + 200 * 30.00) / 1_000_000
	//   = (5000 + 250 + 6000) / 1_000_000
	//   = 0.01125
	got, known := EstimateCodexCost("gpt-5.5", 1000, 500, 200)
	if !known {
		t.Fatalf("EstimateCodexCost(gpt-5.5) known=false, want true")
	}
	if want := 0.01125; !floatsApproxEqual(got, want) {
		t.Errorf("EstimateCodexCost(gpt-5.5, 1000, 500, 200) = %v, want %v", got, want)
	}
}

// TestEstimateCodexCost_ProductionStageModels is the regression for the $0
// stage records: the models Mills pins in pipeline.stage_models must be
// priced, and priced at their own (not the default's) rate.
func TestEstimateCodexCost_ProductionStageModels(t *testing.T) {
	cases := []struct {
		model string
		want  float64
	}{
		// 300 fresh + 200 cached + 150 output
		{"gpt-5.6-terra", (300*2.00 + 200*0.20 + 150*12.00) / 1_000_000},
		{"gpt-5.6-sol", (300*4.00 + 200*0.40 + 150*20.00) / 1_000_000},
		{"gpt-5.6-luna", (300*0.20 + 200*0.02 + 150*1.20) / 1_000_000},
		{"gpt-5.4", (300*2.50 + 200*0.25 + 150*15.00) / 1_000_000},
		{"gpt-6-astra", (300*10.00 + 200*1.00 + 150*50.00) / 1_000_000},
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			got, known := EstimateCodexCost(c.model, 300, 200, 150)
			if !known {
				t.Fatalf("known=false: %q is missing from the price snapshot", c.model)
			}
			if !floatsApproxEqual(got, c.want) {
				t.Errorf("EstimateCodexCost(%q, 300, 200, 150) = %v, want %v", c.model, got, c.want)
			}
		})
	}
}

func TestEstimateCodexCost_GPT5Mini(t *testing.T) {
	// gpt-5-mini: input $0.25/M, cached $0.025/M, output $2.00/M.
	// 4000 fresh + 1000 cached + 800 output = 0.002625
	got, known := EstimateCodexCost("gpt-5-mini", 4000, 1000, 800)
	if !known {
		t.Fatalf("known=false for gpt-5-mini")
	}
	if want := 0.002625; !floatsApproxEqual(got, want) {
		t.Errorf("EstimateCodexCost(gpt-5-mini, 4000, 1000, 800) = %v, want %v", got, want)
	}
}

// TestEstimateCodexCost_UnknownModelBillsAtDefault: an id newer than the
// snapshot must NOT be recorded as free. It is billed at DefaultCodexModel's
// rate and reported known=false so the parser can log it.
func TestEstimateCodexCost_UnknownModelBillsAtDefault(t *testing.T) {
	got, known := EstimateCodexCost("gpt-7-nova", 1000, 500, 200)
	if known {
		t.Fatalf("EstimateCodexCost(unknown) known=true, want false")
	}
	want, _ := EstimateCodexCost(DefaultCodexModel, 1000, 500, 200)
	if want <= 0 {
		t.Fatalf("default model priced at %v, want > 0", want)
	}
	if !floatsApproxEqual(got, want) {
		t.Errorf("EstimateCodexCost(unknown, ...) = %v, want default-rate %v", got, want)
	}
}

// TestEstimateCodexCost_UnknownModelIsConservative: the fallback rate is at
// least as expensive as every non-pro model Codex can run, so an unknown
// model over-estimates rather than under-estimates against a budget cap.
func TestEstimateCodexCost_UnknownModelIsConservative(t *testing.T) {
	fallback, _ := EstimateCodexCost("gpt-7-nova", 1000, 1000, 1000)
	for _, m := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex"} {
		known, ok := EstimateCodexCost(m, 1000, 1000, 1000)
		if !ok {
			t.Fatalf("%q missing from snapshot", m)
		}
		if known > fallback+floatTolerance {
			t.Errorf("unknown-model fallback %v is cheaper than %q at %v; fallback must over-estimate", fallback, m, known)
		}
	}
}

func TestEstimateCodexCost_ZeroTokensReturnsZero(t *testing.T) {
	if got, _ := EstimateCodexCost("gpt-5.5", 0, 0, 0); got != 0 {
		t.Errorf("EstimateCodexCost(gpt-5.5, 0, 0, 0) = %v, want 0", got)
	}
}

func TestEstimateCodexCost_OnlyOutputTokens(t *testing.T) {
	// gpt-5.5 output $30.00/M => 1000 output tokens = $0.03.
	got, _ := EstimateCodexCost("gpt-5.5", 0, 0, 1000)
	if want := 0.03; !floatsApproxEqual(got, want) {
		t.Errorf("EstimateCodexCost(gpt-5.5, 0, 0, 1000) = %v, want %v", got, want)
	}
}

func TestLookupCodexPrice_KnownModel(t *testing.T) {
	p, ok := LookupCodexPrice("gpt-5.6-terra")
	if !ok {
		t.Fatalf("LookupCodexPrice(gpt-5.6-terra) ok=false, want true")
	}
	if p.InputPer1M != 2.00 || p.CachedInputPer1M != 0.20 || p.OutputPer1M != 12.00 {
		t.Errorf("LookupCodexPrice(gpt-5.6-terra) = %+v, want input=2.00 cached=0.20 output=12.00", p)
	}
}

func TestLookupCodexPrice_UnknownFallsBackToDefault(t *testing.T) {
	p, ok := LookupCodexPrice("not-a-real-model")
	if !ok {
		t.Fatalf("LookupCodexPrice(unknown) ok=false, want true (should fall back to %s)", DefaultCodexModel)
	}
	defaultPrice, _ := LookupCodexPrice(DefaultCodexModel)
	if p != defaultPrice {
		t.Errorf("LookupCodexPrice(unknown) = %+v, want default %+v", p, defaultPrice)
	}
}

func TestLookupCodexPrice_DefaultModelExists(t *testing.T) {
	// The default must be in the shared snapshot: EstimateCodexCost's
	// unknown-model fallback and the parser's no-metadata path both bill at
	// this rate, and a missing default would silently return to $0.
	if _, ok := llmpricing.LookupOpenAI(DefaultCodexModel); !ok {
		t.Errorf("DefaultCodexModel %q is not in the llmpricing OpenAI snapshot", DefaultCodexModel)
	}
}

func TestEstimateCodexCost_AllKnownModelsNonZeroForNonZeroInput(t *testing.T) {
	for _, model := range llmpricing.OpenAIModels() {
		got, known := EstimateCodexCost(model, 100, 100, 100)
		if !known || got <= 0 {
			t.Errorf("EstimateCodexCost(%q, 100, 100, 100) = (%v, %v), want (> 0, true)", model, got, known)
		}
	}
}
