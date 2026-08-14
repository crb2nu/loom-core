// Reachability coverage for MergeQueueSection's two empty states.
//
// The "✓ No branches in merge queue" copy is an assertion that every branch was
// evaluated and none qualified. On a failed fetch that assertion is false — an
// unreachable queue is not an empty queue — yet the ✓ was exactly what rendered,
// because the only condition guarding it was `ready.length === 0 &&
// blocked.length === 0`. These pin the ✓ as unreachable whenever the store
// carries an error, in both the rows-present and rows-absent directions.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { render } from 'svelte/server';
import MergeQueueSection from './MergeQueueSection.svelte';
import { mergeQueueStore } from '../../stores/mergeQueue.svelte.ts';
import type { MergeCandidate } from '../../clients/mergeQueue.ts';

const OK_COPY = 'No branches in merge queue';
const FAIL_COPY = 'Merge queue unavailable';
const REFRESH_COPY = 'Merge queue refresh failed';

function candidate(over: Partial<MergeCandidate> = {}): MergeCandidate {
  return {
    agent_id: 'claude-code',
    branch: 'feat/weave',
    namespace: 'loom-core',
    status: 'ready',
    merge_ready: true,
    merge_blockers: [],
    conflict_files: 0,
    blocked_tasks: 0,
    task_count: 1,
    ...over,
  };
}

/** Put the singleton store into a known state; SSR reads it at render time. */
function setStore(opts: { error?: string | null; ready?: MergeCandidate[]; blocked?: MergeCandidate[] }) {
  mergeQueueStore.error = opts.error ?? null;
  mergeQueueStore.ready = opts.ready ?? [];
  mergeQueueStore.blocked = opts.blocked ?? [];
}

function html(): string {
  return render(MergeQueueSection, { props: { collapsed: false } }).body;
}

beforeEach(() => setStore({}));
afterEach(() => setStore({}));

describe('MergeQueueSection — the ✓ empty state is unreachable under error', () => {
  it('shows the outage empty state, not the ✓, when the queue is unreachable and empty', () => {
    setStore({ error: 'daemon unreachable' });
    const out = html();
    expect(out).toContain(FAIL_COPY);
    expect(out).toContain('daemon unreachable');
    expect(out).not.toContain(OK_COPY);
  });

  it('shows neither empty state when the fetch failed but rows are still on screen', () => {
    // A refresh failure over a populated queue keeps the tables and degrades to
    // a banner — the ✓ must not appear here either.
    setStore({ error: 'poll timed out', ready: [candidate()] });
    const out = html();
    expect(out).toContain(REFRESH_COPY);
    expect(out).not.toContain(OK_COPY);
    expect(out).not.toContain(FAIL_COPY);
  });

  it('holds when only the blocked list is populated', () => {
    setStore({ error: 'poll timed out', blocked: [candidate({ merge_ready: false, status: 'blocked' })] });
    const out = html();
    expect(out).not.toContain(OK_COPY);
  });

  it('never renders the ✓ for any error/rows combination', () => {
    // Exhaustive over the branch matrix the template can reach with an error
    // set — the ✓ is the one cell that must stay empty in every row.
    const rowShapes: Array<{ ready: MergeCandidate[]; blocked: MergeCandidate[] }> = [
      { ready: [], blocked: [] },
      { ready: [candidate()], blocked: [] },
      { ready: [], blocked: [candidate({ merge_ready: false })] },
      { ready: [candidate()], blocked: [candidate({ branch: 'fix/x', merge_ready: false })] },
    ];
    for (const shape of rowShapes) {
      setStore({ error: 'daemon unreachable', ...shape });
      expect(html()).not.toContain(OK_COPY);
    }
  });
});

describe('MergeQueueSection — the ✓ empty state is reachable when healthy', () => {
  it('shows the ✓ when the queue genuinely evaluated to empty', () => {
    setStore({ error: null });
    const out = html();
    expect(out).toContain(OK_COPY);
    expect(out).not.toContain(FAIL_COPY);
  });

  it('shows no empty state at all when healthy with rows', () => {
    setStore({ error: null, ready: [candidate()] });
    const out = html();
    expect(out).not.toContain(OK_COPY);
    expect(out).not.toContain(FAIL_COPY);
    expect(out).not.toContain(REFRESH_COPY);
  });
});

describe('MergeQueueSection — collapsed', () => {
  it('renders no empty state either way while collapsed', () => {
    setStore({ error: 'daemon unreachable' });
    const out = render(MergeQueueSection, { props: { collapsed: true } }).body;
    expect(out).not.toContain(OK_COPY);
    expect(out).not.toContain(FAIL_COPY);
  });
});

describe('MergeQueueSection — joined merge request', () => {
  it('renders the MR state and real link instead of the new-MR CTA', () => {
    setStore({ blocked: [candidate({
      merge_ready: false,
      mr_iid: 42,
      mr_state: 'conflict',
      mr_web_url: 'https://gitlab.example.com/group/project/-/merge_requests/42',
      merge_request_new_url: 'https://gitlab.example.com/group/project/-/merge_requests/new',
    })] });
    const out = html();
    expect(out).toContain('conflict');
    expect(out).toContain('MR !42');
    expect(out).toContain('https://gitlab.example.com/group/project/-/merge_requests/42');
    expect(out).not.toContain('New MR');
    expect(out).not.toContain('/merge_requests/new');
  });

  it('preserves the new-MR CTA for an unjoined ready candidate', () => {
    const newMRURL = 'https://gitlab.example.com/group/project/-/merge_requests/new?merge_request[source_branch]=feat%2Fweave';
    setStore({ ready: [candidate({ merge_request_new_url: newMRURL })] });
    const out = html();
    expect(out).toContain('New MR');
    expect(out).toContain(newMRURL.replaceAll('&', '&amp;'));
  });
});
