import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import BacklogDetail from './BacklogDetail.svelte';
import { millsStore, type BacklogItemDetail } from '../../stores/mills.svelte.ts';

let target: HTMLElement;
let component: Record<string, unknown>;

const ITEM: BacklogItemDetail = {
  ID: 'bl-arc',
  Title: 'Per-repo protected_paths overlay',
  State: 'merged',
  Priority: 'P1',
  TargetProject: 'services/flexdeck',
  CreatedBy: 'operator-directed',
  Labels: ['operator-directed'],
};

beforeEach(() => {
  vi.spyOn(millsStore, 'ensureWorkflowRunsLoaded').mockResolvedValue(undefined);
  millsStore.selectedBacklogID = 'bl-arc';
  millsStore.backlogDetailByID = { 'bl-arc': { status: 'loaded', detail: ITEM } };
  millsStore.backlogEventsByID = {};
  millsStore.pipelineRuns = [];
  millsStore.pipelineHistory = [];
  millsStore.workflowRuns = [];
  target = document.createElement('div');
  document.body.appendChild(target);
});

afterEach(() => {
  void unmount(component);
  vi.restoreAllMocks();
  millsStore.selectedBacklogID = null;
  millsStore.backlogDetailByID = {};
  millsStore.backlogEventsByID = {};
  millsStore.pipelineRuns = [];
  millsStore.pipelineHistory = [];
  target.remove();
});

function render(): void {
  component = mount(BacklogDetail, { target }) as Record<string, unknown>;
  flushSync();
}

describe('BacklogDetail provenance chips', () => {
  it('names the target repo and the origin in the header', () => {
    render();
    expect(target.querySelector('[data-repo]')?.getAttribute('data-repo')).toBe('flexdeck');
    expect(target.querySelector('[data-origin]')?.getAttribute('data-origin')).toBe('operator');
  });
});

describe('BacklogDetail journey strip', () => {
  it('renders a transaction reservation deferral in the deferred item journey', () => {
    const deferral = {
      ID: 1, OccurredAt: '2026-09-15T21:00:00Z', Actor: 'reconciler',
      Kind: 'reconciler.deferred', SubjectKind: 'backlog_item', SubjectID: 'bl-arc',
      Payload: { outcome: 'scope_reservation_transaction', reason: 'scope_reserved', blocked_by: 'older', witness: 'pkg/shared.go' },
    };
    millsStore.backlogEventsByID = {
      'bl-arc': {
        status: 'loaded',
        ledger: {
          backlog_id: 'bl-arc', partial: true,
          events: [deferral],
        },
      },
    };
    render();
    expect(target.querySelector('.jlabel')?.textContent?.trim()).toBe('deferred');
    expect(target.textContent).toContain('reason=scope_reserved');
  });

  it('renders the escalate → requeue → merge arc from events and runs together', () => {
    millsStore.backlogEventsByID = {
      'bl-arc': {
        status: 'loaded',
        ledger: {
          backlog_id: 'bl-arc',
          partial: true,
          events: [
            {
              ID: 2,
              OccurredAt: '2026-08-16T14:00:00Z',
              Actor: 'reconciler',
              Kind: 'reconciler.auto_requeued',
              SubjectKind: 'backlog_item',
              SubjectID: 'bl-arc',
              Payload: { class: 'infra' },
            },
            {
              ID: 1,
              OccurredAt: '2026-08-16T10:00:00Z',
              Actor: 'reconciler',
              Kind: 'reconciler.bootstrap_escalated',
              SubjectKind: 'backlog_item',
              SubjectID: 'bl-arc',
              Payload: null,
            },
          ],
        },
      },
    };
    millsStore.pipelineHistory = [
      {
        ID: 'run-a',
        BacklogID: 'bl-arc',
        Template: 'default',
        State: 'merged',
        Attempts: 1,
        StartedAt: '2026-08-16T15:00:00Z',
        EndedAt: '2026-08-16T16:00:00Z',
        MRIID: 99,
      },
    ];
    render();

    const labels = [...target.querySelectorAll('.jlabel')].map((n) => n.textContent?.trim());
    // Newest-first: the merge, then its start, then the requeue, then the escalation.
    expect(labels).toEqual(['run merged', 'run started', 'auto requeued', 'bootstrap escalated']);
    expect(target.querySelector('.jrow')?.className).toContain('tone-success');
    expect(target.textContent).toContain('mr=!99');
  });

  it('captions itself honestly rather than implying a complete history', () => {
    // Not every state change writes an event; a strip that reads as a full
    // lifecycle would be a lie the operator eventually debugs against.
    millsStore.backlogEventsByID = {
      'bl-arc': {
        status: 'loaded',
        ledger: { backlog_id: 'bl-arc', partial: true, events: [] },
      },
    };
    millsStore.pipelineHistory = [
      {
        ID: 'run-a',
        BacklogID: 'bl-arc',
        Template: 'default',
        State: 'merged',
        Attempts: 1,
        StartedAt: '2026-08-16T15:00:00Z',
        EndedAt: '2026-08-16T16:00:00Z',
      },
    ];
    render();
    expect(target.querySelector('.jnote')?.textContent).toContain('gaps in the log, not in the work');
  });

  it('says nothing is recorded rather than rendering an empty rail', () => {
    millsStore.backlogEventsByID = {
      'bl-arc': { status: 'loaded', ledger: { backlog_id: 'bl-arc', partial: true, events: [] } },
    };
    render();
    expect(target.querySelector('.journey')).toBeNull();
    expect(target.textContent).toContain('Nothing recorded yet');
  });

  it('keeps the drawer usable when the ledger feed fails', () => {
    // The ledger is an enrichment: its failure must not take the drawer down.
    millsStore.backlogEventsByID = {
      'bl-arc': { status: 'error', message: 'boom' },
    };
    render();
    expect(target.textContent).toContain('History unavailable');
    expect(target.textContent).toContain('Per-repo protected_paths overlay');
  });
});
