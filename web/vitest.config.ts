import { mkdirSync } from 'node:fs'
import { fileURLToPath, URL } from 'node:url'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'

// Vitest parks two scratch areas outside the repo by default, both under
// os.tmpdir():
//   - each project gets `<tmpdir>/<projectId>/<transformMode>/<sha1>` and every
//     fetched module is written there (vitest 2.1 hardcodes
//     `join(tmpdir(), project.id)` and has no config key for it);
//   - Vite's dependency-optimiser cache lives in the same tree.
// Under a restricted sandbox the temp directory is not writable, those writes
// fail, and the run ends up collecting fewer test files than the suite has
// while still exiting 0 — a short green run instead of a red one (BACKLOG
// G19: "46 passed" while 48 files existed).
//
// Setting the temp variables here is the only hook available for the first
// area: vitest reads os.tmpdir() when it constructs each project, which happens
// after this file is loaded. cacheDir covers the second. Both point inside the
// repo, so collection no longer depends on the environment's temp directory and
// a partial run cannot look green. Both directories are git-ignored.
const tmpDir = fileURLToPath(new URL('./.vitest-tmp', import.meta.url))
mkdirSync(tmpDir, { recursive: true })
process.env.TMPDIR = tmpDir
process.env.TMP = tmpDir
process.env.TEMP = tmpDir

export default defineConfig({
  plugins: [react()],
  cacheDir: './.vitest-cache',
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
    include: ['src/**/*.test.{ts,tsx}'],
    css: false,
  },
})
