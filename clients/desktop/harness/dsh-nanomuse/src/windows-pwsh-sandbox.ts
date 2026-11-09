/**
 * Repair the Windows ACL runner only when DeepSeek Harness is hosted by Electron.
 *
 * Adapted from the MIT-licensed DSH Desktop PowerShell adapter:
 * https://github.com/anywhere-labs/dsh-desktop
 */

import { fileURLToPath } from 'node:url'
import type { ShellExecSpec, ShellExecution } from '@deepseek-ai/dsh-shell'
import { SandboxPwshExecutor } from '@deepseek-ai/dsh-pwsh-sandbox'
import type { Config as PwshConfig } from '@deepseek-ai/dsh-pwsh-local'

const RUN_AS_NODE = 'ELECTRON_RUN_AS_NODE'
const UPSTREAM_RUNNER = fileURLToPath(
  import.meta.resolve('@deepseek-ai/dsh-sandbox-windows-acl/runner'),
)
const NANOMUSE_TRAMPOLINE = fileURLToPath(
  new URL('./windows-acl-runner.js', import.meta.url),
)

export interface WindowsAclAdaptation {
  platform: NodeJS.Platform
  electron: boolean
  execPath: string
  upstreamRunner: string
  trampoline: string
}

export interface AdaptedWindowsAclExecution {
  spec: ShellExecSpec
  argv: readonly string[]
}

/**
 * Insert the nanoMuse trampoline only around the exact Electron → ACL-runner invocation.
 * Direct PowerShell and every non-Windows execution stay unchanged.
 */
export function adaptWindowsAclExecution(
  spec: ShellExecSpec,
  argv: readonly string[],
  adaptation: WindowsAclAdaptation,
): AdaptedWindowsAclExecution {
  const [program, runner, ...args] = argv
  if (
    adaptation.platform !== 'win32' ||
    !adaptation.electron ||
    program !== adaptation.execPath ||
    runner !== adaptation.upstreamRunner
  ) {
    return { spec, argv }
  }

  const env = { ...spec.env }
  for (const key of Object.keys(env)) {
    if (key.toUpperCase() === RUN_AS_NODE) delete env[key]
  }
  env[RUN_AS_NODE] = '1'
  return {
    spec: { ...spec, env },
    argv: [
      adaptation.execPath,
      adaptation.trampoline,
      adaptation.upstreamRunner,
      ...args,
    ],
  }
}

/** PowerShell sandbox provider for nanoMuse Desktop's Electron-hosted Windows process. */
export class NanomuseWindowsPwshSandbox extends SandboxPwshExecutor {
  constructor(
    ctx: ConstructorParameters<typeof SandboxPwshExecutor>[0],
    config: PwshConfig,
  ) {
    super(ctx, config)
  }

  private adapt(
    spec: ShellExecSpec,
    argv: readonly string[],
  ): AdaptedWindowsAclExecution {
    return adaptWindowsAclExecution(spec, argv, {
      platform: process.platform,
      electron: process.versions.electron !== undefined,
      execPath: process.execPath,
      upstreamRunner: UPSTREAM_RUNNER,
      trampoline: NANOMUSE_TRAMPOLINE,
    })
  }

  protected override executeArgv(
    spec: ShellExecSpec,
    argvOrPrepare: readonly string[] | ((signal: AbortSignal) => Promise<readonly string[]>),
    onStarted?: (process: ShellExecution) => void,
  ): Promise<ShellExecution> {
    if (typeof argvOrPrepare !== 'function') {
      const adapted = this.adapt(spec, argvOrPrepare)
      return super.executeArgv(adapted.spec, adapted.argv, onStarted)
    }
    const pending: ShellExecSpec = { ...spec }
    return super.executeArgv(
      pending,
      async (signal) => {
        const adapted = this.adapt(spec, await argvOrPrepare(signal))
        pending.env = adapted.spec.env
        return adapted.argv
      },
      onStarted,
    )
  }
}

export default NanomuseWindowsPwshSandbox
