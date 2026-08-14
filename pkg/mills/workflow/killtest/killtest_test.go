package killtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/worker"
	"github.com/crb2nu/loom/pkg/mills/workflow"
)

func passingFluxFenceEvidence() FluxSourceFenceEvidence {
	preparedAt := time.Date(2026, time.July, 12, 11, 59, 50, 0, time.UTC)
	return passingFluxFenceEvidenceAt(preparedAt, preparedAt.Add(time.Second))
}

func passingFluxFenceEvidenceAt(preparedAt, finalAt time.Time) FluxSourceFenceEvidence {
	platformRevision := strings.Repeat("a", 40)
	loomCoreRevision := strings.Repeat("b", 40)
	platformIdentity := GitOpsScopeIdentity{
		Mode: GitOpsIdentityModeExactRevision, Contract: platformGitOpsScopeV1.name,
		ContractVersion: platformGitOpsScopeV1.version, BaselineRevision: platformRevision,
		ObservedRevision: platformRevision, BaselineDigest: platformRevision,
		ObservedDigest: platformRevision, CheckedCommitCount: 1,
	}
	loomCoreIdentity := GitOpsScopeIdentity{
		Mode: GitOpsIdentityModeExactRevision, Contract: loomCoreSourceScopeV1.name,
		ContractVersion: loomCoreSourceScopeV1.version, BaselineRevision: loomCoreRevision,
		ObservedRevision: loomCoreRevision, BaselineDigest: loomCoreRevision,
		ObservedDigest: loomCoreRevision, CheckedCommitCount: 1,
	}
	snapshot := func(listResourceVersion string, observedAt time.Time) FluxSourceProvenanceSnapshot {
		platformApplied := "main@sha1:" + platformRevision
		loomCoreApplied := "main@sha1:" + loomCoreRevision
		return FluxSourceProvenanceSnapshot{
			Contract: FluxProvenanceContract, ContractVersion: FluxProvenanceContractVersion,
			ListResourceVersion:     listResourceVersion,
			GitRepositoriesOpenedAt: observedAt.Add(-time.Nanosecond),
			ObservedAt:              observedAt,
			GitRepositories: testGitRepositoryProvenanceSnapshot(
				"repos-"+listResourceVersion, observedAt,
				platformApplied, loomCoreApplied, platformIdentity, loomCoreIdentity,
			),
			Sources: []FluxSourceProvenance{
				{Name: "apps", UID: "apps-uid", ResourceVersion: "101", Generation: 7,
					ReadyObservedGeneration: 7, ReadyStatus: "True", AppliedRevision: platformApplied,
					AttemptedRevision: platformApplied, RenderSpec: testFluxRenderSpec("apps", platformIdentity),
					ProtectedIdentity: platformIdentity},
				{Name: "bootstrap", UID: "bootstrap-uid", ResourceVersion: "151", Generation: 5,
					ReadyObservedGeneration: 5, ReadyStatus: "True", AppliedRevision: platformApplied,
					AttemptedRevision: platformApplied, RenderSpec: testFluxRenderSpec("bootstrap", platformIdentity),
					ProtectedIdentity: platformIdentity},
				{Name: "system", UID: "system-uid", ResourceVersion: "181", Generation: 9,
					ReadyObservedGeneration: 9, ReadyStatus: "True", AppliedRevision: platformApplied,
					AttemptedRevision: platformApplied, RenderSpec: testFluxRenderSpec("system", platformIdentity),
					ProtectedIdentity: platformIdentity},
				{Name: "loom-hub-servers", UID: "loom-core-uid", ResourceVersion: "202", Generation: 11,
					ReadyObservedGeneration: 11, ReadyStatus: "True", AppliedRevision: loomCoreApplied,
					AttemptedRevision: loomCoreApplied, RenderSpec: testFluxRenderSpec("loom-hub-servers", platformIdentity),
					ProtectedIdentity: loomCoreIdentity},
			},
		}
	}
	return FluxSourceFenceEvidence{
		Prepared: snapshot("9001", preparedAt),
		Final:    snapshot("9002", finalAt),
	}
}

func setTestFluxSnapshotObservedAt(snapshot *FluxSourceProvenanceSnapshot, observedAt time.Time) {
	snapshot.GitRepositoriesOpenedAt = observedAt.Add(-time.Nanosecond)
	snapshot.ObservedAt = observedAt
	snapshot.GitRepositories.ObservedAt = observedAt
}

func testFluxSpecJSON(name string) string {
	switch name {
	case "apps":
		return `{"path":"./k3s/flux/apps","sourceRef":{"kind":"GitRepository","name":"gitops-gitlab"},"force":false}`
	case "bootstrap":
		return `{"path":"./k3s/flux/bootstrap","sourceRef":{"kind":"GitRepository","name":"gitops-gitlab"},"force":false}`
	case "system":
		return `{"path":"./k3s/flux/system","sourceRef":{"kind":"GitRepository","name":"gitops-gitlab"},"force":false}`
	case "loom-hub-servers":
		return `{"path":"./k8s/base","targetNamespace":"loom-hub","sourceRef":{"kind":"GitRepository","name":"loom-core"},"force":false}`
	default:
		panic("unknown test Flux source " + name)
	}
}

func testGitRepositorySpecJSON(name string) string {
	switch name {
	case "gitops-gitlab":
		return `{"interval":"1m","timeout":"120s","url":"http://gitlab-vm.gitlab.svc.cluster.local/platform/gitops.git","ref":{"branch":"main"},"secretRef":{"name":"gitops-gitlab"}}`
	case "loom-core":
		return `{"interval":"1m","timeout":"120s","url":"http://gitlab-vm.gitlab.svc.cluster.local/services/loom-core.git","ref":{"branch":"main"},"secretRef":{"name":"gitops-gitlab"}}`
	default:
		panic("unknown test GitRepository " + name)
	}
}

func testGitRepositorySpec(name string, platformIdentity GitOpsScopeIdentity) GitRepositorySpecIdentity {
	got, err := parseGitRepositorySpecIdentity(name, json.RawMessage(testGitRepositorySpecJSON(name)))
	if err != nil {
		panic(err)
	}
	got.ReviewedSpecSHA256 = got.SpecSHA256
	got.ReviewedRevision = platformIdentity.BaselineRevision
	got.ReviewedScopeDigest = platformIdentity.BaselineDigest
	return got
}

func testGitRepositoryProvenanceSnapshot(
	listResourceVersion string,
	observedAt time.Time,
	platformRevision, loomCoreRevision string,
	platformIdentity, loomCoreIdentity GitOpsScopeIdentity,
) GitRepositoryProvenanceSnapshot {
	repository := func(
		name, uid, resourceVersion, revision, digest string,
		generation int64,
		identity GitOpsScopeIdentity,
	) GitRepositoryProvenance {
		return GitRepositoryProvenance{
			Name: name, Namespace: "flux-system", UID: uid,
			ResourceVersion: resourceVersion, Generation: generation,
			StatusObservedGeneration: generation,
			ReadyObservedGeneration:  generation, ReadyStatus: "True",
			ArtifactInStorageObservedGeneration: generation, ArtifactInStorageStatus: "True",
			ArtifactRevision: revision, ArtifactDigest: digest,
			Spec: testGitRepositorySpec(name, platformIdentity), ProtectedIdentity: identity,
		}
	}
	return GitRepositoryProvenanceSnapshot{
		ListResourceVersion: listResourceVersion,
		ObservedAt:          observedAt,
		Repositories: []GitRepositoryProvenance{
			repository("gitops-gitlab", "gitops-repo-uid", "501", platformRevision,
				"sha256:"+strings.Repeat("c", 64), 7, platformIdentity),
			repository("loom-core", "loom-core-repo-uid", "502", loomCoreRevision,
				"sha256:"+strings.Repeat("d", 64), 3, loomCoreIdentity),
		},
	}
}

func testFluxRenderSpec(name string, platformIdentity GitOpsScopeIdentity) FluxRenderSpecIdentity {
	got, err := parseFluxRenderSpecIdentity(name, json.RawMessage(testFluxSpecJSON(name)))
	if err != nil {
		panic(err)
	}
	got.ReviewedSpecSHA256 = got.SpecSHA256
	got.ReviewedRevision = platformIdentity.BaselineRevision
	got.ReviewedScopeDigest = platformIdentity.BaselineDigest
	return got
}

func testReviewedFluxSpecDigests() map[string]string {
	digests := make(map[string]string, len(requiredFluxProvenanceOwners))
	for _, name := range requiredFluxProvenanceOwners {
		render, err := parseFluxRenderSpecIdentity(name, json.RawMessage(testFluxSpecJSON(name)))
		if err != nil {
			panic(err)
		}
		digests[name] = render.SpecSHA256
	}
	return digests
}

func testReviewedGitRepositorySpecDigests() map[string]string {
	digests := make(map[string]string, len(requiredGitRepositoryNames))
	for _, name := range requiredGitRepositoryNames {
		spec, err := parseGitRepositorySpecIdentity(name, json.RawMessage(testGitRepositorySpecJSON(name)))
		if err != nil {
			panic(err)
		}
		digests[name] = spec.SpecSHA256
	}
	return digests
}

func configureTestReviewedFluxSpecs(h *Harness) {
	h.reviewedFluxRenderSpecsFn = func(context.Context, string, string) (map[string]string, error) {
		return testReviewedFluxSpecDigests(), nil
	}
	h.reviewedGitRepositorySpecsFn = func(context.Context, string, string) (map[string]string, error) {
		return testReviewedGitRepositorySpecDigests(), nil
	}
}

func bindTestDeploymentProvenance(report *PreflightReport) {
	sources := fluxProvenanceByName(report.FluxSourcesEnd)
	platform := sources["apps"].ProtectedIdentity
	bind := func(
		deployment *DeploymentIdentity,
		namespace, uid, resourceVersion, owner, digest string,
		source GitOpsScopeIdentity,
	) {
		deployment.Namespace = namespace
		deployment.UID = uid
		deployment.ResourceVersion = resourceVersion
		deployment.ContainerName = "controller"
		deployment.SpecSHA256 = digest
		deployment.ReviewedSpecSHA256 = digest
		deployment.PodTemplateSHA256 = strings.Repeat("6", 64)
		deployment.ReviewedPodTemplateSHA256 = deployment.PodTemplateSHA256
		deployment.SelectorSHA256 = strings.Repeat("5", 64)
		deployment.ReviewedSelectorSHA256 = deployment.SelectorSHA256
		deployment.Review = DeploymentReviewIdentity{
			Contract: DeploymentProvenanceContract, ContractVersion: DeploymentProvenanceContractVersion,
			FluxOwner: owner, FluxSpecSHA256: sources[owner].RenderSpec.SpecSHA256,
			Renderer: "flux build kustomization --dry-run", RendererVersion: "flux: v-test",
			RenderedSpecSHA256: strings.Repeat("9", 64),
			PlatformRevision:   platform.BaselineRevision, PlatformScopeDigest: platform.BaselineDigest,
			SourceRevision: source.BaselineRevision, SourceScopeDigest: source.BaselineDigest,
		}
	}
	bind(&report.OperatorDeployment, "loom-mills", "operator-deployment-uid", "701", "apps",
		strings.Repeat("e", 64), platform)
	bind(&report.HudDeployment, "loom-hub", "hud-deployment-uid", "801", "loom-hub-servers",
		strings.Repeat("f", 64), sources["loom-hub-servers"].ProtectedIdentity)
	policyChecksum := strings.Repeat("8", 64)
	report.PolicyChecksum = policyChecksum
	report.OperatorDeployment.PolicyChecksum = policyChecksum
	report.PolicyConfigMapReview = PolicyConfigMapReviewIdentity{
		Contract: PolicyConfigMapProvenanceContract, ContractVersion: PolicyConfigMapProvenanceContractVersion,
		Name: policyConfigMapName, Namespace: s1cOperatorNamespace,
		FluxOwner: policyConfigMapFluxOwner, FluxSpecSHA256: sources["apps"].RenderSpec.SpecSHA256,
		Renderer: policyConfigMapRenderer, RendererVersion: report.OperatorDeployment.Review.RendererVersion,
		PlatformRevision: platform.BaselineRevision, PlatformScopeDigest: platform.BaselineDigest,
		SourcePath: policyConfigMapSourcePath, SourceSHA256: policyChecksum,
		RenderedPayloadSHA256: strings.Repeat("7", 64), LivePayloadSHA256: strings.Repeat("7", 64),
	}
}

func testAuthorityPlane(pod PodIdentity, deployment DeploymentIdentity) AuthorityPlaneEvidence {
	operator := OperatorResponseAuthority{
		Contract: OperatorAuthorityContract, ContractVersion: OperatorAuthorityContractVersion,
		PodName: pod.Name, PodNamespace: pod.Namespace, PodUID: pod.UID,
		DeploymentName: deployment.Name, BootID: strings.Repeat("e", 64),
	}
	return AuthorityPlaneEvidence{
		Contract: AuthorityPlaneContract, ContractVersion: AuthorityPlaneContractVersion,
		Kubernetes: KubernetesClusterAuthority{
			Contract: AuthorityPlaneContract, ContractVersion: AuthorityPlaneContractVersion,
			PublicAuthoritySHA256: strings.Repeat("a", 64), APIServerSHA256: strings.Repeat("b", 64),
			CertificateAuthoritySHA256: strings.Repeat("c", 64), ContextName: "test-context",
			OperatorNamespaceIdentity: KubernetesObjectIdentity{
				Name: s1cOperatorNamespace, UID: "operator-namespace-uid", ResourceVersion: "10",
			},
		},
		Operator: operator, OperatorDeploymentUID: deployment.UID,
	}
}

func bindTestAuthority(report *PreflightReport) {
	report.AuthorityPlane = testAuthorityPlane(report.Operator, report.OperatorDeployment)
	report.EffectivePolicyAuthority = report.AuthorityPlane.Operator
	report.Quiescence.OperatorAuthority = report.AuthorityPlane.Operator
}

