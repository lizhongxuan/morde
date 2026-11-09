import assert from 'node:assert/strict'
import { chmod, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { Readable } from 'node:stream'
import { test } from 'node:test'
import NanomuseCloud, { API_PREFIX, MORDE_CLIENT_CREDENTIAL, TOKEN_REF } from '../lib/cloud.js'
import { credentialRef } from '@deepseek-ai/dsh-credentials'
import { HubClient } from '../lib/hub.js'

function fakeContext() {
  const credentials = new Map()
  const ctx = {
    reflect: { provide() {} },
    logger: { info() {}, warn() {}, debug() {}, error() {} },
    credentials: {
      async set(ref, value) { credentials.set(String(ref), value) },
      async resolve(ref) { return credentials.has(String(ref)) ? { value: credentials.get(String(ref)) } : undefined },
      async unset(ref) { credentials.delete(String(ref)) },
    },
    settings: { async update() {}, async mutate() {}, describe() { return [] } },
    get() { return undefined },
    inject() {},
    effect(fn) { fn() },
    on() { return () => undefined },
  }
  return { ctx, credentials }
}

function response(body, status = 200) {
  return new Response(body === undefined ? null : JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } })
}

async function service() {
  const home = await mkdtemp(join(tmpdir(), 'nm-morde-hub-'))
  const statePath = join(home, 'cloud.json')
  const fake = fakeContext()
  const svc = new NanomuseCloud(fake.ctx, { baseURL: 'https://cloud.nanomuse.invalid', deviceName: 'Desk', statePath })
  svc.state = {}
  svc.startMordeHub = () => undefined
  svc.broadcast = () => undefined
  return { ...fake, svc, statePath, done: () => rm(home, { recursive: true, force: true }) }
}

async function call(svc, method, route, body) {
  const req = Readable.from(body === undefined ? [] : [Buffer.from(JSON.stringify(body))])
  req.method = method
  req.url = `${API_PREFIX}${route}`
  req.headers = {}
  const res = {
    status: 0,
    body: undefined,
    writeHead(status) { res.status = status; return res },
    end(text) { res.body = text ? JSON.parse(text) : undefined; return res },
  }
  await svc.handle(req, res)
  return res
}

test('Morde login gates on capabilities and rejects public plaintext HTTP before sending a password', async () => {
  const out = await service()
  const originalFetch = globalThis.fetch
  const requests = []
  globalThis.fetch = async (input, init) => {
    requests.push({ url: String(input), init })
    return response({ supported_features: ['password_login', 'client_login', 'hub'] })
  }
  try {
    await assert.rejects(out.svc.mordeHubLogin('http://example.com', 'alice', 'password'), /HTTPS/)
    assert.equal(requests.length, 0)
    await assert.rejects(out.svc.mordeHubLogin('https://morde.example', 'alice', 'password'), (error) => error.code === 'hub_unavailable')
    assert.equal(requests.length, 1)
    assert.match(requests[0].url, /\/api\/capabilities$/)
    assert.equal(requests.some((request) => request.init?.body?.includes('password')), false)
  } finally {
    globalThis.fetch = originalFetch
    await out.done()
  }
})

