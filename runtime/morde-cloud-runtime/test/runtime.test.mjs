import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { spawn } from 'node:child_process'
import { readFile } from 'node:fs/promises'
import { once } from 'node:events'
import { createInterface } from 'node:readline'
import { PassThrough } from 'node:stream'
import test from 'node:test'
import { AccountRuntime } from '../src/account-runtime.mjs'
import { ENGINE_VERSION, modelCallID, validMessageDelivery, validStart } from '../src/engine.mjs'
import { WorkerIPC } from '../src/ipc.mjs'

const start = {
  type: 'start', version: 1, task_id: 'task_123', owner_id: 'user_123',
  agent_id: 'agent_123', generation: 1, authorization_version: 1,
  session_id: 'session_456', model: 'test-model', prompt: 'Hello from a local test',
}

function launch(onFrame, extraEnv = {}, entrypoint = 'bin.mjs') {
  const child = spawn(process.execPath, [entrypoint], {
    cwd: new URL('..', import.meta.url),
    env: { PATH: process.env.PATH, LANG: 'C.UTF-8', ...extraEnv },
    stdio: ['pipe', 'pipe', 'pipe'],
  })
  child.responseFences = entrypoint === 'bin.mjs' ? new Map() : undefined
  const frames = []
  const lines = createInterface({ input: child.stdout })
  lines.on('line', line => {
    const frame = JSON.parse(line)
    if (child.responseFences && ['checkpoint', 'model'].includes(frame.type)) child.responseFences.set(frame.id, taskFence(frame))
    frames.push(frame)
    onFrame(frame, child)
  })
  return { child, frames }
}

function launchAccount(onFrame) {
  return launch((frame, proc) => {
    onFrame(frame, proc)
    if (frame.type === 'task_process_ready') {
      send(proc, taskProcessAdmission(frame))
    }
  }, {}, 'bin-account.mjs')
}

function send(child, frame) {
  const fence = child.responseFences?.get(frame.id)
  if (fence && ['checkpoint_ack', 'model_chunk', 'model_end'].includes(frame.type)) {
    frame = { ...fence, ...frame }
    if (frame.type !== 'model_chunk') child.responseFences.delete(frame.id)
  }
  child.stdin.write(`${JSON.stringify(frame)}\n`)
}

const account = {
  type: 'runtime_start', version: 1, server_instance_id: 'server_123', owner_id: 'user_123',
  runtime_id: 'runtime_123', runtime_generation: 4, max_concurrent_tasks: 2,
}

function accountTask(taskID, prompt = `prompt ${taskID}`, runtimeSequence = 1) {
  return {
    ...start, type: 'task_start', task_id: taskID, session_id: `session_${taskID}`, prompt,
    server_instance_id: account.server_instance_id, runtime_id: account.runtime_id, runtime_generation: account.runtime_generation,
    runtime_sequence: runtimeSequence,
  }
}

function taskProcessAdmission(frame) {
  return {
    type: 'task_process_admit', server_instance_id: frame.server_instance_id, runtime_id: frame.runtime_id,
    runtime_generation: frame.runtime_generation, runtime_sequence: frame.runtime_sequence,
    ...taskFence(frame),
  }
}

function taskFence(frame) {
  return Object.fromEntries(['task_id', 'owner_id', 'agent_id', 'session_id', 'generation', 'authorization_version']
    .map(key => [key, frame[key]]))
}

function messageDelivery(identity, messageID = 'm1234567', content = 'Please check this update.', attachmentIDs = []) {
  return { type: 'message_delivery', version: 2, ...taskFence(identity), message_id: messageID, content, attachment_ids: attachmentIDs }
}

function acknowledgeCheckpoint(proc, frame) {
  send(proc, {
    type: 'checkpoint_ack', ...taskFence(frame), id: frame.id, ok: true,
    through_seq: frame.through_seq, sha256: frame.sha256,
  })
}

function replyWithText(proc, frame, text) {
  const chunks = [
    { type: 'block-start', index: 0, blockType: 'text' },
    { type: 'text-delta', index: 0, text },
    { type: 'block-end', index: 0, block: { type: 'text', text } },
    { type: 'usage', usage: { inputTokens: 2, outputTokens: 1 } },
    { type: 'finish', reason: { kind: 'stop' } },
  ]
  chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', ...taskFence(frame), id: frame.id, index, chunk }))
  send(proc, { type: 'model_end', ...taskFence(frame), id: frame.id, ok: true })
}

function testJpeg(width = 3, height = 2) {
  return Buffer.from([
    0xff, 0xd8,
    0xff, 0xc0, 0x00, 0x11, 0x08, 0x00, height, 0x00, width, 0x03,
    0x01, 0x11, 0x00, 0x02, 0x11, 0x00, 0x03, 0x11, 0x00,
    0xff, 0xd9,
  ])
}

function toolImage(bytes = testJpeg(), width = 3, height = 2, mediaType = 'image/jpeg') {
  return {
    attachment_id: `morde_img_${createHash('sha256').update(bytes).digest('hex')}`,
    media_type: mediaType,
    bytes: bytes.length,
    width,
    height,
    data_base64: bytes.toString('base64'),
  }
}

const screenshotTool = {
  name: 'desktop_screenshot', description: 'Capture the current cloud desktop screen.',
  parameters: { type: 'object', properties: {}, additionalProperties: false },
}

const taskAttachmentTool = {
  name: 'morde_read_task_attachment', description: 'Read an authorized image attachment.',
  parameters: { type: 'object', properties: {}, additionalProperties: false },
}

function testPng() {
  return Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg==', 'base64')
}

const fileReadTool = {
  name: 'file_read', description: 'Read a file from the task workspace.',
  parameters: { type: 'object', properties: { path: { type: 'string' } }, required: ['path'], additionalProperties: false },
}
const delegateTool = {
  name: 'dsh_delegate', description: 'Delegate a bounded task to a child agent.',
  parameters: {
    type: 'object',
    properties: {
      title: { type: 'string' },
      objective: { type: 'string' },
      run_in_background: { type: 'boolean' },
      tool_names: { type: 'array', items: { type: 'string', enum: [fileReadTool.name] }, uniqueItems: true },
    },
    required: ['title', 'objective', 'tool_names'],
    additionalProperties: false,
  },
}
const jobListTool = {
  name: 'dsh_job_list', description: 'List this task\'s delegated background jobs.',
  parameters: { type: 'object', properties: {}, required: [], additionalProperties: false },
}
const jobGetTool = {
  name: 'dsh_job_get', description: 'Read one delegated background job.',
  parameters: { type: 'object', properties: { job_id: { type: 'string' } }, required: ['job_id'], additionalProperties: false },
}
const jobOutputTool = {
  name: 'dsh_job_output', description: 'Read new delegated job output.',
  parameters: {
    type: 'object', properties: { job_id: { type: 'string' }, after_seq: { type: 'integer', minimum: -1 } },
    required: ['job_id', 'after_seq'], additionalProperties: false,
  },
}
const jobCancelTool = {
  name: 'dsh_job_cancel', description: 'Cancel one delegated background job.',
  parameters: { type: 'object', properties: { job_id: { type: 'string' } }, required: ['job_id'], additionalProperties: false },
}
const jobTools = [jobListTool, jobGetTool, jobOutputTool, jobCancelTool]

test('model ID has a stable cross-language test vector', () => {
  assert.equal(modelCallID('task_123', 'session_456', 7), 'mdl_2aef0005101ead56ee43e897a0c630a0c67e253476dbfb3fe3c0ae571c147b83')
  assert.throws(() => modelCallID('task\n123', 'session_456', 7))
})

test('keeps Morde checkpoint compatibility version independent of the upstream DSH release', () => {
  assert.equal(ENGINE_VERSION, '0.2.0-rc.2')
})

test('validates message delivery Unicode characters and rejects NUL or invalid Unicode', () => {
  const delivery = messageDelivery(start, 'm1234567', '😀'.repeat(32_768))
  assert.equal(Buffer.byteLength(delivery.content, 'utf8'), 128 * 1024)
  assert.equal(validMessageDelivery(delivery, start), true)
  assert.equal(validMessageDelivery({ ...delivery, content: '', attachment_ids: [] }, start), false)
  assert.equal(validMessageDelivery({ ...delivery, content: `${delivery.content}😀` }, start), false)
  assert.equal(validMessageDelivery({ ...delivery, content: 'contains\0nul' }, start), false)
  assert.equal(validMessageDelivery({ ...delivery, content: '\ud800' }, start), false)
  assert.equal(validMessageDelivery({ ...delivery, message_id: 'M1234567' }, start), false)
  assert.equal(validMessageDelivery({ ...delivery, owner_id: 'other_owner' }, start), false)
})

test('validates queued personal and project attachment IDs, order, and count', () => {
  const first = 'pa_0123456789abcdef0123456789abcdef'
  const second = 'pa_1123456789abcdef0123456789abcdef'
  const delivery = messageDelivery(start, 'm1234567', 'inspect these files', [first, second])
  assert.equal(validMessageDelivery(delivery, start), true)
  assert.equal(validMessageDelivery({ ...delivery, content: '' }, start), true)
  assert.equal(validMessageDelivery({ ...delivery, attachment_ids: [second, first] }, start), false)
  assert.equal(validMessageDelivery({ ...delivery, attachment_ids: [first, first] }, start), false)
  assert.equal(validMessageDelivery({ ...delivery, attachment_ids: ['pu_0123456789abcdef0123456789abcdef'] }, start), false)
  const project = 'pca_0123456789abcdef0123456789abcdef'
  assert.equal(validMessageDelivery({ ...delivery, attachment_ids: [project] }, start), true)
  assert.equal(validMessageDelivery({ ...delivery, attachment_ids: ['pca_0123456789abcdef0123456789abcdeF'] }, start), false)
  assert.equal(validMessageDelivery({ ...delivery, attachment_ids: Array(17).fill(first) }, start), false)
})

test('uses Morde SOUL as the system persona across conversation turns', async () => {
  const personalSoul = 'The user prefers concise answers.'
  const prompt = `Who are you?\n\nAuthorized personal SOUL: ${personalSoul}`
  const delivery = messageDelivery(start, 'm1234567', 'Please continue.')
  let modelCalls = 0
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') {
      send(proc, { ...start, model: 'deepseek-chat', prompt })
      send(proc, delivery)
    }
    if (frame.type === 'checkpoint') acknowledgeCheckpoint(proc, frame)
    if (frame.type === 'model') {
      modelCalls++
      assert.equal(frame.request.model, 'deepseek-chat')
      const system = frame.request.messages.filter(message => message.role === 'system')
      assert.equal(system.length, 1)
      const text = system[0].content.map(block => block.text ?? '').join('')
      assert.match(text, /You are Morde, the user's AI assistant\./)
      assert.match(text, /Be resourceful before asking/)
      assert.match(text, /For current facts or an explicit search request/)
      assert.match(text, /If you update the user's personal SOUL through an authorized tool, tell them after the update succeeds/)
      assert.doesNotMatch(text, /You are an AI agent powered by DeepSeek Harness/)
      assert.ok(frame.request.messages.some(message => message.role === 'user' &&
        message.content.some(block => block.text?.includes(personalSoul))))
      replyWithText(proc, frame, '我是 Morde。')
    }
  })
  const [code] = await once(child, 'close')
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(modelCalls, 2)
  assert.equal(frames.find(frame => frame.type === 'settled')?.state, 'completed_candidate')
})

test('keeps the DSH runtime SOUL identical to the model gateway SOUL', async () => {
  const runtimeSoul = await readFile(new URL('../src/SOUL.md', import.meta.url), 'utf8')
  const modelGatewaySoul = await readFile(new URL('../../../../../server/internal/app/SOUL.md', import.meta.url), 'utf8')
  assert.equal(runtimeSoul, modelGatewaySoul)
  assert.match(runtimeSoul, /You are Morde, the user's AI assistant\./)
  assert.match(runtimeSoul, /GPT, DeepSeek, and other model providers are implementation details/)
})

test('queues running message delivery once with its database id for the next turn', async () => {
  const delivery = messageDelivery(start, 'm1234567', 'Use the updated deployment window.')
  let modelCalls = 0
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') {
      send(proc, start)
      send(proc, delivery)
      send(proc, delivery)
    }
    if (frame.type === 'checkpoint') acknowledgeCheckpoint(proc, frame)
    if (frame.type === 'model') {
      modelCalls++
      if (modelCalls === 1) {
        assert.equal(frame.request.messages.some(message => message.id === delivery.message_id), false)
        replyWithText(proc, frame, 'First response.')
      } else {
        const delivered = frame.request.messages.filter(message => message.id === delivery.message_id)
        assert.equal(delivered.length, 1)
        assert.equal(delivered[0].content[0].text, delivery.content)
        replyWithText(proc, frame, 'Acknowledged the update.')
      }
    }
  })
  const [code] = await once(child, 'close')
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(modelCalls, 2)
  const events = frames.filter(frame => frame.type === 'checkpoint').flatMap(frame => JSON.parse(frame.payload).events)
  assert.equal(events.filter(event => event.type === 'user/message' && event.data.id === delivery.message_id).length, 1)
  assert.equal(frames.find(frame => frame.type === 'settled')?.state, 'completed_candidate')
})

test('queues running image and PDF attachment references and deduplicates the full delivery', async () => {
  const attachmentIDs = ['pa_0123456789abcdef0123456789abcdef', 'pa_1123456789abcdef0123456789abcdef']
  const delivery = messageDelivery(start, 'm1234567', 'Please inspect the attached image and PDF.', attachmentIDs)
  let modelCalls = 0
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') {
      send(proc, start)
      send(proc, delivery)
      send(proc, delivery)
    }
    if (frame.type === 'checkpoint') acknowledgeCheckpoint(proc, frame)
    if (frame.type === 'model') {
      if (modelCalls++ === 0) {
        replyWithText(proc, frame, 'I will inspect both attachments.')
      } else {
        const delivered = frame.request.messages.filter(message => message.id === delivery.message_id)
        assert.equal(delivered.length, 1)
        assert.deepEqual(delivered[0].content.map(block => block.text), [
          delivery.content,
          `[MORDE_TASK_ATTACHMENT_IDS_V1:${attachmentIDs.join(',')}]`,
        ])
        replyWithText(proc, frame, 'I inspected the image and PDF.')
      }
    }
  })
  const [code] = await once(child, 'close')
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(modelCalls, 2)
  const events = frames.filter(frame => frame.type === 'checkpoint').flatMap(frame => JSON.parse(frame.payload).events)
  const saved = events.find(event => event.type === 'user/message' && event.data.id === delivery.message_id)
  assert.deepEqual(saved?.data.content.map(block => block.text), [
    delivery.content,
    `[MORDE_TASK_ATTACHMENT_IDS_V1:${attachmentIDs.join(',')}]`,
  ])
})

test('conflicting content for a delivered message id fails the worker closed', () => {
  const input = new PassThrough()
  const output = new PassThrough()
  const ipc = new WorkerIPC(input, output)
  const deliveries = []
  ipc.setMessageDeliveryHandler(delivery => deliveries.push(delivery))
  const original = messageDelivery(start)
  input.write(`${JSON.stringify(start)}\n${JSON.stringify(original)}\n${JSON.stringify({ ...original, content: 'different content' })}\n`)
  assert.equal(deliveries.length, 1)
  assert.equal(ipc.failed, true)
  input.destroy()
  output.destroy()
})

test('conflicting attachment IDs for a delivered message id fail the worker closed', () => {
  const input = new PassThrough()
  const output = new PassThrough()
  const ipc = new WorkerIPC(input, output)
  const deliveries = []
  ipc.setMessageDeliveryHandler(delivery => deliveries.push(delivery))
  const original = messageDelivery(start, 'm1234567', 'same text', ['pa_0123456789abcdef0123456789abcdef'])
  const changed = messageDelivery(start, 'm1234567', 'same text', ['pa_1123456789abcdef0123456789abcdef'])
  input.write(`${JSON.stringify(start)}\n${JSON.stringify(original)}\n${JSON.stringify(changed)}\n`)
  assert.equal(deliveries.length, 1)
  assert.equal(ipc.failed, true)
  input.destroy()
  output.destroy()
})

test('a message delivered after the agent closes its handler fails closed', () => {
  const input = new PassThrough()
  const output = new PassThrough()
  const ipc = new WorkerIPC(input, output)
  input.write(`${JSON.stringify(start)}\n`)
  ipc.closeMessageDelivery()
  input.write(`${JSON.stringify(messageDelivery(start))}\n`)
  assert.equal(ipc.failed, true)
  input.destroy()
  output.destroy()
})

test('checkpoint acknowledgements must match the requesting session fence', async () => {
  const input = new PassThrough()
  const output = new PassThrough()
  const ipc = new WorkerIPC(input, output)
  input.write(`${JSON.stringify(start)}\n`)
  const sentFrame = once(output, 'data')
  const checkpoint = ipc.checkpoint(taskFence(start), 0, 0, '{}', 'a'.repeat(64))
  const rejected = assert.rejects(checkpoint, /checkpoint not confirmed/)
  const [data] = await sentFrame
  const request = JSON.parse(data.toString())
  input.write(`${JSON.stringify({
    type: 'checkpoint_ack', ...taskFence(start), session_id: 'session_other', id: request.id,
    ok: true, through_seq: 0, sha256: 'a'.repeat(64),
  })}\n`)
  await rejected
  assert.equal(ipc.pending.has(request.id), false)
  input.destroy()
  output.destroy()
})

test('child settlement acknowledgements must match the complete task fence', async () => {
  const input = new PassThrough()
  const output = new PassThrough()
  const ipc = new WorkerIPC(input, output)
  input.write(`${JSON.stringify(start)}\n`)
  const sentFrame = once(output, 'data')
  const settlement = ipc.settleChild(taskFence(start), 'child-call', 'succeeded', -1, '', 'done')
  const rejected = assert.rejects(settlement, /child settlement was not confirmed/)
  await sentFrame
  input.write(`${JSON.stringify({ type: 'child_settle_ack', ...taskFence(start), task_id: 'task_other', call_id: 'child-call', ok: true })}\n`)
  await rejected
  assert.equal(ipc.failed, true)
  input.destroy()
  output.destroy()
})

