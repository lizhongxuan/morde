import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const patchScript = fileURLToPath(new URL('../scripts/apply-dsh-request-budget-hook.mjs', import.meta.url))
const patchRun = spawnSync(process.execPath, [patchScript], { encoding: 'utf8' })
if (patchRun.status !== 0) {
  throw new Error(`DSH request-budget patch failed: ${patchRun.stderr || patchRun.stdout}`)
}

const [{ Context }, { default: AgentRegistry }, { default: AgentLoop }, llm, { default: SessionStore, SessionId }, { default: SessionProjectionRegistry }, { default: SystemPrompt }, { default: ToolRuntime }, nativeExecutor] = await Promise.all([
  import('@deepseek-ai/cordis'),
  import('@deepseek-ai/dsh-agent'),
  import('@deepseek-ai/dsh-agent-loop'),
  import('@deepseek-ai/dsh-llm'),
  import('@deepseek-ai/dsh-session'),
  import('@deepseek-ai/dsh-session-projection'),
  import('@deepseek-ai/dsh-system-prompt'),
  import('@deepseek-ai/dsh-tools'),
  import('@deepseek-ai/dsh-tool-executor-native'),
])

const { default: LlmRuntime, LlmAdapter, createUserMessage } = llm
const { apply, inject, name } = nativeExecutor
const model = 'request-budget-hook-model'

class RequestBudgetProbeAdapter extends LlmAdapter {
  constructor() {
    super()
    this.agents = new Map()
    this.finalRequests = []
    this.requests = []
  }

  async resolveModel(provider, id) {
    await Promise.resolve()
    return {
      provider,
      id,
      name: id,
      context: { contextWindow: 32_768 },
      defaultMaxTokens: 8_192,
    }
  }

  async *stream(options) {
    const agent = this.agents.get(options.sessionId)
    this.requests.push({
      sessionId: options.sessionId,
      maxTokens: options.maxTokens,
      headerMaxTokens: agent?.session.requestHeader()?.config.maxTokens,
      messages: structuredClone(options.messages),
      tools: structuredClone(options.tools ?? []),
      toolHistory: structuredClone(options.toolHistory),
    })
    yield { type: 'block-start', index: 0, blockType: 'text' }
    yield { type: 'block-end', index: 0, block: { type: 'text', text: 'ok' } }
    yield { type: 'finish', reason: { kind: 'stop' } }
  }
}

async function createHarness() {
  const ctx = new Context()
  const nativeToolExecutor = { name, inject, apply }
  for (const plugin of [LlmRuntime, SessionStore, SessionProjectionRegistry, SystemPrompt, ToolRuntime, nativeToolExecutor, AgentRegistry]) {
    await ctx.plugin(plugin)
  }
  await ctx.plugin(AgentLoop, { agents: [] })
  const adapter = new RequestBudgetProbeAdapter()
  ctx.llm.registerAdapter([model], adapter)
  const prepareCall = ctx.llm.prepareCall.bind(ctx.llm)
  ctx.llm.prepareCall = async (...args) => {
    const call = await prepareCall(...args)
    return Object.freeze({
      ...call,
      stream(options) {
        adapter.finalRequests.push(options)
        return call.stream(options)
      },
    })
  }
  return { ctx, adapter }
}

async function createAgent(ctx, adapter, id, maxTokens = 8_192) {
  const agent = await ctx.agentLoop.create(SessionId(`request_budget_${id}`), {
    provider: model,
    model,
    maxTokens,
  })
  adapter.agents.set(agent.id, agent)
  return agent
}

function ask(agent, text) {
  agent.followup(createUserMessage({
    content: [{ type: 'text', text }],
    source: { kind: 'user' },
  }))
}

function createFixtureTool() {
  return {
    name: 'budget_fixture_tool',
    description: 'A tool schema used to verify request previews.',
    parameters: { type: 'object', properties: {}, additionalProperties: false },
    output: {
      schema: {
        type: 'object',
        properties: { ok: { type: 'boolean' } },
        required: ['ok'],
        additionalProperties: false,
      },
      render: () => [{ type: 'text', text: 'ok' }],
    },
    execute: async () => ({ ok: true }),
  }
}

