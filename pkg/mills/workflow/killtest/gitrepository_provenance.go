package killtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var requiredGitRepositoryNames = []string{"gitops-gitlab", "loom-core"}

var requiredGitRepositorySpecs = map[string]GitRepositorySpecIdentity{
	"gitops-gitlab": {
		URL:       "http://gitlab-vm.gitlab.svc.cluster.local/platform/gitops.git",
		RefBranch: "main", SecretRefName: "gitops-gitlab",
		ManifestPath: "clusters/k3s/flux-system/gitrepository-gitlab.yaml",
	},
	"loom-core": {
		URL:       "http://gitlab-vm.gitlab.svc.cluster.local/services/loom-core.git",
		RefBranch: "main", SecretRefName: "gitops-gitlab",
		ManifestPath: "clusters/k3s/flux-system/gitrepository-loom-core.yaml",
	},
}

type gitRepositoryWire struct {
	Metadata struct {
		Name              string  `json:"name"`
		Namespace         string  `json:"namespace"`
		UID               string  `json:"uid"`
		ResourceVersion   string  `json:"resourceVersion"`
		Generation        int64   `json:"generation"`
		DeletionTimestamp *string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec   json.RawMessage `json:"spec"`
	Status struct {
		ObservedGeneration int64 `json:"observedGeneration"`
		Artifact           struct {
			Revision string `json:"revision"`
			Digest   string `json:"digest"`
		} `json:"artifact"`
		Conditions []struct {
			Type               string `json:"type"`
			Status             string `json:"status"`
			ObservedGeneration int64  `json:"observedGeneration"`
		} `json:"conditions"`
	} `json:"status"`
}

type gitRepositoryListWire struct {
	Metadata struct {
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Items []gitRepositoryWire `json:"items"`
}

type gitRepositorySnapshot struct {
	resourceVersion string
	observedAt      time.Time
	platform        GitRepositoryProvenance
	loomCore        GitRepositoryProvenance
}

func (snapshot gitRepositorySnapshot) provenance() GitRepositoryProvenanceSnapshot {
	return GitRepositoryProvenanceSnapshot{
		ListResourceVersion: snapshot.resourceVersion,
		ObservedAt:          snapshot.observedAt,
		Repositories:        []GitRepositoryProvenance{snapshot.platform, snapshot.loomCore},
	}
}

func parseGitRepositorySnapshot(raw string) (gitRepositorySnapshot, error) {
	var list gitRepositoryListWire
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return gitRepositorySnapshot{}, err
	}
	if strings.TrimSpace(list.Metadata.ResourceVersion) == "" {
		return gitRepositorySnapshot{}, errors.New("GitRepository List has no resourceVersion")
	}

	snapshot := gitRepositorySnapshot{resourceVersion: list.Metadata.ResourceVersion}
	seen := make(map[string]bool, len(requiredGitRepositoryNames))
	for _, item := range list.Items {
		if _, required := requiredGitRepositorySpecs[item.Metadata.Name]; !required {
			continue
		}
		if seen[item.Metadata.Name] {
			return gitRepositorySnapshot{}, fmt.Errorf("GitRepository snapshot contains duplicate %q source", item.Metadata.Name)
		}
		seen[item.Metadata.Name] = true

		spec, err := parseGitRepositorySpecIdentity(item.Metadata.Name, item.Spec)
		if err != nil {
			return gitRepositorySnapshot{}, err
		}
		repository := GitRepositoryProvenance{
			Name: item.Metadata.Name, Namespace: item.Metadata.Namespace,
			UID: item.Metadata.UID, ResourceVersion: item.Metadata.ResourceVersion,
			Generation:               item.Metadata.Generation,
			Terminating:              item.Metadata.DeletionTimestamp != nil,
			StatusObservedGeneration: item.Status.ObservedGeneration,
			ArtifactRevision:         item.Status.Artifact.Revision,
			ArtifactDigest:           item.Status.Artifact.Digest,
			Spec:                     spec,
		}
		if item.Metadata.DeletionTimestamp != nil {
			repository.DeletionTimestamp = *item.Metadata.DeletionTimestamp
		}
		conditionSeen := make(map[string]bool, 2)
		for _, condition := range item.Status.Conditions {
			switch condition.Type {
			case "Ready":
				if conditionSeen[condition.Type] {
					return gitRepositorySnapshot{}, fmt.Errorf("GitRepository %s contains duplicate Ready condition", item.Metadata.Name)
				}
				conditionSeen[condition.Type] = true
				repository.ReadyStatus = condition.Status
				repository.ReadyObservedGeneration = condition.ObservedGeneration
			case "ArtifactInStorage":
				if conditionSeen[condition.Type] {
					return gitRepositorySnapshot{}, fmt.Errorf("GitRepository %s contains duplicate ArtifactInStorage condition", item.Metadata.Name)
				}
				conditionSeen[condition.Type] = true
				repository.ArtifactInStorageStatus = condition.Status
				repository.ArtifactInStorageObservedGeneration = condition.ObservedGeneration
			}
		}
		if err := validateLiveGitRepositoryProvenance(repository); err != nil {
			return gitRepositorySnapshot{}, err
		}
		switch repository.Name {
		case "gitops-gitlab":
			snapshot.platform = repository
		case "loom-core":
			snapshot.loomCore = repository
		}
	}
	for _, name := range requiredGitRepositoryNames {
		if !seen[name] {
			return gitRepositorySnapshot{}, fmt.Errorf("GitRepository snapshot is missing %q source", name)
		}
	}
	return snapshot, nil
}

func parseGitRepositorySpecIdentity(name string, raw json.RawMessage) (GitRepositorySpecIdentity, error) {
	want, ok := requiredGitRepositorySpecs[name]
	if !ok {
		return GitRepositorySpecIdentity{}, fmt.Errorf("GitRepository %s has no source-spec contract", name)
	}
	if len(raw) == 0 || string(raw) == "null" {
		return GitRepositorySpecIdentity{}, fmt.Errorf("GitRepository %s has no spec", name)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var canonicalValue any
	if err := decoder.Decode(&canonicalValue); err != nil {
		return GitRepositorySpecIdentity{}, fmt.Errorf("decode GitRepository %s spec: %w", name, err)
	}
	digest, err := canonicalFluxSpecSHA256(canonicalValue)
	if err != nil {
		return GitRepositorySpecIdentity{}, fmt.Errorf("canonicalize GitRepository %s spec: %w", name, err)
	}
	var selected struct {
		URL string `json:"url"`
		Ref struct {
			Branch string `json:"branch"`
		} `json:"ref"`
		SecretRef struct {
			Name string `json:"name"`
		} `json:"secretRef"`
	}
	if err := json.Unmarshal(raw, &selected); err != nil {
		return GitRepositorySpecIdentity{}, fmt.Errorf("decode GitRepository %s source identity: %w", name, err)
	}
	got := GitRepositorySpecIdentity{
		URL: selected.URL, RefBranch: selected.Ref.Branch,
		SecretRefName: selected.SecretRef.Name, ManifestPath: want.ManifestPath,
		SpecSHA256: digest,
	}
	want.SpecSHA256 = digest
	if got != want {
		return GitRepositorySpecIdentity{}, fmt.Errorf("GitRepository %s live source spec is redirected: got=%+v want=%+v", name, got, want)
	}
	return got, nil
}

func validateLiveGitRepositoryProvenance(repository GitRepositoryProvenance) error {
	if _, ok := requiredGitRepositorySpecs[repository.Name]; !ok {
		return fmt.Errorf("GitRepository provenance contains unexpected %q source", repository.Name)
	}
	if repository.Namespace != "flux-system" {
		return fmt.Errorf("GitRepository %s namespace is %q, want flux-system", repository.Name, repository.Namespace)
	}
	if strings.TrimSpace(repository.UID) == "" || strings.TrimSpace(repository.ResourceVersion) == "" {
		return fmt.Errorf("GitRepository %s has incomplete object identity: uid=%q resourceVersion=%q",
			repository.Name, repository.UID, repository.ResourceVersion)
	}
	if repository.Terminating || strings.TrimSpace(repository.DeletionTimestamp) != "" {
		return fmt.Errorf("GitRepository %s is terminating: terminating=%t deletionTimestamp=%q",
			repository.Name, repository.Terminating, repository.DeletionTimestamp)
	}
	if repository.Generation <= 0 || repository.StatusObservedGeneration != repository.Generation {
		return fmt.Errorf("GitRepository %s has stale status generation: generation=%d observedGeneration=%d",
			repository.Name, repository.Generation, repository.StatusObservedGeneration)
	}
	if repository.ReadyStatus != "True" || repository.ReadyObservedGeneration != repository.Generation {
		return fmt.Errorf("GitRepository %s has stale or false Ready condition: generation=%d observedGeneration=%d status=%q",
			repository.Name, repository.Generation, repository.ReadyObservedGeneration, repository.ReadyStatus)
	}
	if repository.ArtifactInStorageStatus != "True" ||
		repository.ArtifactInStorageObservedGeneration != repository.Generation {
		return fmt.Errorf("GitRepository %s has stale or false ArtifactInStorage condition: generation=%d observedGeneration=%d status=%q",
			repository.Name, repository.Generation,
			repository.ArtifactInStorageObservedGeneration, repository.ArtifactInStorageStatus)
	}
	normalizedRevision, err := normalizeGitOpsScopeRevision(repository.ArtifactRevision)
	if err != nil {
		return fmt.Errorf("GitRepository %s has invalid artifact revision %q: %w",
			repository.Name, repository.ArtifactRevision, err)
	}
	if normalizedRevision == "" {
		return fmt.Errorf("GitRepository %s has no artifact revision", repository.Name)
	}
	if !isNormalizedArtifactDigest(repository.ArtifactDigest) {
		return fmt.Errorf("GitRepository %s has invalid artifact digest %q", repository.Name, repository.ArtifactDigest)
	}
	return validateLiveGitRepositorySpecIdentity(repository.Name, repository.Spec)
}

func validateLiveGitRepositorySpecIdentity(name string, got GitRepositorySpecIdentity) error {
	want, ok := requiredGitRepositorySpecs[name]
	if !ok {
		return fmt.Errorf("GitRepository %s has no source-spec contract", name)
	}
	if !isNormalizedSHA256(got.SpecSHA256) {
		return fmt.Errorf("GitRepository %s spec has invalid SHA-256 %q", name, got.SpecSHA256)
	}
	want.SpecSHA256 = got.SpecSHA256
	if got.URL != want.URL || got.RefBranch != want.RefBranch ||
		got.SecretRefName != want.SecretRefName || got.ManifestPath != want.ManifestPath {
		return fmt.Errorf("GitRepository %s serialized source spec is redirected: got=%+v want=%+v", name, got, want)
	}
	return nil
}

func validateGitRepositorySpecIdentity(name string, got GitRepositorySpecIdentity) error {
	if err := validateLiveGitRepositorySpecIdentity(name, got); err != nil {
		return err
	}
	if !isNormalizedSHA256(got.ReviewedSpecSHA256) || got.SpecSHA256 != got.ReviewedSpecSHA256 {
		return fmt.Errorf("GitRepository %s live/reviewed spec SHA-256 mismatch: %q/%q",
			name, got.SpecSHA256, got.ReviewedSpecSHA256)
	}
	reviewedRevision, err := normalizeGitOpsScopeRevision(got.ReviewedRevision)
	if err != nil || reviewedRevision != got.ReviewedRevision {
		return fmt.Errorf("GitRepository %s spec has invalid reviewed revision %q: %v",
			name, got.ReviewedRevision, err)
	}
	if strings.TrimSpace(got.ReviewedScopeDigest) == "" {
		return fmt.Errorf("GitRepository %s spec has no reviewed scope digest", name)
	}
	return nil
}

func isNormalizedArtifactDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && isNormalizedSHA256(strings.TrimPrefix(value, "sha256:"))
}

func reviewedGitRepositorySpecDigests(
	ctx context.Context,
	repoPath, revision string,
) (map[string]string, error) {
	if strings.TrimSpace(repoPath) == "" {
		return nil, errors.New("GitOps repository path is required to reconstruct reviewed GitRepository specs")
	}
	normalizedRevision, err := normalizeGitOpsScopeRevision(revision)
	if err != nil {
		return nil, fmt.Errorf("normalize reviewed GitRepository spec revision: %w", err)
	}
	if err := ensureGitScopeCommits(ctx, repoPath, "GitRepository manifest", normalizedRevision); err != nil {
		return nil, err
	}

	digests := make(map[string]string, len(requiredGitRepositoryNames))
	for _, name := range requiredGitRepositoryNames {
		contract := requiredGitRepositorySpecs[name]
		raw, err := runGitScope(ctx, repoPath, "show", normalizedRevision+":"+contract.ManifestPath)
		if err != nil {
			return nil, fmt.Errorf("read reviewed GitRepository %s manifest %s at %s: %w",
				name, contract.ManifestPath, normalizedRevision, err)
		}
		var manifest struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
			Metadata   struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
			Spec map[string]any `yaml:"spec"`
		}
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		var document yaml.Node
		if err := decoder.Decode(&document); err != nil {
			return nil, fmt.Errorf("decode reviewed GitRepository %s manifest: %w", name, err)
		}
		if yamlDocumentIsEmpty(&document) {
			return nil, fmt.Errorf("reviewed GitRepository %s manifest is an empty YAML document", name)
		}
		var trailing yaml.Node
		if err := decoder.Decode(&trailing); err == nil {
			return nil, fmt.Errorf("reviewed GitRepository %s manifest contains multiple YAML documents", name)
		} else if !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("decode trailing YAML document in reviewed GitRepository %s manifest: %w", name, err)
		}
		if err := document.Decode(&manifest); err != nil {
			return nil, fmt.Errorf("decode reviewed GitRepository %s manifest: %w", name, err)
		}
		if manifest.APIVersion != "source.toolkit.fluxcd.io/v1" || manifest.Kind != "GitRepository" ||
			manifest.Metadata.Name != name || manifest.Metadata.Namespace != "flux-system" || manifest.Spec == nil {
			return nil, fmt.Errorf("reviewed GitRepository manifest %s has unexpected identity: apiVersion=%q kind=%q name=%q namespace=%q",
				contract.ManifestPath, manifest.APIVersion, manifest.Kind,
				manifest.Metadata.Name, manifest.Metadata.Namespace)
		}
		digest, err := canonicalFluxSpecSHA256(manifest.Spec)
		if err != nil {
			return nil, fmt.Errorf("canonicalize reviewed GitRepository %s spec: %w", name, err)
		}
		digests[name] = digest
	}
	return digests, nil
}

