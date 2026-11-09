import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { test } from 'node:test'
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { Service } from '@deepseek-ai/cordis'
import NanomuseRooms from '../lib/rooms.js'


const goal = (id, title, revision = 1) => ({
  object_type: 'goal', object_id: id, revision,
  body: {
    title, description: `${title} description`, status: 'active',
    steps: [{ id: `${id}-step-1`, title: 'First step', status: 'pending', note: '' }],
    related_material_ids: [],
  },
})

function fakeCloud({ scope = 'morde-account-a', personal = [], goals = [], models = [{ id: 'model-text', name: 'Text model', model_name: 'text-v1', capabilities: { text: true, tools: false, vision: false } }] } = {}) {
  const state = { scope, personal: structuredClone(personal), goals: structuredClone(goals), cursorSeen: [], writes: [], scheduleWrites: [], listener: undefined }
  const receipt = (type, id, revision) => ({ object_type: type, object_id: id, result_revision: revision, account_change_sequence: revision, replay: false })
  const cloud = {
    profile: { current: () => ({ name: 'Legacy Cloud Muse', description: 'legacy description', style: 'muse' }) },
    mordePersonalScope: () => state.scope,
    onChange(callback) { state.listener = callback; return () => { state.listener = undefined } },
    async status() { return { ready: false } },
    async mordePersonalSnapshot(cursor = '') {
      state.cursorSeen.push(cursor)
      return { watermark: 1, items: cursor ? [] : structuredClone(state.personal), next_cursor: null }
    },
    async mordePersonalObject(type, id) {
      return structuredClone(state.personal.find((item) => item.object_type === type && item.object_id === id))
    },
    async mordeGoals(cursor = '') { return { items: cursor ? [] : structuredClone(state.goals), next_cursor: null } },
    async mordeReplacePersonalObject(type, id, baseRevision, body) {
      state.writes.push(['replace', type, id, baseRevision, structuredClone(body)])
      const existing = state.personal.find((item) => item.object_type === type && item.object_id === id)
      const actual = existing?.revision ?? 0
      if (actual !== baseRevision) throw Object.assign(new Error('revision conflict'), { status: 409, code: 'revision_conflict' })
      const revision = actual + 1
      const item = { object_type: type, object_id: id, revision, body: structuredClone(body) }
      state.personal = state.personal.filter((entry) => entry !== existing)
      state.personal.push(item)
      return receipt(type, id, revision)
    },
    async mordeCreateGoal(id, body) {
      state.writes.push(['create-goal', id, structuredClone(body)])
      if (state.goals.some((item) => item.object_id === id)) throw Object.assign(new Error('already exists'), { status: 409, code: 'revision_conflict' })
      state.goals.push(goal(id, body.title, 1))
      state.goals.at(-1).body = structuredClone(body)
      return receipt('goal', id, 1)
    },
    async mordeUpdateGoal(id, baseRevision, body) {
      state.writes.push(['update-goal', id, baseRevision, structuredClone(body)])
      const existing = state.goals.find((item) => item.object_id === id)
      if (!existing || existing.revision !== baseRevision) throw Object.assign(new Error('revision conflict'), { status: 409, code: 'revision_conflict' })
      existing.revision++
      existing.body = structuredClone(body)
      return receipt('goal', id, existing.revision)
    },
    async mordeDeleteGoal(id, baseRevision) {
      state.writes.push(['delete-goal', id, baseRevision])
      const existing = state.goals.find((item) => item.object_id === id)
      if (!existing || existing.revision !== baseRevision) throw Object.assign(new Error('revision conflict'), { status: 409, code: 'revision_conflict' })
      state.goals = state.goals.filter((item) => item !== existing)
      return receipt('goal', id, baseRevision + 1)
    },
    async mordeAvailableModels() { return structuredClone(models) },
    async mordeCreateSchedule(input, key) {
      state.scheduleWrites.push({ input: structuredClone(input), key })
      return {
        id: 'morde-schedule-1', version: 1, status: 'enabled', title: input.title ?? input.objective.trim(),
        timing: structuredClone(input.timing),
        next_run_at: input.timing.kind === 'every_n_hours' ? input.timing.anchor_at : new Date(Date.now() + 60_000).toISOString(),
      }
    },
  }
  return { cloud, state, change: () => state.listener?.() }
}

