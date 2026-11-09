/**
 * Memory — what the agent remembers about the person, one line each, kept on this
 * computer (`rooms.json`) and read into every chat's prompt. The drawer's Memory tab
 * lists it; Muse's "import memory" sheet pastes a text from another assistant in,
 * one line per memory; the Data controls page downloads everything as a zip or
 * resets the agent.
 */
import { failureText } from './api.ts'
import { createElement as h, Fragment, useState, type FormEvent, type ReactNode } from 'react'
import type { Translate } from './api.ts'
import { IconBrain, IconClose } from './icons.tsx'
import { roomsCall, useRooms, type MemoryItem } from './rooms.ts'
import { useLive } from './live.ts'
import { ago, Sheet } from './ui.tsx'

/** The memory list with an add field and the import button — the drawer's Memory tab. */
export function MemoryList({ t, onImport }: { t: Translate; onImport(): void }): ReactNode {
  const rooms = useRooms()
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const items = [...rooms.memory].sort((a, b) => b.at - a.at)
  const add = (event: FormEvent) => {
    event.preventDefault()
    const line = text.trim()
    if (!line) return
    setBusy(true)
    roomsCall('memory/add', { text: line }).then(() => setText('')).catch(() => undefined).finally(() => setBusy(false))
  }
  return h(Fragment, null,
    h('form', { className: 'nm-mem-add', onSubmit: add },
      h('input', { className: 'nm-mem-input', value: text, placeholder: t('memAddPlaceholder'), maxLength: 400, onChange: (e: FormEvent<HTMLInputElement>) => setText(e.currentTarget.value) }),
      h('button', { type: 'submit', className: 'nm-pill nm-pill-sm', disabled: busy || !text.trim() }, t('memAdd'))),
    items.length
      ? h('div', { className: 'nm-pf-rows' }, items.map((item) => h(MemoryRow, { key: item.id, t, item })))
      : h('p', { className: 'nm-pf-empty' }, t('memEmpty')),
    h('div', { style: { display: 'flex', gap: 8, flexWrap: 'wrap' } },
      h('button', { type: 'button', className: 'nm-pill nm-pill-ghost nm-pill-sm', onClick: onImport }, t('memImport'))))
}

function MemoryRow({ t, item }: { t: Translate; item: MemoryItem }): ReactNode {
  return h('div', { className: 'nm-pf-row' },
    h('span', { className: 'nm-pf-row-icon' }, h(IconBrain, { size: 18 })),
    h('div', { className: 'nm-pf-row-main' },
      h('div', { className: 'nm-pf-row-title nm-wrap' }, item.text),
      h('div', { className: 'nm-pf-row-sub' }, `${t(item.source === 'agent' ? 'memFromAgent' : item.source === 'import' ? 'memFromImport' : 'memFromYou')} · ${ago(t, item.at)}`)),
    h('button', { type: 'button', className: 'nm-icon-btn nm-mem-forget', 'aria-label': t('memForget'), title: t('memForget'), onClick: () => { void roomsCall('memory/delete', { id: item.id }).catch(() => undefined) } }, h(IconClose, { size: 14 })))
}

/** Muse's "import memory": paste what another assistant knew; each line becomes a memory. */
export function ImportMemorySheet({ t, onClose }: { t: Translate; onClose(): void }): ReactNode {
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [done, setDone] = useState<number | undefined>()
  const [error, setError] = useState<string | undefined>()
  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (!text.trim()) return
    setBusy(true)
    setError(undefined)
    roomsCall<{ added: number }>('memory/import', { text }).then((r) => setDone(r.added)).catch((err: unknown) => setError(failureText(t, err))).finally(() => setBusy(false))
  }
  return h(Sheet, { title: t('memImport'), closeLabel: t('close'), onClose,
    footer: h('div', { className: 'nm-sheet-actions' },
      h('span', { style: { flex: 1 } }),
      h('button', { type: 'button', className: 'nm-pill nm-pill-ghost nm-pill-sm', onClick: onClose }, done === undefined ? t('cancel') : t('close')),
      done === undefined ? h('button', { type: 'submit', form: 'nm-mem-import', className: 'nm-pill nm-pill-sm', disabled: busy || !text.trim() }, busy ? t('memImporting') : t('memImportDo')) : null) },
    h('form', { id: 'nm-mem-import', className: 'nm-sheet-form', onSubmit: submit },
      h('p', { className: 'nm-sheet-lead' }, t('memImportLead')),
      h('ol', { className: 'nm-sheet-steps' },
        h('li', null, t('memImportStep1')),
        h('li', null, t('memImportStep2')),
        h('li', null, t('memImportStep3'))),
      done === undefined
        ? h('textarea', { className: 'nm-textarea', rows: 8, value: text, placeholder: t('memImportPlaceholder'), maxLength: 60000, onChange: (e: FormEvent<HTMLTextAreaElement>) => setText(e.currentTarget.value), 'data-modal-autofocus': true })
        : h('p', { className: 'nm-sheet-lead' }, t('memImported', { n: done })),
      error ? h('div', { className: 'nm-room-error' }, error) : null))
}

