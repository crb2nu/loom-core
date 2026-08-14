package spawn

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func replaceConfigMapSpawnState(
	t *testing.T,
	client *fake.Clientset,
	namespace, name string,
	state *State,
) {
	t.Helper()
	cm, err := client.CoreV1().ConfigMaps(namespace).Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	cm.Data[state.SpawnID] = string(raw)
	if _, err := client.CoreV1().ConfigMaps(namespace).Update(t.Context(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

type toggleFailStore struct {
	fail     bool
	err      error
	delegate Store
}

func (s *toggleFailStore) Save(ctx context.Context, state *State) error {
	if s.fail {
		return s.err
	}
	if s.delegate != nil {
		return s.delegate.Save(ctx, state)
	}
	return nil
}
func (s *toggleFailStore) Load(ctx context.Context, id string) (*State, error) {
	if s.delegate != nil {
		return s.delegate.Load(ctx, id)
	}
	return nil, nil
}
func (s *toggleFailStore) LoadAll(ctx context.Context) ([]*State, error) {
	if s.delegate != nil {
		return s.delegate.LoadAll(ctx)
	}
	return nil, nil
}
func (s *toggleFailStore) Delete(ctx context.Context, id string) error {
	if s.delegate != nil {
		return s.delegate.Delete(ctx, id)
	}
	return nil
}

type loadErrorStore struct {
	err       error
	saveCalls int
}

func (s *loadErrorStore) Save(context.Context, *State) error {
	s.saveCalls++
	return nil
}
func (s *loadErrorStore) Load(context.Context, string) (*State, error) {
	return nil, s.err
}
func (s *loadErrorStore) LoadAll(context.Context) ([]*State, error) { return nil, s.err }
func (s *loadErrorStore) Delete(context.Context, string) error      { return nil }

type discoveryRaceStore struct {
	delegate Store
	winner   *State
	once     sync.Once
}

func (s *discoveryRaceStore) Save(ctx context.Context, state *State) error {
	var injectErr error
	s.once.Do(func() {
		injectErr = s.delegate.Save(ctx, cloneStateForRead(s.winner))
	})
	if injectErr != nil {
		return injectErr
	}
	return s.delegate.Save(ctx, state)
}

func (s *discoveryRaceStore) Load(ctx context.Context, id string) (*State, error) {
	return s.delegate.Load(ctx, id)
}

func (s *discoveryRaceStore) LoadAll(ctx context.Context) ([]*State, error) {
	return s.delegate.LoadAll(ctx)
}

func (s *discoveryRaceStore) Delete(ctx context.Context, id string) error {
	return s.delegate.Delete(ctx, id)
}

type blockingSaveStore struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once

	mu        sync.Mutex
	saveCalls int
	states    map[string]*State
}

type selectiveBlockingStore struct {
	delegate  Store
	blockedID string
	started   chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (s *selectiveBlockingStore) Save(ctx context.Context, state *State) error {
	if state.SpawnID == s.blockedID {
		s.once.Do(func() { close(s.started) })
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.release:
		}
	}
	return s.delegate.Save(ctx, state)
}

func (s *selectiveBlockingStore) Load(ctx context.Context, id string) (*State, error) {
	return s.delegate.Load(ctx, id)
}

func (s *selectiveBlockingStore) LoadAll(ctx context.Context) ([]*State, error) {
	return s.delegate.LoadAll(ctx)
}

func (s *selectiveBlockingStore) Delete(ctx context.Context, id string) error {
	return s.delegate.Delete(ctx, id)
}

func newBlockingSaveStore() *blockingSaveStore {
	return &blockingSaveStore{
		started: make(chan struct{}),
		release: make(chan struct{}),
		states:  make(map[string]*State),
	}
}

func (s *blockingSaveStore) Save(ctx context.Context, state *State) error {
	s.mu.Lock()
	s.saveCalls++
	s.mu.Unlock()
	s.once.Do(func() { close(s.started) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
	}
	copy := *state
	s.mu.Lock()
	s.states[state.SpawnID] = &copy
	s.mu.Unlock()
	return nil
}

func (s *blockingSaveStore) Load(_ context.Context, id string) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.states[id]
	if state == nil {
		return nil, nil
	}
	copy := *state
	return &copy, nil
}

func (s *blockingSaveStore) LoadAll(context.Context) ([]*State, error) { return nil, nil }
func (s *blockingSaveStore) Delete(context.Context, string) error      { return nil }

func (s *blockingSaveStore) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveCalls
}

func TestSpawnWithKeyPersistenceFailureRollsBackProvisionalEntry(t *testing.T) {
	storeErr := errors.New("injected keyed persistence failure")
	store := &toggleFailStore{fail: true, err: storeErr}
	ctrl := NewK8sController(fake.NewSimpleClientset(), "devbox", store, nil)
	ctrl.spawns["unrelated"] = &State{SpawnID: "unrelated", Status: StatusRunning}
	req := Request{
		AgentType: "codex", TaskDescription: "persist before dispatch", Project: "loom-core",
		IdempotencyKey: "mills/run-1/agent-0",
	}

	spawnID, err := ctrl.Spawn(t.Context(), req)
	if err == nil || !errors.Is(err, storeErr) {
		t.Fatalf("Spawn() = id %q error %v, want persistence error", spawnID, err)
	}
	derivedID := DeriveSpawnID(req.IdempotencyKey)
	if _, ok := ctrl.Get(derivedID); ok {
		t.Fatalf("failed keyed Save left provisional entry %s", derivedID)
	}
	if state, ok := ctrl.Get("unrelated"); !ok || state.SpawnID != "unrelated" {
		t.Fatal("failed keyed Save removed an unrelated entry")
	}
}

func TestSpawnWithKeyBlockedStoreDoesNotBlockIndependentKey(t *testing.T) {
	delegate, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := Request{
		AgentType: "codex", Project: "loom-core", TaskDescription: "blocked key",
		IdempotencyKey: "mills/scale/blocked",
	}
	firstID := DeriveSpawnID(first.IdempotencyKey)
	second := Request{
		AgentType: "codex", Project: "loom-core", TaskDescription: "independent key",
	}
	for i := 0; ; i++ {
		second.IdempotencyKey = fmt.Sprintf("mills/scale/independent-%d", i)
		secondID := DeriveSpawnID(second.IdempotencyKey)
		if sha256.Sum256([]byte(firstID))[0] != sha256.Sum256([]byte(secondID))[0] {
			break
		}
	}
	store := &selectiveBlockingStore{
		delegate: delegate, blockedID: firstID,
		started: make(chan struct{}), release: make(chan struct{}),
	}
	ctrl := NewK8sController(nil, "devbox", store, nil)
	type result struct {
		id       string
		dispatch bool
		err      error
	}
	firstResult := make(chan result, 1)
	go func() {
		id, dispatch, err := ctrl.Register(t.Context(), first)
		firstResult <- result{id: id, dispatch: dispatch, err: err}
	}()
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("first registration did not reach blocked durable Save")
	}

	secondResult := make(chan result, 1)
	go func() {
		id, dispatch, err := ctrl.Register(t.Context(), second)
		secondResult <- result{id: id, dispatch: dispatch, err: err}
	}()
	select {
	case got := <-secondResult:
		if got.err != nil || !got.dispatch || got.id != DeriveSpawnID(second.IdempotencyKey) {
			t.Fatalf("independent registration = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked key head-of-line blocked independent registration")
	}

	close(store.release)
	select {
	case got := <-firstResult:
		if got.err != nil || !got.dispatch || got.id != firstID {
			t.Fatalf("blocked registration after release = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked registration did not complete after release")
	}
}

func TestSpawnWithKeyRequiresPersistentStore(t *testing.T) {
	ctrl := NewK8sController(fake.NewSimpleClientset(), "devbox", nil, nil)
	req := Request{
		AgentType: "codex", TaskDescription: "persist before dispatch", Project: "loom-core",
		IdempotencyKey: "mills/run-no-store/agent-0",
	}

	spawnID, err := ctrl.Spawn(t.Context(), req)
	if err == nil || !strings.Contains(err.Error(), "persistent store") {
		t.Fatalf("keyed Spawn without store = id %q error %v, want fail-closed error", spawnID, err)
	}
	if _, ok := ctrl.Get(DeriveSpawnID(req.IdempotencyKey)); ok {
		t.Fatal("keyed Spawn without store created an in-memory entry")
	}
}

func TestSpawnWithKeyConcurrentCallerWaitsForDurability(t *testing.T) {
	store := newBlockingSaveStore()
	ctrl := NewK8sController(fake.NewSimpleClientset(), "devbox", store, nil)
	req := Request{
		AgentType: "codex", TaskDescription: "persist before dispatch", Project: "loom-core",
		IdempotencyKey: "mills/run-2/agent-0",
	}
	type result struct {
		id  string
		err error
	}
	first := make(chan result, 1)
	second := make(chan result, 1)
	secondStarted := make(chan struct{})
	go func() {
		id, err := ctrl.Spawn(t.Context(), req)
		first <- result{id: id, err: err}
	}()
	<-store.started
	go func() {
		close(secondStarted)
		id, err := ctrl.Spawn(t.Context(), req)
		second <- result{id: id, err: err}
	}()
	<-secondStarted

	var earlySecond *result
	select {
	case got := <-second:
		t.Errorf("same-key caller returned before durability: %+v", got)
		earlySecond = &got
	case <-time.After(50 * time.Millisecond):
	}
	close(store.release)

	firstResult := <-first
	var secondResult result
	if earlySecond != nil {
		secondResult = *earlySecond
	} else {
		secondResult = <-second
	}
	if firstResult.err != nil || secondResult.err != nil {
		t.Fatalf("Spawn errors after durable Save: first=%v second=%v", firstResult.err, secondResult.err)
	}
	wantID := DeriveSpawnID(req.IdempotencyKey)
	if firstResult.id != wantID || secondResult.id != wantID {
		t.Fatalf("Spawn ids = %q, %q; want %q", firstResult.id, secondResult.id, wantID)
	}
	if durable, err := store.Load(t.Context(), wantID); err != nil || durable == nil {
		t.Fatalf("same-key caller returned without durable entry: state=%+v err=%v", durable, err)
	}
	if calls := store.calls(); calls != 1 {
		t.Fatalf("Store.Save calls = %d, want 1", calls)
	}
}

func TestSpawn(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewK8sController(client, "devbox", nil, nil)

	ctx := context.Background()
	id, err := ctrl.Spawn(ctx, Request{
		AgentType:       "claude-code",
		TaskDescription: "fix the bug",
		Project:         "loom-core",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty spawn ID")
	}

	state, ok := ctrl.Get(id)
	if !ok {
		t.Fatalf("spawn %s not found after Spawn()", id)
	}
	if state.Status != StatusPending {
		t.Errorf("status: got %q, want %q", state.Status, StatusPending)
	}
	if state.AgentID == "" {
		t.Error("expected non-empty agent ID")
	}
	if state.Request.AgentType != "claude-code" {
		t.Errorf("agent type: got %q, want %q", state.Request.AgentType, "claude-code")
	}
}

func seedStoppingController(t *testing.T) (*K8sController, *toggleFailStore, string) {
	t.Helper()
	store := &toggleFailStore{err: errors.New("injected store failure")}
	ctrl := NewK8sController(fake.NewSimpleClientset(), "devbox", store, nil)
	spawnID, err := ctrl.Spawn(t.Context(), Request{
		AgentType: "codex", TaskDescription: "test stop persistence", Project: "loom-core",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	return ctrl, store, spawnID
}

func TestStopCASPersistenceFailuresRollback(t *testing.T) {
	t.Run("begin stop", func(t *testing.T) {
		ctrl, store, spawnID := seedStoppingController(t)
		store.fail = true
		if _, _, err := ctrl.BeginStop(t.Context(), spawnID); err == nil {
			t.Fatal("BeginStop accepted an unpersisted intent")
		}
		state, _ := ctrl.Get(spawnID)
		if state.StopRequestedAt != nil {
			t.Fatal("BeginStop did not roll back StopRequestedAt")
		}
	})

	t.Run("late pod handle", func(t *testing.T) {
		ctrl, store, spawnID := seedStoppingController(t)
		if _, _, err := ctrl.BeginStop(t.Context(), spawnID); err != nil {
			t.Fatalf("BeginStop: %v", err)
		}
		store.fail = true
		if _, _, err := ctrl.RecordStoppingPod(t.Context(), spawnID, "late-container"); err == nil {
			t.Fatal("RecordStoppingPod accepted an unpersisted handle")
		}
		state, _ := ctrl.Get(spawnID)
		if state.PodName != "" {
			t.Fatalf("PodName = %q after rollback", state.PodName)
		}
	})

	t.Run("cleanup failure", func(t *testing.T) {
		ctrl, store, spawnID := seedStoppingController(t)
		if _, _, err := ctrl.BeginStop(t.Context(), spawnID); err != nil {
			t.Fatalf("BeginStop: %v", err)
		}
		store.fail = true
		if _, _, err := ctrl.RecordStopCleanupFailure(t.Context(), spawnID, "late-container", "delete failed"); err == nil {
			t.Fatal("RecordStopCleanupFailure accepted an unpersisted failure")
		}
		state, _ := ctrl.Get(spawnID)
		if state.PodName != "" || state.Error != "" {
			t.Fatalf("cleanup failure rollback = pod %q error %q", state.PodName, state.Error)
		}
	})

	t.Run("complete stop", func(t *testing.T) {
		ctrl, store, spawnID := seedStoppingController(t)
		if _, _, err := ctrl.BeginStop(t.Context(), spawnID); err != nil {
			t.Fatalf("BeginStop: %v", err)
		}
		store.fail = true
		if _, _, err := ctrl.CompleteStop(t.Context(), spawnID); err == nil {
			t.Fatal("CompleteStop accepted an unpersisted terminal state")
		}
		state, _ := ctrl.Get(spawnID)
		if state.Status == StatusStopped || state.EndedAt != nil {
			t.Fatalf("CompleteStop rollback = status %s ended_at %v", state.Status, state.EndedAt)
		}
	})
}

func TestSpawnValidation(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewK8sController(client, "devbox", nil, nil)
	ctx := context.Background()

	tests := []struct {
		name string
		req  Request
	}{
		{
			name: "unsupported agent type",
			req:  Request{AgentType: "gpt-5", TaskDescription: "x", Project: "p"},
		},
		{
			name: "missing task",
			req:  Request{AgentType: "claude-code", Project: "p"},
		},
		{
			name: "missing project",
			req:  Request{AgentType: "claude-code", TaskDescription: "x"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ctrl.Spawn(ctx, tt.req)
			if err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestSpawnDefaultAgentType(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewK8sController(client, "devbox", nil, nil)

	id, err := ctrl.Spawn(context.Background(), Request{
		TaskDescription: "test task",
		Project:         "test-project",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	state, ok := ctrl.Get(id)
	if !ok {
		t.Fatal("spawn not found")
	}
	if state.Request.AgentType != "claude-code" {
		t.Errorf("default agent type: got %q, want %q", state.Request.AgentType, "claude-code")
	}
}

func TestStop(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewK8sController(client, "devbox", nil, nil)
	ctx := context.Background()

	id, err := ctrl.Spawn(ctx, Request{
		AgentType:       "claude-code",
		TaskDescription: "test",
		Project:         "proj",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if err := ctrl.Stop(ctx, id); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	state, ok := ctrl.Get(id)
	if !ok {
		t.Fatal("spawn not found after stop")
	}
	if state.Status != StatusStopped {
		t.Errorf("status: got %q, want %q", state.Status, StatusStopped)
	}
	if state.EndedAt == nil {
		t.Error("expected EndedAt to be set")
	}
}

func TestStopNotFound(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewK8sController(client, "devbox", nil, nil)

	err := ctrl.Stop(context.Background(), "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent spawn")
	}
}

func TestList(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewK8sController(client, "devbox", nil, nil)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, err := ctrl.Spawn(ctx, Request{
			AgentType:       "claude-code",
			TaskDescription: "task",
			Project:         "proj",
		})
		if err != nil {
			t.Fatalf("Spawn %d: %v", i, err)
		}
	}

	list := ctrl.List()
	if len(list) != 3 {
		t.Errorf("List: got %d, want 3", len(list))
	}
}

func TestActiveCount(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewK8sController(client, "devbox", nil, nil)
	ctx := context.Background()

	id1, _ := ctrl.Spawn(ctx, Request{AgentType: "claude-code", TaskDescription: "t", Project: "p"})
	_, _ = ctrl.Spawn(ctx, Request{AgentType: "claude-code", TaskDescription: "t", Project: "p"})

	if got := ctrl.ActiveCount(); got != 2 {
		t.Errorf("ActiveCount: got %d, want 2", got)
	}

	_ = ctrl.Stop(ctx, id1)

	if got := ctrl.ActiveCount(); got != 1 {
		t.Errorf("ActiveCount after stop: got %d, want 1", got)
	}
}

func TestUpdateState(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewK8sController(client, "devbox", nil, nil)
	ctx := context.Background()

	id, _ := ctrl.Spawn(ctx, Request{AgentType: "claude-code", TaskDescription: "t", Project: "p"})
	state, _ := ctrl.Get(id)
	state.Status = StatusRunning
	state.PodName = "spawn-pod-123"
	ctrl.UpdateState(ctx, state)

	updated, ok := ctrl.Get(id)
	if !ok {
		t.Fatal("spawn not found after update")
	}
	if updated.Status != StatusRunning {
		t.Errorf("status: got %q, want %q", updated.Status, StatusRunning)
	}
	if updated.PodName != "spawn-pod-123" {
		t.Errorf("pod name: got %q, want %q", updated.PodName, "spawn-pod-123")
	}
}

func TestSpawnWithStore(t *testing.T) {
	client := fake.NewSimpleClientset()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctrl := NewK8sController(client, "devbox", store, nil)
	ctx := context.Background()

	id, err := ctrl.Spawn(ctx, Request{
		AgentType:       "claude-code",
		TaskDescription: "test",
		Project:         "proj",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Verify persisted.
	loaded, err := store.Load(ctx, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected persisted state")
	}
	if loaded.SpawnID != id {
		t.Errorf("persisted SpawnID: got %q, want %q", loaded.SpawnID, id)
	}
}

func makePod(name, spawnID, agentID string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "devbox",
			Labels: map[string]string{
				ManagedByLabel: ManagedByValue,
				SpawnIDLabel:   spawnID,
				AgentIDLabel:   agentID,
			},
		},
		Status: corev1.PodStatus{
			Phase: phase,
		},
	}
}

func TestReconcileUpdatesStatus(t *testing.T) {
	pod := makePod("spawn-pod-1", "spawn-abc123", "agent-1", corev1.PodRunning)
	client := fake.NewSimpleClientset([]runtime.Object{pod}...)
	ctrl := NewK8sController(client, "devbox", nil, nil)
	ctx := context.Background()

	// Pre-populate with a pending state.
	ctrl.mu.Lock()
	ctrl.spawns["spawn-abc123"] = &State{
		SpawnID: "spawn-abc123",
		AgentID: "agent-1",
		Status:  StatusPending,
	}
	ctrl.mu.Unlock()

	if err := ctrl.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	state, ok := ctrl.Get("spawn-abc123")
	if !ok {
		t.Fatal("spawn not found after reconcile")
	}
	if state.Status != StatusRunning {
		t.Errorf("status: got %q, want %q", state.Status, StatusRunning)
	}
	if state.PodName != "spawn-pod-1" {
		t.Errorf("pod name: got %q, want %q", state.PodName, "spawn-pod-1")
	}
}

func TestReconcileMarksMissingPodAsFailed(t *testing.T) {
	// No pods in the cluster.
	client := fake.NewSimpleClientset()
	ctrl := NewK8sController(client, "devbox", nil, nil)
	ctx := context.Background()

	ctrl.mu.Lock()
	ctrl.spawns["spawn-gone"] = &State{
		SpawnID: "spawn-gone",
		Status:  StatusRunning,
		PodName: "spawn-pod-gone",
	}
	ctrl.mu.Unlock()

	if err := ctrl.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	state, ok := ctrl.Get("spawn-gone")
	if !ok {
		t.Fatal("spawn not found")
	}
	if state.Status != StatusFailed {
		t.Errorf("status: got %q, want %q", state.Status, StatusFailed)
	}
	if state.Error == "" {
		t.Error("expected error message for missing pod")
	}
	if state.EndedAt == nil {
		t.Error("expected EndedAt to be set")
	}
}

func TestReconcileKeepsPreRuntimeSpawnWithoutPod(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewK8sController(client, "devbox", nil, nil)
	ctx := context.Background()

	ctrl.mu.Lock()
	ctrl.spawns["spawn-building"] = &State{
		SpawnID: "spawn-building",
		Status:  StatusBuilding,
	}
	ctrl.mu.Unlock()

	if err := ctrl.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	state, ok := ctrl.Get("spawn-building")
	if !ok {
		t.Fatal("spawn not found")
	}
	if state.Status != StatusBuilding {
		t.Errorf("status: got %q, want %q", state.Status, StatusBuilding)
	}
	if state.Error != "" {
		t.Errorf("error: got %q, want empty", state.Error)
	}
	if state.EndedAt != nil {
		t.Error("expected EndedAt to remain nil")
	}
}

func TestReconcileDiscoversUntrackedPods(t *testing.T) {
	pod := makePod("spawn-pod-new", "spawn-new123", "agent-new", corev1.PodRunning)
	pod.Labels["loom.dev/agent-type"] = "codex"
	pod.Labels["loom.dev/project"] = "test-proj"
	client := fake.NewSimpleClientset([]runtime.Object{pod}...)
	ctrl := NewK8sController(client, "devbox", nil, nil)

	if err := ctrl.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	state, ok := ctrl.Get("spawn-new123")
	if !ok {
		t.Fatal("untracked pod not discovered")
	}
	if state.Status != StatusRunning {
		t.Errorf("status: got %q, want %q", state.Status, StatusRunning)
	}
	if state.AgentID != "agent-new" {
		t.Errorf("agent ID: got %q, want %q", state.AgentID, "agent-new")
	}
	if state.Request.AgentType != "codex" {
		t.Errorf("agent type: got %q, want %q", state.Request.AgentType, "codex")
	}
	if state.Request.Project != "test-proj" {
		t.Errorf("project: got %q, want %q", state.Request.Project, "test-proj")
	}
}

func TestReconcileDoesNotClaimDurablyTrackedPeerSpawn(t *testing.T) {
	ctx := t.Context()
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, "devbox", "loom-spawn-state")
	owner := NewK8sController(client, "devbox", store, nil)

	req := Request{
		AgentType:       "codex",
		Namespace:       "mills/canary",
		Branch:          "mills-wf/run-1",
		BaseBranch:      "main",
		TaskDescription: "run the keyed crash canary",
		Project:         "loom-core",
		TimeoutMinutes:  30,
		IdempotencyKey:  "wf-peer-owned-step",
	}
	spawnID, err := owner.Spawn(ctx, req)
	if err != nil {
		t.Fatalf("owner Spawn: %v", err)
	}
	pod := makePod("spawn-"+spawnID, spawnID, "agent-owner", corev1.PodRunning)
	if _, err := client.CoreV1().Pods("devbox").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create peer pod: %v", err)
	}
	if _, updated, err := owner.UpdateUnlessStoppingOrTerminal(ctx, spawnID, func(state *State) {
		state.PodName = pod.Name
		state.Status = StatusRunning
	}); err != nil || !updated {
		t.Fatalf("owner failed to advance spawn to running: updated=%v error=%v", updated, err)
	}

	before, err := client.CoreV1().ConfigMaps("devbox").Get(ctx, "loom-spawn-state", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get durable peer state before reconcile: %v", err)
	}
	beforeRaw := before.Data[spawnID]

	observer := NewK8sController(client, "devbox", store, nil)
	cleanupCalls := 0
	observer.SetTerminalHook(func(context.Context, State) error {
		cleanupCalls++
		return nil
	})
	if err := observer.Reconcile(ctx); err != nil {
		t.Fatalf("observer Reconcile: %v", err)
	}
	if _, claimed := observer.Get(spawnID); claimed {
		t.Fatal("observer claimed a spawn that already had a durable peer-owned record")
	}
	after, err := client.CoreV1().ConfigMaps("devbox").Get(ctx, "loom-spawn-state", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get durable peer state after reconcile: %v", err)
	}
	if got := after.Data[spawnID]; got != beforeRaw {
		t.Fatalf("observer rewrote peer state\nbefore: %s\nafter:  %s", beforeRaw, got)
	}

	endedAt := time.Now()
	if _, updated, err := owner.UpdateUnlessStoppingOrTerminal(ctx, spawnID, func(state *State) {
		state.Status = StatusCompleted
		state.EndedAt = &endedAt
	}); err != nil || !updated {
		t.Fatalf("owner failed to advance spawn to completed: updated=%v error=%v", updated, err)
	}
	if err := client.CoreV1().Pods("devbox").Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete completed peer pod: %v", err)
	}
	terminal, err := client.CoreV1().ConfigMaps("devbox").Get(ctx, "loom-spawn-state", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get terminal peer state: %v", err)
	}
	terminalRaw := terminal.Data[spawnID]

	if err := observer.Reconcile(ctx); err != nil {
		t.Fatalf("observer Reconcile after peer completion: %v", err)
	}
	if cleanupCalls != 0 {
		t.Fatalf("observer cleanup calls = %d, want 0 for peer-owned spawn", cleanupCalls)
	}
	final, err := client.CoreV1().ConfigMaps("devbox").Get(ctx, "loom-spawn-state", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get final peer state: %v", err)
	}
	if got := final.Data[spawnID]; got != terminalRaw {
		t.Fatalf("observer rewrote terminal peer state\nbefore: %s\nafter:  %s", terminalRaw, got)
	}
}

func TestReconcileChecksPeerOwnershipWithOneDurableSnapshot(t *testing.T) {
	ctx := t.Context()
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, "devbox", "loom-spawn-state")
	for i := 1; i <= 3; i++ {
		spawnID := fmt.Sprintf("spawn-peer-%d", i)
		state := &State{
			SpawnID: spawnID, AgentID: fmt.Sprintf("agent-peer-%d", i), Status: StatusRunning, StartedAt: time.Now(),
			Request: Request{
				AgentType: "codex", Project: "loom-core", TaskDescription: "peer-owned",
				IdempotencyKey: fmt.Sprintf("wf-peer-%d", i),
			},
		}
		if err := store.Save(ctx, state); err != nil {
			t.Fatalf("save peer %d: %v", i, err)
		}
		pod := makePod("pod-"+spawnID, spawnID, state.AgentID, corev1.PodRunning)
		if _, err := client.CoreV1().Pods("devbox").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create peer pod %d: %v", i, err)
		}
	}
	client.ClearActions()
	observer := NewK8sController(client, "devbox", store, nil)
	if err := observer.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := len(observer.List()); got != 0 {
		t.Fatalf("observer claimed %d peer spawns", got)
	}
	configMapGets := 0
	configMapMutations := 0
	for _, action := range client.Actions() {
		if action.GetResource().Resource != "configmaps" {
			continue
		}
		if action.GetVerb() == "get" {
			configMapGets++
		} else {
			configMapMutations++
		}
	}
	if configMapGets != 1 || configMapMutations != 0 {
		t.Fatalf("ConfigMap actions = gets %d mutations %d, want one snapshot read and no writes", configMapGets, configMapMutations)
	}
	client.ClearActions()
	if err := observer.Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	for _, action := range client.Actions() {
		if action.GetResource().Resource == "configmaps" {
			t.Fatalf("cached peer ownership performed another ConfigMap action: %#v", action)
		}
	}
}

func TestReconcileStoreLoadErrorFailsClosed(t *testing.T) {
	pod := makePod("spawn-pod-load-error", "spawn-load-error", "agent-load-error", corev1.PodRunning)
	store := &loadErrorStore{err: errors.New("injected durable-store read failure")}
	ctrl := NewK8sController(fake.NewSimpleClientset([]runtime.Object{pod}...), "devbox", store, nil)

	err := ctrl.Reconcile(t.Context())
	if !errors.Is(err, store.err) {
		t.Fatalf("Reconcile error = %v, want %v", err, store.err)
	}
	if _, claimed := ctrl.Get("spawn-load-error"); claimed {
		t.Fatal("controller claimed a pod while durable ownership was unknown")
	}
	if store.saveCalls != 0 {
		t.Fatalf("Save calls = %d, want 0 after durable-store read failure", store.saveCalls)
	}
}

func TestReconcileDiscoversDurablyUntrackedOrphan(t *testing.T) {
	ctx := t.Context()
	pod := makePod("spawn-pod-orphan", "spawn-orphan", "agent-orphan", corev1.PodRunning)
	createdAt := time.Now().Add(-time.Minute).Truncate(time.Second)
	pod.CreationTimestamp = metav1.NewTime(createdAt)
	pod.Labels["loom.dev/agent-type"] = "codex"
	pod.Labels["loom.dev/project"] = "loom-core"
	client := fake.NewSimpleClientset([]runtime.Object{pod}...)
	store := NewK8sConfigMapStore(client, "devbox", "loom-spawn-state")
	ctrl := NewK8sController(client, "devbox", store, nil)

	if err := ctrl.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	state, tracked := ctrl.Get("spawn-orphan")
	if !tracked {
		t.Fatal("true orphan pod was not discovered")
	}
	if state.Request.AgentType != "codex" || state.Request.Project != "loom-core" {
		t.Fatalf("recovered request = %#v", state.Request)
	}
	if state.Request.IdempotencyKey != "" || state.Request.TaskDescription != "" {
		t.Fatalf("orphan discovery invented unavailable request identity: %#v", state.Request)
	}
	if !state.StartedAt.Equal(createdAt) {
		t.Fatalf("orphan StartedAt = %s, want pod creation time %s", state.StartedAt, createdAt)
	}
	durable, err := store.Load(ctx, "spawn-orphan")
	if err != nil {
		t.Fatalf("load recovered orphan: %v", err)
	}
	if durable == nil || durable.SpawnID != "spawn-orphan" {
		t.Fatalf("durable orphan = %#v", durable)
	}
}

func TestRecoverFromStoreFiltersDurableDriverOwner(t *testing.T) {
	ctx := t.Context()
	store := NewK8sConfigMapStore(fake.NewSimpleClientset(), "devbox", "loom-spawn-state")
	states := []*State{
		{
			SpawnID: "spawn-cluster-owned", DriverOwnerID: "loom-hub/mobile-hud", Status: StatusRunning, StartedAt: time.Now(),
			Request: Request{AgentType: "codex", Project: "loom-core", TaskDescription: "cluster", IdempotencyKey: "wf-cluster-owned"},
		},
		{
			SpawnID: "spawn-desktop-owned", DriverOwnerID: "local/desktop", Status: StatusRunning, StartedAt: time.Now(),
			Request: Request{AgentType: "codex", Project: "loom-core", TaskDescription: "desktop", IdempotencyKey: "wf-desktop-owned"},
		},
		{
			SpawnID: "spawn-ownerless", Status: StatusRunning, StartedAt: time.Now(),
			Request: Request{AgentType: "codex", Project: "loom-core", TaskDescription: "legacy", IdempotencyKey: "wf-ownerless"},
		},
	}
	for _, state := range states {
		if err := store.Save(ctx, state); err != nil {
			t.Fatalf("seed %s: %v", state.SpawnID, err)
		}
	}

	ctrl := NewK8sController(nil, "devbox", store, nil,
		WithControllerOwnership("loom-hub/mobile-hud", false))
	if err := ctrl.RecoverFromStore(ctx); err != nil {
		t.Fatalf("RecoverFromStore: %v", err)
	}
	if got := ctrl.List(); len(got) != 1 || got[0].SpawnID != "spawn-cluster-owned" {
		t.Fatalf("owned recovery = %#v, want only cluster row", got)
	}
	for _, peerID := range []string{"spawn-desktop-owned", "spawn-ownerless"} {
		if _, ok := ctrl.Get(peerID); ok {
			t.Fatalf("foreign row %s entered mutable lifecycle map", peerID)
		}
	}
	legacy, err := store.Load(ctx, "spawn-ownerless")
	if err != nil {
		t.Fatalf("load ownerless row: %v", err)
	}
	if legacy.DriverOwnerID != "" {
		t.Fatalf("ordinary peer claimed legacy row as %q", legacy.DriverOwnerID)
	}
}

func TestRecoverFromStoreAuthorityClaimsLegacyRow(t *testing.T) {
	ctx := t.Context()
	store := NewK8sConfigMapStore(fake.NewSimpleClientset(), "devbox", "loom-spawn-state")
	legacy := &State{
		SpawnID: "spawn-legacy-claim", Status: StatusRunning, StartedAt: time.Now(),
		Request: Request{AgentType: "codex", Project: "loom-core", TaskDescription: "legacy", IdempotencyKey: "wf-legacy-claim"},
	}
	if err := store.Save(ctx, legacy); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	ctrl := NewK8sController(nil, "devbox", store, nil,
		WithControllerOwnership("loom-hub/mobile-hud", true))
	if err := ctrl.RecoverFromStore(ctx); err != nil {
		t.Fatalf("RecoverFromStore: %v", err)
	}
	state, ok := ctrl.Get(legacy.SpawnID)
	if !ok || state.DriverOwnerID != "loom-hub/mobile-hud" {
		t.Fatalf("claimed state = %#v", state)
	}
	durable, err := store.Load(ctx, legacy.SpawnID)
	if err != nil {
		t.Fatalf("load claimed row: %v", err)
	}
	if durable.DriverOwnerID != "loom-hub/mobile-hud" {
		t.Fatalf("durable claim = %q", durable.DriverOwnerID)
	}
}

func TestRecoverFromStoreAuthorityClaimsMissingOwnerLabelWithGenerationProof(t *testing.T) {
	ctx := t.Context()
	startedAt := time.Now().Add(-time.Minute).UTC()
	legacy := &State{
		SpawnID: "spawn-legacy-runtime", AgentID: "spawn-codex-legacy-runtime",
		PodName: "spawn-legacy-runtime", Status: StatusRunning, StartedAt: startedAt,
		Request: Request{
			AgentType: "codex", Project: "loom-core", TaskDescription: "legacy live runtime",
			IdempotencyKey: "wf-legacy-live-runtime",
		},
	}
	pod := makePod(legacy.PodName, legacy.SpawnID, legacy.AgentID, corev1.PodRunning)
	pod.UID = "legacy-runtime-uid"
	delete(pod.Labels, DriverOwnerLabel)
	pod.Labels[RuntimeGenerationLabel] = RuntimeGenerationLabelValue(startedAt)
	client := fake.NewSimpleClientset(pod)
	store := NewK8sConfigMapStore(client, "devbox", "loom-spawn-state")
	if err := store.Save(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	ctrl := NewK8sController(client, "devbox", store, nil,
		WithControllerOwnership("loom-hub/mobile-hud", true))
	if err := ctrl.RecoverFromStore(ctx); err != nil {
		t.Fatalf("RecoverFromStore: %v", err)
	}
	claimed, ok := ctrl.Get(legacy.SpawnID)
	if !ok || claimed.DriverOwnerID != "loom-hub/mobile-hud" {
		t.Fatalf("claimed state = %#v", claimed)
	}
	claimedPod, err := client.CoreV1().Pods("devbox").Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range RuntimeIdentityLabelsForState(claimed) {
		if got := claimedPod.Labels[key]; got != want {
			t.Errorf("claimed runtime label %s = %q, want %q", key, got, want)
		}
	}
	if err := ctrl.Reconcile(ctx); err != nil {
		t.Fatalf("immediate Reconcile: %v", err)
	}
	if after, ok := ctrl.Get(legacy.SpawnID); !ok || after.DriverOwnerID != claimed.DriverOwnerID {
		t.Fatalf("claimed runtime was evicted by immediate reconcile: %#v", after)
	}
}

func TestRecoverFromStoreDoesNotStampSameNameReplacementWithoutGenerationProof(t *testing.T) {
	ctx := t.Context()
	const ownerID = "loom-hub/mobile-hud"
	startedAt := time.Now().Add(-time.Hour).UTC()
	state := &State{
		SpawnID: "spawn-runtime-aba", AgentID: "spawn-codex-runtime-aba",
		DriverOwnerID: ownerID, PodName: "spawn-runtime-aba", Status: StatusRunning, StartedAt: startedAt,
		Request: Request{
			AgentType: "codex", Project: "loom-core", TaskDescription: "old durable generation",
			IdempotencyKey: "wf-runtime-aba",
		},
	}
	replacement := makePod(state.PodName, state.SpawnID, state.AgentID, corev1.PodRunning)
	replacement.UID = "replacement-runtime-uid"
	replacement.CreationTimestamp = metav1.NewTime(startedAt.Add(30 * time.Minute))
	delete(replacement.Labels, DriverOwnerLabel)
	delete(replacement.Labels, RuntimeGenerationLabel)
	client := fake.NewSimpleClientset(replacement)
	store := NewK8sConfigMapStore(client, "devbox", "loom-spawn-state")
	if err := store.Save(ctx, state); err != nil {
		t.Fatal(err)
	}
	ctrl := NewK8sController(client, "devbox", store, nil,
		WithControllerOwnership(ownerID, true))

	err := ctrl.RecoverFromStore(ctx)
	if !errors.Is(err, ErrSpawnStateConflict) || !strings.Contains(err.Error(), "without immutable generation proof") {
		t.Fatalf("RecoverFromStore error = %v, want generation-proof conflict", err)
	}
	if _, ok := ctrl.Get(state.SpawnID); ok {
		t.Fatal("ambiguous same-name replacement entered the mutable lifecycle map")
	}
	got, getErr := client.CoreV1().Pods("devbox").Get(ctx, replacement.Name, metav1.GetOptions{})
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Labels[DriverOwnerLabel] != "" || got.Labels[RuntimeGenerationLabel] != "" {
		t.Fatalf("ambiguous replacement was stamped with old identity: %v", got.Labels)
	}
}

func TestRegisterKeyedChecksDurableDriverOwner(t *testing.T) {
	ctx := t.Context()
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, "devbox", "loom-spawn-state")
	req := Request{
		AgentType: "codex", Project: "loom-core", TaskDescription: "dispatch once",
		IdempotencyKey: "wf-durable-registration",
	}
	owner := NewK8sController(nil, "devbox", store, nil,
		WithControllerOwnership("loom-hub/mobile-hud", false))
	spawnID, dispatch, err := owner.Register(ctx, req)
	if err != nil || !dispatch {
		t.Fatalf("owner Register = %q/%v/%v", spawnID, dispatch, err)
	}
	if _, updated, err := owner.UpdateUnlessStoppingOrTerminal(ctx, spawnID, func(state *State) {
		state.Status = StatusRunning
		state.PodName = "spawn-" + spawnID
	}); err != nil || !updated {
		t.Fatalf("advance owner state = %v/%v", updated, err)
	}
	before, err := client.CoreV1().ConfigMaps("devbox").Get(ctx, "loom-spawn-state", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get before peer registration: %v", err)
	}
	beforeRaw := before.Data[spawnID]

	peer := NewK8sController(nil, "devbox", store, nil,
		WithControllerOwnership("local/desktop", false))
	if gotID, gotDispatch, err := peer.Register(ctx, req); gotID != "" || gotDispatch || !errors.Is(err, ErrSpawnStateConflict) {
		t.Fatalf("peer Register = %q/%v/%v, want ownership conflict", gotID, gotDispatch, err)
	}
	if _, ok := peer.Get(spawnID); ok {
		t.Fatal("peer registration installed foreign state locally")
	}

	replacement := NewK8sController(nil, "devbox", store, nil,
		WithControllerOwnership("loom-hub/mobile-hud", false))
	gotID, gotDispatch, err := replacement.Register(ctx, req)
	if err != nil || gotID != spawnID || gotDispatch {
		t.Fatalf("same-owner durable reattach = %q/%v/%v", gotID, gotDispatch, err)
	}
	state, ok := replacement.Get(spawnID)
	if !ok || state.Status != StatusRunning {
		t.Fatalf("same-owner reattach state = %#v", state)
	}
	after, err := client.CoreV1().ConfigMaps("devbox").Get(ctx, "loom-spawn-state", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get after registration attempts: %v", err)
	}
	if got := after.Data[spawnID]; got != beforeRaw {
		t.Fatalf("registration retry rewrote Running row\nbefore: %s\nafter:  %s", beforeRaw, got)
	}
}

func TestDeleteDoesNotRemovePeerReplacement(t *testing.T) {
	const namespace, name, spawnID = "devbox", "spawn-state", "spawn-delete-peer-replacement"
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, namespace, name)
	endedAt := time.Now().Add(-2 * time.Hour)
	cleanupAt := endedAt.Add(time.Minute)
	ownerA := &State{
		SpawnID: spawnID, DriverOwnerID: "owner-a", Status: StatusCompleted,
		StartedAt: endedAt.Add(-time.Hour), EndedAt: &endedAt, CleanupAt: &cleanupAt,
		Request: Request{IdempotencyKey: "key-a"},
	}
	if err := store.Save(t.Context(), ownerA); err != nil {
		t.Fatal(err)
	}
	ctrl := NewK8sController(nil, namespace, store, nil, WithControllerOwnership("owner-a", false))
	if err := ctrl.RecoverFromStore(t.Context()); err != nil {
		t.Fatal(err)
	}
	ownerB := &State{
		SpawnID: spawnID, DriverOwnerID: "owner-b", Status: StatusRunning,
		StartedAt: ownerA.StartedAt.Add(time.Minute), Request: Request{IdempotencyKey: "key-b"},
	}
	replaceConfigMapSpawnState(t, client, namespace, name, ownerB)

	err := ctrl.Delete(t.Context(), spawnID)
	if !errors.Is(err, ErrSpawnStateConflict) {
		t.Fatalf("Delete error = %v, want conflict", err)
	}
	loaded, err := store.Load(t.Context(), spawnID)
	if err != nil || loaded == nil || loaded.DriverOwnerID != ownerB.DriverOwnerID {
		t.Fatalf("peer replacement changed: loaded=%+v err=%v", loaded, err)
	}
	if _, ok := ctrl.Get(spawnID); ok {
		t.Fatal("stale local generation was not evicted")
	}
	ctrl.mu.RLock()
	_, peer := ctrl.peerSpawns[spawnID]
	ctrl.mu.RUnlock()
	if !peer {
		t.Fatal("replacement ID was not cached as peer-owned")
	}
}

func TestPruneDoesNotRemovePeerReplacement(t *testing.T) {
	const namespace, name, spawnID = "devbox", "spawn-state", "spawn-prune-peer-replacement"
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, namespace, name)
	endedAt := time.Now().Add(-48 * time.Hour)
	cleanupAt := endedAt.Add(time.Minute)
	ownerA := &State{
		SpawnID: spawnID, DriverOwnerID: "owner-a", Status: StatusFailed,
		StartedAt: endedAt.Add(-time.Hour), EndedAt: &endedAt, CleanupAt: &cleanupAt,
		Request: Request{IdempotencyKey: "key-a"},
	}
	if err := store.Save(t.Context(), ownerA); err != nil {
		t.Fatal(err)
	}
	ctrl := NewK8sController(nil, namespace, store, nil, WithControllerOwnership("owner-a", false))
	if err := ctrl.RecoverFromStore(t.Context()); err != nil {
		t.Fatal(err)
	}
	ownerB := &State{
		SpawnID: spawnID, DriverOwnerID: "owner-b", Status: StatusRunning,
		StartedAt: ownerA.StartedAt.Add(time.Minute), Request: Request{IdempotencyKey: "key-b"},
	}
	replaceConfigMapSpawnState(t, client, namespace, name, ownerB)

	if got := ctrl.Prune(t.Context(), 24*time.Hour); got != 0 {
		t.Fatalf("Prune count = %d, want 0", got)
	}
	loaded, err := store.Load(t.Context(), spawnID)
	if err != nil || loaded == nil || loaded.DriverOwnerID != ownerB.DriverOwnerID {
		t.Fatalf("peer replacement changed: loaded=%+v err=%v", loaded, err)
	}
	if _, ok := ctrl.Get(spawnID); ok {
		t.Fatal("stale local generation was not evicted")
	}
	ctrl.mu.RLock()
	_, peer := ctrl.peerSpawns[spawnID]
	ctrl.mu.RUnlock()
	if !peer {
		t.Fatal("replacement ID was not cached as peer-owned")
	}
}

func TestDeleteRetainsTerminalGenerationUntilCleanupAcknowledged(t *testing.T) {
	const namespace, name, spawnID = "devbox", "spawn-state", "spawn-cleanup-pending-delete"
	ctx := t.Context()
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, namespace, name)
	endedAt := time.Now().Add(-time.Hour)
	state := &State{
		SpawnID: spawnID, DriverOwnerID: "owner-a", Status: StatusCompleted,
		StartedAt: endedAt.Add(-time.Hour), EndedAt: &endedAt,
		Request: Request{IdempotencyKey: "key-a"},
	}
	if err := store.Save(ctx, state); err != nil {
		t.Fatal(err)
	}
	ctrl := NewK8sController(client, namespace, store, nil, WithControllerOwnership("owner-a", false))
	if err := ctrl.RecoverFromStore(ctx); err != nil {
		t.Fatal(err)
	}

	if err := ctrl.Delete(ctx, spawnID); !errors.Is(err, ErrSpawnCleanupPending) {
		t.Fatalf("Delete error = %v, want ErrSpawnCleanupPending", err)
	}
	if durable, err := store.Load(ctx, spawnID); err != nil || durable == nil {
		t.Fatalf("cleanup-pending durable row was removed: state=%+v err=%v", durable, err)
	}
	if _, ok := ctrl.Get(spawnID); !ok {
		t.Fatal("cleanup-pending local row was removed")
	}

	cleanupCalls := 0
	ctrl.SetTerminalHook(func(context.Context, State) error {
		cleanupCalls++
		return nil
	})
	if err := ctrl.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if cleanupCalls != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanupCalls)
	}
	if err := ctrl.Delete(ctx, spawnID); err != nil {
		t.Fatalf("Delete after cleanup acknowledgement: %v", err)
	}
	if durable, err := store.Load(ctx, spawnID); err != nil || durable != nil {
		t.Fatalf("acknowledged row remains durable: state=%+v err=%v", durable, err)
	}
}

func TestPruneRetainsCleanupPendingTerminalGeneration(t *testing.T) {
	ctx := t.Context()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctrl := NewK8sController(fake.NewSimpleClientset(), "devbox", store, nil)
	endedAt := time.Now().Add(-48 * time.Hour)
	state := &State{
		SpawnID: "spawn-cleanup-pending-prune", Status: StatusFailed,
		StartedAt: endedAt.Add(-time.Hour), EndedAt: &endedAt,
	}
	ctrl.mu.Lock()
	ctrl.spawns[state.SpawnID] = state
	ctrl.mu.Unlock()
	if err := store.Save(ctx, state); err != nil {
		t.Fatal(err)
	}

	if got := ctrl.Prune(ctx, 24*time.Hour); got != 0 {
		t.Fatalf("Prune count = %d, want 0 while cleanup is pending", got)
	}
	if durable, err := store.Load(ctx, state.SpawnID); err != nil || durable == nil {
		t.Fatalf("cleanup-pending row was pruned: state=%+v err=%v", durable, err)
	}
}

func TestReconcileStaleHookCannotAcknowledgeDurableReplacement(t *testing.T) {
	const namespace, name, spawnID = "devbox", "spawn-state", "spawn-stale-cleanup-ack"
	ctx := t.Context()
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, namespace, name)
	endedAt := time.Now().Add(-time.Hour)
	original := &State{
		SpawnID: spawnID, DriverOwnerID: "owner-a", Status: StatusCompleted,
		StartedAt: endedAt.Add(-time.Hour), EndedAt: &endedAt,
		Request: Request{IdempotencyKey: "key-a"},
	}
	if err := store.Save(ctx, original); err != nil {
		t.Fatal(err)
	}
	ctrl := NewK8sController(client, namespace, store, nil, WithControllerOwnership("owner-a", false))
	if err := ctrl.RecoverFromStore(ctx); err != nil {
		t.Fatal(err)
	}
	replacement := &State{
		SpawnID: spawnID, DriverOwnerID: "owner-b", Status: StatusCompleted,
		StartedAt: original.StartedAt.Add(time.Minute), EndedAt: &endedAt,
		Error: "replacement error must remain", Request: Request{IdempotencyKey: "key-b"},
	}
	ctrl.SetTerminalHook(func(context.Context, State) error {
		replaceConfigMapSpawnState(t, client, namespace, name, replacement)
		return nil
	})

	if err := ctrl.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	local, ok := ctrl.Get(spawnID)
	if !ok || local.CleanupAt != nil {
		t.Fatalf("stale local generation was acknowledged: %+v", local)
	}
	durable, err := store.Load(ctx, spawnID)
	if err != nil {
		t.Fatal(err)
	}
	if durable == nil || durable.DriverOwnerID != replacement.DriverOwnerID || durable.CleanupAt != nil {
		t.Fatalf("replacement was stamped by stale hook: %+v", durable)
	}
}

