<script lang="ts">
  /**
   * PressLanes — the serial merge queue as a lane list: every MR waiting
   * for, rebasing onto, re-proving for, or being pressed into main, in
   * queue order, then the last few settled (pressed or evicted, with the
   * eviction reason). The instrument rail's "press" number is the depth;
   * this is what the depth is made of.
   */
  import type { PressRow } from '../../utils/shuttleBoardHelpers.ts';
  import Badge from '../../widgets/Badge.svelte';
  import { fmtDuration } from './shared/format.ts';
  import { mrURL } from '../../utils/gitlabLinks.ts';

  let {
    rows,
    off = false,
    error = null,
    onSelect,
  }: {
    rows: PressRow[];
    off?: boolean;
    error?: string | null;
    onSelect?: (runID: string) => void;
  } = $props();

  let active = $derived(rows.filter((r) => !r.settled));
  let settled = $derived(rows.filter((r) => r.settled));
</script>

<section class="press" aria-label="Press — serial merge lane">
  <header class="press-head">
    <span class="press-tag">press</span>
    {#if !off}
      <span class="press-count">{active.length}</span>
    {/if}
    <span class="press-hint">serial merge lane · queue order</span>
  </header>

  {#if off}
    <p class="press-state" role="status">press off by policy — proven MRs merge directly</p>
  {:else if error && rows.length === 0}
    <p class="press-state press-warn" role="status">press feed unavailable — {error}</p>
  {:else if rows.length === 0}
    <p class="press-state" role="status">press idle — nothing queued</p>
  {:else}
    <ol class="press-rows">
      {#each active as r, i (r.key)}
        <li class="press-row">
          <span class="press-pos mono">#{i + 1}</span>
          <a
            class="mr-chip mono"
            href={mrURL(r.project, r.mrIID)}
            target="_blank"
            rel="noreferrer noopener"
            title={`Open merge request !${r.mrIID}`}
          >!{r.mrIID}</a>
          <button
            type="button"
            class="press-warp mono"
            title={`Open run ${r.runID}`}
            onclick={() => onSelect?.(r.runID)}
          >{r.backlogID || r.runID}</button>
          <span class="press-project mono">{r.project}</span>
          <span class="press-spacer"></span>
          <Badge text={r.phrase} variant={r.tone} />
          {#if r.attempts > 1}
            <span class="press-tries mono" title="press attempts">×{r.attempts}</span>
          {/if}
          {#if r.ageMs != null}
            <span class="press-age mono">{fmtDuration(r.ageMs)}</span>
          {/if}
        </li>
      {/each}
      {#if settled.length > 0}
        <li class="press-divider" aria-hidden="true">settled</li>
        {#each settled as r (r.key)}
          <li class="press-row is-settled">
            <span class="press-pos mono">·</span>
            <a
              class="mr-chip mono"
              href={mrURL(r.project, r.mrIID)}
              target="_blank"
              rel="noreferrer noopener"
              title={`Open merge request !${r.mrIID}`}
            >!{r.mrIID}</a>
            <button
              type="button"
              class="press-warp mono"
              title={`Open run ${r.runID}`}
              onclick={() => onSelect?.(r.runID)}
            >{r.backlogID || r.runID}</button>
            <span class="press-spacer"></span>
            <Badge text={r.phrase} variant={r.tone} />
            {#if r.ageMs != null}
              <span class="press-age mono">{fmtDuration(r.ageMs)} ago</span>
            {/if}
          </li>
        {/each}
      {/if}
    </ol>
  {/if}
</section>

<style>
  .press {
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    background: var(--bg-secondary);
    overflow: hidden;
    min-width: 0;
  }
  .press-head {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border);
    background: var(--bg-primary);
  }
  .press-tag {
    font-size: var(--text-2xs);
    letter-spacing: var(--tracking-wide);
    text-transform: uppercase;
    color: var(--accent);
  }
  .press-count { font-family: var(--font-mono); font-size: var(--text-xs); font-weight: 700; color: var(--accent); }
  .press-hint { font-size: var(--text-2xs); color: var(--fg-dim); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .press-state { margin: 0; padding: var(--space-3); color: var(--fg-muted); font-size: var(--text-xs); }
  .press-warn { color: var(--warning); }

  .press-rows { list-style: none; margin: 0; padding: 0; }
  .press-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: 5px var(--space-3);
    border-bottom: 1px solid var(--border-subtle);
    font-size: var(--text-xs);
    min-width: 0;
  }
  .press-row:last-child { border-bottom: 0; }
  .press-row.is-settled { opacity: 0.7; }
  .press-divider {
    padding: 2px var(--space-3);
    font-size: var(--text-2xs);
    letter-spacing: var(--tracking-wide);
    text-transform: uppercase;
    color: var(--fg-dim);
    background: color-mix(in srgb, var(--bg-tertiary) 50%, transparent);
  }
  .mono { font-family: var(--font-mono); }
  .press-pos { color: var(--fg-dim); width: 2ch; }
  .press-warp {
    padding: 0;
    border: 0;
    background: none;
    color: var(--fg-primary);
    font-size: var(--text-xs);
    cursor: pointer;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 30ch;
  }
  .press-warp:hover { color: var(--info); text-decoration: underline; }
  .press-warp:focus-visible { outline: 2px solid var(--focus-ring); outline-offset: 2px; }
  .press-project { color: var(--fg-muted); font-size: var(--text-2xs); white-space: nowrap; }
  .press-spacer { flex: 1 1 0; }
  .press-tries { color: var(--warning); }
  .press-age { color: var(--fg-muted); white-space: nowrap; }
  .mr-chip {
    padding: 0 6px;
    border-radius: var(--radius-sm);
    border: 1px solid color-mix(in srgb, var(--mills) 32%, transparent);
    background: color-mix(in srgb, var(--mills) 14%, transparent);
    color: var(--mills);
    text-decoration: none;
    white-space: nowrap;
  }
  .mr-chip:hover { background: color-mix(in srgb, var(--mills) 24%, transparent); }
  .mr-chip:focus-visible { outline: 2px solid var(--focus-ring); outline-offset: 1px; }

  @media (max-width: 720px) {
    .press-project, .press-hint { display: none; }
  }
</style>
