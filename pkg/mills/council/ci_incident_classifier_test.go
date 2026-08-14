package council

import "testing"

func TestClassifyCIIncident_ObservedExternalDependencyRegression(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		log        string
		wantClass  CIIncidentClass
		dependency string
	}{
		{
			name:       "gitlab agent authentication",
			log:        `level=error component=agentk msg="rpc error: code = Unauthenticated desc = GitLab Agent is unauthenticated"`,
			wantClass:  CIIncidentExternalDependency,
			dependency: CIIncidentSourceGitLabAgent,
		},
		{
			name:       "clickhouse code 432 merge failure",
			log:        "ClickHouse exception Code: 432. DB::Exception: Cannot merge parts because a merge with the same resulting part is already running",
			wantClass:  CIIncidentExternalDependency,
			dependency: CIIncidentDependencyClickHouse,
		},
		{
			name:       "longhorn replica scheduling failure",
			log:        "Longhorn volume degraded: failed to schedule replica: no available disk candidates to create a new replica",
			wantClass:  CIIncidentExternalDependency,
			dependency: CIIncidentDependencyLonghorn,
		},
		{
			name:       "litellm missing authentication",
			log:        "LiteLLM proxy authentication failed: missing API key",
			wantClass:  CIIncidentExternalDependency,
			dependency: CIIncidentSourceLiteLLM,
		},
		{
			name:      "bare unauthorized response",
			log:       "request failed: 401 Unauthorized",
			wantClass: CIIncidentUnclassified,
		},
		{
			name:      "different clickhouse error code",
			log:       "ClickHouse exception Code: 62. DB::Exception: Syntax error while parsing query",
			wantClass: CIIncidentUnclassified,
		},
		{
			name:      "generic scheduling failure",
			log:       "Warning FailedScheduling: 0/3 nodes are available: insufficient cpu",
			wantClass: CIIncidentUnclassified,
		},
		{
			name:      "repository authentication failure",
			log:       "loom API authentication failed: missing API key",
			wantClass: CIIncidentUnclassified,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ClassifyCIIncident(CIBranchEvidence{}, []CIFailureEvidence{{LogExcerpt: tt.log}})
			if got.Class != tt.wantClass {
				t.Fatalf("Class = %q, want %q: %+v", got.Class, tt.wantClass, got)
			}
			if got.Dependency != tt.dependency {
				t.Fatalf("Dependency = %q, want %q: %+v", got.Dependency, tt.dependency, got)
			}
		})
	}
}

