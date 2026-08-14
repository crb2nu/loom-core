import { afterEach, describe, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import EngramTree from './EngramTree.svelte';
import type { EngramGraph, EngramInfo } from '../../stores/engrams.svelte.ts';

let component: Record<string, unknown> | null = null;
let target: HTMLElement;

function node(id: string, tier: number, prerequisites: string[] = []): EngramInfo {
  return { id, name: `Engram ${id}`, tier, proof_status: tier === 1 ? 'verified' : tier === 2 ? 'stale' : 'unverified', description: `Description ${id}`, prerequisites, last_verified_at: '2026-08-08T12:00:00Z', proof: { kind: 'contract', refs: [`${id}.md`] } };
}

function open(graph: EngramGraph | null, unavailable = false): void {
  target = document.createElement('div');
  document.body.appendChild(target);
  component = mount(EngramTree, { target, props: { graph, unavailable } }) as Record<string, unknown>;
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
});
