package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/workflow/killtest"
)

const (
	// Allow the bounded two-minute next-run preflight plus post-final-preflight
	// capture, observer shutdown, evaluation, and atomic artifact writes.
	maxConsecutiveGateRunGap = gatePreflightTimeout + 3*time.Minute

	// Evidence artifacts are untrusted verifier inputs. Keep each allocation
	// bounded even when a regular file is oversized or grows after it is opened.
	maxVerifierEvidenceFileBytes int64 = 32 << 20

	// The typed evidence graph contains many slices and maps. Validate the JSON
	// token stream before typed decoding so a compact file cannot amplify into
	// unbounded container allocations.
	maxVerifierJSONTokens           = 500_000
	maxVerifierJSONNestingDepth     = 128
	maxVerifierJSONContainerEntries = 10_000
	maxVerifierJSONScalarBytes      = 1 << 20
)

var verifierFluxOwners = [...]string{"apps", "bootstrap", "system", "loom-hub-servers"}
var verifierGitRepositoryNames = [...]string{"gitops-gitlab", "loom-core"}

// verifyGateEvidence treats every serialized verdict as untrusted input. It
// recomputes each run through the pure evaluator, binds the wrapper and summary
// copies to that evidence, and requires three distinct canary identities.
func verifyGateEvidence(summaryPath string) error {
	return verifyGateEvidenceWithEvaluator(summaryPath, killtest.Evaluate)
}

