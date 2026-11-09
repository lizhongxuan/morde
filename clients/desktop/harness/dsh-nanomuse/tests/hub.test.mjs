// The hub client against a fake socket: hello with the key, welcome, calls and
// their results/events/errors, incoming calls answered, the device list,
// reconnect after a drop, and a refused key.
import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { createRequire } from 'node:module'
import { test } from 'node:test'
import { helloLlm, HubClient, HubError } from '../lib/hub.js'

const { WebSocketServer } = createRequire(import.meta.url)('ws')

const OPEN = 1

/** A socket the test drives: `sent` collects frames, `push` delivers frames, `drop` closes. */
class FakeSocket {
  constructor(url) {
    this.url = url
    this.readyState = 0
    this.sent = []
    this.listeners = { open: [], message: [], close: [], error: [] }
  }
  addEventListener(type, fn) {
    this.listeners[type].push(fn)
  }
  send(data) {
    this.sent.push(JSON.parse(data))
  }
  close(code = 1000, reason = '') {
    if (this.readyState === 3) return
    this.readyState = 3
    for (const fn of this.listeners.close) fn({ code, reason })
  }
  open() {
    this.readyState = OPEN
    for (const fn of this.listeners.open) fn({})
  }
  push(frame) {
    for (const fn of this.listeners.message) fn({ data: JSON.stringify(frame) })
  }
  drop(code = 1006, reason = '') {
    this.readyState = 3
    for (const fn of this.listeners.close) fn({ code, reason })
  }
}

const DEVICE = { id: 'pc-dsh-test', name: 'Desk', kind: 'computer', os: 'Linux', version: 'test', actions: ['info', 'notify'] }

function harness({ key = 'nm_test', pingMs = 100_000 } = {}) {
  const sockets = []
  const client = new HubClient({
    url: 'ws://relay/v1/hub',
    key: async () => key,
    device: () => DEVICE,
    socket: (url) => {
      const s = new FakeSocket(url)
      sockets.push(s)
      return s
    },
    backoffMs: { min: 5, max: 20 },
    pingMs,
  })
  return { client, sockets }
}

const tick = () => new Promise((r) => setTimeout(r, 0))
const wait = (ms) => new Promise((r) => setTimeout(r, ms))

async function connected(h) {
  h.client.start()
  await tick()
  const s = h.sockets.at(-1)
  s.open()
  assert.deepEqual(s.sent[0], { type: 'hello', key: 'nm_test', device: DEVICE })
  s.push({ type: 'welcome', device_id: DEVICE.id, devices: [{ id: DEVICE.id, name: 'Desk', kind: 'computer', online: true }, { id: 'phone-1', name: 'Phone', kind: 'phone', online: true, controllable: true }], server: {} })
  assert.equal(h.client.connected, true)
  assert.equal(h.client.deviceId, DEVICE.id)
  return s
}

test('hello carries the key; welcome brings the device list', async () => {
  const h = harness()
  const s = await connected(h)
  assert.equal(h.client.devices.length, 2)
  assert.equal(h.client.find('phone')?.id, 'phone-1')
  assert.equal(h.client.find('PHONE')?.id, 'phone-1')
  assert.equal(h.client.find('')?.id, 'phone-1', 'the only other device')
  assert.equal(h.client.find('tablet'), undefined)
  h.client.stop()
  assert.equal(s.readyState, 3)
  assert.equal(h.client.connected, false)
})

test('Morde bearer mode authenticates the socket header and sends a credential-free hello', async () => {
  const server = createServer()
  const wss = new WebSocketServer({ noServer: true })
  let authorization
  let hello
  server.on('upgrade', (request, socket, head) => {
    authorization = request.headers.authorization
    wss.handleUpgrade(request, socket, head, (connection) => wss.emit('connection', connection, request))
  })
  wss.on('connection', (connection) => {
    connection.once('message', (data) => {
      hello = JSON.parse(String(data))
      connection.send(JSON.stringify({ type: 'welcome', device_id: 'i1234567', devices: [] }))
    })
  })
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  const address = server.address()
  const client = new HubClient({
    url: `ws://127.0.0.1:${address.port}/api/hub`,
    auth: 'bearer',
    key: async () => 'chc_test-secret',
    pingMs: 100_000,
    backoffMs: { min: 5, max: 20 },
  })
  const connectedState = new Promise((resolve) => client.onState(() => { if (client.connected) resolve() }))
  client.start()
  try {
    await Promise.race([connectedState, wait(2_000).then(() => { throw new Error('Morde Hub connection timed out') })])
    assert.equal(authorization, 'Bearer chc_test-secret')
    assert.deepEqual(hello, { type: 'hello' })
    assert.equal(client.deviceId, 'i1234567')
  } finally {
    client.stop()
    wss.close()
    await new Promise((resolve) => server.close(resolve))
  }
})