func bindReviewedGitRepositorySpecs(
	snapshot gitRepositorySnapshot,
	revision, scopeDigest string,
	digests map[string]string,
) (gitRepositorySnapshot, error) {
	repositories := []*GitRepositoryProvenance{&snapshot.platform, &snapshot.loomCore}
	for _, repository := range repositories {
		reviewedDigest := digests[repository.Name]
		if !isNormalizedSHA256(reviewedDigest) {
			return snapshot, fmt.Errorf("reviewed GitRepository %s spec has invalid SHA-256 %q", repository.Name, reviewedDigest)
		}
		if repository.Spec.SpecSHA256 != reviewedDigest {
			return snapshot, fmt.Errorf("GitRepository %s live spec does not match reviewed Git manifest %s at %s: %s != %s",
				repository.Name, repository.Spec.ManifestPath, revision,
				repository.Spec.SpecSHA256, reviewedDigest)
		}
		repository.Spec.ReviewedSpecSHA256 = reviewedDigest
		repository.Spec.ReviewedRevision = revision
		repository.Spec.ReviewedScopeDigest = scopeDigest
	}
	return snapshot, nil
}

func (h *Harness) readGitRepositorySnapshot(ctx context.Context) (gitRepositorySnapshot, error) {
	raw, err := h.kubectl(ctx, "get", "--raw",
		"/apis/source.toolkit.fluxcd.io/v1/namespaces/flux-system/gitrepositories?limit=0")
	if err != nil {
		return gitRepositorySnapshot{}, fmt.Errorf("read coherent GitRepository snapshot: %w", err)
	}
	snapshot, err := parseGitRepositorySnapshot(raw)
	if err != nil {
		return gitRepositorySnapshot{}, fmt.Errorf("decode coherent GitRepository snapshot: %w", err)
	}
	snapshot.observedAt = time.Now().UTC()
	return snapshot, nil
}

