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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const renderedPolicyConfigMapFixture = `apiVersion: v1
kind: ConfigMap
metadata:
  name: loom-mills-policy
  namespace: loom-mills
data:
  policy.yaml: |
    policy:
      workflows:
        global_enabled: false
        workflows_enabled: true
        substrate_k8s_only: true
  second: value
binaryData:
  policy.sig: c2lnbmF0dXJl
immutable: false
`

func TestPolicyConfigMapPayloadHashCoversCompletePayload(t *testing.T) {
	immutable := false
	base := &corev1.ConfigMap{
		Data:       map[string]string{"policy.yaml": "policy", "second": "value"},
		BinaryData: map[string][]byte{"b": []byte("two"), "a": []byte("one")},
		Immutable:  &immutable,
	}
	baseSHA, err := canonicalPolicyConfigMapPayloadSHA256(base)
	if err != nil || !isNormalizedSHA256(baseSHA) {
		t.Fatalf("canonical payload SHA-256 = %q, %v", baseSHA, err)
	}
	reordered := &corev1.ConfigMap{
		Data:       map[string]string{"second": "value", "policy.yaml": "policy"},
		BinaryData: map[string][]byte{"a": []byte("one"), "b": []byte("two")},
		Immutable:  &immutable,
	}
	reorderedSHA, _ := canonicalPolicyConfigMapPayloadSHA256(reordered)
	if reorderedSHA != baseSHA {
		t.Fatalf("map insertion order changed payload SHA-256: %s != %s", baseSHA, reorderedSHA)
	}

	mutations := map[string]func(*corev1.ConfigMap){
		"extra data":       func(cm *corev1.ConfigMap) { cm.Data["unreviewed"] = "true" },
		"changed policy":   func(cm *corev1.ConfigMap) { cm.Data["policy.yaml"] = "changed" },
		"extra binaryData": func(cm *corev1.ConfigMap) { cm.BinaryData["unreviewed"] = []byte("true") },
		"changed binaryData": func(cm *corev1.ConfigMap) {
			cm.BinaryData["a"] = []byte("changed")
		},
		"immutable presence": func(cm *corev1.ConfigMap) { cm.Immutable = nil },
		"immutable value": func(cm *corev1.ConfigMap) {
			value := true
			cm.Immutable = &value
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := base.DeepCopy()
			mutate(changed)
			changedSHA, err := canonicalPolicyConfigMapPayloadSHA256(changed)
			if err != nil {
				t.Fatal(err)
			}
			if changedSHA == baseSHA {
				t.Fatalf("%s did not change complete payload SHA-256", name)
			}
		})
	}
}

func TestSelectRenderedPolicyConfigMapFailsClosed(t *testing.T) {
	configMap, digest, err := selectRenderedPolicyConfigMap([]byte(renderedPolicyConfigMapFixture))
	if err != nil || configMap.Name != policyConfigMapName || !isNormalizedSHA256(digest) {
		t.Fatalf("valid rendered policy ConfigMap = %+v digest=%q error=%v", configMap, digest, err)
	}
	if string(configMap.BinaryData["policy.sig"]) != "signature" || configMap.Immutable == nil || *configMap.Immutable {
		t.Fatalf("complete rendered payload was not retained: %+v", configMap)
	}

	for name, raw := range map[string]string{
		"missing":             strings.Replace(renderedPolicyConfigMapFixture, "name: loom-mills-policy", "name: redirected", 1),
		"missing policy.yaml": strings.Replace(renderedPolicyConfigMapFixture, "  policy.yaml:", "  other.yaml:", 1),
		"duplicate":           renderedPolicyConfigMapFixture + "---\n" + renderedPolicyConfigMapFixture,
		"unknown field":       renderedPolicyConfigMapFixture + "attackerField: true\n",
		"runtime identity":    strings.Replace(renderedPolicyConfigMapFixture, "  namespace: loom-mills", "  namespace: loom-mills\n  uid: live-uid", 1),
		"status":              renderedPolicyConfigMapFixture + "status: {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := selectRenderedPolicyConfigMap([]byte(raw)); err == nil {
				t.Fatal("invalid rendered policy ConfigMap accepted")
			}
		})
	}
}

