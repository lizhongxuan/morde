import { ApiError, requestFetch } from '../api/client';

export const messageVariantActions = ['edit_resend', 'regenerate_text', 'continue_generation'] as const;
export const maxMessageVariantScalars = 32_768;
export type MessageVariantAction = typeof messageVariantActions[number];
export type MessageVariantState = {
  operation_id: string;
  variant_group_id: string;
  variant_version: number;
  action: MessageVariantAction;
  state: 'accepted' | 'running' | 'completed' | 'failed' | 'unknown';
  task_id?: string;
  user_message_id?: string;
  assistant_message_id?: string;
};

export type CreateMessageVariantInput = {
  conversationID: string;
  messageID: string;
  action: MessageVariantAction;
  expectedVersion: number;
  content?: string;
  idempotencyKey: string;
  serviceInstanceID: string;
  actorID: string;
  projectID?: string;
};

export function messageVariantContentTooLong(content: string): boolean {
  return Array.from(content).length > maxMessageVariantScalars;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return Boolean(value) && typeof value === 'object' && !Array.isArray(value);
}

function readVariantResponse(value: unknown): MessageVariantState {
  if (!isRecord(value) || typeof value.operation_id !== 'string' || !value.operation_id ||
    typeof value.variant_group_id !== 'string' || !value.variant_group_id ||
    !Number.isSafeInteger(value.variant_version) || Number(value.variant_version) < 2 ||
    !messageVariantActions.includes(value.action as MessageVariantAction) ||
    !['accepted', 'running', 'completed', 'failed', 'unknown'].includes(String(value.state))) {
    throw new Error('消息版本操作响应格式无效。');
  }
  for (const field of ['task_id', 'user_message_id', 'assistant_message_id'] as const) {
    if (value[field] !== undefined && (typeof value[field] !== 'string' || !value[field])) {
      throw new Error('消息版本操作响应格式无效。');
    }
  }
  return value as MessageVariantState;
}

async function throwVariantResponseError(response: Response): Promise<never> {
  let payload: unknown;
  try { payload = await response.json(); } catch { payload = undefined; }
  const outer = isRecord(payload) && isRecord(payload.error) ? payload.error : {};
  const retryAfter = Number(response.headers.get('Retry-After'));
  throw new ApiError({
    message: typeof outer.message === 'string' ? outer.message : '消息版本操作失败，请核对对话状态后重试。',
    code: typeof outer.code === 'string' ? outer.code : 'internal',
    status: response.status,
    retryAfter: Number.isFinite(retryAfter) && retryAfter > 0 ? retryAfter : undefined,
    details: isRecord(outer.details) ? outer.details : {},
  });
}

export async function createMessageVariant(input: CreateMessageVariantInput): Promise<MessageVariantState> {
  if (!input.serviceInstanceID || !input.actorID) throw new Error('服务或账号身份尚未确认。');
  if (!input.idempotencyKey) throw new Error('缺少消息版本操作编号。');
  if (!Number.isSafeInteger(input.expectedVersion) || input.expectedVersion < 1) throw new Error('消息版本已变化，请刷新后重试。');
  if (input.action === 'edit_resend' && !input.content?.trim()) throw new Error('编辑后的消息不能为空。');
  if (input.action === 'edit_resend' && input.content && messageVariantContentTooLong(input.content)) throw new Error('编辑后的消息不能超过 32768 个字符。');
  if (input.action !== 'edit_resend' && input.content !== undefined) throw new Error('此消息版本操作不接受编辑内容。');
  const prefix = input.projectID
    ? `/api/projects/${encodeURIComponent(input.projectID)}/conversations/${encodeURIComponent(input.conversationID)}/messages`
    : `/api/conversations/${encodeURIComponent(input.conversationID)}/messages`;
  const path = `${prefix}/${encodeURIComponent(input.messageID)}/variants`;
  const response = await requestFetch(path, {
    method: 'POST',
    headers: {
      'Idempotency-Key': input.idempotencyKey,
      'X-Morde-Service-Instance-ID': input.serviceInstanceID,
      'X-Morde-Actor-ID': input.actorID,
    },
    body: JSON.stringify({
      action: input.action,
      expected_version: input.expectedVersion,
      ...(input.content !== undefined ? { content: input.content } : {}),
    }),
  });
  if (!response.ok) await throwVariantResponseError(response);
  let value: unknown;
  try { value = await response.json(); } catch { throw new Error('消息版本操作响应格式无效。'); }
  const result = readVariantResponse(value);
  if (result.action !== input.action) throw new Error('服务端返回的消息版本操作与请求不一致。');
  return result;
}

export type VersionableMessage = {
  id: string;
  sequence: number;
  role: 'user' | 'assistant';
  content: string;
  task_id?: string;
  related_task_id?: string;
  output_status?: 'streaming' | 'interrupted' | 'completed';
  sources?: { user_message_id: string }[];
  variant_group_id?: string;
  variant_version?: number;
  variant_action?: MessageVariantAction | 'original';
};

