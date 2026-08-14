package spawn

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// ---------- FileStore tests ----------

func TestFileStore_SaveAndLoad(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctx := context.Background()

	now := time.Now().Truncate(time.Second)
	state := &State{
		SpawnID:   "spawn-abc123",
		AgentID:   "spawn-claude-code-abc123",
		PodName:   "spawn-abc123",
		Status:    StatusRunning,
		StartedAt: now,
		Request: Request{
			AgentType:       "claude-code",
			Namespace:       "test/spawn",
			Project:         "loom-core",
			TaskDescription: "fix the bug",
			MemoryMB:        4096,
			CPUs:            2.0,
			TimeoutMinutes:  60,
		},
	}

	if err := store.Save(ctx, state); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := store.Load(ctx, "spawn-abc123")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected non-nil state")
	}
	if loaded.SpawnID != state.SpawnID {
		t.Errorf("SpawnID: got %q, want %q", loaded.SpawnID, state.SpawnID)
	}
	if loaded.Status != StatusRunning {
		t.Errorf("Status: got %q, want %q", loaded.Status, StatusRunning)
	}
}

func TestFileStore_LoadAll(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctx := context.Background()

	for _, id := range []string{"spawn-001", "spawn-002", "spawn-003"} {
		if err := store.Save(ctx, &State{
			SpawnID:   id,
			Status:    StatusRunning,
			StartedAt: time.Now(),
		}); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}

	states, err := store.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(states) != 3 {
		t.Errorf("LoadAll: got %d, want 3", len(states))
	}
}

func TestFileStore_Delete(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctx := context.Background()

	state := &State{SpawnID: "spawn-del", Status: StatusCompleted, StartedAt: time.Now()}
	if err := store.Save(ctx, state); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Delete(ctx, "spawn-del"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	loaded, err := store.Load(ctx, "spawn-del")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded != nil {
		t.Error("expected nil after delete")
	}
}

func TestFileStore_DeleteNonexistent(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.Delete(context.Background(), "spawn-nope"); err != nil {
		t.Errorf("Delete nonexistent: %v", err)
	}
}

func TestFileStore_SaveNil(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.Save(context.Background(), nil); err == nil {
		t.Error("expected error saving nil state")
	}
}

func TestFileStore_EmptyDir(t *testing.T) {
	_, err := NewFileStore("")
	if err == nil {
		t.Error("expected error for empty dir")
	}
}

func TestFileStore_PruneCompleted(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctx := context.Background()

	now := time.Now()
	old := now.Add(-2 * time.Hour)
	recent := now.Add(-5 * time.Minute)

	_ = store.Save(ctx, &State{SpawnID: "old-done", Status: StatusCompleted, StartedAt: now.Add(-3 * time.Hour), EndedAt: &old})
	_ = store.Save(ctx, &State{SpawnID: "recent-done", Status: StatusCompleted, StartedAt: now.Add(-10 * time.Minute), EndedAt: &recent})
	_ = store.Save(ctx, &State{SpawnID: "running", Status: StatusRunning, StartedAt: now.Add(-3 * time.Hour)})

	if err := store.PruneCompleted(ctx, 1*time.Hour); err != nil {
		t.Fatalf("PruneCompleted: %v", err)
	}

	states, _ := store.LoadAll(ctx)
	if len(states) != 2 {
		t.Fatalf("expected 2 after prune, got %d", len(states))
	}

	ids := make(map[string]bool)
	for _, s := range states {
		ids[s.SpawnID] = true
	}
	if !ids["recent-done"] {
		t.Error("expected recent-done to survive prune")
	}
	if !ids["running"] {
		t.Error("expected running to survive prune")
	}
}

// ---------- K8sConfigMapStore tests ----------

func TestK8sConfigMapStore_SaveAndLoad(t *testing.T) {
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, "devbox", "test-spawn-state")
	ctx := context.Background()

	state := &State{
		SpawnID:   "spawn-k8s-001",
		AgentID:   "agent-k8s-001",
		Status:    StatusRunning,
		StartedAt: time.Now(),
		Request: Request{
			AgentType:       "claude-code",
			Project:         "proj",
			TaskDescription: "task",
		},
	}

	if err := store.Save(ctx, state); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := store.Load(ctx, "spawn-k8s-001")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected non-nil state")
	}
	if loaded.SpawnID != "spawn-k8s-001" {
		t.Errorf("SpawnID: got %q, want %q", loaded.SpawnID, "spawn-k8s-001")
	}
}

func TestK8sConfigMapStore_KeyedRecordRejectsLossyOverwrite(t *testing.T) {
	ctx := t.Context()
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, "devbox", "test-spawn-state")
	startedAt := time.Now().Add(-time.Minute).Truncate(time.Second)
	full := &State{
		SpawnID:   "spawn-keyed-lossy",
		AgentID:   "agent-owner",
		PodName:   "spawn-spawn-keyed-lossy",
		Status:    StatusRunning,
		StartedAt: startedAt,
		Request: Request{
			AgentType:       "codex",
			Project:         "loom-core",
			TaskDescription: "preserve the full request",
			TimeoutMinutes:  30,
			IdempotencyKey:  "wf-keyed-lossy",
		},
	}
	if err := store.Save(ctx, full); err != nil {
		t.Fatalf("save full state: %v", err)
	}
	before, err := client.CoreV1().ConfigMaps("devbox").Get(ctx, "test-spawn-state", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get state before lossy save: %v", err)
	}
	beforeRaw := before.Data[full.SpawnID]

	lossy := &State{
		SpawnID:   full.SpawnID,
		AgentID:   "agent-observer",
		PodName:   full.PodName,
		Status:    StatusFailed,
		StartedAt: time.Now(),
		Error:     "pod not found during reconciliation",
	}
	if err := store.Save(ctx, lossy); err != nil {
		t.Fatalf("lossy peer save should be an ignored no-op, got: %v", err)
	}
	after, err := client.CoreV1().ConfigMaps("devbox").Get(ctx, "test-spawn-state", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get state after lossy save: %v", err)
	}
	if got := after.Data[full.SpawnID]; got != beforeRaw {
		t.Fatalf("lossy writer changed keyed state\nbefore: %s\nafter:  %s", beforeRaw, got)
	}
}