func TestClassifyCIIncident_RealSignalFixtures(t *testing.T) {
	t.Parallel()

	repositoryFailure := CIFailureEvidence{
		JobName:   "test",
		ErrorLine: "--- FAIL: TestClassifyCIIncident (0.00s)",
	}
	gitLabFailure := CIFailureEvidence{
		JobName:                "ci_watch",
		ErrorLine:              "gitlab: GET /projects/47/pipelines: status 503: Service Unavailable",
		UnrelatedBranchMatches: 3,
	}
	tests := []struct {
		name         string
		branch       CIBranchEvidence
		failures     []CIFailureEvidence
		class        CIIncidentClass
		disposition  CIIncidentDisposition
		dependency   string
		evidence     string
		confidence   float64
		retryAllowed bool
	}{
		{
			name:   "clickhouse shared external incident",
			branch: CIBranchEvidence{ChangedFiles: []string{"pkg/mills/council/reviewer.go"}},
			failures: []CIFailureEvidence{{
				JobName:                "metrics-rollup",
				ErrorLine:              "ClickHouse exception: Too many simultaneous queries; timeout exceeded",
				UnrelatedBranchMatches: 2,
			}},
			class: CIIncidentExternalDependency, disposition: CIIncidentDispositionWaitDependency,
			dependency: CIIncidentDependencyClickHouse, evidence: "ClickHouse exception: Too many simultaneous queries; timeout exceeded", confidence: 0.93,
		},
		{
			name:   "langfuse redis shared external incident",
			branch: CIBranchEvidence{ChangedFiles: []string{"pkg/mills/council/reviewer.go"}},
			failures: []CIFailureEvidence{{
				JobName:              "langfuse-worker",
				ErrorLine:            "Langfuse worker: Redis connection error: connect ECONNREFUSED 10.0.0.4:6379",
				MainBranchAlsoFailed: true,
			}},
			class: CIIncidentExternalDependency, disposition: CIIncidentDispositionWaitDependency,
			dependency: CIIncidentDependencyRedis, evidence: "Langfuse worker: Redis connection error: connect ECONNREFUSED 10.0.0.4:6379", confidence: 0.93,
		},
		{
			name:   "gitlab ci shared external incident",
			branch: CIBranchEvidence{ChangedFiles: []string{"pkg/mills/council/reviewer.go"}}, failures: []CIFailureEvidence{gitLabFailure},
			class: CIIncidentExternalDependency, disposition: CIIncidentDispositionWaitDependency,
			dependency: CIIncidentDependencyGitLab, evidence: gitLabFailure.ErrorLine, confidence: 0.96,
		},
		{
			name:   "branch owned repository regression",
			branch: CIBranchEvidence{ChangedFiles: []string{"pkg/mills/council/ci_incident_classifier.go"}}, failures: []CIFailureEvidence{repositoryFailure},
			class: CIIncidentRepositoryRegression, disposition: CIIncidentDispositionFixBranch,
			evidence: repositoryFailure.ErrorLine, confidence: 0.84,
		},
		{
			name:   "shared external wins after repository failure",
			branch: CIBranchEvidence{ChangedFiles: []string{"pkg/mills/council/ci_incident_classifier.go"}}, failures: []CIFailureEvidence{repositoryFailure, gitLabFailure},
			class: CIIncidentExternalDependency, disposition: CIIncidentDispositionWaitDependency,
			dependency: CIIncidentDependencyGitLab, evidence: gitLabFailure.ErrorLine, confidence: 0.96,
		},
		{
			name:   "shared external wins before repository failure",
			branch: CIBranchEvidence{ChangedFiles: []string{"pkg/mills/council/ci_incident_classifier.go"}}, failures: []CIFailureEvidence{gitLabFailure, repositoryFailure},
			class: CIIncidentExternalDependency, disposition: CIIncidentDispositionWaitDependency,
			dependency: CIIncidentDependencyGitLab, evidence: gitLabFailure.ErrorLine, confidence: 0.96,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ClassifyCIIncident(tt.branch, tt.failures)
			assertCIIncident(t, got, tt.class, tt.disposition, tt.retryAllowed)
			if got.Dependency != tt.dependency || got.Evidence != tt.evidence || got.Confidence != tt.confidence {
				t.Fatalf("classification details = {dependency:%q evidence:%q confidence:%.2f}, want {dependency:%q evidence:%q confidence:%.2f}", got.Dependency, got.Evidence, got.Confidence, tt.dependency, tt.evidence, tt.confidence)
			}
		})
	}
}

func TestClassifyCIIncident_ExternalAuthenticationSignatures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		log    string
		source string
	}{
		{
			name:   "gitlab agent grpc log",
			log:    `level=error msg="rpc error: code = Unauthenticated desc = GitLab Agent is unauthenticated" component=agentk`,
			source: CIIncidentSourceGitLabAgent,
		},
		{
			name:   "gitlab agent mixed case",
			log:    "GITLAB-AGENT request failed: NOT AUTHENTICATED",
			source: CIIncidentSourceGitLabAgent,
		},
		{
			name:   "litellm structured log",
			log:    `{"level":"ERROR","message":"LiteLLM authentication failed: Missing API Key for provider"}`,
			source: CIIncidentSourceLiteLLM,
		},
		{
			name:   "litellm alternate phrase",
			log:    "litellm proxy rejected request: API key is missing",
			source: CIIncidentSourceLiteLLM,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ClassifyCIIncident(CIBranchEvidence{
				ChangedFiles: []string{"pkg/mills/council/reviewer.go"},
			}, []CIFailureEvidence{{
				JobName:    "council",
				LogExcerpt: tt.log,
			}})

			assertCIIncident(t, got, CIIncidentExternalDependency, CIIncidentDispositionWaitDependency, false)
			if got.Dependency != tt.source {
				t.Fatalf("Dependency = %q, want source tag %q", got.Dependency, tt.source)
			}
		})
	}
}

