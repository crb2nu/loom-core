package mills

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// stubLearningSignals records the window it was handed and returns a canned
// answer. The real aggregation lives in pkg/mills/guard (which imports this
// package, so it cannot be reached from here); what the reconciler owns — the
// schedule, the window, and never wedging on failure — is what these tests
// pin down.
type stubLearningSignals struct {
	res   LearningSignalSweepResult
	err   error
	calls int
	since time.Time
	now   time.Time
}

func (s *stubLearningSignals) PublishLearningSignals(_ context.Context, since, now time.Time) (LearningSignalSweepResult, error) {
	s.calls++
	s.since, s.now = since, now
	return s.res, s.err
}

func armLearningSignals(env *recTestEnv, stub *stubLearningSignals) *stubLearningSignals {
	env.rec.LearningSignals = stub
	return stub
}

func tickEventKinds(t *testing.T, env *recTestEnv) map[string]*store.Event {
	t.Helper()
	events, err := env.store.Events.ListSince(context.Background(), env.now.Add(-time.Hour), 200)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	out := make(map[string]*store.Event, len(events))
	for _, e := range events {
		if e != nil {
			out[e.Kind] = e
		}
	}
	return out
}

// TestLearningSignalSweepUsesConfiguredWindow: the reconciler owns the clock,
// so the window the gauges describe ends at tick time and reaches back exactly
// one configured window.
func TestLearningSignalSweepUsesConfiguredWindow(t *testing.T) {
	env := newRecEnv(t, nil)
	stub := armLearningSignals(env, &stubLearningSignals{})
	env.rec.LearningSignalWindow = 48 * time.Hour

	if _, err := env.rec.SweepLearningSignals(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("publisher calls = %d, want 1", stub.calls)
	}
	if !stub.now.Equal(env.now) {
		t.Errorf("window end = %s, want %s", stub.now, env.now)
	}
	if want := env.now.Add(-48 * time.Hour); !stub.since.Equal(want) {
		t.Errorf("window start = %s, want %s", stub.since, want)
	}
	if got := (&Reconciler{}).learningSignalWindow(); got != defaultLearningSignalWindow {
		t.Errorf("default window = %s, want %s", got, defaultLearningSignalWindow)
	}
}

// TestLearningSignalSweepDisabledWithoutPublisher: gauge export is opt-in. With
// no publisher the sweep is a no-op and never reports due, so the tick does not
// pay for a schedule it cannot run.
func TestLearningSignalSweepDisabledWithoutPublisher(t *testing.T) {
	env := newRecEnv(t, nil)

	res, err := env.rec.SweepLearningSignals(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res != (LearningSignalSweepResult{}) {
		t.Errorf("result = %+v, want zero value", res)
	}
	if env.rec.learningSignalDue(env.now) {
		t.Error("sweep reported due with no publisher wired")
	}
}

// TestLearningSignalDueRespectsInterval: the sweep is rate-limited rather than
// re-aggregating a two-week window on every tick.
func TestLearningSignalDueRespectsInterval(t *testing.T) {
	env := newRecEnv(t, nil)
	armLearningSignals(env, &stubLearningSignals{})
	env.rec.LearningSignalInterval = 20 * time.Minute

	if !env.rec.learningSignalDue(env.now) {
		t.Fatal("first sweep must be due immediately")
	}
	env.rec.nextLearningSignals = env.now.Add(env.rec.learningSignalInterval())
	if env.rec.learningSignalDue(env.now.Add(19 * time.Minute)) {
		t.Error("sweep due before the interval elapsed")
	}
	if !env.rec.learningSignalDue(env.now.Add(20 * time.Minute)) {
		t.Error("sweep not due after the interval elapsed")
	}
	if got := (&Reconciler{}).learningSignalInterval(); got != DefaultLearningSignalInterval {
		t.Errorf("default interval = %s, want %s", got, DefaultLearningSignalInterval)
	}
}

// TestLearningSignalTickRollsUpAndBacksOff: one tick runs the sweep, folds its
// counts into the tick event, and stamps the next attempt so the following tick
// skips it.
func TestLearningSignalTickRollsUpAndBacksOff(t *testing.T) {
	env := newRecEnv(t, nil)
	stub := armLearningSignals(env, &stubLearningSignals{
		res: LearningSignalSweepResult{
			Gates: 3, JoinedVerdicts: 21, PromotionActions: 8, ConfigRuns: 14, Regressions: 2,
		},
	})
	ctx := context.Background()

	if _, err := env.rec.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if _, err := env.rec.Tick(ctx); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("publisher calls across two ticks = %d, want 1 (interval not elapsed)", stub.calls)
	}

	tick, ok := tickEventKinds(t, env)["reconciler.tick"]
	if !ok {
		t.Fatal("no reconciler.tick event")
	}
	for key, want := range map[string]float64{
		"learning_signal_gates":             3,
		"learning_signal_joined_verdicts":   21,
		"learning_signal_promotion_actions": 8,
		"learning_signal_config_runs":       14,
		"learning_signal_regressions":       2,
	} {
		got, ok := tick.Payload[key]
		if !ok {
			t.Errorf("tick payload missing %s", key)
			continue
		}
		// Payloads round-trip through JSON, so ints come back as float64.
		if n, isNum := got.(float64); !isNum || n != want {
			t.Errorf("tick payload %s = %v, want %v", key, got, want)
		}
	}
}

