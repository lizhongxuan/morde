import assert from 'node:assert/strict'
import test from 'node:test'
import { adaptWindowsAclExecution } from '../lib/windows-pwsh-sandbox.js'
import { ensureWindowsConsoleHost } from '../lib/windows-console-host.js'

const spec = (env = {}) => ({
  command: 'Write-Output ok',
  workdir: 'C:\\work',
  timeoutMs: 60_000,
  onExpiry: 'kill',
  stdoutMaxBytes: 1024,
  env,
  sandboxPolicy: undefined,
})

test('the exact Electron Windows ACL invocation goes through the nanoMuse trampoline', () => {
  const original = spec({ Path: 'C:\\Windows', electron_run_as_node: 'old' })
  const adaptation = {
    platform: 'win32',
    electron: true,
    execPath: 'C:\\nanoMuse\\nanoMuse.exe',
    upstreamRunner: 'C:\\nanoMuse\\runner.js',
    trampoline: 'C:\\nanoMuse\\windows-acl-runner.js',
  }
  const result = adaptWindowsAclExecution(
    original,
    [adaptation.execPath, adaptation.upstreamRunner, '--workspace', 'C:\\work'],
    adaptation,
  )
  assert.deepEqual(result.argv, [
    adaptation.execPath,
    adaptation.trampoline,
    adaptation.upstreamRunner,
    '--workspace',
    'C:\\work',
  ])
  assert.deepEqual(result.spec.env, {
    Path: 'C:\\Windows',
    ELECTRON_RUN_AS_NODE: '1',
  })
  assert.equal(original.env.electron_run_as_node, 'old')
})

test('direct PowerShell and non-Electron or non-Windows invocations stay unchanged', () => {
  const original = spec({ Path: 'C:\\Windows' })
  const argv = ['pwsh.exe', '-Command', 'Write-Output ok']
  const base = {
    platform: 'win32',
    electron: true,
    execPath: 'C:\\nanoMuse\\nanoMuse.exe',
    upstreamRunner: 'C:\\nanoMuse\\runner.js',
    trampoline: 'C:\\nanoMuse\\windows-acl-runner.js',
  }
  for (const adaptation of [
    base,
    { ...base, platform: 'linux' },
    { ...base, electron: false },
  ]) {
    const result = adaptWindowsAclExecution(original, argv, adaptation)
    assert.equal(result.spec, original)
    assert.equal(result.argv, argv)
  }
})

function consoleApi(overrides = {}) {
  return {
    hasConsole: () => false,
    allocWindowlessConsole: () => false,
    attachConsole: () => 0,
    getConsoleWindow: () => null,
    allocConsole: () => 1,
    getLastError: () => 5,
    showWindow: () => 1,
    ...overrides,
  }
}

test('a process that already owns a console needs no allocation', async () => {
  let donorStarted = false
  await ensureWindowsConsoleHost(
    'win32',
    () => consoleApi({ hasConsole: () => true, allocConsole: () => assert.fail('allocated') }),
    async () => {
      donorStarted = true
      throw new Error('not reached')
    },
  )
  assert.equal(donorStarted, false)
})

test('Windows 11 uses its windowless console API before any donor', async () => {
  let donorStarted = false
  await ensureWindowsConsoleHost(
    'win32',
    () => consoleApi({ allocWindowlessConsole: () => true }),
    async () => {
      donorStarted = true
      throw new Error('not reached')
    },
  )
  assert.equal(donorStarted, false)
})

test('older Windows attaches to a temporary windowless console donor', async () => {
  let released = false
  let attached = 0
  await ensureWindowsConsoleHost(
    'win32',
    () => consoleApi({ attachConsole: (pid) => ((attached = pid), 1) }),
    async () => ({ pid: 42, release: () => (released = true) }),
  )
  assert.equal(attached, 42)
  assert.equal(released, true)
})

test('ordinary hidden console is the final fallback and allocation failure is explicit', async () => {
  const windows = {}
  await ensureWindowsConsoleHost(
    'win32',
    () =>
      consoleApi({
        allocConsole: () => 1,
        getConsoleWindow: () => windows,
        showWindow: (window, command) => {
          assert.equal(window, windows)
          assert.equal(command, 0)
          return 1
        },
      }),
    async () => {
      throw new Error('donor unavailable')
    },
  )

  await assert.rejects(
    ensureWindowsConsoleHost(
      'win32',
      () => consoleApi({ allocConsole: () => 0, getLastError: () => 5 }),
      async () => {
        throw new Error('donor unavailable')
      },
    ),
    /Win32 5/,
  )
})

test('non-Windows hosts never load the native console API', async () => {
  await ensureWindowsConsoleHost(
    'linux',
    () => assert.fail('loaded native API'),
    async () => assert.fail('started donor'),
  )
})
