import assert from 'node:assert/strict'
import { PassThrough } from 'node:stream'
import test from 'node:test'
import { ModelBudgetPlanStore, modelCallID, projectModelBudgetDraft, runAgent, validStart } from '../src/engine.mjs'
import { modelBudgetRequestDigest, WorkerIPC } from '../src/ipc.mjs'

function taskIdentity(sessionID = 'budget_session_123') {
  return {
    task_id: 'budget_task_123', owner_id: 'budget_owner_123', agent_id: 'budget_agent_123',
    generation: 2, authorization_version: 3, session_id: sessionID,
  }
}

function startFrame(sessionID, modelBudgetAdmission = true) {
  return {
    type: 'start', version: 1, ...taskIdentity(sessionID), task_revision: 1, model: 'budget-test-model',
    tools: [], prompt: 'Answer this local runtime budget test.',
    ...(modelBudgetAdmission ? { model_budget_admission: true } : {}),
  }
}

function writeFrame(stream, frame) {
  stream.write(`${JSON.stringify(frame)}\n`)
}

function nextOutputFrame(output) {
  let buffer = ''
  return new Promise(resolve => {
    const onData = chunk => {
      buffer += chunk.toString('utf8')
      const end = buffer.indexOf('\n')
      if (end < 0) return
      output.off('data', onData)
      resolve(JSON.parse(buffer.slice(0, end)))
    }
    output.on('data', onData)
  })
}

function modelBudgetPlanResponse(request, { cap = 768, planID = 'budget_plan_123' } = {}) {
  return {
    type: 'model_budget_plan', id: request.id,
    ...Object.fromEntries(['task_id', 'owner_id', 'agent_id', 'generation', 'authorization_version', 'session_id']
      .map(key => [key, request[key]])),
    through_seq: request.through_seq, call_id: request.call_id, dsh_call_id: request.dsh_call_id,
    request_digest: request.request_digest,
    ok: true, budget_plan_id: planID, max_tokens: cap,
  }
}

function modelChunks(identity, request, text = 'local reply') {
  const chunks = [
    { type: 'block-start', index: 0, blockType: 'text' },
    { type: 'text-delta', index: 0, text },
    { type: 'block-end', index: 0, block: { type: 'text', text } },
    { type: 'finish', reason: { kind: 'stop' } },
  ]
  chunks.forEach((chunk, index) => writeFrame(request.output, {
    type: 'model_chunk', ...identity, id: request.id, index, chunk,
  }))
  writeFrame(request.output, { type: 'model_end', ...identity, id: request.id, ok: true })
}

function truncatedModelChunks(identity, request, text = 'partial local reply') {
  const chunks = [
    { type: 'block-start', index: 0, blockType: 'text' },
    { type: 'text-delta', index: 0, text },
    { type: 'block-end', index: 0, block: { type: 'text', text } },
    { type: 'finish', reason: { kind: 'max-tokens' }, replayState: {
      response: { morde_task_text_truncated: true, morde_task_call_id: request.id },
    } },
  ]
  chunks.forEach((chunk, index) => writeFrame(request.output, {
    type: 'model_chunk', ...identity, id: request.id, index, chunk,
  }))
  writeFrame(request.output, { type: 'model_end', ...identity, id: request.id, ok: true })
}

function isTaskCompactionRequest(request, config) {
  return config?.purpose === 'compaction' || request?.max_tokens === 384 ||
    JSON.stringify(request?.messages).includes('你负责把持续进行的 Morde AI 对话压缩成可靠的续聊检查点')
}

async function runLocalRuntime({ sessionID, admission = true, cap = 768, start: customStart, checkpointFrames = [],
  planForPrepare, respondToModel, respondToToolCall, respondToChildCreate, respondToChildSettle, onCheckpointFrame } = {}) {
  const start = customStart ?? startFrame(sessionID, admission)
  const identityForFrame = frame => Object.fromEntries(['task_id', 'owner_id', 'agent_id', 'generation', 'authorization_version', 'session_id']
    .map(key => [key, frame[key]]))
  const input = new PassThrough()
  const output = new PassThrough()
  const ipc = new WorkerIPC(input, output)
  const frames = []
  let buffer = ''
  let prepareIndex = 0
  let modelIndex = 0
  output.on('data', chunk => {
    buffer += chunk.toString('utf8')
    for (;;) {
      const end = buffer.indexOf('\n')
      if (end < 0) break
      const frame = JSON.parse(buffer.slice(0, end))
      buffer = buffer.slice(end + 1)
      frames.push(frame)
      if (frame.type === 'checkpoint') {
        writeFrame(input, {
          type: 'checkpoint_ack', ...identityForFrame(frame), id: frame.id, ok: true,
          through_seq: frame.through_seq, sha256: frame.sha256,
        })
        onCheckpointFrame?.(frame, { input, identity: identityForFrame(frame) })
      } else if (frame.type === 'checkpoint_read') {
        const page = checkpointFrames.find(entry => JSON.parse(entry.payload).from_seq === frame.from_seq)
        if (!page) throw new Error(`missing test checkpoint page from ${frame.from_seq}`)
        const body = JSON.parse(page.payload)
        writeFrame(input, {
          type: 'checkpoint_page', ...identityForFrame(frame), id: frame.id, ok: true, from_seq: body.from_seq,
          through_seq: page.through_seq, payload: page.payload, sha256: page.sha256,
        })
      } else if (frame.type === 'model_budget_prepare') {
        const selection = planForPrepare?.(frame, prepareIndex) ?? { cap, planID: `plan_${sessionID}` }
        prepareIndex++
        writeFrame(input, modelBudgetPlanResponse(frame, selection))
      } else if (frame.type === 'model') {
        const frameIdentity = identityForFrame(frame)
        if (respondToModel) respondToModel(frame, { input, identity: frameIdentity, index: modelIndex })
        else modelChunks(frameIdentity, { ...frame, output: input })
        modelIndex++
      } else if (frame.type === 'tool_call' && respondToToolCall) {
        respondToToolCall(frame, { input, identity: identityForFrame(frame) })
      } else if (frame.type === 'child_create' && respondToChildCreate) {
        respondToChildCreate(frame, { input, identity: identityForFrame(frame) })
      } else if (frame.type === 'child_settle' && respondToChildSettle) {
        respondToChildSettle(frame, { input, identity: identityForFrame(frame) })
      }
    }
  })
  writeFrame(input, start)
  await ipc.start.promise
  try {
    const result = await runAgent(ipc, start)
    return { frames, result }
  } finally {
    input.destroy()
    output.destroy()
  }
}

