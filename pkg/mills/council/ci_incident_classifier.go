package council

import (
	"path/filepath"
	"strings"

	"github.com/crb2nu/loom/pkg/mcperror"
)

// CIIncidentClass is the closed set of recurring GitLab CI incident classes
// used by council triage. Values are stable for persistence and metrics.
type CIIncidentClass string

const (
	CIIncidentRepositoryRegression CIIncidentClass = "repository_regression"
	CIIncidentCIConfiguration      CIIncidentClass = "ci_configuration_regression"
	CIIncidentRunnerInfrastructure CIIncidentClass = "runner_infrastructure_incident"
	CIIncidentExternalDependency   CIIncidentClass = "external_dependency_incident"
	CIIncidentDependencyUpdate     CIIncidentClass = "dependency_update_ci_incident"
	CIIncidentFlakeOrTransient     CIIncidentClass = "flake_or_transient_dependency"
	CIIncidentBranchOrPlanHygiene  CIIncidentClass = "branch_or_plan_hygiene"
	CIIncidentUnclassified         CIIncidentClass = "unclassified"
)

// CIIncidentDisposition is the retry/remediation decision attached to a
// classification.
type CIIncidentDisposition string

const (
	CIIncidentDispositionFixBranch        CIIncidentDisposition = "fix_branch_before_retry"
	CIIncidentDispositionFixCIConfig      CIIncidentDisposition = "fix_ci_config_before_retry"
	CIIncidentDispositionEscalateRunner   CIIncidentDisposition = "escalate_runner_owner"
	CIIncidentDispositionWaitDependency   CIIncidentDisposition = "wait_for_dependency_recovery"
	CIIncidentDispositionFixDependency    CIIncidentDisposition = "fix_dependency_update_before_retry"
	CIIncidentDispositionRetryOnce        CIIncidentDisposition = "retry_once"
	CIIncidentDispositionFixBranchHygiene CIIncidentDisposition = "fix_branch_or_plan_state"
	CIIncidentDispositionEscalateHuman    CIIncidentDisposition = "human_triage_required"
)

const (
	CIIncidentDependencyGitLab            = "gitlab"
	CIIncidentDependencyGitLabAgent       = "gitlab_agent"
	CIIncidentDependencyLonghorn          = "longhorn"
	CIIncidentDependencyClickHouse        = "clickhouse"
	CIIncidentDependencyRedis             = "redis"
	CIIncidentDependencyContainerRegistry = "container_registry"
	CIIncidentDependencyModelProvider     = "model_provider"
	CIIncidentDependencyLoggingBackend    = "logging_backend"
	CIIncidentDependencyObjectStorage     = "object_storage"
	CIIncidentDependencyDNS               = "dns"
	CIIncidentDependencyTLS               = "tls"
	CIIncidentDependencyPackageMirror     = "package_mirror"
	CIIncidentDependencyKubernetesAPI     = "kubernetes_api"
	CIIncidentDependencyRunnerSaturation  = "runner_saturation"
	CIIncidentSourceGitLabAgent           = "gitlab-agent"
	CIIncidentSourceLiteLLM               = "litellm"
)

// CIBranchEvidence describes whether the branch or plan slice can plausibly own
// a failed GitLab CI job.
type CIBranchEvidence struct {
	ProjectPath    string
	BranchName     string
	PlanID         string
	SliceName      string
	ChangedFiles   []string
	SliceFiles     []string
	EmptyMR        bool
	MissingPush    bool
	StalePlanSlice bool
}

// CIFailureEvidence is the normalized subset of pipeline/job evidence needed
// for deterministic incident classification.
type CIFailureEvidence struct {
	PipelineID             string
	JobName                string
	Stage                  string
	RunnerID               string
	ErrorLine              string
	LogExcerpt             string
	FailedBeforeCheckout   bool
	RetryPassed            bool
	RecursAcrossBranches   bool
	UnrelatedBranchMatches int
	MainBranchAlsoFailed   bool
}

// CIIncidentClassification is the structured result returned by
// ClassifyCIIncident.
type CIIncidentClassification struct {
	Class        CIIncidentClass       `json:"class"`
	Disposition  CIIncidentDisposition `json:"disposition"`
	Dependency   string                `json:"dependency,omitempty"`
	Evidence     string                `json:"evidence,omitempty"`
	Reason       string                `json:"reason"`
	Confidence   float64               `json:"confidence"`
	RetryAllowed bool                  `json:"retry_allowed"`
}