func TestClassifyCIIncident_DoesNotBroadenAuthenticationSignatures(t *testing.T) {
	t.Parallel()

	for _, log := range []string{
		"service request failed: Unauthenticated",
		"database user authentication failed",
		"agent is unauthenticated",
		"missing API key for provider openrouter",
		"LiteLLM request failed with status 500",
	} {
		log := log
		t.Run(log, func(t *testing.T) {
			t.Parallel()

			got := ClassifyCIIncident(CIBranchEvidence{}, []CIFailureEvidence{{LogExcerpt: log}})
			if got.Class != CIIncidentUnclassified {
				t.Fatalf("Class = %q, want unclassified for unrelated authentication log: %+v", got.Class, got)
			}
		})
	}
}

func TestClassifyCIIncident_BranchHygieneWinsBeforeFailureEvidence(t *testing.T) {
	t.Parallel()

	got := ClassifyCIIncident(CIBranchEvidence{
		BranchName:  "feat/missing-push",
		MissingPush: true,
	}, []CIFailureEvidence{{
		ErrorLine:            "gitlab: GET /projects/1/pipelines: status 503: Service Unavailable",
		RecursAcrossBranches: true,
	}})

	assertCIIncident(t, got, CIIncidentBranchOrPlanHygiene, CIIncidentDispositionFixBranchHygiene, false)
	if got.Evidence == "" {
		t.Fatal("Evidence is empty")
	}
}

func TestClassifyCIIncident_KnownExternalIncidentSignature(t *testing.T) {
	t.Parallel()

	got := ClassifyCIIncident(CIBranchEvidence{
		ChangedFiles: []string{"pkg/mills/council/editor.go"},
	}, []CIFailureEvidence{{
		JobName:                "ci_watch",
		ErrorLine:              "gitlab: GET /projects/47/pipelines: status 401: unauthorized",
		UnrelatedBranchMatches: 2,
	}})

	assertCIIncident(t, got, CIIncidentExternalDependency, CIIncidentDispositionWaitDependency, false)
	if got.Dependency != "gitlab" {
		t.Fatalf("Dependency = %q, want gitlab", got.Dependency)
	}
	if got.Confidence < 0.90 {
		t.Fatalf("Confidence = %.2f, want high-confidence shared incident", got.Confidence)
	}
}

func TestClassifyCIIncident_ExternalGitLab5xxAcrossBranches(t *testing.T) {
	t.Parallel()

	got := ClassifyCIIncident(CIBranchEvidence{
		ChangedFiles: []string{"pkg/mills/council/brief.go"},
	}, []CIFailureEvidence{{
		JobName:                "ci_watch",
		ErrorLine:              "GitLab returned status 503: Service Unavailable",
		RecursAcrossBranches:   true,
		MainBranchAlsoFailed:   true,
		UnrelatedBranchMatches: 4,
	}})

	assertCIIncident(t, got, CIIncidentExternalDependency, CIIncidentDispositionWaitDependency, false)
	if got.Dependency != "gitlab" {
		t.Fatalf("Dependency = %q, want gitlab", got.Dependency)
	}
}