/** Muse's Data controls, the local part: import memory, download the agent data, reset. */
export function DataRows({ t }: { t: Translate }): ReactNode {
  const live = useLive()
  const [sheet, setSheet] = useState(false)
  const [notice, setNotice] = useState<string | undefined>()
  const [confirm, setConfirm] = useState(false)
  const [busy, setBusy] = useState(false)
  const download = () => {
    setBusy(true)
    setNotice(undefined)
    roomsCall<{ path: string }>('data/export', {}).then((r) => setNotice(t('dataExported', { path: r.path }))).catch((err: unknown) => setNotice(failureText(t, err))).finally(() => setBusy(false))
  }
  const reset = () => {
    setBusy(true)
    roomsCall('data/reset', {}).then(() => window.location.reload()).catch((err: unknown) => { setNotice(failureText(t, err)); setBusy(false) })
  }
  return h(Fragment, null,
    h('div', { className: 'nm-card' },
      h('div', { className: 'nm-row' },
        h('div', { className: 'nm-row-main' },
          h('span', { className: 'nm-row-title' }, t('memImport')),
          h('span', { className: 'nm-row-sub nm-wrap' }, t('dataImportSub'))),
        h('button', { type: 'button', className: 'nm-pill nm-pill-ghost nm-pill-sm', onClick: () => setSheet(true) }, t('dataImportDo'))),
      h('div', { className: 'nm-row' },
        h('div', { className: 'nm-row-main' },
          h('span', { className: 'nm-row-title' }, t('dataDownload')),
          h('span', { className: 'nm-row-sub nm-wrap' }, t('dataDownloadSub'))),
        h('button', { type: 'button', className: 'nm-pill nm-pill-ghost nm-pill-sm', disabled: busy, onClick: download }, t('dataDownloadDo'))),
      h('div', { className: 'nm-row' },
        h('div', { className: 'nm-row-main' },
          h('span', { className: 'nm-row-title nm-danger' }, t('dataReset')),
          h('span', { className: 'nm-row-sub nm-wrap' }, t('dataResetSub'))),
        h('button', { type: 'button', className: 'nm-pill nm-pill-ghost nm-pill-sm nm-danger', disabled: busy, onClick: () => setConfirm(true) }, t('dataResetDo'))),
      notice ? h('div', { className: 'nm-row' }, h('span', { className: 'nm-row-sub nm-wrap' }, notice)) : null),
    live.mordeHub?.signedIn ? h(LegacyRoomsImport, { t }) : null,
    live.mordeHub?.signedIn ? h(LegacySchedulesImport, { t }) : null,
    sheet ? h(ImportMemorySheet, { t, onClose: () => setSheet(false) }) : null,
    confirm
      ? h(Sheet, { title: t('dataReset'), closeLabel: t('close'), onClose: () => setConfirm(false),
          footer: h('div', { className: 'nm-sheet-actions' },
            h('span', { style: { flex: 1 } }),
            h('button', { type: 'button', className: 'nm-pill nm-pill-ghost nm-pill-sm', onClick: () => setConfirm(false) }, t('cancel')),
            h('button', { type: 'button', className: 'nm-pill nm-pill-sm nm-pill-danger', disabled: busy, onClick: reset }, busy ? t('dataResetting') : t('dataResetDo'))) },
          h('div', { className: 'nm-sheet-form' },
            h('p', { className: 'nm-sheet-lead' }, t('dataResetConfirm'))))
      : null)
}

