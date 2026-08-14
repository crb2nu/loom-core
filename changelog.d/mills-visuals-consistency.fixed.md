- HUD Mills: one canonical priority/status tone map — the floor-nav ribbon and
  the Warps priority bands no longer render two different color ramps for
  P0–P3 on the same screen, and mills states (`queued`/`paused`/`escalated`/
  `merged`/`done`) now resolve to real badge tones instead of uniform grey.
- HUD Mills: the floor-nav spine's bolt/spark tallies no longer read 0 on
  Warps/Shuttles — the store primes the run archive on its poll loop instead
  of relying on whichever panel happened to fetch it.
- HUD Mills: honest header counts — Warps counts what its bands render
  (queued/paused), Shuttles counts in-flight runs (matching the spine node on
  the same screen), and the mills sub-tab badges are rewired from the deleted
  `pipelines`/`backlog` arms to live warps/shuttles/sparks/bolts counts.
- HUD Mills: the Overview degraded-health "View escalations" button no longer
  deep-links into a phantom Warps drawer; it opens Sparks. Stale "pipelines"
  vocabulary on the banner/buttons now says shuttles.
- HUD Mills: fetch failures now reach PanelShell's red error surface on
  Audit/Council/Eval/Cross-Repo/Squads/Overseers/Policy/Workflows instead of
  masquerading as a calm idle empty state; stale-refresh keeps last-good rows
  with an inline banner.
- HUD Mills: the pipeline-run drawer opens with the lineage strand
  (warp → stage picks → terminal bolt/spark, failing stage highlighted,
  escalation reasons on the spark) — activating the previously dead
  `LineageRibbon` strand mode.
- HUD Mills: Council's cost column reads the operator's split
  `CostFrontierUSD`/`CostLocalUSD` fields (it previously read a nonexistent
  `CostUSD` and rendered `—` on every row).
- HUD Mills: Shuttles boards escalated/paused runs as diverted/held instead of
  "en route"; delayed shuttles show the observed stage age; the departure
  board's warp column is labelled `warp` (it was mislabelled `bolt`).
- HUD Mills: cross-repo abort routes through `adminFetch` (Labs token or
  Cloudflare Access SSO) with the shared ConfirmDialog — removing the last
  `globalThis.prompt()` credential flow in the app.
- HUD Mills: KPI row grid fits all six cards; Policy proposals and Workflows
  panels poll with the shared visibility-pausing poller instead of a one-shot
  fetch / raw `setInterval`; `relativeTime` gained a future tense so overseer
  suppression leases no longer read "until just now".
