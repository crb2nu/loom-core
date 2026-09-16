import { describe, expect, it } from 'vitest';
import {
  appendChapter,
  chapterKey,
  toChapterEntry,
  MAX_CHAPTERS_PER_CONVERSATION,
  type ChapterEntry,
} from './chapters.ts';

// Unit coverage for the pure chapters reducer (chapters.ts).

// chapterKey buckets by conversation, matching the Fleet table grouping: a
// chapter marked while the chat was in one repo (WS_HASH) belongs to the same
// conversation when it moves to another.
describe('chapters: chapterKey', () => {
  it('chapterKey strips ws-hash to the conversation', () => {
    expect(chapterKey('claude-code-3749726816-1105899468')).toBe('claude-code-1105899468');
  });

  it('sibling-repo member of the same chat shares the key', () => {
    expect(chapterKey('claude-code-401508988-1105899468')).toBe('claude-code-1105899468');
  });

  it('empty agent id → empty key', () => {
    expect(chapterKey('')).toBe('');
  });
});

// toChapterEntry requires a title; trims summary; drops empty summary.
describe('chapters: toChapterEntry', () => {
  const entry = toChapterEntry({
    agent_id: 'claude-code-401508988-1105899468',
    session_id: 's1',
    title: '  Test verification  ',
    summary: '  ran suite green  ',
    marked_at: '2026-06-14T20:00:00Z',
  });

  it('no title → null entry', () => {
    expect(toChapterEntry({ title: '   ', summary: 'x' })).toBe(null);
  });

  it('title trimmed', () => {
    expect(entry?.title).toBe('Test verification');
  });

  it('summary trimmed', () => {
    expect(entry?.summary).toBe('ran suite green');
  });

  it('blank summary → undefined', () => {
    const noSummary = toChapterEntry({ title: 'A', summary: '   ' });
    expect(noSummary?.summary).toBe(undefined);
  });
});

// appendChapter de-dupes by (title, markedAt) so SSE replay doesn't double-count.
describe('chapters: appendChapter', () => {
  const c1: ChapterEntry = { title: 'Exploration', markedAt: 't1' };
  const c2: ChapterEntry = { title: 'Implementation', markedAt: 't2' };
  const twoAppended = appendChapter(appendChapter([], c1), c2);
  const afterReplay = appendChapter(twoAppended, { title: 'Exploration', markedAt: 't1' });

  // appendChapter caps to the most-recent MAX_CHAPTERS_PER_CONVERSATION.
  const capped = (() => {
    let list: ChapterEntry[] = [];
    for (let i = 0; i < MAX_CHAPTERS_PER_CONVERSATION + 5; i += 1) {
      list = appendChapter(list, { title: `ch-${i}`, markedAt: `t-${i}` });
    }
    return list;
  })();

  it('two distinct chapters appended', () => {
    expect(twoAppended.length).toBe(2);
  });

  it('replayed identical chapter is de-duped', () => {
    expect(afterReplay.length).toBe(2);
  });

  it('newest chapter is last', () => {
    expect(afterReplay[afterReplay.length - 1]?.title).toBe('Implementation');
  });

  it('list capped at max', () => {
    expect(capped.length).toBe(MAX_CHAPTERS_PER_CONVERSATION);
  });

  it('oldest chapters dropped (newest kept)', () => {
    expect(capped[capped.length - 1]?.title).toBe(`ch-${MAX_CHAPTERS_PER_CONVERSATION + 4}`);
  });
});