func TestClearTerminalErrorForGenerationRejectsReplacement(t *testing.T) {
	const namespace, name, spawnID = "devbox", "spawn-state", "spawn-stale-error-repair"
	ctx := t.Context()
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, namespace, name)
	endedAt := time.Now().Add(-time.Hour)
	original := &State{
		SpawnID: spawnID, DriverOwnerID: "owner-a", Status: StatusCompleted,
		StartedAt: endedAt.Add(-time.Hour), EndedAt: &endedAt, Error: "old poisoned error",
		Request: Request{IdempotencyKey: "key-a"},
	}
	if err := store.Save(ctx, original); err != nil {
		t.Fatal(err)
	}
	ctrl := NewK8sController(client, namespace, store, nil, WithControllerOwnership("owner-a", false))
	if err := ctrl.RecoverFromStore(ctx); err != nil {
		t.Fatal(err)
	}
	expected, _ := ctrl.Get(spawnID)
	replacement := &State{
		SpawnID: spawnID, DriverOwnerID: "owner-b", Status: StatusCompleted,
		StartedAt: original.StartedAt.Add(time.Minute), EndedAt: &endedAt,
		Error: "replacement error must remain", Request: Request{IdempotencyKey: "key-b"},
	}
	replaceConfigMapSpawnState(t, client, namespace, name, replacement)
	ctrl.mu.Lock()
	ctrl.spawns[spawnID] = cloneStateForRead(replacement)
	ctrl.mu.Unlock()

	if _, repaired, err := ctrl.ClearTerminalErrorForGeneration(ctx, *expected); repaired || !errors.Is(err, ErrSpawnStateConflict) {
		t.Fatalf("stale repair = repaired %v, err %v; want generation conflict", repaired, err)
	}
	local, _ := ctrl.Get(spawnID)
	if local.Error != replacement.Error {
		t.Fatalf("replacement local error changed to %q", local.Error)
	}
	durable, err := store.Load(ctx, spawnID)
	if err != nil || durable == nil || durable.Error != replacement.Error {
		t.Fatalf("replacement durable error changed: state=%+v err=%v", durable, err)
	}
}

