import { describe, expect, it } from 'vitest';
import {
  buildMaterials,
  greenCount,
  materialInputKind,
  materialPlaceholder,
  patternPickerGroups,
} from './spinningRoomHelpers.ts';
import type { PatternInfo, PatternMaterialField } from '../stores/patterns.svelte.ts';

const f = (over: Partial<PatternMaterialField>): PatternMaterialField => ({
  name: 'x',
  type: 'string',
  ...over,
});

describe('materialInputKind / materialPlaceholder', () => {
  it('maps schema types onto input widgets', () => {
    expect(materialInputKind(f({ type: 'bool' }))).toBe('checkbox');
    expect(materialInputKind(f({ type: 'enum' }))).toBe('select');
    expect(materialInputKind(f({ type: 'int' }))).toBe('number');
    expect(materialInputKind(f({ type: 'list' }))).toBe('json');
    expect(materialInputKind(f({ type: 'object' }))).toBe('json');
    expect(materialInputKind(f({ type: 'string' }))).toBe('text');
  });

  it('prefers the example, then the default, for placeholders', () => {
    expect(materialPlaceholder(f({ example: 'sprockctl', default: 'x' }))).toBe('sprockctl');
    expect(materialPlaceholder(f({ default: 'P2' }))).toBe('default: P2');
    expect(materialPlaceholder(f({}))).toBe('');
  });
});

describe('buildMaterials', () => {
  const schema: PatternMaterialField[] = [
    f({ name: 'topic', type: 'string', required: true }),
    f({ name: 'shape', type: 'enum', enum: ['signature-major', 'phase-major'], default: 'signature-major' }),
    f({ name: 'signatures', type: 'list' }),
    f({ name: 'count', type: 'int' }),
    f({ name: 'enqueue_hint', type: 'bool' }),
  ];

  it('coerces, validates, and omits empty optionals so stamp defaults apply', () => {
    const { materials, errors } = buildMaterials(schema, {
      topic: '  ClickHouse merges  ',
      shape: '',
      signatures: '[{"name":"sig"}]',
      count: '3',
      enqueue_hint: true,
    });
    expect(errors).toEqual([]);
    expect(materials).toEqual({
      topic: 'ClickHouse merges',
      signatures: [{ name: 'sig' }],
      count: 3,
      enqueue_hint: true,
    });
    expect('shape' in materials).toBe(false); // empty optional → server default
  });

  it('flags missing required, bad enum members, bad ints, and bad JSON', () => {
    const { errors } = buildMaterials(schema, {
      topic: '',
      shape: 'diagonal',
      signatures: 'not-json',
      count: 'three',
    });
    expect(errors).toContain('topic is required');
    expect(errors.some((e) => e.startsWith('shape must be one of'))).toBe(true);
    expect(errors).toContain('signatures: invalid JSON');
    expect(errors).toContain('count must be an integer');
  });

  it('rejects JSON of the wrong shape for list/object fields', () => {
    const listField = [f({ name: 'items', type: 'list' })];
    expect(buildMaterials(listField, { items: '{"a":1}' }).errors).toContain('items must be a JSON array');
    const objField = [f({ name: 'cfg', type: 'object' })];
    expect(buildMaterials(objField, { cfg: '[1]' }).errors).toContain('cfg must be a JSON object');
  });
});

describe('patternPickerGroups / greenCount', () => {
  const p = (over: Partial<PatternInfo>): PatternInfo =>
    ({ id: 'p', slug: 's', name: 'n', makes: 'm', version: '0.1', status: 'approved', ...over }) as PatternInfo;

  it('splits approved from candidates, drops deprecated, sorts by name', () => {
    const groups = patternPickerGroups([
      p({ id: 'b', name: 'B card', status: 'approved' }),
      p({ id: 'a', name: 'A card', status: 'approved' }),
      p({ id: 'c', name: 'C card', status: 'candidate' }),
      p({ id: 'd', name: 'D card', status: 'deprecated' }),
    ]);
    expect(groups.approved.map((x) => x.id)).toEqual(['a', 'b']);
    expect(groups.candidates.map((x) => x.id)).toEqual(['c']);
  });

  it('reads the taste-gate green count with a zero default', () => {
    expect(greenCount(p({}))).toBe(0);
    expect(greenCount(p({ provenance: { instances_shipped_green: 3 } }))).toBe(3);
  });
});
