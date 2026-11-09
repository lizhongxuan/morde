/**
 * A clip of the person's own as the face (#324). One short video from this computer plays
 * muted in a loop wherever the face moves: for the mood at rest, and for every other mood
 * too, since there is only the one clip. The stills keep their place where a still is
 * shown: a drawn face's mood pictures sit under the clip until its first frame is on
 * screen; with no drawn face the clip's own first frame (a PNG the browser takes at the
 * preview) is the still.
 *
 * The clip lives next to the face's clips as `$DSH_HOME/nanomuse/avatar/own-clip.mp4`,
 * its first frame as `own-clip.png`, and nowhere else: it is this computer's alone, never
 * part of the profile the relay holds and never synced. It survives a new drawn face (the
 * face's clips under `avatar/motion/` are the ones that go) and only "Default face"
 * removes it. The browser plays it from `/nanomuse/avatar/own-clip.mp4?v=<mtime>`.
 *
 * The rule is the one every device follows (`docs/every-device.md`): MP4 or MOV, at most
 * 15 seconds and 30 MB, with H.264 video, or HEVC where the device can play it; nothing is
 * transcoded. The format first (an unreadable file is a format problem whatever its size),
 * then the length, then the size. Node has no decoder, so the file is read as ISO base
 * media boxes: `ftyp`, then `moov/mvhd` for the length and the video track's `stsd` sample
 * entry for the codec. Whether this computer plays HEVC is not known here (Chromium decodes
 * it through the system's decoder, which Windows and macOS have and most Linux builds do
 * not), so an HEVC clip is staged and the preview decides: a clip the browser cannot play
 * is discarded there and refused in one sentence (`client/OwnClipChooser.tsx`).
 */
import { createWriteStream } from 'node:fs'
import { mkdir, readFile, rename, rm, stat, writeFile } from 'node:fs/promises'
import type { IncomingMessage } from 'node:http'
import { join } from 'node:path'
import { pipeline } from 'node:stream/promises'

/** The longest clip taken, in whole seconds; a clip that rounds down to this is fine. */
export const OWN_CLIP_MAX_SECONDS = 15
/** The largest file taken. */
export const OWN_CLIP_MAX_BYTES = 30 * 1024 * 1024
/** The sample entry codes of the video the face takes: H.264 (`avc1`, `avc3`) and HEVC (`hvc1`, `hev1`). */
const VIDEO_CODECS = new Set(['avc1', 'avc3', 'hvc1', 'hev1'])
/** Boxes whose children are boxes. */
const CONTAINERS = new Set(['moov', 'trak', 'mdia', 'minf', 'stbl', 'mvex'])
/** What a file may start with and still be an MP4 or a MOV (an old QuickTime file has no `ftyp`). */
const FIRST_BOXES = new Set(['ftyp', 'moov', 'mdat', 'wide', 'free', 'skip'])

export type OwnClipRefusal = 'format' | 'too_long' | 'too_large'

/** What a look at a file found; an absent field is one that could not be read. */
export interface OwnClipProbe {
  /** True when the file reads as ISO base media (MP4 or MOV). */
  container: boolean
  codec?: string
  durationMs?: number
  bytes: number
}

/** What the browser sees (`live.ownClip`): the clip's size, the cache key, and whether a first frame was kept. */
export interface OwnClipView {
  size: number
  /** Modification time in ms, the browser's cache key. */
  v: number
  poster: boolean
}

export const OWN_CLIP_MOODS = ['idle', 'working', 'waiting', 'happy', 'error'] as const
export type OwnClipMood = (typeof OWN_CLIP_MOODS)[number]
export function isOwnClipMood(value: unknown): value is OwnClipMood {
  return typeof value === 'string' && (OWN_CLIP_MOODS as readonly string[]).includes(value)
}

export type OwnClipStaged =
  | { ok: true; pending: { v: number; size: number; durationMs: number } }
  | { ok: false; why: OwnClipRefusal; probe: OwnClipProbe }