func verifyGateEvidenceWithEvaluator(
	summaryPath string,
	evaluate func(killtest.Evidence) killtest.Verdicts,
) error {
	if evaluate == nil {
		return errors.New("evidence evaluator is required")
	}
	var summary gateSummary
	if _, err := readStrictRegularJSONWithSHA256(summaryPath, &summary); err != nil {
		return fmt.Errorf("read gate summary: %w", err)
	}
	if summary.GateContract != killtest.GateBindingContract ||
		summary.GateContractVersion != killtest.GateBindingContractVersion {
		return fmt.Errorf("gate summary has unsupported contract %q-v%d (want %q-v%d)",
			summary.GateContract, summary.GateContractVersion,
			killtest.GateBindingContract, killtest.GateBindingContractVersion)
	}
	if _, err := killtest.CanaryRunIDForGate(summary.GateID, 1); err != nil {
		return fmt.Errorf("gate summary identity: %w", err)
	}
	if summary.GateStartedAt.IsZero() {
		return errors.New("gate summary has no start timestamp")
	}
	// The S1c gate is a three-run contract; the S6-full merging gate is a
	// one-run contract (each merging run merges its canary MR into main,
	// moving the identity baseline). Every run's evidence must declare the
	// mode matching the summary's contract — checked per run below.
	required := summary.RequiredRuns
	if required != killtest.S1cGateRequiredRuns && required != killtest.MergingGateRequiredRuns {
		return fmt.Errorf("gate summary declares unsupported run contract %d (want %d or %d)",
			required, killtest.S1cGateRequiredRuns, killtest.MergingGateRequiredRuns)
	}
	if summary.CompletedRuns != required || !summary.Overall || len(summary.Runs) != required {
		return fmt.Errorf("gate summary is not a completed %d-run gate: required=%d completed=%d overall=%t runs=%d",
			required, summary.RequiredRuns, summary.CompletedRuns, summary.Overall, len(summary.Runs))
	}
	if summary.GitOpsIdentityMode != killtest.GitOpsIdentityModeProtectedScope {
		return fmt.Errorf("gate summary identity mode is %q, S1c requires %q",
			summary.GitOpsIdentityMode, killtest.GitOpsIdentityModeProtectedScope)
	}
	if err := killtest.ValidateAgentType(summary.AgentType); err != nil {
		return fmt.Errorf("gate summary agent type: %w", err)
	}

	runIDs := make(map[string]struct{}, required)
	spawnIDs := make(map[string]struct{}, required)
	paths := make(map[string]struct{}, required)
	var baseline *killtest.PreflightReport
	var previousFinalPreflight *killtest.PreflightReport
	var previousRunEnd time.Time
	previousEvidenceSHA256 := ""
	for index := 1; index <= required; index++ {
		entry := summary.Runs[index-1]
		expectedPath := filepath.Clean(runEvidencePath(summaryPath, index, required, true))
		entryPath := filepath.Clean(entry.EvidencePath)
		if entry.Index != index || entryPath != expectedPath {
			return fmt.Errorf("summary run %d identity/path mismatch: index=%d path=%q want=%q",
				index, entry.Index, entry.EvidencePath, expectedPath)
		}
		if entry.Error != "" || !entry.Overall {
			return fmt.Errorf("summary run %d is not clean: overall=%t error=%q", index, entry.Overall, entry.Error)
		}
		if _, duplicate := paths[entryPath]; duplicate {
			return fmt.Errorf("summary reuses evidence path %q", entry.EvidencePath)
		}
		paths[entryPath] = struct{}{}

		var output runOutput
		actualEvidenceSHA256, err := readStrictRegularJSONWithSHA256(expectedPath, &output)
		if err != nil {
			return fmt.Errorf("read run %d evidence: %w", index, err)
		}
		if err := validateVerifierGateBinding(index, summary, entry, output.Evidence,
			actualEvidenceSHA256, previousEvidenceSHA256); err != nil {
			return err
		}
		if output.Evidence.MergingCanary != (required == killtest.MergingGateRequiredRuns) {
			return fmt.Errorf("run %d merging_canary=%t contradicts the summary's %d-run contract",
				index, output.Evidence.MergingCanary, required)
		}
		if !reflect.DeepEqual(output.Preflight, output.Evidence.InitialPreflight) {
			return fmt.Errorf("run %d wrapper preflight differs from verdict evidence", index)
		}
		if output.FinalPreflight == nil || !reflect.DeepEqual(*output.FinalPreflight, output.Evidence.FinalPreflight) {
			return fmt.Errorf("run %d wrapper final preflight differs from verdict evidence", index)
		}
		if err := requireProtectedScopeEvidence(index, output.Evidence); err != nil {
			return err
		}
		if err := validateRunGateIdentity(index, output.Evidence); err != nil {
			return err
		}
		if err := validateVerifierPolicyDeleteBoundaries(index, output.Evidence); err != nil {
			return err
		}
		if previousFinalPreflight != nil {
			if err := killtest.ValidateInterRunPodContinuity(*previousFinalPreflight, output.Evidence.InitialPreflight); err != nil {
				return fmt.Errorf("run %d inter-run pod continuity: %w", index, err)
			}
		}
		runStart, runEnd, err := gateRunWindow(index, output.Evidence)
		if err != nil {
			return err
		}
		if index == 1 && summary.GateStartedAt.After(runStart) {
			return fmt.Errorf("gate start %s is after run 1 started at %s", summary.GateStartedAt, runStart)
		}
		if index == 1 {
			if delay := runStart.Sub(summary.GateStartedAt); delay > maxConsecutiveGateRunGap {
				return fmt.Errorf("run 1 starts %s after gate allocation, exceeds gate-start limit %s",
					delay, maxConsecutiveGateRunGap)
			}
		}
		if !previousRunEnd.IsZero() {
			if !previousRunEnd.Before(runStart) {
				return fmt.Errorf("run %d does not start strictly after run %d completed: previous_end=%s start=%s",
					index, index-1, previousRunEnd, runStart)
			}
			if gap := runStart.Sub(previousRunEnd); gap > maxConsecutiveGateRunGap {
				return fmt.Errorf("run %d starts %s after run %d, exceeds consecutive-gate limit %s",
					index, gap, index-1, maxConsecutiveGateRunGap)
			}
		}
		previousRunEnd = runEnd

		recomputed := evaluate(output.Evidence)
		if !reflect.DeepEqual(recomputed, output.Verdicts) {
			return fmt.Errorf("run %d serialized verdicts differ from pure recomputation: serialized=%+v recomputed=%+v",
				index, output.Verdicts, recomputed)
		}
		if !recomputed.Overall {
			return fmt.Errorf("run %d evidence recomputes to FAIL: %+v", index, recomputed)
		}
		if strings.TrimSpace(output.Evidence.RunID) == "" || strings.TrimSpace(output.Evidence.SpawnID) == "" {
			return fmt.Errorf("run %d has incomplete run/spawn identity", index)
		}
		if _, duplicate := runIDs[output.Evidence.RunID]; duplicate {
			return fmt.Errorf("run %d reuses run_id %q", index, output.Evidence.RunID)
		}
		if _, duplicate := spawnIDs[output.Evidence.SpawnID]; duplicate {
			return fmt.Errorf("run %d reuses spawn_id %q", index, output.Evidence.SpawnID)
		}
		runIDs[output.Evidence.RunID] = struct{}{}
		spawnIDs[output.Evidence.SpawnID] = struct{}{}
		if entry.RunID != output.Evidence.RunID || entry.AgentType != output.Evidence.AgentType ||
			entry.FinalState != output.Evidence.Final.Run.State || entry.Overall != recomputed.Overall {
			return fmt.Errorf("summary run %d does not match evidence identity/state", index)
		}
		if baseline == nil {
			copy := output.Evidence.InitialPreflight
			baseline = &copy
		} else if err := sameVerifierGateIdentity(*baseline, output.Evidence.InitialPreflight); err != nil {
			return fmt.Errorf("run %d gate identity drift: %w", index, err)
		}
		finalCopy := output.Evidence.FinalPreflight
		previousFinalPreflight = &finalCopy
		previousEvidenceSHA256 = actualEvidenceSHA256
	}

	if baseline == nil {
		return errors.New("gate summary contained no evidence runs")
	}
	if summary.AgentType == "" || summary.OperatorImage != baseline.Operator.ImageID ||
		summary.HudImage != baseline.Hud.ImageID || summary.PolicyChecksum != baseline.PolicyChecksum ||
		summary.GitOpsIdentityMode != baseline.GitOpsIdentity.Mode ||
		summary.GitOpsBaseline != baseline.GitOpsIdentity.BaselineRevision ||
		summary.GitOpsScopeDigest != baseline.GitOpsIdentity.ObservedDigest ||
		summary.LoomCoreBaseline != baseline.LoomCoreIdentity.BaselineRevision ||
		summary.LoomCoreScopeDigest != baseline.LoomCoreIdentity.ObservedDigest {
		return errors.New("gate summary immutable identity does not match run evidence")
	}
	for _, entry := range summary.Runs {
		if entry.AgentType != summary.AgentType {
			return fmt.Errorf("summary run %d agent type %q differs from gate agent type %q",
				entry.Index, entry.AgentType, summary.AgentType)
		}
	}
	return nil
}

