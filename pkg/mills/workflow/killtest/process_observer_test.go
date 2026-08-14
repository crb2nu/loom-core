package killtest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCanaryProcessObserverStartsSynchronouslyAndCapturesRolloutViolation(t *testing.T) {
	h := New(Config{ProcessPollInterval: time.Millisecond, ProcessMaxSampleGap: time.Second})
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "--field-selector metadata.name=spawn-abc") {
			return spawnPodListJSON("spawn-abc", "uid-1", "Running"), nil
		}
		return "", fmt.Errorf("unexpected kubectl call: %s", strings.Join(args, " "))
	}
	var mu sync.Mutex
	probes := 0
	h.processProbeFn = func(context.Context, string, int, uint64, int, uint64) (CanaryProcessSample, error) {
		mu.Lock()
		defer mu.Unlock()
		probes++
		sample := processObserverSample(time.Now().UTC())
		if probes == 2 {
			sample.HoldState = "Z"
			sample.LiveHoldPIDs = []int{77}
			sample.LiveDriverPIDs = []int{76}
			sample.ZombiePIDs = []int{42, 88}
		}
		return sample, nil
	}

	crashAt := time.Now().UTC()
	observer, err := h.StartCanaryProcessObservation(context.Background(), "abc", processObserverInitial(), crashAt)
	if err != nil {
		t.Fatalf("StartCanaryProcessObservation() error = %v", err)
	}
	mu.Lock()
	synchronousProbes := probes
	mu.Unlock()
	if synchronousProbes != 1 {
		t.Fatalf("observer returned before its first sample: probes=%d", synchronousProbes)
	}
	<-observer.done

	ev := Evidence{SpawnPodName: "spawn-abc", CrashBAt: crashAt, CanaryHoldInitial: processObserverInitial()}
	err = observer.Record(&ev)
	if err == nil || !strings.Contains(err.Error(), "zombie") {
		t.Fatalf("Record() error = %v, want rollout zombie failure", err)
	}
	if len(ev.PostCrashProcessSamples) != 2 || len(ev.PostCrashProcessSamples[1].ZombiePIDs) == 0 {
		t.Fatalf("violating sample was not preserved: %+v", ev.PostCrashProcessSamples)
	}
}

func TestCanaryProcessObserverTreatsExecNotFoundThenPodAbsenceAsCleanEnd(t *testing.T) {
	h := New(Config{ProcessPollInterval: time.Millisecond, ProcessMaxSampleGap: time.Second})
	var mu sync.Mutex
	exactReads := 0
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		if !strings.Contains(strings.Join(args, " "), "--field-selector metadata.name=spawn-abc") {
			return "", fmt.Errorf("unexpected kubectl call: %s", strings.Join(args, " "))
		}
		mu.Lock()
		defer mu.Unlock()
		exactReads++
		if exactReads == 1 {
			return spawnPodListJSON("spawn-abc", "uid-1", "Running"), nil
		}
		return `{"items":[]}`, nil
	}
	probes := 0
	h.processProbeFn = func(context.Context, string, int, uint64, int, uint64) (CanaryProcessSample, error) {
		probes++
		if probes == 1 {
			return processObserverSample(time.Now().UTC()), nil
		}
		return CanaryProcessSample{}, errors.New("pod not found")
	}

	crashAt := time.Now().UTC()
	observer, err := h.StartCanaryProcessObservation(context.Background(), "abc", processObserverInitial(), crashAt)
	if err != nil {
		t.Fatal(err)
	}
	<-observer.done
	ev := Evidence{SpawnPodName: "spawn-abc", CrashBAt: crashAt, CanaryHoldInitial: processObserverInitial()}
	if err := observer.Record(&ev); err != nil {
		t.Fatalf("pod-disappearance race failed closed incorrectly: %v", err)
	}
	if ev.PostCrashProcessObservedEnd.IsZero() || len(ev.PostCrashProcessSamples) != 1 {
		t.Fatalf("clean observation end was not recorded: %+v", ev)
	}
}

