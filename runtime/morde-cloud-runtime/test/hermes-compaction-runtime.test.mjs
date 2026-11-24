import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { once } from 'node:events'
import { createInterface } from 'node:readline'
import test from 'node:test'
import { estimateMessage, estimateToolsTokens } from '@deepseek-ai/dsh-token-meter/estimate'
import { resolveCompactionContextWindow } from '../src/engine.mjs'

const identityKeys = ['task_id', 'owner_id', 'agent_id', 'generation', 'authorization_version', 'session_id']
const fence = frame => Object.fromEntries(identityKeys.map(key => [key, frame[key]]))

const authorizedSourceTool = {
  name: 'authorized_source_read',
  description: 'Read a source explicitly authorized for this task.',
  parameters: {
    type: 'object',
    properties: {
      source_ids: { type: 'array', items: { type: 'string' }, uniqueItems: true },
    },
    required: ['source_ids'],
    additionalProperties: false,
  },
}

function buildEstimatedCorpus(targetTokens, scaleLabel) {
  const count = Math.max(8, Math.ceil(targetTokens / 5_000))
  const middleIndex = Math.floor((count - 1) / 2)
  const anchorIndexes = new Map([[0, 'first'], [middleIndex, 'middle'], [count - 1, 'last']])
  const quotient = Math.floor(targetTokens / count)
  const remainder = targetTokens % count
  const corpus = Array.from({ length: count }, (_, index) => {
    const sourceID = `source-${String(index + 1).padStart(3, '0')}`
    const anchorKind = anchorIndexes.get(index)
    const anchor = anchorKind ? `anchor-${scaleLabel}-${anchorKind}` : undefined
    const prefix = `${sourceID}${anchor ? ` ${anchor}` : ''} `
    const messageTokens = quotient + (index < remainder ? 1 : 0)
    const targetTextCharacters = (messageTokens - 8) * 4
    assert.ok(targetTextCharacters > prefix.length, `corpus chunk ${index} is too small`)
    const content = `${prefix}${'x'.repeat(targetTextCharacters - prefix.length)}`
    const message = { role: 'user', content: [{ type: 'text', text: content }] }
    assert.equal(estimateMessage(message), messageTokens,
      `DSH TokenMeter estimate for ${sourceID} did not match its allocated share`)
    assert.ok(Buffer.byteLength(content, 'utf8') < 128 * 1024,
      `${sourceID} exceeds the 128 KiB per-message hard limit`)
    return {
      sourceID,
      ...(anchor ? { anchor } : {}),
      messageID: `m${String(7000000 + index).padStart(7, '0')}`,
      content,
    }
  })
  assert.equal(corpus.reduce((sum, source) => sum + estimateMessage({
    role: 'user', content: [{ type: 'text', text: source.content }],
  }), 0), targetTokens, `synthetic corpus must measure to its ${targetTokens} DSH TokenMeter estimate`)
  return corpus
}

function requestTokenMeterEstimate(request) {
  return request.messages.reduce((sum, message) => sum + estimateMessage(message), 0) +
    estimateToolsTokens({ tools: request.tools ?? [] })
}

function checkpointEvents(pages) {
  return pages.flatMap(page => JSON.parse(page.payload).events)
}

function makeSummaryText(request, corpus) {
  const serialized = JSON.stringify(request.messages)
  const sourceIDs = corpus.map(source => source.sourceID).filter(sourceID => serialized.includes(sourceID))
  const anchors = corpus.filter(source => source.anchor && serialized.includes(source.anchor)).map(source => source.anchor)
  return `Preserved authorized source IDs: ${sourceIDs.join(', ')}. Preserved anchors: ${anchors.join(', ')}.`
}

function assertBalancedToolMessages(request) {
  const calls = new Map()
  const results = new Map()
  for (const message of request.messages) {
    for (const block of message.content ?? []) {
      if (block.type === 'tool-call') calls.set(block.id, (calls.get(block.id) ?? 0) + 1)
    }
    if (message.role === 'tool') results.set(message.toolCallId, (results.get(message.toolCallId) ?? 0) + 1)
  }
  for (const [id, count] of calls) assert.equal(count, 1, `tool call ${id} was duplicated in a model request`)
  for (const [id, count] of results) {
    assert.equal(count, 1, `tool result ${id} was duplicated in a model request`)
    assert.equal(calls.get(id), 1, `tool result ${id} was separated from its call in a model request`)
  }
}