for (const type of ['model_chunk', 'model_end']) {
  test(`${type} responses must match the requesting session fence`, async () => {
    const input = new PassThrough()
    const output = new PassThrough()
    const ipc = new WorkerIPC(input, output)
    input.write(`${JSON.stringify(start)}\n`)
    const sentFrame = once(output, 'data')
    const response = ipc.streamModel(taskFence(start), 'model_test', 0, { messages: [], tools: [] })
    const next = response.next()
    const rejected = assert.rejects(next, /request cancelled/)
    await sentFrame
    const frame = type === 'model_chunk'
      ? { type, ...taskFence(start), id: 'model_test', index: 0, chunk: { type: 'text-delta', text: 'private' } }
      : { type, ...taskFence(start), id: 'model_test', ok: true }
    frame.session_id = 'session_other'
    input.write(`${JSON.stringify(frame)}\n`)
    await rejected
    assert.equal(ipc.failed, true)
    input.destroy()
    output.destroy()
  })
}

test('late model frames after cancellation are drained through the matching model_end', async () => {
  const input = new PassThrough()
  const output = new PassThrough()
  const ipc = new WorkerIPC(input, output)
  input.write(`${JSON.stringify(start)}\n`)
  const controller = new AbortController()
  const requestFramePromise = once(output, 'data')
  const pending = ipc.streamModel(taskFence(start), 'model_cancelled', 0, { messages: [], tools: [] }, controller.signal).next()
  const [requestData] = await requestFramePromise
  assert.equal(JSON.parse(requestData.toString()).type, 'model')
  const cancelFramePromise = once(output, 'data')
  controller.abort()
  await assert.rejects(pending, /aborted/)
  const [cancelData] = await cancelFramePromise
  assert.equal(JSON.parse(cancelData.toString()).type, 'model_cancel')

  input.write(`${JSON.stringify({
    type: 'model_chunk', ...taskFence(start), id: 'model_cancelled', index: 0,
    chunk: { type: 'text-delta', text: 'late response' },
  })}\n`)
  input.write(`${JSON.stringify({ type: 'model_end', ...taskFence(start), id: 'model_cancelled', ok: true })}\n`)
  await new Promise(resolve => setImmediate(resolve))
  assert.equal(ipc.failed, false)
  assert.equal(ipc.cancelledModels.has('model_cancelled'), false)
  input.destroy()
  output.destroy()
})

test('late model frames after cancellation still require the original session fence', async () => {
  const input = new PassThrough()
  const output = new PassThrough()
  const ipc = new WorkerIPC(input, output)
  input.write(`${JSON.stringify(start)}\n`)
  const controller = new AbortController()
  const requestFramePromise = once(output, 'data')
  const pending = ipc.streamModel(taskFence(start), 'model_cancelled', 0, { messages: [], tools: [] }, controller.signal).next()
  await requestFramePromise
  const cancelFramePromise = once(output, 'data')
  controller.abort()
  await assert.rejects(pending, /aborted/)
  await cancelFramePromise
  input.write(`${JSON.stringify({
    type: 'model_end', ...taskFence(start), session_id: 'session_other', id: 'model_cancelled', ok: false,
  })}\n`)
  await new Promise(resolve => setImmediate(resolve))
  assert.equal(ipc.failed, true)
  input.destroy()
  output.destroy()
})

test('real dsh runs only after worker confirms its events', async () => {
  let through = -1
  let calls = 0
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, start)
    if (frame.type === 'checkpoint') {
      const payload = JSON.parse(frame.payload)
      assert.equal(frame.from_seq, through + 1)
      assert.equal(payload.from_seq, frame.from_seq)
      assert.equal(payload.header.id, start.session_id)
      assert.equal(createHash('sha256').update(frame.payload).digest('hex'), frame.sha256)
      assert.equal(payload.events.at(-1).seq, frame.through_seq)
      through = frame.through_seq
      send(proc, { type: 'checkpoint_ack', id: frame.id, ok: true, through_seq: through, sha256: frame.sha256 })
    }
    if (frame.type === 'model') {
      calls += 1
      assert.equal(frame.through_seq, through)
      assert.equal(frame.id, modelCallID(start.task_id, start.session_id, through))
      assert.equal(frame.sha256, createHash('sha256').update(JSON.stringify(frame.request)).digest('hex'))
      assert.equal(frame.request.model, start.model)
      assert.deepEqual(frame.request.tools, [])
      assert.ok(frame.request.messages.some(message => message.role === 'user' && message.content.some(block => block.text === start.prompt)))
      const chunks = [
        { type: 'block-start', index: 0, blockType: 'text' },
        { type: 'text-delta', index: 0, text: 'hello' },
        { type: 'block-end', index: 0, block: { type: 'text', text: 'hello' } },
        { type: 'usage', usage: { inputTokens: 2, outputTokens: 1 } },
        { type: 'finish', reason: { kind: 'stop' } },
      ]
      chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
      send(proc, { type: 'model_end', id: frame.id, ok: true })
    }
  })
  const [code] = await once(child, 'close')
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(calls, 1)
  const settled = frames.find(frame => frame.type === 'settled')
  assert.equal(settled?.state, 'completed_candidate')
  assert.equal(settled.through_seq, through)
  assert.ok(frames.some(frame => frame.type === 'checkpoint' && JSON.parse(frame.payload).events.some(event => event.type === 'assistant/message')))
})

test('starts a first attachment message once with its authorized context', async t => {
  const attachmentIDs = ['pa_0123456789abcdef0123456789abcdef']
  const initial = { message_id: 'm1234567', content: 'Please inspect this file: ' + 'a'.repeat(40 * 1024), attachment_ids: attachmentIDs }
  const initialStart = { ...start, initial_message: initial, initial_context: 'Earlier authorized context: a draft report.' }
  delete initialStart.prompt
  assert.equal(validStart(initialStart), true)
  let calls = 0
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') {
      send(proc, initialStart)
      send(proc, messageDelivery(initialStart, initial.message_id, initial.content, initial.attachment_ids))
    }
    if (frame.type === 'checkpoint') acknowledgeCheckpoint(proc, frame)
    if (frame.type === 'model') {
      calls++
      const all = frame.request.messages.flatMap(message => message.content ?? [])
      assert.equal(all.filter(block => block.text === initial.content).length, 1)
      assert.equal(all.filter(block => block.text === `[MORDE_TASK_ATTACHMENT_IDS_V1:${attachmentIDs.join(',')}]`).length, 1)
      assert.equal(all.filter(block => block.text === initialStart.initial_context).length, 1)
      replyWithText(proc, frame, 'I can see the attachment reference.')
    }
  })
  t.after(() => child.kill())
  const [code] = await once(child, 'close')
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(calls, 1)
  const events = frames.filter(frame => frame.type === 'checkpoint').flatMap(frame => JSON.parse(frame.payload).events)
  assert.equal(events.filter(event => event.type === 'user/message' && event.data.id === initial.message_id).length, 1)
  assert.ok(events.some(event => event.type === 'user/message' && event.data.source?.kind === 'plugin' &&
    event.data.source?.plugin === 'morde-context' && event.data.content?.some(block => block.text === initialStart.initial_context)))
})

test('persists trusted initial context separately from a prompt-only task', async t => {
  const context = 'Authorized personal context.\nMORDE_MEMORY_STATE_V1:opaque-proof'
  const promptStart = { ...start, prompt: 'Summarize my current task.', initial_context: context }
  let calls = 0
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, promptStart)
    if (frame.type === 'checkpoint') acknowledgeCheckpoint(proc, frame)
    if (frame.type === 'model') {
      calls++
      const messages = frame.request.messages
      assert.ok(messages.some(message => message.role === 'user' && message.source?.kind === 'user' &&
        message.content?.some(block => block.text === promptStart.prompt)))
      assert.ok(messages.some(message => message.role === 'user' && message.source?.kind === 'plugin' &&
        message.source?.plugin === 'morde-context' && message.content?.some(block => block.text === context)))
      replyWithText(proc, frame, 'I found the relevant details.')
    }
  })
  t.after(() => child.kill())
  const [code] = await once(child, 'close')
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(calls, 1)
  const events = frames.filter(frame => frame.type === 'checkpoint').flatMap(frame => JSON.parse(frame.payload).events)
  assert.ok(events.some(event => event.type === 'user/message' && event.data.source?.kind === 'plugin' &&
    event.data.source?.plugin === 'morde-context' && event.data.content?.some(block => block.text === context)))
})

test('delivers a project attachment-only first message once', async t => {
  const initial = { message_id: 'm1234567', content: '', attachment_ids: ['pca_0123456789abcdef0123456789abcdef'] }
  const initialStart = { ...start, initial_message: initial }
  delete initialStart.prompt
  assert.equal(validStart(initialStart), true)
  let calls = 0
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') {
      send(proc, initialStart)
      send(proc, messageDelivery(initialStart, initial.message_id, initial.content, initial.attachment_ids))
    }
    if (frame.type === 'checkpoint') acknowledgeCheckpoint(proc, frame)
    if (frame.type === 'model') {
      calls++
      const marker = `[MORDE_TASK_ATTACHMENT_IDS_V1:${initial.attachment_ids[0]}]`
      const messages = frame.request.messages.filter(message => message.id === initial.message_id)
      assert.equal(messages.length, 1)
      assert.equal(messages[0].content.filter(block => block.text === marker).length, 1)
      replyWithText(proc, frame, 'I can inspect the project attachment.')
    }
  })
  t.after(() => child.kill())
  const [code] = await once(child, 'close')
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(calls, 1)
  const events = frames.filter(frame => frame.type === 'checkpoint').flatMap(frame => JSON.parse(frame.payload).events)
  assert.equal(events.filter(event => event.type === 'user/message' && event.data.id === initial.message_id).length, 1)
})

test('a refused checkpoint prevents a model call', async () => {
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, start)
    if (frame.type === 'checkpoint') send(proc, { type: 'checkpoint_ack', id: frame.id, ok: false, through_seq: frame.through_seq, sha256: frame.sha256 })
  })
  const [code] = await once(child, 'close')
  assert.equal(code, 1)
  assert.equal(frames.some(frame => frame.type === 'model'), false)
  assert.equal(frames.find(frame => frame.type === 'failed')?.code, 'runtime_unavailable')
})

test('rejects an inherited secret-bearing environment before ready', async () => {
  const { child, frames } = launch(() => {}, { TEST_SECRET: 'not-for-child' })
  const [code] = await once(child, 'close')
  assert.equal(code, 1)
  assert.equal(frames.some(frame => frame.type === 'ready'), false)
})

test('rejects recovery without a pinned checkpoint and with a replayed prompt', async () => {
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, { ...start, resume: true })
  })
  const [code] = await once(child, 'close')
  assert.equal(code, 1)
  assert.equal(frames.some(frame => frame.type === 'model'), false)
})

test('cancelling before the agent starts does not dispatch a model', async () => {
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') {
      send(proc, start)
      send(proc, { type: 'cancel' })
    }
    if (frame.type === 'checkpoint') send(proc, { type: 'checkpoint_ack', id: frame.id, ok: true, through_seq: frame.through_seq, sha256: frame.sha256 })
  })
  const [code] = await once(child, 'close')
  assert.equal(code, 0)
  assert.equal(frames.some(frame => frame.type === 'model'), false)
  assert.equal(frames.find(frame => frame.type === 'settled')?.state, 'cancelled')
})

test('cancelling an active model asks the worker to stop and drains the agent', async () => {
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, start)
    if (frame.type === 'checkpoint') send(proc, { type: 'checkpoint_ack', id: frame.id, ok: true, through_seq: frame.through_seq, sha256: frame.sha256 })
    if (frame.type === 'model') send(proc, { type: 'cancel' })
  })
  const [code] = await once(child, 'close')
  assert.equal(code, 0)
  assert.ok(frames.some(frame => frame.type === 'model_cancel'))
  assert.equal(frames.find(frame => frame.type === 'settled')?.state, 'cancelled')
})

test('refuses a model-requested tool until its gate is available', async () => {
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, start)
    if (frame.type === 'checkpoint') send(proc, { type: 'checkpoint_ack', id: frame.id, ok: true, through_seq: frame.through_seq, sha256: frame.sha256 })
    if (frame.type === 'model') send(proc, { type: 'model_chunk', id: frame.id, index: 0, chunk: { type: 'tool-call-delta', index: 0, id: 'call_denied', name: 'not_registered', argumentsDelta: '{}' } })
  })
  const [code] = await once(child, 'close')
  assert.equal(code, 0)
  assert.ok(frames.some(frame => frame.type === 'model_cancel'))
  assert.equal(frames.find(frame => frame.type === 'settled')?.state, 'failed')
  assert.equal(frames.filter(frame => frame.type === 'model').length, 1)
})

test('rejects a start tool with an open parameter schema', async () => {
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, {
      ...start,
      tools: [{ name: 'file_read', description: 'Read a file.', parameters: { type: 'object', properties: {}, additionalProperties: true } }],
    })
  })
  const [code] = await once(child, 'close')
  assert.equal(code, 1)
  assert.equal(frames.some(frame => frame.type === 'checkpoint' || frame.type === 'model'), false)
})

test('accepts dynamic arguments only for the Morde selected MCP broker tool', () => {
  const parameters = {
    type: 'object',
    properties: {
      connector_id: { type: 'string' },
      tool_name: { type: 'string' },
      arguments: { type: 'object' },
    },
    required: ['connector_id', 'tool_name', 'arguments'],
    additionalProperties: false,
  }
  const tool = { name: 'mcp_call', description: 'Call a selected connector tool.', parameters }
  assert.equal(validStart({ ...start, tools: [tool] }), true)
  assert.equal(validStart({ ...start, tools: [{ ...tool, name: 'other_tool' }] }), false)
  assert.equal(validStart({ ...start, tools: [{ ...tool, parameters: { ...parameters, extra: true } }] }), false)
})

test('accepts strict Morde tool schemas with nullable JSON Schema types', () => {
  const tools = [
    {
      name: 'nullable_integer', description: 'Accept an integer or null.',
      parameters: {
        type: 'object', properties: { value: { type: ['integer', 'null'], minimum: 1 } },
        required: ['value'], additionalProperties: false,
      },
    },
    {
      name: 'nullable_object', description: 'Accept an object or null.',
      parameters: {
        type: 'object', properties: {
          timing: {
            type: ['object', 'null'], properties: { time_zone: { type: 'string', minLength: 1 } },
            required: ['time_zone'], additionalProperties: false,
          },
        }, required: ['timing'], additionalProperties: false,
      },
    },
    {
      name: 'nullable_enum', description: 'Accept an enum value or null.',
      parameters: {
        type: 'object', properties: { status: { type: ['string', 'null'], enum: ['active', 'paused', null] } },
        required: ['status'], additionalProperties: false,
      },
    },
  ]
  assert.equal(validStart({ ...start, tools }), true)
  assert.equal(validStart({ ...start, tools: [{
    name: 'invalid_union', description: 'Has an unsupported type.',
    parameters: { type: 'object', properties: { value: { type: ['integer', 'unsupported'] } }, additionalProperties: false },
  }] }), false)
  assert.equal(validStart({ ...start, tools: [{
    name: 'duplicate_union', description: 'Has a duplicate type.',
    parameters: { type: 'object', properties: { value: { type: ['integer', 'integer'] } }, additionalProperties: false },
  }] }), false)
})

