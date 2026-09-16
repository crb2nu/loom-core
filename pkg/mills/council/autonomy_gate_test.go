package council

import (
	"context"
	"reflect"
	"testing"
)

func TestNormalizeAutonomyDecision_DerivesStableReasonCodes(t *testing.T) {
	tests := []struct {
		name     string
		in       AutonomyGateDecision
		wantCode string
		want     []string
	}{
		{
			name:     "policy blocker",
			in:       AutonomyGateDecision{Blockers: []string{" policy disabled ", "policy disabled"}},
			wantCode: AutonomyReasonPolicyDisabled,
			want:     []string{"policy disabled"},
		},
		{
			name:     "capability blocker",
			in:       AutonomyGateDecision{Blockers: []string{"mcp hub red"}},
			wantCode: AutonomyReasonCapabilityRed,
			want:     []string{"mcp hub red"},
		},
		{
			name:     "empty blocked",
			in:       AutonomyGateDecision{},
			wantCode: AutonomyReasonBlocked,
			want:     []string{},
		},
		{
			name:     "explicit normalized",
			in:       AutonomyGateDecision{Code: "Manual Review.Required"},
			wantCode: "manual_review_required",
			want:     []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeAutonomyDecision(tt.in)
			if got.Allowed {
				t.Fatal("Allowed = true, want blocked")
			}
			if got.Code != tt.wantCode {
				t.Fatalf("Code = %q, want %q", got.Code, tt.wantCode)
			}
			if !reflect.DeepEqual(got.Blockers, tt.want) {
				t.Fatalf("Blockers = %#v, want %#v", got.Blockers, tt.want)
			}
		})
	}
}

func TestAutonomyGateFuncNilAllows(t *testing.T) {
	got := (AutonomyGateFunc)(nil).CheckAutonomy(context.Background())
	if !got.Allowed {
		t.Fatalf("Allowed = false, want true: %#v", got)
	}
}

func TestAutonomyTransientCapabilities(t *testing.T) {
	transient := "mcp_hub_session: dial tcp: lookup hub: i/o timeout"
	for _, blockers := range [][]string{nil, {"401 unauthorized"}, {transient, "gitlab: missing token"}, {transient, "policy disabled"}, {"mcp_hub_session: 403 forbidden: EOF"}} {
		if got := (AutonomyGateDecision{Code: AutonomyReasonCapabilityRed, Blockers: blockers}).TransientCapabilities(); len(got) != 0 {
			t.Fatalf("permanent blockers %v: %v", blockers, got)
		}
	}
	got := (AutonomyGateDecision{Blockers: []string{transient, "arbitrary hostname: EOF"}}).TransientCapabilities()
	if !reflect.DeepEqual(got, []string{"mcp_hub_session", "unknown"}) {
		t.Fatal(got)
	}
}

// TestAutonomyTransientCapabilities_HubContention pins the 2026-09-13 evidence
// (PIPE-bl-devbox-tests-checkout-shared-clone-…): the operator's agent_context
// probe timed out waiting for the hub call slot under load and the exact
// blocker text tripped a terminal capability_red one stage from the MR. A
// call-slot / deadline expiry is a busy hub, not configuration, so it must be
// a transient capability (held, then retryable) while an auth failure quoting
// the same deadline text still fails closed.
func TestAutonomyTransientCapabilities_HubContention(t *testing.T) {
	const contention = "mcp_hub_session: MCP hub agent_context unavailable after 1 consecutive failure(s): wait for call slot: context deadline exceeded"
	got := (AutonomyGateDecision{Blockers: []string{contention}}).TransientCapabilities()
	if !reflect.DeepEqual(got, []string{"mcp_hub_session"}) {
		t.Fatalf("call-slot timeout not transient: %v", got)
	}
	got = (AutonomyGateDecision{Blockers: []string{"flexinfer: probe: context deadline exceeded", contention}}).TransientCapabilities()
	if !reflect.DeepEqual(got, []string{"flexinfer", "mcp_hub_session"}) {
		t.Fatalf("deadline expiries not transient: %v", got)
	}
	for _, blockers := range [][]string{
		{"mcp_hub_session: MCP hub agent_context unavailable after 1 consecutive failure(s): 401 unauthorized: context deadline exceeded"},
		{contention, "gitlab: missing token"},
		{contention, "policy.enabled=false"},
	} {
		if got := (AutonomyGateDecision{Blockers: blockers}).TransientCapabilities(); len(got) != 0 {
			t.Fatalf("permanent blockers %v: %v", blockers, got)
		}
	}
}
