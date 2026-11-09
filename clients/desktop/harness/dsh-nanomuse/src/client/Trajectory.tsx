/**
 * The trajectory of a hands run, in the chat (0.1.40): a card under the run's last step
 * row with the frame the model saw at each step, the action drawn on it — the ring where
 * it clicked, the arrow of a drag, the direction of a scroll, a chip with what it typed or
 * pressed — the caption and what the model said before acting; a filmstrip of the steps,
 * ← → and Prev/Next to walk them, Enlarge for a sheet, Copy for the step's words. While
 * the run is on the card follows the newest step and carries *I'll take it* and *Stop*.
 * This is what UI-TARS-desktop's history gives, instead of a picture-in-picture while
 * the hands work.
 *
 * The runs come from the host (`GET nanomuse/cloud/trajectory?session=`), re-read when
 * the live state's `trajectory.rev` moves; the pictures are `stage/frame?seq=`, loaded
 * lazily. Like `RemoteBubbles.ts`, the card is placed by a MutationObserver sweep —
 * after the tool row of the run's last call (`9:tool-call<id>`), else at the end of the
 * thread — and moved as the run goes on; nothing of React's is touched.
 */
import { createElement as h, Fragment, useEffect, useRef, useState, type ReactNode } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { call, type Translate } from './api.ts'
import { IconChevronLeft, IconChevronRight, IconCopy, IconExpand, IconHand, IconSquare } from './icons.tsx'
import { subscribeLive, useLive, type LiveStageAction } from './live.ts'
import { describeStep, frameUrl, keyWords, pointed } from './steps.ts'
import { Sheet } from './ui.tsx'

export interface TrajectoryStep {
  i: number
  seq: number
  at: number
  width: number
  height: number
  title: string
  action: LiveStageAction | null
  words: string
  callId: string
}

export interface TrajectoryRun {
  id: string
  sessionId: string
  source: 'computer' | 'device'
  device: string
  startedAt: number
  endedAt: number
  firstCall: string
  lastCall: string
  dropped: number
  steps: TrajectoryStep[]
}

/** Mutations within this window become one sweep. */
const SWEEP_DELAY_MS = 100
/** The picture in the card is at most this tall; the sheet shows it large. */
const CARD_PICTURE_HEIGHT = 320

/** The chat's node key of a tool row (`${kind.length}:${kind}${id}`). */
export function toolRowKey(callId: string): string {
  return `9:tool-call${callId}`
}

/** The words of a step, for Copy: the caption, the window, what the model said. */
export function stepText(t: Translate, step: TrajectoryStep): string {
  const lines = [`${t('trajStep', { i: step.i })} · ${describeStep(t, step.action, false)}${step.title ? ` · ${step.title}` : ''}`]
  if (step.action?.kind === 'type' && step.action.text) lines.push(step.action.text)
  if (step.words) lines.push(step.words)
  return lines.join('\n')
}

/** The chip's words for an action without a point, or beside the ring: typed text, the keys, the label. */
export function chipText(t: Translate, action: LiveStageAction): string {
  switch (action.kind) {
    case 'type':
      return action.text ? `“${action.text}”` : t('stageTyped', { text: '' })
    case 'key':
      return keyWords(action.text)
    case 'open_app':
      return action.text
    case 'wait':
      return t('stageWaiting')
    case 'hold':
    case 'hand_over':
      return action.text ? `${t('stageYourTurn')} · ${action.text}` : t('stageYourTurn')
    default:
      return action.label
  }
}

/** The step to show first: the newest with a frame, so a run in progress follows along. */
function newest(run: TrajectoryRun): number {
  return Math.max(0, run.steps.length - 1)
}

