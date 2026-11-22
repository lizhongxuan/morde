import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { once } from 'node:events'
import { createInterface } from 'node:readline'
import test from 'node:test'

const identityKeys = ['context_role', 'conversation_id', 'message_id', 'owner_id', 'actor_id', 'agent_id', 'generation',
  'authorization_version', 'session_id', 'purpose', 'operation_id']
const fence = frame => Object.fromEntries(identityKeys.map(key => [key, frame[key]]))
const start = {
  type: 'start', version: 1, context_role: 'coordinator', conversation_id: 'conversation_recovery',
  message_id: 'm1234567', owner_id: 'user_recovery', actor_id: 'actor_recovery', agent_id: 'agent_recovery',
  generation: 1, authorization_version: 1, session_id: 'session_recovery', purpose: 'compaction',
  operation_id: 'operation_recovery', model: 'test-model', tools: [], context_window_tokens: 32_768,
  system_prompt: 'Generate a reliable summary of the authorized conversation.',
  source_messages: [
    { kind: 'message', id: 'm1234567', object_type: '', version: 0, sequence: 1, role: 'user',
      content: 'Preserve this original preference and early requirement. '.repeat(30) },
    { kind: 'message', id: 'm2345678', object_type: '', version: 0, sequence: 2, role: 'assistant',
      content: 'The earlier preference remains active. '.repeat(30) },
    { kind: 'message', id: 'm3456789', object_type: '', version: 0, sequence: 3, role: 'user',
      content: 'Preserve this original preference and early requirement. '.repeat(30) },
    { kind: 'message', id: 'm4567890', object_type: '', version: 0, sequence: 4, role: 'assistant',
      content: 'The previous decision applies to this work. '.repeat(30) },
    { kind: 'message', id: 'm5678901', object_type: '', version: 0, sequence: 5, role: 'user',
      content: 'Keep this new constraint available. '.repeat(30) },
    { kind: 'message', id: 'm6789012', object_type: '', version: 0, sequence: 6, role: 'assistant',
      content: 'The new constraint is recorded. '.repeat(30) },
    { kind: 'message', id: 'm7890123', object_type: '', version: 0, sequence: 7, role: 'user',
      content: 'Answer the current request with the accumulated requirements. '.repeat(30) },
    { kind: 'message', id: 'm8901234', object_type: '', version: 0, sequence: 8, role: 'assistant',
      content: 'The previous decision applies to this work. '.repeat(30) },
  ],
}

async function run(t, { outcome = 'success', pages = [], resume = false } = {}) {
  const child = spawn(process.execPath, ['bin.mjs'], {
    cwd: new URL('..', import.meta.url), env: { PATH: process.env.PATH, LANG: 'C.UTF-8' },
    stdio: ['pipe', 'pipe', 'pipe'],
  })
  t.after(() => child.kill())
  const frames = []
  const writtenPages = []
  const send = frame => child.stdin.write(`${JSON.stringify(frame)}\n`)
  const lines = createInterface({ input: child.stdout })
  lines.on('line', line => {
    const frame = JSON.parse(line)
    frames.push(frame)
    if (frame.type === 'ready') {
      const last = pages.at(-1)
      send(resume ? { ...start, resume: true, checkpoint_through_seq: last.through_seq, checkpoint_sha256: last.sha256 } : start)
    } else if (frame.type === 'checkpoint') {
      writtenPages.push(frame)
      send({ type: 'checkpoint_ack', ...fence(frame), id: frame.id, ok: true, through_seq: frame.through_seq, sha256: frame.sha256 })
    } else if (frame.type === 'checkpoint_read') {
      const page = pages.find(item => item.from_seq === frame.from_seq)
      if (!page) {
        send({ type: 'checkpoint_page', ...fence(frame), id: frame.id, ok: false })
      } else {
        send({ type: 'checkpoint_page', ...fence(frame), id: frame.id, ok: true, from_seq: page.from_seq,
          through_seq: page.through_seq, payload: page.payload, sha256: page.sha256 })
      }
    } else if (frame.type === 'model') {
      if (outcome === 'cancel') {
        send({ type: 'cancel', ...fence(frame) })
      } else if (outcome === 'overflow' || outcome === 'failure') {
        send({ type: 'model_end', ...fence(frame), id: frame.id, ok: false,
          ...(outcome === 'overflow' ? { failure_code: 'CONTEXT_WINDOW_EXCEEDED' } : {}) })
      } else {
        const text = outcome === 'empty' ? '   ' : outcome === 'no-progress' ? 'unhelpful summary '.repeat(2500)
          : 'Original preference and early requirement were preserved.'
        const chunks = [
          { type: 'block-start', index: 0, blockType: 'text' },
          { type: 'text-delta', index: 0, text },
          { type: 'block-end', index: 0, block: { type: 'text', text } },
          { type: 'finish', reason: { kind: outcome === 'truncated' ? 'max-tokens' : 'stop' } },
        ]
        chunks.forEach((chunk, index) => send({ type: 'model_chunk', ...fence(frame), id: frame.id, index, chunk }))
        send({ type: 'model_end', ...fence(frame), id: frame.id, ok: true })
      }
    }
  })
  const [code] = await once(child, 'close')
  return { code, frames, pages: [...pages, ...writtenPages] }
}