test('Morde login persists ordered attempts, stores only the Client credential, and revokes independently', async () => {
  const out = await service()
  const originalFetch = globalThis.fetch
  const attempts = []
  let loginCount = 0
  globalThis.fetch = async (input, init = {}) => {
    const url = String(input)
    if (url.endsWith('/api/devices')) return response({ items: [{ id: 'i1234567', display_name: 'Desk', platform: 'desktop', expires_at: '2030-01-01T00:00:00Z', revoked: false, created_at: '2026-01-01T00:00:00Z', last_used_at: null, online: true }] })
    if (url.endsWith('/api/capabilities')) return response({ supported_features: ['ordered_client_login', 'hub'] })
    if (url.endsWith('/api/auth/client-login')) {
      const body = JSON.parse(init.body)
      const saved = JSON.parse(await readFile(out.statePath, 'utf8')).mordeHub
      assert.equal(saved.attemptGeneration, body.attempt_generation, 'generation was persisted before the request')
      assert.equal(saved.clientInstanceID, body.client_instance_id)
      assert.equal(JSON.stringify(saved).includes('never-persist-this-password'), false)
      assert.equal(init.redirect, 'error')
      attempts.push({ body, idempotencyKey: init.headers['idempotency-key'] })
      loginCount += 1
      if (loginCount === 1) return response({ error: { code: 'upstream_error', message: 'try again' } }, 503)
      return response({
        installation_id: 'i1234567',
        client_credential: 'chc_client-secret',
        client_expires_at: '2030-01-01T00:00:00Z',
        user: { id: 'u1234567', name: 'Alice' },
      })
    }
    if (url.endsWith('/api/auth/client-logout')) {
      assert.equal(init.headers.authorization, 'Bearer chc_client-secret')
      assert.equal(init.redirect, 'error')
      return response(undefined, 204)
    }
    throw new Error(`unexpected local request: ${url}`)
  }
  try {
    await out.credentials.set(credentialRef(TOKEN_REF), 'nanoMuse-cloud-key')
    out.svc.mordeHubDeviceError = 'previous installation was revoked'
    await assert.rejects(out.svc.mordeHubLogin('http://127.0.0.1:8060', 'alice', 'never-persist-this-password'), /try again/)
    const signedIn = await out.svc.mordeHubLogin('http://127.0.0.1:8060', 'alice', 'never-persist-this-password')
    assert.equal(attempts[0].body.attempt_generation, 1)
    assert.equal(attempts[1].body.attempt_generation, 2)
    assert.equal(attempts[0].body.client_instance_id, attempts[1].body.client_instance_id)
    assert.notEqual(attempts[0].idempotencyKey, attempts[1].idempotencyKey)
    assert.equal(attempts[1].body.platform, 'desktop')
    assert.equal(attempts[1].body.display_name, 'Desk')
    assert.equal(signedIn.signedIn, true)
    assert.equal(signedIn.installationID, 'i1234567')
    assert.equal(signedIn.lastError, undefined)
    assert.equal(await out.svc.credential(MORDE_CLIENT_CREDENTIAL), 'chc_client-secret')
    assert.equal(await out.svc.token(), 'nanoMuse-cloud-key')
    const saved = JSON.parse(await readFile(out.statePath, 'utf8'))
    assert.equal(JSON.stringify(saved).includes('never-persist-this-password'), false)

    const response = await call(out.svc, 'GET', '/morde-hub/status')
    assert.equal(response.status, 200)
    assert.equal(response.body.signedIn, true)
    assert.equal(response.body.deviceID, 'i1234567')
    assert.deepEqual(response.body.devices.map(({ id, name, online }) => ({ id, name, online })), [{ id: 'i1234567', name: 'Desk', online: true }])
    const signedOut = await out.svc.mordeHubSignOut()
    assert.equal(signedOut.signedIn, false)
    assert.equal(await out.svc.credential(MORDE_CLIENT_CREDENTIAL), '')
    assert.equal(await out.svc.token(), 'nanoMuse-cloud-key')
    assert.equal('mordeHub' in JSON.parse(await readFile(out.statePath, 'utf8')), false)
  } finally {
    globalThis.fetch = originalFetch
    await out.done()
  }
})

test('signing out of legacy Cloud preserves the independent Morde Hub installation', async () => {
  const out = await service()
  const mordeHub = {
    baseURL: 'https://morde.example', installationID: 'i1234567', accountID: 'u1234567',
    attemptGeneration: 1, targetGrantVersion: 2,
    grants: { screenshot: true, files: false, command: false },
  }
  await out.credentials.set(credentialRef(TOKEN_REF), 'legacy-cloud-key')
  await out.credentials.set(credentialRef(MORDE_CLIENT_CREDENTIAL), 'chc_morde-client-key')
  out.svc.state = { mordeHub }
  out.svc.relay.signOut = async () => undefined

  try {
    await out.svc.signOut()
    assert.ok(!(await out.svc.token()))
    assert.equal(await out.svc.credential(MORDE_CLIENT_CREDENTIAL), 'chc_morde-client-key')
    assert.deepEqual(out.svc.state.mordeHub, mordeHub)
    assert.deepEqual(JSON.parse(await readFile(out.statePath, 'utf8')).mordeHub, mordeHub)
  } finally {
    await out.done()
  }
})

