// Runnable fixture / smoke test for parseNamespace (namespace.ts).
//   pnpm dlx tsx src/lib/utils/namespace.fixture.ts

import { parseNamespace } from './namespace.ts';

function expect(label: string, actual: unknown, want: unknown): boolean {
  const ok = actual === want;
  if (ok) console.log(`PASS ${label}: got=${String(actual)}`);
  else console.error(`FAIL ${label}: got=${String(actual)} want=${String(want)}`);
  return ok;
}

let allOk = true;

function check(label: string, ns: string, repo: string, branch: string, synthetic: boolean) {
  const p = parseNamespace(ns);
  allOk = expect(`${label} · repo`, p.repo, repo) && allOk;
  allOk = expect(`${label} · branch`, p.branch, branch) && allOk;
  allOk = expect(`${label} · synthetic`, p.synthetic, synthetic) && allOk;
}

// Normal namespaces split into 2-level repo + branch (branch may have slashes).
check('main branch', 'services/loom-core/main', 'services/loom-core', 'main', false);
check('feature branch with slash', 'services/loom-core/feat/hud-chapters', 'services/loom-core', 'feat/hud-chapters', false);
check('users bucket', 'Users/cblevins/main', 'Users/cblevins', 'main', false);
check('real repo under agents bucket (has branch)', 'agents/foo/main', 'agents/foo', 'main', false);

// Malformed / synthetic shapes render as "unknown" (no repo).
check('synthetic mirror fallback', 'agents/claude-code-2876934595-3856228933', '', '', true);
check('degenerate codex ////main', '////main', '', '', true);
check('empty namespace', '', '', '', true);
check('single segment', 'loom-core', '', '', true);

if (!allOk) {
  console.error('namespace fixture: FAILURES detected');
  throw new Error('fixture failed');
}
console.log('namespace fixture: all cases pass');
