// Render coverage for InflightBoard's per-lane failure state.
//
// The Operator Deck is a landing surface: a lane whose source fetch failed used
// to fall through to `lane.empty` and read "Nothing in flight" — a dead daemon
// and a quiet mill rendered identically. The lane now carries an `error`, and
// three things have to hold together for that to be honest: the error line
// shows, the reassuring empty copy is suppressed, and rows already on screen
// survive a transient poll blip instead of blanking.

import { describe, expect, it } from 'vitest';
import { render } from 'svelte/server';
import InflightBoard from './InflightBoard.svelte';
import type { InflightRow, InflightKind } from '../../utils/operatorHelpers.ts';

interface Lane {
  kind: InflightKind;
  label: string;
  rows: InflightRow[];
  viewTarget: [string, string];
  empty: string;
  error?: string | null;
}

const EMPTY_COPY = 'Nothing in flight';

function row(over: Partial<InflightRow> = {}): InflightRow {
  return {
    key: 'row-1',
    kind: 'spawn',
    title: 'spawn-42',
    subtitle: 'loom-core · implement',
    state: 'running',
    severity: 'busy',
    age: '3m',
    ...over,
  } as InflightRow;
}

function lane(over: Partial<Lane> = {}): Lane {
  return {
    kind: 'spawn' as InflightKind,
    label: 'Spawns',
    rows: [],
    viewTarget: ['mills', 'spawn'],
    empty: EMPTY_COPY,
    ...over,
  };
}

function html(lanes: Lane[]): string {
  return render(InflightBoard, {
    props: { lanes, selectedKey: null, onSelect: () => {}, onOpenView: () => {} },
  }).body;
}

describe('InflightBoard — lane in error', () => {
  it('renders the error line', () => {
    const out = html([lane({ error: 'daemon unreachable' })]);
    expect(out).toContain('lane-error');
    expect(out).toContain('Unavailable — daemon unreachable');
  });

  it('marks the error line as an alert so it is announced, not just colored', () => {
    const out = html([lane({ error: 'daemon unreachable' })]);
    expect(out).toContain('role="alert"');
  });

  it('suppresses the reassuring empty copy', () => {
    // The regression: `lane.empty` renders whenever rows are absent, so a
    // failed fetch read as a healthy idle lane.
    const out = html([lane({ error: 'daemon unreachable' })]);
    expect(out).not.toContain(EMPTY_COPY);
    expect(out).not.toContain('lane-empty');
  });

  it('still renders rows when rows are non-empty', () => {
    // A poll blip must not blank rows the operator is reading: the error line
    // and the live rows coexist.
    const out = html([
      lane({ error: 'poll timed out', rows: [row(), row({ key: 'row-2', title: 'spawn-43' })] }),
    ]);
    expect(out).toContain('Unavailable — poll timed out');
    expect(out).toContain('spawn-42');
    expect(out).toContain('spawn-43');
    expect(out).not.toContain(EMPTY_COPY);
  });

  it('counts the rows it is still showing rather than reporting zero', () => {
    const out = html([lane({ error: 'poll timed out', rows: [row()] })]);
    expect(out).toContain('>1</span>');
  });
});

describe('InflightBoard — lane without error', () => {
  it('shows the named empty copy when the lane is genuinely quiet', () => {
    const out = html([lane()]);
    expect(out).toContain(EMPTY_COPY);
    expect(out).not.toContain('lane-error');
  });

  it('treats an explicit null error as no error', () => {
    // Stores reset `.error` to null on a successful refetch; null must take the
    // healthy path, not the falsy-but-present one.
    const out = html([lane({ error: null })]);
    expect(out).toContain(EMPTY_COPY);
    expect(out).not.toContain('lane-error');
  });

  it('renders rows with no error line', () => {
    const out = html([lane({ rows: [row()] })]);
    expect(out).toContain('spawn-42');
    expect(out).not.toContain('lane-error');
    expect(out).not.toContain(EMPTY_COPY);
  });
});

describe('InflightBoard — lanes are independent', () => {
  it('fails only the lane whose source is down', () => {
    // Three lanes over one row shape: one store outage must not blank the
    // other two, which is what a board-level error banner would have done.
    const out = html([
      lane({ kind: 'spawn' as InflightKind, label: 'Spawns', error: 'daemon unreachable' }),
      lane({ kind: 'run' as InflightKind, label: 'Runs', rows: [row({ key: 'r-1', title: 'run-7' })] }),
      lane({ kind: 'mr' as InflightKind, label: 'MRs' }),
    ]);
    expect(out).toContain('Unavailable — daemon unreachable');
    expect(out).toContain('run-7');
    // The healthy-but-quiet MR lane keeps its empty copy.
    expect(out).toContain(EMPTY_COPY);
  });
});
