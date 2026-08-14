import type { MRWatchState } from '../utils/mrwatchStates.ts';

export interface MergeCandidate {
  agent_id: string;
  branch: string;
  namespace?: string;
  status: string;
  merge_ready: boolean;
  merge_blockers?: string[];
  conflict_files: number;
  blocked_tasks: number;
  task_count: number;
  /** Deep link to the branch view on the upstream forge. Present when the
   * daemon's `LOOM_HUD_GIT_REMOTE_URL` env is set. */
  branch_url?: string;
  /** Deep link to a pre-filled "new merge request" page on the upstream
   * forge. Present when the daemon's `LOOM_HUD_GIT_REMOTE_URL` env is set. */
  merge_request_new_url?: string;
  /** Existing merge request joined by exact source branch. */
  mr_iid?: number;
  mr_state?: MRWatchState;
  mr_web_url?: string;
}

export interface MergeQueueSummary {
  total_branches: number;
  ready_to_merge: number;
  blocked: number;
  conflict_pairs: number;
}

export interface MergeQueueResponse {
  ready: MergeCandidate[];
  blocked: MergeCandidate[];
  summary: MergeQueueSummary;
}

export interface MergeConflictPair {
  left_agent: string;
  left_branch: string;
  right_agent: string;
  right_branch: string;
  conflict_type: string;
  files?: string[];
  detail?: string;
}

export interface MergeConflictsResponse {
  conflicts: MergeConflictPair[];
  count: number;
}

async function parseResponse(res: Response): Promise<any> {
  let data: any = null;
  try {
    data = await res.json();
  } catch {
    data = null;
  }
  if (!res.ok) {
    const msg = data?.error || `${res.status} ${res.statusText}`;
    throw new Error(msg);
  }
  return data;
}

export async function fetchMergeQueue(): Promise<MergeQueueResponse> {
  const res = await globalThis.fetch('/api/merge-queue');
  const data = await parseResponse(res);
  return {
    ready: data?.ready ?? [],
    blocked: data?.blocked ?? [],
    summary: data?.summary ?? { total_branches: 0, ready_to_merge: 0, blocked: 0, conflict_pairs: 0 },
  };
}

export async function fetchMergeConflicts(): Promise<MergeConflictsResponse> {
  const res = await globalThis.fetch('/api/merge-queue/conflicts');
  const data = await parseResponse(res);
  return {
    conflicts: data?.conflicts ?? [],
    count: data?.count ?? 0,
  };
}
