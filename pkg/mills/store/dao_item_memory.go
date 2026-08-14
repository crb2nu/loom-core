package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crb2nu/loom/pkg/journalengine"
)

// ItemMemoryMaxSnapshotBytes bounds one backlog item's persisted journal.
//
// Entries are naturally bounded (stages x attempts), so this is a defensive
// stop rather than a routine limit: a pathological run must not grow a row that
// every subsequent prompt then re-reads. Put refuses an oversized snapshot with
// ErrItemMemoryTooLarge instead of truncating, because silently rewriting the
// stored history would break the append-only render contract the journal exists
// to provide.
const ItemMemoryMaxSnapshotBytes = 256 * 1024

// ErrItemMemoryTooLarge is returned by ItemMemoryDAO.Put when the marshalled
// snapshot exceeds ItemMemoryMaxSnapshotBytes. Callers log and continue — a
// journal write must never fail a pipeline stage.
var ErrItemMemoryTooLarge = errors.New("mills store: backlog item memory snapshot exceeds the size cap")

// ItemMemoryDAO persists one journalengine.Journal per backlog item
// (migration 017).
//
// Consolidation is deliberately NOT wired in v1: distilling old epochs rewrites
// the top of the journal, which is the one deliberate prefix-cache reset event,
// and a Mills item's history is short enough that the size cap above is a
// cheaper guard than an LLM call. Wire a Consolidator only once the cap is
// observed to bite.
type ItemMemoryDAO struct {
	db *sql.DB
}

// Get returns the item's journal, or a fresh empty one owned by backlogID when
// the item has no memory yet. An absent row is the normal first-stage case, not
// an error.
func (d *ItemMemoryDAO) Get(ctx context.Context, backlogID string) (*journalengine.Journal, error) {
	if d == nil || d.db == nil {
		return nil, errors.New("item memory: dao not configured")
	}
	if backlogID == "" {
		return nil, errors.New("item memory: backlogID required")
	}
	var payload string
	err := d.db.QueryRowContext(ctx,
		`SELECT snapshot_json FROM backlog_item_memory WHERE backlog_id = ?`,
		backlogID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return journalengine.New(backlogID, nil), nil
	}
	if err != nil {
		return nil, fmt.Errorf("item memory: load %s: %w", backlogID, err)
	}
	var snap journalengine.Snapshot
	if err := jsonInto(payload, &snap); err != nil {
		return nil, fmt.Errorf("item memory: decode %s: %w", backlogID, err)
	}
	// A snapshot written before an owner was set (or hand-edited) must still
	// restore under the item's own id, or RecordTurn would attribute the item's
	// own responses to "unknown" and the render bytes would shift.
	if snap.Owner == "" {
		snap.Owner = backlogID
	}
	return journalengine.FromSnapshot(snap), nil
}

// Put upserts the item's journal snapshot. Returns ErrItemMemoryTooLarge
// (wrapped, with sizes) when the payload would exceed the cap; the caller is
// expected to log and continue rather than fail the work that produced it.
func (d *ItemMemoryDAO) Put(ctx context.Context, backlogID string, j *journalengine.Journal) error {
	if d == nil || d.db == nil {
		return errors.New("item memory: dao not configured")
	}
	if backlogID == "" {
		return errors.New("item memory: backlogID required")
	}
	if j == nil {
		return errors.New("item memory: journal required")
	}
	payload, err := jsonField(j.Snapshot())
	if err != nil {
		return fmt.Errorf("item memory: encode %s: %w", backlogID, err)
	}
	if len(payload) > ItemMemoryMaxSnapshotBytes {
		return fmt.Errorf("%w: %s is %d bytes (cap %d)",
			ErrItemMemoryTooLarge, backlogID, len(payload), ItemMemoryMaxSnapshotBytes)
	}
	_, err = d.db.ExecContext(ctx, `
		INSERT INTO backlog_item_memory (backlog_id, snapshot_json, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(backlog_id) DO UPDATE SET
			snapshot_json = excluded.snapshot_json,
			updated_at    = excluded.updated_at`,
		backlogID, payload, timeRFC3339(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("item memory: persist %s: %w", backlogID, err)
	}
	return nil
}

// Delete drops an item's memory. Used by operator recovery when a journal is
// judged poisoned; normal lifecycle relies on the backlog_items cascade.
func (d *ItemMemoryDAO) Delete(ctx context.Context, backlogID string) error {
	if d == nil || d.db == nil {
		return errors.New("item memory: dao not configured")
	}
	if _, err := d.db.ExecContext(ctx,
		`DELETE FROM backlog_item_memory WHERE backlog_id = ?`, backlogID); err != nil {
		return fmt.Errorf("item memory: delete %s: %w", backlogID, err)
	}
	return nil
}
