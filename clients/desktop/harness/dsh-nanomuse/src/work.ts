/**
 * The work a real exit protects, and the gate the exit hands over (the desktop
 * shell's protected quit, and the restart-update request that reuses it): what
 * counts as busy, tracked where each kind already reports itself. A quit asks
 * the host for the aggregate, closes the gate so new work does not start under
 * the hand-over, and only on an explicit confirmation ends what is running.
 */

/** One kind of work a quit or a restart-update would end. */
export type WorkKind = 'reply' | 'computer' | 'task' | 'approval'

/** One line of the aggregate: the kind, and the words the work itself carries (a device's name; empty otherwise). */
export interface WorkItem {
  kind: WorkKind
  label: string
}

/**
 * The turns in progress, by session, across every session of the host: the
 * chat replies, the goals' automations and the tasks other devices asked for
 * are all turns here. Fed from `session/event` (`turn/start`, `turn/end`).
 */
export class Turns {
  private readonly open = new Map<string, number>()

  /** A turn opened on `sessionId`; a second start on the same session is the same turn. */
  began(sessionId: string, now = Date.now()): boolean {
    if (this.open.has(sessionId)) return false
    this.open.set(sessionId, now)
    return true
  }

  /** A turn ended (or was stopped) on `sessionId`. */
  ended(sessionId: string): boolean {
    return this.open.delete(sessionId)
  }

  /** The sessions with a turn open, oldest first. */
  sessions(): string[] {
    return [...this.open.entries()].sort((a, b) => a[1] - b[1]).map(([id]) => id)
  }
}

/** What the host has on, as each source already reports it: the turns of all sessions, the tasks other devices asked for, the hands' calls, the approvals waiting. */
export interface WorkSources {
  turnSessions: string[]
  taskRuns: { sessionId: string; from: string }[]
  handsBusy: boolean
  approvals: number
}

/**
 * One line per kind of work a real exit would end: a turn that is not a task's
 * is a reply, a task names the devices that asked, the hands and the approvals
 * speak for themselves. The task's own session does not count twice.
 */
export function aggregateWork(sources: WorkSources): WorkItem[] {
  const items: WorkItem[] = []
  const taskSessions = new Set(sources.taskRuns.map((run) => run.sessionId))
  const replies = sources.turnSessions.filter((id) => !taskSessions.has(id))
  if (replies.length > 0) items.push({ kind: 'reply', label: '' })
  if (sources.taskRuns.length > 0) items.push({ kind: 'task', label: [...new Set(sources.taskRuns.map((run) => run.from))].join(', ').slice(0, 120) })
  if (sources.handsBusy) items.push({ kind: 'computer', label: '' })
  if (sources.approvals > 0) items.push({ kind: 'approval', label: '' })
  return items
}
