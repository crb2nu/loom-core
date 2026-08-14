# Brainstorm: Mills improvements — textile metaphors + HUD steering

**Date**: 2026-07-03
**Triggered by**: Operator wants to improve Mills building on Pattern Loom (!870–!874) and the Plan Store; wants more factory/textile-mill metaphors for missing machinery, and wants to steer from the loom web HUD: choose models for plan/ideation slices, add generated plans to the queue, reorder priority, and keep plan status ↔ MRs in sync as merges occur.
**Constraints noted**: Build on existing Pattern Loom + Plan Store; steering surface is the loom web HUD; Mills governance today is GitOps policy YAML; budget caps are load-bearing (no-op escalation DoS precedent, !899).

**Grounding (verified 2026-07-03 via codebase sweep)**:
- Plan Store (`cmd/mcp-agent-context/tools_plan.go`): plans have phase lifecycle, `mr_refs[]`, `mills_backlog_id`, but **no priority/order field**; phases advance only via explicit `agent_plan_lifecycle_advance` calls.
- Council models are **policy-static** (`pkg/mills/policy.go` CouncilEnsemble; editor/reviewers instantiated at operator startup) — no per-run override exists.
- HUD (`internal/hud/domain/mills/mills.go`) has ~20 read routes + mutations (council run, pipeline start/pause/escalate, backlog enqueue, kill-switch) but **no plan-store panel, no reorder, no model picker**.
- Pattern Loom patterns are **advisory** — injected into the editor prompt, `pattern_id` recorded best-effort; pipeline does not enforce conformance.
- Metaphors already in use: loom (orchestration), mills (operator), weaver (spawn/inference), pattern loom (catalog), gates, council, squads.

## Phase 1 — Framings

### F1 — The Warp Beam (ordered plan queue as a first-class entity)

Warping is the mill step where threads are wound onto the beam in exact order and tension before any weaving happens. Today the Plan Store has no ordering field and Mills intake has its own opaque prioritization; there is no single place where "what gets woven next, in what order" lives. Make the beam real: add `priority`/`beam_position` to Plan, propagate it through the plan-slice emitter into Mills backlog items, expose reorder APIs, and render a drag-to-reorder **Warping panel** in the HUD (add plan to beam, reorder, pull off the beam).

- **Bet**: a single authoritative, operator-touchable execution order converts Mills from "runs whatever intake found" to "runs what I queued next."
- **Risk**: if demand stays near zero the beam is an empty UI; and ordering must actually reach the pipeline dispatcher or it's cosmetic.

### F2 — The Spinning Room (model-selectable ideation frames)

Spinning turns prepared fiber into yarn, and different frames (mule, ring, open-end) trade quality against speed and cost. Council models today are frozen at operator startup from GitOps YAML. Add a spinning room: from the HUD, pick a **frame** — claude-opus, gpt-5.4, a local flexinfer model — hand it a brief (roving), and it spins a draft plan + slices directly into the Plan Store (`phase=draft`) for review before warping. Optionally spin the same roving on two frames and keep the better yarn.

- **Bet**: operator-triggered, model-chosen ideation is the missing demand source, and model choice materially changes plan quality/cost per work class.
- **Risk**: per-run overrides cut against Mills' config-as-code auditability and budget accounting; competitive spinning multiplies spend.

### F3 — The Take-Up Motion (merge-driven status reconciler)

On a loom the take-up roller winds finished cloth and advances the warp automatically — nobody moves the beam by hand. Today plan/slice phases advance only when an agent remembers to call `lifecycle_advance`; `mr_refs` are append-only strings with no back-pressure from GitLab. Build the take-up motion: a reconciler that watches MR state and pipeline results, advances slice phases (`in_review→merged`), rolls up plan phases, marks the Mills backlog item done, and flags orphans (plan says `merging`, MR actually closed).

- **Bet**: the HUD is only trustworthy for steering if state self-trues; stale phases are why nobody steers from it today.
- **Risk**: two writers (agents + reconciler) can fight — needs precedence rules and idempotent transitions.

### F4 — The Finishing House (post-merge line: dye, calender, cloth hall)

Grey cloth isn't sellable; finishing makes it product. Mills ends at merge + eval verdict; deploy verification, changelog, docs, and announcement are ad-hoc. A finishing line adds post-merge stages: verify Flux actually rolled the image, stamp the changelog, refresh docs, and post a digest of finished goods with eval grades.

- **Bet**: the merged→delivered gap is where autonomous work loses credibility (silent deploy failures, stale images).
- **Risk**: overlaps GitOps/Flux ownership; Mills sprawls beyond its writ.

### F5 — The Overlooker's Alley (self-healing machinery as a line, not patches)

