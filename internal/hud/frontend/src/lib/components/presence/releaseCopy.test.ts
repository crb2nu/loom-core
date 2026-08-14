// Branch coverage for the File Claims force-release confirmation copy.
//
// The per-row Release used to fire with no gate at all. Now that all three
// routes share one ConfirmDialog, the copy is the only thing distinguishing
// "release this one claim" from "release every claim on every conflicting
// path" — so showing the wrong branch's message is a real operator hazard,
// not a cosmetic slip.

import { describe, expect, it } from 'vitest';
import { releaseConfirmCopy, shortClaimPath, type PendingRelease } from './releaseCopy.ts';
import type { FileClaimInfo } from '../../stores/fleet.svelte.ts';

const ctx = { selectedCount: 3, conflictPathList: 'a/b.go, c/d.go' };

function claim(over: Partial<FileClaimInfo> = {}): FileClaimInfo {
  return {
    id: 'claim-1',
    agent_id: 'codex-1',
    file_path: 'internal/hud/app.go',
    claim_type: 'edit',
    reason: 'editing',
    created_at: '2026-07-29T00:00:00Z',
    ...over,
  } as FileClaimInfo;
}

describe('releaseConfirmCopy', () => {
  it('is empty when nothing is pending, so the dialog stays closed', () => {
    expect(releaseConfirmCopy(null, ctx)).toEqual({ title: '', message: '' });
  });

  it('counts the selection for a bulk release of ticked rows', () => {
    const copy = releaseConfirmCopy({ kind: 'selected' }, ctx);
    expect(copy.title).toBe('Release selected file claims?');
    expect(copy.message).toContain('3 file claim(s)');
  });

  it('names every conflicting path for the release-all-conflicts route', () => {
    const copy = releaseConfirmCopy({ kind: 'conflicts' }, ctx);
    expect(copy.title).toBe('Release all conflicting claims?');
    expect(copy.message).toContain('a/b.go, c/d.go');
    // Must not imply it is scoped to the tick-box selection.
    expect(copy.message).not.toContain('3 file claim(s)');
  });

  it('names the holding agent and path for a single row release', () => {
    const copy = releaseConfirmCopy({ kind: 'single', claim: claim() }, ctx);
    expect(copy.title).toBe('Release this file claim?');
    expect(copy.message).toContain('codex-1');
    expect(copy.message).toContain('internal/hud/app.go');
    // The single route must not borrow the bulk blast radius in its copy.
    expect(copy.message).not.toContain('a/b.go');
    expect(copy.message).not.toContain('3 file claim(s)');
  });

  it('warns that the holding agent may be mid-edit', () => {
    // This is the reason the gate exists: a force-release yanks a file out from
    // under a running agent.
    const copy = releaseConfirmCopy({ kind: 'single', claim: claim() }, ctx);
    expect(copy.message).toContain('mid-edit');
  });

  it('truncates a long path so the dialog copy stays readable', () => {
    const long = `internal/hud/frontend/src/lib/components/presence/${'nested/'.repeat(8)}Deep.svelte`;
    const copy = releaseConfirmCopy({ kind: 'single', claim: claim({ file_path: long }) }, ctx);
    expect(copy.message).toContain(shortClaimPath(long));
    expect(copy.message).not.toContain(long);
  });

  it('covers every PendingRelease kind', () => {
    // Guards against a new route being added without its own copy branch,
    // which would fall through to the empty default and render a blank dialog.
    const kinds: PendingRelease[] = [
      { kind: 'selected' },
      { kind: 'conflicts' },
      { kind: 'single', claim: claim() },
    ];
    for (const pending of kinds) {
      const copy = releaseConfirmCopy(pending, ctx);
      expect(copy.title).not.toBe('');
      expect(copy.message).not.toBe('');
    }
  });
});