test('Morde Personal and Goal APIs use only the Client credential and preserve revisions and idempotency keys', async () => {
  const out = await service()
  const originalFetch = globalThis.fetch
  const calls = []
  await out.credentials.set(credentialRef(TOKEN_REF), 'nanoMuse-cloud-key')
  await out.credentials.set(credentialRef(MORDE_CLIENT_CREDENTIAL), 'chc_morde-client-key')
  out.svc.state = { mordeHub: { baseURL: 'https://morde.example', installationID: 'i1234567', accountID: 'u1234567', attemptGeneration: 1, targetGrantVersion: 1, grants: { screenshot: false, files: false, command: false } } }
  globalThis.fetch = async (input, init = {}) => {
    const url = new URL(String(input))
    calls.push({ url, init })
    if (url.pathname.endsWith('/api/personal-data/memory/global')) {
      if (init.method === 'PUT') return response({ object_type: 'memory', object_id: 'global', result_revision: 5, account_change_sequence: 21, replay: false })
      return response({ object_type: 'memory', object_id: 'global', revision: 4, body: { content: 'A memory.' } })
    }
    if (url.pathname.endsWith('/api/goals') && (!init.method || init.method === 'GET')) {
      return response({ items: [{ object_type: 'goal', object_id: 'g_1234', revision: 2, body: { title: 'Learn Go', status: 'active', description: '', steps: [] } }], next_cursor: null })
    }
    if (url.pathname.endsWith('/api/goals') && init.method === 'POST') {
      return response({ object_type: 'goal', object_id: 'g_1234', result_revision: 1, account_change_sequence: 22, replay: false }, 201)
    }
    if (url.pathname.endsWith('/api/goals/g_1234')) {
      return response({ object_type: 'goal', object_id: 'g_1234', result_revision: 3, account_change_sequence: 23, replay: false })
    }
    throw new Error(`unexpected local request: ${url}`)
  }
  try {
    const object = await out.svc.mordePersonalObject('memory', 'global')
    assert.equal(object.revision, 4)
    assert.equal(object.body.content, 'A memory.')
    assert.deepEqual(await out.svc.mordeReplacePersonalObject('memory', 'global', 4, { content: 'Updated memory.' }, 'mutation-123'), {
      object_type: 'memory', object_id: 'global', result_revision: 5, account_change_sequence: 21, replay: false,
    })
    const goals = await out.svc.mordeGoals()
    assert.equal(goals.items[0].object_id, 'g_1234')
    await out.svc.mordeCreateGoal('g_1234', { title: 'Learn Go', status: 'active', description: '', steps: [] }, 'goal-create-1')
    await out.svc.mordeUpdateGoal('g_1234', 2, { title: 'Learn Go', status: 'active', description: 'Practice', steps: [] }, 'goal-update-1')
    await out.svc.mordeDeleteGoal('g_1234', 3, 'goal-delete-1')

    assert.equal(calls.length, 6)
    for (const call of calls) {
      assert.equal(call.init.headers.authorization, 'Bearer chc_morde-client-key')
      assert.ok(!JSON.stringify(call.init).includes('nanoMuse-cloud-key'))
    }
    assert.equal(calls[1].init.headers['idempotency-key'], 'mutation-123')
    assert.deepEqual(JSON.parse(calls[1].init.body), { base_revision: 4, body: { content: 'Updated memory.' } })
    assert.equal(calls[2].url.searchParams.get('limit'), '100')
    assert.deepEqual(JSON.parse(calls[3].init.body), { id: 'g_1234', body: { title: 'Learn Go', status: 'active', description: '', steps: [] } })
    assert.equal(calls[4].init.headers['idempotency-key'], 'goal-update-1')
    assert.deepEqual(JSON.parse(calls[4].init.body), { base_revision: 2, body: { title: 'Learn Go', status: 'active', description: 'Practice', steps: [] } })
    assert.deepEqual(JSON.parse(calls[5].init.body), { base_revision: 3 })
  } finally {
    globalThis.fetch = originalFetch
    await out.done()
  }
})

test('Morde schedule client keeps title separate from objective and requires the returned title', async () => {
  const out = await service()
  const originalFetch = globalThis.fetch
  const calls = []
  let omitResponseTitle = false
  let omitResponseNextRun = false
  await out.credentials.set(credentialRef(MORDE_CLIENT_CREDENTIAL), 'chc_morde-client-key')
  out.svc.state = { mordeHub: { baseURL: 'https://morde.example', installationID: 'i1234567', accountID: 'u1234567', attemptGeneration: 1, targetGrantVersion: 1, grants: { screenshot: false, files: false, command: false } } }
  globalThis.fetch = async (input, init = {}) => {
    const url = new URL(String(input))
    calls.push({ url, init })
    if (url.pathname.endsWith('/api/available-models')) return response({ items: [{ id: 'model-1', name: 'Model', model_name: 'model-v1', capabilities: { text: true, tools: true, vision: false } }] })
    if (url.pathname.endsWith('/api/schedules')) {
      const request = JSON.parse(init.body)
      const receipt = {
        id: `schedule-${calls.filter((call) => call.url.pathname.endsWith('/api/schedules')).length}`, version: 1, status: 'enabled',
        title: request.title ?? request.objective.trim(), timing: request.timing,
        next_run_at: request.timing.kind === 'every_n_hours' ? request.timing.anchor_at : new Date(Date.now() + 60_000).toISOString(),
      }
      if (omitResponseTitle) delete receipt.title
      if (omitResponseNextRun) delete receipt.next_run_at
      return response(receipt, 201)
    }
    throw new Error(`unexpected local request: ${url}`)
  }
  try {
    const scope = out.svc.mordePersonalScope()
    const models = await out.svc.mordeAvailableModels(scope)
    assert.equal(models[0].capabilities.text, true)
    const input = {
      title: 'Separate schedule title', model_id: 'model-1', objective: 'Preserve this objective exactly.', host_id: null,
      timing: { kind: 'daily', starts_on: '2026-11-04', local_time: '09:00', time_zone: 'UTC' },
      budget: { active_seconds: 60, tool_calls: 2, model_tokens: 500 }, misfire_policy: 'skip',
      notification_events: ['completed', 'failed', 'waiting_user'], observation: null,
    }
    const key = `legacy-${'a'.repeat(64)}`
    const created = await out.svc.mordeCreateSchedule(input, key, scope)
    assert.equal(created.title, input.title)
    assert.equal(typeof created.next_run_at, 'string')
    const createCall = calls.find((call) => call.url.pathname.endsWith('/api/schedules'))
    assert.equal(createCall.init.headers.authorization, 'Bearer chc_morde-client-key')
    assert.equal(createCall.init.headers['idempotency-key'], key)
    assert.deepEqual(JSON.parse(createCall.init.body), input)

    const derivedInput = { ...input, objective: 'Derived objective title.' }
    delete derivedInput.title
    const derived = await out.svc.mordeCreateSchedule(derivedInput, `legacy-${'b'.repeat(64)}`, scope)
    assert.equal(derived.title, derivedInput.objective)
    assert.equal(Object.hasOwn(JSON.parse(calls.at(-1).init.body), 'title'), false)
    omitResponseTitle = true
    await assert.rejects(out.svc.mordeCreateSchedule(input, `legacy-${'c'.repeat(64)}`, scope), (error) => error.code === 'bad_response')
    omitResponseTitle = false
    omitResponseNextRun = true
    await assert.rejects(out.svc.mordeCreateSchedule(input, `legacy-${'d'.repeat(64)}`, scope), (error) => error.code === 'bad_response')
    omitResponseNextRun = false
    const anchor = '2026-11-04T10:15:00+08:00'
    const every = await out.svc.mordeCreateSchedule({ ...input, timing: { kind: 'every_n_hours', interval_hours: 3, anchor_at: anchor, time_zone: 'Asia/Shanghai' } }, `legacy-${'e'.repeat(64)}`, scope)
    assert.deepEqual(every.timing, { kind: 'every_n_hours', interval_hours: 3, anchor_at: anchor, time_zone: 'Asia/Shanghai' })
    assert.equal(every.next_run_at, anchor)
    assert.deepEqual(JSON.parse(calls.at(-1).init.body).timing, { kind: 'every_n_hours', interval_hours: 3, anchor_at: anchor, time_zone: 'Asia/Shanghai' })
  } finally {
    globalThis.fetch = originalFetch
    await out.done()
  }
})

