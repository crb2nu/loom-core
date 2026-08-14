package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crb2nu/loom/pkg/journalengine"
)

// CouncilMemoryLane is the fixed owner key of the council lane's single
// memory row (migration 018). It is also the journal's owner string, so a
// snapshot restored from the row records subsequent turns under the same
// identity and the render bytes do not shift.
const CouncilMemoryLane = "council"

// CouncilMemoryMaxSnapshotBytes bounds the council lane's persisted journal.
//
// Mirrors ItemMemoryMaxSnapshotBytes deliberately: same cap, same refusal
// semantics. Unlike a backlog item's journal this one is unbounded in
// principle — the council lane outlives every item — so the cap is the routine
// growth guard rather than a defensive stop. Put refuses an oversized snapshot
// with ErrCouncilMemoryTooLarge instead of truncating, because silently
// rewriting the stored history would break the append-only render contract the
// journal exists to provide.
const CouncilMemoryMaxSnapshotBytes = 256 * 1024

// ErrCouncilMemoryTooLarge is returned by CouncilMemoryDAO.Put when the
// marshalled snapshot exceeds CouncilMemoryMaxSnapshotBytes. Callers log and
// continue — a memory write must never fail a council run.
var ErrCouncilMemoryTooLarge = errors.New("mills store: council memory snapshot exceeds the size cap")

// CouncilMemoryDAO persists one journalengine.Journal for the council lane
// (migration 018).
//
// Consolidation is deliberately NOT wired here, exactly as in ItemMemoryDAO:
// distilling old epochs rewrites the top of the journal, which is the one
// deliberate prefix-cache reset event. The cap above is the v1 guard; wire a
// Consolidator only once it is observed to bite.
type CouncilMemoryDAO struct {
	db *sql.DB
}

// Get returns the council lane's journal, or a fresh empty one when the lane
// has no memory yet. An absent row is the normal first-run case, not an error.
func (d *CouncilMemoryDAO) Get(ctx context.Context) (*journalengine.Journal, error) {
	if d == nil || d.db == nil {
		return nil, errors.New("council memory: dao not configured")
	}
	var payload string
	err := d.db.QueryRowContext(ctx,
		`SELECT snapshot_json FROM council_memory WHERE lane = ?`,
		CouncilMemoryLane).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return journalengine.New(CouncilMemoryLane, nil), nil
	}
	if err != nil {
		return nil, fmt.Errorf("council memory: load: %w", err)
	}
	var snap journalengine.Snapshot
	if err := jsonInto(payload, &snap); err != nil {
		return nil, fmt.Errorf("council memory: decode: %w", err)
	}
	// A snapshot written before an owner was set (or hand-edited) must still
	// restore under the lane id, or RecordTurn would attribute the council's own
	// responses to "unknown" and the render bytes would shift.
	if snap.Owner == "" {
		snap.Owner = CouncilMemoryLane
	}
	return journalengine.FromSnapshot(snap), nil
}

// Put upserts the council lane's journal snapshot. Returns
// ErrCouncilMemoryTooLarge (wrapped, with sizes) when the payload would exceed
// the cap; the caller is expected to log and continue rather than fail the run
// that produced it.
func (d *CouncilMemoryDAO) Put(ctx context.Context, j *journalengine.Journal) error {
	if d == nil || d.db == nil {
		return errors.New("council memory: dao not configured")
	}
	if j == nil {
		return errors.New("council memory: journal required")
	}
	payload, err := jsonField(j.Snapshot())
	if err != nil {
		return fmt.Errorf("council memory: encode: %w", err)
	}
	if len(payload) > CouncilMemoryMaxSnapshotBytes {
		return fmt.Errorf("%w: %s is %d bytes (cap %d)",
			ErrCouncilMemoryTooLarge, CouncilMemoryLane, len(payload), CouncilMemoryMaxSnapshotBytes)
	}
	_, err = d.db.ExecContext(ctx, `
		INSERT INTO council_memory (lane, snapshot_json, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(lane) DO UPDATE SET
			snapshot_json = excluded.snapshot_json,
			updated_at    = excluded.updated_at`,
		CouncilMemoryLane, payload, timeRFC3339(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("council memory: persist: %w", err)
	}
	return nil
}

// Delete drops the council lane's memory. Used by operator recovery when the
// journal is judged poisoned; there is no lifecycle cascade to rely on, because
// the lane outlives every row that could own it.
func (d *CouncilMemoryDAO) Delete(ctx context.Context) error {
	if d == nil || d.db == nil {
		return errors.New("council memory: dao not configured")
	}
	if _, err := d.db.ExecContext(ctx,
		`DELETE FROM council_memory WHERE lane = ?`, CouncilMemoryLane); err != nil {
		return fmt.Errorf("council memory: delete: %w", err)
	}
	return nil
}
