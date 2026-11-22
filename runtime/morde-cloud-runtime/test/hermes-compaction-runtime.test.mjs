import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { once } from 'node:events'
import { createInterface } from 'node:readline'
import test from 'node:test'

const identityKeys = ['task_id', 'owner_id', 'agent_id', 'generation', 'authorization_version', 'session_id']
const fence = frame => Object.fromEntries(identityKeys.map(key => [key, frame[key]]))

test('passes an 18 KB current message intact to the model within its context budget',
  { timeout: 15_000 }, async t => {
    const prefix = 'head-anchor\n'
    const middle = '\nmiddle-anchor\n'
    const suffix = '\ntail-anchor'
    const filler = 18 * 1024 - prefix.length - middle.length - suffix.length
    const left = Math.floor(filler / 2)
    const content = `${prefix}${'a'.repeat(left)}${middle}${'b'.repeat(filler - left)}${suffix}`
    assert.equal(Buffer.byteLength(content), 18 * 1024)
    const start = {
      type: 'start', version: 1, task_id: 'task_long_input', owner_id: 'owner_long_input',
      agent_id: 'agent_long_input', generation: 1, authorization_version: 1, session_id: 'session_long_input', task_revision: 1,
      model: 'test-model', tools: [], context_window_tokens: 32_768, max_tokens: 512,
      initial_message: { message_id: 'm1234567', content, attachment_ids: [] },
    }
    const child = spawn(process.execPath, ['bin.mjs'], {
      cwd: new URL('..', import.meta.url), env: { PATH: process.env.PATH, LANG: 'C.UTF-8' },
      stdio: ['pipe', 'pipe', 'pipe'],
    })
    t.after(() => child.kill())
    const frames = []
    const modelRequests = []
    const send = frame => child.stdin.write(`${JSON.stringify(frame)}\n`)
    const lines = createInterface({ input: child.stdout })
    lines.on('line', line => {
      const frame = JSON.parse(line)
      frames.push(frame)
      if (frame.type === 'ready') send(start)
      else if (frame.type === 'checkpoint') send({ type: 'checkpoint_ack', ...fence(frame), id: frame.id,
        ok: true, through_seq: frame.through_seq, sha256: frame.sha256 })
      else if (frame.type === 'model') {
        modelRequests.push(frame.request)
        const text = 'Received the complete current message.'
        const chunks = [
          { type: 'block-start', index: 0, blockType: 'text' },
          { type: 'text-delta', index: 0, text },
          { type: 'block-end', index: 0, block: { type: 'text', text } },
          { type: 'finish', reason: { kind: 'stop' } },
        ]
        chunks.forEach((chunk, index) => send({ type: 'model_chunk', ...fence(frame), id: frame.id, index, chunk }))
        send({ type: 'model_end', ...fence(frame), id: frame.id, ok: true })
      } else if (['settled', 'failed', 'protocol_error'].includes(frame.type)) child.stdin.end()
    })
    const [code] = await once(child, 'close')
    assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
    assert.equal(modelRequests.length, 1)
    const request = JSON.stringify(modelRequests[0].messages)
    assert.ok(request.includes(JSON.stringify(content).slice(1, -1)),
      'model request omitted or truncated the current message')
    for (const anchor of ['head-anchor', 'middle-anchor', 'tail-anchor']) {
      assert.ok(request.includes(anchor), `model request omitted ${anchor}`)
    }
    assert.ok(frames.some(frame => frame.type === 'settled'))
  })

