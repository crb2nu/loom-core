package killtest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func watchedSpawnPod(name, uid, resourceVersion string) *corev1.Pod {
	started := metav1.NewTime(time.Now().UTC())
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid), ResourceVersion: resourceVersion},
		Spec:       corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Image: "spawn:v1"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, StartTime: &started,
			ContainerStatuses: []corev1.ContainerStatus{{Ready: true, ImageID: "spawn@sha256:abc"}},
		},
	}
}

func waitForObserverCandidates(t *testing.T, observer *SpawnPodObserver, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		observer.mu.Lock()
		got := len(observer.candidates)
		observer.mu.Unlock()
		if got >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("observer did not retain %d candidate(s)", want)
}

func terminalSpawnHistory(count int, resourceVersion string) []corev1.Pod {
	items := make([]corev1.Pod, count)
	for index := range items {
		pod := watchedSpawnPod(
			fmt.Sprintf("spawn-history-%05d", index),
			fmt.Sprintf("uid-history-%05d", index),
			resourceVersion,
		)
		pod.Status.Phase = corev1.PodSucceeded
		items[index] = *pod
	}
	return items
}

func TestSpawnNamespaceObserverUsesBoundedCompleteBaselineAndListRV(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	client.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, kruntime.Object, error) {
		listAction := action.(interface {
			GetListOptions() metav1.ListOptions
		})
		options := listAction.GetListOptions()
		if options.Limit != maxSpawnPodBaselineItems {
			t.Fatalf("baseline List limit = %d, want %d", options.Limit, maxSpawnPodBaselineItems)
		}
		if options.Continue != "" {
			t.Fatalf("baseline List continuation = %q, want empty", options.Continue)
		}
		baseline := watchedSpawnPod("spawn-old", "uid-old", "49")
		baseline.Status.Phase = corev1.PodSucceeded
		unrelated := watchedSpawnPod("api", "uid-api", "49")
		return true, &corev1.PodList{
			ListMeta: metav1.ListMeta{ResourceVersion: "50"},
			Items:    []corev1.Pod{*baseline, *unrelated},
		}, nil
	})
	stream := watch.NewRaceFreeFake()
	client.PrependWatchReactor("pods", func(action k8stesting.Action) (bool, watch.Interface, error) {
		watchAction := action.(interface {
			GetListOptions() metav1.ListOptions
		})
		options := watchAction.GetListOptions()
		if options.ResourceVersion != "50" {
			t.Fatalf("watch resourceVersion = %q, want baseline List rv 50", options.ResourceVersion)
		}
		if !options.AllowWatchBookmarks {
			t.Fatal("watch did not request bookmarks")
		}
		return true, stream, nil
	})

	h := New(Config{SpawnNS: "devbox"})
	h.kube = client
	h.kubeOnce.Do(func() {})
	observer, err := h.StartSpawnNamespaceObservation(context.Background())
	if err != nil {
		t.Fatalf("StartSpawnNamespaceObservation() error = %v", err)
	}
	if observer.initialRV != "50" {
		t.Fatalf("observer initial rv = %q, want 50", observer.initialRV)
	}
	if _, ok := observer.baselineSpawnUID["uid-old"]; !ok {
		t.Fatal("terminal spawn UID missing from baseline")
	}
	if _, ok := observer.baselineSpawnUID["uid-api"]; ok {
		t.Fatal("unrelated Pod UID unexpectedly entered spawn baseline")
	}
	if err := observer.Stop(nil); err != nil {
		t.Fatalf("observer.Stop() error = %v", err)
	}
}

func TestSpawnNamespaceObserverAcceptsExactBaselineCap(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, &corev1.PodList{
			ListMeta: metav1.ListMeta{ResourceVersion: "60"},
			Items:    terminalSpawnHistory(int(maxSpawnPodBaselineItems), "60"),
		}, nil
	})
	stream := watch.NewRaceFreeFake()
	client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, stream, nil
	})

	h := New(Config{SpawnNS: "devbox"})
	h.kube = client
	h.kubeOnce.Do(func() {})
	observer, err := h.StartSpawnNamespaceObservation(context.Background())
	if err != nil {
		t.Fatalf("StartSpawnNamespaceObservation() at exact cap error = %v", err)
	}
	if got := len(observer.baselineSpawnUID); got != int(maxSpawnPodBaselineItems) {
		t.Fatalf("baseline UID count = %d, want %d", got, maxSpawnPodBaselineItems)
	}
	if err := observer.Stop(nil); err != nil {
		t.Fatalf("observer.Stop() error = %v", err)
	}
}

func TestSpawnNamespaceObserverRejectsBaselineContinuation(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, &corev1.PodList{
			ListMeta: metav1.ListMeta{ResourceVersion: "70", Continue: "next-page"},
			Items:    terminalSpawnHistory(1, "70"),
		}, nil
	})
	watchCalled := false
	client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
		watchCalled = true
		return true, watch.NewRaceFreeFake(), nil
	})

	h := New(Config{SpawnNS: "devbox"})
	h.kube = client
	h.kubeOnce.Do(func() {})
	_, err := h.StartSpawnNamespaceObservation(context.Background())
	if err == nil || !strings.Contains(err.Error(), "continuation token") {
		t.Fatalf("StartSpawnNamespaceObservation() error = %v, want incomplete continuation rejection", err)
	}
	if watchCalled {
		t.Fatal("watch started after incomplete baseline List")
	}
}

