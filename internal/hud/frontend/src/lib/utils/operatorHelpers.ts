// Operator Deck helpers — the pure derivations behind the unified in-flight
// board (Operator view). The board renders three lanes (Mills pipeline runs,
// tracked MRs/CI, live agent sessions) as ONE row shape so the lane markup,
// selection model, and severity accents stay identical across domains; these
// functions translate each domain's native type into that shape.
//
// Pure module (no store imports beyond types): the deck's stores are all
// module singletons with their own polling, so keeping the translation here
// makes it unit-testable without mounting Svelte.

import type { PipelineRun, BacklogItem, RunVerdictEvidence } from '../stores/mills.svelte.ts';
import type { MRWatchMergeRequest } from '../stores/mrwatch.svelte.ts';
import type { UnifiedAgent } from './agents.ts';
import { conversationId, liveStatusRank } from './agents.ts';
import { relativeTime } from './format.ts';
import { isLiveMRWatchState } from './mrwatchStates.ts';

export type InflightKind = 'mills' | 'mr' | 'agent';

/**
 * Row accent, ordered by urgency. `busy` is the healthy-and-working accent
 * (spinner-adjacent), distinct from `ok` (healthy-and-settled) so a glance
 * separates "in motion" from "done/waiting".
 */
export type InflightSeverity = 'error' | 'warn' | 'busy' | 'ok' | 'idle';

export interface InflightRow {
  kind: InflightKind;
  /** Selection key, unique across lanes: `<kind>:<native id>`. */
  key: string;
  /** Native id inside its domain (run ID, `repo!iid`, conversation id). */
  id: string;
  title: string;
  subtitle: string;
  /** Native state string, rendered as the row's badge. */
  state: string;
  severity: InflightSeverity;
  /** Relative wall-clock of the row's most recent motion, '' when unknown. */
  age: string;
  /** External link (MR/pipeline web URL) when the domain has one. */
  href?: string;
  /**
   * Concrete drill-down id when `id` is an aggregate. Agent rows are
   * conversation groups, so `id` is the conversation while `drillId` is the
   * lead member's real agent_id (what session lookups and navigation need).
   */
  drillId?: string;
}

/** Stable selection key so the dock survives a poll replacing row arrays. */
export function rowKey(kind: InflightKind, id: string): string {
  return `${kind}:${id}`;
}

// ---- Mills lane -----------------------------------------------------------

const MILLS_SEVERITY: Record<string, InflightSeverity> = {
  escalated: 'error',
  paused: 'warn',
  running: 'busy',
  queued: 'idle',
  done: 'ok',
  merged: 'ok',
};

/**
 * Active Mills pipeline runs → rows. Titles come from the backlog item the
 * run works (the run itself only carries the backlog id); a run whose item
 * fell out of the backlog list (pruned/cross-repo) degrades to the raw id
 * rather than dropping the row — an in-flight run must never be invisible.
 */
export function millsRunRows(
  // Verdict is additive on verdict-aware operator list responses. Accept the
  // legacy Go-field spelling and the JSON-tag spelling during mixed rollout.
  runs: Array<PipelineRun & { Verdict?: RunVerdictEvidence; verdict?: RunVerdictEvidence }>,
  backlog: BacklogItem[],
): InflightRow[] {
  const titles = new Map(backlog.map((b) => [b.ID, b.Title]));
  return runs.map((r) => {
    const state = (r.State || 'unknown').toLowerCase();
    const stage = r.CurrentStage ? ` · ${r.CurrentStage}` : '';
    const attempts = r.Attempts > 1 ? ` · attempt ${r.Attempts}` : '';
    const verdictClass = (r.Verdict?.class ?? r.verdict?.class ?? '').toLowerCase();
    const severity =
      verdictClass === 'merged' || verdictClass === 'merged_after_escalation'
        ? 'ok'
        : MILLS_SEVERITY[state] ?? 'idle';
    return {
      kind: 'mills' as const,
      key: rowKey('mills', r.ID),
      id: r.ID,
      title: titles.get(r.BacklogID) ?? r.BacklogID,
      subtitle: `${r.Template || 'pipeline'}${stage}${attempts}`,
      state,
      severity,
      age: relativeTime(r.StartedAt ?? null),
    };
  });
}