test('long task conversation survives repeated pressure compaction and keeps its early requirement',
  { timeout: 15_000 }, async t => {
    const start = {
      type: 'start', version: 1, task_id: 'task_hermes_window', owner_id: 'owner_hermes_window',
      agent_id: 'agent_hermes_window', generation: 1, authorization_version: 1, session_id: 'session_hermes_window', task_revision: 1,
      model: 'test-model', tools: [], context_window_tokens: 4096, max_tokens: 512,
      initial_message: { message_id: 'm1234567', content: `head-anchor ${'history '.repeat(275)}`, attachment_ids: [] },
    }
    const child = spawn(process.execPath, ['bin.mjs'], {
      cwd: new URL('..', import.meta.url), env: { PATH: process.env.PATH, LANG: 'C.UTF-8' },
      stdio: ['pipe', 'pipe', 'pipe'],
    })
    t.after(() => child.kill())
    const frames = []
    const pages = []
    let continued = false
    const send = frame => child.stdin.write(`${JSON.stringify(frame)}\n`)
    const lines = createInterface({ input: child.stdout })
    lines.on('line', line => {
      const frame = JSON.parse(line)
      frames.push(frame)
      if (frame.type === 'ready') send(start)
      else if (frame.type === 'checkpoint') {
        pages.push(frame)
        send({ type: 'checkpoint_ack', ...fence(frame), id: frame.id, ok: true,
          through_seq: frame.through_seq, sha256: frame.sha256 })
      } else if (frame.type === 'model') {
        const summaryRequest = frame.request.max_tokens === 256
        if (!continued && !summaryRequest) {
          continued = true
          for (let index = 1; index <= 40; index++) {
            send({ type: 'message_delivery', version: 2, ...fence(frame),
              message_id: `m${String(7000000 + index).padStart(7, '0')}`,
              content: `followup-${index} ${'history '.repeat(275)}`, attachment_ids: [] })
          }
        }
        const request = JSON.stringify(frame.request.messages)
        const text = summaryRequest
          ? `Summary preserved the compacted history.${request.includes('head-anchor') ? ' Early requirement: head-anchor.' : ''}`
          : 'Continued after compaction.'
        const chunks = [
          { type: 'block-start', index: 0, blockType: 'text' },
          { type: 'text-delta', index: 0, text },
          { type: 'block-end', index: 0, block: { type: 'text', text } },
          { type: 'finish', reason: { kind: 'stop' } },
        ]
        chunks.forEach((chunk, index) => send({ type: 'model_chunk', ...fence(frame), id: frame.id, index, chunk }))
        send({ type: 'model_end', ...fence(frame), id: frame.id, ok: true })
      } else if (['settled', 'failed', 'protocol_error'].includes(frame.type)) child.stdin.end()
    })
    const [code] = await once(child, 'close')
    assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
    const modelFrames = frames.filter(frame => frame.type === 'model')
    const summaryFrame = modelFrames.find(frame => frame.request.max_tokens === 256)
    assert.ok(summaryFrame, `no DSH summary request in ${modelFrames.length} model calls`)
    assert.ok(modelFrames.filter(frame => frame.request.max_tokens === 256).length >= 2,
      'long task did not recover through repeated compaction')
    assert.ok(modelFrames.indexOf(summaryFrame) > 0)
    assert.notEqual(modelFrames.at(-1)?.request.max_tokens, 256)
    const summaryInput = JSON.stringify(summaryFrame.request.messages)
    assert.ok(summaryInput.includes('followup-'))
    assert.ok(!summaryInput.includes('head-anchor'))
    assert.ok(!summaryInput.includes('followup-3'))
    const events = pages.flatMap(page => JSON.parse(page.payload).events)
    const summaries = events.filter(event => event.type === 'compaction/summary')
    assert.ok(summaries.length >= 2)
    assert.ok(summaries.every(event => Array.isArray(event.data.shadowedSeqs) && event.data.shadowedSeqs.length > 0),
      'every summary must prove which source events it covered')
    assert.ok(events.some(event => event.type === 'compaction/end' && !event.data.error))
    assert.ok(events.some(event => event.type === 'user/message' && JSON.stringify(event).includes('head-anchor')))
    assert.ok(events.some(event => event.type === 'user/message' && JSON.stringify(event).includes('followup-7')))
    assert.ok(events.some(event => event.type === 'user/message' && JSON.stringify(event).includes('followup-40')))
    const finalRequest = JSON.stringify(modelFrames.at(-1)?.request.messages)
    const summaryHadAnchor = modelFrames.some(frame => frame.request.max_tokens === 256 &&
      JSON.stringify(frame.request.messages).includes('head-anchor'))
    assert.ok(finalRequest.includes('head-anchor'),
      `early requirement fell out of the active context; summaryHadAnchor=${summaryHadAnchor}; finalLength=${finalRequest.length}`)
    assert.ok(finalRequest.includes('followup-40'), 'latest follow-up did not reach the final model request')
    for (let index = 1; index <= 40; index++) {
      assert.equal(events.filter(event => event.type === 'user/message' && JSON.stringify(event).includes(`followup-${index} `)).length, 1,
        `follow-up ${index} was missing or duplicated in durable history`)
    }
    assert.ok(frames.some(frame => frame.type === 'settled'), 'long task did not settle successfully')
    assert.equal(frames.some(frame => frame.type === 'failed' || frame.type === 'protocol_error'), false)
  })
