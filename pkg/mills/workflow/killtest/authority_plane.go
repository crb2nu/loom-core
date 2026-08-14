package killtest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	AuthorityPlaneContract        = "mills-s1c-authority-plane"
	AuthorityPlaneContractVersion = 1

	OperatorAuthorityContract        = "mills-s1c-operator-authority"
	OperatorAuthorityContractVersion = 1

	operatorAuthorityContractHeader       = "X-Loom-Mills-Authority-Contract"
	operatorAuthorityVersionHeader        = "X-Loom-Mills-Authority-Version"
	operatorAuthorityPodNameHeader        = "X-Loom-Mills-Pod-Name"
	operatorAuthorityPodNamespaceHeader   = "X-Loom-Mills-Pod-Namespace"
	operatorAuthorityPodUIDHeader         = "X-Loom-Mills-Pod-Uid"
	operatorAuthorityDeploymentNameHeader = "X-Loom-Mills-Deployment-Name"
	operatorAuthorityBootIDHeader         = "X-Loom-Mills-Boot-Id"
)

// ErrOperatorAuthority marks a response whose operational backend identity
// could not be bound to the selected operator incarnation. Callers must treat
// it as terminal, never as an ambiguous transport loss eligible for retry.
var ErrOperatorAuthority = errors.New("operator response authority rejected")

// OperatorResponseAuthority is the Downward-API identity attested by the
// operator on one HTTP response. The harness accepts it only after binding the
// Pod UID to the independently read Pod -> ReplicaSet -> Deployment chain.
type OperatorResponseAuthority struct {
	Contract        string `json:"contract"`
	ContractVersion int    `json:"contract_version"`
	PodName         string `json:"pod_name"`
	PodNamespace    string `json:"pod_namespace"`
	PodUID          string `json:"pod_uid"`
	DeploymentName  string `json:"deployment_name"`
	BootID          string `json:"boot_id"`
}

// KubernetesClusterAuthority identifies the immutable transport source used
// by both kubectl and client-go plus the live operator Namespace anchor. Hashes
// intentionally cover only public trust/endpoint material, never bearer or
// exec-plugin credentials.
type KubernetesClusterAuthority struct {
	Contract                   string                   `json:"contract"`
	ContractVersion            int                      `json:"contract_version"`
	PublicAuthoritySHA256      string                   `json:"public_authority_sha256"`
	APIServerSHA256            string                   `json:"api_server_sha256"`
	CertificateAuthoritySHA256 string                   `json:"certificate_authority_sha256"`
	ContextName                string                   `json:"context_name"`
	OperatorNamespaceIdentity  KubernetesObjectIdentity `json:"operator_namespace_identity"`
}

// AuthorityPlaneEvidence is the cross-plane binding serialized at every
// preflight. OperatorDeploymentUID is derived from the exact owner chain, not
// trusted from an HTTP header that the Pod cannot obtain through Downward API.
type AuthorityPlaneEvidence struct {
	Contract              string                     `json:"contract"`
	ContractVersion       int                        `json:"contract_version"`
	Kubernetes            KubernetesClusterAuthority `json:"kubernetes"`
	Operator              OperatorResponseAuthority  `json:"operator"`
	OperatorDeploymentUID string                     `json:"operator_deployment_uid"`
}

type frozenKubeConfig struct {
	path      string
	dir       string
	rest      *rest.Config
	authority KubernetesClusterAuthority
}

