package guard

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// The report reads the events table through its narrowest surface.
var _ EventLister = (*store.EventDAO)(nil)

// fakeEvents mirrors EventDAO.ListSince (window-bounded, newest-first,
// limit-capped) so aggregation can be tested at exact timestamps.
type fakeEvents struct {
	events   []*store.Event
	err      error
	gotSince time.Time
	gotLimit int
}

func (f *fakeEvents) ListSince(_ context.Context, since time.Time, limit int) ([]*store.Event, error) {
	f.gotSince, f.gotLimit = since, limit
	if f.err != nil {
		return nil, f.err
	}
	out := make([]*store.Event, 0, len(f.events))
	for _, e := range f.events {
		if !e.OccurredAt.Before(since) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OccurredAt.After(out[j].OccurredAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

var reportNow = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

// ev builds a fixture event at now-ago.
func ev(actor, kind, subjectKind, subjectID string, ago time.Duration) *store.Event {
	return &store.Event{
		OccurredAt:  reportNow.Add(-ago),
		Actor:       actor,
		Kind:        kind,
		SubjectKind: subjectKind,
		SubjectID:   subjectID,
	}
}

// wantAction is one expected (actor, action) cell.
type wantAction struct {
	actor          string
	action         string
	dryRun         int
	executed       int
	uniqueSubjects int
	sample         []string
}

func TestBuildPromotionReport(t *testing.T) {
	tests := []struct {
		name          string
		events        []*store.Event
		prefix        string
		window        time.Duration
		wantTotal     int
		wantDryRun    int
		wantExecuted  int
		wantZero      bool
		wantActions   []wantAction
		wantActorsSeq []string
	}{
		{
			name: "dry-run and committed kinds fold into one action row",
			events: []*store.Event{
				ev("overseer.groomer", "overseer.groomer.dedup_close.dryrun", "backlog_item", "A", time.Hour),
				ev("overseer.groomer", "overseer.groomer.dedup_close.dryrun", "backlog_item", "B", 2*time.Hour),
				ev("overseer.groomer", "overseer.groomer.dedup_close", "backlog_item", "A", 30*time.Minute),
			},
			prefix:       "overseer.",
			window:       24 * time.Hour,
			wantTotal:    3,
			wantDryRun:   2,
			wantExecuted: 1,
			wantActions: []wantAction{{
				actor: "overseer.groomer", action: "dedup_close",
				dryRun: 2, executed: 1, uniqueSubjects: 2,
				sample: []string{"backlog_item/A", "backlog_item/B"},
			}},
		},
		{
			name: "grouped by actor then action, foreign actors excluded",
			events: []*store.Event{
				ev("overseer.sentinel", "overseer.sentinel.suppress", "mill", "m1", time.Hour),
				ev("overseer.groomer", "overseer.groomer.retire", "backlog_item", "A", time.Hour),
				ev("overseer.groomer", "overseer.groomer.dedup_close.dryrun", "backlog_item", "A", time.Hour),
				ev("council.mutator", "council.mutator.apply", "policy", "p1", time.Hour),
				ev("reconciler", "reconciler.requeue", "backlog_item", "Z", time.Hour),
			},
			prefix:       "overseer.",
			window:       24 * time.Hour,
			wantTotal:    3,
			wantDryRun:   1,
			wantExecuted: 2,
			wantActions: []wantAction{
				{actor: "overseer.groomer", action: "dedup_close", dryRun: 1, uniqueSubjects: 1, sample: []string{"backlog_item/A"}},
				{actor: "overseer.groomer", action: "retire", executed: 1, uniqueSubjects: 1, sample: []string{"backlog_item/A"}},
				{actor: "overseer.sentinel", action: "suppress", executed: 1, uniqueSubjects: 1, sample: []string{"mill/m1"}},
			},
			wantActorsSeq: []string{"overseer.groomer", "overseer.sentinel"},
		},
		{
			name:         "prefix selects a single actor family",
			events:       []*store.Event{ev("council.mutator", "council.mutator.apply", "policy", "p1", time.Hour), ev("overseer.groomer", "overseer.groomer.retire", "backlog_item", "A", time.Hour)},
			prefix:       "council.",
			window:       24 * time.Hour,
			wantTotal:    1,
			wantExecuted: 1,
			wantActions:  []wantAction{{actor: "council.mutator", action: "apply", executed: 1, uniqueSubjects: 1, sample: []string{"policy/p1"}}},
		},
		{
			name:         "repeat actions on one subject dedup; sample caps at ten",
			events:       repeatedSubjects(),
			prefix:       "overseer.",
			window:       24 * time.Hour,
			wantTotal:    15,
			wantExecuted: 15,
			wantActions: []wantAction{{
				actor: "overseer.groomer", action: "retire", executed: 15, uniqueSubjects: 12,
				sample: []string{"backlog_item/s00", "backlog_item/s01", "backlog_item/s02", "backlog_item/s03",
					"backlog_item/s04", "backlog_item/s05", "backlog_item/s06", "backlog_item/s07",
					"backlog_item/s08", "backlog_item/s09"},
			}},
		},
		{
			name: "events outside the window are excluded on both ends",
			events: []*store.Event{
				ev("overseer.groomer", "overseer.groomer.retire", "backlog_item", "old", 25*time.Hour),
				ev("overseer.groomer", "overseer.groomer.retire", "backlog_item", "future", -time.Hour),
				ev("overseer.groomer", "overseer.groomer.retire", "backlog_item", "inside", time.Hour),
			},
			prefix:       "overseer.",
			window:       24 * time.Hour,
			wantTotal:    1,
			wantExecuted: 1,
			wantActions:  []wantAction{{actor: "overseer.groomer", action: "retire", executed: 1, uniqueSubjects: 1, sample: []string{"backlog_item/inside"}}},
		},
		{
			name:     "a soak that never acted is zero evidence, not a clean run",
			events:   []*store.Event{ev("reconciler", "reconciler.requeue", "backlog_item", "Z", time.Hour)},
			prefix:   "overseer.",
			window:   24 * time.Hour,
			wantZero: true,
		},
		{
			name:         "subjectless events count as actions with no coverage",
			events:       []*store.Event{ev("overseer.foreman", "overseer.foreman.sweep", "", "", time.Hour)},
			prefix:       "overseer.",
			window:       24 * time.Hour,
			wantTotal:    1,
			wantExecuted: 1,
			wantActions:  []wantAction{{actor: "overseer.foreman", action: "sweep", executed: 1, sample: []string{}}},
		},
		{
			name:         "a kind missing its actor prefix is reported verbatim",
			events:       []*store.Event{ev("overseer.groomer", "legacy_retire", "backlog_item", "A", time.Hour)},
			prefix:       "overseer.",
			window:       24 * time.Hour,
			wantTotal:    1,
			wantExecuted: 1,
			wantActions:  []wantAction{{actor: "overseer.groomer", action: "legacy_retire", executed: 1, uniqueSubjects: 1, sample: []string{"backlog_item/A"}}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			since := reportNow.Add(-tc.window)
			rep, err := BuildPromotionReport(context.Background(), &fakeEvents{events: tc.events}, tc.prefix, since, reportNow)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if rep.ActorPrefix != tc.prefix || !rep.WindowStart.Equal(since) || !rep.WindowEnd.Equal(reportNow) {
				t.Fatalf("window = %s [%s..%s]", rep.ActorPrefix, rep.WindowStart, rep.WindowEnd)
			}
			if rep.TotalActions != tc.wantTotal || rep.TotalDryRun != tc.wantDryRun || rep.TotalExecuted != tc.wantExecuted {
				t.Fatalf("totals = %d (%d dry, %d executed), want %d (%d, %d)",
					rep.TotalActions, rep.TotalDryRun, rep.TotalExecuted, tc.wantTotal, tc.wantDryRun, tc.wantExecuted)
			}
			if rep.ZeroEvidence != tc.wantZero {
				t.Fatalf("zero_evidence = %v, want %v", rep.ZeroEvidence, tc.wantZero)
			}
			if tc.wantActorsSeq != nil {
				var got []string
				for _, a := range rep.PerActor {
					got = append(got, a.Actor)
				}
				if !reflect.DeepEqual(got, tc.wantActorsSeq) {
					t.Fatalf("actor order = %v, want %v", got, tc.wantActorsSeq)
				}
			}
			assertActions(t, rep, tc.wantActions)
		})
	}
}

// assertActions checks the flattened (actor, action) cells exhaustively: an
// unexpected extra row is a report defect too.
func assertActions(t *testing.T, rep PromotionReport, want []wantAction) {
	t.Helper()
	got := make(map[string]PromotionAction)
	for _, actor := range rep.PerActor {
		if len(actor.PerAction) == 0 {
			t.Fatalf("actor %s has no action rows", actor.Actor)
		}
		for _, a := range actor.PerAction {
			got[actor.Actor+"|"+a.Action] = a
		}
	}
	if len(got) != len(want) {
		t.Fatalf("action rows = %d, want %d (%+v)", len(got), len(want), rep.PerActor)
	}
	for _, w := range want {
		a, ok := got[w.actor+"|"+w.action]
		if !ok {
			t.Fatalf("missing row %s/%s in %+v", w.actor, w.action, rep.PerActor)
		}
		if a.DryRun != w.dryRun || a.Executed != w.executed {
			t.Errorf("%s/%s counts = %d dry / %d executed, want %d / %d", w.actor, w.action, a.DryRun, a.Executed, w.dryRun, w.executed)
		}
		if a.UniqueSubjects != w.uniqueSubjects {
			t.Errorf("%s/%s unique subjects = %d, want %d", w.actor, w.action, a.UniqueSubjects, w.uniqueSubjects)
		}
		if !reflect.DeepEqual(a.SubjectSample, w.sample) {
			t.Errorf("%s/%s sample = %v, want %v", w.actor, w.action, a.SubjectSample, w.sample)
		}
		if a.First.After(a.Last) {
			t.Errorf("%s/%s first %s after last %s", w.actor, w.action, a.First, a.Last)
		}
	}
}

// repeatedSubjects returns 15 committed actions across 12 subjects: s00..s11,
// with s00 acted on four times.
func repeatedSubjects() []*store.Event {
	var out []*store.Event
	for i := 0; i < 12; i++ {
		out = append(out, ev("overseer.groomer", "overseer.groomer.retire", "backlog_item", fmt.Sprintf("s%02d", i), time.Duration(i+1)*time.Minute))
	}
	for i := 0; i < 3; i++ {
		out = append(out, ev("overseer.groomer", "overseer.groomer.retire", "backlog_item", "s00", time.Duration(i+20)*time.Minute))
	}
	return out
}

func TestBuildPromotionReportFirstLast(t *testing.T) {
	events := &fakeEvents{events: []*store.Event{
		ev("overseer.groomer", "overseer.groomer.retire", "backlog_item", "A", 6*time.Hour),
		ev("overseer.groomer", "overseer.groomer.retire.dryrun", "backlog_item", "B", 2*time.Hour),
		ev("overseer.groomer", "overseer.groomer.retire", "backlog_item", "C", 4*time.Hour),
	}}
	rep, err := BuildPromotionReport(context.Background(), events, "overseer.", reportNow.Add(-24*time.Hour), reportNow)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	row := rep.PerActor[0].PerAction[0]
	if !row.First.Equal(reportNow.Add(-6 * time.Hour)) {
		t.Errorf("first = %s", row.First)
	}
	if !row.Last.Equal(reportNow.Add(-2 * time.Hour)) {
		t.Errorf("last = %s", row.Last)
	}
	// The read must be bounded and window-anchored.
	if !events.gotSince.Equal(reportNow.Add(-24*time.Hour)) || events.gotLimit != promotionReportEventLimit {
		t.Errorf("list since=%s limit=%d", events.gotSince, events.gotLimit)
	}
}

func TestBuildPromotionReportRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	since, now := reportNow.Add(-time.Hour), reportNow

	if _, err := BuildPromotionReport(ctx, nil, "overseer.", since, now); err == nil {
		t.Error("nil lister accepted")
	}
	if _, err := BuildPromotionReport(ctx, &fakeEvents{}, "", since, now); err == nil {
		t.Error("empty actor prefix accepted: the report would span every writer")
	}
	if _, err := BuildPromotionReport(ctx, &fakeEvents{}, "overseer.", now, since); err == nil {
		t.Error("inverted window accepted")
	}
	if _, err := BuildPromotionReport(ctx, &fakeEvents{}, "overseer.", now, now); err == nil {
		t.Error("empty window accepted")
	}
	if _, err := BuildPromotionReport(ctx, &fakeEvents{err: errors.New("boom")}, "overseer.", since, now); err == nil {
		t.Error("store read failure swallowed")
	}
}

// TestBuildPromotionReportRefusesTruncation proves a saturated window fails
// loudly: a review that silently under-counts executed actions would read as
// a safer soak than it was.
func TestBuildPromotionReportRefusesTruncation(t *testing.T) {
	events := &fakeEvents{}
	for i := 0; i < promotionReportEventLimit; i++ {
		events.events = append(events.events, ev("overseer.groomer", "overseer.groomer.retire", "backlog_item", fmt.Sprintf("s%d", i), time.Minute))
	}
	if _, err := BuildPromotionReport(context.Background(), events, "overseer.", reportNow.Add(-time.Hour), reportNow); err == nil {
		t.Fatal("saturated window reported instead of erroring")
	}
}

// TestBuildPromotionReportOverEventDAO runs the report over the real store so
// the kind/actor conventions the recorder writes are the ones it reads back.
func TestBuildPromotionReportOverEventDAO(t *testing.T) {
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "p.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	dry := true
	rec := &ActionRecorder{Events: st.Events, Actor: "overseer.promo", DryRun: func() bool { return dry }}
	if err := rec.Record(ctx, "retire", "backlog_item", "A", nil); err != nil {
		t.Fatalf("dry record: %v", err)
	}
	dry = false
	if err := rec.Record(ctx, "retire", "backlog_item", "B", nil); err != nil {
		t.Fatalf("record: %v", err)
	}

	now := time.Now().UTC().Add(time.Minute)
	rep, err := BuildPromotionReport(ctx, st.Events, "overseer.promo", now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if rep.ZeroEvidence || rep.TotalActions != 2 || rep.TotalDryRun != 1 || rep.TotalExecuted != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if len(rep.PerActor) != 1 || len(rep.PerActor[0].PerAction) != 1 {
		t.Fatalf("per-actor = %+v", rep.PerActor)
	}
	row := rep.PerActor[0].PerAction[0]
	if row.Action != "retire" || row.UniqueSubjects != 2 {
		t.Fatalf("action row = %+v", row)
	}
}
