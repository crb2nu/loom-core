package mills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func dwellHistogramCount(t *testing.T, outcome string) uint64 {
	t.Helper()
	observer, err := ExternalIncidentDwellDurationSeconds.GetMetricWithLabelValues(outcome)
	if err != nil {
		t.Fatal(err)
	}
	metric, ok := observer.(prometheus.Metric)
	if !ok {
		t.Fatalf("dwell observer does not implement prometheus.Metric")
	}
	var dtoMetric dto.Metric
	if err := metric.Write(&dtoMetric); err != nil {
		t.Fatal(err)
	}
	return dtoMetric.GetHistogram().GetSampleCount()
}

// seedRequeueSpec describes one escalated backlog item + its most-recent run for
// the auto-requeue tests.
type seedRequeueSpec struct {
	id           string
	class        string // escalation_class stamped on the run ("" == unclassified)
	endedAgo     time.Duration
	extDep       string // external_dependency name (marks an external incident)
	mrIID        int64  // > 0 attaches an MR to the run
	priorRequeue int    // pre-seed N prior auto_requeued events for this item
	retryable    *bool  // classifier retryable verdict on the run
	costUSD      float64
}

// seedEscalatedForRequeue inserts an escalated backlog item and its escalated
// run, keyed off env.now for deterministic cooldown/window math.
func seedEscalatedForRequeue(t *testing.T, env *recTestEnv, spec seedRequeueSpec) {
	t.Helper()
	ctx := context.Background()
	item := &store.BacklogItem{
		ID:        spec.id,
		Title:     "escalated " + spec.id,
		State:     store.BacklogEscalated,
		Priority:  store.P2,
		CreatedBy: "test",
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed item %s: %v", spec.id, err)
	}
	ended := env.now.Add(-spec.endedAgo)
	run := &store.PipelineRun{
		ID:              "PIPE-" + spec.id,
		BacklogID:       spec.id,
		Template:        "mills-default-pipeline",
		State:           store.PipelineEscalated,
		Attempts:        1,
		StartedAt:       ended.Add(-time.Minute),
		EndedAt:         &ended,
		EscalationClass: spec.class,
	}
	run.EscalationRetryable = spec.retryable
	run.CostUSD = spec.costUSD
	if spec.extDep != "" {
		run.ExternalDependency = spec.extDep
	}
	if spec.mrIID > 0 {
		iid := spec.mrIID
		run.MRIID = &iid
	}
	if err := env.store.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed run %s: %v", spec.id, err)
	}
	for i := 0; i < spec.priorRequeue; i++ {
		if err := env.store.Events.Append(ctx, &store.Event{
			OccurredAt:  env.now.Add(-time.Duration(i+1) * time.Minute),
			Actor:       "reconciler",
			Kind:        eventKindAutoRequeued,
			SubjectKind: autoRequeueSubjectKind,
			SubjectID:   spec.id,
			Payload:     map[string]any{"backlog_id": spec.id},
		}); err != nil {
			t.Fatalf("seed prior requeue event %s: %v", spec.id, err)
		}
	}
}

// newAutoRequeueEnv builds a recTestEnv whose loaded policy is the given YAML —
// used by the caps tests that need budgets/caps the pinned fixtureV1 can't
// express. Mirrors newRecEnv otherwise.
func newAutoRequeueEnv(t *testing.T, policyYAML string) *recTestEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(dir, "h.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	policyPath := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(policyPath, []byte(policyYAML), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	pm, err := NewPolicyManager(context.Background(), policyPath, PolicyManagerOptions{SkipWatch: true})
	if err != nil {
		t.Fatalf("policy mgr: %v", err)
	}
	t.Cleanup(func() { _ = pm.Close() })
	starter := &recordingStarter{}
	rec := NewReconciler(st, pm, NewBudget(pm, NewStoreBudgetReader(st)), starter)
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	rec.Clock = func() time.Time { return now }
	return &recTestEnv{store: st, policy: pm, starter: starter, rec: rec, now: now}
}

