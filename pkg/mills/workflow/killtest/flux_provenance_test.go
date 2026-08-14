package killtest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseFluxSourceSnapshotRejectsTerminatingKustomization(t *testing.T) {
	var list gitOpsKustomizationListWire
	if err := json.Unmarshal([]byte(fluxSourceListJSON(strings.Repeat("a", 40), true,
		strings.Repeat("b", 40), true)), &list); err != nil {
		t.Fatal(err)
	}
	deletedAt := "2026-07-14T12:00:00Z"
	list.Items[0].Metadata.DeletionTimestamp = &deletedAt
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseFluxSourceSnapshot(string(raw)); err == nil || !strings.Contains(err.Error(), "terminating") {
		t.Fatalf("terminating Kustomization accepted: %v", err)
	}
}

func TestValidateFluxSourceProvenanceSnapshotRejectsTerminatingObjects(t *testing.T) {
	tests := map[string]func(*FluxSourceProvenanceSnapshot){
		"Kustomization state": func(snapshot *FluxSourceProvenanceSnapshot) {
			snapshot.Sources[0].Terminating = true
		},
		"Kustomization timestamp": func(snapshot *FluxSourceProvenanceSnapshot) {
			snapshot.Sources[0].DeletionTimestamp = "2026-07-14T12:00:00Z"
		},
		"GitRepository state": func(snapshot *FluxSourceProvenanceSnapshot) {
			snapshot.GitRepositories.Repositories[0].Terminating = true
		},
		"GitRepository timestamp": func(snapshot *FluxSourceProvenanceSnapshot) {
			snapshot.GitRepositories.Repositories[0].DeletionTimestamp = "2026-07-14T12:00:00Z"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			snapshot := passingFluxFenceEvidence().Prepared
			mutate(&snapshot)
			if err := ValidateFluxSourceProvenanceSnapshot(snapshot); err == nil || !strings.Contains(err.Error(), "terminating") {
				t.Fatalf("terminating source accepted: %v", err)
			}
		})
	}
}

func TestValidateFluxSourceProvenanceSnapshotBindsFullObservationBracket(t *testing.T) {
	tests := map[string]func(*FluxSourceProvenanceSnapshot){
		"missing opening": func(snapshot *FluxSourceProvenanceSnapshot) {
			snapshot.GitRepositoriesOpenedAt = time.Time{}
		},
		"opening follows Kustomization": func(snapshot *FluxSourceProvenanceSnapshot) {
			snapshot.GitRepositoriesOpenedAt = snapshot.ObservedAt.Add(time.Nanosecond)
		},
		"closing predates Kustomization": func(snapshot *FluxSourceProvenanceSnapshot) {
			snapshot.GitRepositories.ObservedAt = snapshot.ObservedAt.Add(-time.Nanosecond)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			snapshot := passingFluxFenceEvidence().Prepared
			mutate(&snapshot)
			if err := ValidateFluxSourceProvenanceSnapshot(snapshot); err == nil {
				t.Fatal("invalid GitRepository/Kustomization observation bracket accepted")
			}
		})
	}
}

func TestValidateFluxSourceProvenanceRejectsOverlappingBrackets(t *testing.T) {
	fence := passingFluxFenceEvidence()
	fence.Final.GitRepositoriesOpenedAt = fence.Prepared.GitRepositories.ObservedAt.Add(-time.Nanosecond)
	if err := ValidateFluxSourceFenceEvidence(fence); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("overlapping prepared/final brackets accepted: %v", err)
	}

	report := PreflightReport{
		FluxSourcesStart: passingFluxFenceEvidence().Prepared,
		FluxSourcesEnd:   passingFluxFenceEvidence().Final,
	}
	report.FluxSourcesEnd.GitRepositoriesOpenedAt =
		report.FluxSourcesStart.GitRepositories.ObservedAt.Add(-time.Nanosecond)
	if err := ValidatePreflightFluxProvenance(report); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("overlapping preflight brackets accepted: %v", err)
	}
}

