<script lang="ts">
  /**
   * RollingNumber — an inline mechanical counter. Digit reels roll to
   * the new value instead of snapping, so a change reads as an event
   * (mission-control odometer motion, not skeuomorphic chrome). Inherits
   * font size/weight/color from the parent; static under reduced-motion.
   * Unknown values render an em dash rather than a lying zero.
   */
  import { odometerDigits } from '../utils/andonHelpers.ts';

  let { value, minDigits = 1 }: { value: number | undefined | null; minDigits?: number } = $props();

  let known = $derived(typeof value === 'number' && Number.isFinite(value));
  let digits = $derived(odometerDigits(known ? (value as number) : 0, minDigits));
</script>

{#if known}
  <span class="roll" aria-label={String(value)}>
    {#each digits as d, i (i)}
      <span class="digit" aria-hidden="true">
        <span class="reel" style="--d: {d}">
          {#each [0, 1, 2, 3, 4, 5, 6, 7, 8, 9] as n (n)}
            <span class="n">{n}</span>
          {/each}
        </span>
      </span>
    {/each}
  </span>
{:else}
  <span>—</span>
{/if}

<style>
  .roll {
    display: inline-flex;
    font-variant-numeric: tabular-nums;
  }
  .digit {
    display: inline-block;
    height: 1em;
    overflow: hidden;
    line-height: 1;
  }
  .reel {
    display: flex;
    flex-direction: column;
    transform: translateY(calc(var(--d) * -1em));
    transition: transform 0.65s cubic-bezier(0.22, 1, 0.36, 1);
  }
  .n {
    display: block;
    height: 1em;
    line-height: 1;
  }
  @media (prefers-reduced-motion: reduce) {
    .reel { transition: none; }
  }
</style>
