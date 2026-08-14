package alerting

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

const (
	// maxAlertHistory is the ring buffer cap for alert history.
	maxAlertHistory = 200

	// stuckRuleID is the built-in "pipeline stuck" rule.
	stuckRuleID = "pipeline-stuck"

	// stuckConditionType is the condition type stuck rules evaluate on.
	stuckConditionType = "pipeline_stuck"

	// defaultStuckDuration is how long a pipeline must keep running before the
	// stuck rule fires. Healthy loom-core pipelines routinely take 30-39
	// minutes, so anything tighter flags green runs.
	defaultStuckDuration = 60 * time.Minute
)

// AlertEngine evaluates pipeline state against alert rules and fires alerts.
type AlertEngine struct {
	mu     sync.RWMutex
	rules  []AlertRule
	alerts []Alert
	logger *slog.Logger

	// consecutiveFailures tracks per-project+ref consecutive failure counts.
	consecutiveFailures map[string]int

	// pipelineStartTimes tracks when a pipeline was first seen running.
	pipelineStartTimes map[int]time.Time

	// dispatcher sends alerts to SSE, push, and nudge channels.
	dispatcher *Dispatcher
}

// NewAlertEngine creates an AlertEngine with default rules.
func NewAlertEngine(dispatcher *Dispatcher, logger *slog.Logger) *AlertEngine {
	if logger == nil {
		logger = slog.Default()
	}
	e := &AlertEngine{
		rules:               defaultRules(),
		alerts:              make([]Alert, 0, maxAlertHistory),
		logger:              logger.With("component", "alert-engine"),
		consecutiveFailures: make(map[string]int),
		pipelineStartTimes:  make(map[int]time.Time),
		dispatcher:          dispatcher,
	}
	return e
}

// defaultRules returns the built-in alert rules created on init.
func defaultRules() []AlertRule {
	return []AlertRule{
		{
			ID:      "pipeline-failed",
			Name:    "Pipeline Failed",
			Enabled: true,
			Condition: AlertCondition{
				Type: "pipeline_failed",
			},
			Severity: "warning",
			Cooldown: 5 * time.Minute,
		},
		{
			ID:      "consecutive-failures",
			Name:    "Consecutive Failures",
			Enabled: true,
			Condition: AlertCondition{
				Type:      "consecutive_failures",
				Threshold: 3,
			},
			Severity: "critical",
			Cooldown: 10 * time.Minute,
		},
		{
			ID:      stuckRuleID,
			Name:    "Pipeline Stuck",
			Enabled: true,
			Condition: AlertCondition{
				Type:     stuckConditionType,
				Duration: defaultStuckDuration,
			},
			Severity: "warning",
			Cooldown: 15 * time.Minute,
		},
	}
}

// Evaluate checks all enabled rules against the current pipeline state.
// Call this from PipelineMonitor.OnRefresh with the latest pipeline list.
func (e *AlertEngine) Evaluate(pipelines []bridge.PipelineInfo) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()

	// Clear stuck alerts whose pipeline has since settled. Done before the
	// rule loop so resolution reads the incoming snapshot only, never an
	// alert fired in this same cycle (which requires a running pipeline).
	e.resolveStuckAlerts(pipelines, now)

	for i := range e.rules {
		rule := &e.rules[i]
		if !rule.Enabled {
			continue
		}

		// Cooldown enforcement.
		if !rule.LastFired.IsZero() && now.Sub(rule.LastFired) < rule.Cooldown {
			continue
		}

		alerts := e.evaluateRule(rule, pipelines, now)
		for _, alert := range alerts {
			e.appendAlert(alert)
			rule.LastFired = now
			e.logger.Info("alert fired",
				"rule", rule.Name,
				"severity", alert.Severity,
				"pipeline", alert.Pipeline.ID,
				"project", alert.Pipeline.Project,
			)
			if e.dispatcher != nil {
				go e.dispatcher.Dispatch(alert)
			}
		}
	}

	// Update pipeline start time tracking for stuck detection.
	e.updatePipelineTracking(pipelines, now)
}

// evaluateRule checks a single rule against pipelines and returns any new alerts.
func (e *AlertEngine) evaluateRule(rule *AlertRule, pipelines []bridge.PipelineInfo, now time.Time) []Alert {
	switch rule.Condition.Type {
	case "pipeline_failed":
		return e.evaluatePipelineFailed(rule, pipelines, now)
	case "consecutive_failures":
		return e.evaluateConsecutiveFailures(rule, pipelines, now)
	case "pipeline_stuck":
		return e.evaluatePipelineStuck(rule, pipelines, now)
	default:
		return nil
	}
}

