Vendor transcript federation: the HUD mirror now pushes the workstation's
claude/codex transcript metadata — plus a bounded tail of extracted lines
for changed sessions — to the remote HUD's new `POST
/api/vendor-sessions/mirror` ingest (default on when `LOOM_HUD_MIRROR_URL`
is set; `LOOM_HUD_MIRROR_VENDOR_SESSIONS=0` disables,
`LOOM_HUD_MIRROR_VENDOR_INTERVAL` tunes the 60s cadence). The
vendor-sessions domain merges the per-host federated store (5-minute TTL)
into `GET /api/vendor-sessions[/search]`, tagging rows with `host`, so the
cluster HUD's Operator Deck and the iOS companion can browse and grep
workstation transcripts off-LAN. `degraded:true` now means no source at
all — a live federated host counts. Raw JSONL never leaves the host:
entries are whitespace-normalized 600-byte extracts
(`pkg/vendorsessions.Tail`), and federated search covers each transcript's
recent window (last 200 lines), not the full file.