func sha256String(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func normalizedAPIServer(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid Kubernetes API server %q", raw)
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("kubernetes API server must use https: %q", raw)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("kubernetes API server must not contain userinfo: %q", raw)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("kubernetes API server must not contain query or fragment data: %q", raw)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}

func buildFrozenKubeConfig() (*frozenKubeConfig, error) {
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{},
	)
	raw, err := loader.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("load selected kubeconfig: %w", err)
	}
	contextName := raw.CurrentContext
	if strings.TrimSpace(contextName) == "" {
		return nil, errors.New("selected kubeconfig has no current context")
	}
	if err := clientcmdapi.MinifyConfig(&raw); err != nil {
		return nil, fmt.Errorf("minify selected kubeconfig: %w", err)
	}
	if err := clientcmdapi.FlattenConfig(&raw); err != nil {
		return nil, fmt.Errorf("flatten selected kubeconfig: %w", err)
	}
	data, err := clientcmd.Write(raw)
	if err != nil {
		return nil, fmt.Errorf("serialize selected kubeconfig: %w", err)
	}
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(data)
	if err != nil {
		return nil, fmt.Errorf("build client from frozen kubeconfig: %w", err)
	}
	server, err := normalizedAPIServer(restConfig.Host)
	if err != nil {
		return nil, err
	}
	if restConfig.Insecure || len(restConfig.CAData) == 0 {
		return nil, errors.New("authority-bound kubeconfig requires verified, embedded certificate authority data")
	}
	caSHA := fmt.Sprintf("%x", sha256.Sum256(restConfig.CAData))
	publicIdentity, err := json.Marshal(struct {
		Server     string `json:"server"`
		ServerName string `json:"server_name"`
		CASHA256   string `json:"ca_sha256"`
		Context    string `json:"context"`
	}{server, restConfig.ServerName, caSHA, contextName})
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "mills-s1c-kubeconfig-")
	if err != nil {
		return nil, fmt.Errorf("create private kubeconfig directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("protect private kubeconfig directory: %w", err)
	}
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("write frozen kubeconfig: %w", err)
	}
	return &frozenKubeConfig{
		path: path, dir: dir, rest: rest.CopyConfig(restConfig),
		authority: KubernetesClusterAuthority{
			Contract: AuthorityPlaneContract, ContractVersion: AuthorityPlaneContractVersion,
			PublicAuthoritySHA256: sha256String(string(publicIdentity)),
			APIServerSHA256:       sha256String(server), CertificateAuthoritySHA256: caSHA,
			ContextName: contextName,
		},
	}, nil
}

func (h *Harness) ensureFrozenKubeConfig() (*frozenKubeConfig, error) {
	h.kubeConfigOnce.Do(func() {
		h.frozenKube, h.kubeConfigErr = buildFrozenKubeConfig()
	})
	if h.kubeConfigErr != nil {
		return nil, h.kubeConfigErr
	}
	if h.frozenKube == nil {
		return nil, errors.New("frozen kubeconfig is unavailable")
	}
	return h.frozenKube, nil
}

func (h *Harness) closeFrozenKubeConfig() error {
	if h.frozenKube == nil || h.frozenKube.dir == "" {
		return nil
	}
	return os.RemoveAll(h.frozenKube.dir)
}

func parseOperatorResponseAuthority(header http.Header) (OperatorResponseAuthority, error) {
	version, err := strconv.Atoi(strings.TrimSpace(header.Get(operatorAuthorityVersionHeader)))
	if err != nil {
		return OperatorResponseAuthority{}, errors.New("operator response has no valid authority contract version")
	}
	authority := OperatorResponseAuthority{
		Contract: strings.TrimSpace(header.Get(operatorAuthorityContractHeader)), ContractVersion: version,
		PodName:        strings.TrimSpace(header.Get(operatorAuthorityPodNameHeader)),
		PodNamespace:   strings.TrimSpace(header.Get(operatorAuthorityPodNamespaceHeader)),
		PodUID:         strings.TrimSpace(header.Get(operatorAuthorityPodUIDHeader)),
		DeploymentName: strings.TrimSpace(header.Get(operatorAuthorityDeploymentNameHeader)),
		BootID:         strings.TrimSpace(header.Get(operatorAuthorityBootIDHeader)),
	}
	if authority.Contract != OperatorAuthorityContract || authority.ContractVersion != OperatorAuthorityContractVersion ||
		authority.PodName == "" || authority.PodNamespace == "" || authority.PodUID == "" ||
		authority.DeploymentName == "" || !isNormalizedSHA256(authority.BootID) {
		return authority, fmt.Errorf("operator response authority is incomplete or unsupported: %+v", authority)
	}
	return authority, nil
}