const budgetFileReadTool = {
  name: 'file_read', description: 'Read one local test file.',
  parameters: { type: 'object', properties: { path: { type: 'string' } }, required: ['path'], additionalProperties: false },
}
const budgetDelegateTool = {
  name: 'dsh_delegate', description: 'Delegate one bounded task to a child agent.',
  parameters: {
    type: 'object', properties: {
      title: { type: 'string' }, objective: { type: 'string' }, run_in_background: { type: 'boolean' },
      tool_names: { type: 'array', items: { type: 'string', enum: ['file_read'] }, uniqueItems: true },
    },
    required: ['title', 'objective', 'tool_names'], additionalProperties: false,
  },
}
const budgetJobTools = [
  { name: 'dsh_job_list', description: 'List background jobs.', parameters: { type: 'object', properties: {}, required: [], additionalProperties: false } },
  { name: 'dsh_job_get', description: 'Get a background job.', parameters: { type: 'object', properties: { job_id: { type: 'string' } }, required: ['job_id'], additionalProperties: false } },
  { name: 'dsh_job_output', description: 'Read background job output.', parameters: {
    type: 'object', properties: { job_id: { type: 'string' }, after_seq: { type: 'integer', minimum: -1 } },
    required: ['job_id', 'after_seq'], additionalProperties: false,
  } },
  { name: 'dsh_job_cancel', description: 'Cancel a background job.', parameters: { type: 'object', properties: { job_id: { type: 'string' } }, required: ['job_id'], additionalProperties: false } },
]

async function createIPC(start = startFrame('budget_session_123')) {
  const input = new PassThrough()
  const output = new PassThrough()
  const ipc = new WorkerIPC(input, output)
  writeFrame(input, start)
  await ipc.start.promise
  return { ipc, input, output }
}

const draftPayload = signal => ({
  turn: 1,
  step: 1,
  signal,
  config: { provider: 'morde', model: 'budget-test-model', maxTokens: 2_048, reasoningEffort: 'medium' },
  messages: [{ role: 'user', content: [{ type: 'text', text: 'estimate this request' }] }],
  tools: [],
  toolHistory: { tools: [], updates: [] },
})

test('model budget prepare sends a canonical draft and accepts only a strictly correlated plan', async () => {
  const { ipc, input, output } = await createIPC()
  const signal = new AbortController().signal
  const identity = taskIdentity()
  const callID = '01234567-89ab-4cde-8123-456789abcdef'
  const framePromise = nextOutputFrame(output)
  const planPromise = ipc.prepareModelBudget(identity, 9, callID, draftPayload(signal))
  const frame = await framePromise
  assert.equal(frame.type, 'model_budget_prepare')
  assert.equal(frame.call_id, callID)
  assert.equal(frame.through_seq, 9)
  assert.equal(frame.turn, 1)
  assert.equal(frame.step, 1)
  assert.equal(Object.hasOwn(frame.config, 'maxTokens'), false)
  assert.deepEqual(Object.keys(frame.request).sort(), ['messages', 'model', 'provider', 'tools'])
  assert.equal(frame.request_digest, modelBudgetRequestDigest(frame.request))
  assert.deepEqual(frame.tool_history, { tools: [], updates: [] })
  writeFrame(input, modelBudgetPlanResponse(frame, { cap: 1_536 }))
  const plan = await planPromise
  assert.equal(plan.call_id, callID)
  assert.equal(plan.through_seq, 9)
  assert.equal(plan.request_digest, frame.request_digest)
  assert.equal(plan.max_tokens, 1_536)
  assert.equal(ipc.failed, false)
  input.destroy()
  output.destroy()
})

test('model budget digest sorts object keys, preserves array order, and excludes the output cap', () => {
  const first = {
    provider: 'morde', model: 'budget-test-model', max_tokens: 128,
    messages: [{ role: 'user', content: [{ type: 'text', text: 'hello' }, { type: 'text', text: 'again' }] }],
    tools: [{ name: 'lookup', parameters: { b: 2, a: 1 } }],
  }
  const reordered = {
    tools: [{ parameters: { a: 1, b: 2 }, name: 'lookup' }],
    messages: [{ content: [{ text: 'hello', type: 'text' }, { text: 'again', type: 'text' }], role: 'user' }],
    model: 'budget-test-model', provider: 'morde', max_tokens: 4_096,
  }
  assert.equal(modelBudgetRequestDigest(first), modelBudgetRequestDigest(reordered))
  assert.notEqual(modelBudgetRequestDigest(first), modelBudgetRequestDigest({ ...reordered,
    messages: [{ ...reordered.messages[0], content: [...reordered.messages[0].content].reverse() }] }))
})

test('model budget plans are session-scoped, one-time, and clearable on session exit', () => {
  const store = new ModelBudgetPlanStore()
  const firstSignal = new AbortController().signal
  const wrongSignal = new AbortController().signal
  const secondSignal = new AbortController().signal
  const plan = (sessionID, digest, callID) => ({
    identity: taskIdentity(sessionID), through_seq: 8, call_id: callID,
    dsh_call_id: modelCallID(taskIdentity(sessionID).task_id, sessionID, 8),
    request_digest: digest, budget_plan_id: `plan_${sessionID}`, max_tokens: 1_024,
  })
  store.save(plan('budget_session_store_a', '11'.repeat(32), '77777777-7777-4777-8777-777777777777'), firstSignal)
  store.save(plan('budget_session_store_b', '22'.repeat(32), '88888888-8888-4888-8888-888888888888'), secondSignal)
  assert.throws(() => store.take('budget_session_store_a', wrongSignal), /signal/)
  assert.equal(store.take('budget_session_store_a', firstSignal).identity.session_id, 'budget_session_store_a')
  assert.throws(() => store.take('budget_session_store_a', firstSignal), /missing/)
  store.clearSession('budget_session_store_b')
  assert.throws(() => store.take('budget_session_store_b', secondSignal), /missing/)
})

