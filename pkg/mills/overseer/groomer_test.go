package overseer

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// fakeChat scripts triage verdicts. Each Verdict call pops the next reply;
// OnCall (when set) runs before returning, letting race tests mutate the
// store mid-tick.
type fakeChat struct {
	replies []string
	err     error
	calls   int
	OnCall  func(call int)
}

func (f *fakeChat) ChatStructured(_ context.Context, _, _ string, _ int) (string, float64, error) {
	call := f.calls
	f.calls++
	if f.OnCall != nil {
		f.OnCall(call)
	}
	if f.err != nil {
		return "", 0, f.err
	}
	if call < len(f.replies) {
		return f.replies[call], 0.001, nil
	}
	return f.replies[len(f.replies)-1], 0.001, nil
}

func (f *fakeChat) JudgeModel() string { return "fake-judge" }

func verdictJSON(t *testing.T, verdict string, confidence float64) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"verdict": verdict, "confidence": confidence, "reason": "test"})
	if err != nil {
		t.Fatalf("marshal verdict: %v", err)
	}
	return string(b)
}

type groomerEnv struct {
	store   *store.Store
	groomer *Groomer
	policy  *mills.Policy
	now     time.Time
}

func newGroomerEnv(t *testing.T, gp mills.GroomerPolicy, chat ChatClient) *groomerEnv {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "g.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	pol := mills.Default()
	pol.Overseers = mills.OverseersPolicy{Enabled: true, Groomer: gp}
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	env := &groomerEnv{store: st, policy: pol, now: now}
	var triage *Triage
	if chat != nil {
		triage = &Triage{Client: chat}
	}
	env.groomer = &Groomer{
		Store:  st,
		Policy: func() *mills.Policy { return env.policy },
		Triage: triage,
		Recorder: &ActionRecorder{
			Events: st.Events,
			Actor:  groomerActor,
			DryRun: func() bool { return mills.DryRunOn(env.policy.Overseers.Groomer.DryRun) },
		},
		Now: func() time.Time { return env.now },
	}
	return env
}

func (e *groomerEnv) seedQueued(t *testing.T, id, title string, priority store.Priority, createdAgo time.Duration) *store.BacklogItem {
	t.Helper()
	item := &store.BacklogItem{
		ID: id, Title: title, State: store.BacklogQueued,
		Priority: priority, CreatedBy: "test",
		CreatedAt: e.now.Add(-createdAgo),
	}
	if err := e.store.Backlog.Put(context.Background(), item); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	return item
}

func (e *groomerEnv) itemState(t *testing.T, id string) store.BacklogState {
	t.Helper()
	item, err := e.store.Backlog.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return item.State
}

func (e *groomerEnv) eventCount(t *testing.T, kind string) int {
	t.Helper()
	n, err := e.store.Events.CountByKindSince(context.Background(), kind, e.now.Add(-48*time.Hour))
	if err != nil {
		t.Fatalf("count %s: %v", kind, err)
	}
	return n
}

func boolPtr(v bool) *bool { return &v }

// Two near-identical titles above the hard threshold: the younger retires,
// the older survives, and the audit event names the canonical item.
func TestGroomerHardDuplicateRetireCommitted(t *testing.T) {
	env := newGroomerEnv(t, mills.GroomerPolicy{
		Enabled: true, DryRun: boolPtr(false),
		Allow: mills.GroomerAllowPolicy{DedupClose: true},
	}, nil)
	env.seedQueued(t, "OLD", "Add HUD overseer status panel", store.P2, 48*time.Hour)
	env.seedQueued(t, "YOUNG", "Add the HUD overseer status panel", store.P2, 1*time.Hour)

	res, err := env.groomer.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Acted != 1 {
		t.Fatalf("acted = %d, want 1 (res=%+v)", res.Acted, res)
	}
	if got := env.itemState(t, "YOUNG"); got != store.BacklogRetired {
		t.Fatalf("young state = %s, want retired", got)
	}
	if got := env.itemState(t, "OLD"); got != store.BacklogQueued {
		t.Fatalf("old state = %s, want queued", got)
	}
	events, err := env.store.Events.ListBySubject(context.Background(), groomerSubjectKind, "YOUNG", 10)
	if err != nil || len(events) == 0 {
		t.Fatalf("expected retire event, got %v err=%v", events, err)
	}
	if events[0].Kind != "overseer.groomer.dedup_close" {
		t.Fatalf("event kind = %s", events[0].Kind)
	}
	if events[0].Payload["canonical_id"] != "OLD" {
		t.Fatalf("canonical_id = %v", events[0].Payload["canonical_id"])
	}
}