func TestRegisterPersistsDriverOwnerBeforeDispatch(t *testing.T) {
	ctx := t.Context()
	store := NewK8sConfigMapStore(fake.NewSimpleClientset(), "devbox", "loom-spawn-state")
	ctrl := NewK8sController(nil, "devbox", store, nil,
		WithControllerOwnership("loom-hub/mobile-hud", false))
	for _, req := range []Request{
		{AgentType: "codex", Project: "loom-core", TaskDescription: "legacy registration"},
		{AgentType: "codex", Project: "loom-core", TaskDescription: "keyed registration", IdempotencyKey: "wf-owner-before-dispatch"},
	} {
		spawnID, dispatch, err := ctrl.Register(ctx, req)
		if err != nil || !dispatch {
			t.Fatalf("Register(%q) = %q/%v/%v", req.IdempotencyKey, spawnID, dispatch, err)
		}
		durable, err := store.Load(ctx, spawnID)
		if err != nil {
			t.Fatalf("load %s: %v", spawnID, err)
		}
		if durable == nil || durable.DriverOwnerID != "loom-hub/mobile-hud" || durable.Status != StatusPending {
			t.Fatalf("initial durable state = %#v", durable)
		}
	}
}

func TestRegisterOwnedUnkeyedPersistenceFailureFailsClosed(t *testing.T) {
	storeErr := errors.New("injected owned registration persistence failure")
	store := &toggleFailStore{fail: true, err: storeErr}
	ctrl := NewK8sController(nil, "devbox", store, nil,
		WithControllerOwnership("loom-hub/mobile-hud", false))

	spawnID, dispatch, err := ctrl.Register(t.Context(), Request{
		AgentType: "codex", Project: "loom-core", TaskDescription: "must be durable before dispatch",
	})
	if spawnID != "" || dispatch || !errors.Is(err, storeErr) {
		t.Fatalf("Register = %q/%v/%v, want empty/false/persistence error", spawnID, dispatch, err)
	}
	if got := ctrl.List(); len(got) != 0 {
		t.Fatalf("failed owned registration left provisional state: %#v", got)
	}
}