func canonicalTestQuiescence(observedAt time.Time, runID string, crashLease bool) QuiescenceSnapshot {
	workflowActive := int64(0)
	workflowRuns := 0
	runIDs := []string(nil)
	if runID != "" {
		workflowActive = 1
		workflowRuns = 1
		runIDs = []string{runID}
	}
	return QuiescenceSnapshot{
		ObservedAt: observedAt,
		Quiescent:  runID == "" && !crashLease,
		Counts: QuiescenceCounts{
			ActiveWorkflowRuns: workflowRuns,
		},
		InMemory: QuiescenceInMemoryActivity{
			AdmissionClosed: true, CrashLeaseActive: crashLease,
			PolicyGeneration: 1, SourcesReady: true, SampleStable: true,
			WiringRequired: true, ActivitySources: 6, SourceGeneration: 1,
			SourceOperations: map[string]int64{
				"reconciler": 0, "pipeline": 0, "cross_run": 0,
				"council": 0, "canary": 0, "workflow": workflowActive,
			},
			SourceRunIDs:         map[string][]string{"workflow": runIDs},
			BackgroundOperations: workflowActive,
		},
	}
}

func canonicalTestPreflight(
	observedAt time.Time,
	runID string,
	operator, hud PodIdentity,
) PreflightReport {
	flux := passingFluxFenceEvidenceAt(observedAt.Add(-100*time.Millisecond), observedAt)
	start, end := flux.Prepared, flux.Final
	startSources := fluxProvenanceByName(start)
	endSources := fluxProvenanceByName(end)
	operatorDeployment := DeploymentIdentity{
		Name: "loom-mills-operator", Generation: 7, ObservedGeneration: 7,
		DesiredReplicas: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
		Image: operator.Image, Strategy: "Recreate", PolicyChecksum: "policy-sha256",
	}
	hudDeployment := DeploymentIdentity{
		Name: "mobile-hud", Generation: 9, ObservedGeneration: 9,
		DesiredReplicas: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
		Image: hud.Image, Strategy: "Recreate",
	}
	report := PreflightReport{
		FluxSourcesStart: start, FluxSourcesEnd: end,
		NamespacesOK:  true,
		OperatorImage: operator.Image, Operator: operator, OperatorDeployment: operatorDeployment,
		HudImage: hud.Image, Hud: hud, HudDeployment: hudDeployment,
		GitOpsStartRevision: startSources["apps"].AppliedRevision,
		GitOpsStartIdentity: startSources["apps"].ProtectedIdentity,
		GitOpsRevision:      endSources["apps"].AppliedRevision, GitOpsAttempted: endSources["apps"].AttemptedRevision,
		GitOpsReady: true, GitOpsIdentity: endSources["apps"].ProtectedIdentity,
		GitOpsBootstrapStartRevision: startSources["bootstrap"].AppliedRevision,
		GitOpsBootstrapStartIdentity: startSources["bootstrap"].ProtectedIdentity,
		GitOpsBootstrapRevision:      endSources["bootstrap"].AppliedRevision,
		GitOpsBootstrapAttempted:     endSources["bootstrap"].AttemptedRevision,
		GitOpsBootstrapReady:         true, GitOpsBootstrapIdentity: endSources["bootstrap"].ProtectedIdentity,
		GitOpsSystemStartRevision: startSources["system"].AppliedRevision,
		GitOpsSystemStartIdentity: startSources["system"].ProtectedIdentity,
		GitOpsSystemRevision:      endSources["system"].AppliedRevision,
		GitOpsSystemAttempted:     endSources["system"].AttemptedRevision,
		GitOpsSystemReady:         true, GitOpsSystemIdentity: endSources["system"].ProtectedIdentity,
		LoomCoreStartRevision: startSources["loom-hub-servers"].AppliedRevision,
		LoomCoreStartIdentity: startSources["loom-hub-servers"].ProtectedIdentity,
		LoomCoreRevision:      endSources["loom-hub-servers"].AppliedRevision,
		LoomCoreAttempted:     endSources["loom-hub-servers"].AttemptedRevision,
		LoomCoreReady:         true, LoomCoreIdentity: endSources["loom-hub-servers"].ProtectedIdentity,
		PolicyChecksum: "policy-sha256",
		PolicyConfigMapIdentity: KubernetesObjectIdentity{
			Name: policyConfigMapName, Namespace: s1cOperatorNamespace, UID: "policy-config-uid", ResourceVersion: "40",
		},
		WorkflowsFlag:          "true",
		ConfigMapPolicyEnabled: false, FlagEnabled: true, SubstrateK8sOnly: true,
		EffectivePolicyEnabled: false, EffectiveFlagEnabled: true,
		EffectiveSubstrateK8sOnly: true, EffectivePolicyMatchesConfigMap: true,
		SpawnConfigMap: true, SpawnConfigMapUID: "spawn-state-uid",
		SpawnConfigMapIdentity: KubernetesObjectIdentity{
			Name: spawnStateConfigMapName, Namespace: s1cSpawnNamespace, UID: "spawn-state-uid", ResourceVersion: "50",
		},
		SpawnConfigMapUpdateAllowed: true,
		Quiescence:                  canonicalTestQuiescence(observedAt.Add(-50*time.Millisecond), runID, false),
		LokiReady:                   true, AllPreconditions: true,
	}
	bindTestDeploymentProvenance(&report)
	report.Operator = bindControllerPodFixture(report.Operator, report.OperatorDeployment)
	report.Hud = bindControllerPodFixture(report.Hud, report.HudDeployment)
	bindTestAuthority(&report)
	return report
}

func canonicalTestCrashSafety(
	runID, agentType string,
	step StepView,
	identity SpawnIdentity,
	immediate PreflightReport,
	acquiredAt, renewedAt, targetAt, deleteAt time.Time,
) CrashSafetyEvidence {
	requestID := "s1c-lease-" + deleteAt.Format("150405.000000000")
	evidence := CrashSafetyEvidence{
		ImmediatePreflight: immediate,
		Target: CrashTargetSafetyEvidence{
			Quiescence:            canonicalTestQuiescence(targetAt.Add(-10*time.Millisecond), runID, true),
			QuiescenceCollectedAt: targetAt.Add(-5 * time.Millisecond),
			Run: RunSummary{
				ID: runID, Engine: "imperative", Template: workflow.CanaryTemplateName,
				AgentType: agentType, TemplateVersion: workflow.CanaryTemplateVersion,
				InterpreterVersion: workflow.HostInterpreterVersion, State: "running", StepCount: 1,
			},
			RunAuthority: immediate.AuthorityPlane.Operator,
			AgentStep:    step, DerivedSpawn: identity,
			SpawnState: SpawnStateSnapshot{
				ConfigMapUID: immediate.SpawnConfigMapUID, ConfigMapIdentity: immediate.SpawnConfigMapIdentity,
				RecordIDs: []string{identity.SpawnID}, ActiveIDs: []string{identity.SpawnID},
				Statuses:        map[string]string{identity.SpawnID: "running"},
				IdempotencyKeys: map[string]string{identity.SpawnID: identity.IdempotencyKey},
			},
			ActiveSpawnPodNames: []string{identity.PodName},
			ExactSpawnPodActive: 1, ExactSpawnPodReady: 1,
			ExactSpawnPodNames: []string{identity.PodName}, ObservedAt: targetAt,
		},
		PolicyDeleteBoundary: testPolicyDeleteBoundaryEvidence(
			immediate,
			deleteAt.Add(-80*time.Millisecond), deleteAt.Add(-60*time.Millisecond),
			deleteAt.Add(-40*time.Millisecond), deleteAt.Add(-20*time.Millisecond),
			deleteAt.Add(-10*time.Millisecond),
		),
		LeaseAcquired: CrashLeaseEvidence{
			RequestID: requestID, RunID: runID, SpawnID: identity.SpawnID,
			ObservedAt: acquiredAt, ExpiresAt: deleteAt.Add(2 * time.Minute),
			OperatorAuthority: immediate.AuthorityPlane.Operator,
		},
		LeaseRenewed: CrashLeaseEvidence{
			RequestID: requestID, RunID: runID, SpawnID: identity.SpawnID,
			ObservedAt: renewedAt, ExpiresAt: deleteAt.Add(time.Minute),
			OperatorAuthority: immediate.AuthorityPlane.Operator,
		},
		DeleteIntentRecordedAt: acquiredAt.Add(10 * time.Millisecond),
		DeleteRequestedAt:      deleteAt,
		DeleteAcceptedAt:       deleteAt.Add(time.Millisecond),
	}
	evidence.Target.Quiescence.OperatorAuthority = immediate.AuthorityPlane.Operator
	return evidence
}

func testPolicyDeleteBoundaryEvidence(
	baseline PreflightReport,
	configMapAAt, effectiveAt, deploymentAt, configMapBAt, completedAt time.Time,
) PolicyDeleteBoundaryEvidence {
	snapshot := func(observedAt time.Time) PolicyConfigMapBoundarySnapshot {
		return PolicyConfigMapBoundarySnapshot{
			Identity:         baseline.PolicyConfigMapIdentity,
			PayloadSHA256:    baseline.PolicyConfigMapReview.LivePayloadSHA256,
			PolicyEnabled:    baseline.ConfigMapPolicyEnabled,
			WorkflowsEnabled: baseline.FlagEnabled,
			SubstrateK8sOnly: baseline.SubstrateK8sOnly,
			ObservedAt:       observedAt,
		}
	}
	deployment := baseline.OperatorDeployment
	deployment.ReviewedSpecSHA256 = ""
	deployment.ReviewedPodTemplateSHA256 = ""
	deployment.ReviewedSelectorSHA256 = ""
	deployment.Review = DeploymentReviewIdentity{}
	return PolicyDeleteBoundaryEvidence{
		Contract: PolicyDeleteBoundaryContract, ContractVersion: PolicyDeleteBoundaryContractVersion,
		ConfigMapA: snapshot(configMapAAt),
		Effective: EffectivePolicyBoundarySnapshot{
			PolicyEnabled:     baseline.EffectivePolicyEnabled,
			WorkflowsEnabled:  baseline.EffectiveFlagEnabled,
			SubstrateK8sOnly:  baseline.EffectiveSubstrateK8sOnly,
			ObservedAt:        effectiveAt,
			OperatorAuthority: baseline.AuthorityPlane.Operator,
		},
		OperatorDeployment: deployment, OperatorDeploymentObservedAt: deploymentAt,
		ConfigMapB: snapshot(configMapBAt), Review: baseline.PolicyConfigMapReview,
		CompletedAt: completedAt,
	}
}

