package killtest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func bindTestPreflightFluxEnd(t *testing.T, report *PreflightReport, raw string) {
	t.Helper()
	snapshot, err := parseFluxSourceSnapshot(raw)
	if err != nil {
		t.Fatalf("parse test Flux snapshot: %v", err)
	}
	snapshot.observedAt = time.Now().UTC().Add(-time.Second)
	snapshot.gitRepositoriesOpenedAt = snapshot.observedAt.Add(-time.Nanosecond)
	snapshot.platform.identity = report.GitOpsIdentity
	snapshot.bootstrap.identity = report.GitOpsBootstrapIdentity
	snapshot.system.identity = report.GitOpsSystemIdentity
	snapshot.loomCore.identity = report.LoomCoreIdentity
	snapshot.gitRepositories = provenanceGitRepositorySnapshot(testGitRepositoryProvenanceSnapshot(
		"repo-test-rv", snapshot.observedAt,
		snapshot.platform.applied, snapshot.loomCore.applied,
		report.GitOpsIdentity, report.LoomCoreIdentity,
	))
	snapshot, err = bindReviewedFluxRenderSpecs(snapshot,
		report.GitOpsIdentity.BaselineRevision,
		report.GitOpsIdentity.BaselineDigest,
		testReviewedFluxSpecDigests())
	if err != nil {
		t.Fatalf("bind test reviewed Flux specs: %v", err)
	}
	report.FluxSourcesEnd = snapshot.provenance()
}

func TestReviewedFluxRenderSpecDigestsMatchCanonicalLiveSpecs(t *testing.T) {
	repo, revision := newGitOpsScopeRepo(t, true)
	digests, err := reviewedFluxRenderSpecDigests(t.Context(), repo, revision)
	if err != nil {
		t.Fatalf("reviewedFluxRenderSpecDigests() error = %v", err)
	}
	for _, name := range requiredFluxProvenanceOwners {
		live, err := parseFluxRenderSpecIdentity(name, json.RawMessage(testFluxSpecJSON(name)))
		if err != nil {
			t.Fatalf("parse live %s spec: %v", name, err)
		}
		if digests[name] != live.SpecSHA256 {
			t.Fatalf("%s reviewed/live spec digest = %q/%q", name, digests[name], live.SpecSHA256)
		}
	}
}

func TestReviewedFluxRenderSpecDigestsRejectsWrongManifestNamespace(t *testing.T) {
	repo, _ := newGitOpsScopeRepo(t, true)
	path := requiredFluxRenderSpecs["apps"].ManifestPath
	blob, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	wrongNamespace := strings.Replace(string(blob), "namespace: flux-system", "namespace: other", 1)
	if wrongNamespace == string(blob) {
		t.Fatal("apps fixture has no namespace to replace")
	}
	writeGitOpsScopeFile(t, repo, path, wrongNamespace, 0o644)
	revision := commitGitOpsScope(t, repo, "wrong reviewed manifest namespace")

	if _, err := reviewedFluxRenderSpecDigests(t.Context(), repo, revision); err == nil ||
		!strings.Contains(err.Error(), `namespace="other"`) {
		t.Fatalf("wrong reviewed manifest namespace accepted: %v", err)
	}
}

func TestReviewedFluxRenderSpecDigestsRejectsTrailingYAMLDocument(t *testing.T) {
	tests := []struct {
		name   string
		suffix string
		want   string
	}{
		{
			name: "conflicting document",
			suffix: "\n---\napiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\n" +
				"metadata:\n  name: apps\n  namespace: other\nspec:\n  path: ./attacker\n",
			want: "multiple YAML documents",
		},
		{
			name:   "invalid document",
			suffix: "\n---\nmetadata: [\n",
			want:   "decode trailing YAML document",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, _ := newGitOpsScopeRepo(t, true)
			path := requiredFluxRenderSpecs["apps"].ManifestPath
			blob, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(path)))
			if err != nil {
				t.Fatal(err)
			}
			writeGitOpsScopeFile(t, repo, path, string(blob)+test.suffix, 0o644)
			revision := commitGitOpsScope(t, repo, "trailing reviewed manifest document")

			if _, err := reviewedFluxRenderSpecDigests(t.Context(), repo, revision); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("trailing reviewed manifest document accepted: %v", err)
			}
		})
	}
}

