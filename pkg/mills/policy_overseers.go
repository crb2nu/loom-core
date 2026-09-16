package mills

import (
	"errors"
	"fmt"
	"time"

	"github.com/crb2nu/loom/pkg/mills/textsim"
)

// This file is the policy contract for the Mills overseers — the supervisory
// agents that groom the backlog (groomer), watch dependency health (sentinel),
// and monitor pipeline KPIs for anomalies (foreman). The whole section follows
// the Intake/Workflows optional-section pattern: an omitted `overseers:` block
// yields the zero value, which disables every agent. Guard rails are layered:
// a master gate, a per-agent enable, a per-agent dry-run that DEFAULTS ON
// (nil *bool == dry-run), per-action-class allow flags that default off, and
// per-tick/per-day action caps with hard ceilings. Hot-reloads via the policy
// ConfigMap watcher like every other section.

// Overseer defaults + hard ceilings. Conservative on purpose: an overseer is
// unattended automation acting on the work queue itself, so its unattended
// blast radius must stay small even under a fat-fingered policy.
const (
	overseerGroomerDefaultIntervalMinutes  = 60
	overseerSentinelDefaultIntervalMinutes = 5
	overseerForemanDefaultIntervalMinutes  = 15
	// overseerMinIntervalMinutes bounds how hot any overseer loop can spin.
	overseerMinIntervalMinutes = 1
	// overseerMaxIntervalMinutes bounds how far out a loop can be pushed
	// before it is effectively disabled (use enabled:false for that).
	overseerMaxIntervalMinutes = 24 * 60

	overseerGroomerDefaultTickCap  = 5
	overseerGroomerMaxTickCap      = 20
	overseerGroomerDefaultDayCap   = 20
	overseerGroomerMaxDayCap       = 100
	overseerGroomerDefaultLLMCalls = 8
	overseerGroomerMaxLLMCalls     = 32

	overseerGroomerDefaultZombieQueuedDays  = 14
	overseerGroomerDefaultStalePriorityDays = 7

	// overseerGroomerDefaultDedupThreshold is the hard-duplicate Jaccard bar.
	// Pairs at/above it are deterministic duplicates; pairs in
	// [textsim.GrayBandFloor, threshold) go to the LLM gray-band verdict.
	overseerGroomerDefaultDedupThreshold = 0.85

	overseerSentinelDefaultProbeTimeoutSeconds  = 10
	overseerSentinelMaxProbeTimeoutSeconds      = 120
	overseerSentinelDefaultTripsToOpen          = 3
	overseerSentinelMaxTripsToOpen              = 20
	overseerSentinelDefaultSuppressionTTLMin    = 30
	overseerSentinelMaxSuppressionTTLMin        = 24 * 60
	overseerSandboxDrillDefaultIntervalMinutes  = 6 * 60
	overseerSandboxDrillDefaultBudgetSeconds    = 900
	overseerSandboxDrillMaxBudgetSeconds        = 3600
	overseerSandboxDrillDefaultConcurrency      = 2
	overseerSandboxDrillMaxConcurrency          = 8
	overseerSandboxDrillDefaultThrottlePercent  = 25
	overseerForemanDefaultStuckRunHours         = 4
	overseerForemanDefaultZeroMergeHours        = 24
	overseerForemanDefaultEscalationStorm24h    = 10
	overseerForemanDefaultBudgetBurnRatio       = 0.9
	overseerForemanDefaultSuppressionTTLMinutes = 60

	overseerShepherdDefaultIntervalMinutes = 30
	overseerShepherdDefaultTickCap         = 3
	overseerShepherdMaxTickCap             = 10
	overseerShepherdDefaultDayCap          = 6
	overseerShepherdMaxDayCap              = 24
	overseerShepherdDefaultMinShelfHours   = 24
	overseerShepherdDefaultOrphanMRDays    = 3
)

// OverseersPolicy gates the three supervisory agents. Zero value = fully off.
type OverseersPolicy struct {
	// Enabled is the master gate for every overseer. Plain bool (not *bool):
	// an omitted `overseers:` block must disable the section, matching the
	// Workflows/SpinningRoom optional-section pattern.
	Enabled      bool               `yaml:"enabled,omitempty"`
	Groomer      GroomerPolicy      `yaml:"groomer,omitempty"`
	Sentinel     SentinelPolicy     `yaml:"sentinel,omitempty"`
	Foreman      ForemanPolicy      `yaml:"foreman,omitempty"`
	Shepherd     ShepherdPolicy     `yaml:"shepherd,omitempty"`
	SandboxDrill SandboxDrillPolicy `yaml:"sandbox_drill,omitempty"`
}