// evidence fixture: the shape of a clean dual-crash pass.
func passingEvidence() Evidence {
	const (
		runID    = "wf-canary-1"
		stepKey  = "root/agent#0"
		callHash = "crash-critical-call-hash"
	)
	crashA := time.Date(2026, time.July, 12, 12, 0, 0, 0, time.UTC)
	crashB := crashA.Add(time.Second)
	expectedKey := workflow.DeriveStepIdempotencyKeyFromHash(runID, stepKey, callHash)
	spawnID := worker.DeriveSpawnID(expectedKey)
	podName := "spawn-" + spawnID
	dedupeLine := fmt.Sprintf(`msg="workflow resume: re-attaching to in-flight spawn" spawn_id=%s`, spawnID)
	operatorBefore := bindControllerPodFixture(PodIdentity{
		Name: "operator-old", UID: "operator-uid-old", Node: "worker-1",
		Image: "registry/operator:v1", ImageID: "registry/operator@sha256:aaa",
		StartedAt: crashA.Add(-time.Hour),
	}, DeploymentIdentity{
		Name: "loom-mills-operator", Namespace: "loom-mills", UID: "operator-deployment-uid",
		PodTemplateSHA256: strings.Repeat("6", 64), SelectorSHA256: strings.Repeat("5", 64),
	})
	operatorAfter := bindControllerPodFixture(PodIdentity{
		Name: "operator-new", UID: "operator-uid-new", Node: "worker-1",
		Image: operatorBefore.Image, ImageID: operatorBefore.ImageID,
		StartedAt: crashA.Add(100 * time.Millisecond),
	}, DeploymentIdentity{
		Name: "loom-mills-operator", Namespace: "loom-mills", UID: "operator-deployment-uid",
		PodTemplateSHA256: strings.Repeat("6", 64), SelectorSHA256: strings.Repeat("5", 64),
	})
	hudBefore := bindControllerPodFixture(PodIdentity{
		Name: "hud-old", UID: "hud-uid-old", Node: "worker-2",
		Image: "registry/mobile-hud:v1", ImageID: "registry/mobile-hud@sha256:bbb",
		StartedAt: crashA.Add(-time.Hour),
	}, DeploymentIdentity{
		Name: "mobile-hud", Namespace: "loom-hub", UID: "hud-deployment-uid",
		PodTemplateSHA256: strings.Repeat("6", 64), SelectorSHA256: strings.Repeat("5", 64),
	})
	hudAfter := bindControllerPodFixture(PodIdentity{
		Name: "hud-new", UID: "hud-uid-new", Node: "worker-2",
		Image: hudBefore.Image, ImageID: hudBefore.ImageID,
		StartedAt: crashB.Add(100 * time.Millisecond),
	}, DeploymentIdentity{
		Name: "mobile-hud", Namespace: "loom-hub", UID: "hud-deployment-uid",
		PodTemplateSHA256: strings.Repeat("6", 64), SelectorSHA256: strings.Repeat("5", 64),
	})
	identity := SpawnIdentity{SpawnID: spawnID, PodName: podName, IdempotencyKey: expectedKey}
	pendingStep := StepView{
		StepKey: stepKey, EventType: "spawn_requested", Status: "pending", CallHash: callHash,
	}
	initialPreflight := canonicalTestPreflight(crashA.Add(-5*time.Second), "", operatorBefore, hudBefore)
	crashAPreflight := canonicalTestPreflight(crashA.Add(-600*time.Millisecond), runID, operatorBefore, hudBefore)
	crashBPreflight := canonicalTestPreflight(crashA.Add(300*time.Millisecond), runID, operatorAfter, hudBefore)
	finalPreflight := canonicalTestPreflight(crashB.Add(5*time.Second), "", operatorAfter, hudAfter)
	crashAFence := passingFluxFenceEvidenceAt(crashA.Add(-500*time.Millisecond), crashA.Add(-100*time.Millisecond))
	crashBFence := passingFluxFenceEvidenceAt(crashA.Add(400*time.Millisecond), crashA.Add(800*time.Millisecond))
	return Evidence{
		RunID:                   runID,
		AgentType:               AgentTypeClaudeCode,
		AgentStepKey:            stepKey,
		SpawnID:                 spawnID,
		SpawnPodName:            podName,
		ExpectedIdempotencyKey:  expectedKey,
		MaxConcurrentSpawnPods:  1,
		TotalSpawnPodNames:      []string{podName},
		SpawnPodWatchStartedAt:  crashA.Add(-2 * time.Minute),
		SpawnPodWatchEndedAt:    crashB.Add(2 * time.Minute),
		SpawnPodWatchInitialRV:  "100",
		SpawnPodWatchContinuous: true,
		TotalSpawnPodIncarnations: []PodIdentity{{
			Name: podName, UID: "spawn-uid-1", Node: "node-1", Image: "spawn:v1",
			ImageID: "spawn@sha256:123", StartedAt: crashA.Add(-time.Minute),
		}},
		CanaryHoldInitial: CanaryHoldObservation{
			PodName: podName, PID: 42, StartTimeTicks: 4200,
			DriverPID: 41, DriverStartTimeTicks: 4100, Seconds: workflow.CanaryHoldSeconds,
			ObservedAt: crashA.Add(-10 * time.Second),
		},
		CanaryHoldBeforeCrashA: CanaryHoldObservation{
			PodName: podName, PID: 42, StartTimeTicks: 4200,
			DriverPID: 41, DriverStartTimeTicks: 4100, Seconds: workflow.CanaryHoldSeconds,
			ObservedAt: crashA.Add(-225 * time.Millisecond),
		},
		CanaryHoldBeforeCrashB: CanaryHoldObservation{
			PodName: podName, PID: 42, StartTimeTicks: 4200,
			DriverPID: 41, DriverStartTimeTicks: 4100, Seconds: workflow.CanaryHoldSeconds,
			ObservedAt: crashA.Add(700 * time.Millisecond),
		},
		ProcessObservationStartedAt: crashA.Add(-200 * time.Millisecond),
		PostCrashProcessSamples: []CanaryProcessSample{{
			PodName: podName, ObservedAt: crashA.Add(-190 * time.Millisecond),
			CompletedAt: crashA.Add(-180 * time.Millisecond),
			HoldPID:     42, HoldStartTimeTicks: 4200, HoldState: "S",
			DriverPID: 41, DriverStartTimeTicks: 4100, DriverState: "S",
			LiveHoldPIDs: []int{42}, LiveDriverPIDs: []int{41}, ZombiePIDs: []int{},
		}},
		PostCrashProcessObservedEnd: crashB.Add(time.Second),
		PostCrashProcessMaxGapMS:    ProcessEvidenceMaxSampleGap.Milliseconds(),
		CrashAProcessAuthorization: ProcessDeleteAuthorization{
			SampleIndex: 0, SampleObservedAt: crashA.Add(-190 * time.Millisecond),
			SampleCompletedAt: crashA.Add(-180 * time.Millisecond), AuthorizedAt: crashA,
		},
		CrashBProcessAuthorization: ProcessDeleteAuthorization{
			SampleIndex: 0, SampleObservedAt: crashA.Add(-190 * time.Millisecond),
			SampleCompletedAt: crashA.Add(-180 * time.Millisecond), AuthorizedAt: crashB,
		},
		TotalSpawnRecordIDs: []string{spawnID},
		FinalSpawnRecordStatuses: map[string]string{
			spawnID: "completed",
		},
		FinalSpawnIdempotencyKeys: map[string]string{spawnID: expectedKey},
		DedupeEvidence:            dedupeLine,
		DedupeLog: &LogEvidence{
			Component: "operator",
			Namespace: "loom-mills",
			Pod:       "operator-new",
			Timestamp: crashA.Add(time.Second),
			Line:      dedupeLine,
		},
		CrashAAt:             crashA,
		CrashBAt:             crashB,
		CrashAFluxProvenance: crashAFence,
		CrashBFluxProvenance: crashBFence,
		InitialPreflight:     initialPreflight,
		FinalPreflight:       finalPreflight,
		CrashASafety: canonicalTestCrashSafety(
			runID, AgentTypeClaudeCode, pendingStep, identity, crashAPreflight,
			crashA.Add(-450*time.Millisecond), crashA.Add(-350*time.Millisecond),
			crashA.Add(-250*time.Millisecond), crashA,
		),
		CrashBSafety: canonicalTestCrashSafety(
			runID, AgentTypeClaudeCode, pendingStep, identity, crashBPreflight,
			crashA.Add(450*time.Millisecond), crashA.Add(550*time.Millisecond),
			crashA.Add(650*time.Millisecond), crashB,
		),
		CrashABefore:      operatorBefore,
		CrashAReplacement: operatorAfter,
		CrashBBefore:      hudBefore,
		CrashBReplacement: hudAfter,
		Final: RunDetail{
			OperatorAuthority: finalPreflight.AuthorityPlane.Operator,
			Run: RunSummary{
				ID: runID, Engine: "imperative", State: "done", AgentType: AgentTypeClaudeCode,
				Template: workflow.CanaryTemplateName, TemplateVersion: workflow.CanaryTemplateVersion,
				InterpreterVersion: workflow.HostInterpreterVersion,
			},
			Steps: []StepView{
				{StepKey: stepKey, EventType: "spawn_result", Status: "success",
					SpawnID: spawnID, CallHash: callHash, CostSource: "real", CostUSD: 0.42, EffectCount: 1},
				{StepKey: "root/gate#0", EventType: "gate_eval", Status: "success"},
			},
		},
	}
}

func TestEvaluate_CleanPass(t *testing.T) {
	v := Evaluate(passingEvidence())
	if !v.Overall {
		t.Fatalf("expected overall PASS, got %+v", v)
	}
	for name, ok := range map[string]bool{
		"pass1": v.Pass1NoDoubleSpawn,
		"pass2": v.Pass2JournalOnce,
		"pass4": v.Pass4CostProvenance,
		"pass5": v.Pass5CounterExact,
	} {
		if !ok {
			t.Errorf("%s failed: %+v", name, v)
		}
	}
}

func TestEvidenceSerializesRequestedAndServerAgentIdentity(t *testing.T) {
	blob, err := json.Marshal(passingEvidence())
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	var wire struct {
		AgentType string `json:"agent_type"`
		Final     struct {
			Run struct {
				AgentType string `json:"agent_type"`
			} `json:"run"`
		} `json:"final"`
	}
	if err := json.Unmarshal(blob, &wire); err != nil {
		t.Fatalf("unmarshal evidence: %v", err)
	}
	if wire.AgentType != AgentTypeClaudeCode || wire.Final.Run.AgentType != AgentTypeClaudeCode {
		t.Fatalf("serialized agent identity = requested %q server %q", wire.AgentType, wire.Final.Run.AgentType)
	}
}

func TestValidateAgentTypeCanonicalOnly(t *testing.T) {
	for _, agentType := range []string{AgentTypeClaudeCode, AgentTypeCodex} {
		if err := ValidateAgentType(agentType); err != nil {
			t.Fatalf("ValidateAgentType(%q): %v", agentType, err)
		}
	}
	for _, agentType := range []string{"", "claude", "gemini", " codex ", "CODEX"} {
		if err := ValidateAgentType(agentType); err == nil {
			t.Fatalf("ValidateAgentType(%q) accepted non-canonical value", agentType)
		}
	}
}

func TestEvaluate_DoubleSpawnFails(t *testing.T) {
	ev := passingEvidence()
	ev.MaxConcurrentSpawnPods = 2
	ev.TotalSpawnPodNames = []string{"spawn-abc123", "spawn-def456"}
	v := Evaluate(ev)
	if v.Pass1NoDoubleSpawn || v.Overall {
		t.Fatalf("two concurrent pods must fail PASS-1: %+v", v)
	}
}

func TestEvaluateRequiresPodWatchToSpanFullProcessProof(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Evidence)
	}{
		{
			name: "watch starts after initial hold proof",
			mutate: func(ev *Evidence) {
				ev.SpawnPodWatchStartedAt = ev.CanaryHoldInitial.ObservedAt.Add(time.Nanosecond)
			},
		},
		{
			name: "watch ends before process observer",
			mutate: func(ev *Evidence) {
				ev.SpawnPodWatchEndedAt = ev.PostCrashProcessObservedEnd.Add(-time.Nanosecond)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := passingEvidence()
			tt.mutate(&ev)
			verdict := Evaluate(ev)
			if verdict.Pass1NoDoubleSpawn || verdict.Overall {
				t.Fatalf("truncated exact-pod watch passed: %+v", verdict)
			}
			if !strings.Contains(verdict.Pass1Reason, "watch did not span the full process proof") {
				t.Fatalf("failure reason = %q", verdict.Pass1Reason)
			}
		})
	}
}

func TestEvaluateRejectsPostCrashProcessViolations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Evidence)
	}{
		{name: "missing coverage", mutate: func(ev *Evidence) { ev.PostCrashProcessSamples = nil }},
		{name: "missing observation end", mutate: func(ev *Evidence) { ev.PostCrashProcessObservedEnd = time.Time{} }},
		{name: "missing maximum gap", mutate: func(ev *Evidence) { ev.PostCrashProcessMaxGapMS = 0 }},
		{name: "maximum gap exceeds contract", mutate: func(ev *Evidence) {
			ev.PostCrashProcessMaxGapMS = ProcessEvidenceMaxSampleGap.Milliseconds() + 1
		}},
		{name: "first sample exceeds maximum gap", mutate: func(ev *Evidence) { ev.PostCrashProcessMaxGapMS = 500 }},
		{name: "initial process sample is already missing", mutate: func(ev *Evidence) {
			ev.PostCrashProcessSamples[0].HoldState = "MISSING"
			ev.PostCrashProcessSamples[0].HoldStartTimeTicks = 0
			ev.PostCrashProcessSamples[0].LiveHoldPIDs = nil
			ev.PostCrashProcessSamples[0].DriverState = "MISSING"
			ev.PostCrashProcessSamples[0].DriverStartTimeTicks = 0
			ev.PostCrashProcessSamples[0].LiveDriverPIDs = nil
		}},
		{name: "sample predates crash B", mutate: func(ev *Evidence) {
			ev.PostCrashProcessSamples[0].ObservedAt = ev.CrashBAt.Add(-time.Nanosecond)
		}},
		{name: "observation end precedes sample", mutate: func(ev *Evidence) {
			ev.PostCrashProcessObservedEnd = ev.PostCrashProcessSamples[0].ObservedAt.Add(-time.Nanosecond)
		}},
		{name: "wrong pod", mutate: func(ev *Evidence) {
			ev.PostCrashProcessSamples[0].PodName = "spawn-other"
		}},
		{name: "zombie hold", mutate: func(ev *Evidence) {
			ev.PostCrashProcessSamples[0].HoldState = "Z"
		}},
		{name: "zombie driver", mutate: func(ev *Evidence) {
			ev.PostCrashProcessSamples[0].DriverState = "Z"
		}},
		{name: "unknown hold state", mutate: func(ev *Evidence) {
			ev.PostCrashProcessSamples[0].HoldState = "Q"
		}},
		{name: "empty driver state", mutate: func(ev *Evidence) {
			ev.PostCrashProcessSamples[0].DriverState = ""
		}},
		{name: "reused hold PID", mutate: func(ev *Evidence) {
			ev.PostCrashProcessSamples[0].HoldStartTimeTicks++
		}},
		{name: "reused driver PID", mutate: func(ev *Evidence) {
			ev.PostCrashProcessSamples[0].DriverStartTimeTicks++
		}},
		{name: "two live holds", mutate: func(ev *Evidence) {
			ev.PostCrashProcessSamples[0].LiveHoldPIDs = []int{42, 77}
		}},
		{name: "two live drivers", mutate: func(ev *Evidence) {
			ev.PostCrashProcessSamples[0].LiveDriverPIDs = []int{41, 76}
		}},
		{name: "replacement hold", mutate: func(ev *Evidence) {
			ev.PostCrashProcessSamples[0].LiveHoldPIDs = []int{77}
		}},
		{name: "replacement driver", mutate: func(ev *Evidence) {
			ev.PostCrashProcessSamples[0].LiveDriverPIDs = []int{76}
		}},
		{name: "live hold missing from inventory", mutate: func(ev *Evidence) {
			ev.PostCrashProcessSamples[0].LiveHoldPIDs = nil
		}},
		{name: "missing hold appears in inventory", mutate: func(ev *Evidence) {
			ev.PostCrashProcessSamples[0].HoldState = "MISSING"
		}},
		{name: "live driver missing from inventory", mutate: func(ev *Evidence) {
			ev.PostCrashProcessSamples[0].LiveDriverPIDs = nil
		}},
		{name: "missing driver appears in inventory", mutate: func(ev *Evidence) {
			ev.PostCrashProcessSamples[0].DriverState = "MISSING"
		}},
		{name: "wrong known hold identity", mutate: func(ev *Evidence) {
			ev.PostCrashProcessSamples[0].HoldPID = 77
		}},
		{name: "wrong known driver identity", mutate: func(ev *Evidence) {
			ev.PostCrashProcessSamples[0].DriverPID = 76
		}},
		{name: "process identities resurrect after disappearing", mutate: func(ev *Evidence) {
			live := ev.PostCrashProcessSamples[0]
			live.ObservedAt = ev.ProcessObservationStartedAt.Add(200 * time.Millisecond)
			missing := CanaryProcessSample{
				PodName: ev.SpawnPodName, ObservedAt: ev.ProcessObservationStartedAt.Add(100 * time.Millisecond),
				HoldPID: ev.CanaryHoldInitial.PID, HoldState: "MISSING",
				DriverPID: ev.CanaryHoldInitial.DriverPID, DriverState: "MISSING",
			}
			ev.PostCrashProcessSamples = []CanaryProcessSample{missing, live}
			ev.PostCrashProcessObservedEnd = ev.CrashBAt.Add(time.Second)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := passingEvidence()
			tt.mutate(&ev)
			verdict := Evaluate(ev)
			if verdict.Pass1NoDoubleSpawn || verdict.Overall {
				t.Fatalf("post-crash process violation passed: %+v", verdict)
			}
			if !strings.Contains(verdict.Pass1Reason, "crash-window") {
				t.Fatalf("failure reason does not identify post-crash evidence: %q", verdict.Pass1Reason)
			}
		})
	}
}

