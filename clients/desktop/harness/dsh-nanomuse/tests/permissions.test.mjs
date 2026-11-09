import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { chmod, mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { delimiter, join } from 'node:path'
import { test } from 'node:test'
import { checkMove, checkScreenshot, isBlack, needsAccessibility, runtimeInfo, windowStatus, withMcp } from '../lib/hands-check.js'

/**
 * A stand-in for `nanomuse mcp`: MCP over stdio, one JSON-RPC message per line, with the two
 * tools the checks call. `FAKE_MODE` picks how it behaves.
 */
const FAKE = String.raw`
const mode = process.env.FAKE_MODE || 'ok'
if (mode === 'dead') { process.stderr.write('Traceback: ModuleNotFoundError: no module named pyautogui\n'); process.exit(3) }
let buf = ''
const moves = []
process.stdin.setEncoding('utf8')
process.stdin.on('data', (c) => {
  buf += c
  let i
  while ((i = buf.indexOf('\n')) >= 0) {
    const line = buf.slice(0, i); buf = buf.slice(i + 1)
    if (!line.trim()) continue
    const msg = JSON.parse(line)
    if (msg.method === 'notifications/initialized') continue
    if (mode === 'silent') continue
    let result
    if (msg.method === 'initialize') result = { protocolVersion: '2024-11-05', capabilities: { tools: {} }, serverInfo: { name: 'fake', version: '0' } }
    else if (msg.method === 'tools/call') {
      const { name, arguments: args } = msg.params
      if (name === 'computer_screen') {
        if (mode === 'black') result = { isError: true, content: [{ type: 'text', text: 'the screenshot came back all black. On macOS, allow Screen Recording for nanoMuse Desktop, then relaunch.' }] }
        else result = { content: [{ type: 'text', text: 'Finder · 1440×900 · keyboard hidden\nthe desktop' }, { type: 'image', data: 'AAAA', mimeType: 'image/jpeg' }] }
      } else if (name === 'computer_act' && args.action === 'computer_target') {
        result = { content: [{ type: 'text', text: 'Finder · 1440×900\nWindow mode is not available here (window mode is macOS only; the hands use the shared screen here); the hands work on the whole screen, with Finder as the application they are about. Screen now:' }] }
      } else if (name === 'computer_act') {
        moves.push([args.x, args.y])
        if (mode === 'noaccess') result = { isError: true, content: [{ type: 'text', text: 'move failed: the hands have no Accessibility permission (AXIsProcessTrusted is false)' }] }
        else result = { content: [{ type: 'text', text: 'Finder · 1440×900\nmoved to ' + args.x + ',' + args.y + ' (' + moves.length + ')' }] }
      } else result = { isError: true, content: [{ type: 'text', text: 'unknown tool ' + name }] }
    } else result = {}
    process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id: msg.id, result }) + '\n')
  }
})
process.stdin.on('end', () => process.exit(0))
`

function fakeSpawn(mode) {
  return (command, args, options) => spawn(process.execPath, ['-e', FAKE], { ...options, env: { ...options.env, FAKE_MODE: mode } })
}

const OPTIONS = (mode, timeoutMs = 5000) => ({ command: 'nanomuse', args: ['mcp'], spawn: fakeSpawn(mode), timeoutMs })

test('checkScreenshot: a thumbnail, the size and the window in front', async () => {
  const r = await checkScreenshot(OPTIONS('ok'))
  assert.equal(r.ok, true)
  assert.equal(r.black, false)
  assert.equal(r.thumbnail, 'data:image/jpeg;base64,AAAA')
  assert.equal(r.width, 1440)
  assert.equal(r.height, 900)
  assert.equal(r.title, 'Finder')
  assert.deepEqual(r.window, { available: false, reason: 'window mode is macOS only; the hands use the shared screen here' })
})

test('windowStatus reads the runtime’s note', () => {
  assert.deepEqual(windowStatus('Working in the window of Finder — Desktop. Coordinates are pixels'), { available: true, reason: '' })
  assert.deepEqual(windowStatus('Finder could not be worked in as a window (the window of Finder came back empty: allow Screen Recording); showing the whole screen instead.'), {
    available: true,
    reason: 'the window of Finder came back empty: allow Screen Recording',
  })
  assert.deepEqual(windowStatus("Window mode is not available here ([hands] mode is 'screen'); the hands work"), { available: false, reason: "[hands] mode is 'screen'" })
})

test('checkScreenshot: an all-black capture names Screen Recording', async () => {
  const r = await checkScreenshot(OPTIONS('black'))
  assert.equal(r.ok, false)
  assert.equal(r.black, true)
  assert.match(r.error, /all black/)
})

test('checkMove: 20 px right of the centre and back', async () => {
  const r = await checkMove(OPTIONS('ok'))
  assert.deepEqual(r, { ok: true, accessibility: false })
  // the fake records the two moves; read them back through a session of our own
  const moves = await withMcp(OPTIONS('ok'), async (call) => {
    await call('computer_act', { action: 'move', x: 740, y: 450 })
    const back = await call('computer_act', { action: 'move', x: 720, y: 450 })
    return back.content[0].text
  })
  assert.match(moves, /moved to 720,450 \(2\)/)
})

test('checkMove: a refused move names Accessibility', async () => {
  const r = await checkMove(OPTIONS('noaccess'))
  assert.equal(r.ok, false)
  assert.equal(r.accessibility, true)
  assert.match(r.error, /Accessibility/)
})

test('a runtime that dies or never answers is reported, not hung', async () => {
  const dead = await checkScreenshot(OPTIONS('dead'))
  assert.equal(dead.ok, false)
  assert.match(dead.error, /the runtime stopped: Traceback: ModuleNotFoundError/)
  const silent = await checkMove(OPTIONS('silent', 300))
  assert.equal(silent.ok, false)
  assert.match(silent.error, /did not answer in time/)
})

test('a command that cannot start is reported', async () => {
  const r = await checkScreenshot({ command: join(tmpdir(), 'no-such-nanomuse-binary'), args: ['mcp'], timeoutMs: 2000 })
  assert.equal(r.ok, false)
  assert.match(r.error, /could not start the runtime|the runtime stopped/)
})

test('runtimeInfo: NANOMUSE_PY as set, PATH otherwise, loud about a wrong path', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'nm-rt-'))
  try {
    // Windows has no executable bit; there the name decides (nanomuse.cmd is on the lookup list)
    const bin = join(dir, process.platform === 'win32' ? 'nanomuse.cmd' : 'nanomuse')
    await writeFile(bin, process.platform === 'win32' ? '@exit /b 0\r\n' : '#!/bin/sh\nexit 0\n')
    await chmod(bin, 0o755)
    const plain = join(dir, 'notes.txt')
    await writeFile(plain, 'x')
    assert.deepEqual(await runtimeInfo({ NANOMUSE_PY: bin, PATH: '' }), { path: bin, source: 'env', ok: true })
    assert.deepEqual(await runtimeInfo({ NANOMUSE_PY: join(dir, 'gone'), PATH: dir }), { path: join(dir, 'gone'), source: 'env', ok: false, problem: 'missing' })
    assert.deepEqual(await runtimeInfo({ NANOMUSE_PY: plain, PATH: dir }), { path: plain, source: 'env', ok: false, problem: 'not-executable' })
    assert.deepEqual(await runtimeInfo({ PATH: `/nonexistent${delimiter}${dir}` }), { path: bin, source: 'path', ok: true })
    assert.deepEqual(await runtimeInfo({ PATH: '/nonexistent' }), { path: '', source: 'none', ok: false, problem: 'not-found' })
  } finally {
    await rm(dir, { recursive: true, force: true })
  }
})

