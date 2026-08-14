package killtest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestParseGitRepositorySnapshotProductionFixture(t *testing.T) {
	// Captured from the production source.toolkit.fluxcd.io/v1 List shape on
	// 2026-07-14 and reduced to fields consumed by the parser. The fixture keeps
	// a third unrelated source to prove the persisted contract remains exactly
	// the two sources consumed by the S1c Kustomizations.
	raw := `{"apiVersion":"source.toolkit.fluxcd.io/v1","kind":"GitRepositoryList","metadata":{"continue":"","resourceVersion":"877382782"},"items":[
		{"metadata":{"name":"unrelated","namespace":"flux-system","uid":"other-uid","resourceVersion":"1","generation":1},"spec":{"interval":"1m","url":"https://example.invalid/other.git"},"status":{}},
		{"metadata":{"name":"gitops-gitlab","namespace":"flux-system","uid":"b346189b-3d4f-4d5b-ada5-61aa826c5838","resourceVersion":"877382768","generation":7},"spec":{"interval":"1m","ref":{"branch":"main"},"secretRef":{"name":"gitops-gitlab"},"timeout":"120s","url":"http://gitlab-vm.gitlab.svc.cluster.local/platform/gitops.git"},"status":{"observedGeneration":7,"artifact":{"revision":"main@sha1:cc5bc472efcccb531e876a39c61a1af0e68f6218","digest":"sha256:2b8e9d55280b57d56ef7c781f14ee04f406d7f5f302f655e1c7077db3a4651c2"},"conditions":[{"type":"Ready","status":"True","observedGeneration":7},{"type":"ArtifactInStorage","status":"True","observedGeneration":7}]}},
		{"metadata":{"name":"loom-core","namespace":"flux-system","uid":"7f4f587b-ce8b-4c55-a6ae-a6886d91b741","resourceVersion":"877381865","generation":1},"spec":{"interval":"1m","ref":{"branch":"main"},"secretRef":{"name":"gitops-gitlab"},"timeout":"120s","url":"http://gitlab-vm.gitlab.svc.cluster.local/services/loom-core.git"},"status":{"observedGeneration":1,"artifact":{"revision":"main@sha1:2d611b0f2d15b91e0da7b8e497ceec7e92136111","digest":"sha256:1ec774458b777385a656aa77af8c11c01a55668a5669b2c784fd888f2f955960"},"conditions":[{"type":"Ready","status":"True","observedGeneration":1},{"type":"ArtifactInStorage","status":"True","observedGeneration":1}]}}
	]}`
	snapshot, err := parseGitRepositorySnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.resourceVersion != "877382782" ||
		snapshot.platform.UID != "b346189b-3d4f-4d5b-ada5-61aa826c5838" ||
		snapshot.platform.StatusObservedGeneration != 7 ||
		snapshot.platform.ArtifactRevision != "main@sha1:cc5bc472efcccb531e876a39c61a1af0e68f6218" ||
		snapshot.loomCore.ArtifactDigest != "sha256:1ec774458b777385a656aa77af8c11c01a55668a5669b2c784fd888f2f955960" {
		t.Fatalf("production GitRepository fixture parsed incorrectly: %+v", snapshot)
	}
}

func TestReviewedGitRepositorySpecsMatchCanonicalLiveSpecs(t *testing.T) {
	repo, revision := newGitOpsScopeRepo(t, true)
	digests, err := reviewedGitRepositorySpecDigests(t.Context(), repo, revision)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range requiredGitRepositoryNames {
		live, err := parseGitRepositorySpecIdentity(name, json.RawMessage(testGitRepositorySpecJSON(name)))
		if err != nil {
			t.Fatal(err)
		}
		if digests[name] != live.SpecSHA256 {
			t.Fatalf("%s reviewed/live canonical spec mismatch: %s != %s", name, digests[name], live.SpecSHA256)
		}
	}
}

