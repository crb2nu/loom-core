# Brainstorm: correct + thorough fix for the agent_session_list timeout cascade

**Date**: 2026-06-03
**Triggered by**: "fix the flaky mcps" → the live fix (loomd restart + prune 875→29 + a 6h launchd auto-prune) suppressed the symptom. User wants the actual root cause fixed, not retention-band-aided.
**Constraints noted**: local-first daemon (`loomd`, launchd-supervised); session store is Qdrant-backed (`mcp-agent-context`); `--hub-prefer` is a deliberate 2026-05-25 choice for cross-host fleet visibility; commit `a4e84836` already shipped a partial fix; an event-bus thread already exists in-flight (branches: `plan/event-bus-spectator`, `feat/eventbus-backpressure`, `feat/session-lifecycle-events`, `feat/wire-publisher-to-eventbus`).

## Defect chain (code-grounded)

1. **Inflow** — CLI lifecycle hooks create one session row per agent invocation; most are empty (`entry_count:0`). 875 accumulated in ~2 days of fleet churn.
2. **No retention at source** — nothing inside `mcp-agent-context` reaps terminal (`ended`/`summarized`) sessions.
3. **O(N) query** — `pkg/agentcontext/svc_sessions_list.go:98` `ScrollPoints(filter, sessionListScrollCap=10000)` scrolls *all* matching points before `sort.Slice` + truncate. `a4e84836` moved the per-point recompute *after* truncate (so recompute is O(limit)) but the **scroll + deserialize is still O(matching points)**; the unfiltered call is O(total).
4. **Hot-loop consumer** — `internal/hud/shuttle` + a context-health monitor poll `agent_session_list` every few seconds against the daemon's ~3s upstream recv budget (`internal/daemon/callpipeline_*`).
5. **Routing amplifier** — `--hub-prefer` routes hub-first; a wedged hub WS transport → `muxstdio: transport closed` → reconnect → 30s `preferHubBackoff` (`internal/daemon/daemon.go:135`). The local stdio amplification is gated on `muxStdio`, which **defaults ON** (`internal/daemon/daemon_new.go:147`), so the cross-server collateral observed was on the **hub** path.
6. **Lock contention** — per-server call lock (`LOOM_DAEMON_CALL_LOCK_TIMEOUT=15s`) serializes `agent_context`, so one slow `session_list` blocks sibling `agent_context` calls.

Net: a slow O(N) read, polled hot, over a flaky hub hop, behind a per-server lock = unrelated tools (gitlab, devbox) intermittently drop.

## Phase 1 — Framings

### F1 — Reap at the source (lifecycle-correct sessions)
The store is a landfill: hooks mint a session per invocation and never close the empties. Fix the lifecycle — don't persist a row for a zero-entry hook ping (or collapse it on `session-end` with no entries), and have `mcp-agent-context` own an internal reaper. The external launchd prune becomes a redundant backstop.
- **Bet**: Most of the 875 are junk; if junk is never written (or self-reaps), `N` stays small and every downstream symptom evaporates.
- **Risk**: Presence/HUD may depend on session rows existing for short-lived agents; changing creation/close semantics could blank rosters or drop telemetry that keys off session IDs.

### F2 — Make the list a bounded query (kill the O(N) scroll)
The true residual root cause is the scroll. Maintain a secondary index so `list(status, limit)` and `list(active)` return top-N **without scanning all points** — e.g. a Redis ZSET keyed by `started_at` per status, or a small relational index — and have the service read the index, hydrating only `limit` payloads.
- **Bet**: If the query is genuinely count-independent, growth is *harmless* forever — no pruning, no polling tuning, no routing changes needed.
- **Risk**: Qdrant scroll isn't built for sorted pagination; this adds a second source of truth (index ↔ store drift), and a new storage surface to keep consistent.

