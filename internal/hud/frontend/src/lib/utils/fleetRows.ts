// Pure row builders for the Fleet panel. Extracted from FleetPanel.svelte
// during the Slice B1 panel decomp (.loom/117) so the join logic stays
// out of both the panel composition shell and the fleet store. Single seam
// for testing row shape.

import type { Session, SessionTreeNode } from '../stores/fleet.svelte.ts';
import type { SpawnState } from '../stores/spawn.svelte.ts';
import { conversationId, liveStatusRank, rootAgentId, type UnifiedAgent } from './agents.ts';
import { sanitizeText } from './format.ts';

export interface FleetRow {
  id: string;
  agent: UnifiedAgent;
  depth: number;
  ungrouped: boolean;
  // True when this row was nested under a lead because it is the SAME
  // conversation (conversationId) in a different repo/worktree — not a real
  // session parent/child link. The renderer shows a quiet "same conversation"
  // pill instead of the misleading "root session" badge.
  conversationSibling: boolean;
  // On a conversation's lead row, how many member rows (repos/worktrees) the
  // conversation spans. Only set (>1) when the chat touched more than one
  // workspace, so the renderer can show an "N repos" pill. Undefined otherwise.
  conversationMemberCount?: number;
  session: Session | null;
  parentSession: Session | null;
  rootSession: Session | null;
  childSessions: Session[];
  lineage: Session[];
  liveChildCount: number;
  totalChildCount: number;
}

export interface FleetRowsInput {
  agents: UnifiedAgent[];
  sortKey: string;
  sortDir: 'asc' | 'desc';
  groupByRootSession: boolean;
  sessionById: Map<string, Session>;
  sessionTree: SessionTreeNode[];
  parentSession: (sessionId: string) => Session | null | undefined;
  rootSession: (sessionId: string) => Session | null | undefined;
  childSessions: (sessionId: string) => Session[];
  sessionLineage: (sessionId: string) => Session[];
  agentLookup: Map<string, UnifiedAgent>;
}

export interface FleetRowsResult {
  rows: FleetRow[];
  ungroupedStartIndex: number;
  rootGroupCount: number;
  ungroupedCount: number;
}

export function compareFleetAgents(
  left: UnifiedAgent,
  right: UnifiedAgent,
  sortKey: string,
  sortDir: 'asc' | 'desc',
): number {
  let cmp = 0;
  switch (sortKey) {
    case 'agent':
      cmp = sanitizeText(left.agent_id ?? '').localeCompare(sanitizeText(right.agent_id ?? ''));
      break;
    case 'status': {
      const order: Record<string, number> = { active: 0, idle: 1, offline: 2 };
      cmp = (order[left.status] ?? 9) - (order[right.status] ?? 9);
      break;
    }
    case 'evidence':
      cmp = Number(right.has_session) - Number(left.has_session);
      if (cmp === 0) cmp = Number(right.has_presence) - Number(left.has_presence);
      break;
    case 'namespace':
      cmp = sanitizeText(left.namespace ?? '').localeCompare(sanitizeText(right.namespace ?? ''));
      break;
    case 'heartbeat':
      cmp =
        new Date(left.last_heartbeat || left.session_started_at || 0).getTime() -
        new Date(right.last_heartbeat || right.session_started_at || 0).getTime();
      break;
    default:
      break;
  }
  return sortDir === 'desc' ? -cmp : cmp;
}