func TestCanaryProcessObserverAllowsCleanLiveToMissingThenPodAbsence(t *testing.T) {
	h := New(Config{ProcessPollInterval: time.Millisecond, ProcessMaxSampleGap: time.Second})
	var mu sync.Mutex
	exactReads := 0
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		if !strings.Contains(strings.Join(args, " "), "--field-selector metadata.name=spawn-abc") {
			return "", fmt.Errorf("unexpected kubectl call: %s", strings.Join(args, " "))
		}
		mu.Lock()
		defer mu.Unlock()
		exactReads++
		if exactReads <= 2 {
			return spawnPodListJSON("spawn-abc", "uid-1", "Running"), nil
		}
		return `{"items":[]}`, nil
	}
	probes := 0
	h.processProbeFn = func(context.Context, string, int, uint64, int, uint64) (CanaryProcessSample, error) {
		mu.Lock()
		defer mu.Unlock()
		probes++
		switch probes {
		case 1:
			return processObserverSample(time.Now().UTC()), nil
		case 2:
			return processObserverHoldMissingSample(time.Now().UTC()), nil
		default:
			return processObserverMissingSample(time.Now().UTC()), nil
		}
	}

	crashAt := time.Now().UTC()
	observer, err := h.StartCanaryProcessObservation(context.Background(), "abc", processObserverInitial(), crashAt)
	if err != nil {
		t.Fatal(err)
	}
	<-observer.done

	ev := Evidence{SpawnPodName: "spawn-abc", CrashBAt: crashAt, CanaryHoldInitial: processObserverInitial()}
	if err := observer.Record(&ev); err != nil {
		t.Fatalf("clean process exit failed closed: %v", err)
	}
	if len(ev.PostCrashProcessSamples) != 3 {
		t.Fatalf("process samples = %+v, want live, hold missing, then both missing", ev.PostCrashProcessSamples)
	}
	if intermediate := ev.PostCrashProcessSamples[1]; intermediate.HoldState != "MISSING" ||
		intermediate.DriverState == "MISSING" {
		t.Fatalf("intermediate completion sample = %+v", intermediate)
	}
	missing := ev.PostCrashProcessSamples[2]
	if missing.HoldState != "MISSING" || missing.HoldStartTimeTicks != 0 ||
		missing.DriverState != "MISSING" || missing.DriverStartTimeTicks != 0 ||
		len(missing.LiveHoldPIDs) != 0 || len(missing.LiveDriverPIDs) != 0 {
		t.Fatalf("terminal missing sample = %+v", missing)
	}
}

func TestCanaryProcessObserverPausedStartWaitsForActivation(t *testing.T) {
	h := New(Config{ProcessPollInterval: time.Millisecond, ProcessMaxSampleGap: time.Second})
	var mu sync.Mutex
	exactReads := 0
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		if !strings.Contains(strings.Join(args, " "), "--field-selector metadata.name=spawn-abc") {
			return "", fmt.Errorf("unexpected kubectl call: %s", strings.Join(args, " "))
		}
		mu.Lock()
		exactReads++
		mu.Unlock()
		return `{"items":[]}`, nil
	}
	probes := 0
	h.processProbeFn = func(context.Context, string, int, uint64, int, uint64) (CanaryProcessSample, error) {
		mu.Lock()
		defer mu.Unlock()
		probes++
		return processObserverSample(time.Now().UTC()), nil
	}

	crashAt := time.Now().UTC()
	observer, err := h.StartPausedCanaryProcessObservation(context.Background(), "abc", processObserverInitial(), crashAt)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	mu.Lock()
	readsBeforeActivation, probesBeforeActivation := exactReads, probes
	mu.Unlock()
	if readsBeforeActivation != 0 || probesBeforeActivation != 1 {
		t.Fatalf("paused observer performed periodic work: exact_reads=%d probes=%d", readsBeforeActivation, probesBeforeActivation)
	}
	if err := observer.AssertFreshForDelete(); err != nil {
		t.Fatalf("fresh paused observer rejected before delete: %v", err)
	}
	if err := observer.Activate(); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	<-observer.done

	ev := Evidence{SpawnPodName: "spawn-abc", CrashBAt: crashAt, CanaryHoldInitial: processObserverInitial()}
	if err := observer.Record(&ev); err != nil {
		t.Fatalf("activated observer failed: %v", err)
	}
	mu.Lock()
	readsAfterActivation := exactReads
	mu.Unlock()
	if readsAfterActivation != 1 || ev.PostCrashProcessObservedEnd.IsZero() {
		t.Fatalf("activation did not start exact-pod observation: reads=%d evidence=%+v", readsAfterActivation, ev)
	}
}

func TestCanaryProcessObserverRejectsMissingInitialProcessProof(t *testing.T) {
	h := New(Config{ProcessPollInterval: time.Millisecond, ProcessMaxSampleGap: time.Second})
	h.processProbeFn = func(context.Context, string, int, uint64, int, uint64) (CanaryProcessSample, error) {
		return processObserverMissingSample(time.Now().UTC()), nil
	}
	observer, err := h.StartPausedCanaryProcessObservation(
		context.Background(), "abc", processObserverInitial(), time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), "initial process sample") {
		t.Fatalf("StartPausedCanaryProcessObservation() error = %v, want missing-initial-proof rejection", err)
	}
	if observer == nil {
		t.Fatal("failed observer was not returned for evidence preservation")
	}
}

