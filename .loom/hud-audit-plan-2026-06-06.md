Looking at 54 verified findings, I'll group these into shippable slices ordered by leverage (impact × breadth / risk × effort). Two systemic root-causes dominate and produce the highest-leverage early wins: a theme-aliasing slice that fixes ~250 phantom-token refs in one additive PR, and a DataTable engine flip that fixes the mid-row floating defect across all consumers.

```
# HUD Audit Remediation Plan — 12 Slices, Highest Leverage First

## SLICE 1 — Theme aliasing: kill phantom tokens, kill foreign palettes [SYSTEMIC, ENGINE]
**Closes:** tokens-phantom-text-color-tokens-aggregate, tokens-phantom-color-vars, mills-and-livesessions-orphan-token-palette, tokens-mills-dual-muted-grey, tokens-alerts-spawn-detail-tailwind-palette, tokens-live-sessions-card-foreign-palette (partial), tokens-otel-status-section-dead-code, tokens-drawer-backdrop-magic-number (partial)
**Files:** internal/hud/frontend/src/lib/styles/theme.css (+12 lines), delete OTelStatusSection.svelte
**Why grouped:** ONE additive `:root` aliasing block remaps all 4 phantom token families (--text-*, --color-*, --surface-*, --accent-color-*, --bg-subtle, --danger) to canonical (--fg-*, --success/--error/--warning/--info, --bg-surface/--bg-elevated/--bg-tertiary). Zero source-file edits. ~250 phantom refs collapse to canonical palette in one commit. Mills section, LiveSessionsCard, AlertsPanel, SpawnDetailPanel, ConfirmDialog, WeaverPanel all resolve to the brand palette at once.
**Effort:** S | **Risk:** low (additive only, can't break anything that already worked)
**Verify:** `grep -oE '#(ef4444|f59e0b|22c55e|14161a|889|aab|667|3fb950|d29922|58a6ff|f85149)' dist/assets/*.css | wc -l` should drop from ~80 to single digits after rebuild. Also add `--scrim: rgba(6,12,16,0.6)` token same MR.
**Notes:** Must `make hud-frontend` + commit `dist/` (go:embed'd, CI doesn't rebuild).

---

## SLICE 2 — DataTable engine: vertical-align top + skeleton padding parity [SYSTEMIC, ENGINE]
**Closes:** datatable-default-valign-middle-systemic, presence-agents-tab-tall-cell-floats, servers-table-tall-cell-floats, tasks-blocked-by-tall-cell-floats, datatable-skeleton-row-padding-overrides-cell-padding, presence-evidence-cell-wrap-tall, a11y-data-table-sortable-th-no-focus-visible
**Files:** internal/hud/frontend/src/lib/components/shared/DataTable.svelte (flip vertical-align default + add `.sortable:focus-visible` + match skeleton-row padding), fleet/FleetTable.svelte (drop now-redundant override at L271-274, KEEP L279-286 agent-cell white-space override)
**Why grouped:** All four tables-alignment findings have the SAME root cause: `vertical-align: middle` hardcoded at DataTable.svelte:371. Flip to `top` for `.stable-layout` mode = fixes FleetTable, PresenceAgentsTab, ServersTable, TasksTableView in one component edit (the per-card overrides become redundant). Skeleton padding mismatch is in the same component. Sortable-th focus-visible is also in the same engine.
**Effort:** S | **Risk:** med (ANY DataTable consumer with intentional single-line vertical centering may visually regress — but bundle grep shows only 7 middle decls ship, all body-cell defaults)
**Verify:** Render harness: open Fleet, Presence/Agents, Servers, Tasks tabs side-by-side; all tall cells must align flush-top. `grep -c 'vertical-align:top' dist/assets/index-*.css` should jump from 2 to 1 (unscoped engine rule). `grep skeleton-row dist/assets/*.css | grep 'padding:var(--space-1)'` confirms parity.
**Notes:** REQUIRES REAL RENDER to confirm no consumer regression.

---

## SLICE 3 — Global focus-visible + button reset companion [SYSTEMIC, A11Y]
**Closes:** global-button-no-focus-visible, presence-tab-bar-no-focus-visible, lifecycle-zoom-btn-no-focus-visible, a11y-nav-tab-no-focus-visible, no-focus-visible-on-clickable-rows-and-tabs (button half), dispatch-table-sortable-headers-not-keyboard-focusable (CSS half)
**Files:** internal/hud/frontend/src/lib/styles/theme.css (one rule after L268 button reset), App.svelte (.nav-tab:focus-visible), shared/ViewShell.svelte (.view-tab:focus-visible)
**Why grouped:** Single global `button:focus-visible { outline: 2px solid var(--border-active); outline-offset: 2px; }` rule + 2 nav/view-tab additions covers .tab-btn, .filter-chip, .toggle-btn, .zoom-btn, .header-toggle, .wf-item, .chain-header, .tier-tab, .filter-chip, .domain-header, .squad-name-btn — every clickable surface mentioned across 4 separate findings.
**Effort:** S | **Risk:** low (browser default outline was already rendering; this just makes it themed/consistent)
**Verify:** Bundle grep `grep -oE 'focus-visible' dist/assets/index-*.css | wc -l` should jump from 6 to ≥14. Manual: Tab through Presence panel, Lifecycle, every Mills sub-panel — focus ring visible at every stop.

---

## SLICE 4 — EmptyState prop alias + Council row a11y [SYSTEMIC, CORRECTNESS]
**Closes:** emptystate-message-prop-typo, shuttle-weaver-emptystate-wrong-prop, a11y-council-row-not-keyboard-accessible, dispatch-table-sortable-headers-not-keyboard-focusable (JS half)
**Files:** shared/EmptyState.svelte (add `message` prop aliasing to `heading` for safety net), WeaverPanel.svelte:270,298 (rename to canonical `heading=`), Mills/CouncilPanel.svelte:187-188 (add role/tabindex/onkeydown, remove svelte-ignore), DispatchPanel.svelte:134 (4× sortable th: add tabindex/role/onkeydown/aria-sort)
**Why grouped:** Three small correctness fixes that ship together cleanly. EmptyState alias is a 1-line defensive prop addition that immediately surfaces the right strings on Weaver's 2 visible-today sites + the 4 dormant Alerts/Shuttle sites if they ever wire. CouncilPanel + DispatchPanel are both keyboard-inaccessible interactive surfaces with established repo patterns (BacklogPanel/DataTable) to copy from.
**Effort:** S | **Risk:** low
**Verify:** `grep -c '"No data yet"' dist/assets/index-*.js` should drop. Tab into Council expand rows and Dispatch sortable headers → Enter/Space toggles work.

---

## SLICE 5 — PanelShell error prop + plumb stale stores [SYSTEMIC, ENGINE]
**Closes:** panelshell-no-error-surface, topology-panel-no-loading-empty-error, memory-error-not-rendered, knowledge-error-buried-in-empty, workflows-error-banner-low-affordance, weaver-loading-blocks-error-banner, shuttle-policy-save-fails-silently, reasoning-no-loading-state, stream-timeline-no-error, infra-cards-tunnels-conflates-error-with-empty, request-metrics-card-renders-nothing-when-empty
**Files:** shared/PanelShell.svelte (add `error?` + `errorAction?` props + `.error-state` block; precedence loading>error>empty>children), MemoryPanel, KnowledgePanel, WorkflowsPanel, ReasoningPanel, WeaverPanel, ShuttlePanel, TopologyPanel, RequestMetricsCard, InfraCards (tunnels branch), StreamPanel, TimelinePanel, App.svelte (ConfirmDialog import migration); migrate CatalogPanel + AuditPanel off bespoke patterns; register timelineStore with stalenessStore
**Why grouped:** All 11 findings share root cause: stores set `.error` but panels never read it. PanelShell engine change unlocks 13 consumer fixes. Use ShuttlePanel toastStore plumbing in same pass since pattern is established (TasksPanel.svelte already follows it).
**Effort:** M | **Risk:** med (touches many panels; each consumer migration must preserve existing emptyMessage/emptyHint behavior)
**Verify:** Manual: kill loomd briefly, every panel surfaces error within ~30s instead of going stale-silent. Bundle grep `grep -c 'error-banner\\|error-state' dist/assets/*.css` increases.

---

## SLICE 6 — Reduced-motion + fg-dim contrast lift [SYSTEMIC, A11Y]
**Closes:** a11y-no-reduced-motion, a11y-fg-dim-fails-wcag-aa, a11y-border-focus-contrast
**Files:** internal/hud/frontend/src/lib/styles/theme.css (add `@media (prefers-reduced-motion: reduce)` global block; lift `--fg-dim: #355B66 → #7AA0AC`; lift `--fg-muted: #5C8A96 → #6FA0AD`; add `--focus-ring: var(--info)`; update DataTable/MetricCard/UnseenAboveChip to use --focus-ring)
**Why grouped:** Three a11y fixes that all live in theme.css token + one global media block. Single MR closes 3 WCAG violations (2.3.3 reduced motion, 1.4.3 AA contrast on 96 callsites, 1.4.11 non-text contrast on focus ring). No component code edits beyond the 3 focus-visible callsites for --focus-ring.
**Effort:** S | **Risk:** med (token color shift affects 96+ visual spots — must visually review against teal-palette neighbors so the new fg-dim/fg-muted stay perceptibly tiered)
**Verify:** `grep prefers-reduced-motion dist/assets/index-*.css` returns 1+ hit. macOS System Settings → Accessibility → Reduce Motion: pulse/breathe animations stop. WCAG ratio script: fg-dim vs bg-elevated ≥ 4.5:1.
**Notes:** REQUIRES VISUAL REVIEW after rebuild.

---

## SLICE 7 — Focus trap + dialog focus management [SYSTEMIC, A11Y]
**Closes:** a11y-modal-no-focus-mgmt, a11y-modal-backdrop-button-role, a11y-keyboard-help-presentation-role, a11y-detail-drawer-no-focus-trap, a11y-toast-no-live-region, a11y-icon-only-buttons-missing-labels, a11y-overlay-section-no-aria-expanded
**Files:** NEW src/lib/actions/focusTrap.ts (Svelte action: snapshot focus, focus first, Tab wrap, restore on destroy), widgets/Modal.svelte (drop backdrop role=button, add close btn aria-label, use:focusTrap), shared/ConfirmDialog.svelte (use:focusTrap), shared/action/AuditDrawer.svelte, Mills/PipelineRunDetail.svelte, App.svelte keyboard-help (drop role=presentation, focus on open, close btn), shared/DetailDrawer.svelte (extend with focusTrap), widgets/Toast.svelte (aria-live container + dismiss aria-label), widgets/ErrorBanner.svelte, PresencePanel/CatalogPanel/CreateTaskModal (aria-label sweep), OverlayShell.svelte (aria-expanded on 3 toggle buttons), CatalogPanel.svelte:240 (aria-expanded)
**Why grouped:** All dialog/modal a11y fixes converge on one shared focusTrap action. Doing them separately means 5 MRs touching the same files. Icon-only aria-label + aria-expanded sweeps fit because they touch the same dialog/menu surfaces.
**Effort:** M | **Risk:** med (focus-management has subtle browser quirks; needs vitest covering open/Tab-wrap/Esc/restore)
**Verify:** Vitest: focus moves in on open, Tab wraps, Esc closes, focus restored to trigger. Manual VO/NVDA: dialog announces title; toast announcements work; expanded states announce.

---

## SLICE 8 — Spawn telemetry tabs + spawn admin-disabled UX [SYSTEMIC, CORRECTNESS]
**Closes:** spawn-telemetry-tabs-bare-empty-no-error, spawn-rows-no-disabled-style-on-stop, memory-skeleton-vs-empty-conflation
**Files:** SpawnTelemetry/{Activity,Usage,Transcript,Tools,Files,Errors}Tab.svelte (replace bare `<div class="tab-empty">` with `<EmptyState compact />`; ActivityTab adds eventStore.connectionState branch; UsageTab adds labsAuthStore.hasToken three-state), SpawnList.svelte + SandboxLive.svelte (add `:disabled` styling for stop/start buttons), MemoryPanel.svelte:453 (guard empty branch with `&& memoryStore.lastUpdated && !memoryStore.loading`)
**Why grouped:** Three correctness gaps in adjacent surfaces (Spawn telemetry, Spawn list controls, Memory). EmptyState swap is mechanical. Disabled-style + loading-guard match codebase patterns already used elsewhere.
**Effort:** S | **Risk:** low
**Verify:** Spawn an agent without admin token → UsageTab clearly says "Add Labs admin token". Disabled Stop button shows not-allowed cursor + opacity. Switch between memory tiers → no "No items" flash.

---

## SLICE 9 — Mobile breakpoints: Modal, Memory, Mills tables, Audit/Council/Weaver rows [SYSTEMIC, RESPONSIVE]
**Closes:** responsive-modal-min-width-overflow, responsive-memory-no-media-queries, responsive-mills-audit-row-overflow, responsive-council-ensemble-row-overflow, responsive-mills-tables-no-stacked-mode, responsive-weaver-role-row-fixed-tracks, responsive-shuttle-policy-two-col, responsive-bulk-panels-missing-media (narrowed set)
**Files:** widgets/Modal.svelte:57-58 (min-width to `min(380px, calc(100vw - 32px))`), MemoryPanel.svelte (@media 800px/480px collapses), Mills/{Audit,Council,Pipelines,Backlog,Eval}Panel.svelte (wrap `.mills-table` in `.mills-table-wrap { overflow-x: auto }` + add @media row reflows for AuditPanel grid + CouncilPanel ensemble-row), WeaverPanel.svelte (@media 720px/480px on .role-row + .status-grid), ShuttlePanel.svelte (@media 640px on .policy-view), ContextHealthPanel.svelte, Mills/BacklogDetail.svelte
**Why grouped:** All 7 findings are "no @media query / fixed grid breaks on phone." Modal fix touches every modal consumer (15+) for free. Mills .mills-table-wrap pattern is one-line per panel and ships immediate relief while the longer DataTable migration is filed as a follow-up.
**Effort:** M | **Risk:** low (additive @media + overflow wrappers, no desktop change)
**Verify:** Chrome DevTools responsive 360×640 + 414×896: no horizontal page scroll on any panel. Modal renders inside viewport on iPhone SE.
**Notes:** REQUIRES REAL RENDER at target widths to confirm reflows look right.

---

## SLICE 10 — Legacy palette sweep: cyan-on-orange chrome + MetricCard badges + EntityGraph [SYSTEMIC, POLISH]
**Closes:** tokens-legacy-palette-shadow, presence-stale-cyan-palette-on-orange-accent, tokens-confirmdialog-duplicate-impl, tokens-radius-full-typo, tokens-toast-hardcoded-top, tokens-9px-below-scale
**Files:** shared/MetricCard.svelte:127-131 (rgba → --info-dim/--success-dim/--warning-dim/--error-dim/--accent-dim), widgets/AgentCard.svelte (rgba(129,240,254) → color-mix(--info); rgba(231,179,18) → var(--warning-dim); pick semantic lane for .btn-dispatch; 999px → var(--radius-full)), shared/ConfirmDialog.svelte (cyan rgba + scrim → tokens; consolidate widgets/ConfirmDialog.svelte → shared; delete widgets/), DataTable.svelte:313,396 (rgba → --info-dim), widgets/ErrorBanner.svelte:21,22,44 (rgba → --error-dim + color-mix border), widgets/EntityGraph.svelte:12-15 (move 10 hexes to token-derived ramp), presence/PresenceAgentsTab.svelte:676-715, SpawnDetailPanel.svelte:660-984, presence/{Diagnostics,Claims}Tab.svelte, widgets/Toast.svelte:38 (top: 52px → var(--header-height)), 9 callsites of font-size:9px → var(--text-2xs)
**Why grouped:** All rgba/hex literals using stale palette PLUS the duplicate ConfirmDialog implementation PLUS small token-hygiene wins that touch the same files. Net visual: chrome becomes coherently orange-accent on dark-teal across Fleet/Presence/Spawn/Mills KPI/Confirm dialogs.
**Effort:** M | **Risk:** med (8+ files; visually subtle but a wrong color-mix percentage looks off — review each touched component)
**Verify:** `grep -oE 'rgba\(129,\s*240|rgba\(231,\s*179|999px|#0187|#22b255' dist/assets/*.css` returns ZERO. Visual: AgentCard's Nudge/Dispatch buttons now read as accent-orange or info-cyan consistently (pick one lane).
**Notes:** REQUIRES VISUAL REVIEW after build.

---

## SLICE 11 — Lazy-panel error recovery + Mills typography rhythm [POLISH, MEDIUM]
**Closes:** panel-loading-failure-empty-state-dead-end, mills-pipelines-table-no-row-grouping-hierarchy, overview-tiny-uniform-typography-no-hierarchy, placeholder-glyph-mixed-vocabulary, ai-recommended-copy-and-system-nominal-tone, overview-card-chrome-inconsistency-bordered-vs-bare
**Files:** NEW shared/LazyPanel.svelte (wrapper with name + load prop + Reload button on catch), App.svelte (replace 12 `{:catch}` blocks with `<LazyPanel name="..." load={...}/>`), Mills/PipelinesPanel + BacklogPanel + EvalPanel (promote .subrun to rgba(...,0.12) + 2px --info-dim left border; bump .state pill to 0.8125rem/600), HeroSummary + InboxCard + InstrumentStrip (kill 9px font-sizes, promote one element per card to --text-base, drop uppercase from chip values), NEW lib/utils/format.ts `nullish()` helper, codemod `--- → —`, copy edits to RecommendationsSection/Overview hero/Mills idle, pick one Overview wrapper pattern (bordered cards or borderless sections — recommend borderless with eyebrow per InboxDeck)
**Why grouped:** Six polish findings that share the "operator feel" surface and need taste calls (no exact right answer). Worth bundling because each individually is debatable but together they materially lift the dashboard out of generic-AI-dashboard aesthetic.
**Effort:** M | **Risk:** low-med (mostly visual; copy edits subjective)
**Verify:** Render Overview side-by-side before/after; subjective approval. `grep -c '"---"\\|'\''---'\''' src/lib | wc -l` drops to 0. `grep -c '"Failed to load panel"' dist/assets/*.js` = 0.
**Notes:** REQUIRES VISUAL REVIEW + copy approval.

---

## SLICE 12 — Token-hygiene codemod (low-priority cleanup) [POLISH, LOW]
**Closes:** tokens-agentcard-px-soup, tokens-hardcoded-spacing-scale-noise, tokens-ad-hoc-rgba-vs-semantic-tokens, tokens-transition-hardcodes, presence-agents-tab-heartbeat-90px-clip-risk, global-scope-drop-fleet-table-already-fixed (doc only), global-scope-drop-other-usages-all-clean (doc only), global-scope-leak-none-found (doc only), global-scope-drop-bare-tag-selectors-clean (doc only)
**Files:** widgets/AgentCard.svelte (mechanical px→token), PresenceAgentsTab.svelte:236 (heartbeat width 90→108px), workspace-wide codemod px→tokens for padding/margin/gap/font-size/border-radius/transition where exact match; rgba(R,G,B,A) → color-mix() where RGB matches semantic token. NEW .agents/skills/svelte-global-scoping.md doc note. NEW stylelint rule banning hex literals as 2nd arg of var().
**Why grouped:** Pure follow-up debt — zero visible defects, all-mechanical. Lowest priority, ship after the high-leverage slices land. Lint rule prevents regression of phantom-token re-introduction.
**Effort:** L (codemod scope is large) | **Risk:** low
**Verify:** `grep -rE 'font-size:\s*1[0-3]px' src/lib/components/**/*.svelte | wc -l` drops materially. Lint rule fires on attempted hex-as-var-fallback.

---

# EXECUTION ORDER

Ship 1 → 2 → 3 → 4 in parallel-ish (each is small, low-risk, high-impact). Then 5 → 6 → 7 sequentially (each builds on the previous engine work). Then 8 → 9 → 10 (UX correctness + mobile + polish). Then 11 → 12 (polish + cleanup).

# REAL-RENDER GATES (cannot prove statically)

- Slice 2: verify no consumer regression from vertical-align flip (open Fleet, Presence, Servers, Tasks side-by-side)
- Slice 6: verify fg-dim/fg-muted lift still reads as a tiered hierarchy on every bg token
- Slice 9: verify mobile breakpoints at 360×640 + 414×896 + 768×1024
- Slice 10: verify cyan-vs-orange semantic lane choices on AgentCard buttons
- Slice 11: visual + copy approval on Overview rhythm + tone changes

# ASSUMPTIONS / OPEN QUESTIONS

- Slice 4 EmptyState aliasing keeps the dormant Alerts/Shuttle dead components alive for future wiring; if the call is to delete them, fold into Slice 4.
- Slice 10's .btn-dispatch semantic lane (info vs accent) needs a product-taste call before the MR.
- Slice 11 Overview chrome decision (bordered cards vs borderless sections) needs one taste call upfront.
```
