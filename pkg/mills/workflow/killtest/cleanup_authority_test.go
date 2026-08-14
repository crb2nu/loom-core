package killtest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func cleanupReviewedOperatorDeploymentFixture(t *testing.T) (DeploymentIdentity, string) {
	t.Helper()
	raw := controllerDeploymentFixtureJSON(
		"loom-mills-operator", s1cOperatorNamespace, "operator-deployment-uid", "operator:v1",
	)
	deployment := mustStableDeploymentFixture(raw)
	deployment.ReviewedSpecSHA256 = deployment.SpecSHA256
	deployment.ReviewedPodTemplateSHA256 = deployment.PodTemplateSHA256
	deployment.ReviewedSelectorSHA256 = deployment.SelectorSHA256
	deployment.Review = DeploymentReviewIdentity{
		Contract: DeploymentProvenanceContract, ContractVersion: DeploymentProvenanceContractVersion,
		FluxOwner: "apps", FluxSpecSHA256: strings.Repeat("1", 64),
		Renderer: "flux build kustomization --dry-run", RendererVersion: "flux: v-test",
		RenderedSpecSHA256:  strings.Repeat("2", 64),
		PlatformRevision:    strings.Repeat("a", 40),
		PlatformScopeDigest: strings.Repeat("3", 64),
		SourceRevision:      strings.Repeat("a", 40),
		SourceScopeDigest:   strings.Repeat("4", 64),
	}
	return deployment, raw
}

func cleanupNamespaceFixture(uid string) string {
	return fmt.Sprintf(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":%q,"uid":%q,"resourceVersion":"11"},"status":{"phase":"Active"}}`,
		s1cOperatorNamespace, uid)
}

func setCleanupAuthorityHeaders(header http.Header, authority OperatorResponseAuthority) {
	for key, values := range testOperatorAuthorityHeaders(authority) {
		header[key] = append([]string(nil), values...)
	}
}

func configureCleanupAuthorityRoot(h *Harness, deployment DeploymentIdentity, pod PodIdentity) {
	root := testAuthorityPlane(pod, deployment)
	h.clusterAuthority = root.Kubernetes
	h.reviewedOperatorDeployment = deployment
	h.expectedOperatorDeployment = deployment
	h.expectedOperatorPod = pod
	h.operatorResponseAuthority = root.Operator
}

func configureCleanupAuthorityKubernetes(
	t *testing.T,
	h *Harness,
	deploymentJSON string,
	currentPod PodIdentity,
	spawnStatus *string,
	spawnActive *bool,
	mu *sync.Mutex,
) {
	t.Helper()
	configureControllerPodDryRunFixture(t, h)
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		if mu != nil {
			mu.Lock()
			defer mu.Unlock()
		}
		command := strings.Join(args, " ")
		switch {
		case command == "get --raw /api/v1/namespaces/"+s1cOperatorNamespace:
			return cleanupNamespaceFixture("operator-namespace-uid"), nil
		case strings.Contains(command, "get deploy loom-mills-operator"):
			return deploymentJSON, nil
		case strings.Contains(command, controllerPodCensusPath(s1cOperatorNamespace)):
			return controllerPodListFixtureJSON(currentPod), nil
		case strings.Contains(command, "get replicaset"):
			return controllerReplicaSetFixtureJSON(currentPod, deploymentJSON), nil
		case strings.Contains(command, "get configmap loom-spawn-state") && spawnStatus != nil:
			return cleanupSpawnConfigMapJSON(*spawnStatus), nil
		case strings.Contains(command, "--field-selector metadata.name=spawn-abc") && spawnActive != nil:
			if *spawnActive {
				return spawnPodListJSON("spawn-abc", "uid-cleanup", "Running"), nil
			}
			return `{"items":[]}`, nil
		case strings.Contains(command, "-n devbox get pods -o json") && spawnActive != nil:
			if *spawnActive {
				return spawnPodListJSON("spawn-abc", "uid-cleanup", "Running"), nil
			}
			return `{"items":[]}`, nil
		default:
			return "", fmt.Errorf("unexpected cleanup kubectl call: %s", command)
		}
	}
}