test('Morde Personal API refuses missing credentials, unsafe object paths and malformed receipts', async () => {
  const out = await service()
  const originalFetch = globalThis.fetch
  let requests = 0
  globalThis.fetch = async () => { requests += 1; return response({}) }
  try {
    await assert.rejects(out.svc.mordePersonalObject('memory', 'global'), (error) => error.code === 'signed_out')
    await out.credentials.set(credentialRef(MORDE_CLIENT_CREDENTIAL), 'chc_morde-client-key')
    out.svc.state = { mordeHub: { baseURL: 'https://morde.example', installationID: 'i1234567', attemptGeneration: 1, targetGrantVersion: 1, grants: { screenshot: false, files: false, command: false } } }
    await assert.rejects(out.svc.mordePersonalObject('memory', '../global'), (error) => error.code === 'invalid_personal_object')
    await assert.rejects(out.svc.mordeReplacePersonalObject('memory', 'global', -1, { content: 'x' }, 'mutation-123'), (error) => error.code === 'invalid_personal_mutation')
    assert.equal(requests, 0)

    await assert.rejects(out.svc.mordeDeleteGoal('g_1234', 1, 'bad'), (error) => error.code === 'invalid_personal_mutation')
    assert.equal(requests, 0)
  } finally {
    globalThis.fetch = originalFetch
    await out.done()
  }
})

test('Morde Hub open waits for target approval and never inherits legacy blanket grants', async () => {
  const out = await service()
  try {
    out.svc.state = { remoteControl: true, trusted: { i7654321: { name: 'Phone', at: 1 } } }
    const opening = out.svc.openMordeHub({ url: 'https://example.com/path' }, { id: 'i7654321', name: 'Phone', kind: 'ios' })
    const [ask] = out.svc.asks.list
    assert.equal(ask.action, 'open')
    assert.equal(ask.onceOnly, true)
    assert.equal(out.svc.asks.answer(ask.id, 'always'), true)
    await assert.rejects(opening, (error) => error.code === 'not_allowed')
    assert.deepEqual(out.svc.state.trusted, { i7654321: { name: 'Phone', at: 1 } })
  } finally {
    await out.done()
  }
})

test('cancelling a Morde Hub open removes its pending target approval', async () => {
  const out = await service()
  try {
    const controller = new AbortController()
    const opening = out.svc.openMordeHub({ url: 'https://example.com/path' }, { id: 'i7654321', name: 'Phone', kind: 'ios' }, controller.signal)
    assert.equal(out.svc.asks.list.length, 1)
    controller.abort(new Error('cancelled by caller'))
    await assert.rejects(opening, /cancelled by caller/)
    assert.equal(out.svc.asks.list.length, 0)
  } finally {
    await out.done()
  }
})

