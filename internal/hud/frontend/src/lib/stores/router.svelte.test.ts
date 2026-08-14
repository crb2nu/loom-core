import { describe, expect, it } from 'vitest';
import { panelLoaders } from '../panelRegistry.ts';
import {
  overviewId,
  reservedSubViewKeys,
  resolveSubViewKey,
  views,
  type ViewDef,
} from './router.svelte.ts';

// Nav-contract coverage for the router's view/sub-view table.
//
// The bug this file exists to prevent: App.svelte used to resolve a sub-view
// keypress by ARRAY INDEX (`subViews[key.charCodeAt(0) - 'a']`) instead of the
// declared `key`. That only works for a view whose keys happen to run a, b,
// c… in order. Mills declares out-of-order keys, so its presses landed on the
// wrong panels while ViewShell rendered the declared key as a <kbd> hint —
// i.e. every hint was a lie. Two Mills tabs (Warps, Bolts) additionally
// declared 'r' and 'o', which App.svelte's global handlers claim for refresh
// and Overview, making those tabs keyboard-unreachable outright.

function byId(id: string): ViewDef {
  const v = views.find((x) => x.id === id);
  if (!v) throw new Error(`no view ${id}`);
  return v;
}

describe('sub-view shortcut keys', () => {
  it('are unique within every view', () => {
    for (const v of views) {
      const keys = v.subViews.map((sv) => sv.key);
      expect(new Set(keys).size, `duplicate sub-view key in view "${v.id}": ${keys.join(',')}`).toBe(
        keys.length,
      );
    }
  });

  it('are single letters', () => {
    for (const v of views) {
      for (const sv of v.subViews) {
        expect(sv.key, `${v.id}/${sv.id}`).toMatch(/^[a-z]$/);
      }
    }
  });

  it('never collide with the app-wide globals (o = Overview, r = refresh)', () => {
    for (const v of views) {
      for (const sv of v.subViews) {
        expect(
          reservedSubViewKeys as readonly string[],
          `${v.id}/${sv.id} declares reserved key "${sv.key}"`,
        ).not.toContain(sv.key);
      }
    }
  });

  it('resolves by declared key, not by array position', () => {
    const mills = byId('mills');
    // Mills is deliberately out of alphabetical order — index-based lookup
    // would answer these with whatever sits at position (key - 'a').
    expect(resolveSubViewKey(mills, 'w')?.id).toBe('warps');
    expect(resolveSubViewKey(mills, 'b')?.id).toBe('bolts');
    expect(resolveSubViewKey(mills, 'k')?.id).toBe('factory');
    expect(resolveSubViewKey(mills, 'n')?.id).toBe('mills-workflows');
    // 'c' is claimed by no Mills sub-view; the second entry in the array must
    // NOT answer for it (the index scheme's signature failure).
    expect(resolveSubViewKey(mills, 'c')).toBeNull();
  });

  it('resolves the backend-surface tabs added for blocked/context/mrwatch', () => {
    // These two tabs front live REST domains that had no HUD surface at all
    // (mrwatch registry + shepherd log; context-health monitor). Pinning their
    // keys here is what makes a future key re-cut a test failure rather than a
    // silently dead shortcut — the exact failure mode this file exists for.
    expect(resolveSubViewKey(byId('agents'), 'm')?.id).toBe('mrwatch');
    expect(resolveSubViewKey(byId('knowledge'), 'h')?.id).toBe('context-health');
  });

  it('resolves the alerting triage tab', () => {
    // Same pinning contract as the mrwatch/context-health entries above. 'l'
    // is deliberate: Operations already spends a–e plus m, and 'a' (Fleet) is
    // the obvious mnemonic that was taken.
    expect(resolveSubViewKey(byId('agents'), 'l')?.id).toBe('alerts');
  });

  it('makes every sub-view of every view reachable by its own key', () => {
    for (const v of views) {
      for (const sv of v.subViews) {
        expect(resolveSubViewKey(v, sv.key)?.id, `${v.id}/${sv.id}`).toBe(sv.id);
      }
    }
  });

  it('ignores non-single-character keys and a missing view', () => {
    expect(resolveSubViewKey(byId('mills'), 'Escape')).toBeNull();
    expect(resolveSubViewKey(undefined, 'a')).toBeNull();
  });
});

describe('view shortcut keys', () => {
  it('are unique across top-level views', () => {
    const keys = views.map((v) => v.key);
    expect(new Set(keys).size).toBe(keys.length);
  });
});

describe('view/panel wiring', () => {
  it('registers a panel loader for every sub-view', () => {
    for (const v of views) {
      for (const sv of v.subViews) {
        expect(panelLoaders[sv.id], `no panel registered for ${v.id}/${sv.id}`).toBeTypeOf(
          'function',
        );
      }
    }
  });

  it('points every view default at one of its own sub-views', () => {
    for (const v of views) {
      expect(
        v.subViews.some((sv) => sv.id === v.default),
        `view "${v.id}" defaults to "${v.default}", which is not one of its sub-views`,
      ).toBe(true);
    }
  });

  it('keeps sub-view ids globally unique so id-keyed lookups are unambiguous', () => {
    const ids = views.flatMap((v) => v.subViews.map((sv) => sv.id));
    expect(new Set(ids).size, `duplicate sub-view id in ${ids.join(',')}`).toBe(ids.length);
    // …including against the standalone Overview view id. `overview` used to
    // name BOTH the top-level "Now" view and the Mills overview sub-view.
    expect(ids).not.toContain(overviewId);
  });

  it('groups Mills sub-views into contiguous runs', () => {
    const mills = byId('mills');
    const seen: string[] = [];
    for (const sv of mills.subViews) {
      const g = sv.group ?? '';
      if (seen[seen.length - 1] !== g) seen.push(g);
    }
    // A group name appearing twice would mean its tabs are interleaved with
    // another group's, which ViewShell renders as two separate sections.
    expect(new Set(seen).size).toBe(seen.length);
    expect(seen).toEqual(['Mill floor', 'Mill staff', 'Governance']);
  });
});
