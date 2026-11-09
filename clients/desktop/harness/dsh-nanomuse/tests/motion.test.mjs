import assert from 'node:assert/strict'
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { createServer } from 'node:http'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from 'node:test'
import { ANIMATED, AvatarMotion, MOTION_PREFIX, motionPrompt, SECONDS, serveClip } from '../lib/motion.js'

const EP = { host: 'https://example.invalid', apiKey: 'k', model: 'wan2.2-i2v-flash', label: 't', instanceId: 'nanomuse' }

function harness(overrides = {}) {
  const changes = []
  const made = []
  let face = 'abc123'
  let endpoint = EP
  const deps = {
    dir: '',
    endpoint: async () => endpoint,
    animate: () => true,
    faceId: () => face,
    still: async (mood) => ({ bytes: new Uint8Array([1]), mime: 'image/webp', name: `${mood}.webp` }),
    onChange: () => changes.push(1),
    log: () => undefined,
    generate: async (ep, frame, prompt, seconds, options) => {
      made.push({ mood: frame.name.replace('.webp', ''), prompt, seconds, model: ep.model })
      options?.onProgress?.({ kind: 'submitted' })
      return new Uint8Array([0x00, 0x00, 0x00, 0x18, 0x66, 0x74, 0x79, 0x70])
    },
    ...overrides,
  }
  return { deps, changes, made, setFace: (f) => (face = f), setEndpoint: (e) => (endpoint = e) }
}

async function withDir(fn) {
  const dir = await mkdtemp(join(tmpdir(), 'nm-motion-'))
  try {
    await fn(dir)
  } finally {
    await rm(dir, { recursive: true, force: true })
  }
}

test('the contract: four moods in order, four seconds, the phone prompts word for word', () => {
  assert.deepEqual([...ANIMATED], ['idle', 'working', 'waiting', 'happy'])
  assert.equal(SECONDS, 4)
  assert.ok(motionPrompt('idle').startsWith('The character stays in place and gently shakes its head left and right while its round body sways very slightly, calm and friendly, like the idle animation of a mascot. Plain white background'))
  assert.ok(motionPrompt('waiting').startsWith('The character holds a small glowing crystal ball in its hands and plays with it, turning it slowly and peeking into it with curiosity.'))
  assert.ok(motionPrompt('happy').startsWith('The character plays happily with a small golden five-pointed star, tossing it up a little and catching it, bouncing gently with joy.'))
  assert.ok(motionPrompt('working').startsWith('The character wears headphones and types busily on the small laptop in front of it, nodding slightly to the rhythm, focused and content.'))
  assert.ok(motionPrompt('error').startsWith('The character looks a little flustered, a sweat drop on its brow, then takes a breath and settles.'))
  assert.ok(motionPrompt('idle').endsWith('the motion loops naturally with the character back in its starting pose at the end.'))
  assert.ok(motionPrompt('idle').includes('Plain white background, static camera, no zoom, no cuts, soft even studio lighting, the same 3D toy look as the picture throughout, nothing else appears in the frame,'))
})

test('animateAll draws the four clips one after another and reports progress', async () => {
  await withDir(async (dir) => {
    const h = harness()
    h.deps.dir = join(dir, 'motion')
    const motion = new AvatarMotion(h.deps)
    await motion.init()
    assert.deepEqual(motion.view().clips, {})
    assert.equal(await motion.animateAll(), true)
    assert.equal(motion.view().progress.running, true)
    assert.equal(motion.view().progress.total, 4)
    await motion.settled()
    const view = motion.view()
    // `source` (0.1.41): where the run drew, so the Media page can offer nanoMuse Cloud when an own provider failed
    assert.deepEqual(view.progress, { done: 4, total: 4, failed: [], running: false, source: 'nanomuse' })
    assert.deepEqual(Object.keys(view.clips).sort(), ['happy', 'idle', 'waiting', 'working'])
    assert.equal(view.faceId, 'abc123')
    assert.deepEqual(
      h.made.map((m) => m.mood),
      ['idle', 'working', 'waiting', 'happy'],
    )
    assert.ok(h.made.every((m) => m.seconds === 4 && m.model === 'wan2.2-i2v-flash'))
    assert.equal(h.made[0].prompt, motionPrompt('idle'))
    assert.ok(view.clips.idle.size > 0 && view.clips.idle.v > 0)
    assert.equal(motion.clipPath('idle'), join(dir, 'motion', 'idle.mp4'))
    assert.equal(motion.clipPath('error'), undefined)
    const marker = JSON.parse(await readFile(join(dir, 'motion', 'motion.json'), 'utf8'))
    assert.equal(marker.faceId, 'abc123')
    // nothing missing: a second call does nothing; force redraws
    assert.equal(await motion.animateAll(), false)
    assert.equal(await motion.animateAll(true), true)
    await motion.settled()
    assert.equal(h.made.length, 8)
  })
})

