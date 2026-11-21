/** Task progress and one selected work item's recorded tool activity, inside the right drawer. */
import { createElement as h, useEffect, useState, type ReactNode } from 'react'
import { failureText, type Translate } from './api.ts'
import { IconCheck, IconCheckCircle, IconChevronRight, IconClose, IconCpu, IconFile, IconGlobe, IconSparkle, IconSpinner, IconSquare } from './icons.tsx'
import type { ChatListState } from './MuseChats.tsx'
import { roomsCall, useRooms, type ActivityRecord, type ActivityStep } from './rooms.ts'
import { clockLabel } from './ui.tsx'

export interface TaskDetailProps {
  t: Translate
  sessionId: string
  useSessions?: (<S>(selector: (state: ChatListState) => S) => S) | undefined
  onClose(): void
}

export function TaskDetail({ t, sessionId, useSessions, onClose }: TaskDetailProps): ReactNode {
  const rooms = useRooms()
  const [record, setRecord] = useState<ActivityRecord | null>(null)
  const [error, setError] = useState<string | undefined>()
  const [selectedKey, setSelectedKey] = useState<string | null>(null)
  const title = typeof useSessions === 'function' ? useSessions((s) => s.byId[sessionId]?.displayTitle) : undefined
  const live = rooms.activity.find((r) => r.sessionId === sessionId)

  useEffect(() => {
    let alive = true
    roomsCall<ActivityRecord>(`activity?session=${encodeURIComponent(sessionId)}`)
      .then((value) => { if (alive) { setRecord(value); setError(undefined) } })
      .catch((err: unknown) => { if (alive) setError(failureText(t, err)) })
    return () => { alive = false }
  }, [sessionId, live?.updatedAt])
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return
      event.stopPropagation()
      if (selectedKey) setSelectedKey(null)
      else onClose()
    }
    document.addEventListener('keydown', onKey, true)
    return () => document.removeEventListener('keydown', onKey, true)
  }, [onClose, selectedKey])

  const running = (record?.status ?? live?.status) === 'running'
  const taskTitle = shortText(title?.trim() || record?.request || '', 36) || t('chUntitled')
  const toolSteps = (record?.steps ?? []).map((step, index) => ({ step, key: `${step.at}-${index}` })).filter(({ step }) => step.kind === 'tool')
  const answer = [...(record?.steps ?? [])].reverse().find((step) => step.kind === 'answer')
  const workItems = toolSteps.length ? toolSteps : answer ? [{ step: answer, key: `answer-${answer.at}` }] : []
  const selected = selectedKey ? workItems.find((item) => item.key === selectedKey)?.step : undefined

  const toolName = (step: ActivityStep) => {
    const mcp = /^mcp__(.+?)__(.+)$/.exec(step.name)
    const parts = mcp ? [mcp[1] ?? '', mcp[2] ?? ''] : [step.name]
    return parts.map((part) => part.replace(/[_-]/g, ' ').trim()).filter(Boolean).join(' · ') || t('taskNoTool')
  }
  const stepTitle = (step: ActivityStep) => step.kind === 'answer' ? t('taskNoTool') : shortText(step.title) || toolName(step)
  const stepStatus = (step: ActivityStep) => {
    if (step.kind === 'answer') return t('taskAnalysisNoTool')
    if (step.ok === false) return t('taskFailed')
    if (step.ok === true) return t('taskStepDone')
    return running && record?.current?.name === step.name ? t('taskPending') : t('taskNothing')
  }
  const stepIcon = (step: ActivityStep) => {
    if (step.kind === 'answer') return h(IconCheck, { size: 16 })
    if (step.ok === false) return h(IconClose, { size: 16 })
    if (step.ok === undefined && running && record?.current?.name === step.name) return h(IconSpinner, { size: 16 })
    const name = step.name.replace(/^mcp__[^_]+(?:_[^_]+)*?__/, '')
    if (/file|edit|write|read|glob|grep|present/i.test(name)) return h(IconFile, { size: 16 })
    if (/web|fetch|search|browser/i.test(name)) return h(IconGlobe, { size: 16 })
    if (/computer|screen|act|hands/i.test(name)) return h(IconCpu, { size: 16 })
    if (/draw|studio|image/i.test(name)) return h(IconSparkle, { size: 16 })
    return step.ok ? h(IconCheckCircle, { size: 16 }) : h(IconSquare, { size: 12 })
  }

  return h('div', { className: 'nm-task-backdrop' },
    h('aside', { className: 'nm-task', role: 'complementary', 'aria-label': taskTitle },
      h('div', { className: 'nm-task-head' },
        h('button', { type: 'button', className: 'nm-task-back', onClick: () => selectedKey ? setSelectedKey(null) : onClose() },
          h(IconChevronRight, { size: 16 }), selectedKey ? t('taskBackToDetails') : t('taskBackToList'))),
      error ? h('div', { className: 'nm-doc-error', role: 'alert' }, error) : null,
      !record && !error ? h('div', { className: 'nm-task-loading' }, t('loading')) : null,
      record && !selectedKey
        ? h('div', { className: 'nm-task-scroll' },
            h('header', { className: 'nm-task-summary' },
              h('h2', { className: 'nm-task-title' }, taskTitle),
              h('span', { className: `nm-task-pill${running ? ' nm-live' : ''}` },
                running ? h(IconSpinner, { size: 12 }) : null,
                running ? t('taskRunning') : t('taskStopped'))),
            h('div', { className: 'nm-task-process-title' }, t('taskProcess')),
            workItems.length
              ? h('div', { className: 'nm-task-work-list', role: 'list' }, workItems.map(({ step, key }) => h('button', {
                  key,
                  type: 'button',
                  className: `nm-task-work${step.ok === false ? ' nm-err' : ''}`,
                  role: 'listitem',
                  onClick: () => setSelectedKey(key),
                },
                h('span', { className: `nm-task-step-icon${step.ok === false ? ' nm-err' : ''}` }, stepIcon(step)),
                h('span', { className: 'nm-task-work-main' },
                  h('span', { className: 'nm-task-work-title' }, stepTitle(step)),
                  h('span', { className: 'nm-task-work-sub' }, step.kind === 'tool' ? `${toolName(step)} · ${stepStatus(step)}` : stepStatus(step))),
                h('span', { className: 'nm-task-step-time' }, clockLabel(step.at, rooms.lang)),
                h(IconChevronRight, { size: 16 }))))
              : h('div', { className: 'nm-task-empty' }, t('taskNoSteps')))
        : selectedKey && selected
          ? h('div', { className: 'nm-task-scroll nm-task-detail' },
              selected.kind === 'answer'
                ? h('section', { className: 'nm-task-block' },
                    h('div', { className: 'nm-task-label' }, t('taskNoTool')),
                    h('div', { className: 'nm-task-muted' }, t('taskAnalysisNoTool')))
                : h('div', null,
                    h('section', { className: 'nm-task-block' },
                      h('div', { className: 'nm-task-label' }, t('taskTool')),
                      h('pre', { className: 'nm-task-pre' }, toolName(selected))),
                    selected.detail ? h('section', { className: 'nm-task-block' }, h('div', { className: 'nm-task-label' }, t('taskArguments')), h('pre', { className: 'nm-task-pre' }, safeStepText(selected.detail, rooms.lang))) : null,
                    h('section', { className: 'nm-task-block' },
                      h('div', { className: 'nm-task-label' }, t('taskReturned')),
                      selected.result
                        ? h('pre', { className: 'nm-task-pre' }, safeStepText(selected.result, rooms.lang))
                        : h('div', { className: 'nm-task-muted' }, selected.ok === undefined && running ? t('taskPending') : t('taskNothing')))))
          : selectedKey
            ? h('div', { className: 'nm-task-empty' }, t('taskNoSteps'))
            : null))
}