/** The picture of a step with the action drawn on it; `big` in the sheet. */
function StepPicture({ t, step, big }: { t: Translate; step: TrajectoryStep; big: boolean }): ReactNode {
  const w = step.width > 0 ? step.width : 1600
  const hgt = step.height > 0 ? step.height : 1000
  const ratio = `${w} / ${hgt}`
  /** The action with a point on this frame, else null. */
  const action = step.action && pointed(step.action, w, hgt) ? step.action : null
  const placeless = step.action && !action && step.action.kind !== 'look' && step.action.kind !== 'move' ? step.action : null
  const r = Math.max(10, w / 45)
  const stroke = Math.max(2, w / 400)
  const chip = placeless ? chipText(t, placeless) : action?.label ?? ''
  const chipAt = action ? { left: `${(action.x / w) * 100}%`, top: `${(action.y / hgt) * 100}%` } : undefined
  const left = action ? action.x / w > 0.72 : false
  const drag = action ? action.kind === 'drag' && (action.x2 ?? -1) >= 0 && (action.y2 ?? -1) >= 0 : false
  const scroll = action?.kind === 'scroll'
  const up = (action?.dy ?? 0) < 0
  const style: Record<string, string | number> = { aspectRatio: ratio }
  if (!big) style.width = `min(100%, ${Math.round((CARD_PICTURE_HEIGHT * w) / hgt)}px)`
  return h('div', { className: `nm-traj-picture${big ? ' nm-big' : ''}${step.seq ? '' : ' nm-gone'}`, style },
    step.seq
      ? h('img', { src: frameUrl(step.seq), alt: step.title, loading: 'lazy', decoding: 'async', draggable: false })
      : h('div', { className: 'nm-traj-nopic' }, t('trajNoPicture')),
    action
      ? h('svg', { className: 'nm-traj-marks', viewBox: `0 0 ${w} ${hgt}`, preserveAspectRatio: 'none', 'aria-hidden': true },
          drag
            ? h(Fragment, null,
                h('defs', null, h('linearGradient', { id: `nm-traj-g-${step.seq}-${step.i}`, gradientUnits: 'userSpaceOnUse', x1: action.x, y1: action.y, x2: action.x2, y2: action.y2 },
                  h('stop', { offset: '0', stopColor: 'var(--nm-traj-accent)' }), h('stop', { offset: '1', stopColor: 'var(--nm-traj-cyan)' }))),
                h('line', { x1: action.x, y1: action.y, x2: action.x2, y2: action.y2, stroke: `url(#nm-traj-g-${step.seq}-${step.i})`, strokeWidth: stroke, strokeDasharray: `${stroke * 3} ${stroke * 2}`, strokeLinecap: 'round' }),
                h('circle', { cx: action.x2, cy: action.y2, r: r * 0.8, fill: 'none', stroke: 'var(--nm-traj-cyan)', strokeWidth: stroke }),
                h('polygon', { points: arrowHead(action.x, action.y, action.x2 ?? 0, action.y2 ?? 0, r * 0.9), fill: 'var(--nm-traj-cyan)' }))
            : null,
          h('circle', { cx: action.x, cy: action.y, r: r * 2.2, fill: 'var(--nm-traj-accent)', opacity: 0.16 }),
          h('circle', { cx: action.x, cy: action.y, r, fill: 'none', stroke: 'var(--nm-traj-accent)', strokeWidth: stroke }),
          action.kind === 'double_click' ? h('circle', { cx: action.x, cy: action.y, r: r * 1.45, fill: 'none', stroke: 'var(--nm-traj-accent)', strokeWidth: stroke * 0.7, opacity: 0.7 }) : null,
          h('circle', { cx: action.x, cy: action.y, r: stroke * 1.4, fill: 'var(--nm-traj-accent)' }),
          h('circle', { cx: action.x, cy: action.y, r: stroke * 0.55, fill: '#fff' }),
          scroll
            ? h('path', { d: chevron(action.x, action.y + (up ? -r * 2.6 : r * 2.6), r * 0.9, up), fill: 'none', stroke: 'var(--nm-traj-cyan)', strokeWidth: stroke, strokeLinecap: 'round', strokeLinejoin: 'round' })
            : null)
      : null,
    chip ? h('span', { className: `nm-traj-chip${chipAt ? ' nm-aimed' : ''}${left ? ' nm-left' : ''}`, style: chipAt }, chip) : null)
}

/** The three points of an arrowhead at (x2, y2), pointing away from (x1, y1). */
export function arrowHead(x1: number, y1: number, x2: number, y2: number, size: number): string {
  const a = Math.atan2(y2 - y1, x2 - x1)
  const spread = 0.45
  const p = (ang: number) => `${(x2 - size * Math.cos(ang)).toFixed(1)},${(y2 - size * Math.sin(ang)).toFixed(1)}`
  return `${x2.toFixed(1)},${y2.toFixed(1)} ${p(a - spread)} ${p(a + spread)}`
}

/** A chevron centred on (x, y), opening up or down. */
export function chevron(x: number, y: number, size: number, up: boolean): string {
  const dy = up ? size : -size
  return `M ${(x - size).toFixed(1)} ${(y + dy).toFixed(1)} L ${x.toFixed(1)} ${y.toFixed(1)} L ${(x + size).toFixed(1)} ${(y + dy).toFixed(1)}`
}

