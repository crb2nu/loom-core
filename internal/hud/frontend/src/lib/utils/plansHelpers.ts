// Pure helpers for the Work → Plans panel (Slice 2 of the Work/Mills UX work).
// Plans come from the agent-context Plan store via /api/plans. Refs (MR,
// pipeline) are free-form strings appended by agents; the plan's `project`
// field is the canonical GitLab path_with_namespace (e.g. "services/loom-core").

import type { BadgeVariant } from './tokens.ts';
import { mrURL } from './gitlabLinks.ts';

const GITLAB_BASE = 'https://gitlab.flexinfer.ai';

export interface PlanSlice {
  id: string;
  name: string;
  phase: string;
  order?: number;
  goal?: string;
  files?: string[];
  branch_name?: string;
  assigned_agent_id?: string;
  mr_ref?: string;
  decisions?: string[];
  // Connective tissue: the slice DAG edges + the contract it provides/consumes.
  // depends_on holds resolved slice_ids (<plan_id>#<order>) the store minted.
  depends_on?: string[];
  interface_contracts?: string;
  acceptance_criteria?: string;
}

export interface Plan {
  id: string;
  title: string;
  slug?: string;
  project?: string;
  namespace?: string;
  phase: string;
  priority?: string;
  // The plan's synthesized end-state + through-line (2-4 sentences), distinct
  // from the raw brief in spec_doc. Absent on older/sparse plans — render nothing.
  objective?: string;
  respun_from?: string;
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

// normalizePlan coerces a raw /api/plans record into null-safe shape at the
// fetch boundary: objective is always a string, slices always an array, and each
// slice's array fields default to []. Downstream render + helpers then never see
// `undefined` (which would print as "undefined" in an inline label). Older plans
// that predate the connective-tissue fields normalize cleanly to empty.
export function normalizePlan(raw: Plan | null | undefined): Plan | null {
  if (!raw) return null;
  return {
    ...raw,
    objective: raw.objective ?? '',
    slices: (raw.slices ?? []).map((s) => ({
      ...s,
      depends_on: s.depends_on ?? [],
      interface_contracts: s.interface_contracts ?? '',
      acceptance_criteria: s.acceptance_criteria ?? '',
    })),
  };
}

// sliceDependsLabels maps a slice's depends_on slice_ids to compact order
// labels (e.g. "#1", "#3") by looking each id up in the plan's slice list. A
// dep whose slice isn't in the list falls back to the "#N" suffix of the id
// (slice ids are "<plan_id>#<order>"), then to the raw id. Empty in → [] out,
// so a slice with no prerequisites renders nothing.
export function sliceDependsLabels(
  dependsOn: string[] | undefined,
  slices: PlanSlice[] | undefined,
): string[] {
  if (!dependsOn?.length) return [];
  const orderById = new Map<string, number>();
  for (const s of slices ?? []) {
    if (typeof s.order === 'number') orderById.set(s.id, s.order);
  }
  const out: string[] = [];
  for (const dep of dependsOn) {
    const d = (dep ?? '').trim();
    if (!d) continue;
    const order = orderById.get(d);
    if (order !== undefined) {
      out.push(`#${order}`);
      continue;
    }
    const hash = d.lastIndexOf('#');
    out.push(hash >= 0 && hash < d.length - 1 ? `#${d.slice(hash + 1)}` : d);
  }
  return out;
}

export const PLAN_PHASES = [
  'draft', 'planned', 'in_progress', 'in_review',
  'merging', 'merged', 'deployed', 'done',
] as const;
export const PLAN_ADVANCE_TARGETS = [...PLAN_PHASES, 'abandoned'];

// Warp-beam priority buckets (P0 dispatches first). Unset = the Mills
// plan-slice emitter stamps its own default on emitted items.
export const PLAN_PRIORITIES = ['P0', 'P1', 'P2', 'P3'] as const;

// planPriorityVariant maps a bucket to a Badge variant so urgency reads at a
// glance: P0 red, P1 amber, P2 blue, P3/unset neutral.
export function planPriorityVariant(priority: string | undefined): BadgeVariant {
  switch (priority) {
    case 'P0':
      return 'error';
    case 'P1':
      return 'warning';
    case 'P2':
      return 'info';
    default:
      return 'muted';
  }
}

const DEFAULT_PROJECT = 'services/loom-core';

export function planPhaseVariant(phase: string): BadgeVariant {
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
      return 'muted';
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
  return mrURL(projectPath(project), Number(m[1])) ?? '';
}

// Build a GitLab branch (tree) URL for a slice's working branch. Encode each
// path segment but keep the slashes literal: GitLab's tree route 404s on a
// percent-encoded slash (`feat%2Ffoo`), and its own UI links branches as
// `…/-/tree/feat/foo`. encodeURIComponent('feat/foo') would yield the broken
// form, so split on '/' and encode the segments instead.
export function gitlabBranchUrl(branch: string, project?: string): string {
  const b = (branch ?? '').trim();
  if (!b) return '';
  if (/^https?:\/\//.test(b)) return b;
  const ref = b.split('/').map(encodeURIComponent).join('/');
  return `${GITLAB_BASE}/${projectPath(project)}/-/tree/${ref}`;
}

// Build the instructions body for handing a plan (or a single slice) off to an
// existing agent via POST /api/handoffs. It's a self-contained brief the
// receiving agent reads from its inbox — mirrors the spawn task_description but
// stands alone, and always points back at the live Plan store so the agent
// pulls the authoritative spec rather than trusting this snapshot.
export function dispatchInstructions(plan: Plan, slice?: PlanSlice): string {
  const lines: string[] = [];
  if (slice) {
    lines.push(`Work slice "${slice.name}" of plan "${plan.title}" (${plan.id}).`);
    if (slice.branch_name) lines.push(`Branch: ${slice.branch_name}`);
    if (slice.files?.length) lines.push(`Files: ${slice.files.join(', ')}`);
  } else {
    lines.push(`Work on plan "${plan.title}" (${plan.id}).`);
  }
  if (plan.project) lines.push(`Project: ${plan.project}`);
  if (plan.mirror_path) lines.push(`Spec: ${plan.mirror_path}`);
  lines.push(`Fetch the live spec + slices from the agent-context Plan store (plan_id=${plan.id}) before starting.`);
  return lines.join('\n');
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
      return 'var(--success)';
    case 'in_review':
      return 'var(--warning)';
    case 'implementing':
    case 'implemented':
    case 'claimed':
      return 'var(--info)';
    default: // pending / unknown
      return 'var(--fg-dim)';
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

// buildMillsBacklogItem projects a plan (or a single slice) into the operator's
// BacklogItem wire shape for POST /api/mills/backlog.
//
// Casing is load-bearing: the operator decodes the body into store.BacklogItem
// with json.Decoder.DisallowUnknownFields() and that struct carries NO json tags
// on its top-level fields (see pkg/mills/store/types.go), so the canonical wire
// shape is PascalCase (ID/Title/PlanID/State/SpecDoc/Slices/CreatedBy/
// TargetProject) — the same shape writeJSON emits and the frontend BacklogItem
// type reads back. Snake_case top-level keys (plan_id/spec_doc/created_by) do NOT
// match via Go's case-insensitive field lookup because it can't bridge the
// underscore, so the strict decoder rejects the body with 400 "unknown field".
// Only nested slices keep their snake_case json tags (name/files).
//
// TargetProject is carried through from the plan's `project` (the canonical
// GitLab path_with_namespace). Without it a plan minted into a non-home repo —
// e.g. a bootstrapped services/procmodel handed off from the Spinning Room —
// would silently run against the operator's home repo instead of the minted one.
export function buildMillsBacklogItem(
  plan: Plan,
  slice?: PlanSlice | null,
): Record<string, unknown> {
  const slices = slice
    ? [{ name: slice.name, files: slice.files || [] }]
    : (plan.slices || []).map((s) => ({ name: s.name, files: s.files || [] }));
  const item: Record<string, unknown> = {
    ID: slice ? `bl-${plan.slug || plan.id}-${slice.id}` : `bl-${plan.slug || plan.id}`,
    Title: slice ? `${plan.title} — ${slice.name}` : plan.title,
    PlanID: plan.id,
    State: 'queued',
    SpecDoc: plan.mirror_path || '',
    Slices: slices,
    CreatedBy: 'hud-user',
  };
  const project = (plan.project ?? '').trim();
  if (project) item.TargetProject = project;
  return item;
}
