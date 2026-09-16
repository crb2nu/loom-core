package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// fixedWidthTimestampMigration is the version that rewrote trimmed
// RFC3339Nano timestamps to timeLayout.
const fixedWidthTimestampMigration = 42

// timestampColumnExtras are timestamp TEXT columns whose names do not end in
// _at but are written with timeRFC3339 all the same.
var timestampColumnExtras = map[string]bool{
	"escalation_sweep_state.recheck_after": true,
	"policy_proposals.revert_deadline":     true,
}

// openSchemaAt opens a fresh file-backed database with migrations applied up
// to and including version, bypassing Open so the schema is frozen there.
func openSchemaAt(t *testing.T, version int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", buildDSN(filepath.Join(t.TempDir(), "mills.db")))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrateUpTo(context.Background(), db, version); err != nil {
		t.Fatalf("migrate up to %d: %v", version, err)
	}
	return db
}

func migrationByVersion(t *testing.T, version int) migration {
	t.Helper()
	all, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	for _, m := range all {
		if m.version == version {
			return m
		}
	}
	t.Fatalf("migration %d not found", version)
	return migration{}
}

// timestampColumns lists every "table.column" TEXT column in db that holds a
// timeRFC3339 value: names ending in _at plus timestampColumnExtras.
func timestampColumns(t *testing.T, db *sql.DB) []string {
	t.Helper()
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		tables = append(tables, name)
	}
	_ = rows.Close()
	var out []string
	for _, table := range tables {
		cols, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%q)`, table))
		if err != nil {
			t.Fatalf("table_info %s: %v", table, err)
		}
		for cols.Next() {
			var (
				cid, notNull, pk int
				name, ctype      string
				dflt             any
			)
			if err := cols.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
				t.Fatalf("scan column: %v", err)
			}
			key := table + "." + name
			if strings.EqualFold(ctype, "TEXT") && (strings.HasSuffix(name, "_at") || timestampColumnExtras[key]) {
				out = append(out, key)
			}
		}
		_ = cols.Close()
	}
	sort.Strings(out)
	return out
}

// TestMigration042CoversEveryTimestampColumn checks the generated UPDATE list
// against the schema as it stood at version 42, so a column can be neither
// forgotten nor invented. Later migrations add columns that are written fixed
// width from the start and need no rewrite, which is why the schema is frozen
// at 42 rather than read from a fully migrated store.
func TestMigration042CoversEveryTimestampColumn(t *testing.T) {
	want := timestampColumns(t, openSchemaAt(t, fixedWidthTimestampMigration))

	re := regexp.MustCompile(`(?m)^UPDATE (\w+)\nSET (\w+) = `)
	var got []string
	for _, m := range re.FindAllStringSubmatch(migrationByVersion(t, fixedWidthTimestampMigration).sql, -1) {
		got = append(got, m[1]+"."+m[2])
	}
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("migration 042 column list drifted from the schema\n got: %v\nwant: %v", got, want)
	}
	if len(got) < 60 {
		t.Fatalf("only %d timestamp columns discovered; introspection is broken", len(got))
	}
}

// TestMigration042PadsTrimmedTimestamps seeds rows through the DAOs, rewrites
// their timestamps into the trimmed form pre-042 stores hold, shows the
// misordering, then re-applies the (idempotent) migration body and checks the
// values and the order come back right.
func TestMigration042PadsTrimmedTimestamps(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	db := st.DB()

	if err := st.Backlog.Put(ctx, &BacklogItem{ID: "BL-042", Title: "t", State: BacklogRunning, Priority: P2, CreatedBy: "test"}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	base := time.Date(2026, 9, 15, 23, 2, 26, 0, time.UTC)
	if err := st.Pipeline.PutRun(ctx, &PipelineRun{ID: "RUN-042", BacklogID: "BL-042", Template: "t", State: PipelineTesting, Attempts: 1, StartedAt: base}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	// Attempt 3 carries the fraction RFC3339Nano trims (.848100 → ".8481").
	for attempt, ns := range map[int]int{1: 848150000, 2: 848191000, 3: 848100000} {
		if err := st.Pipeline.PutStage(ctx, &StageResult{PipelineRunID: "RUN-042", Stage: "tests", Attempt: attempt, StartedAt: base.Add(time.Duration(ns))}); err != nil {
			t.Fatalf("seed stage %d: %v", attempt, err)
		}
	}

	// Simulate rows written before 042 (time.RFC3339Nano output), plus a
	// no-fraction legacy value, an already fixed-width value, and text that is
	// not a timestamp at all — the last two must survive untouched.
	trimmed := func(ns int) string { return base.Add(time.Duration(ns)).Format(time.RFC3339Nano) }
	for attempt, ns := range map[int]int{1: 848150000, 2: 848191000, 3: 848100000} {
		if _, err := db.ExecContext(ctx, `UPDATE stage_results SET started_at = ? WHERE pipeline_run_id = 'RUN-042' AND attempt = ?`, trimmed(ns), attempt); err != nil {
			t.Fatalf("trim stage %d: %v", attempt, err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE pipeline_runs SET started_at = ? WHERE id = 'RUN-042'`, base.Format(time.RFC3339)); err != nil {
		t.Fatalf("trim run: %v", err)
	}
	if err := st.Events.Append(ctx, &Event{Actor: "test", Kind: "migration.probe", SubjectKind: "run", SubjectID: "RUN-042", OccurredAt: base.Add(848100000)}); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE events SET occurred_at = 'not-a-timestamp' WHERE kind = 'migration.probe'`); err != nil {
		t.Fatalf("poison event: %v", err)
	}

	var before []int
	stages, err := st.Pipeline.ListStages(ctx, "RUN-042")
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	for _, s := range stages {
		before = append(before, s.Attempt)
	}
	if fmt.Sprint(before) != "[1 2 3]" {
		t.Fatalf("premise: trimmed rows should list as [1 2 3] (attempt 3 last despite being earliest), got %v", before)
	}

	// Re-apply the migration body. It must be idempotent, so running it a
	// second time on a store that already applied it is legitimate.
	if _, err := db.ExecContext(ctx, migrationByVersion(t, fixedWidthTimestampMigration).sql); err != nil {
		t.Fatalf("re-apply migration 042: %v", err)
	}

	wantStarted := map[int]string{
		1: "2026-09-15T23:02:26.848150000Z",
		2: "2026-09-15T23:02:26.848191000Z",
		3: "2026-09-15T23:02:26.848100000Z",
	}
	for attempt, want := range wantStarted {
		var got string
		if err := db.QueryRowContext(ctx, `SELECT started_at FROM stage_results WHERE pipeline_run_id = 'RUN-042' AND attempt = ?`, attempt).Scan(&got); err != nil {
			t.Fatalf("read stage %d: %v", attempt, err)
		}
		if got != want {
			t.Errorf("stage %d started_at = %q, want %q", attempt, got, want)
		}
	}
	var runStarted string
	if err := db.QueryRowContext(ctx, `SELECT started_at FROM pipeline_runs WHERE id = 'RUN-042'`).Scan(&runStarted); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if runStarted != "2026-09-15T23:02:26.000000000Z" {
		t.Errorf("no-fraction run started_at = %q, want padded whole second", runStarted)
	}
	var poisoned string
	if err := db.QueryRowContext(ctx, `SELECT occurred_at FROM events WHERE kind = 'migration.probe'`).Scan(&poisoned); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if poisoned != "not-a-timestamp" {
		t.Errorf("non-timestamp text rewritten to %q", poisoned)
	}
	var endedNull int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM stage_results WHERE pipeline_run_id = 'RUN-042' AND ended_at IS NULL`).Scan(&endedNull); err != nil {
		t.Fatalf("count null ended_at: %v", err)
	}
	if endedNull != 3 {
		t.Errorf("NULL ended_at rows after migration = %d, want 3", endedNull)
	}

	stages, err = st.Pipeline.ListStages(ctx, "RUN-042")
	if err != nil {
		t.Fatalf("list stages after: %v", err)
	}
	var after []int
	for _, s := range stages {
		after = append(after, s.Attempt)
	}
	if fmt.Sprint(after) != "[3 1 2]" {
		t.Fatalf("stages after migration = %v, want [3 1 2] (chronological)", after)
	}
}
