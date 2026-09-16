- `mcp-mills` is now reachable from profile-limited sessions: its eight
  `mills_*` tools sit in the `llm-core` proxy priority list (after position
  100, so antigravity-core is unchanged), and the server sends the Cloudflare
  Access service-token headers (`LOOM_MILLS_CF_ACCESS_ID/SECRET`, falling
  back to `CF_ACCESS_CLIENT_ID/SECRET`) for non-loopback operator URLs. The
  server was registered and buildable but never in the priority list, so a
  Claude Code or Codex session could not see it and every factory interaction
  fell back to curl against the operator REST API (factory-from-CC plan, S0).