test('passes a current message above 8K DSH-estimated tokens intact within its context budget',
  { timeout: 15_000 }, async t => {
    const prefix = 'head-anchor\n'
    const middle = '\nmiddle-anchor\n'
    const suffix = '\ntail-anchor'
    const filler = 36 * 1024 - prefix.length - middle.length - suffix.length
    const left = Math.floor(filler / 2)
    const content = `${prefix}${'a'.repeat(left)}${middle}${'b'.repeat(filler - left)}${suffix}`
    assert.equal(Buffer.byteLength(content), 36 * 1024)
    const estimatedMessageTokens = estimateMessage({ role: 'user', content: [{ type: 'text', text: content }] })
    assert.ok(estimatedMessageTokens > 8_192,
      `single current message must exceed 8K DSH TokenMeter estimate; got ${estimatedMessageTokens}`)
    const start = {
      type: 'start', version: 1, task_id: 'task_long_input', owner_id: 'owner_long_input',
      agent_id: 'agent_long_input', generation: 1, authorization_version: 1, session_id: 'session_long_input', task_revision: 1,
      model: 'test-model', tools: [], context_window_tokens: 16_384, max_tokens: 512,
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
    assert.ok(requestTokenMeterEstimate(modelRequests[0]) <= start.context_window_tokens - start.max_tokens,
      'complete model input plus reserved output must fit the declared context window')
    const request = JSON.stringify(modelRequests[0].messages)
    assert.ok(request.includes(JSON.stringify(content).slice(1, -1)),
      'model request omitted or truncated the current message')
    for (const anchor of ['head-anchor', 'middle-anchor', 'tail-anchor']) {
      assert.ok(request.includes(anchor), `model request omitted ${anchor}`)
    }
    assert.ok(frames.some(frame => frame.type === 'settled'))
  })

test('accepts a provider-resolved window without a frame window and fences it to the compaction plan', () => {
  assert.equal(resolveCompactionContextWindow(8192, undefined, 8192), 8192)
  for (const [providerWindow, frameWindow, plannedWindow] of [
    [undefined, undefined, 8192],
    [Number.MAX_SAFE_INTEGER + 1, undefined, Number.MAX_SAFE_INTEGER + 1],
    [8192, undefined, 4096],
    [8192, 4096, 8192],
  ]) {
    assert.throws(() => resolveCompactionContextWindow(providerWindow, frameWindow, plannedWindow), error =>
      error.code === 'MODEL_CONTEXT_UNAVAILABLE')
  }
})

test('provider overflow with an unknown context window fails without retrying or replacing source history',
  { timeout: 15_000 }, async t => {
    const start = {
      type: 'start', version: 1, task_id: 'task_unknown_window', owner_id: 'owner_unknown_window',
      agent_id: 'agent_unknown_window', generation: 1, authorization_version: 1,
      session_id: 'session_unknown_window', task_revision: 1, model: 'test-model', tools: [], max_tokens: 512,
      initial_message: { message_id: 'm1234567', content: 'Keep this task brief in the original session.', attachment_ids: [] },
    }
    const child = spawn(process.execPath, ['bin.mjs'], {
      cwd: new URL('..', import.meta.url), env: { PATH: process.env.PATH, LANG: 'C.UTF-8' },
      stdio: ['pipe', 'pipe', 'pipe'],
    })
    t.after(() => child.kill())
    const frames = []
    const pages = []
    let modelCalls = 0
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
        modelCalls++
        send({ type: 'model_end', ...fence(frame), id: frame.id, ok: false,
          failure_code: 'CONTEXT_WINDOW_EXCEEDED' })
      } else if (['settled', 'failed', 'protocol_error'].includes(frame.type)) child.stdin.end()
    })
    const [code] = await once(child, 'close')
    assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
    assert.equal(modelCalls, 1, 'unknown capacity must not trigger an unbounded overflow retry')
    const sourceEvents = pages.flatMap(page => JSON.parse(page.payload).events)
    assert.ok(sourceEvents.some(event => event.type === 'user/message' &&
      JSON.stringify(event).includes('Keep this task brief in the original session.')))
    assert.equal(sourceEvents.some(event => event.type === 'compaction/summary'), false,
      'unknown capacity must not produce a summary with an assumed budget')
    assert.ok(frames.some(frame => frame.type === 'settled' && frame.state === 'failed') ||
      frames.some(frame => frame.type === 'failed'), 'provider overflow must end as an explicit runtime failure')
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
    for (const summary of modelFrames.filter(frame => frame.request.max_tokens === 256)) {
      assert.ok(!JSON.stringify(summary.request.messages).includes('head-anchor'),
        'repeated compaction must keep the opening Task brief outside every summary range')
    }
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

