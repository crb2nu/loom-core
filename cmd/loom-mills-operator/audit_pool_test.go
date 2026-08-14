package main

import (
	"testing"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/audit"
)

// TestAuditPoolPolicy_FromConfigmap verifies the dispatcher pool is built
// from policy.yaml's audit.pool_default / audit.pool_escalation when set,
// so model drift is a configmap edit (not a recompile). Entries missing a
// backend or model are dropped; the Driver field is policy-only metadata.
func TestAuditPoolPolicy_FromConfigmap(t *testing.T) {
	cfg := mills.AuditPolicy{
		PoolDefault: []mills.AuditPool{
			{Backend: "flexinfer", Model: "gemma4-26b-a4b-gptq"},
			{Backend: "flexinfer", Model: "qwen36-35b-mtp-uncensored-5930k"},
			{Backend: "flexinfer", Model: ""}, // dropped: no model
			{Backend: "", Model: "orphan"},    // dropped: no backend
		},
		PoolEscalation: []mills.AuditPool{
			{Backend: "spawn", Model: "claude-opus-4-7", Driver: "claude-code"},
		},
	}

	got := auditPoolPolicy(cfg, nil)

	wantBulk := []string{"gemma4-26b-a4b-gptq", "qwen36-35b-mtp-uncensored-5930k"}
	if len(got.Bulk) != len(wantBulk) {
		t.Fatalf("bulk len = %d, want %d (%+v)", len(got.Bulk), len(wantBulk), got.Bulk)
	}
	for i, m := range got.Bulk {
		if m.Backend != "flexinfer" || m.Model != wantBulk[i] {
			t.Errorf("bulk[%d] = %+v, want flexinfer/%s", i, m, wantBulk[i])
		}
	}
	if len(got.Escalation) != 1 || got.Escalation[0].Backend != "spawn" || got.Escalation[0].Model != "claude-opus-4-7" {
		t.Errorf("escalation = %+v, want one spawn/claude-opus-4-7 member", got.Escalation)
	}
}

// TestAuditPoolPolicy_FallbackWhenEmpty verifies that an empty
// pool_default falls back to the two Ready FlexInfer chat models and
// disables escalation (no flexinfer frontier exists; the spawn frontier
// isn't registered, so escalating would 404 or silently skip).
func TestAuditPoolPolicy_FallbackWhenEmpty(t *testing.T) {
	got := auditPoolPolicy(mills.AuditPolicy{}, nil)

	want := []audit.PoolMember{
		{Backend: "flexinfer", Model: "gemma4-26b-a4b-gptq"},
		{Backend: "flexinfer", Model: "qwen36-35b-mtp-uncensored-5930k"},
	}
	if len(got.Bulk) != len(want) {
		t.Fatalf("fallback bulk len = %d, want %d (%+v)", len(got.Bulk), len(want), got.Bulk)
	}
	for i, m := range got.Bulk {
		if m != want[i] {
			t.Errorf("fallback bulk[%d] = %+v, want %+v", i, m, want[i])
		}
	}
	if len(got.Escalation) != 0 {
		t.Errorf("fallback escalation = %+v, want empty", got.Escalation)
	}
}

// TestAuditPoolMembers_DropsInvalid guards the conversion helper against
// half-specified configmap rows so a typo can't register a member that
// the dispatcher would route to a nonexistent backend/model.
func TestAuditPoolMembers_DropsInvalid(t *testing.T) {
	in := []mills.AuditPool{
		{Backend: "flexinfer", Model: "gemma4-26b-a4b-gptq"},
		{Backend: "  ", Model: "ws-only"},
		{Backend: "flexinfer", Model: "   "},
		{Backend: "flexinfer", Model: "qwen36-35b-mtp-uncensored-5930k", Driver: "ignored"},
	}
	got := auditPoolMembers(in)
	if len(got) != 2 {
		t.Fatalf("got %d members, want 2 (%+v)", len(got), got)
	}
	if got[0].Model != "gemma4-26b-a4b-gptq" || got[1].Model != "qwen36-35b-mtp-uncensored-5930k" {
		t.Errorf("unexpected members: %+v", got)
	}
}
