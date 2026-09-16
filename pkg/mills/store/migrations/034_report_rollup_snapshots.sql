-- Materialized report payloads. HTTP report handlers read only this table;
-- the periodic writer owns the bounded events-table aggregation.
CREATE TABLE report_rollup_snapshots (
    report_name    TEXT NOT NULL,
    window_seconds INTEGER NOT NULL CHECK (window_seconds > 0),
    report_key     TEXT NOT NULL DEFAULT '',
    snapshot_at    TEXT NOT NULL,
    payload_json   TEXT NOT NULL,
    PRIMARY KEY (report_name, window_seconds, report_key)
);

CREATE INDEX idx_report_rollups_latest
    ON report_rollup_snapshots(report_name, window_seconds, report_key, snapshot_at DESC);