interface CardProps {
  t: Translate
  run: TrajectoryRun
  stop(sessionId: string): Promise<void>
}

/** One run as a card in the chat. */
export function TrajectoryCard({ t, run, stop }: CardProps): ReactNode {
  const live = useLive()
  const running = run.endedAt === 0
  const [picked, setPicked] = useState<number | null>(null)
  const [big, setBig] = useState(false)
  const [copied, setCopied] = useState(false)
  const [stopping, setStopping] = useState(false)
  const strip = useRef<HTMLDivElement | null>(null)
  const count = run.steps.length
  const index = Math.min(picked ?? newest(run), Math.max(0, count - 1))
  const step = run.steps[index]
  // the selected thumbnail stays in view
  useEffect(() => {
    strip.current?.querySelector<HTMLElement>('.nm-traj-thumb.nm-on')?.scrollIntoView({ block: 'nearest', inline: 'nearest' })
  }, [index, count])
  useEffect(() => {
    if (!running) setStopping(false)
  }, [running])
  if (!step) return null
  const go = (to: number) => setPicked(Math.max(0, Math.min(count - 1, to)))
  const onKey = (event: { key: string; preventDefault(): void; target: EventTarget | null }) => {
    if ((event.target as HTMLElement | null)?.closest('input, textarea, [contenteditable]')) return
    if (event.key === 'ArrowLeft') { event.preventDefault(); go(index - 1) }
    else if (event.key === 'ArrowRight') { event.preventDefault(); go(index + 1) }
    else if (event.key === 'Home') { event.preventDefault(); go(0) }
    else if (event.key === 'End') { event.preventDefault(); setPicked(null) }
  }
  const copy = () => {
    const text = stepText(t, step)
    const done = () => { setCopied(true); window.setTimeout(() => setCopied(false), 1500) }
    if (navigator.clipboard?.writeText) void navigator.clipboard.writeText(text).then(done).catch(() => undefined)
  }
  const hold = live.holds.find((x) => x.thread === run.sessionId)
  const takeOver = () => void call('holds', { thread: run.sessionId, tool: 'computer', reason: '' }).catch(() => undefined)
  const stopRun = () => {
    if (stopping) return
    setStopping(true)
    void stop(run.sessionId).catch(() => setStopping(false))
  }
  const caption = `${describeStep(t, step.action, running && index === count - 1)}${step.title ? ` · ${step.title}` : ''}`
  const where = run.source === 'device' ? run.device : ''
  const nav = (cls: string) => h('div', { className: cls },
    h('button', { type: 'button', className: 'nm-traj-btn', 'aria-label': t('trajPrev'), title: t('trajPrev'), disabled: index === 0, onClick: () => go(index - 1) }, h(IconChevronLeft, { size: 16 })),
    h('span', { className: 'nm-traj-count' }, `${step.i} / ${run.dropped + count}`),
    h('button', { type: 'button', className: 'nm-traj-btn', 'aria-label': t('trajNext'), title: t('trajNext'), disabled: index === count - 1, onClick: () => go(index + 1) }, h(IconChevronRight, { size: 16 })))
  // with the sheet open its own document listener has the keys (React's events bubble through the portal; one handler, not two)
  return h('div', { className: `nm-traj-card${running ? ' nm-running' : ''}`, tabIndex: 0, role: 'group', 'aria-label': t('trajTitle'), onKeyDown: big ? undefined : onKey },
    h('div', { className: 'nm-traj-head' },
      h(IconHand, { size: 15 }),
      h('span', { className: 'nm-traj-title' }, where || t('capsuleHands')),
      running ? h('span', { className: 'nm-traj-live', 'aria-hidden': true }) : null,
      h('span', { className: 'nm-traj-sub' }, running ? t('trajRunning', { n: step.i }) : t('trajSteps', { n: run.dropped + count })),
      h('span', { className: 'nm-traj-space' }),
      nav('nm-traj-nav'),
      h('button', { type: 'button', className: 'nm-traj-btn', 'aria-label': t('trajOpen'), title: t('trajOpen'), onClick: () => setBig(true) }, h(IconExpand, { size: 15 })),
      h('button', { type: 'button', className: 'nm-traj-btn', 'aria-label': t('trajCopy'), title: copied ? t('trajCopied') : t('trajCopy'), onClick: copy }, h(IconCopy, { size: 15 }))),
    h('button', { type: 'button', className: 'nm-traj-open', 'aria-label': t('trajOpen'), onClick: () => setBig(true) }, h(StepPicture, { t, step, big: false })),
    h('div', { className: 'nm-traj-caption' }, caption),
    step.words ? h('div', { className: 'nm-traj-words nm-clamp3' }, step.words) : null,
    count > 1
      ? h('div', { className: 'nm-traj-strip', ref: strip, role: 'list' },
          run.steps.map((s, i) =>
            h('button', { key: s.i, type: 'button', role: 'listitem', className: `nm-traj-thumb${i === index ? ' nm-on' : ''}`, 'aria-label': t('trajStep', { i: s.i }), 'aria-current': i === index ? 'true' : undefined, onClick: () => go(i) },
              s.seq ? h('img', { src: frameUrl(s.seq), alt: '', loading: 'lazy', decoding: 'async', draggable: false }) : h('span', { className: 'nm-traj-thumb-gone' }),
              h('span', { className: 'nm-traj-thumb-n' }, s.i))))
      : null,
    run.dropped > 0 ? h('div', { className: 'nm-traj-fine' }, t('trajDropped', { n: run.dropped })) : null,
    running
      ? h('div', { className: 'nm-traj-actions' },
          hold
            ? h('button', { type: 'button', className: 'nm-traj-pill nm-on', onClick: () => void call(`holds/${encodeURIComponent(hold.id)}/done`, {}).catch(() => undefined) }, t('stageDoneBtn'))
            : run.source === 'computer'
              ? h('button', { type: 'button', className: 'nm-traj-pill', onClick: takeOver }, h(IconHand, { size: 14 }), t('stageTakeIt'))
              : null,
          h('button', { type: 'button', className: 'nm-traj-pill nm-stop', disabled: stopping, onClick: stopRun }, h(IconSquare, { size: 12 }), stopping ? t('capsuleStopping') : t('capsuleStop')))
      : null,
    big
      ? h(Sheet, { title: where || t('trajTitle'), onClose: () => setBig(false), closeLabel: t('close'), wide: true, header: nav('nm-traj-nav nm-traj-nav-sheet') },
          h(BigStep, { t, step, caption, onKey, onCopy: copy, copied }))
      : null)
}

