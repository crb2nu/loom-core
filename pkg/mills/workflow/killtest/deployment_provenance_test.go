package killtest

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const boundedCommandHelperEnv = "GO_WANT_MILLS_BOUNDED_COMMAND_HELPER"

func TestBoundedCommandOutputHelper(t *testing.T) {
	if os.Getenv(boundedCommandHelperEnv) != "1" {
		return
	}
	stdoutBytes, _ := strconv.Atoi(os.Getenv("MILLS_HELPER_STDOUT_BYTES"))
	stderrBytes, _ := strconv.Atoi(os.Getenv("MILLS_HELPER_STDERR_BYTES"))
	exitCode, _ := strconv.Atoi(os.Getenv("MILLS_HELPER_EXIT_CODE"))
	_, _ = os.Stdout.WriteString(strings.Repeat("o", stdoutBytes))
	_, _ = os.Stderr.WriteString(strings.Repeat("e", stderrBytes))
	os.Exit(exitCode)
}

func boundedCommandHelper(t *testing.T, stdoutBytes, stderrBytes, exitCode int) *exec.Cmd {
	t.Helper()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestBoundedCommandOutputHelper$")
	command.Env = append(os.Environ(),
		boundedCommandHelperEnv+"=1",
		"MILLS_HELPER_STDOUT_BYTES="+strconv.Itoa(stdoutBytes),
		"MILLS_HELPER_STDERR_BYTES="+strconv.Itoa(stderrBytes),
		"MILLS_HELPER_EXIT_CODE="+strconv.Itoa(exitCode),
	)
	return command
}

func TestRunBoundedCommandOutputRejectsStreamingStdoutOverflow(t *testing.T) {
	var destination bytes.Buffer
	output, err := runBoundedCommandOutput(boundedCommandHelper(t, 4096, 0, 0), &destination, 32, 32)
	if !output.stdoutOverflow || output.stdoutBytes != 32 || destination.Len() != 32 {
		t.Fatalf("stdout overflow=%t bytes=%d destination=%d, want true/32/32",
			output.stdoutOverflow, output.stdoutBytes, destination.Len())
	}
	if err == nil {
		t.Fatal("streaming stdout overflow did not fail the command")
	}
}

func TestRunBoundedCommandOutputCapsStderr(t *testing.T) {
	var destination bytes.Buffer
	output, err := runBoundedCommandOutput(boundedCommandHelper(t, 0, 4096, 7), &destination, 32, 64)
	if err == nil {
		t.Fatal("failing command unexpectedly succeeded")
	}
	if !output.stderrTruncated || len(output.stderr) != 64 {
		t.Fatalf("stderr truncated=%t bytes=%d, want true/64", output.stderrTruncated, len(output.stderr))
	}
}

func TestRunBoundedCommandBytesFailsClosedOnOutputLimits(t *testing.T) {
	for name, command := range map[string]*exec.Cmd{
		"stdout": boundedCommandHelper(t, 4096, 0, 0),
		"stderr": boundedCommandHelper(t, 0, 4096, 0),
	} {
		t.Run(name, func(t *testing.T) {
			output, err := runBoundedCommandBytes(command, "reviewed command", 32, 64)
			if err == nil || !strings.Contains(err.Error(), name+" exceeds") {
				t.Fatalf("%s overflow accepted: output=%d error=%v", name, len(output), err)
			}
			if output != nil {
				t.Fatalf("%s overflow returned partial output: %d bytes", name, len(output))
			}
		})
	}
}

