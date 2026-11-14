import { createHash } from 'node:crypto'
import { spawn } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { ENGINE_VERSION, validMessageDelivery, validStart } from './engine.mjs'
import { MAX_FRAME_BYTES } from './ipc.mjs'

const ID = /^[A-Za-z0-9_-]{1,128}$/
const MAX_CONCURRENT_TASKS = 16
const MAX_CONCURRENT_COORDINATORS = 16
const MAX_COORDINATOR_MESSAGE_HISTORY = 4096
const SHUTDOWN_TIMEOUT_MS = 5_000
// Startup can be delayed by concurrent tasks or a busy host. Keep a firm
// upper bound, but avoid treating a healthy DSH child as unavailable after
// only five seconds.
const TASK_START_TIMEOUT_MS = 15_000
const TASK_STOP_TIMEOUT_MS = 2_000
const TASK_FENCE_FIELDS = ['task_id', 'owner_id', 'agent_id', 'session_id', 'generation', 'authorization_version']
const COORDINATOR_FENCE_FIELDS = ['context_role', 'conversation_id', 'message_id', 'owner_id', 'actor_id', 'agent_id', 'generation', 'authorization_version', 'session_id', 'purpose', 'operation_id']
const RUNTIME_FIELDS = ['server_instance_id', 'owner_id', 'runtime_id', 'runtime_generation', 'max_concurrent_tasks']
const RUNTIME_START_KEYS = ['type', 'version', ...RUNTIME_FIELDS]
const RUNTIME_SHUTDOWN_KEYS = ['type', 'version', ...RUNTIME_FIELDS]
const TASK_PROCESS_ADMIT_KEYS = ['type', 'server_instance_id', 'runtime_id', 'runtime_generation', 'runtime_sequence', ...TASK_FENCE_FIELDS]
const ACCOUNT_START_KEYS = [
  'type', 'version', ...TASK_FENCE_FIELDS, 'server_instance_id', 'runtime_id', 'runtime_generation',
  'runtime_sequence', 'model', 'tools', 'prompt', 'initial_message', 'initial_context', 'revision_messages', 'resume', 'checkpoint_through_seq', 'checkpoint_sha256', 'max_tokens', 'background_jobs', 'step_retry',
]
const COORDINATOR_START_KEYS = [
  'type', 'version', ...COORDINATOR_FENCE_FIELDS, 'server_instance_id', 'runtime_id', 'runtime_generation',
  'runtime_sequence', 'model', 'tools', 'system_prompt', 'prompt', 'resume',
  'checkpoint_through_seq', 'checkpoint_sha256', 'max_tokens', 'context_window_tokens', 'source_messages', 'previous_summary',
]
const COORDINATOR_PROCESS_ADMIT_KEYS = [
  'type', 'server_instance_id', 'owner_id', 'runtime_id', 'runtime_generation', 'runtime_sequence',
  ...COORDINATOR_FENCE_FIELDS.filter(key => key !== 'owner_id'),
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

function coordinatorFence(frame) {
  return Object.fromEntries(COORDINATOR_FENCE_FIELDS.map(key => [key, frame[key]]))
}

function validCoordinatorFence(fence) {
  return fence.context_role === 'coordinator' &&
    ['conversation_id', 'message_id', 'owner_id', 'actor_id', 'agent_id', 'session_id'].every(key => typeof fence[key] === 'string' && ID.test(fence[key])) &&
    ['conversation', 'compaction'].includes(fence.purpose) &&
    (fence.purpose === 'conversation' ? fence.operation_id === '' : typeof fence.operation_id === 'string' && ID.test(fence.operation_id)) &&
    /^[a-z0-9]{1,128}$/.test(fence.message_id) &&
    ['generation', 'authorization_version'].every(key => Number.isSafeInteger(fence[key]) && fence[key] > 0)
}

function coordinatorMessageKey(fence) {
  return JSON.stringify([fence.conversation_id, fence.session_id, fence.message_id])
}

function coordinatorStartContentDigest(frame) {
  const content = { ...frame }
  for (const field of ['runtime_sequence', 'resume', 'checkpoint_through_seq', 'checkpoint_sha256', 'prompt']) delete content[field]
  return startDigest(content)
}

function validAccountStart(frame, runtime) {
  const start = { ...frame, type: 'start' }
  return frame.type === 'task_start' && Object.keys(frame).every(key => ACCOUNT_START_KEYS.includes(key)) && validStart(start) &&
    Number.isSafeInteger(frame.runtime_sequence) && frame.runtime_sequence > 0 &&
    frame.owner_id === runtime.owner_id && frame.server_instance_id === runtime.server_instance_id &&
    frame.runtime_id === runtime.runtime_id && frame.runtime_generation === runtime.runtime_generation
}

function validAccountCoordinatorStart(frame, runtime) {
  const start = { ...frame, type: 'start' }
  return Object.keys(frame).every(key => COORDINATOR_START_KEYS.includes(key)) && frame.type === 'coordinator_start' && frame.version === 1 &&
    validCoordinatorFence(coordinatorFence(frame)) && Array.isArray(frame.tools) && frame.tools.length === 0 &&
    validStart(start) && Number.isSafeInteger(frame.runtime_sequence) && frame.runtime_sequence > 0 &&
    frame.owner_id === runtime.owner_id && frame.server_instance_id === runtime.server_instance_id &&
    frame.runtime_id === runtime.runtime_id && frame.runtime_generation === runtime.runtime_generation
}

function validCoordinatorChildFrame(frame, identity) {
  const base = ['type', ...COORDINATOR_FENCE_FIELDS]
  if (!validCoordinatorFence(coordinatorFence(frame)) || COORDINATOR_FENCE_FIELDS.some(key => frame[key] !== identity[key])) return false
  if (frame.type === 'checkpoint') {
    return exactKeys(frame, [...base, 'id', 'from_seq', 'through_seq', 'payload', 'sha256']) &&
      typeof frame.id === 'string' && frame.id.length > 0 && Number.isSafeInteger(frame.from_seq) && frame.from_seq >= 0 &&
      Number.isSafeInteger(frame.through_seq) && frame.through_seq >= frame.from_seq && typeof frame.payload === 'string' &&
      Buffer.byteLength(frame.payload) <= MAX_FRAME_BYTES / 2 && /^[a-f0-9]{64}$/.test(frame.sha256)
  }
  if (frame.type === 'checkpoint_read') {
    return exactKeys(frame, [...base, 'id', 'from_seq']) && typeof frame.id === 'string' && frame.id.length > 0 &&
      Number.isSafeInteger(frame.from_seq) && frame.from_seq >= 0
  }
  if (frame.type === 'model') {
    return exactKeys(frame, [...base, 'id', 'through_seq', 'request', 'sha256']) && typeof frame.id === 'string' && frame.id.length > 0 &&
      Number.isSafeInteger(frame.through_seq) && frame.through_seq >= 0 && frame.request && typeof frame.request === 'object' &&
      !Array.isArray(frame.request) && /^[a-f0-9]{64}$/.test(frame.sha256)
  }
  if (frame.type === 'model_cancel') return exactKeys(frame, [...base, 'id']) && typeof frame.id === 'string' && frame.id.length > 0
  if (frame.type === 'coordinator_settled') {
    return exactKeys(frame, [...base, 'through_seq', 'result']) && Number.isSafeInteger(frame.through_seq) && frame.through_seq >= 0 &&
      typeof frame.result === 'string' && frame.result.isWellFormed() &&
      Buffer.byteLength(frame.result, 'utf8') <= (identity.purpose === 'compaction' ? 256 * 1024 : 4096)
  }
  if (frame.type === 'coordinator_cancelled') {
    return exactKeys(frame, [...base, 'through_seq']) && Number.isSafeInteger(frame.through_seq) && frame.through_seq >= -1
  }
  return false
}

function validCoordinatorResponseFrame(frame, identity) {
  const base = ['type', ...COORDINATOR_FENCE_FIELDS]
  if (!validCoordinatorFence(coordinatorFence(frame)) || COORDINATOR_FENCE_FIELDS.some(key => frame[key] !== identity[key])) return false
  if (frame.type === 'checkpoint_ack') {
    return exactKeys(frame, [...base, 'id', 'ok', 'through_seq', 'sha256']) && typeof frame.id === 'string' &&
      typeof frame.ok === 'boolean' && Number.isSafeInteger(frame.through_seq) && frame.through_seq >= 0 && /^[a-f0-9]{64}$/.test(frame.sha256)
  }
  if (frame.type === 'checkpoint_page') {
    return exactKeys(frame, [...base, 'id', 'ok', 'from_seq', 'through_seq', 'payload', 'sha256']) && typeof frame.id === 'string' &&
      typeof frame.ok === 'boolean' && Number.isSafeInteger(frame.from_seq) && frame.from_seq >= 0 && Number.isSafeInteger(frame.through_seq) &&
      frame.through_seq >= frame.from_seq && typeof frame.payload === 'string' && Buffer.byteLength(frame.payload) <= MAX_FRAME_BYTES / 2 &&
      /^[a-f0-9]{64}$/.test(frame.sha256)
  }
  if (frame.type === 'model_chunk') {
    return exactKeys(frame, [...base, 'id', 'index', 'chunk']) && typeof frame.id === 'string' && Number.isSafeInteger(frame.index) &&
      frame.index >= 0 && frame.chunk && typeof frame.chunk === 'object' && !Array.isArray(frame.chunk)
  }
  if (frame.type === 'model_end') {
    return exactKeys(frame, [...base, 'id', 'ok']) && typeof frame.id === 'string' && typeof frame.ok === 'boolean'
  }
  return false
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
    this.coordinators = new Map()
    this.coordinatorMessages = new Map()
    this.activeTaskIDs = new Set()
    this.lastRuntimeSequence = 0
    this.lastAcceptedStart = undefined
    this.lastCoordinatorSequence = 0
    this.lastAcceptedCoordinatorStart = undefined
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
    if (frame.type === 'coordinator_process_admit') {
      await this.admitCoordinatorProcess(frame)
      return
    }
    if (frame.type === 'coordinator_start') {
      await this.startCoordinator(frame)
      return
    }
    if (frame.type === 'coordinator_cancel') {
      await this.routeCoordinatorFrame(frame)
      return
    }
    if (frame.context_role === 'coordinator') {
      await this.routeCoordinatorFrame(frame)
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

  async startCoordinator(frame) {
    if (this.stopping) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: 'runtime_stopping' })
      return
    }
    const fence = coordinatorFence(frame)
    if (!validCoordinatorFence(fence) || !validAccountCoordinatorStart(frame, this.runtime)) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: frame.owner_id !== this.runtime.owner_id ? 'owner_mismatch' : 'invalid_coordinator_start' })
      return
    }
    const key = fence.session_id
    const messageKey = coordinatorMessageKey(fence)
    const digest = startDigest(frame)
    const contentDigest = coordinatorStartContentDigest(frame)
    if (frame.runtime_sequence <= this.lastCoordinatorSequence) {
      if (frame.runtime_sequence < this.lastCoordinatorSequence) {
        await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), ...fence, code: 'stale_runtime_sequence' })
        return
      }
      const previous = this.lastAcceptedCoordinatorStart
      if (!previous || previous.digest !== digest) {
        await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), ...fence, code: 'runtime_sequence_conflict' })
        return
      }
      if (!this.coordinators.has(key)) {
        await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), ...fence, code: 'coordinator_start_already_settled' })
        return
      }
      await this.send({ type: 'coordinator_started', ...runtimeFields(this.runtime), ...fence })
      return
    }
    const active = this.coordinators.get(key)
    if (active) {
      if (active.messageKey === messageKey && active.contentDigest === contentDigest) {
        this.lastCoordinatorSequence = frame.runtime_sequence
        active.runtimeSequence = frame.runtime_sequence
        this.lastAcceptedCoordinatorStart = { digest, key, messageKey, runtimeSequence: frame.runtime_sequence }
        await this.send({ type: 'coordinator_started', ...runtimeFields(this.runtime), ...fence })
      } else {
        await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), ...fence, code: 'coordinator_session_busy' })
      }
      return
    }
    const previousMessage = this.coordinatorMessages.get(messageKey)
    if (previousMessage) {
      if (previousMessage.contentDigest !== contentDigest) {
        await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), ...fence, code: 'coordinator_message_conflict' })
        return
      }
      if (previousMessage.state === 'starting' || previousMessage.state === 'active') {
        await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), ...fence, code: 'coordinator_session_busy' })
        return
      }
    }
    if (this.coordinators.size >= MAX_CONCURRENT_COORDINATORS) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), ...fence, code: 'coordinator_capacity_exceeded' })
      return
    }

    const coordinator = {
      fence, key, messageKey, contentDigest, runtimeSequence: frame.runtime_sequence,
      outputBuffer: Buffer.alloc(0), outputFrames: Promise.resolve(), ready: deferred(), started: deferred(),
      spawned: deferred(), closed: deferred(), finished: deferred(), accepted: false, terminal: false,
      terminating: false, terminated: false, startFrame: frame, admitted: false,
    }
    this.coordinators.set(key, coordinator)
    this.rememberCoordinatorMessage(messageKey, { contentDigest, state: 'starting' })
    try {
      coordinator.child = spawn(process.execPath, [fileURLToPath(new URL('../bin.mjs', import.meta.url))], {
        cwd: fileURLToPath(new URL('..', import.meta.url)),
        env: taskEnvironment(),
        detached: process.platform !== 'win32',
        stdio: ['pipe', 'pipe', 'ignore'],
      })
      coordinator.child.once('spawn', () => coordinator.spawned.resolve())
      coordinator.child.once('error', error => coordinator.spawned.reject(error))
      coordinator.child.stdout.on('data', chunk => this.receiveCoordinatorOutput(coordinator, chunk))
      coordinator.child.once('close', (code, signal) => {
        coordinator.exit = { code, signal }
        if (!coordinator.readyReceived && !coordinator.accepted) coordinator.startFailed = true
        coordinator.closed.resolve(coordinator.exit)
        coordinator.ready.reject(new Error('coordinator process closed before ready'))
        void this.finishClosedCoordinator(coordinator)
      })
      coordinator.child.stdin.on('error', () => {})
      await timeout(coordinator.spawned.promise, TASK_START_TIMEOUT_MS)
      this.lastCoordinatorSequence = frame.runtime_sequence
      coordinator.startDigest = digest
      await this.send({ type: 'coordinator_process_ready', ...runtimeFields(this.runtime), runtime_sequence: frame.runtime_sequence,
        ...fence, container_pid: coordinator.child.pid })
    } catch {
      coordinator.startFailed = !coordinator.accepted
      coordinator.startDispatchFailed = coordinator.accepted
      if (coordinator.startDispatchFailed) {
        coordinator.terminating = true
        coordinator.terminateReason = 'coordinator_start_dispatch_failed'
      }
      coordinator.started.resolve()
      await this.stopCoordinatorProcess(coordinator).catch(() => {})
      await timeout(coordinator.finished.promise, TASK_STOP_TIMEOUT_MS).catch(() => {})
    }
  }

  rememberCoordinatorMessage(key, value) {
    this.coordinatorMessages.set(key, value)
    while (this.coordinatorMessages.size > MAX_COORDINATOR_MESSAGE_HISTORY) {
      let removed = false
      for (const [candidate, message] of this.coordinatorMessages) {
        if (message.state === 'starting' || message.state === 'active') continue
        this.coordinatorMessages.delete(candidate)
        removed = true
        break
      }
      if (!removed) break
    }
  }

  async admitCoordinatorProcess(frame) {
    const fence = coordinatorFence(frame)
    if (!exactKeys(frame, COORDINATOR_PROCESS_ADMIT_KEYS) || !validCoordinatorFence(fence) ||
        frame.server_instance_id !== this.runtime.server_instance_id || frame.owner_id !== this.runtime.owner_id ||
        frame.runtime_id !== this.runtime.runtime_id || frame.runtime_generation !== this.runtime.runtime_generation) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: 'invalid_coordinator_process_admission' })
      return
    }
    const coordinator = this.coordinators.get(fence.session_id)
    if (!coordinator || COORDINATOR_FENCE_FIELDS.some(key => coordinator.fence[key] !== fence[key]) ||
        coordinator.runtimeSequence !== frame.runtime_sequence || coordinator.admitted || coordinator.accepted) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), ...fence, code: 'coordinator_process_admission_mismatch' })
      return
    }
    coordinator.admitted = true
    void this.finishCoordinatorStart(coordinator).catch(() => this.terminateCoordinator(coordinator, 'coordinator_start_dispatch_failed'))
  }

  async finishCoordinatorStart(coordinator) {
    try {
      await coordinator.spawned.promise
      const { server_instance_id, runtime_id, runtime_generation, runtime_sequence, ...startFrame } = coordinator.startFrame
      await this.writeCoordinatorInput(coordinator, { ...startFrame, type: 'start' })
      await timeout(coordinator.ready.promise, TASK_START_TIMEOUT_MS)
      if (coordinator.exit) throw new Error('coordinator process exited before readiness')
      if (!this.lastAcceptedCoordinatorStart || coordinator.runtimeSequence >= this.lastAcceptedCoordinatorStart.runtimeSequence) {
        this.lastAcceptedCoordinatorStart = { digest: coordinator.startDigest, key: coordinator.key,
          messageKey: coordinator.messageKey, runtimeSequence: coordinator.runtimeSequence }
      }
      coordinator.accepted = true
      this.coordinatorMessages.get(coordinator.messageKey).state = 'active'
      await this.send({ type: 'coordinator_started', ...runtimeFields(this.runtime), ...coordinator.fence })
      coordinator.started.resolve()
    } catch {
      coordinator.startFailed = !coordinator.accepted
      coordinator.startDispatchFailed = coordinator.accepted
      if (coordinator.startDispatchFailed) {
        coordinator.terminating = true
        coordinator.terminateReason = 'coordinator_start_dispatch_failed'
      }
      coordinator.started.resolve()
      await this.stopCoordinatorProcess(coordinator).catch(() => {})
      await timeout(coordinator.finished.promise, TASK_STOP_TIMEOUT_MS).catch(() => {})
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
      if (!this.lastAcceptedStart || task.runtimeSequence >= this.lastAcceptedStart.runtimeSequence) {
        this.lastAcceptedStart = { kind: 'task', digest: task.startDigest, key: task.key, runtimeSequence: task.runtimeSequence }
      }
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

  async routeCoordinatorFrame(frame) {
    const fence = coordinatorFence(frame)
    const accountScope = ['server_instance_id', 'runtime_id', 'runtime_generation']
    if (!validCoordinatorFence(fence)) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), code: 'coordinator_fence_required' })
      return
    }
    if (frame.owner_id !== this.runtime.owner_id) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), ...fence, code: 'owner_mismatch' })
      return
    }
    if (accountScope.some(key => Object.hasOwn(frame, key)) &&
        (frame.server_instance_id !== this.runtime.server_instance_id || frame.runtime_id !== this.runtime.runtime_id ||
          frame.runtime_generation !== this.runtime.runtime_generation)) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), ...fence, code: 'runtime_fence_mismatch' })
      return
    }
    const accountKeysPresent = accountScope.filter(key => Object.hasOwn(frame, key))
    const cleanFrame = { ...frame }
    for (const key of accountScope) delete cleanFrame[key]
    const expectedResponse = ['checkpoint_ack', 'checkpoint_page', 'model_chunk', 'model_end'].includes(frame.type)
    const validShape = accountKeysPresent.length === 0 || accountKeysPresent.length === accountScope.length
    const validFrame = frame.type === 'coordinator_cancel'
      ? exactKeys(cleanFrame, ['type', ...COORDINATOR_FENCE_FIELDS])
      : expectedResponse && validCoordinatorResponseFrame(cleanFrame, fence)
    if (!validShape || !validFrame) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), ...fence, code: 'invalid_coordinator_frame' })
      return
    }
    const coordinator = this.coordinators.get(fence.session_id)
    if (!coordinator) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), ...fence, code: 'unknown_coordinator' })
      return
    }
    if (COORDINATOR_FENCE_FIELDS.some(key => coordinator.fence[key] !== fence[key])) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), ...fence, code: 'coordinator_fence_mismatch' })
      return
    }
    if (frame.type === 'coordinator_cancel') {
      if (coordinator.terminating) return
      coordinator.terminating = true
      coordinator.terminateReason = 'cancelled'
      if (!coordinator.accepted) {
        void this.terminateCoordinator(coordinator, 'cancelled')
        return
      }
      await this.writeCoordinatorInput(coordinator, { type: 'cancel', ...coordinator.fence }).catch(() => {
        void this.terminateCoordinator(coordinator, 'cancel_failed')
      })
      coordinator.cancelTimer = setTimeout(() => void this.terminateCoordinator(coordinator, 'cancel_timeout'), TASK_STOP_TIMEOUT_MS)
      coordinator.cancelTimer.unref?.()
      return
    }
    if (!coordinator.accepted) {
      await this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), ...fence, code: 'coordinator_not_running' })
      return
    }
    await this.writeCoordinatorInput(coordinator, cleanFrame)
  }

  async writeCoordinatorInput(coordinator, frame) {
    if (!coordinator.child?.stdin || coordinator.child.stdin.destroyed || coordinator.exit) throw new Error('coordinator process input is unavailable')
    await new Promise((resolve, reject) => {
      coordinator.child.stdin.write(frameBytes(frame), error => error ? reject(error) : resolve())
    })
  }

  receiveCoordinatorOutput(coordinator, chunk) {
    if (coordinator.terminated) return
    coordinator.outputBuffer = Buffer.concat([coordinator.outputBuffer, Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk)])
    for (;;) {
      const end = coordinator.outputBuffer.indexOf(10)
      if (end < 0) {
        if (coordinator.outputBuffer.length >= MAX_FRAME_BYTES) this.terminateCoordinator(coordinator, 'internal_frame_too_large')
        return
      }
      const line = coordinator.outputBuffer.subarray(0, end)
      coordinator.outputBuffer = coordinator.outputBuffer.subarray(end + 1)
      if (end >= MAX_FRAME_BYTES) {
        this.terminateCoordinator(coordinator, 'internal_frame_too_large')
        return
      }
      coordinator.outputFrames = coordinator.outputFrames.then(async () => {
        let frame
        try {
          frame = JSON.parse(UTF8_DECODER.decode(line))
        } catch {
          return this.terminateCoordinator(coordinator, 'invalid_internal_frame')
        }
        if (!frame || typeof frame !== 'object' || Array.isArray(frame) || typeof frame.type !== 'string') {
          throw new Error('invalid internal coordinator frame')
        }
        if (!coordinator.readyReceived) {
          if (Object.keys(frame).sort().join(',') !== 'engine,engine_version,type,version' || frame.type !== 'ready' ||
              frame.version !== 1 || frame.engine !== 'dsh' || frame.engine_version !== ENGINE_VERSION) {
            throw new Error('invalid coordinator readiness frame')
          }
          coordinator.readyReceived = true
          coordinator.ready.resolve()
          return
        }
        if (frame.type === 'ready') throw new Error('duplicate coordinator readiness frame')
        await coordinator.started.promise
        if (coordinator.startDispatchFailed) return
        if (frame.type === 'failed') {
          if (!exactKeys(frame, ['type', 'code']) || typeof frame.code !== 'string') throw new Error('invalid coordinator failure frame')
          coordinator.terminalFrame = { type: 'coordinator_failed', code: frame.code }
          coordinator.terminal = true
          return
        }
        if (!validCoordinatorChildFrame(frame, coordinator.fence)) throw new Error('invalid coordinator output fence')
        if (['coordinator_settled', 'coordinator_cancelled'].includes(frame.type)) {
          if (coordinator.terminalFrame) throw new Error('duplicate coordinator terminal frame')
          coordinator.terminalFrame = frame
          coordinator.terminal = true
          return
        }
        await this.send({ ...frame, ...runtimeFields(this.runtime), ...coordinator.fence })
      }).catch(() => this.terminateCoordinator(coordinator, 'output_failed'))
    }
  }

  async finishClosedCoordinator(coordinator) {
    if (coordinator.finishing) return coordinator.finished.promise
    coordinator.finishing = true
    await coordinator.outputFrames
    clearTimeout(coordinator.cancelTimer)
    const message = this.coordinatorMessages.get(coordinator.messageKey)
    if (coordinator.startFailed) {
      await this.send({ type: 'coordinator_failed', ...runtimeFields(this.runtime), ...coordinator.fence, code: 'runtime_unavailable' }).catch(() => {})
      if (message) message.state = 'failed'
    } else if (coordinator.terminalFrame?.type === 'coordinator_settled') {
      await this.send({ ...coordinator.terminalFrame, state: 'completed', ...runtimeFields(this.runtime), ...coordinator.fence }).catch(() => {})
      if (message) { message.state = 'settled'; message.terminal = coordinator.terminalFrame }
    } else if (coordinator.terminalFrame?.type === 'coordinator_cancelled' || coordinator.terminating) {
      await this.send({ type: 'coordinator_terminated', ...runtimeFields(this.runtime), ...coordinator.fence,
        reason: coordinator.terminateReason ?? 'cancelled', state: 'cancelled', process_exited: true }).catch(() => {})
      if (message) message.state = 'settled'
    } else if (coordinator.terminalFrame?.type === 'coordinator_failed') {
      await this.send({ type: 'coordinator_failed', ...runtimeFields(this.runtime), ...coordinator.fence,
        code: coordinator.terminalFrame.code }).catch(() => {})
      if (message) message.state = 'failed'
    } else {
      await this.send({ type: 'coordinator_failed', ...runtimeFields(this.runtime), ...coordinator.fence,
        code: 'runtime_unavailable', reason: 'child_exited_without_result', process_exited: true }).catch(() => {})
      if (message) message.state = 'failed'
    }
    if (this.coordinators.get(coordinator.key) === coordinator) this.coordinators.delete(coordinator.key)
    coordinator.terminated = true
    coordinator.finished.resolve()
    this.finishShutdownIfIdle()
  }

  terminateCoordinator(coordinator, reason) {
    if (coordinator.terminated || coordinator.terminating && coordinator.stopPromise) return coordinator.finished.promise
    coordinator.terminating = true
    coordinator.terminateReason ??= reason
    coordinator.stopPromise = this.stopCoordinatorProcess(coordinator).catch(() => {
      void this.send({ type: 'protocol_error', ...runtimeFields(this.runtime), ...coordinator.fence, code: 'coordinator_stop_unconfirmed' }).catch(() => {})
    })
    return coordinator.stopPromise
  }

  async stopCoordinatorProcess(coordinator) {
    if (coordinator.exit) return coordinator.exit
    const spawned = await Promise.race([coordinator.spawned.promise.then(() => true), coordinator.closed.promise.then(() => false)])
    if (!spawned || coordinator.exit) return coordinator.exit
    const signalGroup = signal => {
      if (process.platform === 'win32') {
        if (!coordinator.child.kill(signal) && !coordinator.exit) throw new Error('coordinator process could not be signalled')
        return
      }
      try {
        process.kill(-coordinator.child.pid, signal)
      } catch (error) {
        if (error.code !== 'ESRCH') throw error
      }
    }
    signalGroup('SIGTERM')
    try {
      return await timeout(coordinator.closed.promise, 250)
    } catch {
      signalGroup('SIGKILL')
      return timeout(coordinator.closed.promise, TASK_STOP_TIMEOUT_MS)
    }
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
    for (const coordinator of this.coordinators.values()) {
      coordinator.terminating = true
      coordinator.terminateReason = 'runtime_shutdown'
      if (coordinator.accepted) {
        void this.writeCoordinatorInput(coordinator, { type: 'cancel', ...coordinator.fence }).catch(() => this.terminateCoordinator(coordinator, 'cancel_failed'))
      } else {
        void this.terminateCoordinator(coordinator, 'runtime_shutdown')
      }
    }
    this.stopTimer = setTimeout(() => {
      for (const task of this.tasks.values()) void this.terminateTask(task, 'runtime_shutdown_timeout')
      for (const coordinator of this.coordinators.values()) void this.terminateCoordinator(coordinator, 'runtime_shutdown_timeout')
    }, SHUTDOWN_TIMEOUT_MS)
    this.stopTimer.unref?.()
    this.finishShutdownIfIdle()
  }

  finishShutdownIfIdle() {
    if (!this.stopping || this.tasks.size !== 0 || this.coordinators.size !== 0 || this.stopSent) return
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
