import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import EngramTree from './EngramTree.svelte';
import type { EngramGraph, EngramInfo } from '../../stores/engrams.svelte.ts';
import type { PatternInfo } from '../../stores/patterns.svelte.ts';

let component: Record<string, unknown> | null = null;
let target: HTMLElement;

function node(id: string, tier: number, prerequisites: string[] = []): EngramInfo {
  return { id, name: `Engram ${id}`, tier, proof_status: tier === 1 ? 'verified' : tier === 2 ? 'stale' : 'unverified', description: `Description ${id}`, prerequisites, last_verified_at: '2026-08-08T12:00:00Z', proof: { kind: 'contract', refs: [`${id}.md`] } };
}

function open(
  graph: EngramGraph | null,
  unavailable = false,
  error: string | null = null,
  extra: Record<string, unknown> = {}
): void {
  target = document.createElement('div');
  document.body.appendChild(target);
  component = mount(EngramTree, { target, props: { graph, unavailable, error, ...extra } }) as Record<string, unknown>;
  flushSync();
}

afterEach(() => {
  if (component) void unmount(component);
  component = null;
  document.body.innerHTML = '';
});

describe('EngramTree', () => {
  it('renders bridge unavailable instead of a fake empty tree', () => {
    open({ nodes: [], edges: [], degraded: true });
    expect(target.textContent).toContain('bridge unavailable');
    expect(target.textContent).not.toContain('no engrams yet');
  });

  it('renders a catalog fetch failure as unavailable, never eternal loading', () => {
    // The production regression: /api/engrams/graph 502s, graph stays null —
    // with an error present the tree must say so instead of spinning forever.
    open(null, false, '/api/engrams/graph: 502');
    expect(target.textContent).toContain('engram graph unavailable');
    expect(target.textContent).toContain('502');
    expect(target.textContent).not.toContain('loading engram graph');
  });

  it('renders the endpoint-absent state via the unavailable prop', () => {
    open(null, true);
    expect(target.textContent).toContain('bridge unavailable');
    expect(target.textContent).not.toContain('loading engram graph');
  });

  it('shows loading only while genuinely loading (no data, no error)', () => {
    open(null);
    expect(target.textContent).toContain('loading engram graph');
  });

  it('renders twelve nodes in three tiers, edges, and drawer navigation', () => {
    const nodes = [
      node('a1', 1), node('a2', 1), node('a3', 1), node('a4', 1),
      node('b1', 2, ['a1']), node('b2', 2, ['a2']), node('b3', 2, ['a3']), node('b4', 2, ['a4']),
      node('c1', 3, ['b1']), node('c2', 3, ['b2']), node('c3', 3, ['b3']), node('c4', 3, ['b4']),
    ];
    const edges = nodes.flatMap((n) => n.prerequisites.map((to) => ({ from: n.id, to })));
    open({ nodes, edges, degraded: false });
    expect(target.querySelectorAll('.tier')).toHaveLength(3);
    expect(target.querySelectorAll('.node')).toHaveLength(12);
    expect(target.querySelectorAll('.edge')).toHaveLength(8);

    target.querySelector<HTMLButtonElement>('[data-engram-id="b1"]')?.click();
    flushSync();
    expect(document.querySelector('[role="dialog"]')?.textContent).toContain('Description b1');
    const prerequisite = Array.from(document.querySelectorAll<HTMLButtonElement>('.links button')).find((button) => button.textContent?.includes('Engram a1'));
    prerequisite?.click();
    flushSync();
    expect(document.querySelector('[role="dialog"]')?.textContent).toContain('Description a1');
    document.querySelector<HTMLButtonElement>('[aria-label="Close engram detail"]')?.click();
    flushSync();
    expect(document.querySelector('[role="dialog"]')).toBeNull();
  });

  // The canvas used to be a frozen 900x420 box with the row pitch hardcoded
  // into the path math, so edges drifted off the nodes once a tier grew past a
  // handful. The viewBox must now follow the tallest tier.
  it('sizes the canvas from the tallest tier instead of a fixed height', () => {
    const heightOf = (vb: string | null | undefined) => Number((vb ?? '0 0 0 0').split(' ')[3]);

    open({ nodes: [node('a1', 1), node('a2', 1)], edges: [], degraded: false });
    const small = heightOf(target.querySelector('svg.edges')?.getAttribute('viewBox'));

    void unmount(component!);
    component = null;
    document.body.innerHTML = '';

    const many = Array.from({ length: 14 }, (_, i) => node(`n${i}`, 1));
    open({ nodes: many, edges: [], degraded: false });
    const large = heightOf(target.querySelector('svg.edges')?.getAttribute('viewBox'));

    expect(large).toBeGreaterThan(small);
  });

  it('lists the patterns that compose an engram and navigates to one', () => {
    // The reverse edge the API cannot serve, derived by inverting the pattern
    // list. The pattern references the engram by URI while the node is keyed
    // by memory-item id — the join has to tolerate that.
    const patterns: PatternInfo[] = [
      { id: 'pattern-go-cli', slug: 'go-cli', name: 'Go CLI tool', makes: 'a CLI', version: '0.1', status: 'approved', engrams: ['engram://go/cli'] },
    ];
    const onOpenPattern = vi.fn();
    open({ nodes: [node('go/cli', 1)], edges: [], degraded: false }, false, null, { patterns, onOpenPattern });

    target.querySelector<HTMLButtonElement>('[data-engram-id="go/cli"]')?.click();
    flushSync();

    expect(document.querySelector('[role="dialog"]')?.textContent).toContain('composed into');

    const link = Array.from(document.querySelectorAll<HTMLButtonElement>('.links button')).find((b) => b.textContent?.includes('Go CLI tool'));
    expect(link).toBeTruthy();
    link?.click();
    flushSync();
    expect(onOpenPattern).toHaveBeenCalledWith(patterns[0]);
  });

  it('says so plainly when no pattern composes the engram', () => {
    open({ nodes: [node('orphan', 1)], edges: [], degraded: false }, false, null, { patterns: [] });
    target.querySelector<HTMLButtonElement>('[data-engram-id="orphan"]')?.click();
    flushSync();
    expect(document.querySelector('[role="dialog"]')?.textContent).toContain('no pattern in the catalog composes this engram');
  });
});
