package mills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// cancelAfterFirstRequeueCommenter is wired as the AutoRequeueIssueCommenter
// and cancels the sweep's parent context the moment the first requeue commits,
// so the pass sees a dead context at its next loop head — the deterministic
// shape of the 2026-09-08 boot pass, whose 20s budget expired mid-batch.
type cancelAfterFirstRequeueCommenter struct{ cancel context.CancelFunc }

func (c *cancelAfterFirstRequeueCommenter) CommentAutoRequeued(context.Context, *store.BacklogItem, *store.PipelineRun, string) error {
	c.cancel()
	return nil
}

// manuallyExpiredDeadlineContext expires only when the test reaches the intended
// interruption point. Its distant deadline also keeps derived context timers
// from racing SQLite work on a starved runner.
type manuallyExpiredDeadlineContext struct {
	context.Context
	deadline time.Time
	done     chan struct{}
	once     sync.Once
}

func newManuallyExpiredDeadlineContext(t *testing.T) *manuallyExpiredDeadlineContext {
	t.Helper()
	ctx := &manuallyExpiredDeadlineContext{
		Context:  context.Background(),
		deadline: time.Now().Add(time.Hour),
		done:     make(chan struct{}),
	}
	t.Cleanup(ctx.expire)
	return ctx
}

func (c *manuallyExpiredDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *manuallyExpiredDeadlineContext) Done() <-chan struct{}       { return c.done }
func (c *manuallyExpiredDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}
func (c *manuallyExpiredDeadlineContext) expire() { c.once.Do(func() { close(c.done) }) }

// expireAfterFirstRequeueCommenter exhausts the pass after its first commit,
// then waits for cancellation to propagate to the sweep's derived context.
type expireAfterFirstRequeueCommenter struct{ expire func() }

func (c expireAfterFirstRequeueCommenter) CommentAutoRequeued(ctx context.Context, _ *store.BacklogItem, _ *store.PipelineRun, _ string) error {
	c.expire()
	<-ctx.Done()
	return ctx.Err()
}

func seedThreeEligibleItems(t *testing.T, env *recTestEnv) []string {
	t.Helper()
	ids := []string{"MILLS-STARVE-1", "MILLS-STARVE-2", "MILLS-STARVE-3"}
	for _, id := range ids {
		seedEscalatedForRequeue(t, env, seedRequeueSpec{id: id, class: autoRequeueClassInfra, endedAgo: time.Hour})
	}
	return ids
}

