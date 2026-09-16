import { afterEach, describe, expect, it, vi } from 'vitest';
import { millsStore, type BacklogItem, type PipelineRun } from './mills.svelte.ts';
const item = (ID: string): BacklogItem => ({ ID, Title: ID, State: 'escalated', Priority: 'P1', UpdatedAt: 'one' });
const run = (id: string, suffix = ''): PipelineRun => ({ ID: `run-${id}${suffix}`, BacklogID: id, State: 'escalated', Template: 'default', Attempts: 1 });
afterEach(async () => {
  vi.restoreAllMocks();
  vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response('[]'));
  await millsStore.fetchRescueShelf();
  millsStore.pipelineHistory = [];
  vi.restoreAllMocks();
});
function serve(items: () => BacklogItem[], suffix: () => string = () => '') {
  return vi.spyOn(globalThis, 'fetch').mockImplementation(async input => {
    const url = String(input); let body: unknown;
    if (url.includes('/backlog?')) body = items();
    else if (url.includes('backlog_id=')) {
      const id = new URL(url, 'http://local').searchParams.get('backlog_id')!;
      body = [run(id, suffix()), run('unrelated')];
    } else {
      const id = decodeURIComponent(url.split('/').pop()!).replace(/^run-/, '').replace(/-next$/, '');
      body = { run: run(id, suffix()), stages: [], gates: [] };
    }
    return new Response(JSON.stringify(body));
  });
}
describe('rescue shelf hydration', () => {
  it('bounds each pass to eight jobs, rotates fairly and never writes history', async () => {
    const items = Array.from({ length: 11 }, (_, n) => item(String(n)));
    const fetch = serve(() => items);
    millsStore.pipelineHistory = [run('history')];
    await millsStore.fetchRescueShelf();
    expect(Object.keys(millsStore.rescueRunsByItem)).toHaveLength(8);
    expect(fetch.mock.calls.filter(([u]) => String(u).includes('backlog_id='))).toHaveLength(8);
    expect(millsStore.pipelineHistory.map(r => r.ID)).toEqual(['run-history']);
    fetch.mockClear(); await millsStore.fetchRescueShelf();
    expect(Object.keys(millsStore.rescueRunsByItem)).toHaveLength(11);
    expect(fetch.mock.calls.filter(([u]) => /\/runs\/run-/.test(String(u)))).toHaveLength(3);
  });
  it('caches details but invalidates changed items, new runs and removed items', async () => {
    let items = [item('a')]; let suffix = '';
    const fetch = serve(() => items, () => suffix);
    await millsStore.fetchRescueShelf(); fetch.mockClear();
    await millsStore.fetchRescueShelf(); expect(fetch).toHaveBeenCalledTimes(2);
    suffix = '-next'; await millsStore.fetchRescueShelf();
    expect(millsStore.rescueRunsByItem.a.detail?.run.ID).toBe('run-a-next');
    fetch.mockClear(); items = [{ ...items[0], UpdatedAt: 'two' }]; await millsStore.fetchRescueShelf();
    expect(fetch).toHaveBeenCalledTimes(3);
    items = []; await millsStore.fetchRescueShelf(); expect(millsStore.rescueRunsByItem).toEqual({});
  });
  it('retries unavailable detail without presenting stale evidence', async () => {
    const fetch = serve(() => [item('a')]);
    const serveNormally = fetch.getMockImplementation()!;
    let fail = true;
    fetch.mockImplementation(async (...args) => {
      if (fail && String(args[0]).includes('/runs/run-')) return new Response('unavailable', { status: 503 });
      return serveNormally(...args);
    });
    await millsStore.fetchRescueShelf();
    expect(millsStore.rescueRunsByItem.a).toBeUndefined();
    expect(millsStore.rescueError).toContain('unavailable');
    fail = false; await millsStore.fetchRescueShelf();
    expect(millsStore.rescueRunsByItem.a.detail?.run.ID).toBe('run-a');
    expect(millsStore.rescueError).toBeNull();
  });
  it('coalesces concurrent passes and retains last-good items on fetch errors', async () => {
    const fetch = serve(() => [item('a')]);
    await Promise.all([millsStore.fetchRescueShelf(), millsStore.fetchRescueShelf()]);
    expect(fetch).toHaveBeenCalledTimes(3);
    fetch.mockRejectedValue(new Error('offline')); await millsStore.fetchRescueShelf();
    expect(millsStore.rescueItems).toHaveLength(1); expect(millsStore.rescueError).toContain('offline');
  });
});
