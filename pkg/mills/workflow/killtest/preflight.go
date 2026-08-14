package killtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
)

type workflowPolicyView struct {
	Enabled   *bool `yaml:"enabled"`
	Workflows struct {
		Enabled          bool `yaml:"enabled"`
		SubstrateK8sOnly bool `yaml:"substrate_k8s_only"`
	} `yaml:"workflows"`
}

type effectivePolicyView struct {
	Enabled   *bool `json:"Enabled"`
	Workflows struct {
		Enabled          bool `json:"Enabled"`
		SubstrateK8sOnly bool `json:"SubstrateK8sOnly"`
	} `json:"Workflows"`
}

type gitOpsKustomizationWire struct {
	Metadata struct {
		Name              string  `json:"name"`
		UID               string  `json:"uid"`
		ResourceVersion   string  `json:"resourceVersion"`
		Generation        int64   `json:"generation"`
		DeletionTimestamp *string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec   json.RawMessage `json:"spec"`
	Status struct {
		LastAppliedRevision   string `json:"lastAppliedRevision"`
		LastAttemptedRevision string `json:"lastAttemptedRevision"`
		Conditions            []struct {
			Type               string `json:"type"`
			Status             string `json:"status"`
			ObservedGeneration int64  `json:"observedGeneration"`
		} `json:"conditions"`
	} `json:"status"`
}

type gitOpsKustomizationListWire struct {
	Metadata struct {
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Items []gitOpsKustomizationWire `json:"items"`
}

func parseGitOpsKustomization(raw string) (applied, attempted string, ready bool, err error) {
	return parseFluxKustomization("apps", raw)
}

func parseFluxKustomization(name, raw string) (applied, attempted string, ready bool, err error) {
	var obj gitOpsKustomizationWire
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return "", "", false, err
	}
	return parseFluxKustomizationObject(name, obj)
}

func parseFluxKustomizationObject(name string, obj gitOpsKustomizationWire) (applied, attempted string, ready bool, err error) {
	if obj.Metadata.DeletionTimestamp != nil {
		return "", "", false, fmt.Errorf("flux %s is terminating: deletionTimestamp=%q",
			name, *obj.Metadata.DeletionTimestamp)
	}
	readySeen := false
	for _, condition := range obj.Status.Conditions {
		if condition.Type == "Ready" {
			if readySeen {
				return "", "", false, fmt.Errorf("flux %s contains duplicate Ready condition", name)
			}
			readySeen = true
			ready = condition.Status == "True"
		}
	}
	applied, attempted = obj.Status.LastAppliedRevision, obj.Status.LastAttemptedRevision
	if !ready || applied == "" || attempted == "" || applied != attempted {
		return applied, attempted, ready, fmt.Errorf("flux %s is not converged: ready=%t applied=%q attempted=%q", name, ready, applied, attempted)
	}
	return applied, attempted, ready, nil
}

func normalizeExpectedGitOpsRevision(raw string) (string, error) {
	revision := strings.TrimSpace(raw)
	if revision == "" {
		return "", nil
	}
	if index := strings.LastIndex(revision, ":"); index >= 0 {
		revision = revision[index+1:]
	}
	if len(revision) != 40 && len(revision) != 64 {
		return "", fmt.Errorf("expected GitOps revision must be a 40- or 64-character commit SHA, got %q", revision)
	}
	if _, err := hex.DecodeString(revision); err != nil {
		return "", fmt.Errorf("expected GitOps revision is not hexadecimal: %w", err)
	}
	return strings.ToLower(revision), nil
}

func (h *Harness) resolveGitOpsIdentity(ctx context.Context, applied string) (GitOpsScopeIdentity, error) {
	return h.resolveSourceIdentity(ctx, applied, h.cfg.ExpectedGitOpsRevision, h.cfg.GitOpsRepoPath,
		platformGitOpsScopeV1, ResolveGitOpsScopeIdentity)
}

func (h *Harness) resolveLoomCoreIdentity(ctx context.Context, applied string) (GitOpsScopeIdentity, error) {
	return h.resolveSourceIdentity(ctx, applied, h.cfg.ExpectedLoomCoreRevision, h.cfg.LoomCoreRepoPath,
		loomCoreSourceScopeV1, ResolveLoomCoreScopeIdentity)
}

func (h *Harness) resolveSourceIdentity(
	ctx context.Context,
	applied, expected, repoPath string,
	contract gitScopeContract,
	resolve func(context.Context, string, string, string, string) (GitOpsScopeIdentity, error),
) (GitOpsScopeIdentity, error) {
	mode := strings.TrimSpace(h.cfg.GitOpsIdentityMode)
	if mode == "" {
		mode = GitOpsIdentityModeExactRevision
	}
	identity := GitOpsScopeIdentity{
		Mode:            mode,
		Contract:        contract.name,
		ContractVersion: contract.version,
	}
	if mode != GitOpsIdentityModeExactRevision && mode != GitOpsIdentityModeProtectedScope {
		return identity, fmt.Errorf("unsupported %s identity mode %q", contract.subject, mode)
	}
	expectedRevision, err := normalizeExpectedGitOpsRevision(expected)
	if err != nil {
		return identity, fmt.Errorf("expected revision: %w", err)
	}
	// A non-gating diagnostic preflight may intentionally omit the reviewed
	// baseline. Preserve that mode while leaving the comparison identity empty;
	// full-gate option validation requires a baseline before destructive work.
	if expectedRevision == "" {
		return identity, nil
	}
	observedRevision, err := normalizeGitOpsScopeRevision(applied)
	if err != nil {
		return identity, fmt.Errorf("observed revision: %w", err)
	}
	if mode == GitOpsIdentityModeExactRevision {
		identity.BaselineRevision = expectedRevision
		identity.ObservedRevision = observedRevision
		identity.BaselineDigest = expectedRevision
		identity.ObservedDigest = observedRevision
		identity.CheckedCommitCount = 1
		if expectedRevision != observedRevision {
			return identity, fmt.Errorf("exact %s revision changed: %s -> %s", contract.subject, expectedRevision, observedRevision)
		}
		return identity, nil
	}
	return resolve(ctx, repoPath, expectedRevision, observedRevision, mode)
}

type fluxSourceState struct {
	name                    string
	uid                     string
	resourceVersion         string
	generation              int64
	deletionTimestamp       string
	terminating             bool
	readyObservedGeneration int64
	readyStatus             string
	applied                 string
	attempted               string
	ready                   bool
	renderSpec              FluxRenderSpecIdentity
	identity                GitOpsScopeIdentity
}

type fluxSourceSnapshot struct {
	resourceVersion         string
	gitRepositoriesOpenedAt time.Time
	observedAt              time.Time
	gitRepositories         gitRepositorySnapshot
	platform                fluxSourceState
	bootstrap               fluxSourceState
	system                  fluxSourceState
	loomCore                fluxSourceState
	platformConvergenceErr  error
	bootstrapConvergenceErr error
	systemConvergenceErr    error
	loomCoreConvergenceErr  error
}

// SourceIdentityFence is an opaque, identity-resolved snapshot of every
// crash-critical Flux render owner across both source repositories.
// FinalizeSourceIdentityFence compares it with one last coherent API snapshot
// without performing any subsequent Git or Kubernetes reads.
type SourceIdentityFence struct {
	snapshot fluxSourceSnapshot
}

var requiredFluxProvenanceOwners = []string{"apps", "bootstrap", "system", "loom-hub-servers"}

func (state fluxSourceState) provenance() FluxSourceProvenance {
	return FluxSourceProvenance{
		Name:                    state.name,
		UID:                     state.uid,
		ResourceVersion:         state.resourceVersion,
		Generation:              state.generation,
		DeletionTimestamp:       state.deletionTimestamp,
		Terminating:             state.terminating,
		ReadyObservedGeneration: state.readyObservedGeneration,
		ReadyStatus:             state.readyStatus,
		AppliedRevision:         state.applied,
		AttemptedRevision:       state.attempted,
		RenderSpec:              state.renderSpec,
		ProtectedIdentity:       state.identity,
	}
}

func (snapshot fluxSourceSnapshot) provenance() FluxSourceProvenanceSnapshot {
	return FluxSourceProvenanceSnapshot{
		Contract:                FluxProvenanceContract,
		ContractVersion:         FluxProvenanceContractVersion,
		ListResourceVersion:     snapshot.resourceVersion,
		GitRepositoriesOpenedAt: snapshot.gitRepositoriesOpenedAt,
		ObservedAt:              snapshot.observedAt,
		GitRepositories:         snapshot.gitRepositories.provenance(),
		Sources: []FluxSourceProvenance{
			snapshot.platform.provenance(),
			snapshot.bootstrap.provenance(),
			snapshot.system.provenance(),
			snapshot.loomCore.provenance(),
		},
	}
}

func parseFluxSourceSnapshot(raw string) (fluxSourceSnapshot, error) {
	var list gitOpsKustomizationListWire
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return fluxSourceSnapshot{}, err
	}
	if list.Metadata.ResourceVersion == "" {
		return fluxSourceSnapshot{}, errors.New("flux source List has no resourceVersion")
	}

	snapshot := fluxSourceSnapshot{
		resourceVersion: list.Metadata.ResourceVersion,
		platform:        fluxSourceState{name: "apps"},
		bootstrap:       fluxSourceState{name: "bootstrap"},
		system:          fluxSourceState{name: "system"},
		loomCore:        fluxSourceState{name: "loom-hub-servers"},
	}
	seen := make(map[string]bool, 4)
	for _, item := range list.Items {
		name := item.Metadata.Name
		if name != snapshot.platform.name && name != snapshot.bootstrap.name &&
			name != snapshot.system.name && name != snapshot.loomCore.name {
			continue
		}
		if seen[name] {
			return fluxSourceSnapshot{}, fmt.Errorf("flux source snapshot contains duplicate %q Kustomization", name)
		}
		seen[name] = true
		if item.Metadata.DeletionTimestamp != nil {
			return fluxSourceSnapshot{}, fmt.Errorf("flux %s snapshot contains a terminating Kustomization: deletionTimestamp=%q",
				name, *item.Metadata.DeletionTimestamp)
		}
		if item.Metadata.UID == "" || item.Metadata.ResourceVersion == "" {
			return fluxSourceSnapshot{}, fmt.Errorf("flux %s snapshot has incomplete object identity: uid=%q resourceVersion=%q",
				name, item.Metadata.UID, item.Metadata.ResourceVersion)
		}
		if item.Metadata.Generation <= 0 {
			return fluxSourceSnapshot{}, fmt.Errorf("flux %s snapshot has invalid generation %d", name, item.Metadata.Generation)
		}
		readyGeneration := int64(0)
		readyStatus := ""
		readySeen := false
		for _, condition := range item.Status.Conditions {
			if condition.Type == "Ready" {
				if readySeen {
					return fluxSourceSnapshot{}, fmt.Errorf("flux %s contains duplicate Ready condition", name)
				}
				readySeen = true
				readyGeneration = condition.ObservedGeneration
				readyStatus = condition.Status
			}
		}
		if readyGeneration != item.Metadata.Generation {
			return fluxSourceSnapshot{}, fmt.Errorf("flux %s Ready status is stale: generation=%d observedGeneration=%d",
				name, item.Metadata.Generation, readyGeneration)
		}
		applied, attempted, ready, convergenceErr := parseFluxKustomizationObject(name, item)
		renderSpec, renderErr := parseFluxRenderSpecIdentity(name, item.Spec)
		if renderErr != nil {
			return fluxSourceSnapshot{}, renderErr
		}
		state := fluxSourceState{
			name: name, uid: item.Metadata.UID, resourceVersion: item.Metadata.ResourceVersion,
			generation: item.Metadata.Generation, terminating: item.Metadata.DeletionTimestamp != nil,
			readyObservedGeneration: readyGeneration,
			readyStatus:             readyStatus, applied: applied, attempted: attempted, ready: ready,
			renderSpec: renderSpec,
		}
		switch name {
		case snapshot.platform.name:
			snapshot.platform = state
			snapshot.platformConvergenceErr = convergenceErr
		case snapshot.bootstrap.name:
			snapshot.bootstrap = state
			snapshot.bootstrapConvergenceErr = convergenceErr
		case snapshot.system.name:
			snapshot.system = state
			snapshot.systemConvergenceErr = convergenceErr
		default:
			snapshot.loomCore = state
			snapshot.loomCoreConvergenceErr = convergenceErr
		}
	}
	for _, name := range []string{
		snapshot.platform.name, snapshot.bootstrap.name, snapshot.system.name, snapshot.loomCore.name,
	} {
		if !seen[name] {
			return fluxSourceSnapshot{}, fmt.Errorf("flux source snapshot is missing %q Kustomization", name)
		}
	}
	return snapshot, nil
}

