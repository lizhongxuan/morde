/**
 * The agent's face where the harness shows a brand: the sidebar mark, the
 * sidebar name, the hero mark above a blank session, the capsule, the header.
 * The face is the account's — the dragon by default, an emoji on a colour, or
 * one drawn in the avatar studio on the phone whose stills the host pulled —
 * and its mood follows what the agent is doing, the way the Android app's
 * `AgentAvatarDisc` and the web app's `Avatar` do: idle, working, waiting (for
 * the person), happy, error.
 *
 * The dragon moves: a short looping clip per mood — a head shake at rest,
 * headphones and a laptop while working, a crystal ball while waiting, a star
 * when pleased — the same clips the phone and the web app play. A drawn face
 * moves too once this computer has drawn its clips (`motion.ts`, Settings →
 * Media): the same four moods from `/nanomuse/avatar/motion/<mood>.mp4`, with
 * the file's mtime as the cache key, so a redrawn clip is not the old one from
 * the cache. The still shows where a clip is not worth it or not wanted: small
 * sizes, lists and pickers, a person who prefers reduced motion, a clip that
 * failed to load or was not drawn, the error mood. Without a clip the face and
 * the emoji move as a whole instead: breathe when idle, sway while working, hop
 * while waiting. A clip pauses while the window is hidden.
 *
 * A clip of the person's own (`own-clip.ts`, #324) plays instead, for every mood, when one
 * is set on this computer; the stills keep their place, and with no drawn face the clip's
 * first frame is the still.
 */
import { createElement as h, useEffect, useRef, useState, type CSSProperties, type ReactElement } from 'react'
import { useLive, type Live, type LiveMotion, type LiveOwnClip, type LiveProfile } from './live.ts'
import type { Translate } from './api.ts'

export type Mood = 'idle' | 'working' | 'waiting' | 'happy' | 'error'

/** Below this the clip is not worth the bytes: the still shows. */
const CLIP_MIN_PX = 44
/** Clips that did not load: the still, and no second request. */
const noClip = new Set<string>()
const reducedMotion = typeof window !== 'undefined' && typeof window.matchMedia === 'function' && window.matchMedia('(prefers-reduced-motion: reduce)').matches

/**
 * Document-relative, so it resolves under whatever mount served the page. With `ownClip`
 * and no drawn face, the own clip's first frame stands in for the dragon.
 */
export function stillUrl(profile: LiveProfile | undefined, mood: Mood = 'idle', ownClip?: LiveOwnClip | null, ownClips?: Partial<Record<Mood, LiveOwnClip>>): string {
  if (profile?.avatar === 'face' && profile.faceId) return `nanomuse/assets/face/${profile.faceId}/${mood}.webp`
  const override = ownClips?.[mood]
  if (override?.poster) return `nanomuse/avatar/own-clip-${mood}.png?v=${override.v}`
  if (ownClip?.poster) return `nanomuse/avatar/own-clip.png?v=${ownClip.v}`
  return `nanomuse/assets/dragon-${mood}.webp`
}

/** The person's own clip, when one is set: the same for every mood, keyed by its mtime. */
export function ownClipUrl(ownClip: LiveOwnClip | null | undefined, mood?: Mood, ownClips?: Partial<Record<Mood, LiveOwnClip>>): string | null {
  const override = mood ? ownClips?.[mood] : undefined
  if (override && mood) return `nanomuse/avatar/own-clip-${mood}.mp4?v=${override.v}`
  return ownClip ? `nanomuse/avatar/own-clip.mp4?v=${ownClip.v}` : null
}

/**
 * The clip for a mood: none for the error pose (a still says it better) or the emoji. A
 * drawn face has one only when this computer drew it (`motion`), and the URL carries the
 * clip's mtime so a redrawn clip is fetched anew.
 */