export function buildFleetRows(input: FleetRowsInput): FleetRowsResult {
  const {
    agents,
    sortKey,
    sortDir,
    groupByRootSession,
    sessionById,
    sessionTree,
    parentSession,
    rootSession,
    childSessions,
    sessionLineage,
    agentLookup,
  } = input;

  // Every agent is rowed at least twice — once in the flat pass below, once
  // via flattenSessionNode — and each build ran four session lookups. Rows are
  // treated as immutable everywhere downstream (consumers spread to override),
  // so one cached instance per agent is safe to share.
  const rowCache = new Map<string, FleetRow>();

  function baseRow(agent: UnifiedAgent): FleetRow {
    const cached = rowCache.get(agent.agent_id);
    if (cached) return cached;
    const session = agent.session_id ? (sessionById.get(agent.session_id) ?? null) : null;
    const parent = session ? (parentSession(session.id) ?? null) : null;
    const root = session ? (rootSession(session.id) ?? null) : null;
    const children = session ? childSessions(session.id) : [];
    const lineage = session ? sessionLineage(session.id) : [];
    const liveChildCount = children.filter((child) => agentLookup.has(child.agent_id)).length;
    const row: FleetRow = {
      id: agent.agent_id,
      agent,
      depth: 0,
      ungrouped: false,
      conversationSibling: false,
      session,
      parentSession: parent,
      rootSession: root,
      childSessions: children,
      lineage,
      liveChildCount,
      totalChildCount: children.length,
    };
    rowCache.set(agent.agent_id, row);
    return row;
  }

  function buildRow(agent: UnifiedAgent, depth = 0, ungrouped = false): FleetRow {
    const row = baseRow(agent);
    if (row.depth === depth && row.ungrouped === ungrouped) return row;
    return { ...row, depth, ungrouped };
  }

  function leadAgentForNode(
    node: SessionTreeNode,
    agentBySessionId: Map<string, UnifiedAgent>,
  ): UnifiedAgent | null {
    const direct = agentBySessionId.get(node.session.id);
    if (direct) return direct;
    for (const child of node.children ?? []) {
      const nested = leadAgentForNode(child, agentBySessionId);
      if (nested) return nested;
    }
    return null;
  }

  function flattenSessionNode(
    node: SessionTreeNode,
    agentBySessionId: Map<string, UnifiedAgent>,
    depth = 0,
  ): FleetRow[] {
    const rows: FleetRow[] = [];
    const directAgent = agentBySessionId.get(node.session.id);
    if (directAgent) rows.push(buildRow(directAgent, depth));
    const sortedChildren = [...(node.children ?? [])].sort((left, right) => {
      const leftLead = leadAgentForNode(left, agentBySessionId);
      const rightLead = leadAgentForNode(right, agentBySessionId);
      if (leftLead && rightLead) return compareFleetAgents(leftLead, rightLead, sortKey, sortDir);
      if (leftLead) return -1;
      if (rightLead) return 1;
      return new Date(left.session.started_at ?? 0).getTime() - new Date(right.session.started_at ?? 0).getTime();
    });
    for (const child of sortedChildren) {
      rows.push(...flattenSessionNode(child, agentBySessionId, depth + 1));
    }
    return rows;
  }

  const flatRows = [...agents]
    .sort((a, b) => compareFleetAgents(a, b, sortKey, sortDir))
    .map((agent) => buildRow(agent, 0));

  if (!groupByRootSession) {
    return {
      rows: flatRows,
      ungroupedStartIndex: -1,
      rootGroupCount: 0,
      ungroupedCount: 0,
    };
  }

  const agentBySessionId = new Map<string, UnifiedAgent>();
  for (const agent of agents) {
    if (agent.session_id && sessionById.has(agent.session_id)) {
      agentBySessionId.set(agent.session_id, agent);
    }
  }

  const groupedRows: FleetRow[] = [];
  const seenAgents = new Set<string>();
  const sortedRoots = [...sessionTree].sort((left, right) => {
    const leftLead = leadAgentForNode(left, agentBySessionId);
    const rightLead = leadAgentForNode(right, agentBySessionId);
    if (leftLead && rightLead) return compareFleetAgents(leftLead, rightLead, sortKey, sortDir);
    if (leftLead) return -1;
    if (rightLead) return 1;
    return new Date(left.session.started_at ?? 0).getTime() - new Date(right.session.started_at ?? 0).getTime();
  });

  // Flatten each session-tree root into its row list (preserving real
  // subagent depth), then bucket those root-lists by the lead agent's
  // CONVERSATION identity (conversationId). This collapses one chat that moved
  // across repos/worktrees — distinct agent_ids that share a SESSION_SCOPE but
  // differ in WS_HASH, with NO session parent/root linkage — under a single
  // lead, nested one level in. (Distinct chats that merely share a repo have
  // different SESSION_SCOPEs and correctly stay separate.)
  const rootLists: Array<{ key: string; rows: FleetRow[] }> = [];
  for (const root of sortedRoots) {
    const rows = flattenSessionNode(root, agentBySessionId);
    if (rows.length === 0) continue;
    const lead = rows[0].agent;
    rootLists.push({ key: conversationId(lead.agent_id) || lead.agent_id, rows });
  }

  const bucketsByKey = new Map<string, FleetRow[][]>();
  const bucketOrder: string[] = [];
  for (const rl of rootLists) {
    let bucket = bucketsByKey.get(rl.key);
    if (!bucket) {
      bucket = [];
      bucketsByKey.set(rl.key, bucket);
      bucketOrder.push(rl.key);
    }
    bucket.push(rl.rows);
  }

  // The most-live member of a conversation should lead it, so the heartbeat
  // and status shown for the group are the real, freshest ones — not whichever
  // repo's row happened to sort first. Rank by status (active > idle > …) then
  // recency (last heartbeat, falling back to session start).
  function listLiveScore(rows: FleetRow[]): { rank: number; recency: number } {
    let rank = 0;
    let recency = 0;
    for (const row of rows) {
      rank = Math.max(rank, liveStatusRank(row.agent.status));
      const ts = new Date(
        row.agent.last_heartbeat || row.agent.session_started_at || 0,
      ).getTime();
      if (Number.isFinite(ts)) recency = Math.max(recency, ts);
    }
    return { rank, recency };
  }

  for (const key of bucketOrder) {
    const lists = bucketsByKey.get(key)!;
    // Freshest, most-active member-list leads; the rest nest under it.
    const ranked = lists
      .map((rows) => ({ rows, score: listLiveScore(rows) }))
      .sort((a, b) => b.score.rank - a.score.rank || b.score.recency - a.score.recency);

    // Collapse member-lists that resolve to the SAME workspace identity
    // (rootAgentId = base+WS_HASH). A conversation's "N repos" pill should
    // count distinct WORKSPACES it touched, not raw member rows.
    //   - Conversation-scoped vendors (claude/gemini): one chat that hopped
    //     repos keeps its SESSION_SCOPE but changes WS_HASH per repo, so each
    //     member has a distinct rootAgentId and survives as a real repo sibling.
    //   - Workspace-anchored vendors (codex): the scopeless `codex-<WS>` and
    //     scoped `codex-<WS>-<SCOPE>` ids for one app share a single WS_HASH —
    //     conversationId folds them into one bucket, but they are the SAME
    //     agent's duplicate evidence, not two repos. Keeping only the freshest
    //     per workspace collapses the twin to one row (no bogus "2 repos" pill /
    //     "same conversation" child). seenAgents (below) still iterates `lists`,
    //     so the dropped twin id is marked seen and never resurfaces ungrouped.
    const repsByWorkspace = new Map<string, FleetRow[]>();
    for (const { rows } of ranked) {
      const wsKey = rootAgentId(rows[0].agent.agent_id) || rows[0].agent.agent_id;
      if (!repsByWorkspace.has(wsKey)) repsByWorkspace.set(wsKey, rows);
    }
    // Insertion order = ranked (freshest-first) order, so reps[0] still leads.
    const reps = [...repsByWorkspace.values()];
    const memberCount = reps.length;
    reps.forEach((rows, listIndex) => {
      if (listIndex === 0) {
        // The lead member keeps its real (session-tree) depths. Tag its root
        // row with the conversation's member count so the renderer can show an
        // "N repos" pill when the chat spanned more than one workspace.
        rows.forEach((row, rowIndex) => {
          groupedRows.push(
            rowIndex === 0 && memberCount > 1
              ? { ...row, conversationMemberCount: memberCount }
              : row,
          );
        });
      } else {
        // Subsequent member-lists are the SAME conversation in another
        // repo/worktree: indent one level under the lead. The list's own root
        // row (rowIndex 0) is flagged conversationSibling so the renderer shows
        // "same conversation"; any genuine subagents below it keep their
        // session-child pills.
        rows.forEach((row, rowIndex) => {
          groupedRows.push({
            ...row,
            depth: row.depth + 1,
            conversationSibling: rowIndex === 0 ? true : row.conversationSibling,
          });
        });
      }
    });
    for (const rows of lists) for (const row of rows) seenAgents.add(row.agent.agent_id);
  }

  // Anything not slotted into a session tree (orphans, idle session-less
  // presences, spawn-only entries) gets appended below the grouped section
  // with `ungrouped: true` so the renderer can show a divider before the
  // first one.
  for (const row of flatRows) {
    if (!seenAgents.has(row.agent.agent_id)) {
      groupedRows.push({ ...row, ungrouped: true });
    }
  }

  let ungroupedStartIndex = -1;
  for (let i = 0; i < groupedRows.length; i++) {
    if (groupedRows[i].ungrouped) {
      ungroupedStartIndex = i;
      break;
    }
  }

  // One group per distinct conversation bucket in the grouped section.
  const groupKeys = new Set<string>(bucketOrder);

  const ungroupedCount = groupedRows.filter((r) => r.ungrouped).length;

  return {
    rows: groupedRows,
    ungroupedStartIndex,
    rootGroupCount: groupKeys.size,
    ungroupedCount,
  };
}

export function buildSpawnByAgentId(spawns: SpawnState[]): Map<string, SpawnState> {
  const map = new Map<string, SpawnState>();
  for (const s of spawns) {
    map.set(s.agent_id, s);
  }
  return map;
}

export function buildExpiringClaims(
  fileClaims: Array<{ agent_id: string; file_path: string; expires_at?: string | null }>,
  windowMs = 5 * 60 * 1000,
): Map<string, string[]> {
  const map = new Map<string, string[]>();
  const now = Date.now();
  const cutoff = now + windowMs;
  for (const claim of fileClaims) {
    if (!claim.expires_at) continue;
    const exp = new Date(claim.expires_at).getTime();
    if (exp > now && exp <= cutoff) {
      const arr = map.get(claim.agent_id) ?? [];
      arr.push(claim.file_path);
      map.set(claim.agent_id, arr);
    }
  }
  return map;
}
