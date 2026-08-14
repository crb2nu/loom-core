package killtest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAwaitTerminalSamplesProcessesUntilExactPodDisappears(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"run":{"id":"wf-1","state":"done"},"steps":[]}`))
	}))
	defer server.Close()

	h := New(Config{OperatorURL: server.URL, PollInterval: time.Millisecond})
	var exactPodReads atomic.Int32
	var processProbes atomic.Int32
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		command := strings.Join(args, " ")
		switch {
		case strings.Contains(command, "--field-selector metadata.name=spawn-abc"):
			if exactPodReads.Add(1) <= 2 {
				return spawnPodListJSON("spawn-abc", "uid-1", "Running"), nil
			}
			return `{"items":[]}`, nil
		case strings.Contains(command, "mills-canary-process-probe"):
			processProbes.Add(1)
			return "SAMPLE\t42\t4200\tS\t41\t4100\tS\t42\t41\t-", nil
		case strings.Contains(command, "get configmap loom-spawn-state"):
			return testSpawnStateConfigMapJSON(map[string]string{
				"abc": `{"spawn_id":"abc","status":"completed","request":{}}`,
			}), nil
		case strings.Contains(command, "get pods -o json"):
			return `{"items":[]}`, nil
		default:
			return "", fmt.Errorf("unexpected kubectl call: %s", command)
		}
	}

	ev := Evidence{
		SpawnPodName: "spawn-abc",
		CanaryHoldInitial: CanaryHoldObservation{
			PodName: "spawn-abc", PID: 42, StartTimeTicks: 4200,
			DriverPID: 41, DriverStartTimeTicks: 4100,
		},
	}
	observer := startCrashWindowTestObserver(t, h, &ev)
	if err := h.AwaitTerminalWithProcessObserver(context.Background(), "wf-1", "abc", &ev, observer); err != nil {
		t.Fatalf("AwaitTerminal() error = %v; evidence=%+v", err, ev)
	}
	if processProbes.Load() < 1 || len(ev.PostCrashProcessSamples) < 1 {
		t.Fatalf("process probes=%d samples=%+v, want live crash-window coverage", processProbes.Load(), ev.PostCrashProcessSamples)
	}
	if ev.PostCrashProcessObservedEnd.IsZero() ||
		ev.PostCrashProcessObservedEnd.Before(ev.PostCrashProcessSamples[0].ObservedAt) {
		t.Fatalf("process observation did not close after the last sample: sample=%s end=%s",
			ev.PostCrashProcessSamples[0].ObservedAt, ev.PostCrashProcessObservedEnd)
	}
}

func TestAwaitTerminalRejectsPostCrashZombieImmediately(t *testing.T) {
	h := newPostCrashProcessHarness(t, "SAMPLE\t42\t4200\tZ\t41\t4100\tS\t77\t41\t42,88", nil)
	ev := postCrashProcessEvidenceFixture()

	observer, err := h.StartCanaryProcessObservation(
		context.Background(), "abc", ev.CanaryHoldInitial, time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), "zombie") {
		t.Fatalf("StartCanaryProcessObservation() error = %v, want zombie failure", err)
	}
	if observer == nil {
		t.Fatal("failed observer was not returned")
	}
	_ = observer.Record(&ev)
	if len(ev.PostCrashProcessSamples) != 1 || ev.PostCrashProcessSamples[0].HoldState != "Z" {
		t.Fatalf("violating process sample was not serialized: %+v", ev.PostCrashProcessSamples)
	}
	if len(ev.ObservationErrors) == 0 || !strings.Contains(ev.ObservationErrors[0], "crash-window process invariant") {
		t.Fatalf("runtime violation not preserved in observation errors: %v", ev.ObservationErrors)
	}
}

func TestAwaitTerminalFailsClosedWhenProcessProbeIsLost(t *testing.T) {
	h := newPostCrashProcessHarness(t, "", errors.New("exec stream lost"))
	ev := postCrashProcessEvidenceFixture()

	observer, err := h.StartCanaryProcessObservation(
		context.Background(), "abc", ev.CanaryHoldInitial, time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), "exec stream lost") {
		t.Fatalf("StartCanaryProcessObservation() error = %v, want probe failure", err)
	}
	if observer == nil {
		t.Fatal("failed observer was not returned")
	}
	_ = observer.Record(&ev)
	if len(ev.ObservationErrors) == 0 || !strings.Contains(ev.ObservationErrors[0], "probe crash-window processes") {
		t.Fatalf("probe loss not preserved in observation errors: %v", ev.ObservationErrors)
	}
}

func newPostCrashProcessHarness(t *testing.T, processOutput string, processErr error) *Harness {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"run":{"id":"wf-1","state":"running"},"steps":[]}`))
	}))
	t.Cleanup(server.Close)

	h := New(Config{OperatorURL: server.URL, PollInterval: time.Millisecond})
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		command := strings.Join(args, " ")
		switch {
		case strings.Contains(command, "--field-selector metadata.name=spawn-abc"):
			return spawnPodListJSON("spawn-abc", "uid-1", "Running"), nil
		case strings.Contains(command, "mills-canary-process-probe"):
			return processOutput, processErr
		case strings.Contains(command, "get configmap loom-spawn-state"):
			return testSpawnStateConfigMapJSON(map[string]string{
				"abc": `{"spawn_id":"abc","status":"running","request":{}}`,
			}), nil
		case strings.Contains(command, "get pods -o json"):
			return spawnPodListJSON("spawn-abc", "uid-1", "Running"), nil
		default:
			return "", fmt.Errorf("unexpected kubectl call: %s", command)
		}
	}
	return h
}

func postCrashProcessEvidenceFixture() Evidence {
	return Evidence{
		SpawnPodName: "spawn-abc",
		CanaryHoldInitial: CanaryHoldObservation{
			PodName: "spawn-abc", PID: 42, StartTimeTicks: 4200,
			DriverPID: 41, DriverStartTimeTicks: 4100,
		},
	}
}

func startCrashWindowTestObserver(t *testing.T, h *Harness, ev *Evidence) *CanaryProcessObserver {
	t.Helper()
	observer, err := h.StartCanaryProcessObservation(
		context.Background(), "abc", ev.CanaryHoldInitial, time.Now().UTC())
	if err != nil {
		t.Fatalf("start crash-window observer: %v", err)
	}
	ev.CrashAAt = time.Now().UTC()
	authorizationA, err := observer.AuthorizeActiveDelete(ev.CrashAAt)
	if err != nil {
		t.Fatalf("authorize CRASH A process boundary: %v", err)
	}
	ev.CrashAProcessAuthorization = authorizationA
	ev.CrashBAt = ev.CrashAAt.Add(time.Nanosecond)
	authorizationB, err := observer.AuthorizeActiveDelete(ev.CrashBAt)
	if err != nil {
		t.Fatalf("authorize CRASH B process boundary: %v", err)
	}
	ev.CrashBProcessAuthorization = authorizationB
	return observer
}
