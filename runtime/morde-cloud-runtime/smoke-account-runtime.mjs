import { spawn } from 'node:child_process'
import { once } from 'node:events'
import { createInterface } from 'node:readline'
import { fileURLToPath } from 'node:url'

const identity = {
  version: 1,
  server_instance_id: 'server_smoke',
  owner_id: 'user_smoke',
  runtime_id: 'runtime_smoke',
  runtime_generation: 1,
  max_concurrent_tasks: 2,
}

function task(taskID, runtimeSequence) {
  return {
    type: 'task_start',
    version: identity.version,
    server_instance_id: identity.server_instance_id,
    owner_id: identity.owner_id,
    runtime_id: identity.runtime_id,
    runtime_generation: identity.runtime_generation,
    task_id: taskID,
    agent_id: `agent_${taskID}`,
    generation: 1,
    authorization_version: 1,
    session_id: `session_${taskID}`,
    runtime_sequence: runtimeSequence,
    model: 'container-smoke-model',
    tools: [],
    prompt: `Wait for the container smoke response for ${taskID}`,
  }
}

function taskFence(frame) {
  return Object.fromEntries(['task_id', 'owner_id', 'agent_id', 'session_id', 'generation', 'authorization_version']
    .map(key => [key, frame[key]]))
}

function alive(pid) {
  try {
    process.kill(pid, 0)
    return true
  } catch (error) {
    if (error.code === 'ESRCH') return false
    throw error
  }
}

function wait(milliseconds) {
  return new Promise(resolve => setTimeout(resolve, milliseconds))
}