// SandboxDrillPolicy configures the scheduled concurrent devbox sentinel.
type SandboxDrillPolicy struct {
	Enabled                     bool   `yaml:"enabled,omitempty"`
	IntervalMinutes             int    `yaml:"interval_minutes,omitempty"`
	BudgetSeconds               int    `yaml:"budget_seconds,omitempty"`
	Concurrency                 int    `yaml:"concurrency,omitempty"`
	Project                     string `yaml:"project,omitempty"`
	CPUThrottlingCeilingPercent int    `yaml:"cpu_throttling_ceiling_percent,omitempty"`
}

// GroomerPolicy configures the backlog groomer: duplicate retire, obsolete
// retire, priority adjustment, and zombie flagging over the queued backlog.
type GroomerPolicy struct {
	Enabled bool `yaml:"enabled,omitempty"`
	// DryRun is *bool so an OMITTED key means dry-run ON — the fail-safe
	// default. In dry-run every would-be action is recorded as a
	// `overseer.groomer.<action>.dryrun` event and nothing mutates.
	DryRun          *bool `yaml:"dry_run,omitempty"`
	IntervalMinutes int   `yaml:"interval_minutes,omitempty"`
	// MaxActionsPerTick / MaxActionsPerDay cap committed (non-dry-run)
	// actions. The day cap is read from durable events so it survives a
	// restart, like the auto-requeue sweep's.
	MaxActionsPerTick  int `yaml:"max_actions_per_tick,omitempty"`
	MaxActionsPerDay   int `yaml:"max_actions_per_day,omitempty"`
	MaxLLMCallsPerTick int `yaml:"max_llm_calls_per_tick,omitempty"`
	// ZombieQueuedDays: a queued item older than this with zero pipeline runs
	// is flagged (event-only) and, when allow.close_obsolete, judged for
	// retirement.
	ZombieQueuedDays int `yaml:"zombie_queued_days,omitempty"`
	// StalePriorityDays: a P0/P1 item untouched this long becomes a
	// deterministic demotion candidate.
	StalePriorityDays int `yaml:"stale_priority_days,omitempty"`
	// DedupAutoThreshold is the hard-duplicate Jaccard bar; must stay above
	// the council-mirrored gray-band floor (0.55). Values in
	// [floor, threshold) require an LLM verdict.
	DedupAutoThreshold float64            `yaml:"dedup_auto_threshold,omitempty"`
	Allow              GroomerAllowPolicy `yaml:"allow,omitempty"`
}

// GroomerAllowPolicy is the per-action-class opt-in. All default false: an
// enabled, non-dry-run groomer with an empty allow block still only flags.
type GroomerAllowPolicy struct {
	// DedupClose permits retiring the younger of a duplicate pair.
	DedupClose bool `yaml:"dedup_close,omitempty"`
	// CloseObsolete permits retiring LLM-judged-obsolete zombies.
	CloseObsolete bool `yaml:"close_obsolete,omitempty"`
	// Reprioritize permits adjacent-bucket priority moves.
	Reprioritize bool `yaml:"reprioritize,omitempty"`
}

// SentinelPolicy configures the deployment-health sentinel's probe loop
// (wired in cmd/loom-mills-operator alongside the groomer and foreman).
type SentinelPolicy struct {
	Enabled             bool  `yaml:"enabled,omitempty"`
	DryRun              *bool `yaml:"dry_run,omitempty"`
	IntervalMinutes     int   `yaml:"interval_minutes,omitempty"`
	ProbeTimeoutSeconds int   `yaml:"probe_timeout_seconds,omitempty"`
	// TripsToOpen is how many CONSECUTIVE probe failures open an incident.
	TripsToOpen int `yaml:"trips_to_open,omitempty"`
	// SuppressionTTLMinutes bounds how long a single suppression assertion
	// lives. The sentinel re-asserts each tick while unhealthy; expiry is the
	// dead-man's switch when the sentinel itself dies mid-incident.
	SuppressionTTLMinutes int                 `yaml:"suppression_ttl_minutes,omitempty"`
	Allow                 SentinelAllowPolicy `yaml:"allow,omitempty"`
}

