package killtest

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

const controllerFixtureTemplateHash = "fixture-template-hash"

func controllerFixtureAccessVolume(name string) corev1.Volume {
	return corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{},
		},
	}
}

func controllerFixtureAccessMount(name string) corev1.VolumeMount {
	return corev1.VolumeMount{
		Name: name, MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true,
	}
}

func controllerFixtureAdmittedPodSpec(pod PodIdentity, volumeName, nodeName string) corev1.PodSpec {
	return corev1.PodSpec{
		NodeName: nodeName,
		Containers: []corev1.Container{{
			Name: pod.ContainerName, Image: pod.Image,
			VolumeMounts: []corev1.VolumeMount{controllerFixtureAccessMount(volumeName)},
		}},
		Volumes: []corev1.Volume{controllerFixtureAccessVolume(volumeName)},
	}
}

func bindControllerPodExecutionFixture(pod PodIdentity) PodIdentity {
	digest, err := canonicalAdmittedPodSpecSHA256(
		controllerFixtureAdmittedPodSpec(pod, "kube-api-access-live", pod.Node), true,
	)
	if err != nil {
		panic(err)
	}
	pod.PodExecutionContract = PodExecutionProvenanceContract
	pod.PodExecutionContractVersion = PodExecutionProvenanceContractVersion
	pod.PodExecutionRenderer = PodExecutionRenderer
	pod.PodExecutionRendererVersion = PodExecutionRendererVersion
	pod.LivePodSpecSHA256 = digest
	pod.DryRunPodSpecSHA256 = digest
	return pod
}

func controllerPodFixture(
	name, namespace, uid, image, imageID, deploymentName, deploymentUID string,
	startedAt time.Time,
) PodIdentity {
	pod := PodIdentity{
		Name: name, Namespace: namespace, UID: uid, ResourceVersion: "pod-rv-" + uid,
		PodCensusListResourceVersion: "pod-list-rv-" + uid, PodCensusCount: 1,
		Node: "worker-1", Image: image, ImageID: imageID, StartedAt: startedAt,
		ContainerName: "controller", ContainerID: "containerd://" + uid,
		ContainerStartedAt:        startedAt.Add(time.Second),
		ReplicaSetName:            deploymentName + "-rs-abc",
		ReplicaSetUID:             deploymentUID + "-rs-uid",
		ReplicaSetResourceVersion: "rs-rv-" + deploymentUID,
		ReplicaSetGeneration:      3, ReplicaSetObservedGeneration: 3,
		ReplicaSetDesiredReplicas: 1, ReplicaSetReplicas: 1,
		ReplicaSetFullyLabeledReplicas: 1, ReplicaSetReadyReplicas: 1,
		ReplicaSetAvailableReplicas: 1,
		DeploymentName:              deploymentName,
		DeploymentUID:               deploymentUID,
	}
	return bindControllerPodExecutionFixture(pod)
}

func bindControllerPodFixture(pod PodIdentity, deployment DeploymentIdentity) PodIdentity {
	bound := controllerPodFixture(
		pod.Name, deployment.Namespace, pod.UID, pod.Image, pod.ImageID,
		deployment.Name, deployment.UID, pod.StartedAt,
	)
	if pod.Node != "" {
		bound.Node = pod.Node
	}
	bound.ReplicaSetPodTemplateSHA256 = deployment.PodTemplateSHA256
	bound.ReplicaSetSelectorSHA256 = deployment.SelectorSHA256
	return bound
}

func replacementControllerPodFixture(before PodIdentity, name, uid string, startedAt time.Time) PodIdentity {
	replacement := controllerPodFixture(
		name, before.Namespace, uid, before.Image, before.ImageID,
		before.DeploymentName, before.DeploymentUID, startedAt,
	)
	replacement.Node = before.Node
	replacement.ReplicaSetPodTemplateSHA256 = before.ReplicaSetPodTemplateSHA256
	replacement.ReplicaSetSelectorSHA256 = before.ReplicaSetSelectorSHA256
	return replacement
}

