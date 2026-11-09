import { createHash } from 'node:crypto'
import { spawn } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { ENGINE_VERSION, validMessageDelivery, validStart } from './engine.mjs'
import { MAX_FRAME_BYTES } from './ipc.mjs'

const ID = /^[A-Za-z0-9_-]{1,128}$/
const MAX_CONCURRENT_TASKS = 16
const SHUTDOWN_TIMEOUT_MS = 5_000
// Startup can be delayed by concurrent tasks or a busy host. Keep a firm
// upper bound, but avoid treating a healthy DSH child as unavailable after
// only five seconds.
const TASK_START_TIMEOUT_MS = 15_000
const TASK_STOP_TIMEOUT_MS = 2_000
const TASK_FENCE_FIELDS = ['task_id', 'owner_id', 'agent_id', 'session_id', 'generation', 'authorization_version']
const RUNTIME_FIELDS = ['server_instance_id', 'owner_id', 'runtime_id', 'runtime_generation', 'max_concurrent_tasks']
const RUNTIME_START_KEYS = ['type', 'version', ...RUNTIME_FIELDS]
const RUNTIME_SHUTDOWN_KEYS = ['type', 'version', ...RUNTIME_FIELDS]
const TASK_PROCESS_ADMIT_KEYS = ['type', 'server_instance_id', 'runtime_id', 'runtime_generation', 'runtime_sequence', ...TASK_FENCE_FIELDS]
const ACCOUNT_START_KEYS = [
  'type', 'version', ...TASK_FENCE_FIELDS, 'server_instance_id', 'runtime_id', 'runtime_generation',
  'runtime_sequence', 'model', 'tools', 'prompt', 'initial_message', 'initial_context', 'revision_messages', 'resume', 'checkpoint_through_seq', 'checkpoint_sha256', 'max_tokens', 'background_jobs',
]
const UTF8_DECODER = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true })

function deferred() {
  let resolve
  let reject
  const promise = new Promise((ok, fail) => { resolve = ok; reject = fail })
  void promise.catch(() => {})
  return { promise, resolve, reject }
}

function timeout(promise, ms) {
  let timer
  return Promise.race([
    promise,
    new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('task process timed out')), ms) }),
  ]).finally(() => clearTimeout(timer))
}

function taskEnvironment() {
  const allowed = new Set(['PATH', 'LANG', 'LC_ALL', 'TZ', 'NODE_ENV', '__CF_USER_TEXT_ENCODING', 'UV_USE_IO_URING'])
  return Object.fromEntries(Object.entries(process.env).filter(([key]) => allowed.has(key)))
}

function exactKeys(value, expected) {
  return Object.keys(value).sort().join(',') === [...expected].sort().join(',')
}

function validRuntimeStart(frame) {
  return exactKeys(frame, RUNTIME_START_KEYS) && frame.type === 'runtime_start' && frame.version === 1 &&
    RUNTIME_FIELDS.slice(0, 3).every(key => typeof frame[key] === 'string' && ID.test(frame[key])) &&
    Number.isSafeInteger(frame.runtime_generation) && frame.runtime_generation > 0 &&
    Number.isSafeInteger(frame.max_concurrent_tasks) && frame.max_concurrent_tasks > 0 &&
    frame.max_concurrent_tasks <= MAX_CONCURRENT_TASKS
}

function runtimeFields(runtime) {
  return {
    server_instance_id: runtime.server_instance_id,
    owner_id: runtime.owner_id,
    runtime_id: runtime.runtime_id,
    runtime_generation: runtime.runtime_generation,
  }
}

function taskFence(frame) {
  return Object.fromEntries(TASK_FENCE_FIELDS.map(key => [key, frame[key]]))
}

function validTaskFence(fence) {
  return ['task_id', 'owner_id', 'agent_id', 'session_id'].every(key => typeof fence[key] === 'string' && ID.test(fence[key])) &&
    ['generation', 'authorization_version'].every(key => Number.isSafeInteger(fence[key]) && fence[key] > 0)
}

function taskKey(fence) {
  return JSON.stringify(TASK_FENCE_FIELDS.map(key => fence[key]))
}

function validAccountStart(frame, runtime) {
  const start = { ...frame, type: 'start' }
  return frame.type === 'task_start' && Object.keys(frame).every(key => ACCOUNT_START_KEYS.includes(key)) && validStart(start) &&
    Number.isSafeInteger(frame.runtime_sequence) && frame.runtime_sequence > 0 &&
    frame.owner_id === runtime.owner_id && frame.server_instance_id === runtime.server_instance_id &&
    frame.runtime_id === runtime.runtime_id && frame.runtime_generation === runtime.runtime_generation
}

