import assert from 'node:assert/strict'
import test from 'node:test'
import { recoveryToolCalls, validStart, webResearchToolGuardReason, WEB_RESEARCH_PROFILE_PROMPT } from '../src/engine.mjs'

const searchTool = {
  name: 'morde_web_search',
  description: 'Search public web pages.',
  parameters: {
    type: 'object',
    properties: { query: { type: 'string' } },
    required: ['query'],
    additionalProperties: false,
  },
}

const readTool = {
  name: 'morde_web_read',
  description: 'Read a public HTTPS page.',
  parameters: {
    type: 'object',
    properties: { url: { type: 'string' }, start_char: { type: 'integer', minimum: 0 } },
    required: ['url'],
    additionalProperties: false,
  },
}

const resultEvidenceTool = {
  name: 'morde_task_result_evidence',
  description: 'Attach evidence to material claims in the task result.',
  parameters: { type: 'object', properties: {}, additionalProperties: false },
}

const askUserTool = {
  name: 'ask_user',
  description: 'Ask the user a necessary clarification.',
  parameters: { type: 'object', properties: {}, additionalProperties: false },
}

const askUserQuestionTool = {
  name: 'ask_user_question',
  description: 'Ask the user a structured clarification.',
  parameters: { type: 'object', properties: {}, additionalProperties: false },
}

const taskStart = {
  type: 'start',
  version: 1,
  task_id: 'task_123',
  owner_id: 'user_123',
  agent_id: 'agent_123',
  generation: 1,
  authorization_version: 1,
  session_id: 'session_456',
  task_revision: 0,
  model: 'test-model',
  prompt: 'Research this topic.',
}

function call(id, name, args, seq) {
  return {
    type: 'tool/call',
    seq,
    data: { turn: 1, step: seq, callId: id, name, arguments: JSON.stringify(args) },
  }
}

function result(id, text, seq, { failed = false, guardDenied = false } = {}) {
  return {
    type: 'tool/result',
    seq,
    surfaceOp: 'append',
    sourceEventSeqs: [seq - 1],
    data: {
      turn: 1,
      step: seq - 1,
      message: {
        role: 'tool',
        toolCallId: id,
        source: { kind: 'tool', callId: id },
        content: [{ type: 'text', text }],
        isError: guardDenied,
      },
      ...(guardDenied ? { error: { name: 'WebResearchGuardError', code: 'WEB_RESEARCH_GUARD' } } : {}),
    },
    ...(failed ? { failed: true } : {}),
  }
}

function successfulCall(id, name, args, callSeq, resultSeq) {
  return [call(id, name, args, callSeq), result(id, 'Public evidence returned.', resultSeq)]
}

function currentCall(events, id, name, args) {
  const event = call(id, name, args, events.length + 1)
  return [
    webResearchToolGuardReason([...events, event], id, name, args),
    event,
  ]
}

test('web_research start accepts only revision zero and the narrow tool set', () => {
  const profileStart = { ...taskStart, agent_tool_profile: 'web_research', tools: [searchTool, readTool, resultEvidenceTool] }
  assert.equal(validStart(profileStart), true)
  assert.equal(validStart({ ...profileStart, task_revision: 1 }), false)
  assert.equal(validStart({ ...profileStart, agent_tool_profile: 'unknown' }), false)
  assert.equal(validStart({ ...profileStart, tools: [searchTool] }), false)
  assert.equal(validStart({ ...profileStart, tools: [searchTool, readTool, resultEvidenceTool, askUserTool, askUserQuestionTool] }), true)
  assert.equal(validStart({ ...taskStart, tools: [searchTool, readTool, resultEvidenceTool, askUserQuestionTool] }), false)
  assert.equal(validStart({ ...profileStart, tools: [searchTool, readTool, {
    name: 'shell', description: 'Run commands.', parameters: { type: 'object', properties: {}, additionalProperties: false },
  }] }), false)
})

test('the persisted call/result history blocks successful duplicate and follow-up queries', () => {
  const history = successfulCall('search_one', 'morde_web_search', { query: 'today news', category: 'news' }, 1, 2)
  const [duplicate] = currentCall(history, 'search_duplicate', 'morde_web_search', { category: 'news', query: 'TODAY   NEWS' })
  assert.match(duplicate, /already succeeded/)
  const [different] = currentCall(history, 'search_two', 'morde_web_search', { query: 'today technology news', category: 'news' })
  assert.match(different, /one successful search is already recorded/)
})

test('the two-search cap is reconstructed from DSH history', () => {
  const history = [
    call('search_one', 'morde_web_search', { query: 'today finance', category: 'news' }, 1),
    result('search_one', 'Tool failed: temporary upstream failure', 2, { failed: true }),
    call('search_two', 'morde_web_search', { query: 'today science', category: 'news' }, 3),
    result('search_two', 'Tool failed: temporary upstream failure', 4, { failed: true }),
  ]
  const [reason] = currentCall(history, 'search_three', 'morde_web_search', { query: 'today culture', category: 'news' })
  assert.match(reason, /two-search limit/)
})

test('the web research prompt describes the same bounded one-pass workflow as the runtime guard', () => {
  assert.match(WEB_RESEARCH_PROFILE_PROMPT, /one focused search/)
  assert.match(WEB_RESEARCH_PROFILE_PROMPT, /after one successful search, do not search again/)
  assert.match(WEB_RESEARCH_PROFILE_PROMPT, /at most two relevant direct sources/)
  assert.match(WEB_RESEARCH_PROFILE_PROMPT, /A third read is allowed only as a second window/)
  assert.match(WEB_RESEARCH_PROFILE_PROMPT, /do not continue broadening the search/)
})