func TestCanaryProcessObserverUsesProbeStartForCadenceDeadline(t *testing.T) {
	const (
		maxGap       = 80 * time.Millisecond
		transportLag = 55 * time.Millisecond
	)
	h := New(Config{ProcessPollInterval: time.Millisecond, ProcessMaxSampleGap: maxGap})
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "--field-selector metadata.name=spawn-abc") {
			return spawnPodListJSON("spawn-abc", "uid-1", "Running"), nil
		}
		return "", fmt.Errorf("unexpected kubectl call: %s", strings.Join(args, " "))
	}
	var probes atomic.Int32
	var firstResponseAt time.Time
	h.processProbeFn = func(context.Context, string, int, uint64, int, uint64) (CanaryProcessSample, error) {
		call := probes.Add(1)
		sample := processObserverSample(time.Now().UTC())
		// Model a transport that captures the remote snapshot immediately, then
		// returns it late without honoring context cancellation.
		time.Sleep(transportLag)
		if call == 1 {
			firstResponseAt = time.Now().UTC()
		}
		if call > 2 {
			return CanaryProcessSample{}, errors.New("cadence deadline allowed a third probe")
		}
		return sample, nil
	}

	crashAt := time.Now().UTC()
	observer, err := h.StartCanaryProcessObservation(context.Background(), "abc", processObserverInitial(), crashAt)
	if err != nil {
		t.Fatalf("StartCanaryProcessObservation() error = %v", err)
	}
	select {
	case <-observer.done:
	case <-time.After(500 * time.Millisecond):
		observer.cancel()
		t.Fatal("observer did not enforce cadence across delayed probe response")
	}
	ev := Evidence{SpawnPodName: "spawn-abc", CrashBAt: crashAt, CanaryHoldInitial: processObserverInitial()}
	err = observer.Record(&ev)
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("Record() error = %v, want conservative cadence deadline", err)
	}
	if probes.Load() != 2 {
		t.Fatalf("process probes = %d, want deadline during second probe", probes.Load())
	}
	if len(ev.PostCrashProcessSamples) != 2 ||
		firstResponseAt.Sub(ev.PostCrashProcessSamples[0].ObservedAt) < transportLag/2 {
		t.Fatalf("first sample timestamp was not pinned to probe start: response=%s samples=%+v",
			firstResponseAt, ev.PostCrashProcessSamples)
	}
}

func TestCanaryProcessObserverPausedStopDoesNotBlock(t *testing.T) {
	h := New(Config{ProcessPollInterval: time.Millisecond, ProcessMaxSampleGap: time.Second})
	h.processProbeFn = func(context.Context, string, int, uint64, int, uint64) (CanaryProcessSample, error) {
		return processObserverSample(time.Now().UTC()), nil
	}
	crashAt := time.Now().UTC()
	observer, err := h.StartPausedCanaryProcessObservation(context.Background(), "abc", processObserverInitial(), crashAt)
	if err != nil {
		t.Fatal(err)
	}

	ev := Evidence{SpawnPodName: "spawn-abc", CrashBAt: crashAt, CanaryHoldInitial: processObserverInitial()}
	stopped := make(chan error, 1)
	go func() { stopped <- observer.StopAndRecord(&ev) }()
	select {
	case err := <-stopped:
		if err == nil || !strings.Contains(err.Error(), "stopped before exact pod deletion") {
			t.Fatalf("StopAndRecord() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("StopAndRecord blocked while observer was paused")
	}
}

func TestPausedProcessObserverRejectsStaleFinalFenceBeforeDelete(t *testing.T) {
	const maxGap = 20 * time.Millisecond
	h := New(Config{ProcessPollInterval: time.Millisecond, ProcessMaxSampleGap: maxGap})
	h.processProbeFn = func(context.Context, string, int, uint64, int, uint64) (CanaryProcessSample, error) {
		return processObserverSample(time.Now().UTC()), nil
	}
	crashAt := time.Now().UTC()
	observer, err := h.StartPausedCanaryProcessObservation(context.Background(), "abc", processObserverInitial(), crashAt)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * maxGap)
	if err := observer.AssertFreshForDelete(); err == nil || !strings.Contains(err.Error(), "sampling gap") {
		t.Fatalf("AssertFreshForDelete() error = %v, want stale-fence rejection", err)
	}
	select {
	case <-observer.done:
	case <-time.After(time.Second):
		t.Fatal("stale paused observer did not terminate")
	}
}

func TestProcessObserverActiveFreshnessRequiresCrashAActivation(t *testing.T) {
	h := New(Config{ProcessPollInterval: 100 * time.Millisecond, ProcessMaxSampleGap: time.Second})
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "--field-selector metadata.name=spawn-abc") {
			return spawnPodListJSON("spawn-abc", "uid-1", "Running"), nil
		}
		return "", fmt.Errorf("unexpected kubectl call")
	}
	h.processProbeFn = func(context.Context, string, int, uint64, int, uint64) (CanaryProcessSample, error) {
		return processObserverSample(time.Now().UTC()), nil
	}
	observer, err := h.StartPausedCanaryProcessObservation(
		context.Background(), "abc", processObserverInitial(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := observer.AssertActiveFreshForDelete(); err == nil || !strings.Contains(err.Error(), "not active") {
		t.Fatalf("paused observer authorized CRASH B: %v", err)
	}
	if err := observer.Activate(); err != nil {
		t.Fatal(err)
	}
	if err := observer.AssertActiveFreshForDelete(); err != nil {
		t.Fatalf("fresh CRASH-A observer rejected before CRASH B: %v", err)
	}
	_ = observer.StopAndRecord(&Evidence{})
}

func TestProcessObserverPreservesTransientDuplicateBetweenCrashes(t *testing.T) {
	h := New(Config{ProcessPollInterval: time.Millisecond, ProcessMaxSampleGap: time.Second})
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "--field-selector metadata.name=spawn-abc") {
			return spawnPodListJSON("spawn-abc", "uid-1", "Running"), nil
		}
		return "", fmt.Errorf("unexpected kubectl call")
	}
	var probes atomic.Int32
	h.processProbeFn = func(context.Context, string, int, uint64, int, uint64) (CanaryProcessSample, error) {
		sample := processObserverSample(time.Now().UTC())
		if probes.Add(1) == 2 {
			sample.LiveDriverPIDs = []int{41, 76}
		}
		return sample, nil
	}
	startedAt := time.Now().UTC()
	observer, err := h.StartCanaryProcessObservation(
		context.Background(), "abc", processObserverInitial(), startedAt)
	if err != nil {
		t.Fatal(err)
	}
	// The first synchronous sample precedes CRASH A. The second, transient
	// overlap models a replay that starts and exits before CRASH B setup.
	crashAAt := time.Now().UTC()
	<-observer.done
	ev := Evidence{
		SpawnPodName: "spawn-abc", CanaryHoldInitial: processObserverInitial(),
		CrashAAt: crashAAt, CrashBAt: crashAAt.Add(time.Second),
	}
	if err := observer.Record(&ev); err == nil || !strings.Contains(err.Error(), "overlapping executions") {
		t.Fatalf("transient A-to-B duplicate was erased: error=%v evidence=%+v", err, ev)
	}
	if len(ev.PostCrashProcessSamples) != 2 || len(ev.PostCrashProcessSamples[1].LiveDriverPIDs) != 2 {
		t.Fatalf("violating transient sample was not preserved: %+v", ev.PostCrashProcessSamples)
	}
}

