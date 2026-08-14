// Sandbox store — fetches devbox sandbox data from GET /api/sandbox
// and subscribes to SSE events for real-time exec/build activity.
// Follows the health.svelte.ts SSE-first pattern with fallback polling.
import { untrack } from 'svelte';
import { eventStore } from './events.svelte.ts';
import { isStaleFromTimestamp, stalenessStore } from './staleness.svelte.ts';
import { createPoller } from '../utils/poller.ts';
import { adminFetch, labsAuthStore } from './labsAuth.svelte.ts';

export interface SandboxSummary {
  available: boolean;
  status?: string;
  reason?: string;
  hint?: string;
  start_command?: string;
  backend?: string;
  total_sandboxes: number;
  running: number;
  paused: number;
  stopped?: number;
  total_execs: number;
  total_builds: number;
  uptime_seconds: number;
  projects: string[];
  agent_labels?: Record<string, string>;
}

export interface SandboxEvent {
  type: string;       // "exec", "build", "start", "stop"
  project: string;
  detail: string;
  timestamp: Date;
}

export interface SandboxPolicy {
  configured: boolean;
  require_sandbox?: string[];
  recommend_sandbox?: string[];
  auto_provision?: boolean;
  default_backend?: string;
}

export interface SandboxCapabilities {
  available: boolean;
  backend?: string;
  auth_required: boolean;
  supported_actions: string[];
  project_count?: number;
  projects?: string[];
  notes?: {
    async_exec?: boolean;
    polling_required?: boolean;
    streaming_output?: boolean;
    quality_gate?: boolean;
    detect?: boolean;
    telemetry_source?: string;
    sandbox_event_source?: string;
  };
}

export interface SandboxExecRun {
  exec_id: string;
  status: string;
  project: string;
  command: string;
  started_at?: string;
  completed_at?: string;
  elapsed_ms?: number;
  duration_ms?: number;
  exit_code?: number;
  stdout_tail?: string;
  stderr_tail?: string;
  error?: string;
}

export interface SandboxProjectEntry {
  project: string;
  status: string;
  image?: string;
  backend?: string;
  agent_id?: string;
  running?: boolean;
  uptime?: string;
  last_used?: string;
  error?: string;
}

// One detected language runtime (mirrors detect.LanguageSpec).
export interface SandboxLanguage {
  language: string;
  version?: string;
  dep_manager?: string;
  tools?: string[];
}

// Environment fingerprint from devbox_detect — what gets baked into the image.
export interface SandboxDetect {
  project: string;
  languages?: SandboxLanguage[];
  system_deps?: string[];
  build_targets?: string[];
  hash?: string;
  devcontainer?: unknown;
}

// One quality-gate check result (mirrors mcp-devbox qualityCheckResult).
export interface QualityCheck {
  name: string;
  passed: boolean;
  exit_code?: number;
  duration_ms: number;
  output_tail?: string;
  stderr_tail?: string;
}

// Aggregate quality-gate result (mirrors mcp-devbox qualityGateResult).
export interface QualityGateRun {
  project: string;
  language: string;
  passed: boolean;
  checks: QualityCheck[];
  total_duration_ms: number;
  ran_at: Date;
}

const MAX_EVENTS = 20;
const MAX_EXEC_RUNS = 8;

class SandboxStore {
  summary = $state<SandboxSummary | null>(null);
  available = $state(false);
  loading = $state(false);
  error = $state<string | null>(null);
  lastAction = $state<{ kind: 'build' | 'stop' | 'exec'; project: string; message: string; image?: string; cached?: boolean; execId?: string } | null>(null);
  recentEvents = $state<SandboxEvent[]>([]);
  lastUpdated = $state<Date | null>(null);
  policy = $state<SandboxPolicy | null>(null);
  capabilities = $state<SandboxCapabilities | null>(null);
  capabilitiesLoading = $state(false);
  capabilitiesError = $state<string | null>(null);
  execRuns = $state<SandboxExecRun[]>([]);
  projectStatus = $state(new Map<string, SandboxProjectEntry[]>());
  projectStatusLoading = $state(new Set<string>());

  // Detected environment for the active project (devbox_detect).
  detect = $state<SandboxDetect | null>(null);
  detectLoading = $state(false);
  detectError = $state<string | null>(null);

  // Latest quality-gate run (devbox_quality_gate).
  qualityGate = $state<QualityGateRun | null>(null);
  qualityGateRunning = $state(false);
  qualityGateError = $state<string | null>(null);