func controllerPodListFixtureJSON(pod PodIdentity) string {
	controller := true
	object := map[string]any{
		"metadata": map[string]any{
			"name": pod.Name, "namespace": pod.Namespace, "uid": pod.UID,
			"resourceVersion": pod.ResourceVersion,
			"labels": map[string]any{
				"app": pod.DeploymentName, "app.kubernetes.io/name": pod.DeploymentName,
				appsv1.DefaultDeploymentUniqueLabelKey: controllerFixtureTemplateHash,
			},
			"ownerReferences": []map[string]any{{
				"apiVersion": "apps/v1", "kind": "ReplicaSet", "name": pod.ReplicaSetName,
				"uid": pod.ReplicaSetUID, "controller": controller,
			}},
		},
		"spec": map[string]any{
			"nodeName": pod.Node,
			"containers": []map[string]any{{
				"name": pod.ContainerName, "image": pod.Image,
				"volumeMounts": []map[string]any{{
					"name": "kube-api-access-live", "mountPath": "/var/run/secrets/kubernetes.io/serviceaccount",
					"readOnly": true,
				}},
			}},
			"volumes": []map[string]any{{
				"name": "kube-api-access-live", "projected": map[string]any{},
			}},
		},
		"status": map[string]any{
			"phase": "Running", "startTime": pod.StartedAt,
			"conditions": []map[string]any{{"type": "Ready", "status": "True"}},
			"containerStatuses": []map[string]any{{
				"name": pod.ContainerName, "ready": true, "imageID": pod.ImageID,
				"containerID": pod.ContainerID, "restartCount": pod.ContainerRestartCount,
				"state": map[string]any{"running": map[string]any{"startedAt": pod.ContainerStartedAt}},
			}},
		},
	}
	blob, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": pod.PodCensusListResourceVersion},
		"items":    []any{object},
	})
	if err != nil {
		panic(err)
	}
	return string(blob)
}

func controllerReplicaSetFixtureJSON(pod PodIdentity, deploymentJSON ...string) string {
	controller := true
	replicas := int32(1)
	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": pod.DeploymentName}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: pod.ContainerName, Image: pod.Image}}},
	}
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": pod.DeploymentName}}
	if len(deploymentJSON) > 0 {
		var deployment appsv1.Deployment
		if err := json.Unmarshal([]byte(deploymentJSON[0]), &deployment); err != nil {
			panic(err)
		}
		template = *deployment.Spec.Template.DeepCopy()
		selector = deployment.Spec.Selector.DeepCopy()
	}
	if template.Labels == nil {
		template.Labels = make(map[string]string)
	}
	template.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = controllerFixtureTemplateHash
	if selector.MatchLabels == nil {
		selector.MatchLabels = make(map[string]string)
	}
	selector.MatchLabels[appsv1.DefaultDeploymentUniqueLabelKey] = controllerFixtureTemplateHash
	replicaSet := appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: pod.ReplicaSetName, Namespace: pod.Namespace, UID: types.UID(pod.ReplicaSetUID),
			ResourceVersion: pod.ReplicaSetResourceVersion, Generation: 3,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: pod.DeploymentName,
				UID: types.UID(pod.DeploymentUID), Controller: &controller,
			}},
		},
		Spec: appsv1.ReplicaSetSpec{Replicas: &replicas, Selector: selector, Template: template},
		Status: appsv1.ReplicaSetStatus{
			ObservedGeneration: 3, Replicas: 1, FullyLabeledReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
		},
	}
	blob, err := json.Marshal(replicaSet)
	if err != nil {
		panic(err)
	}
	return string(blob)
}

func configureControllerPodDryRunFixture(t *testing.T, h *Harness) {
	t.Helper()
	h.dryRunCreatePodFn = func(
		_ context.Context, namespace string, request *corev1.Pod, options metav1.CreateOptions,
	) (*corev1.Pod, error) {
		if !reflect.DeepEqual(options.DryRun, []string{metav1.DryRunAll}) || options.FieldManager != "" {
			t.Fatalf("dry-run CREATE options = %+v, want DryRunAll and no field manager", options)
		}
		if request == nil {
			t.Fatal("dry-run CREATE request is nil")
		}
		if request.APIVersion != "v1" || request.Kind != "Pod" || request.Namespace != namespace ||
			request.Name != "" || request.GenerateName == "" || request.UID != "" || request.ResourceVersion != "" ||
			!request.CreationTimestamp.IsZero() || request.DeletionTimestamp != nil || !reflect.DeepEqual(request.Status, corev1.PodStatus{}) {
			t.Fatalf("dry-run CREATE request has non-create identity/status fields: %+v", request)
		}
		if len(request.OwnerReferences) != 1 || request.OwnerReferences[0].Kind != "ReplicaSet" ||
			request.OwnerReferences[0].APIVersion != "apps/v1" ||
			request.OwnerReferences[0].Controller == nil || !*request.OwnerReferences[0].Controller ||
			request.OwnerReferences[0].BlockOwnerDeletion == nil || !*request.OwnerReferences[0].BlockOwnerDeletion ||
			replicaSetPodGenerateName(request.OwnerReferences[0].Name) != request.GenerateName ||
			request.OwnerReferences[0].UID == "" {
			t.Fatalf("dry-run CREATE request has inexact ReplicaSet controller owner: %+v", request.OwnerReferences)
		}
		admitted := request.DeepCopy()
		admitted.Name = request.GenerateName + "dry-run"
		const volumeName = "kube-api-access-dry-run"
		admitted.Spec.Volumes = append(admitted.Spec.Volumes, controllerFixtureAccessVolume(volumeName))
		for i := range admitted.Spec.Containers {
			admitted.Spec.Containers[i].VolumeMounts = append(
				admitted.Spec.Containers[i].VolumeMounts, controllerFixtureAccessMount(volumeName),
			)
		}
		return admitted, nil
	}
}

