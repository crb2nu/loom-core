import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import BacklogDetail from './BacklogDetail.svelte';
import {
  millsStore,
  type BacklogItemDetail,
  type PipelineRun,
  type StageResult,
  type GateOutcome,
} from '../../stores/mills.svelte.ts';

// Coverage for the attention block's inline failure evidence: an escalated
// item's drawer must answer "what actually broke?" (failing stage + log tail)
// without the operator drilling run → stage → log for every triaged item.

let target: HTMLElement;
let component: Record<string, unknown>;

const ITEM: BacklogItemDetail = {
  ID: 'bl-esc',
  Title: 'Cloth Hall S2 shift ledger',
  State: 'escalated',
  Priority: 'P2',
  CreatedBy: 'council',
};

const RUN: PipelineRun = {
  ID: 'PIPE-esc-1',
  BacklogID: 'bl-esc',
  Template: 'default',
  State: 'escalated',
  CurrentStage: 'tests',
  Attempts: 3,
  StartedAt: '2026-08-29T04:30:00Z',
  EndedAt: '2026-08-29T04:42:00Z',
};

function stage(over: Partial<StageResult>): StageResult {
  return {
    ID: 1,
    PipelineRunID: 'PIPE-esc-1',
    Stage: 'tests',
    Attempt: 1,
    StartedAt: '2026-08-29T04:31:00Z',
    CostUSD: 0,
    ...over,
  } as StageResult;
}

beforeEach(() => {
  vi.spyOn(millsStore, 'ensureWorkflowRunsLoaded').mockResolvedValue(undefined);
  // The effect calls this on open; the cache is seeded directly per test, so
  // the network never enters the picture.
  vi.spyOn(millsStore, 'ensureRunDetailLoaded').mockImplementation(() => {});
  millsStore.selectedBacklogID = 'bl-esc';
  millsStore.backlogDetailByID = { 'bl-esc': { status: 'loaded', detail: ITEM } };
  millsStore.backlogEventsByID = {};
  millsStore.backlogRunsByID = {};
  millsStore.pipelineRuns = [];
  millsStore.pipelineHistory = [RUN];
  millsStore.workflowRuns = [];
  millsStore.pipelineDetailByRun = {};
  target = document.createElement('div');
  document.body.appendChild(target);
});

afterEach(() => {
  void unmount(component);
  vi.restoreAllMocks();
  millsStore.selectedBacklogID = null;
  millsStore.backlogDetailByID = {};
  millsStore.backlogEventsByID = {};
  millsStore.backlogRunsByID = {};
  millsStore.pipelineRuns = [];
  millsStore.pipelineHistory = [];
  millsStore.pipelineDetailByRun = {};
  target.remove();
});

function render(): void {
  component = mount(BacklogDetail, { target }) as Record<string, unknown>;
  flushSync();
}

describe('BacklogDetail failure evidence', () => {
  it('inlines the newest failing stage and its log tail', () => {
    millsStore.pipelineDetailByRun = {
      'PIPE-esc-1': {
        status: 'loaded',
        detail: {
          run: RUN,
          stages: [
            stage({ ID: 1, Attempt: 1, Outcome: 'error', LogTail: 'older attempt noise' }),
            stage({
              ID: 2,
              Attempt: 3,
              StartedAt: '2026-08-29T04:40:00Z',
              Outcome: 'error',
              LogTail: 'FAIL lint:parity (3482ms)\n0 issues.\n1/2 checks failed',
            }),
            stage({ ID: 3, Stage: 'implement', Attempt: 1, Outcome: 'success' }),
          ],
          gates: [],
        },
      },
    };
    render();

    const head = target.querySelector('.evidence-head');
    expect(head?.textContent).toContain('tests');
    expect(head?.textContent).toContain('attempt 3');
    expect(target.querySelector('.evidence-log')?.textContent).toContain('1/2 checks failed');
    // The older attempt's tail must not win over the newest failure.
    expect(target.querySelector('.evidence-log')?.textContent).not.toContain('older attempt noise');
  });

  it('falls back to failed gate reasons when the stage has no log tail', () => {
    const gate: GateOutcome = {
      ID: 1,
      PipelineRunID: 'PIPE-esc-1',
      AfterStage: 'tests',
      GateName: 'spec_conformance',
      Outcome: 'fail',
      Reasons: ['diff touches files outside the slice scope'],
      EvaluatedAt: '2026-08-29T04:41:00Z',
    };
    millsStore.pipelineDetailByRun = {
      'PIPE-esc-1': {
        status: 'loaded',
        detail: {
          run: RUN,
          stages: [stage({ Outcome: 'gate_fail', LogTail: '' })],
          gates: [gate],
        },
      },
    };
    render();

    expect(target.querySelector('.evidence-log')?.textContent).toContain(
      'spec_conformance: diff touches files outside the slice scope',
    );
  });

  it('shows no evidence block while the run detail is still loading', () => {
    millsStore.pipelineDetailByRun = { 'PIPE-esc-1': { status: 'loading' } };
    render();

    // The attention block itself (with the open-run link) must still render.
    expect(target.querySelector('.attention')).not.toBeNull();
    expect(target.querySelector('.evidence')).toBeNull();
  });
});