func TestReconcileOwnerAwareOrphanAuthority(t *testing.T) {
	ctx := t.Context()
	newWorld := func() (*corev1.Pod, *fake.Clientset, *K8sConfigMapStore) {
		pod := makePod("spawn-ownerless-orphan", "spawn-ownerless-orphan", "agent-orphan", corev1.PodRunning)
		pod.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute).UTC())
		pod.Labels[RuntimeGenerationLabel] = RuntimeGenerationLabelValue(pod.CreationTimestamp.Add(-time.Second))
		client := fake.NewSimpleClientset([]runtime.Object{pod}...)
		return pod, client, NewK8sConfigMapStore(client, "devbox", "loom-spawn-state")
	}

	_, observerClient, observerStore := newWorld()
	observer := NewK8sController(observerClient, "devbox", observerStore, nil,
		WithControllerOwnership("local/desktop", false))
	if err := observer.Reconcile(ctx); err != nil {
		t.Fatalf("observer Reconcile: %v", err)
	}
	if _, ok := observer.Get("spawn-ownerless-orphan"); ok {
		t.Fatal("ordinary owner adopted unlabeled orphan")
	}
	if durable, err := observerStore.Load(ctx, "spawn-ownerless-orphan"); err != nil || durable != nil {
		t.Fatalf("ordinary owner persisted orphan = %#v/%v", durable, err)
	}

	_, authorityClient, authorityStore := newWorld()
	authority := NewK8sController(authorityClient, "devbox", authorityStore, nil,
		WithControllerOwnership("loom-hub/mobile-hud", true))
	if err := authority.Reconcile(ctx); err != nil {
		t.Fatalf("authority Reconcile: %v", err)
	}
	state, ok := authority.Get("spawn-ownerless-orphan")
	if !ok || state.DriverOwnerID != "loom-hub/mobile-hud" {
		t.Fatalf("authority orphan state = %#v", state)
	}
	durable, err := authorityStore.Load(ctx, "spawn-ownerless-orphan")
	if err != nil || durable == nil || durable.DriverOwnerID != "loom-hub/mobile-hud" {
		t.Fatalf("durable orphan claim = %#v/%v", durable, err)
	}
	claimedPod, err := authorityClient.CoreV1().Pods("devbox").Get(ctx, "spawn-ownerless-orphan", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range RuntimeIdentityLabelsForState(state) {
		if got := claimedPod.Labels[key]; got != want {
			t.Errorf("claimed orphan label %s = %q, want %q", key, got, want)
		}
	}
}

