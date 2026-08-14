package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/journalengine"
)

func TestMigrate_018_CouncilMemoryTableExists(t *testing.T) {
	st := newTestStore(t)
	var name string
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "council_memory",
	).Scan(&name); err != nil {
		t.Fatalf("council_memory missing: %v", err)
	}
	for _, col := range []string{"lane", "snapshot_json", "updated_at"} {
		var got string
		if err := st.DB().QueryRowContext(context.Background(),
			`SELECT name FROM pragma_table_info('council_memory') WHERE name=?`, col,
		).Scan(&got); err != nil {
			t.Errorf("council_memory.%s missing: %v", col, err)
		}
	}
}

func TestCouncilMemory_GetAbsentReturnsFreshJournal(t *testing.T) {
	st := newTestStore(t)

	j, err := st.CouncilMemory.Get(context.Background())
	if err != nil {
		t.Fatalf("get absent: %v", err)
	}
	if j == nil {
		t.Fatal("get absent returned a nil journal")
	}
	if j.Owner() != CouncilMemoryLane {
		t.Errorf("owner = %q, want %q", j.Owner(), CouncilMemoryLane)
	}
	if got := j.Render(); got != journalengine.EmptyJournal {
		t.Errorf("fresh journal render = %q, want the empty marker", got)
	}
}

// The render bytes are the product: a round trip that changes them by one byte
// would cold-start the prefix cache on every operator restart.
func TestCouncilMemory_RoundTripPreservesRenderBytes(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	original := journalengine.New(CouncilMemoryLane, nil)
	original.RecordTurn(0, "Council run 1 completed.", nil, "Minted backlog items: none.")
	original.RecordTurn(2, "Council run 2 completed.", nil, "Minted backlog items:\n  - MILLS-1: do the thing")
	want := original.Render()

	if err := st.CouncilMemory.Put(ctx, original); err != nil {
		t.Fatalf("put: %v", err)
	}
	restored, err := st.CouncilMemory.Get(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := restored.Render(); got != want {
		t.Errorf("render drifted across the round trip:\nwant %q\ngot  %q", want, got)
	}
	if restored.Owner() != CouncilMemoryLane {
		t.Errorf("restored owner = %q, want %q", restored.Owner(), CouncilMemoryLane)
	}

	// Upsert: a second Put over the same lane must overwrite, not duplicate.
	restored.RecordTurn(4, "Council run 3 completed.", nil, "Minted backlog items: none.")
	if err := st.CouncilMemory.Put(ctx, restored); err != nil {
		t.Fatalf("re-put: %v", err)
	}
	var rows int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM council_memory`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("council_memory has %d rows, want exactly 1", rows)
	}
	again, err := st.CouncilMemory.Get(ctx)
	if err != nil {
		t.Fatalf("get after re-put: %v", err)
	}
	if err := journalengine.CheckPrefixExtension(want, again.Render()); err != nil {
		t.Errorf("persisted journal is not an append-only extension: %v", err)
	}
}

func TestCouncilMemory_PutRefusesOversizedSnapshot(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	j := journalengine.New(CouncilMemoryLane, nil)
	j.RecordTurn(0, "Council run 1 completed.", nil, strings.Repeat("x", CouncilMemoryMaxSnapshotBytes+1))

	err := st.CouncilMemory.Put(ctx, j)
	if !errors.Is(err, ErrCouncilMemoryTooLarge) {
		t.Fatalf("put oversized = %v, want ErrCouncilMemoryTooLarge", err)
	}
	// The refusal must not leave a partial row behind.
	var rows int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM council_memory`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Errorf("refused Put still wrote %d row(s)", rows)
	}
}

func TestCouncilMemory_Delete(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	j := journalengine.New(CouncilMemoryLane, nil)
	j.RecordTurn(0, "Council run 1 completed.", nil, "Minted backlog items: none.")
	if err := st.CouncilMemory.Put(ctx, j); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := st.CouncilMemory.Delete(ctx); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := st.CouncilMemory.Get(ctx)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if rendered := got.Render(); rendered != journalengine.EmptyJournal {
		t.Errorf("journal survived delete: %q", rendered)
	}
}

// An unconfigured DAO must error rather than panic: the operator passes
// st.CouncilMemory into the editor as an interface, so a nil DAO becomes a
// non-nil interface value the render path still calls.
func TestCouncilMemory_UnconfiguredDAOErrors(t *testing.T) {
	var d *CouncilMemoryDAO
	ctx := context.Background()
	if _, err := d.Get(ctx); err == nil {
		t.Error("Get on a nil DAO should error")
	}
	if err := d.Put(ctx, journalengine.New(CouncilMemoryLane, nil)); err == nil {
		t.Error("Put on a nil DAO should error")
	}
	if err := d.Delete(ctx); err == nil {
		t.Error("Delete on a nil DAO should error")
	}
}