func validateVerifierGateBinding(
	index int,
	summary gateSummary,
	entry gateRunSummary,
	evidence killtest.Evidence,
	actualEvidenceSHA256 string,
	previousEvidenceSHA256 string,
) error {
	binding := evidence.GateBinding
	if binding == (killtest.GateBinding{}) {
		return fmt.Errorf("run %d has no S1c gate binding", index)
	}
	if err := killtest.ValidateGateBinding(evidence); err != nil {
		return fmt.Errorf("run %d gate binding: %w", index, err)
	}
	wantRunID, err := killtest.CanaryRunIDForGate(summary.GateID, index)
	if err != nil {
		return fmt.Errorf("run %d canonical identity: %w", index, err)
	}
	if binding.Contract != summary.GateContract ||
		binding.ContractVersion != summary.GateContractVersion ||
		binding.GateID != summary.GateID || binding.RunIndex != index ||
		binding.RequiredRuns != summary.RequiredRuns ||
		!binding.GateStartedAt.Equal(summary.GateStartedAt) {
		return fmt.Errorf("run %d gate binding differs from summary: binding=%+v", index, binding)
	}
	if evidence.RunID != wantRunID || entry.RunID != wantRunID {
		return fmt.Errorf("run %d identity is not canonical: evidence=%q summary=%q want=%q",
			index, evidence.RunID, entry.RunID, wantRunID)
	}
	if entry.EvidenceSHA256 != actualEvidenceSHA256 {
		return fmt.Errorf("summary run %d evidence SHA-256 %q differs from actual file %q",
			index, entry.EvidenceSHA256, actualEvidenceSHA256)
	}
	if entry.PreviousEvidenceSHA256 != binding.PreviousEvidenceSHA256 ||
		binding.PreviousEvidenceSHA256 != previousEvidenceSHA256 {
		return fmt.Errorf("run %d predecessor evidence SHA-256 mismatch: summary=%q binding=%q previous_actual=%q",
			index, entry.PreviousEvidenceSHA256, binding.PreviousEvidenceSHA256, previousEvidenceSHA256)
	}
	return nil
}

func requireProtectedScopeEvidence(index int, evidence killtest.Evidence) error {
	reports := []struct {
		name   string
		report killtest.PreflightReport
	}{
		{"initial", evidence.InitialPreflight},
		{"CRASH A immediate", evidence.CrashASafety.ImmediatePreflight},
		{"CRASH B immediate", evidence.CrashBSafety.ImmediatePreflight},
		{"final", evidence.FinalPreflight},
	}
	for _, sample := range reports {
		if err := requireProtectedScopeReport(sample.report); err != nil {
			return fmt.Errorf("run %d %s preflight: %w", index, sample.name, err)
		}
	}
	return nil
}

func validateVerifierPolicyDeleteBoundaries(index int, evidence killtest.Evidence) error {
	for _, crash := range []struct {
		name     string
		evidence killtest.CrashSafetyEvidence
	}{
		{"CRASH A", evidence.CrashASafety},
		{"CRASH B", evidence.CrashBSafety},
	} {
		if err := killtest.ValidatePolicyDeleteBoundaryFreshness(
			crash.evidence.DeleteRequestedAt,
			crash.evidence.ImmediatePreflight,
			crash.evidence.PolicyDeleteBoundary,
		); err != nil {
			return fmt.Errorf("run %d %s policy delete-boundary evidence: %w", index, crash.name, err)
		}
	}
	return nil
}