func TestReconcileOwnerAwareControllerClaimsMatchingLabeledOrphan(t *testing.T) {
	ctx := t.Context()
	pod := makePod("spawn-owned-orphan", "spawn-owned-orphan", "agent-orphan", corev1.PodRunning)
	pod.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute).UTC())
	startedAt := pod.CreationTimestamp.Add(-30 * time.Second).UTC()
	pod.Labels[DriverOwnerLabel] = DriverOwnerLabelValue("local/desktop")
	pod.Labels[RuntimeGenerationLabel] = RuntimeGenerationLabelValue(startedAt)
	client := fake.NewSimpleClientset([]runtime.Object{pod}...)
	store := NewK8sConfigMapStore(client, "devbox", "loom-spawn-state")
	ctrl := NewK8sController(client, "devbox", store, nil,
		WithControllerOwnership("local/desktop", false))
	if err := ctrl.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	state, ok := ctrl.Get("spawn-owned-orphan")
	if !ok || state.DriverOwnerID != "local/desktop" {
		t.Fatalf("matching-owner orphan state = %#v", state)
	}
	if !state.StartedAt.Equal(startedAt) {
		t.Fatalf("matching-owner orphan StartedAt = %s, want generation label time %s", state.StartedAt, startedAt)
	}
	durable, err := store.Load(ctx, state.SpawnID)
	if err != nil || durable == nil || !durable.StartedAt.Equal(startedAt) {
		t.Fatalf("matching-owner durable generation = %#v/%v, want %s", durable, err, startedAt)
	}
}

func TestReconcileInvalidRowlessGenerationLabelFailsClosed(t *testing.T) {
	ctx := t.Context()
	pod := makePod("spawn-invalid-generation", "spawn-invalid-generation", "agent-invalid-generation", corev1.PodRunning)
	pod.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute).UTC())
	pod.Labels[DriverOwnerLabel] = DriverOwnerLabelValue("local/desktop")
	pod.Labels[RuntimeGenerationLabel] = "not-a-canonical-generation"
	client := fake.NewSimpleClientset(pod)
	store := NewK8sConfigMapStore(client, "devbox", "loom-spawn-state")
	ctrl := NewK8sController(client, "devbox", store, nil,
		WithControllerOwnership("local/desktop", false))

	if err := ctrl.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := ctrl.Get("spawn-invalid-generation"); ok {
		t.Fatal("invalid generation label entered the mutable lifecycle map")
	}
	if durable, err := store.Load(ctx, "spawn-invalid-generation"); err != nil || durable != nil {
		t.Fatalf("invalid generation label was persisted: %#v/%v", durable, err)
	}
}

func TestReconcileOrphanClaimReloadsConcurrentFullDurableRow(t *testing.T) {
	ctx := t.Context()
	const spawnID = "spawn-discovery-full-row-race"
	pod := makePod(spawnID, spawnID, "agent-full-row", corev1.PodRunning)
	pod.UID = "discovery-full-row-uid"
	pod.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute))
	delete(pod.Labels, DriverOwnerLabel)
	delete(pod.Labels, RuntimeGenerationLabel)
	winner := &State{
		SpawnID: spawnID, AgentID: "agent-full-row", PodName: pod.Name,
		Status: StatusRunning, StartedAt: pod.CreationTimestamp.Add(-time.Minute),
		Request: Request{
			AgentType: "codex", Project: "loom-core", TaskDescription: "preserve complete request",
			IdempotencyKey: "wf-discovery-full-row-race", TimeoutMinutes: 42,
		},
	}
	pod.Labels[RuntimeGenerationLabel] = RuntimeGenerationLabelValue(winner.StartedAt)
	client := fake.NewSimpleClientset(pod)
	baseStore := NewK8sConfigMapStore(client, "devbox", "loom-spawn-state")
	store := &discoveryRaceStore{delegate: baseStore, winner: winner}
	ctrl := NewK8sController(client, "devbox", store, nil,
		WithControllerOwnership("loom-hub/mobile-hud", true))

	if err := ctrl.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	local, ok := ctrl.Get(spawnID)
	if !ok || local.DriverOwnerID != "loom-hub/mobile-hud" ||
		local.Request.IdempotencyKey != winner.Request.IdempotencyKey ||
		local.Request.TaskDescription != winner.Request.TaskDescription ||
		local.Request.TimeoutMinutes != winner.Request.TimeoutMinutes ||
		!local.StartedAt.Equal(winner.StartedAt) {
		t.Fatalf("local orphan claim lost concurrent full row: %#v", local)
	}
	durable, err := baseStore.Load(ctx, spawnID)
	if err != nil || durable == nil || durable.Request.IdempotencyKey != winner.Request.IdempotencyKey {
		t.Fatalf("durable full row changed: state=%#v err=%v", durable, err)
	}
	claimedPod, err := client.CoreV1().Pods("devbox").Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range RuntimeIdentityLabelsForState(local) {
		if got := claimedPod.Labels[key]; got != want {
			t.Errorf("runtime label %s = %q, want authoritative %q", key, got, want)
		}
	}
}

