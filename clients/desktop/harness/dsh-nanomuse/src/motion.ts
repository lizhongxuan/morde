/**
 * The face in motion: one short looping clip per mood, drawn from that mood's still by the
 * video model — Muse's fixed set: a head shake and a sway at rest, headphones and a laptop
 * while working, the crystal ball while waiting, the five-pointed star when pleased. The
 * phone's `avatar/AvatarMotion.kt`, ported: the same four moods in the same order, the same
 * four seconds, the same prompts, word for word.
 *
 * Clips live next to the account's pictures, under `$DSH_HOME/nanomuse/avatar/motion/
 * <mood>.mp4`. The browser plays whichever exists for the current mood from
 * `/nanomuse/avatar/motion/<mood>.mp4?v=<mtime>` and falls back to the still. A new face,
 * drawn here or arriving from the account, throws the old clips away and, when the person
 * has not switched the animation off and a video model is set, draws them again, one after
 * another (each takes a few minutes and costs per second). Since relay 0.29 the clips travel
 * with the face: a clip drawn here is handed to `share` (the service puts it up and names it
 * in the account's profile), and clips another device made are taken in by `adopt` instead
 * of being drawn again. The built-in dragon has its clips in `assets/` and never comes
 * through here.
 */
import { mkdir, readdir, readFile, rename, rm, stat, writeFile } from 'node:fs/promises'
import { createReadStream } from 'node:fs'
import type { IncomingMessage, ServerResponse } from 'node:http'
import { join } from 'node:path'
import { imageToVideo, type Frame, type VideoEndpoint, type VideoProgress } from './video.ts'

export type MotionMood = 'idle' | 'working' | 'waiting' | 'happy'

/** The moods that get a clip, in the order they are drawn: the one seen most first. */
export const ANIMATED: readonly MotionMood[] = ['idle', 'working', 'waiting', 'happy']
export const SECONDS = 4
/** Where the browser asks for a clip. */
export const MOTION_PREFIX = '/nanomuse/avatar'

/** Muse's fixed motions, one per mood, on top of the pose picture (the phone's `motionPrompt`, verbatim). */
export function motionPrompt(mood: MotionMood | 'error'): string {
  let action: string
  switch (mood) {
    case 'idle':
      action = 'The character stays in place and gently shakes its head left and right while its round body sways very slightly, calm and friendly, like the idle animation of a mascot.'
      break
    case 'waiting':
      action = 'The character holds a small glowing crystal ball in its hands and plays with it, turning it slowly and peeking into it with curiosity.'
      break
    case 'happy':
      action = 'The character plays happily with a small golden five-pointed star, tossing it up a little and catching it, bouncing gently with joy.'
      break
    case 'working':
      action = 'The character wears headphones and types busily on the small laptop in front of it, nodding slightly to the rhythm, focused and content.'
      break
    case 'error':
      action = 'The character looks a little flustered, a sweat drop on its brow, then takes a breath and settles.'
      break
  }
  return (
    `${action} Plain white background, static camera, no zoom, no cuts, soft even studio lighting, the same 3D toy look as the picture throughout, ` +
    'nothing else appears in the frame, and the motion loops naturally with the character back in its starting pose at the end.'
  )
}

export interface MotionProgress {
  done: number
  total: number
  failed: MotionMood[]
  running: boolean
  current?: MotionMood
  stage?: VideoProgress
  error?: string
  /** Where the run draws: `nanomuse` for the account, else an own row's id (the failure line offers Cloud when it is an own row). */
  source?: string
}

export interface ClipInfo {
  size: number
  /** Modification time in ms, the browser's cache key. */
  v: number
}

/** What the browser sees of the clips (`live.motion`). */
export interface MotionView {
  progress: MotionProgress | null
  clips: Partial<Record<MotionMood, ClipInfo>>
  /** The face the clips belong to; empty for the dragon or no face. */
  faceId: string
}

export interface MotionDeps {
  /** `$DSH_HOME/nanomuse/avatar/motion`. */
  dir: string
  /** The video model to draw with, or nothing (no key, switched off, OpenRouter); `viaCloud` asks for the account whatever the slot says. */
  endpoint(viaCloud?: boolean): Promise<VideoEndpoint | undefined>
  /** Whether the person wants a new face animated (the Media setting; default on). */
  animate(): boolean
  /** The face worn now: its id (empty for the dragon or an emoji) and a still per mood. */
  faceId(): string
  still(mood: MotionMood): Promise<Frame | undefined>
  /** Called whenever the progress or the clips change. */
  onChange(): void
  log(level: 'info' | 'warn', text: string): void
  /** The clip maker; the tests pass a fake. */
  generate?: typeof imageToVideo
  /** A clip drawn here, for the account's other devices (relay 0.29); a failure is the service's to log. */
  share?(mood: MotionMood, bytes: Uint8Array): Promise<void>
}

const MARKER = 'motion.json'