test('runs an authorized dsh tool through the parent and persists its result before the next model call', async () => {
  const tool = {
    name: 'file_write',
    description: 'Write a file in the task workspace.',
    parameters: {
      type: 'object',
      properties: {
        workspace_id: { type: 'string' },
        path: { type: 'string' },
        content: { type: 'string' },
      },
      required: ['workspace_id', 'path', 'content'],
      additionalProperties: false,
    },
  }
  const launchFrame = { ...start, tools: [tool] }
  let through = -1
  let modelCalls = 0
  const pages = []
  const checkpointPages = []
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, launchFrame)
    if (frame.type === 'checkpoint') {
      const page = JSON.parse(frame.payload)
      pages.push(page)
      checkpointPages.push({ from_seq: frame.from_seq, through_seq: frame.through_seq, payload: frame.payload, sha256: frame.sha256 })
      assert.equal(frame.from_seq, through + 1)
      through = frame.through_seq
      send(proc, { type: 'checkpoint_ack', id: frame.id, ok: true, through_seq: through, sha256: frame.sha256 })
    }
    if (frame.type === 'model') {
      modelCalls += 1
      assert.equal(frame.through_seq, through)
      assert.deepEqual(frame.request.tools, [tool])
      if (modelCalls === 1) {
        const chunks = [
          { type: 'tool-call-delta', index: 0, id: 'call_write_1', name: tool.name, argumentsDelta: '{"workspace_id":"w1234567","path":"notes.txt","content":"saved"}' },
          { type: 'usage', usage: { inputTokens: 3, outputTokens: 2 } },
          { type: 'finish', reason: { kind: 'tool-calls' } },
        ]
        chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
        send(proc, { type: 'model_end', id: frame.id, ok: true })
      } else {
        const resultMessage = frame.request.messages.find(message => message.role === 'tool' && message.toolCallId === 'call_write_1')
        assert.ok(resultMessage, 'next model request includes the persisted tool result message')
        assert.equal(resultMessage.content[0].text, '{"bytes_written":5}')
        const chunks = [
          { type: 'block-start', index: 0, blockType: 'text' },
          { type: 'text-delta', index: 0, text: 'file saved' },
          { type: 'block-end', index: 0, block: { type: 'text', text: 'file saved' } },
          { type: 'usage', usage: { inputTokens: 4, outputTokens: 2 } },
          { type: 'finish', reason: { kind: 'stop' } },
        ]
        chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
        send(proc, { type: 'model_end', id: frame.id, ok: true })
      }
    }
    if (frame.type === 'tool_call') {
      assert.equal(frame.task_id, start.task_id)
      assert.equal(frame.owner_id, start.owner_id)
      assert.equal(frame.agent_id, start.agent_id)
      assert.equal(frame.generation, start.generation)
      assert.equal(frame.authorization_version, start.authorization_version)
      assert.equal(frame.session_id, start.session_id)
      assert.equal(frame.through_seq, through)
      assert.equal(frame.name, tool.name)
      assert.deepEqual(frame.arguments, { workspace_id: 'w1234567', path: 'notes.txt', content: 'saved' })
      const events = pages.flatMap(page => page.events)
      assert.ok(events.some(event => event.type === 'assistant/message' && event.data.message.content.some(block => block.type === 'tool-call')))
      assert.ok(events.some(event => event.type === 'tool/call' && event.data.callId === frame.call_id))
      assert.ok(!events.some(event => event.type === 'tool/result' && event.data.message?.toolCallId === frame.call_id))
      send(proc, {
        type: 'tool_result', id: frame.id,
        task_id: frame.task_id, owner_id: frame.owner_id, agent_id: frame.agent_id,
        generation: frame.generation, authorization_version: frame.authorization_version,
        session_id: frame.session_id, call_id: frame.call_id,
        status: 'succeeded', output: '{"bytes_written":5}',
      })
    }
  })
  const [code] = await once(child, 'close')
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(modelCalls, 2)
  const events = pages.flatMap(page => page.events)
  assert.ok(events.some(event => event.type === 'tool/result' && event.data.message?.toolCallId === 'call_write_1'))
  assert.equal(frames.find(frame => frame.type === 'settled')?.state, 'completed_candidate')

  const reconciled = await resumeSession(checkpointPages, { tools: [tool], onToolCall: frame => {
    assert.equal(frame.call_id, 'call_write_1')
    assert.equal(frame.name, tool.name)
    assert.deepEqual(frame.arguments, { workspace_id: 'w1234567', path: 'notes.txt', content: 'saved' })
    return { status: 'succeeded', output: '{"bytes_written":5}' }
  } })
  assert.equal(reconciled.code, 0, `frames: ${reconciled.frames.map(frame => frame.type).join(', ')}`)
  assert.equal(reconciled.modelCalls, 0)
  assert.equal(reconciled.frames.filter(frame => frame.type === 'tool_call').length, 1)
  assert.equal(reconciled.emitted.flatMap(frame => JSON.parse(frame.payload).events)
    .filter(event => event.type === 'tool/result' && event.data.message?.toolCallId === 'call_write_1').length, 0,
  'an exact prior result must not be rewritten')

  const mismatch = await resumeSession(checkpointPages, { tools: [tool], onToolCall: () => ({ status: 'succeeded', output: '{"bytes_written":6}' }) })
  assert.notEqual(mismatch.code, 0)
  assert.equal(mismatch.modelCalls, 0)
  assert.equal(mismatch.emitted.length, 0)
})

test('routes selected Morde MCP calls through task IPC with image results and reconciles them', async () => {
  const tool = {
    name: 'mcp_call',
    description: 'Search through a task-authorized Morde connector.',
    parameters: {
      type: 'object', properties: {
        connector_id: { type: 'string' }, tool_name: { type: 'string' }, arguments: { type: 'object' },
      },
      required: ['connector_id', 'tool_name', 'arguments'], additionalProperties: false,
    },
  }
  const output = '{"is_error":false,"structured_content":{"count":1},"text":["one result"]}'
  const image = toolImage()
  const args = { connector_id: 'c1234567', tool_name: 'chart', arguments: { query: 'agent' } }
  let through = -1
  let modelCalls = 0
  let brokerCalls = 0
  const checkpointPages = []
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, { ...start, tools: [tool] })
    if (frame.type === 'checkpoint') {
      const page = JSON.parse(frame.payload)
      checkpointPages.push({ from_seq: frame.from_seq, through_seq: frame.through_seq, payload: frame.payload, sha256: frame.sha256 })
      assert.equal(frame.from_seq, through + 1)
      through = frame.through_seq
      assert.equal(page.events.at(-1).seq, through)
      send(proc, { type: 'checkpoint_ack', id: frame.id, ok: true, through_seq: through, sha256: frame.sha256 })
    }
    if (frame.type === 'model') {
      modelCalls++
      assert.deepEqual(frame.request.tools, [tool])
      if (modelCalls === 1) {
        const chunks = [
          { type: 'tool-call-delta', index: 0, id: 'call_mcp_1', name: tool.name, argumentsDelta: JSON.stringify(args) },
          { type: 'usage', usage: { inputTokens: 3, outputTokens: 2 } },
          { type: 'finish', reason: { kind: 'tool-calls' } },
        ]
        chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
        send(proc, { type: 'model_end', id: frame.id, ok: true })
      } else {
        const resultMessage = frame.request.messages.find(message => message.role === 'tool' && message.toolCallId === 'call_mcp_1')
        assert.equal(resultMessage?.content[0]?.text, output, 'structured MCP output remains available to the model')
        assert.equal(resultMessage?.content[1]?.type, 'image', 'verified MCP image is present in the next model request')
        assert.equal(resultMessage.content[1].attachment.attachmentId, image.attachment_id)
        assert.equal(resultMessage.content[1].attachment.data_base64, image.data_base64)
        const chunks = [
          { type: 'block-start', index: 0, blockType: 'text' },
          { type: 'text-delta', index: 0, text: 'Found one result.' },
          { type: 'block-end', index: 0, block: { type: 'text', text: 'Found one result.' } },
          { type: 'usage', usage: { inputTokens: 4, outputTokens: 2 } },
          { type: 'finish', reason: { kind: 'stop' } },
        ]
        chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
        send(proc, { type: 'model_end', id: frame.id, ok: true })
      }
    }
    if (frame.type === 'tool_call') {
      brokerCalls++
      assert.equal(frame.name, tool.name)
      assert.deepEqual(frame.arguments, args)
      send(proc, {
        type: 'tool_result', id: frame.id, ...taskFence(frame), call_id: frame.call_id,
        status: 'succeeded', output, images: [image],
      })
    }
  })
  const [code] = await once(child, 'close')
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(modelCalls, 2)
  assert.equal(brokerCalls, 1)
  const toolResult = checkpointPages.flatMap(page => JSON.parse(page.payload).events)
    .find(event => event.type === 'tool/result' && event.data.message?.toolCallId === 'call_mcp_1')
  assert.equal(toolResult?.data.message.content[0].text, output)
  assert.equal(toolResult?.data.message.content[1].type, 'image')
  assert.equal(toolResult.data.message.content[1].attachment.attachmentId, image.attachment_id)

  const resumed = await resumeSession(checkpointPages, {
    tools: [tool],
    onToolCall: frame => {
      assert.equal(frame.call_id, 'call_mcp_1')
      assert.equal(frame.name, tool.name)
      assert.deepEqual(frame.arguments, args)
      return { status: 'succeeded', output, images: [image] }
    },
  })
  assert.equal(resumed.code, 0, `resume frames: ${resumed.frames.map(frame => frame.type).join(', ')}`)
  assert.equal(resumed.modelCalls, 0)
  const duplicateResults = resumed.emitted.flatMap(frame => JSON.parse(frame.payload).events)
    .filter(event => event.type === 'tool/result' && event.data.message?.toolCallId === 'call_mcp_1')
  assert.equal(duplicateResults.length, 0, 'verified recovery does not append a duplicate tool result')
})

test('returns failed Morde MCP broker results to the DSH model as tool errors and completes the turn', async () => {
  const tool = {
    name: 'mcp_k1234567_0123456789ab',
    description: 'Search through a task-authorized Morde connector.',
    parameters: {
      type: 'object', properties: { query: { type: 'string' } },
      required: ['query'], additionalProperties: false,
    },
  }
  const brokerFailure = 'connector temporarily unavailable'
  const expectedToolError = `Tool failed: ${brokerFailure}`
  let modelCalls = 0
  let brokerCalls = 0
  const checkpointEvents = []
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, { ...start, tools: [tool] })
    if (frame.type === 'checkpoint') {
      checkpointEvents.push(...JSON.parse(frame.payload).events)
      acknowledgeCheckpoint(proc, frame)
    }
    if (frame.type === 'model') {
      modelCalls++
      assert.deepEqual(frame.request.tools, [tool])
      if (modelCalls === 1) {
        const chunks = [
          { type: 'tool-call-delta', index: 0, id: 'call_mcp_failed_1', name: tool.name, argumentsDelta: '{"query":"agent"}' },
          { type: 'finish', reason: { kind: 'tool-calls' } },
        ]
        chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
        send(proc, { type: 'model_end', id: frame.id, ok: true })
      } else {
        const resultMessage = frame.request.messages.find(message => message.role === 'tool' && message.toolCallId === 'call_mcp_failed_1')
        assert.equal(resultMessage?.content[0]?.text, expectedToolError)
        replyWithText(proc, frame, 'The search service is temporarily unavailable, so I could not retrieve results.')
      }
    }
    if (frame.type === 'tool_call') {
      brokerCalls++
      assert.equal(frame.name, tool.name)
      assert.deepEqual(frame.arguments, { query: 'agent' })
      send(proc, {
        type: 'tool_result', id: frame.id, ...taskFence(frame), call_id: frame.call_id,
        status: 'failed', output: brokerFailure,
      })
    }
  })
  const [code] = await once(child, 'close')
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(modelCalls, 2, 'the model receives the broker failure and gets a chance to finish')
  assert.equal(brokerCalls, 1)
  assert.equal(frames.find(frame => frame.type === 'settled')?.state, 'completed_candidate')
  const toolResult = checkpointEvents.find(event => event.type === 'tool/result' && event.data.message?.toolCallId === 'call_mcp_failed_1')
  assert.equal(toolResult?.data.message.content[0].text, expectedToolError)
  assert.ok(checkpointEvents.some(event => event.type === 'assistant/message' && event.data.message.content
    .some(block => block.type === 'text' && block.text === 'The search service is temporarily unavailable, so I could not retrieve results.')))
})

test('runs dsh_delegate in an isolated child session and waits for settlement before resuming the parent', async () => {
  const fileRead = fileReadTool
  const delegate = delegateTool
  const childIdentity = {
    task_id: start.task_id, owner_id: start.owner_id, agent_id: start.agent_id,
    generation: start.generation, authorization_version: start.authorization_version,
    session_id: 'child_789',
  }
  const throughBySession = new Map([[start.session_id, -1], [childIdentity.session_id, -1]])
  const pagesBySession = new Map()
  let parentModels = 0
  let childModels = 0
  let childSettled = false
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, { ...start, tools: [delegate, fileRead] })
    if (frame.type === 'checkpoint') {
      const through = throughBySession.get(frame.session_id) ?? -1
      assert.equal(frame.from_seq, through + 1, `independent checkpoint sequence for ${frame.session_id}`)
      throughBySession.set(frame.session_id, frame.through_seq)
      const page = { from_seq: frame.from_seq, through_seq: frame.through_seq, payload: frame.payload, sha256: frame.sha256 }
      const pages = pagesBySession.get(frame.session_id) ?? []
      pages.push(page)
      pagesBySession.set(frame.session_id, pages)
      send(proc, { type: 'checkpoint_ack', id: frame.id, ok: true, through_seq: frame.through_seq, sha256: frame.sha256 })
    }
    if (frame.type === 'model') {
      assert.ok(frame.request.messages.some(message => message.role === 'system' &&
        message.content.some(block => block.text?.includes("You are Morde, the user's AI assistant."))))
      if (frame.session_id === start.session_id) {
        parentModels += 1
        assert.deepEqual(frame.request.tools, [delegate, fileRead])
        if (parentModels === 1) {
          const chunks = [
            { type: 'tool-call-delta', index: 0, id: 'call_delegate_1', name: delegate.name,
              argumentsDelta: '{"title":"Inspect notes","objective":"Read notes.txt and summarize it.","tool_names":["file_read"]}' },
            { type: 'usage', usage: { inputTokens: 3, outputTokens: 2 } },
            { type: 'finish', reason: { kind: 'tool-calls' } },
          ]
          chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
        } else {
          assert.equal(childSettled, true, 'parent must not resume before Go acknowledges child settlement')
          const result = frame.request.messages.find(message => message.role === 'tool' && message.toolCallId === 'call_delegate_1')
          assert.equal(result?.content[0].text, 'Notes say hello.')
          const chunks = [
            { type: 'block-start', index: 0, blockType: 'text' },
            { type: 'text-delta', index: 0, text: 'Task complete.' },
            { type: 'block-end', index: 0, block: { type: 'text', text: 'Task complete.' } },
            { type: 'usage', usage: { inputTokens: 2, outputTokens: 1 } },
            { type: 'finish', reason: { kind: 'stop' } },
          ]
          chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
        }
      } else {
        childModels += 1
        assert.equal(frame.session_id, childIdentity.session_id)
        assert.deepEqual(frame.request.tools, [fileRead], 'child receives only its granted application tool')
        if (childModels === 1) {
          assert.ok(frame.request.messages.some(message => message.role === 'user' && message.content.some(block => block.text.includes('Read notes.txt'))))
          const chunks = [
            { type: 'tool-call-delta', index: 0, id: 'call_child_read_1', name: fileRead.name, argumentsDelta: '{"path":"notes.txt"}' },
            { type: 'usage', usage: { inputTokens: 2, outputTokens: 1 } },
            { type: 'finish', reason: { kind: 'tool-calls' } },
          ]
          chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
        } else {
          const result = frame.request.messages.find(message => message.role === 'tool' && message.toolCallId === 'call_child_read_1')
          assert.equal(result?.content[0].text, 'notes.txt contents')
          const chunks = [
            { type: 'block-start', index: 0, blockType: 'text' },
            { type: 'text-delta', index: 0, text: 'Notes say hello.' },
            { type: 'block-end', index: 0, block: { type: 'text', text: 'Notes say hello.' } },
            { type: 'usage', usage: { inputTokens: 2, outputTokens: 1 } },
            { type: 'finish', reason: { kind: 'stop' } },
          ]
          chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
        }
      }
      send(proc, { type: 'model_end', id: frame.id, ok: true })
    }
    if (frame.type === 'child_create') {
      assert.equal(frame.session_id, start.session_id)
      assert.equal(frame.call_id, 'call_delegate_1')
      assert.equal(frame.through_seq, throughBySession.get(start.session_id))
      assert.deepEqual(frame.arguments, { title: 'Inspect notes', objective: 'Read notes.txt and summarize it.', tool_names: ['file_read'] })
      send(proc, {
        type: 'child_session', call_id: frame.call_id, ...childIdentity,
        parent_session_id: frame.session_id, objective: frame.arguments.objective,
        tools: [fileRead], checkpoint_through_seq: -1, checkpoint_sha256: '',
        step_status: 'running', summary: '',
      })
    }
    if (frame.type === 'tool_call') {
      assert.equal(frame.name, fileRead.name, 'dsh_delegate must never be sent through ordinary tool_call IPC')
      assert.equal(frame.session_id, childIdentity.session_id)
      assert.deepEqual(frame.arguments, { path: 'notes.txt' })
      send(proc, {
        type: 'tool_result', id: frame.id, task_id: frame.task_id, owner_id: frame.owner_id,
        agent_id: frame.agent_id, generation: frame.generation, authorization_version: frame.authorization_version,
        session_id: frame.session_id, call_id: frame.call_id, status: 'succeeded', output: 'notes.txt contents',
      })
    }
    if (frame.type === 'child_settle') {
      assert.equal(frame.session_id, childIdentity.session_id)
      assert.equal(frame.call_id, 'call_delegate_1')
      assert.equal(frame.status, 'succeeded')
      assert.equal(frame.summary, 'Notes say hello.')
      assert.ok(frame.through_seq >= 0)
      assert.equal(frame.sha256, pagesBySession.get(childIdentity.session_id).at(-1).sha256)
      send(proc, { type: 'child_settle_ack', ...taskFence(frame), call_id: frame.call_id, ok: true })
      childSettled = true
    }
  })
  const [code] = await once(child, 'close')
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(parentModels, 2, frames.map(frame => frame.type).join(', '))
  assert.equal(childModels, 2)
  assert.equal(frames.filter(frame => frame.type === 'child_create').length, 1)
  assert.equal(frames.filter(frame => frame.type === 'child_settle').length, 1)
  assert.equal(frames.filter(frame => frame.type === 'tool_call').length, 1)
  assert.equal(frames.filter(frame => frame.type === 'settled').at(-1)?.state, 'completed_candidate')
  assert.ok(pagesBySession.get(start.session_id)?.length > 0)
  assert.ok(pagesBySession.get(childIdentity.session_id)?.length > 0)
})