func sameGitRepositoryBracket(first, second gitRepositorySnapshot) error {
	for _, pair := range []struct {
		first, second GitRepositoryProvenance
	}{{first.platform, second.platform}, {first.loomCore, second.loomCore}} {
		if pair.first != pair.second {
			return fmt.Errorf("GitRepository %s changed across source observation bracket: %+v -> %+v",
				pair.first.Name, pair.first, pair.second)
		}
	}
	return nil
}

func sameFinalGitRepositorySnapshot(prepared, final gitRepositorySnapshot) error {
	for _, pair := range []struct {
		prepared, final GitRepositoryProvenance
	}{{prepared.platform, final.platform}, {prepared.loomCore, final.loomCore}} {
		left, right := pair.prepared, pair.final
		left.ProtectedIdentity = GitOpsScopeIdentity{}
		right.ProtectedIdentity = GitOpsScopeIdentity{}
		left.Spec.ReviewedSpecSHA256, right.Spec.ReviewedSpecSHA256 = "", ""
		left.Spec.ReviewedRevision, right.Spec.ReviewedRevision = "", ""
		left.Spec.ReviewedScopeDigest, right.Spec.ReviewedScopeDigest = "", ""
		if left != right {
			return fmt.Errorf("GitRepository %s changed during final source fence: %+v -> %+v",
				pair.prepared.Name, left, right)
		}
	}
	return nil
}

