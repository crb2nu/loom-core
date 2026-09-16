package pipeline

import (
	"testing"

	"github.com/crb2nu/loom/pkg/mills/sigfp"
)

// B2: the classification path stamps the failure-shape fingerprint from the
// evidence tail, falling back to the reason. bl-honest-verdicts-s1 tightened
// the floor case: evidence too short for a shape fingerprint now stamps a
// synthetic verdict-keyed identity instead of nothing, so a CLASSIFIED
// escalation can always be joined by the miner and the shepherd.
func TestEscalationMetadataStampsFailureSignature(t *testing.T) {
	tail := "FAIL lint:parity (881ms)\nfatal error: fi_accel.h: No such file or directory"
	md := escalationMetadataFromEvidence(ClassCode, "merge stage exhausted retries", tail)
	if md.FailureSignature == "" {
		t.Fatalf("expected fingerprint from log tail")
	}
	same := escalationMetadataFromEvidence(ClassCode, "different reason", "FAIL lint:parity (12ms)\nfatal error: fi_accel.h: No such file or directory")
	if md.FailureSignature != same.FailureSignature {
		t.Fatalf("same shape must converge: %q vs %q", md.FailureSignature, same.FailureSignature)
	}
	reasonOnly := escalationMetadataFromEvidence(ClassCode, "scope gate failed: file outside slice scope internal/hud/api.go declared pkg/mills", "")
	if reasonOnly.FailureSignature == "" {
		t.Fatalf("expected reason fallback fingerprint")
	}
	short := escalationMetadataFromEvidence(ClassCode, "x", "")
	if want := sigfp.SyntheticFingerprint("code", "x\n"); short.FailureSignature != want {
		t.Fatalf("sub-floor evidence must stamp the synthetic verdict identity %q, got %q", want, short.FailureSignature)
	}
	unclassified := escalationMetadataFromEvidence("", "", "")
	if unclassified.FailureSignature != "" {
		t.Fatalf("verdict-less empty evidence must stamp nothing, got %q", unclassified.FailureSignature)
	}
}
