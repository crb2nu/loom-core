package killtest

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// ValidateFluxSourceProvenanceSnapshot validates a serialized coherent Flux
// snapshot without cluster or Git access. It is intentionally strict so old,
// partial, or hand-edited evidence cannot satisfy the current contract.
func ValidateFluxSourceProvenanceSnapshot(snapshot FluxSourceProvenanceSnapshot) error {
	if snapshot.Contract != FluxProvenanceContract || snapshot.ContractVersion != FluxProvenanceContractVersion {
		return fmt.Errorf("unsupported Flux provenance contract %q-v%d (want %q-v%d)",
			snapshot.Contract, snapshot.ContractVersion, FluxProvenanceContract, FluxProvenanceContractVersion)
	}
	if strings.TrimSpace(snapshot.ListResourceVersion) == "" {
		return fmt.Errorf("flux provenance List has no resourceVersion")
	}
	if snapshot.ObservedAt.IsZero() {
		return fmt.Errorf("flux provenance List has no observation timestamp")
	}
	if snapshot.GitRepositoriesOpenedAt.IsZero() {
		return fmt.Errorf("flux provenance has no opening GitRepository observation timestamp")
	}
	if err := validateGitRepositoryProvenanceSnapshot(snapshot.GitRepositories); err != nil {
		return fmt.Errorf("GitRepository provenance: %w", err)
	}
	if snapshot.GitRepositoriesOpenedAt.After(snapshot.ObservedAt) {
		return fmt.Errorf("GitRepository opening snapshot %s follows Kustomization snapshot %s",
			snapshot.GitRepositoriesOpenedAt, snapshot.ObservedAt)
	}
	if snapshot.GitRepositories.ObservedAt.Before(snapshot.ObservedAt) {
		return fmt.Errorf("GitRepository closing snapshot %s predates Kustomization snapshot %s",
			snapshot.GitRepositories.ObservedAt, snapshot.ObservedAt)
	}
	if len(snapshot.Sources) != len(requiredFluxProvenanceOwners) {
		return fmt.Errorf("flux provenance has %d owners, want %d", len(snapshot.Sources), len(requiredFluxProvenanceOwners))
	}

	seen := make(map[string]bool, len(snapshot.Sources))
	sources := make(map[string]FluxSourceProvenance, len(snapshot.Sources))
	for _, source := range snapshot.Sources {
		if seen[source.Name] {
			return fmt.Errorf("flux provenance contains duplicate %q owner", source.Name)
		}
		seen[source.Name] = true
		sources[source.Name] = source
		if err := validateFluxSourceProvenance(source); err != nil {
			return err
		}
	}
	for _, name := range requiredFluxProvenanceOwners {
		if !seen[name] {
			return fmt.Errorf("flux provenance is missing %q owner", name)
		}
	}
	if err := validateFluxSnapshotRenderBindings(sources); err != nil {
		return err
	}
	return validateGitRepositoryConsumerBindings(snapshot.GitRepositories, sources)
}

func validateFluxSourceProvenance(source FluxSourceProvenance) error {
	if !isRequiredFluxProvenanceOwner(source.Name) {
		return fmt.Errorf("flux provenance contains unexpected %q owner", source.Name)
	}
	if strings.TrimSpace(source.UID) == "" || strings.TrimSpace(source.ResourceVersion) == "" {
		return fmt.Errorf("flux %s provenance has incomplete object identity: uid=%q resourceVersion=%q",
			source.Name, source.UID, source.ResourceVersion)
	}
	if source.Terminating || strings.TrimSpace(source.DeletionTimestamp) != "" {
		return fmt.Errorf("flux %s provenance is terminating: terminating=%t deletionTimestamp=%q",
			source.Name, source.Terminating, source.DeletionTimestamp)
	}
	if source.Generation <= 0 || source.ReadyObservedGeneration != source.Generation {
		return fmt.Errorf("flux %s provenance has stale Ready generation: generation=%d observedGeneration=%d",
			source.Name, source.Generation, source.ReadyObservedGeneration)
	}
	if source.ReadyStatus != "True" || strings.TrimSpace(source.AppliedRevision) == "" ||
		source.AppliedRevision != source.AttemptedRevision {
		return fmt.Errorf("flux %s provenance is not converged: ready=%q applied=%q attempted=%q",
			source.Name, source.ReadyStatus, source.AppliedRevision, source.AttemptedRevision)
	}
	if err := validateFluxRenderSpecIdentity(source.Name, source.RenderSpec); err != nil {
		return err
	}
	return validateFluxProtectedIdentity(source)
}