test('runs a background child beside the parent and exposes durable job status tools', async () => {
  const childIdentity = {
    task_id: start.task_id, owner_id: start.owner_id, agent_id: start.agent_id,
    generation: start.generation, authorization_version: start.authorization_version,
    session_id: 'child_background_1',
  }
  const tools = [delegateTool, fileReadTool, ...jobTools].sort((left, right) => left.name.localeCompare(right.name))
  const throughBySession = new Map([[start.session_id, -1], [childIdentity.session_id, -1]])
  let parentModels = 0
  let pendingChildModel
  let childSettled = false
  const sendModel = (proc, frame, text, call) => {
    const chunks = call ? [
      { type: 'tool-call-delta', index: 0, id: call.id, name: call.name, argumentsDelta: call.arguments },
      { type: 'usage', usage: { inputTokens: 2, outputTokens: 1 } },
      { type: 'finish', reason: { kind: 'tool-calls' } },
    ] : [
      { type: 'block-start', index: 0, blockType: 'text' },
      { type: 'text-delta', index: 0, text },
      { type: 'block-end', index: 0, block: { type: 'text', text } },
      { type: 'usage', usage: { inputTokens: 2, outputTokens: 1 } },
      { type: 'finish', reason: { kind: 'stop' } },
    ]
    chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
    send(proc, { type: 'model_end', id: frame.id, ok: true })
  }
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, { ...start, tools })
    if (frame.type === 'checkpoint') {
      assert.equal(frame.from_seq, (throughBySession.get(frame.session_id) ?? -1) + 1)
      throughBySession.set(frame.session_id, frame.through_seq)
      send(proc, { type: 'checkpoint_ack', id: frame.id, ok: true, through_seq: frame.through_seq, sha256: frame.sha256 })
    }
    if (frame.type === 'model' && frame.session_id === start.session_id) {
      parentModels += 1
      assert.deepEqual(frame.request.tools, tools)
      if (parentModels === 1) {
        sendModel(proc, frame, '', { id: 'call_background_1', name: delegateTool.name,
          arguments: JSON.stringify({ title: 'Background task', objective: 'Finish a delegated task.', tool_names: ['file_read'], run_in_background: true }) })
      } else if (parentModels === 2) {
        const delegated = frame.request.messages.find(message => message.role === 'tool' && message.toolCallId === 'call_background_1')
        assert.match(delegated?.content[0].text ?? '', /"job_id":"child_background_1"/)
        sendModel(proc, frame, '', { id: 'call_job_get_1', name: jobGetTool.name,
          arguments: JSON.stringify({ job_id: childIdentity.session_id }) })
      } else {
        const status = frame.request.messages.find(message => message.role === 'tool' && message.toolCallId === 'call_job_get_1')
        assert.equal(JSON.parse(status.content[0].text).status, 'running')
        assert.equal(childSettled, false, 'parent can continue while its background child is still running')
        sendModel(proc, frame, 'Parent task finished.', undefined)
      }
    }
    if (frame.type === 'model' && frame.session_id === childIdentity.session_id) {
      assert.deepEqual(frame.request.tools, [fileReadTool])
      pendingChildModel = frame
      if (parentModels >= 3) sendModel(proc, frame, 'Background task finished.', undefined)
    }
    if (frame.type === 'child_create') send(proc, {
      type: 'child_session', call_id: frame.call_id, ...childIdentity,
      parent_session_id: frame.session_id, objective: frame.arguments.objective,
      tools: [fileReadTool], checkpoint_through_seq: -1, checkpoint_sha256: '',
      step_status: 'running', summary: '',
    })
    if (frame.type === 'tool_call') {
      assert.equal(frame.name, jobGetTool.name)
      send(proc, { type: 'tool_result', id: frame.id, task_id: frame.task_id, owner_id: frame.owner_id,
        agent_id: frame.agent_id, generation: frame.generation, authorization_version: frame.authorization_version,
        session_id: frame.session_id, call_id: frame.call_id, status: 'succeeded',
        output: JSON.stringify({ job_id: childIdentity.session_id, status: 'running', title: 'Background task', summary: '', output_cursor: 2 }) })
    }
    if (frame.type === 'child_settle') {
      assert.equal(frame.session_id, childIdentity.session_id)
      assert.equal(frame.status, 'succeeded')
      assert.equal(frame.summary, 'Background task finished.')
      send(proc, { type: 'child_settle_ack', ...taskFence(frame), call_id: frame.call_id, ok: true })
      childSettled = true
    }
    if (frame.type === 'model' && frame.session_id === start.session_id && parentModels === 3 && pendingChildModel) {
      sendModel(proc, pendingChildModel, 'Background task finished.', undefined)
      pendingChildModel = undefined
    }
  })
  const [code] = await once(child, 'close')
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(parentModels, 3)
  assert.equal(frames.filter(frame => frame.type === 'child_create').length, 1)
  assert.equal(frames.filter(frame => frame.type === 'child_settle').length, 1)
  assert.equal(frames.filter(frame => frame.type === 'tool_call').length, 1)
  assert.equal(frames.find(frame => frame.type === 'child_settle')?.call_id, 'call_background_1')
  assert.equal(frames.filter(frame => frame.type === 'settled').at(-1)?.state, 'completed_candidate')
})

test('does not settle the parent while a background child outcome is unknown', async () => {
  const childIdentity = {
    task_id: start.task_id, owner_id: start.owner_id, agent_id: start.agent_id,
    generation: start.generation, authorization_version: start.authorization_version,
    session_id: 'child_background_unknown',
  }
  const tools = [delegateTool, fileReadTool, ...jobTools].sort((left, right) => left.name.localeCompare(right.name))
  let parentModels = 0
  let childModels = 0
  const sendDelegate = (proc, frame) => {
    const chunks = [
      { type: 'tool-call-delta', index: 0, id: 'call_background_unknown', name: delegateTool.name,
        argumentsDelta: JSON.stringify({ title: 'Background task', objective: 'Finish a delegated task.', tool_names: ['file_read'], run_in_background: true }) },
      { type: 'usage', usage: { inputTokens: 2, outputTokens: 1 } },
      { type: 'finish', reason: { kind: 'tool-calls' } },
    ]
    chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
    send(proc, { type: 'model_end', id: frame.id, ok: true })
  }
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, { ...start, tools })
    if (frame.type === 'checkpoint') acknowledgeCheckpoint(proc, frame)
    if (frame.type === 'model' && frame.session_id === start.session_id) {
      parentModels += 1
      if (parentModels === 1) sendDelegate(proc, frame)
      else replyWithText(proc, frame, 'Parent task finished.')
    }
    if (frame.type === 'model' && frame.session_id === childIdentity.session_id) {
      childModels += 1
      replyWithText(proc, frame, 'Background task finished.')
    }
    if (frame.type === 'child_create') send(proc, {
      type: 'child_session', call_id: frame.call_id, ...childIdentity,
      parent_session_id: frame.session_id, objective: frame.arguments.objective,
      tools: [fileReadTool], checkpoint_through_seq: -1, checkpoint_sha256: '',
      step_status: 'running', summary: '',
    })
    if (frame.type === 'child_settle') send(proc, { type: 'child_settle_ack', ...taskFence(frame), call_id: frame.call_id, ok: false })
  })
  const close = once(child, 'close').then(([code]) => code)
  const timeout = setTimeout(() => child.kill('SIGKILL'), 5000)
  timeout.unref()
  const code = await close
  clearTimeout(timeout)
  assert.equal(code, 1, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(parentModels, 2)
  assert.equal(childModels, 1)
  assert.equal(frames.some(frame => frame.type === 'child_settle'), true)
  assert.equal(frames.some(frame => frame.type === 'settled'), false)
})

test('cancels a persisted background job and reports only after child settlement', async () => {
  const childIdentity = {
    task_id: start.task_id, owner_id: start.owner_id, agent_id: start.agent_id,
    generation: start.generation, authorization_version: start.authorization_version,
    session_id: 'child_background_cancel',
  }
  const tools = [delegateTool, fileReadTool, ...jobTools].sort((left, right) => left.name.localeCompare(right.name))
  let rootModels = 0
  let childSettled = false
  let cancelCall
  const sendModel = (proc, frame, text, call) => {
    const chunks = call ? [
      { type: 'tool-call-delta', index: 0, id: call.id, name: call.name, argumentsDelta: call.arguments },
      { type: 'usage', usage: { inputTokens: 2, outputTokens: 1 } },
      { type: 'finish', reason: { kind: 'tool-calls' } },
    ] : [
      { type: 'block-start', index: 0, blockType: 'text' },
      { type: 'text-delta', index: 0, text },
      { type: 'block-end', index: 0, block: { type: 'text', text } },
      { type: 'usage', usage: { inputTokens: 2, outputTokens: 1 } },
      { type: 'finish', reason: { kind: 'stop' } },
    ]
    chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
    send(proc, { type: 'model_end', id: frame.id, ok: true })
  }
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, { ...start, tools })
    if (frame.type === 'checkpoint') send(proc, {
      type: 'checkpoint_ack', id: frame.id, ok: true, through_seq: frame.through_seq, sha256: frame.sha256,
    })
    if (frame.type === 'model' && frame.session_id === start.session_id) {
      rootModels += 1
      if (rootModels === 1) sendModel(proc, frame, '', { id: 'call_background_cancel', name: delegateTool.name,
        arguments: JSON.stringify({ title: 'Cancelable task', objective: 'Work until cancelled.', tool_names: ['file_read'], run_in_background: true }) })
      else if (rootModels === 2) sendModel(proc, frame, '', { id: 'call_cancel_job', name: jobCancelTool.name,
        arguments: JSON.stringify({ job_id: childIdentity.session_id }) })
      else {
        assert.equal(childSettled, true)
        const result = frame.request.messages.find(message => message.role === 'tool' && message.toolCallId === 'call_cancel_job')
        assert.equal(JSON.parse(result.content[0].text).status, 'cancelled')
        sendModel(proc, frame, 'Cancelled safely.', undefined)
      }
    }
    if (frame.type === 'model' && frame.session_id === childIdentity.session_id) {
      assert.deepEqual(frame.request.tools, [fileReadTool])
    }
    if (frame.type === 'child_create') send(proc, {
      type: 'child_session', call_id: frame.call_id, ...childIdentity,
      parent_session_id: frame.session_id, objective: frame.arguments.objective,
      tools: [fileReadTool], checkpoint_through_seq: -1, checkpoint_sha256: '',
      step_status: 'running', summary: '',
    })
    if (frame.type === 'child_settle') {
      assert.equal(frame.status, 'failed')
      assert.equal(frame.summary, 'Background job cancelled.')
      send(proc, { type: 'child_settle_ack', ...taskFence(frame), call_id: frame.call_id, ok: true })
      childSettled = true
    }
    if (frame.type === 'job_cancel_result') {
      assert.equal(frame.job_id, childIdentity.session_id)
      assert.equal(frame.status, 'cancelled')
      assert.equal(childSettled, true)
      send(proc, { type: 'tool_result', id: cancelCall.id, task_id: start.task_id, owner_id: start.owner_id,
        agent_id: start.agent_id, generation: start.generation, authorization_version: start.authorization_version,
        session_id: start.session_id, call_id: cancelCall.call_id, status: 'succeeded',
        output: JSON.stringify({ job_id: childIdentity.session_id, status: 'cancelled', title: 'Cancelable task', summary: 'Background job cancelled.', output_cursor: 3 }) })
    }
    if (frame.type === 'tool_call' && frame.name === jobCancelTool.name) {
      cancelCall = frame
      send(proc, { type: 'job_cancel', id: 'job_cancel_control_1', task_id: childIdentity.task_id,
        owner_id: childIdentity.owner_id, agent_id: childIdentity.agent_id,
        generation: childIdentity.generation, authorization_version: childIdentity.authorization_version,
        session_id: start.session_id, job_id: childIdentity.session_id })
    }
  })
  const [code] = await once(child, 'close')
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(frames.filter(frame => frame.type === 'job_cancel_result').length, 1)
  assert.equal(frames.filter(frame => frame.type === 'child_settle').length, 1)
  assert.equal(frames.find(frame => frame.type === 'settled')?.state, 'completed_candidate')
})

test('replays an interrupted delegate using the stored child summary, and fails closed for unknown child state', async () => {
  const pages = await captureInterruptedDelegateSession()
  const childSession = frame => ({
    type: 'child_session', call_id: frame.call_id,
    task_id: frame.task_id, owner_id: frame.owner_id, agent_id: frame.agent_id,
    generation: frame.generation, authorization_version: frame.authorization_version,
    session_id: 'child_789', parent_session_id: frame.session_id,
    objective: frame.arguments.objective, tools: [fileReadTool],
    checkpoint_through_seq: -1, checkpoint_sha256: '',
  })
  const resumed = await resumeSession(pages, {
    tools: [delegateTool, fileReadTool],
    onChildCreate(frame) {
      assert.equal(frame.call_id, 'call_delegate_1')
      return { ...childSession(frame), step_status: 'succeeded', summary: 'Previously completed summary.' }
    },
  })
  assert.equal(resumed.code, 0, `frames: ${resumed.frames.map(frame => frame.type).join(', ')}`)
  assert.equal(resumed.modelCalls, 1)
  assert.equal(resumed.frames.filter(frame => frame.type === 'child_create').length, 1)
  assert.equal(resumed.frames.some(frame => frame.type === 'tool_call' || frame.type === 'child_settle'), false)
  assert.equal(resumed.modelRequests[0].messages.find(message => message.role === 'tool' && message.toolCallId === 'call_delegate_1')?.content[0].text,
    'Previously completed summary.')

  const unknown = await resumeSession(pages, {
    tools: [delegateTool, fileReadTool],
    onChildCreate: frame => ({ ...childSession(frame), step_status: 'unknown', summary: '' }),
  })
  assert.notEqual(unknown.code, 0)
  assert.equal(unknown.modelCalls, 0)
  assert.equal(unknown.frames.some(frame => frame.type === 'tool_call' || frame.type === 'child_settle' || frame.type === 'model'), false)
})

test('resumes an interrupted child from its own checkpoint before settling the parent delegate', async () => {
  const pagesBySession = await captureInterruptedDelegateChildSession()
  const parentPages = pagesBySession.get(start.session_id)
  const childPages = pagesBySession.get('child_789')
  assert.ok(parentPages?.length)
  assert.ok(childPages?.length)
  const resumed = await resumeSession(parentPages, {
    tools: [delegateTool, fileReadTool],
    checkpointPagesBySession: new Map([['child_789', childPages]]),
    onChildCreate(frame) {
      const tail = childPages.at(-1)
      return {
        type: 'child_session', call_id: frame.call_id,
        task_id: frame.task_id, owner_id: frame.owner_id, agent_id: frame.agent_id,
        generation: frame.generation, authorization_version: frame.authorization_version,
        session_id: 'child_789', parent_session_id: frame.session_id,
        objective: frame.arguments.objective, tools: [fileReadTool],
        checkpoint_through_seq: tail.through_seq, checkpoint_sha256: tail.sha256,
        step_status: 'running', summary: '',
      }
    },
  })
  assert.equal(resumed.code, 0, `frames: ${resumed.frames.map(frame => frame.type).join(', ')}`)
  assert.ok(resumed.frames.some(frame => frame.type === 'checkpoint_read' && frame.session_id === 'child_789'))
  assert.ok(resumed.frames.some(frame => frame.type === 'model' && frame.session_id === 'child_789'))
  assert.equal(resumed.frames.filter(frame => frame.type === 'child_create').length, 1)
  assert.equal(resumed.frames.filter(frame => frame.type === 'child_settle').length, 1)
  const settlement = resumed.frames.find(frame => frame.type === 'child_settle')
  assert.equal(settlement.call_id, 'call_delegate_1')
  assert.equal(settlement.status, 'succeeded')
  const parentAfterSettlement = resumed.frames.find(frame => frame.type === 'model' && frame.session_id === start.session_id)
  assert.ok(parentAfterSettlement?.request.messages.some(message => message.role === 'tool' && message.toolCallId === 'call_delegate_1' &&
    message.content[0].text === 'resumed answer'))
})

test('cancelling a running child never sends a successful settlement', async () => {
  const childIdentity = {
    task_id: start.task_id, owner_id: start.owner_id, agent_id: start.agent_id,
    generation: start.generation, authorization_version: start.authorization_version,
    session_id: 'child_789',
  }
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, { ...start, tools: [delegateTool, fileReadTool] })
    if (frame.type === 'checkpoint') send(proc, {
      type: 'checkpoint_ack', id: frame.id, ok: true, through_seq: frame.through_seq, sha256: frame.sha256,
    })
    if (frame.type === 'model' && frame.session_id === start.session_id) {
      const chunks = [
        { type: 'tool-call-delta', index: 0, id: 'call_delegate_1', name: delegateTool.name,
          argumentsDelta: '{"title":"Inspect notes","objective":"Read notes.txt and summarize it.","tool_names":["file_read"]}' },
        { type: 'usage', usage: { inputTokens: 3, outputTokens: 2 } },
        { type: 'finish', reason: { kind: 'tool-calls' } },
      ]
      chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
      send(proc, { type: 'model_end', id: frame.id, ok: true })
    } else if (frame.type === 'model' && frame.session_id === childIdentity.session_id) {
      send(proc, { type: 'cancel' })
    }
    if (frame.type === 'child_create') send(proc, {
      type: 'child_session', call_id: frame.call_id, ...childIdentity,
      parent_session_id: frame.session_id, objective: frame.arguments.objective,
      tools: [fileReadTool], checkpoint_through_seq: -1, checkpoint_sha256: '',
      step_status: 'running', summary: '',
    })
  })
  const [code] = await once(child, 'close')
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(frames.some(frame => frame.type === 'child_settle'), false)
  assert.equal(frames.find(frame => frame.type === 'settled')?.state, 'cancelled')
})

test('rejects ask_user mixed with a Host tool before dispatching either tool', async () => {
  const clarification = {
    name: 'ask_user',
    description: 'Ask the user for one missing detail. Use alone.',
    parameters: {
      type: 'object',
      properties: { question: { type: 'string' } },
      required: ['question'],
      additionalProperties: false,
    },
  }
  const hostTool = {
    name: 'file_read',
    description: 'Read a workspace file.',
    parameters: { type: 'object', properties: {}, additionalProperties: false },
  }
  let through = -1
  let modelCalls = 0
  let toolCalls = 0
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, { ...start, tools: [hostTool, clarification] })
    if (frame.type === 'checkpoint') {
      through = frame.through_seq
      send(proc, { type: 'checkpoint_ack', id: frame.id, ok: true, through_seq: through, sha256: frame.sha256 })
    }
    if (frame.type === 'model') {
      modelCalls += 1
      const chunks = [
        { type: 'tool-call-delta', index: 0, id: 'call_read_1', name: hostTool.name, argumentsDelta: '{}' },
        { type: 'tool-call-delta', index: 1, id: 'call_ask_1', name: clarification.name, argumentsDelta: '{"question":"Which environment?"}' },
        { type: 'usage', usage: { inputTokens: 3, outputTokens: 2 } },
        { type: 'finish', reason: { kind: 'tool-calls' } },
      ]
      chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
      send(proc, { type: 'model_end', id: frame.id, ok: true })
    }
    if (frame.type === 'tool_call') toolCalls += 1
  })
  const [code] = await once(child, 'close')
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(modelCalls, 1)
  assert.equal(toolCalls, 0, 'no IPC tool call may start before the mixed turn is rejected')
  assert.equal(frames.find(frame => frame.type === 'settled')?.state, 'failed')
})

