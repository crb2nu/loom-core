package hud

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/spawn"
)

// startBackstopBackend is a minimal backend.Backend whose Start always returns
// startErr (and records how many times it was called). It models the live k8s
// create path returning apierrors.AlreadyExists across a crash — the cold
// process re-derives the same key, re-creates the same deterministic pod name,
// and the cluster rejects the second create.
type startBackstopBackend struct {
	startErr   error
	startCalls int
	lastOpts   backend.StartOpts
}

func (b *startBackstopBackend) Build(_ context.Context, _ backend.BuildOpts) (*backend.BuildResult, error) {
	return nil, backend.ErrNotSupported
}
func (b *startBackstopBackend) Start(_ context.Context, opts backend.StartOpts) (*backend.StartResult, error) {
	b.startCalls++
	b.lastOpts = opts
	if b.startErr != nil {
		return nil, b.startErr
	}
	return &backend.StartResult{ContainerID: opts.Name}, nil
}
func (b *startBackstopBackend) Exec(_ context.Context, _ backend.ExecOpts) (*backend.ExecResult, error) {
	return nil, backend.ErrNotSupported
}
func (b *startBackstopBackend) Stop(_ context.Context, _ string) error { return nil }
func (b *startBackstopBackend) Status(_ context.Context, _ string) (*backend.StatusResult, error) {
	return nil, backend.ErrNotSupported
}
func (b *startBackstopBackend) Health(_ context.Context) error { return nil }
func (b *startBackstopBackend) Pause(_ context.Context, _ string) error {
	return backend.ErrNotSupported
}
func (b *startBackstopBackend) Resume(_ context.Context, _ string) error {
	return backend.ErrNotSupported
}
func (b *startBackstopBackend) ReadFile(_ context.Context, _, _ string) ([]byte, error) {
	return nil, backend.ErrNotSupported
}
func (b *startBackstopBackend) WriteFile(_ context.Context, _, _ string, _ []byte, _ string) error {
	return backend.ErrNotSupported
}
func (b *startBackstopBackend) CleanupBuilds(_ context.Context, _ time.Duration) (int, error) {
	return 0, nil
}

func newAlreadyExistsErr(podName string) error {
	return apierrors.NewAlreadyExists(
		schema.GroupResource{Group: "", Resource: "pods"}, podName)
}

func backstopOrchestrator() *SpawnOrchestrator {
	return &SpawnOrchestrator{logger: slog.Default()}
}

// TestStartSpawnPod_KeyedAlreadyExistsReAttaches is the core
// exactly-once-across-crash backstop test (.loom/134 §5). A keyed spawn whose
// derived pod name collides with an AlreadyExists from the live create path —
// the COLD-controller crash-resume case — must RE-ATTACH (adopt the existing
// pod, succeed) instead of failing. The orchestrator here has an empty
// in-memory map (no controller seeded), exactly modelling a fresh process that
// lost the spawnWithKey dedupe entry.
func TestStartSpawnPod_KeyedAlreadyExistsReAttaches(t *testing.T) {
	const key = "mills/run-crash/stage-implement"
	spawnID := spawn.DeriveSpawnID(key)
	podName := "spawn-" + spawnID

	be := &startBackstopBackend{startErr: newAlreadyExistsErr(podName)}
	o := backstopOrchestrator()

	req := SpawnRequest{
		AgentType:       "claude-code",
		Project:         "loom-core",
		TaskDescription: "task",
		IdempotencyKey:  key,
	}

	res, err := o.startSpawnPod(context.Background(), be, req, spawnID,
		backend.StartOpts{Name: podName})
	if err != nil {
		t.Fatalf("keyed AlreadyExists must re-attach, not fail; got err=%v", err)
	}
	if res == nil {
		t.Fatal("re-attach must return a StartResult (the adopted pod handle), got nil")
	}
	if res.ContainerID != podName {
		t.Fatalf("re-attach must adopt the deterministic pod name: got %q, want %q",
			res.ContainerID, podName)
	}
	if be.startCalls != 1 {
		t.Fatalf("Start should be attempted exactly once, got %d", be.startCalls)
	}
}

