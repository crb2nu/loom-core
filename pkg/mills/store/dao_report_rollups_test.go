package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestReportRollupLatestAndFreshness(t *testing.T) {
	st, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "mills.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	newer := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	for _, s := range []*ReportRollupSnapshot{
		{ReportName: "promotion", WindowSeconds: 3600, ReportKey: "overseer.", SnapshotAt: newer, Payload: json.RawMessage(`{"value":"new"}`)},
		{ReportName: "promotion", WindowSeconds: 3600, ReportKey: "overseer.", SnapshotAt: newer.Add(-time.Hour), Payload: json.RawMessage(`{"value":"old"}`)},
	} {
		if err := st.Reports.Put(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.Reports.Latest(ctx, "promotion", 3600, "overseer.")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Payload) != `{"value":"new"}` || !got.SnapshotAt.Equal(newer) {
		t.Fatalf("got %+v", got)
	}
	if _, err := st.Reports.Latest(ctx, "promotion", 7200, "overseer."); err != ErrNotFound {
		t.Fatalf("missing err = %v", err)
	}
}

func TestReportRollupLatestQueryDoesNotReferenceEvents(t *testing.T) {
	st, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "mills.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := st.db.QueryContext(context.Background(), `EXPLAIN QUERY PLAN SELECT payload_json FROM report_rollup_snapshots WHERE report_name=? AND window_seconds=? AND report_key=?`, "x", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if detail == "SCAN events" {
			t.Fatal(detail)
		}
	}
}

// BenchmarkReportRollupLatest measures the cold-connection request path:
// one indexed point lookup and JSON payload read, with no events scan. It
// deliberately avoids BOTH fleet-gate conventions: it is not in
// fleet_reliability_benchmark_test.go (the gate compiles that file in
// LOCKSTEP against a pre-branch baseline tree where the Reports API does
// not exist) and it must not carry the reserved BenchmarkFleet* name prefix
// (the gate fails closed on any BenchmarkFleet* result missing from its
// manifest). Promote it into the lockstep suite once the rollup store is
// part of the baseline: rename to BenchmarkFleet*, move it there, and add
// it to scripts/ci/fleet_reliability_suite_v1.json.
func BenchmarkReportRollupLatest(b *testing.B) {
	ctx := context.Background()
	dbPath := filepath.Join(b.TempDir(), "mills.db")
	seed, err := Open(ctx, Options{Path: dbPath})
	if err != nil {
		b.Fatal(err)
	}
	if err := seed.Reports.Put(ctx, &ReportRollupSnapshot{ReportName: "judge_calibration", WindowSeconds: 336 * 3600, Payload: json.RawMessage(`{"total_verdicts":100}`)}); err != nil {
		b.Fatal(err)
	}
	_ = seed.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := Open(ctx, Options{Path: dbPath, SkipMigrations: true})
		if err != nil {
			b.Fatal(err)
		}
		if _, err = st.Reports.Latest(ctx, "judge_calibration", 336*3600, ""); err != nil {
			_ = st.Close()
			b.Fatal(err)
		}
		if err = st.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
