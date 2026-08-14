// Render coverage for WorkflowRunDetail, mounted client-side.
//
// This drawer is the one that GAINED behaviour in the DetailDrawer collapse:
// the a11y pass that gave PipelineRunDetail a focus trap skipped this file, so
// until now it was a modal `<aside>` with no trap and no dialogStore claim —
// Tab walked straight out into the page behind it and window-level shortcuts
// stayed armed. Inheriting DetailDrawer's chrome fixes that; these cases pin
// the fix plus the close paths (Escape / backdrop / ✕), all of which funnel
// through one close().
//
// A `.dom.test.ts` on purpose: the trap and the key handling are listeners,
// and svelte/server renders the markup without attaching any.

import { afterEach, describe, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import WorkflowRunDetail from './WorkflowRunDetail.svelte';
import { millsStore, type WorkflowRunDetail as Detail } from '../../stores/mills.svelte.ts';
import { dialogStore } from '../../stores/dialogs.svelte.ts';

const RUN_ID = 'wf-dom-1';

let cleanup: (() => void) | null = null;

afterEach(() => {
  cleanup?.();
  cleanup = null;
  millsStore.closeWorkflowDetail();
  millsStore.workflowDetailByID = {};
  document.body.innerHTML = '';
});

function detail(): Detail {
  return {
    run: { id: RUN_ID, state: 'running', engine: 'durable', template: 'feature-dev' },
    steps: [],
  } as unknown as Detail;
}

function openLoaded(): HTMLElement {
  millsStore.selectedWorkflowID = RUN_ID;
  millsStore.workflowDetailByID = { [RUN_ID]: { status: 'loaded', detail: detail() } };
  const target = document.createElement('div');
  document.body.appendChild(target);
  const component = mount(WorkflowRunDetail, { target });
  cleanup = () => {
    void unmount(component);
    target.remove();
  };
  flushSync();
  return target;
}

describe('WorkflowRunDetail — drawer chrome', () => {
  it('renders a labelled modal dialog around the step timeline', () => {
    const target = openLoaded();

    const dialog = target.querySelector('[role="dialog"]');
    expect(dialog?.getAttribute('aria-modal')).toBe('true');
    expect(dialog?.getAttribute('aria-label')).toBe('Workflow run detail');
    // The ✕ keeps its own surface-specific name rather than inheriting
    // DetailDrawer's generic "Close detail panel".
    expect(target.querySelector('.drawer-close')?.getAttribute('aria-label')).toBe(
      'Close workflow detail',
    );
    expect(target.textContent).toContain('No steps journaled yet for this run.');
  });

  it('claims the dialog stack so window-level shortcuts stand down', () => {
    // The focus trap this drawer never had. dialogStore is what App.svelte's
    // letter shortcuts read before renavigating the page behind a modal.
    const before = dialogStore.openCount;
    openLoaded();

    expect(dialogStore.openCount).toBe(before + 1);

    cleanup?.();
    cleanup = null;
    expect(dialogStore.openCount).toBe(before);
  });

  it('closes on Escape', () => {
    const target = openLoaded();

    target
      .querySelector('[role="dialog"]')!
      .dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }));
    flushSync();

    expect(millsStore.selectedWorkflowID).toBeNull();
    expect(target.querySelector('[role="dialog"]')).toBeNull();
  });

  it('closes on a backdrop click and on ✕', () => {
    let target = openLoaded();
    target.querySelector<HTMLElement>('.drawer-backdrop')?.click();
    flushSync();
    expect(millsStore.selectedWorkflowID).toBeNull();

    cleanup?.();
    cleanup = null;

    target = openLoaded();
    target.querySelector<HTMLButtonElement>('.drawer-close')?.click();
    flushSync();
    expect(millsStore.selectedWorkflowID).toBeNull();
  });
});