func TestExtractReviewedGitArchiveRejectsEntryCountOverflow(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for index := 0; index < 3; index++ {
		if err := writer.WriteHeader(&tar.Header{
			Name: fmt.Sprintf("entry-%d", index), Mode: 0o600, Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	err := extractReviewedGitArchive(bytes.NewReader(archive.Bytes()), destination, 2)
	if err == nil || !strings.Contains(err.Error(), "entry count exceeds 2") {
		t.Fatalf("archive entry overflow accepted: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(destination, "entry-2")); !os.IsNotExist(statErr) {
		t.Fatalf("overflow entry was materialized: %v", statErr)
	}
}

type reviewedContentReadError struct {
	err error
}

func (reader reviewedContentReadError) Read([]byte) (int, error) {
	return 0, reader.err
}

func TestReadBoundedReviewedContentPropagatesReadError(t *testing.T) {
	readErr := errors.New("injected reviewed-file read error")
	if _, err := readBoundedReviewedContent("reviewed test", reviewedContentReadError{err: readErr}, 32); !errors.Is(err, readErr) {
		t.Fatalf("reviewed-file read error was not propagated: %v", err)
	}
}

func TestReviewedRegularFilesRejectOperationSpecificOverflow(t *testing.T) {
	for name, test := range map[string]struct {
		limit int64
		read  func(string) error
	}{
		"policy": {
			limit: maxReviewedPolicySourceSize,
			read: func(path string) error {
				_, err := readBoundedRegularFile(path, "reviewed policy source", maxReviewedPolicySourceSize)
				return err
			},
		},
		"Flux manifest": {
			limit: maxReviewedFluxManifestSize,
			read:  rejectDynamicFluxSubstitution,
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "reviewed-source")
			file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Truncate(test.limit + 1); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := test.read(path); err == nil || !strings.Contains(err.Error(), "exceeds") {
				t.Fatalf("oversized reviewed %s accepted: %v", name, err)
			}
		})
	}
}

func deploymentProvenanceTestObject(namespace, name, image string) *appsv1.Deployment {
	one := int32(1)
	labels := map[string]string{"app": name}
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, UID: "deployment-uid", ResourceVersion: "10", Generation: 7,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: labels},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: image}}},
			},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 7, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
		},
	}
}

func deploymentProvenanceTestIdentity(t *testing.T, deployment *appsv1.Deployment) DeploymentIdentity {
	t.Helper()
	digest, err := canonicalDeploymentSpecSHA256(deployment.Spec)
	if err != nil {
		t.Fatal(err)
	}
	podTemplateDigest, err := canonicalDeploymentPodTemplateSHA256(deployment.Spec.Template)
	if err != nil {
		t.Fatal(err)
	}
	selectorDigest, err := canonicalDeploymentSelectorSHA256(deployment.Spec.Selector)
	if err != nil {
		t.Fatal(err)
	}
	return DeploymentIdentity{
		Name: deployment.Name, Namespace: deployment.Namespace, UID: string(deployment.UID),
		ResourceVersion: deployment.ResourceVersion, Generation: deployment.Generation,
		ObservedGeneration: deployment.Generation, DesiredReplicas: 1, Replicas: 1,
		UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
		Image:         deployment.Spec.Template.Spec.Containers[0].Image,
		ContainerName: deployment.Spec.Template.Spec.Containers[0].Name,
		Strategy:      string(deployment.Spec.Strategy.Type), SpecSHA256: digest,
		PodTemplateSHA256: podTemplateDigest, SelectorSHA256: selectorDigest,
	}
}

func deploymentProvenanceTestReview(owner string) DeploymentReviewIdentity {
	return DeploymentReviewIdentity{
		Contract: DeploymentProvenanceContract, ContractVersion: DeploymentProvenanceContractVersion,
		FluxOwner: owner, FluxSpecSHA256: strings.Repeat("a", 64),
		Renderer: "flux build kustomization --dry-run", RendererVersion: "flux: v-test",
		RenderedSpecSHA256: strings.Repeat("b", 64),
		PlatformRevision:   strings.Repeat("c", 40), PlatformScopeDigest: strings.Repeat("d", 64),
		SourceRevision: strings.Repeat("c", 40), SourceScopeDigest: strings.Repeat("d", 64),
	}
}