func autoRequeuePolicyYAML(maxRunsPerDay, cooldownMin, perItem, perDay int) string {
	return "version: 2\nenabled: true\n" +
		"budgets:\n" +
		"  council:  { max_usd_per_run: 15, max_usd_per_day: 50 }\n" +
		"  pipeline: { max_usd_per_run: 5, max_usd_per_day: 75, max_concurrent_runs: 4, max_runs_per_day: " + itoa(maxRunsPerDay) + " }\n" +
		"pipeline:\n" +
		"  default_template: mills-default-pipeline\n" +
		"  retry: { max_attempts: 3, cooldown_seconds: 300 }\n" +
		"  auto_requeue: { enabled: true, cooldown_minutes: " + itoa(cooldownMin) + ", per_item_max: " + itoa(perItem) + ", per_day_max: " + itoa(perDay) + " }\n"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func autoRequeueEventCount(t *testing.T, env *recTestEnv, itemID string) int {
	t.Helper()
	n, err := env.store.Events.CountBySubjectKind(context.Background(), autoRequeueSubjectKind, itemID, eventKindAutoRequeued)
	if err != nil {
		t.Fatalf("count events %s: %v", itemID, err)
	}
	return n
}

func backlogState(t *testing.T, env *recTestEnv, id string) store.BacklogState {
	t.Helper()
	item, err := env.store.Backlog.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return item.State
}

// TestAutoRequeue_EligibilityByClass pins the class → eligibility table: infra /
// transient / transient_quota requeue; code / config / unclassified never do.
func TestAutoRequeue_EligibilityByClass(t *testing.T) {
	cases := []struct {
		class        string
		wantRequeued bool
	}{
		{"infra", true},
		{"transient", true},
		{"transient_quota", true},
		{"code", false},
		{"config", false},
		{"", false}, // unclassified fails closed to a human
	}
	for _, tc := range cases {
		t.Run("class="+tc.class, func(t *testing.T) {
			env := newRecEnv(t, nil)
			ctx := context.Background()
			id := "MILLS-CLS-" + tc.class
			if id == "MILLS-CLS-" {
				id = "MILLS-CLS-none"
			}
			seedEscalatedForRequeue(t, env, seedRequeueSpec{id: id, class: tc.class, endedAgo: 30 * time.Minute})

			res, err := env.rec.SweepAutoRequeue(ctx)
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			gotState := backlogState(t, env, id)
			if tc.wantRequeued {
				if res.Requeued != 1 || gotState != store.BacklogQueued {
					t.Fatalf("class %q: want requeued/queued, got res=%+v state=%s", tc.class, res, gotState)
				}
				if n := autoRequeueEventCount(t, env, id); n != 1 {
					t.Fatalf("class %q: want 1 event, got %d", tc.class, n)
				}
			} else {
				if res.Requeued != 0 || gotState != store.BacklogEscalated {
					t.Fatalf("class %q: want skipped/escalated, got res=%+v state=%s", tc.class, res, gotState)
				}
			}
		})
	}
}

// TestAutoRequeue_CooldownGate proves an eligible-class item is held until the
// cooldown elapses, then requeued.
func TestAutoRequeue_CooldownGate(t *testing.T) {
	cases := []struct {
		name         string
		endedAgo     time.Duration
		wantRequeued bool
	}{
		{"inside-cooldown", 5 * time.Minute, false}, // default cooldown 10m
		{"past-cooldown", 20 * time.Minute, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newRecEnv(t, nil)
			seedEscalatedForRequeue(t, env, seedRequeueSpec{id: "MILLS-CD", class: "infra", endedAgo: tc.endedAgo})
			res, err := env.rec.SweepAutoRequeue(context.Background())
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if got := res.Requeued == 1; got != tc.wantRequeued {
				t.Fatalf("%s: requeued=%v want %v (res=%+v)", tc.name, got, tc.wantRequeued, res)
			}
		})
	}
}

