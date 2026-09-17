package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestMigration045PreservesEvidenceAndReopens(t *testing.T) {
	db := openSchemaAt(t, 44)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO events(occurred_at,actor,kind,subject_kind,subject_id,payload_json) VALUES
		('2026-09-12T00:00:00Z','overseer','overseer.soak.daily','utc_day','2026-09-12','{"decisions":1}'),
		('2026-09-12T00:00:01Z','overseer','overseer.soak.daily','utc_day','2026-09-12-bad','invalid'),
		('2026-09-12T00:00:02Z','other','other.kind','utc_day','2026-09-12','unrelated'),
		('2026-09-12T00:00:03Z','other','overseer.soak.daily',NULL,NULL,'{}')`); err != nil {
		t.Fatal(err)
	}
	snapshot := func(db *sql.DB) string {
		t.Helper()
		var value string
		if err := db.QueryRowContext(ctx, `SELECT json_group_array(json_array(id,occurred_at,actor,kind,subject_kind,subject_id,payload_json)) FROM (SELECT * FROM events ORDER BY id)`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := snapshot(db)
	for range 2 {
		if err := Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	var path string
	if err := db.QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if after := snapshot(st.DB()); after != before {
		t.Fatalf("migration or reopen changed event evidence:\nbefore=%s\nafter=%s", before, after)
	}
	var count int
	if err := st.DB().QueryRowContext(ctx, `SELECT count(*) FROM events INDEXED BY idx_events_soak_daily WHERE kind='overseer.soak.daily' AND subject_kind='utc_day'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("partial index must retain valid and malformed soak rows only: count=%d err=%v", count, err)
	}
	if got, err := st.OverseerSoakTelemetry(ctx, time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)); err == nil || got != nil {
		t.Fatalf("migration hid malformed historical evidence: %+v err=%v", got, err)
	}
}

func TestMigration045FailureRollsBackAndRetries(t *testing.T) {
	db := openSchemaAt(t, 44)
	ctx := context.Background()
	broken := migrationByVersion(t, 45)
	broken.sql += `; SELECT missing_migration_probe_column FROM events;`
	if err := applyOne(ctx, db, broken); err == nil {
		t.Fatal("injected migration failure unexpectedly succeeded")
	}
	for _, query := range []string{
		`SELECT count(*) FROM sqlite_master WHERE name='idx_events_soak_daily'`,
		`SELECT count(*) FROM schema_migrations WHERE version=45`,
	} {
		var count int
		if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed migration left state behind: count=%d err=%v", count, err)
		}
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	st := &Store{db: db}
	at := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	if err := st.RecordOverseerSoakDecision(ctx, at, true, false); err != nil {
		t.Fatal(err)
	}
	got, err := st.OverseerSoakTelemetry(ctx, at.AddDate(0, 0, 1))
	if err != nil || len(got) != 1 || got[0].Decisions != 1 || got[0].WouldHaveActed != 1 {
		t.Fatalf("post-migration append missing from index: %+v err=%v", got, err)
	}
}
