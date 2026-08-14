// Render coverage for LineageRibbon's strand mode — the per-run thread the
// PipelineRunDetail drawer draws. Strand shipped fully built but callerless,
// so nothing had ever exercised its template branches; these render the real
// component (server-rendered, no DOM needed) against segments from the same
// pure builder the store delegates to.

import { describe, expect, it } from 'vitest';
import { render } from 'svelte/server';
import LineageRibbon from './LineageRibbon.svelte';
import { lineageFor, spineSegments, type LineageSegment } from './lineage.ts';
import type { BacklogItem, PipelineRun } from '../../../stores/mills.svelte.ts';

function run(over: Partial<PipelineRun> = {}): PipelineRun {
  return {
    ID: 'run-1',
    BacklogID: 'bk-1',
    State: 'running',
    CurrentStage: 'implement',
    Attempts: 1,
    ...over,
  } as PipelineRun;
}

const backlog = [
  { ID: 'bk-1', Priority: 'P1', PlanID: 'plan-42', Title: 'weave the thing' } as BacklogItem,
];

function html(segments: LineageSegment[], mode: 'spine' | 'strand' = 'strand'): string {
  return render(LineageRibbon, { props: { mode, segments } }).body;
}

describe('LineageRibbon — strand mode', () => {
  it('draws a warp, every stage pick, and the threads between them', () => {
    const out = html(lineageFor(run(), backlog));
    // Warp node carries the priority + plan join.
    expect(out).toContain('P1 · plan-42');
    // Strand mode labels each stage node in the loom vocabulary (spine mode
    // shows only the dot).
    expect(out).toContain('threading the warp');
    expect(out).toContain('laying weft');
    expect(out).toContain('sweeping the floor');
    // Per-pick state drives the node color: picks before the current stage
    // read done, the current one active, the rest pending.
    expect(out).toContain('laying weft — active');
    expect(out).toContain('threading the warp — done');
    expect(out).toContain('counting picks — pending');
    expect(out).toContain('class="thread');
    expect(out).toContain('run lineage');
  });

  it('terminates an escalated run in a spark carrying its gate reasons', () => {
    const out = html(
      lineageFor(
        run({ State: 'escalated', CurrentStage: 'ci_watch', EscalationClass: 'ci_red' }),
        backlog,
        ['gate tests: 2 failing', 'gate lint: unformatted'],
      ),
    );
    expect(out).toContain('kind-spark');
    expect(out).toContain('ci_red');
    // Reasons land on the node's hover text — without it the enrichment the
    // drawer passes would render nowhere.
    expect(out).toContain('gate tests: 2 failing');
  });

  it('terminates a merged run in a bolt linked to its MR', () => {
    const out = html(lineageFor(run({ State: 'merged', MRIID: 1207 }), backlog));
    expect(out).toContain('kind-bolt');
    expect(out).toContain('!1207');
    expect(out).toContain('#mills/bolts/run-1');
  });

  it('leaves an in-flight run with no terminal node', () => {
    const out = html(lineageFor(run(), backlog));
    expect(out).not.toContain('kind-bolt');
    expect(out).not.toContain('kind-spark');
  });

  it('renders nothing but the container for empty segments', () => {
    const out = html([]);
    expect(out).toContain('run lineage');
    expect(out).not.toContain('class="node');
  });
});

describe('LineageRibbon — spine mode is unchanged', () => {
  it('renders navigable warp/shuttle/bolt/spark counts', () => {
    const out = html(
      spineSegments({
        backlogByPriority: { P0: [backlog[0]] },
        activeShuttles: 2,
        bolts: 12,
        sparks: 1,
      }),
      'spine',
    );
    expect(out).toContain('mill floor');
    expect(out).toContain('#mills/shuttles');
    expect(out).toContain('bolts');
    expect(out).toContain('sparks');
  });
});
