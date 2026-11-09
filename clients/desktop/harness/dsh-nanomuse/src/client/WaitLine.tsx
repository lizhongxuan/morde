/**
 * The line above a chat's composer while its turn has no first token yet (0.31): nothing for
 * ten seconds, then "The model is busy, 15 s so far" with a Cancel that stops the turn. The
 * host keeps the account's calls that are still waiting (`live.waiting`, by session) and drops
 * each at its first chunk; the seconds are counted here. The phones draw the same line under
 * their typing dots, and add "3 ahead of you" from the relay's status chunks, which the
 * harness's parser does not hand on, so the desktop says how long and nothing more.
 */
import { createElement as h, useEffect, useState, type ReactNode } from 'react'
import { type Translate } from './api.ts'
import { useLive } from './live.ts'

/** The dots have bounced this long before the line says anything. */
export const WAIT_LINE_AFTER_S = 10

export interface WaitLineProps {
  t: Translate
  /** The chat this composer belongs to. */
  sessionId?: string | undefined
  /** Stops the turn of a session (the header's Stop). */
  stop: (sessionId: string) => Promise<void>
}

export function WaitLine({ t, sessionId, stop }: WaitLineProps): ReactNode {
  const live = useLive()
  const wait = (live.waiting ?? []).find((w) => w.session === sessionId)
  const since = wait?.since
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (since === undefined) return undefined
    setNow(Date.now())
    const id = setInterval(() => { setNow(Date.now()) }, 1000)
    return () => { clearInterval(id) }
  }, [since])
  if (since === undefined) return null
  const seconds = Math.max(0, Math.floor((now - since) / 1000))
  if (seconds < WAIT_LINE_AFTER_S) return null
  return h('div', { className: 'nm-waitline', role: 'status', 'data-testid': 'nm-waitline' },
    h('span', { className: 'nm-waitline-text' }, t('wlBusy', { seconds })),
    sessionId
      ? h('button', { type: 'button', className: 'nm-ob-link nm-inline', onClick: () => { void stop(sessionId).catch(() => undefined) } }, t('wlCancel'))
      : null)
}
