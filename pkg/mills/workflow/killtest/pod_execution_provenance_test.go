package killtest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func controllerExecutionObjects(t *testing.T) (PodIdentity, *corev1.Pod, *appsv1.ReplicaSet) {
	t.Helper()
	deploymentJSON := controllerDeploymentFixtureJSON(
		"loom-mills-operator", "loom-mills", "deployment-uid", "operator:v1",
	)
	deployment := mustStableDeploymentFixture(deploymentJSON)
	identity := bindControllerPodFixture(controllerPodFixture(
		"operator", deployment.Namespace, "pod-uid", deployment.Image, "operator@sha256:abc",
		deployment.Name, deployment.UID, time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC),
	), deployment)
	var list corev1.PodList
	if err := json.Unmarshal([]byte(controllerPodListFixtureJSON(identity)), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("fixture Pod count = %d, want 1", len(list.Items))
	}
	var replicaSet appsv1.ReplicaSet
	if err := json.Unmarshal([]byte(controllerReplicaSetFixtureJSON(identity, deploymentJSON)), &replicaSet); err != nil {
		t.Fatal(err)
	}
	return identity, list.Items[0].DeepCopy(), replicaSet.DeepCopy()
}

func TestReplicaSetPodCreateRequestMatchesControllerShape(t *testing.T) {
	controller, blockOwnerDeletion := false, false
	replicaSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "operator-rs", Namespace: "loom-mills", UID: types.UID("rs-uid"),
		},
		Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Name: "must-not-propagate", Namespace: "must-not-propagate", UID: types.UID("must-not-propagate"),
				Labels: map[string]string{"app": "operator"}, Annotations: map[string]string{"proof": "reviewed"},
				Finalizers: []string{"example.test/template-finalizer"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "v1", Kind: "ConfigMap", Name: "must-not-propagate", UID: types.UID("other"),
					Controller: &controller, BlockOwnerDeletion: &blockOwnerDeletion,
				}},
			},
			Spec: corev1.PodSpec{
				ServiceAccountName: "operator", Containers: []corev1.Container{{
					Name: "controller", Image: "operator:v1", Command: []string{"/operator"},
				}},
			},
		}},
	}
	wantTemplate := replicaSet.Spec.Template.DeepCopy()
	request, err := replicaSetPodCreateRequest(replicaSet)
	if err != nil {
		t.Fatalf("replicaSetPodCreateRequest() error = %v", err)
	}
	if request.APIVersion != "v1" || request.Kind != "Pod" || request.Namespace != replicaSet.Namespace ||
		request.Name != "" || request.GenerateName != replicaSet.Name+"-" || request.UID != "" ||
		request.ResourceVersion != "" || !request.CreationTimestamp.IsZero() || request.DeletionTimestamp != nil ||
		!reflect.DeepEqual(request.Status, corev1.PodStatus{}) {
		t.Fatalf("request carries fields the ReplicaSet controller would not create: %+v", request)
	}
	if !reflect.DeepEqual(request.Labels, wantTemplate.Labels) ||
		!reflect.DeepEqual(request.Annotations, wantTemplate.Annotations) ||
		!reflect.DeepEqual(request.Finalizers, wantTemplate.Finalizers) ||
		!reflect.DeepEqual(request.Spec, wantTemplate.Spec) {
		t.Fatalf("request does not preserve exact template metadata/spec: %+v", request)
	}
	wantOwner := exactReplicaSetOwnerReference(replicaSet)
	if !reflect.DeepEqual(request.OwnerReferences, []metav1.OwnerReference{wantOwner}) {
		t.Fatalf("request owners = %+v, want exact ReplicaSet controller owner %+v", request.OwnerReferences, wantOwner)
	}
	request.Labels["app"] = "mutated"
	request.Finalizers[0] = "mutated"
	request.Spec.Containers[0].Command[0] = "mutated"
	if !reflect.DeepEqual(replicaSet.Spec.Template, *wantTemplate) {
		t.Fatal("building or mutating the request changed the ReplicaSet template")
	}
}

