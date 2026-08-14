package spawn

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// TestReconcileRecoveryAfterRestart simulates the stale-after-restart scenario:
// the controller starts with no in-memory state, but pods exist in the cluster.
// Reconcile should discover them and populate the state map.
func TestReconcileRecoveryAfterRestart(t *testing.T) {
	pods := []runtime.Object{
		makePod("spawn-pod-a", "spawn-aaa", "agent-a", corev1.PodRunning),
		makePod("spawn-pod-b", "spawn-bbb", "agent-b", corev1.PodSucceeded),
		makePod("spawn-pod-c", "spawn-ccc", "agent-c", corev1.PodFailed),
	}
	client := fake.NewSimpleClientset(pods...)
	ctrl := NewK8sController(client, "devbox", nil, nil)

	// Controller starts empty (simulating restart with no persisted state).
	if err := ctrl.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// All three pods should be discovered.
	list := ctrl.List()
	if len(list) != 3 {
		t.Fatalf("expected 3 spawns, got %d", len(list))
	}

	// Verify individual states.
	stateA, ok := ctrl.Get("spawn-aaa")
	if !ok {
		t.Fatal("spawn-aaa not found")
	}
	if stateA.Status != StatusRunning {
		t.Errorf("spawn-aaa status: got %q, want %q", stateA.Status, StatusRunning)
	}

	stateB, ok := ctrl.Get("spawn-bbb")
	if !ok {
		t.Fatal("spawn-bbb not found")
	}
	if stateB.Status != StatusCompleted {
		t.Errorf("spawn-bbb status: got %q, want %q", stateB.Status, StatusCompleted)
	}

	stateC, ok := ctrl.Get("spawn-ccc")
	if !ok {
		t.Fatal("spawn-ccc not found")
	}
	if stateC.Status != StatusFailed {
		t.Errorf("spawn-ccc status: got %q, want %q", stateC.Status, StatusFailed)
	}
}

// TestReconcileWithPersistedState simulates recovery from a store plus live
// pods. Persisted state should be reconciled against actual pod status.
func TestReconcileWithPersistedState(t *testing.T) {
	pod := makePod("spawn-pod-x", "spawn-xxx", "agent-x", corev1.PodRunning)
	client := fake.NewSimpleClientset([]runtime.Object{pod}...)

	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctrl := NewK8sController(client, "devbox", store, nil)

	// Persist a state entry that says "pending" but the pod is actually running.
	ctx := context.Background()
	_ = store.Save(ctx, &State{
		SpawnID: "spawn-xxx",
		AgentID: "agent-x",
		Status:  StatusPending,
		PodName: "spawn-pod-x",
	})

	// Recover from store then reconcile.
	if err := ctrl.RecoverFromStore(ctx); err != nil {
		t.Fatalf("RecoverFromStore: %v", err)
	}
	if err := ctrl.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	state, ok := ctrl.Get("spawn-xxx")
	if !ok {
		t.Fatal("spawn-xxx not found")
	}
	// Should have been updated to Running based on actual pod status.
	if state.Status != StatusRunning {
		t.Errorf("status: got %q, want %q", state.Status, StatusRunning)
	}
}

// TestReconcileWithStoreAndMissingPod simulates a persisted running state where
// the pod has been deleted externally. Reconcile should mark it as failed.
func TestReconcileWithStoreAndMissingPod(t *testing.T) {
	// No pods in cluster.
	client := fake.NewSimpleClientset()

	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctrl := NewK8sController(client, "devbox", store, nil)
	ctx := context.Background()

	// Persist a "running" state.
	_ = store.Save(ctx, &State{
		SpawnID: "spawn-missing",
		AgentID: "agent-missing",
		Status:  StatusRunning,
		PodName: "spawn-pod-missing",
	})

	if err := ctrl.RecoverFromStore(ctx); err != nil {
		t.Fatalf("RecoverFromStore: %v", err)
	}
	if err := ctrl.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	state, ok := ctrl.Get("spawn-missing")
	if !ok {
		t.Fatal("spawn-missing not found")
	}
	if state.Status != StatusFailed {
		t.Errorf("status: got %q, want %q", state.Status, StatusFailed)
	}
	if state.Error == "" {
		t.Error("expected error message")
	}
}

