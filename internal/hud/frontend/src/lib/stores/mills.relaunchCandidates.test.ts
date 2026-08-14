import { afterEach, describe, expect, it, vi } from 'vitest';
import { millsStore } from './mills.svelte.ts';

const realFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = realFetch;
  millsStore.relaunchCandidates = [];
  millsStore.relaunchCandidatesLoading = false;
  millsStore.relaunchCandidatesError = null;
});

describe('millsStore.fetchRelaunchCandidates', () => {
  it('GETs the projection and normalizes its PascalCase rows', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify([{
        ID: 'bl-42',
        Title: 'Repair the loom',
        EscalationClass: 'infra',
        FailureClass: 'infrastructure',
        EndedAt: '2026-08-09T10:00:00Z',
      }]), { status: 200 }),
    );
    globalThis.fetch = fetchMock;

    await millsStore.fetchRelaunchCandidates();

    expect(fetchMock).toHaveBeenCalledWith(
      '/api/mills/escalations/relaunch-candidates',
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    expect(millsStore.relaunchCandidates).toEqual([{
      backlogId: 'bl-42',
      title: 'Repair the loom',
      escalationClass: 'infra',
      failureClass: 'infrastructure',
      latestRunEndedAt: '2026-08-09T10:00:00Z',
    }]);
    expect(millsStore.relaunchCandidatesError).toBeNull();
  });

  it('marks the queue unavailable without replacing last-good rows', async () => {
    millsStore.relaunchCandidates = [{
      backlogId: 'cached', title: '', escalationClass: 'config', failureClass: '', latestRunEndedAt: null,
    }];
    globalThis.fetch = vi.fn().mockResolvedValue(new Response('boom', { status: 500 }));

    await millsStore.fetchRelaunchCandidates();

    expect(millsStore.relaunchCandidates).toHaveLength(1);
    expect(millsStore.relaunchCandidatesError).toContain('500');
    expect(millsStore.relaunchCandidatesLoading).toBe(false);
  });
});
