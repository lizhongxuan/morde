/**
 * Ideas (点子): the phone's curated list of things the agent can do today
 * (`assets/ideas.<lang>.json`, the same items), in sections — Travel, Work,
 * Everyday, Learning, Family & friends, More ideas — each row an emoji, a
 * first-person pitch and a grey line of detail; a green check once tried.
 * A row opens the idea's sheet, which says what trying it creates — a
 * conversation, a daily routine, a goal — and offers to do it: Send to chat puts
 * the words in the composer for the person to finish; Create routine schedules
 * it daily at the idea's time in a chat of its own; Start goal opens the goal
 * conversation with the idea's words. ⋯ on a row hides it.
 */
import { failureText } from './api.ts'
import { createElement as h, useMemo, useState, type ReactNode } from 'react'
import type { Translate } from './api.ts'
import { prefillComposer } from './composer.ts'
import { IconAlarm, IconBulb, IconCheckCircle, IconMessage, IconTarget } from './icons.tsx'
import { IDEAS_PANEL } from './panels.ts'
import { nav, roomsCall, useRooms, type Idea } from './rooms.ts'
import { peekWin } from './win.ts'
import { Empty, MoreButton, RoomToggle, Sheet } from './ui.tsx'

export function makeIdeasPanel(t: Translate) {
  return function IdeasPanel(): ReactNode {
    const rooms = useRooms()
    const [open, setOpen] = useState<Idea | undefined>()
    const [error, setError] = useState<string | undefined>()
    const [starting, setStarting] = useState(false)
    const [toast, setToast] = useState<string | undefined>()
    const items = rooms.ideas.items.filter((i) => !i.dismissed)
    const groups = useMemo(() => {
      const order: string[] = []
      const byGroup = new Map<string, Idea[]>()
      for (const idea of items) {
        const key = idea.group || ''
        if (!byGroup.has(key)) {
          byGroup.set(key, [])
          order.push(key)
        }
        byGroup.get(key)!.push(idea)
      }
      return order.map((key) => ({ name: key, ideas: byGroup.get(key)! }))
    }, [items])

    const fail = (err: unknown) => setError(failureText(t, err))
    const say = (text: string) => {
      setToast(text)
      window.setTimeout(() => setToast((current) => (current === text ? undefined : current)), 3500)
    }
    // Send to chat: the words in the composer of the chat the person is in, for them to finish
    const sendToChat = (idea: Idea) => {
      setOpen(undefined)
      if (nav.activePanel() !== null) nav.showChats()
      nav.split(IDEAS_PANEL)
      prefillComposer(idea.prompt)
      const current = peekWin().current
      if (current) roomsCall('ideas/mark', { id: idea.id, sessionId: current }).catch(() => undefined)
    }
    // Create routine / Start goal: the host does it and answers with the chat to open
    const start = (idea: Idea) => {
      setStarting(true)
      setError(undefined)
      roomsCall<{ sessionId: string; scheduleId?: string }>('ideas/start', { id: idea.id })
        .then(({ sessionId, scheduleId }) => {
          setOpen(undefined)
          nav.openSession(sessionId)
          nav.split(IDEAS_PANEL)
          if (scheduleId) say(t('ideaRoutineCreated'))
        })
        .catch(fail)
        .finally(() => setStarting(false))
    }
    const tryIt = (idea: Idea) => { if ((idea.kind ?? 'chat') === 'chat') sendToChat(idea); else start(idea) }
    const dismiss = (idea: Idea) => { roomsCall('ideas/dismiss', { id: idea.id }).catch(fail) }

    return h('div', { className: 'nm-room' },
      h('div', { className: 'nm-room-top', 'data-window-drag': true }),
      h('div', { className: 'nm-room-head' },
        h(RoomToggle, { t, panel: IDEAS_PANEL }),
        h('h1', { className: 'nm-room-title' }, t('railIdeas'))),
      h('div', { className: 'nm-room-body' },
        h('div', { className: 'nm-room-inner nm-ideas' },
          error ? h('div', { className: 'nm-room-error' }, error) : null,
          toast ? h('div', { className: 'nm-room-toast', role: 'status' }, toast) : null,
          items.length === 0
            ? h(Empty, { icon: h(IconBulb, { size: 28 }), text: t('ideasEmpty') }, h('p', { className: 'nm-empty-sub' }, t('ideasEmptyHint')))
            : groups.map((group, index) => h('section', { key: group.name || `g${index}`, className: 'nm-idea-group' },
                // the first section goes without a heading, as on the phone
                group.name && index > 0 ? h('h2', { className: 'nm-room-h2' }, group.name) : null,
                group.ideas.map((idea) => h(IdeaRow, { key: idea.id, t, idea, onOpen: () => setOpen(idea), onDismiss: () => dismiss(idea), onStart: () => tryIt(idea) })))))),
      open ? h(IdeaCard, { t, idea: open, starting, onClose: () => setOpen(undefined), onChat: () => sendToChat(open), onStart: () => start(open) }) : null)
  }
}

