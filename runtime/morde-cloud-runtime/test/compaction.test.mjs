import assert from 'node:assert/strict'
import test from 'node:test'
import { Context } from '@deepseek-ai/cordis'
import AgentRegistry from '@deepseek-ai/dsh-agent'
import AgentLoop from '@deepseek-ai/dsh-agent-loop'
import LlmRuntime, { LlmAdapter, createMessage, createToolResultMessage, createUserMessage, ToolCallId } from '@deepseek-ai/dsh-llm'
import SessionStore, { Session, SessionId } from '@deepseek-ai/dsh-session'
import SessionProjectionRegistry from '@deepseek-ai/dsh-session-projection'
import SystemPrompt from '@deepseek-ai/dsh-system-prompt'
import ToolResultPruner from '@deepseek-ai/dsh-compaction-tool-result-pruner'
import ToolRuntime from '@deepseek-ai/dsh-tools'
import { apply, inject, name } from '@deepseek-ai/dsh-tool-executor-native'
import TokenMeter from '@deepseek-ai/dsh-token-meter'
import { BasicCompactionEngine } from '../vendor/deepseek-harness/packages/compaction/compaction-basic/dist/index.js'

const model = 'synthetic-compaction-model'

class SyntheticAdapter extends LlmAdapter {
  constructor({ failOnRequest, waitOnRequest, contextWindow = 100_000 } = {}) {
    super()
    this.failOnRequest = failOnRequest
    this.waitOnRequest = waitOnRequest
    this.contextWindow = contextWindow
    this.requests = 0
    this.waiting = Promise.withResolvers()
  }

  async resolveModel(provider, id) {
    return { provider, id, name: id, context: { contextWindow: this.contextWindow } }
  }

  async *stream({ signal }) {
    this.requests += 1
    if (this.requests === this.waitOnRequest) {
      this.waiting.resolve()
      await new Promise((resolve, reject) => {
        const abort = () => reject(signal.reason)
        if (signal?.aborted) abort()
        else signal?.addEventListener('abort', abort, { once: true })
      })
    }
    if (this.requests === this.failOnRequest) throw new Error('synthetic summarizer failure')
    yield { type: 'block-start', index: 0, blockType: 'text' }
    yield {
      type: 'block-end', index: 0,
      block: { type: 'text', text: this.requests === 1 ? 'initial answer' : 'synthetic compacted summary' },
    }
    yield { type: 'finish', reason: { kind: 'stop' } }
  }
}

async function createHarness(adapter = new SyntheticAdapter(), compactionConfig = { auto: false }, initialContent = 'older conversation history '.repeat(60)) {
  const ctx = new Context()
  const nativeToolExecutor = { name, inject, apply }
  try {
    for (const plugin of [LlmRuntime, SessionStore, SessionProjectionRegistry, SystemPrompt, ToolRuntime, nativeToolExecutor, AgentRegistry]) {
      await ctx.plugin(plugin)
    }
    await ctx.plugin(AgentLoop, { agents: [] })
    await ctx.plugin(TokenMeter)
    await ctx.plugin(ToolResultPruner)
    await ctx.plugin(BasicCompactionEngine, compactionConfig)
    ctx.llm.registerAdapter([model], adapter)

    const agent = await ctx.agentLoop.create(SessionId('compact_test_session'), { provider: model, model })
    agent.followup(createUserMessage({
      content: [{ type: 'text', text: initialContent }],
      source: { kind: 'user' },
    }))
    await agent.whenIdle()
    return { ctx, agent, adapter }
  } catch (error) {
    await ctx.fiber.dispose()
    throw error
  }
}

test('automatically compacts task history before the next model request under token pressure', async () => {
  const adapter = new SyntheticAdapter({ contextWindow: 4096 })
  const { ctx, agent } = await createHarness(adapter, {
    auto: true,
    thresholdRatio: 0.8,
    headroomTokens: 128,
    retainTokens: 512,
    maxTokens: 256,
    maxOverflowRetries: 1,
  }, 'older conversation history '.repeat(300))
  try {
    agent.followup(createUserMessage({
      content: [{ type: 'text', text: 'recent conversation history '.repeat(300) }],
      source: { kind: 'user' },
    }))
    await agent.whenIdle()
    agent.followup(createUserMessage({
      content: [{ type: 'text', text: 'continue with the latest request' }],
      source: { kind: 'user' },
    }))
    await agent.whenIdle()
    const events = agent.session.snapshotEvents()
    assert.ok(events.some(event => event.type === 'compaction/summary'))
    assert.ok(events.some(event => event.type === 'user/message' && event.surfaceOp?.op === 'replace'))
    assert.equal(adapter.requests, 4)
  } finally {
    await ctx.fiber.dispose()
  }
})

