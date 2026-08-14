<script lang="ts">
  import { fleetStore } from '../stores/fleet.svelte.ts';
  import { taskStore } from '../stores/tasks.svelte.ts';
  import { spawnStore } from '../stores/spawn.svelte.ts';
  import { embedConfig } from '../stores/embedConfig.svelte.ts';
  import { views, overviewId } from '../stores/router.svelte.ts';
  import { focusTrap } from '../actions/focusTrap';

  interface PaletteItem {
    id: string;
    label: string;
    category: string;
    icon: string;
    entity_kind?: string;
    detail_id?: string;
    target_view?: string;
    target_sub_view?: string;
    score?: number;
  }

  let {
    open = $bindable(false),
    onclose = () => {},
    onselect = () => {},
  }: {
    open?: boolean;
    onclose?: () => void;
    onselect?: (item: PaletteItem) => void;
  } = $props();

  let query = $state('');
  let selectedIndex = $state(0);
  let inputEl = $state<HTMLInputElement | null>(null);
  let resultsEl = $state<HTMLElement | null>(null);

  const ENTITY_CAP = 30;

  // Panel entries are DERIVED from the router's view/sub-view definitions, not
  // hand-listed. The old hardcoded array covered 15 of the 37 nav-reachable
  // panels, carried invented labels that matched no tab in the UI, and still
  // advertised two retired ids ('pipelines', 'backlog'); every new panel
  // shipped since was simply unreachable from the palette. Deriving means the
  // palette can never drift from the nav again: add a sub-view, get an entry.
  //
  // Labels are "<View>: <Sub-view>" so the view name is fuzzy-searchable
  // (typing "mills" surfaces every Mills panel) while the visible text still
  // matches the tab an operator is looking for. Each item carries an explicit
  // target_view/target_sub_view, so routing is exact rather than relying on
  // legacy id aliasing. Filtered through the embed subset for the same reason
  // the nav is: an operator-subset HUD must not offer a hidden view.
  let panels = $derived.by<PaletteItem[]>(() => {
    const items: PaletteItem[] = [
      {
        id: overviewId,
        label: 'Now: Overview',
        category: 'Panels',
        icon: '\u25A3',
        target_view: overviewId,
      },
    ];
    for (const v of views) {
      if (!embedConfig.isViewAllowed(v.id)) continue;
      for (const sv of v.subViews) {
        if (!embedConfig.isSubViewAllowed(v.id, sv.id)) continue;
        items.push({
          id: sv.id,
          label: `${v.label}: ${sv.label}`,
          category: 'Panels',
          icon: v.icon,
          target_view: v.id,
          target_sub_view: sv.id,
        });
      }
    }
    return items;
  });

  const actions = [
    { id: 'create-task', label: 'Create Task...', category: 'Actions', icon: '\u2795' },
    { id: 'seed-entity', label: 'Seed Entity...', category: 'Actions', icon: '\u2B21' },
    { id: 'create-handoff', label: 'Create Handoff...', category: 'Actions', icon: '\u21C6' },
    { id: 'approve-workflow', label: 'Approve Workflow Step...', category: 'Actions', icon: '\u2713' },
    { id: 'reject-workflow', label: 'Reject Workflow Step...', category: 'Actions', icon: '\u2717' },
    { id: 'promote-memory', label: 'Promote Memory Item...', category: 'Actions', icon: '\u2191' },
    { id: 'demote-memory', label: 'Demote Memory Item...', category: 'Actions', icon: '\u2193' },
    { id: 'add-memory', label: 'Add Memory Item...', category: 'Actions', icon: '\u29BE' },
    { id: 'pause-stream', label: 'Toggle Stream Pause', category: 'Actions', icon: '\u23F8' },
    { id: 'toggle-scanlines', label: 'Toggle CRT Scanlines', category: 'Actions', icon: '\u2588' },
    { id: 'refresh-all', label: 'Refresh All Data', category: 'Actions', icon: '\u21BB' },
    { id: 'open-audit-drawer', label: 'Open Recent Actions', category: 'Actions', icon: '\u29C9' },
  ];

  // Recent entities (sessions, tasks, spawns). Pulled at palette-open time
  // from the existing stores so we don't pay for re-derivation on every
  // keystroke. Capped at ENTITY_CAP per kind to keep the fuzzy index cheap.
  let entityItems = $state<PaletteItem[]>([]);

  function truncate(str: string | undefined, n: number) {
    if (!str) return '';
    return str.length > n ? str.slice(0, n - 1) + '…' : str;
  }

  function collectEntities() {
    const items: PaletteItem[] = [];

    const sessions = fleetStore.sessions ?? [];
    for (let i = 0; i < Math.min(sessions.length, ENTITY_CAP); i++) {
      const s = sessions[i];
      if (!s?.id) continue;
      const label = truncate(s.description?.trim() || s.namespace || s.id, 80);
      items.push({
        id: `session:${s.id}`,
        label: `${label}  ·  ${s.agent || s.agent_id || '?'}`,
        category: 'Sessions',
        icon: '⧉',
        entity_kind: 'session',
        detail_id: s.id,
        target_view: 'fleet',
      });
    }

    const tasks = taskStore.tasks ?? [];
    for (let i = 0; i < Math.min(tasks.length, ENTITY_CAP); i++) {
      const t = tasks[i];
      if (!t?.id) continue;
      const label = truncate(t.title || t.description || t.id, 80);
      items.push({
        id: `task:${t.id}`,
        label: `${label}  ·  ${t.status}/${t.priority}`,
        category: 'Tasks',
        icon: '☑',
        entity_kind: 'task',
        detail_id: t.id,
        target_view: 'tasks',
      });
    }

    const spawns = spawnStore.spawns ?? [];
    for (let i = 0; i < Math.min(spawns.length, ENTITY_CAP); i++) {
      const sp = spawns[i];
      if (!sp?.spawn_id) continue;
      const label = truncate(sp.request?.task_description || sp.spawn_id, 80);
      items.push({
        id: `spawn:${sp.spawn_id}`,
        label: `${label}  ·  ${sp.status}`,
        category: 'Spawns',
        icon: '❖',
        entity_kind: 'spawn',
        detail_id: sp.spawn_id,
        target_view: 'spawn',
      });
    }

    return items;
  }

  let allItems = $derived([...panels, ...actions, ...entityItems]);

  function fuzzyMatch(str: string, query: string) {
    const lower = str.toLowerCase();
    const q = query.toLowerCase();
    let qi = 0;
    for (let i = 0; i < lower.length && qi < q.length; i++) {
      if (lower[i] === q[qi]) qi++;
    }
    return qi === q.length;
  }

  function fuzzyScore(str: string, query: string) {
    const lower = str.toLowerCase();
    const q = query.toLowerCase();
    let score = 0;
    let qi = 0;
    let lastMatch = -1;
    for (let i = 0; i < lower.length && qi < q.length; i++) {
      if (lower[i] === q[qi]) {
        score += 10;
        // Bonus for consecutive matches
        if (lastMatch === i - 1) score += 5;
        // Bonus for matching at word boundary
        if (i === 0 || lower[i - 1] === ' ' || lower[i - 1] === '-') score += 3;
        lastMatch = i;
        qi++;
      }
    }
    return qi === q.length ? score : 0;
  }

  let filtered = $derived.by(() => {
    if (!query.trim()) return allItems;
    return allItems
      .map(item => ({ ...item, score: fuzzyScore(item.label, query) }))
      .filter(item => item.score > 0)
      .sort((a, b) => b.score - a.score);
  });

  let groupedResults = $derived.by(() => {
    const groups: Record<string, PaletteItem[]> = {};
    filtered.forEach(item => {
      if (!groups[item.category]) groups[item.category] = [];
      groups[item.category].push(item);
    });
    return Object.entries(groups);
  });

  // Reset on open + snapshot entities so the index is stable for this session.
  // focusTrap on .palette-container owns focus-in and focus-restore.
  $effect(() => {
    if (open) {
      query = '';
      selectedIndex = 0;
      entityItems = collectEntities();
    }
  });

  // Clamp selectedIndex when results change
  $effect(() => {
    const max = filtered.length;
    if (selectedIndex >= max) {
      selectedIndex = Math.max(0, max - 1);
    }
  });

  // Keep the arrow-key selection inside the scrolled results box — past ~8
  // presses the highlight otherwise moves off-screen and navigation goes blind.
  $effect(() => {
    const el = resultsEl?.querySelector(`[data-idx="${selectedIndex}"]`);
    el?.scrollIntoView({ block: 'nearest' });
  });

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      e.preventDefault();
      close();
      return;
    }

    if (e.key === 'ArrowDown') {
      e.preventDefault();
      selectedIndex = Math.min(selectedIndex + 1, filtered.length - 1);
      return;
    }

    if (e.key === 'ArrowUp') {
      e.preventDefault();
      selectedIndex = Math.max(selectedIndex - 1, 0);
      return;
    }

    if (e.key === 'Enter') {
      e.preventDefault();
      const item = filtered[selectedIndex];
      if (item) {
        onselect(item);
        close();
      }
      return;
    }
  }

  function close() {
    open = false;
    onclose();
  }

  function handleBackdropClick(e: MouseEvent) {
    if (e.target === e.currentTarget) {
      close();
    }
  }

  function selectItem(item: PaletteItem) {
    onselect(item);
    close();
  }

  // Global keyboard listener. Cmd+K and Cmd+P both open this palette; the
  // two shortcuts coexist with the legacy Cmd+K-only muscle memory while
  // adding Cmd+P for users coming from Cursor / IntelliJ-style "go to
  // anything" conventions.
  function handleGlobalKeydown(e: KeyboardEvent) {
    if (!(e.metaKey || e.ctrlKey)) return;
    if (e.key === 'k' || e.key === 'p') {
      e.preventDefault();
      open = !open;
      if (!open) onclose();
    }
  }

  $effect(() => {
    document.addEventListener('keydown', handleGlobalKeydown);
    return () => document.removeEventListener('keydown', handleGlobalKeydown);
  });

  // Track flat index across groups
  function flatIndex(groupIdx: number, itemIdx: number) {
    let idx = 0;
    const groups = groupedResults;
    for (let g = 0; g < groupIdx; g++) {
      idx += groups[g][1].length;
    }
    return idx + itemIdx;
  }
