package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/runner"
)

// Config bundles every operator-tunable knob. Resolved via flag → env →
// default in that order; see (*Config).BindFlags + (*Config).ApplyEnv.
type Config struct {
	// DBPath is the SQLite file backing the canonical store. The parent
	// directory must exist; the operator creates the file on first run.
	DBPath string

	// PolicyPath points at the YAML policy mounted into the pod (the k3s
	// ConfigMap or a developer's local file).
	PolicyPath string

	// SquadsPath is the directory containing squad manifest YAMLs (one per
	// squad). The loader scans this path on boot and watches it via
	// fsnotify for hot-reload. Empty / missing dir is non-fatal: the
	// operator boots without squads and the squad endpoints return empty
	// results.
	SquadsPath string

	// HTTPAddr is the bind address for the REST + MCP Streamable HTTP
	// listener. Pods expose this via a ClusterIP Service.
	HTTPAddr string

	// MetricsAddr is the bind address for /metrics + /healthz + /readyz.
	// Kept on a separate listener so health probes don't interleave with
	// real traffic and so a misbehaving handler can't break liveness.
	MetricsAddr string

	// EnableReconciler defaults to whatever the policy says. Set
	// LOOM_MILLS_ENABLED=true to override the YAML; "false" forces off.
	// Unset (the default) defers to the policy.
	EnableReconciler *bool

	// RepoRoot is the absolute path to the loom-core checkout the
	// council writes artifacts into and the brief assembler reads
	// .loom/00-index.md from. In production this is the operator pod's
	// mounted clone (a read-write PVC). For local dev it's the
	// developer's worktree.
	RepoRoot string

	// Debug enables verbose slog output.
	Debug bool

	// FlexInferProxyURL is the OpenAI-compatible HTTP proxy that
	// LLM-judged gates and the WeaverWorker call. Empty disables the
	// real LLM clients (gates fall back to skip; the research stage
	// returns empty notes via NoOpDispatcher).
	FlexInferProxyURL string
	// FlexInferToken is an optional bearer auth token forwarded to the
	// proxy.
	FlexInferToken string
	// FlexInferJudgeModel is the model id rubric judges target. Empty
	// uses the client's default (gemma4-26b-a4b-gptq via the aimodels
	// registry; see pkg/mills/clients/flexinfer.go).
	FlexInferJudgeModel string
	// FlexInferWeaverModel is the model id WeaverWorker targets. Empty
	// falls through to JudgeModel.
	FlexInferWeaverModel string
	// FlexInferMemoryModel is the model id item-memory consolidation targets
	// (pipeline.MemoryConsolidator, armed by LOOM_MILLS_MEMORY_CONSOLIDATE).
	// Empty falls through to the resolved weaver/research model: distilling a
	// span of a work journal is a summarization job of the same shape and size
	// as research, so the lane that is already sized and paid for is the right
	// default. Env: FLEXINFER_MEMORY_MODEL.
	FlexInferMemoryModel string
	// FlexInferTimeout caps any single proxy HTTP call. Zero falls
	// through to the client default (5min). Operators tune this via
	// FLEXINFER_TIMEOUT (Go duration; e.g. "180s", "3m") when the
	// backing model is slow enough that the default would clip the
	// research stage prematurely.
	FlexInferTimeout time.Duration

	// LiteLLMProxyURL is the cluster LiteLLM gateway (OpenAI-compatible)
	// that fronts remote providers via OpenRouter — the route council
	// reviewer lenses with backend "litellm" use (e.g. or/kimi-k3, wired
	// 2026-07-17 via gitops!423). Empty leaves such lenses on the fake
	// reviewer fallback so a half-configured policy is visible, not silent.
	LiteLLMProxyURL string
	// LiteLLMToken is the bearer key for the LiteLLM gateway.
	LiteLLMToken string

	// JudgeBackend selects the LLM backend for the rubric judge (the
	// LLM-judged gates spec_conformance + pr_self_review) AND the council
	// contradiction judge. "" or "flexinfer" (the default) keeps the judge
	// on the FlexInfer proxy — zero behavior change. "litellm" binds the
	// cluster LiteLLM gateway (LiteLLMProxyURL + LiteLLMToken) so the judge
	// runs on a frontier OpenRouter model; the model id then follows
	// FlexInferJudgeModel (e.g. or/kimi-k3) and must be gateway-routable.
	// A litellm selection with no gateway or no explicit judge model fails
	// loud at startup and falls back to FlexInfer rather than 404ing per
	// call. Env: MILLS_JUDGE_BACKEND.
	JudgeBackend string
	// WeaverBackend selects the LLM backend for the research/weaver stage,
	// independently of JudgeBackend. Same contract: "" / "flexinfer"
	// default vs "litellm" (model follows FlexInferWeaverModel). Env:
	// MILLS_WEAVER_BACKEND.
	WeaverBackend string

	// GitLabAPIURL is the GitLab REST API base, e.g.
	// "https://gitlab.flexinfer.ai/api/v4". Empty disables the GitLab
	// client (mr/ci_watch/merge/cleanup stages stub out, escalation
	// issues are skipped with a warn log).
	GitLabAPIURL string
	// GitLabToken is the project or personal access token sent as the
	// PRIVATE-TOKEN header.
	GitLabToken string
	// GitLabProject is the URL-encoded slug or numeric id of the
	// project the operator manages MRs against.
	GitLabProject string
	// GitLabHeadSHADeadline overrides how long ci_watch waits for an MR to
	// report any head SHA before failing with ErrMRHeadSHAUnavailable. Env:
	// LOOM_MILLS_GITLAB_HEAD_SHA_DEADLINE. Zero keeps the client default (5m).
	GitLabHeadSHADeadline time.Duration
	// GitLabBranchPipelineDeadline overrides how long ci_watch waits for a push
	// pipeline to appear for a known head SHA before failing with
	// ErrBranchPipelineUnavailable. Env:
	// LOOM_MILLS_GITLAB_BRANCH_PIPELINE_DEADLINE. Zero keeps the client default
	// (10m).
	//
	// Both exist as env knobs because these bounds decide when a run stops
	// waiting and escalates: a repo with unusually slow MR preparation or a
	// backed-up runner fleet needs to widen them without a rebuild, and an
	// operator draining a wedged backlog wants to narrow them. Their neighbours
	// (poll interval, council stage budgets) are already tunable this way; these
	// two shipped without an override.
	GitLabBranchPipelineDeadline time.Duration
	// RegressionSweepInterval bounds how often the reconciler's post-merge
	// regression attribution sweep runs. Env:
	// LOOM_MILLS_REGRESSION_SWEEP_INTERVAL. Zero keeps the reconciler default
	// (1h). Each pass is two read-only GitLab list calls, so the knob exists to
	// slow it down on a rate-limited instance — or to speed it up while
	// verifying attribution after a revert.
	RegressionSweepInterval time.Duration
	// SignatureMiningInterval bounds how often the reconciler's
	// signature-candidate mining sweep runs. Env:
	// LOOM_MILLS_SIGNATURE_MINING_INTERVAL. Zero keeps the reconciler default
	// (6h). Each pass is one store read plus in-process clustering over a
	// two-week window, so the knob exists to slow it down on a large escalation
	// corpus — or to speed it up while reviewing a proposal.
	SignatureMiningInterval time.Duration
	// LearningSignalInterval bounds how often the reconciler's learning-signal
	// export sweep republishes the judge calibration, promotion evidence and
	// configuration outcome gauges. Env:
	// LOOM_MILLS_LEARNING_SIGNAL_INTERVAL. Zero keeps the reconciler default
	// (30m). Each pass is two window scans over the store, so the knob exists
	// to slow it down on a busy events table — or to speed it up while
	// verifying a judge-drift alert.
	LearningSignalInterval time.Duration
	// LearningSignalWindow bounds the window those gauges describe. Env:
	// LOOM_MILLS_LEARNING_SIGNAL_WINDOW. Zero keeps the reconciler default
	// (336h, matching the report endpoints). Narrowing it is the escape hatch
	// when a window grows past the builders' 10000-row scan limits and every
	// export fails.
	LearningSignalWindow time.Duration
	// LearningSignalExport arms the export sweep; nil keeps it on. Env:
	// LOOM_MILLS_LEARNING_SIGNAL_EXPORT. The off switch exists because the
	// sweep is the only reconciler pass whose whole output is metric
	// cardinality: an operator drowning in series can silence it without
	// losing the JSON reports the same builders serve.
	LearningSignalExport *bool

	// GitOpsGitLabToken is a SEPARATE GitLab token scoped to the GitOps
	// repo (platform/gitops). The pipeline token (GitLabToken) is
	// deliberately walled off from platform/gitops (it's a protected
	// path), so the HUD pause/resume kill-switch — which opens a GitOps
	// auto-PR flipping policy `enabled:` — needs its own credential.
	// Empty disables the kill-switch endpoint (returns 503).
	GitOpsGitLabToken string
	// GitOpsGitLabProject is the slug/id of the GitOps repo, e.g.
	// "platform/gitops". Empty disables the kill-switch endpoint.
	GitOpsGitLabProject string
	// GitOpsGitLabAPIURL overrides the API base for the GitOps client.
	// Defaults to GitLabAPIURL when unset (same GitLab instance).
	GitOpsGitLabAPIURL string
	// GitOpsPolicyPath is the in-repo path to the mills policy ConfigMap
	// the kill-switch edits. Defaults to "k3s/mills/configmap-policy.yaml".
	GitOpsPolicyPath string
	// GitOpsDefaultBranch is the branch the kill-switch MR targets and
	// branches off. Defaults to "main".
	GitOpsDefaultBranch string

	// HUDBaseURL is the loom HUD's HTTP base, e.g.
	// "http://hud.loom-system.svc.cluster.local:8090". Empty disables
	// the HUD spawn client (plan_slice/implement/pr_self_review fall
	// back to NoOpDispatcher).
	HUDBaseURL string
	// HUDToken is the mobile bearer token configured via
	// HUD_MOBILE_OPERATOR_TOKEN on the HUD process.
	HUDToken string

	// WeaverURL is the HTTP base for the routed weaver dispatch (POST
	// /api/weaver/query). When set together with MILLS_RESEARCH_VIA_
	// WEAVER=shadow|on, the WeaverWorker delegates research to the
	// loom Router instead of the legacy single-prompt FlexInfer chat.
	// Defaults to HUDBaseURL when unset (the same loomd process owns
	// both surfaces). Empty + non-default mode logs a warning and
	// keeps the legacy chat path.
	WeaverURL string
	// WeaverToken is an optional bearer for /api/weaver/query. Today
	// the endpoint sits behind the HUD's withCORS middleware (no
	// token required); the field is plumbed for future hardening.
	WeaverToken string

	// LokiURL is the in-cluster Loki base for the council brief's
	// workspace-signals section (W3.1 of .loom/126). Defaults to the
	// logging-namespace service; empty disables the Loki fetch while the brief
	// retains an explicit workspace-signals absence note.
	LokiURL string

	// CouncilStages bounds each council phase and the whole pass. Applied
	// inside runner.Execute, so the scheduled (cron) trigger — which has no
	// HTTP handler to cap it — inherits the overall bound too. Defaults come
	// from runner.DefaultStageBudgets(); each field is overridable via
	// LOOM_MILLS_COUNCIL_<STAGE>_TIMEOUT.
	CouncilStages runner.StageBudgets

	// CouncilAsyncConcurrency bounds concurrently executing async council
	// runs (LOOM_MILLS_COUNCIL_ASYNC_CONCURRENCY). It only stops goroutine
	// pileup; the real spend/concurrency bound is the admission kernel's
	// policy limits.
	CouncilAsyncConcurrency int
}