func TestProcessObserverRejectsCompletedExecutionBeforeCrashBDelete(t *testing.T) {
	h := New(Config{ProcessPollInterval: 50 * time.Millisecond, ProcessMaxSampleGap: time.Second})
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "--field-selector metadata.name=spawn-abc") {
			return spawnPodListJSON("spawn-abc", "uid-1", "Running"), nil
		}
		return "", fmt.Errorf("unexpected kubectl call")
	}
	secondSample := make(chan struct{})
	var once sync.Once
	var probes atomic.Int32
	h.processProbeFn = func(context.Context, string, int, uint64, int, uint64) (CanaryProcessSample, error) {
		if probes.Add(1) == 1 {
			return processObserverSample(time.Now().UTC()), nil
		}
		once.Do(func() { close(secondSample) })
		return processObserverMissingSample(time.Now().UTC()), nil
	}
	observer, err := h.StartCanaryProcessObservation(
		context.Background(), "abc", processObserverInitial(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondSample:
	case <-time.After(time.Second):
		t.Fatal("observer did not record completed execution")
	}
	time.Sleep(5 * time.Millisecond)
	if err := observer.AssertActiveFreshForDelete(); err == nil || !strings.Contains(err.Error(), "execution ended") {
		t.Fatalf("completed execution authorized CRASH B: %v", err)
	}
	_ = observer.StopAndRecord(&Evidence{})
}

