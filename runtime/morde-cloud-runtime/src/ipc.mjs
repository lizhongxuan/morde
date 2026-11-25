import { createHash, randomUUID } from 'node:crypto'
import { once } from 'node:events'
import { LlmError, textOnlyImageText } from '@deepseek-ai/dsh-llm'
import {
  coordinatorCompactionModelCallID, coordinatorModelCallID, MODEL_FAILURE_SUMMARIES, TASK_VALIDATION_REVIEW_IDENTITY_FIELDS,
  taskValidationReviewModelCallID, validDelegateSelection, validLateChildTools, validMessageDelivery,
} from './engine.mjs'

export const MAX_FRAME_BYTES = 4 * 1024 * 1024
const MAX_MODEL_QUEUE_BYTES = 1024 * 1024
const MAX_TOOL_OUTPUT_BYTES = 1024 * 1024
const MAX_TOOL_IMAGE_BYTES = 512 * 1024
const MAX_TOOL_IMAGES = 4
const MAX_TASK_ATTACHMENT_IMAGE_BYTES = 2 * 1024 * 1024
// shortcut: retain at most 8 MiB for task recovery; use durable bounded attachments if tasks need longer image histories.
const MAX_TOOL_IMAGE_CACHE_BYTES = 8 * 1024 * 1024
// shortcut: keep a bounded window for late backend replies; expired tombstones fail closed when their reply arrives.
const MAX_ABANDONED_RESPONSES = 4096
const MAX_DELEGATE_TITLE_BYTES = 256
const MAX_DELEGATE_OBJECTIVE_BYTES = 32 * 1024
const TOOL_NAME = /^[A-Za-z][A-Za-z0-9_.-]{0,127}$/
const CHECKPOINT_TIMEOUT_MS = 30_000
const TOOL_RESULT_TIMEOUT_MS = 6 * 60_000
const CHILD_RESULT_TIMEOUT_MS = 6 * 60_000
const RECOVERY_TOOL_SUPERSEDED = Symbol.for('morde-cloud-runtime.recovery-tool-superseded.v1')
const UTF8_DECODER = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true })
const COORDINATOR_IDENTITY_FIELDS = ['context_role', 'conversation_id', 'message_id', 'owner_id', 'actor_id', 'agent_id', 'generation', 'authorization_version', 'session_id', 'purpose', 'operation_id']

function deferred() {
  let resolve, reject
  const promise = new Promise((ok, fail) => { resolve = ok; reject = fail })
  return { promise, resolve, reject }
}

function notifyIPCWaiting(onWaiting) {
  if (typeof onWaiting !== 'function') return
  try {
    void Promise.resolve(onWaiting()).catch(() => {})
  } catch {
    // Waiting telemetry cannot discard a request that has already been sent.
  }
}

function notifyIPCTimeout(onTimeout, requestKind, requestDigest) {
  if (typeof onTimeout !== 'function' || !/^[a-f0-9]{64}$/.test(requestDigest ?? '')) return
  try {
    const notification = onTimeout({ request_kind: requestKind, request_digest: requestDigest })
    void Promise.resolve(notification).catch(() => {})
  } catch {
    // Timeout telemetry is best-effort and cannot change the timed-out request.
  }
}

function sha256(value) {
  return createHash('sha256').update(value, 'utf8').digest('hex')
}

