package overseer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// Foreman action vocabulary. Committed events use "overseer.foreman.<action>";
// dry-run decisions append ".dryrun" (see ActionRecorder). Observations
// (anomaly opened/cleared) always record under the committed kind so a soak's
// audit trail matches production.
const (
	foremanActor = "overseer.foreman"

	actionAnomalyOpened  = "anomaly_opened"  // observation: a rule started firing
	actionAnomalyCleared = "anomaly_cleared" // observation: a rule stopped firing
	actionForemanPause   = "pause"           // TTL admission-suppression lease
	actionForemanFile    = "file_issue"      // file/update the dedup-marked issue
	actionForemanAlert   = "alert"           // post the anomaly to the notify webhook

	foremanSubjectKind      = "anomaly"
	foremanAdmissionSubject = "admission"

	// foremanMaxPausesPerDay is the hard ceiling on COMMITTED pause assertions
	// in a rolling 24h, read durably from events. A persistent anomaly keeps its
	// single lease alive by re-assertion (which never re-counts); a distinct
	// second episode within 24h cannot open a fresh pause.
	foremanMaxPausesPerDay = 1

	// foremanIssueSummaryMaxTokens bounds the LLM-composed issue body. Compact:
	// summary + impact + suggested action, not an essay.
	foremanIssueSummaryMaxTokens = 400
)

// foremanIssueKey is the synthetic backlog-shaped key embedded in the escalation
// dedup marker so the EXISTING GitLab dedup/auto-close machinery works for
// foreman anomaly issues without new client capabilities (mirrors the sentinel).
func foremanIssueKey(rule string) string { return "overseer-foreman:" + rule }

// WebhookPoster posts an anomaly alert to an external webhook. Satisfied by
// *notify.WebhookHook.PostEvent so the foreman reuses the operator's
// policy.notify.webhook_url config verbatim. An interface (not the concrete
// type) so tests can inject a fake and the overseer package need not import
// notify.
type WebhookPoster interface {
	PostEvent(ctx context.Context, payload map[string]any) error
}

// Foreman is the KPI-anomaly overseer: each tick it evaluates the deterministic
// rules (stuck runs, throughput collapse, escalation storm, budget burn) over
// the store, observes anomaly open/clear transitions, and performs the allowed
// guarded actions — file a dedup-marked issue (body optionally LLM-composed),
// post an alert to the notify webhook, and assert a TTL admission-suppression
// lease (hard-capped once per 24h). Fail-safe posture identical to the sentinel:
// dry-run ⇒ plans only, an LLM outage degrades issue bodies to a template but
// never blocks filing, and every action is audited in the events table.
type Foreman struct {
	Store  *store.Store
	Policy func() *mills.Policy
	// Triage is the nil-safe LLM adapter used ONLY to compose issue bodies. A
	// nil Triage or a failed call falls back to a deterministic template.
	Triage   *Triage
	Recorder *ActionRecorder
	// Issues is optional; pipeline.DedupIssueClient / ClosableIssueClient
	// capabilities are type-asserted like the sentinel does.
	Issues pipeline.IssueClient
	// Webhook is optional; when wired and allow.alert is set the foreman posts
	// each firing anomaly to it.
	Webhook WebhookPoster
	Logger  *slog.Logger
	Now     func() time.Time

	mu sync.Mutex
	// open tracks per-rule firing state in memory (rule name → currently open)
	// so a persistent anomaly acts once per episode, not every tick.
	open map[string]bool
	// issueIID remembers the filed issue per rule for auto-close on clear.
	issueIID    map[string]int64
	suppression atomic.Pointer[Suppression]
	// pausePlanned dedups the dry-run "would pause" event per episode (set on
	// plan, cleared when every anomaly clears). Guarded by mu.
	pausePlanned bool
}

// Name implements Agent.
func (f *Foreman) Name() string { return "foreman" }