test('Morde bearer mode stops reconnecting after an HTTP authorization refusal', async () => {
  let requests = 0
  const server = createServer((_request, response) => {
    requests += 1
    response.writeHead(401).end()
  })
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  const address = server.address()
  const client = new HubClient({
    url: `ws://127.0.0.1:${address.port}/api/hub`,
    auth: 'bearer',
    key: async () => 'chc_revoked-secret',
    backoffMs: { min: 5, max: 20 },
  })
  client.start()
  try {
    await wait(50)
    assert.equal(client.lastError, 'the service refused the credential')
    assert.equal(requests, 1)
    await wait(50)
    assert.equal(requests, 1, 'an unauthorized installation does not retry in a loop')
  } finally {
    client.stop()
    await new Promise((resolve) => server.close(resolve))
  }
})

test('a call resolves with the result body, sees events, and fails on an error frame', async () => {
  const h = harness()
  const s = await connected(h)
  const events = []
  const p = h.client.call('phone-1', 'task', { text: 'hi' }, { onEvent: (b) => events.push(b), timeoutMs: 1000 })
  const sent = s.sent.at(-1)
  assert.equal(sent.type, 'call')
  assert.equal(sent.to, 'phone-1')
  assert.equal(sent.action, 'task')
  s.push({ type: 'event', id: sent.id, body: { stage: 'tool', name: 'screen' } })
  s.push({ type: 'result', id: sent.id, ok: true, body: { text: 'done' } })
  assert.deepEqual(await p, { text: 'done' })
  assert.deepEqual(events, [{ stage: 'tool', name: 'screen' }])

  const p2 = h.client.call('phone-1', 'screen', {}, { timeoutMs: 1000 })
  const id2 = s.sent.at(-1).id
  s.push({ type: 'error', id: id2, code: 'device_offline', message: 'gone' })
  await assert.rejects(p2, (e) => e instanceof HubError && e.code === 'device_offline')

  const p3 = h.client.call('phone-1', 'shell', { command: 'ls' }, { timeoutMs: 1000 })
  const id3 = s.sent.at(-1).id
  s.push({ type: 'result', id: id3, ok: false, error: 'denied', message: 'no' })
  await assert.rejects(p3, (e) => e instanceof HubError && e.code === 'denied' && e.message === 'no')

  const p4 = h.client.call('phone-1', 'info', {}, { timeoutMs: 10 })
  const id4 = s.sent.at(-1).id
  await assert.rejects(p4, (e) => e instanceof HubError && e.code === 'timeout')
  assert.deepEqual(s.sent.at(-1), { type: 'cancel', id: id4 }, 'a local timeout also cancels the remote Hub call')

  // A caller may pick the id up front, so it can ask the device to `stop` that very call.
  const id5 = h.client.nextId()
  const p5 = h.client.call('phone-1', 'task', { text: 'later' }, { id: id5, timeoutMs: 1000 })
  assert.equal(s.sent.at(-1).id, id5)
  s.push({ type: 'result', id: id5, ok: true, body: { text: 'ok' } })
  assert.deepEqual(await p5, { text: 'ok' })

  const controller = new AbortController()
  const p6 = h.client.call('phone-1', 'open', { url: 'https://example.com' }, { signal: controller.signal, timeoutMs: 1000 })
  const id6 = s.sent.at(-1).id
  controller.abort()
  await assert.rejects(p6, (e) => e instanceof HubError && e.code === 'aborted')
  assert.deepEqual(s.sent.at(-1), { type: 'cancel', id: id6 }, 'an abort also cancels the remote Hub call')

  const alreadyAborted = new AbortController()
  alreadyAborted.abort()
  const sentBefore = s.sent.length
  await assert.rejects(h.client.call('phone-1', 'open', { url: 'https://example.com' }, { signal: alreadyAborted.signal }), (e) => e instanceof HubError && e.code === 'aborted')
  assert.equal(s.sent.length, sentBefore, 'a call cancelled before dispatch is never sent')
  h.client.stop()
})

