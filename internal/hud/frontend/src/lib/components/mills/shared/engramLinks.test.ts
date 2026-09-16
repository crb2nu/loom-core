import { describe, expect, it } from 'vitest';
import {
  composedEngrams,
  patternsComposing,
  refLabel,
  resolveEngram,
} from './engramLinks.ts';
import type { EngramInfo } from '../../../stores/engrams.svelte.ts';
import type { PatternInfo } from '../../../stores/patterns.svelte.ts';

function node(id: string, name = 'N'): EngramInfo {
  return {
    id,
    name,
    tier: 1,
    proof_status: 'verified',
    prerequisites: [],
    proof: { refs: [] },
  };
}

function pattern(id: string, engrams: string[]): PatternInfo {
  return {
    id,
    slug: id,
    name: id,
    makes: 'thing',
    version: '0.1',
    status: 'approved',
    engrams,
  };
}

describe('resolveEngram', () => {
  // The join that the whole cross-link rests on: a pattern names engrams by
  // URI, but a node's id is the memory-item ID whenever the store sets one.
  it('matches a URI reference against a node keyed by URI', () => {
    const nodes = [node('engram://go/cli')];
    expect(resolveEngram('engram://go/cli', nodes)?.id).toBe('engram://go/cli');
  });

  it('matches a URI reference against a node keyed by memory-item id', () => {
    // This is the case a naive `n.id === ref` comparison silently drops.
    const nodes = [node('go/cli', 'Go CLI')];
    expect(resolveEngram('engram://go/cli', nodes)?.name).toBe('Go CLI');
  });

  it('is case-insensitive and tolerates surrounding whitespace', () => {
    const nodes = [node('engram://Go/CLI')];
    expect(resolveEngram('  engram://go/cli  ', nodes)).not.toBeNull();
  });

  it('returns null for a reference the catalog does not hold', () => {
    expect(resolveEngram('engram://none/here', [node('engram://go/cli')])).toBeNull();
  });

  it('returns null for an empty reference rather than matching arbitrarily', () => {
    expect(resolveEngram('', [node('engram://go/cli')])).toBeNull();
  });
});

describe('composedEngrams', () => {
  it('pairs each ref with its node and keeps unresolvable refs visible', () => {
    const nodes = [node('engram://go/cli', 'Go CLI')];
    const out = composedEngrams(pattern('p1', ['engram://go/cli', 'engram://ghost/x']), nodes);
    expect(out).toHaveLength(2);
    expect(out[0].node?.name).toBe('Go CLI');
    // A dangling ref is a real state, not a reason to drop the row.
    expect(out[1].node).toBeNull();
    expect(out[1].ref).toBe('engram://ghost/x');
  });

  it('returns an empty list for a null pattern', () => {
    expect(composedEngrams(null, [node('a')])).toEqual([]);
  });
});

describe('patternsComposing', () => {
  const patterns = [
    pattern('pattern-go-cli', ['engram://go/cli', 'engram://go/flags']),
    pattern('pattern-rest', ['engram://go/rest']),
    pattern('pattern-none', []),
  ];

  it('inverts the pattern list to find patterns composing an engram', () => {
    const found = patternsComposing(node('engram://go/cli'), patterns);
    expect(found.map((p) => p.id)).toEqual(['pattern-go-cli']);
  });

  it('inverts correctly when the node is keyed by memory-item id', () => {
    const found = patternsComposing(node('go/rest'), patterns);
    expect(found.map((p) => p.id)).toEqual(['pattern-rest']);
  });

  it('returns an empty list when no pattern composes the engram', () => {
    expect(patternsComposing(node('engram://orphan/x'), patterns)).toEqual([]);
  });

  it('returns an empty list for a null node', () => {
    expect(patternsComposing(null, patterns)).toEqual([]);
  });
});

describe('refLabel', () => {
  it('reduces a URI to its slug', () => {
    expect(refLabel('engram://go/cli')).toBe('cli');
  });

  it('falls back to the raw value when there is no slug', () => {
    expect(refLabel('bare-id')).toBe('bare-id');
  });
});
