/**
 * The account's record for turns that did not pass through the relay (relay 0.29,
 * `docs/cloud.md`, "The record"): a chat answered by a key of the person's own is sent up
 * afterwards in the shape the relay keeps, so the record is whole wherever the model was.
 *
 * The host's `llm/stream` waterfall shows every model call: the request as the adapter is
 * about to send it (`GenerateOptions`) and the chunks that come back. `OwnTurns.watch` lets
 * the chunks through untouched and, once the stream has finished, assembles the answer and
 * queues one upload: the pictures first (`PUT /v1/blobs/{sha256}`, a `HEAD` saving what
 * another device already put up), then `POST /v1/turns` with `blob:sha256:` references
 * where the pictures were. Nothing here can fail a chat: an upload that fails is logged
 * and dropped, the queue is bounded, and the key of the person's own never leaves the
 * computer (the relay gets the provider's id and the model's).
 */

import { createHash } from 'node:crypto'
import type { GenerateOptions, StreamChunk, ToolSchema, ContentBlock, TokenUsage, RequestMessage } from '@deepseek-ai/dsh-llm'
import type { ImageAttachmentRef } from '@deepseek-ai/dsh-attachment'

/** `blob:sha256:<hex>`: how a turn names a file put up first. */
export const BLOB_PREFIX = 'blob:sha256:'
/** Uploads waiting their turn; past this the oldest is dropped (a relay out of reach must not hoard memory). */
export const QUEUE_MAX = 32
/** The relay keeps a turn of at most this many characters of JSON (`Cloud.TURN_MAX_CHARS`). */
export const TURN_MAX_CHARS = 2_000_000

/** A picture the request carried, as the relay will get it. */
export interface TurnFile {
  sha256: string
  mime: string
  data: Uint8Array
}

/** `POST /v1/turns`' body (`docs/cloud.md`). */
export interface OwnTurn {
  id: string
  ts: number
  provider: string
  model: string
  conversation: string
  messages: Record<string, unknown>[]
  tools?: Record<string, unknown>[]
  response: { message: Record<string, unknown>; usage?: { prompt_tokens: number; completion_tokens: number; cached_tokens: number } }
  status: 'ok' | 'partial'
  /** With `partial`: why it ended short. `aborted`: the person stopped it; `error`: the stream threw; `abandoned`: the loop stopped reading before a finish. */
  ended?: 'aborted' | 'error' | 'abandoned'
}

/** What `watch` needs of the world; the tests hand in fakes. */
export interface OwnTurnsOptions {
  /** Whether a turn should be sent at all right now (signed in, the relay keeps a record). */
  enabled: () => boolean
  /** The attachment's bytes, from the host's store. */
  readImage: (ref: ImageAttachmentRef) => Promise<{ data: Uint8Array }>
  /** `HEAD /v1/blobs/{sha256}`: true when the account can already read the file. */
  hasBlob: (sha256: string) => Promise<boolean>
  /** `PUT /v1/blobs/{sha256}` with the raw bytes and their type. */
  putBlob: (file: TurnFile) => Promise<void>
  /** `POST /v1/turns`. */
  postTurn: (turn: OwnTurn) => Promise<void>
  /** How a provider route is named to the relay: the catalogue id of an own row (`openrouter`), else the route itself. */
  providerName?: (route: string) => string
  log?: (level: 'info' | 'warn', text: string) => void
  now?: () => number
}

/** The answer as it came, gathered from the chunks. */
interface Gathered {
  content: string
  reasoning: string
  calls: { id: string; name: string; arguments: string }[]
  usage?: TokenUsage
  finish: string
  partial: boolean
}

export class OwnTurns {
  private queue: (() => Promise<void>)[] = []
  private draining = false
  /** Uploads sent since the host started; the tests and the log read it. */
  sent = 0
  dropped = 0

  constructor(private readonly options: OwnTurnsOptions) {}

