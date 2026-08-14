package mills

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// fakeSpawnBreakerEvents is the deterministic escalation-trail seam. It also
// records the query it was asked for so the window math is asserted, not
// assumed.
type fakeSpawnBreakerEvents struct {
	events   []*store.Event
	err      error
	gotActor string
	gotSince time.Time
	gotLimit int
	calls    int
}

func (f *fakeSpawnBreakerEvents) ListByActorSince(_ context.Context, actor string, since time.Time, limit int) ([]*store.Event, error) {
	f.calls++
	f.gotActor, f.gotSince, f.gotLimit = actor, since, limit
	if f.err != nil {
		return nil, f.err
	}
	return f.events, nil
}

// spawnEscalation builds one durable escalation event in the exact shape the
// runner appends: actor "pipeline", kind "pipeline.run.escalated", and the full
// reason string with its [class=…] / [reason=…] markers.
func spawnEscalation(runID, reasonToken string, at time.Time) *store.Event {
	reason := "stage plan_slice errored after 3 attempts [class=infra] [reason=" + reasonToken +
		"]: spawn: agent CLI exited 1"
	return &store.Event{
		OccurredAt: at,
		Actor:      spawnBreakerEventActor,
		Kind:       spawnBreakerEventKind,
		Payload:    map[string]any{"run": runID, "reason": reason, "outcome": "error"},
	}
}

func spawnBreakerRec(events *fakeSpawnBreakerEvents) *Reconciler {
	return &Reconciler{spawnBreakerEvents: events}
}

func TestSpawnBreaker_TripsAtThreshold(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	events := &fakeSpawnBreakerEvents{events: []*store.Event{
		spawnEscalation("run-c", "spawn-stdin-misconfig", now.Add(-2*time.Minute)),
		spawnEscalation("run-b", "spawn-stdin-misconfig", now.Add(-14*time.Minute)),
		spawnEscalation("run-a", "spawn-stdin-misconfig", now.Add(-28*time.Minute)),
	}}
	status := spawnBreakerRec(events).evaluateSpawnBreaker(context.Background(), &Policy{}, now)

	if !status.Open {
		t.Fatalf("breaker should be open at the threshold: %+v", status)
	}
	if status.Reason != "spawn-stdin-misconfig" || status.Failures != 3 {
		t.Fatalf("verdict: got reason=%q failures=%d", status.Reason, status.Failures)
	}
	want := "spawn transport breaker open: 3x spawn-stdin-misconfig in 26m — holding dispatch until 2026-07-25T12:13:00Z"
	if status.Blocker != want {
		t.Fatalf("blocker:\n got %q\nwant %q", status.Blocker, want)
	}
	if events.gotActor != "pipeline" || events.gotLimit != spawnBreakerScanLimit {
		t.Fatalf("query: actor=%q limit=%d", events.gotActor, events.gotLimit)
	}
	if wantSince := now.Add(-30 * time.Minute); !events.gotSince.Equal(wantSince) {
		t.Fatalf("window start: got %s want %s", events.gotSince, wantSince)
	}
}

func TestSpawnBreaker_MixedReasonsDoNotTrip(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	// Three spawn-infra failures, but no single reason reaches the threshold:
	// this is ordinary background flakiness, not one vendor being down.
	events := &fakeSpawnBreakerEvents{events: []*store.Event{
		spawnEscalation("run-a", "spawn-stdin-misconfig", now.Add(-3*time.Minute)),
		spawnEscalation("run-b", "spawn-agent-timeout", now.Add(-9*time.Minute)),
		spawnEscalation("run-c", "spawn-auth-missing", now.Add(-15*time.Minute)),
		spawnEscalation("run-d", "spawn-stdin-misconfig", now.Add(-20*time.Minute)),
	}}
	if status := spawnBreakerRec(events).evaluateSpawnBreaker(context.Background(), &Policy{}, now); status.Open {
		t.Fatalf("mixed reasons must not trip the breaker: %+v", status)
	}
}

