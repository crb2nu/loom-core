/**
 * Provenance helpers for the mill-floor views — who authored a piece of work,
 * and which repo it landed in.
 *
 * Rune-free so vitest exercises the classification directly, mirroring the
 * lineage.ts / format.ts precedent (spec §2.2: no data logic in the component).
 *
 * Everything here reads fields the backlog list ALREADY serves. `GET
 * /api/mills/backlog` returns the full untagged store.BacklogItem, so Labels,
 * CreatedBy and TargetProject arrive on every row of every 15s poll. Until now
 * the HUD fetched them, typed them, and dropped them: TargetProject's only use
 * anywhere was as an argument to mrURL(), i.e. the panel resolved the repo and
 * spent the answer on a link target without ever showing it.
 */

import type { BacklogItem } from '../../../stores/mills.svelte.ts';
import type { BadgeVariant } from '../../../utils/tokens.ts';

/**
 * Origin buckets, ordered by how they read on the floor.
 *
 * The taxonomy is grounded in the live CreatedBy distribution (544 items,
 * 2026-08-16), not in guesswork — which is why `plan` exists at all. It is the
 * single largest author on the floor (mills:plan-slice-emitter, 95 of the 159
 * items merged in the trailing 48h) and appears in nobody's mental model until
 * they see it named.
 */
export type OriginKind =
  | 'operator'
  | 'council'
  | 'sensor'
  | 'plan'
  | 'agent'
  | 'canary'
  | 'automation'
  | 'unknown';

export interface Origin {
  kind: OriginKind;
  /** Short chip text. */
  label: string;
  /** The evidence this classification rests on, for the chip's tooltip. */
  detail: string;
}

/** Display tone per origin. Reuses the shared badge vocabulary — no new palette. */
export function originTone(kind: OriginKind): BadgeVariant {
  switch (kind) {
    case 'operator':
      return 'success';
    case 'council':
      return 'accent';
    case 'sensor':
      return 'info';
    case 'plan':
      return 'info';
    case 'agent':
      return 'accent';
    case 'canary':
      return 'warning';
    default:
      return 'muted';
  }
}

/**
 * Normalize a CreatedBy value for matching.
 *
 * CreatedBy is free text and the fleet writes it 19 different ways —
 * "mills:plan-slice-emitter", "mills canary autopilot", "claude-code
 * ralph-loop", "claude-code:millsreconcilerslow-investigation",
 * "operator-seed:claude-code". Equality against a fixed set would misclassify
 * most of them, so separators collapse to a single space and matching is by
 * substring/prefix.
 */
function normalizeActor(raw: string | undefined | null): string {
  return (raw ?? '')
    .toLowerCase()
    .replace(/[_:/-]+/g, ' ')
    .replace(/\s+/g, ' ')
    .trim();
}

function labelSet(item: Pick<BacklogItem, 'Labels'> | undefined | null): Set<string> {
  return new Set((item?.Labels ?? []).map((l) => (l ?? '').toLowerCase().trim()));
}

/**
 * Classify one backlog item's origin.
 *
 * Precedence is deliberate and load-bearing, most-specific first: an item can
 * legitimately match several rungs (a canary is created by automation; a
 * sensor item is agent-authored) and the FIRST match is the one that tells the
 * operator something they didn't already know.
 *
 * Order: canary → sensor → operator → council → plan → agent → automation.
 */
export function originOf(item: Pick<BacklogItem, 'Labels' | 'CreatedBy'> | undefined | null): Origin {
  const labels = labelSet(item);
  const raw = (item?.CreatedBy ?? '').trim();
  const actor = normalizeActor(raw);
  const via = raw ? `created_by=${raw}` : 'created_by unset';

  // Canary: synthetic heartbeat traffic. First because it is the one bucket an
  // operator scanning real work wants to subtract, and it is authored by
  // automation that would otherwise swallow it one rung down.
  if (labels.has('mills-canary') || actor.includes('canary')) {
    const evidence = labels.has('mills-canary') ? 'label=mills-canary' : via;
    return { kind: 'canary', label: 'canary', detail: `heartbeat traffic — ${evidence}` };
  }

  // Sensor: the maintenance loop's portfolio sensor. Rare on the floor (one
  // item fleet-wide as of 2026-08-16) but the operator explicitly needs it
  // distinguishable, and label evidence is unambiguous where CreatedBy is not.
  if (labels.has('maintenance-loop') || labels.has('portfolio-sensor') || actor.includes('sensor')) {
    return {
      kind: 'sensor',
      label: 'sensor',
      detail: `posted by the maintenance loop — ${
        labels.has('maintenance-loop') ? 'label=maintenance-loop' : via
      }`,
    };
  }

  // Operator-directed: a human asked for this specific thing. Above council and
  // plan because human intent outranks the machinery that carried it out.
  if (
    labels.has('operator-directed') ||
    actor.startsWith('operator') ||
    actor.includes('operator directed') ||
    actor === 'hud user'
  ) {
    const evidence = labels.has('operator-directed') ? 'label=operator-directed' : via;
    return { kind: 'operator', label: 'operator', detail: `human-directed — ${evidence}` };
  }

  if (actor.includes('council')) {
    return { kind: 'council', label: 'council', detail: `council decision — ${via}` };
  }

  // Plan: the mill's own plan-slice decomposition — the floor's dominant author.
  if (actor.includes('plan slice') || actor.includes('pattern stamp') || labels.has('mills-from-plan-slice')) {
    return {
      kind: 'plan',
      label: 'plan',
      detail: `mill plan decomposition — ${
        labels.has('mills-from-plan-slice') ? 'label=mills-from-plan-slice' : via
      }`,
    };
  }

  if (actor.includes('claude') || actor.includes('codex') || actor.includes('gemini')) {
    return { kind: 'agent', label: 'agent', detail: `agent-authored — ${via}` };
  }

  // Automation: the remaining known machine authors (api, mrwatch_shepherd,
  // gitlab importer). Named rather than lumped into unknown so "unknown"
  // stays meaningful — it should mean "we genuinely can't tell", and an
  // unknown that shows up in the filter is a prompt to extend this list.
  if (actor === 'api' || actor.includes('shepherd') || actor.includes('importer') || actor.includes('mills')) {
    return { kind: 'automation', label: 'automation', detail: `machine-authored — ${via}` };
  }

  return { kind: 'unknown', label: 'unknown', detail: via };
}