function fakeContext(nanomuseCloud, warnings, services = {}) {
  return {
    nanomuseCloud,
    reflect: { provide() {} },
    logger: { info() {}, debug() {}, error() {}, warn(...args) { warnings.push(args.map(String).join(' ')) } },
    effect(fn) { fn() },
    inject() {},
    on() { return () => undefined },
    get(name) { return services[name] },
  }
}

async function makeRooms(t, cloud, { waitSync = true, services = {} } = {}) {
  const home = await mkdtemp(join(tmpdir(), 'nm-morde-rooms-'))
  const previousHome = process.env.DSH_HOME
  process.env.DSH_HOME = home
  const warnings = []
  const rooms = new NanomuseRooms(fakeContext(cloud.cloud, warnings, services))
  const timerRecords = []
  const originalSetTimeout = globalThis.setTimeout
  const originalSetInterval = globalThis.setInterval
  globalThis.setTimeout = (...args) => {
    const timer = originalSetTimeout(...args)
    timerRecords.push({ timer, kind: 'timeout', delay: args[1], stack: new Error().stack })
    return timer
  }
  globalThis.setInterval = (...args) => {
    const timer = originalSetInterval(...args)
    timerRecords.push({ timer, kind: 'interval', delay: args[1], stack: new Error().stack })
    return timer
  }
  t.after(async () => {
    if (rooms.pendingMordeSync) clearTimeout(rooms.pendingMordeSync)
    if (rooms.pendingFrame) clearTimeout(rooms.pendingFrame)
    if (rooms.pendingSave) clearTimeout(rooms.pendingSave)
    if (rooms.timer) clearInterval(rooms.timer)
    for (const { timer, kind } of timerRecords) kind === 'interval' ? clearInterval(timer) : clearTimeout(timer)
    await rooms.writing
    await rm(home, { recursive: true, force: true })
    if (previousHome === undefined) delete process.env.DSH_HOME
    else process.env.DSH_HOME = previousHome
  })
  try { await rooms[Service.init]() }
  finally {
    globalThis.setTimeout = originalSetTimeout
    globalThis.setInterval = originalSetInterval
  }
  if (waitSync && rooms.mordeSyncing) await rooms.mordeSyncing
  return { rooms, home, warnings }
}

async function waitForSwitch(rooms) {
  await rooms.mordeScopeChanging
  if (rooms.mordeSyncing) await rooms.mordeSyncing
}