func controllerDeploymentFixtureJSON(name, namespace, uid, image string) string {
	object := map[string]any{
		"metadata": map[string]any{
			"name": name, "namespace": namespace, "uid": uid,
			"resourceVersion": "deployment-rv-" + uid, "generation": 7,
		},
		"spec": map[string]any{
			"replicas": 1, "selector": map[string]any{"matchLabels": map[string]any{"app": name}},
			"strategy": map[string]any{"type": "Recreate"},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": name}, "annotations": map[string]any{
					"loom.flexinfer.ai/policy-checksum": "checksum-1",
				}},
				"spec": map[string]any{"containers": []map[string]any{{"name": "controller", "image": image}}},
			},
		},
		"status": map[string]any{
			"observedGeneration": 7, "replicas": 1, "updatedReplicas": 1,
			"readyReplicas": 1, "availableReplicas": 1,
		},
	}
	blob, err := json.Marshal(object)
	if err != nil {
		panic(err)
	}
	return string(blob)
}

func mutateJSONObject(raw string, mutate func(map[string]any)) string {
	var object map[string]any
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		panic(err)
	}
	mutate(object)
	blob, err := json.Marshal(object)
	if err != nil {
		panic(err)
	}
	return string(blob)
}

func mutateReplicaSetFixtureJSON(raw string, mutate func(*appsv1.ReplicaSet)) string {
	var replicaSet appsv1.ReplicaSet
	if err := json.Unmarshal([]byte(raw), &replicaSet); err != nil {
		panic(err)
	}
	mutate(&replicaSet)
	blob, err := json.Marshal(replicaSet)
	if err != nil {
		panic(err)
	}
	return string(blob)
}

func firstPodObject(object map[string]any) map[string]any {
	return object["items"].([]any)[0].(map[string]any)
}

func cloneJSONObject(object map[string]any) map[string]any {
	blob, err := json.Marshal(object)
	if err != nil {
		panic(err)
	}
	var clone map[string]any
	if err := json.Unmarshal(blob, &clone); err != nil {
		panic(err)
	}
	return clone
}

func mustStableDeploymentFixture(raw string) DeploymentIdentity {
	identity, err := parseStableDeployment(raw)
	if err != nil {
		panic(err)
	}
	identity.ReviewedPodTemplateSHA256 = identity.PodTemplateSHA256
	identity.ReviewedSelectorSHA256 = identity.SelectorSHA256
	return identity
}

func TestControllerPodFixtureIsValid(t *testing.T) {
	pod := controllerPodFixture(
		"operator", "loom-mills", "pod-uid", "operator:v1", "operator@sha256:abc",
		"loom-mills-operator", "deployment-uid", time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC),
	)
	deploymentJSON := controllerDeploymentFixtureJSON(pod.DeploymentName, pod.Namespace, pod.DeploymentUID, pod.Image)
	deployment := mustStableDeploymentFixture(deploymentJSON)
	pod = bindControllerPodFixture(pod, deployment)
	parsed, err := parseReadyPod(controllerPodListFixtureJSON(pod), pod.Namespace,
		"app="+pod.DeploymentName, pod.ContainerName)
	if err != nil {
		t.Fatalf("parse controller pod fixture: %v", err)
	}
	bound, err := bindReplicaSetOwner(controllerReplicaSetFixtureJSON(pod, deploymentJSON), pod.Namespace, parsed, deployment)
	if err != nil {
		t.Fatalf("bind controller pod fixture: %v", err)
	}
	want := pod
	want.PodExecutionContract = ""
	want.PodExecutionContractVersion = 0
	want.PodExecutionRenderer = ""
	want.PodExecutionRendererVersion = ""
	want.LivePodSpecSHA256 = ""
	want.DryRunPodSpecSHA256 = ""
	if bound != want {
		t.Fatalf("bound controller pod = %+v, want %+v", bound, want)
	}
}

func TestReplicaSetTemplateHashNormalizationMatchesDeploymentWithoutMutatingEither(t *testing.T) {
	deploymentJSON := controllerDeploymentFixtureJSON(
		"loom-mills-operator", "loom-mills", "deployment-uid", "operator:v1",
	)
	deployment := mustStableDeploymentFixture(deploymentJSON)
	pod := bindControllerPodFixture(controllerPodFixture(
		"operator", deployment.Namespace, "pod-uid", deployment.Image, "operator@sha256:abc",
		deployment.Name, deployment.UID, time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC),
	), deployment)
	var replicaSet appsv1.ReplicaSet
	if err := json.Unmarshal([]byte(controllerReplicaSetFixtureJSON(pod, deploymentJSON)), &replicaSet); err != nil {
		t.Fatal(err)
	}
	hashBefore := replicaSet.Spec.Template.Labels[appsv1.DefaultDeploymentUniqueLabelKey]
	digest, err := canonicalReplicaSetPodTemplateSHA256(replicaSet.Spec.Template)
	if err != nil {
		t.Fatalf("canonicalReplicaSetPodTemplateSHA256() error = %v", err)
	}
	if digest != deployment.PodTemplateSHA256 {
		t.Fatalf("normalized ReplicaSet/Deployment pod templates = %s/%s", digest, deployment.PodTemplateSHA256)
	}
	if replicaSet.Spec.Template.Labels[appsv1.DefaultDeploymentUniqueLabelKey] != hashBefore || hashBefore == "" {
		t.Fatal("canonicalization mutated the typed ReplicaSet template or lost its controller hash")
	}
}

