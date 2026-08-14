<script lang="ts">
  /**
   * SandboxLive — full sandbox workbench when the devbox backend is connected.
   *
   * Reframed around a single *active project*: pick one, see its detected
   * toolchain (devbox_detect), and drive the three real devbox operations
   * against it — Build image (devbox_build), Run command (devbox_exec_async),
   * and Quality gate (devbox_quality_gate). Results surface in the center
   * (quality gate + activity) and rail (environment, execution, summary,
   * policy). The projects list on the left is navigation into running boxes.
   */
  import { sandboxStore } from '../../stores/sandbox.svelte.ts';
  import { labsAuthStore } from '../../stores/labsAuth.svelte.ts';
  import { formatTime } from '../../utils/format.ts';
  import StatusDot from '../../widgets/StatusDot.svelte';
  import EmptyState from '../shared/EmptyState.svelte';
  import PanelHeader from '../shared/PanelHeader.svelte';
  import ErrorBanner from '../shared/ErrorBanner.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import EnvironmentCard from './EnvironmentCard.svelte';
  import QualityGateCard from './QualityGateCard.svelte';
  import { formatUptime, eventIcon, formatExecDuration, execStatusTone, languageLabel } from '../../utils/sandboxHelpers';

  let summary = $derived(sandboxStore.summary);
  let events = $derived(sandboxStore.recentEvents);
  let running = $derived(sandboxStore.runningCount);
  let paused = $derived(sandboxStore.pausedCount);
  let total = $derived(sandboxStore.totalSandboxes);
  let projects = $derived(sandboxStore.projects);
  let totalExecs = $derived(sandboxStore.totalExecs);
  let totalBuilds = $derived(sandboxStore.totalBuilds);
  let policy = $derived(sandboxStore.policy);
  let capabilities = $derived(sandboxStore.capabilities);
  let capabilitiesLoading = $derived(sandboxStore.capabilitiesLoading);
  let capabilitiesError = $derived(sandboxStore.capabilitiesError);
  let execRuns = $derived(sandboxStore.execRuns);
  let activeExecs = $derived(sandboxStore.activeExecs);
  let error = $derived(sandboxStore.error);
  let lastAction = $derived(sandboxStore.lastAction);
  let lastUpdated = $derived(sandboxStore.lastUpdated);
  let latestEvent = $derived(events[0] ?? null);
  let hasAdminToken = $derived(labsAuthStore.isAdmin);
  let projectStatus = $derived(sandboxStore.projectStatus);
  let projectStatusLoading = $derived(sandboxStore.projectStatusLoading);
  let asyncSupported = $derived(capabilities?.notes?.async_exec ?? false);
  let gateSupported = $derived(capabilities?.notes?.quality_gate ?? false);

  $effect(() => {
    if (hasAdminToken && projects.length > 0) {
      sandboxStore.fetchAllProjectStatuses();
    }
  });

  // Active project — the single subject all actions operate on.
  let activeProject = $state('');
  let execCommand = $state('');
  let execTimeout = $state('10m');
  let buildSubmitting = $state(false);
  let execSubmitting = $state(false);
  let stopConfirmProject = $state<string | null>(null);

  // Default the active project to the only known sandbox, if any.
  $effect(() => {
    if (!activeProject && projects.length >= 1) activeProject = projects[0];
  });

  // Detect the active project's environment (debounced) as it changes.
  $effect(() => {
    const project = activeProject.trim();
    if (!project) return;
    const timer = setTimeout(() => sandboxStore.fetchDetect(project), 300);
    return () => clearTimeout(timer);
  });

  let detect = $derived(sandboxStore.detect);
  let detectForActive = $derived(detect && detect.project === activeProject.trim() ? detect : null);
  let detectLoading = $derived(sandboxStore.detectLoading);
  let inlineLangs = $derived(detectForActive?.languages ?? []);

  async function handleBuild() {
    const project = activeProject.trim();
    if (!project || buildSubmitting) return;
    buildSubmitting = true;
    await sandboxStore.startSandbox(project);
    buildSubmitting = false;
  }
  async function handleRunExec() {
    const project = activeProject.trim();
    const command = execCommand.trim();
    if (!project || !command || execSubmitting) return;
    execSubmitting = true;
    await sandboxStore.startExec(project, command, execTimeout.trim() || '10m');
    if (!sandboxStore.error) execCommand = '';
    execSubmitting = false;
  }
  function handleExecKeydown(event: KeyboardEvent) {
    if (event.key === 'Enter' && (event.metaKey || event.ctrlKey)) {
      event.preventDefault();
      handleRunExec();
    }
  }