func TestSpawnNamespaceObserverRejectsTerminalHistoryAboveCap(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, &corev1.PodList{
			ListMeta: metav1.ListMeta{ResourceVersion: "80"},
			Items:    terminalSpawnHistory(int(maxSpawnPodBaselineItems)+1, "80"),
		}, nil
	})
	watchCalled := false
	client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
		watchCalled = true
		return true, watch.NewRaceFreeFake(), nil
	})

	h := New(Config{SpawnNS: "devbox"})
	h.kube = client
	h.kubeOnce.Do(func() {})
	_, err := h.StartSpawnNamespaceObservation(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeding bounded item limit") {
		t.Fatalf("StartSpawnNamespaceObservation() error = %v, want terminal history overflow rejection", err)
	}
	if watchCalled {
		t.Fatal("watch started after over-limit baseline List")
	}
}

func TestSpawnPodObserverCapturesShortLivedSecondUIDBetweenPolls(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}}, nil
	})
	stream := watch.NewRaceFreeFake()
	client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, stream, nil
	})

	h := New(Config{SpawnNS: "devbox"})
	h.kube = client
	h.kubeOnce.Do(func() {})
	observer, err := h.StartSpawnPodObservation(context.Background(), "abc")
	if err != nil {
		t.Fatalf("StartSpawnPodObservation() error = %v", err)
	}
	ev := Evidence{}
	observer.RecordStart(&ev)
	ev.CrashAAt = time.Now().UTC()
	uid1 := watchedSpawnPod("spawn-abc", "uid-1", "11")
	stream.Add(uid1)
	deletedUID1 := uid1.DeepCopy()
	deletedUID1.ResourceVersion = "12"
	stream.Delete(deletedUID1)
	stream.Add(watchedSpawnPod("spawn-abc", "uid-2", "13"))

	deadline := time.Now().Add(time.Second)
	for len(h.observedSpawnPodIncarnations("abc")) != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	ev.CrashBAt = time.Now().UTC()
	if err := observer.Stop(&ev); err != nil {
		t.Fatalf("observer.Stop() error = %v", err)
	}
	if !ev.SpawnPodWatchContinuous || ev.SpawnPodWatchInitialRV != "10" ||
		len(ev.TotalSpawnPodIncarnations) != 2 {
		t.Fatalf("watch evidence did not retain both UIDs: %+v", ev)
	}
	if len(ev.SpawnPodWatchEvents) != 3 || ev.SpawnPodWatchEvents[1].Type != string(watch.Deleted) {
		t.Fatalf("watch evidence did not retain Added/Deleted/Added events: %+v", ev.SpawnPodWatchEvents)
	}
}

func TestSpawnNamespaceObserverBindsOnlyCanonicalDerivedName(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	client.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, kruntime.Object, error) {
		listAction := action.(k8stesting.ListAction)
		if fields := listAction.GetListRestrictions().Fields.String(); fields != "" {
			t.Fatalf("namespace baseline list unexpectedly used field selector %q", fields)
		}
		return true, &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "20"}}, nil
	})
	stream := watch.NewRaceFreeFake()
	client.PrependWatchReactor("pods", func(action k8stesting.Action) (bool, watch.Interface, error) {
		watchAction := action.(k8stesting.WatchAction)
		if fields := watchAction.GetWatchRestrictions().Fields.String(); fields != "" {
			t.Fatalf("namespace watch unexpectedly used field selector %q", fields)
		}
		return true, stream, nil
	})

	h := New(Config{SpawnNS: "devbox"})
	h.kube = client
	h.kubeOnce.Do(func() {})
	observer, err := h.StartSpawnNamespaceObservation(context.Background())
	if err != nil {
		t.Fatalf("StartSpawnNamespaceObservation() error = %v", err)
	}
	stream.Add(watchedSpawnPod("spawn-alternate", "uid-alternate", "21"))
	waitForObserverCandidates(t, observer, 1)

	err = observer.BindSpawnIdentity("canonical")
	if err == nil || !strings.Contains(err.Error(), "spawn-alternate") || !strings.Contains(err.Error(), "spawn-canonical") {
		t.Fatalf("BindSpawnIdentity() error = %v, want alternate/canonical name rejection", err)
	}
	ev := Evidence{}
	if stopErr := observer.Stop(&ev); stopErr == nil {
		t.Fatal("observer.Stop() unexpectedly accepted alternate spawn-like pod")
	}
	if len(ev.TotalSpawnPodIncarnations) != 1 || ev.TotalSpawnPodIncarnations[0].Name != "spawn-alternate" {
		t.Fatalf("alternate candidate was not retained in evidence: %+v", ev.TotalSpawnPodIncarnations)
	}
}