/** The home repo every project-less item targets. */
export const HOME_REPO = 'loom-core';

/**
 * Short display name for a TargetProject.
 *
 * Two encodings mean the same repo and MUST collapse or the filter lies: 460
 * items carry an empty TargetProject (implicitly home) and 63 carry an explicit
 * "services/loom-core". Both are loom-core. Empty renders as the home repo
 * name rather than a blank cell — "unset" and "home" are the same fact here,
 * and a blank cell reads as missing data.
 */
export function repoLabel(targetProject: string | undefined | null): string {
  const trimmed = (targetProject ?? '').trim();
  if (trimmed === '') return HOME_REPO;
  const tail = trimmed.split('/').filter(Boolean).pop() ?? '';
  return tail || HOME_REPO;
}

/** Full bucket-qualified path for a chip tooltip; home items say so explicitly. */
export function repoTitle(targetProject: string | undefined | null): string {
  const trimmed = (targetProject ?? '').trim();
  return trimmed === '' ? `${HOME_REPO} (home repo — no TargetProject set)` : trimmed;
}

/** True when the item runs against a repo other than the operator's home. */
export function isCrossRepo(targetProject: string | undefined | null): boolean {
  return repoLabel(targetProject) !== HOME_REPO;
}

/**
 * The "why" line for a merged row.
 *
 * Title first, PlanID only as a fallback — the inverse of the existing
 * plan/book column, which preferred PlanID and so suppressed the human-readable
 * reason on exactly the plan-linked items that dominate the floor.
 */
export function whyLine(
  item: Partial<Pick<BacklogItem, 'Title' | 'PlanID'>> | undefined | null,
): string {
  const title = (item?.Title ?? '').trim();
  if (title) return title;
  return (item?.PlanID ?? '').trim();
}

/** One option in a facet filter, with its live count. */
export interface FacetOption<T extends string = string> {
  value: T;
  label: string;
  count: number;
}

/**
 * Build the repo facet from a set of items, most-populated first with the home
 * repo pinned to the front so the list doesn't reorder under the operator as
 * counts drift.
 */
export function repoFacet(items: Array<Pick<BacklogItem, 'TargetProject'>>): FacetOption[] {
  const counts = new Map<string, number>();
  for (const item of items ?? []) {
    const key = repoLabel(item?.TargetProject);
    counts.set(key, (counts.get(key) ?? 0) + 1);
  }
  return [...counts.entries()]
    .map(([value, count]) => ({ value, label: value, count }))
    .sort((a, b) => {
      if (a.value === HOME_REPO) return -1;
      if (b.value === HOME_REPO) return 1;
      return b.count - a.count || a.value.localeCompare(b.value);
    });
}

/** Build the origin facet, ordered by the canonical bucket order, empties dropped. */
export function originFacet(
  items: Array<Pick<BacklogItem, 'Labels' | 'CreatedBy'>>,
): FacetOption<OriginKind>[] {
  const order: OriginKind[] = [
    'operator',
    'council',
    'sensor',
    'plan',
    'agent',
    'canary',
    'automation',
    'unknown',
  ];
  const counts = new Map<OriginKind, number>();
  for (const item of items ?? []) {
    const kind = originOf(item).kind;
    counts.set(kind, (counts.get(kind) ?? 0) + 1);
  }
  return order
    .filter((kind) => (counts.get(kind) ?? 0) > 0)
    .map((kind) => ({ value: kind, label: kind, count: counts.get(kind) ?? 0 }));
}
