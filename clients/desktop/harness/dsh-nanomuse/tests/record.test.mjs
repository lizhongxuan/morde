// A turn run on a key of the person's own goes up to the account's record afterwards
// (src/record.ts, relay 0.29 `POST /v1/turns`): the stream is let through untouched, the
// answer is assembled from the chunks, pictures are put up once by their hash and named as
// `blob:sha256:` references, a stream that broke is kept as partial, the queue is bounded,
// and a failed upload never reaches the chat.
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { test } from 'node:test'
import { BLOB_PREFIX, OwnTurns, QUEUE_MAX, assistantMessage, toolRow, turnId } from '../lib/record.js'

const png = Buffer.from([0x89, 0x50, 0x4e, 0x47, 1, 2, 3])
const sha = createHash('sha256').update(png).digest('hex')
const imageRef = { attachmentId: 'att-1', mediaType: 'image/png', bytes: png.length, width: 1, height: 1 }

/** The fakes a test drives; `fail` makes the relay refuse. */
function world({ enabled = true, known = [], fail = false } = {}) {
  const w = { blobs: [], heads: [], turns: [], logs: [] }
  w.turnsClient = new OwnTurns({
    enabled: () => enabled,
    readImage: async (ref) => ({ data: ref.attachmentId === 'att-1' ? png : Buffer.alloc(0) }),
    hasBlob: async (s) => { w.heads.push(s); return known.includes(s) },
    putBlob: async (file) => { w.blobs.push(file) },
    postTurn: async (turn) => { if (fail) throw new Error('relay says no'); w.turns.push(turn) },
    providerName: (route) => (route === 'own-row' ? 'openrouter' : route),
    log: (level, text) => w.logs.push(`${level}: ${text}`),
    now: () => 1_760_000_000_000,
  })
  return w
}

const request = (messages, extra = {}) => ({ provider: 'own-row', model: 'anthropic/claude-sonnet-4.5', sessionId: 'sess-1', messages, ...extra })

async function* chunks(list) {
  for (const c of list) yield c
}

const answered = [
  { type: 'block-start', index: 0, blockType: 'reasoning' },
  { type: 'reasoning-delta', index: 0, text: 'thinking ' },
  { type: 'reasoning-delta', index: 0, text: 'hard' },
  { type: 'block-end', index: 0, block: { type: 'reasoning', text: 'thinking hard' } },
  { type: 'block-start', index: 1, blockType: 'text' },
  { type: 'text-delta', index: 1, text: 'Hello' },
  { type: 'text-delta', index: 1, text: ' there' },
  { type: 'block-end', index: 1, block: { type: 'text', text: 'Hello there' } },
  { type: 'block-start', index: 2, blockType: 'tool-call' },
  { type: 'tool-call-delta', index: 2, id: 'call_1', name: 'web_search', argumentsDelta: '{"q":' },
  { type: 'tool-call-delta', index: 2, id: 'call_1', argumentsDelta: '"trains"}' },
  { type: 'block-end', index: 2, block: { type: 'tool-call', id: 'call_1', name: 'web_search', arguments: '{"q":"trains"}' } },
  { type: 'usage', usage: { inputTokens: 100, outputTokens: 20, cacheReadTokens: 50 } },
  { type: 'finish', reason: { kind: 'tool-calls' } },
]

async function drain(w, req, list) {
  const seen = []
  for await (const c of w.turnsClient.watch(req, chunks(list))) seen.push(c)
  await w.turnsClient.settle()
  return seen
}