func TestK8sConfigMapStore_KeyedDriverOwnerIsImmutable(t *testing.T) {
	ctx := t.Context()
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, "devbox", "test-spawn-state")
	original := &State{
		SpawnID:       "spawn-owner-fence",
		AgentID:       "agent-owner",
		DriverOwnerID: "loom-hub/mobile-hud",
		Status:        StatusRunning,
		StartedAt:     time.Now().Add(-time.Minute),
		Request: Request{
			AgentType: "codex", Project: "loom-core", TaskDescription: "one driver",
			IdempotencyKey: "wf-owner-fence",
		},
	}
	if err := store.Save(ctx, original); err != nil {
		t.Fatalf("save owner: %v", err)
	}
	before, err := client.CoreV1().ConfigMaps("devbox").Get(ctx, "test-spawn-state", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get before conflict: %v", err)
	}
	beforeRaw := before.Data[original.SpawnID]

	for _, owner := range []string{"desktop/cblevins", ""} {
		incoming := *original
		incoming.DriverOwnerID = owner
		incoming.Status = StatusPending
		if err := store.Save(ctx, &incoming); !errors.Is(err, ErrSpawnStateConflict) {
			t.Fatalf("owner %q save error = %v, want ErrSpawnStateConflict", owner, err)
		}
	}
	after, err := client.CoreV1().ConfigMaps("devbox").Get(ctx, "test-spawn-state", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get after conflict: %v", err)
	}
	if got := after.Data[original.SpawnID]; got != beforeRaw {
		t.Fatalf("owner conflict changed durable row\nbefore: %s\nafter:  %s", beforeRaw, got)
	}
}

func TestK8sConfigMapStore_LegacyRowMayBeClaimedOnce(t *testing.T) {
	ctx := t.Context()
	store := NewK8sConfigMapStore(fake.NewSimpleClientset(), "devbox", "test-spawn-state")
	legacy := &State{
		SpawnID: "spawn-legacy-owner", PodName: "spawn-legacy-owner-pod", Status: StatusRunning, StartedAt: time.Now(),
		Request: Request{AgentType: "codex", Project: "loom-core", TaskDescription: "migrate", IdempotencyKey: "wf-legacy-owner"},
	}
	if err := store.Save(ctx, legacy); err != nil {
		t.Fatalf("save legacy row: %v", err)
	}
	claimed := *legacy
	claimed.DriverOwnerID = "loom-hub/mobile-hud"
	claimed.Status = StatusPending // stale LoadAll snapshot must not regress Running.
	claimed.PodName = ""
	if err := store.Save(ctx, &claimed); err != nil {
		t.Fatalf("claim legacy row: %v", err)
	}
	peer := claimed
	peer.DriverOwnerID = "desktop/cblevins"
	if err := store.Save(ctx, &peer); !errors.Is(err, ErrSpawnStateConflict) {
		t.Fatalf("second claim error = %v, want ErrSpawnStateConflict", err)
	}
	loaded, err := store.Load(ctx, legacy.SpawnID)
	if err != nil {
		t.Fatalf("load claimed row: %v", err)
	}
	if loaded.DriverOwnerID != claimed.DriverOwnerID {
		t.Fatalf("driver owner = %q, want %q", loaded.DriverOwnerID, claimed.DriverOwnerID)
	}
	if loaded.Status != StatusRunning || loaded.PodName != legacy.PodName {
		t.Fatalf("ownership claim rewrote lifecycle state: %#v", loaded)
	}
}

func TestK8sConfigMapStore_KeyedTerminalWinnerIsSticky(t *testing.T) {
	ctx := t.Context()
	store := NewK8sConfigMapStore(fake.NewSimpleClientset(), "devbox", "test-spawn-state")
	request := Request{
		AgentType:       "codex",
		Project:         "loom-core",
		TaskDescription: "finish once",
		IdempotencyKey:  "wf-terminal-winner",
	}
	startedAt := time.Now().Add(-time.Minute).Truncate(time.Second)
	endedAt := startedAt.Add(30 * time.Second)
	completed := &State{
		SpawnID: "spawn-terminal-winner", AgentID: "agent-owner", PodName: "spawn-terminal-winner",
		Status: StatusCompleted, Request: request, StartedAt: startedAt, EndedAt: &endedAt,
	}
	if err := store.Save(ctx, completed); err != nil {
		t.Fatalf("save completed winner: %v", err)
	}

	for _, staleStatus := range []Status{StatusRunning, StatusFailed, StatusCompleted} {
		stale := *completed
		stale.Status = staleStatus
		stale.Error = "stale peer result"
		stale.EndedAt = nil
		if err := store.Save(ctx, &stale); !errors.Is(err, ErrSpawnStateConflict) {
			t.Fatalf("stale %s save error = %v, want ErrSpawnStateConflict", staleStatus, err)
		}
	}
	loaded, err := store.Load(ctx, completed.SpawnID)
	if err != nil {
		t.Fatalf("load terminal winner: %v", err)
	}
	if loaded.Status != StatusCompleted || loaded.Error != "" || loaded.EndedAt == nil || !loaded.EndedAt.Equal(endedAt) {
		t.Fatalf("terminal winner changed: %#v", loaded)
	}
}

func TestK8sConfigMapStore_KeyedTerminalCleanupEnrichesWithoutChangingWinner(t *testing.T) {
	ctx := t.Context()
	store := NewK8sConfigMapStore(fake.NewSimpleClientset(), "devbox", "test-spawn-state")
	startedAt := time.Now().Add(-time.Minute).Truncate(time.Second)
	endedAt := startedAt.Add(30 * time.Second)
	winner := &State{
		SpawnID: "spawn-terminal-cleanup", AgentID: "agent-owner", SessionID: "session-owner",
		PodName: "spawn-terminal-cleanup", Status: StatusCompleted, StartedAt: startedAt, EndedAt: &endedAt,
		Request: Request{
			AgentType: "codex", Project: "loom-core", TaskDescription: "original task",
			IdempotencyKey: "wf-terminal-cleanup",
		},
	}
	if err := store.Save(ctx, winner); err != nil {
		t.Fatalf("save terminal winner: %v", err)
	}
	cleanupAt := time.Now().Truncate(time.Second)
	ack := *winner
	ack.AgentID = "agent-stale"
	ack.SessionID = ""
	ack.Request.TaskDescription = "mutated task"
	ack.Error = "completed records must not acquire an error"
	ack.CleanupAt = &cleanupAt
	if err := store.Save(ctx, &ack); err != nil {
		t.Fatalf("save cleanup acknowledgement: %v", err)
	}

	loaded, err := store.Load(ctx, winner.SpawnID)
	if err != nil {
		t.Fatalf("load enriched winner: %v", err)
	}
	if loaded.Status != StatusCompleted || loaded.Error != "" {
		t.Fatalf("terminal tuple changed: status=%s error=%q", loaded.Status, loaded.Error)
	}
	if loaded.Request.TaskDescription != "original task" || loaded.AgentID != "agent-owner" || loaded.SessionID != "session-owner" || !loaded.StartedAt.Equal(startedAt) {
		t.Fatalf("immutable identity changed: %#v", loaded)
	}
	if loaded.CleanupAt == nil || !loaded.CleanupAt.Equal(cleanupAt) {
		t.Fatalf("cleanup acknowledgement not retained: %#v", loaded.CleanupAt)
	}
}

