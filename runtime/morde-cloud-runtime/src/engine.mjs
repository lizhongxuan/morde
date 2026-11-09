import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { Context } from '@deepseek-ai/cordis'
import AgentRegistry from '@deepseek-ai/dsh-agent'
import AgentLoop from '@deepseek-ai/dsh-agent-loop'
import LlmRuntime, { LlmAdapter, MessageId, createToolResultMessage, createUserMessage, freezeMessage } from '@deepseek-ai/dsh-llm'
import SessionStore, { SessionId, SessionLogOffset } from '@deepseek-ai/dsh-session'
import * as checkpointPolicy from '@deepseek-ai/dsh-session-checkpoint-policy'
import SessionPersistence, { assertContiguous, assertStoredId, assertVersion, materializeAppendBatch, materializeCreateHeader, validateStoredEvents } from '@deepseek-ai/dsh-session-persistence'
import SessionProjectionRegistry from '@deepseek-ai/dsh-session-projection'
import SystemPrompt from '@deepseek-ai/dsh-system-prompt'
import ToolRuntime from '@deepseek-ai/dsh-tools'
import { apply as nativeToolExecutorApply, inject as nativeToolExecutorInject, name as nativeToolExecutorName } from '@deepseek-ai/dsh-tool-executor-native'

const MAX_CHECKPOINT_PAYLOAD_BYTES = 1024 * 1024
const MAX_RESTORE_BYTES = 8 * 1024 * 1024
const MAX_RESTORE_EVENTS = 100_000
const MAX_TOOL_IMAGE_BYTES = 512 * 1024
const MAX_TOOL_IMAGES = 1
const MAX_TASK_ATTACHMENT_IMAGES = 4
const MAX_TOOL_OUTPUT_BYTES = 1024 * 1024
const ID = /^[A-Za-z0-9_-]{1,128}$/
const TOOL_NAME = /^[A-Za-z][A-Za-z0-9_.-]{0,127}$/
const DIGEST = /^[a-f0-9]{64}$/
const SCHEMA_TYPES = new Set(['object', 'array', 'string', 'number', 'integer', 'boolean', 'null'])
const SCHEMA_ANNOTATIONS = new Set(['description', 'title', 'default', 'examples'])
const CLARIFICATION_TOOL = 'ask_user'
const DELEGATE_TOOL = 'dsh_delegate'
const MORDE_MCP_TOOL_PREFIX = 'mcp_'
const JOB_LIST_TOOL = 'dsh_job_list'
const JOB_GET_TOOL = 'dsh_job_get'
const JOB_OUTPUT_TOOL = 'dsh_job_output'
const JOB_CANCEL_TOOL = 'dsh_job_cancel'
const JOB_TOOLS = new Set([JOB_LIST_TOOL, JOB_GET_TOOL, JOB_OUTPUT_TOOL, JOB_CANCEL_TOOL])
const MAX_DELEGATE_TITLE_BYTES = 256
const MAX_DELEGATE_OBJECTIVE_BYTES = 32 * 1024
const MAX_CHILD_SUMMARY_BYTES = 1024 * 1024
const MAX_USER_MESSAGE_BYTES = 128 * 1024
const SOUL = readFileSync(new URL('./SOUL.md', import.meta.url), 'utf8')
// This is Morde's persisted checkpoint/protocol compatibility identifier, not the upstream DSH package release.
// Keep it stable across compatible dependency upgrades; changing it requires a durable-task migration.
export const ENGINE_VERSION = '0.2.0-rc.2'

const MESSAGE_DELIVERY_KEYS_V1 = [
  'type', 'version', 'task_id', 'owner_id', 'agent_id', 'generation', 'authorization_version',
  'session_id', 'message_id', 'content',
]
const MESSAGE_DELIVERY_KEYS_V2 = [...MESSAGE_DELIVERY_KEYS_V1, 'attachment_ids']
const MESSAGE_ID = /^m[a-z0-9]{7}$/
const MESSAGE_ATTACHMENT_ID = /^(?:pa|pca)_[a-f0-9]{32}$/
const MAX_MESSAGE_ATTACHMENTS = 16

function deferred() {
  let resolve, reject
  const promise = new Promise((ok, fail) => { resolve = ok; reject = fail })
  return { promise, resolve, reject }
}

const TOOL_OUTPUT_SCHEMA = {
  type: 'object',
  properties: {
    status: { type: 'string', enum: ['succeeded', 'failed'] },
    output: { type: 'string' },
    images: {
      type: 'array',
      items: {
        type: 'object',
        properties: {
          attachmentId: { type: 'string' },
          mediaType: { type: 'string' },
          bytes: { type: 'integer' },
          width: { type: 'integer' },
          height: { type: 'integer' },
        },
        required: ['attachmentId', 'mediaType', 'bytes', 'width', 'height'],
        additionalProperties: false,
      },
    },
  },
  required: ['status', 'output', 'images'],
  additionalProperties: false,
}

function isObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

function isMordeMCPTool(tool) {
  return typeof tool?.name === 'string' && tool.name.startsWith(MORDE_MCP_TOOL_PREFIX)
}

function mordeMCPToolDefinition(sessionDurable, tool, onError) {
  return {
    name: tool.name,
    description: tool.description,
    parameters: tool.parameters,
    output: {
      schema: TOOL_OUTPUT_SCHEMA,
      render(_args, result) {
        if (!validMordeMCPToolResult(result)) throw new Error('Morde MCP broker returned an invalid result')
        return [
          { type: 'text', text: result.status === 'failed' ? `Tool failed: ${result.output}` : result.output },
          ...result.images.map(attachment => ({ type: 'image', attachment })),
        ]
      }
    },
    async execute(args, execution) {
      try {
        if (!isObject(args)) throw new Error('Morde MCP tool arguments are invalid')
        const result = await sessionDurable.ipc.toolCall(
          sessionDurable.identity, sessionDurable.through, execution.callId, tool.name, args, execution.signal,
        )
        if (!validMordeMCPToolResult(result)) throw new Error('Morde MCP broker returned an invalid result')
        return result
      } catch (error) {
        onError?.(error, execution)
        throw error
      }
    },
  }
}

function validMordeMCPToolResult(result) {
  return isObject(result) && Object.keys(result).sort().join(',') === 'images,output,status' &&
    ['succeeded', 'failed'].includes(result.status) && typeof result.output === 'string' &&
    Buffer.byteLength(result.output) <= MAX_TOOL_OUTPUT_BYTES && Array.isArray(result.images) &&
    result.images.length <= MAX_TOOL_IMAGES && result.images.every(validImageRef) &&
    !(result.status === 'failed' && result.images.length > 0)
}

function exactKeys(value, expected) {
  return Object.keys(value).sort().join(',') === [...expected].sort().join(',')
}

export function validMessageDelivery(frame, identity) {
  const legacy = isObject(frame) && frame.version === 1 && exactKeys(frame, MESSAGE_DELIVERY_KEYS_V1)
  const current = isObject(frame) && frame.version === 2 && exactKeys(frame, MESSAGE_DELIVERY_KEYS_V2)
  if (!isObject(frame) || (!legacy && !current) || frame.type !== 'message_delivery' ||
      !identity || !['task_id', 'owner_id', 'agent_id', 'session_id'].every(key => frame[key] === identity[key]) ||
      !['generation', 'authorization_version'].every(key => Number.isSafeInteger(frame[key]) && frame[key] > 0 && frame[key] === identity[key]) ||
      !MESSAGE_ID.test(frame.message_id ?? '') || typeof frame.content !== 'string' || !frame.content.isWellFormed() ||
      frame.content.includes('\0') ||
      Buffer.byteLength(frame.content, 'utf8') > MAX_USER_MESSAGE_BYTES) return false
  if (legacy) return !!frame.content.trim()
  if (!Array.isArray(frame.attachment_ids) || frame.attachment_ids.length > MAX_MESSAGE_ATTACHMENTS) return false
  if (!frame.content.trim() && frame.attachment_ids.length === 0) return false
  let previous = ''
  for (const id of frame.attachment_ids) {
    if (typeof id !== 'string' || !MESSAGE_ATTACHMENT_ID.test(id) || id <= previous) return false
    previous = id
  }
  return true
}

