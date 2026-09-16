<script lang="ts">
  /**
   * TierBadge — an engram's tier as a named thing with its proof contract
   * attached, not a bare integer.
   *
   * The tier system is the spine of the engram model (svc_engrams.go:523-544):
   * tier 1 needs any proof, tier 2's proof must contain `command:`, tier 3
   * needs `command:` plus a `benchmark:` or `dashboard:`. None of that was
   * stated anywhere in the HUD — tier showed up as a column position in the
   * tree and a raw "tier:2" string in the strip. Here the number keeps its
   * name, and the contract it implies is one hover away.
   */
  let {
    tier,
    /** Show the tier's name next to the number. */
    labelled = true,
  }: { tier: number; labelled?: boolean } = $props();

  const NAMES: Record<number, string> = { 1: 'idiom', 2: 'composite', 3: 'system' };
  const CONTRACTS: Record<number, string> = {
    1: 'Tier 1 — idiom or recipe. Proof contract: any non-empty proof.',
    2: 'Tier 2 — composite. Proof contract: a runnable test (proof must name a command).',
    3: 'Tier 3 — system. Proof contract: a command plus a benchmark or dashboard.',
  };

  const name = $derived(NAMES[tier] ?? 'untiered');
  const contract = $derived(CONTRACTS[tier] ?? 'Tier outside the 1–3 contract range.');
</script>

<span class="tier-badge" data-tier={tier} title={contract}>
  <span class="num">T{tier}</span>{#if labelled}<span class="name">{name}</span>{/if}
</span>

<style>
  .tier-badge {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    padding: 1px var(--space-2) 1px var(--space-1);
    border-radius: var(--radius-full);
    border: 1px solid color-mix(in srgb, var(--tier-tone) 30%, transparent);
    background: color-mix(in srgb, var(--tier-tone) 10%, transparent);
    font-size: var(--text-2xs);
    white-space: nowrap;
    /* Tier is depth of assembly, not health — kept on a single informational
       hue with rising strength so it never competes with ProofBadge's
       green/amber/red, which is the only status signal on this surface. */
    --tier-tone: var(--info);
  }
  .tier-badge[data-tier='1'] { --tier-tone: color-mix(in srgb, var(--info) 55%, var(--fg-tertiary)); }
  .tier-badge[data-tier='2'] { --tier-tone: var(--info); }
  .tier-badge[data-tier='3'] { --tier-tone: var(--accent); }

  .num {
    font-family: var(--font-mono);
    font-weight: 700;
    color: var(--tier-tone);
    padding: 0 var(--space-1);
  }
  .name {
    color: var(--fg-tertiary);
    letter-spacing: var(--tracking-wide);
  }
</style>