func TestSpawnBreaker_NonSpawnEscalationsIgnored(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	plain := func(runID string, at time.Time) *store.Event {
		return &store.Event{
			OccurredAt: at, Actor: spawnBreakerEventActor, Kind: spawnBreakerEventKind,
			Payload: map[string]any{
				"run":    runID,
				"reason": "stage implement errored after 3 attempts [class=code]: tests failed",
			},
		}
	}
	events := &fakeSpawnBreakerEvents{events: []*store.Event{
		plain("run-a", now.Add(-1*time.Minute)),
		plain("run-b", now.Add(-2*time.Minute)),
		plain("run-c", now.Add(-3*time.Minute)),
		// Same actor, different kind — must not be counted either.
		{OccurredAt: now, Actor: spawnBreakerEventActor, Kind: "pipeline.stage.error",
			Payload: map[string]any{"run": "run-d", "reason": "[reason=spawn-stdin-misconfig]"}},
	}}
	if status := spawnBreakerRec(events).evaluateSpawnBreaker(context.Background(), &Policy{}, now); status.Open {
		t.Fatalf("code escalations must not trip a SPAWN breaker: %+v", status)
	}
}

func TestSpawnBreaker_OneRunCountsOnce(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	events := &fakeSpawnBreakerEvents{events: []*store.Event{
		spawnEscalation("run-a", "spawn-driver-lost", now.Add(-1*time.Minute)),
		spawnEscalation("run-a", "spawn-driver-lost", now.Add(-2*time.Minute)),
		spawnEscalation("run-a", "spawn-driver-lost", now.Add(-3*time.Minute)),
	}}
	if status := spawnBreakerRec(events).evaluateSpawnBreaker(context.Background(), &Policy{}, now); status.Open {
		t.Fatalf("the threshold counts distinct RUNS, not rows: %+v", status)
	}
}

func TestSpawnBreaker_CooldownExpiryResumesDispatch(t *testing.T) {
	base := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	events := &fakeSpawnBreakerEvents{events: []*store.Event{
		spawnEscalation("run-a", "spawn-agent-timeout", base.Add(-6*time.Minute)),
		spawnEscalation("run-b", "spawn-agent-timeout", base.Add(-4*time.Minute)),
		spawnEscalation("run-c", "spawn-agent-timeout", base.Add(-2*time.Minute)),
	}}
	rec := spawnBreakerRec(events)
	last := base.Add(-2 * time.Minute)

	if status := rec.evaluateSpawnBreaker(context.Background(), &Policy{}, base); !status.Open {
		t.Fatalf("breaker should be open during the cooldown: %+v", status)
	}
	// One second before the cooldown elapses it is still held...
	held := last.Add(15 * time.Minute).Add(-time.Second)
	if status := rec.evaluateSpawnBreaker(context.Background(), &Policy{}, held); !status.Open {
		t.Fatalf("breaker should still hold at %s: %+v", held, status)
	}
	// ...and at the cooldown boundary it half-opens: dispatch resumes.
	halfOpen := last.Add(15 * time.Minute)
	if status := rec.evaluateSpawnBreaker(context.Background(), &Policy{}, halfOpen); status.Open {
		t.Fatalf("breaker should half-open at the cooldown boundary: %+v", status)
	}
}