  /** The stream, let through as it is; the turn is queued once it has finished. */
  async *watch(request: GenerateOptions, stream: AsyncIterable<StreamChunk>): AsyncIterable<StreamChunk> {
    const g: Gathered = { content: '', reasoning: '', calls: [], finish: '', partial: false }
    const partials = new Map<number, { id: string; name: string; arguments: string }>()
    let finished = false
    try {
      for await (const chunk of stream) {
        gather(g, partials, chunk)
        if (chunk.type === 'finish') finished = true
        yield chunk
      }
    } finally {
      // the loop stopped reading before the finish (an abort, a thrown error): what arrived is kept as partial
      if (!finished) g.partial = true
      if (this.options.enabled()) this.enqueue(request, g)
    }
  }

  /** How many uploads are waiting. */
  get pending(): number {
    return this.queue.length
  }

  private enqueue(request: GenerateOptions, g: Gathered): void {
    if (this.queue.length >= QUEUE_MAX) {
      this.queue.shift()
      this.dropped += 1
    }
    const at = (this.options.now ?? Date.now)()
    this.queue.push(() => this.upload(request, g, at))
    void this.drain()
  }

  private async drain(): Promise<void> {
    if (this.draining) return
    this.draining = true
    try {
      while (this.queue.length) {
        const job = this.queue.shift()
        if (!job) break
        try {
          await job()
        } catch (error: unknown) {
          this.dropped += 1
          this.options.log?.('info', `nanomuse record: an own-key turn was not sent: ${error instanceof Error ? error.message : String(error)}`)
        }
      }
    } finally {
      this.draining = false
    }
  }

  /** Waits for the queue to empty; the tests and the shutdown use it. */
  async settle(): Promise<void> {
    while (this.draining || this.queue.length) await new Promise((r) => setTimeout(r, 5))
  }

  private async upload(request: GenerateOptions, g: Gathered, at: number): Promise<void> {
    const files = new Map<string, TurnFile>()
    const messages: Record<string, unknown>[] = []
    for (const m of request.messages) messages.push(await this.message(m, files))
    const provider = (this.options.providerName ?? ((r: string) => r))(request.provider)
    const turn: OwnTurn = {
      id: turnId(request, at),
      ts: Math.floor(at / 1000),
      provider: provider.slice(0, 60),
      model: request.model.slice(0, 120),
      conversation: String(request.sessionId ?? '').slice(0, 80),
      messages,
      ...(request.tools?.length ? { tools: request.tools.map(toolRow) } : {}),
      response: {
        message: assistantMessage(g),
        ...(g.usage ? { usage: { prompt_tokens: g.usage.inputTokens + (g.usage.cacheReadTokens ?? 0) + (g.usage.cacheWriteTokens ?? 0), completion_tokens: g.usage.outputTokens, cached_tokens: g.usage.cacheReadTokens ?? 0 } } : {}),
      },
      status: g.partial ? 'partial' : 'ok',
      ...(g.partial ? { ended: g.finish === 'aborted' ? 'aborted' : g.finish === 'error' ? 'error' : 'abandoned' } : {}),
    }
    if (JSON.stringify(turn.messages).length + JSON.stringify(turn.response).length > TURN_MAX_CHARS) {
      this.dropped += 1
      this.options.log?.('info', `nanomuse record: a turn of ${request.messages.length} messages is past the relay's size and was not sent`)
      return
    }
    for (const file of files.values()) {
      if (!(await this.options.hasBlob(file.sha256))) await this.options.putBlob(file)
    }
    await this.options.postTurn(turn)
    this.sent += 1
  }

  /** One message in the relay's shape: OpenAI-style roles and content parts, pictures as references. */
  private async message(m: RequestMessage, files: Map<string, TurnFile>): Promise<Record<string, unknown>> {
    const parts: Record<string, unknown>[] = []
    const calls: Record<string, unknown>[] = []
    let reasoning = ''
    for (const block of m.content as readonly ContentBlock[]) {
      if (block.type === 'text') parts.push({ type: 'text', text: block.text })
      else if (block.type === 'reasoning') reasoning += block.text
      else if (block.type === 'tool-call') calls.push({ id: block.id, type: 'function', function: { name: block.name, arguments: block.arguments } })
      else if (block.type === 'image') {
        const ref = await this.file(block.attachment, files)
        parts.push({ type: 'image_url', image_url: { url: ref ?? '' }, ...(block.offloaded ? { offloaded: true } : {}) })
      } else if (block.type === 'file') parts.push({ type: 'text', text: `[file ${block.attachment.name} ${block.attachment.bytes} bytes]` })
      else if (block.type === 'tool-addition') parts.push({ type: 'text', text: `[tool added: ${block.toolName}]` })
      else if (block.type === 'tool-removal') parts.push({ type: 'text', text: `[tool removed: ${block.toolName}]` })
    }
    const content: unknown = parts.length === 1 && parts[0]?.type === 'text' ? parts[0].text : parts
    const out: Record<string, unknown> = { role: m.role, content }
    if (reasoning) out.reasoning_content = reasoning
    if (calls.length) out.tool_calls = calls
    if (m.role === 'tool') {
      const t = m as { toolCallId?: string; isError?: boolean }
      if (t.toolCallId) out.tool_call_id = t.toolCallId
      if (t.isError) out.is_error = true
    }
    return out
  }