func validateFluxRenderSpecIdentity(name string, got FluxRenderSpecIdentity) error {
	want, ok := requiredFluxRenderSpecs[name]
	if !ok {
		return fmt.Errorf("flux %s has no render-spec contract", name)
	}
	if !isNormalizedSHA256(got.SpecSHA256) {
		return fmt.Errorf("flux %s render spec has invalid SHA-256 %q", name, got.SpecSHA256)
	}
	if !isNormalizedSHA256(got.ReviewedSpecSHA256) || got.SpecSHA256 != got.ReviewedSpecSHA256 {
		return fmt.Errorf("flux %s live/reviewed render spec SHA-256 mismatch: %q/%q",
			name, got.SpecSHA256, got.ReviewedSpecSHA256)
	}
	reviewedRevision, err := normalizeGitOpsScopeRevision(got.ReviewedRevision)
	if err != nil || reviewedRevision != got.ReviewedRevision {
		return fmt.Errorf("flux %s render spec has invalid reviewed revision %q: %v",
			name, got.ReviewedRevision, err)
	}
	if strings.TrimSpace(got.ReviewedScopeDigest) == "" {
		return fmt.Errorf("flux %s render spec has no reviewed scope digest", name)
	}
	want.SpecSHA256 = got.SpecSHA256
	want.ReviewedSpecSHA256 = got.ReviewedSpecSHA256
	want.ReviewedRevision = got.ReviewedRevision
	want.ReviewedScopeDigest = got.ReviewedScopeDigest
	if got != want {
		return fmt.Errorf("flux %s serialized render spec is redirected: got=%+v want=%+v", name, got, want)
	}
	return nil
}

func validateFluxSnapshotRenderBindings(sources map[string]FluxSourceProvenance) error {
	platform := sources["apps"].ProtectedIdentity
	for _, name := range []string{"bootstrap", "system"} {
		identity := sources[name].ProtectedIdentity
		if identity.Mode != platform.Mode || identity.Contract != platform.Contract ||
			identity.ContractVersion != platform.ContractVersion ||
			identity.BaselineRevision != platform.BaselineRevision ||
			identity.BaselineDigest != platform.BaselineDigest {
			return fmt.Errorf("flux %s platform review baseline differs from apps: %+v != %+v",
				name, identity, platform)
		}
	}
	for _, name := range requiredFluxProvenanceOwners {
		render := sources[name].RenderSpec
		if render.ReviewedRevision != platform.BaselineRevision ||
			render.ReviewedScopeDigest != platform.BaselineDigest {
			return fmt.Errorf("flux %s render manifest is not bound to the platform review baseline: revision=%q/%q scope=%q/%q",
				name, render.ReviewedRevision, platform.BaselineRevision,
				render.ReviewedScopeDigest, platform.BaselineDigest)
		}
	}
	return nil
}