export function clipUrl(profile: LiveProfile | undefined, mood: Mood = 'idle', motion?: LiveMotion): string | null {
  if (mood === 'error' || profile?.avatar === 'emoji') return null
  if (profile?.avatar === 'face' && profile.faceId) {
    if (!motion || motion.faceId !== profile.faceId) return null
    const clip = motion.clips[mood]
    return clip ? `nanomuse/avatar/motion/${mood}.mp4?v=${clip.v}` : null
  }
  return `nanomuse/assets/dragon-${mood}.mp4`
}

/**
 * One line for the header while the face's clips are drawn — "Animating 2/4…" — or null.
 * With `t`, the locale's wording (`moAnimating`).
 */
export function motionStatus(live: Pick<Live, 'motion'>, t?: Translate): string | null {
  const p = live.motion.progress
  if (!p || !p.running) return null
  const n = Math.min(p.done + 1, p.total)
  return t ? t('moAnimating', { n, total: p.total }) : `Animating ${n}/${p.total}…`
}

/** Whether the document is visible; a hidden window pauses the clips. */
function useVisible(): boolean {
  const [visible, setVisible] = useState(() => typeof document === 'undefined' || document.visibilityState !== 'hidden')
  useEffect(() => {
    if (typeof document === 'undefined') return
    const on = () => setVisible(document.visibilityState !== 'hidden')
    document.addEventListener('visibilitychange', on)
    return () => document.removeEventListener('visibilitychange', on)
  }, [])
  return visible
}

export interface AvatarProps {
  size: number
  profile?: LiveProfile | undefined
  mood?: Mood
  className?: string | undefined
  title?: string
  /** No clip and no motion: lists, pickers, anything that should hold still. */
  still?: boolean
}

/** A round face of the agent at the requested size, moving when it may; an emoji face is drawn, not loaded. */
export function Avatar({ size, profile, mood = 'idle', className, title, still = false }: AvatarProps): ReactElement {
  const name = title ?? profile?.name ?? 'Morde'
  const [, bump] = useState(0)
  const live = useLive()
  const visible = useVisible()
  const video = useRef<HTMLVideoElement | null>(null)
  const candidate = still || reducedMotion || size < CLIP_MIN_PX ? null : ownClipUrl(live.ownClip, mood, live.ownClips) ?? clipUrl(profile, mood, live.motion)
  const clip = candidate && !noClip.has(candidate) ? candidate : null
  const poster = stillUrl(profile, mood, live.ownClip, live.ownClips)
  // the whole face moves when nothing inside it does
  const motion = still || clip || reducedMotion ? '' : mood === 'working' ? ' nm-avatar-working' : mood === 'waiting' ? ' nm-avatar-waiting' : mood === 'idle' ? ' nm-avatar-idle' : ''

  useEffect(() => {
    const el = video.current
    if (!el) return
    if (visible) void el.play().catch(() => undefined)
    else el.pause()
  }, [visible, clip])

  const box: CSSProperties = { width: size, height: size }
  const emoji = profile?.avatar === 'emoji'
  // the own clip plays over any look; without it an emoji is drawn, not loaded
  const inner = clip
    ? h('video', {
        key: clip,
        ref: video,
        src: clip,
        poster,
        autoPlay: visible,
        loop: true,
        muted: true,
        playsInline: true,
        disablePictureInPicture: true,
        'aria-hidden': true,
        onError: () => {
          noClip.add(clip)
          bump((n) => n + 1)
        },
      })
    : emoji
      ? h('span', { className: 'nm-avatar-emoji', style: { background: profile.color || '#0064d4', fontSize: Math.round(size * 0.58), lineHeight: `${size}px` } }, profile.emoji || '✨')
      : h('img', { src: poster, alt: '', width: size, height: size, draggable: false })

  return h(
    'span',
    {
      role: 'img',
      'aria-label': name,
      title,
      className: `nm-avatar${motion}${className ? ` ${className}` : ''}`,
      style: box,
    },
    inner,
  )
}

/** The wordmark beside the mark; plain text in the sidebar's own type. */
export function BrandName({ text }: { text: string }): ReactElement {
  return h('span', { style: { fontWeight: 600, letterSpacing: '-0.01em' } }, text)
}