func TestK8sConfigMapStore_KeyedWriterRepairsLossyRow(t *testing.T) {
	ctx := t.Context()
	store := NewK8sConfigMapStore(fake.NewSimpleClientset(), "devbox", "test-spawn-state")
	partial := &State{SpawnID: "spawn-repair", AgentID: "agent-label", Status: StatusRunning, StartedAt: time.Now()}
	if err := store.Save(ctx, partial); err != nil {
		t.Fatalf("save partial row: %v", err)
	}
	full := &State{
		SpawnID: "spawn-repair", AgentID: "agent-owner", Status: StatusPending, StartedAt: time.Now().Add(-time.Minute),
		Request: Request{
			AgentType: "codex", Project: "loom-core", TaskDescription: "repair metadata",
			IdempotencyKey: "wf-repair",
		},
	}
	if err := store.Save(ctx, full); err != nil {
		t.Fatalf("save keyed repair: %v", err)
	}
	loaded, err := store.Load(ctx, full.SpawnID)
	if err != nil {
		t.Fatalf("load repaired row: %v", err)
	}
	if loaded.Request.IdempotencyKey != "wf-repair" || loaded.Request.TaskDescription != "repair metadata" || loaded.Status != StatusPending {
		t.Fatalf("keyed writer did not repair row: %#v", loaded)
	}
}

func TestK8sConfigMapStore_DifferentKeyForSameSpawnFailsClosed(t *testing.T) {
	ctx := t.Context()
	store := NewK8sConfigMapStore(fake.NewSimpleClientset(), "devbox", "test-spawn-state")
	original := &State{
		SpawnID: "spawn-key-conflict", Status: StatusPending, StartedAt: time.Now(),
		Request: Request{AgentType: "codex", Project: "loom-core", TaskDescription: "one", IdempotencyKey: "wf-one"},
	}
	if err := store.Save(ctx, original); err != nil {
		t.Fatalf("save original key: %v", err)
	}
	conflict := *original
	conflict.Request.IdempotencyKey = "wf-two"
	conflict.Request.TaskDescription = "two"
	if err := store.Save(ctx, &conflict); !errors.Is(err, ErrSpawnStateConflict) {
		t.Fatalf("different-key error = %v, want ErrSpawnStateConflict", err)
	}
	loaded, err := store.Load(ctx, original.SpawnID)
	if err != nil {
		t.Fatalf("load original after conflict: %v", err)
	}
	if loaded.Request.IdempotencyKey != "wf-one" || loaded.Request.TaskDescription != "one" {
		t.Fatalf("conflicting key changed original: %#v", loaded.Request)
	}
}

func TestK8sConfigMapStore_SameKeyAdvancesAndEnrichesActiveState(t *testing.T) {
	ctx := t.Context()
	store := NewK8sConfigMapStore(fake.NewSimpleClientset(), "devbox", "test-spawn-state")
	startedAt := time.Now().Add(-time.Minute).Truncate(time.Second)
	pending := &State{
		SpawnID: "spawn-active-enrich", AgentID: "agent-owner", Status: StatusPending, StartedAt: startedAt,
		Request: Request{
			AgentType: "codex", Project: "loom-core", TaskDescription: "immutable task",
			IdempotencyKey: "wf-active-enrich",
		},
	}
	if err := store.Save(ctx, pending); err != nil {
		t.Fatalf("save pending state: %v", err)
	}
	running := *pending
	running.AgentID = "agent-stale"
	running.PodName = "spawn-active-enrich-pod"
	running.SessionID = "session-active-enrich"
	running.AuthMode = AuthModeClusterAPIKey
	running.Status = StatusRunning
	running.Request.TaskDescription = "mutated task"
	if err := store.Save(ctx, &running); err != nil {
		t.Fatalf("save running state: %v", err)
	}
	loaded, err := store.Load(ctx, pending.SpawnID)
	if err != nil {
		t.Fatalf("load enriched active state: %v", err)
	}
	if loaded.Status != StatusRunning || loaded.PodName != running.PodName || loaded.SessionID != running.SessionID || loaded.AuthMode != running.AuthMode {
		t.Fatalf("active enrichment missing: %#v", loaded)
	}
	if loaded.AgentID != "agent-owner" || loaded.Request.TaskDescription != "immutable task" || !loaded.StartedAt.Equal(startedAt) {
		t.Fatalf("active identity changed: %#v", loaded)
	}
}

func TestK8sConfigMapStore_RejectsStaleGenerationCleanup(t *testing.T) {
	ctx := t.Context()
	store := NewK8sConfigMapStore(fake.NewSimpleClientset(), "devbox", "test-spawn-state")
	endedAt := time.Now().Add(-time.Minute).UTC()
	current := &State{
		SpawnID:       "spawn-cleanup-generation",
		AgentID:       "spawn-codex-cleanup-generation",
		DriverOwnerID: "loom-hub/mobile-hud",
		Status:        StatusCompleted,
		StartedAt:     time.Now().UTC(),
		EndedAt:       &endedAt,
		Request: Request{
			AgentType:       "codex",
			Project:         "loom-core",
			TaskDescription: "replacement",
			IdempotencyKey:  "wf-cleanup-generation",
		},
	}
	if err := store.Save(ctx, current); err != nil {
		t.Fatalf("save replacement generation: %v", err)
	}

	stale := *current
	stale.StartedAt = current.StartedAt.Add(-time.Hour)
	cleanupAt := time.Now().UTC()
	stale.CleanupAt = &cleanupAt
	stale.Error = "stale error repair"
	if err := store.Save(ctx, &stale); !errors.Is(err, ErrSpawnStateConflict) {
		t.Fatalf("stale cleanup Save error = %v, want ErrSpawnStateConflict", err)
	}
	loaded, err := store.Load(ctx, current.SpawnID)
	if err != nil {
		t.Fatalf("load replacement generation: %v", err)
	}
	if !loaded.StartedAt.Equal(current.StartedAt) || loaded.CleanupAt != nil || loaded.Error != current.Error {
		t.Fatalf("stale cleanup mutated replacement generation: %#v", loaded)
	}
}

func TestK8sConfigMapStore_StopIntentFencesStaleLifecycleWriter(t *testing.T) {
	ctx := t.Context()
	store := NewK8sConfigMapStore(fake.NewSimpleClientset(), "devbox", "test-spawn-state")
	request := Request{
		AgentType: "codex", Project: "loom-core", TaskDescription: "stop safely",
		IdempotencyKey: "wf-stop-fence",
	}
	running := &State{SpawnID: "spawn-stop-fence", Status: StatusRunning, Request: request, StartedAt: time.Now()}
	if err := store.Save(ctx, running); err != nil {
		t.Fatalf("save running state: %v", err)
	}
	stopAt := time.Now().Truncate(time.Second)
	stopping := *running
	stopping.StopRequestedAt = &stopAt
	if err := store.Save(ctx, &stopping); err != nil {
		t.Fatalf("save stop intent: %v", err)
	}

	stale := *running
	stale.Status = StatusCompleted
	endedAt := time.Now()
	stale.EndedAt = &endedAt
	if err := store.Save(ctx, &stale); err == nil {
		t.Fatal("stale lifecycle writer erased a durable stop intent")
	}
	loaded, err := store.Load(ctx, running.SpawnID)
	if err != nil {
		t.Fatalf("load stopping state: %v", err)
	}
	if loaded.Status != StatusRunning || loaded.StopRequestedAt == nil || !loaded.StopRequestedAt.Equal(stopAt) {
		t.Fatalf("stop intent changed after stale write: %#v", loaded)
	}

	stopped := *loaded
	stopped.Status = StatusStopped
	stopped.EndedAt = &endedAt
	if err := store.Save(ctx, &stopped); err != nil {
		t.Fatalf("valid stopped transition: %v", err)
	}
	loaded, err = store.Load(ctx, running.SpawnID)
	if err != nil {
		t.Fatalf("load stopped state: %v", err)
	}
	if loaded.Status != StatusStopped || loaded.StopRequestedAt == nil || loaded.Error != "" {
		t.Fatalf("valid stopped transition not retained: %#v", loaded)
	}
}

