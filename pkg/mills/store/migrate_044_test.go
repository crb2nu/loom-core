package store

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestMigration044PreservesEventsAndActorOrdering(t *testing.T) {
	db := openSchemaAt(t, 43)
	ctx := context.Background()
	dao := &EventDAO{db: db}
	at := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		e := &Event{
			Actor: "overseer.groomer", OccurredAt: at,
			Kind: fmt.Sprintf("kind-%d", 7-i), SubjectKind: "backlog_item",
			SubjectID: fmt.Sprintf("subject-%d", i%3),
			Payload:   map[string]any{"historical": fmt.Sprint(i)},
		}
		if i == 0 {
			e.SubjectKind, e.SubjectID = "", ""
		}
		if err := dao.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	before, err := dao.ListByActorSince(ctx, "overseer.groomer", at, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i, event := range before {
		if event.ID != int64(8-i) {
			t.Fatalf("baseline timestamp tie order: %+v", before)
		}
	}
	for range 2 {
		if err := Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	after, err := dao.ListByActorSince(ctx, "overseer.groomer", at, 100)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("migration changed historical events or tie order: before=%+v after=%+v err=%v", before, after, err)
	}
	var nullSubjects int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE subject_kind IS NULL AND subject_id IS NULL`).Scan(&nullSubjects); err != nil || nullSubjects != 1 {
		t.Fatalf("NULL subject preservation: count=%d err=%v", nullSubjects, err)
	}

	var path string
	if err := db.QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	after, err = reopened.Events.ListByActorSince(ctx, "overseer.groomer", at, 100)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("reopen changed historical events: after=%+v err=%v", after, err)
	}
	var visited int
	if err := reopened.Events.WalkActorWindow(ctx, "overseer.", at, at.Add(time.Second), func(e *Event) error {
		visited++
		if e.Payload != nil {
			t.Fatal("metadata scan decoded payload")
		}
		return nil
	}); err != nil || visited != len(before) {
		t.Fatalf("metadata count after upgrade: count=%d err=%v", visited, err)
	}
}

func TestMigration044FailureRestoresPriorIndex(t *testing.T) {
	db := openSchemaAt(t, 43)
	ctx := context.Background()
	indexSQL := func() string {
		t.Helper()
		var definition string
		if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE name='idx_events_actor_occurred'`).Scan(&definition); err != nil {
			t.Fatal(err)
		}
		return definition
	}
	before := indexSQL()
	broken := migrationByVersion(t, 44)
	// Fail after both DROP and CREATE executed: neither the new index nor the
	// migration version may survive the transaction's rollback.
	broken.sql += `; SELECT missing_migration_probe_column FROM events;`
	if err := applyOne(ctx, db, broken); err == nil {
		t.Fatal("injected migration failure unexpectedly succeeded")
	}
	if got := indexSQL(); got != before {
		t.Fatalf("failed replacement lost old index: got %s want %s", got, before)
	}
	var recorded int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version=44`).Scan(&recorded); err != nil || recorded != 0 {
		t.Fatalf("failed migration recorded: count=%d err=%v", recorded, err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("retry migration: %v", err)
	}
	if indexSQL() == before {
		t.Fatal("retry did not replace index")
	}
	// Confirm the retried schema also supports new appends.
	if _, err := db.ExecContext(ctx, `INSERT INTO events (actor,kind,occurred_at) VALUES ('test','after-retry',?)`, timeRFC3339(time.Now())); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("event append after retry: count=%d", count)
	}
}
