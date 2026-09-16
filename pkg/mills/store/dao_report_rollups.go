package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ReportRollupSnapshot is the latest durable JSON representation of one
// report/window/key tuple. Payload retains the report's existing wire shape.
type ReportRollupSnapshot struct {
	ReportName    string
	WindowSeconds int
	ReportKey     string
	SnapshotAt    time.Time
	Payload       json.RawMessage
}

type ReportRollupDAO struct{ db *sql.DB }

// Put atomically replaces a tuple only when the supplied snapshot is at least
// as fresh as the stored value. A failed build never calls Put, preserving the
// last valid report.
func (d *ReportRollupDAO) Put(ctx context.Context, snap *ReportRollupSnapshot) error {
	if snap == nil || strings.TrimSpace(snap.ReportName) == "" {
		return errors.New("report rollup: report name required")
	}
	if snap.WindowSeconds <= 0 {
		return errors.New("report rollup: window seconds must be positive")
	}
	if snap.SnapshotAt.IsZero() {
		snap.SnapshotAt = time.Now().UTC()
	}
	if !json.Valid(snap.Payload) {
		return errors.New("report rollup: payload must be valid JSON")
	}
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO report_rollup_snapshots
			(report_name, window_seconds, report_key, snapshot_at, payload_json)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(report_name, window_seconds, report_key) DO UPDATE SET
			snapshot_at = excluded.snapshot_at,
			payload_json = excluded.payload_json
		WHERE excluded.snapshot_at >= report_rollup_snapshots.snapshot_at
	`, snap.ReportName, snap.WindowSeconds, snap.ReportKey,
		timeRFC3339(snap.SnapshotAt), string(snap.Payload))
	if err != nil {
		return fmt.Errorf("report rollup put: %w", err)
	}
	return nil
}

// Latest returns the current tuple without consulting the events table.
func (d *ReportRollupDAO) Latest(ctx context.Context, report string, windowSeconds int, key string) (*ReportRollupSnapshot, error) {
	var snap ReportRollupSnapshot
	var at, payload string
	err := d.db.QueryRowContext(ctx, `
		SELECT report_name, window_seconds, report_key, snapshot_at, payload_json
		FROM report_rollup_snapshots
		WHERE report_name = ? AND window_seconds = ? AND report_key = ?
	`, report, windowSeconds, key).Scan(&snap.ReportName, &snap.WindowSeconds, &snap.ReportKey, &at, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("report rollup latest: %w", err)
	}
	snap.SnapshotAt, err = parseTime(at)
	if err != nil {
		return nil, fmt.Errorf("report rollup snapshot_at: %w", err)
	}
	snap.Payload = json.RawMessage(payload)
	return &snap, nil
}