func TestK8sConfigMapStore_ConflictRetryRechecksTerminalWinner(t *testing.T) {
	const namespace, name = "devbox", "test-spawn-state"
	ctx := t.Context()
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, namespace, name)
	request := Request{
		AgentType: "codex", Project: "loom-core", TaskDescription: "race terminal write",
		IdempotencyKey: "wf-conflict-terminal",
	}
	running := &State{SpawnID: "spawn-conflict-terminal", Status: StatusRunning, Request: request, StartedAt: time.Now()}
	if err := store.Save(ctx, running); err != nil {
		t.Fatalf("save running state: %v", err)
	}
	endedAt := time.Now()
	completed := *running
	completed.Status = StatusCompleted
	completed.EndedAt = &endedAt
	completedData, err := json.Marshal(&completed)
	if err != nil {
		t.Fatalf("marshal concurrent winner: %v", err)
	}

	resource := corev1.SchemeGroupVersion.WithResource("configmaps")
	updates := 0
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		tracked, getErr := client.Tracker().Get(resource, namespace, name)
		if getErr != nil {
			t.Fatalf("get tracked ConfigMap: %v", getErr)
		}
		concurrent := tracked.(*corev1.ConfigMap).DeepCopy()
		concurrent.ResourceVersion = "2"
		concurrent.Data[running.SpawnID] = string(completedData)
		if updateErr := client.Tracker().Update(resource, concurrent, namespace); updateErr != nil {
			t.Fatalf("inject terminal winner: %v", updateErr)
		}
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Resource: "configmaps"}, name, errors.New("simulated terminal race"))
	})

	lateActive := *running
	lateActive.SessionID = "late-session"
	if err := store.Save(ctx, &lateActive); err == nil {
		t.Fatal("conflict retry overwrote a concurrently committed terminal winner")
	}
	if updates != 1 {
		t.Fatalf("update attempts = %d, want 1 before merge rejected retry", updates)
	}
	loaded, err := store.Load(ctx, running.SpawnID)
	if err != nil {
		t.Fatalf("load concurrent winner: %v", err)
	}
	if loaded.Status != StatusCompleted || loaded.EndedAt == nil || !loaded.EndedAt.Equal(endedAt) {
		t.Fatalf("concurrent terminal winner changed: %#v", loaded)
	}
}

func TestK8sConfigMapStore_CorruptExistingRowFailsClosed(t *testing.T) {
	const namespace, name, spawnID = "devbox", "test-spawn-state", "spawn-corrupt"
	ctx := t.Context()
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string]string{spawnID: "{not-json"},
	})
	store := NewK8sConfigMapStore(client, namespace, name)
	incoming := &State{
		SpawnID: spawnID, Status: StatusPending, StartedAt: time.Now(),
		Request: Request{AgentType: "codex", Project: "loom-core", TaskDescription: "do not hide corruption", IdempotencyKey: "wf-corrupt"},
	}
	if err := store.Save(ctx, incoming); err == nil {
		t.Fatal("Save replaced a corrupt existing row instead of failing closed")
	}
	cm, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get corrupt row after Save: %v", err)
	}
	if got := cm.Data[spawnID]; got != "{not-json" {
		t.Fatalf("corrupt row changed to %q", got)
	}
}

func TestK8sConfigMapStore_FirstSaveCreatesPopulatedConfigMap(t *testing.T) {
	const namespace, name = "devbox", "test-spawn-state"
	client := fake.NewSimpleClientset()
	var created *corev1.ConfigMap
	updates := 0
	client.PrependReactor("create", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		created = action.(k8stesting.CreateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
		return false, nil, nil
	})
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		return false, nil, nil
	})

	store := NewK8sConfigMapStore(client, namespace, name)
	if err := store.Save(context.Background(), &State{SpawnID: "first", Status: StatusPending}); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if created == nil {
		t.Fatal("first Save did not create the backing ConfigMap")
	}
	if _, ok := created.Data["first"]; !ok {
		t.Fatalf("initial Create payload did not contain spawn state: %v", created.Data)
	}
	if updates != 0 {
		t.Fatalf("first uncontended Save issued %d Update request(s), want 0", updates)
	}
}

func TestK8sConfigMapStore_LoadAll(t *testing.T) {
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, "devbox", "")
	ctx := context.Background()

	for _, id := range []string{"spawn-a", "spawn-b", "spawn-c"} {
		_ = store.Save(ctx, &State{SpawnID: id, Status: StatusRunning, StartedAt: time.Now()})
	}

	states, err := store.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(states) != 3 {
		t.Errorf("LoadAll: got %d, want 3", len(states))
	}
}

func TestK8sConfigMapStore_Delete(t *testing.T) {
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, "devbox", "test-cm")
	ctx := context.Background()

	_ = store.Save(ctx, &State{SpawnID: "spawn-del", Status: StatusCompleted, StartedAt: time.Now()})

	if err := store.Delete(ctx, "spawn-del"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	loaded, err := store.Load(ctx, "spawn-del")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded != nil {
		t.Error("expected nil after delete")
	}
	cm, err := client.CoreV1().ConfigMaps("devbox").Get(ctx, "test-cm", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("backing ConfigMap must survive deletion of its last entry: %v", err)
	}
	if len(cm.Data) != 0 {
		t.Fatalf("backing ConfigMap data = %v, want empty", cm.Data)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "delete" && action.GetResource().Resource == "configmaps" {
			t.Fatalf("Delete issued destructive ConfigMap delete action: %#v", action)
		}
	}
}

