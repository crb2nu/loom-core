import type { BacklogItem, PipelineRun, PipelineRunDetail } from '../stores/mills.svelte.ts';

export type RescueVerdict = 'widen' | 'finish-branch' | 'close' | 'retry' | 'unknown';
export interface RescueEvidence {
  detail: PipelineRunDetail | null;
  // Optional verified evidence. The current HUD read API does not provide
  // the live branch probe or MR title; never infer either from MRIID/path.
  branchExists?: boolean;
  mrTitle?: string;
}
export interface ScopeViolation { file: string; rule: string; admitted: boolean; slice_index: number }
export interface RescueRow {
  id: string; title: string; priority: string; age: number | null;
  runID: string | null; escalationClass: string; failureClass: string;
  mrIID: number | null; project?: string; rescueDraft: boolean | null;
  violations: ScopeViolation[]; scopeKnown: boolean; verdict: RescueVerdict;
  verdictText: string; amendCommand: string | null;
}
const record = (v: unknown): Record<string, unknown> =>
  v !== null && typeof v === 'object' && !Array.isArray(v) ? v as Record<string, unknown> : {};
export function latestTerminalRun(runs: PipelineRun[], itemID: string): PipelineRun | undefined {
  return runs.filter(r => r?.BacklogID === itemID && ['done', 'merged', 'escalated', 'paused', 'failed', 'cancelled'].includes(r.State))
    .sort((a, b) => (Date.parse(b.StartedAt ?? '') || 0) - (Date.parse(a.StartedAt ?? '') || 0) || b.ID.localeCompare(a.ID))[0];
}
// Exact wording from cmd/mcp-mills/tools.go where the same evidence supports it.
const rescueText = 'RESCUE: an implement branch exists — do not requeue (the fresh spawn collides with it and dies FailureClass=configuration). Finish the branch, ready its MR, arm merge-when-pipeline-succeeds, and let mills adopt-green-MR settle the item.';
const checkMRText = 'CHECK MR: a run left an MR behind — verify its pipeline and merge state before deciding.';
export function rescueRows(items: BacklogItem[], runsByItem: Record<string, RescueEvidence | undefined>, now = Date.now()): RescueRow[] {
  return items.filter(i => i.State === 'escalated').map(item => {
    const evidence = runsByItem[item.ID];
    const detail = evidence?.detail;
    const run = detail?.run?.BacklogID === item.ID ? detail.run : undefined;
    const stages = run && Array.isArray(detail?.stages) ? detail.stages : [];
    const latest = (name: string) => stages.filter(s => s && s.Stage === name)
      .sort((a, b) => (Date.parse(b.StartedAt) || 0) - (Date.parse(a.StartedAt) || 0) || b.ID - a.ID)[0];
    const scope = record(latest('post_implement_gate')?.Artifacts?.scope_violations);
    const raw = scope.verdicts;
    const violations: ScopeViolation[] = Array.isArray(raw) ? raw.filter((v): v is ScopeViolation => {
      const r = record(v);
      return typeof r.file === 'string' && r.file.length > 0 && typeof r.rule === 'string' && typeof r.admitted === 'boolean' && Number.isInteger(r.slice_index);
    }) : [];
    const scopeKnown = Array.isArray(raw) && violations.length === raw.length;
    // Some operators only persist plan_slice prose, not structured slices.
    // Do not treat backlog scope or incidental paths in a log as plan output:
    // absent structured evidence deliberately disables the amend command.
    const plan = record(latest('plan_slice')?.Artifacts);
    const slices = Array.isArray(plan.slices) ? plan.slices : [];
    const named = new Set(slices.flatMap(s => {
      const files = record(s).files;
      return Array.isArray(files) ? files.filter((f): f is string => typeof f === 'string') : [];
    }));
    const eligible = scopeKnown && violations.length > 0 && !scope.refusal && violations.every(v =>
      ['sensitive-path', 'no-shared-ancestor', 'outside-scope'].includes(v.rule) && named.has(v.file) &&
      !v.file.startsWith('/') && !v.file.split('/').includes('..'));
    const escalationClass = run?.EscalationClass || 'unknown';
    const failureClass = run?.FailureClass || 'unknown';
    let verdict: RescueVerdict = 'unknown';
    if (eligible) verdict = 'widen';
    else if (scopeKnown && violations.length === 0 && evidence?.branchExists === true) verdict = 'finish-branch';
    else if (run && evidence?.branchExists !== true && run.EscalationRetryable === true &&
      ['infra', 'substrate', 'external_dependency', 'infrastructure'].some(c => c === escalationClass || c === failureClass)) verdict = 'retry';
    else if (run?.EscalationRetryable === false && evidence?.branchExists === false && !run.MRIID && scopeKnown && violations.length === 0) verdict = 'close';
    const verdictText = verdict === 'widen' ? 'WIDEN SCOPE: the plan named every violating file; review and amend scope.'
      : verdict === 'finish-branch' ? rescueText
      : verdict === 'close' ? 'CLOSE: no implement branch exists and the failure is non-retryable.'
      : verdict === 'retry' ? 'RETRY: retryable substrate failure; verify branch and MR state before requeueing.'
      : run?.MRIID ? checkMRText : 'UNKNOWN: insufficient evidence; diagnose before deciding.';
    const created = Date.parse(item.CreatedAt ?? '');
    return { id: item.ID, title: item.Title, priority: item.Priority, age: Number.isFinite(created) ? Math.max(0, now - created) : null,
      runID: run?.ID ?? null, escalationClass, failureClass, mrIID: run?.MRIID ?? null, project: item.TargetProject,
      rescueDraft: evidence?.mrTitle === undefined ? null : evidence.mrTitle.startsWith('Draft: [scope-escalated]'),
      violations, scopeKnown, verdict, verdictText,
      amendCommand: eligible ? `mills_backlog_amend_scope(${JSON.stringify({ id: item.ID, add_files: [...new Set(violations.map(v => v.file))].sort() })})` : null };
  }).sort((a, b) => (b.age ?? -1) - (a.age ?? -1) || a.id.localeCompare(b.id));
}