func TestReviewedPolicyConfigMapBindsExactSourceChecksum(t *testing.T) {
	source := []byte("exact source bytes\nwith final newline\n")
	sourceSHA := exactPolicySourceSHA256(source)
	if sourceSHA == exactPolicySourceSHA256(source[:len(source)-1]) {
		t.Fatal("source checksum normalized away the exact final newline")
	}
	deployment := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{policyChecksumAnnotation: sourceSHA}},
	}}}
	review := PolicyConfigMapReviewIdentity{
		Contract: PolicyConfigMapProvenanceContract, ContractVersion: PolicyConfigMapProvenanceContractVersion,
		FluxOwner: policyConfigMapFluxOwner, FluxSpecSHA256: strings.Repeat("a", 64),
		Renderer: policyConfigMapRenderer, RendererVersion: "flux: v-test",
		PlatformRevision: strings.Repeat("b", 40), PlatformScopeDigest: strings.Repeat("c", 64),
	}
	result, err := reviewedPolicyConfigMapFromFluxRender(
		[]byte(renderedPolicyConfigMapFixture), source, deployment, review,
	)
	if err != nil {
		t.Fatalf("valid reviewed policy rejected: %v", err)
	}
	if result.Review.Name != policyConfigMapName || result.Review.Namespace != s1cOperatorNamespace ||
		result.Review.SourcePath != policyConfigMapSourcePath || result.Review.SourceSHA256 != sourceSHA ||
		result.Review.RenderedPayloadSHA256 != result.RenderedPayloadSHA256 || result.Review.LivePayloadSHA256 != "" {
		t.Fatalf("reviewed policy identity is incomplete: %+v", result.Review)
	}

	deployment.Spec.Template.Annotations[policyChecksumAnnotation] = strings.Repeat("d", 64)
	if _, err := reviewedPolicyConfigMapFromFluxRender(
		[]byte(renderedPolicyConfigMapFixture), source, deployment, review,
	); err == nil || !strings.Contains(err.Error(), "differs from exact source") {
		t.Fatalf("forged reviewed Deployment policy checksum accepted: %v", err)
	}
}

func TestParsePolicyConfigMapPayloadHashesLiveCompletePayload(t *testing.T) {
	immutable := false
	live := corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name: policyConfigMapName, Namespace: s1cOperatorNamespace,
			UID: types.UID("policy-uid"), ResourceVersion: "17",
		},
		Data: map[string]string{
			"policy.yaml": "policy:\n  workflows:\n    global_enabled: false\n    workflows_enabled: true\n    substrate_k8s_only: true\n",
			"second":      "value",
		},
		BinaryData: map[string][]byte{"policy.sig": []byte("signature")},
		Immutable:  &immutable,
	}
	raw, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	identity, policy, digest, err := parsePolicyConfigMapPayload(string(raw), s1cOperatorNamespace)
	if err != nil {
		t.Fatalf("valid live policy ConfigMap rejected: %v", err)
	}
	wantDigest, _ := canonicalPolicyConfigMapPayloadSHA256(&live)
	if identity.UID != "policy-uid" || policy == "" || digest != wantDigest {
		t.Fatalf("live policy payload binding = identity=%+v policy=%q digest=%q, want %q",
			identity, policy, digest, wantDigest)
	}
}

