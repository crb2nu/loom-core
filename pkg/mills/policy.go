// Package mills is the meta-orchestration layer above weaver/spawn/mentatlab.
// It owns the planning council and the deterministic execution pipeline that
// together turn ongoing intent (roadmap, telemetry, alerts) into a continuous
// flow of merged changes — "CI above CI for agents".
//
// This file is the policy contract: the YAML-loadable, validate-on-startup,
// hot-reloadable rule set that bounds every council and pipeline run.
package mills

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"

	"github.com/crb2nu/loom/pkg/mills/store"
	basepolicy "github.com/crb2nu/loom/pkg/policy"
)

// Policy is the full rule set the operator consults at runtime.
//
// Schema version evolution:
//   - v1: Phase 1-6 fields (Budgets, Council, Pipeline, HumanHandoff).
//   - v2: Adds Squads, Audit, CrossRepo, Debate, Recursion, AdaptivePolicy
//     for the Mills v2 hierarchical-swarm rollout (Phases 7-8). v2 is a
//     superset of v1 — v1 YAML still parses; v2-only sections default
//     to "off" so v1 behavior is preserved when those keys are omitted.
//
// Defaults policy (.loom/93-product-spec-mills-v2-…2026-05-02.md, §"Policy
// file additions"):
//   - Squads off — flip on per Phase 8 default-on rollout.
//   - Audit on but advisory_only=true; flip to blocking in v2.1 once
//     audit_survival_rate proves low-noise (>0.85 over 100 runs).
//   - CrossRepo off — operator opts in after 4 weeks of dogfooding.
//   - Debate off (incident-only opt-in); Recursion off; AdaptivePolicy off.
type Policy struct {
	Gates   GatesPolicy `yaml:"gates,omitempty"`
	Version int         `yaml:"version,omitempty"`
	Enabled *bool       `yaml:"enabled,omitempty"` // nil treated as enabled
	// MaxConcurrentPipelines bounds simultaneous pipeline work admitted by
	// the runner. Nil preserves the compiled default for older documents.
	MaxConcurrentPipelines *int               `yaml:"max_concurrent_pipelines,omitempty"`
	Budgets                Budgets            `yaml:"budgets"`
	Council                CouncilPolicy      `yaml:"council"`
	Pipeline               PipelinePolicy     `yaml:"pipeline"`
	HumanHandoff           HumanHandoffPolicy `yaml:"human_handoff"`
	Squads                 SquadsPolicy       `yaml:"squads,omitempty"`
	Audit                  AuditPolicy        `yaml:"audit,omitempty"`
	CrossRepo              CrossRepoPolicy    `yaml:"cross_repo,omitempty"`
	Debate                 DebatePolicy       `yaml:"debate,omitempty"`
	Recursion              RecursionPolicy    `yaml:"recursion,omitempty"`
	AdaptivePolicy         AdaptivePolicy     `yaml:"adaptive_policy,omitempty"`
	Intake                 IntakePolicy       `yaml:"intake,omitempty"`
	Notify                 NotifyPolicy       `yaml:"notify,omitempty"`
	Workflows              WorkflowsPolicy    `yaml:"workflows,omitempty"`
	SpinningRoom           SpinningRoomPolicy `yaml:"spinning_room,omitempty"`
	// Overseers gates the supervisory agents (groomer/sentinel/foreman);
	// contract + accessors live in policy_overseers.go. Zero value = off.
	Overseers OverseersPolicy `yaml:"overseers,omitempty"`
	// MergeQueue gates the serial merge queue (pkg/mills/mergequeue): when
	// enabled the pipeline's merge stage enqueues its CI-authorized candidate
	// and the queue processor serializes merges per (project, target_branch),
	// rebasing stale heads onto the exact target tip and re-proving them
	// before the merge PUT. Zero value = off, so rollout is a policy flip.
	MergeQueue MergeQueuePolicy `yaml:"merge_queue,omitempty"`
	// Health gates the factory health-plane poller. Omitted is deliberately
	// disabled so upgrading the operator creates no timer or GitLab traffic.
	Health HealthPolicy `yaml:"health,omitempty"`
}

// HealthPolicy configures the read-only GitLab main-pipeline detector.
type HealthPolicy struct {
	Enabled             bool             `yaml:"enabled,omitempty"`
	PollIntervalSeconds int              `yaml:"poll_interval_seconds,omitempty"`
	Project             string           `yaml:"project,omitempty"`
	Ref                 string           `yaml:"ref,omitempty"`
	OperatorBuiltAt     string           `yaml:"operator_built_at,omitempty"`
	Flightdeck          FlightdeckPolicy `yaml:"flightdeck,omitempty"`
	BaseRed             BaseRedPolicy    `yaml:"base_red,omitempty"`
}

const DefaultBaseRedThreshold = 90 * time.Minute

// BaseRedPolicy holds code-producing backlog work while the default branch is
// continuously red. The zero value is off so older policy documents retain
// their exact admission behaviour.
type BaseRedPolicy struct {
	Enabled          bool   `yaml:"enabled,omitempty"`
	Mode             string `yaml:"mode,omitempty"`
	ThresholdMinutes int    `yaml:"threshold_minutes,omitempty"`
}

func (p BaseRedPolicy) Threshold() time.Duration {
	if p.ThresholdMinutes > 0 {
		return time.Duration(p.ThresholdMinutes) * time.Minute
	}
	return DefaultBaseRedThreshold
}

func (p BaseRedPolicy) Enforced() bool { return p.Enabled && p.Mode != "dry-log" }

func (p BaseRedPolicy) Exempt(labels []string) bool {
	for _, label := range labels {
		switch strings.ToLower(strings.TrimSpace(label)) {
		case "ci-fix", "remediation":
			return true
		}
	}
	return false
}

// FlightdeckPolicy gates best-effort Mills lifecycle export. Omitted is off.
type FlightdeckPolicy struct {
	Enabled        bool   `yaml:"enabled,omitempty"`
	Endpoint       string `yaml:"endpoint,omitempty"`
	Token          string `yaml:"token,omitempty"`
	TimeoutSeconds int    `yaml:"timeout_seconds,omitempty"`
	QueueSize      int    `yaml:"queue_size,omitempty"`
}

func (p FlightdeckPolicy) Timeout() time.Duration {
	if p.TimeoutSeconds > 0 {
		return time.Duration(p.TimeoutSeconds) * time.Second
	}
	return 2 * time.Second
}

// PollInterval returns the configured interval, or the conservative default.
func (p HealthPolicy) PollInterval() time.Duration {
	if p.PollIntervalSeconds > 0 {
		return time.Duration(p.PollIntervalSeconds) * time.Second
	}
	return time.Minute
}

// RefName returns the configured health branch.
func (p HealthPolicy) RefName() string {
	if strings.TrimSpace(p.Ref) != "" {
		return strings.TrimSpace(p.Ref)
	}
	return "main"
}

// MergeQueuePolicy configures the serial merge queue. Mirrors the
// Workflows/SpinningRoom optional-section pattern: an omitted `merge_queue:`
// block yields the zero value (Enabled=false), so a pre-queue policy
// hot-reloaded onto a queue-capable binary keeps merges on the direct path.
type MergeQueuePolicy struct {
	Enabled bool `yaml:"enabled,omitempty"`
	// MainRedExternalHoldMinutes bounds a classified external default-branch
	// incident hold. Resolution and clamping live in pkg/policy.
	MainRedExternalHoldMinutes int `yaml:"main_red_external_hold_minutes,omitempty"`
	// MaxDepth bounds a lane's active entries. An enqueue past the bound is
	// refused and the run escalates immediately with reason queue_full —
	// backpressure surfaces instead of silently deepening the queue. Zero
	// selects DefaultMergeQueueMaxDepth.
	MaxDepth int `yaml:"max_depth,omitempty"`
	// RequeueEvictions (shepherd A2, default OFF) re-enqueues a head_moved
	// or ci_timeout eviction ONCE as an external candidate under the
	// observed head, so a green MR whose waiting run is gone still lands
	// through the queue's own proof path (external candidates merge only on
	// a terminal successful pipeline for the head). One hop: a candidate the
	// evictor itself produced is never re-requeued.
	RequeueEvictions bool `yaml:"requeue_evictions,omitempty"`
	// AwaitPipelineMinutes bounds how long a rebased head may wait for a
	// terminal branch pipeline before the candidate is evicted ci_timeout.
	// Zero selects the processor default (45 minutes). Raise it above the
	// observed runner-queue latency: on 2026-09-10 CI queues sat 60–90
	// minutes deep, every candidate timed out, and with requeue_evictions on
	// each re-adoption rebased and launched another full pipeline — a loop
	// that deepened the very queue that caused the timeout.
	AwaitPipelineMinutes int `yaml:"await_pipeline_minutes,omitempty"`
	// SpeculationDepth bounds successors pre-tested behind a running lane
	// head. Zero disables speculation; values are clamped to MaxDepth.
	SpeculationDepth int `yaml:"speculation_depth,omitempty"`
}

// DefaultMergeQueueMaxDepth bounds a merge-queue lane when the policy does
// not say otherwise.
const DefaultMergeQueueMaxDepth = 10

// SpinningRoomPolicy is the GitOps-governed set of model "frames" the operator
// may spin draft plans on from the HUD (Live Beam slice 3 / F2 in
// .loom/brainstorm-mills-steering-preparation-line-2026-07-03.md). Spinning
// turns a brief (roving) into a draft plan + slices via a chosen frame; the
// operator reviews the draft before warping it onto the beam.
//
// The SET of frames is policy (Git-reviewed) — this section; the frame CHOSEN
// per spin is run-scoped and recorded on the resulting draft plan for audit
// (spec_doc + created_by). This is the F2-vs-GitOps-governance line from the
// brainstorm: allowed models are policy, the per-spin pick is steering.
//
// Default-OFF (zero value): an omitted `spinning_room:` block leaves
// POST /api/mills/spin returning 503, matching the Intake/Workflows optional-
// section pattern. Toggling Enabled + editing Frames hot-reloads via the
// policy ConfigMap watcher, so the HUD frame selector reflects Git the moment
// Flux reconciles.
type SpinningRoomPolicy struct {
	Enabled bool `yaml:"enabled,omitempty"`
	// Frames are the allowed spinning frames, each a CouncilAgent-shaped
	// {name, model, backend} triple. Name is the selector key the HUD picks by
	// and the audit label recorded on the plan; Model/Backend drive which
	// council editor client the operator instantiates for the spin.
	Frames []CouncilAgent `yaml:"frames,omitempty"`
	// DefaultFallback is the remote hop inherited by frames without an override.
	DefaultFallback *CouncilAgent `yaml:"default_fallback,omitempty" json:"default_fallback,omitempty"`
	// DefaultPriority stamps the draft plan's warp-beam bucket when the
	// operator doesn't pick one. Default "P2" (see SpinningRoomDefaultPriority).
	DefaultPriority string `yaml:"default_priority,omitempty"`
}

// WorkflowsPolicy gates the durable imperative workflow runtime (Mills
// dynamic-workflows, plan .loom/134 §S6-min). It is the kill switch for the
// Layer-3 runtime: the WorkflowScheduler self-gates on Enabled inside every
// tick, so a default-OFF flag makes the runtime inert even though it is always
// wired into the operator's errgroup.
//
// Default Enabled=false (see Default()) — S6-min merges to main without
// activating anything; only the S1c canary window flips it on, then reverts.
// SubstrateK8sOnly pins imperative spawns to the k8s substrate (the S1c canary
// targets k8s, NOT harvester-vm); reserved for the runtime to consult once
// substrate selection is wired (S6-full). Mirrors the Intake/Squads optional-
// section pattern: an omitted YAML `workflows:` block yields the zero value
// (Enabled=false), so a v1/v2 policy hot-reloaded onto an S6-min binary keeps
// the runtime off.
type WorkflowsPolicy struct {
	Enabled          bool `yaml:"enabled,omitempty"`
	SubstrateK8sOnly bool `yaml:"substrate_k8s_only,omitempty"`
	// MaxRunMinutes bounds an imperative run's wall clock. A run still
	// `running` past the bound is terminalized as error by the scheduler
	// (the settle then releases its budget reservation and escalates the
	// item), so a wedged run can never hold quiescence — and future canary
	// windows — hostage. Zero selects the default (180m).
	MaxRunMinutes int `yaml:"max_run_minutes,omitempty"`
}

// defaultWorkflowMaxRunMinutes bounds imperative runs when the policy does not
// say otherwise: generous against slow agent work, small against a wedge.
const defaultWorkflowMaxRunMinutes = 180

// WorkflowMaxRunAge returns the effective imperative-run wall-clock bound.
func (p *Policy) WorkflowMaxRunAge() time.Duration {
	if p == nil || p.Workflows.MaxRunMinutes <= 0 {
		return defaultWorkflowMaxRunMinutes * time.Minute
	}
	return time.Duration(p.Workflows.MaxRunMinutes) * time.Minute
}

// NotifyPolicy controls external notification hooks (Slice 3a / .loom/126 W2.1).
// Two independent sinks, both disabled by default and both fired on merge:
//   - a generic JSON webhook (Slack-compatible) — set webhook_url to opt in;
//   - the in-cluster agent_context handoff inbox — set handoff_inbox: true.
type NotifyPolicy struct {
	WebhookURL        string `yaml:"webhook_url,omitempty"`
	WebhookTimeoutSec int    `yaml:"webhook_timeout_seconds,omitempty"` // default 10
	MRBaseURL         string `yaml:"mr_base_url,omitempty"`             // optional MR link prefix
	OnlyAutonomous    bool   `yaml:"only_autonomous,omitempty"`         // future: gate on no-human-touch
	// HandoffInbox, when true, posts a "Mills merged X" record to the
	// agent_context handoff inbox on every merge (agent_handoff_create over
	// the MCP hub) — the in-cluster alternative to webhook_url, no external
	// dependency. Requires the operator's MCP hub to be reachable.
	HandoffInbox bool `yaml:"handoff_inbox,omitempty"`
	// HandoffTarget is the target_agent_id the merge handoff is addressed to
	// (recalled via agent_handoff_inbox{agent_id}). Default "mills-merges".
	HandoffTarget string `yaml:"handoff_target,omitempty"`
}

// IntakePolicy controls external backlog sources. v1 ships only the
// GitLab issue importer; future intake sources (Loki errors, roadmap
// pulls, canary autopilot) live as additional sub-fields here.
type IntakePolicy struct {
	GitLab           GitLabIntake           `yaml:"gitlab,omitempty"`
	CanaryGC         CanaryGCPolicy         `yaml:"canary_gc,omitempty"`
	CanaryAutopilot  CanaryAutopilotPolicy  `yaml:"canary_autopilot,omitempty"`
	PlanSliceEmitter PlanSliceEmitterPolicy `yaml:"plan_slice_emitter,omitempty"`
	Takeup           TakeupPolicy           `yaml:"takeup,omitempty"`
}

// TakeupPolicy controls the take-up motion (Live Beam slice 2): a reconciler
// that trues Plan Store lifecycle state to GitLab MR reality — slices whose
// MR merged advance to "merged" (and their emitted backlog items close),
// slices whose MR closed without merging get an orphan decision note, and
// plans whose slices are all merged roll forward to "merged".
//
// FAIL-CLOSED like the plan-slice emitter: with Namespace empty the
// reconciler is inert even when Enabled. Default-OFF (zero value); toggling
// Enabled takes effect on the next operator start (importer-style snapshot).
type TakeupPolicy struct {
	Enabled             bool   `yaml:"enabled,omitempty"`
	Namespace           string `yaml:"namespace,omitempty"`             // REQUIRED to reconcile; empty = inert (fail-closed)
	Project             string `yaml:"project,omitempty"`               // default: operator GITLAB_PROJECT
	PollIntervalSeconds int    `yaml:"poll_interval_seconds,omitempty"` // default 300 (5min)
	// TickTimeoutSeconds bounds a single reconcile pass so a stalled hub or
	// GitLab call can't wedge the whole loop (the 2026-07-03 "enabled but
	// silent" incident). Default 120 (2min). Not clamped to the poll interval
	// — an operator that wants a longer scan can raise it.
	TickTimeoutSeconds int `yaml:"tick_timeout_seconds,omitempty"`
}

// PlanSliceEmitterPolicy controls the Plan-Store → Mills backlog bridge
// (.loom/163 S2). When Enabled, the operator polls the agent-context Plan
// Store for pending slices belonging to plans in the configured project +
// namespace and emits one deterministic, plan-linked BacklogItem per slice —
// so planning feeds the autonomous loop with real work instead of only
// hand-filed GitLab issues or no-op canaries.
//
// FAIL-CLOSED: with Namespace empty the emitter is inert even when Enabled,
// so it never scoops up arbitrary planning-scaffold slices. Point it at the
// namespace that holds mills-eligible plans to opt a plan family in. Default-
// OFF (zero value), matching the other intake sub-sections. Toggling Enabled
// takes effect on the next operator start (the importer-style snapshot
// pattern), so flip it alongside a deployment pod-checksum bump.
type PlanSliceEmitterPolicy struct {
	Enabled             bool   `yaml:"enabled,omitempty"`
	Namespace           string `yaml:"namespace,omitempty"`             // REQUIRED to emit; empty = inert (fail-closed)
	Project             string `yaml:"project,omitempty"`               // default: operator GITLAB_PROJECT
	PollIntervalSeconds int    `yaml:"poll_interval_seconds,omitempty"` // default 300 (5min)
	TickTimeoutSeconds  int    `yaml:"tick_timeout_seconds,omitempty"`  // default 120 (2min)
	ReadyPhase          string `yaml:"ready_phase,omitempty"`           // slice phase to emit; default "pending"
	Label               string `yaml:"label,omitempty"`                 // default "mills-from-plan-slice"
	Priority            string `yaml:"priority,omitempty"`              // default "P2"
}

