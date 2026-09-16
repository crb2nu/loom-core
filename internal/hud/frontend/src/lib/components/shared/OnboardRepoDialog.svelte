<script lang="ts">
  /**
   * OnboardRepoDialog — bring an existing repo under Mills.
   *
   * GitLab repo: (1) register it at runtime with the operator — live now —
   * and (2) optionally open the gitops policy MR that admits it in Git policy
   * (demand list, issue intake, protected paths, budget caps, checksum bump)
   * — takes effect once merged and the operator restarts.
   *
   * GitHub repo: GitLab is Mills' canonical forge. Most GitHub repos in this
   * workspace are push mirrors of a GitLab project that already exists, so
   * the dialog first looks for a GitLab twin by name (registry, then GitLab
   * search) and defaults to onboarding that — importing again would mint a
   * duplicate. Only when no twin exists (or the operator says so) is the repo
   * imported into GitLab (GitHub kept as the push mirror) and then onboarded.
   * Every step lands in a ledger as it completes; a failure is shown where it
   * happened, never hidden behind a toast.
   */
  import { focusTrap } from '../../actions/focusTrap';
  import { forgeStore, type ForgeRepo } from '../../stores/forge.svelte.ts';
  import { millsProjectsStore, readinessOf, describeCode } from '../../stores/millsProjects.svelte.ts';
  import Badge from '../../widgets/Badge.svelte';
  import {
    DEFAULT_PROTECTED_PATHS,
    defaultVisibilityFor,
    importForgeProject,
    runOnboarding,
    slugify,
    twinCandidates,
    type OnboardingLedger,
    type ProjectResult,
    type StepResult,
  } from './intakeActions.ts';

  interface Props {
    open: boolean;
    onClose: () => void;
    repo: ForgeRepo | null;
    /** Called with the final registry entry (when registration succeeded). */
    onDone?: (project: string) => void;
  }
  let { open, onClose, repo, onDone }: Props = $props();

  // --- form state ---
  let note = $state('');
  let policyMR = $state(true);
  let intakeIssues = $state(false);
  let protectedPathsText = $state(DEFAULT_PROTECTED_PATHS.join('\n'));
  let maxUSD = $state(3);
  let maxRuns = $state(3);
  // GitHub import
  let group = $state('services');
  let slug = $state('');
  let visibility = $state('private');
  let mirrorBack = $state(true);
  // GitHub twin detection: existing GitLab projects with the same name.
  let twins = $state<string[]>([]);
  let twinWeb = $state<Record<string, string>>({});
  let twinsLoading = $state(false);
  let mode = $state<'twin' | 'import'>('import');
  let modeTouched = $state(false);
  let twin = $state('');

  let phase = $state<'form' | 'running' | 'done'>('form');
  let ledger = $state<OnboardingLedger>({ steps: [] });
  let importSteps = $state<StepResult[]>([]);
  let importResult = $state<ProjectResult | null>(null);
  let errorMsg = $state('');

  let seededFor = $state('');
  $effect(() => {
    const key = repo ? `${repo.provider}:${repo.path}` : '';
    if (open && repo && seededFor !== key) {
      seededFor = key;
      phase = 'form';
      ledger = { steps: [] };
      importSteps = [];
      importResult = null;
      errorMsg = '';
      note = '';
      policyMR = millsProjectsStore.registry?.policy_mr_available ?? true;
      intakeIssues = false;
      protectedPathsText = DEFAULT_PROTECTED_PATHS.join('\n');
      maxUSD = 3;
      maxRuns = 3;
      slug = slugify(repo.name || repo.path.split('/').pop() || '');
      group = 'services';
      visibility = repo.visibility === 'public' ? 'public' : defaultVisibilityFor(group);
      mirrorBack = true;
      twins = [];
      twinWeb = {};
      mode = 'import';
      modeTouched = false;
      twin = '';
      if (forgeStore.groups.length === 0 && !forgeStore.groupsLoading) void forgeStore.fetchGroups();
      if (repo.provider === 'github') void findTwins(repo, key);
    }
    if (!open) seededFor = '';
  });

  /** Registry matches show instantly; the GitLab search widens the net to
   *  projects Mills does not know yet. The default flips to "onboard the
   *  twin" only while the operator has not chosen a mode themselves. */
  async function findTwins(r: ForgeRepo, key: string): Promise<void> {
    const name = r.name || r.path.split('/').pop() || '';
    const registryPaths = millsProjectsStore.entries.map((e) => e.project);
    const apply = (found: string[]) => {
      if (seededFor !== key) return; // dialog moved on to another repo
      twins = found;
      if (found.length > 0 && !modeTouched) {
        mode = 'twin';
        if (!twin || !found.includes(twin)) twin = found[0];
      }
    };
    apply(twinCandidates(name, registryPaths, []));
    twinsLoading = true;
    try {
      const res = await forgeStore.listRepos('gitlab', name, 1);
      const web: Record<string, string> = {};
      for (const g of res.repos) web[g.path] = g.web_url;
      if (seededFor === key) twinWeb = web;
      apply(twinCandidates(name, registryPaths, res.repos));
    } catch {
      // Search is best-effort: the registry match (if any) still stands.
    } finally {
      if (seededFor === key) twinsLoading = false;
    }
  }

  function chooseMode(m: 'twin' | 'import'): void {
    mode = m;
    modeTouched = true;
  }

  let isGitHub = $derived(repo?.provider === 'github');
  let useTwin = $derived(isGitHub && mode === 'twin' && twin.length > 0);
  let targetProject = $derived(isGitHub ? (useTwin ? twin : `${group.trim()}/${slug.trim()}`) : (repo?.path ?? ''));
  let existing = $derived(repo ? millsProjectsStore.byProject(useTwin ? twin : repo.path) : undefined);
  let existingReadiness = $derived(readinessOf(existing, millsProjectsStore.registry?.home_project, millsProjectsStore.known));
  let groupAllowed = $derived.by(() => {
    const allowed = millsProjectsStore.registry?.bootstrap_allowed_groups ?? [];
    return allowed.length === 0 || allowed.includes(group.trim().split('/')[0]);
  });
  let protectedPaths = $derived(protectedPathsText.split('\n').map((s) => s.trim()).filter(Boolean));
  /** false once the registry endpoint 404s: the deployed operator predates
   *  intake, so registration cannot succeed. A GitHub import still can. */
  let operatorReady = $derived(millsProjectsStore.available !== false);
  let canSubmit = $derived(
    phase === 'form' && !!repo
      && (!isGitHub || useTwin || (/^[a-z0-9][a-z0-9._-]*$/.test(slug.trim()) && group.trim().length > 0))
      && (operatorReady || (isGitHub && !useTwin)),
  );

  function statusGlyph(s: StepResult['status']): string {
    switch (s) {
      case 'done': return '✓';
      case 'exists': return '=';
      case 'skipped': return '–';
      case 'planned': return '…';
      default: return '✕';
    }
  }

  async function submit(): Promise<void> {
    if (!canSubmit || !repo) return;
    phase = 'running';
    errorMsg = '';
    let project = repo.path;
    let webURL = repo.web_url;
    if (useTwin) {
      // The GitLab twin already exists: no import, onboard it directly.
      project = twin;
      webURL = twinWeb[twin] ?? '';
    } else if (isGitHub) {
      try {
        importResult = await importForgeProject({
          github_repo: repo.path,
          group: group.trim(),
          name: slug.trim(),
          visibility,
          mirror_back: mirrorBack,
        });
        importSteps = importResult.steps;
        project = importResult.project;
        webURL = importResult.web_url ?? '';
      } catch (e) {
        const err = e as Error & { result?: ProjectResult };
        importSteps = err.result?.steps ?? [{ step: 'gitlab_import', status: 'failed', detail: err.message }];
        errorMsg = err.message;
        phase = 'done';
        return;
      }
    }
    ledger = await runOnboarding(
      project,
      {
        note: note.trim() || undefined,
        webURL,
        policyMR,
        intakeIssues,
        protectedPaths,
        maxUSDPerRun: maxUSD > 0 ? maxUSD : undefined,
        maxRunsPerDay: maxRuns > 0 ? maxRuns : undefined,
        reason: note.trim() || undefined,
      },
      (l) => (ledger = l),
    );
    phase = 'done';
    void millsProjectsStore.fetch();
    if (ledger.entry) onDone?.(ledger.entry.project);
  }

  // Window-level, capture phase: the picker hands off to this dialog and focus
  // can sit outside both for a frame; Escape must still close this one.
  function handleWindowKeydown(event: KeyboardEvent): void {
    if (!open || event.key !== 'Escape' || phase === 'running') return;
    event.stopPropagation();
    onClose();
  }
  function handleBackdropClick(event: MouseEvent): void {
    if (phase !== 'running' && (event.target as HTMLElement)?.classList?.contains('ob-backdrop')) onClose();
  }