// ---- MR / CI lane ---------------------------------------------------------

// A red head pipeline outranks its classification. The remaining mappings are
// the canonical registry.State taxonomy, not UI-invented aliases.
function mrSeverity(mr: MRWatchMergeRequest): InflightSeverity {
  const pipeline = (mr.pipeline_status || '').toLowerCase();
  if (pipeline === 'failed') return 'error';
  switch ((mr.state || '').toLowerCase()) {
    case 'ci_failed_deterministic':
    case 'conflict':
      return 'error';
    case 'ci_failed_flaky':
    case 'automerge_unarmed':
    case 'pipeline_skipped':
    case 'stale_branch':
      return 'warn';
    case 'awaiting_pipeline':
    case 'ci_running':
      return 'busy';
    case 'ok':
      return pipeline === 'running' || pipeline === 'pending' ? 'busy' : 'ok';
    default:
      return 'idle';
  }
}

export function mrRows(mrs: MRWatchMergeRequest[]): InflightRow[] {
  return mrs.filter((mr) => isLiveMRWatchState(mr.state)).map((mr) => {
    const ref = `${mr.repo}!${mr.iid}`;
    const pipeline = mr.pipeline_status ? ` · ci ${mr.pipeline_status}` : '';
    return {
      kind: 'mr' as const,
      key: rowKey('mr', ref),
      id: ref,
      title: mr.title || ref,
      subtitle: `${ref}${pipeline}`,
      state: mr.state || 'unknown',
      severity: mrSeverity(mr),
      age: relativeTime(mr.last_transition_at ?? null),
      href: mr.web_url,
    };
  });
}

// ---- Agent lane -----------------------------------------------------------

/**
 * How long a conversation stays on the board after its members stop reading
 * as live. The fleet snapshot's per-member status flaps (active↔offline,
 * orphan on heartbeat timing), so raw filtering made rows blink in and out
 * on every poll. A member counts toward liveness if its status is live OR
 * its heartbeat is recent — the flap has to persist this long before the
 * row actually leaves.
 */
const AGENT_LINGER_SECONDS = 300;

function memberIsLive(a: UnifiedAgent): boolean {
  if (a.status === 'active' || a.status === 'idle') return true;
  return a.heartbeat_age_seconds >= 0 && a.heartbeat_age_seconds < AGENT_LINGER_SECONDS;
}

/**
 * The federation mirror's fallback description (internal/hud/mirror/mirror.go
 * buildHeartbeatBody): a local daemon mirroring a presence row that has no
 * active session and no description of its own stamps this marker. Live data
 * shows both shapes — workspace proxy bases ("<type>-<WS_HASH>") and scoped
 * hook presences ("<type>-<WS_HASH>-<SCOPE>") whose conversation ended but
 * whose CLI keepalive still heartbeats. Each carries its own conversation
 * key, so grouping can't fold them and they drowned the lane as ~15 flat
 * rows.
 */
export const MIRROR_PLACEHOLDER_DESCRIPTION = 'loom-core federation mirror';

/**
 * A mirror placeholder is a row whose only identity is the mirror's fallback
 * description AND that reports no work of its own. An agent that carries a
 * current_task keeps its individual row even if its description fell back to
 * the marker — the collapse must never hide a conversation doing real work.
 */
function isMirrorPlaceholder(a: UnifiedAgent): boolean {
  return (
    (a.description ?? '').trim() === MIRROR_PLACEHOLDER_DESCRIPTION &&
    (a.current_task ?? '').trim() === ''
  );
}

