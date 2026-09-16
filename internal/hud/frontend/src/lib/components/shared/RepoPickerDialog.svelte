<script lang="ts">
  /**
   * RepoPickerDialog — browse the operator's GitLab projects or GitHub repos
   * (most recently active first), see what Mills already knows about each,
   * and pick one to onboard. Listing is provider-backed through the HUD
   * daemon; nothing here mutates.
   */
  import { focusTrap } from '../../actions/focusTrap';
  import { forgeStore, ForgeNotConnectedError, type ForgeProvider, type ForgeRepo } from '../../stores/forge.svelte.ts';
  import { millsProjectsStore, readinessOf } from '../../stores/millsProjects.svelte.ts';
  import Badge from '../../widgets/Badge.svelte';
  import { relativeTime } from '../../utils/format.ts';

  interface Props {
    open: boolean;
    onClose: () => void;
    onPick: (repo: ForgeRepo) => void;
    initialProvider?: ForgeProvider;
  }
  let { open, onClose, onPick, initialProvider = 'gitlab' }: Props = $props();

  // Seeded from `initialProvider` each time the dialog opens (see the effect
  // below), so the prop's later values are honoured without capturing it here.
  let provider = $state<ForgeProvider>('gitlab');
  let query = $state('');
  let repos = $state<ForgeRepo[]>([]);
  let page = $state(1);
  let hasMore = $state(false);
  let loading = $state(false);
  let errorMsg = $state('');
  let connectHint = $state('');

  let debounce: ReturnType<typeof setTimeout> | undefined;
  let openedOnce = $state(false);

  $effect(() => {
    if (open && !openedOnce) {
      openedOnce = true;
      provider = initialProvider;
      void load(1, true);
    }
    if (!open) openedOnce = false;
  });

  async function load(nextPage: number, replace: boolean): Promise<void> {
    loading = true;
    errorMsg = '';
    connectHint = '';
    try {
      const res = await forgeStore.listRepos(provider, query, nextPage);
      repos = replace ? res.repos : [...repos, ...res.repos];
      page = res.page;
      hasMore = res.hasMore;
    } catch (e) {
      if (e instanceof ForgeNotConnectedError) {
        connectHint = e.connectHint;
        errorMsg = `${provider === 'gitlab' ? 'GitLab' : 'GitHub'} is not connected where loomd runs.`;
      } else {
        errorMsg = e instanceof Error ? e.message : String(e);
      }
      if (replace) repos = [];
    } finally {
      loading = false;
    }
  }

  function switchProvider(p: ForgeProvider): void {
    if (p === provider) return;
    provider = p;
    repos = [];
    void load(1, true);
  }

  function onSearch(e: Event): void {
    query = (e.target as HTMLInputElement).value;
    clearTimeout(debounce);
    debounce = setTimeout(() => void load(1, true), 250);
  }

  /** What Mills knows about a row. A GitHub repo is matched by name to its
   *  GitLab twin (the workspace mirrors one canonical GitLab project per name);
   *  the twin's path is shown beside the chip so the match is never silent. */
  function rowStatus(r: ForgeRepo): { readiness: ReturnType<typeof readinessOf>; twin?: string } {
    const direct = millsProjectsStore.byProject(r.path);
    const home = millsProjectsStore.registry?.home_project;
    if (direct || r.provider !== 'github') {
      return { readiness: readinessOf(direct, home, millsProjectsStore.known) };
    }
    const twin = millsProjectsStore.byName(r.name)[0];
    return { readiness: readinessOf(twin, home, millsProjectsStore.known), twin: twin?.project };
  }

  // Escape is handled at the window in the capture phase: when this picker
  // hands off to the onboard dialog, focus can land outside either dialog for
  // a frame, and a backdrop-scoped handler would then miss the key.
  function handleWindowKeydown(event: KeyboardEvent): void {
    if (!open || event.key !== 'Escape') return;
    event.stopPropagation();
    onClose();
  }
  function handleBackdropClick(event: MouseEvent): void {
    if ((event.target as HTMLElement)?.classList?.contains('rp-backdrop')) onClose();
  }
</script>

<svelte:window onkeydowncapture={handleWindowKeydown} />