test('Morde Personal is account scoped, server profile and goals are pulled, and old rooms.json stays untouched', async (t) => {
  const cloud = fakeCloud({
    personal: [
      { object_type: 'profile', object_id: 'default', revision: 2, body: { name: 'Morde Muse', avatar: 'dragon', emoji: '', color: '', description: 'server profile', style: 'muse' } },
      { object_type: 'memory', object_id: 'global', revision: 3, body: { content: '- server memory' } },
    ],
    goals: [goal('server-goal', 'Server goal')],
  })
  const { rooms, home } = await makeRooms(t, cloud)
  const roomsDir = join(home, 'nanomuse')
  await mkdir(roomsDir, { recursive: true })
  const legacy = {
    lang: 'zh-CN', feed: { instructions: 'legacy feed' }, goals: [{ id: 'legacy-goal', title: 'Legacy goal' }],
    memory: [{ id: 'legacy-memory', at: 1, text: 'legacy memory', source: 'person' }],
  }
  await writeFile(join(roomsDir, 'rooms.json'), JSON.stringify(legacy))
  // The service started before the legacy file was created, so reload via a scope change to prove
  // that account scope reads the separate file instead of importing unscoped history.
  cloud.state.scope = ''
  cloud.change()
  await waitForSwitch(rooms)
  cloud.state.scope = 'morde-account-a'
  cloud.change()
  await waitForSwitch(rooms)

  assert.deepEqual(rooms.memory.map((item) => item.text), ['server memory'])
  assert.deepEqual(rooms.store.goals.map((item) => item.id), ['server-goal'])
  assert.match(rooms.docs().identity.text, /Morde Muse/)
  assert.doesNotMatch(rooms.docs().identity.text, /Legacy Cloud Muse/)
  assert.equal(rooms.store.mordeSync.memoryRevisions.global, 3)
  assert.equal(rooms.store.mordeSync.goalRevisions['server-goal'], 1)

  await rooms.remember('account A draft', 'person')
  if (rooms.pendingMordeSync) clearTimeout(rooms.pendingMordeSync)
  rooms.pendingMordeSync = undefined
  await rooms.syncMordePersonal(true)
  assert.ok(cloud.state.writes.some((write) => write[0] === 'replace' && write[2] === 'global'))
  assert.equal(cloud.state.personal.find((item) => item.object_type === 'memory').body.content, '- server memory\n- account A draft')

  const localGoal = { id: 'local-goal', title: 'Local draft', description: 'Draft', category: 'other', status: 'tracking', sessionId: '', createdAt: 1, updatedAt: 1, summary: '', activity: [], steps: [], progress: -1, attention: false }
  rooms.store.goals.unshift(localGoal)
  rooms.markMordeGoalPending(localGoal)
  await rooms.syncMordePersonal(true)
  assert.ok(cloud.state.writes.some((write) => write[0] === 'create-goal' && write[1] === 'local-goal'))
  const scopedA = JSON.parse(await readFile(join(roomsDir, 'rooms.morde-account-a.json'), 'utf8'))
  assert.ok(scopedA.memory.some((item) => item.text === 'account A draft'))
  assert.equal(JSON.parse(await readFile(join(roomsDir, 'rooms.json'), 'utf8')).memory[0].text, 'legacy memory')

  const accountAPersonal = structuredClone(cloud.state.personal)
  const accountAGoals = structuredClone(cloud.state.goals)
  cloud.state.scope = 'morde-account-b'
  cloud.state.personal = [{ object_type: 'memory', object_id: 'global', revision: 1, body: { content: '- account B memory' } }]
  cloud.state.goals = [goal('account-b-goal', 'Account B goal')]
  cloud.change()
  await waitForSwitch(rooms)
  assert.deepEqual(rooms.memory.map((item) => item.text), ['account B memory'])
  assert.deepEqual(rooms.store.goals.map((item) => item.id), ['account-b-goal'])

  await rooms.remember('account B draft', 'person')
  if (rooms.pendingMordeSync) clearTimeout(rooms.pendingMordeSync)
  rooms.pendingMordeSync = undefined
  await rooms.syncMordePersonal(true)
  const scopedB = JSON.parse(await readFile(join(roomsDir, 'rooms.morde-account-b.json'), 'utf8'))
  assert.ok(scopedB.memory.some((item) => item.text === 'account B draft'))

  cloud.state.scope = 'morde-account-a'
  cloud.state.personal = accountAPersonal
  cloud.state.goals = accountAGoals
  cloud.change()
  await waitForSwitch(rooms)
  assert.ok(rooms.memory.some((item) => item.text === 'account A draft'))
  assert.ok(!rooms.memory.some((item) => item.text === 'account B draft'))
  assert.ok(rooms.store.goals.some((item) => item.id === 'local-goal'))
})

