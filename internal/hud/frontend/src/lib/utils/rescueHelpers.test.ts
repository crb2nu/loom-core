import { describe, expect, it } from 'vitest';
import { latestTerminalRun, rescueRows, type RescueEvidence } from './rescueHelpers.ts';
import type { BacklogItem, PipelineRunDetail, StageResult } from '../stores/mills.svelte.ts';

const item: BacklogItem = { ID: 'drain', Title: 'Drain inflight', State: 'escalated', Priority: 'P1', CreatedAt: '2026-09-13T00:00:00Z' };
function stage(Stage: string, Artifacts: Record<string, unknown>): StageResult {
  return { ID: 1, PipelineRunID: 'r', Stage, Attempt: 1, StartedAt: '', CostUSD: 0, Artifacts };
}
function detail(files = ['cmd/devbox/main.go']): PipelineRunDetail {
  return { run: { ID: 'r', BacklogID: item.ID, Template: 'default', State: 'escalated', Attempts: 2, MRIID: 1948, EscalationClass: 'config' }, gates: [], stages: [
    stage('plan_slice', { slices: [{ files }] }),
    stage('post_implement_gate', { scope_violations: { declared_dirs: ['pkg/devbox'], max_files: 8, ancestor_depth: 2, verdicts: files.map(file => ({ file, rule: 'sensitive-path', admitted: false, slice_index: -1 })) } }),
  ] };
}
const row = (e: RescueEvidence) => rescueRows([item], { drain: e }, Date.parse('2026-09-14T00:00:00Z'))[0];
describe('rescue evidence', () => {
  it.each([1948, 1951])('offers exact named files for rescue !%s', iid => {
    const d = detail(); d.run.MRIID = iid;
    const r = row({ detail: d, mrTitle: 'Draft: [scope-escalated] Drain' });
    expect(r.verdict).toBe('widen'); expect(r.mrIID).toBe(iid); expect(r.rescueDraft).toBe(true);
    expect(r.amendCommand).toBe('mills_backlog_amend_scope({"id":"drain","add_files":["cmd/devbox/main.go"]})');
    expect(r.age).toBe(86400000);
  });
  it('finishes a verified branch and preserves diagnosis wording', () => {
    expect(row({ detail: detail([]), branchExists: true }).verdictText).toMatch(/^RESCUE: an implement branch exists/);
  });
  it('closes only with explicit absence and non-retryability', () => {
    const d = detail([]); d.run.MRIID = null; d.run.EscalationRetryable = false;
    expect(row({ detail: d, branchExists: false }).verdict).toBe('close');
    expect(row({ detail: d }).verdict).toBe('unknown');
  });
  it('retries substrate failures but never over a known branch', () => {
    const d = detail([]); d.run.EscalationClass = 'infra'; d.run.EscalationRetryable = true;
    expect(row({ detail: d }).verdict).toBe('retry');
    expect(row({ detail: d, branchExists: true }).verdict).toBe('finish-branch');
    d.run.EscalationClass = 'code'; expect(row({ detail: d }).verdict).toBe('unknown');
  });
  it('does not invent branch or title evidence from an MR or worktree', () => {
    const d = detail([]); d.run.WorktreePath = '/work/branch';
    expect(row({ detail: d })).toMatchObject({ verdict: 'unknown', rescueDraft: null, amendCommand: null });
    expect(row({ detail: d }).verdictText).toMatch(/^CHECK MR:/);
  });
  it.each([null, {}, { verdicts: [null] }, { verdicts: [{ file: 'x' }] }])('rejects malformed scope %j', scope => {
    const d = detail(); d.stages[1].Artifacts = { scope_violations: scope };
    expect(row({ detail: d })).toMatchObject({ verdict: 'unknown', scopeKnown: false, amendCommand: null });
  });
  it('requires all paths and rules to be eligible, not a partial amendment', () => {
    const d = detail(); d.stages[0].Artifacts = { slices: [{ files: ['different.go'] }] };
    expect(row({ detail: d }).amendCommand).toBeNull();
    const bad = detail(['../escape']); expect(row({ detail: bad }).amendCommand).toBeNull();
    const mixed = detail();
    const scope = mixed.stages[1].Artifacts!.scope_violations as { verdicts: unknown[] };
    scope.verdicts.push({ file: 'other', rule: 'policy-disabled', admitted: false, slice_index: -1 });
    expect(row({ detail: mixed }).amendCommand).toBeNull();
  });
  it('does not reuse an older scope decision or infer plan paths from prose', () => {
    const d = detail();
    d.stages.push({ ...stage('post_implement_gate', {}), ID: 2, StartedAt: '2026-09-14' });
    expect(row({ detail: d }).amendCommand).toBeNull();
    const prose = detail();
    prose.stages[0].Artifacts = { last_message: 'Please edit cmd/devbox/main.go' };
    expect(row({ detail: prose }).amendCommand).toBeNull();
  });
  it('keeps missing detail visible and sorts oldest first, ignoring resolved items', () => {
    const rows = rescueRows([{ ...item, ID: 'new', CreatedAt: '2026-09-14T00:00:00Z' }, item, { ...item, ID: 'done', State: 'merged' }], {});
    expect(rows.map(r => r.id)).toEqual(['drain', 'new']); expect(rows[0].verdict).toBe('unknown');
  });
  it('selects the latest terminal run for this item only', () => {
    const r = detail().run;
    expect(latestTerminalRun([{ ...r, ID: 'other', BacklogID: 'other' }, { ...r, ID: 'active', State: 'running' }, { ...r, ID: 'old', StartedAt: '2020-01-01' }, { ...r, ID: 'new', StartedAt: '2026-01-01' }], item.ID)?.ID).toBe('new');
  });
});