</script>

{#if open}
  <!-- Two comments, not one: as the first child of a block Svelte 5 honours only
       the first code in a svelte-ignore list. -->
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <!-- svelte-ignore a11y_click_events_have_key_events -->
  <div class="palette-backdrop" onclick={handleBackdropClick}>
    <div
      class="palette-container"
      role="dialog"
      aria-modal="true"
      aria-label="Command palette"
      use:focusTrap
    >
      <div class="palette-input-wrap">
        <span class="palette-icon">&#9906;</span>
        <!-- handleKeydown lives only here: on the backdrop it also fired for a
             Tab-focused result, so Enter on the 6th result activated the 1st. -->
        <input
          bind:this={inputEl}
          type="text"
          class="palette-input"
          placeholder="Type a command or search..."
          bind:value={query}
          onkeydown={handleKeydown}
          role="combobox"
          aria-expanded={true}
          aria-controls="palette-listbox"
          aria-activedescendant={`palette-opt-${selectedIndex}`}
        />
        <kbd class="palette-kbd">ESC</kbd>
      </div>

      <div class="palette-results" id="palette-listbox" role="listbox" bind:this={resultsEl}>
        {#if filtered.length === 0}
          <div class="palette-empty">
            <span class="text-muted">No results found</span>
          </div>
        {:else}
          {#each groupedResults as [category, items], gi (category)}
            <div class="palette-group">
              <div class="palette-group-label">{category}</div>
              {#each items as item, ii (item.id)}
                <button
                  class="palette-item"
                  class:palette-item-selected={selectedIndex === flatIndex(gi, ii)}
                  onclick={() => selectItem(item)}
                  onmouseenter={() => selectedIndex = flatIndex(gi, ii)}
                  data-idx={flatIndex(gi, ii)}
                  id={`palette-opt-${flatIndex(gi, ii)}`}
                  role="option"
                  aria-selected={selectedIndex === flatIndex(gi, ii)}
                  tabindex="-1"
                >
                  <span class="palette-item-icon">{item.icon}</span>
                  <span class="palette-item-label">{item.label}</span>
                  {#if item.category === 'Panels'}
                    <kbd class="palette-item-hint">{item.id}</kbd>
                  {/if}
                </button>
              {/each}
            </div>
          {/each}
        {/if}
      </div>
    </div>
  </div>
{/if}

<style>
  .palette-backdrop {
    position: fixed;
    inset: 0;
    z-index: 1000;
    display: flex;
    align-items: flex-start;
    justify-content: center;
    padding-top: 15vh;
    background: rgba(0, 0, 0, 0.6);
    backdrop-filter: blur(4px);
    -webkit-backdrop-filter: blur(4px);
  }

  .palette-container {
    width: 560px;
    max-width: 90vw;
    max-height: 60vh;
    background: var(--bg-secondary);
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
    box-shadow:
      0 16px 48px rgba(0, 0, 0, 0.5),
      0 0 0 1px rgba(var(--info-rgb), var(--opacity-light));
    overflow: hidden;
    display: flex;
    flex-direction: column;
    animation: paletteSlideIn 0.12s ease-out;
  }

  @keyframes paletteSlideIn {
    from {
      opacity: 0;
      transform: translateY(-8px) scale(0.98);
    }
    to {
      opacity: 1;
      transform: translateY(0) scale(1);
    }
  }

  .palette-input-wrap {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 14px 16px;
    border-bottom: 1px solid var(--border);
  }

  .palette-icon {
    font-size: 18px;
    color: var(--fg-muted);
    flex-shrink: 0;
  }

  .palette-input {
    flex: 1;
    font-family: var(--font-sans);
    font-size: 16px;
    background: transparent;
    border: none;
    color: var(--fg-primary);
    outline: none;
    padding: 0;
  }

  .palette-input::placeholder {
    color: var(--fg-muted);
  }

  .palette-kbd {
    font-family: var(--font-mono);
    font-size: 10px;
    padding: 2px 6px;
    background: var(--bg-tertiary);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-muted);
    flex-shrink: 0;
  }

  .palette-results {
    overflow-y: auto;
    max-height: calc(60vh - 60px);
    padding: 6px 0;
  }

  .palette-empty {
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 24px;
  }

  .palette-group {
    padding: 0;
  }

  .palette-group-label {
    font-size: 10px;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.5px;
    color: var(--fg-muted);
    padding: 8px 16px 4px;
  }

  .palette-item {
    display: flex;
    align-items: center;
    gap: 10px;
    width: 100%;
    padding: 8px 16px;
    text-align: left;
    font-size: 13px;
    color: var(--fg-primary);
    cursor: pointer;
    border: none;
    background: transparent;
    transition: background 0.08s;
  }

  .palette-item:hover {
    background: var(--bg-tertiary);
  }

  /* Selector raised over .palette-item:hover so the highlight wins on
     specificity instead of !important, and tinted from the same --info token
     the rest of the app selects with. */
  .palette-item.palette-item-selected {
    background: color-mix(in srgb, var(--info) 12%, transparent);
    color: var(--info);
  }

  .palette-item-icon {
    width: 20px;
    text-align: center;
    font-size: 14px;
    flex-shrink: 0;
    opacity: 0.7;
  }

  .palette-item-label {
    flex: 1;
  }

  .palette-item-hint {
    font-family: var(--font-mono);
    font-size: 10px;
    padding: 1px 5px;
    background: var(--bg-tertiary);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-muted);
    flex-shrink: 0;
  }
</style>
