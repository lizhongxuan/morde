/**
 * Words and pictures for one step of the hands, shared by the capsule outside the window
 * (`overlay.ts`) and the trajectory in the chat (`Trajectory.tsx`): the frame's URL, the
 * caption's verb, and how keys are written.
 */
import type { Translate } from './api.ts'
import type { LiveStageAction } from './live.ts'

/** Document-relative, so it resolves under whatever mount served the page. */
export function frameUrl(seq: number): string {
  return `nanomuse/cloud/stage/frame?seq=${seq}`
}

/** `enter`→↵ and friends, the way Muse writes keys in its captions. */
export function keyWords(keys: string): string {
  const names: Record<string, string> = { enter: '↵', return: '↵', cmd: '⌘', command: '⌘', meta: '⌘', ctrl: '⌃', control: '⌃', alt: '⌥', option: '⌥', shift: '⇧', tab: '⇥', esc: '⎋', escape: '⎋', backspace: '⌫', delete: '⌦', space: '␣', up: '↑', down: '↓', left: '←', right: '→' }
  return keys
    .split('+')
    .map((k) => k.trim())
    .filter(Boolean)
    .map((k) => names[k.toLowerCase()] ?? (k.length === 1 ? k.toUpperCase() : k))
    .join(' ')
}

/** The caption's verb: what the hands just did, in the past tense. */
export function describeStep(t: Translate, action: LiveStageAction | null, busy: boolean): string {
  if (!action) return busy ? t('stageLooking') : t('stageDone')
  switch (action.kind) {
    case 'look':
      return busy ? t('stageLooking') : t('stageLooked')
    case 'click':
    case 'double_click':
    case 'right_click':
    case 'middle_click':
      return action.label ? t('stageClicked', { label: action.label }) : t('stageClickedSomewhere')
    case 'type':
      return t('stageTyped', { text: action.text })
    case 'key':
      return t('stagePressed', { keys: keyWords(action.text) })
    case 'scroll':
      return (action.dy ?? 0) < 0 ? t('stageScrolledUp') : t('stageScrolled')
    case 'drag':
      return t('stageDragged')
    case 'move':
      return t('stageMoved')
    case 'open_app':
      return t('stageOpened', { app: action.text })
    case 'wait':
      return t('stageWaiting')
    case 'hold':
    case 'hand_over':
      return action.text ? `${t('stageYourTurn')} · ${action.text}` : t('stageYourTurn')
    default:
      return action.kind
  }
}

/** The kinds that aim at a point of the frame. */
export const POINTED = new Set(['click', 'double_click', 'right_click', 'middle_click', 'move', 'drag', 'scroll'])

/** Whether the action has a point on a frame of this size. */
export function pointed(action: LiveStageAction | null, width: number, height: number): boolean {
  return Boolean(action && action.x >= 0 && action.y >= 0 && width > 0 && height > 0 && POINTED.has(action.kind))
}
