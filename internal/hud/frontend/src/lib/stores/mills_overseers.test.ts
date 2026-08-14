import { afterEach, describe, expect, it, vi } from 'vitest';
import { millsOverseersStore } from './mills_overseers.svelte.ts';

// Fetch-boundary coverage for the Overseers panel store.
//
// A live payload must be normalised at the boundary (agents/recent_actions
// defaulted, each agent's last_result filled) so no panel $derived ever spreads
// a Go nil slice or an absent counts struct. A 503 (operator unconfigured) must
// flip `disabled` like the rest of the Mills stores. A hard error must surface
// via `error` WITHOUT blanking the previously-loaded snapshot.

function respond(
  status: number,
  body: string,
  contentType = 'application/json',
): typeof globalThis.fetch {
  return vi.fn(() =>
    Promise.resolve(new Response(body, { status, headers: { 'Content-Type': contentType } })),
  ) as unknown as typeof globalThis.fetch;
}

const realFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = realFetch;
  millsOverseersStore.status = null;
  millsOverseersStore.error = null;
  millsOverseersStore.disabled = false;
});

describe('millsOverseersStore.refresh', () => {
  it('normalises a live payload (defaults agents, recent_actions, last_result)', async () => {
    globalThis.fetch = respond(
      200,
      JSON.stringify({
        enabled: true,
        agents: [
          {
            name: 'groomer',
            paused: false,
            last_tick_at: '2026-07-20T01:43:03Z',
            last_result: { inspected: 2, acted: 0, planned: 1, skipped: 0, errored: 0, note: 'llm_unavailable' },
            enabled: true,
            dry_run: true,
          },
          // sentinel arrives with a live suppression lease and no last_result
          {
            name: 'sentinel',
            paused: false,
            enabled: true,
            dry_run: false,
            suppression: { reason: 'unhealthy: flexinfer', until: '2026-07-20T02:15:00Z' },
          },
        ],
        recent_actions: null,
      }),
    );

    await millsOverseersStore.refresh();

    expect(millsOverseersStore.disabled).toBe(false);
    expect(millsOverseersStore.error).toBeNull();
    expect(millsOverseersStore.status?.enabled).toBe(true);
    expect(millsOverseersStore.status?.agents).toHaveLength(2);
    expect(millsOverseersStore.status?.agents[0].last_result.inspected).toBe(2);
    // last_result absent on sentinel → coerced to a zero-count struct.
    expect(millsOverseersStore.status?.agents[1].last_result.acted).toBe(0);
    expect(millsOverseersStore.status?.agents[1].suppression?.reason).toContain('flexinfer');
    // null recent_actions → safe empty object.
    expect(millsOverseersStore.status?.recent_actions).toEqual({});
  });

  it('flips disabled on a 503 (operator not configured)', async () => {
    globalThis.fetch = respond(503, 'operator not configured', 'text/plain');

    await millsOverseersStore.refresh();

    expect(millsOverseersStore.disabled).toBe(true);
    expect(millsOverseersStore.error).toBeNull();
  });

  it('surfaces a hard error without blanking the cached snapshot', async () => {
    globalThis.fetch = respond(
      200,
      JSON.stringify({ enabled: true, agents: [{ name: 'foreman', paused: false, enabled: true, dry_run: true, last_result: { inspected: 5, acted: 0, planned: 0, skipped: 5, errored: 0 } }], recent_actions: {} }),
    );
    await millsOverseersStore.refresh();
    expect(millsOverseersStore.status?.agents[0].name).toBe('foreman');

    globalThis.fetch = respond(500, 'boom', 'text/plain');
    await millsOverseersStore.refresh();

    expect(millsOverseersStore.error).toContain('500');
    // The snapshot from a tick ago is still the best view — do not blank it.
    expect(millsOverseersStore.status?.agents[0].name).toBe('foreman');
  });
});
