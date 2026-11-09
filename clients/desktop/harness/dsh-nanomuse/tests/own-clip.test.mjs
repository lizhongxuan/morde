// A clip of the person's own as the face (#324): the rule (MP4 or MOV with H.264 or HEVC video,
// at most 15 seconds and 30 MB, format before length before size), the look at the bytes, the
// pending copy that the preview plays and "Use" or "Cancel" settle, and the route that
// serves the clip, its first frame and the pending copy.
import assert from 'node:assert/strict'
import { mkdtemp, readFile, rm, stat, writeFile } from 'node:fs/promises'
import { createServer, request } from 'node:http'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from 'node:test'
import { MOTION_PREFIX, serveClip } from '../lib/motion.js'
import { OWN_CLIP_MAX_BYTES, OWN_CLIP_MAX_SECONDS, OwnClip, checkOwnClip, probeOwnClip } from '../lib/own-clip.js'

// ---- a small MP4 writer: boxes as the probe reads them ------------------------------------

function box(type, ...parts) {
  const body = Buffer.concat(parts.map((p) => (typeof p === 'string' ? Buffer.from(p, 'latin1') : p)))
  const head = Buffer.alloc(8)
  head.writeUInt32BE(body.length + 8, 0)
  head.write(type, 4, 'latin1')
  return Buffer.concat([head, body])
}

/** The same box with the 64-bit size field (`size` 1, then the length after the type). */
function largeBox(type, ...parts) {
  const body = Buffer.concat(parts.map((p) => (typeof p === 'string' ? Buffer.from(p, 'latin1') : p)))
  const head = Buffer.alloc(16)
  head.writeUInt32BE(1, 0)
  head.write(type, 4, 'latin1')
  head.writeBigUInt64BE(BigInt(body.length + 16), 8)
  return Buffer.concat([head, body])
}

/** The same box with size 0: it runs to the end of the file, so it must be the last one. */
function lastBox(type, ...parts) {
  const out = box(type, ...parts)
  out.writeUInt32BE(0, 0)
  return out
}

function u32(n) {
  const b = Buffer.alloc(4)
  b.writeUInt32BE(n, 0)
  return b
}

function u64(n) {
  const b = Buffer.alloc(8)
  b.writeBigUInt64BE(BigInt(n), 0)
  return b
}

/** `mvhd` version 0: timescale and duration, the rest zero. */
function mvhd(timescale, duration, version = 0) {
  if (version === 1) return box('mvhd', Buffer.from([1, 0, 0, 0]), u64(0), u64(0), u32(timescale), u64(duration), Buffer.alloc(80))
  return box('mvhd', u32(0), u32(0), u32(0), u32(timescale), u32(duration), Buffer.alloc(80))
}

/** `mvex` with a `mehd`, the length of a fragmented file; without `mehd` when `duration` is undefined. */
function mvex(duration, version = 0) {
  if (duration === undefined) return box('mvex', box('trex', Buffer.alloc(24)))
  const mehd = version === 1 ? box('mehd', Buffer.from([1, 0, 0, 0]), u64(duration)) : box('mehd', u32(0), u32(duration))
  return box('mvex', mehd, box('trex', Buffer.alloc(24)))
}

function trak(handler, codec) {
  const stsd = box('stsd', u32(0), u32(1), box(codec, Buffer.alloc(78)))
  const hdlr = box('hdlr', u32(0), u32(0), handler, Buffer.alloc(13))
  return box('trak', box('mdia', hdlr, box('minf', box('stbl', stsd))))
}