// TestAutoRequeue_PerItemCapPersists proves the per-item lifetime cap is read
// from durable events, so it holds across an operator "restart" (a fresh
// Reconciler on the same store).
func TestAutoRequeue_PerItemCapPersists(t *testing.T) {
	env := newRecEnv(t, nil) // default per_item_max 2
	ctx := context.Background()
	// Two prior auto-requeues already recorded ⇒ cap reached.
	seedEscalatedForRequeue(t, env, seedRequeueSpec{id: "MILLS-CAP", class: "infra", endedAgo: 30 * time.Minute, priorRequeue: 2})

	res, err := env.rec.SweepAutoRequeue(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Requeued != 0 || backlogState(t, env, "MILLS-CAP") != store.BacklogEscalated {
		t.Fatalf("cap reached: want skip, got res=%+v state=%s", res, backlogState(t, env, "MILLS-CAP"))
	}

	// "Restart": a fresh reconciler over the same store must still honor the cap.
	fresh := NewReconciler(env.store, env.policy, NewBudget(env.policy, NewStoreBudgetReader(env.store)), &recordingStarter{})
	fresh.Clock = env.rec.Clock
	res2, err := fresh.SweepAutoRequeue(ctx)
	if err != nil {
		t.Fatalf("sweep2: %v", err)
	}
	if res2.Requeued != 0 {
		t.Fatalf("cap must persist across restart, got res=%+v", res2)
	}

	// One prior requeue (below cap) ⇒ still eligible.
	env2 := newRecEnv(t, nil)
	seedEscalatedForRequeue(t, env2, seedRequeueSpec{id: "MILLS-CAP1", class: "infra", endedAgo: 30 * time.Minute, priorRequeue: 1})
	res3, err := env2.rec.SweepAutoRequeue(ctx)
	if err != nil {
		t.Fatalf("sweep3: %v", err)
	}
	if res3.Requeued != 1 {
		t.Fatalf("below cap: want requeue, got res=%+v", res3)
	}
	if n := autoRequeueEventCount(t, env2, "MILLS-CAP1"); n != 2 {
		t.Fatalf("want 2 total events after requeue, got %d", n)
	}
}

// TestAutoRequeue_PerDayCapRolling proves the fleet-wide rolling-24h cap blocks
// the sweep once reached, and that events older than 24h don't count.
func TestAutoRequeue_PerDayCapRolling(t *testing.T) {
	t.Run("cap-reached", func(t *testing.T) {
		env := newRecEnv(t, nil) // default per_day_max 6
		ctx := context.Background()
		// 6 fleet-wide requeues already in the window (on other items).
		for i := 0; i < 6; i++ {
			if err := env.store.Events.Append(ctx, &store.Event{
				OccurredAt: env.now.Add(-time.Hour), Actor: "reconciler",
				Kind: eventKindAutoRequeued, SubjectKind: autoRequeueSubjectKind,
				SubjectID: "OTHER-" + itoa(i), Payload: map[string]any{},
			}); err != nil {
				t.Fatalf("seed day event: %v", err)
			}
		}
		seedEscalatedForRequeue(t, env, seedRequeueSpec{id: "MILLS-DAY", class: "infra", endedAgo: 30 * time.Minute})
		res, err := env.rec.SweepAutoRequeue(ctx)
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if res.Requeued != 0 || backlogState(t, env, "MILLS-DAY") != store.BacklogEscalated {
			t.Fatalf("day cap reached: want skip, got res=%+v", res)
		}
	})

	t.Run("stale-events-excluded", func(t *testing.T) {
		env := newRecEnv(t, nil)
		ctx := context.Background()
		// 6 requeues but all >24h ago ⇒ outside the rolling window ⇒ not counted.
		for i := 0; i < 6; i++ {
			if err := env.store.Events.Append(ctx, &store.Event{
				OccurredAt: env.now.Add(-25 * time.Hour), Actor: "reconciler",
				Kind: eventKindAutoRequeued, SubjectKind: autoRequeueSubjectKind,
				SubjectID: "OLD-" + itoa(i), Payload: map[string]any{},
			}); err != nil {
				t.Fatalf("seed stale event: %v", err)
			}
		}
		seedEscalatedForRequeue(t, env, seedRequeueSpec{id: "MILLS-FRESH", class: "infra", endedAgo: 30 * time.Minute})
		res, err := env.rec.SweepAutoRequeue(ctx)
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if res.Requeued != 1 {
			t.Fatalf("stale events must not count: want requeue, got res=%+v", res)
		}
	})
}

// TestAutoRequeue_BudgetExhaustedSkip proves the sweep never requeues into an
// exhausted MaxRunsPerDay budget.
func TestAutoRequeue_BudgetExhaustedSkip(t *testing.T) {
	env := newAutoRequeueEnv(t, autoRequeuePolicyYAML(1 /*maxRunsPerDay*/, 10, 2, 6))
	ctx := context.Background()
	// Align the budget window with the injected clock, then seed one budgeted
	// run (a done run counts toward MaxRunsPerDay) so the day budget is full.
	env.rec.Budget.Now = func() time.Time { return env.now }
	if err := env.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "BUDGET-FILLER", Title: "filler", State: store.BacklogMerged, Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed filler item: %v", err)
	}
	done := env.now
	if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-BUDGET", BacklogID: "BUDGET-FILLER", Template: "mills-default-pipeline",
		State: store.PipelineDone, Attempts: 1, StartedAt: env.now, EndedAt: &done,
	}); err != nil {
		t.Fatalf("seed budgeted run: %v", err)
	}
	seedEscalatedForRequeue(t, env, seedRequeueSpec{id: "MILLS-BUD", class: "infra", endedAgo: 30 * time.Minute})

	res, err := env.rec.SweepAutoRequeue(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Requeued != 0 || backlogState(t, env, "MILLS-BUD") != store.BacklogEscalated {
		t.Fatalf("budget exhausted: want skip, got res=%+v state=%s", res, backlogState(t, env, "MILLS-BUD"))
	}
}