// ClassifyCIIncident turns branch and GitLab job evidence into a stable
// incident class. The rules intentionally prefer cross-branch and pre-checkout
// signals over branch-owned failures, so external and runner incidents are not
// mislabeled as code regressions.
func ClassifyCIIncident(branch CIBranchEvidence, failures []CIFailureEvidence) CIIncidentClassification {
	if branch.EmptyMR || branch.MissingPush || branch.StalePlanSlice {
		return CIIncidentClassification{
			Class:        CIIncidentBranchOrPlanHygiene,
			Disposition:  CIIncidentDispositionFixBranchHygiene,
			Evidence:     branchHygieneEvidence(branch),
			Reason:       "branch or plan state is invalid before CI evidence can be trusted",
			Confidence:   0.95,
			RetryAllowed: false,
		}
	}

	best := CIIncidentClassification{
		Class:        CIIncidentUnclassified,
		Disposition:  CIIncidentDispositionEscalateHuman,
		Reason:       "no failure evidence supplied",
		Confidence:   0.20,
		RetryAllowed: false,
	}
	if len(failures) == 0 {
		return best
	}

	branchOwnsFailure := branchHasPlausibleOwnership(branch)
	dependencyUpdate := isDependencyUpdateCI(branch, failures)
	for _, failure := range failures {
		text := failureText(failure)
		shared := isSharedFailure(failure)

		if dep, evidence, ok := classifyExternalAuthenticationIncident(text); ok {
			return CIIncidentClassification{
				Class:        CIIncidentExternalDependency,
				Disposition:  CIIncidentDispositionWaitDependency,
				Dependency:   dep,
				Evidence:     firstNonEmpty(evidence, firstEvidenceLine(failure)),
				Reason:       "authentication failure belongs to an external service dependency",
				Confidence:   confidence(shared, 0.97, 0.90),
				RetryAllowed: false,
			}
		}

		if inc, ok := mcperror.ClassifyExternalCIIncident(text); ok {
			return CIIncidentClassification{
				Class:        CIIncidentExternalDependency,
				Disposition:  CIIncidentDispositionWaitDependency,
				Dependency:   inc.Dependency,
				Evidence:     firstNonEmpty(inc.Evidence, firstEvidenceLine(failure)),
				Reason:       inc.Summary,
				Confidence:   confidence(shared, 0.96, 0.82),
				RetryAllowed: false,
			}
		}

		if dep, evidence, ok := classifyExternalDependency(text); ok &&
			(shared || !branchOwnsFailure || failure.FailedBeforeCheckout || isRepositoryIndependentDependency(dep)) {
			return CIIncidentClassification{
				Class:        CIIncidentExternalDependency,
				Disposition:  CIIncidentDispositionWaitDependency,
				Dependency:   dep,
				Evidence:     firstNonEmpty(evidence, firstEvidenceLine(failure)),
				Reason:       "failure points to a shared dependency outside the branch diff",
				Confidence:   confidence(shared || failure.FailedBeforeCheckout, 0.93, 0.78),
				RetryAllowed: false,
			}
		}

		if evidence, ok := classifyRunnerInfrastructure(text, failure); ok {
			return CIIncidentClassification{
				Class:        CIIncidentRunnerInfrastructure,
				Disposition:  CIIncidentDispositionEscalateRunner,
				Evidence:     firstNonEmpty(evidence, firstEvidenceLine(failure)),
				Reason:       "failure happened in the runner, executor, or Kubernetes substrate",
				Confidence:   confidence(shared || failure.FailedBeforeCheckout, 0.92, 0.80),
				RetryAllowed: false,
			}
		}

		if evidence, ok := classifyCIConfiguration(text, branch); ok {
			return CIIncidentClassification{
				Class:        CIIncidentCIConfiguration,
				Disposition:  CIIncidentDispositionFixCIConfig,
				Evidence:     firstNonEmpty(evidence, firstEvidenceLine(failure)),
				Reason:       "failure points to GitLab CI configuration or guardrail script behavior",
				Confidence:   confidence(branchOwnsCIConfig(branch), 0.88, 0.72),
				RetryAllowed: false,
			}
		}

		if failure.RetryPassed && !shared {
			best = stronger(best, CIIncidentClassification{
				Class:        CIIncidentFlakeOrTransient,
				Disposition:  CIIncidentDispositionRetryOnce,
				Evidence:     firstEvidenceLine(failure),
				Reason:       "a bounded retry passed and the signature has not recurred elsewhere",
				Confidence:   0.70,
				RetryAllowed: true,
			})
			continue
		}

		if evidence, ok := classifyDependencyUpdateFailure(text, branch, failure); ok && dependencyUpdate && !shared {
			return CIIncidentClassification{
				Class:        CIIncidentDependencyUpdate,
				Disposition:  CIIncidentDispositionFixDependency,
				Dependency:   dependencyUpdateDependency(branch, failure),
				Evidence:     firstNonEmpty(evidence, firstEvidenceLine(failure)),
				Reason:       "dependency-update CI failure is branch-owned and needs repository disposition",
				Confidence:   confidence(branchHasPlausibleOwnership(branch), 0.86, 0.74),
				RetryAllowed: false,
			}
		}

		if evidence, ok := classifyRepositoryRegression(text); ok && branchOwnsFailure && !shared {
			best = stronger(best, CIIncidentClassification{
				Class:        CIIncidentRepositoryRegression,
				Disposition:  CIIncidentDispositionFixBranch,
				Evidence:     firstNonEmpty(evidence, firstEvidenceLine(failure)),
				Reason:       "branch diff can plausibly own the deterministic job failure",
				Confidence:   0.84,
				RetryAllowed: false,
			})
			continue
		}

		if branchOwnsFailure && !shared {
			best = stronger(best, CIIncidentClassification{
				Class:        CIIncidentRepositoryRegression,
				Disposition:  CIIncidentDispositionFixBranch,
				Evidence:     firstEvidenceLine(failure),
				Reason:       "branch has a meaningful diff and no shared incident signal matched",
				Confidence:   0.62,
				RetryAllowed: false,
			})
		}

		if dependencyUpdate && !shared {
			best = stronger(best, CIIncidentClassification{
				Class:        CIIncidentDependencyUpdate,
				Disposition:  CIIncidentDispositionFixDependency,
				Dependency:   dependencyUpdateDependency(branch, failure),
				Evidence:     firstEvidenceLine(failure),
				Reason:       "dependency-update branch failed CI without a shared infrastructure signal",
				Confidence:   0.64,
				RetryAllowed: false,
			})
		}
	}

	if best.Evidence == "" {
		best.Evidence = firstEvidenceLine(failures[0])
	}
	if best.Reason == "" {
		best.Reason = "failure evidence did not match a deterministic incident class"
	}
	return best
}

