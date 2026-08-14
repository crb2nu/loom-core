package killtest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kubevalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

const (
	PodExecutionProvenanceContract        = "mills-s1c-pod-execution-provenance"
	PodExecutionProvenanceContractVersion = 1
	PodExecutionRenderer                  = "kubernetes-core-v1-pod-dry-run-create"
	PodExecutionRendererVersion           = "1"

	serviceAccountVolumePrefix   = "kube-api-access-"
	normalizedServiceAccountName = "kube-api-access-normalized"
)

func replicaSetPodCreateRequest(replicaSet *appsv1.ReplicaSet) (*corev1.Pod, error) {
	if replicaSet == nil || replicaSet.Namespace == "" || replicaSet.Name == "" || replicaSet.UID == "" {
		return nil, errors.New("cannot build dry-run CREATE Pod request from incomplete ReplicaSet")
	}
	template := replicaSet.Spec.Template.DeepCopy()
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: replicaSet.Namespace, GenerateName: replicaSetPodGenerateName(replicaSet.Name),
			Labels:          template.Labels,
			Annotations:     template.Annotations,
			Finalizers:      template.Finalizers,
			OwnerReferences: []metav1.OwnerReference{exactReplicaSetOwnerReference(replicaSet)},
		},
		Spec: *template.Spec.DeepCopy(),
	}, nil
}

func replicaSetPodGenerateName(replicaSetName string) string {
	prefix := replicaSetName + "-"
	if len(kubevalidation.NameIsDNSSubdomain(prefix, true)) != 0 {
		return replicaSetName
	}
	return prefix
}

func (h *Harness) dryRunCreatePod(
	ctx context.Context,
	namespace string,
	request *corev1.Pod,
) (*corev1.Pod, error) {
	options := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	if h.dryRunCreatePodFn != nil {
		return h.dryRunCreatePodFn(ctx, namespace, request.DeepCopy(), options)
	}
	client, err := h.kubernetesClient()
	if err != nil {
		return nil, err
	}
	created, err := client.CoreV1().Pods(namespace).Create(ctx, request, options)
	if err != nil {
		return nil, fmt.Errorf("server-side dry-run CREATE Pod (no mutation) in %s: %w", namespace, err)
	}
	return created, nil
}

func normalizeServiceAccountVolumeNames(spec *corev1.PodSpec) error {
	if spec == nil {
		return errors.New("PodSpec is nil")
	}
	accessVolumeIndex := -1
	for i := range spec.Volumes {
		name := spec.Volumes[i].Name
		if name == normalizedServiceAccountName {
			return fmt.Errorf("PodSpec already contains reserved normalized volume name %q", name)
		}
		if strings.HasPrefix(name, serviceAccountVolumePrefix) {
			if accessVolumeIndex >= 0 {
				return errors.New("PodSpec contains multiple kube-api-access projected volumes")
			}
			if spec.Volumes[i].Projected == nil {
				return fmt.Errorf("PodSpec volume %q uses kube-api-access prefix without a projected source", name)
			}
			accessVolumeIndex = i
		}
	}
	if accessVolumeIndex < 0 {
		for _, container := range spec.Containers {
			for _, mount := range container.VolumeMounts {
				if strings.HasPrefix(mount.Name, serviceAccountVolumePrefix) {
					return fmt.Errorf("regular container %q has kube-api-access mount without a projected volume", container.Name)
				}
			}
		}
		for _, container := range spec.InitContainers {
			for _, mount := range container.VolumeMounts {
				if strings.HasPrefix(mount.Name, serviceAccountVolumePrefix) {
					return fmt.Errorf("init container %q has kube-api-access mount without a projected volume", container.Name)
				}
			}
		}
		for _, container := range spec.EphemeralContainers {
			for _, mount := range container.VolumeMounts {
				if strings.HasPrefix(mount.Name, serviceAccountVolumePrefix) {
					return fmt.Errorf("ephemeral container %q has kube-api-access mount without a projected volume", container.Name)
				}
			}
		}
		return nil
	}
	oldName := spec.Volumes[accessVolumeIndex].Name
	spec.Volumes[accessVolumeIndex].Name = normalizedServiceAccountName
	mountCount := 0
	normalizeMounts := func(containerType, containerName string, mounts []corev1.VolumeMount) error {
		for i := range mounts {
			if mounts[i].Name == normalizedServiceAccountName {
				return fmt.Errorf("%s container %q already uses reserved normalized volume name", containerType, containerName)
			}
			if strings.HasPrefix(mounts[i].Name, serviceAccountVolumePrefix) && mounts[i].Name != oldName {
				return fmt.Errorf("%s container %q references unmatched kube-api-access volume %q",
					containerType, containerName, mounts[i].Name)
			}
			if mounts[i].Name == oldName {
				mounts[i].Name = normalizedServiceAccountName
				mountCount++
			}
		}
		return nil
	}
	for i := range spec.Containers {
		if err := normalizeMounts("regular", spec.Containers[i].Name, spec.Containers[i].VolumeMounts); err != nil {
			return err
		}
	}
	for i := range spec.InitContainers {
		if err := normalizeMounts("init", spec.InitContainers[i].Name, spec.InitContainers[i].VolumeMounts); err != nil {
			return err
		}
	}
	for i := range spec.EphemeralContainers {
		if err := normalizeMounts("ephemeral", spec.EphemeralContainers[i].Name,
			spec.EphemeralContainers[i].VolumeMounts); err != nil {
			return err
		}
	}
	if mountCount == 0 {
		return fmt.Errorf("PodSpec kube-api-access volume %q has no container mount", oldName)
	}
	return nil
}