// Dry-run is the nil default: nothing mutates, the would-be action lands
// once as a .dryrun event, and a second tick does not re-mint it.
func TestGroomerDryRunDefaultPlansOnly(t *testing.T) {
	env := newGroomerEnv(t, mills.GroomerPolicy{
		Enabled: true, // DryRun nil ⇒ dry-run ON
		Allow:   mills.GroomerAllowPolicy{DedupClose: true},
	}, nil)
	env.seedQueued(t, "OLD", "Fix flaky reconciler tick test", store.P2, 48*time.Hour)
	env.seedQueued(t, "YOUNG", "Fix the flaky reconciler tick test", store.P2, 1*time.Hour)

	res, err := env.groomer.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Acted != 0 || res.Planned != 1 {
		t.Fatalf("acted=%d planned=%d, want 0/1", res.Acted, res.Planned)
	}
	if got := env.itemState(t, "YOUNG"); got != store.BacklogQueued {
		t.Fatalf("young mutated in dry-run: %s", got)
	}
	if n := env.eventCount(t, "overseer.groomer.dedup_close.dryrun"); n != 1 {
		t.Fatalf("dryrun events = %d, want 1", n)
	}
	// Second tick: AppendOnce keeps the soak trail at one event per item.
	res2, err := env.groomer.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if res2.Planned != 0 {
		t.Fatalf("second tick planned = %d, want 0", res2.Planned)
	}
	if n := env.eventCount(t, "overseer.groomer.dedup_close.dryrun"); n != 1 {
		t.Fatalf("dryrun events after tick2 = %d, want 1", n)
	}
}

// Gray-band pairs act only on a confident LLM "duplicate" verdict; a
// "distinct" verdict leaves both items alone.
func TestGroomerGrayBandLLMVerdict(t *testing.T) {
	base := mills.GroomerPolicy{
		Enabled: true, DryRun: boolPtr(false),
		Allow: mills.GroomerAllowPolicy{DedupClose: true},
	}
	// Titles chosen to land in the gray band [0.55, 0.85).
	const titleA = "Mills telemetry stage rollup endpoint"
	const titleB = "Mills telemetry stage rollup panel for HUD"

	t.Run("duplicate verdict retires", func(t *testing.T) {
		env := newGroomerEnv(t, base, &fakeChat{replies: []string{verdictJSON(t, "duplicate", 0.95)}})
		env.seedQueued(t, "OLD", titleA, store.P2, 48*time.Hour)
		env.seedQueued(t, "YOUNG", titleB, store.P2, time.Hour)
		res, err := env.groomer.Tick(context.Background())
		if err != nil {
			t.Fatalf("tick: %v", err)
		}
		if res.Acted != 1 {
			t.Fatalf("acted = %d, want 1 (res=%+v)", res.Acted, res)
		}
		if got := env.itemState(t, "YOUNG"); got != store.BacklogRetired {
			t.Fatalf("young state = %s, want retired", got)
		}
	})
	t.Run("distinct verdict is a no-op", func(t *testing.T) {
		env := newGroomerEnv(t, base, &fakeChat{replies: []string{verdictJSON(t, "distinct", 0.95)}})
		env.seedQueued(t, "OLD", titleA, store.P2, 48*time.Hour)
		env.seedQueued(t, "YOUNG", titleB, store.P2, time.Hour)
		res, err := env.groomer.Tick(context.Background())
		if err != nil {
			t.Fatalf("tick: %v", err)
		}
		if res.Acted != 0 {
			t.Fatalf("acted = %d, want 0", res.Acted)
		}
		if got := env.itemState(t, "YOUNG"); got != store.BacklogQueued {
			t.Fatalf("young state = %s, want queued", got)
		}
	})
	t.Run("low confidence is a no-op", func(t *testing.T) {
		env := newGroomerEnv(t, base, &fakeChat{replies: []string{verdictJSON(t, "duplicate", 0.5)}})
		env.seedQueued(t, "OLD", titleA, store.P2, 48*time.Hour)
		env.seedQueued(t, "YOUNG", titleB, store.P2, time.Hour)
		res, err := env.groomer.Tick(context.Background())
		if err != nil {
			t.Fatalf("tick: %v", err)
		}
		if res.Acted != 0 {
			t.Fatalf("acted = %d, want 0", res.Acted)
		}
	})
}

