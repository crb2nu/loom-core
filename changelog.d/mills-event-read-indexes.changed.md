Mills event store gains `(actor, occurred_at)` and `(kind, occurred_at)` read
indexes (migration 029), unpinning the operator core that 336h report scans
burned after the 2026-08-15 storm. The append cost was measured (+52%,
~+13µs absolute) and merges under a documented, expiring benchmark waiver
rather than a silently moved threshold.
