import { spawn, type ChildProcess } from 'node:child_process'
import { lstat } from 'node:fs/promises'
import { isAbsolute, resolve } from 'node:path'
import { HubError } from './hub.ts'

export const HUB_FILE_ENTRY_LIMIT = 128
export const HUB_FILE_READ_LIMIT = 32 * 1024
export const HUB_SCREEN_BYTES_LIMIT = 40 * 1024
export const HUB_COMMAND_OUTPUT_LIMIT = 16 * 1024

export interface MordeHubFileRoot {
  directory: string
  identity: string
}

export interface MordeHubFileEntry {
  path: string
  kind: 'file' | 'directory'
  size_bytes: number
}

export interface MordeHubTaskGrant {
  targetGrantVersion: number
  grants: { screenshot: boolean; files: boolean; command: boolean }
  fileRoot?: MordeHubFileRoot
}

export function requireMordeHubTaskGrant(action: 'screenshot' | 'files' | 'file.get' | 'command', caller: {
  kind: string
  taskContext?: { taskID: string; authorizationVersion: number; targetGrantVersion: number; operationID: string }
}, grant: MordeHubTaskGrant): number {
  const task = caller.taskContext
  if (caller.kind !== 'task' || !task || !/^q[a-z0-9]{7}$/.test(task.taskID) ||
    !Number.isSafeInteger(task.authorizationVersion) || task.authorizationVersion < 1 ||
    !Number.isSafeInteger(task.targetGrantVersion) || task.targetGrantVersion !== grant.targetGrantVersion ||
    !/^[A-Za-z0-9._:-]{4,80}$/.test(task.operationID)) {
    throw new HubError('not_allowed', 'This action requires a current authorized Morde Task')
  }
  if (action === 'screenshot' && !grant.grants.screenshot) throw new HubError('not_allowed', 'Screenshot access is not allowed on this computer')
  if ((action === 'files' || action === 'file.get') && (!grant.grants.files || !grant.fileRoot?.directory || !grant.fileRoot.identity)) {
    throw new HubError('not_allowed', 'File access is not allowed on this computer')
  }
  if (action === 'command' && (!grant.grants.command || !grant.fileRoot?.directory || !grant.fileRoot.identity)) {
    throw new HubError('not_allowed', 'Command execution is not allowed on this computer')
  }
  return grant.targetGrantVersion
}

type SpawnProcess = typeof spawn
type OperatorConfig = { url: string; token: string }

function helperPath(env: NodeJS.ProcessEnv): string | undefined {
  const value = env.MORDE_SYNC_BIN?.trim()
  return value && isAbsolute(value) && resolve(value) === value ? value : undefined
}

export async function mordeHubFilesAvailable(env: NodeJS.ProcessEnv = process.env): Promise<boolean> {
  const binary = helperPath(env)
  if (!binary) return false
  try {
    const file = await lstat(binary)
    return file.isFile() && !file.isSymbolicLink()
  } catch {
    return false
  }
}

export function operatorConfig(env: NodeJS.ProcessEnv = process.env): OperatorConfig | undefined {
  const urlText = env.NANOMUSE_OPERATOR_URL?.trim() ?? ''
  const token = env.NANOMUSE_OPERATOR_TOKEN?.trim() ?? ''
  if (!/^([a-f0-9]{48})$/i.test(token)) return undefined
  try {
    const url = new URL(urlText)
    if (url.protocol !== 'http:' || url.hostname !== '127.0.0.1' || url.username || url.password ||
      url.pathname !== '/' || url.search || url.hash || !Number(url.port)) return undefined
    return { url: url.origin, token }
  } catch {
    return undefined
  }
}

async function boundedJSON(response: Response, maxBytes: number): Promise<Record<string, unknown>> {
  if (!response.body) throw new HubError('failed', 'The local operator returned no response')
  const reader = response.body.getReader()
  const chunks: Uint8Array[] = []
  let total = 0
  try {
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      total += value.byteLength
      if (total > maxBytes) {
        await reader.cancel().catch(() => undefined)
        throw new HubError('failed', 'The local operator response exceeded its limit')
      }
      chunks.push(value)
    }
  } finally {
    reader.releaseLock()
  }
  let value: unknown
  try {
    value = JSON.parse(Buffer.concat(chunks.map((chunk) => Buffer.from(chunk))).toString('utf8'))
  } catch {
    throw new HubError('failed', 'The local operator returned an invalid response')
  }
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new HubError('failed', 'The local operator returned an invalid response')
  return value as Record<string, unknown>
}