test('displayInfo: Linux reads the session the way the operator and the runtime do; nothing elsewhere', async () => {
  const { displayInfo, WAYLAND_TEXT } = await import('../lib/hands-check.js')
  assert.deepEqual(displayInfo({ DISPLAY: ':0', XDG_SESSION_TYPE: 'x11' }, 'linux'), { session: 'x11', reason: '' })
  assert.deepEqual(displayInfo({ DISPLAY: ':0', XDG_SESSION_TYPE: 'wayland', WAYLAND_DISPLAY: 'wayland-0' }, 'linux'), { session: 'wayland', reason: WAYLAND_TEXT })
  // a compositor started by hand: WAYLAND_DISPLAY without a DISPLAY counts too; with XWayland's DISPLAY it is X11 for the hands
  assert.equal(displayInfo({ WAYLAND_DISPLAY: 'wayland-1' }, 'linux').session, 'wayland')
  assert.equal(displayInfo({ WAYLAND_DISPLAY: 'wayland-1', DISPLAY: ':1' }, 'linux').session, 'x11')
  assert.equal(displayInfo({}, 'linux').session, 'none')
  assert.match(WAYLAND_TEXT, /^Wayland session: .*Log in with Xorg/)
  assert.equal(displayInfo({ DISPLAY: ':0' }, 'darwin'), undefined)
  assert.equal(displayInfo({ XDG_SESSION_TYPE: 'wayland' }, 'win32'), undefined)
})

test('the words that mean "grant Screen Recording" and "grant Accessibility"', () => {
  assert.ok(isBlack('the screenshot came back all black. On macOS, allow Screen Recording'))
  assert.ok(isBlack('the window of Finder came back empty: allow Screen Recording for nanoMuse Desktop'))
  assert.ok(!isBlack('the hands did not finish click in time'))
  assert.ok(needsAccessibility('CGEventPostToPid refused: the process is not trusted'))
  assert.ok(needsAccessibility('no hands backend: install xdotool'))
  assert.ok(!needsAccessibility('the screenshot came back all black'))
})