func TestCanaryProcessObserverRejectsOverdueInFlightProbe(t *testing.T) {
	const maxGap = 30 * time.Millisecond
	h := New(Config{ProcessPollInterval: time.Millisecond, ProcessMaxSampleGap: maxGap})
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "--field-selector metadata.name=spawn-abc") {
			return spawnPodListJSON("spawn-abc", "uid-1", "Running"), nil
		}
		return "", fmt.Errorf("unexpected kubectl call")
	}

	blocked := make(chan bool, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseProbe := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseProbe()
	probes := 0
	h.processProbeFn = func(ctx context.Context, _ string, _ int, _ uint64, _ int, _ uint64) (CanaryProcessSample, error) {
		probes++
		if probes == 1 {
			return processObserverSample(time.Now().UTC()), nil
		}
		deadline, hasDeadline := ctx.Deadline()
		remaining := time.Until(deadline)
		blocked <- hasDeadline && remaining > 0 && remaining <= maxGap
		<-release
		return processObserverSample(time.Now().UTC()), nil
	}

	crashAt := time.Now().UTC()
	observer, err := h.StartCanaryProcessObservation(context.Background(), "abc", processObserverInitial(), crashAt)
	if err != nil {
		t.Fatal(err)
	}
	var hasDeadline bool
	select {
	case hasDeadline = <-blocked:
	case <-time.After(time.Second):
		t.Fatal("observer did not enter its second process probe")
	}
	time.Sleep(2 * maxGap)

	ev := Evidence{SpawnPodName: "spawn-abc", CrashBAt: crashAt, CanaryHoldInitial: processObserverInitial()}
	activeErr := observer.AssertActiveFreshForDelete()
	recordErr := observer.Record(&ev)
	releaseProbe()
	select {
	case <-observer.done:
	case <-time.After(time.Second):
		t.Fatal("observer did not stop after overdue probe cancellation")
	}
	if !hasDeadline {
		t.Fatal("periodic process probe context had no deadline")
	}
	if activeErr == nil || !strings.Contains(activeErr.Error(), "sampling gap") ||
		!strings.Contains(activeErr.Error(), "in-flight") {
		t.Fatalf("AssertActiveFreshForDelete() error = %v, want overdue in-flight sampling failure", activeErr)
	}
	if recordErr == nil || !strings.Contains(recordErr.Error(), "sampling gap") ||
		!strings.Contains(recordErr.Error(), "in-flight") {
		t.Fatalf("Record() error = %v, want overdue in-flight sampling failure", recordErr)
	}
}

// spawnPodTerminatingJSON lists a pod that still exists but is mid-teardown
// (deletionTimestamp set), so SpawnPodStatus keeps it in Names while
// SpawnPodTornDown reports it terminal.
func spawnPodTerminatingJSON(name, uid string) string {
	return fmt.Sprintf(`{"items":[{"metadata":{"name":%q,"uid":%q,"deletionTimestamp":"2026-07-12T12:01:00Z"},"spec":{"nodeName":"worker-1","containers":[{"image":"spawn:v1"}]},"status":{"phase":"Running","startTime":"2026-07-12T11:59:00Z","containerStatuses":[{"ready":false,"imageID":"spawn@sha256:123"}]}}]}`,
		name, uid)
}

func TestCanaryProcessObserverFailsClosedOnProbeErrorWhilePodStaysAlive(t *testing.T) {
	h := New(Config{ProcessPollInterval: time.Millisecond, ProcessMaxSampleGap: time.Second})
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		if !strings.Contains(strings.Join(args, " "), "--field-selector metadata.name=spawn-abc") {
			return "", fmt.Errorf("unexpected kubectl call: %s", strings.Join(args, " "))
		}
		// The exact pod is alive and fully running for every read, including
		// both authoritative rechecks after the probe fails.
		return spawnPodListJSON("spawn-abc", "uid-1", "Running"), nil
	}
	probes := 0
	h.processProbeFn = func(context.Context, string, int, uint64, int, uint64) (CanaryProcessSample, error) {
		probes++
		if probes == 1 {
			return processObserverSample(time.Now().UTC()), nil
		}
		return CanaryProcessSample{}, errors.New("RBAC forbidden")
	}

	crashAt := time.Now().UTC()
	observer, err := h.StartCanaryProcessObservation(context.Background(), "abc", processObserverInitial(), crashAt)
	if err != nil {
		t.Fatal(err)
	}
	<-observer.done
	ev := Evidence{SpawnPodName: "spawn-abc", CrashBAt: crashAt, CanaryHoldInitial: processObserverInitial()}
	// A probe error while the exact pod is authoritatively still alive must
	// fail closed, and the original cause must survive in the message.
	if err := observer.Record(&ev); err == nil ||
		!strings.Contains(err.Error(), "still exists") ||
		!strings.Contains(err.Error(), "RBAC forbidden") {
		t.Fatalf("probe failure on a live pod was not failed closed: error=%v evidence=%+v", err, ev)
	}
}

func TestCanaryProcessObserverTreatsProbeErrorDuringTeardownAsCleanEnd(t *testing.T) {
	h := New(Config{ProcessPollInterval: time.Millisecond, ProcessMaxSampleGap: time.Second})
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		if !strings.Contains(strings.Join(args, " "), "--field-selector metadata.name=spawn-abc") {
			return "", fmt.Errorf("unexpected kubectl call: %s", strings.Join(args, " "))
		}
		// The pod is listed but terminating for every read: the presence
		// check still sees it, and the teardown recheck confirms the end.
		return spawnPodTerminatingJSON("spawn-abc", "uid-1"), nil
	}
	probes := 0
	h.processProbeFn = func(context.Context, string, int, uint64, int, uint64) (CanaryProcessSample, error) {
		probes++
		if probes == 1 {
			return processObserverSample(time.Now().UTC()), nil
		}
		// The signature the in-cluster gate hit: kubelet SIGKILLs the exec'd
		// probe as the canary pod terminates.
		return CanaryProcessSample{}, errors.New("command terminated with exit code 137")
	}

	crashAt := time.Now().UTC()
	observer, err := h.StartCanaryProcessObservation(context.Background(), "abc", processObserverInitial(), crashAt)
	if err != nil {
		t.Fatal(err)
	}
	<-observer.done
	ev := Evidence{SpawnPodName: "spawn-abc", CrashBAt: crashAt, CanaryHoldInitial: processObserverInitial()}
	if err := observer.Record(&ev); err != nil {
		t.Fatalf("probe error during confirmed teardown failed closed: %v", err)
	}
	if ev.PostCrashProcessObservedEnd.IsZero() || len(ev.PostCrashProcessSamples) != 1 {
		t.Fatalf("teardown was not recorded as a clean observation end: %+v", ev)
	}
}

