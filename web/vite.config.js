import { writeFileSync } from 'node:fs';
import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

// emptyOutDir removes dist/.keep, the tracked placeholder that lets a fresh
// clone `go build` (the binary embeds dist). Put it back, so a build never
// shows up as a deleted file waiting to be committed.
const keepDist = {
  name: 'keep-dist',
  closeBundle() {
    writeFileSync('dist/.keep', '');
  },
};

// `npm run dev` serves the UI with hot reload and forwards /api to a
// Homestack server running on :5080.
export default defineConfig({
  plugins: [svelte(), keepDist],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    target: 'es2022', // noVNC uses top-level await
    chunkSizeWarningLimit: 800,
  },
  server: {
    proxy: {
      '/api': { target: 'http://localhost:5080', ws: true, changeOrigin: false },
    },
  },
});