func inheritPreparedGitRepositoryReview(prepared gitRepositorySnapshot, final *gitRepositorySnapshot) {
	final.platform.ProtectedIdentity = prepared.platform.ProtectedIdentity
	final.loomCore.ProtectedIdentity = prepared.loomCore.ProtectedIdentity
	final.platform.Spec = prepared.platform.Spec
	final.loomCore.Spec = prepared.loomCore.Spec
}

func validateGitRepositoryProvenanceSnapshot(snapshot GitRepositoryProvenanceSnapshot) error {
	if strings.TrimSpace(snapshot.ListResourceVersion) == "" {
		return errors.New("GitRepository provenance List has no resourceVersion")
	}
	if snapshot.ObservedAt.IsZero() {
		return errors.New("GitRepository provenance List has no observation timestamp")
	}
	if len(snapshot.Repositories) != len(requiredGitRepositoryNames) {
		return fmt.Errorf("GitRepository provenance has %d sources, want %d",
			len(snapshot.Repositories), len(requiredGitRepositoryNames))
	}
	seen := make(map[string]bool, len(snapshot.Repositories))
	repositories := make(map[string]GitRepositoryProvenance, len(snapshot.Repositories))
	for _, repository := range snapshot.Repositories {
		if seen[repository.Name] {
			return fmt.Errorf("GitRepository provenance contains duplicate %q source", repository.Name)
		}
		seen[repository.Name] = true
		repositories[repository.Name] = repository
		if err := validateLiveGitRepositoryProvenance(repository); err != nil {
			return err
		}
		if err := validateGitRepositorySpecIdentity(repository.Name, repository.Spec); err != nil {
			return err
		}
		if err := validateGitRepositoryProtectedIdentity(repository); err != nil {
			return err
		}
	}
	for _, name := range requiredGitRepositoryNames {
		if !seen[name] {
			return fmt.Errorf("GitRepository provenance is missing %q source", name)
		}
	}
	platform := repositories["gitops-gitlab"].ProtectedIdentity
	for _, name := range requiredGitRepositoryNames {
		spec := repositories[name].Spec
		if spec.ReviewedRevision != platform.BaselineRevision ||
			spec.ReviewedScopeDigest != platform.BaselineDigest {
			return fmt.Errorf("GitRepository %s manifest is not bound to the platform review baseline", name)
		}
	}
	return nil
}