// SuppressAdmission reports whether new work admission should be closed by the
// foreman's pause lease. Fail-safe on every axis: no lease, an expired lease, a
// disabled foreman, a dry-run foreman, or a withdrawn allow.pause flag all read
// false (rechecked at read time so a policy hot-reload takes effect instantly).
func (f *Foreman) SuppressAdmission() bool {
	if f == nil {
		return false
	}
	sup := f.suppression.Load()
	if sup == nil || !f.now().Before(sup.Until) {
		return false
	}
	pol := f.policy()
	if pol == nil || !pol.ForemanEnabled() || !pol.Overseers.Foreman.Allow.Pause {
		return false
	}
	if mills.DryRunOn(pol.Overseers.Foreman.DryRun) {
		return false
	}
	return true
}

// CurrentSuppression returns the live lease (nil when none/expired) for the
// status endpoint.
func (f *Foreman) CurrentSuppression() *Suppression {
	if f == nil {
		return nil
	}
	sup := f.suppression.Load()
	if sup == nil || !f.now().Before(sup.Until) {
		return nil
	}
	return sup
}

// Tick implements Agent: evaluate every rule, observe open/clear transitions,
// perform the allowed guarded actions, and (re-)assert or clear the pause lease.
func (f *Foreman) Tick(ctx context.Context) (TickResult, error) {
	res := TickResult{}
	if f == nil || f.Store == nil || f.Recorder == nil {
		return res, errors.New("foreman: not configured")
	}
	pol := f.policy()
	if pol == nil || !pol.ForemanEnabled() {
		return res, nil
	}
	fp := pol.Overseers.Foreman
	dryRun := mills.DryRunOn(fp.DryRun)
	now := f.now()

	// Evaluate all rules outside the lock (store I/O).
	anomalies, inspected, errored := f.evaluate(ctx, fp, pol.Budgets.Pipeline.MaxUSDPerDay, now)
	res.Inspected = inspected
	res.Errored += errored

	firing := make(map[string]*Anomaly, len(anomalies))
	for _, a := range anomalies {
		firing[a.Rule] = a
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.open == nil {
		f.open = map[string]bool{}
	}
	if f.issueIID == nil {
		f.issueIID = map[string]int64{}
	}

	// Clear rules that were open and no longer fire.
	for rule, wasOpen := range f.open {
		if !wasOpen {
			continue
		}
		if _, still := firing[rule]; still {
			continue
		}
		f.clearAnomaly(ctx, &res, rule, dryRun, now)
	}

	// Handle firing rules in a deterministic order.
	for _, rule := range sortedRules(firing) {
		f.handleAnomaly(ctx, &res, fp, firing[rule], dryRun, now)
	}

	// Reconcile the pause lease from the aggregate firing set.
	f.reconcileSuppression(ctx, &res, fp, firing, dryRun, now)
	mills.OverseerSuppressionActive.WithLabelValues(f.Name()).Set(boolGauge(f.suppression.Load() != nil))
	return res, nil
}

// evaluate runs every rule and returns the firing anomalies plus inspected/
// errored counts. A single rule error is logged and skipped — one failing DAO
// read never blinds the other rules.
func (f *Foreman) evaluate(ctx context.Context, fp mills.ForemanPolicy, maxUSDPerDay float64, now time.Time) (anomalies []*Anomaly, inspected, errored int) {
	rules := []func() (*Anomaly, error){
		func() (*Anomaly, error) { return evalStuckRuns(ctx, f.Store, fp, now) },
		func() (*Anomaly, error) { return evalThroughputCollapse(ctx, f.Store, fp, now) },
		func() (*Anomaly, error) { return evalEscalationStorm(ctx, f.Store, fp, now) },
		func() (*Anomaly, error) { return evalBudgetBurn(ctx, f.Store, fp, maxUSDPerDay, now) },
	}
	for _, fn := range rules {
		inspected++
		a, err := fn()
		if err != nil {
			errored++
			if f.Logger != nil {
				f.Logger.Warn("foreman rule eval failed", "error", err)
			}
			continue
		}
		if a != nil {
			anomalies = append(anomalies, a)
		}
	}
	return anomalies, inspected, errored
}

// handleAnomaly records the anomaly_opened observation (once per rule/UTC-day)
// and performs the file_issue + alert actions. Callers hold f.mu.
func (f *Foreman) handleAnomaly(ctx context.Context, res *TickResult, fp mills.ForemanPolicy, a *Anomaly, dryRun bool, now time.Time) {
	newlyOpen := !f.open[a.Rule]
	f.open[a.Rule] = true
	subjectID := anomalySubjectID(a.Rule, now)

	// anomaly_opened: once per (rule, UTC-day) so a persistent anomaly doesn't
	// spam the observation stream. FlagOnce records under the committed kind.
	if _, err := f.Recorder.FlagOnce(ctx, actionAnomalyOpened, foremanSubjectKind, subjectID, a.Evidence); err != nil {
		res.Errored++
	}
	if newlyOpen && f.Logger != nil {
		f.Logger.Warn("foreman anomaly opened", "rule", a.Rule, "severity", a.Severity)
	}

	f.fileIssueAction(ctx, res, fp, a, dryRun, subjectID, newlyOpen)
	f.alertAction(ctx, res, fp, a, dryRun, subjectID, newlyOpen)
}

// fileIssueAction plans (dry-run) or files (committed) the dedup-marked issue
// for one anomaly. Dry-run dedups per (rule, UTC-day) via RecordOnce so a soak
// yields one auditable plan per day; committed acts once per open episode
// (in-memory transition), and the GitLab marker dedups a post-restart re-file
// into a recurrence comment rather than a duplicate issue.
func (f *Foreman) fileIssueAction(ctx context.Context, res *TickResult, fp mills.ForemanPolicy, a *Anomaly, dryRun bool, subjectID string, newlyOpen bool) {
	payload := map[string]any{"severity": a.Severity, "allowed": fp.Allow.FileIssue}
	if dryRun {
		if ok, err := f.Recorder.RecordOnce(ctx, actionForemanFile, foremanSubjectKind, subjectID, payload); err != nil {
			res.Errored++
		} else if ok {
			res.Planned++
		}
		return
	}
	if !newlyOpen {
		return // already handled this episode
	}
	if !fp.Allow.FileIssue || f.Issues == nil {
		res.Skipped++ // anomaly_opened already tells the story; no action event without an action
		return
	}
	iid, err := f.fileIssue(ctx, a)
	if err != nil {
		res.Errored++
		if f.Logger != nil {
			f.Logger.Warn("foreman issue filing failed", "rule", a.Rule, "error", err)
		}
		return
	}
	if iid != 0 {
		f.issueIID[a.Rule] = iid
		payload["issue_iid"] = iid
	}
	if err := f.Recorder.Record(ctx, actionForemanFile, foremanSubjectKind, subjectID, payload); err == nil {
		res.Acted++
	} else {
		res.Errored++
	}
}

// alertAction plans (dry-run) or posts (committed) the anomaly to the notify
// webhook. Same per-(rule,day) dry-run dedup and per-episode committed dedup as
// fileIssueAction.
func (f *Foreman) alertAction(ctx context.Context, res *TickResult, fp mills.ForemanPolicy, a *Anomaly, dryRun bool, subjectID string, newlyOpen bool) {
	payload := map[string]any{"severity": a.Severity, "allowed": fp.Allow.Alert}
	if dryRun {
		if ok, err := f.Recorder.RecordOnce(ctx, actionForemanAlert, foremanSubjectKind, subjectID, payload); err != nil {
			res.Errored++
		} else if ok {
			res.Planned++
		}
		return
	}
	if !newlyOpen {
		return
	}
	if !fp.Allow.Alert || f.Webhook == nil {
		res.Skipped++
		return
	}
	if err := f.Webhook.PostEvent(ctx, f.alertWebhookPayload(a)); err != nil {
		res.Errored++
		if f.Logger != nil {
			f.Logger.Warn("foreman alert post failed", "rule", a.Rule, "error", err)
		}
		return
	}
	if err := f.Recorder.Record(ctx, actionForemanAlert, foremanSubjectKind, subjectID, payload); err == nil {
		res.Acted++
	} else {
		res.Errored++
	}
}

// clearAnomaly records the recovery observation and best-effort closes the
// anomaly's issue. Callers hold f.mu.
func (f *Foreman) clearAnomaly(ctx context.Context, res *TickResult, rule string, dryRun bool, now time.Time) {
	f.open[rule] = false
	if err := f.Recorder.Observe(ctx, actionAnomalyCleared, foremanSubjectKind, anomalySubjectID(rule, now), nil); err != nil {
		res.Errored++
	}
	if f.Logger != nil {
		f.Logger.Info("foreman anomaly cleared", "rule", rule)
	}
	iid := f.issueIID[rule]
	if iid != 0 && !dryRun && f.Issues != nil {
		if closer, ok := f.Issues.(pipeline.ClosableIssueClient); ok && closer != nil {
			if err := closer.CloseIssue(ctx, iid); err != nil && f.Logger != nil {
				f.Logger.Warn("foreman issue close failed", "rule", rule, "iid", iid, "error", err)
			}
		}
	}
	delete(f.issueIID, rule)
}

// reconcileSuppression derives the pause lease from the aggregate firing set:
// any firing anomaly (with allow.pause and dry-run off) re-asserts a fresh TTL
// lease; none clears it. The FIRST committed pause of a rolling 24h opens the
// lease; the day cap (read durably) refuses a second distinct episode within
// the window. Callers hold f.mu.
func (f *Foreman) reconcileSuppression(ctx context.Context, res *TickResult, fp mills.ForemanPolicy, firing map[string]*Anomaly, dryRun bool, now time.Time) {
	rules := sortedRules(firing)
	if len(rules) == 0 {
		f.pausePlanned = false
		if f.suppression.Swap(nil) != nil {
			if err := f.Recorder.Observe(ctx, "suppression_cleared", foremanSubjectKind, foremanAdmissionSubject, nil); err != nil {
				res.Errored++
			}
			if f.Logger != nil {
				f.Logger.Info("foreman admission suppression cleared")
			}
		}
		return
	}

	reason := "anomaly: " + strings.Join(rules, ", ")
	if dryRun || !fp.Allow.Pause {
		// Plan the pause (dry-run only) once per episode, not every tick; a
		// disallowed action records nothing (anomaly_opened tells the story).
		if dryRun && !f.pausePlanned {
			f.pausePlanned = true
			if err := f.Recorder.Record(ctx, actionForemanPause, foremanSubjectKind, foremanAdmissionSubject, map[string]any{
				"reason": reason, "allowed": fp.Allow.Pause,
			}); err != nil {
				res.Errored++
			} else {
				res.Planned++
			}
		} else if !dryRun {
			res.Skipped++
		}
		// Never hold a live lease in dry-run/disallowed mode.
		f.suppression.Store(nil)
		return
	}

	prev := f.suppression.Load()
	if prev == nil {
		// Opening a NEW pause: enforce the rolling-24h hard cap from durable
		// events so a restart cannot reset it.
		used, err := f.Recorder.DayUsed(ctx, now, actionForemanPause)
		if err != nil {
			res.Errored++
			return
		}
		if used >= foremanMaxPausesPerDay {
			res.Skipped++ // cap reached; do not open a second pause this 24h
			return
		}
	}
	lease := &Suppression{Reason: reason, Until: now.Add(fp.SuppressionTTL())}
	f.suppression.Store(lease)
	if prev == nil {
		if err := f.Recorder.Record(ctx, actionForemanPause, foremanSubjectKind, foremanAdmissionSubject, map[string]any{
			"reason": reason, "until": lease.Until.Format(time.RFC3339), "allowed": true,
		}); err != nil {
			res.Errored++
		} else {
			res.Acted++
		}
		if f.Logger != nil {
			f.Logger.Warn("foreman suppressing new work admission", "reason", reason, "until", lease.Until)
		}
	}
}

// fileIssue creates or updates the dedup-marked anomaly issue for a rule,
// reusing the Escalator's marker + capability pattern. The body is LLM-composed
// when Triage is available and falls back to a deterministic template on any
// LLM error or absence — an LLM failure NEVER blocks the filing.
func (f *Foreman) fileIssue(ctx context.Context, a *Anomaly) (int64, error) {
	key := foremanIssueKey(a.Rule)
	body := f.composeIssueBody(ctx, a)
	if dedup, ok := f.Issues.(pipeline.DedupIssueClient); ok && dedup != nil {
		if ref, found, err := dedup.FindOpenEscalation(ctx, key); err == nil && found {
			note := fmt.Sprintf("Recurrence at %s: anomaly `%s` (severity `%s`) still firing.\n\n%s",
				f.now().Format(time.RFC3339), a.Rule, a.Severity, body)
			if cerr := dedup.CommentIssue(ctx, ref.IID, note); cerr != nil {
				return ref.IID, cerr
			}
			return ref.IID, nil
		}
		// A lookup error fails open into a fresh issue, like the Escalator.
	}
	resp, err := f.Issues.CreateIssue(ctx, pipeline.IssueRequest{
		BacklogID: key,
		Title:     fmt.Sprintf("[mills-overseer] KPI anomaly: %s", a.Rule),
		Description: fmt.Sprintf(
			"The Mills foreman detected a `%s` KPI anomaly (severity `%s`).\n\n%s\n\n"+
				"The anomaly auto-clears (and this issue auto-closes) when the rule stops firing.\n\n%s\n",
			a.Rule, a.Severity, body, pipeline.EscalationDedupMarker(key)),
		Labels: []string{"mills-escalation", "mills-overseer"},
	})
	if err != nil {
		return 0, err
	}
	return resp.IID, nil
}

// composeIssueBody asks the judge model to summarise the anomaly, falling back
// to a deterministic evidence template on any error or when no LLM is wired.
func (f *Foreman) composeIssueBody(ctx context.Context, a *Anomaly) string {
	fallback := f.templateBody(a)
	if !f.Triage.Available() {
		return fallback
	}
	evidence, _ := json.Marshal(a.Evidence)
	prompt := fmt.Sprintf(
		"You are the Mills mill-foreman writing a concise incident note for engineers.\n"+
			"A deterministic KPI-anomaly rule fired. Compose a SHORT plain-text note with three labelled lines:\n"+
			"Summary: <one sentence>\nImpact: <one sentence>\nSuggested action: <one sentence>.\n"+
			"Do not invent numbers beyond the evidence. Do not use JSON or markdown code fences.\n\n"+
			"Rule: %s\nSeverity: %s\nEvidence JSON: %s\n",
		a.Rule, a.Severity, string(evidence))
	content, _, err := f.Triage.Client.ChatStructured(ctx, f.Triage.Client.JudgeModel(), prompt, foremanIssueSummaryMaxTokens)
	if err != nil || strings.TrimSpace(content) == "" {
		if err != nil && f.Logger != nil {
			f.Logger.Warn("foreman issue body compose failed; using template", "rule", a.Rule, "error", err)
		}
		return fallback
	}
	return strings.TrimSpace(content)
}

// templateBody renders the deterministic fallback issue body from the evidence
// blob (keys sorted for a stable rendering).
func (f *Foreman) templateBody(a *Anomaly) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Rule**: `%s`\n**Severity**: `%s`\n\n**Evidence**:\n", a.Rule, a.Severity)
	keys := make([]string, 0, len(a.Evidence))
	for k := range a.Evidence {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "- `%s`: %v\n", k, a.Evidence[k])
	}
	return b.String()
}

// alertWebhookPayload renders the Slack/Discord-compatible alert body (the
// `text` field) plus structured fields for richer consumers.
func (f *Foreman) alertWebhookPayload(a *Anomaly) map[string]any {
	return map[string]any{
		"text":     fmt.Sprintf("⚠️ Mills foreman anomaly: %s (severity %s)", a.Rule, a.Severity),
		"source":   "mills-foreman",
		"rule":     a.Rule,
		"severity": a.Severity,
		"evidence": a.Evidence,
	}
}

func (f *Foreman) policy() *mills.Policy {
	if f.Policy == nil {
		return nil
	}
	return f.Policy()
}

func (f *Foreman) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now().UTC()
}

// anomalySubjectID buckets an anomaly's audit subject by rule and UTC day so
// the once-per-day observation/plan dedup is durable and restart-safe.
func anomalySubjectID(rule string, now time.Time) string {
	return rule + ":" + now.UTC().Format("2006-01-02")
}

// sortedRules returns the firing rule names in a deterministic order.
func sortedRules(firing map[string]*Anomaly) []string {
	rules := make([]string, 0, len(firing))
	for r := range firing {
		rules = append(rules, r)
	}
	sort.Strings(rules)
	return rules
}
