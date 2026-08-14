// MillStaffPanel's five evidence tiles, after they were converted from a
// hand-rolled `.tile` system to the shared MetricCard.
//
// The conversion is mostly cosmetic, but three things about it are load-bearing
// and silently breakable, so they are pinned here rather than left to review:
//
//   1. The tile header must route through MetricCard's own props. The tiles used
//      to render a standalone <Badge> inside a `.tile-header`; MetricCard paints
//      its own `.metric-card-badge`. Passing `badge=""` is what suppresses the
//      pill, so a refactor that reintroduced `<Badge>` (or that leaked an empty
//      pill when there is no evidence) would look right in a screenshot of the
//      populated case and wrong in the zero case.
//   2. MetricCard turns the whole card into `role="button"` the moment an
//      `onclick` prop is passed. Each tile contains a real <button> (the raw-JSON
//      disclosure), and a button nested inside a role="button" container is an
//      a11y violation that also double-fires on Enter/Space. The tiles therefore
//      must never be given `onclick` — asserted directly, because the failure is
//      invisible until someone tabs through the panel.
//   3. The tile bodies are passed as MetricCard children, which Svelte compiles
//      in *this* panel's scope. The disclosure toggle has to keep working from
//      inside that snippet.
//
// A `.dom.test.ts` on purpose: the panel body is only reachable mounted, and the
// interaction assertions need real listeners, which svelte/server never attaches.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import MillStaffPanel from './MillStaffPanel.svelte';
import { millsStaffStore } from '../../stores/mills_staff.svelte.ts';
import type {
  ConfigOutcomeReport,
  JudgeCalibrationReport,
  PromotionReport,
  RegressionsReport,
  ReportSlot,
  SignatureCandidatesReport,
} from '../../stores/mills_staff.svelte.ts';
import { millsOverseersStore } from '../../stores/mills_overseers.svelte.ts';
import { millsSquadsStore } from '../../stores/mills_squads.svelte.ts';

/** The five evidence tiles, in render order, by the label MetricCard shows. */
const TILE_LABELS = [
  'Promotion',
  'Judge calibration',
  'Regressions',
  'Config outcomes',
  'Signature candidates',
];

const WINDOW_START = '2026-08-01T00:00:00Z';
const WINDOW_END = '2026-08-07T00:00:00Z';

// Fully-typed zero reports. Tests spread these and override only the fields the
// tile under test reads, so the fixtures stay valid if the wire types grow.
const PROMOTION_ZERO: PromotionReport = {
  actor_prefix: 'council.',
  window_start: WINDOW_START,
  window_end: WINDOW_END,
  total_actions: 0,
  total_dry_run: 0,
  total_executed: 0,
  per_actor: [],
  zero_evidence: false,
};

const REGRESSIONS_ZERO: RegressionsReport = {
  window: '336h',
  since: WINDOW_START,
  count: 0,
  regressions: [],
};

const SIGNATURES_ZERO: SignatureCandidatesReport = {
  window: '336h',
  since: WINDOW_START,
  count: 0,
  candidates: [],
};

/** A report slot carrying `data`, optionally annotated with a fetch error. */
function slot<T>(data: T | null, error: string | null = null, disabled = false): ReportSlot<T> {
  return { data, error, disabled, lastUpdated: data == null ? null : new Date() };
}

let target: HTMLElement;
let component: Record<string, unknown> | null = null;

function resetStore(): void {
  millsStaffStore.stopPolling();
  millsStaffStore.promotion = slot<PromotionReport>(null);
  millsStaffStore.councilPromotion = slot<PromotionReport>(null);
  millsStaffStore.judge = slot<JudgeCalibrationReport>(null);
  millsStaffStore.regressions = slot<RegressionsReport>(null);
  millsStaffStore.configOutcomes = slot<ConfigOutcomeReport>(null);
  millsStaffStore.signatures = slot<SignatureCandidatesReport>(null);
  millsStaffStore.window = '336h';
}

/**
 * Mount the panel with every bootstrap fetch rejected.
 *
 * That leaves all six report slots at their zero-filled defaults — the state
 * each test then overwrites with only the fields the tile it cares about reads,
 * so no assertion depends on a full endpoint payload shape.
 */
async function mountPanel(): Promise<void> {
  vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('offline')));
  target = document.createElement('div');
  document.body.appendChild(target);
  component = mount(MillStaffPanel, { target }) as Record<string, unknown>;
  await vi.waitFor(
    () => {
      expect(target.querySelector('.evidence')).not.toBeNull();
    },
    { timeout: 10_000, interval: 10 },
  );
  // Let the rejected bootstrap fetches settle before a test writes its own
  // slots, otherwise a late rejection would overwrite them.
  await new Promise((resolve) => setTimeout(resolve, 0));
  resetStore();
  flushSync();
}