test('legacy rooms preview is read-only and explicit selections sync Memory and Goals without changing rooms.json', async (t) => {
  const cloud = fakeCloud()
  const { rooms, home } = await makeRooms(t, cloud)
  const legacyPath = join(home, 'nanomuse', 'rooms.json')
  await mkdir(join(home, 'nanomuse'), { recursive: true })
  const legacyText = JSON.stringify({
    feed: { instructions: 'legacy feed', posts: [{ id: 'post-old', title: 'Old post' }], routine: { on: true, time: '08:00', ensuredAt: 1 } },
    library: [{ id: 'lib-old', name: 'Local file', path: '/private/file.txt' }],
    memory: [{ id: 'memory-old', at: 1, text: 'Only import this memory', source: 'person' }, { id: 'memory-skip', at: 2, text: 'Leave this memory', source: 'person' }],
    goals: [
      { id: 'goal-old', title: 'Old goal', description: 'A local-only description', category: 'career', status: 'tracking', createdAt: 3, steps: ['First step'], sessionId: 'old-chat' },
      { id: 'goal-skip', title: 'Leave this goal', description: 'Not selected', createdAt: 4 },
    ],
  })
  await writeFile(legacyPath, legacyText)

  const preview = await rooms.previewLegacyMordeImport()
  assert.equal(preview.memories.length, 2)
  assert.equal(preview.goals.length, 2)
  assert.deepEqual(preview.retained, { feed_posts: 1, library_items: 1, routine: { on: true, time: '08:00', ensuredAt: 1 } })
  assert.equal(cloud.state.writes.length, 0)

  const result = await rooms.importLegacyMordeData({ memory_ids: ['memory-old'], goal_ids: ['goal-old'] })
  assert.deepEqual(result, { imported_memories: 1, imported_goals: 1, memory_sync_pending: false, goal_sync_pending: 0 })
  assert.equal(cloud.state.personal.find((item) => item.object_type === 'memory')?.body.content, '- Only import this memory')
  const importedGoal = cloud.state.goals.find((item) => item.body.title === 'Old goal')
  assert.equal(importedGoal?.body.description, 'A local-only description')
  assert.equal(importedGoal?.body.steps[0]?.title, 'First step')
  assert.notEqual(importedGoal?.object_id, 'goal-old')
  assert.ok(!cloud.state.goals.some((item) => item.body.title === 'Leave this goal'))
  assert.equal(await readFile(legacyPath, 'utf8'), legacyText)

  const writes = cloud.state.writes.length
  const repeated = await rooms.importLegacyMordeData({ memory_ids: ['memory-old'], goal_ids: ['goal-old'] })
  assert.deepEqual(repeated, { imported_memories: 0, imported_goals: 0, memory_sync_pending: false, goal_sync_pending: 0 })
  assert.equal(cloud.state.writes.length, writes)
  assert.ok((await rooms.previewLegacyMordeImport()).memories.find((item) => item.id === 'memory-old')?.imported)
})

test('legacy rooms cannot be imported after Morde sign-out', async (t) => {
  const cloud = fakeCloud({ scope: '' })
  const { rooms } = await makeRooms(t, cloud)
  await assert.rejects(rooms.previewLegacyMordeImport(), { code: 'signed_out' })
  await assert.rejects(rooms.importLegacyMordeData({ memory_ids: ['memory-old'] }), { code: 'signed_out' })
  assert.equal(cloud.state.writes.length, 0)
})