var requiredFluxRenderSpecs = map[string]FluxRenderSpecIdentity{
	"apps": {
		Path: "./k3s/flux/apps", SourceRefKind: "GitRepository",
		SourceRefName: "gitops-gitlab", SourceRefNamespace: "flux-system",
		ManifestPath: "clusters/k3s/flux-system/kustomization-apps.yaml",
	},
	"bootstrap": {
		Path: "./k3s/flux/bootstrap", SourceRefKind: "GitRepository",
		SourceRefName: "gitops-gitlab", SourceRefNamespace: "flux-system",
		ManifestPath: "clusters/k3s/flux-system/kustomization-bootstrap.yaml",
	},
	"system": {
		Path: "./k3s/flux/system", SourceRefKind: "GitRepository",
		SourceRefName: "gitops-gitlab", SourceRefNamespace: "flux-system",
		ManifestPath: "clusters/k3s/flux-system/kustomization-system.yaml",
	},
	"loom-hub-servers": {
		Path: "./k8s/base", SourceRefKind: "GitRepository", SourceRefName: "loom-core",
		SourceRefNamespace: "flux-system", TargetNamespace: "loom-hub",
		ManifestPath: "clusters/k3s/flux-system/kustomization-loom-hub-servers.yaml",
	},
}

func canonicalFluxSpecSHA256(value any) (string, error) {
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(canonical)), nil
}