/** The evidence section only — the department cards above render their own Badges. */
function evidence(): HTMLElement {
  const section = target.querySelector<HTMLElement>('.evidence');
  if (!section) throw new Error('evidence section did not render');
  return section;
}

function tiles(): HTMLElement[] {
  return Array.from(evidence().querySelectorAll<HTMLElement>('.metric-card'));
}

function tileByLabel(label: string): HTMLElement {
  const found = tiles().find(
    (card) => card.querySelector('.metric-card-label')?.textContent?.trim() === label,
  );
  if (!found) throw new Error(`no evidence tile labelled ${label}`);
  return found;
}

function apply(mutate: () => void): void {
  mutate();
  flushSync();
}

beforeEach(async () => {
  resetStore();
  await mountPanel();
});

afterEach(() => {
  if (component) void unmount(component);
  component = null;
  target?.remove();
  resetStore();
  millsOverseersStore.stopPolling();
  millsSquadsStore.stopPolling();
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
});

describe('MillStaffPanel evidence tiles — shared MetricCard', () => {
  it('renders all five tiles as MetricCards, in order', () => {
    const labels = tiles().map((c) => c.querySelector('.metric-card-label')?.textContent?.trim());
    expect(labels).toEqual(TILE_LABELS);
  });

  it('leaves no hand-rolled tile chrome behind', () => {
    // `.tile`/`.tile-header`/`.tile-label` were the superseded elements; their
    // CSS was deleted with them, so a leftover would render unstyled.
    expect(evidence().querySelector('.tile-header')).toBeNull();
    expect(evidence().querySelector('.tile-label')).toBeNull();
    expect(evidence().querySelector('article.tile')).toBeNull();
  });

  it('keeps each tile an <article>, not a bare <div>', () => {
    // The tiles were `<article class="tile">` before the conversion. MetricCard's
    // root defaults to a plain <div>, which would silently drop the implicit
    // article boundary assistive tech uses to separate the five reports — so the
    // panel opts back in via `element="article"`.
    expect(tiles()).toHaveLength(TILE_LABELS.length);
    for (const card of tiles()) {
      expect(card.tagName).toBe('ARTICLE');
    }
  });
});

describe('MillStaffPanel evidence tiles — badges route through MetricCard', () => {
  it('shows no pill at all while there is no evidence to flag', () => {
    // The zero case: `badge=""` must suppress the element, not paint an empty one.
    expect(evidence().querySelector('.metric-card-badge')).toBeNull();
    // And nothing fell back to the standalone Badge widget.
    expect(evidence().querySelector('.badge')).toBeNull();
  });

  it('paints the zero-evidence pill via MetricCard, not a nested Badge', () => {
    apply(() => {
      millsStaffStore.promotion = slot({ ...PROMOTION_ZERO, zero_evidence: true });
    });

    const badge = tileByLabel('Promotion').querySelector('.metric-card-badge');
    expect(badge?.textContent?.trim()).toBe('zero evidence');
    expect(badge?.classList.contains('badge-warning')).toBe(true);
    expect(tileByLabel('Promotion').querySelector('span.badge')).toBeNull();
  });

  it('keeps the counted pills and their variants', () => {
    apply(() => {
      millsStaffStore.regressions = slot({ ...REGRESSIONS_ZERO, count: 3 });
      millsStaffStore.signatures = slot({ ...SIGNATURES_ZERO, count: 7 });
    });

    const reverted = tileByLabel('Regressions').querySelector('.metric-card-badge');
    expect(reverted?.textContent?.trim()).toBe('3 reverted');
    expect(reverted?.classList.contains('badge-error')).toBe(true);

    const proposed = tileByLabel('Signature candidates').querySelector('.metric-card-badge');
    expect(proposed?.textContent?.trim()).toBe('7 proposed');
    expect(proposed?.classList.contains('badge-info')).toBe(true);
  });

  it('drops the regressions pill again when the count returns to zero', () => {
    apply(() => {
      millsStaffStore.regressions = slot({ ...REGRESSIONS_ZERO, count: 3 });
    });
    expect(tileByLabel('Regressions').querySelector('.metric-card-badge')).not.toBeNull();

    apply(() => {
      millsStaffStore.regressions = slot({ ...REGRESSIONS_ZERO, count: 0 });
    });
    expect(tileByLabel('Regressions').querySelector('.metric-card-badge')).toBeNull();
  });
});