func TestValidateFluxSourceProvenanceSnapshotRejectsMissingOwner(t *testing.T) {
	snapshot := passingFluxFenceEvidence().Prepared
	snapshot.Sources = snapshot.Sources[:len(snapshot.Sources)-1]
	if err := ValidateFluxSourceProvenanceSnapshot(snapshot); err == nil || !strings.Contains(err.Error(), "owners") {
		t.Fatalf("missing owner accepted: %v", err)
	}
}

func TestValidateFluxSourceProvenanceSnapshotRejectsStaleGeneration(t *testing.T) {
	snapshot := passingFluxFenceEvidence().Prepared
	snapshot.Sources[0].ReadyObservedGeneration--
	if err := ValidateFluxSourceProvenanceSnapshot(snapshot); err == nil || !strings.Contains(err.Error(), "stale Ready generation") {
		t.Fatalf("stale Ready generation accepted: %v", err)
	}
}

func TestValidateFluxSourceProvenanceSnapshotRejectsUnboundRenderSpec(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*FluxSourceProvenanceSnapshot)
		want   string
	}{
		{
			name: "live digest differs from reviewed",
			mutate: func(snapshot *FluxSourceProvenanceSnapshot) {
				snapshot.Sources[0].RenderSpec.SpecSHA256 = strings.Repeat("f", 64)
			},
			want: "live/reviewed render spec SHA-256 mismatch",
		},
		{
			name: "reviewed revision differs from platform baseline",
			mutate: func(snapshot *FluxSourceProvenanceSnapshot) {
				snapshot.Sources[3].RenderSpec.ReviewedRevision = strings.Repeat("c", 40)
			},
			want: "not bound to the platform review baseline",
		},
		{
			name: "manifest path redirected",
			mutate: func(snapshot *FluxSourceProvenanceSnapshot) {
				snapshot.Sources[1].RenderSpec.ManifestPath = "clusters/k3s/other.yaml"
			},
			want: "serialized render spec is redirected",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := passingFluxFenceEvidence().Prepared
			test.mutate(&snapshot)
			if err := ValidateFluxSourceProvenanceSnapshot(snapshot); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unbound render spec accepted: %v", err)
			}
		})
	}
}

func TestValidateFluxSourceFenceEvidenceRejectsUIDAndResourceVersionABA(t *testing.T) {
	for _, field := range []string{"uid", "resourceVersion"} {
		t.Run(field, func(t *testing.T) {
			fence := passingFluxFenceEvidence()
			if field == "uid" {
				fence.Final.Sources[0].UID = "apps-recreated-uid"
			} else {
				// A change-and-revert can restore the same applied revision and
				// generation, but cannot restore Kubernetes resourceVersion.
				fence.Final.Sources[0].ResourceVersion = "999"
			}
			if err := ValidateFluxSourceFenceEvidence(fence); err == nil || !strings.Contains(err.Error(), "prepared/final provenance mismatch") {
				t.Fatalf("%s ABA accepted: %v", field, err)
			}
		})
	}
}

func TestValidateFluxSourceFenceEvidenceRejectsPreparedFinalRevisionMismatch(t *testing.T) {
	fence := passingFluxFenceEvidence()
	revision := strings.Repeat("c", 40)
	for i := range fence.Final.Sources {
		fence.Final.Sources[i].RenderSpec.ReviewedRevision = revision
		fence.Final.Sources[i].RenderSpec.ReviewedScopeDigest = revision
		if i == len(fence.Final.Sources)-1 {
			continue
		}
		identity := fence.Final.Sources[i].ProtectedIdentity
		identity.BaselineRevision = revision
		identity.ObservedRevision = revision
		identity.BaselineDigest = revision
		identity.ObservedDigest = revision
		fence.Final.Sources[i].AppliedRevision = "main@sha1:" + revision
		fence.Final.Sources[i].AttemptedRevision = "main@sha1:" + revision
		fence.Final.Sources[i].ProtectedIdentity = identity
	}
	for i := range fence.Final.GitRepositories.Repositories {
		repository := &fence.Final.GitRepositories.Repositories[i]
		repository.Spec.ReviewedRevision = revision
		repository.Spec.ReviewedScopeDigest = revision
		if repository.Name == "gitops-gitlab" {
			repository.ArtifactRevision = "main@sha1:" + revision
			repository.ProtectedIdentity.BaselineRevision = revision
			repository.ProtectedIdentity.ObservedRevision = revision
			repository.ProtectedIdentity.BaselineDigest = revision
			repository.ProtectedIdentity.ObservedDigest = revision
		}
	}
	if err := ValidateFluxSourceFenceEvidence(fence); err == nil || !strings.Contains(err.Error(), "prepared/final provenance mismatch") {
		t.Fatalf("prepared/final revision mismatch accepted: %v", err)
	}
}