</script>

<PanelHeader title="Sandbox" icon={'⬢'} count={total}>
  {#snippet stats()}
    {#if summary?.backend}
      <span class="header-stat backend-stat">backend <span class="text-mono">{summary.backend}</span></span>
    {/if}
    <span class="header-stat running-stat"><span class="dot dot-running"></span>{running} running</span>
    {#if paused > 0}
      <span class="header-stat paused-stat"><span class="dot dot-paused"></span>{paused} paused</span>
    {/if}
    <span class="header-stat exec-stat"><span class="stat-icon">▶</span>{totalExecs} execs</span>
    <span class="header-stat build-stat"><span class="stat-icon">⚒</span>{totalBuilds} builds</span>
  {/snippet}
  {#snippet actions()}
    {#if summary?.uptime_seconds}
      <span class="header-stat uptime-stat">uptime {formatUptime(summary.uptime_seconds)}</span>
    {/if}
    {#if lastUpdated}
      <span class="header-stat updated-stat">updated {formatTime(lastUpdated)}</span>
    {/if}
  {/snippet}
</PanelHeader>

<div class="capability-strip">
  <span class="capability-chip" class:ready={capabilities?.available}>
    {capabilities?.available ? 'Devbox live' : 'Devbox offline'}
  </span>
  <span class="capability-chip" class:ready={gateSupported}>Quality gate</span>
  <span class="capability-chip" class:ready={asyncSupported}>Async exec</span>
  <span class="capability-chip" class:ready={hasAdminToken}>{hasAdminToken ? 'Admin ready' : 'Token required'}</span>
  {#if capabilitiesLoading}
    <span class="capability-meta">checking capabilities…</span>
  {:else if capabilitiesError}
    <span class="capability-meta capability-error">{capabilitiesError}</span>
  {/if}
</div>

<!-- Workbench: one active project drives every action -->
<section class="workbench">
  <div class="wb-project">
    <label class="wb-field wb-field-project">
      <span class="wb-label">Active project</span>
      <input
        class="wb-input"
        type="text"
        list="sandbox-projects"
        bind:value={activeProject}
        placeholder="services/loom-core"
        spellcheck="false"
        autocomplete="off"
      />
    </label>
    <div class="wb-env" aria-live="polite">
      {#if !activeProject.trim()}
        <span class="wb-env-hint">Pick a project to see its toolchain.</span>
      {:else if detectForActive && inlineLangs.length > 0}
        {#each inlineLangs as lang}<span class="wb-env-lang text-mono">{languageLabel(lang)}</span>{/each}
      {:else if detectForActive}
        <span class="wb-env-hint">generic image (no language marker)</span>
      {:else if detectLoading}
        <span class="wb-env-hint">detecting…</span>
      {:else}
        <span class="wb-env-hint">&nbsp;</span>
      {/if}
    </div>
  </div>

  <datalist id="sandbox-projects">
    {#each projects as project}<option value={project}></option>{/each}
  </datalist>

  <div class="wb-actions">
    <div class="wb-exec">
      <label class="wb-field wb-field-cmd">
        <span class="wb-label">Command</span>
        <input class="wb-input" type="text" bind:value={execCommand} placeholder="make test" onkeydown={handleExecKeydown} />
      </label>
      <label class="wb-field wb-field-timeout">
        <span class="wb-label">Timeout</span>
        <input class="wb-input" type="text" bind:value={execTimeout} placeholder="10m" />
      </label>
      <button
        class="wb-btn wb-btn-run"
        disabled={!hasAdminToken || !asyncSupported || !activeProject.trim() || !execCommand.trim() || execSubmitting}
        title={!hasAdminToken ? 'Admin token required' : !activeProject.trim() ? 'Select an active project' : 'Run command in the sandbox (async)'}
        onclick={handleRunExec}
      >{execSubmitting ? 'Queueing…' : 'Run command'}</button>
    </div>
    <button
      class="wb-btn wb-btn-build"
      disabled={!hasAdminToken || !activeProject.trim() || buildSubmitting}
      title={!hasAdminToken ? 'Admin token required' : !activeProject.trim() ? 'Select an active project' : 'Build (or rebuild) the sandbox image for this project'}
      onclick={handleBuild}
    >{buildSubmitting ? 'Building…' : 'Build image'}</button>
  </div>

  <div class="wb-hint">
    <span class="text-mono">Build image</span> provisions the container image · <span class="text-mono">Run command</span> uses async exec with polling — press <span class="text-mono">Cmd/Ctrl+Enter</span> to queue.
  </div>
</section>

{#if lastAction}
  <div class="action-banner">
    <span class="action-banner-kind">
      {#if lastAction.kind === 'build'}Build{:else if lastAction.kind === 'stop'}Stop{:else}Exec{/if}
    </span>
    <span class="action-banner-copy">
      {lastAction.project}: {lastAction.message}
      {#if lastAction.image}<strong>{lastAction.image}</strong>{:else if lastAction.execId}<strong>{lastAction.execId}</strong>{/if}
    </span>
  </div>
{/if}

{#if error}
  <div class="error-row">
    <ErrorBanner message={error} />
    <button class="error-dismiss" onclick={() => sandboxStore.clearError()}>Dismiss</button>
  </div>
{/if}

<div class="sandbox-content">
  <div class="projects-section">
    <div class="section-title">Projects</div>
    {#if projects.length === 0}
      <div class="projects-empty">
        <EmptyState icon={'⬢'} heading="No sandbox projects" compact />
        <div class="empty-copy">
          Build one from the workbench above. It attaches here once the daemon reports the sandbox.
        </div>
      </div>
    {:else}
      <div class="project-list">
        {#each projects as project}
          {@const entries = projectStatus.get(project) ?? []}
          {@const isLoading = projectStatusLoading.has(project)}
          <div class="project-card" class:is-active={project === activeProject.trim()}>
            <div class="project-row">
              <StatusDot status={entries.some(e => e.running) ? "healthy" : entries.length > 0 ? "idle" : "unknown"} />
              <button class="project-name text-mono" title="Make active" onclick={() => (activeProject = project)}>{project}</button>
              {#if summary?.agent_labels?.[project]}
                <span class="agent-badge text-mono">{summary.agent_labels[project]}</span>
              {/if}
              <span class="project-actions">
                <button class="action-btn action-stop" title="Stop sandbox" aria-label="Stop sandbox"
                  disabled={!hasAdminToken}
                  onclick={() => (stopConfirmProject = project)}>■</button>
              </span>
            </div>
            {#if isLoading && entries.length === 0}
              <div class="project-detail-row text-mono">loading…</div>
            {:else if entries.length > 0}
              {#each entries as entry}
                <div class="project-detail-row">
                  <span class="project-detail-status" class:is-running={entry.running}>{entry.status}</span>
                  {#if entry.backend}<span class="project-detail-meta">{entry.backend}</span>{/if}
                  {#if entry.uptime}<span class="project-detail-meta">{entry.uptime}</span>{/if}
                  {#if entry.agent_id}<span class="agent-badge text-mono">{entry.agent_id}</span>{/if}
                </div>
              {/each}
            {/if}
          </div>
        {/each}
      </div>
    {/if}
  </div>

  <div class="center-col">
    <QualityGateCard activeProject={activeProject} hasAdminToken={hasAdminToken} supported={gateSupported} />

    <div class="activity-section">
      <div class="section-title">Recent Activity</div>
      {#if events.length === 0}
        <EmptyState icon={'▶'} heading="No recent activity" compact />
      {:else}
        <div class="activity-list">
          {#each events as evt, i}
            <div class="activity-row" class:fresh={i === 0}>
              <span class="activity-icon">{eventIcon(evt.type)}</span>
              <span class="activity-type text-mono">{evt.type}</span>
              <span class="activity-project">{evt.project}</span>
              <span class="activity-detail truncate">{evt.detail}</span>
              <span class="activity-time text-mono">{formatTime(evt.timestamp)}</span>
            </div>
          {/each}
        </div>
      {/if}
    </div>
  </div>

  <aside class="sandbox-rail">
    <EnvironmentCard activeProject={activeProject} />

    <section class="rail-card">
      <div class="section-title">Execution</div>
      <div class="exec-run-summary">
        <span class="exec-run-summary-value text-mono">{activeExecs.length}</span>
        <span class="exec-run-summary-label">running now</span>
      </div>
      {#if execRuns.length === 0}
        <div class="rail-empty">Queue a command in the workbench to exercise the async devbox path and inspect its output tail here.</div>
      {:else}
        <div class="exec-run-list">
          {#each execRuns as run (run.exec_id)}
            <article class="exec-run-card" class:is-running={run.status === 'running'}>
              <div class="exec-run-head">
                <span class="exec-run-status" data-tone={execStatusTone(run.status)}>{run.status}</span>
                <span class="exec-run-project text-mono">{run.project}</span>
                {#if run.exit_code !== undefined}<span class="exec-run-exit text-mono">exit {run.exit_code}</span>{/if}
              </div>
              <div class="exec-run-command text-mono">{run.command}</div>
              <div class="exec-run-meta text-mono">
                <span>{run.exec_id}</span>
                <span>{formatExecDuration(run.status === 'running' ? run.elapsed_ms : (run.duration_ms ?? run.elapsed_ms))}</span>
              </div>
              {#if run.stdout_tail}<pre class="exec-run-tail">{run.stdout_tail}</pre>{/if}
              {#if run.stderr_tail}<pre class="exec-run-tail exec-run-tail-error">{run.stderr_tail}</pre>{/if}
              {#if run.error}<div class="exec-run-error">{run.error}</div>{/if}
            </article>
          {/each}
        </div>
      {/if}
    </section>

    <section class="rail-card">
      <div class="section-title">Sandbox Summary</div>
      <div class="summary-grid">
        <div class="summary-stat"><span class="summary-value text-mono">{projects.length}</span><span class="summary-label">Projects</span></div>
        <div class="summary-stat"><span class="summary-value text-mono">{running}</span><span class="summary-label">Running</span></div>
        <div class="summary-stat"><span class="summary-value text-mono">{totalExecs}</span><span class="summary-label">Execs</span></div>
        <div class="summary-stat"><span class="summary-value text-mono">{totalBuilds}</span><span class="summary-label">Builds</span></div>
      </div>
      {#if latestEvent}
        <div class="latest-event">
          <div class="latest-event-label">Latest event</div>
          <div class="latest-event-row">
            <span class="activity-icon">{eventIcon(latestEvent.type)}</span>
            <span class="latest-event-text"><strong>{latestEvent.project}</strong> {latestEvent.detail}</span>
          </div>
          <div class="latest-event-time text-mono">{formatTime(latestEvent.timestamp)}</div>
        </div>
      {:else}
        <div class="rail-empty">New exec and build activity will accumulate here as the daemon emits sandbox events.</div>
      {/if}
    </section>

    {#if policy?.configured}
      <section class="rail-card">
        <div class="section-title">Policy</div>
        <div class="policy-section">
          {#if policy.auto_provision}
            <div class="policy-row"><span class="policy-icon">✓</span><span class="policy-text">Auto-provision on session start</span></div>
          {/if}
          {#if policy.default_backend}
            <div class="policy-row"><span class="policy-icon">⬢</span><span class="policy-text">Backend: <span class="text-mono">{policy.default_backend}</span></span></div>
          {/if}
          {#if policy.require_sandbox?.length}
            <div class="policy-group">
              <span class="policy-group-label">Required</span>
              {#each policy.require_sandbox as cmd}<span class="policy-tag policy-tag-require">{cmd}</span>{/each}
            </div>
          {/if}
          {#if policy.recommend_sandbox?.length}
            <div class="policy-group">
              <span class="policy-group-label">Recommended</span>
              {#each policy.recommend_sandbox as cmd}<span class="policy-tag policy-tag-recommend">{cmd}</span>{/each}
            </div>
          {/if}
        </div>
      </section>
    {/if}
  </aside>
</div>

<ConfirmDialog
  open={stopConfirmProject !== null}
  title="Stop sandbox?"
  message={`This will stop the sandbox for "${stopConfirmProject ?? ''}". Running exec jobs will be terminated.`}
  confirmLabel="Stop"
  variant="danger"
  onConfirm={() => { const p = stopConfirmProject; stopConfirmProject = null; if (p) sandboxStore.stopSandbox(p); }}
  onCancel={() => (stopConfirmProject = null)}
/>

<style>
  /* --- Workbench --- */
  .workbench {
    display: flex; flex-direction: column; gap: var(--space-3);
    padding: var(--space-3) var(--space-4);
    margin-bottom: var(--space-3);
    border: 1px solid color-mix(in srgb, var(--accent) 14%, var(--border));
    border-radius: var(--radius-lg);
    background: linear-gradient(180deg, color-mix(in srgb, var(--accent) 4%, transparent), transparent), var(--bg-secondary);
  }
  .wb-project { display: flex; align-items: end; gap: var(--space-3); flex-wrap: wrap; }
  .wb-field { display: flex; flex-direction: column; gap: 6px; min-width: 0; }
  .wb-field-project { flex: 1 1 260px; }
  .wb-field-cmd { flex: 1 1 auto; }
  .wb-field-timeout { flex: 0 0 96px; }
  .wb-label {
    font-size: 10px; text-transform: uppercase; letter-spacing: 0.08em;
    color: var(--fg-muted); font-family: var(--font-mono);
  }
  .wb-input {
    width: 100%; min-width: 0; padding: 9px 11px;
    border-radius: var(--radius-sm); border: 1px solid var(--border);
    background: color-mix(in srgb, var(--bg-primary) 92%, black);
    color: var(--fg-primary); font: inherit;
    transition: border-color var(--transition-fast), box-shadow var(--transition-fast);
  }
  .wb-input:focus {
    outline: none;
    border-color: color-mix(in srgb, var(--accent) 46%, var(--border));
    box-shadow: 0 0 0 1px color-mix(in srgb, var(--accent) 24%, transparent);
  }
  .wb-env {
    display: flex; flex-wrap: wrap; align-items: center; gap: 6px;
    padding-bottom: 9px; min-height: 30px;
  }
  .wb-env-lang {
    font-size: var(--text-2xs); padding: 2px 8px; border-radius: 999px;
    background: color-mix(in srgb, var(--accent) 10%, var(--bg-primary));
    color: var(--accent); border: 1px solid color-mix(in srgb, var(--accent) 22%, var(--border));
    font-weight: 600;
  }
  .wb-env-hint { font-size: var(--text-xs); color: var(--fg-dim); }
  .wb-actions { display: flex; align-items: end; gap: var(--space-3); flex-wrap: wrap; }
  .wb-exec { display: flex; align-items: end; gap: var(--space-2); flex: 1 1 360px; min-width: 0; }
  .wb-btn {
    flex-shrink: 0; padding: 9px 16px; border-radius: var(--radius-sm);
    border: 1px solid var(--border); background: var(--bg-tertiary);
    color: var(--fg-secondary); font-size: var(--text-sm); font-weight: 600; cursor: pointer;
    transition: color var(--transition-fast), border-color var(--transition-fast), box-shadow var(--transition-fast), background var(--transition-fast);
  }
  .wb-btn:disabled { opacity: 0.45; cursor: not-allowed; }
  .wb-btn-run:hover:not(:disabled) { color: var(--info); border-color: var(--info); box-shadow: 0 0 6px var(--glow-info, color-mix(in srgb, var(--info) 40%, transparent)); }
  .wb-btn-build:hover:not(:disabled) { color: var(--success); border-color: var(--success); box-shadow: 0 0 6px var(--glow-success); }
  .wb-hint { font-size: var(--text-xs); color: var(--fg-muted); line-height: 1.5; }
  .wb-hint .text-mono { color: var(--fg-secondary); }

  /* --- Capability strip --- */
  .capability-strip { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; padding: 10px 0 12px; }
  .capability-chip {
    padding: 4px 9px; border-radius: 999px;
    border: 1px solid var(--border); background: var(--bg-secondary);
    color: var(--fg-secondary); font-size: var(--text-xs);
    font-weight: 600; text-transform: uppercase; letter-spacing: var(--tracking-wide);
  }
  .capability-chip.ready {
    border-color: color-mix(in srgb, var(--success) 28%, var(--border));
    color: var(--success);
    background: color-mix(in srgb, var(--success) 10%, var(--bg-secondary));
  }
  .capability-meta { font-size: var(--text-xs); color: var(--fg-muted); font-family: var(--font-mono); }
  .capability-error { color: var(--error); }

  /* --- Banners --- */
  /* Wraps the shared ErrorBanner so the Dismiss control (clearError is a
     real store action here) sits beside it without re-rolling the banner. */
  .error-row { display: flex; align-items: center; gap: var(--space-2); margin-bottom: 14px; }
  .error-row > :global(.error-banner) { flex: 1; min-width: 0; margin-bottom: 0; }
  .error-dismiss {
    flex-shrink: 0; padding: 4px 10px; border-radius: var(--radius-xs);
    border: 1px solid color-mix(in srgb, var(--error) 30%, var(--border));
    background: transparent; color: var(--fg-secondary);
    font-size: var(--text-xs); font-family: var(--font-mono); cursor: pointer;
    transition: color var(--transition-fast), border-color var(--transition-fast);
  }
  .error-dismiss:hover { color: var(--fg-primary); border-color: var(--fg-secondary); }
  .action-banner {
    display: flex; align-items: center; gap: var(--space-2);
    padding: 10px 12px; margin-bottom: 14px;
    border-radius: var(--radius-lg);
    border: 1px solid color-mix(in srgb, var(--success) 24%, var(--border));
    background: color-mix(in srgb, var(--success) 10%, var(--bg-secondary));
    color: var(--fg-secondary);
  }
  .action-banner-kind {
    font-size: var(--text-xs); font-family: var(--font-mono);
    text-transform: uppercase; letter-spacing: var(--tracking-wide);
    color: var(--success);
  }
  .action-banner-copy { min-width: 0; line-height: 1.5; }
  .action-banner-copy strong { margin-left: var(--space-2); font-family: var(--font-mono); color: var(--fg-primary); word-break: break-all; }

  /* --- Header (stats rendered inside the shared PanelHeader snippets) --- */
  .header-stat { display: flex; align-items: center; gap: var(--space-1); color: var(--fg-secondary); font-size: var(--text-sm); }
  .dot { width: 8px; height: 8px; border-radius: 50%; }
  .dot-running { background: var(--success); box-shadow: var(--glow-shadow-sm) var(--glow-success); }
  .dot-paused { background: var(--warning); box-shadow: var(--glow-shadow-sm) var(--glow-warning); }
  .stat-icon { font-size: var(--text-xs); color: var(--fg-muted); }
  .uptime-stat { color: var(--fg-dim); font-family: var(--font-mono); font-size: var(--text-sm); }
  .updated-stat { color: var(--fg-dim); font-family: var(--font-mono); font-size: var(--text-sm); }

  /* --- Content grid --- */
  .sandbox-content {
    display: grid;
    grid-template-columns: 220px minmax(0, 1fr) 300px;
    flex: 1; overflow: hidden; gap: var(--space-3);
  }
  .center-col { display: flex; flex-direction: column; gap: var(--space-3); min-width: 0; overflow-y: auto; }
  .section-title {
    font-size: var(--text-xs); font-weight: 600;
    text-transform: uppercase; letter-spacing: var(--tracking-wide);
    color: var(--fg-muted); padding: var(--space-3) 0 var(--space-2);
    border-bottom: 1px solid var(--border);
  }
  .projects-section { border-right: 1px solid var(--border-subtle); padding-right: var(--space-3); overflow-y: auto; }
  .project-list { display: flex; flex-direction: column; }
  .project-row {
    display: flex; align-items: center; gap: var(--space-2);
    padding: var(--space-2) var(--space-1); font-size: var(--text-sm);
    transition: background var(--transition-fast);
  }
  .project-row:hover { background: var(--bg-tertiary); }
  .project-name {
    color: var(--fg-primary); font-weight: 500; flex: 1; min-width: 0;
    overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
    background: none; border: none; padding: 0; text-align: left; cursor: pointer;
  }
  .project-name:hover { color: var(--accent); }
  .agent-badge {
    font-size: var(--text-2xs); padding: 1px var(--space-1); border-radius: var(--radius-sm);
    background: var(--accent-dim); color: var(--accent);
    border: 1px solid rgba(var(--accent-rgb), 0.2);
    flex-shrink: 0; font-weight: 600;
  }
  .project-actions { display: flex; gap: var(--space-1); flex-shrink: 0; opacity: 0; transition: opacity var(--transition-fast); }
  .project-card { border-bottom: 1px solid var(--border-subtle); padding-bottom: var(--space-1); border-left: 2px solid transparent; }
  .project-card.is-active { border-left-color: var(--accent); background: color-mix(in srgb, var(--accent) 5%, transparent); }
  .project-card:last-child { border-bottom: none; }
  .project-detail-row {
    display: flex; align-items: center; gap: var(--space-2);
    padding: 2px var(--space-1) 2px calc(var(--space-2) + 12px);
    font-size: var(--text-xs); color: var(--fg-dim); font-family: var(--font-mono);
  }
  .project-detail-status { font-weight: 600; color: var(--fg-secondary); }
  .project-detail-status.is-running { color: var(--success); }
  .project-detail-meta { color: var(--fg-dim); }
  .project-row:hover .project-actions { opacity: 1; }
  .action-btn {
    background: none; border: 1px solid var(--border); color: var(--fg-muted);
    cursor: pointer; border-radius: var(--radius-sm); font-size: var(--text-xs);
    padding: 2px var(--space-2);
    transition: color var(--transition-fast), border-color var(--transition-fast);
  }
  .action-btn:hover { color: var(--fg-primary); border-color: var(--fg-secondary); }
  .action-btn:disabled { opacity: 0.4; cursor: not-allowed; }
  .action-stop:hover:not(:disabled) { color: var(--error); border-color: var(--error); box-shadow: var(--glow-shadow-md) var(--glow-error); }
  .projects-empty { display: flex; flex-direction: column; gap: 10px; }
  .empty-copy { font-size: var(--text-sm); color: var(--fg-muted); line-height: var(--leading-relaxed); }

  /* --- Rail --- */
  .sandbox-rail { display: flex; flex-direction: column; gap: var(--space-3); overflow-y: auto; }
  .rail-card {
    background: var(--bg-secondary); border: 1px solid var(--border);
    border-radius: var(--radius-md); padding: 0 var(--space-3) var(--space-3);
    position: relative;
  }
  .rail-card::before { content: ''; position: absolute; inset: 0; border-radius: inherit; background: var(--surface-highlight); pointer-events: none; }
  .summary-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: var(--space-3); padding-top: var(--space-3); }
  .summary-stat {
    display: flex; flex-direction: column; gap: 2px; padding: var(--space-3);
    background: var(--bg-primary); border: 1px solid var(--border-subtle); border-radius: var(--radius-sm);
  }
  .summary-value { font-size: 18px; color: var(--fg-primary); }
  .summary-label { font-size: var(--text-xs); text-transform: uppercase; letter-spacing: var(--tracking-wide); color: var(--fg-dim); }
  .latest-event { margin-top: var(--space-3); padding-top: var(--space-3); border-top: 1px solid color-mix(in srgb, var(--border) 70%, transparent); }
  .latest-event-label { font-size: var(--text-xs); text-transform: uppercase; letter-spacing: var(--tracking-wide); color: var(--fg-dim); margin-bottom: var(--space-2); }
  .latest-event-row { display: flex; gap: var(--space-2); align-items: flex-start; }
  .latest-event-text { font-size: var(--text-sm); color: var(--fg-secondary); line-height: var(--leading-normal); }
  .latest-event-text strong { color: var(--fg-primary); }
  .latest-event-time { margin-top: var(--space-2); font-size: var(--text-xs); color: var(--fg-dim); font-family: var(--font-mono); }
  .rail-empty { padding-top: var(--space-3); font-size: var(--text-sm); color: var(--fg-dim); line-height: var(--leading-normal); }
  .exec-run-summary { display: flex; align-items: baseline; gap: 8px; padding-top: var(--space-3); }
  .exec-run-summary-value { font-size: 20px; color: var(--fg-primary); }
  .exec-run-summary-label { font-size: var(--text-xs); text-transform: uppercase; letter-spacing: var(--tracking-wide); color: var(--fg-dim); }
  .exec-run-list { display: flex; flex-direction: column; gap: var(--space-3); padding-top: var(--space-3); }
  .exec-run-card {
    display: flex; flex-direction: column; gap: 8px;
    padding: var(--space-3); border-radius: var(--radius-md);
    border: 1px solid var(--border-subtle); background: var(--bg-primary);
  }
  .exec-run-card.is-running {
    border-color: color-mix(in srgb, var(--info) 28%, var(--border));
    box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--info) 16%, transparent);
  }
  .exec-run-head, .exec-run-meta { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
  .exec-run-status {
    padding: 2px 6px; border-radius: 999px;
    font-size: var(--text-2xs); font-weight: 700;
    text-transform: uppercase; letter-spacing: var(--tracking-wide);
    border: 1px solid var(--border); color: var(--fg-secondary);
  }
  .exec-run-status[data-tone='info'] { color: var(--info); border-color: color-mix(in srgb, var(--info) 28%, var(--border)); background: color-mix(in srgb, var(--info) 10%, var(--bg-primary)); }
  .exec-run-status[data-tone='success'] { color: var(--success); border-color: color-mix(in srgb, var(--success) 28%, var(--border)); background: color-mix(in srgb, var(--success) 10%, var(--bg-primary)); }
  .exec-run-status[data-tone='error'] { color: var(--error); border-color: color-mix(in srgb, var(--error) 28%, var(--border)); background: color-mix(in srgb, var(--error) 10%, var(--bg-primary)); }
  .exec-run-project, .exec-run-exit { font-size: var(--text-xs); color: var(--fg-muted); }
  .exec-run-command { font-size: var(--text-sm); color: var(--fg-primary); word-break: break-word; }
  .exec-run-meta { justify-content: space-between; font-size: var(--text-2xs); color: var(--fg-dim); }
  .exec-run-tail {
    margin: 0; padding: 10px; border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--bg-secondary) 90%, black);
    border: 1px solid var(--border-subtle); color: var(--fg-secondary);
    font-size: 11px; line-height: 1.45; overflow-x: auto; white-space: pre-wrap; word-break: break-word;
  }
  .exec-run-tail-error { border-color: color-mix(in srgb, var(--error) 22%, var(--border)); color: color-mix(in srgb, var(--error) 70%, var(--fg-primary)); }
  .exec-run-error { font-size: var(--text-xs); color: var(--error); line-height: 1.5; }

  /* --- Policy --- */
  .policy-section { display: flex; flex-direction: column; gap: var(--space-2); padding: var(--space-2) 0; }
  .policy-row { display: flex; align-items: center; gap: var(--space-2); font-size: var(--text-sm); color: var(--fg-secondary); }
  .policy-icon { font-size: var(--text-sm); color: var(--fg-muted); width: 14px; text-align: center; flex-shrink: 0; }
  .policy-group { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-1); padding: 2px 0; }
  .policy-group-label { font-size: var(--text-2xs); font-weight: 600; text-transform: uppercase; letter-spacing: var(--tracking-wide); color: var(--fg-muted); width: 100%; margin-top: 2px; }
  .policy-tag { font-size: var(--text-xs); font-family: var(--font-mono); padding: 1px var(--space-1); border-radius: var(--radius-sm); }
  .policy-tag-require { background: var(--error-dim); color: var(--error); border: 1px solid rgba(var(--error-rgb), 0.2); }
  .policy-tag-recommend { background: var(--warning-dim); color: var(--warning); border: 1px solid rgba(var(--warning-rgb), 0.25); }

  /* --- Activity --- */
  .activity-section {
    min-width: 0; overflow-y: auto;
    background: var(--bg-secondary); border: 1px solid var(--border);
    border-radius: var(--radius-md); padding: 0 var(--space-3) var(--space-3);
  }
  .activity-list { display: flex; flex-direction: column; }
  .activity-row {
    display: flex; align-items: center; gap: var(--space-2);
    padding: var(--space-1) var(--space-1); font-size: var(--text-sm);
    border-bottom: 1px solid var(--border-subtle);
    transition: background var(--transition-fast);
  }
  .activity-row:hover { background: var(--bg-tertiary); }
  .activity-row.fresh { animation: freshPulse 0.5s ease-out; }
  @keyframes freshPulse { from { background: var(--info-dim); } to { background: transparent; } }
  .activity-icon { font-size: var(--text-sm); color: var(--accent); flex-shrink: 0; width: var(--space-4); text-align: center; }
  .activity-type { font-size: var(--text-xs); color: var(--fg-secondary); width: 70px; flex-shrink: 0; }
  .activity-project { color: var(--fg-primary); font-weight: 500; flex-shrink: 0; max-width: 120px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .activity-detail { flex: 1; color: var(--fg-dim); min-width: 0; }
  .activity-time { font-size: var(--text-xs); color: var(--fg-dim); flex-shrink: 0; }
  .truncate { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

  @media (max-width: 1220px) {
    .sandbox-content { grid-template-columns: 200px minmax(0, 1fr); }
    .sandbox-rail {
      grid-column: 1 / -1;
      display: grid; grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
      overflow: visible;
    }
  }
  @media (max-width: 600px) {
    .sandbox-content { grid-template-columns: 1fr; }
    .sandbox-rail { grid-template-columns: 1fr; }
    .projects-section { border-right: none; border-bottom: 1px solid var(--border); padding-right: 0; padding-bottom: var(--space-2); max-height: 200px; }
    .wb-exec { flex-basis: 100%; flex-wrap: wrap; }
    .wb-field-cmd { flex-basis: 100%; }
    .wb-field-timeout { flex: 1 1 80px; }
    .wb-btn { flex: 1 1 auto; }
  }
</style>
