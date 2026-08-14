import { describe, expect, it } from 'vitest';
import { cksum, cksumString } from './cksum.ts';

// Vectors generated with the real /usr/bin/cksum on macOS via
// `printf '%s' "<input>" | cksum` — the exact producer the lifecycle hooks
// call when minting agent ids.
describe('cksum (POSIX CRC-32/CKSUM)', () => {
  it('matches /usr/bin/cksum on the empty string', () => {
    expect(cksum('')).toBe(4294967295);
  });

  it('matches /usr/bin/cksum on plain text', () => {
    expect(cksum('hello')).toBe(3287646509);
  });

  it('reproduces a real SESSION_SCOPE from a Claude session uuid', () => {
    // Live linkage proof: this session uuid hashed to the SESSION_SCOPE
    // segment of the fleet agent id claude-code-3363428570-1735870880.
    expect(cksumString('be2382e8-d62e-40d2-9ed4-abeac9078fbe')).toBe('1735870880');
  });

  it('reproduces a real WS_HASH from a workspace root path', () => {
    expect(
      cksumString(
        '/Users/cblevins/workspace/services/loom-core/.claude/worktrees/mac-session-grouping-transcripts-65cfce',
      ),
    ).toBe('3363428570');
    // Workspace-anchored codex id: codex-1503458259.
    expect(cksumString('/Users/cblevins/workspace/services/familyforge')).toBe('1503458259');
  });
});