func TestBindReviewedDeploymentRejectsFullSpecDrift(t *testing.T) {
	reviewed := deploymentProvenanceTestObject("loom-mills", "loom-mills-operator", "operator:v1")
	reviewed.UID, reviewed.ResourceVersion, reviewed.Generation = "", "", 0
	review := reviewedDeployment{
		Deployment:         reviewed,
		RenderedSpecSHA256: strings.Repeat("b", 64),
		Review:             deploymentProvenanceTestReview("apps"),
	}
	h := New(Config{})
	h.normalizeDeploymentFn = func(_ context.Context, desired, _ *appsv1.Deployment) (*appsv1.Deployment, error) {
		return desired, nil
	}

	liveBase := deploymentProvenanceTestObject("loom-mills", "loom-mills-operator", "operator:v1")
	id := deploymentProvenanceTestIdentity(t, liveBase)
	bound, err := h.bindReviewedDeployment(t.Context(), id, liveBase, review)
	if err != nil || bound.SpecSHA256 != bound.ReviewedSpecSHA256 {
		t.Fatalf("matching reviewed Deployment rejected: identity=%+v error=%v", bound, err)
	}

	mutations := map[string]func(*appsv1.Deployment){
		"command": func(deployment *appsv1.Deployment) {
			deployment.Spec.Template.Spec.Containers[0].Command = []string{"/bin/attacker"}
		},
		"environment": func(deployment *appsv1.Deployment) {
			deployment.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "UNREVIEWED", Value: "true"}}
		},
		"service account": func(deployment *appsv1.Deployment) {
			deployment.Spec.Template.Spec.ServiceAccountName = "unreviewed-admin"
		},
		"volume": func(deployment *appsv1.Deployment) {
			deployment.Spec.Template.Spec.Volumes = []corev1.Volume{{
				Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}},
			}}
		},
		"init container": func(deployment *appsv1.Deployment) {
			deployment.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "unreviewed", Image: "attacker:v1"}}
		},
		"security context": func(deployment *appsv1.Deployment) {
			privileged := true
			deployment.Spec.Template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: &privileged}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			live := liveBase.DeepCopy()
			mutate(live)
			identity := deploymentProvenanceTestIdentity(t, live)
			if _, err := h.bindReviewedDeployment(t.Context(), identity, live, review); err == nil ||
				!strings.Contains(err.Error(), "live spec does not match") {
				t.Fatalf("unreviewed %s drift accepted: %v", name, err)
			}
		})
	}
}

func TestValidateDeploymentProvenanceRejectsIdentityAndReviewDrift(t *testing.T) {
	operator := PodIdentity{Name: "operator", UID: "operator-pod", Image: "operator:v1", ImageID: "operator@sha256:a", StartedAt: testTime}
	hud := PodIdentity{Name: "hud", UID: "hud-pod", Image: "hud:v1", ImageID: "hud@sha256:b", StartedAt: testTime}
	report := canonicalTestPreflight(testTime, "", operator, hud)
	if err := ValidateDeploymentProvenance(report); err != nil {
		t.Fatalf("valid Deployment provenance rejected: %v", err)
	}
	replaced := report.OperatorDeployment
	replaced.UID = "replacement"
	if err := sameDeploymentGateIdentity(report.OperatorDeployment, replaced); err == nil {
		t.Fatal("Deployment UID replacement accepted across the gate")
	}
	tests := map[string]func(*PreflightReport){
		"live spec": func(report *PreflightReport) { report.OperatorDeployment.SpecSHA256 = strings.Repeat("1", 64) },
		"reviewed baseline": func(report *PreflightReport) {
			report.OperatorDeployment.Review.PlatformRevision = strings.Repeat("2", 40)
		},
		"source baseline": func(report *PreflightReport) { report.HudDeployment.Review.SourceScopeDigest = strings.Repeat("3", 64) },
		"Flux transform":  func(report *PreflightReport) { report.HudDeployment.Review.FluxSpecSHA256 = strings.Repeat("4", 64) },
		"renderer":        func(report *PreflightReport) { report.HudDeployment.Review.RendererVersion = "flux: changed" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			changed := report
			mutate(&changed)
			if err := ValidateDeploymentProvenance(changed); err == nil {
				t.Fatalf("%s drift accepted", name)
			}
		})
	}
}

var testTime = metav1.Now().Time