func eventsOfKind(t *testing.T, env *recTestEnv, kind string) []*store.Event {
	t.Helper()
	all, err := env.store.Events.ListSince(context.Background(), time.Unix(0, 0), 500)
	if err != nil {
		t.Fatal(err)
	}
	var out []*store.Event
	for _, ev := range all {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// TestAutoRequeue_DeadContextStopsSweepAndReportsUnreached pins item (3) of
// the boot-burst fix: once the sweep's context is dead the pass stops at the
// next loop head, reports the candidates it never judged as Unreached, returns
// the context error (so the escalation sweep outcome is no longer "ok"), does
// NOT record a phantom auto_requeue_failed row per unreached item, does not
// park them on the recheck cooldown, and still lands its own summary row
// through the detached ledger-write budget.
func TestAutoRequeue_DeadContextStopsSweepAndReportsUnreached(t *testing.T) {
	env := newAutoRequeueEnv(t, autoRequeuePolicyYAML(50, 10, 5, 50))
	ids := seedThreeEligibleItems(t, env)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env.rec.AutoRequeueIssueCommenter = &cancelAfterFirstRequeueCommenter{cancel: cancel}

	res, err := env.rec.SweepAutoRequeue(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("SweepAutoRequeue err = %v, want context.Canceled surfaced", err)
	}
	if res.Requeued != 1 || res.Inspected != 1 || res.Errored != 0 || res.Skipped != 0 {
		t.Fatalf("result = %+v, want exactly one judged+requeued and nothing errored", res)
	}
	if res.Unreached != 2 {
		t.Fatalf("Unreached = %d, want 2 (the candidates never judged)", res.Unreached)
	}
	if got := eventsOfKind(t, env, "reconciler.auto_requeue_failed"); len(got) != 0 {
		t.Fatalf("phantom auto_requeue_failed rows = %d, want 0", len(got))
	}
	// Exactly one item moved; the other two are untouched and re-enter the
	// next pass without a recheck cooldown.
	queued := 0
	for _, id := range ids {
		if backlogState(t, env, id) == store.BacklogQueued {
			queued++
		}
	}
	if queued != 1 {
		t.Fatalf("queued items = %d, want 1", queued)
	}
	for _, id := range ids {
		if _, parked := env.rec.autoRequeueRecheck[id]; parked {
			t.Fatalf("%s parked on the recheck cooldown by a starved pass", id)
		}
	}
	sweeps := eventsOfKind(t, env, "reconciler.auto_requeue_sweep")
	if len(sweeps) != 1 {
		t.Fatalf("auto_requeue_sweep rows = %d, want 1 (written under the detached ledger budget)", len(sweeps))
	}
	if got := sweeps[0].Payload["outcome"]; got != "canceled" {
		t.Fatalf("sweep outcome = %v, want canceled", got)
	}
	if got, ok := sweeps[0].Payload["unreached"].(float64); !ok || int(got) != 2 {
		t.Fatalf("sweep payload unreached = %v, want 2", sweeps[0].Payload["unreached"])
	}
}

// TestEscalationSweeper_StarvedAutoRequeueIsVisible drives pass
// deadline propagation: the pass expires after the first auto-requeue commit,
// and the escalation_sweep ledger row must say so — outcome timeout,
// auto_requeue_error set, auto_requeue_unreached > 0 — and the timeout counter
// must tick. Before this fix the same pass recorded outcome "ok".
func TestEscalationSweeper_StarvedAutoRequeueIsVisible(t *testing.T) {
	env := newAutoRequeueEnv(t, autoRequeuePolicyYAML(50, 10, 5, 50))
	seedThreeEligibleItems(t, env)
	ctx := newManuallyExpiredDeadlineContext(t)
	env.rec.AutoRequeueIssueCommenter = expireAfterFirstRequeueCommenter{expire: ctx.expire}
	env.rec.AutoRequeuePerCandidateAllowance = time.Hour
	timeoutsBefore := testutil.ToFloat64(EscalationSweepTimeoutsTotal)

	sweeper := NewEscalationSweeper(env.rec, env.policy)
	// Keep derived timers beyond the manually expired parent deadline.
	sweeper.Budget = 3 * time.Hour
	sweeper.runPass(ctx)

	rows := eventsOfKind(t, env, "reconciler.escalation_sweep")
	if len(rows) != 1 {
		t.Fatalf("escalation_sweep rows = %d, want 1", len(rows))
	}
	payload := rows[0].Payload
	if got := payload["outcome"]; got != "timeout" {
		t.Fatalf("escalation_sweep outcome = %v, want timeout; payload=%v", got, payload)
	}
	if _, ok := payload["auto_requeue_error"]; !ok {
		t.Fatalf("escalation_sweep payload lacks auto_requeue_error: %v", payload)
	}
	if got, ok := payload["auto_requeue_unreached"].(float64); !ok || got < 1 {
		t.Fatalf("auto_requeue_unreached = %v, want >= 1", payload["auto_requeue_unreached"])
	}
	if got, ok := payload["auto_requeued"].(float64); !ok || int(got) != 1 {
		t.Fatalf("auto_requeued = %v, want 1", payload["auto_requeued"])
	}
	if got := testutil.ToFloat64(EscalationSweepTimeoutsTotal); got != timeoutsBefore+1 {
		t.Fatalf("timeout counter = %v, want %v", got, timeoutsBefore+1)
	}
	if got := eventsOfKind(t, env, "reconciler.auto_requeue_failed"); len(got) != 0 {
		t.Fatalf("phantom auto_requeue_failed rows = %d, want 0", len(got))
	}
}

// TestAutoRequeue_LookupFailureRowCarriesStage pins the ledger contract for a
// genuine per-item failure: the auto_requeue_failed row says WHICH step failed
// (lookup vs transition) so an operator can tell a failed requeue attempt from
// a failed read, and a bounded lookup failure never disguises itself as
// starvation.
func TestAutoRequeue_LookupFailureRowCarriesStage(t *testing.T) {
	env := newAutoRequeueEnv(t, autoRequeuePolicyYAML(50, 10, 5, 50))
	seedEscalatedForRequeue(t, env, seedRequeueSpec{id: "MILLS-STAGE", class: autoRequeueClassInfra, endedAgo: time.Hour})
	ctx := context.Background()
	// Corrupt the run row's started_at so ListByBacklog's scan fails for this
	// item only — a real lookup error on a live context.
	if _, err := env.store.DB().ExecContext(ctx, `UPDATE pipeline_runs SET started_at = 'not-a-time' WHERE backlog_id = ?`, "MILLS-STAGE"); err != nil {
		t.Fatal(err)
	}
	res, err := env.rec.SweepAutoRequeue(ctx)
	if err != nil {
		t.Fatalf("SweepAutoRequeue: %v", err)
	}
	if res.Errored != 1 || res.Inspected != 1 || res.Unreached != 0 {
		t.Fatalf("result = %+v, want one inspected+errored, none unreached", res)
	}
	failed := eventsOfKind(t, env, "reconciler.auto_requeue_failed")
	if len(failed) != 1 {
		t.Fatalf("auto_requeue_failed rows = %d, want 1", len(failed))
	}
	if got := failed[0].Payload["stage"]; got != "lookup" {
		t.Fatalf("stage = %v, want lookup", got)
	}
}

// The fourth eligibility lookup consumes the rest of the tick. The first
// three decisions finish before explicit expiry, regardless of runner speed.
func TestAutoRequeue_CursorThreeThreeTwoAndWrap(t *testing.T) {
	env := newAutoRequeueEnv(t, autoRequeuePolicyYAML(100, 10, 5, 100))
	for i := 0; i < 8; i++ {
		seedEscalatedForRequeue(t, env, seedRequeueSpec{id: fmt.Sprintf("cursor-%02d", i), class: autoRequeueClassConfig, extDep: fmt.Sprintf("dep-%d", i), endedAgo: time.Hour})
	}
	var judged []string
	for tick, want := range []int{3, 3, 2} {
		// New process state each tick: no process-local cooldown can supply fairness.
		rec := NewReconciler(env.store, env.policy, nil, env.starter)
		rec.Clock = func() time.Time { return env.now }
		ctx := newManuallyExpiredDeadlineContext(t)
		rec.AutoRequeuePerCandidateAllowance = time.Hour
		var logs bytes.Buffer
		rec.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
		calls := 0
		rec.ExternalIncidentRetryDecision = func(lookupCtx context.Context, id string) (bool, string, error) {
			calls++
			if calls == 4 {
				ctx.expire()
				<-lookupCtx.Done()
				return false, "", lookupCtx.Err()
			}
			judged = append(judged, id)
			return false, "held", nil
		}
		outcome := "ok"
		if tick < 2 {
			outcome = "timeout"
		}
		before := testutil.ToFloat64(AutoRequeueSweepsTotal.WithLabelValues(outcome))
		res, err := rec.SweepAutoRequeue(ctx)
		ctx.expire()
		if got := testutil.ToFloat64(AutoRequeueSweepsTotal.WithLabelValues(outcome)); got != before+1 {
			t.Fatalf("%s counter=%v want %v", outcome, got, before+1)
		}
		if got := testutil.ToFloat64(AutoRequeueUnreached); got != float64(res.Unreached) {
			t.Fatalf("gauge=%v result=%+v", got, res)
		}
		if res.Inspected != want {
			t.Fatalf("tick %d: %+v, err=%v", tick, res, err)
		}
		if tick < 2 && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("tick %d: %v", tick, err)
		}
		if tick == 2 && err != nil {
			t.Fatal(err)
		}
		cursor, err := env.store.Backlog.AutoRequeueCursor(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if tick < 2 && (cursor == nil || cursor.ID != fmt.Sprintf("cursor-%02d", (tick+1)*3-1)) {
			t.Fatalf("tick %d cursor=%+v", tick, cursor)
		}
		if tick == 2 && cursor != nil {
			t.Fatalf("lap did not reset: %+v", cursor)
		}
		var row map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &row); err != nil {
			t.Fatalf("log: %s: %v", logs.String(), err)
		}
		for _, field := range []string{"elapsed", "deadline", "cursor_before", "cursor_after", "deadline_capped"} {
			if _, ok := row[field]; !ok {
				t.Errorf("missing %s: %v", field, row)
			}
		}
		if row["deadline_capped"] != true {
			t.Fatalf("parent cap missing: %v", row)
		}
	}
	for i, id := range judged {
		if id != fmt.Sprintf("PIPE-cursor-%02d", i) {
			t.Fatalf("judgments: %v", judged)
		}
	}
	if len(judged) != 8 {
		t.Fatalf("judgments: %v", judged)
	}
	rec := NewReconciler(env.store, env.policy, nil, env.starter)
	rec.Clock = func() time.Time { return env.now }
	res, err := rec.SweepAutoRequeue(context.Background())
	if err != nil || res.Inspected != 8 {
		t.Fatalf("wrap: %+v %v", res, err)
	}
}

func TestAutoRequeue_CursorSurvivesRequeueAndStoreReopen(t *testing.T) {
	env := newAutoRequeueEnv(t, autoRequeuePolicyYAML(100, 10, 5, 100))
	// Use a known file path to exercise a real close/reopen, not just a new DAO.
	path := filepath.Join(t.TempDir(), "cursor.db")
	st, err := store.Open(context.Background(), store.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	env.store = st
	env.rec.Store = st
	env.rec.Budget = nil
	seedThreeEligibleItems(t, env)
	ctx, cancel := context.WithCancel(context.Background())
	env.rec.AutoRequeueIssueCommenter = &cancelAfterFirstRequeueCommenter{cancel: cancel}
	res, err := env.rec.SweepAutoRequeue(ctx)
	cancel()
	if !errors.Is(err, context.Canceled) || res.Requeued != 1 {
		t.Fatalf("first: %+v %v", res, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(context.Background(), store.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cursor, err := st.Backlog.AutoRequeueCursor(context.Background())
	if err != nil || cursor == nil || cursor.ID != "MILLS-STARVE-1" {
		t.Fatalf("cursor=%+v %v", cursor, err)
	}
	rec := NewReconciler(st, env.policy, nil, env.starter)
	rec.Clock = func() time.Time { return env.now }
	res, err = rec.SweepAutoRequeue(context.Background())
	if err != nil || res.Requeued != 2 {
		t.Fatalf("resume: %+v %v", res, err)
	}
}

func TestAutoRequeue_CursorBeyondBatchAndDeletedRow(t *testing.T) {
	env := newAutoRequeueEnv(t, autoRequeuePolicyYAML(100, 10, 5, 100))
	for i := 0; i < autoRequeueCandidateBatchSize+2; i++ {
		seedEscalatedForRequeue(t, env, seedRequeueSpec{id: fmt.Sprintf("batch-%03d", i), class: autoRequeueClassCode, endedAgo: time.Hour})
	}
	// Tied ordering fields leave ID as the required final key. The literal
	// must use the store's fixed-width layout: the saved cursor is compared
	// byte-for-byte against created_at (`created_at = ? AND id > ?`).
	if _, err := env.store.DB().ExecContext(context.Background(), `UPDATE backlog_items SET created_at = '2026-01-01T00:00:00.000000000Z'`); err != nil {
		t.Fatal(err)
	}
	res, err := env.rec.SweepAutoRequeue(context.Background())
	if err != nil || res.Inspected != autoRequeueCandidateBatchSize || res.Unreached != 2 {
		t.Fatalf("first: %+v %v", res, err)
	}
	cursor, err := env.store.Backlog.AutoRequeueCursor(context.Background())
	if err != nil || cursor == nil {
		t.Fatalf("cursor=%+v %v", cursor, err)
	}
	// Delete both rows explicitly to satisfy the pipeline FK.
	if _, err := env.store.DB().ExecContext(context.Background(), `DELETE FROM pipeline_runs WHERE backlog_id = ?`, cursor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.DB().ExecContext(context.Background(), `DELETE FROM backlog_items WHERE id = ?`, cursor.ID); err != nil {
		t.Fatal(err)
	}
	rec := NewReconciler(env.store, env.policy, nil, env.starter)
	rec.Clock = func() time.Time { return env.now }
	res, err = rec.SweepAutoRequeue(context.Background())
	if err != nil || res.Inspected != 2 || res.Unreached != 0 {
		t.Fatalf("resume: %+v %v", res, err)
	}
}

func TestAutoRequeue_CursorBudgetMetricAndDeadlineSizing(t *testing.T) {
	env := newAutoRequeueEnv(t, autoRequeuePolicyYAML(100, 10, 5, 1))
	seedEscalatedForRequeue(t, env, seedRequeueSpec{id: "budget", class: autoRequeueClassInfra, endedAgo: time.Hour, priorRequeue: 1})
	before := testutil.ToFloat64(AutoRequeueSweepsTotal.WithLabelValues("budget_blocked"))
	_, err := env.rec.SweepAutoRequeue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(AutoRequeueSweepsTotal.WithLabelValues("budget_blocked")); got != before+1 {
		t.Fatalf("counter=%v", got)
	}
	rows := eventsOfKind(t, env, "reconciler.auto_requeue_sweep")
	if len(rows) != 1 || rows[0].Payload["requested_budget"] != "4s" || rows[0].Payload["deadline_capped"] != false {
		t.Fatalf("summary=%+v", rows)
	}
	AutoRequeueUnreached.Set(99)
	if _, err := env.store.DB().ExecContext(context.Background(), `UPDATE backlog_items SET state = 'queued'`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.rec.SweepAutoRequeue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(AutoRequeueUnreached); got != 0 {
		t.Fatalf("empty gauge=%v", got)
	}
}