function events(result) {
  return result.pages.flatMap(page => JSON.parse(page.payload).events)
}

test('a summary that does not reduce its source window fails before replacing durable history',
  { timeout: 15_000 }, async t => {
    const result = await run(t, { outcome: 'no-progress' })
    assert.equal(result.code, 1, JSON.stringify(result.frames.map(frame => frame.type)))
    const history = events(result)
    assert.ok(history.some(event => event.type === 'user/message' && JSON.stringify(event).includes('Preserve this original preference')))
    assert.equal(history.some(event => event.type === 'compaction/summary'), false)
    assert.equal(history.some(event => event.type === 'user/message' && event.surfaceOp?.op === 'replace'), false)
    assert.ok(history.some(event => event.type === 'compaction/end' && event.data.error?.includes('did not reduce its source window')))
  })

for (const outcome of ['empty', 'truncated', 'failure', 'overflow']) {
  test(`production compaction preserves source events after ${outcome} model output`, { timeout: 15_000 }, async t => {
    const result = await run(t, { outcome })
    assert.equal(result.code, 1, JSON.stringify(result.frames.map(frame => frame.type)))
    assert.equal(result.frames.some(frame => frame.type === 'coordinator_settled'), false)
    assert.equal(result.frames.filter(frame => frame.type === 'model').length, 1)
    const history = events(result)
    assert.ok(history.some(event => event.type === 'user/message' && JSON.stringify(event).includes('Preserve this original preference')))
    assert.equal(history.some(event => event.type === 'compaction/summary'), false)
    assert.equal(history.some(event => event.type === 'user/message' && event.surfaceOp?.op === 'replace'), false)
    assert.ok(history.some(event => event.type === 'compaction/end' && event.data.error))
  })
}

test('a failed durable compaction can be retried from original events, then resumed without another model call',
  { timeout: 20_000 }, async t => {
    const failed = await run(t, { outcome: 'empty' })
    const retried = await run(t, { pages: failed.pages, resume: true })
    assert.equal(retried.code, 0, JSON.stringify(retried.frames.map(frame => frame.type)))
    assert.equal(retried.frames.filter(frame => frame.type === 'model').length, 1)
    assert.ok(JSON.stringify(retried.frames.find(frame => frame.type === 'model').request).includes('Preserve this original preference'))
    assert.ok(retried.frames.some(frame => frame.type === 'coordinator_settled'))
    const history = events(retried)
    assert.ok(history.some(event => event.type === 'compaction/end' && event.data.error))
    assert.equal(history.filter(event => event.type === 'compaction/summary').length, 1)
    const resumed = await run(t, { pages: retried.pages, resume: true })
    assert.equal(resumed.code, 0, JSON.stringify(resumed.frames.map(frame => frame.type)))
    assert.equal(resumed.frames.some(frame => frame.type === 'model'), false)
    assert.equal(resumed.frames.find(frame => frame.type === 'coordinator_settled')?.result,
      retried.frames.find(frame => frame.type === 'coordinator_settled')?.result)
  })

test('cancelling a pending compaction does not settle or replace source events', { timeout: 15_000 }, async t => {
  const result = await run(t, { outcome: 'cancel' })
  assert.equal(result.code, 0, JSON.stringify(result.frames.map(frame => frame.type)))
  assert.ok(result.frames.some(frame => frame.type === 'model_cancel'))
  assert.ok(result.frames.some(frame => frame.type === 'coordinator_cancelled'))
  assert.equal(result.frames.some(frame => frame.type === 'coordinator_settled'), false)
  const history = events(result)
  assert.ok(history.some(event => event.type === 'user/message' && JSON.stringify(event).includes('Preserve this original preference')))
  assert.equal(history.some(event => event.type === 'compaction/summary'), false)
  assert.equal(history.some(event => event.type === 'user/message' && event.surfaceOp?.op === 'replace'), false)
})