func TestReviewedDeploymentRenderCacheUsesExactBaselinesAndDeepCopies(t *testing.T) {
	platformRepo, platformRevision := newGitOpsScopeRepo(t, true)
	loomCoreRepo, loomCoreRevision := newLoomCoreScopeRepo(t)
	identity := func(revision, contract string) GitOpsScopeIdentity {
		return GitOpsScopeIdentity{
			Mode: GitOpsIdentityModeExactRevision, Contract: contract, ContractVersion: 1,
			BaselineRevision: revision, ObservedRevision: revision,
			BaselineDigest: revision, ObservedDigest: revision, CheckedCommitCount: 1,
		}
	}
	snapshot := fluxSourceSnapshot{
		platform: fluxSourceState{name: "apps", identity: identity(platformRevision, platformGitOpsScopeV1.name),
			renderSpec: FluxRenderSpecIdentity{SpecSHA256: strings.Repeat("a", 64)}},
		loomCore: fluxSourceState{name: "loom-hub-servers", identity: identity(loomCoreRevision, loomCoreSourceScopeV1.name),
			renderSpec: FluxRenderSpecIdentity{SpecSHA256: strings.Repeat("b", 64)}},
	}
	h := New(Config{GitOpsRepoPath: platformRepo, LoomCoreRepoPath: loomCoreRepo, FluxBin: "flux-test"})
	var builds, versions atomic.Int32
	h.fluxVersionFn = func(context.Context) (string, error) {
		versions.Add(1)
		return "flux: v-test", nil
	}
	h.fluxBuildFn = func(_ context.Context, _, name, _, _ string) ([]byte, error) {
		builds.Add(1)
		namespace, deploymentName, image := "loom-mills", "loom-mills-operator", "operator:v1"
		annotation := ""
		if name == "loom-hub-servers" {
			namespace, deploymentName, image = "loom-hub", "mobile-hud", "hud:v1"
		} else {
			policySource, readErr := os.ReadFile(filepath.Join(platformRepo, filepath.FromSlash(policyConfigMapSourcePath)))
			if readErr != nil {
				return nil, readErr
			}
			annotation = "      annotations:\n        " + policyChecksumAnnotation + ": " + exactPolicySourceSHA256(policySource) + "\n"
		}
		rendered := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: " + deploymentName +
			"\n  namespace: " + namespace + "\nspec:\n  replicas: 1\n  selector:\n    matchLabels:\n      app: " + deploymentName +
			"\n  strategy:\n    type: Recreate\n  template:\n    metadata:\n      labels:\n        app: " + deploymentName +
			"\n" + annotation + "    spec:\n      containers:\n        - name: main\n          image: " + image + "\n"
		if name == policyConfigMapFluxOwner {
			rendered += "---\n" + renderedPolicyConfigMapFixture
		}
		return []byte(rendered), nil
	}

	first, err := h.reviewedDeployments(t.Context(), snapshot)
	if err != nil {
		t.Fatalf("first reviewed render: %v", err)
	}
	first[operatorDeploymentKey].Deployment.Spec.Replicas = int32Ptr(9)
	first[operatorDeploymentKey].PolicyConfigMap.ConfigMap.Data["unreviewed"] = "caller-mutation"
	second, err := h.reviewedDeployments(t.Context(), snapshot)
	if err != nil {
		t.Fatalf("cached reviewed render: %v", err)
	}
	if builds.Load() != 2 || versions.Load() != 2 {
		t.Fatalf("build/version calls = %d/%d, want two initial renders and a cheap version recheck", builds.Load(), versions.Load())
	}
	if got := *second[operatorDeploymentKey].Deployment.Spec.Replicas; got != 1 {
		t.Fatalf("cached Deployment was mutable through caller copy: replicas=%d", got)
	}
	if second[operatorDeploymentKey].PolicyConfigMap.ConfigMap == nil ||
		!isNormalizedSHA256(second[operatorDeploymentKey].PolicyConfigMap.RenderedPayloadSHA256) {
		t.Fatalf("cached operator review omitted policy ConfigMap: %+v", second[operatorDeploymentKey].PolicyConfigMap)
	}
	if _, ok := second[operatorDeploymentKey].PolicyConfigMap.ConfigMap.Data["unreviewed"]; ok {
		t.Fatal("cached policy ConfigMap was mutable through caller copy")
	}
}

func int32Ptr(value int32) *int32 { return &value }

