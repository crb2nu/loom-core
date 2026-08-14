import { afterEach, describe, expect, it, vi } from 'vitest';
import { millsStaffStore } from './mills_staff.svelte.ts';

// Fetch-boundary coverage for the Mill Staff panel store.
//
// Every list on these payloads is a Go slice, so an empty one arrives as JSON
// `null`. The panel iterates and spreads them inside $derived, and one throw
// there tears down the whole effect tree (the gates:null wedge). The store must
// therefore normalise at the boundary. The six reports also have to fail
// independently: an unreachable judge-calibration endpoint must not blank the
// other five tiles, and a 503 (operator unconfigured) is a calm disabled state
// rather than an error.

type Route = (path: string) => { status: number; body: string };

function routedFetch(route: Route): typeof globalThis.fetch {
  return vi.fn((input: unknown) => {
    const path = String(input);
    const { status, body } = route(path);
    return Promise.resolve(
      new Response(body, { status, headers: { 'Content-Type': 'application/json' } }),
    );
  }) as unknown as typeof globalThis.fetch;
}

// Minimal well-formed bodies keyed by endpoint; every list is deliberately null.
function nullListBody(path: string): string {
  if (path.includes('promotion-report')) {
    return JSON.stringify({ actor_prefix: 'overseer.', total_actions: 0, per_actor: null });
  }
  if (path.includes('judge-calibration')) {
    return JSON.stringify({ total_verdicts: 0, per_gate: null, buckets: null, models: null });
  }
  if (path.includes('signature-candidates')) {
    return JSON.stringify({ window: '336h', candidates: null });
  }
  if (path.includes('config-outcomes')) {
    return JSON.stringify({ stamped_runs: 0, per_policy_checksum: null, per_stage_model: null });
  }
  return JSON.stringify({ window: '336h', regressions: null });
}

const realFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = realFetch;
  millsStaffStore.stopPolling();
  millsStaffStore.promotion = { data: null, error: null, disabled: false, lastUpdated: null };
  millsStaffStore.councilPromotion = { data: null, error: null, disabled: false, lastUpdated: null };
  millsStaffStore.judge = { data: null, error: null, disabled: false, lastUpdated: null };
  millsStaffStore.regressions = { data: null, error: null, disabled: false, lastUpdated: null };
  millsStaffStore.configOutcomes = { data: null, error: null, disabled: false, lastUpdated: null };
  millsStaffStore.signatures = { data: null, error: null, disabled: false, lastUpdated: null };
  millsStaffStore.window = '336h';
});