// Kill-test (c): LLM down ⇒ deterministic actions still land, gray-band
// actions never do, and the tick notes llm_unavailable.
func TestGroomerLLMDownDeterministicOnly(t *testing.T) {
	env := newGroomerEnv(t, mills.GroomerPolicy{
		Enabled: true, DryRun: boolPtr(false),
		Allow: mills.GroomerAllowPolicy{DedupClose: true},
	}, nil) // no triage wired at all
	// Hard duplicate: deterministic, must still retire.
	env.seedQueued(t, "OLD", "Wire spawn pool health probe", store.P2, 48*time.Hour)
	env.seedQueued(t, "YOUNG", "Wire the spawn pool health probe", store.P2, time.Hour)
	// Gray-band pair: must be skipped without a verdict.
	env.seedQueued(t, "G1", "Mills telemetry stage rollup endpoint", store.P2, 48*time.Hour)
	env.seedQueued(t, "G2", "Mills telemetry stage rollup panel for HUD", store.P2, time.Hour)

	res, err := env.groomer.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Acted != 1 {
		t.Fatalf("acted = %d, want 1 (hard dup only)", res.Acted)
	}
	if res.Note != "llm_unavailable" {
		t.Fatalf("note = %q, want llm_unavailable", res.Note)
	}
	if got := env.itemState(t, "G2"); got != store.BacklogQueued {
		t.Fatalf("gray-band item retired without a verdict: %s", got)
	}
}

// A failing LLM backend mid-tick behaves like an outage: the error is
// counted, the breaker opens, and no judgment-gated action lands.
func TestGroomerLLMErrorOpensBreaker(t *testing.T) {
	env := newGroomerEnv(t, mills.GroomerPolicy{
		Enabled: true, DryRun: boolPtr(false),
		Allow: mills.GroomerAllowPolicy{DedupClose: true},
	}, &fakeChat{err: errors.New("backend 503")})
	env.seedQueued(t, "G1", "Mills telemetry stage rollup endpoint", store.P2, 48*time.Hour)
	env.seedQueued(t, "G2", "Mills telemetry stage rollup panel for HUD", store.P2, time.Hour)

	res, err := env.groomer.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Acted != 0 || res.Errored != 1 {
		t.Fatalf("acted=%d errored=%d, want 0/1", res.Acted, res.Errored)
	}
	if res.Note != "llm_unavailable" {
		t.Fatalf("note = %q", res.Note)
	}
}

// Kill-test (a): a concurrent writer claims the item between the groomer's
// list and its transition — exactly one side wins and the loser skips clean.
func TestGroomerRetireLosesCASRaceCleanly(t *testing.T) {
	env := newGroomerEnv(t, mills.GroomerPolicy{
		Enabled: true, DryRun: boolPtr(false),
		Allow: mills.GroomerAllowPolicy{DedupClose: true},
	}, nil)
	chat := &fakeChat{replies: []string{verdictJSON(t, "duplicate", 0.95)}}
	// Mid-tick, before the verdict returns, "the reconciler" moves the retire
	// candidate off queued (fresh items are claim_version 0).
	chat.OnCall = func(int) {
		if _, err := env.store.Backlog.TransitionState(
			context.Background(), "YOUNG", 0, store.BacklogQueued, store.BacklogRunning,
		); err != nil {
			t.Fatalf("race transition: %v", err)
		}
	}
	env.groomer.Triage = &Triage{Client: chat}
	env.seedQueued(t, "OLD", "Mills telemetry stage rollup endpoint", store.P2, 48*time.Hour)
	env.seedQueued(t, "YOUNG", "Mills telemetry stage rollup panel for HUD", store.P2, time.Hour)

	res, err := env.groomer.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Acted != 0 {
		t.Fatalf("acted = %d, want 0 (lost the race)", res.Acted)
	}
	if got := env.itemState(t, "YOUNG"); got != store.BacklogRunning {
		t.Fatalf("state = %s, want running (the racing writer won)", got)
	}
	if n := env.eventCount(t, "overseer.groomer.dedup_close"); n != 0 {
		t.Fatalf("retire event recorded despite lost race")
	}
}