func TestBindReplicaSetOwnerRejectsSameImageTemplateMutation(t *testing.T) {
	deploymentJSON := controllerDeploymentFixtureJSON(
		"loom-mills-operator", "loom-mills", "deployment-uid", "operator:v1",
	)
	deployment := mustStableDeploymentFixture(deploymentJSON)
	pod := bindControllerPodFixture(controllerPodFixture(
		"operator", deployment.Namespace, "pod-uid", deployment.Image, "operator@sha256:abc",
		deployment.Name, deployment.UID, time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC),
	), deployment)
	parsed, err := parseReadyPod(controllerPodListFixtureJSON(pod), pod.Namespace,
		"app="+pod.DeploymentName, pod.ContainerName)
	if err != nil {
		t.Fatal(err)
	}
	base := controllerReplicaSetFixtureJSON(pod, deploymentJSON)
	runAsNonRoot := true
	tests := map[string]func(*appsv1.ReplicaSet){
		"command": func(replicaSet *appsv1.ReplicaSet) {
			replicaSet.Spec.Template.Spec.Containers[0].Command = []string{"/bin/attacker"}
		},
		"environment": func(replicaSet *appsv1.ReplicaSet) {
			replicaSet.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "UNREVIEWED", Value: "true"}}
		},
		"service account": func(replicaSet *appsv1.ReplicaSet) {
			replicaSet.Spec.Template.Spec.ServiceAccountName = "unreviewed-admin"
		},
		"volume": func(replicaSet *appsv1.ReplicaSet) {
			replicaSet.Spec.Template.Spec.Volumes = []corev1.Volume{{
				Name: "host", VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: "/"},
				},
			}}
		},
		"security context": func(replicaSet *appsv1.ReplicaSet) {
			replicaSet.Spec.Template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{
				RunAsNonRoot: &runAsNonRoot,
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			mutated := mutateReplicaSetFixtureJSON(base, mutate)
			_, err := bindReplicaSetOwner(mutated, pod.Namespace, parsed, deployment)
			if err == nil || !strings.Contains(err.Error(), "pod template differs from live Deployment") {
				t.Fatalf("same-image ReplicaSet %s mutation error = %v", name, err)
			}
		})
	}
}

func TestDeploymentRejectsReservedControllerHashLabel(t *testing.T) {
	base := controllerDeploymentFixtureJSON(
		"loom-mills-operator", "loom-mills", "deployment-uid", "operator:v1",
	)
	for _, location := range []string{"template", "selector"} {
		t.Run(location, func(t *testing.T) {
			mutated := mutateJSONObject(base, func(object map[string]any) {
				spec := object["spec"].(map[string]any)
				var labels map[string]any
				if location == "template" {
					labels = spec["template"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]any)
				} else {
					labels = spec["selector"].(map[string]any)["matchLabels"].(map[string]any)
				}
				labels[appsv1.DefaultDeploymentUniqueLabelKey] = "forged"
			})
			_, err := parseStableDeployment(mutated)
			if err == nil || !strings.Contains(err.Error(), "reserved controller label") {
				t.Fatalf("parseStableDeployment() error = %v, want reserved label rejection", err)
			}
		})
	}
}

