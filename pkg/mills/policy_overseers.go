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
	overseerForemanDefaultStuckRunHours         = 4
	overseerForemanDefaultZeroMergeHours        = 24
	overseerForemanDefaultEscalationStorm24h    = 10
	overseerForemanDefaultBudgetBurnRatio       = 0.9
	overseerForemanDefaultSuppressionTTLMinutes = 60
)

// OverseersPolicy gates the three supervisory agents. Zero value = fully off.
type OverseersPolicy struct {
	// Enabled is the master gate for every overseer. Plain bool (not *bool):
	// an omitted `overseers:` block must disable the section, matching the
	// Workflows/SpinningRoom optional-section pattern.
	Enabled  bool           `yaml:"enabled,omitempty"`
	Groomer  GroomerPolicy  `yaml:"groomer,omitempty"`
	Sentinel SentinelPolicy `yaml:"sentinel,omitempty"`
	Foreman  ForemanPolicy  `yaml:"foreman,omitempty"`
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
		"overseers.groomer.interval_minutes":         o.Groomer.IntervalMinutes,
		"overseers.groomer.max_actions_per_tick":     o.Groomer.MaxActionsPerTick,
		"overseers.groomer.max_actions_per_day":      o.Groomer.MaxActionsPerDay,
		"overseers.groomer.max_llm_calls_per_tick":   o.Groomer.MaxLLMCallsPerTick,
		"overseers.groomer.zombie_queued_days":       o.Groomer.ZombieQueuedDays,
		"overseers.groomer.stale_priority_days":      o.Groomer.StalePriorityDays,
		"overseers.sentinel.interval_minutes":        o.Sentinel.IntervalMinutes,
		"overseers.sentinel.probe_timeout_seconds":   o.Sentinel.ProbeTimeoutSeconds,
		"overseers.sentinel.trips_to_open":           o.Sentinel.TripsToOpen,
		"overseers.sentinel.suppression_ttl_minutes": o.Sentinel.SuppressionTTLMinutes,
		"overseers.foreman.interval_minutes":         o.Foreman.IntervalMinutes,
		"overseers.foreman.stuck_run_hours":          o.Foreman.StuckRunHours,
		"overseers.foreman.zero_merge_hours":         o.Foreman.ZeroMergeHours,
		"overseers.foreman.escalation_storm_24h":     o.Foreman.EscalationStorm24h,
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
