import assert from 'node:assert/strict'
import { test, before, after } from 'node:test'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { pathToFileURL, fileURLToPath } from 'node:url'
import { build } from 'esbuild'

let dir
let view
let locales
const root = fileURLToPath(new URL('..', import.meta.url))
before(async () => {
  dir = await mkdtemp(join(tmpdir(), 'nm-update-ui-'))
  for (const name of ['windows-updates', 'locales']) await build({
    entryPoints: [join(root, 'src', 'client', `${name}.ts`)], bundle: true, format: 'esm',
    platform: 'node', outfile: join(dir, `${name}.mjs`), logLevel: 'silent',
  })
  view = await import(pathToFileURL(join(dir, 'windows-updates.mjs')).href)
  locales = await import(pathToFileURL(join(dir, 'locales.mjs')).href)
})
after(async () => { await rm(dir, { recursive: true, force: true }) })

const state = (phase, patch = {}) => ({ currentVersion: '1.0.3', phase, version: '1.0.4', progress: null,
  source: 'mirror', failure: null, checkedAt: 1, ...patch })
const t = (lang) => (key, values = {}) => Object.entries(values).reduce((s, [name, value]) => s.replaceAll(`{${name}}`, String(value)), locales[lang][key])

test('the corner menu offers download, progress without a dot, ready restart and manual retry in both languages', () => {
  for (const lang of ['en', 'zh']) {
    const available = view.updatePresentation(state('available'), t(lang))
    assert.equal(available.action, 'download')
    assert.equal(available.dot, true)
    assert.match(available.label, /1\.0\.4/)
    const downloading = view.updatePresentation(state('downloading', { progress: 42.7 }), t(lang))
    assert.equal(downloading.action, null)
    assert.equal(downloading.dot, false)
    assert.match(downloading.label, /42%/)
    const verifying = view.updatePresentation(state('verifying'), t(lang))
    assert.equal(verifying.action, null)
    assert.equal(verifying.dot, false)
    const ready = view.updatePresentation(state('ready'), t(lang))
    assert.equal(ready.action, 'restart')
    assert.equal(ready.dot, true)
    const failed = view.updatePresentation(state('failed', { failure: 'download' }), t(lang))
    assert.equal(failed.action, 'download')
    assert.equal(failed.dot, true)
  }
})

test('the running version hides the update action and dot, and failed checks remain actionable', () => {
  const current = view.updatePresentation(state('current', { version: null }), t('en'))
  assert.equal(current.label, '')
  assert.equal(current.dot, false)
  assert.equal(current.action, null)
  assert.match(current.status, /1\.0\.3/)
  const failed = view.updatePresentation(state('failed', { version: null, failure: 'check' }), t('en'))
  assert.equal(failed.action, 'check')
  assert.match(failed.status, /Could not check/)
})

test('a cancelled restart keeps the ready action while explaining that work can continue', () => {
  const result = view.updatePresentation(state('ready', { failure: 'restart' }), t('en'))
  assert.equal(result.action, 'restart')
  assert.match(result.status, /Your work can continue/)
})

test('paused automatic downloads explain network cost and keep a manual action and dot in both languages', () => {
  for (const lang of ['en', 'zh']) for (const paused of ['metered', 'unknown', 'disabled']) {
    const result = view.updatePresentation(state('available', { autoDownload: paused !== 'disabled', paused }), t(lang))
    assert.equal(result.dot, true)
    assert.equal(result.action, 'download')
    const reason = t(lang)(paused === 'metered' ? 'wuMetered' : paused === 'unknown' ? 'wuCostUnknown' : 'wuAutoOff')
    assert.ok(result.status.includes(reason))
    if (paused !== 'disabled') assert.ok(result.label.includes(reason))
    const failed = view.updatePresentation(state('failed', { failure: 'download', paused }), t(lang))
    assert.equal(failed.action, 'download')
    assert.equal(failed.dot, true)
    assert.ok(failed.status.includes(reason))
  }
})

test('menu actions reach the shell bridge rather than opening an external installer', async () => {
  const actions = []
  globalThis.nanomuseHarness = { platform: 'win32', permissions: () => undefined,
    updates: Object.fromEntries(['check', 'download', 'restart'].map((action) => [action, async () => { actions.push(action); return null }])) }
  try {
    for (const action of ['check', 'download', 'restart']) view.requestWindowsUpdate(action)
    await Promise.resolve()
    assert.deepEqual(actions, ['check', 'download', 'restart'])
  } finally { delete globalThis.nanomuseHarness }
})
