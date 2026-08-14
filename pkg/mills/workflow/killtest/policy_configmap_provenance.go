package killtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	policyConfigMapSourcePath = "k3s/mills/configmap-policy.yaml"
	policyChecksumAnnotation  = "loom.flexinfer.ai/policy-checksum"
	policyConfigMapFluxOwner  = "apps"
	policyConfigMapRenderer   = "flux build kustomization --dry-run"

	// PolicyDeleteBoundaryContract identifies the final A -> effective ->
	// operator Deployment -> B policy refresh immediately before a Pod DELETE.
	PolicyDeleteBoundaryContract        = "mills-s1c-policy-delete-boundary"
	PolicyDeleteBoundaryContractVersion = 1
)

// PolicyConfigMapBoundarySnapshot is one live, complete ConfigMap payload
// observation in the final delete-boundary bracket.
type PolicyConfigMapBoundarySnapshot struct {
	Identity         KubernetesObjectIdentity `json:"identity"`
	PayloadSHA256    string                   `json:"payload_sha256"`
	PolicyEnabled    bool                     `json:"policy_enabled"`
	WorkflowsEnabled bool                     `json:"workflows_enabled"`
	SubstrateK8sOnly bool                     `json:"substrate_k8s_only"`
	ObservedAt       time.Time                `json:"observed_at"`
}

// EffectivePolicyBoundarySnapshot is the operator's effective policy read
// between the two ConfigMap observations.
type EffectivePolicyBoundarySnapshot struct {
	PolicyEnabled     bool                      `json:"policy_enabled"`
	WorkflowsEnabled  bool                      `json:"workflows_enabled"`
	SubstrateK8sOnly  bool                      `json:"substrate_k8s_only"`
	ObservedAt        time.Time                 `json:"observed_at"`
	OperatorAuthority OperatorResponseAuthority `json:"operator_authority"`
}

// PolicyDeleteBoundaryEvidence proves the final foreground gate reads before
// DELETE retained the exact reviewed policy object, payload, operator checksum,
// and effective closed-admission policy. The independent process observer may
// sample concurrently; the policy reads are ordered but not atomic.
type PolicyDeleteBoundaryEvidence struct {
	Contract                     string                          `json:"contract"`
	ContractVersion              int                             `json:"contract_version"`
	ConfigMapA                   PolicyConfigMapBoundarySnapshot `json:"configmap_a"`
	Effective                    EffectivePolicyBoundarySnapshot `json:"effective"`
	OperatorDeployment           DeploymentIdentity              `json:"operator_deployment"`
	OperatorDeploymentObservedAt time.Time                       `json:"operator_deployment_observed_at"`
	ConfigMapB                   PolicyConfigMapBoundarySnapshot `json:"configmap_b"`
	Review                       PolicyConfigMapReviewIdentity   `json:"review"`
	CompletedAt                  time.Time                       `json:"completed_at"`
}

// reviewedPolicyConfigMap is reconstructed from the same apps Flux output as
// the reviewed operator Deployment. It is intentionally carried inside that
// reviewed Deployment cache entry so two independent renderer executions can
// never be combined into one policy proof.
type reviewedPolicyConfigMap struct {
	ConfigMap             *corev1.ConfigMap
	RenderedPayloadSHA256 string
	Review                PolicyConfigMapReviewIdentity
}

// policyConfigMapPayload is the complete policy-bearing ConfigMap surface.
// Metadata is separately bound by KubernetesObjectIdentity and review fields.
// Empty maps are normalized to {} so API omission and an explicit empty map
// have one semantic representation; immutable retains presence through *bool.
type policyConfigMapPayload struct {
	Data       map[string]string `json:"data"`
	BinaryData map[string][]byte `json:"binaryData"`
	Immutable  *bool             `json:"immutable"`
}

