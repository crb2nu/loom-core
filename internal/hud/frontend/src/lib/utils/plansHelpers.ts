// Pure helpers for the Work → Plans panel (Slice 2 of the Work/Mills UX work).
// Plans come from the agent-context Plan store via /api/plans. Refs (MR,
// pipeline) are free-form strings appended by agents; the plan's `project`
// field is the canonical GitLab path_with_namespace (e.g. "services/loom-core").

export interface PlanSlice {
  id: string;
  name: string;
  phase: string;
  order?: number;
  files?: string[];
  branch_name?: string;
  assigned_agent_id?: string;
  mr_ref?: string;
  decisions?: string[];
}

export interface Plan {
  id: string;
  title: string;
  slug?: string;
  project?: string;
  namespace?: string;
  phase: string;
  created_by?: string;
  mr_refs?: string[];
  pipeline_refs?: string[];
  deploy_refs?: string[];
  mirror_path?: string;
  mills_backlog_id?: string;
  kill_test_status?: string;
  slices?: PlanSlice[];
  slice_summary?: Record<string, number>;
  updated_at?: string;
}

export const PLAN_PHASES = [
  'draft', 'planned', 'in_progress', 'in_review',
  'merging', 'merged', 'deployed', 'done',
] as const;
export const PLAN_ADVANCE_TARGETS = [...PLAN_PHASES, 'abandoned'];

const GITLAB_BASE = 'https://gitlab.flexinfer.ai';
const DEFAULT_PROJECT = 'services/loom-core';

export function planPhaseVariant(phase: string): string {
  switch (phase) {
    case 'in_review':
    case 'merging':
      return 'warning';
    case 'merged':
    case 'deployed':
    case 'done':
      return 'success';
    case 'in_progress':
    case 'planned':
      return 'info';
    case 'abandoned':
      return 'error';
    default:
      return 'default';
  }
}

// Normalize a plan.project into a GitLab path_with_namespace. Plans usually
// carry the full "services/loom-core" form; tolerate a bare repo name.
function projectPath(project?: string): string {
  const p = (project ?? '').trim();
  if (!p) return DEFAULT_PROJECT;
  if (p.includes('/')) return p;
  return `services/${p}`;
}

// Build a clickable URL for a free-form MR ref. Handles full URLs, the
// "<project>!<iid>" / "!<iid>" / "<iid>" shapes. Returns '' when no numeric
// IID can be extracted (caller renders plain text instead of a link).
export function gitlabMrUrl(ref: string, project?: string): string {
  const r = (ref ?? '').trim();
  if (!r) return '';
  if (/^https?:\/\//.test(r)) return r;
  const m = r.match(/(\d+)\s*$/);
  if (!m) return '';
  return `${GITLAB_BASE}/${projectPath(project)}/-/merge_requests/${m[1]}`;
}

export function gitlabPipelineUrl(ref: string, project?: string): string {
  const r = (ref ?? '').trim();
  if (!r) return '';
  if (/^https?:\/\//.test(r)) return r;
  const m = r.match(/(\d+)\s*$/);
  if (!m) return '';
  return `${GITLAB_BASE}/${projectPath(project)}/-/pipelines/${m[1]}`;
}

// Short label for a ref chip: "!806" for MRs, "#15561" for pipelines, or the
// raw ref when it isn't a bare number / URL.
export function refLabel(ref: string, kind: 'mr' | 'pipeline'): string {
  const r = (ref ?? '').trim();
  if (/^https?:\/\//.test(r)) {
    const tail = r.split('/').filter(Boolean).pop() ?? r;
    return kind === 'mr' ? `!${tail}` : `#${tail}`;
  }
  const m = r.match(/^\D*(\d+)$/);
  if (m) return kind === 'mr' ? `!${m[1]}` : `#${m[1]}`;
  return r;
}

export interface ProjectGroup {
  project: string;
  items: Plan[];
}

export function groupPlansByProject(plans: Plan[]): ProjectGroup[] {
  const groups = new Map<string, Plan[]>();
  for (const p of plans) {
    const key = (p.project ?? '').trim() || 'unscoped';
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key)!.push(p);
  }
  return Array.from(groups.entries())
    .map(([project, items]) => ({ project, items }))
    .sort((a, b) => a.project.localeCompare(b.project));
}

export interface PhaseColumn {
  phase: string;
  items: Plan[];
}

export function groupPlansByPhase(plans: Plan[]): PhaseColumn[] {
  const cols = PLAN_PHASES.map((ph) => ({
    phase: ph as string,
    items: plans.filter((p) => p.phase === ph),
  })).filter((c) => c.items.length > 0);
  const abandoned = plans.filter((p) => p.phase === 'abandoned');
  if (abandoned.length > 0) cols.push({ phase: 'abandoned', items: abandoned });
  return cols;
}

export function filterPlans(
  plans: Plan[],
  search: string,
  projectFilter: string,
  phaseFilter: string,
): Plan[] {
  let result = plans;
  const q = search.trim().toLowerCase();
  if (q) {
    result = result.filter(
      (p) =>
        (p.title ?? '').toLowerCase().includes(q) ||
        (p.id ?? '').toLowerCase().includes(q) ||
        (p.project ?? '').toLowerCase().includes(q),
    );
  }
  if (projectFilter) {
    result = result.filter((p) => (p.project ?? '').trim() === projectFilter);
  }
  if (phaseFilter) {
    result = result.filter((p) => p.phase === phaseFilter);
  }
  return result;
}

// --- Slice progress (board cards + drawer) ---------------------------------
// Slice lifecycle order; drives the segmented progress bar.
export const SLICE_PHASES = [
  'pending', 'claimed', 'implementing', 'implemented', 'in_review', 'integrated', 'merged',
] as const;

// CSS color for a slice phase segment. Cool→warm→green as a slice advances.
export function slicePhaseColor(phase: string): string {
  switch (phase) {
    case 'merged':
    case 'integrated':
      return 'var(--success, #4c8)';
    case 'in_review':
      return 'var(--warning, #db4)';
    case 'implementing':
    case 'implemented':
    case 'claimed':
      return 'var(--info, #4af)';
    default: // pending / unknown
      return 'var(--fg-dim, #678)';
  }
}

export interface SliceProgress {
  total: number;
  merged: number;
  segments: Array<{ phase: string; count: number; color: string }>;
}

// Build an ordered, colored segment list from a phase->count summary. Returns
// null when there's nothing to show, so callers can omit the bar entirely.
export function sliceProgress(summary?: Record<string, number> | null): SliceProgress | null {
  if (!summary) return null;
  const total = Object.values(summary).reduce((a, b) => a + (b || 0), 0);
  if (total <= 0) return null;
  const segments: SliceProgress['segments'] = [];
  for (const phase of SLICE_PHASES) {
    const count = summary[phase];
    if (count) segments.push({ phase, count, color: slicePhaseColor(phase) });
  }
  // Surface any non-canonical phases at the end so nothing is silently dropped.
  const known = new Set<string>(SLICE_PHASES);
  for (const [phase, count] of Object.entries(summary)) {
    if (!known.has(phase) && count) segments.push({ phase, count, color: slicePhaseColor(phase) });
  }
  return { total, merged: summary['merged'] ?? 0, segments };
}

export function projectOptionsFrom(plans: Plan[]): Array<{ value: string; label: string }> {
  const set = new Set<string>();
  for (const p of plans) {
    const proj = (p.project ?? '').trim();
    if (proj) set.add(proj);
  }
  return Array.from(set).sort().map((p) => ({ value: p, label: p }));
}