func TestValidateCrashPodIdentityRejectsSerializedReplicaSetSingletonDrift(t *testing.T) {
	deployment := mustStableDeploymentFixture(controllerDeploymentFixtureJSON(
		"loom-mills-operator", "loom-mills", "deployment-uid", "operator:v1",
	))
	pod := bindControllerPodFixture(controllerPodFixture(
		"operator", deployment.Namespace, "pod-uid", deployment.Image, "operator@sha256:abc",
		deployment.Name, deployment.UID, time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC),
	), deployment)
	if err := validateCrashPodIdentity(pod); err != nil {
		t.Fatalf("valid serialized controller identity rejected: %v", err)
	}
	for name, mutate := range map[string]func(*PodIdentity){
		"missing census list resourceVersion": func(pod *PodIdentity) { pod.PodCensusListResourceVersion = "" },
		"overlapping census":                  func(pod *PodIdentity) { pod.PodCensusCount = 2 },
		"missing execution contract":          func(pod *PodIdentity) { pod.PodExecutionContract = "" },
		"wrong execution contract version":    func(pod *PodIdentity) { pod.PodExecutionContractVersion++ },
		"missing execution renderer":          func(pod *PodIdentity) { pod.PodExecutionRenderer = "" },
		"wrong execution renderer version":    func(pod *PodIdentity) { pod.PodExecutionRendererVersion = "2" },
		"missing live PodSpec digest":         func(pod *PodIdentity) { pod.LivePodSpecSHA256 = "" },
		"live/dry-run PodSpec mismatch":       func(pod *PodIdentity) { pod.DryRunPodSpecSHA256 = strings.Repeat("a", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := pod
			mutate(&changed)
			if err := validateCrashPodIdentity(changed); err == nil ||
				(!strings.Contains(err.Error(), "incomplete identity") &&
					!strings.Contains(err.Error(), "execution provenance is incomplete or drifted")) {
				t.Fatalf("validateCrashPodIdentity() error = %v", err)
			}
		})
	}
	tests := map[string]func(*PodIdentity){
		"generation":          func(pod *PodIdentity) { pod.ReplicaSetGeneration = 0 },
		"observed generation": func(pod *PodIdentity) { pod.ReplicaSetObservedGeneration-- },
		"desired replicas":    func(pod *PodIdentity) { pod.ReplicaSetDesiredReplicas = 2 },
		"actual replicas":     func(pod *PodIdentity) { pod.ReplicaSetReplicas = 2 },
		"fully labeled":       func(pod *PodIdentity) { pod.ReplicaSetFullyLabeledReplicas = 0 },
		"ready":               func(pod *PodIdentity) { pod.ReplicaSetReadyReplicas = 0 },
		"available":           func(pod *PodIdentity) { pod.ReplicaSetAvailableReplicas = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			changed := pod
			mutate(&changed)
			if err := validateCrashPodIdentity(changed); err == nil ||
				!strings.Contains(err.Error(), "ReplicaSet is not a fully observed stable singleton") {
				t.Fatalf("validateCrashPodIdentity() error = %v", err)
			}
		})
	}
}

func TestSameControllerPodIncarnationIgnoresOnlyCensusListResourceVersion(t *testing.T) {
	deployment := mustStableDeploymentFixture(controllerDeploymentFixtureJSON(
		"loom-mills-operator", "loom-mills", "deployment-uid", "operator:v1",
	))
	pod := bindControllerPodFixture(controllerPodFixture(
		"operator", deployment.Namespace, "pod-uid", deployment.Image, "operator@sha256:abc",
		deployment.Name, deployment.UID, time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC),
	), deployment)
	listRVOnly := pod
	listRVOnly.PodCensusListResourceVersion = "new-list-observation-rv"
	if !sameControllerPodIncarnation(pod, listRVOnly) {
		t.Fatal("observation-local Pod List resourceVersion changed the controller incarnation")
	}
	for name, mutate := range map[string]func(*PodIdentity){
		"Pod resourceVersion":        func(changed *PodIdentity) { changed.ResourceVersion = "new-pod-rv" },
		"ReplicaSet resourceVersion": func(changed *PodIdentity) { changed.ReplicaSetResourceVersion = "new-rs-rv" },
		"container identity":         func(changed *PodIdentity) { changed.ContainerID = "containerd://other" },
		"execution provenance":       func(changed *PodIdentity) { changed.LivePodSpecSHA256 = strings.Repeat("b", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := pod
			mutate(&changed)
			if sameControllerPodIncarnation(pod, changed) {
				t.Fatalf("%s drift was ignored", name)
			}
		})
	}
}