test('Morde Hub caller permits approved opens and bounded notifications to active installations', async () => {
  const out = await service()
  try {
    await out.credentials.set(credentialRef(MORDE_CLIENT_CREDENTIAL), 'chc_caller-secret')
    out.svc.state = { mordeHub: { baseURL: 'https://morde.example', installationID: 'i1234567', attemptGeneration: 1, targetGrantVersion: 1, grants: { screenshot: false, files: false, command: false } } }
    const calls = []
    const signal = new AbortController().signal
    out.svc.mordeHub = {
      connected: true,
      deviceId: 'i1234567',
      call(...args) { calls.push(args); return Promise.resolve(args[1] === 'notify' ? { notified: true } : { opened: true }) },
    }
    out.svc.mordeHubDevices = [
      { id: 'i1234567', name: 'Desk', platform: 'desktop', expiresAt: new Date(Date.now() + 86_400_000).toISOString(), revoked: false, online: true },
      { id: 'i7654321', name: 'Phone', platform: 'ios', expiresAt: new Date(Date.now() + 86_400_000).toISOString(), revoked: false, online: true },
      { id: 'i7654322', name: 'Revoked', platform: 'ios', expiresAt: new Date(Date.now() + 86_400_000).toISOString(), revoked: true, online: true },
      { id: 'i7654323', name: 'Expired', platform: 'ios', expiresAt: new Date(Date.now() - 1000).toISOString(), revoked: false, online: true },
      { id: 'i7654324', name: 'Offline', platform: 'ios', expiresAt: new Date(Date.now() + 86_400_000).toISOString(), revoked: false, online: false },
      { id: 'i7654325', name: 'Workstation', platform: 'desktop', expiresAt: new Date(Date.now() + 86_400_000).toISOString(), revoked: false, online: true },
    ]

    const options = { signal, timeoutMs: 30_000 }
    assert.deepEqual(await out.svc.mordeHubCall('i7654325', 'open', { url: 'https://example.com/path' }, options), { opened: true })
    assert.deepEqual(calls, [['i7654325', 'open', { url: 'https://example.com/path' }, options]])
    assert.deepEqual(await out.svc.mordeHubCall('i7654325', 'notify', { title: 'Finished', text: 'The task is ready.' }, options), { notified: true })
    assert.deepEqual(calls[1], ['i7654325', 'notify', { text: 'The task is ready.', title: 'Finished' }, options])
    const maxTitle = 'é'.repeat(50)
    const maxText = '😀'.repeat(500)
    assert.deepEqual(await out.svc.mordeHubCall('i7654325', 'notify', { title: maxTitle, text: maxText }, options), { notified: true })
    assert.deepEqual(calls[2][2], { text: maxText, title: maxTitle }, 'the server byte limits are inclusive')
    await assert.rejects(out.svc.mordeHubCall('i1234567', 'open', { url: 'https://example.com' }), (error) => error.code === 'not_allowed')
    await assert.rejects(out.svc.mordeHubCall('i7654322', 'open', { url: 'https://example.com' }), (error) => error.code === 'not_allowed')
    await assert.rejects(out.svc.mordeHubCall('i7654323', 'open', { url: 'https://example.com' }), (error) => error.code === 'not_allowed')
    await assert.rejects(out.svc.mordeHubCall('i7654324', 'open', { url: 'https://example.com' }), (error) => error.code === 'offline')
    await assert.rejects(out.svc.mordeHubCall('i7654321', 'command', { command: 'id' }), (error) => error.code === 'not_allowed')
    await assert.rejects(out.svc.mordeHubCall('i7654321', 'open', { url: 'file:///etc/passwd' }), (error) => error.code === 'not_allowed')
    await assert.rejects(out.svc.mordeHubCall('i7654321', 'open', { url: 'https://user:secret@example.com' }), (error) => error.code === 'not_allowed')
    await assert.rejects(out.svc.mordeHubCall('i7654321', 'open', { url: 'https://example.com', path: '/tmp/file' }), (error) => error.code === 'not_allowed')
    await assert.rejects(out.svc.mordeHubCall('i7654321', 'open', { url: 'https://example.com' }), (error) => error.code === 'not_allowed', 'iOS targets cannot receive the PC open action')
    await assert.rejects(out.svc.mordeHubCall('i7654325', 'open', { url: ' https://example.com' }), (error) => error.code === 'not_allowed', 'URL edge whitespace is rejected')
    await assert.rejects(out.svc.mordeHubCall('i7654321', 'notify', { text: 'Hello' }), (error) => error.code === 'not_allowed', 'iOS targets cannot receive this desktop notification action')
    await assert.rejects(out.svc.mordeHubCall('i7654325', 'notify', {}), (error) => error.code === 'not_allowed')
    await assert.rejects(out.svc.mordeHubCall('i7654325', 'notify', { text: '' }), (error) => error.code === 'not_allowed')
    await assert.rejects(out.svc.mordeHubCall('i7654325', 'notify', { text: '😀'.repeat(501) }), (error) => error.code === 'not_allowed', 'text is bounded by UTF-8 bytes')
    await assert.rejects(out.svc.mordeHubCall('i7654325', 'notify', { title: 'é'.repeat(51), text: 'Hello' }), (error) => error.code === 'not_allowed', 'title is bounded by UTF-8 bytes')
    await assert.rejects(out.svc.mordeHubCall('i7654325', 'notify', { title: 7, text: 'Hello' }), (error) => error.code === 'not_allowed')
    await assert.rejects(out.svc.mordeHubCall('i7654325', 'notify', { text: 'Hello', url: 'https://example.com' }), (error) => error.code === 'not_allowed')
    await assert.rejects(out.svc.mordeHubCall('i7654325', 'notify', Object.create({ text: 'Hello' })), (error) => error.code === 'not_allowed', 'the required text must be an own field')
    assert.equal(calls.length, 3, 'invalid, inactive, or incompatible targets never reach the Hub')

    out.svc.mordeHub.connected = false
    await assert.rejects(out.svc.mordeHubCall('i7654325', 'notify', { text: 'Hello' }), (error) => error.code === 'offline')
    out.svc.mordeHub.connected = true
    out.svc.mordeHub.deviceId = 'i9999999'
    await assert.rejects(out.svc.mordeHubCall('i7654325', 'notify', { text: 'Hello' }), (error) => error.code === 'offline')
  } finally {
    await out.done()
  }
})

