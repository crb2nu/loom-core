// Unit coverage for parseNamespace (namespace.ts).

import { describe, expect, it } from 'vitest';
import { parseNamespace } from './namespace.ts';

/** Asserts all three parsed fields (repo, branch, synthetic) for one namespace. */
function check(ns: string, repo: string, branch: string, synthetic: boolean) {
  const p = parseNamespace(ns);
  expect(p.repo).toBe(repo);
  expect(p.branch).toBe(branch);
  expect(p.synthetic).toBe(synthetic);
}

describe('namespace — parseNamespace', () => {
  // Normal namespaces split into 2-level repo + branch (branch may have slashes).
  it('main branch', () => {
    check('services/loom-core/main', 'services/loom-core', 'main', false);
  });

  it('feature branch with slash', () => {
    check('services/loom-core/feat/hud-chapters', 'services/loom-core', 'feat/hud-chapters', false);
  });

  it('users bucket', () => {
    check('Users/cblevins/main', 'Users/cblevins', 'main', false);
  });

  it('real repo under agents bucket (has branch)', () => {
    check('agents/foo/main', 'agents/foo', 'main', false);
  });

  // Malformed / synthetic shapes render as "unknown" (no repo).
  it('synthetic mirror fallback', () => {
    check('agents/claude-code-2876934595-3856228933', '', '', true);
  });

  it('degenerate codex ////main', () => {
    check('////main', '', '', true);
  });

  it('empty namespace', () => {
    check('', '', '', true);
  });

  it('single segment', () => {
    check('loom-core', '', '', true);
  });
});
