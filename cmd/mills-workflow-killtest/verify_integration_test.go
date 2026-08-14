package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/worker"
	"github.com/crb2nu/loom/pkg/mills/workflow"
	"github.com/crb2nu/loom/pkg/mills/workflow/killtest"
)

// TestVerifyGateEvidenceUsesProductionEvaluator closes the verifier test seam:
// these artifacts contain the full current S1c evidence contract and are
// accepted by verifyGateEvidence through killtest.Evaluate itself.
func TestVerifyGateEvidenceUsesProductionEvaluator(t *testing.T) {
	t.Parallel()
	const gateID = "fedcba9876543210fedcba9876543210"
	base := time.Date(2026, time.July, 14, 15, 0, 0, 0, time.UTC)
	gateStartedAt := base.Add(-10 * time.Second)
	summaryPath := filepath.Join(t.TempDir(), "s1c-production-evidence.json")
	summary := gateSummary{
		GateContract:        killtest.GateBindingContract,
		GateContractVersion: killtest.GateBindingContractVersion,
		GateID:              gateID,
		GateStartedAt:       gateStartedAt,
		RequiredRuns:        killtest.S1cGateRequiredRuns,
		CompletedRuns:       killtest.S1cGateRequiredRuns,
		Overall:             true,
		AgentType:           killtest.AgentTypeClaudeCode,
	}

	previousEvidenceSHA256 := ""
	var baseline killtest.PreflightReport
	for index := 1; index <= killtest.S1cGateRequiredRuns; index++ {
		evidence := productionVerifierPassingEvidence(t, index,
			base.Add(time.Duration(index-1)*time.Minute), gateID, gateStartedAt, previousEvidenceSHA256)
		verdicts := killtest.Evaluate(evidence)
		if !verdicts.Overall {
			t.Fatalf("run %d production evaluator rejected fixture: %+v", index, verdicts)
		}
		finalPreflight := evidence.FinalPreflight
		output := runOutput{
			Preflight: evidence.InitialPreflight, FinalPreflight: &finalPreflight,
			Evidence: evidence, Verdicts: verdicts,
		}
		path := runEvidencePath(summaryPath, index, killtest.S1cGateRequiredRuns, true)
		if err := writeJSON(path, output); err != nil {
			t.Fatalf("write run %d: %v", index, err)
		}
		digest, err := evidenceFileSHA256(path)
		if err != nil {
			t.Fatalf("hash run %d: %v", index, err)
		}
		summary.Runs = append(summary.Runs, gateRunSummary{
			Index: index, EvidencePath: path, EvidenceSHA256: digest,
			PreviousEvidenceSHA256: previousEvidenceSHA256,
			RunID:                  evidence.RunID, AgentType: evidence.AgentType,
			FinalState: evidence.Final.Run.State, Overall: verdicts.Overall,
		})
		previousEvidenceSHA256 = digest
		if index == 1 {
			baseline = evidence.InitialPreflight
		}
	}
	summary.OperatorImage = baseline.Operator.ImageID
	summary.HudImage = baseline.Hud.ImageID
	summary.PolicyChecksum = baseline.PolicyChecksum
	summary.GitOpsIdentityMode = baseline.GitOpsIdentity.Mode
	summary.GitOpsBaseline = baseline.GitOpsIdentity.BaselineRevision
	summary.GitOpsScopeDigest = baseline.GitOpsIdentity.ObservedDigest
	summary.LoomCoreBaseline = baseline.LoomCoreIdentity.BaselineRevision
	summary.LoomCoreScopeDigest = baseline.LoomCoreIdentity.ObservedDigest
	if err := writeJSON(summaryPath, summary); err != nil {
		t.Fatalf("write summary: %v", err)
	}

	if err := verifyGateEvidence(summaryPath); err != nil {
		t.Fatalf("verifyGateEvidence() error = %v", err)
	}
}