func TestK8sConfigMapStore_DeleteIfMatchRechecksAfterConflict(t *testing.T) {
	const namespace, name, spawnID = "devbox", "test-cm", "spawn-conditional-delete"
	startedA := time.Now().Add(-time.Hour).UTC()
	startedB := startedA.Add(time.Minute)
	ownerA := &State{
		SpawnID: spawnID, DriverOwnerID: "owner-a", Status: StatusCompleted, StartedAt: startedA,
		Request: Request{IdempotencyKey: "key-a"},
	}
	ownerB := &State{
		SpawnID: spawnID, DriverOwnerID: "owner-b", Status: StatusRunning, StartedAt: startedB,
		Request: Request{IdempotencyKey: "key-b"},
	}
	rawA, _ := json.Marshal(ownerA)
	rawB, _ := json.Marshal(ownerB)
	resource := corev1.SchemeGroupVersion.WithResource("configmaps")
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, ResourceVersion: "1"},
		Data:       map[string]string{spawnID: string(rawA)},
	})
	updates := 0
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates != 1 {
			return false, nil, nil
		}
		tracked, err := client.Tracker().Get(resource, namespace, name)
		if err != nil {
			t.Fatal(err)
		}
		replacement := tracked.(*corev1.ConfigMap).DeepCopy()
		replacement.ResourceVersion = "2"
		replacement.Data[spawnID] = string(rawB)
		if err := client.Tracker().Update(resource, replacement, namespace); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Resource: "configmaps"}, name, errors.New("peer replaced row"),
		)
	})

	store := NewK8sConfigMapStore(client, namespace, name)
	deleted, err := store.DeleteIfMatch(t.Context(), spawnID, deleteConditionForState(ownerA))
	if deleted || !errors.Is(err, ErrSpawnStateConflict) {
		t.Fatalf("DeleteIfMatch = deleted %v, err %v; want conflict", deleted, err)
	}
	loaded, err := store.Load(t.Context(), spawnID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || loaded.DriverOwnerID != ownerB.DriverOwnerID ||
		loaded.Request.IdempotencyKey != ownerB.Request.IdempotencyKey ||
		!loaded.StartedAt.Equal(ownerB.StartedAt) {
		t.Fatalf("peer replacement was not preserved: %+v", loaded)
	}
}

func TestK8sConfigMapStore_SaveRetriesConflictAndPreservesConcurrentEntry(t *testing.T) {
	const namespace, name = "devbox", "test-cm"
	resource := corev1.SchemeGroupVersion.WithResource("configmaps")
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, ResourceVersion: "1"},
		Data:       map[string]string{"existing": "keep-existing"},
	})
	updates := 0
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates != 1 {
			return false, nil, nil
		}
		tracked, err := client.Tracker().Get(resource, namespace, name)
		if err != nil {
			t.Fatalf("get tracked ConfigMap: %v", err)
		}
		concurrent := tracked.(*corev1.ConfigMap).DeepCopy()
		concurrent.ResourceVersion = "2"
		concurrent.Data["concurrent"] = "preserve-concurrent"
		if err := client.Tracker().Update(resource, concurrent, namespace); err != nil {
			t.Fatalf("inject concurrent update: %v", err)
		}
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Resource: "configmaps"}, name, errors.New("simulated concurrent writer"))
	})

	store := NewK8sConfigMapStore(client, namespace, name)
	if err := store.Save(context.Background(), &State{SpawnID: "target", Status: StatusRunning}); err != nil {
		t.Fatalf("Save after conflict: %v", err)
	}
	cm, err := client.CoreV1().ConfigMaps(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get final ConfigMap: %v", err)
	}
	if updates != 2 {
		t.Fatalf("update attempts = %d, want 2", updates)
	}
	for key, want := range map[string]string{
		"existing":   "keep-existing",
		"concurrent": "preserve-concurrent",
	} {
		if got := cm.Data[key]; got != want {
			t.Fatalf("final ConfigMap data[%q] = %q, want %q; data=%v", key, got, want, cm.Data)
		}
	}
	if _, ok := cm.Data["target"]; !ok {
		t.Fatalf("target state missing after conflict retry: %v", cm.Data)
	}
}

func TestK8sConfigMapStore_RetriesTransientTerminalUpdate(t *testing.T) {
	const namespace, name = "devbox", "test-cm"
	ctx := t.Context()
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, namespace, name)
	request := Request{
		AgentType: "codex", Project: "loom-core", TaskDescription: "retry terminal persistence",
		IdempotencyKey: "wf-transient-terminal",
	}
	running := &State{SpawnID: "spawn-transient-terminal", Status: StatusRunning, Request: request, StartedAt: time.Now()}
	if err := store.Save(ctx, running); err != nil {
		t.Fatalf("save running state: %v", err)
	}
	updates := 0
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			return true, nil, apierrors.NewServiceUnavailable("injected transient outage")
		}
		return false, nil, nil
	})
	endedAt := time.Now()
	completed := *running
	completed.Status = StatusCompleted
	completed.EndedAt = &endedAt
	if err := store.Save(ctx, &completed); err != nil {
		t.Fatalf("terminal Save after transient error: %v", err)
	}
	if updates != 2 {
		t.Fatalf("update attempts = %d, want 2", updates)
	}
	loaded, err := store.Load(ctx, running.SpawnID)
	if err != nil {
		t.Fatalf("load completed state: %v", err)
	}
	if loaded.Status != StatusCompleted || loaded.EndedAt == nil || !loaded.EndedAt.Equal(endedAt) {
		t.Fatalf("terminal state not persisted after retry: %#v", loaded)
	}
}

func TestK8sConfigMapStore_AcknowledgesAmbiguousCommittedRetry(t *testing.T) {
	const namespace, name = "devbox", "test-cm"
	ctx := t.Context()
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, namespace, name)
	request := Request{
		AgentType: "codex", Project: "loom-core", TaskDescription: "ack ambiguous commit",
		IdempotencyKey: "wf-ambiguous-terminal",
	}
	running := &State{SpawnID: "spawn-ambiguous-terminal", Status: StatusRunning, Request: request, StartedAt: time.Now()}
	if err := store.Save(ctx, running); err != nil {
		t.Fatalf("save running state: %v", err)
	}
	resource := corev1.SchemeGroupVersion.WithResource("configmaps")
	updates := 0
	client.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates != 1 {
			return false, nil, nil
		}
		committed := action.(k8stesting.UpdateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
		if err := client.Tracker().Update(resource, committed, namespace); err != nil {
			t.Fatalf("commit update before simulated timeout: %v", err)
		}
		return true, nil, apierrors.NewTimeoutError("response lost after commit", 0)
	})
	endedAt := time.Now()
	completed := *running
	completed.Status = StatusCompleted
	completed.EndedAt = &endedAt
	if err := store.Save(ctx, &completed); err != nil {
		t.Fatalf("ambiguous committed Save: %v", err)
	}
	if updates != 1 {
		t.Fatalf("update requests = %d, want 1; exact retry should be a read-only acknowledgement", updates)
	}
	loaded, err := store.Load(ctx, running.SpawnID)
	if err != nil {
		t.Fatalf("load ambiguous winner: %v", err)
	}
	if loaded.Status != StatusCompleted || loaded.EndedAt == nil || !loaded.EndedAt.Equal(endedAt) {
		t.Fatalf("ambiguous committed winner changed: %#v", loaded)
	}
}