  // Staleness (Slice B3) — see fleet.svelte.ts for the pattern.
  staleAfter = 90_000;
  get isStale(): boolean {
    // Staleness only applies while polling is active (page mounted). An
    // unmounted page's store keeps a frozen lastUpdated forever; reporting
    // it stale would pin the global "Stale data" banner permanently.
    if (!this.poller.running) return false;
    return isStaleFromTimestamp(this.lastUpdated, this.staleAfter);
  }

  // 60s watchdog poll — fires on SSE-down OR on stale.
  private poller = createPoller(() => {
    if (!eventStore.connected || this.isStale) this.fetch();
  }, 60000);
  // 3s exec poll — refreshes in-flight exec status (best-effort).
  private execPoller = createPoller(() => {
    this.pollActiveExecs().catch(() => { /* best-effort */ });
  }, 3000);
  private eventUnsubs: Array<() => void> = [];

  get runningCount(): number {
    return this.summary?.running ?? 0;
  }

  get pausedCount(): number {
    return this.summary?.paused ?? 0;
  }

  get totalExecs(): number {
    return this.summary?.total_execs ?? 0;
  }

  get totalBuilds(): number {
    return this.summary?.total_builds ?? 0;
  }

  get totalSandboxes(): number {
    return this.summary?.total_sandboxes ?? 0;
  }

  get projects(): string[] {
    return this.summary?.projects ?? [];
  }

  clearError(): void {
    this.error = null;
  }

  get activeExecs(): SandboxExecRun[] {
    return this.execRuns.filter((run) => run.status === 'running');
  }