func TestSpawnBreaker_RetripsOnRecurrenceAfterHalfOpen(t *testing.T) {
	base := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	events := &fakeSpawnBreakerEvents{events: []*store.Event{
		spawnEscalation("run-a", "spawn-auth-missing", base),
		spawnEscalation("run-b", "spawn-auth-missing", base.Add(1*time.Minute)),
		spawnEscalation("run-c", "spawn-auth-missing", base.Add(2*time.Minute)),
	}}
	rec := spawnBreakerRec(events)
	halfOpen := base.Add(17 * time.Minute) // last failure + 15m cooldown
	if status := rec.evaluateSpawnBreaker(context.Background(), &Policy{}, halfOpen); status.Open {
		t.Fatalf("precondition: breaker should be half-open: %+v", status)
	}

	// The probe dispatch dies the same way: one fresh failure re-opens the
	// breaker, because the earlier failures are still inside the window.
	events.events = append(events.events, spawnEscalation("run-d", "spawn-auth-missing", halfOpen))
	status := rec.evaluateSpawnBreaker(context.Background(), &Policy{}, halfOpen.Add(time.Second))
	if !status.Open {
		t.Fatalf("a same-reason recurrence must re-open the breaker: %+v", status)
	}
	if status.Failures != 4 {
		t.Fatalf("failures: got %d want 4", status.Failures)
	}
	if wantHold := halfOpen.Add(15 * time.Minute); !status.HoldUntil.Equal(wantHold) {
		t.Fatalf("hold_until: got %s want %s", status.HoldUntil, wantHold)
	}
}

func TestSpawnBreaker_AgedOutFailuresDoNotTrip(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	// The reader is asked for the window only; anything older is not returned in
	// production. Prove the breaker itself also never trips on a stale tail by
	// handing it events the store would have filtered out.
	events := &fakeSpawnBreakerEvents{events: []*store.Event{
		spawnEscalation("run-a", "spawn-stdin-misconfig", now.Add(-90*time.Minute)),
		spawnEscalation("run-b", "spawn-stdin-misconfig", now.Add(-80*time.Minute)),
		spawnEscalation("run-c", "spawn-stdin-misconfig", now.Add(-70*time.Minute)),
	}}
	if status := spawnBreakerRec(events).evaluateSpawnBreaker(context.Background(), &Policy{}, now); status.Open {
		t.Fatalf("failures older than the cooldown must not hold dispatch: %+v", status)
	}
}

func TestSpawnBreaker_ReadErrorKeepsBreakerClosed(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	events := &fakeSpawnBreakerEvents{err: errors.New("database is locked")}
	status := spawnBreakerRec(events).evaluateSpawnBreaker(context.Background(), &Policy{}, now)
	if status.Open || status.Blocker != "" {
		t.Fatalf("a read failure must never hold dispatch: %+v", status)
	}
}

func TestSpawnBreaker_DisabledByPolicy(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	events := &fakeSpawnBreakerEvents{events: []*store.Event{
		spawnEscalation("run-a", "spawn-stdin-misconfig", now.Add(-1*time.Minute)),
		spawnEscalation("run-b", "spawn-stdin-misconfig", now.Add(-2*time.Minute)),
		spawnEscalation("run-c", "spawn-stdin-misconfig", now.Add(-3*time.Minute)),
	}}
	off := false
	p := &Policy{}
	p.Pipeline.SpawnBreaker.Enabled = &off
	if status := spawnBreakerRec(events).evaluateSpawnBreaker(context.Background(), p, now); status.Open {
		t.Fatalf("disabled breaker must never hold dispatch: %+v", status)
	}
	if events.calls != 0 {
		t.Fatalf("disabled breaker must not query the store (calls=%d)", events.calls)
	}
}

func TestSpawnBreaker_PolicyKnobsOverrideDefaults(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	events := &fakeSpawnBreakerEvents{events: []*store.Event{
		spawnEscalation("run-a", "spawn-agent-timeout", now.Add(-1*time.Minute)),
		spawnEscalation("run-b", "spawn-agent-timeout", now.Add(-2*time.Minute)),
	}}
	p := &Policy{}
	p.Pipeline.SpawnBreaker.Threshold = 2
	p.Pipeline.SpawnBreaker.WindowMinutes = 10
	p.Pipeline.SpawnBreaker.CooldownMinutes = 45

	status := spawnBreakerRec(events).evaluateSpawnBreaker(context.Background(), p, now)
	if !status.Open || status.Failures != 2 {
		t.Fatalf("threshold=2 should trip on two runs: %+v", status)
	}
	if wantHold := now.Add(-time.Minute).Add(45 * time.Minute); !status.HoldUntil.Equal(wantHold) {
		t.Fatalf("hold_until: got %s want %s", status.HoldUntil, wantHold)
	}
	if wantSince := now.Add(-10 * time.Minute); !events.gotSince.Equal(wantSince) {
		t.Fatalf("window start: got %s want %s", events.gotSince, wantSince)
	}
}

