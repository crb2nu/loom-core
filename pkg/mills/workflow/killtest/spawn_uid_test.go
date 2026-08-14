package killtest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func spawnPodListJSON(name, uid, phase string) string {
	return fmt.Sprintf(`{"items":[{"metadata":{"name":%q,"uid":%q},"spec":{"nodeName":"worker-1","containers":[{"image":"spawn:v1"}]},"status":{"phase":%q,"startTime":"2026-07-12T11:59:00Z","containerStatuses":[{"ready":true,"imageID":"spawn@sha256:123"}]}}]}`,
		name, uid, phase)
}

func TestParseSpawnPodSampleCapturesExactUIDAndAttribution(t *testing.T) {
	sample, err := parseSpawnPodSample(spawnPodListJSON("spawn-abc", "uid-1", "Running"), "spawn-abc")
	if err != nil {
		t.Fatalf("parseSpawnPodSample() error = %v", err)
	}
	if sample.Concurrent != 1 || len(sample.Names) != 1 || sample.Names[0] != "spawn-abc" || len(sample.Incarnations) != 1 {
		t.Fatalf("parseSpawnPodSample() = %+v", sample)
	}
	identity := sample.Incarnations[0]
	if identity.Name != "spawn-abc" || identity.UID != "uid-1" || identity.Node != "worker-1" ||
		identity.Image != "spawn:v1" || identity.ImageID != "spawn@sha256:123" || identity.StartedAt.IsZero() {
		t.Fatalf("spawn identity attribution incomplete: %+v", identity)
	}

	for name, raw := range map[string]string{
		"missing uid":  spawnPodListJSON("spawn-abc", "", "Running"),
		"wrong name":   spawnPodListJSON("spawn-other", "uid-1", "Running"),
		"invalid json": `{`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSpawnPodSample(raw, "spawn-abc"); err == nil {
				t.Fatal("expected fail-closed pod identity error")
			}
		})
	}
}

func TestAwaitTerminalAccumulatesPreCrashAndReplacementUIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"run":{"id":"wf-1","state":"done"},"steps":[]}`))
	}))
	defer server.Close()

	h := New(Config{OperatorURL: server.URL, PollInterval: time.Millisecond})
	preCrash := true
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		command := strings.Join(args, " ")
		switch {
		case strings.Contains(command, "--field-selector metadata.name=spawn-abc"):
			if !strings.HasSuffix(command, "-o json") {
				t.Fatalf("exact pod observation did not request full JSON: %s", command)
			}
			if preCrash {
				preCrash = false
				return spawnPodListJSON("spawn-abc", "uid-before", "Running"), nil
			}
			return spawnPodListJSON("spawn-abc", "uid-after", "Succeeded"), nil
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

	concurrent, names, err := h.CountSpawnPods(context.Background(), "abc")
	if err != nil || concurrent != 1 || len(names) != 1 {
		t.Fatalf("pre-crash CountSpawnPods() = %d, %v, %v", concurrent, names, err)
	}
	ev := Evidence{MaxConcurrentSpawnPods: concurrent, TotalSpawnPodNames: names}
	if err := h.AwaitTerminal(context.Background(), "wf-1", "abc", &ev); err != nil {
		t.Fatalf("AwaitTerminal() error = %v", err)
	}
	if len(ev.TotalSpawnPodIncarnations) != 2 {
		t.Fatalf("same-name replacement was not retained in evidence: %+v", ev.TotalSpawnPodIncarnations)
	}
	if ev.TotalSpawnPodIncarnations[0].UID != "uid-after" || ev.TotalSpawnPodIncarnations[1].UID != "uid-before" {
		t.Fatalf("unexpected stable UID ordering: %+v", ev.TotalSpawnPodIncarnations)
	}
}

func TestEvaluateRejectsSameNameReplacementUID(t *testing.T) {
	ev := passingEvidence()
	replacement := ev.TotalSpawnPodIncarnations[0]
	replacement.UID = "spawn-uid-2"
	replacement.StartedAt = replacement.StartedAt.Add(time.Minute)
	ev.TotalSpawnPodIncarnations = append(ev.TotalSpawnPodIncarnations, replacement)

	verdict := Evaluate(ev)
	if verdict.Pass1NoDoubleSpawn || verdict.Overall {
		t.Fatalf("delete/recreate under deterministic name must fail PASS-1: %+v", verdict)
	}
	if !strings.Contains(verdict.Pass1Reason, "UID incarnation") {
		t.Fatalf("failure reason should identify the UID proof: %q", verdict.Pass1Reason)
	}
}

func TestEvaluateRequiresObservedSpawnUID(t *testing.T) {
	for name, mutate := range map[string]func(*Evidence){
		"no incarnation": func(ev *Evidence) { ev.TotalSpawnPodIncarnations = nil },
		"blank uid": func(ev *Evidence) {
			ev.TotalSpawnPodIncarnations[0].UID = " "
		},
		"wrong name": func(ev *Evidence) {
			ev.TotalSpawnPodIncarnations[0].Name = "spawn-other"
		},
	} {
		t.Run(name, func(t *testing.T) {
			ev := passingEvidence()
			mutate(&ev)
			if verdict := Evaluate(ev); verdict.Pass1NoDoubleSpawn || verdict.Overall {
				t.Fatalf("incomplete exact pod identity must fail PASS-1: %+v", verdict)
			}
		})
	}
}