test('one successful search blocks a different follow-up query', () => {
  const history = successfulCall('search_one', 'morde_web_search', { query: 'today technology news', category: 'news' }, 1, 2)
  const [reason] = currentCall(history, 'search_two', 'morde_web_search', { query: 'today science news', category: 'news' })
  assert.match(reason, /one successful search is already recorded/)
})

test('failed searches may retry, and guard-denied calls do not consume the search cap', () => {
  const events = [
    call('search_failed', 'morde_web_search', { query: 'today finance' }, 1),
    result('search_failed', 'Tool failed: temporary upstream failure', 2),
    call('search_guarded', 'morde_web_search', { query: 'today finance' }, 3),
    result('search_guarded', 'Error: web_research guard: duplicate', 4, { guardDenied: true }),
  ]
  const [reason] = currentCall(events, 'search_retry', 'morde_web_search', { query: 'today finance' })
  assert.equal(reason, undefined)
})

test('the persisted URL/window history blocks exact repeats but allows a new read window', () => {
  const url = 'https://example.com/news'
  const history = successfulCall('read_one', 'morde_web_read', { url, start_char: 0 }, 1, 2)
  const [duplicate] = currentCall(history, 'read_duplicate', 'morde_web_read', { url: `${url}#fragment`, start_char: 0 })
  assert.match(duplicate, /URL window already succeeded/)
  const [nextWindow] = currentCall(history, 'read_next_window', 'morde_web_read', { url, start_char: 2400 })
  assert.equal(nextWindow, undefined)
})

test('web research bounds the number of sources, read windows, and total reads', () => {
  const twoURLs = [
    ...successfulCall('read_one', 'morde_web_read', { url: 'https://one.example/a' }, 1, 2),
    ...successfulCall('read_two', 'morde_web_read', { url: 'https://two.example/b' }, 3, 4),
  ]
  const [thirdURL] = currentCall(twoURLs, 'read_three', 'morde_web_read', { url: 'https://three.example/c' })
  assert.match(thirdURL, /two-source URL limit/)

  const twoWindows = [
    ...successfulCall('window_zero', 'morde_web_read', { url: 'https://one.example/a', start_char: 0 }, 1, 2),
    ...successfulCall('window_2400', 'morde_web_read', { url: 'https://one.example/a', start_char: 2400 }, 3, 4),
  ]
  const [thirdWindow] = currentCall(twoWindows, 'window_4800', 'morde_web_read', { url: 'https://one.example/a', start_char: 4800 })
  assert.match(thirdWindow, /two-window limit/)

  const threeReads = [
    ...successfulCall('read_1', 'morde_web_read', { url: 'https://source1.example/article', start_char: 0 }, 1, 2),
    ...successfulCall('read_2', 'morde_web_read', { url: 'https://source1.example/article', start_char: 2400 }, 3, 4),
    ...successfulCall('read_3', 'morde_web_read', { url: 'https://source2.example/article', start_char: 0 }, 5, 6),
  ]
  const [fourthRead] = currentCall(threeReads, 'read_4', 'morde_web_read', {
    url: 'https://source2.example/article', start_char: 2400,
  })
  assert.match(fourthRead, /three-read limit/)
})

test('an unresolved prior call cannot be repeated before its outcome is known', () => {
  const pending = [call('read_pending', 'morde_web_read', { url: 'https://example.com/news', start_char: 0 }, 1)]
  const [reason] = currentCall(pending, 'read_duplicate_pending', 'morde_web_read', {
    url: 'https://example.com/news', start_char: 0,
  })
  assert.match(reason, /still unresolved/)
})

test('checkpoint recovery recognizes a guard denial as settled without replaying the tool', () => {
  const callID = 'search_guard_1'
  const argumentsJSON = JSON.stringify({ query: 'today news', category: 'news' })
  const events = [
    { type: 'turn/start', seq: 0, data: { turn: 1 } },
    { type: 'step/start', seq: 1, data: { turn: 1, step: 1 } },
    { type: 'assistant/message', seq: 2, data: { turn: 1, step: 1, message: { role: 'assistant', content: [
      { type: 'tool-call', id: callID, name: 'morde_web_search', arguments: argumentsJSON },
    ] } } },
    { type: 'tool/call', seq: 3, data: { turn: 1, step: 1, callId: callID, name: 'morde_web_search', arguments: argumentsJSON } },
    {
      type: 'tool/result', seq: 4, surfaceOp: 'append', sourceEventSeqs: [3],
      data: {
        turn: 1,
        step: 1,
        message: {
          role: 'tool', toolCallId: callID, source: { kind: 'tool', callId: callID }, isError: true,
          content: [{ type: 'text', text: 'Error: web_research guard: the two-search limit has been reached.' }],
        },
        error: { name: 'WebResearchGuardError', code: 'WEB_RESEARCH_GUARD' },
      },
    },
    { type: 'step/end', seq: 5, data: { turn: 1, step: 1 } },
    { type: 'turn/end', seq: 6, data: { turn: 1, reason: { kind: 'completed' } } },
  ]

  const history = recoveryToolCalls(events, [searchTool])
  assert.equal(history.completed, true)
  assert.equal(history.calls.length, 1)
  assert.equal(history.calls[0].resultGuardDenied, true)
  assert.equal(history.calls[0].resultText.startsWith('Error: web_research guard:'), true)
})