/**
 * Agent sessions → one row per CONVERSATION, not per raw agent_id. The
 * lifecycle hooks mint an agent_id per (workspace, conversation), so a chat
 * that hops worktrees produces several ids that appear/vanish across polls —
 * rendering those raw made the lane unreadably churny. Grouping by
 * conversationId (the Fleet table's own axis) gives rows whose identity is
 * stable for the life of the chat; vendor codex/claude app sessions arrive
 * pre-merged upstream in fleetStore.
 *
 * Severity is aggregate and flap-damped: a conversation warns only when EVERY
 * member reads orphaned/blocked — one healthy member proves the chat is fine.
 *
 * Mirror placeholders collapse to ONE summary row: the federation mirror
 * (mirror.go) forwards every non-offline laptop presence, including rows
 * with no active conversation behind them (idle proxy bases and ended-chat
 * hook presences whose keepalive still heartbeats). Each of those is its own
 * conversation key, so without the collapse they render as one flat row per
 * presence and drown the real sessions. A group folds into the summary only
 * when EVERY member is a placeholder (marker description, no current_task) —
 * identities are never merged, only counted, so fleetview's IsBaseOf
 * reconcile semantics and the one-row-per-real-conversation contract are
 * untouched.
 */
/**
 * Transcript-derived context for one conversation (utils/sessionsUnify.ts
 * linkLiveAgents): the human title and repo the hash-only agent id can't
 * express. Optional — the deck renders identically without it, just with
 * hash-named rows.
 */
export interface ConversationContext {
  title: string;
  repo: string;
  vendor: string;
}

export function agentRows(
  agents: UnifiedAgent[],
  context?: Map<string, ConversationContext>,
): InflightRow[] {
  const groups = new Map<string, UnifiedAgent[]>();
  for (const a of agents) {
    const key = conversationId(a.agent_id) || a.agent_id;
    const members = groups.get(key);
    if (members) members.push(a);
    else groups.set(key, [a]);
  }

  const rows: InflightRow[] = [];
  const mirrored: UnifiedAgent[] = []; // lead member of each collapsed group
  for (const [conv, members] of groups) {
    if (!members.some(memberIsLive)) continue;

    // Lead member: most live status wins, freshest heartbeat breaks ties.
    // Its task/branch is what the conversation is "doing"; its agent_id is
    // the drill-down target.
    const lead = [...members].sort(
      (a, b) =>
        liveStatusRank(b.status) - liveStatusRank(a.status) ||
        a.heartbeat_age_seconds - b.heartbeat_age_seconds,
    )[0];

    if (members.every(isMirrorPlaceholder)) {
      mirrored.push(lead);
      continue;
    }

    const allOrphan = members.every((m) => m.is_orphan);
    const anyBlocked = members.some((m) => m.blocked_tasks > 0);
    const anyActive = members.some((m) => m.status === 'active');
    const severity: InflightSeverity =
      allOrphan || anyBlocked ? 'warn' : anyActive ? 'busy' : 'ok';

    const doing = lead.current_task || (lead.branch ? `on ${lead.branch}` : lead.description);
    const sessionsNote = members.length > 1 ? ` · ${members.length} sessions` : '';
    // A linked vendor transcript upgrades the row from hash identity
    // ("claude-code-1735870880") to what the conversation is actually about
    // (its first prompt / summary), with the repo leading the subtitle.
    const ctx = context?.get(conv);
    const title = ctx?.title || conv;
    // Skip the repo prefix when the doing-text already names it (the
    // federation mirror's fallback description embeds the project).
    const subtitlePrefix =
      ctx?.title && ctx.repo && !doing?.includes(ctx.repo) ? `${ctx.repo} · ` : '';
    rows.push({
      kind: 'agent' as const,
      key: rowKey('agent', conv),
      id: conv,
      title,
      subtitle: `${subtitlePrefix}${doing || lead.agent_type}${sessionsNote}`,
      state: allOrphan ? 'orphan' : anyActive ? 'active' : lead.status,
      severity,
      age: relativeTime(lead.last_heartbeat || lead.session_started_at || null),
      drillId: lead.agent_id,
    });
  }

  if (mirrored.length > 0) {
    // Freshest placeholder represents the bucket: its heartbeat is the
    // bucket's age and its agent_id the drill-down target.
    const freshest = mirrored.reduce((a, b) =>
      b.heartbeat_age_seconds < a.heartbeat_age_seconds ? b : a,
    );
    rows.push({
      kind: 'agent' as const,
      key: rowKey('agent', 'mirrored-presences'),
      id: 'mirrored-presences',
      title: `${mirrored.length} mirrored presence${mirrored.length === 1 ? '' : 's'}`,
      subtitle: 'federated from laptops · no active conversation',
      state: 'mirror',
      severity: 'idle',
      age: relativeTime(freshest.last_heartbeat || freshest.session_started_at || null),
      drillId: freshest.agent_id,
    });
  }
  return rows;
}