/** An MP4 of `ms` milliseconds whose first video track is `codec`, with `extra` bytes of media to pad it. */
function mp4({ ms = 4000, codec = 'avc1', brand = 'isom', extra = 64, audioFirst = false, version = 0, faststart = true } = {}) {
  const ftyp = box('ftyp', brand, u32(0), 'isom', 'mp42')
  const tracks = audioFirst ? [trak('soun', 'mp4a'), trak('vide', codec)] : [trak('vide', codec)]
  const moov = box('moov', mvhd(1000, ms, version), ...tracks)
  const mdat = box('mdat', Buffer.alloc(extra, 7))
  return faststart ? Buffer.concat([ftyp, moov, mdat]) : Buffer.concat([ftyp, mdat, moov])
}

async function withDir(fn) {
  const dir = await mkdtemp(join(tmpdir(), 'nm-own-clip-'))
  try {
    await fn(dir)
  } finally {
    await rm(dir, { recursive: true, force: true })
  }
}

function store(dir) {
  const changes = []
  const own = new OwnClip({ dir: join(dir, 'avatar'), onChange: () => changes.push(own.view()) })
  return { own, changes }
}

/** Sends `body` to `handler` as the request of a POST and returns what the handler returned. */
async function post(handler, body, headers = {}) {
  let outcome
  const server = createServer(async (req, res) => {
    outcome = await handler(req)
    res.writeHead(204).end()
  })
  await new Promise((ok) => server.listen(0, '127.0.0.1', ok))
  const { port } = server.address()
  await new Promise((ok, no) => {
    const req = request({ host: '127.0.0.1', port, method: 'POST', path: '/x', headers }, (res) => {
      res.resume()
      res.on('end', ok)
    })
    req.on('error', no)
    if (typeof body === 'function') body(req)
    else req.end(body)
  })
  await new Promise((ok) => server.close(ok))
  return outcome
}

// ---- the rule ---------------------------------------------------------------------------

test('the limits are the phones\u2019: 15 seconds and 30 MB', () => {
  assert.equal(OWN_CLIP_MAX_SECONDS, 15)
  assert.equal(OWN_CLIP_MAX_BYTES, 30 * 1024 * 1024)
})

test('a clip that rounds down to 15 s is taken, one of 16 s is too long', () => {
  const ok = { container: true, codec: 'avc1', durationMs: 15_999, bytes: 1000 }
  assert.equal(checkOwnClip(ok), undefined)
  assert.equal(checkOwnClip({ ...ok, durationMs: 16_000 }), 'too_long')
})

test('exactly 30 MB is taken, one byte more is too large', () => {
  const ok = { container: true, codec: 'avc1', durationMs: 1000, bytes: OWN_CLIP_MAX_BYTES }
  assert.equal(checkOwnClip(ok), undefined)
  assert.equal(checkOwnClip({ ...ok, bytes: OWN_CLIP_MAX_BYTES + 1 }), 'too_large')
})

test('anything but H.264 or HEVC in an MP4 or MOV is a format problem, and it comes before length and size', () => {
  const base = { container: true, codec: 'avc1', durationMs: 1000, bytes: 1000 }
  // HEVC is taken here (docs/every-device.md: where the device can play it); the preview decides whether this computer can
  assert.equal(checkOwnClip({ ...base, codec: 'hvc1' }), undefined)
  assert.equal(checkOwnClip({ ...base, codec: 'hev1' }), undefined)
  assert.equal(checkOwnClip({ ...base, codec: 'vp09' }), 'format')
  assert.equal(checkOwnClip({ ...base, codec: 'av01' }), 'format')
  assert.equal(checkOwnClip({ ...base, container: false }), 'format')
  assert.equal(checkOwnClip({ container: true, bytes: 1000 }), 'format')
  // a WebM that is also too long and too large is refused for its format, not its size
  assert.equal(checkOwnClip({ container: false, codec: 'vp09', durationMs: 60_000, bytes: OWN_CLIP_MAX_BYTES * 2 }), 'format')
  // too long and too large: the length is named first
  assert.equal(checkOwnClip({ ...base, durationMs: 60_000, bytes: OWN_CLIP_MAX_BYTES * 2 }), 'too_long')
  assert.equal(checkOwnClip({ ...base, codec: 'avc3' }), undefined)
})