test('provider draft projection matches the locked DSH file and tool-update projection', () => {
  const fileRef = { attachmentId: 'morde_file_12345678', name: 'notes.txt', bytes: 42 }
  const messages = [
    { role: 'user', content: [{ type: 'file', attachment: fileRef }] },
    { role: 'developer', id: 'tool_registry_message', content: [{ type: 'tool-addition', toolName: 'deferred_lookup' }] },
  ]
  const tools = [{ name: 'deferred_lookup', deferLoading: true, parameters: { type: 'object' } }]
  const projected = projectModelBudgetDraft(messages, tools, { tools, updates: [] }, {
    fileReadPath: ref => `/readonly/${ref.name}`,
  })
  assert.equal(projected.messages.length, 1)
  assert.equal(projected.messages[0].content[0].type, 'text')
  assert.match(projected.messages[0].content[0].text, /\/readonly\/notes\.txt/)
  assert.deepEqual(projected.tools, [{ name: 'deferred_lookup', parameters: { type: 'object' } }])
})

test('start frame budget admission is opt-in and accepts omission for legacy tasks', () => {
  assert.equal(validStart(startFrame('budget_session_opt_in')), true)
  assert.equal(validStart(startFrame('budget_session_legacy', false)), true)
  assert.equal(validStart({ ...startFrame('budget_session_disabled'), model_budget_admission: false }), false)
})

test('model budget prepare rejects a response with a changed identity, sequence, call id, digest, or extra field', async t => {
  const corrupt = [
    ['request id', frame => { frame.id = '99999999-9999-4999-8999-999999999999' }],
    ['identity', frame => { frame.owner_id = 'other_owner_123' }],
    ['through_seq', frame => { frame.through_seq += 1 }],
    ['call_id', frame => { frame.call_id = 'other_call_123' }],
    ['request_digest', frame => { frame.request_digest = '00'.repeat(32) }],
    ['non-positive max_tokens', frame => { frame.max_tokens = 0 }],
    ['extra field', frame => { frame.unexpected = true }],
  ]
  for (const [name, mutate] of corrupt) {
    await t.test(name, async () => {
      const { ipc, input, output } = await createIPC()
      const framePromise = nextOutputFrame(output)
      const promise = ipc.prepareModelBudget(taskIdentity(), 4, '11111111-1111-4111-8111-111111111111', draftPayload(new AbortController().signal))
      const request = await framePromise
      const response = modelBudgetPlanResponse(request)
      mutate(response)
      const rejected = assert.rejects(promise, /model budget|worker connection|invalid worker frame/)
      writeFrame(input, response)
      await rejected
      assert.equal(ipc.failed, true)
      input.destroy()
      output.destroy()
    })
  }
})

test('an explicit denial fails closed without turning it into a valid plan', async () => {
  const { ipc, input, output } = await createIPC()
  const framePromise = nextOutputFrame(output)
  const promise = ipc.prepareModelBudget(taskIdentity(), 3, '22222222-2222-4222-8222-222222222222', draftPayload(new AbortController().signal))
  const request = await framePromise
  const denial = {
    type: 'model_budget_plan', id: request.id, ...taskIdentity(), through_seq: request.through_seq,
    call_id: request.call_id, dsh_call_id: request.dsh_call_id, request_digest: request.request_digest, ok: false,
  }
  const rejected = assert.rejects(promise, /denied/)
  writeFrame(input, denial)
  await rejected
  assert.equal(ipc.failed, false)
  input.destroy()
  output.destroy()
})

test('an unknown budget capability fails closed', async () => {
  const { ipc, input, output } = await createIPC()
  const framePromise = nextOutputFrame(output)
  const promise = ipc.prepareModelBudget(taskIdentity(), 6, '55555555-5555-4555-8555-555555555555', draftPayload(new AbortController().signal))
  const request = await framePromise
  const rejected = assert.rejects(promise, /worker connection|invalid worker frame/)
  writeFrame(input, { type: 'model_budget_unsupported', id: request.id })
  await rejected
  assert.equal(ipc.failed, true)
  input.destroy()
  output.destroy()
})

test('request cancellation fails closed and discards a late matching plan', async () => {
  const { ipc, input, output } = await createIPC()
  const controller = new AbortController()
  const framePromise = nextOutputFrame(output)
  const promise = ipc.prepareModelBudget(taskIdentity(), 5, '33333333-3333-4333-8333-333333333333', draftPayload(controller.signal))
  const request = await framePromise
  const rejected = assert.rejects(promise, /cancelled/)
  controller.abort()
  await rejected
  writeFrame(input, modelBudgetPlanResponse(request))
  await new Promise(resolve => setImmediate(resolve))
  assert.equal(ipc.failed, false)
  assert.equal(ipc.modelBudgetAdmissions.size, 0)
  input.destroy()
  output.destroy()
})

test('model budget timeout fails closed', async t => {
  const { ipc, input, output } = await createIPC()
  t.mock.timers.enable({ apis: ['setTimeout'] })
  try {
    const framePromise = nextOutputFrame(output)
    const promise = ipc.prepareModelBudget(taskIdentity(), 7, '66666666-6666-4666-8666-666666666666', draftPayload(new AbortController().signal))
    await framePromise
    const rejected = assert.rejects(promise, /timed out/)
    t.mock.timers.tick(30_001)
    await rejected
    assert.equal(ipc.failed, false)
  } finally {
    t.mock.timers.reset()
    input.destroy()
    output.destroy()
  }
})

