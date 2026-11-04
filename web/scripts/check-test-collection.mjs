/**
 * 收集数闸门（BACKLOG G19：`npm test` 会假绿）。
 *
 * 症状（2026-11-04 QA 实测）：在受限沙箱里 `npm test` 只收集到 59 files / 306
 * tests，却仍然 exit 0 —— 磁盘上是 61 个测试文件。`vitest.config.ts` 把 temp
 * 目录钉进仓库只是缓解手段，它无法证明「一个测试文件都没漏」。上线清单要求人工
 * 核对「Test Files 数 == `find src -name '*.test.ts*'` 的数量」，本脚本把那一步
 * 自动化，并且**在收集数不足时真的 exit 1**。
 *
 * 它做两件事：
 *   1. 跑一遍全量 vitest（`--reporter=json`），
 *   2. 把「收集到的测试文件集合」与「磁盘枚举到的测试文件集合」做集合比对，
 *      缺了/多了都打印差集并 exit 1。
 *
 * 用法：`npm run check:tests`
 *
 * 两点刻意设计：
 * - **不改 `npm test`**：那会改变所有人（包括正在并行验收的 QA）的命令语义，
 *   也会让「没有 vitest 报告文件」的环境失效。新入口独立存在，CI 侧应当用
 *   `check:tests` **替代** `test`（它会自己跑一遍测试，不是跟在 test 后面再跑）。
 * - **不碰 `.github/`**：CI 配置属于另一条工作线，本脚本只提供可被调用的入口。
 */
import { spawnSync } from 'node:child_process'
import { existsSync, mkdirSync, readdirSync, readFileSync, rmSync } from 'node:fs'
import { join, relative, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

const webRoot = fileURLToPath(new URL('..', import.meta.url))
const srcRoot = join(webRoot, 'src')
const reportDir = join(webRoot, '.vitest-tmp')
const reportPath = join(reportDir, 'collection-report.json')

/** 与 vitest.config.ts 的 `include: ['src/**\/*.test.{ts,tsx}']` 保持同一口径。 */
const TEST_FILE_PATTERN = /\.test\.(ts|tsx)$/

/** 路径统一成 posix 相对路径，避免 Windows 的 `\` 与大小写造成假差集。 */
function toPosix(path) {
  return path.split(sep).join('/')
}

function walk(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const full = join(dir, entry.name)
    return entry.isDirectory() ? walk(full) : [full]
  })
}

function diskTestFiles() {
  return walk(srcRoot)
    .filter((file) => TEST_FILE_PATTERN.test(file))
    .map((file) => toPosix(relative(webRoot, file)))
    .sort()
}

function fail(lines) {
  console.error('check:tests FAILED')
  for (const line of lines) console.error(line)
  process.exit(1)
}

const onDisk = diskTestFiles()

const vitestBin = join(webRoot, 'node_modules', 'vitest', 'vitest.mjs')
if (!existsSync(vitestBin)) {
  fail(['  找不到 node_modules/vitest/vitest.mjs，请先 npm install'])
}

if (existsSync(reportPath)) rmSync(reportPath, { force: true })
mkdirSync(reportDir, { recursive: true })

const run = spawnSync(
  process.execPath,
  [vitestBin, 'run', '--reporter=json', `--outputFile.json=${reportPath}`],
  { cwd: webRoot, stdio: 'inherit' },
)

if (!existsSync(reportPath)) {
  fail([
    '  vitest 没有产出 JSON 报告（--reporter=json 未生效？）',
    `  期望路径：${toPosix(relative(webRoot, reportPath))}`,
    `  vitest 退出码：${run.status}`,
  ])
}

let report
try {
  report = JSON.parse(readFileSync(reportPath, 'utf8'))
} catch (error) {
  fail([`  JSON 报告无法解析：${error instanceof Error ? error.message : String(error)}`])
}

/**
 * vitest 的 json reporter 把每个测试文件放在 `testResults[].name`（绝对路径）。
 * 用 name 而不是 `numTotalTestSuites`：前者能算出差集，后者只能比对总数。
 */
const collected = (report.testResults ?? [])
  .map((suite) => toPosix(relative(webRoot, String(suite.name ?? ''))))
  .filter((name) => name.length > 0)
  .sort()

const collectedSet = new Set(collected)
const diskSet = new Set(onDisk)
const missing = onDisk.filter((file) => !collectedSet.has(file))
const unexpected = collected.filter((file) => !diskSet.has(file))

if (missing.length > 0 || unexpected.length > 0) {
  const lines = [
    `  磁盘 ${onDisk.length} 个测试文件，vitest 只收集到 ${collected.length} 个（本轮结果不可信）`,
  ]
  if (missing.length > 0) {
    lines.push(`  漏收集 ${missing.length} 个：`)
    for (const file of missing) lines.push(`    - ${file}`)
  }
  if (unexpected.length > 0) {
    lines.push(`  多出 ${unexpected.length} 个（不在磁盘枚举结果里）：`)
    for (const file of unexpected) lines.push(`    + ${file}`)
  }
  lines.push('  常见原因：沙箱下 .vitest-tmp 不可写（BACKLOG G19），或改动过 vitest include/exclude。')
  fail(lines)
}

/**
 * 收集数对上了，但如果测试本身红了也要把非零退出码传出去——否则这个脚本会变成
 * 「只要文件数对就算过」的第二个假绿来源。
 */
if (run.status !== 0) {
  console.error('check:tests FAILED')
  console.error(`  收集数一致（${collected.length} 个文件），但 vitest 退出码为 ${run.status}`)
  process.exit(run.status ?? 1)
}

const totalTests = (report.testResults ?? []).reduce(
  (sum, suite) => sum + (suite.assertionResults?.length ?? 0),
  0,
)

rmSync(reportPath, { force: true })

console.log(
  `check:tests OK (${collected.length} files collected == ${onDisk.length} on disk, ${totalTests} tests)`,
)