func validateGitRepositoryProtectedIdentity(repository GitRepositoryProvenance) error {
	wantContract := platformGitOpsScopeV1
	if repository.Name == "loom-core" {
		wantContract = loomCoreSourceScopeV1
	}
	identity := repository.ProtectedIdentity
	if identity.Contract != wantContract.name || identity.ContractVersion != wantContract.version {
		return fmt.Errorf("GitRepository %s has source identity contract %q-v%d, want %q-v%d",
			repository.Name, identity.Contract, identity.ContractVersion, wantContract.name, wantContract.version)
	}
	if identity.Mode != GitOpsIdentityModeExactRevision && identity.Mode != GitOpsIdentityModeProtectedScope {
		return fmt.Errorf("GitRepository %s has unsupported identity mode %q", repository.Name, identity.Mode)
	}
	if identity.BaselineRevision == "" || identity.ObservedRevision == "" ||
		identity.BaselineDigest == "" || identity.ObservedDigest == "" || identity.CheckedCommitCount <= 0 {
		return fmt.Errorf("GitRepository %s has incomplete protected identity: %+v", repository.Name, identity)
	}
	artifactRevision, err := normalizeGitOpsScopeRevision(repository.ArtifactRevision)
	if err != nil || artifactRevision != identity.ObservedRevision {
		return fmt.Errorf("GitRepository %s artifact revision %q differs from protected identity revision %q: %v",
			repository.Name, repository.ArtifactRevision, identity.ObservedRevision, err)
	}
	// Reuse the Kustomization validator for exact/protected digest semantics.
	ownerName := "apps"
	if repository.Name == "loom-core" {
		ownerName = "loom-hub-servers"
	}
	return validateFluxProtectedIdentity(FluxSourceProvenance{
		Name: ownerName, AppliedRevision: repository.ArtifactRevision,
		ProtectedIdentity: identity,
	})
}