test('opt-in DSH stores the admitted cap in its request header and sends the same cap with its model frame', async () => {
  const { frames } = await runLocalRuntime({ sessionID: 'budget_session_header', cap: 1_280 })
  const prepare = frames.find(frame => frame.type === 'model_budget_prepare')
  const model = frames.find(frame => frame.type === 'model')
  assert.ok(prepare)
  assert.ok(model)
  assert.equal(model.request.max_tokens, 1_280)
  assert.equal(model.budget_plan_id, `plan_${prepare.session_id}`)
  assert.equal(model.budget_call_id, prepare.call_id)
  assert.equal(prepare.dsh_call_id, modelCallID(model.task_id, model.session_id, prepare.through_seq))
  assert.equal(model.request_digest, prepare.request_digest)
  assert.equal(modelBudgetRequestDigest(prepare.request), prepare.request_digest)
  assert.equal(modelBudgetRequestDigest(model.request), prepare.request_digest)
  assert.ok(model.through_seq > prepare.through_seq)
  assert.match(prepare.call_id, /^[a-f0-9-]{36}$/i)
  assert.equal(prepare.call_id === model.id, false)
  assert.equal(model.id, modelCallID(model.task_id, model.session_id, model.through_seq))

  const events = frames.filter(frame => frame.type === 'checkpoint').flatMap(frame => JSON.parse(frame.payload).events)
  const header = events.findLast(event => event.type === 'request/header')?.data?.header
  assert.equal(header?.config.maxTokens, 1_280)
})

test('resumed DSH tool additions and removals produce one projected digest through WorkerModel.stream', async () => {
  const sessionID = 'budget_session_tool_change'
  const oldTool = { name: 'old_lookup', description: 'Previous tool schema.', parameters: { type: 'object', properties: {}, required: [], additionalProperties: false } }
  const newTool = { name: 'new_lookup', description: 'Current tool schema.', parameters: { type: 'object', properties: {}, required: [], additionalProperties: false } }
  const first = await runLocalRuntime({
    sessionID,
    start: { ...startFrame(sessionID, true), tools: [oldTool] },
    cap: 640,
  })
  const finalCheckpoint = first.frames.filter(frame => frame.type === 'checkpoint').at(-1)
  assert.ok(finalCheckpoint)
  const { prompt: _prompt, ...resumeBase } = startFrame(sessionID, true)
  const resumedStart = {
    ...resumeBase, tools: [newTool],
    revision_messages: [{ message_id: 'mchg0001', content: 'Continue after the tool schema changed.', attachment_ids: [] }],
    resume: true, checkpoint_through_seq: finalCheckpoint.through_seq, checkpoint_sha256: finalCheckpoint.sha256,
  }
  const resumed = await runLocalRuntime({
    sessionID,
    start: resumedStart,
    checkpointFrames: first.frames.filter(frame => frame.type === 'checkpoint'),
    cap: 1_024,
  })
  const prepare = resumed.frames.find(frame => frame.type === 'model_budget_prepare')
  const model = resumed.frames.find(frame => frame.type === 'model')
  assert.ok(prepare && model)
  assert.equal(model.request.max_tokens, 1_024)
  assert.equal(model.budget_call_id, prepare.call_id)
  assert.equal(prepare.dsh_call_id, modelCallID(model.task_id, model.session_id, prepare.through_seq))
  assert.equal(model.request_digest, prepare.request_digest)
  assert.equal(modelBudgetRequestDigest(prepare.request), prepare.request_digest)
  assert.equal(modelBudgetRequestDigest(model.request), prepare.request_digest)
  assert.equal(prepare.request.tools.some(tool => tool.name === 'old_lookup'), false)
  assert.equal(prepare.request.tools.some(tool => tool.name === 'new_lookup'), true)
  assert.equal(prepare.request.messages.some(message => message.role === 'developer'), false)
  assert.equal(prepare.tool_history.updates.length > 0, true)
})

test('the same model keeps concurrent Agents isolated by session budget plan', async () => {
  const [first, second] = await Promise.all([
    runLocalRuntime({ sessionID: 'budget_session_parallel_a', cap: 512 }),
    runLocalRuntime({ sessionID: 'budget_session_parallel_b', cap: 3_072 }),
  ])
  for (const [runtime, cap] of [[first, 512], [second, 3_072]]) {
    const prepare = runtime.frames.find(frame => frame.type === 'model_budget_prepare')
    const model = runtime.frames.find(frame => frame.type === 'model')
    assert.ok(prepare && model)
    assert.equal(model.request.max_tokens, cap)
    assert.equal(model.budget_plan_id, `plan_${prepare.session_id}`)
    assert.equal(model.budget_call_id, prepare.call_id)
    assert.equal(model.request_digest, prepare.request_digest)
  }
  assert.notEqual(first.frames.find(frame => frame.type === 'model_budget_prepare').session_id,
    second.frames.find(frame => frame.type === 'model_budget_prepare').session_id)
})