func TestValidateFluxSourceProvenanceSnapshotRejectsHandEditedRevisionAndDigest(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*FluxSourceProvenance)
		want   string
	}{
		{
			name: "arbitrary revision",
			mutate: func(source *FluxSourceProvenance) {
				source.ProtectedIdentity.ObservedRevision = "hand-edited"
			},
			want: "normalized observed revision",
		},
		{
			name: "exact arbitrary digest",
			mutate: func(source *FluxSourceProvenance) {
				source.ProtectedIdentity.ObservedDigest = "same-looking-label"
			},
			want: "exact-revision identity drifted",
		},
		{
			name: "protected arbitrary digest",
			mutate: func(source *FluxSourceProvenance) {
				source.ProtectedIdentity.Mode = GitOpsIdentityModeProtectedScope
				source.ProtectedIdentity.BaselineDigest = "same-label"
				source.ProtectedIdentity.ObservedDigest = "same-label"
			},
			want: "not a normalized SHA-256",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := passingFluxFenceEvidence().Prepared
			test.mutate(&snapshot.Sources[0])
			if err := ValidateFluxSourceProvenanceSnapshot(snapshot); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("hand-edited provenance accepted: %v", err)
			}
		})
	}
}

func TestEvaluateFailsClosedOnSerializedFluxProvenanceDrift(t *testing.T) {
	for _, mutate := range []struct {
		name string
		fn   func(*Evidence)
	}{
		{
			name: "missing owner",
			fn: func(ev *Evidence) {
				ev.CrashAFluxProvenance.Prepared.Sources = ev.CrashAFluxProvenance.Prepared.Sources[:3]
			},
		},
		{
			name: "change and revert",
			fn: func(ev *Evidence) {
				ev.CrashBFluxProvenance.Final.Sources[2].ResourceVersion = "999"
			},
		},
		{
			name: "protected digest drift",
			fn: func(ev *Evidence) {
				ev.CrashAFluxProvenance.Prepared.Sources[1].ProtectedIdentity.Mode = GitOpsIdentityModeProtectedScope
				ev.CrashAFluxProvenance.Prepared.Sources[1].ProtectedIdentity.ObservedDigest = "changed"
			},
		},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			ev := passingEvidence()
			mutate.fn(&ev)
			verdict := Evaluate(ev)
			if verdict.Pass1NoDoubleSpawn || verdict.Overall || !strings.Contains(verdict.Pass1Reason, "Flux") {
				t.Fatalf("invalid serialized provenance passed: %+v", verdict)
			}
		})
	}
}

