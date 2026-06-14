// Reactive store wrapping the pure chapters reducer (chapters.ts). Listens to
// the daemon's `chapter.marked` SSE events and exposes per-conversation chapter
// lists the Fleet table reads to show "latest chapter" + count on a row.

import { eventStore } from './events.svelte.ts';
import {
  appendChapter,
  chapterKey,
  toChapterEntry,
  type ChapterEntry,
  type ChapterMarkedData,
} from './chapters.ts';

class ChaptersStore {
  // Keyed by conversationId (chapterKey). Reassigned on each update so Svelte's
  // fine-grained reactivity sees the change.
  private byConversation = $state(new Map<string, ChapterEntry[]>());
  private connected = false;
  private unsub: (() => void) | null = null;

  /** Idempotent — safe to call from multiple components' onMount. */
  connect() {
    if (this.connected) return;
    this.connected = true;
    this.unsub = eventStore.subscribe<ChapterMarkedData>('chapter.marked', (data) =>
      this.ingest(data),
    );
  }

  disconnect() {
    this.unsub?.();
    this.unsub = null;
    this.connected = false;
  }

  private ingest(data: ChapterMarkedData) {
    const entry = toChapterEntry(data);
    if (!entry) return;
    const key = chapterKey(data.agent_id);
    if (!key) return;
    const next = new Map(this.byConversation);
    next.set(key, appendChapter(next.get(key) ?? [], entry));
    this.byConversation = next;
  }

  /** All chapters for an agent's conversation, oldest → newest. */
  forAgent(agentId: string | null | undefined): ChapterEntry[] {
    return this.byConversation.get(chapterKey(agentId)) ?? [];
  }

  /** Most recent chapter for an agent's conversation, or null. */
  latestForAgent(agentId: string | null | undefined): ChapterEntry | null {
    const list = this.forAgent(agentId);
    return list.length ? list[list.length - 1] : null;
  }

  /** Number of chapters marked in an agent's conversation. */
  countForAgent(agentId: string | null | undefined): number {
    return this.forAgent(agentId).length;
  }
}

export const chaptersStore = new ChaptersStore();