// SentinelAllowPolicy is the sentinel's per-action-class opt-in.
type SentinelAllowPolicy struct {
	// SuppressAdmission permits closing new work admission while unhealthy.
	SuppressAdmission bool `yaml:"suppress_admission,omitempty"`
	// FileIssue permits filing/updating a dedup-marked GitLab incident issue.
	FileIssue bool `yaml:"file_issue,omitempty"`
}

// ForemanPolicy configures the mill foreman's KPI anomaly rules (wired in
// cmd/loom-mills-operator alongside the groomer and sentinel).
type ForemanPolicy struct {
	Enabled         bool  `yaml:"enabled,omitempty"`
	DryRun          *bool `yaml:"dry_run,omitempty"`
	IntervalMinutes int   `yaml:"interval_minutes,omitempty"`
	// StuckRunHours: an active pipeline run older than this is anomalous.
	StuckRunHours int `yaml:"stuck_run_hours,omitempty"`
	// ZeroMergeHours: queue non-empty + zero merges for this long is anomalous.
	ZeroMergeHours int `yaml:"zero_merge_hours,omitempty"`
	// EscalationStorm24h: >= this many escalations in 24h is anomalous.
	EscalationStorm24h int `yaml:"escalation_storm_24h,omitempty"`
	// BudgetBurnRatio: 1d pipeline cost / budgets.pipeline.max_usd_per_day at
	// or above this ratio is anomalous.
	BudgetBurnRatio float64            `yaml:"budget_burn_ratio,omitempty"`
	Allow           ForemanAllowPolicy `yaml:"allow,omitempty"`
}

// ForemanAllowPolicy is the foreman's per-action-class opt-in.
type ForemanAllowPolicy struct {
	// Pause permits TTL-bounded new-work-admission suppression (hard-capped
	// once per rolling 24h; never the GitOps kill-switch, which stays human).
	Pause bool `yaml:"pause,omitempty"`
	// FileIssue permits filing/updating a dedup-marked GitLab anomaly issue.
	FileIssue bool `yaml:"file_issue,omitempty"`
	// Alert permits posting to the policy.notify webhook.
	Alert bool `yaml:"alert,omitempty"`
}

// ShepherdPolicy configures the escalated-shelf shepherd: bounded relaunch of
// escalated items the auto-requeue sweep structurally cannot reach, and
// attention flags for closed-MR orphans. Fail-safe posture matches its
// siblings: default-OFF section, dry-run default ON, every action audited.
type ShepherdPolicy struct {
	Enabled bool `yaml:"enabled,omitempty"`
	// DryRun is *bool so an OMITTED key means dry-run ON — the fail-safe
	// default. In dry-run every would-be relaunch is recorded as an
	// `overseer.shepherd.relaunch.dryrun` event and nothing mutates.
	DryRun          *bool `yaml:"dry_run,omitempty"`
	IntervalMinutes int   `yaml:"interval_minutes,omitempty"`
	// MaxActionsPerTick / MaxActionsPerDay cap committed relaunches. The day
	// cap is read from durable events so it survives a restart.
	MaxActionsPerTick int `yaml:"max_actions_per_tick,omitempty"`
	MaxActionsPerDay  int `yaml:"max_actions_per_day,omitempty"`
	// MinShelfHours: an escalated item whose latest run ended more recently
	// than this is never touched — the auto-requeue sweep and fresh triage
	// own the young shelf.
	MinShelfHours int `yaml:"min_shelf_hours,omitempty"`
	// OrphanMRDays: an item whose MR was closed (not merged) at least this
	// long ago earns an attention flag (event-only, never a state change).
	OrphanMRDays int                 `yaml:"orphan_mr_days,omitempty"`
	Allow        ShepherdAllowPolicy `yaml:"allow,omitempty"`
}

// ShepherdAllowPolicy is the shepherd's per-action-class opt-in. Default
// false: an enabled, non-dry-run shepherd with an empty allow block still
// only flags.
type ShepherdAllowPolicy struct {
	// Relaunch permits the guarded escalated→queued transition.
	Relaunch bool `yaml:"relaunch,omitempty"`
	// ScopeWiden permits CAS-widening a scope-escalated item's slices when
	// every recorded violation is admissible under CURRENT policy, then
	// requeueing it. The run adopts its existing rescue branch and the
	// merge-stage recoverable states un-draft the rescue MR — the shepherd
	// only owns the decision the draft template asks a human for.
	ScopeWiden bool `yaml:"scope_widen,omitempty"`
}

