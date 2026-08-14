// First render coverage for PipelineRunDetail, mounted client-side.
//
// The drawer had zero component-level tests, which is how the gates:null wedge
// reached production: `[...detail.gates]` threw inside a $derived, Svelte tore
// down the effect tree, and the drawer froze at "Loading run detail…" with a
// dead close button. The store now normalises null → [] (see
// stores/mills.detailNormalize.test.ts), and the first case below closes the
// loop by driving the real fetch path with a null-gates payload and asserting
// the drawer actually renders and still closes.
//
// The rest pins the chrome contract after the collapse onto shared/DetailDrawer:
// Escape, backdrop click and ✕ all funnel through the same close(), and the
// Escalate control stays on the header row. A `.dom.test.ts` on purpose —
// svelte/server emits the markup but never attaches a listener, so the node
// project would pass the keyboard cases vacuously.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import PipelineRunDetail from './PipelineRunDetail.svelte';
import {
  millsStore,
  type PipelineRunDetail as Detail,
  type RunEvidence,
  type RunVerdictEvidence,
} from '../../stores/mills.svelte.ts';
import { fmtRunTime } from './shared/format.ts';

const RUN_ID = 'RUN-DOM-1';

let cleanup: (() => void) | null = null;
const realFetch = globalThis.fetch;

afterEach(() => {
  cleanup?.();
  cleanup = null;
  globalThis.fetch = realFetch;
  millsStore.closeRunDetail();
  millsStore.pipelineDetailByRun = {};
  millsStore.backlog = [];
  document.body.innerHTML = '';
});

function detailFor(state = 'implementing', evidence?: RunEvidence): Detail {
  return {
    run: {
      ID: RUN_ID,
      BacklogID: 'bk-1',
      State: state,
      Attempts: 1,
    },
    // Omit the key entirely when unset — that is how a pre-evidence operator
    // answers, and the drawer has to render against it.
    ...(evidence ? { evidence } : {}),
    stages: [],
    gates: [],
  } as unknown as Detail;
}

describe('PipelineRunDetail — merge request link', () => {
  it('routes through the backlog target project with external-link isolation', () => {
    millsStore.backlog = [{ ID: 'bk-1', Title: '', State: '', Priority: '', TargetProject: 'platform/gitops' }];
    const detail = detailFor();
    detail.run.MRIID = 42;
    millsStore.selectedRunID = RUN_ID;
    millsStore.pipelineDetailByRun = { [RUN_ID]: { status: 'loaded', detail } };
    const open = vi.spyOn(millsStore, 'openRunDetail');

    const target = mountDrawer();
    const link = target.querySelector<HTMLAnchorElement>('.mr-link')!;
    expect(link.href).toBe('https://gitlab.flexinfer.ai/platform/gitops/-/merge_requests/42');
    expect(link.target).toBe('_blank');
    expect(link.rel).toContain('noopener');
    link.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
    expect(open).not.toHaveBeenCalled();
  });
});

/** The evidence block as the operator ships it, with only the verdict varied. */
function evidenceWith(verdict: RunVerdictEvidence | null): RunEvidence {
  return { verdicts: [], provenance: null, regression: null, verdict };
}

/** Mount the drawer against whatever the store currently holds. */
function mountDrawer(): HTMLElement {
  const target = document.createElement('div');
  document.body.appendChild(target);
  const component = mount(PipelineRunDetail, { target });
  cleanup = () => {
    void unmount(component);
    target.remove();
  };
  flushSync();
  return target;
}

/** Seed a loaded run and mount. */
function openLoaded(state = 'implementing', evidence?: RunEvidence): HTMLElement {
  millsStore.selectedRunID = RUN_ID;
  millsStore.pipelineDetailByRun = {
    [RUN_ID]: { status: 'loaded', detail: detailFor(state, evidence) },
  };
  return mountDrawer();
}

function press(el: Element, key: string): void {
  el.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true }));
}

describe('PipelineRunDetail — a live run whose gates arrive as null', () => {
  it('renders the drawer and still closes', async () => {
    // The exact wire shape that wedged the drawer: a Go nil slice for a run
    // that has not evaluated a gate yet. Driven through the real store fetch
    // so the normalisation and the render are proven together.
    globalThis.fetch = vi.fn(() =>
      Promise.resolve(
        new Response(
          JSON.stringify({ run: { ID: RUN_ID, BacklogID: 'bk-1', State: 'implementing', Attempts: 1 }, stages: null, gates: null }),
          { status: 200, headers: { 'Content-Type': 'application/json' } },
        ),
      ),
    ) as unknown as typeof globalThis.fetch;

    millsStore.openRunDetail(RUN_ID);
    await vi.waitFor(() => {
      expect(millsStore.pipelineDetailByRun[RUN_ID]?.status).toBe('loaded');
    });

    const target = mountDrawer();

    // Both count-bearing sections render — under the wedge neither did.
    expect(target.textContent).toContain('No gate evaluations yet.');
    expect(target.textContent).toContain('No stage attempts recorded yet.');

    // And the close button is live rather than stranded behind a dead effect tree.
    target.querySelector<HTMLButtonElement>('.drawer-close')?.click();
    flushSync();
    expect(millsStore.selectedRunID).toBeNull();
  });
});

