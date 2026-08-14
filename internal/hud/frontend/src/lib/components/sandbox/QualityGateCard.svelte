<script lang="ts">
  /**
   * QualityGateCard — runs and displays devbox_quality_gate (fmt → lint →
   * test) for the active project. The single most-used devbox operation in
   * the workspace's agent workflows; this is its first HUD surface. Shows an
   * overall verdict plus per-check pass/fail with duration and failure output.
   */
  import { sandboxStore } from '../../stores/sandbox.svelte.ts';
  import { formatTime } from '../../utils/format.ts';
  import { checkTone, formatExecDuration } from '../../utils/sandboxHelpers';

  interface Props {
    activeProject: string;
    hasAdminToken: boolean;
    supported: boolean;
  }
  let { activeProject, hasAdminToken, supported }: Props = $props();

  let run = $derived(sandboxStore.qualityGate);
  let running = $derived(sandboxStore.qualityGateRunning);
  let error = $derived(sandboxStore.qualityGateError);
  let stopOnFail = $state(true);

  let forActive = $derived(run && run.project === activeProject.trim() ? run : null);
  let canRun = $derived(hasAdminToken && supported && activeProject.trim().length > 0 && !running);

  function handleRun() {
    if (!canRun) return;
    sandboxStore.runQualityGate(activeProject.trim(), undefined, stopOnFail);
  }
</script>