// GroomerEnabled reports whether the groomer loop should act: master gate AND
// per-agent enable. Nil-safe like the other Policy accessors.
func (p *Policy) GroomerEnabled() bool {
	return p != nil && p.Overseers.Enabled && p.Overseers.Groomer.Enabled
}

// SentinelEnabled reports whether the sentinel loop should act.
func (p *Policy) SentinelEnabled() bool {
	return p != nil && p.Overseers.Enabled && p.Overseers.Sentinel.Enabled
}

// ForemanEnabled reports whether the foreman loop should act.
func (p *Policy) ForemanEnabled() bool {
	return p != nil && p.Overseers.Enabled && p.Overseers.Foreman.Enabled
}

// ShepherdEnabled reports whether the shepherd loop should act.
func (p *Policy) ShepherdEnabled() bool {
	return p != nil && p.Overseers.Enabled && p.Overseers.Shepherd.Enabled
}

// SandboxDrillEnabled reports whether the master and drill gates are enabled.
func (p *Policy) SandboxDrillEnabled() bool {
	return p != nil && p.Overseers.Enabled && p.Overseers.SandboxDrill.Enabled
}

func (s SandboxDrillPolicy) Interval() time.Duration {
	return overseerInterval(s.IntervalMinutes, overseerSandboxDrillDefaultIntervalMinutes)
}

func (s SandboxDrillPolicy) Budget() time.Duration {
	return time.Duration(capWithDefault(s.BudgetSeconds, overseerSandboxDrillDefaultBudgetSeconds, overseerSandboxDrillMaxBudgetSeconds)) * time.Second
}

func (s SandboxDrillPolicy) GateConcurrency() int {
	return capWithDefault(s.Concurrency, overseerSandboxDrillDefaultConcurrency, overseerSandboxDrillMaxConcurrency)
}

func (s SandboxDrillPolicy) DrillProject() string {
	if s.Project == "" {
		return "loom-core"
	}
	return s.Project
}

func (s SandboxDrillPolicy) ThrottlingCeiling() int {
	return capWithDefault(s.CPUThrottlingCeilingPercent, overseerSandboxDrillDefaultThrottlePercent, 100)
}

// DryRunOn resolves a *bool dry-run flag with its default-ON semantics: nil
// means dry-run. Shared by all three agents so the fail-safe rule lives in
// one place.
func DryRunOn(v *bool) bool { return v == nil || *v }

// overseerInterval resolves an interval_minutes field against its default and
// the shared min/max clamps.
func overseerInterval(minutes, def int) time.Duration {
	m := minutes
	if m <= 0 {
		m = def
	}
	if m < overseerMinIntervalMinutes {
		m = overseerMinIntervalMinutes
	}
	if m > overseerMaxIntervalMinutes {
		m = overseerMaxIntervalMinutes
	}
	return time.Duration(m) * time.Minute
}

// Interval returns the groomer's tick cadence (default 60m, clamped).
func (g GroomerPolicy) Interval() time.Duration {
	return overseerInterval(g.IntervalMinutes, overseerGroomerDefaultIntervalMinutes)
}

// TickCap returns the groomer's per-tick committed-action cap (default 5,
// clamped to the hard ceiling).
func (g GroomerPolicy) TickCap() int {
	return capWithDefault(g.MaxActionsPerTick, overseerGroomerDefaultTickCap, overseerGroomerMaxTickCap)
}

// DayCap returns the groomer's rolling-24h committed-action cap (default 20,
// clamped to the hard ceiling).
func (g GroomerPolicy) DayCap() int {
	return capWithDefault(g.MaxActionsPerDay, overseerGroomerDefaultDayCap, overseerGroomerMaxDayCap)
}

// LLMCallCap returns the groomer's per-tick LLM verdict budget (default 8).
func (g GroomerPolicy) LLMCallCap() int {
	return capWithDefault(g.MaxLLMCallsPerTick, overseerGroomerDefaultLLMCalls, overseerGroomerMaxLLMCalls)
}

// ZombieAge returns how old a runless queued item must be to count as a
// zombie (default 14 days).
func (g GroomerPolicy) ZombieAge() time.Duration {
	d := g.ZombieQueuedDays
	if d <= 0 {
		d = overseerGroomerDefaultZombieQueuedDays
	}
	return time.Duration(d) * 24 * time.Hour
}

// StalePriorityAge returns how long a P0/P1 item may sit untouched before it
// becomes a demotion candidate (default 7 days).
func (g GroomerPolicy) StalePriorityAge() time.Duration {
	d := g.StalePriorityDays
	if d <= 0 {
		d = overseerGroomerDefaultStalePriorityDays
	}
	return time.Duration(d) * 24 * time.Hour
}

