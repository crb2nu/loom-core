package overseer

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// fakeChatClient is a scriptable ChatClient for the issue-body path. When err
// is set every call fails, forcing the deterministic-template fallback.
type fakeChatClient struct {
	reply string
	err   error
	calls int
}

func (c *fakeChatClient) ChatStructured(_ context.Context, _ string, _ string, _ int) (string, float64, error) {
	c.calls++
	if c.err != nil {
		return "", 0, c.err
	}
	return c.reply, 0.001, nil
}
func (c *fakeChatClient) JudgeModel() string { return "fake-judge" }

// fakeWebhook records the alert payloads the foreman posts.
type fakeWebhook struct {
	posts []map[string]any
	err   error
}

func (w *fakeWebhook) PostEvent(_ context.Context, payload map[string]any) error {
	if w.err != nil {
		return w.err
	}
	w.posts = append(w.posts, payload)
	return nil
}

type foremanEnv struct {
	store   *store.Store
	foreman *Foreman
	policy  *mills.Policy
	issues  *fakeIssues
	webhook *fakeWebhook
	chat    *fakeChatClient
	now     time.Time
}

func newForemanEnv(t *testing.T, fp mills.ForemanPolicy) *foremanEnv {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "f.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	pol := mills.Default()
	pol.Overseers = mills.OverseersPolicy{Enabled: true, Foreman: fp}
	env := &foremanEnv{
		store:   st,
		policy:  pol,
		issues:  &fakeIssues{comments: map[int64][]string{}, openByKey: map[string]int64{}},
		webhook: &fakeWebhook{},
		chat:    &fakeChatClient{reply: "Summary: x\nImpact: y\nSuggested action: z"},
		now:     time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC),
	}
	env.foreman = &Foreman{
		Store:  st,
		Policy: func() *mills.Policy { return env.policy },
		Recorder: &ActionRecorder{
			Events: st.Events,
			Actor:  foremanActor,
			DryRun: func() bool { return mills.DryRunOn(env.policy.Overseers.Foreman.DryRun) },
		},
		Now: func() time.Time { return env.now },
	}
	return env
}

func (e *foremanEnv) tick(t *testing.T) TickResult {
	t.Helper()
	res, err := e.foreman.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	return res
}

func (e *foremanEnv) countKind(t *testing.T, kind string) int {
	t.Helper()
	n, err := e.store.Events.CountByKindSince(context.Background(), kind, e.now.Add(-48*time.Hour))
	if err != nil {
		t.Fatalf("count %s: %v", kind, err)
	}
	return n
}