function shortText(value: string, max = 64): string {
  const text = value.replace(/\s+/g, ' ').trim()
  return text.length > max ? `${text.slice(0, max - 1)}…` : text
}

const SENSITIVE_KEY = /(?:password|passwd|passphrase|secret|token|api[\s_-]*key|authorization|credential|private[\s_-]*key|access[\s_-]*key|refresh[\s_-]*token)/i

function redactJson(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(redactJson)
  if (value && typeof value === 'object') {
    return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, SENSITIVE_KEY.test(key) ? '[redacted]' : redactJson(item)]))
  }
  return value
}

function safeStepText(value: string, lang: string, max = 4000): string {
  let text = value
  try {
    const redacted = JSON.stringify(redactJson(JSON.parse(value)), null, 2)
    if (redacted !== undefined) text = redacted
  } catch {
    text = value.replace(
      /(["']?[\w.-]*(?:password|passwd|passphrase|secret|token|api[\s_-]*key|authorization|credential|private[\s_-]*key|access[\s_-]*key|refresh[\s_-]*token)[\w.-]*["']?\s*[:=]\s*)(?:Bearer\s+[^\s,;]+|Basic\s+[^\s,;]+|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s,;]+)/gi,
      '$1[redacted]',
    )
  }
  text = text
    .replace(/\bBearer\s+[^\s,;"']+/gi, 'Bearer [redacted]')
    .replace(/\b(?:sk|pk|rk|ak|dsk)[-_][A-Za-z0-9_-]{12,}\b/gi, '[redacted API key]')
    .replace(/\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[baprs]-[A-Za-z0-9-]{16,}|AIza[0-9A-Za-z_-]{30,})\b/g, '[redacted API key]')
  const chars = Array.from(text)
  if (chars.length <= max) return text
  const marker = /^zh/i.test(lang) ? '…（内容已截断）' : '… [truncated]'
  return `${chars.slice(0, max).join('')}\n\n${marker}`
}
