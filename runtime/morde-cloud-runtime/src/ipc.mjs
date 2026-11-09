import { createHash, randomUUID } from 'node:crypto'
import { once } from 'node:events'
import { textOnlyImageText } from '@deepseek-ai/dsh-llm'
import { validMessageDelivery } from './engine.mjs'

export const MAX_FRAME_BYTES = 4 * 1024 * 1024
const MAX_MODEL_QUEUE_BYTES = 1024 * 1024
const MAX_TOOL_OUTPUT_BYTES = 1024 * 1024
const MAX_TOOL_IMAGE_BYTES = 512 * 1024
const MAX_TOOL_IMAGES = 4
const MAX_TASK_ATTACHMENT_IMAGE_BYTES = 2 * 1024 * 1024
// shortcut: retain at most 8 MiB for task recovery; use durable bounded attachments if tasks need longer image histories.
const MAX_TOOL_IMAGE_CACHE_BYTES = 8 * 1024 * 1024
const CHECKPOINT_TIMEOUT_MS = 30_000
const TOOL_RESULT_TIMEOUT_MS = 6 * 60_000
const CHILD_RESULT_TIMEOUT_MS = 6 * 60_000
const UTF8_DECODER = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true })

function deferred() {
  let resolve, reject
  const promise = new Promise((ok, fail) => { resolve = ok; reject = fail })
  return { promise, resolve, reject }
}

function abortError() {
  return new Error('request cancelled')
}

function isObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

function jpegDimensions(bytes) {
  if (bytes.length < 4 || bytes[0] !== 0xff || bytes[1] !== 0xd8 || bytes.at(-2) !== 0xff || bytes.at(-1) !== 0xd9) return undefined
  const frameMarkers = new Set([0xc0, 0xc1, 0xc2, 0xc3, 0xc5, 0xc6, 0xc7, 0xc9, 0xca, 0xcb, 0xcd, 0xce, 0xcf])
  let offset = 2
  while (offset < bytes.length - 2) {
    if (bytes[offset++] !== 0xff) return undefined
    while (bytes[offset] === 0xff) offset++
    const marker = bytes[offset++]
    if (marker === 0xda || marker === 0xd9 || marker === 0x00) return undefined
    if (marker === 0x01 || (marker >= 0xd0 && marker <= 0xd7)) continue
    if (offset + 2 > bytes.length - 2) return undefined
    const length = bytes.readUInt16BE(offset)
    if (length < 2 || offset + length > bytes.length - 2) return undefined
    if (frameMarkers.has(marker)) {
      if (length < 8) return undefined
      return { height: bytes.readUInt16BE(offset + 3), width: bytes.readUInt16BE(offset + 5) }
    }
    offset += length
  }
  return undefined
}

function pngDimensions(bytes) {
  const signature = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])
  if (bytes.length < 33 || !bytes.subarray(0, 8).equals(signature) || bytes.readUInt32BE(8) !== 13 ||
      bytes.toString('ascii', 12, 16) !== 'IHDR') return undefined
  return { width: bytes.readUInt32BE(16), height: bytes.readUInt32BE(20) }
}

function imageDimensions(mediaType, bytes) {
  if (mediaType === 'image/jpeg') return jpegDimensions(bytes)
  if (mediaType === 'image/png') return pngDimensions(bytes)
  return undefined
}

function toolImageLimit(name) {
  if (name === 'morde_read_task_attachment') return MAX_TOOL_IMAGES
  if (typeof name === 'string' && (name.startsWith('mcp_') || name === 'desktop_screenshot' || name === 'browser_screenshot')) return 1
  return 0
}