interface LegacyRoomsPreview {
  memories: { id: string; text: string; at: number; imported: boolean }[]
  goals: { id: string; at: number; title: string; description: string; status: 'tracking' | 'done' | 'paused'; category: string; steps: string[]; imported: boolean }[]
  retained: { feed_posts: number; library_items: number; routine: { on: boolean; time: string; ensuredAt: number } }
}

function LegacyRoomsImport({ t }: { t: Translate }): ReactNode {
  const [preview, setPreview] = useState<LegacyRoomsPreview | undefined>()
  const [memoryIDs, setMemoryIDs] = useState<string[]>([])
  const [goalIDs, setGoalIDs] = useState<string[]>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [result, setResult] = useState<{ imported_memories: number; imported_goals: number; memory_sync_pending: boolean; goal_sync_pending: number; sync_error?: string }>()
  const toggle = (ids: string[], id: string, set: (next: string[]) => void) => set(ids.includes(id) ? ids.filter((value) => value !== id) : [...ids, id])
  const load = async () => {
    setBusy(true)
    setError('')
    setResult(undefined)
    try {
      setPreview(await roomsCall<LegacyRoomsPreview>('morde/legacy-preview'))
      setMemoryIDs([])
      setGoalIDs([])
    } catch (err) {
      setError(failureText(t, err))
    } finally {
      setBusy(false)
    }
  }
  const submit = async () => {
    setBusy(true)
    setError('')
    try {
      const imported = await roomsCall<typeof result>('morde/legacy-import', { memory_ids: memoryIDs, goal_ids: goalIDs })
      if (imported) setResult(imported)
      setMemoryIDs([])
      setGoalIDs([])
      setPreview(await roomsCall<LegacyRoomsPreview>('morde/legacy-preview'))
    } catch (err) {
      setError(failureText(t, err))
    } finally {
      setBusy(false)
    }
  }
  return h('section', { className: 'nm-card', style: { marginTop: 12, padding: 14 } },
    h('h3', { style: { margin: '0 0 6px' } }, t('legacyRoomsTitle')),
    h('p', { className: 'nm-row-sub nm-wrap' }, t('legacyRoomsDescription')),
    h('button', { type: 'button', className: 'nm-pill nm-pill-ghost nm-pill-sm', disabled: busy, onClick: load }, busy ? t('legacyRoomsLoading') : t('legacyRoomsPreview')),
    error ? h('p', { className: 'nm-room-error', role: 'alert' }, error) : null,
    preview ? h(Fragment, null,
      preview.memories.length ? h('fieldset', { className: 'nm-fieldset', style: { marginTop: 10 } },
        h('legend', null, `${t('legacyRoomsMemories')} (${preview.memories.length})`),
        preview.memories.map((item, index) => h('label', { key: `${item.id}-${index}`, className: 'nm-checkbox-row', style: { alignItems: 'flex-start', marginBottom: 6 } },
          h('input', { type: 'checkbox', checked: item.imported || memoryIDs.includes(item.id), disabled: busy || item.imported, onChange: () => toggle(memoryIDs, item.id, setMemoryIDs) }),
          h('span', { className: 'nm-wrap' }, item.imported ? `${item.text} · ${t('legacyRoomsImportedMark')}` : item.text)))) : null,
      preview.goals.length ? h('fieldset', { className: 'nm-fieldset', style: { marginTop: 10 } },
        h('legend', null, `${t('legacyRoomsGoals')} (${preview.goals.length})`),
        preview.goals.map((item, index) => h('label', { key: `${item.id}-${index}`, className: 'nm-checkbox-row', style: { alignItems: 'flex-start', marginBottom: 6 } },
          h('input', { type: 'checkbox', checked: item.imported || goalIDs.includes(item.id), disabled: busy || item.imported, onChange: () => toggle(goalIDs, item.id, setGoalIDs) }),
          h('span', { className: 'nm-wrap' }, h('strong', null, item.title), item.description ? ` · ${item.description}` : '', item.imported ? ` · ${t('legacyRoomsImportedMark')}` : '')))) : null,
      !preview.memories.length && !preview.goals.length ? h('p', { className: 'nm-row-sub' }, t('legacyRoomsEmpty')) : null,
      h('p', { className: 'nm-row-sub nm-wrap', style: { marginTop: 10 } }, t('legacyRoomsRetained', { feed: preview.retained.feed_posts, library: preview.retained.library_items, routine: preview.retained.routine.on ? `${t('legacyRoomsRoutineOn')} ${preview.retained.routine.time}` : t('legacyRoomsRoutineOff') })),
      h('button', { type: 'button', className: 'nm-pill nm-pill-sm', disabled: busy || (!memoryIDs.length && !goalIDs.length), onClick: () => void submit() }, busy ? t('legacyRoomsImporting') : t('legacyRoomsImport')),
    ) : null,
    result ? h('p', { className: result.memory_sync_pending || result.goal_sync_pending || result.sync_error ? 'nm-room-error' : 'nm-row-sub', role: 'status' },
      result.sync_error ? t('legacyRoomsSyncError', { detail: result.sync_error }) : result.memory_sync_pending || result.goal_sync_pending ? t('legacyRoomsSyncPending', { memories: result.imported_memories, goals: result.imported_goals }) : t('legacyRoomsImportedResult', { memories: result.imported_memories, goals: result.imported_goals })) : null)
}

