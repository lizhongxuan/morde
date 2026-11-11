import assert from 'node:assert/strict'
import { spawn, execFileSync } from 'node:child_process'
import { once } from 'node:events'
import { createHash } from 'node:crypto'
import { writeFileSync } from 'node:fs'
import { createInterface } from 'node:readline'
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { basename, dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'
import { credentialRef } from '@deepseek-ai/dsh-credentials'
import NanomuseCloud, { MORDE_CLIENT_CREDENTIAL } from '../lib/cloud.js'
import { HubClient } from '../lib/hub.js'

const packageRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const repositoryRoot = resolve(packageRoot, '../../../..')
const serverRoot = join(repositoryRoot, 'server')
const syncRoot = join(repositoryRoot, 'runtime/morde-sync')

function fakeContext() {
  const credentials = new Map()
  return {
    credentials,
    ctx: {
      reflect: { provide() {} },
      logger: { info() {}, warn() {}, debug() {}, error() {} },
      credentials: {
        async set(ref, value) { credentials.set(String(ref), value) },
        async resolve(ref) { return credentials.has(String(ref)) ? { value: credentials.get(String(ref)) } : undefined },
        async unset(ref) { credentials.delete(String(ref)) },
      },
      settings: { async update() {}, async mutate() {}, describe() { return [] } },
      get() { return undefined }, inject() {}, effect(fn) { fn() }, on() { return () => undefined },
    },
  }
}

function startFixture() {
  const child = spawn('go', ['test', './internal/store/postgres', '-run', '^TestK4NodeHTTPFixture$', '-count=1', '-v'], {
    cwd: serverRoot, env: { ...process.env, CH_K4_FIXTURE: '1' }, stdio: ['ignore', 'pipe', 'ignore'],
  })
  const exited = once(child, 'close')
  const ready = new Promise((resolveReady, rejectReady) => {
    const timer = setTimeout(() => rejectReady(new Error('K4 fixture did not become ready')), 180_000)
    const lines = createInterface({ input: child.stdout })
    lines.on('line', (line) => {
      const match = line.match(/^K4_FIXTURE_READY (\{.*\})$/)
      if (!match) return
      clearTimeout(timer)
      try { resolveReady(JSON.parse(match[1])) }
      catch { rejectReady(new Error('K4 fixture returned invalid JSON')) }
    })
    child.once('error', () => { clearTimeout(timer); rejectReady(new Error('Could not start the local K4 fixture')) })
    child.once('close', () => { clearTimeout(timer); rejectReady(new Error('Local K4 fixture exited before becoming ready')) })
  })
  return { child, exited, ready }
}

test('real PC helper resumes and commits through the local K4 HTTP service', async () => {
  assert.ok(process.env.CH_TEST_DATABASE_URL, 'set CH_TEST_DATABASE_URL to a disposable local PostgreSQL server')
  const databaseURL = new URL(process.env.CH_TEST_DATABASE_URL)
  assert.ok(['127.0.0.1', 'localhost', '::1'].includes(databaseURL.hostname), 'the live fixture must use loopback PostgreSQL')

  const home = await mkdtemp(join(tmpdir(), 'morde-k4-live-'))
  const helperBinary = join(home, 'morde-sync')
  const originalHelper = process.env.MORDE_SYNC_BIN
  const originalHandle = HubClient.prototype.handle
  const originalStart = HubClient.prototype.start
  const fixtures = []
  let service
  let handlers
  try {
    execFileSync('go', ['build', '-o', helperBinary, './cmd/morde-sync'], { cwd: syncRoot, stdio: 'ignore' })
    process.env.MORDE_SYNC_BIN = helperBinary

    const makeRoot = async (name, bytes) => {
      const directory = join(home, name)
      const reports = join(directory, 'reports')
      await mkdir(reports, { recursive: true })
      await writeFile(join(reports, 'synthetic.pdf'), bytes)
      await writeFile(join(directory, 'notes.txt'), 'plain file.get remains available')
      return { directory, file: join(reports, 'synthetic.pdf') }
    }
    const source = Buffer.concat([Buffer.from('%PDF-1.7\n'), Buffer.alloc(600 * 1024, 0x5a)])
    const changed = Buffer.concat([Buffer.from('%PDF-1.7\n'), Buffer.alloc(600 * 1024, 0x63)])
    const rootA = await makeRoot('selected-a', source)
    const rootB = await makeRoot('selected-b', source)
    const overLimit = Buffer.alloc(16 * 1024 * 1024 + 1, 0x2a)
    await writeFile(join(rootB.directory, 'reports', 'oversized.pdf'), overLimit)

    handlers = new Map()
    HubClient.prototype.handle = function (action, handler) { handlers.set(action, handler); return this }
    HubClient.prototype.start = function () {}
    const fake = fakeContext()
    service = new NanomuseCloud(fake.ctx, { baseURL: 'https://morde.local.invalid', deviceName: 'Synthetic PC', statePath: join(home, 'cloud.json') })
    service.state = {}
    service.startMordeHub = () => undefined
    service.broadcast = () => undefined
    await fake.credentials.set(credentialRef(MORDE_CLIENT_CREDENTIAL), 'chc_local-fixture-only')

    const setFixture = async (fixture, directory) => {
      service.state = { mordeHub: {
        baseURL: fixture.base_url, installationID: 'i1234567', targetGrantVersion: fixture.target_grant_version - 1,
        grants: { screenshot: false, files: false, command: false },
      } }
      service.mordeHubCapabilities = { screenshot: false, files: true, command: false }
      service.mordeHub = undefined
      await service.mordeHubUpdateGrants({ file_root: directory, files: true })
      service.startMordeHub = NanomuseCloud.prototype.startMordeHub.bind(service)
      service.startMordeHub()
    }
    const context = (fixture, signal, event = () => {}) => ({
      from: { kind: 'task', taskContext: {
        taskID: fixture.task_id, taskRevision: fixture.task_revision, authorizationVersion: 1,
        targetGrantVersion: fixture.target_grant_version, operationID: fixture.fetch_id,
      } }, signal, event,
    })
    const fetchArgs = (fixture) => ({
      fetch_id: fixture.fetch_id, path: 'reports/synthetic.pdf', task_revision: fixture.task_revision,
      ticket: fixture.ticket, ticket_expires_at: new Date(Date.now() + 60_000).toISOString(),
    })
    const ownerURL = (fixture, suffix = '') => `${fixture.base_url}/api/tasks/${fixture.task_id}/file-fetches${suffix}`
    const ownerRequest = (fixture, suffix = '', init = {}) => fetch(ownerURL(fixture, suffix), {
      ...init, headers: {
        cookie: `ch_portal=chs_t1234567_${Buffer.alloc(32, 0x44).toString('base64url')}`,
        origin: fixture.base_url, ...init.headers,
      },
    })
    const ownerFetches = async (fixture) => {
      const response = await ownerRequest(fixture)
      assert.equal(response.status, 200)
      return (await response.json()).items
    }

    const fixtureA = startFixture()
    fixtures.push(fixtureA)
    const a = await fixtureA.ready
    await setFixture(a, rootA.directory)
    const eventsA = []
    let changedSource = false
    await assert.rejects(handlers.get('file.fetch')(fetchArgs(a), context(a, new AbortController().signal, (event) => {
      eventsA.push(event)
      if (!changedSource && event.confirmed_bytes >= a.chunk_bytes) {
        changedSource = true
        writeFileSync(rootA.file, changed)
      }
    })), (error) => error.code === 'not_allowed')
    assert.equal(changedSource, true)
    const incompleteA = (await ownerFetches(a))[0]
    assert.equal(incompleteA.source_directory_name, basename(rootA.directory))
    assert.equal(incompleteA.status, 'transferring')
    assert.equal(incompleteA.confirmed_bytes, a.chunk_bytes)
    assert.equal(incompleteA.content_state, 'none')
    assert.equal((await ownerRequest(a, `/${a.fetch_id}/content`)).status, 404)
    await fetch(`${a.base_url}/__test/k4-fixture/shutdown`, { method: 'POST' })
    assert.equal((await fixtureA.exited)[0], 0)

    const fixtureB = startFixture()
    fixtures.push(fixtureB)
    const b = await fixtureB.ready
    await setFixture(b, rootB.directory)
    const handler = handlers.get('file.fetch')
    const controller = new AbortController()
    let interrupted = false
    const eventsB = []
    await assert.rejects(handler(fetchArgs(b), context(b, controller.signal, (event) => {
      eventsB.push(event)
      if (!interrupted && event.confirmed_bytes >= b.chunk_bytes) {
        interrupted = true
        controller.abort(new Error('local live test disconnect'))
      }
    })), (error) => error.code === 'cancelled')
    assert.equal(interrupted, true)
    const incompleteB = (await ownerFetches(b))[0]
    assert.equal(incompleteB.confirmed_bytes, b.chunk_bytes)
    assert.equal(incompleteB.content_state, 'none')
    assert.equal((await ownerRequest(b, `/${b.fetch_id}/content`)).status, 404)

    const eventsResume = []
    const result = await handler(fetchArgs(b), context(b, new AbortController().signal, (event) => eventsResume.push(event)))
    assert.equal(result.status, 'completed')
    assert.equal(result.size_bytes, source.length)
    assert.equal(JSON.stringify([result, eventsB, eventsResume, service.mordeHubView()]).includes(b.ticket), false)
    const completed = (await ownerFetches(b))[0]
    assert.equal(completed.source_directory_name, basename(rootB.directory))
    assert.equal(completed.status, 'completed')
    assert.equal(completed.content_state, 'retained')
    assert.equal(completed.confirmed_bytes, source.length)
    assert.equal(JSON.stringify(completed).includes(b.ticket), false)
    const download = await ownerRequest(b, `/${b.fetch_id}/content`)
    assert.equal(download.status, 200)
    assert.equal(download.headers.get('content-type'), 'application/pdf')
    assert.deepEqual(Buffer.from(await download.arrayBuffer()), source)

    const textResult = await handlers.get('file.get')({ path: 'notes.txt', max_bytes: 1024 }, {
      from: { kind: 'task', taskContext: {
        taskID: b.task_id, authorizationVersion: 1, targetGrantVersion: b.target_grant_version, operationID: 'legacy-file-get',
      } }, signal: new AbortController().signal, event() {},
    })
    assert.equal(textResult.text, 'plain file.get remains available')
    await assert.rejects(handler({ ...fetchArgs(b), path: 'reports/oversized.pdf' }, context(b, new AbortController().signal)),
      (error) => error.code === 'failed')

    service.state.mordeHub.targetGrantVersion += 1
    service.state.mordeHub.grants.files = false
    await assert.rejects(handler(fetchArgs(b), context(b, new AbortController().signal)), (error) => error.code === 'not_allowed')
    const removed = await ownerRequest(b, `/${b.fetch_id}/content`, { method: 'DELETE' })
    assert.equal(removed.status, 204)
    const deleted = (await ownerFetches(b))[0]
    assert.equal(deleted.content_state, 'deleted')
    assert.ok(deleted.deleted_at)
    assert.equal((await ownerRequest(b, `/${b.fetch_id}/content`)).status, 404)

    await fetch(`${b.base_url}/__test/k4-fixture/shutdown`, { method: 'POST' })
    assert.equal((await fixtureB.exited)[0], 0)
    assert.equal(createHash('sha256').update(source).digest('hex'), result.source_sha256)
  } finally {
    for (const fixture of fixtures) {
      if (fixture.child.exitCode === null) {
        try {
          const ready = await fixture.ready.catch(() => undefined)
          if (ready) await fetch(`${ready.base_url}/__test/k4-fixture/shutdown`, { method: 'POST' })
        } catch {}
        const killTimer = setTimeout(() => fixture.child.kill('SIGTERM'), 10_000)
        killTimer.unref()
        await fixture.exited
        clearTimeout(killTimer)
      }
    }
    if (originalHelper === undefined) delete process.env.MORDE_SYNC_BIN
    else process.env.MORDE_SYNC_BIN = originalHelper
    HubClient.prototype.handle = originalHandle
    HubClient.prototype.start = originalStart
    await rm(home, { recursive: true, force: true })
  }
})