test('an incoming call is answered by its handler; unknown actions are refused', async () => {
  const h = harness()
  const s = await connected(h)
  let ordinaryCaller
  h.client.handle('notify', async (args, call) => {
    ordinaryCaller = call.from
    call.event({ stage: 'shown' })
    return { ok: true, text: args.text }
  })
  s.push({ type: 'call', id: 'c1', from: { id: 'phone-1', name: 'Phone', kind: 'phone' }, action: 'notify', args: { text: 'dinner' } })
  await tick()
  assert.deepEqual(ordinaryCaller, { id: 'phone-1', name: 'Phone', kind: 'phone' })
  assert.deepEqual(s.sent.at(-2), { type: 'event', id: 'c1', body: { stage: 'shown' } })
  assert.deepEqual(s.sent.at(-1), { type: 'result', id: 'c1', ok: true, body: { ok: true, text: 'dinner' } })
  s.push({ type: 'call', id: 'c2', from: { id: 'phone-1', name: 'Phone', kind: 'phone' }, action: 'shell', args: {} })
  await tick()
  assert.equal(s.sent.at(-1).ok, false)
  assert.equal(s.sent.at(-1).error, 'unknown_action')
  h.client.stop()
})

test('cancel and disconnect abort an incoming action handler', async () => {
  const h = harness()
  const s = await connected(h)
  const started = []
  h.client.handle('long', async (_args, call) => {
    started.push({ id: call.id, signal: call.signal })
    await new Promise((resolve) => call.signal.addEventListener('abort', resolve, { once: true }))
    throw call.signal.reason
  })

  s.push({ type: 'call', id: 'cancel-me', from: { id: 'server-task', name: 'Task', kind: 'task' }, action: 'long', args: {} })
  assert.equal(started[0].signal.aborted, false)
  s.push({ type: 'cancel', id: 'cancel-me' })
  await tick()
  assert.equal(started[0].signal.aborted, true)
  assert.deepEqual(s.sent.at(-1), {
    type: 'result', id: 'cancel-me', ok: false, error: 'cancelled', message: 'The Hub action was cancelled',
  })

  s.push({ type: 'call', id: 'disconnect-me', from: { id: 'server-task', name: 'Task', kind: 'task' }, action: 'long', args: {} })
  assert.equal(started[1].signal.aborted, false)
  s.drop(1006, 'network')
  assert.equal(started[1].signal.aborted, true)
  h.client.stop()
})

test('an incoming Task caller is trusted only with a complete valid Task and target grant fence', async () => {
  const h = harness()
  const s = await connected(h)
  const callers = []
  h.client.handle('command', async (_args, call) => {
    callers.push(call.from)
    return { accepted: true }
  })
  const base = { id: 'server-task', name: 'Morde Task', kind: 'task' }
  s.push({
    type: 'call', id: 'c1', from: {
      ...base, task_id: 'q1234567', authorization_version: 3, target_grant_version: 2, operation_id: 'op_abcd-1234',
    }, action: 'command', args: {},
  })
  await tick()
  assert.deepEqual(callers[0], {
    ...base, taskContext: {
      taskID: 'q1234567', authorizationVersion: 3, targetGrantVersion: 2, operationID: 'op_abcd-1234',
    },
  })

  s.push({
    type: 'call', id: 'c2', from: {
      ...base, task_id: 'q1234567', authorization_version: 3, operation_id: 'op_abcd-1234',
    }, action: 'command', args: {},
  })
  await tick()
  assert.deepEqual(callers[1], base, 'a missing target grant version discards the whole Task context')

  s.push({
    type: 'call', id: 'c3', from: {
      ...base, task_id: 'q1234567', authorization_version: 3, target_grant_version: 2, operation_id: 'op/unsafe',
    }, action: 'command', args: {},
  })
  await tick()
  assert.deepEqual(callers[2], base, 'an invalid operation id cannot leave a partial Task context')

  s.push({
    type: 'call', id: 'c4', from: {
      ...base, kind: 'desktop', task_id: 'q1234567', authorization_version: 3, target_grant_version: 2, operation_id: 'op_abcd-1234',
    }, action: 'command', args: {},
  })
  await tick()
  assert.deepEqual(callers[3], { ...base, kind: 'desktop' }, 'a device caller cannot claim Task metadata')
  h.client.stop()
})

