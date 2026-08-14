package killtest

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	crashLeaseDeleteSafetyMargin  = 20 * time.Second
	crashQuiescenceMaxStaleness   = 30 * time.Second
	crashQuiescenceMaxFutureSkew  = 5 * time.Second
	crashQuiescenceEvidenceMaxAge = crashQuiescenceMaxStaleness + finalPreDeleteCheckTimeout
)

// ValidateDeleteBoundaryFreshness is the no-I/O mutation gate shared by the
// live harness and pure evidence verifier.
func ValidateDeleteBoundaryFreshness(
	deleteAt time.Time,
	target CrashTargetSafetyEvidence,
	fluxFence FluxSourceFenceEvidence,
) error {
	if deleteAt.IsZero() || target.ObservedAt.IsZero() || target.Quiescence.ObservedAt.IsZero() ||
		target.QuiescenceCollectedAt.IsZero() || fluxFence.Final.ObservedAt.IsZero() ||
		fluxFence.Final.GitRepositories.ObservedAt.IsZero() {
		return errors.New("delete-boundary freshness evidence is incomplete")
	}
	for label, observedAt := range map[string]time.Time{
		"target": target.ObservedAt, "quiescence collection": target.QuiescenceCollectedAt,
		"final Flux snapshot":          fluxFence.Final.ObservedAt,
		"final GitRepository snapshot": fluxFence.Final.GitRepositories.ObservedAt,
	} {
		if observedAt.After(deleteAt) {
			return fmt.Errorf("%s %s follows delete boundary %s", label, observedAt, deleteAt)
		}
	}
	if age := deleteAt.Sub(target.ObservedAt); age > finalPreDeleteCheckTimeout {
		return fmt.Errorf("target safety proof is %s old at DELETE, exceeds %s", age, finalPreDeleteCheckTimeout)
	}
	if age := deleteAt.Sub(fluxFence.Final.ObservedAt); age > finalPreDeleteCheckTimeout {
		return fmt.Errorf("final Flux snapshot is %s old at DELETE, exceeds %s", age, finalPreDeleteCheckTimeout)
	}
	if age := deleteAt.Sub(fluxFence.Final.GitRepositories.ObservedAt); age > finalPreDeleteCheckTimeout {
		return fmt.Errorf("final GitRepository snapshot is %s old at DELETE, exceeds %s", age, finalPreDeleteCheckTimeout)
	}
	if age := deleteAt.Sub(target.Quiescence.ObservedAt); age > crashQuiescenceEvidenceMaxAge {
		return fmt.Errorf("target quiescence proof is %s old at DELETE, exceeds %s", age, crashQuiescenceEvidenceMaxAge)
	}
	if target.QuiescenceCollectedAt.After(target.ObservedAt) {
		return fmt.Errorf("quiescence collection %s follows completed target proof %s",
			target.QuiescenceCollectedAt, target.ObservedAt)
	}
	if err := validateQuiescenceCollectionTiming(target.Quiescence.ObservedAt, target.QuiescenceCollectedAt); err != nil {
		return fmt.Errorf("target quiescence: %w", err)
	}
	return nil
}

func validateQuiescenceCollectionTiming(serverObservedAt, collectedAt time.Time) error {
	age := collectedAt.Sub(serverObservedAt)
	if age > crashQuiescenceMaxStaleness {
		return fmt.Errorf("proof was %s old when collected, exceeds %s", age, crashQuiescenceMaxStaleness)
	}
	if age < -crashQuiescenceMaxFutureSkew {
		return fmt.Errorf("server timestamp was %s in the future when collected, exceeds %s clock skew",
			-age, crashQuiescenceMaxFutureSkew)
	}
	return nil
}

// ValidateGateBoundaryEvidence proves from serialized evidence that both ends
// of a gate were safe, coherent preflights of the same immutable deployment.
// It deliberately permits a protected source to advance to an accepted
// descendant while rejecting live object replacement, generation drift, or a
// change to any protected digest.
func ValidateGateBoundaryEvidence(initial, final PreflightReport) error {
	if err := validateCrashSafetyPreflight("initial gate", initial); err != nil {
		return err
	}
	if err := validateZeroWorkPreflight("initial gate", initial); err != nil {
		return err
	}
	if err := validateCrashSafetyPreflight("final gate", final); err != nil {
		return err
	}
	if err := validateZeroWorkPreflight("final gate", final); err != nil {
		return err
	}
	if !initial.FluxSourcesEnd.GitRepositories.ObservedAt.Before(final.FluxSourcesStart.GitRepositoriesOpenedAt) {
		return fmt.Errorf("gate boundary timestamps are not ordered: initial_end=%s final_start=%s",
			initial.FluxSourcesEnd.GitRepositories.ObservedAt, final.FluxSourcesStart.GitRepositoriesOpenedAt)
	}
	if err := validateSameGateIdentity(initial, final); err != nil {
		return fmt.Errorf("gate boundary immutable identity: %w", err)
	}
	return nil
}

// ValidateGateIdentityContinuity proves that an in-run preflight still carries
// the initial gate's immutable source, workload, policy, and ConfigMap identity.
// Activity is validated by the caller because a crash preflight intentionally
// permits the one target workflow that a boundary preflight forbids.
func ValidateGateIdentityContinuity(initial, observed PreflightReport) error {
	if err := validateCrashSafetyPreflight("initial gate", initial); err != nil {
		return err
	}
	if err := validateCrashSafetyPreflight("in-run gate", observed); err != nil {
		return err
	}
	if err := validateSameGateIdentity(initial, observed); err != nil {
		return fmt.Errorf("in-run immutable identity: %w", err)
	}
	return nil
}

