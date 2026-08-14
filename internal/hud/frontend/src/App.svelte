<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { router, views, overviewId, resolveSubViewKey } from './lib/stores/router.svelte.ts';
  import { fleetStore } from './lib/stores/fleet.svelte.ts';
  import { healthStore } from './lib/stores/health.svelte.ts';
  import { taskStore } from './lib/stores/tasks.svelte.ts';
  import { streamStore } from './lib/stores/stream.svelte.ts';
  import { eventStore } from './lib/stores/events.svelte.ts';
  import { chaptersStore } from './lib/stores/chapters.svelte.ts';
  import { overlayStore } from './lib/stores/overlay.svelte.ts';
  import { embedConfig } from './lib/stores/embedConfig.svelte.ts';
  import { actionStore } from './lib/stores/action.svelte.ts';
  import { labsAuthStore } from './lib/stores/labsAuth.svelte.ts';
  import { millsStore } from './lib/stores/mills.svelte.ts';
  import { millsSquadsStore } from './lib/stores/mills_squads.svelte.ts';
  import { millsAuditStore } from './lib/stores/mills_audit.svelte.ts';
  import { millsCrossRepoStore } from './lib/stores/mills_crossrepo.svelte.ts';
  import { formatTime as fmtTime } from './lib/utils/format.ts';
  import { focusTrap } from './lib/actions/focusTrap';
  import { dialogStore } from './lib/stores/dialogs.svelte.ts';
  import ViewShell from './lib/components/shared/ViewShell.svelte';
  import LazyPanel from './lib/components/shared/LazyPanel.svelte';
  import { panelLoaders } from './lib/panelRegistry.ts';
  import CommandPalette from './lib/components/CommandPalette.svelte';
  import ConnectionBanner from './lib/components/ConnectionBanner.svelte';
  import OverlayShell from './lib/components/OverlayShell.svelte';
  import Toast from './lib/widgets/Toast.svelte';
  import AuditDrawer from './lib/components/shared/action/AuditDrawer.svelte';
  import ActionToast from './lib/components/shared/action/ActionToast.svelte';

  // Structural subset of CommandPalette's item shape (its PaletteItem type is
  // component-local); covers the fields handleCommand reads.
  interface CommandItem {
    id: string;
    entity_kind?: string;
    target_view?: string;
    target_sub_view?: string;
    detail_id?: string;
  }

  let showCommandPalette = $state(false);
  let showKeyboardHelp = $state(false);

  onMount(() => {
    overlayStore.init();
    if (overlayStore.enabled) return;

    // Load embed/subset config first so the router guard can use it on the
    // very first navigate() triggered by hash parsing. Fire-and-forget —
    // the store defaults to "full" until the fetch resolves.
    embedConfig.load();
    // Detect Cloudflare Access SSO admin (tokenless) so the Labs token bar is
    // optional for operators signed in via Gmail. Fire-and-forget.
    labsAuthStore.checkAccess();
    router.init();
    eventStore.connect();
    chaptersStore.connect();
    // The status bar renders fleet-derived facts (connection word, server/
    // agent/session counts) on EVERY route, so the shell registers as a slow
    // polling owner. Owner semantics keep this from duplicating the heavy
    // /api/fleet fetch: only the FIRST owner triggers one (a view mounting
    // later just tightens the cadence), and the owner subscription also
    // activates the SSE hud.fleet snapshot feed. Without a shell owner, a
    // cold load on a non-fleet route (e.g. #mills/staff) left the footer
    // showing its initial state — "Disconnected · 0 servers" over a live
    // daemon.
    fleetStore.startPolling(120000, 'status-bar');
    healthStore.fetch();
  });

  // Filter the nav to the embed-subset allowlist (or pass through when
  // unrestricted). Reactive so the nav updates after `embedConfig.load`
  // resolves.
  let visibleViews = $derived(views.filter((v) => embedConfig.isViewAllowed(v.id)));

  // Keep the active view tab visible: the tab strip scrolls horizontally
  // (desktop overflow fade, mobile bottom bar), and a selection landed via
  // hash/palette can otherwise sit outside the scrolled viewport with no
  // visible active state anywhere. Re-run on resize — the strip's scroll
  // offset survives layout changes and can strand the active tab off-screen.
  function scrollActiveTabIntoView() {
    const active = document.querySelector('.nav-tabs .nav-tab.active');
    active?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  }
  $effect(() => {
    void router.view;
    scrollActiveTabIntoView();
  });

  // Prime the three Mills sub-stores when the operator enters the Mills
  // view so the sub-tab nav can render counts without waiting for each
  // panel to be visited. Each panel still runs its own 15s polling on
  // mount; this is a one-shot refresh per Mills-view entry, not a
  // perpetual poll loop.
  $effect(() => {
    if (router.view === 'mills') {
      void millsSquadsStore.refresh();
      void millsAuditStore.refresh();
      void millsCrossRepoStore.refresh();
    }
  });

  onDestroy(() => {
    fleetStore.stopPolling('status-bar');
    eventStore.disconnect();
  });

  function isVisibleElement(node: HTMLElement | null) {
    return !!node && !!(node.offsetWidth || node.offsetHeight || node.getClientRects?.().length);
  }

  function focusPrimaryPanelSearch() {
    const main = document.getElementById('main-content');
    if (!main) return false;
    const candidates = main.querySelectorAll('[data-panel-search="primary"], .panel-search-input');
    for (const candidate of candidates) {
      if (candidate instanceof HTMLInputElement && isVisibleElement(candidate)) {
        candidate.focus();
        candidate.select();
        return true;
      }
    }
    return false;
  }

  // Keyboard shortcuts — view switching + sub-view switching
  function handleKeydown(e: KeyboardEvent) {
    if (overlayStore.enabled) return;
    const target = e.target as HTMLElement | null;
    const tag = target?.tagName;
    const isInput =
      tag === 'INPUT' ||
      tag === 'TEXTAREA' ||
      tag === 'SELECT' ||
      target?.isContentEditable;

    if (e.key === 'Escape') {
      if (showCommandPalette) { showCommandPalette = false; return; }
      if (showKeyboardHelp) { showKeyboardHelp = false; return; }
      // While typing in an input, Escape belongs to the input (cancel/blur) —
      // don't yank the detail surface away and lose its state.
      if (isInput) return;
      // Clear detail if open
      if (router.detail) { router.back(); return; }
      return;
    }

    // A focus-trapped surface owns the keyboard. Escape (above) still dismisses,
    // but the bare-letter shortcuts below must not renavigate the page behind an
    // open modal.
    if (dialogStore.openCount > 0) return;

    // Cmd/Ctrl + F → focus search
    if ((e.metaKey || e.ctrlKey) && e.key === 'f' && !e.altKey) {
      if (focusPrimaryPanelSearch()) {
        e.preventDefault();
        return;
      }
    }

    if (!isInput && !e.metaKey && !e.ctrlKey && !e.altKey) {
      // ` or o → Overview
      if (e.key === '`' || e.key === 'o') {
        router.navigate(overviewId);
        return;
      }
      // / → focus search
      if (e.key === '/') {
        e.preventDefault();
        focusPrimaryPanelSearch();
        return;
      }
      // r → refresh
      if (e.key === 'r') {
        fleetStore.fetch();
        healthStore.fetch();
        return;
      }
      // ? → keyboard help
      if (e.key === '?') {
        showKeyboardHelp = !showKeyboardHelp;
        return;
      }

      // Digit → view switching. Matched on the DECLARED `key`, so the
      // shortcut a tab advertises is the shortcut that fires even if the
      // views array is reordered.
      const vdByKey = views.find((v) => v.key === e.key);
      if (vdByKey) {
        router.navigate(vdByKey.id);
        return;
      }

      // Letter → sub-view switching within the current view, resolved by the
      // sub-view's DECLARED key (never by its array position — that older
      // scheme silently sent every out-of-order view's keypresses to the
      // wrong panel and made the rendered <kbd> hints lies).
      const sv = resolveSubViewKey(router.currentViewDef, e.key);
      if (sv) {
        router.navigateSub(sv.id);
        return;
      }
    }
  }

  // Command palette handler.
  // Entity items (sessions/tasks/spawns) carry target_view + detail_id so
  // we land directly on the record's drawer instead of just the panel.
  // Navigation items are generated from the router's own view/sub-view
  // definitions and carry an explicit target_view (+ target_sub_view), so
  // they route exactly, with no reliance on legacy id aliasing.
  function handleCommand(item: CommandItem) {
    if (item.entity_kind && item.target_view && item.detail_id) {
      router.navigate(item.target_view, undefined, item.detail_id);
      return;
    }
    if (!item.entity_kind && item.target_view) {
      router.navigate(item.target_view, item.target_sub_view);
      return;
    }
    switch (item.id) {
      case 'refresh-all':
        fleetStore.fetch();
        healthStore.fetch();
        break;
      case 'pause-stream':
        streamStore.togglePause();
        break;
      case 'toggle-scanlines':
        document.body.classList.toggle('scanlines');
        break;
      case 'open-audit-drawer':
        actionStore.openDrawer();
        break;
      default:
        // Navigate: works for both view IDs and legacy panel IDs
        router.navigate(item.id);
        break;
    }
  }

  // Status bar derived values. Until the first fleet snapshot lands the
  // connection state is unknown, not down — render a neutral "Connecting"
  // instead of a red "Disconnected" over a daemon we simply haven't heard
  // from yet.
  let fleetReady = $derived(fleetStore.lastUpdated !== null);
  let daemonOnline = $derived(fleetStore.status.running);
  let serverCount = $derived(fleetStore.status.servers);
  // Numerator and denominator from the SAME store — pairing healthStore's
  // available count with fleetStore's server count rendered "42/0 healthy"
  // when only one of them had data.
  let healthTotal = $derived(healthStore.servers.length);
  let activeSessionCount = $derived(fleetStore.activeSessions.length);
  let liveAgentCount = $derived(fleetStore.liveAgentCount);
  let liveAgentSummary = $derived(fleetStore.unifiedSummary);
  let healthySrv = $derived(healthStore.healthyCount);
  let availableSrv = $derived(healthStore.availableCount);
  let degradedSrv = $derived(healthStore.degradedCount);
  let downSrv = $derived(healthStore.downCount);

  // Badge counts for nav tabs — keyed by view id (only a few views carry one).
  let badgeCounts = $derived<Record<string, number>>({
    agents: fleetStore.liveAgentCount,
    infra: healthStore.degradedCount + healthStore.downCount,
    tasks: taskStore.pendingCount + taskStore.inProgressCount,
  });