// TestStartSpawnPod_LegacyAlreadyExistsStillFails proves the legacy (non-keyed,
// random-name) path is UNCHANGED: an AlreadyExists error on a non-derived name
// propagates verbatim so runSpawn failSpawns exactly as before. This is the
// behavior-preserving guard for the constraint that the backstop must never
// touch the legacy create semantics.
func TestStartSpawnPod_LegacyAlreadyExistsStillFails(t *testing.T) {
	// A server-minted (random) spawn id — no idempotency key.
	spawnID := spawn.NewSpawnID()
	podName := "spawn-" + spawnID

	wantErr := newAlreadyExistsErr(podName)
	be := &startBackstopBackend{startErr: wantErr}
	o := backstopOrchestrator()

	req := SpawnRequest{
		AgentType:       "claude-code",
		Project:         "loom-core",
		TaskDescription: "task",
		// IdempotencyKey intentionally empty — the legacy path.
	}

	res, err := o.startSpawnPod(context.Background(), be, req, spawnID,
		backend.StartOpts{Name: podName})
	if err == nil {
		t.Fatal("legacy non-keyed AlreadyExists must propagate as an error (no re-attach)")
	}
	if res != nil {
		t.Fatalf("legacy path must not synthesize a re-attach StartResult, got %+v", res)
	}
	if !apierrors.IsAlreadyExists(err) {
		t.Fatalf("legacy error must propagate verbatim; got %v", err)
	}
}

// TestStartSpawnPod_NonAlreadyExistsErrorPropagates proves a keyed spawn still
// fails on a NON-AlreadyExists create error: the backstop only adopts on a real
// AlreadyExists, never on an arbitrary failure (e.g. quota exceeded, image
// pull). Otherwise the durable invariant would mask genuine create failures.
func TestStartSpawnPod_NonAlreadyExistsErrorPropagates(t *testing.T) {
	const key = "mills/run-quota/stage-plan"
	spawnID := spawn.DeriveSpawnID(key)
	podName := "spawn-" + spawnID

	wantErr := errors.New("create pod: namespace quota exceeded")
	be := &startBackstopBackend{startErr: wantErr}
	o := backstopOrchestrator()

	req := SpawnRequest{
		AgentType:       "claude-code",
		Project:         "loom-core",
		TaskDescription: "task",
		IdempotencyKey:  key,
	}

	res, err := o.startSpawnPod(context.Background(), be, req, spawnID,
		backend.StartOpts{Name: podName})
	if err == nil {
		t.Fatal("non-AlreadyExists create error must propagate even for a keyed spawn")
	}
	if res != nil {
		t.Fatalf("must not re-attach on a non-AlreadyExists error, got %+v", res)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error must propagate verbatim; got %v", err)
	}
}

// TestStartSpawnPod_SuccessPassesThrough is the happy path: no create error
// means the backend's StartResult is returned untouched, no backstop logic.
func TestStartSpawnPod_SuccessPassesThrough(t *testing.T) {
	const key = "mills/run-ok/stage-implement"
	spawnID := spawn.DeriveSpawnID(key)
	podName := "spawn-" + spawnID

	be := &startBackstopBackend{} // startErr nil → success
	o := backstopOrchestrator()

	req := SpawnRequest{
		AgentType:       "claude-code",
		Project:         "loom-core",
		TaskDescription: "task",
		IdempotencyKey:  key,
	}

	res, err := o.startSpawnPod(context.Background(), be, req, spawnID,
		backend.StartOpts{Name: podName})
	if err != nil {
		t.Fatalf("success path must not error, got %v", err)
	}
	if res == nil || res.ContainerID != podName {
		t.Fatalf("success path must return the backend StartResult, got %+v", res)
	}
}

// TestStartSpawnPod_KeyedButNameMismatchFails covers the defensive case where a
// spawn carries an idempotency key but the pod name does NOT match the derived
// name (e.g. a corrupted or mis-wired Name). IsDerivedSpawnName returns false,
// so AlreadyExists must NOT be silently adopted — we only re-attach when the
// collision is provably the deterministic name.
func TestStartSpawnPod_KeyedButNameMismatchFails(t *testing.T) {
	const key = "mills/run-mismatch/stage-plan"
	spawnID := spawn.DeriveSpawnID(key)
	// Wrong pod name — not "spawn-"+DeriveSpawnID(key).
	podName := "spawn-not-the-derived-name"

	be := &startBackstopBackend{startErr: newAlreadyExistsErr(podName)}
	o := backstopOrchestrator()

	req := SpawnRequest{
		AgentType:       "claude-code",
		Project:         "loom-core",
		TaskDescription: "task",
		IdempotencyKey:  key,
	}

	res, err := o.startSpawnPod(context.Background(), be, req, spawnID,
		backend.StartOpts{Name: podName})
	if err == nil {
		t.Fatal("AlreadyExists on a non-derived name must NOT be adopted as a re-attach")
	}
	if res != nil {
		t.Fatalf("must not re-attach when the name is not the derived name, got %+v", res)
	}
}
