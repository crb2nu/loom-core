// ConfirmDialog's Escape contract, mounted client-side.
//
// This has to be a DOM test rather than a svelte/server render: the behavior
// under test is entirely in the keydown listener, and SSR emits the markup
// without ever attaching one. The regression it pins is a double-dismiss —
// ConfirmDialog sits inside surfaces that close on their own Escape (a parent
// DetailDrawer, and App.svelte's window-level handler), so one Escape used to
// cancel the dialog AND tear down the surface behind it.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { mount, unmount } from 'svelte';
import ConfirmDialog from './ConfirmDialog.svelte';

let cleanup: (() => void) | null = null;

afterEach(() => {
  cleanup?.();
  cleanup = null;
  document.body.innerHTML = '';
});

interface Handlers {
  onConfirm?: () => void;
  onCancel?: () => void;
}

/** Mount the dialog open and return its backdrop — the keydown host. */
function open(handlers: Handlers = {}): HTMLElement {
  const target = document.createElement('div');
  document.body.appendChild(target);
  const component = mount(ConfirmDialog, {
    target,
    props: {
      open: true,
      title: 'Pause the mill',
      message: 'Running shuttles finish; queued ones hold.',
      onConfirm: handlers.onConfirm ?? (() => {}),
      onCancel: handlers.onCancel ?? (() => {}),
    },
  });
  cleanup = () => { void unmount(component); target.remove(); };

  const backdrop = target.querySelector<HTMLElement>('.confirm-backdrop');
  if (!backdrop) throw new Error('dialog did not render a backdrop');
  return backdrop;
}

/** Dispatch a bubbling, cancelable keydown the way a real key press arrives. */
function press(el: HTMLElement, key: string): KeyboardEvent {
  const event = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true });
  el.dispatchEvent(event);
  return event;
}

describe('ConfirmDialog — Escape', () => {
  it('calls onCancel', () => {
    const onCancel = vi.fn();
    const backdrop = open({ onCancel });

    press(backdrop, 'Escape');

    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it('does not also confirm', () => {
    const onConfirm = vi.fn();
    const backdrop = open({ onConfirm, onCancel: () => {} });

    press(backdrop, 'Escape');

    expect(onConfirm).not.toHaveBeenCalled();
  });

  it('stops propagation so App-level shortcuts never see the key', () => {
    // The real containment check: a listener bound above the dialog stands in
    // for App.svelte's <svelte:window onkeydown> and the parent DetailDrawer.
    // Without stopPropagation both fired on the same Escape.
    const ancestor = vi.fn();
    document.addEventListener('keydown', ancestor);
    try {
      const backdrop = open({ onCancel: () => {} });

      press(backdrop, 'Escape');

      expect(ancestor).not.toHaveBeenCalled();
    } finally {
      document.removeEventListener('keydown', ancestor);
    }
  });

  it('lets every other key through untouched', () => {
    // Escape is the only key the dialog claims — swallowing more would break
    // Tab (the focus trap's job) and typing inside any future dialog body.
    const onCancel = vi.fn();
    const ancestor = vi.fn();
    document.addEventListener('keydown', ancestor);
    try {
      const backdrop = open({ onCancel });

      press(backdrop, 'Enter');
      press(backdrop, 'j');
      press(backdrop, 'Tab');

      expect(onCancel).not.toHaveBeenCalled();
      expect(ancestor).toHaveBeenCalledTimes(3);
    } finally {
      document.removeEventListener('keydown', ancestor);
    }
  });
});

describe('ConfirmDialog — closed', () => {
  it('renders nothing, so Escape cannot reach a cancel handler', () => {
    const onCancel = vi.fn();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const component = mount(ConfirmDialog, {
      target,
      props: {
        open: false,
        title: 'Pause the mill',
        message: 'Running shuttles finish; queued ones hold.',
        onConfirm: () => {},
        onCancel,
      },
    });
    cleanup = () => { void unmount(component); target.remove(); };

    expect(target.querySelector('.confirm-backdrop')).toBeNull();

    press(document.body, 'Escape');
    expect(onCancel).not.toHaveBeenCalled();
  });
});
