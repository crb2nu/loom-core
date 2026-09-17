package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestOverseerSoakTelemetryCoveringRange(t *testing.T) {
	st := newTestStore(t)
	plan := strings.Join(queryPlan(t, st, "EXPLAIN QUERY PLAN "+overseerSoakTelemetryQuery,
		[]any{"2026-09-10", "2026-09-17"}), "\n")
	if !strings.Contains(plan, "USING COVERING INDEX idx_events_soak_daily") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("soak evidence must use a covering, sort-free day range:\n%s", plan)
	}
}

func TestOverseerSoakTelemetryWindowAndIncrements(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	// The UTC day boundary, not the caller's wall-clock date, defines the window.
	end := time.Date(2026, 9, 17, 23, 30, 0, 0, time.FixedZone("west", -2*3600))
	start := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	want := make([]OverseerSoakDailyCounters, 7)
	for i := 6; i >= 0; i-- {
		day := start.AddDate(0, 0, i)
		if err := st.RecordOverseerSoakDecision(ctx, day, true, false); err != nil {
			t.Fatal(err)
		}
		want[i] = OverseerSoakDailyCounters{Day: day, Decisions: 1, WouldHaveActed: 1}
	}
	if err := st.RecordOverseerSoakDecision(ctx, start.Add(time.Hour), false, true); err != nil {
		t.Fatal(err)
	}
	want[0].Decisions++
	want[0].Disagreements++
	for _, at := range []time.Time{start.Add(-time.Nanosecond), start.AddDate(0, 0, 7)} {
		if err := st.RecordOverseerSoakDecision(ctx, at, true, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct{ kind, subjectKind, day, payload string }{
		{"other.kind", "utc_day", "2026-09-12", "invalid"},
		{overseerSoakTelemetryEventKind, "other.subject", "2026-09-12", "invalid"},
		{overseerSoakTelemetryEventKind, "utc_day", "2026-09-10", "invalid"},
		// A late historical insert still contributes by its declared UTC day.
		{overseerSoakTelemetryEventKind, "utc_day", "2026-09-13", `{"decisions":1}`},
	} {
		if _, err := st.DB().ExecContext(ctx, `INSERT INTO events(occurred_at,actor,kind,subject_kind,subject_id,payload_json) VALUES ('2000-01-01T00:00:00Z','test',?,?,?,?)`, row.kind, row.subjectKind, row.day, row.payload); err != nil {
			t.Fatal(err)
		}
	}
	want[2].Decisions++
	got, err := st.OverseerSoakTelemetry(ctx, end)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("daily increments = %+v, err=%v; want %+v", got, err, want)
	}
}

func TestOverseerSoakTelemetryRejectsMalformedEvidence(t *testing.T) {
	for _, tc := range []struct{ name, day, payload string }{
		{"invalid day inside lexical range", "2026-09-12-bad", `{"decisions":1}`},
		{"invalid JSON", "2026-09-12", `{`},
		{"missing denominator", "2026-09-12", `{}`},
		{"not one decision", "2026-09-12", `{"decisions":2}`},
		{"fractional counter", "2026-09-12", `{"decisions":1,"would_have_acted":0.5}`},
		{"string counter", "2026-09-12", `{"decisions":"1"}`},
		{"negative action", "2026-09-12", `{"decisions":1,"would_have_acted":-1}`},
		{"excess action", "2026-09-12", `{"decisions":1,"would_have_acted":2}`},
		{"negative disagreement", "2026-09-12", `{"decisions":1,"policy_disagreements":-1}`},
		{"excess disagreement", "2026-09-12", `{"decisions":1,"policy_disagreements":2}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			ctx := context.Background()
			if _, err := st.DB().ExecContext(ctx, `INSERT INTO events(occurred_at,actor,kind,subject_kind,subject_id,payload_json) VALUES ('2026-09-12T00:00:00Z','overseer',?,'utc_day',?,?)`, overseerSoakTelemetryEventKind, tc.day, tc.payload); err != nil {
				t.Fatal(err)
			}
			got, err := st.OverseerSoakTelemetry(ctx, time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC))
			if err == nil || got != nil {
				t.Fatalf("malformed evidence returned usable counters: %+v, err=%v", got, err)
			}
		})
	}
}

func TestOverseerSoakTelemetryDoesNotTruncate(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	end := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	// More than the recent-action list's cap: every increment must be counted,
	// and malformed evidence at the end must not be hidden by a sampling limit.
	const count = 2001
	if _, err := st.DB().ExecContext(ctx, `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<?)
		INSERT INTO events(occurred_at,actor,kind,subject_kind,subject_id,payload_json)
		SELECT '2026-09-12T00:00:00Z','overseer',?,'utc_day','2026-09-12','{"decisions":1}' FROM n`, count, overseerSoakTelemetryEventKind); err != nil {
		t.Fatal(err)
	}
	got, err := st.OverseerSoakTelemetry(ctx, end)
	if err != nil || len(got) != 1 || got[0].Decisions != count {
		t.Fatalf("lost increments or invented missing days: %+v, err=%v", got, err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE events SET payload_json='{"decisions":0}' WHERE id=(SELECT MAX(id) FROM events)`); err != nil {
		t.Fatal(err)
	}
	if got, err = st.OverseerSoakTelemetry(ctx, end); err == nil || got != nil {
		t.Fatalf("ignored malformed tail: %+v, err=%v", got, err)
	}
}

func TestOverseerSoakTelemetryCanceled(t *testing.T) {
	st := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := st.OverseerSoakTelemetry(ctx, time.Now()); !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("canceled evidence read: %+v, err=%v", got, err)
	}
}
