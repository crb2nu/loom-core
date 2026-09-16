<script lang="ts">
  /**
   * AlertRuleDialog — create/edit one alert rule for the whole-set-replace
   * PUT. The backend performs NO validation (an unknown condition type is
   * accepted and then silently never evaluated; a 5 typed where minutes were
   * meant becomes 5 nanoseconds), so every guard lives here:
   *  - condition type is a closed select over what the engine implements;
   *  - durations are entered as "30s / 5m / 2h / 1d" and parsed to ns;
   *  - the id is immutable while editing — stuck-alert resolution keys off
   *    rule identity, and renaming a rule with live alerts strands them.
   * The dialog only BUILDS the rule; the panel owns the store round-trip.
   */
  import {
    KNOWN_RULE_CONDITION_TYPES,
    formatGoDuration,
    parseGoDuration,
    type AlertRule,
  } from '../stores/alerts.svelte.ts';
  import { focusTrap } from '../actions/focusTrap';

  function handleKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') onCancel();
  }

  function handleBackdropClick(event: MouseEvent): void {
    if ((event.target as HTMLElement)?.classList?.contains('scrim')) onCancel();
  }

  let {
    open,
    mode,
    seed,
    saving,
    onSave,
    onCancel,
  }: {
    open: boolean;
    mode: 'create' | 'edit';
    seed: AlertRule | null;
    saving: boolean;
    onSave: (rule: AlertRule) => void;
    onCancel: () => void;
  } = $props();

  let id = $state('');
  let name = $state('');
  let severity = $state('warning');
  let conditionType = $state<string>(KNOWN_RULE_CONDITION_TYPES[0]);
  let threshold = $state(0);
  let durationText = $state('');
  let projectsText = $state('');
  let cooldownText = $state('5m');
  let enabled = $state(true);
  let validationError = $state<string | null>(null);

  // Re-seed the form whenever the dialog opens on a different rule. Tracked
  // on (open, seed) only; the field writes are the point.
  let seededFor = $state<string | null>(null);
  $effect(() => {
    if (!open) {
      seededFor = null;
      return;
    }
    const key = `${mode}:${seed?.id ?? ''}`;
    if (seededFor === key) return;
    seededFor = key;
    validationError = null;
    id = seed?.id ?? '';
    name = seed?.name ?? '';
    severity = seed?.severity ?? 'warning';
    conditionType = seed?.condition?.type ?? KNOWN_RULE_CONDITION_TYPES[0];
    threshold = seed?.condition?.threshold ?? 0;
    durationText = seed?.condition?.duration ? formatGoDuration(seed.condition.duration) : '';
    projectsText = (seed?.condition?.projects ?? []).join(', ');
    cooldownText = seed?.cooldown ? formatGoDuration(seed.cooldown) : '5m';
    enabled = seed?.enabled ?? true;
  });

  function submit(): void {
    const trimmedID = id.trim();
    if (!/^[a-z0-9][a-z0-9-_]*$/.test(trimmedID)) {
      validationError = 'id must be a lowercase slug (letters, digits, dashes)';
      return;
    }
    if (!name.trim()) {
      validationError = 'name is required';
      return;
    }
    const cooldown = parseGoDuration(cooldownText);
    if (cooldown === null) {
      validationError = 'cooldown must look like 30s, 5m, 2h, or 1d';
      return;
    }
    const duration = durationText.trim() === '' ? 0 : parseGoDuration(durationText);
    if (duration === null) {
      validationError = 'duration must look like 30s, 5m, 2h, or 1d (or be empty)';
      return;
    }
    if (!Number.isInteger(threshold) || threshold < 0) {
      validationError = 'threshold must be a non-negative integer';
      return;
    }
    validationError = null;
    const projects = projectsText
      .split(',')
      .map((value) => value.trim())
      .filter((value) => value.length > 0);
    onSave({
      id: trimmedID,
      name: name.trim(),
      enabled,
      severity,
      cooldown,
      // Preserved verbatim by the panel's fresh-copy merge; the zero time is
      // only the create-path seed ("never fired").
      last_fired: seed?.last_fired ?? '0001-01-01T00:00:00Z',
      condition: {
        type: conditionType,
        threshold,
        ...(duration ? { duration } : {}),
        ...(projects.length > 0 ? { projects } : {}),
      },
    });
  }
</script>