// ---- the look at the bytes --------------------------------------------------------------

test('probe reads the length and the codec of an MP4, faststart or not, and a MOV', () => {
  assert.deepEqual(probeOwnClip(mp4({ ms: 4321 })), { container: true, bytes: mp4({ ms: 4321 }).length, durationMs: 4321, codec: 'avc1' })
  const late = probeOwnClip(mp4({ ms: 4321, faststart: false }))
  assert.equal(late.durationMs, 4321)
  assert.equal(late.codec, 'avc1')
  const mov = probeOwnClip(mp4({ ms: 2500, brand: 'qt  ' }))
  assert.equal(mov.container, true)
  assert.equal(mov.durationMs, 2500)
  const v1 = probeOwnClip(mp4({ ms: 12_000, version: 1 }))
  assert.equal(v1.durationMs, 12_000)
})

test('probe finds the video track behind an audio track, and reports other codecs as they are', () => {
  assert.equal(probeOwnClip(mp4({ audioFirst: true })).codec, 'avc1')
  assert.equal(probeOwnClip(mp4({ codec: 'hvc1' })).codec, 'hvc1')
  assert.equal(probeOwnClip(mp4({ codec: 'vp09' })).codec, 'vp09')
})

test('probe reads a moov with a 64-bit size, a file that opens with wide and free atoms, and an mdat that runs to the end', () => {
  const ftyp = box('ftyp', 'isom', u32(0), 'isom', 'mp42')
  const large = Buffer.concat([ftyp, largeBox('moov', mvhd(1000, 7000), trak('vide', 'avc1')), box('mdat', Buffer.alloc(64, 7))])
  assert.deepEqual(probeOwnClip(large), { container: true, bytes: large.length, durationMs: 7000, codec: 'avc1' })
  // an old QuickTime file: no ftyp, a wide and a free atom before the movie
  const quicktime = Buffer.concat([box('wide'), box('free', Buffer.alloc(16)), box('mdat', Buffer.alloc(64, 7)), box('moov', mvhd(600, 1800), trak('vide', 'avc1'))])
  assert.deepEqual(probeOwnClip(quicktime), { container: true, bytes: quicktime.length, durationMs: 3000, codec: 'avc1' })
  // the media as the last box with size 0, which means "to the end of the file"
  const open = Buffer.concat([ftyp, box('moov', mvhd(1000, 2000), trak('vide', 'avc3')), lastBox('mdat', Buffer.alloc(64, 7))])
  assert.deepEqual(probeOwnClip(open), { container: true, bytes: open.length, durationMs: 2000, codec: 'avc3' })
  // a 64-bit size the file cannot hold ends the look without a throw
  const lying = Buffer.concat([ftyp, largeBox('moov', mvhd(1000, 2000), trak('vide', 'avc1'))])
  lying.writeBigUInt64BE(0xfffffffffffffn, ftyp.length + 8)
  assert.equal(probeOwnClip(lying).container, true)
  assert.equal(probeOwnClip(lying).codec, undefined)
})

test('probe takes the length of a fragmented file from mvex/mehd, and has none without it', () => {
  const ftyp = box('ftyp', 'iso5', u32(0), 'iso5', 'dash')
  const fragmented = (header, ex) => Buffer.concat([ftyp, box('moov', header, trak('vide', 'avc1'), ex), box('moof', Buffer.alloc(16)), box('mdat', Buffer.alloc(64, 7))])
  // the movie header says 0 (or all ones, "unknown"), the extends header has the length
  assert.equal(probeOwnClip(fragmented(mvhd(1000, 0), mvex(9500))).durationMs, 9500)
  assert.equal(probeOwnClip(fragmented(mvhd(1000, 0xffffffff), mvex(9500, 1))).durationMs, 9500)
  const unknown64 = box('mvhd', Buffer.from([1, 0, 0, 0]), u64(0), u64(0), u32(1000), Buffer.alloc(8, 0xff), Buffer.alloc(80))
  assert.equal(probeOwnClip(fragmented(unknown64, mvex(12_000))).durationMs, 12_000)
  // without mehd the length is not in the file, and the rule refuses it as a format problem
  const open = probeOwnClip(fragmented(mvhd(1000, 0), mvex(undefined)))
  assert.equal(open.codec, 'avc1')
  assert.equal(open.durationMs, undefined)
  assert.equal(checkOwnClip(open), 'format')
  // a mehd that says 16 s is too long, as a mvhd would be
  assert.equal(checkOwnClip(probeOwnClip(fragmented(mvhd(1000, 0), mvex(16_000)))), 'too_long')
})