test('a failed clip is marked and the rest carry on; the still fills in', async () => {
  await withDir(async (dir) => {
    const h = harness({
      generate: async (ep, frame) => {
        if (frame.name === 'waiting.webp') throw new Error('Video task FAILED')
        return new Uint8Array([1, 2])
      },
    })
    h.deps.dir = join(dir, 'motion')
    const motion = new AvatarMotion(h.deps)
    await motion.init()
    await motion.animateAll()
    await motion.settled()
    const view = motion.view()
    assert.deepEqual(view.progress.failed, ['waiting'])
    assert.equal(view.progress.error, 'Video task FAILED')
    assert.deepEqual(Object.keys(view.clips).sort(), ['happy', 'idle', 'working'])
    assert.equal(motion.clipPath('waiting'), undefined)
    motion.clearProgress()
    assert.equal(motion.view().progress, null)
  })
})

test('no endpoint, no face or animation off: nothing is drawn', async () => {
  await withDir(async (dir) => {
    const h = harness()
    h.deps.dir = join(dir, 'motion')
    const motion = new AvatarMotion(h.deps)
    await motion.init()
    h.setEndpoint(undefined)
    assert.equal(await motion.animateAll(), false)
    assert.equal(await motion.faceChanged(), false)
    h.setEndpoint(EP)
    h.setFace('')
    assert.equal(await motion.animateAll(), false)
    h.setFace('abc123')
    h.deps.animate = () => false
    assert.equal(await motion.faceChanged(), false)
    assert.equal(h.made.length, 0)
  })
})

test('a new face throws the old clips away and draws again; the dragon just clears', async () => {
  await withDir(async (dir) => {
    const h = harness()
    h.deps.dir = join(dir, 'motion')
    const motion = new AvatarMotion(h.deps)
    await motion.init()
    assert.equal(await motion.faceChanged(), true)
    await motion.settled()
    assert.equal(Object.keys(motion.view().clips).length, 4)
    // the same face again: nothing happens
    assert.equal(await motion.faceChanged(), false)
    h.setFace('def456')
    assert.equal(await motion.faceChanged(), true)
    assert.equal(motion.clipPath('idle'), undefined, 'old clips do not serve for the new face')
    await motion.settled()
    assert.equal(motion.view().faceId, 'def456')
    assert.equal(h.made.length, 8)
    // back to the dragon: cleared, nothing drawn
    h.setFace('')
    assert.equal(await motion.faceChanged(), false)
    assert.deepEqual(motion.view().clips, {})
    await assert.rejects(readFile(join(dir, 'motion', 'idle.mp4')))
  })
})

test('init reads clips left from an earlier run, and the marker says whose they are', async () => {
  await withDir(async (dir) => {
    const h = harness()
    h.deps.dir = join(dir, 'motion')
    const first = new AvatarMotion(h.deps)
    await first.init()
    await first.animateAll()
    await first.settled()
    const second = new AvatarMotion(h.deps)
    await second.init()
    assert.equal(Object.keys(second.view().clips).length, 4)
    assert.equal(second.view().faceId, 'abc123')
    assert.equal(await second.animateAll(), false)
  })
})