// sameControllerPodIncarnation compares durable controller identity across
// independent namespace List observations. A List resourceVersion belongs to
// the observation, not the selected Pod, so it is retained as evidence but is
// not part of incarnation continuity. Every Pod, container, ReplicaSet,
// Deployment, and execution-provenance field remains exact.
func sameControllerPodIncarnation(left, right PodIdentity) bool {
	left.PodCensusListResourceVersion = ""
	right.PodCensusListResourceVersion = ""
	return left == right
}

// ValidateCrashAPodContinuity prevents an unplanned singleton restart between
// the initial gate and the first destructive request.
func ValidateCrashAPodContinuity(initial, crashA PreflightReport) error {
	if !sameControllerPodIncarnation(initial.Operator, crashA.Operator) ||
		!sameControllerPodIncarnation(initial.Hud, crashA.Hud) {
		return fmt.Errorf("workload pod identity changed before CRASH A: operator=%+v/%+v hud=%+v/%+v",
			initial.Operator, crashA.Operator, initial.Hud, crashA.Hud)
	}
	return nil
}

// ValidateCrashBPodContinuity proves that only the planned operator restart
// occurred before the second destructive request.
func ValidateCrashBPodContinuity(crashA, crashB PreflightReport, operatorReplacement PodIdentity) error {
	if !sameControllerPodIncarnation(crashB.Operator, operatorReplacement) {
		return fmt.Errorf("CRASH A replacement is not the exact operator at CRASH B: %+v != %+v",
			operatorReplacement, crashB.Operator)
	}
	if !sameControllerPodIncarnation(crashA.Hud, crashB.Hud) {
		return fmt.Errorf("mobile-hud changed unexpectedly before CRASH B: %+v != %+v", crashA.Hud, crashB.Hud)
	}
	return nil
}

// ValidateFinalPodContinuity proves that both planned replacements, and no
// later singleton incarnations, survived through the final gate.
func ValidateFinalPodContinuity(final PreflightReport, operatorReplacement, hudReplacement PodIdentity) error {
	if !sameControllerPodIncarnation(final.Operator, operatorReplacement) ||
		!sameControllerPodIncarnation(final.Hud, hudReplacement) {
		return fmt.Errorf("planned replacements did not persist through final gate: operator=%+v/%+v hud=%+v/%+v",
			operatorReplacement, final.Operator, hudReplacement, final.Hud)
	}
	return nil
}

// ValidateCrashSafetyEvidence proves that one destructive request was bound to
// an immediate coherent preflight, the only active workflow/spawn, a renewed
// non-secret crash lease, and the exact selected singleton pod. label must be
// CRASH A (operator) or CRASH B (mobile-hud).
func ValidateCrashSafetyEvidence(
	label, runID, spawnID string,
	crashAt time.Time,
	before PodIdentity,
	fluxFence FluxSourceFenceEvidence,
	evidence CrashSafetyEvidence,
) error {
	if label != "CRASH A" && label != "CRASH B" {
		return fmt.Errorf("unsupported crash safety label %q", label)
	}
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(spawnID) == "" {
		return fmt.Errorf("%s safety identity is incomplete: run_id=%q spawn_id=%q", label, runID, spawnID)
	}
	if crashAt.IsZero() {
		return fmt.Errorf("%s safety crash timestamp is missing", label)
	}
	if err := validateCrashPodIdentity(before); err != nil {
		return fmt.Errorf("%s selected pod identity: %w", label, err)
	}
	if err := validateCrashSafetyPreflight(label+" immediate preflight", evidence.ImmediatePreflight); err != nil {
		return err
	}
	if err := validateAttachedPreflight(label, runID, evidence.ImmediatePreflight); err != nil {
		return err
	}
	if err := ValidateFluxSourceFenceEvidence(fluxFence); err != nil {
		return fmt.Errorf("%s final Flux fence: %w", label, err)
	}

	preflightPod := evidence.ImmediatePreflight.Operator
	if label == "CRASH B" {
		preflightPod = evidence.ImmediatePreflight.Hud
	}
	if !sameControllerPodIncarnation(preflightPod, before) {
		return fmt.Errorf("%s immediate preflight selected pod differs from delete target: %+v != %+v",
			label, preflightPod, before)
	}
	if err := validateFluxSnapshotBinding(
		evidence.ImmediatePreflight.FluxSourcesEnd,
		fluxFence.Final,
	); err != nil {
		return fmt.Errorf("%s immediate preflight/final Flux binding: %w", label, err)
	}
	if err := validateCrashTargetEvidence(label, runID, spawnID, evidence); err != nil {
		return err
	}
	if err := validateCrashLeaseEvidence(label, runID, spawnID, crashAt, evidence); err != nil {
		return err
	}
	policyAObservedAt := evidence.PolicyDeleteBoundary.ConfigMapA.ObservedAt
	if !fluxFence.Final.ObservedAt.Before(policyAObservedAt) {
		return fmt.Errorf("%s final Flux snapshot %s did not complete before policy ConfigMap A request %s",
			label, fluxFence.Final.ObservedAt, policyAObservedAt)
	}
	if !fluxFence.Final.GitRepositories.ObservedAt.Before(policyAObservedAt) {
		return fmt.Errorf("%s final GitRepository snapshot %s did not complete before policy ConfigMap A request %s",
			label, fluxFence.Final.GitRepositories.ObservedAt, policyAObservedAt)
	}
	if evidence.ImmediatePreflight.FluxSourcesEnd.GitRepositories.ObservedAt.After(fluxFence.Prepared.GitRepositoriesOpenedAt) {
		return fmt.Errorf("%s prepared Flux source bracket %s predates immediate preflight end %s",
			label, fluxFence.Prepared.GitRepositoriesOpenedAt,
			evidence.ImmediatePreflight.FluxSourcesEnd.GitRepositories.ObservedAt)
	}
	if fluxFence.Prepared.GitRepositories.ObservedAt.After(evidence.LeaseAcquired.ObservedAt) {
		return fmt.Errorf("%s lease acquisition %s predates prepared Flux snapshot %s",
			label, evidence.LeaseAcquired.ObservedAt, fluxFence.Prepared.GitRepositories.ObservedAt)
	}
	if evidence.Target.ObservedAt.After(fluxFence.Final.GitRepositoriesOpenedAt) {
		return fmt.Errorf("%s final Flux source bracket %s predates target safety proof %s",
			label, fluxFence.Final.GitRepositoriesOpenedAt, evidence.Target.ObservedAt)
	}
	if fluxFence.Final.ObservedAt.After(evidence.DeleteRequestedAt) {
		return fmt.Errorf("%s final Flux snapshot %s follows DELETE request %s",
			label, fluxFence.Final.ObservedAt, evidence.DeleteRequestedAt)
	}
	if fluxFence.Final.GitRepositories.ObservedAt.After(evidence.DeleteRequestedAt) {
		return fmt.Errorf("%s final GitRepository snapshot %s follows DELETE request %s",
			label, fluxFence.Final.GitRepositories.ObservedAt, evidence.DeleteRequestedAt)
	}
	if age := evidence.DeleteRequestedAt.Sub(fluxFence.Final.ObservedAt); age > finalPreDeleteCheckTimeout {
		return fmt.Errorf("%s final Flux snapshot is %s old at DELETE, exceeds %s",
			label, age, finalPreDeleteCheckTimeout)
	}
	if err := ValidateDeleteBoundaryFreshness(evidence.DeleteRequestedAt, evidence.Target, fluxFence); err != nil {
		return fmt.Errorf("%s delete-boundary freshness: %w", label, err)
	}
	return nil
}