func TestParseReadyPodFailsClosedOnIncompleteOrUnsafeControllerIdentity(t *testing.T) {
	pod := controllerPodFixture(
		"operator", "loom-mills", "pod-uid", "operator:v1", "operator@sha256:abc",
		"loom-mills-operator", "deployment-uid", time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC),
	)
	base := controllerPodListFixtureJSON(pod)
	tests := []struct {
		name    string
		mutate  func(map[string]any)
		wantErr string
	}{
		{
			name: "paginated namespace census",
			mutate: func(object map[string]any) {
				object["metadata"].(map[string]any)["continue"] = "next-page-token"
			},
			wantErr: "namespace-wide pod census has incomplete list",
		},
		{
			name: "over-cap namespace census",
			mutate: func(object map[string]any) {
				object["items"] = make([]any, maxPodCensusItems+1)
			},
			wantErr: "namespace-wide pod census has incomplete list",
		},
		{
			name: "missing restartCount",
			mutate: func(object map[string]any) {
				status := firstPodObject(object)["status"].(map[string]any)
				delete(status["containerStatuses"].([]any)[0].(map[string]any), "restartCount")
			},
			wantErr: "restartCount is missing",
		},
		{
			name: "nonzero restartCount",
			mutate: func(object map[string]any) {
				status := firstPodObject(object)["status"].(map[string]any)
				status["containerStatuses"].([]any)[0].(map[string]any)["restartCount"] = float64(1)
			},
			wantErr: "restartCount=1, want 0",
		},
		{
			name: "missing Pod Ready condition",
			mutate: func(object map[string]any) {
				firstPodObject(object)["status"].(map[string]any)["conditions"] = []any{}
			},
			wantErr: "not a singleton non-terminating Ready pod",
		},
		{
			name: "Pod Ready false despite ready container",
			mutate: func(object map[string]any) {
				status := firstPodObject(object)["status"].(map[string]any)
				status["conditions"].([]any)[0].(map[string]any)["status"] = "False"
			},
			wantErr: "not a singleton non-terminating Ready pod",
		},
		{
			name: "duplicate Pod Ready conditions",
			mutate: func(object map[string]any) {
				status := firstPodObject(object)["status"].(map[string]any)
				status["conditions"] = append(status["conditions"].([]any), map[string]any{
					"type": "Ready", "status": "True",
				})
			},
			wantErr: "not a singleton non-terminating Ready pod",
		},
		{
			name: "missing controller owner",
			mutate: func(object map[string]any) {
				firstPodObject(object)["metadata"].(map[string]any)["ownerReferences"] = []any{}
			},
			wantErr: "no controller ownerReference",
		},
		{
			name: "multiple controller owners",
			mutate: func(object map[string]any) {
				metadata := firstPodObject(object)["metadata"].(map[string]any)
				owners := metadata["ownerReferences"].([]any)
				metadata["ownerReferences"] = append(owners, map[string]any{
					"apiVersion": "apps/v1", "kind": "ReplicaSet", "name": "lookalike-rs",
					"uid": "lookalike-rs-uid", "controller": true,
				})
			},
			wantErr: "multiple controller ownerReferences",
		},
		{
			name: "wrong namespace",
			mutate: func(object map[string]any) {
				firstPodObject(object)["metadata"].(map[string]any)["namespace"] = "other"
			},
			wantErr: "namespace-wide immutable container-name census",
		},
		{
			name: "missing node",
			mutate: func(object map[string]any) {
				firstPodObject(object)["spec"].(map[string]any)["nodeName"] = ""
			},
			wantErr: "not a singleton non-terminating Ready pod",
		},
		{
			name: "missing spec image",
			mutate: func(object map[string]any) {
				spec := firstPodObject(object)["spec"].(map[string]any)
				spec["containers"].([]any)[0].(map[string]any)["image"] = ""
			},
			wantErr: "not a singleton non-terminating Ready pod",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseReadyPod(mutateJSONObject(base, test.mutate), pod.Namespace,
				"app="+pod.DeploymentName, pod.ContainerName)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("parseReadyPod() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestReadyPodUsesOneBoundedRawCensusAndRejectsContinuation(t *testing.T) {
	deployment := mustStableDeploymentFixture(controllerDeploymentFixtureJSON(
		"loom-mills-operator", "loom-mills", "deployment-uid", "operator:v1",
	))
	pod := bindControllerPodFixture(controllerPodFixture(
		"operator", deployment.Namespace, "pod-uid", deployment.Image, "operator@sha256:abc",
		deployment.Name, deployment.UID, time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC),
	), deployment)
	paginated := mutateJSONObject(controllerPodListFixtureJSON(pod), func(object map[string]any) {
		object["metadata"].(map[string]any)["continue"] = "next-page"
	})

	h := New(Config{})
	calls := 0
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		calls++
		want := []string{"get", "--raw", controllerPodCensusPath(deployment.Namespace)}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("controller census args = %#v, want %#v", args, want)
		}
		return paginated, nil
	}
	if _, err := h.readyPod(t.Context(), deployment.Namespace, "app="+deployment.Name, deployment); err == nil ||
		!strings.Contains(err.Error(), "incomplete list") {
		t.Fatalf("continued controller census error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("controller census calls = %d, want one raw API page", calls)
	}
}

func TestParseReadyPodNamespaceCensusRejectsHiddenController(t *testing.T) {
	pod := controllerPodFixture(
		"operator", "loom-mills", "pod-uid", "operator:v1", "operator@sha256:abc",
		"loom-mills-operator", "deployment-uid", time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC),
	)
	base := controllerPodListFixtureJSON(pod)
	for _, removeOwner := range []bool{false, true} {
		name := "label removed"
		if removeOwner {
			name = "label and owner removed"
		}
		t.Run(name, func(t *testing.T) {
			forged := mutateJSONObject(base, func(object map[string]any) {
				hidden := cloneJSONObject(firstPodObject(object))
				metadata := hidden["metadata"].(map[string]any)
				metadata["name"] = "operator-hidden"
				metadata["uid"] = "hidden-pod-uid"
				metadata["resourceVersion"] = "hidden-pod-rv"
				metadata["labels"] = map[string]any{"unrelated": "true"}
				if removeOwner {
					metadata["ownerReferences"] = []any{}
				}
				status := hidden["status"].(map[string]any)
				status["containerStatuses"].([]any)[0].(map[string]any)["containerID"] = "containerd://hidden"
				object["items"] = append(object["items"].([]any), hidden)
			})
			_, err := parseReadyPod(forged, pod.Namespace, "app="+pod.DeploymentName, pod.ContainerName)
			if err == nil || !strings.Contains(err.Error(), "census") || !strings.Contains(err.Error(), "found 2") {
				t.Fatalf("parseReadyPod() error = %v, want hidden-pod census rejection", err)
			}
		})
	}
}

