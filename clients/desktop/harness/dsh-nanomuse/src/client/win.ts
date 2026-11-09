/**
 * The window's own state, the parts the Muse desktop keeps outside any one
 * chat: a room shown beside the chat (the split), the document open in the
 * editor, the search modal, the documents the rail lists as recent, and
 * which chat is on screen. One small store, read with `useSyncExternalStore`;
 * the panels and the sidebar write it through the functions below.
 */
import { useSyncExternalStore } from 'react'
import type { DocName } from './rooms.ts'

/** A document open in the editor: one of the agent's own, or a Library file. */
export type OpenDoc = { kind: 'own'; doc: DocName } | { kind: 'library'; id: string }

export interface RecentDoc {
  key: string
  name: string
  pinned: boolean
  at: number
}

export interface WinState {
  /** The room shown beside the chat, or null. */
  split: string | null
  editor: OpenDoc | null
  search: boolean
  recent: RecentDoc[]
  /** The chat on screen, as the session-scoped seats report it. */
  current: string | null
}

const RECENT_KEY = 'nanomuse.recentDocs'
const RECENT_MAX = 4

function readRecent(): RecentDoc[] {
  try {
    const raw = JSON.parse(globalThis.localStorage?.getItem(RECENT_KEY) ?? '[]') as unknown
    return Array.isArray(raw) ? raw.filter((r): r is RecentDoc => Boolean(r) && typeof r === 'object' && typeof (r as RecentDoc).key === 'string') : []
  } catch {
    return []
  }
}

let state: WinState = { split: null, editor: null, search: false, recent: readRecent(), current: null }
const listeners = new Set<() => void>()

function set(patch: Partial<WinState>): void {
  state = { ...state, ...patch }
  for (const listener of listeners) listener()
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener)
  return () => { listeners.delete(listener) }
}

export function useWin(): WinState {
  return useSyncExternalStore(subscribe, () => state, () => state)
}

export function peekWin(): WinState {
  return state
}

export const win = {
  split(panel: string | null): void {
    if (state.split !== panel) set({ split: panel })
  },
  current(sessionId: string | null): void {
    if (state.current !== sessionId) set({ current: sessionId })
  },
  openDoc(doc: OpenDoc | null): void {
    set({ editor: doc })
  },
  search(open: boolean): void {
    set({ search: open })
  },
  /** A document was opened: it moves to the top of the rail's recent list (pinned ones stay). */
  touchRecent(key: string, name: string): void {
    const existing = state.recent.find((r) => r.key === key)
    const next = [{ key, name, pinned: existing?.pinned ?? false, at: Date.now() }, ...state.recent.filter((r) => r.key !== key)]
    const pinned = next.filter((r) => r.pinned)
    const rest = next.filter((r) => !r.pinned).slice(0, Math.max(0, RECENT_MAX - pinned.length))
    saveRecent([...pinned, ...rest])
  },
  pinRecent(key: string, pinned: boolean): void {
    saveRecent(state.recent.map((r) => (r.key === key ? { ...r, pinned } : r)))
  },
  dropRecent(key: string): void {
    saveRecent(state.recent.filter((r) => r.key !== key))
  },
}

function saveRecent(recent: RecentDoc[]): void {
  try {
    globalThis.localStorage?.setItem(RECENT_KEY, JSON.stringify(recent))
  } catch {
    // private mode: the list lives until reload
  }
  set({ recent })
}
