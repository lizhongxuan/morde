import { test, before, after } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { pathToFileURL, fileURLToPath } from 'node:url'
import { build } from 'esbuild'

const here = fileURLToPath(new URL('.', import.meta.url))
let dir
let bundle

const ENTRY = `
import { createElement as h } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
export { en, zh, fill } from './locales.ts'
import { makeCapsule } from './Capsule.tsx'
export function renderCard(t, ask) {
  globalThis.__nmCapsuleLive = { hub: { asks: [ask] }, notices: [], blackScreenAt: 0 }
  const Capsule = makeCapsule({ t })
  return renderToStaticMarkup(h(Capsule))
}
`

before(async () => {
  dir = await mkdtemp(join(tmpdir(), 'nm-capsule-'))
  await build({
    stdin: { contents: ENTRY, resolveDir: here, loader: 'js', sourcefile: 'capsule-test.js' },
    outfile: join(dir, 'capsule.mjs'),
    format: 'esm',
    platform: 'node',
    target: 'node22',
    bundle: true,
    jsx: 'automatic',
    logLevel: 'silent',
    define: { 'process.env.NODE_ENV': '"production"' },
    banner: { js: "import { createRequire as __nmCreateRequire } from 'node:module'; const require = __nmCreateRequire(import.meta.url);" },
    plugins: [{
      name: 'stub-capsule-runtime',
      setup(b) {
        b.onResolve({ filter: /^@deepseek-ai\/dsh-client-ui-primitives$/ }, (args) => ({ path: args.path, namespace: 'stub' }))
        b.onResolve({ filter: /^\.\/(api|live|bridge)\.ts$|^\.\/HandsCheck\.tsx$/ }, (args) => ({ path: args.path.slice(2), namespace: 'stub' }))
        b.onLoad({ filter: /.*/, namespace: 'stub' }, (args) => {
          if (args.path === '@deepseek-ai/dsh-client-ui-primitives') return { contents: `
            import { createElement as h } from 'react'
            export const Button = ({ children, ...p }) => h('button', { type: 'button', ...p }, children)
            export const Toast = () => null
          `, loader: 'js', resolveDir: here }
          if (args.path === 'api.ts') return { contents: `export const call = async () => undefined`, loader: 'js', resolveDir: here }
          if (args.path === 'live.ts') return { contents: `export function useLive() { return globalThis.__nmCapsuleLive }`, loader: 'js', resolveDir: here }
          if (args.path === 'bridge.ts') return { contents: `export const bridge = () => undefined`, loader: 'js', resolveDir: here }
          return { contents: `export const BlackScreenNotice = () => null`, loader: 'js', resolveDir: here }
        })
      },
    }],
  })
  bundle = await import(pathToFileURL(join(dir, 'capsule.mjs')).href)
})

after(async () => {
  if (dir) await rm(dir, { recursive: true, force: true })
})

function translator(lang) {
  const dict = bundle[lang]
  return (key, values) => values ? bundle.fill(dict[key], values) : dict[key]
}

const onceOnlyAsk = { id: 'ask-1', from: 'Phone', fromId: 'device-1', action: 'shell', text: 'list files', at: 1, onceOnly: true }

test('once-only questions offer Allow once and Not now, with no Always action', () => {
  const markup = bundle.renderCard(translator('en'), onceOnlyAsk)
  assert.match(markup, />Allow once<\/button>/)
  assert.match(markup, />Not now<\/button>/)
  assert.doesNotMatch(markup, /Always for/)
  assert.equal([...markup.matchAll(/<button\b/g)].length, 2)
})

test('once-only questions keep the same two choices in Chinese', () => {
  const markup = bundle.renderCard(translator('zh'), onceOnlyAsk)
  assert.match(markup, />允许一次<\/button>/)
  assert.match(markup, />暂不<\/button>/)
  assert.doesNotMatch(markup, /始终允许/)
  assert.equal([...markup.matchAll(/<button\b/g)].length, 2)
})

test('ordinary questions still offer Always for this device', () => {
  const markup = bundle.renderCard(translator('en'), { ...onceOnlyAsk, onceOnly: false })
  assert.match(markup, />Always for Phone<\/button>/)
  assert.equal([...markup.matchAll(/<button\b/g)].length, 3)
})