func TestReviewedFluxRenderSpecBindingRejectsUnreviewedFullSpecField(t *testing.T) {
	repo, revision := newGitOpsScopeRepo(t, true)
	digests, err := reviewedFluxRenderSpecDigests(t.Context(), repo, revision)
	if err != nil {
		t.Fatal(err)
	}
	var list gitOpsKustomizationListWire
	if err := json.Unmarshal([]byte(fluxSourceListJSON(revision, true, revision, true)), &list); err != nil {
		t.Fatal(err)
	}
	var liveSpec map[string]any
	if err := json.Unmarshal(list.Items[0].Spec, &liveSpec); err != nil {
		t.Fatal(err)
	}
	// postBuild is deliberately outside the explicit routing fields. The full
	// canonical digest must still reject it when the reviewed manifest lacks it.
	liveSpec["postBuild"] = map[string]any{"substitute": map[string]any{"UNREVIEWED": "true"}}
	list.Items[0].Spec, err = json.Marshal(liveSpec)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := parseFluxSourceSnapshot(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bindReviewedFluxRenderSpecs(snapshot, revision, revision, digests); err == nil ||
		!strings.Contains(err.Error(), "does not match reviewed Git manifest") {
		t.Fatalf("unreviewed full-spec field accepted: %v", err)
	}
}

func TestResolveGitOpsScopeIdentityUnrelatedDescendant(t *testing.T) {
	repo, baseline := newGitOpsScopeRepo(t, true)
	writeGitOpsScopeFile(t, repo, "mcp/context/registry.yaml", "unrelated: true\n", 0o644)
	observed := commitGitOpsScope(t, repo, "unrelated registry update")

	identity, err := ResolveGitOpsScopeIdentity(context.Background(), repo, baseline, observed,
		GitOpsIdentityModeProtectedScope)
	if err != nil {
		t.Fatalf("ResolveGitOpsScopeIdentity() error = %v", err)
	}
	if identity.BaselineRevision != baseline || identity.ObservedRevision != observed {
		t.Fatalf("identity revisions = %+v", identity)
	}
	if identity.BaselineDigest == "" || identity.BaselineDigest != identity.ObservedDigest {
		t.Fatalf("protected digests = %q -> %q", identity.BaselineDigest, identity.ObservedDigest)
	}
	if identity.Contract != "platform-gitops" || identity.ContractVersion != 1 || identity.CheckedCommitCount != 2 {
		t.Fatalf("protected identity contract = %+v", identity)
	}
}

func TestResolveGitOpsScopeIdentityRejectsIntermediateProtectedChangeRevertedAtEndpoint(t *testing.T) {
	repo, baseline := newGitOpsScopeRepo(t, true)
	protectedPath := "k3s/mills/deployment.yaml"
	writeGitOpsScopeFile(t, repo, protectedPath, "kind: Deployment\nchanged: transiently\n", 0o644)
	changed := commitGitOpsScope(t, repo, "transient protected change")
	writeGitOpsScopeFile(t, repo, protectedPath, fmt.Sprintf("fixture: %s\n", protectedPath), 0o644)
	observed := commitGitOpsScope(t, repo, "revert protected change")

	identity, err := ResolveGitOpsScopeIdentity(context.Background(), repo, baseline, observed,
		GitOpsIdentityModeProtectedScope)
	if err == nil || !strings.Contains(err.Error(), "protected scope changed at ancestry revision "+changed) {
		t.Fatalf("ResolveGitOpsScopeIdentity() identity=%+v error=%v, want intermediate-change failure", identity, err)
	}
	if identity.BaselineDigest == "" || identity.BaselineDigest != identity.ObservedDigest {
		t.Fatalf("endpoint digests should prove the revert while ancestry rejects it: %+v", identity)
	}
}

func TestResolveGitOpsScopeIdentityCoversCrashAndEvidenceDependencyClosure(t *testing.T) {
	paths := []string{
		"clusters/k3s/flux-system/kustomization-monitoring.yaml",
		"k3s/flux/system/kustomization.yaml",
		"k3s/system/ingress-nginx/helmchart.yaml",
		"k3s/net/metallb-pool.yaml",
		"k3s/coredns/coredns-custom.yaml",
		"k3s/kube-vip/kube-vip.yaml",
		"k3s/flux/apps/logging/kustomization.yaml",
		"k3s/logging/loki-single.yaml",
	}
	for _, protectedPath := range paths {
		t.Run(protectedPath, func(t *testing.T) {
			repo, baseline := newGitOpsScopeRepo(t, true)
			writeGitOpsScopeFile(t, repo, protectedPath, "changed: true\n", 0o644)
			observed := commitGitOpsScope(t, repo, "change crash-proof dependency")
			identity, err := ResolveGitOpsScopeIdentity(context.Background(), repo, baseline, observed,
				GitOpsIdentityModeProtectedScope)
			if err == nil || !strings.Contains(err.Error(), "protected scope changed") {
				t.Fatalf("ResolveGitOpsScopeIdentity() identity=%+v error=%v", identity, err)
			}
		})
	}
}

func TestResolveLoomCoreScopeIdentityUnrelatedDescendant(t *testing.T) {
	repo, baseline := newLoomCoreScopeRepo(t)
	writeGitOpsScopeFile(t, repo, "pkg/mills/runtime.go", "package mills\n", 0o644)
	observed := commitGitOpsScope(t, repo, "unrelated loom-core change")

	identity, err := ResolveLoomCoreScopeIdentity(context.Background(), repo, baseline, observed,
		GitOpsIdentityModeProtectedScope)
	if err != nil {
		t.Fatalf("ResolveLoomCoreScopeIdentity() error = %v", err)
	}
	if identity.Contract != "loom-core-source" || identity.ContractVersion != 1 ||
		identity.CheckedCommitCount != 2 || identity.BaselineDigest != identity.ObservedDigest {
		t.Fatalf("loom-core identity = %+v", identity)
	}
}

func TestResolveLoomCoreScopeIdentityK8sBaseChangeFails(t *testing.T) {
	repo, baseline := newLoomCoreScopeRepo(t)
	writeGitOpsScopeFile(t, repo, "k8s/base/servers/mobile-hud/deployment.yaml", "changed: true\n", 0o644)
	observed := commitGitOpsScope(t, repo, "change mobile HUD manifest")

	identity, err := ResolveLoomCoreScopeIdentity(context.Background(), repo, baseline, observed,
		GitOpsIdentityModeProtectedScope)
	if err == nil || !strings.Contains(err.Error(), "loom-core source protected scope changed") {
		t.Fatalf("ResolveLoomCoreScopeIdentity() identity=%+v error=%v", identity, err)
	}
}

func TestPreflightCapturesProtectedIdentityWhileFluxIsScanning(t *testing.T) {
	repo, baseline := newGitOpsScopeRepo(t, true)
	writeGitOpsScopeFile(t, repo, "mcp/context/registry.yaml", "unrelated: true\n", 0o644)
	observed := commitGitOpsScope(t, repo, "unrelated registry update")

	h := New(Config{
		ExpectedGitOpsRevision: baseline,
		GitOpsIdentityMode:     GitOpsIdentityModeProtectedScope,
		GitOpsRepoPath:         repo,
	})
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		command := strings.Join(args, " ")
		switch {
		case strings.Contains(command, "get ns"):
			return testNamespaceListJSON(s1cOperatorNamespace, "loom-hub", s1cSpawnNamespace, "logging"), nil
		case strings.Contains(command, "/apis/source.toolkit.fluxcd.io/v1/namespaces/flux-system/gitrepositories?limit=0"):
			return gitRepositoryListJSON(observed, strings.Repeat("b", 40)), nil
		case strings.Contains(command, "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/flux-system/kustomizations?limit=0"):
			return fluxSourceListJSON(observed, false, strings.Repeat("b", 40), true), nil
		default:
			return "", fmt.Errorf("unexpected kubectl call after Flux convergence failure: %s", command)
		}
	}

	report, err := h.Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "flux apps is not converged") {
		t.Fatalf("Preflight() error = %v, want convergence failure", err)
	}
	if report.GitOpsIdentity.Mode != GitOpsIdentityModeProtectedScope ||
		report.GitOpsIdentity.BaselineRevision != baseline ||
		report.GitOpsIdentity.ObservedRevision != observed ||
		report.GitOpsIdentity.BaselineDigest == "" ||
		report.GitOpsIdentity.BaselineDigest != report.GitOpsIdentity.ObservedDigest {
		t.Fatalf("protected identity was not serialized before convergence retry: %+v", report.GitOpsIdentity)
	}
}

