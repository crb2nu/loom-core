# HUD UX Issues — screenshot review (2026-04-17)

Captured alongside the agent-orchestration + context-mgmt wave. These are **distinct from Slice A** (running now) and should be folded into either Slice E (UX polish) or a standalone "HUD readout fixes" slice.

## Captured issues

### Issue U1. "Now" health counter contradicts footer
- **Where:** Now view top counter reads `3/42 HEALTH`; footer reads `42/42 healthy` (or `41/42` when one degrades); Runtime card reads `3 of 42 healthy — All reachable · daemon up · 3 proc`.
- **Root cause (likely):** the top counter is counting *running* servers (3) and labeling it "health". Running vs healthy are different states.
- **Fix:** relabel to "RUNNING" or compute healthy as `42 - failing`, not `running`. One-line in the summary derivation; one Svelte label change.

### Issue U2. Coordination shows "Attention 7 / Risk 7" chips with body text "No hotspots"
- **Where:** Now view → Surfaces → Coordination card.
- **Root cause:** two independent computations — chip counter vs. hotspot list — disagree on threshold.
- **Fix:** drive chip counts from the hotspot list (empty list → chips hidden), or show a "Show 7 at-risk" expander.

### Issue U3. Servers panel `<1ms` latency for idle servers
- **Where:** Infrastructure → Servers. 39/42 show `<1ms` in the LATENCY column despite being idle.
- **Root cause:** latency column renders raw last-observed value; for never-polled / idle servers the zero value lies.
- **Fix:** render `—` (em dash) when the server has no recent call (e.g. `lastCallAt > 60s ago` or `never`). Keep transport column as-is (that was Slice A from `.loom/85`).

### Issue U4. "Transport" column shipped but legend missing
- **Where:** Servers panel header.
- **Observation:** Slice A from `.loom/85` landed — all rows show `stdio`. Works, but there's no one-line legend explaining `stdio|ws|sse|unavailable`.
- **Fix:** tooltip on the column header; one static string.

### Issue U5. Fleet "0 tokens" card vs Memory Tiers ~79K tokens disagreement
- **Where:** Operations → Fleet.
- **Root cause:** the "0 TOKENS" card likely shows *current-session live* tokens while Memory Tiers show *cumulative persisted*. Same word, different scope.
- **Fix:** label the card "LIVE TOKENS" or "TODAY'S TOKENS"; keep Memory Tiers as-is. Or surface both — live + cumulative.

### Issue U6. Repeated "26 blocked" across three surfaces
- **Where:** Now → Attention Lanes, Now → Work Queue, Fleet → Tasks. Same number shown three times.
- **Fix:** keep one primary surface (Attention Lanes), de-emphasize the others (badge only, no duplicate headline).

### Issue U7. Weaver empty state missing a CTA
- **Where:** Infrastructure → Weaver → Recent Queries. "No data yet" with no action.
- **Fix:** add a "Run a test query" button that POSTs `weaver__query("status check")` with a dry-run flag, or links to the existing weaver CLI path in docs.

### Issue U8. Labs sandbox "TOKEN REQUIRED" banner with empty field
- **Where:** Labs → Sandbox.
- **Observation:** HUD is already authenticated (top-right), but the Operator Token field is empty and the panel is locked.
- **Fix:** if HUD session has an admin token, pre-populate the field (or remove the redundant prompt). Otherwise, link to the auth doc.

### Issue U9. agent_context 369 ms latency vs codebase_memory 3 ms
- **Where:** Servers panel.
- **Observation:** 100x gap. Likely the pool exhaustion story from `.loom/85 Issue B` — the fix landed, but throughput remains a hotspot because HUD monitors poll agent_context concurrently.
- **Fix:** not a UX issue per se — surface the hot-server hint in the HUD sidebar. Investigation belongs in a separate perf slice.

### Issue U10. Operations/Fleet "Grouped by root 12" but only 6 agents visible without scroll indicator
- **Where:** Operations → Fleet → Live Agents.
- **Fix:** add a scroll indicator or pagination count (`Showing 1–6 of 12`) at the bottom of the agent list.

## Severity + effort ranking

| # | Severity | Effort | Notes |
|---|---|---|---|
| U1 | high (trust-eroding contradiction) | XS | label + derivation fix |
| U2 | high (contradictory UI) | S | drive chip from list |
| U3 | medium (misleading) | S | conditional render |
| U5 | medium (ambiguous) | XS | rename chip |
| U6 | medium (noise) | S | consolidate |
| U10 | medium (discoverability) | XS | scroll affordance |
| U4 | low | XS | tooltip |
| U7 | low | XS | CTA button |
| U8 | low | S | pre-populate field |
| U9 | low (UX); high (perf) | M | separate perf slice |

## Placement in the wave

**Option A (recommended):** Create a new "Slice F — HUD readout fixes" dedicated to U1/U2/U3/U5/U6/U10 (the six high/medium items). Parallelizable with Slices B–E. All-frontend + light backend; no touch to agent-context or weaver.

**Option B:** Roll U1/U2/U3/U5/U6/U10 into Slice E (UX polish) alongside F9 file-claim overlay and F10 weaver auto-compose. Slice E grows from 2 to ~8 items.

Slice F is preferred because it keeps diff scope manageable and ships fast (all ~XS/S fixes).

## Sources

- User screenshots reviewed 2026-04-17
- `.loom/85-plan-hud-robustness-2026-04-12.md` (Slice A: Transport column landed; Issue B: pool exhaustion context for U9)
- `.loom/87-product-spec-agent-orchestration-context-next-2026-04-17.md` §5.F9 (adjacent UX work)