func TestReviewedGitRepositorySpecBindingRejectsUnreviewedFullSpecField(t *testing.T) {
	repo, revision := newGitOpsScopeRepo(t, true)
	digests, err := reviewedGitRepositorySpecDigests(t.Context(), repo, revision)
	if err != nil {
		t.Fatal(err)
	}
	var list gitRepositoryListWire
	if err := json.Unmarshal([]byte(gitRepositoryListJSON(revision, revision)), &list); err != nil {
		t.Fatal(err)
	}
	var liveSpec map[string]any
	if err := json.Unmarshal(list.Items[0].Spec, &liveSpec); err != nil {
		t.Fatal(err)
	}
	// include is outside the explicit URL/ref/secret routing fields. The full
	// canonical hash must still fail when the reviewed manifest omitted it.
	liveSpec["include"] = []any{map[string]any{"repository": map[string]any{"name": "other"}}}
	list.Items[0].Spec, err = json.Marshal(liveSpec)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := parseGitRepositorySnapshot(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bindReviewedGitRepositorySpecs(snapshot, revision, revision, digests); err == nil ||
		!strings.Contains(err.Error(), "does not match reviewed Git manifest") {
		t.Fatalf("unreviewed complete GitRepository spec field accepted: %v", err)
	}
}

func TestParseGitRepositorySnapshotRejectsIncompleteOrRedirectedSources(t *testing.T) {
	platformRevision := strings.Repeat("a", 40)
	loomCoreRevision := strings.Repeat("b", 40)
	baseline := gitRepositoryListJSON(platformRevision, loomCoreRevision)
	tests := map[string]func(*gitRepositoryListWire){
		"wrong namespace":         func(list *gitRepositoryListWire) { list.Items[0].Metadata.Namespace = "default" },
		"missing UID":             func(list *gitRepositoryListWire) { list.Items[0].Metadata.UID = "" },
		"missing resourceVersion": func(list *gitRepositoryListWire) { list.Items[0].Metadata.ResourceVersion = "" },
		"terminating": func(list *gitRepositoryListWire) {
			deletedAt := "2026-07-14T12:00:00Z"
			list.Items[0].Metadata.DeletionTimestamp = &deletedAt
		},
		"stale status generation": func(list *gitRepositoryListWire) { list.Items[0].Status.ObservedGeneration-- },
		"stale Ready generation": func(list *gitRepositoryListWire) {
			list.Items[0].Status.Conditions[0].ObservedGeneration--
		},
		"false ArtifactInStorage": func(list *gitRepositoryListWire) {
			list.Items[0].Status.Conditions[1].Status = "False"
		},
		"duplicate Ready": func(list *gitRepositoryListWire) {
			list.Items[0].Status.Conditions = append(list.Items[0].Status.Conditions, list.Items[0].Status.Conditions[0])
		},
		"bad artifact revision": func(list *gitRepositoryListWire) { list.Items[0].Status.Artifact.Revision = "main" },
		"bad artifact digest":   func(list *gitRepositoryListWire) { list.Items[0].Status.Artifact.Digest = "sha256:ABC" },
		"URL redirect": func(list *gitRepositoryListWire) {
			mutateGitRepositoryWireSpec(t, &list.Items[0], func(spec map[string]any) { spec["url"] = "https://evil.invalid/repo.git" })
		},
		"ref redirect": func(list *gitRepositoryListWire) {
			mutateGitRepositoryWireSpec(t, &list.Items[0], func(spec map[string]any) { spec["ref"] = map[string]any{"branch": "other"} })
		},
		"secret redirect": func(list *gitRepositoryListWire) {
			mutateGitRepositoryWireSpec(t, &list.Items[0], func(spec map[string]any) { spec["secretRef"] = map[string]any{"name": "other"} })
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			var list gitRepositoryListWire
			if err := json.Unmarshal([]byte(baseline), &list); err != nil {
				t.Fatal(err)
			}
			mutate(&list)
			raw, err := json.Marshal(list)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseGitRepositorySnapshot(string(raw)); err == nil {
				t.Fatal("invalid GitRepository source accepted")
			}
		})
	}
}