### F3 — Push, don't poll (event-driven fleet projection)
The HUD should never call `agent_session_list` on a loop. Subscribe to session lifecycle events (bus already half-built), maintain an in-memory fleet projection updated incrementally, and serve the HUD from that. `list` reverts to a rare, cold-path query.
- **Bet**: Removing the hot heavy read entirely is the only fix that scales to an arbitrarily large fleet *and* arbitrarily large history.
- **Risk**: Biggest lift; bus isn't wired end-to-end; needs cold-start snapshot+replay and careful eventual-consistency handling — easy to ship a projection that silently drifts.

### F4 — Contain the blast radius (isolation + per-tool budgets)
Accept that some reads are slow; guarantee a slow/timed-out call on one server can **never** drop in-flight calls on another. Give `session_list` its own longer recv budget, ensure per-server transport independence on both local and hub paths, and shorten/relax the per-server lock so a slow read doesn't block siblings.
- **Bet**: Even if `session_list` is occasionally slow, "flaky MCPs" stops being a user-visible thing — the symptom is the cross-server collateral, not the slowness.
- **Risk**: Pure masking. Growth continues; one day the slow read is slow enough to matter on its own path (HUD goes stale) and we've only deferred the reckoning.

### F5 — Right-size routing (don't send cheap local reads over the hub)
The amplifier is `--hub-prefer` shipping `agent_context` reads + health probes across a WS hop that, when wedged, melts down with 30s backoff. Classify calls: local-cheap reads/health stay local; reserve hub routing for calls that genuinely need cross-host reach.
- **Bet**: The cross-server cascade only happens *because* local reads ride the flaky hub; cut that and a wedged hub stops taking down gitlab/devbox.
- **Risk**: `--hub-prefer` exists for cross-host fleet visibility; narrowing it may shrink the roster/telemetry completeness the operator added it for.

### F6 — Native TTL/retention in the store
Replace the external launchd cron with retention owned by `mcp-agent-context`: a reaper goroutine (or Qdrant payload TTL) that deletes terminal sessions past a window, plus a hard cap on live rows. Self-healing, no host-level dependency that breaks if the plist is removed.
- **Bet**: Data lifecycle belongs in the service, not in a Mac launchd job nobody remembers; co-locating it makes correctness durable across reinstalls.
- **Risk**: Still O(N) list *between* reaps if churn spikes faster than the reaper; TTL semantics get awkward for "summarized but still valuable" sessions.

### F7 — Hot/cold split (active working-set vs archive)
Two stores: a tiny "live" set (active + very recent) the HUD reads, and an "archive" for ended sessions queried rarely and paginated. `list(active)` is structurally tiny regardless of total history.
- **Bet**: The HUD's hot path becomes bounded by *concurrency* (how many agents are live now, ~30), never by *history* (how many ever ran).
- **Risk**: Dual-write complexity; recall queries must span both tiers; needs a migration and a mover that promotes/demotes rows.

### F8 — Tame the consumer (adaptive, degrade-safe pollers)
The monitors are too aggressive: tight interval, tight budget, and they treat a read timeout as transport-fatal (triggering reconnect). Make them adaptive (back off under load), force `light` + `active` projections always, cache last-good, and decouple a monitor read timeout from any upstream reconnect.
- **Bet**: The monitors are self-DOSing the daemon; a polite consumer stops manufacturing the reconnect storms even when the store is large.
- **Risk**: Introduces HUD staleness; doesn't help legitimate non-monitor callers of `session_list`; tuning is fiddly and regresses silently.

## Phase 2 — Cross-Pollinations & Tensions

### Combinations
- **F1 + F6** — Fix inflow *and* outflow inside the service: don't write empty sessions, and reap terminal ones natively. Addresses growth from both ends with zero external dependency. This is the smallest change that is genuinely "root cause," not symptom.
- **F7 + F3** — The "live" tier *is* an event-fed in-memory projection; the archive is the durable store, queried on demand. Together they're the textbook-correct architecture: push for hot state, paginated pull for cold history. Most correct, highest cost.
- **F2 + F4** — Bound the query *and* isolate the path: the index makes `list` fast; isolation makes the rare slow case harmless. Defense-in-depth that doesn't require touching the consumer or the data lifecycle.

