import { afterEach, describe, expect, it, vi } from 'vitest';
import { blockedStore } from './blocked.svelte.ts';

// Fetch-boundary coverage for GET /api/blocked ("Waiting on you").
//
// The failure this guards: "no agent is blocked" and "this build does not
// serve /api/blocked" render as the same empty card unless the store keeps
// them apart. The SPA catch-all answers an unregistered route with 200 +
// index.html, so an absent endpoint does NOT arrive as a 404.

function respond(status: number, body: string, contentType = 'application/json'): typeof globalThis.fetch {
  return vi.fn(() =>
    Promise.resolve(new Response(body, { status, headers: { 'Content-Type': contentType } })),
  ) as unknown as typeof globalThis.fetch;
}

const realFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = realFetch;
  blockedStore.sessions = [];
  blockedStore.error = null;
  blockedStore.unavailable = false;
});

describe('blockedStore.fetch', () => {
  it('parses the live payload and derives the longest wait', async () => {
    globalThis.fetch = respond(
      200,
      JSON.stringify({
        blocked: [
          {
            session_id: 'sess-a',
            agent_id: 'claude-code',
            reason: 'permission',
            tool_name: 'Bash',
            cwd: '/Users/x/workspace',
            since: '2026-07-25T10:00:00Z',
            waited_seconds: 412,
          },
          {
            session_id: 'sess-b',
            agent_id: 'codex',
            reason: 'idle',
            since: '2026-07-25T10:05:00Z',
            waited_seconds: 90,
          },
        ],
        count: 2,
      }),
    );

    await blockedStore.fetch();

    expect(blockedStore.unavailable).toBe(false);
    expect(blockedStore.error).toBeNull();
    expect(blockedStore.count).toBe(2);
    expect(blockedStore.sessions[0].tool_name).toBe('Bash');
    // Optional fields stay undefined rather than being invented.
    expect(blockedStore.sessions[1].tool_name).toBeUndefined();
    expect(blockedStore.longestWaitSeconds).toBe(412);
  });

  it('treats an empty list as "nothing blocked", not as unavailable', async () => {
    globalThis.fetch = respond(200, JSON.stringify({ blocked: [], count: 0 }));

    await blockedStore.fetch();

    expect(blockedStore.unavailable).toBe(false);
    expect(blockedStore.count).toBe(0);
    expect(blockedStore.longestWaitSeconds).toBe(0);
  });

  it('coerces a null array so the card never iterates null', async () => {
    globalThis.fetch = respond(200, JSON.stringify({ blocked: null, count: 0 }));

    await blockedStore.fetch();

    expect(blockedStore.sessions).toEqual([]);
    expect(blockedStore.unavailable).toBe(false);
  });

  it('flags endpoint-absent on a 404', async () => {
    globalThis.fetch = respond(404, 'not found', 'text/plain');

    await blockedStore.fetch();

    expect(blockedStore.unavailable).toBe(true);
    expect(blockedStore.error).toBeNull();
  });

  it('flags endpoint-absent on a 200 index.html (SPA catch-all)', async () => {
    globalThis.fetch = respond(200, '<!doctype html><html></html>', 'text/html');

    await blockedStore.fetch();

    expect(blockedStore.unavailable).toBe(true);
    expect(blockedStore.error).toBeNull();
  });

  it('surfaces a hard error without blanking the last known list', async () => {
    globalThis.fetch = respond(
      200,
      JSON.stringify({
        blocked: [{ session_id: 's1', agent_id: 'a', reason: 'permission', since: '', waited_seconds: 5 }],
        count: 1,
      }),
    );
    await blockedStore.fetch();
    expect(blockedStore.count).toBe(1);

    globalThis.fetch = respond(500, 'boom', 'text/plain');
    await blockedStore.fetch();

    expect(blockedStore.error).toContain('500');
    // An agent blocked a tick ago is still the best answer available.
    expect(blockedStore.count).toBe(1);
  });
});
