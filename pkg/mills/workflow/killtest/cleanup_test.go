package killtest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func cleanupSpawnConfigMapJSON(status string) string {
	state, _ := json.Marshal(map[string]any{
		"spawn_id": "abc",
		"status":   status,
		"request": map[string]string{
			"branch": "mills-wf/wf-cleanup", "idempotency_key": "key",
		},
	})
	return testSpawnStateConfigMapJSON(map[string]string{"abc": string(state)})
}

func TestCleanupRunStopsExactSpawnFailsJournalAndVerifiesTerminal(t *testing.T) {
	var mu sync.Mutex
	runState := "running"
	spawnStatus := "running"
	spawnActive := true
	stopCalls, failCalls := 0, 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/mills/safety/quiescence":
			w.Header().Set("Cache-Control", "no-store")
			_, _ = fmt.Fprintf(w, `{"observed_at":%q,"quiescent":true,"counts":{},"in_memory":{"admission_closed":true,"policy_generation":2,"sources_ready":true,"sample_stable":true,"wiring_required":true,"activity_sources":6,"source_generation":3,"source_operations":{"reconciler":0,"pipeline":0,"cross_run":0,"council":0,"canary":0,"workflow":0},"source_run_ids":{"workflow":[]}}}`,
				time.Now().UTC().Format(time.RFC3339Nano))
		case r.Method == http.MethodGet && r.URL.Path == "/api/mills/workflow/runs/wf-cleanup":
			_, _ = fmt.Fprintf(w, `{"run":{"id":"wf-cleanup","state":%q},"steps":[]}`, runState)
		case r.Method == http.MethodPost && r.URL.Path == "/api/mills/workflow/runs/wf-cleanup/fail":
			failCalls++
			runState = "error"
			_, _ = w.Write([]byte(`{"state":"error"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/agent/spawn/abc/stop":
			if r.Header.Get("X-Admin-Token") != "hud-secret" {
				http.Error(w, "missing hud token", http.StatusUnauthorized)
				return
			}
			stopCalls++
			spawnStatus = "stopped"
			spawnActive = false
			_, _ = w.Write([]byte(`{"stopped":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	h := New(Config{
		OperatorURL: server.URL, HudURL: server.URL, HudAdminToken: "hud-secret",
		PollInterval: time.Millisecond,
	})
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		command := strings.Join(args, " ")
		switch {
		case strings.Contains(command, "get configmap loom-spawn-state"):
			return cleanupSpawnConfigMapJSON(spawnStatus), nil
		case strings.Contains(command, "--field-selector metadata.name=spawn-abc"):
			if spawnActive {
				return spawnPodListJSON("spawn-abc", "uid-cleanup", "Running"), nil
			}
			return `{"items":[]}`, nil
		case strings.Contains(command, "get pods -o json"):
			if spawnActive {
				return spawnPodListJSON("spawn-abc", "uid-cleanup", "Running"), nil
			}
			return `{"items":[]}`, nil
		default:
			return "", fmt.Errorf("unexpected kubectl call: %s", command)
		}
	}

	if err := h.CleanupRun(context.Background(), "wf-cleanup", "abc", "test cleanup"); err != nil {
		t.Fatalf("CleanupRun() error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if runState != "error" || spawnStatus != "stopped" || spawnActive || stopCalls == 0 || failCalls == 0 {
		t.Fatalf("cleanup incomplete: run=%s spawn=%s active=%t stop_calls=%d fail_calls=%d",
			runState, spawnStatus, spawnActive, stopCalls, failCalls)
	}
}

func TestStopSpawnRequiresCleanupCredentials(t *testing.T) {
	h := New(Config{})
	if err := h.StopSpawn(context.Background(), "abc"); err == nil {
		t.Fatal("StopSpawn accepted missing HUD cleanup credentials")
	}
}

func TestCleanupRunAcceptsNeverRegisteredSpawnAfterStableZeroProof(t *testing.T) {
	runState := "running"
	stopCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/mills/workflow/runs/wf-never-registered":
			_, _ = fmt.Fprintf(w, `{"run":{"id":"wf-never-registered","state":%q},"steps":[]}`, runState)
		case r.Method == http.MethodPost && r.URL.Path == "/api/mills/workflow/runs/wf-never-registered/fail":
			runState = "error"
			_, _ = w.Write([]byte(`{"state":"error"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/mills/safety/quiescence":
			w.Header().Set("Cache-Control", "no-store")
			_, _ = fmt.Fprintf(w, `{"observed_at":%q,"quiescent":true,"counts":{},"in_memory":{"admission_closed":true,"policy_generation":2,"sources_ready":true,"sample_stable":true,"wiring_required":true,"activity_sources":6,"source_generation":3,"source_operations":{"reconciler":0,"pipeline":0,"cross_run":0,"council":0,"canary":0,"workflow":0},"source_run_ids":{"workflow":[]}}}`,
				time.Now().UTC().Format(time.RFC3339Nano))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/api/agent/spawn/"):
			stopCalls++
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	h := New(Config{
		OperatorURL: server.URL, HudURL: server.URL, HudAdminToken: "hud-secret",
		PollInterval: time.Millisecond,
	})
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		command := strings.Join(args, " ")
		switch {
		case strings.Contains(command, "get configmap loom-spawn-state"):
			return testSpawnStateConfigMapJSON(nil), nil
		case strings.Contains(command, "get pods"):
			return `{"items":[]}`, nil
		default:
			return "", fmt.Errorf("unexpected kubectl call: %s", command)
		}
	}
	if err := h.CleanupRun(context.Background(), "wf-never-registered", "notregistered", "cleanup"); err != nil {
		t.Fatalf("CleanupRun() error = %v", err)
	}
	if stopCalls != 0 || runState != "error" {
		t.Fatalf("never-registered cleanup stop_calls=%d run_state=%s", stopCalls, runState)
	}
}