Mills reliability history is a string of wedges fixed one MR at a time (resume-wedge, zombie spawns, attempt collisions, empty-diff retries). An overlooker (tackler) subsystem walks the alley continuously: detects wedged runs, stalled spawns, non-advancing MRs; applies known recipes (405 close+reopen, zombie reap, carry-forward diff); escalates only novel faults. HUD gets an Alley panel showing per-line machine health.

- **Bet**: throughput is lost more to jams than to missing features; codified recovery beats added capacity.
- **Risk**: most known jams are already individually fixed (!868, !859, !876); a generic overlooker may be a framework in search of remaining bugs.

### F6 — The Cloth Hall (operator grading feeds the mill back)

Finished cloth was graded in the hall, and grades determined which mill got the next order. Eval scores merges, but the human operator's judgment never re-enters the system. Add one-tap grading on finished MRs in the HUD (keep / meh / regret + one line), stored on the plan/backlog item, consumed by squad routing, audit-pool weighting, and adaptive policy proposals.

- **Bet**: a small amount of human grading signal outperforms more autonomous auditors for steering quality.
- **Risk**: single operator = sparse, bursty labels; policy overfits to a handful of grades.

### F7 — The Carding Engine (continuous demand refinement)

The mill is proven idle for lack of demand (×0 merge days = no demand; .loom/163 demand-sourcing). Carding turns raw tangled fiber into clean aligned sliver. A carding engine continuously ingests raw fiber — TODOs, audit findings, flaky tests, docs drift, renovate leftovers, tech-debt registry — dedupes and normalizes it into sized slivers (well-formed backlog candidates), with a **combing** pass discarding short fibers (low-value items) before they burn budget.

- **Bet**: demand starvation, not execution, is the binding constraint; automated carding keeps the beam full without operator effort.
- **Risk**: garbage-in — auto-demand already caused the budget no-op DoS (!899); combing thresholds are hard to tune and the failure mode is expensive noise.

### F8 — Dobby & Jacquard Cards (executable pattern tiers)

Pattern Loom proved pattern-stamped work merges (sprocket stamp !874 = first non-canary autonomous merge), but patterns are advisory strings in the editor prompt. Tier them: **dobby cards** — simple parameterized stamps (dep bump, config knob, HUD panel scaffold) that compile to concrete pipeline programs — and **jacquard cards** — multi-slice punch-card programs backed by the S6 durable workflow engine. Cards become executable stage programs with their own gates, not vibes.

- **Bet**: raising the pattern-conformant share of work is the highest-leverage path to autonomous merge rate (the one proven lever).
- **Risk**: card authoring is expensive; over-constraining pushes novel work into escalation.

## Phase 2 — Cross-Pollinations & Tensions

### Combinations

- **F7 + F2 + F1 — The Preparation Line**: carding fills rovings → the spinning room turns rovings into plans (operator picks the frame/model) → warping orders them on the beam → the loom weaves. This is the full front-of-mill line, and it gives every backlog item a provenance trail: fiber → sliver → roving → yarn → beam slot. One HUD line view spans it.
- **F1 + F3 — The Live Beam**: ordering without sync shows lies; sync without ordering trues a heap. Together the beam is both *authoritative* (you set the order) and *honest* (merges advance it automatically). This is the minimal combination that makes HUD steering real rather than theater.
- **F8 + F2 — Pattern-first spinning**: choosing a frame also chooses card constraints — dobby card + cheap local model for mechanical work, jacquard card + frontier model for multi-slice features. Model choice stops being taste and becomes the seed of a routing rule.

### Tensions

- **F2 vs. GitOps governance**: per-run model choice is ephemeral steering; Mills governance is Git-reviewed policy. The axis: which decisions are session-scoped vs policy-scoped? Proposed line: the set of models *allowed* in the spinning room is policy (Git); the model *chosen per spin* is run-scoped, recorded on the council/plan row for audit.
- **F7 vs. budget safety**: auto-demand vs the no-op DoS precedent. Combing (value filter) and `escalation_class` budget discounting are prerequisites for carding, not nice-to-haves.
- **F5 vs. F1/F2 (reliability-first vs demand-first)**: evidence says demand-first — the mill idles with green machinery; the recent wedge family is patched.

## Phase 3 — Convergence

### Recommended: The Live Beam first, then the Spinning Room (F1+F3 → F2)

Every steering ask terminates in the same substrate: an ordered, truthful plan queue. Ship the beam first — priority/ordering on Plan Store plans, propagated into Mills backlog dispatch; the take-up reconciler so merges advance slice/plan phases and close backlog items without anyone remembering to call `lifecycle_advance`; and the Warping panel in the HUD (add / reorder / pull). Then the Spinning Room (per-spin model choice writing draft plans into the store) has somewhere trustworthy to deliver its yarn. This sequencing also respects the proven bottleneck: spinning generates demand, the beam executes it in the order you choose, sync keeps the whole loop honest.

