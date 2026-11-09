// A face drawn with your own provider while signed out is worn on this computer
// (`ProfileStore.wearDrawn`, #521): the stills land under `faces/<id>/`, a missing mood
// shows the idle still, the look is written to profile.json, and the relay is never asked.
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { mkdtemp, readFile, readdir, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { MOODS, ProfileStore } from '../lib/profile.js'

const relayNeverAsked = {
  async profile() { throw new Error('the relay must not be asked while signed out') },
}

test('a drawn face is worn locally without the relay', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'nm-profile-'))
  try {
    const store = new ProfileStore(dir, relayNeverAsked)
    await store.load()
    const idle = Buffer.from('idle-still').toString('base64')
    const happy = Buffer.from('happy-still').toString('base64')
    const faceId = await store.wearDrawn({ idle, happy }, 'a red fox in a scarf', 'muse')
    assert.match(faceId, /^[a-f0-9]{16}$/)

    const worn = store.current()
    assert.equal(worn.avatar, 'face')
    assert.equal(worn.faceId, faceId)
    assert.equal(worn.description, 'a red fox in a scarf')
    assert.equal(worn.style, 'muse')
    assert.equal(worn.rev, 0, 'nothing was written to the account')

    const folder = join(dir, 'faces', faceId)
    assert.deepEqual((await readdir(folder)).sort(), [...MOODS.map((m) => `${m}.webp`), 'face.json'].sort())
    assert.equal((await readFile(join(folder, 'happy.webp'))).toString(), 'happy-still')
    assert.equal((await readFile(join(folder, 'working.webp'))).toString(), 'idle-still', 'a mood that was not drawn shows the idle still')
    assert.equal(JSON.parse(await readFile(join(folder, 'face.json'), 'utf8')).from, 'local')
    assert.equal(store.stillPath(faceId, 'idle'), join(folder, 'idle.webp'))

    const saved = JSON.parse(await readFile(join(dir, 'profile.json'), 'utf8'))
    assert.equal(saved.faceId, faceId)

    // The same idle still is the same face; a new one replaces the old folder.
    assert.equal(await store.wearDrawn({ idle }, 'again', 'muse'), faceId)
    const other = await store.wearDrawn({ idle: Buffer.from('another').toString('base64') }, 'an owl', 'ink')
    assert.notEqual(other, faceId)
    assert.deepEqual(await readdir(join(dir, 'faces')), [other])
  } finally {
    await rm(dir, { recursive: true, force: true })
  }
})

test('a face without an idle still is refused', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'nm-profile-'))
  try {
    const store = new ProfileStore(dir, relayNeverAsked)
    await store.load()
    assert.equal(await store.wearDrawn({ happy: Buffer.from('x').toString('base64') }, 'a fox', 'muse'), undefined)
    assert.equal(await store.wearDrawn({ idle: '' }, 'a fox', 'muse'), undefined)
    assert.equal(store.current().avatar, 'dragon')
  } finally {
    await rm(dir, { recursive: true, force: true })
  }
})