func TestCanaryProcessObserverRejectsCadenceGap(t *testing.T) {
	h := New(Config{ProcessPollInterval: 20 * time.Millisecond, ProcessMaxSampleGap: 5 * time.Millisecond})
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "--field-selector metadata.name=spawn-abc") {
			return spawnPodListJSON("spawn-abc", "uid-1", "Running"), nil
		}
		return "", fmt.Errorf("unexpected kubectl call")
	}
	h.processProbeFn = func(context.Context, string, int, uint64, int, uint64) (CanaryProcessSample, error) {
		return processObserverSample(time.Now().UTC()), nil
	}

	crashAt := time.Now().UTC()
	observer, err := h.StartCanaryProcessObservation(context.Background(), "abc", processObserverInitial(), crashAt)
	if err == nil || observer != nil || !strings.Contains(err.Error(), "invalid cadence") {
		// Poll >= max gap is invalid up front; an unprovable configured cadence
		// must never authorize CRASH B.
		t.Fatalf("StartCanaryProcessObservation() observer=%v error=%v", observer, err)
	}
}

func TestCanaryProcessObserverRejectsConfiguredGapAboveEvidenceContract(t *testing.T) {
	h := New(Config{
		ProcessPollInterval: time.Millisecond,
		ProcessMaxSampleGap: ProcessEvidenceMaxSampleGap + time.Millisecond,
	})
	observer, err := h.StartCanaryProcessObservation(
		context.Background(), "abc", processObserverInitial(), time.Now().UTC())
	if err == nil || observer != nil || !strings.Contains(err.Error(), "invalid cadence") {
		t.Fatalf("StartCanaryProcessObservation() observer=%v error=%v", observer, err)
	}
}

func TestCanaryProcessObserverRejectsRuntimeSamplingGap(t *testing.T) {
	h := New(Config{ProcessPollInterval: time.Millisecond, ProcessMaxSampleGap: 20 * time.Millisecond})
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "--field-selector metadata.name=spawn-abc") {
			return spawnPodListJSON("spawn-abc", "uid-1", "Running"), nil
		}
		return "", fmt.Errorf("unexpected kubectl call")
	}
	probes := 0
	h.processProbeFn = func(context.Context, string, int, uint64, int, uint64) (CanaryProcessSample, error) {
		probes++
		if probes > 1 {
			time.Sleep(40 * time.Millisecond)
		}
		return processObserverSample(time.Now().UTC()), nil
	}

	crashAt := time.Now().UTC()
	observer, err := h.StartCanaryProcessObservation(context.Background(), "abc", processObserverInitial(), crashAt)
	if err != nil {
		t.Fatal(err)
	}
	<-observer.done
	ev := Evidence{SpawnPodName: "spawn-abc", CrashBAt: crashAt, CanaryHoldInitial: processObserverInitial()}
	if err := observer.Record(&ev); err == nil || !strings.Contains(err.Error(), "sampling gap") {
		t.Fatalf("runtime cadence gap accepted: error=%v evidence=%+v", err, ev)
	}
	if len(ev.PostCrashProcessSamples) != 2 {
		t.Fatalf("gap-closing sample was not preserved: %+v", ev.PostCrashProcessSamples)
	}
}

