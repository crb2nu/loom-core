import { describe, expect, it } from 'vitest';
import { normalizeRequeueResponse } from './mills.svelte.ts';

// Unit coverage for the pure requeue response-shape mapper (plan wave-2 W3).
//
// The operator's start?requeue=1 endpoint returns pipelineStartResponse
// ({run_id?, backlog_id, decision, state?, reason?, blockers?}) and encodes the
// decision on the HTTP status: 201 started, 409 conflict (one-way terminal
// guard), 403 policy disabled / autonomy blocked. normalizeRequeueResponse maps
// status + parsed body onto a RequeueOutcome the drawer renders inline, so it is
// tested here without any DOM.

describe('normalizeRequeueResponse', () => {
  describe('201 started', () => {
    it('reports the new run id', () => {
      const out = normalizeRequeueResponse(201, {
        run_id: 'RUN-abc123',
        backlog_id: 'MILLS-1',
        decision: 'started',
        state: 'planning',
      });
      expect(out.kind).toBe('started');
      expect(out.runId).toBe('RUN-abc123');
      expect(out.message).toBe('Requeued as RUN-abc123');
    });

    it('still reports started when the run id is absent', () => {
      const out = normalizeRequeueResponse(201, { backlog_id: 'MILLS-1', decision: 'started' });
      expect(out.kind).toBe('started');
      expect(out.runId).toBeUndefined();
      expect(out.message).toBe('Requeued');
    });
  });

  describe('409 conflict', () => {
    it('reads a merged-state conflict as already-completed (the ghost spark)', () => {
      const out = normalizeRequeueResponse(409, {
        backlog_id: 'MILLS-1',
        decision: 'skipped',
        reason: 'state is merged',
      });
      expect(out.kind).toBe('conflict');
      expect(out.alreadyCompleted).toBe(true);
      expect(out.reason).toBe('state is merged');
      expect(out.message).toBe('Already completed (state is merged) — nothing to requeue');
    });

    it('treats a done-state conflict as already-completed too', () => {
      const out = normalizeRequeueResponse(409, {
        decision: 'skipped',
        reason: 'state is done',
      });
      expect(out.kind).toBe('conflict');
      expect(out.alreadyCompleted).toBe(true);
    });

    it('surfaces a non-terminal conflict reason without the already-done phrasing', () => {
      const out = normalizeRequeueResponse(409, {
        decision: 'skipped',
        reason: 'state is running',
      });
      expect(out.kind).toBe('conflict');
      expect(out.alreadyCompleted).toBe(false);
      expect(out.message).toBe("Can't requeue: state is running");
    });

    it('falls back to a generic conflict message when no reason is given', () => {
      const out = normalizeRequeueResponse(409, { decision: 'skipped' });
      expect(out.kind).toBe('conflict');
      expect(out.alreadyCompleted).toBe(false);
      expect(out.message).toBe("Can't requeue — item is no longer requeueable");
    });
  });

  describe('403 forbidden', () => {
    it('carries the admin-token hint for a policy-disabled response', () => {
      const out = normalizeRequeueResponse(403, {
        decision: 'skipped',
        reason: 'policy disabled',
      });
      expect(out.kind).toBe('forbidden');
      expect(out.reason).toBe('policy disabled');
      expect(out.message).toBe(
        'Requeue forbidden: policy disabled — set the admin token in the Labs access bar',
      );
    });

    it('appends autonomy blockers when the operator returns them', () => {
      const out = normalizeRequeueResponse(403, {
        decision: 'skipped',
        reason: 'autonomy blocked',
        blockers: ['policy.enabled=false', 'kpi policy_enabled=false'],
      });
      expect(out.kind).toBe('forbidden');
      expect(out.blockers).toEqual(['policy.enabled=false', 'kpi policy_enabled=false']);
      expect(out.message).toBe(
        'Requeue forbidden: autonomy blocked (policy.enabled=false, kpi policy_enabled=false) — set the admin token in the Labs access bar',
      );
    });

    it('shows the bare admin-token hint when there is no reason', () => {
      const out = normalizeRequeueResponse(403, {});
      expect(out.kind).toBe('forbidden');
      expect(out.message).toBe('Requeue forbidden — set the admin token in the Labs access bar');
    });
  });

  describe('other statuses (plain-text http.Error bodies)', () => {
    it('maps a 404 plain-text body to an error with the status', () => {
      const out = normalizeRequeueResponse(404, 'backlog item not found');
      expect(out.kind).toBe('error');
      expect(out.reason).toBe('backlog item not found');
      expect(out.message).toBe('Requeue failed (404): backlog item not found');
    });

    it('maps a 500 with no body to a bare status error', () => {
      const out = normalizeRequeueResponse(500, '');
      expect(out.kind).toBe('error');
      expect(out.reason).toBeUndefined();
      expect(out.message).toBe('Requeue failed (500)');
    });

    it('tolerates a null body', () => {
      const out = normalizeRequeueResponse(503, null);
      expect(out.kind).toBe('error');
      expect(out.message).toBe('Requeue failed (503)');
    });
  });
});

describe('normalizeRequeueResponse 401 gate rejection', () => {
  it('maps the HUD admin-gate 401 {"error": ...} to forbidden with the token hint', () => {
    const out = normalizeRequeueResponse(401, { error: 'invalid admin token' });
    expect(out.kind).toBe('forbidden');
    expect(out.message).toContain('invalid admin token');
    expect(out.message).toContain('admin token in the Labs access bar');
  });

  it('maps a bare 401 to forbidden with the token hint', () => {
    const out = normalizeRequeueResponse(401, '');
    expect(out.kind).toBe('forbidden');
    expect(out.message).toContain('admin token in the Labs access bar');
  });
});
