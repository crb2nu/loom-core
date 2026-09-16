<script lang="ts">
  /**
   * NewProjectDialog — create a project the workspace way: a GitLab project
   * under a chosen group (the canonical repo), seeded with README, a
   * self-contained green .gitlab-ci.yml, AGENTS.md, ROADMAP.md, CHANGELOG.md
   * and a .loom/ note; a GitHub repo as its push mirror (default on); and
   * onboarded into Mills in the same motion (default on). Preview runs the
   * daemon's dry run and shows the exact step plan before anything mutates.
   */
  import { focusTrap } from '../../actions/focusTrap';
  import { forgeStore } from '../../stores/forge.svelte.ts';
  import { millsProjectsStore, readinessOf } from '../../stores/millsProjects.svelte.ts';
  import Badge from '../../widgets/Badge.svelte';
  import {
    DEFAULT_PROTECTED_PATHS,
    createForgeProject,
    defaultVisibilityFor,
    runOnboarding,
    slugify,
    type OnboardingLedger,
    type ProjectResult,
    type StepResult,
  } from './intakeActions.ts';

  interface Props {
    open: boolean;
    onClose: () => void;
    onCreated?: (project: string) => void;
  }
  let { open, onClose, onCreated }: Props = $props();

  let name = $state('');
  let group = $state('services');
  let description = $state('');
  let visibility = $state('private');
  let visibilityTouched = $state(false);
  let template = $state('generic');
  let mirror = $state(true);
  let onboard = $state(true);
  let policyMR = $state(true);

  let phase = $state<'form' | 'preview' | 'running' | 'done'>('form');
  let plan = $state<ProjectResult | null>(null);
  let result = $state<ProjectResult | null>(null);
  let ledger = $state<OnboardingLedger>({ steps: [] });
  let errorMsg = $state('');

  let wasOpen = $state(false);
  $effect(() => {
    if (open && !wasOpen) {
      wasOpen = true;
      phase = 'form';
      plan = null;
      result = null;
      ledger = { steps: [] };
      errorMsg = '';
      name = '';
      description = '';
      template = 'generic';
      mirror = forgeStore.github?.configured ?? true;
      // An operator that predates intake 404s the register call; default the
      // step off rather than promise a registration that cannot happen.
      onboard = millsProjectsStore.available !== false;
      policyMR = millsProjectsStore.registry?.policy_mr_available ?? true;
      visibilityTouched = false;
      visibility = defaultVisibilityFor(group);
      if (forgeStore.groups.length === 0 && !forgeStore.groupsLoading) void forgeStore.fetchGroups();
      if (!forgeStore.auth && !forgeStore.authLoading) void forgeStore.fetchAuth();
    }
    if (!open) wasOpen = false;
  });

  let slug = $derived(slugify(name));
  let fullPath = $derived(`${group.trim()}/${slug}`);
  let slugValid = $derived(/^[a-z0-9][a-z0-9._-]*$/.test(slug));
  let githubOwner = $derived(forgeStore.github?.user ?? '');
  let githubOK = $derived(forgeStore.github?.configured ?? false);
  let gitlabOK = $derived(forgeStore.gitlab?.configured ?? false);
  let groupAllowed = $derived.by(() => {
    const allowed = millsProjectsStore.registry?.bootstrap_allowed_groups ?? [];
    return allowed.length === 0 || allowed.includes(group.trim().split('/')[0]);
  });
  let canSubmit = $derived(slugValid && group.trim().length > 0 && gitlabOK && (!mirror || githubOK) && phase !== 'running');

  function onGroupChange(): void {
    if (!visibilityTouched) visibility = defaultVisibilityFor(group);
  }

  function body(dryRun: boolean) {
    return {
      name: slug,
      group: group.trim(),
      description: description.trim() || undefined,
      visibility,
      template,
      mirror_to_github: mirror,
      github_visibility: visibility === 'public' ? 'public' : 'private',
      dry_run: dryRun,
    };
  }

  async function preview(): Promise<void> {
    if (!canSubmit) return;
    errorMsg = '';
    try {
      plan = await createForgeProject(body(true));
      phase = 'preview';
    } catch (e) {
      const err = e as Error & { result?: ProjectResult };
      plan = err.result ?? null;
      errorMsg = err.message;
    }
  }

  async function create(): Promise<void> {
    if (!canSubmit) return;
    phase = 'running';
    errorMsg = '';
    try {
      result = await createForgeProject(body(false));
    } catch (e) {
      const err = e as Error & { result?: ProjectResult };
      result = err.result ?? null;
      errorMsg = err.message;
      phase = 'done';
      return;
    }
    if (onboard) {
      ledger = await runOnboarding(
        result.project,
        {
          note: description.trim() ? `created from the HUD — ${description.trim()}` : 'created from the HUD',
          webURL: result.web_url,
          policyMR,
          protectedPaths: DEFAULT_PROTECTED_PATHS,
          maxUSDPerRun: 3,
          maxRunsPerDay: 3,
        },
        (l) => (ledger = l),
      );
    }
    phase = 'done';
    void millsProjectsStore.fetch();
    onCreated?.(result.project);
  }

  function statusGlyph(s: StepResult['status']): string {
    switch (s) {
      case 'done': return '✓';
      case 'exists': return '=';
      case 'skipped': return '–';
      case 'planned': return '…';
      default: return '✕';
    }
  }
  // Window-level, capture phase: focus can sit outside the dialog for a frame
  // after a hand-off between dialogs, and Escape must still close this one.
  function handleWindowKeydown(event: KeyboardEvent): void {
    if (!open || event.key !== 'Escape' || phase === 'running') return;
    event.stopPropagation();
    onClose();
  }
  function handleBackdropClick(event: MouseEvent): void {
    if (phase !== 'running' && (event.target as HTMLElement)?.classList?.contains('np-backdrop')) onClose();
  }