func TestReconcileOrphanHasOneDurableRecoveryAuthorityWinner(t *testing.T) {
	ctx := t.Context()
	pod := makePod("spawn-authority-race", "spawn-authority-race", "agent-orphan", corev1.PodRunning)
	pod.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute).UTC())
	pod.Labels[RuntimeGenerationLabel] = RuntimeGenerationLabelValue(pod.CreationTimestamp.Add(-time.Second))
	client := fake.NewSimpleClientset([]runtime.Object{pod}...)
	store := NewK8sConfigMapStore(client, "devbox", "loom-spawn-state")
	first := NewK8sController(client, "devbox", store, nil,
		WithControllerOwnership("loom-hub/mobile-hud", true))
	second := NewK8sController(client, "devbox", store, nil,
		WithControllerOwnership("local/desktop", true))
	if err := first.Reconcile(ctx); err != nil {
		t.Fatalf("first authority Reconcile: %v", err)
	}
	if err := second.Reconcile(ctx); err != nil {
		t.Fatalf("second authority Reconcile: %v", err)
	}
	if _, ok := second.Get("spawn-authority-race"); ok {
		t.Fatal("second authority adopted the first authority's durable orphan")
	}
	durable, err := store.Load(ctx, "spawn-authority-race")
	if err != nil {
		t.Fatalf("load durable winner: %v", err)
	}
	if durable == nil || durable.DriverOwnerID != "loom-hub/mobile-hud" {
		t.Fatalf("durable authority winner = %#v", durable)
	}
}

func TestUpdateUnlessStoppingOrTerminalReloadsDurableTerminalWinner(t *testing.T) {
	ctx := t.Context()
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, "devbox", "loom-spawn-state")
	request := Request{
		AgentType: "codex", Project: "loom-core", TaskDescription: "one terminal owner",
		IdempotencyKey: "wf-controller-terminal-winner",
	}
	running := &State{
		SpawnID: "spawn-controller-terminal-winner", AgentID: "agent-owner", PodName: "spawn-controller-terminal-winner",
		Status: StatusRunning, Request: request, StartedAt: time.Now().Add(-time.Minute),
	}
	if err := store.Save(ctx, running); err != nil {
		t.Fatalf("save running state: %v", err)
	}
	stale := NewK8sController(client, "devbox", store, nil)
	if err := stale.RecoverFromStore(ctx); err != nil {
		t.Fatalf("recover stale controller: %v", err)
	}
	endedAt := time.Now()
	winner := *running
	winner.Status = StatusCompleted
	winner.EndedAt = &endedAt
	if err := store.Save(ctx, &winner); err != nil {
		t.Fatalf("commit peer terminal winner: %v", err)
	}

	staleEndedAt := time.Now().Add(time.Second)
	result, updated, err := stale.UpdateUnlessStoppingOrTerminal(ctx, running.SpawnID, func(state *State) {
		state.Status = StatusCompleted
		state.EndedAt = &staleEndedAt
	})
	if updated || !errors.Is(err, ErrSpawnStateConflict) {
		t.Fatalf("stale terminal transition = updated %v error %v, want rejected conflict", updated, err)
	}
	if result.Status != StatusCompleted || result.EndedAt == nil || !result.EndedAt.Equal(endedAt) {
		t.Fatalf("returned winner = %#v", result)
	}
	loaded, ok := stale.Get(running.SpawnID)
	if !ok || loaded.Status != StatusCompleted || loaded.EndedAt == nil || !loaded.EndedAt.Equal(endedAt) {
		t.Fatalf("controller did not reload durable winner: %#v", loaded)
	}
}

func TestUpdateUnlessStoppingOrTerminalSurfacesTransientPersistenceFailure(t *testing.T) {
	storeErr := errors.New("injected transient save failure")
	store := &toggleFailStore{fail: true, err: storeErr}
	ctrl := NewK8sController(fake.NewSimpleClientset(), "devbox", store, nil)
	ctrl.spawns["spawn-transient-save"] = &State{SpawnID: "spawn-transient-save", Status: StatusPending}

	result, updated, err := ctrl.UpdateUnlessStoppingOrTerminal(t.Context(), "spawn-transient-save", func(state *State) {
		state.Status = StatusBuilding
	})
	if updated || !errors.Is(err, storeErr) {
		t.Fatalf("transition = updated %v error %v, want surfaced save failure", updated, err)
	}
	if result.Status != StatusPending {
		t.Fatalf("result status = %s, want rolled-back pending", result.Status)
	}
	loaded, _ := ctrl.Get("spawn-transient-save")
	if loaded.Status != StatusPending {
		t.Fatalf("in-memory status = %s, want rolled-back pending", loaded.Status)
	}
}

func TestReconcileRejectedTerminalTransitionDoesNotFireHook(t *testing.T) {
	ctx := t.Context()
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, "devbox", "loom-spawn-state")
	request := Request{
		AgentType: "codex", Project: "loom-core", TaskDescription: "peer completion wins",
		IdempotencyKey: "wf-reconcile-terminal-winner",
	}
	running := &State{
		SpawnID: "spawn-reconcile-terminal-winner", PodName: "missing-peer-pod", Status: StatusRunning,
		Request: request, StartedAt: time.Now().Add(-time.Minute),
	}
	if err := store.Save(ctx, running); err != nil {
		t.Fatalf("save running state: %v", err)
	}
	ctrl := NewK8sController(client, "devbox", store, nil)
	if err := ctrl.RecoverFromStore(ctx); err != nil {
		t.Fatalf("recover stale controller: %v", err)
	}
	endedAt := time.Now()
	completed := *running
	completed.Status = StatusCompleted
	completed.EndedAt = &endedAt
	if err := store.Save(ctx, &completed); err != nil {
		t.Fatalf("commit peer terminal winner: %v", err)
	}
	hookCalls := 0
	ctrl.SetTerminalHook(func(context.Context, State) error {
		hookCalls++
		return nil
	})

	if err := ctrl.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if hookCalls != 0 {
		t.Fatalf("terminal hook calls = %d, want 0 for rejected local transition", hookCalls)
	}
	loaded, _ := ctrl.Get(running.SpawnID)
	if loaded.Status != StatusCompleted || loaded.EndedAt == nil || !loaded.EndedAt.Equal(endedAt) {
		t.Fatalf("controller did not converge on peer winner: %#v", loaded)
	}
}

func TestBeginStopOnlyOneControllerClaimsDurableIntent(t *testing.T) {
	ctx := t.Context()
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, "devbox", "loom-spawn-state")
	running := &State{
		SpawnID: "spawn-stop-owner", Status: StatusRunning, StartedAt: time.Now(),
		Request: Request{AgentType: "codex", Project: "loom-core", TaskDescription: "single stop owner", IdempotencyKey: "wf-stop-owner"},
	}
	if err := store.Save(ctx, running); err != nil {
		t.Fatalf("save running state: %v", err)
	}
	first := NewK8sController(client, "devbox", store, nil)
	second := NewK8sController(client, "devbox", store, nil)
	if err := first.RecoverFromStore(ctx); err != nil {
		t.Fatalf("recover first controller: %v", err)
	}
	if err := second.RecoverFromStore(ctx); err != nil {
		t.Fatalf("recover second controller: %v", err)
	}
	firstState, firstDisposition, err := first.BeginStop(ctx, running.SpawnID)
	if err != nil || firstDisposition != StopBegan {
		t.Fatalf("first BeginStop = disposition %v error %v", firstDisposition, err)
	}
	secondState, secondDisposition, err := second.BeginStop(ctx, running.SpawnID)
	if err != nil || secondDisposition != StopAlreadyRequested {
		t.Fatalf("second BeginStop = disposition %v error %v", secondDisposition, err)
	}
	if firstState.StopRequestedAt == nil || secondState.StopRequestedAt == nil || !secondState.StopRequestedAt.Equal(*firstState.StopRequestedAt) {
		t.Fatalf("controllers disagree on stop owner: first=%#v second=%#v", firstState.StopRequestedAt, secondState.StopRequestedAt)
	}
}

func TestBeginStopConvergesOnPeerTerminalWinner(t *testing.T) {
	ctx := t.Context()
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, "devbox", "loom-spawn-state")
	running := &State{
		SpawnID: "spawn-stop-vs-terminal", Status: StatusRunning, StartedAt: time.Now(),
		Request: Request{AgentType: "codex", Project: "loom-core", TaskDescription: "terminal beats stop", IdempotencyKey: "wf-stop-vs-terminal"},
	}
	if err := store.Save(ctx, running); err != nil {
		t.Fatalf("save running state: %v", err)
	}
	terminalOwner := NewK8sController(client, "devbox", store, nil)
	stopper := NewK8sController(client, "devbox", store, nil)
	if err := terminalOwner.RecoverFromStore(ctx); err != nil {
		t.Fatalf("recover terminal owner: %v", err)
	}
	if err := stopper.RecoverFromStore(ctx); err != nil {
		t.Fatalf("recover stopper: %v", err)
	}
	endedAt := time.Now()
	if _, updated, err := terminalOwner.UpdateUnlessStoppingOrTerminal(ctx, running.SpawnID, func(state *State) {
		state.Status = StatusCompleted
		state.EndedAt = &endedAt
	}); err != nil || !updated {
		t.Fatalf("commit terminal winner: updated=%v error=%v", updated, err)
	}
	winner, disposition, err := stopper.BeginStop(ctx, running.SpawnID)
	if err != nil || disposition != StopTerminal {
		t.Fatalf("BeginStop after peer terminal = disposition %v error %v", disposition, err)
	}
	if winner.Status != StatusCompleted || winner.EndedAt == nil || !winner.EndedAt.Equal(endedAt) {
		t.Fatalf("stopper did not converge on terminal winner: %#v", winner)
	}
}

func TestReconcilePreservesTerminalState(t *testing.T) {
	// Pod succeeded, but the state was already marked failed.
	pod := makePod("spawn-pod-x", "spawn-term", "agent-x", corev1.PodSucceeded)
	client := fake.NewSimpleClientset([]runtime.Object{pod}...)
	ctrl := NewK8sController(client, "devbox", nil, nil)

	ctrl.mu.Lock()
	ctrl.spawns["spawn-term"] = &State{
		SpawnID: "spawn-term",
		Status:  StatusFailed,
		Error:   "manually failed",
	}
	ctrl.mu.Unlock()

	if err := ctrl.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	state, _ := ctrl.Get("spawn-term")
	if state.Status != StatusFailed {
		t.Errorf("terminal state was overwritten: got %q, want %q", state.Status, StatusFailed)
	}
}

func TestReconcileNilClient(t *testing.T) {
	// Controller with nil K8s client should skip reconciliation.
	ctrl := NewK8sController(nil, "", nil, nil)
	ctx := context.Background()

	// Pre-populate a running spawn.
	ctrl.mu.Lock()
	ctrl.spawns["spawn-nil"] = &State{
		SpawnID: "spawn-nil",
		Status:  StatusRunning,
	}
	ctrl.mu.Unlock()

	// Reconcile should not error and should not modify state.
	if err := ctrl.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile with nil client: %v", err)
	}

	state, ok := ctrl.Get("spawn-nil")
	if !ok {
		t.Fatal("spawn-nil not found")
	}
	if state.Status != StatusRunning {
		t.Errorf("status should be unchanged: got %q, want %q", state.Status, StatusRunning)
	}
}

