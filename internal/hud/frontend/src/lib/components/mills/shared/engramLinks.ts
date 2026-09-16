// engramLinks — the join between the two halves of the reusable-knowledge
// surface, and the reverse index the API does not give us.
//
// A pattern names the engrams it composes as URIs (`engram://<family>/<slug>`,
// schema_pattern.go:122). An engram node's `id`, however, is the *memory-item*
// ID and only falls back to the URI when that is empty
// (bridge/agent_engrams.go:54-56), while `prerequisites[]` and graph edge
// `from`/`to` are always URIs (svc_engrams.go:581). So `nodes.find(n => n.id
// === uri)` matches on some stores and silently matches nothing on others —
// the exact shape of bug that renders a cross-link inert without erroring.
// Every lookup here is therefore tolerant: id, then URI, then family/slug tail.
//
// The engram -> pattern direction has no API at all (the seeded
// `pattern:<id>` tag lives in `tags`, which EngramInfo does not expose), but it
// is exactly the inversion of the pattern list the panel has already fetched.

import type { EngramInfo } from '../../../stores/engrams.svelte.ts';
import type { PatternInfo } from '../../../stores/patterns.svelte.ts';

/** Strip the `engram://` scheme, leaving `<family>/<slug>`. */
function tail(ref: string): string {
  const trimmed = ref.trim();
  const scheme = trimmed.indexOf('://');
  return (scheme >= 0 ? trimmed.slice(scheme + 3) : trimmed).toLowerCase();
}

/**
 * Every identity a node can be referenced by: its id, and — when the id is a
 * memory-item ID rather than a URI — the `family/slug` tail of any URI that
 * addresses it. Node objects carry no explicit `uri`, so the tail of the id is
 * the only additional key available; matching stays correct because a pattern
 * ref and a node id that denote the same engram share that tail.
 */
function keysFor(node: EngramInfo): string[] {
  const keys = [node.id.toLowerCase(), tail(node.id)];
  return keys.filter((k, i) => k !== '' && keys.indexOf(k) === i);
}

/**
 * Resolve one engram reference (URI or bare id) against the loaded nodes.
 * Returns null when the catalog has no such engram — a real state worth
 * rendering honestly rather than papering over.
 */
export function resolveEngram(ref: string, nodes: EngramInfo[]): EngramInfo | null {
  if (!ref) return null;
  const needle = ref.toLowerCase();
  const needleTail = tail(ref);
  return (
    nodes.find((n) => keysFor(n).includes(needle)) ??
    nodes.find((n) => keysFor(n).includes(needleTail)) ??
    null
  );
}

/** A pattern's composed engrams, each paired with its node when resolvable. */
export interface ComposedEngram {
  ref: string;
  node: EngramInfo | null;
}

export function composedEngrams(
  pattern: PatternInfo | null,
  nodes: EngramInfo[]
): ComposedEngram[] {
  return (pattern?.engrams ?? []).map((ref) => ({ ref, node: resolveEngram(ref, nodes) }));
}

/**
 * The reverse edge: which patterns compose this engram. Derived by inverting
 * the pattern list rather than asking the API, which cannot answer it.
 */
export function patternsComposing(
  node: EngramInfo | null,
  patterns: PatternInfo[]
): PatternInfo[] {
  if (!node) return [];
  const keys = keysFor(node);
  return patterns.filter((p) =>
    (p.engrams ?? []).some((ref) => {
      const needle = ref.toLowerCase();
      return keys.includes(needle) || keys.includes(tail(ref));
    })
  );
}

/** Short display label for a reference the catalog could not resolve. */
export function refLabel(ref: string): string {
  const t = tail(ref);
  const slug = t.slice(t.lastIndexOf('/') + 1);
  return slug || ref;
}
