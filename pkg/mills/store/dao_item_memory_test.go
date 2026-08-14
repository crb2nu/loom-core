package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/journalengine"
)

func seedItemMemoryItem(t *testing.T, st *Store, id string) string {
	t.Helper()
	item := &BacklogItem{
		ID:       id,
		Title:    "item memory fixture",
		State:    BacklogRunning,
		Priority: P2,
	}
	if err := st.Backlog.Put(context.Background(), item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	return item.ID
}

func TestMigrate_017_BacklogItemMemoryTableExists(t *testing.T) {
	st := newTestStore(t)
	var name string
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "backlog_item_memory",
	).Scan(&name); err != nil {
		t.Fatalf("backlog_item_memory missing: %v", err)
	}
	for _, col := range []string{"backlog_id", "snapshot_json", "updated_at"} {
		var got string
		if err := st.DB().QueryRowContext(context.Background(),
			`SELECT name FROM pragma_table_info('backlog_item_memory') WHERE name=?`, col,
		).Scan(&got); err != nil {
			t.Errorf("backlog_item_memory.%s missing: %v", col, err)
		}
	}
}

func TestItemMemory_GetAbsentReturnsFreshJournal(t *testing.T) {
	st := newTestStore(t)
	id := seedItemMemoryItem(t, st, "MILLS-ITEM-MEMORY-ABSENT")

	j, err := st.ItemMemory.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get absent: %v", err)
	}
	if j == nil {
		t.Fatal("get absent returned a nil journal")
	}
	if j.Owner() != id {
		t.Errorf("owner = %q, want %q", j.Owner(), id)
	}
	if got := j.Render(); got != journalengine.EmptyJournal {
		t.Errorf("fresh journal render = %q, want the empty marker", got)
	}
}

// A round trip through SQLite must reproduce the render byte for byte: a
// restored journal that renders differently would break the prefix contract at
// every operator restart, which is exactly the silent-and-expensive failure the
// package warns about.
func TestItemMemory_RoundTripPreservesRenderBytes(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	id := seedItemMemoryItem(t, st, "MILLS-ITEM-MEMORY-ROUNDTRIP")

	original := journalengine.New(id, nil)
	original.RecordTurn(0, "Pipeline stage \"research\" ran (attempt 1).", nil, "Outcome: succeeded.\nLog tail:\nfound prior art in pkg/mills/pipeline")
	original.RecordTurn(1, "Pipeline stage \"plan_slice\" ran (attempt 1).", nil, "Outcome: succeeded.\nDiff: 0 file(s), +0/-0")
	original.RecordTurn(2, "Pipeline stage \"implement\" ran (attempt 2).", nil, "Outcome: FAILED — scope gate\nDiff: 2 file(s), +40/-3 — a.go, b.go")

	if err := st.ItemMemory.Put(ctx, id, original); err != nil {
		t.Fatalf("put: %v", err)
	}
	restored, err := st.ItemMemory.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got, want := restored.Render(), original.Render(); got != want {
		t.Fatalf("restored render differs:\n got: %q\nwant: %q", got, want)
	}
	if got, want := len(restored.Entries()), len(original.Entries()); got != want {
		t.Errorf("restored entries = %d, want %d", got, want)
	}

	// Put is an upsert: a second write for the same item replaces the row.
	restored.RecordTurn(3, "Pipeline stage \"tests\" ran (attempt 1).", nil, "Outcome: succeeded.")
	if err := st.ItemMemory.Put(ctx, id, restored); err != nil {
		t.Fatalf("re-put: %v", err)
	}
	var rows int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM backlog_item_memory WHERE backlog_id = ?`, id).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("rows = %d, want 1 (upsert, not append)", rows)
	}
	again, err := st.ItemMemory.Get(ctx, id)
	if err != nil {
		t.Fatalf("get after re-put: %v", err)
	}
	if err := journalengine.CheckPrefixExtension(original.Render(), again.Render()); err != nil {
		t.Errorf("persisted growth broke the prefix contract: %v", err)
	}
}

func TestItemMemory_PutRefusesOversizedSnapshot(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	id := seedItemMemoryItem(t, st, "MILLS-ITEM-MEMORY-OVERSIZE")

	j := journalengine.New(id, nil)
	// One entry past the cap is enough; the guard is on the marshalled payload.
	j.RecordTurn(0, "stage", nil, strings.Repeat("x", ItemMemoryMaxSnapshotBytes+1))

	err := st.ItemMemory.Put(ctx, id, j)
	if !errors.Is(err, ErrItemMemoryTooLarge) {
		t.Fatalf("put oversized = %v, want ErrItemMemoryTooLarge", err)
	}
	var rows int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM backlog_item_memory WHERE backlog_id = ?`, id).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 0 {
		t.Errorf("rows = %d, want 0 (an over-cap record must not persist)", rows)
	}
}

func TestItemMemory_Delete(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	id := seedItemMemoryItem(t, st, "MILLS-ITEM-MEMORY-DELETE")

	j := journalengine.New(id, nil)
	j.RecordTurn(0, "stage", nil, "did a thing")
	if err := st.ItemMemory.Put(ctx, id, j); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := st.ItemMemory.Delete(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := st.ItemMemory.Get(ctx, id)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if got.Render() != journalengine.EmptyJournal {
		t.Errorf("render after delete = %q, want the empty marker", got.Render())
	}
}