func TestK8sConfigMapStore_ForbiddenUpdatePreservesConfigMap(t *testing.T) {
	const namespace, name = "devbox", "test-cm"
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, ResourceVersion: "7"},
		Data:       map[string]string{"original": "unchanged"},
	})
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "configmaps"}, name, errors.New("RBAC denied"))
	})

	store := NewK8sConfigMapStore(client, namespace, name)
	err := store.Save(context.Background(), &State{SpawnID: "target", Status: StatusRunning})
	if err == nil || !strings.Contains(err.Error(), "requires update permission") {
		t.Fatalf("Save forbidden error = %v, want actionable update-permission guidance", err)
	}
	cm, getErr := client.CoreV1().ConfigMaps(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if getErr != nil {
		t.Fatalf("original ConfigMap was removed: %v", getErr)
	}
	if len(cm.Data) != 1 || cm.Data["original"] != "unchanged" {
		t.Fatalf("forbidden update changed original ConfigMap: %v", cm.Data)
	}
	for _, action := range client.Actions() {
		if action.GetResource().Resource == "configmaps" &&
			(action.GetVerb() == "delete" || action.GetVerb() == "create") {
			t.Fatalf("forbidden update used destructive fallback action: %#v", action)
		}
	}
}

func TestK8sConfigMapStore_SaveToleratesCreateAlreadyExistsRace(t *testing.T) {
	const namespace, name = "devbox", "test-cm"
	resource := corev1.SchemeGroupVersion.WithResource("configmaps")
	client := fake.NewSimpleClientset()
	creates := 0
	client.PrependReactor("create", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		creates++
		winner := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, ResourceVersion: "1"},
			Data:       map[string]string{"concurrent": "race-winner"},
		}
		if err := client.Tracker().Create(resource, winner, namespace); err != nil {
			t.Fatalf("inject create-race winner: %v", err)
		}
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, name)
	})

	store := NewK8sConfigMapStore(client, namespace, name)
	if err := store.Save(context.Background(), &State{SpawnID: "target", Status: StatusRunning}); err != nil {
		t.Fatalf("Save after create race: %v", err)
	}
	cm, err := client.CoreV1().ConfigMaps(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get final ConfigMap: %v", err)
	}
	if creates != 1 || cm.Data["concurrent"] != "race-winner" {
		t.Fatalf("create race lost winner state: creates=%d data=%v", creates, cm.Data)
	}
	if _, ok := cm.Data["target"]; !ok {
		t.Fatalf("target state missing after create race: %v", cm.Data)
	}
}

func TestK8sConfigMapStore_LoadNonexistentCM(t *testing.T) {
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, "devbox", "does-not-exist")
	ctx := context.Background()

	loaded, err := store.Load(ctx, "spawn-nope")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded != nil {
		t.Error("expected nil for nonexistent CM")
	}
}

func TestK8sConfigMapStore_LoadAllNonexistentCM(t *testing.T) {
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, "devbox", "does-not-exist")
	ctx := context.Background()

	states, err := store.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("expected 0, got %d", len(states))
	}
}

func TestK8sConfigMapStore_SaveNil(t *testing.T) {
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, "devbox", "test")
	if err := store.Save(context.Background(), nil); err == nil {
		t.Error("expected error saving nil state")
	}
}

func TestK8sConfigMapStore_SerializedSizeBudgetBoundary(t *testing.T) {
	const namespace, name = "devbox", "size-budget-boundary"
	state := &State{
		SpawnID:       "spawn-size-boundary",
		AgentID:       "spawn-codex-size-boundary",
		DriverOwnerID: "loom-hub/mobile-hud",
		Status:        StatusRunning,
		StartedAt:     time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC),
		Request: Request{
			AgentType:       "codex",
			Project:         "loom-core",
			TaskDescription: "",
			IdempotencyKey:  "wf-size-boundary",
		},
	}
	baseSize := configMapSizeWithStates(t, namespace, name, state)
	fillerBytes := DefaultK8sConfigMapSerializedSizeBudget - baseSize
	if fillerBytes <= 0 {
		t.Fatalf("default budget %d does not fit base ConfigMap size %d", DefaultK8sConfigMapSerializedSizeBudget, baseSize)
	}
	state.Request.TaskDescription = strings.Repeat("x", fillerBytes)
	if got := configMapSizeWithStates(t, namespace, name, state); got != DefaultK8sConfigMapSerializedSizeBudget {
		t.Fatalf("boundary fixture serialized size = %d, want %d", got, DefaultK8sConfigMapSerializedSizeBudget)
	}

	t.Run("exact budget succeeds", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		store := NewK8sConfigMapStore(client, namespace, name)
		if err := store.Save(t.Context(), state); err != nil {
			t.Fatalf("Save at exact serialized budget: %v", err)
		}
		if loaded, err := store.Load(t.Context(), state.SpawnID); err != nil || loaded == nil {
			t.Fatalf("load exact-budget state: loaded=%v err=%v", loaded, err)
		}
	})

	t.Run("one byte below candidate fails before create", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		budget := DefaultK8sConfigMapSerializedSizeBudget - 1
		store := NewK8sConfigMapStore(
			client, namespace, name,
			WithK8sConfigMapSerializedSizeBudget(budget),
		)
		err := store.Save(t.Context(), state)
		if !errors.Is(err, ErrConfigMapSizeBudgetExceeded) {
			t.Fatalf("Save error = %v, want ErrConfigMapSizeBudgetExceeded", err)
		}
		var sizeErr *ConfigMapSizeBudgetError
		if !errors.As(err, &sizeErr) {
			t.Fatalf("Save error type = %T, want *ConfigMapSizeBudgetError", err)
		}
		if sizeErr.Namespace != namespace || sizeErr.Name != name ||
			sizeErr.SerializedBytes != DefaultK8sConfigMapSerializedSizeBudget || sizeErr.BudgetBytes != budget {
			t.Fatalf("size error fields = %+v", sizeErr)
		}
		if !strings.Contains(sizeErr.Error(), "prune retained terminal rows") {
			t.Fatalf("size error is not actionable: %v", sizeErr)
		}
		if _, getErr := client.CoreV1().ConfigMaps(namespace).Get(t.Context(), name, metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
			t.Fatalf("oversized Save created ConfigMap: %v", getErr)
		}
		for _, action := range client.Actions() {
			if action.GetResource().Resource == "configmaps" &&
				(action.GetVerb() == "create" || action.GetVerb() == "update") {
				t.Fatalf("oversized Save reached mutating API action: %#v", action)
			}
		}
	})
}