### Tensions
- **F2/F3/F7 (fix the slowness) vs. F4/F5/F8 (make slowness harmless)** — This is the axis. The user asked for *root cause*: the cascade is downstream of an **O(N) read on a growing collection**, so the root lives in the data/query layer (F1/F2/F6/F7). The transport/routing/consumer layer (F4/F5/F8) is what *converted* a slow read into "flaky MCPs" — real, but the amplifier, not the cause. A thorough fix treats the cause and at least defangs the amplifier.
- **F1 vs. F8** — Both blame "too much churn," opposite ends: F1 says stop *creating* junk sessions; F8 says stop *reading* them so often. F1 is structurally cleaner (less data is always better); F8 is lower-risk but leaves the landfill.

## Phase 3 — Convergence

### Recommended: F2 + F1 (bounded query as the root-cause fix, fed by killing junk inflow), with F5 as the amplifier patch
The single most-correct change is **making `agent_session_list` count-independent for real** — finish what `a4e84836` started by removing the O(N) scroll, not just the O(N) recompute. A maintained per-status index (Redis ZSET by `started_at`, hydrate only `limit` payloads) turns both `list(active)` and `list(status,limit)` into O(limit) regardless of total history; growth becomes permanently harmless and the launchd prune drops to pure hygiene. Pair with **F1** (don't persist zero-entry hook sessions) to stop manufacturing the landfill in the first place, and a **narrow F5** change (keep `agent_context` reads + health probes on the local path under `--hub-prefer`) so a future hub wedge can't melt unrelated servers. This trio is proportionate: it fixes the cause (query + inflow), defangs the amplifier (routing), and avoids the large event-bus rewrite.

### Runner-up: F3 + F7 (event-driven hot/cold)
If the team is already committing to the event-bus thread (the branches suggest momentum), the projection-plus-archive architecture is *more* correct long-term and removes the hot read entirely. It tips the other way the moment session lifecycle events are reliably on the bus with snapshot/replay — at that point building an index for a query you're about to stop making would be wasted work.

### Open question
**Is the Qdrant collection the committed system of record for sessions, or is moving session metadata to an indexed store (Redis/SQLite) on the table?** The F2 implementation forks here: if Qdrant stays, F2 means a *side index* with drift-reconciliation; if session metadata can live in a sorted/indexed store, `list(status ORDER BY started_at LIMIT n)` is native and F2 is trivial. This single answer decides whether the recommended path is a half-day or a multi-day slice.

## Riskiest assumption + kill-test

**Load-bearing assumption**: The dominant cost of `agent_session_list` at N≈875 is the `ScrollPoints(..., sessionListScrollCap=10000)` scroll + payload deserialize in `pkg/agentcontext/svc_sessions_list.go` — i.e. an index that returns only `limit` IDs and hydrates only `limit` payloads would bring a cold (non-`light`, unfiltered) call back under the daemon's 3s recv budget at N≥1000.

**Kill test** (≤30 min, on the live local daemon):
1. Re-seed the store to ~1000+ ended sessions (or wait for fleet churn / temporarily disable the new auto-prune).
2. Time the existing cold path: `loom agent session-list --status "" --limit 20` (unfiltered) — confirm it’s ≥3s / times out, and capture a CPU/profile or add a temporary timer around `ScrollPoints` vs the recompute to attribute the cost.
3. Prototype the index read (even a throwaway: scroll IDs+started_at only with `withPayload=false`, sort, hydrate 20) and time it at the same N.
4. **Pass** = prototype returns in <500ms at N≥1000 *and* the attribution shows scroll/deserialize (not recompute, not Qdrant latency floor) was the dominant term. **Fail** = the cost is the Qdrant round-trip floor or recompute regardless of payload, which would point at F3/F7 (stop making the call) over F2 (make the call cheap).

**Failure mode if wrong**: We build a secondary index and reconciliation machinery, but the latency floor is Qdrant round-trip or connection overhead unrelated to payload size — so `list` stays slow at scale and we've added a drift-prone index for nothing. The honest fix would then be F3/F7 (don't make the hot call) or moving sessions off Qdrant entirely.