func TestClassifyCIIncident_RegistryFailureBeforeCheckoutIsExternal(t *testing.T) {
	t.Parallel()

	got := ClassifyCIIncident(CIBranchEvidence{
		ChangedFiles: []string{"pkg/mills/council/roadmap.go"},
	}, []CIFailureEvidence{{
		JobName:              "test",
		ErrorLine:            "Failed to pull image registry.example/ci/go: Harbor returned 401 Unauthorized",
		FailedBeforeCheckout: true,
	}})

	assertCIIncident(t, got, CIIncidentExternalDependency, CIIncidentDispositionWaitDependency, false)
	if got.Dependency != "container_registry" {
		t.Fatalf("Dependency = %q, want container_registry", got.Dependency)
	}
}

func TestClassifyCIIncident_RunnerInfrastructure(t *testing.T) {
	t.Parallel()

	got := ClassifyCIIncident(CIBranchEvidence{
		ChangedFiles: []string{"pkg/mills/council/reviewer.go"},
	}, []CIFailureEvidence{{
		JobName:              "unit",
		ErrorLine:            "runner system failure: pod pending: failed scheduling due to disk pressure",
		FailedBeforeCheckout: true,
	}})

	assertCIIncident(t, got, CIIncidentRunnerInfrastructure, CIIncidentDispositionEscalateRunner, false)
}

func TestClassifyCIIncident_CIConfigurationGuardrail(t *testing.T) {
	t.Parallel()

	got := ClassifyCIIncident(CIBranchEvidence{
		ChangedFiles: []string{"pkg/mills/council/ci_incident_classifier.go"},
	}, []CIFailureEvidence{{
		JobName:   "guardrails:docs-cli",
		ErrorLine: "scripts/ci/check_docs_guardrails.sh: code-facing changes require docs",
	}})

	assertCIIncident(t, got, CIIncidentCIConfiguration, CIIncidentDispositionFixCIConfig, false)
}

func TestClassifyCIIncident_RepositoryRegression(t *testing.T) {
	t.Parallel()

	got := ClassifyCIIncident(CIBranchEvidence{
		ChangedFiles: []string{"pkg/mills/council/ci_incident_classifier.go"},
	}, []CIFailureEvidence{{
		JobName:   "test",
		ErrorLine: "--- FAIL: TestClassifyCIIncident (0.00s)",
	}})

	assertCIIncident(t, got, CIIncidentRepositoryRegression, CIIncidentDispositionFixBranch, false)
}

func TestClassifyCIIncident_SharedExternalBeatsRepoRegression(t *testing.T) {
	t.Parallel()

	got := ClassifyCIIncident(CIBranchEvidence{
		ChangedFiles: []string{"pkg/mills/council/ci_incident_classifier.go"},
	}, []CIFailureEvidence{{
		JobName:                "test",
		ErrorLine:              "--- FAIL: TestWidget",
		LogExcerpt:             "gitlab: GET /projects/47/jobs: status 504: Gateway Timeout",
		UnrelatedBranchMatches: 3,
	}})

	assertCIIncident(t, got, CIIncidentExternalDependency, CIIncidentDispositionWaitDependency, false)
}

func TestClassifyCIIncident_RenovateCompileFailureIsDependencyUpdate(t *testing.T) {
	t.Parallel()

	got := ClassifyCIIncident(CIBranchEvidence{
		BranchName: "renovate/all-minor-patch",
	}, []CIFailureEvidence{{
		JobName:    "test",
		ErrorLine:  "pkg/client/api.go:42:13: undefined: sdk.LegacyClient",
		LogExcerpt: "FAIL\tgithub.com/crb2nu/loom/pkg/client",
	}})

	assertCIIncident(t, got, CIIncidentDependencyUpdate, CIIncidentDispositionFixDependency, false)
	if got.Dependency != "dependency_update" {
		t.Fatalf("Dependency = %q, want dependency_update", got.Dependency)
	}
	if got.Confidence < 0.70 {
		t.Fatalf("Confidence = %.2f, want deterministic dependency-update classification", got.Confidence)
	}
}