test('legacy schedules preview title and objective separately, block unknown fields, and import only after explicit confirmation', async (t) => {
  const cloud = fakeCloud()
  const runAt = new Date(Date.now() + 24 * 60 * 60_000).toISOString()
  const everyAnchor = '2026-11-04T10:15:00+08:00'
  const localSchedules = [
    { id: 'old-with-title', kind: 'daily', title: 'Separate display title', prompt: 'Keep this objective unchanged.', scheduledAt: runAt, time: '09:30', timeZone: 'UTC' },
    { id: 'old-derived-title', kind: 'weekly', prompt: 'Derived title comes from this objective.', scheduledAt: runAt, time: '10:00', timeZone: 'UTC', weekdays: [1, 3] },
    { id: 'old-model-option', kind: 'daily', title: 'Do not drop model', prompt: 'Old model option must be reviewed.', scheduledAt: runAt, time: '11:00', timeZone: 'UTC', model_id: 'legacy-model' },
    { id: 'old-hourly', kind: 'every', title: 'Old interval', prompt: 'Preserve the next run.', scheduledAt: everyAnchor, everySeconds: 3600, timeZone: 'UTC' },
    { id: 'old-invalid-anchor', kind: 'every', title: 'Invalid phase', prompt: 'Do not guess the phase.', scheduledAt: '2026-02-30T09:00:00Z', everySeconds: 3600, timeZone: 'UTC' },
    { id: 'old-fractional-hour', kind: 'every', title: 'Unsupported interval', prompt: 'Do not round the interval.', scheduledAt: runAt, everySeconds: 3601 },
    { id: 'old-overprecision-anchor', kind: 'every', title: 'Unsupported precision', prompt: 'Do not round the phase.', scheduledAt: '2026-11-04T10:15:00.1234567+08:00', everySeconds: 3600, timeZone: 'UTC' },
  ]
  const schedule = { async list() { return structuredClone(localSchedules) } }
  const sessions = { async list() { return { items: [{ sessionId: 'legacy-session' }] } } }
  const { rooms } = await makeRooms(t, cloud, { services: { schedule, sessionController: sessions } })

  const preview = await rooms.previewLegacyMordeSchedules()
  assert.equal(preview.available, true)
  assert.equal(preview.schedules.length, 7)
  assert.equal(preview.schedules[0].title, 'Separate display title')
  assert.equal(preview.schedules[0].title_is_derived, false)
  assert.equal(preview.schedules[0].objective, 'Keep this objective unchanged.')
  assert.equal(preview.schedules[1].title_is_derived, true)
  assert.equal(preview.schedules[1].title, 'Derived title comes from this objective.')
  assert.ok(preview.schedules[2].unavailable_reason?.includes('model_id'))
  assert.equal(preview.schedules[3].source_every_seconds, 3600)
  assert.deepEqual(preview.schedules[3].timing, { kind: 'every_n_hours', interval_hours: 1, anchor_at: everyAnchor, time_zone: 'UTC' })
  assert.equal(preview.schedules[4].unavailable_reason, 'The old interval schedule has no valid RFC3339 next-run anchor.')
  assert.equal(preview.schedules[5].unavailable_reason, 'Only whole-hour intervals from 1 to 168 hours can be represented.')
  assert.equal(preview.schedules[6].unavailable_reason, 'The old interval schedule has no valid RFC3339 next-run anchor.')
  assert.deepEqual(cloud.state.scheduleWrites, [])

  const source = preview.schedules[0]
  const selection = {
    source_id: source.source_id, fingerprint: source.fingerprint, model_id: 'model-text', time_zone: 'UTC',
    budget: { active_seconds: 60, tool_calls: 2, model_tokens: 500 }, original_disabled: true,
  }
  await assert.rejects(rooms.importLegacyMordeSchedule({ ...selection, source_id: preview.schedules[2].source_id, fingerprint: preview.schedules[2].fingerprint }), { code: 'unsupported_schedule' })
  await assert.rejects(rooms.importLegacyMordeSchedule({ ...selection, source_id: preview.schedules[4].source_id, fingerprint: preview.schedules[4].fingerprint }), { code: 'unsupported_schedule' })
  await assert.rejects(rooms.importLegacyMordeSchedule({ ...selection, source_id: preview.schedules[5].source_id, fingerprint: preview.schedules[5].fingerprint }), { code: 'unsupported_schedule' })
  await assert.rejects(rooms.importLegacyMordeSchedule({ ...selection, original_disabled: false }), { code: 'bad_request' })
  assert.equal(cloud.state.scheduleWrites.length, 0)

  const createSchedule = cloud.cloud.mordeCreateSchedule
  let dropFirstReply = true
  cloud.cloud.mordeCreateSchedule = async (input, key, scope) => {
    if (dropFirstReply) {
      dropFirstReply = false
      cloud.state.scheduleWrites.push({ input: structuredClone(input), key })
      throw new TypeError('connection closed after request')
    }
    return createSchedule(input, key, scope)
  }
  await assert.rejects(rooms.importLegacyMordeSchedule(selection), /connection closed after request/)
  assert.equal(cloud.state.scheduleWrites.length, 1)

  const imported = await rooms.importLegacyMordeSchedule(selection)
  assert.equal(imported.already_imported, false)
  assert.equal(imported.schedule.title, 'Separate display title')
  assert.equal(cloud.state.scheduleWrites.length, 2)
  assert.deepEqual(cloud.state.scheduleWrites[1].input, {
    title: 'Separate display title', model_id: 'model-text', objective: 'Keep this objective unchanged.', host_id: null,
    timing: { kind: 'daily', starts_on: runAt.slice(0, 10), local_time: '09:30', time_zone: 'UTC' },
    budget: selection.budget, misfire_policy: 'skip', notification_events: ['completed', 'failed', 'waiting_user'], observation: null,
  })
  assert.match(cloud.state.scheduleWrites[0].key, /^legacy-[a-f0-9]{64}$/)
  assert.equal(cloud.state.scheduleWrites[1].key, cloud.state.scheduleWrites[0].key)
  const duplicate = await rooms.importLegacyMordeSchedule(selection)
  assert.deepEqual(duplicate, { already_imported: true, source_id: source.source_id, schedule_id: 'morde-schedule-1' })
  assert.equal(cloud.state.scheduleWrites.length, 2)

  const derived = preview.schedules[1]
  const derivedResult = await rooms.importLegacyMordeSchedule({ ...selection, source_id: derived.source_id, fingerprint: derived.fingerprint })
  assert.equal(derivedResult.schedule.title, derived.objective)
  assert.equal(Object.hasOwn(cloud.state.scheduleWrites[2].input, 'title'), false)
  assert.equal(cloud.state.scheduleWrites[2].input.objective, derived.objective)
  assert.notEqual(cloud.state.scheduleWrites[2].key, cloud.state.scheduleWrites[1].key)

  const every = preview.schedules[3]
  const everyResult = await rooms.importLegacyMordeSchedule({ ...selection, source_id: every.source_id, fingerprint: every.fingerprint })
  assert.deepEqual(cloud.state.scheduleWrites[3].input.timing, every.timing)
  assert.equal(everyResult.schedule.timing.anchor_at, everyAnchor)
  assert.equal(everyResult.schedule.next_run_at, everyAnchor)
  assert.equal(localSchedules.length, 7, 'the old DSH schedules remain untouched')
})

