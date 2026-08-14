<script lang="ts">
  import type { Component } from 'svelte';
  // Renders a lazily-imported panel with the standard loading/error
  // treatment. Pairs with panelRegistry.ts so every panel gets one
  // consistent pending state instead of each App.svelte branch
  // hand-rolling the same await scaffolding.
  import type { PanelLoader } from '../../panelRegistry.ts';
  import EmptyState from './EmptyState.svelte';

  let { loader }: { loader: PanelLoader } = $props();
</script>

{#await loader()}
  <div class="panel-loading"><div class="loading-bar"><div class="loading-bar-inner"></div></div></div>
{:then mod}
  {@const Panel = mod.default as Component}
  <Panel />
{:catch err}
  <!-- A content-hashed chunk that 404s after a redeploy is the common cause, so
       surface the error and offer the one recovery that works. -->
  <EmptyState
    icon="!"
    heading="Failed to load panel"
    description={`${err?.message ?? err} — the HUD may have been redeployed since this tab loaded.`}
  >
    {#snippet action()}
      <button class="btn btn-accent" onclick={() => globalThis.location.reload()}>Reload HUD</button>
    {/snippet}
  </EmptyState>
{/await}

<style>
  /* Moved from App.svelte with the await scaffolding — scoped styles must
     live where the markup lives. */
  .panel-loading {
    display: flex;
    align-items: center;
    justify-content: center;
    padding: var(--space-8);
    min-height: 200px;
  }

  .loading-bar {
    width: 100px;
    height: 2px;
    background: var(--bg-tertiary);
    border-radius: 1px;
    overflow: hidden;
  }

  .loading-bar-inner {
    width: 40%;
    height: 100%;
    background: linear-gradient(90deg, var(--info), var(--accent));
    border-radius: 1px;
    animation: loadSlide 1s ease-in-out infinite;
  }

  @keyframes loadSlide {
    0%   { transform: translateX(-100%); }
    100% { transform: translateX(350%); }
  }
</style>
