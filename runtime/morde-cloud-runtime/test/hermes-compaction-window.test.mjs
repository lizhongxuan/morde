import assert from 'node:assert/strict'
import test from 'node:test'
import { Context } from '@deepseek-ai/cordis'
import { createMessage, createToolResultMessage, createUserMessage, ToolCallId } from '@deepseek-ai/dsh-llm'
import { Session, SessionId } from '@deepseek-ai/dsh-session'
import SessionProjectionRegistry from '@deepseek-ai/dsh-session-projection'
import ToolResultPruner from '@deepseek-ai/dsh-compaction-tool-result-pruner'
import TokenMeter from '@deepseek-ai/dsh-token-meter'
import { validStart } from '../src/engine.mjs'
import { hermesCompactionBudget, hermesCompactionMadeProgress, originalToolResultsForRange, planHermesCompaction, selectHermesCompactionRange, taskBudgetHermesCompactionBudget } from '../src/hermes-compaction-window.mjs'

test('Hermes trigger and lean-tail budgets use output-reserved capacity at both context scales', () => {
  assert.deepEqual(hermesCompactionBudget(64_000, 8_000), { thresholdTokens: 47_600, tailTokens: 10_000 })
  assert.deepEqual(hermesCompactionBudget(1_000_000, 100_000), { thresholdTokens: 450_000, tailTokens: 25_000 })
  assert.throws(() => hermesCompactionBudget(4096, 4096), /output reservation/)
})

test('adaptive Task compaction stays inside conservative byte-based context holds', () => {
  const large = taskBudgetHermesCompactionBudget(65_536, 40_000)
  assert.equal(large.thresholdTokens, 8_192)
  assert.equal(large.rangeOutputTokens, 49_152)
  assert.equal(large.tailTokens, 1_638)
  assert.ok(large.thresholdTokens * 4 < 65_536,
    'the DSH pressure threshold leaves at least half the model window for byte-reserved input')
  assert.ok(large.tailTokens * 2 < 65_536,
    'the compacted tail leaves room for framing and summary instructions under byte reservations')

  const small = taskBudgetHermesCompactionBudget(16_384, 10_756)
  assert.equal(small.thresholdTokens, 2_048)
  assert.equal(small.rangeOutputTokens, 12_288)
  assert.equal(small.tailTokens, 409)
})

test('compaction progress is measured by token reduction or new covered source events', () => {
  const covered = new Set([4, 5])
  assert.equal(hermesCompactionMadeProgress(100, 90, [], covered), true)
  assert.equal(hermesCompactionMadeProgress(100, 100, [6], covered), true)
  assert.equal(hermesCompactionMadeProgress(100, 100, [4, 5], covered), false)
  assert.equal(hermesCompactionMadeProgress(100, 110, [4, 5], covered), false)
})

test('Hermes windows keep the system/head, last user and assistant, and whole tool groups', () => {
  const rows = [
    { role: 'system', tokens: 20, balancedBefore: true },
    { role: 'user', tokens: 300, balancedBefore: true, isUserInput: true },
    { role: 'assistant', tokens: 200, balancedBefore: true, hasText: true },
    { role: 'user', tokens: 300, balancedBefore: true, isUserInput: true },
    { role: 'assistant', tokens: 400, balancedBefore: true, hasToolCalls: true },
    { role: 'tool', tokens: 500, balancedBefore: false },
    { role: 'assistant', tokens: 300, balancedBefore: true, hasText: true },
    { role: 'user', tokens: 300, balancedBefore: true, isUserInput: true },
    { role: 'assistant', tokens: 200, balancedBefore: true, hasText: true },
    { role: 'user', tokens: 300, balancedBefore: true, isUserInput: true },
    { role: 'assistant', tokens: 200, balancedBefore: true, hasText: true },
  ]
  const first = planHermesCompaction(rows, { contextWindow: 4096 })
  assert.equal(first.start, 4)
  assert.ok(rows[first.end].balancedBefore)
  assert.ok(rows.slice(first.end).some(row => row.role === 'user' && row.isUserInput))
  assert.ok(rows.slice(first.end).some(row => row.role === 'assistant' && row.hasText))
  const repeated = planHermesCompaction(rows.map((row, index) => index === 1 ? { ...row, isSummary: true } : row), {
    contextWindow: 4096, repeated: true,
  })
  assert.equal(repeated.start, 1)
  const task = planHermesCompaction(rows, { contextWindow: 4096, pinTask: true })
  assert.equal(task.start, 2)
  const withContext = planHermesCompaction([
    rows[0],
    { role: 'user', tokens: 100, balancedBefore: true, isUserInput: false },
    { role: 'user', tokens: 200, balancedBefore: true, isUserInput: true },
    ...rows.slice(2),
  ], { contextWindow: 4096, pinTask: true })
  assert.equal(withContext.start, 3, 'pin the actual task input after trusted context')
})

