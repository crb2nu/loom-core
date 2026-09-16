package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestEventAppend_DedicatedWriterSurvivesReadPoolSaturation pins the fix for
// the 2026-09-07/08 operator boot bursts: every boot logged 16×
// "append event failed: event append: context deadline exceeded" for
// reconciler.auto_requeue_failed plus one for auto_requeue_sweep, because the
// cold-cache report rollups (overseers alone 16s) and the boot ticks were
// holding every pooled connection and a ledger INSERT queued in database/sql
// behind them until its context expired. The ledger now writes through its own
// single-connection handle, so a saturated read pool cannot starve it.
//
// The saturation here is the real mechanism, not a sleep: every read-pool
// connection is parked inside an open read transaction (the shape of a long
// window scan), which is exactly what a slow concurrent read does to the pool.
// The control assertion proves the pool really is exhausted before the append
// is timed, so a regression that quietly routes appends back through the pool
// fails the test rather than passing by luck on a fast machine.
func TestEventAppend_DedicatedWriterSurvivesReadPoolSaturation(t *testing.T) {
	st, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "mills.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	held := make([]*sql.Tx, 0, readPoolMaxOpenConns)
	t.Cleanup(func() {
		for _, tx := range held {
			_ = tx.Rollback()
		}
	})
	for i := 0; i < readPoolMaxOpenConns; i++ {
		tx, err := st.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("hold read conn %d: %v", i, err)
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
			t.Fatalf("read inside held tx %d: %v", i, err)
		}
		held = append(held, tx)
	}

	// Control: the shared pool is exhausted — a plain statement cannot even
	// acquire a connection inside a generous deadline.
	ctrlCtx, ctrlCancel := context.WithTimeout(ctx, 300*time.Millisecond)
	_, ctrlErr := st.db.ExecContext(ctrlCtx, `SELECT 1`)
	ctrlCancel()
	if !errors.Is(ctrlErr, context.DeadlineExceeded) {
		t.Fatalf("read pool not saturated: exec err = %v, want context deadline exceeded", ctrlErr)
	}

	// The ledger append rides its own connection and lands well inside its
	// own budget while every read connection is still held.
	appendCtx, appendCancel := context.WithTimeout(ctx, 2*time.Second)
	defer appendCancel()
	started := time.Now()
	ev := &Event{Actor: "test", Kind: "test.append_under_read_saturation", Payload: map[string]any{"ok": true}}
	if err := st.Events.Append(appendCtx, ev); err != nil {
		t.Fatalf("Append starved by read-pool saturation: %v", err)
	}
	if ev.ID == 0 {
		t.Fatal("Append did not assign an id")
	}
	inserted, err := st.Events.AppendOnceBySubjectKind(appendCtx, &Event{
		Actor: "test", Kind: "test.append_once_under_read_saturation",
		SubjectKind: "probe", SubjectID: "one",
	})
	if err != nil {
		t.Fatalf("AppendOnceBySubjectKind starved by read-pool saturation: %v", err)
	}
	if !inserted {
		t.Fatal("AppendOnceBySubjectKind did not insert a fresh subject")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("ledger appends took %s under read saturation; want well under the 2s budget", elapsed)
	}

	// Once the readers release their connections the row is visible to the
	// shared pool: same file, same WAL.
	for _, tx := range held {
		_ = tx.Rollback()
	}
	held = held[:0]
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE kind = ?`, ev.Kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("appended rows visible via read pool = %d, want 1", n)
	}
}

// TestEventDAO_NilWriterFallsBackToPool keeps direct EventDAO{db: …}
// constructions (tests, migrations tooling) working without a writer handle.
func TestEventDAO_NilWriterFallsBackToPool(t *testing.T) {
	st, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "mills.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dao := &EventDAO{db: st.db}
	if got := dao.appendExecer(); got != contextExecer(st.db) {
		t.Fatalf("nil writer should fall back to the pool handle")
	}
	if err := dao.Append(context.Background(), &Event{Actor: "test", Kind: "test.fallback"}); err != nil {
		t.Fatalf("fallback append: %v", err)
	}
	if got := st.Events.appendExecer(); got != contextExecer(st.writer) {
		t.Fatalf("store-opened DAO should append through the dedicated writer")
	}
}