export class AvatarMotion {
  private progress: MotionProgress | null = null
  private clips: Partial<Record<MotionMood, ClipInfo>> = {}
  /** The face the clips on disk were drawn for. */
  private clipsFace = ''
  private run: { abort: AbortController; promise: Promise<void> } | undefined

  constructor(private readonly deps: MotionDeps) {}

  clipFile(mood: MotionMood): string {
    return join(this.deps.dir, `${mood}.mp4`)
  }

  async init(): Promise<void> {
    try {
      const raw = JSON.parse(await readFile(join(this.deps.dir, MARKER), 'utf8')) as { faceId?: unknown }
      this.clipsFace = typeof raw.faceId === 'string' ? raw.faceId : ''
    } catch {
      this.clipsFace = ''
    }
    await this.rescan()
  }

  view(): MotionView {
    return { progress: this.progress, clips: { ...this.clips }, faceId: this.clipsFace }
  }

  /** The clip on disk for a mood, when the face it belongs to is the one worn. */
  clipPath(mood: string): string | undefined {
    if (!(ANIMATED as readonly string[]).includes(mood)) return undefined
    const m = mood as MotionMood
    if (!this.clips[m] || this.clipsFace !== this.deps.faceId()) return undefined
    return this.clipFile(m)
  }

  async rescan(): Promise<void> {
    const next: Partial<Record<MotionMood, ClipInfo>> = {}
    for (const mood of ANIMATED) {
      try {
        const s = await stat(this.clipFile(mood))
        if (s.size > 0) next[mood] = { size: s.size, v: Math.floor(s.mtimeMs) }
      } catch {
        // no clip for this mood
      }
    }
    this.clips = next
    this.deps.onChange()
  }

  /** A new face, or none: the old clips belonged to the old face. */
  async clear(): Promise<void> {
    this.cancel()
    this.progress = null
    try {
      for (const name of await readdir(this.deps.dir)) await rm(join(this.deps.dir, name), { force: true })
    } catch {
      // nothing there
    }
    this.clipsFace = ''
    await this.rescan()
  }

  /** True when a video model is set and the person has not turned the animation off. */
  async enabled(): Promise<boolean> {
    return this.deps.animate() && (await this.deps.endpoint()) !== undefined
  }

  /**
   * The face changed (drawn here, or pulled from the account): clips of another face go,
   * and the new face is animated when the setting and a video model allow it. Returns
   * true when drawing started.
   */
  async faceChanged(): Promise<boolean> {
    const face = this.deps.faceId()
    if (face === this.clipsFace) return false
    await this.clear()
    if (!face) return false
    if (!(await this.enabled())) return false
    return this.animateAll()
  }

  /**
   * Draws the missing clips (all of them with `force`). No-op without a face or a video
   * model. Returns false when nothing was started. Clips are drawn one after another; a
   * failure marks that mood and the rest carry on — a partial set is fine, the still fills in.
   * `viaCloud` draws this run with the account (*Use nanoMuse Cloud this time*), the slot untouched.
   */
  async animateAll(force = false, viaCloud = false): Promise<boolean> {
    const ep = await this.deps.endpoint(viaCloud)
    if (!ep) return false
    const face = this.deps.faceId()
    if (!face) return false
    if (face !== this.clipsFace) {
      // clips of another face are never kept
      await this.clear()
    }
    const todo = ANIMATED.filter((mood) => force || !this.clips[mood])
    if (!todo.length) return false
    this.cancel()
    const abort = new AbortController()
    this.progress = { done: 0, total: todo.length, failed: [], running: true, source: ep.instanceId }
    this.deps.onChange()
    const generate = this.deps.generate ?? imageToVideo
    const promise = (async () => {
      const failed: MotionMood[] = []
      let lastError: string | undefined
      await mkdir(this.deps.dir, { recursive: true })
      this.clipsFace = face
      await writeFile(join(this.deps.dir, MARKER), JSON.stringify({ faceId: face, model: ep.model, at: Date.now() }, null, 2) + '\n').catch(() => undefined)
      for (const [i, mood] of todo.entries()) {
        if (abort.signal.aborted) break
        this.progress = { done: i, total: todo.length, failed: [...failed], running: true, current: mood, source: ep.instanceId }
        this.deps.onChange()
        const frame = (await this.deps.still(mood).catch(() => undefined)) ?? (await this.deps.still('idle').catch(() => undefined))
        if (!frame) {
          failed.push(mood)
          continue
        }
        try {
          const bytes = await generate(ep, frame, motionPrompt(mood), SECONDS, {
            signal: abort.signal,
            onProgress: (stage) => {
              this.progress = { done: i, total: todo.length, failed: [...failed], running: true, current: mood, stage, source: ep.instanceId }
              this.deps.onChange()
            },
          })
          if (abort.signal.aborted) break
          const file = this.clipFile(mood)
          const tmp = `${file}.tmp`
          await writeFile(tmp, bytes)
          await rename(tmp, file)
          await this.rescan()
          if (this.deps.share) void this.deps.share(mood, bytes).catch((error: unknown) => this.deps.log('warn', `avatar motion: clip ${mood} not shared with the account: ${error instanceof Error ? error.message : String(error)}`))
        } catch (error: unknown) {
          if (abort.signal.aborted) break
          const text = error instanceof Error ? error.message : String(error)
          this.deps.log('warn', `avatar motion: clip ${mood} failed: ${text}`)
          failed.push(mood)
          lastError = text
        }
      }
      if (!abort.signal.aborted) {
        this.progress = { done: todo.length, total: todo.length, failed, running: false, source: ep.instanceId, ...(lastError ? { error: lastError } : {}) }
        this.deps.onChange()
      }
    })().finally(() => {
      if (this.run?.abort === abort) this.run = undefined
    })
    this.run = { abort, promise }
    return true
  }