func TestGitRepositoryBracketRejectsRequiredSourceMutation(t *testing.T) {
	platformRevision := strings.Repeat("a", 40)
	loomCoreRevision := strings.Repeat("b", 40)
	tests := map[string]func(*gitRepositoryListWire){
		"UID":             func(list *gitRepositoryListWire) { list.Items[0].Metadata.UID = "recreated-uid" },
		"resourceVersion": func(list *gitRepositoryListWire) { list.Items[0].Metadata.ResourceVersion = "999" },
		"generation": func(list *gitRepositoryListWire) {
			list.Items[0].Metadata.Generation++
			list.Items[0].Status.ObservedGeneration++
			for i := range list.Items[0].Status.Conditions {
				list.Items[0].Status.Conditions[i].ObservedGeneration++
			}
		},
		"artifact revision": func(list *gitRepositoryListWire) {
			list.Items[0].Status.Artifact.Revision = "main@sha1:" + strings.Repeat("e", 40)
		},
		"artifact digest": func(list *gitRepositoryListWire) {
			list.Items[0].Status.Artifact.Digest = "sha256:" + strings.Repeat("e", 64)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			first := gitRepositoryListJSON(platformRevision, loomCoreRevision)
			var second gitRepositoryListWire
			if err := json.Unmarshal([]byte(first), &second); err != nil {
				t.Fatal(err)
			}
			mutate(&second)
			secondRaw, err := json.Marshal(second)
			if err != nil {
				t.Fatal(err)
			}
			repositoryReads := 0
			h := New(Config{})
			h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
				command := strings.Join(args, " ")
				switch {
				case strings.Contains(command, "/gitrepositories?limit=0"):
					repositoryReads++
					if repositoryReads == 1 {
						return first, nil
					}
					return string(secondRaw), nil
				case strings.Contains(command, "/kustomizations?limit=0"):
					return fluxSourceListJSON(platformRevision, true, loomCoreRevision, true), nil
				default:
					return "", fmt.Errorf("unexpected kubectl call: %s", command)
				}
			}
			if _, err := h.readFluxSourceSnapshot(t.Context()); err == nil || !strings.Contains(err.Error(), "changed across") {
				t.Fatalf("GitRepository %s mutation crossed bracket: %v", name, err)
			}
		})
	}
}

func TestValidateFluxSourceFenceEvidenceRejectsGitRepositoryDrift(t *testing.T) {
	tests := map[string]func(*GitRepositoryProvenance){
		"URL":    func(repository *GitRepositoryProvenance) { repository.Spec.URL = "https://evil.invalid/repo.git" },
		"ref":    func(repository *GitRepositoryProvenance) { repository.Spec.RefBranch = "other" },
		"secret": func(repository *GitRepositoryProvenance) { repository.Spec.SecretRefName = "other" },
		"artifact revision": func(repository *GitRepositoryProvenance) {
			repository.ArtifactRevision = "main@sha1:" + strings.Repeat("e", 40)
		},
		"artifact digest": func(repository *GitRepositoryProvenance) {
			repository.ArtifactDigest = "sha256:" + strings.Repeat("e", 64)
		},
		"UID": func(repository *GitRepositoryProvenance) { repository.UID = "recreated-uid" },
		"generation": func(repository *GitRepositoryProvenance) {
			repository.Generation++
			repository.StatusObservedGeneration++
			repository.ReadyObservedGeneration++
			repository.ArtifactInStorageObservedGeneration++
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fence := passingFluxFenceEvidence()
			mutate(&fence.Final.GitRepositories.Repositories[0])
			if err := ValidateFluxSourceFenceEvidence(fence); err == nil {
				t.Fatalf("GitRepository %s drift accepted", name)
			}
		})
	}
}

func TestParseFluxSourceSnapshotRejectsDuplicateReadyCondition(t *testing.T) {
	var list gitOpsKustomizationListWire
	if err := json.Unmarshal([]byte(fluxSourceListJSON(strings.Repeat("a", 40), true,
		strings.Repeat("b", 40), true)), &list); err != nil {
		t.Fatal(err)
	}
	list.Items[0].Status.Conditions = append(list.Items[0].Status.Conditions, list.Items[0].Status.Conditions[0])
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseFluxSourceSnapshot(string(raw)); err == nil || !strings.Contains(err.Error(), "duplicate Ready") {
		t.Fatalf("duplicate Kustomization Ready condition accepted: %v", err)
	}
}

func mutateGitRepositoryWireSpec(t *testing.T, item *gitRepositoryWire, mutate func(map[string]any)) {
	t.Helper()
	var spec map[string]any
	if err := json.Unmarshal(item.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	mutate(spec)
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	item.Spec = encoded
}