// CanaryAutopilotPolicy controls the daily heartbeat-canary scheduler
// (.loom/126 Wave 1 / A3-sustain). Without it the north-star
// (autonomous_merges_24h) only ticks when a human runs
// `loom mills pipelines canary`, so the loop drops to 0 merges on any day
// nobody hand-feeds it (observed 2026-06-26 ×0). When Enabled, the operator
// enqueues + starts one deterministic heartbeat canary per ScheduleCron match,
// honoring the same 24h dedupe guard the manual path uses — so a still-in-flight
// or escalated canary suppresses the autopilot enqueue rather than piling on.
//
// Default-OFF (zero value), matching the Intake/Workflows optional-section
// pattern: an omitted `canary_autopilot:` block keeps the operator's prior
// behavior. Flip via configmap-policy.yaml after the operator image carrying
// this reader is deployed; the ConfigMap hot-reloads via fsnotify.
type CanaryAutopilotPolicy struct {
	Enabled      bool   `yaml:"enabled,omitempty"`
	ScheduleCron string `yaml:"schedule_cron,omitempty"` // default "0 9 * * *" (daily 09:00 UTC)
	Priority     string `yaml:"priority,omitempty"`      // default "P3"
	FixturePath  string `yaml:"fixture_path,omitempty"`  // default "testdata/mills-canary/heartbeat.md"
}

// CanaryGCPolicy controls the stale-escalated-canary sweeper (Slice
// "Stale-canary GC" in plan 43). Without GC, escalated canaries block
// new mills-canary enqueues forever per commit 2fcc705a — leaving the
// operator starved once a handful of canaries hit the bad-cluster
// epoch. Default disabled until the operator confirms supply-side
// fixes are healthy enough that we want fresh canaries flowing again.
type CanaryGCPolicy struct {
	Enabled         bool `yaml:"enabled,omitempty"`
	StaleAfterHours int  `yaml:"stale_after_hours,omitempty"` // default 48
	IntervalMinutes int  `yaml:"interval_minutes,omitempty"`  // default 60
	DryRun          bool `yaml:"dry_run,omitempty"`
}

// GitLabIntake configures the GitLab issue importer (Slice 1a). The
// importer polls the configured GitLab projects on PollIntervalSeconds
// and creates a backlog item for each open issue carrying the
// EligibleLabel that the operator hasn't already imported. Disabled by
// default — opt in via configmap policy.intake.gitlab.enabled: true.
// Projects is fail-closed: only listed repositories are polled; an empty
// list preserves legacy behavior by polling only the operator home project.
type GitLabIntake struct {
	Enabled             bool     `yaml:"enabled,omitempty"`
	EligibleLabel       string   `yaml:"eligible_label,omitempty"`        // default "mills-eligible"
	PollIntervalSeconds int      `yaml:"poll_interval_seconds,omitempty"` // default 300 (5min)
	DefaultPriority     string   `yaml:"default_priority,omitempty"`      // default "P2"
	Projects            []string `yaml:"projects,omitempty"`
}

// Budgets holds per-tier spend limits.
type Budgets struct {
	Council  BudgetLimits `yaml:"council"`
	Pipeline BudgetLimits `yaml:"pipeline"`
	// ThroughputGuardrail bounds fleet-level shift KPIs. Zero values select
	// the conservative compiled defaults through Effective().
	ThroughputGuardrail ThroughputGuardrailThresholds `yaml:"throughput_guardrail,omitempty"`
	// ModelPrices lets policy price a model the compiled tables do not know
	// (a new frontier tier the day it ships, a gateway alias) or correct a
	// list price without an image deploy. Keyed by the exact model id the
	// policy routes ("claude-fable-5-1", "gpt-6-astra", "or/kimi-k3"). A row
	// here wins over the compiled table for the same id. Pricing a model is
	// not cosmetic: an unpriced council/spin model loses the per-attempt
	// ceiling and charges its whole run reservation on a timeout.
	ModelPrices map[string]ModelPriceOverride `yaml:"model_prices,omitempty"`
}

// ThroughputGuardrailThresholds are advisory shift-report limits. They do not
// stop admission; they make degraded throughput explicit to operators.
type ThroughputGuardrailThresholds struct {
	MaxEscalationRate           float64 `yaml:"max_escalation_rate,omitempty"`
	MaxCostPerMergedPipelineUSD float64 `yaml:"max_cost_per_merged_pipeline_usd,omitempty"`
	MaxScopeQueueAgeSeconds     float64 `yaml:"max_scope_queue_age_seconds,omitempty"`
	MaxStarvedQueues            int     `yaml:"max_starved_queues,omitempty"`
}

var DefaultThroughputGuardrailThresholds = ThroughputGuardrailThresholds{
	MaxEscalationRate: .25, MaxCostPerMergedPipelineUSD: 5,
	MaxScopeQueueAgeSeconds: 6 * 60 * 60, MaxStarvedQueues: 0,
}

// Effective fills omitted thresholds with defaults. MaxStarvedQueues is
// intentionally allowed to be zero: any starvation reservation is unhealthy.
func (t ThroughputGuardrailThresholds) Effective() ThroughputGuardrailThresholds {
	d := DefaultThroughputGuardrailThresholds
	if t.MaxEscalationRate > 0 {
		d.MaxEscalationRate = t.MaxEscalationRate
	}
	if t.MaxCostPerMergedPipelineUSD > 0 {
		d.MaxCostPerMergedPipelineUSD = t.MaxCostPerMergedPipelineUSD
	}
	if t.MaxScopeQueueAgeSeconds > 0 {
		d.MaxScopeQueueAgeSeconds = t.MaxScopeQueueAgeSeconds
	}
	if t.MaxStarvedQueues > 0 {
		d.MaxStarvedQueues = t.MaxStarvedQueues
	}
	return d
}

// ModelPriceOverride is one policy-supplied price row. USD per million
// tokens. Provider selects the price table it overlays: "anthropic"
// (Messages API: cached_input = cache read, cache_write optional and
// defaulting to 1.25× input), "openai" (Responses / LiteLLM oa/ rates), or
// "openrouter" (or/ ceilings). A missing cached_input charges cache reads at
// the full input rate — conservative by construction.
type ModelPriceOverride struct {
	Provider              string  `yaml:"provider"`
	InputPerMillion       float64 `yaml:"input_per_million"`
	CachedInputPerMillion float64 `yaml:"cached_input_per_million,omitempty"`
	CacheWritePerMillion  float64 `yaml:"cache_write_per_million,omitempty"`
	OutputPerMillion      float64 `yaml:"output_per_million"`
}

// modelPriceProviders mirrors clients.ModelPriceProviders; kept as a literal
// here so the policy package stays free of a clients import.
var modelPriceProviders = map[string]bool{"anthropic": true, "openai": true, "openrouter": true}

// ModelPriceOverrides returns a copy of budgets.model_prices (nil when none).
func (p *Policy) ModelPriceOverrides() map[string]ModelPriceOverride {
	if p == nil || len(p.Budgets.ModelPrices) == 0 {
		return nil
	}
	out := make(map[string]ModelPriceOverride, len(p.Budgets.ModelPrices))
	for k, v := range p.Budgets.ModelPrices {
		out[k] = v
	}
	return out
}

func validateModelPrices(rows map[string]ModelPriceOverride) error {
	for id, row := range rows {
		if !validModelToken(id) {
			return fmt.Errorf("budgets.model_prices: %q is not a valid model id", id)
		}
		provider := strings.ToLower(strings.TrimSpace(row.Provider))
		if !modelPriceProviders[provider] {
			return fmt.Errorf("budgets.model_prices[%s].provider must be one of anthropic, openai, openrouter; got %q", id, row.Provider)
		}
		if row.InputPerMillion <= 0 || row.OutputPerMillion <= 0 {
			return fmt.Errorf("budgets.model_prices[%s]: input_per_million and output_per_million must be > 0", id)
		}
		if row.CachedInputPerMillion < 0 || row.CacheWritePerMillion < 0 {
			return fmt.Errorf("budgets.model_prices[%s]: cache rates must be >= 0", id)
		}
	}
	return nil
}

// BudgetLimits captures one tier's caps. Zero values disable that specific cap.
type BudgetLimits struct {
	MaxUSDPerRun float64 `yaml:"max_usd_per_run"`
	// MaxUSDPerDay caps METERED spend only: dollars an API account is billed.
	// Subscription turns (Claude Code / Codex under the cluster OAuth
	// accounts) and local inference are excluded — see store.BillingClass.
	MaxUSDPerDay float64 `yaml:"max_usd_per_day"`
	// MaxSubscriptionUSDPerDay optionally caps the list-price equivalent of
	// subscription turns per rolling day, so a runaway harness stops before
	// the vendor's rate limits do. Zero (the default) leaves it uncapped.
	MaxSubscriptionUSDPerDay float64 `yaml:"max_subscription_usd_per_day,omitempty"`
	MaxConcurrentRuns        int     `yaml:"max_concurrent_runs,omitempty"`
	MaxRunsPerDay            int     `yaml:"max_runs_per_day,omitempty"`
}

// CouncilPolicy bounds the planning tier.
type CouncilPolicy struct {
	ScheduleCron           string          `yaml:"schedule_cron"`
	Triggers               CouncilTriggers `yaml:"triggers"`
	Ensemble               CouncilEnsemble `yaml:"ensemble"`
	ArtifactsBranch        string          `yaml:"artifacts_branch"`
	ArtifactsMergeStrategy string          `yaml:"artifacts_merge_strategy"`

	// RequireRoadmapIntents blocks a council run whose brief was marked
	// intents_missing (empty canonical roadmap_intents store). *bool so an
	// omitted key defaults to TRUE — this is a fail-closed guardrail; opt OUT
	// with `require_roadmap_intents: false`. Hot-reloads via PolicyManager.
	RequireRoadmapIntents *bool `yaml:"require_roadmap_intents,omitempty"`

	// Dedup bounds the proposal-authoring guards that suppress work the mill
	// has already served.
	Dedup CouncilDedupPolicy `yaml:"dedup,omitempty"`

	// Sources bounds the optional demand inputs the brief assembler pulls
	// from outside the canonical store.
	Sources CouncilSourcesPolicy `yaml:"sources,omitempty"`
}

// CouncilSourcesPolicy groups the brief's optional demand inputs — the ones
// that reach outside the canonical store and can therefore be unavailable.
type CouncilSourcesPolicy struct {
	FactoryExhaust CouncilFactoryExhaustPolicy `yaml:"factory_exhaust,omitempty"`
}

// CouncilFactoryExhaustPolicy controls sourcing council demand from the
// factory's own exhaust: the open `flaky-test` and `audit-digest` issues the
// mill filed against itself and nobody triaged.
//
// Production policy lives in `platform/gitops/k3s/mills/configmap-policy.yaml`;
// editing it MUST be paired with a `loom.flexinfer.ai/policy-checksum` bump on
// `platform/gitops/k3s/mills/deployment.yaml` — fsnotify misses the Kubernetes
// `..data` symlink swap, so the operator will not otherwise reload. The
// defaults here need no ConfigMap change to ship: ParsePolicy is lenient, so an
// old binary ignores these keys and a new binary reads an old ConfigMap.
type CouncilFactoryExhaustPolicy struct {
	// Enabled turns the brief section on. *bool so an omitted key defaults to
	// TRUE — this only ADDS evidence to a brief (the council still decides
	// what to propose, under the unchanged dedup and grounding guards), so it
	// is on unless someone opts out with `enabled: false`. Inert regardless
	// when the operator has no GitLab client.
	Enabled *bool `yaml:"enabled,omitempty"`

	// LookbackHours bounds how far back the exhaust snapshot reaches. Zero
	// substitutes the default (336 = 14 days), matching merged-work grounding
	// so both corpora describe the same era.
	LookbackHours int `yaml:"lookback_hours,omitempty"`

	// MaxItems caps how many exhaust items reach the brief. Zero substitutes
	// the default (10). The brief has a byte budget and roadmap intents
	// outrank self-maintenance, so this stays small on purpose.
	MaxItems int `yaml:"max_items,omitempty"`
}

// CouncilDedupPolicy groups the council's proposal-authoring suppression
// guards. Backlog and plan-lane dedup are unconditional; merged-work grounding
// is the one with an off switch, because it depends on GitLab being reachable.
type CouncilDedupPolicy struct {
	MergedWork CouncilMergedWorkPolicy `yaml:"merged_work,omitempty"`
}

// CouncilMergedWorkPolicy controls grounding proposals against merge requests
// the target branch has already taken.
//
// Production policy lives in `platform/gitops/k3s/mills/configmap-policy.yaml`;
// editing it MUST be paired with a `loom.flexinfer.ai/policy-checksum` bump on
// `platform/gitops/k3s/mills/deployment.yaml` — fsnotify misses the Kubernetes
// `..data` symlink swap, so the operator will not otherwise reload. The
// defaults here need no ConfigMap change to ship: ParsePolicy is lenient, so an
// old binary ignores these keys and a new binary reads an old ConfigMap.
type CouncilMergedWorkPolicy struct {
	// Enabled turns the grounding pass on. *bool so an omitted key defaults to
	// TRUE — a council proposing already-merged work burns escalation attempts
	// on empty diffs, so the guard is on unless someone opts out with
	// `enabled: false`. Inert regardless when the operator has no GitLab client.
	Enabled *bool `yaml:"enabled,omitempty"`

	// LookbackHours bounds the merged-MR corpus. Zero substitutes the default
	// (336 = 14 days).
	LookbackHours int `yaml:"lookback_hours,omitempty"`
}

// CouncilTriggers controls when a council run is initiated.
type CouncilTriggers struct {
	OnRoadmapChange   bool `yaml:"on_roadmap_change"`
	OnIncident        bool `yaml:"on_incident"`
	OnMergeDriftHours int  `yaml:"on_merge_drift_hours,omitempty"`
}

// CouncilEnsemble names the editor + reviewer + judge agents the council uses.
type CouncilEnsemble struct {
	Editor CouncilAgent `yaml:"editor"`
	// EditorFallbackModel optionally pins the LOCAL flexinfer model used by
	// the editor's per-run fallback (and the no-API-key degrade path) when
	// Editor.Backend is a remote provider. A remote frontier model id is
	// never deployable on the flexinfer tier, so the fallback must not
	// inherit it. Empty resolves to the flexinfer client's weaver chain.
	EditorFallbackModel string `yaml:"editor_fallback_model,omitempty"`
	// EditorFallback optionally names a REMOTE cross-vendor editor tried
	// between the primary and the local flexinfer fallback when Editor.Backend
	// is a frontier provider: anthropic → openai (or the reverse). It exists
	// because the two failure modes a primary hits most — a billing hold
	// ("credit balance is too low", 2026-09-07 ×3 runs) or a provider outage
	// — are vendor-scoped, so the other vendor is the fallback most likely to
	// still produce a frontier-quality plan. Omitted resolves through
	// EditorCrossVendorFallback (an anthropic primary defaults to
	// DefaultEditorCrossVendorFallback; any other primary has no default).
	// `backend: none` disables the cross-vendor hop explicitly. The hop is
	// skipped at build time when the vendor's API key is absent, so a policy
	// naming it never hard-fails a deployment without that key.
	EditorFallback CouncilAgent   `yaml:"editor_fallback,omitempty"`
	Reviewers      []CouncilAgent `yaml:"reviewers"`
	Judge          CouncilAgent   `yaml:"judge,omitempty"`
}

// DefaultEditorCrossVendorFallback is the remote editor an anthropic-backed
// council editor falls back to when policy leaves editor_fallback unset. gpt-5.5
// is priced in pkg/llmpricing (an unpriced model loses the per-attempt ceiling)
// and is already the Spinning Room "mule" frame, so it carries no new pricing
// or credential surface.
var DefaultEditorCrossVendorFallback = CouncilAgent{Model: "gpt-5.5", Backend: "openai"}

// editorBackendVendor collapses the accepted remote editor backend spellings
// into a vendor label ("anthropic", "openai") so the cross-vendor rule can
// compare like with like. Local/unknown backends return "".
func editorBackendVendor(backend string) string {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "anthropic", "claude":
		return "anthropic"
	case "openai", "openai-responses":
		return "openai"
	case "openrouter":
		return "openrouter"
	default:
		return ""
	}
}

// EditorCrossVendorFallback returns the remote editor the council editor
// chain tries between the primary and the local flexinfer fallback, and
// whether one applies. The explicit policy pin wins; `backend: none` opts out;
// an omitted pin defaults an anthropic primary to
// DefaultEditorCrossVendorFallback and leaves every other primary without a
// cross-vendor hop. A pin whose vendor matches the primary's is rejected here
// (same billing account, same outage — it would only double the failure) so
// callers never build an anthropic→anthropic chain.
func (e CouncilEnsemble) EditorCrossVendorFallback() (CouncilAgent, bool) {
	primaryVendor := editorBackendVendor(e.Editor.Backend)
	if primaryVendor == "" {
		return CouncilAgent{}, false
	}
	pin := e.EditorFallback
	if strings.EqualFold(strings.TrimSpace(pin.Backend), "none") {
		return CouncilAgent{}, false
	}
	if strings.TrimSpace(pin.Model) == "" {
		if primaryVendor != "anthropic" {
			return CouncilAgent{}, false
		}
		pin = DefaultEditorCrossVendorFallback
	}
	if v := editorBackendVendor(pin.Backend); v == "" || v == primaryVendor {
		return CouncilAgent{}, false
	}
	return pin, true
}