func TestSpawnBreakerReasonToken(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{"stage plan_slice errored after 3 attempts [class=infra] [reason=spawn-stdin-misconfig]: boom", "spawn-stdin-misconfig"},
		{"stage plan_slice errored [reason=SPAWN-Agent-Timeout]: boom", "spawn-agent-timeout"},
		{"stage implement errored after 3 attempts [class=code]: tests failed", ""},
		{"stage ci_watch errored [reason=ci-pipeline-terminal]: boom", ""},
		{"truncated marker [reason=spawn-driver-lost", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := spawnBreakerReasonToken(tc.reason); got != tc.want {
			t.Errorf("spawnBreakerReasonToken(%q) = %q want %q", tc.reason, got, tc.want)
		}
	}
}

func TestFormatSpawnBreakerSpan(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{20 * time.Second, "<1m"},
		{90 * time.Second, "2m"},
		{28 * time.Minute, "28m"},
		{65 * time.Minute, "1h05m"},
	}
	for _, tc := range cases {
		if got := formatSpawnBreakerSpan(tc.in); got != tc.want {
			t.Errorf("formatSpawnBreakerSpan(%s) = %q want %q", tc.in, got, tc.want)
		}
	}
}

// ----- Policy surface -----

func TestSpawnBreakerPolicy_Defaults(t *testing.T) {
	var p PipelinePolicy
	if !p.SpawnBreakerEnabled() {
		t.Fatal("spawn breaker must default ON")
	}
	if got := p.SpawnBreaker.FailureThreshold(); got != 3 {
		t.Errorf("threshold default: got %d want 3", got)
	}
	if got := p.SpawnBreaker.WindowDuration(); got != 30*time.Minute {
		t.Errorf("window default: got %s want 30m", got)
	}
	if got := p.SpawnBreaker.CooldownDuration(); got != 15*time.Minute {
		t.Errorf("cooldown default: got %s want 15m", got)
	}
}

func TestSpawnBreakerPolicy_ValidationBounds(t *testing.T) {
	cases := []struct {
		name    string
		policy  SpawnBreakerPolicy
		wantErr bool
	}{
		{"defaults", SpawnBreakerPolicy{}, false},
		{"tuned", SpawnBreakerPolicy{Threshold: 5, WindowMinutes: 60, CooldownMinutes: 30}, false},
		{"negative threshold", SpawnBreakerPolicy{Threshold: -1}, true},
		{"threshold over max", SpawnBreakerPolicy{Threshold: spawnBreakerMaxThreshold + 1}, true},
		{"negative window", SpawnBreakerPolicy{WindowMinutes: -1}, true},
		{"window over max", SpawnBreakerPolicy{WindowMinutes: spawnBreakerMaxWindowMinutes + 1}, true},
		{"negative cooldown", SpawnBreakerPolicy{CooldownMinutes: -1}, true},
		{"cooldown over max", SpawnBreakerPolicy{CooldownMinutes: spawnBreakerMaxCooldownMinutes + 1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateSpawnBreaker(tc.policy); (err != nil) != tc.wantErr {
				t.Fatalf("validateSpawnBreaker(%+v) error = %v, wantErr %v", tc.policy, err, tc.wantErr)
			}
		})
	}
}

// ----- Tick integration -----