test('resumes a standalone ask_user call with the exact user answer as its tool result', async () => {
  const clarification = {
    name: 'ask_user',
    description: 'Ask the user for one missing detail. Use alone.',
    parameters: {
      type: 'object',
      properties: { question: { type: 'string', minLength: 1, maxLength: 4096 } },
      required: ['question'],
      additionalProperties: false,
    },
  }
  const pages = []
  let through = -1
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, { ...start, tools: [clarification] })
    if (frame.type === 'checkpoint') {
      pages.push({ from_seq: frame.from_seq, through_seq: frame.through_seq, payload: frame.payload, sha256: frame.sha256 })
      through = frame.through_seq
      send(proc, { type: 'checkpoint_ack', id: frame.id, ok: true, through_seq: through, sha256: frame.sha256 })
    }
    if (frame.type === 'model') {
      const chunks = [
        { type: 'tool-call-delta', index: 0, id: 'call_ask_1', name: clarification.name, argumentsDelta: '{"question":"Which environment?"}' },
        { type: 'usage', usage: { inputTokens: 3, outputTokens: 2 } },
        { type: 'finish', reason: { kind: 'tool-calls' } },
      ]
      chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
      send(proc, { type: 'model_end', id: frame.id, ok: true })
    }
    if (frame.type === 'tool_call') {
      assert.equal(frame.call_id, 'call_ask_1')
      assert.equal(frame.name, clarification.name)
      proc.stdin.end()
    }
  })
  const [code] = await once(child, 'close')
  assert.notEqual(code, 0)
  assert.ok(pages.length > 0)
  assert.ok(frames.some(frame => frame.type === 'tool_call'))

  const resumed = await resumeSession(pages, {
    tools: [clarification],
    onToolCall: frame => {
      assert.equal(frame.call_id, 'call_ask_1')
      assert.equal(frame.name, clarification.name)
      return { status: 'succeeded', output: 'Use staging.' }
    },
  })
  assert.equal(resumed.code, 0, `frames: ${resumed.frames.map(frame => frame.type).join(', ')}`)
  assert.equal(resumed.modelCalls, 1)
  const answer = resumed.modelRequests[0].messages.find(message => message.role === 'tool' && message.toolCallId === 'call_ask_1')
  assert.equal(answer?.content[0].text, 'Use staging.')
  assert.equal(resumed.frames.filter(frame => frame.type === 'tool_call').length, 1)
})

test('rejects an unknown tool result without starting another model call', async () => {
  const tool = {
    name: 'file_read', description: 'Read a workspace file.',
    parameters: { type: 'object', properties: {}, additionalProperties: false },
  }
  let modelCalls = 0
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, { ...start, tools: [tool] })
    if (frame.type === 'checkpoint') send(proc, { type: 'checkpoint_ack', id: frame.id, ok: true, through_seq: frame.through_seq, sha256: frame.sha256 })
    if (frame.type === 'model') {
      modelCalls += 1
      const chunks = [
        { type: 'tool-call-delta', index: 0, id: 'call_unknown_1', name: tool.name, argumentsDelta: '{}' },
        { type: 'usage', usage: { inputTokens: 1, outputTokens: 1 } },
        { type: 'finish', reason: { kind: 'tool-calls' } },
      ]
      chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
      send(proc, { type: 'model_end', id: frame.id, ok: true })
    }
    if (frame.type === 'tool_call') {
      send(proc, {
        type: 'tool_result', id: frame.id,
        task_id: frame.task_id, owner_id: frame.owner_id, agent_id: frame.agent_id,
        generation: frame.generation, authorization_version: frame.authorization_version,
        session_id: frame.session_id, call_id: frame.call_id,
        status: 'unknown', output: 'operation state is unknown',
      })
    }
  })
  const [code] = await once(child, 'close')
  assert.equal(code, 1)
  assert.equal(modelCalls, 1)
  assert.equal(frames.some(frame => frame.type === 'settled'), false)
})

test('fails closed when the parent disconnects before confirming a tool result', async () => {
  const tool = {
    name: 'file_read', description: 'Read a workspace file.',
    parameters: { type: 'object', properties: {}, additionalProperties: false },
  }
  let modelCalls = 0
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, { ...start, tools: [tool] })
    if (frame.type === 'checkpoint') send(proc, { type: 'checkpoint_ack', id: frame.id, ok: true, through_seq: frame.through_seq, sha256: frame.sha256 })
    if (frame.type === 'model') {
      modelCalls += 1
      const chunks = [
        { type: 'tool-call-delta', index: 0, id: 'call_disconnected_1', name: tool.name, argumentsDelta: '{}' },
        { type: 'usage', usage: { inputTokens: 1, outputTokens: 1 } },
        { type: 'finish', reason: { kind: 'tool-calls' } },
      ]
      chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
      send(proc, { type: 'model_end', id: frame.id, ok: true })
    }
    if (frame.type === 'tool_call') proc.stdin.end()
  })
  const [code] = await once(child, 'close')
  assert.equal(code, 1)
  assert.equal(modelCalls, 1)
  assert.equal(frames.some(frame => frame.type === 'settled'), false)
})

test('refuses invalid UTF-8 in a worker frame', async () => {
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') {
      const [prefix, suffix] = JSON.stringify({ ...start, prompt: 'utf8_marker' }).split('utf8_marker')
      proc.stdin.write(Buffer.concat([Buffer.from(prefix), Buffer.from([0xc3, 0x28]), Buffer.from(`${suffix}\n`)]))
    }
  })
  const timer = setTimeout(() => child.kill('SIGKILL'), 5000)
  const [code] = await once(child, 'close')
  clearTimeout(timer)
  assert.equal(code, 1)
  assert.equal(frames.some(frame => frame.type === 'checkpoint' || frame.type === 'model'), false)
})

async function captureSession({ interrupt = false } = {}) {
  const pages = []
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, start)
    if (frame.type === 'checkpoint') {
      pages.push({ from_seq: frame.from_seq, through_seq: frame.through_seq, payload: frame.payload, sha256: frame.sha256 })
      send(proc, { type: 'checkpoint_ack', id: frame.id, ok: true, through_seq: frame.through_seq, sha256: frame.sha256 })
    }
    if (frame.type === 'model') {
      if (interrupt) {
        proc.kill('SIGKILL')
        return
      }
      const chunks = [
        { type: 'block-start', index: 0, blockType: 'text' },
        { type: 'text-delta', index: 0, text: 'saved answer' },
        { type: 'block-end', index: 0, block: { type: 'text', text: 'saved answer' } },
        { type: 'usage', usage: { inputTokens: 2, outputTokens: 1 } },
        { type: 'finish', reason: { kind: 'stop' } },
      ]
      chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
      send(proc, { type: 'model_end', id: frame.id, ok: true })
    }
  })
  const [code, signal] = await once(child, 'close')
  if (interrupt) assert.equal(signal, 'SIGKILL')
  else assert.equal(code, 0)
  assert.ok(pages.length > 0)
  assert.ok(frames.some(frame => frame.type === 'model'))
  return pages
}

async function captureInterruptedToolSession(tool, argumentsJSON = '{"workspace_id":"w1234567","path":"notes.txt","content":"saved"}', beforeKill) {
  const pages = []
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, { ...start, tools: [tool] })
    if (frame.type === 'checkpoint') {
      pages.push({ from_seq: frame.from_seq, through_seq: frame.through_seq, payload: frame.payload, sha256: frame.sha256 })
      send(proc, { type: 'checkpoint_ack', id: frame.id, ok: true, through_seq: frame.through_seq, sha256: frame.sha256 })
    }
    if (frame.type === 'model') {
      const chunks = [
        { type: 'tool-call-delta', index: 0, id: 'call_interrupted_1', name: tool.name, argumentsDelta: argumentsJSON },
        { type: 'usage', usage: { inputTokens: 3, outputTokens: 2 } },
        { type: 'finish', reason: { kind: 'tool-calls' } },
      ]
      chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
      send(proc, { type: 'model_end', id: frame.id, ok: true })
    }
    if (frame.type === 'tool_call') {
      beforeKill?.(frame)
      proc.kill('SIGKILL')
    }
  })
  const [code, signal] = await once(child, 'close')
  assert.equal(signal, 'SIGKILL')
  assert.ok(frames.some(frame => frame.type === 'tool_call'))
  const events = pages.flatMap(page => JSON.parse(page.payload).events)
  assert.ok(events.some(event => event.type === 'tool/call' && event.data.callId === 'call_interrupted_1'))
  assert.equal(events.some(event => event.type === 'tool/result' && event.data.message?.toolCallId === 'call_interrupted_1'), false)
  assert.equal(code, null)
  return pages
}

async function captureInterruptedDelegateSession() {
  const pages = []
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, { ...start, tools: [delegateTool, fileReadTool] })
    if (frame.type === 'checkpoint') {
      pages.push({ from_seq: frame.from_seq, through_seq: frame.through_seq, payload: frame.payload, sha256: frame.sha256 })
      send(proc, { type: 'checkpoint_ack', id: frame.id, ok: true, through_seq: frame.through_seq, sha256: frame.sha256 })
    }
    if (frame.type === 'model') {
      const chunks = [
        { type: 'tool-call-delta', index: 0, id: 'call_delegate_1', name: delegateTool.name,
          argumentsDelta: '{"title":"Inspect notes","objective":"Read notes.txt and summarize it.","tool_names":["file_read"]}' },
        { type: 'usage', usage: { inputTokens: 3, outputTokens: 2 } },
        { type: 'finish', reason: { kind: 'tool-calls' } },
      ]
      chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
      send(proc, { type: 'model_end', id: frame.id, ok: true })
    }
    if (frame.type === 'child_create') proc.kill('SIGKILL')
  })
  const [code, signal] = await once(child, 'close')
  assert.equal(signal, 'SIGKILL')
  assert.equal(code, null)
  assert.ok(frames.some(frame => frame.type === 'child_create'))
  const events = pages.flatMap(page => JSON.parse(page.payload).events)
  assert.ok(events.some(event => event.type === 'tool/call' && event.data.callId === 'call_delegate_1'))
  assert.equal(events.some(event => event.type === 'tool/result' && event.data.message?.toolCallId === 'call_delegate_1'), false)
  return pages
}

async function captureInterruptedDelegateChildSession() {
  const pagesBySession = new Map()
  const throughBySession = new Map()
  const childIdentity = {
    task_id: start.task_id, owner_id: start.owner_id, agent_id: start.agent_id,
    generation: start.generation, authorization_version: start.authorization_version,
    session_id: 'child_789',
  }
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, { ...start, tools: [delegateTool, fileReadTool] })
    if (frame.type === 'checkpoint') {
      const previous = throughBySession.get(frame.session_id) ?? -1
      assert.equal(frame.from_seq, previous + 1)
      throughBySession.set(frame.session_id, frame.through_seq)
      const pages = pagesBySession.get(frame.session_id) ?? []
      pages.push({ from_seq: frame.from_seq, through_seq: frame.through_seq, payload: frame.payload, sha256: frame.sha256 })
      pagesBySession.set(frame.session_id, pages)
      send(proc, { type: 'checkpoint_ack', id: frame.id, ok: true, through_seq: frame.through_seq, sha256: frame.sha256 })
    }
    if (frame.type === 'model' && frame.session_id === start.session_id) {
      const chunks = [
        { type: 'tool-call-delta', index: 0, id: 'call_delegate_1', name: delegateTool.name,
          argumentsDelta: '{"title":"Inspect notes","objective":"Read notes.txt and summarize it.","tool_names":["file_read"]}' },
        { type: 'usage', usage: { inputTokens: 3, outputTokens: 2 } },
        { type: 'finish', reason: { kind: 'tool-calls' } },
      ]
      chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
      send(proc, { type: 'model_end', id: frame.id, ok: true })
    } else if (frame.type === 'model' && frame.session_id === childIdentity.session_id) {
      proc.kill('SIGKILL')
    }
    if (frame.type === 'child_create') {
      send(proc, {
        type: 'child_session', call_id: frame.call_id, ...childIdentity,
        parent_session_id: frame.session_id, objective: frame.arguments.objective,
        tools: [fileReadTool], checkpoint_through_seq: -1, checkpoint_sha256: '',
        step_status: 'running', summary: '',
      })
    }
  })
  const [code, signal] = await once(child, 'close')
  assert.equal(signal, 'SIGKILL')
  assert.equal(code, null)
  assert.ok(frames.some(frame => frame.type === 'model' && frame.session_id === childIdentity.session_id))
  const parentEvents = pagesBySession.get(start.session_id).flatMap(page => JSON.parse(page.payload).events)
  assert.ok(parentEvents.some(event => event.type === 'tool/call' && event.data.callId === 'call_delegate_1'))
  assert.equal(parentEvents.some(event => event.type === 'tool/result' && event.data.message?.toolCallId === 'call_delegate_1'), false)
  return pagesBySession
}

async function captureScreenshotResultInterrupted(image, tool = screenshotTool, output = 'Screenshot captured') {
  const images = Array.isArray(image) ? image : [image]
  const pages = []
  let modelCalls = 0
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') send(proc, { ...start, tools: [tool] })
    if (frame.type === 'checkpoint') {
      pages.push({ from_seq: frame.from_seq, through_seq: frame.through_seq, payload: frame.payload, sha256: frame.sha256 })
      send(proc, { type: 'checkpoint_ack', id: frame.id, ok: true, through_seq: frame.through_seq, sha256: frame.sha256 })
    }
    if (frame.type === 'model') {
      modelCalls += 1
      if (modelCalls > 1) {
        proc.kill('SIGKILL')
        return
      }
      const chunks = [
        { type: 'tool-call-delta', index: 0, id: 'call_screenshot_1', name: tool.name, argumentsDelta: '{}' },
        { type: 'usage', usage: { inputTokens: 3, outputTokens: 2 } },
        { type: 'finish', reason: { kind: 'tool-calls' } },
      ]
      chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
      send(proc, { type: 'model_end', id: frame.id, ok: true })
    }
    if (frame.type === 'tool_call') {
      assert.equal(frame.call_id, 'call_screenshot_1')
      send(proc, {
        type: 'tool_result', id: frame.id, task_id: frame.task_id, owner_id: frame.owner_id,
        agent_id: frame.agent_id, generation: frame.generation, authorization_version: frame.authorization_version,
        session_id: frame.session_id, call_id: frame.call_id, status: 'succeeded', output, images,
      })
    }
  })
  const [code, signal] = await once(child, 'close')
  assert.equal(signal, 'SIGKILL')
  assert.equal(code, null)
  assert.equal(modelCalls, 2)
  assert.ok(pages.length > 0)
  return { pages, frames }
}

test('reconciles an interrupted tool call before resuming the dsh model', async () => {
  const tool = {
    name: 'file_write', description: 'Write a file in the task workspace.',
    parameters: {
      type: 'object', properties: { workspace_id: { type: 'string' }, path: { type: 'string' }, content: { type: 'string' } },
      required: ['workspace_id', 'path', 'content'], additionalProperties: false,
    },
  }
  const pages = await captureInterruptedToolSession(tool)
  const resumed = await resumeSession(pages, { tools: [tool], onToolCall: frame => {
    assert.equal(frame.call_id, 'call_interrupted_1')
    assert.equal(frame.name, 'file_write')
    assert.deepEqual(frame.arguments, { workspace_id: 'w1234567', path: 'notes.txt', content: 'saved' })
    return { status: 'succeeded', output: '{"bytes_written":5}' }
  } })
  assert.equal(resumed.code, 0, `frames: ${resumed.frames.map(frame => frame.type).join(', ')}`)
  assert.equal(resumed.frames.filter(frame => frame.type === 'tool_call').length, 1)
  assert.equal(resumed.modelCalls, 1)
  assert.ok(resumed.frames.findIndex(frame => frame.type === 'tool_call') < resumed.frames.findIndex(frame => frame.type === 'model'))
  const recoveryResults = resumed.emitted.flatMap(frame => JSON.parse(frame.payload).events)
    .filter(event => event.type === 'tool/result' && event.data.message?.toolCallId === 'call_interrupted_1')
  assert.equal(recoveryResults.length, 1)
  assert.equal(recoveryResults[0].data.message.content[0].text, '{"bytes_written":5}')
  const callSeq = pages.flatMap(page => JSON.parse(page.payload).events).find(event => event.type === 'tool/call').seq
  assert.deepEqual(recoveryResults[0].sourceEventSeqs, [callSeq])
})

test('reconciles an interrupted MCP IPC call from the task ledger before the model resumes', async () => {
  const tool = {
    name: 'mcp__linear__create_issue', description: 'Create an issue through the task-granted connector.',
    parameters: {
      type: 'object', properties: { title: { type: 'string' } },
      required: ['title'], additionalProperties: false,
    },
  }
  let remoteDispatches = 0
  const operationLedger = new Map()
  const pages = await captureInterruptedToolSession(tool, '{"title":"Keep OAuth token server-side"}', frame => {
    assert.equal(frame.name, tool.name)
    remoteDispatches++
    operationLedger.set(frame.call_id, { status: 'succeeded', output: '{"issue_id":"MOR-17"}' })
  })
  const resumed = await resumeSession(pages, {
    tools: [tool],
    onToolCall: frame => {
      assert.equal(frame.call_id, 'call_interrupted_1')
      assert.equal(frame.name, tool.name)
      assert.deepEqual(frame.arguments, { title: 'Keep OAuth token server-side' })
      // Model the broker resolving the dispatched call from its durable ledger.
      return operationLedger.get(frame.call_id)
    },
  })
  assert.equal(resumed.code, 0, `frames: ${resumed.frames.map(frame => frame.type).join(', ')}`)
  assert.equal(resumed.frames.filter(frame => frame.type === 'tool_call').length, 1)
  assert.equal(resumed.modelCalls, 1)
  assert.equal(remoteDispatches, 1, 'recovery must not issue a second connector request')
  assert.ok(resumed.frames.findIndex(frame => frame.type === 'tool_call') < resumed.frames.findIndex(frame => frame.type === 'model'))
})

