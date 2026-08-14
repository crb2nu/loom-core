- **Branch-MR awareness now reaches agent context automatically (mrwatch M3a).**
  `loom sync claude`/`loom sync gemini` now generate a context-injection hook
  that runs `loom agent mr-status --hook <vendor>` at the start of each turn —
  Claude Code on `UserPromptSubmit`, Gemini CLI on `BeforeAgent` — so an agent
  learns its own branch's merge request is stalled without being told.
  Injection is doubly gated and stays quiet by default: only merge requests in
  an attention-worthy state (`conflict`, `ci_failed_deterministic`,
  `ci_failed_flaky`, `automerge_unarmed`, `pipeline_skipped`, `stale_branch`)
  are eligible, and that set must have changed since the last injection, so
  ordinary pipeline progress never interrupts a turn. The hook is best-effort:
  an unreachable or slow HUD prints nothing and exits 0 rather than breaking
  the turn. Disable per platform with
  `platform_permissions.<platform>.settings.mrwatch_hook: false`.