test('adaptive Task compaction keeps a bounded recent message tail', () => {
  const rows = [
    { role: 'system', tokens: 20, balancedBefore: true },
    { role: 'user', tokens: 300, balancedBefore: true, isUserInput: true },
    { role: 'assistant', tokens: 200, balancedBefore: true, hasText: true },
    ...Array.from({ length: 10 }, (_, index) => [
      { role: 'user', tokens: 550, balancedBefore: true, isUserInput: true, index },
      { role: 'assistant', tokens: 100, balancedBefore: true, hasText: true, index },
    ]).flat(),
    { role: 'user', tokens: 550, balancedBefore: true, isUserInput: true, index: 10 },
  ]
  const defaultRange = planHermesCompaction(rows, {
    contextWindow: 16_384, outputTokens: 12_288, pinTask: true,
  })
  const adaptiveRange = planHermesCompaction(rows, {
    contextWindow: 16_384, outputTokens: 12_288, pinTask: true, tailTokens: 409,
  })
  assert.ok(defaultRange)
  assert.ok(adaptiveRange)
  assert.ok(adaptiveRange.end - adaptiveRange.start > defaultRange.end - defaultRange.start,
    `the adaptive Task range should cover more history; default=${JSON.stringify(defaultRange)} adaptive=${JSON.stringify(adaptiveRange)}`)
  assert.equal(rows.at(-1).isUserInput, true, 'keep the latest user request')
  assert.ok(adaptiveRange.end < rows.length - 1, 'the adaptive range must stop before the latest user request')
})

test('DSH frame validation rejects an output cap that consumes the known context window', () => {
  const start = {
    type: 'start', version: 1, task_id: 'task_window', owner_id: 'owner_window', agent_id: 'agent_window',
    generation: 1, authorization_version: 1, session_id: 'session_window', task_revision: 1, model: 'test-model', tools: [],
    prompt: 'continue', context_window_tokens: 4096,
  }
  assert.equal(validStart({ ...start, max_tokens: 1024 }), true)
  assert.equal(validStart({ ...start, max_tokens: 4096 }), false)
})

test('Hermes selection maps the planned window back to current DSH surface sequences', async () => {
  const ctx = new Context()
  await ctx.plugin(SessionProjectionRegistry)
  await ctx.plugin(TokenMeter)
  const session = Session.create(SessionId('hermes_window_session'))
  for (let i = 0; i < 10; i++) {
    session.append('user/message', createUserMessage({
      content: [{ type: 'text', text: `message ${i} ${'history '.repeat(120)}` }],
      source: { kind: 'user' },
    }), { surfaceOp: 'append' })
  }
  try {
    const measurement = ctx.tokenMeter.measure(session)
    const range = selectHermesCompactionRange(session, measurement, { contextWindow: 4096 })
    assert.ok(range)
    assert.equal(range.start, session.surface.nodes[3])
    assert.ok(session.surface.nodes.indexOf(range.end) < session.surface.nodes.length - 1)
    assert.equal(session.deriveMessages().at(-1).content[0].text.includes('message 9'), true)
  } finally {
    await ctx.fiber.dispose()
  }
})

test('compaction can restore a pruned tool result through DSH source-event links', async () => {
  const ctx = new Context()
  await ctx.plugin(SessionProjectionRegistry)
  await ctx.plugin(TokenMeter)
  await ctx.plugin(ToolResultPruner)
  const session = Session.create(SessionId('hermes_tool_source_session'))
  const callId = ToolCallId('call_hermes_tool_source')
  const originalText = 'complete authorized tool output '.repeat(800)
  session.append('turn/start', { turn: 1 })
  session.append('step/start', { turn: 1, step: 1 })
  session.append('assistant/message', {
    stream: [], turn: 1, step: 1,
    message: createMessage({ role: 'assistant',
      content: [{ type: 'tool-call', id: callId, name: 'bash', arguments: '{}' }],
      source: { kind: 'model', provider: 'test', model: 'test' } }),
  }, { surfaceOp: 'append' })
  const call = session.append('tool/call', { turn: 1, step: 1, callId, name: 'bash', arguments: '{}' })
  const original = session.append('tool/result', {
    turn: 1, step: 1,
    message: createToolResultMessage({ callId, content: [{ type: 'text', text: originalText }], isError: false }),
  }, { surfaceOp: 'append', sourceEventSeqs: [call.seq] })
  session.append('step/end', { turn: 1, step: 1 })
  session.append('turn/end', { turn: 1, reason: { kind: 'completed' } })
  const { pruned } = ctx.toolResultPruner.pruneSession(session)
  try {
    assert.equal(pruned.length, 1)
    const replacement = session.snapshotEvents().findLast(event => event.type === 'tool/result')
    const restored = originalToolResultsForRange(session, replacement.seq, replacement.seq)
    assert.equal(restored.get(callId).content[0].text, originalText)
    assert.ok(session.deriveEventMessage(replacement).content[0].text.length < originalText.length)
    assert.equal(session.eventAt(original.seq).data.message.content[0].text, originalText)
  } finally {
    await ctx.fiber.dispose()
  }
})