// TestAutoRequeue_ExternalIncident proves an item escalated for an external
// dependency is held while the incident is active and requeued once it clears.
func TestAutoRequeue_ExternalIncident(t *testing.T) {
	t.Run("active-incident-skips", func(t *testing.T) {
		env := newRecEnv(t, nil)
		ctx := context.Background()
		// 3 escalated runs for the same dependency ⇒ at/above the degraded-mode
		// threshold ⇒ an ACTIVE incident. All past cooldown so only the incident
		// gate can block them.
		for i := 0; i < 3; i++ {
			seedEscalatedForRequeue(t, env, seedRequeueSpec{
				id: "MILLS-EXT-" + itoa(i), class: "infra",
				endedAgo: 30 * time.Minute, extDep: "flexinfer",
			})
		}
		res, err := env.rec.SweepAutoRequeue(ctx)
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if res.Requeued != 0 {
			t.Fatalf("active incident: want 0 requeued, got res=%+v", res)
		}
	})

	t.Run("cleared-incident-requeues", func(t *testing.T) {
		env := newRecEnv(t, nil)
		ctx := context.Background()
		var decidedRun string
		env.rec.ExternalIncidentRetryDecision = func(_ context.Context, runID string) (bool, string, error) {
			decidedRun = runID
			return true, "", nil
		}
		// A single external-dependency escalation ⇒ below threshold ⇒ cleared.
		seedEscalatedForRequeue(t, env, seedRequeueSpec{
			id: "MILLS-EXT-OK", class: "infra",
			endedAgo: 30 * time.Minute, extDep: "ci-watch-solo",
		})
		res, err := env.rec.SweepAutoRequeue(ctx)
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if res.Requeued != 1 || backlogState(t, env, "MILLS-EXT-OK") != store.BacklogQueued {
			t.Fatalf("cleared incident: want requeue, got res=%+v", res)
		}
		if decidedRun == "" {
			t.Fatal("external incident retry policy was not consulted")
		}
	})

	t.Run("retry-cap-parks", func(t *testing.T) {
		env := newRecEnv(t, nil)
		ctx := context.Background()
		env.rec.ExternalIncidentRetryDecision = func(context.Context, string) (bool, string, error) {
			return false, "wait_for_dependency_recovery", nil
		}
		seedEscalatedForRequeue(t, env, seedRequeueSpec{
			id: "MILLS-EXT-CAPPED", class: "infra",
			endedAgo: 30 * time.Minute, extDep: "ci-watch-capped",
		})
		res, err := env.rec.SweepAutoRequeue(ctx)
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if res.Requeued != 0 || backlogState(t, env, "MILLS-EXT-CAPPED") != store.BacklogEscalated {
			t.Fatalf("capped incident: want parked escalation, got res=%+v", res)
		}
		n, err := env.store.Events.CountByKindSince(ctx, "reconciler.auto_requeue_parked", time.Time{})
		if err != nil {
			t.Fatalf("count parked events: %v", err)
		}
		if n != 1 {
			t.Fatalf("parked events = %d, want 1", n)
		}
		dwell, err := env.store.Pipeline.GetExternalIncidentDwell(ctx, "PIPE-MILLS-EXT-CAPPED")
		if err != nil {
			t.Fatalf("get dwell: %v", err)
		}
		if !dwell.StartedAt.Equal(env.now) || !dwell.DeadlineAt.Equal(env.now.Add(6*time.Hour)) {
			t.Fatalf("default dwell = %+v", dwell)
		}
	})

	t.Run("dwell-timeout-is-idempotent-and-run-scoped", func(t *testing.T) {
		env := newAutoRequeueEnv(t, autoRequeuePolicyYAML(100, 10, 2, 6))
		ctx := context.Background()
		env.rec.ExternalIncidentRetryDecision = func(context.Context, string) (bool, string, error) {
			return false, "wait_for_dependency_recovery", nil
		}
		seedEscalatedForRequeue(t, env, seedRequeueSpec{id: "MILLS-EXT-TIMEOUT", class: "infra", endedAgo: 30 * time.Minute, extDep: "gitlab"})
		before := dwellHistogramCount(t, store.ExternalIncidentDwellTimeout)
		if _, err := env.rec.SweepAutoRequeue(ctx); err != nil {
			t.Fatal(err)
		}
		env.now = env.now.Add(6 * time.Hour)
		env.rec.Clock = func() time.Time { return env.now }
		env.rec.autoRequeueRecheck = nil
		if _, err := env.rec.SweepAutoRequeue(ctx); err != nil {
			t.Fatal(err)
		}
		dwell, err := env.store.Pipeline.GetExternalIncidentDwell(ctx, "PIPE-MILLS-EXT-TIMEOUT")
		if err != nil {
			t.Fatal(err)
		}
		if dwell.CompletionReason != store.ExternalIncidentDwellTimeout || dwell.ElapsedDuration < 6*time.Hour-time.Second {
			t.Fatalf("timed out dwell = %+v", dwell)
		}
		if _, err := env.store.Events.FirstBySubjectKind(ctx, "pipeline_run", "PIPE-MILLS-EXT-TIMEOUT", "reconciler.external_incident_dwell_completed"); err != nil {
			t.Fatalf("run-scoped completion event: %v", err)
		}
		completed := *dwell.CompletedAt
		env.now = env.now.Add(time.Hour)
		env.rec.Clock = func() time.Time { return env.now }
		env.rec.autoRequeueRecheck = nil
		if _, err := env.rec.SweepAutoRequeue(ctx); err != nil {
			t.Fatal(err)
		}
		again, err := env.store.Pipeline.GetExternalIncidentDwell(ctx, "PIPE-MILLS-EXT-TIMEOUT")
		if err != nil {
			t.Fatal(err)
		}
		if again.CompletionReason != dwell.CompletionReason || again.CompletedAt == nil || !again.CompletedAt.Equal(completed) {
			t.Fatalf("timeout was rewritten: before=%+v after=%+v", dwell, again)
		}
		if delta := dwellHistogramCount(t, store.ExternalIncidentDwellTimeout) - before; delta != 1 {
			t.Fatalf("timeout dwell observations = %d, want 1", delta)
		}
	})
}