func TestSameFluxSourceFenceRejectsMixedSnapshotAndAllowsProvenDescendant(t *testing.T) {
	diagnostic := fluxSourceState{
		name: "apps", uid: "apps-uid", generation: 1,
		applied: "main@sha1:start", attempted: "main@sha1:start", ready: true,
		identity: GitOpsScopeIdentity{Mode: GitOpsIdentityModeExactRevision, Contract: "platform-gitops", ContractVersion: 1},
	}
	changed := diagnostic
	changed.applied, changed.attempted = "main@sha1:end", "main@sha1:end"
	if err := sameFluxSourceFence(diagnostic, changed); err == nil || !strings.Contains(err.Error(), "without a reviewed baseline") {
		t.Fatalf("unreviewed mixed snapshot accepted: %v", err)
	}

	protected := fluxSourceState{
		name: "apps", uid: "apps-uid", generation: 1,
		applied: "main@sha1:start", attempted: "main@sha1:start", ready: true,
		identity: GitOpsScopeIdentity{
			Mode: GitOpsIdentityModeProtectedScope, Contract: "platform-gitops", ContractVersion: 1,
			BaselineRevision: "baseline", ObservedRevision: "start",
			BaselineDigest: "scope", ObservedDigest: "scope", CheckedCommitCount: 2,
		},
	}
	descendant := protected
	descendant.applied, descendant.attempted = "main@sha1:end", "main@sha1:end"
	descendant.identity.ObservedRevision = "end"
	descendant.identity.CheckedCommitCount = 3
	if err := sameFluxSourceFence(protected, descendant); err != nil {
		t.Fatalf("proven protected-scope descendant rejected: %v", err)
	}
	descendant.identity.ObservedDigest = "changed"
	if err := sameFluxSourceFence(protected, descendant); err == nil || !strings.Contains(err.Error(), "protected identity changed") {
		t.Fatalf("protected-scope drift accepted: %v", err)
	}
}