func TestEvaluateToleratesUnrelatedZombies(t *testing.T) {
	ev := passingEvidence()
	// Container-wide zombies — transient mid-reap children AND children
	// orphaned onto a reaperless pod PID 1 by the mobile-hud crash — are
	// recorded evidence, not verdict inputs. Only the exact hold/driver
	// identities going Z/X is fatal (covered by the violations table).
	ev.PostCrashProcessSamples[0].ZombiePIDs = []int{88}
	second := ev.PostCrashProcessSamples[0]
	second.ObservedAt = ev.CrashBAt.Add(100 * time.Millisecond)
	second.CompletedAt = ev.CrashBAt.Add(200 * time.Millisecond)
	second.ZombiePIDs = []int{88, 99}
	ev.PostCrashProcessSamples = append(ev.PostCrashProcessSamples, second)
	verdict := Evaluate(ev)
	if !verdict.Overall {
		t.Fatalf("unrelated zombies failed the gate: %+v", verdict)
	}
}

func TestEvaluate_RequiresOneObservedSpawnPodAndExactConcurrency(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Evidence)
	}{
		{
			name: "no observed pod",
			mutate: func(ev *Evidence) {
				ev.TotalSpawnPodNames = nil
			},
		},
		{
			name: "zero observed concurrency",
			mutate: func(ev *Evidence) {
				ev.MaxConcurrentSpawnPods = 0
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := passingEvidence()
			tt.mutate(&ev)
			if v := Evaluate(ev); v.Pass1NoDoubleSpawn || v.Overall {
				t.Fatalf("incomplete spawn observation must fail PASS-1: %+v", v)
			}
		})
	}
}

func TestEvaluate_RequiresSameOrderedCanaryHoldAcrossCrashes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Evidence)
	}{
		{"missing initial PID", func(ev *Evidence) { ev.CanaryHoldInitial.PID = 0 }},
		{"missing initial driver PID", func(ev *Evidence) { ev.CanaryHoldInitial.DriverPID = 0 }},
		{"missing initial hold start time", func(ev *Evidence) { ev.CanaryHoldInitial.StartTimeTicks = 0 }},
		{"missing initial driver start time", func(ev *Evidence) { ev.CanaryHoldInitial.DriverStartTimeTicks = 0 }},
		{"wrong hold duration", func(ev *Evidence) { ev.CanaryHoldInitial.Seconds++ }},
		{"PID changed before CRASH A", func(ev *Evidence) { ev.CanaryHoldBeforeCrashA.PID++ }},
		{"driver PID changed before CRASH B", func(ev *Evidence) { ev.CanaryHoldBeforeCrashB.DriverPID++ }},
		{"hold start time changed before CRASH A", func(ev *Evidence) { ev.CanaryHoldBeforeCrashA.StartTimeTicks++ }},
		{"driver start time changed before CRASH B", func(ev *Evidence) { ev.CanaryHoldBeforeCrashB.DriverStartTimeTicks++ }},
		{"pod changed before CRASH B", func(ev *Evidence) { ev.CanaryHoldBeforeCrashB.PodName = "spawn-other" }},
		{"initial proof after CRASH A proof", func(ev *Evidence) {
			ev.CanaryHoldInitial.ObservedAt = ev.CanaryHoldBeforeCrashA.ObservedAt.Add(time.Second)
		}},
		{"CRASH A proof after deletion", func(ev *Evidence) {
			ev.CanaryHoldBeforeCrashA.ObservedAt = ev.CrashAAt.Add(time.Second)
		}},
		{"CRASH B proof before CRASH A", func(ev *Evidence) {
			ev.CanaryHoldBeforeCrashB.ObservedAt = ev.CrashAAt.Add(-time.Second)
		}},
		{"CRASH B proof after deletion", func(ev *Evidence) {
			ev.CanaryHoldBeforeCrashB.ObservedAt = ev.CrashBAt.Add(time.Second)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := passingEvidence()
			tt.mutate(&ev)
			v := Evaluate(ev)
			if v.Pass1NoDoubleSpawn || v.Overall {
				t.Fatalf("invalid hold proof must fail PASS-1: %+v", v)
			}
			if !strings.Contains(v.Pass1Reason, "canary hold") {
				t.Fatalf("failure reason does not identify hold proof: %q", v.Pass1Reason)
			}
		})
	}
}

func TestEvaluate_SecondPodNameFails(t *testing.T) {
	ev := passingEvidence()
	// Never concurrent, but a second distinct pod name appeared over time —
	// the idempotency key drifted and a second logical spawn was created.
	ev.TotalSpawnPodNames = []string{"spawn-abc123", "spawn-zzz999"}
	v := Evaluate(ev)
	if v.Pass1NoDoubleSpawn {
		t.Fatalf("second pod name must fail PASS-1: %+v", v)
	}
}

func TestEvaluate_OnePodWithoutDedupeEvidenceFails(t *testing.T) {
	// Plan §3.4 mustChange: "one pod" alone is insufficient — a failed racing
	// Create also leaves one pod. The durable-path log line is required.
	ev := passingEvidence()
	ev.DedupeEvidence = ""
	v := Evaluate(ev)
	if v.Pass1NoDoubleSpawn {
		t.Fatalf("missing dedupe evidence must fail PASS-1: %+v", v)
	}
	if !strings.Contains(v.Pass1Reason, "durable-path") {
		t.Errorf("reason should explain the durable-path requirement: %q", v.Pass1Reason)
	}
}

func TestEvaluate_RequiresOrderedCrashTimestamps(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Evidence)
	}{
		{"missing CRASH A", func(ev *Evidence) { ev.CrashAAt = time.Time{} }},
		{"missing CRASH B", func(ev *Evidence) { ev.CrashBAt = time.Time{} }},
		{"equal timestamps", func(ev *Evidence) { ev.CrashBAt = ev.CrashAAt }},
		{"CRASH B before CRASH A", func(ev *Evidence) { ev.CrashBAt = ev.CrashAAt.Add(-time.Second) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := passingEvidence()
			tt.mutate(&ev)
			if v := Evaluate(ev); v.Pass1NoDoubleSpawn || v.Overall {
				t.Fatalf("invalid crash timestamps must fail PASS-1: %+v", v)
			}
		})
	}
}

func TestEvaluate_RequiresReplacementUIDAndImageID(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Evidence)
	}{
		{"CRASH A UID", func(ev *Evidence) { ev.CrashAReplacement.UID = "" }},
		{"CRASH A image ID", func(ev *Evidence) { ev.CrashAReplacement.ImageID = "" }},
		{"CRASH B UID", func(ev *Evidence) { ev.CrashBReplacement.UID = " " }},
		{"CRASH B image ID", func(ev *Evidence) { ev.CrashBReplacement.ImageID = "" }},
		{"CRASH A same UID", func(ev *Evidence) { ev.CrashAReplacement.UID = ev.CrashABefore.UID }},
		{"CRASH B digest drift", func(ev *Evidence) { ev.CrashBReplacement.ImageID = "registry/mobile-hud@sha256:changed" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := passingEvidence()
			tt.mutate(&ev)
			if v := Evaluate(ev); v.Pass1NoDoubleSpawn || v.Overall {
				t.Fatalf("missing replacement identity must fail PASS-1: %+v", v)
			}
		})
	}
}

func TestEvaluate_AcceptsExactHUDDedupeLog(t *testing.T) {
	ev := passingEvidence()
	ev.DedupeEvidence = fmt.Sprintf(`idempotent spawn re-attach (already exists) spawn_id=%s`, ev.SpawnID)
	ev.DedupeLog = &LogEvidence{
		Component: "mobile-hud",
		Namespace: "loom-hub",
		Pod:       ev.CrashBReplacement.Name,
		Timestamp: ev.CrashBAt,
		Line:      ev.DedupeEvidence,
	}
	if v := Evaluate(ev); !v.Pass1NoDoubleSpawn || !v.Overall {
		t.Fatalf("exact HUD replacement evidence at crash time must pass: %+v", v)
	}
}

func TestEvaluate_AcceptsHUDRestartAlreadyExistsBackstop(t *testing.T) {
	ev := passingEvidence()
	ev.DedupeEvidence = fmt.Sprintf(`msg="k8s AlreadyExists on derived spawn name — re-attaching to existing pod (exactly-once-across-crash backstop)" spawn_id=%s`, ev.SpawnID)
	ev.DedupeLog = &LogEvidence{
		Component: "mobile-hud", Namespace: "loom-hub", Pod: ev.CrashBReplacement.Name,
		Timestamp: ev.CrashBAt.Add(time.Second), Line: ev.DedupeEvidence,
	}
	if verdict := Evaluate(ev); !verdict.Pass1NoDoubleSpawn || !verdict.Overall {
		t.Fatalf("HUD restart AlreadyExists evidence must pass: %+v", verdict)
	}
}

func TestEvaluate_RequiresExactPostCrashDedupeLog(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Evidence)
	}{
		{"missing log", func(ev *Evidence) { ev.DedupeLog = nil }},
		{"line mismatch", func(ev *Evidence) { ev.DedupeLog.Line = "different line" }},
		{"unapproved phrase", func(ev *Evidence) {
			ev.DedupeEvidence = "arbitrary success spawn_id=" + ev.SpawnID
			ev.DedupeLog.Line = ev.DedupeEvidence
		}},
		{"spawn id prefix collision", func(ev *Evidence) {
			ev.DedupeEvidence = `workflow resume: re-attaching to in-flight spawn spawn_id=` + ev.SpawnID + "4"
			ev.DedupeLog.Line = ev.DedupeEvidence
		}},
		{"unknown component", func(ev *Evidence) { ev.DedupeLog.Component = "old-operator" }},
		{"wrong operator pod", func(ev *Evidence) { ev.DedupeLog.Pod = "operator-old" }},
		{"missing operator pod identity", func(ev *Evidence) { ev.CrashAReplacement.Name = "" }},
		{"operator log before crash", func(ev *Evidence) { ev.DedupeLog.Timestamp = ev.CrashAAt.Add(-time.Nanosecond) }},
		{"wrong HUD pod", func(ev *Evidence) {
			ev.DedupeLog.Component = "mobile-hud"
			ev.DedupeLog.Pod = "hud-old"
			ev.DedupeLog.Timestamp = ev.CrashBAt
		}},
		{"HUD log before crash", func(ev *Evidence) {
			ev.DedupeLog.Component = "mobile-hud"
			ev.DedupeLog.Pod = ev.CrashBReplacement.Name
			ev.DedupeLog.Timestamp = ev.CrashBAt.Add(-time.Nanosecond)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := passingEvidence()
			tt.mutate(&ev)
			if v := Evaluate(ev); v.Pass1NoDoubleSpawn || v.Overall {
				t.Fatalf("unattributed or pre-crash dedupe log must fail PASS-1: %+v", v)
			}
		})
	}
}

func TestEvaluate_QuarantineFailsPass2(t *testing.T) {
	ev := passingEvidence()
	ev.Final.Run.State = "quarantined"
	v := Evaluate(ev)
	if v.Pass2JournalOnce || v.Overall {
		t.Fatalf("quarantined run must fail PASS-2: %+v", v)
	}
}

func TestEvaluate_MissingCostSourceFailsPass4(t *testing.T) {
	ev := passingEvidence()
	ev.Final.Steps[0].CostSource = ""
	v := Evaluate(ev)
	if v.Pass4CostProvenance {
		t.Fatalf("success row without cost_source must fail PASS-4: %+v", v)
	}
}

func TestEvaluate_HonestFailureStepFailsGate(t *testing.T) {
	ev := passingEvidence()
	ev.Final.Steps[0].Status = "error"
	ev.Final.Steps[0].CostSource = ""
	v := Evaluate(ev)
	if v.Pass4CostProvenance {
		t.Fatalf("failed claude-code step must fail PASS-4: %+v", v)
	}
	if v.Overall {
		t.Fatalf("failed spawn must not produce an overall PASS: %+v", v)
	}
}

func TestEvaluate_RequiresDone(t *testing.T) {
	for _, state := range []string{"running", "paused", "error", "escalated", "quarantined"} {
		t.Run(state, func(t *testing.T) {
			ev := passingEvidence()
			ev.Final.Run.State = state
			v := Evaluate(ev)
			if v.Pass2JournalOnce || v.Overall {
				t.Fatalf("state %q must fail PASS-2: %+v", state, v)
			}
		})
	}
}

func TestEvaluate_RequiresFinalAgentIdentity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Evidence)
	}{
		{"spawn id drift", func(ev *Evidence) { ev.Final.Steps[0].SpawnID = "different-spawn" }},
		{"missing call hash", func(ev *Evidence) { ev.Final.Steps[0].CallHash = "" }},
		{"call hash drift", func(ev *Evidence) { ev.Final.Steps[0].CallHash = "different-call-hash" }},
		{"run id drift", func(ev *Evidence) { ev.Final.Run.ID = "different-run" }},
		{"non-agent event", func(ev *Evidence) { ev.Final.Steps[0].EventType = "gate_eval" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := passingEvidence()
			tt.mutate(&ev)
			if v := Evaluate(ev); v.Pass2JournalOnce || v.Overall {
				t.Fatalf("final agent identity drift must fail PASS-2: %+v", v)
			}
		})
	}
}

func TestEvaluate_ClaudeRequiresRealCostSource(t *testing.T) {
	// Steady-state sources other than the agent's own are fabrication and
	// fail; "unavailable" is the honest crash-degraded recording (the gate
	// crashes the telemetry harvester inside the turn by design) and passes
	// with the degradation named in the reason.
	for _, source := range []string{"", "estimated"} {
		t.Run(source, func(t *testing.T) {
			ev := passingEvidence()
			ev.Final.Steps[0].CostSource = source
			if v := Evaluate(ev); v.Pass4CostProvenance || v.Overall {
				t.Fatalf("cost source %q must fail PASS-4: %+v", source, v)
			}
		})
	}
	t.Run("unavailable", func(t *testing.T) {
		ev := passingEvidence()
		ev.Final.Steps[0].CostSource = "unavailable"
		v := Evaluate(ev)
		if !v.Pass4CostProvenance || !v.Overall {
			t.Fatalf("crash-degraded unavailable provenance must pass PASS-4: %+v", v)
		}
		if !strings.Contains(v.Pass4Reason, "degraded") {
			t.Fatalf("degraded provenance reason must name the degradation: %q", v.Pass4Reason)
		}
	})
}