// TestAutoRequeue_OpenMRSkip proves an item whose latest run has an MR is never
// requeued — a merged MR is the ghost-spark sweep's job, an open/closed MR is a
// human branch-fix.
func TestAutoRequeue_OpenMRSkip(t *testing.T) {
	for _, state := range []string{"opened", "merged", "closed"} {
		t.Run("mr="+state, func(t *testing.T) {
			env := newRecEnv(t, nil)
			mrs := &fakeMRStateClient{states: map[int64]string{4242: state}}
			env.rec.GhostSparkMRState = mrs
			seedEscalatedForRequeue(t, env, seedRequeueSpec{
				id: "MILLS-MR", class: "infra", endedAgo: 30 * time.Minute, mrIID: 4242,
			})
			res, err := env.rec.SweepAutoRequeue(context.Background())
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if res.Requeued != 0 || backlogState(t, env, "MILLS-MR") != store.BacklogEscalated {
				t.Fatalf("mr=%s: want skip, got res=%+v", state, res)
			}
			if got := mrs.callCount(); got != 0 {
				t.Fatalf("mr=%s: auto-requeue made %d redundant MR lookups, want 0", state, got)
			}
		})
	}
}

// TestAutoRequeue_EventAppendedOnce proves one requeue writes exactly one event
// and bumps the metric once; a second sweep (the item now queued) is a no-op.
func TestAutoRequeue_EventAppendedOnce(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	seedEscalatedForRequeue(t, env, seedRequeueSpec{id: "MILLS-ONCE", class: "transient", endedAgo: 30 * time.Minute})

	before := testutil.ToFloat64(AutoRequeuesTotal.WithLabelValues("transient"))
	res, err := env.rec.SweepAutoRequeue(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Requeued != 1 {
		t.Fatalf("first sweep: want 1 requeued, got %+v", res)
	}
	if n := autoRequeueEventCount(t, env, "MILLS-ONCE"); n != 1 {
		t.Fatalf("want 1 event after first sweep, got %d", n)
	}
	if got := testutil.ToFloat64(AutoRequeuesTotal.WithLabelValues("transient")) - before; got != 1 {
		t.Fatalf("metric delta: want 1, got %v", got)
	}

	// Item is now queued; a second sweep must not touch it or write a 2nd event.
	res2, err := env.rec.SweepAutoRequeue(ctx)
	if err != nil {
		t.Fatalf("sweep2: %v", err)
	}
	if res2.Requeued != 0 {
		t.Fatalf("second sweep: want 0 requeued, got %+v", res2)
	}
	if n := autoRequeueEventCount(t, env, "MILLS-ONCE"); n != 1 {
		t.Fatalf("want still 1 event, got %d", n)
	}
}

// TestAutoRequeue_DuplicateCommitIsNotCounted proves the cap-counting event is
// tied to the writer that actually moves the backlog aggregate. TransitionState
// is intentionally idempotent for general lifecycle callers, so reusing it here
// must not let a second auto/manual requeue race report another successful
// unattended retry after the item is already queued.
func TestAutoRequeue_DuplicateCommitIsNotCounted(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	seedEscalatedForRequeue(t, env, seedRequeueSpec{
		id: "MILLS-DUPLICATE-COMMIT", class: "infra", endedAgo: 30 * time.Minute,
	})

	item, err := env.store.Backlog.Get(ctx, "MILLS-DUPLICATE-COMMIT")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	run, err := env.store.Pipeline.GetRun(ctx, "PIPE-MILLS-DUPLICATE-COMMIT")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	eval := autoRequeueEval{eligible: true, class: "infra"}

	committed, err := env.rec.commitAutoRequeue(ctx, item, run, eval, 2, 0, 6)
	if err != nil || !committed {
		t.Fatalf("first commit = (%v, %v), want (true, nil)", committed, err)
	}
	committed, err = env.rec.commitAutoRequeue(ctx, item, run, eval, 2, 1, 6)
	if err != nil {
		t.Fatalf("duplicate commit must be a clean stale skip: %v", err)
	}
	if committed {
		t.Fatal("duplicate commit reported a second unattended requeue")
	}
	if n := autoRequeueEventCount(t, env, item.ID); n != 1 {
		t.Fatalf("duplicate commit wrote %d cap events, want 1", n)
	}
}

// TestAutoRequeue_StaleClaimCleanSkip proves that when a concurrent writer has
// already moved an item off escalated, the guarded transition loses the race
// cleanly: no requeue, no event, no error storm.
func TestAutoRequeue_StaleClaimCleanSkip(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	seedEscalatedForRequeue(t, env, seedRequeueSpec{id: "MILLS-RACE", class: "infra", endedAgo: 30 * time.Minute})

	stale, err := env.store.Backlog.Get(ctx, "MILLS-RACE")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// Simulate the ghost-spark sweep winning the race: it reaps the item
	// escalated→merged out of band (its MR merged). The stale escalated→queued
	// commit must then lose cleanly against the claim-version + from-state fence.
	if _, err := env.store.Backlog.TransitionState(ctx, stale.ID, stale.ClaimVersion, store.BacklogEscalated, store.BacklogMerged); err != nil {
		t.Fatalf("concurrent transition: %v", err)
	}
	run := &store.PipelineRun{ID: "PIPE-MILLS-RACE", BacklogID: stale.ID}

	// commit against the now-stale in-memory escalated view.
	committed, cerr := env.rec.commitAutoRequeue(ctx, stale, run,
		autoRequeueEval{eligible: true, class: "infra"}, 2, 0, 6)
	if cerr != nil {
		t.Fatalf("stale claim must not error, got %v", cerr)
	}
	if committed {
		t.Fatalf("stale claim must not commit a requeue")
	}
	if n := autoRequeueEventCount(t, env, "MILLS-RACE"); n != 0 {
		t.Fatalf("stale claim must not append an event, got %d", n)
	}
}

// TestAutoRequeue_DisabledPolicyNoOp proves the sweep is inert when the policy
// flag is off, even with an otherwise-eligible item.
func TestAutoRequeue_DisabledPolicyNoOp(t *testing.T) {
	env := newAutoRequeueEnv(t, "version: 2\nenabled: true\n"+
		"budgets:\n  council:  { max_usd_per_run: 15, max_usd_per_day: 50 }\n"+
		"  pipeline: { max_usd_per_run: 5, max_usd_per_day: 75, max_runs_per_day: 20 }\n"+
		"pipeline:\n  default_template: mills-default-pipeline\n"+
		"  retry: { max_attempts: 3, cooldown_seconds: 300 }\n"+
		"  auto_requeue: { enabled: false }\n")
	seedEscalatedForRequeue(t, env, seedRequeueSpec{id: "MILLS-OFF", class: "infra", endedAgo: 30 * time.Minute})
	res, err := env.rec.SweepAutoRequeue(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Requeued != 0 || res.Inspected != 0 {
		t.Fatalf("disabled sweep must be inert, got %+v", res)
	}
	if backlogState(t, env, "MILLS-OFF") != store.BacklogEscalated {
		t.Fatalf("disabled sweep must not move the item")
	}
}

// TestAutoRequeue_CodeConfigOptIn covers the include_code_config opt-in: a
// retryable code/config escalation gets exactly one context-carrying
// unattended retry, and every guard fails closed.
func TestAutoRequeue_CodeConfigOptIn(t *testing.T) {
	yes := true
	no := false
	base := seedRequeueSpec{endedAgo: 30 * time.Minute, retryable: &yes, costUSD: 0.42}

	cases := []struct {
		name         string
		mutate       func(*seedRequeueSpec)
		optIn        bool
		journalEnv   string
		wantRequeued bool
	}{
		{"code retryable requeues", func(sp *seedRequeueSpec) { sp.class = "code" }, true, "1", true},
		{"config retryable requeues", func(sp *seedRequeueSpec) { sp.class = "config" }, true, "1", true},
		{"policy off skips", func(sp *seedRequeueSpec) { sp.class = "code" }, false, "1", false},
		{"journal off skips", func(sp *seedRequeueSpec) { sp.class = "code" }, true, "", false},
		{"not retryable skips", func(sp *seedRequeueSpec) { sp.class = "code"; sp.retryable = &no }, true, "1", false},
		{"nil retryable skips", func(sp *seedRequeueSpec) { sp.class = "code"; sp.retryable = nil }, true, "1", false},
		{"zero-cost no-op skips", func(sp *seedRequeueSpec) { sp.class = "code"; sp.costUSD = 0 }, true, "1", false},
		{"one-shot: prior requeue caps", func(sp *seedRequeueSpec) { sp.class = "code"; sp.priorRequeue = 1 }, true, "1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(ItemJournalEnvName, tc.journalEnv)
			env := newRecEnv(t, func(p *Policy) {
				p.Pipeline.AutoRequeue.IncludeCodeConfig = tc.optIn
			})
			ctx := context.Background()
			sp := base
			sp.id = "MILLS-CC-" + strings.ReplaceAll(tc.name, " ", "-")
			tc.mutate(&sp)
			seedEscalatedForRequeue(t, env, sp)

			res, err := env.rec.SweepAutoRequeue(ctx)
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			gotState := backlogState(t, env, sp.id)
			if tc.wantRequeued {
				if res.Requeued != 1 || gotState != store.BacklogQueued {
					t.Fatalf("want requeued/queued, got res=%+v state=%s", res, gotState)
				}
			} else if res.Requeued != 0 || gotState != store.BacklogEscalated {
				t.Fatalf("want skipped/escalated, got res=%+v state=%s", res, gotState)
			}
		})
	}
}
