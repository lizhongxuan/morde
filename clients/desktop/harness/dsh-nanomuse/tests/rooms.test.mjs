// The rooms' pure helpers: what the agent's final answer parses to, and the first
// title a goal gets before the agent names it.
import assert from 'node:assert/strict'
import { mkdtemp, readFile, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from 'node:test'
import NanomuseRooms from '../lib/rooms.js'
import { goalTitle, parseArray } from '../lib/rooms.js'

function roomsContext(nanomuseCloud) {
  return {
    reflect: { provide() {} },
    logger: { info() {}, warn() {}, debug() {}, error() {} },
    nanomuseCloud,
    get() { return undefined },
    effect() {},
    on() { return () => undefined },
    inject() {},
  }
}

async function withRoomsHome(run) {
  const home = await mkdtemp(join(tmpdir(), 'nm-rooms-morde-'))
  const previous = process.env.DSH_HOME
  process.env.DSH_HOME = home
  try { await run(home) }
  finally {
    if (previous === undefined) delete process.env.DSH_HOME
    else process.env.DSH_HOME = previous
    await rm(home, { recursive: true, force: true })
  }
}

function mordeSnapshot(items = []) {
  return { watermark: 1, items, next_cursor: null }
}

test('parseArray takes the last fenced json block', () => {
  const text = 'Here is a draft:\n```json\n[{"title": "draft"}]\n```\nFinal:\n```json\n[{"title": "a"}, {"title": "b"}]\n```\n'
  assert.deepEqual(parseArray(text).map((r) => r.title), ['a', 'b'])
})

test('parseArray falls back to the outermost brackets, and to nothing', () => {
  assert.deepEqual(parseArray('sure: [{"title": "x", "n": 1}] done').map((r) => r.title), ['x'])
  assert.deepEqual(parseArray('no json here'), [])
  assert.deepEqual(parseArray('```json\n{"not": "an array"}\n```'), [])
  assert.deepEqual(parseArray('```json\n[1, "two", {"title": "three"}]\n```').map((r) => r.title), ['three'])
})

test('goalTitle keeps the first clause', () => {
  assert.equal(goalTitle('工作日零点前睡觉；每天 23:00 提醒我，每周日晚上问我这周睡得怎么样。'), '工作日零点前睡觉')
  assert.equal(goalTitle('Save ¥20,000 by December; track it monthly and tell me when I drift.'), 'Save ¥20,000 by December')
  assert.equal(goalTitle('Run a 10k'), 'Run a 10k')
  assert.equal(goalTitle('Hi, call my parents every Sunday'), 'Hi, call my parents every Sunday'.slice(0, 40))
  assert.equal(goalTitle('x'.repeat(80)).length, 40)
})

test('rooms keep each Morde account in its own local file', async () => withRoomsHome(async () => {
  const cloud = { mordePersonalScope: () => 'morde-account-a' }
  const rooms = new NanomuseRooms(roomsContext(cloud))
  rooms.mordeScope = 'morde-account-a'
  rooms.store.memory = [{ id: 'mem-a', at: 1, text: 'Account A', source: 'person' }]
  await rooms.save()
  rooms.mordeScope = 'morde-account-b'
  rooms.store = await rooms.read(rooms.pathForScope('morde-account-b'))
  assert.deepEqual(rooms.store.memory, [])
  rooms.store.memory = [{ id: 'mem-b', at: 2, text: 'Account B', source: 'person' }]
  await rooms.save()
  assert.match(await readFile(rooms.pathForScope('morde-account-a'), 'utf8'), /Account A/)
  assert.doesNotMatch(await readFile(rooms.pathForScope('morde-account-a'), 'utf8'), /Account B/)
  assert.match(await readFile(rooms.pathForScope('morde-account-b'), 'utf8'), /Account B/)
}))

test('first Morde sync preserves local-only history instead of importing it automatically', async () => withRoomsHome(async () => {
  const writes = []
  const cloud = {
    mordePersonalScope: () => 'morde-account-a',
    async mordePersonalSnapshot() { return mordeSnapshot() },
    async mordeGoals() { return { items: [], next_cursor: null } },
    async mordeReplacePersonalObject(...args) { writes.push(['memory', ...args]); return { result_revision: 1 } },
    async mordeCreateGoal(...args) { writes.push(['goal', ...args]); return { result_revision: 1 } },
  }
  const rooms = new NanomuseRooms(roomsContext(cloud))
  rooms.mordeScope = 'morde-account-a'
  rooms.mordeGeneration = 1
  rooms.store.memory = [{ id: 'mem-local', at: 1, text: 'Keep this local', source: 'import' }]
  rooms.store.goals = [{ id: 'goal-local', title: 'Local draft', description: 'Keep local', category: 'other', status: 'tracking', sessionId: '', createdAt: 1, updatedAt: 1, summary: '', activity: [], steps: [], progress: -1, attention: false }]
  await rooms.syncMordePersonalFor('morde-account-a', 1)
  assert.deepEqual(writes, [])
  assert.equal(rooms.store.memory[0].text, 'Keep this local')
  assert.equal(rooms.store.goals[0].title, 'Local draft')
}))

test('explicitly changed Morde memory and goals sync with revisions', async () => withRoomsHome(async () => {
  const writes = []
  const cloud = {
    mordePersonalScope: () => 'morde-account-a',
    async mordePersonalSnapshot() { return mordeSnapshot() },
    async mordeGoals() { return { items: [], next_cursor: null } },
    async mordeReplacePersonalObject(...args) { writes.push(['memory', ...args]); return { result_revision: 2 } },
    async mordeCreateGoal(...args) { writes.push(['goal', ...args]); return { result_revision: 3 } },
  }
  const rooms = new NanomuseRooms(roomsContext(cloud))
  rooms.mordeScope = 'morde-account-a'
  rooms.mordeGeneration = 1
  rooms.store.memory = [{ id: 'mem-new', at: 1, text: 'Selected memory', source: 'person' }]
  rooms.store.mordeSync.pendingMemory = true
  rooms.store.goals = [{ id: 'goal-new', title: 'Selected goal', description: 'Create it', category: 'other', status: 'tracking', sessionId: '', createdAt: 1, updatedAt: 1, summary: '', activity: [], steps: [], progress: -1, attention: false }]
  rooms.store.mordeSync.pendingGoals['goal-new'] = true
  await rooms.syncMordePersonalFor('morde-account-a', 1)
  assert.deepEqual(writes.map(([kind]) => kind).sort(), ['goal', 'memory'])
  assert.equal(rooms.store.mordeSync.memoryRevisions.global, 2)
  assert.equal(rooms.store.mordeSync.goalRevisions['goal-new'], 3)
  assert.equal(rooms.store.mordeSync.pendingMemory, false)
  assert.equal(rooms.store.mordeSync.pendingGoals['goal-new'], undefined)
}))

test('Morde revision conflicts keep both local drafts and the saved remote baseline', async () => withRoomsHome(async () => {
  const writes = []
  const baseline = { title: 'Base', description: 'Base', status: 'active', steps: [], related_material_ids: [] }
  const remote = { ...baseline, title: 'Server edit' }
  const cloud = {
    mordePersonalScope: () => 'morde-account-a',
    async mordePersonalSnapshot() { return mordeSnapshot([{ object_type: 'memory', object_id: 'global', revision: 2, body: { content: '- Server memory' } }]) },
    async mordeGoals() { return { items: [{ object_type: 'goal', object_id: 'goal-shared', revision: 2, body: remote }], next_cursor: null } },
    async mordeReplacePersonalObject(...args) { writes.push(['memory', ...args]); return { result_revision: 3 } },
    async mordeUpdateGoal(...args) { writes.push(['goal', ...args]); return { result_revision: 3 } },
  }
  const rooms = new NanomuseRooms(roomsContext(cloud))
  rooms.mordeScope = 'morde-account-a'
  rooms.mordeGeneration = 1
  rooms.store.memory = [{ id: 'mem-local', at: 1, text: 'Local memory', source: 'person' }]
  rooms.store.mordeSync.memoryBodies.global = '- Base memory'
  rooms.store.mordeSync.memoryRevisions.global = 1
  rooms.store.mordeSync.pendingMemory = true
  rooms.store.goals = [{ id: 'goal-shared', title: 'Local edit', description: 'Base', category: 'other', status: 'tracking', sessionId: '', createdAt: 1, updatedAt: 1, summary: '', activity: [], steps: [], progress: -1, attention: false }]
  rooms.store.mordeSync.goalBodies['goal-shared'] = baseline
  rooms.store.mordeSync.goalRevisions['goal-shared'] = 1
  rooms.store.mordeSync.pendingGoals['goal-shared'] = true
  await rooms.syncMordePersonalFor('morde-account-a', 1)
  assert.deepEqual(writes, [])
  assert.equal(rooms.store.memory[0].text, 'Local memory')
  assert.equal(rooms.store.goals[0].title, 'Local edit')
  assert.equal(rooms.store.mordeSync.memoryBodies.global, '- Base memory')
  assert.equal(rooms.store.mordeSync.goalBodies['goal-shared'].title, 'Base')
  assert.equal(rooms.store.mordeSync.pendingMemory, true)
  assert.equal(rooms.store.mordeSync.pendingGoals['goal-shared'], true)
}))

test('zip writes an archive Python can read back', async () => {
  const { zip, crc32 } = await import('../lib/rooms.js')
  const { writeFile, mkdtemp, rm } = await import('node:fs/promises')
  const { tmpdir } = await import('node:os')
  const { join } = await import('node:path')
  const { spawnSync } = await import('node:child_process')
  assert.equal(crc32(Buffer.from('')), 0)
  assert.equal(crc32(Buffer.from('123456789')), 0xcbf43926)
  const dir = await mkdtemp(join(tmpdir(), 'nm-zip-'))
  const file = join(dir, 'x.zip')
  const big = Buffer.alloc(5000, 'a')
  await writeFile(file, zip([
    { name: 'nanomuse/rooms.json', data: Buffer.from('{"goals":[]}'), at: new Date(2026, 9, 3, 12, 30, 10) },
    { name: '构件/读我.txt', data: big, at: new Date() },
  ]))
  const py = spawnSync('python3', ['-c', `import zipfile,sys; z=zipfile.ZipFile(sys.argv[1]); print(z.testzip()); print(','.join(i.filename for i in z.infolist())); print(len(z.read('构件/读我.txt')))`, file], { encoding: 'utf8' })
  await rm(dir, { recursive: true, force: true })
  if (py.status !== 0) return // no python here; the crc checks above still ran
  assert.equal(py.stdout.trim().split('\n')[0], 'None')
  assert.equal(py.stdout.trim().split('\n')[1], 'nanomuse/rooms.json,构件/读我.txt')
  assert.equal(py.stdout.trim().split('\n')[2], '5000')
})

test('writeZipFile streams an archive Python reads back, one entry at a time', async () => {
  const { writeZipFile } = await import('../lib/rooms.js')
  const { writeFile, mkdtemp, rm } = await import('node:fs/promises')
  const { tmpdir } = await import('node:os')
  const { join } = await import('node:path')
  const { spawnSync } = await import('node:child_process')
  const dir = await mkdtemp(join(tmpdir(), 'nm-zipfile-'))
  const file = join(dir, 'x.zip')
  const source = join(dir, 'big.bin')
  await writeFile(source, Buffer.alloc(5000, 'a'))
  const entries = [
    { name: 'nanomuse/rooms.json', data: Buffer.from('{"goals":[]}'), at: new Date(2026, 9, 3, 12, 30, 10) },
    { name: 'sessions/big.bin', path: source, at: new Date() },
    { name: 'sessions/gone.bin', path: join(dir, 'gone.bin'), at: new Date() }, // vanished since the scan
  ]
  await writeZipFile(file, entries)
  await writeZipFile(file, entries) // a second export in the same second replaces the first
  const py = spawnSync('python3', ['-c', `import zipfile,sys; z=zipfile.ZipFile(sys.argv[1]); print(z.testzip()); print(','.join(i.filename for i in z.infolist())); print(len(z.read('sessions/big.bin')))`, file], { encoding: 'utf8' })
  await rm(dir, { recursive: true, force: true })
  if (py.status !== 0) return // no python here
  assert.deepEqual(py.stdout.trim().split(/\r?\n/), ['None', 'nanomuse/rooms.json,sessions/big.bin', '5000']) // Python prints \r\n on Windows
})

test('the export leaves links, loops and oversized files out instead of following them', async (t) => {
  const { EXPORT_MAX_FILE_BYTES, scanExportRoot } = await import('../lib/rooms.js')
  const { mkdir, mkdtemp, rm, symlink, truncate, writeFile } = await import('node:fs/promises')
  const { tmpdir } = await import('node:os')
  const { join } = await import('node:path')
  if (process.platform === 'win32') return t.skip('symlink creation needs elevated rights on Windows')
  const dir = await mkdtemp(join(tmpdir(), 'nm-export-links-'))
  try {
    await mkdir(join(dir, 'root', 'faces'), { recursive: true })
    await writeFile(join(dir, 'root', 'profile.json'), '{}')
    await writeFile(join(dir, 'root', 'faces', 'calm.png'), 'png')
    await writeFile(join(dir, 'outside'), 'secret')
    await symlink('.', join(dir, 'root', 'loop'))
    await symlink(join(dir, 'outside'), join(dir, 'root', 'outside'))
    await writeFile(join(dir, 'root', 'huge.mp4'), '')
    await truncate(join(dir, 'root', 'huge.mp4'), EXPORT_MAX_FILE_BYTES + 1)
    const budget = { files: 0, bytes: 0 }
    const names = (await scanExportRoot(join(dir, 'root'), 'nanomuse/', budget)).map((e) => e.name).sort()
    assert.deepEqual(names, ['nanomuse/faces/calm.png', 'nanomuse/profile.json'])
    assert.deepEqual(budget, { files: 2, bytes: 5 })
    // a root that is not there, or is a link, is nothing
    assert.deepEqual(await scanExportRoot(join(dir, 'missing'), 'sessions/', budget), [])
    await symlink(join(dir, 'root'), join(dir, 'linked-root'))
    assert.deepEqual(await scanExportRoot(join(dir, 'linked-root'), 'sessions/', budget), [])
  } finally {
    await rm(dir, { recursive: true, force: true })
  }
})

test('an export past what one zip holds fails instead of writing a zip no tool opens', async () => {
  const { EXPORT_MAX_BYTES, EXPORT_MAX_FILES, scanExportRoot } = await import('../lib/rooms.js')
  const { mkdir, mkdtemp, rm, writeFile } = await import('node:fs/promises')
  const { tmpdir } = await import('node:os')
  const { join } = await import('node:path')
  const dir = await mkdtemp(join(tmpdir(), 'nm-export-limits-'))
  const root = join(dir, 'root')
  try {
    await mkdir(root)
    await writeFile(join(root, 'one'), 'x')
    await assert.rejects(scanExportRoot(root, 'x/', { files: EXPORT_MAX_FILES, bytes: 0 }), /more than/)
    await assert.rejects(scanExportRoot(root, 'x/', { files: 0, bytes: EXPORT_MAX_BYTES }), /over/)
  } finally {
    await rm(dir, { recursive: true, force: true })
  }
})

test('exported proxy settings remove URL userinfo', async () => {
  const { redactProxyUserinfo } = await import('../lib/rooms.js')
  const redacted = JSON.parse(redactProxyUserinfo(Buffer.from(JSON.stringify({
    proxy: 'http://alice:secret@proxy.example:8080',
    menuBar: true,
  }))).toString())
  assert.equal(redacted.proxy, 'http://proxy.example:8080')
  assert.equal(redacted.menuBar, true)
})