function kindOf(idea: Idea): 'chat' | 'routine' | 'goal' {
  return idea.kind === 'routine' || idea.kind === 'goal' ? idea.kind : 'chat'
}

function IdeaRow({ t, idea, onOpen, onDismiss, onStart }: { t: Translate; idea: Idea; onOpen(): void; onDismiss(): void; onStart(): void }): ReactNode {
  const kind = kindOf(idea)
  return h('div', { className: 'nm-idea', role: 'button', tabIndex: 0, 'aria-label': idea.title, onClick: onOpen, onKeyDown: (e: KeyboardEvent) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onOpen() } } },
    h('div', { className: 'nm-idea-mark', 'aria-hidden': true }, idea.emoji || '💡'),
    h('div', { className: 'nm-idea-main' },
      h('div', { className: 'nm-idea-title' }, idea.title),
      idea.detail ? h('div', { className: 'nm-idea-detail' }, idea.detail) : null),
    idea.started
      ? h('span', { className: 'nm-idea-done', title: t('ideaStarted') }, h(IconCheckCircle, { size: 20 }))
      : h(MoreButton, { label: t('more'), quiet: true, size: 28, items: [
          { id: 'start', label: t(kind === 'routine' ? 'ideaCreateRoutine' : kind === 'goal' ? 'ideaStartGoal' : 'ideaSendToChat'), onSelect: onStart },
          { id: 'dismiss', label: t('ideaDismiss'), onSelect: onDismiss },
        ] }))
}

/** The idea's sheet: what it does, what trying it creates, and the buttons (the phone's bottom sheet). */
function IdeaCard({ t, idea, starting, onClose, onChat, onStart }: { t: Translate; idea: Idea; starting: boolean; onClose(): void; onChat(): void; onStart(): void }): ReactNode {
  const kind = kindOf(idea)
  const kindText = kind === 'routine' ? `${t('ideaKindRoutine')}${idea.time ? ` · ${idea.time}` : ''}` : kind === 'goal' ? t('ideaKindGoal') : t('ideaKindChat')
  const KindIcon = kind === 'routine' ? IconAlarm : kind === 'goal' ? IconTarget : IconMessage
  const primary = kind === 'routine' ? t('ideaCreateRoutine') : kind === 'goal' ? t('ideaStartGoal') : t('ideaSendToChat')
  const started = Boolean(idea.started)
  return h(Sheet, { title: idea.title, closeLabel: t('close'), onClose, wide: true,
    footer: h('div', { className: 'nm-sheet-actions nm-idea-actions' },
      kind !== 'chat' ? h('button', { type: 'button', className: 'nm-pill nm-pill-ghost nm-pill-sm', onClick: onChat }, t('ideaSendToChat')) : null,
      h('span', { style: { flex: 1 } }),
      started && kind !== 'chat'
        ? h('button', { type: 'button', className: 'nm-pill nm-pill-sm', onClick: () => { nav.openSession(idea.started!); nav.split(IDEAS_PANEL); onClose() } }, t('ideaOpenChat'))
        : h('button', { type: 'button', className: 'nm-pill nm-pill-sm', disabled: starting, onClick: kind === 'chat' ? onChat : onStart }, starting ? t('ideaStarting') : primary)) },
    h('div', { className: 'nm-idea-card' },
      h('div', { className: 'nm-idea-hero' }, h('span', { className: 'nm-idea-hero-mark', 'aria-hidden': true }, idea.emoji || '💡')),
      idea.detail ? h('p', { className: 'nm-sheet-lead' }, idea.detail) : null,
      h('div', { className: 'nm-idea-kind' }, h(KindIcon, { size: 15 }), h('span', null, kindText)),
      h('section', null, h('h3', null, t('ideaPrompt')), h('blockquote', { className: 'nm-idea-prompt' }, idea.prompt))))
}