/** The sheet: the step large, the arrows working from anywhere in it. */
function BigStep({ t, step, caption, onKey, onCopy, copied }: { t: Translate; step: TrajectoryStep; caption: string; onKey: (e: KeyboardEvent) => void; onCopy(): void; copied: boolean }): ReactNode {
  useEffect(() => {
    const handler = (event: KeyboardEvent) => onKey(event)
    document.addEventListener('keydown', handler)
    return () => document.removeEventListener('keydown', handler)
  }, [onKey])
  return h('div', { className: 'nm-traj-big' },
    h(StepPicture, { t, step, big: true }),
    h('div', { className: 'nm-traj-big-foot' },
      h('div', { className: 'nm-traj-big-text' },
        h('div', { className: 'nm-traj-caption' }, caption),
        step.words ? h('div', { className: 'nm-traj-words' }, step.words) : null),
      h('button', { type: 'button', className: 'nm-traj-pill', onClick: onCopy }, h(IconCopy, { size: 14 }), copied ? t('trajCopied') : t('trajCopy'))))
}

/** The element the chat lays out: the row itself, or the turn-process group it sits in. */
function flowItem(node: HTMLElement, content: HTMLElement): HTMLElement {
  let item = node
  while (item.parentElement && item.parentElement !== content && item.parentElement.hasAttribute('data-chat-group-key')) item = item.parentElement
  return item
}

/** Where a run's card goes, and a key for that place so a card is moved only when it changes. */
export function anchorFor(run: TrajectoryRun, rows: Map<string, HTMLElement>, newestOfSession: boolean): { key: string; row: HTMLElement | null; mode: 'after-row' | 'end' } | null {
  for (const id of [run.lastCall, run.firstCall]) {
    const row = id ? rows.get(toolRowKey(id)) : undefined
    if (row) return { key: `row:${id}`, row, mode: 'after-row' }
  }
  // no row of its own on screen (the steps are not rendered yet, or a run from another way in): the newest run sits at the end
  return newestOfSession ? { key: 'end', row: null, mode: 'end' } : null
}