test('the chunks pass through untouched and the turn goes up in the relay\'s shape: roles, parts, tool calls, usage, provider and model as ids', async () => {
  const w = world()
  const req = request([
    { id: 'm0', role: 'system', content: [{ type: 'text', text: 'You are nanoMuse.' }], source: { kind: 'system-prompt' } },
    { id: 'm1', role: 'user', content: [{ type: 'text', text: 'What is on the screen?' }, { type: 'image', attachment: imageRef }], source: { kind: 'user' } },
    { id: 'm2', role: 'assistant', content: [{ type: 'tool-call', id: 'call_0', name: 'computer_screen', arguments: '{}' }], source: { kind: 'model', provider: 'own-row', model: 'x' } },
    { id: 'm3', role: 'tool', toolCallId: 'call_0', isError: true, content: [{ type: 'text', text: 'no display' }], source: { kind: 'tool', callId: 'call_0' } },
  ], { tools: [{ name: 'web_search', description: 'Search the web', parameters: { type: 'object', properties: {} } }] })
  const seen = await drain(w, req, answered)
  assert.deepEqual(seen, answered, 'nothing of the stream was changed or held back')
  assert.equal(w.turns.length, 1)
  const turn = w.turns[0]
  assert.equal(turn.provider, 'openrouter', 'the catalogue id of the row, never the key or the URL')
  assert.equal(turn.model, 'anthropic/claude-sonnet-4.5')
  assert.equal(turn.conversation, 'sess-1')
  assert.equal(turn.ts, 1_760_000_000)
  assert.equal(turn.status, 'ok')
  assert.deepEqual(turn.messages[0], { role: 'system', content: 'You are nanoMuse.' })
  assert.deepEqual(turn.messages[1], { role: 'user', content: [{ type: 'text', text: 'What is on the screen?' }, { type: 'image_url', image_url: { url: `${BLOB_PREFIX}${sha}` } }] })
  assert.deepEqual(turn.messages[2], { role: 'assistant', content: [], tool_calls: [{ id: 'call_0', type: 'function', function: { name: 'computer_screen', arguments: '{}' } }] })
  assert.deepEqual(turn.messages[3], { role: 'tool', content: 'no display', tool_call_id: 'call_0', is_error: true })
  assert.deepEqual(turn.tools, [{ type: 'function', function: { name: 'web_search', description: 'Search the web', parameters: { type: 'object', properties: {} } } }])
  assert.deepEqual(turn.response.message, {
    role: 'assistant',
    content: 'Hello there',
    reasoning_content: 'thinking hard',
    tool_calls: [{ id: 'call_1', type: 'function', function: { name: 'web_search', arguments: '{"q":"trains"}' } }],
    finish_reason: 'tool_calls',
  })
  assert.deepEqual(turn.response.usage, { prompt_tokens: 150, completion_tokens: 20, cached_tokens: 50 })
  // the picture went up once, by its hash, after the HEAD said it was not there
  assert.deepEqual(w.heads, [sha])
  assert.equal(w.blobs.length, 1)
  assert.equal(w.blobs[0].sha256, sha)
  assert.equal(w.blobs[0].mime, 'image/png')
  assert.equal(w.turnsClient.sent, 1)
})

test('a picture another device already put up is not sent again; the same picture twice in a turn is one file', async () => {
  const w = world({ known: [sha] })
  const req = request([
    { id: 'm1', role: 'user', content: [{ type: 'image', attachment: imageRef }, { type: 'image', attachment: imageRef }], source: { kind: 'user' } },
  ])
  await drain(w, req, [{ type: 'text-delta', index: 0, text: 'ok' }, { type: 'finish', reason: { kind: 'stop' } }])
  assert.deepEqual(w.heads, [sha])
  assert.equal(w.blobs.length, 0)
  assert.equal(w.turns[0].messages[0].content.length, 2)
  assert.equal(w.turns[0].response.message.finish_reason, 'stop')
})

test('a stream that ends in an error or is abandoned is kept as partial, with the call that never closed', async () => {
  const w = world()
  await drain(w, request([{ id: 'm1', role: 'user', content: [{ type: 'text', text: 'hi' }], source: { kind: 'user' } }]), [
    { type: 'text-delta', index: 0, text: 'Half an ans' },
    { type: 'tool-call-delta', index: 1, id: 'call_9', name: 'web_fetch', argumentsDelta: '{"url":"https://' },
    { type: 'finish', reason: { kind: 'error', failure: { code: 'upstream', message: 'gone' } } },
  ])
  assert.equal(w.turns[0].status, 'partial')
  assert.equal(w.turns[0].ended, 'error')
  assert.equal(w.turns[0].response.message.content, 'Half an ans')
  assert.deepEqual(w.turns[0].response.message.tool_calls, [{ id: 'call_9', type: 'function', function: { name: 'web_fetch', arguments: '{"url":"https://' } }])
  // the consumer stops reading before the finish: what arrived is a partial turn too
  const w2 = world()
  const it = w2.turnsClient.watch(request([{ id: 'm1', role: 'user', content: [{ type: 'text', text: 'hi' }], source: { kind: 'user' } }]), chunks([{ type: 'text-delta', index: 0, text: 'a' }, { type: 'text-delta', index: 0, text: 'b' }, { type: 'finish', reason: { kind: 'stop' } }]))[Symbol.asyncIterator]()
  await it.next()
  await it.return()
  await w2.turnsClient.settle()
  assert.equal(w2.turns.length, 1)
  assert.equal(w2.turns[0].status, 'partial')
  assert.equal(w2.turns[0].ended, 'abandoned')
  assert.equal(w2.turns[0].response.message.content, 'a')
  // a stop by the person: the stream's own finish says so
  const w3 = world()
  await drain(w3, request([{ id: 'm1', role: 'user', content: [{ type: 'text', text: 'hi' }], source: { kind: 'user' } }]), [
    { type: 'text-delta', index: 0, text: 'Half' },
    { type: 'finish', reason: { kind: 'aborted' } },
  ])
  assert.equal(w3.turns[0].status, 'partial')
  assert.equal(w3.turns[0].ended, 'aborted')
})

