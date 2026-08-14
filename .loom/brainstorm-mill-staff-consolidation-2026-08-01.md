# Mill Staff — consolidating council / squads / overseers (2026-08-01)

**Question**: the mill has three "judgment" subsystems — council, squads, and
overseers — plus the weaver name floating nearby. What should combine, what
should stay distinct, and what factory-floor naming distinguishes the lanes?

**Answer (converged)**: keep **three lanes** — they sit at different phases of
the line and must not merge — but consolidate the **substrate** they share,
and have each lane **shed the dead limbs that reach into another lane's live
territory**. Theming applies to docs + HUD labels only; code names
(`council`, `squads`, `overseer`) stay.

Grounding: three parallel code maps run 2026-08-01 against the current tree
(overseers ~3.8k LOC, squads ~3.1k LOC, council ~12.2k LOC incl. runner +
clients). File:line citations below are from those maps.

## 1. The lane charter

The machinery weaves; the staff decide. Umbrella name: **Mill Staff**.

| Dept (themed) | Code | Phase | Decision rights | Cadence | Risk posture |
|---|---|---|---|---|---|
| **Drawing Office** | `pkg/mills/council` (+ `runner`, `clients/council*`) | plan-time | what enters the mill: briefs → proposals → backlog items / plans / workflow selections; dedup at authoring | cron (6h) + manual + incident | quorum → forced-Partial drops mutations; transactional budget reservations; fail-closed on missing intents / storage health |
| **Drawing-in Room** | `pkg/mills/squads` | dispatch-time | which crew a committed run is bound to; outcome memory feeds routing confidence | per-dispatch, deterministic | policy-gated; **advisory today** (Decision is telemetry-only) |
| **The Alley** | `pkg/mills/overseer` | floor-time | interventions on the running floor: dedup-close, zombie-close, demote, suppress admission, file issue, alert, pause | 5–60 min ticks | guarded auto-act: dry-run default, per-action allow flags, durable day/lifetime caps, audit actors |

Why three lanes and not one: plan-time work is LLM-deliberative and budgeted
per-run; dispatch-time work must be deterministic and crash-reconstructable
(routing happens post-commit precisely so recovery re-derives identical
attribution, `reconciler.go:1533-1535`); floor-time work is periodic,
act-under-caps supervision. Different cadences, different failure domains,
different promotion gates. The maps confirmed the lanes are already almost
disjoint in code — the only live cross-imports are `council.TitleJaccard`
(borrowed by the groomer) and the `workflow/registry` leaf (used by the
council mutator to validate selections).

**Weaver is not staff.** `pkg/weaver` is the daemon-side local-model MCP tool
orchestrator: zero imports to/from any Mills package. The HUD Shuttles panel's
"weaver capacity" strip uses *weaver* as the loom-metaphor word for a fleet
agent — a vocabulary collision, not a code relationship. Excluded from this
consolidation; the glossary should note the collision.

## 2. What each lane sheds (the actual consolidation)

The maps found a symmetric pattern: each package carries dead code that
belongs to another lane's live implementation. Consolidation = deletion along
lane boundaries, not merging.

| Dead limb | Where | LOC | Whose territory it reaches into | Action |
|---|---|---|---|---|
| `Planner` + `prompts/squad_planner.md` + `FlexInferSpawner` | squads | ~520 (+ ~400 test) | plan refinement = Drawing Office (council/spin own it) | **delete** |
| `MarkRegressed` + `SquadDAO.UpdateOutcome` + `merged_regressed` enum | squads | ~50 | the "regression gate (slice 6.3)" never existed | **delete** (HUD stat is structurally 0) |
| manifest `tests`/`gates`/`ensemble`/`budget_share`/`recursion_enabled` | squads | schema | execution shaping = workflow templates (S7 landed on `ItemPolicy`, not the squad manifest) | **document as reserved/unenforced now**; enforce-or-remove decided in slice 4 |
| `policy.squads.routing.min_confidence` / `.fallback` | policy | — | dead config masked by defaults that happen to match | **wire `min_confidence`** into the router adapter (small); fallback already implicit |
| `EvaluateStalePlan`, `status.go` evaluators, `StabilityFirstOrder`, `DecideEscalation` | council | ~600 | floor-watching = the Alley (foreman/groomer are the live implementation); escalation = pipeline Escalator | **delete after caller re-verification** (zero non-test callers per map) |
| `ci_incident_classifier.go` (614 LOC) | council | 614 | live classification comes from the pipeline's persisted `escalation_*` columns | **verify-then-decide** — riskier claim, own slice |
| Debate Mode (`debate.go`, `moderator.go`, DAO, endpoint, HUD expander) | council | ~600+ | nothing — designed but `Runner.Moderator` is never assigned in prod | **keep, explicitly parked** (open decision below) |

## 3. What combines (shared substrate)

1. **`pkg/mills/textsim` (new leaf)** — tokenizer + `TitleJaccard` +
   gray-band constants. Today the 0.55 gray-band floor is mirrored in three
   files (`council/backlog_mutator.go`, `policy_overseers.go`,
   `overseer/groomer.go`) with "keep in lockstep" comments and no enforcing
   test, and `TitleJaccard` is exported from council *for* the groomer.
   Extracting a leaf kills the mirror and the backwards export.