// SpinningRoomFallback resolves frame override, room default, then council policy.
// Council defaults are evaluated against the frame's vendor so mixed rooms work.
func (p *Policy) SpinningRoomFallback(frame CouncilAgent) (CouncilAgent, bool) {
	if p == nil {
		return CouncilAgent{}, false
	}
	pin := p.Council.Ensemble.EditorFallback
	if p.SpinningRoom.DefaultFallback != nil {
		pin = *p.SpinningRoom.DefaultFallback
	}
	if frame.Fallback != nil {
		pin = *frame.Fallback
	}
	return (CouncilEnsemble{Editor: frame, EditorFallback: pin}).EditorCrossVendorFallback()
}

func validateEditorFallback(fb CouncilAgent, primary, path string) error {
	backend, model := strings.ToLower(strings.TrimSpace(fb.Backend)), strings.TrimSpace(fb.Model)
	switch {
	case backend == "" && model == "":
		return nil
	case backend == "none":
		if model != "" {
			return fmt.Errorf("%s: backend none takes no model", path)
		}
	case editorBackendVendor(backend) == "":
		return fmt.Errorf("%s.backend %q must be anthropic, claude, openai, openai-responses, openrouter, or none", path, fb.Backend)
	case model == "":
		return fmt.Errorf("%s.model is required when editor_fallback.backend is set", path)
	case editorBackendVendor(backend) == editorBackendVendor(primary):
		return fmt.Errorf("%s.backend %q is the same vendor as editor.backend %q; a cross-vendor fallback must name the other provider", path, fb.Backend, primary)
	}
	return nil
}

// CouncilAgent identifies one council participant.
type CouncilAgent struct {
	// Fallback overrides the spinning-room remote hop; nil inherits policy.
	Fallback *CouncilAgent `yaml:"fallback,omitempty" json:"fallback,omitempty"`
	Name     string        `yaml:"name,omitempty"`
	Model    string        `yaml:"model"`
	Backend  string        `yaml:"backend"`
}

// PipelinePolicy bounds the execution tier.
type PipelinePolicy struct {
	DefaultTemplate   string          `yaml:"default_template"`
	PerLabelOverrides []LabelOverride `yaml:"per_label_overrides,omitempty"`
	ProtectedPaths    []string        `yaml:"protected_paths,omitempty"`

	// ProtectedPathsPerRepo replaces ProtectedPaths for pipeline runs targeting
	// a specific repo
	// (BacklogItem.TargetProject — the cross-repo lane). Keys are project
	// identifiers, bucket-qualified ("services/flexdeck") or bare
	// ("flexdeck"), matched case-insensitively via store.SameRepo; values are
	// doublestar globs evaluated against repo-relative paths in the TARGET
	// repo's layout. An explicitly empty list therefore means that the target
	// has no protected paths. Known targets without an entry inherit the global
	// list; unknown foreign targets fail closed.
	// An item with an empty TargetProject (home-repo item) never consults the
	// overlay. Hot-reloads via PolicyManager like the rest of this section;
	// older operator images lenient-decode past the unknown key.
	ProtectedPathsPerRepo map[string][]string `yaml:"protected_paths_per_repo,omitempty"`

	// PerRepoOverrides narrows execution policy for a target repository. An
	// absent entry preserves the global/item/label behaviour. Boolean pointers
	// distinguish an omitted setting from an explicit false value.
	PerRepoOverrides map[string]RepoExecutionOverride `yaml:"per_repo_overrides,omitempty"`

	Retry                  RetryPolicy   `yaml:"retry"`
	AutoRevertOnRegression bool          `yaml:"auto_revert_on_regression,omitempty"`
	CIWatch                CIWatchPolicy `yaml:"ci_watch,omitempty"`

	// RankerEnabled flips the dispatch order from the store's
	// FIFO-within-priority to the heuristic DispatchRanker (W3.2): queued
	// items are reordered by expected merge probability (priority, recent
	// escalation count, age) so the limited per-tick dispatch slots go to the
	// work most likely to merge. Default-off (unset = FIFO-within-priority);
	// the ranker is a strict refinement, so flipping it on never reorders
	// across priority bands. Hot-reloads via PolicyManager.
	RankerEnabled             bool    `yaml:"ranker_enabled,omitempty"`
	RankerTimeoutMilliseconds int     `yaml:"ranker_timeout_milliseconds,omitempty"`
	RankerMaxCandidates       int     `yaml:"ranker_max_candidates,omitempty"`
	RankerMaxCost             float64 `yaml:"ranker_max_cost,omitempty"`

	// SerializeOverlappingScopes gates the reconciler's scope-overlap
	// dispatch guard (pkg/mills/scope_overlap.go): a queued item defers
	// while a RUNNING item's slice envelope intersects its own, so sibling
	// items that declare files in the same package produce sequential MRs
	// instead of a mutually-conflicting pile (2026-07-09: seven open
	// failure-classification MRs, zero mergeable). *bool so an omitted key
	// defaults to true — this is a correctness guard; opt OUT with
	// serialize_overlapping_scopes: false. Hot-reloads via PolicyManager.
	SerializeOverlappingScopes *bool               `yaml:"serialize_overlapping_scopes,omitempty"`
	ScopeFairness              ScopeFairnessPolicy `yaml:"scope_fairness,omitempty"`

	// StageSubstrate selects which devbox backend each spawn-driven stage
	// runs against (harvester-vm Slice 2). Keys are stage IDs; values are
	// backend names. Empty / unset entries fall back to SubstrateDefault.
	// Only spawn-driven stages are configurable here (plan_slice, research,
	// implement, tests, pr_self_review); GitLabWorker-driven stages (mr,
	// ci_watch, merge, cleanup) are HTTP-only and have no sandbox.
	//
	//	pipeline:
	//	  stage_substrate:
	//	    plan_slice:     k8s
	//	    research:       k8s
	//	    implement:      harvester-vm
	//	    tests:          harvester-vm
	//	    pr_self_review: k8s
	//
	// Spec: .loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md
	StageSubstrate map[string]string `yaml:"stage_substrate,omitempty"`

	// StageAgents selects which spawn agent (harness/vendor) each
	// spawn-driven stage runs on, so an expensive stage like pr_self_review
	// can run on a cheaper agent than implement by policy alone. Keys are
	// stage IDs; values are agent ids from the closed spawn AgentType
	// vocabulary (claude-code, codex, gemini — see StageAgentValuesValid).
	// Empty / unset entries fall back to the operator's built-in default
	// agent (AgentDefault).
	//
	// Precedence when the operator resolves the effective agent for a stage
	// (highest first):
	//   1. LOOM_MILLS_SPAWN_AGENT env — the global break-glass override.
	//      When set it wins for EVERY stage regardless of this map (the
	//      auth-outage failover knob); it stays the break-glass.
	//   2. this map's per-stage entry.
	//   3. AgentDefault ("claude-code").
	//
	// Only the SpawnWorker-driven stages are configurable here (plan_slice,
	// implement, pr_self_review — see StageAgentKeysValid). research
	// (WeaverWorker / FlexInfer) and tests (DevboxWorker) do not consume an
	// agent selection, so — unlike StageSubstrate — they are NOT valid keys.
	//
	//	pipeline:
	//	  stage_agents:
	//	    pr_self_review: gemini   # cheaper reviewer than the default
	//
	// This field is the mechanism only; flipping production policy is a
	// separate gitops change (it needs the deployment policy-checksum bump).
	StageAgents map[string]string `yaml:"stage_agents,omitempty"`

	// AgentRouting routes individual backlog items to a harness+model by
	// label / priority / slice file paths, so the fleet can run claude-code
	// and codex implementers SIMULTANEOUSLY instead of choosing one harness
	// globally per stage. It sits ABOVE StageAgents in the precedence chain
	// and is inert when the block is absent. See agent_routing.go for the
	// shape, the resolution rules, and Policy.ResolveAgentRoute.
	AgentRouting AgentRoutingPolicy `yaml:"agent_routing,omitempty"`

	// AutoRequeue configures the reconciler's bounded auto-requeue sweep: the
	// escalated→queued retry that runs each tick without a human hitting the
	// requeue endpoint. It is default-ON with conservative caps (see
	// AutoRequeuePolicy) because ~53% of runs escalate for retryable classes
	// (infra/transient) that a human otherwise has to requeue by hand. The
	// caps (per-item, per-day, run budget, cooldown, class eligibility, and
	// active-external-incident gating) are the guard rails.
	AutoRequeue AutoRequeuePolicy `yaml:"auto_requeue,omitempty"`

	// SpawnBreaker configures the consecutive-spawn-infra circuit breaker that
	// HOLDS new dispatch while the agent vendor is down (see
	// SpawnBreakerPolicy). Default-ON: per-run spawn classification already
	// makes each individual failure retryable-infra, but nothing stopped the
	// reconciler from feeding the whole queue into the outage one run at a
	// time (issue #382, the 2026-07-25 codex websocket 503s).
	SpawnBreaker SpawnBreakerPolicy `yaml:"spawn_breaker,omitempty"`

	// AutonomyHoldMinutes bounds transient capability holds (0 uses 30m; max 24h).
	AutonomyHoldMinutes int `yaml:"autonomy_hold_minutes,omitempty"`

	// ScopeAmendment configures the runtime scope auto-amendment the runner
	// applies when the ONLY failing gate at post_implement_gate is `scope`
	// (see ScopeAmendmentPolicy). Default-ON.
	ScopeAmendment ScopeAmendmentPolicy `yaml:"scope_amendment,omitempty"`

	// TestsBaselineOracle (shepherd B3, default OFF) re-runs the FAILED
	// subset of a tests-stage gate against the bare main checkout: a check
	// that also fails without the change is an environment/baseline problem
	// and classifies as infrastructure instead of burning code-class
	// attempts. Costs one extra devbox call, only on failure.
	TestsBaselineOracle bool `yaml:"tests_baseline_oracle,omitempty"`

	// StageModels selects the vendor-native LLM model id each spawn-driven
	// stage's agent CLI runs — e.g. "gpt-5.6-terra" for the codex implementer
	// and "gpt-5.6-sol" for the codex planner. It is orthogonal to StageAgents:
	// StageAgents picks the VENDOR/harness (claude-code|codex|gemini),
	// StageModels picks the MODEL that vendor's CLI runs. Keys are stage IDs
	// from the same closed set as StageAgents (plan_slice, implement,
	// pr_self_review — see StageModelKeysValid).
	//
	// Values are FREE-FORM vendor-native model ids (unlike StageAgents' closed
	// vendor vocabulary): the operator does not carry every provider's model
	// catalog, so the only validation is a non-empty "sane token" shape (see
	// validModelToken). The agent CLI is the real authority — it rejects an
	// unknown id at run time.
	//
	// Precedence when the operator resolves the effective model for a stage
	// (highest first), mirroring StageAgents:
	//   1. LOOM_MILLS_SPAWN_MODEL env — the global break-glass override. When
	//      set it wins for EVERY stage regardless of this map.
	//   2. this map's per-stage entry.
	//   3. vendor default — the empty string is sent, so the HUD spawn server
	//      applies its own default (SPAWN_CODEX_MODEL env / resolveCodexModel's
	//      compiled default for codex). Non-codex agents (claude-code, gemini)
	//      have no CLI model knob today: a set model is ignored with a wiring
	//      log on the spawn server, never an error.
	//
	//	pipeline:
	//	  stage_models:
	//	    implement:  gpt-5.6-terra   # codex implementer
	//	    plan_slice: gpt-5.6-sol     # codex planner
	//
	// This field is the mechanism only; flipping production policy is a
	// separate gitops change (it needs the deployment policy-checksum bump).
	StageModels map[string]string `yaml:"stage_models,omitempty"`
}

type CIWatchPolicy struct {
	FlakyJobs           []string `yaml:"flaky_jobs,omitempty"`
	MaxWallClockMinutes int      `yaml:"max_wall_clock_minutes,omitempty"`
}

// MaxWallClock resolves the independent ceiling for watching one CI pipeline.
// Zero keeps the backwards-compatible three-session (90 minute) default.
func (p CIWatchPolicy) MaxWallClock() time.Duration {
	minutes := p.MaxWallClockMinutes
	if minutes <= 0 {
		minutes = 90
	}
	return time.Duration(minutes) * time.Minute
}

// SerializeOverlappingScopesEnabled resolves the *bool with its default-on
// semantics (nil = enabled). Callers must use this instead of reading the
// field so the default rule stays in one place.
func (p PipelinePolicy) SerializeOverlappingScopesEnabled() bool {
	return p.SerializeOverlappingScopes == nil || *p.SerializeOverlappingScopes
}

// ScopeFairnessPolicy bounds writer-preference for repeatedly scope-blocked
// backlog items. It is default-on because it is the liveness half of scope
// serialization; an explicit enabled:false restores the legacy admission path.
// Reservations only delay equal or lower priority work while their owner is
// otherwise eligible and clear of active scopes. Inactivity preserves aging;
// the existing maximum hold duration still releases and resets a reservation.
type ScopeFairnessPolicy struct {
	Enabled              *bool `yaml:"enabled,omitempty"`
	DeferralThreshold    int   `yaml:"deferral_threshold,omitempty"`
	AgeThresholdHours    int   `yaml:"age_threshold_hours,omitempty"`
	ReservationHoldHours int   `yaml:"reservation_hold_hours,omitempty"`
}

func (p ScopeFairnessPolicy) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }
func (p ScopeFairnessPolicy) Deferrals() int {
	if p.DeferralThreshold > 0 {
		return p.DeferralThreshold
	}
	return 50
}
func (p ScopeFairnessPolicy) Age() time.Duration {
	if p.AgeThresholdHours > 0 {
		return time.Duration(p.AgeThresholdHours) * time.Hour
	}
	return 6 * time.Hour
}
func (p ScopeFairnessPolicy) Hold() time.Duration {
	if p.ReservationHoldHours > 0 {
		return time.Duration(p.ReservationHoldHours) * time.Hour
	}
	return 2 * time.Hour
}

// Auto-requeue defaults. Conservative on purpose: a retryable escalation
// gets a small, bounded number of unattended retries before it parks for a
// human, and the fleet as a whole cannot burn more than a handful of
// unattended retries per rolling day.
const (
	autoRequeueDefaultCooldownMinutes = 10
	autoRequeueDefaultPerItemMax      = 2
	autoRequeueDefaultPerDayMax       = 6
	autoRequeueDefaultDwellMinutes    = 6 * 60
	// autoRequeueMaxCooldownMinutes bounds the configured cooldown so a
	// fat-fingered policy can't disable the sweep by pushing eligibility a
	// year out; a week is already far past any useful retry horizon.
	autoRequeueMaxCooldownMinutes = 7 * 24 * 60
	autoRequeueMaxPerItem         = 20
	autoRequeueMaxPerDay          = 100
)

// AutoRequeuePolicy configures the reconciler's bounded auto-requeue sweep — the
// escalated→queued retry the operator performs itself, each tick, for
// escalations whose fault class can recover without a human (infra, transient,
// and external-dependency incidents that have since cleared). It is the config
// surface for pkg/mills.Reconciler.SweepAutoRequeue; every field has a
// conservative default so an omitted `pipeline.auto_requeue:` block still yields
// a safe, bounded, ENABLED sweep.
//
//	pipeline:
//	  auto_requeue:
//	    enabled: true
//	    cooldown_minutes: 10
//	    external_incident_max_dwell_minutes: 360
//	    per_item_max: 2
//	    per_day_max: 6
type AutoRequeuePolicy struct {
	// Enabled turns the sweep on. *bool so an omitted key defaults to ON
	// (nil == enabled) — the slice ships default-ON with conservative caps.
	// Opt OUT explicitly with `auto_requeue: {enabled: false}`.
	Enabled *bool `yaml:"enabled,omitempty"`
	// CooldownMinutes is how long after a run escalates before its backlog
	// item is eligible for an unattended requeue. Gives a transient fault time
	// to clear on its own and avoids a hot requeue loop. 0/unset → default 10.
	CooldownMinutes int `yaml:"cooldown_minutes,omitempty"`
	// ExternalIncidentMaxDwellMinutes bounds wait_for_dependency_recovery.
	// 0/unset uses the six-hour default.
	ExternalIncidentMaxDwellMinutes int `yaml:"external_incident_max_dwell_minutes,omitempty"`
	// PerItemMax caps how many times ONE backlog item may be auto-requeued over
	// its lifetime. Once hit, the item parks escalated for a human (the retry
	// clearly isn't working). Counted from durable events, so it survives an
	// operator restart. 0/unset → default 2.
	PerItemMax int `yaml:"per_item_max,omitempty"`
	// PerDayMax caps auto-requeues fleet-wide over the rolling 24h window — the
	// blast-radius limit if a whole class of runs starts flapping. 0/unset →
	// default 6.
	PerDayMax int `yaml:"per_day_max,omitempty"`
	// IncludeCodeConfig opts retryable code/config-class escalations into the
	// sweep (2026-08-01 shift data: 39/53 weekly escalations were code/config,
	// re-landed only by council-minted sibling items at a council run's cost
	// and hours of latency). A code/config retry is only meaningful when the
	// per-item journal is feeding prior-attempt context into stage prompts, so
	// eligibility is ADDITIONALLY gated on LOOM_MILLS_ITEM_JOURNAL being
	// active in the operator's environment — this flag alone can never
	// produce blind identical retries. Guards stacked on top of the normal
	// cooldown/caps: the run must carry EscalationRetryable=true (the
	// classifier's own verdict), must have spent > $0 (a free escalated run is
	// the no-op noise pattern; retrying it is a loop, not a fix), and the item
	// must have NO prior auto-requeue of any class (code/config retries are
	// one-shot). Default off.
	IncludeCodeConfig bool `yaml:"include_code_config,omitempty"`
	// Release governs recovery-triggered requeues. These bypass unattended
	// retry counters because the failed substrate, rather than the item, healed.
	Release SubstrateReleasePolicy `yaml:"release,omitempty"`
}