function trimGoSpace(value) {
  return value.replace(/^[\u0009-\u000d\u0020\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+/, '')
    .replace(/[\u0009-\u000d\u0020\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+$/, '')
}

function abortError() {
  return new Error('request cancelled')
}

function isObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

function exactKeys(value, expected) {
  return Object.keys(value).sort().join(',') === [...expected].sort().join(',')
}

function digestNames(names) {
  return createHash('sha256').update(JSON.stringify([...names].sort())).digest('hex')
}

function childPendingKey(kind, sessionID, callID) {
  return JSON.stringify([kind, sessionID, callID])
}

function validModelEndFrame(frame, identity) {
  const fence = identity.context_role === 'coordinator'
    ? COORDINATOR_IDENTITY_FIELDS
    : identity.context_role === 'task_validation_review'
      ? TASK_VALIDATION_REVIEW_IDENTITY_FIELDS
      : ['task_id', 'owner_id', 'agent_id', 'generation', 'authorization_version', 'session_id']
  if (typeof frame.ok !== 'boolean') return false
  if (frame.ok === true || !Object.hasOwn(frame, 'failure_code')) {
    return exactKeys(frame, ['type', ...fence, 'id', 'ok'])
  }
  return frame.ok === false && typeof frame.failure_code === 'string' && Object.hasOwn(MODEL_FAILURE_SUMMARIES, frame.failure_code) &&
    exactKeys(frame, ['type', ...fence, 'id', 'ok', 'failure_code'])
}

function matchesCoordinatorIdentity(frame, identity) {
  return identity?.context_role === 'coordinator' && COORDINATOR_IDENTITY_FIELDS.every(key => frame[key] === identity[key])
}

function matchesTaskValidationReviewIdentity(frame, identity) {
  return identity?.context_role === 'task_validation_review' &&
    TASK_VALIDATION_REVIEW_IDENTITY_FIELDS.every(key => frame[key] === identity[key])
}

function matchesIdentity(frame, identity) {
  return Object.entries(identity).every(([key, value]) => frame[key] === value)
}

function validAbandonedCheckpointAck(frame, id, pending) {
  return exactKeys(frame, ['type', ...Object.keys(pending.identity), 'id', 'ok', 'through_seq', 'sha256']) &&
    frame.id === id && typeof frame.ok === 'boolean' && frame.through_seq === pending.through &&
    frame.sha256 === pending.digest && matchesIdentity(frame, pending.identity)
}

function validAbandonedCheckpointPage(frame, id, pending) {
  return exactKeys(frame, ['type', ...Object.keys(pending.identity), 'id', 'ok', 'from_seq', 'through_seq', 'payload', 'sha256']) &&
    frame.id === id && typeof frame.ok === 'boolean' && frame.from_seq === pending.from &&
    Number.isSafeInteger(frame.through_seq) && frame.through_seq >= frame.from_seq &&
    typeof frame.payload === 'string' && Buffer.byteLength(frame.payload) <= MAX_FRAME_BYTES / 2 &&
    /^[a-f0-9]{64}$/.test(frame.sha256) && createHash('sha256').update(frame.payload).digest('hex') === frame.sha256 &&
    matchesIdentity(frame, pending.identity)
}

function validAbandonedChildSession(frame, callID, pending) {
  const keys = ['type', ...Object.keys(pending.identity), 'call_id', 'parent_session_id', 'objective', 'tools',
    'checkpoint_through_seq', 'checkpoint_sha256', 'step_status', 'summary']
  const hasSettledOutput = Object.hasOwn(frame, 'settled_output')
  const parent = pending.identity
  return exactKeys(frame, hasSettledOutput ? [...keys, 'settled_output'] : keys) && frame.call_id === callID &&
    frame.task_id === parent.task_id && frame.owner_id === parent.owner_id && frame.agent_id === parent.agent_id &&
    frame.generation === parent.generation && frame.authorization_version === parent.authorization_version &&
    typeof frame.session_id === 'string' && /^[A-Za-z0-9_-]{1,128}$/.test(frame.session_id) &&
    frame.session_id !== parent.session_id && frame.parent_session_id === parent.session_id &&
    typeof frame.objective === 'string' && createHash('sha256').update(frame.objective).digest('hex') === pending.objectiveDigest &&
    validLateChildTools(frame.tools, pending.toolNamesDigest, pending.allowControls) &&
    Number.isSafeInteger(frame.checkpoint_through_seq) && frame.checkpoint_through_seq >= -1 &&
    (frame.checkpoint_through_seq === -1 ? frame.checkpoint_sha256 === '' :
      typeof frame.checkpoint_sha256 === 'string' && /^[a-f0-9]{64}$/.test(frame.checkpoint_sha256)) &&
    ['running', 'succeeded', 'failed', 'unknown'].includes(frame.step_status) &&
    typeof frame.summary === 'string' && Buffer.byteLength(frame.summary) <= MAX_TOOL_OUTPUT_BYTES &&
    (!hasSettledOutput || pending.runInBackground !== true &&
      ['succeeded', 'failed'].includes(frame.step_status) && typeof frame.settled_output === 'string' &&
      Buffer.byteLength(frame.settled_output) <= MAX_TOOL_OUTPUT_BYTES)
}

function validAbandonedChildSettleAck(frame, callID, pending) {
  const keys = ['type', ...Object.keys(pending.identity), 'call_id', 'ok', 'status', 'summary']
  const hasOutput = Object.hasOwn(frame, 'output')
  return exactKeys(frame, hasOutput ? [...keys, 'output'] : keys) && frame.call_id === callID && frame.ok === true &&
    ['succeeded', 'failed'].includes(frame.status) && frame.status === pending.status &&
    typeof frame.summary === 'string' && Buffer.byteLength(frame.summary) <= 2048 &&
    (!hasOutput || typeof frame.output === 'string' && Buffer.byteLength(frame.output) <= MAX_TOOL_OUTPUT_BYTES) &&
    matchesIdentity(frame, pending.identity)
}

function validTaskValidationReviewIdentity(identity) {
  return isObject(identity) && exactKeys(identity, TASK_VALIDATION_REVIEW_IDENTITY_FIELDS) &&
    identity.context_role === 'task_validation_review' && identity.purpose === 'task_validation_review' &&
    ['owner_id', 'actor_id', 'task_id', 'session_id'].every(key => typeof identity[key] === 'string' && /^[A-Za-z0-9_-]{1,128}$/.test(identity[key])) &&
    Number.isSafeInteger(identity.task_revision) && identity.task_revision >= 0 &&
    Number.isSafeInteger(identity.result_event_sequence) && identity.result_event_sequence > 0 &&
    typeof identity.result_sha256 === 'string' && /^[a-f0-9]{64}$/.test(identity.result_sha256) &&
    Number.isSafeInteger(identity.claim_generation) && identity.claim_generation > 0 &&
    typeof identity.lease_owner === 'string' && identity.lease_owner.length > 0 && identity.lease_owner.trim() === identity.lease_owner &&
    Buffer.byteLength(identity.lease_owner, 'utf8') <= 128
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

export function decodeToolImages(images, toolName) {
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

  abandonPending(map, id, pending) {
    if (map.get(id) !== pending) return
    const value = { kind: pending.kind, identity: { ...pending.identity }, abandoned: true }
    if (pending.kind === 'checkpoint_ack') Object.assign(value, { through: pending.through, digest: pending.digest })
    if (pending.kind === 'checkpoint_page') Object.assign(value, { from: pending.from })
    if (pending.kind === 'tool_result') Object.assign(value, { callID: pending.callID, name: pending.name })
    if (pending.kind === 'child_session') Object.assign(value, {
      callID: pending.callID,
      objectiveDigest: createHash('sha256').update(pending.arguments.objective).digest('hex'),
      toolNamesDigest: digestNames(pending.arguments.tool_names),
      runInBackground: pending.arguments.run_in_background,
      allowControls: pending.identity.session_id === this.startFrame?.session_id &&
        this.startFrame.tools?.some(tool => tool.name === 'dsh_delegate') === true,
    })
    if (pending.kind === 'child_settle_ack') Object.assign(value, { callID: pending.callID, status: pending.status })
    map.set(id, value)
    let retained = 0
    for (const [key, entry] of map) {
      if (!entry.abandoned) continue
      if (++retained > MAX_ABANDONED_RESPONSES) map.delete(key)
    }
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
          { signature: JSON.stringify([frame.initial_message.content, frame.initial_message.attachment_ids ?? [], frame.initial_message.mode ?? 'followup']),
            supersedeActive: false, supersedeTargets: new Map() })
      }
      if (Array.isArray(frame.revision_messages)) {
        for (const message of frame.revision_messages) {
          this.messageDeliveries.set(message.message_id,
            { signature: JSON.stringify([message.content, message.attachment_ids ?? [], message.mode ?? 'followup']),
              supersedeActive: false, supersedeTargets: new Map() })
        }
      }
      this.start.resolve(frame)
      return
    }
    if (!this.started) throw new Error('runtime has not started')
    if (frame.type === 'cancel') {
      if (this.startFrame.context_role === 'coordinator' &&
          (!exactKeys(frame, ['type', ...COORDINATOR_IDENTITY_FIELDS]) || !matchesCoordinatorIdentity(frame, this.startFrame))) {
        throw new Error('invalid coordinator cancellation fence')
      }
      if (this.startFrame.context_role === 'task_validation_review' &&
          (!exactKeys(frame, ['type', ...TASK_VALIDATION_REVIEW_IDENTITY_FIELDS]) ||
            !matchesTaskValidationReviewIdentity(frame, this.startFrame))) {
        throw new Error('invalid Task validation review cancellation fence')
      }
      this.cancel()
      return
    }
    if (frame.type === 'message_delivery') {
      if (this.cancelled || this.messageDeliveryClosed || !validMessageDelivery(frame, this.startFrame)) {
        throw new Error('invalid message delivery')
      }
      const attachmentIDs = frame.attachment_ids ?? []
      const mode = frame.mode ?? 'followup'
      const signature = JSON.stringify([frame.content, attachmentIDs, mode])
      const supersedeActive = frame.version === 4 && frame.supersede_active === true
      const previous = this.messageDeliveries.get(frame.message_id)
      if (previous !== undefined) {
        if (previous.signature !== signature) throw new Error('message delivery id conflicts with prior content or mode')
        const priorTargetCallID = previous.supersedeTargets.get(frame.supersede_session_id)
        if (supersedeActive && priorTargetCallID !== undefined && priorTargetCallID !== frame.supersede_call_id) {
          throw new Error('superseding message target call identity changed')
        }
        const newSupersedeTarget = supersedeActive && priorTargetCallID === undefined
        if (newSupersedeTarget) {
          const supersedeRoot = !previous.supersedeActive
          previous.supersedeActive = true
          previous.supersedeTargets.set(frame.supersede_session_id, frame.supersede_call_id)
          this.resolveRecoveringToolCall(frame)
          const delivery = { message_id: frame.message_id, content: frame.content, attachment_ids: [...attachmentIDs], mode,
            supersede_active: true, supersede_session_id: frame.supersede_session_id,
            supersede_call_id: frame.supersede_call_id, supersede_root: supersedeRoot }
          if (this.messageDeliveryHandler) this.messageDeliveryHandler(delivery)
          else this.pendingMessageDeliveries.push(delivery)
        }
        return
      }
      this.messageDeliveries.set(frame.message_id, { signature, supersedeActive,
        supersedeTargets: supersedeActive ? new Map([[frame.supersede_session_id, frame.supersede_call_id]]) : new Map() })
      if (supersedeActive) this.resolveRecoveringToolCall(frame)
      const delivery = { message_id: frame.message_id, content: frame.content, attachment_ids: [...attachmentIDs], mode,
        ...(supersedeActive ? { supersede_active: true, supersede_session_id: frame.supersede_session_id,
          supersede_call_id: frame.supersede_call_id, supersede_root: true } : {}) }
      if (this.messageDeliveryHandler) this.messageDeliveryHandler(delivery)
      else this.pendingMessageDeliveries.push(delivery)
      return
    }
    if (frame.type === 'child_abort') {
      const keys = ['agent_id', 'authorization_version', 'call_id', 'generation', 'owner_id', 'reason', 'session_id', 'task_id', 'type']
      const rootFence = Object.fromEntries(['task_id', 'owner_id', 'agent_id', 'generation', 'authorization_version']
        .map(key => [key, this.startFrame[key]]))
      if (this.cancelled || this.startFrame.context_role || !exactKeys(frame, keys) ||
          !Object.entries(rootFence).every(([key, value]) => frame[key] === value) ||
          typeof frame.session_id !== 'string' || !/^[A-Za-z0-9_-]{1,128}$/.test(frame.session_id) ||
          frame.session_id === this.startFrame.session_id || typeof frame.call_id !== 'string' || !/^[A-Za-z0-9_-]{1,128}$/.test(frame.call_id) ||
          typeof frame.reason !== 'string' || !frame.reason.isWellFormed() || frame.reason.includes('\0') ||
          Buffer.byteLength(frame.reason, 'utf8') < 1 || Buffer.byteLength(frame.reason, 'utf8') > 1024) {
        throw new Error('invalid child cancellation request')
      }
      if (typeof this.onChildAbort !== 'function') throw new Error('child cancellation is unavailable')
      this.onChildAbort({ session_id: frame.session_id, call_id: frame.call_id, reason: frame.reason })
      return
    }
    if (frame.type === 'job_cancel') {
      const keys = ['agent_id', 'authorization_version', 'generation', 'id', 'job_id', 'owner_id', 'session_id', 'task_id', 'type']
      if (Object.keys(frame).sort().join(',') !== keys.sort().join(',') ||
          !Object.entries(Object.fromEntries(['task_id', 'owner_id', 'agent_id', 'generation', 'authorization_version'].map(key => [key, this.startFrame[key]])))
            .every(([key, value]) => frame[key] === value) || typeof frame.id !== 'string' || !frame.id ||
          typeof frame.session_id !== 'string' || !/^[A-Za-z0-9_-]{1,128}$/.test(frame.session_id) ||
          typeof frame.job_id !== 'string' || !/^[A-Za-z0-9_-]{1,128}$/.test(frame.job_id)) throw new Error('invalid job cancellation request')
      if (!this.onJobCancel) throw new Error('job cancellation is unavailable')
      Promise.resolve(this.onJobCancel(frame.job_id, frame.session_id)).then(status => {
        if (!['cancelled', 'succeeded', 'failed', 'unknown'].includes(status)) throw new Error('invalid job cancellation status')
        return this.send({ type: 'job_cancel_result', ...Object.fromEntries(keys.filter(key => key !== 'type' && key !== 'id' && key !== 'job_id').map(key => [key, this.startFrame[key]])),
          session_id: frame.session_id, id: frame.id, job_id: frame.job_id, status })
      }).catch(() => this.fail('job cancellation failed'))
      return
    }
    if (frame.type === 'checkpoint_ack') {
      const pending = this.pending.get(frame.id)
      if (!pending || pending.kind !== 'checkpoint_ack') throw new Error('unknown checkpoint response')
      if (pending.abandoned) {
        this.pending.delete(frame.id)
        if (!validAbandonedCheckpointAck(frame, frame.id, pending)) throw new Error('invalid late checkpoint acknowledgment')
        return
      }
      if (pending.identity.context_role === 'coordinator' &&
          (!exactKeys(frame, ['type', ...COORDINATOR_IDENTITY_FIELDS, 'id', 'ok', 'through_seq', 'sha256']) ||
            !matchesCoordinatorIdentity(frame, pending.identity))) throw new Error('invalid coordinator checkpoint acknowledgment')
      if (pending.identity.context_role === 'task_validation_review' &&
          (!exactKeys(frame, ['type', ...TASK_VALIDATION_REVIEW_IDENTITY_FIELDS, 'id', 'ok', 'through_seq', 'sha256']) ||
            !matchesTaskValidationReviewIdentity(frame, pending.identity))) throw new Error('invalid Task validation review checkpoint acknowledgment')
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
      if (pending.abandoned) {
        this.pending.delete(frame.id)
        if (!validAbandonedCheckpointPage(frame, frame.id, pending)) throw new Error('invalid late checkpoint page')
        return
      }
      if (pending.identity.context_role === 'coordinator' &&
          (!exactKeys(frame, ['type', ...COORDINATOR_IDENTITY_FIELDS, 'id', 'ok', 'from_seq', 'through_seq', 'payload', 'sha256']) ||
            !matchesCoordinatorIdentity(frame, pending.identity))) throw new Error('invalid coordinator checkpoint page')
      if (pending.identity.context_role === 'task_validation_review' &&
          (!exactKeys(frame, ['type', ...TASK_VALIDATION_REVIEW_IDENTITY_FIELDS, 'id', 'ok', 'from_seq', 'through_seq', 'payload', 'sha256']) ||
            !matchesTaskValidationReviewIdentity(frame, pending.identity))) throw new Error('invalid Task validation review checkpoint page')
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
      const valid = validKeys && images !== undefined && (pending.abandoned || addedBytes !== undefined && this.imageCacheBytes + addedBytes <= MAX_TOOL_IMAGE_CACHE_BYTES) &&
        Object.entries(pending.identity).every(([key, value]) => frame[key] === value) &&
        frame.call_id === pending.callID && ['succeeded', 'failed'].includes(frame.status) &&
        typeof frame.output === 'string' && Buffer.byteLength(frame.output) <= MAX_TOOL_OUTPUT_BYTES
      if (!valid) {
        pending.reject?.(new Error('tool result not authorized'))
        throw new Error('invalid tool response')
      }
      if (pending.abandoned) return
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
      const key = childPendingKey('child_session', frame.parent_session_id, frame.call_id)
      const pending = this.childPending.get(key)
      if (!pending || pending.kind !== 'child_session') throw new Error('unknown child session response')
      if (pending.abandoned) {
        this.childPending.delete(key)
        if (!validAbandonedChildSession(frame, frame.call_id, pending)) throw new Error('invalid late child session response')
        return
      }
      this.childPending.delete(key)
      clearTimeout(pending.timer)
      pending.resolve(frame)
      return
    }
    if (frame.type === 'child_settle_ack') {
      const key = childPendingKey('child_settle_ack', frame.session_id, frame.call_id)
      const pending = this.childPending.get(key)
      if (!pending || pending.kind !== 'child_settle_ack') throw new Error('unknown child settlement response')
      if (pending.abandoned) {
        this.childPending.delete(key)
        if (!validAbandonedChildSettleAck(frame, frame.call_id, pending)) throw new Error('invalid late child settlement response')
        return
      }
      this.childPending.delete(key)
      clearTimeout(pending.timer)
      const hasOutput = Object.hasOwn(frame, 'output')
      if (frame.ok !== true || hasOutput && (typeof frame.output !== 'string' || Buffer.byteLength(frame.output) > MAX_TOOL_OUTPUT_BYTES) ||
          !Object.entries(pending.identity).every(([key, value]) => frame[key] === value)) {
        pending.reject(new Error('child settlement was not confirmed'))
        throw new Error('invalid child settlement response')
      }
      pending.resolve(hasOutput ? frame.output : undefined)
      return
    }
    if (frame.type === 'model_chunk' || frame.type === 'model_end') {
      const cancelledIdentity = this.cancelledModels.get(frame.id)
      if (cancelledIdentity) {
        if (frame.type === 'model_end' && !validModelEndFrame(frame, cancelledIdentity) ||
            frame.type === 'model_chunk' && cancelledIdentity.context_role === 'coordinator' &&
            (!exactKeys(frame, frame.type === 'model_chunk'
              ? ['type', ...COORDINATOR_IDENTITY_FIELDS, 'id', 'index', 'chunk']
              : ['type', ...COORDINATOR_IDENTITY_FIELDS, 'id', 'ok']) || !matchesCoordinatorIdentity(frame, cancelledIdentity)) ||
            frame.type === 'model_chunk' && cancelledIdentity.context_role === 'task_validation_review' &&
            (!exactKeys(frame, frame.type === 'model_chunk'
              ? ['type', ...TASK_VALIDATION_REVIEW_IDENTITY_FIELDS, 'id', 'index', 'chunk']
              : ['type', ...TASK_VALIDATION_REVIEW_IDENTITY_FIELDS, 'id', 'ok']) || !matchesTaskValidationReviewIdentity(frame, cancelledIdentity))) {
          throw new Error(cancelledIdentity.context_role === 'task_validation_review'
            ? 'invalid Task validation review model response identity' : 'invalid coordinator model response identity')
        }
        if (!Object.entries(cancelledIdentity).every(([key, value]) => frame[key] === value)) throw new Error('invalid model response identity')
        if (frame.type === 'model_end') this.cancelledModels.delete(frame.id)
        return
      }
      const model = this.models.get(frame.id)
      if (!model) throw new Error('unknown model response')
      if (frame.type === 'model_end' && !validModelEndFrame(frame, model.identity) ||
          frame.type === 'model_chunk' && model.identity.context_role === 'coordinator' &&
          (!exactKeys(frame, frame.type === 'model_chunk'
            ? ['type', ...COORDINATOR_IDENTITY_FIELDS, 'id', 'index', 'chunk']
            : ['type', ...COORDINATOR_IDENTITY_FIELDS, 'id', 'ok']) || !matchesCoordinatorIdentity(frame, model.identity)) ||
          frame.type === 'model_chunk' && model.identity.context_role === 'task_validation_review' &&
          (!exactKeys(frame, frame.type === 'model_chunk'
            ? ['type', ...TASK_VALIDATION_REVIEW_IDENTITY_FIELDS, 'id', 'index', 'chunk']
            : ['type', ...TASK_VALIDATION_REVIEW_IDENTITY_FIELDS, 'id', 'ok']) || !matchesTaskValidationReviewIdentity(frame, model.identity))) {
        throw new Error(model.identity.context_role === 'task_validation_review'
          ? 'invalid Task validation review model response identity' : 'invalid coordinator model response identity')
      }
      if (!Object.entries(model.identity).every(([key, value]) => frame[key] === value)) throw new Error('invalid model response identity')
      model.receive(frame)
      return
    }
    throw new Error('unknown worker message')
  }

  resolveRecoveringToolCall(frame) {
    if (frame.version !== 4 || frame.supersede_active !== true) return
    const root = this.startFrame
    const matchesRoot = identity => identity.task_id === root.task_id && identity.task_id === frame.task_id &&
      identity.owner_id === root.owner_id && identity.owner_id === frame.owner_id &&
      identity.agent_id === root.agent_id && identity.agent_id === frame.agent_id &&
      identity.generation === root.generation && identity.generation === frame.generation &&
      identity.authorization_version === root.authorization_version && identity.authorization_version === frame.authorization_version
    for (const [id, pending] of this.pending) {
      if (pending.kind !== 'tool_result' || pending.abandoned || pending.recovering !== true || !matchesRoot(pending.identity) ||
          pending.identity.session_id !== frame.supersede_session_id || pending.callID !== frame.supersede_call_id) continue
      queueMicrotask(() => {
        // An already delivered tool result is authoritative if both frames
        // were queued together; supersede only releases a still-pending
        // recovery waiter.
        if (this.pending.get(id) === pending) pending.resolve(RECOVERY_TOOL_SUPERSEDED)
      })
      return
    }
    for (const [key, pending] of this.childPending) {
      if (pending.kind !== 'child_session' || pending.recovering !== true || !matchesRoot(pending.identity) ||
          pending.identity.session_id !== frame.supersede_session_id || pending.callID !== frame.supersede_call_id) continue
      queueMicrotask(() => {
        if (this.childPending.get(key) === pending) pending.resolve(RECOVERY_TOOL_SUPERSEDED)
      })
      return
    }
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
      if (pending.abandoned) continue
      clearTimeout(pending.timer)
      if (pending.kind === 'tool_result') {
        this.abandonToolResult(id, pending)
        pending.reject(abortError())
        continue
      }
      this.abandonPending(this.pending, id, pending)
      pending.reject(abortError())
    }
    for (const [callID, pending] of this.childPending) {
      if (pending.abandoned) continue
      clearTimeout(pending.timer)
      this.abandonPending(this.childPending, callID, pending)
      pending.reject(abortError())
    }
    this.onCancel?.()
  }

  async checkpoint(identity, from, through, payload, digest, { onWaiting, onTimeout } = {}) {
    const id = randomUUID()
    const result = deferred()
    const pending = { ...result, timer: undefined, kind: 'checkpoint_ack', through, digest, identity: { ...identity } }
    const timer = setTimeout(() => {
      this.abandonPending(this.pending, id, pending)
      notifyIPCTimeout(onTimeout, 'checkpoint_write', sha256(`checkpoint_write\n${through}\n${digest}`))
      result.reject(new Error('checkpoint confirmation timed out'))
    }, CHECKPOINT_TIMEOUT_MS)
    pending.timer = timer
    this.pending.set(id, pending)
    try {
      await this.send({ type: 'checkpoint', id, ...identity, from_seq: from, through_seq: through, payload, sha256: digest })
      if (this.pending.get(id) === pending) notifyIPCWaiting(onWaiting)
      await result.promise
    } finally {
      clearTimeout(timer)
      if (this.pending.get(id) === pending) this.pending.delete(id)
    }
  }

  async readCheckpoint(identity, from, { checkpointThrough, checkpointDigest, onWaiting, onTimeout } = {}) {
    if (this.cancelled) throw abortError()
    if (!Number.isSafeInteger(from) || from < 0) throw new Error('invalid checkpoint offset')
    const id = randomUUID()
    const result = deferred()
    void result.promise.catch(() => {})
    const pending = { ...result, timer: undefined, kind: 'checkpoint_page', identity: { ...identity }, from }
    const timer = setTimeout(() => {
      this.abandonPending(this.pending, id, pending)
      if (Number.isSafeInteger(checkpointThrough) && typeof checkpointDigest === 'string' && /^[a-f0-9]{64}$/.test(checkpointDigest)) {
        notifyIPCTimeout(onTimeout, 'checkpoint_read', sha256(`checkpoint_read\n${from}\n${checkpointThrough}\n${checkpointDigest}`))
      }
      result.reject(new Error('checkpoint read timed out'))
    }, CHECKPOINT_TIMEOUT_MS)
    pending.timer = timer
    this.pending.set(id, pending)
    try {
      await this.send({ type: 'checkpoint_read', id, ...identity, from_seq: from })
      if (this.pending.get(id) === pending) notifyIPCWaiting(onWaiting)
      return await result.promise
    } finally {
      clearTimeout(timer)
      if (this.pending.get(id) === pending) this.pending.delete(id)
    }
  }

  abandonToolResult(id, pending) {
    this.abandonPending(this.pending, id, pending)
  }

  async toolCall(identity, through, callID, name, args, signal, { recovering = false, onWaiting, onTimeout } = {}) {
    if (this.cancelled) throw abortError()
    if (!Number.isSafeInteger(through) || through < 0 || typeof callID !== 'string' || !callID ||
        Buffer.byteLength(callID) > 256 || typeof name !== 'string' || !name || !isObject(args)) {
      throw new Error('invalid tool call')
    }
    signal?.throwIfAborted()
    const id = randomUUID()
    const result = deferred()
    void result.promise.catch(() => {})
    const pending = { ...result, timer: undefined, kind: 'tool_result', identity, callID, name, recovering }
    const timer = setTimeout(() => {
      this.abandonToolResult(id, pending)
      notifyIPCTimeout(onTimeout, 'tool_call', sha256(`tool_call\n${name}\n${JSON.stringify(args)}`))
      result.reject(new Error('tool result timed out'))
    }, TOOL_RESULT_TIMEOUT_MS)
    const abort = () => {
      this.abandonToolResult(id, pending)
      clearTimeout(timer)
      result.reject(abortError())
    }
    pending.timer = timer
    this.pending.set(id, pending)
    signal?.addEventListener('abort', abort, { once: true })
    try {
      await this.send({ type: 'tool_call', id, ...identity, through_seq: through, call_id: callID, name, arguments: args })
      if (this.pending.get(id) === pending) notifyIPCWaiting(onWaiting)
      return await result.promise
    } catch (error) {
      if (this.pending.get(id) === pending) {
        this.pending.delete(id)
        clearTimeout(timer)
      }
      throw error
    } finally {
      signal?.removeEventListener('abort', abort)
      if (this.pending.get(id) === pending) {
        this.pending.delete(id)
        clearTimeout(timer)
      }
    }
  }

  async createChild(identity, through, callID, args, signal, { recovering = false, onWaiting, onTimeout } = {}) {
    if (this.cancelled) throw abortError()
    const key = childPendingKey('child_session', identity?.session_id, callID)
    if (!Number.isSafeInteger(through) || through < 0 || typeof callID !== 'string' || !callID ||
        Buffer.byteLength(callID) > 256 || !validDelegateSelection(args) ||
        typeof args.title !== 'string' || !args.title.trim() || Buffer.byteLength(args.title) > MAX_DELEGATE_TITLE_BYTES ||
        typeof args.objective !== 'string' || !args.objective.trim() || Buffer.byteLength(args.objective) > MAX_DELEGATE_OBJECTIVE_BYTES ||
        (Object.hasOwn(args, 'run_in_background') && typeof args.run_in_background !== 'boolean') ||
        !Array.isArray(args.tool_names) || args.tool_names.length > 64 ||
        args.tool_names.some(name => typeof name !== 'string' || !TOOL_NAME.test(name)) ||
        new Set(args.tool_names).size !== args.tool_names.length || this.childPending.has(key)) {
      throw new Error('invalid child session request')
    }
    signal?.throwIfAborted()
    const result = deferred()
    void result.promise.catch(() => {})
    const pending = { ...result, timer: undefined, kind: 'child_session', identity: { ...identity }, callID,
      arguments: { ...args }, recovering }
    const timer = setTimeout(() => {
      this.abandonPending(this.childPending, key, pending)
      notifyIPCTimeout(onTimeout, 'child_create', sha256(`child_create\n${JSON.stringify(args)}`))
      result.reject(new Error('child session response timed out'))
    }, CHILD_RESULT_TIMEOUT_MS)
    const abort = () => {
      this.abandonPending(this.childPending, key, pending)
      clearTimeout(timer)
      result.reject(abortError())
    }
    pending.timer = timer
    this.childPending.set(key, pending)
    signal?.addEventListener('abort', abort, { once: true })
    try {
      await this.send({ type: 'child_create', ...identity, through_seq: through, call_id: callID, arguments: args })
      if (this.childPending.get(key) === pending) notifyIPCWaiting(onWaiting)
      return await result.promise
    } catch (error) {
      if (this.childPending.get(key) === pending) {
        this.childPending.delete(key)
        clearTimeout(timer)
      }
      throw error
    } finally {
      signal?.removeEventListener('abort', abort)
      if (this.childPending.get(key) === pending) {
        this.childPending.delete(key)
        clearTimeout(timer)
      }
    }
  }

  async settleChild(identity, callID, status, through, digest, summary, signal, { onWaiting, onTimeout } = {}) {
    if (this.cancelled) throw abortError()
    const key = childPendingKey('child_settle_ack', identity?.session_id, callID)
    if (typeof callID !== 'string' || !callID || Buffer.byteLength(callID) > 256 ||
        !['succeeded', 'failed'].includes(status) || !Number.isSafeInteger(through) || through < -1 ||
        (through === -1 ? digest !== '' : typeof digest !== 'string' || !/^[a-f0-9]{64}$/.test(digest)) ||
        typeof summary !== 'string' || Buffer.byteLength(summary) > MAX_TOOL_OUTPUT_BYTES || this.childPending.has(key)) {
      throw new Error('invalid child settlement')
    }
    signal?.throwIfAborted()
    const result = deferred()
    void result.promise.catch(() => {})
    const pending = { ...result, timer: undefined, kind: 'child_settle_ack', identity: { ...identity }, callID, status, summary }
    const timer = setTimeout(() => {
      this.abandonPending(this.childPending, key, pending)
      const summaryDigest = sha256(trimGoSpace(summary))
      notifyIPCTimeout(onTimeout, 'child_settle', sha256(`child_settle\n${status}\n${through}\n${digest}\n${summaryDigest}`))
      result.reject(new Error('child settlement response timed out'))
    }, CHILD_RESULT_TIMEOUT_MS)
    const abort = () => {
      this.abandonPending(this.childPending, key, pending)
      clearTimeout(timer)
      result.reject(abortError())
    }
    pending.timer = timer
    this.childPending.set(key, pending)
    signal?.addEventListener('abort', abort, { once: true })
    try {
      await this.send({
        type: 'child_settle', ...identity, call_id: callID, status,
        through_seq: through, sha256: digest, summary,
      })
      if (this.childPending.get(key) === pending) notifyIPCWaiting(onWaiting)
      return await result.promise
    } catch (error) {
      if (this.childPending.get(key) === pending) {
        this.childPending.delete(key)
        clearTimeout(timer)
      }
      throw error
    } finally {
      signal?.removeEventListener('abort', abort)
      if (this.childPending.get(key) === pending) {
        this.childPending.delete(key)
        clearTimeout(timer)
      }
    }
  }

  async *streamModel(identity, id, through, request, signal) {
    if (this.models.size >= 17 || this.models.has(id) || this.cancelledModels.has(id)) throw new Error('too many active model requests')
    if (identity.context_role === 'task_validation_review') {
      if (!validTaskValidationReviewIdentity(identity) ||
          id !== taskValidationReviewModelCallID(identity.owner_id, identity.task_id, identity.task_revision,
            identity.result_event_sequence, identity.result_sha256, identity.claim_generation, identity.session_id, through) ||
          !Array.isArray(request.tools) || request.tools.length !== 0) {
        throw new Error('model call does not match the Task validation review fence')
      }
    } else if (identity.context_role === 'coordinator') {
      const expectedID = identity.purpose === 'compaction'
        ? coordinatorCompactionModelCallID(identity.conversation_id, identity.operation_id, identity.session_id, through)
        : coordinatorModelCallID(identity.conversation_id, identity.message_id, identity.session_id, through)
      if (id !== expectedID || identity.purpose !== (identity.operation_id ? 'compaction' : 'conversation')) {
        throw new Error('model call does not match the coordinator purpose')
      }
    } else if (Object.hasOwn(identity, 'purpose') || Object.hasOwn(identity, 'operation_id')) {
      throw new Error('Task model call cannot use a coordinator purpose')
    }
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
        if (frame.ok !== true) {
          failure = Object.hasOwn(MODEL_FAILURE_SUMMARIES, frame.failure_code)
            ? new LlmError(MODEL_FAILURE_SUMMARIES[frame.failure_code], frame.failure_code)
            : new Error('model request failed')
        } else if (!finished) failure = new Error('model request failed')
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
      pending.reject?.(new Error(reason))
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
