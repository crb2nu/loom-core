import { afterEach, describe, expect, it, vi } from 'vitest';
import { millsStore } from './mills.svelte.ts';

// Fetch-boundary coverage for the Overview "Loom wiring" card.
//
// The /api/mills/wiring endpoint is live; an operator older than the route
// 404s — the store must flag that (wiringUnavailable) so the panel can name
// the cause instead of rendering a blank card. A live payload
// must be normalised at the boundary (arrays defaulted, nested structs
// filled) so no panel $derived ever spreads a Go nil slice. A hard error must
// surface via wiringError WITHOUT blanking previously-loaded wiring, and a 503
// (operator unconfigured) must flip `disabled` like the rest of the store.

function respond(status: number, body: string, contentType = 'application/json'): typeof globalThis.fetch {
  return vi.fn(() =>
    Promise.resolve(new Response(body, { status, headers: { 'Content-Type': contentType } })),
  ) as unknown as typeof globalThis.fetch;
}

const realFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = realFetch;
  millsStore.wiring = null;
  millsStore.wiringUnavailable = false;
  millsStore.wiringError = null;
  millsStore.disabled = false;
});

describe('fetchWiring', () => {
  it('normalises a live payload and clears the unavailable flag', async () => {
    globalThis.fetch = respond(
      200,
      JSON.stringify({
        judge: { backend: 'litellm', model: 'or/kimi-k3', fallbacks: null },
        stages: [{ stage: 'implement', agent: 'codex', model: 'gpt-5.6-terra', source: 'policy' }],
        // weaver / council / spawn / gates / litellm / policy omitted on purpose
      }),
    );

    await millsStore.fetchWiring();

    expect(millsStore.wiringUnavailable).toBe(false);
    expect(millsStore.wiringError).toBeNull();
    expect(millsStore.wiring?.judge.backend).toBe('litellm');
    // null fallbacks + missing council coerced to safe defaults.
    expect(millsStore.wiring?.judge.fallbacks).toEqual([]);
    expect(millsStore.wiring?.council.lenses).toEqual([]);
    expect(millsStore.wiring?.stages).toHaveLength(1);
  });

  it('flags endpoint-absent on a 404', async () => {
    globalThis.fetch = respond(404, 'not found', 'text/plain');

    await millsStore.fetchWiring();

    expect(millsStore.wiring).toBeNull();
    expect(millsStore.wiringUnavailable).toBe(true);
    expect(millsStore.wiringError).toBeNull();
  });

  it('treats a 200 index.html (SPA catch-all) as endpoint-absent', async () => {
    // A route the HUD build doesn't register yet resolves to index.html;
    // JSON.parse throws SyntaxError, which must degrade to endpoint-absent,
    // not error.
    globalThis.fetch = respond(200, '<!doctype html><html></html>', 'text/html');

    await millsStore.fetchWiring();

    expect(millsStore.wiring).toBeNull();
    expect(millsStore.wiringUnavailable).toBe(true);
    expect(millsStore.wiringError).toBeNull();
  });

  it('surfaces a hard error without blanking cached wiring', async () => {
    globalThis.fetch = respond(200, JSON.stringify({ judge: { backend: 'litellm' } }));
    await millsStore.fetchWiring();
    expect(millsStore.wiring?.judge.backend).toBe('litellm');

    globalThis.fetch = respond(500, 'boom', 'text/plain');
    await millsStore.fetchWiring();

    expect(millsStore.wiringError).toContain('500');
    // The routing shown a tick ago is still the best guess — do not blank it.
    expect(millsStore.wiring?.judge.backend).toBe('litellm');
  });

  it('flips disabled on a 503 (operator not configured)', async () => {
    globalThis.fetch = respond(503, 'operator not configured', 'text/plain');

    await millsStore.fetchWiring();

    expect(millsStore.disabled).toBe(true);
    expect(millsStore.wiringError).toBeNull();
  });

  it('no-ops while disabled', async () => {
    millsStore.disabled = true;
    const spy = vi.fn();
    globalThis.fetch = spy as unknown as typeof globalThis.fetch;

    await millsStore.fetchWiring();

    expect(spy).not.toHaveBeenCalled();
  });
});