test('legacy schedule export contains exact hostless Morde requests and stable keys only for importable rows', async (t) => {
  const cloud = fakeCloud()
  const runAt = new Date(Date.now() + 24 * 60 * 60_000).toISOString()
  const localSchedules = [
    { id: 'exportable', kind: 'daily', title: 'Exported title', prompt: 'Keep this objective.', scheduledAt: runAt, time: '09:30', timeZone: 'UTC' },
    { id: 'unsupported', kind: 'daily', title: 'Do not drop a field', prompt: 'Needs review.', scheduledAt: runAt, time: '10:00', timeZone: 'UTC', model_id: 'old-model' },
  ]
  const schedule = { async list() { return structuredClone(localSchedules) } }
  const sessions = { async list() { return { items: [{ sessionId: 'legacy-session' }] } } }
  const { rooms } = await makeRooms(t, cloud, { services: { schedule, sessionController: sessions } })
  const budget = { active_seconds: 60, tool_calls: 2, model_tokens: 500 }
  const exported = await rooms.exportLegacyMordeSchedules({ model_id: 'model-text', budget })

  assert.equal(exported.format, 'morde-nanomuse-dsh-schedules')
  assert.equal(exported.version, 1)
  assert.ok(Number.isFinite(Date.parse(exported.exported_at)))
  const item = exported.schedules[0]
  assert.deepEqual(item.migration?.request, {
    title: 'Exported title', model_id: 'model-text', objective: 'Keep this objective.', host_id: null,
    timing: item.timing, budget, misfire_policy: 'skip',
    notification_events: ['completed', 'failed', 'waiting_user'], observation: null,
  })
  assert.equal(item.migration?.idempotency_key, `legacy-${createHash('sha256').update(`morde-account-a\0${item.source_id}`).digest('hex')}`)
  assert.equal(Object.hasOwn(exported.schedules[1], 'migration'), false)
  assert.deepEqual(cloud.state.scheduleWrites, [])

  await rooms.importLegacyMordeSchedule({
    source_id: item.source_id, fingerprint: item.fingerprint, model_id: 'model-text', time_zone: 'UTC', budget, original_disabled: true,
  })
  const afterImport = await rooms.exportLegacyMordeSchedules({ model_id: 'model-text', budget })
  assert.equal(afterImport.schedules[0].imported, true)
  assert.equal(Object.hasOwn(afterImport.schedules[0], 'migration'), false)
})

test('legacy schedule export refuses files larger than Portal’s 10 MiB limit', async (t) => {
  const cloud = fakeCloud()
  const runAt = new Date(Date.now() + 24 * 60 * 60_000).toISOString()
  const schedules = Array.from({ length: 170 }, (_, index) => ({
    id: `large-${index}`, kind: 'daily', title: `Schedule ${index}`, prompt: 'x'.repeat(32768),
    scheduledAt: runAt, time: '09:30', timeZone: 'UTC',
  }))
  const schedule = { async list() { return structuredClone(schedules) } }
  const sessions = { async list() { return { items: [{ sessionId: 'legacy-session' }] } } }
  const { rooms } = await makeRooms(t, cloud, { services: { schedule, sessionController: sessions } })

  await assert.rejects(rooms.exportLegacyMordeSchedules({
    model_id: 'model-text', budget: { active_seconds: 60, tool_calls: 2, model_tokens: 500 },
  }), { code: 'legacy_schedule_export_too_large' })
})