func TestReplicaSetPodGenerateNameMatchesControllerFallback(t *testing.T) {
	if got := replicaSetPodGenerateName("operator-rs"); got != "operator-rs-" {
		t.Fatalf("short ReplicaSet generateName = %q", got)
	}
	validMaxLengthName := strings.Repeat("a", 253)
	if got := replicaSetPodGenerateName(validMaxLengthName); got != validMaxLengthName+"-" {
		t.Fatalf("valid max-length ReplicaSet generateName = %q", got)
	}
	legacyInvalidName := "legacy_replica_set"
	if got := replicaSetPodGenerateName(legacyInvalidName); got != legacyInvalidName {
		t.Fatalf("invalid ReplicaSet generateName = %q, want controller fallback %q", got, legacyInvalidName)
	}
}

func TestDryRunCreatePodSeamReceivesNonMutatingCreateOptions(t *testing.T) {
	h := New(Config{})
	request := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "loom-mills"}}
	h.dryRunCreatePodFn = func(
		_ context.Context, namespace string, got *corev1.Pod, options metav1.CreateOptions,
	) (*corev1.Pod, error) {
		if namespace != request.Namespace || !reflect.DeepEqual(options.DryRun, []string{metav1.DryRunAll}) ||
			options.FieldManager != "" {
			t.Fatalf("dry-run CREATE call = namespace %q options %+v", namespace, options)
		}
		got.Labels = map[string]string{"hook": "mutated-copy"}
		return got, nil
	}
	if _, err := h.dryRunCreatePod(context.Background(), request.Namespace, request); err != nil {
		t.Fatalf("dryRunCreatePod() error = %v", err)
	}
	if request.Labels != nil {
		t.Fatal("dry-run seam mutated caller's request")
	}
}

func TestBindLivePodExecutionProvenanceAcceptsOnlyRandomizedAccessVolumeAndScheduledNode(t *testing.T) {
	identity, live, replicaSet := controllerExecutionObjects(t)
	liveBefore, replicaSetBefore := live.DeepCopy(), replicaSet.DeepCopy()
	h := New(Config{})
	configureControllerPodDryRunFixture(t, h)
	got, err := h.bindLivePodExecutionProvenance(context.Background(), identity, live, replicaSet)
	if err != nil {
		t.Fatalf("bindLivePodExecutionProvenance() error = %v", err)
	}
	if got.PodExecutionContract != PodExecutionProvenanceContract ||
		got.PodExecutionContractVersion != PodExecutionProvenanceContractVersion ||
		got.PodExecutionRenderer != PodExecutionRenderer ||
		got.PodExecutionRendererVersion != PodExecutionRendererVersion ||
		got.LivePodSpecSHA256 == "" || got.LivePodSpecSHA256 != got.DryRunPodSpecSHA256 {
		t.Fatalf("execution proof is incomplete: %+v", got)
	}
	if !reflect.DeepEqual(live, liveBefore) || !reflect.DeepEqual(replicaSet, replicaSetBefore) {
		t.Fatal("execution proof mutated the live Pod or ReplicaSet")
	}
}

