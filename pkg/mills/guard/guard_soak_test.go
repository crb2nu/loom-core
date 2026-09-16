package guard

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// soakSpy records the S2 decisions the recorder/harness hand to the store.
type soakSpy struct {
	decisions []bool // would-have-acted per call
}

func (s *soakSpy) RecordOverseerSoakDecision(_ context.Context, at time.Time, wouldHaveActed, policyDisagreement bool) error {
	if at.IsZero() {
		panic("zero decision time")
	}
	if policyDisagreement {
		panic("recorder must never infer a policy disagreement")
	}
	s.decisions = append(s.decisions, wouldHaveActed)
	return nil
}

// TestActionRecorder_DryRunActionsRecordSoakDecisions pins the evidence path
// the S2 promotion runbook reads: every DRY-RUN action record (Record and the
// first RecordOnce) is a would-have-acted decision; committed actions,
// observations, flags and repeated RecordOnce calls are not. Live
// 2026-09-02: this path had no callers, so `mills_overseer_soak_*` never
// moved and the verdict could only fail closed.
func TestActionRecorder_DryRunActionsRecordSoakDecisions(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "soak.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	spy := &soakSpy{}
	dry := true
	rec := &ActionRecorder{Events: st.Events, Actor: "overseer.test", Soak: spy, DryRun: func() bool { return dry }}

	if err := rec.Record(ctx, "act", "backlog_item", "A", nil); err != nil {
		t.Fatalf("record: %v", err)
	}
	if ok, err := rec.RecordOnce(ctx, "flag", "backlog_item", "B", nil); err != nil || !ok {
		t.Fatalf("record once: ok=%v err=%v", ok, err)
	}
	if ok, err := rec.RecordOnce(ctx, "flag", "backlog_item", "B", nil); err != nil || ok {
		t.Fatalf("second record once: ok=%v err=%v (want dedup)", ok, err)
	}
	if err := rec.Observe(ctx, "seen", "backlog_item", "C", nil); err != nil {
		t.Fatalf("observe: %v", err)
	}
	if _, err := rec.FlagOnce(ctx, "flagged", "backlog_item", "D", nil); err != nil {
		t.Fatalf("flag once: %v", err)
	}
	if got := len(spy.decisions); got != 2 {
		t.Fatalf("soak decisions = %d, want 2 (Record + first RecordOnce)", got)
	}
	for i, wouldAct := range spy.decisions {
		if !wouldAct {
			t.Fatalf("decision %d: would-have-acted = false, want true for a dry-run action", i)
		}
	}

	// Committed mode: the action is real, not a soak decision.
	dry = false
	if err := rec.Record(ctx, "act", "backlog_item", "E", nil); err != nil {
		t.Fatalf("committed record: %v", err)
	}
	if got := len(spy.decisions); got != 2 {
		t.Fatalf("committed record added a soak decision: %d", got)
	}

	// Unwired recorder: no panic, no decision.
	bare := &ActionRecorder{Events: st.Events, Actor: "overseer.bare"}
	if err := bare.Record(ctx, "act", "backlog_item", "F", nil); err != nil {
		t.Fatalf("bare record: %v", err)
	}
}

type quietAgent struct {
	name string
	res  TickResult
	err  error
}

func (a quietAgent) Name() string                             { return a.name }
func (a quietAgent) Tick(context.Context) (TickResult, error) { return a.res, a.err }

// TestHarness_QuietDryRunTickRecordsNoActionDecision: a dry-run tick that
// inspected the floor and planned nothing is still a reviewed decision
// (would-have-acted = false) — the runbook fails closed on a UTC day with no
// decisions at all. Ticks that planned actions leave the decision to the
// recorder; committed mode and errored ticks record nothing.
func TestHarness_QuietDryRunTickRecordsNoActionDecision(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		dry    bool
		res    TickResult
		err    error
		expect []bool
	}{
		{"quiet dry-run tick", true, TickResult{Inspected: 4}, nil, []bool{false}},
		{"dry-run tick that planned", true, TickResult{Inspected: 4, Planned: 1}, nil, nil},
		{"committed tick", false, TickResult{Inspected: 4}, nil, nil},
		{"errored tick", true, TickResult{}, context.DeadlineExceeded, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := &soakSpy{}
			dry := tc.dry
			h := &Harness{
				Agent:  quietAgent{name: "test", res: tc.res, err: tc.err},
				Soak:   spy,
				DryRun: func() bool { return dry },
			}
			_, _ = h.TickOnce(ctx)
			if len(spy.decisions) != len(tc.expect) {
				t.Fatalf("decisions = %v, want %v", spy.decisions, tc.expect)
			}
			for i := range tc.expect {
				if spy.decisions[i] != tc.expect[i] {
					t.Fatalf("decisions = %v, want %v", spy.decisions, tc.expect)
				}
			}
		})
	}

	// Unwired harness: no panic.
	h := &Harness{Agent: quietAgent{name: "bare", res: TickResult{Inspected: 1}}}
	if _, err := h.TickOnce(ctx); err != nil {
		t.Fatalf("bare tick: %v", err)
	}
}
