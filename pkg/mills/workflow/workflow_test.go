package workflow

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// ---------------------------------------------------------------------------
// Real-store test harness. Every scenario runs against a temp-file SQLite store
// with migration 004 applied (via store.Open), NOT an in-memory map. The
// durable success-row count replaces the spike's atomic effect-counter; "live
// effects in THIS run" is measured as the delta in that count across a run.
// ---------------------------------------------------------------------------

// newTestStore opens a fresh temp-file mills store with all migrations applied.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(dir, "mills.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// newHost builds an EffectHost bound to a DAO journal over st. Each call models
// a "fresh interpreter+host" while the DURABLE journal (the store) persists.
func newHost(t *testing.T, st *store.Store, runID string) *EffectHost {
	t.Helper()
	j := NewDAOJournal(context.Background(), st)
	return NewEffectHost(runID, j)
}

// successCount reads the durable success-row count for a run.
func successCount(t *testing.T, st *store.Store, runID string) int64 {
	t.Helper()
	j := NewDAOJournal(context.Background(), st)
	n, err := j.SuccessCount(runID)
	if err != nil {
		t.Fatalf("success count: %v", err)
	}
	return n
}

// runAndDelta runs h.Run(script) and returns the number of LIVE effects that
// fired during the run (the change in durable success rows). This is the
// faithful equivalent of the spike's fresh-counter-per-host assertion.
func runAndDelta(t *testing.T, st *store.Store, h *EffectHost, script string) (int64, error) {
	t.Helper()
	before := successCount(t, st, h.RunID)
	err := h.Run(script)
	after := successCount(t, st, h.RunID)
	return after - before, err
}

// ---------------------------------------------------------------------------
// Workflow scripts (verbatim from the spike spec).
// ---------------------------------------------------------------------------

func workflowScript(nIters, fanOut int) string {
	return fmt.Sprintf(`
agent("a")
gate("g")
agent("b")

def branch0(): agent("p0-worker")
def branch1(): agent("p1-worker")
parallel([branch0, branch1], fan_out_width=%d)

def drain_body(i):
    agent("drain-item")
    return i >= %d - 1   # truthy => dry => stop; yields exactly nIters bodies
loop_until_dry(drain_body, max_iter=%d)
`, fanOut, nIters, nIters+5)
}

// countLiveSteps re-derives, on a FRESH run+store, how many distinct effect
// steps a script produces (its total success rows). Used to compute expected
// live deltas independent of any other run.
func countLiveSteps(t *testing.T, script string) int64 {
	t.Helper()
	st := newTestStore(t)
	h := newHost(t, st, "count-"+t.Name())
	if err := h.Run(script); err != nil {
		t.Fatalf("countLiveSteps: run failed: %v", err)
	}
	return successCount(t, st, h.RunID)
}

// ---------------------------------------------------------------------------
// Scenario 1: Short-circuit + exactly-once (against a real store).
// ---------------------------------------------------------------------------
func TestScenario1_ShortCircuitExactlyOnce(t *testing.T) {
	st := newTestStore(t)
	script := workflowScript(3, 2)
	runID := "run-s1"

	// First run.
	live1, err := runAndDelta(t, st, newHost(t, st, runID), script)
	if err != nil {
		t.Fatalf("first run failed: %v", err)
	}
	if live1 == 0 {
		t.Fatalf("expected live effects on first run, got 0")
	}
	records := successCount(t, st, runID)

	// DROP interpreter+host; keep ONLY the durable store. Replay on a brand-new
	// host -> every recorded step short-circuits, ZERO new live effects.
	live2, err := runAndDelta(t, st, newHost(t, st, runID), script)
	if err != nil {
		t.Fatalf("replay run failed: %v", err)
	}
	if live2 != 0 {
		t.Fatalf("replay re-executed effects: live=%d, want 0 (all should short-circuit)", live2)
	}
	if successCount(t, st, runID) != records {
		t.Fatalf("replay mutated success-row count: got %d want %d", successCount(t, st, runID), records)
	}

	// Targeted exactly-once proof: a replay that adds ONE new un-recorded call
	// must produce EXACTLY one live effect. Append a fresh gate at the tail.
	scriptPlusOne := script + "\ngate(\"new-tail-gate\")\n"
	live3, err := runAndDelta(t, st, newHost(t, st, runID), scriptPlusOne)
	if err != nil {
		t.Fatalf("plus-one replay failed: %v", err)
	}
	if live3 != 1 {
		t.Fatalf("plus-one replay: live=%d, want exactly 1 (only the new call runs live)", live3)
	}
}