func branchHygieneEvidence(branch CIBranchEvidence) string {
	var parts []string
	if branch.MissingPush {
		parts = append(parts, "missing_push")
	}
	if branch.EmptyMR {
		parts = append(parts, "empty_mr")
	}
	if branch.StalePlanSlice {
		parts = append(parts, "stale_plan_slice")
	}
	if branch.BranchName != "" {
		parts = append(parts, "branch="+branch.BranchName)
	}
	return strings.Join(parts, " ")
}

func branchHasPlausibleOwnership(branch CIBranchEvidence) bool {
	return len(branch.ChangedFiles) > 0 || len(branch.SliceFiles) > 0 || branchLooksLikeDependencyUpdate(branch)
}

func branchOwnsCIConfig(branch CIBranchEvidence) bool {
	for _, file := range branch.ChangedFiles {
		clean := filepath.ToSlash(strings.TrimSpace(file))
		if clean == ".gitlab-ci.yml" || clean == ".gitlab/ci.yml" ||
			strings.HasPrefix(clean, ".gitlab-ci/") ||
			strings.HasPrefix(clean, "scripts/ci/") ||
			strings.HasPrefix(clean, "docs/") {
			return true
		}
	}
	return false
}

func isSharedFailure(f CIFailureEvidence) bool {
	return f.RecursAcrossBranches || f.UnrelatedBranchMatches > 0 || f.MainBranchAlsoFailed
}

func isDependencyUpdateCI(branch CIBranchEvidence, failures []CIFailureEvidence) bool {
	if branchLooksLikeDependencyUpdate(branch) {
		return true
	}
	for _, file := range append(branch.ChangedFiles, branch.SliceFiles...) {
		if dependencyUpdateFile(file) {
			return true
		}
	}
	for _, failure := range failures {
		text := strings.ToLower(failureText(failure))
		if hasAny(text, "renovate", "dependabot", "dependency update", "dependency-update") {
			return true
		}
	}
	return false
}