function validateInboxState(inbox) {
  if (!inbox || !Array.isArray(inbox['next-turn']) || !Array.isArray(inbox['next-step'])) {
    throw new Error('pending session inbox is invalid')
  }
  const seen = new Set()
  for (const message of [...inbox['next-turn'], ...inbox['next-step']]) {
    if (!isObject(message) || !ID.test(message.id ?? '') || message.role !== 'user' || !Array.isArray(message.content) ||
        !isObject(message.source) || typeof message.source.kind !== 'string' || !message.source.kind) {
      throw new Error('pending session inbox message is invalid')
    }
    if (seen.has(message.id)) throw new Error('pending session inbox message identity is repeated')
    seen.add(message.id)
  }
  return inbox
}

function deliveredMessageBlocks(content, attachmentIDs = []) {
  const blocks = [{ type: 'text', text: content }]
  if (attachmentIDs.length) {
    blocks.push({ type: 'text', text: `[MORDE_TASK_ATTACHMENT_IDS_V1:${attachmentIDs.join(',')}]` })
  }
  return blocks
}

function sameDeliveredMessage(message, messageID, content, attachmentIDs = []) {
  return isObject(message) && message.id === messageID && message.role === 'user' &&
    message.source?.kind === 'user' && Array.isArray(message.content) && message.content.length === 1 + (attachmentIDs.length ? 1 : 0) &&
    isObject(message.content[0]) && message.content[0].type === 'text' && message.content[0].text === content &&
    (!attachmentIDs.length || isObject(message.content[1]) && message.content[1].type === 'text' &&
      message.content[1].text === `[MORDE_TASK_ATTACHMENT_IDS_V1:${attachmentIDs.join(',')}]`)
}

function validImageRef(ref) {
  return isObject(ref) && Object.keys(ref).sort().join(',') === 'attachmentId,bytes,height,mediaType,width' &&
    typeof ref.attachmentId === 'string' && /^morde_img_[a-f0-9]{64}$/.test(ref.attachmentId) &&
    ['image/jpeg', 'image/png'].includes(ref.mediaType) && Number.isSafeInteger(ref.bytes) && ref.bytes > 0 && ref.bytes <= MAX_TOOL_IMAGE_BYTES &&
    Number.isSafeInteger(ref.width) && ref.width > 0 && ref.width <= 1280 &&
    Number.isSafeInteger(ref.height) && ref.height > 0 && ref.height <= 720
}

function toolImageLimit(name) {
  if (name === 'morde_read_task_attachment') return MAX_TASK_ATTACHMENT_IMAGES
  if (typeof name === 'string' && (name.startsWith(MORDE_MCP_TOOL_PREFIX) || name === 'desktop_screenshot' || name === 'browser_screenshot')) return 1
  return 0
}

function sameImageRefs(left, right) {
  return left.length === right.length && left.every((ref, index) =>
    ref.attachmentId === right[index].attachmentId && ref.mediaType === right[index].mediaType &&
    ref.bytes === right[index].bytes && ref.width === right[index].width && ref.height === right[index].height)
}

function recoveryToolCalls(events, tools) {
  const allowed = new Map((tools ?? []).map(tool => [tool.name, tool]))
  const calls = new Map()
  let openTurn = null
  let openStep = null
  for (const event of events) {
    if (event.type === 'turn/start') {
      openTurn = event.data.turn
      openStep = null
    } else if (event.type === 'turn/end') {
      for (const call of calls.values()) {
        if (call.turn === event.data.turn && call.resultText === undefined) throw new Error('closed dsh turn has an unresolved tool call')
      }
      openTurn = null
      openStep = null
    } else if (event.type === 'step/start') {
      openStep = event.data.step
    } else if (event.type === 'step/end') {
      for (const call of calls.values()) {
        if (call.turn === event.data.turn && call.step === event.data.step && call.resultText === undefined) throw new Error('closed dsh step has an unresolved tool call')
      }
      openStep = null
    } else if (event.type === 'assistant/message' && event.data.message.role === 'assistant') {
      if (openTurn !== event.data.turn || openStep !== event.data.step) throw new Error('checkpointed dsh assistant call is outside its open step')
      for (const block of event.data.message.content) {
        if (block.type !== 'tool-call') continue
        const tool = allowed.get(block.name)
        if (!ID.test(block.id) || !tool || typeof block.arguments !== 'string' || !Buffer.byteLength(block.arguments) || Buffer.byteLength(block.arguments) > 64 * 1024) {
          throw new Error('checkpointed dsh tool call is not currently authorized')
        }
        const args = JSON.parse(block.arguments)
        if (!isObject(args) || calls.has(block.id)) throw new Error('checkpointed dsh tool call identity is invalid')
        calls.set(block.id, { id: block.id, name: block.name, definition: tool, arguments: block.arguments, args, turn: event.data.turn, step: event.data.step, callSeq: undefined, resultText: undefined, resultImages: undefined })
      }
    } else if (event.type === 'tool/call') {
      const call = calls.get(event.data.callId)
      if (!call || call.callSeq !== undefined || call.turn !== event.data.turn || call.step !== event.data.step ||
          openTurn !== call.turn || openStep !== call.step || call.name !== event.data.name || call.arguments !== event.data.arguments) throw new Error('checkpointed dsh tool start does not match its assistant call')
      call.callSeq = event.seq
    } else if (event.type === 'tool/result') {
      const message = event.data.message
      const call = calls.get(message?.source?.callId)
      const imageContent = Array.isArray(message?.content) ? message.content.slice(1) : []
      if (!call || call.resultText !== undefined || call.callSeq === undefined || message.toolCallId !== call.id ||
          message.isError !== false || event.data.error !== undefined || event.surfaceOp !== 'append' ||
          !Array.isArray(event.sourceEventSeqs) || event.sourceEventSeqs.length !== 1 || event.sourceEventSeqs[0] !== call.callSeq ||
          event.data.turn !== call.turn || event.data.step !== call.step || openTurn !== call.turn || openStep !== call.step || !Array.isArray(message.content) ||
          message.content.length < 1 || message.content.length > 1 + toolImageLimit(call.name) ||
          message.content[0].type !== 'text' || typeof message.content[0].text !== 'string' ||
          imageContent.some(block => !isObject(block) || Object.keys(block).sort().join(',') !== 'attachment,type' ||
            block.type !== 'image' || !validImageRef(block.attachment))) {
        throw new Error('checkpointed dsh tool result is invalid')
      }
      call.resultText = message.content[0].text
      call.resultImages = imageContent.map(block => block.attachment)
    }
  }
  for (const call of calls.values()) {
    if (call.resultText === undefined && (openTurn !== call.turn || openStep !== call.step)) throw new Error('checkpointed dsh tool call is outside the open tail step')
  }
  const lastTurnEnd = events.findLast(event => event.type === 'turn/end')
  return { calls: [...calls.values()], openTurn, openStep, completed: openTurn === null && lastTurnEnd?.data?.reason?.kind === 'completed' }
}

function freezeJSON(value) {
  const pending = [value]
  while (pending.length) {
    const item = pending.pop()
    if (item === null || typeof item !== 'object') continue
    for (const child of Object.values(item)) pending.push(child)
    Object.freeze(item)
  }
  return value
}

