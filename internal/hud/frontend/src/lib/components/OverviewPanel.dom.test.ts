// Coverage for OverviewPanel's heroSummary ladder, specifically its top rung.
//
// The hero is the single largest claim on the landing page. Every store it
// composes zero-fills on a failed fetch, so a dead daemon used to render the
// most reassuring headline in the app — "System nominal / No active pressure" —
// off five simultaneously-empty stores. The ladder now opens with a signal-loss
// rung that outranks every pressure branch below it: with a store dark we
// cannot know whether there is pressure, so claiming either way is wrong.
//
// A DOM test rather than a svelte/server render because the panel gates its
// whole body behind `initialLoad`, which is only cleared in fetchKPIs()'s
// `finally` — and that runs from an $effect, which SSR never executes. Rendered
// on the server the panel is nothing but a skeleton, so the hero is only
// reachable mounted. Assertions read HeroSummary's output: `command-alert` is
// the tone-alert class, `command-eyebrow` the rung label.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import OverviewPanel from './OverviewPanel.svelte';
import { fleetStore } from '../stores/fleet.svelte.ts';
import { healthStore } from '../stores/health.svelte.ts';
import { taskStore } from '../stores/tasks.svelte.ts';
import { millsStore } from '../stores/mills.svelte.ts';
import { coordinationStore } from '../stores/coordination.svelte.ts';

const SIGNAL_LOST = 'Signal lost';
const SIGNAL_LOST_HEADLINE = 'HUD data is incomplete';
const NOMINAL = 'System nominal';

/** The five stores the outage ladder reads, by the label the hero shows. */
const OUTAGE_STORES = [
  ['Fleet', fleetStore],
  ['Servers', healthStore],
  ['Tasks', taskStore],
  ['Mills', millsStore],
  ['Coordination', coordinationStore],
] as const;

let target: HTMLElement;
let component: Record<string, unknown> | null = null;

function clearErrors(): void {
  for (const [, store] of OUTAGE_STORES) store.error = null;
  healthStore.servers = [];
}

/**
 * Mount the panel and wait for it to leave its skeleton.
 *
 * Every bootstrap fetch is stubbed to reject: that is enough to clear
 * `initialLoad` (set in a `finally`) and it leaves the stores at their
 * zero-filled defaults — exactly the state that used to render "System
 * nominal". Each test then sets the store errors it cares about explicitly,
 * so the assertions never depend on per-endpoint payload shapes.
 */
async function mountPanel(): Promise<void> {
  vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('offline')));
  vi.stubGlobal('EventSource', undefined);
  target = document.createElement('div');
  document.body.appendChild(target);
  component = mount(OverviewPanel, { target }) as Record<string, unknown>;
  // Let every rejected bootstrap fetch settle before the test writes its own
  // errors, otherwise a late rejection would overwrite them.
  await vi.waitFor(
    () => {
      expect(target.querySelector('.skeleton-block')).toBeNull();
    },
    // Generous vs. the 1s default: mount + effect flush is fast locally but
    // this runs on a shared CI runner, and a timeout here would read as a
    // hero-ladder regression rather than a slow box.
    { timeout: 10_000, interval: 10 },
  );
  await new Promise((resolve) => setTimeout(resolve, 0));
  clearErrors();
  flushSync();
}

interface Hero {
  eyebrow: string;
  headline: string;
  detail: string;
  alert: boolean;
}

/**
 * Apply store mutations and read back the hero spec as rendered.
 *
 * Scoped to the hero element rather than matched against the panel's whole
 * markup: "System nominal" is also the InboxDeck's empty-state title, so a
 * substring match over innerHTML passes for the wrong reason in both
 * directions.
 */
function renderHero(mutate: () => void): Hero {
  mutate();
  flushSync();
  const section = target.querySelector('.command-section');
  if (!section) throw new Error('hero did not render');
  const text = (sel: string) => section.querySelector(sel)?.textContent?.trim() ?? '';
  return {
    eyebrow: text('.command-eyebrow'),
    headline: text('.command-title'),
    detail: text('.command-detail'),
    alert: section.classList.contains('command-alert'),
  };
}

beforeEach(async () => {
  clearErrors();
  await mountPanel();
});

afterEach(() => {
  if (component) void unmount(component);
  component = null;
  target?.remove();
  clearErrors();
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
});

describe('OverviewPanel heroSummary — signal loss', () => {
  for (const [label, store] of OUTAGE_STORES) {
    it(`returns the Signal lost spec when ${label.toLowerCase()} is dark`, () => {
      const hero = renderHero(() => { store.error = 'daemon unreachable'; });
      expect(hero.eyebrow).toBe(SIGNAL_LOST);
      expect(hero.headline).toBe(SIGNAL_LOST_HEADLINE);
      expect(hero.alert).toBe(true);
      // The failing store is named, so the operator knows what is missing.
      expect(hero.detail).toBe(`${label} unreachable — daemon unreachable`);
    });
  }

  it('names every dark store, not just the first', () => {
    const hero = renderHero(() => {
      fleetStore.error = 'daemon unreachable';
      coordinationStore.error = 'timeout';
    });
    expect(hero.detail).toBe('Fleet, Coordination unreachable — daemon unreachable');
  });

  it('outranks a pressure branch that would otherwise win', () => {
    // The precedence claim. Two servers down is a real "Infrastructure watch"
    // alert — but with the fleet store dark, that count comes from partial
    // data, so signal loss has to take the rung instead.
    const pressureOnly = renderHero(() => {
      healthStore.servers = [
        { name: 'a', status: 'down' },
        { name: 'b', status: 'down' },
      ] as unknown as typeof healthStore.servers;
    });
    expect(pressureOnly.eyebrow).toBe('Infrastructure watch');

    const withOutage = renderHero(() => { fleetStore.error = 'daemon unreachable'; });
    expect(withOutage.eyebrow).toBe(SIGNAL_LOST);
  });
});

describe('OverviewPanel heroSummary — healthy', () => {
  it('falls through to the calm nominal rung when every store is readable', () => {
    const hero = renderHero(() => {});
    expect(hero.eyebrow).toBe(NOMINAL);
    expect(hero.headline).toBe('No active pressure');
    expect(hero.alert).toBe(false);
  });

  it('treats a cleared error as healthy again', () => {
    // Stores reset `.error` to null on a successful refetch; the hero has to
    // climb back down the ladder rather than latch on the outage rung.
    expect(renderHero(() => { fleetStore.error = 'daemon unreachable'; }).eyebrow).toBe(SIGNAL_LOST);

    const hero = renderHero(() => { fleetStore.error = null; });
    expect(hero.eyebrow).toBe(NOMINAL);
    expect(hero.alert).toBe(false);
  });
});