const substrateReleaseDefaultMaxPerSweep = 10

// substrateReleaseDefaultSignatures maps a failure-signature token to the
// operator capability whose red→green edge should release the escalation.
// Each token names a distinct capability: the devbox quality gate is reached
// through the hub session (its 2026-09-05 outages — the sandbox rebuild
// loop, the shared-child websocket close — showed up as mcp_hub_session red,
// not as spawn-pool trouble), spawn pod readiness is the HUD spawn pool, and
// ci_watch poll timeouts are GitLab.
var substrateReleaseDefaultSignatures = map[string]string{
	"sandbox_build":   "mcp_hub_session",
	"spawn_infra":     "hud_spawn",
	"ci_poll_timeout": "gitlab",
	"mcp_hub_session": "mcp_hub_session",
}

// SubstrateReleasePolicy controls release of escalations after a capability's
// red window closes. Enabled is default-on; maps are signature prefix/token to
// capability name, permitting operators to extend the built-in vocabulary.
type SubstrateReleasePolicy struct {
	Enabled               *bool             `yaml:"enabled,omitempty"`
	MaxPerSweep           int               `yaml:"max_per_sweep,omitempty"`
	EligibleClasses       []string          `yaml:"eligible_classes,omitempty"`
	SignatureCapabilities map[string]string `yaml:"signature_capabilities,omitempty"`
}

func (p SubstrateReleasePolicy) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }
func (p SubstrateReleasePolicy) SweepCap() int {
	if p.MaxPerSweep > 0 {
		return p.MaxPerSweep
	}
	return substrateReleaseDefaultMaxPerSweep
}
func (p SubstrateReleasePolicy) Classes() []string {
	if len(p.EligibleClasses) > 0 {
		return p.EligibleClasses
	}
	return []string{autoRequeueClassTransient, autoRequeueClassInfra, autoRequeueClassTransientQuota}
}
func (p SubstrateReleasePolicy) SignatureMap() map[string]string {
	if len(p.SignatureCapabilities) > 0 {
		return p.SignatureCapabilities
	}
	return substrateReleaseDefaultSignatures
}

// AutoRequeueEnabled resolves the *bool with default-ON semantics (nil ==
// enabled). Callers must use this instead of reading the field directly so the
// default rule stays in one place.
func (p PipelinePolicy) AutoRequeueEnabled() bool {
	return p.AutoRequeue.Enabled == nil || *p.AutoRequeue.Enabled
}

// CooldownDuration returns the configured cooldown, or the default when unset.
func (a AutoRequeuePolicy) CooldownDuration() time.Duration {
	m := a.CooldownMinutes
	if m <= 0 {
		m = autoRequeueDefaultCooldownMinutes
	}
	return time.Duration(m) * time.Minute
}

// ExternalIncidentMaxDwell returns the configured dependency recovery wait.
func (a AutoRequeuePolicy) ExternalIncidentMaxDwell() time.Duration {
	minutes := a.ExternalIncidentMaxDwellMinutes
	if minutes <= 0 {
		minutes = autoRequeueDefaultDwellMinutes
	}
	return time.Duration(minutes) * time.Minute
}

// ItemCap returns the per-item lifetime cap, or the default when unset.
func (a AutoRequeuePolicy) ItemCap() int {
	if a.PerItemMax <= 0 {
		return autoRequeueDefaultPerItemMax
	}
	return a.PerItemMax
}

// DayCap returns the fleet-wide rolling-24h cap, or the default when unset.
func (a AutoRequeuePolicy) DayCap() int {
	if a.PerDayMax <= 0 {
		return autoRequeueDefaultPerDayMax
	}
	return a.PerDayMax
}

// Spawn-transport breaker defaults. Deliberately tight: three runs is already
// enough evidence that the failure is the vendor and not the diff (every spawn
// reason token names a defect where the agent CLI produced NO verdict), and a
// 15m hold is short enough that a false trip costs one idle cadence rather than
// an operator's morning.
const (
	spawnBreakerDefaultThreshold       = 3
	spawnBreakerDefaultWindowMinutes   = 30
	spawnBreakerDefaultCooldownMinutes = 15
	// Ceilings so a fat-fingered policy cannot turn the breaker into a
	// permanent dispatch stop. A day is already far past any vendor outage the
	// breaker is meant to ride out; beyond that a human should pause the
	// operator explicitly.
	spawnBreakerMaxThreshold       = 100
	spawnBreakerMaxWindowMinutes   = 24 * 60
	spawnBreakerMaxCooldownMinutes = 24 * 60
)

// SpawnBreakerPolicy configures the consecutive-spawn-infra circuit breaker in
// pkg/mills.Reconciler: when the last few pipeline runs all escalated at the
// spawn layer for the SAME reason token (`spawn-agent-timeout`,
// `spawn-stdin-misconfig`, `spawn-auth-missing`, `spawn-driver-lost` — see
// pkg/mills/pipeline/spawn_class.go), the vendor is down and every further
// dispatch just burns another item's attempt budget against a dead endpoint.
// The breaker HOLDS new dispatch for a cooldown; it never touches in-flight
// runs and never mutates backlog items.
//
// It is the fast complement to the existing mechanisms, not a replacement:
// BreakerEvaluator reads a 24h KPI window, the sentinel overseer trips on
// dependency PROBES (and is default-OFF), and auto-requeue retries AFTER the
// fact. Only this one reacts to observed spawn failures within minutes, so the
// auto-requeue budget is not spent back into the same outage.
//
//	pipeline:
//	  spawn_breaker:
//	    enabled: true
//	    threshold: 3
//	    window_minutes: 30
//	    cooldown_minutes: 15
type SpawnBreakerPolicy struct {
	// Enabled turns the breaker on. *bool so an omitted key defaults to ON
	// (nil == enabled). Opt OUT explicitly with
	// `spawn_breaker: {enabled: false}`.
	Enabled *bool `yaml:"enabled,omitempty"`
	// Threshold is how many DISTINCT runs must escalate with the same spawn
	// reason token inside the window before dispatch is held. 0/unset → 3.
	Threshold int `yaml:"threshold,omitempty"`
	// WindowMinutes is the rolling observation window for those failures. It
	// also bounds the maximum hold: once the failures age out of the window the
	// breaker closes regardless of the cooldown. 0/unset → 30.
	WindowMinutes int `yaml:"window_minutes,omitempty"`
	// CooldownMinutes is how long after the LAST matching failure dispatch stays
	// held. When it elapses the breaker half-opens (dispatch resumes); one fresh
	// same-reason failure re-opens it because the earlier failures are still in
	// the window. 0/unset → 15.
	CooldownMinutes int `yaml:"cooldown_minutes,omitempty"`
}

// SpawnBreakerEnabled resolves the *bool with default-ON semantics (nil ==
// enabled). Callers must use this instead of reading the field directly so the
// default rule stays in one place.
func (p PipelinePolicy) SpawnBreakerEnabled() bool {
	return p.SpawnBreaker.Enabled == nil || *p.SpawnBreaker.Enabled
}

// FailureThreshold returns the configured trip count, or the default when unset.
func (s SpawnBreakerPolicy) FailureThreshold() int {
	if s.Threshold <= 0 {
		return spawnBreakerDefaultThreshold
	}
	return s.Threshold
}

// WindowDuration returns the configured observation window, or the default.
func (s SpawnBreakerPolicy) WindowDuration() time.Duration {
	m := s.WindowMinutes
	if m <= 0 {
		m = spawnBreakerDefaultWindowMinutes
	}
	return time.Duration(m) * time.Minute
}

// CooldownDuration returns the configured hold after the last matching failure,
// or the default when unset.
func (s SpawnBreakerPolicy) CooldownDuration() time.Duration {
	m := s.CooldownMinutes
	if m <= 0 {
		m = spawnBreakerDefaultCooldownMinutes
	}
	return time.Duration(m) * time.Minute
}

// Scope-amendment defaults. Both are deliberately small: the amendment exists
// to absorb the SIBLING-DIRECTORY reach an item author drew too narrowly, not
// to turn the scope envelope into a whole-tree grant.
const (
	// scopeAmendmentDefaultAncestorDepth is how many leading path segments a
	// violating file's directory must share with a declared slice file's
	// directory before the violation is admissible. 2 is the natural module
	// boundary in this workspace ("pkg/mills", "internal/hud",
	// "cmd/loom-mills-operator"): both 2026-07-26 production cases clear it
	// (token-sweep reached …/components/shared from …/components/mills;
	// stop-lever reached pkg/mills/store + pkg/mills/clients from
	// pkg/mills/pipeline) while a reach into platform/gitops, another service,
	// or the repo root does not.
	scopeAmendmentDefaultAncestorDepth = 2
	// scopeAmendmentDefaultMaxFiles caps how many files ONE run may amend into
	// the item's declared scope. Past a handful the diff is no longer "the
	// author forgot a sibling" — it is a decomposition failure a human should
	// look at, so it escalates with the S2 artifacts instead.
	scopeAmendmentDefaultMaxFiles = 6
	// scopeAmendmentMaxAncestorDepth / scopeAmendmentMaxFiles bound a
	// fat-fingered policy. A depth past 8 can never match anything in this
	// tree (so the amendment would silently become a no-op), and 50 amended
	// files is far past any "author drew it too narrowly" story.
	scopeAmendmentMaxAncestorDepth = 8
	scopeAmendmentMaxFiles         = 50
)

// ScopeAmendmentPolicy configures the runtime scope auto-amendment: when the
// implement stage's diff trips ONLY the `scope` gate, and every violating file
// shares a directory ancestor of at least AncestorDepth segments with a file
// the item already declared, the runner appends those files to the item's slice
// scope and CONTINUES with the existing diff instead of rewinding to a full
// implement respawn.
//
// Why this exists: 24h KPI on 2026-07-26 read 83% escalation / 17% auto-merge
// while gate_pass_rate was 95.7% — runs died disproportionately at this ONE
// gate. In every scope escalation examined (token-sweep, stop-lever, and the
// 2026-07-08 basename cohort) the implementer NEEDED the files and the item
// author — council or human — simply drew Slices[].files too narrowly. Retry
// can never converge on a necessary file: a fresh spawn correctly reaches for
// it again, burning a full implement respawn (~$1.7–5) per attempt.
//
//	pipeline:
//	  scope_amendment:
//	    enabled: true
//	    ancestor_depth: 2
//	    max_files: 6
type ScopeAmendmentPolicy struct {
	// Enabled turns the amendment on. *bool with nil == ENABLED, which
	// deliberately INVERTS this file's usual zero-value-off pattern for a new
	// optional section. Two reasons, both load-bearing:
	//
	//  1. The amendment never weakens the judgement of the diff. path_policy,
	//     secret_scan, diff_size, docs_guardrail, spec_conformance and
	//     pr_self_review all still run on the SAME files, and an amended file
	//     that is genuinely dangerous still fails them (the amendment also
	//     refuses protected paths outright — defense in depth).
	//  2. The production base rate of "author drew the envelope too narrowly"
	//     is ~100% of observed scope escalations, so shipping this OFF would
	//     ship the fix inert through exactly the window it was written for.
	//
	// Opt OUT explicitly with `scope_amendment: {enabled: false}`.
	Enabled *bool `yaml:"enabled,omitempty"`
	// AncestorDepth is the minimum number of shared leading path segments
	// between a violating file's directory and a declared slice file's
	// directory. 0/unset → 2 (see scopeAmendmentDefaultAncestorDepth).
	AncestorDepth int `yaml:"ancestor_depth,omitempty"`
	// MaxFiles caps the number of files admitted in ONE amendment. 0/unset → 6.
	MaxFiles int `yaml:"max_files,omitempty"`
}

// ScopeAmendmentEnabled resolves the *bool with default-ON semantics (nil ==
// enabled). Callers must use this instead of reading the field directly so the
// default rule stays in one place.
func (p PipelinePolicy) ScopeAmendmentEnabled() bool {
	return p.ScopeAmendment.Enabled == nil || *p.ScopeAmendment.Enabled
}

// Depth returns the configured minimum shared-ancestor depth, or the default.
func (s ScopeAmendmentPolicy) Depth() int {
	if s.AncestorDepth <= 0 {
		return scopeAmendmentDefaultAncestorDepth
	}
	return s.AncestorDepth
}

// FileCap returns the configured per-amendment file cap, or the default.
func (s ScopeAmendmentPolicy) FileCap() int {
	if s.MaxFiles <= 0 {
		return scopeAmendmentDefaultMaxFiles
	}
	return s.MaxFiles
}

// validateScopeAmendment bounds the tunables so a typo cannot turn the
// amendment into a silent no-op (unreachable depth) or an unbounded grant.
func validateScopeAmendment(s ScopeAmendmentPolicy) error {
	if s.AncestorDepth < 0 {
		return errors.New("pipeline.scope_amendment.ancestor_depth must be >= 0")
	}
	if s.AncestorDepth > scopeAmendmentMaxAncestorDepth {
		return fmt.Errorf("pipeline.scope_amendment.ancestor_depth must be <= %d", scopeAmendmentMaxAncestorDepth)
	}
	if s.MaxFiles < 0 {
		return errors.New("pipeline.scope_amendment.max_files must be >= 0")
	}
	if s.MaxFiles > scopeAmendmentMaxFiles {
		return fmt.Errorf("pipeline.scope_amendment.max_files must be <= %d", scopeAmendmentMaxFiles)
	}
	return nil
}

// ProtectedPathsMatch returns the subset of paths matching any glob in
// patterns. It is the pattern-matching kernel behind Policy.ProtectedPathsHit,
// factored out so callers that hold only the glob list — the scope-amendment
// evaluator, which must NEVER admit a protected path — enforce the identical
// rule instead of re-implementing it.
func ProtectedPathsMatch(patterns, paths []string) []string {
	if len(patterns) == 0 || len(paths) == 0 {
		return nil
	}
	var hits []string
	for _, path := range paths {
		for _, pat := range patterns {
			ok, err := doublestar.Match(pat, path)
			if err == nil && ok {
				hits = append(hits, path)
				break
			}
		}
	}
	return hits
}

// SubstrateDefault is the substrate returned by Policy.SubstrateForStage when
// no explicit mapping exists. Today's prod baseline is the k3s buildah-in-pod
// path; switch to "harvester-vm" only via explicit per-stage opt-in until the
// substrate proves out (see Slice 4 in .loom/45-…).
const SubstrateDefault = "k8s"

// SubstrateValuesValid is the closed set of devbox backend identifiers
// permitted in PipelinePolicy.StageSubstrate values.
var SubstrateValuesValid = map[string]struct{}{
	"k8s":          {},
	"harvester-vm": {},
}

// StageSubstrateKeysValid is the closed set of spawn-driven stage IDs whose
// substrate is operator-configurable. Keep in lockstep with the SpawnWorker
// entries in pkg/mills/pipeline/dispatcher.go::BuildDefaultWorkers and the
// `Type: "agent_spawn"` / `Type: "shell"` (tests) entries in
// pkg/mills/pipeline/runner.go::DefaultStages. Stages run by GitLabWorker
// (mr, ci_watch, merge, cleanup) are intentionally excluded — they have no
// devbox sandbox to choose.
var StageSubstrateKeysValid = map[string]struct{}{
	"plan_slice":     {},
	"research":       {},
	"implement":      {},
	"tests":          {},
	"pr_self_review": {},
}

// SubstrateForStage returns the configured devbox backend for stage. Returns
// SubstrateDefault ("k8s") when the stage has no entry, when the entry is
// the empty string, or when p is nil. This is the only caller-facing
// accessor; callers must not range over StageSubstrate directly so the
// fallback rule stays in one place.
func (p *Policy) SubstrateForStage(stage string) string {
	if p == nil {
		return SubstrateDefault
	}
	if v, ok := p.Pipeline.StageSubstrate[stage]; ok && v != "" {
		return v
	}
	return SubstrateDefault
}

// AgentDefault is the spawn agent Policy.AgentForStage callers fall back to
// when no per-stage override applies — the most common pipeline harness.
// Mirrors the spawn client's own empty-Model fallback
// (clients.agentTypeOrDefault) so a policy that configures nothing keeps the
// historical claude-code default.
const AgentDefault = "claude-code"

// StageAgentValuesValid is the closed set of agent ids permitted in
// PipelinePolicy.StageAgents values — the canonical spawn AgentType
// vocabulary (pkg/mills/worker.AgentType*). It is duplicated here rather
// than imported so policy.go (package mills) stays clear of the
// mills→worker→pipeline→mills import cycle. Keep in lockstep with
// worker.ValidateAgentType's canonical token set.
var StageAgentValuesValid = map[string]struct{}{
	"claude-code": {},
	"codex":       {},
	"gemini":      {},
}