export type MessageVariantVersion<T> = { version: number; action: MessageVariantAction | 'original'; messages: T[] };
export type MessageVariantGroup<T> = {
  id: string;
  versions: Map<number, MessageVariantVersion<T>>;
  latestVersion: number;
  anchorSequence: number;
  messageIDs: Set<string>;
};
export type MessageVariantTimeline<T> = {
  items: T[];
  groups: Map<string, MessageVariantGroup<T>>;
  groupByMessageID: Map<string, string>;
};

/** Build the visible timeline by replacing each source turn with one immutable version. */
export function buildMessageVariantTimeline<T extends VersionableMessage>(
  input: readonly T[], selectedVersions: ReadonlyMap<string, number> = new Map(),
): MessageVariantTimeline<T> {
  const messages = [...input].sort((left, right) => left.sequence - right.sequence || left.id.localeCompare(right.id));
  const userMessageIDs = new Set(messages.filter((message) => message.role === 'user').map((message) => message.id));
  const usersByTask = new Map<string, T[]>();
  for (const message of messages) {
    if (message.role !== 'user') continue;
    for (const taskID of [message.task_id, message.related_task_id].filter((value): value is string => Boolean(value))) {
      usersByTask.set(taskID, [...(usersByTask.get(taskID) ?? []), message]);
    }
  }
  const groupByMessageID = new Map<string, string>();
  const versionByMessageID = new Map<string, { version: number; action: MessageVariantAction | 'original' }>();
  for (const message of messages) {
    if (message.variant_group_id) {
      const version = Number.isSafeInteger(message.variant_version) && (message.variant_version ?? 0) > 0 ? message.variant_version! : 1;
      groupByMessageID.set(message.id, message.variant_group_id);
      versionByMessageID.set(message.id, { version, action: message.variant_action ?? (version === 1 ? 'original' : 'edit_resend') });
    } else if (message.role === 'user') {
      groupByMessageID.set(message.id, message.id);
      versionByMessageID.set(message.id, { version: 1, action: 'original' });
    } else {
      const sourceGroups = [...new Set((message.sources ?? []).map((source) => source.user_message_id)
        .filter((id) => userMessageIDs.has(id)))];
      let rootMessageID = sourceGroups.length === 1 ? sourceGroups[0] : undefined;
      if (!rootMessageID && message.task_id) {
        const candidates = [...new Set((usersByTask.get(message.task_id) ?? []).map((candidate) => candidate.id))];
        if (candidates.length === 1) rootMessageID = candidates[0];
      }
      if (rootMessageID) {
        groupByMessageID.set(message.id, rootMessageID);
        versionByMessageID.set(message.id, { version: 1, action: 'original' });
      }
    }
  }

  const groups = new Map<string, MessageVariantGroup<T>>();
  for (const message of messages) {
    const groupID = groupByMessageID.get(message.id);
    if (!groupID) continue;
    const versionInfo = versionByMessageID.get(message.id) ?? { version: 1, action: 'original' as const };
    let group = groups.get(groupID);
    if (!group) {
      group = { id: groupID, versions: new Map(), latestVersion: 1, anchorSequence: Number.MAX_SAFE_INTEGER, messageIDs: new Set() };
      groups.set(groupID, group);
    }
    const version = group.versions.get(versionInfo.version) ?? { version: versionInfo.version, action: versionInfo.action, messages: [] };
    if (versionInfo.action !== 'original') version.action = versionInfo.action;
    version.messages.push(message);
    group.versions.set(versionInfo.version, version);
    group.latestVersion = Math.max(group.latestVersion, versionInfo.version);
    group.anchorSequence = Math.min(group.anchorSequence, message.sequence);
    group.messageIDs.add(message.id);
  }

  const selected: Array<{ sequence: number; ordinal: number; message: T }> = [];
  const inserted = new Set<string>();
  let ordinal = 0;
  for (const message of messages) {
    const groupID = groupByMessageID.get(message.id);
    if (!groupID) {
      selected.push({ sequence: message.sequence, ordinal: ordinal++, message });
      continue;
    }
    if (inserted.has(groupID)) continue;
    inserted.add(groupID);
    const group = groups.get(groupID)!;
    const requested = selectedVersions.get(groupID) ?? group.latestVersion;
    const chosen = group.versions.get(requested) ?? group.versions.get(1) ?? [...group.versions.values()].sort((a, b) => a.version - b.version)[0];
    for (const [index, candidate] of (chosen?.messages ?? []).entries()) {
      selected.push({ sequence: group.anchorSequence + index * 0.0001, ordinal: ordinal++, message: candidate });
    }
  }
  return {
    items: selected.sort((left, right) => left.sequence - right.sequence || left.ordinal - right.ordinal).map(({ message }) => message),
    groups,
    groupByMessageID,
  };
}

export function messageVariantActionLabel(action: MessageVariantAction | 'original'): string {
  if (action === 'edit_resend') return '编辑重发';
  if (action === 'regenerate_text') return '重新生成';
  if (action === 'continue_generation') return '继续生成';
  return '原始';
}
