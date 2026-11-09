import { useSyncExternalStore } from 'react'
import type { Translate } from './api.ts'
import { bridge, type WindowsUpdateState } from './bridge.ts'

let state: WindowsUpdateState | null | undefined
let started = false
const listeners = new Set<() => void>()

/** The menu and settings subscribe to the same shell state, once per window. */
export function useWindowsUpdates(): WindowsUpdateState | null | undefined {
  const updates = bridge()?.platform === 'win32' ? bridge()?.updates : undefined
  return useSyncExternalStore((listener) => {
    listeners.add(listener)
    if (!started) {
      started = true
      let received = false
      const publish = (next: WindowsUpdateState | null) => { state = next; for (const notify of listeners) notify() }
      if (updates) {
        updates.subscribe((next) => { received = true; publish(next) })
        void updates.snapshot().then((next) => { if (!received) publish(next) }).catch(() => publish(null))
      } else publish(null)
    }
    return () => { listeners.delete(listener) }
  }, () => updates ? state : null, () => null)
}

export function updatePresentation(state: WindowsUpdateState, t: Translate) {
  const version = state.version ?? state.currentVersion
  const values = { version, percent: Math.floor(state.progress ?? 0) }
  let action: 'check' | 'download' | 'restart' | null = null
  let label = ''
  let status = ''
  switch (state.phase) {
    case 'idle': case 'checking': status = t('vrChecking'); break
    case 'current': status = t('vrHave', { version: state.currentVersion }); break
    case 'available': label = t('wuDownload', values); status = t('wuAvailable', values); action = 'download'; break
    case 'downloading': label = status = t('wuDownloading', values); break
    case 'verifying': label = status = t('wuVerifying', values); break
    case 'ready': label = t('wuRestart', values); status = t('wuReady', values); action = 'restart'; break
    case 'installing': label = status = t('wuInstalling', values); break
    case 'failed':
      label = state.failure === 'check' ? t('vrCheckNow') : t('wuRetry', values)
      status = state.failure === 'check' ? t('vrFailed') : state.failure === 'restart' ? t('wuRestartFailed') : t('wuDownloadFailed')
      action = state.failure === 'check' || !state.version ? 'check' : 'download'
      break
  }
  if (state.paused && (state.phase === 'available' || state.phase === 'failed')) {
    const reason = t(state.paused === 'disabled' ? 'wuAutoOff' : state.paused === 'metered' ? 'wuMetered' : 'wuCostUnknown')
    status += ` · ${reason}`
    if (state.paused !== 'disabled') label += ` · ${reason}`
  }
  if (state.phase === 'ready' && state.failure) status += ` · ${t(state.failure === 'check' ? 'vrFailed' : 'wuRestartFailed')}`
  return { dot: ['available', 'ready', 'failed'].includes(state.phase), action, label, status,
    busy: ['checking', 'downloading', 'verifying', 'installing'].includes(state.phase) }
}

export function requestWindowsUpdate(action: 'check' | 'download' | 'restart'): void {
  void bridge()?.updates?.[action]().catch(() => {
    if (state) {
      state = { ...state, phase: 'failed', failure: action === 'check' ? 'check' : action === 'download' ? 'download' : 'restart' }
      for (const listener of listeners) listener()
    }
  })
}
