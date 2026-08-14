package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/workflow/killtest"
)

const verifierGateID = "0123456789abcdef0123456789abcdef"

type verifierFixture struct {
	summaryPath string
	summary     gateSummary
	outputs     [3]runOutput
}

func TestVerifyGateEvidenceAcceptsCanonicalProtectedScopeGate(t *testing.T) {
	fixture := newVerifierFixture(t)
	fixture.write(t)

	if err := verifyGateEvidenceWithEvaluator(fixture.summaryPath, verifierTestEvaluate); err != nil {
		t.Fatalf("verifyGateEvidenceWithEvaluator() error = %v", err)
	}
}

func TestVerifyGateEvidenceRejectsInterRunSingletonRestart(t *testing.T) {
	fixture := newVerifierFixture(t)
	fixture.outputs[1].Evidence.InitialPreflight.Operator.UID = "unexpected-between-run-restart"
	fixture.outputs[1].Evidence.InitialPreflight.AuthorityPlane.Operator.PodUID = "unexpected-between-run-restart"
	fixture.outputs[1].Evidence.InitialPreflight.EffectivePolicyAuthority.PodUID = "unexpected-between-run-restart"
	fixture.outputs[1].Evidence.InitialPreflight.Quiescence.OperatorAuthority.PodUID = "unexpected-between-run-restart"
	fixture.outputs[1].Preflight = fixture.outputs[1].Evidence.InitialPreflight
	fixture.write(t)

	err := verifyGateEvidenceWithEvaluator(fixture.summaryPath, verifierTestEvaluate)
	if err == nil || !strings.Contains(err.Error(), "inter-run pod continuity") {
		t.Fatalf("verifyGateEvidenceWithEvaluator() error = %v, want inter-run continuity rejection", err)
	}
}

func TestVerifyGateEvidenceAllowsProtectedGitRepositoryDescendant(t *testing.T) {
	fixture := newVerifierFixture(t)
	newRevision := strings.Repeat("d", 40)
	for _, report := range []*killtest.PreflightReport{
		&fixture.outputs[1].Evidence.InitialPreflight,
		&fixture.outputs[1].Evidence.CrashASafety.ImmediatePreflight,
		&fixture.outputs[1].Evidence.CrashBSafety.ImmediatePreflight,
		&fixture.outputs[1].Evidence.FinalPreflight,
	} {
		advanceVerifierPlatformSource(report, newRevision, strings.Repeat("e", 64))
	}
	fixture.outputs[1].Preflight = fixture.outputs[1].Evidence.InitialPreflight
	final := fixture.outputs[1].Evidence.FinalPreflight
	fixture.outputs[1].FinalPreflight = &final
	fixture.write(t)

	if err := verifyGateEvidenceWithEvaluator(fixture.summaryPath, verifierTestEvaluate); err != nil {
		t.Fatalf("protected GitRepository descendant rejected: %v", err)
	}
}

func TestVerifyGateEvidenceRejectsArtifactDigestChangeWithoutRevisionChange(t *testing.T) {
	fixture := newVerifierFixture(t)
	for _, report := range []*killtest.PreflightReport{
		&fixture.outputs[1].Evidence.InitialPreflight,
		&fixture.outputs[1].Evidence.CrashASafety.ImmediatePreflight,
		&fixture.outputs[1].Evidence.CrashBSafety.ImmediatePreflight,
		&fixture.outputs[1].Evidence.FinalPreflight,
	} {
		for _, snapshot := range []*killtest.FluxSourceProvenanceSnapshot{
			&report.FluxSourcesStart, &report.FluxSourcesEnd,
		} {
			snapshot.GitRepositories.Repositories[0].ArtifactDigest =
				"sha256:" + strings.Repeat("9", 64)
		}
	}
	fixture.outputs[1].Preflight = fixture.outputs[1].Evidence.InitialPreflight
	final := fixture.outputs[1].Evidence.FinalPreflight
	fixture.outputs[1].FinalPreflight = &final
	fixture.write(t)

	err := verifyGateEvidenceWithEvaluator(fixture.summaryPath, verifierTestEvaluate)
	if err == nil || !strings.Contains(err.Error(), "artifact digest changed without a revision change") {
		t.Fatalf("same-revision GitRepository artifact digest drift accepted: %v", err)
	}
}