interface LegacySchedulesPreview {
  available: boolean
  schedules: {
    source_id: string; fingerprint: string; source_session_id: string; title: string; title_is_derived: boolean; objective: string
    kind: string; source_scheduled_at: string; source_every_seconds?: number; timing: Record<string, unknown> | null; time_zone: string
    unsupported_fields: string[]; unavailable_reason?: string; imported_schedule_id?: string; imported: boolean
  }[]
  models: { id: string; name: string; model_name: string }[]
  model_error?: string
}

interface LegacySchedulesExport {
  format: 'morde-nanomuse-dsh-schedules'
  version: 1
  exported_at: string
  schedules: (LegacySchedulesPreview['schedules'][number] & {
    migration?: {
      request: {
        title?: string; model_id: string; objective: string; host_id: null; timing: Record<string, unknown>
        budget: { active_seconds: number; tool_calls: number; model_tokens: number }; misfire_policy: 'skip'
        notification_events: ['completed', 'failed', 'waiting_user']; observation: null
      }
      idempotency_key: string
    }
  })[]
}

function LegacySchedulesImport({ t }: { t: Translate }): ReactNode {
  const [preview, setPreview] = useState<LegacySchedulesPreview>()
  const [modelID, setModelID] = useState('')
  const [budget, setBudget] = useState({ active_seconds: '', tool_calls: '', model_tokens: '' })
  const [stopped, setStopped] = useState<Record<string, boolean>>({})
  const [loading, setLoading] = useState(false)
  const [busyID, setBusyID] = useState('')
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [nextRunAt, setNextRunAt] = useState<string | null>()
  const valid = (raw: string, max: number) => /^\d+$/.test(raw) && Number(raw) >= 1 && Number(raw) <= max
  const budgetReady = valid(budget.active_seconds, 1800) && valid(budget.tool_calls, 100) && valid(budget.model_tokens, 50000)
  const budgetFields: [keyof typeof budget, number, Parameters<Translate>[0]][] = [
    ['active_seconds', 1800, 'legacySchedulesBudgetActive'],
    ['tool_calls', 100, 'legacySchedulesBudgetTools'],
    ['model_tokens', 50000, 'legacySchedulesBudgetTokens'],
  ]
  const load = async () => {
    setLoading(true)
    setError('')
    setNotice('')
    setNextRunAt(undefined)
    try {
      setPreview(await roomsCall<LegacySchedulesPreview>('morde/schedules-preview'))
      setStopped({})
    } catch (err) {
      setError(failureText(t, err))
    } finally {
      setLoading(false)
    }
  }
  const importSchedule = async (item: LegacySchedulesPreview['schedules'][number]) => {
    if (!preview || !stopped[item.source_id] || !modelID || !budgetReady) return
    setBusyID(item.source_id)
    setError('')
    setNotice('')
    try {
      const result = await roomsCall<{ already_imported: boolean; schedule_id?: string; schedule?: { id: string; title: string; next_run_at: string | null } }>('morde/schedule-import', {
        source_id: item.source_id, fingerprint: item.fingerprint, model_id: modelID, time_zone: item.time_zone,
        budget: { active_seconds: Number(budget.active_seconds), tool_calls: Number(budget.tool_calls), model_tokens: Number(budget.model_tokens) },
        original_disabled: true,
      })
      setNotice(t(result.already_imported ? 'legacySchedulesAlreadyImported' : 'legacySchedulesImported', {
        id: result.schedule?.id ?? result.schedule_id ?? '', title: result.schedule?.title ?? item.title,
      }))
      setNextRunAt(result.already_imported ? undefined : result.schedule?.next_run_at ?? null)
      setStopped((value) => ({ ...value, [item.source_id]: false }))
      setPreview(await roomsCall<LegacySchedulesPreview>('morde/schedules-preview'))
    } catch (err) {
      setError(failureText(t, err))
    } finally {
      setBusyID('')
    }
  }
  const exportSchedules = async () => {
    if (!preview || !modelID || !budgetReady) return
    setBusyID('export')
    setError('')
    setNotice('')
    try {
      const bundle = await roomsCall<LegacySchedulesExport>('morde/schedules-export', {
        model_id: modelID,
        budget: { active_seconds: Number(budget.active_seconds), tool_calls: Number(budget.tool_calls), model_tokens: Number(budget.model_tokens) },
      })
      const blobURL = URL.createObjectURL(new Blob([JSON.stringify(bundle, null, 2)], { type: 'application/json' }))
      const link = document.createElement('a')
      link.href = blobURL
      link.download = `morde-dsh-schedules-${bundle.exported_at.slice(0, 10)}.json`
      document.body.append(link)
      link.click()
      link.remove()
      window.setTimeout(() => URL.revokeObjectURL(blobURL), 0)
      setNotice(t('legacySchedulesExported'))
    } catch (err) {
      setError(failureText(t, err))
    } finally {
      setBusyID('')
    }
  }
  return h('section', { className: 'nm-card', style: { marginTop: 12, padding: 14 } },
    h('h3', { style: { margin: '0 0 6px' } }, t('legacySchedulesTitle')),
    h('p', { className: 'nm-row-sub nm-wrap' }, t('legacySchedulesDescription')),
    h('button', { type: 'button', className: 'nm-pill nm-pill-ghost nm-pill-sm', disabled: loading || Boolean(busyID), onClick: () => void load() }, loading ? t('legacySchedulesLoading') : t('legacySchedulesPreview')),
    error ? h('p', { className: 'nm-room-error', role: 'alert' }, error) : null,
    notice ? h('p', { className: 'nm-row-sub nm-wrap', role: 'status' }, notice) : null,
    nextRunAt !== undefined ? h('p', { className: 'nm-row-sub nm-wrap', role: 'status' }, nextRunAt
      ? t('legacySchedulesNextRun', { time: new Date(nextRunAt).toLocaleString() })
      : t('legacySchedulesNoNextRun')) : null,
    preview ? h(Fragment, null,
      !preview.available ? h('p', { className: 'nm-row-sub nm-wrap' }, t('legacySchedulesUnavailable')) : null,
      preview.model_error ? h('p', { className: 'nm-room-error', role: 'alert' }, t('legacySchedulesModelError', { detail: preview.model_error })) : null,
      preview.available && preview.schedules.length ? h('div', { className: 'nm-sheet-form', style: { marginTop: 12 } },
        h('label', { className: 'nm-row-sub' }, t('legacySchedulesModel'),
          h('select', { className: 'nm-field nm-select', disabled: !preview.models.length, value: modelID, onChange: (event: FormEvent<HTMLSelectElement>) => setModelID(event.currentTarget.value) },
            h('option', { value: '' }, t('legacySchedulesChooseModel')),
            preview.models.map((model) => h('option', { key: model.id, value: model.id }, `${model.name} (${model.model_name})`)))),
        !preview.models.length ? h('p', { className: 'nm-row-sub nm-wrap' }, t('legacySchedulesNoModels')) : null,
        h('div', { style: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(150px, 1fr))', gap: 8, marginTop: 10 } },
          ...budgetFields.map(([key, max, label]) => h('label', { key, className: 'nm-row-sub' },
            t(label),
            h('input', {
              className: 'nm-field', type: 'number', min: 1, max, step: 1, value: budget[key],
              onChange: (event: FormEvent<HTMLInputElement>) => setBudget((value) => ({ ...value, [key]: event.currentTarget.value })),
            }),
          )),
        ),
      ) : null,
      preview.available && !preview.schedules.length ? h('p', { className: 'nm-row-sub' }, t('legacySchedulesEmpty')) : null,
      preview.schedules.map((item) => {
        const anchorAt = item.timing?.kind === 'every_n_hours' && typeof item.timing.anchor_at === 'string' ? item.timing.anchor_at : ''
        return h('article', { key: item.source_id, className: 'nm-card', style: { marginTop: 10, padding: 12 } },
          h('strong', { className: 'nm-wrap' }, item.title),
          item.title_is_derived ? h('p', { className: 'nm-row-sub nm-wrap' }, t('legacySchedulesDerivedTitle')) : null,
          h('p', { className: 'nm-row-sub nm-wrap' }, `${t('legacySchedulesSession')}: ${item.source_session_id} · ${item.kind} · ${item.source_scheduled_at}${item.source_every_seconds !== undefined ? ` · ${t('legacySchedulesEverySeconds', { seconds: item.source_every_seconds })}` : ''}`),
          anchorAt ? h('p', { className: 'nm-row-sub nm-wrap' }, t('legacySchedulesAnchor', { time: anchorAt })) : null,
          h('pre', { className: 'nm-wrap', style: { whiteSpace: 'pre-wrap', overflowWrap: 'anywhere', fontSize: 12 } }, JSON.stringify({
            title: item.title_is_derived ? undefined : item.title, objective: item.objective, model_id: modelID || undefined, host_id: null,
            timing: item.timing, time_zone: item.time_zone,
            budget: { active_seconds: budget.active_seconds || null, tool_calls: budget.tool_calls || null, model_tokens: budget.model_tokens || null },
            misfire_policy: 'skip', notification_events: ['completed', 'failed', 'waiting_user'], observation: null,
          }, null, 2)),
          item.unavailable_reason ? h('p', { className: 'nm-room-error nm-wrap', role: 'status' }, item.unavailable_reason) : null,
          item.imported ? h('p', { className: 'nm-row-sub', role: 'status' }, t('legacySchedulesImportedMark', { id: item.imported_schedule_id ?? '' })) : null,
          !item.imported && !item.unavailable_reason ? h(Fragment, null,
            h('label', { className: 'nm-checkbox-row', style: { alignItems: 'flex-start' } },
              h('input', { type: 'checkbox', checked: stopped[item.source_id] === true, disabled: Boolean(busyID), onChange: () => setStopped((value) => ({ ...value, [item.source_id]: !value[item.source_id] })) }),
              h('span', { className: 'nm-wrap' }, t('legacySchedulesStopConfirm'))),
            h('button', { type: 'button', className: 'nm-pill nm-pill-sm', disabled: Boolean(busyID) || !stopped[item.source_id] || !modelID || !budgetReady || !item.timing, onClick: () => void importSchedule(item) }, busyID === item.source_id ? t('legacySchedulesImporting') : t('legacySchedulesImport'))) : null)
      }),
      preview.schedules.some((item) => !item.imported && !item.unavailable_reason && item.timing)
        ? h(Fragment, null,
          h('p', { className: 'nm-row-sub nm-wrap', style: { marginTop: 12 } }, t('legacySchedulesExportDescription')),
          h('button', { type: 'button', className: 'nm-pill nm-pill-ghost nm-pill-sm', disabled: Boolean(busyID) || !modelID || !budgetReady, onClick: () => void exportSchedules() }, busyID === 'export' ? t('legacySchedulesExporting') : t('legacySchedulesExport')))
        : null
    ) : null)
}