<section class="rail-card gate-card">
  <header class="gate-head">
    <div class="section-title gate-title">Quality gate</div>
    <label class="gate-toggle" title="Stop at the first failing check (fmt → lint → test)">
      <input type="checkbox" bind:checked={stopOnFail} />
      <span>Stop on first failure</span>
    </label>
    <button
      class="gate-run"
      disabled={!canRun}
      title={!supported ? 'Quality gate unavailable on this backend'
        : !hasAdminToken ? 'Admin token required'
        : !activeProject.trim() ? 'Select an active project first'
        : 'Run fmt → lint → test in the sandbox'}
      onclick={handleRun}
    >
      {running ? 'Running…' : 'Run gate'}
    </button>
  </header>

  <p class="gate-lede">
    Runs <span class="text-mono">fmt → lint → test</span> in the project sandbox with language auto-detection.
  </p>

  {#if error}
    <div class="gate-error" role="alert">{error}</div>
  {/if}

  {#if running && !forActive}
    <div class="gate-running">
      <span class="gate-spinner" aria-hidden="true"></span>
      Provisioning sandbox and running checks — a cold image build can take a minute.
    </div>
  {:else if forActive}
    <div class="gate-verdict" data-tone={forActive.passed ? 'success' : 'error'}>
      <span class="gate-verdict-badge">{forActive.passed ? 'Passed' : 'Failed'}</span>
      <span class="gate-verdict-meta text-mono">{forActive.language}</span>
      <span class="gate-verdict-meta text-mono">{formatExecDuration(forActive.total_duration_ms)}</span>
      <span class="gate-verdict-time text-mono">{formatTime(forActive.ran_at)}</span>
    </div>

    {#if forActive.checks.length === 0}
      <div class="gate-empty">The gate returned no checks — the sandbox may still be building. Try again shortly.</div>
    {:else}
      <ul class="gate-checks">
        {#each forActive.checks as check}
          <li class="gate-check" class:failed={!check.passed}>
            <div class="gate-check-row">
              <span class="gate-check-status" data-tone={checkTone(check.passed)}>
                {check.passed ? '✓' : '✕'}
              </span>
              <span class="gate-check-name text-mono">{check.name}</span>
              {#if check.exit_code !== undefined && !check.passed}
                <span class="gate-check-exit text-mono">exit {check.exit_code}</span>
              {/if}
              <span class="gate-check-dur text-mono">{formatExecDuration(check.duration_ms)}</span>
            </div>
            {#if !check.passed && (check.output_tail || check.stderr_tail)}
              {#if check.output_tail}<pre class="gate-check-out">{check.output_tail}</pre>{/if}
              {#if check.stderr_tail && check.stderr_tail !== check.output_tail}
                <pre class="gate-check-out gate-check-err">{check.stderr_tail}</pre>
              {/if}
            {/if}
          </li>
        {/each}
      </ul>
    {/if}
  {:else}
    <div class="gate-empty">
      No gate run yet. {#if !supported}Quality gate isn't available on this backend.{:else if !hasAdminToken}Load an admin token to run it.{:else}Run it to verify the active project.{/if}
    </div>
  {/if}
</section>

<style>
  .gate-card { display: flex; flex-direction: column; }
  .gate-head {
    display: flex; align-items: center; gap: var(--space-3);
    padding: var(--space-3) 0 var(--space-2);
    border-bottom: 1px solid var(--border);
  }
  .section-title {
    font-size: var(--text-xs); font-weight: 600;
    text-transform: uppercase; letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
  }
  .gate-title { border: none; padding: 0; }
  .gate-toggle {
    display: flex; align-items: center; gap: 6px; margin-left: auto;
    font-size: var(--text-xs); color: var(--fg-muted); cursor: pointer; user-select: none;
  }
  .gate-toggle input { accent-color: var(--accent); cursor: pointer; }
  .gate-run {
    flex-shrink: 0; padding: 6px 14px; border-radius: var(--radius-sm);
    border: 1px solid color-mix(in srgb, var(--accent) 40%, var(--border));
    background: color-mix(in srgb, var(--accent) 12%, var(--bg-secondary));
    color: var(--accent); font-size: var(--text-sm); font-weight: 600; cursor: pointer;
    transition: background var(--transition-fast), border-color var(--transition-fast), box-shadow var(--transition-fast);
  }
  .gate-run:hover:not(:disabled) {
    background: color-mix(in srgb, var(--accent) 20%, var(--bg-secondary));
    box-shadow: 0 0 8px color-mix(in srgb, var(--accent) 30%, transparent);
  }
  .gate-run:disabled { opacity: 0.45; cursor: not-allowed; }
  .gate-lede { margin: var(--space-3) 0 0; font-size: var(--text-sm); color: var(--fg-muted); line-height: 1.5; }
  .gate-lede .text-mono { color: var(--fg-secondary); }
  .gate-error {
    margin-top: var(--space-3); padding: var(--space-2) var(--space-3);
    border-radius: var(--radius-sm); font-size: var(--text-sm); color: var(--error);
    border: 1px solid color-mix(in srgb, var(--error) 28%, var(--border));
    background: color-mix(in srgb, var(--error) 8%, var(--bg-secondary));
  }
  .gate-running {
    display: flex; align-items: center; gap: var(--space-2);
    margin-top: var(--space-3); font-size: var(--text-sm); color: var(--fg-secondary);
  }
  .gate-spinner {
    width: 12px; height: 12px; flex-shrink: 0; border-radius: 50%;
    border: 2px solid color-mix(in srgb, var(--accent) 25%, transparent);
    border-top-color: var(--accent); animation: gateSpin 0.8s linear infinite;
  }
  @keyframes gateSpin { to { transform: rotate(360deg); } }
  .gate-empty { margin-top: var(--space-3); font-size: var(--text-sm); color: var(--fg-dim); line-height: 1.5; }
  .gate-verdict {
    display: flex; align-items: center; gap: var(--space-3); flex-wrap: wrap;
    margin-top: var(--space-3); padding: var(--space-2) var(--space-3);
    border-radius: var(--radius-sm); border: 1px solid var(--border-subtle);
  }
  .gate-verdict[data-tone='success'] {
    border-color: color-mix(in srgb, var(--success) 30%, var(--border));
    background: color-mix(in srgb, var(--success) 8%, var(--bg-primary));
  }
  .gate-verdict[data-tone='error'] {
    border-color: color-mix(in srgb, var(--error) 30%, var(--border));
    background: color-mix(in srgb, var(--error) 8%, var(--bg-primary));
  }
  .gate-verdict-badge {
    font-size: var(--text-xs); font-weight: 700; text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }
  .gate-verdict[data-tone='success'] .gate-verdict-badge { color: var(--success); }
  .gate-verdict[data-tone='error'] .gate-verdict-badge { color: var(--error); }
  .gate-verdict-meta { font-size: var(--text-xs); color: var(--fg-secondary); }
  .gate-verdict-time { margin-left: auto; font-size: var(--text-2xs); color: var(--fg-dim); }
  .gate-checks { list-style: none; margin: var(--space-3) 0 0; padding: 0; display: flex; flex-direction: column; gap: 6px; }
  .gate-check {
    padding: var(--space-2) var(--space-3); border-radius: var(--radius-sm);
    background: var(--bg-primary); border: 1px solid var(--border-subtle);
  }
  .gate-check.failed { border-color: color-mix(in srgb, var(--error) 24%, var(--border)); }
  .gate-check-row { display: flex; align-items: center; gap: var(--space-2); }
  .gate-check-status { width: 16px; text-align: center; font-weight: 700; }
  .gate-check-status[data-tone='success'] { color: var(--success); }
  .gate-check-status[data-tone='error'] { color: var(--error); }
  .gate-check-name { font-size: var(--text-sm); color: var(--fg-primary); font-weight: 500; }
  .gate-check-exit { font-size: var(--text-2xs); color: var(--error); }
  .gate-check-dur { margin-left: auto; font-size: var(--text-2xs); color: var(--fg-dim); }
  .gate-check-out {
    margin: var(--space-2) 0 0; padding: var(--space-2);
    border-radius: var(--radius-xs);
    background: color-mix(in srgb, var(--bg-secondary) 90%, black);
    border: 1px solid var(--border-subtle); color: var(--fg-secondary);
    font-size: 11px; line-height: 1.45; overflow-x: auto; white-space: pre-wrap; word-break: break-word;
    max-height: 160px; overflow-y: auto;
  }
  .gate-check-err { border-color: color-mix(in srgb, var(--error) 22%, var(--border)); color: color-mix(in srgb, var(--error) 70%, var(--fg-primary)); }
</style>