func gitRepositoryProvenanceByName(snapshot GitRepositoryProvenanceSnapshot) map[string]GitRepositoryProvenance {
	result := make(map[string]GitRepositoryProvenance, len(snapshot.Repositories))
	for _, repository := range snapshot.Repositories {
		result[repository.Name] = repository
	}
	return result
}

func provenanceGitRepositorySnapshot(snapshot GitRepositoryProvenanceSnapshot) gitRepositorySnapshot {
	repositories := gitRepositoryProvenanceByName(snapshot)
	return gitRepositorySnapshot{
		resourceVersion: snapshot.ListResourceVersion,
		observedAt:      snapshot.ObservedAt,
		platform:        repositories["gitops-gitlab"],
		loomCore:        repositories["loom-core"],
	}
}

func validateGitRepositoryConsumerBindings(
	repositories GitRepositoryProvenanceSnapshot,
	sources map[string]FluxSourceProvenance,
) error {
	byName := gitRepositoryProvenanceByName(repositories)
	for _, owner := range []string{"apps", "bootstrap", "system"} {
		if sources[owner].AppliedRevision != byName["gitops-gitlab"].ArtifactRevision {
			return fmt.Errorf("flux %s applied revision %q differs from gitops-gitlab artifact %q",
				owner, sources[owner].AppliedRevision, byName["gitops-gitlab"].ArtifactRevision)
		}
	}
	if sources["loom-hub-servers"].AppliedRevision != byName["loom-core"].ArtifactRevision {
		return fmt.Errorf("flux loom-hub-servers applied revision %q differs from loom-core artifact %q",
			sources["loom-hub-servers"].AppliedRevision, byName["loom-core"].ArtifactRevision)
	}
	return nil
}

func sameGitRepositoryGateIdentity(initial, final GitRepositoryProvenanceSnapshot) error {
	left, right := gitRepositoryProvenanceByName(initial), gitRepositoryProvenanceByName(final)
	for _, name := range requiredGitRepositoryNames {
		before, after := left[name], right[name]
		if before.Name != after.Name || before.Namespace != after.Namespace ||
			before.UID != after.UID || before.Generation != after.Generation ||
			before.DeletionTimestamp != after.DeletionTimestamp || before.Terminating != after.Terminating ||
			before.Spec != after.Spec {
			return fmt.Errorf("GitRepository %s stable gate object/spec identity changed: %+v -> %+v", name, before, after)
		}
		if err := validateSameProtectedIdentity(before.ProtectedIdentity, after.ProtectedIdentity); err != nil {
			return fmt.Errorf("GitRepository %s protected identity: %w", name, err)
		}
		beforeRevision, beforeRevisionErr := normalizeGitOpsScopeRevision(before.ArtifactRevision)
		afterRevision, afterRevisionErr := normalizeGitOpsScopeRevision(after.ArtifactRevision)
		if beforeRevisionErr != nil || afterRevisionErr != nil {
			return fmt.Errorf("GitRepository %s artifact revision normalization failed: before=%v after=%v",
				name, beforeRevisionErr, afterRevisionErr)
		}
		if beforeRevision == afterRevision && before.ArtifactDigest != after.ArtifactDigest {
			return fmt.Errorf("GitRepository %s artifact digest changed without a revision change: revision=%q digest=%q/%q",
				name, beforeRevision, before.ArtifactDigest, after.ArtifactDigest)
		}
		// Protected-scope gates may accept a descendant commit. The artifact
		// archive digest and observed revision naturally change in that case;
		// each snapshot independently binds them to its Kustomization consumers
		// and protected digest. Exact-revision mode has no such allowance.
		if before.ProtectedIdentity.Mode == GitOpsIdentityModeExactRevision &&
			(before.ArtifactRevision != after.ArtifactRevision || before.ArtifactDigest != after.ArtifactDigest) {
			return fmt.Errorf("GitRepository %s exact-revision artifact changed: revision=%q/%q digest=%q/%q",
				name, before.ArtifactRevision, after.ArtifactRevision,
				before.ArtifactDigest, after.ArtifactDigest)
		}
	}
	return nil
}