function frameBytes(frame) {
  const bytes = Buffer.from(`${JSON.stringify(frame)}\n`)
  if (bytes.length > MAX_FRAME_BYTES) throw new Error('runtime frame exceeds limit')
  return bytes
}

function canonicalJSON(value) {
  if (Array.isArray(value)) return `[${value.map(canonicalJSON).join(',')}]`
  if (value && typeof value === 'object') {
    return `{${Object.keys(value).sort().map(key => `${JSON.stringify(key)}:${canonicalJSON(value[key])}`).join(',')}}`
  }
  return JSON.stringify(value)
}

function startDigest(frame) {
  return createHash('sha256').update(canonicalJSON(frame)).digest('hex')
}

export class AccountRuntime {
  constructor(input, output) {
    this.input = input
    this.output = output
    this.runtime = undefined
    this.tasks = new Map()
    this.activeTaskIDs = new Set()
    this.lastRuntimeSequence = 0
    this.lastAcceptedStart = undefined
    this.buffer = Buffer.alloc(0)
    this.discardingOversizedFrame = false
    this.writes = Promise.resolve()
    this.frames = Promise.resolve()
    this.stopping = false
    this.stopReason = undefined
    this.stopTimer = undefined
    this.stopSent = false
    this.resolveRun = undefined
    this.rejectRun = undefined
  }

  run() {
    return new Promise((resolve, reject) => {
      this.resolveRun = resolve
      this.rejectRun = reject
      this.input.on('data', chunk => this.receive(chunk))
      this.input.once('end', () => this.shutdown('input_closed'))
      this.input.once('error', () => this.shutdown('input_failed'))
    })
  }