for (const { targetTokens, contextWindow, scaleLabel } of [
  { targetTokens: 4_000, contextWindow: 4_096, scaleLabel: '4k' },
  { targetTokens: 8_000, contextWindow: 8_192, scaleLabel: '8k' },
  { targetTokens: 100_000, contextWindow: 65_536, scaleLabel: '100k' },
  { targetTokens: 200_000, contextWindow: 131_072, scaleLabel: '200k' },
]) {
  test(`keeps a ${scaleLabel} DSH TokenMeter-estimated corpus within model and restore budgets`,
    { timeout: 90_000 }, async t => {
      const corpus = buildEstimatedCorpus(targetTokens, scaleLabel)
      const anchors = corpus.filter(source => source.anchor)
      const authorizedSourceIDs = anchors.map(source => source.sourceID)
      const start = {
        type: 'start', version: 1,
        task_id: `task_scale_${scaleLabel}`, owner_id: `owner_scale_${scaleLabel}`,
        agent_id: `agent_scale_${scaleLabel}`, generation: 1, authorization_version: 1,
        session_id: `session_scale_${scaleLabel}`, task_revision: 1,
        model: 'test-model', tools: [authorizedSourceTool], context_window_tokens: contextWindow, max_tokens: 512,
        initial_message: { message_id: corpus[0].messageID, content: corpus[0].content, attachment_ids: [] },
      }
      const child = spawn(process.execPath, ['bin.mjs'], {
        cwd: new URL('..', import.meta.url), env: { PATH: process.env.PATH, LANG: 'C.UTF-8' },
        stdio: ['pipe', 'pipe', 'pipe'],
      })
      t.after(() => child.kill())
      const frames = []
      const pages = []
      const modelRequests = []
      let nextDeliveryIndex = 1
      let authorizedLookupIssued = false
      let authorizedLookupOutput = ''
      let toolCalls = 0
      let finalRequest
      const send = frame => child.stdin.write(`${JSON.stringify(frame)}\n`)
      const replyWithText = (frame, text) => {
        const chunks = [
          { type: 'block-start', index: 0, blockType: 'text' },
          { type: 'text-delta', index: 0, text },
          { type: 'block-end', index: 0, block: { type: 'text', text } },
          { type: 'finish', reason: { kind: 'stop' } },
        ]
        chunks.forEach((chunk, index) => send({ type: 'model_chunk', ...fence(frame), id: frame.id, index, chunk }))
        send({ type: 'model_end', ...fence(frame), id: frame.id, ok: true })
      }
      const lines = createInterface({ input: child.stdout })
      lines.on('line', line => {
        const frame = JSON.parse(line)
        frames.push(frame)
        if (frame.type === 'ready') send(start)
        else if (frame.type === 'checkpoint') {
          pages.push({ from_seq: frame.from_seq, through_seq: frame.through_seq, payload: frame.payload, sha256: frame.sha256 })
          send({ type: 'checkpoint_ack', ...fence(frame), id: frame.id, ok: true,
            through_seq: frame.through_seq, sha256: frame.sha256 })
        } else if (frame.type === 'model') {
          const request = frame.request
          const reservation = request.max_tokens
          assert.ok(Number.isSafeInteger(reservation) && reservation > 0,
            'every provider request must reserve an explicit output budget')
          const estimatedInputTokens = requestTokenMeterEstimate(request)
          assert.ok(estimatedInputTokens <= contextWindow - reservation,
            `DSH TokenMeter estimate ${estimatedInputTokens} exceeds ${contextWindow} context minus ${reservation} output reservation`)
          assertBalancedToolMessages(request)
          modelRequests.push({ request, estimatedInputTokens, reservation })

          if (request.tools.length === 0) {
            replyWithText(frame, makeSummaryText(request, corpus))
          } else if (nextDeliveryIndex < corpus.length) {
            const nextSource = corpus[nextDeliveryIndex++]
            send({ type: 'message_delivery', version: 2, ...fence(frame),
              message_id: nextSource.messageID, content: nextSource.content, attachment_ids: [] })
            replyWithText(frame, 'Re-evaluate the newly delivered source before continuing.')
          } else if (!authorizedLookupIssued) {
            authorizedLookupIssued = true
            const argumentsText = JSON.stringify({ source_ids: authorizedSourceIDs })
            const chunks = [
              { type: 'tool-call-delta', index: 0, id: `call_source_${scaleLabel}`,
                name: authorizedSourceTool.name, argumentsDelta: argumentsText },
              { type: 'finish', reason: { kind: 'tool-calls' } },
            ]
            chunks.forEach((chunk, index) => send({ type: 'model_chunk', ...fence(frame), id: frame.id, index, chunk }))
            send({ type: 'model_end', ...fence(frame), id: frame.id, ok: true })
          } else {
            finalRequest = request
            replyWithText(frame, `Recovered ${anchors.map(source => `${source.sourceID}:${source.anchor}`).join(', ')}.`)
          }
        } else if (frame.type === 'tool_call') {
          toolCalls++
          assert.equal(frame.name, authorizedSourceTool.name)
          assert.deepEqual(frame.arguments.source_ids, authorizedSourceIDs)
          const sources = frame.arguments.source_ids.map(sourceID => {
            const source = corpus.find(value => value.sourceID === sourceID)
            assert.ok(source?.anchor, `authorized source ${sourceID} must resolve to an anchor`)
            return { source_id: source.sourceID, anchor: source.anchor }
          })
          authorizedLookupOutput = JSON.stringify({ sources })
          send({ type: 'tool_result', ...fence(frame), id: frame.id, call_id: frame.call_id,
            status: 'succeeded', output: authorizedLookupOutput })
        } else if (['settled', 'failed', 'protocol_error'].includes(frame.type)) child.stdin.end()
      })
      const [code] = await once(child, 'close')
      assert.equal(code, 0, `frames: ${frames.map(frame => frame.type).join(', ')}`)
      assert.ok(frames.some(frame => frame.type === 'settled'), 'synthetic task did not settle')
      assert.equal(frames.some(frame => frame.type === 'failed' || frame.type === 'protocol_error'), false)
      assert.equal(nextDeliveryIndex, corpus.length, 'every synthetic source message must be admitted')
      assert.equal(toolCalls, 1, 'authorized source retrieval must execute once')
      assert.ok(finalRequest, 'task must make a final model request after authorized retrieval')
      assert.equal(modelRequests.length >= 2, true)

      const measuredInputTotals = corpus.reduce((sum, source) => sum + estimateMessage({
        role: 'user', content: [{ type: 'text', text: source.content }],
      }), 0)
      assert.equal(measuredInputTotals, targetTokens)
      assert.ok(pages.length > 0, 'runtime did not persist a checkpoint')
      let expectedFromSeq = 0
      for (const page of pages) {
        assert.equal(page.from_seq, expectedFromSeq, 'checkpoint pages must remain contiguous and unique')
        assert.ok(Buffer.byteLength(page.payload, 'utf8') <= 1024 * 1024,
          'a synthetic checkpoint page exceeded the existing per-page limit')
        expectedFromSeq = page.through_seq + 1
      }
      const restoredBytes = pages.reduce((sum, page) => sum + Buffer.byteLength(page.payload, 'utf8'), 0)
      assert.ok(restoredBytes <= 8 * 1024 * 1024,
        `checkpoint restore estimate ${restoredBytes} bytes exceeds the existing 8 MiB hard limit`)
      const events = checkpointEvents(pages)
      assert.ok(events.length <= 100_000,
        `checkpoint event count ${events.length} exceeds the existing 100,000-event hard limit`)
      const summaries = events.filter(event => event.type === 'compaction/summary')
      assert.ok(summaries.length > 0, 'synthetic cumulative scale must exercise compaction')
      assert.ok(summaries.every(event => Array.isArray(event.data.shadowedSeqs) && event.data.shadowedSeqs.length > 0),
        'each DSH summary must retain source sequence provenance')

      const sourceEvents = events.filter(event => event.type === 'user/message')
      for (const source of corpus) {
        const matching = sourceEvents.filter(event => event.data.id === source.messageID)
        assert.equal(matching.length, 1, `${source.sourceID} was not durably recorded exactly once`)
        assert.ok(JSON.stringify(matching[0]).includes(source.sourceID), `${source.sourceID} was lost from its source event`)
      }
      for (const source of anchors) {
        assert.ok(authorizedLookupOutput.includes(source.sourceID) && authorizedLookupOutput.includes(source.anchor),
          `authorized retrieval omitted ${source.sourceID} / ${source.anchor}`)
      }
      const finalRequestText = JSON.stringify(finalRequest.messages)
      for (const source of anchors) {
        assert.ok(finalRequestText.includes(source.sourceID) && finalRequestText.includes(source.anchor),
          `the final model request could not recover ${source.sourceID} / ${source.anchor}: ${finalRequestText.slice(-2_000)}`)
      }
      const toolCallEvents = events.filter(event => event.type === 'tool/call' && event.data.callId === `call_source_${scaleLabel}`)
      const toolResultEvents = events.filter(event => event.type === 'tool/result' &&
        event.data.message?.toolCallId === `call_source_${scaleLabel}`)
      assert.equal(toolCallEvents.length, 1, 'durable tool call was duplicated or lost')
      assert.equal(toolResultEvents.length, 1, 'durable tool result was duplicated or lost')
      assert.ok(anchors.every(source => JSON.stringify(toolResultEvents[0]).includes(source.sourceID) &&
        JSON.stringify(toolResultEvents[0]).includes(source.anchor)),
      'the checkpointed authorized retrieval result must retain every first/middle/last anchor and source ID')

      const tail = pages.at(-1)
      const { initial_message: _initialMessage, ...resumeBase } = start
      const resumeStart = { ...resumeBase, agent_id: `agent_scale_restart_${scaleLabel}`, generation: 2,
        resume: true, checkpoint_through_seq: tail.through_seq, checkpoint_sha256: tail.sha256 }
      const resumed = spawn(process.execPath, ['bin.mjs'], {
        cwd: new URL('..', import.meta.url), env: { PATH: process.env.PATH, LANG: 'C.UTF-8' },
        stdio: ['pipe', 'pipe', 'pipe'],
      })
      t.after(() => resumed.kill())
      const resumedFrames = []
      const resumedCheckpoints = []
      let recoveredToolCalls = 0
      const resumeSend = frame => resumed.stdin.write(`${JSON.stringify(frame)}\n`)
      const resumedLines = createInterface({ input: resumed.stdout })
      resumedLines.on('line', line => {
        const frame = JSON.parse(line)
        resumedFrames.push(frame)
        if (frame.type === 'ready') resumeSend(resumeStart)
        else if (frame.type === 'checkpoint_read') {
          const page = pages.find(value => value.from_seq === frame.from_seq)
          assert.ok(page, `restart requested missing checkpoint page from ${frame.from_seq}`)
          resumeSend({ type: 'checkpoint_page', ...fence(frame), id: frame.id, ok: true,
            from_seq: page.from_seq, through_seq: page.through_seq, payload: page.payload, sha256: page.sha256 })
        } else if (frame.type === 'checkpoint') {
          resumedCheckpoints.push({ from_seq: frame.from_seq, through_seq: frame.through_seq,
            payload: frame.payload, sha256: frame.sha256 })
          resumeSend({ type: 'checkpoint_ack', ...fence(frame), id: frame.id, ok: true,
            through_seq: frame.through_seq, sha256: frame.sha256 })
        } else if (frame.type === 'tool_call') {
          recoveredToolCalls++
          assert.equal(frame.name, authorizedSourceTool.name)
          assert.deepEqual(frame.arguments.source_ids, authorizedSourceIDs)
          resumeSend({ type: 'tool_result', ...fence(frame), id: frame.id, call_id: frame.call_id,
            status: 'succeeded', output: authorizedLookupOutput })
        } else if (frame.type === 'model') {
          assert.fail('completed scale corpus should not replay a model call after restart')
        } else if (['settled', 'failed', 'protocol_error'].includes(frame.type)) resumed.stdin.end()
      })
      const [resumeCode] = await once(resumed, 'close')
      assert.equal(resumeCode, 0, `restart frames: ${resumedFrames.map(frame => frame.type).join(', ')}`)
      assert.equal(resumedFrames.some(frame => frame.type === 'model'), false,
        'restart replayed an already completed model operation')
      assert.equal(resumedFrames.some(frame => frame.type === 'failed' || frame.type === 'protocol_error'), false)
      assert.equal(recoveredToolCalls, 1,
        'restart should reconcile the authorized read result against its idempotent host ledger exactly once')
      const resumedEvents = resumedCheckpoints.flatMap(page => JSON.parse(page.payload).events)
      assert.ok(resumedEvents.every(event => event.type === 'session/end-seed'),
        `restart appended unexpected source or tool events: ${JSON.stringify(resumedEvents.map(event => event.type))}`)
      assert.equal(resumedFrames.find(frame => frame.type === 'settled')?.state, 'completed_candidate')
      assert.ok(frames.every(frame => frame.type !== 'settled' || frame.state === 'completed_candidate'),
        'the original process must finish as a completed candidate before restart')
    })
}