func TestValidatePreflightFluxProvenanceAllowsProvenDescendantAndRejectsUIDReplacement(t *testing.T) {
	start := passingFluxFenceEvidence().Prepared
	end := passingFluxFenceEvidence().Final
	baselineRevision := strings.Repeat("0", 40)
	baselineDigest := strings.Repeat("e", 64)
	for i := range start.Sources {
		start.Sources[i].RenderSpec.ReviewedRevision = baselineRevision
		start.Sources[i].RenderSpec.ReviewedScopeDigest = baselineDigest
		end.Sources[i].RenderSpec.ReviewedRevision = baselineRevision
		end.Sources[i].RenderSpec.ReviewedScopeDigest = baselineDigest
	}
	for i := 0; i < 3; i++ {
		identity := start.Sources[i].ProtectedIdentity
		identity.Mode = GitOpsIdentityModeProtectedScope
		identity.BaselineRevision = baselineRevision
		identity.BaselineDigest = baselineDigest
		identity.ObservedDigest = baselineDigest
		start.Sources[i].ProtectedIdentity = identity
		endIdentity := identity
		endIdentity.ObservedRevision = strings.Repeat("d", 40)
		endIdentity.CheckedCommitCount++
		end.Sources[i].AppliedRevision = "main@sha1:" + endIdentity.ObservedRevision
		end.Sources[i].AttemptedRevision = end.Sources[i].AppliedRevision
		end.Sources[i].ProtectedIdentity = endIdentity
		end.Sources[i].ResourceVersion = "8" + end.Sources[i].ResourceVersion
	}
	startRepositories := gitRepositoryProvenanceByName(start.GitRepositories)
	endRepositories := gitRepositoryProvenanceByName(end.GitRepositories)
	startPlatform := startRepositories["gitops-gitlab"]
	startPlatform.Spec.ReviewedRevision = baselineRevision
	startPlatform.Spec.ReviewedScopeDigest = baselineDigest
	startPlatform.ProtectedIdentity = start.Sources[0].ProtectedIdentity
	endPlatform := endRepositories["gitops-gitlab"]
	endPlatform.Spec.ReviewedRevision = baselineRevision
	endPlatform.Spec.ReviewedScopeDigest = baselineDigest
	endPlatform.ArtifactRevision = end.Sources[0].AppliedRevision
	endPlatform.ArtifactDigest = "sha256:" + strings.Repeat("f", 64)
	endPlatform.ProtectedIdentity = end.Sources[0].ProtectedIdentity
	endPlatform.ResourceVersion = "8501"
	startLoomCore := startRepositories["loom-core"]
	startLoomCore.Spec.ReviewedRevision = baselineRevision
	startLoomCore.Spec.ReviewedScopeDigest = baselineDigest
	endLoomCore := endRepositories["loom-core"]
	endLoomCore.Spec.ReviewedRevision = baselineRevision
	endLoomCore.Spec.ReviewedScopeDigest = baselineDigest
	start.GitRepositories.Repositories = []GitRepositoryProvenance{
		startPlatform, startLoomCore,
	}
	end.GitRepositories.Repositories = []GitRepositoryProvenance{
		endPlatform, endLoomCore,
	}
	report := PreflightReport{FluxSourcesStart: start, FluxSourcesEnd: end}
	if err := ValidatePreflightFluxProvenance(report); err != nil {
		t.Fatalf("proven protected-scope descendants rejected: %v", err)
	}
	sameRevision := report.FluxSourcesEnd.GitRepositories
	sameRevision.Repositories = append([]GitRepositoryProvenance(nil), sameRevision.Repositories...)
	sameRevision.Repositories[0].ArtifactDigest = "sha256:" + strings.Repeat("9", 64)
	if err := sameGitRepositoryGateIdentity(report.FluxSourcesEnd.GitRepositories, sameRevision); err == nil ||
		!strings.Contains(err.Error(), "artifact digest changed without a revision change") {
		t.Fatalf("same-revision GitRepository artifact digest drift accepted: %v", err)
	}
	report.FluxSourcesEnd.Sources[1].UID = "bootstrap-recreated"
	if err := ValidatePreflightFluxProvenance(report); err == nil || !strings.Contains(err.Error(), "UID changed") {
		t.Fatalf("preflight object replacement accepted: %v", err)
	}
}

func TestValidatePreflightFluxProvenanceRejectsGenerationChange(t *testing.T) {
	report := PreflightReport{
		FluxSourcesStart: passingFluxFenceEvidence().Prepared,
		FluxSourcesEnd:   passingFluxFenceEvidence().Final,
	}
	report.FluxSourcesEnd.Sources[1].Generation++
	report.FluxSourcesEnd.Sources[1].ReadyObservedGeneration++
	if err := ValidatePreflightFluxProvenance(report); err == nil || !strings.Contains(err.Error(), "generation changed") {
		t.Fatalf("preflight Kustomization spec generation drift accepted: %v", err)
	}
}