// seedSpawnEscalations writes the durable escalation trail of a vendor outage
// through the REAL store, so the breaker's production read path (actor +
// occurred_at window) is what the test exercises.
func seedSpawnEscalations(t *testing.T, env *recTestEnv, reason string, ages ...time.Duration) {
	t.Helper()
	for i, age := range ages {
		ev := spawnEscalation(fmt.Sprintf("RUN-%d", i), reason, env.now.Add(-age))
		if err := env.store.Events.Append(context.Background(), ev); err != nil {
			t.Fatalf("seed escalation: %v", err)
		}
	}
}

func TestReconciler_SpawnBreakerHoldsDispatch(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	seedSpawnEscalations(t, env, "spawn-stdin-misconfig", 2*time.Minute, 8*time.Minute, 20*time.Minute)

	item := &store.BacklogItem{
		ID: "MILLS-OUTAGE", Title: "queued during a vendor outage",
		State: store.BacklogQueued, Priority: store.P2, CreatedBy: "test",
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.SkipReason != "autonomy blocked" {
		t.Fatalf("skip reason: got %q want autonomy blocked", res.SkipReason)
	}
	if env.starter.calls() != 0 {
		t.Fatalf("starter calls: got %d want 0 (dispatch must be held)", env.starter.calls())
	}
	got, err := env.store.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("read backlog: %v", err)
	}
	if got.State != store.BacklogQueued {
		t.Fatalf("backlog state: got %q want %q (the breaker must not mutate items)", got.State, store.BacklogQueued)
	}

	// The blocker reaches the durable blocked-tick event the autonomy path
	// already publishes — the same payload the operator reads back as
	// autonomy_blockers.
	blocker := findTickBlocker(t, env, "spawn transport breaker open")
	if !strings.Contains(blocker, "3x spawn-stdin-misconfig") ||
		!strings.Contains(blocker, "holding dispatch until") {
		t.Fatalf("blocker text is not operator-actionable: %q", blocker)
	}
}

func TestReconciler_SpawnBreakerDispatchesAfterCooldown(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	// Same three failures, but the newest is older than the 15m cooldown: the
	// breaker has half-opened and the queue drains again.
	seedSpawnEscalations(t, env, "spawn-stdin-misconfig", 16*time.Minute, 22*time.Minute, 28*time.Minute)

	item := &store.BacklogItem{
		ID: "MILLS-AFTER-COOLDOWN", Title: "queued after the outage cleared",
		State: store.BacklogQueued, Priority: store.P2, CreatedBy: "test",
	}
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.SkipReason != "" {
		t.Fatalf("skip reason: got %q want none", res.SkipReason)
	}
	if env.starter.calls() != 1 {
		t.Fatalf("starter calls: got %d want 1", env.starter.calls())
	}
}

func TestReconciler_SpawnBreakerStatusIsReadable(t *testing.T) {
	env := newRecEnv(t, nil)
	seedSpawnEscalations(t, env, "spawn-auth-missing", 1*time.Minute, 3*time.Minute, 5*time.Minute)

	status := env.rec.SpawnTransportBreakerStatus(context.Background())
	if !status.Open || status.Reason != "spawn-auth-missing" || status.Failures != 3 {
		t.Fatalf("status: %+v", status)
	}
	if !status.HoldUntil.Equal(env.now.Add(-time.Minute).Add(15 * time.Minute)) {
		t.Fatalf("hold_until: got %s", status.HoldUntil)
	}
}

// findTickBlocker returns the first blocked-tick blocker containing needle.
func findTickBlocker(t *testing.T, env *recTestEnv, needle string) string {
	t.Helper()
	events, err := env.store.Events.ListByActorSince(context.Background(), "reconciler", env.now.Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, ev := range events {
		if ev.Kind != "reconciler.tick" {
			continue
		}
		raw, ok := ev.Payload["blockers"].([]any)
		if !ok {
			continue
		}
		for _, b := range raw {
			if s, _ := b.(string); strings.Contains(s, needle) {
				return s
			}
		}
	}
	t.Fatalf("no reconciler.tick blocker containing %q", needle)
	return ""
}