func canonicalPolicyConfigMapPayloadSHA256(configMap *corev1.ConfigMap) (string, error) {
	if configMap == nil {
		return "", errors.New("policy ConfigMap is missing")
	}
	data := configMap.Data
	if data == nil {
		data = map[string]string{}
	}
	binaryData := configMap.BinaryData
	if binaryData == nil {
		binaryData = map[string][]byte{}
	}
	canonical, err := json.Marshal(policyConfigMapPayload{
		Data: data, BinaryData: binaryData, Immutable: configMap.Immutable,
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(canonical)), nil
}

func exactPolicySourceSHA256(raw []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

// parsePolicyConfigMapPayload binds the API object identity, required
// policy.yaml entry, and complete payload digest from one kubectl response.
func parsePolicyConfigMapPayload(
	raw, expectedNamespace string,
) (KubernetesObjectIdentity, string, string, error) {
	identity, policy, err := parsePolicyConfigMap(raw, expectedNamespace)
	if err != nil {
		return identity, "", "", err
	}
	var configMap corev1.ConfigMap
	if err := json.Unmarshal([]byte(raw), &configMap); err != nil {
		return identity, "", "", fmt.Errorf("decode policy ConfigMap payload: %w", err)
	}
	if configMap.Name != identity.Name || configMap.Namespace != identity.Namespace {
		return identity, "", "", errors.New("policy ConfigMap payload identity differs from object identity")
	}
	digest, err := canonicalPolicyConfigMapPayloadSHA256(&configMap)
	if err != nil {
		return identity, "", "", err
	}
	return identity, policy, digest, nil
}

// selectRenderedPolicyConfigMap extracts exactly one desired ConfigMap from a
// reviewed Flux render. Unknown fields and API-server identity are rejected.
func selectRenderedPolicyConfigMap(raw []byte) (*corev1.ConfigMap, string, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var selected *corev1.ConfigMap
	selectedDigest := ""
	for documentIndex := 1; ; documentIndex++ {
		var document yaml.Node
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("decode rendered document %d: %w", documentIndex, err)
		}
		if yamlDocumentIsEmpty(&document) {
			continue
		}
		var genericDocument any
		if err := document.Decode(&genericDocument); err != nil {
			return nil, "", err
		}
		jsonDocument, err := json.Marshal(genericDocument)
		if err != nil {
			return nil, "", fmt.Errorf("convert rendered document %d to canonical JSON: %w", documentIndex, err)
		}
		var envelope struct {
			APIVersion string            `json:"apiVersion"`
			Kind       string            `json:"kind"`
			Metadata   metav1.ObjectMeta `json:"metadata"`
		}
		if err := json.Unmarshal(jsonDocument, &envelope); err != nil {
			return nil, "", fmt.Errorf("decode rendered document %d identity: %w", documentIndex, err)
		}
		if envelope.APIVersion != "v1" || envelope.Kind != "ConfigMap" ||
			envelope.Metadata.Namespace != s1cOperatorNamespace || envelope.Metadata.Name != policyConfigMapName {
			continue
		}
		if selected != nil {
			return nil, "", fmt.Errorf("render contains duplicate ConfigMap %s/%s",
				s1cOperatorNamespace, policyConfigMapName)
		}
		if envelope.Metadata.UID != "" || envelope.Metadata.ResourceVersion != "" ||
			envelope.Metadata.Generation != 0 || envelope.Metadata.DeletionTimestamp != nil ||
			!envelope.Metadata.CreationTimestamp.IsZero() || len(envelope.Metadata.ManagedFields) != 0 {
			return nil, "", fmt.Errorf("reviewed ConfigMap %s/%s contains live object identity",
				s1cOperatorNamespace, policyConfigMapName)
		}
		strict := json.NewDecoder(bytes.NewReader(jsonDocument))
		strict.DisallowUnknownFields()
		var configMap corev1.ConfigMap
		if err := strict.Decode(&configMap); err != nil {
			return nil, "", fmt.Errorf("strictly decode reviewed ConfigMap %s/%s: %w",
				s1cOperatorNamespace, policyConfigMapName, err)
		}
		if err := requireJSONEOF(strict); err != nil {
			return nil, "", err
		}
		if _, ok := configMap.Data["policy.yaml"]; !ok {
			return nil, "", errors.New(`reviewed policy ConfigMap data is missing "policy.yaml"`)
		}
		digest, err := canonicalPolicyConfigMapPayloadSHA256(&configMap)
		if err != nil {
			return nil, "", err
		}
		selected, selectedDigest = &configMap, digest
	}
	if selected == nil {
		return nil, "", fmt.Errorf("render contains no exact ConfigMap %s/%s",
			s1cOperatorNamespace, policyConfigMapName)
	}
	return selected, selectedDigest, nil
}

func reviewedPolicyConfigMapFromFluxRender(
	rendered, exactSource []byte,
	operatorDeployment *appsv1.Deployment,
	review PolicyConfigMapReviewIdentity,
) (reviewedPolicyConfigMap, error) {
	var result reviewedPolicyConfigMap
	configMap, payloadSHA, err := selectRenderedPolicyConfigMap(rendered)
	if err != nil {
		return result, err
	}
	if operatorDeployment == nil {
		return result, errors.New("reviewed operator Deployment is missing")
	}
	sourceSHA := exactPolicySourceSHA256(exactSource)
	checksum := operatorDeployment.Spec.Template.Annotations[policyChecksumAnnotation]
	if !isNormalizedSHA256(checksum) || checksum != sourceSHA {
		return result, fmt.Errorf("reviewed operator policy checksum %q differs from exact source SHA-256 %q",
			checksum, sourceSHA)
	}
	review.Name = policyConfigMapName
	review.Namespace = s1cOperatorNamespace
	review.SourcePath = policyConfigMapSourcePath
	review.SourceSHA256 = sourceSHA
	review.RenderedPayloadSHA256 = payloadSHA
	review.LivePayloadSHA256 = ""
	if err := validatePolicyConfigMapReviewShape(review, false); err != nil {
		return result, fmt.Errorf("reviewed policy identity: %w", err)
	}
	return reviewedPolicyConfigMap{
		ConfigMap: configMap, RenderedPayloadSHA256: payloadSHA, Review: review,
	}, nil
}

func cloneReviewedPolicyConfigMap(source reviewedPolicyConfigMap) reviewedPolicyConfigMap {
	if source.ConfigMap != nil {
		source.ConfigMap = source.ConfigMap.DeepCopy()
	}
	return source
}

func validatePolicyConfigMapReviewShape(review PolicyConfigMapReviewIdentity, requireLive bool) error {
	if review.Contract != PolicyConfigMapProvenanceContract ||
		review.ContractVersion != PolicyConfigMapProvenanceContractVersion {
		return fmt.Errorf("unsupported contract %q version %d", review.Contract, review.ContractVersion)
	}
	if review.Name != policyConfigMapName || review.Namespace != s1cOperatorNamespace {
		return fmt.Errorf("redirected ConfigMap %s/%s", review.Namespace, review.Name)
	}
	if review.FluxOwner != policyConfigMapFluxOwner || !isNormalizedSHA256(review.FluxSpecSHA256) {
		return fmt.Errorf("invalid Flux owner/spec identity %q/%q", review.FluxOwner, review.FluxSpecSHA256)
	}
	if review.Renderer != policyConfigMapRenderer || strings.TrimSpace(review.RendererVersion) == "" ||
		strings.TrimSpace(review.RendererVersion) != review.RendererVersion {
		return fmt.Errorf("unsupported renderer identity %q/%q", review.Renderer, review.RendererVersion)
	}
	if normalized, err := normalizeGitOpsScopeRevision(review.PlatformRevision); err != nil ||
		normalized != review.PlatformRevision {
		return fmt.Errorf("invalid platform baseline %q/%q", review.PlatformRevision, review.PlatformScopeDigest)
	}
	if normalized, err := normalizeGitOpsScopeRevision(review.PlatformScopeDigest); err != nil ||
		normalized != review.PlatformScopeDigest {
		return fmt.Errorf("invalid platform scope digest %q", review.PlatformScopeDigest)
	}
	if review.SourcePath != policyConfigMapSourcePath || !isNormalizedSHA256(review.SourceSHA256) {
		return fmt.Errorf("invalid policy source identity %q/%q", review.SourcePath, review.SourceSHA256)
	}
	if !isNormalizedSHA256(review.RenderedPayloadSHA256) {
		return fmt.Errorf("invalid rendered policy payload SHA-256 %q", review.RenderedPayloadSHA256)
	}
	if requireLive && !isNormalizedSHA256(review.LivePayloadSHA256) {
		return fmt.Errorf("invalid live policy payload SHA-256 %q", review.LivePayloadSHA256)
	}
	if !requireLive && review.LivePayloadSHA256 != "" {
		return fmt.Errorf("reviewed policy unexpectedly contains live payload SHA-256 %q", review.LivePayloadSHA256)
	}
	return nil
}

func bindReviewedPolicyConfigMap(
	identity KubernetesObjectIdentity,
	livePayloadSHA, liveDeploymentChecksum string,
	reviewed reviewedPolicyConfigMap,
) (PolicyConfigMapReviewIdentity, error) {
	if err := ValidateKubernetesObjectIdentity(identity, s1cOperatorNamespace, policyConfigMapName); err != nil {
		return PolicyConfigMapReviewIdentity{}, err
	}
	if reviewed.ConfigMap == nil || reviewed.ConfigMap.Name != policyConfigMapName ||
		reviewed.ConfigMap.Namespace != s1cOperatorNamespace {
		return PolicyConfigMapReviewIdentity{}, errors.New("reviewed policy ConfigMap is missing or redirected")
	}
	if err := validatePolicyConfigMapReviewShape(reviewed.Review, false); err != nil {
		return PolicyConfigMapReviewIdentity{}, err
	}
	recomputedReviewedSHA, err := canonicalPolicyConfigMapPayloadSHA256(reviewed.ConfigMap)
	if err != nil {
		return PolicyConfigMapReviewIdentity{}, err
	}
	if recomputedReviewedSHA != reviewed.RenderedPayloadSHA256 {
		return PolicyConfigMapReviewIdentity{}, errors.New("reviewed policy ConfigMap payload differs from cached digest")
	}
	if reviewed.RenderedPayloadSHA256 != reviewed.Review.RenderedPayloadSHA256 {
		return PolicyConfigMapReviewIdentity{}, errors.New("reviewed policy payload cache differs from serialized review")
	}
	if !isNormalizedSHA256(livePayloadSHA) || livePayloadSHA != reviewed.RenderedPayloadSHA256 {
		return PolicyConfigMapReviewIdentity{}, fmt.Errorf(
			"live policy ConfigMap payload SHA-256 %q differs from reviewed render %q",
			livePayloadSHA, reviewed.RenderedPayloadSHA256,
		)
	}
	if !isNormalizedSHA256(liveDeploymentChecksum) ||
		liveDeploymentChecksum != reviewed.Review.SourceSHA256 {
		return PolicyConfigMapReviewIdentity{}, fmt.Errorf(
			"live operator policy checksum %q differs from exact reviewed source SHA-256 %q",
			liveDeploymentChecksum, reviewed.Review.SourceSHA256,
		)
	}
	review := reviewed.Review
	review.LivePayloadSHA256 = livePayloadSHA
	return review, nil
}

// ValidatePolicyConfigMapProvenance is the pure/offline evaluator for the
// reviewed policy proof. It cross-binds ConfigMap identity and complete live
// payload to the apps Flux spec, platform baseline, exact source bytes, and
// both the reviewed and live operator Deployment checksum.
func ValidatePolicyConfigMapProvenance(report PreflightReport) error {
	if err := ValidateKubernetesObjectIdentity(
		report.PolicyConfigMapIdentity, s1cOperatorNamespace, policyConfigMapName,
	); err != nil {
		return fmt.Errorf("live policy ConfigMap identity: %w", err)
	}
	review := report.PolicyConfigMapReview
	if err := validatePolicyConfigMapReviewShape(review, true); err != nil {
		return fmt.Errorf("policy ConfigMap review identity: %w", err)
	}
	if review.Name != report.PolicyConfigMapIdentity.Name ||
		review.Namespace != report.PolicyConfigMapIdentity.Namespace {
		return errors.New("policy review identity differs from live ConfigMap identity")
	}
	if review.RenderedPayloadSHA256 != review.LivePayloadSHA256 {
		return fmt.Errorf("reviewed/live policy payload SHA-256 mismatch: %q/%q",
			review.RenderedPayloadSHA256, review.LivePayloadSHA256)
	}
	if !isNormalizedSHA256(report.PolicyChecksum) ||
		report.PolicyChecksum != report.OperatorDeployment.PolicyChecksum ||
		report.PolicyChecksum != review.SourceSHA256 {
		return fmt.Errorf("exact policy source SHA-256 is not bound across source/report/live Deployment: %q/%q/%q",
			review.SourceSHA256, report.PolicyChecksum, report.OperatorDeployment.PolicyChecksum)
	}
	sources := fluxProvenanceByName(report.FluxSourcesEnd)
	apps, ok := sources[policyConfigMapFluxOwner]
	if !ok {
		return errors.New("policy review has no apps Flux owner")
	}
	if review.FluxSpecSHA256 != apps.RenderSpec.SpecSHA256 {
		return errors.New("policy review is not bound to the apps Flux full spec")
	}
	if review.PlatformRevision != apps.ProtectedIdentity.BaselineRevision ||
		review.PlatformScopeDigest != apps.ProtectedIdentity.BaselineDigest {
		return errors.New("policy review is not bound to the platform baseline")
	}
	operatorReview := report.OperatorDeployment.Review
	if operatorReview.FluxOwner != policyConfigMapFluxOwner ||
		operatorReview.FluxSpecSHA256 != review.FluxSpecSHA256 ||
		operatorReview.PlatformRevision != review.PlatformRevision ||
		operatorReview.PlatformScopeDigest != review.PlatformScopeDigest ||
		operatorReview.Renderer != review.Renderer ||
		operatorReview.RendererVersion != review.RendererVersion {
		return errors.New("policy ConfigMap and operator Deployment were not reconstructed by one reviewed apps render identity")
	}
	return nil
}

// ValidatePolicyConfigMapGateIdentity freezes the complete reviewed policy
// proof across every immediate/final preflight in the destructive gate.
func ValidatePolicyConfigMapGateIdentity(initial, observed PreflightReport) error {
	if err := ValidatePolicyConfigMapProvenance(initial); err != nil {
		return fmt.Errorf("initial policy provenance: %w", err)
	}
	if err := ValidatePolicyConfigMapProvenance(observed); err != nil {
		return fmt.Errorf("observed policy provenance: %w", err)
	}
	if initial.PolicyConfigMapReview != observed.PolicyConfigMapReview {
		return fmt.Errorf("policy ConfigMap review identity changed: %+v -> %+v",
			initial.PolicyConfigMapReview, observed.PolicyConfigMapReview)
	}
	return nil
}

// CollectPolicyDeleteBoundaryEvidence performs the final bounded policy
// bracket. ObservedAt values are captured at request start so network latency
// consumes, rather than hides, freshness budget.
func (h *Harness) CollectPolicyDeleteBoundaryEvidence(
	parent context.Context,
	baseline PreflightReport,
) (PolicyDeleteBoundaryEvidence, error) {
	var evidence PolicyDeleteBoundaryEvidence
	if err := ValidatePolicyConfigMapProvenance(baseline); err != nil {
		return evidence, fmt.Errorf("baseline policy provenance: %w", err)
	}
	ctx, cancel := context.WithTimeout(parent, h.finalPreDeleteCheckTimeout)
	defer cancel()

	evidence.Contract = PolicyDeleteBoundaryContract
	evidence.ContractVersion = PolicyDeleteBoundaryContractVersion
	evidence.Review = baseline.PolicyConfigMapReview
	var err error
	evidence.ConfigMapA, err = h.readPolicyConfigMapBoundarySnapshot(ctx)
	if err != nil {
		return evidence, fmt.Errorf("policy ConfigMap A: %w", err)
	}
	evidence.Effective.ObservedAt = time.Now().UTC()
	evidence.Effective.PolicyEnabled, evidence.Effective.WorkflowsEnabled,
		evidence.Effective.SubstrateK8sOnly, evidence.Effective.OperatorAuthority, err = h.effectivePolicy(ctx)
	if err != nil {
		return evidence, fmt.Errorf("effective policy: %w", err)
	}
	evidence.OperatorDeploymentObservedAt = time.Now().UTC()
	evidence.OperatorDeployment, err = h.stableDeployment(
		ctx, h.cfg.OperatorNS, "loom-mills-operator",
	)
	if err != nil {
		return evidence, fmt.Errorf("live policy-bearing operator Deployment: %w", err)
	}
	evidence.ConfigMapB, err = h.readPolicyConfigMapBoundarySnapshot(ctx)
	if err != nil {
		return evidence, fmt.Errorf("policy ConfigMap B: %w", err)
	}
	evidence.CompletedAt = time.Now().UTC()
	if err := ctx.Err(); err != nil {
		return evidence, fmt.Errorf("policy delete-boundary refresh exceeded %s: %w",
			h.finalPreDeleteCheckTimeout, err)
	}
	if err := ValidatePolicyDeleteBoundaryEvidence(baseline, evidence); err != nil {
		return evidence, err
	}
	return evidence, nil
}

func (h *Harness) readPolicyConfigMapBoundarySnapshot(
	ctx context.Context,
) (PolicyConfigMapBoundarySnapshot, error) {
	snapshot := PolicyConfigMapBoundarySnapshot{ObservedAt: time.Now().UTC()}
	raw, err := h.kubectl(
		ctx, "-n", h.cfg.OperatorNS, "get", "configmap", policyConfigMapName, "-o", "json",
	)
	if err != nil {
		return snapshot, err
	}
	var policyRaw string
	snapshot.Identity, policyRaw, snapshot.PayloadSHA256, err = parsePolicyConfigMapPayload(
		raw, h.cfg.OperatorNS,
	)
	if err != nil {
		return snapshot, err
	}
	snapshot.PolicyEnabled, snapshot.WorkflowsEnabled, snapshot.SubstrateK8sOnly, err =
		parseWorkflowPolicy(policyRaw)
	if err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

// ValidatePolicyDeleteBoundaryEvidence is the pure/offline evaluator for the
// final policy bracket. The live reads must retain the exact immediate-
// preflight object/payload and reviewed source checksum.
func ValidatePolicyDeleteBoundaryEvidence(
	baseline PreflightReport,
	evidence PolicyDeleteBoundaryEvidence,
) error {
	if err := ValidatePolicyConfigMapProvenance(baseline); err != nil {
		return fmt.Errorf("baseline policy provenance: %w", err)
	}
	if evidence.Contract != PolicyDeleteBoundaryContract ||
		evidence.ContractVersion != PolicyDeleteBoundaryContractVersion {
		return fmt.Errorf("unsupported policy delete-boundary contract %q version %d",
			evidence.Contract, evidence.ContractVersion)
	}
	if evidence.Review != baseline.PolicyConfigMapReview {
		return errors.New("policy delete-boundary review differs from immediate preflight")
	}
	if baseline.FluxSourcesEnd.ObservedAt.IsZero() || evidence.ConfigMapA.ObservedAt.IsZero() ||
		evidence.Effective.ObservedAt.IsZero() || evidence.OperatorDeploymentObservedAt.IsZero() ||
		evidence.ConfigMapB.ObservedAt.IsZero() || evidence.CompletedAt.IsZero() {
		return errors.New("policy delete-boundary timestamps are incomplete")
	}
	ordered := []struct {
		label string
		at    time.Time
	}{
		{"immediate preflight", baseline.FluxSourcesEnd.ObservedAt},
		{"ConfigMap A", evidence.ConfigMapA.ObservedAt},
		{"effective policy", evidence.Effective.ObservedAt},
		{"operator Deployment", evidence.OperatorDeploymentObservedAt},
		{"ConfigMap B", evidence.ConfigMapB.ObservedAt},
		{"bracket completion", evidence.CompletedAt},
	}
	for index := 1; index < len(ordered); index++ {
		if !ordered[index-1].at.Before(ordered[index].at) {
			return fmt.Errorf("policy delete-boundary timestamps are not ordered: %s=%s %s=%s",
				ordered[index-1].label, ordered[index-1].at,
				ordered[index].label, ordered[index].at)
		}
	}
	for _, sample := range []struct {
		label    string
		snapshot PolicyConfigMapBoundarySnapshot
	}{
		{"ConfigMap A", evidence.ConfigMapA},
		{"ConfigMap B", evidence.ConfigMapB},
	} {
		if err := ValidateKubernetesObjectIdentity(
			sample.snapshot.Identity, s1cOperatorNamespace, policyConfigMapName,
		); err != nil {
			return fmt.Errorf("%s identity: %w", sample.label, err)
		}
		if sample.snapshot.Identity != baseline.PolicyConfigMapIdentity {
			return fmt.Errorf("%s identity differs from immediate preflight: %+v != %+v",
				sample.label, sample.snapshot.Identity, baseline.PolicyConfigMapIdentity)
		}
		if !isNormalizedSHA256(sample.snapshot.PayloadSHA256) ||
			sample.snapshot.PayloadSHA256 != baseline.PolicyConfigMapReview.LivePayloadSHA256 ||
			sample.snapshot.PayloadSHA256 != baseline.PolicyConfigMapReview.RenderedPayloadSHA256 {
			return fmt.Errorf("%s complete payload differs from reviewed/live immediate preflight", sample.label)
		}
		if sample.snapshot.PolicyEnabled != baseline.ConfigMapPolicyEnabled ||
			sample.snapshot.WorkflowsEnabled != baseline.FlagEnabled ||
			sample.snapshot.SubstrateK8sOnly != baseline.SubstrateK8sOnly {
			return fmt.Errorf("%s parsed policy differs from immediate preflight", sample.label)
		}
	}
	if evidence.ConfigMapA.Identity != evidence.ConfigMapB.Identity ||
		evidence.ConfigMapA.PayloadSHA256 != evidence.ConfigMapB.PayloadSHA256 ||
		evidence.ConfigMapA.PolicyEnabled != evidence.ConfigMapB.PolicyEnabled ||
		evidence.ConfigMapA.WorkflowsEnabled != evidence.ConfigMapB.WorkflowsEnabled ||
		evidence.ConfigMapA.SubstrateK8sOnly != evidence.ConfigMapB.SubstrateK8sOnly {
		return errors.New("policy ConfigMap changed across A/B delete-boundary bracket")
	}
	if evidence.Effective.PolicyEnabled != baseline.EffectivePolicyEnabled ||
		evidence.Effective.WorkflowsEnabled != baseline.EffectiveFlagEnabled ||
		evidence.Effective.SubstrateK8sOnly != baseline.EffectiveSubstrateK8sOnly {
		return errors.New("effective policy differs from immediate preflight")
	}
	if evidence.Effective.OperatorAuthority != baseline.AuthorityPlane.Operator ||
		evidence.Effective.OperatorAuthority != baseline.EffectivePolicyAuthority {
		return fmt.Errorf("effective policy REST authority differs from immediate operator: boundary=%+v preflight=%+v",
			evidence.Effective.OperatorAuthority, baseline.AuthorityPlane.Operator)
	}
	if evidence.ConfigMapB.PolicyEnabled || !evidence.ConfigMapB.WorkflowsEnabled ||
		!evidence.ConfigMapB.SubstrateK8sOnly || evidence.Effective.PolicyEnabled ||
		!evidence.Effective.WorkflowsEnabled || !evidence.Effective.SubstrateK8sOnly {
		return errors.New("delete-boundary policy is not closed-admission, workflow-enabled, and k8s-only")
	}
	if evidence.ConfigMapB.PolicyEnabled != evidence.Effective.PolicyEnabled ||
		evidence.ConfigMapB.WorkflowsEnabled != evidence.Effective.WorkflowsEnabled ||
		evidence.ConfigMapB.SubstrateK8sOnly != evidence.Effective.SubstrateK8sOnly {
		return errors.New("delete-boundary ConfigMap and effective policy differ")
	}
	if err := sameLiveDeploymentIdentity(
		baseline.OperatorDeployment, evidence.OperatorDeployment,
	); err != nil {
		return fmt.Errorf("live policy-bearing operator Deployment differs from immediate preflight: %w", err)
	}
	deployment := evidence.OperatorDeployment
	if deployment.Name != "loom-mills-operator" || deployment.Namespace != s1cOperatorNamespace ||
		strings.TrimSpace(deployment.UID) == "" || strings.TrimSpace(deployment.ResourceVersion) == "" ||
		deployment.Generation <= 0 || deployment.ObservedGeneration != deployment.Generation ||
		deployment.DesiredReplicas != 1 || deployment.Replicas != 1 || deployment.UpdatedReplicas != 1 ||
		deployment.ReadyReplicas != 1 || deployment.AvailableReplicas != 1 ||
		strings.TrimSpace(deployment.Image) == "" || deployment.Strategy != "Recreate" ||
		!isNormalizedSHA256(deployment.SpecSHA256) ||
		!isNormalizedSHA256(deployment.PodTemplateSHA256) ||
		!isNormalizedSHA256(deployment.SelectorSHA256) {
		return fmt.Errorf("live policy-bearing operator Deployment is not a stable singleton: %+v", deployment)
	}
	if evidence.OperatorDeployment.PolicyChecksum != evidence.Review.SourceSHA256 {
		return errors.New("live operator Deployment checksum differs from exact reviewed policy source")
	}
	return nil
}

// ValidatePolicyDeleteBoundaryFreshness binds the completed policy bracket to
// the exact DELETE handoff. ConfigMap B's request start is the conservative
// freshness clock; slow response time consumes the allowance.
func ValidatePolicyDeleteBoundaryFreshness(
	deleteAt time.Time,
	baseline PreflightReport,
	evidence PolicyDeleteBoundaryEvidence,
) error {
	if err := ValidatePolicyDeleteBoundaryEvidence(baseline, evidence); err != nil {
		return err
	}
	if deleteAt.IsZero() || !evidence.CompletedAt.Before(deleteAt) {
		return fmt.Errorf("policy delete-boundary completion %s is not before DELETE %s",
			evidence.CompletedAt, deleteAt)
	}
	if age := deleteAt.Sub(evidence.ConfigMapB.ObservedAt); age > finalPreDeleteCheckTimeout {
		return fmt.Errorf("policy ConfigMap B proof is %s old at DELETE, exceeds %s",
			age, finalPreDeleteCheckTimeout)
	}
	return nil
}
