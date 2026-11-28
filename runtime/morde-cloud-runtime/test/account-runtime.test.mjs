import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { once } from 'node:events'
import { createInterface } from 'node:readline'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const account = {
  type: 'runtime_start', version: 1, server_instance_id: 'server_123', owner_id: 'user_123',
  runtime_id: 'runtime_123', runtime_generation: 4, max_concurrent_tasks: 2,
}

function taskStart(taskID, runtimeSequence = 1) {
  return {
    type: 'task_start', version: 1, task_id: taskID, owner_id: account.owner_id,
    agent_id: `agent_${taskID}`, generation: 1, authorization_version: 1,
    session_id: `session_${taskID}`, task_revision: 0, model: 'test-model', tools: [], prompt: 'Test request',
    server_instance_id: account.server_instance_id, runtime_id: account.runtime_id,
    runtime_generation: account.runtime_generation, runtime_sequence: runtimeSequence,
  }
}

function launchAccount(onFrame) {
  const child = spawn(process.execPath, ['bin-account.mjs'], {
    cwd: fileURLToPath(new URL('..', import.meta.url)),
    env: { PATH: process.env.PATH, LANG: 'C.UTF-8' },
    stdio: ['pipe', 'pipe', 'ignore'],
  })
  const frames = []
  const lines = createInterface({ input: child.stdout })
  lines.on('line', line => {
    const frame = JSON.parse(line)
    frames.push(frame)
    onFrame(frame, child)
  })
  return { child, frames }
}

function send(child, frame) {
  child.stdin.write(`${JSON.stringify(frame)}\n`)
}

function taskProcessAdmission(frame) {
  const fields = ['task_id', 'owner_id', 'agent_id', 'session_id', 'generation', 'authorization_version']
  return {
    type: 'task_process_admit', server_instance_id: frame.server_instance_id, runtime_id: frame.runtime_id,
    runtime_generation: frame.runtime_generation, runtime_sequence: frame.runtime_sequence,
    ...Object.fromEntries(fields.map(key => [key, frame[key]])),
  }
}

test('account runtime accepts the server-validated web research profile and ordinary starts without one', async t => {
  const profile = taskStart('task_profile', 1)
  profile.agent_tool_profile = 'web_research'
  profile.tools = [
    {
      name: 'morde_web_search', description: 'Search public web sources.',
      parameters: { type: 'object', properties: { query: { type: 'string' } }, required: ['query'], additionalProperties: false },
    },
    {
      name: 'morde_web_read', description: 'Read a public web source.',
      parameters: { type: 'object', properties: { url: { type: 'string' } }, required: ['url'], additionalProperties: false },
    },
    {
      name: 'morde_task_result_evidence', description: 'Attach evidence to material claims in the task result.',
      parameters: { type: 'object', properties: {}, additionalProperties: false },
    },
  ]
  const ordinary = taskStart('task_ordinary', 2)
  assert.equal(Object.hasOwn(ordinary, 'agent_tool_profile'), false)

  const started = new Set()
  const protocolErrors = []
  const { child, frames } = launchAccount((frame, proc) => {
    if (frame.type === 'runtime_ready') {
      send(proc, profile)
      send(proc, ordinary)
    }
    if (frame.type === 'task_process_ready') send(proc, taskProcessAdmission(frame))
    if (frame.type === 'protocol_error') {
      protocolErrors.push(frame.code)
      send(proc, { ...account, type: 'runtime_shutdown' })
    }
    if (frame.type === 'task_started') {
      started.add(frame.task_id)
      if (started.size === 2) send(proc, { ...account, type: 'runtime_shutdown' })
    }
    if (frame.type === 'runtime_stopped') proc.stdin.end()
  })
  t.after(() => child.kill())
  const closed = once(child, 'close')
  send(child, account)
  const [code] = await closed
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.deepEqual(started, new Set(['task_profile', 'task_ordinary']))
  assert.deepEqual(protocolErrors, [])
})

test('account runtime rejects an unsupported task agent tool profile', async t => {
  const forged = taskStart('task_forged')
  forged.agent_tool_profile = 'all_tools'
  const protocolErrors = []
  const { child, frames } = launchAccount((frame, proc) => {
    if (frame.type === 'runtime_ready') send(proc, forged)
    if (frame.type === 'protocol_error') {
      protocolErrors.push(frame.code)
      send(proc, { ...account, type: 'runtime_shutdown' })
    }
    if (frame.type === 'runtime_stopped') proc.stdin.end()
  })
  t.after(() => child.kill())
  const closed = once(child, 'close')
  send(child, account)
  const [code] = await closed
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.ok(protocolErrors.includes('invalid_task_start'))
  assert.equal(frames.some(frame => frame.type === 'task_process_ready'), false)
})

test('account runtime rejects unlisted task start fields', async t => {
  const forged = taskStart('task_extra_field')
  forged.client_profile = 'web_research'
  const protocolErrors = []
  const { child, frames } = launchAccount((frame, proc) => {
    if (frame.type === 'runtime_ready') send(proc, forged)
    if (frame.type === 'protocol_error') {
      protocolErrors.push(frame.code)
      send(proc, { ...account, type: 'runtime_shutdown' })
    }
    if (frame.type === 'runtime_stopped') proc.stdin.end()
  })
  t.after(() => child.kill())
  const closed = once(child, 'close')
  send(child, account)
  const [code] = await closed
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.ok(protocolErrors.includes('invalid_task_start'))
  assert.equal(frames.some(frame => frame.type === 'task_process_ready'), false)
})