func validateCrashSafetyPreflight(label string, report PreflightReport) error {
	if !report.AllPreconditions {
		return fmt.Errorf("%s did not satisfy all preconditions", label)
	}
	if err := ValidatePreflightFluxProvenance(report); err != nil {
		return fmt.Errorf("%s serialized Flux provenance: %w", label, err)
	}
	if err := validatePreflightFluxBindings(report); err != nil {
		return fmt.Errorf("%s Flux summary binding: %w", label, err)
	}
	if err := ValidateDeploymentProvenance(report); err != nil {
		return fmt.Errorf("%s Deployment provenance: %w", label, err)
	}
	if err := ValidatePreflightConfigMapIdentities(report); err != nil {
		return fmt.Errorf("%s ConfigMap identity: %w", label, err)
	}
	if err := ValidatePolicyConfigMapProvenance(report); err != nil {
		return fmt.Errorf("%s policy ConfigMap provenance: %w", label, err)
	}
	if err := ValidateAuthorityPlaneEvidence(
		report.AuthorityPlane, report.Operator, report.OperatorDeployment,
	); err != nil {
		return fmt.Errorf("%s authority plane: %w", label, err)
	}
	if report.EffectivePolicyAuthority != report.AuthorityPlane.Operator ||
		report.Quiescence.OperatorAuthority != report.AuthorityPlane.Operator {
		return fmt.Errorf("%s policy/quiescence REST authority differs from selected operator: policy=%+v quiescence=%+v operator=%+v",
			label, report.EffectivePolicyAuthority, report.Quiescence.OperatorAuthority,
			report.AuthorityPlane.Operator)
	}
	if !report.NamespacesOK || !report.LokiReady {
		return fmt.Errorf("%s cluster evidence path is incomplete: namespaces_ok=%t loki_ready=%t",
			label, report.NamespacesOK, report.LokiReady)
	}
	if err := validatePreflightWorkload("operator", report.OperatorImage, report.Operator, report.OperatorDeployment); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if err := validatePreflightWorkload("mobile-hud", report.HudImage, report.Hud, report.HudDeployment); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if strings.TrimSpace(report.PolicyChecksum) == "" || report.OperatorDeployment.PolicyChecksum != report.PolicyChecksum {
		return fmt.Errorf("%s policy checksum is incomplete or not bound to the operator deployment", label)
	}
	if report.ConfigMapPolicyEnabled || report.EffectivePolicyEnabled ||
		!report.FlagEnabled || !report.EffectiveFlagEnabled ||
		!report.SubstrateK8sOnly || !report.EffectiveSubstrateK8sOnly ||
		!report.EffectivePolicyMatchesConfigMap {
		return fmt.Errorf("%s policy evidence is not the closed-admission, enabled-workflow, k8s-only canary policy", label)
	}
	if !report.SpawnConfigMap || strings.TrimSpace(report.SpawnConfigMapUID) == "" ||
		!report.SpawnConfigMapUpdateAllowed {
		return fmt.Errorf("%s durable spawn ConfigMap evidence is incomplete", label)
	}
	if len(report.ActiveSpawnIDs) != 0 || len(report.ActiveSpawnPodNames) != 0 {
		return fmt.Errorf("%s contains unfiltered active spawn evidence: ids=%v pods=%v",
			label, report.ActiveSpawnIDs, report.ActiveSpawnPodNames)
	}
	if report.Quiescence.ObservedAt.IsZero() {
		return fmt.Errorf("%s quiescence observation timestamp is missing", label)
	}
	if report.Quiescence.ObservedAt.Before(report.FluxSourcesStart.ObservedAt) ||
		report.Quiescence.ObservedAt.After(report.FluxSourcesEnd.ObservedAt) {
		return fmt.Errorf("%s quiescence observation %s is outside coherent source boundary %s -> %s",
			label, report.Quiescence.ObservedAt,
			report.FluxSourcesStart.ObservedAt, report.FluxSourcesEnd.ObservedAt)
	}
	return nil
}

