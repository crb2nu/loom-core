- Spawn: claude-code spawns always pin a model. `resolveClaudeModel` mirrors
  the Codex resolver — request model (Mills `agent_routing` / `stage_models`)
  wins, then `SPAWN_CLAUDE_MODEL`, then the compiled default `claude-opus-5`
  — on both the CLI and SDK-driver paths, so label-routed Claude spawns no
  longer fall through to whatever the installed CLI defaults to. The
  mobile-hud manifest sets `SPAWN_CLAUDE_MODEL=claude-opus-5` explicitly.