func requireProtectedScopeReport(report killtest.PreflightReport) error {
	if err := killtest.ValidatePolicyConfigMapProvenance(report); err != nil {
		return fmt.Errorf("policy ConfigMap provenance: %w", err)
	}
	identities := []struct {
		name     string
		identity killtest.GitOpsScopeIdentity
	}{
		{"apps start", report.GitOpsStartIdentity},
		{"apps end", report.GitOpsIdentity},
		{"bootstrap start", report.GitOpsBootstrapStartIdentity},
		{"bootstrap end", report.GitOpsBootstrapIdentity},
		{"system start", report.GitOpsSystemStartIdentity},
		{"system end", report.GitOpsSystemIdentity},
		{"loom-hub-servers start", report.LoomCoreStartIdentity},
		{"loom-hub-servers end", report.LoomCoreIdentity},
	}
	for _, source := range identities {
		if source.identity.Mode != killtest.GitOpsIdentityModeProtectedScope {
			return fmt.Errorf("%s identity mode is %q, want %q", source.name, source.identity.Mode,
				killtest.GitOpsIdentityModeProtectedScope)
		}
	}
	for _, group := range []struct {
		phase                string
		apps, bootstrap, sys killtest.GitOpsScopeIdentity
	}{
		{"start", report.GitOpsStartIdentity, report.GitOpsBootstrapStartIdentity, report.GitOpsSystemStartIdentity},
		{"end", report.GitOpsIdentity, report.GitOpsBootstrapIdentity, report.GitOpsSystemIdentity},
	} {
		apps := verifierIdentity(group.apps)
		if verifierIdentity(group.bootstrap) != apps {
			return fmt.Errorf("bootstrap %s protected identity differs from apps platform baseline", group.phase)
		}
		if verifierIdentity(group.sys) != apps {
			return fmt.Errorf("system %s protected identity differs from apps platform baseline", group.phase)
		}
	}
	for _, snapshot := range []struct {
		name  string
		value killtest.FluxSourceProvenanceSnapshot
	}{
		{"start", report.FluxSourcesStart},
		{"end", report.FluxSourcesEnd},
	} {
		sources, err := verifierFluxObjectIdentity(snapshot.value)
		if err != nil {
			return fmt.Errorf("%s source snapshot: %w", snapshot.name, err)
		}
		repositories, err := verifierGitRepositoryIdentity(snapshot.value)
		if err != nil {
			return fmt.Errorf("%s GitRepository snapshot: %w", snapshot.name, err)
		}
		for _, name := range verifierGitRepositoryNames {
			if repositories[name].Identity.Mode != killtest.GitOpsIdentityModeProtectedScope {
				return fmt.Errorf("%s GitRepository %s identity mode is %q, want %q",
					snapshot.name, name, repositories[name].Identity.Mode,
					killtest.GitOpsIdentityModeProtectedScope)
			}
		}
		for _, owner := range verifierFluxOwners {
			if sources[owner].Identity.Mode != killtest.GitOpsIdentityModeProtectedScope {
				return fmt.Errorf("%s %s identity mode is %q, want %q", snapshot.name, owner,
					sources[owner].Identity.Mode, killtest.GitOpsIdentityModeProtectedScope)
			}
		}
		for _, owner := range []string{"bootstrap", "system"} {
			if sources[owner].Identity != sources["apps"].Identity {
				return fmt.Errorf("%s %s protected identity differs from apps platform baseline", snapshot.name, owner)
			}
		}
		platform := sources["apps"].Identity
		for _, owner := range verifierFluxOwners {
			render := sources[owner].RenderSpec
			if render.ReviewedRevision != platform.BaselineRevision ||
				render.ReviewedScopeDigest != platform.BaselineDigest {
				return fmt.Errorf("%s %s render manifest is not bound to the apps platform review baseline",
					snapshot.name, owner)
			}
		}
	}
	return nil
}

func validateRunGateIdentity(index int, evidence killtest.Evidence) error {
	baseline := evidence.InitialPreflight
	for _, sample := range []struct {
		name   string
		report killtest.PreflightReport
	}{
		{"initial", baseline},
		{"CRASH A immediate", evidence.CrashASafety.ImmediatePreflight},
		{"CRASH B immediate", evidence.CrashBSafety.ImmediatePreflight},
		{"final", evidence.FinalPreflight},
	} {
		if err := sameVerifierGateIdentity(baseline, sample.report); err != nil {
			return fmt.Errorf("run %d %s gate identity drift: %w", index, sample.name, err)
		}
	}
	return nil
}

func gateRunWindow(index int, evidence killtest.Evidence) (time.Time, time.Time, error) {
	start := evidence.InitialPreflight.FluxSourcesStart.GitRepositoriesOpenedAt
	end := evidence.FinalPreflight.FluxSourcesEnd.GitRepositories.ObservedAt
	if start.IsZero() || end.IsZero() || !start.Before(end) {
		return time.Time{}, time.Time{}, fmt.Errorf("run %d has invalid gate window: start=%s end=%s", index, start, end)
	}
	return start, end, nil
}