test('parallel DSH child Agents under one Task keep independent budget plans through settlement', { timeout: 15_000 }, async () => {
  const sessionID = 'budget_parent_task_session'
  const childSessionByCall = new Map([
    ['budget_child_call_a', 'budget_child_agent_a'],
    ['budget_child_call_b', 'budget_child_agent_b'],
  ])
  const capBySession = new Map([
    ['budget_child_agent_a', 512],
    ['budget_child_agent_b', 3_072],
  ])
  const start = { ...startFrame(sessionID, true), tools: [budgetDelegateTool, budgetFileReadTool, ...budgetJobTools] }
  const prepares = []
  const childModels = []
  const childSettlements = []
  const childSummaryBySession = new Map()
  let parentModels = 0

  const { frames, result } = await runLocalRuntime({
    sessionID,
    start,
    planForPrepare: (prepare, index) => {
      prepares.push(prepare)
      const cap = capBySession.get(prepare.session_id) ?? 768
      return { cap, planID: `parallel_plan_${prepare.session_id}_${index}` }
    },
    respondToChildCreate: (frame, { input, identity }) => {
      const childSessionID = childSessionByCall.get(frame.call_id)
      assert.ok(childSessionID, `unexpected child call ${frame.call_id}`)
      writeFrame(input, {
        type: 'child_session', ...identity, session_id: childSessionID, call_id: frame.call_id,
        parent_session_id: frame.session_id, objective: frame.arguments.objective,
        tools: [budgetFileReadTool], checkpoint_through_seq: -1, checkpoint_sha256: '',
        step_status: 'running', summary: '',
      })
    },
    respondToChildSettle: (frame, { input, identity }) => {
      childSettlements.push(frame)
      writeFrame(input, { type: 'child_settle_ack', ...identity, call_id: frame.call_id, ok: true })
    },
    respondToModel: (frame, { input, identity }) => {
      if (frame.session_id === sessionID) {
        parentModels++
        if (parentModels === 1) {
          const calls = [
            { id: 'budget_child_call_a', title: 'Worker A', objective: 'Return result A.' },
            { id: 'budget_child_call_b', title: 'Worker B', objective: 'Return result B.' },
          ]
          calls.forEach((call, index) => writeFrame(input, {
            type: 'model_chunk', ...identity, id: frame.id, index,
            chunk: { type: 'tool-call-delta', index, id: call.id, name: budgetDelegateTool.name,
              argumentsDelta: JSON.stringify({ title: call.title, objective: call.objective,
                tool_names: ['file_read'], run_in_background: true }) },
          }))
          writeFrame(input, { type: 'model_chunk', ...identity, id: frame.id, index: calls.length,
            chunk: { type: 'finish', reason: { kind: 'tool-calls' } } })
          writeFrame(input, { type: 'model_end', ...identity, id: frame.id, ok: true })
          return
        }
        modelChunks(identity, { ...frame, output: input })
        return
      }

      childModels.push({ frame, identity })
      if (childModels.length !== 2) return
      assert.notEqual(childModels[0].frame.session_id, childModels[1].frame.session_id,
        'the concurrent requests must belong to two real child sessions')
      for (const [index, child] of childModels.entries()) {
        const cap = capBySession.get(child.frame.session_id)
        assert.ok(cap, `unknown child session ${child.frame.session_id}`)
        const summary = `child result ${index ? 'B' : 'A'}`
        childSummaryBySession.set(child.frame.session_id, summary)
        modelChunks(child.identity, { ...child.frame, output: input }, summary)
      }
    },
  })

  assert.equal(result.type, 'settled')
  assert.equal(result.state, 'completed_candidate', JSON.stringify(frames.map(frame => frame.type)))
  assert.equal(parentModels, 2)
  assert.equal(childModels.length, 2, 'both child Agents must make their model request before either is answered')
  const childPrepares = prepares.filter(frame => capBySession.has(frame.session_id))
  const childModelFrames = frames.filter(frame => frame.type === 'model' && capBySession.has(frame.session_id))
  assert.equal(childPrepares.length, 2)
  assert.equal(childModelFrames.length, 2)
  assert.equal(childSettlements.length, 2)
  assert.deepEqual(new Set(childSettlements.map(frame => frame.status)), new Set(['succeeded']))
  assert.deepEqual(new Set(childSettlements.map(frame => frame.session_id)), new Set(capBySession.keys()))
  for (const child of childSettlements) {
    assert.equal(childSessionByCall.get(child.call_id), child.session_id,
      'each child settlement must match the delegated call that created that child')
    assert.equal(child.summary, childSummaryBySession.get(child.session_id),
      'each child settlement must contain its own model result')
    assert.equal(child.task_id, start.task_id)
  }

  const callIDs = new Set()
  const dshCallIDs = new Set()
  const planIDs = new Set()
  for (const prepare of childPrepares) {
    assert.equal(prepare.task_id, start.task_id)
    assert.equal(prepare.owner_id, start.owner_id)
    assert.equal(prepare.agent_id, start.agent_id)
    assert.equal(prepare.generation, start.generation)
    assert.equal(prepare.authorization_version, start.authorization_version)
    assert.notEqual(prepare.session_id, sessionID)
    assert.notEqual(callIDs.has(prepare.call_id), true)
    callIDs.add(prepare.call_id)
    assert.equal(prepare.dsh_call_id, modelCallID(prepare.task_id, prepare.session_id, prepare.through_seq))
    assert.equal(dshCallIDs.has(prepare.dsh_call_id), false)
    dshCallIDs.add(prepare.dsh_call_id)
    const model = childModelFrames.find(candidate => candidate.budget_call_id === prepare.call_id)
    assert.ok(model, `missing child model frame for ${prepare.session_id}`)
    assert.equal(model.request.max_tokens, capBySession.get(prepare.session_id))
    assert.equal(model.budget_plan_id, `parallel_plan_${prepare.session_id}_${prepares.indexOf(prepare)}`)
    assert.equal(model.request_digest, prepare.request_digest)
    assert.equal(modelBudgetRequestDigest(prepare.request), prepare.request_digest)
    assert.equal(modelBudgetRequestDigest(model.request), prepare.request_digest)
    assert.notEqual(planIDs.has(model.budget_plan_id), true)
    planIDs.add(model.budget_plan_id)
  }
  const childCreateFrames = frames.filter(frame => frame.type === 'child_create')
  assert.equal(childCreateFrames.length, 2)
  assert.ok(childCreateFrames.every(frame => frame.session_id === sessionID && frame.task_id === start.task_id),
    'both child Agents must be spawned by the same parent session and Task')
})

