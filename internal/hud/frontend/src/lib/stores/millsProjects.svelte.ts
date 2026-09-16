// Mills project registry — GET /api/mills/projects. Every project the
// operator knows (home, cross_repo.demand_projects, intake.gitlab.projects,
// the runtime bootstrapped/onboarded registry) with a readiness verdict
// computed by the operator from the same policy accessors the reconciler
// and gates use, so the HUD never has to guess.
import { createPoller } from '../utils/poller.ts';
import type { BadgeVariant } from '../utils/tokens.ts';

export interface RegisteredProject {
  project: string;
  plan_id: string;
  web_url: string;
  created_by: string;
  created_at: string;
}

export interface MillsProjectEntry {
  project: string;
  sources: string[];
  web_url?: string;
  protected_paths: 'per_repo' | 'global' | 'unknown';
  protected_path_count: number;
  max_usd_per_run?: number;
  max_runs_per_day?: number;
  registered?: RegisteredProject;
  ready: boolean;
  blockers: string[];
  pending: string[];
}

export interface MillsProjectsRegistry {
  home_project: string;
  cross_repo_enabled: boolean;
  allow_bootstrapped: boolean;
  bootstrap_allowed_groups: string[];
  onboardable: boolean;
  policy_mr_available: boolean;
  projects: MillsProjectEntry[];
  count: number;
}

export type ReadinessKind = 'home' | 'weaving' | 'registered' | 'blocked' | 'unknown';

export interface Readiness {
  kind: ReadinessKind;
  label: string;
  variant: BadgeVariant;
  detail: string;
}

/** Operator blocker/pending codes → operator-facing copy. */
export const BLOCKER_COPY: Record<string, string> = {
  cross_repo_disabled: 'cross-repo execution is off in policy (cross_repo.enabled)',
  not_in_demand: 'not in cross_repo.demand_projects and not registered at runtime',
  allow_bootstrapped_off: 'registered at runtime, but policy has allow_bootstrapped: false',
  protected_paths_unknown: 'no protected-paths entry — every touched file counts as protected',
  git_policy_missing: 'live via the runtime registry; not yet in Git policy (open the policy MR)',
};

export function describeCode(code: string): string {
  return BLOCKER_COPY[code] ?? code.replaceAll('_', ' ');
}

/** The readiness chip for an entry (or for a project the operator has never
 *  heard of when `entry` is undefined). `registryKnown` is false when the
 *  registry never loaded (older operator, or unreachable): then an absent
 *  entry means "we cannot tell", not "not onboarded" — the home project would
 *  otherwise read as un-onboarded. */
export function readinessOf(entry: MillsProjectEntry | undefined, homeProject = '', registryKnown = true): Readiness {
  if (!entry) {
    if (!registryKnown) {
      return { kind: 'unknown', label: 'unknown', variant: 'muted', detail: 'The Mills registry is unavailable — the deployed operator predates intake or is unreachable.' };
    }
    return { kind: 'unknown', label: 'not onboarded', variant: 'muted', detail: 'Mills does not know this repo yet.' };
  }
  if (entry.sources.includes('home') || (homeProject && entry.project === homeProject)) {
    return { kind: 'home', label: 'home', variant: 'accent', detail: 'The operator’s own repo.' };
  }
  if (!entry.ready) {
    return {
      kind: 'blocked',
      label: 'blocked',
      variant: 'warning',
      detail: entry.blockers.map(describeCode).join(' · '),
    };
  }
  if (entry.pending.length > 0) {
    return {
      kind: 'registered',
      label: 'registered · live',
      variant: 'info',
      detail: entry.pending.map(describeCode).join(' · '),
    };
  }
  return { kind: 'weaving', label: 'weaving', variant: 'success', detail: 'Admitted in Git policy; Mills can weave here.' };
}

class MillsProjectsStore {
  registry = $state<MillsProjectsRegistry | null>(null);
  loading = $state(false);
  error = $state<string | null>(null);
  /** false once the endpoint 404s — the deployed operator predates intake. */
  available = $state(true);
  lastUpdated = $state<Date | null>(null);

  private poller = createPoller(() => { void this.fetch(); }, 30000);

  get entries(): MillsProjectEntry[] {
    return this.registry?.projects ?? [];
  }

  /** true once a registry response has been received; false while loading,
   *  after a 404 (older operator) or when the operator is unreachable. */
  get known(): boolean {
    return this.registry !== null;
  }

  byProject(project: string): MillsProjectEntry | undefined {
    const key = project.trim().replace(/\.git$/, '').replace(/^\/+|\/+$/g, '');
    return this.entries.find((e) => e.project === key);
  }

  /** Entries whose repo name (last path segment) matches — how a GitHub
   *  mirror such as crb2nu/fi-fhir finds its GitLab twin libs/fi-fhir. A name
   *  match is a strong hint in this workspace (one repo per name), not proof;
   *  callers label it as a twin, never as the repo itself. */
  byName(name: string): MillsProjectEntry[] {
    const key = name.trim().replace(/\.git$/, '').toLowerCase();
    if (!key) return [];
    return this.entries.filter((e) => (e.project.split('/').pop() ?? '').toLowerCase() === key);
  }

  async fetch(): Promise<void> {
    this.loading = true;
    try {
      const res = await globalThis.fetch('/api/mills/projects', { cache: 'no-store' });
      if (res.status === 404) {
        this.available = false;
        this.error = null;
        return;
      }
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      this.registry = (await res.json()) as MillsProjectsRegistry;
      this.available = true;
      this.error = null;
      this.lastUpdated = new Date();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    } finally {
      this.loading = false;
    }
  }

  startPolling(intervalMs = 30000): void {
    void this.fetch();
    this.poller.start(intervalMs);
  }

  stopPolling(): void {
    this.poller.stop();
  }
}

export const millsProjectsStore = new MillsProjectsStore();
