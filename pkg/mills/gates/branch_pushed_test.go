package gates

import (
	"context"
	"strings"
	"testing"
)

func TestBranchPushedFailsOnMissingRemoteRef(t *testing.T) {
	in := StageInput{
		GitCaptureStatus: "fetch_failed",
		GitCaptureReason: "branch ref feat/example fetch failed (exit 128): fatal: couldn't find remote ref refs/heads/feat/example",
	}
	out, err := (&BranchPushed{}).Evaluate(context.Background(), in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if out.Pass {
		t.Fatalf("outcome = %+v, want failure", out)
	}
	reason := strings.Join(out.Reasons, " ")
	for _, want := range []string{"configured git identity", "git push -u origin HEAD"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q does not contain %q", reason, want)
		}
	}
}

func TestBranchPushedPassesWithoutDefinitiveMissingRef(t *testing.T) {
	tests := []StageInput{
		{GitCaptureStatus: "captured"},
		{GitCaptureStatus: "captured_empty"},
		{GitCaptureStatus: "skipped_no_capture_context"},
		{GitCaptureStatus: "fetch_failed", GitCaptureReason: "dial tcp: connection timed out"},
	}
	for _, in := range tests {
		out, err := (&BranchPushed{}).Evaluate(context.Background(), in)
		if err != nil {
			t.Fatalf("Evaluate(%q): %v", in.GitCaptureStatus, err)
		}
		if !out.Pass {
			t.Errorf("Evaluate(%q, %q) = %+v, want pass", in.GitCaptureStatus, in.GitCaptureReason, out)
		}
	}
}

func TestBranchPushedDigestUsesSemanticCaptureSignals(t *testing.T) {
	missingA := StageInput{GitCaptureStatus: "fetch_failed", GitCaptureReason: "fatal: couldn't find remote ref refs/heads/feat/a"}
	missingB := StageInput{GitCaptureStatus: "fetch_failed", GitCaptureReason: "prefix: fatal: couldn't find remote ref refs/heads/feat/b"}
	if got, want := inputDigestForGate("branch_pushed", missingA), inputDigestForGate("branch_pushed", missingB); got != want {
		t.Fatalf("missing-ref digests differ: %q != %q", got, want)
	}
	other := StageInput{GitCaptureStatus: "fetch_failed", GitCaptureReason: "connection timed out"}
	if inputDigestForGate("branch_pushed", missingA) == inputDigestForGate("branch_pushed", other) {
		t.Fatal("missing-ref and inconclusive capture digests must differ")
	}
}