func parseFluxRenderSpecIdentity(name string, raw json.RawMessage) (FluxRenderSpecIdentity, error) {
	want, ok := requiredFluxRenderSpecs[name]
	if !ok {
		return FluxRenderSpecIdentity{}, fmt.Errorf("flux %s has no render-spec contract", name)
	}
	if len(raw) == 0 || string(raw) == "null" {
		return FluxRenderSpecIdentity{}, fmt.Errorf("flux %s Kustomization has no spec", name)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var canonicalValue any
	if err := decoder.Decode(&canonicalValue); err != nil {
		return FluxRenderSpecIdentity{}, fmt.Errorf("decode Flux %s spec: %w", name, err)
	}
	digest, err := canonicalFluxSpecSHA256(canonicalValue)
	if err != nil {
		return FluxRenderSpecIdentity{}, fmt.Errorf("canonicalize Flux %s spec: %w", name, err)
	}
	var selected struct {
		Path            string `json:"path"`
		TargetNamespace string `json:"targetNamespace"`
		SourceRef       struct {
			Kind      string `json:"kind"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"sourceRef"`
	}
	if err := json.Unmarshal(raw, &selected); err != nil {
		return FluxRenderSpecIdentity{}, fmt.Errorf("decode Flux %s render identity: %w", name, err)
	}
	sourceNamespace := selected.SourceRef.Namespace
	if sourceNamespace == "" {
		sourceNamespace = "flux-system"
	}
	got := FluxRenderSpecIdentity{
		Path: selected.Path, SourceRefKind: selected.SourceRef.Kind,
		SourceRefName: selected.SourceRef.Name, SourceRefNamespace: sourceNamespace,
		TargetNamespace: selected.TargetNamespace, ManifestPath: want.ManifestPath,
		SpecSHA256: digest,
	}
	want.SpecSHA256 = got.SpecSHA256
	if got != want {
		return FluxRenderSpecIdentity{}, fmt.Errorf("flux %s live render spec is redirected: got=%+v want=%+v", name, got, want)
	}
	return got, nil
}

func reviewedFluxRenderSpecDigests(
	ctx context.Context,
	repoPath, revision string,
) (map[string]string, error) {
	if strings.TrimSpace(repoPath) == "" {
		return nil, errors.New("GitOps repository path is required to reconstruct reviewed Flux specs")
	}
	normalizedRevision, err := normalizeGitOpsScopeRevision(revision)
	if err != nil {
		return nil, fmt.Errorf("normalize reviewed Flux spec revision: %w", err)
	}
	if err := ensureGitScopeCommits(ctx, repoPath, "GitOps render manifest", normalizedRevision); err != nil {
		return nil, err
	}

	digests := make(map[string]string, len(requiredFluxProvenanceOwners))
	for _, name := range requiredFluxProvenanceOwners {
		contract := requiredFluxRenderSpecs[name]
		raw, err := runGitScope(ctx, repoPath, "show", normalizedRevision+":"+contract.ManifestPath)
		if err != nil {
			return nil, fmt.Errorf("read reviewed Flux %s manifest %s at %s: %w",
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
			return nil, fmt.Errorf("decode reviewed Flux %s manifest: %w", name, err)
		}
		if yamlDocumentIsEmpty(&document) {
			return nil, fmt.Errorf("reviewed Flux %s manifest is an empty YAML document", name)
		}
		var trailing yaml.Node
		if err := decoder.Decode(&trailing); err == nil {
			return nil, fmt.Errorf("reviewed Flux %s manifest contains multiple YAML documents", name)
		} else if !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("decode trailing YAML document in reviewed Flux %s manifest: %w", name, err)
		}
		if err := document.Decode(&manifest); err != nil {
			return nil, fmt.Errorf("decode reviewed Flux %s manifest: %w", name, err)
		}
		if manifest.APIVersion != "kustomize.toolkit.fluxcd.io/v1" ||
			manifest.Kind != "Kustomization" || manifest.Metadata.Name != name ||
			manifest.Metadata.Namespace != "flux-system" || manifest.Spec == nil {
			return nil, fmt.Errorf("reviewed Flux manifest %s has unexpected identity: apiVersion=%q kind=%q name=%q namespace=%q",
				contract.ManifestPath, manifest.APIVersion, manifest.Kind,
				manifest.Metadata.Name, manifest.Metadata.Namespace)
		}
		// Flux's CRD defaults spec.force to false. Reconstruct that one API
		// default before hashing so an omitted false in reviewed YAML matches
		// the canonical live object without ignoring any user-controlled field.
		if _, ok := manifest.Spec["force"]; !ok {
			manifest.Spec["force"] = false
		}
		digest, err := canonicalFluxSpecSHA256(manifest.Spec)
		if err != nil {
			return nil, fmt.Errorf("canonicalize reviewed Flux %s spec: %w", name, err)
		}
		digests[name] = digest
	}
	return digests, nil
}

func yamlDocumentIsEmpty(document *yaml.Node) bool {
	if document == nil || len(document.Content) == 0 {
		return true
	}
	root := document.Content[0]
	return root.Kind == 0 ||
		(root.Kind == yaml.ScalarNode && root.Tag == "!!null" && strings.TrimSpace(root.Value) == "")
}

func bindReviewedFluxRenderSpecs(
	snapshot fluxSourceSnapshot,
	revision, scopeDigest string,
	digests map[string]string,
) (fluxSourceSnapshot, error) {
	states := []*fluxSourceState{&snapshot.platform, &snapshot.bootstrap, &snapshot.system, &snapshot.loomCore}
	for _, state := range states {
		reviewedDigest := digests[state.name]
		if !isNormalizedSHA256(reviewedDigest) {
			return snapshot, fmt.Errorf("reviewed Flux %s spec has invalid SHA-256 %q", state.name, reviewedDigest)
		}
		if state.renderSpec.SpecSHA256 != reviewedDigest {
			return snapshot, fmt.Errorf("flux %s live spec does not match reviewed Git manifest %s at %s: %s != %s",
				state.name, state.renderSpec.ManifestPath, revision,
				state.renderSpec.SpecSHA256, reviewedDigest)
		}
		state.renderSpec.ReviewedSpecSHA256 = reviewedDigest
		state.renderSpec.ReviewedRevision = revision
		state.renderSpec.ReviewedScopeDigest = scopeDigest
	}
	return snapshot, nil
}

func (h *Harness) readKustomizationSnapshot(ctx context.Context) (fluxSourceSnapshot, error) {
	// Read the raw collection endpoint as one unpaginated Kubernetes List.
	// Fetching the resource names independently would issue multiple GETs and could
	// construct a mixed source snapshot. The raw endpoint also preserves the
	// List resourceVersion that kubectl's object printer clears.
	raw, err := h.kubectl(ctx, "get", "--raw",
		"/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/flux-system/kustomizations?limit=0")
	if err != nil {
		return fluxSourceSnapshot{}, fmt.Errorf("read coherent Flux source snapshot: %w", err)
	}
	snapshot, err := parseFluxSourceSnapshot(raw)
	if err != nil {
		return fluxSourceSnapshot{}, fmt.Errorf("decode coherent Flux source snapshot: %w", err)
	}
	// Timestamp after the complete List response has been received and parsed.
	// This conservative completion boundary is serialized into the evidence.
	snapshot.observedAt = time.Now().UTC()
	return snapshot, nil
}

func (h *Harness) readFluxSourceSnapshot(ctx context.Context) (fluxSourceSnapshot, error) {
	firstRepositories, err := h.readGitRepositorySnapshot(ctx)
	if err != nil {
		return fluxSourceSnapshot{}, fmt.Errorf("open GitRepository source bracket: %w", err)
	}
	snapshot, err := h.readKustomizationSnapshot(ctx)
	if err != nil {
		return fluxSourceSnapshot{}, err
	}
	finalRepositories, err := h.readGitRepositorySnapshot(ctx)
	if err != nil {
		return fluxSourceSnapshot{}, fmt.Errorf("close GitRepository source bracket: %w", err)
	}
	if err := sameGitRepositoryBracket(firstRepositories, finalRepositories); err != nil {
		return fluxSourceSnapshot{}, err
	}
	if firstRepositories.observedAt.After(snapshot.observedAt) ||
		snapshot.observedAt.After(finalRepositories.observedAt) {
		return fluxSourceSnapshot{}, fmt.Errorf("GitRepository/Kustomization source bracket timestamps are out of order: %s -> %s -> %s",
			firstRepositories.observedAt, snapshot.observedAt, finalRepositories.observedAt)
	}
	// Persist B: it is the source state proven stable around the Kustomization
	// List and the last completed read in the observation bracket.
	snapshot.gitRepositoriesOpenedAt = firstRepositories.observedAt
	snapshot.gitRepositories = finalRepositories
	return snapshot, nil
}

func (h *Harness) resolveFluxSourceSnapshot(ctx context.Context, snapshot fluxSourceSnapshot) (fluxSourceSnapshot, error) {
	var err error
	snapshot.gitRepositories.platform.ProtectedIdentity, err = h.resolveGitOpsIdentity(
		ctx, snapshot.gitRepositories.platform.ArtifactRevision)
	if err != nil {
		return snapshot, fmt.Errorf("resolve gitops-gitlab artifact identity: %w", err)
	}
	snapshot.gitRepositories.loomCore.ProtectedIdentity, err = h.resolveLoomCoreIdentity(
		ctx, snapshot.gitRepositories.loomCore.ArtifactRevision)
	if err != nil {
		return snapshot, fmt.Errorf("resolve loom-core artifact identity: %w", err)
	}
	platformArtifact := snapshot.gitRepositories.platform.ArtifactRevision
	for _, state := range []*fluxSourceState{&snapshot.platform, &snapshot.bootstrap, &snapshot.system} {
		if state.applied != platformArtifact {
			return snapshot, fmt.Errorf("flux %s applied revision %q differs from gitops-gitlab artifact %q",
				state.name, state.applied, platformArtifact)
		}
		state.identity = snapshot.gitRepositories.platform.ProtectedIdentity
	}
	if snapshot.loomCore.applied != snapshot.gitRepositories.loomCore.ArtifactRevision {
		return snapshot, fmt.Errorf("flux loom-hub-servers applied revision %q differs from loom-core artifact %q",
			snapshot.loomCore.applied, snapshot.gitRepositories.loomCore.ArtifactRevision)
	}
	snapshot.loomCore.identity = snapshot.gitRepositories.loomCore.ProtectedIdentity
	// Diagnostic preflight intentionally permits an omitted reviewed baseline.
	// A gate baseline, however, must reconstruct all four defining manifests
	// from platform Git. loom-hub-servers consumes loom-core content but its
	// Kustomization object is still defined by platform/gitops.
	if snapshot.platform.identity.BaselineRevision == "" {
		return snapshot, nil
	}
	repositoryLoad := h.reviewedGitRepositorySpecsFn
	if repositoryLoad == nil {
		repositoryLoad = reviewedGitRepositorySpecDigests
	}
	repositoryDigests, err := repositoryLoad(ctx, h.cfg.GitOpsRepoPath,
		snapshot.platform.identity.BaselineRevision)
	if err != nil {
		return snapshot, fmt.Errorf("reconstruct reviewed GitRepository specs: %w", err)
	}
	snapshot.gitRepositories, err = bindReviewedGitRepositorySpecs(snapshot.gitRepositories,
		snapshot.platform.identity.BaselineRevision, snapshot.platform.identity.BaselineDigest,
		repositoryDigests)
	if err != nil {
		return snapshot, err
	}
	load := h.reviewedFluxRenderSpecsFn
	if load == nil {
		load = reviewedFluxRenderSpecDigests
	}
	digests, err := load(ctx, h.cfg.GitOpsRepoPath, snapshot.platform.identity.BaselineRevision)
	if err != nil {
		return snapshot, fmt.Errorf("reconstruct reviewed Flux render specs: %w", err)
	}
	return bindReviewedFluxRenderSpecs(snapshot, snapshot.platform.identity.BaselineRevision,
		snapshot.platform.identity.BaselineDigest, digests)
}

func (h *Harness) observeFluxSourceSnapshot(ctx context.Context) (fluxSourceSnapshot, error) {
	snapshot, err := h.readFluxSourceSnapshot(ctx)
	if err != nil {
		return snapshot, err
	}
	return h.resolveFluxSourceSnapshot(ctx, snapshot)
}

func sameFluxSourceFence(start, end fluxSourceState) error {
	if start.name != end.name || start.name == "" {
		return fmt.Errorf("source name changed from %q to %q", start.name, end.name)
	}
	if !start.ready || start.applied == "" || start.applied != start.attempted {
		return fmt.Errorf("flux %s started unconverged: ready=%t applied=%q attempted=%q",
			start.name, start.ready, start.applied, start.attempted)
	}
	if !end.ready || end.applied == "" || end.applied != end.attempted {
		return fmt.Errorf("flux %s ended unconverged: ready=%t applied=%q attempted=%q",
			end.name, end.ready, end.applied, end.attempted)
	}
	if start.renderSpec != end.renderSpec {
		return fmt.Errorf("flux %s render spec changed during source fence: %+v -> %+v",
			start.name, start.renderSpec, end.renderSpec)
	}
	if start.uid == "" || end.uid == "" || start.uid != end.uid ||
		start.generation <= 0 || start.generation != end.generation {
		return fmt.Errorf("flux %s object identity changed during source fence: uid=%q/%q generation=%d/%d",
			start.name, start.uid, end.uid, start.generation, end.generation)
	}
	// Diagnostic preflight intentionally allows an omitted reviewed baseline.
	// It still rejects a source revision change while the workload tuple is
	// collected, because there is no protected-scope proof to authorize it.
	if start.identity.BaselineDigest == "" || end.identity.BaselineDigest == "" {
		if start.applied != end.applied {
			return fmt.Errorf("flux %s revision changed during preflight without a reviewed baseline: %q -> %q",
				start.name, start.applied, end.applied)
		}
		return nil
	}
	if start.identity.Mode != end.identity.Mode ||
		start.identity.Contract != end.identity.Contract ||
		start.identity.ContractVersion != end.identity.ContractVersion ||
		start.identity.BaselineRevision != end.identity.BaselineRevision ||
		start.identity.BaselineDigest != end.identity.BaselineDigest ||
		start.identity.ObservedDigest != end.identity.ObservedDigest {
		return fmt.Errorf("flux %s protected identity changed: %+v -> %+v", start.name, start.identity, end.identity)
	}
	return nil
}

func sourceFenceReportSnapshot(want PreflightReport) fluxSourceSnapshot {
	sources := fluxProvenanceByName(want.FluxSourcesEnd)
	return fluxSourceSnapshot{
		gitRepositoriesOpenedAt: want.FluxSourcesEnd.GitRepositoriesOpenedAt,
		gitRepositories:         provenanceGitRepositorySnapshot(want.FluxSourcesEnd.GitRepositories),
		platform:                provenanceFluxSourceState(sources["apps"]),
		bootstrap:               provenanceFluxSourceState(sources["bootstrap"]),
		system:                  provenanceFluxSourceState(sources["system"]),
		loomCore:                provenanceFluxSourceState(sources["loom-hub-servers"]),
	}
}

func validateFluxSourceConvergence(snapshot fluxSourceSnapshot) error {
	if snapshot.platformConvergenceErr != nil {
		return snapshot.platformConvergenceErr
	}
	if snapshot.bootstrapConvergenceErr != nil {
		return snapshot.bootstrapConvergenceErr
	}
	if snapshot.systemConvergenceErr != nil {
		return snapshot.systemConvergenceErr
	}
	return snapshot.loomCoreConvergenceErr
}

// PrepareSourceIdentityFence takes one coherent snapshot of every crash-critical
// Flux render owner, resolves both repositories' protected identities, and binds
// them to the reviewed preflight. It performs all potentially slow Git work up
// front.
func (h *Harness) PrepareSourceIdentityFence(ctx context.Context, want PreflightReport) (SourceIdentityFence, error) {
	snapshot, err := h.observeFluxSourceSnapshot(ctx)
	if err != nil {
		return SourceIdentityFence{}, err
	}
	if err := validateFluxSourceConvergence(snapshot); err != nil {
		return SourceIdentityFence{}, err
	}
	wantSnapshot := sourceFenceReportSnapshot(want)
	if err := sameGitRepositoryGateIdentity(
		wantSnapshot.gitRepositories.provenance(), snapshot.gitRepositories.provenance(),
	); err != nil {
		return SourceIdentityFence{}, err
	}
	if err := sameFluxSourceFence(wantSnapshot.platform, snapshot.platform); err != nil {
		return SourceIdentityFence{}, err
	}
	if err := sameFluxSourceFence(wantSnapshot.bootstrap, snapshot.bootstrap); err != nil {
		return SourceIdentityFence{}, err
	}
	if err := sameFluxSourceFence(wantSnapshot.system, snapshot.system); err != nil {
		return SourceIdentityFence{}, err
	}
	if err := sameFluxSourceFence(wantSnapshot.loomCore, snapshot.loomCore); err != nil {
		return SourceIdentityFence{}, err
	}
	prepared := snapshot.provenance()
	if prepared.GitRepositoriesOpenedAt.Before(want.FluxSourcesEnd.GitRepositories.ObservedAt) {
		return SourceIdentityFence{}, fmt.Errorf("prepared Flux source bracket %s predates immediate preflight end %s",
			prepared.GitRepositoriesOpenedAt, want.FluxSourcesEnd.GitRepositories.ObservedAt)
	}
	if err := validateFluxSnapshotBinding(want.FluxSourcesEnd, prepared); err != nil {
		return SourceIdentityFence{}, fmt.Errorf("bind immediate preflight to prepared Flux source snapshot: %w", err)
	}
	return SourceIdentityFence{snapshot: snapshot}, nil
}

func sameFinalFluxSourceSnapshot(prepared, final fluxSourceState) error {
	if prepared.name != final.name || prepared.name == "" {
		return fmt.Errorf("source name changed from %q to %q", prepared.name, final.name)
	}
	if !final.ready || final.applied == "" || final.applied != final.attempted {
		return fmt.Errorf("flux %s final snapshot is not converged: ready=%t applied=%q attempted=%q",
			final.name, final.ready, final.applied, final.attempted)
	}
	if prepared.ready != final.ready || prepared.applied != final.applied || prepared.attempted != final.attempted {
		return fmt.Errorf("flux %s changed during final source fence: ready=%t/%t applied=%q/%q attempted=%q/%q",
			prepared.name, prepared.ready, final.ready, prepared.applied, final.applied,
			prepared.attempted, final.attempted)
	}
	if prepared.uid != final.uid || prepared.resourceVersion != final.resourceVersion {
		return fmt.Errorf("flux %s object identity changed during final source fence: uid=%q/%q resourceVersion=%q/%q",
			prepared.name, prepared.uid, final.uid, prepared.resourceVersion, final.resourceVersion)
	}
	preparedLiveSpec := prepared.renderSpec
	preparedLiveSpec.ReviewedSpecSHA256 = ""
	preparedLiveSpec.ReviewedRevision = ""
	preparedLiveSpec.ReviewedScopeDigest = ""
	finalLiveSpec := final.renderSpec
	finalLiveSpec.ReviewedSpecSHA256 = ""
	finalLiveSpec.ReviewedRevision = ""
	finalLiveSpec.ReviewedScopeDigest = ""
	if prepared.generation != final.generation || preparedLiveSpec != finalLiveSpec {
		return fmt.Errorf("flux %s spec identity changed during final source fence: generation=%d/%d spec=%+v/%+v",
			prepared.name, prepared.generation, final.generation, preparedLiveSpec, finalLiveSpec)
	}
	return nil
}

// FinalizeSourceIdentityFence performs exactly one source observation bracket
// (GitRepository A, Kustomization List, GitRepository B) and only in-memory
// comparisons. Call it as the final external operation before the
// UID-preconditioned pod deletion. Its return value is the durable proof used
// to re-derive the gate verdict without cluster access.
func (h *Harness) FinalizeSourceIdentityFence(ctx context.Context, fence SourceIdentityFence) (FluxSourceFenceEvidence, error) {
	final, err := h.readFluxSourceSnapshot(ctx)
	if err != nil {
		return FluxSourceFenceEvidence{}, err
	}
	if err := validateFluxSourceConvergence(final); err != nil {
		return FluxSourceFenceEvidence{}, err
	}
	if err := sameFinalGitRepositorySnapshot(fence.snapshot.gitRepositories, final.gitRepositories); err != nil {
		return FluxSourceFenceEvidence{}, err
	}
	if err := sameFinalFluxSourceSnapshot(fence.snapshot.platform, final.platform); err != nil {
		return FluxSourceFenceEvidence{}, err
	}
	if err := sameFinalFluxSourceSnapshot(fence.snapshot.bootstrap, final.bootstrap); err != nil {
		return FluxSourceFenceEvidence{}, err
	}
	if err := sameFinalFluxSourceSnapshot(fence.snapshot.system, final.system); err != nil {
		return FluxSourceFenceEvidence{}, err
	}
	if err := sameFinalFluxSourceSnapshot(fence.snapshot.loomCore, final.loomCore); err != nil {
		return FluxSourceFenceEvidence{}, err
	}
	// The final path intentionally performs no Git I/O. Exact object identity
	// and revision equality allow it to inherit the prepared protected-scope
	// proof after every comparison succeeds.
	final.platform.identity = fence.snapshot.platform.identity
	final.bootstrap.identity = fence.snapshot.bootstrap.identity
	final.system.identity = fence.snapshot.system.identity
	final.loomCore.identity = fence.snapshot.loomCore.identity
	inheritPreparedGitRepositoryReview(fence.snapshot.gitRepositories, &final.gitRepositories)
	final.platform.renderSpec = fence.snapshot.platform.renderSpec
	final.bootstrap.renderSpec = fence.snapshot.bootstrap.renderSpec
	final.system.renderSpec = fence.snapshot.system.renderSpec
	final.loomCore.renderSpec = fence.snapshot.loomCore.renderSpec
	evidence := FluxSourceFenceEvidence{
		Prepared: fence.snapshot.provenance(),
		Final:    final.provenance(),
	}
	if err := ValidateFluxSourceFenceEvidence(evidence); err != nil {
		return FluxSourceFenceEvidence{}, fmt.Errorf("validate serialized source fence: %w", err)
	}
	return evidence, nil
}

// RecheckSourceIdentities is the convenience form used by callers that have no
// work to place between identity resolution and the final coherent snapshot.
func (h *Harness) RecheckSourceIdentities(ctx context.Context, want PreflightReport) error {
	fence, err := h.PrepareSourceIdentityFence(ctx, want)
	if err != nil {
		return err
	}
	_, err = h.FinalizeSourceIdentityFence(ctx, fence)
	return err
}

func parseWorkflowPolicy(raw string) (globalEnabled, workflowsEnabled, substrateK8sOnly bool, err error) {
	var policy workflowPolicyView
	if err := yaml.Unmarshal([]byte(raw), &policy); err != nil {
		return false, false, false, err
	}
	globalEnabled = true
	if policy.Enabled != nil {
		globalEnabled = *policy.Enabled
	}
	return globalEnabled, policy.Workflows.Enabled, policy.Workflows.SubstrateK8sOnly, nil
}

func (h *Harness) effectivePolicy(ctx context.Context) (
	globalEnabled, workflowsEnabled, substrateK8sOnly bool,
	authority OperatorResponseAuthority,
	err error,
) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		h.freshOperatorURL("/api/mills/policy"), nil)
	if err != nil {
		return false, false, false, authority, err
	}
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	resp, authority, err := h.doOperatorRequest(req)
	if err != nil {
		return false, false, false, authority, err
	}
	defer resp.Body.Close()
	body, err := readSafetyEndpointBody("policy GET", resp.Body)
	if err != nil {
		return false, false, false, authority, err
	}
	if resp.StatusCode != http.StatusOK {
		return false, false, false, authority, fmt.Errorf("policy GET: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var policy effectivePolicyView
	if err := json.Unmarshal(body, &policy); err != nil {
		return false, false, false, authority, err
	}
	globalEnabled = true
	if policy.Enabled != nil {
		globalEnabled = *policy.Enabled
	}
	return globalEnabled, policy.Workflows.Enabled, policy.Workflows.SubstrateK8sOnly, authority, nil
}

func (h *Harness) quiescence(ctx context.Context) (QuiescenceSnapshot, error) {
	var snapshot QuiescenceSnapshot
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		h.freshOperatorURL("/api/mills/safety/quiescence"), nil)
	if err != nil {
		return snapshot, err
	}
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	resp, authority, err := h.doOperatorRequest(req)
	if err != nil {
		return snapshot, err
	}
	defer resp.Body.Close()
	body, err := readSafetyEndpointBody("quiescence GET", resp.Body)
	if err != nil {
		return snapshot, err
	}
	if resp.StatusCode != http.StatusOK {
		return snapshot, fmt.Errorf("quiescence GET: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Cache-Control")), "no-store") {
		return snapshot, errors.New("quiescence response is not marked Cache-Control: no-store")
	}
	if err := json.Unmarshal(body, &snapshot); err != nil {
		return snapshot, err
	}
	snapshot.OperatorAuthority = authority
	if snapshot.ObservedAt.IsZero() {
		return snapshot, errors.New("quiescence response has no observed_at")
	}
	age := time.Since(snapshot.ObservedAt)
	if age < -5*time.Second || age > 30*time.Second {
		return snapshot, fmt.Errorf("quiescence response is stale or clock-skewed: observed_at=%s age=%s", snapshot.ObservedAt, age)
	}
	return snapshot, nil
}