func TestBindReplicaSetOwnerFailsClosedOnOwnerIdentityDrift(t *testing.T) {
	pod := controllerPodFixture(
		"operator", "loom-mills", "pod-uid", "operator:v1", "operator@sha256:abc",
		"loom-mills-operator", "deployment-uid", time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC),
	)
	deploymentJSON := controllerDeploymentFixtureJSON(pod.DeploymentName, pod.Namespace, pod.DeploymentUID, pod.Image)
	deployment := mustStableDeploymentFixture(deploymentJSON)
	pod = bindControllerPodFixture(pod, deployment)
	base := controllerReplicaSetFixtureJSON(pod, deploymentJSON)
	tests := []struct {
		name    string
		mutate  func(map[string]any)
		wantErr string
	}{
		{
			name: "ReplicaSet UID differs",
			mutate: func(object map[string]any) {
				object["metadata"].(map[string]any)["uid"] = "lookalike-rs-uid"
			},
			wantErr: "ReplicaSet owner identity differs from pod",
		},
		{
			name: "wrong Deployment owner",
			mutate: func(object map[string]any) {
				metadata := object["metadata"].(map[string]any)
				metadata["ownerReferences"].([]any)[0].(map[string]any)["uid"] = "other-deployment-uid"
			},
			wantErr: "is owned by Deployment",
		},
		{
			name: "multiple controller owners",
			mutate: func(object map[string]any) {
				metadata := object["metadata"].(map[string]any)
				owners := metadata["ownerReferences"].([]any)
				metadata["ownerReferences"] = append(owners, map[string]any{
					"apiVersion": "apps/v1", "kind": "Deployment", "name": "other",
					"uid": "other-uid", "controller": true,
				})
			},
			wantErr: "multiple controller ownerReferences",
		},
		{
			name: "terminating ReplicaSet",
			mutate: func(object map[string]any) {
				object["metadata"].(map[string]any)["deletionTimestamp"] = "2026-07-14T12:00:01Z"
			},
			wantErr: "ReplicaSet owner identity differs from pod",
		},
		{
			name: "desired replicas patched",
			mutate: func(object map[string]any) {
				object["spec"].(map[string]any)["replicas"] = float64(2)
			},
			wantErr: "not a singleton template",
		},
		{
			name: "status generation stale",
			mutate: func(object map[string]any) {
				object["status"].(map[string]any)["observedGeneration"] = float64(2)
			},
			wantErr: "not a singleton template",
		},
		{
			name: "ready replica missing",
			mutate: func(object map[string]any) {
				object["status"].(map[string]any)["readyReplicas"] = float64(0)
			},
			wantErr: "not a singleton template",
		},
		{
			name: "selector content patched",
			mutate: func(object map[string]any) {
				spec := object["spec"].(map[string]any)
				spec["selector"].(map[string]any)["matchLabels"].(map[string]any)["app"] = "other"
			},
			wantErr: "selector differs from live Deployment",
		},
		{
			name: "selector hash differs from template hash",
			mutate: func(object map[string]any) {
				spec := object["spec"].(map[string]any)
				spec["selector"].(map[string]any)["matchLabels"].(map[string]any)[appsv1.DefaultDeploymentUniqueLabelKey] = "other-hash"
			},
			wantErr: "labels do not match",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := bindReplicaSetOwner(mutateJSONObject(base, test.mutate), pod.Namespace, pod, deployment)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("bindReplicaSetOwner() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestReadyPodRejectsLabelLookalikeOwnedByAnotherDeployment(t *testing.T) {
	pod := controllerPodFixture(
		"operator-lookalike", "loom-mills", "pod-uid", "operator:v1", "operator@sha256:abc",
		"other-operator", "other-deployment-uid", time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC),
	)
	lookalikeDeploymentJSON := controllerDeploymentFixtureJSON(
		pod.DeploymentName, pod.Namespace, pod.DeploymentUID, pod.Image,
	)
	lookalikePodJSON := mutateJSONObject(controllerPodListFixtureJSON(pod), func(object map[string]any) {
		firstPodObject(object)["metadata"].(map[string]any)["labels"].(map[string]any)["app"] = "operator"
	})
	h := New(Config{})
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		command := strings.Join(args, " ")
		switch {
		case strings.Contains(command, controllerPodCensusPath(pod.Namespace)):
			return lookalikePodJSON, nil
		case strings.Contains(command, "get replicaset"):
			return controllerReplicaSetFixtureJSON(pod, lookalikeDeploymentJSON), nil
		default:
			return "", nil
		}
	}
	reviewedDeployment := mustStableDeploymentFixture(controllerDeploymentFixtureJSON(
		"loom-mills-operator", pod.Namespace, "reviewed-deployment-uid", pod.Image,
	))
	_, err := h.readyPod(context.Background(), pod.Namespace, "app=operator", reviewedDeployment)
	if err == nil || !strings.Contains(err.Error(), "is owned by Deployment") {
		t.Fatalf("readyPod() error = %v, want exact Deployment owner rejection", err)
	}
}

