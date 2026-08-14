package killtest

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func testOperatorAuthorityHeaders(authority OperatorResponseAuthority) http.Header {
	header := make(http.Header)
	header.Set(operatorAuthorityContractHeader, authority.Contract)
	header.Set(operatorAuthorityVersionHeader, "1")
	header.Set(operatorAuthorityPodNameHeader, authority.PodName)
	header.Set(operatorAuthorityPodNamespaceHeader, authority.PodNamespace)
	header.Set(operatorAuthorityPodUIDHeader, authority.PodUID)
	header.Set(operatorAuthorityDeploymentNameHeader, authority.DeploymentName)
	header.Set(operatorAuthorityBootIDHeader, authority.BootID)
	return header
}

func TestParseOperatorResponseAuthorityFailsClosed(t *testing.T) {
	report := canonicalTestPreflight(authorityTestTime(), "", testOperatorPod(), testHUDPod())
	valid := report.AuthorityPlane.Operator
	if got, err := parseOperatorResponseAuthority(testOperatorAuthorityHeaders(valid)); err != nil || got != valid {
		t.Fatalf("valid authority = %+v, %v", got, err)
	}
	for name, mutate := range map[string]func(http.Header){
		"missing pod UID": func(header http.Header) { header.Del(operatorAuthorityPodUIDHeader) },
		"wrong contract":  func(header http.Header) { header.Set(operatorAuthorityContractHeader, "old") },
		"bad version":     func(header http.Header) { header.Set(operatorAuthorityVersionHeader, "not-an-int") },
		"missing boot ID": func(header http.Header) { header.Del(operatorAuthorityBootIDHeader) },
	} {
		t.Run(name, func(t *testing.T) {
			header := testOperatorAuthorityHeaders(valid)
			mutate(header)
			if _, err := parseOperatorResponseAuthority(header); err == nil {
				t.Fatal("invalid authority accepted")
			}
		})
	}
}

func TestNormalizedAPIServerRequiresHTTPSWithoutUserinfo(t *testing.T) {
	for name, test := range map[string]struct {
		raw     string
		want    string
		wantErr bool
	}{
		"https":    {raw: "https://kubernetes.example:6443/", want: "https://kubernetes.example:6443"},
		"http":     {raw: "http://kubernetes.example:6443", wantErr: true},
		"userinfo": {raw: "https://user:secret@kubernetes.example:6443", wantErr: true},
		"query":    {raw: "https://kubernetes.example:6443?token=secret", wantErr: true},
		"fragment": {raw: "https://kubernetes.example:6443#other", wantErr: true},
		"no host":  {raw: "https://", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := normalizedAPIServer(test.raw)
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("normalizedAPIServer(%q) = %q, %v", test.raw, got, err)
			}
		})
	}
}

func TestObserveOperatorResponseRejectsSamePodBootIDDrift(t *testing.T) {
	report := canonicalTestPreflight(authorityTestTime(), "", testOperatorPod(), testHUDPod())
	h := New(Config{RequireAuthorityBinding: true})
	h.expectedOperatorPod = report.Operator
	h.expectedOperatorDeployment = report.OperatorDeployment
	h.operatorResponseAuthority = report.AuthorityPlane.Operator
	drifted := report.AuthorityPlane.Operator
	drifted.BootID = strings.Repeat("f", 64)
	response := &http.Response{Header: testOperatorAuthorityHeaders(drifted)}
	if _, err := h.observeOperatorResponse(response); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("same-Pod boot ID drift accepted: %v", err)
	}
}

func authorityTestTime() time.Time {
	return time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
}

func testOperatorPod() PodIdentity {
	return PodIdentity{
		Name: "operator", UID: "operator-uid", Node: "node-a",
		Image: "operator:v1", ImageID: "operator@sha256:aaa", StartedAt: authorityTestTime().Add(-time.Hour),
	}
}

func testHUDPod() PodIdentity {
	return PodIdentity{
		Name: "hud", UID: "hud-uid", Node: "node-b",
		Image: "hud:v1", ImageID: "hud@sha256:bbb", StartedAt: authorityTestTime().Add(-time.Hour),
	}
}

func TestValidateAuthorityPlaneEvidenceRejectsCrossPlaneDrift(t *testing.T) {
	report := canonicalTestPreflight(authorityTestTime(), "", testOperatorPod(), testHUDPod())
	valid := report.AuthorityPlane
	if err := ValidateAuthorityPlaneEvidence(valid, report.Operator, report.OperatorDeployment); err != nil {
		t.Fatalf("valid authority rejected: %v", err)
	}
	for name, mutate := range map[string]func(*AuthorityPlaneEvidence){
		"API server": func(evidence *AuthorityPlaneEvidence) {
			evidence.Kubernetes.APIServerSHA256 = strings.Repeat("d", 64)[:63]
		},
		"namespace UID":    func(evidence *AuthorityPlaneEvidence) { evidence.Kubernetes.OperatorNamespaceIdentity.UID = "" },
		"operator pod UID": func(evidence *AuthorityPlaneEvidence) { evidence.Operator.PodUID = "other-pod" },
		"deployment UID":   func(evidence *AuthorityPlaneEvidence) { evidence.OperatorDeploymentUID = "other-deployment" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := valid
			mutate(&changed)
			if err := ValidateAuthorityPlaneEvidence(changed, report.Operator, report.OperatorDeployment); err == nil {
				t.Fatal("drifted authority accepted")
			}
		})
	}
}