type verifierFluxObject struct {
	UID               string
	Generation        int64
	DeletionTimestamp string
	Terminating       bool
	RenderSpec        killtest.FluxRenderSpecIdentity
	Identity          verifierSourceIdentity
}

type verifierGitRepositoryObject struct {
	UID               string
	Generation        int64
	DeletionTimestamp string
	Terminating       bool
	ArtifactRevision  string
	ArtifactDigest    string
	Spec              killtest.GitRepositorySpecIdentity
	Identity          verifierSourceIdentity
}

type verifierSourceIdentity struct {
	Mode             string
	Contract         string
	ContractVersion  int
	BaselineRevision string
	BaselineDigest   string
	ObservedDigest   string
}

func verifierIdentity(identity killtest.GitOpsScopeIdentity) verifierSourceIdentity {
	return verifierSourceIdentity{
		Mode: identity.Mode, Contract: identity.Contract, ContractVersion: identity.ContractVersion,
		BaselineRevision: identity.BaselineRevision, BaselineDigest: identity.BaselineDigest,
		ObservedDigest: identity.ObservedDigest,
	}
}

func verifierFluxObjectIdentity(snapshot killtest.FluxSourceProvenanceSnapshot) (map[string]verifierFluxObject, error) {
	if err := killtest.ValidateFluxSourceProvenanceSnapshot(snapshot); err != nil {
		return nil, err
	}
	objects := make(map[string]verifierFluxObject, len(snapshot.Sources))
	for _, source := range snapshot.Sources {
		if _, duplicate := objects[source.Name]; duplicate {
			return nil, fmt.Errorf("duplicate Flux owner %q", source.Name)
		}
		objects[source.Name] = verifierFluxObject{
			UID: source.UID, Generation: source.Generation,
			DeletionTimestamp: source.DeletionTimestamp, Terminating: source.Terminating,
			RenderSpec: source.RenderSpec,
			Identity:   verifierIdentity(source.ProtectedIdentity),
		}
	}
	if len(objects) != len(verifierFluxOwners) {
		return nil, fmt.Errorf("got %d Flux owners, want %d", len(objects), len(verifierFluxOwners))
	}
	for _, owner := range verifierFluxOwners {
		object, ok := objects[owner]
		if !ok || object.UID == "" || object.Generation <= 0 || object.Terminating ||
			strings.TrimSpace(object.DeletionTimestamp) != "" || object.RenderSpec.SpecSHA256 == "" ||
			object.RenderSpec.SpecSHA256 != object.RenderSpec.ReviewedSpecSHA256 ||
			object.RenderSpec.ManifestPath == "" || object.RenderSpec.ReviewedRevision == "" ||
			object.RenderSpec.ReviewedScopeDigest == "" ||
			object.Identity.Contract == "" ||
			object.Identity.ContractVersion <= 0 || object.Identity.BaselineRevision == "" ||
			object.Identity.BaselineDigest == "" || object.Identity.ObservedDigest == "" {
			return nil, fmt.Errorf("flux owner %q has incomplete stable identity", owner)
		}
	}
	return objects, nil
}