### Runner-up: Full Preparation Line (add F7 carding)

If, after the Spinning Room ships, hand-spinning proves too slow to keep the mill fed (empty-beam days persist), the carding engine becomes the priority — automated demand refinement with a combing gate, feeding the same beam. What tips the choice: observed beam occupancy after 2–3 weeks of operator-driven spinning.

### Open question

Where does beam order live authoritatively — on the **Plan** (Plan Store is source of truth; Mills intake reads it) or on the **Mills backlog item** (Plan Store mirrors)? This determines who owns the reorder API, and how a conflict resolves when both are edited. (Recommendation lean: Plan Store owns order; Mills consumes — plans outlive backlog items and are already the cross-agent surface.)

## Riskiest assumption + kill-test

**Load-bearing assumption**: Mills pipeline dispatch order can be driven by a priority field propagated from Plan Store plans — i.e., a working junction exists (or can be added without schema conflict): `Plan.priority` → plan-slice emitter → `BacklogItem` priority → `tryStart` selection order in `pkg/mills` (store + pipeline dispatcher).

**Kill test** (≤30 min): (1) Read `pkg/mills/store` backlog schema + the dispatcher's item-selection query — does any priority/ordering column influence start order today? (2) Enqueue two backlog items with different priorities via `POST /api/mills/backlog` with `MaxConcurrentRuns=1` and observe which starts first. Unambiguous outcome: either dispatch respects an ordering signal, or it's FIFO/other and slice 1 must include a dispatcher change + migration.

**Failure mode if wrong**: the Warping panel reorders a list the mill ignores — steering theater. Every HUD investment downstream inherits the lie.

**Status**: not run

> Disconfirming search before declaring passed: check for a hardcoded ORDER BY in the dispatcher and for intake sources that bypass the backlog store (e.g. canary autopilot direct-starts).

## Appendix — Metaphor glossary (machinery → Mills mapping)

| Machinery | Textile function | Mills mapping | Status |
|---|---|---|---|
| Bale breaking / opening | loosen raw bales | raw demand ingestion (issues, TODOs) | partial (intake) |
| Blending | mix fiber sources | merge demand sources | proposed (F7) |
| **Carding** | untangle + align fiber into sliver | normalize raw demand into clean items | proposed (F7) |
| **Combing** | remove short fibers | value gate discarding low-value demand | proposed (F7) |
| Drawing frame | merge slivers, even them out | dedupe + normalize slice size | proposed (F7) |
| Roving | light twist, pre-spin | brief / pre-plan stub | ≈ council brief |
| **Spinning frames** (mule/ring) | fiber → yarn | model-selectable plan generation | proposed (F2) |
| Winding / creel | yarn onto bobbins, racked | slices ready, awaiting order | ≈ pending slices |
| **Warping / warp beam** | thread order + tension set pre-weave | ordered execution queue | proposed (F1) |
| Sizing / slashing | strengthen warp pre-weave | kill-test + acceptance-criteria hardening gate | proposed |
| Drawing-in (heddles) | thread warp through harness | bind slices to squads/agents | ≈ squads |
| Shuttle | carries weft across | spawn agent carrying work | in use (weaver/spawn) |
| Reed | beats weft uniformly into place | gates (diff size, scope, format) | in use (gates) |
| Selvage | self-finished edge, no unravel | path-policy / scope guardrails | in use |
| **Dobby** | simple geometric patterns | cheap parameterized stamps | proposed (F8) |
| **Jacquard cards** | punch-card programmed weave | executable multi-slice pattern programs | partial (advisory Pattern Loom) |
| Piecing | rejoin broken thread mid-spin | carry-forward diff on retry | in use (!876) |
| Doffing | remove full bobbins | zombie/spawn reaping, cleanup | in use (!859) |
| **Take-up motion** | auto-wind cloth, advance warp | merge-driven status reconciler | proposed (F3) |
| Tentering | hold cloth to shape while drying | post-merge deploy verification | proposed (F4) |
| Perching / burling / mending | inspect + repair cloth | audit swarm + fix loop | in use (audit) / mending proposed |
| Dyeing / calendering | finish for market | docs, changelog, release polish | proposed (F4) |
| Cloth hall | graded marketplace | finished-goods digest + operator grading | proposed (F6) |
| Overlooker / tackler | fixes jammed looms | jam detection + recovery recipes | proposed (F5); partial in runbooks |
| Line shaft + governor | distribute/regulate power | budget caps + adaptive policy | in use |
| Mill race | water supply powering everything | flexinfer/token capacity | implicit |

## Handoff

- If chosen → next step is: `plan-loom-core` (spec + slice plan for the Live Beam; slice 1 completion criterion = the kill-test above run live)
- Linked spec/plan doc (fill in once it exists): `<.loom/NNN-...md>`
