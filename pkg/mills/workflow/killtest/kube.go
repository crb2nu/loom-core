package killtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	maxKubectlStdoutSize = int64(64 << 20)
	maxKubectlStderrSize = int64(64 << 10)
)

// kubectl runs one kubectl invocation and returns trimmed stdout.
func (h *Harness) kubectl(ctx context.Context, args ...string) (string, error) {
	if h.kubectlFn != nil {
		return h.kubectlFn(ctx, args...)
	}
	commandArgs := append([]string(nil), args...)
	if h.cfg.RequireAuthorityBinding {
		for _, arg := range commandArgs {
			if arg == "--kubeconfig" || strings.HasPrefix(arg, "--kubeconfig=") {
				return "", errors.New("internal kubectl call attempted to override frozen kubeconfig")
			}
		}
		frozen, err := h.ensureFrozenKubeConfig()
		if err != nil {
			return "", fmt.Errorf("freeze Kubernetes authority: %w", err)
		}
		commandArgs = append([]string{"--kubeconfig", frozen.path}, commandArgs...)
	}
	cmd := exec.CommandContext(ctx, h.cfg.KubectlBin, commandArgs...)
	label := "kubectl " + strings.Join(commandArgs, " ")
	out, err := runBoundedCommandBytes(cmd, label, maxKubectlStdoutSize, maxKubectlStderrSize)
	if err != nil {
		// Command failures and either output overflow reject every stdout byte.
		// Callers must never parse a partial Kubernetes object as authoritative.
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (h *Harness) deletePodWithIdentity(ctx context.Context, namespace, name, uid, resourceVersion string) error {
	if strings.TrimSpace(uid) == "" || strings.TrimSpace(resourceVersion) == "" {
		return errors.New("delete exact pod requires UID and resourceVersion preconditions")
	}
	if h.deletePodFn != nil {
		return h.deletePodFn(ctx, namespace, name, uid, resourceVersion)
	}
	if err := h.validateDeleteClusterAuthority(ctx); err != nil {
		return fmt.Errorf("validate DELETE cluster authority: %w", err)
	}
	return h.deletePodWithIdentityRequest(ctx, namespace, name, uid, resourceVersion)
}

// deletePodWithIdentityRequest performs only the exact preconditioned DELETE.
// crashPod calls it after completing cluster validation and the final
// time-sensitive policy/process hook, so no discovery I/O can stale that hook.
func (h *Harness) deletePodWithIdentityRequest(ctx context.Context, namespace, name, uid, resourceVersion string) error {
	if strings.TrimSpace(uid) == "" || strings.TrimSpace(resourceVersion) == "" {
		return errors.New("delete exact pod requires UID and resourceVersion preconditions")
	}
	client, err := h.kubernetesClient()
	if err != nil {
		return err
	}
	preconditionUID := types.UID(uid)
	preconditionResourceVersion := resourceVersion
	zero := int64(0)
	if err := client.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{
		GracePeriodSeconds: &zero,
		Preconditions: &metav1.Preconditions{
			UID: &preconditionUID, ResourceVersion: &preconditionResourceVersion,
		},
	}); err != nil {
		return fmt.Errorf("delete exact pod %s/%s uid=%s resourceVersion=%s: %w",
			namespace, name, uid, resourceVersion, err)
	}
	return nil
}

func (h *Harness) kubernetesClient() (kubernetes.Interface, error) {
	h.kubeOnce.Do(func() {
		var cfg *rest.Config
		var err error
		if h.cfg.RequireAuthorityBinding {
			var frozen *frozenKubeConfig
			frozen, err = h.ensureFrozenKubeConfig()
			if err == nil {
				cfg = rest.CopyConfig(frozen.rest)
			}
		} else {
			cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
				clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{},
			).ClientConfig()
		}
		if err != nil {
			h.kubeErr = err
			return
		}
		h.kube, h.kubeErr = kubernetes.NewForConfig(cfg)
	})
	if h.kubeErr != nil {
		return nil, fmt.Errorf("build Kubernetes client: %w", h.kubeErr)
	}
	return h.kube, nil
}