func verifierGitRepositoryIdentity(
	snapshot killtest.FluxSourceProvenanceSnapshot,
) (map[string]verifierGitRepositoryObject, error) {
	repositorySnapshot := snapshot.GitRepositories
	if snapshot.GitRepositoriesOpenedAt.IsZero() ||
		snapshot.GitRepositoriesOpenedAt.After(snapshot.ObservedAt) ||
		repositorySnapshot.ListResourceVersion == "" || repositorySnapshot.ObservedAt.IsZero() ||
		repositorySnapshot.ObservedAt.Before(snapshot.ObservedAt) {
		return nil, errors.New("GitRepository List identity/timestamp is incomplete")
	}
	if len(repositorySnapshot.Repositories) != len(verifierGitRepositoryNames) {
		return nil, fmt.Errorf("got %d GitRepositories, want %d",
			len(repositorySnapshot.Repositories), len(verifierGitRepositoryNames))
	}
	objects := make(map[string]verifierGitRepositoryObject, len(repositorySnapshot.Repositories))
	sources := make(map[string]killtest.FluxSourceProvenance, len(snapshot.Sources))
	for _, source := range snapshot.Sources {
		sources[source.Name] = source
	}
	for _, repository := range repositorySnapshot.Repositories {
		if _, duplicate := objects[repository.Name]; duplicate {
			return nil, fmt.Errorf("duplicate GitRepository %q", repository.Name)
		}
		if repository.Namespace != "flux-system" || repository.UID == "" ||
			repository.ResourceVersion == "" || repository.Generation <= 0 ||
			repository.Terminating || strings.TrimSpace(repository.DeletionTimestamp) != "" ||
			repository.StatusObservedGeneration != repository.Generation ||
			repository.ReadyObservedGeneration != repository.Generation || repository.ReadyStatus != "True" ||
			repository.ArtifactInStorageObservedGeneration != repository.Generation ||
			repository.ArtifactInStorageStatus != "True" {
			return nil, fmt.Errorf("GitRepository %q has incomplete live identity/status", repository.Name)
		}
		if !strings.HasPrefix(repository.ArtifactDigest, "sha256:") ||
			!verifierNormalizedSHA256(strings.TrimPrefix(repository.ArtifactDigest, "sha256:")) {
			return nil, fmt.Errorf("GitRepository %q has invalid artifact digest", repository.Name)
		}
		identity := verifierIdentity(repository.ProtectedIdentity)
		artifactRevision := verifierNormalizedRevision(repository.ArtifactRevision)
		if artifactRevision == "" || artifactRevision != repository.ProtectedIdentity.ObservedRevision {
			return nil, fmt.Errorf("GitRepository %q artifact revision is not bound to protected identity", repository.Name)
		}
		if err := validateVerifierGitRepositorySpec(repository.Name, repository.Spec); err != nil {
			return nil, err
		}
		objects[repository.Name] = verifierGitRepositoryObject{
			UID: repository.UID, Generation: repository.Generation,
			DeletionTimestamp: repository.DeletionTimestamp, Terminating: repository.Terminating,
			ArtifactRevision: repository.ArtifactRevision, ArtifactDigest: repository.ArtifactDigest,
			Spec: repository.Spec, Identity: identity,
		}
	}
	for _, name := range verifierGitRepositoryNames {
		if _, ok := objects[name]; !ok {
			return nil, fmt.Errorf("missing GitRepository %q", name)
		}
	}
	platform := objects["gitops-gitlab"]
	for _, name := range verifierGitRepositoryNames {
		spec := objects[name].Spec
		if spec.ReviewedRevision != platform.Identity.BaselineRevision ||
			spec.ReviewedScopeDigest != platform.Identity.BaselineDigest {
			return nil, fmt.Errorf("GitRepository %s reviewed spec is not bound to platform baseline", name)
		}
	}
	for _, owner := range []string{"apps", "bootstrap", "system"} {
		if sources[owner].AppliedRevision != platform.ArtifactRevision {
			return nil, fmt.Errorf("flux %s revision is not bound to gitops-gitlab artifact", owner)
		}
	}
	if sources["loom-hub-servers"].AppliedRevision != objects["loom-core"].ArtifactRevision {
		return nil, errors.New("flux loom-hub-servers revision is not bound to loom-core artifact")
	}
	return objects, nil
}

func validateVerifierGitRepositorySpec(name string, spec killtest.GitRepositorySpecIdentity) error {
	wantURL, wantPath := "", ""
	switch name {
	case "gitops-gitlab":
		wantURL = "http://gitlab-vm.gitlab.svc.cluster.local/platform/gitops.git"
		wantPath = "clusters/k3s/flux-system/gitrepository-gitlab.yaml"
	case "loom-core":
		wantURL = "http://gitlab-vm.gitlab.svc.cluster.local/services/loom-core.git"
		wantPath = "clusters/k3s/flux-system/gitrepository-loom-core.yaml"
	default:
		return fmt.Errorf("unexpected GitRepository %q", name)
	}
	if spec.URL != wantURL || spec.RefBranch != "main" || spec.SecretRefName != "gitops-gitlab" ||
		spec.ManifestPath != wantPath || !verifierNormalizedSHA256(spec.SpecSHA256) ||
		spec.SpecSHA256 != spec.ReviewedSpecSHA256 || spec.ReviewedRevision == "" ||
		spec.ReviewedScopeDigest == "" {
		return fmt.Errorf("GitRepository %q has incomplete or redirected reviewed spec", name)
	}
	return nil
}

func verifierNormalizedRevision(value string) string {
	if index := strings.LastIndex(value, ":"); index >= 0 {
		value = value[index+1:]
	}
	if len(value) != 40 && len(value) != 64 || strings.ToLower(value) != value {
		return ""
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return ""
		}
	}
	return value
}

func verifierNormalizedSHA256(value string) bool {
	return len(value) == 64 && verifierNormalizedRevision(value) == value
}

