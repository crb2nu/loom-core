// Projects store (Slice 4 of the Work/Mills UX work).
//
// There is no first-class Project entity in the backend — "project" is a
// GitLab path_with_namespace field stamped on plans, tasks, and sessions. This
// store federates those three existing read endpoints client-side into a
// per-project rollup so the HUD can offer a Project lens (a single pane that
// answers "what's the state of <project>?"). A server-side pm_project_status
// enrichment can layer on later without changing this shape.
import { taskStore, type Task } from './tasks.svelte.ts';
import type { Plan } from '../utils/plansHelpers';
import { planPhaseVariant } from '../utils/plansHelpers';
import type { BadgeVariant } from '../utils/tokens.ts';
import { createPoller } from '../utils/poller.ts';

export interface ProjectSession {
  id: string;
  agent_id: string;
  project?: string;
  namespace?: string;
  status: string;
  description?: string;
  started_at?: string;
}

export interface ProjectRollup {
  project: string;
  plans: Plan[];
  plansByPhase: Array<{ phase: string; count: number; variant: BadgeVariant }>;
  tasks: Task[];
  openTasks: number;
  inProgressTasks: number;
  blockedTasks: number;
  sessions: ProjectSession[];
  activeSessions: number;
  agents: string[];
  lastActivity: number; // epoch ms, 0 when unknown
}

function tsOf(v?: string): number {
  if (!v) return 0;
  const t = new Date(v).getTime();
  return Number.isFinite(t) ? t : 0;
}

function normProject(p?: string): string {
  return (p ?? '').trim() || 'unscoped';
}

class ProjectsStore {
  plans = $state<Plan[]>([]);
  sessions = $state<ProjectSession[]>([]);
  loading = $state(false);
  error = $state<string | null>(null);
  available = $state(true);
  lastUpdated = $state<Date | null>(null);

  // 30s poll — refreshes the projects list.
  private poller = createPoller(() => { void this.fetch(); }, 30000);

  async fetch(): Promise<void> {
    this.loading = true;
    this.error = null;
    const [plansRes, sessRes] = await Promise.allSettled([
      globalThis.fetch('/api/plans'),
      globalThis.fetch('/api/sessions?status=active&limit=200'),
    ]);
    try {
      // /api/plans is the primary source for this lens. A rejected or non-OK
      // response was previously ignored silently, so a daemon-down failure
      // left the panel on an indistinguishable "No projects yet" empty state
      // (error swallowed). Throw so the catch records it and the panel can
      // tell a fetch failure from a genuinely empty roster.
      if (plansRes.status === 'rejected') {
        throw plansRes.reason instanceof Error
          ? plansRes.reason
          : new Error(String(plansRes.reason));
      }
      if (!plansRes.value.ok) {
        throw new Error(`Plans: HTTP ${plansRes.value.status}`);
      }
      const data = await plansRes.value.json();
      this.available = data.available !== false;
      this.plans = data.plans ?? [];
      // Sessions are secondary — a failure here only degrades the active-
      // session counts, so it stays best-effort and never sets error.
      if (sessRes.status === 'fulfilled' && sessRes.value.ok) {
        const sessData = await sessRes.value.json();
        this.sessions = sessData.sessions ?? [];
      }
      this.lastUpdated = new Date();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    } finally {
      this.loading = false;
    }
    // Tasks power task counts; piggyback on the shared task store.
    void taskStore.fetch();
  }

  get projects(): ProjectRollup[] {
    const map = new Map<string, ProjectRollup>();
    const ensure = (key: string): ProjectRollup => {
      let r = map.get(key);
      if (!r) {
        r = {
          project: key,
          plans: [],
          plansByPhase: [],
          tasks: [],
          openTasks: 0,
          inProgressTasks: 0,
          blockedTasks: 0,
          sessions: [],
          activeSessions: 0,
          agents: [],
          lastActivity: 0,
        };
        map.set(key, r);
      }
      return r;
    };

    for (const p of this.plans) {
      const r = ensure(normProject(p.project));
      r.plans.push(p);
      r.lastActivity = Math.max(r.lastActivity, tsOf(p.updated_at));
    }
    for (const t of taskStore.tasks ?? []) {
      const r = ensure(normProject((t as Task & { project?: string }).project));
      r.tasks.push(t);
      if (t.status === 'in_progress') r.inProgressTasks++;
      else if (t.status === 'blocked') r.blockedTasks++;
      if (t.status === 'pending' || t.status === 'in_progress' || t.status === 'blocked') r.openTasks++;
      r.lastActivity = Math.max(r.lastActivity, tsOf(t.updated_at));
    }
    const agentSets = new Map<string, Set<string>>();
    for (const s of this.sessions) {
      const key = normProject(s.project);
      const r = ensure(key);
      r.sessions.push(s);
      if (s.status === 'active') r.activeSessions++;
      if (s.agent_id) {
        if (!agentSets.has(key)) agentSets.set(key, new Set());
        agentSets.get(key)!.add(s.agent_id);
      }
      r.lastActivity = Math.max(r.lastActivity, tsOf(s.started_at));
    }

    for (const [key, r] of map) {
      r.agents = Array.from(agentSets.get(key) ?? []).sort();
      const phaseCounts = new Map<string, number>();
      for (const p of r.plans) phaseCounts.set(p.phase, (phaseCounts.get(p.phase) ?? 0) + 1);
      r.plansByPhase = Array.from(phaseCounts.entries())
        .map(([phase, count]) => ({ phase, count, variant: planPhaseVariant(phase) }))
        .sort((a, b) => b.count - a.count);
    }

    return Array.from(map.values()).sort((a, b) => {
      // Most-active first, then by name for stability.
      if (b.lastActivity !== a.lastActivity) return b.lastActivity - a.lastActivity;
      return a.project.localeCompare(b.project);
    });
  }

  byId(project: string): ProjectRollup | undefined {
    return this.projects.find((p) => p.project === project);
  }

  startPolling(intervalMs = 30000): void {
    void this.fetch();
    this.poller.start(intervalMs);
  }

  stopPolling(): void {
    this.poller.stop();
  }
}

export const projectsStore = new ProjectsStore();