func TestRecheckSourceIdentitiesBindsBothFluxSources(t *testing.T) {
	platformRevision := strings.Repeat("a", 40)
	loomCoreRevision := strings.Repeat("b", 40)
	observedLoomCore := loomCoreRevision
	h := New(Config{
		ExpectedGitOpsRevision:   platformRevision,
		ExpectedLoomCoreRevision: loomCoreRevision,
		GitOpsIdentityMode:       GitOpsIdentityModeExactRevision,
	})
	configureTestReviewedFluxSpecs(h)
	reads := 0
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		command := strings.Join(args, " ")
		if strings.Contains(command, "/apis/source.toolkit.fluxcd.io/v1/namespaces/flux-system/gitrepositories?limit=0") {
			return gitRepositoryListJSON(platformRevision, observedLoomCore), nil
		}
		if strings.Contains(command, "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/flux-system/kustomizations?limit=0") {
			reads++
			return fluxSourceListJSON(platformRevision, true, observedLoomCore, true), nil
		}
		return "", fmt.Errorf("unexpected kubectl call: %s", command)
	}
	platformIdentity, err := h.resolveGitOpsIdentity(context.Background(), "main@sha1:"+platformRevision)
	if err != nil {
		t.Fatal(err)
	}
	loomCoreIdentity, err := h.resolveLoomCoreIdentity(context.Background(), "main@sha1:"+loomCoreRevision)
	if err != nil {
		t.Fatal(err)
	}
	want := PreflightReport{
		GitOpsRevision: "main@sha1:" + platformRevision, GitOpsAttempted: "main@sha1:" + platformRevision,
		GitOpsReady: true, GitOpsIdentity: platformIdentity,
		GitOpsBootstrapRevision: "main@sha1:" + platformRevision, GitOpsBootstrapAttempted: "main@sha1:" + platformRevision,
		GitOpsBootstrapReady: true, GitOpsBootstrapIdentity: platformIdentity,
		GitOpsSystemRevision: "main@sha1:" + platformRevision, GitOpsSystemAttempted: "main@sha1:" + platformRevision,
		GitOpsSystemReady: true, GitOpsSystemIdentity: platformIdentity,
		LoomCoreRevision: "main@sha1:" + loomCoreRevision, LoomCoreAttempted: "main@sha1:" + loomCoreRevision,
		LoomCoreReady: true, LoomCoreIdentity: loomCoreIdentity,
	}
	bindTestPreflightFluxEnd(t, &want, fluxSourceListJSON(platformRevision, true, loomCoreRevision, true))
	if err := h.RecheckSourceIdentities(context.Background(), want); err != nil {
		t.Fatalf("unchanged source identities rejected: %v", err)
	}
	if reads != 2 {
		t.Fatalf("unchanged source recheck List reads = %d, want prepare+final", reads)
	}
	observedLoomCore = strings.Repeat("c", 40)
	if err := h.RecheckSourceIdentities(context.Background(), want); err == nil || !strings.Contains(err.Error(), "exact loom-core source revision changed") {
		t.Fatalf("loom-core source drift accepted: %v", err)
	}
	if reads != 3 {
		t.Fatalf("drifted source recheck List reads = %d, want fail during prepare", reads)
	}
}

func TestPrepareSourceIdentityFenceBindsEveryPlatformRenderOwner(t *testing.T) {
	for _, source := range []string{"apps", "bootstrap", "system"} {
		t.Run(source, func(t *testing.T) {
			platformRevision := strings.Repeat("a", 40)
			driftedRevision := strings.Repeat("c", 40)
			loomCoreRevision := strings.Repeat("b", 40)
			h := New(Config{
				ExpectedGitOpsRevision:   platformRevision,
				ExpectedLoomCoreRevision: loomCoreRevision,
				GitOpsIdentityMode:       GitOpsIdentityModeExactRevision,
			})
			configureTestReviewedFluxSpecs(h)
			h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
				command := strings.Join(args, " ")
				if strings.Contains(command, "/apis/source.toolkit.fluxcd.io/v1/namespaces/flux-system/gitrepositories?limit=0") {
					return gitRepositoryListJSON(platformRevision, loomCoreRevision), nil
				}
				var list gitOpsKustomizationListWire
				if err := json.Unmarshal([]byte(fluxSourceListJSON(platformRevision, true, loomCoreRevision, true)), &list); err != nil {
					return "", err
				}
				for i := range list.Items {
					if list.Items[i].Metadata.Name == source {
						list.Items[i].Status.LastAppliedRevision = "main@sha1:" + driftedRevision
						list.Items[i].Status.LastAttemptedRevision = "main@sha1:" + driftedRevision
					}
				}
				encoded, err := json.Marshal(list)
				return string(encoded), err
			}
			platformIdentity, err := h.resolveGitOpsIdentity(context.Background(), "main@sha1:"+platformRevision)
			if err != nil {
				t.Fatal(err)
			}
			loomCoreIdentity, err := h.resolveLoomCoreIdentity(context.Background(), "main@sha1:"+loomCoreRevision)
			if err != nil {
				t.Fatal(err)
			}
			want := PreflightReport{
				GitOpsRevision: "main@sha1:" + platformRevision, GitOpsAttempted: "main@sha1:" + platformRevision,
				GitOpsReady: true, GitOpsIdentity: platformIdentity,
				GitOpsBootstrapRevision: "main@sha1:" + platformRevision, GitOpsBootstrapAttempted: "main@sha1:" + platformRevision,
				GitOpsBootstrapReady: true, GitOpsBootstrapIdentity: platformIdentity,
				GitOpsSystemRevision: "main@sha1:" + platformRevision, GitOpsSystemAttempted: "main@sha1:" + platformRevision,
				GitOpsSystemReady: true, GitOpsSystemIdentity: platformIdentity,
				LoomCoreRevision: "main@sha1:" + loomCoreRevision, LoomCoreAttempted: "main@sha1:" + loomCoreRevision,
				LoomCoreReady: true, LoomCoreIdentity: loomCoreIdentity,
			}
			bindTestPreflightFluxEnd(t, &want, fluxSourceListJSON(platformRevision, true, loomCoreRevision, true))
			if _, err := h.PrepareSourceIdentityFence(context.Background(), want); err == nil ||
				!strings.Contains(err.Error(), "differs from gitops-gitlab artifact") {
				t.Fatalf("drifted %s render owner accepted: %v", source, err)
			}
		})
	}
}