/** The rule: nothing means the clip is taken. */
export function checkOwnClip(p: OwnClipProbe): OwnClipRefusal | undefined {
  if (!p.container || !p.codec || p.durationMs === undefined) return 'format'
  if (!VIDEO_CODECS.has(p.codec)) return 'format'
  if (p.durationMs >= (OWN_CLIP_MAX_SECONDS + 1) * 1000) return 'too_long'
  if (p.bytes > OWN_CLIP_MAX_BYTES) return 'too_large'
  return undefined
}

interface Box {
  type: string
  /** Where the box's own content starts. */
  start: number
  end: number
}

function* boxes(buf: Buffer, from: number, to: number): Generator<Box> {
  let at = from
  while (at + 8 <= to) {
    let size = buf.readUInt32BE(at)
    const type = buf.toString('latin1', at + 4, at + 8)
    let head = 8
    if (size === 1) {
      if (at + 16 > to) return
      const large = buf.readBigUInt64BE(at + 8)
      if (large > BigInt(Number.MAX_SAFE_INTEGER)) return
      size = Number(large)
      head = 16
    } else if (size === 0) {
      size = to - at
    }
    if (size < head || at + size > to) return
    yield { type, start: at + head, end: at + size }
    at += size
  }
}

function find(buf: Buffer, from: number, to: number, type: string): Box | undefined {
  for (const box of boxes(buf, from, to)) if (box.type === type) return box
  return undefined
}

/** `mvhd` (or `mehd`): the length in ms, or undefined when it is not there or zero. */
function lengthOf(buf: Buffer, moov: Box): number | undefined {
  const mvhd = find(buf, moov.start, moov.end, 'mvhd')
  if (mvhd && mvhd.end - mvhd.start >= 20) {
    const version = buf[mvhd.start]
    let timescale: number
    let duration: number
    if (version === 1) {
      if (mvhd.end - mvhd.start < 32) return undefined
      timescale = buf.readUInt32BE(mvhd.start + 20)
      const d = buf.readBigUInt64BE(mvhd.start + 24)
      // all ones is "unknown" in a version 1 header, as 0xffffffff is in a version 0 one
      duration = d === 0xffffffffffffffffn ? 0 : Number(d)
    } else {
      timescale = buf.readUInt32BE(mvhd.start + 12)
      duration = buf.readUInt32BE(mvhd.start + 16)
    }
    if (timescale > 0 && duration > 0 && duration !== 0xffffffff) return Math.round((duration * 1000) / timescale)
    // a fragmented file keeps the length in mvex/mehd
    const mvex = find(buf, moov.start, moov.end, 'mvex')
    const mehd = mvex ? find(buf, mvex.start, mvex.end, 'mehd') : undefined
    if (mehd && timescale > 0) {
      const v = buf[mehd.start]
      const d = v === 1 ? (mehd.end - mehd.start >= 12 ? Number(buf.readBigUInt64BE(mehd.start + 4)) : 0) : mehd.end - mehd.start >= 8 ? buf.readUInt32BE(mehd.start + 4) : 0
      if (d > 0) return Math.round((d * 1000) / timescale)
    }
  }
  return undefined
}

/** The first video track's sample entry code (`avc1`, `hvc1`, `vp09`, …). */
function codecOf(buf: Buffer, moov: Box): string | undefined {
  for (const trak of boxes(buf, moov.start, moov.end)) {
    if (trak.type !== 'trak') continue
    const mdia = find(buf, trak.start, trak.end, 'mdia')
    if (!mdia) continue
    const hdlr = find(buf, mdia.start, mdia.end, 'hdlr')
    if (!hdlr || hdlr.end - hdlr.start < 12 || buf.toString('latin1', hdlr.start + 8, hdlr.start + 12) !== 'vide') continue
    const minf = find(buf, mdia.start, mdia.end, 'minf')
    const stbl = minf ? find(buf, minf.start, minf.end, 'stbl') : undefined
    const stsd = stbl ? find(buf, stbl.start, stbl.end, 'stsd') : undefined
    if (!stsd || stsd.end - stsd.start < 16) return undefined
    // version and flags, entry count, then the first entry: its size and its code
    return buf.toString('latin1', stsd.start + 12, stsd.start + 16).toLowerCase()
  }
  return undefined
}