func sameVerifierGateIdentity(want, got killtest.PreflightReport) error {
	if err := sameGateIdentity(want, got); err != nil {
		return err
	}
	if err := killtest.ValidateDeploymentProvenance(want); err != nil {
		return fmt.Errorf("baseline Deployment provenance: %w", err)
	}
	if err := killtest.ValidateDeploymentProvenance(got); err != nil {
		return fmt.Errorf("observed Deployment provenance: %w", err)
	}
	if err := killtest.ValidatePolicyConfigMapGateIdentity(want, got); err != nil {
		return fmt.Errorf("policy ConfigMap provenance: %w", err)
	}
	stableDeploymentIdentity := func(deployment killtest.DeploymentIdentity) any {
		return struct {
			Name, Namespace, UID, Image, Strategy, PolicyChecksum string
			Generation                                            int64
			SpecSHA256, ReviewedSpecSHA256                        string
			Review                                                killtest.DeploymentReviewIdentity
		}{
			deployment.Name, deployment.Namespace, deployment.UID, deployment.Image,
			deployment.Strategy, deployment.PolicyChecksum, deployment.Generation,
			deployment.SpecSHA256, deployment.ReviewedSpecSHA256, deployment.Review,
		}
	}
	if stableDeploymentIdentity(want.OperatorDeployment) != stableDeploymentIdentity(got.OperatorDeployment) ||
		stableDeploymentIdentity(want.HudDeployment) != stableDeploymentIdentity(got.HudDeployment) {
		return errors.New("deployment UID, generation, full spec, or reviewed render identity changed")
	}
	if want.ConfigMapPolicyEnabled != got.ConfigMapPolicyEnabled ||
		want.FlagEnabled != got.FlagEnabled || want.SubstrateK8sOnly != got.SubstrateK8sOnly ||
		want.EffectivePolicyEnabled != got.EffectivePolicyEnabled ||
		want.EffectiveFlagEnabled != got.EffectiveFlagEnabled ||
		want.EffectiveSubstrateK8sOnly != got.EffectiveSubstrateK8sOnly ||
		want.EffectivePolicyMatchesConfigMap != got.EffectivePolicyMatchesConfigMap ||
		want.SpawnConfigMap != got.SpawnConfigMap ||
		want.SpawnConfigMapUpdateAllowed != got.SpawnConfigMapUpdateAllowed {
		return errors.New("policy or durable spawn safety identity changed")
	}
	wantSources, err := verifierFluxObjectIdentity(want.FluxSourcesEnd)
	if err != nil {
		return fmt.Errorf("baseline Flux identity: %w", err)
	}
	wantRepositories, err := verifierGitRepositoryIdentity(want.FluxSourcesEnd)
	if err != nil {
		return fmt.Errorf("baseline GitRepository identity: %w", err)
	}
	for _, snapshot := range []struct {
		name  string
		value killtest.FluxSourceProvenanceSnapshot
	}{
		{"start", got.FluxSourcesStart},
		{"end", got.FluxSourcesEnd},
	} {
		gotSources, err := verifierFluxObjectIdentity(snapshot.value)
		if err != nil {
			return fmt.Errorf("%s Flux identity: %w", snapshot.name, err)
		}
		gotRepositories, err := verifierGitRepositoryIdentity(snapshot.value)
		if err != nil {
			return fmt.Errorf("%s GitRepository identity: %w", snapshot.name, err)
		}
		for _, owner := range verifierFluxOwners {
			if gotSources[owner] != wantSources[owner] {
				return fmt.Errorf("flux owner %s stable identity changed at %s snapshot", owner, snapshot.name)
			}
		}
		for _, name := range verifierGitRepositoryNames {
			wantRepository := wantRepositories[name]
			gotRepository := gotRepositories[name]
			if gotRepository.UID != wantRepository.UID ||
				gotRepository.Generation != wantRepository.Generation ||
				gotRepository.DeletionTimestamp != wantRepository.DeletionTimestamp ||
				gotRepository.Terminating != wantRepository.Terminating ||
				gotRepository.Spec != wantRepository.Spec ||
				gotRepository.Identity != wantRepository.Identity {
				return fmt.Errorf("GitRepository %s stable identity changed at %s snapshot", name, snapshot.name)
			}
			wantRevision := verifierNormalizedRevision(wantRepository.ArtifactRevision)
			gotRevision := verifierNormalizedRevision(gotRepository.ArtifactRevision)
			if wantRevision == gotRevision && wantRepository.ArtifactDigest != gotRepository.ArtifactDigest {
				return fmt.Errorf("GitRepository %s artifact digest changed without a revision change at %s snapshot",
					name, snapshot.name)
			}
		}
	}
	return nil
}