function validParameterSchema(root) {
  if (!root || typeof root !== 'object' || Array.isArray(root) || root.type !== 'object' || root.additionalProperties !== false ||
      !root.properties || typeof root.properties !== 'object' || Array.isArray(root.properties)) return false
  const pending = [{ schema: root, depth: 0 }]
  let visited = 0
  while (pending.length) {
    const { schema, depth } = pending.pop()
    if (!schema || typeof schema !== 'object' || Array.isArray(schema) || ++visited > 2048 || depth > 32 ||
        !(typeof schema.type === 'string' || Array.isArray(schema.type))) return false
    const types = typeof schema.type === 'string' ? [schema.type] : schema.type
    if (!types.length || types.some((type, index) => typeof type !== 'string' || !SCHEMA_TYPES.has(type) || types.indexOf(type) !== index)) return false
    const typeSet = new Set(types)
    const hasType = (...candidates) => candidates.some(type => typeSet.has(type))
    const allowed = new Set([...SCHEMA_ANNOTATIONS, 'type'])
    if (hasType('object')) for (const key of ['properties', 'required', 'additionalProperties', 'minProperties', 'maxProperties']) allowed.add(key)
    if (hasType('array')) for (const key of ['items', 'minItems', 'maxItems', 'uniqueItems']) allowed.add(key)
    if (hasType('string')) for (const key of ['enum', 'const', 'pattern', 'format', 'minLength', 'maxLength']) allowed.add(key)
    if (hasType('number', 'integer')) for (const key of ['enum', 'const', 'minimum', 'maximum', 'exclusiveMinimum', 'exclusiveMaximum', 'multipleOf']) allowed.add(key)
    if (hasType('boolean', 'null')) for (const key of ['enum', 'const']) allowed.add(key)
    if (Object.keys(schema).some(key => !allowed.has(key))) return false
    if (Object.hasOwn(schema, 'description') && typeof schema.description !== 'string') return false
    if (Object.hasOwn(schema, 'title') && typeof schema.title !== 'string') return false
    if (hasType('object')) {
      if (schema.additionalProperties !== false || (schema.properties !== undefined &&
          (!schema.properties || typeof schema.properties !== 'object' || Array.isArray(schema.properties)))) return false
      const properties = schema.properties ?? {}
      const required = schema.required ?? []
      if (!Array.isArray(required) || required.some((name, index) => typeof name !== 'string' || !Object.hasOwn(properties, name) || required.indexOf(name) !== index)) return false
      for (const child of Object.values(properties)) pending.push({ schema: child, depth: depth + 1 })
    }
    if (hasType('array')) {
      if (!schema.items) return false
      pending.push({ schema: schema.items, depth: depth + 1 })
    }
    for (const key of ['minLength', 'maxLength', 'minItems', 'maxItems', 'minProperties', 'maxProperties']) {
      if (Object.hasOwn(schema, key) && (!Number.isSafeInteger(schema[key]) || schema[key] < 0)) return false
    }
    for (const key of ['minimum', 'maximum', 'exclusiveMinimum', 'exclusiveMaximum', 'multipleOf']) {
      if (Object.hasOwn(schema, key) && (typeof schema[key] !== 'number' || !Number.isFinite(schema[key]) || (key === 'multipleOf' && schema[key] <= 0))) return false
    }
    if (Object.hasOwn(schema, 'uniqueItems') && typeof schema.uniqueItems !== 'boolean') return false
    if (Object.hasOwn(schema, 'pattern')) {
      if (typeof schema.pattern !== 'string') return false
      try { new RegExp(schema.pattern) } catch { return false }
    }
    if (Object.hasOwn(schema, 'format') && typeof schema.format !== 'string') return false
    if (Object.hasOwn(schema, 'enum') && (!Array.isArray(schema.enum) || !schema.enum.length)) return false
    if (Object.hasOwn(schema, 'const') && schema.const === undefined) return false
    const matchesType = (value, type) => type === 'object' ? value !== null && typeof value === 'object' && !Array.isArray(value)
      : type === 'array' ? Array.isArray(value)
        : type === 'integer' ? Number.isSafeInteger(value)
          : type === 'number' ? typeof value === 'number' && Number.isFinite(value)
            : type === 'null' ? value === null : typeof value === type
    const matchesSchemaType = value => types.some(type => matchesType(value, type))
    if (Object.hasOwn(schema, 'enum') && (!Array.isArray(schema.enum) || schema.enum.some(value => !matchesSchemaType(value)))) return false
    if (Object.hasOwn(schema, 'const') && (schema.const === undefined || !matchesSchemaType(schema.const))) return false
    if (Object.hasOwn(schema, 'minLength') && Object.hasOwn(schema, 'maxLength') && schema.minLength > schema.maxLength) return false
    if (Object.hasOwn(schema, 'minItems') && Object.hasOwn(schema, 'maxItems') && schema.minItems > schema.maxItems) return false
    if (Object.hasOwn(schema, 'minProperties') && Object.hasOwn(schema, 'maxProperties') && schema.minProperties > schema.maxProperties) return false
  }
  return true
}

function validMordeMCPCallParameters(schema) {
  const rootKeys = ['additionalProperties', 'properties', 'required', 'type']
  const properties = schema?.properties
  const argumentsSchema = properties?.arguments
  if (!isObject(schema) || Object.keys(schema).sort().join(',') !== rootKeys.join(',') || schema.type !== 'object' ||
      schema.additionalProperties !== false || !isObject(properties) || Object.keys(properties).sort().join(',') !== 'arguments,connector_id,tool_name' ||
      !Array.isArray(schema.required) || [...schema.required].sort().join(',') !== 'arguments,connector_id,tool_name' ||
      new Set(schema.required).size !== 3 || !isObject(argumentsSchema) || argumentsSchema.type !== 'object' ||
      Object.keys(argumentsSchema).some(key => !['type', 'description', 'title'].includes(key)) ||
      (Object.hasOwn(argumentsSchema, 'description') && typeof argumentsSchema.description !== 'string') ||
      (Object.hasOwn(argumentsSchema, 'title') && typeof argumentsSchema.title !== 'string')) return false

  // MCP tool inputs are discovered at runtime and validated by the server
  // against the pinned connector schema before dispatch. Keep this dynamic map
  // exception limited to the single broker tool that owns that validation.
  return validParameterSchema({ ...schema, properties: {
    ...properties, arguments: { type: 'object', properties: {}, additionalProperties: false },
  } })
}

function validToolDefinition(tool) {
  if (!tool || typeof tool !== 'object' || Array.isArray(tool) ||
      Object.keys(tool).sort().join(',') !== 'description,name,parameters' ||
      typeof tool.name !== 'string' || !TOOL_NAME.test(tool.name) ||
      typeof tool.description !== 'string' || !tool.description.trim() || Buffer.byteLength(tool.description) > 2048) return false
  const parametersValid = tool.name === 'mcp_call' ? validMordeMCPCallParameters(tool.parameters) : validParameterSchema(tool.parameters)
  return parametersValid && Buffer.byteLength(JSON.stringify(tool.parameters)) <= 64 * 1024
}

function validTools(tools) {
  if (tools === undefined) return true
  if (!Array.isArray(tools) || tools.length > 64) return false
  const names = new Set()
  for (const tool of tools) {
    if (!validToolDefinition(tool) || names.has(tool.name)) return false
    names.add(tool.name)
  }
  return true
}

function validDelegateTool(tool) {
  if (!tool || tool.name !== DELEGATE_TOOL || !validToolDefinition(tool)) return false
  const schema = tool.parameters
  const properties = schema.properties
  return schema.additionalProperties === false &&
    Array.isArray(schema.required) && schema.required.length === 3 &&
    ['title', 'objective', 'tool_names'].every(name => schema.required.includes(name)) &&
    Object.keys(properties).sort().join(',') === 'objective,run_in_background,title,tool_names' &&
    properties.title?.type === 'string' && properties.objective?.type === 'string' &&
    properties.run_in_background?.type === 'boolean' &&
    properties.tool_names?.type === 'array' && properties.tool_names.items?.type === 'string' &&
    properties.tool_names.items.enum?.every(name => typeof name === 'string' && TOOL_NAME.test(name)) === true
}

function validDelegateArguments(args, appTools) {
  if (!isObject(args) || !['objective,title,tool_names', 'objective,run_in_background,title,tool_names'].includes(Object.keys(args).sort().join(',')) ||
      typeof args.title !== 'string' || !args.title.trim() || Buffer.byteLength(args.title) > MAX_DELEGATE_TITLE_BYTES ||
      typeof args.objective !== 'string' || !args.objective.trim() || Buffer.byteLength(args.objective) > MAX_DELEGATE_OBJECTIVE_BYTES ||
      (Object.hasOwn(args, 'run_in_background') && typeof args.run_in_background !== 'boolean') ||
      !Array.isArray(args.tool_names) || args.tool_names.length > 64) return false
  const available = new Set(appTools.map(tool => tool.name).filter(name => name !== CLARIFICATION_TOOL && name !== DELEGATE_TOOL && !JOB_TOOLS.has(name)))
  const names = new Set()
  return args.tool_names.every(name => typeof name === 'string' && TOOL_NAME.test(name) &&
    name !== CLARIFICATION_TOOL && name !== DELEGATE_TOOL && !JOB_TOOLS.has(name) && available.has(name) && !names.has(name) && names.add(name))
}

function validJobTool(tool) {
  if (!tool || !JOB_TOOLS.has(tool.name) || !validToolDefinition(tool)) return false
  const properties = tool.parameters.properties
  const keys = Object.keys(properties).sort().join(',')
  const required = [...(tool.parameters.required ?? [])].sort().join(',')
  if (tool.name === JOB_LIST_TOOL) return keys === '' && required === ''
  if (tool.name === JOB_GET_TOOL) return keys === 'job_id' && required === 'job_id' && properties.job_id.type === 'string'
  if (tool.name === JOB_CANCEL_TOOL) return keys === 'job_id' && required === 'job_id' && properties.job_id.type === 'string'
  return keys === 'after_seq,job_id' && required === 'after_seq,job_id' &&
    properties.job_id.type === 'string' && properties.after_seq.type === 'integer' && properties.after_seq.minimum === -1
}

