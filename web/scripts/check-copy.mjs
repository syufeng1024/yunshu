/**
 * CI guard for the T17 copy/colour constraints.
 *
 * Run with `npm run check:copy`. Banned brand forms are assembled from
 * fragments so this script itself never contains the forbidden literal.
 */
import { readdirSync, readFileSync } from 'node:fs'
import { join, relative, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

const webRoot = fileURLToPath(new URL('..', import.meta.url))
const srcRoot = join(webRoot, 'src')

const BANNED_BRAND_FORMS = [
  ['云枢', 'API'].join(''),
  ['云枢', 'api'].join(''),
  ['云枢', '_API'].join(''),
  ['云枢', ' API 中转站'].join(''),
]

function walk(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const full = join(dir, entry.name)
    return entry.isDirectory() ? walk(full) : [full]
  })
}

const files = walk(srcRoot).filter((file) => /\.(ts|tsx|css)$/.test(file))
const failures = []

for (const file of files) {
  const rel = relative(webRoot, file).split(sep).join('/')
  const text = readFileSync(file, 'utf8')

  for (const term of BANNED_BRAND_FORMS) {
    if (text.includes(term)) failures.push(`${rel}: banned brand form`)
  }
  if (!rel.endsWith('src/styles/tokens.css') && /#[0-9a-fA-F]{6}/.test(text)) {
    failures.push(`${rel}: raw hex colour outside tokens.css`)
  }
  // Tests are exempt from the storage rule only. A component test has to lay
  // out the browser state it asserts on (ScrollRestorer's case: clear
  // sessionStorage, then render, then check the restored offset), which
  // production code may never do. The brand, hex-colour and credit-conversion
  // rules stay in force for tests: a test that shipped product copy, a raw
  // colour or a second conversion would still fail.
  const isTestFile = /\.(test|spec)\.(ts|tsx)$/.test(rel)
  if (!isTestFile && (text.includes('localStorage') || text.includes('sessionStorage'))) {
    failures.push(`${rel}: browser storage reference`)
  }
  if (rel.startsWith('src/pages/') && text.includes('/ 100')) {
    failures.push(`${rel}: duplicate credit conversion`)
  }
}

if (failures.length > 0) {
  console.error('check:copy FAILED')
  for (const failure of failures) console.error(`  ${failure}`)
  process.exit(1)
}

console.log(`check:copy OK (${files.length} files): brand / hex / storage / conversion`)