func validateZeroWorkPreflight(label string, report PreflightReport) error {
	if !report.Quiescence.Quiescent || !report.Quiescence.Counts.unrelatedIdle(0) ||
		!report.Quiescence.InMemory.idle() {
		return fmt.Errorf("%s did not prove zero-work quiescence: durable=%+v in_memory=%+v quiescent=%t",
			label, report.Quiescence.Counts, report.Quiescence.InMemory, report.Quiescence.Quiescent)
	}
	return nil
}

func validateAttachedPreflight(label, runID string, report PreflightReport) error {
	if !report.Quiescence.Counts.unrelatedIdle(1) ||
		!report.Quiescence.InMemory.idleForWorkflow(runID) {
		return fmt.Errorf("%s immediate preflight did not isolate target workflow %q: durable=%+v in_memory=%+v",
			label, runID, report.Quiescence.Counts, report.Quiescence.InMemory)
	}
	return nil
}

func validatePreflightWorkload(
	label, image string,
	pod PodIdentity,
	deployment DeploymentIdentity,
) error {
	if err := validateCrashPodIdentity(pod); err != nil {
		return fmt.Errorf("%s pod identity: %w", label, err)
	}
	if strings.TrimSpace(deployment.Name) == "" || strings.TrimSpace(deployment.Namespace) == "" ||
		strings.TrimSpace(deployment.UID) == "" || strings.TrimSpace(deployment.ResourceVersion) == "" ||
		strings.TrimSpace(deployment.Image) == "" ||
		deployment.Strategy != "Recreate" ||
		deployment.Generation <= 0 || deployment.ObservedGeneration != deployment.Generation ||
		deployment.DesiredReplicas != 1 || deployment.Replicas != 1 || deployment.UpdatedReplicas != 1 ||
		deployment.ReadyReplicas != 1 || deployment.AvailableReplicas != 1 {
		return fmt.Errorf("%s deployment is not a fully observed stable singleton: %+v", label, deployment)
	}
	if strings.TrimSpace(image) == "" || image != deployment.Image || image != pod.Image {
		return fmt.Errorf("%s requested image is not bound across report/deployment/pod: %q/%q/%q",
			label, image, deployment.Image, pod.Image)
	}
	if strings.TrimSpace(deployment.ContainerName) == "" || pod.ContainerName != deployment.ContainerName {
		return fmt.Errorf("%s main container name is not bound across Deployment/pod: %q/%q",
			label, deployment.ContainerName, pod.ContainerName)
	}
	if pod.Namespace != deployment.Namespace || pod.DeploymentName != deployment.Name ||
		pod.DeploymentUID != deployment.UID {
		return fmt.Errorf("%s pod is not owned by the reviewed Deployment: pod=%+v deployment=%+v",
			label, pod, deployment)
	}
	if !isNormalizedSHA256(pod.ReplicaSetPodTemplateSHA256) ||
		pod.ReplicaSetPodTemplateSHA256 != deployment.PodTemplateSHA256 ||
		deployment.PodTemplateSHA256 != deployment.ReviewedPodTemplateSHA256 {
		return fmt.Errorf("%s ReplicaSet pod template is not bound to the reviewed Deployment template: pod=%q deployment=%q reviewed=%q",
			label, pod.ReplicaSetPodTemplateSHA256, deployment.PodTemplateSHA256,
			deployment.ReviewedPodTemplateSHA256)
	}
	if !isNormalizedSHA256(pod.ReplicaSetSelectorSHA256) ||
		pod.ReplicaSetSelectorSHA256 != deployment.SelectorSHA256 ||
		deployment.SelectorSHA256 != deployment.ReviewedSelectorSHA256 {
		return fmt.Errorf("%s ReplicaSet selector is not bound to the reviewed Deployment selector: pod=%q deployment=%q reviewed=%q",
			label, pod.ReplicaSetSelectorSHA256, deployment.SelectorSHA256,
			deployment.ReviewedSelectorSHA256)
	}
	return nil
}

