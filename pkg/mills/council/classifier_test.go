package council

import "testing"

func TestClassifyExternalDependencyIncident_CIShapedGitLabEvidence(t *testing.T) {
	t.Parallel()

	got, ok := ClassifyExternalDependencyIncident(CIBranchEvidence{
		ChangedFiles: []string{"pkg/mills/council/classifier.go"},
	}, []CIFailureEvidence{{
		JobName:                "ci_watch",
		Stage:                  "merge",
		ErrorLine:              "gitlab: GET /projects/47/pipelines: status 503: Service Unavailable",
		RecursAcrossBranches:   true,
		UnrelatedBranchMatches: 3,
	}})

	if !ok {
		t.Fatal("ClassifyExternalDependencyIncident returned ok=false")
	}
	assertCIIncident(t, got, CIIncidentExternalDependency, CIIncidentDispositionWaitDependency, false)
	if got.Dependency != "gitlab" {
		t.Fatalf("Dependency = %q, want gitlab", got.Dependency)
	}
}

func TestClassifyExternalDependencyIncident_LonghornStorageEvidence(t *testing.T) {
	t.Parallel()

	got, ok := ClassifyExternalDependencyIncident(CIBranchEvidence{
		ChangedFiles: []string{"pkg/mills/council/classifier.go"},
	}, []CIFailureEvidence{{
		JobName:                "test",
		Stage:                  "prepare",
		ErrorLine:              `MountVolume.MountDevice failed for volume "pvc-123": Longhorn volume is not ready`,
		LogExcerpt:             "csi.longhorn.io failed attach for volumeattachment/pvc-123",
		FailedBeforeCheckout:   true,
		UnrelatedBranchMatches: 2,
	}})

	if !ok {
		t.Fatal("ClassifyExternalDependencyIncident returned ok=false")
	}
	assertCIIncident(t, got, CIIncidentExternalDependency, CIIncidentDispositionWaitDependency, false)
	if got.Dependency != "longhorn" {
		t.Fatalf("Dependency = %q, want longhorn", got.Dependency)
	}
	if got.Evidence == "" {
		t.Fatal("Evidence is empty")
	}
}

func TestClassifyExternalDependencyIncident_GitLabAgentEvidence(t *testing.T) {
	t.Parallel()

	got, ok := ClassifyExternalDependencyIncident(CIBranchEvidence{
		ChangedFiles: []string{"pkg/mills/council/classifier.go"},
	}, []CIFailureEvidence{{
		JobName:                "deploy",
		Stage:                  "verify",
		ErrorLine:              "gitlab-agent agentk rpc error: code = Unavailable desc = transport is closing",
		LogExcerpt:             "KAS websocket: bad handshake while contacting Kubernetes agent",
		FailedBeforeCheckout:   true,
		UnrelatedBranchMatches: 4,
	}})

	if !ok {
		t.Fatal("ClassifyExternalDependencyIncident returned ok=false")
	}
	assertCIIncident(t, got, CIIncidentExternalDependency, CIIncidentDispositionWaitDependency, false)
	if got.Dependency != CIIncidentDependencyGitLabAgent {
		t.Fatalf("Dependency = %q, want %q", got.Dependency, CIIncidentDependencyGitLabAgent)
	}
}

func TestClassifyExternalDependencyIncident_ClickHouseSaturationEvidence(t *testing.T) {
	t.Parallel()

	got, ok := ClassifyExternalDependencyIncident(CIBranchEvidence{
		ChangedFiles: []string{"pkg/mills/council/classifier.go"},
	}, []CIFailureEvidence{{
		JobName:                "metrics-rollup",
		Stage:                  "report",
		ErrorLine:              "ClickHouse exception: Too many simultaneous queries; timeout exceeded",
		LogExcerpt:             "clickhouse read timed out while querying pipeline history",
		UnrelatedBranchMatches: 2,
		MainBranchAlsoFailed:   true,
	}})

	if !ok {
		t.Fatal("ClassifyExternalDependencyIncident returned ok=false")
	}
	assertCIIncident(t, got, CIIncidentExternalDependency, CIIncidentDispositionWaitDependency, false)
	if got.Dependency != CIIncidentDependencyClickHouse {
		t.Fatalf("Dependency = %q, want %q", got.Dependency, CIIncidentDependencyClickHouse)
	}
}

func TestClassifyCIIncident_InfraSaturationEvidence(t *testing.T) {
	t.Parallel()

	got := ClassifyCIIncident(CIBranchEvidence{
		ChangedFiles: []string{"pkg/mills/council/classifier.go"},
	}, []CIFailureEvidence{{
		JobName:              "test",
		Stage:                "prepare",
		ErrorLine:            "runner system failure: 0/6 nodes are available: insufficient cpu, exceeded quota pods",
		LogExcerpt:           "waiting for pod running: pod pending because resource quota exceeded",
		FailedBeforeCheckout: true,
	}})

	assertCIIncident(t, got, CIIncidentRunnerInfrastructure, CIIncidentDispositionEscalateRunner, false)
	if key := CIIncidentPolicyDedupKey(got); key != "runner_infrastructure_incident|runner_saturation|escalate_runner_owner" {
		t.Fatalf("dedup key = %q", key)
	}
}

