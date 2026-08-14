package killtest

import (
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/workflow"
)

func crashSafetyTestActivity(runID string, lease bool) QuiescenceInMemoryActivity {
	operations := map[string]int64{
		"reconciler": 0,
		"pipeline":   0,
		"cross_run":  0,
		"council":    0,
		"canary":     0,
		"workflow":   0,
	}
	runIDs := map[string][]string{"workflow": {}}
	background := int64(0)
	if runID != "" {
		operations["workflow"] = 1
		runIDs["workflow"] = []string{runID}
		background = 1
	}
	return QuiescenceInMemoryActivity{
		AdmissionClosed:      true,
		CrashLeaseActive:     lease,
		PolicyGeneration:     1,
		SourcesReady:         true,
		SampleStable:         true,
		WiringRequired:       true,
		ActivitySources:      6,
		SourceGeneration:     1,
		SourceOperations:     operations,
		SourceRunIDs:         runIDs,
		BackgroundOperations: background,
	}
}

func crashSafetyTestQuiescence(runID string, lease, quiescent bool, observedAt time.Time) QuiescenceSnapshot {
	activeRuns := 0
	if runID != "" {
		activeRuns = 1
	}
	return QuiescenceSnapshot{
		ObservedAt: observedAt,
		Quiescent:  quiescent,
		Counts: QuiescenceCounts{
			ActiveWorkflowRuns: activeRuns,
		},
		InMemory: crashSafetyTestActivity(runID, lease),
	}
}

func crashSafetyTestPreflight(quiescence QuiescenceSnapshot) PreflightReport {
	fence := passingFluxFenceEvidence()
	setTestFluxSnapshotObservedAt(&fence.Prepared, quiescence.ObservedAt.Add(-time.Second))
	setTestFluxSnapshotObservedAt(&fence.Final, quiescence.ObservedAt.Add(time.Second))
	start := fluxProvenanceByName(fence.Prepared)
	end := fluxProvenanceByName(fence.Final)
	operatorImage := "registry/operator:v1"
	hudImage := "registry/mobile-hud:v1"
	report := PreflightReport{
		FluxSourcesStart: fence.Prepared,
		FluxSourcesEnd:   fence.Final,
		NamespacesOK:     true,
		OperatorImage:    operatorImage,
		Operator: PodIdentity{
			Name: "operator-old", UID: "operator-uid", Node: "node-a",
			Image: operatorImage, ImageID: "registry/operator@sha256:aaa",
			StartedAt: fence.Prepared.ObservedAt.Add(-time.Minute),
		},
		OperatorDeployment: DeploymentIdentity{
			Name: "loom-mills-operator", Generation: 7, ObservedGeneration: 7,
			DesiredReplicas: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
			Image: operatorImage, Strategy: "Recreate", PolicyChecksum: "policy-sha",
		},
		HudImage: hudImage,
		Hud: PodIdentity{
			Name: "hud-old", UID: "hud-uid", Node: "node-b",
			Image: hudImage, ImageID: "registry/mobile-hud@sha256:bbb",
			StartedAt: fence.Prepared.ObservedAt.Add(-time.Minute),
		},
		HudDeployment: DeploymentIdentity{
			Name: "mobile-hud", Generation: 11, ObservedGeneration: 11,
			DesiredReplicas: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
			Image: hudImage, Strategy: "Recreate",
		},
		GitOpsStartRevision:          start["apps"].AppliedRevision,
		GitOpsStartIdentity:          start["apps"].ProtectedIdentity,
		GitOpsRevision:               end["apps"].AppliedRevision,
		GitOpsAttempted:              end["apps"].AttemptedRevision,
		GitOpsReady:                  true,
		GitOpsIdentity:               end["apps"].ProtectedIdentity,
		GitOpsBootstrapStartRevision: start["bootstrap"].AppliedRevision,
		GitOpsBootstrapStartIdentity: start["bootstrap"].ProtectedIdentity,
		GitOpsBootstrapRevision:      end["bootstrap"].AppliedRevision,
		GitOpsBootstrapAttempted:     end["bootstrap"].AttemptedRevision,
		GitOpsBootstrapReady:         true,
		GitOpsBootstrapIdentity:      end["bootstrap"].ProtectedIdentity,
		GitOpsSystemStartRevision:    start["system"].AppliedRevision,
		GitOpsSystemStartIdentity:    start["system"].ProtectedIdentity,
		GitOpsSystemRevision:         end["system"].AppliedRevision,
		GitOpsSystemAttempted:        end["system"].AttemptedRevision,
		GitOpsSystemReady:            true,
		GitOpsSystemIdentity:         end["system"].ProtectedIdentity,
		LoomCoreStartRevision:        start["loom-hub-servers"].AppliedRevision,
		LoomCoreStartIdentity:        start["loom-hub-servers"].ProtectedIdentity,
		LoomCoreRevision:             end["loom-hub-servers"].AppliedRevision,
		LoomCoreAttempted:            end["loom-hub-servers"].AttemptedRevision,
		LoomCoreReady:                true,
		LoomCoreIdentity:             end["loom-hub-servers"].ProtectedIdentity,
		PolicyChecksum:               "policy-sha",
		PolicyConfigMapIdentity: KubernetesObjectIdentity{
			Name: policyConfigMapName, Namespace: s1cOperatorNamespace, UID: "policy-config-uid", ResourceVersion: "40",
		},
		WorkflowsFlag:                   "global_enabled: false\nworkflows_enabled: true\nsubstrate_k8s_only: true",
		FlagEnabled:                     true,
		SubstrateK8sOnly:                true,
		EffectiveFlagEnabled:            true,
		EffectiveSubstrateK8sOnly:       true,
		EffectivePolicyMatchesConfigMap: true,
		SpawnConfigMap:                  true,
		SpawnConfigMapUID:               "spawn-config-uid",
		SpawnConfigMapIdentity: KubernetesObjectIdentity{
			Name: spawnStateConfigMapName, Namespace: s1cSpawnNamespace, UID: "spawn-config-uid", ResourceVersion: "50",
		},
		SpawnConfigMapUpdateAllowed: true,
		Quiescence:                  quiescence,
		LokiReady:                   true,
		AllPreconditions:            true,
	}
	bindTestDeploymentProvenance(&report)
	report.Operator = bindControllerPodFixture(report.Operator, report.OperatorDeployment)
	report.Hud = bindControllerPodFixture(report.Hud, report.HudDeployment)
	bindTestAuthority(&report)
	return report
}