func TestValidatePostCrashProcessSampleStrictMissingIdentity(t *testing.T) {
	ev := Evidence{SpawnPodName: "spawn-abc", CanaryHoldInitial: processObserverInitial()}
	if err := validatePostCrashProcessSample(ev, 0, processObserverMissingSample(time.Now().UTC())); err != nil {
		t.Fatalf("valid terminal missing sample rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*CanaryProcessSample)
	}{
		{name: "missing hold retains starttime", mutate: func(sample *CanaryProcessSample) {
			sample.HoldStartTimeTicks = 4200
		}},
		{name: "missing driver retains starttime", mutate: func(sample *CanaryProcessSample) {
			sample.DriverStartTimeTicks = 4100
		}},
		{name: "missing hold remains inventoried", mutate: func(sample *CanaryProcessSample) {
			sample.LiveHoldPIDs = []int{42}
		}},
		{name: "missing driver remains inventoried", mutate: func(sample *CanaryProcessSample) {
			sample.LiveDriverPIDs = []int{41}
		}},
		{name: "driver disappears before hold", mutate: func(sample *CanaryProcessSample) {
			sample.HoldState = "S"
			sample.HoldStartTimeTicks = 4200
			sample.LiveHoldPIDs = []int{42}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sample := processObserverMissingSample(time.Now().UTC())
			tt.mutate(&sample)
			if err := validatePostCrashProcessSample(ev, 0, sample); err == nil {
				t.Fatalf("invalid missing sample accepted: %+v", sample)
			}
		})
	}
}

// Both v5 gate attempts died 2/3 here: one kubectl observation call exceeded
// its deadline during the third consecutive dual-crash cycle and the observer
// aborted an otherwise contract-clean window. A transport failure must retry
// on the next tick; the sampling-gap contract stays the fail-closed arbiter.
func TestCanaryProcessObserverRetriesTransientTransportFailure(t *testing.T) {
	h := New(Config{ProcessPollInterval: time.Millisecond, ProcessMaxSampleGap: time.Second})
	var reads atomic.Int64
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		if !strings.Contains(strings.Join(args, " "), "--field-selector metadata.name=spawn-abc") {
			return "", fmt.Errorf("unexpected kubectl call: %s", strings.Join(args, " "))
		}
		switch reads.Add(1) {
		case 2, 3:
			// Two consecutive flakes: the exact-pod read killed on
			// deadline, exactly the v5c run-3 signature.
			return "", errors.New("signal: killed")
		case 6:
			return `{"items":[]}`, nil
		default:
			return spawnPodListJSON("spawn-abc", "uid-1", "Running"), nil
		}
	}
	h.processProbeFn = func(context.Context, string, int, uint64, int, uint64) (CanaryProcessSample, error) {
		return processObserverSample(time.Now().UTC()), nil
	}

	crashAt := time.Now().UTC()
	observer, err := h.StartCanaryProcessObservation(context.Background(), "abc", processObserverInitial(), crashAt)
	if err != nil {
		t.Fatal(err)
	}
	<-observer.done
	ev := Evidence{SpawnPodName: "spawn-abc", CrashBAt: crashAt, CanaryHoldInitial: processObserverInitial()}
	if err := observer.Record(&ev); err != nil {
		t.Fatalf("transient transport failure aborted the window: %v", err)
	}
	if len(ev.ObservationErrors) != 0 {
		t.Fatalf("transient failures must not become observation errors: %v", ev.ObservationErrors)
	}
	if ev.PostCrashProcessObservedEnd.IsZero() {
		t.Fatalf("window did not end cleanly after retries: %+v", ev)
	}
	if ev.PostCrashProcessTransientFailureCount != 2 || len(ev.PostCrashProcessTransientFailures) != 2 {
		t.Fatalf("transient failures not recorded honestly: count=%d stored=%v",
			ev.PostCrashProcessTransientFailureCount, ev.PostCrashProcessTransientFailures)
	}
	if len(ev.PostCrashProcessSamples) < 2 {
		t.Fatalf("expected samples on both sides of the flakes, got %d", len(ev.PostCrashProcessSamples))
	}
}

// v6d run 1: a single HUNG probe exec consumed the entire sampling-gap
// budget, so the transient retry never got room and the window breached.
// The per-attempt timeout must kill a hung call early so the next tick's
// retry can complete a sample inside the gap.
func TestCanaryProcessObserverBoundsHungProbeAttempt(t *testing.T) {
	h := New(Config{
		ProcessPollInterval:        time.Millisecond,
		ProcessMaxSampleGap:        time.Second,
		ProcessProbeAttemptTimeout: 20 * time.Millisecond,
	})
	var probes atomic.Int64
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		if !strings.Contains(strings.Join(args, " "), "--field-selector metadata.name=spawn-abc") {
			return "", fmt.Errorf("unexpected kubectl call: %s", strings.Join(args, " "))
		}
		// The pod stays authoritatively alive until the post-hang retry
		// sample has landed (probe 1 is the synchronous initial sample,
		// probe 3 hangs, probe 4 is the retry), then disappears cleanly.
		if probes.Load() >= 4 {
			return `{"items":[]}`, nil
		}
		return spawnPodListJSON("spawn-abc", "uid-1", "Running"), nil
	}
	h.processProbeFn = func(ctx context.Context, _ string, _ int, _ uint64, _ int, _ uint64) (CanaryProcessSample, error) {
		if probes.Add(1) == 3 {
			// Hang until the ATTEMPT deadline kills the call; without
			// the attempt bound this would block through the whole
			// sample-gap budget and breach it.
			<-ctx.Done()
			return CanaryProcessSample{}, ctx.Err()
		}
		return processObserverSample(time.Now().UTC()), nil
	}

	crashAt := time.Now().UTC()
	observer, err := h.StartCanaryProcessObservation(context.Background(), "abc", processObserverInitial(), crashAt)
	if err != nil {
		t.Fatal(err)
	}
	<-observer.done
	ev := Evidence{SpawnPodName: "spawn-abc", CrashBAt: crashAt, CanaryHoldInitial: processObserverInitial()}
	if err := observer.Record(&ev); err != nil {
		t.Fatalf("hung probe attempt aborted the window: %v", err)
	}
	if len(ev.ObservationErrors) != 0 {
		t.Fatalf("hung attempt must be transient, got observation errors: %v", ev.ObservationErrors)
	}
	if ev.PostCrashProcessTransientFailureCount == 0 {
		t.Fatal("hung attempt was not recorded as a transient failure")
	}
	if len(ev.PostCrashProcessSamples) < 3 {
		t.Fatalf("post-hang retry sample did not land inside the gap: samples=%d", len(ev.PostCrashProcessSamples))
	}
	if ev.PostCrashProcessObservedEnd.IsZero() {
		t.Fatalf("window did not end cleanly after the hung attempt: %+v", ev)
	}
}

