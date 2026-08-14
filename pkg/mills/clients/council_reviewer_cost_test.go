package clients

import (
	"context"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/council"
)

// A LiteLLM cost-map omission (usage tokens present, usage.cost absent) must
// not mark a KNOWN gateway council model unpriced: run-level unpriced spend
// is charged at the full admission reservation, which turned a $0.47 council
// run into a $15.00 charge on COUNCIL-2026-08-03-000011 and drained the
// council's daily budget in three runs. The judge already fills the omission
// from pinned prices (knownRemoteJudgeCost); reviewers must too.
func TestFlexInferCouncilReviewerFillsKnownGatewayPricingOmission(t *testing.T) {
	body := `{"model":"or/kimi-k3","choices":[{"message":{"content":"- risk: none"}}],` +
		`"usage":{"prompt_tokens":500,"completion_tokens":300}}`
	cli := bodyStubClient(t, FlexInferConfig{ProxyURL: "http://litellm.test", DisableRegistryFallbacks: true}, body)
	r := &FlexInferCouncilReviewer{Client: cli}
	brief := &council.Brief{Markdown: "brief"}

	out, err := r.Review(context.Background(), brief,
		council.ReviewerLens{Name: "frontier", Model: "or/kimi-k3", Backend: "litellm"})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if out.CostUnpriced {
		t.Fatal("known gateway model left CostUnpriced on a cost-map omission")
	}
	if out.CostUSD <= 0 {
		t.Fatalf("want conservative filled cost > 0, got %v", out.CostUSD)
	}
}

// An unknown gateway model must stay unpriced — the pinned ceilings never
// leak onto aliases, mirroring the oa/ table's no-alias-inheritance rule.
func TestFlexInferCouncilReviewerUnknownGatewayModelStaysUnpriced(t *testing.T) {
	body := `{"model":"or/new-frontier","choices":[{"message":{"content":"- risk: none"}}],` +
		`"usage":{"prompt_tokens":500,"completion_tokens":300}}`
	cli := bodyStubClient(t, FlexInferConfig{ProxyURL: "http://litellm.test", DisableRegistryFallbacks: true}, body)
	r := &FlexInferCouncilReviewer{Client: cli}

	out, err := r.Review(context.Background(), &council.Brief{Markdown: "brief"},
		council.ReviewerLens{Name: "novel", Model: "or/new-frontier", Backend: "litellm"})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if !out.CostUnpriced {
		t.Fatal("unknown gateway model must remain unpriced (no alias rate inheritance)")
	}
}
