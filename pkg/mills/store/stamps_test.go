package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestStampRoundTripRequiresTargetProject(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	wantTime := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	want := &Stamp{ID: "stamp-widget", TargetProject: " services/widgets ", CreatedAt: wantTime}
	if err := st.Stamps.Put(ctx, want); err != nil {
		t.Fatalf("put stamp: %v", err)
	}
	got, err := st.Stamps.Get(ctx, want.ID)
	if err != nil {
		t.Fatalf("get stamp: %v", err)
	}
	if got.TargetProject != "services/widgets" {
		t.Fatalf("target_project = %q, want services/widgets", got.TargetProject)
	}
	if !got.CreatedAt.Equal(wantTime) {
		t.Fatalf("created_at = %v, want %v", got.CreatedAt, wantTime)
	}
}

func TestStampSchemaCreatedByMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mills.db")
	st, err := Open(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	defer st.Close()

	var notNull int
	rows, err := st.DB().QueryContext(context.Background(), `PRAGMA table_info(cross_repo_stamps)`)
	if err != nil {
		t.Fatalf("table info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan table info: %v", err)
		}
		if name == "target_project" {
			if notNull != 1 {
				t.Fatal("target_project is nullable")
			}
			return
		}
	}
	t.Fatal("target_project column missing")
}

func TestStampMigrationAppliesToExistingStore(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx, `
		DROP TABLE cross_repo_stamps;
		DELETE FROM schema_migrations WHERE version = 21;
	`); err != nil {
		t.Fatalf("prepare pre-stamp schema: %v", err)
	}
	if err := Migrate(ctx, st.DB()); err != nil {
		t.Fatalf("migrate existing store: %v", err)
	}
	if err := st.Stamps.Put(ctx, &Stamp{ID: "migrated", TargetProject: "services/widgets"}); err != nil {
		t.Fatalf("write after migration: %v", err)
	}
}

func TestStampDAODoesNotWriteBlankTarget(t *testing.T) {
	st := newTestStore(t)
	for _, target := range []string{"", " ", "\t\n"} {
		if err := st.Stamps.Put(context.Background(), &Stamp{ID: "blank", TargetProject: target}); err == nil {
			t.Fatalf("Put target %q succeeded", target)
		}
	}
	var count int
	if err := st.DB().QueryRowContext(context.Background(), `SELECT count(*) FROM cross_repo_stamps`).Scan(&count); err != nil {
		t.Fatalf("count stamps: %v", err)
	}
	if count != 0 {
		t.Fatalf("stamp rows = %d, want 0", count)
	}
}

func TestStampDAODoesNotWriteBlankID(t *testing.T) {
	st := newTestStore(t)
	for _, id := range []string{"", " ", "\t\n"} {
		if err := st.Stamps.Put(context.Background(), &Stamp{ID: id, TargetProject: "services/widgets"}); err == nil {
			t.Fatalf("Put ID %q succeeded", id)
		}
	}
	var count int
	if err := st.DB().QueryRowContext(context.Background(), `SELECT count(*) FROM cross_repo_stamps`).Scan(&count); err != nil {
		t.Fatalf("count stamps: %v", err)
	}
	if count != 0 {
		t.Fatalf("stamp rows = %d, want 0", count)
	}
}

func TestStampDAORejectsCorruptTargetlessRow(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	// Simulate a corrupt or externally restored database. Normal writes cannot
	// create this row because both StampDAO and migration 021 fail closed.
	if _, err := st.DB().ExecContext(ctx, `PRAGMA ignore_check_constraints = ON`); err != nil {
		t.Fatalf("disable check constraints: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
		INSERT INTO cross_repo_stamps (id, target_project, created_at)
		VALUES ('corrupt', '', '2026-08-07T12:00:00Z')
	`); err != nil {
		t.Fatalf("insert corrupt stamp: %v", err)
	}
	if _, err := st.Stamps.Get(ctx, "corrupt"); err == nil {
		t.Fatal("Get corrupt target-less stamp succeeded")
	}
}