async function operatorRequest(path: string, body?: Record<string, unknown>, env: NodeJS.ProcessEnv = process.env, fetcher: typeof fetch = fetch): Promise<Record<string, unknown>> {
  const config = operatorConfig(env)
  if (!config) throw new HubError('unsupported', 'The desktop screen operator is unavailable')
  let response: Response
  try {
    response = await fetcher(`${config.url}${path}`, {
      method: body ? 'POST' : 'GET',
      headers: { authorization: `Bearer ${config.token}`, accept: 'application/json', ...(body ? { 'content-type': 'application/json' } : {}) },
      ...(body ? { body: JSON.stringify(body) } : {}),
      redirect: 'error',
      signal: AbortSignal.timeout(10_000),
    })
  } catch {
    throw new HubError('failed', 'The desktop screen operator did not answer')
  }
  if (response.status === 403) throw new HubError('permission_required', 'Screen Recording permission is not available')
  if (response.status === 503) throw new HubError('unsupported', 'Screen capture is not available on this desktop')
  if (!response.ok) throw new HubError('failed', 'The desktop screen operator refused the request')
  return boundedJSON(response, 1 << 20)
}

export async function mordeHubScreenshotAvailable(env: NodeJS.ProcessEnv = process.env, fetcher: typeof fetch = fetch): Promise<boolean> {
  try {
    return (await operatorRequest('/info', undefined, env, fetcher)).available === true
  } catch {
    return false
  }
}

export async function mordeHubScreenshot(targetGrantVersion: number, env: NodeJS.ProcessEnv = process.env, fetcher: typeof fetch = fetcherDefault): Promise<Record<string, unknown>> {
  for (const maxPixels of [262_144, 65_536, 16_384]) {
    const body = await operatorRequest('/screenshot', { max_pixels: maxPixels, format: 'jpeg', quality: 35 }, env, fetcher)
    const encoded = typeof body.base64 === 'string' ? body.base64 : ''
    const data = Buffer.from(encoded, 'base64')
    if (!encoded || data.length === 0 || data.toString('base64') !== encoded || !data.subarray(0, 3).equals(Buffer.from([0xff, 0xd8, 0xff]))) {
      throw new HubError('failed', 'The desktop screen operator returned an invalid image')
    }
    if (data.length > HUB_SCREEN_BYTES_LIMIT) continue
    const width = Number(body.width), height = Number(body.height)
    if (!Number.isSafeInteger(width) || !Number.isSafeInteger(height) || width < 1 || height < 1 || width > 4096 || height > 4096) {
      throw new HubError('failed', 'The desktop screen operator returned invalid image dimensions')
    }
    return { mime_type: 'image/jpeg', width, height, data_base64: encoded, target_grant_version: targetGrantVersion }
  }
  throw new HubError('failed', 'The screenshot exceeds the Hub size limit')
}

function fetcherDefault(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
  return fetch(input, init)
}

function isRelativePath(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0 && value.length <= 1024 && !value.startsWith('/') &&
    !value.includes('\\') && !value.includes('\0') && !/^[A-Za-z]:/.test(value) &&
    value.split('/').every((part) => part !== '' && part !== '.' && part !== '..')
}