describe('MillStaffPanel evidence tiles — the card must stay non-interactive', () => {
  it('never promotes a tile to role="button"', () => {
    // Passing `onclick` to MetricCard sets role=button + tabindex=0 on the card.
    // Every tile contains a real <button>, so that would nest interactives.
    for (const card of tiles()) {
      expect(card.getAttribute('role')).toBeNull();
      expect(card.getAttribute('tabindex')).toBeNull();
      expect(card.classList.contains('clickable')).toBe(false);
    }
  });

  it('keeps the raw-JSON disclosure as the only focusable control in a tile', () => {
    for (const card of tiles()) {
      const focusable = card.querySelectorAll('button, [tabindex], [role="button"]');
      expect(focusable.length).toBe(1);
      expect(focusable[0].tagName).toBe('BUTTON');
      expect(focusable[0].textContent).toContain('raw JSON');
    }
  });
});

describe('MillStaffPanel evidence tiles — children snippet stays live', () => {
  it('expands and collapses the raw JSON from inside the MetricCard body', () => {
    const card = tileByLabel('Promotion');
    const toggle = card.querySelector<HTMLButtonElement>('.tile-toggle');
    if (!toggle) throw new Error('disclosure button did not render');

    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    expect(card.querySelector('.tile-raw')).toBeNull();

    toggle.click();
    flushSync();
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    expect(card.querySelector('.tile-raw')).not.toBeNull();

    toggle.click();
    flushSync();
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    expect(card.querySelector('.tile-raw')).toBeNull();
  });

  it('toggles each tile independently', () => {
    const promotion = tileByLabel('Promotion');
    promotion.querySelector<HTMLButtonElement>('.tile-toggle')?.click();
    flushSync();

    expect(promotion.querySelector('.tile-raw')).not.toBeNull();
    expect(tileByLabel('Regressions').querySelector('.tile-raw')).toBeNull();
  });

  it('still renders the figures and sub-line inside the card body', () => {
    apply(() => {
      millsStaffStore.promotion = slot({
        ...PROMOTION_ZERO,
        total_dry_run: 4,
        total_executed: 9,
        total_actions: 13,
        per_actor: [{ actor: 'council.mutator', per_action: [] }],
      });
    });

    const card = tileByLabel('Promotion');
    const figures = Array.from(card.querySelectorAll('.figure')).map((f) => [
      f.querySelector('.fig-value')?.textContent?.trim(),
      f.querySelector('.fig-label')?.textContent?.trim(),
    ]);
    expect(figures).toEqual([
      ['4', 'dry-run'],
      ['9', 'executed'],
    ]);
    expect(card.querySelector('.tile-sub')?.textContent).toContain('13 audited actions');
  });
});

describe('MillStaffPanel evidence tiles — empty and error states', () => {
  it('states the empty case rather than rendering a blank card', () => {
    // Every slot is null after reset, which is the `zero` render state.
    for (const label of TILE_LABELS) {
      expect(tileByLabel(label).querySelector('.tile-empty')?.textContent?.trim()).toBe(
        'No evidence in window.',
      );
    }
  });

  it('reports an unconfigured operator', () => {
    apply(() => {
      millsStaffStore.judge = slot<JudgeCalibrationReport>(null, null, true);
    });
    expect(tileByLabel('Judge calibration').querySelector('.tile-empty')?.textContent?.trim()).toBe(
      'Mills operator not configured.',
    );
  });

  it('surfaces a hard error in the card body', () => {
    apply(() => {
      millsStaffStore.configOutcomes = slot<ConfigOutcomeReport>(null, 'operator unreachable');
    });
    const err = tileByLabel('Config outcomes').querySelector('.tile-error');
    expect(err?.textContent?.trim()).toBe('operator unreachable');
  });

  it('keeps stale data visible with a staleness note beside it', () => {
    // error + data present is the "stale" branch: the numbers stay, annotated.
    apply(() => {
      millsStaffStore.signatures = slot({ ...SIGNATURES_ZERO, count: 2 }, 'refresh failed');
    });

    const card = tileByLabel('Signature candidates');
    expect(card.querySelector('.tile-error')).toBeNull();
    expect(card.querySelector('.fig-value')?.textContent?.trim()).toBe('2');
    expect(card.querySelector('.tile-stale')?.textContent).toContain('refresh failed');
  });
});