func TestEvaluate_CodexRequiresEstimatedCostSource(t *testing.T) {
	ev := passingEvidence()
	ev.AgentType = AgentTypeCodex
	ev.Final.Run.AgentType = AgentTypeCodex
	ev.CrashASafety.Target.Run.AgentType = AgentTypeCodex
	ev.CrashBSafety.Target.Run.AgentType = AgentTypeCodex
	ev.Final.Steps[0].CostSource = "estimated"
	if v := Evaluate(ev); !v.Pass4CostProvenance || !v.Overall {
		t.Fatalf("codex estimated provenance must pass: %+v", v)
	}

	for _, source := range []string{"", "real"} {
		t.Run(source, func(t *testing.T) {
			ev := ev
			ev.Final.Steps[0].CostSource = source
			if v := Evaluate(ev); v.Pass4CostProvenance || v.Overall {
				t.Fatalf("codex cost source %q must fail PASS-4: %+v", source, v)
			}
		})
	}
	t.Run("unavailable", func(t *testing.T) {
		ev := ev
		ev.Final.Steps[0].CostSource = "unavailable"
		v := Evaluate(ev)
		if !v.Pass4CostProvenance || !v.Overall {
			t.Fatalf("codex crash-degraded unavailable provenance must pass PASS-4: %+v", v)
		}
	})
}

func TestEvaluate_RequiresImmutableRunIdentity(t *testing.T) {
	for name, mutate := range map[string]func(*Evidence){
		"missing evidence agent": func(ev *Evidence) { ev.AgentType = "" },
		"server agent drift":     func(ev *Evidence) { ev.Final.Run.AgentType = AgentTypeCodex },
		"template version drift": func(ev *Evidence) { ev.Final.Run.TemplateVersion = "v-next" },
		"interpreter version drift": func(ev *Evidence) {
			ev.Final.Run.InterpreterVersion = "starlark-next"
		},
	} {
		t.Run(name, func(t *testing.T) {
			ev := passingEvidence()
			mutate(&ev)
			if v := Evaluate(ev); v.Pass2JournalOnce || v.Overall {
				t.Fatalf("run identity drift must fail closed: %+v", v)
			}
		})
	}
}

func TestEvaluate_RequiresCompletedSpawnRecord(t *testing.T) {
	for _, status := range []string{"creating", "running", "failed", "stopped", "unknown"} {
		t.Run(status, func(t *testing.T) {
			ev := passingEvidence()
			ev.FinalSpawnRecordStatuses[ev.SpawnID] = status
			if v := Evaluate(ev); v.Pass1NoDoubleSpawn || v.Overall {
				t.Fatalf("spawn status %q must fail PASS-1: %+v", status, v)
			}
		})
	}
}

func TestEvaluate_RequiresExactDerivedIdempotencyKey(t *testing.T) {
	ev := passingEvidence()
	ev.FinalSpawnIdempotencyKeys[ev.SpawnID] = "different-key"
	if v := Evaluate(ev); v.Pass1NoDoubleSpawn || v.Overall {
		t.Fatalf("mismatched idempotency key must fail PASS-1: %+v", v)
	}
}

func TestEvaluate_ObservationGapFailsClosed(t *testing.T) {
	ev := passingEvidence()
	ev.ObservationErrors = []string{"count spawn pods: apiserver timeout"}
	if v := Evaluate(ev); v.Pass1NoDoubleSpawn || v.Overall {
		t.Fatalf("incomplete sampling must fail PASS-1: %+v", v)
	}
}

func TestEvaluate_ZeroSuccessRowsFailsPass5(t *testing.T) {
	ev := passingEvidence()
	ev.Final.Steps[0].Status = "pending"
	v := Evaluate(ev)
	if v.Pass5CounterExact || v.Pass2JournalOnce {
		t.Fatalf("pending-only journal must fail PASS-2 and PASS-5: %+v", v)
	}
}

func TestFindAgentStep(t *testing.T) {
	d := RunDetail{Steps: []StepView{
		{StepKey: "root/gate#0", EventType: "gate_eval", Status: "success"},
		{StepKey: "root/agent#0", EventType: "spawn_requested", Status: "pending", SpawnID: "abc"},
	}}
	st, ok := FindAgentStep(d)
	if !ok || st.StepKey != "root/agent#0" {
		t.Fatalf("FindAgentStep: got %+v ok=%v", st, ok)
	}

	// A spawn row WITHOUT a spawn id still matches: a healthy in-flight
	// dispatch's pending row has no spawn_id (recorded only on completion) —
	// the identity is derived, not read.
	d.Steps[1].SpawnID = ""
	if _, ok := FindAgentStep(d); !ok {
		t.Fatal("pending spawn step without spawn_id must match")
	}
}

func TestDeriveSpawnIdentity(t *testing.T) {
	st := StepView{StepKey: "root/agent~abcd#0", CallHash: "deadbeef"}
	identity, err := DeriveSpawnIdentity("wf-canary-1", st)
	if err != nil || identity.SpawnID == "" || identity.PodName != "spawn-"+identity.SpawnID || identity.IdempotencyKey == "" {
		t.Fatalf("derived identity: identity=%+v err=%v", identity, err)
	}
	// Deterministic.
	identity2, err := DeriveSpawnIdentity("wf-canary-1", st)
	if err != nil || identity != identity2 {
		t.Fatalf("identity not deterministic: %+v vs %+v err=%v", identity, identity2, err)
	}
	// A matching recorded id is verified; a mismatch fails closed.
	st.SpawnID = identity.SpawnID
	if got, err := DeriveSpawnIdentity("wf-canary-1", st); err != nil || got != identity {
		t.Fatalf("matching recorded id rejected: %+v %v", got, err)
	}
	st.SpawnID = "wrong-recorded-id"
	if _, err := DeriveSpawnIdentity("wf-canary-1", st); err == nil {
		t.Fatal("recorded/derived identity mismatch must fail")
	}
}

func TestParseWorkflowPolicy(t *testing.T) {
	global, enabled, k8sOnly, err := parseWorkflowPolicy("enabled: false\nworkflows:\n  enabled: true\n  substrate_k8s_only: true\n")
	if err != nil || global || !enabled || !k8sOnly {
		t.Fatalf("parseWorkflowPolicy() = %v %v %v %v", global, enabled, k8sOnly, err)
	}
}

