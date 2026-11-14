import { Fragment, useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import type { ChangeEvent, FormEvent } from 'react';
import { Link, useLocation, useNavigate } from 'react-router';
import { useInfiniteQuery, useMutation, useQueries, useQuery, useQueryClient } from '@tanstack/react-query';
import { enterHostInNewTab, errorDetails, newIdempotencyKey, portalApi } from '../app/api';
import type { ConversationDeliveryStatus, HostEmbedTicket, HostEmbedTicketRequest, HostMemberPermission } from '../app/api';
import { readK2TaskProjection } from '../app/k2-task-projection';
import type { K2ResultReview, K2TaskDependency } from '../app/k2-task-projection';
import type { MessageOutboxEntry } from '../app/use-message-outbox';
import { useMessageOutbox } from '../app/use-message-outbox';
import { deliveryStatusText, userVisibleRoutingStatusText } from '../app/message-status';
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
import { ScreenPagination } from '../components/ScreenPagination';
import { sha256Hex } from './personal-data-store';
import { extractAssistantPDFText } from './assistant-pdf';
import { assistantComposerDraftScope, assistantDraftStorage } from './assistant-drafts';
import type { AssistantComposerDraft } from './assistant-drafts';
import './AssistantSidebar.css';
import { FloatingError, FloatingNotice } from '../components/FloatingError';

type Conversation = Awaited<ReturnType<typeof portalApi.conversations>>['items'][number];
type ConversationMessage = Awaited<ReturnType<typeof portalApi.conversationMessages>>['items'][number];
type ConversationMessagePage = Awaited<ReturnType<typeof portalApi.conversationMessages>>;
type ConversationMessagesData = { pages: ConversationMessagePage[]; pageParams: unknown[] };
type TaskSummary = Awaited<ReturnType<typeof portalApi.tasks>>['items'][number];
type Task = Awaited<ReturnType<typeof portalApi.task>>;
type TaskStep = Task['steps'][number];
type TaskAgent = NonNullable<Task['agents']>[number];
type TaskApproval = NonNullable<Task['pending_request']>;
type TaskApprovalTextDisclosure = TaskApproval['disclosures']['risk'];
type TaskApprovalCostDisclosure = TaskApproval['disclosures']['this_cost'];
type TaskApprovalAlternativesDisclosure = TaskApproval['disclosures']['alternatives'];
type TaskStepDetail = Awaited<ReturnType<typeof portalApi.taskStepDetail>>;
type TaskArtifact = Task['artifacts'][number];
type TaskFileFetch = Awaited<ReturnType<typeof portalApi.taskFileFetches>>['items'][number];
type Host = Awaited<ReturnType<typeof portalApi.hosts>>['items'][number];
type ComputerHostOption = { id: string; name: string; url: string; state: string; owned: boolean; permissions: HostMemberPermission[] };
type ComputerEmbedSession = {
  conversationID: string;
  hostID: string;
  hostOrigin: string;
  exchangeURL: string;
  nonce: string;
  ticket?: string;
  targets: HostEmbedTicket['targets'];
  status: 'requesting' | 'submitting' | 'loading' | 'ready' | 'error';
  error?: string;
};
type AssistantDisplayMessage = Omit<ConversationMessage, 'delivery_status'> & { delivery_status?: ConversationDeliveryStatus } & {
  snapshotStatus?: 'streaming' | 'completed' | 'interrupted';
};
type ComposerAttachment = { id: string; file: File; displayName?: string; objectID: string; objectKey: string; uploadKey: string; commitKey: string; attachmentID?: string; needsPreparation?: boolean; preparationCancelled?: boolean };
type OfficePreviewState = { status: 'loading' } | { status: 'ready'; text: string };

function taskApprovalTextDisclosure(value?: TaskApprovalTextDisclosure): string {
  if (!value) return '未知';
  if (value.state !== 'known') return value.state === 'not_applicable' ? '不适用' : '未知';
  return value.summary?.trim() || '服务端标记为已知，但未提供说明';
}

function taskApprovalCostDisclosure(value?: TaskApprovalCostDisclosure): string {
  if (!value) return '未知';
  if (value.state !== 'known') return value.state === 'not_applicable' ? '不适用' : '未知';
  if (value.amount_micros === null || !Number.isSafeInteger(value.amount_micros) || value.amount_micros < 0 ||
    !value.currency || !/^[A-Z]{3}$/.test(value.currency) || !value.basis) return '费用数据不完整';
  const whole = Math.floor(value.amount_micros / 1_000_000).toLocaleString('zh-CN');
  const fraction = String(value.amount_micros % 1_000_000).padStart(6, '0').replace(/0+$/u, '');
  const amount = `${whole}${fraction ? `.${fraction}` : ''}`;
  return `${value.basis === 'estimate' ? '估算' : '已结算'} ${value.currency} ${amount}`;
}

function taskApprovalAlternativesDisclosure(value?: TaskApprovalAlternativesDisclosure): string {
  if (!value) return '未知';
  if (value.state !== 'known') return value.state === 'not_applicable' ? '不适用' : '未知';
  return value.options.length ? value.options.join('；') : '服务端标记为已知，但未提供方案';
}

function conversationTitleFromText(content?: string): string | null {
  const normalized = content?.replace(/\s+/gu, ' ').trim();
  if (!normalized) return null;
  const characters = Array.from(normalized);
  return characters.length > 42 ? `${characters.slice(0, 42).join('')}…` : normalized;
}

function conversationTitleFromMessages(messages: readonly Pick<ConversationMessage, 'role' | 'content'>[]): string | null {
  return conversationTitleFromText([...messages].reverse().find((message) => message.role === 'user')?.content);
}

function isPlaceholderConversationTitle(title?: string): boolean {
  return !title?.trim() || ['新对话', '新旁聊', '对话'].includes(title.trim());
}

function AssistantConversationRow({ item, isMain, selected, recent, previewTitle, onSelect }: {
  item: Conversation; isMain?: boolean; selected: boolean; recent: boolean; previewTitle?: string | null; onSelect: (id: string) => void;
}) {
  return <div className={`conversation-row${isMain ? ' conversation-row-main' : ''}`}>
    <button type="button" className={`conversation-item ${selected ? 'is-selected' : ''}`} onClick={() => onSelect(item.id)}>
      <strong>{isMain ? '主会话' : isPlaceholderConversationTitle(item.title) ? previewTitle || item.title || '新对话' : item.title}</strong>
      {recent && !isMain && <small>本机最近查看</small>}
      <small>{isMain ? '最近更新 · ' : ''}{formatDate(item.updated_at)}</small>
    </button>
  </div>;
}

function conversationMessageStatus(message: AssistantDisplayMessage, task?: Task): string | null {
  if (message.duplicate_of_message_id) {
    const relatedTask = message.related_task_id ? ` · 关联任务“${conversationTitleFromText(task?.objective) ?? message.related_task_id}”` : '';
    const status = task ? ` · ${taskActivityStatusText(task, task)}` : message.related_task_id ? ' · 任务状态待核对' : '';
    return `重复请求 · 原消息 ${message.duplicate_of_message_id}${relatedTask}${status}`;
  }
  if (message.routing_status === 'failed' || message.routing_status === 'pending' || message.routing_status === 'processing') return null;
  const taskTitle = conversationTitleFromText(task?.objective);
  const messageSource = taskTitle ? `任务“${taskTitle}”`
    : message.task_id ? `关联任务 ${message.task_id}` : '此消息';
  const routingStatus = userVisibleRoutingStatusText(message.routing_status);
  if (routingStatus) return `${messageSource} · ${routingStatus}`;
  if (message.routing_status === 'waiting_user') return null;
  if (message.delivery_status === 'applied') return null;
  const deliveryStatus = message.delivery_status === 'pending' && task && terminalTaskStatuses.has(task.status)
    ? '消息未处理，处理已结束' : deliveryStatusText(message.delivery_status);
  return `${messageSource} · ${deliveryStatus}`;
}

function hasCompleteVerifiedArtifactProof(artifact: TaskArtifact): boolean {
  return artifact.verified && Boolean(artifact.workspace_id) && Number.isSafeInteger(artifact.workspace_revision)
    && (artifact.workspace_revision ?? -1) >= 0 && Number.isSafeInteger(artifact.size_bytes)
    && (artifact.size_bytes ?? -1) >= 0 && /^[a-f0-9]{64}$/iu.test(artifact.sha256 ?? '')
    && Boolean(artifact.path.trim()) && Boolean(artifact.media_type.trim());
}

function AssistantOutboxRetryButton({ entry, sendMessage, label }: {
  entry: MessageOutboxEntry; sendMessage: (entry: MessageOutboxEntry, reconcile: boolean) => Promise<void>; label: string;
}) {
  return <button type="button" className="button button-small button-secondary" onClick={() => void sendMessage(entry, true)}>{label}</button>;
}

function AssistantTaskSupplementForm({ task, value, disabled, saving, needsWorkspaceTarget, workspaceID, workspaces, onWorkspaceChange, onChange, onSubmit }: {
  task: Task; value: string; disabled: boolean; saving: boolean;
  needsWorkspaceTarget: boolean; workspaceID: string; workspaces: { id: string; name: string; revision: number }[];
  onWorkspaceChange: (workspaceID: string) => void;
  onChange: (event: ChangeEvent<HTMLTextAreaElement>) => void;
  onSubmit: (event: FormEvent<HTMLFormElement>, task: Task) => void;
}) {
  return <form className="task-supplement-form" aria-label="补充任务" onSubmit={(event) => onSubmit(event, task)}>
    {needsWorkspaceTarget && <div className="task-supplement-target">
      <label htmlFor={`task-supplement-workspace-${task.id}`}>保存到 Workspace</label>
      <select id={`task-supplement-workspace-${task.id}`} aria-label="选择任务 Workspace" value={workspaceID}
        required disabled={disabled} onChange={(event) => onWorkspaceChange(event.target.value)}>
        <option value="">请选择可写 Workspace</option>
        {workspaces.map((workspace) => <option key={workspace.id} value={workspace.id}>{workspace.name} · v{workspace.revision}</option>)}
      </select>
      <small>选择后会把这个任务绑定到该 Workspace，再继续保存文档。</small>
    </div>}
    <label htmlFor={`task-supplement-${task.id}`}>补充任务内容</label>
    <textarea id={`task-supplement-${task.id}`} value={value} maxLength={maxMessageBytes} onChange={onChange} disabled={disabled} />
    <button type="submit" className="button button-primary" disabled={(!value.trim() && !needsWorkspaceTarget) || (needsWorkspaceTarget && !workspaceID) || disabled || saving}>
      {needsWorkspaceTarget ? '选择 Workspace 并继续' : '发送补充任务'}
    </button>
  </form>;
}

function hostEmbedTargetPlan(host: ComputerHostOption): HostEmbedTicketRequest['targets'] {
  if (host.owned) return ['browser', 'terminal', 'workspace', 'files', 'vscode'];
  const has = (permission: HostMemberPermission) => host.permissions.includes(permission);
  const targets: HostEmbedTicketRequest['targets'] = [];
  if (has('desktop:view') || has('desktop:control')) targets.push('browser');
  // Embedded terminal needs a user control lease as well as terminal permission.
  if (has('terminal:control') && has('desktop:control')) targets.push('terminal');
  if (has('workspace:file:read')) targets.push('workspace');
  return targets;
}

function hostEmbedInitialTarget(targets: HostEmbedTicketRequest['targets']): HostEmbedTicketRequest['target'] | null {
  return targets.includes('browser') ? 'browser' : targets.includes('terminal') ? 'terminal'
    : targets.includes('workspace') ? 'workspace' : null;
}

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

function flattenConversationPages(data: ConversationMessagesData | undefined): ConversationMessage[] {
  const unique = new Map<string, ConversationMessage>();
  for (const page of data?.pages ?? []) for (const message of page.items) unique.set(message.id, message);
  return [...unique.values()].sort((left, right) => left.sequence - right.sequence ||
    left.created_at.localeCompare(right.created_at) || left.id.localeCompare(right.id));
}

function appendConversationReceipt(data: ConversationMessagesData | undefined, receipt: ConversationMessage): ConversationMessagesData | undefined {
  if (!data?.pages.length || data.pages.some((page) => page.items.some((item) => item.id === receipt.id))) return data;
  const [latest, ...older] = data.pages;
  if (!latest) return data;
  return { ...data, pages: [{ ...latest, items: [...latest.items, receipt].sort((left, right) => left.sequence - right.sequence) }, ...older] };
}

function browserIanaTimeZone(): string | undefined {
  try {
    const zone = Intl.DateTimeFormat().resolvedOptions().timeZone;
    if (!zone) return undefined;
    new Intl.DateTimeFormat('en', { timeZone: zone });
    return zone;
  } catch { return undefined; }
}

export function assistantRecentConversationKey(serviceInstanceID: string, userID: string): string {
  return `morde.assistant.recentConversation.v1:${serviceInstanceID}:${userID}`;
}

export function assistantMainConversationCacheKey(userID: string): string {
  return `morde.assistant.mainConversation.v1:${window.location.origin}:${userID}`;
}

type CachedMainConversation = { conversationID: string; serviceInstanceID: string | null };

function readMainConversationCache(key: string): CachedMainConversation | null {
  try {
    const value: unknown = JSON.parse(window.localStorage.getItem(key) ?? 'null');
    if (!value || typeof value !== 'object' || !('conversationID' in value) || typeof value.conversationID !== 'string' ||
      !value.conversationID || value.conversationID.length > 200 || !('serviceInstanceID' in value) ||
      (value.serviceInstanceID !== null && typeof value.serviceInstanceID !== 'string')) return null;
    return { conversationID: value.conversationID, serviceInstanceID: value.serviceInstanceID };
  } catch { return null; }
}

function writeMainConversationCache(key: string, value: CachedMainConversation | null): void {
  try {
    if (value) window.localStorage.setItem(key, JSON.stringify(value));
    else window.localStorage.removeItem(key);
  } catch { /* the current page can still use its in-memory conversation */ }
}

export function resolveTaskObservationURL(ticket: string, hostURL: string, hostID: string, portalHostname = window.location.hostname): string {
  let host: URL;
  try { host = new URL(hostURL); } catch { throw new Error('服务器返回了无效的 Workbench 地址。'); }
  const localHTTP = ['portal.localhost', 'localhost', '127.0.0.1', '[::1]'].includes(portalHostname)
    && host.protocol === 'http:' && host.hostname === `${hostID}.hosts.localhost` && !host.port;
  // eslint-disable-next-line no-control-regex -- Control bytes must not enter an opaque credential URL.
  if (!ticket || ticket.length < 16 || ticket.length > 4096 || /[\u0000-\u001f\u007f]/.test(ticket)) throw new Error('Task 观察票据格式无效。');
  if ((!localHTTP && host.protocol !== 'https:') || host.username || host.password || host.hash || host.search || host.pathname !== '/') {
    throw new Error('服务器返回的 Workbench 地址不符合安全规则。');
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
  none: '无副本', retained: '保留中', deleted: '已删除', expired: '已到期',
};
type ConversationFileItem =
  | { kind: 'attachment'; key: string; attachmentID: string; messageID: string; index: number; createdAt: string }
  | { kind: 'artifact'; key: string; taskID: string; taskTitle: string; artifact: TaskArtifact };
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

function taskTargetText(task: TaskSummary, hosts: Host[]): string {
  const host = task.host_id ? hosts.find((item) => item.id === task.host_id) : undefined;
  const computer = task.host_id ? host ? `${host.name || host.id}（${hostStateLabel[host.state] || host.state}）` : `云端电脑 ${task.host_id}（当前不可访问）` : '无电脑目标';
  return task.input_revision === undefined ? computer : `${computer} · 文件版本 v${task.input_revision}`;
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
  if (task.status === 'waiting_user' && task.waiting_reason === 'workspace_unavailable') return '云端电脑文件当前不可用、版本未就绪或无法确认访问权限，本次操作未执行。';
  if (task.status === 'waiting_user' && task.waiting_reason === 'workspace_target_changed') return '任务已绑定其他云端电脑的文件，无法安全切换；请为新目标另发起任务。';
  if (task.status === 'waiting_user' && task.waiting_reason === 'target_result_unknown') return '上次电脑操作的结果需要核对，暂不能重复执行。';
  if (task.status === 'waiting_user' && task.waiting_reason === 'workspace_host_sync') return '任务等待目标云端电脑重新就绪并同步文件；确认版本已应用后才会完成。';
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
  if (task.result_message_pending) return '任务已完成，正在整理回复。';
  if (task.result_message_published) return '任务已完成，最终回复已发布。';
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

function isResourceNoLongerAccessible(error: unknown): boolean {
  return error instanceof ApiError && [401, 403, 404].includes(error.status);
}

const taskEventTypes = [
  'task.status', 'task.step', 'task.operation', 'task.message', 'task.batch_item',
  'task.artifact', 'task.waiting_user', 'task.response', 'task.result', 'task.snapshot_required',
] as const;

function stepTitle(step: Task['steps'][number]): string {
  const mcpPrefix = 'Run cloud dsh MCP tool: ';
  const prefix = ['Run cloud dsh tool: ', mcpPrefix, 'Run Hostless Workspace tool: ']
    .find((value) => step.title.startsWith(value));
  if (!prefix) return step.title;
  const name = step.title.slice(prefix.length);
  if (name === 'morde_web_search' || name.includes('searxng_web_search')) return '搜索网页';
  if (name === 'morde_web_read' || name.includes('web_url_read')) return '读取网页';
  if (name === 'morde_workspace_list') return '查看电脑文件';
  if (name === 'morde_workspace_read') return '读取电脑文件';
  if (name === 'morde_workspace_write') return '保存电脑文件';
  if (prefix === mcpPrefix) return name.startsWith('morde_mcp_') ? '使用 MCP 工具' : `使用 MCP 工具：${name}`;
  if (name.startsWith('browser_')) return '操作浏览器';
  if (name.startsWith('desktop_')) return '操作云端电脑';
  if (name.startsWith('file_')) return '处理文件';
  if (name === 'shell_exec') return '运行命令';
  return '使用工具';
}

function stepDetailToolName(step: TaskStep, fallback?: string | null): string | undefined {
  const prefix = 'Run cloud dsh MCP tool: ';
  if (!step.title.startsWith(prefix)) return fallback ?? undefined;
  const name = step.title.slice(prefix.length);
  return name.startsWith('morde_mcp_') || !name ? 'MCP 工具' : name;
}

function stepDetailObject(step: TaskStep, object: string | null): string | null {
  if (object === null || !step.title.startsWith('Run cloud dsh MCP tool: ') || !object.startsWith('morde_mcp_')) return object;
  return 'MCP 连接器';
}

const stepStatus: Record<Task['steps'][number]['status'], string> = {
  queued: '等待中', running: '进行中', waiting_user: '等待确认', succeeded: '已完成', failed: '失败', unknown: '结果待确认',
};

function stepStatusText(step: TaskStep): string {
  if (step.phase === 'prepare' && step.status === 'failed') return '失败（未派发）';
  if (step.phase === 'prepare' && (step.status === 'queued' || step.status === 'running')) return '准备中';
  return stepStatus[step.status];
}

type TaskActivityGroup = { id: string; title: string; status: string; depth: number; steps: TaskStep[] };
type TaskPanelGroup = { id: string; title: string; tasks: TaskSummary[] };
type TaskPanelRow = { key: string; group: TaskPanelGroup; task: TaskSummary | null; first: boolean };

function taskActivityGroups(task: Task, steps: TaskStep[]): TaskActivityGroup[] {
  const agents = task.agents ?? [];
  const byID = new Map(agents.map((agent) => [agent.id, agent]));
  const roots = agents.filter((agent) => agent.role === 'main' && agent.parent_id === null);
  const groups: TaskActivityGroup[] = [];
  const groupedSteps = new Set<string>();
  const visited = new Set<string>();
  const sequenceFor = (agent: TaskAgent) => task.steps.find((step) => step.id === agent.step_id)?.sequence ?? Number.MAX_SAFE_INTEGER;
  const ownerFor = (step: TaskStep): string | undefined => {
    if (step.agent_id) return step.agent_id;
    const delegations = agents.filter((agent) => agent.step_id === step.id && agent.parent_id);
    return delegations.length === 1 ? delegations[0]?.parent_id ?? undefined : undefined;
  };
  const visit = (agent: TaskAgent, path: number[]) => {
    if (visited.has(agent.id) || byID.get(agent.id) !== agent) return;
    visited.add(agent.id);
    const owned = steps.filter((step) => ownerFor(step) === agent.id).sort((a, b) => a.sequence - b.sequence);
    owned.forEach((step) => groupedSteps.add(step.id));
    const delegation = task.steps.find((step) => step.id === agent.step_id);
    const observed = agent.status === 'running' ? '进行中' : agent.status === 'idle' ? '空闲' : '状态待确认';
    groups.push({ id: `agent:${agent.id}`, title: path.length ? `子 Agent ${path.map((part) => String(part).padStart(2, '0')).join('.')}` : '主 Agent',
      status: agent.role === 'subagent' && delegation ? stepStatusText(delegation) : observed, depth: path.length, steps: owned });
    const children = agents.filter((child) => child.role === 'subagent' && child.parent_id === agent.id)
      .sort((left, right) => sequenceFor(left) - sequenceFor(right) || left.id.localeCompare(right.id));
    children.forEach((child, index) => visit(child, [...path, index + 1]));
  };
  if (roots.length === 1) visit(roots[0]!, []);
  const unassigned = steps.filter((step) => !groupedSteps.has(step.id)).sort((a, b) => a.sequence - b.sequence);
  if (unassigned.length) groups.push({ id: 'unassigned', title: '归属待确认', status: '', depth: 0, steps: unassigned });
  return groups;
}

function latestTaskSnapshot(previous: unknown, next: unknown): unknown {
  const oldTask = previous as Task | undefined;
  const newTask = next as Task;
  return oldTask && oldTask.id === newTask.id && oldTask.event_sequence > newTask.event_sequence ? oldTask : newTask;
}

function TaskStepDetails({ task, step, conversationID, actorID, serviceInstanceID }: {
  task: Task; step: TaskStep; conversationID: string; actorID: string; serviceInstanceID: string;
}) {
  const client = useQueryClient();
  const [accessRevoked, setAccessRevoked] = useState(false);
  const queryKey = ['task-step-detail', actorID, serviceInstanceID, conversationID, task.id, step.id] as const;
  const detail = useQuery({
    queryKey,
    queryFn: async () => {
      const result = await portalApi.taskStepDetail(task.id, step.id);
      if (result.task_id !== task.id || result.step_id !== step.id) throw new Error('详情身份不匹配');
      return result.detail_state === 'available' ? result : { ...result, object: null, input: null, result: null };
    },
    enabled: Boolean(actorID && serviceInstanceID && conversationID) && !accessRevoked,
    retry: false,
    gcTime: 0,
    structuralSharing: (previous: unknown, next: unknown) => {
      const oldDetail = previous as TaskStepDetail | undefined;
      const newDetail = next as TaskStepDetail;
      if (newDetail.detail_state !== 'available') return newDetail;
      return oldDetail && oldDetail.task_id === newDetail.task_id && oldDetail.step_id === newDetail.step_id &&
        oldDetail.event_sequence > newDetail.event_sequence ? oldDetail : newDetail;
    },
    refetchInterval: (query) => !terminalTaskStatuses.has(task.status) ||
      (query.state.data && query.state.data.event_sequence < task.event_sequence) ? 1500 : false,
  });
  const inaccessible = isResourceNoLongerAccessible(detail.error);
  if (inaccessible && !accessRevoked) setAccessRevoked(true);
  useEffect(() => {
    if (!accessRevoked) return;
    client.removeQueries({ queryKey: ['task-step-detail', actorID, serviceInstanceID, conversationID, task.id, step.id], exact: true });
  }, [accessRevoked, actorID, client, conversationID, serviceInstanceID, step.id, task.id]);
  if (accessRevoked || inaccessible) return <p className="task-step-detail-message" role="status">详情不可访问或已删除。</p>;
  if (detail.isError) return <p className="task-step-detail-message" role="alert">详情暂不可用。
    <button type="button" className="button button-small button-secondary" onClick={() => void detail.refetch()}>重试</button></p>;
  if (!actorID || !serviceInstanceID) return <p className="task-step-detail-message" role="status">账号或服务身份暂不可确认。</p>;
  const data = detail.data;
  if (!data) return <p className="task-step-detail-message" role="status">正在读取详情…</p>;
  if (data.task_id !== task.id || data.step_id !== step.id) return <p className="task-step-detail-message" role="alert">详情身份不匹配。</p>;
  const unavailable: Record<Exclude<TaskStepDetail['detail_state'], 'available'>, string> = {
    not_recorded: '详情未记录。', expired: '详情已过期。', restricted: '没有权限查看详情。',
  };
  const toolName = stepDetailToolName(step, data.tool_name);
  const object = stepDetailObject(step, data.object);
  if (data.detail_state === 'available' && data.event_sequence < task.event_sequence) return <p className="task-step-detail-message" role="status">正在读取详情…</p>;
  return <div className="task-step-detail" aria-label="动作详情">
    {toolName && <p><strong>工具</strong> {toolName}</p>}
    <p><strong>时间</strong> {formatDate(step.created_at)}</p>
    {data.detail_state !== 'available' ? <p role="status">{unavailable[data.detail_state]}</p> : <>
      {object !== null && <p><strong>对象</strong> {object || '（空）'}</p>}
      {data.input !== null && <p><strong>输入</strong> {data.input || '（空）'}</p>}
      {data.result !== null && <p><strong>结果</strong> {data.result || '（空结果）'}</p>}
      {data.object === null && data.input === null && data.result === null && <p>没有可展示的输入或结果。</p>}
      {data.truncated && <p>内容已截断。</p>}
    </>}
  </div>;
}

const taskStatusLabel: Record<Task['status'], string> = {
  queued: '排队中', running: '运行中', waiting_user: '等待用户', pausing: '正在暂停', paused: '已暂停',
  cancelling: '正在停止', completed: '已完成', failed: '执行失败', cancelled: '已停止',
};

function taskActivityStatusText(summary: TaskSummary, task?: Task): string {
  const status = task?.status ?? summary.status;
  if (status === 'completed' && ['pending', 'processing'].includes(summary.result_review?.state ?? '')) return '回复整理中';
  if (status === 'waiting_user' && task?.pending_request) return '待审批';
  if (status === 'waiting_user' && task?.clarification_request) return '待补充信息';
  const labels: Record<string, string> = {
    queued: '等待执行', running: '进行中', waiting_user: '待你处理', pausing: '正在暂停', paused: '已暂停',
    cancelling: '正在停止', completed: '已完成', failed: '失败', cancelled: '已停止',
  };
  return labels[status] ?? '状态待确认';
}

function taskActivityProgressText(summary: TaskSummary, task: Task | undefined, cloudTasksAvailable: boolean | undefined): string {
  if (!task) return summary.status === 'running' ? '正在处理，打开详情查看进展。'
    : summary.status === 'queued' ? '已排队等待开始。' : '打开详情查看任务状态。';
  if (task.status === 'completed' && (task.result_message_pending || ['pending', 'processing'].includes(summary.result_review?.state ?? '')))
    return '任务已完成，回复整理中。';
  if (task.status === 'waiting_user') {
    if (task.pending_request) return `等待审批：${task.pending_request.title}`;
    if (task.clarification_request) return `等待补充：${task.clarification_request.question}`;
  }
  const currentStep = task.steps.find((step) => step.id === task.current_step);
  const latestStep = currentStep?.kind !== 'model' ? currentStep : undefined;
  const progressStep = latestStep ?? [...task.steps].reverse().find((step) => ['tool', 'subagent', 'verification', 'verify', 'progress'].includes(step.kind));
  if (progressStep && task.status === 'running') return stepTitle(progressStep);
  if (task.status === 'failed') return '任务执行失败，查看工作过程或更多信息了解原因。';
  if (task.status === 'completed') return '任务已完成，打开详情查看结果。';
  return taskStatusText(task, cloudTasksAvailable);
}

function taskFileFetchFailureText(reason: TaskFileFetch['failure_reason']): string {
  const labels: Record<NonNullable<TaskFileFetch['failure_reason']>, string> = {
    access_denied: '读取权限已变更', source_changed: '本机文件在读取期间发生变化', file_unavailable: '本机文件不可用',
    limit_exceeded: '文件超出大小限制', unsupported: '暂不支持此文件类型', source_unavailable: '本机暂不可用',
    transfer_failed: '文件传输失败',
  };
  return reason ? labels[reason] : '文件读取失败';
}

function taskFileFetchContentText(item: TaskFileFetch): string {
  if (item.status !== 'completed') return '传输完成后再检查文件内容是否可读取。';
  if (item.parse_status === 'extracted') return '内容已提取，可供任务处理。';
  if (item.parse_status === 'raw_only') return '云端保留了原文件，当前无法提取正文。';
  if (item.parse_status === 'failed') {
    const reasons: Record<NonNullable<TaskFileFetch['parse_error']>, string> = {
      invalid_encoding: '文件编码无法读取', invalid_document: '文件格式无法读取',
      result_too_large: '文件内容超过读取上限', content_unavailable: '文件内容暂不可用', integrity_failed: '文件校验未通过',
    };
    return `文件已传输，${item.parse_error ? reasons[item.parse_error] : '内容读取失败'}。`;
  }
  if (item.parse_status === 'unavailable') return '文件已传输，但内容暂不可用。';
  return '文件已传输，内容处理状态暂不可用。';
}

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
    let resyncing = false;
    let retry: ReturnType<typeof setTimeout> | undefined;
    const resync = () => {
      if (disposed || resyncing) return;
      resyncing = true;
      source.close();
      onState(task.id, 'resyncing');
      void client.fetchQuery({ queryKey: ['task', task.id], queryFn: () => portalApi.task(task.id), staleTime: 0,
        structuralSharing: latestTaskSnapshot }).then((snapshot) => {
        if (!disposed) {
          if (snapshot.event_sequence < cursor.current) {
            retry = setTimeout(() => { resyncing = false; resync(); }, 1000);
            return;
          }
          cursor.current = snapshot.event_sequence;
          void client.invalidateQueries({ queryKey: ['conversation-messages', conversationID] });
          void client.invalidateQueries({ queryKey: ['task-file-fetches', task.id] });
          void client.invalidateQueries({ queryKey: ['task-step-detail'], predicate: (query) => query.queryKey[4] === task.id });
          setGeneration((value) => value + 1);
        }
      }).catch((error: unknown) => {
        if (!disposed) {
          onState(task.id, 'disconnected');
          if (!isResourceNoLongerAccessible(error)) retry = setTimeout(() => setGeneration((value) => value + 1), 3000);
        }
      });
    };
    source.onopen = () => { if (!disposed && !resyncing) onState(task.id, 'connected'); };
    source.onerror = () => resync();
    for (const type of taskEventTypes) {
      source.addEventListener(type, (event) => {
        if (disposed || resyncing) return;
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
        void client.invalidateQueries({ queryKey: ['task-step-detail'], predicate: (query) => query.queryKey[4] === task.id });
        if (type === 'task.status' || type === 'task.waiting_user') {
          void client.invalidateQueries({ queryKey: ['assistant-account-tasks'] });
          void client.invalidateQueries({ queryKey: ['conversation-tasks', conversationID] });
        }
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
  const { notify, dismiss } = useToast();
  const { user } = useAuth();
  const location = useLocation();
  const navigate = useNavigate();
  const navigationIdea = typeof location.state === 'object' && location.state !== null
    ? location.state as { ideaID?: unknown; ideaPrompt?: unknown }
    : null;
  const ideaPrompt = typeof navigationIdea?.ideaPrompt === 'string' ? navigationIdea.ideaPrompt : '';
  const hasNavigationIdea = typeof navigationIdea?.ideaID === 'string' && Boolean(navigationIdea.ideaID) && Boolean(ideaPrompt);
  const [conversationSelection, setConversationSelection] = useState<{ scope: string; id: string | null } | null>(null);
  const [composerDraftError, setComposerDraftError] = useState<string | null>(null);
  const [hydratedComposerDraftKey, setHydratedComposerDraftKey] = useState<string | null>(null);
  const [hostSelection, setHostSelection] = useState<string | null>(null);
  const [workspaceSelection, setWorkspaceSelection] = useState<string | null>(null);
  const [retryAcceptedMessageIDs, setRetryAcceptedMessageIDs] = useState<Set<string>>(() => new Set());
  const [retryAcceptedResultReviewTaskIDs, setRetryAcceptedResultReviewTaskIDs] = useState<Set<string>>(() => new Set());
  const [modelID, setModelID] = useState('');
  const [mcpConnectorIDs, setMcpConnectorIDs] = useState<string[]>([]);
  const [content, setContent] = useState(() => hasNavigationIdea ? ideaPrompt : '');
  const [savingMessage, setSavingMessage] = useState(false);
  const [composerAttachments, setComposerAttachments] = useState<ComposerAttachment[]>([]);
  const [officePreviews, setOfficePreviews] = useState<Record<string, OfficePreviewState>>({});
  const [preparingAttachmentCount, setPreparingAttachmentCount] = useState(0);
  const [preparingAttachmentIDsForRender, setPreparingAttachmentIDsForRender] = useState<ReadonlySet<string>>(() => new Set());
  const [clarificationAnswers, setClarificationAnswers] = useState<Record<string, string>>({});
  const [taskSupplementDrafts, setTaskSupplementDrafts] = useState<Record<string, string>>({});
  const [taskWorkspaceSelections, setTaskWorkspaceSelections] = useState<Record<string, string>>({});
  const [supplementTaskID, setSupplementTaskID] = useState<string | null>(null);
  const [taskDetailTaskID, setTaskDetailTaskID] = useState<string | null>(null);
  const [taskStepSelection, setTaskStepSelection] = useState<{ taskID: string; stepID: string } | null>(null);
  const [visibleActivitySteps, setVisibleActivitySteps] = useState<Record<string, number>>({});
  const [taskPanelRecentOpen, setTaskPanelRecentOpen] = useState(false);
  const [taskPanelTab, setTaskPanelTab] = useState<'tasks' | 'files' | 'computer'>('tasks');
  const [taskPanelOpen, setTaskPanelOpen] = useState(false);
  const [computerHostID, setComputerHostID] = useState<string | null>(null);
  const [computerEmbed, setComputerEmbed] = useState<ComputerEmbedSession | null>(null);
  const [computerControlExpanded, setComputerControlExpanded] = useState(false);
  const [computerConnecting, setComputerConnecting] = useState(false);
  const [computerPanelError, setComputerPanelError] = useState('');
  const computerFrame = useRef<HTMLIFrameElement>(null);
  const computerEmbedRef = useRef<ComputerEmbedSession | null>(null);
  const computerRequestGeneration = useRef(0);
  const submittedEmbedNonce = useRef<string | null>(null);
  const initializedEmbedNonce = useRef<string | null>(null);
  useLayoutEffect(() => { computerEmbedRef.current = computerEmbed; }, [computerEmbed]);
  const postComputerVisibility = useCallback((visible: boolean) => {
    const session = computerEmbedRef.current;
    if (!session || initializedEmbedNonce.current !== session.nonce) return;
    computerFrame.current?.contentWindow?.postMessage({
      channel: 'morde.workbench.embed', version: 1, type: 'visibility', nonce: session.nonce, visible,
    }, session.hostOrigin);
  }, []);
  const taskCardButtons = useRef(new Map<string, HTMLButtonElement>());
  const [sideConversationsOpen, setSideConversationsOpen] = useState(false);
  const [conversationSearch, setConversationSearch] = useState('');
  const [showJumpToLatest, setShowJumpToLatest] = useState(false);
  const [historyPageLoadVersion, setHistoryPageLoadVersion] = useState(0);
  const [openingTaskID, setOpeningTaskID] = useState('');
  const [taskObservationError, setTaskObservationError] = useState<{ taskID: string; message: string } | null>(null);
  const [taskObservationRequest, setTaskObservationRequest] = useState(false);
  const pendingMessageSave = useRef<Promise<void>>(Promise.resolve());
  const savingMessageRef = useRef(false);
  const preparingAttachmentCountRef = useRef(0);
  const attachmentPreparationCounts = useRef(new Map<string, number>());
  const attachmentPreparationGenerations = useRef(new Map<string, number>());
  const pdfPreparationControllers = useRef(new Map<string, Set<AbortController>>());
  const preparingAttachmentIDsByDraft = useRef(new Map<string, Set<string>>());
  const preparingAttachmentIDs = useRef(new Set<string>());
  const sendingMessageIDs = useRef(new Set<string>());
  const receiptOrder = useRef(new MessageReceiptOrder());
  const newConversationKey = useRef<string | null>(null);
  const [pendingTaskActions, setPendingTaskActions] = useState<Map<string, { fingerprint: string; key: string }>>(() => new Map());
  const ensureMainKey = useRef<{ userID: string; key: string } | null>(null);
  const ensureAttemptedUserID = useRef<string | null>(null);
  const conversationView = useRef<HTMLDivElement>(null);
  const olderMessagesAnchor = useRef<{ height: number; top: number } | null>(null);
  const activeComposerDraftIdentity = useRef<string | null>(null);
  const composerDraftWrite = useRef<Promise<void>>(Promise.resolve());
  const composerEditRevision = useRef(0);
  const composerSwitchGeneration = useRef(0);
  const activeDraftLoad = useRef<string | null>(null);
  const localConversationKeyRef = useRef<string | null>(null);
  const taskPanelToggle = useRef<HTMLButtonElement>(null);
  const followConversation = useRef(true);
  useEffect(() => () => {
    for (const controllers of pdfPreparationControllers.current.values()) for (const controller of controllers) controller.abort();
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
    queryKey: ['conversations', conversationSearch.trim()],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) => portalApi.conversations(pageParam, 5, conversationSearch.trim() || undefined),
    getNextPageParam: (page) => page.next_cursor ?? undefined,
  });
  const mainConversation = useQuery({
    queryKey: ['main-conversation', user?.id], queryFn: portalApi.mainConversation,
    enabled: Boolean(user), retry: false,
  });
  const mainConversationCacheKey = user ? assistantMainConversationCacheKey(user.id) : null;
  const cachedMainConversation = useQuery({
    queryKey: ['assistant-main-conversation-cache', mainConversationCacheKey],
    queryFn: () => mainConversationCacheKey ? readMainConversationCache(mainConversationCacheKey) : null,
    enabled: Boolean(mainConversationCacheKey),
    initialData: () => mainConversationCacheKey ? readMainConversationCache(mainConversationCacheKey) : null,
    staleTime: Infinity,
  });
  const models = useQuery({ queryKey: ['model-connections'], queryFn: portalApi.modelConnections, staleTime: 60_000 });
  const workspaces = useQuery({ queryKey: ['assistant-workspaces'], queryFn: portalApi.workspaces, staleTime: 30_000 });
  const mcpConnectors = useQuery({ queryKey: ['mcp-connectors'], queryFn: portalApi.mcpConnectors, retry: false });
  const availableModels = useQuery({ queryKey: ['available-models'], queryFn: portalApi.availableModels });
  const preferences = useQuery({ queryKey: ['user-preferences'], queryFn: portalApi.preferences });
  const hosts = useQuery({
    queryKey: ['assistant-hosts'], queryFn: portalApi.hosts,
    refetchInterval: (query) => query.state.data?.items.some((host) => ['creating', 'stopping', 'deleting'].includes(host.state)) ? 3000 : 30000,
  });
  const sharedHosts = useQuery({
    queryKey: ['assistant-shared-hosts'], queryFn: portalApi.sharedHosts,
    enabled: taskPanelOpen && taskPanelTab === 'computer',
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
  const cachedMainConversationID = cachedMainConversation.data && (!capabilities.data?.service_instance_id ||
    cachedMainConversation.data.serviceInstanceID === capabilities.data.service_instance_id)
    ? cachedMainConversation.data.conversationID : null;
  const mainConversationID = mainConversation.data?.status === 'ready' ? mainConversation.data.conversation_id
    : mainConversation.data && !mainConversation.isError ? null : cachedMainConversationID;
  useEffect(() => {
    if (!mainConversationCacheKey) return;
    if (mainConversation.data?.status === 'ready' && mainConversation.data.conversation_id) {
      const next = { conversationID: mainConversation.data.conversation_id,
        serviceInstanceID: capabilities.data?.service_instance_id ?? cachedMainConversation.data?.serviceInstanceID ?? null };
      if (cachedMainConversation.data?.conversationID !== next.conversationID || cachedMainConversation.data.serviceInstanceID !== next.serviceInstanceID) {
        writeMainConversationCache(mainConversationCacheKey, next);
        client.setQueryData(['assistant-main-conversation-cache', mainConversationCacheKey], next);
      }
    } else if (mainConversation.data && !mainConversation.isError && cachedMainConversation.data) {
      writeMainConversationCache(mainConversationCacheKey, null);
      client.setQueryData(['assistant-main-conversation-cache', mainConversationCacheKey], null);
    }
  }, [cachedMainConversation.data, capabilities.data?.service_instance_id, client, mainConversation.data, mainConversation.isError, mainConversationCacheKey]);
  const recentConversationID = recentConversation.data && conversationsData.some((item) => item.id === recentConversation.data)
    ? recentConversation.data : null;
  const selectedConversationValue = scopedConversationSelection?.id;
  const selectedLocalDraftKey = selectedConversationValue?.startsWith('local:') ? selectedConversationValue.slice('local:'.length) : null;
  const conversationID = selectedConversationValue === undefined ? mainConversationID
    : selectedLocalDraftKey ? null : selectedConversationValue;
  const [panelConversationID, setPanelConversationID] = useState(conversationID);
  if (panelConversationID !== conversationID) {
    setPanelConversationID(conversationID);
    setTaskDetailTaskID(null);
    setTaskStepSelection(null);
    setVisibleActivitySteps({});
    setTaskPanelRecentOpen(false);
    setTaskPanelTab('tasks');
    setComputerConnecting(false);
    setComputerEmbed(null);
    setComputerControlExpanded(false);
    setComputerHostID(null);
    setComputerPanelError('');
  }
  const setConversationID = (id: string | null) => setConversationSelection({ scope: conversationScope, id });
  const selectedConversation = useRef(conversationID);
  useLayoutEffect(() => {
    selectedConversation.current = conversationID;
    localConversationKeyRef.current = selectedLocalDraftKey;
  }, [conversationID, selectedLocalDraftKey]);
  const composerDraftScope = user?.id && capabilities.data?.service_instance_id
    ? assistantComposerDraftScope(capabilities.data.service_instance_id, user.id) : null;
  const composerDraftID = selectedLocalDraftKey ? `local:${selectedLocalDraftKey}` : conversationID;
  const composerDraftIdentity = composerDraftScope && composerDraftID
    ? JSON.stringify([composerDraftScope, composerDraftID]) : null;
  const composerDrafts = useQuery({
    queryKey: ['assistant-composer-drafts', composerDraftScope],
    queryFn: () => assistantDraftStorage.list(composerDraftScope!),
    enabled: Boolean(composerDraftScope), retry: false, staleTime: 30_000,
  });
  const localComposerDrafts = (composerDrafts.data ?? []).filter((draft) => draft.conversationID === null && draft.id.startsWith('local:'))
    .sort((left, right) => right.updatedAt - left.updatedAt);
  const latestComposerDraft = useRef<{ scope: string; identity: string; draft: AssistantComposerDraft } | null>(null);
  const composerDraftSnapshots = useRef(new Map<string, { scope: string; draft: AssistantComposerDraft }>());
  const migratedComposerDrafts = useRef(new Map<string, string>());
  const preparationKeysForIdentity = useCallback((identity: string) => {
    const keys = [identity];
    for (const [source, target] of migratedComposerDrafts.current) {
      const [scope] = JSON.parse(source) as [string, string];
      if (JSON.stringify([scope, target]) === identity) keys.push(source);
    }
    return keys;
  }, []);
  const updateComposerDraftSnapshot = (change: Partial<AssistantComposerDraft>) => {
    const current = latestComposerDraft.current;
    if (!current || current.identity !== composerDraftIdentity) return;
    const draft = { ...current.draft, ...change };
    latestComposerDraft.current = { ...current, draft };
    composerDraftSnapshots.current.set(current.identity, { scope: current.scope, draft });
    const migratedID = migratedComposerDrafts.current.get(current.identity);
    if (migratedID) composerDraftSnapshots.current.set(JSON.stringify([current.scope, migratedID]),
      { scope: current.scope, draft: { ...draft, id: migratedID, conversationID: migratedID } });
  };
  const composerInputSnapshot = useRef({ content, attachments: composerAttachments.length });
  const draftSnapshot: AssistantComposerDraft | null = composerDraftScope && composerDraftID ? {
    id: composerDraftID,
    conversationID: selectedLocalDraftKey ? null : conversationID,
    content, modelID, hostID: hostSelection ?? '', mcpConnectorIDs: [...mcpConnectorIDs],
    attachments: composerAttachments.map((item) => {
      const preview = officePreviews[item.id];
      return {
        id: item.id, name: item.file.name, ...(item.displayName ? { displayName: item.displayName } : {}),
        mediaType: assistantFileMediaType(item.file), blob: item.file, objectID: item.objectID, objectKey: item.objectKey,
        uploadKey: item.uploadKey, commitKey: item.commitKey, ...(item.attachmentID ? { attachmentID: item.attachmentID } : {}),
        ...(item.needsPreparation ? { needsPreparation: true } : {}), ...(item.preparationCancelled ? { preparationCancelled: true } : {}),
        ...(preview?.status === 'ready' ? { previewText: preview.text } : {}),
      };
    }),
    updatedAt: 0,
  } : null;
  useLayoutEffect(() => {
    activeComposerDraftIdentity.current = composerDraftIdentity;
    const currentDraft = draftSnapshot;
    latestComposerDraft.current = currentDraft && composerDraftScope && composerDraftIdentity
      ? { scope: composerDraftScope, identity: composerDraftIdentity, draft: currentDraft } : null;
    composerInputSnapshot.current = { content, attachments: composerAttachments.length };
    if (currentDraft && composerDraftScope && composerDraftIdentity) {
      composerDraftSnapshots.current.set(composerDraftIdentity, { scope: composerDraftScope, draft: currentDraft });
      const migratedID = migratedComposerDrafts.current.get(composerDraftIdentity);
      if (migratedID) composerDraftSnapshots.current.set(JSON.stringify([composerDraftScope, migratedID]),
        { scope: composerDraftScope, draft: { ...currentDraft, id: migratedID, conversationID: migratedID } });
    }
  });
  useLayoutEffect(() => {
    const count = composerDraftIdentity ? preparationKeysForIdentity(composerDraftIdentity)
      .reduce((total, key) => total + (attachmentPreparationCounts.current.get(key) ?? 0), 0) : 0;
    preparingAttachmentCountRef.current = count;
    setPreparingAttachmentCount(count);
  }, [composerDraftIdentity, preparationKeysForIdentity]);
  const persistComposerDraft = useCallback(async (scope: string, draft: AssistantComposerDraft) => {
    const migratedID = migratedComposerDrafts.current.get(JSON.stringify([scope, draft.id]));
    const currentDraft = { ...draft, ...(migratedID ? { id: migratedID, conversationID: migratedID } : {}), updatedAt: Date.now() };
    const identity = JSON.stringify([scope, currentDraft.id]);
    client.setQueryData<AssistantComposerDraft[]>(['assistant-composer-drafts', scope], (previous) => {
      const next = (previous ?? []).filter((item) => item.id !== currentDraft.id);
      return [...next, currentDraft].sort((left, right) => right.updatedAt - left.updatedAt);
    });
    const write = composerDraftWrite.current.then(() => assistantDraftStorage.put(scope, currentDraft));
    composerDraftWrite.current = write.catch(() => undefined);
    try {
      await write;
      setComposerDraftError(null);
      return identity;
    } catch (error) {
      setComposerDraftError(errorDetails(error).message);
      throw error;
    }
  }, [client]);
  const persistActiveComposerDraft = useCallback(async () => {
    const current = latestComposerDraft.current;
    if (!current) return true;
    try { await persistComposerDraft(current.scope, current.draft); return true; }
    catch { return false; }
  }, [persistComposerDraft]);
  const restoreComposerDraft = useCallback((draft: AssistantComposerDraft | null, scope: string, id: string) => {
    setContent(draft?.content ?? '');
    setModelID(draft?.modelID ?? '');
    setHostSelection(draft?.hostID || null);
    setMcpConnectorIDs(draft?.mcpConnectorIDs ?? []);
    const attachments: ComposerAttachment[] = [];
    const previews: Record<string, OfficePreviewState> = {};
    for (const item of draft?.attachments ?? []) {
      try {
        if (!(item.blob instanceof Blob)) throw new Error('附件原文件不可用。');
        attachments.push({ id: item.id, file: new File([item.blob], item.name, { type: item.mediaType }),
          ...(item.displayName ? { displayName: item.displayName } : {}), objectID: item.objectID,
          objectKey: item.objectKey, uploadKey: item.uploadKey, commitKey: item.commitKey,
          ...(item.attachmentID ? { attachmentID: item.attachmentID } : {}), ...(item.needsPreparation ? { needsPreparation: true } : {}),
          ...(item.preparationCancelled ? { preparationCancelled: true } : {}) });
        if (item.previewText !== undefined) previews[item.id] = { status: 'ready', text: item.previewText };
      } catch {
        setComposerDraftError('部分附件原文件已不可用；草稿文字和其他附件仍已保留，请重新选择该文件。');
      }
    }
    setComposerAttachments(attachments);
    setOfficePreviews(previews);
    const identity = JSON.stringify([scope, id]);
    activeDraftLoad.current = identity;
    setHydratedComposerDraftKey(identity);
  }, []);
  useEffect(() => {
    const draftSnapshots = composerDraftSnapshots.current;
    const migratedDrafts = migratedComposerDrafts.current;
    if (!composerDraftScope || !composerDraftID || !composerDraftIdentity) {
      activeDraftLoad.current = null;
      return;
    }
    if (activeDraftLoad.current === composerDraftIdentity && hydratedComposerDraftKey === composerDraftIdentity) return;
    const previousIdentity = activeDraftLoad.current;
    const preserveUnboundInput = previousIdentity === null && (composerInputSnapshot.current.content || composerInputSnapshot.current.attachments > 0);
    const editRevision = composerEditRevision.current;
    activeDraftLoad.current = composerDraftIdentity;
    let cancelled = false;
    if (previousIdentity !== null) {
      setContent('');
      setModelID('');
      setHostSelection(null);
      setWorkspaceSelection(null);
      setMcpConnectorIDs([]);
      setComposerAttachments([]);
      setOfficePreviews({});
    }
    void assistantDraftStorage.get(composerDraftScope, composerDraftID).then((draft) => {
      if (cancelled || activeDraftLoad.current !== composerDraftIdentity) return;
      if (composerEditRevision.current !== editRevision) {
        setHydratedComposerDraftKey(composerDraftIdentity);
        setComposerDraftError(null);
        return;
      }
      if (draft) restoreComposerDraft(draft, composerDraftScope, composerDraftID);
      else {
        if (!preserveUnboundInput) {
          setContent('');
          setModelID('');
          setHostSelection(null);
          setWorkspaceSelection(null);
          setMcpConnectorIDs([]);
          setComposerAttachments([]);
          setOfficePreviews({});
        }
        setHydratedComposerDraftKey(composerDraftIdentity);
      }
      setComposerDraftError(null);
    }).catch((error) => {
      if (cancelled || activeDraftLoad.current !== composerDraftIdentity) return;
      setComposerDraftError(errorDetails(error).message);
      setHydratedComposerDraftKey(composerDraftIdentity);
    });
    return () => {
      cancelled = true;
      const previous = draftSnapshots.get(composerDraftIdentity);
      if (previous && !migratedDrafts.has(composerDraftIdentity)) {
        void persistComposerDraft(previous.scope, previous.draft).catch(() => undefined);
      }
    };
  }, [composerDraftID, composerDraftIdentity, composerDraftScope, hydratedComposerDraftKey, persistComposerDraft, restoreComposerDraft]);
  useEffect(() => {
    if (!composerDraftScope || !composerDraftIdentity || hydratedComposerDraftKey !== composerDraftIdentity) return;
    const snapshot = latestComposerDraft.current;
    if (!snapshot || snapshot.identity !== composerDraftIdentity) return;
    const timer = window.setTimeout(() => {
      void persistComposerDraft(snapshot.scope, snapshot.draft).catch(() => undefined);
    }, 250);
    return () => window.clearTimeout(timer);
  }, [composerDraftIdentity, composerDraftScope, content, modelID, hostSelection, mcpConnectorIDs, composerAttachments, officePreviews, hydratedComposerDraftKey, persistComposerDraft]);
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
  const conversationPreviewItems = sideConversations.slice(0, visibleSideConversationCount);
  const conversationPreviewQueries = useQueries({ queries: conversationPreviewItems.map((item) => ({
    queryKey: ['conversation-preview', item.id],
    queryFn: () => portalApi.conversationPreview(item.id),
    enabled: sideConversationsOpen && isPlaceholderConversationTitle(item.title),
    staleTime: 60_000,
    retry: false,
  })) });
  const conversationPreviewTitles = new Map(conversationPreviewItems.map((item, index) => {
    const preview = conversationPreviewQueries[index];
    return [item.id, isResourceNoLongerAccessible(preview?.error) ? null : conversationTitleFromMessages(preview?.data?.items ?? [])] as const;
  }));
  const messages = useInfiniteQuery({
    queryKey: ['conversation-messages', conversationID],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) => portalApi.conversationMessages(conversationID!, pageParam),
    getNextPageParam: (page) => page.next_cursor ?? undefined,
    enabled: Boolean(conversationID),
    refetchInterval: (query) => query.state.data?.pages.some((page) => page.items.some((item) =>
      item.routing_status === 'pending' || item.routing_status === 'processing' || item.routing_status === 'waiting_adjustment')) ? 2000 : false,
  });
  const conversationContentAccessRevoked = isResourceNoLongerAccessible(messages.error);
  const conversationMessageItems = conversationContentAccessRevoked ? [] : flattenConversationPages(messages.data);
  const conversationTasks = useQuery({
    queryKey: ['conversation-tasks', conversationID],
    queryFn: () => portalApi.conversationTasks(conversationID!),
    enabled: Boolean(conversationID),
    refetchInterval: 5000,
  });
  const recentConversationTasks = useQuery({
    queryKey: ['conversation-tasks', conversationID, 'recent'],
    queryFn: () => portalApi.conversationTasks(conversationID!, { view: 'recent' }),
    enabled: Boolean(conversationID) && taskPanelOpen,
    refetchInterval: 30000,
  });
  const conversationTaskItems = conversationContentAccessRevoked ? [] : conversationTasks.data?.items ?? [];
  const recentConversationTaskItems = conversationContentAccessRevoked ? [] : recentConversationTasks.data?.items ?? [];
  const taskIDs = [...new Set([
    ...conversationTaskItems.map((item) => item.id),
    ...recentConversationTaskItems.map((item) => item.id),
    ...conversationMessageItems.flatMap((message) => [message.task_id, message.related_task_id].filter((id): id is string => Boolean(id))),
  ])];
  const currentConversationTaskIDs = new Set([
    ...conversationTaskItems.map((item) => item.id),
    ...recentConversationTaskItems.map((item) => item.id),
    ...conversationMessageItems.flatMap((message) => [message.task_id, message.related_task_id].filter((id): id is string => Boolean(id))),
  ]);
  const taskQueries = useQueries({ queries: taskIDs.map((id) => ({
    queryKey: ['task', id], queryFn: () => portalApi.task(id),
    enabled: currentConversationTaskIDs.has(id) || taskDetailTaskID === id,
    structuralSharing: latestTaskSnapshot,
    refetchInterval: (query: { state: { data: Task | undefined } }) => query.state.data?.result_message_pending ? 1000
      : query.state.data && terminalTaskStatuses.has(query.state.data.status) ? false
        : query.state.data && query.state.data.conversation_id !== conversationID ? 5000 : 1500,
  })) });
  const taskFileFetchQueries = useQueries({ queries: taskIDs.map((id, index) => ({
    queryKey: ['task-file-fetches', id], queryFn: () => portalApi.taskFileFetches(id),
    enabled: taskPanelOpen && taskDetailTaskID === id,
    refetchInterval: (query: { state: { data: { items: TaskFileFetch[] } | undefined } }) => {
      const task = taskQueries[index]?.data;
      return query.state.data?.items.some((item) => activeTaskFileFetchStatuses.has(item.status)) ||
        (task && !terminalTaskStatuses.has(task.status)) ? 1000 : false;
    },
  })) });
  const allTasks = taskQueries.flatMap((query) => query.data && !isResourceNoLongerAccessible(query.error) ? [query.data] : []);
  const tasks = allTasks.filter((task) => task.conversation_id === conversationID);
  const allTasksByID = new Map(allTasks.map((task) => [task.id, task]));
  const taskPanelIDs = [...new Set([
    ...conversationTaskItems.map((item) => item.id),
    ...recentConversationTaskItems.map((item) => item.id),
    ...conversationMessageItems.flatMap((message) => [message.task_id, message.related_task_id].filter((id): id is string => Boolean(id))),
  ])];
  const taskPanelTasks = taskPanelIDs.flatMap((id) => {
    const summary = conversationTaskItems.find((item) => item.id === id) ?? recentConversationTaskItems.find((item) => item.id === id) ?? allTasksByID.get(id);
    return summary && !isResourceNoLongerAccessible(taskQueries[taskIDs.indexOf(id)]?.error) ? [summary] : [];
  }).sort((left, right) => Date.parse(allTasksByID.get(right.id)?.updated_at ?? right.updated_at) -
    Date.parse(allTasksByID.get(left.id)?.updated_at ?? left.updated_at) || right.id.localeCompare(left.id));
  const taskProgressReadError = taskQueries.some((query, index) => query.isError && currentConversationTaskIDs.has(taskIDs[index] ?? ''));
  const visibleTaskPanelTasks = taskPanelTasks.filter((item) => item.conversation_id === conversationID);
  const taskPanelGroups: TaskPanelGroup[] = [
    { id: 'attention', title: '待你处理', tasks: visibleTaskPanelTasks.filter((item) => (allTasksByID.get(item.id)?.status ?? item.status) === 'waiting_user') },
    { id: 'running', title: '进行中', tasks: visibleTaskPanelTasks.filter((item) => ['queued', 'running', 'pausing', 'cancelling'].includes(allTasksByID.get(item.id)?.status ?? item.status)) },
    { id: 'paused', title: '已暂停', tasks: visibleTaskPanelTasks.filter((item) => (allTasksByID.get(item.id)?.status ?? item.status) === 'paused') },
    { id: 'recent', title: '最近结束', tasks: visibleTaskPanelTasks.filter((item) => terminalTaskStatuses.has(allTasksByID.get(item.id)?.status ?? item.status)) },
  ];
  const taskPanelRows = taskPanelGroups.flatMap<TaskPanelRow>((group) => {
    if (!group.tasks.length) return [];
    if (group.id === 'recent' && !taskPanelRecentOpen) return [{ key: 'group-recent', group, task: null, first: true }];
    return group.tasks.map((item, index) => ({ key: `task-${item.id}`, group, task: item, first: index === 0 }));
  });
  const conversationFiles: ConversationFileItem[] = [
    ...conversationMessageItems.flatMap((item) => (item.attachment_ids ?? []).map((attachmentID, index) => ({
      kind: 'attachment' as const, key: `attachment:${item.id}:${attachmentID}`, attachmentID,
      messageID: item.id, index: index + 1, createdAt: item.created_at,
    }))),
    ...tasks.flatMap((item) => item.artifacts.filter(hasCompleteVerifiedArtifactProof).map((artifact) => ({ kind: 'artifact' as const,
      key: `artifact:${item.id}:${artifact.id}`, taskID: item.id, taskTitle: item.objective || '未命名任务', artifact }))),
  ];
  const pendingTaskCount = !conversationContentAccessRevoked && conversationTasks.isSuccess && !taskProgressReadError ? conversationTasks.data.pending_count : null;
  const resultsAwaitingMessages = tasks.some((task) => task.result_message_pending || (task.result_message_published &&
    !conversationMessageItems.some((message) => message.sources?.some((source) => source.task_id === task.id &&
      source.task_revision === task.assistant_message?.task_revision && source.event_sequence === task.assistant_message?.result_event_sequence))));
  useEffect(() => {
    if (!conversationID || !resultsAwaitingMessages) return;
    const timer = window.setInterval(() => { void client.invalidateQueries({ queryKey: ['conversation-messages', conversationID] }); }, 1000);
    return () => window.clearInterval(timer);
  }, [client, conversationID, resultsAwaitingMessages]);
  const tasksByID = new Map(tasks.map((task) => [task.id, task]));
  const activePanelTasks = tasks.filter((task) => !terminalTaskStatuses.has(task.status));
  const selectedActiveTask = activePanelTasks.find((task) => task.id === taskDetailTaskID);
  const streamedTasks = [
    ...(selectedActiveTask ? [selectedActiveTask] : []),
    ...activePanelTasks.filter((task) => task.id !== selectedActiveTask?.id),
  ].slice(0, maxTaskEventStreams);
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
    ...taskQueries.map((query) => ({ failed: query.isError, error: query.error, retry: () => query.refetch() })),
  ].filter((item) => item.failed && isTransientNetworkError(item.error));
  const showConsolidatedNetworkError = networkReadFailures.length > 1;
  const networkReadFailuresRef = useRef(networkReadFailures);
  useEffect(() => { networkReadFailuresRef.current = networkReadFailures; }, [networkReadFailures]);
  const retryNetworkReads = useCallback(() => { void Promise.all(networkReadFailuresRef.current.map((item) => item.retry())); }, []);
  const networkErrorToastID = useRef<number | null>(null);
  useEffect(() => {
    if (!showConsolidatedNetworkError) {
      if (networkErrorToastID.current !== null) dismiss(networkErrorToastID.current);
      networkErrorToastID.current = null;
      return;
    }
    if (networkErrorToastID.current === null) {
      networkErrorToastID.current = notify('网络异常，部分对话数据暂时无法读取。', 'error', { label: '重试', onClick: retryNetworkReads });
    }
  }, [dismiss, notify, retryNetworkReads, showConsolidatedNetworkError]);
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
  const workspaceItems = (workspaces.data?.items ?? []).filter((workspace) => workspace.state === 'ready' && !workspace.is_read_only);
  const selectedHost = hostItems.find((host) => host.id === preferredHostID);
  const hostIsReady = selectedHost?.state === 'running' && selectedHost.control_state === 'ai';
  const computerHostOptions: ComputerHostOption[] = [
    ...hostItems.map((host) => ({ id: host.id, name: host.name || host.id, url: host.url, state: host.state, owned: true, permissions: [] })),
    ...(sharedHosts.data?.items ?? []).map((host) => ({
      id: host.id, name: host.name || host.id, url: host.url, state: host.state, owned: false, permissions: host.permissions,
    })),
  ];
  const computerHostSelection = computerHostID ?? hostSelection ?? preferences.data?.default_host_id ?? null;
  const selectedComputerHost = computerHostSelection === null ? computerHostOptions[0]
    : computerHostOptions.find((host) => host.id === computerHostSelection);
  const selectedComputerTargets = selectedComputerHost ? hostEmbedTargetPlan(selectedComputerHost) : [];
  const selectedComputerInitialTarget = hostEmbedInitialTarget(selectedComputerTargets);
  const requestComputerEmbed = async () => {
    if (!selectedComputerHost || !conversationID || !selectedComputerInitialTarget || computerConnecting) {
      if (!conversationID) setComputerPanelError('先发送一条消息创建当前旁聊，再连接云端电脑。');
      else if (!selectedComputerHost) setComputerPanelError('请选择一台有权限访问的云端电脑。');
      else if (!selectedComputerInitialTarget) setComputerPanelError('当前账号没有此电脑的可用工作台权限。');
      return;
    }
    const generation = ++computerRequestGeneration.current;
    setComputerConnecting(true);
    setComputerPanelError('');
    setComputerEmbed(null);
    setComputerControlExpanded(false);
    try {
      const issued = await portalApi.issueHostEmbedTicket(selectedComputerHost.id, {
        conversation_id: conversationID, target: selectedComputerInitialTarget, targets: selectedComputerTargets,
      });
      if (generation !== computerRequestGeneration.current) return;
      const hostURL = new URL(selectedComputerHost.url);
      const exchangeURL = new URL(issued.exchange_url);
      const allowedTargets = new Set(selectedComputerTargets);
      const validTargets = issued.targets.length > 0 && issued.targets.every((target) => allowedTargets.has(target));
      if (hostURL.username || hostURL.password || exchangeURL.origin !== hostURL.origin ||
        exchangeURL.pathname !== '/__ch/embed-auth' || exchangeURL.search || exchangeURL.hash ||
        exchangeURL.username || exchangeURL.password || !/^[\w-]{43}$/.test(issued.ticket) ||
        !/^[\w-]{32}$/.test(issued.embed_nonce) || !validTargets || !issued.targets.includes(selectedComputerInitialTarget)) {
        throw new Error('电脑连接凭据或授权范围无效，请刷新后重试。');
      }
      setComputerHostID(selectedComputerHost.id);
      setComputerEmbed({
        conversationID, hostID: selectedComputerHost.id, hostOrigin: hostURL.origin,
        exchangeURL: exchangeURL.toString(), nonce: issued.embed_nonce, ticket: issued.ticket,
        targets: issued.targets, status: 'submitting',
      });
    } catch (error) {
      if (generation === computerRequestGeneration.current) setComputerPanelError(errorDetails(error).message || '连接云端电脑失败，请重试。');
    } finally {
      if (generation === computerRequestGeneration.current) setComputerConnecting(false);
    }
  };
  async function openTaskObservation(task: Task, host: Host) {
    if (taskObservationRequest) return;
    const popup = window.open('about:blank', '_blank');
    if (!popup) {
      setTaskObservationError({ taskID: task.id, message: '浏览器阻止了新窗口；允许弹出窗口后重试。' });
      return;
    }
    popup.opener = null;
    setTaskObservationRequest(true);
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
      setTaskObservationRequest(false);
      setOpeningTaskID('');
    }
  }
  const displayMessages = mergeTaskAssistantMessages(conversationMessageItems, tasks);
  const startedTaskMessageIDs = new Set<string>();
  const markedStartedTasks = new Set<string>();
  for (const message of [...displayMessages].reverse()) {
    const taskID = message.task_id ?? message.related_task_id;
    if (message.role !== 'user' || !taskID || message.routing_status === 'failed' || message.duplicate_of_message_id || markedStartedTasks.has(taskID)) continue;
    const task = tasksByID.get(taskID);
    if (task?.current_revision_started !== true || (task.current_revision !== undefined && task.desired_revision !== undefined
      && task.current_revision !== task.desired_revision)) continue;
    markedStartedTasks.add(taskID);
    startedTaskMessageIDs.add(message.id);
  }
  const latestDisplayMessageContent = displayMessages.at(-1)?.content;
  const loadOlderMessages = async () => {
    const view = conversationView.current;
    if (!view || !messages.hasNextPage || messages.isFetchingNextPage) return;
    olderMessagesAnchor.current = { height: view.scrollHeight, top: view.scrollTop };
    followConversation.current = false;
    setShowJumpToLatest(true);
    const result = await messages.fetchNextPage();
    if (result.isFetchNextPageError) olderMessagesAnchor.current = null;
    else setHistoryPageLoadVersion((version) => version + 1);
  };
  const submissionModelID = readyModels.some((item) => item.id === effectiveModelID) ? effectiveModelID : '';
  const taskActionKey = (scope: string, fingerprint: string) => {
    const previous = pendingTaskActions.get(scope);
    if (previous?.fingerprint === fingerprint) return previous.key;
    const key = newIdempotencyKey();
    setPendingTaskActions((current) => new Map(current).set(scope, { fingerprint, key }));
    return key;
  };
  const clearTaskActionKey = (scope: string, key: string) => {
    setPendingTaskActions((current) => {
      if (current.get(scope)?.key !== key) return current;
      const next = new Map(current);
      next.delete(scope);
      return next;
    });
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
        const localDraftID = `local:${message.conversationCreateKey}`;
        const draftScope = assistantComposerDraftScope(message.serviceInstanceID!, message.actorID);
        const localDraftIdentity = JSON.stringify([draftScope, localDraftID]);
        let canSelectCreatedConversation = true;
        if (localConversationKeyRef.current === message.conversationCreateKey) canSelectCreatedConversation = await persistActiveComposerDraft();
        try {
          const localDraft = canSelectCreatedConversation ? await assistantDraftStorage.get(draftScope, localDraftID) : null;
          if (localDraft) {
            const latest = composerDraftSnapshots.current.get(localDraftIdentity)?.draft ?? localDraft;
            const migratedDraft = { ...latest, id: targetID, conversationID: targetID };
            migratedComposerDrafts.current.set(localDraftIdentity, targetID);
            composerDraftSnapshots.current.set(JSON.stringify([draftScope, targetID]), { scope: draftScope, draft: migratedDraft });
            await persistComposerDraft(draftScope, migratedDraft);
            await assistantDraftStorage.remove(draftScope, localDraftID);
            composerDraftSnapshots.current.delete(localDraftIdentity);
            client.setQueryData<AssistantComposerDraft[]>(['assistant-composer-drafts', draftScope], (previous) =>
              (previous ?? []).filter((item) => item.id !== localDraftID));
          }
        } catch (error) {
          migratedComposerDrafts.current.delete(localDraftIdentity);
          canSelectCreatedConversation = false;
          setComposerDraftError(errorDetails(error).message);
        }
        if (canSelectCreatedConversation && localConversationKeyRef.current === message.conversationCreateKey && activeOutboxScope.current === scope) {
          newConversationKey.current = null;
          activeDraftLoad.current = JSON.stringify([draftScope, targetID]);
          setHydratedComposerDraftKey(activeDraftLoad.current);
          setConversationID(targetID);
        }
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
          client.setQueryData<ConversationMessagesData>(['conversation-messages', targetID], (previous) => appendConversationReceipt(previous, receipt));
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
        client.setQueryData<ConversationMessagesData>(['conversation-messages', targetID], (previous) => appendConversationReceipt(previous, receipt));
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
    mutationFn: async (target: { conversationID: string; messageID: string; taskID?: string; resultReview?: boolean }) => {
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
      if (target.resultReview && target.taskID) {
        setRetryAcceptedResultReviewTaskIDs((current) => new Set(current).add(target.taskID!));
        window.setTimeout(() => setRetryAcceptedResultReviewTaskIDs((current) => {
          if (!current.has(target.taskID!)) return current;
          const next = new Set(current);
          next.delete(target.taskID!);
          return next;
        }), 30_000);
      } else {
        setRetryAcceptedMessageIDs((current) => new Set(current).add(target.messageID));
        window.setTimeout(() => setRetryAcceptedMessageIDs((current) => {
          if (!current.has(target.messageID)) return current;
          const next = new Set(current);
          next.delete(target.messageID);
          return next;
        }), 30_000);
      }
      await client.invalidateQueries({ queryKey: ['conversation-messages', target.conversationID] });
      if (target.taskID) {
        await client.invalidateQueries({ queryKey: ['task', target.taskID] });
        await client.invalidateQueries({ queryKey: ['conversation-tasks', target.conversationID] });
        await client.invalidateQueries({ queryKey: ['assistant-account-tasks'] });
      }
    },
    onError: (error, target) => {
      if (target.taskID) setRetryAcceptedResultReviewTaskIDs((current) => {
        if (!current.has(target.taskID!)) return current;
        const next = new Set(current);
        next.delete(target.taskID!);
        return next;
      });
      setRetryAcceptedMessageIDs((current) => {
        if (!current.has(target.messageID)) return current;
        const next = new Set(current);
        next.delete(target.messageID);
        return next;
      });
      notify(errorDetails(error).message, 'error');
    },
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
    const identity = activeComposerDraftIdentity.current;
    if (!identity) return;
    const cancelledIDs = new Set<string>();
    for (const key of preparationKeysForIdentity(identity)) {
      attachmentPreparationGenerations.current.set(key, (attachmentPreparationGenerations.current.get(key) ?? 0) + 1);
      for (const controller of pdfPreparationControllers.current.get(key) ?? []) controller.abort();
      pdfPreparationControllers.current.delete(key);
      for (const id of preparingAttachmentIDsByDraft.current.get(key) ?? []) cancelledIDs.add(id);
      preparingAttachmentIDsByDraft.current.delete(key);
      attachmentPreparationCounts.current.set(key, 0);
    }
    for (const id of cancelledIDs) preparingAttachmentIDs.current.delete(id);
    setPreparingAttachmentIDsForRender((current) => {
      const next = new Set(current);
      for (const id of cancelledIDs) next.delete(id);
      return next;
    });
    preparingAttachmentCountRef.current = 0;
    setPreparingAttachmentCount(0);
    setComposerAttachments((current) => current.map((item) => item.needsPreparation
      ? { ...item, preparationCancelled: true } : item));
    const current = latestComposerDraft.current;
    if (current?.identity === identity) {
      const draft = { ...current.draft,
        attachments: current.draft.attachments.map((item) => item.needsPreparation ? { ...item, preparationCancelled: true } : item),
        updatedAt: Date.now(),
      };
      latestComposerDraft.current = { ...current, draft };
      composerDraftSnapshots.current.set(identity, { scope: current.scope, draft });
      void persistComposerDraft(current.scope, draft).catch(() => undefined);
    }
  };

  const prepareComposerAttachmentItems = useCallback(async (items: ComposerAttachment[], scope: string | null, draftID: string | null) => {
    const pending = items.filter((item) => item.needsPreparation && !preparingAttachmentIDs.current.has(item.id));
    if (!pending.length) return;
    const identity = scope && draftID ? JSON.stringify([scope, draftID]) : null;
    const preparationKey = identity ?? `unscoped:${draftID ?? 'composer'}`;
    const generation = attachmentPreparationGenerations.current.get(preparationKey) ?? 0;
    const controller = new AbortController();
    const controllers = pdfPreparationControllers.current.get(preparationKey) ?? new Set<AbortController>();
    controllers.add(controller);
    pdfPreparationControllers.current.set(preparationKey, controllers);
    const ids = preparingAttachmentIDsByDraft.current.get(preparationKey) ?? new Set<string>();
    for (const item of pending) { preparingAttachmentIDs.current.add(item.id); ids.add(item.id); }
    preparingAttachmentIDsByDraft.current.set(preparationKey, ids);
    setPreparingAttachmentIDsForRender((current) => new Set([...current, ...pending.map((item) => item.id)]));
    const currentCount = (attachmentPreparationCounts.current.get(preparationKey) ?? 0) + pending.length;
    attachmentPreparationCounts.current.set(preparationKey, currentCount);
    if (identity && activeComposerDraftIdentity.current === identity) {
      preparingAttachmentCountRef.current = currentCount;
      setPreparingAttachmentCount(currentCount);
    }
    const accepted = new Map<string, ComposerAttachment>();
    const rejected = new Set<string>();
    try {
      for (const item of pending) {
        if (controller.signal.aborted || generation !== (attachmentPreparationGenerations.current.get(preparationKey) ?? 0)) break;
        const file = item.file;
        const mediaType = assistantFileMediaType(file);
        const unsafeName = !file.name || file.name === '.' || file.name === '..' || [...file.name].length > 255 ||
          file.name.includes('/') || file.name.includes('\\') || [...file.name].some((character) => character.charCodeAt(0) < 32 || character.charCodeAt(0) === 127);
        if (unsafeName) {
          notify('附件文件名无效。', 'error');
          rejected.add(item.id);
          continue;
        }
        let preparedFile = file;
        let displayName = item.displayName;
        if (mediaType === 'application/pdf') {
          try {
            const extractedText = await extractAssistantPDFText(file, controller.signal);
            if (controller.signal.aborted || generation !== (attachmentPreparationGenerations.current.get(preparationKey) ?? 0)) break;
            const safeDerivedName = `${Array.from(file.name).slice(0, 250).join('')}.txt`;
            preparedFile = new File([extractedText], safeDerivedName, { type: 'text/plain', lastModified: file.lastModified });
            displayName = `${file.name} · 已提取文字`;
          } catch (error) {
            if (generation === (attachmentPreparationGenerations.current.get(preparationKey) ?? 0) && !(error instanceof Error && error.name === 'AbortError')) {
              notify(`${file.name}：${error instanceof Error ? error.message : '无法读取 PDF。'}`, 'error');
            }
            continue;
          }
        }
        const fileError = assistantFileError(preparedFile, assistantFileMediaType(preparedFile));
        if (fileError) {
          notify(`${file.name}：${fileError}`, 'error');
          rejected.add(item.id);
          continue;
        }
        accepted.set(item.id, { ...item, needsPreparation: undefined, preparationCancelled: undefined, file: preparedFile, ...(displayName ? { displayName } : {}) });
      }
    } finally {
      controllers.delete(controller);
      if (!controllers.size) pdfPreparationControllers.current.delete(preparationKey);
      const activeIDs = preparingAttachmentIDsByDraft.current.get(preparationKey);
      for (const item of pending) { preparingAttachmentIDs.current.delete(item.id); activeIDs?.delete(item.id); }
      setPreparingAttachmentIDsForRender((current) => {
        const next = new Set(current);
        for (const item of pending) next.delete(item.id);
        return next;
      });
      if (activeIDs && !activeIDs.size) preparingAttachmentIDsByDraft.current.delete(preparationKey);
      const nextCount = Math.max(0, (attachmentPreparationCounts.current.get(preparationKey) ?? 0) - pending.length);
      attachmentPreparationCounts.current.set(preparationKey, nextCount);
      const activeIdentity = activeComposerDraftIdentity.current;
      if (activeIdentity && preparationKeysForIdentity(activeIdentity).includes(preparationKey)) {
        const total = preparationKeysForIdentity(activeIdentity)
          .reduce((count, key) => count + (attachmentPreparationCounts.current.get(key) ?? 0), 0);
        preparingAttachmentCountRef.current = total;
        setPreparingAttachmentCount(total);
      }
    }
    if (generation === (attachmentPreparationGenerations.current.get(preparationKey) ?? 0)) {
      let effectiveDraftID = (identity && migratedComposerDrafts.current.get(identity)) || draftID;
      const targetLocalKey = effectiveDraftID?.startsWith('local:') ? effectiveDraftID.slice('local:'.length) : null;
      const isCurrent = effectiveDraftID ? targetLocalKey ? localConversationKeyRef.current === targetLocalKey : selectedConversation.current === effectiveDraftID : false;
      if (isCurrent && (accepted.size || rejected.size)) {
        setComposerAttachments((current) => current.filter((item) => !rejected.has(item.id)).map((item) => accepted.get(item.id) ?? item));
      }
      if (scope && effectiveDraftID) {
        try {
          let stored = await assistantDraftStorage.get(scope, effectiveDraftID);
          const migratedID = identity && migratedComposerDrafts.current.get(identity);
          if (migratedID && migratedID !== effectiveDraftID) {
            effectiveDraftID = migratedID;
            stored = await assistantDraftStorage.get(scope, effectiveDraftID);
            if (selectedConversation.current === effectiveDraftID && (accepted.size || rejected.size)) {
              setComposerAttachments((current) => current.filter((item) => !rejected.has(item.id)).map((item) => accepted.get(item.id) ?? item));
            }
          }
          const effectiveIdentity = JSON.stringify([scope, effectiveDraftID]);
          const snapshot = effectiveIdentity ? composerDraftSnapshots.current.get(effectiveIdentity) : undefined;
          const base = snapshot?.scope === scope ? snapshot.draft : stored;
          if (base) {
            const attachments = base.attachments.flatMap((item) => {
              if (rejected.has(item.id)) return [];
              const ready = accepted.get(item.id);
              if (!ready) return [item];
              return [{ ...item, name: ready.file.name, ...(ready.displayName ? { displayName: ready.displayName } : {}),
                mediaType: assistantFileMediaType(ready.file), blob: ready.file, needsPreparation: undefined, preparationCancelled: undefined }];
            });
            const draft = { ...base, attachments };
            if (effectiveIdentity) {
              composerDraftSnapshots.current.set(effectiveIdentity, { scope, draft });
              if (latestComposerDraft.current?.identity === effectiveIdentity) latestComposerDraft.current = { scope, identity: effectiveIdentity, draft };
            }
            await persistComposerDraft(scope, draft);
          }
        } catch (error) { setComposerDraftError(errorDetails(error).message); }
      }
    }
  }, [notify, persistComposerDraft, preparationKeysForIdentity]);

  const addComposerFiles = async (fileList: FileList | null) => {
    if (!fileList) return;
    composerEditRevision.current += 1;
    if (!conversationID && !selectedLocalDraftKey) ensureLocalConversationKey();
    const incoming = Array.from(fileList);
    const available = Math.max(0, 16 - composerAttachments.length - preparingAttachmentCountRef.current);
    const selected = incoming.slice(0, available);
    const localKey = selectedLocalDraftKey ?? localConversationKeyRef.current ?? newConversationKey.current;
    const draftID = conversationID ?? (localKey ? `local:${localKey}` : null);
    const scope = composerDraftScope;
    const staged = selected.map((file) => {
      const id = newIdempotencyKey();
      return { id, file, objectID: `chat-${id}`, objectKey: newIdempotencyKey(), uploadKey: newIdempotencyKey(),
        commitKey: newIdempotencyKey(), needsPreparation: true } satisfies ComposerAttachment;
    });
    if (staged.length && draftID) {
      const targetIdentity = scope ? JSON.stringify([scope, draftID]) : null;
      const active = latestComposerDraft.current;
      const base = targetIdentity && active?.identity === targetIdentity ? active.draft : {
        id: draftID, conversationID: draftID.startsWith('local:') ? null : draftID, content, modelID,
        hostID: hostSelection ?? '', mcpConnectorIDs: [...mcpConnectorIDs],
        attachments: composerAttachments.map((item) => ({ id: item.id, name: item.file.name, mediaType: assistantFileMediaType(item.file),
          blob: item.file, objectID: item.objectID, objectKey: item.objectKey, uploadKey: item.uploadKey, commitKey: item.commitKey,
          ...(item.attachmentID ? { attachmentID: item.attachmentID } : {}), ...(item.needsPreparation ? { needsPreparation: true } : {}),
          ...(item.preparationCancelled ? { preparationCancelled: true } : {}) })),
        updatedAt: 0,
      } satisfies AssistantComposerDraft;
      const draft = { ...base, attachments: [...base.attachments, ...staged.map((item) => ({
        id: item.id, name: item.file.name, mediaType: assistantFileMediaType(item.file), blob: item.file,
        objectID: item.objectID, objectKey: item.objectKey, uploadKey: item.uploadKey, commitKey: item.commitKey,
        needsPreparation: true,
      }))] };
      if (scope && targetIdentity) {
        composerDraftSnapshots.current.set(targetIdentity, { scope, draft });
        if (!active || active.identity === targetIdentity) latestComposerDraft.current = { scope, identity: targetIdentity, draft };
      }
      const isCurrent = draftID.startsWith('local:') ? localConversationKeyRef.current === draftID.slice('local:'.length)
        : selectedConversation.current === draftID;
      if (isCurrent) setComposerAttachments((current) => [...current, ...staged]);
      if (scope) {
        try {
          await persistComposerDraft(scope, draft);
        } catch { /* the resilient draft store keeps the staged file in memory */ }
      }
      void prepareComposerAttachmentItems(staged, scope, draftID);
    }
    if (incoming.length > available) notify('每条消息最多添加 16 个附件。', 'error');
  };

  const previewOfficeAttachment = async (item: ComposerAttachment) => {
    const mediaType = assistantFileMediaType(item.file);
    if (!assistantOfficeAttachmentTypes.has(mediaType)) return;
    setOfficePreviews((current) => ({ ...current, [item.id]: { status: 'loading' } }));
    try {
      const text = await portalApi.previewPersonalOfficeText(item.file, mediaType);
      setOfficePreviews((current) => ({ ...current, [item.id]: { status: 'ready', text } }));
    } catch (error) {
      setOfficePreviews((current) => {
        const next = { ...current };
        delete next[item.id];
        return next;
      });
      notify(`${item.file.name} 预览失败：${errorDetails(error).message}`, 'error');
    }
  };

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (savingMessageRef.current || preparingAttachmentCountRef.current > 0 || composerAttachments.some((item) => item.needsPreparation)) return;
    const text = content;
    if (!text.trim() && !composerAttachments.length) return;
    const effectiveText = text.trim() ? text : '请阅读并处理我附加的文件。';
    const timeZone = browserIanaTimeZone();
    if (new TextEncoder().encode(effectiveText).length > maxMessageBytes) {
      notify('单条消息不能超过 8 MiB；完整内容仍保留在输入框中。', 'error');
      return;
    }
    if (models.isSuccess && !models.isStale && models.data.configuration_status === 'not_configured') {
      return;
    }
    const id = newIdempotencyKey();
    if (!conversationID && !newConversationKey.current) newConversationKey.current = selectedLocalDraftKey ?? newIdempotencyKey();
    const createKey = conversationID ? undefined : selectedLocalDraftKey ?? newConversationKey.current ?? undefined;
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
          ...(timeZone ? { time_zone: timeZone } : {}),
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
      const serverPredecessor = latestConversationClientMessageID(conversationMessageItems);
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

  const ensureLocalConversationKey = () => {
    if (conversationID) return null;
    const key = selectedLocalDraftKey ?? newConversationKey.current ?? newIdempotencyKey();
    newConversationKey.current = key;
    if (!selectedLocalDraftKey) {
      const localID = `local:${key}`;
      setConversationID(localID);
      localConversationKeyRef.current = key;
      if (composerDraftScope) {
        const identity = JSON.stringify([composerDraftScope, localID]);
        activeDraftLoad.current = identity;
        setHydratedComposerDraftKey(identity);
      }
    }
    return key;
  };

  const startNew = async () => {
    const generation = ++composerSwitchGeneration.current;
    if (!composerDraftScope && (content || composerAttachments.length)) {
      notify('账号或服务身份尚未确认；当前草稿仍留在此页面，请待服务恢复后再切换。', 'error');
      return;
    }
    const revision = composerEditRevision.current;
    const saved = await persistActiveComposerDraft();
    if (generation !== composerSwitchGeneration.current) return;
    if (!saved) {
      notify('当前草稿尚未可靠保存，已留在此会话，请重试。', 'error');
      return;
    }
    if (revision !== composerEditRevision.current) {
      notify('输入已变化，请再次选择新建旁聊。', 'error');
      return;
    }
    const key = newIdempotencyKey();
    const id = `local:${key}`;
    newConversationKey.current = key;
    localConversationKeyRef.current = key;
    setConversationID(id);
    setModelID('');
    setHostSelection(null);
    setWorkspaceSelection(null);
    setMcpConnectorIDs([]);
    setContent('');
    setComposerAttachments([]);
    setOfficePreviews({});
    if (composerDraftScope) restoreComposerDraft(null, composerDraftScope, id);
    if (recentConversationKey) {
      writeRecentConversation(recentConversationKey, null);
      client.setQueryData(['assistant-recent-conversation', recentConversationKey], null);
    }
  };

  const selectConversationID = async (id: string) => {
    const generation = ++composerSwitchGeneration.current;
    if (!composerDraftScope && (content || composerAttachments.length)) {
      notify('账号或服务身份尚未确认；当前草稿仍留在此页面，请待服务恢复后再切换。', 'error');
      return;
    }
    const revision = composerEditRevision.current;
    const saved = await persistActiveComposerDraft();
    if (generation !== composerSwitchGeneration.current) return;
    if (!saved) {
      notify('当前草稿尚未可靠保存，已留在此会话，请重试。', 'error');
      return;
    }
    if (composerDraftScope) {
      try { await assistantDraftStorage.get(composerDraftScope, id); }
      catch (error) {
        if (generation !== composerSwitchGeneration.current) return;
        setComposerDraftError(errorDetails(error).message);
        notify('无法恢复目标会话草稿；当前输入仍已保留。', 'error');
        return;
      }
    }
    if (generation !== composerSwitchGeneration.current) return;
    if (revision !== composerEditRevision.current) {
      notify('输入已变化，请再次选择会话。', 'error');
      return;
    }
    setConversationSelection({ scope: conversationScope, id });
    if (recentConversationKey && !id.startsWith('local:')) {
      writeRecentConversation(recentConversationKey, id);
      client.setQueryData(['assistant-recent-conversation', recentConversationKey], id);
    }
  };

  const selectLocalDraft = (draft: AssistantComposerDraft) => { void selectConversationID(draft.id); };

  const submitTaskSupplement = (event: FormEvent<HTMLFormElement>, task: Task) => {
    event.preventDefault();
    const supplementalText = (taskSupplementDrafts[task.id] ?? '').trim();
    const needsWorkspaceTarget = task.waiting_reason === 'workspace_unavailable' && !task.workspace_id;
    const selectedWorkspaceID = needsWorkspaceTarget ? taskWorkspaceSelections[task.id] ?? '' : '';
    const text = supplementalText || (needsWorkspaceTarget && selectedWorkspaceID
      ? '请继续当前任务，并将结果保存到我选择的 Workspace。' : '');
    if (savingMessageRef.current || !text || (needsWorkspaceTarget && !selectedWorkspaceID)) return;
    if (new TextEncoder().encode(text).length > maxMessageBytes) {
      notify('单条消息不能超过 8 MiB；完整内容仍保留在输入框中。', 'error');
      return;
    }
    const id = newIdempotencyKey();
    savingMessageRef.current = true;
    setSavingMessage(true);
    const previous = pendingMessageSave.current.then(async () => {
      const targetMessages = task.conversation_id === conversationID && messages.data
        ? conversationMessageItems : (await portalApi.conversationMessages(task.conversation_id)).items;
      const pending: PendingConversationMessage = {
        actorID: user?.id ?? '',
        request: { client_message_id: id, content: text, reference_task_id: task.id,
          ...(selectedWorkspaceID ? { workspace_id: selectedWorkspaceID, change_task_target: true } : {}) },
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

  const showMoreSideConversations = async () => {
    const targetCount = Math.min(Math.max(sideConversations.length, localComposerDrafts.length), visibleSideConversationCount) + 5;
    let result = conversations;
    let loadedSideCount = sideConversations.length;
    while (loadedSideCount < Math.min(targetCount, sideConversations.length + 5) && result.hasNextPage) {
      result = await conversations.fetchNextPage();
      loadedSideCount = new Set((result.data?.pages.flatMap((page) => page.items) ?? [])
        .filter((item) => item.id !== mainConversationID).map((item) => item.id)).size;
      if (result.isFetchNextPageError) break;
    }
    setVisibleSideConversationCount(targetCount);
  };
  const renderLocalDraftItem = (draft: AssistantComposerDraft) => {
    const title = draft.content.trim().split('\n')[0]?.slice(0, 42) || '新旁聊草稿';
    return <div className="conversation-row" key={draft.id}>
      <button type="button" className={`conversation-item ${selectedLocalDraftKey && draft.id === `local:${selectedLocalDraftKey}` ? 'is-selected' : ''}`}
        aria-label={`恢复草稿：${title}`} onClick={() => selectLocalDraft(draft)}>
        <strong>{title}</strong><small>未发送草稿 · {new Date(draft.updatedAt).toLocaleString()}</small>
      </button>
    </div>;
  };
  const renderCachedMainConversation = () => mainConversationID ? <div className="conversation-row conversation-row-main" key={mainConversationID}>
    <button type="button" className={`conversation-item ${conversationID === mainConversationID ? 'is-selected' : ''}`}
      onClick={() => selectConversationID(mainConversationID)}>
      <strong>主会话</strong><small>最近记录</small>
    </button>
  </div> : null;

  const finishedTaskIDs = tasks.filter((task) => terminalTaskStatuses.has(task.status)).map((task) => task.id).join(',');
  useEffect(() => {
    olderMessagesAnchor.current = null;
    followConversation.current = true;
    postComputerVisibility(false);
    computerRequestGeneration.current += 1;
  }, [conversationID, postComputerVisibility]);
  useEffect(() => {
    const session = computerEmbed;
    if (!session || session.status !== 'submitting' || !session.ticket) return;
    const ticket = session.ticket;
    const frame = computerFrame.current;
    if (!frame || submittedEmbedNonce.current === session.nonce) return;
    submittedEmbedNonce.current = session.nonce;
    const form = document.createElement('form');
    form.method = 'post';
    form.enctype = 'application/x-www-form-urlencoded';
    form.action = session.exchangeURL;
    form.target = frame.name;
    form.hidden = true;
    const fields: [string, string][] = [['ticket', ticket], ['embed_nonce', session.nonce]];
    for (const [name, value] of fields) {
      const input = document.createElement('input');
      input.type = 'hidden';
      input.name = name;
      input.value = value;
      form.append(input);
    }
    document.body.append(form);
    try {
      form.submit();
      window.setTimeout(() => setComputerEmbed((current) => current?.nonce === session.nonce
        ? { ...current, ticket: undefined, status: 'loading' } : current), 0);
    } catch {
      submittedEmbedNonce.current = null;
      window.setTimeout(() => setComputerEmbed((current) => current?.nonce === session.nonce
        ? { ...current, ticket: undefined, status: 'error', error: '无法提交电脑连接请求。' } : current), 0);
    } finally {
      form.remove();
    }
  }, [computerEmbed]);
  useEffect(() => {
    const session = computerEmbed;
    if (!session) return;
    const send = (type: 'init' | 'visibility', visible: boolean) => {
      computerFrame.current?.contentWindow?.postMessage({
        channel: 'morde.workbench.embed', version: 1, type, nonce: session.nonce,
        ...(type === 'init' ? { conversation_id: session.conversationID } : {}), visible,
      }, session.hostOrigin);
    };
    const onMessage = (event: MessageEvent<unknown>) => {
      if (event.origin !== session.hostOrigin || event.source !== computerFrame.current?.contentWindow ||
        !event.data || typeof event.data !== 'object') return;
      const message = event.data as Record<string, unknown>;
      if (message.channel !== 'morde.workbench.embed' || message.version !== 1 || message.nonce !== session.nonce) return;
      if (message.type === 'ready') {
        initializedEmbedNonce.current = session.nonce;
        send('init', taskPanelOpen && taskPanelTab === 'computer');
      } else if (message.type === 'state' && message.conversation_id === session.conversationID &&
        message.host_id === session.hostID && message.workbench_ready === true) {
        if (message.control_state === 'user' && message.control_lease_active === true && message.control_lease_held_by_me === true) {
          setComputerControlExpanded(true);
        } else if (message.control_state === 'ai' && message.control_lease_active === false && message.control_lease_held_by_me === false) {
          setComputerControlExpanded(false);
        }
        setComputerEmbed((current) => current?.nonce === session.nonce ? { ...current, status: 'ready' } : current);
      }
    };
    window.addEventListener('message', onMessage);
    if (initializedEmbedNonce.current === session.nonce) send('visibility', taskPanelOpen && taskPanelTab === 'computer');
    return () => window.removeEventListener('message', onMessage);
  }, [computerEmbed, taskPanelOpen, taskPanelTab]);
  useEffect(() => {
    if (!computerEmbed || computerEmbed.status !== 'loading') return;
    const nonce = computerEmbed.nonce;
    const timer = window.setTimeout(() => setComputerEmbed((current) => current?.nonce === nonce && current.status === 'loading'
      ? { ...current, status: 'error', error: '工作台没有在预期时间内就绪，请重试或在独立窗口打开。' } : current), 30_000);
    return () => window.clearTimeout(timer);
  }, [computerEmbed]);
  useEffect(() => {
    if (!taskPanelOpen) return;
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return;
      postComputerVisibility(false);
      computerRequestGeneration.current += 1;
      setComputerConnecting(false);
      setComputerEmbed(null);
      setComputerControlExpanded(false);
      setTaskPanelOpen(false);
      setTaskDetailTaskID(null);
      window.requestAnimationFrame(() => taskPanelToggle.current?.focus());
    };
    window.addEventListener('keydown', closeOnEscape);
    return () => window.removeEventListener('keydown', closeOnEscape);
  }, [postComputerVisibility, taskPanelOpen]);
  useLayoutEffect(() => {
    const view = conversationView.current;
    const anchor = olderMessagesAnchor.current;
    if (view && anchor) {
      view.scrollTop = anchor.top + view.scrollHeight - anchor.height;
      olderMessagesAnchor.current = null;
      followConversation.current = false;
      setShowJumpToLatest(true);
      return;
    }
    if (view && followConversation.current) {
      view.scrollTop = view.scrollHeight;
      setShowJumpToLatest(false);
    }
  }, [conversationID, displayMessages.length, latestDisplayMessageContent, outbox.entries.length, historyPageLoadVersion]);

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

  const pendingMessages = outbox.entries.filter(({ message }) => !message.projectID && (conversationID
    ? message.conversationID === conversationID
    : message.conversationID === null && Boolean(selectedLocalDraftKey) && message.conversationCreateKey === selectedLocalDraftKey));
  const composerDraftReady = !composerDraftIdentity || hydratedComposerDraftKey === composerDraftIdentity;
  const visibleComposerContent = composerDraftReady ? content : '';
  const incompleteAttachmentPreparationKey = composerAttachments.filter((item) => item.needsPreparation && !item.preparationCancelled).map((item) => item.id).join(',');
  useEffect(() => {
    if (!composerDraftReady || !composerDraftID || !incompleteAttachmentPreparationKey) return;
    void prepareComposerAttachmentItems(composerAttachments.filter((item) => item.needsPreparation && !item.preparationCancelled), composerDraftScope, composerDraftID);
  }, [composerAttachments, composerDraftID, composerDraftIdentity, composerDraftReady, composerDraftScope, incompleteAttachmentPreparationKey, prepareComposerAttachmentItems]);
  const renderPendingMessage = (entry: MessageOutboxEntry) => {
    const { message, scope } = entry;
    const alreadyVisible = displayMessages.some((item) => item.client_message_id === message.request.client_message_id);
    const failed = message.status === 'failed';
    const status = message.status === 'accepted' ? (message.attachments.length ? '消息已发送，附件等待处理' : '消息已发送')
      : message.status === 'sending' ? '正在发送' : message.status === 'unknown' ? '发送结果待确认'
        : message.status === 'failed' ? '发送失败' : '已保存在本机，等待发送';
    return <div className="chat-message-row chat-message-row-user chat-message-row-pending" key={`outbox:${message.request.client_message_id}`}>
      {failed && <button type="button" className="message-retry" aria-label="重试发送这条消息" title="重试发送"
        onClick={() => void sendMessage(entry, true)}>!</button>}
      <article className="chat-message chat-message-user chat-message-pending" aria-label="你的消息">
      {!alreadyVisible && <><strong className="chat-message-author">你</strong><p>{message.request.content}</p></>}
      {!failed && <small role="status">{status}{message.error ? `：${message.error}` : ''}</small>}
      {message.attachments.length > 0 && <small>附件 · {message.attachments.length}</small>}
      {message.status !== 'sending' && !failed && <AssistantOutboxRetryButton entry={{ scope, message }} sendMessage={sendMessage}
        label={message.status === 'accepted' && message.attachments.length ? '重试附件' : '核对并重试'} />}
      </article>
    </div>;
  };
  const renderTaskDetail = (summary: TaskSummary) => {
    const task = allTasksByID.get(summary.id);
    const taskQuery = taskQueries[taskIDs.indexOf(summary.id)];
    const taskFileFetchQuery = taskFileFetchQueries[taskIDs.indexOf(summary.id)];
    if (!task) return taskQuery?.isError
      ? <FloatingError className="error-inline task-detail-load-error" role="alert">任务详情暂不可用，当前状态未确认。
        <button type="button" className="button button-small button-secondary" onClick={() => void taskQuery.refetch()}>重试</button></FloatingError>
      : <p className="muted" role="status">正在读取任务详情…</p>;
    const busy = !terminalTaskStatuses.has(task.status);
    const k2Read = readK2TaskProjection(task);
    const k2 = k2Read.kind === 'available' ? k2Read.projection : null;
    const review = k2?.result_review ?? null;
    const currentStep = task.steps.find((step) => step.id === task.current_step);
    const recoveryMessage = task.waiting_reason === 'dsh_recovery_unresolved' ? recoveryHint(currentStep) : null;
    const visibleSteps = task.steps.filter((step) => ['tool', 'subagent', 'verification', 'verify', 'batch_item', 'batch_result', 'progress'].includes(step.kind) &&
      !(task.waiting_reason === 'dsh_recovery_unresolved' && step.status === 'unknown'));
    const activityGroups = taskActivityGroups(task, visibleSteps);
    const publishedReplyVisible = Boolean(review?.state === 'published' && review.source_task_id === task.id && review.reply_message_id &&
      (task.conversation_id !== conversationID || displayMessages.some((message) => message.id === review.reply_message_id && message.role === 'assistant' &&
        message.sources?.some((source) => source.task_id === review.source_task_id &&
          source.task_revision === review.source_task_revision && source.event_sequence === review.source_event_sequence))));
    const taskHost = task.host_id ? hostItems.find((host) => host.id === task.host_id) : undefined;
    const browserHandoffRequested = task.waiting_reason === 'clarification' && Boolean(task.clarification_request) && task.steps.some((step) =>
      step.kind === 'tool' && (step.title === 'Run cloud dsh tool: browser_prepare_user_handoff' ||
        step.title === 'Run cloud dsh MCP tool: browser_prepare_user_handoff'));
    const answer = clarificationAnswers[task.id] ?? '';
    const supplementEntries = outbox.entries.filter(({ message }) => message.conversationID === task.conversation_id &&
      message.request.reference_task_id === task.id);
    const fileFetches = taskFileFetchQuery?.data?.items ?? [];
    const progressText = taskActivityProgressText(summary, task, cloudTasksAvailable);
    return <section className="task-activity" data-task-id={task.id} aria-label={`任务详情：${task.objective || '未命名任务'}`}>
      <header className="task-activity-heading">
        <div><strong>{task.objective || '未命名任务'}</strong>
          <small>更新于 {formatDate(task.updated_at)}</small></div>
        <span className={`task-activity-status status-${task.status}`}>{taskActivityStatusText(summary, task)}</span>
      </header>
      {task.pending_request && <section className="notice notice-warning task-detail-attention"
        aria-label={task.pending_request.status === 'expired' ? '审批已到期' : '需要批准的操作'}>
        <h3>{task.pending_request.title}</h3><p>{task.pending_request.object}</p><p>{task.pending_request.content}</p><p>可能影响：{task.pending_request.impact}</p>
        <dl className="task-approval-disclosures" aria-label="风险、费用和其他审批信息">
          <dt>风险</dt><dd>{taskApprovalTextDisclosure(task.pending_request.disclosures?.risk)}</dd>
          <dt>本次费用</dt><dd>{taskApprovalCostDisclosure(task.pending_request.disclosures?.this_cost)}</dd>
          <dt>累计费用</dt><dd>{taskApprovalCostDisclosure(task.pending_request.disclosures?.cumulative_cost)}</dd>
          <dt>替代方案</dt><dd>{taskApprovalAlternativesDisclosure(task.pending_request.disclosures?.alternatives)}</dd>
          <dt>撤销方式</dt><dd>{taskApprovalTextDisclosure(task.pending_request.disclosures?.revocation)}</dd>
        </dl>
        {task.pending_request.expires_at && <p>审批到期：{formatDate(task.pending_request.expires_at)}</p>}
        {task.pending_request.status === 'expired' ? <p role="status">审批已到期；该操作已被拒绝并未执行。</p>
          : <div className="inline-actions">{(['approve', 'reject'] as const).map((decision) => <button key={decision} type="button"
            className={`button ${decision === 'approve' ? 'button-primary' : 'button-secondary'}`} disabled={respondToApproval.isPending}
            onClick={() => {
              const request = task.pending_request!;
              const actionScope = `approval:${task.id}:${request.id}`;
              const fingerprint = JSON.stringify({ version: request.version, decision });
              const key = taskActionKey(actionScope, fingerprint);
              respondToApproval.mutate({ taskID: task.id, approvalID: request.id, version: request.version,
                decision, actionScope, key });
            }}>{decision === 'approve' ? '批准并继续' : '拒绝'}</button>)}</div>}
      </section>}
      {task.clarification_request && <form className="notice notice-warning task-detail-attention" aria-label="补充任务信息"
        onSubmit={(event) => {
          event.preventDefault();
          const request = task.clarification_request;
          const clarificationAnswer = (clarificationAnswers[task.id] ?? '').trim();
          if (!request || !clarificationAnswer || answerClarification.isPending) return;
          const actionScope = `clarification:${task.id}:${request.id}`;
          const fingerprint = JSON.stringify({ version: request.version, answer: clarificationAnswer });
          const key = taskActionKey(actionScope, fingerprint);
          answerClarification.mutate({ taskID: task.id, clarificationID: request.id, version: request.version,
            answer: clarificationAnswer, actionScope, key });
        }}>
        <h3>任务需要补充信息</h3><p>{task.clarification_request.question}</p>
        <label htmlFor={`task-clarification-${task.id}`}>你的回答</label>
        <textarea id={`task-clarification-${task.id}`} value={answer} maxLength={16384} required
          onChange={(event) => setClarificationAnswers((current) => ({ ...current, [task.id]: event.target.value }))} disabled={answerClarification.isPending} />
        <button type="submit" className="button button-primary" disabled={!answer.trim() || answerClarification.isPending}>
          {answerClarification.isPending ? '正在提交…' : '提交并继续'}
        </button>
      </form>}
      <section className="task-current-progress" aria-label="当前进展">
        <h3>当前进展</h3>
        <p role={task.status === 'failed' ? 'alert' : 'status'}>{progressText}</p>
        {recoveryMessage && <p role="status">{recoveryMessage}</p>}
        {['disconnected', 'resyncing'].includes(taskEventFeeds[task.id] ?? '') && !showConsolidatedNetworkError && <p role="status">连接中断，正在重连…</p>}
      </section>
      {browserHandoffRequested && task.host_id && <div className="inline-actions assistant-browser-handoff">
        <Link className="button button-secondary" to={`/hosts#host-${encodeURIComponent(task.host_id)}`}>打开目标电脑进行登录</Link>
        <span className="muted">登录完成后交还 AI，再回到这里提交并继续。</span>
      </div>}
      {taskFileFetchQuery?.isPending && !taskFileFetchQuery.data && <p className="muted" role="status">正在核对本机文件读取记录…</p>}
      {taskFileFetchQuery?.isError && <FloatingError className="error-inline task-file-fetch-error" role="alert">
        {fileFetches.length ? '读取记录暂不可用，以下为最近一次已确认状态。' : '部分任务记录暂不可用，无法确认任务是否读取了本机文件。'}
        <button type="button" className="button button-small button-secondary" onClick={() => void taskFileFetchQuery.refetch()}>重试</button>
      </FloatingError>}
      {fileFetches.length > 0 && <section className="task-activity-k2 task-activity-file-fetches" aria-label="读取本机文件">
        <h3>读取本机文件</h3>
        <p>文件副本临时保存在云端供本任务使用；删除副本不会更改本机原文件，也不会撤回任务已读取的内容。</p>
        <ul>{fileFetches.map((item) => {
          const hasSize = item.size_bytes !== null;
          const progressValue = hasSize ? Math.min(item.confirmed_bytes, item.size_bytes!) : 0;
          return <li key={item.fetch_id}>
            <span><strong>{item.relative_path}</strong><small>{item.source_device_name} · {item.status === 'failed'
              ? taskFileFetchFailureText(item.failure_reason) : taskFileFetchStatusLabel[item.status]}</small></span>
            <small>已确认 {formatBytes(item.confirmed_bytes)}{hasSize ? ` / ${formatBytes(item.size_bytes!)}` : ' / 总大小待确认'}</small>
            {hasSize && item.size_bytes! > 0 && <progress max={item.size_bytes!} value={progressValue}
              aria-label={`读取本机文件进度：${item.relative_path}`} />}
            <small>{taskFileFetchContentText(item)}</small>
            <small>云端临时副本：{taskFileFetchContentStateLabel[item.content_state]}
              {item.expires_at ? ` · 到期 ${formatDate(item.expires_at)}` : ''}
              {item.deleted_at ? ` · 删除于 ${formatDate(item.deleted_at)}` : ''}</small>
            {item.status === 'completed' && item.content_state === 'retained' && <button type="button" className="button button-small button-secondary"
              disabled={deleteTaskFileFetchContent.isPending || taskFileFetchQuery?.isError}
              onClick={() => {
                if (window.confirm(`确认删除“${item.relative_path}”的云端临时副本？删除后无法恢复，本机原文件不受影响。`)) {
                  deleteTaskFileFetchContent.mutate({ taskID: task.id, fetchID: item.fetch_id });
                }
              }}>删除云端副本</button>}
          </li>;
        })}</ul>
      </section>}
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
      {busy && supplementTaskID === task.id && task.waiting_reason !== 'project_recovery_unavailable' && <AssistantTaskSupplementForm
        task={task} value={taskSupplementDrafts[task.id] ?? ''} disabled={savingMessage} saving={savingMessage}
        needsWorkspaceTarget={task.waiting_reason === 'workspace_unavailable' && !task.workspace_id}
        workspaceID={taskWorkspaceSelections[task.id] ?? ''} workspaces={workspaceItems}
        onWorkspaceChange={(workspaceID) => setTaskWorkspaceSelections((current) => ({ ...current, [task.id]: workspaceID }))}
        onSubmit={submitTaskSupplement}
        onChange={(event) => setTaskSupplementDrafts((current) => ({ ...current, [task.id]: event.target.value }))} />}
      {supplementEntries.map(({ message, scope }) => <div className="task-supplement-status" key={message.request.client_message_id}>
        <small role="status">补充消息：{message.status === 'accepted' ? '已发送' : message.status === 'sending' ? '正在发送'
          : message.status === 'unknown' ? '发送结果待确认' : message.status === 'failed' ? '发送失败' : '等待发送'}{message.error ? `：${message.error}` : ''}</small>
        {message.status !== 'sending' && <AssistantOutboxRetryButton entry={{ scope, message }} sendMessage={sendMessage}
          label={message.status === 'accepted' ? '核对消息' : '核对并重试'} />}
      </div>)}
      {activityGroups.length > 0 && <section className="task-detail-work" aria-label="工作过程"><h3>工作过程</h3>
        {activityGroups.map((group) => {
          const groupKey = `${task.id}:${group.id}`;
          const visibleCount = visibleActivitySteps[groupKey] ?? 30;
          return <section className="task-agent-group" key={group.id} aria-label={group.title}
            style={{ marginInlineStart: `${Math.min(group.depth, 3) * 12}px` }}>
          <h4>{group.title}{group.status && <small>{group.status}</small>}</h4>
          <ol className="task-activity-steps">{group.steps.slice(0, visibleCount).map((step) => {
            const expanded = taskStepSelection?.taskID === task.id && taskStepSelection.stepID === step.id;
            return <li key={step.id}><span className={`task-step-icon status-${step.status}`} aria-hidden="true">●</span><div>
              <button type="button" className="task-activity-step-button" aria-expanded={expanded} aria-controls={`task-step-detail-${step.id}`}
                onClick={() => setTaskStepSelection(expanded ? null : { taskID: task.id, stepID: step.id })}>
                <strong>{stepTitle(step)}</strong><small>{stepStatusText(step)}</small>
                {step.summary.trim() && <small className="task-activity-summary">{step.summary}</small>}
              </button>
              {expanded && <section id={`task-step-detail-${step.id}`} aria-label={`动作详情：${stepTitle(step)}`}>
                <TaskStepDetails key={`${user?.id ?? ''}:${capabilities.data?.service_instance_id ?? ''}:${task.conversation_id}:${task.id}:${step.id}`}
                  task={task} step={step} conversationID={task.conversation_id} actorID={user?.id ?? ''}
                  serviceInstanceID={capabilities.data?.service_instance_id ?? ''} />
              </section>}
            </div></li>;
          })}</ol>
          {group.steps.length > visibleCount && <button type="button" className="button button-small button-secondary"
            onClick={() => setVisibleActivitySteps((current) => ({ ...current, [groupKey]: visibleCount + 30 }))}>
            显示更多工作过程（剩余 {group.steps.length - visibleCount} 项）
          </button>}
        </section>;
        })}
      </section>}
      {k2Read.kind === 'invalid' && <FloatingError>
        任务协作记录暂不可用；当前仅展示已确认的任务状态和结果。
      </FloatingError>}
      {k2?.child_jobs.length ? <section className="task-activity-k2" aria-label="子任务进度">
        <h3>子任务进度</h3><ul>{k2.child_jobs.map((job) => <li key={job.job_id}>
          <span><strong>{job.title || '子任务'}</strong><small>{stepStatus[job.status]}</small></span>
        </li>)}</ul>
      </section> : null}
      {review && <section className="task-activity-k2 task-activity-review" aria-label="结果整理">
        <h3>结果整理</h3>
        <p role={review.state === 'failed' && !retryAcceptedResultReviewTaskIDs.has(task.id) ? 'alert' : 'status'}>
          {retryAcceptedResultReviewTaskIDs.has(task.id) ? '正在重新整理任务结果…' : resultReviewStatusText(review, publishedReplyVisible)}</p>
        {publishedReplyVisible && review.reply_message_id && <a href={`#conversation-message-${encodeURIComponent(review.reply_message_id)}`}>查看最终回复</a>}
        {review.state === 'failed' && review.trigger_message_id && !retryAcceptedResultReviewTaskIDs.has(task.id) && <button type="button" className="button button-small button-secondary"
          disabled={retryProcessing.isPending}
          onClick={() => retryProcessing.mutate({ conversationID: task.conversation_id, messageID: review.trigger_message_id!, taskID: task.id, resultReview: true })}>
          重试结果整理
        </button>}
      </section>}
      {task.artifacts.some(hasCompleteVerifiedArtifactProof) && <section className="task-activity-k2 task-activity-artifacts" aria-label="任务成果">
        <h3>任务成果</h3><ul>{task.artifacts.filter(hasCompleteVerifiedArtifactProof).map((artifact) => <li key={artifact.id}>
          <strong>{artifact.label}</strong>
          <small>{artifact.path} · {artifact.media_type} · {formatBytes(artifact.size_bytes!)}</small>
          <a className="task-activity-link" href={portalApi.taskArtifactContentURL(task.id, artifact.id)} download
            aria-label={`下载任务成果 ${artifact.label}`}>下载成果 ↓</a>
        </li>)}</ul>
      </section>}
      {task.execution_location === 'server' && task.host_id && <div className="inline-actions">
        {taskHost?.state === 'running'
          ? <button type="button" className="button button-secondary" disabled={openingTaskID === task.id}
            onClick={openTaskObservation.bind(undefined, task, taskHost)}>{openingTaskID === task.id ? '正在准备只读观察…'
              : task.artifacts.some(hasCompleteVerifiedArtifactProof) ? '在 Workbench 查看已核验产物' : '在 Workbench 只读观察'}</button>
          : <p className="muted" role="status">目标电脑当前不可用或未运行，暂不能打开 Workbench 观察页。</p>}
        {taskObservationError?.taskID === task.id && <FloatingError>{taskObservationError.message}</FloatingError>}
      </div>}
      <details className="task-detail-diagnostics"><summary>更多信息</summary>
        <p>任务 ID：{task.id}{task.current_revision !== undefined || task.desired_revision !== undefined
          ? ` · 修订 ${task.current_revision ?? '—'} / ${task.desired_revision ?? '—'}` : ''}</p>
        {task.host_id && <p>执行目标：{taskTargetText(task, hostItems)}</p>}
        {k2?.dependencies.length ? <section className="task-activity-k2" aria-label="依赖任务">
          <h3>依赖任务</h3><ul>{k2.dependencies.map((dependency) => <li key={`${dependency.task_id}:${dependency.required_revision}`}>
            <span>{dependencyStatusText(dependency)}</span>
          </li>)}</ul>
        </section> : null}
        {k2?.child_jobs.length ? <ul>{k2.child_jobs.map((job) => <li key={job.job_id}>
          {job.job_id} · 步骤 {job.task_step_id} · 修订 {job.task_revision}
        </li>)}</ul> : null}
        {review && <p>结果来源任务 {review.source_task_id} · 修订 {review.source_task_revision} · 结果事件 {review.source_event_sequence}
          {review.reply_message_id ? ` · 回复消息 ${review.reply_message_id}` : ''}</p>}
        {task.result && <section className="task-activity-k2" aria-label="原始结果">
          <h3>原始结果</h3><details><summary>查看原始结果（不是最终回复）</summary><ChatMarkdown content={task.result} /></details>
        </section>}
      </details>
    </section>;
  };

  const renderTaskSummary = (summary: TaskSummary) => {
    const task = allTasksByID.get(summary.id);
    const title = task?.objective || summary.objective || '未命名任务';
    const status = task?.status ?? summary.status;
    const updatedAt = task?.updated_at ?? summary.updated_at;
    const waiting = status === 'waiting_user';
    return <article className="task-activity-card" data-task-id={summary.id} key={summary.id}>
      <button ref={(button) => { if (button) taskCardButtons.current.set(summary.id, button); else taskCardButtons.current.delete(summary.id); }}
        type="button" className="task-activity-card-button"
        aria-label={`${waiting ? '查看并处理' : '查看详情'}：${title}，${taskActivityStatusText(summary, task)}`}
        onClick={() => { setTaskStepSelection(null); setTaskDetailTaskID(summary.id); }}>
        <span className="task-activity-card-title"><strong>{title}</strong>
          <span className={`task-activity-status status-${status}`}>{taskActivityStatusText(summary, task)}</span></span>
        <span className="task-activity-card-subtitle">{taskActivityProgressText(summary, task, cloudTasksAvailable)}</span>
        <span className="task-activity-card-footer"><small>更新于 {formatDate(updatedAt)}</small>
          <span>{waiting ? '查看并处理' : '查看详情'} <span aria-hidden="true">›</span></span></span>
      </button>
    </article>;
  };
  const selectedTaskSummary = taskDetailTaskID ? taskPanelTasks.find((item) => item.id === taskDetailTaskID) ?? null : null;
  const selectedTaskQuery = taskDetailTaskID ? taskQueries[taskIDs.indexOf(taskDetailTaskID)] : undefined;
  const selectedConversationTitle = selected && !isPlaceholderConversationTitle(selected.title) ? selected.title
    : conversationTitleFromMessages(conversationMessageItems) ?? (conversationID ? conversationPreviewTitles.get(conversationID) : null)
      ?? (conversationID ? '对话' : '新旁聊');
  const showConversationWelcome = Boolean(conversationID && messages.isSuccess && displayMessages.length === 0 && pendingMessages.length === 0);
  const conversationWelcome = <div className="assistant-welcome"><span className="assistant-mark assistant-logo" aria-hidden="true" />
    <h2>今天想聊些什么？</h2><p>告诉我你想完成的事情。</p>
  </div>;

  return <section className="assistant-page">
    {import.meta.env.DEV && import.meta.env.VITE_MOCK_API === '1' && <FloatingNotice className="notice-warning" data-notice-label="演示模式">当前为演示模式，尚未连接真实 AI 服务。这里的消息不会由 AI 执行。</FloatingNotice>}
    {mainConversation.isPending && <FloatingNotice className="notice-info">正在读取服务端主会话…</FloatingNotice>}
    {mainConversation.isError && !showConsolidatedNetworkError && <FloatingError role="alert">无法刷新服务端主会话{cachedMainConversationID ? '，已保留上次记录' : '，请重试'}。<button type="button" className="button button-small button-secondary" onClick={() => void mainConversation.refetch()}>重试</button></FloatingError>}
    {mainConversation.data?.status === 'uninitialized' && <FloatingNotice className="notice-info">正在初始化你的主会话…{ensureMainConversation.isError && <button type="button" className="button button-small button-secondary" onClick={ensureMain}>重试</button>}</FloatingNotice>}
    {capabilities.isPending && <FloatingNotice className="notice-info">正在检查服务能力…</FloatingNotice>}
    {capabilities.isError && !showConsolidatedNetworkError && <FloatingError className="error-inline" role="alert">无法读取服务能力：{errorDetails(capabilities.error).message}<button type="button" className="button button-small button-secondary" aria-label="重试服务能力" onClick={() => void capabilities.refetch()}>重试</button></FloatingError>}
    {capabilities.isSuccess && conversationAutoRoutingAvailable === false && <FloatingNotice className="notice-warning">
      服务端尚未确认支持对话自动处理；仍可发送消息，发送状态会根据服务端回执显示，未确认的消息会保留在本机。
    </FloatingNotice>}
    {capabilities.isSuccess && cloudTasksAvailable === false && <FloatingNotice className="notice-warning">
      云端工具当前不可用；普通消息仍可发送，实际调用该工具时会检查可用状态。
    </FloatingNotice>}
    <div className={`assistant-layout${taskPanelOpen ? ' assistant-layout-with-task-panel' : ''}${taskPanelOpen && taskPanelTab === 'computer' ? ' assistant-layout-with-computer-panel' : ''}${taskPanelOpen && taskPanelTab === 'computer' && computerControlExpanded ? ' assistant-layout-computer-expanded' : ''}`}>
      <aside id="context-sidebar" className="assistant-sidebar" aria-label="对话">
        <div className="assistant-sidebar-heading">
          <button type="button" className="icon-button" aria-label="刷新对话" title="刷新会话列表和当前消息" onClick={() => { void conversations.refetch(); void mainConversation.refetch(); void messages.refetch(); }}><MordeIcon name="refresh" /></button>
        </div>
        <div id="conversation-history" className="assistant-history-content">
          {mainConversationID && (mainConversationItem ? <AssistantConversationRow item={mainConversationItem} isMain selected={conversationID === mainConversationItem.id}
            recent={false} onSelect={selectConversationID} />
            : renderCachedMainConversation())}
          {!mainConversationID && mainConversation.isPending && <p className="muted" role="status">正在读取主会话…</p>}
          {conversations.isPending && !conversations.data && <p className="muted" role="status">正在读取旁聊…</p>}
          {conversations.isError && !conversations.data && <p className="muted" role="status">旁聊历史暂不可用</p>}
          <label className="conversation-search">
            <span className="sr-only">搜索旁聊标题</span>
            <input type="search" aria-label="搜索旁聊标题" placeholder="搜索旁聊标题" value={conversationSearch}
              onChange={(event) => { setConversationSearch(event.currentTarget.value); setVisibleSideConversationCount(5);
                if (event.currentTarget.value.trim()) setSideConversationsOpen(true); }} />
          </label>
          <div className="conversation-group">
            <details className="conversation-group-details" open={sideConversationsOpen} onToggle={(event) => setSideConversationsOpen(event.currentTarget.open)}>
              <summary>旁聊</summary>
              <div className="conversation-list">
                {localComposerDrafts.slice(0, visibleSideConversationCount).map(renderLocalDraftItem)}
                {sideConversations.slice(0, visibleSideConversationCount).map((item) => <AssistantConversationRow key={item.id} item={item}
                  selected={conversationID === item.id} recent={recentConversationID === item.id} previewTitle={conversationPreviewTitles.get(item.id)} onSelect={selectConversationID} />)}
              </div>
              {composerDrafts.isError && <FloatingError className="error-inline" role="alert">未发送草稿暂不可用：{errorDetails(composerDrafts.error).message}
                <button type="button" className="button button-small button-secondary" onClick={() => void composerDrafts.refetch()}>重试</button></FloatingError>}
              {composerDraftError && <FloatingError className="error-inline" role="alert">草稿保存状态：{composerDraftError}</FloatingError>}
              {conversations.isFetchNextPageError && <FloatingError className="error-inline" role="alert">{errorDetails(conversations.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void conversations.fetchNextPage()}>重试</button></FloatingError>}
              {(sideConversations.length > visibleSideConversationCount || localComposerDrafts.length > visibleSideConversationCount || conversations.hasNextPage) && <button type="button" className="button button-secondary button-small conversation-more"
                disabled={conversations.isFetchingNextPage} onClick={() => void showMoreSideConversations()}>
                {conversations.isFetchingNextPage ? '正在加载…' : '更多'}
              </button>}
              {!conversations.isError && conversations.isSuccess && !sideConversations.length && !localComposerDrafts.length && !conversations.hasNextPage && !composerDrafts.isPending && <p className="muted assistant-sidebar-empty">{conversationSearch.trim() ? '没有找到匹配标题的旁聊。' : '还没有旁聊。'}</p>}
              {composerDrafts.isPending && !localComposerDrafts.length && <p className="muted assistant-sidebar-empty" role="status">正在读取未发送草稿…</p>}
            </details>
            <button type="button" className="button button-secondary conversation-new" aria-label="新建旁聊" title="新建旁聊" onClick={startNew}>+</button>
          </div>
          <p className="assistant-sidebar-hint">主会话保留日常交流，旁聊可用于单独讨论。</p>
        </div>
      </aside>
      <div className="assistant-main">
        <header className="conversation-view-heading">
          <div className="assistant-chat-title"><span className="assistant-logo assistant-logo-header" aria-hidden="true" /><div>
            <h2>{conversationID && conversationID === mainConversationID ? '主会话' : selectedConversationTitle}</h2>
            <p>{conversationID && conversationID === mainConversationID ? '你的 Morde' : '旁聊 · 你的 Morde'}</p>
          </div></div>
          {!taskPanelOpen && <button ref={taskPanelToggle} type="button" className="button button-secondary task-panel-toggle"
            aria-label="打开侧边栏" aria-describedby={pendingTaskCount === null ? undefined : 'assistant-task-pending-count'}
            aria-expanded="false" aria-controls="assistant-task-panel"
            title={pendingTaskCount === null ? '打开侧边栏' : `打开侧边栏，${pendingTaskCount} 项待处理`}
            onClick={() => setTaskPanelOpen(true)}>
            <MordeIcon name="panel" />{pendingTaskCount !== null && <span id="assistant-task-pending-count" className="sr-only">{pendingTaskCount} 项等待用户</span>}
            {pendingTaskCount !== null && pendingTaskCount > 0 && <span className="task-pending-count" aria-hidden="true">{pendingTaskCount}</span>}
          </button>}
        </header>
        {streamedTasks.map((task) => <TaskStream key={task.id} task={task} conversationID={task.conversation_id} onState={setTaskEventFeed} />)}
        <div className={`conversation-view${!conversationID || showConversationWelcome ? ' conversation-view-welcome' : ''}`} aria-live="polite" ref={conversationView} onScroll={(event) => {
          const view = event.currentTarget;
          const atLatest = view.scrollHeight - view.scrollTop - view.clientHeight < 80;
          followConversation.current = atLatest;
          setShowJumpToLatest(!atLatest);
        }}>
          {!conversationID ? <>{conversationWelcome}{pendingMessages.map(renderPendingMessage)}</> : <>
          {conversationContentAccessRevoked ? <div className="empty-card" role="alert">
            <strong>当前对话内容暂不可访问。</strong><p className="muted">可以重试，或从左侧切换其他会话。</p>
            <button type="button" className="button button-secondary" onClick={() => void messages.refetch()}>重试读取对话</button>
          </div> : messages.isPending && displayMessages.length === 0 ? <div className="loading-card" role="status">正在加载消息…</div>
            : messages.isError && displayMessages.length === 0 ? showConsolidatedNetworkError ? <div className="loading-card">消息暂不可用</div>
              : <FloatingError className="error-card" role="alert">{errorDetails(messages.error).message}<button type="button" className="button button-secondary" onClick={() => void messages.refetch()}>重试</button></FloatingError>
              : <div className="message-list">
                {showConversationWelcome && conversationWelcome}
                {messages.isError && !showConsolidatedNetworkError && <FloatingError className="error-inline" role="alert">{errorDetails(messages.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void messages.refetch()}>重试</button></FloatingError>}
                {messages.hasNextPage && <div className="conversation-history-load">
                  {messages.isFetchNextPageError && <FloatingError className="error-inline" role="alert">更早的消息暂不可用：{errorDetails(messages.error).message}</FloatingError>}
                  <button type="button" className="button button-small button-secondary" disabled={messages.isFetchingNextPage}
                    onClick={() => void loadOlderMessages()}>{messages.isFetchingNextPage ? '正在加载更早消息…' : messages.isFetchNextPageError ? '重试加载更早消息' : '加载更早消息'}</button>
                </div>}
                {displayMessages.map((message, index) => {
                  const messageTask = tasksByID.get(message.task_id ?? message.related_task_id ?? '');
                  const acceptedRetryStatus = retryAcceptedMessageIDs.has(message.id) &&
                    ['failed', 'pending', 'processing'].includes(message.routing_status ?? '') ? '正在重新处理消息' : null;
                  const routingStatus = message.role === 'user' ? acceptedRetryStatus ?? conversationMessageStatus(message, messageTask) : null;
                  const previous = displayMessages[index - 1];
                  const messageDate = formatDate(message.created_at).split(' ')[0];
                  const showDate = !previous || formatDate(previous.created_at).split(' ')[0] !== messageDate;
                  return <Fragment key={message.id}>
                    {showDate && <div className="conversation-time-divider"><time dateTime={message.created_at}>{messageDate}</time></div>}
                    <div className={`chat-message-row chat-message-row-${message.role}`}>
                    {message.role === 'assistant' && <span className="assistant-logo assistant-logo-message" aria-hidden="true" />}
                    {message.role === 'user' && message.routing_status === 'failed' && message.routing_failure_reason !== 'message_too_long'
                      && !message.related_task_id && !retryAcceptedMessageIDs.has(message.id) && <button type="button" className="message-retry"
                      aria-label="重新处理这条消息" title="重新处理" disabled={retryProcessing.isPending && retryProcessing.variables?.messageID === message.id}
                      onClick={() => retryProcessing.mutate({ conversationID: message.conversation_id, messageID: message.id })}>!</button>}
                    <div className="chat-message-body">
                    {message.role === 'assistant' && <strong className="chat-message-author">Morde</strong>}
                    <article id={`conversation-message-${encodeURIComponent(message.id)}`} className={`chat-message chat-message-${message.role}${startedTaskMessageIDs.has(message.id) ? ' chat-message-user-started' : ''}`}
                      aria-label={message.role === 'assistant' ? 'Morde 的回复' : '你的消息'} tabIndex={message.role === 'assistant' ? 0 : undefined}>
                      {message.role === 'assistant' ? <ChatMarkdown content={message.content} /> : <p>{message.content}</p>}
                      {message.attachment_ids?.length ? <small className="chat-message-attachments">附件 · {message.attachment_ids.length}
                        {message.attachment_ids.map((attachmentID, index) => <span key={attachmentID}> · <a
                          href={`/api/personal-data/attachments/${encodeURIComponent(attachmentID)}`} download>下载附件 {index + 1}</a></span>)}
                      </small> : null}
                      {message.snapshotStatus === 'streaming' && <small role="status">正在生成…</small>}
                      {message.snapshotStatus === 'interrupted' && <small role="status">回复已中断</small>}
                      {startedTaskMessageIDs.has(message.id) && <span className="chat-task-started" role="img" aria-label="任务已开始处理" title="任务已开始处理">👌🏻</span>}
                    </article>
                    {message.role === 'user' && routingStatus && <div className="chat-message-status" role="status">
                        {routingStatus}
                        {message.delivery_status === 'unknown' && <button type="button" className="button button-small button-secondary"
                          disabled={messages.isFetching} onClick={() => {
                            void client.invalidateQueries({ queryKey: ['conversation-messages', conversationID] });
                            if (message.task_id) void client.invalidateQueries({ queryKey: ['task', message.task_id] });
                          }}>核对状态</button>}
                      </div>}
                    {message.role === 'assistant' && <time className="assistant-reply-time" dateTime={message.created_at} aria-label={`回复时间：${formatDate(message.created_at)}`}>{formatDate(message.created_at)}</time>}
                    </div></div>
                  </Fragment>;
                })}
              </div>}
          {pendingMessages.map(renderPendingMessage)}
          </>}
          {models.isError && !showConsolidatedNetworkError && <FloatingError className="error-inline" role="alert">无法读取模型：{errorDetails(models.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void models.refetch()}>重试</button></FloatingError>}
          {modelConfigurationKnown && models.data?.configuration_status === 'not_configured' && <FloatingNotice className="notice-warning">还没有配置云端模型。<Link to="/connections">配置模型</Link></FloatingNotice>}
          {modelConfigurationKnown && models.data?.configuration_status === 'configured' && !readyModels.length && <FloatingNotice className="notice-warning">模型已配置，当前没有通过检查的文字模型；消息仍可先发送。<Link to="/connections">检查模型设置</Link></FloatingNotice>}
          {outbox.storageError && <FloatingError className="error-inline" role="alert">本地消息存储不可用：{outbox.storageError}</FloatingError>}
          {execution.isError && !showConsolidatedNetworkError && <FloatingError className="error-inline" role="alert">无法读取 AI 服务状态：{errorDetails(execution.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void execution.refetch()}>重试</button></FloatingError>}
          {execution.data?.state === 'stopping' && <FloatingNotice className="notice-warning">AI 服务正在停止；消息仍可先发送。</FloatingNotice>}
          {hosts.isError && !showConsolidatedNetworkError && <FloatingError className="error-inline" role="alert">无法读取目标电脑：{errorDetails(hosts.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void hosts.refetch()}>重试</button></FloatingError>}
          {preferredHostID && !selectedHost && !hosts.isPending && !hosts.isError && <FloatingNotice className="notice-warning">所选电脑状态待核对；实际使用该目标时会检查权限和状态。</FloatingNotice>}
          {selectedHost && !hostIsReady && <FloatingNotice className="notice-warning">目标电脑暂不可用：{hostStateLabel[selectedHost.state] || selectedHost.state} · {controlStateLabels[selectedHost.control_state]}。</FloatingNotice>}
        </div>
        {showJumpToLatest && conversationID && <div className="assistant-jump-latest"><button type="button" className="button button-secondary"
          onClick={() => { followConversation.current = true; if (conversationView.current) conversationView.current.scrollTop = conversationView.current.scrollHeight; setShowJumpToLatest(false); }}>
          ↓ 回到最新消息</button></div>}
        <section className="assistant-composer" aria-label="发送对话消息">
          <form onSubmit={submit}>
            {!composerDraftReady && <p className="muted" role="status">正在恢复此会话草稿…</p>}
            {composerDraftError && <FloatingError className="error-inline" role="alert">草稿尚未可靠保存：{composerDraftError}。当前输入仍保留在页面中。</FloatingError>}
            <label className="sr-only" htmlFor="assistant-content">消息内容</label>
            <textarea id="assistant-content" value={visibleComposerContent} style={{ resize: 'none' }} onChange={(event) => {
              composerEditRevision.current += 1;
              if (!conversationID) ensureLocalConversationKey();
              const value = event.target.value;
              updateComposerDraftSnapshot({ content: value });
              setContent(value);
            }} onKeyDown={(event) => {
              if (event.key !== 'Enter' || event.shiftKey || event.nativeEvent.isComposing || event.nativeEvent.keyCode === 229) return;
              event.preventDefault();
              event.currentTarget.form?.requestSubmit();
            }} disabled={!composerDraftReady} placeholder="告诉我你想完成什么…" />
            {composerDraftReady && composerAttachments.length > 0 && <div className="assistant-attachment-list" aria-label="待发送附件">
              {composerAttachments.map((item) => {
                const mediaType = assistantFileMediaType(item.file);
                const preview = officePreviews[item.id];
                return <Fragment key={item.id}>
                  <span className="assistant-attachment-chip">
                    <span title={item.displayName ?? item.file.name}>{item.displayName ?? item.file.name}{item.attachmentID ? ' · 已准备' : item.needsPreparation ? item.preparationCancelled ? ' · 已暂停读取' : ' · 待读取' : ''}</span>
                    {item.needsPreparation && <button type="button" className="button button-small button-secondary"
                      disabled={preparingAttachmentIDsForRender.has(item.id)} onClick={() => void prepareComposerAttachmentItems([item], composerDraftScope, composerDraftID)}>
                      {preparingAttachmentIDsForRender.has(item.id) ? '读取中…' : '继续读取'}
                    </button>}
                    {assistantOfficeAttachmentTypes.has(mediaType) && <button type="button" aria-label={`预览附件 ${item.file.name}`}
                      disabled={preview?.status === 'loading'} style={{ width: 'auto', padding: '0 5px', whiteSpace: 'nowrap' }}
                      onClick={() => void previewOfficeAttachment(item)}>{preview?.status === 'loading' ? '预览中…' : '预览文本'}</button>}
                    <button type="button" aria-label={`移除附件 ${item.file.name}`} onClick={() => {
                      composerEditRevision.current += 1;
                      const snapshot = latestComposerDraft.current;
                      if (snapshot?.identity === composerDraftIdentity) {
                        updateComposerDraftSnapshot({ attachments: snapshot.draft.attachments.filter((candidate) => candidate.id !== item.id) });
                      }
                      setComposerAttachments((current) => current.filter((candidate) => candidate.id !== item.id));
                      setOfficePreviews((current) => {
                        const next = { ...current };
                        delete next[item.id];
                        return next;
                      });
                    }}>×</button>
                  </span>
                  {preview?.status === 'loading' && <span role="status" style={{ flexBasis: '100%' }}>正在提取 {item.file.name} 的文字…</span>}
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
                    multiple onChange={(event) => { if (!conversationID) ensureLocalConversationKey(); void addComposerFiles(event.currentTarget.files); event.currentTarget.value = ''; }} />
                </label>
                <AssistantVoiceInput compact onTranscript={(transcript) => {
                  composerEditRevision.current += 1;
                  if (!conversationID) ensureLocalConversationKey();
                  const snapshot = latestComposerDraft.current;
                  const current = snapshot?.identity === composerDraftIdentity ? snapshot.draft.content : content;
                  const value = current.trim() ? `${current.trimEnd()} ${transcript}` : transcript;
                  updateComposerDraftSnapshot({ content: value });
                  setContent(value);
                }} />
              </div>
              <div className="assistant-selectors">
                <label className="assistant-selector"><MordeIcon name="spark" /><span className="sr-only">AI 模型</span><select aria-label="选择 AI 模型"
                  value={composerDraftReady ? effectiveModelID : ''} onChange={(event) => { composerEditRevision.current += 1; updateComposerDraftSnapshot({ modelID: event.target.value }); setModelID(event.target.value); }} disabled={!composerDraftReady || models.isPending}>
                  <option value="">自动选择模型</option>{effectiveModelID && !readyModels.some((item) => item.id === effectiveModelID) && <option value={effectiveModelID}>已选择模型，状态待核对</option>}{readyModels.map((item) => <option key={item.id} value={item.id}>{item.name} · {item.model_name}</option>)}
                </select></label>
                <div className="assistant-target-row">
                  <label className="assistant-selector"><MordeIcon name="monitor" /><span className="sr-only">目标电脑</span><select aria-label="选择目标电脑" value={composerDraftReady ? selectedHost?.id ?? hostSelection ?? '' : ''}
                    disabled={!composerDraftReady || hosts.isPending} onChange={(event) => { composerEditRevision.current += 1; updateComposerDraftSnapshot({ hostID: event.target.value }); setHostSelection(event.target.value); }}>
                    <option value="">无电脑目标</option>
                    {hostSelection && !selectedHost && <option value={hostSelection}>所选电脑状态待核验</option>}
                    {hostItems.map((host) => <option key={host.id} value={host.id}>{host.name || host.id} · {hostStateLabel[host.state] || host.state} · {controlStateLabels[host.control_state]}</option>)}
                  </select></label>
                  <label className="assistant-selector"><MordeIcon name="folder" /><span className="sr-only">目标 Workspace</span><select aria-label="选择目标 Workspace"
                    value={composerDraftReady ? workspaceSelection ?? '' : ''} disabled={!composerDraftReady || workspaces.isPending}
                    onChange={(event) => { composerEditRevision.current += 1; setWorkspaceSelection(event.target.value || null); }}>
                    <option value="">自动选择 Workspace</option>
                    {workspaceSelection && !workspaceItems.some((workspace) => workspace.id === workspaceSelection) &&
                      <option value={workspaceSelection}>所选 Workspace 状态待核对</option>}
                    {workspaceItems.map((workspace) => <option key={workspace.id} value={workspace.id}>{workspace.name} · v{workspace.revision}</option>)}
                  </select></label>
                </div>
                {workspaces.isError && !showConsolidatedNetworkError && <FloatingError className="error-inline assistant-connector-error" role="alert">工作区列表暂不可用<button type="button" className="button button-small button-secondary" onClick={() => void workspaces.refetch()}>重试</button></FloatingError>}
                {enabledMCPConnectors.length > 0 && <details className="assistant-tool-menu"><summary><MordeIcon name="sliders" />工具</summary>
                  <fieldset className="assistant-tool-connectors"><legend>工具连接</legend>
                    <div>{enabledMCPConnectors.map((connector) => <label key={connector.id}>
                      <input type="checkbox" checked={composerDraftReady && mcpConnectorIDs.includes(connector.id)} disabled={!composerDraftReady}
                        onChange={(event) => {
                          composerEditRevision.current += 1;
                          const snapshot = latestComposerDraft.current;
                          const current = snapshot?.identity === composerDraftIdentity ? snapshot.draft.mcpConnectorIDs : mcpConnectorIDs;
                          const next = event.target.checked ? [...new Set([...current, connector.id])] : current.filter((id) => id !== connector.id);
                          updateComposerDraftSnapshot({ mcpConnectorIDs: next });
                          setMcpConnectorIDs(next);
                        }} />
                      <span>{connector.name}</span>
                    </label>)}</div>
                  </fieldset>
                </details>}
                {mcpConnectors.isError && !showConsolidatedNetworkError && <FloatingError className="error-inline assistant-connector-error" role="alert">工具连接暂不可用<button type="button" className="button button-small button-secondary" onClick={() => void mcpConnectors.refetch()}>重试</button></FloatingError>}
              </div>
              <div className="assistant-task-actions" role="group" aria-label="发送操作">
                {preparingAttachmentCount > 0 && <button type="button" className="button button-secondary" onClick={cancelComposerAttachmentPreparation}>取消读取</button>}
                <button className="button button-primary assistant-send" type="submit" disabled={!composerDraftReady || savingMessage || preparingAttachmentCount > 0 || composerAttachments.some((item) => item.needsPreparation) || (!content.trim() && !composerAttachments.length)}>
                  {preparingAttachmentCount > 0 || composerAttachments.some((item) => item.needsPreparation) ? '读取附件…' : savingMessage ? '保存中…' : '发送'} <span aria-hidden="true">↑</span>
                </button>
              </div>
            </div>
          </form>
        </section>
      </div>
      {taskPanelOpen && <aside id="assistant-task-panel" className="assistant-task-panel" aria-label="当前会话侧边栏">
        <header className="task-panel-heading">
          <nav className="task-panel-tabs" aria-label="侧边栏内容">
            <button type="button" aria-pressed={taskPanelTab === 'tasks'} className={taskPanelTab === 'tasks' ? 'is-selected' : ''}
              onClick={() => setTaskPanelTab('tasks')}>任务</button>
            <button type="button" aria-pressed={taskPanelTab === 'files'} className={taskPanelTab === 'files' ? 'is-selected' : ''}
              onClick={() => setTaskPanelTab('files')}>文件</button>
            <button type="button" aria-pressed={taskPanelTab === 'computer'} className={taskPanelTab === 'computer' ? 'is-selected' : ''}
              onClick={() => setTaskPanelTab('computer')}>云端电脑</button>
          </nav>
          <div className="inline-actions">
            {taskPanelTab !== 'computer' && <button type="button" className="icon-button" aria-label="刷新当前会话侧边栏" onClick={() => {
              void conversationTasks.refetch();
              void recentConversationTasks.refetch();
              void client.invalidateQueries({ queryKey: ['task'] });
              if (taskDetailTaskID) void client.invalidateQueries({ queryKey: ['task-file-fetches', taskDetailTaskID] });
            }}>↻</button>}
            <button type="button" className="icon-button" aria-label="关闭侧边栏" onClick={() => {
              postComputerVisibility(false);
              computerRequestGeneration.current += 1;
              setComputerConnecting(false);
              setComputerEmbed(null);
              setComputerControlExpanded(false);
              setTaskPanelOpen(false);
              setTaskDetailTaskID(null);
              setTaskPanelTab('tasks');
              window.requestAnimationFrame(() => taskPanelToggle.current?.focus());
            }}>×</button>
          </div>
        </header>
        {conversationContentAccessRevoked && taskPanelTab !== 'computer' && <div className="loading-card" role="alert">
          当前会话内容暂不可访问。<button type="button" className="button button-small button-secondary" onClick={() => void messages.refetch()}>重试</button>
        </div>}
        {!conversationContentAccessRevoked && taskPanelTab === 'tasks' && taskDetailTaskID && <div className="task-panel-detail task-panel-list">
          <div className="task-panel-detail-nav"><button type="button" className="button button-small button-secondary"
            onClick={() => {
              setTaskStepSelection(null);
              setTaskDetailTaskID(null);
              window.requestAnimationFrame(() => taskCardButtons.current.get(taskDetailTaskID)?.focus());
            }}>‹ 返回任务列表</button></div>
          {selectedTaskSummary ? renderTaskDetail(selectedTaskSummary)
            : selectedTaskQuery?.isError ? <FloatingError className="error-inline task-detail-load-error" role="alert">任务详情暂不可用，当前状态未确认。
              <button type="button" className="button button-small button-secondary" onClick={() => void selectedTaskQuery.refetch()}>重试</button></FloatingError>
              : <p className="muted" role="status">此任务已不在当前可见列表中，请刷新任务动态。</p>}
        </div>}
        {!conversationContentAccessRevoked && taskPanelTab === 'tasks' && <div className="task-panel-list" hidden={Boolean(taskDetailTaskID)}>
          {capabilities.isPending && <p className="muted" role="status">正在确认账号与服务身份…</p>}
          {conversationID && conversationTasks.isPending && !conversationTasks.data && <p className="muted" role="status">正在读取任务…</p>}
          {conversationTasks.isError && <FloatingError className="error-inline" role="alert">任务状态暂不可用。
            <button type="button" className="button button-small button-secondary" onClick={() => void conversationTasks.refetch()}>重试</button></FloatingError>}
          {recentConversationTasks.isError && <FloatingError className="error-inline" role="alert">已结束任务暂不可用。
            <button type="button" className="button button-small button-secondary" onClick={() => void recentConversationTasks.refetch()}>重试</button></FloatingError>}
          {taskProgressReadError && <FloatingError className="error-inline" role="alert">
            部分任务进展暂不可用，显示最近一次已确认状态。<button type="button" className="button button-small button-secondary"
              onClick={() => void client.invalidateQueries({ queryKey: ['task'] })}>重试</button></FloatingError>}
          {!conversationID && <p className="muted">开始对话后，这里会显示相关任务。</p>}
          {conversationID && !conversationTasks.isPending && !recentConversationTasks.isPending && !taskProgressReadError && !visibleTaskPanelTasks.length &&
            <p className="muted">此会话还没有任务。</p>}
          {taskPanelRows.length > 0 && <ScreenPagination items={taskPanelRows} getKey={(item) => item.key}
            estimatedItemHeight={116} height="100%" ariaLabel="当前会话任务分页" className="task-panel-pagination"
            renderItem={(row) => <div className="task-panel-group">
              {row.first && (row.group.id === 'recent'
                ? <button type="button" className="task-panel-group-toggle" aria-expanded={taskPanelRecentOpen}
                  onClick={() => setTaskPanelRecentOpen((open) => !open)}>最近结束 <span aria-hidden="true">{taskPanelRecentOpen ? '▾' : '▸'}</span></button>
                : <h3>{row.group.title}</h3>)}
              {row.task && renderTaskSummary(row.task)}
            </div>} />}
        </div>}
        {!conversationContentAccessRevoked && taskPanelTab === 'files' && <div className="task-panel-list task-panel-files">
          <Link className="button button-secondary button-small" to="/workspaces">管理文件同步</Link>
          {messages.hasNextPage && <button type="button" className="button button-small button-secondary" disabled={messages.isFetchingNextPage}
            onClick={() => void loadOlderMessages()}>{messages.isFetchingNextPage ? '正在加载更早消息…' : '加载更早消息中的附件'}</button>}
          {!conversationFiles.length ? <p className="muted">此会话还没有附件或任务成果。</p> : <ScreenPagination items={conversationFiles}
            getKey={(item) => item.key} estimatedItemHeight={72} height="100%" ariaLabel="当前会话文件分页"
            renderItem={(item) => item.kind === 'attachment'
              ? <article className="task-file-card" key={item.key}>
                <strong>消息附件 {item.index}</strong><small>{formatDate(item.createdAt)}</small>
                <a href={`/api/personal-data/attachments/${encodeURIComponent(item.attachmentID)}`} download
                  aria-label={`下载消息附件 ${item.index}`}>下载附件 <span aria-hidden="true">↓</span></a>
              </article>
              : <article className="task-file-card" key={item.key}>
                <strong>{item.artifact.label || '任务成果'}</strong>
                <small>{item.taskTitle} · {item.artifact.path} · {item.artifact.media_type} · {formatBytes(item.artifact.size_bytes!)}</small>
                <a href={portalApi.taskArtifactContentURL(item.taskID, item.artifact.id)} download
                  aria-label={`下载任务成果 ${item.artifact.label}`}>下载成果 ↓</a>
                <button type="button" className="button button-small button-secondary" onClick={() => {
                  setTaskPanelTab('tasks');
                  setTaskDetailTaskID(item.taskID);
                }}>查看任务成果</button>
              </article>} />}
        </div>}
        <section className="task-panel-list task-panel-computer" aria-label="当前会话云端电脑" hidden={taskPanelTab !== 'computer'}>
          <div className="computer-panel-toolbar">
            <label>云端电脑
              <select aria-label="选择侧栏云端电脑" value={selectedComputerHost?.id ?? ''} disabled={sharedHosts.isPending || hosts.isPending}
                onChange={(event) => {
                  postComputerVisibility(false);
                  computerRequestGeneration.current += 1;
                  setComputerConnecting(false);
                  setComputerEmbed(null);
                  setComputerControlExpanded(false);
                  setComputerPanelError('');
                  setComputerHostID(event.target.value);
                }}>
                <option value="">选择有权限访问的电脑</option>
                {computerHostOptions.map((host) => <option key={host.id} value={host.id}>
                  {host.name} · {host.owned ? '我的电脑' : '共享电脑'}
                </option>)}
              </select>
            </label>
            <button type="button" className="button button-primary" disabled={computerConnecting || !selectedComputerHost ||
              selectedComputerHost.state !== 'running' || !selectedComputerInitialTarget}
              onClick={() => void requestComputerEmbed()}>
              {computerConnecting ? '正在连接…' : computerEmbed?.status === 'ready' ? '重新连接'
                : computerEmbed?.status === 'error' ? '重试连接' : '连接到此会话'}
            </button>
          </div>
          {sharedHosts.isError && <FloatingError className="error-inline" role="alert">共享电脑列表暂不可用。
            <button type="button" className="button button-small button-secondary" onClick={() => void sharedHosts.refetch()}>重试</button></FloatingError>}
          {computerPanelError && <FloatingError role="alert">{computerPanelError}</FloatingError>}
          {!conversationID && <p className="muted">新旁聊创建后，才能把电脑连接到该会话。</p>}
          {selectedComputerHost && selectedComputerHost.state !== 'running' && <p className="muted">此电脑当前未运行，启动后才能连接。</p>}
          {selectedComputerHost && !selectedComputerTargets.length && <p className="muted">此电脑没有可嵌入的授权功能。</p>}
          {computerEmbed?.conversationID === conversationID && <>
            <div className="computer-panel-status" role="status" aria-live="polite">
              {computerEmbed.status === 'ready' ? '工作台已就绪' : computerEmbed.status === 'error'
                ? computerEmbed.error || '工作台连接失败。' : computerEmbed.status === 'submitting' ? '正在安全连接…' : '正在等待工作台就绪…'}
              {computerEmbed.status === 'ready' && <small>远程桌面连接和控制状态以工作台内的实时提示为准。</small>}
              {computerEmbed.status === 'ready' && <small>可用功能：{computerEmbed.targets.map((target) => ({
                browser: '桌面', terminal: '终端', workspace: 'Workspace', files: '主机文件', vscode: 'VS Code',
              }[target])).join('、')}</small>}
              {computerEmbed.status === 'error' && selectedComputerHost && <button type="button" className="button button-small button-secondary"
                onClick={() => {
                  try {
                    const fallbackTarget = selectedComputerHost.owned ? undefined
                      : selectedComputerInitialTarget === 'browser' ? 'desktop'
                        : selectedComputerInitialTarget === 'terminal' || selectedComputerInitialTarget === 'workspace'
                          ? selectedComputerInitialTarget : undefined;
                    enterHostInNewTab(selectedComputerHost.id, fallbackTarget);
                  } catch (error) { setComputerPanelError(errorDetails(error).message); }
                }}>在新窗口打开工作台</button>}
            </div>
            <div className="computer-panel-frame">
              <iframe key={computerEmbed.nonce} ref={computerFrame} name={`morde-cloud-${computerEmbed.nonce}`} title="当前会话云端电脑工作台"
                src="about:blank" allow="fullscreen" onError={() => setComputerEmbed((current) => current?.nonce === computerEmbed.nonce
                  ? { ...current, status: 'error', error: '工作台页面加载失败，请重试。' } : current)} />
            </div>
          </>}
        </section>
      </aside>}
    </div>
  </section>;
}