// Zombies are flagged exactly once across ticks, event-only.
func TestGroomerZombieFlagOnce(t *testing.T) {
	env := newGroomerEnv(t, mills.GroomerPolicy{
		Enabled: true, DryRun: boolPtr(false),
	}, nil)
	env.seedQueued(t, "Z", "Ancient forgotten work item", store.P3, 30*24*time.Hour)

	for i := 0; i < 2; i++ {
		if _, err := env.groomer.Tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if n := env.eventCount(t, "overseer.groomer.zombie_flagged"); n != 1 {
		t.Fatalf("zombie flags = %d, want 1", n)
	}
	if got := env.itemState(t, "Z"); got != store.BacklogQueued {
		t.Fatalf("zombie mutated: %s", got)
	}
}

// A stale P0 demotes one bucket when allowed, and only once per item.
func TestGroomerStalePriorityDemote(t *testing.T) {
	env := newGroomerEnv(t, mills.GroomerPolicy{
		Enabled: true, DryRun: boolPtr(false),
		Allow: mills.GroomerAllowPolicy{Reprioritize: true},
	}, nil)
	item := env.seedQueued(t, "S", "Urgent thing nobody started", store.P0, 10*24*time.Hour)
	// Backdate UpdatedAt past the staleness age (Put stamps it to now).
	backdate(t, env.store, item.ID, env.now.Add(-10*24*time.Hour))

	res, err := env.groomer.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Acted != 1 {
		t.Fatalf("acted = %d, want 1 (res=%+v)", res.Acted, res)
	}
	got, err := env.store.Backlog.Get(context.Background(), "S")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Priority != store.P1 {
		t.Fatalf("priority = %s, want P1", got.Priority)
	}
	// Lifetime once: backdate again, second tick must skip.
	backdate(t, env.store, item.ID, env.now.Add(-10*24*time.Hour))
	res2, err := env.groomer.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if res2.Acted != 0 {
		t.Fatalf("second demotion committed; want lifetime-once skip")
	}
}

// Kill-test (b): a retired item drops out of every state-specific list the
// admission/sweep paths read.
func TestRetiredStateLeaksNowhere(t *testing.T) {
	env := newGroomerEnv(t, mills.GroomerPolicy{Enabled: true}, nil)
	item := env.seedQueued(t, "R", "Retired item", store.P2, time.Hour)
	item.State = store.BacklogRetired
	if err := env.store.Backlog.Put(context.Background(), item); err != nil {
		t.Fatalf("retire: %v", err)
	}
	for _, state := range []store.BacklogState{
		store.BacklogQueued, store.BacklogRunning, store.BacklogEscalated, store.BacklogPaused, store.BacklogMerged,
	} {
		items, err := env.store.Backlog.ListByState(context.Background(), state)
		if err != nil {
			t.Fatalf("list %s: %v", state, err)
		}
		for _, it := range items {
			if it.ID == "R" {
				t.Fatalf("retired item leaked into ListByState(%s)", state)
			}
		}
	}
	items, err := env.store.Backlog.ListByState(context.Background(), store.BacklogRetired)
	if err != nil || len(items) != 1 {
		t.Fatalf("retired list = %v err=%v, want the one item", items, err)
	}
}

// Committed actions respect the per-tick cap.
func TestGroomerTickCap(t *testing.T) {
	env := newGroomerEnv(t, mills.GroomerPolicy{
		Enabled: true, DryRun: boolPtr(false),
		MaxActionsPerTick: 1,
		Allow:             mills.GroomerAllowPolicy{DedupClose: true},
	}, nil)
	// Two independent hard-duplicate pairs ⇒ two eligible actions, cap 1.
	env.seedQueued(t, "A1", "Refactor spawn pool warmup logic", store.P2, 48*time.Hour)
	env.seedQueued(t, "A2", "Refactor the spawn pool warmup logic", store.P2, time.Hour)
	env.seedQueued(t, "B1", "Document weaver gather endpoint", store.P2, 48*time.Hour)
	env.seedQueued(t, "B2", "Document the weaver gather endpoint", store.P2, time.Hour)

	res, err := env.groomer.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Acted != 1 {
		t.Fatalf("acted = %d, want 1 (capped)", res.Acted)
	}
}

// Disabled (master gate off) ⇒ complete no-op.
func TestGroomerDisabledNoOp(t *testing.T) {
	env := newGroomerEnv(t, mills.GroomerPolicy{
		Enabled: true, DryRun: boolPtr(false),
		Allow: mills.GroomerAllowPolicy{DedupClose: true},
	}, nil)
	env.policy.Overseers.Enabled = false
	env.seedQueued(t, "OLD", "Add HUD overseer status panel", store.P2, 48*time.Hour)
	env.seedQueued(t, "YOUNG", "Add the HUD overseer status panel", store.P2, time.Hour)

	res, err := env.groomer.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Inspected != 0 || res.Acted != 0 {
		t.Fatalf("disabled groomer did work: %+v", res)
	}
}

// backdate rewrites updated_at directly (Put always stamps now, so staleness
// scenarios reach under it via the raw DB handle).
func backdate(t *testing.T, st *store.Store, id string, to time.Time) {
	t.Helper()
	if _, err := st.DB().ExecContext(context.Background(),
		`UPDATE backlog_items SET updated_at = ? WHERE id = ?`,
		to.UTC().Format(time.RFC3339Nano), id,
	); err != nil {
		t.Fatalf("backdate %s: %v", id, err)
	}
}