{#if open}
  <!-- svelte-ignore a11y_no_static_element_interactions, a11y_click_events_have_key_events -->
  <div class="rp-backdrop" onclick={handleBackdropClick}>
    <div class="rp-dialog" role="dialog" aria-modal="true" aria-labelledby="rp-title" use:focusTrap>
      <div class="rp-head">
        <div class="rp-title" id="rp-title">⊕ Pick a repo to onboard</div>
        <div class="rp-sub">Mills weaves on GitLab. A GitHub repo is imported into GitLab first and GitHub stays its mirror.</div>
      </div>

      <div class="rp-tools">
        <div class="rp-tabs" role="tablist" aria-label="Forge">
          <button type="button" role="tab" class="rp-tab" class:active={provider === 'gitlab'} aria-selected={provider === 'gitlab'} onclick={() => switchProvider('gitlab')}>⬢ GitLab</button>
          <button type="button" role="tab" class="rp-tab" class:active={provider === 'github'} aria-selected={provider === 'github'} onclick={() => switchProvider('github')}>⌥ GitHub</button>
        </div>
        <input class="rp-search" type="search" placeholder={provider === 'gitlab' ? 'Search projects…' : 'Filter repos…'} value={query} oninput={onSearch} aria-label="Search repositories" />
      </div>

      <div class="rp-list" aria-busy={loading}>
        {#if errorMsg}
          <div class="rp-state rp-warn" role="status">
            {errorMsg}
            {#if connectHint}<code class="rp-hint">{connectHint}</code>{/if}
          </div>
        {:else if loading && repos.length === 0}
          <div class="rp-state">Loading {provider === 'gitlab' ? 'projects' : 'repos'}…</div>
        {:else if repos.length === 0}
          <div class="rp-state">No repositories match.</div>
        {:else}
          <ul class="rp-rows">
            {#each repos as r (r.provider + ':' + r.path)}
              {@const st = rowStatus(r)}
              <li>
                <button type="button" class="rp-row" onclick={() => onPick(r)} title={r.description || r.path}>
                  <span class="rp-path mono">{r.path}</span>
                  <span class="rp-meta">
                    <span class="rp-vis">{r.visibility}</span>
                    {#if r.default_branch}<span class="rp-dim mono">{r.default_branch}</span>{/if}
                    {#if r.archived}<span class="rp-dim">archived</span>{/if}
                    {#if r.last_activity}<span class="rp-dim">{relativeTime(r.last_activity)}</span>{/if}
                  </span>
                  <span class="rp-ready" title={st.twin ? `${st.readiness.detail} (via GitLab twin ${st.twin})` : st.readiness.detail}>
                    {#if st.twin}<span class="rp-twin mono">≈ {st.twin}</span>{/if}
                    <Badge text={st.readiness.label} variant={st.readiness.variant} />
                  </span>
                </button>
              </li>
            {/each}
          </ul>
          {#if hasMore}
            <button type="button" class="rp-more" onclick={() => load(page + 1, false)} disabled={loading}>{loading ? 'Loading…' : 'Load more'}</button>
          {/if}
        {/if}
      </div>

      <div class="rp-actions">
        <button type="button" class="btn-cancel" onclick={onClose}>Close</button>
      </div>
    </div>
  </div>
{/if}

<style>
  .rp-backdrop {
    position: fixed; inset: 0; z-index: 9999;
    display: flex; align-items: center; justify-content: center;
    background: var(--scrim); backdrop-filter: blur(2px);
  }
  .rp-dialog {
    display: flex; flex-direction: column; gap: var(--space-3);
    width: 94%; max-width: 720px; max-height: 86vh;
    padding: var(--space-4);
    background: var(--bg-primary); border: 1px solid var(--border);
    border-radius: var(--radius-lg); box-shadow: 0 8px 32px rgba(0, 0, 0, 0.4);
  }
  .rp-title { font-size: var(--text-lg); font-weight: 700; color: var(--fg-primary); }
  .rp-sub { font-size: var(--text-xs); color: var(--fg-muted); margin-top: 2px; }
  .rp-tools { display: flex; gap: var(--space-2); align-items: center; flex-wrap: wrap; }
  .rp-tabs { display: inline-flex; border: 1px solid var(--border-subtle); border-radius: var(--radius-full); overflow: hidden; }
  .rp-tab {
    padding: 3px 12px; border: 0; background: transparent; color: var(--fg-muted);
    font-size: var(--text-xs); cursor: pointer;
  }
  .rp-tab + .rp-tab { border-left: 1px solid var(--border-subtle); }
  .rp-tab.active { background: color-mix(in srgb, var(--info) 14%, transparent); color: var(--info); }
  .rp-search {
    flex: 1 1 200px; padding: 5px 10px;
    border: 1px solid var(--border); border-radius: var(--radius-sm);
    background: var(--bg-secondary); color: var(--fg-primary); font-size: var(--text-sm);
  }
  .rp-list { flex: 1 1 auto; min-height: 200px; overflow-y: auto; border: 1px solid var(--border-subtle); border-radius: var(--radius-md); }
  .rp-state { padding: var(--space-4); text-align: center; color: var(--fg-muted); font-size: var(--text-sm); }
  .rp-warn { color: var(--warning); }
  .rp-hint { display: block; margin-top: var(--space-2); font-family: var(--font-mono); font-size: var(--text-xs); color: var(--fg-secondary); }
  .rp-rows { list-style: none; margin: 0; padding: 0; }
  .rp-row {
    display: grid; grid-template-columns: minmax(0, 1fr) auto auto; gap: var(--space-3); align-items: center;
    width: 100%; padding: 7px var(--space-3); border: 0; border-bottom: 1px solid var(--border-subtle);
    background: transparent; color: var(--fg-secondary); text-align: left; cursor: pointer;
  }
  .rp-row:hover { background: var(--bg-tertiary); color: var(--fg-primary); }
  .rp-row:focus-visible { outline: 2px solid var(--focus-ring); outline-offset: -2px; }
  .mono { font-family: var(--font-mono); }
  .rp-path { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--fg-primary); }
  .rp-meta { display: inline-flex; gap: var(--space-2); font-size: var(--text-2xs); white-space: nowrap; }
  .rp-vis { color: var(--fg-secondary); }
  .rp-dim { color: var(--fg-dim); }
  .rp-ready { display: inline-flex; align-items: center; gap: var(--space-2); }
  .rp-twin { font-size: var(--text-2xs); color: var(--fg-dim); white-space: nowrap; }
  .rp-more { margin: var(--space-2); padding: 4px 10px; border: 1px solid var(--border-subtle); border-radius: var(--radius-sm); background: transparent; color: var(--fg-muted); cursor: pointer; }
  .rp-actions { display: flex; justify-content: flex-end; }
  .btn-cancel { padding: 6px 12px; border: 1px solid var(--border); border-radius: var(--radius-sm); background: transparent; color: var(--fg-secondary); cursor: pointer; }
  @media (max-width: 640px) {
    .rp-row { grid-template-columns: minmax(0, 1fr) auto; }
    .rp-meta { display: none; }
    .rp-twin { display: none; }
  }
</style>
