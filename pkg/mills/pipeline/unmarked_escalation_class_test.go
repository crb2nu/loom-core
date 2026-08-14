package pipeline

import (
	"strings"
	"testing"
)

// TestClassifyUnmarkedEscalation_NeverInventsAutoRequeueEligibility is the
// safety pin for the unmarked-escalation fallback.
//
// The fallback only exists because escalation class was transported as a
// "[class=…]" marker inside a human-readable reason and recovered by
// re-parsing it — so every escalate path that formatted its reason without the
// marker persisted NO classification, bucketed as "unclassified", and was
// failed closed to a human by the auto-requeue sweep.
//
// Classifying that evidence is only safe because Classify's fallthrough is
// ClassCode and autoRequeueBaseClass admits ONLY infra/transient/
// transient_quota. If either of those ever changes, arbitrary prose in an
// escalation reason could silently make a run auto-requeue-eligible and loop.
// This test fails loudly if that invariant breaks.
func TestClassifyUnmarkedEscalation_NeverInventsAutoRequeueEligibility(t *testing.T) {
	// Prose that matches no needle in the taxonomy — the common case for the
	// gate-retry-cap, cross-repo, integrator, and adapter escalate paths.
	unrecognized := []string{
		"gate post_implement_gate failed (nonempty_diff); implement exceeded 3 attempts",
		"gate scope failed (file outside slice scope) and no RetryFrom defined",
		"cross-repo terminal state failed",
		"integration branch unavailable",
		"merge conflict in 4 files",
		"sub-run failure: child pipeline did not converge",
		"something nobody has ever written down before",
	}
	for _, reason := range unrecognized {
		t.Run(reason[:min(len(reason), 40)], func(t *testing.T) {
			cls := classifyUnmarkedEscalation(reason, "")
			if !cls.Valid() {
				t.Fatalf("evidence %q produced an invalid class %q; the fallback must always attribute non-empty evidence", reason, cls)
			}
			if autoRequeueEligibleClass(string(cls)) {
				t.Fatalf("unrecognized evidence %q classified %q, which IS auto-requeue eligible — "+
					"arbitrary prose must never mint a retryable class or escalations will loop", reason, cls)
			}
		})
	}
}

// TestClassifyUnmarkedEscalation_RecognizedInfraBecomesRequeueable is the
// other half of the contract: evidence that DOES match a known infra needle
// must classify infra so the sweep can retry it. These are the failures that
// were previously invisible AND permanently stranded.
func TestClassifyUnmarkedEscalation_RecognizedInfraBecomesRequeueable(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		tail   string
	}{
		{
			name:   "spawn driver lost across a hud restart",
			reason: "pipeline drive failed: hud spawn spawn-c725e916ef11 status=failed: agent turn driver lost across mobile-hud restart",
		},
		{
			name:   "evidence arrives only on the stage log tail",
			reason: "gate post_implement_gate failed (tests); implement exceeded 3 attempts",
			tail:   "stage=implement attempt=3 spawn=spawn-abc: agent CLI exited 124 (no stderr; stdout: command timed out)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cls := classifyUnmarkedEscalation(tc.reason, tc.tail)
			if cls != ClassInfra {
				t.Fatalf("class = %q, want %q — this evidence names a spawn-layer defect, not a code defect", cls, ClassInfra)
			}
			if !autoRequeueEligibleClass(string(cls)) {
				t.Fatalf("class %q is not auto-requeue eligible; a recoverable infra failure must be retryable", cls)
			}
		})
	}
}

// TestDeclaredClassIsAuthoritative replaces the former marker-path test. The
// class is now an ARGUMENT from the escalating call site, not a marker parsed
// back out of the reason, so what must stay authoritative is what the caller
// declared — even when the surrounding prose would classify differently.
func TestDeclaredClassIsAuthoritative(t *testing.T) {
	reason := "stage tests errored after 3 total attempts (cap 5) [class=config]: gitlab: status 401"
	md := escalationMetadataFromEvidence(ClassConfig, reason, "")
	if md.EscalationClass != "config" {
		t.Fatalf("EscalationClass = %q, want config (the declared class must win)", md.EscalationClass)
	}
	if md.Retryable == nil || *md.Retryable {
		t.Fatalf("config must remain non-retryable; Retryable = %v", md.Retryable)
	}
}

// TestPolicyBlockEscalationsCarryConfigClass pins that the operator-policy
// block paths (autonomy circuit breaker, degraded mode, fail-closed health
// gates) mark themselves config rather than letting the prose fallback read
// them as a code defect in the diff. They are terminal human signals.
func TestPolicyBlockEscalationsCarryConfigClass(t *testing.T) {
	cases := []struct {
		name   string
		reason string
	}{
		{"autonomy circuit breaker", "autonomy circuit breaker blocked before stage implement [class=config] [reason_code=blocked]"},
		{"degraded mode", "degraded-mode policy blocked before stage research [class=config] [reason_code=embedder_unavailable]"},
		{"fail-closed health gates", "infrastructure health gates blocked pipeline [class=config]: health gates unavailable (fail-closed)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// These paths pass ClassConfig explicitly (autonomy_gate.go,
			// degraded_policy.go, preflight.go); the marker in the prose is
			// operator-facing text only.
			md := escalationMetadataFromEvidence(ClassConfig, tc.reason, "")
			if md.EscalationClass != string(ClassConfig) {
				t.Fatalf("EscalationClass = %q, want %q", md.EscalationClass, ClassConfig)
			}
			if md.Retryable == nil || *md.Retryable {
				t.Fatalf("a policy block must be terminal, not retryable; Retryable = %v", md.Retryable)
			}
			if autoRequeueEligibleClass(md.EscalationClass) {
				t.Fatalf("policy block classified %q, which is auto-requeue eligible — "+
					"an operator decision must never be retried around", md.EscalationClass)
			}
		})
	}
}

// TestMeasuredUnhealthyHealthGateIsInfra pins the deliberate split in
// runPreflight: a gate that could not be evaluated (fail-closed) is config (a
// human must find out why), while measured-unhealthy infrastructure is infra
// so bounded auto-requeue can retry it once the dependency recovers.
func TestMeasuredUnhealthyHealthGateIsInfra(t *testing.T) {
	reason := "infrastructure health gates blocked pipeline [class=infra]: qdrant unreachable"
	md := escalationMetadataFromEvidence(ClassInfra, reason, "")
	if md.EscalationClass != string(ClassInfra) {
		t.Fatalf("EscalationClass = %q, want %q", md.EscalationClass, ClassInfra)
	}
	if !autoRequeueEligibleClass(md.EscalationClass) {
		t.Fatal("measured-unhealthy infrastructure must be auto-requeue eligible")
	}
}

// autoRequeueEligibleClass mirrors reconciler_auto_requeue.autoRequeueBaseClass
// (which lives in package mills and cannot be imported here without a cycle).
// Kept deliberately literal so a divergence in the real sweep shows up as a
// test that no longer describes production.
func autoRequeueEligibleClass(class string) bool {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "infra", "transient", "transient_quota":
		return true
	default:
		return false
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