func TestDeletePodWithIdentitySendsUIDAndResourceVersionPreconditions(t *testing.T) {
	h := New(Config{})
	client := k8sfake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "operator", Namespace: "loom-mills", UID: types.UID("pod-uid"), ResourceVersion: "pod-rv",
	}})
	h.kube = client
	h.kubeOnce.Do(func() {})
	if err := h.deletePodWithIdentity(context.Background(), "loom-mills", "operator", "pod-uid", "pod-rv"); err != nil {
		t.Fatalf("deletePodWithIdentity() error = %v", err)
	}
	if len(client.Actions()) != 1 {
		t.Fatalf("client actions = %d, want 1", len(client.Actions()))
	}
	action, ok := client.Actions()[0].(clienttesting.DeleteAction)
	if !ok {
		t.Fatalf("action type = %T, want DeleteAction", client.Actions()[0])
	}
	options := action.GetDeleteOptions()
	if options.Preconditions == nil || options.Preconditions.UID == nil ||
		string(*options.Preconditions.UID) != "pod-uid" ||
		options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != "pod-rv" {
		t.Fatalf("delete preconditions = %+v, want uid=pod-uid resourceVersion=pod-rv", options.Preconditions)
	}

	for _, test := range []struct{ uid, resourceVersion string }{{"", "pod-rv"}, {"pod-uid", ""}} {
		if err := h.deletePodWithIdentity(context.Background(), "loom-mills", "operator", test.uid, test.resourceVersion); err == nil {
			t.Fatalf("deletePodWithIdentity(uid=%q, rv=%q) unexpectedly succeeded", test.uid, test.resourceVersion)
		}
	}
	if len(client.Actions()) != 1 {
		t.Fatalf("incomplete identities caused client actions: %d", len(client.Actions()))
	}
}

func TestCrashPodFinalRereadRejectsSameUIDContainerRestart(t *testing.T) {
	deploymentJSON := controllerDeploymentFixtureJSON(
		"loom-mills-operator", "loom-mills", "deployment-uid", "operator:v1",
	)
	deployment := mustStableDeploymentFixture(deploymentJSON)
	before := controllerPodFixture(
		"operator", deployment.Namespace, "pod-uid", deployment.Image, "operator@sha256:abc",
		deployment.Name, deployment.UID, time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC),
	)
	before = bindControllerPodFixture(before, deployment)
	restarted := before
	restarted.ResourceVersion = "pod-rv-after-restart"
	restarted.ContainerID = "containerd://restarted-container"
	restarted.ContainerRestartCount = 1
	restarted.ContainerStartedAt = before.ContainerStartedAt.Add(time.Minute)

	h := New(Config{})
	configureControllerPodDryRunFixture(t, h)
	podReads := 0
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		command := strings.Join(args, " ")
		switch {
		case strings.Contains(command, "get deploy"):
			return deploymentJSON, nil
		case strings.Contains(command, controllerPodCensusPath(deployment.Namespace)):
			podReads++
			if podReads == 1 {
				return controllerPodListFixtureJSON(before), nil
			}
			return controllerPodListFixtureJSON(restarted), nil
		case strings.Contains(command, "get replicaset"):
			return controllerReplicaSetFixtureJSON(before, deploymentJSON), nil
		default:
			return "", nil
		}
	}
	deleteCalled := false
	h.deletePodFn = func(context.Context, string, string, string, string) error {
		deleteCalled = true
		return nil
	}
	_, err := h.CrashPod(context.Background(), deployment.Namespace,
		"app="+deployment.Name, deployment.Name, before, deployment)
	if err == nil || !strings.Contains(err.Error(), "restartCount=1, want 0") {
		t.Fatalf("CrashPod() error = %v, want final-reread restart rejection", err)
	}
	if deleteCalled {
		t.Fatal("pod DELETE ran after same-UID container restart")
	}
	if podReads != 2 {
		t.Fatalf("pod identity reads = %d, want initial and final reread", podReads)
	}
}