test('a revoked MCP grant blocks an interrupted call before IPC or model resume', async () => {
  const tool = {
    name: 'mcp__linear__create_issue', description: 'Create an issue through the task-granted connector.',
    parameters: {
      type: 'object', properties: { title: { type: 'string' } },
      required: ['title'], additionalProperties: false,
    },
  }
  const pages = await captureInterruptedToolSession(tool, '{"title":"Must not run after revocation"}')
  const resumed = await resumeSession(pages, { tools: [] })
  assert.notEqual(resumed.code, 0)
  assert.equal(resumed.frames.some(frame => frame.type === 'tool_call'), false)
  assert.equal(resumed.modelCalls, 0)
  assert.equal(resumed.emitted.length, 0)
})

test('rehydrates a screenshot reference before recovery and sends image bytes only in the model frame', async () => {
  const image = toolImage()
  const { pages } = await captureScreenshotResultInterrupted(image)
  const savedResult = pages.flatMap(page => JSON.parse(page.payload).events)
    .find(event => event.type === 'tool/result' && event.data.message?.toolCallId === 'call_screenshot_1')
  assert.equal(savedResult.data.message.content[0].text, 'Screenshot captured')
  assert.deepEqual(savedResult.data.message.content[1], {
    type: 'image',
    attachment: {
      attachmentId: image.attachment_id,
      mediaType: image.media_type,
      bytes: image.bytes,
      width: image.width,
      height: image.height,
    },
  })
  assert.equal(JSON.stringify(savedResult).includes('data_base64'), false, 'checkpoint stores no image bytes')

  const resumed = await resumeSession(pages, {
    tools: [screenshotTool],
    onToolCall: frame => {
      assert.equal(frame.call_id, 'call_screenshot_1')
      return { status: 'succeeded', output: 'Screenshot captured', images: [image] }
    },
  })
  assert.equal(resumed.code, 0, `frames: ${resumed.frames.map(frame => frame.type).join(', ')}`)
  assert.equal(resumed.modelCalls, 1)
  const resultMessage = resumed.modelRequests[0].messages.find(message => message.role === 'tool' && message.toolCallId === 'call_screenshot_1')
  assert.equal(resultMessage.content[0].text, 'Screenshot captured')
  assert.deepEqual(resultMessage.content[1], {
    type: 'image',
    attachment: {
      attachmentId: image.attachment_id,
      mediaType: image.media_type,
      bytes: image.bytes,
      width: image.width,
      height: image.height,
      data_base64: image.data_base64,
    },
  })
  const recoveryEvents = resumed.emitted.flatMap(frame => JSON.parse(frame.payload).events)
  assert.equal(JSON.stringify(recoveryEvents).includes('data_base64'), false)
})

test('rehydrates a PNG task attachment reference and sends its original format to the model', async () => {
  const image = toolImage(testPng(), 1, 1, 'image/png')
  const output = 'Image attachment read'
  const { pages } = await captureScreenshotResultInterrupted(image, taskAttachmentTool, output)
  const savedResult = pages.flatMap(page => JSON.parse(page.payload).events)
    .find(event => event.type === 'tool/result' && event.data.message?.toolCallId === 'call_screenshot_1')
  assert.equal(savedResult.data.message.content[0].text, output)
  assert.equal(savedResult.data.message.content[1].attachment.mediaType, 'image/png')
  assert.equal(JSON.stringify(savedResult).includes('data_base64'), false)

  const resumed = await resumeSession(pages, {
    tools: [taskAttachmentTool],
    onToolCall: frame => {
      assert.equal(frame.call_id, 'call_screenshot_1')
      return { status: 'succeeded', output, images: [image] }
    },
  })
  assert.equal(resumed.code, 0, `frames: ${resumed.frames.map(frame => frame.type).join(', ')}`)
  assert.equal(resumed.modelCalls, 1)
  const resultMessage = resumed.modelRequests[0].messages.find(message => message.role === 'tool' && message.toolCallId === 'call_screenshot_1')
  assert.equal(resultMessage.content[1].attachment.mediaType, 'image/png')
  assert.equal(resultMessage.content[1].attachment.data_base64, image.data_base64)
})

test('rehydrates four bounded PPTX image references through Task Reader checkpoint recovery', async () => {
  const image = toolImage(testPng(), 1, 1, 'image/png')
  const images = [image, image, image, image]
  const output = 'PPTX text and four embedded images'
  const { pages } = await captureScreenshotResultInterrupted(images, taskAttachmentTool, output)
  const savedResult = pages.flatMap(page => JSON.parse(page.payload).events)
    .find(event => event.type === 'tool/result' && event.data.message?.toolCallId === 'call_screenshot_1')
  assert.equal(savedResult.data.message.content[0].text, output)
  assert.equal(savedResult.data.message.content.length, 5)
  assert.deepEqual(savedResult.data.message.content.slice(1).map(block => block.attachment.attachmentId),
    images.map(item => item.attachment_id))
  assert.equal(JSON.stringify(savedResult).includes('data_base64'), false, 'checkpoint stores references, not image bytes')

  const resumed = await resumeSession(pages, {
    tools: [taskAttachmentTool],
    onToolCall: frame => {
      assert.equal(frame.call_id, 'call_screenshot_1')
      return { status: 'succeeded', output, images }
    },
  })
  assert.equal(resumed.code, 0, `frames: ${resumed.frames.map(frame => frame.type).join(', ')}`)
  assert.equal(resumed.modelCalls, 1)
  const resultMessage = resumed.modelRequests[0].messages.find(message => message.role === 'tool' && message.toolCallId === 'call_screenshot_1')
  assert.equal(resultMessage.content.length, 5)
  assert.deepEqual(resultMessage.content.slice(1).map(block => block.attachment.data_base64),
    images.map(item => item.data_base64))
  const recoveryEvents = resumed.emitted.flatMap(frame => JSON.parse(frame.payload).events)
  assert.equal(JSON.stringify(recoveryEvents).includes('data_base64'), false)
})

test('rejects more than four images from Task Reader and images from other tools', async () => {
  const image = toolImage(testPng(), 1, 1, 'image/png')
  const pages = await captureScreenshotResultInterrupted([image, image, image, image], taskAttachmentTool, 'four images')
  const tooMany = await resumeSession(pages.pages, {
    tools: [taskAttachmentTool],
    onToolCall: () => ({ status: 'succeeded', output: 'five images', images: [image, image, image, image, image] }),
  })
  assert.notEqual(tooMany.code, 0)
  assert.equal(tooMany.modelCalls, 0)

  const screenshotPages = await captureScreenshotResultInterrupted(image)
  const screenshotWithTwo = await resumeSession(screenshotPages.pages, {
    tools: [screenshotTool],
    onToolCall: () => ({ status: 'succeeded', output: 'invalid screenshot images', images: [image, image] }),
  })
  assert.notEqual(screenshotWithTwo.code, 0)
  assert.equal(screenshotWithTwo.modelCalls, 0)
})

test('rejects oversized and tampered tool screenshot images', async () => {
  const image = toolImage()
  const { pages } = await captureScreenshotResultInterrupted(image)
  const invalidImages = [
    { ...image, bytes: 512 * 1024 + 1 },
    { ...image, attachment_id: `morde_img_${'0'.repeat(64)}` },
    { ...image, data_base64: 'not-base64' },
    { ...image, width: 1281 },
  ]
  for (const invalid of invalidImages) {
    const result = await resumeSession(pages, {
      tools: [screenshotTool],
      onToolCall: () => ({ status: 'succeeded', output: 'Screenshot captured', images: [invalid] }),
    })
    assert.notEqual(result.code, 0)
    assert.equal(result.modelCalls, 0)
    assert.equal(result.frames.some(frame => frame.type === 'settled'), false)
  }
})

test('refuses a valid re-fetched screenshot when its reference differs from the checkpoint', async () => {
  const original = toolImage()
  const { pages } = await captureScreenshotResultInterrupted(original)
  const changed = toolImage(testJpeg(4, 2), 4, 2)
  const result = await resumeSession(pages, {
    tools: [screenshotTool],
    onToolCall: () => ({ status: 'succeeded', output: 'Screenshot captured', images: [changed] }),
  })
  assert.notEqual(result.code, 0)
  assert.equal(result.modelCalls, 0)
  assert.equal(result.emitted.length, 0)
  assert.equal(result.frames.some(frame => frame.type === 'settled'), false)
})

function resumedStart(pages, tools = []) {
  const { prompt, ...identity } = start
  assert.ok(prompt)
  const tail = pages.at(-1)
  return { ...identity, tools, agent_id: 'agent_124', generation: 2, resume: true,
    checkpoint_through_seq: tail.through_seq, checkpoint_sha256: tail.sha256 }
}

function checkpointPage(request, page) {
  return { type: 'checkpoint_page', id: request.id, ok: true,
    task_id: request.task_id, owner_id: request.owner_id, agent_id: request.agent_id,
    generation: request.generation, authorization_version: request.authorization_version, session_id: request.session_id,
    from_seq: page.from_seq, through_seq: page.through_seq, payload: page.payload, sha256: page.sha256 }
}

async function resumeSession(pages, { alterStart = value => value, alterPage = value => value, stopRead = false, refuseCheckpoint = false, withholdToolResult = false, onToolCall, onChildCreate, checkpointPagesBySession, tools = [], deliveries = [] } = {}) {
  const emitted = []
  const offsets = []
  const modelRequests = []
  let modelCalls = 0
  const { child, frames } = launch((frame, proc) => {
    if (frame.type === 'ready') {
      const startFrame = alterStart(resumedStart(pages, tools))
      send(proc, startFrame)
      for (const delivery of deliveries) send(proc, typeof delivery === 'function' ? delivery(startFrame) : delivery)
    }
    if (frame.type === 'checkpoint_read') {
      offsets.push(frame.from_seq)
      if (stopRead) {
        send(proc, { type: 'cancel' })
        return
      }
      const availablePages = checkpointPagesBySession?.get(frame.session_id) ?? pages
      const page = availablePages.find(item => item.from_seq === frame.from_seq)
      if (!page) {
        proc.stdin.end()
        return
      }
      send(proc, alterPage(checkpointPage(frame, page)))
    }
    if (frame.type === 'checkpoint') {
      emitted.push(frame)
      send(proc, { type: 'checkpoint_ack', id: frame.id, ok: !refuseCheckpoint, through_seq: frame.through_seq, sha256: frame.sha256 })
    }
    if (frame.type === 'tool_call') {
      if (withholdToolResult) {
        proc.stdin.end()
        return
      }
      const result = onToolCall?.(frame) ?? { status: 'succeeded', output: 'reconciled output' }
      send(proc, {
        type: 'tool_result', id: frame.id, task_id: frame.task_id, owner_id: frame.owner_id,
        agent_id: frame.agent_id, generation: frame.generation, authorization_version: frame.authorization_version,
        session_id: frame.session_id, call_id: frame.call_id, status: result.status, output: result.output,
        ...(result.images === undefined ? {} : { images: result.images }),
      })
    }
    if (frame.type === 'child_create') {
      const result = onChildCreate?.(frame)
      if (result) send(proc, result)
    }
    if (frame.type === 'child_settle') {
      send(proc, { type: 'child_settle_ack', ...taskFence(frame), call_id: frame.call_id, ok: true })
    }
    if (frame.type === 'model') {
      modelCalls += 1
      modelRequests.push(frame.request)
      const chunks = [
        { type: 'block-start', index: 0, blockType: 'text' },
        { type: 'text-delta', index: 0, text: 'resumed answer' },
        { type: 'block-end', index: 0, block: { type: 'text', text: 'resumed answer' } },
        { type: 'usage', usage: { inputTokens: 2, outputTokens: 1 } },
        { type: 'finish', reason: { kind: 'stop' } },
      ]
      chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', id: frame.id, index, chunk }))
      send(proc, { type: 'model_end', id: frame.id, ok: true })
    }
  })
  const [code] = await once(child, 'close')
  return { code, frames, emitted, offsets, modelCalls, modelRequests }
}

test('restores a completed dsh session from verified pages without replaying the prompt', async () => {
  const pages = await captureSession()
  const result = await resumeSession(pages)
  assert.equal(result.code, 0, `frames: ${result.frames.map(frame => frame.type).join(', ')}`)
  assert.deepEqual(result.offsets, pages.map(page => page.from_seq))
  assert.equal(result.frames.find(frame => frame.type === 'settled')?.state, 'completed_candidate')
  assert.equal(result.frames.find(frame => frame.type === 'settled')?.session_id, start.session_id)
  assert.equal(result.frames.some(frame => frame.type === 'model'), false)
})

test('restores a completed 0.2.0-rc.2 checkpoint on the upgraded DSH runtime', async () => {
  const fixture = JSON.parse(await readFile(new URL('./fixtures/dsh-session-0.2.0-rc.2-complete.json', import.meta.url), 'utf8'))
  assert.equal(fixture.sourceEngine, '0.2.0-rc.2')
  const result = await resumeSession(fixture.pages)
  assert.equal(result.code, 0, `frames: ${result.frames.map(frame => frame.type).join(', ')}`)
  assert.equal(result.frames.find(frame => frame.type === 'settled')?.state, 'completed_candidate')
  assert.deepEqual(result.offsets, fixture.pages.map(page => page.from_seq))
  assert.equal(result.modelCalls, 0)
})

test('reconciles an old pending MCP call from Morde ledger without redispatching it', async () => {
  const fixture = JSON.parse(await readFile(new URL('./fixtures/dsh-session-0.2.0-rc.2-pending-mcp.json', import.meta.url), 'utf8'))
  const ledgerLookups = []
  const result = await resumeSession(fixture.pages, {
    tools: [fixture.tool],
    onToolCall(frame) {
      ledgerLookups.push(frame.call_id)
      assert.equal(frame.name, fixture.tool.name)
      assert.deepEqual(frame.arguments, { message: 'write once' })
      return { status: 'succeeded', output: 'committed once' }
    },
  })
  assert.equal(result.code, 0, `frames: ${result.frames.map(frame => frame.type).join(', ')}`)
  assert.deepEqual(ledgerLookups, ['call_migration_1'])
  assert.equal(result.frames.filter(frame => frame.type === 'tool_call').length, 1)
  assert.equal(result.modelCalls, 1)
  const toolResult = result.modelRequests[0].messages.find(message => message.role === 'tool' && message.toolCallId === 'call_migration_1')
  assert.equal(toolResult.content[0].text, 'committed once')
})

test('does not resume an old pending MCP call when its side effect outcome is unknown', async () => {
  const fixture = JSON.parse(await readFile(new URL('./fixtures/dsh-session-0.2.0-rc.2-pending-mcp.json', import.meta.url), 'utf8'))
  const result = await resumeSession(fixture.pages, { tools: [fixture.tool], withholdToolResult: true })
  assert.notEqual(result.code, 0)
  assert.equal(result.frames.some(frame => frame.type === 'settled'), false)
  assert.equal(result.modelCalls, 0)
})

test('passes an approval denial from the Morde ledger to an old pending DSH tool call', async () => {
  const fixture = JSON.parse(await readFile(new URL('./fixtures/dsh-session-0.2.0-rc.2-pending-mcp.json', import.meta.url), 'utf8'))
  let approvalResults = 0
  const result = await resumeSession(fixture.pages, {
    tools: [fixture.tool],
    onToolCall() {
      approvalResults += 1
      return { status: 'failed', output: 'Approval was denied by the user.' }
    },
  })
  assert.equal(result.code, 0, `frames: ${result.frames.map(frame => frame.type).join(', ')}`)
  assert.equal(approvalResults, 1)
  assert.equal(result.modelCalls, 1)
  const toolResult = result.modelRequests[0].messages.find(message => message.role === 'tool' && message.toolCallId === 'call_migration_1')
  assert.match(toolResult.content[0].text, /Approval was denied/)
})

test('persists dsh interrupted-turn closers before reporting a restored session', async () => {
  const pages = await captureSession({ interrupt: true })
  const result = await resumeSession(pages)
  assert.equal(result.code, 0, `frames: ${result.frames.map(frame => frame.type).join(', ')}`)
  assert.equal(result.frames.find(frame => frame.type === 'settled')?.state, 'completed_candidate')
  assert.equal(result.modelCalls, 1)
  const events = result.emitted.flatMap(frame => JSON.parse(frame.payload).events)
  assert.ok(events.some(event => event.type === 'turn/end' && event.data.reason.kind === 'interrupted'))
  assert.equal(result.emitted[0].from_seq, pages.at(-1).through_seq + 1)
})

