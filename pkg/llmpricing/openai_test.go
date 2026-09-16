package llmpricing

import (
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

// TestOpenAIPrices_PinnedProductionModels guards the rows production actually
// bills against: the Mills stage_models pins (terra/sol), the Codex
// ChatGPT-safe default (gpt-5.5), the council editor (gpt-5.4), and the
// current Codex default for eligible accounts (gpt-6-astra). Values are the
// Standard tier read off OpenAISource on OpenAISnapshotDate.
func TestOpenAIPrices_PinnedProductionModels(t *testing.T) {
	cases := []struct {
		model               string
		in, cached, out     float64
		wantThousandFresh   float64 // cost of 1000 uncached + 0 + 0
		wantThousandCached  float64 // cost of 0 + 1000 cached + 0
		wantThousandOutputs float64 // cost of 0 + 0 + 1000 output
	}{
		{"gpt-5.6-sol", 4.00, 0.40, 20.00, 0.004, 0.0004, 0.02},
		{"gpt-5.6-terra", 2.00, 0.20, 12.00, 0.002, 0.0002, 0.012},
		{"gpt-5.6-luna", 0.20, 0.02, 1.20, 0.0002, 0.00002, 0.0012},
		{"gpt-5.5", 5.00, 0.50, 30.00, 0.005, 0.0005, 0.03},
		{"gpt-5.4", 2.50, 0.25, 15.00, 0.0025, 0.00025, 0.015},
		{"gpt-6-astra", 10.00, 1.00, 50.00, 0.01, 0.001, 0.05},
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			p, ok := LookupOpenAI(c.model)
			if !ok {
				t.Fatalf("LookupOpenAI(%q) ok=false; production model missing from snapshot", c.model)
			}
			if p.InputPer1M != c.in || p.CachedInputPer1M != c.cached || p.OutputPer1M != c.out {
				t.Fatalf("LookupOpenAI(%q) = %+v, want in=%v cached=%v out=%v", c.model, p, c.in, c.cached, c.out)
			}
			if got := p.CostUSD(1000, 0, 0); !approx(got, c.wantThousandFresh) {
				t.Errorf("1000 fresh = %v, want %v", got, c.wantThousandFresh)
			}
			if got := p.CostUSD(0, 1000, 0); !approx(got, c.wantThousandCached) {
				t.Errorf("1000 cached = %v, want %v", got, c.wantThousandCached)
			}
			if got := p.CostUSD(0, 0, 1000); !approx(got, c.wantThousandOutputs) {
				t.Errorf("1000 output = %v, want %v", got, c.wantThousandOutputs)
			}
		})
	}
}

func TestLookupOpenAI_ExactMatchOnly(t *testing.T) {
	if _, ok := LookupOpenAI("oa/gpt-5.6-terra"); ok {
		t.Error("gateway prefix must not resolve: prefix stripping is the caller's job")
	}
	if _, ok := LookupOpenAI("GPT-5.6-TERRA"); ok {
		t.Error("lookup must be case-sensitive; ids are exact")
	}
	if _, ok := LookupOpenAI("gpt-7-nova"); ok {
		t.Error("unknown model resolved")
	}
	if _, ok := LookupOpenAI("gpt-5-codex"); ok {
		t.Error("gpt-5-codex is delisted and must not carry a stale rate")
	}
	if _, ok := LookupOpenAI("  gpt-5.5  "); !ok {
		t.Error("surrounding whitespace should be ignored")
	}
}

func TestOpenAICostUSD_UnknownModelIsZeroAndNotOK(t *testing.T) {
	got, ok := OpenAICostUSD("gpt-7-nova", 1000, 1000, 1000)
	if ok || got != 0 {
		t.Fatalf("OpenAICostUSD(unknown) = (%v, %v), want (0, false)", got, ok)
	}
}

func TestOpenAIPrice_CostUSD_Arithmetic(t *testing.T) {
	p, _ := LookupOpenAI("gpt-5.6-terra")
	// 300 fresh @ $2 + 200 cached @ $0.20 + 150 out @ $12, per 1M.
	want := (300*2.00 + 200*0.20 + 150*12.00) / 1_000_000
	if got := p.CostUSD(300, 200, 150); !approx(got, want) {
		t.Errorf("CostUSD(300,200,150) = %v, want %v", got, want)
	}
	if got := p.CostUSD(0, 0, 0); got != 0 {
		t.Errorf("zero usage = %v, want 0", got)
	}
	if got := p.CostUSD(-5, -5, -5); got != 0 {
		t.Errorf("negative usage should clamp to 0, got %v", got)
	}
}

func TestOpenAIPrice_CostUSD_LongContext(t *testing.T) {
	p, _ := LookupOpenAI("gpt-5.5")
	// Exactly at the threshold: standard rate.
	at := p.CostUSD(OpenAILongContextThreshold, 0, 1000)
	wantAt := (float64(OpenAILongContextThreshold)*5.00 + 1000*30.00) / 1_000_000
	if !approx(at, wantAt) {
		t.Errorf("at threshold = %v, want %v", at, wantAt)
	}
	// One token over, split across cached + uncached: input 2x, output 1.5x
	// (gpt-5.5 long-context list price is $10 / $1 / $45).
	over := p.CostUSD(OpenAILongContextThreshold-1000, 1001, 1000)
	wantOver := (float64(OpenAILongContextThreshold-1000)*10.00 + 1001*1.00 + 1000*45.00) / 1_000_000
	if !approx(over, wantOver) {
		t.Errorf("over threshold = %v, want %v", over, wantOver)
	}
}

func TestOpenAIModels_EveryRowIsPositiveAndSorted(t *testing.T) {
	models := OpenAIModels()
	if len(models) == 0 {
		t.Fatal("empty snapshot")
	}
	for i, m := range models {
		if i > 0 && models[i-1] >= m {
			t.Errorf("OpenAIModels not sorted at %q", m)
		}
		p, _ := LookupOpenAI(m)
		if p.InputPer1M <= 0 || p.CachedInputPer1M <= 0 || p.OutputPer1M <= 0 {
			t.Errorf("%q has a non-positive rate: %+v", m, p)
		}
		if p.CachedInputPer1M > p.InputPer1M {
			t.Errorf("%q cached rate %v exceeds input rate %v", m, p.CachedInputPer1M, p.InputPer1M)
		}
	}
}