func TestClassifyCIIncident_RepoOwnedZeroSlashLineIsNotInfraSaturation(t *testing.T) {
	t.Parallel()

	got := ClassifyCIIncident(CIBranchEvidence{
		ChangedFiles: []string{"pkg/service/handler.go"},
	}, []CIFailureEvidence{{
		JobName:   "test",
		Stage:     "test",
		ErrorLine: "--- FAIL: TestRatio (0.00s): got 0/12 successful requests",
	}})

	assertCIIncident(t, got, CIIncidentRepositoryRegression, CIIncidentDispositionFixBranch, false)
}

func TestCIIncidentPolicyDedupKey_CollapsesSameIncidentOwner(t *testing.T) {
	t.Parallel()

	first, ok := ClassifyExternalDependencyIncident(CIBranchEvidence{}, []CIFailureEvidence{{
		JobName:                "ci_watch",
		ErrorLine:              "gitlab: GET /projects/47/pipelines: status 503: Service Unavailable",
		RecursAcrossBranches:   true,
		UnrelatedBranchMatches: 2,
	}})
	if !ok {
		t.Fatal("first classification ok=false")
	}
	second, ok := ClassifyExternalDependencyIncident(CIBranchEvidence{}, []CIFailureEvidence{{
		JobName:              "merge",
		ErrorLine:            "GitLab returned status 504: Gateway Timeout while reading jobs",
		MainBranchAlsoFailed: true,
	}})
	if !ok {
		t.Fatal("second classification ok=false")
	}

	if first.Evidence == second.Evidence {
		t.Fatalf("test setup expected distinct evidence, got %q", first.Evidence)
	}
	if got, want := CIIncidentPolicyDedupKey(first), CIIncidentPolicyDedupKey(second); got != want {
		t.Fatalf("dedup key mismatch: %q != %q", got, want)
	}
}

func TestClassifyExternalDependencyIncident_IgnoresRepoOwnedFailure(t *testing.T) {
	t.Parallel()

	got, ok := ClassifyExternalDependencyIncident(CIBranchEvidence{
		ChangedFiles: []string{"pkg/mills/council/classifier.go"},
	}, []CIFailureEvidence{{
		JobName:   "test",
		ErrorLine: "--- FAIL: TestClassifyExternalDependencyIncident (0.00s)",
	}})

	if ok {
		t.Fatalf("ClassifyExternalDependencyIncident returned ok=true, got %+v", got)
	}
}

func TestClassifyRecurringInfrastructureWorkspaceSignal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		service    string
		sample     string
		wantMatch  bool
		dependency string
	}{
		{name: "clickhouse code 432", service: "monitoring/clickhouse", sample: "DB::Exception: Cannot merge parts: Code: 432", wantMatch: true, dependency: "clickhouse"},
		{name: "clickhouse formatting variant", service: "clickhouse-operator", sample: "Code 432: MergeTree is merging parts", wantMatch: true, dependency: "clickhouse"},
		{name: "longhorn replica scheduling", service: "longhorn-system/longhorn-manager", sample: "unable to schedule a replica on any node", wantMatch: true, dependency: "longhorn"},
		{name: "longhorn case variant", service: "storage/longhorn", sample: "FAILED TO SCHEDULE REPLICA: insufficient storage", wantMatch: true, dependency: "longhorn"},
		{name: "litellm missing key", service: "ai/litellm-proxy", sample: "API key is missing for the configured provider", wantMatch: true, dependency: "litellm"},
		{name: "litellm env formatting", service: "litellm", sample: "LITELLM_API_KEY not set", wantMatch: true, dependency: "litellm"},
		{name: "postgres missing role", service: "database/postgresql", sample: `FATAL: role "loom_reader" does not exist`, wantMatch: true, dependency: "postgres"},
		{name: "postgres service suffix", service: "platform/psql", sample: `role "mills" does not exist`, wantMatch: true, dependency: "postgres"},
		{name: "code alone is not clickhouse", service: "loom-core/tests", sample: "validation returned code 432", wantMatch: false},
		{name: "generic scheduling is not longhorn", service: "loom-core/scheduler", sample: "failed to schedule replica task", wantMatch: false},
		{name: "generic missing api key is not litellm", service: "loom-core/unit-tests", sample: "API key is missing", wantMatch: false},
		{name: "repository role assertion is not postgres", service: "loom-core/unit-tests", sample: `expected role "admin" does not exist`, wantMatch: false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			input := WorkspaceSignal{Source: "loki", Service: tc.service, Sample: tc.sample}
			got, matched := ClassifyRecurringInfrastructureWorkspaceSignal(input)
			if matched != tc.wantMatch {
				t.Fatalf("matched = %t, want %t: %+v", matched, tc.wantMatch, got)
			}
			if !tc.wantMatch {
				if got != input {
					t.Fatalf("near miss changed: got %+v, want %+v", got, input)
				}
				return
			}
			if got.IncidentClass != CIIncidentExternalDependency {
				t.Fatalf("class = %q, want %q", got.IncidentClass, CIIncidentExternalDependency)
			}
			if got.ExternalDependency != tc.dependency {
				t.Fatalf("dependency = %q, want %q", got.ExternalDependency, tc.dependency)
			}
		})
	}
}

func TestClassifyExternalWorkspaceSignals_UsesRecurringInfrastructureAllowlist(t *testing.T) {
	t.Parallel()

	got := ClassifyExternalWorkspaceSignals([]WorkspaceSignal{{
		Source:  "loki",
		Service: "database/postgres",
		Sample:  `FATAL: role "loom" does not exist`,
	}})
	if len(got) != 1 || got[0].IncidentClass != CIIncidentExternalDependency || got[0].ExternalDependency != "postgres" {
		t.Fatalf("unexpected classification: %+v", got)
	}
}
