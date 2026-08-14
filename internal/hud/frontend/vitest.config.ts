/// <reference types="vitest/config" />
import { defineConfig } from 'vitest/config';
import { svelte } from '@sveltejs/vite-plugin-svelte';

// Dedicated vitest config so the dev-server proxy in vite.config.js doesn't
// leak into test runs. The svelte plugin is required so the rune-based
// `.svelte.ts` store modules compile under the test runner.
//
// Two projects, split by what the test actually needs:
//
//   node — the default. Stores, pure helpers, and server-rendered component
//     markup need only DOM-free globals (fetch / AbortController /
//     DOMException), all of which Node 18+ provides. Keeping this the default
//     means the overwhelming majority of the suite pays no DOM-emulation cost.
//
//   dom  — `*.dom.test.ts` only. Keyboard/focus behavior (ConfirmDialog's
//     Escape contract, DataTable's j/k row cursor) is event wiring, not
//     markup: svelte/server renders the template but never attaches a
//     listener, so these have to mount client-side against a real DOM.
//     `resolve.conditions: ['browser']` is what makes svelte resolve its
//     client runtime instead of the SSR build.
//     happy-dom rather than jsdom: jsdom 30 bundles an undici whose
//     CacheStorage init throws `webidl.util.markAsUncloneable is not a
//     function` on Node 20 (the version CI's node:20 image and this repo run),
//     killing the whole project before collection.
export default defineConfig({
  test: {
    projects: [
      {
        plugins: [svelte()],
        test: {
          name: 'node',
          environment: 'node',
          include: ['src/**/*.test.ts'],
          exclude: ['src/**/*.dom.test.ts'],
        },
      },
      {
        plugins: [svelte()],
        resolve: { conditions: ['browser'] },
        test: {
          name: 'dom',
          environment: 'happy-dom',
          include: ['src/**/*.dom.test.ts'],
        },
      },
    ],
  },
});