test('cancel stops after the current clip', async () => {
  await withDir(async (dir) => {
    let release
    const gate = new Promise((r) => (release = r))
    const h = harness({
      generate: async () => {
        await gate
        return new Uint8Array([1])
      },
    })
    h.deps.dir = join(dir, 'motion')
    const motion = new AvatarMotion(h.deps)
    await motion.init()
    await motion.animateAll()
    motion.cancel()
    assert.equal(motion.view().progress.running, false)
    release()
    await new Promise((r) => setTimeout(r, 20))
    assert.deepEqual(motion.view().clips, {})
  })
})

test('serveClip: the worn face only, mp4 with a long cache when the URL carries the mtime', async () => {
  await withDir(async (dir) => {
    const file = join(dir, 'idle.mp4')
    await writeFile(file, new Uint8Array([1, 2, 3]))
    const server = createServer((req, res) => void serveClip(req, res, (mood) => (mood === 'idle' ? file : undefined)))
    await new Promise((r) => server.listen(0, '127.0.0.1', r))
    const base = `http://127.0.0.1:${server.address().port}${MOTION_PREFIX}`
    try {
      const ok = await fetch(`${base}/motion/idle.mp4?v=123`)
      assert.equal(ok.status, 200)
      assert.equal(ok.headers.get('content-type'), 'video/mp4')
      assert.match(ok.headers.get('cache-control'), /immutable/)
      assert.equal((await ok.arrayBuffer()).byteLength, 3)
      const plain = await fetch(`${base}/motion/idle.mp4`)
      assert.equal(plain.headers.get('cache-control'), 'no-cache')
      assert.equal((await fetch(`${base}/motion/happy.mp4`)).status, 404)
      assert.equal((await fetch(`${base}/motion/../idle.mp4`)).status, 404)
      assert.equal((await fetch(`${base}/motion/idle.mp4`, { method: 'POST' })).status, 404)
    } finally {
      server.close()
    }
  })
})

test('a clip drawn here is handed to `share`; clips the account lists are adopted for the moods missing, never for another face or during a run', async () => {
  await withDir(async (dir) => {
    const shared = []
    const h = harness({ share: async (mood, bytes) => { shared.push({ mood, size: bytes.length }) } })
    h.deps.dir = join(dir, 'motion')
    const motion = new AvatarMotion(h.deps)
    await motion.init()
    assert.equal(await motion.animateAll(), true)
    await motion.settled()
    assert.deepEqual(shared.map((s) => s.mood), ['idle', 'working', 'waiting', 'happy'])
    assert.ok(shared.every((s) => s.size === 8))

    // another device's clips for a new face: only the moods not on disk are fetched
    const fetched = []
    const clip = (mood) => async () => { fetched.push(mood); return new Uint8Array([9, 9, 9, mood.length]) }
    h.setFace('def456')
    assert.equal(await motion.adopt('abc123', { idle: clip('idle') }), 0, 'clips of a face not worn are not taken')
    assert.deepEqual(fetched, [])
    assert.equal(await motion.adopt('def456', { idle: clip('idle'), waiting: clip('waiting'), happy: async () => undefined }), 2)
    assert.deepEqual(fetched, ['idle', 'waiting'])
    const view = motion.view()
    assert.equal(view.faceId, 'def456', 'the clips on disk now belong to the new face')
    assert.deepEqual(Object.keys(view.clips).sort(), ['idle', 'waiting'])
    assert.equal((await readFile(join(dir, 'motion', 'idle.mp4'))).length, 4)
    assert.equal(JSON.parse(await readFile(join(dir, 'motion', 'motion.json'), 'utf8')).from, 'account')
    // a second listing adds only what is missing; an existing clip is not fetched again
    fetched.length = 0
    assert.equal(await motion.adopt('def456', { idle: clip('idle'), working: clip('working') }), 1)
    assert.deepEqual(fetched, ['working'])
    // what the account did not have is drawn, and only that
    h.made.length = 0
    assert.equal(await motion.animateAll(), true)
    await motion.settled()
    assert.deepEqual(h.made.map((m) => m.mood), ['happy'])
    assert.equal(await motion.animateAll(), false, 'all four are here now')
  })
})