test('Morde Hub target grants are versioned, revocable, and require a current Task fence', async () => {
  const out = await service()
  const originalFetch = globalThis.fetch
  const previousURL = process.env.NANOMUSE_OPERATOR_URL
  const previousToken = process.env.NANOMUSE_OPERATOR_TOKEN
  let available = true
  let restarts = 0
  process.env.NANOMUSE_OPERATOR_URL = 'http://127.0.0.1:45678'
  process.env.NANOMUSE_OPERATOR_TOKEN = 'ab'.repeat(24)
  globalThis.fetch = async (input, init) => {
    assert.equal(String(input), 'http://127.0.0.1:45678/info')
    assert.equal(init.headers.authorization, `Bearer ${'ab'.repeat(24)}`)
    return response({ available })
  }
  try {
    await out.credentials.set(credentialRef(MORDE_CLIENT_CREDENTIAL), 'chc_target-secret')
    out.svc.state = {
      mordeHub: {
        baseURL: 'https://morde.example', installationID: 'i1234567', attemptGeneration: 1,
        targetGrantVersion: 1, grants: { screenshot: false, files: false },
      },
    }
    out.svc.mordeHub = { restart() { restarts++ } }
    const granted = await call(out.svc, 'POST', '/morde-hub/grants', { screenshot: true })
    assert.equal(granted.status, 200)
    assert.equal(granted.body.signedIn, true)
    assert.equal(granted.body.targetGrantVersion, 2)
    assert.equal(granted.body.grants.screenshot, true)
    assert.equal(restarts, 1)

    const caller = { kind: 'task', taskContext: { taskID: 'q1234567', authorizationVersion: 1, targetGrantVersion: 2, operationID: 'op_0123456789abcdef' } }
    await assert.doesNotReject(out.svc.mordeHubTaskGrant('screenshot', caller))
    await assert.rejects(out.svc.mordeHubTaskGrant('files', caller), (error) => error.code === 'not_allowed')
    await assert.rejects(out.svc.mordeHubTaskGrant('screenshot', { ...caller, kind: 'ios' }), (error) => error.code === 'not_allowed')
    await assert.rejects(out.svc.mordeHubTaskGrant('screenshot', { ...caller, taskContext: { ...caller.taskContext, targetGrantVersion: 1 } }), (error) => error.code === 'not_allowed')

    available = false
    const revoked = await call(out.svc, 'POST', '/morde-hub/grants', { screenshot: false })
    assert.equal(revoked.body.targetGrantVersion, 3)
    assert.equal(revoked.body.grants.screenshot, false)
    assert.equal(restarts, 2)
    await assert.rejects(out.svc.mordeHubTaskGrant('screenshot', caller), (error) => error.code === 'not_allowed')

    out.svc.state.mordeHub = {
      ...out.svc.state.mordeHub,
      targetGrantVersion: 3,
      grants: { screenshot: false, files: true },
      fileRoot: { directory: '/Users/muse/Projects', identity: 'ab'.repeat(32) },
    }
    const filesRevoked = await call(out.svc, 'POST', '/morde-hub/grants', { files: false })
    assert.equal(filesRevoked.body.targetGrantVersion, 4)
    assert.equal(filesRevoked.body.grants.fileRoot, '/Users/muse/Projects')
    assert.equal(out.svc.state.mordeHub.grants.files, false)
    assert.equal(restarts, 3)

    const workspaceCleared = await call(out.svc, 'POST', '/morde-hub/grants', { file_root: null })
    assert.equal(workspaceCleared.body.targetGrantVersion, 5)
    assert.equal(workspaceCleared.body.grants.fileRoot, null)
    assert.equal('fileRoot' in out.svc.state.mordeHub, false)
    assert.equal('fileRoot' in JSON.parse(await readFile(out.statePath, 'utf8')).mordeHub, false)
    assert.equal(restarts, 4)
  } finally {
    globalThis.fetch = originalFetch
    if (previousURL === undefined) delete process.env.NANOMUSE_OPERATOR_URL
    else process.env.NANOMUSE_OPERATOR_URL = previousURL
    if (previousToken === undefined) delete process.env.NANOMUSE_OPERATOR_TOKEN
    else process.env.NANOMUSE_OPERATOR_TOKEN = previousToken
    await out.done()
  }
})