func TestFinalizeSourceIdentityFenceRejectsObjectMutationEvenWhenRevisionReverts(t *testing.T) {
	for _, source := range []string{"apps", "bootstrap", "system", "loom-hub-servers"} {
		t.Run(source, func(t *testing.T) {
			platformRevision := strings.Repeat("a", 40)
			loomCoreRevision := strings.Repeat("b", 40)
			resourceVersions := map[string]string{
				"apps": "101", "bootstrap": "151", "system": "181", "loom-hub-servers": "202",
			}
			reads := 0
			h := New(Config{
				ExpectedGitOpsRevision:   platformRevision,
				ExpectedLoomCoreRevision: loomCoreRevision,
				GitOpsIdentityMode:       GitOpsIdentityModeExactRevision,
			})
			configureTestReviewedFluxSpecs(h)
			renderList := func() (string, error) {
				var list gitOpsKustomizationListWire
				raw := fluxSourceListJSONWithVersions(platformRevision, true, resourceVersions["apps"],
					loomCoreRevision, true, resourceVersions["loom-hub-servers"])
				if err := json.Unmarshal([]byte(raw), &list); err != nil {
					return "", err
				}
				for i := range list.Items {
					list.Items[i].Metadata.ResourceVersion = resourceVersions[list.Items[i].Metadata.Name]
				}
				encoded, err := json.Marshal(list)
				return string(encoded), err
			}
			h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
				command := strings.Join(args, " ")
				if strings.Contains(command, "/apis/source.toolkit.fluxcd.io/v1/namespaces/flux-system/gitrepositories?limit=0") {
					return gitRepositoryListJSON(platformRevision, loomCoreRevision), nil
				}
				if strings.Contains(command, "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/flux-system/kustomizations?limit=0") {
					reads++
					return renderList()
				}
				return "", fmt.Errorf("unexpected kubectl call: %s", command)
			}
			platformIdentity, err := h.resolveGitOpsIdentity(context.Background(), "main@sha1:"+platformRevision)
			if err != nil {
				t.Fatal(err)
			}
			loomCoreIdentity, err := h.resolveLoomCoreIdentity(context.Background(), "main@sha1:"+loomCoreRevision)
			if err != nil {
				t.Fatal(err)
			}
			want := PreflightReport{
				GitOpsRevision: "main@sha1:" + platformRevision, GitOpsAttempted: "main@sha1:" + platformRevision,
				GitOpsReady: true, GitOpsIdentity: platformIdentity,
				GitOpsBootstrapRevision: "main@sha1:" + platformRevision, GitOpsBootstrapAttempted: "main@sha1:" + platformRevision,
				GitOpsBootstrapReady: true, GitOpsBootstrapIdentity: platformIdentity,
				GitOpsSystemRevision: "main@sha1:" + platformRevision, GitOpsSystemAttempted: "main@sha1:" + platformRevision,
				GitOpsSystemReady: true, GitOpsSystemIdentity: platformIdentity,
				LoomCoreRevision: "main@sha1:" + loomCoreRevision, LoomCoreAttempted: "main@sha1:" + loomCoreRevision,
				LoomCoreReady: true, LoomCoreIdentity: loomCoreIdentity,
			}
			baselineRaw, err := renderList()
			if err != nil {
				t.Fatal(err)
			}
			bindTestPreflightFluxEnd(t, &want, baselineRaw)
			fence, err := h.PrepareSourceIdentityFence(context.Background(), want)
			if err != nil {
				t.Fatalf("prepare source fence: %v", err)
			}
			// A change-and-revert can restore the same applied revision. Kubernetes
			// resourceVersion still proves that the source object moved in the gap.
			resourceVersions[source] = "999"
			if _, err := h.FinalizeSourceIdentityFence(context.Background(), fence); err == nil || !strings.Contains(err.Error(), "object identity changed") {
				t.Fatalf("change-and-revert source drift accepted: %v", err)
			}
			if reads != 2 {
				t.Fatalf("coherent source List reads = %d, want prepare+final", reads)
			}
		})
	}
}