func validateFluxProtectedIdentity(source FluxSourceProvenance) error {
	identity := source.ProtectedIdentity
	wantContract := platformGitOpsScopeV1
	if source.Name == "loom-hub-servers" {
		wantContract = loomCoreSourceScopeV1
	}
	if identity.Contract != wantContract.name || identity.ContractVersion != wantContract.version {
		return fmt.Errorf("flux %s provenance has source identity contract %q-v%d, want %q-v%d",
			source.Name, identity.Contract, identity.ContractVersion, wantContract.name, wantContract.version)
	}
	if identity.Mode != GitOpsIdentityModeExactRevision && identity.Mode != GitOpsIdentityModeProtectedScope {
		return fmt.Errorf("flux %s provenance has unsupported identity mode %q", source.Name, identity.Mode)
	}
	if identity.BaselineRevision == "" || identity.ObservedRevision == "" ||
		identity.BaselineDigest == "" || identity.ObservedDigest == "" || identity.CheckedCommitCount <= 0 {
		return fmt.Errorf("flux %s provenance has incomplete protected identity: %+v", source.Name, identity)
	}
	baselineRevision, err := normalizeGitOpsScopeRevision(identity.BaselineRevision)
	if err != nil || baselineRevision != identity.BaselineRevision {
		return fmt.Errorf("flux %s provenance has invalid normalized baseline revision %q: %v",
			source.Name, identity.BaselineRevision, err)
	}
	observedRevision, err := normalizeGitOpsScopeRevision(identity.ObservedRevision)
	if err != nil || observedRevision != identity.ObservedRevision {
		return fmt.Errorf("flux %s provenance has invalid normalized observed revision %q: %v",
			source.Name, identity.ObservedRevision, err)
	}
	appliedRevision, err := normalizeGitOpsScopeRevision(source.AppliedRevision)
	if err != nil {
		return fmt.Errorf("flux %s provenance has invalid applied revision %q: %w", source.Name, source.AppliedRevision, err)
	}
	if appliedRevision != observedRevision {
		return fmt.Errorf("flux %s applied revision %q differs from protected identity revision %q",
			source.Name, appliedRevision, observedRevision)
	}
	switch identity.Mode {
	case GitOpsIdentityModeExactRevision:
		if baselineRevision != observedRevision ||
			identity.BaselineDigest != baselineRevision ||
			identity.ObservedDigest != observedRevision {
			return fmt.Errorf("flux %s exact-revision identity drifted: %+v", source.Name, identity)
		}
	case GitOpsIdentityModeProtectedScope:
		if !isNormalizedSHA256(identity.BaselineDigest) || !isNormalizedSHA256(identity.ObservedDigest) {
			return fmt.Errorf("flux %s protected-scope digest is not a normalized SHA-256: %q/%q",
				source.Name, identity.BaselineDigest, identity.ObservedDigest)
		}
		if identity.BaselineDigest != identity.ObservedDigest {
			return fmt.Errorf("flux %s protected-scope digest drifted: %q -> %q",
				source.Name, identity.BaselineDigest, identity.ObservedDigest)
		}
	}
	return nil
}

func isNormalizedSHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func isRequiredFluxProvenanceOwner(name string) bool {
	for _, required := range requiredFluxProvenanceOwners {
		if name == required {
			return true
		}
	}
	return false
}

// ValidateFluxSourceFenceEvidence proves from serialized evidence that every
// required owner remained byte-for-byte stable across the final delete fence.
// List resourceVersions may differ because an unrelated Kustomization can
// advance between the two coherent collection reads.
func ValidateFluxSourceFenceEvidence(fence FluxSourceFenceEvidence) error {
	if err := ValidateFluxSourceProvenanceSnapshot(fence.Prepared); err != nil {
		return fmt.Errorf("prepared Flux provenance: %w", err)
	}
	if err := ValidateFluxSourceProvenanceSnapshot(fence.Final); err != nil {
		return fmt.Errorf("final Flux provenance: %w", err)
	}
	if fence.Final.GitRepositoriesOpenedAt.Before(fence.Prepared.GitRepositories.ObservedAt) {
		return fmt.Errorf("final Flux source bracket overlaps prepared snapshot: prepared_closed=%s final_opened=%s",
			fence.Prepared.GitRepositories.ObservedAt, fence.Final.GitRepositoriesOpenedAt)
	}
	prepared := fluxProvenanceByName(fence.Prepared)
	final := fluxProvenanceByName(fence.Final)
	for _, name := range requiredFluxProvenanceOwners {
		if prepared[name] != final[name] {
			return fmt.Errorf("flux %s prepared/final provenance mismatch: %+v -> %+v",
				name, prepared[name], final[name])
		}
	}
	preparedRepositories := gitRepositoryProvenanceByName(fence.Prepared.GitRepositories)
	finalRepositories := gitRepositoryProvenanceByName(fence.Final.GitRepositories)
	for _, name := range requiredGitRepositoryNames {
		if preparedRepositories[name] != finalRepositories[name] {
			return fmt.Errorf("GitRepository %s prepared/final provenance mismatch: %+v -> %+v",
				name, preparedRepositories[name], finalRepositories[name])
		}
	}
	return nil
}