test('probe calls a WebM, a text file and an empty file no container at all', () => {
  const webm = Buffer.concat([Buffer.from([0x1a, 0x45, 0xdf, 0xa3]), Buffer.alloc(64, 1)])
  assert.equal(probeOwnClip(webm).container, false)
  assert.equal(probeOwnClip(Buffer.from('hello there, not a video at all')).container, false)
  assert.equal(probeOwnClip(Buffer.alloc(0)).container, false)
  // a box that claims more bytes than there are does not break the look
  const cut = mp4().subarray(0, 40)
  assert.equal(probeOwnClip(cut).container, true)
  assert.equal(probeOwnClip(cut).codec, undefined)
  assert.equal(checkOwnClip(probeOwnClip(cut)), 'format')
})

// ---- the pending copy, "Use", "Cancel", "Default face" ------------------------------------

test('stage keeps a clip the rule takes as the pending copy; use makes it the clip and keeps the first frame', async () => {
  await withDir(async (dir) => {
    const { own, changes } = store(dir)
    await own.init()
    assert.equal(own.view(), null)
    const bytes = mp4({ ms: 15_400 })
    const staged = await post((req) => own.stage(req), bytes, { 'content-length': String(bytes.length) })
    assert.equal(staged.ok, true)
    assert.equal(staged.pending.size, bytes.length)
    assert.equal(staged.pending.durationMs, 15_400)
    assert.ok(staged.pending.v > 0)
    assert.equal(own.view(), null, 'not the clip yet')
    assert.equal(own.path('own-clip.pending.mp4'), join(dir, 'avatar', 'own-clip.pending.mp4'))
    assert.equal(own.path('own-clip.mp4'), undefined)
    const poster = Buffer.from([0x89, 0x50, 0x4e, 0x47])
    assert.equal(await own.use(poster), true)
    const view = own.view()
    assert.equal(view.size, bytes.length)
    assert.equal(view.poster, true)
    assert.ok(view.v > 0)
    assert.deepEqual(await readFile(join(dir, 'avatar', 'own-clip.mp4')), bytes)
    assert.deepEqual(await readFile(join(dir, 'avatar', 'own-clip.png')), poster)
    await assert.rejects(stat(join(dir, 'avatar', 'own-clip.pending.mp4')))
    assert.equal(own.path('own-clip.mp4'), join(dir, 'avatar', 'own-clip.mp4'))
    assert.equal(own.path('own-clip.png'), join(dir, 'avatar', 'own-clip.png'))
    assert.equal(own.path('motion/idle.mp4'), undefined)
    assert.ok(changes.length >= 1)
    // use with nothing pending changes nothing
    assert.equal(await own.use(), false)
    assert.equal(own.view().size, bytes.length)
  })
})

test('discard drops the pending copy and leaves the clip; clear removes everything', async () => {
  await withDir(async (dir) => {
    const { own } = store(dir)
    await own.init()
    await post((req) => own.stage(req), mp4({ ms: 3000 }))
    await own.use()
    assert.equal(own.view().poster, false, 'no first frame handed over')
    const before = own.view()
    await post((req) => own.stage(req), mp4({ ms: 5000, extra: 1024 }))
    await own.discard()
    await assert.rejects(stat(join(dir, 'avatar', 'own-clip.pending.mp4')))
    assert.deepEqual(own.view(), before)
    await own.clear()
    assert.equal(own.view(), null)
    await assert.rejects(stat(join(dir, 'avatar', 'own-clip.mp4')))
    await assert.rejects(stat(join(dir, 'avatar', 'own-clip.png')))
  })
})

