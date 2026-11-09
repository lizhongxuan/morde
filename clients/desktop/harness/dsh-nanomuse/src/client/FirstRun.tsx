/**
 * The first conversation's opening, in the chat (C4): the app speaks first. The three
 * paragraphs the phone shows — who it is, how it works, "what should I call you?" — are
 * drawn here as agent bubbles at the top of the main chat, for as long as the chat is the
 * one Start bound (`firstrun.json`). They cost no tokens and never enter the model's
 * history: the model is told what was said in its system prompt instead (the host's
 * `firstRunAddendum`), so the person's reply makes sense to it.
 *
 * While the agent is waiting for its own name the composer's placeholder says where to
 * write one ("Write a name here"), as the phone's does.
 */
import { createElement as h, useEffect, useState, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { introLines, showsIntro } from '../firstrun.ts'
import type { Translate } from './api.ts'
import { useRooms } from './rooms.ts'
import { useWin } from './win.ts'

const HOST = 'nm-intro-host'

/** The conversation column of the chat on screen, when it is mounted. */
function scrollOf(sessionId: string): HTMLElement | null {
  const content = document.querySelector<HTMLElement>(`[data-conversation-content][data-conversation-session="${CSS.escape(sessionId)}"]`)
  return content?.querySelector<HTMLElement>('[data-conversation-scroll]') ?? content ?? null
}

/** A host node inserted as the first child of the chat's column; re-inserted when the chat remounts. */
function useIntroHost(sessionId: string | null): HTMLElement | null {
  const [host, setHost] = useState<HTMLElement | null>(null)
  useEffect(() => {
    if (!sessionId) {
      setHost(null)
      return
    }
    const node = document.createElement('div')
    node.className = HOST
    node.setAttribute('data-nm-intro', sessionId)
    let placed: HTMLElement | null = null
    const place = () => {
      const scroll = scrollOf(sessionId)
      if (!scroll) {
        if (node.isConnected) node.remove()
        placed = null
        return
      }
      if (scroll.firstElementChild === node) return
      scroll.insertBefore(node, scroll.firstChild)
      if (placed !== scroll) {
        placed = scroll
        setHost(node)
      }
    }
    place()
    let scheduled = 0
    const observer = new MutationObserver(() => {
      if (!scheduled) scheduled = window.requestAnimationFrame(() => { scheduled = 0; place() })
    })
    observer.observe(document.body, { childList: true, subtree: true })
    setHost(node)
    return () => {
      observer.disconnect()
      if (scheduled) window.cancelAnimationFrame(scheduled)
      node.remove()
    }
  }, [sessionId])
  return host
}

export function makeFirstRunIntro(t: Translate) {
  return function FirstRunIntro(): ReactNode {
    const rooms = useRooms()
    const win = useWin()
    const { firstRun } = rooms
    const bound = win.current !== null && showsIntro(firstRun, win.current) ? win.current : null
    const host = useIntroHost(bound)
    // the composer's placeholder, while a name is wanted
    const naming = bound !== null && firstRun.phase === 'ask_agent_name' && firstRun.chosen === null
    useEffect(() => {
      if (!naming) return
      const root = document.documentElement
      const before = root.style.getPropertyValue('--nm-placeholder')
      root.style.setProperty('--nm-placeholder', JSON.stringify(t('frNamingPlaceholder')))
      return () => {
        if (before) root.style.setProperty('--nm-placeholder', before)
        else root.style.removeProperty('--nm-placeholder')
      }
    }, [naming])
    if (!host || !bound) return null
    const lang = rooms.lang || (typeof navigator !== 'undefined' ? navigator.language : 'en')
    const lines = introLines(lang)
    return createPortal(
      h('div', { className: 'nm-intro', 'data-chat-flow-kind': 'nm-intro' },
        lines.map((line, i) => h('div', { key: i, className: 'nm-intro-bubble' }, line))),
      host)
  }
}
