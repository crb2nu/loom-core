// Runnable fixture / smoke test for the pure chapters reducer (chapters.ts).
// The HUD ships no test runner, so this self-checks via:
//   pnpm dlx tsx src/lib/stores/chapters.fixture.ts

import {
  appendChapter,
  chapterKey,
  toChapterEntry,
  MAX_CHAPTERS_PER_CONVERSATION,
  type ChapterEntry,
} from './chapters.ts';

function expect(label: string, actual: unknown, want: unknown): boolean {
  const ok = actual === want;
  if (ok) console.log(`PASS ${label}: got=${String(actual)}`);
  else console.error(`FAIL ${label}: got=${String(actual)} want=${String(want)}`);
  return ok;
}

let allOk = true;

// chapterKey buckets by conversation, matching the Fleet table grouping: a
// chapter marked while the chat was in one repo (WS_HASH) belongs to the same
// conversation when it moves to another.
allOk = expect(
  'chapterKey strips ws-hash to the conversation',
  chapterKey('claude-code-3749726816-1105899468'),
  'claude-code-1105899468',
) && allOk;
allOk = expect(
  'sibling-repo member of the same chat shares the key',
  chapterKey('claude-code-401508988-1105899468'),
  'claude-code-1105899468',
) && allOk;
allOk = expect('empty agent id → empty key', chapterKey(''), '') && allOk;

// toChapterEntry requires a title; trims summary; drops empty summary.
allOk = expect(
  'no title → null entry',
  toChapterEntry({ title: '   ', summary: 'x' }),
  null,
) && allOk;
const entry = toChapterEntry({
  agent_id: 'claude-code-401508988-1105899468',
  session_id: 's1',
  title: '  Test verification  ',
  summary: '  ran suite green  ',
  marked_at: '2026-06-14T20:00:00Z',
});
allOk = expect('title trimmed', entry?.title, 'Test verification') && allOk;
allOk = expect('summary trimmed', entry?.summary, 'ran suite green') && allOk;
const noSummary = toChapterEntry({ title: 'A', summary: '   ' });
allOk = expect('blank summary → undefined', noSummary?.summary, undefined) && allOk;

// appendChapter de-dupes by (title, markedAt) so SSE replay doesn't double-count.
const c1: ChapterEntry = { title: 'Exploration', markedAt: 't1' };
const c2: ChapterEntry = { title: 'Implementation', markedAt: 't2' };
let list = appendChapter([], c1);
list = appendChapter(list, c2);
allOk = expect('two distinct chapters appended', list.length, 2) && allOk;
list = appendChapter(list, { title: 'Exploration', markedAt: 't1' });
allOk = expect('replayed identical chapter is de-duped', list.length, 2) && allOk;
allOk = expect('newest chapter is last', list[list.length - 1]?.title, 'Implementation') && allOk;

// appendChapter caps to the most-recent MAX_CHAPTERS_PER_CONVERSATION.
let capped: ChapterEntry[] = [];
for (let i = 0; i < MAX_CHAPTERS_PER_CONVERSATION + 5; i += 1) {
  capped = appendChapter(capped, { title: `ch-${i}`, markedAt: `t-${i}` });
}
allOk = expect('list capped at max', capped.length, MAX_CHAPTERS_PER_CONVERSATION) && allOk;
allOk = expect('oldest chapters dropped (newest kept)', capped[capped.length - 1]?.title, `ch-${MAX_CHAPTERS_PER_CONVERSATION + 4}`) && allOk;

if (!allOk) {
  console.error('chapters fixture: FAILURES detected');
  throw new Error('fixture failed');
}
console.log('chapters fixture: all cases pass');