func TestSpawnNamespaceObserverFailsOnSecondPostBaselineUID(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, kruntime.Object, error) {
		baseline := watchedSpawnPod("spawn-old", "uid-baseline", "30")
		baseline.Status.Phase = corev1.PodSucceeded
		return true, &corev1.PodList{
			ListMeta: metav1.ListMeta{ResourceVersion: "30"},
			Items:    []corev1.Pod{*baseline},
		}, nil
	})
	stream := watch.NewRaceFreeFake()
	client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, stream, nil
	})

	h := New(Config{SpawnNS: "devbox"})
	h.kube = client
	h.kubeOnce.Do(func() {})
	observer, err := h.StartSpawnNamespaceObservation(context.Background())
	if err != nil {
		t.Fatalf("StartSpawnNamespaceObservation() error = %v", err)
	}
	uid1 := watchedSpawnPod("spawn-abc", "uid-1", "31")
	stream.Add(uid1)
	deletedUID1 := uid1.DeepCopy()
	deletedUID1.ResourceVersion = "32"
	stream.Delete(deletedUID1)
	stream.Add(watchedSpawnPod("spawn-abc", "uid-2", "33"))
	waitForObserverCandidates(t, observer, 2)

	if err := observer.AssertHealthy(); err == nil || !strings.Contains(err.Error(), "multiple post-baseline") {
		t.Fatalf("AssertHealthy() error = %v, want multiple UID rejection", err)
	}
	ev := Evidence{}
	if err := observer.Stop(&ev); err == nil {
		t.Fatal("observer.Stop() unexpectedly accepted a second UID")
	}
	if len(ev.TotalSpawnPodIncarnations) != 2 {
		t.Fatalf("watch evidence did not retain both post-baseline UIDs: %+v", ev.TotalSpawnPodIncarnations)
	}
}

func TestSpawnNamespaceObserverRejectsPreBindSpawnIDLabelDrift(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "35"}}, nil
	})
	stream := watch.NewRaceFreeFake()
	client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, stream, nil
	})

	h := New(Config{SpawnNS: "devbox"})
	h.kube = client
	h.kubeOnce.Do(func() {})
	observer, err := h.StartSpawnNamespaceObservation(context.Background())
	if err != nil {
		t.Fatalf("StartSpawnNamespaceObservation() error = %v", err)
	}
	pod := watchedSpawnPod("spawn-canonical", "uid-canonical", "36")
	pod.Labels = map[string]string{"loom.dev/spawn-id": "different-id"}
	stream.Add(pod)
	waitForObserverCandidates(t, observer, 1)

	err = observer.BindSpawnIdentity("canonical")
	if err == nil || !strings.Contains(err.Error(), "spawn-id label") || !strings.Contains(err.Error(), "different-id") {
		t.Fatalf("BindSpawnIdentity() error = %v, want label identity rejection", err)
	}
	ev := Evidence{}
	_ = observer.Stop(&ev)
	if len(ev.SpawnPodWatchEvents) != 1 || ev.SpawnPodWatchEvents[0].SpawnIDLabel == nil ||
		*ev.SpawnPodWatchEvents[0].SpawnIDLabel != "different-id" {
		t.Fatalf("spawn-id label identity was not retained: %+v", ev.SpawnPodWatchEvents)
	}
}

func TestSpawnNamespaceObserverRejectsConflictingIdentityForOneUID(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "37"}}, nil
	})
	stream := watch.NewRaceFreeFake()
	client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, stream, nil
	})

	h := New(Config{SpawnNS: "devbox"})
	h.kube = client
	h.kubeOnce.Do(func() {})
	observer, err := h.StartSpawnNamespaceObservation(context.Background())
	if err != nil {
		t.Fatalf("StartSpawnNamespaceObservation() error = %v", err)
	}
	first := watchedSpawnPod("spawn-abc", "uid-abc", "38")
	second := watchedSpawnPod("spawn-abc", "uid-abc", "39")
	second.Spec.Containers[0].Image = "spawn:v2"
	stream.Add(first)
	stream.Modify(second)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		err = observer.AssertHealthy()
		if err != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err == nil || !strings.Contains(err.Error(), "image changed") {
		t.Fatalf("AssertHealthy() error = %v, want same-UID identity conflict", err)
	}
	_ = observer.Stop(nil)
}

func TestSpawnNamespaceObserverRejectsActiveSpawnInBaseline(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, kruntime.Object, error) {
		active := watchedSpawnPod("spawn-raced", "uid-raced", "40")
		return true, &corev1.PodList{
			ListMeta: metav1.ListMeta{ResourceVersion: "40"},
			Items:    []corev1.Pod{*active},
		}, nil
	})

	h := New(Config{SpawnNS: "devbox"})
	h.kube = client
	h.kubeOnce.Do(func() {})
	_, err := h.StartSpawnNamespaceObservation(context.Background())
	if err == nil || !strings.Contains(err.Error(), "active spawn-related pod") {
		t.Fatalf("StartSpawnNamespaceObservation() error = %v, want raced active pod rejection", err)
	}
}