  /** The picture's reference, reading and hashing it once per turn; nothing when the store has no bytes for it. */
  private async file(ref: ImageAttachmentRef, files: Map<string, TurnFile>): Promise<string | undefined> {
    try {
      const { data } = await this.options.readImage(ref)
      if (!data.length) return undefined
      const sha256 = createHash('sha256').update(data).digest('hex')
      if (!files.has(sha256)) files.set(sha256, { sha256, mime: String(ref.mediaType || 'image/png'), data })
      return `${BLOB_PREFIX}${sha256}`
    } catch {
      return undefined
    }
  }
}

/** Fold one chunk into the answer under assembly. */
function gather(g: Gathered, partials: Map<number, { id: string; name: string; arguments: string }>, chunk: StreamChunk): void {
  switch (chunk.type) {
    case 'text-delta':
      g.content += chunk.text
      break
    case 'reasoning-delta':
      g.reasoning += chunk.text
      break
    case 'tool-call-delta': {
      const p = partials.get(chunk.index) ?? { id: String(chunk.id), name: '', arguments: '' }
      if (chunk.name) p.name = chunk.name
      p.arguments += chunk.argumentsDelta
      partials.set(chunk.index, p)
      break
    }
    case 'block-end':
      // the block whole is the truth; a tool call closes here with its final arguments
      if (chunk.block.type === 'tool-call') {
        partials.delete(chunk.index)
        g.calls.push({ id: String(chunk.block.id), name: chunk.block.name, arguments: chunk.block.arguments })
      }
      break
    case 'usage':
      g.usage = chunk.usage
      break
    case 'finish':
      g.finish = chunk.reason.kind
      if (chunk.reason.kind === 'error' || chunk.reason.kind === 'aborted') g.partial = true
      // calls that never closed (the stream broke) are kept with what arrived
      for (const p of partials.values()) g.calls.push(p)
      partials.clear()
      break
    default:
      break
  }
}

/** The assistant's message as the relay keeps it (OpenAI's shape). */
export function assistantMessage(g: Gathered): Record<string, unknown> {
  const finish = g.finish === 'tool-calls' ? 'tool_calls' : g.finish === 'max-tokens' ? 'length' : g.finish === 'stop' ? 'stop' : g.finish || 'stop'
  return {
    role: 'assistant',
    content: g.content,
    ...(g.reasoning ? { reasoning_content: g.reasoning } : {}),
    ...(g.calls.length ? { tool_calls: g.calls.map((c) => ({ id: c.id, type: 'function', function: { name: c.name, arguments: c.arguments } })) } : {}),
    finish_reason: finish,
  }
}

/** A tool as OpenAI's `tools[]` carries it. */
export function toolRow(t: ToolSchema): Record<string, unknown> {
  return { type: 'function', function: { name: t.name, description: t.description, parameters: t.parameters } }
}

/** A stable id for the turn: the session, the time and a hash of the last message, so a retry does not make two. */
export function turnId(request: GenerateOptions, at: number): string {
  const last = request.messages[request.messages.length - 1]
  const h = createHash('sha256').update(`${request.sessionId ?? ''}|${request.model}|${last ? JSON.stringify(last.content) : ''}`).digest('hex').slice(0, 16)
  return `dsh-${Math.floor(at / 1000)}-${h}`
}