func TestReviewedDeploymentRenderingFailsClosed(t *testing.T) {
	platformRepo, platformRevision := newGitOpsScopeRepo(t, true)
	loomCoreRepo, loomCoreRevision := newLoomCoreScopeRepo(t)
	identity := func(revision, contract string) GitOpsScopeIdentity {
		return GitOpsScopeIdentity{
			Mode: GitOpsIdentityModeExactRevision, Contract: contract, ContractVersion: 1,
			BaselineRevision: revision, ObservedRevision: revision,
			BaselineDigest: revision, ObservedDigest: revision, CheckedCommitCount: 1,
		}
	}
	snapshot := fluxSourceSnapshot{
		platform: fluxSourceState{identity: identity(platformRevision, platformGitOpsScopeV1.name), renderSpec: FluxRenderSpecIdentity{SpecSHA256: strings.Repeat("a", 64)}},
		loomCore: fluxSourceState{identity: identity(loomCoreRevision, loomCoreSourceScopeV1.name), renderSpec: FluxRenderSpecIdentity{SpecSHA256: strings.Repeat("b", 64)}},
	}
	h := New(Config{GitOpsRepoPath: platformRepo, LoomCoreRepoPath: loomCoreRepo})
	h.fluxVersionFn = func(context.Context) (string, error) { return "flux: v-test", nil }
	h.fluxBuildFn = func(context.Context, string, string, string, string) ([]byte, error) {
		return nil, errors.New("renderer crashed")
	}
	if _, err := h.reviewedDeployments(t.Context(), snapshot); err == nil || !strings.Contains(err.Error(), "renderer crashed") {
		t.Fatalf("renderer failure accepted: %v", err)
	}

	manifest := filepath.Join(t.TempDir(), "kustomization.yaml")
	if err := os.WriteFile(manifest, []byte("spec:\n  postBuild:\n    substituteFrom:\n      - kind: Secret\n        name: dynamic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectDynamicFluxSubstitution(manifest); err == nil || !strings.Contains(err.Error(), "substituteFrom") {
		t.Fatalf("dynamic substitution accepted: %v", err)
	}
}

func TestSelectRenderedDeploymentRejectsDuplicateUnknownAndRuntimeState(t *testing.T) {
	valid := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: mobile-hud\n  namespace: loom-hub\nspec:\n  replicas: 1\n  selector:\n    matchLabels: {app: hud}\n  strategy: {type: Recreate}\n  template:\n    metadata:\n      labels: {app: hud}\n    spec:\n      containers:\n        - {name: main, image: hud:v1}\n"
	deployment, digest, err := selectRenderedDeployment([]byte(valid), "loom-hub", "mobile-hud")
	if err != nil || deployment.Name != "mobile-hud" || !isNormalizedSHA256(digest) {
		t.Fatalf("valid rendered Deployment = %+v digest=%q error=%v", deployment, digest, err)
	}
	for name, raw := range map[string]string{
		"duplicate":        valid + "---\n" + valid,
		"unknown field":    strings.Replace(valid, "  replicas: 1", "  replicas: 1\n  attackerField: true", 1),
		"runtime identity": strings.Replace(valid, "  namespace: loom-hub", "  namespace: loom-hub\n  uid: live-uid", 1),
		"status":           valid + "status:\n  replicas: 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := selectRenderedDeployment([]byte(raw), "loom-hub", "mobile-hud"); err == nil {
				t.Fatalf("invalid rendered Deployment accepted")
			}
		})
	}
}

func TestParseStableDeploymentRejectsDeletionTimestamp(t *testing.T) {
	object := deploymentProvenanceTestObject("loom-hub", "mobile-hud", "hud:v1")
	deleted := metav1.Now()
	object.DeletionTimestamp = &deleted
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseStableDeployment(string(raw)); err == nil || !strings.Contains(err.Error(), "stable singleton") {
		t.Fatalf("terminating Deployment accepted: %v", err)
	}
}

func TestDeploymentSpecHashCoversProbeAndVolumeDetails(t *testing.T) {
	left := deploymentProvenanceTestObject("loom-hub", "mobile-hud", "hud:v1")
	right := left.DeepCopy()
	right.Spec.Template.Spec.Containers[0].StartupProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(3333)}},
	}
	leftDigest, _ := canonicalDeploymentSpecSHA256(left.Spec)
	rightDigest, _ := canonicalDeploymentSpecSHA256(right.Spec)
	if leftDigest == rightDigest {
		t.Fatal("probe mutation did not change full Deployment spec hash")
	}
}