function decodeToolImages(images, toolName) {
  const limit = toolImageLimit(toolName)
  if (!Array.isArray(images) || images.length > limit) throw new Error('invalid tool images')
  let totalBytes = 0
  const decoded = images.map(image => {
    if (!isObject(image) || Object.keys(image).sort().join(',') !== 'attachment_id,bytes,data_base64,height,media_type,width' ||
        !['image/jpeg', 'image/png'].includes(image.media_type) || !Number.isSafeInteger(image.bytes) || image.bytes < 1 || image.bytes > MAX_TOOL_IMAGE_BYTES ||
        !Number.isSafeInteger(image.width) || image.width < 1 || image.width > 1280 ||
        !Number.isSafeInteger(image.height) || image.height < 1 || image.height > 720 ||
        typeof image.data_base64 !== 'string' || image.data_base64.length !== Math.ceil(image.bytes / 3) * 4 ||
        !/^([A-Za-z0-9+/]{4})*([A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(image.data_base64)) throw new Error('invalid tool image metadata')
    const bytes = Buffer.from(image.data_base64, 'base64')
    if (bytes.length !== image.bytes || bytes.toString('base64') !== image.data_base64 ||
        image.attachment_id !== `morde_img_${createHash('sha256').update(bytes).digest('hex')}`) throw new Error('invalid tool image bytes')
    const dimensions = imageDimensions(image.media_type, bytes)
    if (!dimensions || dimensions.width !== image.width || dimensions.height !== image.height) throw new Error('invalid tool image dimensions')
    totalBytes += bytes.length
    const attachment = {
      attachmentId: image.attachment_id,
      mediaType: image.media_type,
      bytes: image.bytes,
      width: image.width,
      height: image.height,
    }
    return { attachment, bytes }
  })
  if (toolName === 'morde_read_task_attachment' && totalBytes > MAX_TASK_ATTACHMENT_IMAGE_BYTES) {
    throw new Error('task attachment images exceed the aggregate size limit')
  }
  return decoded
}

function modelRequestWithImages(request, images) {
  let imageBytes = 0
  const messages = request.messages.map(message => ({
    ...message,
    content: message.content.map(block => {
      if (block.type !== 'image') return block
      const ref = block.attachment
      if (!isObject(ref) || Object.keys(ref).sort().join(',') !== 'attachmentId,bytes,height,mediaType,width' ||
          typeof ref.attachmentId !== 'string' || !/^morde_img_[a-f0-9]{64}$/.test(ref.attachmentId) ||
          !['image/jpeg', 'image/png'].includes(ref.mediaType) || !Number.isSafeInteger(ref.bytes) || ref.bytes < 1 || ref.bytes > MAX_TOOL_IMAGE_BYTES ||
          !Number.isSafeInteger(ref.width) || ref.width < 1 || ref.width > 1280 ||
          !Number.isSafeInteger(ref.height) || ref.height < 1 || ref.height > 720) throw new Error('invalid model image reference')
      if (block.offloaded === true) return { type: 'text', text: textOnlyImageText(ref) }
      const stored = images.get(ref.attachmentId)
      if (!stored || Object.keys(ref).some(key => stored.attachment[key] !== ref[key])) throw new Error('image attachment unavailable')
      imageBytes += stored.bytes.length
      if (imageBytes > MAX_FRAME_BYTES / 2) throw new Error('model images exceed frame limit')
      return { ...block, attachment: { ...ref, data_base64: stored.bytes.toString('base64') } }
    }),
  }))
  return { ...request, messages }
}

export class WorkerIPC {
  constructor(input, output) {
    this.input = input
    this.output = output
    this.start = deferred()
    this.pending = new Map()
    this.childPending = new Map()
    this.images = new Map()
    this.imageCacheBytes = 0
    this.models = new Map()
    this.cancelledModels = new Map()
    this.messageDeliveries = new Map()
    this.pendingMessageDeliveries = []
    this.messageDeliveryHandler = undefined
    this.messageDeliveryClosed = false
    this.failed = false
    this.started = false
    this.cancelled = false
    this.buffer = Buffer.alloc(0)
    this.writes = Promise.resolve()
    input.on('data', chunk => this.receive(chunk))
    input.on('end', () => this.fail('worker connection closed'))
    input.on('error', () => this.fail('worker connection failed'))
    output.on('error', () => this.fail('worker connection failed'))
  }

  async send(frame) {
    if (this.failed) throw new Error('worker connection unavailable')
    const encoded = Buffer.from(`${JSON.stringify(frame)}\n`)
    if (encoded.length > MAX_FRAME_BYTES) throw new Error('worker frame exceeds limit')
    const writing = this.writes.then(async () => {
      if (this.failed) throw new Error('worker connection unavailable')
      if (!this.output.write(encoded)) await once(this.output, 'drain')
    })
    this.writes = writing.catch(() => {})
    return writing
  }

  receive(chunk) {
    if (this.failed) return
    try {
      const incoming = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk)
      this.buffer = Buffer.concat([this.buffer, incoming])
      for (;;) {
        const end = this.buffer.indexOf(10)
        if (end === -1) {
          if (this.buffer.length >= MAX_FRAME_BYTES) throw new Error('worker frame exceeds limit')
          break
        }
        if (end >= MAX_FRAME_BYTES) throw new Error('worker frame exceeds limit')
        const line = this.buffer.subarray(0, end)
        this.buffer = this.buffer.subarray(end + 1)
        const frame = JSON.parse(UTF8_DECODER.decode(line))
        if (!isObject(frame) || typeof frame.type !== 'string') throw new Error('invalid worker frame')
        this.dispatch(frame)
      }
    } catch {
      this.fail('invalid worker frame')
    }
  }

  dispatch(frame) {
    if (frame.type === 'start' && !this.started) {
      this.started = true
      this.startFrame = frame
      if (frame.initial_message) {
        this.messageDeliveries.set(frame.initial_message.message_id,
          JSON.stringify([frame.initial_message.content, frame.initial_message.attachment_ids]))
      }
      if (Array.isArray(frame.revision_messages)) {
        for (const message of frame.revision_messages) {
          this.messageDeliveries.set(message.message_id, JSON.stringify([message.content, message.attachment_ids]))
        }
      }
      this.start.resolve(frame)
      return
    }
    if (!this.started) throw new Error('runtime has not started')
    if (frame.type === 'cancel') {
      this.cancel()
      return
    }
    if (frame.type === 'message_delivery') {
      if (this.cancelled || this.messageDeliveryClosed || !validMessageDelivery(frame, this.startFrame)) {
        throw new Error('invalid message delivery')
      }
      const attachmentIDs = frame.attachment_ids ?? []
      const signature = JSON.stringify([frame.content, attachmentIDs])
      const previous = this.messageDeliveries.get(frame.message_id)
      if (previous !== undefined) {
        if (previous !== signature) throw new Error('message delivery id conflicts with prior content')
        return
      }
      this.messageDeliveries.set(frame.message_id, signature)
      const delivery = { message_id: frame.message_id, content: frame.content, attachment_ids: [...attachmentIDs] }
      if (this.messageDeliveryHandler) this.messageDeliveryHandler(delivery)
      else this.pendingMessageDeliveries.push(delivery)
      return
    }
    if (frame.type === 'job_cancel') {
      const keys = ['agent_id', 'authorization_version', 'generation', 'id', 'job_id', 'owner_id', 'session_id', 'task_id', 'type']
      if (Object.keys(frame).sort().join(',') !== keys.sort().join(',') ||
          !Object.entries(Object.fromEntries(['task_id', 'owner_id', 'agent_id', 'session_id', 'generation', 'authorization_version'].map(key => [key, this.startFrame[key]])))
            .every(([key, value]) => frame[key] === value) || typeof frame.id !== 'string' || !frame.id ||
          typeof frame.job_id !== 'string' || !frame.job_id) throw new Error('invalid job cancellation request')
      if (!this.onJobCancel) throw new Error('job cancellation is unavailable')
      Promise.resolve(this.onJobCancel(frame.job_id)).then(status => {
        if (!['cancelled', 'succeeded', 'failed', 'unknown'].includes(status)) throw new Error('invalid job cancellation status')
        return this.send({ type: 'job_cancel_result', ...Object.fromEntries(keys.filter(key => key !== 'type' && key !== 'id' && key !== 'job_id').map(key => [key, this.startFrame[key]])),
          id: frame.id, job_id: frame.job_id, status })
      }).catch(() => this.fail('job cancellation failed'))
      return
    }
    if (frame.type === 'checkpoint_ack') {
      const pending = this.pending.get(frame.id)
      if (!pending || pending.kind !== 'checkpoint_ack') throw new Error('unknown checkpoint response')
      this.pending.delete(frame.id)
      clearTimeout(pending.timer)
      if (frame.ok !== true || frame.through_seq !== pending.through || frame.sha256 !== pending.digest ||
          !Object.entries(pending.identity).every(([key, value]) => frame[key] === value)) {
        pending.reject(new Error('checkpoint not confirmed'))
      } else {
        pending.resolve()
      }
      return
    }
    if (frame.type === 'checkpoint_page') {
      const pending = this.pending.get(frame.id)
      if (!pending || pending.kind !== 'checkpoint_page') throw new Error('unknown checkpoint page')
      this.pending.delete(frame.id)
      clearTimeout(pending.timer)
      if (frame.ok !== true || !Object.entries(pending.identity).every(([key, value]) => frame[key] === value)) {
        pending.reject(new Error('checkpoint page not authorized'))
      } else {
        pending.resolve(frame)
      }
      return
    }
    if (frame.type === 'tool_result') {
      const pending = this.pending.get(frame.id)
      if (!pending || pending.kind !== 'tool_result') throw new Error('unknown tool response')
      this.pending.delete(frame.id)
      clearTimeout(pending.timer)
      const expectedKeys = ['agent_id', 'authorization_version', 'call_id', 'generation', 'id', 'output', 'owner_id', 'session_id', 'status', 'task_id', 'type']
      const actualKeys = Object.keys(frame).sort()
      let images
      try { images = decodeToolImages(Object.hasOwn(frame, 'images') ? frame.images : [], pending.name) } catch {}
      const actualKeyList = actualKeys.join(',')
      const validKeys = actualKeyList === expectedKeys.join(',') || actualKeyList === [...expectedKeys, 'images'].sort().join(',')
      const addedBytes = images?.reduce((total, item) => total + (this.images.has(item.attachment.attachmentId) ? 0 : item.bytes.length), 0)
      const valid = validKeys && images !== undefined && addedBytes !== undefined && this.imageCacheBytes + addedBytes <= MAX_TOOL_IMAGE_CACHE_BYTES &&
        Object.entries(pending.identity).every(([key, value]) => frame[key] === value) &&
        frame.call_id === pending.callID && ['succeeded', 'failed'].includes(frame.status) &&
        typeof frame.output === 'string' && Buffer.byteLength(frame.output) <= MAX_TOOL_OUTPUT_BYTES
      if (!valid) {
        pending.reject(new Error('tool result not authorized'))
        throw new Error('invalid tool response')
      }
      for (const item of images) {
        const id = item.attachment.attachmentId
        if (!this.images.has(id)) {
          this.images.set(id, item)
          this.imageCacheBytes += item.bytes.length
        }
      }
      pending.resolve({ status: frame.status, output: frame.output, images: images.map(({ attachment }) => attachment) })
      return
    }
    if (frame.type === 'child_session') {
      const pending = this.childPending.get(frame.call_id)
      if (!pending || pending.kind !== 'child_session') throw new Error('unknown child session response')
      this.childPending.delete(frame.call_id)
      clearTimeout(pending.timer)
      pending.resolve(frame)
      return
    }
    if (frame.type === 'child_settle_ack') {
      const pending = this.childPending.get(frame.call_id)
      if (!pending || pending.kind !== 'child_settle_ack') throw new Error('unknown child settlement response')
      this.childPending.delete(frame.call_id)
      clearTimeout(pending.timer)
      if (frame.ok !== true || !Object.entries(pending.identity).every(([key, value]) => frame[key] === value)) {
        pending.reject(new Error('child settlement was not confirmed'))
        throw new Error('invalid child settlement response')
      }
      pending.resolve()
      return
    }
    if (frame.type === 'model_chunk' || frame.type === 'model_end') {
      const cancelledIdentity = this.cancelledModels.get(frame.id)
      if (cancelledIdentity) {
        if (!Object.entries(cancelledIdentity).every(([key, value]) => frame[key] === value)) throw new Error('invalid model response identity')
        if (frame.type === 'model_end') this.cancelledModels.delete(frame.id)
        return
      }
      const model = this.models.get(frame.id)
      if (!model) throw new Error('unknown model response')
      if (!Object.entries(model.identity).every(([key, value]) => frame[key] === value)) throw new Error('invalid model response identity')
      model.receive(frame)
      return
    }
    throw new Error('unknown worker message')
  }

  setMessageDeliveryHandler(handler) {
    if (this.messageDeliveryClosed || this.messageDeliveryHandler || typeof handler !== 'function') {
      throw new Error('message delivery handler is unavailable')
    }
    this.messageDeliveryHandler = handler
    for (const delivery of this.pendingMessageDeliveries.splice(0)) handler(delivery)
  }

  closeMessageDelivery() {
    this.messageDeliveryClosed = true
    this.messageDeliveryHandler = undefined
    this.pendingMessageDeliveries = []
  }

  cancel() {
    this.cancelled = true
    for (const [id, pending] of this.pending) {
      if (pending.kind !== 'checkpoint_page' && pending.kind !== 'tool_result') continue
      this.pending.delete(id)
      clearTimeout(pending.timer)
      pending.reject(abortError())
    }
    for (const [callID, pending] of this.childPending) {
      this.childPending.delete(callID)
      clearTimeout(pending.timer)
      pending.reject(abortError())
    }
    this.onCancel?.()
  }

  async checkpoint(identity, from, through, payload, digest) {
    const id = randomUUID()
    const result = deferred()
    const timer = setTimeout(() => {
      this.pending.delete(id)
      result.reject(new Error('checkpoint confirmation timed out'))
    }, CHECKPOINT_TIMEOUT_MS)
    this.pending.set(id, { ...result, timer, kind: 'checkpoint_ack', through, digest, identity: { ...identity } })
    try {
      await this.send({ type: 'checkpoint', id, ...identity, from_seq: from, through_seq: through, payload, sha256: digest })
      await result.promise
    } finally {
      clearTimeout(timer)
      this.pending.delete(id)
    }
  }

  async readCheckpoint(identity, from) {
    if (this.cancelled) throw abortError()
    if (!Number.isSafeInteger(from) || from < 0) throw new Error('invalid checkpoint offset')
    const id = randomUUID()
    const result = deferred()
    void result.promise.catch(() => {})
    const timer = setTimeout(() => {
      this.pending.delete(id)
      result.reject(new Error('checkpoint read timed out'))
    }, CHECKPOINT_TIMEOUT_MS)
    this.pending.set(id, { ...result, timer, kind: 'checkpoint_page', identity })
    try {
      await this.send({ type: 'checkpoint_read', id, ...identity, from_seq: from })
      return await result.promise
    } finally {
      clearTimeout(timer)
      this.pending.delete(id)
    }
  }

  async toolCall(identity, through, callID, name, args, signal) {
    if (this.cancelled) throw abortError()
    if (!Number.isSafeInteger(through) || through < 0 || typeof callID !== 'string' || !callID ||
        Buffer.byteLength(callID) > 256 || typeof name !== 'string' || !name || !isObject(args)) {
      throw new Error('invalid tool call')
    }
    signal?.throwIfAborted()
    const id = randomUUID()
    const result = deferred()
    void result.promise.catch(() => {})
    const timer = setTimeout(() => {
      this.pending.delete(id)
      result.reject(new Error('tool result timed out'))
    }, TOOL_RESULT_TIMEOUT_MS)
    const abort = () => {
      this.pending.delete(id)
      clearTimeout(timer)
      result.reject(abortError())
    }
    this.pending.set(id, { ...result, timer, kind: 'tool_result', identity, callID, name })
    signal?.addEventListener('abort', abort, { once: true })
    try {
      await this.send({ type: 'tool_call', id, ...identity, through_seq: through, call_id: callID, name, arguments: args })
      return await result.promise
    } catch (error) {
      if (this.pending.delete(id)) clearTimeout(timer)
      throw error
    } finally {
      signal?.removeEventListener('abort', abort)
      if (this.pending.delete(id)) clearTimeout(timer)
    }
  }

  async createChild(identity, through, callID, args, signal) {
    if (this.cancelled) throw abortError()
    if (!Number.isSafeInteger(through) || through < 0 || typeof callID !== 'string' || !callID ||
        Buffer.byteLength(callID) > 256 || !isObject(args) || this.childPending.has(callID)) {
      throw new Error('invalid child session request')
    }
    signal?.throwIfAborted()
    const result = deferred()
    void result.promise.catch(() => {})
    const timer = setTimeout(() => {
      this.childPending.delete(callID)
      result.reject(new Error('child session response timed out'))
    }, CHILD_RESULT_TIMEOUT_MS)
    const abort = () => {
      this.childPending.delete(callID)
      clearTimeout(timer)
      result.reject(abortError())
    }
    this.childPending.set(callID, { ...result, timer, kind: 'child_session' })
    signal?.addEventListener('abort', abort, { once: true })
    try {
      await this.send({ type: 'child_create', ...identity, through_seq: through, call_id: callID, arguments: args })
      return await result.promise
    } catch (error) {
      if (this.childPending.delete(callID)) clearTimeout(timer)
      throw error
    } finally {
      signal?.removeEventListener('abort', abort)
      if (this.childPending.delete(callID)) clearTimeout(timer)
    }
  }

  async settleChild(identity, callID, status, through, digest, summary, signal) {
    if (this.cancelled) throw abortError()
    if (typeof callID !== 'string' || !callID || Buffer.byteLength(callID) > 256 ||
        !['succeeded', 'failed'].includes(status) || !Number.isSafeInteger(through) || through < -1 ||
        (through === -1 ? digest !== '' : typeof digest !== 'string' || !/^[a-f0-9]{64}$/.test(digest)) ||
        typeof summary !== 'string' || Buffer.byteLength(summary) > MAX_TOOL_OUTPUT_BYTES || this.childPending.has(callID)) {
      throw new Error('invalid child settlement')
    }
    signal?.throwIfAborted()
    const result = deferred()
    void result.promise.catch(() => {})
    const timer = setTimeout(() => {
      this.childPending.delete(callID)
      result.reject(new Error('child settlement response timed out'))
    }, CHILD_RESULT_TIMEOUT_MS)
    const abort = () => {
      this.childPending.delete(callID)
      clearTimeout(timer)
      result.reject(abortError())
    }
    this.childPending.set(callID, { ...result, timer, kind: 'child_settle_ack', identity: { ...identity } })
    signal?.addEventListener('abort', abort, { once: true })
    try {
      await this.send({
        type: 'child_settle', ...identity, call_id: callID, status,
        through_seq: through, sha256: digest, summary,
      })
      await result.promise
    } catch (error) {
      if (this.childPending.delete(callID)) clearTimeout(timer)
      throw error
    } finally {
      signal?.removeEventListener('abort', abort)
      if (this.childPending.delete(callID)) clearTimeout(timer)
    }
  }

  async *streamModel(identity, id, through, request, signal) {
    if (this.models.size >= 17 || this.models.has(id) || this.cancelledModels.has(id)) throw new Error('too many active model requests')
    signal?.throwIfAborted()
    const queue = []
    let queuedBytes = 0
    let nextIndex = 0
    let finished = false
    let ended = false
    let cancelSent = false
    let failure
    let wake = deferred()
    const notify = () => { wake.resolve(); wake = deferred() }
    const notifyCancel = () => {
      if (cancelSent) return
      cancelSent = true
      this.cancelledModels.set(id, { ...identity })
      void this.send({ type: 'model_cancel', id, ...identity }).catch(() => {})
    }
    const receive = frame => {
      if (ended) throw new Error('model response already ended')
      if (frame.type === 'model_end') {
        ended = true
        if (frame.ok !== true || !finished) failure = new Error('model request failed')
        notify()
        return
      }
      if (frame.index !== nextIndex++ || !isObject(frame.chunk) || typeof frame.chunk.type !== 'string' || finished) {
        throw new Error('invalid model response')
      }
      const chunk = frame.chunk
      const allowed = ['block-start', 'text-delta', 'reasoning-delta', 'tool-call-delta', 'block-end', 'usage', 'finish']
      if (!allowed.includes(chunk.type) || chunk.blockType === 'tool-call' || chunk.block?.type === 'tool-call') {
        failure = new Error('model returned an unavailable tool')
        ended = true
        notifyCancel()
        notify()
        return
      }
      if (chunk.type === 'tool-call-delta' && !request.tools.length) {
        failure = new Error('model returned an unavailable tool')
        ended = true
        notifyCancel()
        notify()
        return
      }
      if (chunk.type === 'finish') finished = true
      const size = Buffer.byteLength(JSON.stringify(chunk))
      if (queuedBytes + size > MAX_MODEL_QUEUE_BYTES) throw new Error('model response exceeds queue limit')
      queuedBytes += size
      queue.push({ chunk, size })
      notify()
    }
    const cancel = () => {
      if (!ended) {
        ended = true
        failure = abortError()
        notifyCancel()
        notify()
      }
    }
    this.models.set(id, { receive, cancel, identity: { ...identity } })
    signal?.addEventListener('abort', cancel, { once: true })
    try {
      request = modelRequestWithImages(request, this.images)
      const digest = createHash('sha256').update(JSON.stringify(request), 'utf8').digest('hex')
      await this.send({ type: 'model', id, ...identity, through_seq: through, request, sha256: digest })
      for (;;) {
        signal?.throwIfAborted()
        if (queue.length) {
          const item = queue.shift()
          queuedBytes -= item.size
          yield item.chunk
          continue
        }
        if (failure) throw failure
        if (ended) return
        await wake.promise
      }
    } finally {
      signal?.removeEventListener('abort', cancel)
      if (!ended) cancel()
      this.models.delete(id)
    }
  }

  fail(reason) {
    if (this.failed) return
    this.failed = true
    this.closeMessageDelivery()
    this.start.reject(new Error(reason))
    for (const pending of this.pending.values()) {
      clearTimeout(pending.timer)
      pending.reject(new Error(reason))
    }
    this.pending.clear()
    for (const pending of this.childPending.values()) {
      clearTimeout(pending.timer)
      pending.reject(new Error(reason))
    }
    this.childPending.clear()
    for (const model of this.models.values()) model.cancel()
    this.onFailure?.()
  }
}