func productionVerifierPassingEvidence(
	t *testing.T,
	runIndex int,
	crashA time.Time,
	gateID string,
	gateStartedAt time.Time,
	previousEvidenceSHA256 string,
) killtest.Evidence {
	t.Helper()
	const (
		stepKey  = "root/agent#0"
		callHash = "crash-critical-call-hash"
	)
	runID, err := killtest.CanaryRunIDForGate(gateID, runIndex)
	if err != nil {
		t.Fatal(err)
	}
	crashB := crashA.Add(time.Second)
	expectedKey := workflow.DeriveStepIdempotencyKeyFromHash(runID, stepKey, callHash)
	spawnID := worker.DeriveSpawnID(expectedKey)
	podName := "spawn-" + spawnID
	dedupeLine := fmt.Sprintf(`msg="workflow resume: re-attaching to in-flight spawn" spawn_id=%s`, spawnID)
	operatorBefore := killtest.PodIdentity{
		Name: "operator-old", UID: "operator-uid-old", Node: "worker-1",
		Image: "registry/operator:v1", ImageID: "registry/operator@sha256:aaa",
		StartedAt: crashA.Add(-time.Hour),
	}
	if runIndex > 1 {
		operatorBefore.Name = fmt.Sprintf("operator-new-%d", runIndex-1)
		operatorBefore.UID = fmt.Sprintf("operator-uid-new-%d", runIndex-1)
		operatorBefore.StartedAt = crashA.Add(-time.Minute + 100*time.Millisecond)
	}
	operatorAfter := operatorBefore
	operatorAfter.Name, operatorAfter.UID, operatorAfter.StartedAt =
		fmt.Sprintf("operator-new-%d", runIndex), fmt.Sprintf("operator-uid-new-%d", runIndex), crashA.Add(100*time.Millisecond)
	hudBefore := killtest.PodIdentity{
		Name: "hud-old", UID: "hud-uid-old", Node: "worker-2",
		Image: "registry/mobile-hud:v1", ImageID: "registry/mobile-hud@sha256:bbb",
		StartedAt: crashA.Add(-time.Hour),
	}
	if runIndex > 1 {
		hudBefore.Name = fmt.Sprintf("hud-new-%d", runIndex-1)
		hudBefore.UID = fmt.Sprintf("hud-uid-new-%d", runIndex-1)
		hudBefore.StartedAt = crashA.Add(-time.Minute + 1100*time.Millisecond)
	}
	hudAfter := hudBefore
	hudAfter.Name, hudAfter.UID, hudAfter.StartedAt =
		fmt.Sprintf("hud-new-%d", runIndex), fmt.Sprintf("hud-uid-new-%d", runIndex), crashB.Add(100*time.Millisecond)
	identity := killtest.SpawnIdentity{SpawnID: spawnID, PodName: podName, IdempotencyKey: expectedKey}
	pendingStep := killtest.StepView{
		StepKey: stepKey, EventType: "spawn_requested", Status: "pending", CallHash: callHash,
	}
	initialPreflight := productionVerifierPreflight(t, crashA.Add(-5*time.Second), "", operatorBefore, hudBefore)
	crashAPreflight := productionVerifierPreflight(t, crashA.Add(-600*time.Millisecond), runID, operatorBefore, hudBefore)
	crashBPreflight := productionVerifierPreflight(t, crashA.Add(300*time.Millisecond), runID, operatorAfter, hudBefore)
	finalPreflight := productionVerifierPreflight(t, crashB.Add(5*time.Second), "", operatorAfter, hudAfter)
	operatorBefore = initialPreflight.Operator
	operatorAfter = crashBPreflight.Operator
	hudBefore = initialPreflight.Hud
	hudAfter = finalPreflight.Hud
	crashAFence := productionVerifierFluxFence(t, crashA.Add(-500*time.Millisecond), crashA.Add(-100*time.Millisecond))
	crashBFence := productionVerifierFluxFence(t, crashA.Add(400*time.Millisecond), crashA.Add(800*time.Millisecond))
	return killtest.Evidence{
		GateBinding: killtest.GateBinding{
			Contract: killtest.GateBindingContract, ContractVersion: killtest.GateBindingContractVersion,
			GateID: gateID, RunIndex: runIndex, RequiredRuns: killtest.S1cGateRequiredRuns,
			GateStartedAt: gateStartedAt, PreviousEvidenceSHA256: previousEvidenceSHA256,
		},
		RunID: runID, AgentType: killtest.AgentTypeClaudeCode,
		AgentStepKey: stepKey, SpawnID: spawnID, SpawnPodName: podName,
		ExpectedIdempotencyKey: expectedKey, MaxConcurrentSpawnPods: 1,
		TotalSpawnPodNames:      []string{podName},
		SpawnPodWatchStartedAt:  crashA.Add(-4 * time.Second),
		CanaryLaunchRequestedAt: crashA.Add(-3900 * time.Millisecond),
		SpawnPodWatchEndedAt:    crashB.Add(2 * time.Minute), SpawnPodWatchInitialRV: "100",
		SpawnPodWatchContinuous: true,
		TotalSpawnPodIncarnations: []killtest.PodIdentity{{
			Name: podName, UID: "spawn-uid-1", Node: "node-1", Image: "spawn:v1",
			ImageID: "spawn@sha256:123", StartedAt: crashA.Add(-time.Minute),
		}},
		SpawnPodWatchEvents: []killtest.SpawnPodWatchEvent{{
			Type: "ADDED", ResourceVersion: "101", ObservedAt: crashA.Add(-3500 * time.Millisecond),
			Pod: killtest.PodIdentity{
				Name: podName, UID: "spawn-uid-1", Node: "node-1", Image: "spawn:v1",
				ImageID: "spawn@sha256:123", StartedAt: crashA.Add(-time.Minute),
			},
			SpawnIDLabel: &spawnID,
		}},
		CanaryHoldInitial: killtest.CanaryHoldObservation{
			PodName: podName, PID: 42, StartTimeTicks: 4200,
			DriverPID: 41, DriverStartTimeTicks: 4100, Seconds: workflow.CanaryHoldSeconds,
			ObservedAt: crashA.Add(-3 * time.Second),
		},
		CanaryHoldBeforeCrashA: killtest.CanaryHoldObservation{
			PodName: podName, PID: 42, StartTimeTicks: 4200,
			DriverPID: 41, DriverStartTimeTicks: 4100, Seconds: workflow.CanaryHoldSeconds,
			ObservedAt: crashA.Add(-225 * time.Millisecond),
		},
		CanaryHoldBeforeCrashB: killtest.CanaryHoldObservation{
			PodName: podName, PID: 42, StartTimeTicks: 4200,
			DriverPID: 41, DriverStartTimeTicks: 4100, Seconds: workflow.CanaryHoldSeconds,
			ObservedAt: crashA.Add(700 * time.Millisecond),
		},
		ProcessObservationStartedAt: crashA.Add(-200 * time.Millisecond),
		PostCrashProcessSamples: []killtest.CanaryProcessSample{{
			PodName: podName, ObservedAt: crashA.Add(-190 * time.Millisecond),
			CompletedAt: crashA.Add(-180 * time.Millisecond),
			HoldPID:     42, HoldStartTimeTicks: 4200, HoldState: "S",
			DriverPID: 41, DriverStartTimeTicks: 4100, DriverState: "S",
			LiveHoldPIDs: []int{42}, LiveDriverPIDs: []int{41}, ZombiePIDs: []int{},
		}},
		PostCrashProcessObservedEnd: crashB.Add(time.Second),
		PostCrashProcessMaxGapMS:    killtest.ProcessEvidenceMaxSampleGap.Milliseconds(),
		CrashAProcessAuthorization: killtest.ProcessDeleteAuthorization{
			SampleIndex: 0, SampleObservedAt: crashA.Add(-190 * time.Millisecond),
			SampleCompletedAt: crashA.Add(-180 * time.Millisecond), AuthorizedAt: crashA,
		},
		CrashBProcessAuthorization: killtest.ProcessDeleteAuthorization{
			SampleIndex: 0, SampleObservedAt: crashA.Add(-190 * time.Millisecond),
			SampleCompletedAt: crashA.Add(-180 * time.Millisecond), AuthorizedAt: crashB,
		},
		TotalSpawnRecordIDs:       []string{spawnID},
		FinalSpawnRecordStatuses:  map[string]string{spawnID: "completed"},
		FinalSpawnIdempotencyKeys: map[string]string{spawnID: expectedKey},
		DedupeEvidence:            dedupeLine,
		DedupeLog: &killtest.LogEvidence{
			Component: "operator", Namespace: "loom-mills", Pod: operatorAfter.Name,
			Timestamp: crashA.Add(time.Second), Line: dedupeLine,
		},
		CrashAAt: crashA, CrashBAt: crashB,
		CrashAFluxProvenance: crashAFence, CrashBFluxProvenance: crashBFence,
		InitialPreflight: initialPreflight, FinalPreflight: finalPreflight,
		CrashASafety: productionVerifierCrashSafety(
			runID, killtest.AgentTypeClaudeCode, pendingStep, identity, crashAPreflight,
			crashA.Add(-450*time.Millisecond), crashA.Add(-350*time.Millisecond),
			crashA.Add(-250*time.Millisecond), crashA,
		),
		CrashBSafety: productionVerifierCrashSafety(
			runID, killtest.AgentTypeClaudeCode, pendingStep, identity, crashBPreflight,
			crashA.Add(450*time.Millisecond), crashA.Add(550*time.Millisecond),
			crashA.Add(650*time.Millisecond), crashB,
		),
		CrashABefore: operatorBefore, CrashAReplacement: operatorAfter,
		CrashBBefore: hudBefore, CrashBReplacement: hudAfter,
		Final: killtest.RunDetail{
			OperatorAuthority: finalPreflight.AuthorityPlane.Operator,
			Run: killtest.RunSummary{
				ID: runID, Engine: "imperative", State: "done", AgentType: killtest.AgentTypeClaudeCode,
				Template: workflow.CanaryTemplateName, TemplateVersion: workflow.CanaryTemplateVersion,
				InterpreterVersion: workflow.HostInterpreterVersion,
			},
			Steps: []killtest.StepView{
				{StepKey: stepKey, EventType: "spawn_result", Status: "success",
					SpawnID: spawnID, CallHash: callHash, CostSource: "real", CostUSD: 0.42, EffectCount: 1},
				{StepKey: "root/gate#0", EventType: "gate_eval", Status: "success"},
			},
		},
	}
}