func validateOperatorAuthorityForWorkload(
	authority OperatorResponseAuthority,
	pod PodIdentity,
	deployment DeploymentIdentity,
) error {
	if authority.Contract != OperatorAuthorityContract || authority.ContractVersion != OperatorAuthorityContractVersion {
		return fmt.Errorf("unsupported operator authority %q-v%d", authority.Contract, authority.ContractVersion)
	}
	if !isNormalizedSHA256(authority.BootID) {
		return fmt.Errorf("operator authority has invalid process boot ID %q", authority.BootID)
	}
	if authority.PodName != pod.Name || authority.PodNamespace != pod.Namespace || authority.PodUID != pod.UID {
		return fmt.Errorf("REST operator Pod authority differs from Kubernetes-selected Pod: authority=%+v pod=%s/%s uid=%s",
			authority, pod.Namespace, pod.Name, pod.UID)
	}
	if authority.DeploymentName != deployment.Name || pod.DeploymentName != deployment.Name ||
		pod.DeploymentUID != deployment.UID {
		return fmt.Errorf("REST operator authority is not bound to exact Deployment: authority=%+v pod_deployment=%s/%s deployment=%s/%s",
			authority, pod.DeploymentName, pod.DeploymentUID, deployment.Name, deployment.UID)
	}
	return nil
}

func (h *Harness) observeOperatorResponse(response *http.Response) (OperatorResponseAuthority, error) {
	if !h.cfg.RequireAuthorityBinding {
		return OperatorResponseAuthority{}, nil
	}
	authority, err := parseOperatorResponseAuthority(response.Header)
	if err != nil {
		return authority, err
	}
	h.authorityMu.Lock()
	defer h.authorityMu.Unlock()
	if h.operatorResponseAuthority != (OperatorResponseAuthority{}) && h.operatorResponseAuthority != authority {
		return authority, fmt.Errorf("operator REST backend identity changed: %+v -> %+v",
			h.operatorResponseAuthority, authority)
	}
	if h.expectedOperatorPod.Name != "" {
		if err := validateOperatorAuthorityForWorkload(
			authority, h.expectedOperatorPod, h.expectedOperatorDeployment,
		); err != nil {
			return authority, err
		}
	}
	h.operatorResponseAuthority = authority
	return authority, nil
}

func (h *Harness) doOperatorRequest(
	request *http.Request,
) (*http.Response, OperatorResponseAuthority, error) {
	response, err := h.http.Do(request)
	if err != nil {
		return nil, OperatorResponseAuthority{}, err
	}
	authority, err := h.observeOperatorResponse(response)
	if err != nil {
		_ = response.Body.Close()
		return nil, authority, fmt.Errorf("%w: %v", ErrOperatorAuthority, err)
	}
	return response, authority, nil
}

func (h *Harness) bindClusterAuthority(namespace KubernetesObjectIdentity) error {
	if !h.cfg.RequireAuthorityBinding {
		return nil
	}
	frozen, err := h.ensureFrozenKubeConfig()
	if err != nil {
		return err
	}
	if err := ValidateKubernetesObjectIdentity(namespace, "", h.cfg.OperatorNS); err != nil {
		return fmt.Errorf("operator Namespace authority: %w", err)
	}
	authority := frozen.authority
	authority.OperatorNamespaceIdentity = namespace
	h.authorityMu.Lock()
	defer h.authorityMu.Unlock()
	if h.clusterAuthority != (KubernetesClusterAuthority{}) {
		if h.clusterAuthority.PublicAuthoritySHA256 != authority.PublicAuthoritySHA256 ||
			h.clusterAuthority.APIServerSHA256 != authority.APIServerSHA256 ||
			h.clusterAuthority.CertificateAuthoritySHA256 != authority.CertificateAuthoritySHA256 ||
			h.clusterAuthority.ContextName != authority.ContextName ||
			h.clusterAuthority.OperatorNamespaceIdentity.UID != namespace.UID {
			return fmt.Errorf("kubernetes cluster authority changed: %+v -> %+v", h.clusterAuthority, authority)
		}
	}
	h.clusterAuthority = authority
	return nil
}