2. **Guarded-action discipline** — the overseer harness
   (`Agent`/`Harness`/`ActionRecorder`: dry-run-by-default via nil `*bool`,
   per-action allow flags, durable day caps from events, per-subject lifetime
   caps, `RecordOnce`/`FlagOnce`/`Observe` semantics) is the best supervision
   substrate in the tree. Slice 4 extracts it to a leaf (working name
   `pkg/mills/guard`) so council mutations and future squad actions ride the
   same discipline. Existing audit actors/kinds (`overseer.<agent>.<action>`)
   are **frozen** — consumers exist; only new actors adopt the convention.
3. **One staff surface in the HUD** — the three panels group under **Mill
   Staff** with themed labels: "Drawing Office (Council)", "Drawing-in
   (Squads)", "The Alley (Overseers)". A recent-actions strip reads across
   actors via the existing `Events.ListByActorSince`. Code names stay.

## 4. Slice plan

- **S0 (this MR)** — charter: this doc + `docs/FACTORY_MODEL.md` §3 "Mill staff".
- **S1 — foundation cleanup** (loom-core): extract `pkg/mills/textsim`;
  delete squads Planner/FlexInferSpawner/MarkRegressed; wire
  `squads.routing.min_confidence`; fix stale doc comments
  (`squads/types.go:6-8` source-of-truth, `clients/flexinfer.go:1501`,
  `main.go:1008-1010`, `policy_overseers.go:116-117,140-141`,
  `scheduler_min.go:344`, `interp.go:16-18`).
- **S2 — deploy the Alley** (gitops + loom-core): add the missing
  `overseers:` block to `configmap-policy.yaml` (all three agents enabled,
  `dry_run` on) + policy-checksum bump; add `mills_overseer_*` Prometheus
  metrics (three-table metrics_test discipline) so the soak is observable;
  fix the hardcoded agent-name switch in `handleOverseersStatus`.
  Promotion gate (from the ship-time plan, never started): ≥1 week soak,
  zero false-positive dedup verdicts in `overseer.groomer.*.dryrun` events
  → flip `allow.dedup_close`.
- **S3 — close the attribution hole**: workflow-lane (imperative) runs
  bypass `dispatchCommittedStart`, so they are never squad-routed and never
  produce `squad_outcomes` rows — squad confidence silently thins as that
  lane grows. Route + attribute at imperative start; record outcomes at
  terminal settle.
- **S4 — guarded-action substrate**: extract harness/recorder to
  `pkg/mills/guard`; council mutator + squad actions adopt it; unified Mill
  Staff HUD group + recent-actions strip; decide manifest
  advisory-fields enforce-vs-remove here.

## 5. Riskiest assumption + kill-test

**Assumption (for the whole charter)**: the three lanes are structurally
separable — no live coupling *requires* merging them, so substrate extraction
is safe refactoring rather than an architecture change.

**Kill-test (cheap, before S1 merges)**: after extracting `textsim`, the
**direct** import lists of `pkg/mills/council`, `pkg/mills/squads`, and
`pkg/mills/overseer` must show pairwise no cross-lane imports — shared
dependencies limited to `store`, `textsim`, `workflow/registry`, and
`pkg/mills` itself. If a hidden coupling surfaces, stop and re-plan before
S2–S4.

**Status: RUN 2026-08-01, PASSED on direct imports** (S1 branch,
`go list -f '{{join .Imports "\n"}}'` per lane → zero cross-lane hits).
One transitive path remains and is expected: `overseer → pipeline →
council` — the sentinel/foreman borrow `pipeline.IssueClient` and the
escalation dedup markers, and pipeline borrows council's policy
vocabulary (`AutonomyGate`, degraded policy, `FormatExternalEscalation`).
That is machinery-level coupling, not a lane merge; §2's shed list
(`DecideEscalation` → Escalator territory) is the eventual fix.

**Assumption (for S2 promotion)**: dry-run groomer verdicts on real floor
data are accurate enough to promote. Kill-test = the soak itself; the
promotion gate is zero false-positive dedup verdicts over ≥1 week.

## 6. Open decisions (parked, not blocking)

- **Debate Mode**: wire a real Moderator adapter behind the existing
  default-off `debate.enabled.*` flags, or delete ~600 LOC + table +
  endpoint. Parked until the Drawing Office needs multi-round quality.
  (`prompts/moderator.md` is unreferenced by any Go file and documents
  policy keys that don't exist — fix or delete with this decision.)
- **Squad manifest advisory fields**: enforce (`budget_share` at dispatch,
  `gates` as required-gate injection) or remove from schema. Note the live
  ConfigMap sets `budget_share` on both squads today; removal is a
  ConfigMap-visible change.
- **`ci_incident_classifier.go`**: confirm off-live-path claim, then fold
  its vocabulary into the pipeline classifier or delete.
- **Unread council trigger keys** (`triggers.on_roadmap_change`,
  `on_incident`, `on_merge_drift_hours`): implement or drop from policy.

## 7. Session provenance

Maps: three parallel explorers, 2026-08-01. Related docs:
`docs/FACTORY_MODEL.md` (canonical machinery map),
`.loom/brainstorm-mills-steering-preparation-line-2026-07-03.md` (glossary
appendix — drawing-in ≈ squads, overlooker ≈ overseers were already mapped
there). Naming decision (docs+HUD only) and slice order (S1→S4) confirmed by
operator 2026-08-01.
