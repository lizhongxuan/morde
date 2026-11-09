/**
 * The name chooser of the first conversation (the phone's `NamingCard`): a grey card
 * titled "Give me a name", the suggested names as full-width rows, and a dashed
 * "Something else…" row that opens an input on the card — a name of the person's own is
 * saved by the pick, the way the phones already do it, instead of waiting on the model to
 * repeat it in a ```nanomuse-naming block. After a pick the chosen row keeps a tick, the
 * others dim, and the card stops taking clicks. Mounted by `FenceCards.ts` beside the
 * agent's ```nanomuse-naming block (hidden); the names and the pick come from the host's
 * first-run state, so a reopened chat shows the same card.
 */
import { createElement as h, useState, type ReactNode } from 'react'
import type { Translate } from './api.ts'
import { IconCheckCircle } from './icons.tsx'
import { roomsCall, useRooms } from './rooms.ts'

export function NamingCard({ t, suggested }: { t: Translate; suggested: string[] }): ReactNode {
  const { firstRun } = useRooms()
  const [sending, setSending] = useState<string | null>(null)
  const [editing, setEditing] = useState(false)
  const [value, setValue] = useState('')
  const chosen = firstRun.chosen ?? sending
  const base = firstRun.chips.length ? firstRun.chips : suggested
  const options = chosen !== null && !base.includes(chosen) ? [...base, chosen] : base
  const pick = (name: string) => {
    if (chosen !== null) return
    setSending(name)
    roomsCall('firstrun/pick', { name }).catch(() => setSending(null))
  }
  const typed = value.trim()
  return h('div', { className: 'nm-naming', role: 'group', 'aria-label': t('frNamingTitle') },
    h('div', { className: 'nm-naming-title' }, t('frNamingTitle')),
    options.map((name) =>
      h('button', {
        key: name,
        type: 'button',
        className: `nm-naming-row${chosen === name ? ' nm-selected' : ''}${chosen !== null && chosen !== name ? ' nm-dimmed' : ''}`,
        disabled: chosen !== null,
        'aria-pressed': chosen === name,
        onClick: () => pick(name),
      },
        h('span', { className: 'nm-naming-name' }, name),
        chosen === name ? h(IconCheckCircle, { size: 18 }) : null)),
    chosen === null && editing
      ? h('form', {
          className: 'nm-naming-row nm-naming-custom',
          onSubmit: (e: { preventDefault(): void }) => {
            e.preventDefault()
            if (typed) pick(typed)
          },
        },
          h('input', {
            autoFocus: true,
            maxLength: 16,
            value,
            placeholder: t('frNamingPlaceholder'),
            onChange: (e: { currentTarget: { value: string } }) => setValue(e.currentTarget.value),
            onKeyDown: (e: { key: string }) => {
              if (e.key === 'Escape') {
                setEditing(false)
                setValue('')
              }
            },
          }),
          h('button', { type: 'submit', className: 'nm-naming-confirm', disabled: !typed }, t('frNamingConfirm')))
      : chosen === null
        ? h('button', { type: 'button', className: 'nm-naming-row nm-naming-custom', onClick: () => setEditing(true) }, t('frNamingCustom'))
        : null)
}