test('single Agent applies per-request caps and previews the exact final messages, tools, and tool history', async () => {
  const { ctx, adapter } = await createHarness()
  const agent = await createAgent(ctx, adapter, 'single')
  const caps = [512, 1_024]
  const payloads = []
  agent.ctx.on('agent/request-budget', (payload) => {
    payloads.push(payload)
    return { maxTokens: caps.shift() }
  })

  try {
    ask(agent, 'first request budget prompt')
    await agent.whenIdle()
    agent.ctx.tools.register(createFixtureTool())
    ask(agent, 'second request budget prompt')
    await agent.whenIdle()

    assert.deepEqual(adapter.requests.map(({ maxTokens }) => maxTokens), [512, 1_024])
    assert.deepEqual(payloads.map(({ turn, step }) => [turn, step]), [[1, 1], [2, 1]])
    assert.equal(payloads.every((payload) => payload.agent === agent && payload.signal instanceof AbortSignal), true)
    assert.equal(payloads[0].messages.some((message) => JSON.stringify(message).includes('first request budget prompt')), true)
    assert.equal(payloads[1].messages.some((message) => JSON.stringify(message).includes('second request budget prompt')), true)
    assert.equal(payloads[1].tools.some((tool) => tool.name === 'budget_fixture_tool'), true)
    assert.equal(payloads[1].messages.some((message) => message.source?.kind === 'tool-registry'), true)

    for (const [index, payload] of payloads.entries()) {
      const request = adapter.finalRequests[index]
      assert.deepEqual(payload.messages, request.messages)
      assert.deepEqual(payload.tools, request.tools)
      assert.deepEqual(payload.toolHistory, request.toolHistory)
    }
  } finally {
    await ctx.fiber.dispose()
  }
})

test('a new request series resets tool-history preview when the header itself is unchanged', async () => {
  const { ctx, adapter } = await createHarness()
  const agent = await createAgent(ctx, adapter, 'series_reset')
  const payloads = []
  agent.ctx.on('agent/request-budget', (payload) => {
    payloads.push(payload)
    return { maxTokens: 2_048 }
  })
  agent.ctx.on('agent/pre-step', (payload, next) => {
    if (payload.turn !== 3) return next()
    return Promise.resolve(next()).then((decision) => ({ ...decision, startsRequestSeries: true }))
  })

  try {
    ask(agent, 'request before tool registration')
    await agent.whenIdle()
    agent.ctx.tools.register(createFixtureTool())
    ask(agent, 'request adding the fixture tool')
    await agent.whenIdle()
    assert.equal(adapter.finalRequests[1].toolHistory.updates.length > 0, true)

    ask(agent, 'request starting a new unchanged-header series')
    await agent.whenIdle()

    assert.deepEqual(payloads[2].toolHistory, adapter.finalRequests[2].toolHistory)
    assert.deepEqual(payloads[2].toolHistory, { tools: payloads[2].tools, updates: [] })
  } finally {
    await ctx.fiber.dispose()
  }
})

test('concurrent Agents on the same model keep their request caps isolated', async () => {
  const { ctx, adapter } = await createHarness()
  const first = await createAgent(ctx, adapter, 'concurrent_a')
  const second = await createAgent(ctx, adapter, 'concurrent_b')
  first.ctx.on('agent/request-budget', () => ({ maxTokens: 768 }))
  second.ctx.on('agent/request-budget', () => ({ maxTokens: 3_072 }))

  try {
    ask(first, 'first child request')
    ask(second, 'second child request')
    await Promise.all([first.whenIdle(), second.whenIdle()])

    const bySession = Object.fromEntries(adapter.requests.map((request) => [request.sessionId, request.maxTokens]))
    assert.equal(bySession[first.id], 768)
    assert.equal(bySession[second.id], 3_072)
    assert.equal(first.session.requestHeader().config.maxTokens, 768)
    assert.equal(second.session.requestHeader().config.maxTokens, 3_072)
  } finally {
    await ctx.fiber.dispose()
  }
})

test('the logged request header cap always matches the prepared stream options', async () => {
  const { ctx, adapter } = await createHarness()
  const agent = await createAgent(ctx, adapter, 'header_match')
  const caps = [2_048, 4_096]
  agent.ctx.on('agent/request-budget', () => ({ maxTokens: caps.shift() }))

  try {
    ask(agent, 'first header match request')
    await agent.whenIdle()
    ask(agent, 'second header match request')
    await agent.whenIdle()

    assert.deepEqual(adapter.requests.map(({ maxTokens, headerMaxTokens }) => [maxTokens, headerMaxTokens]), [[2_048, 2_048], [4_096, 4_096]])
  } finally {
    await ctx.fiber.dispose()
  }
})

test('an Agent without a listener or with an undefined hook result preserves the original request path', async () => {
  const { ctx, adapter } = await createHarness()
  const agent = await createAgent(ctx, adapter, 'no_handler', 2_048)

  try {
    ask(agent, 'request without a budget listener')
    await agent.whenIdle()
    agent.ctx.on('agent/request-budget', () => undefined)
    ask(agent, 'request with an undefined budget result')
    await agent.whenIdle()
    assert.deepEqual(adapter.requests.map(({ maxTokens, headerMaxTokens }) => [maxTokens, headerMaxTokens]), [[2_048, 2_048], [2_048, 2_048]])
  } finally {
    await ctx.fiber.dispose()
  }
})