func TestVerifyGateEvidenceOpensCanonicalPathInsteadOfTraversalSpelling(t *testing.T) {
	fixture := newVerifierFixture(t)
	dir := filepath.Dir(fixture.summaryPath)
	attackerDir := filepath.Join(dir, "attacker", "nested")
	if err := os.MkdirAll(attackerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(attackerDir, filepath.Join(dir, "redirect")); err != nil {
		t.Fatal(err)
	}
	fixture.summary.Runs[0].EvidencePath = filepath.Join(dir, "redirect") +
		string(os.PathSeparator) + ".." + string(os.PathSeparator) +
		filepath.Base(runEvidencePath(fixture.summaryPath, 1, 3, true))
	fixture.write(t)
	attackerPath := filepath.Join(dir, "attacker", filepath.Base(runEvidencePath(fixture.summaryPath, 1, 3, true)))
	if err := os.WriteFile(attackerPath, []byte(`{"forged":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := verifyGateEvidenceWithEvaluator(fixture.summaryPath, verifierTestEvaluate); err != nil {
		t.Fatalf("canonical evidence read was redirected: %v", err)
	}
}

func TestVerifyGateEvidenceAcceptsGeneratedRedundantRelativeSegment(t *testing.T) {
	fixture := newVerifierFixture(t)
	fixture.summaryPath = filepath.Dir(fixture.summaryPath) + string(os.PathSeparator) + "." +
		string(os.PathSeparator) + filepath.Base(fixture.summaryPath)
	for i := range fixture.summary.Runs {
		fixture.summary.Runs[i].EvidencePath = runEvidencePath(fixture.summaryPath, i+1, 3, true)
	}
	fixture.write(t)

	if err := verifyGateEvidenceWithEvaluator(fixture.summaryPath, verifierTestEvaluate); err != nil {
		t.Fatalf("generated relative path spelling rejected: %v", err)
	}
}

func TestVerifyGateEvidenceRejectsUntrustedArtifacts(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*testing.T, *verifierFixture)
		afterWrite func(*testing.T, *verifierFixture)
		want       string
	}{
		{
			name: "partial summary",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.summary.CompletedRuns = 2
				fixture.summary.Runs = fixture.summary.Runs[:2]
			},
			want: "not a completed 3-run gate",
		},
		{
			name: "unsupported gate contract",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.summary.GateContractVersion++
			},
			want: "unsupported contract",
		},
		{
			name: "unsupported Flux provenance contract",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[0].Evidence.InitialPreflight.FluxSourcesStart.ContractVersion--
				fixture.outputs[0].Preflight = fixture.outputs[0].Evidence.InitialPreflight
			},
			want: "unsupported Flux provenance contract",
		},
		{
			name: "terminating Kustomization evidence",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[0].Evidence.InitialPreflight.FluxSourcesStart.Sources[0].Terminating = true
				fixture.outputs[0].Preflight = fixture.outputs[0].Evidence.InitialPreflight
			},
			want: "terminating",
		},
		{
			name: "terminating GitRepository evidence",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[0].Evidence.InitialPreflight.FluxSourcesStart.
					GitRepositories.Repositories[0].DeletionTimestamp = "2026-07-14T12:00:00Z"
				fixture.outputs[0].Preflight = fixture.outputs[0].Evidence.InitialPreflight
			},
			want: "terminating",
		},
		{
			name: "terminating policy ConfigMap evidence",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[0].Evidence.InitialPreflight.PolicyConfigMapIdentity.Terminating = true
				fixture.outputs[0].Evidence.InitialPreflight.PolicyConfigMapIdentity.DeletionTimestamp = "2026-07-14T12:00:00Z"
				fixture.outputs[0].Preflight = fixture.outputs[0].Evidence.InitialPreflight
			},
			want: "terminating",
		},
		{
			name: "missing policy ConfigMap review",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[0].Evidence.InitialPreflight.PolicyConfigMapReview =
					killtest.PolicyConfigMapReviewIdentity{}
				fixture.outputs[0].Preflight = fixture.outputs[0].Evidence.InitialPreflight
			},
			want: "policy ConfigMap provenance",
		},
		{
			name: "live policy payload differs from reviewed render",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[0].Evidence.InitialPreflight.PolicyConfigMapReview.LivePayloadSHA256 =
					strings.Repeat("6", 64)
				fixture.outputs[0].Preflight = fixture.outputs[0].Evidence.InitialPreflight
			},
			want: "reviewed/live policy payload",
		},
		{
			name: "policy source SHA differs from live Deployment",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[0].Evidence.InitialPreflight.PolicyConfigMapReview.SourceSHA256 =
					strings.Repeat("6", 64)
				fixture.outputs[0].Preflight = fixture.outputs[0].Evidence.InitialPreflight
			},
			want: "exact policy source SHA-256",
		},
		{
			name: "policy review Flux full spec mismatch",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[0].Evidence.InitialPreflight.PolicyConfigMapReview.FluxSpecSHA256 =
					strings.Repeat("6", 64)
				fixture.outputs[0].Preflight = fixture.outputs[0].Evidence.InitialPreflight
			},
			want: "apps Flux full spec",
		},
		{
			name: "missing spawn ConfigMap resourceVersion",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[0].Evidence.InitialPreflight.SpawnConfigMapIdentity.ResourceVersion = ""
				fixture.outputs[0].Preflight = fixture.outputs[0].Evidence.InitialPreflight
			},
			want: "resourceVersion",
		},
		{
			name: "policy ConfigMap changes within run",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[0].Evidence.CrashASafety.ImmediatePreflight.PolicyConfigMapIdentity.ResourceVersion = "changed"
			},
			want: "policy ConfigMap identity changed",
		},
		{
			name: "policy ConfigMap payload review changes within run",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				review := &fixture.outputs[0].Evidence.CrashASafety.ImmediatePreflight.PolicyConfigMapReview
				review.RenderedPayloadSHA256 = strings.Repeat("6", 64)
				review.LivePayloadSHA256 = strings.Repeat("6", 64)
			},
			want: "policy ConfigMap review identity changed",
		},
		{
			name: "missing policy delete-boundary evidence",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[0].Evidence.CrashASafety.PolicyDeleteBoundary =
					killtest.PolicyDeleteBoundaryEvidence{}
			},
			want: "unsupported policy delete-boundary contract",
		},
		{
			name: "policy delete-boundary ConfigMap A replacement",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[0].Evidence.CrashASafety.PolicyDeleteBoundary.ConfigMapA.Identity.ResourceVersion = "forged"
			},
			want: "ConfigMap A identity differs from immediate preflight",
		},
		{
			name: "policy delete-boundary ConfigMap B payload drift",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[0].Evidence.CrashASafety.PolicyDeleteBoundary.ConfigMapB.PayloadSHA256 =
					strings.Repeat("6", 64)
			},
			want: "ConfigMap B complete payload differs",
		},
		{
			name: "policy delete-boundary effective policy opens",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[0].Evidence.CrashASafety.PolicyDeleteBoundary.Effective.PolicyEnabled = true
			},
			want: "effective policy differs from immediate preflight",
		},
		{
			name: "policy delete-boundary operator checksum drift",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[0].Evidence.CrashASafety.PolicyDeleteBoundary.OperatorDeployment.PolicyChecksum =
					strings.Repeat("6", 64)
			},
			want: "live policy-bearing operator Deployment differs",
		},
		{
			name: "policy delete-boundary operator spec drift",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[0].Evidence.CrashASafety.PolicyDeleteBoundary.OperatorDeployment.SpecSHA256 =
					strings.Repeat("6", 64)
			},
			want: "live policy-bearing operator Deployment differs",
		},
		{
			name: "stale policy delete-boundary ConfigMap B",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				boundary := &fixture.outputs[0].Evidence.CrashASafety.PolicyDeleteBoundary
				fixture.outputs[0].Evidence.CrashASafety.DeleteRequestedAt =
					boundary.ConfigMapB.ObservedAt.Add(10*time.Second + time.Nanosecond)
			},
			want: "policy ConfigMap B proof",
		},
		{
			name: "policy delete-boundary completes at DELETE",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				safety := &fixture.outputs[0].Evidence.CrashASafety
				safety.PolicyDeleteBoundary.CompletedAt = safety.DeleteRequestedAt
			},
			want: "not before DELETE",
		},
		{
			name: "invalid gate id",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.summary.GateID = strings.Repeat("G", 32)
			},
			want: "gate summary identity",
		},
		{
			name: "missing gate binding",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[1].Evidence.GateBinding = killtest.GateBinding{}
			},
			want: "has no S1c gate binding",
		},
		{
			name: "mixed gate history",
			mutate: func(t *testing.T, fixture *verifierFixture) {
				otherGateID := strings.Repeat("f", 32)
				runID, err := killtest.CanaryRunIDForGate(otherGateID, 2)
				if err != nil {
					t.Fatal(err)
				}
				fixture.outputs[1].Evidence.GateBinding.GateID = otherGateID
				fixture.outputs[1].Evidence.RunID = runID
				fixture.outputs[1].Evidence.Final.Run.ID = runID
				fixture.summary.Runs[1].RunID = runID
			},
			want: "gate binding differs from summary",
		},
		{
			name: "duplicate run id",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[1].Evidence.RunID = fixture.outputs[0].Evidence.RunID
				fixture.outputs[1].Evidence.Final.Run.ID = fixture.outputs[0].Evidence.RunID
				fixture.summary.Runs[1].RunID = fixture.outputs[0].Evidence.RunID
			},
			want: "differs from canonical",
		},
		{
			name: "duplicate spawn id",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[1].Evidence.SpawnID = fixture.outputs[0].Evidence.SpawnID
			},
			want: "reuses spawn_id",
		},
		{
			name: "tampered verdict",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[1].Verdicts.Pass1Reason = "hand-edited"
			},
			want: "serialized verdicts differ",
		},
		{
			name: "tampered evidence",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[1].Evidence.MaxConcurrentSpawnPods = 2
			},
			want: "serialized verdicts differ",
		},
		{
			name: "missing durable delete receipt",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[1].Evidence.CrashASafety.DeleteAcceptedAt = time.Time{}
			},
			want: "serialized verdicts differ",
		},
		{
			name: "missing process authorization",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[1].Evidence.CrashBProcessAuthorization = killtest.ProcessDeleteAuthorization{}
			},
			want: "serialized verdicts differ",
		},
		{
			name: "unknown run field",
			afterWrite: func(t *testing.T, fixture *verifierFixture) {
				path := runEvidencePath(fixture.summaryPath, 2, 3, true)
				blob, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				blob = bytes.Replace(blob, []byte("{"), []byte(`{"unknown_verifier_field":true,`), 1)
				if err := os.WriteFile(path, blob, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "unknown field",
		},
		{
			name: "tampered file bytes",
			afterWrite: func(t *testing.T, fixture *verifierFixture) {
				path := runEvidencePath(fixture.summaryPath, 2, killtest.S1cGateRequiredRuns, true)
				blob, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(blob, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "differs from actual file",
		},
		{
			name: "forged summary evidence hash",
			afterWrite: func(t *testing.T, fixture *verifierFixture) {
				fixture.summary.Runs[1].EvidenceSHA256 = strings.Repeat("f", 64)
				fixture.writeSummary(t)
			},
			want: "differs from actual file",
		},
		{
			name: "summary predecessor hash mismatch",
			afterWrite: func(t *testing.T, fixture *verifierFixture) {
				fixture.summary.Runs[1].PreviousEvidenceSHA256 = strings.Repeat("f", 64)
				fixture.writeSummary(t)
			},
			want: "predecessor evidence SHA-256 mismatch",
		},
		{
			name: "coherently rewritten wrong predecessor",
			afterWrite: func(t *testing.T, fixture *verifierFixture) {
				fixture.outputs[1].Evidence.GateBinding.PreviousEvidenceSHA256 = strings.Repeat("f", 64)
				fixture.summary.Runs[1].PreviousEvidenceSHA256 = strings.Repeat("f", 64)
				fixture.rewriteFrom(t, 2)
			},
			want: "predecessor evidence SHA-256 mismatch",
		},
		{
			name: "symlink evidence file",
			afterWrite: func(t *testing.T, fixture *verifierFixture) {
				path := runEvidencePath(fixture.summaryPath, 1, 3, true)
				target := filepath.Join(filepath.Dir(path), "redirected-run.json")
				blob, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, blob, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
			want: "not a regular non-symlink file",
		},
		{
			name: "symlink summary file",
			afterWrite: func(t *testing.T, fixture *verifierFixture) {
				target := filepath.Join(filepath.Dir(fixture.summaryPath), "redirected-summary.json")
				blob, err := os.ReadFile(fixture.summaryPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, blob, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(fixture.summaryPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, fixture.summaryPath); err != nil {
					t.Fatal(err)
				}
			},
			want: "not a regular non-symlink file",
		},
		{
			name: "exact revision gate",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.summary.GitOpsIdentityMode = killtest.GitOpsIdentityModeExactRevision
			},
			want: "S1c requires \"protected-scope\"",
		},
		{
			name: "overlapping run windows",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				previousEnd := fixture.outputs[0].Evidence.FinalPreflight.FluxSourcesEnd.GitRepositories.ObservedAt
				fixture.outputs[1].Evidence.InitialPreflight.FluxSourcesStart.GitRepositoriesOpenedAt = previousEnd
				fixture.outputs[1].Preflight = fixture.outputs[1].Evidence.InitialPreflight
			},
			want: "does not start strictly after",
		},
		{
			name: "gate starts after first run",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				gateStartedAt := fixture.outputs[0].Evidence.InitialPreflight.FluxSourcesStart.
					GitRepositoriesOpenedAt.Add(time.Second)
				fixture.summary.GateStartedAt = gateStartedAt
				for i := range fixture.outputs {
					fixture.outputs[i].Evidence.GateBinding.GateStartedAt = gateStartedAt
				}
			},
			want: "predates gate start",
		},
		{
			name: "stale gate allocation",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				gateStartedAt := fixture.outputs[0].Evidence.InitialPreflight.FluxSourcesStart.GitRepositoriesOpenedAt.
					Add(-maxConsecutiveGateRunGap - time.Nanosecond)
				fixture.summary.GateStartedAt = gateStartedAt
				for i := range fixture.outputs {
					fixture.outputs[i].Evidence.GateBinding.GateStartedAt = gateStartedAt
				}
			},
			want: "exceeds gate-start limit",
		},
		{
			name: "historical run gap",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				previousEnd := fixture.outputs[0].Evidence.FinalPreflight.FluxSourcesEnd.GitRepositories.ObservedAt
				start := previousEnd.Add(maxConsecutiveGateRunGap + time.Nanosecond)
				initial := &fixture.outputs[1].Evidence.InitialPreflight
				initial.FluxSourcesStart.GitRepositoriesOpenedAt = start
				initial.FluxSourcesStart.ObservedAt = start
				initial.FluxSourcesStart.GitRepositories.ObservedAt = start
				initial.FluxSourcesEnd.GitRepositoriesOpenedAt = start.Add(time.Nanosecond)
				initial.FluxSourcesEnd.ObservedAt = start.Add(time.Second)
				initial.FluxSourcesEnd.GitRepositories.ObservedAt = start.Add(time.Second)
				initial.Quiescence.ObservedAt = start.Add(500 * time.Millisecond)
				finalReport := &fixture.outputs[1].Evidence.FinalPreflight
				finalReport.FluxSourcesEnd.GitRepositoriesOpenedAt = start.Add(8 * time.Second)
				finalReport.FluxSourcesEnd.ObservedAt = start.Add(9 * time.Second)
				finalReport.FluxSourcesEnd.GitRepositories.ObservedAt = start.Add(9 * time.Second)
				fixture.outputs[1].Preflight = fixture.outputs[1].Evidence.InitialPreflight
				final := fixture.outputs[1].Evidence.FinalPreflight
				fixture.outputs[1].FinalPreflight = &final
			},
			want: "exceeds consecutive-gate limit",
		},
		{
			name: "stable identity drift",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				for _, report := range []*killtest.PreflightReport{
					&fixture.outputs[1].Evidence.InitialPreflight,
					&fixture.outputs[1].Evidence.CrashASafety.ImmediatePreflight,
					&fixture.outputs[1].Evidence.CrashBSafety.ImmediatePreflight,
					&fixture.outputs[1].Evidence.FinalPreflight,
				} {
					report.OperatorDeployment.Strategy = "RollingUpdate"
				}
				fixture.outputs[1].Preflight = fixture.outputs[1].Evidence.InitialPreflight
				final := fixture.outputs[1].Evidence.FinalPreflight
				fixture.outputs[1].FinalPreflight = &final
			},
			want: "operator deployment is not a fully observed stable singleton",
		},
		{
			name: "render spec drift",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				for _, report := range []*killtest.PreflightReport{
					&fixture.outputs[1].Evidence.InitialPreflight,
					&fixture.outputs[1].Evidence.CrashASafety.ImmediatePreflight,
					&fixture.outputs[1].Evidence.CrashBSafety.ImmediatePreflight,
					&fixture.outputs[1].Evidence.FinalPreflight,
				} {
					setVerifierRenderSpecDigest(report, strings.Repeat("4", 64))
				}
				fixture.outputs[1].Preflight = fixture.outputs[1].Evidence.InitialPreflight
				final := fixture.outputs[1].Evidence.FinalPreflight
				fixture.outputs[1].FinalPreflight = &final
			},
			want: "policy review is not bound to the apps Flux full spec",
		},
		{
			name: "GitRepository UID drift",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				for _, report := range []*killtest.PreflightReport{
					&fixture.outputs[1].Evidence.InitialPreflight,
					&fixture.outputs[1].Evidence.CrashASafety.ImmediatePreflight,
					&fixture.outputs[1].Evidence.CrashBSafety.ImmediatePreflight,
					&fixture.outputs[1].Evidence.FinalPreflight,
				} {
					report.FluxSourcesStart.GitRepositories.Repositories[0].UID = "recreated-repo-uid"
					report.FluxSourcesEnd.GitRepositories.Repositories[0].UID = "recreated-repo-uid"
				}
				fixture.outputs[1].Preflight = fixture.outputs[1].Evidence.InitialPreflight
				final := fixture.outputs[1].Evidence.FinalPreflight
				fixture.outputs[1].FinalPreflight = &final
			},
			want: "GitRepository gitops-gitlab stable gate object/spec identity changed",
		},
		{
			name: "GitRepository generation drift",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				for _, report := range []*killtest.PreflightReport{
					&fixture.outputs[1].Evidence.InitialPreflight,
					&fixture.outputs[1].Evidence.CrashASafety.ImmediatePreflight,
					&fixture.outputs[1].Evidence.CrashBSafety.ImmediatePreflight,
					&fixture.outputs[1].Evidence.FinalPreflight,
				} {
					for _, snapshot := range []*killtest.FluxSourceProvenanceSnapshot{
						&report.FluxSourcesStart, &report.FluxSourcesEnd,
					} {
						repository := &snapshot.GitRepositories.Repositories[0]
						repository.Generation++
						repository.StatusObservedGeneration++
						repository.ReadyObservedGeneration++
						repository.ArtifactInStorageObservedGeneration++
					}
				}
				fixture.outputs[1].Preflight = fixture.outputs[1].Evidence.InitialPreflight
				final := fixture.outputs[1].Evidence.FinalPreflight
				fixture.outputs[1].FinalPreflight = &final
			},
			want: "GitRepository gitops-gitlab stable gate object/spec identity changed",
		},
		{
			name: "platform owner baseline mismatch",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.outputs[1].Evidence.InitialPreflight.GitOpsBootstrapIdentity.BaselineDigest = strings.Repeat("5", 64)
				fixture.outputs[1].Preflight = fixture.outputs[1].Evidence.InitialPreflight
			},
			want: "bootstrap end protected identity differs from apps platform baseline",
		},
		{
			name: "summary identity tamper",
			mutate: func(_ *testing.T, fixture *verifierFixture) {
				fixture.summary.PolicyChecksum = "hand-edited"
			},
			want: "summary immutable identity",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newVerifierFixture(t)
			if test.mutate != nil {
				test.mutate(t, fixture)
			}
			fixture.write(t)
			if test.afterWrite != nil {
				test.afterWrite(t, fixture)
			}
			err := verifyGateEvidenceWithEvaluator(fixture.summaryPath, verifierTestEvaluate)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("verifyGateEvidenceWithEvaluator() error = %v, want %q", err, test.want)
			}
		})
	}
}

func newVerifierFixture(t *testing.T) *verifierFixture {
	t.Helper()
	fixture := &verifierFixture{summaryPath: filepath.Join(t.TempDir(), "s1c-evidence.json")}
	base := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	gateStartedAt := base.Add(-time.Second)
	fixture.summary.GateContract = killtest.GateBindingContract
	fixture.summary.GateContractVersion = killtest.GateBindingContractVersion
	fixture.summary.GateID = verifierGateID
	fixture.summary.GateStartedAt = gateStartedAt
	for i := range fixture.outputs {
		index := i + 1
		fixture.outputs[i] = verifierRunOutput(t, index, base.Add(time.Duration(i)*time.Minute), gateStartedAt)
		fixture.summary.Runs = append(fixture.summary.Runs, gateRunSummary{
			Index: index, EvidencePath: runEvidencePath(fixture.summaryPath, index, killtest.S1cGateRequiredRuns, true),
			RunID: fixture.outputs[i].Evidence.RunID, AgentType: killtest.AgentTypeClaudeCode,
			FinalState: "done", Overall: true,
		})
	}
	baseline := fixture.outputs[0].Evidence.InitialPreflight
	fixture.summary.RequiredRuns = killtest.S1cGateRequiredRuns
	fixture.summary.CompletedRuns = killtest.S1cGateRequiredRuns
	fixture.summary.Overall = true
	fixture.summary.AgentType = killtest.AgentTypeClaudeCode
	fixture.summary.OperatorImage = baseline.Operator.ImageID
	fixture.summary.HudImage = baseline.Hud.ImageID
	fixture.summary.PolicyChecksum = baseline.PolicyChecksum
	fixture.summary.GitOpsIdentityMode = baseline.GitOpsIdentity.Mode
	fixture.summary.GitOpsBaseline = baseline.GitOpsIdentity.BaselineRevision
	fixture.summary.GitOpsScopeDigest = baseline.GitOpsIdentity.ObservedDigest
	fixture.summary.LoomCoreBaseline = baseline.LoomCoreIdentity.BaselineRevision
	fixture.summary.LoomCoreScopeDigest = baseline.LoomCoreIdentity.ObservedDigest
	return fixture
}

func (fixture *verifierFixture) write(t *testing.T) {
	t.Helper()
	previousEvidenceSHA256 := ""
	for i := range fixture.outputs {
		if fixture.outputs[i].Evidence.GateBinding != (killtest.GateBinding{}) {
			fixture.outputs[i].Evidence.GateBinding.PreviousEvidenceSHA256 = previousEvidenceSHA256
		}
		if i < len(fixture.summary.Runs) {
			fixture.summary.Runs[i].PreviousEvidenceSHA256 = previousEvidenceSHA256
		}
		path := runEvidencePath(fixture.summaryPath, i+1, killtest.S1cGateRequiredRuns, true)
		if err := writeJSON(path, fixture.outputs[i]); err != nil {
			t.Fatalf("write run %d: %v", i+1, err)
		}
		digest, err := evidenceFileSHA256(path)
		if err != nil {
			t.Fatalf("hash run %d: %v", i+1, err)
		}
		if i < len(fixture.summary.Runs) {
			fixture.summary.Runs[i].EvidenceSHA256 = digest
		}
		previousEvidenceSHA256 = digest
	}
	fixture.writeSummary(t)
}

func (fixture *verifierFixture) rewriteFrom(t *testing.T, startIndex int) {
	t.Helper()
	previousEvidenceSHA256 := fixture.summary.Runs[startIndex-2].EvidenceSHA256
	for index := startIndex; index <= len(fixture.outputs); index++ {
		i := index - 1
		if index > startIndex {
			fixture.outputs[i].Evidence.GateBinding.PreviousEvidenceSHA256 = previousEvidenceSHA256
			fixture.summary.Runs[i].PreviousEvidenceSHA256 = previousEvidenceSHA256
		}
		path := runEvidencePath(fixture.summaryPath, index, killtest.S1cGateRequiredRuns, true)
		if err := writeJSON(path, fixture.outputs[i]); err != nil {
			t.Fatalf("rewrite run %d: %v", index, err)
		}
		digest, err := evidenceFileSHA256(path)
		if err != nil {
			t.Fatalf("hash rewritten run %d: %v", index, err)
		}
		fixture.summary.Runs[i].EvidenceSHA256 = digest
		previousEvidenceSHA256 = digest
	}
	fixture.writeSummary(t)
}

func (fixture *verifierFixture) writeSummary(t *testing.T) {
	t.Helper()
	if err := writeJSON(fixture.summaryPath, fixture.summary); err != nil {
		t.Fatalf("write summary: %v", err)
	}
}

func verifierRunOutput(t *testing.T, index int, start, gateStartedAt time.Time) runOutput {
	t.Helper()
	runID, err := killtest.CanaryRunIDForGate(verifierGateID, index)
	if err != nil {
		t.Fatal(err)
	}
	operator := killtest.PodIdentity{
		Name: "operator", UID: "operator-uid", Node: "worker-1",
		Image: "registry/operator:v1", ImageID: "operator@sha256:aaa",
		StartedAt: time.Date(2026, time.July, 14, 10, 0, 0, 0, time.UTC),
	}
	hud := killtest.PodIdentity{
		Name: "mobile-hud", UID: "hud-uid", Node: "worker-2",
		Image: "registry/mobile-hud:v1", ImageID: "hud@sha256:bbb",
		StartedAt: time.Date(2026, time.July, 14, 10, 0, 0, 0, time.UTC),
	}
	initial := productionVerifierPreflight(t, start.Add(time.Second), "", operator, hud)
	crashA := productionVerifierPreflight(t, start.Add(3*time.Second), runID, operator, hud)
	crashB := productionVerifierPreflight(t, start.Add(5*time.Second), runID, operator, hud)
	final := productionVerifierPreflight(t, start.Add(9*time.Second), "", operator, hud)
	crashAAt := start.Add(3100 * time.Millisecond)
	crashBAt := start.Add(5100 * time.Millisecond)
	evidence := killtest.Evidence{
		GateBinding: killtest.GateBinding{
			Contract: killtest.GateBindingContract, ContractVersion: killtest.GateBindingContractVersion,
			GateID: verifierGateID, RunIndex: index, RequiredRuns: killtest.S1cGateRequiredRuns,
			GateStartedAt: gateStartedAt,
		},
		RunID: runID, AgentType: killtest.AgentTypeClaudeCode,
		SpawnID: fmt.Sprintf("spawn-%d", index), MaxConcurrentSpawnPods: 1,
		InitialPreflight: initial, FinalPreflight: final,
		CrashASafety: killtest.CrashSafetyEvidence{
			ImmediatePreflight: crashA, DeleteIntentRecordedAt: start.Add(2 * time.Second),
			DeleteRequestedAt: crashAAt, DeleteAcceptedAt: crashAAt.Add(time.Millisecond),
			PolicyDeleteBoundary: verifierPolicyDeleteBoundary(crashA, crashAAt),
		},
		CrashBSafety: killtest.CrashSafetyEvidence{
			ImmediatePreflight: crashB, DeleteIntentRecordedAt: start.Add(4 * time.Second),
			DeleteRequestedAt: crashBAt, DeleteAcceptedAt: crashBAt.Add(time.Millisecond),
			PolicyDeleteBoundary: verifierPolicyDeleteBoundary(crashB, crashBAt),
		},
		CrashAAt: crashAAt, CrashBAt: crashBAt,
		PostCrashProcessSamples: []killtest.CanaryProcessSample{
			{ObservedAt: start.Add(2500 * time.Millisecond), CompletedAt: start.Add(2600 * time.Millisecond)},
			{ObservedAt: start.Add(4500 * time.Millisecond), CompletedAt: start.Add(4600 * time.Millisecond)},
		},
		CrashAProcessAuthorization: killtest.ProcessDeleteAuthorization{
			SampleIndex: 0, SampleObservedAt: start.Add(2500 * time.Millisecond),
			SampleCompletedAt: start.Add(2600 * time.Millisecond), AuthorizedAt: crashAAt,
		},
		CrashBProcessAuthorization: killtest.ProcessDeleteAuthorization{
			SampleIndex: 1, SampleObservedAt: start.Add(4500 * time.Millisecond),
			SampleCompletedAt: start.Add(4600 * time.Millisecond), AuthorizedAt: crashBAt,
		},
		Final: killtest.RunDetail{OperatorAuthority: final.AuthorityPlane.Operator, Run: killtest.RunSummary{
			ID: runID, AgentType: killtest.AgentTypeClaudeCode, State: "done",
		}},
	}
	finalWrapper := final
	return runOutput{
		Preflight: initial, FinalPreflight: &finalWrapper,
		Evidence: evidence, Verdicts: verifierPassingVerdicts(),
	}
}

func bindVerifierDeploymentProvenance(report *killtest.PreflightReport) {
	sources := make(map[string]killtest.FluxSourceProvenance, len(report.FluxSourcesEnd.Sources))
	for _, source := range report.FluxSourcesEnd.Sources {
		sources[source.Name] = source
	}
	platform := sources["apps"].ProtectedIdentity
	bind := func(deployment *killtest.DeploymentIdentity, namespace, uid, resourceVersion, owner, digest string, source killtest.GitOpsScopeIdentity) {
		deployment.Namespace = namespace
		deployment.UID = uid
		deployment.ResourceVersion = resourceVersion
		deployment.ContainerName = "controller"
		deployment.ObservedGeneration = deployment.Generation
		deployment.DesiredReplicas = 1
		deployment.Replicas = 1
		deployment.UpdatedReplicas = 1
		deployment.ReadyReplicas = 1
		deployment.AvailableReplicas = 1
		deployment.SpecSHA256 = digest
		deployment.ReviewedSpecSHA256 = digest
		deployment.PodTemplateSHA256 = strings.Repeat("6", 64)
		deployment.ReviewedPodTemplateSHA256 = deployment.PodTemplateSHA256
		deployment.SelectorSHA256 = strings.Repeat("5", 64)
		deployment.ReviewedSelectorSHA256 = deployment.SelectorSHA256
		deployment.Review = killtest.DeploymentReviewIdentity{
			Contract: killtest.DeploymentProvenanceContract, ContractVersion: killtest.DeploymentProvenanceContractVersion,
			FluxOwner: owner, FluxSpecSHA256: sources[owner].RenderSpec.SpecSHA256,
			Renderer: "flux build kustomization --dry-run", RendererVersion: "flux: v-test",
			RenderedSpecSHA256: strings.Repeat("9", 64),
			PlatformRevision:   platform.BaselineRevision, PlatformScopeDigest: platform.BaselineDigest,
			SourceRevision: source.BaselineRevision, SourceScopeDigest: source.BaselineDigest,
		}
	}
	bind(&report.OperatorDeployment, "loom-mills", "operator-deployment-uid", "701", "apps",
		strings.Repeat("e", 64), platform)
	bind(&report.HudDeployment, "loom-hub", "hud-deployment-uid", "801", "loom-hub-servers",
		strings.Repeat("f", 64), sources["loom-hub-servers"].ProtectedIdentity)
	policyChecksum := strings.Repeat("8", 64)
	report.PolicyChecksum = policyChecksum
	report.OperatorDeployment.PolicyChecksum = policyChecksum
	report.PolicyConfigMapReview = killtest.PolicyConfigMapReviewIdentity{
		Contract:        killtest.PolicyConfigMapProvenanceContract,
		ContractVersion: killtest.PolicyConfigMapProvenanceContractVersion,
		Name:            "loom-mills-policy", Namespace: "loom-mills",
		FluxOwner: "apps", FluxSpecSHA256: sources["apps"].RenderSpec.SpecSHA256,
		Renderer: "flux build kustomization --dry-run", RendererVersion: report.OperatorDeployment.Review.RendererVersion,
		PlatformRevision: platform.BaselineRevision, PlatformScopeDigest: platform.BaselineDigest,
		SourcePath: "k3s/mills/configmap-policy.yaml", SourceSHA256: policyChecksum,
		RenderedPayloadSHA256: strings.Repeat("7", 64), LivePayloadSHA256: strings.Repeat("7", 64),
	}
	if report.Operator.Name != "" && report.Operator.UID != "" {
		report.Operator = bindVerifierControllerPod(report.Operator, report.OperatorDeployment)
	}
	if report.Hud.Name != "" && report.Hud.UID != "" {
		report.Hud = bindVerifierControllerPod(report.Hud, report.HudDeployment)
	}
	if report.Operator.Name != "" && report.Operator.UID != "" {
		operator := killtest.OperatorResponseAuthority{
			Contract: killtest.OperatorAuthorityContract, ContractVersion: killtest.OperatorAuthorityContractVersion,
			PodName: report.Operator.Name, PodNamespace: report.Operator.Namespace,
			PodUID: report.Operator.UID, DeploymentName: report.OperatorDeployment.Name,
			BootID: strings.Repeat("e", 64),
		}
		report.AuthorityPlane = killtest.AuthorityPlaneEvidence{
			Contract: killtest.AuthorityPlaneContract, ContractVersion: killtest.AuthorityPlaneContractVersion,
			Kubernetes: killtest.KubernetesClusterAuthority{
				Contract: killtest.AuthorityPlaneContract, ContractVersion: killtest.AuthorityPlaneContractVersion,
				PublicAuthoritySHA256: strings.Repeat("a", 64), APIServerSHA256: strings.Repeat("b", 64),
				CertificateAuthoritySHA256: strings.Repeat("c", 64), ContextName: "verifier-context",
				OperatorNamespaceIdentity: killtest.KubernetesObjectIdentity{
					Name: "loom-mills", UID: "operator-namespace-uid", ResourceVersion: "10",
				},
			},
			Operator: operator, OperatorDeploymentUID: report.OperatorDeployment.UID,
		}
		report.EffectivePolicyAuthority = operator
		report.Quiescence.OperatorAuthority = operator
	}
}

func bindVerifierControllerPod(pod killtest.PodIdentity, deployment killtest.DeploymentIdentity) killtest.PodIdentity {
	pod.Namespace = deployment.Namespace
	pod.ResourceVersion = "pod-rv-" + pod.UID
	pod.ContainerName = "controller"
	pod.ContainerID = "containerd://" + pod.UID
	pod.ContainerRestartCount = 0
	pod.ContainerStartedAt = pod.StartedAt.Add(time.Second)
	pod.ReplicaSetName = deployment.Name + "-rs-abc"
	pod.ReplicaSetUID = deployment.UID + "-rs-uid"
	pod.ReplicaSetResourceVersion = "rs-rv-" + deployment.UID
	pod.ReplicaSetPodTemplateSHA256 = deployment.PodTemplateSHA256
	pod.ReplicaSetSelectorSHA256 = deployment.SelectorSHA256
	pod.ReplicaSetGeneration = 3
	pod.ReplicaSetObservedGeneration = 3
	pod.ReplicaSetDesiredReplicas = 1
	pod.ReplicaSetReplicas = 1
	pod.ReplicaSetFullyLabeledReplicas = 1
	pod.ReplicaSetReadyReplicas = 1
	pod.ReplicaSetAvailableReplicas = 1
	pod.DeploymentName = deployment.Name
	pod.DeploymentUID = deployment.UID
	pod.PodCensusListResourceVersion = "pod-list-rv-" + pod.UID
	pod.PodCensusCount = 1
	pod.PodExecutionContract = killtest.PodExecutionProvenanceContract
	pod.PodExecutionContractVersion = killtest.PodExecutionProvenanceContractVersion
	pod.PodExecutionRenderer = killtest.PodExecutionRenderer
	pod.PodExecutionRendererVersion = killtest.PodExecutionRendererVersion
	pod.LivePodSpecSHA256 = strings.Repeat("4", 64)
	pod.DryRunPodSpecSHA256 = pod.LivePodSpecSHA256
	return pod
}

func verifierPolicyDeleteBoundary(
	report killtest.PreflightReport,
	deleteAt time.Time,
) killtest.PolicyDeleteBoundaryEvidence {
	configMap := func(observedAt time.Time) killtest.PolicyConfigMapBoundarySnapshot {
		return killtest.PolicyConfigMapBoundarySnapshot{
			Identity: report.PolicyConfigMapIdentity, PayloadSHA256: report.PolicyConfigMapReview.LivePayloadSHA256,
			PolicyEnabled: report.ConfigMapPolicyEnabled, WorkflowsEnabled: report.FlagEnabled,
			SubstrateK8sOnly: report.SubstrateK8sOnly, ObservedAt: observedAt,
		}
	}
	liveOperator := report.OperatorDeployment
	liveOperator.ReviewedSpecSHA256 = ""
	liveOperator.ReviewedPodTemplateSHA256 = ""
	liveOperator.ReviewedSelectorSHA256 = ""
	liveOperator.Review = killtest.DeploymentReviewIdentity{}
	return killtest.PolicyDeleteBoundaryEvidence{
		Contract: killtest.PolicyDeleteBoundaryContract, ContractVersion: killtest.PolicyDeleteBoundaryContractVersion,
		ConfigMapA: configMap(deleteAt.Add(-50 * time.Millisecond)),
		Effective: killtest.EffectivePolicyBoundarySnapshot{
			PolicyEnabled: report.EffectivePolicyEnabled, WorkflowsEnabled: report.EffectiveFlagEnabled,
			SubstrateK8sOnly: report.EffectiveSubstrateK8sOnly, ObservedAt: deleteAt.Add(-40 * time.Millisecond),
			OperatorAuthority: report.AuthorityPlane.Operator,
		},
		OperatorDeployment: liveOperator, OperatorDeploymentObservedAt: deleteAt.Add(-30 * time.Millisecond),
		ConfigMapB: configMap(deleteAt.Add(-20 * time.Millisecond)), Review: report.PolicyConfigMapReview,
		CompletedAt: deleteAt.Add(-10 * time.Millisecond),
	}
}

func setVerifierRenderSpecDigest(report *killtest.PreflightReport, digest string) {
	for i := range report.FluxSourcesStart.Sources {
		report.FluxSourcesStart.Sources[i].RenderSpec.SpecSHA256 = digest
		report.FluxSourcesStart.Sources[i].RenderSpec.ReviewedSpecSHA256 = digest
	}
	for i := range report.FluxSourcesEnd.Sources {
		report.FluxSourcesEnd.Sources[i].RenderSpec.SpecSHA256 = digest
		report.FluxSourcesEnd.Sources[i].RenderSpec.ReviewedSpecSHA256 = digest
	}
}

func advanceVerifierPlatformSource(report *killtest.PreflightReport, revision, artifactDigest string) {
	applied := "main@sha1:" + revision
	for _, snapshot := range []*killtest.FluxSourceProvenanceSnapshot{
		&report.FluxSourcesStart, &report.FluxSourcesEnd,
	} {
		for i := range snapshot.Sources {
			if snapshot.Sources[i].Name == "loom-hub-servers" {
				continue
			}
			snapshot.Sources[i].AppliedRevision = applied
			snapshot.Sources[i].AttemptedRevision = applied
			snapshot.Sources[i].ProtectedIdentity.ObservedRevision = revision
			snapshot.Sources[i].ProtectedIdentity.CheckedCommitCount++
		}
		for i := range snapshot.GitRepositories.Repositories {
			repository := &snapshot.GitRepositories.Repositories[i]
			if repository.Name != "gitops-gitlab" {
				continue
			}
			repository.ArtifactRevision = applied
			repository.ArtifactDigest = "sha256:" + artifactDigest
			repository.ProtectedIdentity.ObservedRevision = revision
			repository.ProtectedIdentity.CheckedCommitCount++
		}
	}
	for _, identity := range []*killtest.GitOpsScopeIdentity{
		&report.GitOpsStartIdentity, &report.GitOpsIdentity,
		&report.GitOpsBootstrapStartIdentity, &report.GitOpsBootstrapIdentity,
		&report.GitOpsSystemStartIdentity, &report.GitOpsSystemIdentity,
	} {
		identity.ObservedRevision = revision
		identity.CheckedCommitCount++
	}
	report.GitOpsStartRevision = applied
	report.GitOpsRevision = applied
	report.GitOpsAttempted = applied
	report.GitOpsBootstrapStartRevision = applied
	report.GitOpsBootstrapRevision = applied
	report.GitOpsBootstrapAttempted = applied
	report.GitOpsSystemStartRevision = applied
	report.GitOpsSystemRevision = applied
	report.GitOpsSystemAttempted = applied
}

func verifierPassingVerdicts() killtest.Verdicts {
	return killtest.Verdicts{
		Pass1NoDoubleSpawn: true, Pass1Reason: "pass",
		Pass2JournalOnce: true, Pass2Reason: "pass", Pass3NotExercised: "not exercised",
		Pass4CostProvenance: true, Pass4Reason: "pass",
		Pass5CounterExact: true, Pass5Reason: "pass",
		Pass8CrashSafety: true, Pass8Reason: "pass", Overall: true,
	}
}

func verifierTestEvaluate(evidence killtest.Evidence) killtest.Verdicts {
	verdicts := verifierPassingVerdicts()
	if evidence.MaxConcurrentSpawnPods != 1 ||
		evidence.CrashASafety.DeleteIntentRecordedAt.IsZero() ||
		evidence.CrashASafety.DeleteAcceptedAt.IsZero() ||
		evidence.CrashBSafety.DeleteIntentRecordedAt.IsZero() ||
		evidence.CrashBSafety.DeleteAcceptedAt.IsZero() ||
		evidence.CrashAProcessAuthorization.AuthorizedAt.IsZero() ||
		evidence.CrashBProcessAuthorization.AuthorizedAt.IsZero() {
		verdicts.Pass1NoDoubleSpawn = false
		verdicts.Pass1Reason = "required evidence missing or duplicate spawn"
		verdicts.Overall = false
	}
	return verdicts
}