func validateCrashPodIdentity(pod PodIdentity) error {
	if strings.TrimSpace(pod.Name) == "" || strings.TrimSpace(pod.Namespace) == "" ||
		strings.TrimSpace(pod.UID) == "" || strings.TrimSpace(pod.ResourceVersion) == "" ||
		strings.TrimSpace(pod.PodCensusListResourceVersion) == "" || pod.PodCensusCount != 1 ||
		strings.TrimSpace(pod.Node) == "" || strings.TrimSpace(pod.Image) == "" ||
		strings.TrimSpace(pod.ImageID) == "" || pod.StartedAt.IsZero() ||
		strings.TrimSpace(pod.ContainerName) == "" || strings.TrimSpace(pod.ContainerID) == "" ||
		pod.ContainerStartedAt.IsZero() || strings.TrimSpace(pod.ReplicaSetName) == "" ||
		strings.TrimSpace(pod.ReplicaSetUID) == "" || strings.TrimSpace(pod.ReplicaSetResourceVersion) == "" ||
		!isNormalizedSHA256(pod.ReplicaSetPodTemplateSHA256) ||
		!isNormalizedSHA256(pod.ReplicaSetSelectorSHA256) ||
		strings.TrimSpace(pod.DeploymentName) == "" || strings.TrimSpace(pod.DeploymentUID) == "" {
		return fmt.Errorf("incomplete identity: %+v", pod)
	}
	if pod.ContainerRestartCount != 0 {
		return fmt.Errorf("controller container restarted %d time(s): %+v", pod.ContainerRestartCount, pod)
	}
	if pod.PodExecutionContract != PodExecutionProvenanceContract ||
		pod.PodExecutionContractVersion != PodExecutionProvenanceContractVersion ||
		pod.PodExecutionRenderer != PodExecutionRenderer ||
		pod.PodExecutionRendererVersion != PodExecutionRendererVersion ||
		!isNormalizedSHA256(pod.LivePodSpecSHA256) ||
		pod.LivePodSpecSHA256 != pod.DryRunPodSpecSHA256 {
		return fmt.Errorf("live Pod execution provenance is incomplete or drifted: %+v", pod)
	}
	if pod.ReplicaSetGeneration <= 0 ||
		pod.ReplicaSetObservedGeneration != pod.ReplicaSetGeneration ||
		pod.ReplicaSetDesiredReplicas != 1 || pod.ReplicaSetReplicas != 1 ||
		pod.ReplicaSetFullyLabeledReplicas != 1 || pod.ReplicaSetReadyReplicas != 1 ||
		pod.ReplicaSetAvailableReplicas != 1 {
		return fmt.Errorf("ReplicaSet is not a fully observed stable singleton: %+v", pod)
	}
	if pod.ContainerStartedAt.Before(pod.StartedAt) {
		return fmt.Errorf("controller container predates pod: %+v", pod)
	}
	return nil
}

func validatePreflightFluxBindings(report PreflightReport) error {
	if report.FluxSourcesStart.ObservedAt.IsZero() || report.FluxSourcesEnd.ObservedAt.IsZero() ||
		report.FluxSourcesEnd.ObservedAt.Before(report.FluxSourcesStart.ObservedAt) {
		return fmt.Errorf("coherent snapshot timestamps are missing or out of order: %s -> %s",
			report.FluxSourcesStart.ObservedAt, report.FluxSourcesEnd.ObservedAt)
	}
	start := fluxProvenanceByName(report.FluxSourcesStart)
	end := fluxProvenanceByName(report.FluxSourcesEnd)
	bindings := []struct {
		name                   string
		startRevision          string
		startIdentity          GitOpsScopeIdentity
		endRevision, attempted string
		ready                  bool
		endIdentity            GitOpsScopeIdentity
	}{
		{"apps", report.GitOpsStartRevision, report.GitOpsStartIdentity, report.GitOpsRevision, report.GitOpsAttempted, report.GitOpsReady, report.GitOpsIdentity},
		{"bootstrap", report.GitOpsBootstrapStartRevision, report.GitOpsBootstrapStartIdentity, report.GitOpsBootstrapRevision, report.GitOpsBootstrapAttempted, report.GitOpsBootstrapReady, report.GitOpsBootstrapIdentity},
		{"system", report.GitOpsSystemStartRevision, report.GitOpsSystemStartIdentity, report.GitOpsSystemRevision, report.GitOpsSystemAttempted, report.GitOpsSystemReady, report.GitOpsSystemIdentity},
		{"loom-hub-servers", report.LoomCoreStartRevision, report.LoomCoreStartIdentity, report.LoomCoreRevision, report.LoomCoreAttempted, report.LoomCoreReady, report.LoomCoreIdentity},
	}
	for _, binding := range bindings {
		startSource := start[binding.name]
		endSource := end[binding.name]
		if binding.startRevision != startSource.AppliedRevision ||
			binding.startIdentity != startSource.ProtectedIdentity {
			return fmt.Errorf("flux %s start summary differs from coherent snapshot", binding.name)
		}
		if binding.endRevision != endSource.AppliedRevision ||
			binding.attempted != endSource.AttemptedRevision ||
			binding.ready != (endSource.ReadyStatus == "True") ||
			binding.endIdentity != endSource.ProtectedIdentity {
			return fmt.Errorf("flux %s end summary differs from coherent snapshot", binding.name)
		}
	}
	return nil
}