test('five mood overrides survive restart, retain the legacy clip, and clear independently', async () => {
  await withDir(async (dir) => {
    const { own } = store(dir)
    await own.init()
    await post((req) => own.stage(req), mp4({ ms: 1000 }))
    await own.use()
    for (const mood of ['idle', 'working', 'waiting', 'happy', 'error']) {
      await post((req) => own.stage(req), mp4({ ms: 2000 }))
      assert.equal(await own.use(Buffer.from('poster'), mood), true)
      assert.equal(own.moods()[mood]?.poster, true)
      assert.ok(own.path(`own-clip-${mood}.mp4`))
      assert.ok(own.path(`own-clip-${mood}.png`))
    }
    assert.equal(own.path('own-clip-unknown.mp4'), undefined)
    const restarted = store(dir).own
    await restarted.init()
    assert.equal(Object.keys(restarted.moods()).length, 5)
    assert.ok(restarted.view(), 'the legacy video remains in place')
    await restarted.clear('working')
    assert.equal(restarted.moods().working, undefined)
    assert.equal(Object.keys(restarted.moods()).length, 4)
    assert.ok(restarted.view(), 'clearing one mood does not remove the legacy video')
    await restarted.clear()
    assert.deepEqual(restarted.moods(), {})
    assert.equal(restarted.view(), null)
  })
})

test('a stale pending copy is dropped on init, and an empty clip file counts as none', async () => {
  await withDir(async (dir) => {
    const avatar = join(dir, 'avatar')
    const first = store(dir)
    await first.own.init()
    await post((req) => first.own.stage(req), mp4())
    assert.ok(await stat(join(avatar, 'own-clip.pending.mp4')))
    const second = store(dir)
    await second.own.init()
    await assert.rejects(stat(join(avatar, 'own-clip.pending.mp4')))
    await writeFile(join(avatar, 'own-clip.mp4'), Buffer.alloc(0))
    await second.own.rescan()
    assert.equal(second.own.view(), null)
  })
})

test('stage refuses a WebM, a 16 s clip and an AV1 clip, and keeps nothing of them; an HEVC clip is staged', async () => {
  await withDir(async (dir) => {
    const { own } = store(dir)
    await own.init()
    const webm = Buffer.concat([Buffer.from([0x1a, 0x45, 0xdf, 0xa3]), Buffer.alloc(64, 1)])
    const a = await post((req) => own.stage(req), webm)
    assert.equal(a.ok, false)
    assert.equal(a.why, 'format')
    const b = await post((req) => own.stage(req), mp4({ ms: 16_000 }))
    assert.equal(b.ok, false)
    assert.equal(b.why, 'too_long')
    assert.equal(b.probe.durationMs, 16_000)
    const c = await post((req) => own.stage(req), mp4({ codec: 'av01' }))
    assert.equal(c.ok, false)
    assert.equal(c.why, 'format')
    await assert.rejects(stat(join(dir, 'avatar', 'own-clip.pending.mp4')))
    assert.equal(own.view(), null)
    const d = await post((req) => own.stage(req), mp4({ codec: 'hvc1' }))
    assert.equal(d.ok, true)
    await stat(join(dir, 'avatar', 'own-clip.pending.mp4'))
    await own.discard()
    await assert.rejects(stat(join(dir, 'avatar', 'own-clip.pending.mp4')))
  })
})