// TestReconcileReapsDeadlineExceededSpawn covers the mobile-hud-restart zombie
// path: a "running" spawn recovered from the store whose pod is still alive but
// whose request deadline (StartedAt + TimeoutMinutes + grace) has passed is
// marked failed and stops counting against ActiveCount — even though the pod
// still exists. Without the backstop the in-process watchdog died with the old
// process and never re-attached, so the spawn held its pool slot forever.
func TestReconcileReapsDeadlineExceededSpawn(t *testing.T) {
	// Pod is alive and Running in the cluster.
	pod := makePod("spawn-pod-zombie", "spawn-zombie", "agent-zombie", corev1.PodRunning)
	client := fake.NewSimpleClientset([]runtime.Object{pod}...)

	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctrl := NewK8sController(client, "devbox", store, nil)
	ctx := context.Background()

	// Persisted "running" spawn started 90 min ago with a 60-min timeout —
	// 25 min past deadline+grace.
	_ = store.Save(ctx, &State{
		SpawnID:   "spawn-zombie",
		AgentID:   "agent-zombie",
		Status:    StatusRunning,
		PodName:   "spawn-pod-zombie",
		StartedAt: time.Now().Add(-90 * time.Minute),
		Request:   Request{TimeoutMinutes: 60},
	})

	if err := ctrl.RecoverFromStore(ctx); err != nil {
		t.Fatalf("RecoverFromStore: %v", err)
	}
	if err := ctrl.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	state, ok := ctrl.Get("spawn-zombie")
	if !ok {
		t.Fatal("spawn-zombie not found")
	}
	if state.Status != StatusFailed {
		t.Errorf("status: got %q, want %q (deadline-exceeded spawn must be reaped)", state.Status, StatusFailed)
	}
	if state.Error == "" {
		t.Error("expected a deadline-exceeded error message")
	}
	if got := ctrl.ActiveCount(); got != 0 {
		t.Errorf("ActiveCount: got %d, want 0 (reaped spawn must not hold a slot)", got)
	}
}

// TestReconcileKeepsHealthyRunningSpawn verifies the deadline backstop does NOT
// reap a running spawn still within its effective deadline. This includes a
// spawn with no request timeout (zero TimeoutMinutes) that is still YOUNGER than
// the spawnAbsoluteMaxAge floor — young in-flight work is never killed. A stale
// zero-timeout spawn (older than the floor) IS now reaped; that path is covered
// by TestReconcileReapsUnboundedZombieViaStore and the spawnDeadlineExceeded
// table test, and is the whole point of the 2026-07-02 pool-wedge fix.
func TestReconcileKeepsHealthyRunningSpawn(t *testing.T) {
	cases := []struct {
		name    string
		started time.Duration // age of the spawn (negative offset)
		timeout int
	}{
		{"within deadline", -5 * time.Minute, 60},
		{"unbounded but within floor", -10 * time.Minute, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := makePod("spawn-pod-live", "spawn-live", "agent-live", corev1.PodRunning)
			client := fake.NewSimpleClientset([]runtime.Object{pod}...)
			store, err := NewFileStore(t.TempDir())
			if err != nil {
				t.Fatalf("NewFileStore: %v", err)
			}
			ctrl := NewK8sController(client, "devbox", store, nil)
			ctx := context.Background()

			_ = store.Save(ctx, &State{
				SpawnID:   "spawn-live",
				AgentID:   "agent-live",
				Status:    StatusRunning,
				PodName:   "spawn-pod-live",
				StartedAt: time.Now().Add(tc.started),
				Request:   Request{TimeoutMinutes: tc.timeout},
			})

			if err := ctrl.RecoverFromStore(ctx); err != nil {
				t.Fatalf("RecoverFromStore: %v", err)
			}
			if err := ctrl.Reconcile(ctx); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			state, _ := ctrl.Get("spawn-live")
			if state.Status != StatusRunning {
				t.Errorf("healthy running spawn was changed: got %q, want %q", state.Status, StatusRunning)
			}
			if got := ctrl.ActiveCount(); got != 1 {
				t.Errorf("ActiveCount: got %d, want 1", got)
			}
		})
	}
}

// TestReconcileReapsUnboundedZombieViaStore covers the exact 2026-07-02
// production route: a "running" spawn with EMPTY request metadata
// (TimeoutMinutes == 0) is recovered from loom-spawn-state on restart, its pod
// is still alive, and it has run far past the absolute-age floor. The old
// deadline path short-circuited on TimeoutMinutes <= 0 and left it running
// forever, holding a pool slot; the absolute-age backstop must now reap it so
// ActiveCount drops and the pool slot is freed.
func TestReconcileReapsUnboundedZombieViaStore(t *testing.T) {
	pod := makePod("spawn-pod-unbounded", "spawn-unbounded", "agent-unbounded", corev1.PodRunning)
	client := fake.NewSimpleClientset([]runtime.Object{pod}...)

	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctrl := NewK8sController(client, "devbox", store, nil)
	ctx := context.Background()

	// Persisted "running" spawn started 24h ago with a blank Request — no
	// timeout, no agent_type/project/task_description, exactly like the wedged
	// entries pinning the pool at cap 6.
	_ = store.Save(ctx, &State{
		SpawnID:   "spawn-unbounded",
		AgentID:   "agent-unbounded",
		Status:    StatusRunning,
		PodName:   "spawn-pod-unbounded",
		StartedAt: time.Now().Add(-24 * time.Hour),
		Request:   Request{}, // TimeoutMinutes == 0
	})

	if err := ctrl.RecoverFromStore(ctx); err != nil {
		t.Fatalf("RecoverFromStore: %v", err)
	}
	if err := ctrl.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	state, ok := ctrl.Get("spawn-unbounded")
	if !ok {
		t.Fatal("spawn-unbounded not found")
	}
	if state.Status != StatusFailed {
		t.Errorf("status: got %q, want %q (blank-request zombie must be reaped by the absolute-age backstop)", state.Status, StatusFailed)
	}
	if got := ctrl.ActiveCount(); got != 0 {
		t.Errorf("ActiveCount: got %d, want 0 (reaped zombie must free its pool slot)", got)
	}
}