describe('PipelineRunDetail — drawer chrome', () => {
  it('renders a labelled modal dialog', () => {
    const target = openLoaded();

    const dialog = target.querySelector('[role="dialog"]');
    expect(dialog).not.toBeNull();
    expect(dialog?.getAttribute('aria-modal')).toBe('true');
    // Preserved verbatim from the hand-rolled <aside> this replaced.
    expect(dialog?.getAttribute('aria-label')).toBe('Pipeline run detail');
    // As is the ✕'s own name — DetailDrawer's generic default would have made
    // it "Close detail panel" across every consumer.
    expect(target.querySelector('.drawer-close')?.getAttribute('aria-label')).toBe(
      'Close run detail',
    );
  });

  it('closes on Escape', () => {
    // DetailDrawer scopes the key to the panel, where focusTrap puts focus on
    // mount. The orphaned-focus case below covers when that stops holding.
    const target = openLoaded();

    press(target.querySelector('[role="dialog"]')!, 'Escape');
    flushSync();

    expect(millsStore.selectedRunID).toBeNull();
    expect(target.querySelector('[role="dialog"]')).toBeNull();
  });

  it('closes on Escape after the focused control unmounts under a live run', () => {
    // Regression: watching an in-flight run reach a terminal state is the
    // drawer's NORMAL use, and it used to strand Escape.
    //
    // focusTrap lands on `.run-escalate` — the first focusable in DOM order,
    // ahead of `.drawer-close`. A background poll flips the run to `merged`,
    // canEscalate goes false, and that button unmounts with focus on it.
    // document.activeElement falls back to <body>, so the <aside>'s scoped
    // keydown handler is no longer on the event path. Before the window-level
    // fallback, Escape here did nothing and selectedRunID stayed set — a
    // regression against the <svelte:window onkeydown> both drawers had
    // before the collapse onto DetailDrawer.
    const target = openLoaded('implementing');

    const escalate = target.querySelector('.run-escalate');
    expect(document.activeElement).toBe(escalate);

    // The background refresh: same run, now terminal.
    millsStore.pipelineDetailByRun = {
      [RUN_ID]: { status: 'loaded', detail: detailFor('merged') },
    };
    flushSync();

    // Premise of the bug — the focused node is gone and focus fell to <body>,
    // outside the drawer.
    expect(target.querySelector('.run-escalate')).toBeNull();
    expect(document.activeElement).toBe(document.body);
    expect(target.querySelector('[role="dialog"]')!.contains(document.activeElement)).toBe(false);

    press(document.activeElement!, 'Escape');
    flushSync();

    expect(millsStore.selectedRunID).toBeNull();
    expect(target.querySelector('[role="dialog"]')).toBeNull();
  });

  it('leaves Escape to whichever surface holds focus over the drawer', () => {
    // The fallback must not fire merely because focus sits outside the panel:
    // a ConfirmDialog (or the audit drawer) opened over this one renders in a
    // sibling subtree and legitimately owns the keyboard. Only ORPHANED focus
    // — body / detached — re-arms the window path.
    const target = openLoaded();

    const overlay = document.createElement('button');
    document.body.appendChild(overlay);
    overlay.focus();
    expect(document.activeElement).toBe(overlay);

    press(overlay, 'Escape');
    flushSync();

    expect(millsStore.selectedRunID).toBe(RUN_ID);
    expect(target.querySelector('[role="dialog"]')).not.toBeNull();
    overlay.remove();
  });

  it('closes on a backdrop click', () => {
    const target = openLoaded();

    target.querySelector<HTMLElement>('.drawer-backdrop')?.click();
    flushSync();

    expect(millsStore.selectedRunID).toBeNull();
  });

  it('renders nothing when no run is selected', () => {
    millsStore.selectedRunID = null;
    const target = mountDrawer();

    expect(target.querySelector('[role="dialog"]')).toBeNull();
  });
});

describe('PipelineRunDetail — Escalate control', () => {
  it('sits on the header row for an in-flight run', () => {
    const target = openLoaded('implementing');

    const escalate = target.querySelector('.run-escalate');
    expect(escalate).not.toBeNull();
    // Header row, not the body — it moved into DetailDrawer's headerActions slot.
    expect(escalate?.closest('.drawer-header')).not.toBeNull();
  });

  it('is absent for a terminal run', () => {
    const target = openLoaded('merged');

    expect(target.querySelector('.run-escalate')).toBeNull();
  });
});

