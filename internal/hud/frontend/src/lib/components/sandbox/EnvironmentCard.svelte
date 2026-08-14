<script lang="ts">
  /**
   * EnvironmentCard — shows the devbox_detect fingerprint for the active
   * project: the languages, tools, system deps, and image hash that determine
   * what the sandbox container is built from. This is the "representative"
   * surface — it tells the operator exactly what toolchain a build/exec/gate
   * will run against, before anything is provisioned.
   */
  import { sandboxStore } from '../../stores/sandbox.svelte.ts';
  import { languageLabel, shortHash } from '../../utils/sandboxHelpers';

  interface Props {
    activeProject: string;
  }
  let { activeProject }: Props = $props();

  let detect = $derived(sandboxStore.detect);
  let loading = $derived(sandboxStore.detectLoading);
  let error = $derived(sandboxStore.detectError);
  // Only trust the cached detect when it matches the current active project.
  let forActive = $derived(detect && detect.project === activeProject.trim() ? detect : null);
  let languages = $derived(forActive?.languages ?? []);
  let systemDeps = $derived(forActive?.system_deps ?? []);
  let hasProject = $derived(activeProject.trim().length > 0);
</script>

<section class="rail-card env-card">
  <div class="section-title">Environment</div>

  {#if !hasProject}
    <div class="env-empty">Pick an active project to preview the toolchain baked into its sandbox image.</div>
  {:else if loading && !forActive}
    <div class="env-empty">Detecting environment for <span class="text-mono">{activeProject}</span>…</div>
  {:else if error && !forActive}
    <div class="env-error">Couldn't detect environment: {error}</div>
  {:else if forActive}
    {#if languages.length > 0}
      <div class="env-langs">
        {#each languages as lang}
          <div class="env-lang">
            <span class="env-lang-name text-mono">{languageLabel(lang)}</span>
            {#if lang.dep_manager}<span class="env-lang-dep">{lang.dep_manager}</span>{/if}
            {#if lang.tools?.length}
              <span class="env-lang-tools">
                {#each lang.tools as tool}<span class="env-tool text-mono">{tool}</span>{/each}
              </span>
            {/if}
          </div>
        {/each}
      </div>
    {:else}
      <div class="env-empty">No language runtime detected — the sandbox falls back to a generic image.</div>
    {/if}

    {#if systemDeps.length > 0}
      <div class="env-deps">
        <span class="env-deps-label">System deps</span>
        {#each systemDeps as dep}<span class="env-dep text-mono">{dep}</span>{/each}
      </div>
    {/if}

    {#if forActive.hash}
      <div class="env-hash">
        <span class="env-hash-label">Image fingerprint</span>
        <code class="text-mono">{shortHash(forActive.hash)}</code>
      </div>
    {/if}
  {:else}
    <div class="env-empty">Environment preview loads when a project is selected.</div>
  {/if}
</section>

<style>
  .env-card { display: flex; flex-direction: column; }
  .section-title {
    font-size: var(--text-xs); font-weight: 600;
    text-transform: uppercase; letter-spacing: var(--tracking-wide);
    color: var(--fg-muted); padding: var(--space-3) 0 var(--space-2);
    border-bottom: 1px solid var(--border);
  }
  .env-empty { padding-top: var(--space-3); font-size: var(--text-sm); color: var(--fg-dim); line-height: var(--leading-normal); }
  .env-empty .text-mono { color: var(--fg-secondary); }
  .env-error { padding-top: var(--space-3); font-size: var(--text-sm); color: var(--error); line-height: 1.5; }
  .env-langs { display: flex; flex-direction: column; gap: var(--space-2); padding-top: var(--space-3); }
  .env-lang {
    display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    background: var(--bg-primary); border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm);
  }
  .env-lang-name {
    font-size: var(--text-sm); font-weight: 600; color: var(--accent);
  }
  .env-lang-dep {
    font-size: var(--text-2xs); color: var(--fg-dim);
    text-transform: uppercase; letter-spacing: var(--tracking-wide);
  }
  .env-lang-tools { display: flex; flex-wrap: wrap; gap: 4px; margin-left: auto; }
  .env-tool {
    font-size: var(--text-2xs); padding: 1px 6px; border-radius: var(--radius-sm);
    background: var(--bg-tertiary); color: var(--fg-secondary); border: 1px solid var(--border-subtle);
  }
  .env-deps { display: flex; flex-wrap: wrap; align-items: center; gap: 4px; padding-top: var(--space-3); }
  .env-deps-label {
    width: 100%; font-size: var(--text-2xs); font-weight: 600;
    text-transform: uppercase; letter-spacing: var(--tracking-wide);
    color: var(--fg-muted); margin-bottom: 2px;
  }
  .env-dep {
    font-size: var(--text-2xs); padding: 1px 6px; border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--info) 10%, var(--bg-primary));
    color: var(--info); border: 1px solid color-mix(in srgb, var(--info) 22%, var(--border));
  }
  .env-hash {
    display: flex; align-items: center; justify-content: space-between;
    gap: var(--space-2); margin-top: var(--space-3); padding-top: var(--space-3);
    border-top: 1px solid color-mix(in srgb, var(--border) 70%, transparent);
  }
  .env-hash-label { font-size: var(--text-2xs); text-transform: uppercase; letter-spacing: var(--tracking-wide); color: var(--fg-dim); }
  .env-hash code { font-size: var(--text-xs); color: var(--fg-secondary); }
</style>
