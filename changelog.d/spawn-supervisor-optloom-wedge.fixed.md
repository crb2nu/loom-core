Fix the S4 supervisor zero-output wedge: the launcher runs as the
unprivileged agent user, but `/opt/loom` was root-owned in the spawn runtime
image, so `mkdir -p` of the per-spawn state dir failed silently and the
outcome wait-loop spun for the full exec deadline (20–60 min at $0, live
2026-07-19 — no codex process ever started). The runtime image now creates
`/opt/loom` agent-owned, and the launcher fails fast with exit 233 and a
diagnostic (orphan marker in attach mode) when the state dir cannot be
created.