func productionVerifierFluxFence(t *testing.T, preparedAt, finalAt time.Time) killtest.FluxSourceFenceEvidence {
	t.Helper()
	platformRevision := strings.Repeat("a", 40)
	loomCoreRevision := strings.Repeat("b", 40)
	platformDigest := strings.Repeat("1", 64)
	loomCoreDigest := strings.Repeat("2", 64)
	platformIdentity := killtest.GitOpsScopeIdentity{
		Mode: killtest.GitOpsIdentityModeProtectedScope, Contract: "platform-gitops", ContractVersion: 1,
		BaselineRevision: platformRevision, ObservedRevision: platformRevision,
		BaselineDigest: platformDigest, ObservedDigest: platformDigest, CheckedCommitCount: 1,
	}
	loomCoreIdentity := killtest.GitOpsScopeIdentity{
		Mode: killtest.GitOpsIdentityModeProtectedScope, Contract: "loom-core-source", ContractVersion: 1,
		BaselineRevision: loomCoreRevision, ObservedRevision: loomCoreRevision,
		BaselineDigest: loomCoreDigest, ObservedDigest: loomCoreDigest, CheckedCommitCount: 1,
	}
	snapshot := func(listResourceVersion string, observedAt time.Time) killtest.FluxSourceProvenanceSnapshot {
		platformApplied := "main@sha1:" + platformRevision
		loomCoreApplied := "main@sha1:" + loomCoreRevision
		return killtest.FluxSourceProvenanceSnapshot{
			Contract: killtest.FluxProvenanceContract, ContractVersion: killtest.FluxProvenanceContractVersion,
			ListResourceVersion:     listResourceVersion,
			GitRepositoriesOpenedAt: observedAt.Add(-time.Nanosecond),
			ObservedAt:              observedAt,
			GitRepositories: verifierGitRepositorySnapshot(
				"repos-"+listResourceVersion, observedAt, platformIdentity, loomCoreIdentity,
			),
			Sources: []killtest.FluxSourceProvenance{
				productionVerifierFluxSource(t, "apps", "apps-uid", "101", 7,
					platformApplied, platformIdentity, platformIdentity),
				productionVerifierFluxSource(t, "bootstrap", "bootstrap-uid", "151", 5,
					platformApplied, platformIdentity, platformIdentity),
				productionVerifierFluxSource(t, "system", "system-uid", "181", 9,
					platformApplied, platformIdentity, platformIdentity),
				productionVerifierFluxSource(t, "loom-hub-servers", "loom-core-uid", "202", 11,
					loomCoreApplied, loomCoreIdentity, platformIdentity),
			},
		}
	}
	return killtest.FluxSourceFenceEvidence{
		Prepared: snapshot("9001", preparedAt), Final: snapshot("9002", finalAt),
	}
}