// evaluatePipelineFailed fires for any pipeline with failed status.
func (e *AlertEngine) evaluatePipelineFailed(rule *AlertRule, pipelines []bridge.PipelineInfo, now time.Time) []Alert {
	var alerts []Alert
	for _, p := range pipelines {
		if !matchesProject(rule.Condition.Projects, p.Project) {
			continue
		}
		if normalizePipelineStatus(p.Status) != "failed" {
			continue
		}
		if e.hasUnresolvedAlert(samePipelineAlert(rule.ID, p.ID)) {
			continue
		}
		alerts = append(alerts, Alert{
			ID:       fmt.Sprintf("alert-%d", now.UnixNano()),
			RuleID:   rule.ID,
			RuleName: rule.Name,
			Severity: rule.Severity,
			Title:    fmt.Sprintf("Pipeline failed: %s", p.Project),
			Message:  fmt.Sprintf("Pipeline %d on %s (%s) has failed.", p.ID, p.Project, p.Ref),
			Pipeline: PipelineRef{
				ID:      p.ID,
				Project: p.Project,
				Ref:     p.Ref,
				Status:  p.Status,
				URL:     p.WebURL,
			},
			FiredAt: now,
		})
		// Fire at most once per evaluation cycle for this rule.
		break
	}
	return alerts
}

// evaluateConsecutiveFailures tracks failure streaks per project+ref.
func (e *AlertEngine) evaluateConsecutiveFailures(rule *AlertRule, pipelines []bridge.PipelineInfo, now time.Time) []Alert {
	threshold := rule.Condition.Threshold
	if threshold <= 0 {
		threshold = 3
	}

	// Update consecutive failure counters.
	for _, p := range pipelines {
		if !matchesProject(rule.Condition.Projects, p.Project) {
			continue
		}
		key := p.Project + ":" + p.Ref
		status := normalizePipelineStatus(p.Status)
		switch status {
		case "failed":
			e.consecutiveFailures[key]++
		case "success":
			e.consecutiveFailures[key] = 0
		}
	}

	// Check if any key crossed the threshold.
	var alerts []Alert
	for _, p := range pipelines {
		if !matchesProject(rule.Condition.Projects, p.Project) {
			continue
		}
		key := p.Project + ":" + p.Ref
		count := e.consecutiveFailures[key]
		if count >= threshold && normalizePipelineStatus(p.Status) == "failed" {
			// The streak (project+ref) is the condition's identity, not the
			// individual pipeline: while one streak card is open, each newly
			// failed pipeline extending it must not mint another.
			if e.hasUnresolvedAlert(sameStreakAlert(rule.ID, p.Project, p.Ref)) {
				break
			}
			alerts = append(alerts, Alert{
				ID:       fmt.Sprintf("alert-%d", now.UnixNano()),
				RuleID:   rule.ID,
				RuleName: rule.Name,
				Severity: rule.Severity,
				Title:    fmt.Sprintf("Consecutive failures: %s (%s)", p.Project, p.Ref),
				Message:  fmt.Sprintf("%d consecutive failures on %s (%s).", count, p.Project, p.Ref),
				Pipeline: PipelineRef{
					ID:      p.ID,
					Project: p.Project,
					Ref:     p.Ref,
					Status:  p.Status,
					URL:     p.WebURL,
				},
				FiredAt: now,
			})
			break
		}
	}
	return alerts
}

// evaluatePipelineStuck fires for pipelines running longer than the configured duration.
func (e *AlertEngine) evaluatePipelineStuck(rule *AlertRule, pipelines []bridge.PipelineInfo, now time.Time) []Alert {
	maxDuration := rule.Condition.Duration
	if maxDuration <= 0 {
		maxDuration = defaultStuckDuration
	}

	var alerts []Alert
	for _, p := range pipelines {
		if !matchesProject(rule.Condition.Projects, p.Project) {
			continue
		}
		if normalizePipelineStatus(p.Status) != "running" {
			continue
		}
		startTime, tracked := e.pipelineStartTimes[p.ID]
		if !tracked {
			continue
		}
		if now.Sub(startTime) > maxDuration {
			if e.hasUnresolvedAlert(samePipelineAlert(rule.ID, p.ID)) {
				// Already carded: keep the open card's age honest instead of
				// minting a duplicate (each append also re-notifies via the
				// dispatcher). resolveStuckAlerts closes it on settle, after
				// which a re-stuck pipeline may card again.
				e.refreshStuckAlert(rule.ID, p, now.Sub(startTime))
				continue
			}
			alerts = append(alerts, Alert{
				ID:       fmt.Sprintf("alert-%d", now.UnixNano()),
				RuleID:   rule.ID,
				RuleName: rule.Name,
				Severity: rule.Severity,
				Title:    fmt.Sprintf("Pipeline stuck: %s", p.Project),
				Message:  fmt.Sprintf("Pipeline %d on %s has been running for %s.", p.ID, p.Project, now.Sub(startTime).Truncate(time.Minute)),
				Pipeline: PipelineRef{
					ID:      p.ID,
					Project: p.Project,
					Ref:     p.Ref,
					Status:  p.Status,
					URL:     p.WebURL,
				},
				FiredAt: now,
			})
			break
		}
	}
	return alerts
}