function processOutput(child: ChildProcess, method: string, params: Record<string, unknown>, timeoutMs: number, signal?: AbortSignal): Promise<unknown> {
  return new Promise((resolveResult, rejectResult) => {
    const chunks: Buffer[] = []
    let bytes = 0
    let failed = false
    let stoppedBy = ''
    let killTimer: ReturnType<typeof setTimeout> | undefined
    const stopChild = (reason: string) => {
      if (stoppedBy) return
      stoppedBy = reason
      child.kill('SIGTERM')
      killTimer = setTimeout(() => child.kill('SIGKILL'), 15_000)
    }
    const timer = setTimeout(() => stopChild('timeout'), timeoutMs)
    const fail = (error: HubError) => {
      if (failed) return
      failed = true
      clearTimeout(timer)
      if (killTimer) clearTimeout(killTimer)
      signal?.removeEventListener('abort', onAbort)
      rejectResult(error)
    }
    const onAbort = () => stopChild('cancelled')
    if (signal?.aborted) onAbort()
    else signal?.addEventListener('abort', onAbort, { once: true })
    child.stdout?.on('data', (chunk: Buffer | string) => {
      if (stoppedBy) return
      const value = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk)
      bytes += value.length
      if (bytes > 1 << 20) {
        stopChild('oversized')
        return
      }
      chunks.push(value)
    })
    child.stderr?.resume()
    child.once('error', () => fail(new HubError('unsupported', 'The local file helper could not start')))
    child.once('close', (code) => {
      clearTimeout(timer)
      if (killTimer) clearTimeout(killTimer)
      if (failed) return
      signal?.removeEventListener('abort', onAbort)
      if (stoppedBy === 'cancelled') return fail(new HubError('cancelled', 'The local helper request was cancelled'))
      if (stoppedBy === 'timeout') return fail(new HubError('failed', 'The local helper request timed out'))
      if (stoppedBy === 'oversized') return fail(new HubError('failed', 'The local helper exceeded its response limit'))
      if (code !== 0) return fail(new HubError('failed', 'The local file helper refused the request'))
      let response: Record<string, unknown>
      try {
        const parsed: unknown = JSON.parse(Buffer.concat(chunks).toString('utf8'))
        if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('invalid response')
        response = parsed as Record<string, unknown>
      } catch {
        return fail(new HubError('failed', 'The local file helper returned an invalid response'))
      }
      if (response.jsonrpc !== '2.0' || response.id !== 'morde-hub' || response.error) {
        return fail(new HubError('not_allowed', 'The local file helper rejected this path'))
      }
      resolveResult(response.result)
    })
    child.stdin?.end(`${JSON.stringify({ jsonrpc: '2.0', id: 'morde-hub', method, params })}\n`)
  })
}

export async function mordeHubHelperRequest(method: string, params: Record<string, unknown>, env: NodeJS.ProcessEnv = process.env, spawnProcess: SpawnProcess = spawn, timeoutMs = 15_000, signal?: AbortSignal): Promise<unknown> {
  const binary = helperPath(env)
  if (!binary || !(await mordeHubFilesAvailable(env))) throw new HubError('unsupported', 'The local file helper is unavailable')
  let child: ChildProcess
  try {
    child = spawnProcess(binary, [], {
      shell: false, stdio: ['pipe', 'pipe', 'ignore'], windowsHide: true,
      env: process.platform === 'win32' && process.env.SystemRoot ? { SystemRoot: process.env.SystemRoot } : {},
    })
  } catch {
    throw new HubError('unsupported', 'The local file helper could not start')
  }
  return processOutput(child, method, params, timeoutMs, signal)
}

let commandAvailability: { binary: string; checkedAt: number; available: boolean } | undefined

export async function mordeHubCommandAvailable(env: NodeJS.ProcessEnv = process.env): Promise<boolean> {
  if (process.platform !== 'linux') return false
  const binary = helperPath(env)
  if (!binary || !(await mordeHubFilesAvailable(env))) return false
  if (commandAvailability?.binary === binary && Date.now() - commandAvailability.checkedAt < 30_000) return commandAvailability.available
  let available = false
  try {
    const value = await mordeHubHelperRequest('hub_command_probe', {}, env, spawn, 10_000) as Record<string, unknown> | undefined
    available = value?.available === true
  } catch {
    available = false
  }
  commandAvailability = { binary, checkedAt: Date.now(), available }
  return available
}