func verifierGitRepositorySnapshot(
	listResourceVersion string,
	observedAt time.Time,
	platformIdentity, loomCoreIdentity killtest.GitOpsScopeIdentity,
) killtest.GitRepositoryProvenanceSnapshot {
	repository := func(
		name, uid, resourceVersion, revision, digest string,
		generation int64,
		identity killtest.GitOpsScopeIdentity,
	) killtest.GitRepositoryProvenance {
		return killtest.GitRepositoryProvenance{
			Name: name, Namespace: "flux-system", UID: uid,
			ResourceVersion: resourceVersion, Generation: generation,
			StatusObservedGeneration: generation,
			ReadyObservedGeneration:  generation, ReadyStatus: "True",
			ArtifactInStorageObservedGeneration: generation, ArtifactInStorageStatus: "True",
			ArtifactRevision:  "main@sha1:" + revision,
			ArtifactDigest:    "sha256:" + digest,
			Spec:              verifierGitRepositorySpec(name, platformIdentity),
			ProtectedIdentity: identity,
		}
	}
	return killtest.GitRepositoryProvenanceSnapshot{
		ListResourceVersion: listResourceVersion, ObservedAt: observedAt,
		Repositories: []killtest.GitRepositoryProvenance{
			repository("gitops-gitlab", "gitops-repo-uid", "501",
				platformIdentity.ObservedRevision, strings.Repeat("c", 64), 7, platformIdentity),
			repository("loom-core", "loom-core-repo-uid", "502",
				loomCoreIdentity.ObservedRevision, strings.Repeat("d", 64), 3, loomCoreIdentity),
		},
	}
}