</script>

<svelte:window onkeydowncapture={handleWindowKeydown} />

{#if open && repo}
  <!-- svelte-ignore a11y_no_static_element_interactions, a11y_click_events_have_key_events -->
  <div class="ob-backdrop" onclick={handleBackdropClick}>
    <div class="ob-dialog" role="dialog" aria-modal="true" aria-labelledby="ob-title" use:focusTrap>
      <div class="ob-head">
        <div class="ob-title" id="ob-title">⊕ Onboard <span class="mono">{repo.path}</span> into Mills</div>
        <div class="ob-sub">
          {#if useTwin}
            GitLab is where Mills weaves, and <span class="mono">{twin}</span> already exists there — this onboards that project and leaves <span class="mono">{repo.path}</span> as its mirror. Nothing is imported.
          {:else if isGitHub}
            GitLab is where Mills weaves. This imports the GitHub repo into GitLab under
            <span class="mono">{targetProject}</span>{mirrorBack ? ' and keeps GitHub as the push mirror' : ''}, then onboards it.
          {:else}
            Registers the repo with the operator now, and can open the gitops MR that admits it in Git policy.
          {/if}
        </div>
        {#if existing}
          <div class="ob-existing">
            Mills already knows <span class="mono">{existing.project}</span>: <Badge text={existingReadiness.label} variant={existingReadiness.variant} />
            <span class="ob-dim">{existingReadiness.detail}</span>
          </div>
        {/if}
      </div>

      {#if phase === 'form'}
        {#if !operatorReady}
          <div class="ob-note warn" role="status">
            The deployed Mills operator predates intake, so registration will fail until it is upgraded.
            {#if isGitHub && !useTwin}The GitLab import itself still works; the Mills step will be reported as failed.{/if}
          </div>
        {/if}
        {#if isGitHub && (twins.length > 0 || twinsLoading)}
          <!-- Twin chooser: the workspace mirrors one GitLab project per name,
               so a same-name GitLab project is the likely canonical repo. The
               operator confirms; the dialog never imports a duplicate silently. -->
          <fieldset class="ob-twins">
            <legend class="fld-label">GitLab twin {#if twinsLoading}<span class="opt">searching GitLab…</span>{/if}</legend>
            {#if twins.length > 0}
              <label class="chk">
                <input type="radio" name="ob-mode" checked={mode === 'twin'} onchange={() => chooseMode('twin')} />
                <span>
                  Onboard the existing GitLab project
                  {#if twins.length === 1}
                    <span class="mono">{twins[0]}</span>
                  {:else}
                    <select class="inp inline" bind:value={twin} onchange={() => chooseMode('twin')} aria-label="GitLab twin">
                      {#each twins as t (t)}<option value={t}>{t}</option>{/each}
                    </select>
                  {/if}
                  <span class="ob-dim">— same repo name; GitHub stays the mirror</span>
                </span>
              </label>
              <label class="chk">
                <input type="radio" name="ob-mode" checked={mode === 'import'} onchange={() => chooseMode('import')} />
                <span>Import <span class="mono">{repo.path}</span> as a new GitLab project anyway</span>
              </label>
            {:else}
              <div class="ob-dim">No GitLab project named <span class="mono">{repo.name}</span> yet — it will be imported.</div>
            {/if}
          </fieldset>
        {/if}

        {#if isGitHub && !useTwin}
          <div class="fld-row">
            <label class="fld narrow">
              <span class="fld-label">GitLab group</span>
              {#if forgeStore.groups.length > 0}
                <select class="inp" bind:value={group} onchange={() => (visibility = defaultVisibilityFor(group))}>
                  {#each forgeStore.groups as g (g.id)}<option value={g.full_path}>{g.full_path}</option>{/each}
                </select>
              {:else}
                <input class="inp" bind:value={group} placeholder="services" />
              {/if}
            </label>
            <label class="fld">
              <span class="fld-label">Project slug</span>
              <input class="inp" bind:value={slug} />
            </label>
            <label class="fld narrow">
              <span class="fld-label">Visibility</span>
              <select class="inp" bind:value={visibility}>
                <option value="private">private</option>
                <option value="internal">internal</option>
                <option value="public">public</option>
              </select>
            </label>
          </div>
          <label class="chk">
            <input type="checkbox" bind:checked={mirrorBack} />
            <span>Keep GitHub as the push mirror (GitLab → <span class="mono">{repo.path}</span>)</span>
          </label>
          {#if !groupAllowed}
            <div class="ob-note warn">Group <code>{group}</code> is not in <code>bootstrap_allowed_groups</code>; the import still works, but the operator will not mint repos there.</div>
          {/if}
        {/if}

        <label class="fld">
          <span class="fld-label">Note <span class="opt">optional</span></span>
          <input class="inp" placeholder="why this repo joins the mills" bind:value={note} />
        </label>

        <label class="chk">
          <input type="checkbox" bind:checked={policyMR} disabled={millsProjectsStore.registry?.policy_mr_available === false} />
          <span>
            Open the gitops policy MR (Git admission — takes effect after merge + operator restart)
            {#if millsProjectsStore.registry?.policy_mr_available === false}<span class="ob-dim">— the operator has no gitops committer configured</span>{/if}
          </span>
        </label>

        {#if policyMR}
          <div class="ob-policy">
            <label class="chk">
              <input type="checkbox" bind:checked={intakeIssues} />
              <span>Also import <code>mills-eligible</code> issues from this repo</span>
            </label>
            <label class="fld">
              <span class="fld-label">Protected paths <span class="opt">one glob per line; a human reviews any run that touches these</span></span>
              <textarea class="inp ta" rows="3" bind:value={protectedPathsText}></textarea>
            </label>
            <div class="fld-row">
              <label class="fld narrow">
                <span class="fld-label">Max USD / run</span>
                <input class="inp" type="number" min="0" step="0.5" bind:value={maxUSD} />
              </label>
              <label class="fld narrow">
                <span class="fld-label">Max runs / day</span>
                <input class="inp" type="number" min="0" step="1" bind:value={maxRuns} />
              </label>
            </div>
          </div>
        {/if}

        <div class="ob-actions">
          <button class="btn-cancel" onclick={onClose}>Cancel</button>
          <button class="btn-act" onclick={submit} disabled={!canSubmit}>
            {isGitHub && !useTwin ? 'Import + onboard' : 'Onboard'}
          </button>
        </div>
        <div class="ob-note dim">Requires the HUD admin token (Labs access bar){isGitHub && !useTwin ? ' and both forge credentials' : ''}.</div>
      {:else}
        <div class="ob-ledger" aria-live="polite">
          {#if importSteps.length > 0}
            <div class="ob-ledger-title">Import</div>
            <ol class="steps">
              {#each importSteps as s (s.step)}
                <li class="step s-{s.status}"><span class="glyph">{statusGlyph(s.status)}</span><span class="step-name mono">{s.step}</span><span class="step-detail">{s.detail ?? ''}</span></li>
              {/each}
            </ol>
            {#if importResult?.github}
              <div class="ob-dim">GitHub mirror: <a href={importResult.github.web_url} target="_blank" rel="noreferrer noopener">{importResult.github.repo}</a>{importResult.github.mirror_enabled ? ' · push mirror enabled' : ''}</div>
            {/if}
          {/if}
          {#if ledger.steps.length > 0 || phase === 'running'}
            <div class="ob-ledger-title">Mills</div>
            <ol class="steps">
              {#each ledger.steps as s (s.step)}
                <li class="step s-{s.status}"><span class="glyph">{statusGlyph(s.status)}</span><span class="step-name mono">{s.step}</span><span class="step-detail">{s.detail ?? ''}</span></li>
              {/each}
              {#if phase === 'running'}<li class="step s-planned"><span class="glyph">…</span><span class="step-name mono">working</span></li>{/if}
            </ol>
          {/if}
          {#if ledger.entry}
            {@const r = readinessOf(ledger.entry, millsProjectsStore.registry?.home_project)}
            <div class="ob-result">
              <Badge text={r.label} variant={r.variant} />
              <span>{r.detail}</span>
            </div>
            {#if ledger.entry.blockers.length > 0}
              <ul class="ob-codes">{#each ledger.entry.blockers as b}<li>✕ {describeCode(b)}</li>{/each}</ul>
            {/if}
            {#if ledger.entry.pending.length > 0}
              <ul class="ob-codes pending">{#each ledger.entry.pending as p}<li>⧗ {describeCode(p)}</li>{/each}</ul>
            {/if}
          {/if}
          {#if ledger.policy?.mr_url}
            <div class="ob-mr">Policy MR: <a href={ledger.policy.mr_url} target="_blank" rel="noreferrer noopener">{ledger.policy.mr_url}</a> — {ledger.policy.edited.join(', ')}</div>
          {/if}
          {#if errorMsg}
            <div class="ob-error" role="alert">{errorMsg}</div>
          {/if}
        </div>
        <div class="ob-actions">
          <button class="btn-act" onclick={onClose} disabled={phase === 'running'}>{phase === 'running' ? 'Working…' : 'Done'}</button>
        </div>
      {/if}
    </div>
  </div>
{/if}

<style>
  .ob-backdrop { position: fixed; inset: 0; z-index: 9999; display: flex; align-items: center; justify-content: center; background: var(--scrim); backdrop-filter: blur(2px); }
  .ob-dialog { display: flex; flex-direction: column; gap: var(--space-3); width: 94%; max-width: 640px; max-height: 90vh; overflow-y: auto; padding: var(--space-4); background: var(--bg-primary); border: 1px solid var(--border); border-radius: var(--radius-lg); box-shadow: 0 8px 32px rgba(0,0,0,0.4); }
  .ob-title { font-size: var(--text-lg); font-weight: 700; color: var(--fg-primary); }
  .ob-sub { font-size: var(--text-xs); color: var(--fg-muted); margin-top: 2px; line-height: 1.5; }
  .ob-existing { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-2); margin-top: var(--space-2); font-size: var(--text-xs); color: var(--fg-secondary); }
  .mono { font-family: var(--font-mono); }
  .fld-row { display: flex; gap: var(--space-2); flex-wrap: wrap; }
  /* The 160px basis is a row measure; in the dialog's column it would become height. */
  .fld { display: flex; flex-direction: column; gap: 4px; flex: 0 0 auto; }
  .fld-row > .fld { flex: 1 1 160px; }
  .fld-row > .fld.narrow { flex: 0 1 160px; }
  .fld-label { font-size: var(--text-2xs); letter-spacing: var(--tracking-wide); text-transform: uppercase; color: var(--fg-dim); }
  .opt { text-transform: none; letter-spacing: 0; color: var(--fg-dim); }
  .inp { padding: 6px 10px; border: 1px solid var(--border); border-radius: var(--radius-sm); background: var(--bg-secondary); color: var(--fg-primary); font-size: var(--text-sm); }
  .ta { font-family: var(--font-mono); font-size: var(--text-xs); resize: vertical; }
  .chk { display: flex; align-items: flex-start; gap: var(--space-2); font-size: var(--text-xs); color: var(--fg-secondary); }
  .chk input { margin-top: 2px; }
  .ob-policy { display: flex; flex-direction: column; gap: var(--space-2); padding: var(--space-3); border: 1px solid var(--border-subtle); border-radius: var(--radius-md); background: var(--bg-secondary); }
  .ob-twins { display: flex; flex-direction: column; gap: var(--space-2); margin: 0; padding: var(--space-3); border: 1px solid color-mix(in srgb, var(--info) 35%, transparent); border-radius: var(--radius-md); background: color-mix(in srgb, var(--info) 6%, transparent); }
  .ob-twins legend { padding: 0 4px; }
  .inp.inline { display: inline-block; width: auto; padding: 2px 6px; font-size: var(--text-xs); margin: 0 4px; }
  .ob-note { font-size: var(--text-xs); color: var(--fg-muted); }
  .ob-note.warn { color: var(--warning); }
  .ob-dim { color: var(--fg-muted); }
  .ob-actions { display: flex; justify-content: flex-end; gap: var(--space-2); }
  .btn-cancel { padding: 6px 12px; border: 1px solid var(--border); border-radius: var(--radius-sm); background: transparent; color: var(--fg-secondary); cursor: pointer; }
  .btn-act { padding: 6px 14px; border: 1px solid color-mix(in srgb, var(--info) 40%, transparent); border-radius: var(--radius-sm); background: color-mix(in srgb, var(--info) 16%, transparent); color: var(--info); font-weight: 600; cursor: pointer; }
  .btn-act:disabled { opacity: 0.5; cursor: not-allowed; }
  .ob-ledger { display: flex; flex-direction: column; gap: var(--space-2); }
  .ob-ledger-title { font-size: var(--text-2xs); letter-spacing: var(--tracking-wide); text-transform: uppercase; color: var(--fg-dim); }
  .steps { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 4px; }
  .step { display: grid; grid-template-columns: 1.2rem 9rem minmax(0, 1fr); gap: var(--space-2); font-size: var(--text-xs); color: var(--fg-secondary); }
  .step-detail { overflow-wrap: anywhere; }
  .glyph { font-family: var(--font-mono); }
  .s-done .glyph { color: var(--success); }
  .s-exists .glyph, .s-skipped .glyph { color: var(--fg-muted); }
  .s-planned .glyph { color: var(--info); }
  .s-failed .glyph, .s-failed .step-detail { color: var(--error); }
  .ob-result { display: flex; align-items: center; gap: var(--space-2); font-size: var(--text-xs); color: var(--fg-secondary); }
  .ob-codes { margin: 0; padding-left: var(--space-3); font-size: var(--text-xs); color: var(--warning); list-style: none; }
  .ob-codes.pending { color: var(--info); }
  .ob-mr { font-size: var(--text-xs); color: var(--fg-secondary); overflow-wrap: anywhere; }
  .ob-error { padding: var(--space-2) var(--space-3); border: 1px solid var(--error); border-radius: var(--radius-sm); background: color-mix(in srgb, var(--error) 10%, transparent); color: var(--error); font-size: var(--text-xs); overflow-wrap: anywhere; }
  a { color: var(--info); }
</style>