// Preflight asserts the §3.2 preconditions without requiring kubectl exec.
// Reading policy from the live ConfigMap keeps the gate usable when one node's
// kubelet proxy is unavailable, while image IDs/checksum preserve exact identity.
func (h *Harness) Preflight(ctx context.Context, allowedRunID ...string) (PreflightReport, error) {
	var rep PreflightReport

	namespaceRaw, err := h.kubectl(ctx, "get", "ns", h.cfg.OperatorNS, h.cfg.HudNS, h.cfg.SpawnNS, h.cfg.LokiNS, "-o", "json")
	if err != nil {
		return rep, fmt.Errorf("P1 cluster/namespaces: %w", err)
	}
	if err := validateActiveNamespaces(namespaceRaw, h.cfg.OperatorNS, h.cfg.HudNS, h.cfg.SpawnNS, h.cfg.LokiNS); err != nil {
		return rep, fmt.Errorf("P1 cluster/namespaces: %w", err)
	}
	operatorNamespace, err := namespaceIdentityFromRaw(namespaceRaw, h.cfg.OperatorNS)
	if err != nil {
		return rep, fmt.Errorf("P1 operator Namespace authority: %w", err)
	}
	if err := h.bindClusterAuthority(operatorNamespace); err != nil {
		return rep, fmt.Errorf("P1 cluster authority: %w", err)
	}
	rep.NamespacesOK = true
	sourceStart, err := h.observeFluxSourceSnapshot(ctx)
	if err != nil {
		return rep, fmt.Errorf("flux sources: %w", err)
	}
	platformStart := sourceStart.platform
	rep.GitOpsStartRevision = platformStart.applied
	rep.GitOpsStartIdentity = platformStart.identity
	rep.GitOpsRevision = platformStart.applied
	rep.GitOpsAttempted = platformStart.attempted
	rep.GitOpsReady = platformStart.ready
	rep.GitOpsIdentity = platformStart.identity

	bootstrapStart := sourceStart.bootstrap
	rep.GitOpsBootstrapStartRevision = bootstrapStart.applied
	rep.GitOpsBootstrapStartIdentity = bootstrapStart.identity
	rep.GitOpsBootstrapRevision = bootstrapStart.applied
	rep.GitOpsBootstrapAttempted = bootstrapStart.attempted
	rep.GitOpsBootstrapReady = bootstrapStart.ready
	rep.GitOpsBootstrapIdentity = bootstrapStart.identity

	systemStart := sourceStart.system
	rep.GitOpsSystemStartRevision = systemStart.applied
	rep.GitOpsSystemStartIdentity = systemStart.identity
	rep.GitOpsSystemRevision = systemStart.applied
	rep.GitOpsSystemAttempted = systemStart.attempted
	rep.GitOpsSystemReady = systemStart.ready
	rep.GitOpsSystemIdentity = systemStart.identity

	loomCoreStart := sourceStart.loomCore
	rep.LoomCoreStartRevision = loomCoreStart.applied
	rep.LoomCoreStartIdentity = loomCoreStart.identity
	rep.LoomCoreRevision = loomCoreStart.applied
	rep.LoomCoreAttempted = loomCoreStart.attempted
	rep.LoomCoreReady = loomCoreStart.ready
	rep.LoomCoreIdentity = loomCoreStart.identity
	if sourceStart.platformConvergenceErr != nil {
		return rep, fmt.Errorf("GitOps convergence: %w", sourceStart.platformConvergenceErr)
	}
	if sourceStart.bootstrapConvergenceErr != nil {
		return rep, fmt.Errorf("GitOps bootstrap convergence: %w", sourceStart.bootstrapConvergenceErr)
	}
	if sourceStart.systemConvergenceErr != nil {
		return rep, fmt.Errorf("GitOps system convergence: %w", sourceStart.systemConvergenceErr)
	}
	if sourceStart.loomCoreConvergenceErr != nil {
		return rep, fmt.Errorf("loom-core convergence: %w", sourceStart.loomCoreConvergenceErr)
	}
	rep.FluxSourcesStart = sourceStart.provenance()
	var reviewedDeployments map[string]reviewedDeployment
	if h.cfg.ExpectedGitOpsRevision != "" && h.cfg.ExpectedLoomCoreRevision != "" {
		reviewedDeployments, err = h.reviewedDeployments(ctx, sourceStart)
		if err != nil {
			return rep, fmt.Errorf("reconstruct reviewed Deployment desired state: %w", err)
		}
	}

	var operatorObject *appsv1.Deployment
	rep.OperatorDeployment, operatorObject, err = h.stableDeploymentObject(ctx, h.cfg.OperatorNS, "loom-mills-operator")
	if err != nil {
		return rep, fmt.Errorf("P2 operator deployment: %w", err)
	}
	if reviewedDeployments != nil {
		rep.OperatorDeployment, err = h.bindReviewedDeployment(ctx, rep.OperatorDeployment, operatorObject,
			reviewedDeployments[operatorDeploymentKey])
		if err != nil {
			return rep, fmt.Errorf("P2 operator reviewed Deployment provenance: %w", err)
		}
	}
	rep.OperatorImage = rep.OperatorDeployment.Image
	rep.PolicyChecksum = rep.OperatorDeployment.PolicyChecksum
	if !isNormalizedSHA256(rep.PolicyChecksum) {
		return rep, fmt.Errorf("P2 operator deployment policy checksum %q is not a normalized SHA-256", rep.PolicyChecksum)
	}

	policyObjectRaw, err := h.kubectl(ctx, "-n", h.cfg.OperatorNS, "get", "configmap", policyConfigMapName, "-o", "json")
	if err != nil {
		return rep, fmt.Errorf("P3 policy ConfigMap probe: %w", err)
	}
	var policyRaw string
	var livePolicyPayloadSHA string
	rep.PolicyConfigMapIdentity, policyRaw, livePolicyPayloadSHA, err = parsePolicyConfigMapPayload(
		policyObjectRaw, h.cfg.OperatorNS,
	)
	if err != nil {
		return rep, fmt.Errorf("P3 policy ConfigMap identity: %w", err)
	}
	if reviewedDeployments != nil {
		rep.PolicyConfigMapReview, err = bindReviewedPolicyConfigMap(
			rep.PolicyConfigMapIdentity, livePolicyPayloadSHA, rep.OperatorDeployment.PolicyChecksum,
			reviewedDeployments[operatorDeploymentKey].PolicyConfigMap,
		)
		if err != nil {
			return rep, fmt.Errorf("P3 reviewed policy ConfigMap provenance: %w", err)
		}
	}
	rep.ConfigMapPolicyEnabled, rep.FlagEnabled, rep.SubstrateK8sOnly, err = parseWorkflowPolicy(policyRaw)
	if err != nil {
		return rep, fmt.Errorf("P3 decode workflow policy: %w", err)
	}
	rep.WorkflowsFlag = fmt.Sprintf("global_enabled: %t\nworkflows_enabled: %t\nsubstrate_k8s_only: %t",
		rep.ConfigMapPolicyEnabled, rep.FlagEnabled, rep.SubstrateK8sOnly)
	rep.EffectivePolicyEnabled, rep.EffectiveFlagEnabled, rep.EffectiveSubstrateK8sOnly,
		rep.EffectivePolicyAuthority, err = h.effectivePolicy(ctx)
	if err != nil {
		return rep, fmt.Errorf("P3 effective operator policy: %w", err)
	}
	rep.EffectivePolicyMatchesConfigMap = rep.ConfigMapPolicyEnabled == rep.EffectivePolicyEnabled &&
		rep.FlagEnabled == rep.EffectiveFlagEnabled &&
		rep.SubstrateK8sOnly == rep.EffectiveSubstrateK8sOnly
	if !rep.EffectivePolicyMatchesConfigMap {
		return rep, fmt.Errorf("P3 ConfigMap/effective policy mismatch: configmap global=%t workflows=%t k8s_only=%t, effective global=%t workflows=%t k8s_only=%t",
			rep.ConfigMapPolicyEnabled, rep.FlagEnabled, rep.SubstrateK8sOnly,
			rep.EffectivePolicyEnabled, rep.EffectiveFlagEnabled, rep.EffectiveSubstrateK8sOnly)
	}

	rep.Operator, err = h.readyPod(ctx, h.cfg.OperatorNS, h.cfg.OperatorSelector, rep.OperatorDeployment)
	if err != nil {
		return rep, fmt.Errorf("operator pod: %w", err)
	}
	if rep.Operator.Image != rep.OperatorImage {
		return rep, fmt.Errorf("operator deployment image %q differs from Ready pod image %q", rep.OperatorImage, rep.Operator.Image)
	}
	rep.AuthorityPlane, err = h.bindOperatorAuthority(rep.Operator, rep.OperatorDeployment)
	if err != nil {
		return rep, fmt.Errorf("operator REST/Kubernetes authority binding: %w", err)
	}

	var hudObject *appsv1.Deployment
	rep.HudDeployment, hudObject, err = h.stableDeploymentObject(ctx, h.cfg.HudNS, "mobile-hud")
	if err != nil {
		return rep, fmt.Errorf("mobile-hud deployment: %w", err)
	}
	if reviewedDeployments != nil {
		rep.HudDeployment, err = h.bindReviewedDeployment(ctx, rep.HudDeployment, hudObject,
			reviewedDeployments[hudDeploymentKey])
		if err != nil {
			return rep, fmt.Errorf("mobile-hud reviewed Deployment provenance: %w", err)
		}
	}
	rep.HudImage = rep.HudDeployment.Image
	rep.Hud, err = h.readyPod(ctx, h.cfg.HudNS, h.cfg.HudSelector, rep.HudDeployment)
	if err != nil {
		return rep, fmt.Errorf("mobile-hud pod: %w", err)
	}
	if rep.Hud.Image != rep.HudImage {
		return rep, fmt.Errorf("mobile-hud deployment image %q differs from Ready pod image %q", rep.HudImage, rep.Hud.Image)
	}

	spawns, err := h.getSpawnStateSnapshot(ctx, "")
	if err != nil {
		return rep, fmt.Errorf("P6 durable spawn state: %w", err)
	}
	rep.SpawnConfigMap = true
	rep.SpawnConfigMapUID = spawns.ConfigMapUID
	rep.SpawnConfigMapIdentity = spawns.ConfigMapIdentity
	updateAllowed, err := h.kubectl(ctx, "auth", "can-i", "update", "configmap/loom-spawn-state",
		"-n", h.cfg.SpawnNS, "--as=system:serviceaccount:"+h.cfg.HudNS+":loom-hub")
	if err != nil {
		return rep, fmt.Errorf("P6 durable spawn state update authorization: %w", err)
	}
	rep.SpawnConfigMapUpdateAllowed = strings.TrimSpace(updateAllowed) == "yes"
	rep.SpawnRecordIDs = spawns.RecordIDs
	rep.ActiveSpawnIDs = spawns.ActiveIDs
	allowedID := ""
	expectedWorkflowRuns := 0
	if len(allowedRunID) > 0 && allowedRunID[0] != "" {
		detail, getErr := h.GetRun(ctx, allowedRunID[0])
		if getErr != nil {
			return rep, fmt.Errorf("allowed workflow run: %w", getErr)
		}
		if detail.Run.State != "running" {
			return rep, fmt.Errorf("allowed workflow run %s state %q, want running", allowedRunID[0], detail.Run.State)
		}
		step, ok := FindAgentStep(detail)
		if !ok {
			return rep, fmt.Errorf("allowed workflow run %s has no agent step", allowedRunID[0])
		}
		identity, deriveErr := DeriveSpawnIdentity(allowedRunID[0], step)
		if deriveErr != nil {
			return rep, fmt.Errorf("allowed run spawn identity: %w", deriveErr)
		}
		// The journal identity is immutable. A spawn request's branch is mutable
		// lifecycle metadata and can temporarily disappear while the HUD rewrites
		// its durable record, so it must not participate in crash authorization.
		allowedID = identity.SpawnID
		expectedWorkflowRuns = 1
	}
	if allowedID != "" {
		rep.ActiveSpawnIDs = unrelatedActiveSpawnIDs(rep.ActiveSpawnIDs, allowedID)
	}
	rep.ActiveSpawnPodNames, err = h.activeSpawnPodNames(ctx)
	if err != nil {
		return rep, fmt.Errorf("active spawn pods: %w", err)
	}
	if allowedID != "" {
		var blockers []string
		for _, name := range rep.ActiveSpawnPodNames {
			if name != "spawn-"+allowedID {
				blockers = append(blockers, name)
			}
		}
		rep.ActiveSpawnPodNames = blockers
	}
	rep.Quiescence, err = h.quiescence(ctx)
	if err != nil {
		return rep, fmt.Errorf("fleet quiescence: %w", err)
	}

	lokiPath := fmt.Sprintf("/api/v1/namespaces/%s/services/%s/proxy/ready", h.cfg.LokiNS, h.cfg.LokiService)
	loki, err := h.kubectl(ctx, "get", "--raw", lokiPath)
	if err != nil {
		return rep, fmt.Errorf("loki evidence path unavailable: %w", err)
	}
	rep.LokiReady = strings.TrimSpace(loki) == "ready"

	// Fence all independently advancing Flux render owners after every workload and
	// deployment identity has been collected. This prevents a mixed preflight
	// snapshot from authorizing a destructive singleton restart.
	sourceEnd, err := h.observeFluxSourceSnapshot(ctx)
	if err != nil {
		return rep, fmt.Errorf("ending coherent Flux source fence: %w", err)
	}
	if sourceEnd.platformConvergenceErr != nil {
		return rep, fmt.Errorf("ending GitOps convergence: %w", sourceEnd.platformConvergenceErr)
	}
	if sourceEnd.bootstrapConvergenceErr != nil {
		return rep, fmt.Errorf("ending GitOps bootstrap convergence: %w", sourceEnd.bootstrapConvergenceErr)
	}
	if sourceEnd.systemConvergenceErr != nil {
		return rep, fmt.Errorf("ending GitOps system convergence: %w", sourceEnd.systemConvergenceErr)
	}
	if sourceEnd.loomCoreConvergenceErr != nil {
		return rep, fmt.Errorf("ending loom-core convergence: %w", sourceEnd.loomCoreConvergenceErr)
	}
	platformEnd := sourceEnd.platform
	bootstrapEnd := sourceEnd.bootstrap
	systemEnd := sourceEnd.system
	loomCoreEnd := sourceEnd.loomCore
	if err := sameFluxSourceFence(platformStart, platformEnd); err != nil {
		return rep, fmt.Errorf("GitOps source fence: %w", err)
	}
	if err := sameFluxSourceFence(bootstrapStart, bootstrapEnd); err != nil {
		return rep, fmt.Errorf("GitOps bootstrap source fence: %w", err)
	}
	if err := sameFluxSourceFence(systemStart, systemEnd); err != nil {
		return rep, fmt.Errorf("GitOps system source fence: %w", err)
	}
	if err := sameFluxSourceFence(loomCoreStart, loomCoreEnd); err != nil {
		return rep, fmt.Errorf("loom-core source fence: %w", err)
	}
	rep.GitOpsRevision = platformEnd.applied
	rep.GitOpsAttempted = platformEnd.attempted
	rep.GitOpsReady = platformEnd.ready
	rep.GitOpsIdentity = platformEnd.identity
	rep.GitOpsBootstrapRevision = bootstrapEnd.applied
	rep.GitOpsBootstrapAttempted = bootstrapEnd.attempted
	rep.GitOpsBootstrapReady = bootstrapEnd.ready
	rep.GitOpsBootstrapIdentity = bootstrapEnd.identity
	rep.GitOpsSystemRevision = systemEnd.applied
	rep.GitOpsSystemAttempted = systemEnd.attempted
	rep.GitOpsSystemReady = systemEnd.ready
	rep.GitOpsSystemIdentity = systemEnd.identity
	rep.LoomCoreRevision = loomCoreEnd.applied
	rep.LoomCoreAttempted = loomCoreEnd.attempted
	rep.LoomCoreReady = loomCoreEnd.ready
	rep.LoomCoreIdentity = loomCoreEnd.identity
	rep.FluxSourcesEnd = sourceEnd.provenance()
	// Diagnostic preflight intentionally permits omitted reviewed baselines.
	// A full gate always supplies both baselines and therefore validates the
	// independently re-playable serialized proof before reporting success.
	if h.cfg.ExpectedGitOpsRevision != "" && h.cfg.ExpectedLoomCoreRevision != "" {
		if err := ValidatePreflightFluxProvenance(rep); err != nil {
			return rep, fmt.Errorf("serialized preflight Flux provenance: %w", err)
		}
		if err := ValidateDeploymentProvenance(rep); err != nil {
			return rep, fmt.Errorf("serialized preflight Deployment provenance: %w", err)
		}
		if err := ValidatePolicyConfigMapProvenance(rep); err != nil {
			return rep, fmt.Errorf("serialized preflight policy ConfigMap provenance: %w", err)
		}
	}

	memoryIdle := rep.Quiescence.InMemory.idle()
	if expectedWorkflowRuns == 1 {
		memoryIdle = rep.Quiescence.InMemory.idleForWorkflow(allowedRunID[0])
	}
	quiescenceMatches := rep.Quiescence.Counts.unrelatedIdle(expectedWorkflowRuns) && memoryIdle
	if expectedWorkflowRuns == 0 {
		quiescenceMatches = quiescenceMatches && rep.Quiescence.Quiescent
	}
	configMapIdentitiesOK := ValidatePreflightConfigMapIdentities(rep) == nil
	rep.AllPreconditions = rep.NamespacesOK && rep.OperatorImage != "" &&
		rep.Operator.ImageID != "" && rep.Operator.UID != "" && rep.HudImage != "" &&
		rep.Hud.ImageID != "" && rep.Hud.UID != "" &&
		rep.GitOpsReady && rep.GitOpsRevision != "" && rep.GitOpsRevision == rep.GitOpsAttempted &&
		rep.GitOpsBootstrapReady && rep.GitOpsBootstrapRevision != "" &&
		rep.GitOpsBootstrapRevision == rep.GitOpsBootstrapAttempted &&
		rep.GitOpsSystemReady && rep.GitOpsSystemRevision != "" &&
		rep.GitOpsSystemRevision == rep.GitOpsSystemAttempted &&
		rep.LoomCoreReady && rep.LoomCoreRevision != "" && rep.LoomCoreRevision == rep.LoomCoreAttempted &&
		rep.PolicyChecksum != "" && !rep.ConfigMapPolicyEnabled && !rep.EffectivePolicyEnabled &&
		rep.FlagEnabled && rep.SubstrateK8sOnly && rep.EffectivePolicyMatchesConfigMap &&
		rep.SpawnConfigMap && configMapIdentitiesOK && rep.SpawnConfigMapUpdateAllowed &&
		len(rep.ActiveSpawnIDs) == 0 && len(rep.ActiveSpawnPodNames) == 0 &&
		quiescenceMatches && rep.LokiReady
	return rep, nil
}

