<script lang="ts">
  /**
   * PatternMaterialsFields — THE materials form for stamping a pattern,
   * shared by the Pattern Loom page and the Spin-a-plan dialog. The two used
   * to carry separate forms with diverging semantics (the dialog force-seeded
   * every bool to explicit `false`, overriding `default: true` materials; the
   * page couldn't validate enums); consolidating here makes
   * spinningRoomHelpers.buildMaterials the single validation/coercion path.
   *
   * Semantics: every field starts EMPTY and an empty optional is omitted so
   * the stamp applies the pattern's default — bools included (tri-state
   * select, not a checkbox). The component renders fields only; the consumer
   * owns the CTA, validation call, and error display.
   */
  import {
    materialInputKind,
    materialPlaceholder,
    type RawMaterialValues,
  } from '../../utils/spinningRoomHelpers.ts';
  import type { PatternMaterialField } from '../../stores/patterns.svelte.ts';

  let {
    schema,
    values = $bindable(),
    disabled = false,
    idPrefix = 'mat',
  }: {
    schema: PatternMaterialField[];
    values: RawMaterialValues;
    disabled?: boolean;
    idPrefix?: string;
  } = $props();

  // Narrow the string|boolean union for template value= bindings (an indexed
  // access doesn't narrow across two reads in markup).
  function text(name: string): string {
    const v = values[name];
    return typeof v === 'string' ? v : '';
  }
</script>

{#each schema as f (f.name)}
  <div class="pm-field">
    <label class="pm-label" for={`${idPrefix}-${f.name}`}>
      {f.name}
      {#if f.required}<span class="pm-req" title="required">*</span>{:else}<span class="pm-opt">optional</span>{/if}
      <span class="pm-type text-mono">{f.type}</span>
    </label>
    {#if f.description}<div class="pm-hint">{f.description}</div>{/if}

    {#if f.type === 'bool'}
      <select
        id={`${idPrefix}-${f.name}`}
        class="pm-input"
        value={text(f.name)}
        onchange={(e) => (values[f.name] = (e.currentTarget as HTMLSelectElement).value)}
        {disabled}
      >
        <option value="">{f.default !== undefined && f.default !== '' ? `default (${f.default})` : '(default)'}</option>
        <option value="true">true</option>
        <option value="false">false</option>
      </select>
    {:else if materialInputKind(f) === 'select'}
      <select
        id={`${idPrefix}-${f.name}`}
        class="pm-input"
        value={text(f.name)}
        onchange={(e) => (values[f.name] = (e.currentTarget as HTMLSelectElement).value)}
        {disabled}
      >
        <option value="">{f.default ? `default (${f.default})` : f.required ? '— choose —' : '(default)'}</option>
        {#each f.enum ?? [] as opt (opt)}<option value={opt}>{opt}</option>{/each}
      </select>
    {:else if materialInputKind(f) === 'json'}
      <textarea
        id={`${idPrefix}-${f.name}`}
        class="pm-input pm-ta text-mono"
        rows="3"
        placeholder={materialPlaceholder(f) || (f.type === 'list' ? '[…]' : '{…}')}
        value={text(f.name)}
        oninput={(e) => (values[f.name] = (e.currentTarget as HTMLTextAreaElement).value)}
        {disabled}
      ></textarea>
    {:else}
      <input
        id={`${idPrefix}-${f.name}`}
        class="pm-input"
        type={materialInputKind(f) === 'number' ? 'number' : 'text'}
        placeholder={materialPlaceholder(f)}
        value={text(f.name)}
        oninput={(e) => (values[f.name] = (e.currentTarget as HTMLInputElement).value)}
        {disabled}
      />
    {/if}
  </div>
{/each}

<style>
  .pm-field {
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
    min-width: 0;
  }
  .pm-label {
    display: flex;
    align-items: baseline;
    gap: 0.35rem;
    font-size: var(--text-xs);
    color: var(--fg-secondary);
  }
  .pm-req { color: var(--error); }
  .pm-opt { color: var(--text-muted); font-size: var(--text-2xs); }
  .pm-type { color: var(--text-muted); font-size: var(--text-2xs); margin-left: auto; }
  .pm-hint { font-size: var(--text-2xs); color: var(--text-muted); }
  .pm-input {
    background: var(--bg-subtle);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    color: var(--fg-primary);
    padding: 0.35rem 0.5rem;
    font-size: var(--text-xs);
    width: 100%;
  }
  .pm-input:disabled { opacity: 0.6; }
  .pm-ta { resize: vertical; }
</style>
