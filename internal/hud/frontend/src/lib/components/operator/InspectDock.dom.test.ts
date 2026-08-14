import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import InspectDock from './InspectDock.svelte';
import { millsStore, type PipelineRunDetail } from '../../stores/mills.svelte.ts';
import type { InflightRow } from '../../utils/operatorHelpers.ts';

const RUN_ID = 'RUN-CORRECTED-1';
const realFetch = globalThis.fetch;
let cleanup: (() => void) | null = null;

afterEach(() => {
  cleanup?.();
  cleanup = null;
  globalThis.fetch = realFetch;
  millsStore.closeRunDetail();
  millsStore.pipelineDetailByRun = {};
  document.body.innerHTML = '';
});

describe('InspectDock — Mills verdict', () => {
  it('renders the corrected verdict chip', () => {
    const detail = {
      run: { ID: RUN_ID, BacklogID: 'bk-1', State: 'escalated', Attempts: 1 },
      stages: [],
      gates: [],
      evidence: {
        verdicts: [],
        provenance: null,
        regression: null,
        verdict: {
          class: 'merged_after_escalation',
          superseded: true,
          prior_class: 'code',
        },
      },
    } as unknown as PipelineRunDetail;
    millsStore.pipelineDetailByRun = {
      [RUN_ID]: { status: 'loaded', detail },
    };
    // Keep the background refresh started by InspectDock pending so the
    // seeded authoritative detail remains visible for this synchronous test.
    globalThis.fetch = vi.fn(() => new Promise<Response>(() => {})) as typeof fetch;

    const selected: InflightRow = {
      kind: 'mills',
      key: `mills:${RUN_ID}`,
      id: RUN_ID,
      title: 'Corrected run',
      subtitle: 'implement',
      state: 'escalated',
      severity: 'ok',
      age: '',
    };
    const target = document.createElement('div');
    document.body.appendChild(target);
    const component = mount(InspectDock, { target, props: { selected, onClose: vi.fn() } });
    cleanup = () => {
      void unmount(component);
      target.remove();
    };
    flushSync();

    const chip = target.querySelector('[data-testid="run-verdict-chip"]');
    expect(chip?.textContent).toContain('merged_after_escalation');
    expect(chip?.classList.contains('corrected')).toBe(true);
  });
});