// StageAgentKeysValid is the closed set of stage IDs whose spawn agent is
// operator-configurable. It is a STRICT SUBSET of StageSubstrateKeysValid:
// only the SpawnWorker-driven stages (plan_slice, implement, pr_self_review)
// consume an agent selection. research (WeaverWorker) and tests
// (DevboxWorker) have a devbox substrate but no agent/harness choice, so
// they are intentionally excluded here.
var StageAgentKeysValid = map[string]struct{}{
	"plan_slice":     {},
	"implement":      {},
	"pr_self_review": {},
}

// AgentForStage returns the configured spawn agent for stage, or the empty
// string when the stage has no entry, the entry is empty, or p is nil. Empty
// means "no policy override" — the operator's wiring then applies the
// break-glass LOOM_MILLS_SPAWN_AGENT env override or AgentDefault. This is one
// RUNG of the effective chain, not the whole chain: Policy.ResolveAgentRoute
// layers per-item routing above it and cmd/loom-mills-operator.spawnRouteFor
// adds the env break-glass on top. This is the only caller-facing accessor;
// callers must not range over StageAgents directly so the fallback rule stays
// in one place.
func (p *Policy) AgentForStage(stage string) string {
	if p == nil {
		return ""
	}
	if v, ok := p.Pipeline.StageAgents[stage]; ok && v != "" {
		return v
	}
	return ""
}

// StageModelKeysValid is the closed set of stage IDs whose spawn LLM model is
// operator-configurable. It is identical to StageAgentKeysValid — only the
// SpawnWorker-driven stages (plan_slice, implement, pr_self_review) carry a
// model — but kept as a distinct name so the stage_models validation error
// message reads independently of stage_agents.
var StageModelKeysValid = StageAgentKeysValid

// modelTokenPattern bounds the shape of a stage_models value: a vendor-native
// model id. Model ids across providers use alphanumerics plus a small set of
// separators (dot, dash, underscore, slash, colon) — e.g. "gpt-5.6-terra",
// "openai/gpt-5.6-sol", "claude-opus-4-8", "kimi-k3:0711". No whitespace, no
// empty. The agent CLI is the real authority on whether the id exists; this
// only rejects obviously malformed tokens (spaces, control chars, shell
// metacharacters) before they reach a spawn command line.
var modelTokenPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

// validModelToken reports whether s is a non-empty, sanely-shaped vendor-native
// model id (see modelTokenPattern). The 128-char bound is defensive — no real
// model id approaches it — and guards against a pathological policy value
// reaching a spawn command line.
func validModelToken(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	return modelTokenPattern.MatchString(s)
}

// ModelForStage returns the configured spawn LLM model for stage, or the empty
// string when the stage has no entry, the entry is empty, or p is nil. Empty
// means "no policy override" — the operator's wiring then applies the
// break-glass LOOM_MILLS_SPAWN_MODEL env override or leaves the model empty so
// the HUD spawn server picks its vendor default. Like AgentForStage this is one
// RUNG of the effective chain — Policy.ResolveAgentRoute may replace it with a
// route's own model, or drop it entirely when a route re-targets the vendor.
// This is the only caller-facing accessor; callers must not range over
// StageModels directly so the fallback rule stays in one place.
func (p *Policy) ModelForStage(stage string) string {
	if p == nil {
		return ""
	}
	if v, ok := p.Pipeline.StageModels[stage]; ok && v != "" {
		return v
	}
	return ""
}

// LabelOverride lets a label flip auto_merge / human_review for a specific
// class of work without rewriting the global policy.
type LabelOverride struct {
	Label       string `yaml:"label"`
	AutoMerge   bool   `yaml:"auto_merge"`
	HumanReview bool   `yaml:"human_review"`
}

// RepoExecutionOverride is a fail-safe, narrowing overlay for one repository.
// Zero budget values leave the corresponding global cap unchanged.
type RepoExecutionOverride struct {
	AutoMerge          *bool   `yaml:"auto_merge,omitempty"`
	RequireHumanReview *bool   `yaml:"require_human_review,omitempty"`
	MaxUSDPerRun       float64 `yaml:"max_usd_per_run,omitempty"`
	MaxRunsPerDay      int     `yaml:"max_runs_per_day,omitempty"`
}

// RetryPolicy controls how the pipeline retries failed stages.
//
// MaxAttempts caps "real" failures (Code + Infra error classes per
// pkg/mills/pipeline/error_class.go). TransientRetryCap caps the
// *extra* free retries the runner spends on Transient + TransientQuota
// failures (k8s pod GC, MCP transport drop, flexinfer timeout, etc.).
// Hard cap on total attempts is MaxAttempts + TransientRetryCap so a
// permanent transient can't loop forever.
type RetryPolicy struct {
	MaxAttempts       int `yaml:"max_attempts"`
	CooldownSeconds   int `yaml:"cooldown_seconds"`
	TransientRetryCap int `yaml:"transient_retry_cap,omitempty"` // default 5
	// ExternalIncidentPaidRetryCap bounds paid auto-requeues after an
	// external-dependency escalation. Zero uses the pipeline default (2).
	ExternalIncidentPaidRetryCap int `yaml:"external_incident_paid_retry_cap,omitempty"`
	// EscalationAutoRetryCap (Slice 3d) lets a transient-cap escalation
	// auto-spawn a fresh pipeline_run on the same backlog item, up to
	// this many extra runs. 0 disables (the code default, including
	// Default() — escalate straight to human); the deployed cluster
	// policy (platform/gitops k3s/mills/configmap-policy.yaml) opts in
	// with 2, which covers a flaky cluster afternoon without burning
	// more than 3 attempts of a real-code-bug budget.
	EscalationAutoRetryCap int `yaml:"escalation_auto_retry_cap,omitempty"`
}

// HumanHandoffPolicy controls escalation behavior.
type HumanHandoffPolicy struct {
	OnEscalationCreateHandoff bool   `yaml:"on_escalation_create_handoff"`
	OnEscalationCreateIssue   bool   `yaml:"on_escalation_create_issue"`
	NotifyAgentID             string `yaml:"notify_agent_id"`
}

// SquadsPolicy gates and tunes the v2 squad-routing layer. When Enabled
// is false the reconciler bypasses the router and the v1 generic path
// runs. Routing.MinConfidence mirrors squads.Router.MinConfidence — the
// router still enforces its own internal default if this is zero.
type SquadsPolicy struct {
	Enabled bool                `yaml:"enabled,omitempty"`
	Routing SquadsRoutingPolicy `yaml:"routing,omitempty"`
}

// SquadsRoutingPolicy bounds router behavior. MinConfidence, when
// positive, overrides the router's compiled-in floor (served live via
// Policy.SquadsMinConfidence → squads.RoutingPolicy). Fallback is
// RESERVED — the router always uses its compiled-in FallbackName
// ("_default") today.
type SquadsRoutingPolicy struct {
	MinConfidence float64 `yaml:"min_confidence,omitempty"`
	Fallback      string  `yaml:"fallback,omitempty"`
}

// AuditPolicy controls the adversarial audit swarm. AdvisoryOnly is the
// v2.0 default: audit findings open follow-up issues + score in the HUD
// but never block merges. v2.1 flips it to blocking once the survival
// rate clears the SurvivalThreshold (default 0.6).
//
// AdvisoryOnly is a *bool so an omitted YAML key defaults to true (the
// spec's v2.0 fail-safe). An explicit `advisory_only: false` lets the
// operator opt into blocking mode once survival rates prove low-noise.
type AuditPolicy struct {
	Enabled           bool        `yaml:"enabled,omitempty"`
	PoolDefault       []AuditPool `yaml:"pool_default,omitempty"`
	PoolEscalation    []AuditPool `yaml:"pool_escalation,omitempty"`
	DailyBudgetUSD    float64     `yaml:"daily_budget_usd,omitempty"`
	AdvisoryOnly      *bool       `yaml:"advisory_only,omitempty"`
	SurvivalThreshold float64     `yaml:"survival_threshold,omitempty"`
}

// AuditPool names one auditor model. Backend is the spawn-side driver
// ("flexinfer" or "spawn"); Model is the model identifier; Driver is
// the spawn driver name when Backend == "spawn".
type AuditPool struct {
	Backend string `yaml:"backend,omitempty"`
	Model   string `yaml:"model,omitempty"`
	Driver  string `yaml:"driver,omitempty"`
}

// CrossRepoPolicy controls atomic-merge runs that span multiple repos.
// Disabled in v2.0; flipped after 4 weeks of dogfooding per V2-D4.
type CrossRepoPolicy struct {
	Enabled               bool   `yaml:"enabled,omitempty"`
	PerRepoTimeoutMinutes int    `yaml:"per_repo_timeout_minutes,omitempty"`
	RevertStrategy        string `yaml:"revert_strategy,omitempty"`
	// DemandProjects is the allowlist of NON-home repos the plan-slice emitter
	// may source demand from and stamp as an item's TargetProject (S6). Each
	// entry is a services-group project path, e.g. "services/flexdeck". Empty =
	// home-only demand (pre-S6 behavior). This is the SECOND key of a two-key
	// activation: the list is consulted only when Enabled is also true (see
	// CrossRepoDemandProjects), so a stray allowlist can never source foreign
	// demand while cross-repo execution is off, and the reconciler's fail-closed
	// gate still guards execution of any foreign item that does get stamped.
	DemandProjects []string `yaml:"demand_projects,omitempty"`
	// AllowBootstrapped extends demand sourcing to projects the operator
	// bootstrapped at runtime from a Spinning Room plan (the
	// bootstrapped_projects store table), WITHOUT a per-repo gitops allowlist
	// edit. Same two-key discipline as DemandProjects: this flag is consulted
	// only when Enabled is also true (see CrossRepoBootstrapEnabled), so
	// bootstrapped demand is inert until cross-repo execution is on AND gitops
	// opts in to runtime-minted repos. Default false (ships inert).
	AllowBootstrapped bool `yaml:"allow_bootstrapped,omitempty"`
	// BootstrapAllowedGroups is the allow-list of GitLab group paths a repo may
	// be minted UNDER by the plan→repo bootstrap flow (the endpoint AND the
	// reconciler's cross-repo pre-flight). Each entry is a group path, e.g.
	// "services" or "labs"; a TargetProject "services/familyforge" is allowed
	// only when its parent group "services" is listed. This is a THIRD safety
	// beyond the two-key AllowBootstrapped gate: it bounds *where* autonomous
	// repo creation can happen so a typo'd or malicious TargetProject can never
	// mint a repo in an arbitrary namespace. Consulted only when the two-key
	// gate is on (see CrossRepoBootstrapGroupAllowed); an empty list fails
	// closed — bootstrap creates nothing until a group is explicitly listed.
	BootstrapAllowedGroups []string `yaml:"bootstrap_allowed_groups,omitempty"`
}

// DebatePolicy controls the v2 council debate rounds. Disabled by
// default for cron + roadmap_change triggers; enabled for incident
// triggers per V2-D5.
type DebatePolicy struct {
	Enabled            DebateTriggers `yaml:"enabled,omitempty"`
	MaxUSD             float64        `yaml:"max_usd,omitempty"`
	MaxRounds          int            `yaml:"max_rounds,omitempty"`
	EarlyExitThreshold float64        `yaml:"early_exit_threshold,omitempty"`
}

// DebateTriggers controls which trigger sources kick off a debate run.
type DebateTriggers struct {
	Cron          bool `yaml:"cron,omitempty"`
	RoadmapChange bool `yaml:"roadmap_change,omitempty"`
	Incident      bool `yaml:"incident,omitempty"`
	Manual        bool `yaml:"manual,omitempty"`
}

// AllowedFor reports whether the debate runner should engage for the
// given store.CouncilTrigger string. Mapping: cron → Cron, roadmap →
// RoadmapChange (spec naming differs from store enum), incident →
// Incident, manual → Manual. Unknown triggers return false (safe
// default; the runner falls back to single-pass).
//
// Lives on DebateTriggers rather than DebatePolicy so the policy
// validation tests can pin trigger gating without instantiating a
// whole DebatePolicy.
func (t DebateTriggers) AllowedFor(trigger string) bool {
	switch trigger {
	case "cron":
		return t.Cron
	case "roadmap":
		return t.RoadmapChange
	case "incident":
		return t.Incident
	case "manual":
		return t.Manual
	default:
		return false
	}
}

// RecursionPolicy controls bounded sub-runs in the pipeline. Disabled
// by default; opt-in per-squad via the squad manifest's
// default_ensemble.recursion: true (V2-D6).
type RecursionPolicy struct {
	Enabled              bool    `yaml:"enabled,omitempty"`
	MaxDepth             int     `yaml:"max_depth,omitempty"`
	SubrunMaxBudgetShare float64 `yaml:"subrun_max_budget_share,omitempty"`
}

// AdaptivePolicy controls the policy-proposal engine. Disabled by
// default; v2.0 ships with auto_apply=false so all proposals require
// human edit before applying.
type AdaptivePolicy struct {
	Enabled           bool     `yaml:"enabled,omitempty"`
	AutoApply         bool     `yaml:"auto_apply,omitempty"`
	RelaxPathDenylist []string `yaml:"relax_path_denylist,omitempty"`
	RevertWindowHours int      `yaml:"revert_window_hours,omitempty"`
}

// Default returns a baseline policy suitable for local development. Production
// deployments override via the ConfigMap-mounted policy.yaml. Enabled is true
// because phase 6 has shipped (slice 6.6 default-on flip, 2026-05-02). The
// kill switch is `enabled: false` in the YAML; nil treats as enabled per
// IsEnabled.
//
// Schema version is 2 (the Mills v2 hierarchical-swarm rollout). v2-only
// sections default to "off" so a v1 deployment that hot-reloads onto a
// v2 binary keeps its v1 behavior until the operator opts in.
func Default() *Policy {
	enabled := true
	return &Policy{
		Version: 2,
		Enabled: &enabled,
		Budgets: Budgets{
			Council:             BudgetLimits{MaxUSDPerRun: 15, MaxUSDPerDay: 50},
			Pipeline:            BudgetLimits{MaxUSDPerRun: 5, MaxUSDPerDay: 75, MaxConcurrentRuns: 4, MaxRunsPerDay: 20},
			ThroughputGuardrail: DefaultThroughputGuardrailThresholds,
		},
		Council: CouncilPolicy{
			ScheduleCron:           "0 */6 * * *",
			ArtifactsBranch:        "council/{date}",
			ArtifactsMergeStrategy: "fast-merge-loom-only",
			Triggers:               CouncilTriggers{OnRoadmapChange: true, OnIncident: true, OnMergeDriftHours: 48},
		},
		Pipeline: PipelinePolicy{
			DefaultTemplate: "mills-default-pipeline",
			CIWatch:         CIWatchPolicy{FlakyJobs: []string{"test:reliability", "test:unit"}},
			Retry:           RetryPolicy{MaxAttempts: 3, CooldownSeconds: 300},
			ProtectedPaths: []string{
				"platform/gitops/**",
				"cmd/loomd/**",
				"**/*auth*.go",
				"**/secret*.yaml",
			},
			// Default-ON with the conservative built-in caps (10m cooldown,
			// 2/item, 6/day). Explicit here so `Default()` and an omitted
			// `auto_requeue:` block resolve to the same enabled sweep.
			AutoRequeue: AutoRequeuePolicy{Enabled: boolPtr(true), Release: SubstrateReleasePolicy{Enabled: boolPtr(true)}},
			// Default-ON with the built-in depth/file caps, so `Default()` and
			// an omitted `scope_amendment:` block resolve identically (see
			// ScopeAmendmentPolicy.Enabled for why this section inverts the
			// usual zero-value-off default).
			ScopeAmendment: ScopeAmendmentPolicy{Enabled: boolPtr(true)},
		},
		HumanHandoff: HumanHandoffPolicy{
			OnEscalationCreateHandoff: true,
			OnEscalationCreateIssue:   true,
		},
		// v2 defaults: squads off, audit advisory-on, everything else off.
		// Operator flips per Phase 8 default-on rollout (one feature, one
		// week soak, repeat).
		Squads: SquadsPolicy{
			Enabled: false,
			Routing: SquadsRoutingPolicy{
				MinConfidence: 0.6,
				Fallback:      "_default",
			},
		},
		Audit: AuditPolicy{
			Enabled:           true,
			AdvisoryOnly:      boolPtr(true),
			SurvivalThreshold: 0.6,
		},
		CrossRepo: CrossRepoPolicy{Enabled: false},
		Debate:    DebatePolicy{},
		Recursion: RecursionPolicy{Enabled: false},
		AdaptivePolicy: AdaptivePolicy{
			Enabled:   false,
			AutoApply: false,
		},
		// S6-min: the durable imperative workflow runtime ships default-OFF.
		// The scheduler is always wired but self-gates on this flag, so the
		// runtime is inert until the S1c canary window flips it on (then
		// reverts). SubstrateK8sOnly defaults false; the runtime treats the
		// canary as k8s explicitly until S6-full wires substrate selection.
		Workflows: WorkflowsPolicy{
			Enabled:          false,
			SubstrateK8sOnly: false,
		},
	}
}

// LoadPolicy reads, parses, and validates a policy YAML file.
func LoadPolicy(path string) (*Policy, error) {
	p, _, err := LoadPolicyWithChecksum(path)
	return p, err
}

