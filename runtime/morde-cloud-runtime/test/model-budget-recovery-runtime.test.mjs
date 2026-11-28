import assert from 'node:assert/strict'
import { PassThrough } from 'node:stream'
import test from 'node:test'
import { modelCallID, modelRecoveryCallIDs, validStart } from '../src/engine.mjs'
import { WorkerIPC } from '../src/ipc.mjs'

function recoveryFrame(overrides = {}) {
  const base = {
    type: 'start', version: 1,
    task_id: 'budget_task_123', owner_id: 'budget_owner_123', agent_id: 'budget_agent_123',
    generation: 4, authorization_version: 7, session_id: 'budget_session_123',
    task_revision: 3, model: 'budget-test-model', tools: [],
    model_budget_admission: true, resume: true,
    checkpoint_through_seq: 9, checkpoint_sha256: 'ab'.repeat(32),
  }
  const failedCallID = modelCallID(base.task_id, base.session_id, base.checkpoint_through_seq)
  const ids = modelRecoveryCallIDs(base.task_id, base.task_revision, failedCallID, base.checkpoint_through_seq)
  return {
    ...base,
    model_recovery: {
      task_id: base.task_id,
      task_revision: base.task_revision,
      session_id: base.session_id,
      authorization_version: base.authorization_version,
      checkpoint_through_seq: base.checkpoint_through_seq,
      checkpoint_sha256: base.checkpoint_sha256,
      failed_call_id: failedCallID,
      recovery_dsh_call_id: ids.dshCallID,
      recovery_call_id: ids.ledgerCallID,
    },
    ...overrides,
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

test('recovery start binds the exact Task checkpoint and distinct DSH and ledger identities', () => {
  const start = recoveryFrame()
  assert.equal(validStart(start), true)
  assert.notEqual(start.model_recovery.failed_call_id, start.model_recovery.recovery_dsh_call_id)
  assert.notEqual(start.model_recovery.recovery_dsh_call_id, start.model_recovery.recovery_call_id)

  const badRecoveryID = structuredClone(start)
  badRecoveryID.model_recovery.recovery_dsh_call_id = badRecoveryID.model_recovery.failed_call_id
  assert.equal(validStart(badRecoveryID), false)

  const badCheckpoint = structuredClone(start)
  badCheckpoint.model_recovery.checkpoint_sha256 = 'cd'.repeat(32)
  assert.equal(validStart(badCheckpoint), false)

  const badRevision = structuredClone(start)
  badRevision.model_recovery.task_revision++
  assert.equal(validStart(badRevision), false)
})

test('recovery budget plan and final model frame use the authorized recovery DSH identity', async () => {
  const start = recoveryFrame()
  const identity = Object.fromEntries(['task_id', 'owner_id', 'agent_id', 'generation', 'authorization_version', 'session_id']
    .map(key => [key, start[key]]))
  const input = new PassThrough()
  const output = new PassThrough()
  const ipc = new WorkerIPC(input, output)
  writeFrame(input, start)
  await ipc.start.promise

  try {
    const recovery = start.model_recovery
    const failedCallID = recovery.failed_call_id
    const recoveryCallID = recovery.recovery_dsh_call_id
    const budgetCallID = '01234567-89ab-4cde-8123-456789abcdef'
    const payload = {
      turn: 1,
      step: 2,
      signal: new AbortController().signal,
      config: { provider: 'morde', model: start.model },
      messages: [{ role: 'user', content: [{ type: 'text', text: 'resume with a fresh budget' }] }],
      tools: [],
      toolHistory: { tools: [], updates: [] },
    }

    await assert.rejects(ipc.prepareModelBudget(identity, 9, budgetCallID, failedCallID, payload), /invalid/)

    const preparePromise = ipc.prepareModelBudget(identity, 9, budgetCallID, recoveryCallID, payload)
    const prepare = await nextOutputFrame(output)
    assert.equal(prepare.type, 'model_budget_prepare')
    assert.equal(prepare.call_id, budgetCallID)
    assert.equal(prepare.dsh_call_id, recoveryCallID)
    assert.equal(prepare.through_seq, recovery.checkpoint_through_seq)

    const planResponse = {
      type: 'model_budget_plan', id: prepare.id, ...identity,
      through_seq: prepare.through_seq, call_id: prepare.call_id, dsh_call_id: prepare.dsh_call_id,
      request_digest: prepare.request_digest, ok: true, budget_plan_id: 'recovery_plan_123', max_tokens: 640,
    }
    writeFrame(input, planResponse)
    const admission = await preparePromise

    const request = { ...prepare.request, max_tokens: admission.max_tokens }
    const streamPromise = (async () => {
      const chunks = []
      for await (const chunk of ipc.streamModel(identity, recoveryCallID, 9, request, undefined, admission)) chunks.push(chunk)
      return chunks
    })()
    const model = await nextOutputFrame(output)
    assert.equal(model.type, 'model')
    assert.equal(model.id, recoveryCallID)
    assert.equal(model.budget_call_id, budgetCallID)
    assert.equal(model.budget_plan_id, 'recovery_plan_123')
    assert.equal(model.request.max_tokens, 640)

    writeFrame(input, { type: 'model_chunk', ...identity, id: recoveryCallID, index: 0,
      chunk: { type: 'finish', reason: { kind: 'stop' } } })
    writeFrame(input, { type: 'model_end', ...identity, id: recoveryCallID, ok: true })
    const chunks = await streamPromise
    assert.equal(chunks.length, 1)
    assert.equal(chunks[0].type, 'finish')
  } finally {
    input.destroy()
    output.destroy()
  }
})

test('recovery final model frame uses its authorized DSH identity without budget admission', async () => {
  const start = recoveryFrame()
  delete start.model_budget_admission
  assert.equal(validStart(start), true)
  const identity = Object.fromEntries(['task_id', 'owner_id', 'agent_id', 'generation', 'authorization_version', 'session_id']
    .map(key => [key, start[key]]))
  const input = new PassThrough()
  const output = new PassThrough()
  const ipc = new WorkerIPC(input, output)
  writeFrame(input, start)
  await ipc.start.promise

  try {
    const recoveryCallID = start.model_recovery.recovery_dsh_call_id
    const request = { provider: 'morde', model: start.model,
      messages: [{ role: 'user', content: [{ type: 'text', text: 'resume' }] }], tools: [] }
    const recoveryThrough = start.model_recovery.checkpoint_through_seq + 5
    const streamPromise = (async () => {
      const chunks = []
      for await (const chunk of ipc.streamModel(identity, recoveryCallID, recoveryThrough,
        request, undefined)) chunks.push(chunk)
      return chunks
    })()
    const model = await nextOutputFrame(output)
    assert.equal(model.type, 'model')
    assert.equal(model.id, recoveryCallID)
    assert.equal(model.through_seq, recoveryThrough)
    assert.equal(Object.hasOwn(model, 'budget_call_id'), false)

    writeFrame(input, { type: 'model_chunk', ...identity, id: recoveryCallID, index: 0,
      chunk: { type: 'finish', reason: { kind: 'stop' } } })
    writeFrame(input, { type: 'model_end', ...identity, id: recoveryCallID, ok: true })
    assert.equal((await streamPromise).length, 1)
  } finally {
    input.destroy()
    output.destroy()
  }
})
