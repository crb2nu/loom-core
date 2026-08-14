<script lang="ts">
  /**
   * SpinPlanDialog — the Mills "Spinning Room" (Live Beam slice 3 / F2). The
   * operator picks a model frame, hands it a brief (roving), and the operator
   * spins a draft plan + slices into the agent-context Plan Store (phase=draft,
   * with a warp-beam priority) for review before the beam picks it up.
   *
   * Self-contained: it fetches the policy-allowed frames from the operator
   * (GET /api/mills/spinning-room/frames) on open and fires the spin
   * ASYNCHRONOUSLY (POST /api/mills/spin/async) — the admin bearer is attached by
   * the HUD proxy so the browser never sees it. The async endpoint returns 202 +
   * a spin_id immediately (a frontier frame like claude-opus-4-8 runs minutes,
   * past the client-facing proxy timeout, so the request can't be held open); the
   * dialog closes at once and hands the spin_id to the caller via onQueued. The
   * caller registers that id with the shared spinRuns store, which polls the
   * LIST endpoint (GET /api/mills/spin/runs?limit=) — one request covering every
   * in-flight spin — and surfaces the draft when the id turns up terminal.
   *
   * Competitive spinning (the deferred F2 item): picking 2+ frames spins the
   * same roving on each concurrently — one draft plan per frame, each recording
   * its competitors — so the operator compares siblings and advances the winner.
   * A single-frame pick keeps the legacy {frame} request shape, so the dialog
   * still works against an operator image that predates `frames[]`.
   */
  import { focusTrap } from '../../actions/focusTrap';
  import { toastStore } from '../../stores/toasts.svelte.ts';
  import { submitAsyncSpin } from './spinActions.ts';
  import { patternsStore, type PatternInfo } from '../../stores/patterns.svelte.ts';
  import {
    buildMaterials,
    greenCount,
    materialInputKind,
    materialPlaceholder,
    patternPickerGroups,
    type RawMaterialValues,
  } from '../../utils/spinningRoomHelpers.ts';

  interface Frame {
    name: string;
    model: string;
    backend: string;
  }

  /**
   * Optional pre-fill for a "respin": seed the brief + scope from an existing
   * plan or slice so the operator can redo a sparse/older plan on a chosen
   * frame. label re-titles the dialog head (e.g. "Respin plan: …"). Null/absent
   * = a fresh spin with empty fields.
   */
  interface SpinSeed {
    brief?: string;
    project?: string;
    namespace?: string;
    priority?: string;
    label?: string;
    /** Source plan_id this respin redoes — recorded on the fresh draft so the
     * board can link them and offer a one-click supersede. */
    respunFrom?: string;
    /** Pre-select these frames (e.g. a Retry re-uses the failed spin's frames).
     * Filtered to policy-allowed frames on load; ignored when none are valid. */
    frames?: string[];
  }

  interface Props {
    open: boolean;
    onClose: () => void;
    /**
     * Called once the async spin is ACCEPTED (202), with the spin id + the
     * frames it runs on — NOT when the draft plan is ready. The caller tracks
     * the id through the spinRuns store, which polls the list endpoint
     * (GET /api/mills/spin/runs?limit=) for the terminal status + plan ids.
     */
    onQueued?: (spin: { spinId: string; frames: string[] }) => void;
    seed?: SpinSeed | null;
  }

  let { open, onClose, onQueued, seed = null }: Props = $props();

  const PRIORITIES = ['P0', 'P1', 'P2', 'P3'];
  // Mirrors the operator-side cap (spin.maxCompetitiveFrames): each frame is a
  // live model synthesis, so an uncapped pick multiplies spend.
  const MAX_FRAMES = 3;

  // Frame catalog (policy) + room state, fetched on open.
  let frames = $state<Frame[]>([]);
  let enabled = $state(true);
  let available = $state(true);
  let defaultPriority = $state('P2');
  let loadingFrames = $state(false);
  let framesError = $state('');

  // Spin form. selectedFrames drives competitive spinning: one pick = the
  // classic single-frame spin; 2–3 picks = the same roving on each frame.
  let brief = $state('');
  let selectedFrames = $state<string[]>([]);
  let priority = $state('');
  let project = $state('');
  let namespace = $state('');
  let busy = $state(false);

  // Mode: 'free' = frame(s) + brief (the classic spin); 'pattern' = pick a
  // catalog card and stamp it with materials (the J1 pattern-first front
  // door). A respin seed always means free mode — it redoes a free-form plan.
  let mode = $state<'free' | 'pattern'>('free');

  // Pattern mode state. The catalog is fetched locally on open (like frames)
  // so this dialog never disturbs the Patterns panel's shared status filter;
  // the stamp ACTION goes through patternsStore so its stamping/stampError
  // state stays the single source of truth.
  let catalog = $state<PatternInfo[]>([]);
  let catalogError = $state('');
  let loadingCatalog = $state(false);
  let selectedPatternId = $state('');
  let materialValues = $state<RawMaterialValues>({});
  let materialErrors = $state<string[]>([]);

  let picker = $derived(patternPickerGroups(catalog));
  let selectedPattern = $derived(catalog.find((p) => p.id === selectedPatternId) ?? null);

  async function loadCatalog() {
    loadingCatalog = true;
    catalogError = '';
    try {
      // All statuses: candidates render (disabled) so the operator sees what
      // exists and what still needs a kill-test/promote before it stamps.
      const res = await fetch('/api/patterns?status=all', { cache: 'no-store' });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      catalog = data.patterns ?? [];
    } catch (e) {
      catalogError = e instanceof Error ? e.message : String(e);
    } finally {
      loadingCatalog = false;
    }
  }

  // matText narrows the string|boolean union for template value= bindings
  // (an indexed access doesn't narrow across two reads in markup).
  function matText(name: string): string {
    const v = materialValues[name];
    return typeof v === 'string' ? v : '';
  }

  function pickPattern(p: PatternInfo) {
    if (p.status !== 'approved') return;
    selectedPatternId = p.id;
    // Fresh form per card: checkbox fields start explicit-false, the rest
    // empty (empty optionals are omitted so the stamp applies defaults).
    const next: RawMaterialValues = {};
    for (const f of p.materials_schema ?? []) {
      if (f.type === 'bool') next[f.name] = false;
    }
    materialValues = next;
    materialErrors = [];
  }

  async function submitStamp(enqueue: boolean) {
    const p = selectedPattern;
    if (!p) return;
    const { materials, errors } = buildMaterials(p.materials_schema, materialValues);
    materialErrors = errors;
    if (errors.length > 0) return;
    busy = true;
    try {
      const result = await patternsStore.stamp(p.id, materials, project.trim(), { enqueue });
      if (!result) {
        toastStore.error(`Stamp failed: ${patternsStore.stampError ?? 'unknown error'}`);
        return;
      }
      toastStore.success(
        result.enqueued
          ? `Stamped ${result.plan_id} (${result.slice_count} slices) and queued ${result.backlog_id} to the beam`
          : `Stamped ${result.plan_id} (${result.slice_count} slices) — advance it to planned to feed the beam`
      );
      onClose();
    } finally {
      busy = false;
    }
  }

  function toggleFrame(name: string) {
    if (selectedFrames.includes(name)) {
      selectedFrames = selectedFrames.filter((f) => f !== name);
    } else if (selectedFrames.length < MAX_FRAMES) {
      selectedFrames = [...selectedFrames, name];
    }
  }

  async function loadFrames() {
    loadingFrames = true;
    framesError = '';
    try {
      // no-store: the frame list is live policy — never serve a cached (possibly
      // empty/stale) response, or the picker shows "— none —" after a policy edit.
      const res = await fetch('/api/mills/spinning-room/frames', { cache: 'no-store' });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      enabled = data.enabled !== false;
      available = data.available !== false;
      defaultPriority = data.default_priority || 'P2';
      frames = data.frames ?? [];
      // A seed (e.g. Retry) may request specific frames — honour the valid ones
      // first. Otherwise drop picks the hot-reloaded policy no longer allows and
      // seed from the first frame when nothing valid remains.
      const seeded = (seed?.frames ?? []).filter((name) => frames.some((f) => f.name === name));
      if (seeded.length > 0) {
        selectedFrames = seeded;
      } else {
        selectedFrames = selectedFrames.filter((name) => frames.some((f) => f.name === name));
        if (selectedFrames.length === 0 && frames.length > 0) {
          selectedFrames = [frames[0].name];
        }
      }
    } catch (e) {
      framesError = e instanceof Error ? e.message : String(e);
    } finally {
      loadingFrames = false;
    }
  }

  // Re-fetch frames each time the dialog opens (policy can hot-reload between
  // opens) and, for a respin, seed the form from the plan/slice ONCE per open.
  // `openedOnce` is a plain latch (not reactive) so a re-render can't re-seed
  // and clobber the operator's edits while the dialog stays open.
  let openedOnce = false;
  $effect(() => {
    if (open && !openedOnce) {
      openedOnce = true;
      void loadFrames();
      void loadCatalog();
      mode = 'free'; // a respin seed is always a free spin; fresh opens reset too
      selectedPatternId = '';
      materialErrors = [];
      brief = seed?.brief ?? '';
      project = seed?.project ?? '';
      namespace = seed?.namespace ?? '';
      priority = seed?.priority ?? '';
    }
    if (!open) openedOnce = false;
  });

  async function submit() {
    const b = brief.trim();
    if (!b) {
      toastStore.error('Brief is required');
      return;
    }
    if (selectedFrames.length === 0) {
      toastStore.error('Pick at least one frame');
      return;
    }
    busy = true;
    const spinFrames = [...selectedFrames];
    try {
      // Async-always: a frame's latency isn't knowable up front (a frontier
      // frame runs minutes, past the proxy timeout), so every spin fires and
      // forgets — 202 + spin_id now, the draft lands in the background. The
      // request body is the SAME shape as the sync /spin: one frame keeps the
      // {frame} form, 2+ switches to {frames} (competitive).
      const scope = {
        priority,
        project: project.trim(),
        namespace: namespace.trim(),
        // On a respin, link the fresh draft back to the plan it redoes.
        ...(seed?.respunFrom ? { respun_from: seed.respunFrom } : {}),
      };
      const body =
        spinFrames.length === 1
          ? { brief: b, frame: spinFrames[0], ...scope }
          : { brief: b, frames: spinFrames, ...scope };
      // submitAsyncSpin routes through adminFetch (Labs access-bar token) —
      // spinning is admin-gated at the HUD, so a bare fetch 401s "invalid admin
      // token". See spinActions.ts.
      const spinId = await submitAsyncSpin(body);
      toastStore.success(
        `Spin queued on ${spinFrames.join(' + ')} — tracking${spinId ? ` (${spinId})` : ''}…`
      );
      brief = '';
      priority = '';
      project = '';
      namespace = '';
      onQueued?.({ spinId, frames: spinFrames });
      onClose();
    } catch (e) {
      toastStore.error(`Spin failed: ${e instanceof Error ? e.message : e}`);
    } finally {
      busy = false;
    }
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape' && !busy) onClose();
  }
  function handleBackdropClick(event: MouseEvent): void {
    if (!busy && (event.target as HTMLElement)?.classList?.contains('spin-backdrop')) onClose();
  }

  let pickedFrames = $derived(frames.filter((f) => selectedFrames.includes(f.name)));
  let canSpin = $derived(
    enabled && available && selectedFrames.length > 0 && brief.trim().length > 0 && !busy
  );
