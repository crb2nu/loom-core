<script lang="ts">
  /**
   * RepoChip — which repo this work targeted.
   *
   * The floor already resolved this on every Bolts/Sparks/RunDetail row and
   * spent it entirely on an mrURL() argument, so an operator watching cross-repo
   * work land in flexdeck/flexinfer/procmodel saw one undifferentiated stream.
   * This renders the answer that was already in hand.
   *
   * Home work is deliberately quiet (neutral, no fill) and cross-repo work is
   * marked — the signal an operator scans for is "this one left the home repo",
   * so spending colour on the 85% case would drown it.
   */
  import { repoLabel, repoTitle, isCrossRepo } from './provenance.ts';

  let {
    targetProject,
  }: {
    targetProject: string | undefined | null;
  } = $props();

  const label = $derived(repoLabel(targetProject));
  const title = $derived(repoTitle(targetProject));
  const cross = $derived(isCrossRepo(targetProject));
</script>

<span class="repo-chip" class:cross {title} data-repo={label}>
  {#if cross}<span class="mark" aria-hidden="true">↗</span>{/if}{label}
</span>

<style>
  .repo-chip {
    display: inline-flex;
    align-items: center;
    gap: 3px;
    padding: 1px var(--space-2);
    border-radius: var(--radius-full);
    font-size: var(--text-2xs);
    font-family: var(--font-mono);
    letter-spacing: var(--tracking-wide);
    white-space: nowrap;
    color: var(--fg-tertiary);
    border: 1px solid var(--border-subtle);
    background: transparent;
  }

  /* Cross-repo is the exception worth seeing. */
  .repo-chip.cross {
    color: var(--accent);
    background: color-mix(in srgb, var(--accent) 16%, transparent);
    border-color: color-mix(in srgb, var(--accent) 34%, transparent);
  }

  .mark {
    font-size: var(--text-2xs);
    opacity: 0.8;
  }
</style>