test('Morde Hub command stays unavailable when the Linux sandbox probe fails', async () => {
  const out = await service()
  const previousHelper = process.env.MORDE_SYNC_BIN
  const originalFetch = globalThis.fetch
  process.env.MORDE_SYNC_BIN = process.execPath
  globalThis.fetch = async () => response({ items: [] })
  try {
    await out.credentials.set(credentialRef(MORDE_CLIENT_CREDENTIAL), 'chc_target-secret')
    out.svc.state = {
      mordeHub: {
        baseURL: 'https://morde.example', installationID: 'i1234567', attemptGeneration: 1,
        targetGrantVersion: 1, grants: { screenshot: false, files: false, command: false },
        fileRoot: { directory: '/Users/muse/Projects', identity: 'ab'.repeat(32) },
      },
    }
    const status = await out.svc.mordeHubStatus()
    assert.equal(status.capabilities.command, false)
    const rejected = await call(out.svc, 'POST', '/morde-hub/grants', { command: true })
    assert.equal(rejected.status, 409)
    assert.equal(rejected.body.error.code, 'capability_unavailable')
    assert.equal(out.svc.state.mordeHub.grants.command, false)
    const caller = { kind: 'task', taskContext: { taskID: 'q1234567', authorizationVersion: 1, targetGrantVersion: 1, operationID: 'op_0123456789abcdef' } }
    await assert.rejects(out.svc.mordeHubTaskGrant('command', caller), (error) => error.code === 'unsupported')
  } finally {
    globalThis.fetch = originalFetch
    if (previousHelper === undefined) delete process.env.MORDE_SYNC_BIN
    else process.env.MORDE_SYNC_BIN = previousHelper
    await out.done()
  }
})

test('Morde Hub advertises Linux command only after the local sandbox probe passes', async () => {
  const out = await service()
  const previousHelper = process.env.MORDE_SYNC_BIN
  const originalFetch = globalThis.fetch
  const platform = Object.getOwnPropertyDescriptor(process, 'platform')
  const helper = `${out.statePath}.helper`
  process.env.MORDE_SYNC_BIN = helper
  Object.defineProperty(process, 'platform', { ...platform, value: 'linux' })
  globalThis.fetch = async () => response({ items: [] })
  try {
    await writeFile(helper, '#!/bin/sh\nIFS= read -r request\nprintf \'%s\\n\' \'{"jsonrpc":"2.0","id":"morde-hub","result":{"available":true}}\'\n')
    await chmod(helper, 0o700)
    await out.credentials.set(credentialRef(MORDE_CLIENT_CREDENTIAL), 'chc_target-secret')
    out.svc.state = {
      mordeHub: {
        baseURL: 'https://morde.example', installationID: 'i1234567', attemptGeneration: 1,
        targetGrantVersion: 1, grants: { screenshot: false, files: false, command: false },
        fileRoot: { directory: '/Users/muse/Projects', identity: 'ab'.repeat(32) },
      },
    }
    const status = await out.svc.mordeHubStatus()
    assert.equal(status.capabilities.command, true)
    const grant = await call(out.svc, 'POST', '/morde-hub/grants', { command: true })
    assert.equal(grant.status, 200)
    assert.equal(grant.body.grants.command, true)
  } finally {
    globalThis.fetch = originalFetch
    if (platform) Object.defineProperty(process, 'platform', platform)
    if (previousHelper === undefined) delete process.env.MORDE_SYNC_BIN
    else process.env.MORDE_SYNC_BIN = previousHelper
    await out.done()
  }
})