func TestK8sConfigMapStore_SizeGuardRechecksConflictWinner(t *testing.T) {
	const namespace, name = "devbox", "size-budget-conflict"
	resource := corev1.SchemeGroupVersion.WithResource("configmaps")
	initial := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, ResourceVersion: "1"},
		Data:       map[string]string{"existing": "keep"},
	}
	target := &State{SpawnID: "spawn-size-target", Status: StatusRunning, StartedAt: time.Now().UTC()}
	targetRaw, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	firstCandidate := initial.DeepCopy()
	firstCandidate.Data[target.SpawnID] = string(targetRaw)
	budget := serializedConfigMapSize(t, firstCandidate)

	client := fake.NewSimpleClientset(initial)
	updates := 0
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		tracked, getErr := client.Tracker().Get(resource, namespace, name)
		if getErr != nil {
			t.Fatal(getErr)
		}
		winner := tracked.(*corev1.ConfigMap).DeepCopy()
		winner.ResourceVersion = "2"
		winner.Data["peer"] = strings.Repeat("p", 4096)
		if updateErr := client.Tracker().Update(resource, winner, namespace); updateErr != nil {
			t.Fatal(updateErr)
		}
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Resource: "configmaps"}, name, errors.New("peer enlarged ConfigMap"),
		)
	})

	store := NewK8sConfigMapStore(
		client, namespace, name,
		WithK8sConfigMapSerializedSizeBudget(budget),
	)
	err = store.Save(t.Context(), target)
	if !errors.Is(err, ErrConfigMapSizeBudgetExceeded) {
		t.Fatalf("Save after larger conflict winner = %v, want size budget error", err)
	}
	if updates != 1 {
		t.Fatalf("update calls = %d, want one pre-conflict attempt and no oversized retry write", updates)
	}
	final, getErr := client.CoreV1().ConfigMaps(namespace).Get(t.Context(), name, metav1.GetOptions{})
	if getErr != nil {
		t.Fatal(getErr)
	}
	if final.Data["peer"] == "" || final.Data[target.SpawnID] != "" {
		t.Fatalf("conflict winner was not preserved: %v", final.Data)
	}
}

func TestK8sConfigMapStore_DeletionRecoversAlreadyOversizedConfigMap(t *testing.T) {
	const namespace, name = "devbox", "oversized-recovery"
	startedAt := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	// NON-terminal states on purpose: the Save-path overflow valve prunes
	// terminal rows (TestK8sConfigMapStore_SaveOverflowPrunesTerminalEntries),
	// so an all-running fixture is what still exercises the refusal + the
	// Delete-only recovery contract this test pins.
	states := []*State{
		{
			SpawnID: "spawn-prune-1", DriverOwnerID: "owner-a", Status: StatusRunning, StartedAt: startedAt,
			Request: Request{IdempotencyKey: "key-1", TaskDescription: strings.Repeat("a", 4096)},
		},
		{
			SpawnID: "spawn-prune-2", DriverOwnerID: "owner-a", Status: StatusRunning, StartedAt: startedAt.Add(time.Second),
			Request: Request{IdempotencyKey: "key-2", TaskDescription: strings.Repeat("b", 4096)},
		},
		{
			SpawnID: "spawn-prune-3", DriverOwnerID: "owner-a", Status: StatusRunning, StartedAt: startedAt.Add(2 * time.Second),
			Request: Request{IdempotencyKey: "key-3", TaskDescription: strings.Repeat("c", 4096)},
		},
	}
	cm := configMapWithStates(t, namespace, name, states...)
	cm.ResourceVersion = "1"
	afterFirstDelete := cm.DeepCopy()
	delete(afterFirstDelete.Data, states[0].SpawnID)
	budget := serializedConfigMapSize(t, afterFirstDelete) - 1
	if budget <= 0 || serializedConfigMapSize(t, cm) <= budget || serializedConfigMapSize(t, afterFirstDelete) <= budget {
		t.Fatalf("invalid oversized recovery fixture: current=%d after_delete=%d budget=%d",
			serializedConfigMapSize(t, cm), serializedConfigMapSize(t, afterFirstDelete), budget)
	}

	client := fake.NewSimpleClientset(cm)
	store := NewK8sConfigMapStore(
		client, namespace, name,
		WithK8sConfigMapSerializedSizeBudget(budget),
	)
	if err := store.Save(t.Context(), &State{SpawnID: "spawn-new", Status: StatusPending, StartedAt: time.Now()}); !errors.Is(err, ErrConfigMapSizeBudgetExceeded) {
		t.Fatalf("Save into oversized ConfigMap = %v, want size budget error", err)
	}
	if err := store.Delete(t.Context(), states[0].SpawnID); err != nil {
		t.Fatalf("Delete must shrink an already oversized ConfigMap: %v", err)
	}
	stillOversized, err := client.CoreV1().ConfigMaps(namespace).Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := serializedConfigMapSize(t, stillOversized); got <= budget {
		t.Fatalf("first Delete unexpectedly crossed budget: size=%d budget=%d", got, budget)
	}

	deleted, err := store.DeleteIfMatch(t.Context(), states[1].SpawnID, deleteConditionForState(states[1]))
	if err != nil || !deleted {
		t.Fatalf("conditional prune from oversized ConfigMap = deleted %v, err %v", deleted, err)
	}
	recovered, err := client.CoreV1().ConfigMaps(namespace).Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := serializedConfigMapSize(t, recovered); got > budget {
		t.Fatalf("conditional prune did not recover budget: size=%d budget=%d", got, budget)
	}
	if _, exists := recovered.Data[states[2].SpawnID]; !exists || len(recovered.Data) != 1 {
		t.Fatalf("recovery deleted the wrong entries: %v", recovered.Data)
	}
}

func configMapSizeWithStates(t *testing.T, namespace, name string, states ...*State) int {
	t.Helper()
	return serializedConfigMapSize(t, configMapWithStates(t, namespace, name, states...))
}

func configMapWithStates(t *testing.T, namespace, name string, states ...*State) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{ManagedByLabel: ManagedByValue},
		},
		Data: make(map[string]string, len(states)),
	}
	for _, state := range states {
		raw, err := json.Marshal(state)
		if err != nil {
			t.Fatalf("marshal state %s: %v", state.SpawnID, err)
		}
		cm.Data[state.SpawnID] = string(raw)
	}
	return cm
}

func serializedConfigMapSize(t *testing.T, cm *corev1.ConfigMap) int {
	t.Helper()
	raw, err := json.Marshal(cm)
	if err != nil {
		t.Fatalf("marshal ConfigMap: %v", err)
	}
	return len(raw)
}

// ---------- IsTerminal tests ----------

func TestIsTerminal(t *testing.T) {
	tests := []struct {
		status   Status
		terminal bool
	}{
		{StatusPending, false},
		{StatusBuilding, false},
		{StatusRunning, false},
		{StatusCompleted, true},
		{StatusFailed, true},
		{StatusStopped, true},
		{StatusUnknown, false},
	}
	for _, tt := range tests {
		if got := IsTerminal(tt.status); got != tt.terminal {
			t.Errorf("IsTerminal(%q) = %v, want %v", tt.status, got, tt.terminal)
		}
	}
}

// ---------- Save-path overflow prune (prune-on-write) ----------