function childIdentity(frame, parent) {
  const keys = ['task_id', 'owner_id', 'agent_id', 'session_id', 'generation', 'authorization_version']
  if (!isObject(frame) || keys.some(key => !Object.hasOwn(frame, key)) ||
      ![frame.task_id, frame.owner_id, frame.agent_id, frame.session_id].every(value => typeof value === 'string' && ID.test(value)) ||
      !Number.isSafeInteger(frame.generation) || frame.generation < 1 ||
      !Number.isSafeInteger(frame.authorization_version) || frame.authorization_version < 1 ||
      frame.task_id !== parent.task_id || frame.owner_id !== parent.owner_id || frame.agent_id !== parent.agent_id ||
      frame.generation !== parent.generation || frame.authorization_version !== parent.authorization_version ||
      frame.session_id === parent.session_id) return undefined
  return Object.fromEntries(keys.map(key => [key, frame[key]]))
}

function validateChildSession(response, parentIdentity, callID, argumentsValue, appTools) {
  if (response.type !== 'child_session' || response.call_id !== callID ||
      response.parent_session_id !== parentIdentity.session_id || response.objective !== argumentsValue.objective ||
      !['running', 'succeeded', 'failed', 'unknown'].includes(response.step_status) ||
      typeof response.summary !== 'string' || Buffer.byteLength(response.summary) > MAX_CHILD_SUMMARY_BYTES) {
    throw new Error('child session response is invalid')
  }
  const identity = childIdentity(response, parentIdentity)
  const tools = response.tools
  if (!identity || !validTools(tools) || tools.some(tool => tool.name === DELEGATE_TOOL || tool.name === CLARIFICATION_TOOL)) {
    throw new Error('child session authorization is invalid')
  }
  const requestedNames = [...argumentsValue.tool_names].sort()
  const returnedNames = tools.map(tool => tool.name).sort()
  if (JSON.stringify(requestedNames) !== JSON.stringify(returnedNames) ||
      returnedNames.some(name => !appTools.some(tool => tool.name === name && name !== DELEGATE_TOOL && name !== CLARIFICATION_TOOL && !JOB_TOOLS.has(name)))) {
    throw new Error('child tools exceed the requested allowlist')
  }
  const through = response.checkpoint_through_seq
  const digest = response.checkpoint_sha256
  if (!Number.isSafeInteger(through) || through < -1 ||
      (through === -1 ? digest !== '' : typeof digest !== 'string' || !DIGEST.test(digest))) {
    throw new Error('child checkpoint identity is invalid')
  }
  return { identity, tools, through, digest }
}

export function modelCallID(taskID, sessionID, throughSeq) {
  if (!ID.test(taskID) || !ID.test(sessionID) || !Number.isSafeInteger(throughSeq) || throughSeq < 0) {
    throw new Error('invalid model call identity')
  }
  const input = `morde-dsh-model-v1\n${taskID}\n${sessionID}\n${throughSeq}`
  return `mdl_${createHash('sha256').update(input, 'utf8').digest('hex')}`
}

export function validStart(frame) {
  if (frame.version !== 1 || frame.type !== 'start') return false
  if (!['task_id', 'owner_id', 'agent_id', 'session_id'].every(key => typeof frame[key] === 'string' && ID.test(frame[key]))) return false
  if (!['generation', 'authorization_version'].every(key => Number.isSafeInteger(frame[key]) && frame[key] > 0)) return false
  if (typeof frame.model !== 'string' || !frame.model.trim() || Buffer.byteLength(frame.model) > 256) return false
  if (!validTools(frame.tools)) return false
  const delegate = frame.tools?.find(tool => tool.name === DELEGATE_TOOL)
  if (delegate && !validDelegateTool(delegate)) return false
  const jobTools = frame.tools?.filter(tool => JOB_TOOLS.has(tool.name)) ?? []
  if (jobTools.length !== 0 && (jobTools.length !== JOB_TOOLS.size || jobTools.some(tool => !validJobTool(tool)))) return false
  if (frame.background_jobs !== undefined) {
    if (!Array.isArray(frame.background_jobs) || frame.background_jobs.length > 16 ||
        (frame.background_jobs.length > 0 && jobTools.length !== JOB_TOOLS.size)) return false
    const seen = new Set()
    for (const job of frame.background_jobs) {
      const identity = childIdentity(job, frame)
      if (!identity || !ID.test(job.spawn_key ?? '') || job.parent_session_id !== frame.session_id || job.background !== true ||
          job.step_status !== 'running' || job.cancel_requested === true || !Number.isSafeInteger(job.checkpoint_through_seq) ||
          job.checkpoint_through_seq < 0 || typeof job.checkpoint_sha256 !== 'string' || !DIGEST.test(job.checkpoint_sha256) ||
          typeof job.title !== 'string' || Buffer.byteLength(job.title) > 120 || !validTools(job.tools) ||
          job.tools.some(tool => tool.name === DELEGATE_TOOL || tool.name === CLARIFICATION_TOOL || JOB_TOOLS.has(tool.name) ||
            !frame.tools.some(parentTool => parentTool.name === tool.name)) || seen.has(identity.session_id)) return false
      seen.add(identity.session_id)
    }
  }
  if (frame.checkpoint !== undefined || (frame.resume !== undefined && typeof frame.resume !== 'boolean')) return false
  if (frame.initial_message !== undefined) {
    const initial = frame.initial_message
    if (!isObject(initial) || !exactKeys(initial, ['message_id', 'content', 'attachment_ids']) ||
        !validMessageDelivery({ type: 'message_delivery', version: 2,
          ...Object.fromEntries(['task_id', 'owner_id', 'agent_id', 'generation', 'authorization_version', 'session_id'].map(key => [key, frame[key]])),
          ...initial }, frame) || frame.prompt !== undefined || frame.resume === true) return false
  }
  if (frame.initial_context !== undefined && (typeof frame.initial_context !== 'string' || !frame.initial_context.isWellFormed() ||
      frame.initial_context.includes('\0') || Buffer.byteLength(frame.initial_context) > MAX_CHECKPOINT_PAYLOAD_BYTES / 2 || frame.resume === true)) return false
  if (frame.revision_messages !== undefined) {
    if (!Array.isArray(frame.revision_messages) || frame.revision_messages.length < 1 || frame.revision_messages.length > 128) return false
    const seen = new Set(frame.initial_message ? [frame.initial_message.message_id] : [])
    for (const message of frame.revision_messages) {
      if (!isObject(message) || !exactKeys(message, ['message_id', 'content', 'attachment_ids']) || seen.has(message.message_id) ||
          !validMessageDelivery({ type: 'message_delivery', version: 2,
            ...Object.fromEntries(['task_id', 'owner_id', 'agent_id', 'generation', 'authorization_version', 'session_id'].map(key => [key, frame[key]])),
            ...message }, frame)) return false
      seen.add(message.message_id)
    }
  }
  if (frame.resume === true) {
    if (frame.prompt !== undefined || !Number.isSafeInteger(frame.checkpoint_through_seq) || frame.checkpoint_through_seq < 0 || !DIGEST.test(frame.checkpoint_sha256)) return false
  } else {
    if (frame.checkpoint_through_seq !== undefined || frame.checkpoint_sha256 !== undefined) return false
    if (frame.initial_message === undefined && (typeof frame.prompt !== 'string' || !frame.prompt.trim() || Buffer.byteLength(frame.prompt) > MAX_CHECKPOINT_PAYLOAD_BYTES / 2)) return false
  }
  return frame.max_tokens === undefined || (Number.isSafeInteger(frame.max_tokens) && frame.max_tokens > 0 && frame.max_tokens <= 1_000_000)
}

class DurableSession {
  constructor(ipc, identity, start) {
    this.ipc = ipc
    this.identity = identity
    this.start = start
    this.through = -1
    this.digest = start.resume === true ? start.checkpoint_sha256 : ''
    this.lastModelThrough = -1
    this.failure = undefined
    this.serial = Promise.resolve()
    this.restored = undefined
    this.recoveryReady = false
    this.recoveryHistory = undefined
    this.recoveryHasInbox = false
    this.firstCheckpoint = deferred()
    void this.firstCheckpoint.promise.catch(() => {})
  }