// DedupThreshold returns the hard-duplicate Jaccard bar (default 0.85). The
// validated range keeps it strictly above the gray-band floor.
func (g GroomerPolicy) DedupThreshold() float64 {
	if g.DedupAutoThreshold <= 0 {
		return overseerGroomerDefaultDedupThreshold
	}
	return g.DedupAutoThreshold
}

// Interval returns the sentinel's tick cadence (default 5m, clamped).
func (s SentinelPolicy) Interval() time.Duration {
	return overseerInterval(s.IntervalMinutes, overseerSentinelDefaultIntervalMinutes)
}

// ProbeTimeout returns the per-probe timeout (default 10s, clamped).
func (s SentinelPolicy) ProbeTimeout() time.Duration {
	return time.Duration(capWithDefault(s.ProbeTimeoutSeconds,
		overseerSentinelDefaultProbeTimeoutSeconds, overseerSentinelMaxProbeTimeoutSeconds)) * time.Second
}

// TripThreshold returns how many consecutive failures open an incident
// (default 3, clamped).
func (s SentinelPolicy) TripThreshold() int {
	return capWithDefault(s.TripsToOpen, overseerSentinelDefaultTripsToOpen, overseerSentinelMaxTripsToOpen)
}

// SuppressionTTL returns the admission-suppression lease duration (default
// 30m, clamped).
func (s SentinelPolicy) SuppressionTTL() time.Duration {
	return time.Duration(capWithDefault(s.SuppressionTTLMinutes,
		overseerSentinelDefaultSuppressionTTLMin, overseerSentinelMaxSuppressionTTLMin)) * time.Minute
}

// Interval returns the foreman's tick cadence (default 15m, clamped).
func (f ForemanPolicy) Interval() time.Duration {
	return overseerInterval(f.IntervalMinutes, overseerForemanDefaultIntervalMinutes)
}

// StuckRunAge returns the stuck-run anomaly age (default 4h).
func (f ForemanPolicy) StuckRunAge() time.Duration {
	h := f.StuckRunHours
	if h <= 0 {
		h = overseerForemanDefaultStuckRunHours
	}
	return time.Duration(h) * time.Hour
}

// ZeroMergeWindow returns the throughput-collapse window (default 24h).
func (f ForemanPolicy) ZeroMergeWindow() time.Duration {
	h := f.ZeroMergeHours
	if h <= 0 {
		h = overseerForemanDefaultZeroMergeHours
	}
	return time.Duration(h) * time.Hour
}

// StormThreshold returns the 24h escalation-storm count (default 10).
func (f ForemanPolicy) StormThreshold() int {
	if f.EscalationStorm24h <= 0 {
		return overseerForemanDefaultEscalationStorm24h
	}
	return f.EscalationStorm24h
}

// BurnRatio returns the budget-burn anomaly ratio (default 0.9).
func (f ForemanPolicy) BurnRatio() float64 {
	if f.BudgetBurnRatio <= 0 {
		return overseerForemanDefaultBudgetBurnRatio
	}
	return f.BudgetBurnRatio
}

// SuppressionTTL returns the foreman's pause-lease duration (fixed 60m). The
// foreman's pause is hard-capped at once per rolling 24h and re-asserted every
// tick while the triggering anomaly persists, so the TTL is only the dead-man's
// switch — a foreman that dies mid-anomaly can never suppress admission past its
// last lease.
func (f ForemanPolicy) SuppressionTTL() time.Duration {
	return time.Duration(overseerForemanDefaultSuppressionTTLMinutes) * time.Minute
}

// Interval returns the shepherd's tick cadence (default 30m, clamped).
func (s ShepherdPolicy) Interval() time.Duration {
	return overseerInterval(s.IntervalMinutes, overseerShepherdDefaultIntervalMinutes)
}

// TickCap returns the shepherd's per-tick committed-relaunch cap (default 3).
func (s ShepherdPolicy) TickCap() int {
	return capWithDefault(s.MaxActionsPerTick, overseerShepherdDefaultTickCap, overseerShepherdMaxTickCap)
}

// DayCap returns the shepherd's rolling-24h committed-relaunch cap (default 6).
func (s ShepherdPolicy) DayCap() int {
	return capWithDefault(s.MaxActionsPerDay, overseerShepherdDefaultDayCap, overseerShepherdMaxDayCap)
}