func TestClassifyCIIncident_DependencyUpdateResolutionFailureIsBranchOwned(t *testing.T) {
	t.Parallel()

	got := ClassifyCIIncident(CIBranchEvidence{
		BranchName:   "chore/dependency-update-go-modules",
		ChangedFiles: []string{"go.mod", "go.sum"},
	}, []CIFailureEvidence{{
		JobName:   "test",
		ErrorLine: "go: github.com/example/lib@v9.9.9: invalid version: unknown revision v9.9.9",
	}})

	assertCIIncident(t, got, CIIncidentDependencyUpdate, CIIncidentDispositionFixDependency, false)
	if got.Dependency != "go_module" {
		t.Fatalf("Dependency = %q, want go_module", got.Dependency)
	}
}

func TestClassifyCIIncident_RenovateSharedRegistryFailureStaysExternal(t *testing.T) {
	t.Parallel()

	got := ClassifyCIIncident(CIBranchEvidence{
		BranchName: "renovate/docker-buildkit",
	}, []CIFailureEvidence{{
		JobName:                "test",
		ErrorLine:              "npm registry returned status 503: Service Unavailable",
		UnrelatedBranchMatches: 2,
		MainBranchAlsoFailed:   true,
	}})

	assertCIIncident(t, got, CIIncidentExternalDependency, CIIncidentDispositionWaitDependency, false)
	if got.Dependency != "package_mirror" {
		t.Fatalf("Dependency = %q, want package_mirror", got.Dependency)
	}
}

func TestClassifyCIIncident_ProviderFailureWithBranchDiffIsExternal(t *testing.T) {
	t.Parallel()

	got := ClassifyCIIncident(CIBranchEvidence{
		ChangedFiles: []string{"pkg/mills/council/reviewer.go"},
	}, []CIFailureEvidence{{
		JobName:    "review",
		ErrorLine:  "OpenAI request failed with 429 rate limit from model provider",
		LogExcerpt: "model provider quota exhausted while judging self-review",
	}})

	assertCIIncident(t, got, CIIncidentExternalDependency, CIIncidentDispositionWaitDependency, false)
	if got.Dependency != CIIncidentDependencyModelProvider {
		t.Fatalf("Dependency = %q, want %q", got.Dependency, CIIncidentDependencyModelProvider)
	}
}

func TestClassifyCIIncident_LoggingBackendFailureWithBranchDiffIsExternal(t *testing.T) {
	t.Parallel()

	got := ClassifyCIIncident(CIBranchEvidence{
		ChangedFiles: []string{"pkg/mills/council/brief.go"},
	}, []CIFailureEvidence{{
		JobName:    "telemetry-rollup",
		ErrorLine:  "Loki log query failed: status 503 service unavailable",
		LogExcerpt: "logging backend timed out while reading ci_watch logs",
	}})

	assertCIIncident(t, got, CIIncidentExternalDependency, CIIncidentDispositionWaitDependency, false)
	if got.Dependency != CIIncidentDependencyLoggingBackend {
		t.Fatalf("Dependency = %q, want %q", got.Dependency, CIIncidentDependencyLoggingBackend)
	}
}

func TestClassifyCIIncident_ObjectStorageFailureWithBranchDiffIsExternal(t *testing.T) {
	t.Parallel()

	got := ClassifyCIIncident(CIBranchEvidence{
		ChangedFiles: []string{"pkg/mills/council/artifacts.go"},
	}, []CIFailureEvidence{{
		JobName:    "archive",
		ErrorLine:  "S3 artifact storage returned status 503: Service Unavailable",
		LogExcerpt: "cache storage upload timed out while persisting artifacts",
	}})

	assertCIIncident(t, got, CIIncidentExternalDependency, CIIncidentDispositionWaitDependency, false)
	if got.Dependency != CIIncidentDependencyObjectStorage {
		t.Fatalf("Dependency = %q, want %q", got.Dependency, CIIncidentDependencyObjectStorage)
	}
}

