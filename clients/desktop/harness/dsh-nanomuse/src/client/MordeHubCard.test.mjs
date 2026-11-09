import { test, before, after } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { build } from 'esbuild'

const here = fileURLToPath(new URL('.', import.meta.url))
let dir
let bundle
const previousWindow = globalThis.window

const ENTRY = `
import * as React from 'react'
import { MordeHubCard } from './MordeHubCard.tsx'
export { en, zh, fill } from './locales.ts'
export function renderCard(t, mordeHub, reset = false) {
  if (reset) React.__resetHooks()
  React.__beginRender()
  globalThis.__nmMordeHubLive = { mordeHub }
  return resolve(MordeHubCard({ t }))
}
function resolve(node) {
  if (Array.isArray(node)) return node.map(resolve)
  if (!node || typeof node !== 'object') return node
  if (typeof node.type === 'function') return resolve(node.type(node.props))
  return { ...node, props: { ...node.props, children: resolve(node.props.children) } }
}
`

before(async () => {
  dir = await mkdtemp(join(tmpdir(), 'nm-morde-hub-card-'))
  await build({
    stdin: { contents: ENTRY, resolveDir: here, loader: 'js', sourcefile: 'morde-hub-card-test.js' },
    outfile: join(dir, 'card.mjs'),
    format: 'esm',
    platform: 'node',
    target: 'node22',
    bundle: true,
    jsx: 'automatic',
    logLevel: 'silent',
    plugins: [{
      name: 'test-client-runtime',
      setup(b) {
        b.onResolve({ filter: /^react$/ }, () => ({ path: 'react-shim', namespace: 'react-shim' }))
        b.onLoad({ filter: /.*/, namespace: 'react-shim' }, () => ({ contents: `
          let slots = []
          let cursor = 0
          export const createElement = (type, props, ...children) => ({
            type,
            props: { ...(props ?? {}), ...(children.length ? { children: children.length === 1 ? children[0] : children } : {}) },
          })
          export function __beginRender() { cursor = 0 }
          export function __resetHooks() { slots = []; cursor = 0 }
          export function useState(initial) {
            const index = cursor++
            if (!(index in slots)) slots[index] = typeof initial === 'function' ? initial() : initial
            return [slots[index], (next) => { slots[index] = typeof next === 'function' ? next(slots[index]) : next }]
          }
          export function useRef(initial) {
            const index = cursor++
            if (!(index in slots)) slots[index] = { current: initial }
            return slots[index]
          }
          export function useEffect() { cursor++ }
        `, loader: 'js' }))
        b.onResolve({ filter: /^@deepseek-ai\/dsh-client-ui-primitives$/ }, (args) => ({ path: args.path, namespace: 'stub' }))
        b.onResolve({ filter: /^\.\/(api|live)\.ts$/ }, (args) => ({ path: args.path.slice(2), namespace: 'stub' }))
        b.onLoad({ filter: /.*/, namespace: 'stub' }, (args) => {
          if (args.path === '@deepseek-ai/dsh-client-ui-primitives') return { contents: `
            import { createElement as h } from 'react'
            export const Button = ({ children, ...p }) => h('button', { type: 'button', ...p }, children)
            export const Input = (p) => h('input', p)
            export const StateDot = ({ state }) => h('span', { 'data-state': state })
          `, loader: 'js', resolveDir: here }
          if (args.path === 'api.ts') return { contents: `
            export const call = async (path, body) => {
              const api = globalThis.__nmMordeHubAPI
              api.calls.push({ path, body })
              return api.respond(path, body)
            }
            export const column = { display: 'flex', flexDirection: 'column', gap: 12 }
            export const row = { display: 'flex', gap: 8 }
            export const muted = { color: 'gray' }
          `, loader: 'js', resolveDir: here }
          return { contents: `export function useLive() { return globalThis.__nmMordeHubLive ?? { mordeHub: undefined } }`, loader: 'js', resolveDir: here }
        })
      },
    }],
  })
  bundle = await import(pathToFileURL(join(dir, 'card.mjs')).href)
})

after(async () => {
  if (previousWindow === undefined) delete globalThis.window
  else globalThis.window = previousWindow
  delete globalThis.__nmMordeHubLive
  delete globalThis.__nmMordeHubAPI
  if (dir) await rm(dir, { recursive: true, force: true })
})

function translator(lang) {
  const dict = bundle[lang]
  return (key, values) => values ? bundle.fill(dict[key], values) : dict[key]
}

function status(patch = {}) {
  return {
    signedIn: true,
    connected: true,
    baseURL: 'https://hub.example',
    accountName: 'muse-user',
    installationID: 'install-123',
    deviceID: 'i1234567',
    devices: [],
    expiresAt: '2027-01-02T03:04:05Z',
    targetGrantVersion: 4,
    grants: { screenshot: false, files: false, command: false, fileRoot: null },
    capabilities: { screenshot: true, files: true, command: false },
    ...patch,
  }
}

function children(node) {
  const value = node?.props?.children
  return Array.isArray(value) ? value : [value]
}