func branchLooksLikeDependencyUpdate(branch CIBranchEvidence) bool {
	text := strings.ToLower(strings.Join([]string{
		branch.BranchName,
		branch.PlanID,
		branch.SliceName,
	}, "\n"))
	return hasAny(text,
		"renovate/",
		"renovate-",
		"dependabot/",
		"dependabot-",
		"dependency update",
		"dependency-update",
		"update dependencies",
		"update-dependencies",
		"deps/",
		"deps-",
		"bump ",
	)
}

func dependencyUpdateFile(path string) bool {
	clean := filepath.ToSlash(strings.ToLower(strings.TrimSpace(path)))
	switch clean {
	case "go.mod", "go.sum", "package.json", "package-lock.json", "npm-shrinkwrap.json",
		"yarn.lock", "pnpm-lock.yaml", "bun.lockb", "cargo.toml", "cargo.lock",
		"gemfile", "gemfile.lock", "requirements.txt", "poetry.lock", "pyproject.toml",
		"uv.lock", "pipfile", "pipfile.lock", "composer.json", "composer.lock":
		return true
	}
	return strings.HasPrefix(clean, "vendor/") ||
		strings.HasPrefix(clean, "third_party/") ||
		strings.Contains(clean, "/requirements/") ||
		strings.HasSuffix(clean, ".csproj") ||
		strings.HasSuffix(clean, ".fsproj") ||
		strings.HasSuffix(clean, ".vbproj")
}

func failureText(f CIFailureEvidence) string {
	var b strings.Builder
	for _, part := range []string{f.JobName, f.Stage, f.ErrorLine, f.LogExcerpt} {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(part)
	}
	return b.String()
}

func classifyExternalAuthenticationIncident(text string) (source string, evidence string, ok bool) {
	for _, line := range evidenceLines(text) {
		lower := strings.ToLower(line)
		switch {
		case hasAny(lower, "gitlab-agent", "gitlab agent", "agentk") &&
			hasAny(lower, "unauthenticated", "not authenticated"):
			return CIIncidentSourceGitLabAgent, line, true
		case strings.Contains(lower, "litellm") &&
			hasAny(lower, "missing api key", "api key is missing", "no api key"):
			return CIIncidentSourceLiteLLM, line, true
		}
	}
	return "", "", false
}