// TestReconcileLoopStopsOnCancel verifies the background loop exits cleanly.
func TestReconcileLoopStopsOnCancel(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewK8sController(client, "devbox", nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	ctrl.StartReconcileLoop(ctx, 50*time.Millisecond)

	// Let a few ticks pass.
	time.Sleep(150 * time.Millisecond)
	cancel()

	// Give goroutine time to exit.
	time.Sleep(100 * time.Millisecond)
	// If the goroutine leaked, the test framework detects it.
}

// TestPodPhaseMapping verifies all PodPhase values map correctly.
func TestPodPhaseMapping(t *testing.T) {
	tests := []struct {
		phase  corev1.PodPhase
		status Status
	}{
		{corev1.PodPending, StatusPending},
		{corev1.PodRunning, StatusRunning},
		{corev1.PodSucceeded, StatusCompleted},
		{corev1.PodFailed, StatusFailed},
		{corev1.PodPhase("SomeOther"), StatusUnknown},
	}
	for _, tt := range tests {
		got := podPhaseToStatus(tt.phase)
		if got != tt.status {
			t.Errorf("podPhaseToStatus(%q): got %q, want %q", tt.phase, got, tt.status)
		}
	}
}

// TestReconcileMultiplePodUpdates verifies that running multiple reconcile
// cycles correctly tracks pod transitions (e.g., Pending -> Running -> Succeeded).
func TestReconcileMultiplePodUpdates(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spawn-pod-multi",
			Namespace: "devbox",
			Labels: map[string]string{
				ManagedByLabel: ManagedByValue,
				SpawnIDLabel:   "spawn-multi",
				AgentIDLabel:   "agent-multi",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}
	client := fake.NewSimpleClientset([]runtime.Object{pod}...)
	ctrl := NewK8sController(client, "devbox", nil, nil)
	ctx := context.Background()

	// First reconcile: discovers pending pod.
	if err := ctrl.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile 1: %v", err)
	}
	state, _ := ctrl.Get("spawn-multi")
	if state.Status != StatusPending {
		t.Errorf("after reconcile 1: got %q, want %q", state.Status, StatusPending)
	}

	// Simulate pod transitioning to Running.
	pod.Status.Phase = corev1.PodRunning
	_, _ = client.CoreV1().Pods("devbox").UpdateStatus(ctx, pod, metav1.UpdateOptions{})

	if err := ctrl.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile 2: %v", err)
	}
	state, _ = ctrl.Get("spawn-multi")
	if state.Status != StatusRunning {
		t.Errorf("after reconcile 2: got %q, want %q", state.Status, StatusRunning)
	}

	// Simulate pod completing.
	pod.Status.Phase = corev1.PodSucceeded
	_, _ = client.CoreV1().Pods("devbox").UpdateStatus(ctx, pod, metav1.UpdateOptions{})

	if err := ctrl.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile 3: %v", err)
	}
	state, _ = ctrl.Get("spawn-multi")
	if state.Status != StatusCompleted {
		t.Errorf("after reconcile 3: got %q, want %q", state.Status, StatusCompleted)
	}
	if state.EndedAt == nil {
		t.Error("expected EndedAt to be set on completion")
	}

	// After reaching terminal state, another reconcile should not change it.
	pod.Status.Phase = corev1.PodRunning // hypothetical revert
	_, _ = client.CoreV1().Pods("devbox").UpdateStatus(ctx, pod, metav1.UpdateOptions{})

	if err := ctrl.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile 4: %v", err)
	}
	state, _ = ctrl.Get("spawn-multi")
	if state.Status != StatusCompleted {
		t.Errorf("terminal state should be preserved: got %q, want %q", state.Status, StatusCompleted)
	}
}