describe('millsStaffStore.refresh', () => {
  it('normalises null lists on every report to empty arrays', async () => {
    globalThis.fetch = routedFetch((path) => ({ status: 200, body: nullListBody(path) }));

    await millsStaffStore.refresh();

    expect(millsStaffStore.promotion.data?.per_actor).toEqual([]);
    expect(millsStaffStore.councilPromotion.data?.per_actor).toEqual([]);
    expect(millsStaffStore.judge.data?.per_gate).toEqual([]);
    expect(millsStaffStore.judge.data?.buckets).toEqual([]);
    expect(millsStaffStore.judge.data?.models).toEqual([]);
    expect(millsStaffStore.regressions.data?.regressions).toEqual([]);
    expect(millsStaffStore.configOutcomes.data?.per_policy_checksum).toEqual([]);
    expect(millsStaffStore.configOutcomes.data?.per_stage_model).toEqual([]);
    expect(millsStaffStore.signatures.data?.candidates).toEqual([]);
    // The panel spreads these to sort; the spread must be safe on the cached
    // payload, not just the template's belt-and-braces `?? []`.
    expect([...(millsStaffStore.signatures.data?.candidates ?? [])]).toEqual([]);
  });

  it('fills absent nested structs so a totals read never hits undefined', async () => {
    globalThis.fetch = routedFetch((path) => {
      if (path.includes('config-outcomes')) {
        // totals and regressions omitted entirely.
        return { status: 200, body: JSON.stringify({ stamped_runs: 3, uncovered_runs: 1 }) };
      }
      return { status: 200, body: nullListBody(path) };
    });

    await millsStaffStore.refresh();

    expect(millsStaffStore.configOutcomes.data?.totals.merge_rate).toBe(0);
    expect(millsStaffStore.configOutcomes.data?.totals.runs).toBe(0);
    expect(millsStaffStore.configOutcomes.data?.regressions.unlinked).toBe(0);
  });

  it('derives count from the list when the wire omits it', async () => {
    globalThis.fetch = routedFetch((path) => {
      if (path.includes('signature-candidates')) {
        return {
          status: 200,
          body: JSON.stringify({
            window: '336h',
            candidates: [{ fingerprint: 'f1', phrase: 'context deadline exceeded' }],
          }),
        };
      }
      return { status: 200, body: nullListBody(path) };
    });

    await millsStaffStore.refresh();

    expect(millsStaffStore.signatures.data?.count).toBe(1);
    // Absent numeric payload fields default rather than reaching the panel.
    expect(millsStaffStore.signatures.data?.candidates[0].member_count).toBe(0);
    expect(millsStaffStore.signatures.data?.candidates[0].sample_evidence).toEqual([]);
  });

  it("surfaces the operator's error body, not just the status code", async () => {
    const explanation =
      'promotion report: window holds at least 10000 events; narrow the window rather than review a truncated count';
    globalThis.fetch = routedFetch((path) =>
      path.includes('promotion-report')
        ? { status: 500, body: JSON.stringify({ error: explanation }) }
        : { status: 200, body: nullListBody(path) },
    );

    await millsStaffStore.refresh();

    expect(millsStaffStore.promotion.error).toContain('narrow the window');
    expect(millsStaffStore.promotion.error).toContain('500');
  });

  it('isolates one failing report from the other five', async () => {
    globalThis.fetch = routedFetch((path) =>
      path.includes('judge-calibration')
        ? { status: 500, body: 'boom' }
        : { status: 200, body: nullListBody(path) },
    );

    await millsStaffStore.refresh();

    expect(millsStaffStore.judge.error).toContain('500');
    expect(millsStaffStore.judge.data).toBeNull();
    // The other five still loaded.
    expect(millsStaffStore.promotion.error).toBeNull();
    expect(millsStaffStore.regressions.data).not.toBeNull();
    expect(millsStaffStore.configOutcomes.data).not.toBeNull();
    expect(millsStaffStore.signatures.data).not.toBeNull();
  });

  it('flips disabled on a 503 (operator not configured) without an error', async () => {
    globalThis.fetch = routedFetch(() => ({ status: 503, body: 'operator not configured' }));

    await millsStaffStore.refresh();

    expect(millsStaffStore.promotion.disabled).toBe(true);
    expect(millsStaffStore.promotion.error).toBeNull();
    expect(millsStaffStore.signatures.disabled).toBe(true);
  });

  it('keeps the last good snapshot when a refresh fails', async () => {
    globalThis.fetch = routedFetch((path) => ({
      status: 200,
      body: path.includes('regressions')
        ? JSON.stringify({ window: '336h', count: 2, regressions: [] })
        : nullListBody(path),
    }));
    await millsStaffStore.refresh();
    expect(millsStaffStore.regressions.data?.count).toBe(2);

    globalThis.fetch = routedFetch(() => ({ status: 502, body: 'upstream down' }));
    await millsStaffStore.refresh();

    expect(millsStaffStore.regressions.error).toContain('502');
    expect(millsStaffStore.regressions.data?.count).toBe(2);
  });

  it('applies the selected window to every report request', async () => {
    const seen: string[] = [];
    globalThis.fetch = routedFetch((path) => {
      seen.push(path);
      return { status: 200, body: nullListBody(path) };
    });

    millsStaffStore.window = '720h';
    await millsStaffStore.refresh();

    expect(seen).toHaveLength(6);
    for (const path of seen) {
      expect(path).toContain('window=720h');
    }
    // The promotion report is fetched once per staff actor family.
    expect(seen.filter((p) => p.includes('actor=overseer.'))).toHaveLength(1);
    expect(seen.filter((p) => p.includes('actor=council.'))).toHaveLength(1);
  });
});
