/** Login to a Morde Client Installation, separately from the nanoMuse Cloud account. */
import { Button, Input, StateDot } from '@deepseek-ai/dsh-client-ui-primitives'
import { createElement as h, useEffect, useRef, useState, type FormEvent, type ReactNode } from 'react'
import { call, column, muted, row, type MordeHubStatus, type Translate } from './api.ts'
import { useLive } from './live.ts'

declare global {
  interface Window {
    nanomuseHarness?: { chooseDirectory?: () => Promise<string | null> }
  }
}

const cardHeading: Record<string, string | number> = { fontSize: 14, fontWeight: 600, margin: '0 0 4px' }

/** Login for a Morde Hub installation. Its credentials and status never touch CloudStatus. */
export function MordeHubCard({ t }: { t: Translate }): ReactNode {
  const live = useLive()
  const [status, setStatus] = useState<MordeHubStatus | undefined>(live.mordeHub)
  const liveStatus = useRef<MordeHubStatus | undefined>(live.mordeHub)
  const [baseURL, setBaseURL] = useState('')
  const [name, setName] = useState('')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | undefined>()

  const apply = (next: MordeHubStatus) => {
    setStatus(next)
    setError(undefined)
    if (next.baseURL) setBaseURL(next.baseURL)
    if (next.accountName) setName(next.accountName)
  }
  const errorText = (err: unknown) => {
    const message = typeof err === 'object' && err !== null && 'message' in err
      ? String((err as { message: unknown }).message ?? '')
      : String(err ?? '')
    return t('failed', { message: message || '?' })
  }
  const currentStatus = status

  useEffect(() => {
    let alive = true
    call<MordeHubStatus>('morde-hub/status')
      .then((next) => { if (alive) apply(liveStatus.current ?? next) })
      .catch((err: unknown) => { if (alive) setError(errorText(err)) })
    return () => { alive = false }
  }, [])
  useEffect(() => {
    liveStatus.current = live.mordeHub
    if (live.mordeHub) apply(live.mordeHub)
  }, [live.mordeHub])

  const updateGrants = async (patch: { screenshot?: boolean; files?: boolean; command?: boolean; file_root?: string | null }) => {
    setBusy(true)
    setError(undefined)
    try {
      apply(await call<MordeHubStatus>('morde-hub/grants', patch))
    } catch (err: unknown) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }

  const chooseFolder = async () => {
    const picker = typeof window === 'undefined' ? undefined : window.nanomuseHarness?.chooseDirectory
    if (!picker || (currentStatus?.capabilities?.files !== true && currentStatus?.capabilities?.command !== true) || busy) return
    setBusy(true)
    setError(undefined)
    try {
      const fileRoot = await picker()
      if (fileRoot) apply(await call<MordeHubStatus>('morde-hub/grants', { file_root: fileRoot }))
    } catch (err: unknown) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }

  const login = (event: FormEvent) => {
    event.preventDefault()
    setBusy(true)
    setError(undefined)
    void call<MordeHubStatus>('morde-hub/login', { base_url: baseURL.trim(), name: name.trim(), password })
      .then(apply)
      .catch((err: unknown) => setError(errorText(err)))
      .finally(() => { setPassword(''); setBusy(false) })
  }
  const signOut = () => {
    setBusy(true)
    setError(undefined)
    void call<MordeHubStatus>('morde-hub/sign-out', {})
      .then(apply)
      .catch((err: unknown) => setError(errorText(err)))
      .finally(() => setBusy(false))
  }
  const refreshDevices = () => {
    setBusy(true)
    setError(undefined)
    void call<MordeHubStatus>('morde-hub/status')
      .then(apply)
      .catch((err: unknown) => setError(errorText(err)))
      .finally(() => setBusy(false))
  }
  const revokeDevice = (device: MordeHubStatus['devices'][number]) => {
    if (typeof window !== 'undefined' && !window.confirm(t('mordeHubRevokeConfirm', { name: device.name }))) return
    setBusy(true)
    setError(undefined)
    void call<MordeHubStatus>('morde-hub/revoke-device', { installation_id: device.id })
      .then(apply)
      .catch((err: unknown) => setError(errorText(err)))
      .finally(() => setBusy(false))
  }

  const statusError = currentStatus?.lastError ?? error
  const state = statusError ? 'error' : currentStatus?.connected ? 'done' : 'idle'
  const grants = currentStatus?.grants
  const capabilities = currentStatus?.capabilities
  const screenshotGranted = grants?.screenshot === true
  const filesGranted = grants?.files === true
  const commandGranted = grants?.command === true
  const hasRoot = typeof grants?.fileRoot === 'string' && grants.fileRoot.length > 0
  const pickerAvailable = typeof window !== 'undefined' && typeof window.nanomuseHarness?.chooseDirectory === 'function'
  const canChooseFolder = (capabilities?.files === true || capabilities?.command === true) && pickerAvailable
  const stateText = statusError
    ? t('mordeHubError', { reason: statusError })
    : currentStatus?.connected ? t('mordeHubOnline') : t('mordeHubOffline')

  return h('div', { className: 'nm-card', style: { padding: '14px 16px' }, 'data-testid': 'nm-morde-hub-login' },
    h('h3', { style: cardHeading }, t('mordeHubTitle')),
    h('div', { style: muted }, t('mordeHubIntro')),
    h('div', { style: { ...row, marginTop: 8 }, 'aria-live': 'polite' },
      h(StateDot, { state, size: 10 }),
      h('span', null, stateText)),
    currentStatus?.signedIn
      ? h('div', { style: column },
          h('div', { style: muted }, t('mordeHubAddress', { baseURL: currentStatus.baseURL })),
          currentStatus.accountName ? h('div', null, t('mordeHubAccount', { name: currentStatus.accountName })) : null,
          currentStatus.installationID ? h('div', null, t('mordeHubInstallation', { id: currentStatus.installationID })) : null,
          currentStatus.expiresAt ? h('div', null, t('mordeHubExpires', { expiresAt: currentStatus.expiresAt })) : null,
          h('section', { style: column, 'data-testid': 'nm-morde-hub-devices' },
            h('div', { style: row },
              h('h4', { style: cardHeading }, t('mordeHubDevices')),
              h(Button, { variant: 'outline', size: 'sm', disabled: busy, onClick: refreshDevices }, t('mordeHubRefreshDevices'))),
            h('div', { style: muted }, t('mordeHubDeviceListHint')),
            ...(currentStatus.devices ?? []).map((device) => {
              const isCurrent = device.id === currentStatus.deviceID
              const isExpired = Date.parse(device.expiresAt) <= Date.now()
              return h('div', { key: device.id, style: row, 'data-testid': `nm-morde-hub-device-${device.id}` },
                h('span', null, `${device.name} · ${device.platform} · ${device.revoked ? t('mordeHubDeviceRevoked') : isExpired ? t('mordeHubDeviceExpired') : device.online ? t('mordeHubOnline') : t('mordeHubOffline')}${isCurrent ? ` · ${t('mordeHubThisComputer')}` : ''}`),
                !isCurrent && !device.revoked && !isExpired
                  ? h(Button, { variant: 'outline', size: 'sm', disabled: busy, onClick: () => revokeDevice(device) }, t('mordeHubRevoke'))
                  : null)
            })),
          h('section', { style: column, 'data-testid': 'nm-morde-hub-grants' },
            h('h4', { style: cardHeading }, t('mordeHubTargetGrants')),
            h('label', { style: row },
              h(Input, {
                type: 'checkbox',
                checked: screenshotGranted,
                disabled: busy || (!screenshotGranted && capabilities?.screenshot !== true),
                'aria-label': t('mordeHubScreenshot'),
                onChange: (event: { currentTarget: { checked: boolean } }) => void updateGrants({ screenshot: event.currentTarget.checked }),
              }),
              h('span', null, t('mordeHubScreenshot'))),
            h('div', { style: muted }, t('mordeHubScreenshotHelp')),
            capabilities?.screenshot === true ? null : h('div', { style: muted, 'data-testid': 'nm-morde-hub-screenshot-unavailable' }, t('mordeHubScreenshotUnavailable')),
            h('div', { style: row },
              h('span', null, t('mordeHubWorkspace')),
              h(Button, {
                variant: 'outline', size: 'sm', disabled: busy || !canChooseFolder,
                onClick: () => void chooseFolder(),
              }, hasRoot ? t('mordeHubChangeFolder') : t('mordeHubChooseFolder')),
              hasRoot ? h(Button, {
                variant: 'outline', size: 'sm', disabled: busy,
                onClick: () => void updateGrants({ file_root: null }),
              }, t('mordeHubClearWorkspace')) : null),
            hasRoot && grants?.fileRoot ? h('div', { style: muted, 'data-testid': 'nm-morde-hub-file-root' }, t('mordeHubFolderRoot', { path: grants.fileRoot })) : null,
            h('label', { style: row },
              h(Input, {
                type: 'checkbox',
                checked: filesGranted,
                disabled: busy || (!filesGranted && (capabilities?.files !== true || !hasRoot)),
                'aria-label': t('mordeHubFiles'),
                onChange: (event: { currentTarget: { checked: boolean } }) => void updateGrants({ files: event.currentTarget.checked }),
              }),
              h('span', null, t('mordeHubFiles'))),
            !canChooseFolder ? h('div', { style: muted, 'data-testid': 'nm-morde-hub-files-unavailable' },
              capabilities?.files !== true && capabilities?.command !== true ? t('mordeHubFilesUnavailable') : t('mordeHubPickerUnavailable')) : null,
            h('label', { style: row },
              h(Input, {
                type: 'checkbox',
                checked: commandGranted,
                disabled: busy || (!commandGranted && (capabilities?.command !== true || !hasRoot)),
                'aria-label': t('mordeHubCommand'),
                onChange: (event: { currentTarget: { checked: boolean } }) => void updateGrants({ command: event.currentTarget.checked }),
              }),
              h('span', null, t('mordeHubCommand'))),
            h('div', { style: muted }, t('mordeHubCommandHelp')),
            capabilities?.command === true ? null : h('div', { style: muted, 'data-testid': 'nm-morde-hub-command-unavailable' }, t('mordeHubCommandUnavailable'))),
          h('div', { style: row },
            h(Button, { variant: 'outline', size: 'sm', disabled: busy, onClick: signOut }, busy ? t('mordeHubDisconnecting') : t('mordeHubDisconnect'))))
      : h('form', { style: { ...column, marginTop: 8 }, onSubmit: login },
          h(Input, { type: 'url', value: baseURL, onChange: (e: FormEvent<HTMLInputElement>) => setBaseURL(e.currentTarget.value), placeholder: t('mordeHubBaseURL'), autoComplete: 'url', spellCheck: false, required: true, 'aria-label': t('mordeHubBaseURL') }),
          h(Input, { value: name, onChange: (e: FormEvent<HTMLInputElement>) => setName(e.currentTarget.value), placeholder: t('mordeHubName'), autoComplete: 'username', required: true, 'aria-label': t('mordeHubName') }),
          h(Input, { type: 'password', value: password, onChange: (e: FormEvent<HTMLInputElement>) => setPassword(e.currentTarget.value), placeholder: t('mordeHubPassword'), autoComplete: 'current-password', required: true, 'aria-label': t('mordeHubPassword') }),
          h('div', { style: muted }, currentStatus?.baseURL ? t('mordeHubAddress', { baseURL: currentStatus.baseURL }) : ''),
          h('div', { style: row },
            h(Button, { variant: 'primary', size: 'sm', type: 'submit', disabled: busy || !baseURL.trim() || !name.trim() || !password }, busy ? t('mordeHubSigningIn') : t('mordeHubSignIn')))))
}