func TestFinalizeSourceIdentityFenceReturnsVersionedProvenance(t *testing.T) {
	platformRevision := strings.Repeat("a", 40)
	loomCoreRevision := strings.Repeat("b", 40)
	h := New(Config{
		ExpectedGitOpsRevision:   platformRevision,
		ExpectedLoomCoreRevision: loomCoreRevision,
		GitOpsIdentityMode:       GitOpsIdentityModeExactRevision,
	})
	configureTestReviewedFluxSpecs(h)
	listResourceVersion := "9001"
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "/apis/source.toolkit.fluxcd.io/v1/namespaces/flux-system/gitrepositories?limit=0") {
			return gitRepositoryListJSON(platformRevision, loomCoreRevision), nil
		}
		return strings.Replace(
			fluxSourceListJSON(platformRevision, true, loomCoreRevision, true),
			`"resourceVersion":"9001"`, `"resourceVersion":"`+listResourceVersion+`"`, 1,
		), nil
	}
	platformIdentity, err := h.resolveGitOpsIdentity(context.Background(), "main@sha1:"+platformRevision)
	if err != nil {
		t.Fatal(err)
	}
	loomCoreIdentity, err := h.resolveLoomCoreIdentity(context.Background(), "main@sha1:"+loomCoreRevision)
	if err != nil {
		t.Fatal(err)
	}
	want := PreflightReport{
		GitOpsRevision: "main@sha1:" + platformRevision, GitOpsAttempted: "main@sha1:" + platformRevision,
		GitOpsReady: true, GitOpsIdentity: platformIdentity,
		GitOpsBootstrapRevision: "main@sha1:" + platformRevision, GitOpsBootstrapAttempted: "main@sha1:" + platformRevision,
		GitOpsBootstrapReady: true, GitOpsBootstrapIdentity: platformIdentity,
		GitOpsSystemRevision: "main@sha1:" + platformRevision, GitOpsSystemAttempted: "main@sha1:" + platformRevision,
		GitOpsSystemReady: true, GitOpsSystemIdentity: platformIdentity,
		LoomCoreRevision: "main@sha1:" + loomCoreRevision, LoomCoreAttempted: "main@sha1:" + loomCoreRevision,
		LoomCoreReady: true, LoomCoreIdentity: loomCoreIdentity,
	}
	bindTestPreflightFluxEnd(t, &want, strings.Replace(
		fluxSourceListJSON(platformRevision, true, loomCoreRevision, true),
		`"resourceVersion":"9001"`, `"resourceVersion":"`+listResourceVersion+`"`, 1,
	))
	fence, err := h.PrepareSourceIdentityFence(context.Background(), want)
	if err != nil {
		t.Fatalf("prepare source fence: %v", err)
	}
	listResourceVersion = "9002"
	evidence, err := h.FinalizeSourceIdentityFence(context.Background(), fence)
	if err != nil {
		t.Fatalf("finalize source fence: %v", err)
	}
	if evidence.Prepared.Contract != FluxProvenanceContract ||
		evidence.Prepared.ContractVersion != FluxProvenanceContractVersion ||
		evidence.Prepared.ListResourceVersion != "9001" || evidence.Final.ListResourceVersion != "9002" {
		t.Fatalf("unexpected serialized fence: %+v", evidence)
	}
	if err := ValidateFluxSourceFenceEvidence(evidence); err != nil {
		t.Fatalf("returned provenance is not independently valid: %v", err)
	}
}

