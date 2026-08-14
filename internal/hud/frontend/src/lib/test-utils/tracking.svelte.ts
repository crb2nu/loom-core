/**
 * Test-only reactivity harness. Runes only compile in `.svelte.ts` modules,
 * so plain `.test.ts` files import this to reproduce a component's tracking
 * context without mounting a component.
 */

/**
 * Runs `fn` inside a tracking `$effect` under a dedicated effect root —
 * exactly the context a panel's mount effect provides. Any synchronous
 * `$state` read inside `fn` becomes a dependency: if `fn` later writes that
 * state, the effect re-runs `fn`. Returns the root's teardown.
 */
export function runInTrackingEffect(fn: () => void): () => void {
  return $effect.root(() => {
    $effect(() => {
      fn();
    });
  });
}