/** A look at the bytes of a file: whether it is an MP4 or MOV, its video codec and its length. */
export function probeOwnClip(buf: Buffer): OwnClipProbe {
  const probe: OwnClipProbe = { container: false, bytes: buf.length }
  if (buf.length < 8 || !FIRST_BOXES.has(buf.toString('latin1', 4, 8))) return probe
  probe.container = true
  const moov = find(buf, 0, buf.length, 'moov')
  if (!moov) return probe
  const durationMs = lengthOf(buf, moov)
  if (durationMs !== undefined) probe.durationMs = durationMs
  const codec = codecOf(buf, moov)
  if (codec) probe.codec = codec
  return probe
}

export interface OwnClipDeps {
  /** `$DSH_HOME/nanomuse/avatar`. */
  dir: string
  /** Called whenever the clip changes. */
  onChange(): void
}

export class OwnClip {
  private current: OwnClipView | null = null
  private perMood: Partial<Record<OwnClipMood, OwnClipView>> = {}

  constructor(private readonly deps: OwnClipDeps) {}

  get clipFile(): string {
    return join(this.deps.dir, 'own-clip.mp4')
  }

  get posterFile(): string {
    return join(this.deps.dir, 'own-clip.png')
  }

  get pendingFile(): string {
    return join(this.deps.dir, 'own-clip.pending.mp4')
  }

  private moodFile(mood: OwnClipMood, extension: 'mp4' | 'png'): string {
    return join(this.deps.dir, `own-clip-${mood}.${extension}`)
  }

  /** A copy that was never used or cancelled (the app quit in between) is of no use now. */
  async init(): Promise<void> {
    await rm(this.pendingFile, { force: true }).catch(() => undefined)
    await this.rescan()
  }

  view(): OwnClipView | null {
    return this.current
  }

  moods(): Partial<Record<OwnClipMood, OwnClipView>> {
    return this.perMood
  }

  /** The file behind `/nanomuse/avatar/<name>`: the clip, its first frame, or the pending copy; else nothing. */
  path(name: string): string | undefined {
    if (name === 'own-clip.mp4') return this.current ? this.clipFile : undefined
    if (name === 'own-clip.png') return this.current?.poster ? this.posterFile : undefined
    if (name === 'own-clip.pending.mp4') return this.pendingFile
    const match = /^own-clip-(idle|working|waiting|happy|error)\.(mp4|png)$/.exec(name)
    if (match && isOwnClipMood(match[1])) {
      const mood = match[1]
      const clip = this.perMood[mood]
      if (clip && (match[2] === 'mp4' || clip.poster)) return this.moodFile(mood, match[2] as 'mp4' | 'png')
    }
    return undefined
  }

  async rescan(): Promise<void> {
    let next: OwnClipView | null = null
    try {
      const s = await stat(this.clipFile)
      if (s.size > 0) {
        let poster = false
        try {
          poster = (await stat(this.posterFile)).size > 0
        } catch {
          // no first frame kept
        }
        next = { size: s.size, v: Math.floor(s.mtimeMs), poster }
      }
    } catch {
      // no clip
    }
    const moods: Partial<Record<OwnClipMood, OwnClipView>> = {}
    for (const mood of OWN_CLIP_MOODS) {
      try {
        const s = await stat(this.moodFile(mood, 'mp4'))
        if (s.size > 0) {
          const poster = await stat(this.moodFile(mood, 'png')).then((p) => p.size > 0).catch(() => false)
          moods[mood] = { size: s.size, v: Math.floor(s.mtimeMs), poster }
        }
      } catch {
        // no override for this mood
      }
    }
    const changed = JSON.stringify(next) !== JSON.stringify(this.current) || JSON.stringify(moods) !== JSON.stringify(this.perMood)
    this.current = next
    this.perMood = moods
    if (changed) this.deps.onChange()
  }