  async fetch(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      const res = await globalThis.fetch('/api/sandbox');
      if (!res.ok) throw new Error(`Sandbox API: ${res.status}`);
      const data: SandboxSummary = await res.json();
      this.summary = data;
      this.available = data.available ?? false;
      this.lastUpdated = new Date();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
      this.available = false;
    } finally {
      this.loading = false;
    }
  }

  async fetchCapabilities(): Promise<void> {
    this.capabilitiesLoading = true;
    this.capabilitiesError = null;
    try {
      const res = await globalThis.fetch('/api/sandbox/capabilities');
      if (!res.ok) throw new Error(`Sandbox capabilities API: ${res.status}`);
      this.capabilities = await res.json();
    } catch (e) {
      this.capabilitiesError = e instanceof Error ? e.message : String(e);
    } finally {
      this.capabilitiesLoading = false;
    }
  }

  /** Apply full sandbox snapshot from SSE hud.sandbox event. */
  applySnapshot(data: Record<string, unknown>): void {
    this.summary = data as unknown as SandboxSummary;
    this.available = (data.available as boolean) ?? false;
    this.lastUpdated = new Date();
    this.error = null;
  }

  /** Push a sandbox activity event from SSE hud.sandbox.event. */
  pushEvent(data: Record<string, unknown>): void {
    const evt: SandboxEvent = {
      type: (data.type as string) ?? 'unknown',
      project: (data.project as string) ?? '',
      detail: (data.detail as string) ?? '',
      timestamp: new Date((data.timestamp as string) ?? Date.now()),
    };
    this.recentEvents = [evt, ...this.recentEvents].slice(0, MAX_EVENTS);
  }

  private normalizeExecRun(data: Record<string, unknown>): SandboxExecRun {
    return {
      exec_id: String(data.exec_id ?? ''),
      status: String(data.status ?? 'unknown'),
      project: String(data.project ?? ''),
      command: String(data.command ?? ''),
      started_at: typeof data.started_at === 'string' ? data.started_at : undefined,
      completed_at: typeof data.completed_at === 'string' ? data.completed_at : undefined,
      elapsed_ms: typeof data.elapsed_ms === 'number' ? data.elapsed_ms : Number(data.elapsed_ms ?? 0),
      duration_ms: typeof data.duration_ms === 'number' ? data.duration_ms : Number(data.duration_ms ?? 0),
      exit_code: typeof data.exit_code === 'number' ? data.exit_code : (data.exit_code == null ? undefined : Number(data.exit_code)),
      stdout_tail: typeof data.stdout_tail === 'string' ? data.stdout_tail : undefined,
      stderr_tail: typeof data.stderr_tail === 'string' ? data.stderr_tail : undefined,
      error: typeof data.error === 'string' ? data.error : undefined,
    };
  }

  private upsertExecRun(run: SandboxExecRun): void {
    const next = [run, ...this.execRuns.filter((existing) => existing.exec_id !== run.exec_id)];
    this.execRuns = next.slice(0, MAX_EXEC_RUNS);
  }

  async startSandbox(project: string): Promise<void> {
    this.error = null;
    try {
      const res = await adminFetch('/api/sandbox/start', {
        method: 'POST',
        requireToken: true,
        action: 'Starting a sandbox',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ project }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error((data as { error?: string }).error || `Build image: ${res.status}`);
      const cached = data.cached === true;
      const image = typeof data.image === 'string' ? data.image : undefined;
      this.lastAction = {
        kind: 'build',
        project,
        message: typeof data.message === 'string' && data.message
          ? data.message
          : image
            ? (cached ? 'Image already built (cached)' : 'Sandbox image built')
            : `Image build requested for ${project}`,
        image,
        cached,
      };
      // A successful build changes the detected image; refresh env preview.
      if (this.detect?.project === project) this.fetchDetect(project);
      // Refresh after building.
      await this.fetch();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    }
  }

  /** Fetch the detected environment fingerprint for a project (open GET). */
  async fetchDetect(project: string): Promise<void> {
    const target = project.trim();
    if (!target) {
      this.detect = null;
      this.detectError = null;
      return;
    }
    this.detectLoading = true;
    this.detectError = null;
    try {
      const res = await globalThis.fetch(`/api/sandbox/detect/${encodeURIComponent(target)}`);
      const data = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error((data as { error?: string }).error || `Detect: ${res.status}`);
      // Guard against a stale response if the active project changed mid-flight.
      const detect = data as SandboxDetect;
      if ((detect.project ?? target) === target) {
        this.detect = { ...detect, project: detect.project ?? target };
      }
    } catch (e) {
      this.detect = null;
      this.detectError = e instanceof Error ? e.message : String(e);
    } finally {
      this.detectLoading = false;
    }
  }

  /** Run the fmt → lint → test quality gate for a project (admin action). */
  async runQualityGate(project: string, checks?: string[], failFast = true): Promise<void> {
    const target = project.trim();
    if (!target || this.qualityGateRunning) return;
    this.qualityGateRunning = true;
    this.qualityGateError = null;
    try {
      const res = await adminFetch('/api/sandbox/quality-gate', {
        method: 'POST',
        requireToken: true,
        action: 'Running the quality gate',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ project: target, checks, fail_fast: failFast }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error((data as { error?: string }).error || `Quality gate: ${res.status}`);
      const checksArr = Array.isArray(data.checks)
        ? (data.checks as Record<string, unknown>[]).map((c) => ({
            name: String(c.name ?? ''),
            passed: c.passed === true,
            exit_code: typeof c.exit_code === 'number' ? c.exit_code : undefined,
            duration_ms: typeof c.duration_ms === 'number' ? c.duration_ms : Number(c.duration_ms ?? 0),
            output_tail: typeof c.output_tail === 'string' ? c.output_tail : undefined,
            stderr_tail: typeof c.stderr_tail === 'string' ? c.stderr_tail : undefined,
          }))
        : [];
      this.qualityGate = {
        project: target,
        language: typeof data.language === 'string' ? data.language : 'unknown',
        passed: data.passed === true,
        checks: checksArr,
        total_duration_ms: typeof data.total_duration_ms === 'number' ? data.total_duration_ms : Number(data.total_duration_ms ?? 0),
        ran_at: new Date(),
      };
    } catch (e) {
      this.qualityGateError = e instanceof Error ? e.message : String(e);
    } finally {
      this.qualityGateRunning = false;
    }
  }

  async stopSandbox(project: string): Promise<void> {
    this.error = null;
    try {
      const res = await adminFetch('/api/sandbox/stop', {
        method: 'POST',
        requireToken: true,
        action: 'Stopping a sandbox',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ project }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error((data as { error?: string }).error || `Stop sandbox: ${res.status}`);
      this.lastAction = {
        kind: 'stop',
        project,
        message: typeof data.message === 'string' ? data.message : `Sandbox stop requested for ${project}`,
      };
      // Refresh after stopping.
      await this.fetch();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    }
  }

  async startExec(project: string, command: string, timeout = '10m'): Promise<void> {
    this.error = null;
    try {
      const res = await adminFetch('/api/sandbox/exec', {
        method: 'POST',
        requireToken: true,
        action: 'Running a sandbox command',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ project, command, timeout }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error((data as { error?: string }).error || `Sandbox exec: ${res.status}`);

      const run = this.normalizeExecRun(data as Record<string, unknown>);
      this.upsertExecRun(run);
      this.lastAction = {
        kind: 'exec',
        project,
        message: `Queued ${command}`,
        execId: run.exec_id || undefined,
      };
      await this.fetch();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    }
  }

  async pollExec(execId: string): Promise<void> {
    const res = await adminFetch(`/api/sandbox/exec/${encodeURIComponent(execId)}`, {
      requireToken: true,
      action: 'Polling a sandbox command',
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      throw new Error((data as { error?: string }).error || `Sandbox exec poll: ${res.status}`);
    }
    this.upsertExecRun(this.normalizeExecRun(data as Record<string, unknown>));
  }

  async pollActiveExecs(): Promise<void> {
    if (!labsAuthStore.hasToken || this.activeExecs.length === 0) {
      return;
    }
    const results = await Promise.allSettled(this.activeExecs.map((run) => this.pollExec(run.exec_id)));
    for (const result of results) {
      if (result.status === 'rejected') {
        this.error = result.reason instanceof Error ? result.reason.message : String(result.reason);
      }
    }
  }

  async fetchPolicy(): Promise<void> {
    try {
      const res = await globalThis.fetch('/api/sandbox/policy');
      if (!res.ok) return;
      const data = await res.json();
      // If it has require_sandbox or recommend_sandbox, it's configured.
      this.policy = { configured: !!(data.require_sandbox || data.recommend_sandbox), ...data };
    } catch {
      // Policy is optional — silently ignore errors.
    }
  }

  async fetchProjectStatus(project: string): Promise<void> {
    // Snapshot reactive reads OUTSIDE the caller's tracking context. This is
    // reached synchronously from SandboxLive's mount $effect; a tracked read
    // of projectStatusLoading here, combined with the synchronous rewrite
    // just below (new Set identity every call), re-runs that effect
    // unboundedly — Svelte kills the effect tree with
    // effect_update_depth_exceeded and the panel goes dark.
    if (!untrack(() => labsAuthStore.hasToken)) return;
    const next = untrack(() => new Set(this.projectStatusLoading));
    next.add(project);
    this.projectStatusLoading = next;
    try {
      const res = await adminFetch(`/api/sandbox/project/${encodeURIComponent(project)}`, {
        requireToken: true,
        action: 'Loading sandbox project status',
      });
      if (!res.ok) return;
      const data = await res.json();
      const entries: SandboxProjectEntry[] = Array.isArray(data.sandboxes)
        ? data.sandboxes.map((s: Record<string, unknown>) => ({
            project: String(s.project ?? project),
            status: String(s.status ?? 'unknown'),
            image: typeof s.image === 'string' ? s.image : undefined,
            backend: typeof s.backend === 'string' ? s.backend : undefined,
            agent_id: typeof s.agent_id === 'string' ? s.agent_id : undefined,
            running: typeof s.running === 'boolean' ? s.running : undefined,
            uptime: typeof s.uptime === 'string' ? s.uptime : undefined,
            last_used: typeof s.last_used === 'string' ? s.last_used : undefined,
            error: typeof s.error === 'string' ? s.error : undefined,
          }))
        : [];
      const nextMap = new Map(this.projectStatus);
      nextMap.set(project, entries);
      this.projectStatus = nextMap;
    } catch {
      // best-effort
    } finally {
      const done = new Set(this.projectStatusLoading);
      done.delete(project);
      this.projectStatusLoading = done;
    }
  }

  async fetchAllProjectStatuses(): Promise<void> {
    // Untracked: the caller's $effect already decides *when* to refresh
    // (its own read of `projects`); reading it again here must not add a
    // second dependency on the summary rewritten by every poll tick.
    const projects = untrack(() => this.projects);
    if (projects.length === 0) return;
    await Promise.allSettled(projects.map(p => this.fetchProjectStatus(p)));
  }

  startPolling(intervalMs = 60000): void {
    this.stopPolling();
    this.fetch();
    this.fetchCapabilities();
    this.fetchPolicy();
    // Watchdog: poll on SSE-down OR on stale, so a mounted-but-quiet page
    // self-heals instead of sticking on the stale banner. The 3s exec poll
    // refreshes in-flight exec status.
    this.poller.start(intervalMs);
    this.execPoller.start(3000);

    // Subscribe to SSE events.
    this.eventUnsubs.push(
      eventStore.on('hud.sandbox', (e) => this.applySnapshot(e.data)),
      eventStore.on('hud.sandbox.event', (e) => this.pushEvent(e.data)),
    );
  }

  stopPolling(): void {
    this.poller.stop();
    this.execPoller.stop();
    for (const unsub of this.eventUnsubs) unsub();
    this.eventUnsubs = [];
  }
}

export const sandboxStore = new SandboxStore();
stalenessStore.register('sandbox', () => sandboxStore.isStale);