test('a dropped socket reconnects; a refused key stops and reports', async () => {
  const h = harness()
  let states = 0
  let unauthorized = 0
  h.client.onState(() => states++)
  h.client.onUnauthorized(() => unauthorized++)
  const s = await connected(h)
  const pending = h.client.call('phone-1', 'info', {}, { timeoutMs: 1000 })
  s.drop(1006, 'network')
  await assert.rejects(pending, (e) => e instanceof HubError && e.code === 'disconnected')
  assert.equal(h.client.connected, false)
  await wait(30)
  assert.equal(h.sockets.length, 2, 'a new socket was opened')
  const s2 = h.sockets[1]
  s2.open()
  s2.push({ type: 'welcome', device_id: DEVICE.id, devices: [] })
  assert.equal(h.client.connected, true)
  s2.drop(4001, 'bad key')
  await tick()
  assert.equal(unauthorized, 1)
  assert.equal(h.client.connected, false)
  await wait(30)
  assert.equal(h.sockets.length, 2, 'no reconnect after a refused key')
  assert.ok(states >= 3)
})

test('without a key nothing connects until restart', async () => {
  let key
  const sockets = []
  const client = new HubClient({ url: 'ws://relay/v1/hub', key: async () => key, device: () => DEVICE, socket: (u) => { const s = new FakeSocket(u); sockets.push(s); return s }, backoffMs: { min: 5, max: 20 } })
  client.start()
  await tick()
  assert.equal(sockets.length, 0)
  key = 'nm_test'
  client.restart()
  await tick()
  assert.equal(sockets.length, 1)
  client.stop()
})

test('the sync frame carries the cursor and the device; the working frame (C9) reaches its listener as it came', async () => {
  const h = harness()
  const s = await connected(h)
  const syncs = []
  const working = []
  const offSync = h.client.onSync((cursor, from) => syncs.push([cursor, from]))
  const offWorking = h.client.onWorking((frame) => working.push(frame))
  s.push({ type: 'sync', what: 'conversations', cursor: 42, from: 'phone-1' })
  s.push({ type: 'sync', what: 'something-else', cursor: 43, from: 'phone-1' })
  s.push({ type: 'working', cid: 'c1', from: 'phone-1', device_name: 'Pixel 8', working: true, at: 1738000000 })
  s.push({ type: 'working', cid: 'c1', from: 'phone-1', device_name: 'Pixel 8', working: false, at: 1738000009 })
  s.push({ type: 'working', cid: 'c2' })
  assert.deepEqual(syncs, [[42, 'phone-1']])
  assert.deepEqual(working, [
    { cid: 'c1', from: 'phone-1', device_name: 'Pixel 8', working: true, at: 1738000000 },
    { cid: 'c1', from: 'phone-1', device_name: 'Pixel 8', working: false, at: 1738000009 },
    { cid: 'c2', from: '', device_name: '', working: false, at: 0 },
  ])
  offSync()
  offWorking()
  s.push({ type: 'working', cid: 'c3', from: 'phone-1', device_name: 'Pixel 8', working: true, at: 1 })
  assert.equal(working.length, 3)
  h.client.stop()
})

test('a restart while the key is still being read opens one socket, not two', async () => {
  // The key comes from the vault asynchronously; a sign-in's restart() landing in that
  // window used to leave the first attempt's socket open next to the new one.
  let release
  const gate = new Promise((r) => (release = r))
  const sockets = []
  const client = new HubClient({
    url: 'ws://relay/v1/hub',
    key: async () => {
      await gate
      return 'nm_test'
    },
    device: () => DEVICE,
    socket: (url) => {
      const s = new FakeSocket(url)
      sockets.push(s)
      return s
    },
    backoffMs: { min: 5, max: 20 },
    pingMs: 100_000,
  })
  client.start()
  await tick()
  client.restart()
  await tick()
  assert.equal(sockets.length, 0)
  release()
  await tick()
  await tick()
  assert.equal(sockets.length, 1, 'the attempt that lost the race opens nothing')
  client.stop()
})