  enqueue(action) {
    const result = this.serial.then(async () => {
      if (this.failure) throw this.failure
      await action()
    })
    this.serial = result.catch(error => { this.failure = error })
    return result
  }

  async write(header, events) {
    if (header.id !== this.identity.session_id) throw new Error('session identity mismatch')
    let cursor = 0
    while (cursor < events.length) {
      const from = this.through + 1
      const batch = []
      let payload
      while (cursor < events.length) {
        const event = events[cursor]
        if (event.seq !== from + batch.length) throw new Error('non-contiguous session events')
        const candidate = JSON.stringify({ header, from_seq: from, events: [...batch, event] })
        if (Buffer.byteLength(candidate) > MAX_CHECKPOINT_PAYLOAD_BYTES) {
          if (!batch.length) throw new Error('session event exceeds checkpoint limit')
          break
        }
        payload = candidate
        batch.push(event)
        cursor += 1
      }
      const through = from + batch.length - 1
      const digest = createHash('sha256').update(payload, 'utf8').digest('hex')
      await this.ipc.checkpoint(this.identity, from, through, payload, digest)
      this.through = through
      this.digest = digest
      this.firstCheckpoint.resolve()
      if (this.restored !== undefined) for (const event of batch) this.restored.events.push(event)
    }
  }

  append(header, events) {
    return this.enqueue(() => this.write(header, events))
  }

  flush(session) {
    if (session.id !== this.identity.session_id) return Promise.reject(new Error('session identity mismatch'))
    return this.enqueue(() => this.write(session.header, session.snapshotEvents(SessionLogOffset(this.through + 1))))
  }

  async restore(frame = this.start) {
    if (this.restored !== undefined || this.through !== -1) throw new Error('session recovery already started')
    if (frame.resume !== true || !Number.isSafeInteger(frame.checkpoint_through_seq) || frame.checkpoint_through_seq < 0 ||
        typeof frame.checkpoint_sha256 !== 'string' || !DIGEST.test(frame.checkpoint_sha256)) throw new Error('session recovery is unavailable')
    let from = 0
    let header
    let serializedHeader
    let totalBytes = 0
    const events = []
    while (from <= frame.checkpoint_through_seq) {
      const page = await this.ipc.readCheckpoint(this.identity, from)
      if (this.ipc.cancelled || page.from_seq !== from || !Number.isSafeInteger(page.through_seq) ||
          page.through_seq < from || page.through_seq > frame.checkpoint_through_seq ||
          typeof page.payload !== 'string' || Buffer.byteLength(page.payload) > MAX_CHECKPOINT_PAYLOAD_BYTES ||
          typeof page.sha256 !== 'string' || !DIGEST.test(page.sha256) ||
          createHash('sha256').update(page.payload, 'utf8').digest('hex') !== page.sha256) throw new Error('invalid checkpoint page')
      totalBytes += Buffer.byteLength(page.payload)
      if (totalBytes > MAX_RESTORE_BYTES) throw new Error('checkpoint history exceeds restore limit')
      const body = JSON.parse(page.payload)
      if (JSON.stringify(body) !== page.payload || !body || typeof body !== 'object' || Array.isArray(body) || Object.keys(body).sort().join(',') !== 'events,from_seq,header' ||
          body.from_seq !== from || !Array.isArray(body.events) || body.events.length === 0 ||
          body.events.length !== page.through_seq - from + 1 || events.length + body.events.length > MAX_RESTORE_EVENTS) throw new Error('invalid checkpoint event range')
      const pageHeader = materializeCreateHeader(body.header)
      assertStoredId(this.identity.session_id, pageHeader)
      assertVersion(pageHeader)
      if (pageHeader.isSeeded !== false) throw new Error('seeded checkpoint recovery is unavailable')
      const encodedHeader = JSON.stringify(pageHeader)
      if (header !== undefined && encodedHeader !== serializedHeader) throw new Error('checkpoint header changed')
      header ??= freezeJSON(pageHeader)
      serializedHeader ??= encodedHeader
      assertContiguous(this.identity.session_id, body.events, from)
      for (const event of validateStoredEvents(header, body.events)) events.push(freezeJSON(event))
      if (page.through_seq === frame.checkpoint_through_seq && page.sha256 !== frame.checkpoint_sha256) throw new Error('checkpoint tail does not match pinned digest')
      from = page.through_seq + 1
    }
    this.through = frame.checkpoint_through_seq
    this.digest = frame.checkpoint_sha256
    this.restored = { header, events }
    this.firstCheckpoint.resolve()
    return this.restored
  }

  waitForCheckpoint() {
    return this.firstCheckpoint.promise
  }

  async appendRecovery(events, context, signal) {
    signal?.throwIfAborted()
    const batch = validateStoredEvents(this.restored.header, materializeAppendBatch(events)).map(freezeJSON)
    assertContiguous(this.identity.session_id, batch, this.through + 1)
    const preview = context.sessions.prepare(SessionId(this.identity.session_id), {
      seed: [...this.restored.events, ...batch], meta: structuredClone(this.restored.header), inheritedEventCount: 0, eventState: 'shared-frozen',
    })
    const inbox = context.sessionProjections.stateOf(preview, 'inbox')
    validateInboxState(inbox)
    await this.append(this.restored.header, batch)
  }

  async reconcileTools(context, tools, signal) {
    if (!this.restored || this.recoveryReady) throw new Error('session recovery is unavailable')
    const history = recoveryToolCalls(this.restored.events, tools)
    const missingCalls = history.calls.filter(call => call.callSeq === undefined)
    const missingStarts = missingCalls.map(call => ({
      type: 'tool/call', seq: 0, time: this.restored.events.at(-1).time,
      data: { turn: call.turn, step: call.step, callId: call.id, name: call.name, arguments: call.arguments },
    }))
    if (missingStarts.length) {
      let seq = this.through + 1
      for (const event of missingStarts) event.seq = seq++
      await this.appendRecovery(missingStarts, context, signal)
      for (let index = 0; index < missingStarts.length; index++) missingCalls[index].callSeq = missingStarts[index].seq
    }
    for (const call of history.calls) {
      signal?.throwIfAborted()
      let resultText
      let resultImages
      if (call.name === DELEGATE_TOOL) {
        const result = await this.executeDelegate?.(this, call.id, call.args, signal)
        if (!result) throw new Error('delegated child recovery is unavailable')
        resultText = result.status === 'failed' ? `Tool failed: ${result.output}` : result.output
        resultImages = result.images
      } else if (isMordeMCPTool(call.definition)) {
        const definition = mordeMCPToolDefinition(this, call.definition)
        const value = await definition.execute(call.args, { callId: call.id, signal })
        const content = definition.output.render(call.args, value)
        if (!Array.isArray(content) || content.length !== 1 + value.images.length || content[0]?.type !== 'text' ||
            typeof content[0].text !== 'string' || content.slice(1).some((block, index) =>
              block?.type !== 'image' || !validImageRef(block.attachment) ||
              !sameImageRefs([block.attachment], [value.images[index]]))) {
          throw new Error('Morde MCP recovery result could not be rendered')
        }
        resultText = content[0].text
        resultImages = value.images
      } else {
        const result = await this.ipc.toolCall(this.identity, this.through, call.id, call.name, call.args, signal)
        if (!result) throw new Error('tool recovery result is unavailable')
        resultText = result.status === 'failed' ? `Tool failed: ${result.output}` : result.output
        resultImages = result.images
      }
      if (call.resultText !== undefined) {
        if (call.resultText !== resultText || !sameImageRefs(call.resultImages, resultImages)) throw new Error('checkpointed dsh result differs from the reconciled operation ledger')
        continue
      }
      const last = this.restored.events.at(-1)
      await this.appendRecovery([{
        type: 'tool/result', seq: this.through + 1, time: last.time,
        data: { turn: call.turn, step: call.step, message: createToolResultMessage({
          callId: call.id, content: [{ type: 'text', text: resultText }, ...resultImages.map(attachment => ({ type: 'image', attachment }))], isError: false,
        }) },
        surfaceOp: 'append', sourceEventSeqs: [call.callSeq],
      }], context, signal)
    }
    this.recoveryReady = true
    this.recoveryHistory = history
    return history
  }
}