func crashSafetyPassingBoundary() (PreflightReport, PreflightReport) {
	base := time.Date(2026, time.July, 12, 11, 58, 0, 0, time.UTC)
	initial := crashSafetyTestPreflight(crashSafetyTestQuiescence("", false, true, base))
	final := crashSafetyTestPreflight(crashSafetyTestQuiescence("", false, true, base.Add(5*time.Minute)))
	final.Operator = bindControllerPodFixture(PodIdentity{
		Name: "operator-new", UID: "operator-replacement-uid", Node: initial.Operator.Node,
		Image: initial.Operator.Image, ImageID: initial.Operator.ImageID,
		StartedAt: final.Operator.StartedAt.Add(time.Second),
	}, final.OperatorDeployment)
	bindTestAuthority(&final)
	final.Hud = bindControllerPodFixture(PodIdentity{
		Name: "hud-new", UID: "hud-replacement-uid", Node: initial.Hud.Node,
		Image: initial.Hud.Image, ImageID: initial.Hud.ImageID,
		StartedAt: final.Hud.StartedAt.Add(time.Second),
	}, final.HudDeployment)
	return initial, final
}

func crashSafetyPassingCrash() (
	CrashSafetyEvidence,
	FluxSourceFenceEvidence,
	PodIdentity,
	time.Time,
	string,
	string,
) {
	const runID = "wf-canary-crash-safety"
	step := StepView{
		StepKey: "root/agent#0", EventType: "spawn_requested", Status: "pending",
		CallHash: "crash-safety-call-hash", Badge: "pending",
	}
	derived, err := DeriveSpawnIdentity(runID, step)
	if err != nil {
		panic(err)
	}
	step.SpawnID = derived.SpawnID

	immediateAt := time.Date(2026, time.July, 12, 11, 59, 45, 0, time.UTC)
	immediate := crashSafetyTestPreflight(crashSafetyTestQuiescence(runID, false, false, immediateAt))

	fence := passingFluxFenceEvidence()
	setTestFluxSnapshotObservedAt(&fence.Prepared, immediateAt.Add(3*time.Second))
	setTestFluxSnapshotObservedAt(&fence.Final, immediateAt.Add(8*time.Second))
	crashAt := immediateAt.Add(15 * time.Second)
	acquiredAt := immediateAt.Add(4 * time.Second)
	renewedAt := immediateAt.Add(5 * time.Second)
	targetAt := immediateAt.Add(7 * time.Second)

	evidence := CrashSafetyEvidence{
		ImmediatePreflight: immediate,
		Target: CrashTargetSafetyEvidence{
			Quiescence:            crashSafetyTestQuiescence(runID, true, false, targetAt.Add(-time.Second)),
			QuiescenceCollectedAt: targetAt.Add(-500 * time.Millisecond),
			Run: RunSummary{
				ID: runID, Engine: "imperative", Template: workflow.CanaryTemplateName,
				AgentType: AgentTypeClaudeCode, TemplateVersion: workflow.CanaryTemplateVersion,
				InterpreterVersion: workflow.HostInterpreterVersion, State: "running", StepCount: 1,
			},
			RunAuthority: immediate.AuthorityPlane.Operator,
			AgentStep:    step,
			DerivedSpawn: derived,
			SpawnState: SpawnStateSnapshot{
				ConfigMapUID: "spawn-config-uid",
				ConfigMapIdentity: KubernetesObjectIdentity{
					Name: spawnStateConfigMapName, Namespace: s1cSpawnNamespace, UID: "spawn-config-uid", ResourceVersion: "51",
				},
				RecordIDs: []string{"historical-spawn", derived.SpawnID},
				ActiveIDs: []string{derived.SpawnID},
				Statuses:  map[string]string{derived.SpawnID: "running"},
				IdempotencyKeys: map[string]string{
					derived.SpawnID: derived.IdempotencyKey,
				},
			},
			ActiveSpawnPodNames: []string{derived.PodName},
			ExactSpawnPodActive: 1,
			ExactSpawnPodReady:  1,
			ExactSpawnPodNames:  []string{derived.PodName},
			ObservedAt:          targetAt,
		},
		LeaseAcquired: CrashLeaseEvidence{
			RequestID: "lease-request", RunID: runID, SpawnID: derived.SpawnID,
			ObservedAt: acquiredAt, ExpiresAt: crashAt.Add(time.Minute),
			OperatorAuthority: immediate.AuthorityPlane.Operator,
		},
		LeaseRenewed: CrashLeaseEvidence{
			RequestID: "lease-request", RunID: runID, SpawnID: derived.SpawnID,
			ObservedAt: renewedAt, ExpiresAt: crashAt.Add(2 * time.Minute),
			OperatorAuthority: immediate.AuthorityPlane.Operator,
		},
		DeleteIntentRecordedAt: acquiredAt.Add(500 * time.Millisecond),
		DeleteRequestedAt:      crashAt,
		DeleteAcceptedAt:       crashAt.Add(time.Millisecond),
	}
	evidence.Target.Quiescence.OperatorAuthority = immediate.AuthorityPlane.Operator
	evidence.PolicyDeleteBoundary = testPolicyDeleteBoundaryEvidence(
		immediate,
		targetAt.Add(time.Second+time.Nanosecond), targetAt.Add(2*time.Second),
		targetAt.Add(3*time.Second), targetAt.Add(4*time.Second),
		targetAt.Add(5*time.Second),
	)
	return evidence, fence, immediate.Operator, crashAt, runID, derived.SpawnID
}

