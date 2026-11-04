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
//
// 每次运行再用**按 pid 区分的子目录**。这是**防御性改动，不是已验证的修复**：
// 它消除的是「多个 vitest 进程共用同一个 cache / temp 目录」这个**潜在**风险。
//
// 关于 G19（「少收集测试文件但仍然 exit 0」）的已知事实，别当成已结案：
//   - 2026-11-04 观测到 59/61，stdout 里有 `EPERM ... .vitest-tmp ...`（沙箱
//     文件系统写不进去）。这是目前最可信的机制，但至今没有被独立复现；
//   - 同期还出现过一次「某个进程收集到 `e2e/**/*.spec.ts`」的现象，事后查明是
//     启动命令的 cwd 写错（`cd web && A & B & wait` 里 `&` 的优先级高于 `&&`，
//     只有 A 拿到了 `cd`，B 在仓库根目录跑、根本没加载本文件），于是走了 vitest
//     默认的 include `**/*.{test,spec}.?(c|m)[jt]s?(x)` 才收进 e2e 目录
//     （67 = 61 + 6），并且因为没有 jsdom 而报 `window is not defined`。
//     **那不是缓存泄漏**，是「没吃到本配置」的指纹；
//   - 所以「并发共用 cache 会互相污染」这条**未被复现**（QA 的对照实验里两份
//     全量并发各写各的 report，都是 61 files / 0 泄漏）。本改动的定位是降低这类
//     风险的概率，不构成结论。
//
// 这两个目录（`.vitest-cache/<pid>`、`.vitest-tmp/<pid>`）会按 pid 累积（每次
// 运行留 2~3 个目录，实测 40 次跑攒了 139 个），已由 `web/.gitignore` 忽略，
// 随时可以整个删掉，删掉只会让下一次跑得慢一点。
const runId = String(process.pid)
const cacheDir = `./.vitest-cache/${runId}`
const tmpDir = fileURLToPath(new URL(`./.vitest-tmp/${runId}`, import.meta.url))
mkdirSync(tmpDir, { recursive: true })
process.env.TMPDIR = tmpDir
process.env.TMP = tmpDir
process.env.TEMP = tmpDir

export default defineConfig({
  plugins: [react()],
  cacheDir,
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
    // 5000ms 是 vitest 的默认值，不是设计意图。本机负载下实测
    // `ResetPasswordPage` / `TicketsPage` / `AccountPage` / `CreateKeyDialog` 的
    // 合法用例落在 5080–5398ms，用默认阈值会被随机误杀；一道会误报的闸门，
    // 两次之后所有人就学会「看到红先重跑」，等于没有闸门。抬到 15s 后真死循环
    // 仍然会暴露。
    testTimeout: 15000,
  },
})