// ---------------------------------------------------------------------------
// Scenario 2: Divergent loop-iteration-count replay.
// ---------------------------------------------------------------------------
func TestScenario2_DivergentLoopIterations(t *testing.T) {
	st := newTestStore(t)
	runID := "run-s2"

	short := workflowScript(3, 2)
	long := workflowScript(7, 2) // 4 extra drain iterations

	shortLive := countLiveSteps(t, short)
	longLive := countLiveSteps(t, long)
	extra := longLive - shortLive
	if extra <= 0 {
		t.Fatalf("test setup wrong: long(%d) should exceed short(%d)", longLive, shortLive)
	}

	// Record the short run.
	if _, err := runAndDelta(t, st, newHost(t, st, runID), short); err != nil {
		t.Fatalf("short run failed: %v", err)
	}

	// Replay the LONGER script against the same durable journal.
	liveExtra, err := runAndDelta(t, st, newHost(t, st, runID), long)
	if err != nil {
		var q *QuarantineError
		if errors.As(err, &q) {
			t.Fatalf("sibling key shift caused spurious quarantine: %v", err)
		}
		t.Fatalf("long replay failed: %v", err)
	}
	if liveExtra != extra {
		t.Fatalf("divergent-loop replay: live=%d, want exactly %d extra iterations (first %d cached)",
			liveExtra, extra, shortLive)
	}
}