func classifyExternalDependency(text string) (dependency string, evidence string, ok bool) {
	for _, line := range evidenceLines(text) {
		lower := strings.ToLower(line)
		switch {
		case hasAny(lower, "gitlab") && hasAny(lower, "status 500", "status 502", "status 503", "status 504", "service unavailable", "bad gateway", "gateway timeout"):
			return CIIncidentDependencyGitLab, line, true
		case hasAny(lower, "gitlab-agent", "gitlab agent", "agentk", "kas", "kubernetes agent") &&
			hasAny(lower, "unavailable", "connection refused", "context deadline exceeded", "deadline exceeded", "rpc error", "transport is closing", "websocket: bad handshake", "status 500", "status 502", "status 503", "status 504"):
			return CIIncidentDependencyGitLabAgent, line, true
		case hasAny(lower, "proxy.golang.org", "npm registry", "package mirror", "package proxy", "apt-get", "apk add") &&
			hasAny(lower, "timeout", "connection refused", "service unavailable", "no such host", "tls handshake", "429", "rate limit"):
			return CIIncidentDependencyPackageMirror, line, true
		case hasAny(lower, "registry", "harbor", "dockerhub", "docker hub", "image pull", "pull image") &&
			hasAny(lower, "401", "403", "429", "500", "502", "503", "504", "unauthorized", "rate limit", "too many requests", "service unavailable", "blob upload"):
			return CIIncidentDependencyContainerRegistry, line, true
		case hasAny(lower, "no such host", "server misbehaving", "temporary failure in name resolution", "dns"):
			return CIIncidentDependencyDNS, line, true
		case hasAny(lower, "tls handshake timeout", "certificate signed by unknown authority", "x509:"):
			return CIIncidentDependencyTLS, line, true
		case hasAny(lower, "openai", "anthropic", "flexinfer", "model provider") &&
			hasAny(lower, "429", "rate limit", "quota", "status 500", "status 502", "status 503", "timeout"):
			return CIIncidentDependencyModelProvider, line, true
		case hasAny(lower, "kubernetes api", "apiserver", "kubectl") &&
			hasAny(lower, "service unavailable", "connection refused", "context deadline exceeded", "bad gateway", "gateway timeout"):
			return CIIncidentDependencyKubernetesAPI, line, true
		case hasAny(lower, "longhorn", "csi", "volumeattachment", "volume attachment", "persistentvolumeclaim", "pvc") &&
			hasAny(lower, "attachvolume", "mountvolume", "failed attach", "failed mount", "volume is not ready", "volume attachment", "replica failed", "failed to schedule replica", "engine image", "detached unexpectedly", "i/o error", "stale file handle"):
			return CIIncidentDependencyLonghorn, line, true
		case hasAny(lower, "loki", "grafana", "prometheus", "tempo", "otel collector", "otlp", "logging backend", "log backend", "log query") &&
			hasAny(lower, "connection refused", "connection reset", "too many requests", "rate limit", "timeout", "timed out", "context deadline exceeded", "read timed out", "status 500", "status 502", "status 503", "status 504", "service unavailable", "temporarily unavailable"):
			return CIIncidentDependencyLoggingBackend, line, true
		case hasAny(lower, "clickhouse", "click-house") &&
			hasAny(lower, "connection refused", "connection reset", "too many simultaneous queries", "too many parts", "memory limit", "not enough memory", "timeout exceeded", "read timed out", "status 500", "status 502", "status 503", "status 504", "service unavailable", "temporarily unavailable", "no space left on device") ||
			hasAny(lower, "clickhouse", "click-house") && strings.Contains(lower, "code: 432") && strings.Contains(lower, "merge"):
			return CIIncidentDependencyClickHouse, line, true
		case strings.Contains(lower, "langfuse") && strings.Contains(lower, "redis") &&
			hasAny(lower, "econnrefused", "connection refused", "connect refused"):
			return CIIncidentDependencyRedis, line, true
		case hasAny(lower, "s3", "gcs", "minio", "object storage", "blob storage", "artifact storage", "cache storage") &&
			hasAny(lower, "access denied", "connection refused", "connection reset", "timeout", "timed out", "context deadline exceeded", "status 500", "status 502", "status 503", "status 504", "service unavailable", "temporarily unavailable", "slow down", "throttl", "no space left on device"):
			return CIIncidentDependencyObjectStorage, line, true
		}
	}
	return "", "", false
}

func isRepositoryIndependentDependency(dependency string) bool {
	switch dependency {
	case CIIncidentDependencyGitLab,
		CIIncidentDependencyGitLabAgent,
		CIIncidentDependencyLonghorn,
		CIIncidentDependencyClickHouse,
		CIIncidentDependencyRedis,
		CIIncidentDependencyContainerRegistry,
		CIIncidentDependencyModelProvider,
		CIIncidentDependencyLoggingBackend,
		CIIncidentDependencyObjectStorage,
		CIIncidentDependencyDNS,
		CIIncidentDependencyTLS,
		CIIncidentDependencyPackageMirror,
		CIIncidentDependencyKubernetesAPI:
		return true
	default:
		return false
	}
}

func classifyRunnerInfrastructure(text string, failure CIFailureEvidence) (string, bool) {
	for _, line := range evidenceLines(text) {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "failedscheduling") &&
			!hasAny(lower, "runner", "executor", "pod pending", "waiting for pod") {
			continue
		}
		if strings.Contains(lower, "0/") && strings.Contains(lower, "nodes are available") {
			return line, true
		}
		if hasAny(lower,
			"runner system failure",
			"executor system failure",
			"prepare environment",
			"pod pending",
			"failed scheduling",
			"insufficient cpu",
			"insufficient memory",
			"insufficient ephemeral-storage",
			"disk pressure",
			"memory pressure",
			"ephemeral-storage",
			"oomkilled",
			"evicted",
			"resource quota",
			"exceeded quota",
			"quota exceeded",
			"too many pods",
			"max pods",
			"waiting for pod running",
			"no nodes available",
		) {
			return line, true
		}
	}
	if failure.FailedBeforeCheckout && hasAny(strings.ToLower(text), "runner", "executor", "kubernetes", "pod") {
		return firstEvidenceLine(failure), true
	}
	return "", false
}