func validateSameGateIdentity(initial, final PreflightReport) error {
	if initial.OperatorImage != final.OperatorImage || initial.Operator.ImageID != final.Operator.ImageID ||
		initial.HudImage != final.HudImage || initial.Hud.ImageID != final.Hud.ImageID {
		return errors.New("workload image, digest, deployment generation, or strategy changed")
	}
	if !sameAuthorityCluster(initial.AuthorityPlane, final.AuthorityPlane) {
		return errors.New("kubernetes authority transport or operator Namespace UID changed")
	}
	if err := ValidateConfigMapGateIdentity(initial, final); err != nil {
		return fmt.Errorf("ConfigMap gate identity: %w", err)
	}
	if err := ValidatePolicyConfigMapGateIdentity(initial, final); err != nil {
		return fmt.Errorf("policy ConfigMap gate identity: %w", err)
	}
	if initial.PolicyChecksum != final.PolicyChecksum ||
		initial.ConfigMapPolicyEnabled != final.ConfigMapPolicyEnabled ||
		initial.FlagEnabled != final.FlagEnabled ||
		initial.SubstrateK8sOnly != final.SubstrateK8sOnly ||
		initial.EffectivePolicyEnabled != final.EffectivePolicyEnabled ||
		initial.EffectiveFlagEnabled != final.EffectiveFlagEnabled ||
		initial.EffectiveSubstrateK8sOnly != final.EffectiveSubstrateK8sOnly {
		return errors.New("policy or durable spawn ConfigMap identity changed")
	}
	if err := sameDeploymentGateIdentity(initial.OperatorDeployment, final.OperatorDeployment); err != nil {
		return fmt.Errorf("operator %w", err)
	}
	if err := sameDeploymentGateIdentity(initial.HudDeployment, final.HudDeployment); err != nil {
		return fmt.Errorf("mobile-hud %w", err)
	}

	initialSources := fluxProvenanceByName(initial.FluxSourcesEnd)
	finalSources := fluxProvenanceByName(final.FluxSourcesEnd)
	for _, name := range requiredFluxProvenanceOwners {
		left, right := initialSources[name], finalSources[name]
		if left.UID != right.UID || left.Generation != right.Generation {
			return fmt.Errorf("flux %s object identity changed: uid=%q/%q generation=%d/%d",
				name, left.UID, right.UID, left.Generation, right.Generation)
		}
		if left.RenderSpec != right.RenderSpec {
			return fmt.Errorf("flux %s render spec identity changed: %+v -> %+v",
				name, left.RenderSpec, right.RenderSpec)
		}
		if err := validateSameProtectedIdentity(left.ProtectedIdentity, right.ProtectedIdentity); err != nil {
			return fmt.Errorf("flux %s protected identity: %w", name, err)
		}
	}
	if err := sameGitRepositoryGateIdentity(
		initial.FluxSourcesEnd.GitRepositories,
		final.FluxSourcesEnd.GitRepositories,
	); err != nil {
		return err
	}
	return nil
}

func validateSameProtectedIdentity(left, right GitOpsScopeIdentity) error {
	if left.Mode == "" || left.Contract == "" || left.ContractVersion <= 0 ||
		left.BaselineRevision == "" || left.BaselineDigest == "" || left.ObservedDigest == "" {
		return errors.New("initial identity is incomplete")
	}
	if left.Mode != right.Mode || left.Contract != right.Contract ||
		left.ContractVersion != right.ContractVersion ||
		left.BaselineRevision != right.BaselineRevision ||
		left.BaselineDigest != right.BaselineDigest ||
		left.ObservedDigest != right.ObservedDigest {
		return fmt.Errorf("immutable identity changed: %+v -> %+v", left, right)
	}
	return nil
}

func validateFluxSnapshotBinding(preflight, final FluxSourceProvenanceSnapshot) error {
	if err := ValidateFluxSourceProvenanceSnapshot(preflight); err != nil {
		return fmt.Errorf("preflight snapshot: %w", err)
	}
	if err := ValidateFluxSourceProvenanceSnapshot(final); err != nil {
		return fmt.Errorf("final snapshot: %w", err)
	}
	preflightSources := fluxProvenanceByName(preflight)
	finalSources := fluxProvenanceByName(final)
	for _, name := range requiredFluxProvenanceOwners {
		if preflightSources[name] != finalSources[name] {
			return fmt.Errorf("flux %s source changed between immediate preflight and final fence", name)
		}
	}
	preflightRepositories := gitRepositoryProvenanceByName(preflight.GitRepositories)
	finalRepositories := gitRepositoryProvenanceByName(final.GitRepositories)
	for _, name := range requiredGitRepositoryNames {
		if preflightRepositories[name] != finalRepositories[name] {
			return fmt.Errorf("GitRepository %s changed between immediate preflight and final fence", name)
		}
	}
	return nil
}

