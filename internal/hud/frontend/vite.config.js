import { existsSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

// dist/.gitkeep is the only git-tracked file in dist/ — it keeps
// `//go:embed all:frontend/dist` (internal/hud/app.go) valid before the
// frontend has been built. Vite's emptyOutDir deletes it, and the deletion
// then lands in unrelated commits as `D dist/.gitkeep`, which reds every Go
// CI job with "pattern all:frontend/dist: no matching files found". So
// emptyOutDir stays off and this plugin cleans dist/ itself, sparing (and
// restoring) the placeholder.
function preserveDistGitkeep() {
  let outDir;
  let gitkeep;
  let contents = null;
  return {
    name: 'preserve-dist-gitkeep',
    apply: 'build',
    configResolved(config) {
      outDir = path.resolve(config.root, config.build.outDir);
      gitkeep = path.join(outDir, '.gitkeep');
    },
    buildStart() {
      if (!existsSync(outDir)) return;
      if (existsSync(gitkeep)) contents = readFileSync(gitkeep);
      for (const entry of readdirSync(outDir)) {
        if (entry === '.gitkeep') continue;
        rmSync(path.join(outDir, entry), { recursive: true, force: true });
      }
    },
    closeBundle() {
      if (contents !== null && !existsSync(gitkeep)) {
        writeFileSync(gitkeep, contents);
      }
    },
  };
}

// Dev proxy target for the HUD daemon. Honor HUD_API_TARGET so the documented
// `HUD_API_TARGET=http://localhost:3333 npx vite` workflow actually points the
// proxy at the running daemon; fall back to the historical default otherwise.
const apiTarget = process.env.HUD_API_TARGET || 'http://localhost:9800';

export default defineConfig({
  plugins: [svelte(), preserveDistGitkeep()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: apiTarget,
        changeOrigin: true,
      },
      '/api/events': {
        target: apiTarget,
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: 'dist',
    // Cleaning is handled by preserveDistGitkeep so the tracked
    // dist/.gitkeep survives the build. Do not turn this back on.
    emptyOutDir: false,
  },
});
