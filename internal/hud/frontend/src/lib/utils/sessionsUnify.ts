// Unification logic for the Sessions panel — pure, rune-free, unit-tested.
//
// Joins the two halves of "what have my agents been doing":
//   1. vendor transcripts — the on-disk truth of every Claude Code / Codex
//      desktop session (local bridge + federated Mac hosts), which carry
//      cwd, title, and kind but know nothing about the fleet, and
//   2. live fleet agents — presence/session rows keyed by hook-minted
//      agent ids (<base>-<WS_HASH>[-<SESSION_SCOPE>]), which know liveness
//      but have hash-only identities.
//
// The join is EXACT, not fuzzy: SESSION_SCOPE is the POSIX cksum of the
// vendor session uuid and WS_HASH the cksum of the workspace root path
// (pkg/generator/configs_hooks.go), so recomputing both over transcript
// metadata (utils/cksum.ts) links a transcript to its live agent row —
// which is what lets the UI put a truthful LIVE badge on a transcript and
// a human title on a hash-named agent row.

import { conversationId } from './agents.ts';
import type { UnifiedAgent } from './agents.ts';
import { cksumString } from './cksum.ts';
import type { VendorSession } from '../clients/vendorSessions.ts';

// ---- Repo grouping --------------------------------------------------------

/** Where a transcript ran, reduced to a stable display grouping. */
export interface RepoRef {
  /** Grouping key + section label, e.g. "services/loom-core". */
  repo: string;
  /** Linked-worktree name when the cwd is inside one, e.g. "mac-session-…". */
  worktree?: string;
}

const WORKSPACE_BUCKETS = new Set(['services', 'libs', 'labs', 'platform', 'private', 'apps']);

/**
 * repoFromCwd reduces a session cwd to its repo identity: the
 * `<bucket>/<repo>` pair under a workspace root when recognizable, with
 * `.claude/worktrees/<name>` / `.worktrees/<name>` suffixes split out as the
 * worktree facet (one repo section, not one section per worktree). Falls
 * back to the last two path segments for out-of-workspace cwds.
 */
export function repoFromCwd(cwd: string | null | undefined): RepoRef {
  const segments = (cwd ?? '').split('/').filter(Boolean);
  if (segments.length === 0) return { repo: '(unknown)' };

  let repoEnd = -1;
  let repo = '';
  const wsIdx = segments.indexOf('workspace');
  if (wsIdx >= 0 && wsIdx + 1 < segments.length) {
    const bucket = segments[wsIdx + 1];
    if (WORKSPACE_BUCKETS.has(bucket) && wsIdx + 2 < segments.length) {
      repo = `${bucket}/${segments[wsIdx + 2]}`;
      repoEnd = wsIdx + 3;
    } else {
      repo = bucket;
      repoEnd = wsIdx + 2;
    }
  } else {
    repo = segments.slice(-2).join('/');
    repoEnd = segments.length;
  }

  const rest = segments.slice(repoEnd);
  for (let i = 0; i < rest.length - 1; i += 1) {
    if ((rest[i] === '.worktrees' || rest[i] === 'worktrees') && rest[i + 1]) {
      return { repo, worktree: rest[i + 1] };
    }
  }
  return { repo };
}

// ---- Live linkage ---------------------------------------------------------

/** Result of joining transcripts against the live fleet. */
export interface LiveLinkage {
  /** Transcript keys (`vendor:id`) that have a live fleet agent behind them. */
  liveKeys: Set<string>;
  /**
   * Conversation id (utils/agents.ts) → transcript-derived context for the
   * fleet/deck side of the join: the human title and repo that the hash-only
   * agent id can't express.
   */
  byConversation: Map<string, { title: string; repo: string; vendor: string }>;
}

export function transcriptKey(s: Pick<VendorSession, 'vendor' | 'id'>): string {
  return `${s.vendor}:${s.id}`;
}

function agentIdParts(agentId: string): { wsHash?: string; scope?: string } {
  const parts = agentId.split('-');
  const isNumeric = (p: string) => p.length > 0 && /^[0-9]+$/.test(p);
  let wsIdx = -1;
  for (let i = 0; i < parts.length; i += 1) {
    if (isNumeric(parts[i])) {
      wsIdx = i;
      break;
    }
  }
  if (wsIdx < 0) return {};
  let scope: string | undefined;
  for (let i = parts.length - 1; i > wsIdx; i -= 1) {
    if (isNumeric(parts[i])) {
      scope = parts[i];
      break;
    }
  }
  return { wsHash: parts[wsIdx], scope };
}

function sessionTime(s: VendorSession): number {
  const t = new Date(s.modified_at || s.started_at || 0).getTime();
  return Number.isFinite(t) ? t : 0;
}