// pruneTestState builds a padded state so a handful of entries meaningfully
// move the serialized ConfigMap size in the overflow tests below.
func pruneTestState(id string, status Status, endedAgo time.Duration) *State {
	st := &State{
		SpawnID:   id,
		AgentID:   "agent-" + id,
		Status:    status,
		StartedAt: time.Now().Add(-2 * time.Hour),
		Request: Request{
			AgentType:       "codex",
			Project:         "loom-core",
			TaskDescription: strings.Repeat("x", 400),
		},
	}
	if IsTerminal(status) {
		at := time.Now().Add(-endedAgo)
		st.EndedAt = &at
	}
	return st
}

// TestK8sConfigMapStore_SaveOverflowPrunesTerminalEntries pins the
// prune-on-write valve: a Save that trips the size budget must make room by
// pruning terminal entries oldest-first (never a non-terminal entry) and then
// commit, instead of refusing the write. Before this, the refusal surfaced as
// "persist owned spawn before dispatch" 400s on every dispatch between prune
// loop ticks — 18 of 73 failed stage-attempts on 2026-07-26.
func TestK8sConfigMapStore_SaveOverflowPrunesTerminalEntries(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	seeder := NewK8sConfigMapStore(client, "devbox", "test-spawn-state")
	for _, st := range []*State{
		pruneTestState("spawn-term-oldest", StatusCompleted, 30*time.Minute),
		pruneTestState("spawn-term-mid", StatusFailed, 20*time.Minute),
		pruneTestState("spawn-term-newest", StatusCompleted, 10*time.Minute),
		pruneTestState("spawn-live", StatusRunning, 0),
	} {
		if err := seeder.Save(ctx, st); err != nil {
			t.Fatalf("seed %s: %v", st.SpawnID, err)
		}
	}
	seeded, err := client.CoreV1().ConfigMaps("devbox").Get(ctx, "test-spawn-state", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get seeded cm: %v", err)
	}
	raw, err := json.Marshal(seeded)
	if err != nil {
		t.Fatalf("marshal seeded cm: %v", err)
	}
	// Budget admits the seeded object but NOT one more padded entry.
	budget := len(raw) + 100
	store := NewK8sConfigMapStore(client, "devbox", "test-spawn-state",
		WithK8sConfigMapSerializedSizeBudget(budget))

	if err := store.Save(ctx, pruneTestState("spawn-incoming", StatusPending, 0)); err != nil {
		t.Fatalf("Save under pressure must prune terminal rows and succeed, got: %v", err)
	}

	after, err := client.CoreV1().ConfigMaps("devbox").Get(ctx, "test-spawn-state", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cm after pressured save: %v", err)
	}
	if _, ok := after.Data["spawn-incoming"]; !ok {
		t.Error("incoming entry missing after pressured save")
	}
	if _, ok := after.Data["spawn-live"]; !ok {
		t.Error("non-terminal entry must never be pruned")
	}
	if _, ok := after.Data["spawn-term-oldest"]; ok {
		t.Error("oldest terminal entry should have been pruned first")
	}
	afterRaw, err := json.Marshal(after)
	if err != nil {
		t.Fatalf("marshal cm after pressured save: %v", err)
	}
	if len(afterRaw) > budget {
		t.Errorf("cm serializes to %d bytes, above the %d-byte budget", len(afterRaw), budget)
	}
}

// TestK8sConfigMapStore_SaveOverflowWithoutTerminalRowsStillRefuses: when every
// retained entry is non-terminal there is nothing safe to prune, so the size
// guard's refusal (and its actionable error) must be preserved byte-for-byte.
func TestK8sConfigMapStore_SaveOverflowWithoutTerminalRowsStillRefuses(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	seeder := NewK8sConfigMapStore(client, "devbox", "test-spawn-state")
	for _, st := range []*State{
		pruneTestState("spawn-live-1", StatusRunning, 0),
		pruneTestState("spawn-live-2", StatusRunning, 0),
	} {
		if err := seeder.Save(ctx, st); err != nil {
			t.Fatalf("seed %s: %v", st.SpawnID, err)
		}
	}
	seeded, err := client.CoreV1().ConfigMaps("devbox").Get(ctx, "test-spawn-state", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get seeded cm: %v", err)
	}
	raw, err := json.Marshal(seeded)
	if err != nil {
		t.Fatalf("marshal seeded cm: %v", err)
	}
	store := NewK8sConfigMapStore(client, "devbox", "test-spawn-state",
		WithK8sConfigMapSerializedSizeBudget(len(raw)+100))

	err = store.Save(ctx, pruneTestState("spawn-live-3", StatusRunning, 0))
	if err == nil {
		t.Fatal("Save must still refuse when no terminal rows can be pruned")
	}
	if !errors.Is(err, ErrConfigMapSizeBudgetExceeded) {
		t.Errorf("error must unwrap to ErrConfigMapSizeBudgetExceeded, got: %v", err)
	}
	after, err := client.CoreV1().ConfigMaps("devbox").Get(ctx, "test-spawn-state", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cm after refused save: %v", err)
	}
	if len(after.Data) != 2 {
		t.Errorf("refused save must not lose entries: got %d, want 2", len(after.Data))
	}
}

// TestK8sConfigMapStore_SaveOverflowNeverEvictsItsOwnWrite: a terminal state
// write that itself trips the budget must not prune the very entry being
// saved to "make room" — the touched entry is excluded from candidates, so
// the write refuses instead of silently discarding the final state.
func TestK8sConfigMapStore_SaveOverflowNeverEvictsItsOwnWrite(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	seeder := NewK8sConfigMapStore(client, "devbox", "test-spawn-state")
	if err := seeder.Save(ctx, pruneTestState("spawn-live", StatusRunning, 0)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seeded, err := client.CoreV1().ConfigMaps("devbox").Get(ctx, "test-spawn-state", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get seeded cm: %v", err)
	}
	raw, err := json.Marshal(seeded)
	if err != nil {
		t.Fatalf("marshal seeded cm: %v", err)
	}
	store := NewK8sConfigMapStore(client, "devbox", "test-spawn-state",
		WithK8sConfigMapSerializedSizeBudget(len(raw)+100))

	err = store.Save(ctx, pruneTestState("spawn-done", StatusCompleted, time.Minute))
	if err == nil {
		t.Fatal("Save must refuse rather than evict the terminal entry it is writing")
	}
	if !errors.Is(err, ErrConfigMapSizeBudgetExceeded) {
		t.Errorf("error must unwrap to ErrConfigMapSizeBudgetExceeded, got: %v", err)
	}
	after, err := client.CoreV1().ConfigMaps("devbox").Get(ctx, "test-spawn-state", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cm after refused save: %v", err)
	}
	if _, ok := after.Data["spawn-live"]; !ok {
		t.Error("running entry must survive the refused terminal save")
	}
	if _, ok := after.Data["spawn-done"]; ok {
		t.Error("refused terminal save must not be committed")
	}
}