// ValidatePreflightFluxProvenance validates both coherent preflight snapshots
// and their protected-scope continuity. A proven protected-scope descendant is
// allowed; an object replacement, missing owner, or protected digest drift is
// not.
func ValidatePreflightFluxProvenance(report PreflightReport) error {
	if err := ValidateFluxSourceProvenanceSnapshot(report.FluxSourcesStart); err != nil {
		return fmt.Errorf("preflight start provenance: %w", err)
	}
	if err := ValidateFluxSourceProvenanceSnapshot(report.FluxSourcesEnd); err != nil {
		return fmt.Errorf("preflight end provenance: %w", err)
	}
	if report.FluxSourcesEnd.ObservedAt.Before(report.FluxSourcesStart.ObservedAt) {
		return fmt.Errorf("preflight Flux provenance moved backwards: %s -> %s",
			report.FluxSourcesStart.ObservedAt, report.FluxSourcesEnd.ObservedAt)
	}
	if report.FluxSourcesEnd.GitRepositoriesOpenedAt.Before(report.FluxSourcesStart.GitRepositories.ObservedAt) {
		return fmt.Errorf("preflight source brackets overlap or move backwards: start_closed=%s end_opened=%s",
			report.FluxSourcesStart.GitRepositories.ObservedAt, report.FluxSourcesEnd.GitRepositoriesOpenedAt)
	}
	if err := sameGitRepositoryGateIdentity(
		report.FluxSourcesStart.GitRepositories,
		report.FluxSourcesEnd.GitRepositories,
	); err != nil {
		return fmt.Errorf("preflight GitRepository identity: %w", err)
	}
	start := fluxProvenanceByName(report.FluxSourcesStart)
	end := fluxProvenanceByName(report.FluxSourcesEnd)
	for _, name := range requiredFluxProvenanceOwners {
		if start[name].UID != end[name].UID {
			return fmt.Errorf("flux %s object UID changed during preflight: %q -> %q", name, start[name].UID, end[name].UID)
		}
		if start[name].Generation != end[name].Generation {
			return fmt.Errorf("flux %s object generation changed during preflight: %d -> %d",
				name, start[name].Generation, end[name].Generation)
		}
		if err := sameFluxSourceFence(provenanceFluxSourceState(start[name]), provenanceFluxSourceState(end[name])); err != nil {
			return err
		}
	}
	return nil
}

func fluxProvenanceByName(snapshot FluxSourceProvenanceSnapshot) map[string]FluxSourceProvenance {
	result := make(map[string]FluxSourceProvenance, len(snapshot.Sources))
	for _, source := range snapshot.Sources {
		result[source.Name] = source
	}
	return result
}

func provenanceFluxSourceState(source FluxSourceProvenance) fluxSourceState {
	return fluxSourceState{
		name:                    source.Name,
		uid:                     source.UID,
		resourceVersion:         source.ResourceVersion,
		generation:              source.Generation,
		deletionTimestamp:       source.DeletionTimestamp,
		terminating:             source.Terminating,
		readyObservedGeneration: source.ReadyObservedGeneration,
		readyStatus:             source.ReadyStatus,
		applied:                 source.AppliedRevision,
		attempted:               source.AttemptedRevision,
		ready:                   source.ReadyStatus == "True",
		renderSpec:              source.RenderSpec,
		identity:                source.ProtectedIdentity,
	}
}
