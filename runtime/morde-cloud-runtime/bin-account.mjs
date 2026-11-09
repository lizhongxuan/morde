import { AccountRuntime } from './src/account-runtime.mjs'

const allowedEnv = new Set(['PATH', 'LANG', 'LC_ALL', 'TZ', 'NODE_ENV'])
if (process.platform === 'darwin') allowedEnv.add('__CF_USER_TEXT_ENCODING')
if (process.platform === 'linux' && ['0', '1'].includes(process.env.UV_USE_IO_URING)) allowedEnv.add('UV_USE_IO_URING')
if (Object.keys(process.env).some(key => !allowedEnv.has(key))) {
  process.exitCode = 1
} else {
  const runtime = new AccountRuntime(process.stdin, process.stdout)
  process.on('SIGTERM', () => runtime.shutdown('signal'))
  try {
    await runtime.run()
  } catch {
    process.exitCode = 1
    await new Promise(resolve => process.stdout.write(`${JSON.stringify({ type: 'failed', code: 'runtime_unavailable' })}\n`, resolve))
  }
}
process.stdin.destroy()