test('opt-in model stream rejects mutated input or a missing plan before sending a model request', async t => {
  async function assertNoModelRequest(ipc, output) {
    await ipc.writes
    const buffered = output.read()
    const frames = buffered === null ? [] : buffered.toString('utf8').split('\n').filter(Boolean).map(line => JSON.parse(line))
    assert.equal(frames.some(frame => frame.type === 'model'), false)
  }

  await t.test('mutated request', async () => {
    const { ipc, input, output } = await createIPC()
    const framePromise = nextOutputFrame(output)
    const planPromise = ipc.prepareModelBudget(taskIdentity(), 5, '44444444-4444-4444-8444-444444444444', draftPayload(new AbortController().signal))
    const prepare = await framePromise
    writeFrame(input, modelBudgetPlanResponse(prepare, { cap: 512 }))
    const plan = await planPromise
    const stream = ipc.streamModel(taskIdentity(), modelCallID(taskIdentity().task_id, taskIdentity().session_id, 7), 7, {
      provider: 'morde', model: 'budget-test-model', max_tokens: 512,
      messages: [{ role: 'user', content: [{ type: 'text', text: 'changed after admission' }] }], tools: [],
    }, undefined, plan)
    await assert.rejects(async () => { for await (const _chunk of stream) {} }, /changed after budget admission/)
    assert.equal(ipc.models.size, 0)
    await assertNoModelRequest(ipc, output)
    input.destroy()
    output.destroy()
  })

  await t.test('missing plan', async () => {
    const { ipc, input, output } = await createIPC()
    const identity = taskIdentity()
    const stream = ipc.streamModel(identity, modelCallID(identity.task_id, identity.session_id, 7), 7, {
      provider: 'morde', model: 'budget-test-model', max_tokens: 512, messages: [], tools: [],
    })
    await assert.rejects(async () => { for await (const _chunk of stream) {} }, /model budget plan is missing/)
    assert.equal(ipc.models.size, 0)
    await assertNoModelRequest(ipc, output)
    input.destroy()
    output.destroy()
  })
})

test('budget admission stays disabled by default and preserves the old model frame', async () => {
  const { frames } = await runLocalRuntime({ sessionID: 'budget_session_legacy', admission: false })
  const model = frames.find(frame => frame.type === 'model')
  assert.ok(model)
  assert.equal(frames.some(frame => frame.type === 'model_budget_prepare'), false)
  assert.equal(Object.hasOwn(model, 'budget_plan_id'), false)
  assert.equal(Object.hasOwn(model, 'budget_call_id'), false)
  assert.equal(Object.hasOwn(model, 'request_digest'), false)
})

test('signed pure-text truncation continues once with a fresh budget plan', { timeout: 30_000 }, async () => {
  const sessionID = 'budget_session_text_continue'
  const start = {
    type: 'start', version: 1, ...taskIdentity(sessionID), task_revision: 1, model: 'budget-test-model',
    tools: [], initial_message: { message_id: 'mcont001', content: 'Give a complete multi-part answer.', attachment_ids: [] },
    model_budget_admission: true,
  }
  const { frames, result } = await runLocalRuntime({
    sessionID,
    start,
    planForPrepare: (_prepare, index) => ({ cap: 768, planID: `plan_text_continue_${index}` }),
    respondToModel: (frame, { input, identity, index }) => index === 0
      ? truncatedModelChunks(identity, { ...frame, output: input }, 'The first part.')
      : modelChunks(identity, { ...frame, output: input }),
  })

  assert.equal(result.type, 'settled')
  assert.equal(result.state, 'completed_candidate')
  const prepares = frames.filter(frame => frame.type === 'model_budget_prepare')
  const models = frames.filter(frame => frame.type === 'model')
  assert.equal(prepares.length, 2)
  assert.equal(models.length, 2)
  assert.notEqual(prepares[0].call_id, prepares[1].call_id)
  assert.ok(prepares[1].through_seq > models[0].through_seq)
  for (const [index, prepare] of prepares.entries()) {
    const model = models.find(candidate => candidate.budget_call_id === prepare.call_id)
    assert.ok(model)
    assert.equal(model.budget_plan_id, `plan_text_continue_${index}`)
    assert.equal(model.request_digest, prepare.request_digest)
  }
  assert.ok(models[1].request.messages.some(message => message.role === 'user' &&
    message.content?.some(block => block.type === 'text' && block.text.includes('Continue from where it ended'))))
  assert.ok(models[1].request.messages.some(message => message.role === 'assistant' &&
    message.content?.some(block => block.type === 'text' && block.text === 'The first part.')))
  const events = frames.filter(frame => frame.type === 'checkpoint').flatMap(frame => JSON.parse(frame.payload).events)
  const continuation = events.find(event => event.type === 'user/message' &&
    event.data.source?.plugin === 'morde-budget-continuation')
  assert.ok(continuation)
  assert.equal(continuation.data.source.ordinal, 1)
  assert.equal(continuation.data.source.call_id, models[0].id)
})

test('signed text continuation stops after two continuations and does not expose tools', { timeout: 30_000 }, async () => {
  const sessionID = 'budget_session_text_continue_limit'
  const start = {
    type: 'start', version: 1, ...taskIdentity(sessionID), task_revision: 1, model: 'budget-test-model',
    tools: [], initial_message: { message_id: 'mcont002', content: 'Give a complete multi-part answer.', attachment_ids: [] },
    model_budget_admission: true,
  }
  const { frames, result } = await runLocalRuntime({
    sessionID,
    start,
    planForPrepare: (_prepare, index) => ({ cap: 768, planID: `plan_text_limit_${index}` }),
    respondToModel: (frame, { input, identity, index }) => truncatedModelChunks(identity, { ...frame, output: input }, `partial ${index}`),
  })

  assert.equal(result.type, 'settled')
  assert.equal(result.state, 'failed')
  assert.equal(frames.filter(frame => frame.type === 'model_budget_prepare').length, 3)
  assert.equal(frames.filter(frame => frame.type === 'model').length, 3)
  const events = frames.filter(frame => frame.type === 'checkpoint').flatMap(frame => JSON.parse(frame.payload).events)
  const continuations = events.filter(event => event.type === 'user/message' &&
    event.data.source?.plugin === 'morde-budget-continuation')
  assert.equal(continuations.length, 2)
  assert.deepEqual(continuations.map(event => event.data.source.ordinal), [1, 2])

  const toolSessionID = 'budget_session_text_continue_tool'
  const toolStart = {
    type: 'start', version: 1, ...taskIdentity(toolSessionID), task_revision: 1, model: 'budget-test-model',
    tools: [{ name: 'test_lookup', description: 'Test lookup', parameters: { type: 'object', properties: {}, additionalProperties: false } }],
    initial_message: { message_id: 'mcont003', content: 'Answer using tools if needed.', attachment_ids: [] },
    model_budget_admission: true,
  }
  const toolResult = await runLocalRuntime({
    sessionID: toolSessionID,
    start: toolStart,
    respondToModel: (frame, { input, identity }) => truncatedModelChunks(identity, { ...frame, output: input }),
  })
  assert.equal(toolResult.frames.filter(frame => frame.type === 'model').length, 1)
  const toolEvents = toolResult.frames.filter(frame => frame.type === 'checkpoint').flatMap(frame => JSON.parse(frame.payload).events)
  assert.equal(toolEvents.some(event => event.type === 'user/message' &&
    event.data.source?.plugin === 'morde-budget-continuation'), false)
})