type podListWire struct {
	Metadata struct {
		ResourceVersion string `json:"resourceVersion"`
		Continue        string `json:"continue"`
	} `json:"metadata"`
	Items []struct {
		Metadata struct {
			Name              string                  `json:"name"`
			Namespace         string                  `json:"namespace"`
			UID               string                  `json:"uid"`
			ResourceVersion   string                  `json:"resourceVersion"`
			Labels            map[string]string       `json:"labels"`
			OwnerReferences   []metav1.OwnerReference `json:"ownerReferences"`
			DeletionTimestamp *string                 `json:"deletionTimestamp"`
		} `json:"metadata"`
		Spec struct {
			NodeName   string `json:"nodeName"`
			Containers []struct {
				Name  string `json:"name"`
				Image string `json:"image"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			Phase      string    `json:"phase"`
			StartTime  time.Time `json:"startTime"`
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
			ContainerStatuses []struct {
				Name         string `json:"name"`
				Ready        bool   `json:"ready"`
				ImageID      string `json:"imageID"`
				ContainerID  string `json:"containerID"`
				RestartCount *int32 `json:"restartCount"`
				State        struct {
					Running *struct {
						StartedAt time.Time `json:"startedAt"`
					} `json:"running"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

func exactControllerOwnerReference(
	references []metav1.OwnerReference,
	expectedAPIVersion, expectedKind string,
) (metav1.OwnerReference, error) {
	var controller *metav1.OwnerReference
	for i := range references {
		if references[i].Controller == nil || !*references[i].Controller {
			continue
		}
		if controller != nil {
			return metav1.OwnerReference{}, errors.New("object has multiple controller ownerReferences")
		}
		controller = &references[i]
	}
	if controller == nil {
		return metav1.OwnerReference{}, errors.New("object has no controller ownerReference")
	}
	if controller.APIVersion != expectedAPIVersion || controller.Kind != expectedKind ||
		strings.TrimSpace(controller.Name) == "" || strings.TrimSpace(string(controller.UID)) == "" {
		return metav1.OwnerReference{}, fmt.Errorf(
			"controller ownerReference is %s %s/%s uid=%q, want %s %s with complete identity",
			controller.APIVersion, controller.Kind, controller.Name, controller.UID,
			expectedAPIVersion, expectedKind,
		)
	}
	return *controller, nil
}

func podPhaseIsActive(phase string, deleting bool) bool {
	return deleting || (phase != "Succeeded" && phase != "Failed")
}

const maxPodCensusItems = 5000

func controllerPodCensusPath(namespace string) string {
	return fmt.Sprintf("/api/v1/namespaces/%s/pods?limit=%d",
		url.PathEscape(namespace), maxPodCensusItems)
}

func podSpecContainsContainerName(containers []struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}, expected string) bool {
	for _, container := range containers {
		if container.Name == expected {
			return true
		}
	}
	return false
}

func parseReadyPod(raw, expectedNamespace, selector, expectedContainerName string) (PodIdentity, error) {
	var pods podListWire
	if err := json.Unmarshal([]byte(raw), &pods); err != nil {
		return PodIdentity{}, err
	}
	parsedSelector, err := klabels.Parse(selector)
	if err != nil {
		return PodIdentity{}, fmt.Errorf("parse configured pod selector %q: %w", selector, err)
	}
	if strings.TrimSpace(pods.Metadata.ResourceVersion) == "" || strings.TrimSpace(pods.Metadata.Continue) != "" ||
		len(pods.Items) > maxPodCensusItems || strings.TrimSpace(expectedContainerName) == "" {
		return PodIdentity{}, errors.New("namespace-wide pod census has incomplete list or Deployment container identity")
	}
	selectedIndexes := make([]int, 0, 1)
	censusIndexes := make([]int, 0, 1)
	for i := range pods.Items {
		candidate := pods.Items[i]
		active := podPhaseIsActive(candidate.Status.Phase, candidate.Metadata.DeletionTimestamp != nil)
		if !active {
			continue
		}
		if parsedSelector.Matches(klabels.Set(candidate.Metadata.Labels)) {
			selectedIndexes = append(selectedIndexes, i)
		}
		if candidate.Metadata.Namespace == expectedNamespace &&
			podSpecContainsContainerName(candidate.Spec.Containers, expectedContainerName) {
			censusIndexes = append(censusIndexes, i)
		}
	}
	if len(selectedIndexes) != 1 {
		return PodIdentity{}, fmt.Errorf("want exactly one active pod matching selector %q, got %d",
			selector, len(selectedIndexes))
	}
	if len(censusIndexes) != 1 || censusIndexes[0] != selectedIndexes[0] {
		return PodIdentity{}, fmt.Errorf(
			"namespace-wide immutable container-name census for %q found %d active pod(s), selected index=%d census=%v",
			expectedContainerName, len(censusIndexes), selectedIndexes[0], censusIndexes,
		)
	}
	pod := pods.Items[selectedIndexes[0]]
	statuses := pod.Status.ContainerStatuses
	podReadyConditions := 0
	podReady := false
	for _, condition := range pod.Status.Conditions {
		if condition.Type != string(corev1.PodReady) {
			continue
		}
		podReadyConditions++
		podReady = condition.Status == string(corev1.ConditionTrue)
	}
	owner, ownerErr := exactControllerOwnerReference(pod.Metadata.OwnerReferences, "apps/v1", "ReplicaSet")
	if ownerErr != nil {
		return PodIdentity{}, fmt.Errorf("selected pod %q owner: %w", pod.Metadata.Name, ownerErr)
	}
	if len(statuses) == 1 && statuses[0].RestartCount == nil {
		return PodIdentity{}, fmt.Errorf("selected pod %q controller container restartCount is missing", pod.Metadata.Name)
	}
	if len(statuses) == 1 && *statuses[0].RestartCount != 0 {
		return PodIdentity{}, fmt.Errorf("selected pod %q controller container restartCount=%d, want 0",
			pod.Metadata.Name, *statuses[0].RestartCount)
	}
	if pod.Metadata.DeletionTimestamp != nil || pod.Status.Phase != "Running" ||
		podReadyConditions != 1 || !podReady ||
		pod.Metadata.Namespace != expectedNamespace || pod.Metadata.ResourceVersion == "" ||
		strings.TrimSpace(pod.Spec.NodeName) == "" ||
		len(pod.Spec.Containers) != 1 || len(statuses) != 1 ||
		pod.Spec.Containers[0].Name != expectedContainerName || pod.Spec.Containers[0].Image == "" ||
		statuses[0].Name != pod.Spec.Containers[0].Name ||
		!statuses[0].Ready || statuses[0].ImageID == "" || statuses[0].ContainerID == "" ||
		statuses[0].State.Running == nil ||
		statuses[0].State.Running.StartedAt.IsZero() ||
		pod.Metadata.Name == "" || pod.Metadata.UID == "" || pod.Status.StartTime.IsZero() {
		return PodIdentity{}, fmt.Errorf("selected pod %q is not a singleton non-terminating Ready pod", pod.Metadata.Name)
	}
	if statuses[0].State.Running.StartedAt.Before(pod.Status.StartTime) {
		return PodIdentity{}, fmt.Errorf("selected pod %q container started before the pod", pod.Metadata.Name)
	}
	return PodIdentity{
		Name: pod.Metadata.Name, Namespace: pod.Metadata.Namespace, UID: pod.Metadata.UID,
		ResourceVersion:              pod.Metadata.ResourceVersion,
		PodCensusListResourceVersion: pods.Metadata.ResourceVersion, PodCensusCount: len(censusIndexes),
		Node:  pod.Spec.NodeName,
		Image: pod.Spec.Containers[0].Image, ImageID: statuses[0].ImageID, StartedAt: pod.Status.StartTime,
		ContainerName: pod.Spec.Containers[0].Name, ContainerID: statuses[0].ContainerID,
		ContainerRestartCount: *statuses[0].RestartCount,
		ContainerStartedAt:    statuses[0].State.Running.StartedAt,
		ReplicaSetName:        owner.Name, ReplicaSetUID: string(owner.UID),
	}, nil
}

func bindReplicaSetOwner(
	raw, expectedNamespace string,
	pod PodIdentity,
	deployment DeploymentIdentity,
) (PodIdentity, error) {
	if deployment.Namespace != expectedNamespace || strings.TrimSpace(deployment.Name) == "" ||
		strings.TrimSpace(deployment.UID) == "" ||
		!isNormalizedSHA256(deployment.PodTemplateSHA256) ||
		!isNormalizedSHA256(deployment.SelectorSHA256) {
		return PodIdentity{}, fmt.Errorf("expected Deployment identity is incomplete: %+v", deployment)
	}
	var replicaSet appsv1.ReplicaSet
	if err := json.Unmarshal([]byte(raw), &replicaSet); err != nil {
		return PodIdentity{}, err
	}
	if replicaSet.DeletionTimestamp != nil ||
		replicaSet.Namespace != expectedNamespace ||
		replicaSet.Name != pod.ReplicaSetName ||
		string(replicaSet.UID) != pod.ReplicaSetUID ||
		strings.TrimSpace(replicaSet.ResourceVersion) == "" {
		return PodIdentity{}, fmt.Errorf(
			"ReplicaSet owner identity differs from pod: pod=%s/%s uid=%s replicaSet=%s/%s uid=%s rv=%s",
			pod.Namespace, pod.ReplicaSetName, pod.ReplicaSetUID,
			replicaSet.Namespace, replicaSet.Name, replicaSet.UID,
			replicaSet.ResourceVersion,
		)
	}
	owner, err := exactControllerOwnerReference(replicaSet.OwnerReferences, "apps/v1", "Deployment")
	if err != nil {
		return PodIdentity{}, fmt.Errorf("ReplicaSet %s/%s owner: %w", expectedNamespace, replicaSet.Name, err)
	}
	if owner.Name != deployment.Name || string(owner.UID) != deployment.UID {
		return PodIdentity{}, fmt.Errorf(
			"ReplicaSet %s/%s is owned by Deployment %s uid=%s, want %s uid=%s",
			expectedNamespace, replicaSet.Name, owner.Name, owner.UID,
			deployment.Name, deployment.UID,
		)
	}
	if replicaSet.Generation <= 0 || replicaSet.Status.ObservedGeneration != replicaSet.Generation ||
		replicaSet.Spec.Replicas == nil || *replicaSet.Spec.Replicas != 1 ||
		replicaSet.Status.Replicas != 1 || replicaSet.Status.FullyLabeledReplicas != 1 ||
		replicaSet.Status.ReadyReplicas != 1 || replicaSet.Status.AvailableReplicas != 1 ||
		len(replicaSet.Spec.Template.Spec.Containers) != 1 ||
		replicaSet.Spec.Template.Spec.Containers[0].Name != pod.ContainerName ||
		replicaSet.Spec.Template.Spec.Containers[0].Image != pod.Image {
		return PodIdentity{}, fmt.Errorf("ReplicaSet %s/%s is not a singleton template for selected container %s image %s",
			expectedNamespace, replicaSet.Name, pod.ContainerName, pod.Image)
	}
	templateDigest, err := canonicalReplicaSetPodTemplateSHA256(replicaSet.Spec.Template)
	if err != nil {
		return PodIdentity{}, fmt.Errorf("canonicalize ReplicaSet %s/%s pod template: %w",
			expectedNamespace, replicaSet.Name, err)
	}
	if templateDigest != deployment.PodTemplateSHA256 {
		return PodIdentity{}, fmt.Errorf(
			"ReplicaSet %s/%s pod template differs from live Deployment %s: %s != %s",
			expectedNamespace, replicaSet.Name, deployment.Name, templateDigest, deployment.PodTemplateSHA256,
		)
	}
	selectorDigest, err := canonicalReplicaSetSelectorSHA256(replicaSet.Spec.Selector, replicaSet.Spec.Template)
	if err != nil {
		return PodIdentity{}, fmt.Errorf("canonicalize ReplicaSet %s/%s selector: %w",
			expectedNamespace, replicaSet.Name, err)
	}
	if selectorDigest != deployment.SelectorSHA256 {
		return PodIdentity{}, fmt.Errorf(
			"ReplicaSet %s/%s selector differs from live Deployment %s: %s != %s",
			expectedNamespace, replicaSet.Name, deployment.Name, selectorDigest, deployment.SelectorSHA256,
		)
	}
	pod.ReplicaSetResourceVersion = replicaSet.ResourceVersion
	pod.ReplicaSetPodTemplateSHA256 = templateDigest
	pod.ReplicaSetSelectorSHA256 = selectorDigest
	pod.ReplicaSetGeneration = replicaSet.Generation
	pod.ReplicaSetObservedGeneration = replicaSet.Status.ObservedGeneration
	pod.ReplicaSetDesiredReplicas = *replicaSet.Spec.Replicas
	pod.ReplicaSetReplicas = replicaSet.Status.Replicas
	pod.ReplicaSetFullyLabeledReplicas = replicaSet.Status.FullyLabeledReplicas
	pod.ReplicaSetReadyReplicas = replicaSet.Status.ReadyReplicas
	pod.ReplicaSetAvailableReplicas = replicaSet.Status.AvailableReplicas
	pod.DeploymentName = owner.Name
	pod.DeploymentUID = string(owner.UID)
	return pod, nil
}

type deploymentWire struct {
	Metadata struct {
		Name              string  `json:"name"`
		Namespace         string  `json:"namespace"`
		UID               string  `json:"uid"`
		ResourceVersion   string  `json:"resourceVersion"`
		Generation        int64   `json:"generation"`
		DeletionTimestamp *string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int32 `json:"replicas"`
		Strategy struct {
			Type string `json:"type"`
		} `json:"strategy"`
		Template struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				Containers []struct {
					Name  string `json:"name"`
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64 `json:"observedGeneration"`
		Replicas           int32 `json:"replicas"`
		UpdatedReplicas    int32 `json:"updatedReplicas"`
		ReadyReplicas      int32 `json:"readyReplicas"`
		AvailableReplicas  int32 `json:"availableReplicas"`
	} `json:"status"`
}

func parseStableDeployment(raw string) (DeploymentIdentity, error) {
	id, _, err := parseStableDeploymentObject(raw)
	return id, err
}

func parseStableDeploymentObject(raw string) (DeploymentIdentity, *appsv1.Deployment, error) {
	var dep deploymentWire
	if err := json.Unmarshal([]byte(raw), &dep); err != nil {
		return DeploymentIdentity{}, nil, err
	}
	var typed appsv1.Deployment
	if err := json.Unmarshal([]byte(raw), &typed); err != nil {
		return DeploymentIdentity{}, nil, err
	}
	if dep.Spec.Replicas == nil || len(dep.Spec.Template.Spec.Containers) != 1 {
		return DeploymentIdentity{}, nil, errors.New("deployment must declare one replica and one container")
	}
	specDigest, err := canonicalDeploymentSpecSHA256(typed.Spec)
	if err != nil {
		return DeploymentIdentity{}, nil, fmt.Errorf("canonicalize Deployment spec: %w", err)
	}
	podTemplateDigest, err := canonicalDeploymentPodTemplateSHA256(typed.Spec.Template)
	if err != nil {
		return DeploymentIdentity{}, nil, fmt.Errorf("canonicalize Deployment pod template: %w", err)
	}
	selectorDigest, err := canonicalDeploymentSelectorSHA256(typed.Spec.Selector)
	if err != nil {
		return DeploymentIdentity{}, nil, fmt.Errorf("canonicalize Deployment selector: %w", err)
	}
	id := DeploymentIdentity{
		Name: dep.Metadata.Name, Namespace: dep.Metadata.Namespace,
		UID: dep.Metadata.UID, ResourceVersion: dep.Metadata.ResourceVersion,
		Generation:         dep.Metadata.Generation,
		ObservedGeneration: dep.Status.ObservedGeneration, DesiredReplicas: *dep.Spec.Replicas,
		Replicas: dep.Status.Replicas, UpdatedReplicas: dep.Status.UpdatedReplicas,
		ReadyReplicas: dep.Status.ReadyReplicas, AvailableReplicas: dep.Status.AvailableReplicas,
		Image:         dep.Spec.Template.Spec.Containers[0].Image,
		ContainerName: dep.Spec.Template.Spec.Containers[0].Name, Strategy: dep.Spec.Strategy.Type,
		PolicyChecksum: dep.Spec.Template.Metadata.Annotations["loom.flexinfer.ai/policy-checksum"],
		SpecSHA256:     specDigest, PodTemplateSHA256: podTemplateDigest, SelectorSHA256: selectorDigest,
	}
	if id.Name == "" || id.Namespace == "" || id.UID == "" || id.ResourceVersion == "" ||
		dep.Metadata.DeletionTimestamp != nil ||
		id.Image == "" || id.ContainerName == "" || id.Strategy != "Recreate" ||
		id.Generation <= 0 || id.ObservedGeneration != id.Generation ||
		id.DesiredReplicas != 1 || id.Replicas != 1 || id.UpdatedReplicas != 1 ||
		id.ReadyReplicas != 1 || id.AvailableReplicas != 1 {
		return DeploymentIdentity{}, nil, fmt.Errorf("deployment %q is not a fully observed stable singleton: %+v", id.Name, id)
	}
	return id, &typed, nil
}

func (h *Harness) stableDeployment(ctx context.Context, namespace, name string) (DeploymentIdentity, error) {
	id, _, err := h.stableDeploymentObject(ctx, namespace, name)
	return id, err
}

func (h *Harness) stableDeploymentObject(
	ctx context.Context,
	namespace, name string,
) (DeploymentIdentity, *appsv1.Deployment, error) {
	raw, err := h.kubectl(ctx, "-n", namespace, "get", "deploy", name, "-o", "json")
	if err != nil {
		return DeploymentIdentity{}, nil, err
	}
	id, object, err := parseStableDeploymentObject(raw)
	if err != nil {
		return DeploymentIdentity{}, nil, err
	}
	if id.Namespace != namespace || id.Name != name {
		return DeploymentIdentity{}, nil, fmt.Errorf("deployment lookup %s/%s returned %s/%s",
			namespace, name, id.Namespace, id.Name)
	}
	return id, object, nil
}

func (h *Harness) readyPod(
	ctx context.Context,
	namespace, selector string,
	deployment DeploymentIdentity,
) (PodIdentity, error) {
	// Use the raw endpoint so kubectl cannot transparently follow continuation
	// tokens and merge multiple pages. parseReadyPod rejects a continuation or
	// an over-cap response, making this one bounded API snapshot.
	raw, err := h.kubectl(ctx, "get", "--raw", controllerPodCensusPath(namespace))
	if err != nil {
		return PodIdentity{}, err
	}
	pod, err := parseReadyPod(raw, namespace, selector, deployment.ContainerName)
	if err != nil {
		return PodIdentity{}, err
	}
	replicaSetRaw, err := h.kubectl(ctx, "-n", namespace, "get", "replicaset", pod.ReplicaSetName, "-o", "json")
	if err != nil {
		return PodIdentity{}, fmt.Errorf("read selected pod ReplicaSet owner: %w", err)
	}
	bound, err := bindReplicaSetOwner(replicaSetRaw, namespace, pod, deployment)
	if err != nil {
		return PodIdentity{}, err
	}
	var replicaSet appsv1.ReplicaSet
	if err := json.Unmarshal([]byte(replicaSetRaw), &replicaSet); err != nil {
		return PodIdentity{}, fmt.Errorf("decode selected pod ReplicaSet for execution proof: %w", err)
	}
	livePod, err := livePodFromCensus(raw, bound)
	if err != nil {
		return PodIdentity{}, err
	}
	return h.bindLivePodExecutionProvenance(ctx, bound, livePod, &replicaSet)
}

// CrashPod revalidates the stable singleton immediately before mutation,
// deletes that exact API object with UID and resourceVersion preconditions, and
// waits for a same-build replacement. Used for both CRASH A and CRASH B.
func (h *Harness) CrashPod(ctx context.Context, ns, selector, deploy string, before PodIdentity, beforeDeployment DeploymentIdentity) (PodIdentity, error) {
	return h.crashPod(ctx, ns, selector, deploy, before, beforeDeployment, nil, nil, nil)
}

// CrashPodWithLease renews the token before the final safety proof and bounded
// UID+resourceVersion-preconditioned delete.
func (h *Harness) CrashPodWithLease(ctx context.Context, ns, selector, deploy string, before PodIdentity, beforeDeployment DeploymentIdentity, lease CrashLease) (PodIdentity, error) {
	return h.CrashPodWithLeaseAndCheck(ctx, ns, selector, deploy, before, beforeDeployment, lease, nil)
}

const finalPreDeleteCheckTimeout = 10 * time.Second

// CrashPodWithLeaseAndCheck renews the crash lease, executes one final
// fail-closed safety proof, then performs a bounded Deployment+Pod+ReplicaSet
// identity reread. The callback is the place for time-sensitive target-workload
// checks such as the S1c foreground hold. The reread closes same-UID container
// restart races, and the DELETE resourceVersion precondition closes the final
// reread-to-mutation race.
func (h *Harness) CrashPodWithLeaseAndCheck(
	ctx context.Context,
	ns, selector, deploy string,
	before PodIdentity,
	beforeDeployment DeploymentIdentity,
	lease CrashLease,
	finalCheck func(context.Context) error,
) (PodIdentity, error) {
	return h.CrashPodWithLeaseAndHooks(ctx, ns, selector, deploy, before, beforeDeployment,
		lease, finalCheck, nil)
}

// CrashPodWithLeaseAndHooks adds a delete-start hook that runs after the final
// identity reread and immediately before the UID+resourceVersion-preconditioned
// DELETE request.
// S1c uses it for the final policy refresh and in-memory authorization after a
// process observer has crossed its deliberately paused source fence. The hook
// still forms the DELETE handoff boundary and is covered by the active observer.
func (h *Harness) CrashPodWithLeaseAndHooks(
	ctx context.Context,
	ns, selector, deploy string,
	before PodIdentity,
	beforeDeployment DeploymentIdentity,
	lease CrashLease,
	finalCheck func(context.Context) error,
	atDeleteStart func() error,
) (PodIdentity, error) {
	replacement, _, err := h.CrashPodWithLeaseEvidenceAndHooks(
		ctx, ns, selector, deploy, before, beforeDeployment, lease, finalCheck, atDeleteStart, nil)
	return replacement, err
}

// CrashPodWithLeaseEvidenceAndHooks is the evidence-producing form used by
// S1c. It preserves the renewed, server-authored lease response without
// exposing its bearer token in the serialized evidence contract.
func (h *Harness) CrashPodWithLeaseEvidenceAndHooks(
	ctx context.Context,
	ns, selector, deploy string,
	before PodIdentity,
	beforeDeployment DeploymentIdentity,
	lease CrashLease,
	finalCheck func(context.Context) error,
	atDeleteStart func() error,
	afterDeleteAccepted func(time.Time, CrashLease) error,
) (PodIdentity, CrashLease, error) {
	if err := validateAcquiredCrashLease(lease); err != nil {
		return PodIdentity{}, CrashLease{}, err
	}
	var renewed CrashLease
	deleteStart := func() error {
		if atDeleteStart != nil {
			if err := atDeleteStart(); err != nil {
				return err
			}
		}
		// This is deliberately the final callback operation. A caller hook may
		// block; only the remaining TTL at the actual delete handoff matters.
		return validateRenewedLeaseAtDelete(renewed, time.Now().UTC())
	}
	var acceptedReceipt func(time.Time) error
	if afterDeleteAccepted != nil {
		acceptedReceipt = func(acceptedAt time.Time) error {
			return afterDeleteAccepted(acceptedAt, renewed)
		}
	}
	replacement, err := h.crashPod(ctx, ns, selector, deploy, before, beforeDeployment, func(ctx context.Context) error {
		var err error
		renewed, err = h.RenewCrashLease(ctx, lease.Token)
		if err != nil {
			return fmt.Errorf("renew crash lease: %w", err)
		}
		if err := validateRenewedCrashLeaseIdentity(lease, renewed); err != nil {
			return err
		}
		if finalCheck != nil {
			checkCtx, cancel := context.WithTimeout(ctx, h.finalPreDeleteCheckTimeout)
			err := finalCheck(checkCtx)
			deadlineErr := checkCtx.Err()
			cancel()
			if err != nil {
				return fmt.Errorf("final workload check: %w", err)
			}
			if deadlineErr != nil {
				return fmt.Errorf("final workload check exceeded %s: %w", h.finalPreDeleteCheckTimeout, deadlineErr)
			}
		}
		return nil
	}, deleteStart, acceptedReceipt)
	return replacement, renewed, err
}

func validateAcquiredCrashLease(lease CrashLease) error {
	if strings.TrimSpace(lease.Token) == "" || strings.TrimSpace(lease.RequestID) == "" ||
		strings.TrimSpace(lease.RunID) == "" || strings.TrimSpace(lease.SpawnID) == "" ||
		lease.ObservedAt.IsZero() || lease.ExpiresAt.IsZero() || !lease.ExpiresAt.After(lease.ObservedAt) {
		return errors.New("acquired crash lease evidence is incomplete")
	}
	return nil
}

func validateRenewedCrashLeaseIdentity(acquired, renewed CrashLease) error {
	if renewed.Token != acquired.Token || renewed.RequestID != acquired.RequestID ||
		renewed.RunID != acquired.RunID || renewed.SpawnID != acquired.SpawnID ||
		renewed.Authority != acquired.Authority {
		return fmt.Errorf("renewed crash lease identity changed: acquired=%s/%s/%s renewed=%s/%s/%s",
			acquired.RequestID, acquired.RunID, acquired.SpawnID,
			renewed.RequestID, renewed.RunID, renewed.SpawnID)
	}
	if !renewed.ObservedAt.After(acquired.ObservedAt) || !renewed.ObservedAt.Before(acquired.ExpiresAt) {
		return fmt.Errorf("renewed crash lease observation %s is outside acquired lease lifetime %s -> %s",
			renewed.ObservedAt, acquired.ObservedAt, acquired.ExpiresAt)
	}
	return nil
}

func validateRenewedLeaseAtDelete(lease CrashLease, deleteAt time.Time) error {
	if deleteAt.IsZero() || lease.ObservedAt.IsZero() || lease.ExpiresAt.IsZero() ||
		strings.TrimSpace(lease.RequestID) == "" || strings.TrimSpace(lease.RunID) == "" ||
		strings.TrimSpace(lease.SpawnID) == "" {
		return errors.New("renewed crash lease evidence is incomplete at delete boundary")
	}
	if lease.ObservedAt.After(deleteAt) {
		return fmt.Errorf("renewed crash lease observation %s follows delete boundary %s", lease.ObservedAt, deleteAt)
	}
	minimumExpiry := deleteAt.Add(podDeleteRequestTimeout + crashLeaseDeleteSafetyMargin)
	if lease.ExpiresAt.Before(minimumExpiry) {
		return fmt.Errorf("renewed crash lease expires at %s, need at least %s through %s at delete boundary",
			lease.ExpiresAt, podDeleteRequestTimeout+crashLeaseDeleteSafetyMargin, minimumExpiry)
	}
	return nil
}

const replacementStartMaxClockSkew = 30 * time.Second

func (h *Harness) crashPod(
	ctx context.Context,
	ns, selector, deploy string,
	before PodIdentity,
	beforeDeployment DeploymentIdentity,
	beforeDelete func(context.Context) error,
	atDeleteStart func() error,
	afterDeleteAccepted func(time.Time) error,
) (PodIdentity, error) {
	currentDeployment, err := h.stableDeployment(ctx, ns, deploy)
	if err != nil {
		return PodIdentity{}, fmt.Errorf("pre-delete deployment check: %w", err)
	}
	if err := sameLiveDeploymentIdentity(beforeDeployment, currentDeployment); err != nil {
		return PodIdentity{}, fmt.Errorf("deployment %s/%s drifted before crash: before=%+v current=%+v",
			ns, deploy, beforeDeployment, currentDeployment)
	}
	currentPod, err := h.readyPod(ctx, ns, selector, beforeDeployment)
	if err != nil {
		return PodIdentity{}, fmt.Errorf("pre-delete singleton check: %w", err)
	}
	if !sameControllerPodIncarnation(currentPod, before) {
		return PodIdentity{}, fmt.Errorf("selected pod drifted before crash: before=%+v current=%+v", before, currentPod)
	}
	// Build the REST client before the final callback so the final object reread
	// and DELETE are not separated by lazy client initialization.
	if h.deletePodFn == nil {
		if _, err := h.kubernetesClient(); err != nil {
			return PodIdentity{}, fmt.Errorf("prepare UID+resourceVersion-preconditioned pod delete: %w", err)
		}
	}
	h.cfg.Logf("CRASH: preparing force-delete of exact pod %s/%s uid=%s resourceVersion=%s",
		ns, before.Name, before.UID, before.ResourceVersion)
	if beforeDelete != nil {
		if err := beforeDelete(ctx); err != nil {
			return PodIdentity{}, fmt.Errorf("pre-delete safety check: %w", err)
		}
	}
	identityCtx, cancelIdentity := context.WithTimeout(ctx, h.finalPreDeleteCheckTimeout)
	finalDeployment, identityErr := h.stableDeployment(identityCtx, ns, deploy)
	if identityErr == nil {
		identityErr = sameLiveDeploymentIdentity(beforeDeployment, finalDeployment)
	}
	var finalPod PodIdentity
	if identityErr == nil {
		finalPod, identityErr = h.readyPod(identityCtx, ns, selector, beforeDeployment)
	}
	identityDeadlineErr := identityCtx.Err()
	cancelIdentity()
	if identityErr != nil {
		return PodIdentity{}, fmt.Errorf("final pre-delete controller identity check: %w", identityErr)
	}
	if identityDeadlineErr != nil {
		return PodIdentity{}, fmt.Errorf("final pre-delete controller identity check exceeded %s: %w",
			h.finalPreDeleteCheckTimeout, identityDeadlineErr)
	}
	if finalDeployment != currentDeployment {
		return PodIdentity{}, fmt.Errorf("deployment changed during final pre-delete proof: before=%+v final=%+v",
			currentDeployment, finalDeployment)
	}
	if !sameControllerPodIncarnation(finalPod, currentPod) {
		return PodIdentity{}, fmt.Errorf("selected pod changed during final pre-delete proof: before=%+v final=%+v",
			currentPod, finalPod)
	}
	if h.deletePodFn == nil {
		clusterCtx, cancelCluster := context.WithTimeout(ctx, h.finalPreDeleteCheckTimeout)
		clusterErr := h.validateDeleteClusterAuthority(clusterCtx)
		clusterDeadlineErr := clusterCtx.Err()
		cancelCluster()
		if clusterErr != nil {
			return PodIdentity{}, fmt.Errorf("final pre-delete cluster authority check: %w", clusterErr)
		}
		if clusterDeadlineErr != nil {
			return PodIdentity{}, fmt.Errorf("final pre-delete cluster authority check exceeded %s: %w",
				h.finalPreDeleteCheckTimeout, clusterDeadlineErr)
		}
	}
	if atDeleteStart != nil {
		if err := atDeleteStart(); err != nil {
			return PodIdentity{}, fmt.Errorf("delete-start hook: %w", err)
		}
	}
	// The hard DELETE deadline starts only after the potentially blocking final
	// hook. From here to the API mutation there is no authority/discovery read.
	deleteCtx, cancelDelete := context.WithTimeout(ctx, podDeleteRequestTimeout)
	deleteStartedAt := time.Now().UTC()
	if h.deletePodFn != nil {
		err = h.deletePodWithIdentity(deleteCtx, ns, finalPod.Name, finalPod.UID, finalPod.ResourceVersion)
	} else {
		err = h.deletePodWithIdentityRequest(deleteCtx, ns, finalPod.Name, finalPod.UID, finalPod.ResourceVersion)
	}
	cancelDelete()
	if err != nil {
		return PodIdentity{}, err
	}
	deleteAcceptedAt := time.Now().UTC()
	var postDeleteErr error
	if afterDeleteAccepted != nil {
		if err := afterDeleteAccepted(deleteAcceptedAt); err != nil {
			// DELETE has already crossed the mutation boundary. Preserve the
			// receipt error, but still discover the exact replacement through
			// Kubernetes so later REST cleanup cannot remain pinned to the dead
			// operator Pod identity.
			postDeleteErr = errors.Join(postDeleteErr, fmt.Errorf("record accepted pod delete: %w", err))
		}
	}
	replacementTimeout := h.cfg.StepTimeout
	// Once DELETE is accepted, caller cancellation is evidence of failure but
	// must not prevent mitigation. Use a fresh bounded context to discover and
	// bind the exact replacement, then join the original cancellation below.
	recoveryCtx, cancelRecovery := context.WithTimeout(context.WithoutCancel(ctx), replacementTimeout)
	defer cancelRecovery()
	h.cfg.Logf("waiting for %s/%s rollout", ns, deploy)
	if _, err := h.kubectl(recoveryCtx, "-n", ns, "rollout", "status", "deploy/"+deploy, "--timeout=300s"); err != nil {
		// A rollout command failure does not prove that replacement failed.
		// Continue with authoritative Deployment/Pod reads and return the
		// rollout error after rebinding if a replacement converges.
		postDeleteErr = errors.Join(postDeleteErr, fmt.Errorf("wait for replacement rollout: %w", err))
	}
	deadline := time.Now().Add(replacementTimeout)
	var after PodIdentity
	for {
		currentDeployment, err = h.stableDeployment(recoveryCtx, ns, deploy)
		if err == nil {
			err = sameLiveDeploymentIdentity(beforeDeployment, currentDeployment)
		}
		if err == nil {
			after, err = h.readyPod(recoveryCtx, ns, selector, beforeDeployment)
		}
		if err == nil && (after.UID == before.UID || after.ContainerID == before.ContainerID) {
			err = fmt.Errorf("replacement has not advanced pod/container identity: pod_uid=%s container_id=%s",
				after.UID, after.ContainerID)
		}
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return PodIdentity{}, errors.Join(postDeleteErr, ctx.Err(),
				fmt.Errorf("replacement singleton not stable within %s: %w", replacementTimeout, err))
		}
		select {
		case <-recoveryCtx.Done():
			return PodIdentity{}, errors.Join(postDeleteErr, ctx.Err(), recoveryCtx.Err())
		case <-time.After(h.cfg.PollInterval):
		}
	}
	// For CRASH A this exact, stable Pod -> ReplicaSet -> Deployment chain is
	// the only safe REST routing identity after DELETE was accepted. Install it
	// before the remaining evidence checks so any failure below still permits
	// cleanup to authenticate the replacement response instead of the dead Pod.
	if ns == h.cfg.OperatorNS && deploy == "loom-mills-operator" {
		h.advanceOperatorAuthority(after, currentDeployment)
	}
	postDeleteFailure := func(cause error) (PodIdentity, error) {
		return after, errors.Join(postDeleteErr, ctx.Err(), cause)
	}
	if after.UID == before.UID {
		return postDeleteFailure(fmt.Errorf("deployment %s/%s did not replace pod uid %s", ns, deploy, before.UID))
	}
	if after.Image != before.Image || after.ImageID != before.ImageID {
		return postDeleteFailure(fmt.Errorf("deployment %s/%s changed image across crash: %s (%s) -> %s (%s)",
			ns, deploy, before.Image, before.ImageID, after.Image, after.ImageID))
	}
	if after.Namespace != before.Namespace || after.ContainerName != before.ContainerName ||
		after.ContainerID == before.ContainerID ||
		after.ReplicaSetName != before.ReplicaSetName || after.ReplicaSetUID != before.ReplicaSetUID ||
		after.ReplicaSetPodTemplateSHA256 != before.ReplicaSetPodTemplateSHA256 ||
		after.ReplicaSetSelectorSHA256 != before.ReplicaSetSelectorSHA256 ||
		after.ReplicaSetGeneration != before.ReplicaSetGeneration ||
		after.PodExecutionContract != before.PodExecutionContract ||
		after.PodExecutionContractVersion != before.PodExecutionContractVersion ||
		after.PodExecutionRenderer != before.PodExecutionRenderer ||
		after.PodExecutionRendererVersion != before.PodExecutionRendererVersion ||
		after.LivePodSpecSHA256 != before.LivePodSpecSHA256 ||
		after.DryRunPodSpecSHA256 != before.DryRunPodSpecSHA256 ||
		after.DeploymentName != before.DeploymentName || after.DeploymentUID != before.DeploymentUID {
		return postDeleteFailure(fmt.Errorf("deployment %s/%s changed controller lineage or reused a container across crash: before=%+v after=%+v",
			ns, deploy, before, after))
	}
	if !after.StartedAt.After(before.StartedAt) {
		return postDeleteFailure(fmt.Errorf("deployment %s/%s replacement start time %s is not after original %s",
			ns, deploy, after.StartedAt, before.StartedAt))
	}
	if !after.ContainerStartedAt.After(before.ContainerStartedAt) {
		return postDeleteFailure(fmt.Errorf("deployment %s/%s replacement container start time %s is not after original %s",
			ns, deploy, after.ContainerStartedAt, before.ContainerStartedAt))
	}
	if after.ContainerStartedAt.Before(deleteStartedAt.Add(-replacementStartMaxClockSkew)) {
		return postDeleteFailure(fmt.Errorf("deployment %s/%s replacement container start time %s predates delete boundary %s beyond %s clock skew",
			ns, deploy, after.ContainerStartedAt, deleteStartedAt, replacementStartMaxClockSkew))
	}
	if after.StartedAt.Before(deleteStartedAt.Add(-replacementStartMaxClockSkew)) {
		return postDeleteFailure(fmt.Errorf("deployment %s/%s replacement start time %s predates delete boundary %s beyond %s clock skew",
			ns, deploy, after.StartedAt, deleteStartedAt, replacementStartMaxClockSkew))
	}
	if err := sameLiveDeploymentIdentity(beforeDeployment, currentDeployment); err != nil {
		return postDeleteFailure(fmt.Errorf("deployment %s/%s changed stable full-spec identity across crash: %w",
			ns, deploy, err))
	}
	if err := errors.Join(postDeleteErr, ctx.Err()); err != nil {
		return after, err
	}
	return after, nil
}

const podDeleteRequestTimeout = 20 * time.Second