test('device revocation refuses this installation and reconciles an unknown result', async () => {
  const out = await service()
  const originalFetch = globalThis.fetch
  let revokeFails = false
  globalThis.fetch = async (input, init = {}) => {
    const url = String(input)
    assert.equal(init.headers.authorization, 'Bearer chc_target-secret')
    if (url.endsWith('/api/devices/i7654321')) {
      assert.equal(init.method, 'DELETE')
      if (revokeFails) throw new TypeError('connection closed')
      return response(undefined, 204)
    }
    if (url.endsWith('/api/devices')) return response({ items: [
      { id: 'i1234567', display_name: 'Desk', platform: 'desktop', expires_at: '2030-01-01T00:00:00Z', revoked: false, created_at: '2026-01-01T00:00:00Z', last_used_at: null, online: true },
      { id: 'i7654321', display_name: 'Phone', platform: 'ios', expires_at: '2030-01-01T00:00:00Z', revoked: true, created_at: '2026-01-01T00:00:00Z', last_used_at: null, online: false },
    ] })
    throw new Error(`unexpected request ${url}`)
  }
  try {
    await out.credentials.set(credentialRef(MORDE_CLIENT_CREDENTIAL), 'chc_target-secret')
    out.svc.state = { mordeHub: { baseURL: 'https://morde.example', installationID: 'i1234567', targetGrantVersion: 1, attemptGeneration: 1, grants: { screenshot: false, files: false, command: false } } }
    out.svc.mordeHubDevices = [
      { id: 'i1234567', name: 'Desk', platform: 'desktop', expiresAt: '2030-01-01T00:00:00Z', revoked: false, online: true },
      { id: 'i7654321', name: 'Phone', platform: 'ios', expiresAt: '2030-01-01T00:00:00Z', revoked: false, online: false },
    ]
    await assert.rejects(out.svc.mordeHubRevokeDevice('i1234567'), (error) => error.code === 'current_device')
    const confirmed = await call(out.svc, 'POST', '/morde-hub/revoke-device', { installation_id: 'i7654321' })
    assert.equal(confirmed.status, 200)
    assert.equal(confirmed.body.devices.find((device) => device.id === 'i7654321').revoked, true)

    revokeFails = true
    out.svc.mordeHubDevices = out.svc.mordeHubDevices.map((device) => device.id === 'i7654321' ? { ...device, revoked: false } : device)
    const reconciled = await out.svc.mordeHubRevokeDevice('i7654321')
    assert.equal(reconciled.devices.find((device) => device.id === 'i7654321').revoked, true)
  } finally {
    globalThis.fetch = originalFetch
    await out.done()
  }
})

test('Hub device refresh reports credential-store failures without an unhandled rejection', async () => {
  const out = await service()
  const hub = {}
  out.svc.state = { mordeHub: { baseURL: 'https://morde.example', installationID: 'i1234567' } }
  out.svc.mordeHub = hub
  out.svc.credential = async () => { throw new Error('secure storage unavailable') }
  try {
    await assert.doesNotReject(out.svc.refreshMordeHubDevicesFromHub(hub))
    assert.match(out.svc.mordeHubDeviceError, /secure storage unavailable/)
  } finally {
    await out.done()
  }
})

test('a failed revocation with an unavailable reconciliation stays unknown', async () => {
  const out = await service()
  const originalFetch = globalThis.fetch
  globalThis.fetch = async (input) => {
    if (String(input).endsWith('/api/devices/i7654321')) throw new TypeError('connection closed')
    throw new TypeError('reconciliation unavailable')
  }
  try {
    await out.credentials.set(credentialRef(MORDE_CLIENT_CREDENTIAL), 'chc_target-secret')
    out.svc.state = { mordeHub: { baseURL: 'https://morde.example', installationID: 'i1234567', targetGrantVersion: 1, attemptGeneration: 1, grants: { screenshot: false, files: false, command: false } } }
    out.svc.mordeHubDevices = [{ id: 'i7654321', name: 'Phone', platform: 'ios', expiresAt: '2030-01-01T00:00:00Z', revoked: false, online: true }]
    await assert.rejects(out.svc.mordeHubRevokeDevice('i7654321'), (error) => error.code === 'device_revoke_unknown')
  } finally {
    globalThis.fetch = originalFetch
    await out.done()
  }
})

test('Morde Hub credential refusal clears local installation state and stops its socket', async () => {
  const out = await service()
  const originalStart = HubClient.prototype.start
  const originalOnUnauthorized = HubClient.prototype.onUnauthorized
  let unauthorized
  HubClient.prototype.start = function () {}
  HubClient.prototype.onUnauthorized = function (listener) { unauthorized = listener; return () => undefined }
  try {
    await out.credentials.set(credentialRef(MORDE_CLIENT_CREDENTIAL), 'chc_target-secret')
    out.svc.state = { mordeHub: { baseURL: 'https://morde.example', installationID: 'i1234567', targetGrantVersion: 1, attemptGeneration: 1, grants: { screenshot: false, files: false, command: false } } }
    out.svc.startMordeHub = NanomuseCloud.prototype.startMordeHub.bind(out.svc)
    out.svc.startMordeHub()
    assert.equal(typeof unauthorized, 'function')
    unauthorized()
    await out.svc.serialize(async () => undefined)
    assert.equal(await out.svc.credential(MORDE_CLIENT_CREDENTIAL), '')
    assert.equal(out.svc.mordeHubView().signedIn, false)
    assert.equal(out.svc.mordeHubView().deviceID, undefined)
    assert.equal('mordeHub' in out.svc.state, false)
  } finally {
    HubClient.prototype.start = originalStart
    HubClient.prototype.onUnauthorized = originalOnUnauthorized
    await out.done()
  }
})