// LoadPolicyWithChecksum is LoadPolicy plus the checksum of the EXACT bytes the
// returned policy was parsed from. Provenance callers must use this rather than
// hashing a second read of the same path: a ConfigMap swap between the two
// reads would attribute a run to a checksum its policy never came from.
func LoadPolicyWithChecksum(path string) (*Policy, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("mills: read policy %s: %w", path, err)
	}
	p, err := ParsePolicy(data)
	if err != nil {
		return nil, "", err
	}
	return p, PolicyChecksum(data), nil
}

// PolicyChecksum is the provenance digest of a policy document's exact bytes.
// The format matches the deployment pod-template annotation
// loom.flexinfer.ai/policy-checksum, so a run stamped with this value can be
// cross-checked against the ConfigMap the operator was rolled out with.
func PolicyChecksum(raw []byte) string {
	return ProvenanceDigest(raw)
}

// ParsePolicy parses + validates a policy from raw YAML.
func ParsePolicy(data []byte) (*Policy, error) {
	var p Policy
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("mills: parse policy: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("mills: validate policy: %w", err)
	}
	return &p, nil
}

// GatesPolicy configures the dissent second opinion independently of the primary judge.
type GatesPolicy struct {
	Tiebreaker GateTiebreakerPolicy `yaml:"tiebreaker,omitempty"`
}

type GateJudgeHop struct {
	Backend string `yaml:"backend"`
	Model   string `yaml:"model"`
}

type GateTiebreakerPolicy struct {
	Backend string `yaml:"backend,omitempty"`
	Model   string `yaml:"model,omitempty"`
	// Nil selects defaults; an explicitly empty list disables fallback.
	Fallbacks []GateJudgeHop `yaml:"fallbacks"`
}

// Hops resolves defaults without mutating policy. flexModel is the local
// client's resolved model, so gateway-only model aliases cannot leak to it.
func (p GateTiebreakerPolicy) Hops(flexModel string) []GateJudgeHop {
	backend := strings.ToLower(strings.TrimSpace(p.Backend))
	if backend == "" {
		backend = "anthropic"
	}
	hops := []GateJudgeHop{{Backend: backend, Model: p.Model}}
	if p.Fallbacks == nil {
		hops = append(hops, GateJudgeHop{Backend: "openai", Model: "gpt-5.5"}, GateJudgeHop{Backend: "flexinfer", Model: flexModel})
	} else {
		hops = append(hops, p.Fallbacks...)
	}
	for i := range hops {
		h := &hops[i]
		h.Backend = strings.ToLower(strings.TrimSpace(h.Backend))
		h.Model = strings.TrimSpace(h.Model)
		if h.Model == "" {
			switch h.Backend {
			case "anthropic":
				h.Model = "claude-sonnet-5"
			case "openai":
				h.Model = "gpt-5.5"
			case "flexinfer":
				h.Model = flexModel
			}
		}
	}
	return hops
}

func (p GateTiebreakerPolicy) Validate() error {
	previous := ""
	for i, h := range p.Hops("local-judge") {
		switch h.Backend {
		case "anthropic", "openai", "openrouter", "flexinfer":
		default:
			return fmt.Errorf("gates.tiebreaker hop %d: unsupported backend %q", i, h.Backend)
		}
		if h.Backend == "openrouter" && h.Model == "" {
			return fmt.Errorf("gates.tiebreaker hop %d: openrouter requires a model", i)
		}
		if h.Backend == previous {
			return fmt.Errorf("gates.tiebreaker hop %d: consecutive vendor %q", i, h.Backend)
		}
		previous = h.Backend
	}
	return nil
}

// Validate enforces the rules a malformed policy must trip on.
//
// Version handling: 0 (omitted) is treated as legacy v1 — pre-Phase-7
// YAML had no `version` key in many test fixtures and dev configs, and
// silently bumping those to "unsupported" would break hot-reload. v1
// and v2 are both valid; anything else is rejected.
func (p *Policy) Validate() error {
	if p == nil {
		return errors.New("policy is nil")
	}
	if err := p.Gates.Tiebreaker.Validate(); err != nil {
		return err
	}
	switch p.Version {
	case 0, 1, 2:
		// 0 = omitted; treat as legacy v1.
	default:
		return fmt.Errorf("unsupported policy version %d (supported: 1, 2)", p.Version)
	}
	if _, err := basepolicy.ResolvePipelineConcurrencyLimit(p.MaxConcurrentPipelines); err != nil {
		return err
	}
	if p.Health.PollIntervalSeconds < 0 {
		return errors.New("health.poll_interval_seconds must be >= 0")
	}
	if p.Health.Enabled && strings.TrimSpace(p.Health.Project) == "" {
		return errors.New("health.project is required when health is enabled")
	}
	if p.Health.BaseRed.ThresholdMinutes < 0 {
		return errors.New("health.base_red.threshold_minutes must be >= 0")
	}
	if p.Health.BaseRed.Enabled {
		switch p.Health.BaseRed.Mode {
		case "", "enforce", "dry-log":
		default:
			return errors.New("health.base_red.mode must be enforce or dry-log")
		}
	}
	if p.Health.Flightdeck.TimeoutSeconds < 0 {
		return errors.New("health.flightdeck.timeout_seconds must be >= 0")
	}
	if p.Health.Flightdeck.QueueSize < 0 {
		return errors.New("health.flightdeck.queue_size must be >= 0")
	}
	if p.Health.Flightdeck.Enabled && (strings.TrimSpace(p.Health.Flightdeck.Endpoint) == "" || strings.TrimSpace(p.Health.Flightdeck.Token) == "") {
		return errors.New("health.flightdeck.endpoint and health.flightdeck.token are required when flightdeck is enabled")
	}
	if err := validateBudget("council", p.Budgets.Council); err != nil {
		return err
	}
	if err := validateBudget("pipeline", p.Budgets.Pipeline); err != nil {
		return err
	}
	if err := validateModelPrices(p.Budgets.ModelPrices); err != nil {
		return err
	}
	if t := p.Budgets.ThroughputGuardrail; t.MaxEscalationRate < 0 || t.MaxEscalationRate > 1 {
		return errors.New("budgets.throughput_guardrail.max_escalation_rate must be in [0,1]")
	} else if t.MaxCostPerMergedPipelineUSD < 0 || t.MaxScopeQueueAgeSeconds < 0 || t.MaxStarvedQueues < 0 {
		return errors.New("budgets.throughput_guardrail thresholds must be >= 0")
	}
	if p.Council.ArtifactsMergeStrategy != "" {
		switch p.Council.ArtifactsMergeStrategy {
		case "fast-merge-loom-only", "always-mr":
		default:
			return fmt.Errorf("council.artifacts_merge_strategy: must be 'fast-merge-loom-only' or 'always-mr', got %q", p.Council.ArtifactsMergeStrategy)
		}
	}
	if p.Council.Dedup.MergedWork.LookbackHours < 0 {
		return errors.New("council.dedup.merged_work.lookback_hours must be >= 0 (0 = default 14d)")
	}
	if p.Council.Sources.FactoryExhaust.LookbackHours < 0 {
		return errors.New("council.sources.factory_exhaust.lookback_hours must be >= 0 (0 = default 14d)")
	}
	if p.Council.Sources.FactoryExhaust.MaxItems < 0 {
		return errors.New("council.sources.factory_exhaust.max_items must be >= 0 (0 = default 10)")
	}
	if p.Pipeline.Retry.MaxAttempts < 0 {
		return errors.New("pipeline.retry.max_attempts must be >= 0")
	}
	if p.Pipeline.Retry.CooldownSeconds < 0 {
		return errors.New("pipeline.retry.cooldown_seconds must be >= 0")
	}
	if p.Pipeline.CIWatch.MaxWallClockMinutes < 0 {
		return errors.New("pipeline.ci_watch.max_wall_clock_minutes must be >= 0")
	}
	if p.Pipeline.RankerTimeoutMilliseconds < 0 || p.Pipeline.RankerMaxCandidates < 0 || p.Pipeline.RankerMaxCost < 0 {
		return errors.New("pipeline ranker bounds must be >= 0")
	}
	if p.Pipeline.ScopeFairness.DeferralThreshold < 0 || p.Pipeline.ScopeFairness.AgeThresholdHours < 0 || p.Pipeline.ScopeFairness.ReservationHoldHours < 0 {
		return errors.New("pipeline.scope_fairness thresholds must be >= 0")
	}
	for i, ov := range p.Pipeline.PerLabelOverrides {
		if ov.Label == "" {
			return fmt.Errorf("pipeline.per_label_overrides[%d].label is empty", i)
		}
	}
	for i, gp := range p.Pipeline.ProtectedPaths {
		if !doublestar.ValidatePattern(gp) {
			return fmt.Errorf("pipeline.protected_paths[%d] %q is not a valid glob", i, gp)
		}
	}
	seenOverlayRepos := make(map[string]string, len(p.Pipeline.ProtectedPathsPerRepo))
	for key, globs := range p.Pipeline.ProtectedPathsPerRepo {
		base, ok := normalizedRepoKey(key)
		if !ok {
			return fmt.Errorf("pipeline.protected_paths_per_repo: key %q does not name a repo", key)
		}
		// Two keys naming the same repo ("flexdeck" and "services/flexdeck")
		// would make the effective overlay depend on map iteration order.
		if prev, dup := seenOverlayRepos[base]; dup {
			return fmt.Errorf("pipeline.protected_paths_per_repo: keys %q and %q name the same repo", prev, key)
		}
		seenOverlayRepos[base] = key
		for i, gp := range globs {
			if !doublestar.ValidatePattern(gp) {
				return fmt.Errorf("pipeline.protected_paths_per_repo[%s][%d] %q is not a valid glob", key, i, gp)
			}
		}
	}
	seenExecutionRepos := make(map[string]string, len(p.Pipeline.PerRepoOverrides))
	for key, override := range p.Pipeline.PerRepoOverrides {
		base, ok := normalizedRepoKey(key)
		if !ok {
			return fmt.Errorf("pipeline.per_repo_overrides: key %q does not name a repo", key)
		}
		if prev, dup := seenExecutionRepos[base]; dup {
			return fmt.Errorf("pipeline.per_repo_overrides: keys %q and %q name the same repo", prev, key)
		}
		seenExecutionRepos[base] = key
		if override.MaxUSDPerRun < 0 {
			return fmt.Errorf("pipeline.per_repo_overrides[%s].max_usd_per_run must be >= 0", key)
		}
		if override.MaxRunsPerDay < 0 {
			return fmt.Errorf("pipeline.per_repo_overrides[%s].max_runs_per_day must be >= 0", key)
		}
	}
	seenKnownRepos := make(map[string]string, len(p.CrossRepo.DemandProjects))
	for _, key := range p.CrossRepo.DemandProjects {
		base, ok := normalizedRepoKey(key)
		if !ok {
			return fmt.Errorf("cross_repo.demand_projects: entry %q does not name a repo", key)
		}
		if prev, dup := seenKnownRepos[base]; dup {
			return fmt.Errorf("cross_repo.demand_projects: entries %q and %q name the same repo", prev, key)
		}
		seenKnownRepos[base] = key
	}
	seenIntakeRepos := make(map[string]string, len(p.Intake.GitLab.Projects))
	for _, key := range p.Intake.GitLab.Projects {
		base, ok := normalizedRepoKey(key)
		if !ok {
			return fmt.Errorf("intake.gitlab.projects: entry %q does not name a repo", key)
		}
		if prev, dup := seenIntakeRepos[base]; dup {
			return fmt.Errorf("intake.gitlab.projects: entries %q and %q name the same repo", prev, key)
		}
		seenIntakeRepos[base] = key
	}
	for stage, sub := range p.Pipeline.StageSubstrate {
		if _, ok := StageSubstrateKeysValid[stage]; !ok {
			return fmt.Errorf("pipeline.stage_substrate: %q is not a configurable stage (allowed: plan_slice, research, implement, tests, pr_self_review)", stage)
		}
		if _, ok := SubstrateValuesValid[sub]; !ok {
			return fmt.Errorf("pipeline.stage_substrate[%s]: %q is not a recognized substrate (allowed: k8s, harvester-vm)", stage, sub)
		}
	}
	for stage, agent := range p.Pipeline.StageAgents {
		if _, ok := StageAgentKeysValid[stage]; !ok {
			return fmt.Errorf("pipeline.stage_agents: %q is not an agent-configurable stage (allowed: plan_slice, implement, pr_self_review)", stage)
		}
		if _, ok := StageAgentValuesValid[agent]; !ok {
			return fmt.Errorf("pipeline.stage_agents[%s]: %q is not a recognized agent (allowed: claude-code, codex, gemini)", stage, agent)
		}
	}
	for stage, model := range p.Pipeline.StageModels {
		if _, ok := StageModelKeysValid[stage]; !ok {
			return fmt.Errorf("pipeline.stage_models: %q is not a model-configurable stage (allowed: plan_slice, implement, pr_self_review)", stage)
		}
		if !validModelToken(model) {
			return fmt.Errorf("pipeline.stage_models[%s]: %q is not a valid model id (expect a non-empty vendor-native token like gpt-5.6-terra)", stage, model)
		}
	}
	if err := validateAgentRouting(p.Pipeline.AgentRouting); err != nil {
		return err
	}
	if err := validateAutoRequeue(p.Pipeline.AutoRequeue); err != nil {
		return err
	}
	if p.Pipeline.AutonomyHoldMinutes < 0 || p.Pipeline.AutonomyHoldMinutes > 1440 {
		return errors.New("pipeline.autonomy_hold_minutes must be between 0 and 1440")
	}
	if err := validateSpawnBreaker(p.Pipeline.SpawnBreaker); err != nil {
		return err
	}
	if err := validateScopeAmendment(p.Pipeline.ScopeAmendment); err != nil {
		return err
	}
	if err := validateOverseers(p.Overseers); err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(p.Council.Ensemble.Editor.Backend), "openrouter") && strings.TrimSpace(p.Council.Ensemble.Editor.Model) == "" {
		return errors.New("council.ensemble.editor.model is required for openrouter")
	}
	if p.Council.Ensemble.Editor.Model != "" && p.Council.Ensemble.Editor.Backend == "" {
		return errors.New("council.ensemble.editor.backend is required when editor.model is set")
	}
	if err := validateEditorFallback(p.Council.Ensemble.EditorFallback, p.Council.Ensemble.Editor.Backend, "council.ensemble.editor_fallback"); err != nil {
		return err
	}
	if fb := p.SpinningRoom.DefaultFallback; fb != nil {
		if err := validateEditorFallback(*fb, "", "spinning_room.default_fallback"); err != nil {
			return err
		}
	}
	for i, frame := range p.SpinningRoom.Frames {
		fb := frame.Fallback
		if fb == nil {
			fb = p.SpinningRoom.DefaultFallback
		}
		if fb != nil {
			if err := validateEditorFallback(*fb, frame.Backend, fmt.Sprintf("spinning_room.frames[%d].fallback", i)); err != nil {
				return err
			}
		}
	}

	for i, r := range p.Council.Ensemble.Reviewers {
		if r.Model == "" || r.Backend == "" {
			return fmt.Errorf("council.ensemble.reviewers[%d] requires both model and backend", i)
		}
	}
	// Spinning room frames: when the room is enabled each frame must be
	// selectable (Name) and drivable (Model). Backend defaults to flexinfer at
	// build time, so it isn't required here. Names must be unique (case-
	// insensitively) since the HUD selects by name.
	if p.SpinningRoom.Enabled {
		seen := map[string]bool{}
		for i, f := range p.SpinningRoom.Frames {
			name := strings.ToLower(strings.TrimSpace(f.Name))
			if name == "" {
				return fmt.Errorf("spinning_room.frames[%d].name is required", i)
			}
			if strings.TrimSpace(f.Model) == "" {
				return fmt.Errorf("spinning_room.frames[%d] (%q) requires a model", i, f.Name)
			}
			if seen[name] {
				return fmt.Errorf("spinning_room.frames[%d]: duplicate frame name %q", i, f.Name)
			}
			seen[name] = true
		}
	}
	if pr := strings.TrimSpace(p.SpinningRoom.DefaultPriority); pr != "" {
		switch strings.ToUpper(pr) {
		case "P0", "P1", "P2", "P3":
		default:
			return fmt.Errorf("spinning_room.default_priority: must be one of P0..P3, got %q", p.SpinningRoom.DefaultPriority)
		}
	}
	return nil
}

func normalizedRepoKey(project string) (string, bool) {
	if project == "" || project != strings.TrimSpace(project) || strings.ContainsAny(project, " \t\r\n") {
		return "", false
	}
	parts := strings.Split(project, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", false
		}
	}
	return strings.ToLower(parts[len(parts)-1]), true
}