func unrelatedActiveSpawnIDs(activeIDs []string, allowedID string) []string {
	blockers := make([]string, 0, len(activeIDs))
	for _, id := range activeIDs {
		if id != allowedID {
			blockers = append(blockers, id)
		}
	}
	return blockers
}

// AssertSafeToCrash closes the preflight-to-delete race. Immediately before
// each singleton pod deletion, only the target workflow/spawn may be active.
func (h *Harness) AssertSafeToCrash(ctx context.Context, runID, spawnID string) (CrashTargetSafetyEvidence, error) {
	var evidence CrashTargetSafetyEvidence
	snapshot, err := h.quiescence(ctx)
	if err != nil {
		return evidence, err
	}
	evidence.Quiescence = snapshot
	evidence.QuiescenceCollectedAt = time.Now().UTC()
	if !snapshot.Counts.unrelatedIdle(1) || !snapshot.InMemory.idleForWorkflowCrashLease(runID) {
		return evidence, fmt.Errorf("fleet activity changed: durable=%+v in_memory=%+v", snapshot.Counts, snapshot.InMemory)
	}
	detail, err := h.GetRun(ctx, runID)
	if err != nil {
		return evidence, err
	}
	evidence.Run = detail.Run
	evidence.RunAuthority = detail.OperatorAuthority
	if detail.Run.State != "running" {
		return evidence, fmt.Errorf("target workflow %s state %q, want running", runID, detail.Run.State)
	}
	step, ok := FindAgentStep(detail)
	if !ok {
		return evidence, fmt.Errorf("target workflow %s has no agent step", runID)
	}
	evidence.AgentStep = step
	identity, err := DeriveSpawnIdentity(runID, step)
	if err != nil {
		return evidence, err
	}
	evidence.DerivedSpawn = identity
	if identity.SpawnID != spawnID {
		return evidence, fmt.Errorf("target workflow derived spawn %s, want %s", identity.SpawnID, spawnID)
	}
	spawns, err := h.getSpawnStateSnapshot(ctx, "")
	if err != nil {
		return evidence, err
	}
	evidence.SpawnState = spawns
	if len(spawns.ActiveIDs) != 1 || spawns.ActiveIDs[0] != spawnID {
		return evidence, fmt.Errorf("active durable spawns %v, want only %s", spawns.ActiveIDs, spawnID)
	}
	if status := spawns.Statuses[spawnID]; status != "running" {
		return evidence, fmt.Errorf("durable spawn %s status %q, want running immediately before agent execution crash", spawnID, status)
	}
	if key := spawns.IdempotencyKeys[spawnID]; key != identity.IdempotencyKey {
		return evidence, fmt.Errorf("durable spawn %s idempotency key %q differs from derived key %q",
			spawnID, key, identity.IdempotencyKey)
	}
	pods, err := h.activeSpawnPodNames(ctx)
	if err != nil {
		return evidence, err
	}
	evidence.ActiveSpawnPodNames = append([]string(nil), pods...)
	wantPod := "spawn-" + spawnID
	if len(pods) != 1 || pods[0] != wantPod {
		return evidence, fmt.Errorf("active spawn pods %v, want only %s", pods, wantPod)
	}
	active, ready, exactNames, err := h.SpawnPodStatus(ctx, spawnID)
	if err != nil {
		return evidence, err
	}
	evidence.ExactSpawnPodActive = active
	evidence.ExactSpawnPodReady = ready
	evidence.ExactSpawnPodNames = append([]string(nil), exactNames...)
	evidence.ObservedAt = time.Now().UTC()
	if active != 1 || ready != 1 || len(exactNames) != 1 || exactNames[0] != wantPod {
		return evidence, fmt.Errorf("exact spawn pod is not one non-terminating Running+Ready workload: active=%d ready=%d names=%v",
			active, ready, exactNames)
	}
	return evidence, nil
}