// The run's current-belief verdict (Trustworthy Verdicts S4). Two things are
// being defended here. First, the correction must stay VISIBLE: a rescued
// escalation reads merged_after_escalation, never a plain "merged" that would
// make it indistinguishable from a clean run. Second, the zero-time trap: the
// operator sends a verdict for live runs too (Go's OccurredAt has no
// omitempty), so a naive "verdict != null ⇒ render" would paint a duplicate
// state chip dated year 1 on every in-flight run on the floor.
describe('PipelineRunDetail — run verdict chip', () => {
  const CORRECTED: RunVerdictEvidence = {
    class: 'merged_after_escalation',
    superseded: true,
    source: 'ghost_spark_closed',
    prior_class: 'code',
    outcome: 'mr_merged',
    occurred_at: '2026-08-07T12:34:56Z',
  };

  function chip(target: HTMLElement): HTMLElement | null {
    return target.querySelector<HTMLElement>('.run-verdict-chip');
  }

  it('renders the corrected class verbatim, never as plain "merged"', () => {
    const target = openLoaded('escalated', evidenceWith(CORRECTED));

    const value = chip(target)?.querySelector('.esc-chip-value');
    expect(value?.textContent).toBe('merged_after_escalation');
    // The whole point of the class: folding it to "merged" would erase the
    // fact that this run escalated first.
    expect(value?.textContent).not.toBe('merged');
    // And the immutable row state still shows in the header beside it.
    expect(target.querySelector('.run-state')?.textContent).toBe('escalated');
  });

  it('gives a corrected verdict the accent treatment and a sourced tooltip', () => {
    const target = openLoaded('escalated', evidenceWith(CORRECTED));

    const el = chip(target);
    expect(el?.classList.contains('verdict-corrected')).toBe(true);
    expect(el?.classList.contains('esc-accent')).toBe(true);

    const title = el?.getAttribute('title') ?? '';
    expect(title).toContain('ghost_spark_closed');
    expect(title).toContain('code');
    expect(title).toContain('merged_after_escalation');
    // Cites when the correction happened, through the shared formatter.
    expect(title).toContain(fmtRunTime(CORRECTED.occurred_at));
  });

  it('renders the supersede history line', () => {
    const target = openLoaded('escalated', evidenceWith(CORRECTED));

    const history = target.querySelector('.verdict-history');
    expect(history).not.toBeNull();
    expect(history?.textContent?.replace(/\s+/g, ' ').trim()).toBe(
      'was code → merged_after_escalation via ghost_spark_closed',
    );
  });

  it('shows an uncorrected verdict without accent or history', () => {
    // An escalated run nobody rescued: the verdict resolves to the escalation
    // class, which still differs from the row state and is worth surfacing.
    const target = openLoaded(
      'escalated',
      evidenceWith({ class: 'code', superseded: false, occurred_at: '2026-08-07T09:00:00Z' }),
    );

    const el = chip(target);
    expect(el?.querySelector('.esc-chip-value')?.textContent).toBe('code');
    expect(el?.classList.contains('verdict-corrected')).toBe(false);
    expect(target.querySelector('.verdict-history')).toBeNull();
  });

  it('renders no chip for a live run carrying the zero-time verdict', () => {
    // Exactly what an in-flight run looks like on the wire — NOT verdict:null.
    const target = openLoaded(
      'implementing',
      evidenceWith({
        class: 'implementing',
        superseded: false,
        occurred_at: '0001-01-01T00:00:00Z',
      }),
    );

    expect(chip(target)).toBeNull();
    // No year-1 timestamp leaked anywhere in the drawer either.
    expect(target.textContent).not.toContain('0001');
  });

  it('renders no chip when the verdict only echoes a terminal state', () => {
    const target = openLoaded(
      'merged',
      evidenceWith({ class: 'merged', superseded: false, occurred_at: '2026-08-07T10:00:00Z' }),
    );

    expect(chip(target)).toBeNull();
  });

  it('suppresses a zero occurred_at in the tooltip when the chip does render', () => {
    const target = openLoaded(
      'escalated',
      evidenceWith({ class: 'code', occurred_at: '0001-01-01T00:00:00Z' }),
    );

    const title = chip(target)?.getAttribute('title') ?? '';
    expect(title).not.toContain('0001');
    expect(title).not.toContain(fmtRunTime('0001-01-01T00:00:00Z'));
  });

  it('renders nothing for verdict:null', () => {
    const target = openLoaded('implementing', evidenceWith(null));

    expect(chip(target)).toBeNull();
    // And the rest of the drawer is unaffected.
    expect(target.querySelector('[role="dialog"]')).not.toBeNull();
  });

  it('renders nothing when the operator omits the evidence block', () => {
    // A mixed-version operator predating the field entirely.
    const target = openLoaded('escalated');

    expect(chip(target)).toBeNull();
    expect(target.querySelector('[role="dialog"]')).not.toBeNull();
  });

  it('renders nothing when the evidence block omits only the verdict key', () => {
    const target = openLoaded('escalated', {
      verdicts: [],
      provenance: null,
      regression: null,
    });

    expect(chip(target)).toBeNull();
  });
});
