- **Mills store timestamps now sort chronologically inside a second**
  (`pkg/mills/store`): every `*_at` column was written with
  `time.RFC3339Nano`, which trims trailing fractional zeros, so SQLite's
  byte-wise `ORDER BY started_at` put `…28.8481Z` after `…28.84815Z` and a
  keyset cursor rebuilt from a parsed row could re-match its own row. Timestamps
  are now stored fixed width (`2006-01-02T15:04:05.000000000Z`, still
  RFC3339Nano-parseable), migration 042 rewrites existing trimmed rows in
  place, and `ListStages` / `ListGates` break exact ties by `id`. Fixes the
  `TestHandlePipelineRunRetryAttempts` flake (9 of 300 runs on main).