test('stage refuses a body over 30 MB by its declared length before reading it, and a chunked one as it arrives', async () => {
  await withDir(async (dir) => {
    const { own } = store(dir)
    await own.init()
    const small = await post((req) => own.stage(req), Buffer.alloc(16), { 'content-length': '16' })
    assert.equal(small.ok, false, 'a small declared body goes through to the probe, which finds no video')
    assert.equal(small.why, 'format')
    const big = await post((req) => own.stage(req), Buffer.alloc(OWN_CLIP_MAX_BYTES + 1), { 'content-length': String(OWN_CLIP_MAX_BYTES + 1) })
    assert.equal(big.ok, false)
    assert.equal(big.why, 'too_large')
    assert.equal(big.probe.bytes, OWN_CLIP_MAX_BYTES + 1)
    // chunked, no length: the cap is hit while the body streams in
    const chunk = Buffer.alloc(1024 * 1024, 3)
    const streamed = await post((req) => own.stage(req), (req) => {
      for (let i = 0; i < 31; i++) req.write(chunk)
      req.end()
    })
    assert.equal(streamed.ok, false)
    assert.equal(streamed.why, 'too_large')
    assert.ok(streamed.probe.bytes > OWN_CLIP_MAX_BYTES)
    await assert.rejects(stat(join(dir, 'avatar', 'own-clip.pending.mp4')))
  })
})

// ---- the route --------------------------------------------------------------------------

test('serveClip hands out the own clip, its first frame and the pending copy, and 404 for the rest', async () => {
  await withDir(async (dir) => {
    const { own } = store(dir)
    await own.init()
    const bytes = mp4({ ms: 2000 })
    await post((req) => own.stage(req), bytes)
    const server = createServer((req, res) => serveClip(req, res, () => undefined, (name) => own.path(name)))
    await new Promise((ok) => server.listen(0, '127.0.0.1', ok))
    const { port } = server.address()
    const get = async (path) => {
      const res = await fetch(`http://127.0.0.1:${port}${MOTION_PREFIX}${path}`)
      return { status: res.status, type: res.headers.get('content-type'), cache: res.headers.get('cache-control'), body: Buffer.from(await res.arrayBuffer()) }
    }
    try {
      const pending = await get('/own-clip.pending.mp4?v=1')
      assert.equal(pending.status, 200)
      assert.equal(pending.type, 'video/mp4')
      assert.deepEqual(pending.body, bytes)
      assert.equal((await get('/own-clip.mp4')).status, 404, 'not the clip until Use')
      await own.use(Buffer.from([1, 2, 3]))
      const clip = await get(`/own-clip.mp4?v=${own.view().v}`)
      assert.equal(clip.status, 200)
      assert.deepEqual(clip.body, bytes)
      assert.match(clip.cache, /immutable/)
      const poster = await get(`/own-clip.png?v=${own.view().v}`)
      assert.equal(poster.status, 200)
      assert.equal(poster.type, 'image/png')
      assert.deepEqual(poster.body, Buffer.from([1, 2, 3]))
      assert.equal((await get('/own-clip.pending.mp4')).status, 404)
      assert.equal((await get('/own-clip.txt')).status, 404)
      assert.equal((await get('/../own-clip.mp4')).status, 404)
      assert.equal((await get('/motion/idle.mp4')).status, 404)
      await own.clear()
      assert.equal((await get('/own-clip.mp4')).status, 404)
      assert.equal((await get('/own-clip.png')).status, 404)
    } finally {
      await new Promise((ok) => server.close(ok))
    }
  })
})

test('without an own-clip path the route is as it was: only the moods', async () => {
  const server = createServer((req, res) => serveClip(req, res, () => undefined))
  await new Promise((ok) => server.listen(0, '127.0.0.1', ok))
  const { port } = server.address()
  try {
    const res = await fetch(`http://127.0.0.1:${port}${MOTION_PREFIX}/own-clip.mp4`)
    assert.equal(res.status, 404)
  } finally {
    await new Promise((ok) => server.close(ok))
  }
})
