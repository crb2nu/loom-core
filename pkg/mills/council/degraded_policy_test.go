package council

import (
	"testing"

	"github.com/crb2nu/loom/pkg/telemetry"
)

func TestEvaluateDegradedModePolicy_DocumentFallbackVectorAllowsDegraded(t *testing.T) {
	got := EvaluateDegradedModePolicy(DegradedModePolicyInput{
		Signals: []DegradedModeSignal{{
			Path:           telemetry.EmbeddingPathDocuments,
			Outcome:        telemetry.EmbeddingOutcomeFallbackSuccess,
			Reason:         telemetry.EmbeddingReasonProviderOverload,
			Provider:       "morph",
			Model:          "morph-embedding-v3",
			FallbackVector: true,
		}},
	})
	if !got.Allowed || got.Mode != DegradedPolicyModeDegraded {
		t.Fatalf("decision = %+v, want allowed degraded", got)
	}
	if got.Code != DegradedPolicyCodeEmbedderFallbackVector {
		t.Fatalf("code = %q, want %q", got.Code, DegradedPolicyCodeEmbedderFallbackVector)
	}
	if !got.FallbackUsed {
		t.Fatal("FallbackUsed = false, want true")
	}
}

func TestEvaluateDegradedModePolicy_QueryDegradationBlocks(t *testing.T) {
	got := EvaluateDegradedModePolicy(DegradedModePolicyInput{
		Signals: []DegradedModeSignal{{
			Path:    telemetry.EmbeddingPathQuery,
			Outcome: telemetry.EmbeddingOutcomeDegraded,
			Reason:  telemetry.EmbeddingReasonCircuitOpen,
		}},
	})
	if got.Allowed || got.Mode != DegradedPolicyModeBlocked {
		t.Fatalf("decision = %+v, want blocked", got)
	}
	if got.Code != DegradedPolicyCodeEmbedderQueryDegraded {
		t.Fatalf("code = %q, want %q", got.Code, DegradedPolicyCodeEmbedderQueryDegraded)
	}
	if len(got.Blockers) != 1 || got.Blockers[0] != "embedding query degraded to keyword search" {
		t.Fatalf("blockers = %+v", got.Blockers)
	}
}

func TestEvaluateDegradedModePolicy_OpenCircuitWithoutFallbackBlocks(t *testing.T) {
	got := EvaluateDegradedModePolicy(DegradedModePolicyInput{
		Signals: []DegradedModeSignal{{
			Path:    telemetry.EmbeddingPathDocuments,
			Outcome: telemetry.EmbeddingOutcomeShortCircuit,
			Reason:  telemetry.EmbeddingReasonCircuitOpen,
		}},
	})
	if got.Allowed {
		t.Fatalf("decision = %+v, want blocked", got)
	}
	if got.Code != DegradedPolicyCodeEmbedderUnavailable {
		t.Fatalf("code = %q, want %q", got.Code, DegradedPolicyCodeEmbedderUnavailable)
	}
}

func TestEvaluateDegradedModePolicy_AutonomyBreakerWins(t *testing.T) {
	autonomy := AutonomyGateDecision{
		Allowed:  false,
		Code:     AutonomyReasonPolicyDisabled,
		Blockers: []string{"policy.enabled=false"},
	}
	got := EvaluateDegradedModePolicy(DegradedModePolicyInput{
		Autonomy: &autonomy,
		Signals: []DegradedModeSignal{{
			Path:           telemetry.EmbeddingPathDocuments,
			Outcome:        telemetry.EmbeddingOutcomeFallbackSuccess,
			Reason:         telemetry.EmbeddingReasonProviderOverload,
			FallbackVector: true,
		}},
	})
	if got.Allowed {
		t.Fatalf("decision = %+v, want blocked", got)
	}
	if got.Code != AutonomyReasonPolicyDisabled {
		t.Fatalf("code = %q, want autonomy code", got.Code)
	}
	if len(got.Blockers) != 1 || got.Blockers[0] != "policy.enabled=false" {
		t.Fatalf("blockers = %+v", got.Blockers)
	}
}