function find(node, testID) {
  if (Array.isArray(node)) {
    for (const item of node) { const found = find(item, testID); if (found) return found }
    return undefined
  }
  if (!node || typeof node !== 'object') return undefined
  if (node.props?.['data-testid'] === testID) return node
  return find(children(node), testID)
}

function findBy(node, predicate) {
  if (Array.isArray(node)) {
    for (const item of node) { const found = findBy(item, predicate); if (found) return found }
    return undefined
  }
  if (!node || typeof node !== 'object') return undefined
  if (predicate(node)) return node
  return findBy(children(node), predicate)
}

function textOf(node) {
  if (Array.isArray(node)) return node.map(textOf).join('')
  if (node === null || node === undefined || typeof node === 'boolean') return ''
  if (typeof node !== 'object') return String(node)
  return textOf(children(node))
}

function findButton(node, label) {
  return findBy(node, (item) => item.type === 'button' && textOf(item) === label)
}

function commandCheckbox(node, label = 'Command execution') {
  return findBy(node, (item) => item.type === 'input' && item.props['aria-label'] === label)
}

function setupAPI(initial) {
  let current = initial
  const api = {
    calls: [],
    async respond(path, body) {
      assert.equal(path, 'morde-hub/grants')
      const grants = { ...current.grants }
      if (typeof body.screenshot === 'boolean') grants.screenshot = body.screenshot
      if (typeof body.files === 'boolean') grants.files = body.files
      if (typeof body.command === 'boolean') grants.command = body.command
      if (typeof body.file_root === 'string') grants.fileRoot = body.file_root
      if (body.file_root === null) { grants.fileRoot = null; grants.files = false; grants.command = false }
      current = { ...current, grants, targetGrantVersion: current.targetGrantVersion + 1 }
      return current
    },
  }
  globalThis.__nmMordeHubAPI = api
  return api
}

const settle = () => new Promise((resolve) => setImmediate(resolve))

test('signed out: shows only the independent Hub login', () => {
  delete globalThis.window
  const markup = bundle.renderCard(translator('en'), undefined, true)
  assert.match(textOf(markup), /Sign in to a Morde Client Installation/)
  assert.ok(findBy(markup, (node) => node.type === 'input' && node.props.placeholder === 'Username'))
  assert.equal(findBy(markup, (node) => node.type === 'input' && node.props.placeholder === 'Password')?.props.type, 'password')
  assert.equal(find(markup, 'nm-morde-hub-grants'), undefined)
})

test('signed in: shows installation details and the current grants', () => {
  delete globalThis.window
  const markup = bundle.renderCard(translator('zh'), status({ grants: { screenshot: true, files: true, command: false, fileRoot: '/Users/muse/Documents' } }), true)
  assert.match(textOf(markup), /在线/)
  assert.match(textOf(markup), /https:\/\/hub\.example/)
  assert.match(textOf(markup), /install-123/)
  assert.match(textOf(markup), /断开连接/)
  assert.match(textOf(markup), /已选择的工作目录：\/Users\/muse\/Documents/)
  assert.equal(findBy(markup, (node) => node.type === 'input' && node.props['aria-label'] === '共享屏幕截图').props.checked, true)
  assert.equal(findBy(markup, (node) => node.type === 'input' && node.props.type === 'password'), undefined)
  assert.equal(commandCheckbox(markup, '执行命令').props.disabled, true)
})

test('workspace selection is independent from file and command grants', async () => {
  globalThis.window = { nanomuseHarness: { chooseDirectory: async () => '/Users/muse/Projects' } }
  const initial = status({ capabilities: { screenshot: true, files: true, command: true } })
  const api = setupAPI(initial)
  const t = translator('en')
  let markup = bundle.renderCard(t, initial, true)
  findBy(markup, (node) => node.type === 'input' && node.props['aria-label'] === 'Share screenshots').props.onChange({ currentTarget: { checked: true } })
  await settle()
  assert.deepEqual(api.calls[0], { path: 'morde-hub/grants', body: { screenshot: true } })

  markup = bundle.renderCard(t, undefined)
  findButton(markup, 'Choose folder').props.onClick()
  await settle()
  assert.deepEqual(api.calls[1], { path: 'morde-hub/grants', body: { file_root: '/Users/muse/Projects' } })
  markup = bundle.renderCard(t, undefined)
  assert.match(textOf(markup), /Selected workspace: \/Users\/muse\/Projects/)
  assert.equal(findBy(markup, (node) => node.type === 'input' && node.props['aria-label'] === 'Read files in the selected folder').props.checked, false)
  assert.equal(commandCheckbox(markup).props.checked, false)
  commandCheckbox(markup).props.onChange({ currentTarget: { checked: true } })
  await settle()
  assert.deepEqual(api.calls[2], { path: 'morde-hub/grants', body: { command: true } })
  markup = bundle.renderCard(t, undefined)
  assert.equal(commandCheckbox(markup).props.checked, true)
})