func TestBindLivePodExecutionProvenanceRejectsSameImagePodSpecMutation(t *testing.T) {
	privileged := true
	mode := int32(0444)
	tests := map[string]func(*corev1.Pod){
		"command": func(pod *corev1.Pod) {
			pod.Spec.Containers[0].Command = []string{"/unreviewed"}
		},
		"environment": func(pod *corev1.Pod) {
			pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "UNREVIEWED", Value: "true"}}
		},
		"service account": func(pod *corev1.Pod) {
			pod.Spec.ServiceAccountName = "unreviewed-admin"
		},
		"volume source": func(pod *corev1.Pod) {
			pod.Spec.Volumes[0].Projected.DefaultMode = &mode
		},
		"security context": func(pod *corev1.Pod) {
			pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: &privileged}
		},
		"init container": func(pod *corev1.Pod) {
			pod.Spec.InitContainers = []corev1.Container{{Name: "unreviewed-init", Image: pod.Spec.Containers[0].Image}}
		},
		"ephemeral container": func(pod *corev1.Pod) {
			pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
				EphemeralContainerCommon: corev1.EphemeralContainerCommon{
					Name: "unreviewed-debug", Image: pod.Spec.Containers[0].Image,
				},
			}}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			identity, live, replicaSet := controllerExecutionObjects(t)
			mutate(live)
			h := New(Config{})
			configureControllerPodDryRunFixture(t, h)
			_, err := h.bindLivePodExecutionProvenance(context.Background(), identity, live, replicaSet)
			if err == nil || !strings.Contains(err.Error(), "PodSpec differs from dry-run CREATE admission result") {
				t.Fatalf("same-image live Pod %s mutation error = %v", name, err)
			}
		})
	}
}