// ---------------------------------------------------------------------------
// Scenario 3: Concurrent parallel() key assignment under goroutine jitter.
// ---------------------------------------------------------------------------
func TestScenario3_ConcurrentParallelKeyStability(t *testing.T) {
	st := newTestStore(t)
	runID := "run-s3"

	script := `
def b0(): agent("branch-A")
def b1(): agent("branch-B")
def b2(): agent("branch-C")
def b3(): agent("branch-D")
parallel([b0, b1, b2, b3], fan_out_width=4)
`
	jitter := func(primKind string, args map[string]any, seq int64) any {
		if rand.Intn(2) == 0 {
			time.Sleep(time.Duration(rand.Intn(500)) * time.Microsecond)
		}
		runtime.Gosched()
		return fmt.Sprintf("%s#%d", primKind, seq)
	}

	// Record with jitter.
	h1 := newHost(t, st, runID)
	h1.liveFn = jitter
	live, err := runAndDelta(t, st, h1, script)
	if err != nil {
		t.Fatalf("jittered record run failed: %v", err)
	}
	if live != 4 {
		t.Fatalf("expected 4 live branch effects, got %d", live)
	}
	firstKeys := stepKeySet(t, st, runID)

	// Replay many times under jitter; keys must be byte-stable and all cached.
	for iter := 0; iter < 25; iter++ {
		h := newHost(t, st, runID)
		h.liveFn = jitter
		got, err := runAndDelta(t, st, h, script)
		if err != nil {
			var q *QuarantineError
			if errors.As(err, &q) {
				t.Fatalf("iter %d: parallel branch key raced -> quarantine: %v", iter, err)
			}
			t.Fatalf("iter %d: jittered replay failed: %v", iter, err)
		}
		if got != 0 {
			t.Fatalf("iter %d: branch effect re-executed (key race): live=%d want 0", iter, got)
		}
		if keys := stepKeySet(t, st, runID); !sameKeySet(firstKeys, keys) {
			t.Fatalf("iter %d: step key set changed across jittered runs", iter)
		}
	}

	// Sanity: branch keys are b0..b3 in slice order, regardless of finish order.
	for j := 0; j < 4; j++ {
		want := fmt.Sprintf("/b%d/", j)
		found := false
		for k := range firstKeys {
			if strings.Contains(k, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing deterministic branch segment %q in keys: %v", want, keysOf(firstKeys))
		}
	}
}

// ---------------------------------------------------------------------------
// Scenario 4: Non-determinism tripwire (durable variant).
// Record a run, then CORRUPT a recorded step's call_hash directly in the store
// (via SQL, since AppendStep refuses a silent overwrite) and replay. The
// corrupted step QUARANTINES; it does NOT double-execute and does NOT
// mass-abort.
// ---------------------------------------------------------------------------
func TestScenario4_NonDeterminismTripwire(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	runID := "run-s4"
	script := workflowScript(2, 2)

	if _, err := runAndDelta(t, st, newHost(t, st, runID), script); err != nil {
		t.Fatalf("record run failed: %v", err)
	}

	// Find a stable target: the root gate("g") success step.
	var targetKey, targetHash string
	all, err := st.Workflow.ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("list by run: %v", err)
	}
	for _, s := range all {
		if s.EventType == store.WorkflowEventGateEval &&
			s.Status == store.WorkflowStepSuccess &&
			strings.Contains(s.StepKey, "root/gate") {
			targetKey = s.StepKey
			targetHash = s.CallHash
			break
		}
	}
	if targetKey == "" {
		t.Fatalf("could not find target gate step to corrupt")
	}

	// Corrupt the recorded call_hash directly in the durable store. AppendStep
	// would refuse this overwrite (ErrStepCallHashMismatch); SQL bypasses it to
	// simulate on-disk nondeterminism / tampering.
	corrupt := "deadbeef" + targetHash[8:]
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE workflow_steps SET call_hash = ? WHERE run_id = ? AND step_key = ?`,
		corrupt, runID, targetKey); err != nil {
		t.Fatalf("corrupt call_hash: %v", err)
	}

	// Replay -> must quarantine at the corrupted step.
	rowsBefore := countRows(t, st, runID)
	err = newHost(t, st, runID).Run(script)
	var q *QuarantineError
	if !errors.As(err, &q) {
		t.Fatalf("expected QuarantineError, got %v", err)
	}
	if q.StepKey != targetKey {
		t.Fatalf("quarantine on wrong step: got %s want %s", q.StepKey, targetKey)
	}

	// No double-execution: the corrupted step did NOT run live (no fresh
	// pending+success cycle, no new row), and effects before it replayed from
	// cache. The quarantine flips the corrupted row in place (success -> error),
	// so the TOTAL row count is unchanged — no live re-execution inserted a row.
	if rowsAfter := countRows(t, st, runID); rowsAfter != rowsBefore {
		t.Fatalf("quarantine double-executed effects: rows %d -> %d (want unchanged)", rowsBefore, rowsAfter)
	}

	// No mass-abort: exactly ONE quarantined (error-terminal) record.
	all, err = st.Workflow.ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("list by run after quarantine: %v", err)
	}
	quarantined := 0
	for _, s := range all {
		if s.Status == store.WorkflowStepError {
			quarantined++
		}
	}
	if quarantined != 1 {
		t.Fatalf("expected exactly 1 quarantined record (no mass-abort), got %d", quarantined)
	}
}

// ---------------------------------------------------------------------------
// Scenario 5: Interpreter-version drift.
// ---------------------------------------------------------------------------
func TestScenario5_InterpreterVersionDrift(t *testing.T) {
	st := newTestStore(t)
	runID := "run-s5"
	script := workflowScript(2, 2)

	if _, err := runAndDelta(t, st, newHost(t, st, runID), script); err != nil {
		t.Fatalf("record run failed: %v", err)
	}

	// Replay with a drifted pinned version -> REFUSE before any effect runs.
	h2 := newHost(t, st, runID)
	h2.InterpreterVersion = "starlark-go@v9.9.9-future"
	before := successCount(t, st, runID)
	err := h2.Run(script)
	var vd *VersionDriftError
	if !errors.As(err, &vd) {
		t.Fatalf("expected VersionDriftError, got %v", err)
	}
	if after := successCount(t, st, runID); after != before {
		t.Fatalf("version-drift replay ran effects before refusing: success rows %d -> %d", before, after)
	}
}

// ---------------------------------------------------------------------------
// Scenario 6: Hostile params clamped by host ceiling.
// ---------------------------------------------------------------------------
func TestScenario6_HostileParamsClamped(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	runID := "run-s6"

	absurdFanout := HostMaxFanout * 1000
	absurdIter := HostMaxIter * 1000

	branches := make([]string, absurdFanout)
	defs := &strings.Builder{}
	names := &strings.Builder{}
	for i := 0; i < absurdFanout; i++ {
		fmt.Fprintf(defs, "def hb%d(): agent(\"hostile-%d\")\n", i, i)
		branches[i] = fmt.Sprintf("hb%d", i)
	}
	fmt.Fprintf(names, "[%s]", strings.Join(branches, ", "))

	script := fmt.Sprintf(`
%s
parallel(%s, fan_out_width=%d)

def never_dry(i):
    agent("hostile-drain")
    return False   # never dry -> only the host clamp stops the loop
loop_until_dry(never_dry, max_iter=%d)
`, defs.String(), names.String(), absurdFanout, absurdIter)

	live, err := runAndDelta(t, st, newHost(t, st, runID), script)
	if err != nil {
		t.Fatalf("hostile run failed: %v", err)
	}

	wantBranches := int64(clampFanout(absurdFanout))
	wantIters := int64(clampIter(absurdIter))
	wantTotal := wantBranches + wantIters
	if live != wantTotal {
		t.Fatalf("clamp failed: live effects=%d want %d (fanout %d + iters %d)",
			live, wantTotal, wantBranches, wantIters)
	}

	// Defensive: ensure NO branch index >= HostMaxFanout and NO loop iter >=
	// HostMaxIter was ever keyed.
	all, err := st.Workflow.ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("list by run: %v", err)
	}
	for _, s := range all {
		if strings.Contains(s.StepKey, fmt.Sprintf("/b%d/", HostMaxFanout)) {
			t.Fatalf("clamp leak: found branch index >= HostMaxFanout in key %s", s.StepKey)
		}
		if strings.Contains(s.StepKey, fmt.Sprintf("loop:drain#%d/", HostMaxIter)) {
			t.Fatalf("clamp leak: found loop iter >= HostMaxIter in key %s", s.StepKey)
		}
	}
}

// ---------------------------------------------------------------------------
// Scenario 7: Capability confinement. The interpreter universe cannot reach
// net/os/time/random and load() is disabled.
// ---------------------------------------------------------------------------
func TestScenario7_CapabilityConfinement(t *testing.T) {
	st := newTestStore(t)
	h := newHost(t, st, "run-s7-universe")

	// STRUCTURAL check: the universe is EXACTLY the 7 effect builtins.
	want := []string{"agent", "ctx_now", "ctx_uuid", "gate", "loop_until_dry", "merge", "parallel"}
	got := h.UniverseNames()
	if len(got) != len(want) {
		t.Fatalf("universe must contain exactly %d names, got %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("universe name mismatch at %d: got %q want %q (full: %v)", i, got[i], want[i], got)
		}
	}
	for _, forbidden := range []string{"os", "time", "random", "open", "load", "print", "json", "math", "struct", "net"} {
		for _, name := range got {
			if name == forbidden {
				t.Fatalf("capability leak: forbidden name %q present in universe", forbidden)
			}
		}
	}

	// BEHAVIORAL check: every forbidden reference / load() FAILS, and a bare
	// reference fails specifically with "undefined" (the NAME is absent).
	cases := []struct {
		name       string
		script     string
		mustResolv bool
	}{
		{"bare_os", `x = os`, true},
		{"bare_time", `x = time`, true},
		{"bare_random", `x = random`, true},
		{"bare_open", `x = open`, true},
		{"bare_net", `x = net`, true},
		{"call_os", `os.getenv("HOME")`, false},
		{"no_load", `load("builtins.star", "x")`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			runID := "run-s7-" + tc.name
			before := successCount(t, st, runID)
			err := newHost(t, st, runID).Run(tc.script)
			if err == nil {
				t.Fatalf("expected capability-confinement error for %q, got nil", tc.script)
			}
			if after := successCount(t, st, runID); after != before {
				t.Fatalf("capability leak: effect fired (success rows %d -> %d) for %q", before, after, tc.script)
			}
			if tc.mustResolv && !strings.Contains(strings.ToLower(err.Error()), "undefined") {
				t.Fatalf("bare reference %q should fail as 'undefined', got: %v", tc.script, err)
			}
			if tc.name == "no_load" && !strings.Contains(strings.ToLower(err.Error()), "load") {
				t.Fatalf("load() should fail mentioning 'load', got: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// CARRY-FORWARD #2 (required): scope-preserving sibling INSERT and DELETE drift
// in ONE scope frame. Insert/remove an unrelated agent()/gate() call in the
// SAME frame, replay, and assert every unchanged sibling key still resolves to
// its ORIGINAL durable record.
//
// The 7 spike scenarios only used a flat global counter; a buggy
// scope-preserving-but-flat-leaf counter would still pass them. This test would
// NOT: a flat leaf ordinal would shift the trailing siblings' keys when a
// sibling is inserted/removed in the middle of the frame.
// ---------------------------------------------------------------------------
func TestCarryForward2_SiblingInsertDeleteDrift(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Pass 1: a flat root frame with several DISTINCT-arg siblings. Each call
	// has unique args, so each leaf dupOrdinal is 0 and the key is a pure
	// function of its own (primKind, callHash) — order-independent.
	base := `
agent("alpha")
gate("gate-1")
agent("beta")
gate("gate-2")
agent("gamma")
`
	runID := "run-cf2"
	if err := newHost(t, st, runID).Run(base); err != nil {
		t.Fatalf("base run failed: %v", err)
	}

	// Capture every sibling's (step_key -> call_hash) after the base run.
	orig := map[string]string{}
	all, err := st.Workflow.ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("list by run: %v", err)
	}
	for _, s := range all {
		if s.Status == store.WorkflowStepSuccess {
			orig[s.StepKey] = s.CallHash
		}
	}
	if len(orig) != 5 {
		t.Fatalf("base run expected 5 sibling success rows, got %d", len(orig))
	}

	// The two siblings we will keep unchanged across the drift and assert on.
	// (Their keys must be byte-identical before and after the insert+delete.)
	stableProbes := []string{"alpha", "gamma"}
	stableKeys := map[string]string{}
	for _, probe := range stableProbes {
		found := ""
		for k, h := range orig {
			// agent leaf segment for arg "alpha"/"gamma": its key contains the
			// agent prim and a hash derived only from its own args, so we locate
			// it by re-deriving the same hash.
			wantHash := canonicalCallHash("agent", map[string]any{"_0": probe})
			if h == wantHash {
				found = k
				break
			}
		}
		if found == "" {
			t.Fatalf("could not locate stable sibling %q in base keys", probe)
		}
		stableKeys[probe] = found
	}

	// Pass 2 — DELETE the middle sibling gate("gate-1") from the journal AND
	// INSERT a brand-new unrelated sibling gate("gate-INSERTED") in the same
	// frame. Replay a script reflecting that edit.
	deletedHash := canonicalCallHash("gate", map[string]any{"_0": "gate-1"})
	var deletedKey string
	for k, h := range orig {
		if h == deletedHash {
			deletedKey = k
			break
		}
	}
	if deletedKey == "" {
		t.Fatalf("could not locate gate-1 sibling to delete")
	}
	if _, err := st.DB().ExecContext(ctx,
		`DELETE FROM workflow_steps WHERE run_id = ? AND step_key = ?`, runID, deletedKey); err != nil {
		t.Fatalf("delete sibling: %v", err)
	}

	// Asymmetric edit: gate-1 is DELETED from position 1, and a brand-new
	// gate-INSERTED is added at the TAIL. This deliberately makes the trailing
	// siblings' POSITIONAL index change (beta/gate-2/gamma each move up one slot)
	// while their args are unchanged. A correct args-keyed per-frame dupCounts
	// gives every distinct-arg call dupOrdinal=0, so the keys stay byte-stable; a
	// flat positional leaf counter would shift them and miss cache.
	drifted := `
agent("alpha")
agent("beta")
gate("gate-2")
agent("gamma")
gate("gate-INSERTED")
`
	before := successCount(t, st, runID)
	if err := newHost(t, st, runID).Run(drifted); err != nil {
		var q *QuarantineError
		if errors.As(err, &q) {
			t.Fatalf("sibling insert/delete shifted a key -> spurious quarantine: %v", err)
		}
		t.Fatalf("drifted replay failed: %v", err)
	}

	// Exactly ONE new live effect fires: the INSERTED gate("gate-INSERTED") runs
	// for the first time. gate-1 was both deleted from the journal AND removed
	// from the script, so it does not re-run. Every other sibling (alpha, beta,
	// gate-2, gamma) short-circuits from cache. If a flat-leaf counter had
	// shifted those siblings' keys, they would miss cache and re-run live
	// (delta > 1) or quarantine (caught above).
	if liveDelta := successCount(t, st, runID) - before; liveDelta != 1 {
		t.Fatalf("expected exactly 1 live effect after insert+delete, got %d", liveDelta)
	}

	// THE ASSERTION WITH TEETH: each unchanged sibling key still resolves to its
	// ORIGINAL durable record (byte-identical key + same call_hash). A flat-leaf
	// counter would have shifted beta/gamma/gate-2 when gate-1 was removed and
	// gate-INSERTED added.
	for probe, key := range stableKeys {
		got, err := st.Workflow.GetStep(ctx, runID, key)
		if err != nil {
			t.Fatalf("unchanged sibling %q (key %q) lookup failed: %v", probe, key, err)
		}
		wantHash := canonicalCallHash("agent", map[string]any{"_0": probe})
		if got.CallHash != wantHash {
			t.Fatalf("sibling %q drifted: key %q now call_hash %q want %q", probe, key, got.CallHash, wantHash)
		}
		if got.Status != store.WorkflowStepSuccess {
			t.Fatalf("sibling %q (key %q) status changed: %s", probe, key, got.Status)
		}
	}

	// gate-2 (a middle sibling, after the edit point) must also be untouched.
	gate2Hash := canonicalCallHash("gate", map[string]any{"_0": "gate-2"})
	gate2Found := false
	all, err = st.Workflow.ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("list by run after drift: %v", err)
	}
	for _, s := range all {
		if s.CallHash == gate2Hash && s.Status == store.WorkflowStepSuccess {
			gate2Found = true
			break
		}
	}
	if !gate2Found {
		t.Fatalf("gate-2 sibling lost its original record after sibling insert/delete")
	}
}

// ---------------------------------------------------------------------------
// CARRY-FORWARD #3 (required): durable UNIQUE(run_id, step_key) pending->success
// upsert against the real WorkflowDAO (the spike only exercised a map). Proves
// the durable journal advances a single row in place rather than inserting a
// duplicate, and that the success-row count is correct.
// ---------------------------------------------------------------------------
func TestCarryForward3_DurablePendingSuccessUpsert(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	runID := "run-cf3"
	j := NewDAOJournal(ctx, st)

	stepKey := "root/agent~abc#0"
	callHash := canonicalCallHash("agent", map[string]any{"_0": "x"})

	// pending append (record-before-effect).
	if err := j.Append(Record{
		RunID:    runID,
		StepKey:  stepKey,
		PrimName: "agent",
		CallHash: callHash,
		Status:   StatusPending,
	}); err != nil {
		t.Fatalf("pending append: %v", err)
	}
	if got, ok, err := j.Get(runID, stepKey); err != nil || !ok || got.Status != StatusPending {
		t.Fatalf("after pending append: ok=%v status=%v err=%v", ok, got.Status, err)
	}
	if n, _ := j.SuccessCount(runID); n != 0 {
		t.Fatalf("success count after pending should be 0, got %d", n)
	}
	if rows := countRows(t, st, runID); rows != 1 {
		t.Fatalf("expected 1 durable row after pending, got %d", rows)
	}

	// success append for the SAME (run_id, step_key) -> upsert in place, not a
	// new row.
	if err := j.Append(Record{
		RunID:      runID,
		StepKey:    stepKey,
		PrimName:   "agent",
		CallHash:   callHash,
		Status:     StatusSuccess,
		ResultBlob: []byte(`"agent#1"`),
	}); err != nil {
		t.Fatalf("success append: %v", err)
	}
	if rows := countRows(t, st, runID); rows != 1 {
		t.Fatalf("UNIQUE(run_id, step_key) violated: expected 1 row after upsert, got %d", rows)
	}
	got, ok, err := j.Get(runID, stepKey)
	if err != nil || !ok {
		t.Fatalf("get after success: ok=%v err=%v", ok, err)
	}
	if got.Status != StatusSuccess {
		t.Fatalf("status not advanced to success: %v", got.Status)
	}
	if string(got.ResultBlob) != `"agent#1"` {
		t.Fatalf("result blob not persisted on upsert: %q", string(got.ResultBlob))
	}
	if n, _ := j.SuccessCount(runID); n != 1 {
		t.Fatalf("success count after upsert should be 1, got %d", n)
	}

	// Idempotent re-append of the identical success step -> still one row, count
	// unchanged.
	if err := j.Append(Record{
		RunID:      runID,
		StepKey:    stepKey,
		PrimName:   "agent",
		CallHash:   callHash,
		Status:     StatusSuccess,
		ResultBlob: []byte(`"agent#1"`),
	}); err != nil {
		t.Fatalf("idempotent re-append: %v", err)
	}
	if rows := countRows(t, st, runID); rows != 1 {
		t.Fatalf("idempotent re-append created a duplicate row: %d", rows)
	}
	if n, _ := j.SuccessCount(runID); n != 1 {
		t.Fatalf("success count after idempotent re-append should be 1, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// test helpers
// ---------------------------------------------------------------------------

func stepKeySet(t *testing.T, st *store.Store, runID string) map[string]bool {
	t.Helper()
	all, err := st.Workflow.ListByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("list by run: %v", err)
	}
	out := map[string]bool{}
	for _, s := range all {
		out[s.StepKey] = true
	}
	return out
}

func sameKeySet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func countRows(t *testing.T, st *store.Store, runID string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM workflow_steps WHERE run_id = ?`, runID).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