</script>

{#if open}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="spin-backdrop" onkeydown={handleKeydown} onclick={handleBackdropClick}>
    <div
      class="spin-dialog"
      role="dialog"
      aria-modal="true"
      aria-labelledby="spin-title"
      use:focusTrap
    >
      <div class="spin-head">
        <div class="spin-title" id="spin-title">⟳ {seed?.label ?? 'Spinning Room'}</div>
        <div class="spin-sub">
          {#if seed}
            Redoing an existing plan — review the seeded brief, pick a frame, and it spins a fresh draft to compare.
          {:else if mode === 'free'}
            Pick a model frame and hand it a brief — it spins a draft plan into the beam for review.
          {:else}
            Pick a catalog card and fill its materials — the stamp expands the instruction book into a plan deterministically.
          {/if}
        </div>
      </div>

      {#if !seed}
        <div class="mode-tabs" role="tablist" aria-label="Spin mode">
          <button
            class="mode-tab"
            class:active={mode === 'free'}
            role="tab"
            aria-selected={mode === 'free'}
            onclick={() => (mode = 'free')}
            disabled={busy}
          >Free spin</button>
          <button
            class="mode-tab"
            class:active={mode === 'pattern'}
            role="tab"
            aria-selected={mode === 'pattern'}
            onclick={() => (mode = 'pattern')}
            disabled={busy}
          >From pattern{#if picker.approved.length > 0}&nbsp;· {picker.approved.length}{/if}</button>
        </div>
      {/if}

      {#if mode === 'pattern' && !seed}
        {#if catalogError}
          <div class="spin-note err">Couldn't load the pattern catalog: {catalogError}</div>
        {:else if loadingCatalog && catalog.length === 0}
          <div class="spin-note">Loading the catalog…</div>
        {:else if catalog.length === 0}
          <div class="spin-note">No patterns in the catalog yet.</div>
        {/if}

        {#if catalog.length > 0}
          <div class="fld">
            <span class="fld-label">Pattern cards</span>
            <div class="card-picks" role="group" aria-label="Pattern cards">
              {#each picker.approved as p (p.id)}
                <button
                  class="card-pick"
                  class:picked={selectedPatternId === p.id}
                  title="{p.makes} · v{p.version}{greenCount(p) > 0 ? ` · ${greenCount(p)} shipped green` : ''}"
                  onclick={() => pickPattern(p)}
                  disabled={busy}
                >
                  <span class="card-name">{p.name}</span>
                  <span class="card-makes">{p.makes}</span>
                  {#if greenCount(p) > 0}<span class="card-green">✓{greenCount(p)}</span>{/if}
                </button>
              {/each}
              {#each picker.candidates as p (p.id)}
                <button
                  class="card-pick candidate"
                  title="candidate — needs a kill-test or a promote before it can stamp"
                  disabled
                >
                  <span class="card-name">{p.name}</span>
                  <span class="card-badge">candidate</span>
                </button>
              {/each}
            </div>
          </div>
        {/if}

        {#if selectedPattern}
          <div class="pattern-desc">{selectedPattern.description ?? selectedPattern.makes}</div>
          {#each selectedPattern.materials_schema ?? [] as f (f.name)}
            <label class="fld">
              <span class="fld-label">
                {f.name}
                {#if !f.required}<span class="opt">optional</span>{/if}
                {#if f.description}<span class="opt"> — {f.description}</span>{/if}
              </span>
              {#if materialInputKind(f) === 'checkbox'}
                <input
                  type="checkbox"
                  class="mat-check"
                  checked={materialValues[f.name] === true}
                  onchange={(e) => (materialValues[f.name] = (e.currentTarget as HTMLInputElement).checked)}
                  disabled={busy}
                />
              {:else if materialInputKind(f) === 'select'}
                <select
                  class="inp"
                  value={matText(f.name)}
                  onchange={(e) => (materialValues[f.name] = (e.currentTarget as HTMLSelectElement).value)}
                  disabled={busy}
                >
                  <option value="">{f.default ? `default (${f.default})` : '— pick —'}</option>
                  {#each f.enum ?? [] as opt}<option value={opt}>{opt}</option>{/each}
                </select>
              {:else if materialInputKind(f) === 'json'}
                <textarea
                  class="inp ta"
                  rows="3"
                  placeholder={materialPlaceholder(f) || (f.type === 'list' ? '[…]' : '{…}')}
                  value={matText(f.name)}
                  oninput={(e) => (materialValues[f.name] = (e.currentTarget as HTMLTextAreaElement).value)}
                  disabled={busy}
                ></textarea>
              {:else}
                <input
                  class="inp"
                  type={materialInputKind(f) === 'number' ? 'number' : 'text'}
                  placeholder={materialPlaceholder(f)}
                  value={matText(f.name)}
                  oninput={(e) => (materialValues[f.name] = (e.currentTarget as HTMLInputElement).value)}
                  disabled={busy}
                />
              {/if}
            </label>
          {/each}

          {#if materialErrors.length > 0}
            <div class="spin-note err">{materialErrors.join(' · ')}</div>
          {/if}

          <div class="spin-actions">
            <button class="btn-cancel" onclick={onClose} disabled={busy}>Cancel</button>
            <button class="btn-cancel" onclick={() => void submitStamp(false)} disabled={busy}>
              {busy ? 'Stamping…' : 'Stamp draft plan'}
            </button>
            <button
              class="btn-act"
              onclick={() => void submitStamp(true)}
              disabled={busy}
              title="Stamps and immediately queues a Mills backlog item (admin)"
            >
              {busy ? 'Stamping…' : 'Stamp + queue to beam'}
            </button>
          </div>
          <div class="spin-note dim">
            Stamping is deterministic — no model call. The card's pins and slice
            template expand with your materials; green merges feed the card's
            taste gate automatically.
          </div>
        {:else if catalog.length > 0}
          <div class="spin-note dim">Pick an approved card to fill its materials.</div>
        {/if}
      {/if}

      {#if mode === 'free' || seed}
      {#if framesError}
        <div class="spin-note err">Couldn't load frames: {framesError}</div>
      {:else if !enabled}
        <div class="spin-note">The Spinning Room is disabled in policy (<code>spinning_room.enabled</code>).</div>
      {:else if !available}
        <div class="spin-note">Spinning Room enabled, but the operator can't reach it right now (MCP hub down).</div>
      {:else if frames.length === 0 && !loadingFrames}
        <div class="spin-note">No frames configured. Add entries under <code>spinning_room.frames</code> in policy.</div>
      {/if}

      <label class="fld">
        <span class="fld-label">Brief (roving)</span>
        <textarea
          class="inp ta"
          rows="4"
          placeholder="What should this plan accomplish? e.g. Harden the GitLab importer against 5xx with retries + tests."
          bind:value={brief}
          disabled={busy}
        ></textarea>
      </label>

      <div class="fld-row">
        <div class="fld">
          <span class="fld-label">
            Frames
            <span class="opt">pick 2+ to spin competitively (max {MAX_FRAMES})</span>
          </span>
          <div class="frame-picks" role="group" aria-label="Spinning frames">
            {#if !frames.length}
              <span class="frame-none">— none —</span>
            {/if}
            {#each frames as f}
              <label
                class="frame-pick"
                class:picked={selectedFrames.includes(f.name)}
                title="{f.model} ({f.backend || 'flexinfer'})"
              >
                <input
                  type="checkbox"
                  checked={selectedFrames.includes(f.name)}
                  onchange={() => toggleFrame(f.name)}
                  disabled={busy ||
                    (!selectedFrames.includes(f.name) && selectedFrames.length >= MAX_FRAMES)}
                />
                <span>{f.name} · {f.model}</span>
              </label>
            {/each}
          </div>
        </div>
        <label class="fld narrow">
          <span class="fld-label">Priority</span>
          <select
            class="inp"
            bind:value={priority}
            disabled={busy}
            title="Warp-beam priority; empty uses the policy default"
          >
            <option value="">default ({defaultPriority})</option>
            {#each PRIORITIES as p}<option value={p}>{p}</option>{/each}
          </select>
        </label>
      </div>

      <div class="fld-row">
        <label class="fld">
          <span class="fld-label">Project <span class="opt">optional</span></span>
          <input class="inp" placeholder="services/loom-core" bind:value={project} disabled={busy} />
        </label>
        <label class="fld">
          <span class="fld-label">Namespace <span class="opt">optional</span></span>
          <input class="inp" placeholder="mills/spun" bind:value={namespace} disabled={busy} />
        </label>
      </div>

      {#if pickedFrames.length === 1}
        <div class="frame-hint">
          Spinning on <strong>{pickedFrames[0].model}</strong>
          ({pickedFrames[0].backend || 'flexinfer'}). The draft lands <strong>phase=draft</strong> —
          advance it to <em>planned</em> to feed the beam.
        </div>
      {:else if pickedFrames.length > 1}
        <div class="frame-hint">
          Competitive spin: the same roving goes to
          <strong>{pickedFrames.map((f) => f.model).join(' vs ')}</strong> — one draft per frame,
          each labeled with its competitors. Keep the better yarn (advance it to
          <em>planned</em>) and leave the rest in <strong>draft</strong>.
        </div>
      {/if}

      <div class="spin-actions">
        <button class="btn-cancel" onclick={onClose} disabled={busy}>Cancel</button>
        <button class="btn-act" onclick={submit} disabled={!canSpin}>
          {busy
            ? 'Queuing…'
            : selectedFrames.length > 1
              ? `Spin ${selectedFrames.length} competing drafts`
              : 'Spin draft'}
        </button>
      </div>
      <div class="spin-note dim">
        Spins run in the background — the draft appears on the board when it lands
        (a frontier frame can take a few minutes).
      </div>
      {/if}
    </div>
  </div>
{/if}

<style>
  .spin-backdrop {
    position: fixed;
    inset: 0;
    z-index: 9999;
    display: flex;
    align-items: center;
    justify-content: center;
    background: var(--scrim);
    backdrop-filter: blur(2px);
  }

  .spin-dialog {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    max-width: 520px;
    width: 94%;
    padding: var(--space-4);
    background: var(--bg-primary);
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
    box-shadow: 0 8px 32px rgba(0, 0, 0, 0.4);
  }

  .spin-head {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .spin-title {
    font-size: var(--text-base);
    font-weight: 600;
    color: var(--fg-primary);
  }
  .spin-sub {
    font-size: var(--text-sm);
    color: var(--fg-secondary);
    line-height: 1.4;
  }

  .spin-note {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    background: var(--bg-tertiary);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
  }
  .spin-note.err {
    color: var(--status-error);
    border-color: color-mix(in srgb, var(--status-error) 40%, var(--border));
  }
  .spin-note.dim {
    background: none;
    border: none;
    padding: 0;
    font-style: italic;
  }
  .spin-note code {
    font-family: var(--font-mono);
  }

  .fld {
    display: flex;
    flex-direction: column;
    gap: 3px;
    flex: 1;
    min-width: 0;
  }
  .fld.narrow {
    flex: 0 0 160px;
  }
  .fld-row {
    display: flex;
    gap: var(--space-2);
  }
  .fld-label {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    font-weight: 600;
  }
  .fld-label .opt {
    font-weight: 400;
    opacity: 0.7;
  }

  .inp {
    background: var(--bg-tertiary);
    border: 1px solid var(--border);
    color: var(--fg-primary);
    border-radius: var(--radius-sm);
    padding: 6px 8px;
    font-size: var(--text-sm);
    font-family: var(--font-mono);
    width: 100%;
  }
  .inp:focus {
    outline: none;
    border-color: var(--border-active);
  }
  .inp.ta {
    resize: vertical;
    min-height: 60px;
    line-height: 1.45;
  }
  .inp:disabled {
    opacity: 0.6;
  }

  .frame-picks {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
  }
  .frame-pick {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    padding: 4px 8px;
    background: var(--bg-tertiary);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    color: var(--fg-secondary);
    cursor: pointer;
    transition: border-color var(--transition-fast), color var(--transition-fast);
  }
  .frame-pick.picked {
    border-color: var(--border-focus);
    color: var(--accent);
  }
  .frame-pick:has(input:disabled) {
    opacity: 0.55;
    cursor: default;
  }
  .frame-pick input {
    margin: 0;
    accent-color: var(--accent);
  }
  .frame-none {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    font-family: var(--font-mono);
    padding: 4px 0;
  }

  .frame-hint {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    line-height: 1.5;
  }
  .frame-hint strong {
    color: var(--fg-primary);
  }

  .mode-tabs {
    display: flex;
    gap: var(--space-1);
    border-bottom: 1px solid var(--border-subtle);
    padding-bottom: var(--space-1);
  }
  .mode-tab {
    background: transparent;
    border: 1px solid transparent;
    border-radius: var(--radius-xs);
    color: var(--fg-secondary);
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    padding: 3px 10px;
    cursor: pointer;
    transition: color var(--transition-fast), border-color var(--transition-fast);
  }
  .mode-tab.active {
    color: var(--accent);
    border-color: var(--border-focus);
  }
  .mode-tab:hover:not(.active):not(:disabled) {
    color: var(--fg-primary);
  }

  .card-picks {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    max-height: 200px;
    overflow-y: auto;
  }
  .card-pick {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
    padding: 5px 8px;
    background: var(--bg-tertiary);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    cursor: pointer;
    text-align: left;
    transition: border-color var(--transition-fast), color var(--transition-fast);
  }
  .card-pick.picked {
    border-color: var(--border-focus);
    color: var(--fg-primary);
  }
  .card-pick.candidate,
  .card-pick:disabled {
    opacity: 0.55;
    cursor: default;
  }
  .card-name {
    font-weight: 600;
    font-family: var(--font-mono);
    white-space: nowrap;
  }
  .card-makes {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .card-green {
    color: var(--success);
    font-family: var(--font-mono);
  }
  .card-badge {
    font-family: var(--font-mono);
    color: var(--warning);
    border: 1px solid color-mix(in srgb, var(--warning) 40%, transparent);
    border-radius: var(--radius-xs);
    padding: 0 5px;
  }

  .pattern-desc {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    line-height: 1.5;
    background: var(--bg-tertiary);
    border-left: 2px solid var(--border-focus);
    border-radius: var(--radius-xs);
    padding: var(--space-1) var(--space-2);
  }

  .mat-check {
    align-self: flex-start;
    accent-color: var(--accent);
    margin: 2px 0;
  }

  .spin-actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
    margin-top: var(--space-1);
  }

  .btn-act,
  .btn-cancel {
    padding: var(--space-1) var(--space-3);
    border-radius: var(--radius-xs);
    font-size: var(--text-sm);
    font-family: var(--font-mono);
    cursor: pointer;
    white-space: nowrap;
    transition: background var(--transition-fast), border-color var(--transition-fast);
  }
  .btn-act {
    background: transparent;
    border: 1px solid var(--border-focus);
    color: var(--accent);
  }
  .btn-act:hover:not(:disabled) {
    background: var(--accent-dim);
  }
  .btn-act:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .btn-cancel {
    background: transparent;
    border: 1px solid var(--border);
    color: var(--fg-secondary);
  }
  .btn-cancel:hover:not(:disabled) {
    border-color: var(--border-active);
    color: var(--fg-primary);
  }
  .btn-cancel:disabled {
    opacity: 0.5;
    cursor: default;
  }
</style>
