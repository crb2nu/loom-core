Skills registry optimization pass: generated Claude command frontmatter now
emits quoted/escaped `description:` scalars (33 files were strict-YAML
invalid); the always_allow validator no longer counts `>/dev/null`, `>&2`,
`->` arrows, or numeric comparisons as file writes and supports an explicit
`# loom-always-allow: <rationale>` opt-in marker; registry validation errors
went 133 → 0 (removed unsupported MCP-tool-name always_allow blocks from the
six icc-* skills and the auto-ship `gitlab_green_loop.sh` entry); 24 skill
descriptions rewritten to trigger-oriented what+when form with exclusion
clauses per current Agent Skills authoring guidance; stale Kilocode/Gemini
config-path rows and dead `quality_*` tool references fixed; explicit
`targets:` blocks added to auto-quality-gate and tdd-dev.