</script>

<svelte:window onkeydown={handleKeydown} onresize={scrollActiveTabIntoView} />

{#if overlayStore.enabled}
  <OverlayShell />
{:else}
<a class="skip-link" href="#main-content">Skip to content</a>
<div class="hud-shell">
  <!-- Top navigation bar -->
  <header class="nav-bar">
    <div class="nav-brand">
      <span class="nav-logo">{'\u25C8'}</span>
      <span class="nav-title">LOOM HUD</span>
    </div>

    <nav class="nav-tabs" aria-label="Main navigation">
      <!-- Overview tab -->
      <button
        class="nav-tab"
        class:active={router.view === overviewId}
        onclick={() => { router.navigate(overviewId); }}
        aria-current={router.view === overviewId ? 'page' : undefined}
        title="What needs attention now? (` or o)"
      >
        <span class="nav-tab-icon">{'\u25A3'}</span>
        <span class="nav-tab-label">Now</span>
        <kbd class="nav-tab-key">o</kbd>
      </button>

      <span class="nav-divider"></span>

      <!-- Grouped view tabs (filtered by embed subset; see Slice B5) -->
      {#each visibleViews as v}
        <button
          class="nav-tab"
          class:active={router.view === v.id}
          onclick={() => { router.navigate(v.id); }}
          aria-current={router.view === v.id ? 'page' : undefined}
          title="{v.label} ({v.key})"
        >
          <span class="nav-tab-icon">{v.icon}</span>
          <span class="nav-tab-label">{v.label}</span>
          {#if badgeCounts[v.id] > 0}
            <span class="nav-badge">{badgeCounts[v.id]}</span>
          {/if}
          <kbd class="nav-tab-key">{v.key}</kbd>
        </button>
      {/each}
    </nav>

    <div class="nav-actions">
      <button
        class="btn btn-ghost audit-trigger"
        data-state={actionStore.errorCount > 0 ? 'error' : (actionStore.pendingCount > 0 ? 'pending' : 'idle')}
        onclick={() => actionStore.openDrawer()}
        title="Recent Actions"
        aria-label="Open recent actions drawer"
      >
        {'\u29c9'}
        {#if actionStore.errorCount > 0}
          <span class="audit-badge error" aria-label="{actionStore.errorCount} failed actions">{actionStore.errorCount}</span>
        {:else if actionStore.pendingCount > 0}
          <span class="audit-badge pending" aria-label="{actionStore.pendingCount} pending actions">{actionStore.pendingCount}</span>
        {/if}
      </button>
      <button
        class="btn btn-ghost"
        onclick={() => { showCommandPalette = true; }}
        title="Command Palette (Cmd+K)"
        aria-label="Open command palette"
      >
        {'\u2318'}K
      </button>
    </div>
  </header>

  <ConnectionBanner />

  <!-- Main content area -->
  <main class="panel-area" id="main-content">
    {#key router.view}
      <div class="panel-enter">
        {#if router.view === overviewId}
          <LazyPanel loader={panelLoaders.overview} />
        {:else}
          {#key router.subView}
            {@const vd = router.currentViewDef}
            {@const enrichedSubViews = vd && vd.id === 'mills'
              ? vd.subViews.map((sv) => {
                  // Surface item counts as small badges next to the
                  // sub-tab label so an operator can see at a glance
                  // where activity lives without opening each panel.
                  // Every badge must equal the count its panel puts in
                  // its own header — the mill-floor four therefore read
                  // the same store getters those panels do, never a raw
                  // array length. Cross-repo uses inFlightCount because
                  // the runs array accumulates terminal-state history;
                  // total length would show a misleadingly large number
                  // on a Mills that's been running for a while. Audit
                  // and squads use total length to match the other
                  // five tabs.
                  switch (sv.id) {
                    case 'warps':      return { ...sv, count: millsStore.strungCount };
                    case 'shuttles':   return { ...sv, count: millsStore.activeShuttleCount };
                    case 'sparks':     return { ...sv, count: millsStore.escalatedRuns.length };
                    case 'bolts':      return { ...sv, count: millsStore.boltRuns.length };
                    case 'council':    return { ...sv, count: millsStore.councilRuns.length };
                    case 'eval':       return { ...sv, count: millsStore.evalScores.length };
                    case 'policy':     return { ...sv, count: millsStore.policyProposals.length };
                    case 'squads':     return { ...sv, count: millsSquadsStore.state.length };
                    case 'audit':      return { ...sv, count: millsAuditStore.state.length };
                    case 'cross-repo': return { ...sv, count: millsCrossRepoStore.inFlightCount };
                    default:           return sv;
                  }
                })
              : vd?.subViews}
            {#if vd}
              <ViewShell
                subViews={enrichedSubViews}
                activeSubView={router.subView}
                onSwitch={(id) => router.navigateSub(id)}
              >
                {#if router.subView === 'spawn'}
                  <LazyPanel loader={router.detail ? panelLoaders['spawn-detail'] : panelLoaders.spawn} />
                {:else if router.subView === 'plans' && router.detail?.includes('+')}
                  <!-- 2+ compared plan ids (id1+id2) → compare/merge editor;
                       a single-id detail (#tasks/plans/<id>) stays on the
                       normal PlansPanel board drawer. -->
                  <LazyPanel loader={panelLoaders['plans-compare']} />
                {:else if panelLoaders[router.subView]}
                  <LazyPanel loader={panelLoaders[router.subView]} />
                {/if}
              </ViewShell>
            {/if}
          {/key}
        {/if}
      </div>
    {/key}
  </main>

  <!-- Status bar -->
  <!-- No role="status" on the footer: it would make the whole subtree — including
       the HH:MM:SS timestamp below — a live region re-read on every poll. Only
       the connection word announces. -->
  <footer class="status-bar" aria-label="System status">
    <div class="status-bar-left">
      <span
        class="status-indicator"
        class:online={fleetReady && daemonOnline}
        class:offline={fleetReady && !daemonOnline}
        class:connecting={!fleetReady}
      ></span>
      <span class="status-text" role="status">{fleetReady ? (daemonOnline ? 'Connected' : 'Disconnected') : 'Connecting'}</span>
      {#if fleetReady}
        <span class="status-divider"></span>
        <span class="status-text">{serverCount} servers</span>
        {#if degradedSrv > 0}
          <span class="status-text" style="color: var(--warning);">({degradedSrv} degraded)</span>
        {/if}
        <span class="status-divider"></span>
        <span class="status-text">{liveAgentCount} live agent{liveAgentCount !== 1 ? 's' : ''}</span>
        <span class="status-divider"></span>
        <span class="status-text">
          {activeSessionCount} active session{activeSessionCount !== 1 ? 's' : ''}
          {#if liveAgentSummary.orphans > 0}
            <span style="color: var(--warning);"> · {liveAgentSummary.orphans} orphan{liveAgentSummary.orphans !== 1 ? 's' : ''}</span>
          {/if}
        </span>
      {/if}
    </div>
    <div class="status-bar-right">
      {#if healthTotal > 0}
        <span class="status-text text-muted">{availableSrv}/{healthTotal} healthy</span>
        {#if downSrv > 0}
          <span class="status-text" style="color: var(--error);">({downSrv} down)</span>
        {/if}
      {/if}
      <span class="status-divider"></span>
      <span class="status-text text-mono" aria-label="Last updated {fmtTime(fleetStore.lastUpdated)}">{fmtTime(fleetStore.lastUpdated)}</span>
    </div>
  </footer>

  <CommandPalette bind:open={showCommandPalette} onselect={handleCommand} />

  <!-- Keyboard shortcut help overlay -->
  {#if showKeyboardHelp}
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <!-- svelte-ignore a11y_click_events_have_key_events -->
    <div class="keyboard-help-overlay" onclick={() => { showKeyboardHelp = false; }}>
      <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
      <div class="keyboard-help" role="dialog" aria-label="Keyboard shortcuts" tabindex="-1" onclick={(e) => e.stopPropagation()} use:focusTrap>
        <button class="help-close" aria-label="Close" onclick={() => { showKeyboardHelp = false; }}>{'✕'}</button>
        <div class="help-title">Keyboard Shortcuts</div>
        <div class="help-grid">
          <div class="help-section">
            <div class="help-section-title">Views</div>
            <div class="help-row"><kbd>`</kbd> / <kbd>o</kbd> <span>Overview</span></div>
            {#each visibleViews as v}
              <div class="help-row"><kbd>{v.key}</kbd> <span>{v.label}</span></div>
            {/each}
          </div>
          <div class="help-section">
            <div class="help-section-title">Sub-views</div>
            <div class="help-row"><kbd>a</kbd>-<kbd>z</kbd> <span>Switch within view</span></div>
            <div class="help-section-title" style="margin-top: var(--space-3);">Actions</div>
            <div class="help-row"><kbd>/</kbd> <span>Focus search</span></div>
            <div class="help-row"><kbd>r</kbd> <span>Refresh data</span></div>
            <div class="help-row"><kbd>{'\u2318'}K</kbd> <span>Command palette</span></div>
          </div>
          <div class="help-section">
            <div class="help-section-title">General</div>
            <div class="help-row"><kbd>?</kbd> <span>Toggle this help</span></div>
            <div class="help-row"><kbd>Esc</kbd> <span>Close / back</span></div>
          </div>
        </div>
      </div>
    </div>
  {/if}

  <Toast />
  <ActionToast />
  <AuditDrawer />
</div>
{/if}

<style>
  .skip-link {
    position: absolute;
    top: -40px;
    left: 0;
    padding: 8px 16px;
    background: var(--accent);
    color: var(--bg-primary);
    font-weight: 600;
    font-size: 13px;
    z-index: 1000;
    transition: top 0.15s ease;
    border-radius: 0 0 var(--radius-sm) 0;
  }

  .skip-link:focus {
    top: 0;
  }

  .hud-shell {
    display: flex;
    flex-direction: column;
    height: 100vh;
    width: 100vw;
    overflow: hidden;
    background:
      linear-gradient(180deg, rgba(255, 255, 255, 0.015), transparent 16%),
      transparent;
  }

  /* ═══ Nav Bar ═══════════════════════════════════════════════ */

  .nav-bar {
    display: flex;
    align-items: center;
    height: var(--header-height);
    background:
      linear-gradient(180deg, rgba(255, 255, 255, 0.025), transparent 100%),
      color-mix(in srgb, var(--bg-secondary) 90%, black 10%);
    border-bottom: 1px solid var(--border);
    padding: 0 max(var(--content-gutter), var(--space-4));
    flex-shrink: 0;
    gap: var(--space-5);
    z-index: 100;
    position: relative;
    /* No backdrop-filter here: the background above is opaque so a blur has
       nothing to show — and a filtered element becomes the containing block
       for fixed descendants, which silently pinned the ≤800px bottom tab bar
       to the header instead of the viewport. */
  }

  /* Subtle bottom-edge glow */
  .nav-bar::after {
    content: '';
    position: absolute;
    bottom: 0;
    left: 10%;
    right: 10%;
    height: 1px;
    background: linear-gradient(
      90deg,
      transparent,
      rgba(0, 200, 255, 0.05) 30%,
      rgba(0, 200, 255, 0.08) 50%,
      rgba(0, 200, 255, 0.05) 70%,
      transparent
    );
  }

  .nav-brand {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-shrink: 0;
  }

  .nav-logo {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 26px;
    height: 26px;
    font-size: 15px;
    color: var(--accent);
    background: var(--accent-dim);
    border: 1px solid rgba(var(--accent-rgb), var(--opacity-medium));
    border-radius: var(--radius-sm);
    filter: drop-shadow(0 0 6px rgba(255, 107, 53, 0.2));
  }

  .nav-title {
    font-size: var(--text-xs);
    font-weight: 700;
    letter-spacing: 0.16em;
    color: var(--fg-secondary);
    text-transform: uppercase;
  }

  .nav-tabs {
    display: flex;
    gap: var(--space-1);
    flex: 1;
    min-width: 0;
    justify-content: flex-start;
    align-items: center;
    overflow-x: auto;
    scrollbar-width: none;
    /* Fade both edges so a clipped tab reads as "scrolls", not "broken" —
       28px on the right (usually over empty flex space), a subtle 8px on
       the left for when the strip is scrolled. scroll-padding keeps a
       scrollIntoView'd active tab clear of the fades. */
    mask-image: linear-gradient(90deg, transparent, black 8px, black calc(100% - 28px), transparent);
    scroll-padding-inline: 32px;
  }

  .nav-tabs::-webkit-scrollbar {
    display: none;
  }

  .nav-divider {
    width: 1px;
    height: 18px;
    background: var(--border);
    margin: 0 var(--space-2);
  }

  .nav-tab {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 8px 12px;
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    font-weight: 500;
    color: var(--fg-muted);
    transition: background var(--transition-fast),
                color var(--transition-fast),
                border-color var(--transition-fast),
                box-shadow var(--transition-fast);
    position: relative;
    cursor: pointer;
    background: none;
    border: 1px solid transparent;
    letter-spacing: var(--tracking-normal);
    white-space: nowrap;
    min-height: 36px;
  }

  .nav-tab:hover {
    background: rgba(var(--fg-rgb), 0.05);
    color: var(--fg-primary);
  }

  .nav-tab:focus-visible {
    outline: 2px solid var(--info);
    outline-offset: 2px;
    border-radius: var(--radius-sm);
  }

  /* Active = one signifier: an accent-tinted pill. (Previously the filled
     card + underline bar + drop shadow stacked three.) Label stays
     fg-primary for contrast; the tint and icon carry the state. */
  .nav-tab.active {
    background: var(--accent-dim);
    color: var(--fg-primary);
    font-weight: 600;
    border-color: rgba(var(--accent-rgb), var(--opacity-strong));
  }

  .nav-tab-icon {
    font-size: var(--text-sm);
    opacity: 0.72;
  }

  .nav-tab.active .nav-tab-icon {
    opacity: 1;
    color: var(--accent);
  }

  .nav-tab-label {
    font-weight: inherit;
  }

  .nav-badge {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    min-width: 16px;
    height: 16px;
    padding: 0 5px;
    font-size: 9px;
    font-family: var(--font-mono);
    font-weight: 700;
    line-height: 1;
    color: var(--bg-primary);
    background: var(--accent);
    border-radius: var(--radius-full);
    box-shadow: 0 0 8px var(--glow-accent);
  }

  /* Shortcut hints reserve their width but only surface on hover/focus (and
     on the active tab) — ten always-on chips were the loudest thing in the
     bar. Full listing stays one `?` away in the help overlay. */
  .nav-tab-key {
    font-size: 9px;
    font-family: var(--font-mono);
    color: var(--fg-muted);
    padding: 1px 4px;
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-xs);
    line-height: 1;
    background: rgba(255, 255, 255, 0.02);
    opacity: 0;
    transition: opacity var(--transition-fast);
  }

  .nav-tab:hover .nav-tab-key,
  .nav-tab:focus-visible .nav-tab-key,
  .nav-tab.active .nav-tab-key {
    opacity: 0.8;
  }

  .nav-actions {
    flex-shrink: 0;
  }

  .nav-actions .btn {
    font-family: var(--font-mono);
    font-size: 11px;
    color: var(--fg-secondary);
    padding: 6px 10px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
    background: rgba(255, 255, 255, 0.02);
    transition: color var(--transition-fast), border-color var(--transition-fast), background var(--transition-fast);
  }

  .nav-actions .btn:hover {
    color: var(--fg-primary);
    border-color: var(--border-focus);
    background: rgba(255, 255, 255, 0.04);
  }

  .audit-trigger {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    margin-right: 6px;
  }

  .audit-trigger[data-state='error'] {
    color: var(--error);
    border-color: var(--error-dim);
  }

  .audit-trigger[data-state='pending'] {
    color: var(--info);
    border-color: var(--info-dim);
  }

  .audit-badge {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    min-width: 16px;
    height: 16px;
    padding: 0 4px;
    border-radius: 8px;
    font-size: 9px;
    font-weight: 700;
    line-height: 1;
    font-family: var(--font-mono);
  }

  .audit-badge.error {
    background: var(--error-dim);
    color: var(--error);
  }

  .audit-badge.pending {
    background: var(--info-dim);
    color: var(--info);
  }

  /* ═══ Panel Area ════════════════════════════════════════════ */

  .panel-area {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    display: flex;
    flex-direction: column;
    padding: var(--space-4) var(--content-gutter) var(--content-gutter);
  }

  /* ═══ Status Bar ════════════════════════════════════════════ */

  .status-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    height: var(--statusbar-height);
    background: color-mix(in srgb, var(--bg-secondary) 92%, black 8%);
    border-top: 1px solid var(--border);
    padding: 0 max(var(--content-gutter), var(--space-4));
    flex-shrink: 0;
    z-index: 100;
  }

  .status-bar-left,
  .status-bar-right {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
    overflow: hidden;
  }

  .status-indicator {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    flex-shrink: 0;
  }

  .status-indicator.online {
    background: var(--success);
    box-shadow: 0 0 6px var(--success);
  }

  .status-indicator.offline {
    background: var(--error);
    box-shadow: 0 0 6px var(--error);
    animation: pulse 1.5s infinite;
  }

  .status-indicator.connecting {
    background: var(--fg-dim);
    animation: pulse 1.5s infinite;
  }

  .status-text {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    font-family: var(--font-mono);
    /* Segments truncate as a row (overflow on the flex parent) rather than
       wrapping to two lines inside the 32px bar on narrow screens. */
    white-space: nowrap;
  }

  .status-divider {
    width: 1px;
    height: 12px;
    background: var(--border);
  }

  /* ═══ Keyboard Help Overlay ═════════════════════════════════ */

  .keyboard-help-overlay {
    position: fixed;
    inset: 0;
    background: rgba(6, 12, 16, 0.8);
    backdrop-filter: blur(8px);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 1000;
    animation: fadeIn 0.15s ease-out;
  }

  .keyboard-help {
    background: var(--bg-secondary);
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
    padding: var(--space-6);
    max-width: 520px;
    width: 90%;
    box-shadow: var(--shadow-xl);
    position: relative;
    overflow: hidden;
  }

  /* Top-edge glow on modal */
  .keyboard-help::before {
    content: '';
    position: absolute;
    top: 0;
    left: 0;
    right: 0;
    height: 1px;
    background: linear-gradient(
      90deg,
      transparent 10%,
      rgba(0, 200, 255, 0.2) 50%,
      transparent 90%
    );
  }

  .help-close {
    position: absolute;
    top: var(--space-3);
    right: var(--space-3);
    font-size: var(--text-base);
    color: var(--fg-muted);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-sm);
    transition: color var(--transition-fast), background var(--transition-fast);
    z-index: 1;
  }

  .help-close:hover {
    color: var(--fg-primary);
    background: var(--bg-tertiary);
  }

  .help-title {
    font-size: var(--text-lg);
    font-weight: 700;
    color: var(--fg-primary);
    margin-bottom: var(--space-5);
    letter-spacing: var(--tracking-tight);
  }

  .help-grid {
    display: grid;
    grid-template-columns: 1fr 1fr 1fr;
    gap: var(--space-5);
  }

  .help-section-title {
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
    margin-bottom: var(--space-2);
  }

  .help-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-sm);
    color: var(--fg-secondary);
    margin-bottom: 6px;
  }

  .help-row kbd {
    font-family: var(--font-mono);
    font-size: 9px;
    padding: 2px 6px;
    border: 1px solid var(--border);
    border-radius: var(--radius-xs);
    background: var(--bg-tertiary);
    color: var(--fg-primary);
    min-width: 20px;
    text-align: center;
    font-weight: 500;
  }

  /* ═══ Responsive ════════════════════════════════════════════ */

  /* ≤800px — phone + small-tablet layout. Slice B5 of the HUD UX overhaul
     extended the existing 768px breakpoint to 800px and reflows the nav
     into a bottom-fixed tab bar (thumb-zone navigation), bumps tap targets
     to 44px, and leaves room above the bar so panel content doesn't sit
     underneath it. Tabs scroll horizontally when their natural width
     exceeds the viewport. */
  @media (max-width: 800px) {
    .hud-shell {
      /* Reserve space for the fixed bottom nav so the last row of panel
         content stays scrollable into view. 56px tab + 8px gap. */
      padding-bottom: calc(64px + env(safe-area-inset-bottom, 0px));
    }

    .nav-bar {
      /* Reflow: brand + actions stay top-aligned, tabs detach to bottom. */
      gap: var(--space-3);
      padding: 0 var(--space-3);
    }

    .nav-tabs {
      position: fixed;
      left: 0;
      right: 0;
      bottom: 0;
      z-index: 200;
      flex: 0 0 auto;
      width: 100%;
      gap: 2px;
      padding: 6px max(var(--space-2), env(safe-area-inset-left, 0px))
        calc(6px + env(safe-area-inset-bottom, 0px))
        max(var(--space-2), env(safe-area-inset-right, 0px));
      /* Real glass: a translucent fill so the backdrop blur has content to
         show through (the old opaque color-mix made the blur a no-op). */
      background: var(--glass-bg);
      border-top: 1px solid var(--border);
      backdrop-filter: blur(var(--glass-blur));
      overflow-x: auto;
      -webkit-overflow-scrolling: touch;
      scrollbar-width: none;
      justify-content: flex-start;
    }
    .nav-tabs::-webkit-scrollbar {
      display: none;
    }
    .nav-divider {
      display: none;
    }
    .nav-tab-key {
      display: none;
    }
    .nav-tab {
      min-height: 44px;
      min-width: 44px;
      flex-shrink: 0;
      padding: 6px 10px;
    }
    .status-bar-right {
      display: none;
    }
  }

  @media (max-width: 480px) {
    .nav-bar {
      padding: 0 var(--space-2);
      gap: var(--space-2);
    }
    .nav-title {
      display: none;
    }
    .nav-tab-label {
      display: none;
    }
    .nav-tab {
      padding: 6px var(--space-2);
    }
    .nav-tab-icon {
      font-size: var(--text-base);
      opacity: 1;
    }
    .nav-actions .btn {
      font-size: 10px;
      padding: 3px 6px;
    }
    .status-text {
      font-size: var(--text-xs);
    }

    .panel-area {
      padding: var(--space-2);
    }

    .help-grid {
      grid-template-columns: 1fr;
    }
  }
</style>
