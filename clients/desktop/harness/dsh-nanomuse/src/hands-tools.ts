/**
 * The hands, from inside a chat: one `@deepseek-ai/dsh-mcp-client` over stdio
 * to the runtime's `nanomuse mcp` (nanomuse/bridge/mcp_server.py), mounted here
 * in the agent's scope so its tools arrive as `mcp__nanomuse__computer_screen`
 * and `computer_act` like any other MCP server's.
 *
 * Until 0.1.40 the preset mounted the client itself and read the hands model
 * from `$DSH_HOME/nanomuse/hands.json` once, when the server started; a change
 * of model needed a restart. The MCP client has no way to change a server's
 * environment once it runs, so this plugin does what the connectors do
 * (`connectors-tools.ts`): it asks the cloud service for the hands'
 * environment (`NANOMUSE_GUI_*`), mounts the client with it, and when the
 * environment changes (Settings → Models → Operating the screen, a key saved
 * or removed, a sign-in) disposes the client and mounts it again. The runtime
 * starts in a second or two; a chat in the middle of a hands call keeps its
 * client until the call ends. A preset row; it needs the host row
 * `nanomuse-cloud`.
 */
import type { Context, Fiber } from '@deepseek-ai/cordis'
import * as mcpClient from '@deepseek-ai/dsh-mcp-client'
import { homedir } from 'node:os'
import type {} from './cloud.ts'

export const name = 'nanomuse-hands-tools'
export const inject = ['tools', 'nanomuseCloud']

/** The names the harness drops from a child's inherited environment (credential-shaped), so they are passed by name. */
const INHERITED = ['DISPLAY', 'NANOMUSE_MCP_CONFIRM', 'NANOMUSE_OPERATOR_URL', 'NANOMUSE_OPERATOR_TOKEN'] as const

/**
 * The MCP client's config for `nanomuse mcp` with the hands' environment. NANOMUSE_PY names the
 * runtime's executable when it is not on PATH (a venv, the desktop app's bundled runtime); a
 * missing runtime leaves the preset without hands rather than broken (`failOnStartupError: false`).
 */
export function clientConfig(hands: Record<string, string>, env: NodeJS.ProcessEnv = process.env): Record<string, unknown> {
  const inherited = Object.fromEntries(INHERITED.filter((k) => env[k]).map((k) => [k, env[k] as string]))
  return {
    transport: 'stdio',
    serverName: 'nanomuse',
    command: env.NANOMUSE_PY || 'nanomuse',
    args: ['mcp'],
    env: { ...inherited, ...hands },
    cwd: env.HOME || env.USERPROFILE || homedir(),
    toolCallTimeoutMs: 120_000,
    failOnStartupError: false,
  }
}

/**
 * The sentence for a mount that failed, for the log and the UI: the error's message, and the
 * cause's behind a colon when there is one (the MCP client wraps the spawn or startup error
 * in `cause`), with the harness's `mcp-client(nanomuse): ` prefix dropped.
 */
export function mountError(error: unknown): string {
  const text = (e: unknown): string => (e instanceof Error ? e.message : String(e)).replace(/^mcp-client\([^)]*\):\s*/, '').trim()
  const message = text(error) || 'the hands did not mount'
  const cause = error instanceof Error && error.cause !== undefined ? text(error.cause) : ''
  return cause && !message.includes(cause) ? `${message}: ${cause}` : message
}

export function apply(ctx: Context): void {
  const cloud = ctx.nanomuseCloud
  let mounted: { key: string; fiber: Fiber } | undefined
  let disposed = false
  let pending: Promise<void> = Promise.resolve()

  // the Computer-use page and the Connectors row say why the hands are off: the mount's own words
  const failed = (error: unknown) => {
    const reason = mountError(error)
    ctx.logger.warn('nanomuse hands: mount: %s', reason)
    cloud.handsMounted({ ok: false, reason })
  }

  const mount = (hands: Record<string, string>, key: string) => {
    let fiber: Fiber
    try {
      fiber = ctx.plugin(mcpClient as unknown as Parameters<Context['plugin']>[0], clientConfig(hands))
    } catch (error) {
      failed(error)
      return
    }
    mounted = { key, fiber }
    ctx.logger.info('nanomuse hands: runtime started%s', hands.NANOMUSE_GUI_MODEL ? ` with ${hands.NANOMUSE_GUI_PROVIDER === 'chatgpt' ? 'chatgpt' : hands.NANOMUSE_GUI_BASE_URL || 'openai'} · ${hands.NANOMUSE_GUI_MODEL}` : ' without a hands model')
    // the client's start settles later: a config it refused, or a startup it was told to fail on
    void fiber.await().then(
      () => {
        if (mounted?.fiber === fiber) cloud.handsMounted({ ok: true })
      },
      (error: unknown) => {
        if (mounted?.fiber === fiber) failed(error)
      },
    )
  }

  const sync = async () => {
    if (disposed) return
    const hands = await cloud.handsEnv().catch((error: unknown) => {
      ctx.logger.warn('nanomuse hands: environment: %s', error instanceof Error ? error.message : String(error))
      // nothing mounted yet: this is why the hands are off
      if (!mounted) cloud.handsMounted({ ok: false, reason: `the hands' environment could not be read: ${error instanceof Error ? error.message : String(error)}` })
      return undefined
    })
    if (!hands || disposed) return
    const key = JSON.stringify(hands)
    if (mounted?.key === key) return
    // a hands call is in flight: the new model waits; the call's end is a change too, so this runs again
    if (mounted && cloud.handsBusy()) return
    if (mounted) {
      const old = mounted
      mounted = undefined
      await old.fiber.dispose().catch((error: unknown) => ctx.logger.warn('nanomuse hands: unmount: %s', error instanceof Error ? error.message : String(error)))
      if (disposed) return
    }
    mount(hands, key)
  }

  const schedule = () => {
    pending = pending.then(sync).catch(() => undefined)
  }

  // the service broadcasts often (a hands step, a stream); one look at the environment per burst
  let timer: NodeJS.Timeout | undefined
  const later = () => {
    if (timer) clearTimeout(timer)
    timer = setTimeout(() => {
      timer = undefined
      schedule()
    }, 300)
  }

  schedule()
  ctx.effect(() => cloud.onChange(later), 'nanomuse hands: follow the model')
  ctx.effect(() => () => {
    disposed = true
    if (timer) clearTimeout(timer)
  })
}