func TestBindReviewedPolicyConfigMapRejectsPreExistingPayloadDrift(t *testing.T) {
	immutable := false
	reviewedObject := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: policyConfigMapName, Namespace: s1cOperatorNamespace},
		Data:       map[string]string{"policy.yaml": "reviewed"},
		Immutable:  &immutable,
	}
	reviewedSHA, err := canonicalPolicyConfigMapPayloadSHA256(reviewedObject)
	if err != nil {
		t.Fatal(err)
	}
	checksum := strings.Repeat("8", 64)
	reviewed := reviewedPolicyConfigMap{
		ConfigMap: reviewedObject, RenderedPayloadSHA256: reviewedSHA,
		Review: PolicyConfigMapReviewIdentity{
			Contract: PolicyConfigMapProvenanceContract, ContractVersion: PolicyConfigMapProvenanceContractVersion,
			Name: policyConfigMapName, Namespace: s1cOperatorNamespace,
			FluxOwner: policyConfigMapFluxOwner, FluxSpecSHA256: strings.Repeat("a", 64),
			Renderer: policyConfigMapRenderer, RendererVersion: "flux: v-test",
			PlatformRevision: strings.Repeat("b", 40), PlatformScopeDigest: strings.Repeat("c", 64),
			SourcePath: policyConfigMapSourcePath, SourceSHA256: checksum,
			RenderedPayloadSHA256: reviewedSHA,
		},
	}
	identity := KubernetesObjectIdentity{
		Name: policyConfigMapName, Namespace: s1cOperatorNamespace,
		UID: "policy-uid", ResourceVersion: "41",
	}
	bound, err := bindReviewedPolicyConfigMap(identity, reviewedSHA, checksum, reviewed)
	if err != nil || bound.LivePayloadSHA256 != reviewedSHA {
		t.Fatalf("matching live policy rejected: review=%+v error=%v", bound, err)
	}

	liveWithExtraData := reviewedObject.DeepCopy()
	liveWithExtraData.Data["unreviewed"] = "true"
	driftSHA, _ := canonicalPolicyConfigMapPayloadSHA256(liveWithExtraData)
	if _, err := bindReviewedPolicyConfigMap(identity, driftSHA, checksum, reviewed); err == nil ||
		!strings.Contains(err.Error(), "differs from reviewed render") {
		t.Fatalf("pre-existing live extra data accepted: %v", err)
	}
	if _, err := bindReviewedPolicyConfigMap(identity, reviewedSHA, strings.Repeat("9", 64), reviewed); err == nil ||
		!strings.Contains(err.Error(), "exact reviewed source") {
		t.Fatalf("live Deployment checksum drift accepted: %v", err)
	}
	forgedCache := cloneReviewedPolicyConfigMap(reviewed)
	forgedCache.ConfigMap.Data["unreviewed"] = "true"
	if _, err := bindReviewedPolicyConfigMap(identity, reviewedSHA, checksum, forgedCache); err == nil ||
		!strings.Contains(err.Error(), "differs from cached digest") {
		t.Fatalf("forged reviewed payload cache accepted: %v", err)
	}
}

func TestValidatePolicyConfigMapProvenanceRejectsForgedReview(t *testing.T) {
	operator := PodIdentity{
		Name: "operator", UID: "operator-pod", Image: "operator:v1",
		ImageID: "operator@sha256:a", StartedAt: testTime,
	}
	hud := PodIdentity{
		Name: "hud", UID: "hud-pod", Image: "hud:v1",
		ImageID: "hud@sha256:b", StartedAt: testTime,
	}
	report := canonicalTestPreflight(testTime, "", operator, hud)
	if err := ValidatePolicyConfigMapProvenance(report); err != nil {
		t.Fatalf("valid policy ConfigMap provenance rejected: %v", err)
	}

	tests := map[string]func(*PreflightReport){
		"missing review": func(report *PreflightReport) {
			report.PolicyConfigMapReview = PolicyConfigMapReviewIdentity{}
		},
		"contract": func(report *PreflightReport) { report.PolicyConfigMapReview.Contract = "forged" },
		"contract version": func(report *PreflightReport) {
			report.PolicyConfigMapReview.ContractVersion++
		},
		"redirected name": func(report *PreflightReport) { report.PolicyConfigMapReview.Name = "attacker" },
		"redirected namespace": func(report *PreflightReport) {
			report.PolicyConfigMapReview.Namespace = "attacker"
		},
		"Flux owner": func(report *PreflightReport) { report.PolicyConfigMapReview.FluxOwner = "system" },
		"Flux full spec": func(report *PreflightReport) {
			report.PolicyConfigMapReview.FluxSpecSHA256 = strings.Repeat("1", 64)
		},
		"renderer": func(report *PreflightReport) { report.PolicyConfigMapReview.Renderer = "kubectl get" },
		"renderer version": func(report *PreflightReport) {
			report.PolicyConfigMapReview.RendererVersion = ""
		},
		"platform revision": func(report *PreflightReport) {
			report.PolicyConfigMapReview.PlatformRevision = strings.Repeat("2", 40)
		},
		"platform digest": func(report *PreflightReport) {
			report.PolicyConfigMapReview.PlatformScopeDigest = strings.Repeat("3", 64)
		},
		"source path": func(report *PreflightReport) { report.PolicyConfigMapReview.SourcePath = "other.yaml" },
		"source SHA": func(report *PreflightReport) {
			report.PolicyConfigMapReview.SourceSHA256 = strings.Repeat("4", 64)
		},
		"rendered payload": func(report *PreflightReport) {
			report.PolicyConfigMapReview.RenderedPayloadSHA256 = strings.Repeat("5", 64)
		},
		"missing live payload": func(report *PreflightReport) {
			report.PolicyConfigMapReview.LivePayloadSHA256 = ""
		},
		"live payload drift": func(report *PreflightReport) {
			report.PolicyConfigMapReview.LivePayloadSHA256 = strings.Repeat("6", 64)
		},
		"report checksum": func(report *PreflightReport) { report.PolicyChecksum = strings.Repeat("a", 64) },
		"Deployment checksum": func(report *PreflightReport) {
			report.OperatorDeployment.PolicyChecksum = strings.Repeat("b", 64)
		},
		"live ConfigMap redirect": func(report *PreflightReport) {
			report.PolicyConfigMapIdentity.Name = "attacker"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			changed := report
			mutate(&changed)
			if err := ValidatePolicyConfigMapProvenance(changed); err == nil {
				t.Fatalf("%s forgery accepted", name)
			}
		})
	}

	changed := report
	changed.PolicyConfigMapReview.LivePayloadSHA256 = strings.Repeat("6", 64)
	changed.PolicyConfigMapReview.RenderedPayloadSHA256 = strings.Repeat("6", 64)
	if err := ValidatePolicyConfigMapGateIdentity(report, changed); err == nil {
		t.Fatal("complete policy payload identity change accepted across gate")
	}
}