func TestBindLivePodExecutionProvenanceRequiresFullReplicaSetSelector(t *testing.T) {
	identity, live, replicaSet := controllerExecutionObjects(t)
	delete(live.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	h := New(Config{})
	h.dryRunCreatePodFn = func(context.Context, string, *corev1.Pod, metav1.CreateOptions) (*corev1.Pod, error) {
		t.Fatal("dry-run CREATE must not run after the full selector fails")
		return nil, nil
	}
	_, err := h.bindLivePodExecutionProvenance(context.Background(), identity, live, replicaSet)
	if err == nil || !strings.Contains(err.Error(), "labels do not satisfy full ReplicaSet selector") {
		t.Fatalf("full ReplicaSet selector error = %v", err)
	}
}

func TestBindLivePodExecutionProvenancePreservesExplicitTemplateNodeName(t *testing.T) {
	identity, live, replicaSet := controllerExecutionObjects(t)
	replicaSet.Spec.Template.Spec.NodeName = "pinned-node"
	live.Spec.NodeName = "pinned-node"
	h := New(Config{})
	configureControllerPodDryRunFixture(t, h)
	if _, err := h.bindLivePodExecutionProvenance(context.Background(), identity, live, replicaSet); err != nil {
		t.Fatalf("matching explicit nodeName rejected: %v", err)
	}
	live.Spec.NodeName = "other-node"
	if _, err := h.bindLivePodExecutionProvenance(context.Background(), identity, live, replicaSet); err == nil ||
		!strings.Contains(err.Error(), "PodSpec differs from dry-run CREATE admission result") {
		t.Fatalf("explicit nodeName drift error = %v", err)
	}
}

func TestBindLivePodExecutionProvenanceAllowsAutomountDisabledWithoutAccessVolume(t *testing.T) {
	identity, live, replicaSet := controllerExecutionObjects(t)
	automount := false
	live.Spec.AutomountServiceAccountToken = &automount
	live.Spec.Volumes = nil
	live.Spec.Containers[0].VolumeMounts = nil
	replicaSet.Spec.Template.Spec.AutomountServiceAccountToken = &automount
	h := New(Config{})
	h.dryRunCreatePodFn = func(
		_ context.Context, _ string, request *corev1.Pod, options metav1.CreateOptions,
	) (*corev1.Pod, error) {
		if !reflect.DeepEqual(options.DryRun, []string{metav1.DryRunAll}) || options.FieldManager != "" {
			t.Fatalf("dry-run CREATE options = %+v", options)
		}
		return request.DeepCopy(), nil
	}
	if _, err := h.bindLivePodExecutionProvenance(context.Background(), identity, live, replicaSet); err != nil {
		t.Fatalf("automount-disabled Pod without API access volume rejected: %v", err)
	}
}

func TestBindLivePodExecutionProvenanceReportsDryRunFailureAsNonMutating(t *testing.T) {
	identity, live, replicaSet := controllerExecutionObjects(t)
	h := New(Config{})
	h.dryRunCreatePodFn = func(context.Context, string, *corev1.Pod, metav1.CreateOptions) (*corev1.Pod, error) {
		return nil, errors.New("admission unavailable")
	}
	_, err := h.bindLivePodExecutionProvenance(context.Background(), identity, live, replicaSet)
	if err == nil || !strings.Contains(err.Error(), "dry-run CREATE") || !strings.Contains(err.Error(), "no mutation") {
		t.Fatalf("dry-run failure does not state the non-mutation boundary: %v", err)
	}
}

func TestNormalizeServiceAccountVolumeNamesFailsClosed(t *testing.T) {
	pod := controllerPodFixture(
		"operator", "loom-mills", "pod-uid", "operator:v1", "operator@sha256:abc",
		"loom-mills-operator", "deployment-uid", time.Now().UTC(),
	)
	base := controllerFixtureAdmittedPodSpec(pod, "kube-api-access-one", "")
	tests := map[string]struct {
		mutate  func(*corev1.PodSpec)
		wantErr string
	}{
		"reserved sentinel": {
			mutate: func(spec *corev1.PodSpec) {
				spec.Volumes[0].Name = normalizedServiceAccountName
			},
			wantErr: "reserved normalized volume name",
		},
		"multiple access volumes": {
			mutate: func(spec *corev1.PodSpec) {
				spec.Volumes = append(spec.Volumes, controllerFixtureAccessVolume("kube-api-access-two"))
			},
			wantErr: "multiple kube-api-access projected volumes",
		},
		"prefix mount without volume": {
			mutate: func(spec *corev1.PodSpec) {
				spec.Volumes = nil
			},
			wantErr: "mount without a projected volume",
		},
		"access volume without mount": {
			mutate: func(spec *corev1.PodSpec) {
				spec.Containers[0].VolumeMounts = nil
			},
			wantErr: "has no container mount",
		},
		"unmatched prefix mount": {
			mutate: func(spec *corev1.PodSpec) {
				spec.Containers[0].VolumeMounts[0].Name = "kube-api-access-other"
			},
			wantErr: "references unmatched kube-api-access volume",
		},
		"nonprojected prefix volume": {
			mutate: func(spec *corev1.PodSpec) {
				spec.Volumes[0].Projected = nil
				spec.Volumes[0].EmptyDir = &corev1.EmptyDirVolumeSource{}
			},
			wantErr: "without a projected source",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			spec := base.DeepCopy()
			test.mutate(spec)
			if err := normalizeServiceAccountVolumeNames(spec); err == nil ||
				!strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("normalizeServiceAccountVolumeNames() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestNormalizeServiceAccountVolumeNamesCoversEveryContainerKind(t *testing.T) {
	pod := controllerPodFixture(
		"operator", "loom-mills", "pod-uid", "operator:v1", "operator@sha256:abc",
		"loom-mills-operator", "deployment-uid", time.Now().UTC(),
	)
	const originalName = "kube-api-access-random"
	spec := controllerFixtureAdmittedPodSpec(pod, originalName, "")
	spec.InitContainers = []corev1.Container{{
		Name: "init", Image: pod.Image,
		VolumeMounts: []corev1.VolumeMount{controllerFixtureAccessMount(originalName)},
	}}
	spec.EphemeralContainers = []corev1.EphemeralContainer{{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name: "debug", Image: pod.Image,
			VolumeMounts: []corev1.VolumeMount{controllerFixtureAccessMount(originalName)},
		},
	}}
	if err := normalizeServiceAccountVolumeNames(&spec); err != nil {
		t.Fatalf("normalizeServiceAccountVolumeNames() error = %v", err)
	}
	if spec.Volumes[0].Name != normalizedServiceAccountName ||
		spec.Containers[0].VolumeMounts[0].Name != normalizedServiceAccountName ||
		spec.InitContainers[0].VolumeMounts[0].Name != normalizedServiceAccountName ||
		spec.EphemeralContainers[0].VolumeMounts[0].Name != normalizedServiceAccountName {
		t.Fatalf("not every access-volume reference was normalized: %+v", spec)
	}
}
