// Contract coverage for the DetailDrawer props the Mills run drawers lean on.
//
// PipelineRunDetail / WorkflowRunDetail retired their hand-rolled drawer chrome
// in favour of this component, which meant teaching it three things they used
// to do themselves: a rich (non-string) title, controls beside the ✕, and a
// body whose sections pad themselves full-bleed. All three are additive and
// defaulted — the existing string-title consumers (TaskDetail, BacklogDetail,
// SessionDetail, ServerDetail, …) must keep rendering exactly as before, which
// is what the "backward compatible" cases below pin.
//
// Server-rendered (node project): this is markup, not event wiring — the
// keyboard/focus half of the drawer contract is covered by the mounted
// PipelineRunDetail.dom.test.ts.

import { describe, expect, it } from 'vitest';
import { createRawSnippet } from 'svelte';
import { render } from 'svelte/server';
import DetailDrawer from './DetailDrawer.svelte';

const body = createRawSnippet(() => ({
  render: () => '<p class="body-marker">body</p>',
}));

function html(props: Record<string, unknown> = {}): string {
  return render(DetailDrawer, {
    props: { open: true, children: body, ...props },
  }).body;
}

describe('DetailDrawer — titleContent', () => {
  it('renders the snippet instead of the plain title heading', () => {
    const out = html({
      title: 'Pipeline run detail',
      titleContent: createRawSnippet(() => ({
        render: () => '<div class="run-title">Pipeline Run</div>',
      })),
    });

    expect(out).toContain('class="run-title"');
    expect(out).not.toContain('drawer-title');
  });

  it('still reads the aria-label off `title` so the dialog stays named', () => {
    // The rich title is markup, not an accessible name — dropping `title`
    // would leave screen readers with the generic "Detail panel" fallback.
    const out = html({
      title: 'Pipeline run detail',
      titleContent: createRawSnippet(() => ({ render: () => '<div>Pipeline Run</div>' })),
    });

    expect(out).toContain('aria-label="Pipeline run detail"');
  });

  it('falls back to the <h3> heading when omitted (backward compatible)', () => {
    const out = html({ title: 'Backlog item' });

    expect(out).toContain('drawer-title');
    expect(out).toContain('Backlog item');
  });
});

describe('DetailDrawer — headerActions', () => {
  it('renders consumer controls on the title row, before the close button', () => {
    const out = html({
      title: 'Pipeline run detail',
      headerActions: createRawSnippet(() => ({
        render: () => '<button class="run-escalate">Escalate</button>',
      })),
    });

    const action = out.indexOf('run-escalate');
    const close = out.indexOf('drawer-close');
    expect(action).toBeGreaterThan(-1);
    expect(close).toBeGreaterThan(-1);
    // Escalate must sit LEFT of the ✕ — it moved out of a hand-rolled header
    // that ordered it exactly this way.
    expect(action).toBeLessThan(close);
  });

  it('keeps the close button when omitted (backward compatible)', () => {
    const out = html({ title: 'Backlog item' });

    expect(out).toContain('drawer-close');
    expect(out).toContain('aria-label="Close detail panel"');
  });
});

describe('DetailDrawer — closeLabel', () => {
  it('names the ✕ after the surface it dismisses when given one', () => {
    // The hand-rolled Mills headers labelled their own ✕ ("Close run detail" /
    // "Close workflow detail"); harmonising every consumer onto the generic
    // default would have been the one a11y string the collapse dropped.
    const out = html({ title: 'Pipeline run detail', closeLabel: 'Close run detail' });

    expect(out).toContain('aria-label="Close run detail"');
    expect(out).not.toContain('aria-label="Close detail panel"');
  });
});

describe('DetailDrawer — contentPadding', () => {
  it('defaults to the standard drawer gutter', () => {
    expect(html({ title: 'Backlog item' })).toContain('padding: var(--space-3) var(--space-4)');
  });

  it('honours "0" so a full-bleed body pads its own sections', () => {
    // The Mills bodies are stacks of self-padded, edge-to-edge <section>s;
    // the drawer's own gutter would double them up and break the rules.
    const out = html({ title: 'Pipeline run detail', contentPadding: '0' });

    expect(out).toContain('padding: 0');
    expect(out).not.toContain('padding: var(--space-3) var(--space-4)');
  });
});

describe('DetailDrawer — width', () => {
  it('honours a caller-supplied width', () => {
    // The Mills drawers are ~640/680px, not the 420px default; passing the
    // width through is what keeps them from shrinking by a third.
    expect(html({ width: 'min(640px, 96vw)' })).toContain('width: min(640px, 96vw)');
  });
});
