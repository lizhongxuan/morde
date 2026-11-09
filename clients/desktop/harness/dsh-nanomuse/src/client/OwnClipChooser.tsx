/**
 * Picking a clip of one's own as the face (#324): the file dialog, the host's look at the
 * file (`own-clip.ts`: MP4 or MOV with H.264 or HEVC video, at most 15 seconds and 30 MB),
 * the refusal in one sentence, and the preview sheet with the clip playing in the face's own
 * round frame, "Use" and "Cancel". The preview is also the last check: a clip this computer
 * cannot decode (HEVC on a system without the decoder, mostly Linux) never plays there, the
 * video element says so, and the clip is discarded with the refusal in place of the sheet;
 * "Use" waits until the first frame can play. The agent's page and the avatar studio call
 * `choose()` from a button or a menu and render `element` once. "Use" also hands the host
 * the clip's first frame, drawn from the preview, as the still for when there is no drawn face.
 */
import { createElement as h, Fragment, useEffect, useRef, useState, type ReactNode } from 'react'
import { call, failureText, type Translate } from './api.ts'
import { Sheet } from './ui.tsx'
import type { Mood } from './Avatar.tsx'

const API = 'nanomuse/cloud'
const MAX_SECONDS = 15
const MAX_BYTES = 30 * 1024 * 1024
/** The first frame kept as the still: square, this many pixels on a side. */
const POSTER_PX = 256

interface Pending {
  v: number
  size: number
  durationMs: number
}

/** `12.3 MB`, in the UI language. */
export function sizeText(bytes: number, lang: string): string {
  const mb = bytes / (1024 * 1024)
  const n = mb >= 10 ? Math.round(mb) : Math.round(mb * 10) / 10
  return `${n.toLocaleString(lang || undefined)} MB`
}

/** The one sentence a refused clip gets, with the limits in it. */
export function refusalText(t: Translate, code: string, seconds: number, bytes: number): string {
  if (code === 'clip_too_long') return t('ocRefusedLong', { seconds })
  if (code === 'clip_too_large') return t('ocRefusedLarge', { size: sizeText(bytes, t('langTag')) })
  return t('ocRefusedFormat')
}

/** The clip's first frame as a square PNG, base64, from the playing preview; empty when the frame cannot be read yet. */
function posterOf(video: HTMLVideoElement | null): string {
  if (!video || video.readyState < 2 || !video.videoWidth || !video.videoHeight) return ''
  try {
    const side = Math.min(video.videoWidth, video.videoHeight)
    const canvas = document.createElement('canvas')
    canvas.width = POSTER_PX
    canvas.height = POSTER_PX
    const ctx = canvas.getContext('2d')
    if (!ctx) return ''
    ctx.drawImage(video, (video.videoWidth - side) / 2, (video.videoHeight - side) / 2, side, side, 0, 0, POSTER_PX, POSTER_PX)
    const url = canvas.toDataURL('image/png')
    return url.slice(url.indexOf(',') + 1)
  } catch {
    return ''
  }
}

export interface OwnClipChooser {
  /** Open the file dialog. */
  choose(mood?: Mood): void
  /** True from the pick until the host has looked at the file. */
  busy: boolean
  /** The preview sheet and the refusal, to render once on the screen. */
  element: ReactNode
}

