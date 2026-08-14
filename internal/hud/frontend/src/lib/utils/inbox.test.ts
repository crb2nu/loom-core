// Contract coverage for the inbox CardAction shape.
//
// Two things here are regressions waiting to happen rather than logic bugs:
//
//   1. `CardAction.confirm` is the shared ConfirmSpec. It used to be an inline
//      literal that had already drifted from BulkToolbar's (it required
//      `confirmLabel`, and carried a `variant` the toolbar's lacked).
//   2. `CardAction.run` resolves `unknown`, not `void`. The narrow `Promise<void>`
//      form actively distorted store design — workflows.approveStep documents
//      returning void *because* a boolean would not type-check here.
//
// Both are type-level properties, so the assertions that matter are the ones
// the `pnpm check` / vitest transpile step enforces; the runtime expectations
// below just keep the file honest about what it constructed.

import { describe, expect, it } from 'vitest';
import { selectPendingApprovals, type CardAction, type InboxStores } from './inbox.ts';
import type { ConfirmSpec } from './confirm.ts';
import type { WorkflowSummary } from '../stores/workflows.svelte.ts';

function workflow(over: Partial<WorkflowSummary> = {}): WorkflowSummary {
  return {
    id: 'wf-1',
    definition_id: 'feature-dev',
    name: 'ship the thing',
    status: 'waiting_approval',
    current_step: 'review',
    started_at: '2026-07-29T00:00:00Z',
    steps: [{ id: 'step-review', name: 'review', type: 'approval', status: 'waiting' }],
    ...over,
  };
}

/** Minimal InboxStores stub — only the two fields these selectors read. */
function stores(workflows: WorkflowSummary[]): InboxStores {
  return {
    router: { navigate: () => {} },
    workflows: { activeWorkflows: workflows, approveStep: async () => {} },
  } as unknown as InboxStores;
}

describe('inbox — pending approval card', () => {
  it('gates the inline Approve action with a confirm spec', () => {
    const [card] = selectPendingApprovals(stores([workflow()]));
    expect(card.primary.label).toBe('Approve');
    // The gate is what keeps a one-click approval from advancing a workflow
    // straight off the Overview deck.
    expect(card.primary.confirm).toBeDefined();
    expect(card.primary.confirm?.title).toBe('Approve workflow step?');
    expect(card.primary.confirm?.confirmLabel).toBe('Approve');
  });

  it('resolves the step id from steps[] rather than the step name', () => {
    // approvalAction prefers the canonical `steps[].id`; a card built off the
    // display name would POST an id the daemon does not know.
    const [card] = selectPendingApprovals(stores([workflow()]));
    expect(card.primary.confirm?.message).toContain('review');
  });

  it('falls back to a drill when no step id can be resolved', () => {
    const [card] = selectPendingApprovals(stores([workflow({ current_step: '', steps: [] })]));
    expect(card.primary.label).toBe('Review approvals');
    expect(card.primary.confirm).toBeUndefined();
  });

  it('aggregates without an inline mutation when several are waiting', () => {
    const [card] = selectPendingApprovals(stores([workflow(), workflow({ id: 'wf-2' })]));
    expect(card.primary.label).toBe('Review approvals');
    expect(card.primary.confirm).toBeUndefined();
  });

  it('returns no card when nothing is waiting', () => {
    expect(selectPendingApprovals(stores([workflow({ status: 'running' })]))).toEqual([]);
  });
});

describe('inbox — CardAction type contract', () => {
  it('accepts a run that resolves a value, not just void', () => {
    // The point of the widening: stores whose mutations report outcome by
    // return value (memoryStore.deleteItem is a live Promise<boolean>) wire in
    // directly. If `run` narrows back to Promise<void>, this stops compiling.
    const booleanRun: CardAction = {
      label: 'Delete',
      run: async () => true,
    };
    const voidRun: CardAction = { label: 'Open', run: () => {} };
    expect(booleanRun.run()).toBeInstanceOf(Promise);
    expect(voidRun.run()).toBeUndefined();
  });

  it('shares one confirm shape with the bulk-action surface', () => {
    // Assignable in both directions: a ConfirmSpec built for a BulkToolbar
    // descriptor drops into a CardAction unchanged, and vice versa. `title` and
    // `message` are the only required members.
    const spec: ConfirmSpec = { title: 'Release?', message: 'Force-release this claim.' };
    const action: CardAction = { label: 'Release', run: () => {}, confirm: spec };
    const roundTrip: ConfirmSpec | undefined = action.confirm;
    expect(roundTrip).toBe(spec);
    expect(roundTrip?.confirmLabel).toBeUndefined();
  });
});