test('replaces the old DSH identity when an interrupted session resumes', async () => {
  const pages = await captureSession({ interrupt: true })
  let replaced = false
  const legacyPages = pages.map(page => {
    const checkpoint = JSON.parse(page.payload)
    for (const event of checkpoint.events) {
      if (event.type === 'system/message') {
        event.data.message.content = [{ type: 'text', text: 'You are an AI agent powered by DeepSeek Harness.' }]
        replaced = true
      }
    }
    const payload = JSON.stringify(checkpoint)
    return { ...page, payload, sha256: createHash('sha256').update(payload).digest('hex') }
  })
  assert.equal(replaced, true)
  const result = await resumeSession(legacyPages)
  assert.equal(result.code, 0, `frames: ${result.frames.map(frame => frame.type).join(', ')}`)
  assert.equal(result.modelCalls, 1)
  const system = result.modelRequests[0].messages.filter(message => message.role === 'system')
  assert.equal(system.length, 1)
  const text = system[0].content.map(block => block.text ?? '').join('')
  assert.match(text, /You are Morde, the user's AI assistant\./)
  assert.doesNotMatch(text, /You are an AI agent powered by DeepSeek Harness/)
})

test('refuses recovery when interrupted-turn closers cannot be committed', async () => {
  const pages = await captureSession({ interrupt: true })
  const result = await resumeSession(pages, { refuseCheckpoint: true })
  assert.notEqual(result.code, 0)
  assert.ok(result.emitted.length > 0)
  assert.equal(result.frames.some(frame => frame.type === 'settled'), false)
  assert.equal(result.frames.some(frame => frame.type === 'model'), false)
})

test('refuses damaged, missing, mismatched, or unauthorized checkpoint pages', async () => {
  const pages = await captureSession()
  const cases = [
    { name: 'wrong identity', alterPage: page => ({ ...page, owner_id: 'another_owner' }) },
    { name: 'wrong offset', alterPage: page => ({ ...page, from_seq: page.from_seq + 1 }) },
    { name: 'bad digest', alterPage: page => ({ ...page, sha256: '0'.repeat(64) }) },
    { name: 'bad pinned tail', alterStart: value => ({ ...value, checkpoint_sha256: '0'.repeat(64) }) },
    { name: 'oversized page', alterPage: page => ({ ...page, payload: 'x'.repeat(1024 * 1024 + 1) }) },
    { name: 'duplicate JSON key', alterPage: page => {
      const field = `"from_seq":${page.from_seq}`
      const payload = page.payload.replace(field, `${field},${field}`)
      return { ...page, payload, sha256: createHash('sha256').update(payload).digest('hex') }
    } },
    { name: 'noncontiguous events', alterPage: page => {
      const body = JSON.parse(page.payload)
      body.events[0].seq += 1
      const payload = JSON.stringify(body)
      return { ...page, payload, sha256: createHash('sha256').update(payload).digest('hex') }
    } },
    { name: 'unknown required event', alterPage: page => {
      const body = JSON.parse(page.payload)
      body.events[0].type = 'unknown/required'
      const payload = JSON.stringify(body)
      return { ...page, payload, sha256: createHash('sha256').update(payload).digest('hex') }
    } },
    { name: 'cancelled read', stopRead: true },
  ]
  for (const scenario of cases) {
    const result = await resumeSession(pages, scenario)
    assert.notEqual(result.code, 0, scenario.name)
    assert.equal(result.frames.some(frame => frame.type === 'model'), false, scenario.name)
    assert.equal(result.emitted.length, 0, scenario.name)
  }
})

test('refuses a checkpoint history with a missing final page', async () => {
  const pages = await captureSession()
  assert.ok(pages.length > 1)
  const tail = pages.at(-1)
  const result = await resumeSession(pages.slice(0, -1), { alterStart: value => ({ ...value,
    checkpoint_through_seq: tail.through_seq, checkpoint_sha256: tail.sha256 }) })
  assert.notEqual(result.code, 0)
  assert.equal(result.emitted.length, 0)
  assert.equal(result.frames.some(frame => frame.type === 'model'), false)
})

test('refuses checkpoint headers that change between pages', async () => {
  const captured = await captureSession()
  const original = JSON.parse(captured[0].payload).header
  const pages = [original, { ...original, createdAt: original.createdAt + 1 }].map((header, seq) => {
    const payload = JSON.stringify({ header, from_seq: seq, events: [{ type: 'extension/test', seq, time: 1, data: {}, ignorable: true }] })
    return { from_seq: seq, through_seq: seq, payload, sha256: createHash('sha256').update(payload).digest('hex') }
  })
  const result = await resumeSession(pages)
  assert.notEqual(result.code, 0)
  assert.equal(result.emitted.length, 0)
  assert.equal(result.frames.some(frame => frame.type === 'model'), false)
})

test('refuses history larger than the bounded dsh restore budget', async () => {
  const captured = await captureSession()
  const header = JSON.parse(captured[0].payload).header
  const pages = Array.from({ length: 10 }, (_, seq) => {
    const payload = JSON.stringify({ header, from_seq: seq,
      events: [{ type: 'extension/test', seq, time: 1, data: { padding: 'x'.repeat(900_000) }, ignorable: true }] })
    return { from_seq: seq, through_seq: seq, payload, sha256: createHash('sha256').update(payload).digest('hex') }
  })
  const result = await resumeSession(pages)
  assert.notEqual(result.code, 0)
  assert.equal(result.emitted.length, 0)
  assert.equal(result.frames.some(frame => frame.type === 'model'), false)
  assert.ok(result.offsets.length >= 9)
})

test('refuses historical tool calls before any recovery rewrite', async () => {
  const captured = await captureSession({ interrupt: true })
  const pages = captured.map(page => ({ ...page }))
  const last = pages.at(-1)
  const body = JSON.parse(last.payload)
  const event = body.events.at(-1)
  event.type = 'tool/call'
  event.data = { turn: 1, step: 1, callId: 'call_1', name: 'shell', arguments: '{}' }
  last.payload = JSON.stringify(body)
  last.sha256 = createHash('sha256').update(last.payload).digest('hex')
  const result = await resumeSession(pages)
  assert.notEqual(result.code, 0)
  assert.equal(result.emitted.length, 0)
  assert.equal(result.frames.some(frame => frame.type === 'model'), false)
})

test('refuses a validly hashed checkpoint for a different session', async () => {
  const captured = await captureSession()
  const pages = captured.map(page => ({ ...page }))
  const body = JSON.parse(pages[0].payload)
  body.header.id = 'other_session'
  pages[0].payload = JSON.stringify(body)
  pages[0].sha256 = createHash('sha256').update(pages[0].payload).digest('hex')
  const result = await resumeSession(pages)
  assert.notEqual(result.code, 0)
  assert.equal(result.emitted.length, 0)
  assert.equal(result.frames.some(frame => frame.type === 'model'), false)
})

test('replays a persisted inbox message once during recovery', async () => {
  const captured = await captureSession({ interrupt: true })
  const decoded = captured.flatMap(page => JSON.parse(page.payload).events)
  const pending = decoded.findIndex(event => event.type === 'agent/inbox/spliced' && event.data.inserted.length > 0)
  assert.ok(pending >= 0)
  const body = { header: JSON.parse(captured[0].payload).header, from_seq: 0, events: decoded.slice(0, pending + 1) }
  const pendingMessage = body.events[pending].data.inserted[0]
  pendingMessage.id = 'm1234567'
  pendingMessage.content = [{ type: 'text', text: 'Please resume this pending message.' }]
  const payload = JSON.stringify(body)
  const page = { from_seq: 0, through_seq: body.events.at(-1).seq, payload, sha256: createHash('sha256').update(payload).digest('hex') }
  const result = await resumeSession([page], {
    deliveries: [resumeStart => messageDelivery(resumeStart, pendingMessage.id, pendingMessage.content[0].text)],
  })
  assert.equal(result.code, 0, `frames: ${result.frames.map(frame => frame.type).join(', ')}`)
  assert.ok(result.modelCalls > 0)
  const recoveredEvents = result.emitted.flatMap(frame => JSON.parse(frame.payload).events)
  assert.equal(recoveredEvents.filter(event => event.type === 'user/message' && event.data.id === pendingMessage.id).length, 1)
  assert.equal(result.modelRequests[0].messages.filter(message => message.id === pendingMessage.id).length, 1)

  const conflict = await resumeSession([page], {
    deliveries: [resumeStart => messageDelivery(resumeStart, pendingMessage.id, 'conflicting content')],
  })
  assert.notEqual(conflict.code, 0)
  assert.equal(conflict.modelCalls, 0)
  assert.equal(conflict.frames.some(frame => frame.type === 'settled'), false)
})

test('restages a partially persisted revision batch before one model turn', async () => {
  const captured = await captureSession({ interrupt: true })
  const decoded = captured.flatMap(page => JSON.parse(page.payload).events)
  const pending = decoded.findIndex(event => event.type === 'agent/inbox/spliced' && event.data.inserted.length > 0)
  assert.ok(pending >= 0)
  const body = { header: JSON.parse(captured[0].payload).header, from_seq: 0, events: decoded.slice(0, pending + 1) }
  const persisted = body.events[pending].data.inserted[0]
  persisted.id = 'm1234567'
  persisted.content = [{ type: 'text', text: 'Use revenue only.' }]
  const payload = JSON.stringify(body)
  const page = { from_seq: 0, through_seq: body.events.at(-1).seq, payload, sha256: createHash('sha256').update(payload).digest('hex') }
  const revisionMessages = [
    { message_id: 'm1234567', content: 'Use revenue only.', attachment_ids: [] },
    { message_id: 'm2345678', content: 'Use Chinese headings.', attachment_ids: [] },
  ]
  const result = await resumeSession([page], { alterStart: value => ({ ...value, revision_messages: revisionMessages }) })
  assert.equal(result.code, 0, `frames: ${result.frames.map(frame => frame.type).join(', ')}`)
  assert.equal(result.modelCalls, 1)
  const messages = result.modelRequests[0].messages.filter(message => revisionMessages.some(item => item.message_id === message.id))
  assert.deepEqual(messages.map(message => message.id), revisionMessages.map(item => item.message_id))
  const events = result.emitted.flatMap(frame => JSON.parse(frame.payload).events)
  for (const message of revisionMessages) {
    assert.equal(events.filter(event => event.type === 'user/message' && event.data.id === message.message_id).length, 1)
  }
})

test('account runtime routes concurrent Tasks independently and cancels only the selected Task', async t => {
  const models = new Map()
  const settled = new Map()
  let cancelledA = false
  const { child, frames } = launchAccount((frame, proc) => {
    if (frame.type === 'runtime_ready') {
      assert.equal(frame.server_instance_id, account.server_instance_id)
      assert.equal(frame.owner_id, account.owner_id)
      assert.equal(frame.runtime_id, account.runtime_id)
      assert.equal(frame.runtime_generation, account.runtime_generation)
      assert.equal(frame.max_concurrent_tasks, account.max_concurrent_tasks)
      assert.equal(frame.engine_version, '0.2.0-rc.2')
      send(proc, accountTask('task_A', 'answer A'))
      send(proc, accountTask('task_B', 'answer B', 2))
    }
    if (frame.type === 'checkpoint') acknowledgeCheckpoint(proc, frame)
    if (frame.type === 'model') {
      models.set(frame.task_id, frame)
      if (models.size === 2 && !cancelledA) {
        cancelledA = true
        send(proc, { type: 'cancel', ...taskFence(models.get('task_A')) })
        replyWithText(proc, models.get('task_B'), 'result B')
      }
    }
    if (frame.type === 'settled') {
      settled.set(frame.task_id, frame)
      if (settled.size === 2) send(proc, { ...account, type: 'runtime_shutdown' })
    }
    if (frame.type === 'runtime_stopped') proc.stdin.end()
  })
  t.after(() => child.kill())
  const closed = once(child, 'close')
  send(child, account)
  const [code] = await closed
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(models.size, 2, 'both Task model requests must be active before either response')
  assert.equal(settled.get('task_A')?.state, 'cancelled')
  assert.equal(settled.get('task_B')?.state, 'completed_candidate')
  assert.ok(frames.some(frame => frame.type === 'model_cancel' && frame.task_id === 'task_A'))
  assert.equal(frames.some(frame => frame.type === 'model_cancel' && frame.task_id === 'task_B'), false)
  for (const frame of frames.filter(item => item.type === 'checkpoint')) {
    assert.equal(JSON.parse(frame.payload).header.id, frame.session_id)
  }
})

test('account runtime waits for external Task process admission before dispatching its start frame', async t => {
  const input = new PassThrough()
  const output = new PassThrough()
  const runtime = new AccountRuntime(input, output)
  const frames = []
  const waiters = new Set()
  const lines = createInterface({ input: output })
  lines.on('line', line => {
    const frame = JSON.parse(line)
    frames.push(frame)
    for (const waiter of waiters) {
      if (!waiter.predicate(frame)) continue
      waiters.delete(waiter)
      clearTimeout(waiter.timer)
      waiter.resolve(frame)
    }
  })
  const waitFor = predicate => {
    const found = frames.find(predicate)
    if (found) return Promise.resolve(found)
    return new Promise((resolve, reject) => {
      const waiter = { predicate, resolve, reject, timer: undefined }
      waiter.timer = setTimeout(() => {
        waiters.delete(waiter)
        reject(new Error('account runtime frame timed out'))
      }, 5_000)
      waiters.add(waiter)
    })
  }
  const run = runtime.run()
  t.after(async () => {
    for (const task of [...runtime.tasks.values()]) await runtime.terminateTask(task, 'test_cleanup').catch(() => {})
    runtime.shutdown('test_cleanup')
    input.end()
    let timer
    await Promise.race([run, new Promise(resolve => { timer = setTimeout(resolve, 3_000) })])
    clearTimeout(timer)
    lines.close()
    output.destroy()
  })

  send({ stdin: input }, account)
  await waitFor(frame => frame.type === 'runtime_ready')
  const startFrame = accountTask('task_admission', 'must wait for Guardian', 1)
  send({ stdin: input }, startFrame)
  const processReady = await waitFor(frame => frame.type === 'task_process_ready' && frame.task_id === startFrame.task_id)
  await new Promise(resolve => setTimeout(resolve, 40))
  assert.equal(frames.some(frame => frame.type === 'task_started' && frame.task_id === startFrame.task_id), false)
  assert.equal(frames.some(frame => frame.type === 'model' && frame.task_id === startFrame.task_id), false)
  send({ stdin: input }, taskProcessAdmission(processReady))
  await waitFor(frame => frame.type === 'task_started' && frame.task_id === startFrame.task_id)
})

test('account runtime preserves child session identity on task output and rejects a changed task fence', async () => {
  const runtime = new AccountRuntime(new PassThrough(), new PassThrough())
  runtime.runtime = account
  const sent = []
  runtime.send = async frame => sent.push(frame)
  const rootFence = taskFence(accountTask('task_output'))
  const outputTask = () => ({
    fence: rootFence, outputBuffer: Buffer.alloc(0), outputFrames: Promise.resolve(), readyReceived: true,
    started: { promise: Promise.resolve() }, startDispatchFailed: false, terminalFrame: undefined,
    terminal: false, terminated: false, exit: { code: 0 },
  })
  const childFrame = {
    type: 'checkpoint', ...rootFence, session_id: 'child_123', id: 'checkpoint_1',
    from_seq: 0, through_seq: 1, payload: '{}', sha256: 'digest',
  }
  const childTask = outputTask()
  runtime.receiveTaskOutput(childTask, `${JSON.stringify(childFrame)}\n`)
  await childTask.outputFrames
  assert.equal(sent[0].session_id, 'child_123')
  assert.equal(sent[0].task_id, rootFence.task_id)
  assert.equal(sent[0].owner_id, rootFence.owner_id)
  assert.equal(sent[0].runtime_generation, account.runtime_generation)

  const invalidTask = outputTask()
  runtime.receiveTaskOutput(invalidTask, `${JSON.stringify({ ...childFrame, owner_id: 'other_user' })}\n`)
  await invalidTask.outputFrames
  assert.equal(sent.length, 1)
  assert.equal(invalidTask.terminateReason, 'output_failed')
})

test('account runtime rejects a cancel frame with a different session fence', async () => {
  const runtime = new AccountRuntime(new PassThrough(), new PassThrough())
  runtime.runtime = account
  const received = []
  const writes = []
  runtime.send = async frame => received.push(frame)
  const fence = taskFence(accountTask('task_cancel_scope'))
  const task = {
    fence,
    child: { stdin: { destroyed: false, write(bytes, callback) { writes.push(bytes); callback() } } },
  }
  runtime.tasks.set('task_cancel_scope', task)

  await runtime.routeTaskFrame({ type: 'cancel', ...fence, session_id: 'session_other' })

  assert.equal(received[0]?.code, 'task_fence_mismatch')
  assert.equal(writes.length, 0)

  await runtime.routeTaskFrame({ type: 'tool_result', ...fence, session_id: 'session_child' })
  assert.equal(writes.length, 1, 'valid child-session IPC must remain routable')
})

test('account runtime forwards fenced running message delivery to the task agent', async t => {
  const task = accountTask('task_account_message', 'Start the account-scoped task.', 1)
  const deliveryID = 'm7654321'
  let modelCalls = 0
  const errors = []
  const { child, frames } = launchAccount((frame, proc) => {
    if (frame.type === 'runtime_ready') send(proc, task)
    if (frame.type === 'checkpoint') acknowledgeCheckpoint(proc, frame)
    if (frame.type === 'model') {
      modelCalls++
      if (modelCalls === 1) {
        send(proc, {
          ...messageDelivery(taskFence(frame), deliveryID, 'The account review changed this task.'),
          server_instance_id: account.server_instance_id, runtime_id: account.runtime_id,
          runtime_generation: account.runtime_generation,
        })
        replyWithText(proc, frame, 'First response.')
      } else {
        assert.equal(frame.request.messages.filter(message => message.id === deliveryID).length, 1)
        assert.ok(frame.request.messages.some(message => message.id === deliveryID && message.content[0].text === 'The account review changed this task.'))
        replyWithText(proc, frame, 'Updated response.')
      }
    }
    if (frame.type === 'protocol_error') errors.push(frame.code)
    if (frame.type === 'settled') send(proc, { ...account, type: 'runtime_shutdown' })
    if (frame.type === 'runtime_stopped') proc.stdin.end()
  })
  t.after(() => child.kill())
  const closed = once(child, 'close')
  send(child, account)
  const [code] = await closed
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(modelCalls, 2)
  assert.deepEqual(errors, [])
  assert.equal(frames.find(frame => frame.type === 'settled')?.state, 'completed_candidate')
})

test('account runtime strips account fences before forwarding a tool result to dsh', async t => {
  let modelCalls = 0
  const errors = []
  const { child, frames } = launchAccount((frame, proc) => {
    if (frame.type === 'runtime_ready') send(proc, { ...accountTask('task_account_tool', 'Read notes.txt.', 1), tools: [fileReadTool] })
    if (frame.type === 'checkpoint') acknowledgeCheckpoint(proc, frame)
    if (frame.type === 'model') {
      if (modelCalls++ === 0) {
        const chunks = [
          { type: 'tool-call-delta', index: 0, id: 'call_account_read', name: fileReadTool.name, argumentsDelta: '{"path":"notes.txt"}' },
          { type: 'usage', usage: { inputTokens: 2, outputTokens: 1 } },
          { type: 'finish', reason: { kind: 'tool-calls' } },
        ]
        chunks.forEach((chunk, index) => send(proc, { type: 'model_chunk', ...taskFence(frame), id: frame.id, index, chunk }))
        send(proc, { type: 'model_end', ...taskFence(frame), id: frame.id, ok: true })
      } else {
        replyWithText(proc, frame, 'Notes say hello.')
      }
    }
    if (frame.type === 'tool_call') send(proc, {
      type: 'tool_result', ...taskFence(frame), server_instance_id: account.server_instance_id,
      runtime_id: account.runtime_id, runtime_generation: account.runtime_generation,
      id: frame.id, call_id: frame.call_id, status: 'succeeded', output: 'Notes say hello.', images: [],
    })
    if (frame.type === 'protocol_error') errors.push(frame.code)
    if (frame.type === 'settled') send(proc, { ...account, type: 'runtime_shutdown' })
    if (frame.type === 'runtime_stopped') proc.stdin.end()
  })
  t.after(() => child.kill())
  const closed = once(child, 'close')
  send(child, account)
  const [code] = await closed
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(frames.filter(frame => frame.type === 'tool_call').length, 1)
  assert.equal(frames.find(frame => frame.type === 'settled')?.state, 'completed_candidate')
  assert.deepEqual(errors, [])
})

test('account runtime strips account fences before forwarding cancel to dsh', async t => {
  const errors = []
  const { child, frames } = launchAccount((frame, proc) => {
    if (frame.type === 'runtime_ready') send(proc, accountTask('task_account_cancel'))
    if (frame.type === 'checkpoint') acknowledgeCheckpoint(proc, frame)
    if (frame.type === 'model') send(proc, {
      type: 'cancel', ...taskFence(frame), server_instance_id: account.server_instance_id,
      runtime_id: account.runtime_id, runtime_generation: account.runtime_generation,
    })
    if (frame.type === 'protocol_error') errors.push(frame.code)
    if (frame.type === 'settled') send(proc, { ...account, type: 'runtime_shutdown' })
    if (frame.type === 'runtime_stopped') proc.stdin.end()
  })
  t.after(() => child.kill())
  const closed = once(child, 'close')
  send(child, account)
  const [code] = await closed
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(frames.find(frame => frame.type === 'settled')?.state, 'cancelled')
  assert.deepEqual(errors, [])
})

test('account runtime can stop one Task process while a sibling process continues', async t => {
  const input = new PassThrough()
  const output = new PassThrough()
  const runtime = new AccountRuntime(input, output)
  const frames = []
  const waiters = new Set()
  const lines = createInterface({ input: output })
  lines.on('line', line => {
    const frame = JSON.parse(line)
    frames.push(frame)
    if (frame.type === 'checkpoint') {
      send({ stdin: input }, {
        type: 'checkpoint_ack', ...taskFence(frame), id: frame.id, ok: true,
        through_seq: frame.through_seq, sha256: frame.sha256,
      })
    }
    if (frame.type === 'task_process_ready') send({ stdin: input }, taskProcessAdmission(frame))
    for (const waiter of waiters) {
      if (!waiter.predicate(frame)) continue
      waiters.delete(waiter)
      clearTimeout(waiter.timer)
      waiter.resolve(frame)
    }
  })
  const waitFor = predicate => {
    const found = frames.find(predicate)
    if (found) return Promise.resolve(found)
    return new Promise((resolve, reject) => {
      const waiter = { predicate, resolve, reject, timer: undefined }
      waiter.timer = setTimeout(() => {
        waiters.delete(waiter)
        reject(new Error('account runtime frame timed out'))
      }, 5_000)
      waiters.add(waiter)
    })
  }
  const run = runtime.run()
  t.after(async () => {
    for (const task of [...runtime.tasks.values()]) await runtime.terminateTask(task, 'test_cleanup').catch(() => {})
    runtime.shutdown('test_cleanup')
    input.end()
    let timer
    await Promise.race([run, new Promise(resolve => { timer = setTimeout(resolve, 3_000) })])
    clearTimeout(timer)
    lines.close()
    output.destroy()
  })

  send({ stdin: input }, account)
  await waitFor(frame => frame.type === 'runtime_ready')
  const frameA = accountTask('task_process_A', 'wait for A', 1)
  const frameB = accountTask('task_process_B', 'wait for B', 2)
  send({ stdin: input }, frameA)
  await waitFor(frame => frame.type === 'task_started' && frame.task_id === frameA.task_id)
  send({ stdin: input }, frameB)
  await waitFor(frame => frame.type === 'task_started' && frame.task_id === frameB.task_id)
  const modelA = await waitFor(frame => frame.type === 'model' && frame.task_id === frameA.task_id)
  const modelB = await waitFor(frame => frame.type === 'model' && frame.task_id === frameB.task_id)
  assert.ok(modelA)
  const taskA = [...runtime.tasks.values()].find(task => task.fence.task_id === frameA.task_id)
  const taskB = [...runtime.tasks.values()].find(task => task.fence.task_id === frameB.task_id)
  assert.ok(taskA?.child?.pid)
  assert.ok(taskB?.child?.pid)
  assert.notEqual(taskA.child.pid, taskB.child.pid)

  await runtime.terminateTask(taskA, 'test_single_task_stop')
  const settledA = await waitFor(frame => frame.type === 'settled' && frame.task_id === frameA.task_id)
  assert.equal(settledA.state, 'cancelled')
  assert.ok(taskA.exit)
  assert.equal(taskB.exit, undefined)
  replyWithText({ stdin: input }, modelB, 'Task B remained active')
  const settledB = await waitFor(frame => frame.type === 'settled' && frame.task_id === frameB.task_id)
  assert.equal(settledB.state, 'completed_candidate')
  send({ stdin: input }, { ...account, type: 'runtime_shutdown' })
  await waitFor(frame => frame.type === 'runtime_stopped')
  input.end()
  await run
  assert.equal(frames.some(frame => frame.type === 'task_terminated' && frame.task_id === frameB.task_id), false)
})

test('account runtime rejects an invalid first runtime identity before accepting Tasks', async t => {
  const { child, frames } = launchAccount(() => {})
  t.after(() => child.kill())
  const closed = once(child, 'close')
  send(child, { ...account, runtime_id: '' })
  const [code] = await closed
  assert.equal(code, 1)
  assert.equal(frames.some(frame => frame.type === 'runtime_ready'), false)
  assert.equal(frames.some(frame => frame.type === 'task_started'), false)
})

test('account runtime accepts a new Task after settlement and rejects duplicate identity', async t => {
  const accountOne = { ...account, max_concurrent_tasks: 1 }
  const settled = new Set()
  const errors = []
  const { child, frames } = launchAccount((frame, proc) => {
    if (frame.type === 'runtime_ready') send(proc, accountTask('task_A'))
    if (frame.type === 'checkpoint') acknowledgeCheckpoint(proc, frame)
    if (frame.type === 'model') replyWithText(proc, frame, `done ${frame.task_id}`)
    if (frame.type === 'protocol_error') errors.push(frame.code)
    if (frame.type === 'settled' && frame.task_id === 'task_A') {
      settled.add('task_A')
      send(proc, accountTask('task_A'))
      send(proc, accountTask('task_B', 'prompt task_B', 2))
    }
    if (frame.type === 'settled' && frame.task_id === 'task_B') {
      settled.add('task_B')
      send(proc, { ...accountOne, type: 'runtime_shutdown' })
    }
    if (frame.type === 'runtime_stopped') proc.stdin.end()
  })
  t.after(() => child.kill())
  const closed = once(child, 'close')
  send(child, accountOne)
  const [code] = await closed
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.deepEqual(settled, new Set(['task_A', 'task_B']))
  assert.ok(errors.includes('task_start_already_settled'))
  assert.equal(frames.filter(frame => frame.type === 'task_started' && frame.task_id === 'task_B').length, 1)
})

test('account runtime acknowledges an identical active start retry without launching it twice', async t => {
  const original = accountTask('task_A')
  let retried = false
  const errors = []
  const { child, frames } = launchAccount((frame, proc) => {
    if (frame.type === 'runtime_ready') send(proc, original)
    if (frame.type === 'task_started' && !retried) {
      retried = true
      send(proc, Object.fromEntries(Object.entries(original).reverse()))
    }
    if (frame.type === 'checkpoint') acknowledgeCheckpoint(proc, frame)
    if (frame.type === 'model') replyWithText(proc, frame, 'one execution')
    if (frame.type === 'protocol_error') errors.push(frame.code)
    if (frame.type === 'settled') send(proc, { ...account, type: 'runtime_shutdown' })
    if (frame.type === 'runtime_stopped') proc.stdin.end()
  })
  t.after(() => child.kill())
  const closed = once(child, 'close')
  send(child, account)
  const [code] = await closed
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(frames.filter(frame => frame.type === 'task_started' && frame.task_id === 'task_A').length, 2)
  assert.equal(frames.filter(frame => frame.type === 'model' && frame.task_id === 'task_A').length, 1)
  assert.equal(frames.filter(frame => frame.type === 'settled' && frame.task_id === 'task_A').length, 1)
  assert.deepEqual(errors, [])
})

test('account runtime rejects a same-sequence start with different content', async t => {
  let sentConflict = false
  const errors = []
  const { child, frames } = launchAccount((frame, proc) => {
    if (frame.type === 'runtime_ready') send(proc, accountTask('task_A'))
    if (frame.type === 'task_started' && !sentConflict) {
      sentConflict = true
      send(proc, accountTask('task_A', 'different prompt', 1))
    }
    if (frame.type === 'checkpoint') acknowledgeCheckpoint(proc, frame)
    if (frame.type === 'model') replyWithText(proc, frame, 'one execution')
    if (frame.type === 'protocol_error') errors.push(frame.code)
    if (frame.type === 'settled') send(proc, { ...account, type: 'runtime_shutdown' })
    if (frame.type === 'runtime_stopped') proc.stdin.end()
  })
  t.after(() => child.kill())
  const closed = once(child, 'close')
  send(child, account)
  const [code] = await closed
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.deepEqual(errors, ['runtime_sequence_conflict'])
  assert.equal(frames.filter(frame => frame.type === 'task_started' && frame.task_id === 'task_A').length, 1)
  assert.equal(frames.filter(frame => frame.type === 'settled' && frame.task_id === 'task_A').length, 1)
})

test('account runtime accepts an empty background-job restore without delegation tools', async t => {
  const errors = []
  const jobIDParameters = {
    type: 'object', properties: { job_id: { type: 'string', minLength: 1, maxLength: 128 } },
    required: ['job_id'], additionalProperties: false,
  }
  const jobTools = [
    { name: 'dsh_job_list', description: 'List delegated jobs.', parameters: { type: 'object', properties: {}, required: [], additionalProperties: false } },
    { name: 'dsh_job_get', description: 'Read a delegated job.', parameters: jobIDParameters },
    { name: 'dsh_job_output', description: 'Read delegated job output.', parameters: {
      type: 'object', properties: { job_id: jobIDParameters.properties.job_id, after_seq: { type: 'integer', minimum: -1 } },
      required: ['job_id', 'after_seq'], additionalProperties: false,
    } },
    { name: 'dsh_job_cancel', description: 'Cancel a delegated job.', parameters: jobIDParameters },
  ]
  assert.equal(validStart({ ...start, tools: jobTools, background_jobs: [] }), true)
  assert.equal(validStart({ ...start, tools: jobTools.slice(0, 1), background_jobs: [] }), false)
  assert.equal(validStart({ ...start, tools: [], background_jobs: [{}] }), false)
  const { child, frames } = launchAccount((frame, proc) => {
    if (frame.type === 'runtime_ready') send(proc, { ...accountTask('task_A'), tools: jobTools, background_jobs: [] })
    if (frame.type === 'checkpoint') acknowledgeCheckpoint(proc, frame)
    if (frame.type === 'model') replyWithText(proc, frame, 'scheduled work completed')
    if (frame.type === 'protocol_error') errors.push(frame.code)
    if (frame.type === 'settled') send(proc, { ...account, type: 'runtime_shutdown' })
    if (frame.type === 'runtime_stopped') proc.stdin.end()
  })
  t.after(() => child.kill())
  const closed = once(child, 'close')
  send(child, account)
  const [code] = await closed
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.deepEqual(errors, [])
  assert.equal(frames.filter(frame => frame.type === 'task_started' && frame.task_id === 'task_A').length, 1)
  assert.equal(frames.find(frame => frame.type === 'settled' && frame.task_id === 'task_A')?.state, 'completed_candidate')
})

test('account runtime keeps only the latest retry window and rejects older sequences', async t => {
  const taskCount = 32
  let next = 1
  const errors = []
  const { child, frames } = launchAccount((frame, proc) => {
    if (frame.type === 'runtime_ready') send(proc, accountTask('task_1', 'prompt 1', 1))
    if (frame.type === 'checkpoint') acknowledgeCheckpoint(proc, frame)
    if (frame.type === 'model') replyWithText(proc, frame, `done ${frame.task_id}`)
    if (frame.type === 'settled') {
      if (next < taskCount) {
        next += 1
        send(proc, accountTask(`task_${next}`, `prompt ${next}`, next))
      } else {
        send(proc, accountTask('task_old', 'old start', 1))
      }
    }
    if (frame.type === 'protocol_error') {
      errors.push(frame.code)
      if (frame.code === 'stale_runtime_sequence') send(proc, { ...account, type: 'runtime_shutdown' })
    }
    if (frame.type === 'runtime_stopped') proc.stdin.end()
  })
  t.after(() => child.kill())
  const closed = once(child, 'close')
  send(child, account)
  const [code] = await closed
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(frames.filter(frame => frame.type === 'task_started').length, taskCount)
  assert.equal(frames.filter(frame => frame.type === 'settled').length, taskCount)
  assert.deepEqual(errors, ['stale_runtime_sequence'])
})

test('account runtime rejects wrong owner, missing or unknown fences, and over-quota starts without failing active work', async t => {
  const accountOne = { ...account, max_concurrent_tasks: 1 }
  const errors = []
  let sentInvalidFrames = false
  const settled = new Map()
  const { child, frames } = launchAccount((frame, proc) => {
    if (frame.type === 'runtime_ready') send(proc, accountTask('task_A'))
    if (frame.type === 'task_started' && frame.task_id === 'task_A' && !sentInvalidFrames) {
      sentInvalidFrames = true
      send(proc, accountTask('task_B', 'prompt task_B', 2))
      send(proc, accountTask('task_A', 'prompt task_A', 2))
      send(proc, { ...accountTask('task_C', 'prompt task_C', 3), owner_id: 'another_user' })
      send(proc, { ...accountTask('task_D', 'prompt task_D', 4), runtime_generation: account.runtime_generation + 1 })
      send(proc, { type: 'model_end', task_id: 'task_A', id: 'missing-fence', ok: true })
      send(proc, { ...taskFence(accountTask('task_unknown')), type: 'model_end', id: 'unknown', ok: true })
      send(proc, {
        ...taskFence(accountTask('task_A')), server_instance_id: account.server_instance_id,
        runtime_id: account.runtime_id, runtime_generation: account.runtime_generation + 1,
        type: 'model_end', id: 'wrong-runtime', ok: true,
      })
    }
    if (frame.type === 'checkpoint') acknowledgeCheckpoint(proc, frame)
    if (frame.type === 'model') replyWithText(proc, frame, `still works ${frame.task_id}`)
    if (frame.type === 'protocol_error') errors.push(frame.code)
    if (frame.type === 'settled' && frame.task_id === 'task_A') {
      settled.set('task_A', frame)
      send(proc, accountTask('task_E', 'prompt task_E', 2))
    }
    if (frame.type === 'settled' && frame.task_id === 'task_E') {
      settled.set('task_E', frame)
      send(proc, { ...accountOne, type: 'runtime_shutdown' })
    }
    if (frame.type === 'runtime_stopped') proc.stdin.end()
  })
  t.after(() => child.kill())
  const closed = once(child, 'close')
  send(child, accountOne)
  const [code] = await closed
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(settled.get('task_A')?.state, 'completed_candidate')
  assert.equal(settled.get('task_E')?.state, 'completed_candidate')
  assert.ok(errors.includes('task_capacity_exceeded'))
  assert.ok(errors.includes('duplicate_task'))
  assert.ok(errors.includes('owner_mismatch'))
  assert.ok(errors.includes('invalid_task_start'))
  assert.ok(errors.includes('task_fence_required'))
  assert.ok(errors.includes('unknown_task'))
  assert.ok(errors.includes('runtime_fence_mismatch'))
  assert.equal(frames.filter(frame => frame.type === 'task_started' && frame.task_id === 'task_A').length, 1)
})

test('account runtime shutdown explicitly terminates unfinished Tasks', async t => {
  const settled = []
  let shutdownSent = false
  const { child, frames } = launchAccount((frame, proc) => {
    if (frame.type === 'runtime_ready') send(proc, accountTask('task_A'))
    if (frame.type === 'checkpoint') acknowledgeCheckpoint(proc, frame)
    if (frame.type === 'model' && !shutdownSent) {
      shutdownSent = true
      send(proc, { ...account, type: 'runtime_shutdown' })
    }
    if (frame.type === 'settled' || frame.type === 'task_terminated') settled.push(frame)
    if (frame.type === 'runtime_stopped') proc.stdin.end()
  })
  t.after(() => child.kill())
  const closed = once(child, 'close')
  send(child, account)
  const [code] = await closed
  assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
  assert.equal(settled.length, 1)
  assert.equal(settled[0].task_id, 'task_A')
  assert.ok(settled[0].type === 'task_terminated' || settled[0].state === 'cancelled')
  assert.ok(frames.some(frame => frame.type === 'runtime_stopped'))
  assert.ok(frames.some(frame => frame.type === 'model_cancel' && frame.task_id === 'task_A'))
})
