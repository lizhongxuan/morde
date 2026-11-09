/**
 * The model picker of Settings → Models and Settings → Media (the Models contract, same on
 * the phones): a button that reads `<provider> · <model>` and opens a panel under it with the
 * fixed rows first (*Automatic*, *Off*), a search field once the lists are long, then one
 * group per provider, each folded to a few rows with *Show {n} more* at its foot
 * (`model-list.ts` decides what is shown). A native `<select>` can neither fold nor filter,
 * and OpenRouter or SiliconFlow list hundreds of models. Esc, a click outside and a pick
 * close it; the rows are buttons, so Tab reaches them, and the arrows walk them.
 */
import { createElement as h, useCallback, useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import type { Translate } from './api.ts'
import { IconCheck, IconChevronDown, IconSearch } from './icons.tsx'
import { hasSearch, layoutGroups, type Current, type ModelGroup, type ModelRow } from './model-list.ts'

/** A row before the groups: *Automatic*, *Off*. */
export interface PickerHead {
  value: string
  label: string
}

export interface ModelPickerProps<R extends ModelRow = ModelRow> {
  t: Translate
  /** The row's title, for the button's accessible name. */
  label: string
  /** What the button reads: `<provider> · <model>`, or the head row's label. */
  text: string
  heads?: PickerHead[]
  /** The selected head row's value, when the picker stands on one. */
  headValue?: string
  groups: readonly ModelGroup<R>[]
  /** The chosen model, when the picker stands on one. */
  current?: Current
  disabled?: boolean
  testId?: string
  onHead?(value: string): void
  onPick(group: string, row: R): void
  /** How a row is written; the row's name by default. */
  rowText?(row: R): ReactNode
}

const PANEL_WIDTH = 340
const PANEL_MAX_HEIGHT = 380

function place(anchor: HTMLElement): Record<string, number> {
  const rect = anchor.getBoundingClientRect()
  const below = rect.bottom + PANEL_MAX_HEIGHT + 12 < window.innerHeight || rect.top < window.innerHeight / 2
  const width = Math.min(PANEL_WIDTH, window.innerWidth - 16)
  const left = Math.max(8, Math.min(rect.right - width, window.innerWidth - width - 8))
  return {
    left,
    width,
    ...(below ? { top: rect.bottom + 4, maxHeight: Math.min(PANEL_MAX_HEIGHT, window.innerHeight - rect.bottom - 12) } : { bottom: window.innerHeight - rect.top + 4, maxHeight: Math.min(PANEL_MAX_HEIGHT, rect.top - 12) }),
  }
}

export function ModelPicker<R extends ModelRow>({ t, label, text, heads = [], headValue, groups, current, disabled, testId, onHead, onPick, rowText }: ModelPickerProps<R>): ReactNode {
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const [expanded, setExpanded] = useState<Set<string>>(() => new Set())
  const [style, setStyle] = useState<Record<string, number>>({})
  const button = useRef<HTMLButtonElement>(null)
  const panel = useRef<HTMLDivElement>(null)
  const searchable = hasSearch(groups)

  const close = useCallback((refocus = false) => {
    setOpen(false)
    setQuery('')
    setExpanded(new Set())
    if (refocus) button.current?.focus()
  }, [])

  // where the panel sits: under the button, over it when the screen ends; again on scroll and resize
  useLayoutEffect(() => {
    if (!open || !button.current) return
    const update = () => { if (button.current) setStyle(place(button.current)) }
    update()
    window.addEventListener('resize', update)
    document.addEventListener('scroll', update, true)
    return () => {
      window.removeEventListener('resize', update)
      document.removeEventListener('scroll', update, true)
    }
  }, [open])

  // Esc and a click outside close it
  useEffect(() => {
    if (!open) return
    const onPointer = (event: PointerEvent) => {
      const target = event.target as Node
      if (panel.current?.contains(target) || button.current?.contains(target)) return
      close()
    }
    const onKey = (event: globalThis.KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.stopPropagation()
        close(true)
      }
    }
    document.addEventListener('pointerdown', onPointer, true)
    document.addEventListener('keydown', onKey, true)
    return () => {
      document.removeEventListener('pointerdown', onPointer, true)
      document.removeEventListener('keydown', onKey, true)
    }
  }, [open, close])

  // focus lands on the search field when there is one, else on the selected row, else the first
  useEffect(() => {
    if (!open || !panel.current) return
    const first = panel.current.querySelector<HTMLElement>('input') ?? panel.current.querySelector<HTMLElement>('[aria-selected="true"]') ?? panel.current.querySelector<HTMLElement>('[data-nm-mp-row]')
    first?.focus()
  }, [open])

  const walk = (event: { key: string; preventDefault(): void; target: EventTarget | null }) => {
    if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp') return
    const rows = [...(panel.current?.querySelectorAll<HTMLElement>('[data-nm-mp-row]') ?? [])]
    if (!rows.length) return
    event.preventDefault()
    const at = rows.indexOf(event.target as HTMLElement)
    const next = event.key === 'ArrowDown' ? (at < 0 ? 0 : Math.min(rows.length - 1, at + 1)) : (at < 0 ? rows.length - 1 : Math.max(0, at - 1))
    rows[next]?.focus()
  }

  const shown = layoutGroups(groups, { ...(current ? { current } : {}), query, expanded })
  const searching = query.trim().length > 0

  const row = (key: string, selected: boolean, content: ReactNode, onClick: () => void, extra: Record<string, unknown> = {}) =>
    h('button', { key, type: 'button', role: 'option', 'aria-selected': selected, 'data-nm-mp-row': '', className: `nm-mp-row${selected ? ' nm-selected' : ''}`, onClick, onKeyDown: walk, ...extra },
      h('span', { className: 'nm-mp-row-text' }, content),
      selected ? h(IconCheck, { size: 15 }) : null)

  return h('div', { className: 'nm-mp' },
    h('button', {
      ref: button,
      type: 'button',
      className: 'nm-field nm-select nm-mp-btn',
      disabled,
      'aria-label': label,
      'aria-haspopup': 'listbox',
      'aria-expanded': open,
      'data-testid': testId,
      title: text,
      onClick: () => (open ? close() : setOpen(true)),
    }, h('span', { className: 'nm-mp-btn-text' }, text), h(IconChevronDown, { size: 16 })),
    open
      ? h('div', { ref: panel, className: 'nm-mp-panel', role: 'listbox', 'aria-label': label, style, 'data-testid': testId ? `${testId}-panel` : undefined },
          heads.length ? h('div', { className: 'nm-mp-heads' }, heads.map((head) => row(`head:${head.value}`, headValue === head.value, head.label, () => { onHead?.(head.value); close(true) }))) : null,
          searchable
            ? h('label', { className: 'nm-mp-search' },
                h(IconSearch, { size: 15 }),
                h('input', { type: 'search', value: query, placeholder: t('mpSearch'), 'aria-label': t('mpSearch'), autoComplete: 'off', spellCheck: false, onChange: (e: { currentTarget: HTMLInputElement }) => setQuery(e.currentTarget.value), onKeyDown: walk }))
            : null,
          h('div', { className: 'nm-mp-list' },
            shown.length === 0 && searching ? h('div', { className: 'nm-mp-none' }, t('mpNoMatch')) : null,
            shown.map((group) =>
              h('div', { key: group.key, className: 'nm-mp-group', role: 'group', 'aria-label': group.label },
                h('div', { className: 'nm-mp-group-label' }, group.label),
                group.rows.map((r) => row(`${group.key}:${r.id}`, current?.group === group.key && current.id === r.id, rowText ? rowText(r) : r.name, () => { onPick(group.key, r); close(true) }, { title: r.id })),
                group.more > 0
                  ? h('button', { type: 'button', className: 'nm-mp-more', 'data-nm-mp-row': '', onKeyDown: walk, onClick: () => setExpanded((prev) => new Set([...prev, group.key])) }, t('mpShowMore', { n: group.more }))
                  : null))))
      : null)
}