// validateAutoRequeue bounds the auto-requeue caps. Values must be
// non-negative (0 means "use the default", resolved by the accessors) and
// within sane ceilings so a fat-fingered policy cannot disable the sweep by
// pushing the cooldown out of reach or turn the caps into effectively-unbounded
// retry loops.
func validateAutoRequeue(a AutoRequeuePolicy) error {
	if a.CooldownMinutes < 0 {
		return errors.New("pipeline.auto_requeue.cooldown_minutes must be >= 0")
	}
	if a.CooldownMinutes > autoRequeueMaxCooldownMinutes {
		return fmt.Errorf("pipeline.auto_requeue.cooldown_minutes (%d) exceeds the max of %d (7 days)", a.CooldownMinutes, autoRequeueMaxCooldownMinutes)
	}
	if a.ExternalIncidentMaxDwellMinutes < 0 {
		return errors.New("pipeline.auto_requeue.external_incident_max_dwell_minutes must be >= 0")
	}
	if a.ExternalIncidentMaxDwellMinutes > autoRequeueMaxCooldownMinutes {
		return fmt.Errorf("pipeline.auto_requeue.external_incident_max_dwell_minutes (%d) exceeds the max of %d (7 days)", a.ExternalIncidentMaxDwellMinutes, autoRequeueMaxCooldownMinutes)
	}
	if a.PerItemMax < 0 {
		return errors.New("pipeline.auto_requeue.per_item_max must be >= 0")
	}
	if a.PerItemMax > autoRequeueMaxPerItem {
		return fmt.Errorf("pipeline.auto_requeue.per_item_max (%d) exceeds the max of %d", a.PerItemMax, autoRequeueMaxPerItem)
	}
	if a.PerDayMax < 0 {
		return errors.New("pipeline.auto_requeue.per_day_max must be >= 0")
	}
	if a.PerDayMax > autoRequeueMaxPerDay {
		return fmt.Errorf("pipeline.auto_requeue.per_day_max (%d) exceeds the max of %d", a.PerDayMax, autoRequeueMaxPerDay)
	}
	if a.Release.MaxPerSweep < 0 || a.Release.MaxPerSweep > 100 {
		return errors.New("pipeline.auto_requeue.release.max_per_sweep must be between 0 and 100")
	}
	knownClasses := map[string]bool{"transient": true, "infra": true, "transient_quota": true}
	for _, class := range a.Release.Classes() {
		if !knownClasses[strings.ToLower(strings.TrimSpace(class))] {
			return fmt.Errorf("pipeline.auto_requeue.release.eligible_classes contains unknown class %q", class)
		}
	}
	for signature, capability := range a.Release.SignatureMap() {
		if strings.TrimSpace(signature) == "" || strings.TrimSpace(capability) == "" {
			return errors.New("pipeline.auto_requeue.release.signature_capabilities requires non-empty signatures and capabilities")
		}
	}
	return nil
}

// validateSpawnBreaker bounds the spawn-transport breaker knobs. Values must be
// non-negative (0 means "use the default", resolved by the accessors) and within
// ceilings that keep the breaker a bounded HOLD rather than an accidental
// permanent dispatch stop.
func validateSpawnBreaker(s SpawnBreakerPolicy) error {
	if s.Threshold < 0 {
		return errors.New("pipeline.spawn_breaker.threshold must be >= 0")
	}
	if s.Threshold > spawnBreakerMaxThreshold {
		return fmt.Errorf("pipeline.spawn_breaker.threshold (%d) exceeds the max of %d", s.Threshold, spawnBreakerMaxThreshold)
	}
	if s.WindowMinutes < 0 {
		return errors.New("pipeline.spawn_breaker.window_minutes must be >= 0")
	}
	if s.WindowMinutes > spawnBreakerMaxWindowMinutes {
		return fmt.Errorf("pipeline.spawn_breaker.window_minutes (%d) exceeds the max of %d (24h)", s.WindowMinutes, spawnBreakerMaxWindowMinutes)
	}
	if s.CooldownMinutes < 0 {
		return errors.New("pipeline.spawn_breaker.cooldown_minutes must be >= 0")
	}
	if s.CooldownMinutes > spawnBreakerMaxCooldownMinutes {
		return fmt.Errorf("pipeline.spawn_breaker.cooldown_minutes (%d) exceeds the max of %d (24h)", s.CooldownMinutes, spawnBreakerMaxCooldownMinutes)
	}
	return nil
}

func validateBudget(tier string, b BudgetLimits) error {
	if b.MaxUSDPerRun < 0 {
		return fmt.Errorf("budgets.%s.max_usd_per_run must be >= 0", tier)
	}
	if b.MaxUSDPerDay < 0 {
		return fmt.Errorf("budgets.%s.max_usd_per_day must be >= 0", tier)
	}
	if b.MaxUSDPerRun > 0 && b.MaxUSDPerDay > 0 && b.MaxUSDPerRun > b.MaxUSDPerDay {
		return fmt.Errorf("budgets.%s.max_usd_per_run (%v) exceeds max_usd_per_day (%v)", tier, b.MaxUSDPerRun, b.MaxUSDPerDay)
	}
	if b.MaxSubscriptionUSDPerDay < 0 {
		return fmt.Errorf("budgets.%s.max_subscription_usd_per_day must be >= 0", tier)
	}
	if b.MaxConcurrentRuns < 0 {
		return fmt.Errorf("budgets.%s.max_concurrent_runs must be >= 0", tier)
	}
	if b.MaxRunsPerDay < 0 {
		return fmt.Errorf("budgets.%s.max_runs_per_day must be >= 0", tier)
	}
	return nil
}

// IsEnabled reports whether the mills should act. The kill switch defaults to
// enabled; an explicit `enabled: false` in YAML freezes everything.
func (p *Policy) IsEnabled() bool {
	if p == nil {
		return false
	}
	if p.Enabled == nil {
		return true
	}
	return *p.Enabled
}

// TestsBaselineOracleEnabled reports whether the tests-stage baseline oracle
// (rerun-without-patch, shepherd B3) should run. Nil-safe; folds the kill
// switch.
func (p *Policy) TestsBaselineOracleEnabled() bool {
	return p != nil && p.IsEnabled() && p.Pipeline.TestsBaselineOracle
}

// MergeQueueEnabled reports whether the serial merge queue should act.
// Nil-safe, defaults to false, and folds in the global kill switch: a frozen
// mills must also freeze the queue processor (entries stay durably queued).
func (p *Policy) MergeQueueEnabled() bool {
	if p == nil {
		return false
	}
	return p.IsEnabled() && p.MergeQueue.Enabled
}

// MergeQueueRequeueEvictions reports whether the queue's eviction re-enqueue
// hop (shepherd A2) is enabled: master enable AND queue enable AND the flag.
func (p *Policy) MergeQueueRequeueEvictions() bool {
	return p.MergeQueueEnabled() && p.MergeQueue.RequeueEvictions
}

// MergeQueueMaxDepth returns the effective lane depth bound.
func (p *Policy) MergeQueueMaxDepth() int {
	if p == nil || p.MergeQueue.MaxDepth <= 0 {
		return DefaultMergeQueueMaxDepth
	}
	return p.MergeQueue.MaxDepth
}

// MergeQueueAwaitPipeline returns the policy's pipeline wait bound for a
// rebased head, or zero when the policy does not set one — the processor
// then applies its own default.
func (p *Policy) MergeQueueAwaitPipeline() time.Duration {
	if p == nil || p.MergeQueue.AwaitPipelineMinutes <= 0 {
		return 0
	}
	return time.Duration(p.MergeQueue.AwaitPipelineMinutes) * time.Minute
}

// MergeQueueSpeculationDepth returns the bounded, opt-in speculative depth.
func (p *Policy) MergeQueueSpeculationDepth() int {
	if p == nil || p.MergeQueue.SpeculationDepth <= 0 {
		return 0
	}
	d := p.MergeQueue.SpeculationDepth
	if max := p.MergeQueueMaxDepth(); d > max {
		return max
	}
	return d
}

// SquadsEnabled reports whether v2 squad routing is on. Nil-safe and
// defaults to false so a v1-policy hot-reloaded onto a v2 binary keeps
// the v1 generic path. Mirrors the spec's `policy.squads.enabled` flag.
func (p *Policy) SquadsEnabled() bool {
	if p == nil {
		return false
	}
	return p.Squads.Enabled
}

// SquadsMinConfidence returns the routing confidence floor from policy, or
// 0 when unset — the router falls back to its compiled-in default. Nil-safe
// so the squads.RoutingPolicy adapter can call it on a cold policy manager.
func (p *Policy) SquadsMinConfidence() float64 {
	if p == nil {
		return 0
	}
	return p.Squads.Routing.MinConfidence
}

// AuditEnabled reports whether the v2 adversarial audit swarm should
// run. Nil-safe and defaults to false. Per the v2.0 spec audit defaults
// to enabled in Default(), but a missing/empty YAML section yields
// false here so the trigger gate fails closed.
func (p *Policy) AuditEnabled() bool {
	if p == nil {
		return false
	}
	return p.Audit.Enabled
}

// AuditAdvisoryOnly reports whether audit findings should never block
// merges. The v2.0 default is true; v2.1 flips it once survival rates
// prove low-noise. Nil-safe and YAML-omission-safe; returns true on a
// nil receiver or when `advisory_only` is omitted, so downstream code
// that forgets to wire the policy never accidentally blocks a merge.
func (p *Policy) AuditAdvisoryOnly() bool {
	if p == nil {
		return true
	}
	if p.Audit.AdvisoryOnly == nil {
		return true
	}
	return *p.Audit.AdvisoryOnly
}

// CouncilRequireRoadmapIntents reports whether a council run must abort when
// its brief was marked intents_missing. Nil-safe and YAML-omission-safe:
// returns true on a nil receiver or an omitted key, so the guardrail fails
// CLOSED. Set `council.require_roadmap_intents: false` as break-glass while
// the extractor backfills the canonical store.
func (p *Policy) CouncilRequireRoadmapIntents() bool {
	if p == nil {
		return basepolicy.DefaultCouncilRequireRoadmapIntents
	}
	return basepolicy.CouncilIntentPolicy{
		RequireRoadmapIntents: p.Council.RequireRoadmapIntents,
	}.RequireRoadmapIntentsEnabled()
}

// defaultCouncilMergedWorkLookbackHours is the merged-MR window, 14 days. Kept
// here rather than in the council package so the policy contract states its own
// default; council.defaultMergedWorkLookback is the same span for callers that
// pass no override.
const defaultCouncilMergedWorkLookbackHours = 14 * 24

// CouncilMergedWorkGroundingEnabled reports whether the council should suppress
// proposals that restate recently-merged work. Nil-safe and YAML-omission-safe:
// returns true on a nil receiver or an omitted key, so the guard is ON by
// default. Opt out with `council.dedup.merged_work.enabled: false`.
func (p *Policy) CouncilMergedWorkGroundingEnabled() bool {
	if p == nil || p.Council.Dedup.MergedWork.Enabled == nil {
		return true
	}
	return *p.Council.Dedup.MergedWork.Enabled
}

// CouncilMergedWorkLookback returns how far back the merged-MR corpus reaches.
// Nil-safe; an omitted or non-positive `lookback_hours` resolves to 14 days.
func (p *Policy) CouncilMergedWorkLookback() time.Duration {
	if p == nil || p.Council.Dedup.MergedWork.LookbackHours <= 0 {
		return defaultCouncilMergedWorkLookbackHours * time.Hour
	}
	return time.Duration(p.Council.Dedup.MergedWork.LookbackHours) * time.Hour
}

// Factory-exhaust bounds, stated policy-side so the contract carries its own
// defaults. council.defaultFactoryExhaustLookback / defaultFactoryExhaustLimit
// are the same values for callers that pass no override.
const (
	defaultCouncilFactoryExhaustLookbackHours = 14 * 24
	defaultCouncilFactoryExhaustMaxItems      = 10
)

// CouncilFactoryExhaustEnabled reports whether the council brief should source
// demand from the factory's own open self-maintenance issues. Nil-safe and
// YAML-omission-safe: returns true on a nil receiver or an omitted key, so the
// source is ON by default — it only adds evidence to a brief, and the council's
// proposals still ride the unchanged dedup and merged-work guards. Opt out with
// `council.sources.factory_exhaust.enabled: false`.
func (p *Policy) CouncilFactoryExhaustEnabled() bool {
	if p == nil || p.Council.Sources.FactoryExhaust.Enabled == nil {
		return true
	}
	return *p.Council.Sources.FactoryExhaust.Enabled
}

// CouncilFactoryExhaustLookback returns how far back the exhaust snapshot
// reaches. Nil-safe; an omitted or non-positive `lookback_hours` is 14 days.
func (p *Policy) CouncilFactoryExhaustLookback() time.Duration {
	if p == nil || p.Council.Sources.FactoryExhaust.LookbackHours <= 0 {
		return defaultCouncilFactoryExhaustLookbackHours * time.Hour
	}
	return time.Duration(p.Council.Sources.FactoryExhaust.LookbackHours) * time.Hour
}

// CouncilFactoryExhaustMaxItems returns how many exhaust items may reach the
// brief. Nil-safe; an omitted or non-positive `max_items` is 10.
func (p *Policy) CouncilFactoryExhaustMaxItems() int {
	if p == nil || p.Council.Sources.FactoryExhaust.MaxItems <= 0 {
		return defaultCouncilFactoryExhaustMaxItems
	}
	return p.Council.Sources.FactoryExhaust.MaxItems
}

// WorkflowsEnabled reports whether the durable imperative workflow runtime
// should advance runs. Nil-safe and YAML-omission-safe: returns false on a nil
// receiver or when the `workflows:` section is omitted, so the runtime fails
// CLOSED (inert) by default — the S6-min default-OFF contract. The S1c canary
// flips policy.workflows.enabled=true in the ConfigMap (hot-reloaded) for the
// canary window only.
func (p *Policy) WorkflowsEnabled() bool {
	if p == nil {
		return false
	}
	return p.Workflows.Enabled
}

// canaryAutopilotDefaultCron is the policy-layer default schedule when the YAML
// leaves it unset (daily 09:00 UTC). The priority/path defaults are owned by
// the store package (store.CanaryDefaultPriority / store.CanaryDefaultFixturePath)
// so the canary's shape has one source of truth shared with the builder.
const canaryAutopilotDefaultCron = "0 9 * * *"

// CanaryAutopilotEnabled reports whether the daily heartbeat-canary scheduler
// should fire. Nil-safe and YAML-omission-safe: returns false on a nil receiver
// or an omitted `intake.canary_autopilot:` block, so the autopilot fails CLOSED
// (inert) by default — the A3-sustain scheduler ships default-OFF.
func (p *Policy) CanaryAutopilotEnabled() bool {
	if p == nil {
		return false
	}
	return p.Intake.CanaryAutopilot.Enabled
}

// CanaryAutopilotCron returns the effective cron expression for the heartbeat
// canary, defaulting to daily 09:00 UTC when unset. Read on every scheduler
// tick so a ConfigMap hot-reload retimes the autopilot without a restart.
func (p *Policy) CanaryAutopilotCron() string {
	if p == nil {
		return canaryAutopilotDefaultCron
	}
	if c := strings.TrimSpace(p.Intake.CanaryAutopilot.ScheduleCron); c != "" {
		return c
	}
	return canaryAutopilotDefaultCron
}

// CanaryAutopilotPriority returns the effective backlog priority for the
// heartbeat canary, defaulting to P3 (the deterministic-canary baseline).
func (p *Policy) CanaryAutopilotPriority() string {
	if p == nil {
		return store.CanaryDefaultPriority
	}
	if pr := strings.TrimSpace(p.Intake.CanaryAutopilot.Priority); pr != "" {
		return pr
	}
	return store.CanaryDefaultPriority
}

// CanaryAutopilotFixturePath returns the effective fixture path the autopilot
// canary may touch, defaulting to the shared heartbeat fixture.
func (p *Policy) CanaryAutopilotFixturePath() string {
	if p == nil {
		return store.CanaryDefaultFixturePath
	}
	if fp := strings.TrimSpace(p.Intake.CanaryAutopilot.FixturePath); fp != "" {
		return fp
	}
	return store.CanaryDefaultFixturePath
}

// plan-slice emitter defaults (.loom/163 S2).
const (
	planSliceEmitterDefaultPhase    = "pending"
	planSliceEmitterDefaultLabel    = "mills-from-plan-slice"
	planSliceEmitterDefaultPriority = "P2"
	planSliceEmitterDefaultPollSecs = 300
	planSliceEmitterDefaultTimeout  = 120
)

// PlanSliceEmitterEnabled reports whether the Plan-Store → backlog emitter
// should run. Nil-safe and YAML-omission-safe: fails CLOSED (default-OFF).
func (p *Policy) PlanSliceEmitterEnabled() bool {
	if p == nil {
		return false
	}
	return p.Intake.PlanSliceEmitter.Enabled
}

// PlanSliceEmitterNamespace returns the namespace gate. Empty = inert: the
// emitter never scans without an explicit namespace, so it can't scoop up
// arbitrary planning-scaffold slices.
func (p *Policy) PlanSliceEmitterNamespace() string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(p.Intake.PlanSliceEmitter.Namespace)
}

// PlanSliceEmitterProject returns the project filter; empty means the caller
// should fall back to the operator's GITLAB_PROJECT.
func (p *Policy) PlanSliceEmitterProject() string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(p.Intake.PlanSliceEmitter.Project)
}

// PlanSliceEmitterReadyPhase returns the slice phase that is eligible to
// emit, defaulting to "pending".
func (p *Policy) PlanSliceEmitterReadyPhase() string {
	if p == nil {
		return planSliceEmitterDefaultPhase
	}
	if s := strings.TrimSpace(p.Intake.PlanSliceEmitter.ReadyPhase); s != "" {
		return s
	}
	return planSliceEmitterDefaultPhase
}

// PlanSliceEmitterLabel returns the label stamped on emitted items.
func (p *Policy) PlanSliceEmitterLabel() string {
	if p == nil {
		return planSliceEmitterDefaultLabel
	}
	if s := strings.TrimSpace(p.Intake.PlanSliceEmitter.Label); s != "" {
		return s
	}
	return planSliceEmitterDefaultLabel
}