func verifierGitRepositorySpec(
	name string,
	platformIdentity killtest.GitOpsScopeIdentity,
) killtest.GitRepositorySpecIdentity {
	identity := killtest.GitRepositorySpecIdentity{
		RefBranch: "main", SecretRefName: "gitops-gitlab",
		ReviewedRevision:    platformIdentity.BaselineRevision,
		ReviewedScopeDigest: platformIdentity.BaselineDigest,
	}
	var raw string
	switch name {
	case "gitops-gitlab":
		identity.URL = "http://gitlab-vm.gitlab.svc.cluster.local/platform/gitops.git"
		identity.ManifestPath = "clusters/k3s/flux-system/gitrepository-gitlab.yaml"
		raw = `{"interval":"1m","timeout":"120s","url":"http://gitlab-vm.gitlab.svc.cluster.local/platform/gitops.git","ref":{"branch":"main"},"secretRef":{"name":"gitops-gitlab"}}`
	case "loom-core":
		identity.URL = "http://gitlab-vm.gitlab.svc.cluster.local/services/loom-core.git"
		identity.ManifestPath = "clusters/k3s/flux-system/gitrepository-loom-core.yaml"
		raw = `{"interval":"1m","timeout":"120s","url":"http://gitlab-vm.gitlab.svc.cluster.local/services/loom-core.git","ref":{"branch":"main"},"secretRef":{"name":"gitops-gitlab"}}`
	default:
		panic("unknown verifier GitRepository " + name)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var canonical any
	if err := decoder.Decode(&canonical); err != nil {
		panic(err)
	}
	blob, err := json.Marshal(canonical)
	if err != nil {
		panic(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(blob))
	identity.SpecSHA256 = digest
	identity.ReviewedSpecSHA256 = digest
	return identity
}

func productionVerifierFluxSource(
	t *testing.T,
	name, uid, resourceVersion string,
	generation int64,
	applied string,
	identity, platformIdentity killtest.GitOpsScopeIdentity,
) killtest.FluxSourceProvenance {
	t.Helper()
	return killtest.FluxSourceProvenance{
		Name: name, UID: uid, ResourceVersion: resourceVersion, Generation: generation,
		ReadyObservedGeneration: generation, ReadyStatus: "True",
		AppliedRevision: applied, AttemptedRevision: applied,
		RenderSpec:        productionVerifierRenderSpec(t, name, platformIdentity),
		ProtectedIdentity: identity,
	}
}

func productionVerifierRenderSpec(
	t *testing.T,
	name string,
	platformIdentity killtest.GitOpsScopeIdentity,
) killtest.FluxRenderSpecIdentity {
	t.Helper()
	var raw string
	identity := killtest.FluxRenderSpecIdentity{
		SourceRefKind: "GitRepository", SourceRefNamespace: "flux-system",
		ReviewedRevision:    platformIdentity.BaselineRevision,
		ReviewedScopeDigest: platformIdentity.BaselineDigest,
	}
	switch name {
	case "apps", "bootstrap", "system":
		identity.Path = "./k3s/flux/" + name
		identity.SourceRefName = "gitops-gitlab"
		identity.ManifestPath = "clusters/k3s/flux-system/kustomization-" + name + ".yaml"
		raw = fmt.Sprintf(`{"path":%q,"sourceRef":{"kind":"GitRepository","name":"gitops-gitlab"},"force":false}`, identity.Path)
	case "loom-hub-servers":
		identity.Path = "./k8s/base"
		identity.SourceRefName = "loom-core"
		identity.TargetNamespace = "loom-hub"
		identity.ManifestPath = "clusters/k3s/flux-system/kustomization-loom-hub-servers.yaml"
		raw = `{"path":"./k8s/base","targetNamespace":"loom-hub","sourceRef":{"kind":"GitRepository","name":"loom-core"},"force":false}`
	default:
		t.Fatalf("unknown Flux owner %q", name)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var canonical any
	if err := decoder.Decode(&canonical); err != nil {
		t.Fatalf("decode %s Flux spec: %v", name, err)
	}
	blob, err := json.Marshal(canonical)
	if err != nil {
		t.Fatalf("canonicalize %s Flux spec: %v", name, err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(blob))
	identity.SpecSHA256 = digest
	identity.ReviewedSpecSHA256 = digest
	return identity
}

func productionVerifierPreflight(
	t *testing.T,
	observedAt time.Time,
	runID string,
	operator, hud killtest.PodIdentity,
) killtest.PreflightReport {
	t.Helper()
	flux := productionVerifierFluxFence(t, observedAt.Add(-100*time.Millisecond), observedAt)
	start, end := flux.Prepared, flux.Final
	startSources := productionVerifierFluxByName(start)
	endSources := productionVerifierFluxByName(end)
	report := killtest.PreflightReport{
		FluxSourcesStart: start, FluxSourcesEnd: end, NamespacesOK: true,
		OperatorImage: operator.Image, Operator: operator,
		OperatorDeployment: killtest.DeploymentIdentity{
			Name: "loom-mills-operator", Generation: 7, ObservedGeneration: 7,
			DesiredReplicas: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
			Image: operator.Image, Strategy: "Recreate", PolicyChecksum: "policy-sha256",
		},
		HudImage: hud.Image, Hud: hud,
		HudDeployment: killtest.DeploymentIdentity{
			Name: "mobile-hud", Generation: 9, ObservedGeneration: 9,
			DesiredReplicas: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
			Image: hud.Image, Strategy: "Recreate",
		},
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
		PolicyConfigMapIdentity: killtest.KubernetesObjectIdentity{
			Name: "loom-mills-policy", Namespace: "loom-mills", UID: "policy-config-uid", ResourceVersion: "40",
		},
		WorkflowsFlag:          "true",
		ConfigMapPolicyEnabled: false, FlagEnabled: true, SubstrateK8sOnly: true,
		EffectivePolicyEnabled: false, EffectiveFlagEnabled: true,
		EffectiveSubstrateK8sOnly: true, EffectivePolicyMatchesConfigMap: true,
		SpawnConfigMap: true, SpawnConfigMapUID: "spawn-state-uid",
		SpawnConfigMapIdentity: killtest.KubernetesObjectIdentity{
			Name: "loom-spawn-state", Namespace: "devbox", UID: "spawn-state-uid", ResourceVersion: "50",
		},
		SpawnConfigMapUpdateAllowed: true,
		Quiescence:                  productionVerifierQuiescence(observedAt.Add(-50*time.Millisecond), runID, false),
		LokiReady:                   true, AllPreconditions: true,
	}
	bindVerifierDeploymentProvenance(&report)
	return report
}

func productionVerifierFluxByName(snapshot killtest.FluxSourceProvenanceSnapshot) map[string]killtest.FluxSourceProvenance {
	result := make(map[string]killtest.FluxSourceProvenance, len(snapshot.Sources))
	for _, source := range snapshot.Sources {
		result[source.Name] = source
	}
	return result
}

func productionVerifierQuiescence(observedAt time.Time, runID string, crashLease bool) killtest.QuiescenceSnapshot {
	workflowActive := int64(0)
	workflowRuns := 0
	var runIDs []string
	if runID != "" {
		workflowActive = 1
		workflowRuns = 1
		runIDs = []string{runID}
	}
	return killtest.QuiescenceSnapshot{
		ObservedAt: observedAt, Quiescent: runID == "" && !crashLease,
		Counts: killtest.QuiescenceCounts{ActiveWorkflowRuns: workflowRuns},
		InMemory: killtest.QuiescenceInMemoryActivity{
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

func productionVerifierCrashSafety(
	runID, agentType string,
	step killtest.StepView,
	identity killtest.SpawnIdentity,
	immediate killtest.PreflightReport,
	acquiredAt, renewedAt, targetAt, deleteAt time.Time,
) killtest.CrashSafetyEvidence {
	requestID := "s1c-lease-" + deleteAt.Format("150405.000000000")
	evidence := killtest.CrashSafetyEvidence{
		ImmediatePreflight: immediate,
		Target: killtest.CrashTargetSafetyEvidence{
			Quiescence:            productionVerifierQuiescence(targetAt.Add(-10*time.Millisecond), runID, true),
			QuiescenceCollectedAt: targetAt.Add(-5 * time.Millisecond),
			Run: killtest.RunSummary{
				ID: runID, Engine: "imperative", Template: workflow.CanaryTemplateName,
				AgentType: agentType, TemplateVersion: workflow.CanaryTemplateVersion,
				InterpreterVersion: workflow.HostInterpreterVersion, State: "running", StepCount: 1,
			},
			RunAuthority: immediate.AuthorityPlane.Operator,
			AgentStep:    step, DerivedSpawn: identity,
			SpawnState: killtest.SpawnStateSnapshot{
				ConfigMapUID: immediate.SpawnConfigMapUID, ConfigMapIdentity: immediate.SpawnConfigMapIdentity,
				RecordIDs: []string{identity.SpawnID}, ActiveIDs: []string{identity.SpawnID},
				Statuses:        map[string]string{identity.SpawnID: "running"},
				IdempotencyKeys: map[string]string{identity.SpawnID: identity.IdempotencyKey},
			},
			ActiveSpawnPodNames: []string{identity.PodName},
			ExactSpawnPodActive: 1, ExactSpawnPodReady: 1,
			ExactSpawnPodNames: []string{identity.PodName}, ObservedAt: targetAt,
		},
		LeaseAcquired: killtest.CrashLeaseEvidence{
			RequestID: requestID, RunID: runID, SpawnID: identity.SpawnID,
			ObservedAt: acquiredAt, ExpiresAt: deleteAt.Add(2 * time.Minute),
			OperatorAuthority: immediate.AuthorityPlane.Operator,
		},
		LeaseRenewed: killtest.CrashLeaseEvidence{
			RequestID: requestID, RunID: runID, SpawnID: identity.SpawnID,
			ObservedAt: renewedAt, ExpiresAt: deleteAt.Add(time.Minute),
			OperatorAuthority: immediate.AuthorityPlane.Operator,
		},
		DeleteIntentRecordedAt: acquiredAt.Add(10 * time.Millisecond),
		DeleteRequestedAt:      deleteAt, DeleteAcceptedAt: deleteAt.Add(time.Millisecond),
		PolicyDeleteBoundary: verifierPolicyDeleteBoundary(immediate, deleteAt),
	}
	evidence.Target.Quiescence.OperatorAuthority = immediate.AuthorityPlane.Operator
	return evidence
}