func TestValidatePolicyDeleteBoundaryEvidenceRejectsStaleOrForgedRefresh(t *testing.T) {
	operator := PodIdentity{
		Name: "operator", UID: "operator-pod", Image: "operator:v1",
		ImageID: "operator@sha256:a", StartedAt: testTime,
	}
	hud := PodIdentity{
		Name: "hud", UID: "hud-pod", Image: "hud:v1",
		ImageID: "hud@sha256:b", StartedAt: testTime,
	}
	baseline := canonicalTestPreflight(testTime, "", operator, hud)
	start := baseline.FluxSourcesEnd.ObservedAt.Add(time.Second)
	evidence := testPolicyDeleteBoundaryEvidence(
		baseline, start, start.Add(time.Second), start.Add(2*time.Second),
		start.Add(3*time.Second), start.Add(4*time.Second),
	)
	deleteAt := start.Add(5 * time.Second)
	if err := ValidatePolicyDeleteBoundaryFreshness(deleteAt, baseline, evidence); err != nil {
		t.Fatalf("valid policy delete-boundary evidence rejected: %v", err)
	}

	tests := map[string]func(*PolicyDeleteBoundaryEvidence){
		"missing contract": func(evidence *PolicyDeleteBoundaryEvidence) { evidence.Contract = "" },
		"ConfigMap A replacement": func(evidence *PolicyDeleteBoundaryEvidence) {
			evidence.ConfigMapA.Identity.UID = "replacement"
		},
		"ConfigMap A resourceVersion drift": func(evidence *PolicyDeleteBoundaryEvidence) {
			evidence.ConfigMapA.Identity.ResourceVersion = "changed"
		},
		"ConfigMap A payload drift": func(evidence *PolicyDeleteBoundaryEvidence) {
			evidence.ConfigMapA.PayloadSHA256 = strings.Repeat("1", 64)
		},
		"ConfigMap B payload drift": func(evidence *PolicyDeleteBoundaryEvidence) {
			evidence.ConfigMapB.PayloadSHA256 = strings.Repeat("2", 64)
		},
		"effective policy opens": func(evidence *PolicyDeleteBoundaryEvidence) {
			evidence.Effective.PolicyEnabled = true
		},
		"operator checksum drift": func(evidence *PolicyDeleteBoundaryEvidence) {
			evidence.OperatorDeployment.PolicyChecksum = strings.Repeat("3", 64)
		},
		"operator full spec drift": func(evidence *PolicyDeleteBoundaryEvidence) {
			evidence.OperatorDeployment.SpecSHA256 = strings.Repeat("4", 64)
		},
		"operator not stable": func(evidence *PolicyDeleteBoundaryEvidence) {
			evidence.OperatorDeployment.ObservedGeneration--
		},
		"review forgery": func(evidence *PolicyDeleteBoundaryEvidence) {
			evidence.Review.SourcePath = "attacker.yaml"
		},
		"read order forgery": func(evidence *PolicyDeleteBoundaryEvidence) {
			evidence.ConfigMapB.ObservedAt = evidence.Effective.ObservedAt
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			changed := evidence
			mutate(&changed)
			if err := ValidatePolicyDeleteBoundaryEvidence(baseline, changed); err == nil {
				t.Fatalf("%s accepted", name)
			}
		})
	}

	staleDeleteAt := evidence.ConfigMapB.ObservedAt.Add(finalPreDeleteCheckTimeout + time.Nanosecond)
	if err := ValidatePolicyDeleteBoundaryFreshness(staleDeleteAt, baseline, evidence); err == nil ||
		!strings.Contains(err.Error(), "ConfigMap B proof") {
		t.Fatalf("stale ConfigMap B request-start accepted: %v", err)
	}
	if err := ValidatePolicyDeleteBoundaryFreshness(evidence.CompletedAt, baseline, evidence); err == nil ||
		!strings.Contains(err.Error(), "not before DELETE") {
		t.Fatalf("non-ordered bracket completion accepted: %v", err)
	}
}