export function useOwnClipChooser(t: Translate): OwnClipChooser {
  const [busy, setBusy] = useState(false)
  const [pending, setPending] = useState<Pending | null>(null)
  const [message, setMessage] = useState<string | null>(null)
  const [pendingMood, setPendingMood] = useState<Mood | undefined>()
  const alive = useRef(true)
  useEffect(() => () => { alive.current = false }, [])

  const stage = async (file: File) => {
    // the size is known before anything is sent; the rest the host reads from the file
    if (file.size > MAX_BYTES) {
      setMessage(refusalText(t, 'clip_too_large', 0, file.size))
      return
    }
    setBusy(true)
    try {
      const type = file.type || (/\.mov$/i.test(file.name) ? 'video/quicktime' : 'video/mp4')
      const res = await fetch(`${API}/own-clip/stage`, { method: 'POST', headers: { 'content-type': type, 'x-nanomuse': '1' }, body: file })
      const json = (await res.json().catch(() => ({}))) as { pending?: Pending; error?: { code?: string; message?: string }; seconds?: number; bytes?: number }
      if (!alive.current) return
      if (res.ok && json.pending) {
        setPending(json.pending)
      } else if (json.error?.code?.startsWith('clip_')) {
        setMessage(refusalText(t, json.error.code, json.seconds ?? 0, json.bytes ?? file.size))
      } else {
        setMessage(failureText(t, Object.assign(new Error(json.error?.message ?? `${res.status}`), { code: json.error?.code ?? `${res.status}` })))
      }
    } catch (err: unknown) {
      if (alive.current) setMessage(failureText(t, err))
    } finally {
      if (alive.current) setBusy(false)
    }
  }

  const choose = (mood?: Mood) => {
    if (busy) return
    const input = document.createElement('input')
    input.type = 'file'
    input.accept = 'video/mp4,video/quicktime,.mp4,.mov'
    input.onchange = () => {
      const file = input.files?.[0]
      if (file) { setPendingMood(mood); void stage(file) }
    }
    input.click()
  }

  const element = h(Fragment, null,
    pending ? h(OwnClipPreview, { t, pending, mood: pendingMood, onDone: () => setPending(null), onUnplayable: () => { setPending(null); setMessage(t('ocRefusedPlay')) } }) : null,
    message
      ? h(Sheet, { title: t('ocTitle'), closeLabel: t('close'), onClose: () => setMessage(null), footer: h('div', { className: 'nm-sheet-actions' }, h('span', { style: { flex: 1 } }), h('button', { type: 'button', className: 'nm-pill nm-pill-sm', onClick: () => setMessage(null) }, t('ocOk'))) },
          h('p', { className: 'nm-sheet-lead', role: 'alert' }, message))
      : null)

  return { choose, busy, element }
}

/** The preview: the clip playing as the face will play it, its length and size, "Use" and "Cancel", and that it stays here. */
function OwnClipPreview({ t, pending, mood, onDone, onUnplayable }: { t: Translate; pending: Pending; mood?: Mood | undefined; onDone(): void; onUnplayable(): void }): ReactNode {
  const video = useRef<HTMLVideoElement | null>(null)
  const [busy, setBusy] = useState(false)
  const [ready, setReady] = useState(false)
  const [error, setError] = useState<string | undefined>()
  const decided = useRef(false)
  const cancel = () => {
    if (decided.current) return
    decided.current = true
    void call('own-clip/discard', {}).catch(() => undefined)
    onDone()
  }
  // the browser has no decoder for the clip's video (HEVC on most Linux builds): the same
  // refusal a wrong format gets, and the pending copy goes
  const unplayable = () => {
    if (decided.current) return
    decided.current = true
    void call('own-clip/discard', {}).catch(() => undefined)
    onUnplayable()
  }
  const use = async () => {
    if (decided.current) return
    setBusy(true)
    setError(undefined)
    try {
      await call('own-clip/use', { poster: posterOf(video.current), mood })
      decided.current = true
      onDone()
    } catch (err: unknown) {
      setError(failureText(t, err))
      setBusy(false)
    }
  }
  // closed any other way (Escape, the ×): the same as "Cancel"
  useEffect(() => () => { if (!decided.current) void call('own-clip/discard', {}).catch(() => undefined) }, [])
  const seconds = Math.round(pending.durationMs / 1000)
  const footer = h('div', { className: 'nm-sheet-actions' },
    h('span', { style: { flex: 1 } }),
    h('button', { type: 'button', className: 'nm-pill nm-pill-ghost nm-pill-sm', disabled: busy, onClick: cancel }, t('cancel')),
    h('button', { type: 'button', className: 'nm-pill nm-pill-sm', disabled: busy || !ready, onClick: () => void use() }, t('ocUse')))
  return h(Sheet, { title: t('ocPreviewTitle'), closeLabel: t('close'), onClose: cancel, footer },
    h('div', { className: 'nm-sheet-form' },
      h('p', { className: 'nm-sheet-lead' }, t('ocPreviewLead')),
      h('span', { className: 'nm-avatar nm-oc-disc', role: 'img', 'aria-label': t('ocPreviewLabel') },
        h('video', { ref: video, src: `nanomuse/avatar/own-clip.pending.mp4?v=${pending.v}`, autoPlay: true, loop: true, muted: true, playsInline: true, disablePictureInPicture: true, 'aria-hidden': true, onCanPlay: () => setReady(true), onError: unplayable })),
      h('p', { className: 'nm-fine nm-oc-details' }, t('ocDetails', { seconds: Math.min(seconds, MAX_SECONDS), size: sizeText(pending.size, t('langTag')) })),
      error ? h('div', { className: 'nm-room-error' }, error) : null,
      h('p', { className: 'nm-fine' }, t('ocLocal'))))
}