  /**
   * A picked file arrives as the request body: it is written to the pending copy while
   * it stays under the size cap, then looked at; a clip the rule refuses is removed at
   * once and the refusal returned, one it takes waits for "Use" or "Cancel".
   */
  async stage(req: IncomingMessage): Promise<OwnClipStaged> {
    const declared = Number(req.headers['content-length'] ?? 0) || 0
    if (declared > OWN_CLIP_MAX_BYTES) {
      req.resume()
      return { ok: false, why: 'too_large', probe: { container: true, bytes: declared } }
    }
    await mkdir(this.deps.dir, { recursive: true })
    const file = this.pendingFile
    await rm(file, { force: true }).catch(() => undefined)
    let size = 0
    // Past the cap the rest is read and dropped (so the refusal still reaches the browser),
    // up to twice the cap; a body longer than that is cut off.
    const cap = async function* (source: AsyncIterable<Buffer>) {
      for await (const chunk of source) {
        size += chunk.length
        if (size > OWN_CLIP_MAX_BYTES) {
          if (size > 2 * OWN_CLIP_MAX_BYTES) return
          continue
        }
        yield chunk
      }
    }
    try {
      await pipeline(req, cap, createWriteStream(file))
    } catch (error: unknown) {
      await rm(file, { force: true }).catch(() => undefined)
      throw error
    }
    if (size > OWN_CLIP_MAX_BYTES) {
      await rm(file, { force: true }).catch(() => undefined)
      return { ok: false, why: 'too_large', probe: { container: true, bytes: Math.max(size, declared) } }
    }
    const probe = probeOwnClip(await readFile(file))
    const why = checkOwnClip(probe)
    if (why) {
      await rm(file, { force: true }).catch(() => undefined)
      return { ok: false, why, probe }
    }
    const s = await stat(file)
    return { ok: true, pending: { v: Math.floor(s.mtimeMs), size: s.size, durationMs: probe.durationMs ?? 0 } }
  }

  /** "Use": the pending copy becomes the clip; `poster`, when the browser took one, its first frame. */
  async use(poster?: Buffer, mood?: OwnClipMood): Promise<boolean> {
    try {
      await stat(this.pendingFile)
    } catch {
      return false
    }
    const file = mood ? this.moodFile(mood, 'mp4') : this.clipFile
    const posterFile = mood ? this.moodFile(mood, 'png') : this.posterFile
    await rm(file, { force: true }).catch(() => undefined)
    await rename(this.pendingFile, file)
    if (poster && poster.length) await writeFile(posterFile, poster)
    else await rm(posterFile, { force: true }).catch(() => undefined)
    await this.rescan()
    return true
  }

  /** "Cancel" in the preview: the copy goes. */
  async discard(): Promise<void> {
    await rm(this.pendingFile, { force: true }).catch(() => undefined)
  }

  /** "Default face": back to the drawn face, or the dragon. */
  async clear(mood?: OwnClipMood): Promise<void> {
    if (mood) {
      await rm(this.moodFile(mood, 'mp4'), { force: true }).catch(() => undefined)
      await rm(this.moodFile(mood, 'png'), { force: true }).catch(() => undefined)
    } else {
      await rm(this.clipFile, { force: true }).catch(() => undefined)
      await rm(this.posterFile, { force: true }).catch(() => undefined)
      for (const state of OWN_CLIP_MOODS) {
        await rm(this.moodFile(state, 'mp4'), { force: true }).catch(() => undefined)
        await rm(this.moodFile(state, 'png'), { force: true }).catch(() => undefined)
      }
    }
    await rm(this.pendingFile, { force: true }).catch(() => undefined)
    await this.rescan()
  }
}
