import { Fragment, useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { Link, useLocation, useNavigate } from 'react-router';
import { useInfiniteQuery, useMutation, useQueries, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, newIdempotencyKey, portalApi } from '../app/api';
import type { ConversationDeliveryStatus } from '../app/api';
import { readK2TaskProjection } from '../app/k2-task-projection';
import type { K2ResultReview, K2TaskDependency } from '../app/k2-task-projection';
import type { MessageOutboxEntry } from '../app/use-message-outbox';
import { useMessageOutbox } from '../app/use-message-outbox';
import { deliveryStatusText, routingFailureText, routingStatusText } from '../app/message-status';
import { clearProcessingRetryKey, processingRetryKey } from '../app/message-processing-retry';
import { maxMessageBytes, needsMessageUpload, uploadMessage } from '../app/message-upload';
import { outboxReceiptRequest } from '../app/message-outbox';
import { MessageReceiptOrder } from '../app/message-receipt-order';
import type { PendingConversationMessage, PendingMessageAttachment } from '../app/message-outbox';
import { ApiError } from '../api/client';
import { useAuth } from '../app/auth';
import { formatBytes, formatDate, hostStateLabel } from '../app/format';
import { useToast } from '../app/toast';
import { AssistantVoiceInput } from '../components/AssistantVoiceInput';
import { ChatMarkdown } from '../components/ChatMarkdown';
import { MordeIcon } from '../components/MordeIcon';
import { sha256Hex } from './personal-data-store';
import { extractAssistantPDFText } from './assistant-pdf';

type Conversation = Awaited<ReturnType<typeof portalApi.conversations>>['items'][number];
type ConversationMessage = Awaited<ReturnType<typeof portalApi.conversationMessages>>['items'][number];
type TaskSummary = Awaited<ReturnType<typeof portalApi.tasks>>['items'][number];
type Task = Awaited<ReturnType<typeof portalApi.task>>;
type TaskFileFetch = Awaited<ReturnType<typeof portalApi.taskFileFetches>>['items'][number];
type Host = Awaited<ReturnType<typeof portalApi.hosts>>['items'][number];
type WorkspaceBinding = Host['workspaces'][number];
type AssistantDisplayMessage = Omit<ConversationMessage, 'delivery_status'> & { delivery_status?: ConversationDeliveryStatus } & {
  snapshotStatus?: 'streaming' | 'completed' | 'interrupted';
};
type ComposerAttachment = { id: string; file: File; displayName?: string; objectID: string; objectKey: string; uploadKey: string; commitKey: string; attachmentID?: string };
type OfficePreviewState = { status: 'loading' } | { status: 'ready'; text: string } | { status: 'error'; message: string };

const assistantAttachmentTypes = new Set([
  'image/jpeg', 'image/png', 'text/plain',
  'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
  'application/vnd.openxmlformats-officedocument.presentationml.presentation',
]);
const assistantOfficeAttachmentTypes = new Set([
  'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
  'application/vnd.openxmlformats-officedocument.presentationml.presentation',
]);
const attachmentChunkBytes = 4 * 1024 * 1024;

function assistantFileMediaType(file: File): string {
  if (assistantAttachmentTypes.has(file.type)) return file.type;
  const extension = file.name.toLowerCase().split('.').at(-1);
  return ({ txt: 'text/plain', jpg: 'image/jpeg', jpeg: 'image/jpeg', png: 'image/png',
    docx: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
    xlsx: 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
    pptx: 'application/vnd.openxmlformats-officedocument.presentationml.presentation', pdf: 'application/pdf' } as Record<string, string>)[extension ?? ''] ?? file.type;
}

function assistantFileError(file: File, mediaType: string): string | null {
  if (!assistantAttachmentTypes.has(mediaType)) return '暂不支持读取此文件类型。可添加 PNG、JPEG、TXT、DOCX、XLSX 或 PPTX。';
  if (file.size < 1) return '空文件无法读取。';
  const maxBytes = mediaType === 'image/jpeg' || mediaType === 'image/png' ? 512 * 1024
    : mediaType === 'text/plain' ? 63 * 1024 : 16 * 1024 * 1024;
  if (file.size > maxBytes) return mediaType.startsWith('image/') ? '图片不能超过 512 KiB。'
    : mediaType === 'text/plain' ? 'TXT 文件不能超过 63 KiB。' : 'DOCX/XLSX/PPTX 文件不能超过 16 MiB。';
  return null;
}

async function uploadAssistantAttachment(item: PendingMessageAttachment, serviceInstanceID: string, actorID: string, checkScope: () => void): Promise<string> {
  if (item.attachmentID) return item.attachmentID;
  const file = new File([item.blob], item.name, { type: item.mediaType });
  const mediaType = assistantFileMediaType(file);
  const fileError = assistantFileError(file, mediaType);
  if (fileError) throw new Error(fileError);
  const bytes = await file.arrayBuffer();
  const digest = await sha256Hex(bytes);
  checkScope();
  const object = await portalApi.replacePersonalObject('library_item', item.objectID, 0, {
    name: file.name, kind: mediaType.startsWith('image/') ? 'media' : 'artifact', mime: mediaType, size_bytes: file.size,
  }, item.objectKey, actorID, serviceInstanceID);
  checkScope();
  const upload = await portalApi.createPersonalAttachmentUpload({
    object_id: item.objectID, base_revision: object.result_revision, size_bytes: file.size,
    sha256: digest, media_type: mediaType,
  }, item.uploadKey, serviceInstanceID, actorID);
  for (let offset = 0, sequence = 0; offset < bytes.byteLength; offset += attachmentChunkBytes, sequence += 1) {
    const chunk = bytes.slice(offset, Math.min(offset + attachmentChunkBytes, bytes.byteLength));
    const chunkDigest = await sha256Hex(chunk);
    checkScope();
    await portalApi.putPersonalAttachmentChunk(upload.upload_id, sequence, chunk, chunkDigest, serviceInstanceID, actorID);
  }
  checkScope();
  const committed = await portalApi.commitPersonalAttachment(upload.upload_id, item.commitKey, serviceInstanceID, actorID);
  return committed.attachment_id;
}

function sortAssistantMessages(messages: AssistantDisplayMessage[]): AssistantDisplayMessage[] {
  return messages.sort((left, right) => (left.sequence ?? Number.MAX_SAFE_INTEGER) - (right.sequence ?? Number.MAX_SAFE_INTEGER) ||
    left.created_at.localeCompare(right.created_at) || left.id.localeCompare(right.id));
}

async function conversationHistory(conversationID: string): Promise<{ items: ConversationMessage[]; next_cursor: null }> {
  const items = new Map<string, ConversationMessage>();
  const seen = new Set<string>();
  let cursor: string | undefined;
  for (let page = 0; page < 1000; page += 1) {
    const result = await portalApi.conversationMessages(conversationID, cursor);
    for (const message of result.items) if (!items.has(message.id)) items.set(message.id, message);
    if (!result.next_cursor) return { items: [...items.values()], next_cursor: null };
    if (seen.has(result.next_cursor)) throw new Error('消息分页游标重复，无法继续读取。');
    seen.add(result.next_cursor);
    cursor = result.next_cursor;
  }
  throw new Error('消息页数超过读取上限。');
}

export function assistantRecentConversationKey(serviceInstanceID: string, userID: string): string {
  return `morde.assistant.recentConversation.v1:${serviceInstanceID}:${userID}`;
}

export function resolveTaskObservationURL(ticket: string, hostURL: string, hostID: string, portalHostname = window.location.hostname): string {
  let host: URL;
  try { host = new URL(hostURL); } catch { throw new Error('服务器返回了无效的 Workbench 地址。'); }
  const localHTTP = (portalHostname === 'portal.localhost' || portalHostname === 'localhost')
    && host.protocol === 'http:' && host.hostname === `${hostID}.hosts.localhost` && !host.port;
  // eslint-disable-next-line no-control-regex -- Control bytes must not enter an opaque credential URL.
  if (!ticket || ticket.length < 16 || ticket.length > 4096 || /[\u0000-\u001f\u007f]/.test(ticket)
    || (!localHTTP && host.protocol !== 'https:') || host.username || host.password || host.hash || host.search || host.pathname !== '/') {
    throw new Error('服务器返回了无效的 Workbench 地址或观察票据。');
  }
  const entry = new URL('/__ch/task-auth', host);
  entry.searchParams.set('ticket', ticket);
  return entry.href;
}

function readRecentConversation(key: string): string | null {
  try { return window.localStorage.getItem(key); } catch { return null; }
}

function writeRecentConversation(key: string, conversationID: string | null): void {
  try {
    if (conversationID) window.localStorage.setItem(key, conversationID);
    else window.localStorage.removeItem(key);
  } catch { /* the conversation remains usable for this page session */ }
}

export function mergeTaskAssistantMessage(items: AssistantDisplayMessage[], task: Task | undefined): AssistantDisplayMessage[] {
  const messages: AssistantDisplayMessage[] = items.map((message) => message.output_status
    ? { ...message, snapshotStatus: message.output_status } : message);
  const snapshot = task?.assistant_message;
  const k2Read = readK2TaskProjection(task);
  const currentK2Snapshot = k2Read.kind !== 'available' || (task?.current_revision !== undefined &&
    snapshot?.task_revision === task.current_revision);
  // K2 publication is represented by its durable conversation message; a Task snapshot is never a fallback final reply.
  if (k2Read.kind === 'invalid' || (k2Read.kind === 'available' && (snapshot?.status !== 'streaming'
    || !task || terminalTaskStatuses.has(task.status) || !currentK2Snapshot))) {
    return sortAssistantMessages(messages);
  }
  if ((task?.result_message_pending || task?.result_message_published) &&
    snapshot?.status !== 'streaming' && snapshot?.status !== 'interrupted') {
    return sortAssistantMessages(messages);
  }
  if (snapshot?.role === 'assistant') {
    const existingIndex = messages.findIndex((message) => message.id === snapshot.id);
    if (existingIndex >= 0) {
      const existing = messages[existingIndex];
      if (existing?.output_status === 'streaming' && existing.output_task_revision !== undefined &&
        existing.output_task_revision === snapshot.task_revision && snapshot.content.startsWith(existing.content)) {
        // A Task event can arrive after the conversation page was read. Keep
        // its sequence, but use the newer verified prefix for the same revision.
        messages[existingIndex] = {
          ...existing,
          content: snapshot.content,
          snapshotStatus: resolveTaskSnapshotStatus(task, snapshot.status),
        };
      } else if (existing) {
        // A persisted completion or interruption cannot be reverted by a stale snapshot.
        messages[existingIndex] = {
          ...existing,
          snapshotStatus: resolveTaskSnapshotStatus(task, existing.output_status ?? 'completed'),
        };
      }
    } else {
      const coveredByCombinedMessage = snapshot.task_revision !== undefined && snapshot.result_event_sequence !== undefined
        && messages.some((message) => message.role === 'assistant' && message.sources?.some((source) =>
          source.task_id === task?.id && snapshot.task_id === task?.id
          && source.task_revision === snapshot.task_revision && source.event_sequence === snapshot.result_event_sequence));
      if (coveredByCombinedMessage) return sortAssistantMessages(messages);
      const sequence = snapshot.sequence ?? messages.reduce((latest, message) => Math.max(latest, message.sequence), 0) + 1;
      messages.push({ ...snapshot, sequence, snapshotStatus: resolveTaskSnapshotStatus(task, snapshot.status) });
    }
  }
  return sortAssistantMessages(messages);
}

function mergeTaskAssistantMessages(items: ConversationMessage[], tasks: Task[]): AssistantDisplayMessage[] {
  const initial: AssistantDisplayMessage[] = items.map((message) => message.output_status
    ? { ...message, snapshotStatus: message.output_status } : message);
  return tasks.reduce<AssistantDisplayMessage[]>((current, task) => mergeTaskAssistantMessage(current, task), initial);
}

const terminalTaskStatuses = new Set<Task['status']>(['completed', 'failed', 'cancelled']);
const activeTaskFileFetchStatuses = new Set<TaskFileFetch['status']>(['queued', 'waiting_device', 'transferring', 'verifying', 'cancelling']);
const taskFileFetchStatusLabel: Record<TaskFileFetch['status'], string> = {
  queued: '排队中', waiting_device: '等待设备', transferring: '正在传输', verifying: '正在核验', completed: '已完成',
  cancelling: '正在取消', cancelled: '已取消', failed: '失败',
};
const taskFileFetchContentStateLabel: Record<TaskFileFetch['content_state'], string> = {
  none: '无服务端副本', retained: '服务端副本保留中', deleted: '服务端副本已删除', expired: '服务端副本已到期',
};
// shortcut: stream four tasks per tab; the rest use 1.5s snapshot polling until task events are multiplexed.
const maxTaskEventStreams = 4;
function resolveTaskSnapshotStatus(task: Task | undefined, status: AssistantDisplayMessage['snapshotStatus']): AssistantDisplayMessage['snapshotStatus'] {
  if (status === 'streaming' && task && terminalTaskStatuses.has(task.status)) {
    return task.status === 'completed' ? 'completed' : 'interrupted';
  }
  return status;
}

function latestConversationClientMessageID(messages: ConversationMessage[] | undefined): string | undefined {
  return messages?.reduce<ConversationMessage | undefined>((latest, current) => {
    if (current.role !== 'user' || !current.client_message_id) return latest;
    return !latest || current.sequence > latest.sequence ? current : latest;
  }, undefined)?.client_message_id;
}

const controlStateLabels: Record<Host['control_state'], string> = {
  ai: '云端 AI 控制', taking_over: '正在接管', user: '人工控制', returning: '正在交还 AI',
};
function workspaceBindingReady(binding: WorkspaceBinding): boolean {
  return binding.state === 'active' && binding.workspace.state === 'ready' && binding.sync_state === 'healthy' &&
    binding.applied_revision === binding.desired_revision && binding.applied_revision === binding.workspace.revision;
}

function taskTargetText(task: TaskSummary, hosts: Host[]): string {
  const host = task.host_id ? hosts.find((item) => item.id === task.host_id) : undefined;
  const computer = task.host_id ? host ? `${host.name || host.id}（${hostStateLabel[host.state] || host.state}）` : `主机 ${task.host_id}（当前不可访问）` : '无电脑目标';
  const binding = task.workspace_id ? host?.workspaces.find((item) => item.workspace.id === task.workspace_id) : undefined;
  const workspace = task.workspace_id ? `${binding?.workspace.name || task.workspace_id}${task.input_revision === undefined ? '' : ` v${task.input_revision}`}` : '无 Workspace';
  return `${computer} · ${workspace}`;
}

function taskStatusText(task: Task, cloudTasksAvailable: boolean | undefined): string {
  if (task.status === 'queued') {
    return cloudTasksAvailable === false
      ? '任务已保存并排队；当前服务未启用云端执行，启用后才会开始运行。'
      : '任务已保存，正在等待开始。关闭页面不会取消任务。';
  }
  if (task.status === 'running') {
    if (task.current_revision !== undefined && task.desired_revision !== undefined && task.current_revision !== task.desired_revision) {
      return '正在调整任务，等待开始新要求。';
    }
    if (task.current_revision_started === true && task.current_revision !== undefined && task.current_revision === task.desired_revision) {
      return '开始处理你的请求。';
    }
    if (task.current_revision_started !== undefined) return '任务已保存，正在等待开始。';
    return 'AI 正在处理…';
  }
  if (task.status === 'waiting_user' && task.waiting_reason === 'computer_target_missing') return '尚未选择云端电脑，本次操作未执行。';
  if (task.status === 'waiting_user' && task.waiting_reason === 'computer_target_changed') return '任务已绑定其他云端电脑，无法安全切换；请为新目标另发起任务。';
  if (task.status === 'waiting_user' && task.waiting_reason === 'computer_unavailable') return '所选云端电脑当前不可用或无法确认访问权限，本次操作未执行。';
  if (task.status === 'waiting_user' && task.waiting_reason === 'workspace_unavailable') return '所选 Workspace 当前不可用、版本未就绪或无法确认访问权限，本次操作未执行。';
  if (task.status === 'waiting_user' && task.waiting_reason === 'workspace_target_changed') return '任务已绑定其他 Workspace，无法安全切换；请为新目标另发起任务。';
  if (task.status === 'waiting_user' && task.waiting_reason === 'target_result_unknown') return '上次电脑操作的结果需要核对，暂不能重复执行。';
  if (task.status === 'waiting_user' && task.waiting_reason === 'workspace_host_sync') return '任务等待目标云端电脑重新就绪并同步 Workspace；确认版本已应用后才会完成。';
  if (task.status === 'waiting_user' && task.waiting_reason === 'model_context_limit') return '消息及上下文超过当前模型输入上限，原文已保留；请缩短后重新发送。';
  if (task.status === 'waiting_user' && task.waiting_reason === 'dsh_recovery_unresolved') return 'AI 运行中断，无法确认本次执行结果。请停止任务后重试。';
  if (task.status === 'waiting_user') return '任务正在等待你的确认或补充信息。';
  if (task.status === 'paused') return '任务已暂停。';
  if (task.status === 'pausing') return '正在安全暂停任务。';
  if (task.status === 'cancelling') {
    const unsettled = task.stop_request?.unsettled_operation_ids.length ?? 0;
    if (unsettled > 0) return `停止请求已受理，仍有 ${unsettled} 个操作结果待确认。`;
    if (task.stop_request?.status === 'pending') return '正在等待服务端确认停止结果。';
    if (task.stop_request?.status === 'completed') return '服务端已处理停止请求，正在等待最终任务状态。';
    return '停止状态待确认，正在重新读取服务端任务。';
  }
  if (task.status === 'failed') return task.result || '任务执行失败。';
  if (task.status === 'cancelled') return '服务端已确认任务停止。';
  if (task.result_message_pending || task.result_message_published) return '任务已完成，正在整理回复。';
  return task.result || '任务已完成。';
}

function recoveryHint(step: Task['steps'][number] | undefined): string | null {
  if (step?.title !== 'Review cloud task recovery') return null;
  const category = /^The model request could not be verified \(category: ([a-z_]+)\); retry after checking the model connection\.$/.exec(step.summary)?.[1];
  switch (category) {
    case 'provider_rate_limited':
    case 'rate_limited':
      return '模型服务正在限流，稍后再试。';
    case 'provider_invalid_api_key':
    case 'authentication_rejected':
      return '模型密钥未通过服务商验证，请检查模型连接。';
    case 'provider_quota_exhausted':
    case 'quota_exhausted':
    case 'task_budget_exhausted':
      return '模型额度已用尽，请检查额度后重试。';
    case 'provider_model_not_found':
      return '服务商未找到此模型，请检查模型名称。';
    case 'provider_permission_denied':
    case 'access_rejected':
      return '此 API Key 无权调用该模型，请检查模型权限。';
    case 'timeout':
    case 'transport':
    case 'unavailable':
    case 'upstream_error':
    case 'provider_server_error':
      return '模型服务暂时不可用，请稍后重试。';
    default:
      return null;
  }
}

function isTransientNetworkError(error: unknown): boolean {
  if (error instanceof SyntaxError) return true;
  if (error instanceof TypeError && /failed to fetch|networkerror|load failed|network request failed/i.test(error.message)) return true;
  return error instanceof ApiError && error.status >= 500;
}

const taskEventTypes = [
  'task.status', 'task.step', 'task.operation', 'task.message', 'task.batch_item',
  'task.artifact', 'task.waiting_user', 'task.response', 'task.result', 'task.snapshot_required',
] as const;

function stepTitle(step: Task['steps'][number]): string {
  const prefix = ['Run cloud dsh tool: ', 'Run cloud dsh MCP tool: ', 'Run Hostless Workspace tool: ']
    .find((value) => step.title.startsWith(value));
  if (!prefix) return step.title;
  const name = step.title.slice(prefix.length);
  if (name === 'morde_web_search' || name.includes('searxng_web_search')) return '搜索网页';
  if (name === 'morde_web_read' || name.includes('web_url_read')) return '读取网页';
  if (name === 'morde_workspace_list') return '查看工作区文件';
  if (name === 'morde_workspace_read') return '读取工作区文件';
  if (name === 'morde_workspace_write') return '保存工作区文件';
  if (name.startsWith('browser_')) return '操作浏览器';
  if (name.startsWith('desktop_')) return '操作云端电脑';
  if (name.startsWith('file_')) return '处理文件';
  if (name === 'shell_exec') return '运行命令';
  return '使用工具';
}

const stepStatus: Record<Task['steps'][number]['status'], string> = {
  queued: '等待中', running: '进行中', waiting_user: '等待确认', succeeded: '已完成', failed: '失败', unknown: '结果待确认',
};

const taskStatusLabel: Record<Task['status'], string> = {
  queued: '排队中', running: '运行中', waiting_user: '等待用户', pausing: '正在暂停', paused: '已暂停',
  cancelling: '正在停止', completed: '已完成', failed: '执行失败', cancelled: '已停止',
};

function dependencyStatusText(dependency: K2TaskDependency): string {
  return dependency.satisfied
    ? `已满足：任务 ${dependency.task_id} 修订 ${dependency.required_revision}（${taskStatusLabel[dependency.task_status]}）`
    : `等待任务 ${dependency.task_id} 修订 ${dependency.required_revision}（当前状态：${taskStatusLabel[dependency.task_status]}）`;
}

function resultReviewStatusText(review: K2ResultReview, replyVisible: boolean): string {
  switch (review.state) {
    case 'pending': return '原始结果已保存，等待主助手整理。';
    case 'processing': return '主助手正在整理原始结果。';
    case 'published': return replyVisible ? '最终回复已同步到会话。' : '最终回复已发布，正在从会话历史补取。';
    case 'failed': return '结果整理失败；原始任务结果仍保留，尚未发布最终回复。';
  }
}

type TaskFeedState = 'connected' | 'disconnected' | 'resyncing';

function TaskStream({ task, conversationID, onState }: { task: Task; conversationID: string; onState: (id: string, state: TaskFeedState) => void }) {
  const client = useQueryClient();
  const cursor = useRef(task.event_sequence);
  const [generation, setGeneration] = useState(0);
  const terminal = terminalTaskStatuses.has(task.status);
  useEffect(() => { cursor.current = Math.max(cursor.current, task.event_sequence); }, [task.event_sequence]);
  useEffect(() => {
    if (terminal || typeof EventSource === 'undefined') return;
    const source = new EventSource(`/api/tasks/${encodeURIComponent(task.id)}/events?after=${cursor.current}`);
    let disposed = false;
    let retry: ReturnType<typeof setTimeout> | undefined;
    const resync = () => {
      source.close();
      onState(task.id, 'resyncing');
      void client.fetchQuery({ queryKey: ['task', task.id], queryFn: () => portalApi.task(task.id), staleTime: 0 }).then((snapshot) => {
        if (!disposed) {
          cursor.current = snapshot.event_sequence;
          void client.invalidateQueries({ queryKey: ['conversation-messages', conversationID] });
          void client.invalidateQueries({ queryKey: ['task-file-fetches', task.id] });
          setGeneration((value) => value + 1);
        }
      }).catch(() => {
        if (!disposed) {
          onState(task.id, 'disconnected');
          retry = setTimeout(() => setGeneration((value) => value + 1), 3000);
        }
      });
    };
    source.onopen = () => onState(task.id, 'connected');
    source.onerror = () => onState(task.id, 'disconnected');
    for (const type of taskEventTypes) {
      source.addEventListener(type, (event) => {
        let value: unknown;
        try { value = JSON.parse((event as MessageEvent<string>).data); } catch { return; }
        if (type === 'task.snapshot_required') {
          resync();
          return;
        }
        if (!value || typeof value !== 'object' || !('task_id' in value) || !('sequence' in value) ||
          value.task_id !== task.id || typeof value.sequence !== 'number' || !Number.isSafeInteger(value.sequence) || value.sequence <= cursor.current) return;
        if (value.sequence > cursor.current + 1) {
          resync();
          return;
        }
        cursor.current = value.sequence;
        onState(task.id, 'connected');
        void client.invalidateQueries({ queryKey: ['task', task.id] });
        void client.invalidateQueries({ queryKey: ['task-file-fetches', task.id] });
        if (type === 'task.status' || type === 'task.waiting_user') void client.invalidateQueries({ queryKey: ['assistant-account-tasks'] });
        if (type === 'task.message' || type === 'task.result' || type === 'task.response') {
          void client.invalidateQueries({ queryKey: ['conversation-messages', conversationID] });
        }
      });
    }
    return () => { disposed = true; if (retry) clearTimeout(retry); source.close(); };
  }, [client, conversationID, generation, onState, task.id, terminal]);
  return null;
}

export function Assistant() {
  const client = useQueryClient();
  const { notify } = useToast();
  const { user } = useAuth();
  const location = useLocation();
  const navigate = useNavigate();
  const navigationIdea = typeof location.state === 'object' && location.state !== null
    ? location.state as { ideaID?: unknown; ideaPrompt?: unknown }
    : null;
  const ideaPrompt = typeof navigationIdea?.ideaPrompt === 'string' ? navigationIdea.ideaPrompt : '';
  const hasNavigationIdea = typeof navigationIdea?.ideaID === 'string' && Boolean(navigationIdea.ideaID) && Boolean(ideaPrompt);
  const [conversationSelection, setConversationSelection] = useState<{ scope: string; id: string | null } | null>(null);
  const [hostSelection, setHostSelection] = useState<string | null>(null);
  const [workspaceSelection, setWorkspaceSelection] = useState('');
  const [modelID, setModelID] = useState('');
  const [mcpConnectorIDs, setMcpConnectorIDs] = useState<string[]>([]);
  const [content, setContent] = useState(() => hasNavigationIdea ? ideaPrompt : '');
  const [savingMessage, setSavingMessage] = useState(false);
  const [composerAttachments, setComposerAttachments] = useState<ComposerAttachment[]>([]);
  const [officePreviews, setOfficePreviews] = useState<Record<string, OfficePreviewState>>({});
  const [preparingAttachmentCount, setPreparingAttachmentCount] = useState(0);
  const [clarificationAnswers, setClarificationAnswers] = useState<Record<string, string>>({});
  const [taskSupplementDrafts, setTaskSupplementDrafts] = useState<Record<string, string>>({});
  const [supplementTaskID, setSupplementTaskID] = useState<string | null>(null);
  const [expandedTaskIDs, setExpandedTaskIDs] = useState<string[]>([]);
  const [taskPanelOpen, setTaskPanelOpen] = useState(false);
  const [sideConversationsOpen, setSideConversationsOpen] = useState(false);
  const [showJumpToLatest, setShowJumpToLatest] = useState(false);
  const [openingTaskID, setOpeningTaskID] = useState('');
  const [taskObservationError, setTaskObservationError] = useState<{ taskID: string; message: string } | null>(null);
  const taskObservationRequest = useRef(false);
  const pendingMessageSave = useRef<Promise<void>>(Promise.resolve());
  const savingMessageRef = useRef(false);
  const preparingAttachmentCountRef = useRef(0);
  const composerGeneration = useRef(0);
  const pdfPreparationControllers = useRef(new Set<AbortController>());
  const sendingMessageIDs = useRef(new Set<string>());
  const receiptOrder = useRef(new MessageReceiptOrder());
  const newConversationKey = useRef<string | null>(null);
  const pendingTaskActions = useRef(new Map<string, { fingerprint: string; key: string }>());
  const ensureMainKey = useRef<{ userID: string; key: string } | null>(null);
  const ensureAttemptedUserID = useRef<string | null>(null);
  const conversationView = useRef<HTMLDivElement>(null);
  const taskPanelToggle = useRef<HTMLButtonElement>(null);
  const followConversation = useRef(true);
  useEffect(() => () => {
    for (const controller of pdfPreparationControllers.current) controller.abort();
    pdfPreparationControllers.current.clear();
  }, []);
  const [taskEventFeeds, setTaskEventFeeds] = useState<Record<string, TaskFeedState>>({});
  const [visibleSideConversationCount, setVisibleSideConversationCount] = useState(5);
  const setTaskEventFeed = useCallback((taskID: string, state: TaskFeedState) => {
    setTaskEventFeeds((current) => current[taskID] === state ? current : { ...current, [taskID]: state });
  }, []);
  const capabilities = useQuery({ queryKey: ['service-capabilities'], queryFn: portalApi.capabilities });
  const outbox = useMessageOutbox(user?.id, capabilities.data?.service_instance_id);
  const activeOutboxScope = useRef(outbox.currentScope);
  useLayoutEffect(() => { activeOutboxScope.current = outbox.currentScope; }, [outbox.currentScope]);
  const conversations = useInfiniteQuery({
    queryKey: ['conversations'],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) => portalApi.conversations(pageParam, 5),
    getNextPageParam: (page) => page.next_cursor ?? undefined,
  });
  const mainConversation = useQuery({
    queryKey: ['main-conversation', user?.id], queryFn: portalApi.mainConversation,
    enabled: Boolean(user), retry: false,
  });
  const models = useQuery({ queryKey: ['model-connections'], queryFn: portalApi.modelConnections, staleTime: 60_000 });
  const mcpConnectors = useQuery({ queryKey: ['mcp-connectors'], queryFn: portalApi.mcpConnectors, retry: false });
  const availableModels = useQuery({ queryKey: ['available-models'], queryFn: portalApi.availableModels });
  const preferences = useQuery({ queryKey: ['user-preferences'], queryFn: portalApi.preferences });
  const hosts = useQuery({
    queryKey: ['assistant-hosts'], queryFn: portalApi.hosts,
    refetchInterval: (query) => query.state.data?.items.some((host) => ['creating', 'stopping', 'deleting'].includes(host.state)) ? 3000 : 30000,
  });
  const execution = useQuery({ queryKey: ['execution-control'], queryFn: portalApi.execution, refetchInterval: 15000,
    enabled: capabilities.data?.supported_features.includes('cloud_tasks') === true });
  const conversationsData = [...new Map((conversations.data?.pages.flatMap((page) => page.items) ?? [])
    .map((item) => [item.id, item])).values()]
    .sort((left, right) => Date.parse(right.updated_at) - Date.parse(left.updated_at) || right.id.localeCompare(left.id));
  const ensureMainConversation = useMutation({
    mutationFn: portalApi.ensureMainConversation,
    onSuccess: (state) => client.setQueryData(['main-conversation', user?.id], state),
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const ensureMain = useCallback(() => {
    if (!user) return;
    ensureAttemptedUserID.current = user.id;
    if (ensureMainKey.current?.userID !== user.id) ensureMainKey.current = { userID: user.id, key: newIdempotencyKey() };
    ensureMainConversation.mutate(ensureMainKey.current.key);
  }, [ensureMainConversation, user]);
  useEffect(() => {
    if (user && mainConversation.data?.status === 'uninitialized' && ensureAttemptedUserID.current !== user.id) ensureMain();
  }, [ensureMain, mainConversation.data?.status, user]);
  const recentConversationKey = user && capabilities.data?.service_instance_id
    ? assistantRecentConversationKey(capabilities.data.service_instance_id, user.id) : null;
  const recentConversation = useQuery({
    queryKey: ['assistant-recent-conversation', recentConversationKey],
    queryFn: () => recentConversationKey ? readRecentConversation(recentConversationKey) : null,
    enabled: Boolean(recentConversationKey),
    staleTime: Infinity,
  });
  const conversationScope = `${capabilities.data?.service_instance_id ?? window.location.origin}:${user?.id ?? 'anonymous'}`;
  const scopedConversationSelection = conversationSelection?.scope === conversationScope ? conversationSelection : null;
  const mainConversationID = mainConversation.data?.status === 'ready' ? mainConversation.data.conversation_id : null;
  const recentConversationID = recentConversation.data && conversationsData.some((item) => item.id === recentConversation.data)
    ? recentConversation.data : null;
  const conversationID = scopedConversationSelection ? scopedConversationSelection.id : mainConversationID;
  const setConversationID = (id: string | null) => setConversationSelection({ scope: conversationScope, id });
  const selectedConversation = useRef(conversationID);
  useLayoutEffect(() => { selectedConversation.current = conversationID; }, [conversationID]);
  const conversationDetail = useQuery({
    queryKey: ['conversation', conversationID],
    queryFn: () => portalApi.conversation(conversationID!),
    enabled: Boolean(conversationID) && !conversationsData.some((item) => item.id === conversationID),
  });
  const mainConversationDetail = useQuery({
    queryKey: ['conversation', mainConversationID],
    queryFn: () => portalApi.conversation(mainConversationID!),
    enabled: Boolean(mainConversationID) && !conversationsData.some((item) => item.id === mainConversationID),
  });
  const mainConversationItem = conversationsData.find((item) => item.id === mainConversationID) ?? mainConversationDetail.data;
  const sideConversations = conversationsData.filter((item) => item.id !== mainConversationID);
  const messages = useQuery({
    queryKey: ['conversation-messages', conversationID],
    queryFn: () => conversationHistory(conversationID!),
    enabled: Boolean(conversationID),
    refetchInterval: (query) => query.state.data?.items.some((item) =>
      item.routing_status === 'pending' || item.routing_status === 'processing' || item.routing_status === 'waiting_adjustment') ? 2000 : false,
  });
  const conversationTasks = useQuery({
    queryKey: ['conversation-tasks', conversationID],
    queryFn: () => portalApi.conversationTasks(conversationID!),
    enabled: Boolean(conversationID),
    refetchInterval: 5000,
  });
  const accountTasksEnabled = Boolean(user?.id && capabilities.data?.service_instance_id);
  const accountTasks = useQuery({
    queryKey: ['assistant-account-tasks', capabilities.data?.service_instance_id, user?.id],
    queryFn: () => portalApi.tasks(),
    enabled: accountTasksEnabled,
    refetchInterval: 30000,
  });
  const accountTaskIDs = accountTasks.data?.items.map((item) => item.id) ?? [];
  const taskIDs = [...new Set([
    ...accountTaskIDs,
    ...(conversationTasks.data?.items.map((item) => item.id) ?? []),
    ...(messages.data?.items.flatMap((message) => message.task_id ? [message.task_id] : []) ?? []),
  ])];
  const currentConversationTaskIDs = new Set([
    ...(conversationTasks.data?.items.map((item) => item.id) ?? []),
    ...(messages.data?.items.flatMap((message) => message.task_id ? [message.task_id] : []) ?? []),
  ]);
  const taskQueries = useQueries({ queries: taskIDs.map((id) => ({
    queryKey: ['task', id], queryFn: () => portalApi.task(id),
    enabled: currentConversationTaskIDs.has(id) || expandedTaskIDs.includes(id),
    refetchInterval: (query: { state: { data: Task | undefined } }) => query.state.data?.result_message_pending ? 1000
      : query.state.data && terminalTaskStatuses.has(query.state.data.status) ? false
        : query.state.data && query.state.data.conversation_id !== conversationID ? 5000 : 1500,
  })) });
  const taskFileFetchQueries = useQueries({ queries: taskIDs.map((id, index) => ({
    queryKey: ['task-file-fetches', id], queryFn: () => portalApi.taskFileFetches(id),
    enabled: currentConversationTaskIDs.has(id) || expandedTaskIDs.includes(id),
    refetchInterval: (query: { state: { data: { items: TaskFileFetch[] } | undefined } }) => {
      const task = taskQueries[index]?.data;
      return query.state.data?.items.some((item) => activeTaskFileFetchStatuses.has(item.status)) ||
        (task && !terminalTaskStatuses.has(task.status)) ? 1000 : false;
    },
  })) });
  const allTasks = taskQueries.flatMap((query) => query.data ? [query.data] : []);
  const tasks = allTasks.filter((task) => task.conversation_id === conversationID);
  const allTasksByID = new Map(allTasks.map((task) => [task.id, task]));
  const accountTaskByID = new Map((accountTasks.data?.items ?? []).map((item) => [item.id, item]));
  const taskPanelIDs = [...new Set([...accountTaskIDs,
    ...(conversationTasks.data?.items.map((item) => item.id) ?? []),
    ...(messages.data?.items.flatMap((message) => message.task_id ? [message.task_id] : []) ?? []),
  ])];
  const taskPanelTasks = taskPanelIDs.flatMap((id) => {
    const summary = accountTaskByID.get(id) ?? conversationTasks.data?.items.find((item) => item.id === id) ?? allTasksByID.get(id);
    return summary ? [summary] : [];
  });
  const pendingTaskCount = accountTasks.data?.items.filter((item) => item.status === 'waiting_user').length ?? null;
  const resultsAwaitingMessages = tasks.some((task) => task.result_message_pending || (task.result_message_published &&
    !messages.data?.items.some((message) => message.sources?.some((source) => source.task_id === task.id &&
      source.task_revision === task.assistant_message?.task_revision && source.event_sequence === task.assistant_message?.result_event_sequence))));
  useEffect(() => {
    if (!conversationID || !resultsAwaitingMessages) return;
    const timer = window.setInterval(() => { void client.invalidateQueries({ queryKey: ['conversation-messages', conversationID] }); }, 1000);
    return () => window.clearInterval(timer);
  }, [client, conversationID, resultsAwaitingMessages]);
  const tasksByID = new Map(tasks.map((task) => [task.id, task]));
  const activePanelTasks = allTasks.filter((task) => !terminalTaskStatuses.has(task.status));
  const networkReadFailures = [
    { failed: capabilities.isError, error: capabilities.error, retry: () => capabilities.refetch() },
    { failed: conversations.isError, error: conversations.error, retry: () => conversations.refetch() },
    { failed: Boolean(user) && mainConversation.isError, error: mainConversation.error, retry: () => mainConversation.refetch() },
    { failed: models.isError, error: models.error, retry: () => models.refetch() },
    { failed: mcpConnectors.isError, error: mcpConnectors.error, retry: () => mcpConnectors.refetch() },
    { failed: preferences.isError, error: preferences.error, retry: () => preferences.refetch() },
    { failed: hosts.isError, error: hosts.error, retry: () => hosts.refetch() },
    { failed: execution.isError, error: execution.error, retry: () => execution.refetch() },
    { failed: Boolean(conversationID) && conversationDetail.isError, error: conversationDetail.error, retry: () => conversationDetail.refetch() },
    { failed: Boolean(conversationID) && messages.isError, error: messages.error, retry: () => messages.refetch() },
    { failed: Boolean(conversationID) && conversationTasks.isError, error: conversationTasks.error, retry: () => conversationTasks.refetch() },
    { failed: Boolean(user?.id) && accountTasks.isError, error: accountTasks.error, retry: () => accountTasks.refetch() },
    ...taskQueries.map((query) => ({ failed: query.isError, error: query.error, retry: () => query.refetch() })),
  ].filter((item) => item.failed && isTransientNetworkError(item.error));
  const showConsolidatedNetworkError = networkReadFailures.length > 1;
  const retryNetworkReads = () => { void Promise.all(networkReadFailures.map((item) => item.retry())); };
  const selected = conversationsData.find((item) => item.id === conversationID) ?? conversationDetail.data;
  const readyModels = models.data?.items.filter((item) => item.checked_at && item.capabilities.text) ?? [];
  const modelConfigurationKnown = models.isSuccess && !models.isStale;
  const enabledMCPConnectors = mcpConnectors.data?.items.filter((item) => item.enabled) ?? [];
  const defaultModelID = availableModels.data?.default_model_id;
  const defaultModelReady = Boolean(availableModels.data?.default_model_configured && defaultModelID &&
    availableModels.data.items.some((item) => item.id === defaultModelID) &&
    readyModels.some((item) => item.id === defaultModelID));
  const chosenModelID = modelID || preferences.data?.selected_model_id ||
    (defaultModelReady ? defaultModelID : readyModels.length === 1 ? readyModels[0]?.id : '') || '';
  const effectiveModelID = chosenModelID;
  const cloudTasksAvailable = capabilities.isSuccess
    ? capabilities.data.supported_features.includes('cloud_tasks')
    : undefined;
  const conversationAutoRoutingAvailable = capabilities.isSuccess
    ? capabilities.data.supported_features.includes('conversation_auto_routing_v1')
    : undefined;
  const preferredHostID = hostSelection ?? '';
  const hostItems = hosts.data?.items ?? [];
  const selectedHost = hostItems.find((host) => host.id === preferredHostID);
  const hostIsReady = selectedHost?.state === 'running' && selectedHost.control_state === 'ai';
  const workspaceBindings = selectedHost?.workspaces ?? [];
  const selectedBinding = workspaceBindings.find((binding) => binding.workspace.id === workspaceSelection);
  async function openTaskObservation(task: Task, host: Host) {
    if (taskObservationRequest.current) return;
    const popup = window.open('about:blank', '_blank');
    if (!popup) {
      setTaskObservationError({ taskID: task.id, message: '浏览器阻止了新窗口；允许弹出窗口后重试。' });
      return;
    }
    popup.opener = null;
    taskObservationRequest.current = true;
    setOpeningTaskID(task.id);
    setTaskObservationError(null);
    try {
      const grant = await portalApi.taskWorkbenchObservationTicket(task.id);
      if (grant.task_id !== task.id || grant.host_id !== task.host_id || grant.host_id !== host.id) {
        throw new Error('Task observation ticket 与当前任务或电脑不匹配。');
      }
      popup.location.replace(resolveTaskObservationURL(grant.ticket, host.url, host.id));
    } catch (error) {
      popup.close();
      setTaskObservationError({ taskID: task.id, message: errorDetails(error).message });
    } finally {
      taskObservationRequest.current = false;
      setOpeningTaskID('');
    }
  }
  const displayMessages = mergeTaskAssistantMessages(messages.data?.items ?? [], tasks);
  const latestDisplayMessageContent = displayMessages.at(-1)?.content;
  const submissionModelID = readyModels.some((item) => item.id === effectiveModelID) ? effectiveModelID : '';
  const taskActionKey = (scope: string, fingerprint: string) => {
    const previous = pendingTaskActions.current.get(scope);
    if (previous?.fingerprint === fingerprint) return previous.key;
    const key = newIdempotencyKey();
    pendingTaskActions.current.set(scope, { fingerprint, key });
    return key;
  };
  const clearTaskActionKey = (scope: string, key: string) => {
    if (pendingTaskActions.current.get(scope)?.key === key) pendingTaskActions.current.delete(scope);
  };
  const handleTaskActionError = async (error: unknown, taskID: string) => {
    const details = errorDetails(error);
    if (details.code === 'conflict') {
      await client.invalidateQueries({ queryKey: ['task', taskID] });
      notify('任务状态已变化，已刷新最新状态。请核对后再操作。', 'error');
      return;
    }
    notify(details.message, 'error');
  };

  const sendMessage = async (entry: MessageOutboxEntry, reconcile: boolean) => {
    let { scope, message } = entry;
    const id = message.request.client_message_id;
    if (sendingMessageIDs.current.has(id)) return;
    sendingMessageIDs.current.add(id);
    const finishReceipt = receiptOrder.current.begin(scope, id);
    let accepted = message.status === 'accepted' && Boolean(message.messageID);
    if (accepted) finishReceipt(true);
    const checkScope = () => {
      if (!activeOutboxScope.current || scope !== activeOutboxScope.current || scope === outbox.unknownScope || !message.actorID
        || message.actorID !== user?.id || !message.serviceInstanceID || message.serviceInstanceID !== capabilities.data?.service_instance_id) {
        throw new Error('账号或服务尚未确认，消息保留在本机。');
      }
    };
    try {
      if (scope === outbox.unknownScope) {
        if (!capabilities.data?.service_instance_id || !reconcile) {
          await outbox.change(scope, id, (current) => ({ ...current, status: 'pending', error: '等待确认原服务和账号。' }));
          return;
        }
        message = { ...message, serviceInstanceID: capabilities.data.service_instance_id };
        const nextScope = await outbox.add(message);
        await outbox.remove(scope, id);
        scope = nextScope;
      }
      checkScope();
      const currentCapabilities = await portalApi.capabilities();
      if (currentCapabilities.service_instance_id !== message.serviceInstanceID) {
        throw new Error('服务身份已变化，消息保留在原服务的本机队列中。');
      }
      let targetID = message.conversationID;
      if (!targetID) {
        if (!message.conversationCreateKey) throw new Error('缺少会话编号，请重新核对。');
        const created = await portalApi.createConversation({}, message.conversationCreateKey, message.serviceInstanceID!, message.actorID);
        targetID = created.id;
        await outbox.bindConversation(scope, message.actorID, message.conversationCreateKey, targetID);
        checkScope();
        message = { ...message, conversationID: targetID };
        if (!selectedConversation.current && activeOutboxScope.current === scope) setConversationID(targetID);
      }

      if (reconcile || message.status === 'accepted' || message.status === 'unknown') {
        const page = await portalApi.conversationMessages(targetID, undefined, id, message.serviceInstanceID!, message.actorID);
        const receipt = page.items.find((item) => item.id === message.messageID || item.client_message_id === id);
        if (receipt) {
          if (receipt.conversation_id !== targetID || receipt.role !== 'user' || receipt.client_message_id !== id ||
            receipt.content !== message.request.content) throw new Error('恢复的消息回执与当前账号或原文不匹配，消息已保留。');
          accepted = true;
          message = { ...message, messageID: receipt.id, status: 'accepted' };
          finishReceipt(true);
          await outbox.change(scope, id, (current) => ({ ...current, messageID: receipt.id, status: 'accepted', error: undefined }));
          client.setQueryData<{ items: ConversationMessage[]; next_cursor: string | null }>(
            ['conversation-messages', targetID], (previous) => previous?.items.some((item) => item.id === receipt.id)
              ? previous : { items: [...(previous?.items ?? []), receipt], next_cursor: previous?.next_cursor ?? null },
          );
        }
      }

      if (!accepted) {
        const request = outboxReceiptRequest(message);
        if (request.previous_client_message_id) {
          const previousID = request.previous_client_message_id;
          if (previousID === id) throw new Error('消息前序编号无效，原消息已保留。');
          const pending = receiptOrder.current.previous(scope, previousID);
          const previousAccepted = pending ? await pending : (await portalApi.conversationMessages(
            targetID, undefined, previousID, message.serviceInstanceID!, message.actorID,
          )).items.some((item) => item.conversation_id === targetID && item.role === 'user' && item.client_message_id === previousID);
          checkScope();
          if (!previousAccepted) {
            await outbox.change(scope, id, (current) => ({ ...current, status: 'pending', error: '前一条消息尚未确认，请先核对后重试。' }));
            return;
          }
        }
        await outbox.change(scope, id, (current) => ({ ...current, status: 'sending', error: undefined }));
        checkScope();
        const receipt = needsMessageUpload(request.content)
          ? await uploadMessage<ConversationMessage>({ conversationID: targetID }, request, message.serviceInstanceID!, message.actorID, checkScope)
          : (await portalApi.acceptConversationMessage(targetID, request, id, message.serviceInstanceID!, message.actorID)).message;
        if (receipt.conversation_id !== targetID || receipt.role !== 'user' || receipt.client_message_id !== id ||
          receipt.content !== request.content) throw new Error('服务端消息回执与当前账号或原文不匹配，请核对后重试。');
        accepted = true;
        message = { ...message, messageID: receipt.id, status: 'accepted' };
        finishReceipt(true);
        await outbox.change(scope, id, (current) => ({ ...current, messageID: receipt.id, status: 'accepted', error: undefined }));
        client.setQueryData<{ items: ConversationMessage[]; next_cursor: string | null }>(
          ['conversation-messages', targetID], (previous) => previous?.items.some((item) => item.id === receipt.id)
            ? previous : { items: [...(previous?.items ?? []), receipt], next_cursor: previous?.next_cursor ?? null },
        );
      }
      if (recentConversationKey && activeOutboxScope.current === scope) {
        writeRecentConversation(recentConversationKey, targetID);
        client.setQueryData(['assistant-recent-conversation', recentConversationKey], targetID);
      }
      for (const attachment of message.attachments) {
        checkScope();
        const attachmentID = await uploadAssistantAttachment(attachment, message.serviceInstanceID!, message.actorID, checkScope);
        message = { ...message, attachments: message.attachments.map((item) => item.id === attachment.id ? { ...item, attachmentID } : item) };
        await outbox.change(scope, id, (current) => ({ ...current,
          attachments: current.attachments.map((item) => item.id === attachment.id ? { ...item, attachmentID } : item) }));
        checkScope();
        await portalApi.associateConversationAttachment(targetID, message.messageID!, attachment.id, attachmentID, message.serviceInstanceID!, message.actorID);
      }
      await outbox.remove(scope, id);
      void client.invalidateQueries({ queryKey: ['conversations'] });
      void client.invalidateQueries({ queryKey: ['conversation-messages', targetID] });
      void client.invalidateQueries({ queryKey: ['conversation-tasks', targetID] });
    } catch (error) {
      const uncertain = !(error instanceof ApiError) || error.status >= 500 || [408, 425, 429].includes(error.status);
      const details = error instanceof ApiError && error.status === 409
        ? '账号、服务或消息状态已变化，消息保留在本机，请核对后重试。' : errorDetails(error).message;
      await outbox.change(scope, id, (current) => ({ ...current,
        status: accepted ? 'accepted' : uncertain ? 'unknown' : 'failed', error: details }))
        .catch(() => notify('无法更新本地消息状态，消息将保留等待核对。', 'error'));
    } finally {
      finishReceipt(false);
      sendingMessageIDs.current.delete(id);
    }
  };
  const retryProcessing = useMutation({
    mutationFn: async (target: { conversationID: string; messageID: string }) => {
      if (!user?.id || !capabilities.data?.service_instance_id) throw new Error('账号或服务尚未确认。');
      const instanceID = capabilities.data.service_instance_id;
      const current = await portalApi.capabilities();
      if (current.service_instance_id !== instanceID || !current.supported_features.includes('conversation_auto_routing_v1')) {
        throw new Error('服务身份已变化或尚不支持消息处理，请核对后重试。');
      }
      const key = processingRetryKey(instanceID, user.id, '', target.conversationID, target.messageID);
      await portalApi.retryConversationMessageProcessing(target.conversationID, target.messageID, key, instanceID, user.id);
      clearProcessingRetryKey(instanceID, user.id, '', target.conversationID, target.messageID);
    },
    onSuccess: async (_value, target) => {
      notify('已请求重新处理消息。', 'success');
      await client.invalidateQueries({ queryKey: ['conversation-messages', target.conversationID] });
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const respondToApproval = useMutation({
    mutationFn: (request: { taskID: string; approvalID: string; version: number; decision: 'approve' | 'reject'; actionScope: string; key: string }) =>
      portalApi.respondToTaskApproval(request.taskID, request.approvalID,
        { version: request.version, decision: request.decision }, request.key),
    onSuccess: async (_response, request) => {
      clearTaskActionKey(request.actionScope, request.key);
      await client.invalidateQueries({ queryKey: ['task', request.taskID] });
      await client.invalidateQueries({ queryKey: ['assistant-account-tasks'] });
      notify(request.decision === 'approve' ? '已批准，任务正在恢复。' : '已拒绝，任务正在处理决定。', 'success');
    },
    onError: (error, request) => handleTaskActionError(error, request.taskID),
  });
  const answerClarification = useMutation({
    mutationFn: (request: { taskID: string; clarificationID: string; version: number; answer: string; actionScope: string; key: string }) =>
      portalApi.answerTaskInput(request.taskID, {
        clarification_id: request.clarificationID, clarification_version: request.version, answer: request.answer,
      }, request.key),
    onSuccess: async (_response, request) => {
      clearTaskActionKey(request.actionScope, request.key);
      setClarificationAnswers((current) => ({ ...current, [request.taskID]: '' }));
      await client.invalidateQueries({ queryKey: ['task', request.taskID] });
      await client.invalidateQueries({ queryKey: ['assistant-account-tasks'] });
      notify('补充信息已提交，任务正在恢复。', 'success');
    },
    onError: (error, request) => handleTaskActionError(error, request.taskID),
  });
  const taskControl = useMutation({
    mutationFn: async (request: { taskID: string; action: 'pause' | 'resume' | 'cancel'; expectedStatus: Task['status'];
      expectedCurrentRevision: number | null; expectedDesiredRevision: number | null; actionScope: string; key: string }) => {
      const hasCurrentRevision = request.expectedCurrentRevision !== null;
      const hasDesiredRevision = request.expectedDesiredRevision !== null;
      if (hasCurrentRevision !== hasDesiredRevision ||
        (hasCurrentRevision && (!Number.isSafeInteger(request.expectedCurrentRevision) || request.expectedCurrentRevision! < 0 ||
          !Number.isSafeInteger(request.expectedDesiredRevision) || request.expectedDesiredRevision! < 0))) {
        throw new Error('任务修订信息不完整或无效，请刷新状态后重试。');
      }
      const latest = await portalApi.task(request.taskID);
      if (latest.status !== request.expectedStatus || (latest.current_revision ?? null) !== request.expectedCurrentRevision
        || (latest.desired_revision ?? null) !== request.expectedDesiredRevision) {
        throw new ApiError({ message: '任务状态或修订已变化', code: 'conflict', status: 409 });
      }
      return portalApi.taskAction(request.taskID, {
        action: request.action,
        expected_status: request.expectedStatus,
        ...(hasCurrentRevision && hasDesiredRevision ? {
          expected_current_revision: request.expectedCurrentRevision!,
          expected_desired_revision: request.expectedDesiredRevision!,
        } : {}),
      }, request.key);
    },
    onSuccess: async (response, request) => {
      clearTaskActionKey(request.actionScope, request.key);
      await client.invalidateQueries({ queryKey: ['task', request.taskID] });
      await client.invalidateQueries({ queryKey: ['assistant-account-tasks'] });
      if (request.action === 'cancel' && response.status === 'cancelled' && response.unsettled_operation_ids.length === 0) {
        notify('服务端已确认任务停止。', 'success');
      } else if (request.action === 'cancel' && response.unsettled_operation_ids.length > 0) {
        notify('停止请求已受理，等待任务状态更新。', 'info');
      } else {
        notify(request.action === 'pause' ? '暂停请求已受理，等待安全停止确认。' : request.action === 'resume' ? '继续请求已受理，等待任务状态更新。' : '停止请求已受理，仍需等待服务端确认任务状态。', 'info');
      }
    },
    onError: (error, request) => handleTaskActionError(error, request.taskID),
  });
  const deleteTaskFileFetchContent = useMutation({
    mutationFn: ({ taskID, fetchID }: { taskID: string; fetchID: string }) => portalApi.deleteTaskFileFetchContent(taskID, fetchID),
    onSuccess: async (_response, request) => {
      await client.invalidateQueries({ queryKey: ['task-file-fetches', request.taskID] });
      notify('服务端副本已请求删除，正在核对删除状态。', 'success');
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });

  const cancelComposerAttachmentPreparation = () => {
    composerGeneration.current += 1;
    for (const controller of pdfPreparationControllers.current) controller.abort();
    pdfPreparationControllers.current.clear();
    preparingAttachmentCountRef.current = 0;
    setPreparingAttachmentCount(0);
  };

  const addComposerFiles = async (fileList: FileList | null) => {
    if (!fileList) return;
    const incoming = Array.from(fileList);
    const available = Math.max(0, 16 - composerAttachments.length - preparingAttachmentCountRef.current);
    const selected = incoming.slice(0, available);
    const generation = composerGeneration.current;
    const controller = new AbortController();
    pdfPreparationControllers.current.add(controller);
    preparingAttachmentCountRef.current += selected.length;
    setPreparingAttachmentCount(preparingAttachmentCountRef.current);
    const accepted: ComposerAttachment[] = [];
    try {
      for (const file of selected) {
        if (controller.signal.aborted || generation !== composerGeneration.current) break;
        const mediaType = assistantFileMediaType(file);
        const unsafeName = !file.name || file.name === '.' || file.name === '..' || [...file.name].length > 255 ||
          file.name.includes('/') || file.name.includes('\\') || [...file.name].some((character) => character.charCodeAt(0) < 32 || character.charCodeAt(0) === 127);
        if (unsafeName) {
          notify('附件文件名无效。', 'error');
          continue;
        }
        let preparedFile = file;
        let displayName: string | undefined;
        if (mediaType === 'application/pdf') {
          try {
            const extractedText = await extractAssistantPDFText(file, controller.signal);
            if (generation !== composerGeneration.current) continue;
            const safeDerivedName = `${Array.from(file.name).slice(0, 250).join('')}.txt`;
            preparedFile = new File([extractedText], safeDerivedName, { type: 'text/plain', lastModified: file.lastModified });
            displayName = `${file.name} · 已提取文字`;
          } catch (error) {
            if (generation === composerGeneration.current && !(error instanceof Error && error.name === 'AbortError')) {
              notify(`${file.name}：${error instanceof Error ? error.message : '无法读取 PDF。'}`, 'error');
            }
            continue;
          }
        }
        const fileError = assistantFileError(preparedFile, assistantFileMediaType(preparedFile));
        if (fileError) {
          notify(`${file.name}：${fileError}`, 'error');
          continue;
        }
        const id = newIdempotencyKey();
        accepted.push({ id, file: preparedFile, ...(displayName ? { displayName } : {}), objectID: `chat-${id}`,
          objectKey: newIdempotencyKey(), uploadKey: newIdempotencyKey(), commitKey: newIdempotencyKey() });
      }
    } finally {
      pdfPreparationControllers.current.delete(controller);
      preparingAttachmentCountRef.current = Math.max(0, preparingAttachmentCountRef.current - selected.length);
      setPreparingAttachmentCount(preparingAttachmentCountRef.current);
    }
    if (incoming.length > available) notify('每条消息最多添加 16 个附件。', 'error');
    if (generation === composerGeneration.current && accepted.length) setComposerAttachments((current) => [...current, ...accepted]);
  };

  const previewOfficeAttachment = async (item: ComposerAttachment) => {
    const mediaType = assistantFileMediaType(item.file);
    if (!assistantOfficeAttachmentTypes.has(mediaType)) return;
    setOfficePreviews((current) => ({ ...current, [item.id]: { status: 'loading' } }));
    try {
      const text = await portalApi.previewPersonalOfficeText(item.file, mediaType);
      setOfficePreviews((current) => ({ ...current, [item.id]: { status: 'ready', text } }));
    } catch (error) {
      setOfficePreviews((current) => ({ ...current, [item.id]: { status: 'error', message: errorDetails(error).message } }));
    }
  };

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (savingMessageRef.current || preparingAttachmentCountRef.current > 0) return;
    const text = content;
    if (!text.trim() && !composerAttachments.length) return;
    const effectiveText = text.trim() ? text : '请阅读并处理我附加的文件。';
    if (new TextEncoder().encode(effectiveText).length > maxMessageBytes) {
      notify('单条消息不能超过 8 MiB；完整内容仍保留在输入框中。', 'error');
      return;
    }
    if (models.isSuccess && !models.isStale && models.data.configuration_status === 'not_configured') {
      return;
    }
    const id = newIdempotencyKey();
    if (!conversationID && !newConversationKey.current) newConversationKey.current = newIdempotencyKey();
    const createKey = conversationID ? undefined : newConversationKey.current ?? undefined;
    const attachments: PendingMessageAttachment[] = composerAttachments.map((item) => ({
      id: item.id, name: item.file.name, mediaType: assistantFileMediaType(item.file), blob: item.file,
      objectID: item.objectID, objectKey: item.objectKey, uploadKey: item.uploadKey, commitKey: item.commitKey,
      ...(item.attachmentID ? { attachmentID: item.attachmentID } : {}),
    }));
    savingMessageRef.current = true;
    setSavingMessage(true);
    const previous = pendingMessageSave.current.then(async () => {
      const message: PendingConversationMessage = {
        actorID: user?.id ?? '',
        request: {
          client_message_id: id, content: effectiveText,
          ...(submissionModelID ? { model_id: submissionModelID } : {}),
          ...(hostSelection ? { host_id: hostSelection } : {}),
          ...(workspaceSelection ? { workspace_id: workspaceSelection } : {}),
          ...(mcpConnectorIDs.length ? { mcp_connector_ids: [...mcpConnectorIDs] } : {}),
          ...(attachments.length ? { client_attachment_ids: attachments.map((item) => item.id) } : {}),
        },
        conversationID, ...(createKey ? { conversationCreateKey: createKey } : {}),
        ...(capabilities.data?.service_instance_id ? { serviceInstanceID: capabilities.data.service_instance_id } : {}),
        attachments, status: 'pending', createdAt: Date.now(),
      };
      const serverPredecessor = latestConversationClientMessageID(messages.data?.items);
      const entry = await outbox.enqueue(message, serverPredecessor);
      if (activeOutboxScope.current === entry.scope) {
        setContent((current) => current === text ? '' : current);
        setComposerAttachments((current) => current.filter((item) => !attachments.some((attachment) => attachment.id === item.id)));
        setOfficePreviews((current) => {
          const next = { ...current };
          for (const attachment of attachments) delete next[attachment.id];
          return next;
        });
        followConversation.current = true;
      }
      void sendMessage(entry, false);
    }).finally(() => {
      savingMessageRef.current = false;
      setSavingMessage(false);
    });
    pendingMessageSave.current = previous.catch((error) => { notify(`${errorDetails(error).message} 输入已保留。`, 'error'); });
  };

  const submitTaskSupplement = (event: FormEvent<HTMLFormElement>, task: Task) => {
    event.preventDefault();
    const text = (taskSupplementDrafts[task.id] ?? '').trim();
    if (savingMessageRef.current || !text) return;
    if (new TextEncoder().encode(text).length > maxMessageBytes) {
      notify('单条消息不能超过 8 MiB；完整内容仍保留在输入框中。', 'error');
      return;
    }
    const id = newIdempotencyKey();
    savingMessageRef.current = true;
    setSavingMessage(true);
    const previous = pendingMessageSave.current.then(async () => {
      const targetMessages = task.conversation_id === conversationID && messages.data
        ? messages.data.items : (await conversationHistory(task.conversation_id)).items;
      const pending: PendingConversationMessage = {
        actorID: user?.id ?? '',
        request: { client_message_id: id, content: text, reference_task_id: task.id },
        conversationID: task.conversation_id,
        ...(capabilities.data?.service_instance_id ? { serviceInstanceID: capabilities.data.service_instance_id } : {}),
        attachments: [], status: 'pending', createdAt: Date.now(),
      };
      const entry = await outbox.enqueue(pending, latestConversationClientMessageID(targetMessages));
      if (activeOutboxScope.current === entry.scope) {
        setTaskSupplementDrafts((current) => ({ ...current, [task.id]: '' }));
        setSupplementTaskID(null);
      }
      void sendMessage(entry, false);
    }).finally(() => {
      savingMessageRef.current = false;
      setSavingMessage(false);
    });
    pendingMessageSave.current = previous.catch((error) => { notify(`${errorDetails(error).message} 输入已保留。`, 'error'); });
  };

  const submitTaskControl = (action: 'pause' | 'resume' | 'cancel', task: TaskSummary) => {
    const actionScope = `${action}:${task.id}`;
    const expectedCurrentRevision = task.current_revision ?? null;
    const expectedDesiredRevision = task.desired_revision ?? null;
    const fingerprint = JSON.stringify({ action, expected_status: task.status,
      current_revision: expectedCurrentRevision, desired_revision: expectedDesiredRevision });
    taskControl.mutate({ taskID: task.id, action, expectedStatus: task.status,
      expectedCurrentRevision, expectedDesiredRevision, actionScope,
      key: taskActionKey(actionScope, fingerprint) });
  };

  const startNew = () => {
    cancelComposerAttachmentPreparation();
    newConversationKey.current = null;
    setConversationID(null);
    setMcpConnectorIDs([]);
    setContent('');
    setComposerAttachments([]);
    setOfficePreviews({});
    if (recentConversationKey) {
      writeRecentConversation(recentConversationKey, null);
      client.setQueryData(['assistant-recent-conversation', recentConversationKey], null);
    }
  };

  const selectConversationID = (id: string) => {
    cancelComposerAttachmentPreparation();
    setConversationID(id);
    setContent('');
    setComposerAttachments([]);
    setOfficePreviews({});
    if (recentConversationKey) {
      writeRecentConversation(recentConversationKey, id);
      client.setQueryData(['assistant-recent-conversation', recentConversationKey], id);
    }
  };
  const selectConversation = (item: Conversation) => selectConversationID(item.id);
  const showMoreSideConversations = async () => {
    const targetCount = Math.min(sideConversations.length, visibleSideConversationCount) + 5;
    let result = conversations;
    let loadedSideCount = sideConversations.length;
    while (loadedSideCount < targetCount && result.hasNextPage) {
      result = await conversations.fetchNextPage();
      loadedSideCount = new Set((result.data?.pages.flatMap((page) => page.items) ?? [])
        .filter((item) => item.id !== mainConversationID).map((item) => item.id)).size;
      if (result.isFetchNextPageError) break;
    }
    setVisibleSideConversationCount(targetCount);
  };
  const renderConversationItem = (item: Conversation, isMain = false) => <div className={`conversation-row${isMain ? ' conversation-row-main' : ''}`} key={item.id}>
    <button type="button" className={`conversation-item ${conversationID === item.id ? 'is-selected' : ''}`}
      onClick={() => selectConversation(item)}>
      <strong>{isMain ? '主会话' : item.title || '新对话'}</strong>
      {item.id === recentConversationID && !isMain && <small>本机最近查看</small>}
      <small>{isMain ? '最近更新 · ' : ''}{formatDate(item.updated_at)}</small>
    </button>
  </div>;

  const submitClarification = (event: FormEvent<HTMLFormElement>, task: Task) => {
    event.preventDefault();
    const request = task.clarification_request;
    const answer = (clarificationAnswers[task.id] ?? '').trim();
    if (!request || !answer || answerClarification.isPending) return;
    const actionScope = `clarification:${task.id}:${request.id}`;
    const fingerprint = JSON.stringify({ version: request.version, answer });
    answerClarification.mutate({ taskID: task.id, clarificationID: request.id, version: request.version,
      answer, actionScope, key: taskActionKey(actionScope, fingerprint) });
  };

  const finishedTaskIDs = tasks.filter((task) => terminalTaskStatuses.has(task.status)).map((task) => task.id).join(',');
  useEffect(() => { followConversation.current = true; }, [conversationID]);
  useEffect(() => {
    if (!taskPanelOpen) return;
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return;
      setTaskPanelOpen(false);
      taskPanelToggle.current?.focus();
    };
    window.addEventListener('keydown', closeOnEscape);
    return () => window.removeEventListener('keydown', closeOnEscape);
  }, [taskPanelOpen]);
  useEffect(() => {
    const view = conversationView.current;
    if (view && followConversation.current) {
      view.scrollTop = view.scrollHeight;
      setShowJumpToLatest(false);
    }
  }, [conversationID, displayMessages.length, latestDisplayMessageContent, outbox.entries.length]);

  useEffect(() => {
    if (conversationID && finishedTaskIDs) {
      void client.invalidateQueries({ queryKey: ['conversation-messages', conversationID] });
      void client.invalidateQueries({ queryKey: ['conversation-tasks', conversationID] });
    }
  }, [finishedTaskIDs, client, conversationID]);

  useEffect(() => {
    if (!hasNavigationIdea) return;
    navigate(`${location.pathname}${location.search}${location.hash}`, { replace: true, state: null });
  }, [hasNavigationIdea, location.hash, location.pathname, location.search, navigate]);

  const pendingMessages = outbox.entries.filter(({ message }) => !message.projectID && message.conversationID === conversationID);
  const renderPendingMessage = (entry: MessageOutboxEntry) => {
    const { message, scope } = entry;
    const alreadyVisible = displayMessages.some((item) => item.client_message_id === message.request.client_message_id);
    const status = message.status === 'accepted' ? (message.attachments.length ? '消息已发送，附件等待处理' : '消息已发送')
      : message.status === 'sending' ? '正在发送' : message.status === 'unknown' ? '发送结果待确认'
        : message.status === 'failed' ? '发送失败' : '已保存在本机，等待发送';
    return <article className="chat-message chat-message-user chat-message-pending" aria-label="你的消息" key={`outbox:${message.request.client_message_id}`}>
      {!alreadyVisible && <><strong className="chat-message-author">你</strong><p>{message.request.content}</p></>}
      <small role="status">{status}{message.error ? `：${message.error}` : ''}</small>
      {message.attachments.length > 0 && <small>附件 · {message.attachments.length}</small>}
      {message.status !== 'sending' && <button type="button" className="button button-small button-secondary"
        onClick={() => void sendMessage({ scope, message }, true)}>{message.status === 'accepted' && message.attachments.length ? '重试附件' : '核对并重试'}</button>}
    </article>;
  };
  const renderTaskActivity = (summary: TaskSummary) => {
    const task = allTasksByID.get(summary.id);
    const listItem = task ?? summary;
    const busy = !terminalTaskStatuses.has(listItem.status);
    const taskConversation = conversationsData.find((item) => item.id === listItem.conversation_id);
    const expanded = expandedTaskIDs.includes(listItem.id);
    const taskQuery = taskQueries[taskIDs.indexOf(listItem.id)];
    const taskFileFetchQuery = taskFileFetchQueries[taskIDs.indexOf(listItem.id)];
    if (!task || (!expanded && !currentConversationTaskIDs.has(listItem.id))) return <section className="task-activity task-activity-summary" key={`activity:${listItem.id}`}
      aria-label={`任务 ${listItem.id} 的工作过程`}>
      <header className="task-activity-heading">
        <div><strong>{listItem.objective || `任务 ${listItem.id}`}</strong>
          <small>{taskConversation?.title || `会话 ${listItem.conversation_id}`} · 任务 {listItem.id}</small></div>
          <span>服务端状态：{taskStatusLabel[listItem.status]}{listItem.current_revision !== undefined || listItem.desired_revision !== undefined
          ? ` · 修订 ${listItem.current_revision ?? '—'} / ${listItem.desired_revision ?? '—'}` : ''}</span>
      </header>
      {listItem.host_id && <p className="assistant-task-target">当前任务使用：{taskTargetText(listItem, hostItems)}。输入区的目标只影响下一条消息。</p>}
      <div className="inline-actions task-activity-actions">
        <button type="button" className="button button-small button-secondary" onClick={() => selectConversationID(listItem.conversation_id)}>打开关联会话</button>
        <button type="button" className="button button-small button-secondary" aria-expanded={expanded}
          onClick={() => setExpandedTaskIDs((current) => expanded ? current.filter((id) => id !== listItem.id) : [...current, listItem.id])}>
          {expanded ? '收起任务详情' : '查看任务详情'}
        </button>
        {busy && listItem.status === 'paused' && <button type="button" className="button button-small button-secondary" disabled={taskControl.isPending}
          onClick={() => submitTaskControl('resume', listItem)}>继续任务</button>}
        {busy && ['queued', 'running', 'waiting_user'].includes(listItem.status) && <button type="button" className="button button-small button-secondary" disabled={taskControl.isPending}
          onClick={() => submitTaskControl('pause', listItem)}>暂停任务</button>}
        {busy && ['queued', 'running', 'waiting_user', 'paused'].includes(listItem.status) && <button type="button" className="button button-small button-secondary" disabled={taskControl.isPending}
          onClick={() => submitTaskControl('cancel', listItem)}>{taskControl.isPending ? '正在停止…' : '停止任务'}</button>}
      </div>
      {expanded && taskQuery?.isPending && <p className="muted" role="status">正在读取任务详情…</p>}
    </section>;
    const k2Read = readK2TaskProjection(task);
    const k2 = k2Read.kind === 'available' ? k2Read.projection : null;
    const review = k2?.result_review ?? null;
    const currentStep = task.steps.find((step) => step.id === task.current_step);
    const recoveryMessage = task.waiting_reason === 'dsh_recovery_unresolved' ? recoveryHint(currentStep) : null;
    const visibleSteps = task.steps.filter((step) => ['tool', 'subagent', 'verification', 'verify', 'batch_item', 'batch_result', 'progress'].includes(step.kind) &&
      !(task.waiting_reason === 'dsh_recovery_unresolved' && step.status === 'unknown'));
    const ownConversationReplyVisible = task.conversation_id === conversationID && displayMessages.some((message) => message.role === 'assistant' && (message.task_id === task.id ||
      message.sources?.some((source) => source.task_id === task.id && source.task_revision === task.assistant_message?.task_revision &&
        source.event_sequence === task.assistant_message?.result_event_sequence)));
    const publishedReplyVisible = Boolean(review?.state === 'published' && review.source_task_id === task.id && review.reply_message_id &&
      (task.conversation_id !== conversationID || displayMessages.some((message) => message.id === review.reply_message_id && message.role === 'assistant' &&
        message.sources?.some((source) => source.task_id === review.source_task_id &&
          source.task_revision === review.source_task_revision && source.event_sequence === review.source_event_sequence))));
    const hasReply = ownConversationReplyVisible || (task.conversation_id !== conversationID && publishedReplyVisible);
    const taskHost = task.host_id ? hostItems.find((host) => host.id === task.host_id) : undefined;
    const browserHandoffRequested = task.waiting_reason === 'clarification' && Boolean(task.clarification_request) && task.steps.some((step) =>
      step.kind === 'tool' && (step.title === 'Run cloud dsh tool: browser_prepare_user_handoff' ||
        step.title === 'Run cloud dsh MCP tool: browser_prepare_user_handoff'));
    const answer = clarificationAnswers[task.id] ?? '';
    const supplementEntries = outbox.entries.filter(({ message }) => message.conversationID === task.conversation_id &&
      message.request.reference_task_id === task.id);
    return <section className="task-activity" key={`activity:${task.id}`} aria-label={`任务 ${task.id} 的工作过程`}>
        <header className="task-activity-heading">
          <div><strong>{task.objective || `任务 ${task.id}`}</strong>
            <small>{taskConversation?.title || `会话 ${task.conversation_id}`} · 任务 {task.id}</small></div>
          <span>服务端状态：{taskStatusLabel[task.status]}{task.current_revision !== undefined || task.desired_revision !== undefined
            ? ` · 修订 ${task.current_revision ?? '—'} / ${task.desired_revision ?? '—'}` : ''}</span>
        </header>
        {(busy || task.status === 'failed' || task.status === 'cancelled' || task.result_message_pending ||
          (task.result_message_published && !hasReply)) && <div className="assistant-progress" role={task.status === 'failed' ? 'alert' : 'status'}>
          <span>{taskStatusText(task, cloudTasksAvailable)}</span>
          {recoveryMessage && <span>{recoveryMessage}</span>}
          {busy && currentStep?.title && task.waiting_reason !== 'dsh_recovery_unresolved' && currentStep.kind !== 'model' && <span>{stepTitle(currentStep)}</span>}
          {busy && taskEventFeeds[task.id] === 'disconnected' && !showConsolidatedNetworkError && <span>连接中断，正在重连…</span>}
        </div>}
        <div className="inline-actions task-activity-actions">
          <button type="button" className="button button-small button-secondary" onClick={() => selectConversationID(task.conversation_id)}>打开关联会话</button>
          {!currentConversationTaskIDs.has(task.id) && <button type="button" className="button button-small button-secondary"
            onClick={() => setExpandedTaskIDs((current) => current.filter((id) => id !== task.id))}>收起任务详情</button>}
        </div>
        {task.host_id && <p className="assistant-task-target">当前任务使用：{taskTargetText(task, hostItems)}。下面的目标选择只影响下一条新消息。</p>}
        {(taskFileFetchQuery?.isPending || taskFileFetchQuery?.isError || (taskFileFetchQuery?.data?.items.length ?? 0) > 0) &&
          <section className="task-activity-k2 task-activity-file-fetches" aria-label={`任务 ${task.id} 的文件取回`}>
            <h4>PC 文件取回</h4>
            {taskFileFetchQuery?.isPending && <p className="muted" role="status">正在读取文件取回状态…</p>}
            {taskFileFetchQuery?.isError && <p className="error-inline" role="alert">无法读取文件取回状态：{errorDetails(taskFileFetchQuery.error).message}
              <button type="button" className="button button-small button-secondary" onClick={() => void taskFileFetchQuery.refetch()}>重试</button>
            </p>}
            {taskFileFetchQuery?.data?.items.length ? <ul>{taskFileFetchQuery.data.items.map((item) => {
              const hasSize = item.size_bytes !== null;
              const progressValue = hasSize ? Math.min(item.confirmed_bytes, item.size_bytes!) : 0;
              return <li key={item.fetch_id}>
                <span><strong>{item.relative_path}</strong><small>{taskFileFetchStatusLabel[item.status]}</small></span>
                <small>来源设备：{item.source_device_name} · 已确认 {formatBytes(item.confirmed_bytes)}{hasSize ? ` / ${formatBytes(item.size_bytes!)}` : ' / 总大小待确认'}</small>
                <small>来源版本：{item.source_version ?? '待确认'}</small>
                {hasSize && item.size_bytes! > 0 && <progress max={item.size_bytes!} value={progressValue}
                  aria-label={`文件取回进度：${item.relative_path}`} />}
                <small>服务端副本：{taskFileFetchContentStateLabel[item.content_state]}
                  {item.expires_at ? ` · 保留到 ${formatDate(item.expires_at)}` : ''}
                  {item.deleted_at ? ` · 删除于 ${formatDate(item.deleted_at)}` : ''}</small>
                {item.status === 'completed' && item.content_state === 'retained' && <button type="button" className="button button-small button-secondary"
                  disabled={deleteTaskFileFetchContent.isPending}
                  onClick={() => {
                    if (window.confirm(`确认删除“${item.relative_path}”的服务端副本？删除后无法恢复。`)) {
                      deleteTaskFileFetchContent.mutate({ taskID: task.id, fetchID: item.fetch_id });
                    }
                  }}>删除服务端副本</button>}
              </li>;
            })}</ul> : null}
          </section>}
        {browserHandoffRequested && task.host_id && <div className="inline-actions assistant-browser-handoff">
          <Link className="button button-secondary" to={`/hosts#host-${encodeURIComponent(task.host_id)}`}>打开目标电脑进行登录</Link>
          <span className="muted">登录完成后交还 AI，再回到这里提交并继续。</span>
        </div>}
        {busy && <div className="inline-actions task-activity-actions">
          {task.status === 'paused' && <button type="button" className="button button-secondary" disabled={taskControl.isPending}
            onClick={() => submitTaskControl('resume', task)}>继续任务</button>}
          {['queued', 'running', 'waiting_user'].includes(task.status) && <button type="button" className="button button-secondary" disabled={taskControl.isPending}
            onClick={() => submitTaskControl('pause', task)}>暂停任务</button>}
          {['queued', 'running', 'waiting_user', 'paused'].includes(task.status) && <button type="button" className="button button-secondary" disabled={taskControl.isPending}
            onClick={() => submitTaskControl('cancel', task)}>{taskControl.isPending ? '正在停止…' : '停止任务'}</button>}
          <button type="button" className="button button-secondary"
            onClick={() => setSupplementTaskID((current) => current === task.id ? null : task.id)}>
            {supplementTaskID === task.id ? '收起补充任务' : '补充任务'}
          </button>
        </div>}
        {busy && supplementTaskID === task.id && task.waiting_reason !== 'project_recovery_unavailable' && <form className="task-supplement-form"
          aria-label={`补充任务 ${task.id}`} onSubmit={(event) => submitTaskSupplement(event, task)}>
          <label htmlFor={`task-supplement-${task.id}`}>补充任务内容</label>
          <textarea id={`task-supplement-${task.id}`} value={taskSupplementDrafts[task.id] ?? ''} maxLength={maxMessageBytes}
            onChange={(event) => setTaskSupplementDrafts((current) => ({ ...current, [task.id]: event.target.value }))}
            disabled={savingMessage} />
          <button type="submit" className="button button-primary" disabled={!taskSupplementDrafts[task.id]?.trim() || savingMessage}>发送补充任务</button>
        </form>}
        {supplementEntries.map(({ message, scope }) => <div className="task-supplement-status" key={message.request.client_message_id}>
          <small role="status">补充消息：{message.status === 'accepted' ? '已发送' : message.status === 'sending' ? '正在发送'
            : message.status === 'unknown' ? '发送结果待确认' : message.status === 'failed' ? '发送失败' : '等待发送'}{message.error ? `：${message.error}` : ''}</small>
          {message.status !== 'sending' && <button type="button" className="button button-small button-secondary"
            onClick={() => void sendMessage({ scope, message }, true)}>{message.status === 'accepted' ? '核对消息' : '核对并重试'}</button>}
        </div>)}
        {visibleSteps.length > 0 && <details><summary>查看工作过程</summary><ol className="task-activity-steps">
          {visibleSteps.map((step) => <li key={step.id}><span aria-hidden="true">•</span><span><strong>{stepTitle(step)}</strong>
            {step.kind !== 'progress' && <small>{stepStatus[step.status]}</small>}
            {step.summary.trim() && <small className="task-activity-summary">{step.summary}</small>}
          </span></li>)}
        </ol></details>}
        {k2Read.kind === 'invalid' && <div className="notice notice-warning" role="alert">
          并发任务投影格式无效；当前只显示服务端 Task 状态，不会把未核验的结果当作最终回复。
        </div>}
        {k2 && k2.dependencies.length > 0 && <section className="task-activity-k2" aria-label={`任务 ${task.id} 的依赖`}>
          <h4>依赖任务</h4>
          <ul>{k2.dependencies.map((dependency) => <li key={`${dependency.task_id}:${dependency.required_revision}`}>
            <span>{dependencyStatusText(dependency)}</span>
          </li>)}</ul>
        </section>}
        {k2 && k2.child_jobs.length > 0 && <section className="task-activity-k2" aria-label={`任务 ${task.id} 的子任务`}>
          <h4>子任务进度</h4>
          <ul>{k2.child_jobs.map((job) => <li key={job.job_id}>
            <span><strong>{job.title || job.job_id}</strong><small>{stepStatus[job.status]}</small></span>
            <small>子会话 {job.job_id} · 步骤 {job.task_step_id} · 修订 {job.task_revision}</small>
          </li>)}</ul>
        </section>}
        {review && <section className="task-activity-k2 task-activity-review" aria-label={`任务 ${task.id} 的结果整理`}>
          <h4>主助手结果整理</h4>
          <p role={review.state === 'failed' ? 'alert' : 'status'}>{resultReviewStatusText(review, publishedReplyVisible)}</p>
          <small>来源任务 {review.source_task_id} · 修订 {review.source_task_revision} · 结果事件 {review.source_event_sequence}</small>
          {review.reply_message_id && <small>最终回复消息 {review.reply_message_id}</small>}
        </section>}
        {task.result && <section className="task-activity-k2" aria-label={`任务 ${task.id} 的原始结果`}>
          <h4>Task 原始结果</h4>
          <details><summary>查看 Task 原始结果（不是最终回复）</summary><ChatMarkdown content={task.result} /></details>
        </section>}
        {task.artifacts.map((artifact) => <p className="task-activity-artifact" key={artifact.id}>成果：{artifact.label}</p>)}
        {task.execution_location === 'server' && task.host_id && <div className="inline-actions">
          {taskHost?.state === 'running'
            ? <button type="button" className="button button-secondary" disabled={openingTaskID === task.id}
              onClick={() => void openTaskObservation(task, taskHost)}>{openingTaskID === task.id ? '正在准备只读观察…' : '在 Workbench 只读观察'}</button>
            : <p className="muted" role="status">目标电脑当前不可用或未运行，暂不能打开 Workbench 观察页。</p>}
          {taskObservationError?.taskID === task.id && <p className="error-text" role="alert">{taskObservationError.message}</p>}
        </div>}
        {task.pending_request && <section className="notice notice-warning" aria-label="需要批准的操作">
          <h3>{task.pending_request.title}</h3><p>{task.pending_request.object}</p><p>{task.pending_request.content}</p><p>可能影响：{task.pending_request.impact}</p>
          <div className="inline-actions">{(['approve', 'reject'] as const).map((decision) => <button key={decision} type="button"
            className={`button ${decision === 'approve' ? 'button-primary' : 'button-secondary'}`} disabled={respondToApproval.isPending}
            onClick={() => {
              const request = task.pending_request!;
              const actionScope = `approval:${task.id}:${request.id}`;
              const fingerprint = JSON.stringify({ version: request.version, decision });
              respondToApproval.mutate({ taskID: task.id, approvalID: request.id, version: request.version,
                decision, actionScope, key: taskActionKey(actionScope, fingerprint) });
            }}>{decision === 'approve' ? '批准并继续' : '拒绝'}</button>)}</div>
        </section>}
        {task.clarification_request && <form className="notice notice-warning" aria-label="补充任务信息" onSubmit={(event) => submitClarification(event, task)}>
          <h3>任务需要补充信息</h3><p>{task.clarification_request.question}</p>
          <label htmlFor={`task-clarification-${task.id}`}>你的回答</label>
          <textarea id={`task-clarification-${task.id}`} value={answer} maxLength={16384} required
            onChange={(event) => setClarificationAnswers((current) => ({ ...current, [task.id]: event.target.value }))} disabled={answerClarification.isPending} />
          <button type="submit" className="button button-primary" disabled={!answer.trim() || answerClarification.isPending}>
            {answerClarification.isPending ? '正在提交…' : '提交并继续'}
          </button>
        </form>}
      </section>;
  };

  return <section className="assistant-page">
    {showConsolidatedNetworkError && <div className="error-card assistant-network-error" role="alert">网络异常，部分对话数据暂时无法读取。<button type="button" className="button button-secondary" onClick={retryNetworkReads}>重试</button></div>}
    {import.meta.env.DEV && import.meta.env.VITE_MOCK_API === '1' && <p className="notice notice-warning" role="status">当前为演示模式，尚未连接真实 AI 服务。这里的消息不会由 AI 执行。</p>}
    {mainConversation.isPending && <p className="notice notice-info" role="status">正在读取服务端主会话…</p>}
    {mainConversation.isError && !showConsolidatedNetworkError && <div className="notice notice-warning" role="status">当前服务尚未提供主会话接口。你仍可查看和使用会话列表，但本机最近记录不会被当作主会话。<button type="button" className="button button-small button-secondary" onClick={() => void mainConversation.refetch()}>重试</button></div>}
    {mainConversation.data?.status === 'uninitialized' && <p className="notice notice-info" role="status">正在初始化你的主会话…{ensureMainConversation.isError && <button type="button" className="button button-small button-secondary" onClick={ensureMain}>重试</button>}</p>}
    {capabilities.isPending && <p className="notice notice-info" role="status">正在检查服务能力…</p>}
    {capabilities.isError && !showConsolidatedNetworkError && <div className="error-inline" role="alert">无法读取服务能力：{errorDetails(capabilities.error).message}<button type="button" className="button button-small button-secondary" aria-label="重试服务能力" onClick={() => void capabilities.refetch()}>重试</button></div>}
    {capabilities.isSuccess && conversationAutoRoutingAvailable === false && <div className="notice notice-warning" role="status">
      服务端尚未确认支持对话自动处理；仍可发送消息，发送状态会根据服务端回执显示，未确认的消息会保留在本机。
    </div>}
    {capabilities.isSuccess && cloudTasksAvailable === false && <div className="notice notice-warning" role="status">
      云端工具当前不可用；普通消息仍可发送，实际调用该工具时会检查可用状态。
    </div>}
    <div className={`assistant-layout${taskPanelOpen ? ' assistant-layout-with-task-panel' : ''}`}>
      <aside className="assistant-sidebar" aria-label="最近对话">
        <div className="assistant-sidebar-heading"><button type="button" className="icon-button" aria-label="刷新对话" title="刷新会话列表" onClick={() => { void conversations.refetch(); void mainConversation.refetch(); }}><MordeIcon name="refresh" /></button></div>
        {conversations.isPending ? <p className="muted" role="status">正在加载对话…</p> : conversations.isError && !conversations.data
          ? showConsolidatedNetworkError ? <p className="muted">对话列表暂不可用</p>
            : <div className="error-inline" role="alert">{errorDetails(conversations.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void conversations.refetch()}>重试</button></div>
          : <>
            {mainConversationID && (mainConversationItem ? renderConversationItem(mainConversationItem, true)
              : mainConversationDetail.isError ? <div className="error-inline" role="alert">无法读取主会话：{errorDetails(mainConversationDetail.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void mainConversationDetail.refetch()}>重试</button></div>
                : <p className="muted" role="status">正在读取主会话…</p>)}
            <div className="conversation-group">
              <details className="conversation-group-details" open={sideConversationsOpen} onToggle={(event) => setSideConversationsOpen(event.currentTarget.open)}>
                <summary>旁路会话</summary>
                <div className="conversation-list">{sideConversations.slice(0, visibleSideConversationCount).map((item) => renderConversationItem(item))}</div>
                {conversations.isFetchNextPageError && <div className="error-inline" role="alert">{errorDetails(conversations.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void conversations.fetchNextPage()}>重试</button></div>}
                {(sideConversations.length > visibleSideConversationCount || conversations.hasNextPage) && <button type="button" className="button button-secondary button-small conversation-more"
                  disabled={conversations.isFetchingNextPage} onClick={() => void showMoreSideConversations()}>
                  {conversations.isFetchingNextPage ? '正在加载…' : '更多'}
                </button>}
                {!sideConversations.length && !conversations.hasNextPage && <p className="muted assistant-sidebar-empty">还没有旁路会话。</p>}
              </details>
              <button type="button" className="button button-secondary conversation-new" aria-label="新建旁路会话" title="新建旁路会话" onClick={startNew}>+</button>
            </div>
          </>}
        <p className="assistant-sidebar-hint">主会话保留日常交流，旁路会话可用于单独讨论。</p>
      </aside>
      <div className="assistant-main">
        <header className="conversation-view-heading">
          <div className="assistant-chat-title"><span className="assistant-logo assistant-logo-header" aria-hidden="true" /><div>
            <h2>{conversationID && conversationID === mainConversationID ? '主会话' : selected?.title || (conversationID ? '对话' : '新旁路会话')}</h2>
            <p>{conversationID && conversationID === mainConversationID ? '你的 moder' : '旁路会话 · 你的 moder'}</p>
          </div></div>
          <button ref={taskPanelToggle} type="button" className="button button-secondary task-panel-toggle"
            aria-label="任务动态" aria-describedby={pendingTaskCount === null ? undefined : 'assistant-task-pending-count'}
            aria-expanded={taskPanelOpen} aria-controls="assistant-task-panel"
            title={pendingTaskCount === null ? '任务动态，待处理数量暂不可用' : `任务动态，${pendingTaskCount} 项等待用户`}
            onClick={() => setTaskPanelOpen((open) => !open)}>
            <MordeIcon name="panel" />任务动态{pendingTaskCount !== null && <span id="assistant-task-pending-count" className="sr-only">{pendingTaskCount} 项等待用户</span>}
            {pendingTaskCount !== null && pendingTaskCount > 0 && <span className="task-pending-count" aria-hidden="true">{pendingTaskCount}</span>}
          </button>
        </header>
        {activePanelTasks.slice(0, maxTaskEventStreams).map((task) => <TaskStream key={task.id} task={task} conversationID={task.conversation_id} onState={setTaskEventFeed} />)}
        <div className={`conversation-view${conversationID ? '' : ' conversation-view-welcome'}`} aria-live="polite" ref={conversationView} onScroll={(event) => {
          const view = event.currentTarget;
          const atLatest = view.scrollHeight - view.scrollTop - view.clientHeight < 80;
          followConversation.current = atLatest;
          setShowJumpToLatest(!atLatest);
        }}>
          {!conversationID ? <><div className="assistant-welcome"><span className="assistant-mark assistant-logo" aria-hidden="true" />
            <h2>今天想聊些什么？</h2><p>告诉我你想完成的事情。</p>
          </div>{pendingMessages.map(renderPendingMessage)}</> : <>
          {messages.isPending && displayMessages.length === 0 ? <div className="loading-card" role="status">正在加载消息…</div>
            : messages.isError && displayMessages.length === 0 ? showConsolidatedNetworkError ? <div className="loading-card">消息暂不可用</div>
              : <div className="error-card" role="alert">{errorDetails(messages.error).message}<button type="button" className="button button-secondary" onClick={() => void messages.refetch()}>重试</button></div>
              : <div className="message-list">
                {messages.isError && !showConsolidatedNetworkError && <div className="error-inline" role="alert">{errorDetails(messages.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void messages.refetch()}>重试</button></div>}
                {displayMessages.map((message, index) => {
                  const messageTask = message.task_id ? tasksByID.get(message.task_id) : undefined;
                  const routingStatus = message.role === 'user' ? routingStatusText(message.routing_status) : null;
                  const previous = displayMessages[index - 1];
                  const showDate = !previous || formatDate(previous.created_at).split(' ')[0] !== formatDate(message.created_at).split(' ')[0];
                  return <Fragment key={message.id}>
                    {showDate && <div className="conversation-time-divider"><time dateTime={message.created_at}>{formatDate(message.created_at)}</time></div>}
                    <div className={`chat-message-row chat-message-row-${message.role}`}>
                    {message.role === 'assistant' && <span className="assistant-logo assistant-logo-message" aria-hidden="true" />}
                    <div className="chat-message-body">
                    {message.role === 'assistant' && <strong className="chat-message-author">moder</strong>}
                    <article id={`conversation-message-${encodeURIComponent(message.id)}`} className={`chat-message chat-message-${message.role}`}
                      aria-label={message.role === 'assistant' ? 'moder 的回复' : '你的消息'} tabIndex={message.role === 'assistant' ? 0 : undefined}>
                      {message.role === 'assistant' ? <ChatMarkdown content={message.content} /> : <p>{message.content}</p>}
                      {message.attachment_ids?.length ? <small className="chat-message-attachments">附件 · {message.attachment_ids.length}
                        {message.attachment_ids.map((attachmentID, index) => <span key={attachmentID}> · <a
                          href={`/api/personal-data/attachments/${encodeURIComponent(attachmentID)}`} download>下载附件 {index + 1}</a></span>)}
                      </small> : null}
                      {message.snapshotStatus === 'streaming' && <small role="status">正在生成…</small>}
                      {message.snapshotStatus === 'interrupted' && <small role="status">回复已中断</small>}
                      {message.role === 'user' && (routingStatus || (message.delivery_status !== 'applied' && message.delivery_status)) && <small role="status">
                        {routingStatus ?? (message.delivery_status === 'pending' && messageTask && terminalTaskStatuses.has(messageTask.status)
                          ? '消息未处理，处理已结束' : deliveryStatusText(message.delivery_status))}
                        {!routingStatus && message.delivery_status === 'unknown' && <button type="button" className="button button-small button-secondary"
                          disabled={messages.isFetching} onClick={() => {
                            void client.invalidateQueries({ queryKey: ['conversation-messages', conversationID] });
                            if (message.task_id) void client.invalidateQueries({ queryKey: ['task', message.task_id] });
                          }}>核对状态</button>}
                      </small>}
                      {message.role === 'user' && message.routing_status === 'failed' && !message.task_id && <div>
                        <small>{routingFailureText(message.routing_failure_reason)}</small>
                        {message.routing_failure_reason !== 'message_too_long' && <button type="button" className="button button-small button-secondary" disabled={retryProcessing.isPending}
                          onClick={() => retryProcessing.mutate({ conversationID: message.conversation_id, messageID: message.id })}>重试处理</button>}
                      </div>}
                      {message.role === 'user' && <small>{formatDate(message.created_at)}</small>}
                    </article>
                    {message.role === 'assistant' && <time className="assistant-reply-time" dateTime={message.created_at} aria-label={`回复时间：${formatDate(message.created_at)}`}>{formatDate(message.created_at)}</time>}
                    </div></div>
                  </Fragment>;
                })}
              </div>}
          {pendingMessages.map(renderPendingMessage)}
          </>}
          {models.isError && !showConsolidatedNetworkError && <div className="error-inline" role="alert">无法读取模型：{errorDetails(models.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void models.refetch()}>重试</button></div>}
          {modelConfigurationKnown && models.data?.configuration_status === 'not_configured' && <div className="notice notice-warning assistant-setup-notice" role="status">还没有配置云端模型。<Link to="/connections">配置模型</Link></div>}
          {modelConfigurationKnown && models.data?.configuration_status === 'configured' && !readyModels.length && <div className="notice notice-warning assistant-setup-notice" role="status">模型已配置，当前没有通过检查的文字模型；消息仍可先发送。<Link to="/connections">检查模型设置</Link></div>}
          {outbox.storageError && <div className="error-inline" role="alert">本地消息存储不可用：{outbox.storageError}</div>}
          {execution.isError && !showConsolidatedNetworkError && <div className="error-inline" role="alert">无法读取 AI 服务状态：{errorDetails(execution.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void execution.refetch()}>重试</button></div>}
          {execution.data?.state === 'stopping' && <p className="notice notice-warning" role="status">AI 服务正在停止；消息仍可先发送。</p>}
          {hosts.isError && !showConsolidatedNetworkError && <div className="error-inline" role="alert">无法读取目标电脑：{errorDetails(hosts.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void hosts.refetch()}>重试</button></div>}
          {preferredHostID && !selectedHost && !hosts.isPending && !hosts.isError && <p className="notice notice-warning" role="status">所选电脑状态待核对；实际使用该目标时会检查权限和状态。</p>}
          {selectedHost && !hostIsReady && <p className="notice notice-warning" role="status">目标电脑暂不可用：{hostStateLabel[selectedHost.state] || selectedHost.state} · {controlStateLabels[selectedHost.control_state]}。</p>}
          {workspaceSelection && !selectedBinding && <p className="notice notice-warning" role="status">所选 Workspace 状态待核对；消息仍可发送，使用时会再次核验。</p>}
          {showJumpToLatest && conversationID && <div className="assistant-jump-latest"><button type="button" className="button button-secondary"
            onClick={() => { followConversation.current = true; if (conversationView.current) conversationView.current.scrollTop = conversationView.current.scrollHeight; setShowJumpToLatest(false); }}>
            ↓ 回到最新消息</button></div>}
        </div>
        <section className="assistant-composer" aria-label="发送对话消息">
          <form onSubmit={submit}>
            <label className="sr-only" htmlFor="assistant-content">消息内容</label>
            <textarea id="assistant-content" value={content} onChange={(event) => setContent(event.target.value)}
              placeholder="告诉我你想完成什么…" />
            {composerAttachments.length > 0 && <div className="assistant-attachment-list" aria-label="待发送附件">
              {composerAttachments.map((item) => {
                const mediaType = assistantFileMediaType(item.file);
                const preview = officePreviews[item.id];
                return <Fragment key={item.id}>
                  <span className="assistant-attachment-chip">
                    <span title={item.displayName ?? item.file.name}>{item.displayName ?? item.file.name}{item.attachmentID ? ' · 已准备' : ''}</span>
                    {assistantOfficeAttachmentTypes.has(mediaType) && <button type="button" aria-label={`预览附件 ${item.file.name}`}
                      disabled={preview?.status === 'loading'} style={{ width: 'auto', padding: '0 5px', whiteSpace: 'nowrap' }}
                      onClick={() => void previewOfficeAttachment(item)}>{preview?.status === 'loading' ? '预览中…' : '预览文本'}</button>}
                    <button type="button" aria-label={`移除附件 ${item.file.name}`} onClick={() => {
                      setComposerAttachments((current) => current.filter((candidate) => candidate.id !== item.id));
                      setOfficePreviews((current) => {
                        const next = { ...current };
                        delete next[item.id];
                        return next;
                      });
                    }}>×</button>
                  </span>
                  {preview?.status === 'loading' && <span role="status" style={{ flexBasis: '100%' }}>正在提取 {item.file.name} 的文字…</span>}
                  {preview?.status === 'error' && <span role="alert" style={{ flexBasis: '100%' }}>{item.file.name} 预览失败：{preview.message}</span>}
                  {preview?.status === 'ready' && <pre role="region" aria-label={`文本预览 ${item.file.name}`}
                    style={{ flexBasis: '100%', maxHeight: '12rem', overflow: 'auto', whiteSpace: 'pre-wrap', overflowWrap: 'anywhere', margin: '4px 0', padding: '8px', border: '1px solid #dce5f4', borderRadius: '6px' }}>
                    {preview.text}</pre>}
                </Fragment>;
              })}
            </div>}
            <div className="assistant-composer-footer">
              <div className="assistant-composer-actions">
                <label className="assistant-attach-control" title="添加附件">
                  <MordeIcon name="paperclip" /><span className="sr-only">附件</span>
                  <input type="file" aria-label="添加附件" accept=".txt,.jpg,.jpeg,.png,.docx,.xlsx,.pptx,.pdf,text/plain,image/jpeg,image/png,application/pdf,application/vnd.openxmlformats-officedocument.wordprocessingml.document,application/vnd.openxmlformats-officedocument.spreadsheetml.sheet,application/vnd.openxmlformats-officedocument.presentationml.presentation"
                    multiple onChange={(event) => { void addComposerFiles(event.currentTarget.files); event.currentTarget.value = ''; }} />
                </label>
                <AssistantVoiceInput compact onTranscript={(transcript) => setContent((current) => current.trim()
                  ? `${current.trimEnd()} ${transcript}` : transcript)} />
              </div>
              <div className="assistant-selectors">
                <label className="assistant-selector"><MordeIcon name="spark" /><span className="sr-only">AI 模型</span><select aria-label="选择 AI 模型"
                  value={effectiveModelID} onChange={(event) => setModelID(event.target.value)} disabled={models.isPending}>
                  <option value="">自动选择模型</option>{effectiveModelID && !readyModels.some((item) => item.id === effectiveModelID) && <option value={effectiveModelID}>已选择模型，状态待核对</option>}{readyModels.map((item) => <option key={item.id} value={item.id}>{item.name} · {item.model_name}</option>)}
                </select></label>
                <div className="assistant-target-row">
                  <label className="assistant-selector"><MordeIcon name="monitor" /><span className="sr-only">目标电脑</span><select aria-label="选择目标电脑" value={selectedHost?.id ?? hostSelection ?? ''}
                    disabled={hosts.isPending} onChange={(event) => { setHostSelection(event.target.value); setWorkspaceSelection(''); }}>
                    <option value="">无电脑目标</option>
                    {hostSelection && !selectedHost && <option value={hostSelection}>所选电脑状态待核验</option>}
                    {hostItems.map((host) => <option key={host.id} value={host.id}>{host.name || host.id} · {hostStateLabel[host.state] || host.state} · {controlStateLabels[host.control_state]}</option>)}
                  </select></label>
                  {selectedHost && <label className="assistant-selector"><MordeIcon name="folder" /><span className="sr-only">Workspace</span><select aria-label="选择目标 Workspace" value={selectedBinding?.workspace.id ?? workspaceSelection}
                    onChange={(event) => setWorkspaceSelection(event.target.value)}>
                    <option value="">不附加 Workspace</option>
                    {workspaceSelection && !selectedBinding && <option value={workspaceSelection}>所选 Workspace 状态待核验</option>}
                    {workspaceBindings.map((binding) => <option key={binding.workspace.id} value={binding.workspace.id}>
                      {binding.workspace.name || binding.workspace.id} · v{binding.workspace.revision} · {workspaceBindingReady(binding) ? '已应用' : '未就绪'}
                    </option>)}
                  </select></label>}
                </div>
                {enabledMCPConnectors.length > 0 && <details className="assistant-tool-menu"><summary><MordeIcon name="sliders" />工具</summary>
                  <fieldset className="assistant-tool-connectors"><legend>工具连接</legend>
                    <div>{enabledMCPConnectors.map((connector) => <label key={connector.id}>
                      <input type="checkbox" checked={mcpConnectorIDs.includes(connector.id)} onChange={(event) => setMcpConnectorIDs((current) => event.target.checked
                        ? [...new Set([...current, connector.id])] : current.filter((id) => id !== connector.id))} />
                      <span>{connector.name}</span>
                    </label>)}</div>
                  </fieldset>
                </details>}
                {mcpConnectors.isError && !showConsolidatedNetworkError && <div className="error-inline assistant-connector-error" role="alert">工具连接暂不可用<button type="button" className="button button-small button-secondary" onClick={() => void mcpConnectors.refetch()}>重试</button></div>}
              </div>
              <div className="assistant-task-actions" role="group" aria-label="发送操作">
                {preparingAttachmentCount > 0 && <button type="button" className="button button-secondary" onClick={cancelComposerAttachmentPreparation}>取消读取</button>}
                <button className="button button-primary assistant-send" type="submit" disabled={savingMessage || preparingAttachmentCount > 0 || (!content.trim() && !composerAttachments.length)}>
                  {preparingAttachmentCount > 0 ? '读取附件…' : savingMessage ? '保存中…' : '发送'} <span aria-hidden="true">↑</span>
                </button>
              </div>
            </div>
          </form>
        </section>
      </div>
      {taskPanelOpen && <aside id="assistant-task-panel" className="assistant-task-panel" aria-label="任务动态">
        <header className="task-panel-heading">
          <div><h2>任务动态</h2><span>{accountTasks.isSuccess ? `账号任务 · ${accountTasks.data.items.length} 项` : '当前账号可见任务'}</span></div>
          <div className="inline-actions">
            <button type="button" className="icon-button" aria-label="刷新任务动态" onClick={() => { void accountTasks.refetch(); void conversationTasks.refetch(); }}>↻</button>
            <button type="button" className="icon-button" aria-label="关闭任务动态" onClick={() => { setTaskPanelOpen(false); taskPanelToggle.current?.focus(); }}>×</button>
          </div>
        </header>
        <div className="task-panel-list">
          {capabilities.isPending && <p className="muted" role="status">正在确认账号与服务身份…</p>}
          {!accountTasksEnabled && !capabilities.isPending && <p className="muted" role="status">账号或服务身份尚未确认，任务列表暂不可用。</p>}
          {accountTasksEnabled && accountTasks.isPending && <p className="muted" role="status">正在读取账号任务…</p>}
          {accountTasks.isError && <div className="error-inline" role="alert">无法读取完整任务列表，当前任务可能不全：{errorDetails(accountTasks.error).message}
            <button type="button" className="button button-small button-secondary" onClick={() => void accountTasks.refetch()}>重试</button></div>}
          {conversationTasks.isError && accountTasks.isError && <div className="error-inline" role="alert">当前会话活动任务也无法读取：{errorDetails(conversationTasks.error).message}
            <button type="button" className="button button-small button-secondary" onClick={() => void conversationTasks.refetch()}>重试</button></div>}
          {taskQueries.some((query, index) => query.isPending && (currentConversationTaskIDs.has(taskIDs[index] ?? '') || expandedTaskIDs.includes(taskIDs[index] ?? '')))
            && <p className="muted" role="status">正在读取任务详情…</p>}
          {taskQueries.map((query, index) => query.isError && <div className="error-inline" role="alert" key={`task-error:${taskIDs[index]}`}>
            无法读取任务 {taskIDs[index]} 的详情：{errorDetails(query.error).message}
            <button type="button" className="button button-small button-secondary" onClick={() => void query.refetch()}>重试</button></div>)}
          {!accountTasks.isPending && !accountTasks.isError && taskPanelTasks.length === 0 && <p className="muted">还没有任务。</p>}
          {taskPanelTasks.map(renderTaskActivity)}
        </div>
      </aside>}
    </div>
  </section>;
}