func classifyCIConfiguration(text string, branch CIBranchEvidence) (string, bool) {
	for _, line := range evidenceLines(text) {
		lower := strings.ToLower(line)
		if hasAny(lower,
			"yaml invalid",
			"jobs config should contain",
			"invalid ci config",
			"missing required variable",
			"check_docs_guardrails.sh",
			"guardrails:docs-cli",
			"docs guardrail",
			"invalid image name",
			"rules:if invalid",
		) {
			return line, true
		}
	}
	return "", branchOwnsCIConfig(branch) && hasAny(strings.ToLower(text), "gitlab-ci", "guardrail", "ci config")
}

func classifyRepositoryRegression(text string) (string, bool) {
	for _, line := range evidenceLines(text) {
		lower := strings.ToLower(line)
		if hasAny(lower,
			"--- fail:",
			"fail\t",
			"build failed",
			"compilation failed",
			"undefined:",
			"does not implement",
			"golden file mismatch",
			"contract test",
			"lint failed",
			"golangci-lint",
		) {
			return line, true
		}
	}
	return "", false
}

func classifyDependencyUpdateFailure(text string, branch CIBranchEvidence, failure CIFailureEvidence) (string, bool) {
	for _, line := range evidenceLines(text) {
		lower := strings.ToLower(line)
		if hasAny(lower,
			"--- fail:",
			"fail\t",
			"build failed",
			"compilation failed",
			"undefined:",
			"does not implement",
			"golden file mismatch",
			"contract test",
			"lint failed",
			"golangci-lint",
			"no matching version",
			"unknown revision",
			"module declares its path",
			"checksum mismatch",
			"integrity checksum failed",
			"peer dependency",
			"eresolve",
			"lockfile",
		) {
			return line, true
		}
	}
	for _, file := range append(branch.ChangedFiles, branch.SliceFiles...) {
		if dependencyUpdateFile(file) {
			return firstEvidenceLine(failure), true
		}
	}
	return "", false
}

func dependencyUpdateDependency(branch CIBranchEvidence, failure CIFailureEvidence) string {
	for _, file := range append(branch.ChangedFiles, branch.SliceFiles...) {
		clean := filepath.ToSlash(strings.ToLower(strings.TrimSpace(file)))
		switch clean {
		case "go.mod", "go.sum":
			return "go_module"
		case "package.json", "package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml", "bun.lockb":
			return "npm_package"
		case "cargo.toml", "cargo.lock":
			return "rust_crate"
		case "requirements.txt", "poetry.lock", "pyproject.toml", "uv.lock", "pipfile", "pipfile.lock":
			return "python_package"
		}
	}
	for _, value := range []string{branch.BranchName, branch.SliceName, failure.JobName, failure.ErrorLine, failure.LogExcerpt} {
		lower := strings.ToLower(value)
		switch {
		case strings.Contains(lower, "go.mod") || strings.Contains(lower, "go.sum") || strings.Contains(lower, "proxy.golang.org"):
			return "go_module"
		case strings.Contains(lower, "npm") || strings.Contains(lower, "package-lock") || strings.Contains(lower, "yarn") || strings.Contains(lower, "pnpm"):
			return "npm_package"
		case strings.Contains(lower, "cargo"):
			return "rust_crate"
		case strings.Contains(lower, "pip") || strings.Contains(lower, "poetry") || strings.Contains(lower, "pyproject") || strings.Contains(lower, "requirements"):
			return "python_package"
		}
	}
	return "dependency_update"
}

func evidenceLines(text string) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func firstEvidenceLine(f CIFailureEvidence) string {
	for _, part := range []string{f.ErrorLine, f.LogExcerpt, f.JobName, f.Stage, f.PipelineID} {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i := strings.IndexByte(part, '\n'); i >= 0 {
			part = strings.TrimSpace(part[:i])
		}
		return part
	}
	return ""
}

func hasAny(s string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func confidence(cond bool, yes float64, no float64) float64 {
	if cond {
		return yes
	}
	return no
}

func stronger(current CIIncidentClassification, next CIIncidentClassification) CIIncidentClassification {
	if next.Confidence > current.Confidence {
		return next
	}
	return current
}