func TestResolveGitOpsScopeIdentityProtectedChangesFail(t *testing.T) {
	tests := map[string]func(*testing.T, string){
		"modify": func(t *testing.T, repo string) {
			writeGitOpsScopeFile(t, repo, "k3s/mills/deployment.yaml", "kind: Deployment\nchanged: true\n", 0o644)
		},
		"add": func(t *testing.T, repo string) {
			writeGitOpsScopeFile(t, repo, "k3s/mills/new-resource.yaml", "kind: ConfigMap\n", 0o644)
		},
		"delete": func(t *testing.T, repo string) {
			if err := os.Remove(filepath.Join(repo, "k3s/loom-hub/rbac.yaml")); err != nil {
				t.Fatal(err)
			}
		},
		"chmod": func(t *testing.T, repo string) {
			if err := os.Chmod(filepath.Join(repo, "k3s/devbox/namespace.yaml"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, repo string) {
			link := filepath.Join(repo, "k3s/security-posture/linked-policy.yaml")
			if err := os.Symlink("kustomization.yaml", link); err != nil {
				t.Fatal(err)
			}
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			repo, baseline := newGitOpsScopeRepo(t, true)
			mutate(t, repo)
			observed := commitGitOpsScope(t, repo, "protected "+name)
			identity, err := ResolveGitOpsScopeIdentity(context.Background(), repo, baseline, observed,
				GitOpsIdentityModeProtectedScope)
			if err == nil || !strings.Contains(err.Error(), "protected scope changed") {
				t.Fatalf("ResolveGitOpsScopeIdentity() identity=%+v error=%v, want protected-scope failure", identity, err)
			}
			if identity.BaselineDigest == "" || identity.ObservedDigest == "" ||
				identity.BaselineDigest == identity.ObservedDigest {
				t.Fatalf("changed protected digests = %q -> %q", identity.BaselineDigest, identity.ObservedDigest)
			}
		})
	}
}

func TestResolveGitOpsScopeIdentityExactRevision(t *testing.T) {
	repo, baseline := newGitOpsScopeRepo(t, true)
	identity, err := ResolveGitOpsScopeIdentity(context.Background(), repo,
		"main@sha1:"+strings.ToUpper(baseline), baseline, GitOpsIdentityModeExactRevision)
	if err != nil {
		t.Fatalf("exact identity error = %v", err)
	}
	if identity.BaselineDigest != baseline || identity.ObservedDigest != baseline {
		t.Fatalf("exact identity = %+v", identity)
	}

	writeGitOpsScopeFile(t, repo, "mcp/context/registry.yaml", "unrelated: true\n", 0o644)
	observed := commitGitOpsScope(t, repo, "unrelated descendant")
	identity, err = ResolveGitOpsScopeIdentity(context.Background(), repo, baseline, observed,
		GitOpsIdentityModeExactRevision)
	if err == nil || !strings.Contains(err.Error(), "exact GitOps revision changed") {
		t.Fatalf("exact descendant identity=%+v error=%v", identity, err)
	}
}

func TestResolveGitOpsScopeIdentityRejectsNonDescendant(t *testing.T) {
	repo, baseline := newGitOpsScopeRepo(t, true)
	runGitOpsScopeTest(t, repo, "checkout", "--orphan", "other")
	runGitOpsScopeTest(t, repo, "rm", "-rf", ".")
	seedGitOpsProtectedScope(t, repo)
	observed := commitGitOpsScope(t, repo, "unrelated root")

	identity, err := ResolveGitOpsScopeIdentity(context.Background(), repo, baseline, observed,
		GitOpsIdentityModeProtectedScope)
	if err == nil || !strings.Contains(err.Error(), "is not an ancestor") {
		t.Fatalf("ResolveGitOpsScopeIdentity() identity=%+v error=%v, want ancestry failure", identity, err)
	}
}

func TestResolveGitOpsScopeIdentityRejectsAncestryAboveBound(t *testing.T) {
	repo, baseline := newGitOpsScopeRepo(t, true)
	tree := strings.TrimSpace(runGitOpsScopeTest(t, repo, "rev-parse", baseline+"^{tree}"))
	observed := baseline
	for i := 0; i < maxGitScopeAncestryCommits+1; i++ {
		observed = strings.TrimSpace(runGitOpsScopeTest(t, repo,
			"commit-tree", tree, "-p", observed, "-m", fmt.Sprintf("empty descendant %d", i)))
	}
	runGitOpsScopeTest(t, repo, "update-ref", "refs/heads/main", observed)

	identity, err := ResolveGitOpsScopeIdentity(context.Background(), repo, baseline, observed,
		GitOpsIdentityModeProtectedScope)
	if err == nil || !strings.Contains(err.Error(), "more than 512 commits") {
		t.Fatalf("ResolveGitOpsScopeIdentity() identity=%+v error=%v, want ancestry-bound failure", identity, err)
	}
}

func TestResolveGitOpsScopeIdentityFetchesMissingCommit(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitOpsScopeTest(t, "", "init", "--bare", remote)

	upstream := filepath.Join(t.TempDir(), "upstream")
	runGitOpsScopeTest(t, "", "clone", remote, upstream)
	configureGitOpsScopeRepo(t, upstream)
	runGitOpsScopeTest(t, upstream, "checkout", "-b", "main")
	seedGitOpsProtectedScope(t, upstream)
	baseline := commitGitOpsScope(t, upstream, "baseline")
	runGitOpsScopeTest(t, upstream, "push", "-u", "origin", "main")

	consumer := filepath.Join(t.TempDir(), "consumer")
	runGitOpsScopeTest(t, "", "clone", "--branch", "main", remote, consumer)
	configureGitOpsScopeRepo(t, consumer)
	writeGitOpsScopeFile(t, upstream, "mcp/context/registry.yaml", "unrelated: true\n", 0o644)
	observed := commitGitOpsScope(t, upstream, "unrelated remote descendant")
	runGitOpsScopeTest(t, upstream, "push", "origin", "main")

	if _, err := exec.CommandContext(t.Context(), "git", "-C", consumer, "cat-file", "-e", observed+"^{commit}").CombinedOutput(); err == nil {
		t.Fatalf("consumer unexpectedly has observed commit %s before resolver fetch", observed)
	}
	identity, err := ResolveGitOpsScopeIdentity(context.Background(), consumer, baseline, observed,
		GitOpsIdentityModeProtectedScope)
	if err != nil {
		t.Fatalf("ResolveGitOpsScopeIdentity() fetch error = %v", err)
	}
	if identity.BaselineDigest != identity.ObservedDigest {
		t.Fatalf("fetched identity digests = %+v", identity)
	}
}

func TestResolveGitOpsScopeIdentityMissingCommitFailsClosed(t *testing.T) {
	repo, baseline := newGitOpsScopeRepo(t, true)
	missing := strings.Repeat("f", 40)
	identity, err := ResolveGitOpsScopeIdentity(context.Background(), repo, baseline, missing,
		GitOpsIdentityModeProtectedScope)
	if err == nil || !strings.Contains(err.Error(), "fetch origin main") {
		t.Fatalf("ResolveGitOpsScopeIdentity() identity=%+v error=%v, want missing-commit fetch failure", identity, err)
	}
}

func TestResolveGitOpsScopeIdentityMissingBaselinePathFailsClosed(t *testing.T) {
	repo, baseline := newGitOpsScopeRepo(t, false)
	identity, err := ResolveGitOpsScopeIdentity(context.Background(), repo, baseline, baseline,
		GitOpsIdentityModeProtectedScope)
	if err == nil || !strings.Contains(err.Error(), "matched no Git objects") ||
		!strings.Contains(err.Error(), "k3s/security-posture") {
		t.Fatalf("ResolveGitOpsScopeIdentity() identity=%+v error=%v, want missing-path failure", identity, err)
	}
}

func TestResolveGitOpsScopeIdentityRejectsInvalidMode(t *testing.T) {
	identity, err := ResolveGitOpsScopeIdentity(context.Background(), "", "", "", "semantic-ish")
	if err == nil || !strings.Contains(err.Error(), "unsupported GitOps identity mode") {
		t.Fatalf("ResolveGitOpsScopeIdentity() identity=%+v error=%v", identity, err)
	}
}

func TestNormalizeGitOpsScopeRevision(t *testing.T) {
	sha40 := strings.Repeat("aB", 20)
	sha64 := strings.Repeat("Cd", 32)
	for input, want := range map[string]string{
		sha40:                strings.ToLower(sha40),
		"main@sha1:" + sha40: strings.ToLower(sha40),
		sha64:                strings.ToLower(sha64),
	} {
		if got, err := normalizeGitOpsScopeRevision(input); err != nil || got != want {
			t.Errorf("normalizeGitOpsScopeRevision(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "abc", strings.Repeat("g", 40)} {
		if got, err := normalizeGitOpsScopeRevision(input); err == nil {
			t.Errorf("normalizeGitOpsScopeRevision(%q) = %q, nil; want error", input, got)
		}
	}
}

func newGitOpsScopeRepo(t *testing.T, includeSecurityPosture bool) (string, string) {
	t.Helper()
	repo := t.TempDir()
	runGitOpsScopeTest(t, "", "init", "-b", "main", repo)
	configureGitOpsScopeRepo(t, repo)
	seedGitOpsProtectedScope(t, repo)
	if !includeSecurityPosture {
		if err := os.RemoveAll(filepath.Join(repo, "k3s/security-posture")); err != nil {
			t.Fatal(err)
		}
	}
	return repo, commitGitOpsScope(t, repo, "baseline")
}

func newLoomCoreScopeRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	runGitOpsScopeTest(t, "", "init", "-b", "main", repo)
	configureGitOpsScopeRepo(t, repo)
	writeGitOpsScopeFile(t, repo, "k8s/base/kustomization.yaml", "resources:\n  - servers/mobile-hud/deployment.yaml\n", 0o644)
	writeGitOpsScopeFile(t, repo, "k8s/base/servers/mobile-hud/deployment.yaml", "kind: Deployment\n", 0o644)
	return repo, commitGitOpsScope(t, repo, "baseline")
}

func configureGitOpsScopeRepo(t *testing.T, repo string) {
	t.Helper()
	runGitOpsScopeTest(t, repo, "config", "user.name", "S1c Test")
	runGitOpsScopeTest(t, repo, "config", "user.email", "s1c@example.invalid")
	runGitOpsScopeTest(t, repo, "config", "core.filemode", "true")
}

func seedGitOpsProtectedScope(t *testing.T, repo string) {
	t.Helper()
	const testPolicyConfigMapSource = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: loom-mills-policy\n  namespace: loom-mills\ndata:\n  policy.yaml: |\n    policy:\n      workflows:\n        global_enabled: false\n        workflows_enabled: true\n        substrate_k8s_only: true\n"
	files := []string{
		"clusters/k3s/flux-system/gitrepository-gitlab.yaml",
		"clusters/k3s/flux-system/gitrepository-loom-core.yaml",
		"clusters/k3s/flux-system/kustomization-apps.yaml",
		"clusters/k3s/flux-system/kustomization-bootstrap.yaml",
		"clusters/k3s/flux-system/kustomization-loom-hub-servers.yaml",
		"clusters/k3s/flux-system/kustomization-monitoring.yaml",
		"clusters/k3s/flux-system/kustomization-system.yaml",
		"k3s/flux/bootstrap/kustomization.yaml",
		"k3s/flux/apps/kustomization.yaml",
		"k3s/flux/apps/services/kustomization.yaml",
		"k3s/flux/apps/logging/kustomization.yaml",
		"k3s/logging/loki-single.yaml",
		"k3s/flux/system/kustomization.yaml",
		"k3s/system/ingress-nginx/helmchart.yaml",
		"k3s/net/metallb-pool.yaml",
		"k3s/coredns/coredns-custom.yaml",
		"k3s/kube-vip/kube-vip.yaml",
		"k3s/mills/deployment.yaml",
		policyConfigMapSourcePath,
		"k3s/loom-hub/rbac.yaml",
		"k3s/devbox/namespace.yaml",
		"k3s/security-posture/kustomization.yaml",
		"k3s/flux/image-automation/kustomization.yaml",
		"k3s/flux/image-automation/loom-core-imageupdateautomation.yaml",
		"k3s/flux/image-automation/loom-mills-operator-imageupdateautomation.yaml",
	}
	for _, name := range files {
		contents := fmt.Sprintf("fixture: %s\n", name)
		if name == policyConfigMapSourcePath {
			contents = testPolicyConfigMapSource
		}
		for sourceName, contract := range requiredGitRepositorySpecs {
			if name == contract.ManifestPath {
				contents = fmt.Sprintf("apiVersion: source.toolkit.fluxcd.io/v1\nkind: GitRepository\nmetadata:\n  name: %s\n  namespace: flux-system\nspec: %s\n",
					sourceName, testGitRepositorySpecJSON(sourceName))
				break
			}
		}
		for sourceName, contract := range requiredFluxRenderSpecs {
			if name == contract.ManifestPath {
				contents = fmt.Sprintf("apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: %s\n  namespace: flux-system\nspec: %s\n",
					sourceName, testFluxSpecJSON(sourceName))
				break
			}
		}
		writeGitOpsScopeFile(t, repo, name, contents, 0o644)
	}
}

func writeGitOpsScopeFile(t *testing.T, repo, name, contents string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func commitGitOpsScope(t *testing.T, repo, message string) string {
	t.Helper()
	runGitOpsScopeTest(t, repo, "add", "-A")
	runGitOpsScopeTest(t, repo, "commit", "-m", message)
	return strings.TrimSpace(runGitOpsScopeTest(t, repo, "rev-parse", "HEAD"))
}

func runGitOpsScopeTest(t *testing.T, repo string, args ...string) string {
	t.Helper()
	commandArgs := args
	if repo != "" {
		commandArgs = append([]string{"-C", repo}, args...)
	}
	cmd := exec.CommandContext(t.Context(), "git", commandArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(commandArgs, " "), err, output)
	}
	return string(output)
}