test('new user input preempts a queued automatic text continuation', { timeout: 30_000 }, async () => {
  const sessionID = 'budget_session_text_continue_user'
  const start = {
    type: 'start', version: 1, ...taskIdentity(sessionID), task_revision: 1, model: 'budget-test-model',
    tools: [], initial_message: { message_id: 'mcont004', content: 'Give a complete multi-part answer.', attachment_ids: [] },
    model_budget_admission: true,
  }
  let delivered = false
  const { frames, result } = await runLocalRuntime({
    sessionID,
    start,
    respondToModel: (frame, { input, identity, index }) => index === 0
      ? truncatedModelChunks(identity, { ...frame, output: input }, 'The first part.')
      : modelChunks(identity, { ...frame, output: input }),
    onCheckpointFrame: (frame, { input, identity }) => {
      if (delivered) return
      let events
      try { events = JSON.parse(frame.payload).events } catch { return }
      if (!events.some(event => event.type === 'user/message' &&
          event.data.source?.plugin === 'morde-budget-continuation')) return
      delivered = true
      writeFrame(input, {
        type: 'message_delivery', version: 2, ...identity, message_id: 'mnew0001',
        content: 'Actually, answer only the cost section.', attachment_ids: [],
      })
    },
  })

  assert.equal(delivered, true)
  assert.equal(result.type, 'settled')
  assert.equal(result.state, 'completed_candidate')
  const models = frames.filter(frame => frame.type === 'model')
  assert.equal(models.length, 2)
  assert.ok(models[1].request.messages.some(message => message.role === 'user' &&
    message.content?.some(block => block.type === 'text' && block.text.includes('Actually, answer only the cost section.'))))
})

test('opt-in Task compaction prepares its direct LLM stream against the final projected request',
  { timeout: 30_000 }, async () => {
    const sessionID = 'budget_session_compaction'
    const start = {
      type: 'start', version: 1, ...taskIdentity(sessionID), task_revision: 1, model: 'budget-test-model',
      tools: [], context_window_tokens: 4_096,
      initial_message: { message_id: 'mcompact', content: 'Preserve this opening requirement.', attachment_ids: [] },
      model_budget_admission: true,
    }
    let followupsDelivered = false
    const { frames, result } = await runLocalRuntime({
      sessionID,
      start,
      planForPrepare: (prepare, index) => ({
        cap: isTaskCompactionRequest(prepare.request, prepare.config) ? 384 : 768,
        planID: `plan_budget_compaction_${index}`,
      }),
      respondToModel: (frame, { input, identity, index }) => {
        if (index === 0 && !followupsDelivered) {
          followupsDelivered = true
          for (let followup = 1; followup <= 20; followup++) {
            writeFrame(input, {
              type: 'message_delivery', version: 2, ...identity,
              message_id: `mf${String(followup).padStart(6, '0')}`,
              content: `followup-${followup} ${'history '.repeat(275)}`,
              attachment_ids: [],
            })
          }
        }
        modelChunks(identity, { ...frame, output: input })
      },
    })

    assert.equal(result.type, 'settled')
    assert.equal(result.state, 'completed_candidate')
    const prepareFrames = frames.filter(frame => frame.type === 'model_budget_prepare' && isTaskCompactionRequest(frame.request, frame.config))
    const allPrepareFrames = frames.filter(frame => frame.type === 'model_budget_prepare')
    const modelFrames = frames.filter(frame => frame.type === 'model' && isTaskCompactionRequest(frame.request))
    assert.ok(prepareFrames.length > 0, `no Task compaction prepare frame among ${frames.map(frame => frame.type).join(', ')}`)
    assert.equal(modelFrames.length, prepareFrames.length)
    for (const prepare of prepareFrames) {
      const model = modelFrames.find(candidate => candidate.budget_call_id === prepare.call_id)
      assert.ok(model, `no final model frame matched compaction call ${prepare.call_id}`)
      assert.equal(prepare.request_digest, model.request_digest)
      assert.equal(modelBudgetRequestDigest(prepare.request), prepare.request_digest)
      assert.equal(modelBudgetRequestDigest(model.request), prepare.request_digest)
      assert.equal(model.request.max_tokens, 384)
      assert.equal(model.budget_plan_id, `plan_budget_compaction_${allPrepareFrames.indexOf(prepare)}`)
      assert.equal(model.through_seq, prepare.through_seq)
    }

    const events = frames.filter(frame => frame.type === 'checkpoint').flatMap(frame => JSON.parse(frame.payload).events)
    assert.ok(events.some(event => event.type === 'compaction/summary'))
    assert.ok(events.some(event => event.type === 'compaction/end' && !event.data.error))
    const firstCompactionPrepare = prepareFrames[0]
    const prefix = events.filter(event => event.seq <= firstCompactionPrepare.through_seq)
    let activeTurn
    let activeStep
    let completedStep = 0
    for (const event of prefix) {
      if (event.type === 'turn/start') {
        activeTurn = event.data.turn
        activeStep = undefined
        completedStep = 0
      } else if (event.type === 'step/start' && event.data.turn === activeTurn) {
        activeStep = event.data.step
      } else if (event.type === 'step/end' && event.data.turn === activeTurn) {
        completedStep = event.data.step
        activeStep = undefined
      } else if (event.type === 'turn/end' && event.data.turn === activeTurn) {
        activeTurn = undefined
        activeStep = undefined
        completedStep = 0
      }
    }
    assert.equal(firstCompactionPrepare.turn, activeTurn)
    assert.equal(firstCompactionPrepare.step, activeStep ?? completedStep + 1)
  })