test('legacy schedule export refuses more entries than Portal accepts', async (t) => {
  const cloud = fakeCloud()
  const runAt = new Date(Date.now() + 24 * 60 * 60_000).toISOString()
  const schedules = Array.from({ length: 2001 }, (_, index) => ({
    id: `schedule-${index}`, kind: 'daily', title: `Schedule ${index}`, prompt: 'A short objective.',
    scheduledAt: runAt, time: '09:30', timeZone: 'UTC',
  }))
  const schedule = { async list() { return structuredClone(schedules) } }
  const sessions = { async list() { return { items: [{ sessionId: 'legacy-session' }] } } }
  const { rooms } = await makeRooms(t, cloud, { services: { schedule, sessionController: sessions } })

  await assert.rejects(rooms.exportLegacyMordeSchedules({
    model_id: 'model-text', budget: { active_seconds: 60, tool_calls: 2, model_tokens: 500 },
  }), { code: 'legacy_schedule_export_too_large' })
})

test('legacy schedule export keeps unavailable empty and oversized source text as preview-only rows', async (t) => {
  const cloud = fakeCloud()
  const runAt = new Date(Date.now() + 24 * 60 * 60_000).toISOString()
  const schedules = [
    { id: 'empty-prompt', kind: 'daily', title: '', prompt: '', scheduledAt: runAt, time: '09:30', timeZone: 'UTC' },
    { id: 'oversized-text', kind: 'daily', title: '标题😀'.repeat(10000), prompt: '执行内容😀'.repeat(8000), scheduledAt: runAt, time: '09:30', timeZone: 'UTC' },
    { id: 'invalid-time', kind: 'daily', title: '', prompt: '', scheduledAt: 'not a date', time: '09:30', timeZone: 'Invalid/Zone' },
  ]
  const schedule = { async list() { return structuredClone(schedules) } }
  const sessions = { async list() { return { items: [{ sessionId: 'legacy-session' }] } } }
  const { rooms } = await makeRooms(t, cloud, { services: { schedule, sessionController: sessions } })

  const exported = await rooms.exportLegacyMordeSchedules({
    model_id: 'model-text', budget: { active_seconds: 60, tool_calls: 2, model_tokens: 500 },
  })
  assert.equal(exported.schedules.length, 3)
  assert.equal(exported.schedules[0].objective, '')
  assert.equal(exported.schedules[0].title, 'empty-prompt')
  assert.equal(exported.schedules[0].title_is_derived, true)
  assert.equal(exported.schedules[0].migration, undefined)
  for (const field of ['title', 'objective']) {
    const value = exported.schedules[1][field]
    assert.ok(Buffer.byteLength(value, 'utf8') <= 32768)
    assert.equal(value.includes('\uFFFD'), false)
    assert.equal(/[\uD800-\uDBFF]$/.test(value), false)
  }
  assert.equal(exported.schedules[1].migration, undefined)
  assert.match(exported.schedules[1].unavailable_reason, /shortened in this export preview/)
  assert.equal(exported.schedules[2].source_scheduled_at, 'not a date')
  assert.equal(exported.schedules[2].time_zone, 'Invalid/Zone')
  assert.equal(exported.schedules[2].migration, undefined)
})

test('Morde profile edits use the current revision and preserve server fields', async (t) => {
  const profile = { object_type: 'profile', object_id: 'default', revision: 2, body: { name: 'Morde Muse', avatar: 'emoji', emoji: '🌙', color: '#7a4ea8', description: 'server description', style: 'flat' } }
  const cloud = fakeCloud({ personal: [profile] })
  const { rooms } = await makeRooms(t, cloud)

  const saved = await rooms.updateMordeProfile({ name: 'Updated Muse' })

  assert.deepEqual(cloud.state.writes.at(-1), ['replace', 'profile', 'default', 2, { ...profile.body, name: 'Updated Muse' }])
  assert.equal(saved.revision, 3)
  assert.deepEqual(rooms.view().mordeProfile, { revision: 3, body: { ...profile.body, name: 'Updated Muse' } })
  assert.equal(cloud.cloud.profile.current().name, 'Legacy Cloud Muse')
  const changedAvatar = await rooms.updateMordeProfile({ avatar: 'dragon' })
  assert.deepEqual(cloud.state.writes.at(-1), ['replace', 'profile', 'default', 3, { name: 'Updated Muse', avatar: 'dragon', description: 'server description', style: 'flat' }])
  assert.equal(changedAvatar.revision, 4)
  await assert.rejects(rooms.updateMordeProfile({ color: 'red' }), { code: 'invalid_profile' })
  assert.equal(cloud.state.writes.length, 2)
})

