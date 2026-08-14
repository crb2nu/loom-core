import { describe, expect, it } from 'vitest';
import { bookNamesByRun, patternBooks, stampedPatternSlug } from './patternBooks.ts';
import type { BacklogItem, PipelineRun } from '../stores/mills.svelte.ts';
import type { PatternInfo } from '../stores/patterns.svelte.ts';

function pattern(over: Partial<PatternInfo>): PatternInfo {
  return {
    id: 'pat-x',
    slug: 'slug-x',
    name: 'Pattern X',
    makes: 'a service',
    version: '1',
    status: 'approved',
    ...over,
  } as PatternInfo;
}

function backlog(id: string, planID?: string): BacklogItem {
  return { ID: id, Title: id, State: 'running', Priority: 'P2', PlanID: planID } as BacklogItem;
}

function run(id: string, backlogID: string, state = 'running'): PipelineRun {
  return { ID: id, BacklogID: backlogID, Template: 't', State: state, Attempts: 1 } as PipelineRun;
}

describe('stampedPatternSlug', () => {
  const slugs = ['go-rest', 'go-rest-service', 'cli-tool'];

  it('matches the longest slug — go-rest must not swallow go-rest-service', () => {
    expect(stampedPatternSlug('plan-stamp-go-rest-service-payments', slugs)).toBe('go-rest-service');
    expect(stampedPatternSlug('plan-stamp-go-rest-payments', slugs)).toBe('go-rest');
  });

  it('matches a bare slug with no primary suffix', () => {
    expect(stampedPatternSlug('plan-stamp-cli-tool', slugs)).toBe('cli-tool');
  });

  it('returns null for non-stamp plans, unknown slugs, and missing ids', () => {
    expect(stampedPatternSlug('plan-council-something', slugs)).toBeNull();
    expect(stampedPatternSlug('plan-stamp-unknown-book-x', slugs)).toBeNull();
    expect(stampedPatternSlug(undefined, slugs)).toBeNull();
    // a slug that only partially overlaps a segment must not match
    expect(stampedPatternSlug('plan-stamp-go-restaurant-x', slugs)).toBeNull();
  });
});

describe('patternBooks', () => {
  const patterns = [
    pattern({ slug: 'go-rest-service', name: 'Go REST Service' }),
    pattern({ slug: 'cli-tool', name: 'CLI Tool' }),
    pattern({ slug: 'draft-thing', name: 'Draft', status: 'candidate' }),
  ];
  const items = [
    backlog('b1', 'plan-stamp-go-rest-service-payments'),
    backlog('b2', 'plan-stamp-cli-tool-loomctl'),
    backlog('b3'), // council-born, no plan
    backlog('b4', 'plan-council-organic'), // plan-born but not stamped
  ];

  it('attributes runs through backlog PlanID and counts by outcome', () => {
    const books = patternBooks(
      patterns,
      items,
      [run('a1', 'b1'), run('a2', 'b1'), run('a3', 'b3')],
      [run('h1', 'b2', 'merged'), run('h2', 'b1', 'escalated'), run('h3', 'b4', 'merged')],
    );
    const rest = books.find((b) => b.slug === 'go-rest-service')!;
    const cli = books.find((b) => b.slug === 'cli-tool')!;
    expect(rest).toMatchObject({ active: 2, merged: 0, escalated: 1 });
    expect(cli).toMatchObject({ active: 0, merged: 1, escalated: 0 });
  });

  it('shelves only approved patterns, working books first', () => {
    const books = patternBooks(patterns, items, [run('a1', 'b2')], []);
    expect(books.map((b) => b.slug)).toEqual(['cli-tool', 'go-rest-service']);
    expect(books.find((b) => b.slug === 'draft-thing')).toBeUndefined();
  });

  it('returns an empty shelf for an empty catalog', () => {
    expect(patternBooks([], items, [run('a1', 'b1')], [])).toEqual([]);
  });
});

describe('bookNamesByRun', () => {
  it('labels only attributable active runs', () => {
    const names = bookNamesByRun(
      [pattern({ slug: 'cli-tool', name: 'CLI Tool' })],
      [backlog('b2', 'plan-stamp-cli-tool-loomctl'), backlog('b3')],
      [run('a1', 'b2'), run('a2', 'b3')],
    );
    expect(names.get('a1')).toBe('CLI Tool');
    expect(names.has('a2')).toBe(false);
  });
});