func TestCollectPolicyDeleteBoundaryEvidenceUsesFinalOrderedReads(t *testing.T) {
	operator := PodIdentity{
		Name: "operator", UID: "operator-pod", Image: "operator:v1",
		ImageID: "operator@sha256:a", StartedAt: testTime,
	}
	hud := PodIdentity{
		Name: "hud", UID: "hud-pod", Image: "hud:v1",
		ImageID: "hud@sha256:b", StartedAt: testTime,
	}
	baseline := canonicalTestPreflight(time.Now().UTC().Add(-time.Minute), "", operator, hud)

	immutable := false
	livePolicy := corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name: policyConfigMapName, Namespace: s1cOperatorNamespace,
			UID:             types.UID(baseline.PolicyConfigMapIdentity.UID),
			ResourceVersion: baseline.PolicyConfigMapIdentity.ResourceVersion,
		},
		Data: map[string]string{
			"policy.yaml": "enabled: false\nworkflows:\n  enabled: true\n  substrate_k8s_only: true\n",
		},
		Immutable: &immutable,
	}
	policyRaw, err := json.Marshal(livePolicy)
	if err != nil {
		t.Fatal(err)
	}
	payloadSHA, _ := canonicalPolicyConfigMapPayloadSHA256(&livePolicy)
	baseline.PolicyConfigMapReview.RenderedPayloadSHA256 = payloadSHA
	baseline.PolicyConfigMapReview.LivePayloadSHA256 = payloadSHA

	liveDeployment := deploymentProvenanceTestObject(
		s1cOperatorNamespace, "loom-mills-operator", baseline.OperatorDeployment.Image,
	)
	liveDeployment.Spec.Template.Annotations = map[string]string{
		policyChecksumAnnotation: baseline.PolicyConfigMapReview.SourceSHA256,
	}
	deploymentRaw, err := json.Marshal(liveDeployment)
	if err != nil {
		t.Fatal(err)
	}
	liveDeploymentIdentity, err := parseStableDeployment(string(deploymentRaw))
	if err != nil {
		t.Fatal(err)
	}
	liveDeploymentIdentity.ReviewedSpecSHA256 = liveDeploymentIdentity.SpecSHA256
	liveDeploymentIdentity.ReviewedPodTemplateSHA256 = liveDeploymentIdentity.PodTemplateSHA256
	liveDeploymentIdentity.ReviewedSelectorSHA256 = liveDeploymentIdentity.SelectorSHA256
	liveDeploymentIdentity.Review = baseline.OperatorDeployment.Review
	baseline.OperatorDeployment = liveDeploymentIdentity
	baseline.OperatorImage = liveDeploymentIdentity.Image

	var mu sync.Mutex
	var calls []string
	record := func(call string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, call)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		record("effective")
		for name, values := range testOperatorAuthorityHeaders(baseline.AuthorityPlane.Operator) {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Enabled":false,"Workflows":{"Enabled":true,"SubstrateK8sOnly":true}}`))
	}))
	defer server.Close()
	h := New(Config{OperatorURL: server.URL, RequireAuthorityBinding: true})
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "get configmap"):
			record("configmap")
			return string(policyRaw), nil
		case strings.Contains(joined, "get deploy"):
			record("deployment")
			return string(deploymentRaw), nil
		default:
			return "", fmt.Errorf("unexpected kubectl call: %s", joined)
		}
	}
	evidence, err := h.CollectPolicyDeleteBoundaryEvidence(t.Context(), baseline)
	if err != nil {
		t.Fatalf("CollectPolicyDeleteBoundaryEvidence() error = %v", err)
	}
	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	if got := strings.Join(gotCalls, ","); got != "configmap,effective,deployment,configmap" {
		t.Fatalf("policy boundary call order = %s", got)
	}
	if err := ValidatePolicyDeleteBoundaryFreshness(time.Now().UTC(), baseline, evidence); err != nil {
		t.Fatalf("fresh collected policy boundary rejected: %v", err)
	}
}