test('DSH tool-result pruning retains the original log and replaces only the model projection', async () => {
  const ctx = new Context()
  await ctx.plugin(SessionProjectionRegistry)
  await ctx.plugin(TokenMeter)
  await ctx.plugin(ToolResultPruner)
  try {
    const session = Session.create(SessionId('tool-pruner-test'))
    const callId = ToolCallId('call_pruner_test')
    const originalText = 'Large tool output '.repeat(800)
    session.append('turn/start', { turn: 1 })
    session.append('step/start', { turn: 1, step: 1 })
    session.append('assistant/message', {
      stream: [], turn: 1, step: 1,
      message: createMessage({ role: 'assistant', content: [{ type: 'tool-call', id: callId, name: 'bash', arguments: '{}' }],
        source: { kind: 'model', provider: model, model } }),
    }, { surfaceOp: 'append' })
    const toolCall = session.append('tool/call', { turn: 1, step: 1, callId, name: 'bash', arguments: '{}' })
    const original = session.append('tool/result', {
      turn: 1, step: 1,
      message: createToolResultMessage({ callId, content: [{ type: 'text', text: originalText }], isError: false }),
    }, { surfaceOp: 'append', sourceEventSeqs: [toolCall.seq] })
    session.append('step/end', { turn: 1, step: 1 })
    session.append('turn/end', { turn: 1, reason: { kind: 'completed' } })

    const pruned = ctx.toolResultPruner.pruneSession(session)
    const events = session.snapshotEvents()
    const replacement = events.at(-1)
    assert.equal(pruned.pruned.length, 1)
    assert.equal(events.at(-2).type, 'compaction/prune')
    assert.equal(replacement?.type, 'tool/result')
    assert.deepEqual(replacement?.surfaceOp, { op: 'replace', startSeq: original.seq, endSeq: original.seq })
    assert.equal(session.eventAt(original.seq).data.message.content[0].text, originalText)
    assert.ok(session.deriveEventMessage(replacement).content[0].text.length < originalText.length)
    assert.equal(session.surface.nodes.includes(original.seq), false)
    assert.equal(session.surface.nodes.includes(replacement.seq), true)
  } finally {
    await ctx.fiber.dispose()
  }
})

test('runs DSH manual compaction over an idle agent and preserves the append-only history', async () => {
  const { ctx, agent } = await createHarness()
  try {
    assert.equal(ctx.compaction instanceof BasicCompactionEngine, true)
    assert.equal(typeof ctx.tokenMeter.measure, 'function')
    const result = await ctx.compaction.compactNow(agent, new AbortController().signal)
    const events = agent.session.snapshotEvents()
    const compactionEvents = events.filter(event => event.type.startsWith('compaction/'))

    assert.ok(result)
    assert.deepEqual(compactionEvents.map(event => event.type), [
      'compaction/start', 'compaction/summary', 'compaction/end',
    ])
    assert.equal(events.some(event => event.type === 'user/message' && event.data.content.some(block =>
      block.type === 'text' && block.text.includes('older conversation history'))), true)
    assert.ok(JSON.stringify(compactionEvents[1]).includes('synthetic compacted summary'))
    assert.ok(agent.session.deriveMessages().some(message => message.content.some(block =>
      block.type === 'text' && block.text === 'synthetic compacted summary')))
  } finally {
    await ctx.fiber.dispose()
  }
})

test('records a failed summary without changing the session projection', async () => {
  const { ctx, agent } = await createHarness(new SyntheticAdapter({ failOnRequest: 2 }))
  try {
    const before = agent.session.deriveMessages()
    await assert.rejects(ctx.compaction.compactNow(agent, new AbortController().signal), error => {
      assert.equal(error.code, 'summary')
      assert.match(error.cause?.message, /synthetic summarizer failure/)
      return true
    })
    const events = agent.session.snapshotEvents().filter(event => event.type.startsWith('compaction/'))
    assert.deepEqual(events.map(event => event.type), ['compaction/start', 'compaction/end'])
    assert.ok(events[1].data.error)
    assert.deepEqual(agent.session.deriveMessages(), before)
  } finally {
    await ctx.fiber.dispose()
  }
})

test('cancels a summary in flight before committing its projection', async () => {
  const adapter = new SyntheticAdapter({ waitOnRequest: 2 })
  const { ctx, agent } = await createHarness(adapter)
  try {
    const before = agent.session.deriveMessages()
    const controller = new AbortController()
    const pending = ctx.compaction.compactNow(agent, controller.signal)
    await adapter.waiting.promise
    controller.abort(new Error('synthetic compaction cancelled'))
    await assert.rejects(pending, /synthetic compaction cancelled/)
    const events = agent.session.snapshotEvents().filter(event => event.type.startsWith('compaction/'))
    assert.deepEqual(events.map(event => event.type), ['compaction/start', 'compaction/end'])
    assert.ok(events[1].data.error)
    assert.deepEqual(agent.session.deriveMessages(), before)
  } finally {
    await ctx.fiber.dispose()
  }
})
