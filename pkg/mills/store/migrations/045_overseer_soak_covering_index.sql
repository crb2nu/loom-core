-- Each soak event contains one small decision counter, not a daily snapshot.
-- Cover the seven-day evidence scan without reading payload-bearing table pages.
-- Keep all rows (including malformed evidence) so the Go validator fails closed.
-- Other event kinds do not occupy or maintain entries in this partial index.
CREATE INDEX idx_events_soak_daily
    ON events(subject_id, id, payload_json)
    WHERE kind = 'overseer.soak.daily' AND subject_kind = 'utc_day';