func TestCleanupRunRebindsEphemeralAuthorityWithoutAdvancingGateEvidence(t *testing.T) {
	deployment, deploymentJSON := cleanupReviewedOperatorDeploymentFixture(t)
	started := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	oldPod := bindControllerPodFixture(controllerPodFixture(
		"operator-old", deployment.Namespace, "operator-old-uid", deployment.Image, "operator@sha256:abc",
		deployment.Name, deployment.UID, started,
	), deployment)
	currentPod := replacementControllerPodFixture(
		oldPod, "operator-unplanned", "operator-unplanned-uid", started.Add(time.Minute),
	)
	currentAuthority := OperatorResponseAuthority{
		Contract: OperatorAuthorityContract, ContractVersion: OperatorAuthorityContractVersion,
		PodName: currentPod.Name, PodNamespace: currentPod.Namespace, PodUID: currentPod.UID,
		DeploymentName: deployment.Name, BootID: strings.Repeat("d", 64),
	}

	var mu sync.Mutex
	runState, spawnStatus, spawnActive := "running", "running", true
	stopCalls, failCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		setCleanupAuthorityHeaders(w.Header(), currentAuthority)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/mills/workflow/runs/wf-cleanup":
			_, _ = fmt.Fprintf(w, `{"run":{"id":"wf-cleanup","state":%q},"steps":[]}`, runState)
		case r.Method == http.MethodPost && r.URL.Path == "/api/mills/workflow/runs/wf-cleanup/fail":
			failCalls++
			runState = "error"
			_, _ = w.Write([]byte(`{"state":"error"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/agent/spawn/abc/stop":
			stopCalls++
			spawnStatus = "stopped"
			spawnActive = false
			_, _ = w.Write([]byte(`{"stopped":true}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/mills/safety/quiescence":
			w.Header().Set("Cache-Control", "no-store")
			_, _ = fmt.Fprintf(w, `{"observed_at":%q,"quiescent":true,"counts":{},"in_memory":{"admission_closed":true,"policy_generation":2,"sources_ready":true,"sample_stable":true,"wiring_required":true,"activity_sources":6,"source_generation":3,"source_operations":{"reconciler":0,"pipeline":0,"cross_run":0,"council":0,"canary":0,"workflow":0},"source_run_ids":{"workflow":[]}}}`,
				time.Now().UTC().Format(time.RFC3339Nano))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	h := New(Config{
		OperatorURL: server.URL, HudURL: server.URL, HudAdminToken: "hud-secret",
		RequireAuthorityBinding: true, PollInterval: time.Millisecond,
	})
	configureCleanupAuthorityRoot(h, deployment, oldPod)
	configureCleanupAuthorityKubernetes(t, h, deploymentJSON, currentPod, &spawnStatus, &spawnActive, &mu)
	originalAuthority := h.operatorResponseAuthority
	originalExpectedPod := h.expectedOperatorPod
	originalExpectedDeployment := h.expectedOperatorDeployment

	if err := h.CleanupRun(t.Context(), "wf-cleanup", "abc", "gate failed"); err != nil {
		t.Fatalf("CleanupRun() error = %v", err)
	}
	mu.Lock()
	if runState != "error" || spawnStatus != "stopped" || spawnActive || stopCalls == 0 || failCalls == 0 {
		mu.Unlock()
		t.Fatalf("cleanup incomplete: run=%s spawn=%s active=%t stop=%d fail=%d",
			runState, spawnStatus, spawnActive, stopCalls, failCalls)
	}
	mu.Unlock()
	if h.operatorResponseAuthority != originalAuthority ||
		!reflect.DeepEqual(h.expectedOperatorPod, originalExpectedPod) ||
		h.expectedOperatorDeployment != originalExpectedDeployment {
		t.Fatalf("cleanup replacement advanced gate authority: response=%+v pod=%+v deployment=%+v",
			h.operatorResponseAuthority, h.expectedOperatorPod, h.expectedOperatorDeployment)
	}
	gateAuthority, err := h.currentAuthorityPlane()
	if err != nil {
		t.Fatalf("original gate authority was damaged by cleanup: %v", err)
	}
	if gateAuthority.Operator.PodUID != oldPod.UID || gateAuthority.Operator.BootID == currentAuthority.BootID {
		t.Fatalf("current cleanup backend became passing gate evidence: %+v", gateAuthority.Operator)
	}
}

func TestCleanupRunReturnsTypedAuthorityMismatchWithoutPolling(t *testing.T) {
	deployment, deploymentJSON := cleanupReviewedOperatorDeploymentFixture(t)
	started := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	oldPod := bindControllerPodFixture(controllerPodFixture(
		"operator-old", deployment.Namespace, "operator-old-uid", deployment.Image, "operator@sha256:abc",
		deployment.Name, deployment.UID, started,
	), deployment)
	currentPod := replacementControllerPodFixture(
		oldPod, "operator-unplanned", "operator-unplanned-uid", started.Add(time.Minute),
	)
	currentAuthority := OperatorResponseAuthority{
		Contract: OperatorAuthorityContract, ContractVersion: OperatorAuthorityContractVersion,
		PodName: currentPod.Name, PodNamespace: currentPod.Namespace, PodUID: currentPod.UID,
		DeploymentName: deployment.Name, BootID: strings.Repeat("d", 64),
	}
	driftedAuthority := currentAuthority
	driftedAuthority.BootID = strings.Repeat("e", 64)

	runRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/mills/workflow/runs/wf-cleanup" {
			http.NotFound(w, r)
			return
		}
		runRequests++
		if runRequests == 1 {
			setCleanupAuthorityHeaders(w.Header(), currentAuthority)
		} else {
			setCleanupAuthorityHeaders(w.Header(), driftedAuthority)
		}
		_, _ = w.Write([]byte(`{"run":{"id":"wf-cleanup","state":"running"},"steps":[]}`))
	}))
	defer server.Close()

	h := New(Config{
		OperatorURL: server.URL, RequireAuthorityBinding: true,
		PollInterval: 30 * time.Second,
	})
	configureCleanupAuthorityRoot(h, deployment, oldPod)
	configureCleanupAuthorityKubernetes(t, h, deploymentJSON, currentPod, nil, nil, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := h.CleanupRun(ctx, "wf-cleanup", "", "gate failed")
	var authorityErr *CleanupAuthorityError
	if !errors.Is(err, ErrOperatorAuthority) || !errors.As(err, &authorityErr) {
		t.Fatalf("CleanupRun() error = %v, want typed operator authority mismatch", err)
	}
	if runRequests != 2 {
		t.Fatalf("authority drift was retried: requests=%d error=%v", runRequests, err)
	}
}

