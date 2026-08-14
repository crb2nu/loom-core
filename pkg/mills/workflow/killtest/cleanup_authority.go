package killtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// CleanupAuthorityError is a fail-closed cleanup result. It marks an inability
// to bind the mitigation client to the original reviewed cluster/Deployment
// root and the current operator Pod+BootID. It is intentionally also
// errors.Is-compatible with ErrOperatorAuthority so cleanup loops stop rather
// than treating an authority change as an ambiguous, retryable transport loss.
type CleanupAuthorityError struct {
	Stage string
	Cause error
}

func (err *CleanupAuthorityError) Error() string {
	if err == nil {
		return "cleanup authority recovery rejected"
	}
	if err.Cause == nil {
		return "cleanup authority recovery rejected at " + err.Stage
	}
	return fmt.Sprintf("cleanup authority recovery rejected at %s: %v", err.Stage, err.Cause)
}

func (err *CleanupAuthorityError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

func (err *CleanupAuthorityError) Is(target error) bool {
	return target == ErrOperatorAuthority
}

func cleanupAuthorityError(stage string, cause error) error {
	var existing *CleanupAuthorityError
	if errors.As(cause, &existing) {
		return cause
	}
	return &CleanupAuthorityError{Stage: stage, Cause: cause}
}

type cleanupNamespaceWire struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name              string  `json:"name"`
		UID               string  `json:"uid"`
		ResourceVersion   string  `json:"resourceVersion"`
		DeletionTimestamp *string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

func parseActiveCleanupNamespace(raw, expectedName string) (KubernetesObjectIdentity, error) {
	var namespace cleanupNamespaceWire
	if err := json.Unmarshal([]byte(raw), &namespace); err != nil {
		return KubernetesObjectIdentity{}, fmt.Errorf("decode Namespace: %w", err)
	}
	identity := KubernetesObjectIdentity{
		Name: namespace.Metadata.Name, UID: namespace.Metadata.UID,
		ResourceVersion: namespace.Metadata.ResourceVersion,
		Terminating:     namespace.Metadata.DeletionTimestamp != nil,
	}
	if namespace.Metadata.DeletionTimestamp != nil {
		identity.DeletionTimestamp = *namespace.Metadata.DeletionTimestamp
	}
	if namespace.APIVersion != "v1" || namespace.Kind != "Namespace" ||
		identity.Name != expectedName || strings.TrimSpace(identity.UID) == "" ||
		strings.TrimSpace(identity.ResourceVersion) == "" || identity.Terminating ||
		namespace.Status.Phase != "Active" {
		return KubernetesObjectIdentity{}, fmt.Errorf(
			"namespace %q is not one exact active object: apiVersion=%q kind=%q identity=%+v phase=%q",
			expectedName, namespace.APIVersion, namespace.Kind, identity, namespace.Status.Phase,
		)
	}
	return identity, nil
}

func validateCleanupClusterAuthority(authority KubernetesClusterAuthority) error {
	if authority.Contract != AuthorityPlaneContract ||
		authority.ContractVersion != AuthorityPlaneContractVersion ||
		!isNormalizedSHA256(authority.PublicAuthoritySHA256) ||
		!isNormalizedSHA256(authority.APIServerSHA256) ||
		!isNormalizedSHA256(authority.CertificateAuthoritySHA256) ||
		strings.TrimSpace(authority.ContextName) == "" {
		return fmt.Errorf("original Kubernetes authority root is incomplete: %+v", authority)
	}
	return ValidateKubernetesObjectIdentity(authority.OperatorNamespaceIdentity, "", s1cOperatorNamespace)
}

func validateCleanupReviewedOperatorDeployment(deployment DeploymentIdentity) error {
	review := deployment.Review
	if deployment.Name != "loom-mills-operator" || deployment.Namespace != s1cOperatorNamespace ||
		strings.TrimSpace(deployment.UID) == "" ||
		!isNormalizedSHA256(deployment.SpecSHA256) ||
		deployment.SpecSHA256 != deployment.ReviewedSpecSHA256 ||
		!isNormalizedSHA256(deployment.PodTemplateSHA256) ||
		deployment.PodTemplateSHA256 != deployment.ReviewedPodTemplateSHA256 ||
		!isNormalizedSHA256(deployment.SelectorSHA256) ||
		deployment.SelectorSHA256 != deployment.ReviewedSelectorSHA256 {
		return fmt.Errorf("original reviewed operator Deployment root is incomplete: %+v", deployment)
	}
	if review.Contract != DeploymentProvenanceContract ||
		review.ContractVersion != DeploymentProvenanceContractVersion ||
		review.FluxOwner != "apps" || !isNormalizedSHA256(review.FluxSpecSHA256) ||
		strings.TrimSpace(review.Renderer) == "" || strings.TrimSpace(review.RendererVersion) == "" ||
		!isNormalizedSHA256(review.RenderedSpecSHA256) ||
		strings.TrimSpace(review.PlatformRevision) == "" ||
		!isNormalizedSHA256(review.PlatformScopeDigest) ||
		strings.TrimSpace(review.SourceRevision) == "" ||
		!isNormalizedSHA256(review.SourceScopeDigest) {
		return fmt.Errorf("original operator Deployment review identity is incomplete: %+v", review)
	}
	return nil
}

// cleanupMitigationHarness creates an ephemeral authority state that is never
// copied back into the gate harness. This permits fail-safe /fail cleanup after
// an unplanned operator replacement while ensuring that replacement can never
// become passing crash evidence.
func (h *Harness) cleanupMitigationHarness(ctx context.Context, runID string) (*Harness, error) {
	h.authorityMu.Lock()
	clusterRoot := h.clusterAuthority
	reviewedDeployment := h.reviewedOperatorDeployment
	h.authorityMu.Unlock()

	if err := validateCleanupClusterAuthority(clusterRoot); err != nil {
		return nil, cleanupAuthorityError("original cluster root", err)
	}
	if err := validateCleanupReviewedOperatorDeployment(reviewedDeployment); err != nil {
		return nil, cleanupAuthorityError("original reviewed Deployment root", err)
	}

	mitigation := New(h.cfg)
	mitigation.http = h.http
	mitigation.kubectlFn = h.kubectlFn
	mitigation.dryRunCreatePodFn = h.dryRunCreatePodFn
	mitigation.reviewedOperatorDeployment = reviewedDeployment

	// Production cleanup must continue through the exact frozen kubeconfig that
	// established the gate root. Unit seams already represent that transport and
	// deliberately avoid reading developer kubeconfig state.
	if h.kubectlFn == nil {
		frozen, err := h.ensureFrozenKubeConfig()
		if err != nil {
			return nil, cleanupAuthorityError("frozen Kubernetes transport", err)
		}
		mitigation.kubeConfigOnce.Do(func() { mitigation.frozenKube = frozen })
	} else if h.frozenKube != nil {
		mitigation.kubeConfigOnce.Do(func() { mitigation.frozenKube = h.frozenKube })
	}

	namespacePath := "/api/v1/namespaces/" + url.PathEscape(h.cfg.OperatorNS)
	namespaceRaw, err := mitigation.kubectl(ctx, "get", "--raw", namespacePath)
	if err != nil {
		return nil, cleanupAuthorityError("current operator Namespace read", err)
	}
	currentNamespace, err := parseActiveCleanupNamespace(namespaceRaw, h.cfg.OperatorNS)
	if err != nil {
		return nil, cleanupAuthorityError("current operator Namespace proof", err)
	}
	if currentNamespace.UID != clusterRoot.OperatorNamespaceIdentity.UID {
		return nil, cleanupAuthorityError("current operator Namespace proof", fmt.Errorf(
			"namespace UID changed from %q to %q",
			clusterRoot.OperatorNamespaceIdentity.UID, currentNamespace.UID,
		))
	}
	clusterRoot.OperatorNamespaceIdentity = currentNamespace

	currentDeployment, err := mitigation.stableDeployment(ctx, h.cfg.OperatorNS, reviewedDeployment.Name)
	if err != nil {
		return nil, cleanupAuthorityError("current operator Deployment read", err)
	}
	if err := sameLiveDeploymentIdentity(reviewedDeployment, currentDeployment); err != nil {
		return nil, cleanupAuthorityError("current reviewed operator Deployment proof", err)
	}
	currentPod, err := mitigation.readyPod(ctx, h.cfg.OperatorNS, h.cfg.OperatorSelector, currentDeployment)
	if err != nil {
		return nil, cleanupAuthorityError("current operator Deployment-to-Pod proof", err)
	}

	// Carry the immutable review fields onto the freshly read live identity. The
	// live fields were compared above; this makes the mitigation's expected root
	// explicit without reconstructing or weakening the original reviewed proof.
	currentDeployment.ReviewedSpecSHA256 = reviewedDeployment.ReviewedSpecSHA256
	currentDeployment.ReviewedPodTemplateSHA256 = reviewedDeployment.ReviewedPodTemplateSHA256
	currentDeployment.ReviewedSelectorSHA256 = reviewedDeployment.ReviewedSelectorSHA256
	currentDeployment.Review = reviewedDeployment.Review
	if err := sameDeploymentGateIdentity(reviewedDeployment, currentDeployment); err != nil {
		return nil, cleanupAuthorityError("current reviewed operator Deployment proof", err)
	}

	mitigation.authorityMu.Lock()
	mitigation.clusterAuthority = clusterRoot
	mitigation.expectedOperatorPod = currentPod
	mitigation.expectedOperatorDeployment = currentDeployment
	mitigation.authorityMu.Unlock()

	// GET is side-effect free. Its response must attest the exact selected Pod
	// and a valid BootID before mitigation is allowed to issue /fail.
	_, probeErr := mitigation.GetRun(ctx, runID)
	if errors.Is(probeErr, ErrOperatorAuthority) {
		return nil, cleanupAuthorityError("current operator response proof", probeErr)
	}
	if _, err := mitigation.currentAuthorityPlane(); err != nil {
		return nil, cleanupAuthorityError("current operator response proof", errors.Join(probeErr, err))
	}
	return mitigation, nil
}