  receive(chunk) {
    const incoming = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk)
    this.buffer = Buffer.concat([this.buffer, incoming])
    for (;;) {
      const end = this.buffer.indexOf(10)
      if (this.discardingOversizedFrame) {
        if (end < 0) {
          this.buffer = Buffer.alloc(0)
          return
        }
        this.buffer = this.buffer.subarray(end + 1)
        this.discardingOversizedFrame = false
        continue
      }
      if (end < 0) {
        if (this.buffer.length >= MAX_FRAME_BYTES) {
          this.buffer = Buffer.alloc(0)
          this.discardingOversizedFrame = true
          this.queueProtocolError('frame_too_large')
        }
        return
      }
      const line = this.buffer.subarray(0, end)
      this.buffer = this.buffer.subarray(end + 1)
      if (end >= MAX_FRAME_BYTES) {
        this.queueProtocolError('frame_too_large')
        continue
      }
      this.frames = this.frames.then(() => this.handleLine(line)).catch(() => {
        if (!this.runtime) this.failStartup()
        else this.queueProtocolError('invalid_frame')
      })
    }
  }

  async handleLine(line) {
    let frame
    try {
      frame = JSON.parse(UTF8_DECODER.decode(line))
    } catch {
      if (!this.runtime) return this.failStartup()
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: 'invalid_frame' })
      return
    }
    if (!frame || typeof frame !== 'object' || Array.isArray(frame) || typeof frame.type !== 'string') {
      if (!this.runtime) return this.failStartup()
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: 'invalid_frame' })
      return
    }
    if (!this.runtime) {
      if (!validRuntimeStart(frame)) return this.failStartup()
      this.runtime = frame
      await this.send({ ...frame, type: 'runtime_ready', engine_version: ENGINE_VERSION })
      return
    }
    if (frame.type === 'runtime_start') {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: 'duplicate_runtime_start' })
      return
    }
    if (frame.type === 'runtime_shutdown') {
      if (!exactKeys(frame, RUNTIME_SHUTDOWN_KEYS) || frame.version !== 1 ||
          !RUNTIME_FIELDS.every(key => frame[key] === this.runtime[key])) {
        await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: 'runtime_fence_mismatch' })
        return
      }
      this.shutdown('requested')
      return
    }
    if (frame.type === 'task_process_admit') {
      await this.admitTaskProcess(frame)
      return
    }
    if (frame.type === 'task_start') {
      await this.startTask(frame)
      return
    }
    await this.routeTaskFrame(frame)
  }

  async startTask(frame) {
    if (this.stopping) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: 'runtime_stopping' })
      return
    }
    const fence = taskFence(frame)
    if (!validTaskFence(fence) || !validAccountStart(frame, this.runtime)) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: frame.owner_id !== this.runtime.owner_id ? 'owner_mismatch' : 'invalid_task_start' })
      return
    }
    const key = taskKey(fence)
    const digest = startDigest(frame)
    if (frame.runtime_sequence <= this.lastRuntimeSequence) {
      if (frame.runtime_sequence < this.lastRuntimeSequence) {
        await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: 'stale_runtime_sequence' })
        return
      }
      const previous = this.lastAcceptedStart
      if (!previous || previous.digest !== digest) {
        await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: 'runtime_sequence_conflict' })
        return
      }
      if (!this.tasks.has(previous.key)) {
        await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: 'task_start_already_settled' })
        return
      }
      await this.send({ type: 'task_started', ...runtimeFields(this.runtime), ...fence })
      return
    }
    if (this.activeTaskIDs.has(fence.task_id)) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: 'duplicate_task' })
      return
    }
    if (this.tasks.size >= this.runtime.max_concurrent_tasks) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: 'task_capacity_exceeded' })
      return
    }

    const task = {
      fence, key, runtimeSequence: frame.runtime_sequence, outputBuffer: Buffer.alloc(0),
      outputFrames: Promise.resolve(), ready: deferred(), started: deferred(), spawned: deferred(),
      closed: deferred(), finished: deferred(), accepted: false, terminal: false, terminating: false,
      terminated: false, startFrame: frame, admitted: false,
    }
    this.tasks.set(key, task)
    this.activeTaskIDs.add(fence.task_id)
    try {
      task.child = spawn(process.execPath, [fileURLToPath(new URL('../bin.mjs', import.meta.url))], {
        cwd: fileURLToPath(new URL('..', import.meta.url)),
        env: taskEnvironment(),
        detached: process.platform !== 'win32',
        stdio: ['pipe', 'pipe', 'ignore'],
      })
      task.child.once('spawn', () => task.spawned.resolve())
      task.child.once('error', error => task.spawned.reject(error))
      task.child.stdout.on('data', chunk => this.receiveTaskOutput(task, chunk))
      task.child.once('close', (code, signal) => {
        task.exit = { code, signal }
        if (!task.readyReceived && !task.accepted) task.startFailed = true
        task.closed.resolve(task.exit)
        task.ready.reject(new Error('task process closed before ready'))
        void this.finishClosedTask(task)
      })
      task.child.stdin.on('error', () => {})
      await timeout(task.spawned.promise, TASK_START_TIMEOUT_MS)
      this.lastRuntimeSequence = frame.runtime_sequence
      task.startDigest = digest
      await this.send({ type: 'task_process_ready', ...runtimeFields(this.runtime), runtime_sequence: frame.runtime_sequence,
        ...fence, container_pid: task.child.pid })
    } catch {
      task.startFailed = !task.accepted
      task.startDispatchFailed = task.accepted
      if (task.startDispatchFailed) {
        task.terminating = true
        task.terminateReason = 'task_start_dispatch_failed'
      }
      task.started.resolve()
      await this.stopTaskProcess(task).catch(() => {})
      await timeout(task.finished.promise, TASK_STOP_TIMEOUT_MS).catch(() => {})
    }
  }

  async admitTaskProcess(frame) {
    const fence = taskFence(frame)
    if (!exactKeys(frame, TASK_PROCESS_ADMIT_KEYS) || !validTaskFence(fence) ||
        frame.server_instance_id !== this.runtime.server_instance_id || frame.owner_id !== this.runtime.owner_id ||
        frame.runtime_id !== this.runtime.runtime_id || frame.runtime_generation !== this.runtime.runtime_generation) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: 'invalid_task_process_admission' })
      return
    }
    const task = this.tasks.get(taskKey(fence))
    if (!task || task.runtimeSequence !== frame.runtime_sequence || task.admitted || task.accepted) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), ...fence, code: 'task_process_admission_mismatch' })
      return
    }
    task.admitted = true
    void this.finishTaskStart(task).catch(() => this.terminateTask(task, 'task_start_dispatch_failed'))
  }

  async finishTaskStart(task) {
    try {
      await task.spawned.promise
      const { server_instance_id, runtime_id, runtime_generation, runtime_sequence, ...startFrame } = task.startFrame
      await this.writeTaskInput(task, { ...startFrame, type: 'start' })
      await timeout(task.ready.promise, TASK_START_TIMEOUT_MS)
      if (task.exit) throw new Error('task process exited before readiness')
      this.lastAcceptedStart = { digest: task.startDigest, key: task.key }
      task.accepted = true
      await this.send({ type: 'task_started', ...runtimeFields(this.runtime), ...task.fence })
      task.started.resolve()
    } catch {
      task.startFailed = !task.accepted
      task.startDispatchFailed = task.accepted
      if (task.startDispatchFailed) {
        task.terminating = true
        task.terminateReason = 'task_start_dispatch_failed'
      }
      task.started.resolve()
      await this.stopTaskProcess(task).catch(() => {})
      await timeout(task.finished.promise, TASK_STOP_TIMEOUT_MS).catch(() => {})
    }
  }

  async routeTaskFrame(frame) {
    const fence = taskFence(frame)
    if (!validTaskFence(fence)) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: 'task_fence_required' })
      return
    }
    if (frame.owner_id !== this.runtime.owner_id) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: 'owner_mismatch' })
      return
    }
    const accountScope = ['server_instance_id', 'runtime_id', 'runtime_generation']
    if (accountScope.some(key => Object.hasOwn(frame, key)) &&
        (frame.server_instance_id !== this.runtime.server_instance_id || frame.runtime_id !== this.runtime.runtime_id ||
          frame.runtime_generation !== this.runtime.runtime_generation)) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: 'runtime_fence_mismatch' })
      return
    }
    const task = [...this.tasks.values()].find(item => item.fence.task_id === fence.task_id)
    if (task && (task.fence.owner_id !== fence.owner_id || task.fence.agent_id !== fence.agent_id ||
        task.fence.generation !== fence.generation || task.fence.authorization_version !== fence.authorization_version)) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), ...fence, code: 'task_fence_mismatch' })
      return
    }
    if (!task) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: 'unknown_task' })
      return
    }
    for (const key of accountScope) delete frame[key]
    if (frame.type === 'message_delivery' && !validMessageDelivery(frame, fence)) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), ...fence, code: 'invalid_message_delivery' })
      return
    }
    if (frame.type === 'cancel' && !exactKeys(frame, ['type', ...TASK_FENCE_FIELDS])) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: 'invalid_cancel' })
      return
    }
    if (frame.type === 'cancel' && task.fence.session_id !== fence.session_id) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), ...fence, code: 'task_fence_mismatch' })
      return
    }
    await this.writeTaskInput(task, frame)
  }

  async writeTaskInput(task, frame) {
    if (!task.child?.stdin || task.child.stdin.destroyed || task.exit) throw new Error('task process input is unavailable')
    await new Promise((resolve, reject) => {
      task.child.stdin.write(frameBytes(frame), error => error ? reject(error) : resolve())
    })
  }

  receiveTaskOutput(task, chunk) {
    if (task.terminated) return
    task.outputBuffer = Buffer.concat([task.outputBuffer, Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk)])
    for (;;) {
      const end = task.outputBuffer.indexOf(10)
      if (end < 0) {
        if (task.outputBuffer.length >= MAX_FRAME_BYTES) this.terminateTask(task, 'internal_frame_too_large')
        return
      }
      const line = task.outputBuffer.subarray(0, end)
      task.outputBuffer = task.outputBuffer.subarray(end + 1)
      if (end >= MAX_FRAME_BYTES) {
        this.terminateTask(task, 'internal_frame_too_large')
        return
      }
      task.outputFrames = task.outputFrames.then(async () => {
        let frame
        try {
          frame = JSON.parse(UTF8_DECODER.decode(line))
        } catch {
          return this.terminateTask(task, 'invalid_internal_frame')
        }
        if (!frame || typeof frame !== 'object' || Array.isArray(frame) || typeof frame.type !== 'string') {
          throw new Error('invalid internal task frame')
        }
        if (!task.readyReceived) {
          if (Object.keys(frame).sort().join(',') !== 'engine,engine_version,type,version' || frame.type !== 'ready' ||
              frame.version !== 1 || frame.engine !== 'dsh' || frame.engine_version !== ENGINE_VERSION) {
            throw new Error('invalid task readiness frame')
          }
          task.readyReceived = true
          task.ready.resolve()
          return
        }
        if (frame.type === 'ready') throw new Error('duplicate task readiness frame')
        await task.started.promise
        if (task.startDispatchFailed) return
        if (frame.type !== 'failed' && (!validTaskFence({ ...task.fence, session_id: frame.session_id }) ||
            TASK_FENCE_FIELDS.some(key => key !== 'session_id' && frame[key] !== task.fence[key]) ||
            (frame.type === 'settled' && frame.session_id !== task.fence.session_id))) {
          throw new Error('invalid task output fence')
        }
        if (frame.type === 'settled' || frame.type === 'failed') {
          if (task.terminalFrame) throw new Error('duplicate task terminal frame')
          task.terminalFrame = frame
          task.terminal = true
          return
        }
        await this.send({ ...frame, ...runtimeFields(this.runtime), ...task.fence, session_id: frame.session_id })
      }).catch(() => this.terminateTask(task, 'output_failed'))
    }
  }

  async finishClosedTask(task) {
    if (task.finishing) return task.finished.promise
    task.finishing = true
    await task.outputFrames
    if (task.startFailed) {
      this.finishTask(task, false)
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: 'task_runtime_unavailable' }).catch(() => {})
      this.finishShutdownIfIdle()
    } else if (task.terminalFrame) {
      this.finishTask(task, false)
      await this.send({ ...task.terminalFrame, ...runtimeFields(this.runtime), ...task.fence }).catch(() => {})
      this.finishShutdownIfIdle()
    } else if (task.terminating || !task.terminal) {
      this.finishTask(task, false)
      // shortcut: direct child exit is not cgroup proof; Guardian must verify all descendants before production stop claims.
      await this.send({
        type: 'task_terminated', ...runtimeFields(this.runtime), ...task.fence, state: 'unknown',
        reason: task.terminateReason ?? 'child_exited_without_result', process_exited: true,
      }).catch(() => {})
      this.finishShutdownIfIdle()
    } else {
      this.finishTask(task)
    }
    task.terminated = true
    task.finished.resolve()
  }

  terminateTask(task, reason) {
    if (task.terminated || task.terminating) return task.finished.promise
    task.terminating = true
    task.terminateReason = reason
    task.stopPromise = this.stopTaskProcess(task).catch(() => {
      void this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: 'task_stop_unconfirmed' }).catch(() => {})
    })
    return task.stopPromise
  }

  async stopTaskProcess(task) {
    if (task.exit) return task.exit
    const spawned = await Promise.race([task.spawned.promise.then(() => true), task.closed.promise.then(() => false)])
    if (!spawned || task.exit) return task.exit
    const signalGroup = signal => {
      if (process.platform === 'win32') {
        if (!task.child.kill(signal) && !task.exit) throw new Error('task process could not be signalled')
        return
      }
      try {
        process.kill(-task.child.pid, signal)
      } catch (error) {
        if (error.code !== 'ESRCH') throw error
      }
    }
    signalGroup('SIGTERM')
    try {
      return await timeout(task.closed.promise, 250)
    } catch {
      signalGroup('SIGKILL')
      return timeout(task.closed.promise, TASK_STOP_TIMEOUT_MS)
    }
  }

  finishTask(task, checkShutdown = true) {
    if (this.tasks.get(task.key) !== task) return
    this.tasks.delete(task.key)
    this.activeTaskIDs.delete(task.fence.task_id)
    if (checkShutdown) this.finishShutdownIfIdle()
  }

  queueProtocolError(code) {
    if (!this.runtime) {
      this.failStartup()
      return
    }
    this.frames = this.frames.then(() => this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code })).catch(() => {})
  }

  shutdown(reason) {
    if (!this.runtime) {
      this.failStartup()
      return
    }
    if (this.stopping) return
    this.stopping = true
    this.stopReason = reason
    for (const task of this.tasks.values()) {
      void this.writeTaskInput(task, { type: 'cancel', ...task.fence }).catch(() => this.terminateTask(task, 'cancel_failed'))
    }
    this.stopTimer = setTimeout(() => {
      for (const task of this.tasks.values()) void this.terminateTask(task, 'runtime_shutdown_timeout')
    }, SHUTDOWN_TIMEOUT_MS)
    this.stopTimer.unref?.()
    this.finishShutdownIfIdle()
  }

  finishShutdownIfIdle() {
    if (!this.stopping || this.tasks.size !== 0 || this.stopSent) return
    this.stopSent = true
    clearTimeout(this.stopTimer)
    void this.send({ type: 'runtime_stopped', ...runtimeFields(this.runtime), reason: this.stopReason })
      .then(() => this.resolveRun?.())
      .catch(error => this.rejectRun?.(error))
  }

  async send(frame) {
    const encoded = frameBytes(frame)
    const writing = this.writes.then(async () => {
      if (!this.output.write(encoded)) await new Promise(resolve => this.output.once('drain', resolve))
    })
    this.writes = writing.catch(() => {})
    return writing
  }

  failStartup() {
    if (!this.rejectRun) return
    const reject = this.rejectRun
    this.rejectRun = undefined
    reject(new Error('invalid account runtime protocol'))
  }
}

export async function runAccountRuntime(input, output) {
  return new AccountRuntime(input, output).run()
}