test('first Morde profile write uses the valid dragon shape', async (t) => {
  const cloud = fakeCloud()
  const { rooms } = await makeRooms(t, cloud)

  await rooms.updateMordeProfile({ name: 'First Muse' })

  assert.deepEqual(cloud.state.writes[0], ['replace', 'profile', 'default', 0, { name: 'First Muse', avatar: 'dragon', description: '', style: '' }])
})

test('a concurrent Morde profile edit wins the revision conflict without changing the local baseline', async (t) => {
  const profile = { object_type: 'profile', object_id: 'default', revision: 1, body: { name: 'Morde Muse', avatar: 'dragon', description: '', style: '' } }
  const cloud = fakeCloud({ personal: [profile] })
  const { rooms } = await makeRooms(t, cloud)
  cloud.cloud.mordePersonalObject = async () => {
    const snapshot = structuredClone(cloud.state.personal[0])
    cloud.state.personal[0].revision = 2
    cloud.state.personal[0].body = { ...cloud.state.personal[0].body, name: 'Remote Edit' }
    return snapshot
  }

  await assert.rejects(rooms.updateMordeProfile({ name: 'Local Edit' }), { code: 'revision_conflict' })

  assert.equal(cloud.state.personal[0].body.name, 'Remote Edit')
  assert.equal(rooms.store.mordeSync.profile.body.name, 'Morde Muse')
})

test('concurrent local and server edits keep the local draft and do not overwrite Morde', async (t) => {
  const cloud = fakeCloud({
    personal: [{ object_type: 'memory', object_id: 'global', revision: 1, body: { content: '- shared baseline' } }],
    goals: [goal('shared-goal', 'Shared baseline')],
  })
  const { rooms, warnings } = await makeRooms(t, cloud)
  await rooms.remember('local edit', 'person')
  rooms.store.goals[0].title = 'Local Goal edit'
  await rooms.save()
  cloud.state.personal[0].revision = 2
  cloud.state.personal[0].body = { content: '- server edit' }
  cloud.state.goals[0].revision = 2
  cloud.state.goals[0].body = { ...cloud.state.goals[0].body, title: 'Server Goal edit' }
  const writesBefore = cloud.state.writes.length
  if (rooms.pendingMordeSync) clearTimeout(rooms.pendingMordeSync)
  rooms.pendingMordeSync = undefined
  await rooms.syncMordePersonal(true)

  assert.deepEqual(rooms.memory.map((item) => item.text), ['shared baseline', 'local edit'])
  assert.equal(rooms.store.goals[0].title, 'Local Goal edit')
  assert.equal(cloud.state.writes.length, writesBefore)
  assert.ok(warnings.some((warning) => warning.includes('Morde memory conflict')))
  assert.ok(warnings.some((warning) => warning.includes('Morde Goal conflict')))
})

test('logout during a paged snapshot stops subsequent Morde requests', async (t) => {
  let finishSnapshot
  let started
  const snapshotStarted = new Promise((resolve) => { started = resolve })
  const cloud = fakeCloud()
  cloud.cloud.mordePersonalSnapshot = async (cursor = '') => {
    cloud.state.cursorSeen.push(cursor)
    if (cursor) return { watermark: 1, items: [], next_cursor: null }
    started()
    return new Promise((resolve) => { finishSnapshot = resolve })
  }
  const { rooms } = await makeRooms(t, cloud, { waitSync: false })
  await snapshotStarted
  cloud.state.scope = ''
  cloud.change()
  await rooms.mordeScopeChanging
  finishSnapshot({ watermark: 1, items: [], next_cursor: 'next' })
  if (rooms.mordeSyncing) await rooms.mordeSyncing
  await new Promise((resolve) => setTimeout(resolve, 10))
  assert.deepEqual(cloud.state.cursorSeen, [''])
  assert.deepEqual(cloud.state.writes, [])
})