// seedRun inserts a pipeline run in an arbitrary state/startedAt/cost. It also
// seeds the backing backlog item (pipeline_runs.backlog_id has a FK), in the
// RUNNING state so it never perturbs the queued-depth the throughput rule reads.
func (e *foremanEnv) seedRun(t *testing.T, id string, state store.PipelineState, startedAt time.Time, cost float64) {
	t.Helper()
	backlogID := "BL-" + id
	if err := e.store.Backlog.Put(context.Background(), &store.BacklogItem{
		ID: backlogID, Title: id, State: store.BacklogRunning, Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed backlog %s: %v", backlogID, err)
	}
	if err := e.store.Pipeline.PutRun(context.Background(), &store.PipelineRun{
		ID: id, BacklogID: backlogID, Template: "t", State: state,
		Attempts: 1, StartedAt: startedAt, CostUSD: cost,
	}); err != nil {
		t.Fatalf("seed run %s: %v", id, err)
	}
}

// seedQueued inserts a queued backlog item.
func (e *foremanEnv) seedQueued(t *testing.T, id string) {
	t.Helper()
	if err := e.store.Backlog.Put(context.Background(), &store.BacklogItem{
		ID: id, Title: id, State: store.BacklogQueued, Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed queued %s: %v", id, err)
	}
}

// --- Rule firing / not-firing -------------------------------------------------

func TestForemanStuckRunsRule(t *testing.T) {
	env := newForemanEnv(t, mills.ForemanPolicy{Enabled: true, DryRun: boolPtr(false)})
	fp := env.policy.Overseers.Foreman

	// A run within the 4h threshold is not stuck; a terminal run never counts.
	env.seedRun(t, "FRESH", store.PipelineImplementing, env.now.Add(-1*time.Hour), 0)
	env.seedRun(t, "DONE", store.PipelineDone, env.now.Add(-6*time.Hour), 0)
	if a, _ := evalStuckRuns(context.Background(), env.store, fp, env.now); a != nil {
		t.Fatalf("stuck_runs fired with no stuck run: %+v", a)
	}

	// An active run past the threshold fires the anomaly.
	env.seedRun(t, "STUCK", store.PipelineImplementing, env.now.Add(-6*time.Hour), 0)
	a, err := evalStuckRuns(context.Background(), env.store, fp, env.now)
	if err != nil {
		t.Fatalf("evalStuckRuns: %v", err)
	}
	if a == nil {
		t.Fatal("expected stuck_runs anomaly")
	}
	if got := a.Evidence["count"].(int); got != 1 {
		t.Fatalf("stuck count = %d, want 1", got)
	}
	if ids, ok := a.Evidence["run_ids"].([]string); !ok || len(ids) != 1 || ids[0] != "STUCK" {
		t.Fatalf("run_ids = %v", a.Evidence["run_ids"])
	}
}

func TestForemanThroughputCollapseRule(t *testing.T) {
	env := newForemanEnv(t, mills.ForemanPolicy{Enabled: true, DryRun: boolPtr(false)})
	fp := env.policy.Overseers.Foreman

	// Empty queue ⇒ no collapse even with zero merges.
	if a, _ := evalThroughputCollapse(context.Background(), env.store, fp, env.now); a != nil {
		t.Fatalf("collapse with empty queue: %+v", a)
	}

	// Queue non-empty + zero merges in window ⇒ collapse.
	env.seedQueued(t, "Q1")
	env.seedQueued(t, "Q2")
	a, err := evalThroughputCollapse(context.Background(), env.store, fp, env.now)
	if err != nil {
		t.Fatalf("collapse: %v", err)
	}
	if a == nil {
		t.Fatal("expected throughput_collapse anomaly")
	}
	if got := a.Evidence["queued_depth"].(int); got != 2 {
		t.Fatalf("queued_depth = %d, want 2", got)
	}

	// A merge OUTSIDE the window does not clear the collapse.
	env.seedRun(t, "OLD", store.PipelineDone, env.now.Add(-30*time.Hour), 0)
	if a, _ := evalThroughputCollapse(context.Background(), env.store, fp, env.now); a == nil {
		t.Fatal("stale out-of-window merge cleared the collapse")
	}

	// A merge in-window clears it.
	env.seedRun(t, "MERGED", store.PipelineDone, env.now.Add(-1*time.Hour), 0)
	if a, _ := evalThroughputCollapse(context.Background(), env.store, fp, env.now); a != nil {
		t.Fatalf("collapse after a merge: %+v", a)
	}
}

func TestForemanEscalationStormRule(t *testing.T) {
	env := newForemanEnv(t, mills.ForemanPolicy{Enabled: true, DryRun: boolPtr(false), EscalationStorm24h: 3})
	fp := env.policy.Overseers.Foreman

	env.seedRun(t, "E1", store.PipelineEscalated, env.now.Add(-1*time.Hour), 0)
	env.seedRun(t, "E2", store.PipelineEscalated, env.now.Add(-2*time.Hour), 0)
	if a, _ := evalEscalationStorm(context.Background(), env.store, fp, env.now); a != nil {
		t.Fatalf("storm below threshold: %+v", a)
	}
	env.seedRun(t, "E3", store.PipelineEscalated, env.now.Add(-3*time.Hour), 0)
	a, err := evalEscalationStorm(context.Background(), env.store, fp, env.now)
	if err != nil {
		t.Fatalf("storm: %v", err)
	}
	if a == nil || a.Evidence["count"].(int) != 3 {
		t.Fatalf("expected storm at 3, got %+v", a)
	}
	// One outside the 24h window does not count.
	env.seedRun(t, "E-OLD", store.PipelineEscalated, env.now.Add(-25*time.Hour), 0)
	a, _ = evalEscalationStorm(context.Background(), env.store, fp, env.now)
	if a == nil || a.Evidence["count"].(int) != 3 {
		t.Fatalf("stale escalation counted: %+v", a)
	}
}

func TestForemanBudgetBurnRule(t *testing.T) {
	env := newForemanEnv(t, mills.ForemanPolicy{Enabled: true, DryRun: boolPtr(false), BudgetBurnRatio: 0.9})
	fp := env.policy.Overseers.Foreman

	// max_usd_per_day unset ⇒ rule skipped.
	if a, _ := evalBudgetBurn(context.Background(), env.store, fp, 0, env.now); a != nil {
		t.Fatalf("burn fired with unset budget: %+v", a)
	}

	// $50 spent against a $100 budget = 0.5 < 0.9 ⇒ no burn.
	env.seedRun(t, "C1", store.PipelineDone, env.now.Add(-1*time.Hour), 50)
	if a, _ := evalBudgetBurn(context.Background(), env.store, fp, 100, env.now); a != nil {
		t.Fatalf("burn at 0.5: %+v", a)
	}
	// Another $45 → $95/$100 = 0.95 ≥ 0.9 ⇒ burn.
	env.seedRun(t, "C2", store.PipelineImplementing, env.now.Add(-2*time.Hour), 45)
	a, err := evalBudgetBurn(context.Background(), env.store, fp, 100, env.now)
	if err != nil {
		t.Fatalf("burn: %v", err)
	}
	if a == nil {
		t.Fatal("expected budget_burn anomaly")
	}
	if r := a.Evidence["ratio"].(float64); r < 0.9 {
		t.Fatalf("ratio = %v, want >= 0.9", r)
	}
}

// --- Committed actions + dedup ------------------------------------------------

// A firing anomaly with file_issue + alert allowed files one dedup-marked issue
// and posts one webhook alert; anomaly_opened is observed once; on clear the
// issue auto-closes.
func TestForemanCommittedActionsAndClear(t *testing.T) {
	env := newForemanEnv(t, mills.ForemanPolicy{
		Enabled: true, DryRun: boolPtr(false), EscalationStorm24h: 2,
		Allow: mills.ForemanAllowPolicy{FileIssue: true, Alert: true},
	})
	env.foreman.Issues = env.issues
	env.foreman.Webhook = env.webhook

	env.seedRun(t, "E1", store.PipelineEscalated, env.now.Add(-1*time.Hour), 0)
	env.seedRun(t, "E2", store.PipelineEscalated, env.now.Add(-2*time.Hour), 0)

	res := env.tick(t)
	if res.Acted < 2 {
		t.Fatalf("expected >=2 committed actions, got %+v", res)
	}
	if len(env.issues.created) != 1 {
		t.Fatalf("issues created = %d, want 1", len(env.issues.created))
	}
	if env.issues.created[0].BacklogID != "overseer-foreman:escalation_storm" {
		t.Fatalf("issue key = %s", env.issues.created[0].BacklogID)
	}
	if len(env.webhook.posts) != 1 {
		t.Fatalf("webhook posts = %d, want 1", len(env.webhook.posts))
	}
	if env.countKind(t, "overseer.foreman.anomaly_opened") != 1 {
		t.Fatal("anomaly_opened not observed exactly once")
	}

	// Second tick, still firing: no duplicate issue, no duplicate alert.
	env.tick(t)
	if len(env.issues.created) != 1 {
		t.Fatalf("duplicate issue filed: %d", len(env.issues.created))
	}
	if len(env.webhook.posts) != 1 {
		t.Fatalf("duplicate alert posted: %d", len(env.webhook.posts))
	}

	// Clear the anomaly: escalations age out of the window.
	env.now = env.now.Add(25 * time.Hour)
	env.tick(t)
	if len(env.issues.closed) != 1 {
		t.Fatalf("issue closes = %d, want 1", len(env.issues.closed))
	}
	if env.countKind(t, "overseer.foreman.anomaly_cleared") != 1 {
		t.Fatal("anomaly_cleared not observed")
	}
}

// anomaly_opened is observed at most once per (rule, UTC-day) even across many
// ticks of a persistent anomaly.
func TestForemanAnomalyOpenedDayDedup(t *testing.T) {
	env := newForemanEnv(t, mills.ForemanPolicy{
		Enabled: true, DryRun: boolPtr(false), EscalationStorm24h: 1,
	})
	env.seedRun(t, "E1", store.PipelineEscalated, env.now.Add(-1*time.Hour), 0)
	env.tick(t)
	env.tick(t)
	env.tick(t)
	if got := env.countKind(t, "overseer.foreman.anomaly_opened"); got != 1 {
		t.Fatalf("anomaly_opened events = %d, want 1", got)
	}
}

// --- Pause lease --------------------------------------------------------------

// A firing anomaly with allow.pause asserts a live suppression lease; it clears
// when the anomaly clears.
func TestForemanPauseSuppression(t *testing.T) {
	env := newForemanEnv(t, mills.ForemanPolicy{
		Enabled: true, DryRun: boolPtr(false), EscalationStorm24h: 1,
		Allow: mills.ForemanAllowPolicy{Pause: true},
	})
	env.seedRun(t, "E1", store.PipelineEscalated, env.now.Add(-1*time.Hour), 0)
	env.tick(t)
	if !env.foreman.SuppressAdmission() {
		t.Fatal("expected admission suppressed while anomaly fires")
	}
	sup := env.foreman.CurrentSuppression()
	if sup == nil || sup.Reason != "anomaly: escalation_storm" {
		t.Fatalf("suppression = %+v", sup)
	}
	// Clear: escalation ages out.
	env.now = env.now.Add(25 * time.Hour)
	env.tick(t)
	if env.foreman.SuppressAdmission() {
		t.Fatal("still suppressed after clear")
	}
}

// The committed pause is hard-capped at once per rolling 24h, read durably: a
// pre-seeded committed pause event blocks a fresh pause this window.
func TestForemanPauseDayCap(t *testing.T) {
	env := newForemanEnv(t, mills.ForemanPolicy{
		Enabled: true, DryRun: boolPtr(false), EscalationStorm24h: 1,
		Allow: mills.ForemanAllowPolicy{Pause: true},
	})
	// Pre-seed a committed pause 2h ago (inside the rolling 24h window).
	if err := env.store.Events.Append(context.Background(), &store.Event{
		Actor: foremanActor, Kind: "overseer.foreman.pause",
		SubjectKind: foremanSubjectKind, SubjectID: foremanAdmissionSubject,
		OccurredAt: env.now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("seed pause: %v", err)
	}
	env.seedRun(t, "E1", store.PipelineEscalated, env.now.Add(-1*time.Hour), 0)
	res := env.tick(t)
	if env.foreman.SuppressAdmission() {
		t.Fatal("second pause opened despite 24h cap")
	}
	if res.Skipped == 0 {
		t.Fatalf("expected the capped pause to be skipped: %+v", res)
	}
}

// A foreman that dies mid-anomaly (no more ticks re-asserting) cannot suppress
// admission past its lease TTL.
func TestForemanPauseDeadMan(t *testing.T) {
	env := newForemanEnv(t, mills.ForemanPolicy{
		Enabled: true, DryRun: boolPtr(false), EscalationStorm24h: 1,
		Allow: mills.ForemanAllowPolicy{Pause: true},
	})
	env.seedRun(t, "E1", store.PipelineEscalated, env.now.Add(-1*time.Hour), 0)
	env.tick(t)
	if !env.foreman.SuppressAdmission() {
		t.Fatal("not suppressed")
	}
	// The foreman "dies": no more ticks. The clock passes the 60m TTL.
	env.now = env.now.Add(61 * time.Minute)
	if env.foreman.SuppressAdmission() {
		t.Fatal("dead foreman still suppressing past TTL")
	}
}

// --- Dry-run ------------------------------------------------------------------

// Dry-run plans every would-be action (.dryrun events), holds no live lease,
// files no issue, posts no alert, and does not re-plan the same episode.
func TestForemanDryRunPlansOnly(t *testing.T) {
	env := newForemanEnv(t, mills.ForemanPolicy{
		Enabled: true, EscalationStorm24h: 1, // DryRun nil ⇒ ON
		Allow: mills.ForemanAllowPolicy{FileIssue: true, Alert: true, Pause: true},
	})
	env.foreman.Issues = env.issues
	env.foreman.Webhook = env.webhook

	env.seedRun(t, "E1", store.PipelineEscalated, env.now.Add(-1*time.Hour), 0)
	res := env.tick(t)
	if res.Planned == 0 {
		t.Fatalf("no planned actions in dry-run: %+v", res)
	}
	if env.foreman.SuppressAdmission() {
		t.Fatal("dry-run held a live suppression")
	}
	if len(env.issues.created) != 0 || len(env.webhook.posts) != 0 {
		t.Fatalf("dry-run performed side effects: issues=%d posts=%d", len(env.issues.created), len(env.webhook.posts))
	}

	// Further ticks must not re-plan the same episode/day.
	env.tick(t)
	env.tick(t)
	if got := env.countKind(t, "overseer.foreman.pause.dryrun"); got != 1 {
		t.Fatalf("planned pause events = %d, want 1", got)
	}
	if got := env.countKind(t, "overseer.foreman.file_issue.dryrun"); got != 1 {
		t.Fatalf("planned file_issue events = %d, want 1", got)
	}
	if got := env.countKind(t, "overseer.foreman.alert.dryrun"); got != 1 {
		t.Fatalf("planned alert events = %d, want 1", got)
	}
	// Observations record under committed kinds even in dry-run.
	if got := env.countKind(t, "overseer.foreman.anomaly_opened"); got != 1 {
		t.Fatalf("anomaly_opened events = %d, want 1", got)
	}
}

// --- LLM fallback -------------------------------------------------------------

// When the LLM errors, the issue is still filed with the deterministic template
// body — an LLM failure never blocks filing.
func TestForemanIssueBodyLLMFallback(t *testing.T) {
	env := newForemanEnv(t, mills.ForemanPolicy{
		Enabled: true, DryRun: boolPtr(false), EscalationStorm24h: 1,
		Allow: mills.ForemanAllowPolicy{FileIssue: true},
	})
	env.foreman.Issues = env.issues
	env.chat.err = errors.New("flexinfer down")
	env.foreman.Triage = &Triage{Client: env.chat}

	env.seedRun(t, "E1", store.PipelineEscalated, env.now.Add(-1*time.Hour), 0)
	env.tick(t)
	if len(env.issues.created) != 1 {
		t.Fatalf("issues created = %d, want 1 despite LLM error", len(env.issues.created))
	}
	body := env.issues.created[0].Description
	if !strings.Contains(body, "**Rule**: `escalation_storm`") {
		t.Fatalf("issue body did not fall back to template:\n%s", body)
	}
	if env.chat.calls == 0 {
		t.Fatal("LLM was not attempted before falling back")
	}
}

// When the LLM succeeds, its reply composes the issue body.
func TestForemanIssueBodyLLMUsed(t *testing.T) {
	env := newForemanEnv(t, mills.ForemanPolicy{
		Enabled: true, DryRun: boolPtr(false), EscalationStorm24h: 1,
		Allow: mills.ForemanAllowPolicy{FileIssue: true},
	})
	env.foreman.Issues = env.issues
	env.foreman.Triage = &Triage{Client: env.chat}

	env.seedRun(t, "E1", store.PipelineEscalated, env.now.Add(-1*time.Hour), 0)
	env.tick(t)
	if len(env.issues.created) != 1 {
		t.Fatalf("issues created = %d", len(env.issues.created))
	}
	if !strings.Contains(env.issues.created[0].Description, "Summary: x") {
		t.Fatalf("LLM body not used:\n%s", env.issues.created[0].Description)
	}
}

// --- Gates --------------------------------------------------------------------

// A disabled foreman is inert.
func TestForemanDisabledNoop(t *testing.T) {
	env := newForemanEnv(t, mills.ForemanPolicy{Enabled: false, DryRun: boolPtr(false), EscalationStorm24h: 1})
	env.seedRun(t, "E1", store.PipelineEscalated, env.now.Add(-1*time.Hour), 0)
	res := env.tick(t)
	if res.Inspected != 0 || res.Acted != 0 || res.Planned != 0 {
		t.Fatalf("disabled foreman acted: %+v", res)
	}
}

// TestForemanRuleWiring proves the four rules are wired into the tick via
// evaluate: Inspected counts every rule each tick.
func TestForemanRuleWiring(t *testing.T) {
	env := newForemanEnv(t, mills.ForemanPolicy{Enabled: true, DryRun: boolPtr(false), EscalationStorm24h: 1})
	env.seedRun(t, "E1", store.PipelineEscalated, env.now.Add(-1*time.Hour), 0)
	res := env.tick(t)
	if res.Inspected != 4 {
		t.Fatalf("Inspected = %d, want 4 (one per rule)", res.Inspected)
	}
}

// TestForemanEscalationStormDiscountsSupersededVerdicts proves Trustworthy
// Verdicts S3: an escalation whose verdict was superseded (its MR merged) no
// longer feeds the storm counter, and the discount is visible in evidence.
func TestForemanEscalationStormDiscountsSupersededVerdicts(t *testing.T) {
	env := newForemanEnv(t, mills.ForemanPolicy{Enabled: true, DryRun: boolPtr(false), EscalationStorm24h: 3})
	fp := env.policy.Overseers.Foreman
	ctx := context.Background()

	env.seedRun(t, "S1", store.PipelineEscalated, env.now.Add(-1*time.Hour), 0)
	env.seedRun(t, "S2", store.PipelineEscalated, env.now.Add(-2*time.Hour), 0)
	env.seedRun(t, "S3", store.PipelineEscalated, env.now.Add(-3*time.Hour), 0)
	if a, _ := evalEscalationStorm(ctx, env.store, fp, env.now); a == nil {
		t.Fatal("storm must fire at threshold before any correction")
	}

	// Supersede one via the explicit verdict event: the storm drops below
	// threshold and stays quiet.
	if err := env.store.Events.Append(ctx, &store.Event{
		Actor: "reconciler", Kind: mills.RunVerdictKindGhostSparkMerged,
		SubjectKind: "pipeline_run", SubjectID: "S2",
		OccurredAt: env.now.Add(-30 * time.Minute),
	}); err != nil {
		t.Fatalf("append correction: %v", err)
	}
	if a, err := evalEscalationStorm(ctx, env.store, fp, env.now); err != nil || a != nil {
		t.Fatalf("superseded escalation must not feed the storm: a=%+v err=%v", a, err)
	}

	// A fourth active escalation re-arms it; evidence shows the discount.
	// The legacy closure kind must discount identically.
	env.seedRun(t, "S4", store.PipelineEscalated, env.now.Add(-4*time.Hour), 0)
	env.seedRun(t, "S5", store.PipelineEscalated, env.now.Add(-5*time.Hour), 0)
	if err := env.store.Events.Append(ctx, &store.Event{
		Actor: "reconciler", Kind: mills.GhostSparkClosedEventKind,
		SubjectKind: "pipeline_run", SubjectID: "S4",
		OccurredAt: env.now.Add(-20 * time.Minute),
	}); err != nil {
		t.Fatalf("append legacy correction: %v", err)
	}
	a, err := evalEscalationStorm(ctx, env.store, fp, env.now)
	if err != nil {
		t.Fatalf("storm: %v", err)
	}
	if a == nil || a.Evidence["count"].(int) != 3 {
		t.Fatalf("active count wrong: %+v", a)
	}
	if a.Evidence["raw_count"].(int) != 5 || a.Evidence["superseded"].(int) != 2 {
		t.Fatalf("discount must be visible in evidence: %+v", a.Evidence)
	}
}
