package backend

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodReadyTrackerPhases(t *testing.T) {
	t0 := time.Unix(1, 0)
	short := time.Minute
	budgets := podReadyBudgets{attach: 5 * short, image: short, start: short}
	waiting := func(reason string) []corev1.ContainerStatus {
		return []corev1.ContainerStatus{{Name: "spawn", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: "detail"}}}}
	}
	event := func(reason string) []corev1.Event { return []corev1.Event{{Reason: reason}} }
	started := true

	tests := []struct {
		name         string
		observations []struct {
			at     time.Duration
			pod    *corev1.Pod
			events []corev1.Event
		}
		wantErr string
	}{
		{
			name: "delayed RWO detach then running",
			observations: []struct {
				at     time.Duration
				pod    *corev1.Pod
				events []corev1.Event
			}{
				{4 * short, &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: waiting("ContainerCreating")}}, event("FailedAttachVolume")},
				{4*short + 30*time.Second, &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}, event("SuccessfulAttachVolume")},
			},
		},
		{
			name: "attach budget exhausted",
			observations: []struct {
				at     time.Duration
				pod    *corev1.Pod
				events []corev1.Event
			}{
				{5 * short, &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}, event("FailedAttachVolume")},
			},
			wantErr: "attach_wait_exceeded (last event FailedAttachVolume)",
		},
		{
			name: "terminal image pull",
			observations: []struct {
				at     time.Duration
				pod    *corev1.Pod
				events []corev1.Event
			}{
				{10 * time.Second, &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: waiting("ImagePullBackOff")}}, event("Failed")},
			},
			wantErr: "image_pull_backoff (last event Failed)",
		},
		{
			name: "terminal init image pull",
			observations: []struct {
				at     time.Duration
				pod    *corev1.Pod
				events []corev1.Event
			}{
				{10 * time.Second, &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: waiting("ImagePullBackOff")}}, event("Failed")},
			},
			wantErr: "image_pull_backoff (last event Failed)",
		},
		{
			name: "transient pull gets its own budget",
			observations: []struct {
				at     time.Duration
				pod    *corev1.Pod
				events []corev1.Event
			}{
				{4 * short, &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: waiting("ContainerCreating")}}, event("Pulling")},
				{4*short + 50*time.Second, &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}, event("Started")},
			},
		},
		{
			name: "unschedulable fails immediately",
			observations: []struct {
				at     time.Duration
				pod    *corev1.Pod
				events []corev1.Event
			}{
				{10 * time.Second, &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: "no matching nodes"}}}}, event("FailedScheduling")},
			},
			wantErr: "unschedulable (last event FailedScheduling)",
		},
		{
			name: "started container crashes",
			observations: []struct {
				at     time.Duration
				pod    *corev1.Pod
				events []corev1.Event
			}{
				{10 * time.Second, &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{Name: "spawn", Started: &started, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2, Reason: "Error", StartedAt: metav1.NewTime(t0)}}}}}}, event("BackOff")},
			},
			wantErr: "container_crash (last event BackOff)",
		},
		{
			// The 2026-09-05 failure: images pulled, git-clone init container
			// still cloning three minutes in. Neither the image nor the start
			// budget may tick while init work is in progress.
			name: "init container cloning past the image and start budgets keeps waiting",
			observations: []struct {
				at     time.Duration
				pod    *corev1.Pod
				events []corev1.Event
			}{
				{30 * time.Second, cloningPod(), event("Pulled")},
				{30*time.Second + 2*short, cloningPod(), event("Started")},
				{30*time.Second + 3*short, cloningPod(), event("Started")},
				{4 * short, &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}, event("Started")},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := newPodReadyTracker(t0)
			var got error
			for _, observation := range tt.observations {
				done, err := tracker.observe(observation.pod, observation.events, t0.Add(observation.at), budgets)
				got = err
				if done {
					break
				}
			}
			if tt.wantErr == "" {
				if got != nil {
					t.Fatalf("unexpected error: %v", got)
				}
				return
			}
			if got == nil || !strings.Contains(got.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", got, tt.wantErr)
			}
		})
	}
}

func TestPodReadyTrackerTimeoutIncludesPhaseAndEvent(t *testing.T) {
	tracker := newPodReadyTracker(time.Unix(1, 0))
	tracker.phase = podReadyImage
	tracker.lastEvent = "Pulling"
	err := tracker.timeoutError(5 * time.Minute)
	for _, want := range []string{"image_pull_backoff", "last event Pulling", "5m0s", "image phase"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}

	tracker.phase = podReadyInit
	tracker.lastEvent = "Started"
	err = tracker.timeoutError(5 * time.Minute)
	for _, want := range []string{"init_wait_exceeded", "last event Started", "init phase"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

// cloningPod is a Pending pod whose git-clone init container is Running with
// its image pulled and the main container waiting on it — the shape a spawn
// or sandbox pod has for the whole clone.
func cloningPod() *corev1.Pod {
	return &corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "git-clone"}},
			Containers:     []corev1.Container{{Name: "spawn"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name: "git-clone", ImageID: "sha256:clone",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "spawn", ImageID: "sha256:spawn",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
			}},
		},
	}
}

// TestPodReadyTrackerInitBudgetWhenSet pins that an init budget, when a
// caller sets one, is enforced with its own reason — and that the default
// (zero) budget leaves init bounded only by the overall timeout.
func TestPodReadyTrackerInitBudgetWhenSet(t *testing.T) {
	t0 := time.Unix(1, 0)
	bounded := podReadyBudgets{attach: 5 * time.Minute, image: time.Minute, init: 90 * time.Second, start: time.Minute}
	tracker := newPodReadyTracker(t0)
	if done, err := tracker.observe(cloningPod(), nil, t0.Add(10*time.Second), bounded); done || err != nil {
		t.Fatalf("first observation done=%v err=%v", done, err)
	}
	if tracker.phase != podReadyInit {
		t.Fatalf("phase = %s, want init", tracker.phase)
	}
	done, err := tracker.observe(cloningPod(), nil, t0.Add(10*time.Second+2*time.Minute), bounded)
	if !done || err == nil || !strings.Contains(err.Error(), "init_wait_exceeded") {
		t.Fatalf("bounded init: done=%v err=%v, want init_wait_exceeded", done, err)
	}

	unbounded := podReadyBudgets{attach: 5 * time.Minute, image: time.Minute, start: time.Minute}
	tracker = newPodReadyTracker(t0)
	for _, at := range []time.Duration{10 * time.Second, 3 * time.Minute, 20 * time.Minute} {
		if done, err := tracker.observe(cloningPod(), nil, t0.Add(at), unbounded); done || err != nil {
			t.Fatalf("unbounded init at %s: done=%v err=%v", at, done, err)
		}
	}
	// Once init completes the start budget begins from that moment.
	running := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if done, err := tracker.observe(running, nil, t0.Add(21*time.Minute), unbounded); !done || err != nil {
		t.Fatalf("running after init: done=%v err=%v", done, err)
	}
}
