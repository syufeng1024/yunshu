import { defineConfig, devices } from '@playwright/test'

/**
 * T21 second segment: browser-level security checks.
 *
 * Two web servers:
 *  - the Vite dev server on 5180 (the card's required entry);
 *  - controld on 8081 when no backend is already running. Locally the Docker
 *    container from scripts/e2e-backend.sh is reused; CI lets Playwright start
 *    `go run ./cmd/controld` against the MySQL/Redis service containers.
 *
 * The tests never mock the API except where F4-2 explicitly allows a
 * component-layer fallback; F7-1 and F19-1 must hit the real control plane.
 */

// T37 段2: the G-12 spec asserts the rendered price on the full stack that is
// already running (nginx + production build). Set E2E_BASE_URL to point the
// fixture `page`/`request` at it; the security specs still pass their own
// baseURL through e2e/security/helpers.ts.
const DEV_SERVER_URL = 'http://127.0.0.1:5180'
// Default stays the dev server so `npm run e2e` keeps its old behaviour;
// E2E_BASE_URL is opt-in for runs against the already-running stack.
const BASE_URL = process.env.E2E_BASE_URL ?? DEV_SERVER_URL
const EXTERNAL_STACK = Boolean(process.env.E2E_BASE_URL)
const CONTROL_URL = 'http://127.0.0.1:8081/healthz'

const backendDSN =
  process.env.E2E_DB_DSN ??
  'root:test@tcp(127.0.0.1:3306)/yunshu_e2e?parseTime=true&loc=UTC&charset=utf8mb4'
const backendRedis = process.env.E2E_REDIS_ADDR ?? '127.0.0.1:6379'

// Base64 of exactly 32 bytes (the control plane's AES-256-GCM KEK contract).
const E2E_CHANNEL_KEY_KEK = 'MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY='

const backendEnv: Record<string, string> = {
  ...(process.env as Record<string, string>),
  ENV: 'dev',
  DB_DSN: backendDSN,
  LOG_SQL_DSN: backendDSN,
  REDIS_ADDR: backendRedis,
  SESSION_SECRET: 'e2e-only-session-secret-change-before-deploy',
  CONTROL_HTTP_ADDR: ':8081',
  CHANNEL_KEY_KEK: E2E_CHANNEL_KEY_KEK,
  FP_HMAC_KEY: 'e2e-only-fp-hmac-key',
  EXPORT_HMAC_KEY: 'e2e-only-export-hmac-key',
  // The security specs create their accounts through POST /auth/register and
  // have no mailbox to read a code from. "off" is the documented mode for
  // deployments that do not verify mailboxes; the code path itself is covered
  // by internal/control/auth's unit tests, not by the browser suite.
  AUTH_VERIFICATION_MODE: 'off',
}

// Local Windows machines use the installed Chrome; CI has only the bundled
// Chromium and must not be forced onto a channel.
const browserChannel = (process.env.E2E_BROWSER_CHANNEL ??
  (process.env.CI ? undefined : 'chrome')) as 'chrome' | 'msedge' | 'chromium' | undefined

const frontendEnv: Record<string, string> = {
  ...(process.env as Record<string, string>),
  // The e2e specs use the real API; never let a developer shell flip the
  // client into fixture mode.
  VITE_USE_FIXTURES: '0',
}

export default defineConfig({
  testDir: './e2e',
  outputDir: './test-results',
  fullyParallel: false,
  workers: 1,
  retries: process.env.CI ? 1 : 0,
  timeout: 60_000,
  expect: { timeout: 10_000 },
  reporter: [['list']],
  use: {
    baseURL: BASE_URL,
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
    video: 'off',
  },
  projects: [
    {
      name: 'chromium',
      use: {
        ...devices['Desktop Chrome'],
        ...(browserChannel ? { channel: browserChannel } : {}),
      },
    },
  ],
  // E2E_BASE_URL means the stack is already up (e.g. the 8088 production
  // build); never start a second dev server in that mode.
  webServer: EXTERNAL_STACK
    ? undefined
    : [
        {
          // Explicit IPv4: the dev-server URL is 127.0.0.1 and Windows may
          // resolve "localhost" to ::1 only.
          command: 'npm run dev -- --host 127.0.0.1 --port 5180',
          url: DEV_SERVER_URL,
          reuseExistingServer: true,
          timeout: 120_000,
          env: frontendEnv,
        },
        {
          command: 'go run ./cmd/controld',
          cwd: '..',
          url: CONTROL_URL,
          reuseExistingServer: true,
          timeout: 180_000,
          env: backendEnv,
        },
      ],
})