export async function mordeHubCommand(
  root: MordeHubFileRoot,
  command: unknown,
  timeoutMs: unknown,
  targetGrantVersion: number,
  signal?: AbortSignal,
  env: NodeJS.ProcessEnv = process.env,
): Promise<Record<string, unknown>> {
  if (typeof command !== 'string' || !command || Buffer.byteLength(command) > 8192 || command.includes('\0') ||
    !Number.isSafeInteger(timeoutMs) || Number(timeoutMs) < 1000 || Number(timeoutMs) > 300_000) {
    throw new HubError('not_allowed', 'The command request is outside the allowed bounds')
  }
  if (signal?.aborted) throw new HubError('cancelled', 'The command was cancelled')
  const value = await mordeHubHelperRequest('hub_command', {
    directory: root.directory, identity: root.identity, command, timeout_ms: timeoutMs,
  }, env, spawn, Number(timeoutMs) + 20_000, signal) as Record<string, unknown> | undefined
  if (!value || !Number.isSafeInteger(value.exit_code) || Number(value.exit_code) < 0 || Number(value.exit_code) > 255 ||
    typeof value.stdout !== 'string' || Buffer.byteLength(value.stdout) > HUB_COMMAND_OUTPUT_LIMIT ||
    typeof value.stderr !== 'string' || Buffer.byteLength(value.stderr) > HUB_COMMAND_OUTPUT_LIMIT ||
    typeof value.timed_out !== 'boolean' || typeof value.stopped !== 'boolean') {
    throw new HubError('failed', 'The local command helper returned an invalid result')
  }
  if (!value.stopped) throw new HubError('timeout', 'The local command helper could not confirm that the command stopped')
  return { exit_code: value.exit_code, stdout: value.stdout, stderr: value.stderr, timed_out: value.timed_out, stopped: true, target_grant_version: targetGrantVersion }
}

export async function mordeHubSelectedRoot(directory: unknown, env: NodeJS.ProcessEnv = process.env): Promise<MordeHubFileRoot> {
  if (typeof directory !== 'string' || !isAbsolute(directory) || resolve(directory) !== directory || directory.length > 4096) {
    throw new HubError('not_allowed', 'Choose a valid local folder')
  }
  const value = await mordeHubHelperRequest('hub_root', { directory }, env) as Record<string, unknown> | undefined
  if (!value || typeof value.directory !== 'string' || !isAbsolute(value.directory) || resolve(value.directory) !== value.directory ||
    typeof value.identity !== 'string' || !/^[A-Za-z0-9_-]{16,256}$/.test(value.identity)) {
    throw new HubError('failed', 'The local file helper returned an invalid folder identity')
  }
  return { directory: value.directory, identity: value.identity }
}

export async function mordeHubFiles(root: MordeHubFileRoot, path: unknown, targetGrantVersion: number, env: NodeJS.ProcessEnv = process.env): Promise<Record<string, unknown>> {
  if (path !== undefined && path !== '' && !isRelativePath(path)) throw new HubError('not_allowed', 'The folder path is outside the selected range')
  const value = await mordeHubHelperRequest('hub_files', {
    directory: root.directory, identity: root.identity, ...(typeof path === 'string' && path ? { path } : {}),
  }, env) as Record<string, unknown> | undefined
  if (!value || !Array.isArray(value.entries) || value.entries.length > HUB_FILE_ENTRY_LIMIT) throw new HubError('failed', 'The local file helper returned an invalid listing')
  const entries = value.entries.map((entry) => {
    if (!entry || typeof entry !== 'object') throw new HubError('failed', 'The local file helper returned an invalid listing')
    const item = entry as Record<string, unknown>
    if (!isRelativePath(item.path) || (item.kind !== 'file' && item.kind !== 'directory') ||
      !Number.isSafeInteger(item.size_bytes) || Number(item.size_bytes) < 0) {
      throw new HubError('failed', 'The local file helper returned an invalid listing')
    }
    return { path: item.path, kind: item.kind, size_bytes: item.size_bytes }
  })
  return { entries, target_grant_version: targetGrantVersion }
}

export async function mordeHubFileGet(root: MordeHubFileRoot, path: unknown, maxBytes: unknown, targetGrantVersion: number, env: NodeJS.ProcessEnv = process.env): Promise<Record<string, unknown>> {
  if (!isRelativePath(path) || !Number.isSafeInteger(maxBytes) || Number(maxBytes) < 1 || Number(maxBytes) > HUB_FILE_READ_LIMIT) {
    throw new HubError('not_allowed', 'The file request is outside the selected range')
  }
  const value = await mordeHubHelperRequest('hub_file_get', {
    directory: root.directory, identity: root.identity, path, max_bytes: maxBytes,
  }, env) as Record<string, unknown> | undefined
  if (!value || value.path !== path || typeof value.text !== 'string' || Buffer.byteLength(value.text) > Number(maxBytes) ||
    typeof value.truncated !== 'boolean') throw new HubError('failed', 'The local file helper returned an invalid file')
  return { path, text: value.text, truncated: value.truncated, target_grant_version: targetGrantVersion }
}