func TestValidateGateBoundaryEvidenceAcceptsStableReplacementPods(t *testing.T) {
	initial, final := crashSafetyPassingBoundary()
	if err := ValidateGateBoundaryEvidence(initial, final); err != nil {
		t.Fatalf("ValidateGateBoundaryEvidence() error = %v", err)
	}
}

func TestValidateGateBoundaryEvidenceRejectsIncompleteOrDriftedProof(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*PreflightReport, *PreflightReport)
		want   string
	}{
		{"missing initial preconditions", func(initial, _ *PreflightReport) { initial.AllPreconditions = false }, "initial gate did not satisfy"},
		{"missing final preconditions", func(_, final *PreflightReport) { final.AllPreconditions = false }, "final gate did not satisfy"},
		{"invalid provenance", func(_, final *PreflightReport) { final.FluxSourcesEnd.Sources = final.FluxSourcesEnd.Sources[:3] }, "serialized Flux provenance"},
		{"workload digest drift", func(_, final *PreflightReport) { final.Operator.ImageID = "operator@sha256:changed" }, "workload image"},
		{"policy drift", func(_, final *PreflightReport) {
			final.PolicyChecksum = "changed"
			final.OperatorDeployment.PolicyChecksum = "changed"
		}, "exact policy source SHA-256"},
		{"configmap replacement", func(_, final *PreflightReport) {
			final.SpawnConfigMapUID = "replacement-configmap"
			final.SpawnConfigMapIdentity.UID = "replacement-configmap"
		}, "durable spawn ConfigMap stable identity"},
		{"source object replacement", func(_, final *PreflightReport) {
			final.FluxSourcesStart.Sources[0].UID = "apps-replacement"
			final.FluxSourcesEnd.Sources[0].UID = "apps-replacement"
		}, "flux apps object identity changed"},
		{"final work remains", func(_, final *PreflightReport) {
			final.Quiescence.Quiescent = false
			final.Quiescence.Counts.ActiveWorkflowRuns = 1
		}, "zero-work quiescence"},
		{"quiescence outside source boundary", func(_, final *PreflightReport) {
			final.Quiescence.ObservedAt = final.FluxSourcesEnd.ObservedAt.Add(time.Nanosecond)
		}, "outside coherent source boundary"},
		{"reversed gate boundary", func(initial, final *PreflightReport) {
			final.FluxSourcesStart.GitRepositoriesOpenedAt = initial.FluxSourcesEnd.GitRepositories.ObservedAt
		}, "boundary timestamps are not ordered"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			initial, final := crashSafetyPassingBoundary()
			test.mutate(&initial, &final)
			err := ValidateGateBoundaryEvidence(initial, final)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateGateBoundaryEvidence() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateGateIdentityContinuityBindsCrashPreflightToBaseline(t *testing.T) {
	initial, _ := crashSafetyPassingBoundary()
	crash, _, _, _, runID, _ := crashSafetyPassingCrash()
	if err := ValidateGateIdentityContinuity(initial, crash.ImmediatePreflight); err != nil {
		t.Fatalf("ValidateGateIdentityContinuity() error = %v", err)
	}
	crash.ImmediatePreflight.Hud.ImageID = "hud@sha256:drifted"
	if err := ValidateGateIdentityContinuity(initial, crash.ImmediatePreflight); err == nil ||
		!strings.Contains(err.Error(), "workload image") {
		t.Fatalf("ValidateGateIdentityContinuity() drift error = %v (run %s)", err, runID)
	}
}

func TestValidateCrashSafetyEvidenceAcceptsWorkerDerivedSpawn(t *testing.T) {
	evidence, fence, before, crashAt, runID, spawnID := crashSafetyPassingCrash()
	if spawnID == "" || spawnID == "abc123" {
		t.Fatalf("fixture did not use a real derived spawn id: %q", spawnID)
	}
	if err := ValidateCrashSafetyEvidence("CRASH A", runID, spawnID, crashAt, before, fence, evidence); err != nil {
		t.Fatalf("ValidateCrashSafetyEvidence() error = %v", err)
	}
}

func TestValidateCrashSafetyEvidenceRejectsForgedOrMisorderedProof(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CrashSafetyEvidence, *FluxSourceFenceEvidence, *PodIdentity, *time.Time)
		want   string
	}{
		{"immediate preflight failed", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.ImmediatePreflight.AllPreconditions = false
		}, "immediate preflight did not satisfy"},
		{"target pod mismatch", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.ImmediatePreflight.Operator.UID = "wrong-pod"
			e.ImmediatePreflight.AuthorityPlane.Operator.PodUID = "wrong-pod"
			e.ImmediatePreflight.EffectivePolicyAuthority = e.ImmediatePreflight.AuthorityPlane.Operator
			e.ImmediatePreflight.Quiescence.OperatorAuthority = e.ImmediatePreflight.AuthorityPlane.Operator
		}, "differs from delete target"},
		{"stale final source", func(_ *CrashSafetyEvidence, fence *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			fence.Prepared.Sources[0].ResourceVersion = "different"
			fence.Final.Sources[0].ResourceVersion = "different"
		}, "immediate preflight/final Flux binding"},
		{"durable fleet busy", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.Target.Quiescence.Counts.ActivePipelineRuns = 1
		}, "target fleet was not isolated"},
		{"lease not active in memory", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.Target.Quiescence.InMemory.CrashLeaseActive = false
		}, "target fleet was not isolated"},
		{"wrong run", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.Target.Run.ID = "other-run"
		}, "target run identity"},
		{"completed step", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.Target.AgentStep.Status = "success"
		}, "not an in-flight spawn event"},
		{"forged derived spawn", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.Target.DerivedSpawn.SpawnID = "forged"
		}, "serialized derived spawn differs"},
		{"duplicate active record", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.Target.SpawnState.ActiveIDs = append(e.Target.SpawnState.ActiveIDs, "other")
		}, "durable spawn proof"},
		{"wrong durable status", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.Target.SpawnState.Statuses[e.Target.DerivedSpawn.SpawnID] = "completed"
		}, "durable spawn proof"},
		{"wrong idempotency key", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.Target.SpawnState.IdempotencyKeys[e.Target.DerivedSpawn.SpawnID] = "forged"
		}, "durable spawn proof"},
		{"second global pod", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.Target.ActiveSpawnPodNames = append(e.Target.ActiveSpawnPodNames, "spawn-other")
		}, "global active spawn pods"},
		{"exact pod not ready", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.Target.ExactSpawnPodReady = 0
		}, "not one active Ready workload"},
		{"lease request changed", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.LeaseRenewed.RequestID = "other-request"
		}, "lease identity changed"},
		{"renewal before acquisition", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.LeaseRenewed.ObservedAt = e.LeaseAcquired.ObservedAt
		}, "not after acquisition"},
		{"target before renewal", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.Target.ObservedAt = e.LeaseRenewed.ObservedAt.Add(-time.Nanosecond)
			e.Target.Quiescence.ObservedAt = e.Target.ObservedAt.Add(-time.Second)
			e.Target.QuiescenceCollectedAt = e.Target.ObservedAt.Add(-500 * time.Millisecond)
		}, "not ordered between lease renewal and DELETE"},
		{"stale target at delete", func(e *CrashSafetyEvidence, fence *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.Target.ObservedAt = e.DeleteRequestedAt.Add(-11 * time.Second)
			e.Target.Quiescence.ObservedAt = e.Target.ObservedAt.Add(-time.Second)
			e.Target.QuiescenceCollectedAt = e.Target.ObservedAt.Add(-500 * time.Millisecond)
			e.LeaseRenewed.ObservedAt = e.Target.ObservedAt.Add(-time.Second)
			e.LeaseAcquired.ObservedAt = e.Target.ObservedAt.Add(-2 * time.Second)
			setTestFluxSnapshotObservedAt(&fence.Prepared, e.Target.ObservedAt.Add(-3*time.Second))
		}, "target safety proof is"},
		{"stale quiescence at delete", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.Target.Quiescence.ObservedAt = e.DeleteRequestedAt.Add(-41 * time.Second)
		}, "old when collected"},
		{"missing quiescence collection timestamp", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.Target.QuiescenceCollectedAt = time.Time{}
		}, "observation timestamp is missing"},
		{"quiescence collected before renewal", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.Target.QuiescenceCollectedAt = e.LeaseRenewed.ObservedAt.Add(-time.Nanosecond)
		}, "collected before lease renewal"},
		{"quiescence server timestamp too far in future", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.Target.Quiescence.ObservedAt = e.Target.QuiescenceCollectedAt.Add(6 * time.Second)
		}, "in the future when collected"},
		{"delete timestamp forged", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, crashAt *time.Time) {
			*crashAt = crashAt.Add(time.Second)
			_ = e
		}, "differs from crash timestamp"},
		{"missing durable delete intent", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.DeleteIntentRecordedAt = time.Time{}
		}, "intent/receipt evidence is incomplete"},
		{"missing accepted delete receipt", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.DeleteAcceptedAt = time.Time{}
		}, "intent/receipt evidence is incomplete"},
		{"delete intent after renewal", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.DeleteIntentRecordedAt = e.LeaseRenewed.ObservedAt
		}, "intent is not ordered"},
		{"accepted receipt before request", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.DeleteAcceptedAt = e.DeleteRequestedAt.Add(-time.Nanosecond)
		}, "outside the bounded request window"},
		{"renewal expires too soon", func(e *CrashSafetyEvidence, _ *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			e.LeaseRenewed.ExpiresAt = e.DeleteRequestedAt.Add(29 * time.Second)
		}, "need at least"},
		{"prepared snapshot predates preflight", func(e *CrashSafetyEvidence, fence *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			setTestFluxSnapshotObservedAt(&fence.Prepared,
				e.ImmediatePreflight.FluxSourcesEnd.GitRepositories.ObservedAt.Add(-time.Nanosecond))
		}, "predates immediate preflight"},
		{"prepared snapshot follows acquisition", func(e *CrashSafetyEvidence, fence *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			setTestFluxSnapshotObservedAt(&fence.Prepared, e.LeaseAcquired.ObservedAt.Add(time.Nanosecond))
		}, "predates prepared Flux snapshot"},
		{"final snapshot predates target", func(e *CrashSafetyEvidence, fence *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			setTestFluxSnapshotObservedAt(&fence.Final, e.Target.ObservedAt.Add(-time.Nanosecond))
		}, "predates target safety proof"},
		{"final Flux completion equals policy A", func(e *CrashSafetyEvidence, fence *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			fence.Final.ObservedAt = e.PolicyDeleteBoundary.ConfigMapA.ObservedAt
			fence.Final.GitRepositories.ObservedAt = e.PolicyDeleteBoundary.ConfigMapA.ObservedAt
		}, "did not complete before policy ConfigMap A"},
		{"final GitRepository completion equals policy A", func(e *CrashSafetyEvidence, fence *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			fence.Final.GitRepositories.ObservedAt = e.PolicyDeleteBoundary.ConfigMapA.ObservedAt
		}, "did not complete before policy ConfigMap A"},
		{"final snapshot follows delete", func(e *CrashSafetyEvidence, fence *FluxSourceFenceEvidence, _ *PodIdentity, _ *time.Time) {
			setTestFluxSnapshotObservedAt(&fence.Final, e.DeleteRequestedAt.Add(time.Nanosecond))
		}, "did not complete before policy ConfigMap A"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence, fence, before, crashAt, runID, spawnID := crashSafetyPassingCrash()
			test.mutate(&evidence, &fence, &before, &crashAt)
			err := ValidateCrashSafetyEvidence("CRASH A", runID, spawnID, crashAt, before, fence, evidence)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateCrashSafetyEvidence() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateCrashSafetyEvidenceBindsCrashLabelToWorkload(t *testing.T) {
	evidence, fence, _, crashAt, runID, spawnID := crashSafetyPassingCrash()
	if err := ValidateCrashSafetyEvidence("CRASH B", runID, spawnID, crashAt,
		evidence.ImmediatePreflight.Hud, fence, evidence); err != nil {
		t.Fatalf("CRASH B rejected mobile-hud target: %v", err)
	}
	if err := ValidateCrashSafetyEvidence("CRASH B", runID, spawnID, crashAt,
		evidence.ImmediatePreflight.Operator, fence, evidence); err == nil ||
		!strings.Contains(err.Error(), "differs from delete target") {
		t.Fatalf("CRASH B accepted operator target: %v", err)
	}
	if err := ValidateCrashSafetyEvidence("crash-a", runID, spawnID, crashAt,
		evidence.ImmediatePreflight.Operator, fence, evidence); err == nil ||
		!strings.Contains(err.Error(), "unsupported crash safety label") {
		t.Fatalf("unsupported label accepted: %v", err)
	}
}