  /**
   * Clips another device made of the worn face (the account's profile names them): the moods
   * this computer has no clip for are fetched and kept as if drawn here, so nothing is drawn
   * twice. Nothing happens while a run is drawing, for a face other than the worn one, or
   * for a mood that already has a clip. Returns how many clips were taken in.
   */
  async adopt(face: string, clips: Partial<Record<MotionMood, () => Promise<Uint8Array | undefined>>>): Promise<number> {
    if (!face || face !== this.deps.faceId() || this.run) return 0
    if (face !== this.clipsFace) await this.clear()
    let taken = 0
    for (const mood of ANIMATED) {
      const fetch = clips[mood]
      if (!fetch || this.clips[mood]) continue
      try {
        const bytes = await fetch()
        if (!bytes?.length) continue
        if (this.run || this.deps.faceId() !== face) break
        await mkdir(this.deps.dir, { recursive: true })
        if (!taken && this.clipsFace !== face) {
          this.clipsFace = face
          await writeFile(join(this.deps.dir, MARKER), JSON.stringify({ faceId: face, from: 'account', at: Date.now() }, null, 2) + '\n').catch(() => undefined)
        }
        const file = this.clipFile(mood)
        const tmp = `${file}.tmp`
        await writeFile(tmp, bytes)
        await rename(tmp, file)
        taken += 1
      } catch (error: unknown) {
        this.deps.log('warn', `avatar motion: clip ${mood} from the account not kept: ${error instanceof Error ? error.message : String(error)}`)
      }
    }
    if (taken) await this.rescan()
    return taken
  }

  /** Stop drawing after the current clip; what is on disk stays. */
  cancel(): void {
    const run = this.run
    if (!run) return
    run.abort.abort()
    this.run = undefined
    if (this.progress) {
      this.progress = { ...this.progress, running: false }
      this.deps.onChange()
    }
  }

  /** Forget a finished run's progress line. */
  clearProgress(): void {
    if (this.progress && !this.progress.running) {
      this.progress = null
      this.deps.onChange()
    }
  }

  /** For the tests: wait until the current run is over. */
  async settled(): Promise<void> {
    await this.run?.promise
  }
}

const CLIP = /^\/motion\/([a-z]+)\.mp4$/
/** The person's own clip (#324), its first frame, and the copy waiting in the preview (`own-clip.ts`). */
const OWN = /^\/(own-clip\.mp4|own-clip\.png|own-clip\.pending\.mp4|own-clip-(?:idle|working|waiting|happy|error)\.(?:mp4|png))$/

/**
 * `GET /nanomuse/avatar/motion/<mood>.mp4`: the clip of the worn face, or 404. With `ownPath`,
 * `GET /nanomuse/avatar/own-clip.mp4` (and `.png`, `.pending.mp4`) the same way for the person's own clip.
 */
export async function serveClip(req: IncomingMessage, res: ServerResponse, clipPath: (mood: string) => string | undefined, ownPath?: (name: string) => string | undefined): Promise<void> {
  const url = new URL(req.url ?? '/', 'http://127.0.0.1')
  const rel = url.pathname.slice(MOTION_PREFIX.length)
  const m = CLIP.exec(rel)
  const own = ownPath ? OWN.exec(rel) : null
  if ((req.method !== 'GET' && req.method !== 'HEAD') || (!m && !own)) {
    res.writeHead(404, { 'cache-control': 'no-store' }).end()
    return
  }
  const path = own ? ownPath?.(own[1] ?? '') : clipPath(m?.[1] ?? '')
  if (!path) {
    res.writeHead(404, { 'cache-control': 'no-store' }).end()
    return
  }
  let size: number
  try {
    size = (await stat(path)).size
  } catch {
    res.writeHead(404, { 'cache-control': 'no-store' }).end()
    return
  }
  // the URL carries the mtime, so a clip may be cached for good
  const type = own && own[1]?.endsWith('.png') ? 'image/png' : 'video/mp4'
  res.writeHead(200, { 'content-type': type, 'content-length': size, 'cache-control': url.searchParams.has('v') ? 'public, max-age=31536000, immutable' : 'no-cache' })
  if (req.method === 'HEAD') {
    res.end()
    return
  }
  createReadStream(path).pipe(res)
}