// A persistent transport outage must still fail closed: with every attempt
// failing, no sample completes inside ProcessMaxSampleGap and beginSample
// reports the gap breach as a fatal observation error.
func TestCanaryProcessObserverPersistentTransportFailureBreachesGap(t *testing.T) {
	h := New(Config{ProcessPollInterval: time.Millisecond, ProcessMaxSampleGap: 30 * time.Millisecond})
	var reads atomic.Int64
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		if reads.Add(1) == 1 {
			return spawnPodListJSON("spawn-abc", "uid-1", "Running"), nil
		}
		return "", errors.New("signal: killed")
	}
	probes := 0
	h.processProbeFn = func(context.Context, string, int, uint64, int, uint64) (CanaryProcessSample, error) {
		probes++
		if probes == 1 {
			return processObserverSample(time.Now().UTC()), nil
		}
		return CanaryProcessSample{}, errors.New("probe should not be reached after pod reads fail")
	}

	crashAt := time.Now().UTC()
	observer, err := h.StartCanaryProcessObservation(context.Background(), "abc", processObserverInitial(), crashAt)
	if err != nil {
		t.Fatal(err)
	}
	<-observer.done
	ev := Evidence{SpawnPodName: "spawn-abc", CrashBAt: crashAt, CanaryHoldInitial: processObserverInitial()}
	_ = observer.Record(&ev)
	if len(ev.ObservationErrors) == 0 {
		t.Fatalf("persistent outage must fail closed with a gap breach, got none (transients=%d)",
			ev.PostCrashProcessTransientFailureCount)
	}
	found := false
	for _, message := range ev.ObservationErrors {
		if strings.Contains(message, "sampling gap") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a sampling-gap observation error, got %v", ev.ObservationErrors)
	}
	if ev.PostCrashProcessTransientFailureCount == 0 {
		t.Fatal("retried attempts should be recorded as transient failures")
	}
}

func processObserverInitial() CanaryHoldObservation {
	return CanaryHoldObservation{
		PodName: "spawn-abc", PID: 42, StartTimeTicks: 4200,
		DriverPID: 41, DriverStartTimeTicks: 4100, Seconds: 90,
		ObservedAt: time.Now().UTC().Add(-time.Second),
	}
}

func processObserverSample(observedAt time.Time) CanaryProcessSample {
	return CanaryProcessSample{
		PodName: "spawn-abc", ObservedAt: observedAt, CompletedAt: observedAt.Add(time.Nanosecond),
		HoldPID: 42, HoldStartTimeTicks: 4200, HoldState: "S",
		DriverPID: 41, DriverStartTimeTicks: 4100, DriverState: "S",
		LiveHoldPIDs: []int{42}, LiveDriverPIDs: []int{41}, ZombiePIDs: []int{},
	}
}

func processObserverMissingSample(observedAt time.Time) CanaryProcessSample {
	return CanaryProcessSample{
		PodName: "spawn-abc", ObservedAt: observedAt, CompletedAt: observedAt.Add(time.Nanosecond),
		HoldPID: 42, HoldState: "MISSING",
		DriverPID: 41, DriverState: "MISSING",
		LiveHoldPIDs: []int{}, LiveDriverPIDs: []int{}, ZombiePIDs: []int{},
	}
}

func processObserverHoldMissingSample(observedAt time.Time) CanaryProcessSample {
	return CanaryProcessSample{
		PodName: "spawn-abc", ObservedAt: observedAt, CompletedAt: observedAt.Add(time.Nanosecond),
		HoldPID: 42, HoldState: "MISSING",
		DriverPID: 41, DriverStartTimeTicks: 4100, DriverState: "S",
		LiveHoldPIDs: []int{}, LiveDriverPIDs: []int{41}, ZombiePIDs: []int{},
	}
}
