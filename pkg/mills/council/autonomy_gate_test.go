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