{#if open}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="scrim" onkeydown={handleKeydown} onclick={handleBackdropClick}>
    <div
      class="dialog"
      role="dialog"
      aria-modal="true"
      aria-label={mode === 'create' ? 'Add alert rule' : `Edit rule ${id}`}
      use:focusTrap
    >
      <h3>{mode === 'create' ? 'Add alert rule' : 'Edit alert rule'}</h3>

      <label class="field">
        <span>id</span>
        <input class="mono" bind:value={id} disabled={mode === 'edit'} placeholder="pipeline-failed"
          title={mode === 'edit' ? 'Rule identity is immutable — stuck-alert resolution keys off it' : ''} />
      </label>
      <label class="field">
        <span>name</span>
        <input bind:value={name} placeholder="Pipeline Failed" />
      </label>
      <div class="row">
        <label class="field">
          <span>severity</span>
          <select bind:value={severity}>
            <option value="critical">critical</option>
            <option value="warning">warning</option>
            <option value="info">info</option>
          </select>
        </label>
        <label class="field">
          <span>condition</span>
          <select bind:value={conditionType}>
            {#each KNOWN_RULE_CONDITION_TYPES as t}
              <option value={t}>{t}</option>
            {/each}
          </select>
        </label>
      </div>
      <div class="row">
        <label class="field">
          <span>threshold</span>
          <input type="number" min="0" step="1" bind:value={threshold} />
        </label>
        <label class="field">
          <span>duration <span class="dim">(stuck window)</span></span>
          <input class="mono" bind:value={durationText} placeholder="30m" />
        </label>
        <label class="field">
          <span>cooldown</span>
          <input class="mono" bind:value={cooldownText} placeholder="5m" />
        </label>
      </div>
      <label class="field">
        <span>projects <span class="dim">(comma-separated, empty = all)</span></span>
        <input class="mono" bind:value={projectsText} placeholder="services/loom-core, platform/gitops" />
      </label>
      <label class="check">
        <input type="checkbox" bind:checked={enabled} />
        <span>enabled</span>
      </label>

      {#if validationError}
        <p class="validation" role="alert">{validationError}</p>
      {/if}

      <div class="actions">
        <button type="button" class="btn" onclick={onCancel} disabled={saving}>Cancel</button>
        <button type="button" class="btn btn-primary" onclick={submit} disabled={saving}>
          {saving ? 'Saving…' : mode === 'create' ? 'Add rule' : 'Save rule'}
        </button>
      </div>
    </div>
  </div>
{/if}

<style>
  .scrim {
    position: fixed;
    inset: 0;
    z-index: 60;
    background: rgba(0, 0, 0, 0.55);
    display: flex;
    align-items: center;
    justify-content: center;
    padding: var(--space-4);
  }
  .dialog {
    width: min(30rem, 100%);
    max-height: 90vh;
    overflow-y: auto;
    background: var(--bg-elevated);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    padding: var(--space-4);
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  h3 { margin: 0; font-size: var(--text-sm); }
  .row { display: flex; gap: var(--space-3); }
  .row .field { flex: 1 1 0; min-width: 0; }
  .field { display: flex; flex-direction: column; gap: 0.25rem; font-size: var(--text-xs); }
  .field > span { color: var(--text-muted); }
  .field input, .field select {
    background: var(--bg-subtle);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    color: var(--fg-primary);
    padding: 0.35rem 0.5rem;
    font-size: var(--text-xs);
  }
  .field input:disabled { opacity: 0.6; cursor: not-allowed; }
  .check { display: flex; align-items: center; gap: 0.4rem; font-size: var(--text-xs); }
  .dim { color: var(--text-muted); }
  .validation { margin: 0; color: var(--error); font-size: var(--text-xs); }
  .actions { display: flex; justify-content: flex-end; gap: var(--space-2); }
  .btn {
    background: var(--bg-subtle);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    color: var(--fg-secondary);
    padding: 0.35rem 0.8rem;
    font-size: var(--text-xs);
    cursor: pointer;
  }
  .btn:hover { color: var(--fg-primary); }
  .btn:disabled { opacity: 0.6; cursor: default; }
  .btn-primary {
    background: color-mix(in srgb, var(--accent) 18%, transparent);
    border-color: color-mix(in srgb, var(--accent) 45%, var(--border-default));
    color: var(--accent);
  }
</style>
