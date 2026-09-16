<script lang="ts">
  /**
   * ProofBadge — the single chip for "how proven is this thing", covering both
   * registers of the reusable-knowledge surface: an engram's `proof_status`
   * (unverified|verified|stale|failing) and a pattern's `status`
   * (candidate|approved|deprecated).
   *
   * Before this existed the same semantic was drawn three ways in one panel —
   * a 16% tint chip in the engram strip, a 3px left border on a tree node, and
   * an 18% tint chip on a pattern card — so "verified" and "approved" looked
   * like unrelated ideas. One anatomy, one tint formula, tone chosen by
   * meaning, and the meaning itself carried in the tooltip.
   */

  type Tone = 'good' | 'warn' | 'bad' | 'neutral';

  let {
    status,
    /** Render the state as a dot + label instead of a filled chip, for use
     *  inside a card that already carries its own surface. */
    subtle = false,
  }: { status: string; subtle?: boolean } = $props();

  const key = $derived((status ?? '').trim().toLowerCase());

  const TONES: Record<string, Tone> = {
    verified: 'good',
    approved: 'good',
    stale: 'warn',
    candidate: 'warn',
    failing: 'bad',
    unverified: 'neutral',
    deprecated: 'neutral',
  };

  // Said plainly, because nothing on this surface previously explained it.
  const MEANINGS: Record<string, string> = {
    verified: 'Proof re-ran and passed.',
    unverified: 'Never verified — the proof has not been run.',
    stale: 'Verified once, but not recently enough to trust.',
    failing: 'Proof ran and did not pass.',
    approved: 'Approved for stamping.',
    candidate: 'Proposed — not yet approved for stamping.',
    deprecated: 'Superseded; avoid stamping new work from it.',
  };

  const tone = $derived(TONES[key] ?? 'neutral');
  const label = $derived(key || 'unknown');
  const meaning = $derived(MEANINGS[key] ?? '');
</script>

<span class="proof-badge tone-{tone}" class:subtle title={meaning} data-status={key}>
  <span class="dot" aria-hidden="true"></span>{label}
</span>

<style>
  .proof-badge {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    padding: 1px var(--space-2);
    border-radius: var(--radius-full);
    font-size: var(--text-2xs);
    font-family: var(--font-mono);
    letter-spacing: var(--tracking-wide);
    white-space: nowrap;
    /* One tint formula for every state — the old 16%/18% split was noise. */
    background: color-mix(in srgb, var(--tone) 16%, transparent);
    color: var(--tone);
    border: 1px solid color-mix(in srgb, var(--tone) 34%, transparent);
    --tone: var(--fg-tertiary);
  }

  .tone-good { --tone: var(--success); }
  .tone-warn { --tone: var(--warning); }
  .tone-bad { --tone: var(--error); }
  .tone-neutral { --tone: var(--fg-tertiary); }

  /* Inside a card that already has a surface, drop the fill and keep the dot
     as the carrier so cards don't turn into a wall of coloured pills. */
  .proof-badge.subtle {
    background: transparent;
    border-color: transparent;
    color: var(--fg-tertiary);
    padding-left: 0;
  }

  .dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--tone);
    flex-shrink: 0;
  }
</style>