// MinShelfAge returns how old an escalation must be before the shepherd may
// touch it (default 24h).
func (s ShepherdPolicy) MinShelfAge() time.Duration {
	h := s.MinShelfHours
	if h <= 0 {
		h = overseerShepherdDefaultMinShelfHours
	}
	return time.Duration(h) * time.Hour
}

// OrphanAge returns how stale a closed-MR orphan must be to earn an attention
// flag (default 3 days).
func (s ShepherdPolicy) OrphanAge() time.Duration {
	d := s.OrphanMRDays
	if d <= 0 {
		d = overseerShepherdDefaultOrphanMRDays
	}
	return time.Duration(d) * 24 * time.Hour
}

// capWithDefault resolves a positive-int cap field: <=0 means the default,
// anything above the ceiling clamps down to it.
func capWithDefault(v, def, ceiling int) int {
	if v <= 0 {
		v = def
	}
	if v > ceiling {
		v = ceiling
	}
	return v
}

// validateOverseers enforces the rules a malformed overseers section must
// trip on. Interval/cap fields are clamped by their accessors rather than
// rejected (0 = default); only values that would silently change semantics
// (a dedup bar inside or below the gray band, negative numbers) are errors.
func validateOverseers(o OverseersPolicy) error {
	if o.Groomer.DedupAutoThreshold != 0 &&
		(o.Groomer.DedupAutoThreshold <= textsim.GrayBandFloor || o.Groomer.DedupAutoThreshold > 1) {
		return fmt.Errorf("overseers.groomer.dedup_auto_threshold (%v) must be in (%v, 1]",
			o.Groomer.DedupAutoThreshold, textsim.GrayBandFloor)
	}
	for name, v := range map[string]int{
		"overseers.groomer.interval_minutes":                     o.Groomer.IntervalMinutes,
		"overseers.groomer.max_actions_per_tick":                 o.Groomer.MaxActionsPerTick,
		"overseers.groomer.max_actions_per_day":                  o.Groomer.MaxActionsPerDay,
		"overseers.groomer.max_llm_calls_per_tick":               o.Groomer.MaxLLMCallsPerTick,
		"overseers.groomer.zombie_queued_days":                   o.Groomer.ZombieQueuedDays,
		"overseers.groomer.stale_priority_days":                  o.Groomer.StalePriorityDays,
		"overseers.sentinel.interval_minutes":                    o.Sentinel.IntervalMinutes,
		"overseers.sentinel.probe_timeout_seconds":               o.Sentinel.ProbeTimeoutSeconds,
		"overseers.sentinel.trips_to_open":                       o.Sentinel.TripsToOpen,
		"overseers.sentinel.suppression_ttl_minutes":             o.Sentinel.SuppressionTTLMinutes,
		"overseers.sandbox_drill.interval_minutes":               o.SandboxDrill.IntervalMinutes,
		"overseers.sandbox_drill.budget_seconds":                 o.SandboxDrill.BudgetSeconds,
		"overseers.sandbox_drill.concurrency":                    o.SandboxDrill.Concurrency,
		"overseers.sandbox_drill.cpu_throttling_ceiling_percent": o.SandboxDrill.CPUThrottlingCeilingPercent,
		"overseers.foreman.interval_minutes":                     o.Foreman.IntervalMinutes,
		"overseers.foreman.stuck_run_hours":                      o.Foreman.StuckRunHours,
		"overseers.foreman.zero_merge_hours":                     o.Foreman.ZeroMergeHours,
		"overseers.foreman.escalation_storm_24h":                 o.Foreman.EscalationStorm24h,
		"overseers.shepherd.interval_minutes":                    o.Shepherd.IntervalMinutes,
		"overseers.shepherd.max_actions_per_tick":                o.Shepherd.MaxActionsPerTick,
		"overseers.shepherd.max_actions_per_day":                 o.Shepherd.MaxActionsPerDay,
		"overseers.shepherd.min_shelf_hours":                     o.Shepherd.MinShelfHours,
		"overseers.shepherd.orphan_mr_days":                      o.Shepherd.OrphanMRDays,
	} {
		if v < 0 {
			return errors.New(name + " must be >= 0")
		}
	}
	if o.Foreman.BudgetBurnRatio < 0 || o.Foreman.BudgetBurnRatio > 1 {
		return fmt.Errorf("overseers.foreman.budget_burn_ratio (%v) must be in [0, 1]", o.Foreman.BudgetBurnRatio)
	}
	return nil
}
