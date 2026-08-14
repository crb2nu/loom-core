// Pure namespace parsing for the Fleet table. Agent namespaces are minted as
// `<repo-2-level>/<branch>` (NS_PROJECT/NS_BRANCH, pkg/generator), e.g.
// `services/loom-core/feat/x` → repo `services/loom-core`, branch `feat/x`.
//
// Two malformed shapes also occur in the wild and must render gracefully
// (the generator root-causes are fixed separately, but old sessions linger and
// federated peers may still emit them):
//   - synthetic mirror fallback `agents/<agent-id>` (no real repo)
//   - degenerate `////main` from a pre-fix Codex git inference
//
// Rune-free so it is unit-testable via the tsx fixture (namespace.fixture.ts).

export interface ParsedNamespace {
  /** 2-level repo path (e.g. "services/loom-core"), or "" when unknown. */
  repo: string;
  /** Branch (may contain slashes, e.g. "feat/x"), or "" when none. */
  branch: string;
  /** True when the value is a synthetic/fallback/malformed namespace with no real repo. */
  synthetic: boolean;
}

export function parseNamespace(namespace: string | null | undefined): ParsedNamespace {
  const ns = (namespace ?? '').trim();
  if (!ns) return { repo: '', branch: '', synthetic: true };

  const parts = ns.split('/');

  // Degenerate "////main" etc. — empty segment(s) in the repo position.
  if (parts.length < 2 || parts[0] === '' || parts[1] === '') {
    return { repo: '', branch: '', synthetic: true };
  }

  // Synthetic mirror fallback `agents/<agent-id>`: the heartbeat ensure-session
  // path used when no real namespace was resolvable. A real repo literally under
  // an "agents" bucket would carry a branch (≥3 parts), so only the bare 2-part
  // form is synthetic.
  if (parts[0] === 'agents' && parts.length === 2) {
    return { repo: '', branch: '', synthetic: true };
  }

  // Normal `repo(2-level)/branch(rest)`. Branch may contain slashes.
  return {
    repo: parts.slice(0, 2).join('/'),
    branch: parts.slice(2).join('/'),
    synthetic: false,
  };
}