func TestParseGitOpsKustomizationRequiresReadyConvergence(t *testing.T) {
	good := `{"status":{"lastAppliedRevision":"main@sha1:abc","lastAttemptedRevision":"main@sha1:abc","conditions":[{"type":"Ready","status":"True"}]}}`
	applied, attempted, ready, err := parseGitOpsKustomization(good)
	if err != nil || !ready || applied != attempted {
		t.Fatalf("parse converged Flux state = %q %q %t %v", applied, attempted, ready, err)
	}
	for name, raw := range map[string]string{
		"not ready": `{"status":{"lastAppliedRevision":"a","lastAttemptedRevision":"a","conditions":[{"type":"Ready","status":"False"}]}}`,
		"drifted":   `{"status":{"lastAppliedRevision":"a","lastAttemptedRevision":"b","conditions":[{"type":"Ready","status":"True"}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := parseGitOpsKustomization(raw); err == nil {
				t.Fatal("expected fail-closed Flux parse")
			}
		})
	}
}

func fluxSourceListJSON(platformRevision string, platformReady bool, loomCoreRevision string, loomCoreReady bool) string {
	return fluxSourceListJSONWithVersions(platformRevision, platformReady, "101", loomCoreRevision, loomCoreReady, "202")
}

func gitRepositoryListJSON(platformRevision, loomCoreRevision string) string {
	return fmt.Sprintf(`{"apiVersion":"source.toolkit.fluxcd.io/v1","kind":"GitRepositoryList","metadata":{"resourceVersion":"9501"},"items":[
		{"metadata":{"name":"gitops-gitlab","namespace":"flux-system","uid":"gitops-repo-uid","resourceVersion":"501","generation":7},"spec":%s,"status":{"observedGeneration":7,"artifact":{"revision":"main@sha1:%s","digest":"sha256:%s"},"conditions":[{"type":"Ready","status":"True","observedGeneration":7},{"type":"ArtifactInStorage","status":"True","observedGeneration":7}]}},
		{"metadata":{"name":"loom-core","namespace":"flux-system","uid":"loom-core-repo-uid","resourceVersion":"502","generation":3},"spec":%s,"status":{"observedGeneration":3,"artifact":{"revision":"main@sha1:%s","digest":"sha256:%s"},"conditions":[{"type":"Ready","status":"True","observedGeneration":3},{"type":"ArtifactInStorage","status":"True","observedGeneration":3}]}}
	]}`,
		testGitRepositorySpecJSON("gitops-gitlab"), platformRevision, strings.Repeat("c", 64),
		testGitRepositorySpecJSON("loom-core"), loomCoreRevision, strings.Repeat("d", 64))
}

func fluxSourceListJSONWithVersions(
	platformRevision string,
	platformReady bool,
	platformResourceVersion string,
	loomCoreRevision string,
	loomCoreReady bool,
	loomCoreResourceVersion string,
) string {
	readyStatus := func(ready bool) string {
		if ready {
			return "True"
		}
		return "False"
	}
	return fmt.Sprintf(`{"apiVersion":"kustomize.toolkit.fluxcd.io/v1","kind":"KustomizationList","metadata":{"resourceVersion":"9001"},"items":[
		{"metadata":{"name":"apps","uid":"apps-uid","resourceVersion":"%s","generation":7},"spec":%s,"status":{"lastAppliedRevision":"main@sha1:%s","lastAttemptedRevision":"main@sha1:%s","conditions":[{"type":"Ready","status":"%s","observedGeneration":7}]}},
		{"metadata":{"name":"bootstrap","uid":"bootstrap-uid","resourceVersion":"151","generation":5},"spec":%s,"status":{"lastAppliedRevision":"main@sha1:%s","lastAttemptedRevision":"main@sha1:%s","conditions":[{"type":"Ready","status":"%s","observedGeneration":5}]}},
		{"metadata":{"name":"system","uid":"system-uid","resourceVersion":"181","generation":9},"spec":%s,"status":{"lastAppliedRevision":"main@sha1:%s","lastAttemptedRevision":"main@sha1:%s","conditions":[{"type":"Ready","status":"%s","observedGeneration":9}]}},
		{"metadata":{"name":"loom-hub-servers","uid":"loom-core-uid","resourceVersion":"%s","generation":11},"spec":%s,"status":{"lastAppliedRevision":"main@sha1:%s","lastAttemptedRevision":"main@sha1:%s","conditions":[{"type":"Ready","status":"%s","observedGeneration":11}]}}
	]}`,
		platformResourceVersion, testFluxSpecJSON("apps"), platformRevision, platformRevision, readyStatus(platformReady),
		testFluxSpecJSON("bootstrap"), platformRevision, platformRevision, readyStatus(platformReady),
		testFluxSpecJSON("system"), platformRevision, platformRevision, readyStatus(platformReady),
		loomCoreResourceVersion, testFluxSpecJSON("loom-hub-servers"), loomCoreRevision, loomCoreRevision, readyStatus(loomCoreReady))
}

func TestParseFluxSourceSnapshotRequiresBothUniqueSources(t *testing.T) {
	raw := fluxSourceListJSON("platform", true, "loom-core", true)
	var list gitOpsKustomizationListWire
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatal(err)
	}
	var unrelated gitOpsKustomizationWire
	unrelated.Metadata.Name = "unrelated"
	unrelated.Metadata.UID = "unrelated-uid"
	unrelated.Metadata.ResourceVersion = "303"
	list.Items = []gitOpsKustomizationWire{list.Items[3], unrelated, list.Items[2], list.Items[0], list.Items[1]}
	reordered, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := parseFluxSourceSnapshot(string(reordered))
	if err != nil || snapshot.platform.applied != "main@sha1:platform" ||
		snapshot.bootstrap.applied != "main@sha1:platform" || snapshot.system.applied != "main@sha1:platform" ||
		snapshot.loomCore.applied != "main@sha1:loom-core" {
		t.Fatalf("parseFluxSourceSnapshot() = %+v, %v", snapshot, err)
	}
	for name, invalid := range map[string]string{
		"missing list resourceVersion": strings.Replace(raw, `"resourceVersion":"9001"`, `"resourceVersion":""`, 1),
		"missing item resourceVersion": strings.Replace(raw, `"resourceVersion":"101"`, `"resourceVersion":""`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseFluxSourceSnapshot(invalid); err == nil || !strings.Contains(err.Error(), "resourceVersion") {
				t.Fatalf("parseFluxSourceSnapshot() error = %v", err)
			}
		})
	}
	staleReady := strings.Replace(raw, `"observedGeneration":7`, `"observedGeneration":6`, 1)
	if _, err := parseFluxSourceSnapshot(staleReady); err == nil || !strings.Contains(err.Error(), "Ready status is stale") {
		t.Fatalf("stale Ready generation error = %v", err)
	}
	missing := fmt.Sprintf(`{"metadata":{"resourceVersion":"1"},"items":[{"metadata":{"name":"apps","uid":"apps-uid","resourceVersion":"2","generation":1},"spec":%s,"status":{"lastAppliedRevision":"a","lastAttemptedRevision":"a","conditions":[{"type":"Ready","status":"True","observedGeneration":1}]}}]}`,
		testFluxSpecJSON("apps"))
	if _, err := parseFluxSourceSnapshot(missing); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing source error = %v", err)
	}
	duplicate := fmt.Sprintf(`{"metadata":{"resourceVersion":"1"},"items":[
		{"metadata":{"name":"apps","uid":"apps-uid","resourceVersion":"2","generation":1},"spec":%s,"status":{"lastAppliedRevision":"a","lastAttemptedRevision":"a","conditions":[{"type":"Ready","status":"True","observedGeneration":1}]}},
		{"metadata":{"name":"apps","uid":"apps-uid","resourceVersion":"3","generation":1},"spec":%s,"status":{"lastAppliedRevision":"a","lastAttemptedRevision":"a","conditions":[{"type":"Ready","status":"True","observedGeneration":1}]}},
		{"metadata":{"name":"loom-hub-servers","uid":"loom-uid","resourceVersion":"4","generation":1},"spec":%s,"status":{"lastAppliedRevision":"b","lastAttemptedRevision":"b","conditions":[{"type":"Ready","status":"True","observedGeneration":1}]}}
	]}`, testFluxSpecJSON("apps"), testFluxSpecJSON("apps"), testFluxSpecJSON("loom-hub-servers"))
	if _, err := parseFluxSourceSnapshot(duplicate); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate source error = %v", err)
	}
}

func TestNormalizeExpectedGitOpsRevision(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	for _, raw := range []string{sha, "main@sha1:" + sha} {
		got, err := normalizeExpectedGitOpsRevision(raw)
		if err != nil || got != sha {
			t.Fatalf("normalizeExpectedGitOpsRevision(%q) = %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"short", strings.Repeat("z", 40)} {
		if _, err := normalizeExpectedGitOpsRevision(raw); err == nil {
			t.Fatalf("invalid expected revision %q accepted", raw)
		}
	}
}

func TestCanonicalCanaryGateBinding(t *testing.T) {
	gateID, err := NewCanaryGateID()
	if err != nil {
		t.Fatal(err)
	}
	if len(gateID) != 32 || strings.ToLower(gateID) != gateID {
		t.Fatalf("NewCanaryGateID() = %q", gateID)
	}
	startedAt := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	previous := strings.Repeat("a", 64)
	runID, err := CanaryRunIDForGate(gateID, 2)
	if err != nil {
		t.Fatal(err)
	}
	evidence := Evidence{
		RunID: runID,
		GateBinding: GateBinding{
			Contract: GateBindingContract, ContractVersion: GateBindingContractVersion,
			GateID: gateID, RunIndex: 2, RequiredRuns: S1cGateRequiredRuns,
			GateStartedAt: startedAt, PreviousEvidenceSHA256: previous,
		},
	}
	if err := ValidateGateBinding(evidence); err != nil {
		t.Fatalf("canonical gate binding rejected: %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*Evidence)
	}{
		{"contract", func(ev *Evidence) { ev.GateBinding.ContractVersion++ }},
		{"gate id", func(ev *Evidence) { ev.GateBinding.GateID = strings.Repeat("G", 32) }},
		{"run index", func(ev *Evidence) { ev.GateBinding.RunIndex = 4 }},
		{"required runs", func(ev *Evidence) { ev.GateBinding.RequiredRuns = 2 }},
		{"canonical run id", func(ev *Evidence) { ev.RunID = "wf-canary-other" }},
		{"start timestamp", func(ev *Evidence) { ev.GateBinding.GateStartedAt = time.Time{} }},
		{"predecessor hash", func(ev *Evidence) { ev.GateBinding.PreviousEvidenceSHA256 = "bad" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutated := evidence
			test.mutate(&mutated)
			if err := ValidateGateBinding(mutated); err == nil {
				t.Fatalf("invalid gate binding accepted: %+v", mutated.GateBinding)
			}
		})
	}
}

func TestEvaluateRejectsInvalidNonEmptyGateBinding(t *testing.T) {
	evidence := passingEvidence()
	evidence.GateBinding = GateBinding{Contract: "hand-edited"}
	verdict := Evaluate(evidence)
	if verdict.Pass8CrashSafety || verdict.Overall || !strings.Contains(verdict.Pass8Reason, "gate binding contract") {
		t.Fatalf("invalid gate binding passed: %+v", verdict)
	}
}

func TestParseStableDeploymentRejectsRolloutOverlap(t *testing.T) {
	stable := controllerDeploymentFixtureJSON("mobile-hud", "loom-hub", "deploy-uid", "hud:v1")
	if _, err := parseStableDeployment(stable); err != nil {
		t.Fatalf("stable deployment rejected: %v", err)
	}
	rolling := strings.Replace(stable, `"type":"Recreate"`, `"type":"RollingUpdate"`, 1)
	if _, err := parseStableDeployment(rolling); err == nil || !strings.Contains(err.Error(), "stable singleton") {
		t.Fatalf("rolling deployment strategy accepted: %v", err)
	}
	overlap := mutateJSONObject(stable, func(object map[string]any) {
		object["status"].(map[string]any)["replicas"] = float64(2)
	})
	if _, err := parseStableDeployment(overlap); err == nil {
		t.Fatal("rolling overlap must fail stable singleton proof")
	}
}

func TestParseSpawnStateConfigMap(t *testing.T) {
	state := `{"spawn_id":"spawn-abc","status":"running","request":{"branch":"mills-wf/run-1","idempotency_key":"wf-key"}}`
	raw := testSpawnStateConfigMapJSON(map[string]string{"spawn-abc": state})
	snapshot, err := parseSpawnStateConfigMap(raw, "run-1")
	if err != nil || len(snapshot.RecordIDs) != 1 || snapshot.RecordIDs[0] != "spawn-abc" ||
		len(snapshot.ActiveIDs) != 1 || snapshot.IdempotencyKeys["spawn-abc"] != "wf-key" ||
		snapshot.ConfigMapUID != "spawn-state-uid" || snapshot.ConfigMapIdentity.ResourceVersion != "100" {
		t.Fatalf("parseSpawnStateConfigMap() = %+v, %v", snapshot, err)
	}
}

func TestUnrelatedActiveSpawnIDs(t *testing.T) {
	got := unrelatedActiveSpawnIDs(
		[]string{"allowed", "foreign", "foreign", "allowed"},
		"allowed",
	)
	want := []string{"foreign", "foreign"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unrelatedActiveSpawnIDs() = %v, want %v", got, want)
	}
	if got := unrelatedActiveSpawnIDs([]string{"allowed"}, "allowed"); len(got) != 0 {
		t.Fatalf("exact allowed spawn remained a blocker: %v", got)
	}
}

func TestParseSpawnStateConfigMapFailsClosed(t *testing.T) {
	deleted := "2026-07-14T12:00:00Z"
	tests := map[string]string{
		"bad configmap": `{`,
		"bad entry": testSpawnStateConfigMapJSON(map[string]string{
			"spawn-abc": "{",
		}),
		"key mismatch": testSpawnStateConfigMapJSON(map[string]string{
			"spawn-key": `{"spawn_id":"spawn-payload","status":"running"}`,
		}),
		"terminating":              testConfigMapJSON(s1cSpawnNamespace, spawnStateConfigMapName, "spawn-state-uid", "100", nil, &deleted),
		"wrong name":               testConfigMapJSON(s1cSpawnNamespace, "other", "spawn-state-uid", "100", nil, nil),
		"wrong namespace":          testConfigMapJSON("other", spawnStateConfigMapName, "spawn-state-uid", "100", nil, nil),
		"missing resource version": testConfigMapJSON(s1cSpawnNamespace, spawnStateConfigMapName, "spawn-state-uid", "", nil, nil),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSpawnStateConfigMap(raw, ""); err == nil {
				t.Fatal("expected fail-closed parse error")
			}
		})
	}
}

func TestParseReadyPodRejectsTerminatingOrAmbiguous(t *testing.T) {
	want := controllerPodFixture(
		"new", "loom-mills", "uid-new", "repo:v1", "repo@sha256:abc",
		"operator", "deployment-uid", time.Date(2026, time.July, 12, 0, 0, 0, 0, time.UTC),
	)
	ready := controllerPodListFixtureJSON(want)
	identity, err := parseReadyPod(ready, want.Namespace, "app=operator", want.ContainerName)
	if err != nil || identity.UID != "uid-new" || identity.ImageID != "repo@sha256:abc" {
		t.Fatalf("parseReadyPod() = %+v, %v", identity, err)
	}
	terminating := mutateJSONObject(ready, func(object map[string]any) {
		firstPodObject(object)["metadata"].(map[string]any)["deletionTimestamp"] = "2026-07-12T00:00:00Z"
	})
	if _, err := parseReadyPod(terminating, want.Namespace, "app=operator", want.ContainerName); err == nil {
		t.Fatal("terminating Ready pod must fail closed")
	}
	if _, err := parseReadyPod(`{"metadata":{"resourceVersion":"1"},"items":[{},{}]}`,
		want.Namespace, "app=operator", want.ContainerName); err == nil {
		t.Fatal("terminating plus Ready pod must fail closed")
	}
}

func TestParseActivePodNamesTreatsUnknownAndDeletingAsActive(t *testing.T) {
	raw := `{"items":[
		{"metadata":{"name":"spawn-unknown"},"status":{"phase":"Unknown"}},
		{"metadata":{"name":"spawn-deleting","deletionTimestamp":"2026-07-12T00:00:00Z"},"status":{"phase":"Running"}},
		{"metadata":{"name":"spawn-done"},"status":{"phase":"Succeeded"}}
	]}`
	names, err := parseActivePodNames(raw)
	if err != nil || len(names) != 2 || names[0] != "spawn-deleting" || names[1] != "spawn-unknown" {
		t.Fatalf("parseActivePodNames() = %v, %v", names, err)
	}
}

func TestPreflightUsesConfigMapAndCapturesExactIdentity(t *testing.T) {
	policyChecksum := strings.Repeat("1", 64)
	var activeWorkflow, activePipeline, activeRecord, activePod, foreignRecord, globalEnabled bool
	spawnUpdateAllowed := true
	var allowedRunID, activeRecordID, activePodName string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/mills/policy":
			_, _ = fmt.Fprintf(w, `{"Enabled":%t,"Workflows":{"Enabled":true,"SubstrateK8sOnly":true}}`, globalEnabled)
		case "/api/mills/safety/quiescence":
			workflowCount, pipelineCount, workflowOperations, backgroundOperations := 0, 0, 0, 0
			workflowRunIDs := "[]"
			if activeWorkflow {
				workflowCount = 1
			}
			if allowedRunID != "" {
				workflowCount, workflowOperations, backgroundOperations = 1, 1, 1
				workflowRunIDs = fmt.Sprintf("[%q]", allowedRunID)
			}
			if activePipeline {
				pipelineCount = 1
			}
			w.Header().Set("Cache-Control", "no-store")
			_, _ = fmt.Fprintf(w, `{"observed_at":%q,"quiescent":%t,"counts":{"active_workflow_runs":%d,"active_pipeline_runs":%d},"in_memory":{"admission_closed":true,"policy_generation":2,"sources_ready":true,"sample_stable":true,"wiring_required":true,"activity_sources":6,"source_generation":3,"source_operations":{"reconciler":0,"pipeline":0,"cross_run":0,"council":0,"canary":0,"workflow":%d},"source_run_ids":{"workflow":%s},"background_operations":%d}}`,
				time.Now().UTC().Format(time.RFC3339Nano),
				workflowCount == 0 && pipelineCount == 0, workflowCount, pipelineCount,
				workflowOperations, workflowRunIDs, backgroundOperations)
		default:
			if allowedRunID != "" && r.URL.Path == "/api/mills/workflow/runs/"+allowedRunID {
				_, _ = fmt.Fprintf(w, `{"run":{"id":%q,"state":"running"},"steps":[{"step_key":"root/agent#0","event_type":"spawn_requested","status":"pending","call_hash":"call-hash"}]}`, allowedRunID)
				return
			}
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	spawnPodList := func(name, uid, image, imageID string) string {
		return fmt.Sprintf(`{"items":[{"metadata":{"name":%q,"uid":%q},"spec":{"nodeName":"node-1","containers":[{"image":%q}]},"status":{"phase":"Running","startTime":"2026-07-12T00:00:00Z","containerStatuses":[{"ready":true,"imageID":%q}]}}]}`,
			name, uid, image, imageID)
	}
	deployment := func(name, image, checksum, strategy string) string {
		namespace := "loom-mills"
		if name == "mobile-hud" {
			namespace = "loom-hub"
		}
		return fmt.Sprintf(`{"metadata":{"name":%q,"namespace":%q,"uid":%q,"resourceVersion":"10","generation":7},"spec":{"replicas":1,"selector":{"matchLabels":{"app":%q}},"strategy":{"type":%q},"template":{"metadata":{"labels":{"app":%q},"annotations":{"loom.flexinfer.ai/policy-checksum":%q}},"spec":{"containers":[{"name":"controller","image":%q}]}}},"status":{"observedGeneration":7,"replicas":1,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1}}`,
			name, namespace, name+"-deployment-uid", name, strategy, name, checksum, image)
	}
	controllerStartedAt := time.Date(2026, time.July, 12, 0, 0, 0, 0, time.UTC)
	operatorPod := controllerPodFixture(
		"operator-pod", s1cOperatorNamespace, "op-uid", "operator:v1", "operator@sha256:abc",
		"loom-mills-operator", "loom-mills-operator-deployment-uid", controllerStartedAt,
	)
	hudPod := controllerPodFixture(
		"hud-pod", "loom-hub", "hud-uid", "hud:v1", "hud@sha256:def",
		"mobile-hud", "mobile-hud-deployment-uid", controllerStartedAt,
	)
	var calls []string
	h := New(Config{OperatorURL: server.URL})
	configureControllerPodDryRunFixture(t, h)
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		cmd := strings.Join(args, " ")
		calls = append(calls, cmd)
		switch {
		case strings.Contains(cmd, " exec "):
			return "", fmt.Errorf("exec must not be used")
		case strings.Contains(cmd, "get ns"):
			return testNamespaceListJSON(s1cOperatorNamespace, "loom-hub", s1cSpawnNamespace, "logging"), nil
		case strings.Contains(cmd, "/apis/source.toolkit.fluxcd.io/v1/namespaces/flux-system/gitrepositories?limit=0"):
			return gitRepositoryListJSON(strings.Repeat("a", 40), strings.Repeat("b", 40)), nil
		case strings.Contains(cmd, "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/flux-system/kustomizations?limit=0"):
			return fluxSourceListJSON(strings.Repeat("a", 40), true, strings.Repeat("b", 40), true), nil
		case strings.Contains(cmd, "get deploy loom-mills-operator"):
			return deployment("loom-mills-operator", "operator:v1", policyChecksum, "Recreate"), nil
		case strings.Contains(cmd, "get configmap loom-mills-policy"):
			policy := fmt.Sprintf("enabled: %t\nworkflows:\n  enabled: true\n  substrate_k8s_only: true\n", globalEnabled)
			return testConfigMapJSON(s1cOperatorNamespace, policyConfigMapName, "policy-configmap-uid", "200",
				map[string]string{"policy.yaml": policy}, nil), nil
		case strings.Contains(cmd, controllerPodCensusPath("loom-mills")):
			return controllerPodListFixtureJSON(operatorPod), nil
		case strings.Contains(cmd, "loom-mills get replicaset"):
			return controllerReplicaSetFixtureJSON(operatorPod,
				deployment("loom-mills-operator", "operator:v1", policyChecksum, "Recreate")), nil
		case strings.Contains(cmd, "get deploy mobile-hud"):
			return deployment("mobile-hud", "hud:v1", "", "Recreate"), nil
		case strings.Contains(cmd, controllerPodCensusPath("loom-hub")):
			return controllerPodListFixtureJSON(hudPod), nil
		case strings.Contains(cmd, "loom-hub get replicaset"):
			return controllerReplicaSetFixtureJSON(hudPod,
				deployment("mobile-hud", "hud:v1", "", "Recreate")), nil
		case strings.Contains(cmd, "get configmap loom-spawn-state"):
			data := make(map[string]string)
			if activeRecord {
				id := activeRecordID
				if id == "" {
					id = "spawn-active"
				}
				data[id] = fmt.Sprintf(`{"spawn_id":%q,"status":"running","request":{}}`, id)
			}
			if foreignRecord {
				data["spawn-foreign"] = `{"spawn_id":"spawn-foreign","status":"running","request":{}}`
			}
			return testConfigMapJSON(s1cSpawnNamespace, spawnStateConfigMapName, "spawn-configmap-uid", "300", data, nil), nil
		case strings.Contains(cmd, "auth can-i update configmap/loom-spawn-state"):
			if !spawnUpdateAllowed {
				return "no", nil
			}
			return "yes", nil
		case strings.Contains(cmd, "get pods -o json"):
			if activePod {
				name := activePodName
				if name == "" {
					name = "spawn-active"
				}
				return spawnPodList(name, "spawn-uid", "spawn:v1", "spawn@sha256:123"), nil
			}
			return `{"items":[]}`, nil
		case strings.Contains(cmd, "get --raw"):
			return "ready", nil
		default:
			return "", fmt.Errorf("unexpected kubectl call: %s", cmd)
		}
	}

	report, err := h.Preflight(context.Background())
	if err != nil || !report.AllPreconditions {
		t.Fatalf("Preflight() = %+v, %v\ncalls=%v", report, err, calls)
	}
	if report.Operator.UID != "op-uid" || report.Operator.ImageID != "operator@sha256:abc" ||
		report.Hud.UID != "hud-uid" || report.PolicyChecksum != policyChecksum ||
		report.PolicyConfigMapIdentity.UID != "policy-configmap-uid" ||
		report.SpawnConfigMapIdentity.UID != report.SpawnConfigMapUID ||
		report.GitOpsRevision == "" || report.GitOpsBootstrapRevision == "" ||
		report.GitOpsSystemRevision == "" || report.LoomCoreRevision == "" {
		t.Fatalf("identity incomplete: %+v", report)
	}
	for _, call := range calls {
		if strings.Contains(call, " exec ") {
			t.Fatalf("preflight used kubectl exec: %s", call)
		}
	}
	coherentSourceReads := 0
	for _, call := range calls {
		if strings.Contains(call, "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/flux-system/kustomizations?limit=0") {
			coherentSourceReads++
		}
	}
	if coherentSourceReads != 2 {
		t.Fatalf("coherent Flux source reads = %d, want start/end fence; calls=%v", coherentSourceReads, calls)
	}
	for name, enable := range map[string]func(){
		"open global admission": func() { globalEnabled = true },
		"workflow":              func() { activeWorkflow = true },
		"pipeline":              func() { activePipeline = true },
		"record":                func() { activeRecord = true },
		"pod":                   func() { activePod = true },
		"spawn update RBAC":     func() { spawnUpdateAllowed = false },
	} {
		t.Run("fails closed on active "+name, func(t *testing.T) {
			activeWorkflow, activePipeline, activeRecord, activePod, globalEnabled = false, false, false, false, false
			spawnUpdateAllowed = true
			enable()
			got, err := h.Preflight(context.Background())
			if err != nil {
				t.Fatalf("Preflight() error = %v", err)
			}
			if got.AllPreconditions {
				t.Fatalf("active %s must block preflight: %+v", name, got)
			}
		})
	}

	activeWorkflow, activePipeline, activeRecord, activePod, foreignRecord, globalEnabled = false, false, true, true, false, false
	spawnUpdateAllowed = true
	allowedRunID = "wf-canary-allowed"
	identity, err := DeriveSpawnIdentity(allowedRunID, StepView{StepKey: "root/agent#0", CallHash: "call-hash"})
	if err != nil {
		t.Fatalf("derive allowed spawn identity: %v", err)
	}
	activeRecordID, activePodName = identity.SpawnID, identity.PodName
	report, err = h.Preflight(context.Background(), allowedRunID)
	if err != nil || !report.AllPreconditions {
		t.Fatalf("branchless exact allowed spawn must pass: report=%+v err=%v", report, err)
	}
	if len(report.ActiveSpawnIDs) != 0 || len(report.ActiveSpawnPodNames) != 0 {
		t.Fatalf("exact allowed spawn remained a blocker: records=%v pods=%v", report.ActiveSpawnIDs, report.ActiveSpawnPodNames)
	}

	foreignRecord = true
	report, err = h.Preflight(context.Background(), allowedRunID)
	if err != nil {
		t.Fatalf("foreign active spawn preflight error = %v", err)
	}
	if report.AllPreconditions || !reflect.DeepEqual(report.ActiveSpawnIDs, []string{"spawn-foreign"}) {
		t.Fatalf("foreign active spawn must remain a blocker: %+v", report)
	}
}

func TestCrashPodRequiresReplacementUIDAndSameDigest(t *testing.T) {
	deploymentJSON := controllerDeploymentFixtureJSON("operator", "loom-mills", "operator-deployment-uid", "operator:v1")
	beforeDeployment := mustStableDeploymentFixture(deploymentJSON)
	before := controllerPodFixture(
		"operator-old", beforeDeployment.Namespace, "uid-old", beforeDeployment.Image, "operator@sha256:abc",
		beforeDeployment.Name, beforeDeployment.UID, time.Date(2026, time.July, 12, 0, 1, 0, 0, time.UTC),
	)
	before = bindControllerPodFixture(before, beforeDeployment)
	for _, tt := range []struct {
		name, uid, imageID string
		wantErr            bool
	}{
		{"replacement", "uid-new", "operator@sha256:abc", false},
		{"same uid", "uid-old", "operator@sha256:abc", true},
		{"digest drift", "uid-new", "operator@sha256:def", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			after := replacementControllerPodFixture(before, "operator-new", tt.uid, time.Now().UTC())
			after.ImageID = tt.imageID
			h := New(Config{PollInterval: time.Millisecond, StepTimeout: 10 * time.Millisecond})
			configureControllerPodDryRunFixture(t, h)
			deleted := false
			preDeletePodReads := 0
			h.deletePodFn = func(_ context.Context, namespace, name, uid, resourceVersion string) error {
				if namespace != before.Namespace || name != before.Name || uid != before.UID ||
					resourceVersion != before.ResourceVersion {
					t.Fatalf("delete target = %s/%s uid=%s resourceVersion=%s", namespace, name, uid, resourceVersion)
				}
				deleted = true
				return nil
			}
			h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
				cmd := strings.Join(args, " ")
				switch {
				case strings.Contains(cmd, "get deploy"):
					return deploymentJSON, nil
				case strings.Contains(cmd, controllerPodCensusPath(before.Namespace)) && !deleted:
					preDeletePodReads++
					observed := before
					observed.PodCensusListResourceVersion = fmt.Sprintf("pre-delete-list-rv-%d", preDeletePodReads)
					return controllerPodListFixtureJSON(observed), nil
				case strings.Contains(cmd, controllerPodCensusPath(before.Namespace)):
					return controllerPodListFixtureJSON(after), nil
				case strings.Contains(cmd, "get replicaset") && !deleted:
					return controllerReplicaSetFixtureJSON(before, deploymentJSON), nil
				case strings.Contains(cmd, "get replicaset"):
					return controllerReplicaSetFixtureJSON(after, deploymentJSON), nil
				default:
					return "", nil
				}
			}
			after, err := h.CrashPod(context.Background(), "loom-mills", "app=operator", "operator", before, beforeDeployment)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CrashPod() = %+v, %v; wantErr %v", after, err, tt.wantErr)
			}
			if preDeletePodReads != 2 {
				t.Fatalf("pre-delete Pod census reads = %d, want current and final", preDeletePodReads)
			}
		})
	}
}

func TestCrashPodWithLeaseRunsFinalCheckAfterRenewalImmediatelyBeforeDelete(t *testing.T) {
	var renewed atomic.Bool
	var checked atomic.Bool
	var deleteStartHookRan atomic.Bool
	var checkCount atomic.Int32
	var postCheckIdentityReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/mills/safety/crash-lease/lease-token/renew" {
			http.NotFound(w, r)
			return
		}
		renewed.Store(true)
		_, _ = fmt.Fprintf(w, `{"token":"lease-token","request_id":"request-1","run_id":"wf-canary-1","spawn_id":"abc","expires_at":%q}`,
			time.Now().UTC().Add(90*time.Second).Format(time.RFC3339Nano))
	}))
	defer server.Close()

	deploymentJSON := controllerDeploymentFixtureJSON("operator", "loom-mills", "operator-deployment-uid", "operator:v1")
	beforeDeployment := mustStableDeploymentFixture(deploymentJSON)
	before := controllerPodFixture(
		"operator-old", beforeDeployment.Namespace, "uid-old", beforeDeployment.Image, "operator@sha256:abc",
		beforeDeployment.Name, beforeDeployment.UID, time.Date(2026, time.July, 12, 0, 0, 0, 0, time.UTC),
	)
	before = bindControllerPodFixture(before, beforeDeployment)
	after := replacementControllerPodFixture(before, "operator-new", "uid-new", time.Now().UTC())
	h := New(Config{OperatorURL: server.URL, AdminToken: "admin"})
	configureControllerPodDryRunFixture(t, h)
	deleted := false
	h.deletePodFn = func(context.Context, string, string, string, string) error {
		if !renewed.Load() {
			t.Fatal("UID delete ran before lease renewal")
		}
		if !checked.Load() {
			t.Fatal("UID delete ran before final workload check")
		}
		if !deleteStartHookRan.Load() {
			t.Fatal("UID delete ran before delete-start hook")
		}
		if postCheckIdentityReads.Load() != 3 {
			t.Fatalf("post-check controller identity reads = %d, want Deployment+Pod+ReplicaSet", postCheckIdentityReads.Load())
		}
		deleted = true
		return nil
	}
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		command := strings.Join(args, " ")
		if checked.Load() && !deleted && (strings.Contains(command, "get deploy") ||
			strings.Contains(command, controllerPodCensusPath(before.Namespace)) || strings.Contains(command, "get replicaset")) {
			postCheckIdentityReads.Add(1)
		}
		switch {
		case strings.Contains(command, "get deploy"):
			return deploymentJSON, nil
		case strings.Contains(command, controllerPodCensusPath(before.Namespace)) && !deleted:
			return controllerPodListFixtureJSON(before), nil
		case strings.Contains(command, controllerPodCensusPath(before.Namespace)):
			return controllerPodListFixtureJSON(after), nil
		case strings.Contains(command, "get replicaset") && !deleted:
			return controllerReplicaSetFixtureJSON(before, deploymentJSON), nil
		case strings.Contains(command, "get replicaset"):
			return controllerReplicaSetFixtureJSON(after, deploymentJSON), nil
		default:
			return "", nil
		}
	}
	if _, err := h.CrashPodWithLeaseAndHooks(context.Background(), "loom-mills", "app=operator", "operator",
		before, beforeDeployment, testAcquiredCrashLease(), func(context.Context) error {
			if !renewed.Load() {
				t.Fatal("final workload check ran before lease renewal")
			}
			checkCount.Add(1)
			checked.Store(true)
			return nil
		}, func() error {
			if !checked.Load() || deleted {
				t.Fatal("delete-start hook did not run after final check and identity reread")
			}
			if postCheckIdentityReads.Load() != 3 {
				t.Fatal("delete-start hook ran before bounded Deployment+Pod+ReplicaSet reread")
			}
			deleteStartHookRan.Store(true)
			return nil
		}); err != nil {
		t.Fatalf("CrashPodWithLeaseAndHooks() error = %v", err)
	}
	if got := checkCount.Load(); got != 1 {
		t.Fatalf("final workload check calls = %d, want 1", got)
	}
	if !deleteStartHookRan.Load() {
		t.Fatal("delete-start hook did not run")
	}
}

func TestCrashPodDeleteStartObserverCoversDelayedUIDDelete(t *testing.T) {
	deploymentJSON := controllerDeploymentFixtureJSON("mobile-hud", "loom-hub", "hud-deployment-uid", "hud:v1")
	beforeDeployment := mustStableDeploymentFixture(deploymentJSON)
	before := controllerPodFixture(
		"hud-old", beforeDeployment.Namespace, "uid-old", beforeDeployment.Image, "hud@sha256:abc",
		beforeDeployment.Name, beforeDeployment.UID, time.Date(2026, time.July, 12, 0, 0, 0, 0, time.UTC),
	)
	before = bindControllerPodFixture(before, beforeDeployment)
	after := replacementControllerPodFixture(before, "hud-new", "uid-new", time.Now().UTC())

	h := New(Config{
		PollInterval:        time.Millisecond,
		ProcessPollInterval: 5 * time.Millisecond,
		ProcessMaxSampleGap: 100 * time.Millisecond,
	})
	configureControllerPodDryRunFixture(t, h)
	var deleted atomic.Bool
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		command := strings.Join(args, " ")
		switch {
		case strings.Contains(command, "--field-selector metadata.name=spawn-abc"):
			if deleted.Load() {
				return `{"items":[]}`, nil
			}
			return spawnPodListJSON("spawn-abc", "spawn-uid", "Running"), nil
		case strings.Contains(command, "get deploy"):
			return deploymentJSON, nil
		case strings.Contains(command, controllerPodCensusPath(before.Namespace)) && !deleted.Load():
			return controllerPodListFixtureJSON(before), nil
		case strings.Contains(command, controllerPodCensusPath(before.Namespace)):
			return controllerPodListFixtureJSON(after), nil
		case strings.Contains(command, "get replicaset") && !deleted.Load():
			return controllerReplicaSetFixtureJSON(before, deploymentJSON), nil
		case strings.Contains(command, "get replicaset"):
			return controllerReplicaSetFixtureJSON(after, deploymentJSON), nil
		default:
			return "", nil
		}
	}
	var processProbes atomic.Int32
	h.processProbeFn = func(context.Context, string, int, uint64, int, uint64) (CanaryProcessSample, error) {
		processProbes.Add(1)
		return processObserverSample(time.Now().UTC()), nil
	}
	crashAt := time.Now().UTC()
	observer, err := h.StartPausedCanaryProcessObservation(context.Background(), "abc", processObserverInitial(), crashAt)
	if err != nil {
		t.Fatalf("StartPausedCanaryProcessObservation() error = %v", err)
	}
	h.deletePodFn = func(ctx context.Context, _, _, _, _ string) error {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for processProbes.Load() < 2 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("observer did not sample during delayed UID delete: %w", ctx.Err())
			case <-ticker.C:
			}
		}
		deleted.Store(true)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := h.crashPod(ctx, "loom-hub", "app=mobile-hud", "mobile-hud",
		before, beforeDeployment, nil, observer.Activate, nil); err != nil {
		t.Fatalf("crashPod() error = %v", err)
	}
	select {
	case <-observer.done:
	case <-ctx.Done():
		t.Fatal("process observer did not finish after exact pod disappearance")
	}
	ev := Evidence{SpawnPodName: "spawn-abc", CrashBAt: crashAt, CanaryHoldInitial: processObserverInitial()}
	if err := observer.Record(&ev); err != nil {
		t.Fatalf("observer evidence across delayed delete failed: %v", err)
	}
	if processProbes.Load() < 2 {
		t.Fatalf("process probes = %d, want a sample during delayed UID delete", processProbes.Load())
	}
}

func TestCrashPodWithLeaseRenewsImmediatelyBeforeDelete(t *testing.T) {
	var renewed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/mills/safety/crash-lease/lease-token/renew" {
			http.NotFound(w, r)
			return
		}
		renewed.Store(true)
		_, _ = fmt.Fprintf(w, `{"token":"lease-token","request_id":"request-1","run_id":"wf-canary-1","spawn_id":"abc","expires_at":%q}`,
			time.Now().UTC().Add(90*time.Second).Format(time.RFC3339Nano))
	}))
	defer server.Close()

	deploymentJSON := controllerDeploymentFixtureJSON("operator", "loom-mills", "operator-deployment-uid", "operator:v1")
	beforeDeployment := mustStableDeploymentFixture(deploymentJSON)
	before := controllerPodFixture(
		"operator-old", beforeDeployment.Namespace, "uid-old", beforeDeployment.Image, "operator@sha256:abc",
		beforeDeployment.Name, beforeDeployment.UID, time.Date(2026, time.July, 12, 0, 0, 0, 0, time.UTC),
	)
	before = bindControllerPodFixture(before, beforeDeployment)
	after := replacementControllerPodFixture(before, "operator-new", "uid-new", time.Now().UTC())
	h := New(Config{OperatorURL: server.URL, AdminToken: "admin"})
	configureControllerPodDryRunFixture(t, h)
	deleted := false
	h.deletePodFn = func(context.Context, string, string, string, string) error {
		if !renewed.Load() {
			t.Fatal("UID delete ran before lease renewal")
		}
		deleted = true
		return nil
	}
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		command := strings.Join(args, " ")
		switch {
		case strings.Contains(command, "get deploy"):
			return deploymentJSON, nil
		case strings.Contains(command, controllerPodCensusPath(before.Namespace)) && !deleted:
			return controllerPodListFixtureJSON(before), nil
		case strings.Contains(command, controllerPodCensusPath(before.Namespace)):
			return controllerPodListFixtureJSON(after), nil
		case strings.Contains(command, "get replicaset") && !deleted:
			return controllerReplicaSetFixtureJSON(before, deploymentJSON), nil
		case strings.Contains(command, "get replicaset"):
			return controllerReplicaSetFixtureJSON(after, deploymentJSON), nil
		default:
			return "", nil
		}
	}
	if _, err := h.CrashPodWithLease(context.Background(), "loom-mills", "app=operator", "operator",
		before, beforeDeployment, testAcquiredCrashLease()); err != nil {
		t.Fatalf("CrashPodWithLease() error = %v", err)
	}
}

func TestCrashPodWithLeaseFinalCheckFailurePreventsDelete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"token":"lease-token","request_id":"request-1","run_id":"wf-canary-1","spawn_id":"abc","expires_at":%q}`,
			time.Now().UTC().Add(90*time.Second).Format(time.RFC3339Nano))
	}))
	defer server.Close()

	deploymentJSON := controllerDeploymentFixtureJSON("operator", "loom-mills", "operator-deployment-uid", "operator:v1")
	beforeDeployment := mustStableDeploymentFixture(deploymentJSON)
	before := controllerPodFixture(
		"operator-old", beforeDeployment.Namespace, "uid-old", beforeDeployment.Image, "operator@sha256:abc",
		beforeDeployment.Name, beforeDeployment.UID, time.Date(2026, time.July, 12, 0, 0, 0, 0, time.UTC),
	)
	before = bindControllerPodFixture(before, beforeDeployment)
	h := New(Config{OperatorURL: server.URL, AdminToken: "admin"})
	configureControllerPodDryRunFixture(t, h)
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		command := strings.Join(args, " ")
		switch {
		case strings.Contains(command, "get deploy"):
			return deploymentJSON, nil
		case strings.Contains(command, controllerPodCensusPath(before.Namespace)):
			return controllerPodListFixtureJSON(before), nil
		case strings.Contains(command, "get replicaset"):
			return controllerReplicaSetFixtureJSON(before, deploymentJSON), nil
		default:
			return "", nil
		}
	}
	h.deletePodFn = func(context.Context, string, string, string, string) error {
		t.Fatal("UID delete ran after final workload check failed")
		return nil
	}
	want := errors.New("foreground hold exited")
	_, err := h.CrashPodWithLeaseAndHooks(context.Background(), "loom-mills", "app=operator", "operator",
		before, beforeDeployment, testAcquiredCrashLease(), func(context.Context) error { return want }, func() error {
			t.Fatal("delete-start hook ran after final workload check failed")
			return nil
		})
	if !errors.Is(err, want) {
		t.Fatalf("CrashPodWithLeaseAndCheck() error = %v, want wrapped final-check error", err)
	}

	h.finalPreDeleteCheckTimeout = 10 * time.Millisecond
	_, err = h.CrashPodWithLeaseAndHooks(context.Background(), "loom-mills", "app=operator", "operator",
		before, beforeDeployment, testAcquiredCrashLease(), func(checkCtx context.Context) error {
			<-checkCtx.Done()
			return nil // model a callback that swallows context cancellation
		}, func() error {
			t.Fatal("delete-start hook ran after final workload check exceeded its deadline")
			return nil
		})
	if err == nil || !strings.Contains(err.Error(), "final workload check exceeded") ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired successful callback authorized delete: %v", err)
	}
}