func TestCleanupRunRejectsLiveDeploymentDriftBeforeOperatorRequest(t *testing.T) {
	deployment, _ := cleanupReviewedOperatorDeploymentFixture(t)
	driftedJSON := controllerDeploymentFixtureJSON(
		deployment.Name, deployment.Namespace, deployment.UID, "operator:unreviewed",
	)
	started := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	oldPod := bindControllerPodFixture(controllerPodFixture(
		"operator-old", deployment.Namespace, "operator-old-uid", deployment.Image, "operator@sha256:abc",
		deployment.Name, deployment.UID, started,
	), deployment)
	currentPod := replacementControllerPodFixture(
		oldPod, "operator-unplanned", "operator-unplanned-uid", started.Add(time.Minute),
	)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()

	h := New(Config{OperatorURL: server.URL, RequireAuthorityBinding: true})
	configureCleanupAuthorityRoot(h, deployment, oldPod)
	configureCleanupAuthorityKubernetes(t, h, driftedJSON, currentPod, nil, nil, nil)
	err := h.CleanupRun(t.Context(), "wf-cleanup", "", "gate failed")
	var authorityErr *CleanupAuthorityError
	if !errors.Is(err, ErrOperatorAuthority) || !errors.As(err, &authorityErr) ||
		!strings.Contains(err.Error(), "reviewed operator Deployment") {
		t.Fatalf("CleanupRun() drift error = %v", err)
	}
	if requests != 0 {
		t.Fatalf("cleanup contacted unreviewed operator backend %d time(s)", requests)
	}
}
