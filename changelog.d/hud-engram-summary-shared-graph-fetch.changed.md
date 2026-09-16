- **`GET /api/engrams/summary` rolls up the graph fetch instead of re-listing
  the catalog** (`internal/hud/bridge/agent_engrams.go`,
  `pkg/agentcontext/svc_engrams.go`): the summary issued its own
  `agent_engram_list` call — the same tool and arguments as the catalog list —
  while the HUD fetched `/api/engrams/graph` alongside it on every poll. The
  bridge now serves both routes from one `agent_engram_graph` fetch:
  overlapping requests coalesce on a single in-flight call and share its result
  for a few seconds, so the paired poll costs one upstream call and the tree
  and the summary strip always describe the same snapshot. Both response
  shapes are unchanged. Stub nodes the full graph emits for dangling
  prerequisites now carry `stub: true` and are left out of the counts (a server
  predating the marker counts them as unverified until it is redeployed). Like
  the tree, the rollup is bounded by the graph's 500-node cap, and URI-less
  legacy recipes, which the graph never carried, no longer count.