func (h *Harness) bindOperatorAuthority(
	pod PodIdentity,
	deployment DeploymentIdentity,
) (AuthorityPlaneEvidence, error) {
	if !h.cfg.RequireAuthorityBinding {
		return AuthorityPlaneEvidence{}, nil
	}
	h.authorityMu.Lock()
	defer h.authorityMu.Unlock()
	if h.clusterAuthority == (KubernetesClusterAuthority{}) {
		return AuthorityPlaneEvidence{}, errors.New("kubernetes cluster authority was not established")
	}
	if h.operatorResponseAuthority == (OperatorResponseAuthority{}) {
		return AuthorityPlaneEvidence{}, errors.New("operator REST authority was not observed")
	}
	if err := validateOperatorAuthorityForWorkload(h.operatorResponseAuthority, pod, deployment); err != nil {
		return AuthorityPlaneEvidence{}, err
	}
	if h.reviewedOperatorDeployment == (DeploymentIdentity{}) {
		h.reviewedOperatorDeployment = deployment
	} else if err := sameDeploymentGateIdentity(h.reviewedOperatorDeployment, deployment); err != nil {
		return AuthorityPlaneEvidence{}, fmt.Errorf("operator reviewed Deployment root changed: %w", err)
	}
	h.expectedOperatorPod = pod
	h.expectedOperatorDeployment = deployment
	evidence := AuthorityPlaneEvidence{
		Contract: AuthorityPlaneContract, ContractVersion: AuthorityPlaneContractVersion,
		Kubernetes: h.clusterAuthority, Operator: h.operatorResponseAuthority,
		OperatorDeploymentUID: deployment.UID,
	}
	if err := ValidateAuthorityPlaneEvidence(evidence, pod, deployment); err != nil {
		return AuthorityPlaneEvidence{}, err
	}
	return evidence, nil
}

// advanceOperatorAuthority installs the exact replacement owner chain after
// the planned CRASH A. The next REST response must attest this new Pod UID;
// until then no response authority is considered established.
func (h *Harness) advanceOperatorAuthority(pod PodIdentity, deployment DeploymentIdentity) {
	if !h.cfg.RequireAuthorityBinding {
		return
	}
	h.authorityMu.Lock()
	h.expectedOperatorPod = pod
	h.expectedOperatorDeployment = deployment
	h.operatorResponseAuthority = OperatorResponseAuthority{}
	h.authorityMu.Unlock()
}

func (h *Harness) currentAuthorityPlane() (AuthorityPlaneEvidence, error) {
	if !h.cfg.RequireAuthorityBinding {
		return AuthorityPlaneEvidence{}, nil
	}
	h.authorityMu.Lock()
	defer h.authorityMu.Unlock()
	evidence := AuthorityPlaneEvidence{
		Contract: AuthorityPlaneContract, ContractVersion: AuthorityPlaneContractVersion,
		Kubernetes: h.clusterAuthority, Operator: h.operatorResponseAuthority,
		OperatorDeploymentUID: h.expectedOperatorDeployment.UID,
	}
	if err := ValidateAuthorityPlaneEvidence(
		evidence, h.expectedOperatorPod, h.expectedOperatorDeployment,
	); err != nil {
		return AuthorityPlaneEvidence{}, err
	}
	return evidence, nil
}