async function main() {
  const runtime = spawn(process.execPath, [fileURLToPath(new URL('./bin-account.mjs', import.meta.url))], {
    cwd: fileURLToPath(new URL('.', import.meta.url)),
    env: { PATH: process.env.PATH, LANG: 'C.UTF-8', NODE_ENV: 'production' },
    stdio: ['pipe', 'pipe', 'inherit'],
  })
  const closed = once(runtime, 'close')
  const lines = createInterface({ input: runtime.stdout })
  const frames = []
  const waiters = new Set()
  const taskPIDs = new Map()
  let protocolFailure

  function fail(error) {
    protocolFailure ??= error instanceof Error ? error : new Error(String(error))
    for (const waiter of waiters) {
      clearTimeout(waiter.timer)
      waiter.reject(protocolFailure)
    }
    waiters.clear()
  }

  function send(frame) {
    if (runtime.stdin.destroyed || runtime.exitCode !== null) {
      return Promise.reject(new Error('account Runtime input closed unexpectedly'))
    }
    return new Promise((resolve, reject) => {
      runtime.stdin.write(`${JSON.stringify(frame)}\n`, error => error ? reject(error) : resolve())
    })
  }

  function waitFor(predicate, description) {
    if (protocolFailure) return Promise.reject(protocolFailure)
    const found = frames.find(predicate)
    if (found) return Promise.resolve(found)
    return new Promise((resolve, reject) => {
      const waiter = { predicate, description, resolve, reject, timer: undefined }
      waiter.timer = setTimeout(() => {
        waiters.delete(waiter)
        reject(new Error(`timed out waiting for ${description}; frames: ${frames.map(frame => frame.type).join(', ')}`))
      }, 30_000)
      waiters.add(waiter)
    })
  }

  lines.on('line', line => {
    try {
      const frame = JSON.parse(line)
      frames.push(frame)
      if (frame.type === 'protocol_error' || frame.type === 'task_terminated' || frame.type === 'failed') {
        throw new Error(`account Runtime emitted ${frame.type}: ${JSON.stringify(frame)}`)
      }
      if (frame.type === 'task_process_ready') {
        if (!Number.isSafeInteger(frame.container_pid) || frame.container_pid <= 0 || taskPIDs.has(frame.task_id)) {
          throw new Error(`invalid or duplicate Task child process identity: ${JSON.stringify(frame)}`)
        }
        taskPIDs.set(frame.task_id, frame.container_pid)
        void send({
          type: 'task_process_admit',
          server_instance_id: identity.server_instance_id,
          runtime_id: identity.runtime_id,
          runtime_generation: identity.runtime_generation,
          runtime_sequence: frame.runtime_sequence,
          ...taskFence(frame),
        }).catch(fail)
      }
      if (frame.type === 'checkpoint') {
        void send({
          type: 'checkpoint_ack',
          ...taskFence(frame),
          id: frame.id,
          ok: true,
          through_seq: frame.through_seq,
          sha256: frame.sha256,
        }).catch(fail)
      }
      for (const waiter of waiters) {
        if (!waiter.predicate(frame)) continue
        waiters.delete(waiter)
        clearTimeout(waiter.timer)
        waiter.resolve(frame)
      }
    } catch (error) {
      fail(error)
    }
  })
  runtime.once('error', fail)
  runtime.once('close', (code, signal) => {
    lines.close()
    if (code !== 0) fail(new Error(`account Runtime exited with code ${code}, signal ${signal}`))
  })

  try {
    await send({ type: 'runtime_start', ...identity })
    const ready = await waitFor(frame => frame.type === 'runtime_ready', 'runtime_ready')
    if (ready.engine_version !== '0.2.0-rc.2' || ready.max_concurrent_tasks !== 2) {
      throw new Error(`account Runtime readiness identity is invalid: ${JSON.stringify(ready)}`)
    }

    const taskA = task('task_A', 1)
    const taskB = task('task_B', 2)
    const modelAWaiter = waitFor(frame => frame.type === 'model' && frame.task_id === taskA.task_id, 'Task A model request')
    const modelBWaiter = waitFor(frame => frame.type === 'model' && frame.task_id === taskB.task_id, 'Task B model request')
    await send(taskA)
    await waitFor(frame => frame.type === 'task_started' && frame.task_id === taskA.task_id, 'Task A start')
    await send(taskB)
    await waitFor(frame => frame.type === 'task_started' && frame.task_id === taskB.task_id, 'Task B start')
    const [modelA, modelB] = await Promise.all([modelAWaiter, modelBWaiter])
    const pidA = taskPIDs.get(taskA.task_id)
    const pidB = taskPIDs.get(taskB.task_id)
    if (!pidA || !pidB || pidA === pidB || !alive(pidA) || !alive(pidB)) {
      throw new Error(`expected two live, distinct Task child processes; pids=${pidA},${pidB}`)
    }

    await send({ type: 'cancel', ...taskFence(taskA) })
    const modelCancelA = await waitFor(frame => frame.type === 'model_cancel' && frame.task_id === taskA.task_id, 'Task A model cancellation')
    if (modelCancelA.id !== modelA.id) throw new Error('Task A cancellation targeted a different model request')
    const settledA = await waitFor(frame => frame.type === 'settled' && frame.task_id === taskA.task_id, 'Task A settlement')
    if (settledA.state !== 'cancelled') throw new Error(`Task A did not settle as cancelled: ${JSON.stringify(settledA)}`)
    if (alive(pidA)) throw new Error(`Task A process ${pidA} remained alive after cancellation settled`)
    if (!alive(pidB)) throw new Error(`Task B process ${pidB} exited when Task A was cancelled`)

    const response = 'Task B continued after Task A stopped'
    const chunks = [
      { type: 'block-start', index: 0, blockType: 'text' },
      { type: 'text-delta', index: 0, text: response },
      { type: 'block-end', index: 0, block: { type: 'text', text: response } },
      { type: 'usage', usage: { inputTokens: 2, outputTokens: 1 } },
      { type: 'finish', reason: { kind: 'stop' } },
    ]
    for (const [index, chunk] of chunks.entries()) {
      await send({ type: 'model_chunk', ...taskFence(modelB), id: modelB.id, index, chunk })
    }
    await send({ type: 'model_end', ...taskFence(modelB), id: modelB.id, ok: true })
    const settledB = await waitFor(frame => frame.type === 'settled' && frame.task_id === taskB.task_id, 'Task B settlement')
    if (settledB.state !== 'completed_candidate' || alive(pidB)) {
      throw new Error(`Task B did not complete normally after continuing: ${JSON.stringify(settledB)}`)
    }

    const taskC = task('task_C', 3)
    const modelCWaiter = waitFor(frame => frame.type === 'model' && frame.task_id === taskC.task_id, 'Task C model request')
    await send(taskC)
    await waitFor(frame => frame.type === 'task_started' && frame.task_id === taskC.task_id, 'Task C start')
    const modelC = await modelCWaiter
    const pidC = taskPIDs.get(taskC.task_id)
    if (!pidC || !alive(pidC)) throw new Error('account Runtime did not start Task C after both earlier Tasks stopped')
    for (const [index, chunk] of chunks.entries()) {
      await send({ type: 'model_chunk', ...taskFence(modelC), id: modelC.id, index, chunk })
    }
    await send({ type: 'model_end', ...taskFence(modelC), id: modelC.id, ok: true })
    const settledC = await waitFor(frame => frame.type === 'settled' && frame.task_id === taskC.task_id, 'Task C settlement')
    if (settledC.state !== 'completed_candidate' || alive(pidC)) {
      throw new Error(`Task C did not complete after Runtime reuse: ${JSON.stringify(settledC)}`)
    }

    await send({ type: 'runtime_shutdown', ...identity })
    const stopped = await waitFor(frame => frame.type === 'runtime_stopped', 'runtime_stopped')
    if (stopped.reason !== 'requested') throw new Error(`unexpected Runtime stop reason: ${stopped.reason}`)
    runtime.stdin.end()
    const [code, signal] = await closed
    if (code !== 0 || signal !== null) throw new Error(`account Runtime did not exit cleanly: code=${code}, signal=${signal}`)
    console.log(`account runtime container smoke passed (Task A pid ${pidA} cancelled; Task B pid ${pidB} continued; Task C pid ${pidC} completed after reuse)`)
  } finally {
    for (const pid of taskPIDs.values()) {
      if (!alive(pid)) continue
      try {
        process.kill(-pid, 'SIGKILL')
      } catch (error) {
        if (error.code !== 'ESRCH') throw error
      }
    }
    if (runtime.exitCode === null) {
      runtime.kill('SIGTERM')
      await Promise.race([closed, wait(2_000)])
      if (runtime.exitCode === null) runtime.kill('SIGKILL')
    }
  }
}

main().catch(error => {
  console.error(error.message)
  process.exitCode = 1
})