func TestAssertQueuedProofRequiresAutoMergedNonEmptyMR(t *testing.T) {
	now := time.Now().UTC()
	valid := QueuedProofEvidence{
		CapturedAt: now, BacklogID: "pattern-item", RunID: "run-1",
		States:   []QueuedProofState{{State: "queued", ObservedAt: now.Add(-time.Minute)}, {State: "done", ObservedAt: now}},
		Terminal: QueuedProofTerminal{State: "done"},
		MR:       QueuedProofMergeRequest{Project: "services/loom-core", IID: 42, URL: "https://gitlab.example/mr/42", State: "merged", ChangedFiles: 1, Additions: 2},
	}
	if err := AssertQueuedProof(valid); err != nil {
		t.Fatalf("valid queued proof rejected: %v", err)
	}

	for name, mutate := range map[string]func(*QueuedProofEvidence){
		"open MR":     func(e *QueuedProofEvidence) { e.MR.State = "opened" },
		"empty MR":    func(e *QueuedProofEvidence) { e.MR.ChangedFiles, e.MR.Additions = 0, 0 },
		"quarantined": func(e *QueuedProofEvidence) { e.Terminal.Quarantined = true },
		"not done":    func(e *QueuedProofEvidence) { e.States[1].State, e.Terminal.State = "paused", "paused" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.States = append([]QueuedProofState(nil), valid.States...)
			mutate(&candidate)
			if err := AssertQueuedProof(candidate); err == nil {
				t.Fatal("unsafe queued proof unexpectedly passed")
			}
		})
	}
}
