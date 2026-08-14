// Pure helpers for the Spinning Room's pattern mode (factory model J1 front
// door): grouping the catalog for the picker and turning a schema-driven
// materials form into a validated stamp payload. Rune-free so the node vitest
// project covers the whole chain without a Svelte runtime.

import type { PatternInfo, PatternMaterialField } from '../stores/patterns.svelte.ts';

/** Raw form values as the inputs hold them (strings + checkbox booleans). */
export type RawMaterialValues = Record<string, string | boolean>;

export type MaterialInputKind = 'checkbox' | 'select' | 'number' | 'json' | 'text';

/** Which input widget a materials field renders as. */
export function materialInputKind(f: PatternMaterialField): MaterialInputKind {
  switch (f.type) {
    case 'bool':
      return 'checkbox';
    case 'enum':
      return 'select';
    case 'int':
      return 'number';
    case 'list':
    case 'object':
      return 'json';
    default:
      return 'text';
  }
}

/** Placeholder text: the example wins, then the default, then empty. */
export function materialPlaceholder(f: PatternMaterialField): string {
  return f.example || (f.default ? `default: ${f.default}` : '');
}

/**
 * Validate + coerce raw form values into the stamp materials payload.
 * Empty OPTIONAL fields are omitted entirely so the stamp core applies the
 * pattern's declared defaults; empty REQUIRED fields are errors. list/object
 * fields are JSON text areas and must parse.
 */
export function buildMaterials(
  schema: PatternMaterialField[] | undefined,
  raw: RawMaterialValues,
): { materials: Record<string, unknown>; errors: string[] } {
  const materials: Record<string, unknown> = {};
  const errors: string[] = [];
  for (const f of schema ?? []) {
    const v = raw[f.name];
    if (f.type === 'bool') {
      // Checkboxes always carry an explicit value.
      materials[f.name] = v === true;
      continue;
    }
    const text = typeof v === 'string' ? v.trim() : '';
    if (text === '') {
      if (f.required) errors.push(`${f.name} is required`);
      continue; // optional + empty → let the stamp apply the default
    }
    switch (f.type) {
      case 'int': {
        const n = Number.parseInt(text, 10);
        if (Number.isNaN(n)) errors.push(`${f.name} must be an integer`);
        else materials[f.name] = n;
        break;
      }
      case 'enum': {
        if (f.enum && !f.enum.includes(text)) {
          errors.push(`${f.name} must be one of: ${f.enum.join(', ')}`);
        } else {
          materials[f.name] = text;
        }
        break;
      }
      case 'list':
      case 'object': {
        try {
          const parsed: unknown = JSON.parse(text);
          const isList = Array.isArray(parsed);
          if (f.type === 'list' && !isList) {
            errors.push(`${f.name} must be a JSON array`);
          } else if (f.type === 'object' && (isList || typeof parsed !== 'object' || parsed === null)) {
            errors.push(`${f.name} must be a JSON object`);
          } else {
            materials[f.name] = parsed;
          }
        } catch {
          errors.push(`${f.name}: invalid JSON`);
        }
        break;
      }
      default:
        materials[f.name] = text;
    }
  }
  return { materials, errors };
}

export interface PatternPickerGroups {
  approved: PatternInfo[];
  candidates: PatternInfo[];
}

/**
 * Split the catalog for the picker: approved cards are stampable; candidates
 * render visibly but disabled (the taste gate refuses to stamp them), sorted
 * name-ascending within each group. Deprecated patterns are dropped.
 */
export function patternPickerGroups(patterns: PatternInfo[]): PatternPickerGroups {
  const byName = (a: PatternInfo, b: PatternInfo) => a.name.localeCompare(b.name);
  return {
    approved: patterns.filter((p) => p.status === 'approved').sort(byName),
    candidates: patterns.filter((p) => p.status === 'candidate').sort(byName),
  };
}

/** Green-instance count for the picker badge (taste-gate provenance). */
export function greenCount(p: PatternInfo): number {
  return p.provenance?.instances_shipped_green ?? 0;
}