func canonicalAdmittedPodSpecSHA256(spec corev1.PodSpec, clearNodeName bool) (string, error) {
	canonical := spec.DeepCopy()
	if clearNodeName {
		canonical.NodeName = ""
	}
	if err := normalizeServiceAccountVolumeNames(canonical); err != nil {
		return "", err
	}
	blob, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(blob)), nil
}

func livePodFromCensus(raw string, identity PodIdentity) (*corev1.Pod, error) {
	var list corev1.PodList
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, err
	}
	for i := range list.Items {
		pod := &list.Items[i]
		if pod.Namespace == identity.Namespace && pod.Name == identity.Name && string(pod.UID) == identity.UID {
			return pod.DeepCopy(), nil
		}
	}
	return nil, fmt.Errorf("selected live Pod %s/%s uid=%s is absent from census object list",
		identity.Namespace, identity.Name, identity.UID)
}

func (h *Harness) bindLivePodExecutionProvenance(
	ctx context.Context,
	identity PodIdentity,
	live *corev1.Pod,
	replicaSet *appsv1.ReplicaSet,
) (PodIdentity, error) {
	if live == nil || replicaSet == nil {
		return PodIdentity{}, errors.New("live Pod execution proof requires Pod and ReplicaSet objects")
	}
	selector, err := metav1.LabelSelectorAsSelector(replicaSet.Spec.Selector)
	if err != nil {
		return PodIdentity{}, fmt.Errorf("compile full ReplicaSet selector: %w", err)
	}
	if !selector.Matches(labels.Set(live.Labels)) {
		return PodIdentity{}, fmt.Errorf("selected live Pod %s/%s labels do not satisfy full ReplicaSet selector",
			live.Namespace, live.Name)
	}
	request, err := replicaSetPodCreateRequest(replicaSet)
	if err != nil {
		return PodIdentity{}, err
	}
	admitted, err := h.dryRunCreatePod(ctx, identity.Namespace, request)
	if err != nil {
		return PodIdentity{}, fmt.Errorf("prove selected live Pod with dry-run CREATE (no mutation): %w", err)
	}
	if admitted == nil || admitted.Namespace != identity.Namespace {
		return PodIdentity{}, fmt.Errorf("dry-run CREATE Pod (no mutation) returned incomplete namespace identity: %+v", admitted)
	}
	if replicaSet.Spec.Template.Spec.NodeName != "" &&
		admitted.Spec.NodeName != replicaSet.Spec.Template.Spec.NodeName {
		return PodIdentity{}, fmt.Errorf("dry-run CREATE Pod (no mutation) changed explicit ReplicaSet nodeName %q to %q",
			replicaSet.Spec.Template.Spec.NodeName, admitted.Spec.NodeName)
	}
	clearLiveNodeName := admitted.Spec.NodeName == ""
	liveDigest, err := canonicalAdmittedPodSpecSHA256(live.Spec, clearLiveNodeName)
	if err != nil {
		return PodIdentity{}, fmt.Errorf("canonicalize live PodSpec %s/%s: %w", live.Namespace, live.Name, err)
	}
	dryRunDigest, err := canonicalAdmittedPodSpecSHA256(admitted.Spec, false)
	if err != nil {
		return PodIdentity{}, fmt.Errorf("canonicalize dry-run CREATE PodSpec (no mutation): %w", err)
	}
	if liveDigest != dryRunDigest {
		return PodIdentity{}, fmt.Errorf("selected live PodSpec differs from dry-run CREATE admission result (no mutation): %s != %s",
			liveDigest, dryRunDigest)
	}
	identity.PodExecutionContract = PodExecutionProvenanceContract
	identity.PodExecutionContractVersion = PodExecutionProvenanceContractVersion
	identity.PodExecutionRenderer = PodExecutionRenderer
	identity.PodExecutionRendererVersion = PodExecutionRendererVersion
	identity.LivePodSpecSHA256 = liveDigest
	identity.DryRunPodSpecSHA256 = dryRunDigest
	return identity, nil
}

func exactReplicaSetOwnerReference(replicaSet *appsv1.ReplicaSet) metav1.OwnerReference {
	controller, blockOwnerDeletion := true, true
	return metav1.OwnerReference{
		APIVersion: "apps/v1", Kind: "ReplicaSet", Name: replicaSet.Name,
		UID: types.UID(replicaSet.UID), Controller: &controller, BlockOwnerDeletion: &blockOwnerDeletion,
	}
}