test('missing picker shows a disabled file control and a reason', () => {
  delete globalThis.window
  const markup = bundle.renderCard(translator('en'), status(), true)
  assert.equal(findButton(markup, 'Choose folder').props.disabled, true)
  assert.match(textOf(find(markup, 'nm-morde-hub-files-unavailable')), /folder picker is unavailable/i)

  const unavailable = bundle.renderCard(translator('en'), status({ capabilities: { screenshot: false, files: false, command: false } }), true)
  assert.equal(findBy(unavailable, (node) => node.type === 'input' && node.props['aria-label'] === 'Share screenshots').props.disabled, true)
  assert.match(textOf(find(unavailable, 'nm-morde-hub-screenshot-unavailable')), /screenshot capture is unavailable/i)
  assert.equal(findButton(unavailable, 'Choose folder').props.disabled, true)
  assert.match(textOf(find(unavailable, 'nm-morde-hub-files-unavailable')), /file access is unavailable/i)
  assert.equal(commandCheckbox(unavailable).props.disabled, true)
  assert.match(textOf(find(unavailable, 'nm-morde-hub-command-unavailable')), /command isolation is unavailable/i)
})

test('file access can be revoked without clearing the selected workspace', async () => {
  delete globalThis.window
  const initial = status({ grants: { screenshot: false, files: true, command: false, fileRoot: '/Users/muse/Documents' } })
  const api = setupAPI(initial)
  const markup = bundle.renderCard(translator('zh'), initial, true)
  findBy(markup, (node) => node.type === 'input' && node.props['aria-label'] === '读取所选文件夹中的文件').props.onChange({ currentTarget: { checked: false } })
  await settle()
  assert.deepEqual(api.calls[0], { path: 'morde-hub/grants', body: { files: false } })
  const updated = bundle.renderCard(translator('zh'), undefined)
  assert.match(textOf(updated), /已选择的工作目录：\/Users\/muse\/Documents/)
})

test('clearing the selected workspace is explicit', async () => {
  delete globalThis.window
  const initial = status({ grants: { screenshot: false, files: false, command: true, fileRoot: '/Users/muse/Documents' } })
  const api = setupAPI(initial)
  const markup = bundle.renderCard(translator('en'), initial, true)
  findButton(markup, 'Clear workspace').props.onClick()
  await settle()
  assert.deepEqual(api.calls[0], { path: 'morde-hub/grants', body: { file_root: null } })
  const updated = bundle.renderCard(translator('en'), undefined)
  assert.doesNotMatch(textOf(updated), /Selected workspace:/)
  assert.equal(commandCheckbox(updated).props.checked, false)
})

test('command permission needs both a sandbox capability and a selected workspace', () => {
  delete globalThis.window
  let markup = bundle.renderCard(translator('en'), status({ capabilities: { screenshot: true, files: true, command: true } }), true)
  assert.equal(commandCheckbox(markup).props.disabled, true)
  markup = bundle.renderCard(translator('en'), status({
    grants: { screenshot: false, files: false, command: false, fileRoot: '/Users/muse/Projects' },
    capabilities: { screenshot: true, files: true, command: true },
  }), true)
  assert.equal(commandCheckbox(markup).props.disabled, false)
})

test('lists account installations and asks before revoking another device', async () => {
  const previousWindow = globalThis.window
  const devices = [
    { id: 'i1234567', name: 'Desk', platform: 'desktop', online: true, revoked: false, expiresAt: '2099-01-01T00:00:00Z' },
    { id: 'i7654321', name: 'Phone', platform: 'ios', online: false, revoked: false, expiresAt: '2099-01-01T00:00:00Z' },
    { id: 'i2345678', name: 'Old phone', platform: 'ios', online: false, revoked: true, expiresAt: '2099-01-01T00:00:00Z' },
  ]
  const current = status({ devices })
  const api = {
    calls: [],
    async respond(path, body) {
      assert.equal(path, 'morde-hub/revoke-device')
      assert.deepEqual(body, { installation_id: 'i7654321' })
      current.devices[1] = { ...current.devices[1], revoked: true }
      return current
    },
  }
  globalThis.window = { confirm: (prompt) => prompt.includes('Phone') }
  globalThis.__nmMordeHubAPI = api
  try {
    let markup = bundle.renderCard(translator('en'), current, true)
    assert.match(textOf(find(markup, 'nm-morde-hub-devices')), /Desk.*This computer.*Phone.*Offline.*Old phone.*Revoked/)
    const revoke = findButton(markup, 'Revoke')
    revoke.props.onClick()
    await settle()
    assert.deepEqual(api.calls, [{ path: 'morde-hub/revoke-device', body: { installation_id: 'i7654321' } }])
    markup = bundle.renderCard(translator('en'), undefined)
    assert.match(textOf(find(markup, 'nm-morde-hub-device-i7654321')), /Revoked/)
    assert.equal(findButton(find(markup, 'nm-morde-hub-device-i2345678'), 'Revoke'), undefined)
  } finally {
    if (previousWindow === undefined) delete globalThis.window
    else globalThis.window = previousWindow
    delete globalThis.__nmMordeHubAPI
  }
})