**Status**: FAILED (assumption refuted) 2026-06-03 — see Kill-test result below.

> Pair with a disconfirming search before declaring passed: check whether `light:true` already skips the scroll/deserialize (it skips recompute — confirm whether it also reduces `withPayload`), and whether the HUD monitors already pass `light`/`active` (if they do, the residual slow caller is something else and F8/F4 move up).

## Kill-test result (2026-06-03) — F2 assumption REFUTED, root cause re-localized

Ran via code attribution rather than a synthetic benchmark — the call sites
answer it unambiguously:

- `agent_session_list` (`pkg/agentcontext/svc_sessions_list.go`) always
  `ScrollPoints(filter, 10000, withVector=false)` then deserializes; the
  `a4e84836` fix moved the per-session recompute (`countContextEntries`, a
  *Qdrant scroll per zero-entry session*) to *after* sort+truncate — bounding it
  to `limit`, **not to a constant**.
- Two bridge paths: `FleetSessions()`/`ActiveSessions()` pass `light=true`
  (skip recompute); `Sessions()` passes `light=false, limit=defaultSessionListLimit=1000`.
- **The hot-loop monitors that were timing out call the non-light `Sessions()`:**
  - `internal/hud/shuttle/monitor.go:37` — `bridge.Sessions()`, polls **3s**
  - `internal/hud/monitor/context_health.go:173` — `agent.Sessions()`, polls **5s**
  - Only `internal/hud/monitor/fleet.go` was migrated to the light/active path
    by `!578` — the fix was **never propagated** to these two.
- At N≥1000 mostly-empty sessions, each non-light call runs recompute up to
  `limit=1000` times = ~1000 serial Qdrant round-trips → blows the 3s budget
  every poll. The **light path stayed under budget at N=875** (fleet monitor
  wasn't the hard timeout) → **the O(N) scroll is NOT the dominant term**, so
  building an index (F2) would target the wrong bottleneck.
- Disconfirming check: `light=true` skips recompute but still scrolls+deserializes
  all matching points — so light helps via (a) no recompute fan-out and (b)
  smaller wire payload, not via avoiding the scroll. Consistent with the scroll
  being tolerable at realistic N.

**Revised recommendation: F8 (fix the consumers) + source-side recompute cap.**
1. Wire `shuttle/monitor.go` → `FleetSessions()` (light) and
   `context_health.go` → `ActiveSessions()` (active-only, light) — lossless
   (both only read status/agent_id/tokens; active tokens come from the live
   overlay). This is the `!578` fix finally propagated.
2. Cap the non-light recompute fan-out at the source so `limit=1000` can't mean
   1000 round-trips (recompute only the visible/active subset, or hard-cap at a
   small constant). The `a4e84836` "bounded by limit" is insufficient when
   `limit` is large.
3. Audit remaining non-light `Sessions()` callers (coordinator compressor/
   summarizer, mobile handlers, `domain/fleet/handler_session.go`,
   `bridge/agent_context.go:70`) — request-driven, lower urgency; cap limit
   and/or move to light where they don't need recomputed ended-session stats.

**F1 (don't persist empty sessions) and F2 (bounded-query index) are now
de-scoped from this fix** — they remain valid scaling/hygiene work for when the
fleet or history grows another order of magnitude, but they are not required to
stop the cascade. The launchd auto-prune stays as hygiene. This revised fix is
smaller, lower-risk, and more correct than the index.

## Handoff

- If chosen → next step is: `plan-loom-core` (slice the F2 index + F1 inflow guard; gate slice 1 on the kill-test above)
- Linked spec/plan doc (fill in once it exists): `<.loom/NNN-...md>`
- Related: `.loom/brainstorm-stdio-mux-2026-05-20.md` (transport-isolation lineage); memory `project_hud_no_agents_session_list_timeout`
