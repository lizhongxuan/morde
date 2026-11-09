/**
 * *Always in this chat* and *Always in <app>* beside the chat's approval card. The card is the
 * harness's own (Allow once, Reject) and has no place for more; when the question is a hands
 * step of ours (the host's pending approval names the app it asked in), a strip just above the
 * card offers the two standing answers. Either is the stage's answer (`POST approvals/<id>` with
 * the scope), which the host keeps as a grant and which withdraws the card, as the capsule's does.
 *
 * In a scheduled task's run, *Always in this task* stands in for *Always in this chat*.
 *
 * Like `Trajectory.tsx`, the strip is placed by a MutationObserver sweep; nothing of React's is
 * touched, and a strip whose question is gone is taken away.
 */
import { appLabel } from '../desk.ts'
import { call, type Translate } from './api.ts'
import { recordApproval } from './prefs.ts'
import { subscribeLive, type PendingApproval } from './live.ts'

const SWEEP_DELAY_MS = 60

export function renderApprovalScopes(t: Translate): () => void {
  let pending: PendingApproval[] = []
  let scheduled: ReturnType<typeof setTimeout> | undefined
  let stopped = false

  /** The hands question the card in this conversation is showing, if one of ours is waiting. */
  const questionFor = (panel: HTMLElement): PendingApproval | undefined => {
    const hands = pending.filter((a) => a.app)
    const sessionId = panel.closest('[data-conversation-session]')?.getAttribute('data-conversation-session') ?? ''
    if (sessionId) return hands.find((a) => a.sessionId === sessionId)
    // no conversation around the card: only an unambiguous question is answered from here
    return hands.length === 1 ? hands[0] : undefined
  }

  const strip = (approval: PendingApproval): HTMLElement => {
    const node = document.createElement('div')
    node.className = 'nm-ap-scope'
    node.setAttribute('data-nm-approval', approval.id)
    const label = document.createElement('span')
    label.className = 'nm-ap-scope-label'
    label.textContent = t('apScopeLead')
    node.append(label)
    const button = (text: string, scope: 'schedule' | 'conversation' | 'always') => {
      const b = document.createElement('button')
      b.type = 'button'
      b.className = 'nm-pill nm-pill-ghost nm-pill-sm'
      b.textContent = text
      b.addEventListener('click', () => {
        for (const other of node.querySelectorAll('button')) other.disabled = true
        recordApproval({ at: Date.now(), toolName: approval.toolName, reason: approval.summaryZh || approval.summary, outcome: 'allowed' })
        void call(`approvals/${encodeURIComponent(approval.id)}`, { approved: true, scope, reason: '' }).catch(() => {
          for (const other of node.querySelectorAll('button')) other.disabled = false
        })
      })
      node.append(b)
    }
    if (approval.scheduleId) button(t('stageAlwaysTask'), 'schedule')
    else button(t('stageAlwaysChat'), 'conversation')
    button(t('stageAlwaysApp', { app: appLabel(approval.app).slice(0, 20) }), 'always')
    return node
  }

  const sweep = () => {
    scheduled = undefined
    const seen = new Set<HTMLElement>()
    for (const panel of document.querySelectorAll<HTMLElement>('[data-approval-key]')) {
      const approval = questionFor(panel)
      if (!approval) continue
      let node = panel.previousElementSibling as HTMLElement | null
      if (!node?.classList.contains('nm-ap-scope') || node.getAttribute('data-nm-approval') !== approval.id) {
        if (node?.classList.contains('nm-ap-scope')) node.remove()
        node = strip(approval)
        panel.insertAdjacentElement('beforebegin', node)
      }
      seen.add(node)
    }
    // a strip whose card or question is gone
    for (const node of document.querySelectorAll<HTMLElement>('.nm-ap-scope')) if (!seen.has(node)) node.remove()
  }
  const schedule = () => {
    if (scheduled || stopped) return
    scheduled = setTimeout(() => window.requestAnimationFrame(sweep), SWEEP_DELAY_MS)
  }
  /** Whether a mutation is one the strips made themselves: no sweep for those. */
  const ours = (m: MutationRecord): boolean => {
    const target = m.target instanceof Element ? m.target : m.target.parentElement
    if (target?.closest('.nm-ap-scope')) return true
    const nodes = [...m.addedNodes, ...m.removedNodes]
    return nodes.length > 0 && nodes.every((n) => n instanceof Element && n.classList.contains('nm-ap-scope'))
  }
  const observer = new MutationObserver((records) => {
    if (records.some((m) => !ours(m))) schedule()
  })
  observer.observe(document.body, { childList: true, subtree: true })
  const offLive = subscribeLive((live) => {
    const key = live.approvals.map((a) => `${a.id}:${a.app}:${a.scheduleId}`).join('|')
    if (key === pending.map((a) => `${a.id}:${a.app}:${a.scheduleId}`).join('|')) return
    pending = live.approvals
    schedule()
  })
  schedule()
  return () => {
    stopped = true
    observer.disconnect()
    offLive()
    if (scheduled) clearTimeout(scheduled)
    for (const node of document.querySelectorAll<HTMLElement>('.nm-ap-scope')) node.remove()
  }
}