func TestSetK8sClient(t *testing.T) {
	ctrl := NewK8sController(nil, "", nil, nil)

	// Initially nil client, reconcile is a no-op.
	if err := ctrl.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile with nil client: %v", err)
	}

	// Inject a fake client.
	pod := makePod("spawn-pod-set", "spawn-set", "agent-set", corev1.PodRunning)
	client := fake.NewSimpleClientset([]runtime.Object{pod}...)
	ctrl.SetK8sClient(client, "devbox")

	// Now reconcile should discover the pod.
	if err := ctrl.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile after SetK8sClient: %v", err)
	}

	state, ok := ctrl.Get("spawn-set")
	if !ok {
		t.Fatal("spawn-set not discovered after SetK8sClient")
	}
	if state.Status != StatusRunning {
		t.Errorf("status: got %q, want %q", state.Status, StatusRunning)
	}
}

func TestNewSpawnID(t *testing.T) {
	id1 := NewSpawnID()
	id2 := NewSpawnID()
	if id1 == id2 {
		t.Error("expected unique spawn IDs")
	}
	if len(id1) < 10 {
		t.Errorf("spawn ID too short: %q", id1)
	}
}

// TestReconcileFiresTerminalHookOnNewlyFailedSpawn verifies the cleanup
// hook is invoked when Reconcile transitions a spawn into a terminal
// state from observing the pod's Failed phase. The hook receives the
// state by value so concurrent reconciles do not race the caller.
func TestReconcileFiresTerminalHookOnNewlyFailedSpawn(t *testing.T) {
	pod := makePod("spawn-pod-fail", "spawn-fail", "agent-fail", corev1.PodFailed)
	client := fake.NewSimpleClientset([]runtime.Object{pod}...)
	ctrl := NewK8sController(client, "devbox", nil, nil)

	ctrl.mu.Lock()
	ctrl.spawns["spawn-fail"] = &State{
		SpawnID: "spawn-fail",
		AgentID: "agent-fail",
		Status:  StatusRunning,
	}
	ctrl.mu.Unlock()

	var hookCalls []State
	ctrl.SetTerminalHook(func(_ context.Context, st State) error {
		hookCalls = append(hookCalls, st)
		return nil
	})

	if err := ctrl.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(hookCalls) != 1 {
		t.Fatalf("hook fired %d times, want 1", len(hookCalls))
	}
	if hookCalls[0].SpawnID != "spawn-fail" || hookCalls[0].AgentID != "agent-fail" {
		t.Fatalf("hook state mismatch: %+v", hookCalls[0])
	}
	state, _ := ctrl.Get("spawn-fail")
	if state.CleanupAt == nil {
		t.Fatal("CleanupAt should be stamped after hook fires")
	}
}

// TestReconcileTerminalHookIsIdempotent ensures CleanupAt gates the
// hook so a long-lived terminal state does not re-trigger cleanup on
// every reconcile tick — the symptom that filled namespace quota
// with stale spawn pods before the reaper landed.
func TestReconcileTerminalHookIsIdempotent(t *testing.T) {
	// Pod is still alive in the cluster even though the spawn's
	// state went terminal — exactly the orphan path that motivated
	// the reaper.
	pod := makePod("spawn-pod-orphan", "spawn-orphan", "agent-orphan", corev1.PodRunning)
	client := fake.NewSimpleClientset([]runtime.Object{pod}...)
	ctrl := NewK8sController(client, "devbox", nil, nil)

	ctrl.mu.Lock()
	ctrl.spawns["spawn-orphan"] = &State{
		SpawnID: "spawn-orphan",
		AgentID: "agent-orphan",
		Status:  StatusFailed,
		PodName: "spawn-pod-orphan",
	}
	ctrl.mu.Unlock()

	var fires int
	ctrl.SetTerminalHook(func(_ context.Context, _ State) error { fires++; return nil })

	for range 3 {
		if err := ctrl.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}

	if fires != 1 {
		t.Fatalf("hook fired %d times across 3 reconciles, want 1 (CleanupAt should gate)", fires)
	}
}

func TestReconcileTerminalHookFailureLeavesCleanupRetryable(t *testing.T) {
	pod := makePod("spawn-pod-retry", "spawn-retry", "agent-retry", corev1.PodRunning)
	client := fake.NewSimpleClientset([]runtime.Object{pod}...)
	ctrl := NewK8sController(client, "devbox", nil, nil)
	ctrl.mu.Lock()
	ctrl.spawns["spawn-retry"] = &State{
		SpawnID: "spawn-retry",
		AgentID: "agent-retry",
		Status:  StatusFailed,
		PodName: "spawn-pod-retry",
	}
	ctrl.mu.Unlock()

	attempts := 0
	ctrl.SetTerminalHook(func(_ context.Context, _ State) error {
		attempts++
		if attempts == 1 {
			return errors.New("injected cleanup failure")
		}
		return nil
	})

	if err := ctrl.Reconcile(context.Background()); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	state, _ := ctrl.Get("spawn-retry")
	if state.CleanupAt != nil {
		t.Fatal("CleanupAt stamped after failed terminal cleanup")
	}
	if err := ctrl.Reconcile(context.Background()); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	state, _ = ctrl.Get("spawn-retry")
	if state.CleanupAt == nil {
		t.Fatal("CleanupAt not stamped after successful retry")
	}
	if err := ctrl.Reconcile(context.Background()); err != nil {
		t.Fatalf("third Reconcile: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("terminal cleanup attempts = %d, want 2", attempts)
	}
}

func TestReconcileTerminalCleanupSaveFailureLeavesCleanupRetryable(t *testing.T) {
	pod := makePod("spawn-pod-save-retry", "spawn-save-retry", "agent-save-retry", corev1.PodRunning)
	client := fake.NewSimpleClientset([]runtime.Object{pod}...)
	durableStore := NewK8sConfigMapStore(client, "devbox", "loom-spawn-state")
	store := &toggleFailStore{
		fail:     true,
		err:      errors.New("injected store failure"),
		delegate: durableStore,
	}
	ctrl := NewK8sController(client, "devbox", store, nil)
	startedAt := time.Now().Add(-time.Minute)
	initial := &State{
		SpawnID:   "spawn-save-retry",
		AgentID:   "agent-save-retry",
		Status:    StatusFailed,
		PodName:   "spawn-pod-save-retry",
		StartedAt: startedAt,
	}
	if err := durableStore.Save(t.Context(), initial); err != nil {
		t.Fatalf("seed durable state: %v", err)
	}
	ctrl.mu.Lock()
	ctrl.spawns[initial.SpawnID] = cloneStateForRead(initial)
	ctrl.mu.Unlock()

	attempts := 0
	ctrl.SetTerminalHook(func(_ context.Context, _ State) error {
		attempts++
		return nil
	})

	if err := ctrl.Reconcile(t.Context()); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	state, _ := ctrl.Get("spawn-save-retry")
	if state.CleanupAt != nil {
		t.Fatal("CleanupAt retained after its persistence failed")
	}

	store.fail = false
	if err := ctrl.Reconcile(t.Context()); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	state, _ = ctrl.Get("spawn-save-retry")
	if state.CleanupAt == nil {
		t.Fatal("CleanupAt not stamped after persistence recovered")
	}
	if attempts != 2 {
		t.Fatalf("terminal cleanup attempts = %d, want 2", attempts)
	}
}

func TestReconcileStoppingPodSaveFailureDefersCleanup(t *testing.T) {
	pod := makePod("late-runtime-handle", "spawn-stopping-save", "agent-stopping-save", corev1.PodRunning)
	store := &toggleFailStore{fail: true, err: errors.New("injected store failure")}
	ctrl := NewK8sController(fake.NewSimpleClientset([]runtime.Object{pod}...), "devbox", store, nil)
	requestedAt := time.Now()
	ctrl.mu.Lock()
	ctrl.spawns["spawn-stopping-save"] = &State{
		SpawnID:         "spawn-stopping-save",
		AgentID:         "agent-stopping-save",
		Status:          StatusRunning,
		StopRequestedAt: &requestedAt,
	}
	ctrl.mu.Unlock()

	attempts := 0
	ctrl.SetStoppingHook(func(_ context.Context, _ State) error {
		attempts++
		return nil
	})

	if err := ctrl.Reconcile(t.Context()); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	state, _ := ctrl.Get("spawn-stopping-save")
	if state.PodName != "" {
		t.Fatalf("PodName = %q after failed persistence, want rollback", state.PodName)
	}
	if attempts != 0 {
		t.Fatalf("cleanup attempts = %d after failed handle persistence, want 0", attempts)
	}

	store.fail = false
	if err := ctrl.Reconcile(t.Context()); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	state, _ = ctrl.Get("spawn-stopping-save")
	if state.PodName != "late-runtime-handle" {
		t.Fatalf("PodName = %q after persistence recovered", state.PodName)
	}
	if attempts != 1 {
		t.Fatalf("cleanup attempts = %d, want 1", attempts)
	}
}

// TestReconcileTerminalHookFiresForDiscoveredOrphan covers the
// operator-restart recovery path: a pod observed for the first time
// already in PodFailed must trigger cleanup so it doesn't linger.
func TestReconcileTerminalHookFiresForDiscoveredOrphan(t *testing.T) {
	pod := makePod("spawn-pod-found", "spawn-found", "agent-found", corev1.PodFailed)
	client := fake.NewSimpleClientset([]runtime.Object{pod}...)
	ctrl := NewK8sController(client, "devbox", nil, nil)

	var fires int
	ctrl.SetTerminalHook(func(_ context.Context, _ State) error { fires++; return nil })

	if err := ctrl.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if fires != 1 {
		t.Fatalf("hook fired %d times for newly-discovered failed pod, want 1", fires)
	}
}

// TestPruneDropsTerminalSpawnsOlderThanMaxAge validates the periodic
// retention loop: terminal spawns whose EndedAt is older than the cutoff
// are removed from both the in-memory map and the persistent store, while
// recent terminal spawns and any non-terminal spawn are retained.
func TestPruneDropsTerminalSpawnsOlderThanMaxAge(t *testing.T) {
	client := fake.NewSimpleClientset()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctrl := NewK8sController(client, "devbox", store, nil)
	ctx := context.Background()

	now := time.Now()
	old := now.Add(-25 * time.Hour)
	recent := now.Add(-5 * time.Minute)

	cases := []struct {
		id      string
		status  Status
		ended   *time.Time
		cleanup *time.Time
	}{
		{id: "old-completed", status: StatusCompleted, ended: &old, cleanup: &old},
		{id: "old-failed", status: StatusFailed, ended: &old, cleanup: &old},
		{id: "old-cleanup-only", status: StatusFailed, cleanup: &old},
		{id: "recent-completed", status: StatusCompleted, ended: &recent, cleanup: &recent},
		{id: "running", status: StatusRunning, ended: nil},
		{id: "stale-no-timestamps", status: StatusFailed},
	}

	ctrl.mu.Lock()
	for _, c := range cases {
		st := &State{
			SpawnID:   c.id,
			AgentID:   "agent-" + c.id,
			Status:    c.status,
			StartedAt: now.Add(-26 * time.Hour),
			EndedAt:   c.ended,
			CleanupAt: c.cleanup,
		}
		ctrl.spawns[c.id] = st
		_ = store.Save(ctx, st)
	}
	ctrl.mu.Unlock()

	pruned := ctrl.Prune(ctx, 24*time.Hour)
	if pruned != 3 {
		t.Fatalf("Prune count: got %d, want 3 (old-completed, old-failed, old-cleanup-only)", pruned)
	}

	// Check in-memory survivors.
	for _, want := range []string{"recent-completed", "running", "stale-no-timestamps"} {
		if _, ok := ctrl.Get(want); !ok {
			t.Errorf("expected %q to remain in-memory after Prune", want)
		}
	}
	for _, gone := range []string{"old-completed", "old-failed", "old-cleanup-only"} {
		if _, ok := ctrl.Get(gone); ok {
			t.Errorf("expected %q to be evicted from in-memory after Prune", gone)
		}
	}

	// Check store survivors mirror in-memory.
	for _, want := range []string{"recent-completed", "running", "stale-no-timestamps"} {
		st, err := store.Load(ctx, want)
		if err != nil {
			t.Errorf("store.Load(%q): %v", want, err)
			continue
		}
		if st == nil {
			t.Errorf("expected %q to remain on disk after Prune", want)
		}
	}
	for _, gone := range []string{"old-completed", "old-failed", "old-cleanup-only"} {
		st, err := store.Load(ctx, gone)
		if err != nil {
			t.Errorf("store.Load(%q): %v", gone, err)
			continue
		}
		if st != nil {
			t.Errorf("expected %q to be deleted from disk after Prune", gone)
		}
	}
}

func TestPrunePressureDropsOldestTerminalStatesToSoftLimit(t *testing.T) {
	const namespace, name = "devbox", "loom-spawn-state"
	ctx := t.Context()
	client := fake.NewSimpleClientset()
	now := time.Now()

	states := []*State{
		{SpawnID: "terminal-oldest", Status: StatusCompleted, StartedAt: now.Add(-4 * time.Hour), EndedAt: timePtr(now.Add(-3 * time.Hour)), Error: strings.Repeat("x", 300)},
		{SpawnID: "terminal-middle", Status: StatusFailed, StartedAt: now.Add(-3 * time.Hour), EndedAt: timePtr(now.Add(-2 * time.Hour)), Error: strings.Repeat("x", 300)},
		{SpawnID: "terminal-newest", Status: StatusStopped, StartedAt: now.Add(-2 * time.Hour), EndedAt: timePtr(now.Add(-time.Hour)), Error: strings.Repeat("x", 300)},
		{SpawnID: "running", Status: StatusRunning, StartedAt: now.Add(-time.Hour), Error: strings.Repeat("x", 300)},
	}
	data := make(map[string]string, len(states))
	for _, state := range states {
		raw, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		data[state.SpawnID] = string(raw)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{ManagedByLabel: ManagedByValue}}, Data: data}
	if _, err := client.CoreV1().ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	store := NewK8sConfigMapStore(client, namespace, name, WithK8sConfigMapSerializedSizeBudget(2000))
	ctrl := NewK8sController(client, namespace, store, nil)
	ctrl.mu.Lock()
	for _, state := range states {
		ctrl.spawns[state.SpawnID] = state
	}
	ctrl.mu.Unlock()

	if got := ctrl.Prune(ctx, 24*time.Hour); got != 2 {
		t.Fatalf("Prune count = %d, want 2 oldest terminal states", got)
	}
	after, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	serialized, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if got, softLimit := len(serialized), 1600; got > softLimit {
		t.Fatalf("serialized ConfigMap = %d bytes, want <= soft limit %d", got, softLimit)
	}
	for _, gone := range []string{"terminal-oldest", "terminal-middle"} {
		if _, ok := after.Data[gone]; ok {
			t.Errorf("pressure prune retained %q", gone)
		}
	}
	for _, kept := range []string{"terminal-newest", "running"} {
		if _, ok := after.Data[kept]; !ok {
			t.Errorf("pressure prune removed %q", kept)
		}
	}
	if _, ok := ctrl.Get("running"); !ok {
		t.Error("pressure prune removed non-terminal state from memory")
	}
}

