import { WorkerIPC } from './src/ipc.mjs'
import { ENGINE_VERSION, runAgent } from './src/engine.mjs'

const ipc = new WorkerIPC(process.stdin, process.stdout)
process.on('SIGTERM', () => {
  if (ipc.started) ipc.cancel()
  else ipc.fail('runtime stopped')
})

try {
  const allowedEnv = new Set(['PATH', 'LANG', 'LC_ALL', 'TZ', 'NODE_ENV'])
  if (process.platform === 'darwin') allowedEnv.add('__CF_USER_TEXT_ENCODING')
  // Node 24 on Linux adds this libuv control to its own process environment.
  if (process.platform === 'linux' && ['0', '1'].includes(process.env.UV_USE_IO_URING)) allowedEnv.add('UV_USE_IO_URING')
  if (Object.keys(process.env).some(key => !allowedEnv.has(key))) throw new Error('unclean process environment')
  await ipc.send({ type: 'ready', version: 1, engine: 'dsh', engine_version: ENGINE_VERSION })
  const start = await ipc.start.promise
  const result = await runAgent(ipc, start)
  await ipc.send(result)
} catch {
  process.exitCode = 1
  await ipc.send({ type: 'failed', code: 'runtime_unavailable' }).catch(() => {})
} finally {
  process.stdin.destroy()
}