/** Watch the chat for sessions with a run and show their trajectories; returns the stop function. */
export function renderTrajectory(t: Translate, stop: (sessionId: string) => Promise<void>): () => void {
  const cache = new Map<string, TrajectoryRun[]>()
  const inflight = new Set<string>()
  const roots = new Map<HTMLElement, Root>()
  let sessions = new Set<string>()
  let scheduled: ReturnType<typeof setTimeout> | undefined
  let stopped = false

  const unmount = (node: HTMLElement) => {
    const root = roots.get(node)
    if (root) {
      roots.delete(node)
      // never unmount while React may be rendering this tree
      window.setTimeout(() => root.unmount(), 0)
    }
    node.remove()
  }

  const fetchFor = (sessionId: string) => {
    if (inflight.has(sessionId)) return
    inflight.add(sessionId)
    call<{ runs?: TrajectoryRun[] }>(`trajectory?session=${encodeURIComponent(sessionId)}`)
      .then((view) => {
        if (stopped) return
        cache.set(sessionId, view.runs ?? [])
        schedule()
      })
      .catch(() => undefined)
      .finally(() => inflight.delete(sessionId))
  }

  const dress = (content: HTMLElement, runs: TrajectoryRun[]) => {
    const rows = new Map<string, HTMLElement>()
    for (const node of content.querySelectorAll<HTMLElement>('[data-chat-node-key]')) rows.set(node.getAttribute('data-chat-node-key') ?? '', node)
    const cards = new Map<string, HTMLElement>()
    for (const node of content.querySelectorAll<HTMLElement>('.nm-traj')) cards.set(node.getAttribute('data-nm-run') ?? '', node)
    const newestId = runs.length ? runs[runs.length - 1]!.id : ''
    for (const run of runs) {
      const anchor = anchorFor(run, rows, run.id === newestId)
      let card = cards.get(run.id)
      cards.delete(run.id)
      if (!anchor) {
        if (card) unmount(card)
        continue
      }
      if (!card) {
        card = document.createElement('div')
        card.className = 'nm-traj'
        card.setAttribute('data-nm-run', run.id)
        roots.set(card, createRoot(card))
      }
      if (card.getAttribute('data-nm-anchor') !== anchor.key || !card.isConnected) {
        card.setAttribute('data-nm-anchor', anchor.key)
        if (anchor.row) anchor.row.insertAdjacentElement('afterend', card)
        else {
          const last = [...rows.values()].at(-1)
          const seat = last ? null : content.querySelector<HTMLElement>('[data-conversation-scroll] > [data-composer-seat]')
          if (last) flowItem(last, content).insertAdjacentElement('afterend', card)
          else if (seat) seat.insertAdjacentElement('beforebegin', card)
          else content.append(card)
        }
      }
      roots.get(card)?.render(h(TrajectoryCard, { t, run, stop }))
    }
    // a card whose run is gone
    for (const node of cards.values()) unmount(node)
  }

  const sweep = () => {
    scheduled = undefined
    for (const content of document.querySelectorAll<HTMLElement>('[data-conversation-content][data-conversation-session]')) {
      const sessionId = content.getAttribute('data-conversation-session') ?? ''
      if (!sessionId) continue
      if (!sessions.has(sessionId)) {
        // nothing to look back at in this chat: its rows are not read at all
        for (const node of content.querySelectorAll<HTMLElement>('.nm-traj')) unmount(node)
        continue
      }
      const runs = cache.get(sessionId)
      if (!runs) {
        fetchFor(sessionId)
        continue
      }
      dress(content, runs)
    }
  }
  const schedule = () => {
    if (scheduled || stopped) return
    scheduled = setTimeout(() => window.requestAnimationFrame(sweep), SWEEP_DELAY_MS)
  }
  /** Whether a mutation is one the cards made themselves: no sweep for those. */
  const ours = (m: MutationRecord): boolean => {
    const target = m.target instanceof Element ? m.target : m.target.parentElement
    if (target?.closest('.nm-traj')) return true
    const nodes = [...m.addedNodes, ...m.removedNodes]
    return nodes.length > 0 && nodes.every((n) => n instanceof Element && n.classList.contains('nm-traj'))
  }
  const observer = new MutationObserver((records) => {
    if (records.some((m) => !ours(m))) schedule()
  })
  observer.observe(document.body, { childList: true, subtree: true })
  // a step was taken somewhere: the sessions on screen are read again
  let rev = -1
  const offLive = subscribeLive((live) => {
    const next = live.trajectory?.rev ?? 0
    if (next === rev) return
    rev = next
    sessions = new Set(live.trajectory?.sessions ?? [])
    cache.clear()
    schedule()
  })
  schedule()
  return () => {
    stopped = true
    observer.disconnect()
    offLive()
    if (scheduled) clearTimeout(scheduled)
    for (const node of [...roots.keys()]) unmount(node)
    for (const node of document.querySelectorAll<HTMLElement>('.nm-traj')) node.remove()
  }
}