func TestCrashPodWithLeaseDeleteStartHookRunsBeforeUIDDeleteFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"token":"lease-token","request_id":"request-1","run_id":"wf-canary-1","spawn_id":"abc","expires_at":%q}`,
			time.Now().UTC().Add(90*time.Second).Format(time.RFC3339Nano))
	}))
	defer server.Close()

	deploymentJSON := controllerDeploymentFixtureJSON("operator", "loom-mills", "operator-deployment-uid", "operator:v1")
	beforeDeployment := mustStableDeploymentFixture(deploymentJSON)
	before := controllerPodFixture(
		"operator-old", beforeDeployment.Namespace, "uid-old", beforeDeployment.Image, "operator@sha256:abc",
		beforeDeployment.Name, beforeDeployment.UID, time.Date(2026, time.July, 12, 0, 0, 0, 0, time.UTC),
	)
	before = bindControllerPodFixture(before, beforeDeployment)
	h := New(Config{OperatorURL: server.URL, AdminToken: "admin"})
	configureControllerPodDryRunFixture(t, h)
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		command := strings.Join(args, " ")
		switch {
		case strings.Contains(command, "get deploy"):
			return deploymentJSON, nil
		case strings.Contains(command, controllerPodCensusPath(before.Namespace)):
			return controllerPodListFixtureJSON(before), nil
		case strings.Contains(command, "get replicaset"):
			return controllerReplicaSetFixtureJSON(before, deploymentJSON), nil
		default:
			return "", nil
		}
	}
	want := errors.New("UID precondition failed")
	h.deletePodFn = func(context.Context, string, string, string, string) error { return want }
	var hookRan atomic.Bool
	_, err := h.CrashPodWithLeaseAndHooks(context.Background(), "loom-mills", "app=operator", "operator",
		before, beforeDeployment, testAcquiredCrashLease(), nil, func() error {
			hookRan.Store(true)
			return nil
		})
	if !errors.Is(err, want) {
		t.Fatalf("CrashPodWithLeaseAndHooks() error = %v, want UID delete failure", err)
	}
	if !hookRan.Load() {
		t.Fatal("delete-start hook did not run before UID delete failure")
	}
}

func testAcquiredCrashLease() CrashLease {
	now := time.Now().UTC()
	return CrashLease{
		Token: "lease-token", RequestID: "request-1", RunID: "wf-canary-1", SpawnID: "abc",
		ObservedAt: now.Add(-time.Second), ExpiresAt: now.Add(2 * time.Minute),
	}
}

func TestRenewedCrashLeaseIdentityAndDeleteBoundary(t *testing.T) {
	acquired := testAcquiredCrashLease()
	renewed := acquired
	renewed.ObservedAt = acquired.ObservedAt.Add(500 * time.Millisecond)
	renewed.ExpiresAt = time.Now().UTC().Add(90 * time.Second)
	if err := validateRenewedCrashLeaseIdentity(acquired, renewed); err != nil {
		t.Fatalf("valid renewed identity rejected: %v", err)
	}
	for name, mutate := range map[string]func(*CrashLease){
		"request": func(lease *CrashLease) { lease.RequestID = "other-request" },
		"run":     func(lease *CrashLease) { lease.RunID = "other-run" },
		"spawn":   func(lease *CrashLease) { lease.SpawnID = "other-spawn" },
		"token":   func(lease *CrashLease) { lease.Token = "other-token" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := renewed
			mutate(&changed)
			if err := validateRenewedCrashLeaseIdentity(acquired, changed); err == nil {
				t.Fatal("renewed lease identity drift accepted")
			}
		})
	}

	deleteAt := time.Now().UTC()
	renewed.ObservedAt = deleteAt.Add(-time.Second)
	renewed.ExpiresAt = deleteAt.Add(podDeleteRequestTimeout + crashLeaseDeleteSafetyMargin)
	if err := validateRenewedLeaseAtDelete(renewed, deleteAt); err != nil {
		t.Fatalf("lease with exact required delete window rejected: %v", err)
	}
	renewed.ExpiresAt = renewed.ExpiresAt.Add(-time.Nanosecond)
	if err := validateRenewedLeaseAtDelete(renewed, deleteAt); err == nil || !strings.Contains(err.Error(), "at delete boundary") {
		t.Fatalf("lease expiring inside delete window accepted: %v", err)
	}
}

func TestCollectDedupeEvidenceAttributesExactReplacementPodAndTime(t *testing.T) {
	crashA := time.Now().UTC().Add(-time.Minute)
	emitted := crashA.Add(time.Second)
	h := New(Config{})
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		cmd := strings.Join(args, " ")
		if !strings.Contains(cmd, `pod%3D%22operator-new%22`) && !strings.Contains(cmd, `pod%3D%5C%22operator-new%5C%22`) {
			// The encoded query format is implementation-specific; the exact
			// pod is also visible once the URL is decoded.
			if decoded, _ := url.QueryUnescape(cmd); !strings.Contains(decoded, `pod="operator-new"`) {
				t.Fatalf("Loki query was not pinned to replacement pod: %s", cmd)
			}
		}
		return fmt.Sprintf(`{"status":"success","data":{"result":[{"stream":{"namespace":"loom-mills","pod":"operator-new"},"values":[[%q,%q]]}]}}`,
			strconv.FormatInt(emitted.UnixNano(), 10), `workflow resume: re-attaching to in-flight spawn spawn_id=abc123`), nil
	}
	evidence, err := h.CollectDedupeEvidence(context.Background(), "abc123", crashA,
		PodIdentity{Name: "operator-new"}, time.Now().UTC(), PodIdentity{Name: "hud-new"})
	if err != nil || evidence.Pod != "operator-new" || evidence.Component != "operator" || !evidence.Timestamp.Equal(emitted) {
		t.Fatalf("CollectDedupeEvidence() = %+v, %v", evidence, err)
	}
	old := []LogEvidence{{Pod: "operator-new", Timestamp: crashA.Add(-time.Second), Line: evidence.Line}}
	if got := findDedupeEvidence(old, []string{"re-attaching"}, "abc123", crashA); got != nil {
		t.Fatalf("pre-crash evidence accepted: %+v", got)
	}
}

func TestAwaitTerminalFailsOnObservationError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"run":{"id":"wf-1","state":"done"},"steps":[]}`))
	}))
	defer server.Close()
	h := New(Config{OperatorURL: server.URL, PollInterval: time.Millisecond})
	h.kubectlFn = func(_ context.Context, _ ...string) (string, error) {
		return "", fmt.Errorf("apiserver unavailable")
	}
	ev := Evidence{}
	err := h.AwaitTerminal(context.Background(), "wf-1", "spawn-1", &ev)
	if err == nil || len(ev.ObservationErrors) == 0 {
		t.Fatalf("AwaitTerminal() err=%v observation_errors=%v", err, ev.ObservationErrors)
	}
}
