- **Mills: the council sources demand from the factory's own exhaust**
  (`pkg/mills/council/factory_exhaust.go`, `pkg/mills/council/brief.go`,
  `pkg/mills/clients/council_factory_exhaust.go`, `pkg/mills/policy.go`,
  `pkg/mills/runner/runner.go`, `cmd/loom-mills-operator/main.go`): the mill
  files machine-readable maintenance demand against itself — one `flaky-test`
  issue per quarantined test from `scripts/flakereport`, one `audit-digest`
  issue per day from `pkg/mills/audit` — and then waits for a human to notice
  it, so an unattended shift with a thin roadmap idles instead of doing
  self-maintenance. `council.Compile` now renders a **Factory exhaust (open
  self-maintenance demand)** section listing the newest open issues under both
  labels with their refs, bounded to 10 items over a 14d window. The section
  carries evidence, not instructions: the council still decides what to propose,
  and an exhaust-seeded proposal is an ordinary `BacklogProposal` that rides the
  unchanged backlog-dedup, gray-band, and merged-work grounding guards with no
  provenance special-casing — which is what stops a still-open issue re-minting
  the same item nightly. A fetch failure renders the section as `exhaust source
  unavailable` rather than omitting it (an omitted section reads as "the factory
  is clean") and never blocks brief compilation; a partial fetch fails the whole
  call for the same reason. Raises
  `mills_council_factory_exhaust_items_total{kind}` and
  `mills_council_factory_exhaust_errors_total`. Policy-gated via
  `council.sources.factory_exhaust.enabled` (default true), `lookback_hours`,
  and `max_items`.