function isLiveAgent(a: UnifiedAgent): boolean {
  return a.status === 'active' || a.status === 'idle';
}

/**
 * linkLiveAgents joins live fleet agents to vendor transcripts.
 *
 * Claude (and any conversation-scoped vendor) links by SESSION_SCOPE =
 * cksum(transcript uuid) — exact per conversation. Codex's hook mints
 * workspace-anchored ids (no scope), so scopeless agents link by WS_HASH =
 * cksum(cwd), taking the freshest interactive transcript in that workspace;
 * kind-tagged transcripts (subagent/automation) only match scoped ids so a
 * background run never steals the workspace's identity.
 */
export function linkLiveAgents(
  agents: UnifiedAgent[],
  sessions: VendorSession[],
): LiveLinkage {
  const byScope = new Map<string, VendorSession>();
  const byWsHash = new Map<string, VendorSession[]>();
  for (const s of sessions) {
    if (!s.id) continue;
    const scope = cksumString(s.id);
    const prev = byScope.get(scope);
    if (!prev || sessionTime(s) > sessionTime(prev)) byScope.set(scope, s);
    if (s.cwd) {
      const ws = cksumString(s.cwd);
      const list = byWsHash.get(ws);
      if (list) list.push(s);
      else byWsHash.set(ws, [s]);
    }
  }

  const linkage: LiveLinkage = { liveKeys: new Set(), byConversation: new Map() };
  for (const agent of agents) {
    if (!isLiveAgent(agent)) continue;
    const { wsHash, scope } = agentIdParts(agent.agent_id);
    let hit: VendorSession | undefined;
    if (scope) hit = byScope.get(scope);
    if (!hit && wsHash) {
      const candidates = (byWsHash.get(wsHash) ?? []).filter((s) => !s.kind);
      candidates.sort((a, b) => sessionTime(b) - sessionTime(a));
      hit = candidates[0];
    }
    if (!hit) continue;
    linkage.liveKeys.add(transcriptKey(hit));
    const conv = conversationId(agent.agent_id) || agent.agent_id;
    const existing = linkage.byConversation.get(conv);
    if (!existing || (hit.title && !existing.title)) {
      linkage.byConversation.set(conv, {
        title: hit.title ?? '',
        repo: repoFromCwd(hit.cwd).repo,
        vendor: hit.vendor,
      });
    }
  }
  return linkage;
}

// ---- Grouped rendering model ---------------------------------------------

/** One transcript row inside a repo group. */
export interface SessionRow {
  key: string;
  session: VendorSession;
  live: boolean;
  worktree?: string;
}

/** One repo section of the unified sessions list. */
export interface RepoGroup {
  repo: string;
  /** Interactive rows, newest first. */
  rows: SessionRow[];
  /**
   * Background transcripts (codex subagents/automations, claude sidechains)
   * collapsed out of the main list, newest first. Rendered as one aggregate
   * affordance — the "N background runs" pattern — expandable on demand.
   */
  background: SessionRow[];
  /** Max session time across ALL rows, for ordering groups. */
  lastActivity: number;
  /** Distinct hosts contributing rows (federated Macs), for the header. */
  hosts: string[];
}

/**
 * groupSessions buckets transcripts by repo (repoFromCwd), splits background
 * kinds out of the interactive flow, and orders groups by recency. A live
 * linked row always leads its group regardless of mtime jitter.
 */
export function groupSessions(sessions: VendorSession[], linkage: LiveLinkage): RepoGroup[] {
  const groups = new Map<string, RepoGroup>();
  for (const s of sessions) {
    const ref = repoFromCwd(s.cwd);
    let group = groups.get(ref.repo);
    if (!group) {
      group = { repo: ref.repo, rows: [], background: [], lastActivity: 0, hosts: [] };
      groups.set(ref.repo, group);
    }
    const row: SessionRow = {
      key: transcriptKey(s),
      session: s,
      live: linkage.liveKeys.has(transcriptKey(s)),
      worktree: ref.worktree,
    };
    if (s.kind) group.background.push(row);
    else group.rows.push(row);
    group.lastActivity = Math.max(group.lastActivity, sessionTime(s));
    if (s.host && !group.hosts.includes(s.host)) group.hosts.push(s.host);
  }

  const byRecency = (a: SessionRow, b: SessionRow) =>
    Number(b.live) - Number(a.live) || sessionTime(b.session) - sessionTime(a.session);
  for (const group of groups.values()) {
    group.rows.sort(byRecency);
    group.background.sort(byRecency);
  }
  return [...groups.values()].sort((a, b) => b.lastActivity - a.lastActivity);
}

/** Display title for a transcript row, with stable fallbacks. */
export function sessionTitle(s: VendorSession): string {
  if (s.title) return s.title;
  if (s.source) return s.source;
  return s.id.slice(0, 12);
}