function persistencePlugin(durables) {
  return class WorkerSessionPersistence extends SessionPersistence {
    static inject = ['sessions', 'sessionProjections']

    constructor(ctx) {
      super(ctx)
      this.context = ctx
      ctx.on('session/flush', session => {
        const durable = durables.get(session.id)
        if (!durable) throw new Error('session has no Morde persistence fence')
        return durable.flush(session)
      })
    }

    async create(header) {
      const durable = durables.get(header.id)
      if (!durable || durable.start.resume === true || header.id !== durable.identity.session_id) throw new Error('session identity mismatch')
      const context = this.context
      let closed = false
      return {
        id: header.id,
        header,
        inheritedEventCount: 0,
        access: 'write',
        async append(events) {
          if (closed || events.length) throw new Error('session seed not supported')
        },
        async flush() {
          if (closed) throw new Error('session handle closed')
          const session = context.sessions.get(header.id)
          if (session) await durable.flush(session)
        },
        async close() {
          if (closed) return
          const session = context.sessions.get(header.id)
          if (session) await durable.flush(session)
          closed = true
        },
      }
    }

    async open(id, access, options = {}) {
      const durable = durables.get(id)
      if (!durable || durable.start.resume !== true || id !== durable.identity.session_id || access !== 'write') throw new Error('session recovery is unavailable')
      options.signal?.throwIfAborted()
      const restored = await durable.restore()
      options.signal?.throwIfAborted()
      const context = this.context
      const preview = context.sessions.prepare(SessionId(id), {
        seed: [...restored.events], meta: structuredClone(restored.header), inheritedEventCount: 0, eventState: 'shared-frozen',
      })
      const inbox = context.sessionProjections.stateOf(preview, 'inbox')
      validateInboxState(inbox)
      durable.recoveryHasInbox = inbox['next-turn'].length > 0 || inbox['next-step'].length > 0
      await durable.reconcileTools(context, durable.start.tools, options.signal)
      let closed = false
      return {
        id, header: restored.header, inheritedEventCount: 0, access: 'write',
        async read(offset = 0, length, readOptions = {}) {
          readOptions.signal?.throwIfAborted()
          if (closed || !Number.isSafeInteger(offset) || offset < 0 ||
              (length !== undefined && (!Number.isSafeInteger(length) || length < 0))) throw new Error('invalid session read')
          return { eventState: 'shared-frozen', events: restored.events.slice(offset, length === undefined ? undefined : offset + length) }
        },
        async append(events, appendOptions = {}) {
          appendOptions.signal?.throwIfAborted()
          if (closed) throw new Error('session handle closed')
          const batch = validateStoredEvents(restored.header, materializeAppendBatch(events)).map(freezeJSON)
          assertContiguous(id, batch, durable.through + 1)
          await durable.append(restored.header, batch)
        },
        async flush(flushOptions = {}) {
          flushOptions.signal?.throwIfAborted()
          if (closed) throw new Error('session handle closed')
          const session = context.sessions.get(id)
          if (session) await durable.flush(session)
          else await durable.serial
          if (durable.failure) throw durable.failure
        },
        async close() {
          if (closed) return
          const session = context.sessions.get(id)
          if (session) await durable.flush(session)
          else await durable.serial
          if (durable.failure) throw durable.failure
          closed = true
        },
      }
    }
    async flush() {
      for (const durable of durables.values()) {
        const session = this.context.sessions.get(durable.identity.session_id)
        if (session) await durable.flush(session)
      }
    }
    async stat() { throw new Error('session inspection is unavailable') }
    async list() { throw new Error('session listing is unavailable') }
  }
}