// hasUnresolvedAlert reports whether the ring still holds an unresolved alert
// matching the predicate. Fire-side dedup: cooldown alone only spaces
// duplicates out — a condition persisting across evaluation cycles minted a
// fresh card (and a fresh dispatcher notification) every cooldown window
// until it cleared (2026-08-09: one zombie stuck pipeline stacked 9 identical
// cards). Acked-but-unresolved alerts still suppress: the operator already
// has the card; only the condition recurring after RESOLUTION warrants a new
// one.
//
// Caller must hold e.mu.
func (e *AlertEngine) hasUnresolvedAlert(match func(*Alert) bool) bool {
	for i := range e.alerts {
		if e.alerts[i].ResolvedAt == nil && match(&e.alerts[i]) {
			return true
		}
	}
	return false
}

// samePipelineAlert matches alerts fired by ruleID for one concrete pipeline.
func samePipelineAlert(ruleID string, pipelineID int) func(*Alert) bool {
	return func(a *Alert) bool {
		return a.RuleID == ruleID && a.Pipeline.ID == pipelineID
	}
}

// sameStreakAlert matches alerts fired by ruleID for a project+ref failure
// streak, regardless of which pipeline in the streak carried the card.
func sameStreakAlert(ruleID, project, ref string) func(*Alert) bool {
	return func(a *Alert) bool {
		return a.RuleID == ruleID && a.Pipeline.Project == project && a.Pipeline.Ref == ref
	}
}

// refreshStuckAlert updates the open stuck card's message with the current
// age, so the panel shows one card with a live duration rather than a growing
// stack of point-in-time snapshots.
//
// Caller must hold e.mu.
func (e *AlertEngine) refreshStuckAlert(ruleID string, p bridge.PipelineInfo, age time.Duration) {
	for i := range e.alerts {
		a := &e.alerts[i]
		if a.ResolvedAt == nil && a.RuleID == ruleID && a.Pipeline.ID == p.ID {
			a.Message = fmt.Sprintf("Pipeline %d on %s has been running for %s.", p.ID, p.Project, age.Truncate(time.Minute))
			return
		}
	}
}

// resolveStuckAlerts marks stuck alerts resolved once their pipeline stops
// running -- it either reached a terminal state or dropped off the active
// list. Without this a fired stuck alert sits in the panel forever: the
// embedded PipelineRef is a snapshot from fire time and never updates, so a
// pipeline that passed seconds later still reads as "running".
//
// Caller must hold e.mu.
func (e *AlertEngine) resolveStuckAlerts(pipelines []bridge.PipelineInfo, now time.Time) {
	ruleIDs := e.stuckRuleIDs()

	running := make(map[int]struct{}, len(pipelines))
	settled := make(map[int]string, len(pipelines))
	for _, p := range pipelines {
		if normalizePipelineStatus(p.Status) == "running" {
			running[p.ID] = struct{}{}
			continue
		}
		settled[p.ID] = p.Status
	}

	for i := range e.alerts {
		alert := &e.alerts[i]
		if alert.ResolvedAt != nil {
			continue
		}
		if _, isStuck := ruleIDs[alert.RuleID]; !isStuck {
			continue
		}
		if _, stillRunning := running[alert.Pipeline.ID]; stillRunning {
			continue
		}

		resolvedAt := now
		alert.ResolvedAt = &resolvedAt
		// Refresh the stale snapshot when the terminal status is still
		// visible; a pipeline that aged out of the list keeps its old status.
		if status, ok := settled[alert.Pipeline.ID]; ok && status != "" {
			alert.Pipeline.Status = status
		}
		e.logger.Info("alert resolved",
			"rule", alert.RuleName,
			"pipeline", alert.Pipeline.ID,
			"project", alert.Pipeline.Project,
			"status", alert.Pipeline.Status,
		)
	}
}

