package spawn

import (
	"context"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// TestDeriveSpawnIDDeterministic verifies the derived id is stable for a
// given key and shape-compatible with NewSpawnID() output: "spawn-" prefix
// plus derivedSpawnIDHexLen lowercase hex chars, sliceable at [6:].
func TestDeriveSpawnIDDeterministic(t *testing.T) {
	const key = "mills/run-123/stage-implement"

	got1 := DeriveSpawnID(key)
	got2 := DeriveSpawnID(key)
	if got1 != got2 {
		t.Fatalf("DeriveSpawnID not deterministic: %q != %q", got1, got2)
	}

	if !strings.HasPrefix(got1, "spawn-") {
		t.Errorf("derived id %q missing %q prefix", got1, "spawn-")
	}
	body := got1[len("spawn-"):]
	if len(body) != derivedSpawnIDHexLen {
		t.Errorf("derived id body len: got %d, want %d (%q)", len(body), derivedSpawnIDHexLen, got1)
	}
	for _, r := range body {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !isHex {
			t.Errorf("derived id body %q has non-hex char %q", body, r)
			break
		}
	}

	// Distinct keys produce distinct ids.
	if other := DeriveSpawnID(key + "-x"); other == got1 {
		t.Errorf("distinct keys collided: both %q", got1)
	}
}

// TestSpawnNoKeyIsServerMinted proves the no-key path is byte-identical to
// legacy behavior: the id is server-minted (random), so two creates with
// the same content but no key produce DIFFERENT ids, and neither equals the
// deterministic derivation. This is the regression guard for the
// behavior-preserving requirement.
func TestSpawnNoKeyIsServerMinted(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewK8sController(client, "devbox", nil, nil)
	ctx := context.Background()

	req := Request{AgentType: "claude-code", TaskDescription: "task", Project: "proj"}

	id1, err := ctrl.Spawn(ctx, req)
	if err != nil {
		t.Fatalf("Spawn 1: %v", err)
	}
	id2, err := ctrl.Spawn(ctx, req)
	if err != nil {
		t.Fatalf("Spawn 2: %v", err)
	}
	if id1 == id2 {
		t.Fatalf("no-key path should mint random ids; got identical %q", id1)
	}
	// Two separate spawns must exist — no dedupe on the legacy path.
	if _, ok := ctrl.Get(id1); !ok {
		t.Errorf("spawn %q missing", id1)
	}
	if _, ok := ctrl.Get(id2); !ok {
		t.Errorf("spawn %q missing", id2)
	}
	if n := len(ctrl.List()); n != 2 {
		t.Errorf("expected 2 spawns on no-key path, got %d", n)
	}
}

// TestSpawnWithKeyIsDeterministicAndIdempotent proves the opt-in path:
// the id equals DeriveSpawnID(key), and a second create with the SAME key
// re-attaches to the existing spawn (AlreadyExists no-op) — no second
// entry, no second pod handle.
func TestSpawnWithKeyIsDeterministicAndIdempotent(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewK8sController(client, "devbox", nil, nil)
	ctx := context.Background()

	const key = "mills/run-abc/stage-plan"
	req := Request{
		AgentType:       "claude-code",
		TaskDescription: "task",
		Project:         "proj",
		IdempotencyKey:  key,
	}

	id1, err := ctrl.Spawn(ctx, req)
	if err != nil {
		t.Fatalf("Spawn 1: %v", err)
	}
	if want := DeriveSpawnID(key); id1 != want {
		t.Fatalf("key path id: got %q, want derived %q", id1, want)
	}

	first, ok := ctrl.Get(id1)
	if !ok {
		t.Fatalf("spawn %q missing after first create", id1)
	}

	// Second create with the same key → AlreadyExists no-op.
	id2, err := ctrl.Spawn(ctx, req)
	if err != nil {
		t.Fatalf("Spawn 2 (duplicate key): %v", err)
	}
	if id2 != id1 {
		t.Fatalf("duplicate-key create should return same id: got %q, want %q", id2, id1)
	}
	if n := len(ctrl.List()); n != 1 {
		t.Fatalf("duplicate-key create must not add a second spawn; got %d entries", n)
	}
	second, _ := ctrl.Get(id2)
	if second != first {
		t.Errorf("duplicate-key create should re-attach to the same state pointer (no replacement)")
	}
}

// TestSpawnWithKeyRecordBeforeDispatch simulates a crash AFTER the
// controller records the spawn but BEFORE the orchestrator dispatches the
// pod-create. The persisted state is recovered into a fresh controller (the
// post-restart world), and a retry with the SAME key re-attaches to the
// recovered id rather than minting a duplicate. This is the record-before-
// dispatch guarantee that closes the double-spawn window.
func TestSpawnWithKeyRecordBeforeDispatch(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctx := context.Background()

	const key = "mills/run-xyz/stage-implement"
	req := Request{
		AgentType:       "claude-code",
		TaskDescription: "task",
		Project:         "proj",
		IdempotencyKey:  key,
	}

	// --- pre-crash controller: record (persist) the spawn, then "crash"
	// before any pod-create dispatch. The controller never creates a pod;
	// Spawn only records + persists, so the persisted record is the only
	// surviving handle.
	preCrash := NewK8sController(fake.NewSimpleClientset(), "devbox", store, nil)
	id, err := preCrash.Spawn(ctx, req)
	if err != nil {
		t.Fatalf("pre-crash Spawn: %v", err)
	}

	// The handle must be durable: recoverable by the deterministic id.
	loaded, err := store.Load(ctx, id)
	if err != nil || loaded == nil {
		t.Fatalf("record-before-dispatch: state not persisted (err=%v, loaded=%v)", err, loaded)
	}
	if loaded.SpawnID != DeriveSpawnID(key) {
		t.Fatalf("persisted id %q != derived %q", loaded.SpawnID, DeriveSpawnID(key))
	}

	// --- post-crash controller: recover from store, then retry the SAME
	// key. The retry must re-attach to the recovered id (no duplicate).
	postCrash := NewK8sController(fake.NewSimpleClientset(), "devbox", store, nil)
	if err := postCrash.RecoverFromStore(ctx); err != nil {
		t.Fatalf("RecoverFromStore: %v", err)
	}
	retryID, err := postCrash.Spawn(ctx, req)
	if err != nil {
		t.Fatalf("post-crash retry Spawn: %v", err)
	}
	if retryID != id {
		t.Fatalf("retry after crash should re-attach: got %q, want %q", retryID, id)
	}
	if n := len(postCrash.List()); n != 1 {
		t.Fatalf("retry must not duplicate; got %d entries", n)
	}
}