// ---- Board rollups --------------------------------------------------------

const SEVERITY_RANK: Record<InflightSeverity, number> = {
  error: 0,
  warn: 1,
  busy: 2,
  ok: 3,
  idle: 4,
};

/**
 * Lane ordering: severity buckets first, then a STABLE alphabetical key
 * within a bucket. The upstream arrays reorder on every poll (the fleet
 * snapshot sorts agents by heartbeat recency, runs move as they progress),
 * and passing that order through made same-severity rows swap places every
 * few seconds — an unreadably jumpy board. With the stable tiebreak the only
 * movement left is a row changing severity, which is exactly the movement
 * that means something.
 */
export function sortRowsStable(rows: InflightRow[]): InflightRow[] {
  return [...rows].sort(
    (a, b) => SEVERITY_RANK[a.severity] - SEVERITY_RANK[b.severity] || a.id.localeCompare(b.id),
  );
}

/**
 * Fully pinned ordering: alphabetical by id, ignoring severity. For lanes
 * whose severity signal FLAPS between polls — the fleet's orphan
 * classification flips with heartbeat timing — severity-first ranking moves
 * rows across buckets every few seconds even with a stable tiebreak. Pinning
 * the position and letting the dot/badge carry the state keeps the lane
 * readable; the lane header's attention count still surfaces the rollup.
 */
export function sortRowsPinned(rows: InflightRow[]): InflightRow[] {
  return [...rows].sort((a, b) => a.id.localeCompare(b.id));
}

export interface LaneSummary {
  kind: InflightKind;
  total: number;
  attention: number; // error + warn rows
}

export function laneSummary(kind: InflightKind, rows: InflightRow[]): LaneSummary {
  return {
    kind,
    total: rows.length,
    attention: rows.filter((r) => r.severity === 'error' || r.severity === 'warn').length,
  };
}

/**
 * Resolve a selection key against the current rows. Returns null when the
 * selected row no longer exists (run finished, MR merged, agent gone) so the
 * dock can fall back to the ambient log view instead of showing stale data.
 */
// ---- Mill efficiency (factory model §J4 signals) ---------------------------
//
// First-pass yield = merged/(merged+escalated) over the KPI window, and true
// cost per bolt = cost_per_merged_pipeline_usd (spend including escalated
// runs, divided by merges). These are the factory-model quality dials
// (docs/FACTORY_MODEL.md): the strip shows them so the operator reads line
// health without opening the Factory panel.

export interface MillEfficiency {
  /** 0..100, rounded. */
  yieldPct: number;
  /** Formatted "$X.XX" or null when the KPI key is absent. */
  costPerBolt: string | null;
  /** Chip detail line, e.g. "82% first-pass · $1.49/bolt". */
  detail: string;
  tone: 'ok' | 'warn' | 'error';
}

export function millEfficiency(metrics: {
  pipeline_merged_runs?: number;
  pipeline_escalated_runs?: number;
  cost_per_merged_pipeline_usd?: number;
} | null | undefined): MillEfficiency | null {
  const merged = metrics?.pipeline_merged_runs;
  const escalated = metrics?.pipeline_escalated_runs;
  if (merged === undefined || escalated === undefined) return null;
  const terminal = merged + escalated;
  if (terminal === 0) return null; // idle window — no yield to report
  const yieldPct = Math.round((merged / terminal) * 100);
  const cost = metrics?.cost_per_merged_pipeline_usd;
  const costPerBolt = cost !== undefined && Number.isFinite(cost) ? `$${cost.toFixed(2)}` : null;
  return {
    yieldPct,
    costPerBolt,
    detail: `${yieldPct}% first-pass${costPerBolt ? ` · ${costPerBolt}/bolt` : ''}`,
    tone: yieldPct >= 70 ? 'ok' : yieldPct >= 40 ? 'warn' : 'error',
  };
}

export function findRow(rows: InflightRow[], key: string | null): InflightRow | null {
  if (!key) return null;
  return rows.find((r) => r.key === key) ?? null;
}