// stuckRuleIDs returns the rule IDs whose alerts auto-resolve on settle.
// Rules are replaceable at runtime via PUT /api/alerts/rules, so this tracks
// the condition type rather than the built-in ID alone -- while still
// covering alerts left behind by a stuck rule that has since been removed.
//
// Caller must hold e.mu.
func (e *AlertEngine) stuckRuleIDs() map[string]struct{} {
	ids := map[string]struct{}{stuckRuleID: {}}
	for _, r := range e.rules {
		if r.Condition.Type == stuckConditionType {
			ids[r.ID] = struct{}{}
		}
	}
	return ids
}

// updatePipelineTracking records when pipelines are first seen running
// and prunes entries for pipelines no longer in the list.
func (e *AlertEngine) updatePipelineTracking(pipelines []bridge.PipelineInfo, now time.Time) {
	activeIDs := make(map[int]struct{}, len(pipelines))
	for _, p := range pipelines {
		activeIDs[p.ID] = struct{}{}
		if normalizePipelineStatus(p.Status) == "running" {
			if _, exists := e.pipelineStartTimes[p.ID]; !exists {
				e.pipelineStartTimes[p.ID] = now
			}
		}
	}

	// Prune stale entries.
	for id := range e.pipelineStartTimes {
		if _, active := activeIDs[id]; !active {
			delete(e.pipelineStartTimes, id)
		}
	}
}

// appendAlert adds an alert to the ring buffer, capping at maxAlertHistory.
func (e *AlertEngine) appendAlert(alert Alert) {
	e.alerts = append(e.alerts, alert)
	if len(e.alerts) > maxAlertHistory {
		e.alerts = e.alerts[len(e.alerts)-maxAlertHistory:]
	}
}

// AddRule adds or replaces a rule by ID.
func (e *AlertEngine) AddRule(rule AlertRule) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for i, r := range e.rules {
		if r.ID == rule.ID {
			e.rules[i] = rule
			return
		}
	}
	e.rules = append(e.rules, rule)
}

// RemoveRule removes a rule by ID.
func (e *AlertEngine) RemoveRule(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for i, r := range e.rules {
		if r.ID == id {
			e.rules = append(e.rules[:i], e.rules[i+1:]...)
			return
		}
	}
}

// ListRules returns a copy of all rules.
func (e *AlertEngine) ListRules() []AlertRule {
	e.mu.RLock()
	defer e.mu.RUnlock()

	out := make([]AlertRule, len(e.rules))
	copy(out, e.rules)
	return out
}

// UpdateRules replaces the entire rule set.
func (e *AlertEngine) UpdateRules(rules []AlertRule) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rules = make([]AlertRule, len(rules))
	copy(e.rules, rules)
}

// ListAlerts returns the most recent unresolved alerts, newest-first, capped
// at `limit` (0 or negative means no cap). Resolved alerts stay in the ring so
// AckAlert can still find them, but they are excluded here: this is the active
// view every HUD surface reads, and a settled pipeline is not a live alert.
func (e *AlertEngine) ListAlerts(limit int) []Alert {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if limit <= 0 || limit > len(e.alerts) {
		limit = len(e.alerts)
	}

	// Walk newest first, skipping resolved entries.
	out := make([]Alert, 0, limit)
	for i := len(e.alerts) - 1; i >= 0 && len(out) < limit; i-- {
		if e.alerts[i].ResolvedAt != nil {
			continue
		}
		out = append(out, e.alerts[i])
	}
	return out
}

// AckAlert marks an alert as acknowledged.
func (e *AlertEngine) AckAlert(id, ackedBy string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	for i := range e.alerts {
		if e.alerts[i].ID == id {
			now := time.Now()
			e.alerts[i].AckedAt = &now
			e.alerts[i].AckedBy = ackedBy
			return nil
		}
	}
	return fmt.Errorf("alerting: alert %q not found", id)
}

// matchesProject returns true if the project passes the filter.
// An empty filter matches all projects.
func matchesProject(filter []string, project string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, f := range filter {
		if f == project {
			return true
		}
	}
	return false
}

// normalizePipelineStatus normalizes a pipeline status string.
func normalizePipelineStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running":
		return "running"
	case "success", "passed":
		return "success"
	case "pending", "created", "scheduled", "manual":
		return "pending"
	case "failed", "canceled", "cancelled", "skipped":
		return "failed"
	default:
		return "failed"
	}
}