export async function runAgent(ipc, frame) {
  if (!validStart(frame)) throw new Error('invalid start request')
  const identity = Object.fromEntries(['task_id', 'owner_id', 'agent_id', 'generation', 'authorization_version', 'session_id'].map(key => [key, frame[key]]))
  const durables = new Map()
  const durable = new DurableSession(ipc, identity, frame)
  durables.set(identity.session_id, durable)
  const ctx = new Context()
  const nativeToolExecutor = { name: nativeToolExecutorName, inject: nativeToolExecutorInject, apply: nativeToolExecutorApply }
  const plugins = [SessionStore, SessionProjectionRegistry, AgentRegistry, SystemPrompt, ToolRuntime, nativeToolExecutor, LlmRuntime, persistencePlugin(durables), AgentLoop, checkpointPolicy]
  for (const plugin of plugins) await ctx.plugin(plugin, plugin === SystemPrompt ? { includeHarnessIdentity: false } : undefined)
  ctx.systemPrompt.section({
    name: 'morde:soul', order: ctx.systemPrompt.getSectionOrder('DEPLOYMENT_PERSONA_PREFIX'),
    text: SOUL, interpolate: false,
  })
  if (!frame.tools || frame.tools.length === 0) {
    ctx.systemPrompt.section({
      name: 'morde:no-tools', order: ctx.systemPrompt.getSectionOrder('DEPLOYMENT_PERSONA_SUFFIX'),
      text: 'No tools are available for this model run. Answer text requests normally. If a requested action requires a tool, explain that the tool is unavailable and never claim to have used it or completed the action.',
      interpolate: false,
    })
  }

  const liveHandles = new Set()
  const delegatedCalls = new Map()
  const childOwners = new Map()
  const backgroundJobs = new Map()
  let delegateQueue = Promise.resolve()
  let failed = false
  const appTools = (frame.tools ?? []).filter(tool => tool.name !== DELEGATE_TOOL && !JOB_TOOLS.has(tool.name))
  const agentOptions = { provider: 'morde', model: frame.model, ...(frame.max_tokens === undefined ? {} : { maxTokens: frame.max_tokens }) }

  function createSessionHandle(sessionDurable, parentAgent) {
    const setup = (agentCtx, agent) => {
      const state = { failed: false, toolFailure: false }
      sessionDurable.agent = agent
      sessionDurable.agentState = state
      agentCtx.on('agent/error', () => { state.failed = true })
      const tools = sessionDurable.start.tools ?? []
      const toolNames = new Set(tools.map(tool => tool.name))
      agentCtx.on('tools/pre-execute', async exec => {
        const assistantMessage = agent.session.snapshotEvents().findLast(event =>
          event.type === 'assistant/message' && event.data?.message?.role === 'assistant' &&
          Array.isArray(event.data.message.content) &&
          event.data.message.content.some(block => block.type === 'tool-call' && block.id === exec.callId))
        const turnCalls = assistantMessage?.data.message.content.filter(block => block.type === 'tool-call')
        if (!turnCalls || (turnCalls.some(block => block.name === CLARIFICATION_TOOL) && turnCalls.length !== 1)) {
          state.toolFailure = true
          agent.cancel({ kind: 'hook', reason: 'clarification must be the only tool call in its model turn' })
          return { kind: 'deny', reason: 'clarification must be the only tool call in its model turn' }
        }
        if (toolNames.has(exec.name)) return { kind: 'allow' }
        state.toolFailure = true
        agent.cancel({ kind: 'hook', reason: 'tool authorization is unavailable' })
        return { kind: 'deny', reason: 'tool authorization is unavailable' }
      })
      for (const tool of tools) {
        const onError = (_error, exec) => {
          if (!ipc.cancelled && !exec.signal?.aborted) {
            state.toolFailure = true
            agent.cancel({ kind: 'hook', reason: 'tool result unavailable' })
          }
        }
        if (isMordeMCPTool(tool)) {
          agentCtx.tools.register(mordeMCPToolDefinition(sessionDurable, tool, onError))
          continue
        }
        agentCtx.tools.register({
          name: tool.name,
          description: tool.description,
          parameters: tool.parameters,
          output: {
            schema: TOOL_OUTPUT_SCHEMA,
            render: (_args, value) => [
              { type: 'text', text: value.status === 'failed' ? `Tool failed: ${value.output}` : value.output },
              ...value.images.map(attachment => ({ type: 'image', attachment })),
            ],
          },
          async execute(args, exec) {
            if (!args || typeof args !== 'object' || Array.isArray(args)) throw new Error('tool arguments must be an object')
            try {
              if (tool.name === DELEGATE_TOOL) {
                return await runDelegation(sessionDurable, agent, exec.callId, args, exec.signal)
              }
              return await ipc.toolCall(sessionDurable.identity, sessionDurable.through, exec.callId, tool.name, args, exec.signal)
            } catch (error) {
              onError(error, exec)
              throw error
            }
          },
        })
      }
    }
    const options = { agentOptions, setup, ...(parentAgent ? { parentAgent } : {}) }
    const pending = sessionDurable.start.resume === true
      ? ctx.agents.resume({ ...options, resumeSessionId: SessionId(sessionDurable.identity.session_id) })
      : ctx.agents.create({ ...options, sessionId: SessionId(sessionDurable.identity.session_id) })
    return pending.then(handle => {
      liveHandles.add(handle)
      return handle
    })
  }

  async function runChildSession(child, callID, argumentsValue, parentAgent, signal, job) {
    const childStart = {
      type: 'start', version: 1, ...child.identity, model: frame.model, tools: child.tools,
      ...(child.through < 0 ? { prompt: `# ${argumentsValue.title}\n\n${argumentsValue.objective}` } : {
        resume: true, checkpoint_through_seq: child.through, checkpoint_sha256: child.digest,
      }),
    }
    const durableChild = new DurableSession(ipc, child.identity, childStart)
    durableChild.parentCallID = callID
    durables.set(child.identity.session_id, durableChild)
    const ownerKey = `${parentAgent?.session?.id ?? identity.session_id}\0${callID}`
    childOwners.set(child.identity.session_id, ownerKey)
    let handle
    try {
      signal?.throwIfAborted()
      if (ipc.cancelled) throw new Error('child agent was cancelled')
      handle = await createSessionHandle(durableChild, parentAgent)
      if (job) job.handle = handle
      const agent = handle.agent
      const state = durableChild.agentState
      if (ipc.cancelled) throw new Error('child agent was cancelled')
      if (childStart.resume === true) {
        const recovery = durableChild.recoveryHistory
        if (!recovery || (!recovery.completed && recovery.openTurn === null)) throw new Error('child session checkpoint is not safely resumable')
        await durableChild.waitForCheckpoint()
        job?.started.resolve()
        await agent.whenIdle()
        if (!await ctx.sessions.flush(agent.session)) throw new Error('child checkpoint listener unavailable')
        signal?.throwIfAborted()
        if (ipc.cancelled || state.failed || state.toolFailure) throw new Error('child session recovery did not remain idle')
        if (!recovery.completed) {
          agent.followup(createUserMessage({
            content: [{ type: 'text', text: 'Continue this delegated task from the last verified checkpoint. Do not repeat completed tool actions; use their recorded results.' }],
            source: { kind: 'user' },
          }))
          await agent.whenIdle()
          if (!await ctx.sessions.flush(agent.session)) throw new Error('child checkpoint listener unavailable')
          signal?.throwIfAborted()
          if (ipc.cancelled || state.failed || state.toolFailure) throw new Error('child session continuation did not complete')
        }
      } else {
        agent.followup(createUserMessage({
          content: [{ type: 'text', text: childStart.prompt }],
          source: { kind: 'user' },
        }))
        await durableChild.waitForCheckpoint()
        job?.started.resolve()
        await agent.whenIdle()
        if (!await ctx.sessions.flush(agent.session)) throw new Error('child checkpoint listener unavailable')
        signal?.throwIfAborted()
        if (ipc.cancelled) throw new Error('child agent was cancelled')
      }
      const end = agent.session.snapshotEvents().findLast(event => event.type === 'turn/end')
      if (ipc.cancelled || signal?.aborted) throw new Error('child agent was cancelled')
      const status = job?.cancelRequested ? 'failed' : !state.failed && !state.toolFailure && end?.data?.reason?.kind === 'completed' ? 'succeeded' : 'failed'
      let summary = delegatedSummary(agent)
      if (job?.cancelRequested) summary = 'Background job cancelled.'
      else if (!summary) summary = status === 'succeeded' ? 'Child agent completed without a text summary.' : 'Child agent did not complete successfully.'
      if (Buffer.byteLength(summary) > MAX_CHILD_SUMMARY_BYTES) throw new Error('child summary exceeds limit')
      await ipc.settleChild(durableChild.identity, callID, status, durableChild.through, durableChild.digest, summary, signal)
      return { status, output: summary, images: [] }
    } finally {
      if (handle) {
        liveHandles.delete(handle)
        if (job?.handle === handle) job.handle = undefined
        await handle.dispose()
      }
      if (durables.get(child.identity.session_id) === durableChild) durables.delete(child.identity.session_id)
      childOwners.delete(child.identity.session_id)
    }
  }

  function startBackgroundJob(child, callID, argumentsValue, parentAgent) {
    const existing = backgroundJobs.get(child.identity.session_id)
    if (existing) {
      if (existing.callID !== callID) throw new Error('background job identity was reused')
      return existing
    }
    const started = deferred()
    const job = { child, callID, handle: undefined, started, status: 'running', cancelRequested: false }
    backgroundJobs.set(child.identity.session_id, job)
    job.promise = Promise.resolve().then(() => runChildSession(child, callID, argumentsValue, parentAgent, undefined, job)).then(result => {
      job.status = job.cancelRequested ? 'cancelled' : result.status
      job.output = result.output
      return result
    }, error => {
      job.status = 'unknown'
      started.reject(error)
      throw error
    })
    void job.promise.catch(() => {})
    return job
  }

  async function waitForBackgroundJobs() {
    const jobs = [...backgroundJobs.values()]
    await Promise.allSettled(jobs.map(job => job.promise))
    // An unknown child may have committed side effects; let the worker pause for recovery review.
    if (jobs.some(job => job.status === 'unknown')) throw new Error('background child outcome is unknown')
  }

  async function cancelBackgroundJob(sessionID) {
    const job = backgroundJobs.get(sessionID)
    if (!job) return 'unknown'
    if (job.status !== 'running') return job.status
    job.cancelRequested = true
    job.handle?.agent.cancel({ kind: 'user' })
    await job.promise
    return job.status === 'cancelled' ? 'cancelled' : job.status
  }

  async function runDelegation(parentDurable, parentAgent, callID, argumentsValue, signal) {
    if (!validDelegateArguments(argumentsValue, appTools)) throw new Error('delegated task arguments are invalid')
    const ownerKey = `${parentDurable.identity.session_id}\0${callID}`
    const existing = delegatedCalls.get(ownerKey)
    if (existing) return existing
    const run = delegateQueue.then(async () => {
      signal?.throwIfAborted()
      if (ipc.cancelled) throw new Error('delegated task was cancelled')
      const response = await ipc.createChild(parentDurable.identity, parentDurable.through, callID, argumentsValue, signal)
      const child = validateChildSession(response, parentDurable.identity, callID, argumentsValue, appTools)
      if (argumentsValue.run_in_background === true) {
        if (response.step_status === 'running') {
          const childOwner = childOwners.get(child.identity.session_id)
          if (childOwner && childOwner !== ownerKey) throw new Error('child session is already owned by another call')
          const job = startBackgroundJob(child, callID, argumentsValue, parentAgent)
          await Promise.race([job.started.promise, job.promise])
        }
        return { status: 'succeeded', output: JSON.stringify({ job_id: child.identity.session_id, status: 'running' }), images: [] }
      }
      if (response.step_status === 'unknown') throw new Error('delegated task outcome is unknown')
      if (response.step_status === 'succeeded' || response.step_status === 'failed') {
        return {
          status: response.step_status,
          output: response.summary || (response.step_status === 'succeeded' ? 'Child agent completed.' : 'Child agent failed.'),
          images: [],
        }
      }
      const childOwner = childOwners.get(child.identity.session_id)
      if (childOwner && childOwner !== ownerKey) throw new Error('child session is already owned by another call')
      return runChildSession(child, callID, argumentsValue, parentAgent, signal)
    })
    delegateQueue = run.catch(() => {})
    delegatedCalls.set(ownerKey, run)
    try {
      return await run
    } finally {
      if (delegatedCalls.get(ownerKey) === run) delegatedCalls.delete(ownerKey)
    }
  }

  durable.executeDelegate = (parentDurable, callID, args, signal) => runDelegation(parentDurable, undefined, callID, args, signal)
  class WorkerModel extends LlmAdapter {
    async *stream(options) {
      const sessionDurable = typeof options.sessionId === 'string' ? durables.get(options.sessionId) : undefined
      if (!sessionDurable || sessionDurable.start.resume === true && !sessionDurable.recoveryReady) {
        throw new Error('model execution requires an authorized session checkpoint')
      }
      if (options.provider !== 'morde' || options.model !== frame.model) {
        throw new Error('model route is not authorized')
      }
      if (sessionDurable.agentState?.toolFailure) throw new Error('tool result is unavailable')
      if (sessionDurable.through <= sessionDurable.lastModelThrough) throw new Error('model request has no new durable prefix')
      sessionDurable.lastModelThrough = sessionDurable.through
      const request = {
        provider: options.provider,
        model: options.model,
        messages: options.messages,
        tools: options.tools ?? [],
        ...(options.maxTokens === undefined ? {} : { max_tokens: options.maxTokens }),
      }
      const id = modelCallID(sessionDurable.identity.task_id, sessionDurable.identity.session_id, sessionDurable.through)
      yield* ipc.streamModel(sessionDurable.identity, id, sessionDurable.through, request, options.signal)
    }
  }
  ctx.llm.registerAdapter(['morde'], new WorkerModel())
  const cancelAgents = cause => {
    for (const handle of liveHandles) handle.agent.cancel(cause)
  }
  ipc.onJobCancel = cancelBackgroundJob
  ipc.onCancel = () => cancelAgents({ kind: 'user' })
  ipc.onFailure = () => cancelAgents({ kind: 'hook', reason: 'worker connection unavailable' })
  let handle
  try {
    handle = await createSessionHandle(durable)
    const agent = handle.agent
    const state = durable.agentState
    const installMessageDeliveryHandler = () => ipc.setMessageDeliveryHandler(({ message_id, content, attachment_ids }) => {
      const attachmentIDs = attachment_ids ?? []
      let found = false
      const check = message => {
        if (message?.id !== message_id) return
        if (!sameDeliveredMessage(message, message_id, content, attachmentIDs)) throw new Error('message delivery id conflicts with session history')
        found = true
      }
      for (const event of agent.session.snapshotEvents()) {
        if (event.type === 'user/message') check(event.data)
        else check(event.data?.message)
      }
      for (const message of [...agent.inbox.nextTurn, ...agent.inbox.nextStep]) check(message)
      if (!found) agent.followup(freezeMessage({
        id: MessageId(message_id), role: 'user', content: deliveredMessageBlocks(content, attachmentIDs), source: { kind: 'user' },
      }))
    })
    const revisionMessages = frame.revision_messages ?? []
    const stageRevisionMessages = deliveries => {
      const byID = new Map(deliveries.map(message => [message.message_id, message]))
      for (const event of agent.session.snapshotEvents()) {
        if (event.type === 'user/message' && byID.has(event.data?.id)) {
          throw new Error('pending revision is already present in the checkpoint')
        }
      }
      let contextCount = 0
      for (const existing of [...agent.inbox.nextStep, ...agent.inbox.nextTurn]) {
        const message = byID.get(existing.id)
        if (!message && existing.source?.kind === 'plugin' && existing.source?.plugin === 'morde-context' && ++contextCount === 1) continue
        if (!message || !sameDeliveredMessage(existing, message.message_id, message.content, message.attachment_ids)) {
          throw new Error('checkpoint contains an unrelated pending message')
        }
        if (!agent.inbox.remove(existing.id)) throw new Error('pending revision could not be restaged')
      }
      for (let index = 0; index < deliveries.length; index++) {
        const message = deliveries[index]
        const value = freezeMessage({ id: MessageId(message.message_id), role: 'user',
          content: deliveredMessageBlocks(message.content, message.attachment_ids), source: { kind: 'user' } })
        if (index + 1 === deliveries.length) agent.followup(value)
        else agent.inject(value)
      }
    }
    if (frame.resume === true && revisionMessages.length === 0) installMessageDeliveryHandler()
    for (const record of frame.background_jobs ?? []) {
      const child = {
        identity: childIdentity(record, identity), tools: record.tools,
        through: record.checkpoint_through_seq, digest: record.checkpoint_sha256,
      }
      startBackgroundJob(child, record.spawn_key, { title: record.title }, agent)
    }
    await Promise.all([...backgroundJobs.values()].map(job => job.started.promise))
    const recovery = frame.resume === true ? durable.recoveryHistory : undefined
    if (ipc.cancelled) {
      return { type: 'settled', ...identity, state: 'cancelled', through_seq: durable.through }
    }
    if (frame.resume === true) {
      if (!recovery || (!recovery.completed && recovery.openTurn === null && !durable.recoveryHasInbox && revisionMessages.length === 0)) throw new Error('checkpoint tail is not safely resumable')
      await agent.whenIdle()
      if (!await ctx.sessions.flush(agent.session)) throw new Error('checkpoint listener unavailable')
      if (state.failed || state.toolFailure || ipc.cancelled) throw new Error('session recovery did not remain idle')
      if (revisionMessages.length !== 0) {
        stageRevisionMessages(revisionMessages)
        installMessageDeliveryHandler()
        await agent.whenIdle()
        if (!await ctx.sessions.flush(agent.session)) throw new Error('checkpoint listener unavailable')
        if (state.failed || state.toolFailure || ipc.cancelled) throw new Error('revised task did not remain idle')
      } else if (!recovery.completed || (durable.recoveryHasInbox && agent.inbox.hasPending)) {
        agent.followup(createUserMessage({
          content: [{ type: 'text', text: 'Continue the existing task from the last verified checkpoint. Do not repeat completed tool actions; use their recorded results. Process any user message already queued in the session inbox.' }],
          source: { kind: 'user' },
        }))
        await agent.whenIdle()
        if (!await ctx.sessions.flush(agent.session)) throw new Error('checkpoint listener unavailable')
        if (state.failed || state.toolFailure || ipc.cancelled) throw new Error('session recovery continuation did not remain idle')
      }
      const end = agent.session.snapshotEvents().findLast(event => event.type === 'turn/end')
      if (end?.data?.reason?.kind !== 'completed') throw new Error('recovered dsh turn did not complete')
      await waitForBackgroundJobs()
      return { type: 'settled', ...identity, state: 'completed_candidate', through_seq: durable.through }
    }
    if (frame.initial_message) {
      if (frame.initial_context) agent.inject(createUserMessage({
        content: [{ type: 'text', text: frame.initial_context }], source: { kind: 'plugin', plugin: 'morde-context' },
      }))
      if (revisionMessages.length !== 0) stageRevisionMessages([frame.initial_message, ...revisionMessages])
      else agent.followup(freezeMessage({ id: MessageId(frame.initial_message.message_id), role: 'user',
        content: deliveredMessageBlocks(frame.initial_message.content, frame.initial_message.attachment_ids), source: { kind: 'user' } }))
    } else if (revisionMessages.length !== 0) {
      agent.inject(createUserMessage({ content: [{ type: 'text', text: frame.prompt }], source: { kind: 'plugin', plugin: 'morde-context' } }))
      stageRevisionMessages(revisionMessages)
    } else {
      if (frame.initial_context) agent.inject(createUserMessage({
        content: [{ type: 'text', text: frame.initial_context }], source: { kind: 'plugin', plugin: 'morde-context' },
      }))
      agent.followup(createUserMessage({ content: [{ type: 'text', text: frame.prompt }], source: { kind: 'user' } }))
    }
    installMessageDeliveryHandler()
    await agent.whenIdle()
    if (!await ctx.sessions.flush(agent.session)) throw new Error('checkpoint listener unavailable')
    const end = agent.session.snapshotEvents().findLast(event => event.type === 'turn/end')
    const reason = end?.data?.reason?.kind
    const resultState = state.toolFailure || state.failed ? 'failed' : ipc.cancelled || reason === 'aborted' ? 'cancelled' : reason === 'completed' ? 'completed_candidate' : 'failed'
    if (resultState === 'completed_candidate') await waitForBackgroundJobs()
    return { type: 'settled', ...identity, state: resultState, through_seq: durable.through }
  } finally {
    ipc.closeMessageDelivery()
    ipc.onCancel = undefined
    ipc.onFailure = undefined
    ipc.onJobCancel = undefined
    if (handle) {
      liveHandles.delete(handle)
      await handle.dispose().catch(() => {})
    }
    for (const remaining of liveHandles) {
      await remaining.dispose().catch(() => {})
    }
    liveHandles.clear()
  }
}

function delegatedSummary(agent) {
  const events = agent.session.snapshotEvents()
  const endIndex = events.findLastIndex(event => event.type === 'turn/end')
  const end = events[endIndex]
  const message = end && events.slice(0, endIndex + 1).findLast(event =>
    event.type === 'assistant/message' && event.data?.message?.role === 'assistant' &&
    (end.data?.turn === undefined || event.data.turn === end.data.turn))
  const content = message?.data?.message?.content
  return Array.isArray(content) ? content.filter(block => block?.type === 'text' && typeof block.text === 'string').map(block => block.text).join('').trim() : ''
}
