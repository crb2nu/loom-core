- **Four shipped backend surfaces now have a HUD** (`internal/hud/frontend`):
  each of these REST domains was live, tested, and rendered nowhere, so the only
  way to read them was to curl the endpoint.
  - **"Waiting on you" on Overview.** `GET /api/blocked` lists the sessions the
    flightdeck bridge classified as stalled on a human — a permission prompt
    burning wall clock — longest wait first. The mobile dashboard has consumed
    it since MBL-1; the desktop HUD served the route and showed nothing. The new
    card names the agent, the reason and tool, the working directory, and the
    wait, escalating its badge past 2m and 10m, and drills into the agent's live
    session.
  - **Context ▸ Health.** `GET /api/context/{health,budget}` and
    `POST /api/context/compact/{session_id}` expose
    `monitor.ContextHealthMonitor`: per-agent context-window utilization, the
    health score behind it, stale-entry and recall-hit counts, and manual
    compaction. Compaction goes through the shared `ConfirmDialog` +
    `runAdminAction` path every other HUD mutation uses. A 503 from the
    endpoints (monitor unwired) renders as *not configured*, not as an empty
    fleet.
  - **Operations ▸ MRs.** `GET /api/mrwatch/{summary,actions}` expose the
    classified branch→MR registry and the shepherd's bounded auto-action audit
    log. An autonomous actor was retrying pipelines and arming auto-merge with
    no operator-visible record; the panel is that record, read-only.
  - **Engram rollup on Patterns.** `GET /api/engrams/summary` counts the engram
    tech tree by proof status and tier. Patterns compose engrams, so the
    library's proof state is now the header of the page that stamps them.
- **`degraded` responses stop reading as measurements** (`internal/hud/frontend`):
  `GET /api/cache` and `GET /api/engrams/summary` both set `degraded: true` when
  their upstream (the daemon cache RPC / the agent bridge) is unreachable and the
  zeros they return are placeholders. The Servers ▸ Response Cache card and the
  Fleet infrastructure tile now show a *degraded / no cache data* state instead
  of "0 entries · 0.0% hit rate", and the engram strip shows a muted
  *unavailable* instead of an all-zero breakdown. A HUD build older than the flag
  omits it, which is correctly read as *not* degraded rather than muting live
  data.
- New sub-view keys: `Operations ▸ MRs` = `m`, `Context ▸ Health` = `h`. Both are
  asserted by name in `router.svelte.test.ts`, so a future key re-cut fails the
  suite rather than silently killing the shortcut.
- New stores share one fetch boundary (`utils/apiJson.ts`) that separates *route
  absent on this build* — a 404, or the SPA catch-all answering 200 +
  `index.html` — from a real failure, so a HUD older than an endpoint says so
  instead of rendering a convincing empty dashboard.
