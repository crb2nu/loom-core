import { describe, expect, it } from 'vitest';

import {
  mergeSessionCalls,
  RECENT_CALLS_PER_SESSION,
  type ToolCall,
} from './liveSessions.svelte.ts';

function call(id: string, timestamp: string, source: ToolCall['source'] = 'context'): ToolCall {
  return {
    call_id: id,
    tool_name: source === 'context' ? 'finding' : 'tool',
    started_at: timestamp,
    ended_at: timestamp,
    in_flight: false,
    source,
  };
}

describe('mergeSessionCalls', () => {
  it('links context backfill into the same newest-first session activity list', () => {
    const live = call('tool-1', '2026-07-14T12:00:00Z', 'tool');
    const context = call('context-1', '2026-07-14T12:01:00Z');

    expect(mergeSessionCalls([live], [context]).map((item) => item.call_id)).toEqual([
      'context-1',
      'tool-1',
    ]);
  });

  it('preserves richer live entries when a backfill returns the same id', () => {
    const live = { ...call('shared', '2026-07-14T12:00:00Z', 'tool'), result_summary: 'live' };
    const backfill = { ...call('shared', '2026-07-14T12:01:00Z'), result_summary: 'backfill' };

    expect(mergeSessionCalls([live], [backfill])).toEqual([live]);
  });

  it('caps merged activity at the shared session limit', () => {
    const incoming = Array.from({ length: RECENT_CALLS_PER_SESSION + 4 }, (_, index) =>
      call(`context-${index}`, new Date(Date.UTC(2026, 6, 14, 12, index)).toISOString()),
    );

    expect(mergeSessionCalls([], incoming)).toHaveLength(RECENT_CALLS_PER_SESSION);
  });
});
