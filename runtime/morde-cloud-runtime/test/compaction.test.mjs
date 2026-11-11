import assert from 'node:assert/strict'
import test from 'node:test'
import { Context } from '@deepseek-ai/cordis'
import AgentRegistry from '@deepseek-ai/dsh-agent'
import AgentLoop from '@deepseek-ai/dsh-agent-loop'
import LlmRuntime, { LlmAdapter, createUserMessage } from '@deepseek-ai/dsh-llm'
import SessionStore, { SessionId } from '@deepseek-ai/dsh-session'
import SessionProjectionRegistry from '@deepseek-ai/dsh-session-projection'
import SystemPrompt from '@deepseek-ai/dsh-system-prompt'
import ToolRuntime from '@deepseek-ai/dsh-tools'
import { apply, inject, name } from '@deepseek-ai/dsh-tool-executor-native'
import TokenMeter from '@deepseek-ai/dsh-token-meter'
import { BasicCompactionEngine } from '../vendor/deepseek-harness/packages/compaction/compaction-basic/dist/index.js'

const model = 'synthetic-compaction-model'

class SyntheticAdapter extends LlmAdapter {
  constructor({ failOnRequest, waitOnRequest } = {}) {
    super()
    this.failOnRequest = failOnRequest
    this.waitOnRequest = waitOnRequest
    this.requests = 0
    this.waiting = Promise.withResolvers()
  }

  async resolveModel(provider, id) {
    return { provider, id, name: id, context: { contextWindow: 100_000 } }
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

async function createHarness(adapter = new SyntheticAdapter()) {
  const ctx = new Context()
  const nativeToolExecutor = { name, inject, apply }
  try {
    for (const plugin of [LlmRuntime, SessionStore, SessionProjectionRegistry, SystemPrompt, ToolRuntime, nativeToolExecutor, AgentRegistry]) {
      await ctx.plugin(plugin)
    }
    await ctx.plugin(AgentLoop, { agents: [] })
    await ctx.plugin(TokenMeter)
    await ctx.plugin(BasicCompactionEngine, { auto: false })
    ctx.llm.registerAdapter([model], adapter)

    const agent = await ctx.agentLoop.create(SessionId('compact_test_session'), { provider: model, model })
    agent.followup(createUserMessage({
      content: [{ type: 'text', text: 'older conversation history '.repeat(60) }],
      source: { kind: 'user' },
    }))
    await agent.whenIdle()
    return { ctx, agent, adapter }
  } catch (error) {
    await ctx.fiber.dispose()
    throw error
  }
}

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