// PlanSliceEmitterPriority returns the priority for emitted items.
func (p *Policy) PlanSliceEmitterPriority() string {
	if p == nil {
		return planSliceEmitterDefaultPriority
	}
	if s := strings.TrimSpace(p.Intake.PlanSliceEmitter.Priority); s != "" {
		return s
	}
	return planSliceEmitterDefaultPriority
}

// PlanSliceEmitterPollInterval returns the poll cadence, defaulting to 5min.
func (p *Policy) PlanSliceEmitterPollInterval() time.Duration {
	secs := planSliceEmitterDefaultPollSecs
	if p != nil && p.Intake.PlanSliceEmitter.PollIntervalSeconds > 0 {
		secs = p.Intake.PlanSliceEmitter.PollIntervalSeconds
	}
	return time.Duration(secs) * time.Second
}

// PlanSliceEmitterTickTimeout returns the maximum duration of one complete
// demand scan, defaulting to 2min so a stalled hub cannot wedge the poll loop.
func (p *Policy) PlanSliceEmitterTickTimeout() time.Duration {
	secs := planSliceEmitterDefaultTimeout
	if p != nil && p.Intake.PlanSliceEmitter.TickTimeoutSeconds > 0 {
		secs = p.Intake.PlanSliceEmitter.TickTimeoutSeconds
	}
	return time.Duration(secs) * time.Second
}

// CrossRepoDemandProjects returns the allowlist of NON-home repos the
// plan-slice emitter may source demand from and stamp as TargetProject (S6).
// It enforces the two-key activation invariant in ONE place: the list is
// returned ONLY when cross_repo execution is enabled, so a configured
// allowlist is inert while Enabled is false. Nil-safe; trims blanks and
// drops empty entries. Fails CLOSED (nil) on a nil policy or disabled
// cross-repo.
func (p *Policy) CrossRepoDemandProjects() []string {
	if p == nil || !p.CrossRepo.Enabled {
		return nil
	}
	out := make([]string, 0, len(p.CrossRepo.DemandProjects))
	for _, proj := range p.CrossRepo.DemandProjects {
		if s := strings.TrimSpace(proj); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// CrossRepoBootstrapEnabled reports whether runtime-bootstrapped projects
// (repos minted from a Spinning Room plan via POST /api/mills/projects/
// bootstrap) may source demand and be created at all. Two-key like
// CrossRepoDemandProjects: cross_repo.enabled AND cross_repo.
// allow_bootstrapped must both be set. Fails CLOSED on a nil policy.
func (p *Policy) CrossRepoBootstrapEnabled() bool {
	return p != nil && p.CrossRepo.Enabled && p.CrossRepo.AllowBootstrapped
}

// CrossRepoBootstrapAllowedGroups returns the allow-list of GitLab group paths
// the bootstrap flow may mint repos under. Enforces the same two-key invariant
// as the other cross-repo accessors: the list is returned ONLY when the
// bootstrap two-key gate is on (CrossRepoBootstrapEnabled), so a configured
// allow-list is inert while the gate is off. Nil-safe; trims blanks and drops
// empty entries. Fails CLOSED (nil) on a nil policy, a disabled gate, or an
// empty list.
func (p *Policy) CrossRepoBootstrapAllowedGroups() []string {
	if !p.CrossRepoBootstrapEnabled() {
		return nil
	}
	out := make([]string, 0, len(p.CrossRepo.BootstrapAllowedGroups))
	for _, g := range p.CrossRepo.BootstrapAllowedGroups {
		if s := normalizeGroupPath(g); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// CrossRepoBootstrapGroupAllowed reports whether the bootstrap flow may mint a
// repo under the given GitLab group path. True only when the two-key bootstrap
// gate is on AND group is an exact (case-insensitive, slash-trimmed) member of
// the allow-list. This is the single decision point both the manual endpoint
// and the reconciler's cross-repo pre-flight consult before creating a repo,
// so "bootstrap can mint here" is defined in exactly one place. Fails CLOSED.
func (p *Policy) CrossRepoBootstrapGroupAllowed(group string) bool {
	group = normalizeGroupPath(group)
	if group == "" {
		return false
	}
	for _, g := range p.CrossRepoBootstrapAllowedGroups() {
		if strings.EqualFold(g, group) {
			return true
		}
	}
	return false
}

// normalizeGroupPath trims surrounding whitespace and slashes from a GitLab
// group path so allow-list membership is compared on a canonical form.
func normalizeGroupPath(group string) string {
	return strings.Trim(strings.TrimSpace(group), "/")
}

// TakeupEnabled reports whether the take-up reconciler runs. Default-off.
func (p *Policy) TakeupEnabled() bool {
	if p == nil {
		return false
	}
	return p.Intake.Takeup.Enabled
}

// TakeupNamespace returns the namespace gate. Empty = inert (fail-closed).
func (p *Policy) TakeupNamespace() string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(p.Intake.Takeup.Namespace)
}

// TakeupProject returns the project filter; empty means the caller falls back
// to the operator's GITLAB_PROJECT.
func (p *Policy) TakeupProject() string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(p.Intake.Takeup.Project)
}

// TakeupPollInterval returns the reconcile cadence, defaulting to 5min.
func (p *Policy) TakeupPollInterval() time.Duration {
	secs := planSliceEmitterDefaultPollSecs
	if p != nil && p.Intake.Takeup.PollIntervalSeconds > 0 {
		secs = p.Intake.Takeup.PollIntervalSeconds
	}
	return time.Duration(secs) * time.Second
}

// takeupDefaultTickTimeoutSecs bounds a single reconcile pass. Sized well
// under the 5min default poll interval so a stalled hub/GitLab call surfaces
// as a bounded, logged timeout instead of wedging the goroutine forever.
const takeupDefaultTickTimeoutSecs = 120

// TakeupTickTimeout returns the per-tick deadline, defaulting to 2min.
func (p *Policy) TakeupTickTimeout() time.Duration {
	secs := takeupDefaultTickTimeoutSecs
	if p != nil && p.Intake.Takeup.TickTimeoutSeconds > 0 {
		secs = p.Intake.Takeup.TickTimeoutSeconds
	}
	return time.Duration(secs) * time.Second
}

// spinningRoomDefaultPriority is the warp-beam bucket a spun draft plan gets
// when neither the request nor policy names one.
const spinningRoomDefaultPriority = "P2"

// SpinningRoomEnabled reports whether the operator may spin draft plans from
// the HUD. Default-off (zero value): an omitted `spinning_room:` block leaves
// POST /api/mills/spin returning 503.
func (p *Policy) SpinningRoomEnabled() bool {
	return p != nil && p.SpinningRoom.Enabled
}

// SpinningRoomFrames returns the allowed frames (a copy, trimmed of unnamed
// entries) so a caller can't mutate policy state. Returns nil when the room is
// disabled or no frames are configured.
func (p *Policy) SpinningRoomFrames() []CouncilAgent {
	if p == nil || !p.SpinningRoom.Enabled {
		return nil
	}
	out := make([]CouncilAgent, 0, len(p.SpinningRoom.Frames))
	for _, f := range p.SpinningRoom.Frames {
		if strings.TrimSpace(f.Name) == "" {
			continue
		}
		if f.Fallback != nil {
			fallback := *f.Fallback
			f.Fallback = &fallback
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// SpinningRoomFrame resolves an allowed frame by name (case-insensitive,
// trimmed). ok=false when the room is disabled or no frame carries that name —
// the handler maps that to a 400 so a caller can't spin on an arbitrary model
// off-policy.
func (p *Policy) SpinningRoomFrame(name string) (CouncilAgent, bool) {
	want := strings.ToLower(strings.TrimSpace(name))
	if want == "" {
		return CouncilAgent{}, false
	}
	for _, f := range p.SpinningRoomFrames() {
		if strings.ToLower(strings.TrimSpace(f.Name)) == want {
			return f, true
		}
	}
	return CouncilAgent{}, false
}

// SpinningRoomDefaultPriority returns the draft-plan priority bucket applied
// when a spin request omits one, defaulting to P2.
func (p *Policy) SpinningRoomDefaultPriority() string {
	if p != nil {
		if pr := strings.ToUpper(strings.TrimSpace(p.SpinningRoom.DefaultPriority)); pr != "" {
			return pr
		}
	}
	return spinningRoomDefaultPriority
}

// boolPtr is a tiny convenience for constructing the *bool fields on
// the policy struct from literal values. Defined here so callers
// outside the package don't need to introduce their own helper.
func boolPtr(b bool) *bool { return &b }

// LabelOverrideFor returns the per-label override matching the given labels in
// declaration order. The first match wins. If no override matches, ok=false.
func (p *Policy) LabelOverrideFor(labels []string) (LabelOverride, bool) {
	if p == nil {
		return LabelOverride{}, false
	}
	want := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		want[strings.ToLower(strings.TrimSpace(l))] = struct{}{}
	}
	for _, ov := range p.Pipeline.PerLabelOverrides {
		if _, ok := want[strings.ToLower(ov.Label)]; ok {
			return ov, true
		}
	}
	return LabelOverride{}, false
}

// ProtectedPathsHit returns the subset of input paths that match any pattern
// in pipeline.protected_paths. Used by the path-policy gate to decide whether
// an item must require human review regardless of its label policy.
// Target-repo-agnostic: equivalent to ProtectedPathsHitFor with an empty
// target project (home-repo semantics — global globs only, no per-repo
// overlay). Callers that hold a backlog item should prefer
// ProtectedPathsHitFor with the item's TargetProject so cross-repo runs are
// judged against the target repo's own protected surface.
func (p *Policy) ProtectedPathsHit(paths []string) []string {
	return p.ProtectedPathsHitFor("", paths)
}

// ProtectedPathsFor returns the effective protected-path glob list for a run.
// A matching per-repo entry replaces the global list. Unknown foreign targets
// return a match-all rule so legacy, errorless callers fail closed; new callers
// should use ResolveProtectedPaths and surface its error.
func (p *Policy) ProtectedPathsFor(targetProject string) []string {
	paths, err := p.ResolveProtectedPaths(targetProject)
	if err != nil {
		// Legacy callers cannot return an error. Matching every path preserves
		// the resolver's fail-closed contract until they can surface one.
		return []string{"**"}
	}
	return paths
}

// ResolveProtectedPaths resolves the replacement overlay for targetProject.
// Empty identifies the home repository. A foreign repository is known when it
// appears in cross_repo.demand_projects or has an explicit overlay.
func (p *Policy) ResolveProtectedPaths(targetProject string) ([]string, error) {
	if p == nil {
		return nil, errors.New("protected paths: nil policy")
	}
	global := p.Pipeline.ProtectedPaths
	target := strings.TrimSpace(targetProject)
	if target == "" {
		return global, nil
	}
	for key, overlay := range p.Pipeline.ProtectedPathsPerRepo {
		if !store.SameRepo(key, target) {
			continue
		}
		return overlay, nil
	}
	for _, known := range p.CrossRepo.DemandProjects {
		if store.SameRepo(known, target) {
			return global, nil
		}
	}
	// Runtime-registered repos (the bootstrapped_projects registry: minted by
	// the operator or onboarded from the HUD) are known too — but only under
	// the same two-key gate that lets them source demand. Without this a
	// registered repo could be woven yet every path it touched matched the
	// fail-closed "**" overlay, sending every run to human review.
	if p.CrossRepo.Enabled && p.CrossRepo.AllowBootstrapped {
		if fn := runtimeKnownProjects.Load(); fn != nil {
			for _, known := range (*fn)() {
				if store.SameRepo(known, target) {
					return global, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("protected paths: unknown target repository %q", target)
}

// runtimeKnownProjects is the operator-supplied source of runtime-registered
// foreign projects consulted by ResolveProtectedPaths. A Policy is an
// immutable snapshot with no store access, so the registry reaches it through
// this process-wide hook rather than a field. Nil until the operator wires it.
var runtimeKnownProjects atomic.Pointer[func() []string]

// SetRuntimeKnownProjects installs (or clears, with nil) the runtime source of
// registered foreign projects. The operator wires it to a cached read of the
// bootstrapped_projects registry at boot. Cheap to call on every resolve is
// the caller's contract — cache accordingly.
func SetRuntimeKnownProjects(fn func() []string) {
	if fn == nil {
		runtimeKnownProjects.Store(nil)
		return
	}
	runtimeKnownProjects.Store(&fn)
}

// ProtectedPathsHitFor returns the subset of input paths matching the
// effective protected-path globs for a run targeting targetProject (see
// ProtectedPathsFor). This is the item-aware form every consumer that holds a
// BacklogItem should use — the global-only ProtectedPathsHit misses the
// target repo's own protected surface on cross-repo runs.
func (p *Policy) ProtectedPathsHitFor(targetProject string, paths []string) []string {
	if p == nil {
		return nil
	}
	return ProtectedPathsMatch(p.ProtectedPathsFor(targetProject), paths)
}

// PerRepoOverrideFor returns the execution overlay for targetProject. Empty
// targetProject identifies homeProject, matching BacklogItem routing.
func (p *Policy) PerRepoOverrideFor(targetProject, homeProject string) (RepoExecutionOverride, bool) {
	if p == nil {
		return RepoExecutionOverride{}, false
	}
	target := strings.TrimSpace(targetProject)
	if target == "" {
		target = strings.TrimSpace(homeProject)
	}
	if target == "" {
		return RepoExecutionOverride{}, false
	}
	for key, override := range p.Pipeline.PerRepoOverrides {
		if store.SameRepo(key, target) {
			return override, true
		}
	}
	return RepoExecutionOverride{}, false
}

// RequiresHumanReview reports whether item's effective policy withholds it
// from autonomous admission — either the item's own require_human_review flag
// or the per-repo override for its target. The reason names which rail fired,
// in the wording the reconciler records on its reconciler.skipped event, so
// the tryStart gate and the /api/mills/status queue_held_human count cannot
// disagree about what "held for a human" means.
func (p *Policy) RequiresHumanReview(item *store.BacklogItem, homeProject string) (bool, string) {
	if item == nil {
		return false, ""
	}
	if item.Policy.RequireHumanReview {
		return true, "require_human_review=true"
	}
	if override, ok := p.PerRepoOverrideFor(item.TargetProject, homeProject); ok &&
		override.RequireHumanReview != nil && *override.RequireHumanReview {
		return true, "per-repo override require_human_review=true"
	}
	return false, ""
}

// PipelineBudgetLimitsFor applies the target repo's narrowing caps to the
// global pipeline limits. A zero overlay cap means no additional restriction.
func (p *Policy) PipelineBudgetLimitsFor(targetProject, homeProject string) BudgetLimits {
	if p == nil {
		return BudgetLimits{}
	}
	limits := p.Budgets.Pipeline
	if override, ok := p.PerRepoOverrideFor(targetProject, homeProject); ok {
		limits.MaxUSDPerRun = narrowerFloatCap(limits.MaxUSDPerRun, override.MaxUSDPerRun)
		limits.MaxRunsPerDay = narrowerIntCap(limits.MaxRunsPerDay, override.MaxRunsPerDay)
	}
	return limits
}

// PerRepoRunCapFor returns the additional repository-scoped daily cap. The
// boolean is false when the repository does not configure one, so callers do
// not accidentally reinterpret the global cap as a per-repository cap.
func (p *Policy) PerRepoRunCapFor(targetProject, homeProject string) (int, bool) {
	if p == nil {
		return 0, false
	}
	override, ok := p.PerRepoOverrideFor(targetProject, homeProject)
	if !ok || override.MaxRunsPerDay <= 0 {
		return 0, false
	}
	return narrowerIntCap(p.Budgets.Pipeline.MaxRunsPerDay, override.MaxRunsPerDay), true
}

// AutoMergeFor resolves item and label intent, then applies the per-repo
// narrowing rail. A repo true value is deliberately non-authorizing.
func (p *Policy) AutoMergeFor(targetProject, homeProject string, itemIntent bool, labels []string) bool {
	if p == nil {
		return false
	}
	if override, ok := p.PerRepoOverrideFor(targetProject, homeProject); ok &&
		override.AutoMerge != nil && !*override.AutoMerge {
		return false
	}
	if itemIntent {
		return true
	}
	override, ok := p.LabelOverrideFor(labels)
	return ok && override.AutoMerge
}

func narrowerFloatCap(global, repo float64) float64 {
	if repo > 0 && (global <= 0 || repo < global) {
		return repo
	}
	return global
}

func narrowerIntCap(global, repo int) int {
	if repo > 0 && (global <= 0 || repo < global) {
		return repo
	}
	return global
}

// CooldownDuration is a typed accessor around RetryPolicy.CooldownSeconds.
func (r RetryPolicy) CooldownDuration() time.Duration {
	if r.CooldownSeconds <= 0 {
		return 0
	}
	return time.Duration(r.CooldownSeconds) * time.Second
}

// MergeQueueMainRedExternalHold returns the bounded external-CI hold policy.
func (p *Policy) MergeQueueMainRedExternalHold() basepolicy.MainRedExternalHoldPolicy {
	if p == nil {
		return basepolicy.MainRedExternalHoldPolicy{}
	}
	return basepolicy.MainRedExternalHoldPolicy{HoldMinutes: p.MergeQueue.MainRedExternalHoldMinutes}
}

// AutonomyHoldDuration resolves the bounded hold window even for unvalidated policies.
func (p PipelinePolicy) AutonomyHoldDuration() time.Duration {
	minutes := p.AutonomyHoldMinutes
	if minutes <= 0 {
		minutes = 30
	}
	if minutes > 1440 {
		minutes = 1440
	}
	return time.Duration(minutes) * time.Minute
}
