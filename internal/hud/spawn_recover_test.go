package hud

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/spawn"
)

type transientRecoveryStore struct {
	inner    spawn.Store
	failures int
}

func (s *transientRecoveryStore) Save(ctx context.Context, state *spawn.State) error {
	return s.inner.Save(ctx, state)
}
func (s *transientRecoveryStore) Load(ctx context.Context, id string) (*spawn.State, error) {
	return s.inner.Load(ctx, id)
}
func (s *transientRecoveryStore) LoadAll(ctx context.Context) ([]*spawn.State, error) {
	if s.failures > 0 {
		s.failures--
		return nil, errors.New("transient ConfigMap GET failure")
	}
	return s.inner.LoadAll(ctx)
}
func (s *transientRecoveryStore) Delete(ctx context.Context, id string) error {
	return s.inner.Delete(ctx, id)
}

// TestClassifyInterruptedSpawn pins the restart-recovery decision table for
// loom-core#300: a spawn whose driving exec session died with the previous
// mobile-hud process must be re-driven (keyed) or failed fast (unkeyed) —
// never left as a `running` zombie.
func TestClassifyInterruptedSpawn(t *testing.T) {
	keyedReq := spawn.Request{
		AgentType:       "claude-code",
		TaskDescription: "do the thing",
		Project:         "services/loom-core",
		IdempotencyKey:  "wf-abc123",
	}
	unkeyedReq := spawn.Request{
		AgentType:       "claude-code",
		TaskDescription: "do the thing",
		Project:         "services/loom-core",
	}

	cases := []struct {
		name  string
		state *spawn.State
		want  interruptedSpawnAction
	}{
		{"nil state", nil, interruptedSkip},
		{"terminal completed", &spawn.State{Status: spawn.StatusCompleted, Request: keyedReq}, interruptedSkip},
		{"terminal failed", &spawn.State{Status: spawn.StatusFailed, Request: keyedReq}, interruptedSkip},
		{
			// resumePreRuntimeSpawns owns pending/building with no pod.
			"pre-runtime no pod",
			&spawn.State{Status: spawn.StatusPending, Request: keyedReq},
			interruptedSkip,
		},
		{
			// The S1c kill-test shape: keyed, running, live pod — the turn
			// died with the old process; re-drive on the adopted pod.
			"keyed running with pod",
			&spawn.State{Status: spawn.StatusRunning, PodName: "spawn-spawn-abc", Request: keyedReq},
			interruptedRedrive,
		},
		{
			// Keyed, dispatched but crash landed before the pod-name was
			// recorded — re-drive recreates/adopts the deterministic pod.
			"keyed running no pod",
			&spawn.State{Status: spawn.StatusRunning, Request: keyedReq},
			interruptedRedrive,
		},
		{
			// Building WITH a pod name is not pre-runtime shape; keyed →
			// re-drive rather than zombie.
			"keyed building with pod",
			&spawn.State{Status: spawn.StatusBuilding, PodName: "spawn-spawn-abc", Request: keyedReq},
			interruptedRedrive,
		},
		{
			"unkeyed running with pod",
			&spawn.State{Status: spawn.StatusRunning, PodName: "spawn-abc", Request: unkeyedReq},
			interruptedFailFast,
		},
		{
			// Keyed but the request lost the fields a re-drive needs (e.g.
			// label-reconstructed state) — cannot re-drive; fail honestly.
			"keyed but empty task",
			&spawn.State{Status: spawn.StatusRunning, PodName: "spawn-abc",
				Request: spawn.Request{IdempotencyKey: "wf-abc", Project: "services/loom-core"}},
			interruptedFailFast,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyInterruptedSpawn(tc.state); got != tc.want {
				t.Errorf("classifyInterruptedSpawn = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDefaultSpawnConfigControllerOwnershipFromEnv(t *testing.T) {
	t.Setenv("SPAWN_CONTROLLER_ID", "loom-hub/mobile-hud")
	t.Setenv("SPAWN_RECOVERY_AUTHORITY", "yes")
	cfg := DefaultSpawnConfig()
	if cfg.ControllerID != "loom-hub/mobile-hud" || !cfg.RecoveryAuthority {
		t.Fatalf("ownership defaults = %q/%v", cfg.ControllerID, cfg.RecoveryAuthority)
	}
}

func TestLocalSpawnControllerIDSeparatesRuntimeRoles(t *testing.T) {
	loomd := localSpawnControllerID("devbox", "/Users/operator", "loomd")
	standalone := localSpawnControllerID("devbox", "/Users/operator", "loom")
	if loomd == standalone {
		t.Fatalf("embedded and standalone owner IDs collided: %q", loomd)
	}
	if loomd != localSpawnControllerID("devbox", "/Users/operator", "loomd") {
		t.Fatal("local owner ID is not stable for one runtime role")
	}
}

func TestLocalSpawnControllerIDSeparatesRuntimeEndpoints(t *testing.T) {
	first := localSpawnControllerID("devbox", "/Users/operator", "loom", "/tmp/loom-a.sock", "127.0.0.1:3333")
	second := localSpawnControllerID("devbox", "/Users/operator", "loom", "/tmp/loom-b.sock", "127.0.0.1:3334")
	if first == second {
		t.Fatalf("different local endpoints shared controller ID %q", first)
	}
	if first != localSpawnControllerID("devbox", "/Users/operator", "loom", "/tmp/loom-a.sock", "127.0.0.1:3333") {
		t.Fatal("same local endpoint did not retain a stable controller ID")
	}
}

// TestRecoverInterruptedSpawns_RedrivesKeyedAndFailsUnkeyed drives the
// orchestrator-level loop: a keyed interrupted spawn is re-driven via the
// (test-seamed) runSpawn launcher; an unkeyed one is failed immediately with
// the honest driver-lost error.
func TestRecoverInterruptedSpawns_RedrivesKeyedAndFailsUnkeyed(t *testing.T) {
	ctx := t.Context()
	store, err := spawn.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create spawn store: %v", err)
	}
	ctrl := spawn.NewK8sController(nil, "", store, slog.Default())

	keyedID, err := ctrl.Spawn(ctx, spawn.Request{
		AgentType:       "claude-code",
		TaskDescription: "workflow step",
		Project:         "services/loom-core",
		IdempotencyKey:  "wf-deadbeef",
	})
	if err != nil {
		t.Fatalf("seed keyed spawn: %v", err)
	}
	keyedState, _ := ctrl.Get(keyedID)
	keyedState.Status = spawn.StatusRunning
	keyedState.PodName = "spawn-" + keyedID
	ctrl.UpdateState(ctx, keyedState)

	unkeyedID, err := ctrl.Spawn(ctx, spawn.Request{
		AgentType:       "claude-code",
		TaskDescription: "legacy turn",
		Project:         "services/loom-core",
	})
	if err != nil {
		t.Fatalf("seed unkeyed spawn: %v", err)
	}
	unkeyedState, _ := ctrl.Get(unkeyedID)
	unkeyedState.Status = spawn.StatusRunning
	unkeyedState.PodName = "spawn-" + unkeyedID
	ctrl.UpdateState(ctx, unkeyedState)

	// Terminal spawn: must be untouched.
	doneID, err := ctrl.Spawn(ctx, spawn.Request{
		AgentType:       "claude-code",
		TaskDescription: "already done",
		Project:         "services/loom-core",
	})
	if err != nil {
		t.Fatalf("seed done spawn: %v", err)
	}
	doneState, _ := ctrl.Get(doneID)
	doneState.Status = spawn.StatusCompleted
	ctrl.UpdateState(ctx, doneState)

	var redriven []string
	o := &SpawnOrchestrator{
		ctrl:             ctrl,
		logger:           slog.Default(),
		backends:         map[string]backend.Backend{DefaultSubstrate: &recordingBackend{}},
		defaultSubstrate: DefaultSubstrate,
		redriveSpawn: func(spawnID string, _ SpawnRequest) {
			redriven = append(redriven, spawnID)
		},
	}

	o.recoverInterruptedSpawns()

	if len(redriven) != 1 || redriven[0] != keyedID {
		t.Errorf("redriven = %v, want exactly [%s]", redriven, keyedID)
	}

	got, _ := ctrl.Get(unkeyedID)
	if got.Status != spawn.StatusFailed {
		t.Errorf("unkeyed spawn status = %s, want failed", got.Status)
	}
	if got.Error == "" || got.EndedAt == nil {
		t.Errorf("unkeyed spawn must carry an honest error + EndedAt; got error=%q ended=%v", got.Error, got.EndedAt)
	}

	gotDone, _ := ctrl.Get(doneID)
	if gotDone.Status != spawn.StatusCompleted {
		t.Errorf("terminal spawn must be untouched; got %s", gotDone.Status)
	}

	// The keyed spawn's state must NOT have been failed by the loop — the
	// re-drive owns its lifecycle from here.
	gotKeyed, _ := ctrl.Get(keyedID)
	if gotKeyed.Status != spawn.StatusRunning {
		t.Errorf("keyed spawn must be left to the re-drive; got %s", gotKeyed.Status)
	}
}

func TestRecoverSpawnsRedrivesOnlyMatchingDriverOwner(t *testing.T) {
	ctx := t.Context()
	client := k8sfake.NewSimpleClientset()
	store := spawn.NewK8sConfigMapStore(client, "devbox", "loom-spawn-state")
	req := spawn.Request{
		AgentType: "codex", Project: "loom-core", TaskDescription: "resume only on owner",
		IdempotencyKey: "wf-owner-recovery",
	}
	writer := spawn.NewK8sController(nil, "devbox", store, slog.Default(),
		spawn.WithControllerOwnership("loom-hub/mobile-hud", false))
	spawnID, dispatch, err := writer.Register(ctx, req)
	if err != nil || !dispatch {
		t.Fatalf("writer Register = %q/%v/%v", spawnID, dispatch, err)
	}
	if _, updated, err := writer.UpdateUnlessStoppingOrTerminal(ctx, spawnID, func(state *spawn.State) {
		state.Status = spawn.StatusRunning
		state.PodName = "spawn-" + spawnID
	}); err != nil || !updated {
		t.Fatalf("writer running transition = %v/%v", updated, err)
	}
	before, err := client.CoreV1().ConfigMaps("devbox").Get(ctx, "loom-spawn-state", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get durable row before recovery: %v", err)
	}
	beforeRaw := before.Data[spawnID]

	foreignCtrl := spawn.NewK8sController(nil, "devbox", store, slog.Default(),
		spawn.WithControllerOwnership("local/desktop", false))
	var foreignRedrives []string
	foreign := &SpawnOrchestrator{
		ctrl: foreignCtrl, logger: slog.Default(),
		backends: map[string]backend.Backend{DefaultSubstrate: &recordingBackend{}}, defaultSubstrate: DefaultSubstrate,
		redriveSpawn: func(id string, _ SpawnRequest) { foreignRedrives = append(foreignRedrives, id) },
	}
	foreign.RecoverSpawns()
	foreign.RecoverSpawns()
	if len(foreignRedrives) != 0 || len(foreignCtrl.List()) != 0 {
		t.Fatalf("foreign recovery redrives=%v states=%#v", foreignRedrives, foreignCtrl.List())
	}

	replacementCtrl := spawn.NewK8sController(nil, "devbox", store, slog.Default(),
		spawn.WithControllerOwnership("loom-hub/mobile-hud", false))
	var ownerRedrives []string
	replacement := &SpawnOrchestrator{
		ctrl: replacementCtrl, logger: slog.Default(),
		backends: map[string]backend.Backend{DefaultSubstrate: &recordingBackend{}}, defaultSubstrate: DefaultSubstrate,
		redriveSpawn: func(id string, _ SpawnRequest) { ownerRedrives = append(ownerRedrives, id) },
	}
	replacement.RecoverSpawns()
	replacement.RecoverSpawns()
	if len(ownerRedrives) != 1 || ownerRedrives[0] != spawnID {
		t.Fatalf("owner redrives = %v, want exactly [%s]", ownerRedrives, spawnID)
	}
	state, ok := replacementCtrl.Get(spawnID)
	if !ok || state.DriverOwnerID != "loom-hub/mobile-hud" || state.Request.IdempotencyKey != req.IdempotencyKey {
		t.Fatalf("replacement state = %#v", state)
	}
	after, err := client.CoreV1().ConfigMaps("devbox").Get(ctx, "loom-spawn-state", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get durable row after recovery: %v", err)
	}
	if got := after.Data[spawnID]; got != beforeRaw {
		t.Fatalf("recovery rewrote durable row\nbefore: %s\nafter:  %s", beforeRaw, got)
	}
}

func TestRecoverSpawnsContext_TransientFailureRemainsRetryable(t *testing.T) {
	inner, err := spawn.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	req := spawn.Request{
		AgentType: "codex", Project: "loom-core", TaskDescription: "resume",
		IdempotencyKey: "mills/recovery-retry",
	}
	spawnID := spawn.DeriveSpawnID(req.IdempotencyKey)
	state := &spawn.State{
		SpawnID: spawnID, AgentID: "spawn-codex-" + spawnID[6:],
		DriverOwnerID: "loom-hub/mobile-hud", Status: spawn.StatusRunning,
		PodName: "spawn-" + spawnID, Request: req, StartedAt: time.Now(),
	}
	if err := inner.Save(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	store := &transientRecoveryStore{inner: inner, failures: 1}
	ctrl := spawn.NewK8sController(nil, "devbox", store, slog.Default(),
		spawn.WithControllerOwnership("loom-hub/mobile-hud", false))
	var redrives []string
	o := &SpawnOrchestrator{
		ctrl: ctrl, logger: slog.Default(),
		backends:         map[string]backend.Backend{DefaultSubstrate: &recordingBackend{}},
		defaultSubstrate: DefaultSubstrate,
		redriveSpawn:     func(id string, _ SpawnRequest) { redrives = append(redrives, id) },
	}
	if err := o.RecoverSpawnsContext(t.Context()); err == nil {
		t.Fatal("first recovery unexpectedly succeeded")
	}
	if o.recovered || len(redrives) != 0 || len(ctrl.List()) != 0 {
		t.Fatalf("failed recovery leaked state: recovered=%v redrives=%v states=%v", o.recovered, redrives, ctrl.List())
	}
	if err := o.RecoverSpawnsContext(t.Context()); err != nil {
		t.Fatalf("retry recovery: %v", err)
	}
	if !o.recovered || len(redrives) != 1 || redrives[0] != spawnID {
		t.Fatalf("retry recovery = recovered %v redrives %v", o.recovered, redrives)
	}
}