func validateCrashTargetEvidence(
	label, runID, spawnID string,
	evidence CrashSafetyEvidence,
) error {
	target := evidence.Target
	wantAuthority := evidence.ImmediatePreflight.AuthorityPlane.Operator
	if target.Quiescence.OperatorAuthority != wantAuthority || target.RunAuthority != wantAuthority {
		return fmt.Errorf("%s quiescence/run REST authority differs from immediate operator: quiescence=%+v run=%+v operator=%+v",
			label, target.Quiescence.OperatorAuthority, target.RunAuthority, wantAuthority)
	}
	if target.ObservedAt.IsZero() || target.Quiescence.ObservedAt.IsZero() || target.QuiescenceCollectedAt.IsZero() {
		return fmt.Errorf("%s target safety observation timestamp is missing", label)
	}
	if target.QuiescenceCollectedAt.After(target.ObservedAt) {
		return fmt.Errorf("%s target quiescence collection follows the completed target proof", label)
	}
	if err := validateQuiescenceCollectionTiming(target.Quiescence.ObservedAt, target.QuiescenceCollectedAt); err != nil {
		return fmt.Errorf("%s target quiescence: %w", label, err)
	}
	if !target.Quiescence.Counts.unrelatedIdle(1) ||
		!target.Quiescence.InMemory.idleForWorkflowCrashLease(runID) {
		return fmt.Errorf("%s target fleet was not isolated to workflow %q with an active lease: durable=%+v in_memory=%+v",
			label, runID, target.Quiescence.Counts, target.Quiescence.InMemory)
	}
	if target.Run.ID != runID || target.Run.State != "running" {
		return fmt.Errorf("%s target run identity/state is %q/%q, want %q/running",
			label, target.Run.ID, target.Run.State, runID)
	}
	if err := ValidateAgentType(target.Run.AgentType); err != nil {
		return fmt.Errorf("%s target run: %w", label, err)
	}
	if err := validateCanaryRunDetail(runID, target.Run.AgentType, target.Run.TemplateVersion, RunDetail{Run: target.Run}); err != nil {
		return fmt.Errorf("%s target run: %w", label, err)
	}
	if err := validateKnownCanaryTemplateVersion(target.Run.TemplateVersion); err != nil {
		return fmt.Errorf("%s target run: %w", label, err)
	}
	if target.AgentStep.Status != "pending" || !isCrashSafetyAgentEvent(target.AgentStep.EventType) {
		return fmt.Errorf("%s target agent step is not an in-flight spawn event: %+v", label, target.AgentStep)
	}
	derived, err := DeriveSpawnIdentity(runID, target.AgentStep)
	if err != nil {
		return fmt.Errorf("%s derive target spawn: %w", label, err)
	}
	if derived.SpawnID != spawnID || target.DerivedSpawn != derived {
		return fmt.Errorf("%s serialized derived spawn differs from journal identity: got=%+v derived=%+v want_spawn=%q",
			label, target.DerivedSpawn, derived, spawnID)
	}
	if err := ValidateSpawnStateSnapshotIdentity(target.SpawnState, s1cSpawnNamespace); err != nil {
		return fmt.Errorf("%s durable spawn ConfigMap target identity: %w", label, err)
	}
	if !sameKubernetesObjectStableIdentity(
		evidence.ImmediatePreflight.SpawnConfigMapIdentity,
		target.SpawnState.ConfigMapIdentity,
	) {
		return fmt.Errorf("%s durable spawn ConfigMap stable identity changed: preflight=%+v target=%+v",
			label, evidence.ImmediatePreflight.SpawnConfigMapIdentity, target.SpawnState.ConfigMapIdentity)
	}
	if len(target.SpawnState.ActiveIDs) != 1 || target.SpawnState.ActiveIDs[0] != spawnID ||
		target.SpawnState.Statuses[spawnID] != "running" ||
		target.SpawnState.IdempotencyKeys[spawnID] != derived.IdempotencyKey ||
		countStrings(target.SpawnState.RecordIDs, spawnID) != 1 {
		return fmt.Errorf("%s durable spawn proof is not one exact running identity: active=%v records=%v status=%q key=%q",
			label, target.SpawnState.ActiveIDs, target.SpawnState.RecordIDs,
			target.SpawnState.Statuses[spawnID], target.SpawnState.IdempotencyKeys[spawnID])
	}
	if len(target.ActiveSpawnPodNames) != 1 || target.ActiveSpawnPodNames[0] != derived.PodName {
		return fmt.Errorf("%s global active spawn pods are %v, want only %q",
			label, target.ActiveSpawnPodNames, derived.PodName)
	}
	if target.ExactSpawnPodActive != 1 || target.ExactSpawnPodReady != 1 ||
		len(target.ExactSpawnPodNames) != 1 || target.ExactSpawnPodNames[0] != derived.PodName {
		return fmt.Errorf("%s exact spawn pod is not one active Ready workload: active=%d ready=%d names=%v",
			label, target.ExactSpawnPodActive, target.ExactSpawnPodReady, target.ExactSpawnPodNames)
	}
	return nil
}

func isCrashSafetyAgentEvent(eventType string) bool {
	switch eventType {
	case "spawn_requested", "spawn_result", "spawn_resumed":
		return true
	default:
		return false
	}
}