// TestLearningSignalSweepFailureDoesNotWedgeTick: an export that cannot build
// its reports is recorded and dropped. The tick still reports ok — a window too
// large to aggregate says nothing about whether the reconciler is healthy — and
// the schedule still backs off so a permanently failing export cannot pin every
// tick to a doomed pass.
func TestLearningSignalSweepFailureDoesNotWedgeTick(t *testing.T) {
	env := newRecEnv(t, nil)
	boom := errors.New("window holds at least 10000 events")
	stub := armLearningSignals(env, &stubLearningSignals{err: boom})
	before := testutil.ToFloat64(LearningSignalExportErrorsTotal.WithLabelValues("sweep"))
	ctx := context.Background()

	res, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick returned an error for a failed export: %v", err)
	}
	if res.Errored != 0 {
		t.Errorf("tick errored = %d, want 0 (export failure is out of TickResult)", res.Errored)
	}
	if stub.calls != 1 {
		t.Fatalf("publisher calls = %d, want 1", stub.calls)
	}

	kinds := tickEventKinds(t, env)
	failed, ok := kinds["reconciler.learning_signal_sweep_failed"]
	if !ok {
		t.Fatal("no reconciler.learning_signal_sweep_failed event")
	}
	if got := failed.Payload["outcome"]; got != "error" {
		t.Errorf("failure outcome = %v, want error", got)
	}
	if got, _ := failed.Payload["error"].(string); got != boom.Error() {
		t.Errorf("failure error = %q, want %q", got, boom.Error())
	}
	if _, ok := kinds["reconciler.tick"]; !ok {
		t.Error("tick event missing after a failed export")
	}
	if after := testutil.ToFloat64(LearningSignalExportErrorsTotal.WithLabelValues("sweep")); after != before+1 {
		t.Errorf("sweep export errors = %v, want %v", after, before+1)
	}
	if !env.rec.nextLearningSignals.After(env.now) {
		t.Error("next sweep not stamped after a failure: a doomed export would retry every tick")
	}
}

// TestLearningSignalSweepExhaustedBudgetRecordsTimeout: the sweep runs on its
// own budget, so an export that outran it is recorded as a timeout rather than
// as an error — the distinction is what tells an operator to narrow the window
// instead of hunting a bug.
func TestLearningSignalSweepExhaustedBudgetRecordsTimeout(t *testing.T) {
	env := newRecEnv(t, nil)
	armLearningSignals(env, &stubLearningSignals{err: context.DeadlineExceeded})

	if _, err := env.rec.Tick(context.Background()); err != nil {
		t.Fatalf("tick returned an error for an exhausted export budget: %v", err)
	}
	failed, ok := tickEventKinds(t, env)["reconciler.learning_signal_sweep_failed"]
	if !ok {
		t.Fatal("no reconciler.learning_signal_sweep_failed event")
	}
	if got := failed.Payload["outcome"]; got != "timeout" {
		t.Errorf("failure outcome = %v, want timeout", got)
	}
}

// TestLearningSignalSweepCancelledParentUnwinds: a cancelled tick stops before
// the publisher runs rather than paying for a window nobody will read.
func TestLearningSignalSweepCancelledParentUnwinds(t *testing.T) {
	env := newRecEnv(t, nil)
	stub := armLearningSignals(env, &stubLearningSignals{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := env.rec.SweepLearningSignals(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("sweep error = %v, want context.Canceled", err)
	}
	if stub.calls != 0 {
		t.Errorf("publisher calls = %d, want 0 on a cancelled sweep", stub.calls)
	}
}