</script>

<svelte:window onkeydowncapture={handleWindowKeydown} />

{#if open}
  <!-- svelte-ignore a11y_no_static_element_interactions, a11y_click_events_have_key_events -->
  <div class="np-backdrop" onclick={handleBackdropClick}>
    <div class="np-dialog" role="dialog" aria-modal="true" aria-labelledby="np-title" use:focusTrap>
      <div class="np-head">
        <div class="np-title" id="np-title">＋ New project</div>
        <div class="np-sub">GitLab is the canonical repo; GitHub is its push mirror. Seeded with README, a green <code>.gitlab-ci.yml</code>, AGENTS.md, ROADMAP.md, CHANGELOG.md and <code>.loom/</code>.</div>
      </div>

      {#if phase === 'form' || phase === 'preview'}
        <div class="fld-row">
          <label class="fld">
            <span class="fld-label">Name</span>
            <input class="inp" placeholder="my-service" bind:value={name} disabled={phase === 'preview'} />
          </label>
          <label class="fld narrow">
            <span class="fld-label">Group</span>
            {#if forgeStore.groups.length > 0}
              <select class="inp" bind:value={group} onchange={onGroupChange} disabled={phase === 'preview'}>
                {#each forgeStore.groups as g (g.id)}<option value={g.full_path}>{g.full_path}</option>{/each}
              </select>
            {:else}
              <input class="inp" bind:value={group} oninput={onGroupChange} placeholder="services" disabled={phase === 'preview'} />
            {/if}
          </label>
          <label class="fld narrow">
            <span class="fld-label">Visibility</span>
            <select class="inp" bind:value={visibility} onchange={() => (visibilityTouched = true)} disabled={phase === 'preview'}>
              <option value="private">private</option>
              <option value="internal">internal</option>
              <option value="public">public</option>
            </select>
          </label>
        </div>
        <label class="fld">
          <span class="fld-label">Description <span class="opt">optional</span></span>
          <input class="inp" placeholder="one line — lands in the README and the GitLab description" bind:value={description} disabled={phase === 'preview'} />
        </label>
        <div class="fld-row">
          <label class="fld narrow">
            <span class="fld-label">Template</span>
            <select class="inp" bind:value={template} disabled={phase === 'preview'}>
              <option value="generic">generic</option>
              <option value="go">go (module + cmd + Makefile)</option>
            </select>
          </label>
          <div class="fld">
            <span class="fld-label">Path</span>
            <span class="np-path mono" class:bad={!slugValid && name.trim().length > 0}>{slugValid ? fullPath : 'name must be a lowercase slug'}</span>
          </div>
        </div>

        <label class="chk">
          <input type="checkbox" bind:checked={mirror} disabled={phase === 'preview'} />
          <span>
            Mirror to GitHub as <span class="mono">{githubOwner || '<owner>'}/{slug || '<name>'}</span> ({visibility === 'public' ? 'public' : 'private'})
            {#if mirror && !githubOK}<span class="np-warn">— GitHub is not connected (see the forge bar)</span>{/if}
          </span>
        </label>
        <label class="chk">
          <input type="checkbox" bind:checked={onboard} disabled={phase === 'preview'} />
          <span>
            Onboard into Mills (register at runtime; default protected paths + 3 USD / 3 runs per day caps)
            {#if millsProjectsStore.available === false}<span class="np-warn">— the deployed operator predates intake; registration would fail</span>{/if}
          </span>
        </label>
        {#if onboard}
          <label class="chk sub">
            <input type="checkbox" bind:checked={policyMR} disabled={phase === 'preview' || millsProjectsStore.registry?.policy_mr_available === false} />
            <span>Open the gitops policy MR too</span>
          </label>
        {/if}
        {#if !gitlabOK}
          <div class="np-note warn">GitLab is not connected where loomd runs — nothing can be created until it is.</div>
        {:else if !groupAllowed}
          <div class="np-note">Group <code>{group}</code> is outside <code>bootstrap_allowed_groups</code>; creating there is fine, the operator just won't mint repos there itself.</div>
        {/if}

        {#if plan}
          <div class="np-plan">
            <div class="np-ledger-title">Plan{plan.dry_run ? ' (dry run — nothing created)' : ''}</div>
            <ol class="steps">
              {#each plan.steps as s (s.step)}
                <li class="step s-{s.status}"><span class="glyph">{statusGlyph(s.status)}</span><span class="step-name mono">{s.step}</span><span class="step-detail">{s.detail ?? ''}</span></li>
              {/each}
            </ol>
          </div>
        {/if}
        {#if errorMsg}<div class="np-error" role="alert">{errorMsg}</div>{/if}

        <div class="np-actions">
          <button class="btn-cancel" onclick={onClose}>Cancel</button>
          {#if phase === 'form'}
            <button class="btn-ghost" onclick={preview} disabled={!canSubmit}>Preview</button>
          {:else}
            <button class="btn-ghost" onclick={() => { phase = 'form'; plan = null; }}>Edit</button>
          {/if}
          <button class="btn-act" onclick={create} disabled={!canSubmit}>Create</button>
        </div>
        <div class="np-note dim">Requires the HUD admin token (Labs access bar) and the forge credentials above.</div>
      {:else}
        <div class="np-ledger" aria-live="polite">
          <div class="np-ledger-title">Forge</div>
          <ol class="steps">
            {#each result?.steps ?? [] as s (s.step)}
              <li class="step s-{s.status}"><span class="glyph">{statusGlyph(s.status)}</span><span class="step-name mono">{s.step}</span><span class="step-detail">{s.detail ?? ''}</span></li>
            {/each}
            {#if phase === 'running' && !result}<li class="step s-planned"><span class="glyph">…</span><span class="step-name mono">creating</span></li>{/if}
          </ol>
          {#if result?.web_url}
            <div class="np-links">
              <a href={result.web_url} target="_blank" rel="noreferrer noopener">{result.project} on GitLab</a>
              {#if result.github?.web_url}· <a href={result.github.web_url} target="_blank" rel="noreferrer noopener">{result.github.repo} on GitHub</a>{/if}
            </div>
          {/if}
          {#if onboard && result && !errorMsg}
            <div class="np-ledger-title">Mills</div>
            <ol class="steps">
              {#each ledger.steps as s (s.step)}
                <li class="step s-{s.status}"><span class="glyph">{statusGlyph(s.status)}</span><span class="step-name mono">{s.step}</span><span class="step-detail">{s.detail ?? ''}</span></li>
              {/each}
              {#if phase === 'running'}<li class="step s-planned"><span class="glyph">…</span><span class="step-name mono">working</span></li>{/if}
            </ol>
            {#if ledger.entry}
              {@const r = readinessOf(ledger.entry, millsProjectsStore.registry?.home_project)}
              <div class="np-result"><Badge text={r.label} variant={r.variant} /><span>{r.detail}</span></div>
            {/if}
            {#if ledger.policy?.mr_url}
              <div class="np-links">Policy MR: <a href={ledger.policy.mr_url} target="_blank" rel="noreferrer noopener">{ledger.policy.mr_url}</a></div>
            {/if}
          {/if}
          {#if errorMsg}<div class="np-error" role="alert">{errorMsg}</div>{/if}
        </div>
        <div class="np-actions">
          <button class="btn-act" onclick={onClose} disabled={phase === 'running'}>{phase === 'running' ? 'Working…' : 'Done'}</button>
        </div>
      {/if}
    </div>
  </div>
{/if}

<style>
  .np-backdrop { position: fixed; inset: 0; z-index: 9999; display: flex; align-items: center; justify-content: center; background: var(--scrim); backdrop-filter: blur(2px); }
  .np-dialog { display: flex; flex-direction: column; gap: var(--space-3); width: 94%; max-width: 640px; max-height: 90vh; overflow-y: auto; padding: var(--space-4); background: var(--bg-primary); border: 1px solid var(--border); border-radius: var(--radius-lg); box-shadow: 0 8px 32px rgba(0,0,0,0.4); }
  .np-title { font-size: var(--text-lg); font-weight: 700; color: var(--fg-primary); }
  .np-sub { font-size: var(--text-xs); color: var(--fg-muted); margin-top: 2px; line-height: 1.5; }
  .mono { font-family: var(--font-mono); }
  .fld-row { display: flex; gap: var(--space-2); flex-wrap: wrap; }
  /* The 160px basis is a row measure; in the dialog's column it would become height. */
  .fld { display: flex; flex-direction: column; gap: 4px; flex: 0 0 auto; }
  .fld-row > .fld { flex: 1 1 160px; }
  .fld-row > .fld.narrow { flex: 0 1 170px; }
  .fld-label { font-size: var(--text-2xs); letter-spacing: var(--tracking-wide); text-transform: uppercase; color: var(--fg-dim); }
  .opt { text-transform: none; letter-spacing: 0; color: var(--fg-dim); }
  .inp { padding: 6px 10px; border: 1px solid var(--border); border-radius: var(--radius-sm); background: var(--bg-secondary); color: var(--fg-primary); font-size: var(--text-sm); }
  .np-path { padding: 6px 0; font-size: var(--text-sm); color: var(--fg-primary); }
  .np-path.bad { color: var(--warning); }
  .chk { display: flex; align-items: flex-start; gap: var(--space-2); font-size: var(--text-xs); color: var(--fg-secondary); flex-wrap: wrap; }
  .chk.sub { margin-left: var(--space-4); }
  .chk input { margin-top: 2px; }
  .np-warn { color: var(--warning); }
  .np-note { font-size: var(--text-xs); color: var(--fg-muted); }
  .np-note.warn { color: var(--warning); }
  .np-plan { padding: var(--space-3); border: 1px dashed var(--border-subtle); border-radius: var(--radius-md); background: var(--bg-secondary); }
  .np-ledger { display: flex; flex-direction: column; gap: var(--space-2); }
  .np-ledger-title { font-size: var(--text-2xs); letter-spacing: var(--tracking-wide); text-transform: uppercase; color: var(--fg-dim); margin-bottom: 4px; }
  .steps { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 4px; }
  .step { display: grid; grid-template-columns: 1.2rem 9rem minmax(0, 1fr); gap: var(--space-2); font-size: var(--text-xs); color: var(--fg-secondary); }
  .step-detail { overflow-wrap: anywhere; }
  .glyph { font-family: var(--font-mono); }
  .s-done .glyph { color: var(--success); }
  .s-exists .glyph, .s-skipped .glyph { color: var(--fg-muted); }
  .s-planned .glyph { color: var(--info); }
  .s-failed .glyph, .s-failed .step-detail { color: var(--error); }
  .np-result { display: flex; align-items: center; gap: var(--space-2); font-size: var(--text-xs); color: var(--fg-secondary); }
  .np-links { font-size: var(--text-xs); color: var(--fg-secondary); overflow-wrap: anywhere; }
  .np-error { padding: var(--space-2) var(--space-3); border: 1px solid var(--error); border-radius: var(--radius-sm); background: color-mix(in srgb, var(--error) 10%, transparent); color: var(--error); font-size: var(--text-xs); overflow-wrap: anywhere; }
  .np-actions { display: flex; justify-content: flex-end; gap: var(--space-2); }
  .btn-cancel, .btn-ghost { padding: 6px 12px; border: 1px solid var(--border); border-radius: var(--radius-sm); background: transparent; color: var(--fg-secondary); cursor: pointer; }
  .btn-ghost:disabled, .btn-act:disabled { opacity: 0.5; cursor: not-allowed; }
  .btn-act { padding: 6px 14px; border: 1px solid color-mix(in srgb, var(--info) 40%, transparent); border-radius: var(--radius-sm); background: color-mix(in srgb, var(--info) 16%, transparent); color: var(--info); font-weight: 600; cursor: pointer; }
  a { color: var(--info); }
</style>
