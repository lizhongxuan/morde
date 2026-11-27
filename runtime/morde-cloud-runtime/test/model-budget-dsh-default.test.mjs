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

const model = 'synthetic-budget-model'

class BudgetProbeAdapter extends LlmAdapter {
  constructor({ barrierSize = 1 } = {}) {
    super()
    this.plannedCaps = new WeakMap()
    this.barrierSize = barrierSize
    this.resolversWaiting = []
    this.requests = []
  }

  async resolveModel(provider, id, signal) {
    if (this.barrierSize > 1) {
      await new Promise(resolve => {
        this.resolversWaiting.push(resolve)
        if (this.resolversWaiting.length === this.barrierSize) {
          for (const release of this.resolversWaiting.splice(0)) release()
        }
      })
    }
    const defaultMaxTokens = this.plannedCaps.get(signal)
    if (!Number.isSafeInteger(defaultMaxTokens) || defaultMaxTokens <= 0) {
      throw new Error('per-step output budget plan is missing')
    }
    return {
      provider,
      id,
      name: id,
      context: { contextWindow: 32_768 },
      defaultMaxTokens,
    }
  }

  async *stream(options) {
    this.requests.push({ sessionId: options.sessionId, maxTokens: options.maxTokens })
    yield { type: 'block-start', index: 0, blockType: 'text' }
    yield { type: 'block-end', index: 0, block: { type: 'text', text: 'ok' } }
    yield { type: 'finish', reason: { kind: 'stop' } }
  }
}

async function createProbeContext(options) {
  const ctx = new Context()
  const nativeToolExecutor = { name, inject, apply }
  for (const plugin of [LlmRuntime, SessionStore, SessionProjectionRegistry, SystemPrompt, ToolRuntime, nativeToolExecutor, AgentRegistry]) {
    await ctx.plugin(plugin)
  }
  await ctx.plugin(AgentLoop, { agents: [] })

  const adapter = new BudgetProbeAdapter(options)
  ctx.llm.registerAdapter([model], adapter)

  return { ctx, adapter }
}

async function createProbeAgent(ctx, adapter, { id, maxTokens, stepCaps }) {
  const agent = await ctx.agentLoop.create(SessionId(`budget_probe_${id}`), {
    provider: model,
    model,
    ...(maxTokens === undefined ? {} : { maxTokens }),
  })
  const pendingCaps = [...stepCaps]
  agent.ctx.on('agent/pre-step', async ({ signal }, next) => {
    const cap = pendingCaps.shift()
    if (!Number.isSafeInteger(cap) || cap <= 0) throw new Error('probe has no output cap for this step')
    adapter.plannedCaps.set(signal, cap)
    return next()
  }, { prepend: true })
  return agent
}

function ask(agent, text) {
  agent.followup(createUserMessage({
    content: [{ type: 'text', text }],
    source: { kind: 'user' },
  }))
}

test('DSH re-resolves adapter default maxTokens between followups when AgentOptions has no explicit cap', async () => {
  const { ctx, adapter } = await createProbeContext()
  const agent = await createProbeAgent(ctx, adapter, { id: 'dynamic', stepCaps: [512, 1_024] })
  try {
    ask(agent, 'first')
    await agent.whenIdle()
    const firstHeader = agent.session.requestHeader()

    ask(agent, 'second')
    await agent.whenIdle()
    const secondHeader = agent.session.requestHeader()

    assert.deepEqual(adapter.requests.map(request => request.maxTokens), [512, 1_024])
    assert.deepEqual([firstHeader.config.maxTokens, secondHeader.config.maxTokens], [512, 1_024])
    assert.equal(firstHeader.adapterDefaults.maxTokens, true)
    assert.equal(secondHeader.adapterDefaults.maxTokens, true)
  } finally {
    await ctx.fiber.dispose()
  }
})

test('DSH preserves an explicit AgentOptions maxTokens cap when adapter defaults change', async () => {
  const { ctx, adapter } = await createProbeContext()
  const agent = await createProbeAgent(ctx, adapter, { id: 'explicit', maxTokens: 2_048, stepCaps: [512, 1_024] })
  try {
    ask(agent, 'first')
    await agent.whenIdle()
    const firstHeader = agent.session.requestHeader()

    ask(agent, 'second')
    await agent.whenIdle()
    const secondHeader = agent.session.requestHeader()

    assert.deepEqual(adapter.requests.map(request => request.maxTokens), [2_048, 2_048])
    assert.deepEqual([firstHeader.config.maxTokens, secondHeader.config.maxTokens], [2_048, 2_048])
    assert.equal(firstHeader.adapterDefaults?.maxTokens, undefined)
    assert.equal(secondHeader.adapterDefaults?.maxTokens, undefined)
  } finally {
    await ctx.fiber.dispose()
  }
})

test('DSH signal-keyed adapter defaults do not cross concurrent same-model Agents', async () => {
  const { ctx, adapter } = await createProbeContext({ barrierSize: 2 })
  const first = await createProbeAgent(ctx, adapter, { id: 'child-a', stepCaps: [768] })
  const second = await createProbeAgent(ctx, adapter, { id: 'child-b', stepCaps: [3_072] })
  try {
    ask(first, 'first child')
    ask(second, 'second child')
    await Promise.all([first.whenIdle(), second.whenIdle()])

    const bySession = Object.fromEntries(adapter.requests.map(request => [request.sessionId, request.maxTokens]))
    assert.equal(bySession[first.id], 768)
    assert.equal(bySession[second.id], 3_072)
    assert.equal(first.session.requestHeader().config.maxTokens, 768)
    assert.equal(second.session.requestHeader().config.maxTokens, 3_072)
  } finally {
    await ctx.fiber.dispose()
  }
})