func timePtr(t time.Time) *time.Time { return &t }

// TestReconcileDoesNotFailRunningSpawnWithMatchingPod is a regression test for
// the Mills spawn "pod not found" canary failure. When the devbox backend labels
// spawn pods with managed-by=loom-spawn (matching the reconciler's selector),
// Reconcile must discover the pod and NOT mark the spawn as failed.
func TestReconcileDoesNotFailRunningSpawnWithMatchingPod(t *testing.T) {
	spawnID := "spawn-regression"
	agentID := "agent-regression"

	// Pod labeled with the spawn constants — exactly what the HUD spawn
	// orchestrator now produces via ManagedByOverride + ExtraLabels.
	pod := makePod("spawn-"+spawnID, spawnID, agentID, corev1.PodRunning)
	client := fake.NewSimpleClientset([]runtime.Object{pod}...)
	ctrl := NewK8sController(client, "devbox", nil, nil)

	// Pre-populate a Running spawn entry (simulating the orchestrator having
	// started the pod successfully and advanced state to Running).
	ctrl.mu.Lock()
	ctrl.spawns[spawnID] = &State{
		SpawnID: spawnID,
		AgentID: agentID,
		Status:  StatusRunning,
		PodName: "spawn-" + spawnID,
	}
	ctrl.mu.Unlock()

	if err := ctrl.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	state, ok := ctrl.Get(spawnID)
	if !ok {
		t.Fatal("spawn not found after reconcile")
	}
	if state.Status != StatusRunning {
		t.Errorf("status: got %q, want %q — reconciler must not mark spawn as failed when pod exists with correct labels", state.Status, StatusRunning)
	}
	if state.Error != "" {
		t.Errorf("error should be empty, got %q", state.Error)
	}
	if state.EndedAt != nil {
		t.Error("EndedAt should remain nil for a running spawn")
	}
}

func TestReconcileSkipsK8sPodNotFoundForHarvesterState(t *testing.T) {
	ctrl := NewK8sController(fake.NewSimpleClientset(), "devbox", nil, nil)
	state := &State{
		SpawnID: "spawn-harvester-running", AgentID: "agent-harvester-running",
		PodName: "spawn-harvester-running", Status: StatusRunning,
		StartedAt: time.Now().Add(-time.Minute),
		Request: Request{
			AgentType: "codex", Project: "loom-core", TaskDescription: "keep VM alive",
			Substrate: "harvester-vm",
		},
	}
	ctrl.mu.Lock()
	ctrl.spawns[state.SpawnID] = state
	ctrl.mu.Unlock()

	if err := ctrl.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, ok := ctrl.Get(state.SpawnID)
	if !ok {
		t.Fatal("Harvester state disappeared during K8s reconciliation")
	}
	if got.Status != StatusRunning || got.Error != "" || got.EndedAt != nil {
		t.Fatalf("Harvester state was reconciled as a missing Pod: %+v", got)
	}
}

// TestReconcileReapsRunningSpawnWithBlankRequest is the regression test for the
// 2026-07-02 spawn-pool wedge: six codex spawns with empty request metadata
// (agent_type/project/task_description blank ⇒ Request.TimeoutMinutes == 0)
// stayed status:"running" in loom-spawn-state for 22–30h, pinning the pool at
// its cap of 6 so every pipeline stage escalated with "400 max concurrent
// spawns reached (6)". The !859 deadline reaper missed them because the old
// spawnDeadlineExceeded short-circuited to false whenever TimeoutMinutes <= 0.
// This models exactly that entry — running, blank Request, StartedAt 24h ago,
// with its pod still alive — and asserts the reconciler now reaps it via the
// absolute-age backstop and fires the terminal hook so the pod (pool slot) is
// released.
func TestReconcileReapsRunningSpawnWithBlankRequest(t *testing.T) {
	// The pod is still alive in the cluster — this is what makes the spawn a
	// slot-holding zombie rather than a "pod not found" failure.
	pod := makePod("spawn-pod-zombie", "spawn-zombie", "agent-zombie", corev1.PodRunning)
	client := fake.NewSimpleClientset([]runtime.Object{pod}...)
	ctrl := NewK8sController(client, "devbox", nil, nil)

	// A loom-spawn-state entry with a blank Request (TimeoutMinutes == 0),
	// status "running", and StartedAt 24h ago — the exact shape of the wedged
	// entries, e.g. one the discovered-untracked-pod path rebuilt from labels.
	ctrl.mu.Lock()
	ctrl.spawns["spawn-zombie"] = &State{
		SpawnID:   "spawn-zombie",
		AgentID:   "agent-zombie",
		Status:    StatusRunning,
		PodName:   "spawn-pod-zombie",
		StartedAt: time.Now().Add(-24 * time.Hour),
		Request:   Request{}, // blank: TimeoutMinutes == 0
	}
	ctrl.mu.Unlock()

	var hookCalls []State
	ctrl.SetTerminalHook(func(_ context.Context, st State) error {
		hookCalls = append(hookCalls, st)
		return nil
	})

	if err := ctrl.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	state, ok := ctrl.Get("spawn-zombie")
	if !ok {
		t.Fatal("spawn-zombie not found after reconcile")
	}
	if state.Status != StatusFailed {
		t.Fatalf("status: got %q, want %q — blank-request zombie must be reaped by the absolute-age backstop", state.Status, StatusFailed)
	}
	if state.EndedAt == nil {
		t.Error("expected EndedAt to be stamped on the reaped zombie")
	}
	if state.Error == "" {
		t.Error("expected a deadline-exceeded error on the reaped zombie")
	}
	if len(hookCalls) != 1 {
		t.Fatalf("terminal hook fired %d times, want 1 (pod/pool slot must be released)", len(hookCalls))
	}
	if hookCalls[0].PodName != "spawn-pod-zombie" {
		t.Errorf("hook must carry the pod name so reap can delete it: got %q", hookCalls[0].PodName)
	}
}

// TestSpawnDeadlineExceeded locks the reaper's core predicate across the matrix
// that motivated the absolute-age floor: blank-request zombies get reaped,
// explicitly-longer spawns are honored (not reaped early), recent spawns are
// left alone, and a spawn with no age anchor is never touched.
func TestSpawnDeadlineExceeded(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	tests := []struct {
		name  string
		state *State
		want  bool
	}{
		{
			name: "blank request, 24h old — reaped by absolute floor",
			state: &State{
				Status:    StatusRunning,
				StartedAt: ago(24 * time.Hour),
				Request:   Request{}, // TimeoutMinutes == 0
			},
			want: true,
		},
		{
			name: "blank request, 30m old — within 65m floor, not yet reaped",
			state: &State{
				Status:    StatusRunning,
				StartedAt: ago(30 * time.Minute),
				Request:   Request{},
			},
			want: false,
		},
		{
			name: "blank request, just past 65m floor — reaped",
			state: &State{
				Status:    StatusRunning,
				StartedAt: ago(spawnAbsoluteMaxAge + reconcileDeadlineGrace + time.Minute),
				Request:   Request{},
			},
			want: true,
		},
		{
			name: "explicit 120m timeout, 90m old — honored, not reaped early",
			state: &State{
				Status:    StatusRunning,
				StartedAt: ago(90 * time.Minute),
				Request:   Request{TimeoutMinutes: 120},
			},
			want: false,
		},
		{
			name: "explicit 120m timeout, past its own deadline — reaped",
			state: &State{
				Status:    StatusRunning,
				StartedAt: ago(120*time.Minute + reconcileDeadlineGrace + time.Minute),
				Request:   Request{TimeoutMinutes: 120},
			},
			want: true,
		},
		{
			name: "negative timeout, 24h old — clamped to floor, reaped",
			state: &State{
				Status:    StatusRunning,
				StartedAt: ago(24 * time.Hour),
				Request:   Request{TimeoutMinutes: -5},
			},
			want: true,
		},
		{
			name: "zero StartedAt — unbounded, never reaped",
			state: &State{
				Status:  StatusRunning,
				Request: Request{}, // no age anchor
			},
			want: false,
		},
		{
			name:  "nil state — never reaped",
			state: nil,
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := spawnDeadlineExceeded(tt.state, now); got != tt.want {
				t.Errorf("spawnDeadlineExceeded = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPruneZeroMaxAgeIsNoOp protects against accidentally wiping the entire
// store via a misconfigured retention window.
func TestPruneZeroMaxAgeIsNoOp(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewK8sController(client, "devbox", nil, nil)
	now := time.Now()
	old := now.Add(-100 * time.Hour)
	ctrl.mu.Lock()
	ctrl.spawns["x"] = &State{SpawnID: "x", Status: StatusCompleted, EndedAt: &old}
	ctrl.mu.Unlock()

	if got := ctrl.Prune(context.Background(), 0); got != 0 {
		t.Fatalf("Prune(0): got %d, want 0", got)
	}
	if _, ok := ctrl.Get("x"); !ok {
		t.Fatal("Prune with zero maxAge must not evict anything")
	}
}
