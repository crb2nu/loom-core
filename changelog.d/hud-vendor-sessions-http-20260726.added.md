HUD HTTP surface for the cross-vendor session bridge: `GET /api/vendor-sessions`
and `GET /api/vendor-sessions/search` (new `vendorsessions` domain) bridge to the
agent-context `agent_vendor_session_list` / `agent_vendor_session_search` tools so
Claude Code + Codex desktop transcripts are browsable from the HUD. The Operator
Deck's InspectDock agent lens gains a "Vendor transcripts" affordance (list +
substring search, seeded with the agent's project as a cwd filter), and the
mobile operator token may reach the new read-only routes for a future iOS
surface. Requires the agent-context server to resolve locally
(`agent_context: prefer-local`) — hub-delegated pods see empty transcript roots.