// DefaultConfig returns the values used when neither flag nor env supplies one.
func DefaultConfig() Config {
	return Config{
		DBPath:      "/var/lib/loom-mills/state.db",
		PolicyPath:  "/etc/loom-mills/policy.yaml",
		SquadsPath:  "/etc/loom-mills/squads",
		HTTPAddr:    ":8090",
		MetricsAddr: ":9090",
		RepoRoot:    "/workspace/loom-core",
		LokiURL:     "http://loki.logging.svc.cluster.local:3100",

		CouncilStages:           runner.DefaultStageBudgets(),
		CouncilAsyncConcurrency: defaultCouncilAsyncConcurrency,
	}
}

// defaultCouncilAsyncConcurrency keeps async council runs serialized by
// default: a council pass is expensive and the admission kernel already
// serializes durable spend.
const defaultCouncilAsyncConcurrency = 1

// ApplyEnv overlays env-derived values on top of c. Flags should be parsed
// after this call so they win over env. LOOM_MILLS_* is the canonical prefix.
func (c *Config) ApplyEnv() {
	if v := strings.TrimSpace(os.Getenv("LOOM_MILLS_DB_PATH")); v != "" {
		c.DBPath = v
	}
	if v := strings.TrimSpace(os.Getenv("LOKI_URL")); v != "" {
		c.LokiURL = v
	}
	if v := strings.TrimSpace(os.Getenv("LOOM_MILLS_POLICY_PATH")); v != "" {
		c.PolicyPath = v
	}
	if v := strings.TrimSpace(os.Getenv("LOOM_MILLS_SQUADS_PATH")); v != "" {
		c.SquadsPath = v
	}
	if v := strings.TrimSpace(os.Getenv("LOOM_MILLS_HTTP_ADDR")); v != "" {
		c.HTTPAddr = v
	}
	if v := strings.TrimSpace(os.Getenv("LOOM_MILLS_METRICS_ADDR")); v != "" {
		c.MetricsAddr = v
	}
	if v := strings.TrimSpace(os.Getenv("LOOM_MILLS_REPO_ROOT")); v != "" {
		c.RepoRoot = v
	}
	if v := strings.TrimSpace(os.Getenv("LOOM_MILLS_ENABLED")); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			b := true
			c.EnableReconciler = &b
		case "0", "false", "no", "off":
			b := false
			c.EnableReconciler = &b
		}
	}
	if v := strings.TrimSpace(os.Getenv("LOOM_MILLS_DEBUG")); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			c.Debug = true
		}
	}
	if v := strings.TrimSpace(os.Getenv("FLEXINFER_PROXY_URL")); v != "" {
		c.FlexInferProxyURL = v
	}
	if v := strings.TrimSpace(os.Getenv("FLEXINFER_TOKEN")); v != "" {
		c.FlexInferToken = v
	}
	if v := strings.TrimSpace(os.Getenv("FLEXINFER_JUDGE_MODEL")); v != "" {
		c.FlexInferJudgeModel = v
	}
	if v := strings.TrimSpace(os.Getenv("FLEXINFER_WEAVER_MODEL")); v != "" {
		c.FlexInferWeaverModel = v
	}
	if v := strings.TrimSpace(os.Getenv("FLEXINFER_MEMORY_MODEL")); v != "" {
		c.FlexInferMemoryModel = v
	}
	if v := strings.TrimSpace(os.Getenv("FLEXINFER_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			c.FlexInferTimeout = d
		}
	}
	if v := strings.TrimSpace(os.Getenv("LITELLM_PROXY_URL")); v != "" {
		c.LiteLLMProxyURL = v
	}
	if v := strings.TrimSpace(os.Getenv("LOOM_MILLS_LITELLM_KEY")); v != "" {
		c.LiteLLMToken = v
	}
	if v := strings.TrimSpace(os.Getenv("MILLS_JUDGE_BACKEND")); v != "" {
		c.JudgeBackend = v
	}
	if v := strings.TrimSpace(os.Getenv("MILLS_WEAVER_BACKEND")); v != "" {
		c.WeaverBackend = v
	}
	if v := strings.TrimSpace(os.Getenv("GITLAB_API_URL")); v != "" {
		c.GitLabAPIURL = v
	}
	if v := strings.TrimSpace(os.Getenv("GITLAB_TOKEN")); v != "" {
		c.GitLabToken = v
	}
	if v := strings.TrimSpace(os.Getenv("GITLAB_PROJECT")); v != "" {
		c.GitLabProject = v
	}
	// ci_watch bounds. durationEnv honours a negative value as "no bound" for
	// council stages, which is meaningless here — a negative deadline would
	// fail every poll on its first observation — so only positive values apply.
	for name, dst := range map[string]*time.Duration{
		"LOOM_MILLS_GITLAB_HEAD_SHA_DEADLINE":        &c.GitLabHeadSHADeadline,
		"LOOM_MILLS_GITLAB_BRANCH_PIPELINE_DEADLINE": &c.GitLabBranchPipelineDeadline,
	} {
		if d, ok := durationEnv(name); ok && d > 0 {
			*dst = d
		}
	}
	// Regression attribution sweep cadence. Same positive-only rule: a
	// non-positive interval would run the sweep on every tick.
	if d, ok := durationEnv("LOOM_MILLS_REGRESSION_SWEEP_INTERVAL"); ok && d > 0 {
		c.RegressionSweepInterval = d
	}
	// Signature-candidate mining cadence. Same positive-only rule.
	if d, ok := durationEnv("LOOM_MILLS_SIGNATURE_MINING_INTERVAL"); ok && d > 0 {
		c.SignatureMiningInterval = d
	}
	// Learning-signal export cadence and window. Same positive-only rule: a
	// non-positive interval would run the sweep on every tick, and a
	// non-positive window would make the report builders reject every pass.
	if d, ok := durationEnv("LOOM_MILLS_LEARNING_SIGNAL_INTERVAL"); ok && d > 0 {
		c.LearningSignalInterval = d
	}
	if d, ok := durationEnv("LOOM_MILLS_LEARNING_SIGNAL_WINDOW"); ok && d > 0 {
		c.LearningSignalWindow = d
	}
	if v := strings.TrimSpace(os.Getenv("LOOM_MILLS_LEARNING_SIGNAL_EXPORT")); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			b := true
			c.LearningSignalExport = &b
		case "0", "false", "no", "off":
			b := false
			c.LearningSignalExport = &b
		}
	}
	if v := strings.TrimSpace(os.Getenv("GITOPS_GITLAB_TOKEN")); v != "" {
		c.GitOpsGitLabToken = v
	}
	if v := strings.TrimSpace(os.Getenv("GITOPS_GITLAB_PROJECT")); v != "" {
		c.GitOpsGitLabProject = v
	}
	if v := strings.TrimSpace(os.Getenv("GITOPS_GITLAB_API_URL")); v != "" {
		c.GitOpsGitLabAPIURL = v
	}
	if v := strings.TrimSpace(os.Getenv("GITOPS_POLICY_PATH")); v != "" {
		c.GitOpsPolicyPath = v
	}
	if v := strings.TrimSpace(os.Getenv("GITOPS_DEFAULT_BRANCH")); v != "" {
		c.GitOpsDefaultBranch = v
	}
	if v := strings.TrimSpace(os.Getenv("LOOM_HUD_URL")); v != "" {
		c.HUDBaseURL = v
	}
	if v := strings.TrimSpace(os.Getenv("LOOM_HUD_TOKEN")); v != "" {
		c.HUDToken = v
	}
	if v := strings.TrimSpace(os.Getenv("LOOM_WEAVER_URL")); v != "" {
		c.WeaverURL = v
	}
	if v := strings.TrimSpace(os.Getenv("LOOM_WEAVER_TOKEN")); v != "" {
		c.WeaverToken = v
	}

	// Council stage budgets. Each knob overrides one phase; anything left
	// unset keeps runner.DefaultStageBudgets().
	for name, dst := range map[string]*time.Duration{
		"LOOM_MILLS_COUNCIL_OVERALL_TIMEOUT":   &c.CouncilStages.Overall,
		"LOOM_MILLS_COUNCIL_BRIEF_TIMEOUT":     &c.CouncilStages.Brief,
		"LOOM_MILLS_COUNCIL_REVIEWERS_TIMEOUT": &c.CouncilStages.Reviewers,
		"LOOM_MILLS_COUNCIL_DEBATE_TIMEOUT":    &c.CouncilStages.Debate,
		"LOOM_MILLS_COUNCIL_EDITOR_TIMEOUT":    &c.CouncilStages.Editor,
		"LOOM_MILLS_COUNCIL_JUDGE_TIMEOUT":     &c.CouncilStages.Judge,
		"LOOM_MILLS_COUNCIL_MUTATOR_TIMEOUT":   &c.CouncilStages.Mutator,
	} {
		if d, ok := durationEnv(name); ok {
			*dst = d
		}
	}
	if v := strings.TrimSpace(os.Getenv("LOOM_MILLS_COUNCIL_ASYNC_CONCURRENCY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.CouncilAsyncConcurrency = n
		}
	}
}

// durationEnv parses one LOOM_MILLS_* duration knob. Unset, unparseable, or
// zero values leave the caller's default in place; a negative value is honored
// and means "no bound for this stage" (see runner.StageBudgets).
func durationEnv(name string) (time.Duration, bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d == 0 {
		return 0, false
	}
	return d, true
}

// Validate ensures the resolved config is internally consistent. Called once
// after flag parsing.
func (c *Config) Validate() error {
	if c.DBPath == "" {
		return errors.New("config: --db-path is required")
	}
	if c.PolicyPath == "" {
		return errors.New("config: --policy-path is required")
	}
	if c.HTTPAddr == "" && c.MetricsAddr == "" {
		return errors.New("config: at least one of --listen / --metrics-addr must be set")
	}
	dbDir := filepath.Dir(c.DBPath)
	if dbDir == "" || dbDir == "." {
		return fmt.Errorf("config: db-path %q must include a directory", c.DBPath)
	}
	return nil
}
