package mills

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var normalizedSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// TestPolicyChecksum_NormalizedHex: the stamped format must be 64 lowercase
// hex characters so it compares byte-for-byte against the deployment's
// loom.flexinfer.ai/policy-checksum annotation.
func TestPolicyChecksum_NormalizedHex(t *testing.T) {
	sum := PolicyChecksum([]byte(fixtureV1))
	if !normalizedSHA256.MatchString(sum) {
		t.Fatalf("checksum %q is not 64 lowercase hex chars", sum)
	}
	if other := PolicyChecksum([]byte(fixtureV1 + "\n")); other == sum {
		t.Fatal("checksum ignored a trailing byte")
	}
	if ProvenanceDigest([]byte(fixtureV1)) != sum {
		t.Fatal("PolicyChecksum and ProvenanceDigest disagree on the same bytes")
	}
}

// TestPolicyManager_ChecksumTracksLoadedBytes: the reported checksum must
// digest the bytes the ACTIVE policy was parsed from. A checksum that advances
// on a rejected reload — or lags a successful one — would attribute runs to a
// policy revision that never ran them.
func TestPolicyManager_ChecksumTracksLoadedBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(path, []byte(fixtureV1), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mgr, err := NewPolicyManager(context.Background(), path, PolicyManagerOptions{SkipWatch: true})
	if err != nil {
		t.Fatalf("new mgr: %v", err)
	}
	defer mgr.Close()
	if got, want := mgr.CurrentChecksum(), PolicyChecksum([]byte(fixtureV1)); got != want {
		t.Fatalf("initial checksum = %q, want %q", got, want)
	}

	updated := fixtureV1 + "\n# operator note\n"
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatalf("write update: %v", err)
	}
	if err := mgr.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, want := mgr.CurrentChecksum(), PolicyChecksum([]byte(updated)); got != want {
		t.Fatalf("checksum after reload = %q, want %q", got, want)
	}

	if err := os.WriteFile(path, []byte("version: 99\nbudgets: {}\n"), 0o644); err != nil {
		t.Fatalf("write bad: %v", err)
	}
	if err := mgr.Reload(); err == nil {
		t.Fatal("expected validation error on bad policy")
	}
	if got, want := mgr.CurrentChecksum(), PolicyChecksum([]byte(updated)); got != want {
		t.Fatalf("rejected reload moved the checksum to %q, want %q", got, want)
	}
}

// TestPolicyManager_NilChecksum: a nil manager reports no checksum rather than
// panicking, so a reconciler wired without a policy manager still stamps.
func TestPolicyManager_NilChecksum(t *testing.T) {
	var mgr *PolicyManager
	if got := mgr.CurrentChecksum(); got != "" {
		t.Fatalf("nil manager checksum = %q, want empty", got)
	}
}