test('a restart fails the calls in flight at once and is offline until the new welcome', async () => {
  const h = harness()
  const s1 = await connected(h)
  const states = []
  h.client.onState(() => states.push(h.client.connected))
  const inFlight = h.client.call('phone-1', 'info', {}, { timeoutMs: 60_000 })
  h.client.restart()
  await assert.rejects(inFlight, (e) => e instanceof HubError && e.code === 'disconnected')
  assert.equal(h.client.connected, false, 'not connected between the sockets')
  assert.deepEqual(states, [false])
  // a call made before the new socket is open is refused, not written into a closed socket
  await assert.rejects(h.client.call('phone-1', 'info', {}), (e) => e instanceof HubError && e.code === 'offline')
  await tick()
  const s2 = h.sockets.at(-1)
  assert.notEqual(s2, s1)
  assert.equal(s2.sent.length, 0, 'nothing is sent before the socket opens')
  s2.open()
  s2.push({ type: 'welcome', device_id: DEVICE.id, devices: [], server: {} })
  assert.equal(h.client.connected, true)
  assert.deepEqual(states, [false, true])
  h.client.stop()
})

test('hub_paused waits well beyond the normal back-off; a refused device stays closed without signing out', async () => {
  const h = harness()
  const s = await connected(h)
  s.drop(4003, 'hub_paused')
  assert.equal(h.client.lastError, 'the hub is paused on the relay')
  await wait(60)
  assert.equal(h.sockets.length, 1, 'no reconnect within the normal back-off')
  h.client.stop()

  const h2 = harness()
  let unauthorized = 0
  h2.client.onUnauthorized(() => (unauthorized += 1))
  const s2 = await connected(h2)
  s2.drop(4002, 'bad device')
  await wait(60)
  assert.equal(h2.sockets.length, 1, 'the same device again would be refused again')
  assert.equal(h2.client.lastError, 'the relay refused this device')
  assert.equal(unauthorized, 0, 'a refused device is not a refused key')
  h2.client.restart()
  await tick()
  assert.equal(h2.sockets.length, 2, 'a restart (a new sign-in, a rename) tries again')
  h2.client.stop()
})

test('a ping is sent on the interval; a relay that answers nothing for two pings is dropped and the client reconnects', async () => {
  const h = harness({ pingMs: 40 })
  const s = await connected(h)
  const states = []
  h.client.onState(() => states.push(h.client.connected))
  await wait(60)
  assert.equal(s.sent.filter((f) => f.type === 'ping').length, 1, 'the first ping went out')
  s.push({ type: 'pong' })
  await wait(40)
  assert.equal(s.sent.filter((f) => f.type === 'ping').length, 2, 'a pong keeps the socket; the second ping went out')
  assert.equal(h.client.connected, true)
  // nothing answers the second ping: a call made meanwhile fails when the socket is dropped
  const failed = h.client.call('phone-1', 'screen', {}, { timeoutMs: 5000 }).then(() => undefined, (e) => e)
  await wait(40)
  assert.equal(h.client.connected, false, 'two pings without a frame back: the link is dead')
  assert.equal(s.readyState, 3, 'the silent socket was closed by the client')
  assert.equal(h.client.lastError, 'the relay stopped answering')
  const e = await failed
  assert.ok(e instanceof HubError && e.code === 'disconnected', 'the call in flight failed at once')
  await wait(40)
  assert.equal(h.sockets.length, 2, 'the usual reconnect followed')
  const s2 = h.sockets.at(-1)
  s2.open()
  s2.push({ type: 'welcome', device_id: DEVICE.id, devices: [], server: {} })
  assert.equal(h.client.connected, true)
  assert.deepEqual(states, [false, true])
  h.client.stop()
})

test('hello names the model only as two plain ids; a key, a URL or a sentence is left out', () => {
  assert.deepEqual(helloLlm('cloud', 'Qwen/Qwen3.6-27B-FP8'), { provider: 'cloud', model: 'Qwen/Qwen3.6-27B-FP8' })
  assert.deepEqual(helloLlm('openrouter', 'anthropic/claude-sonnet-4.5:beta'), { provider: 'openrouter', model: 'anthropic/claude-sonnet-4.5:beta' })
  assert.equal(helloLlm('', 'gpt-5'), undefined)
  assert.equal(helloLlm('custom', ''), undefined)
  assert.equal(helloLlm('https://api.example.com/v1', 'gpt-5'), undefined)
  assert.equal(helloLlm('custom', 'sk-abcdefghijklmnopqrstuvwxyz0123456789 keep this secret'), undefined)
  assert.equal(helloLlm('custom', 'm'.repeat(81)), undefined)
})