func countStrings(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

func validateCrashLeaseEvidence(
	label, runID, spawnID string,
	crashAt time.Time,
	evidence CrashSafetyEvidence,
) error {
	acquired, renewed := evidence.LeaseAcquired, evidence.LeaseRenewed
	wantAuthority := evidence.ImmediatePreflight.AuthorityPlane.Operator
	if acquired.OperatorAuthority != wantAuthority || renewed.OperatorAuthority != wantAuthority {
		return fmt.Errorf("%s lease REST authority differs from immediate operator: acquired=%+v renewed=%+v operator=%+v",
			label, acquired.OperatorAuthority, renewed.OperatorAuthority, wantAuthority)
	}
	if err := validateOneCrashLease(label+" acquired", runID, spawnID, acquired); err != nil {
		return err
	}
	if err := validateOneCrashLease(label+" renewed", runID, spawnID, renewed); err != nil {
		return err
	}
	if acquired.RequestID != renewed.RequestID || acquired.RunID != renewed.RunID ||
		acquired.SpawnID != renewed.SpawnID {
		return fmt.Errorf("%s acquired/renewed lease identity changed", label)
	}
	if !renewed.ObservedAt.After(acquired.ObservedAt) {
		return fmt.Errorf("%s renewed lease observation %s is not after acquisition %s",
			label, renewed.ObservedAt, acquired.ObservedAt)
	}
	if !renewed.ObservedAt.Before(acquired.ExpiresAt) {
		return fmt.Errorf("%s lease was renewed at or after the acquired lease expiry", label)
	}
	if evidence.DeleteRequestedAt.IsZero() || !evidence.DeleteRequestedAt.Equal(crashAt) {
		return fmt.Errorf("%s DELETE timestamp %s differs from crash timestamp %s",
			label, evidence.DeleteRequestedAt, crashAt)
	}
	if evidence.DeleteIntentRecordedAt.IsZero() || evidence.DeleteAcceptedAt.IsZero() {
		return fmt.Errorf("%s durable delete intent/receipt evidence is incomplete", label)
	}
	if renewed.ObservedAt.After(evidence.DeleteRequestedAt) {
		return fmt.Errorf("%s lease renewal follows DELETE request", label)
	}
	if evidence.ImmediatePreflight.FluxSourcesEnd.ObservedAt.After(acquired.ObservedAt) {
		return fmt.Errorf("%s lease acquisition predates the completed immediate preflight", label)
	}
	if renewed.ObservedAt.After(evidence.Target.ObservedAt) ||
		evidence.Target.ObservedAt.After(evidence.DeleteRequestedAt) {
		return fmt.Errorf("%s final target proof is not ordered between lease renewal and DELETE", label)
	}
	if renewed.ObservedAt.After(evidence.Target.QuiescenceCollectedAt) {
		return fmt.Errorf("%s target quiescence was collected before lease renewal", label)
	}
	if age := evidence.DeleteRequestedAt.Sub(evidence.Target.ObservedAt); age > finalPreDeleteCheckTimeout {
		return fmt.Errorf("%s target safety proof is %s old at DELETE, exceeds %s",
			label, age, finalPreDeleteCheckTimeout)
	}
	if age := evidence.DeleteRequestedAt.Sub(evidence.Target.Quiescence.ObservedAt); age > crashQuiescenceEvidenceMaxAge {
		return fmt.Errorf("%s target quiescence proof is %s old at DELETE, exceeds %s",
			label, age, crashQuiescenceEvidenceMaxAge)
	}
	if evidence.Target.ObservedAt.Before(acquired.ObservedAt) {
		return fmt.Errorf("%s final target proof predates lease acquisition", label)
	}
	if evidence.DeleteRequestedAt.Before(evidence.ImmediatePreflight.FluxSourcesEnd.ObservedAt) {
		return fmt.Errorf("%s DELETE predates the completed immediate preflight", label)
	}
	if evidence.DeleteIntentRecordedAt.Before(acquired.ObservedAt) ||
		!evidence.DeleteIntentRecordedAt.Before(renewed.ObservedAt) {
		return fmt.Errorf("%s durable delete intent is not ordered between lease acquisition and renewal", label)
	}
	if evidence.DeleteAcceptedAt.Before(evidence.DeleteRequestedAt) ||
		evidence.DeleteAcceptedAt.Sub(evidence.DeleteRequestedAt) > podDeleteRequestTimeout {
		return fmt.Errorf("%s accepted DELETE receipt is outside the bounded request window: request=%s accepted=%s",
			label, evidence.DeleteRequestedAt, evidence.DeleteAcceptedAt)
	}
	minimumExpiry := evidence.DeleteRequestedAt.Add(podDeleteRequestTimeout + crashLeaseDeleteSafetyMargin)
	if renewed.ExpiresAt.Before(minimumExpiry) {
		return fmt.Errorf("%s renewed lease expires at %s, need at least %s through %s",
			label, renewed.ExpiresAt,
			podDeleteRequestTimeout+crashLeaseDeleteSafetyMargin, minimumExpiry)
	}
	policy := evidence.PolicyDeleteBoundary
	if err := ValidatePolicyDeleteBoundaryFreshness(
		evidence.DeleteRequestedAt, evidence.ImmediatePreflight, policy,
	); err != nil {
		return fmt.Errorf("%s policy delete-boundary evidence: %w", label, err)
	}
	if renewed.ObservedAt.After(policy.ConfigMapA.ObservedAt) ||
		evidence.Target.ObservedAt.After(policy.ConfigMapA.ObservedAt) ||
		evidence.ImmediatePreflight.FluxSourcesEnd.ObservedAt.After(policy.ConfigMapA.ObservedAt) {
		return fmt.Errorf("%s policy delete-boundary bracket did not follow lease, target, and immediate preflight proofs", label)
	}
	return nil
}

func validateOneCrashLease(label, runID, spawnID string, lease CrashLeaseEvidence) error {
	if strings.TrimSpace(lease.RequestID) == "" || lease.RunID != runID || lease.SpawnID != spawnID ||
		lease.ObservedAt.IsZero() || lease.ExpiresAt.IsZero() {
		return fmt.Errorf("%s lease evidence is incomplete or mismatched: %+v", label, lease)
	}
	if !lease.ExpiresAt.After(lease.ObservedAt) {
		return fmt.Errorf("%s lease expiry %s is not after observation %s",
			label, lease.ExpiresAt, lease.ObservedAt)
	}
	return nil
}
