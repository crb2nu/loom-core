package clients

import (
	"context"
	"testing"
)

// TestAuditReviewerTagsItsOwnComponent: audit pool traffic must land under
// mills-audit, not the untagged mills-flexinfer bucket, so the usage
// dashboard can tell the bulk reviewers apart from the overseer triage and
// any other local-lane caller (they share the same FlexInfer models).
func TestAuditReviewerTagsItsOwnComponent(t *testing.T) {
	var seen []string
	cli := newStubClient(t, cachedUsageBody, 200)
	cli.SetTransport(componentCapturingTransport(t, cachedUsageBody, &seen))

	rev := NewFlexInferAuditReviewer(cli)
	if _, _, err := rev.Review(context.Background(), "qwen38-27b-xtx-warm-canary", "audit this diff", 0); err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(seen) != 1 || seen[0] != ComponentAudit {
		t.Fatalf("component = %v, want [%s]", seen, ComponentAudit)
	}
}
