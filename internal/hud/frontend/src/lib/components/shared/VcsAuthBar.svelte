<script lang="ts">
  /**
   * VcsAuthBar — who the HUD daemon is on GitLab and GitHub, and how it
   * knows (env token, or the operator's glab / gh login where loomd runs).
   * When a provider is missing it shows the exact command to run; the HUD
   * cannot run an interactive device-code login itself, so it never pretends
   * to. Sits beside the Labs admin-token bar on intake surfaces so a 401 or
   * a 503 is never a dead end.
   */
  import { forgeStore, type ForgeProvider, type ForgeStatus } from '../../stores/forge.svelte.ts';
  import { toastStore } from '../../stores/toasts.svelte.ts';

  const PROVIDERS: Array<{ id: ForgeProvider; glyph: string; label: string }> = [
    { id: 'gitlab', glyph: '⬢', label: 'GitLab' },
    { id: 'github', glyph: '⌥', label: 'GitHub' },
  ];

  $effect(() => {
    if (!forgeStore.auth && !forgeStore.authLoading && forgeStore.available) void forgeStore.fetchAuth();
  });

  function statusFor(id: ForgeProvider): ForgeStatus | null {
    return forgeStore.auth?.[id] ?? null;
  }

  async function copyHint(hint: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(hint);
      toastStore.info('Connect command copied');
    } catch {
      // clipboard unavailable in odd embeds — the hint is visible anyway
    }
  }
</script>

<div class="vcs-bar" role="group" aria-label="Forge identity">
  {#if !forgeStore.available}
    <span class="vcs-note">This daemon has no forge domain — update loomd to list repos and create projects.</span>
  {:else}
    {#each PROVIDERS as p (p.id)}
      {@const st = statusFor(p.id)}
      <div class="vcs-chip" class:ok={st?.configured} class:missing={st && !st.configured} title={st?.error || st?.api_url || ''}>
        <span class="vcs-glyph" aria-hidden="true">{p.glyph}</span>
        <span class="vcs-label">{p.label}</span>
        {#if !st}
          <span class="vcs-dim">{forgeStore.authLoading ? 'checking…' : '—'}</span>
        {:else if st.configured}
          <span class="vcs-user mono">{st.user}</span>
          <span class="vcs-source" title="credential source">{st.source}</span>
        {:else}
          <span class="vcs-dim">not connected</span>
          {#if st.connect_hint}
            <button type="button" class="vcs-hint" onclick={() => copyHint(st.connect_hint ?? '')} title={st.connect_hint}>
              copy connect command
            </button>
          {/if}
        {/if}
      </div>
    {/each}
    <button type="button" class="vcs-refresh" onclick={() => forgeStore.fetchAuth()} disabled={forgeStore.authLoading} title="Re-check forge credentials">↻</button>
    {#if forgeStore.authError}
      <span class="vcs-err" role="status">{forgeStore.authError}</span>
    {/if}
  {/if}
</div>

<style>
  .vcs-bar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-xs);
  }
  .vcs-chip {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 2px 8px;
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-full);
    color: var(--fg-secondary);
    background: var(--bg-secondary);
  }
  .vcs-chip.ok { border-color: color-mix(in srgb, var(--success) 40%, var(--border-subtle)); }
  .vcs-chip.missing { border-color: color-mix(in srgb, var(--warning) 45%, var(--border-subtle)); }
  .vcs-glyph { color: var(--fg-dim); }
  .vcs-label { font-weight: 600; color: var(--fg-primary); }
  .mono { font-family: var(--font-mono); }
  .vcs-user { color: var(--success); }
  .vcs-source {
    padding: 0 5px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-2xs);
    color: var(--fg-muted);
  }
  .vcs-dim { color: var(--fg-muted); }
  .vcs-hint {
    padding: 0 6px;
    border: 1px solid color-mix(in srgb, var(--warning) 40%, transparent);
    border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--warning) 10%, transparent);
    color: var(--warning);
    font-size: var(--text-2xs);
    cursor: pointer;
  }
  .vcs-refresh {
    padding: 0 6px;
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--fg-muted);
    cursor: pointer;
  }
  .vcs-refresh:disabled { opacity: 0.5; cursor: default; }
  .vcs-note, .vcs-err { color: var(--fg-muted); }
  .vcs-err { color: var(--warning); }
</style>