test('nothing goes up while the record is off; a relay that refuses is logged, not thrown into the chat; the queue is bounded', async () => {
  const off = world({ enabled: false })
  const seen = await drain(off, request([{ id: 'm1', role: 'user', content: [{ type: 'text', text: 'hi' }], source: { kind: 'user' } }]), [{ type: 'finish', reason: { kind: 'stop' } }])
  assert.equal(seen.length, 1)
  assert.equal(off.turns.length, 0)
  assert.equal(off.heads.length, 0)

  const refused = world({ fail: true })
  await drain(refused, request([{ id: 'm1', role: 'user', content: [{ type: 'text', text: 'hi' }], source: { kind: 'user' } }]), [{ type: 'finish', reason: { kind: 'stop' } }])
  assert.equal(refused.turns.length, 0)
  assert.equal(refused.turnsClient.dropped, 1)
  assert.ok(refused.logs.some((l) => l.startsWith('info: nanomuse record: an own-key turn was not sent: relay says no')))

  // a relay out of reach must not hoard memory: past QUEUE_MAX the oldest waiting upload is dropped
  let release
  const gate = new Promise((r) => { release = r })
  const slow = new OwnTurns({
    enabled: () => true,
    readImage: async () => ({ data: Buffer.alloc(0) }),
    hasBlob: async () => true,
    putBlob: async () => {},
    postTurn: async () => { await gate },
    now: () => 1_760_000_000_000,
  })
  const msg = [{ id: 'm1', role: 'user', content: [{ type: 'text', text: 'hi' }], source: { kind: 'user' } }]
  for (let i = 0; i < QUEUE_MAX + 5; i++) {
    for await (const _ of slow.watch(request(msg), chunks([{ type: 'finish', reason: { kind: 'stop' } }]))) { /* read through */ }
  }
  // one upload is in flight behind the gate, QUEUE_MAX wait behind it, the rest were dropped
  assert.equal(slow.pending, QUEUE_MAX)
  assert.equal(slow.dropped, 4)
  release()
  await slow.settle()
  assert.equal(slow.sent, QUEUE_MAX + 1)
})

test('the helpers: finish reasons in OpenAI\'s words, a tool row, a stable turn id', () => {
  assert.equal(assistantMessage({ content: 'x', reasoning: '', calls: [], finish: 'max-tokens', partial: false }).finish_reason, 'length')
  assert.equal(assistantMessage({ content: 'x', reasoning: '', calls: [], finish: '', partial: true }).finish_reason, 'stop')
  assert.deepEqual(toolRow({ name: 'a', description: 'b', parameters: { type: 'object' } }), { type: 'function', function: { name: 'a', description: 'b', parameters: { type: 'object' } } })
  const req = request([{ id: 'm1', role: 'user', content: [{ type: 'text', text: 'hi' }], source: { kind: 'user' } }])
  assert.equal(turnId(req, 1_760_000_000_000), turnId(req, 1_760_000_000_500), 'the same request in the same second is one turn')
  assert.match(turnId(req, 1_760_000_000_000), /^dsh-1760000000-[0-9a-f]{16}$/)
  assert.notEqual(turnId(req, 1_760_000_000_000), turnId({ ...req, model: 'other' }, 1_760_000_000_000))
})
