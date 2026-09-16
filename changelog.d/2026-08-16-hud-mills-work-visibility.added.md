- **HUD: mills work visibility — repo, origin, journey, and waiver expiry**
  (`internal/hud/frontend/src/lib/components/mills/`,
  `cmd/loom-mills-operator/handlers_provenance.go`): 159 backlog items reached
  `merged` in a trailing-48h window on the live floor, and the Bolts table
  answered almost nothing about them. Its columns were `run · bolt · plan/book ·
  cost · merged · swatch`, where `run` is an opaque slug and `plan/book`
  preferred `PlanID` over `Title` — so the one field carrying *why an item
  mattered* was the fallback branch of a column that hid itself first at narrow
  widths. **The data was already on the wire the whole time**: the backlog list
  serializes the full untagged `store.BacklogItem`, so `TargetProject`,
  `Labels`, and `CreatedBy` arrive on every row of every 15s poll. `Labels`
  rendered in exactly one place (the drawer header, as flat grey chips),
  `CreatedBy` in exactly one (a suffix on a timestamp), and `TargetProject`
  **nowhere at all** — its only use anywhere in the frontend was as an argument
  to `mrURL()`, so three panels resolved the repo and spent the answer on an
  `href`. With work now spanning flexdeck/flexinfer/procmodel, cross-repo merges
  and home merges were pixel-identical.
  Bolts rows now lead with the item title over its id, and carry a repo chip
  (cross-repo marked, home deliberately quiet — the exception is the signal) and
  an origin chip, with facet filters for both. Classification lives in one pure,
  unit-tested `provenance.ts`; its taxonomy is grounded in the live `CreatedBy`
  distribution rather than assumed, which is why `plan` exists as a bucket at
  all — `mills:plan-slice-emitter` authored 95 of those 159 items and appears in
  nobody's mental model until it is named. `CreatedBy` is free text with 19
  live spellings (`mills canary autopilot`, `claude-code:millsreconcilerslow-investigation`,
  `operator-seed:claude-code`), so matching normalizes separators and works by
  prefix/substring; equality against a fixed set would misclassify most of the
  floor. The two encodings of the home repo (460 items with an empty
  `TargetProject`, 63 with an explicit `services/loom-core`) collapse to one
  facet, or the counts would lie.
- **Two small read-only operator endpoints.** `GET /api/mills/backlog/{id}/events`
  serves an item's recorded-event ledger (`Events.ListBySubject`, already indexed
  by migration 029) behind the drawer's new Journey strip. The strip is
  captioned as a record of what was *logged*, never as a complete lifecycle:
  only transitions routed through `TransitionStateWithEvent` plus explicit
  appends are recorded, and a plain queued→running claim writes no event, so the
  item's pipeline runs are interleaved to cover the stretches the events table
  never sees. An unknown id is 404, not an empty list — "no such item" and "item
  with a quiet ledger" are different answers, and collapsing them sends an
  operator hunting for a missing writer that was never the problem.
  `GET /api/mills/fleet-gate/waivers` reads the committed
  `scripts/ci/fleet_reliability_suite_v1.json` from the operator's repo checkout
  and adds the expiry countdown, surfaced as a Telemetry card. Benchmark waivers
  were legible only to CI, which makes their self-expiry a silent cliff: past
  `until` the gate snaps back to the global threshold on its own and the next
  run fails with no warning anyone saw. `Until` is **inclusive**, so a waiver
  expiring today reports `0` days and is *not* expired — an off-by-one here
  would declare a live waiver dead. Both endpoints fail soft (503 with a reason
  when the manifest is unreadable; the ledger's failure leaves the drawer
  intact), and the HUD degrades cleanly against an operator that predates them —
  verified in-browser against live mills, where both routes 404 and the panels
  render normally.
- **Regression guard hit during this work**: `openBacklogDetail` is called from
  tracking `$effect`s, and the new ledger fetch wrote its loading state
  *tracked*, re-arming the caller's effect into an infinite fetch loop —
  `mills_loop.dom.test.ts` caught it at 12 calls. The write is now `untrack`ed
  like its sibling, and the test pins the exact expected fetch count (detail +
  ledger) rather than a bare "no loop".