func TestEvaluateRejectsRESTAuthorityDrift(t *testing.T) {
	for name, mutate := range map[string]func(*Evidence){
		"policy": func(evidence *Evidence) {
			evidence.CrashASafety.PolicyDeleteBoundary.Effective.OperatorAuthority.PodUID = "other"
		},
		"quiescence": func(evidence *Evidence) {
			evidence.CrashASafety.Target.Quiescence.OperatorAuthority.PodUID = "other"
		},
		"run": func(evidence *Evidence) {
			evidence.CrashASafety.Target.RunAuthority.PodUID = "other"
		},
		"lease": func(evidence *Evidence) {
			evidence.CrashASafety.LeaseRenewed.OperatorAuthority.PodUID = "other"
		},
	} {
		t.Run(name, func(t *testing.T) {
			evidence := passingEvidence()
			mutate(&evidence)
			verdict := Evaluate(evidence)
			if verdict.Pass8CrashSafety || !strings.Contains(strings.ToLower(verdict.Pass8Reason), "authority") {
				t.Fatalf("authority drift verdict = %+v", verdict)
			}
		})
	}
}

func TestDeleteClientRejectsDifferentNamespaceUID(t *testing.T) {
	objects := []runtime.Object{&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: s1cOperatorNamespace, UID: types.UID("other-cluster-uid")},
		Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	}}
	h := New(Config{RequireAuthorityBinding: true})
	h.clusterAuthority = KubernetesClusterAuthority{
		OperatorNamespaceIdentity: KubernetesObjectIdentity{
			Name: s1cOperatorNamespace, UID: "expected-cluster-uid", ResourceVersion: "10",
		},
	}
	h.kubeOnce.Do(func() { h.kube = fake.NewSimpleClientset(objects...) })
	if err := h.validateDeleteClusterAuthority(t.Context()); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("different delete cluster accepted: %v", err)
	}
}

func TestAcceptedOperatorDeleteRebindsReplacementBeforeReturningEvidenceError(t *testing.T) {
	deploymentJSON := controllerDeploymentFixtureJSON(
		"loom-mills-operator", s1cOperatorNamespace, "operator-deployment-uid", "operator:v1")
	deployment := mustStableDeploymentFixture(deploymentJSON)
	before := controllerPodFixture(
		"operator-old", deployment.Namespace, "operator-old-uid", deployment.Image, "operator@sha256:old",
		deployment.Name, deployment.UID, authorityTestTime().Add(-time.Hour))
	before = bindControllerPodFixture(before, deployment)
	after := replacementControllerPodFixture(before, "operator-new", "operator-new-uid", time.Now().UTC())
	// Force a post-convergence evidence failure as well as a delete-receipt
	// failure. Cleanup must still be routed to the independently selected new
	// Pod instead of remaining pinned to the deleted backend.
	after.ImageID = "operator@sha256:unexpected"

	h := New(Config{
		RequireAuthorityBinding: true, PollInterval: time.Millisecond, StepTimeout: time.Second,
	})
	configureControllerPodDryRunFixture(t, h)
	h.expectedOperatorPod = before
	h.expectedOperatorDeployment = deployment
	h.operatorResponseAuthority = testAuthorityPlane(before, deployment).Operator
	deleted := false
	h.deletePodFn = func(context.Context, string, string, string, string) error {
		deleted = true
		return nil
	}
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		command := strings.Join(args, " ")
		switch {
		case strings.Contains(command, "get deploy"):
			return deploymentJSON, nil
		case strings.Contains(command, controllerPodCensusPath(deployment.Namespace)) && !deleted:
			return controllerPodListFixtureJSON(before), nil
		case strings.Contains(command, controllerPodCensusPath(deployment.Namespace)):
			return controllerPodListFixtureJSON(after), nil
		case strings.Contains(command, "get replicaset") && !deleted:
			return controllerReplicaSetFixtureJSON(before, deploymentJSON), nil
		case strings.Contains(command, "get replicaset"):
			return controllerReplicaSetFixtureJSON(after, deploymentJSON), nil
		default:
			return "", nil
		}
	}
	receiptErr := errors.New("checkpoint storage unavailable")
	ctx, cancel := context.WithCancel(t.Context())
	replacement, err := h.crashPod(ctx, deployment.Namespace, "app=loom-mills-operator", deployment.Name,
		before, deployment, nil, nil, func(time.Time) error {
			cancel()
			return receiptErr
		})
	if !errors.Is(err, receiptErr) || !errors.Is(err, context.Canceled) ||
		!strings.Contains(err.Error(), "changed image") {
		t.Fatalf("post-delete errors = %v", err)
	}
	if replacement.UID != after.UID || h.expectedOperatorPod.UID != after.UID ||
		h.expectedOperatorDeployment.Name != deployment.Name ||
		h.expectedOperatorDeployment.UID != deployment.UID ||
		h.operatorResponseAuthority != (OperatorResponseAuthority{}) {
		t.Fatalf("replacement authority was not advanced: replacement=%+v expected=%+v response=%+v",
			replacement, h.expectedOperatorPod, h.operatorResponseAuthority)
	}
}