test('context-overflow compaction retry prepares a fresh plan after checkpointing the failed attempt',
  { timeout: 30_000 }, async () => {
    const sessionID = 'budget_session_compaction_retry'
    const tool = {
      name: 'budget_lookup', description: 'Returns a local synthetic result.',
      parameters: { type: 'object', properties: { query: { type: 'string' } }, required: ['query'], additionalProperties: false },
    }
    const start = {
      type: 'start', version: 1, ...taskIdentity(sessionID), task_revision: 1, model: 'budget-test-model',
      tools: [tool], context_window_tokens: 4_096,
      initial_message: { message_id: 'mcompct2', content: 'Use the lookup result and preserve the opening requirement.', attachment_ids: [] },
      model_budget_admission: true,
    }
    let toolRequested = false
    let compactionFailed = false
    let followupsDelivered = false
    const { frames, result } = await runLocalRuntime({
      sessionID,
      start,
      planForPrepare: (prepare, index) => ({
        cap: isTaskCompactionRequest(prepare.request, prepare.config) ? 384 : 768,
        planID: `plan_budget_retry_${index}`,
      }),
      respondToModel: (frame, { input, identity }) => {
        if (isTaskCompactionRequest(frame.request)) {
          if (!compactionFailed) {
            compactionFailed = true
            writeFrame(input, { type: 'model_end', ...identity, id: frame.id, ok: false,
              failure_code: 'CONTEXT_WINDOW_EXCEEDED' })
          } else {
            modelChunks(identity, { ...frame, output: input })
          }
          return
        }
        if (!toolRequested) {
          toolRequested = true
          const chunks = [
            { type: 'tool-call-delta', index: 0, id: 'budget_tool_call_01', name: tool.name,
              argumentsDelta: JSON.stringify({ query: 'synthetic' }) },
            { type: 'finish', reason: { kind: 'tool-calls' } },
          ]
          chunks.forEach((chunk, index) => writeFrame(input, {
            type: 'model_chunk', ...identity, id: frame.id, index, chunk,
          }))
          writeFrame(input, { type: 'model_end', ...identity, id: frame.id, ok: true })
          return
        }
        modelChunks(identity, { ...frame, output: input })
      },
      respondToToolCall: (frame, { input, identity }) => {
        writeFrame(input, {
          type: 'tool_result', ...identity, id: frame.id, call_id: frame.call_id,
          status: 'succeeded', output: `full synthetic tool output ${'evidence '.repeat(5_000)}`, images: [],
        })
        if (!followupsDelivered) {
          followupsDelivered = true
          for (let followup = 1; followup <= 20; followup++) {
            writeFrame(input, {
              type: 'message_delivery', version: 2, ...identity,
              message_id: `mt${String(followup).padStart(6, '0')}`,
              content: `followup-${followup} ${'history '.repeat(275)}`, attachment_ids: [],
            })
          }
        }
      },
    })

    assert.equal(result.type, 'settled')
    assert.equal(result.state, 'completed_candidate', JSON.stringify(frames.map(frame => frame.type)))
    const compactionPrepares = frames.filter(frame => frame.type === 'model_budget_prepare' && isTaskCompactionRequest(frame.request, frame.config))
    const allPrepareFrames = frames.filter(frame => frame.type === 'model_budget_prepare')
    const compactionModels = frames.filter(frame => frame.type === 'model' && isTaskCompactionRequest(frame.request))
    assert.ok(compactionPrepares.length >= 2, `expected initial and degraded compaction plans; frames=${frames.map(frame => frame.type).join(', ')}`)
    assert.equal(compactionModels.length, compactionPrepares.length)
    const [initialPrepare, retryPrepare] = compactionPrepares
    const initialModel = compactionModels.find(candidate => candidate.budget_call_id === initialPrepare.call_id)
    const retryModel = compactionModels.find(candidate => candidate.budget_call_id === retryPrepare.call_id)
    assert.ok(initialModel)
    assert.ok(retryModel)
    assert.notEqual(initialPrepare.call_id, retryPrepare.call_id)
    assert.notEqual(initialPrepare.request_digest, retryPrepare.request_digest)
    assert.notEqual(initialPrepare.id, retryPrepare.id)
    assert.ok(retryPrepare.through_seq > initialModel.through_seq,
      'the failed call must checkpoint its non-surface assistant attempt before retry preparation')
    for (const [prepare, model] of [[initialPrepare, initialModel], [retryPrepare, retryModel]]) {
      assert.equal(model.budget_call_id, prepare.call_id)
      assert.equal(model.budget_plan_id, `plan_budget_retry_${allPrepareFrames.indexOf(prepare)}`)
      assert.equal(model.request.max_tokens, 384)
      assert.equal(model.request_digest, prepare.request_digest)
      assert.equal(modelBudgetRequestDigest(prepare.request), prepare.request_digest)
      assert.equal(modelBudgetRequestDigest(model.request), prepare.request_digest)
    }
    const events = frames.filter(frame => frame.type === 'checkpoint').flatMap(frame => JSON.parse(frame.payload).events)
    assert.equal(events.filter(event => event.type === 'assistant/attempt').length, 1)
    assert.ok(events.some(event => event.type === 'compaction/summary' &&
      event.data.summary?.some(block => block.type === 'text' && block.text.includes('覆盖不足'))))
    assert.ok(events.some(event => event.type === 'compaction/end' && !event.data.error))
  })
