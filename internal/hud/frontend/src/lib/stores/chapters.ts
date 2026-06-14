// Pure reducer + selectors for session "chapters" — milestones an agent marks
// with the Claude Code `mark_chapter` tool, surfaced on the Fleet table next to
// the conversation they belong to. The daemon emits a `chapter.marked` event
// (cmd/loom/cmd_agent_event_emit.go) per mark; this module turns the stream of
// those payloads into per-conversation chapter lists. Rune-free so it is
// unit-testable via the tsx fixture (chapters.fixture.ts).

import { conversationId } from '../utils/agents.ts';

export interface ChapterEntry {
  title: string;
  summary?: string;
  /** ISO timestamp the chapter was marked (server `marked_at`). */
  markedAt: string;
  sessionId?: string;
  agentId?: string;
}

/** Shape of the `chapter.marked` SSE payload (cmd_agent_event_emit.go). */
export interface ChapterMarkedData {
  agent_id?: string;
  session_id?: string;
  title?: string;
  summary?: string;
  marked_at?: string;
  tool_use_id?: string;
}

export const MAX_CHAPTERS_PER_CONVERSATION = 25;

// chapterKey returns the conversation bucket a chapter belongs to — the SAME
// identity the Fleet table groups rows by (conversationId), so a chapter marked
// while a chat was in repo A still shows on that chat's row in repo B.
export function chapterKey(agentId: string | null | undefined): string {
  return conversationId(agentId) || (agentId ?? '').trim();
}

// toChapterEntry normalizes a `chapter.marked` payload, or returns null when it
// lacks a usable title (defensive — the server requires one, but SSE replay or
// a future producer might not).
export function toChapterEntry(data: ChapterMarkedData | null | undefined): ChapterEntry | null {
  if (!data) return null;
  const title = (data.title ?? '').trim();
  if (!title) return null;
  return {
    title,
    summary: (data.summary ?? '').trim() || undefined,
    markedAt: (data.marked_at ?? '').trim(),
    sessionId: data.session_id,
    agentId: data.agent_id,
  };
}

// appendChapter returns a new list with `entry` appended, de-duplicated by
// (title, markedAt) so the SSE replay a reconnecting HUD receives doesn't
// double-count, and capped to the most recent `cap` entries.
export function appendChapter(
  existing: ChapterEntry[],
  entry: ChapterEntry,
  cap = MAX_CHAPTERS_PER_CONVERSATION,
): ChapterEntry[] {
  const dup = existing.some((c) => c.title === entry.title && c.markedAt === entry.markedAt);
  const next = dup ? existing.slice() : [...existing, entry];
  return next.length > cap ? next.slice(next.length - cap) : next;
}
