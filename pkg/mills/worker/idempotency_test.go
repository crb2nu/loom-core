package worker

import (
	"strings"
	"testing"

	"github.com/crb2nu/loom/internal/spawn"
)

// TestDeriveSpawnIDParity is the load-bearing parity guard: the worker
// package mirrors internal/spawn's derivation (it cannot import that
// package without dragging in k8s client-go), so a key sent on a create
// (idempotency_key) and the same key used to Resume MUST map to the same
// spawn id the controller registers. If either side's algorithm drifts,
// resume-by-key silently breaks and the double-spawn window reopens.
func TestDeriveSpawnIDParity(t *testing.T) {
	keys := []string{
		"",
		"mills/run-1/stage-plan",
		"mills/run-abc/stage-implement",
		"a-very-long-idempotency-key-with-unicode-✓-and-slashes/x/y/z",
		"spawn-already-looks-like-an-id",
	}
	for _, k := range keys {
		got := DeriveSpawnID(k)
		want := spawn.DeriveSpawnID(k)
		if got != want {
			t.Errorf("DeriveSpawnID parity drift for key %q: worker=%q spawn=%q", k, got, want)
		}
	}
}

// TestDeriveSpawnIDDeterministicAndShaped verifies the worker-side helper
// is stable and shape-compatible with a spawn id.
func TestDeriveSpawnIDDeterministicAndShaped(t *testing.T) {
	const key = "mills/run-9/stage-pr_self_review"
	id := DeriveSpawnID(key)
	if again := DeriveSpawnID(key); again != id {
		t.Fatalf("DeriveSpawnID not deterministic: %q != %q", again, id)
	}
	if !strings.HasPrefix(id, "spawn-") {
		t.Errorf("id %q missing spawn- prefix", id)
	}
	if got := len(id[len("spawn-"):]); got != derivedSpawnIDHexLen {
		t.Errorf("id body len: got %d want %d", got, derivedSpawnIDHexLen)
	}
}
