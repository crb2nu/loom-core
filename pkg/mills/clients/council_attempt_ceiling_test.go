package clients

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/council"
)

// timeoutStubClient answers every request with a context-deadline error —
// the shape a reviewer sees when its per-lens deadline expires before the
// gateway responds (COUNCIL-2026-09-02-000053: three lenses at 90s).
func timeoutStubClient(t *testing.T, cfg FlexInferConfig) *FlexInferClient {
	t.Helper()
	if cfg.ProxyURL == "" {
		cfg.ProxyURL = "http://stub"
	}
	cli, err := NewFlexInferClient(cfg)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	}))
	return cli
}

func TestUnpricedAttemptCeilingUSD(t *testing.T) {
	prompt := strings.Repeat("x", 3000) // 1000 tokens at the 3-chars/token ceiling ratio
	cases := []struct {
		name      string
		model     string
		backend   string
		maxTokens int
		want      float64
		ok        bool
	}{
		// or/kimi-k3 ceiling: 1000 × $5/M input + 384 × $12/M output.
		{"known openrouter model", "or/kimi-k3", "litellm", 384, (1000*5.00 + 384*12.00) / 1e6, true},
		// oa/ models price through the shared llmpricing snapshot with no
		// cached share: gpt-5.6-luna $0.20/M input, $1.20/M output.
		{"known openai model", "oa/gpt-5.6-luna", "litellm", 384, (1000*0.20 + 384*1.20) / 1e6, true},
		{"unknown model keeps reservation fallback", "or/new-frontier", "litellm", 384, 0, false},
		{"local backend is free", "or/kimi-k3", "flexinfer", 384, 0, false},
		{"negative max_tokens clamps to prompt only", "or/kimi-k3", "litellm", -1, (1000 * 5.00) / 1e6, true},
	}
	for _, c := range cases {
		got, ok := unpricedAttemptCeilingUSD(c.model, c.backend, prompt, c.maxTokens)
		if ok != c.ok || math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: unpricedAttemptCeilingUSD = (%v, %v), want (%v, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
	// A ceiling is a floor for the charge: a partial known cost is never
	// reduced by it, and a priced attempt is left alone.
	if cost, priced := ceilingPricedAttempt("or/kimi-k3", "litellm", prompt, 384, 1.00, false); !priced || cost != 1.00 {
		t.Errorf("ceilingPricedAttempt kept partial cost = (%v, %v), want (1.00, true)", cost, priced)
	}
	if cost, priced := ceilingPricedAttempt("or/kimi-k3", "litellm", prompt, 384, 0.02, true); !priced || cost != 0.02 {
		t.Errorf("ceilingPricedAttempt on priced attempt = (%v, %v), want untouched (0.02, true)", cost, priced)
	}
	if cost, priced := ceilingPricedAttempt("or/new-frontier", "litellm", prompt, 384, 0, false); priced || cost != 0 {
		t.Errorf("ceilingPricedAttempt unknown model = (%v, %v), want (0, false)", cost, priced)
	}
}

// A per-lens timeout on a KNOWN gateway model must not mark the run unpriced:
// unpriced spend consumes the whole $15 admission reservation, which is what
// turned three ~$0.72 council runs into $15.00 charges. The attempt is charged
// its bounded worst case instead.
func TestFlexInferCouncilReviewerTimeoutOnKnownGatewayModelChargesCeiling(t *testing.T) {
	cli := timeoutStubClient(t, FlexInferConfig{ProxyURL: "http://litellm.test", DisableRegistryFallbacks: true})
	r := &FlexInferCouncilReviewer{Client: cli}

	out, err := r.Review(context.Background(), &council.Brief{Markdown: "brief"},
		council.ReviewerLens{Name: "frontier", Model: "or/kimi-k3", Backend: "litellm"})
	if err == nil {
		t.Fatal("expected the timeout to surface as an error")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("unexpected error shape: %v", err)
	}
	if out.CostUnpriced {
		t.Fatal("timed-out attempt on a known gateway model must be ceiling-priced, not unpriced")
	}
	if out.CostUSD <= 0 || out.CostUSD > 0.10 {
		t.Fatalf("want a small bounded ceiling charge, got %v", out.CostUSD)
	}
}

// Unknown models keep the conservative reservation fallback even on timeout —
// the pinned ceilings never leak onto aliases.
func TestFlexInferCouncilReviewerTimeoutOnUnknownGatewayModelStaysUnpriced(t *testing.T) {
	cli := timeoutStubClient(t, FlexInferConfig{ProxyURL: "http://litellm.test", DisableRegistryFallbacks: true})
	r := &FlexInferCouncilReviewer{Client: cli}

	out, err := r.Review(context.Background(), &council.Brief{Markdown: "brief"},
		council.ReviewerLens{Name: "novel", Model: "or/new-frontier", Backend: "litellm"})
	if err == nil {
		t.Fatal("expected the timeout to surface as an error")
	}
	if !out.CostUnpriced {
		t.Fatal("unknown gateway model must remain unpriced on timeout")
	}
}

// A local lens that times out costs nothing and is never unpriced.
func TestFlexInferCouncilReviewerTimeoutOnLocalBackendIsFree(t *testing.T) {
	cli := timeoutStubClient(t, FlexInferConfig{DisableRegistryFallbacks: true})
	r := &FlexInferCouncilReviewer{Client: cli}

	out, err := r.Review(context.Background(), &council.Brief{Markdown: "brief"},
		council.ReviewerLens{Name: "security", Model: "qwen38-27b-autoround-workhorse", Backend: "flexinfer"})
	if err == nil {
		t.Fatal("expected the timeout to surface as an error")
	}
	if out.CostUnpriced || out.CostUSD != 0 {
		t.Fatalf("local timeout = (cost %v, unpriced %v), want free and priced", out.CostUSD, out.CostUnpriced)
	}
}

func TestAnthropicAttemptCeilingUSD(t *testing.T) {
	// 3000 chars → 1000 tokens at cache-write $12.50/M + 4096 × $50/M output.
	got, ok := anthropicAttemptCeilingUSD("claude-fable-5", 3000, 4096)
	want := (1000*12.50 + 4096*50.00) / 1e6
	if !ok || math.Abs(got-want) > 1e-9 {
		t.Fatalf("anthropicAttemptCeilingUSD = (%v, %v), want (%v, true)", got, ok, want)
	}
	if _, ok := anthropicAttemptCeilingUSD("claude-unknown", 3000, 4096); ok {
		t.Fatal("unknown Anthropic model must keep the reservation fallback")
	}
}