func ValidateAuthorityPlaneEvidence(
	evidence AuthorityPlaneEvidence,
	pod PodIdentity,
	deployment DeploymentIdentity,
) error {
	if evidence.Contract != AuthorityPlaneContract || evidence.ContractVersion != AuthorityPlaneContractVersion {
		return fmt.Errorf("unsupported authority-plane contract %q-v%d", evidence.Contract, evidence.ContractVersion)
	}
	kubernetes := evidence.Kubernetes
	if kubernetes.Contract != AuthorityPlaneContract || kubernetes.ContractVersion != AuthorityPlaneContractVersion ||
		!isNormalizedSHA256(kubernetes.PublicAuthoritySHA256) ||
		!isNormalizedSHA256(kubernetes.APIServerSHA256) ||
		!isNormalizedSHA256(kubernetes.CertificateAuthoritySHA256) ||
		strings.TrimSpace(kubernetes.ContextName) == "" {
		return fmt.Errorf("kubernetes authority is incomplete: %+v", kubernetes)
	}
	if err := ValidateKubernetesObjectIdentity(
		kubernetes.OperatorNamespaceIdentity, "", s1cOperatorNamespace,
	); err != nil {
		return fmt.Errorf("operator Namespace authority: %w", err)
	}
	if kubernetes.OperatorNamespaceIdentity.Name != pod.Namespace {
		return fmt.Errorf("operator Namespace anchor %q differs from selected Pod namespace %q",
			kubernetes.OperatorNamespaceIdentity.Name, pod.Namespace)
	}
	if evidence.OperatorDeploymentUID != deployment.UID || strings.TrimSpace(deployment.UID) == "" {
		return fmt.Errorf("authority Deployment UID %q differs from selected Deployment UID %q",
			evidence.OperatorDeploymentUID, deployment.UID)
	}
	return validateOperatorAuthorityForWorkload(evidence.Operator, pod, deployment)
}

func sameAuthorityCluster(left, right AuthorityPlaneEvidence) bool {
	return left.Kubernetes.PublicAuthoritySHA256 == right.Kubernetes.PublicAuthoritySHA256 &&
		left.Kubernetes.APIServerSHA256 == right.Kubernetes.APIServerSHA256 &&
		left.Kubernetes.CertificateAuthoritySHA256 == right.Kubernetes.CertificateAuthoritySHA256 &&
		left.Kubernetes.ContextName == right.Kubernetes.ContextName &&
		left.Kubernetes.OperatorNamespaceIdentity.Name == right.Kubernetes.OperatorNamespaceIdentity.Name &&
		left.Kubernetes.OperatorNamespaceIdentity.UID == right.Kubernetes.OperatorNamespaceIdentity.UID
}

// validateDeleteClusterAuthority proves the cached client-go client targets
// the same live Namespace UID observed through the frozen kubectl source.
func (h *Harness) validateDeleteClusterAuthority(ctx context.Context) error {
	if !h.cfg.RequireAuthorityBinding {
		return nil
	}
	h.authorityMu.Lock()
	want := h.clusterAuthority.OperatorNamespaceIdentity
	h.authorityMu.Unlock()
	if err := ValidateKubernetesObjectIdentity(want, "", h.cfg.OperatorNS); err != nil {
		return err
	}
	client, err := h.kubernetesClient()
	if err != nil {
		return err
	}
	namespace, err := client.CoreV1().Namespaces().Get(ctx, h.cfg.OperatorNS, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read operator Namespace through delete client: %w", err)
	}
	if namespace.DeletionTimestamp != nil || namespace.Status.Phase != corev1.NamespaceActive ||
		string(namespace.UID) != want.UID {
		return fmt.Errorf("delete client cluster anchor differs: namespace=%s uid=%s phase=%s deleting=%t want_uid=%s",
			namespace.Name, namespace.UID, namespace.Status.Phase, namespace.DeletionTimestamp != nil, want.UID)
	}
	return nil
}

func namespaceIdentityFromRaw(raw, name string) (KubernetesObjectIdentity, error) {
	var list namespaceListWire
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return KubernetesObjectIdentity{}, err
	}
	for _, namespace := range list.Items {
		if namespace.Metadata.Name != name {
			continue
		}
		identity := KubernetesObjectIdentity{
			Name: name, UID: namespace.Metadata.UID,
			ResourceVersion: namespace.Metadata.ResourceVersion,
			Terminating:     namespace.Metadata.DeletionTimestamp != nil,
		}
		if namespace.Metadata.DeletionTimestamp != nil {
			identity.DeletionTimestamp = *namespace.Metadata.DeletionTimestamp
		}
		return identity, nil
	}
	return KubernetesObjectIdentity{}, fmt.Errorf("namespace %q not found", name)
}
