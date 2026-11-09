// The words under the face while a task runs (src/client/ProfileDrawer.tsx): the step's own
// words, the kind of step for a tool the client knows, the tool's own name for one it does
// not, "On it: <the request>" between steps. Never a state of mind where something concrete
// is known. ProfileDrawer is bundled on the fly with the harness's primitives stubbed.
import { after, before, test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { build } from 'esbuild'

const here = fileURLToPath(new URL('.', import.meta.url))
let dir
let m

const PRIMITIVES = `
import { createElement as h } from 'react'
export const Button = ({ children, ...p }) => h('button', { type: 'button', ...p }, children)
export const Input = (p) => h('input', p)
export const Switch = (p) => h('input', { type: 'checkbox', ...p })
export const Textarea = (p) => h('textarea', p)
export const Modal = ({ children }) => h('div', null, children)
export const Toast = { show() {}, success() {}, error() {}, info() {} }
export const StateDot = (p) => h('span', p)
export const closeTopModal = () => {}
export const useModalLayer = () => ({})
export const resolveSlotLabel = (s) => String(s ?? '')
`

const ENTRY = `
export { en, zh, fill } from './src/client/locales.ts'
export { statusWords, toolWords, onIt, requestBrief } from './src/client/ProfileDrawer.tsx'
`

before(async () => {
  dir = await mkdtemp(join(tmpdir(), 'nm-status-words-'))
  await build({
    stdin: { contents: ENTRY, resolveDir: join(here, '..'), loader: 'js', sourcefile: 'status-entry.js' },
    outfile: join(dir, 'status.mjs'),
    format: 'esm',
    platform: 'node',
    target: 'node22',
    bundle: true,
    jsx: 'automatic',
    logLevel: 'silent',
    define: { 'process.env.NODE_ENV': '"production"' },
    plugins: [{
      name: 'stub-harness',
      setup(b) {
        b.onResolve({ filter: /^@deepseek-ai\// }, (args) => ({ path: args.path, namespace: 'stub' }))
        b.onLoad({ filter: /.*/, namespace: 'stub' }, () => ({ contents: PRIMITIVES, loader: 'js', resolveDir: join(here, '..') }))
      },
    }],
  })
  m = await import(pathToFileURL(join(dir, 'status.mjs')).href)
})

after(async () => {
  await rm(dir, { recursive: true, force: true })
})

const t = (key, vars) => m.fill(m.en[key] ?? key, vars)
const record = (current, request = 'Book the table for Friday') => ({ status: 'running', request, current, steps: [] })

test('a known tool shows the kind of step; its description joins when the tool takes one', () => {
  assert.equal(m.statusWords(t, record({ name: 'web_search', title: '' })), 'Searching')
  assert.equal(m.statusWords(t, record({ name: 'computer_screen', title: '' })), 'Looking at the screen')
  assert.equal(m.statusWords(t, record({ name: 'web_fetch', title: 'ctrip.com' })), 'Reading · ctrip.com')
  assert.equal(m.statusWords(t, record({ name: 'computer_act', title: '打开携程网站', own: true })), '打开携程网站')
})

test('a tool the client does not know is named in its own words, not as a mood', () => {
  assert.equal(m.statusWords(t, record({ name: 'mcp__12306__get-tickets', title: '' })), '12306 · get tickets')
  assert.equal(m.statusWords(t, record({ name: 'mcp__amap_maps__geocode', title: '北京南站' })), 'amap maps · geocode · 北京南站')
  assert.equal(m.statusWords(t, record({ name: 'translate_text', title: '' })), 'translate text')
  assert.equal(m.toolWords('mcp__kuaidi100__query-trace', 'query-trace'), 'kuaidi100 · query trace')
  assert.equal(m.toolWords('remember', 'remember'), 'remember')
})

test('between steps the request itself is the line; only with nothing known at all does the generic line show', () => {
  assert.equal(m.statusWords(t, record(undefined)), 'On it: Book the table for Friday')
  assert.equal(m.statusWords(t, record(undefined, '')), 'Hard at work')
  assert.equal(m.statusWords(t, { ...record(undefined), status: 'waiting' }), 'Waiting for you')
  assert.equal(m.requestBrief('  第一行很长很长很长很长很长很长很长很长很长很长很长很长很长很长很长很长很长很长很长很长\n第二行'), `${'第一行很长很长很长很长很长很长很长很长很长很长很长很长很长很长很长很长很长很长很长很长'.slice(0, 36)}…`)
  assert.equal(m.requestBrief('Find the cheapest train to Shanghai tomorrow morning, second class'), 'Find the cheapest train to Shanghai…')
})