func TestClassifyCIIncident_DNSFailureWithBranchDiffIsExternal(t *testing.T) {
	t.Parallel()

	got := ClassifyCIIncident(CIBranchEvidence{
		ChangedFiles: []string{"pkg/mills/council/reviewer.go"},
	}, []CIFailureEvidence{{
		JobName:   "test",
		ErrorLine: "lookup gitlab.flexinfer.ai on 10.43.0.10:53: no such host",
	}})

	assertCIIncident(t, got, CIIncidentExternalDependency, CIIncidentDispositionWaitDependency, false)
	if got.Dependency != CIIncidentDependencyDNS {
		t.Fatalf("Dependency = %q, want %q", got.Dependency, CIIncidentDependencyDNS)
	}
}

func TestClassifyCIIncident_TLSFailureWithBranchDiffIsExternal(t *testing.T) {
	t.Parallel()

	got := ClassifyCIIncident(CIBranchEvidence{
		ChangedFiles: []string{"pkg/mills/council/reviewer.go"},
	}, []CIFailureEvidence{{
		JobName:   "test",
		ErrorLine: "Get https://gitlab.flexinfer.ai/api/v4: tls handshake timeout",
	}})

	assertCIIncident(t, got, CIIncidentExternalDependency, CIIncidentDispositionWaitDependency, false)
	if got.Dependency != CIIncidentDependencyTLS {
		t.Fatalf("Dependency = %q, want %q", got.Dependency, CIIncidentDependencyTLS)
	}
}

func TestClassifyCIIncident_FlakeRetryPassed(t *testing.T) {
	t.Parallel()

	got := ClassifyCIIncident(CIBranchEvidence{
		ChangedFiles: []string{"pkg/mills/council/editor.go"},
	}, []CIFailureEvidence{{
		JobName:     "test",
		ErrorLine:   "context deadline exceeded",
		RetryPassed: true,
	}})

	assertCIIncident(t, got, CIIncidentFlakeOrTransient, CIIncidentDispositionRetryOnce, true)
}

func TestClassifyCIIncident_DefaultsToRepoRegressionWhenBranchOwnsUnknownFailure(t *testing.T) {
	t.Parallel()

	got := ClassifyCIIncident(CIBranchEvidence{
		ChangedFiles: []string{"pkg/mills/council/moderator.go"},
	}, []CIFailureEvidence{{
		JobName:   "verify",
		ErrorLine: "job failed with an unknown deterministic assertion",
	}})

	assertCIIncident(t, got, CIIncidentRepositoryRegression, CIIncidentDispositionFixBranch, false)
	if got.Confidence >= 0.70 {
		t.Fatalf("Confidence = %.2f, want lower-confidence conservative fallback", got.Confidence)
	}
}

func TestClassifyCIIncident_UnclassifiedWithoutBranchOwnership(t *testing.T) {
	t.Parallel()

	got := ClassifyCIIncident(CIBranchEvidence{}, []CIFailureEvidence{{
		JobName:   "verify",
		ErrorLine: "job failed with an unknown deterministic assertion",
	}})

	assertCIIncident(t, got, CIIncidentUnclassified, CIIncidentDispositionEscalateHuman, false)
}

func assertCIIncident(t *testing.T, got CIIncidentClassification, class CIIncidentClass, disposition CIIncidentDisposition, retryAllowed bool) {
	t.Helper()
	if got.Class != class {
		t.Fatalf("Class = %q, want %q: %+v", got.Class, class, got)
	}
	if got.Disposition != disposition {
		t.Fatalf("Disposition = %q, want %q: %+v", got.Disposition, disposition, got)
	}
	if got.RetryAllowed != retryAllowed {
		t.Fatalf("RetryAllowed = %v, want %v: %+v", got.RetryAllowed, retryAllowed, got)
	}
	if got.Reason == "" {
		t.Fatalf("Reason is empty: %+v", got)
	}
}
