/**
 * Search (⌘K), the way the Muse desktop opens it: a modal centred over a dimmed
 * window with one field and, under it, "Recently visited" — the chats by last
 * activity, each with its title, a line of what was last said in it and when.
 * Typing narrows the list by title and by that line; ↑ ↓ move, Enter opens.
 */
import { createElement as h, useEffect, useMemo, useRef, useState, type FormEvent, type ReactNode } from 'react'
import type { Translate } from './api.ts'
import { IconChat, IconSearch } from './icons.tsx'
import { composing } from './keys.ts'
import { useSyncList, type ChatActions, type UseSessionList } from './MuseChats.tsx'
import { useRooms } from './rooms.ts'
import { ago } from './ui.tsx'
import { useWin, win } from './win.ts'

export interface SearchModalProps {
  t: Translate
  useSessions?: UseSessionList | undefined
  actions: ChatActions
}

interface Hit {
  id: string
  title: string
  preview: string
  at: number
}

export function SearchModal({ t, useSessions, actions }: SearchModalProps): ReactNode {
  const open = useWin().search
  if (!open) return null
  return h(Modal, { t, useSessions, actions })
}

function Modal({ t, useSessions, actions }: SearchModalProps): ReactNode {
  const rooms = useRooms()
  const [query, setQuery] = useState('')
  const [index, setIndex] = useState(0)
  const field = useRef<HTMLInputElement>(null)
  const list = useRef<HTMLDivElement>(null)
  // another account's chats are not searched either (C10)
  const sync = useSyncList()
  const chats = typeof useSessions === 'function'
    ? useSessions((s) => s.ids.map((id) => s.byId[id]).filter((c) => c !== undefined && c.origin !== 'subagent' && !c.blank).map((c) => ({ id: c!.id, title: c!.displayTitle, at: c!.updatedAt })))
    : []
  const hits = useMemo<Hit[]>(() => {
    // nor are the rooms' running batches (the feed being written)
    const rows = chats
      .filter((c) => !sync.foreign.has(c.id) && !rooms.batches.includes(c.id))
      .map((c) => {
        const record = rooms.activity.find((r) => r.sessionId === c.id)
        return { id: c.id, title: c.title, preview: record?.words || record?.request || '', at: Math.max(c.at, record?.updatedAt ?? 0) }
      })
      .sort((a, b) => b.at - a.at)
    const q = query.trim().toLowerCase()
    return (q ? rows.filter((r) => r.title.toLowerCase().includes(q) || r.preview.toLowerCase().includes(q)) : rows).slice(0, 12)
  }, [chats, rooms.activity, rooms.batches, query, sync.foreign])

  useEffect(() => { field.current?.focus() }, [])
  useEffect(() => { setIndex(0) }, [query])
  useEffect(() => {
    const el = list.current?.children[index] as HTMLElement | undefined
    el?.scrollIntoView({ block: 'nearest' })
  }, [index])

  const close = () => win.search(false)
  const pick = (hit: Hit | undefined) => {
    if (!hit) return
    close()
    actions.openSession(hit.id)
  }
  const onKey = (event: React.KeyboardEvent) => {
    if (event.key === 'Escape') { event.preventDefault(); close() }
    else if (event.key === 'ArrowDown') { event.preventDefault(); setIndex((i) => Math.min(hits.length - 1, i + 1)) }
    else if (event.key === 'ArrowUp') { event.preventDefault(); setIndex((i) => Math.max(0, i - 1)) }
    else if (event.key === 'Enter' && !composing(event)) { event.preventDefault(); pick(hits[index]) }
  }

  return h('div', { className: 'nm-search-backdrop', onMouseDown: (e: React.MouseEvent) => { if (e.target === e.currentTarget) close() } },
    h('div', { className: 'nm-search', role: 'dialog', 'aria-modal': true, 'aria-label': t('railSearch'), onKeyDown: onKey },
      h('div', { className: 'nm-search-field' },
        h(IconSearch, { size: 18 }),
        h('input', { ref: field, className: 'nm-search-input', placeholder: t('searchPlaceholder'), value: query, 'aria-label': t('railSearch'), onChange: (e: FormEvent<HTMLInputElement>) => setQuery(e.currentTarget.value) })),
      h('div', { className: 'nm-search-label' }, query.trim() ? t('searchResults') : t('searchRecent')),
      hits.length === 0
        ? h('div', { className: 'nm-search-empty' }, query.trim() ? t('searchNone') : t('chEmpty'))
        : h('div', { ref: list, className: 'nm-search-list', role: 'listbox' }, hits.map((hit, i) =>
            h('button', { key: hit.id, type: 'button', role: 'option', 'aria-selected': i === index, className: `nm-search-row${i === index ? ' nm-active' : ''}`, onMouseEnter: () => setIndex(i), onClick: () => pick(hit) },
              h('span', { className: 'nm-search-row-icon' }, h(IconChat, { size: 18 })),
              h('span', { className: 'nm-search-row-main' },
                h('span', { className: 'nm-search-row-title' }, hit.title),
                hit.preview ? h('span', { className: 'nm-search-row-sub' }, hit.preview) : null),
              h('span', { className: 'nm-search-row-time' }, ago(t, hit.at)))))))
}
