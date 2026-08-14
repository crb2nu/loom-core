import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

// Dev proxy target for the HUD daemon. Honor HUD_API_TARGET so the documented
// `HUD_API_TARGET=http://localhost:3333 npx vite` workflow actually points the
// proxy at the running daemon; fall back to the historical default otherwise.
const apiTarget = process.env.HUD_API_TARGET || 'http://localhost:9800';

export default defineConfig({
  plugins: [svelte()],
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
    emptyOutDir: true,
  },
});
