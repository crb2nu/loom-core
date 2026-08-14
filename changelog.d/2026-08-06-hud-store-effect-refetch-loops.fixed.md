Fixed the MR !1474 refetch-loop class across every HUD frontend store: store
methods invoked synchronously from component mount `$effect`s no longer read
`$state` before their first `await`, so a completed fetch can never re-run the
effect that started it. Confirmed live loops fixed: `millsStore.openRunDetail`
(InspectDock re-fetched the pipeline drawer every round trip),
`millsStore.ensureWorkflowRunsLoaded` (BacklogDetail looped whenever the mill
had zero workflow runs), `spawnStore.startPolling` (looped config + roster
fetches when `/api/agent/spawn/config` errored), and
`sandboxStore.fetchProjectStatus` (synchronous read-and-rewrite of the loading
set killed SandboxLive with `effect_update_depth_exceeded` once an admin token
was set). Same-class pre-await reads untracked in `openBacklogDetail`, the
mills `disabled` gates, and the filter-driven fetches of the catalog, graph,
knowledge, memory, patterns, stream, and presence-diagnostics stores (each
previously double-fetched and restarted its poller on every filter keystroke).
Regression `.dom` tests pin each fixed entry point to exactly one fetch inside
a tracking effect.