// readStrictRegularJSONWithSHA256 parses and fingerprints the same bytes from
// one opened regular file. Checking the opened descriptor against the path on
// both sides of the read prevents a symlink or rename swap from redirecting the
// verifier between its file-kind, digest, and JSON checks.
func readStrictRegularJSONWithSHA256(path string, destination any) (string, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a regular non-symlink file", path)
	}

	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return "", fmt.Errorf("%q changed before its evidence read", path)
	}
	if openedInfo.Size() > maxVerifierEvidenceFileBytes {
		return "", fmt.Errorf("%q exceeds the %d-byte evidence file limit", path, maxVerifierEvidenceFileBytes)
	}
	blob, err := io.ReadAll(io.LimitReader(file, maxVerifierEvidenceFileBytes+1))
	if err != nil {
		return "", err
	}
	afterInfo, err := file.Stat()
	if err != nil {
		return "", err
	}
	currentInfo, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if currentInfo.Mode()&os.ModeSymlink != 0 || !currentInfo.Mode().IsRegular() ||
		!os.SameFile(openedInfo, currentInfo) {
		return "", fmt.Errorf("%q changed during its evidence read", path)
	}
	if int64(len(blob)) > maxVerifierEvidenceFileBytes {
		return "", fmt.Errorf("%q exceeds the %d-byte evidence file limit", path, maxVerifierEvidenceFileBytes)
	}
	if openedInfo.Size() != int64(len(blob)) || afterInfo.Size() != int64(len(blob)) ||
		!openedInfo.ModTime().Equal(afterInfo.ModTime()) {
		return "", fmt.Errorf("%q was modified during its evidence read", path)
	}
	if err := decodeStrictJSON(blob, destination); err != nil {
		return "", err
	}
	digest := sha256.Sum256(blob)
	return fmt.Sprintf("%x", digest), nil
}

type verifierJSONContainer struct {
	kind         json.Delim
	entries      int
	expectingKey bool
}

func decodeStrictJSON(blob []byte, destination any) error {
	if err := validateVerifierJSONShape(bytes.NewReader(blob)); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(blob))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return fmt.Errorf("trailing JSON: %w", err)
	}
	return nil
}

func validateVerifierJSONShape(reader io.Reader) error {
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	stack := make([]verifierJSONContainer, 0, 16)
	tokens := 0
	rootValues := 0

	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if rootValues > 0 && len(stack) == 0 {
				return fmt.Errorf("trailing JSON: %w", err)
			}
			return err
		}
		tokens++
		if tokens > maxVerifierJSONTokens {
			return fmt.Errorf("JSON token count exceeds %d", maxVerifierJSONTokens)
		}

		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				if err := recordVerifierJSONValue(stack, &rootValues); err != nil {
					return err
				}
				if len(stack)+1 > maxVerifierJSONNestingDepth {
					return fmt.Errorf("JSON nesting depth exceeds %d", maxVerifierJSONNestingDepth)
				}
				stack = append(stack, verifierJSONContainer{
					kind: delimiter, expectingKey: delimiter == '{',
				})
			case '}', ']':
				if len(stack) == 0 ||
					(delimiter == '}' && stack[len(stack)-1].kind != '{') ||
					(delimiter == ']' && stack[len(stack)-1].kind != '[') {
					return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
				}
				if delimiter == '}' && !stack[len(stack)-1].expectingKey {
					return errors.New("JSON object field has no value")
				}
				stack = stack[:len(stack)-1]
			}
			continue
		}

		switch scalar := token.(type) {
		case string:
			if len(scalar) > maxVerifierJSONScalarBytes {
				return fmt.Errorf("JSON string token exceeds %d bytes", maxVerifierJSONScalarBytes)
			}
		case json.Number:
			if len(scalar) > maxVerifierJSONScalarBytes {
				return fmt.Errorf("JSON number token exceeds %d bytes", maxVerifierJSONScalarBytes)
			}
		}
		if len(stack) > 0 && stack[len(stack)-1].kind == '{' && stack[len(stack)-1].expectingKey {
			if _, ok := token.(string); !ok {
				return errors.New("JSON object key is not a string")
			}
			container := &stack[len(stack)-1]
			container.entries++
			if container.entries > maxVerifierJSONContainerEntries {
				return fmt.Errorf("JSON object field count exceeds %d", maxVerifierJSONContainerEntries)
			}
			container.expectingKey = false
			continue
		}
		if err := recordVerifierJSONValue(stack, &rootValues); err != nil {
			return err
		}
	}
	if len(stack) != 0 {
		return errors.New("incomplete JSON container")
	}
	return nil
}

func recordVerifierJSONValue(stack []verifierJSONContainer, rootValues *int) error {
	if len(stack) == 0 {
		*rootValues = *rootValues + 1
		if *rootValues > 1 {
			return errors.New("multiple JSON values")
		}
		return nil
	}
	container := &stack[len(stack)-1]
	if container.kind == '[' {
		container.entries++
		if container.entries > maxVerifierJSONContainerEntries {
			return fmt.Errorf("JSON array element count exceeds %d", maxVerifierJSONContainerEntries)
		}
		return nil
	}
	if container.expectingKey {
		return errors.New("JSON object value appeared where a key was required")
	}
	container.expectingKey = true
	return nil
}
