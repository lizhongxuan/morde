import { Fragment, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import type { CSSProperties, FormEvent, KeyboardEvent as ReactKeyboardEvent } from 'react';
import { Link, useLocation, useNavigate, useOutletContext } from 'react-router';
import { useInfiniteQuery, useMutation, useQueries, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, newIdempotencyKey, portalApi } from '../app/api';
import type { ConversationDeliveryStatus, HostEmbedTicket, HostEmbedTicketRequest, HostMemberPermission } from '../app/api';
import { readK2TaskProjection } from '../app/k2-task-projection';
import type { K2ResultReview } from '../app/k2-task-projection';
import type { MessageOutboxEntry } from '../app/use-message-outbox';
import { useMessageOutbox } from '../app/use-message-outbox';
import { useMessageVariantOutbox } from '../app/use-message-variant-outbox';
import type { MessageVariantOutboxEntry } from '../app/use-message-variant-outbox';
import { buildMessageVariantTimeline, createMessageVariant, messageVariantActionLabel } from '../app/message-variants';
import type { VersionableMessage } from '../app/message-variants';
import { clearProcessingRetryKey, processingRetryKey } from '../app/message-processing-retry';
import { routingFailureText } from '../app/message-status';
import { maxMessageBytes, needsMessageUpload, uploadMessage } from '../app/message-upload';
import { conversationTitleFromText, firstOutboxConversationMessage, messageOutbox, outboxReceiptRequest } from '../app/message-outbox';
import type { PendingConversationMessage, PendingMessageAttachment } from '../app/message-outbox';
import { ApiError } from '../api/client';
import { isTransientNetworkError } from '../api/errors';
import { useAuth } from '../app/auth';
import { formatBytes, formatDate, hostStateLabel } from '../app/format';
import { useToast } from '../app/toast';
import { AssistantVoiceInput } from '../components/AssistantVoiceInput';
import { AgentWorkspaceBrowser } from '../components/AgentWorkspaceBrowser';
import { ChatMarkdown } from '../components/ChatMarkdown';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { HostControlPanel } from '../components/HostControlPanel';
import { MordeIcon } from '../components/MordeIcon';
import { ScreenPagination } from '../components/ScreenPagination';
import { TaskApprovalCard } from '../components/TaskApprovalCard';
import { sha256Hex } from './personal-data-store';
import { extractAssistantPDFText } from './assistant-pdf';
import { assistantComposerDraftScope, assistantDraftStorage } from './assistant-drafts';
import type { AssistantComposerDraft } from './assistant-drafts';
import './AssistantSidebar.css';
import './MessageVariants.css';
import { FloatingError, FloatingNotice } from '../components/FloatingError';

type Conversation = Awaited<ReturnType<typeof portalApi.conversations>>['items'][number];
type ConversationMessage = Awaited<ReturnType<typeof portalApi.conversationMessages>>['items'][number];
type ConversationMessagePage = Awaited<ReturnType<typeof portalApi.conversationMessages>>;
type ConversationMessagesData = { pages: ConversationMessagePage[]; pageParams: unknown[] };
type TaskSummary = Awaited<ReturnType<typeof portalApi.tasks>>['items'][number];
type Task = Awaited<ReturnType<typeof portalApi.task>>;
type TaskStep = Task['steps'][number];
type TaskStepDetail = Awaited<ReturnType<typeof portalApi.taskStepDetail>>;
type TaskStepDetailWithExecution = Omit<TaskStepDetail, 'execution'> & { execution?: TaskStepDetail['execution'] | null };
type TaskArtifact = Task['artifacts'][number];
type ClarificationInteraction = {
  clarificationID: string;
  version: number;
  question: string;
  status: 'sending' | 'submitted' | 'failed' | 'unknown' | 'revoked';
  message?: string;
};
type AssistantPanelTab = 'tasks' | 'files' | 'computer';
type AssistantOutletContext = {
  panelOpen: boolean;
  panelTab: AssistantPanelTab;
  workspaceWidth: number | null;
  setWorkspaceWidth: (width: number) => void;
  setPanelOpen: (open: boolean) => void;
  setPanelTab: (tab: AssistantPanelTab) => void;
  focusPanelToggle?: () => void;
};
type Host = Awaited<ReturnType<typeof portalApi.hosts>>['items'][number];
type ComputerHostOption = { id: string; name: string; url: string; state: string; owned: boolean; permissions: HostMemberPermission[]; control_state?: Host['control_state'] };
type ComputerEmbedSession = {
  conversationID: string;
  hostID: string;
  hostOrigin: string;
  exchangeURL?: string;
  nonce: string;
  ticket?: string;
  native?: boolean;
  targets: HostEmbedTicket['targets'];
  status: 'requesting' | 'submitting' | 'loading' | 'ready' | 'error';
  remoteDesktop: 'connecting' | 'connected' | 'disconnected' | 'failed';
  activeTarget: 'browser' | 'terminal' | 'files' | 'vscode' | null;
  controlLocked: boolean;
  controlVersion: number | null;
  selectionSeq: number | null;
  lockSeq: number | null;
  error?: string;
};
type ComputerEmbedCommand =
  | { type: 'init'; conversation_id: string; visible: boolean }
  | { type: 'visibility'; conversation_id: string; visible: boolean }
  | { type: 'select_target'; target: 'browser' | 'terminal' | 'files' | 'vscode'; seq: number }
  | { type: 'control_lock'; locked: boolean; seq: number };
type NativeWorkbenchBridge = {
  openHostWorkbenchEmbed?: (request: { hostID: string; conversationID: string; initialTarget: string; targets: string[]; shared: boolean }) => Promise<unknown>;
  setHostWorkbenchBounds?: (nonce: string, bounds: { x: number; y: number; width: number; height: number }) => Promise<unknown>;
  sendHostWorkbenchCommand?: (nonce: string, command: ComputerEmbedCommand) => Promise<unknown>;
  closeHostWorkbenchEmbed?: (nonce: string) => Promise<unknown>;
  onHostWorkbenchMessage?: (listener: (value: unknown) => void) => () => void;
};
type ComputerLockAck = { nonce: string; seq: number; locked: boolean; resolve: (acknowledged: boolean) => void; promise: Promise<boolean>; timer: number };
function nativeWorkbenchBridge(): NativeWorkbenchBridge | undefined {
  return (window as Window & { morde?: NativeWorkbenchBridge }).morde;
}
type AssistantDisplayMessage = Omit<ConversationMessage, 'delivery_status'> & { delivery_status?: ConversationDeliveryStatus } & {
  snapshotStatus?: 'streaming' | 'completed' | 'interrupted';
} & Pick<VersionableMessage, 'variant_group_id' | 'variant_version' | 'variant_action'>;
type RoutingFailureMessage = Pick<ConversationMessage,
  'id' | 'role' | 'routing_status' | 'routing_failure_reason' | 'task_id' | 'related_task_id' | 'duplicate_of_message_id'> & {
  actor_id?: unknown;
};
type ComposerAttachment = { id: string; file: File; displayName?: string; objectID: string; objectKey: string; uploadKey: string; commitKey: string; attachmentID?: string; needsPreparation?: boolean; preparationCancelled?: boolean };
type OfficePreviewState = { status: 'loading' } | { status: 'ready'; text: string };

function conversationTitleFromMessages(messages: readonly Pick<ConversationMessage, 'role' | 'content'>[]): string | null {
  return conversationTitleFromText([...messages].reverse().find((message) => message.role === 'user')?.content);
}

function isPlaceholderConversationTitle(title?: string): boolean {
  return !title?.trim() || ['新对话', '新旁聊', '对话'].includes(title.trim());
}

function clarificationDraftKey(taskID: string, clarificationID: string, version: number): string {
  return `${taskID}:${clarificationID}:${version}`;
}

function conversationMessageActorID(message: RoutingFailureMessage): string | undefined {
  const actorID = message.actor_id;
  return typeof actorID === 'string' ? actorID : undefined;
}

function isUnlinkedRoutingFailure(message: RoutingFailureMessage): boolean {
  return message.role === 'user' && message.routing_status === 'failed'
    && !message.task_id && !message.related_task_id && !message.duplicate_of_message_id;
}

function retryEligibilityReasonText(reason: string): string {
  switch (reason) {
    case 'route_not_failed': return '服务端确认这条消息当前不再处于失败状态；刷新对话后核对最新状态。';
    case 'task_already_created': return '服务端确认这条消息已关联任务；请从该任务查看进度。';
    case 'message_too_long': return '原文超过当前处理限制；请缩短后重新发送，不能原样重试。';
    case 'reconciliation_required': return '上次共享模型调用仍待对账；完成对账前不会重复处理。';
    case 'route_processing': return '这条消息仍在处理中；请稍后刷新状态。';
    case 'result_review_not_retryable': return '这条结果复核不支持从原消息重试。';
    case 'result_review_outcome_unknown': return '结果复核调用状态仍未知；核对完成前不会重试。';
    case 'retry_limit_reached': return '这条消息已达到重试次数上限。';
    default: return '服务端暂时无法确认这条消息是否可以重试；原消息仍已保留。';
  }
}

function retryEligibilityAllowsDispatch(eligibility: { retry_eligible: boolean; retry_reason: string; retry_warning?: string } | undefined): boolean {
  return eligibility?.retry_eligible === true && eligibility.retry_reason === 'retry_available'
    && (!eligibility.retry_warning || eligibility.retry_warning === 'previous_personal_model_call_outcome_unknown');
}

function routingFailureNextStep(message: RoutingFailureMessage, actorMatches: boolean,
  capability: 'confirmed' | 'unsupported' | 'unknown', eligibility?: {
    isFetching: boolean;
    isError: boolean;
    data?: { retry_eligible: boolean; retry_reason: string; retry_warning?: string };
  }): string {
  if (message.routing_failure_reason === 'message_too_long') return '原文已保留；请缩短后重新发送，不能重试这条原文。';
  if (!actorMatches) return '只有发送这条消息的账号可以重新处理。';
  if (capability === 'unknown') return '服务身份或消息处理能力尚未确认；请检查服务状态后再试。';
  if (capability === 'unsupported') return '当前服务未开放安全重试；消息已保留，请稍后检查服务功能。';
  if (!eligibility || eligibility.isFetching) return '正在核对服务端重试条件；核对完成前不会发送重试。';
  if (eligibility.isError || !eligibility.data) return '暂时无法确认重试资格；原消息已保留，请稍后重新核对。';
  if (!eligibility.data.retry_eligible) return retryEligibilityReasonText(eligibility.data.retry_reason);
  if (eligibility.data.retry_reason !== 'retry_available') return retryEligibilityReasonText(eligibility.data.retry_reason);
  if (eligibility.data.retry_warning === 'previous_personal_model_call_outcome_unknown') {
    return '上一次个人模型调用结果仍未知；新的尝试可能产生重复调用，确认后才会继续。';
  }
  if (eligibility.data.retry_warning) return '服务端返回尚未支持的重试警告；为避免重复操作，暂不重试。';
  return '';
}

const fileDeliveryIntentClarificationCallIDs = new Set([
  'morde-file-delivery-intent',
  'morde-route-file-delivery-intent',
]);
const routeFileDeliveryIntentClarificationCallID = 'morde-route-file-delivery-intent';

function isFileDeliveryIntentClarification(callID: string | null | undefined): boolean {
  return callID != null && fileDeliveryIntentClarificationCallIDs.has(callID);
}

function canAnswerClarification(task: Task): boolean {
  const request = task.clarification_request;
  if (!request?.question) return false;
  if (task.status === 'waiting_user' && task.waiting_reason === 'clarification_required') return true;
  return request.call_id === routeFileDeliveryIntentClarificationCallID &&
    (task.status === 'running' || task.status === 'pausing' || task.status === 'paused');
}

function UserMessageContent({ content, attachmentIDs = [] }: { content: string; attachmentIDs?: string[] }) {
  return <>
    <p>{content}</p>
    {attachmentIDs.length > 0 && <small className="chat-message-attachments">附件 · {attachmentIDs.length}
      {attachmentIDs.map((attachmentID, attachmentIndex) => <span key={attachmentID}> · <a
        href={`/api/personal-data/attachments/${encodeURIComponent(attachmentID)}`} download>下载附件 {attachmentIndex + 1}</a></span>)}
    </small>}
  </>;
}

function AssistantConversationRow({ item, isMain, selected, recent, previewTitle, onSelect, onMakeMain, makeMainPending }: {
  item: Conversation; isMain?: boolean; selected: boolean; recent: boolean; previewTitle?: string | null; onSelect: (id: string) => void;
  onMakeMain?: (id: string) => void; makeMainPending?: boolean;
}) {
  const title = isMain ? '主会话' : isPlaceholderConversationTitle(item.title) ? previewTitle || item.title || '新对话' : item.title;
  return <div className={`conversation-row${isMain ? ' conversation-row-main' : ''}`}>
    <button type="button" className={`conversation-item ${selected ? 'is-selected' : ''}`} onClick={() => onSelect(item.id)}>
      {isMain && <span className="assistant-logo conversation-main-logo" aria-hidden="true" />}
      <strong>{title}</strong>
      {recent && !isMain && <small>本机最近查看</small>}
      <small>{isMain ? '最近更新 · ' : ''}{formatDate(item.updated_at)}</small>
    </button>
    {onMakeMain && <button type="button" className="button button-small button-secondary conversation-make-main"
      aria-label={`将 ${title} 设为主会话`} disabled={makeMainPending} onClick={() => onMakeMain(item.id)}>
      {makeMainPending ? '正在设为主会话…' : '设为主会话'}
    </button>}
  </div>;
}

function hasCompleteVerifiedArtifactProof(artifact: TaskArtifact): boolean {
  return artifact.verified && Boolean(artifact.workspace_id) && Number.isSafeInteger(artifact.workspace_revision)
    && (artifact.workspace_revision ?? -1) >= 0 && Number.isSafeInteger(artifact.size_bytes)
    && (artifact.size_bytes ?? -1) >= 0 && /^[a-f0-9]{64}$/iu.test(artifact.sha256 ?? '')
    && Boolean(artifact.path.trim()) && Boolean(artifact.media_type.trim());
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

const computerQuickActions: { target: 'browser' | 'terminal' | 'files' | 'vscode'; label: string; icon: 'browser' | 'terminal' | 'files' | 'vscode' }[] = [
  { target: 'browser', label: '桌面', icon: 'browser' },
  { target: 'terminal', label: '终端', icon: 'terminal' },
  { target: 'files', label: '文件', icon: 'files' },
  { target: 'vscode', label: 'VS Code', icon: 'vscode' },
];

function ComputerActionIcon({ icon }: { icon: 'browser' | 'terminal' | 'files' | 'vscode' }) {
  if (icon === 'browser') return <svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="8.5" /><path d="M3.5 12h17M12 3.5c2.4 2.3 3.6 5.2 3.6 8.5s-1.2 6.2-3.6 8.5c-2.4-2.3-3.6-5.2-3.6-8.5S9.6 5.8 12 3.5Z" /></svg>;
  if (icon === 'terminal') return <svg viewBox="0 0 24 24"><rect x="3" y="4.5" width="18" height="15" rx="2.5" /><path d="m7 9.5 3 2.5-3 2.5M12.5 15.5H17" /></svg>;
  if (icon === 'files') return <svg viewBox="0 0 24 24"><path d="M3.5 6.5A1.5 1.5 0 0 1 5 5h4l2 2.5h8A1.5 1.5 0 0 1 20.5 9v8.5a1.5 1.5 0 0 1-1.5 1.5H5a1.5 1.5 0 0 1-1.5-1.5Z" /></svg>;
  return <svg viewBox="0 0 24 24"><path d="m8.5 8.5-4 3.5 4 3.5m7-7 4 3.5-4 3.5m-2-10-3 13" /></svg>;
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
  const chunkCount = Math.ceil(bytes.byteLength / attachmentChunkBytes);
  if (!['active', 'committed'].includes(upload.state) || upload.chunk_count !== chunkCount ||
    !Array.isArray(upload.received_sequences) || upload.received_sequences.some((sequence) =>
      !Number.isSafeInteger(sequence) || sequence < 0 || sequence >= chunkCount) ||
    new Set(upload.received_sequences).size !== upload.received_sequences.length) {
    throw new Error('附件上传进度无效，消息已保留，请重试。');
  }
  const received = new Set(upload.received_sequences);
  for (let offset = 0, sequence = 0; offset < bytes.byteLength; offset += attachmentChunkBytes, sequence += 1) {
    if (received.has(sequence)) continue;
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
      messages.push({ ...snapshot, sequence, agent_acknowledged: false, snapshotStatus: resolveTaskSnapshotStatus(task, snapshot.status) });
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

function isResourceNoLongerAccessible(error: unknown): boolean {
  return error instanceof ApiError && [401, 403, 404].includes(error.status);
}

const taskEventTypes = [
  'task.status', 'task.step', 'task.operation', 'task.message', 'task.batch_item',
  'task.artifact', 'task.waiting_user', 'task.response', 'task.result', 'task.plan', 'task.priority', 'task.snapshot_required',
] as const;

function stepTitle(step: Task['steps'][number]): string {
  return stepPresentationTitle(step.title);
}

function taskStepStatusText(status: string): string {
  switch (status) {
    case 'queued': return '等待执行';
    case 'running': return '进行中';
    case 'waiting_user': return '等待用户';
    case 'succeeded': return '已完成';
    case 'failed': return '失败';
    case 'cancelled': return '已取消';
    default: return '状态待确认';
  }
}

const taskStepOutputMaxBytes = 64 * 1024;
const taskStepOutputPreviewCharacters = 2_000;

function safeTaskStepOutput(value: unknown): { value: string; truncated: boolean } | null {
  if (typeof value !== 'string') return null;
  const redacted = safeTaskActivityText(value, taskStepOutputMaxBytes);
  let bytes = 0;
  let bounded = '';
  let byteTruncated = false;
  for (const character of redacted.value) {
    const point = character.codePointAt(0) ?? 0;
    const characterBytes = point <= 0x7f ? 1 : point <= 0x7ff ? 2 : point <= 0xffff ? 3 : 4;
    if (bytes + characterBytes > taskStepOutputMaxBytes) {
      byteTruncated = true;
      break;
    }
    bounded += character;
    bytes += characterBytes;
  }
  return { value: bounded, truncated: redacted.truncated || byteTruncated };
}

function TaskStepOutput({ label, value, clientTruncated, truncated, omittedBytes }: {
  label: string;
  value: string;
  clientTruncated: boolean;
  truncated: boolean;
  omittedBytes: number | null;
}) {
  let preview = '';
  let previewCharacters = 0;
  let hasMore = false;
  for (const character of value) {
    if (previewCharacters >= taskStepOutputPreviewCharacters) {
      hasMore = true;
      break;
    }
    preview += character;
    previewCharacters += 1;
  }
  return <div className="task-step-output">
    <p><strong>{label}</strong> {value ? preview : '（无输出）'}{hasMore ? '…' : ''}</p>
    {clientTruncated && <p role="status">已记录的{label}超过客户端 64 KiB 显示上限，本页内容已截断。</p>}
    {truncated && <p role="status">已记录的{label}有截断标记{omittedBytes === null ? '。' : `，省略 ${omittedBytes} 字节。`}</p>}
    {hasMore && <details>
      <summary>查看完整已记录输出</summary>
      <pre>{value}</pre>
    </details>}
  </div>;
}

function conversationToolActivity(message: Pick<ConversationMessage, 'role' | 'tool_activity'>): { tool: 'web_search' | 'web_read'; status: string } | null {
  if (message.role !== 'user') return null;
  const activity: unknown = message.tool_activity;
  if (!activity || typeof activity !== 'object' || Array.isArray(activity)) return null;
  const { tool, status } = activity as Record<string, unknown>;
  if ((tool !== 'web_search' && tool !== 'web_read') || typeof status !== 'string' ||
      !['queued', 'running', 'waiting_user', 'succeeded', 'failed', 'unknown', 'cancelled'].includes(status)) return null;
  return { tool, status };
}

function stepPresentationTitle(title: string): string {
  const mcpPrefix = 'Run cloud dsh MCP tool: ';
  const prefix = ['Run cloud dsh tool: ', mcpPrefix, 'Run Hostless Workspace tool: ']
    .find((value) => title.startsWith(value));
  if (!prefix) return title;
  const name = title.slice(prefix.length);
  if (name === 'morde_web_search' || name.includes('searxng_web_search')) return '搜索网页';
  if (name === 'morde_web_read' || name.includes('web_url_read')) return '读取网页';
  if (name === 'morde_workspace_list') return '查看电脑文件';
  if (name === 'morde_workspace_read') return '读取电脑文件';
  if (name === 'morde_workspace_search') return '搜索工作区文件';
  if (name === 'morde_workspace_write') return '保存电脑文件';
  if (['read_file', 'file_read', 'workspace_read_file'].includes(name)) return '读取电脑文件';
  if (['write_file', 'file_write', 'workspace_write_file'].includes(name)) return '保存电脑文件';
  if (['list_files', 'file_list', 'workspace_list_files'].includes(name)) return '查看电脑文件';
  if (['delete_file', 'file_delete', 'workspace_delete_file'].includes(name)) return '删除电脑文件';
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

type TaskAgent = NonNullable<Task['agents']>[number];
type TaskAgentActivityGroup = { id: string; title: string; currentActivity: string | null; steps: TaskStep[] };

function taskAgentCurrentActivity(agent: TaskAgent): string | null {
  if (!agent.activity_phase || !agent.activity_state) return null;
  const phaseLabels = { model: '模型', tool: '工具', subagent: '子 Agent', checkpoint: '检查点' } as const;
  const phaseLabel = phaseLabels[agent.activity_phase as keyof typeof phaseLabels];
  if (!phaseLabel) return null;
  const phaseActivity = {
    model: { started: '模型：正在响应', waiting: '模型：等待响应' },
    tool: { started: '工具：正在执行', waiting: '工具：等待结果' },
    subagent: { started: '子 Agent：正在运行', waiting: '子 Agent：等待响应' },
    checkpoint: { started: '检查点：正在保存', waiting: '检查点：等待服务端确认' },
  } as const;
  const terminal = { completed: '已完成', failed: '执行失败', cancelled: '已取消' } as const;
  const terminalLabel = terminal[agent.activity_state as keyof typeof terminal];
  if (terminalLabel) return `${phaseLabel}：${terminalLabel}`;
  return phaseActivity[agent.activity_phase as keyof typeof phaseActivity]?.[agent.activity_state as 'started' | 'waiting'] ?? null;
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
  const { notify, dismiss } = useToast();
  const [accessRevoked, setAccessRevoked] = useState(false);
  const queryKey = ['task-step-detail', actorID, serviceInstanceID, conversationID, task.id, step.id] as const;
  const detail = useQuery({
    queryKey,
    queryFn: async () => {
      const result = await portalApi.taskStepDetail(task.id, step.id) as TaskStepDetailWithExecution;
      if (result.task_id !== task.id || result.step_id !== step.id) throw new Error('详情身份不匹配');
      return result.detail_state === 'available' ? result : { ...result, object: null, input: null, result: null,
        execution: null, content_evidence: undefined };
    },
    enabled: Boolean(actorID && serviceInstanceID && conversationID) && !accessRevoked,
    retry: false,
    gcTime: 0,
    structuralSharing: (previous: unknown, next: unknown) => {
      const oldDetail = previous as TaskStepDetailWithExecution | undefined;
      const newDetail = next as TaskStepDetailWithExecution;
      if (newDetail.detail_state !== 'available') return newDetail;
      return oldDetail && oldDetail.task_id === newDetail.task_id && oldDetail.step_id === newDetail.step_id &&
        oldDetail.event_sequence > newDetail.event_sequence ? oldDetail : newDetail;
    },
    refetchInterval: (query) => !terminalTaskStatuses.has(task.status) ||
      (query.state.data && query.state.data.event_sequence < task.event_sequence) ? 1500 : false,
  });
  const refetchDetail = detail.refetch;
  const inaccessible = isResourceNoLongerAccessible(detail.error);
  if (inaccessible && !accessRevoked) setAccessRevoked(true);
  useEffect(() => {
    if (!accessRevoked) return;
    client.removeQueries({ queryKey: ['task-step-detail', actorID, serviceInstanceID, conversationID, task.id, step.id], exact: true });
  }, [accessRevoked, actorID, client, conversationID, serviceInstanceID, step.id, task.id]);
  useEffect(() => {
    if (!detail.isError || !isTransientNetworkError(detail.error)) return;
    let toastID = 0;
    toastID = notify('网络异常，步骤详情暂时无法读取。', 'error', {
      label: '重试',
      onClick: () => { void refetchDetail().then((result) => { if (!result.isError) dismiss(toastID); }); },
    });
    return () => dismiss(toastID);
  }, [detail.error, detail.isError, dismiss, notify, refetchDetail]);
  if (accessRevoked || inaccessible) return <p className="task-step-detail-message" role="status">详情不可访问或已删除。</p>;
  if (detail.isError && isTransientNetworkError(detail.error)) return <p className="task-step-detail-message" role="status">正在读取详情…</p>;
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
  const safeToolName = toolName ? safeTaskActivityText(toolName, 160) : null;
  const safeObject = object !== null ? safeTaskActivityText(object) : null;
  const safeInput = data.input !== null ? safeTaskActivityText(data.input) : null;
  const safeResult = data.result !== null ? safeTaskActivityText(data.result) : null;
  const contentEvidence = data.detail_state === 'available' && step.status === 'succeeded' &&
    data.content_evidence && typeof data.content_evidence === 'object' &&
    typeof data.content_evidence.preview === 'string' && data.content_evidence.preview.length > 0 &&
    data.content_evidence.preview.length <= 4_096 && /^[0-9a-f]{64}$/u.test(data.content_evidence.sha256) &&
    typeof data.content_evidence.truncated === 'boolean' ? data.content_evidence : null;
  const safeContentEvidence = contentEvidence ? safeTaskActivityText(contentEvidence.preview, 4_096) : null;
  const execution = data.detail_state === 'available' && data.execution && !Array.isArray(data.execution) &&
    typeof data.execution === 'object' ? data.execution : null;
  const isShell = data.tool_name === 'shell_exec';
  const executionText = (value: unknown) => typeof value === 'string' ? safeTaskActivityText(value, 8_192) : null;
  const executionNumber = (value: unknown) => typeof value === 'number' && Number.isSafeInteger(value) && value >= 0 ? value : null;
  const location = executionText(execution?.location);
  const command = executionText(execution?.command);
  const parameters = executionText(execution?.parameters);
  const cwd = executionText(execution?.working_directory);
  const hasStdout = typeof execution?.stdout === 'string';
  const hasStderr = typeof execution?.stderr === 'string';
  const confirmedResult = data.detail_state === 'available' && ['succeeded', 'failed'].includes(step.status);
  const safeStdout = confirmedResult ? safeTaskStepOutput(execution?.stdout) : null;
  const safeStderr = confirmedResult ? safeTaskStepOutput(execution?.stderr) : null;
  const duration = executionNumber(execution?.duration_ms);
  const exitCode = executionNumber(execution?.exit_code);
  const stdoutBytes = executionNumber(execution?.stdout_total_bytes);
  const stderrBytes = executionNumber(execution?.stderr_total_bytes);
  const stdoutOmitted = executionNumber(execution?.stdout_omitted_bytes);
  const stderrOmitted = executionNumber(execution?.stderr_omitted_bytes);
  return <div className="task-step-detail" aria-label="动作详情">
    {safeToolName && <p><strong>工具</strong> {safeToolName.value}</p>}
    <p><strong>时间</strong> {formatDate(step.created_at)}</p>
    {data.detail_state !== 'available' ? <p role="status">{unavailable[data.detail_state]}</p> : <>
      {safeObject && <p><strong>对象</strong> {safeObject.value || '（空）'}</p>}
      {safeInput && <p><strong>输入</strong> {safeInput.value || '（空）'}</p>}
      {safeResult && <p><strong>结果</strong> {safeResult.value || '（空结果）'}</p>}
      {safeContentEvidence && contentEvidence && <details className="task-step-web-evidence">
        <summary>查看已记录网页内容片段</summary>
        <p>这是工具返回的网页文本预览，不是结构化来源链接或独立事实核验。</p>
        <pre>{safeContentEvidence.value}</pre>
        {contentEvidence.truncated && <p>只显示有界预览；完整内容的 SHA-256 为 <code>{contentEvidence.sha256}</code>。</p>}
        {!contentEvidence.truncated && <p>完整记录的 SHA-256 为 <code>{contentEvidence.sha256}</code>。</p>}
      </details>}
      {execution ? <>
        {(location || isShell) && <p><strong>执行位置</strong> {location?.value || '未记录'}</p>}
        {(command || isShell) && <p><strong>命令</strong> {command?.value || '未记录'}</p>}
        {parameters && <p><strong>参数</strong> {parameters.value || '（空）'}</p>}
        {(cwd || isShell) && <p><strong>工作目录</strong> {cwd?.value || '未记录'}</p>}
        {(duration !== null || isShell) && <p><strong>耗时</strong> {duration === null ? '未记录' : `${duration} 毫秒`}</p>}
        {(exitCode !== null || isShell) && <p><strong>退出码</strong> {exitCode ?? '未记录'}</p>}
        {(typeof execution.timed_out === 'boolean' || isShell) && <p><strong>是否超时</strong> {typeof execution.timed_out === 'boolean' ? (execution.timed_out ? '是' : '否') : '未记录'}</p>}
        {safeStdout && <TaskStepOutput label="标准输出" value={safeStdout.value} clientTruncated={safeStdout.truncated}
          truncated={execution.stdout_truncated === true} omittedBytes={stdoutOmitted} />}
        {!safeStdout && (hasStdout || isShell) && <p><strong>标准输出</strong> 未记录</p>}
        {stdoutBytes !== null && <p><strong>标准输出字节数</strong> {stdoutBytes} 字节</p>}
        {safeStderr && <TaskStepOutput label="错误输出" value={safeStderr.value} clientTruncated={safeStderr.truncated}
          truncated={execution.stderr_truncated === true} omittedBytes={stderrOmitted} />}
        {!safeStderr && (hasStderr || isShell) && <p><strong>错误输出</strong> 未记录</p>}
        {stderrBytes !== null && <p><strong>错误输出字节数</strong> {stderrBytes} 字节</p>}
        {[location, command, parameters, cwd].some((field) => field?.truncated) && <p>执行信息在界面显示时截断。</p>}
      </> : isShell && <p>执行详情未记录或不可用。</p>}
      {data.object === null && data.input === null && data.result === null && !execution && !isShell && <p>没有可展示的输入或结果。</p>}
      {(data.truncated || [safeToolName, safeObject, safeInput, safeResult].some((field) => field?.truncated)) && <p>详情已截断。</p>}
    </>}
    {step.failure_reason === 'connector_rate_limited' && step.retry_after_at && Number.isFinite(Date.parse(step.retry_after_at)) && !step.retryable &&
      <p role="status">连接器限流，最早可在 {formatDate(step.retry_after_at)} 后手动重试。</p>}
  </div>;
}

function taskActivityIsExecuting(status: string): boolean {
  return status === 'running' || status === 'cancelling';
}

function taskStatusText(status: string): string {
  switch (status) {
    case 'queued': return '排队中';
    case 'running': return '执行中';
    case 'waiting_user': return '等待你处理';
    case 'pausing': return '正在暂停';
    case 'paused': return '已暂停';
    case 'cancelling': return '正在停止';
    case 'completed': return '已完成';
    case 'failed': return '失败';
    case 'cancelled': return '已取消';
    default: return '状态待确认';
  }
}

function taskStatusClass(status: string): string {
  switch (status) {
    case 'queued': case 'running': case 'waiting_user': case 'pausing': case 'paused':
    case 'cancelling': case 'completed': case 'failed': case 'cancelled':
      return `status-${status}`;
    default: return 'status-unknown';
  }
}

function resultReviewStatusText(review: K2ResultReview, replyVisible: boolean): string {
  switch (review.state) {
    case 'pending': return '原始结果已保存，等待主助手整理。';
    case 'processing': return '主助手正在整理原始结果。';
    case 'published': return replyVisible ? '最终回复已同步到会话。' : '最终回复已发布，正在从会话历史补取。';
    case 'failed': return '结果整理失败；原始任务结果仍保留，尚未发布最终回复。';
  }
}

function childJobStatusText(status: string): string {
  if (status === 'queued') return '等待执行';
  if (status === 'running') return '进行中';
  if (status === 'succeeded') return '已完成';
  if (status === 'failed') return '失败';
  if (status === 'cancelled') return '已停止';
  return '状态待确认';
}

function taskActivitySnippet(value: string, maximumCharacters: number): string | null {
  const safe = safeTaskActivityText(value, 1_000).value.replace(/\s+/gu, ' ').trim();
  if (!safe || /^(?:the model request could not be verified|the cloud dsh session completed|the task (?:failed|was interrupted)|(?:internal|runtime) error\b|traceback\b)/iu.test(safe)) return null;
  const first = safe.split(/(?<=[。！？!?；;，,：:\n])/u, 1)[0]?.trim();
  return first ? safeTaskActivityText(first, maximumCharacters).value : null;
}

function taskActivityStepTitle(step: TaskStep): string {
  const title = stepTitle(step);
  if (title === 'Review cloud task recovery') return '核对任务恢复状态';
  if (title === 'Review connector result') return '核对连接器结果';
  if (/^the model request could not be verified\b/iu.test(title)) return '模型请求未能确认';
  if (/^the cloud dsh session completed\b/iu.test(title)) return '任务会话已结束';
  if (/^the task (?:failed|was interrupted)\b/iu.test(title)) return '任务未完成';
  if (/^(?:internal|runtime) error\b|^traceback\b/iu.test(title)) return '运行步骤失败';
  return title;
}

function taskStepFailureText(step: TaskStep): string {
  switch (step.failure_reason) {
    case 'not_dispatched': return '工具未派发，当前记录未显示它已开始执行。';
    case 'tool_failed_after_dispatch': return '工具已派发后报告失败；展开查看已记录的执行详情。';
    case 'tool_execution_failed': return '工具执行已确认失败；请查看已记录的执行详情。';
    case 'side_effect_unknown': return '执行结果尚未确认，暂不提供重试；先核对已记录状态，避免重复操作。';
    case 'authorization_changed': return '执行期间授权状态发生变化；请核对权限和操作状态后再继续。';
    case 'resource_changed': return '目标资源状态发生变化；请先核对当前状态，再决定下一步。';
    case 'connector_rate_limited': return step.retry_after_at && Number.isFinite(Date.parse(step.retry_after_at))
      ? `连接器限流，最早可在 ${formatDate(step.retry_after_at)} 后手动重试。`
      : '连接器触发限流，重试时间尚未记录。';
    default: return '步骤失败，但当前记录没有可确认的失败原因。';
  }
}

function taskActivityStepSummary(step: TaskStep): string {
  const summary = step.summary.trim();
  const source = summary || taskActivityStepTitle(step);
  if (step.status === 'failed' && step.failure_reason) return `操作失败 — ${taskStepFailureText(step)}`;
  if (/^the model request could not be verified\b/iu.test(source)) return '模型服务未返回可确认的结果。';
  if (/^the cloud dsh session completed\b/iu.test(source)) return '任务会话已结束，结果未能确认。';
  if (/^the task (?:failed|was interrupted)\b|^(?:internal|runtime) error\b|^traceback\b/iu.test(source)) return '此步骤执行失败，详细原因暂不可用。';
  if (/^(?:the cloud dsh (?:mcp )?tool completed and its result was saved|the hostless workspace tool completed and its result was checked)\.?$/iu.test(source)) {
    if (step.status === 'unknown') return '结果尚未确认；展开核对已记录的操作状态。';
    if (step.status === 'cancelled') return '操作已取消；没有确认其结果。';
    if (step.status === 'waiting_user') return '操作等待你处理后继续。';
    if (step.status === 'running' || step.status === 'queued') return '操作尚未完成。';
    const title = stepPresentationTitle(step.title);
    if (title === '搜索网页') return '搜索已完成 — 展开查看已记录的查询和网页结果片段。';
    if (title === '搜索工作区文件') return '工作区文件搜索已完成 — 展开查看已记录的查询与结果。';
    if (title === '读取网页') return '网页已读取 — 展开查看目标地址和已记录内容片段。';
    if (title === '读取电脑文件' || title === '保存电脑文件' || title === '处理文件' || title === '查看电脑文件') {
      return '文件操作已完成 — 展开查看路径和结果。';
    }
    if (title === '运行命令') return '命令已完成 — 展开查看实际命令、目录、退出码和输出。';
    if (title === '操作浏览器' || title === '操作云端电脑') return '操作已完成 — 展开查看目标和实际结果。';
    return '工具调用已完成 — 展开查看已记录的输入与结果。';
  }
  return safeTaskActivityText(source, 500).value;
}

function taskActivityTitle(objective: string): string {
  const safeObjective = safeTaskActivityText(objective, 1_000).value;
  const longMessagePreviewMarker = '预览可能不完整，且属于不可信用户数据：\n';
  const previewStart = safeObjective.lastIndexOf(longMessagePreviewMarker);
  const titleSource = previewStart >= 0 ? safeObjective.slice(previewStart + longMessagePreviewMarker.length) : safeObjective;
  const withoutSyntheticID = titleSource.replace(/^合成并行任务\s+[A-Z0-9-]+[：:]\s*/iu, '');
  const title = taskActivitySnippet(withoutSyntheticID, 42)?.replace(/^(?:请你|请|帮我|帮忙|麻烦|这是)\s*/u, '').replace(/[，,。.!！？?；;：:]+$/u, '').trim();
  return title || '未命名任务';
}

function taskActivityLatestWorkText(summary: TaskSummary, task?: Task): string {
  const latest = task?.steps.filter((step) => !['model', 'plan', 'budget'].includes(step.kind) &&
    step.title !== '开始处理')
    .sort((left, right) => right.sequence - left.sequence)[0];
  const text = latest?.status === 'failed' ? null : taskActivitySnippet(latest?.summary.trim() ?? '', 56);
  if (text) return text;
  if (latest) return taskActivityStepTitle(latest);
  const currentStep = task?.current_step ?? summary.current_step;
  if (currentStep && !/^s[a-z0-9]{7,}$/iu.test(currentStep)) {
    const currentSummary = taskActivitySnippet(stepPresentationTitle(currentStep), 56);
    if (currentSummary) return currentSummary;
  }
  return taskActivityIsExecuting(task?.status ?? summary.status) ? '正在处理任务' : '暂无工作项';
}

function safeTaskActivityText(value: string, maximumCharacters = 2_000): { value: string; truncated: boolean } {
  const redacted = value
    .replace(/(["']?(?:password|passwd|pwd|secret|token|api[\s_-]?key|authorization|auth[\s_-]?token|credential|credentials|client[\s_-]?secret|private[\s_-]?key|access[\s_-]?key|refresh[\s_-]?token)["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,}\]]+)/giu, '$1[已隐藏]')
    .replace(/\bBearer\s+[^\s,}\]]+/giu, 'Bearer [已隐藏]');
  const characters = Array.from(redacted);
  if (characters.length <= maximumCharacters) return { value: redacted, truncated: false };
  return { value: `${characters.slice(0, maximumCharacters - 1).join('')}…`, truncated: true };
}

function taskAgentActivityGroups(task: Task): TaskAgentActivityGroup[] {
  const steps = task.steps.filter((step) => !['model', 'plan', 'budget'].includes(step.kind) &&
    step.title !== '开始处理')
    .sort((left, right) => left.sequence - right.sequence);
  const unassigned = (items: TaskStep[]): TaskAgentActivityGroup[] => items.length
    ? [{ id: 'unassigned', title: '归属未确认', currentActivity: null, steps: items }] : [];
  const main = (items: TaskStep[]): TaskAgentActivityGroup[] => items.length
    ? [{ id: 'main', title: 'MAIN', currentActivity: null, steps: items }] : [];
  const fallback = (items: TaskStep[]): TaskAgentActivityGroup[] => [
    ...main(items.filter((step) => !step.agent_id)),
    ...unassigned(items.filter((step) => Boolean(step.agent_id))),
  ];
  const agents = task.agents ?? [];
  if (!agents.length) return fallback(steps);

  const byID = new Map(agents.map((agent) => [agent.id, agent]));
  const roots = agents.filter((agent) => agent.parent_id === null && agent.role === 'main');
  if (byID.size !== agents.length || roots.length !== 1 || agents.some((agent) =>
    !agent.id || agent.task_revision < 0 || (agent.parent_id === null ? agent.role !== 'main' : agent.role !== 'subagent' || !byID.has(agent.parent_id))))
    return fallback(steps);

  const byStepID = new Map(steps.map((step) => [step.id, step]));
  const children = agents.filter((agent) => agent.parent_id !== null).sort((left, right) => {
    const leftSequence = left.step_id ? byStepID.get(left.step_id)?.sequence ?? Number.MAX_SAFE_INTEGER : Number.MAX_SAFE_INTEGER;
    const rightSequence = right.step_id ? byStepID.get(right.step_id)?.sequence ?? Number.MAX_SAFE_INTEGER : Number.MAX_SAFE_INTEGER;
    return leftSequence - rightSequence || left.id.localeCompare(right.id);
  });
  const visited = new Set<string>();
  const groups: TaskAgentActivityGroup[] = [];
  let invalidTree = false;
  const append = (agent: TaskAgent, path: number[]) => {
    if (visited.has(agent.id)) { invalidTree = true; return; }
    visited.add(agent.id);
    const ownSteps = steps.filter((step) => step.agent_id === agent.id || (agent.role === 'main' && !step.agent_id));
    const currentActivity = taskAgentCurrentActivity(agent);
    if (ownSteps.length || currentActivity) groups.push({ id: agent.id,
      title: path.length ? `SUBAGENT ${path.map((part) => String(part).padStart(2, '0')).join('.')}` : 'MAIN',
      currentActivity, steps: ownSteps });
    children.filter((item) => item.parent_id === agent.id).forEach((child, index) => append(child, [...path, index + 1]));
  };
  append(roots[0]!, []);
  if (invalidTree || visited.size !== agents.length) return fallback(steps);
  const assignedStepIDs = new Set(groups.flatMap((group) => group.steps.map((step) => step.id)));
  return [...groups, ...unassigned(steps.filter((step) => Boolean(step.agent_id) && !assignedStepIDs.has(step.id)))];
}

type TaskFeedState = 'connected' | 'disconnected' | 'resyncing';

function TaskStream({ task, conversationID, onState }: {
  task: Task; conversationID: string; onState: (id: string, state: TaskFeedState) => void;
}) {
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
    const resync = (reconnectDelay = 0) => {
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
          void client.invalidateQueries({ queryKey: ['task-step-detail'], predicate: (query) => query.queryKey[4] === task.id });
          const reconnect = () => setGeneration((value) => value + 1);
          if (reconnectDelay) retry = setTimeout(reconnect, reconnectDelay);
          else reconnect();
        }
      }).catch((error: unknown) => {
        if (!disposed) {
          onState(task.id, 'disconnected');
          if (!isResourceNoLongerAccessible(error)) retry = setTimeout(() => setGeneration((value) => value + 1), 3000);
        }
      });
    };
    source.onopen = () => { if (!disposed && !resyncing) onState(task.id, 'connected'); };
    source.onerror = () => resync(1500);
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
        void client.invalidateQueries({ queryKey: ['task-step-detail'], predicate: (query) => query.queryKey[4] === task.id });
        if (type === 'task.status' || type === 'task.waiting_user') {
          void client.invalidateQueries({ queryKey: ['assistant-account-tasks'] });
          void client.invalidateQueries({ queryKey: ['conversation-tasks', conversationID] });
        }
        const revisionStarted = type === 'task.step' && 'payload' in value && value.payload !== null &&
          typeof value.payload === 'object' && 'phase' in value.payload && value.payload.phase === 'started';
        if (revisionStarted || type === 'task.message' || type === 'task.result' || type === 'task.response') {
          void client.invalidateQueries({ queryKey: ['conversation-messages', conversationID] });
        }
      });
    }
    return () => { disposed = true; if (retry) clearTimeout(retry); source.close(); };
  }, [client, conversationID, generation, onState, task.id, terminal]);
  return null;
}

export function Assistant() {
  const outletContext = useOutletContext<AssistantOutletContext | null>();
  const client = useQueryClient();
  const { notify, dismiss } = useToast();
  const { user } = useAuth();
  const location = useLocation();
  const navigate = useNavigate();
  const navigationIdea = typeof location.state === 'object' && location.state !== null
    ? location.state as { ideaID?: unknown; ideaPrompt?: unknown; conversationID?: unknown; taskID?: unknown }
    : null;
  const ideaPrompt = typeof navigationIdea?.ideaPrompt === 'string' ? navigationIdea.ideaPrompt : '';
  const hasNavigationIdea = typeof navigationIdea?.ideaID === 'string' && Boolean(navigationIdea.ideaID) && Boolean(ideaPrompt);
  const navigationConversationID = typeof navigationIdea?.conversationID === 'string' && /^c[a-z0-9]{7}$/.test(navigationIdea.conversationID)
    ? navigationIdea.conversationID : null;
  const navigationTaskID = typeof navigationIdea?.taskID === 'string' && /^q[a-z0-9]{7}$/.test(navigationIdea.taskID)
    ? navigationIdea.taskID : null;
  const [conversationSelection, setConversationSelection] = useState<{ scope: string; id: string | null } | null>(null);
  const [composerDraftError, setComposerDraftError] = useState<string | null>(null);
  const [hydratedComposerDraftKey, setHydratedComposerDraftKey] = useState<string | null>(null);
  const observedProcessingRetryStates = useRef(new Map<string, string>());
  const processingRetryInFlight = useRef(new Set<string>());
  const currentRetryIdentity = useRef<{ actorID?: string; serviceInstanceID?: string }>({});
  const [modelID, setModelID] = useState('');
  const [mcpConnectorIDs, setMcpConnectorIDs] = useState<string[]>([]);
  const [content, setContent] = useState(() => hasNavigationIdea ? ideaPrompt : '');
  const [savingMessage, setSavingMessage] = useState(false);
  const [composerAttachments, setComposerAttachments] = useState<ComposerAttachment[]>([]);
  const [officePreviews, setOfficePreviews] = useState<Record<string, OfficePreviewState>>({});
  const [preparingAttachmentCount, setPreparingAttachmentCount] = useState(0);
  const [preparingAttachmentIDsForRender, setPreparingAttachmentIDsForRender] = useState<ReadonlySet<string>>(() => new Set());
  const [taskDetailTaskID, setTaskDetailTaskID] = useState<string | null>(null);
  const [taskDetailStepID, setTaskDetailStepID] = useState<string | null>(null);
  const [visibleTaskPanelCount, setVisibleTaskPanelCount] = useState(5);
  const [pendingTaskActions, setPendingTaskActions] = useState<Map<string, { fingerprint: string; key: string }>>(() => new Map());
  const [clarificationAnswers, setClarificationAnswers] = useState<Record<string, string>>({});
  const [clarificationFileDeliveryIntents, setClarificationFileDeliveryIntents] = useState<Record<string, 'none' | 'required'>>({});
  const [clarificationInteractions, setClarificationInteractions] = useState<Record<string, ClarificationInteraction>>({});
  const clarificationSubmissionsInFlight = useRef(new Set<string>());
  const [taskEventFeeds, setTaskEventFeeds] = useState<Record<string, TaskFeedState>>({});
  const setTaskEventFeed = useCallback((id: string, state: TaskFeedState) => {
    setTaskEventFeeds((current) => current[id] === state ? current : { ...current, [id]: state });
  }, []);
  const handledNavigationTaskID = useRef<string | null>(null);
  const [enableExecutionConfirmationOpen, setEnableExecutionConfirmationOpen] = useState(false);
  const enableExecutionAttempt = useRef<{ userID: string; epoch: number; key: string } | null>(null);
  const [standaloneTaskPanelTab, setStandaloneTaskPanelTab] = useState<AssistantPanelTab>('tasks');
  const [standaloneTaskPanelOpen, setStandaloneTaskPanelOpen] = useState(false);
  const [standaloneWorkspaceWidth, setStandaloneWorkspaceWidth] = useState<number | null>(null);
  const taskPanelTab = outletContext?.panelTab ?? standaloneTaskPanelTab;
  const taskPanelOpen = outletContext?.panelOpen ?? standaloneTaskPanelOpen;
  const workspacePanelWidth = outletContext?.workspaceWidth ?? standaloneWorkspaceWidth;
  const setTaskPanelTab = outletContext?.setPanelTab ?? setStandaloneTaskPanelTab;
  const setTaskPanelOpen = outletContext?.setPanelOpen ?? setStandaloneTaskPanelOpen;
  const focusPanelToggle = outletContext?.focusPanelToggle;
  const setWorkspacePanelWidth = outletContext?.setWorkspaceWidth ?? setStandaloneWorkspaceWidth;
  const [conversationSearchVisible, setConversationSearchVisible] = useState(false);
  const [contextSidebarWidth, setContextSidebarWidth] = useState(210);
  const [dividerMeasurements, setDividerMeasurements] = useState({ contextSidebarMaximum: 420, workspacePanelMaximum: 900, workspacePanelWidth: 560 });
  const assistantLayoutRef = useRef<HTMLDivElement>(null);
  const assistantDividerDrag = useRef<{ side: 'sidebar' | 'workspace'; pointerID: number; x: number; width: number } | null>(null);
  const [computerEmbed, setComputerEmbed] = useState<ComputerEmbedSession | null>(null);
  const [computerControlExpanded, setComputerControlExpanded] = useState(false);
  const [computerConnecting, setComputerConnecting] = useState(false);
  const [computerPanelError, setComputerPanelError] = useState('');
  const computerFrame = useRef<HTMLIFrameElement>(null);
  const computerFrameBounds = useRef<HTMLDivElement>(null);
  const computerEmbedRef = useRef<ComputerEmbedSession | null>(null);
  const computerRequestGeneration = useRef(0);
  const submittedEmbedNonce = useRef<string | null>(null);
  const initializedEmbedNonce = useRef<string | null>(null);
  const computerCommandSequences = useRef({ selection: 0, lock: 0 });
  const computerAutoConnectKey = useRef('');
  const computerControlReport = useRef('');
  const computerLockAck = useRef<ComputerLockAck | null>(null);
  const computerLockRequested = useRef<{ nonce: string; locked: boolean; seq: number } | null>(null);
  useLayoutEffect(() => { computerEmbedRef.current = computerEmbed; }, [computerEmbed]);
  const sendComputerCommand = useCallback(async (session: ComputerEmbedSession, command: ComputerEmbedCommand): Promise<boolean> => {
    if (session.native) {
      const send = nativeWorkbenchBridge()?.sendHostWorkbenchCommand;
      if (!send) return false;
      try {
        const result = await send(session.nonce, command);
        return Boolean(result && typeof result === 'object' && (result as { sent?: unknown }).sent === true);
      } catch { return false; }
    }
    const target = computerFrame.current?.contentWindow;
    if (!target) return false;
    target.postMessage({ channel: 'morde.workbench.embed', version: 1, nonce: session.nonce, ...command }, session.hostOrigin);
    return true;
  }, []);
  const setComputerControlLock = useCallback(async (locked: boolean, waitForAck = false): Promise<boolean> => {
    const session = computerEmbedRef.current;
    if (!session || session.status !== 'ready' || initializedEmbedNonce.current !== session.nonce) return false;
    const pending = computerLockAck.current;
    if (pending?.nonce === session.nonce && pending.locked === locked) {
      return waitForAck ? pending.promise : true;
    }
    if (computerLockRequested.current?.nonce === session.nonce && computerLockRequested.current.locked === locked) {
      return session.controlLocked === locked && session.lockSeq === computerLockRequested.current.seq || !waitForAck;
    }
    if (pending) {
      window.clearTimeout(pending.timer);
      pending.resolve(false);
      computerLockAck.current = null;
    }
    const seq = ++computerCommandSequences.current.lock;
    let resolveAck: (acknowledged: boolean) => void = () => undefined;
    const acknowledged = new Promise<boolean>((resolve) => { resolveAck = resolve; });
    const timer = window.setTimeout(() => {
      if (computerLockAck.current?.seq !== seq) return;
      computerLockAck.current = null;
      resolveAck(false);
    }, 2500);
    computerLockAck.current = { nonce: session.nonce, seq, locked, resolve: resolveAck, promise: acknowledged, timer };
    computerLockRequested.current = { nonce: session.nonce, locked, seq };
    const sent = await sendComputerCommand(session, { type: 'control_lock', locked, seq });
    if (!sent) {
      const current = computerLockAck.current;
      if (current?.seq === seq) {
        window.clearTimeout(current.timer);
        computerLockAck.current = null;
        current.resolve(false);
      }
      computerLockRequested.current = null;
      return false;
    }
    return waitForAck ? acknowledged : true;
  }, [sendComputerCommand]);
  const postComputerVisibility = useCallback((visible: boolean) => {
    const session = computerEmbedRef.current;
    if (!session || initializedEmbedNonce.current !== session.nonce) return;
    void sendComputerCommand(session, { type: 'visibility', conversation_id: session.conversationID, visible });
  }, [sendComputerCommand]);
  const taskCardButtons = useRef(new Map<string, HTMLButtonElement>());
  const [sideConversationsOpen, setSideConversationsOpen] = useState(false);
  const [conversationSearch, setConversationSearch] = useState('');
  const [showJumpToLatest, setShowJumpToLatest] = useState(false);
  const [selectedMessageVariantVersions, setSelectedMessageVariantVersions] = useState<Record<string, number>>({});
  const [historyPageLoadVersion, setHistoryPageLoadVersion] = useState(0);
  const pendingMessageSave = useRef<Promise<void>>(Promise.resolve());
  const savingMessageRef = useRef(false);
  const preparingAttachmentCountRef = useRef(0);
  const attachmentPreparationCounts = useRef(new Map<string, number>());
  const attachmentPreparationGenerations = useRef(new Map<string, number>());
  const pdfPreparationControllers = useRef(new Map<string, Set<AbortController>>());
  const preparingAttachmentIDsByDraft = useRef(new Map<string, Set<string>>());
  const preparingAttachmentIDs = useRef(new Set<string>());
  const sendingMessageIDs = useRef(new Set<string>());
  const newConversationKey = useRef<string | null>(null);
  const ensureMainKey = useRef<{ userID: string; key: string } | null>(null);
  const ensureAttemptedUserID = useRef<string | null>(null);
  const ensureMainErrorToastID = useRef<number | null>(null);
  const conversationView = useRef<HTMLDivElement>(null);
  const olderMessagesAnchor = useRef<{ conversationID: string; height: number; top: number } | null>(null);
  const conversationScrollPositions = useRef(new Map<string, number>());
  const previousScrollPositionKey = useRef<string | null>(null);
  const pendingScrollRestoreKey = useRef<string | null>(null);
  const historyLoadsInFlight = useRef(new Set<string>());
  const variantScrollAnchor = useRef<number | null>(null);
  const sendingVariantOperationKeys = useRef(new Set<string>());
  const activeComposerDraftIdentity = useRef<string | null>(null);
  const composerDraftWrite = useRef<Promise<void>>(Promise.resolve());
  const composerEditRevision = useRef(0);
  const composerSwitchGeneration = useRef(0);
  const activeDraftLoad = useRef<string | null>(null);
  const localConversationKeyRef = useRef<string | null>(null);
  const taskPanelToggle = useRef<HTMLButtonElement>(null);
  const composerTextarea = useRef<HTMLTextAreaElement>(null);
  const followConversation = useRef(true);
  useEffect(() => () => {
    for (const controllers of pdfPreparationControllers.current.values()) for (const controller of controllers) controller.abort();
    pdfPreparationControllers.current.clear();
  }, []);
  const [visibleSideConversationCount, setVisibleSideConversationCount] = useState(5);
  const capabilities = useQuery({ queryKey: ['service-capabilities'], queryFn: portalApi.capabilities });
  useLayoutEffect(() => {
    currentRetryIdentity.current = { actorID: user?.id, serviceInstanceID: capabilities.data?.service_instance_id };
  }, [capabilities.data?.service_instance_id, user?.id]);
  const outbox = useMessageOutbox(user?.id, capabilities.data?.service_instance_id);
  const variantOutbox = useMessageVariantOutbox(user?.id, capabilities.data?.service_instance_id);
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
  const makeMainConversationMutation = useMutation({
    mutationFn: ({ conversationID, expectedVersion, idempotencyKey }: { conversationID: string; expectedVersion: number; idempotencyKey: string }) =>
      portalApi.setMainConversation(conversationID, expectedVersion, idempotencyKey),
    onSuccess: (state) => {
      client.setQueryData(['main-conversation', user?.id], state);
      notify('已恢复主会话；其他对话和消息未更改。', 'success');
    },
    onError: () => {
      notify('主会话更新结果尚未确认，正在核对当前状态。', 'error');
      void mainConversation.refetch();
    },
  });
  const makeMainConversation = (conversationID: string) => {
    const state = mainConversation.data;
    if (state?.status !== 'selection_required' || makeMainConversationMutation.isPending) return;
    makeMainConversationMutation.mutate({ conversationID, expectedVersion: state.version, idempotencyKey: newIdempotencyKey() });
  };
  const mainConversationCacheKey = user ? assistantMainConversationCacheKey(user.id) : null;
  const cachedMainConversation = useQuery({
    queryKey: ['assistant-main-conversation-cache', mainConversationCacheKey],
    queryFn: () => mainConversationCacheKey ? readMainConversationCache(mainConversationCacheKey) : null,
    enabled: Boolean(mainConversationCacheKey),
    initialData: () => mainConversationCacheKey ? readMainConversationCache(mainConversationCacheKey) : null,
    staleTime: Infinity,
  });
  const models = useQuery({ queryKey: ['model-connections'], queryFn: portalApi.modelConnections, staleTime: 60_000, refetchOnMount: 'always' });
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
  const enableExecution = useMutation({
    mutationFn: async ({ ownerID, epoch, key }: { ownerID: string; epoch: number; key: string }) => {
      const state = await portalApi.enableExecution(epoch, key);
      if (state.owner_id !== ownerID || state.state !== 'enabled' || state.epoch < epoch) {
        throw new Error('服务器未确认云端执行权限已启用。');
      }
      return state;
    },
    onSuccess: (state) => {
      client.setQueryData(['execution-control'], state);
      setEnableExecutionConfirmationOpen(false);
      notify('已启用云端任务；执行仍受现有授权范围和审批规则约束。', 'success');
    },
    onError: () => {
      notify('启用结果尚未确认，正在读取最新状态。', 'error');
      void execution.refetch().then(({ data }) => {
        if (data && user?.id === data.owner_id && data.state === 'enabled') setEnableExecutionConfirmationOpen(false);
      });
    },
  });
  const conversationsData = [...new Map((conversations.data?.pages.flatMap((page) => page.items) ?? [])
    .map((item) => [item.id, item])).values()]
    .sort((left, right) => Date.parse(right.updated_at) - Date.parse(left.updated_at) || right.id.localeCompare(left.id));
  const ensureMainConversation = useMutation({
    mutationFn: portalApi.ensureMainConversation,
    onSuccess: (state) => {
      client.setQueryData(['main-conversation', user?.id], state);
      if (ensureMainErrorToastID.current !== null) dismiss(ensureMainErrorToastID.current);
      ensureMainErrorToastID.current = null;
    },
    onError: (error) => {
      if (ensureMainErrorToastID.current !== null) dismiss(ensureMainErrorToastID.current);
      ensureMainErrorToastID.current = notify(errorDetails(error).message, 'error', {
        label: '重试',
        onClick: () => {
          const attempt = ensureMainKey.current;
          if (attempt && attempt.userID === user?.id) ensureMainConversation.mutate(attempt.key);
        },
      });
    },
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
  const scopedConversationSelection = conversationSelection?.scope === conversationScope ? conversationSelection
    : navigationConversationID && user?.id && capabilities.data?.service_instance_id
      ? { scope: conversationScope, id: navigationConversationID } : null;
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
  useEffect(() => {
    if (!navigationTaskID || !navigationConversationID || conversationID !== navigationConversationID ||
      handledNavigationTaskID.current === navigationTaskID) return;
    handledNavigationTaskID.current = navigationTaskID;
    setTaskDetailStepID(null);
    setTaskDetailTaskID(navigationTaskID);
    setTaskPanelTab('tasks');
    setTaskPanelOpen(true);
    navigate('/assistant', { replace: true, state: { conversationID: navigationConversationID } });
  }, [conversationID, navigate, navigationConversationID, navigationTaskID, setTaskPanelOpen, setTaskPanelTab]);
  const conversationScrollPositionKey = conversationID ? JSON.stringify([conversationScope, conversationID]) : null;
  const [panelConversationID, setPanelConversationID] = useState(conversationID);
  if (panelConversationID !== conversationID) {
    setPanelConversationID(conversationID);
    setTaskDetailStepID(null);
    setTaskDetailTaskID(null);
    setComputerConnecting(false);
    setComputerEmbed(null);
    setComputerControlExpanded(false);
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
  const localDraftItems = new Map<string, Pick<AssistantComposerDraft, 'id' | 'content' | 'updatedAt'> & { pendingMessage?: boolean }>();
  for (const draft of composerDrafts.data ?? []) {
    if (draft.conversationID === null && draft.id.startsWith('local:') && (draft.content.trim() || draft.attachments.length)) {
      localDraftItems.set(draft.id, draft);
    }
  }
  for (const { message } of outbox.entries) {
    if (message.projectID || message.conversationID || !message.conversationCreateKey) continue;
    const id = `local:${message.conversationCreateKey}`;
    if (!localDraftItems.has(id)) {
      localDraftItems.set(id, { id, content: message.request.content, updatedAt: message.createdAt, pendingMessage: true });
    }
  }
  const localComposerDrafts = [...localDraftItems.values()].sort((left, right) => right.updatedAt - left.updatedAt);
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
    content, modelID, hostID: '', mcpConnectorIDs: [...mcpConnectorIDs],
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
      if (draft) restoreComposerDraft(
        preserveUnboundInput && composerInputSnapshot.current.content && !draft.content
          ? { ...draft, content: composerInputSnapshot.current.content }
          : draft,
        composerDraftScope, composerDraftID,
      );
      else {
        if (!preserveUnboundInput) {
          setContent('');
          setModelID('');
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
  }, [composerDraftIdentity, composerDraftScope, content, modelID, mcpConnectorIDs, composerAttachments, officePreviews, hydratedComposerDraftKey, persistComposerDraft]);
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
    refetchInterval: (query) => !isResourceNoLongerAccessible(query.state.error) && query.state.data?.pages.some((page) => page.items.some((item) =>
      item.routing_status === 'pending' || item.routing_status === 'processing' || item.routing_status === 'waiting_adjustment' ||
      ['queued', 'running', 'waiting_user'].includes(conversationToolActivity(item)?.status ?? ''))) ? 2000 : false,
  });
  const conversationContentAccessRevoked = isResourceNoLongerAccessible(messages.error);
  const conversationMessageItems = conversationContentAccessRevoked ? [] : flattenConversationPages(messages.data);
  const routingRetryCapabilityConfirmed = capabilities.isSuccess && !capabilities.isError && !capabilities.isFetching
    && Boolean(capabilities.data?.service_instance_id)
    && capabilities.data.supported_features.includes('conversation_auto_routing_v1');
  const routingRetryCapabilityState: 'confirmed' | 'unsupported' | 'unknown' = !capabilities.isSuccess || capabilities.isError
    || capabilities.isFetching || !capabilities.data?.service_instance_id ? 'unknown'
      : routingRetryCapabilityConfirmed ? 'confirmed' : 'unsupported';
  const routingRetryCandidateMessages = conversationMessageItems.filter((item) =>
    isUnlinkedRoutingFailure(item) && Boolean(user?.id) && conversationMessageActorID(item) === user?.id);
  const routingRetryEligibilityQueries = useQueries({ queries: routingRetryCandidateMessages.map((item) => {
    const actorID = user!.id;
    const serviceInstanceID = capabilities.data?.service_instance_id ?? '';
    return {
      queryKey: ['conversation-message-retry-eligibility', item.conversation_id, item.id, actorID, serviceInstanceID],
      queryFn: () => portalApi.conversationMessageRetryEligibility(item.conversation_id, item.id, serviceInstanceID, actorID),
      enabled: Boolean(routingRetryCapabilityConfirmed && serviceInstanceID),
      staleTime: 5_000,
      retry: false,
      refetchOnWindowFocus: false,
    };
  }) });
  const routingRetryEligibilityByMessageID = new Map(routingRetryCandidateMessages.map((item, index) =>
    [item.id, routingRetryEligibilityQueries[index]] as const));
  const [unknownRetryConfirmation, setUnknownRetryConfirmation] = useState<{ messageID: string; warning: string } | null>(null);
  const retryConversationMessageProcessing = useMutation({
    mutationFn: async (targetInput: { messageID: string; confirmedWarning?: string }): Promise<{ conversationID: string; messageID: string } | null> => {
      const { messageID, confirmedWarning } = targetInput;
      if (processingRetryInFlight.current.has(messageID)) return null;
      const target = conversationMessageItems.find((item) => item.id === messageID && item.conversation_id === conversationID);
      const actorID = user?.id;
      const serviceInstanceID = capabilities.data?.service_instance_id;
      if (!target || !isUnlinkedRoutingFailure(target) || target.routing_failure_reason === 'message_too_long'
        || !actorID || conversationMessageActorID(target) !== actorID
        || !capabilities.isSuccess || capabilities.isError || capabilities.isFetching || !serviceInstanceID
        || !capabilities.data.supported_features.includes('conversation_auto_routing_v1')) {
        throw new Error('此消息、账号或服务状态尚未通过安全重试检查。');
      }
      processingRetryInFlight.current.add(messageID);
      try {
        const [currentCapabilities, currentEligibility] = await Promise.all([
          portalApi.capabilities(),
          portalApi.conversationMessageRetryEligibility(target.conversation_id, target.id, serviceInstanceID, actorID),
        ]);
        if (currentCapabilities.service_instance_id !== serviceInstanceID
          || !currentCapabilities.supported_features.includes('conversation_auto_routing_v1')) {
          throw new Error('服务身份已变化或尚不支持消息处理；未发送重试请求。');
        }
        if (currentRetryIdentity.current.actorID !== actorID || currentRetryIdentity.current.serviceInstanceID !== serviceInstanceID) {
          throw new Error('账号或服务身份已变化；未发送重试请求。');
        }
        if (!retryEligibilityAllowsDispatch(currentEligibility)) {
          const unknownWarning = currentEligibility.retry_eligible && currentEligibility.retry_reason === 'retry_available'
            && currentEligibility.retry_warning;
          if (unknownWarning) {
            throw new Error('服务端返回尚未支持的重试警告；为避免重复操作，暂不重试。');
          }
          throw new Error(retryEligibilityReasonText(currentEligibility.retry_reason));
        }
        if (currentEligibility.retry_warning && currentEligibility.retry_warning !== confirmedWarning) {
          setUnknownRetryConfirmation({ messageID, warning: currentEligibility.retry_warning });
          return null;
        }
        const key = processingRetryKey(serviceInstanceID, actorID, '', target.conversation_id, target.id);
        await portalApi.retryConversationMessageProcessing(target.conversation_id, target.id, key, serviceInstanceID, actorID);
        // A confirmed 202 has consumed this key. If the new attempt fails
        // before the next poll, its next retry must use a fresh key.
        try {
          clearProcessingRetryKey(serviceInstanceID, actorID, '', target.conversation_id, target.id);
        } catch {
          // A storage error must not turn a confirmed acceptance into a failure.
        }
        return { conversationID: target.conversation_id, messageID: target.id };
      } finally {
        processingRetryInFlight.current.delete(messageID);
      }
    },
    onSuccess: (target) => {
      if (target) {
        void client.invalidateQueries({ queryKey: ['conversation-messages', target.conversationID] });
        void client.invalidateQueries({ queryKey: ['conversation-message-retry-eligibility', target.conversationID, target.messageID] });
      }
    },
    onError: (error, target) => {
      void client.invalidateQueries({ queryKey: ['conversation-message-retry-eligibility', conversationID, target.messageID] });
      notify(errorDetails(error).message, 'error');
    },
  });
  const conversationTasks = useQuery({
    queryKey: ['conversation-tasks', conversationID],
    queryFn: () => portalApi.conversationTasks(conversationID!),
    enabled: Boolean(conversationID),
    refetchInterval: 5000,
  });
  const accountTasks = useQuery({
    queryKey: ['assistant-account-tasks'], queryFn: () => portalApi.tasks(),
    enabled: taskPanelOpen && taskPanelTab === 'tasks', refetchInterval: 5000,
  });
  const waitingAccountTasks = useQuery({
    queryKey: ['assistant-account-tasks', 'waiting_user'],
    queryFn: () => portalApi.tasks({ status: 'waiting_user' }),
    enabled: Boolean(user?.id), refetchInterval: (query) => query.state.data?.items.length ? 5000 : 30000,
  });
  const recentConversationTasks = useQuery({
    queryKey: ['conversation-tasks', conversationID, 'recent'],
    queryFn: () => portalApi.conversationTasks(conversationID!, { view: 'recent' }),
    enabled: Boolean(conversationID) && taskPanelOpen,
    refetchInterval: 30000,
  });
  const conversationTaskItems = conversationContentAccessRevoked ? [] : conversationTasks.data?.items ?? [];
  const recentConversationTaskItems = conversationContentAccessRevoked ? [] : recentConversationTasks.data?.items ?? [];
  const taskPanelItemsByID = new Map<string, TaskSummary>();
  for (const item of accountTasks.data?.items ?? []) taskPanelItemsByID.set(item.id, item);
  for (const item of waitingAccountTasks.data?.items ?? []) taskPanelItemsByID.set(item.id, item);
  for (const item of [...conversationTaskItems, ...recentConversationTaskItems]) taskPanelItemsByID.set(item.id, item);
  const taskPanelSummaryItems = [...taskPanelItemsByID.values()].sort((left, right) =>
    Number(taskActivityIsExecuting(right.status)) - Number(taskActivityIsExecuting(left.status)) || Date.parse(right.updated_at) - Date.parse(left.updated_at));
  const taskPanelPreviewTaskIDs = new Set(taskPanelSummaryItems.slice(0, visibleTaskPanelCount).map((item) => item.id));
  const taskIDs = [...new Set([
    ...conversationTaskItems.map((item) => item.id),
    ...recentConversationTaskItems.map((item) => item.id),
    ...conversationMessageItems.flatMap((message) => [message.task_id, message.related_task_id].filter((id): id is string => Boolean(id))),
    ...(waitingAccountTasks.data?.items.map((item) => item.id) ?? []),
    ...taskPanelPreviewTaskIDs,
    ...(taskDetailTaskID ? [taskDetailTaskID] : []),
  ])];
  const currentConversationTaskIDs = new Set([
    ...conversationTaskItems.map((item) => item.id),
    ...recentConversationTaskItems.map((item) => item.id),
    ...conversationMessageItems.flatMap((message) => [message.task_id, message.related_task_id].filter((id): id is string => Boolean(id))),
  ]);
  const taskQueries = useQueries({ queries: taskIDs.map((id) => ({
    queryKey: ['task', id], queryFn: () => portalApi.task(id),
    enabled: currentConversationTaskIDs.has(id) || taskPanelPreviewTaskIDs.has(id) || taskDetailTaskID === id,
    structuralSharing: latestTaskSnapshot,
    refetchInterval: (query: { state: { data: Task | undefined } }) => query.state.data?.result_message_pending ? 1000
      : query.state.data && terminalTaskStatuses.has(query.state.data.status) ? false
        : query.state.data && query.state.data.conversation_id !== conversationID ? 5000 : 1500,
  })) });
  const allTasks = taskQueries.flatMap((query) => query.data && !isResourceNoLongerAccessible(query.error) ? [query.data] : []);
  const tasks = allTasks.filter((task) => task.conversation_id === conversationID);
  const allTasksByID = new Map(allTasks.map((task) => [task.id, task]));
  useEffect(() => {
    const serviceInstanceID = capabilities.data?.service_instance_id;
    const actorID = user?.id;
    if (!messages.data || !serviceInstanceID || !actorID) return;
    for (const page of messages.data.pages) {
      for (const message of page.items) {
        const routingStatus = message.routing_status;
        if (!routingStatus) continue;
        const retryIdentity = JSON.stringify([serviceInstanceID, actorID, message.conversation_id, message.id]);
        const previousStatus = observedProcessingRetryStates.current.get(retryIdentity);
        observedProcessingRetryStates.current.set(retryIdentity, routingStatus);
        if (routingStatus === 'failed' || previousStatus === routingStatus) continue;
        try {
          clearProcessingRetryKey(serviceInstanceID, actorID, '', message.conversation_id, message.id);
        } catch {
          // Keeping a stale key is safe; it prevents a duplicate retry if state is still uncertain.
        }
      }
    }
  }, [capabilities.data?.service_instance_id, messages.data, user?.id]);
  for (const item of allTasks) taskPanelItemsByID.set(item.id, item);
  const taskPanelTasks = [...taskPanelItemsByID.values()].sort((left, right) => {
    const leftTask = allTasksByID.get(left.id);
    const rightTask = allTasksByID.get(right.id);
    return Number(taskActivityIsExecuting(rightTask?.status ?? right.status)) - Number(taskActivityIsExecuting(leftTask?.status ?? left.status)) ||
      Date.parse(rightTask?.updated_at ?? right.updated_at) - Date.parse(leftTask?.updated_at ?? left.updated_at) || right.id.localeCompare(left.id);
  });
  const taskProgressReadError = taskQueries.some((query, index) => query.isError && !isResourceNoLongerAccessible(query.error) &&
    currentConversationTaskIDs.has(taskIDs[index] ?? ''));
  const taskProgressNetworkReadError = taskQueries.some((query, index) => query.isError &&
    isTransientNetworkError(query.error) && currentConversationTaskIDs.has(taskIDs[index] ?? ''));
  const visibleTaskPanelTasks = taskPanelTasks.slice(0, visibleTaskPanelCount);
  const conversationFiles: ConversationFileItem[] = [
    ...conversationMessageItems.flatMap((item) => (item.attachment_ids ?? []).map((attachmentID, index) => ({
      kind: 'attachment' as const, key: `attachment:${item.id}:${attachmentID}`, attachmentID,
      messageID: item.id, index: index + 1, createdAt: item.created_at,
    }))),
    ...tasks.flatMap((item) => item.artifacts.filter(hasCompleteVerifiedArtifactProof).map((artifact) => ({ kind: 'artifact' as const,
      key: `artifact:${item.id}:${artifact.id}`, taskID: item.id, taskTitle: item.objective || '未命名任务', artifact }))),
  ];
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
  const showConsolidatedNetworkError = [
    { failed: capabilities.isError, error: capabilities.error },
    { failed: conversations.isError, error: conversations.error },
    { failed: Boolean(user) && mainConversation.isError, error: mainConversation.error },
    { failed: models.isError, error: models.error },
    { failed: availableModels.isError, error: availableModels.error },
    { failed: preferences.isError, error: preferences.error },
    { failed: hosts.isError, error: hosts.error },
    { failed: sharedHosts.isError, error: sharedHosts.error },
    { failed: execution.isError, error: execution.error },
    { failed: Boolean(conversationID) && conversationDetail.isError, error: conversationDetail.error },
    { failed: Boolean(mainConversationID) && mainConversationDetail.isError, error: mainConversationDetail.error },
    { failed: Boolean(conversationID) && messages.isError, error: messages.error },
    { failed: Boolean(conversationID) && conversationTasks.isError, error: conversationTasks.error },
    { failed: taskPanelOpen && taskPanelTab === 'tasks' && accountTasks.isError, error: accountTasks.error },
    { failed: Boolean(user?.id) && waitingAccountTasks.isError, error: waitingAccountTasks.error },
    { failed: Boolean(conversationID) && taskPanelOpen && recentConversationTasks.isError, error: recentConversationTasks.error },
    ...conversationPreviewQueries.map((query) => ({ failed: query.isError, error: query.error })),
    ...taskQueries.map((query) => ({ failed: query.isError, error: query.error })),
  ].some((item) => item.failed && isTransientNetworkError(item.error));
  const networkErrorToastID = useRef<number | null>(null);
  useEffect(() => {
    if (!showConsolidatedNetworkError) {
      if (networkErrorToastID.current !== null) dismiss(networkErrorToastID.current);
      networkErrorToastID.current = null;
      return;
    }
    if (networkErrorToastID.current === null) {
      networkErrorToastID.current = notify('网络异常，部分对话数据暂时无法读取。', 'error');
    }
  }, [dismiss, notify, showConsolidatedNetworkError]);
  const selected = conversationsData.find((item) => item.id === conversationID) ?? conversationDetail.data;
  const readyModels = models.data?.items.filter((item) => item.checked_at && item.capabilities.text) ?? [];
  const unavailableModels = models.data?.items.filter((item) => !item.checked_at || !item.capabilities.text) ?? [];
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
  const requestExecutionEnable = () => {
    const state = execution.data;
    if (!user?.id || !state || state.owner_id !== user.id || state.state !== 'disabled') {
      void execution.refetch();
      return;
    }
    enableExecution.reset();
    setEnableExecutionConfirmationOpen(true);
  };
  const confirmExecutionEnable = () => {
    const state = execution.data;
    if (!user?.id || !state || state.owner_id !== user.id || state.state !== 'disabled') {
      void execution.refetch();
      setEnableExecutionConfirmationOpen(false);
      return;
    }
    const previous = enableExecutionAttempt.current;
    const attempt = previous?.userID === user.id && previous.epoch === state.epoch
      ? previous : { userID: user.id, epoch: state.epoch, key: newIdempotencyKey() };
    enableExecutionAttempt.current = attempt;
    enableExecution.mutate({ ownerID: user.id, epoch: state.epoch, key: attempt.key });
  };
  const hostItems = useMemo(() => hosts.data?.items ?? [], [hosts.data?.items]);
  const preferredHostID = preferences.data?.default_host_id || (hostItems.length === 1 ? hostItems[0]?.id : '') || '';
  const computerHostOptions = useMemo<ComputerHostOption[]>(() => [
    ...hostItems.map((host) => ({ id: host.id, name: host.name || host.id, url: host.url, state: host.state, owned: true, permissions: [], control_state: host.control_state })),
    ...(sharedHosts.data?.items ?? []).map((host) => ({
      id: host.id, name: host.name || host.id, url: host.url, state: host.state, owned: false, permissions: host.permissions,
    })),
  ], [hostItems, sharedHosts.data?.items]);
  const selectedComputerHost = useMemo(() => preferredHostID ? computerHostOptions.find((host) => host.id === preferredHostID) : undefined,
    [computerHostOptions, preferredHostID]);
  const selectedComputerTargets = useMemo(() => selectedComputerHost ? hostEmbedTargetPlan(selectedComputerHost) : [], [selectedComputerHost]);
  const computerControl = useQuery({
    queryKey: ['host-control', selectedComputerHost?.id ?? ''],
    queryFn: () => portalApi.hostControl(selectedComputerHost!.id),
    enabled: Boolean(selectedComputerHost),
  });
  const computerControlHeldByMe = computerControl.data?.control_state === 'user'
    && computerControl.data.control_lease_active && computerControl.data.control_lease_held_by_me
    && !computerControl.isFetching && !computerControl.isError;
  const computerEmbedFailed = computerEmbed?.status === 'error' && computerEmbed.conversationID === conversationID
    && computerEmbed.hostID === selectedComputerHost?.id;
  const computerEmbedReady = computerEmbed?.status === 'ready';
  const requestComputerEmbed = useCallback(async (requestedTarget?: HostEmbedTicketRequest['target']) => {
    const target = requestedTarget ?? 'browser';
    if (!conversationID) { setComputerPanelError('先发送一条消息创建当前旁聊，再连接云端电脑。'); return; }
    if (!selectedComputerHost) { setComputerPanelError('请先在云端电脑设置中配置默认电脑。'); return; }
    if (selectedComputerHost.state !== 'running') { setComputerPanelError('此电脑当前未运行，启动后才能连接。'); return; }
    if (!selectedComputerTargets.includes(target)) { setComputerPanelError('当前账号没有此电脑的可用工作台权限。'); return; }
    if (computerConnecting) return;
    if (computerLockAck.current) {
      window.clearTimeout(computerLockAck.current.timer);
      computerLockAck.current.resolve(false);
      computerLockAck.current = null;
    }
    computerCommandSequences.current = { selection: 0, lock: 0 };
    computerLockRequested.current = null;
    computerControlReport.current = '';
    const generation = ++computerRequestGeneration.current;
    setComputerConnecting(true);
    setComputerPanelError('');
    setComputerEmbed(null);
    setComputerControlExpanded(false);
    try {
      const desktopBridge = nativeWorkbenchBridge();
      if (desktopBridge?.openHostWorkbenchEmbed) {
        const opened = await desktopBridge.openHostWorkbenchEmbed({
          hostID: selectedComputerHost.id, conversationID,
          initialTarget: target, targets: selectedComputerTargets,
          shared: !selectedComputerHost.owned,
        }) as { hostID?: unknown; hostOrigin?: unknown; conversationID?: unknown; nonce?: unknown; targets?: unknown };
        if (generation !== computerRequestGeneration.current) {
          if (typeof opened?.nonce === 'string') void desktopBridge.closeHostWorkbenchEmbed?.(opened.nonce);
          return;
        }
        if (opened?.hostID !== selectedComputerHost.id || opened.conversationID !== conversationID ||
          typeof opened.hostOrigin !== 'string' || typeof opened.nonce !== 'string' || !Array.isArray(opened.targets) ||
          !opened.targets.length || !opened.targets.every((item) => selectedComputerTargets.includes(item as HostEmbedTicket['targets'][number]))) {
          throw new Error('桌面客户端返回了无效的工作台状态。');
        }
        setComputerEmbed({ conversationID, hostID: selectedComputerHost.id, hostOrigin: opened.hostOrigin, nonce: opened.nonce,
          targets: opened.targets as HostEmbedTicket['targets'], native: true, status: 'loading', remoteDesktop: 'connecting',
          activeTarget: target === 'workspace' ? 'files' : target as ComputerEmbedSession['activeTarget'],
          controlLocked: true, controlVersion: null, selectionSeq: null, lockSeq: null });
        return;
      }
      const issued = await portalApi.issueHostEmbedTicket(selectedComputerHost.id, {
        conversation_id: conversationID, target, targets: selectedComputerTargets,
      });
      if (generation !== computerRequestGeneration.current) return;
      const hostURL = new URL(selectedComputerHost.url);
      const exchangeURL = new URL(issued.exchange_url);
      const allowedTargets = new Set(selectedComputerTargets);
      const validTargets = issued.targets.length > 0 && issued.targets.every((target) => allowedTargets.has(target));
      if (hostURL.username || hostURL.password || exchangeURL.origin !== hostURL.origin ||
        exchangeURL.pathname !== '/__ch/embed-auth' || exchangeURL.search || exchangeURL.hash ||
        exchangeURL.username || exchangeURL.password || !/^[\w-]{43}$/.test(issued.ticket) ||
        !/^[\w-]{32}$/.test(issued.embed_nonce) || !validTargets || !issued.targets.includes(target)) {
        throw new Error('电脑连接凭据或授权范围无效，请刷新后重试。');
      }
      setComputerEmbed({
        conversationID, hostID: selectedComputerHost.id, hostOrigin: hostURL.origin,
        exchangeURL: exchangeURL.toString(), nonce: issued.embed_nonce, ticket: issued.ticket,
        targets: issued.targets, status: 'submitting', native: false, remoteDesktop: 'connecting',
        activeTarget: target === 'workspace' ? 'files' : target as ComputerEmbedSession['activeTarget'],
        controlLocked: true, controlVersion: null, selectionSeq: null, lockSeq: null,
      });
    } catch (error) {
      if (generation === computerRequestGeneration.current) {
        setComputerEmbed({ conversationID, hostID: selectedComputerHost.id, hostOrigin: selectedComputerHost.url,
          nonce: `failed-${generation}`, targets: selectedComputerTargets, status: 'error', remoteDesktop: 'connecting',
          activeTarget: null, controlLocked: true, controlVersion: null, selectionSeq: null, lockSeq: null,
          error: errorDetails(error).message || '连接云端电脑失败。' });
      }
    } finally {
      if (generation === computerRequestGeneration.current) setComputerConnecting(false);
    }
  }, [computerConnecting, conversationID, selectedComputerHost, selectedComputerTargets]);
  const applyComputerWorkbenchState = useCallback((nonce: string, message: Record<string, unknown>) => {
    const current = computerEmbedRef.current;
    if (!current || current.nonce !== nonce) return;
    if (typeof message.control_state === 'string' && typeof message.control_lease_active === 'boolean' &&
      typeof message.control_lease_held_by_me === 'boolean' &&
      (typeof message.control_version === 'number' || message.control_version === null)) {
      const signature = [nonce, message.control_state, message.control_version, message.control_lease_active,
        message.control_lease_held_by_me, message.control_lease_expires_at].join(':');
      if (computerControlReport.current !== signature) {
        computerControlReport.current = signature;
        void client.invalidateQueries({ queryKey: ['host-control', current.hostID] });
      }
    }
    const remoteDesktop = ['connecting', 'connected', 'disconnected', 'failed'].includes(String(message.remote_desktop))
      ? message.remote_desktop as ComputerEmbedSession['remoteDesktop'] : current.remoteDesktop;
    const incomingSelectionSeq = Number.isSafeInteger(message.selection_seq) && Number(message.selection_seq) >= 0
      ? Number(message.selection_seq) : null;
    const incomingLockSeq = Number.isSafeInteger(message.lock_seq) && Number(message.lock_seq) >= 0
      ? Number(message.lock_seq) : null;
    const selectionIsCurrent = incomingSelectionSeq !== null &&
      (current.selectionSeq === null || incomingSelectionSeq >= current.selectionSeq);
    const lockIsCurrent = incomingLockSeq !== null &&
      (current.lockSeq === null || incomingLockSeq >= current.lockSeq);
    const activeTarget = selectionIsCurrent && ['browser', 'terminal', 'files', 'vscode'].includes(String(message.active_target))
      ? message.active_target as ComputerEmbedSession['activeTarget'] : current.activeTarget;
    const controlLocked = lockIsCurrent && typeof message.control_locked === 'boolean'
      ? message.control_locked : current.controlLocked;
    const controlVersion = typeof message.control_version === 'number' && Number.isSafeInteger(message.control_version) && message.control_version >= 0
      ? message.control_version : message.control_version === null ? null : current.controlVersion;
    const next: ComputerEmbedSession = {
      ...current,
      status: message.workbench_ready === true ? 'ready' : current.status,
      remoteDesktop, activeTarget, controlLocked, controlVersion,
      selectionSeq: selectionIsCurrent ? incomingSelectionSeq : current.selectionSeq,
      lockSeq: lockIsCurrent ? incomingLockSeq : current.lockSeq,
    };
    computerEmbedRef.current = next;
    setComputerEmbed(next);
    if (message.control_state === 'user' && message.control_lease_active === true && message.control_lease_held_by_me === true) {
      setComputerControlExpanded(true);
    } else if (message.control_state === 'ai' && message.control_lease_active === false && message.control_lease_held_by_me === false) {
      setComputerControlExpanded(false);
    }
    const pending = computerLockAck.current;
    if (pending && pending.nonce === nonce && incomingLockSeq === pending.seq && message.control_locked === pending.locked) {
      window.clearTimeout(pending.timer);
      computerLockAck.current = null;
      computerLockRequested.current = { nonce, locked: pending.locked, seq: pending.seq };
      pending.resolve(true);
    }
  }, [client]);
  const selectComputerTarget = useCallback(async (requested: 'browser' | 'terminal' | 'files' | 'vscode') => {
    const session = computerEmbedRef.current;
    if (!conversationID || !selectedComputerHost || selectedComputerHost.state !== 'running') return;
    if (!computerControlHeldByMe || session?.controlLocked !== false) return;
    const target: HostEmbedTicketRequest['target'] = requested === 'files' && !selectedComputerTargets.includes('files') && selectedComputerTargets.includes('workspace')
      ? 'workspace' : requested;
    if (!selectedComputerTargets.includes(target)) {
      setComputerPanelError('当前账号没有此电脑的可用工作台权限。');
      return;
    }
    setComputerPanelError('');
    if (!session || session.conversationID !== conversationID || session.hostID !== selectedComputerHost.id || session.status === 'error') {
      await requestComputerEmbed(target);
      return;
    }
    if (session.status !== 'ready' || initializedEmbedNonce.current !== session.nonce) return;
    const seq = ++computerCommandSequences.current.selection;
    const sent = await sendComputerCommand(session, { type: 'select_target', target: requested, seq });
    if (!sent) setComputerPanelError('工作台尚未确认功能切换，请重试。');
  }, [computerControlHeldByMe, conversationID, requestComputerEmbed, selectedComputerHost, selectedComputerTargets, sendComputerCommand]);
  const prepareComputerControlAction = useCallback(async () => {
    const ready = await setComputerControlLock(true, true);
    if (!ready) notify('工作台尚未确认已锁定输入，控制权请求没有发送。', 'error');
    return ready;
  }, [notify, setComputerControlLock]);
  useEffect(() => {
    if (!taskPanelOpen || taskPanelTab !== 'computer') {
      computerAutoConnectKey.current = '';
      return;
    }
    if (!conversationID || !selectedComputerHost) return;
    const key = `${conversationID}:${selectedComputerHost.id}`;
    const existing = computerEmbed?.conversationID === conversationID && computerEmbed.hostID === selectedComputerHost.id;
    if (existing && computerEmbed?.status !== 'error') return;
    if (computerAutoConnectKey.current === key) return;
    if (computerConnecting) return;
    if (selectedComputerHost.state !== 'running' || !selectedComputerTargets.includes('browser')) return;
    const timer = window.setTimeout(() => {
      computerAutoConnectKey.current = key;
      void requestComputerEmbed('browser');
    }, 0);
    return () => window.clearTimeout(timer);
  }, [computerConnecting, computerEmbed?.conversationID, computerEmbed?.hostID, computerEmbed?.status, conversationID, requestComputerEmbed,
    selectedComputerHost, selectedComputerTargets, taskPanelOpen, taskPanelTab]);
  useEffect(() => {
    if (!computerEmbedReady || !computerControl.data) return;
    const status = computerControl.data;
    const locked = !(status.control_state === 'user' && status.control_lease_active && status.control_lease_held_by_me
      && !computerControl.isFetching && !computerControl.isError);
    void setComputerControlLock(locked);
  }, [computerControl.data, computerControl.isError, computerControl.isFetching, computerEmbed?.nonce, computerEmbedReady, setComputerControlLock]);
  const mergedDisplayMessages = mergeTaskAssistantMessages(conversationMessageItems, tasks);
  const selectedVariantMap = new Map(Object.entries(selectedMessageVariantVersions).map(([groupID, version]) => [groupID, version]));
  const variantTimeline = buildMessageVariantTimeline(mergedDisplayMessages, selectedVariantMap);
  const displayMessages = variantTimeline.items;
  const messageVariantOperationGroups = [...variantTimeline.groups.values()].flatMap((group) => {
    const groupMessages = [...group.versions.values()].flatMap((version) => version.messages);
    const interruptedMessages = groupMessages.filter((item) => item.role === 'assistant' && item.output_status === 'interrupted');
    const operations = variantOutbox.entries.filter((entry) => entry.operation.input.conversationID === conversationID &&
      (entry.operation.input.messageID === group.id || group.messageIDs.has(entry.operation.input.messageID)) &&
      entry.operation.status !== 'failed');
    return group.versions.size > 1 || interruptedMessages.length > 0 || operations.length > 0
      ? [{ group, groupMessages, interruptedMessages, operations }]
      : [];
  });
  const latestUserMessageByTaskID = new Map<string, string>();
  for (const message of displayMessages) {
    const linkedTaskID = message.task_id ?? message.related_task_id;
    if (message.role === 'user' && linkedTaskID) latestUserMessageByTaskID.set(linkedTaskID, message.id);
  }
  const variantSelectionSignature = JSON.stringify(Object.entries(selectedMessageVariantVersions).sort(([left], [right]) => left.localeCompare(right)));
  let showConversationThinking = displayMessages.some((message) => message.role === 'assistant' && message.snapshotStatus === 'streaming');
  const checkedTaskIDs = new Set<string>();
  for (const message of [...displayMessages].reverse()) {
    if (message.role !== 'user' || message.duplicate_of_message_id) continue;
    const taskID = message.task_id ?? message.related_task_id;
    if (!taskID) {
      if (message.routing_status === 'processing' && !messages.isError && !messages.isRefetchError) showConversationThinking = true;
      continue;
    }
    if (checkedTaskIDs.has(taskID)) continue;
    checkedTaskIDs.add(taskID);
    const task = tasksByID.get(taskID);
    const taskQuery = taskQueries[taskIDs.indexOf(taskID)];
    const revisionIsCurrent = task?.current_revision === undefined || task?.desired_revision === undefined ||
      task.current_revision === task.desired_revision;
    if (!task || taskQuery?.isError || taskQuery?.isRefetchError || task.status !== 'running' ||
      task.current_revision_started !== true || !revisionIsCurrent || task.waiting_reason || task.pending_request?.status === 'pending') continue;
    showConversationThinking = true;
  }
  const latestDisplayMessageContent = displayMessages.at(-1)?.content;
  const loadOlderMessages = async () => {
    const view = conversationView.current;
    if (!view || !conversationID || !messages.hasNextPage || messages.isFetchingNextPage || historyLoadsInFlight.current.has(conversationID)) return;
    const targetConversationID = conversationID;
    historyLoadsInFlight.current.add(targetConversationID);
    olderMessagesAnchor.current = { conversationID: targetConversationID, height: view.scrollHeight, top: view.scrollTop };
    followConversation.current = false;
    setShowJumpToLatest(true);
    try {
      const result = await messages.fetchNextPage();
      if (selectedConversation.current !== targetConversationID) return;
      if (result.isFetchNextPageError) olderMessagesAnchor.current = null;
      else setHistoryPageLoadVersion((version) => version + 1);
    } finally {
      historyLoadsInFlight.current.delete(targetConversationID);
    }
  };
  const selectMessageVariantVersion = (groupID: string, version: number) => {
    variantScrollAnchor.current = conversationView.current?.scrollTop ?? 0;
    followConversation.current = false;
    setSelectedMessageVariantVersions((current) => ({ ...current, [groupID]: version }));
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
  const sendVariantOperation = async (entry: MessageVariantOutboxEntry): Promise<boolean> => {
    const key = entry.operation.input.idempotencyKey;
    if (sendingVariantOperationKeys.current.has(key)) return false;
    sendingVariantOperationKeys.current.add(key);
    try {
      if (entry.scope !== variantOutbox.currentScope || entry.operation.input.actorID !== user?.id ||
        entry.operation.input.serviceInstanceID !== capabilities.data?.service_instance_id) {
        throw new Error('账号或服务身份已变化；此操作保留在本机，请核对后重新发起。');
      }
      await variantOutbox.change(entry.scope, key, (operation) => ({ ...operation, status: 'sending', error: undefined }));
      const currentCapabilities = await portalApi.capabilities();
      if (currentCapabilities.service_instance_id !== entry.operation.input.serviceInstanceID) {
        throw new Error('服务身份已变化；此操作未重放，请核对后重新发起。');
      }
      const result = await createMessageVariant(entry.operation.input);
      if (result.state === 'unknown') {
        await variantOutbox.change(entry.scope, key, (operation) => ({ ...operation, status: 'unknown', error: '服务端尚未确认操作结果；重试会复用原操作编号。' }));
        void client.invalidateQueries({ queryKey: ['conversation-messages', entry.operation.input.conversationID] });
        notify('操作结果待核对；可以使用同一请求编号安全重试。', 'error');
        return false;
      }
      await variantOutbox.remove(entry.scope, key);
      if (result.state !== 'failed') {
        setSelectedMessageVariantVersions((current) => ({ ...current, [result.variant_group_id]: result.variant_version }));
      }
      void client.invalidateQueries({ queryKey: ['conversation-messages', entry.operation.input.conversationID] });
      void client.invalidateQueries({ queryKey: ['conversation-tasks', entry.operation.input.conversationID] });
      if (result.state === 'failed') {
        notify('服务端已确认消息版本操作失败；对话已刷新。', 'error');
        return false;
      }
      notify(result.state === 'completed' ? '已创建新的消息版本。' : '消息版本操作已受理，正在更新对话。', 'success');
      return true;
    } catch (error) {
      const staleVersion = error instanceof ApiError && error.status === 409;
      const uncertain = !(error instanceof ApiError) || error.status >= 500 || [408, 425, 429].includes(error.status);
      const details = errorDetails(error);
      await variantOutbox.change(entry.scope, key, (operation) => ({ ...operation,
        status: staleVersion ? 'failed' : uncertain ? 'unknown' : 'failed', error: staleVersion
          ? '版本已变化，已刷新消息；请选中最新版本后重新操作。' : details.message }));
      if (staleVersion) {
        await client.invalidateQueries({ queryKey: ['conversation-messages', entry.operation.input.conversationID] });
        notify('消息版本已变化，已刷新对话；请核对后再操作。', 'error');
      } else if (uncertain) notify('操作结果待核对；可以用同一请求编号安全重试。', 'error');
      else notify(details.message, 'error');
      return false;
    } finally {
      sendingVariantOperationKeys.current.delete(key);
    }
  };

  const startVariantOperation = async (groupID: string, messageID: string) => {
    const group = variantTimeline.groups.get(groupID);
    const serviceInstanceID = capabilities.data?.service_instance_id;
    if (!group || !user?.id || !serviceInstanceID) {
      notify('账号、服务或消息版本尚未确认，无法执行此操作。', 'error');
      return false;
    }
    try {
      const operation = await variantOutbox.enqueue({
        conversationID: conversationID!, messageID, action: 'continue_generation', expectedVersion: group.latestVersion,
        serviceInstanceID, actorID: user.id,
      });
      return sendVariantOperation(operation);
    } catch (error) {
      notify(errorDetails(error).message, 'error');
      return false;
    }
  };

  const sendMessage = async (entry: MessageOutboxEntry, reconcile: boolean) => {
    let { scope, message } = entry;
    const id = message.request.client_message_id;
    if (sendingMessageIDs.current.has(id)) return;
    sendingMessageIDs.current.add(id);
    let accepted = message.status === 'accepted' && Boolean(message.messageID);
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
        const original = message.conversationCreateTitle === undefined
          ? firstOutboxConversationMessage(await messageOutbox.read(scope), message) : message;
        const title = original.conversationCreateTitle === undefined
          ? conversationTitleFromText(original.request.content) : original.conversationCreateTitle;
        checkScope();
        const created = await portalApi.createConversation(title ? { title } : {}, message.conversationCreateKey, message.serviceInstanceID!, message.actorID);
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
          await outbox.change(scope, id, (current) => ({ ...current, messageID: receipt.id, status: 'accepted', error: undefined }));
          client.setQueryData<ConversationMessagesData>(['conversation-messages', targetID], (previous) => appendConversationReceipt(previous, receipt));
        }
      }

      if (!accepted) {
        const request = outboxReceiptRequest(message);
        await outbox.change(scope, id, (current) => ({ ...current, status: 'sending', error: undefined }));
        checkScope();
        let receipt: ConversationMessage;
        if (needsMessageUpload(request.content)) {
          receipt = await uploadMessage<ConversationMessage>({ conversationID: targetID }, request, message.serviceInstanceID!, message.actorID, checkScope);
        } else {
          try {
            receipt = (await portalApi.acceptConversationMessage(targetID, request, id, message.serviceInstanceID!, message.actorID)).message;
          } catch (error) {
            const uncertain = !(error instanceof ApiError) || error.status >= 500 || [408, 425, 429].includes(error.status);
            if (!uncertain) throw error;
            let recovered: ConversationMessage | undefined;
            try {
              const page = await portalApi.conversationMessages(targetID, undefined, id, message.serviceInstanceID!, message.actorID);
              recovered = page.items.find((item) => item.client_message_id === id);
            } catch {
              throw error;
            }
            if (!recovered) throw error;
            receipt = recovered;
          }
        }
        if (receipt.conversation_id !== targetID || receipt.role !== 'user' || receipt.client_message_id !== id ||
          receipt.content !== request.content) throw new Error('服务端消息回执与当前账号或原文不匹配，请核对后重试。');
        accepted = true;
        message = { ...message, messageID: receipt.id, status: 'accepted' };
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
      sendingMessageIDs.current.delete(id);
    }
  };
  const respondToApproval = useMutation({
    mutationFn: (request: { taskID: string; approvalID: string; version: number; decision: 'approve' | 'reject'; actionScope: string; key: string }) =>
      portalApi.respondToTaskApproval(request.taskID, request.approvalID,
        { version: request.version, decision: request.decision }, request.key),
    onSuccess: async (_response, request) => {
      clearTaskActionKey(request.actionScope, request.key);
      await Promise.all([
        client.invalidateQueries({ queryKey: ['task', request.taskID] }),
        client.invalidateQueries({ queryKey: ['assistant-account-tasks'] }),
        client.invalidateQueries({ queryKey: ['conversation-tasks', conversationID] }),
        client.invalidateQueries({ queryKey: ['task-approvals'] }),
      ]);
      const decisionLabel = request.decision === 'approve' ? '已允许本次操作' : '已拒绝本次操作';
      notify(`${decisionLabel}，任务后续状态等待更新。`, 'success');
    },
    onError: async (error, request) => {
      if (isResourceNoLongerAccessible(error)) {
        clearTaskActionKey(request.actionScope, request.key);
        client.removeQueries({ queryKey: ['task', request.taskID], exact: true });
        await Promise.all([
          client.invalidateQueries({ queryKey: ['task', request.taskID], exact: true }),
          client.invalidateQueries({ queryKey: ['assistant-account-tasks'] }),
          client.invalidateQueries({ queryKey: ['conversation-tasks'] }),
          client.invalidateQueries({ queryKey: ['task-approvals'] }),
        ]);
        notify(errorDetails(error).message, 'error');
        return;
      }
      await handleTaskActionError(error, request.taskID);
    },
  });
  const submitApprovalDecision = (task: Task, decision: 'approve' | 'reject') => {
    const request = task.pending_request;
    if (!request || request.status !== 'pending' || task.conversation_id !== conversationID) {
      return Promise.reject(new Error('审批状态已变化，请刷新后核对。'));
    }
    const actionScope = `approval:${task.id}:${request.id}`;
    const fingerprint = JSON.stringify({ version: request.version, decision });
    return respondToApproval.mutateAsync({ taskID: task.id, approvalID: request.id, version: request.version,
      decision, actionScope, key: taskActionKey(actionScope, fingerprint) });
  };
  const submitClarificationAnswer = async (task: Task) => {
    const request = task.clarification_request;
    if (!request || !canAnswerClarification(task) || task.conversation_id !== conversationID) {
      notify('澄清请求已变化，请核对任务最新状态。', 'error');
      return;
    }
    const draftKey = clarificationDraftKey(task.id, request.id, request.version);
    const answer = (clarificationAnswers[draftKey] ?? '').trim();
    if (!answer) return;
    const isFileDeliveryClarification = isFileDeliveryIntentClarification(request.call_id);
    const fileDeliveryIntent = isFileDeliveryClarification ? clarificationFileDeliveryIntents[draftKey] : undefined;
    if (isFileDeliveryClarification && (!fileDeliveryIntent ||
      (fileDeliveryIntent === 'required' && !task.workspace_id))) return;
    if (clarificationSubmissionsInFlight.current.has(task.id)) return;
    clarificationSubmissionsInFlight.current.add(task.id);
    const actionScope = `clarification:${task.id}:${request.id}`;
    const fingerprint = JSON.stringify({ call_id: request.call_id, version: request.version, answer,
      ...(fileDeliveryIntent ? { file_delivery_intent: fileDeliveryIntent } : {}) });
    const key = taskActionKey(actionScope, fingerprint);
    setClarificationInteractions((current) => ({ ...current, [task.id]: {
      clarificationID: request.id, version: request.version, question: request.question, status: 'sending',
    } }));
    try {
      await portalApi.answerTaskInput(task.id, {
        clarification_id: request.id,
        clarification_version: request.version,
        answer,
        ...(fileDeliveryIntent ? { file_delivery_intent: fileDeliveryIntent } : {}),
      }, key);
      clearTaskActionKey(actionScope, key);
      setClarificationAnswers((current) => ({ ...current, [draftKey]: '' }));
      setClarificationFileDeliveryIntents((current) => {
        if (!(draftKey in current)) return current;
        const next = { ...current };
        delete next[draftKey];
        return next;
      });
      setClarificationInteractions((current) => ({ ...current, [task.id]: {
        clarificationID: request.id, version: request.version, question: request.question,
        status: 'submitted', message: '补充信息已提交，等待任务状态更新。',
      } }));
      await Promise.all([
        client.invalidateQueries({ queryKey: ['task', task.id] }),
        client.invalidateQueries({ queryKey: ['assistant-account-tasks'] }),
        client.invalidateQueries({ queryKey: ['conversation-tasks', conversationID] }),
      ]);
      notify('补充信息已提交，等待任务状态更新。', 'success');
    } catch (error) {
      const details = errorDetails(error);
      if (isResourceNoLongerAccessible(error)) {
        clearTaskActionKey(actionScope, key);
        setClarificationInteractions((current) => ({ ...current, [task.id]: {
          clarificationID: request.id, version: request.version, question: request.question,
          status: 'revoked', message: details.message,
        } }));
        client.removeQueries({ queryKey: ['task', task.id], exact: true });
        await Promise.all([
          client.invalidateQueries({ queryKey: ['task', task.id], exact: true }),
          client.invalidateQueries({ queryKey: ['assistant-account-tasks'] }),
          client.invalidateQueries({ queryKey: ['conversation-tasks'] }),
        ]);
        notify(details.message, 'error');
      } else {
        const uncertain = !(error instanceof ApiError) || error.status >= 500 || [408, 425, 429].includes(error.status);
        setClarificationInteractions((current) => ({ ...current, [task.id]: {
          clarificationID: request.id, version: request.version, question: request.question,
          status: uncertain ? 'unknown' : 'failed',
          message: uncertain ? '提交结果待确认；重试相同回答会复用原请求编号。' : details.message,
        } }));
        if (details.code === 'conflict') {
          await client.invalidateQueries({ queryKey: ['task', task.id] });
          notify('澄清请求状态已变化，已刷新最新任务状态。请核对后再操作。', 'error');
        } else if (uncertain) notify('补充信息提交结果待确认；重试相同回答会复用原请求编号。', 'error');
        else notify(details.message, 'error');
      }
    } finally {
      clarificationSubmissionsInFlight.current.delete(task.id);
    }
  };
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
        hostID: '', mcpConnectorIDs: [...mcpConnectorIDs],
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
          ...(preferredHostID ? { host_id: preferredHostID } : {}),
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

  const copyConversationMessage = async (text: string, role: 'user' | 'assistant') => {
    try {
      await navigator.clipboard.writeText(text);
      notify(role === 'assistant' ? '已复制回复。' : '已复制消息。', 'success');
    } catch {
      notify('无法访问剪贴板，请检查系统权限。', 'error');
    }
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
  const renderLocalDraftItem = (draft: typeof localComposerDrafts[number]) => {
    const title = draft.content.trim().split('\n')[0]?.slice(0, 42) || '新旁聊草稿';
    const label = draft.pendingMessage ? '待发送消息' : '未发送草稿';
    return <div className="conversation-row" key={draft.id}>
      <button type="button" className={`conversation-item ${selectedLocalDraftKey && draft.id === `local:${selectedLocalDraftKey}` ? 'is-selected' : ''}`}
        aria-label={`恢复${draft.pendingMessage ? label : '草稿'}：${title}`} onClick={() => void selectConversationID(draft.id)}>
        <strong>{title}</strong><small>{label} · {new Date(draft.updatedAt).toLocaleString()}</small>
      </button>
    </div>;
  };
  const renderCachedMainConversation = () => mainConversationID ? <div className="conversation-row conversation-row-main" key={mainConversationID}>
    <button type="button" className={`conversation-item ${conversationID === mainConversationID ? 'is-selected' : ''}`}
      onClick={() => selectConversationID(mainConversationID)}>
      <span className="assistant-logo conversation-main-logo" aria-hidden="true" />
      <strong>主会话</strong><small>最近记录</small>
    </button>
  </div> : null;

  const finishedTaskIDs = tasks.filter((task) => terminalTaskStatuses.has(task.status)).map((task) => task.id).join(',');
  useEffect(() => {
    olderMessagesAnchor.current = null;
    postComputerVisibility(false);
    computerRequestGeneration.current += 1;
  }, [conversationID, conversationScrollPositionKey, postComputerVisibility]);
  useEffect(() => {
    const session = computerEmbed;
    if (!session || session.native || session.status !== 'submitting' || !session.ticket || !session.exchangeURL) return;
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
    if (session.native) {
      const nativeSession = session;
      const bridge = nativeWorkbenchBridge();
      if (!bridge?.onHostWorkbenchMessage) return;
      const unsubscribe = bridge.onHostWorkbenchMessage((value) => {
        if (!value || typeof value !== 'object') return;
        const message = value as Record<string, unknown>;
        if (message.nonce !== nativeSession.nonce) return;
        if (message.type === 'error' && typeof message.error === 'string') {
          void Promise.resolve(bridge.closeHostWorkbenchEmbed?.(nativeSession.nonce)).catch(() => undefined);
          setComputerEmbed((current) => current?.nonce === nativeSession.nonce
            ? { ...current, native: false, status: 'error', error: message.error as string } : current);
        } else if (message.type === 'state' && message.conversation_id === nativeSession.conversationID &&
          message.host_id === nativeSession.hostID && typeof message.workbench_ready === 'boolean') {
          applyComputerWorkbenchState(nativeSession.nonce, message);
        }
      });
      if (initializedEmbedNonce.current === nativeSession.nonce) {
        void Promise.resolve(bridge.sendHostWorkbenchCommand?.(nativeSession.nonce, {
          type: 'visibility', conversation_id: nativeSession.conversationID, visible: taskPanelOpen && taskPanelTab === 'computer',
        })).catch(() => undefined);
      }
      return () => unsubscribe();
    }
    const send = (type: 'init' | 'visibility', visible: boolean) => {
      computerFrame.current?.contentWindow?.postMessage({
        channel: 'morde.workbench.embed', version: 1, type, nonce: session.nonce,
        conversation_id: session.conversationID, visible,
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
      } else if (message.type === 'error' && typeof message.error === 'string') {
        setComputerEmbed((current) => current?.nonce === session.nonce
          ? { ...current, status: 'error', error: message.error as string } : current);
      } else if (message.type === 'state' && message.conversation_id === session.conversationID &&
        message.host_id === session.hostID && typeof message.workbench_ready === 'boolean') {
        applyComputerWorkbenchState(session.nonce, message);
      }
    };
    window.addEventListener('message', onMessage);
    if (initializedEmbedNonce.current === session.nonce) send('visibility', taskPanelOpen && taskPanelTab === 'computer');
    return () => window.removeEventListener('message', onMessage);
  }, [applyComputerWorkbenchState, computerEmbed, taskPanelOpen, taskPanelTab]);
  const nativeEmbedNonce = computerEmbed?.native ? computerEmbed.nonce : null;
  const nativeEmbedConversationID = computerEmbed?.native ? computerEmbed.conversationID : null;
  useEffect(() => {
    if (!nativeEmbedNonce || !nativeEmbedConversationID || initializedEmbedNonce.current === nativeEmbedNonce) return;
    initializedEmbedNonce.current = nativeEmbedNonce;
    void Promise.resolve(nativeWorkbenchBridge()?.sendHostWorkbenchCommand?.(nativeEmbedNonce, {
      type: 'init', conversation_id: nativeEmbedConversationID, visible: taskPanelOpen && taskPanelTab === 'computer',
    })).catch(() => undefined);
  }, [nativeEmbedNonce, nativeEmbedConversationID, taskPanelOpen, taskPanelTab]);
  useEffect(() => {
    if (!nativeEmbedNonce) return;
    const bridge = nativeWorkbenchBridge();
    return () => { void Promise.resolve(bridge?.closeHostWorkbenchEmbed?.(nativeEmbedNonce)).catch(() => undefined); };
  }, [nativeEmbedNonce]);
  useLayoutEffect(() => {
    if (!nativeEmbedNonce) return;
    const element = computerFrameBounds.current;
    const setBounds = nativeWorkbenchBridge()?.setHostWorkbenchBounds;
    if (!setBounds) return;
    let boundsUpdateFailed = false;
    const updateBounds = (bounds: { x: number; y: number; width: number; height: number }) => {
      const reportFailure = () => {
        if (boundsUpdateFailed) return;
        boundsUpdateFailed = true;
        setComputerEmbed((current) => current?.nonce === nativeEmbedNonce && current.native
          ? { ...current, native: false, status: 'error', error: '云端电脑窗口布局失败，请关闭并重新打开云端电脑面板。' }
          : current);
      };
      void Promise.resolve().then(() => setBounds(nativeEmbedNonce, bounds)).then((result) => {
        if (!result || typeof result !== 'object' || (result as { updated?: unknown }).updated !== true) reportFailure();
      }).catch(reportFailure);
    };
    if (!taskPanelOpen || taskPanelTab !== 'computer' || !element) {
      updateBounds({ x: 0, y: 0, width: 0, height: 0 });
      return;
    }
    const update = () => {
      const rect = element.getBoundingClientRect();
      let left = Math.max(0, rect.left), top = Math.max(0, rect.top);
      let right = Math.min(window.innerWidth, rect.right), bottom = Math.min(window.innerHeight, rect.bottom);
      for (let parent = element.parentElement; parent && parent !== document.body; parent = parent.parentElement) {
        const style = window.getComputedStyle(parent);
        if ([style.overflow, style.overflowX, style.overflowY].some((value) => value !== 'visible')) {
          const clip = parent.getBoundingClientRect();
          left = Math.max(left, clip.left); top = Math.max(top, clip.top);
          right = Math.min(right, clip.right); bottom = Math.min(bottom, clip.bottom);
        }
      }
      left = Math.min(window.innerWidth, Math.max(0, left));
      top = Math.min(window.innerHeight, Math.max(0, top));
      right = Math.max(left, Math.min(window.innerWidth, right));
      bottom = Math.max(top, Math.min(window.innerHeight, bottom));
      const x = Math.floor(left), y = Math.floor(top);
      const width = Math.max(0, Math.ceil(right) - x), height = Math.max(0, Math.ceil(bottom) - y);
      updateBounds({ x, y, width, height });
    };
    const observer = new ResizeObserver(update);
    observer.observe(element);
    window.addEventListener('resize', update);
    window.addEventListener('scroll', update, true);
    update();
    return () => { observer.disconnect(); window.removeEventListener('resize', update); window.removeEventListener('scroll', update, true); };
  }, [nativeEmbedNonce, taskPanelOpen, taskPanelTab, conversationID]);
  useEffect(() => {
    if (!computerEmbed || computerEmbed.status !== 'loading') return;
    const nonce = computerEmbed.nonce;
    const timer = window.setTimeout(() => {
      if (computerEmbed.native) void Promise.resolve(nativeWorkbenchBridge()?.closeHostWorkbenchEmbed?.(nonce)).catch(() => undefined);
      setComputerEmbed((current) => current?.nonce === nonce && current.status === 'loading'
        ? { ...current, native: false, status: 'error', error: '工作台连接超时，请关闭并重新打开云端电脑面板。' } : current);
    }, 30_000);
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
      setTaskDetailStepID(null);
      setTaskDetailTaskID(null);
      window.requestAnimationFrame(() => {
        if (focusPanelToggle) focusPanelToggle();
        else taskPanelToggle.current?.focus();
      });
    };
    window.addEventListener('keydown', closeOnEscape);
    return () => window.removeEventListener('keydown', closeOnEscape);
  }, [focusPanelToggle, postComputerVisibility, setTaskPanelOpen, taskPanelOpen]);
  useLayoutEffect(() => {
    const view = conversationView.current;
    if (previousScrollPositionKey.current !== conversationScrollPositionKey) {
      previousScrollPositionKey.current = conversationScrollPositionKey;
      olderMessagesAnchor.current = null;
      variantScrollAnchor.current = null;
      pendingScrollRestoreKey.current = conversationScrollPositionKey && conversationScrollPositions.current.has(conversationScrollPositionKey)
        ? conversationScrollPositionKey : null;
      if (!pendingScrollRestoreKey.current) followConversation.current = true;
    }
    const scrollRestoreKey = pendingScrollRestoreKey.current;
    if (view && scrollRestoreKey && scrollRestoreKey === conversationScrollPositionKey) {
      if (!messages.data) return;
      const savedPosition = conversationScrollPositions.current.get(scrollRestoreKey);
      view.scrollTop = Math.min(savedPosition ?? 0, Math.max(0, view.scrollHeight - view.clientHeight));
      pendingScrollRestoreKey.current = null;
      const atLatest = view.scrollHeight - view.scrollTop - view.clientHeight < 80;
      followConversation.current = atLatest;
      conversationScrollPositions.current.set(scrollRestoreKey, view.scrollTop);
      setShowJumpToLatest(!atLatest);
      return;
    }
    const anchor = olderMessagesAnchor.current;
    if (view && anchor?.conversationID === conversationID) {
      view.scrollTop = anchor.top + view.scrollHeight - anchor.height;
      olderMessagesAnchor.current = null;
      followConversation.current = false;
      if (conversationScrollPositionKey) conversationScrollPositions.current.set(conversationScrollPositionKey, view.scrollTop);
      setShowJumpToLatest(true);
      return;
    }
    if (anchor && anchor.conversationID !== conversationID) olderMessagesAnchor.current = null;
    if (view && variantScrollAnchor.current !== null) {
      view.scrollTop = variantScrollAnchor.current;
      variantScrollAnchor.current = null;
      const atLatest = view.scrollHeight - view.scrollTop - view.clientHeight < 80;
      followConversation.current = atLatest;
      if (conversationScrollPositionKey) conversationScrollPositions.current.set(conversationScrollPositionKey, view.scrollTop);
      setShowJumpToLatest(!atLatest);
      return;
    }
    if (view && followConversation.current) {
      view.scrollTop = view.scrollHeight;
      if (conversationScrollPositionKey) conversationScrollPositions.current.set(conversationScrollPositionKey, view.scrollTop);
      setShowJumpToLatest(false);
    }
  }, [conversationID, conversationScrollPositionKey, messages.data, displayMessages.length, latestDisplayMessageContent,
    outbox.entries.length, historyPageLoadVersion, variantSelectionSignature]);

  useEffect(() => {
    if (conversationID && finishedTaskIDs) {
      void client.invalidateQueries({ queryKey: ['conversation-messages', conversationID] });
      void client.invalidateQueries({ queryKey: ['conversation-tasks', conversationID] });
    }
  }, [finishedTaskIDs, client, conversationID]);

  useEffect(() => {
    if (!hasNavigationIdea) return;
    // A routed prompt can arrive after mount; preserve an existing user draft.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setContent((current) => current || ideaPrompt);
    navigate(`${location.pathname}${location.search}${location.hash}`, { replace: true, state: null });
  }, [hasNavigationIdea, ideaPrompt, location.hash, location.pathname, location.search, navigate]);

  const pendingMessages = outbox.entries.filter(({ message }) => !message.projectID && (conversationID
    ? message.conversationID === conversationID
    : Boolean(selectedLocalDraftKey) && message.conversationCreateKey === selectedLocalDraftKey));
  const composerDraftReady = !composerDraftIdentity || hydratedComposerDraftKey === composerDraftIdentity;
  const visibleComposerContent = composerDraftReady ? content : '';
  const resizeComposer = useCallback(() => {
    const textarea = composerTextarea.current;
    if (!textarea) return;
    const lineHeight = Number.parseFloat(getComputedStyle(textarea).lineHeight) || 24;
    const minHeight = lineHeight * 2;
    const maxHeight = lineHeight * 6;
    textarea.style.height = 'auto';
    const contentHeight = textarea.scrollHeight;
    textarea.style.height = `${Math.min(Math.max(contentHeight, minHeight), maxHeight)}px`;
    textarea.style.overflowY = contentHeight > maxHeight ? 'auto' : 'hidden';
    const view = conversationView.current;
    if (view && followConversation.current) {
      view.scrollTop = view.scrollHeight;
      if (conversationScrollPositionKey) conversationScrollPositions.current.set(conversationScrollPositionKey, view.scrollTop);
    }
  }, [conversationScrollPositionKey]);
  useLayoutEffect(resizeComposer, [resizeComposer, visibleComposerContent]);
  useEffect(() => {
    const textarea = composerTextarea.current;
    if (!textarea || typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver(resizeComposer);
    observer.observe(textarea);
    return () => observer.disconnect();
  }, [resizeComposer]);
  const incompleteAttachmentPreparationKey = composerAttachments.filter((item) => item.needsPreparation && !item.preparationCancelled).map((item) => item.id).join(',');
  useEffect(() => {
    if (!composerDraftReady || !composerDraftID || !incompleteAttachmentPreparationKey) return;
    void prepareComposerAttachmentItems(composerAttachments.filter((item) => item.needsPreparation && !item.preparationCancelled), composerDraftScope, composerDraftID);
  }, [composerAttachments, composerDraftID, composerDraftIdentity, composerDraftReady, composerDraftScope, incompleteAttachmentPreparationKey, prepareComposerAttachmentItems]);
  const renderPendingMessage = (entry: MessageOutboxEntry) => {
    const { message } = entry;
    const alreadyVisible = displayMessages.some((item) => item.client_message_id === message.request.client_message_id);
    if (alreadyVisible) return null;
    const content = message.request.content || (message.attachments.length ? '发送了附件' : '');
    const createdAt = new Date(message.createdAt).toISOString();
    return <div className="chat-message-row chat-message-row-user chat-message-row-pending" key={`outbox:${message.request.client_message_id}`}>
      <div className="chat-message-body">
        <article className="chat-message chat-message-user" aria-label="你的消息" tabIndex={0}>
          <UserMessageContent content={content} />
        </article>
        <div className="chat-message-meta">
          <time className="chat-message-time" dateTime={createdAt} aria-label={`消息时间：${formatDate(createdAt)}`}>{formatDate(createdAt)}</time>
          <button type="button" className="chat-message-copy" aria-label="复制你的消息" title="复制消息"
            onClick={() => void copyConversationMessage(content, 'user')}><MordeIcon name="copy" /></button>
        </div>
      </div>
    </div>;
  };
  const renderTaskDetail = (summary: TaskSummary) => {
    const task = allTasksByID.get(summary.id);
    const taskQuery = taskQueries[taskIDs.indexOf(summary.id)];
    if (!task) return isResourceNoLongerAccessible(taskQuery?.error)
      ? <p className="muted" role="status">任务详情不可访问或已删除。</p>
      : taskQuery?.isError && !isTransientNetworkError(taskQuery.error)
      ? <FloatingError className="error-inline task-detail-load-error" role="alert">任务工作过程暂不可用。
        <button type="button" className="button button-small button-secondary" onClick={() => void taskQuery.refetch()}>重试</button></FloatingError>
      : <p className="muted" role="status">正在读取任务工作过程…</p>;

    if (taskDetailStepID) {
      const step = task.steps.find((item) => item.id === taskDetailStepID);
      if (!step) return <p className="muted" role="status">此工作项已不在当前任务记录中。</p>;
      const text = taskActivitySnippet(taskActivityStepSummary(step), 280) ?? '暂无可展示的步骤结果。';
      return <div className="task-work-step-detail" aria-label="工作项详情">
        <p className="task-step-detail-message" role="status">状态：{taskStepStatusText(step.status)}</p>
        {step.status === 'failed' && <p className="task-step-detail-message" role="status">原因：{taskStepFailureText(step)}</p>}
        {step.kind === 'tool' || step.tool_name
          ? <TaskStepDetails task={task} step={step} conversationID={task.conversation_id} actorID={user?.id ?? ''}
            serviceInstanceID={capabilities.data?.service_instance_id ?? ''} />
          : <div className="task-step-detail" aria-label="步骤结果"><p>{text}</p></div>}
      </div>;
    }

    const groups = taskAgentActivityGroups(task);
    const isRunning = taskActivityIsExecuting(task.status);
    const title = taskActivityTitle(task.objective);
    const taskStatus = taskStatusText(task.status);
    const showFeedStatus = !terminalTaskStatuses.has(task.status) ||
      (task.status === 'completed' && (task.result_message_pending || ['pending', 'processing'].includes(task.result_review?.state ?? '')));
    const projectionRead = readK2TaskProjection(task);
    const projection = projectionRead.kind === 'available' ? projectionRead.projection : null;
    const review = projection?.result_review ?? null;
    const publishedReplyVisible = Boolean(review?.state === 'published' && review.source_task_id === task.id && review.reply_message_id &&
      (task.conversation_id !== conversationID || displayMessages.some((message) => message.id === review.reply_message_id && message.role === 'assistant' &&
        message.sources?.some((source) => source.task_id === review.source_task_id &&
          source.task_revision === review.source_task_revision && source.event_sequence === review.source_event_sequence))));
    const verifiedArtifacts = task.artifacts.filter(hasCompleteVerifiedArtifactProof);
    const hasUnverifiedArtifacts = verifiedArtifacts.length < task.artifacts.length;
    return <section className="task-activity" data-task-id={task.id} aria-label={`任务详情：${title}`}>
      <header className="task-activity-heading">
        <div><strong>{title}</strong><small>更新于 {formatDate(task.updated_at)}</small></div>
        <span className={`task-activity-status task-dynamics-status ${taskStatusClass(task.status)} ${isRunning ? 'is-running' : 'is-stopped'}`}>
          {taskStatus}
        </span>
      </header>
      {projection && (projection.dependencies.length > 0 || projection.child_jobs.length > 0) && <section className="task-activity-k2" aria-label="任务依赖与子任务">
        <h3>任务依赖与子任务</h3>
        {projection.dependencies.map((dependency) => <p key={`${dependency.task_id}:${dependency.required_revision}`}>
          {dependency.satisfied ? '依赖已满足' : '依赖任务未就绪'} · {dependency.task_id} · 修订 {dependency.required_revision}
        </p>)}
        {projection.child_jobs.map((job) => <p key={job.job_id}>{job.title} · {childJobStatusText(job.status)}</p>)}
      </section>}
      {showFeedStatus && ['disconnected', 'resyncing'].includes(taskEventFeeds[task.id] ?? '') && !showConsolidatedNetworkError &&
        <p role="status">连接中断，正在重连…</p>}
      {task.status === 'failed' && <p role="alert">任务执行失败，请查看当前状态和处理提示。</p>}
      {task.waiting_reason === 'clarification_required' && task.clarification_request?.question && <section className="task-activity-k2 task-activity-clarification" aria-label="待补充信息">
        <h3>等待补充信息</h3>
        <p>{safeTaskActivityText(task.clarification_request.question, 1_000).value}</p>
        <p className="muted">请直接在对话中回复，Morde 会按当前任务继续处理。</p>
      </section>}
      {review && <section className="task-activity-k2 task-activity-review" aria-label="结果整理">
        <h3>结果整理</h3>
        <p role={review.state === 'failed' ? 'alert' : 'status'}>{resultReviewStatusText(review, publishedReplyVisible)}</p>
        {publishedReplyVisible && review.reply_message_id && <a href={`#conversation-message-${encodeURIComponent(review.reply_message_id)}`}>查看最终回复</a>}
      </section>}
      {(verifiedArtifacts.length > 0 || hasUnverifiedArtifacts) && <section className="task-activity-k2 task-activity-artifacts" aria-label="任务成果">
        <h3>任务成果</h3>{verifiedArtifacts.length > 0 && <ul>{verifiedArtifacts.map((artifact) => <li key={artifact.id}>
          <strong>{artifact.label}</strong><small>{artifact.path} · {artifact.media_type} · {formatBytes(artifact.size_bytes!)}</small>
          <a className="task-activity-link" href={portalApi.taskArtifactContentURL(task.id, artifact.id)} download
            aria-label={`下载任务成果 ${artifact.label}`}>下载成果 ↓</a>
        </li>)}</ul>}
        {hasUnverifiedArtifacts && <p role="status">有成果尚未通过完整核验，当前不提供下载。可查看相关工作项。</p>}
      </section>}
      <section className="task-detail-work" aria-label="任务工作项">
        {groups.length ? groups.map((group) => <section className="task-agent-group" key={group.id}>
          <h4><strong>{group.title}</strong><span aria-hidden="true" /></h4>
          {group.currentActivity && <p className="muted task-agent-current-activity" role="status">{group.currentActivity}</p>}
          {group.steps.map((step) => {
            const stepTitleText = safeTaskActivityText(conversationTitleFromText(taskActivityStepTitle(step)) ?? 'Agent 工作项', 120).value;
            const stepSummaryText = taskActivitySnippet(taskActivityStepSummary(step), 120) ?? '';
            const statusID = `task-step-status-${task.id}-${step.id}`;
            return <button key={step.id} type="button" className="task-activity-step-button task-work-item"
              aria-label={`查看工作项：${stepTitleText}`} aria-describedby={statusID} onClick={() => setTaskDetailStepID(step.id)}>
              <span className={`task-step-icon status-${step.status}`} aria-hidden="true" />
              <span className="task-work-copy"><strong>{stepTitleText}</strong>
                {stepSummaryText && <small>{stepSummaryText}</small>}
              </span>
              <span className="task-work-status" id={statusID}>{taskStepStatusText(step.status)}</span>
              <span className="task-work-arrow" aria-hidden="true">›</span>
            </button>;
          })}
        </section>) : <p className="muted task-work-empty">暂无已记录的工作项。</p>}
      </section>
    </section>;
  };

  const renderTaskSummary = (summary: TaskSummary) => {
    const task = allTasksByID.get(summary.id);
    const title = taskActivityTitle(task?.objective ?? summary.objective);
    const status = task?.status ?? summary.status;
    const statusText = taskStatusText(status);
    const updatedAt = task?.updated_at ?? summary.updated_at;
    const isRunning = taskActivityIsExecuting(status);
    const subtitle = taskActivityLatestWorkText(summary, task);
    return <article className="task-activity-card" data-task-id={summary.id} key={summary.id}>
      <button ref={(button) => { if (button) taskCardButtons.current.set(summary.id, button); else taskCardButtons.current.delete(summary.id); }}
        type="button" className="task-activity-card-button"
        aria-label={`查看任务：${title}，${statusText}`}
        onClick={() => {
          if (summary.conversation_id !== conversationID) {
            setTaskDetailStepID(null);
            setConversationSelection(null);
            navigate('/assistant', { state: { conversationID: summary.conversation_id, taskID: summary.id } });
          } else {
            setTaskDetailStepID(null);
            setTaskDetailTaskID(summary.id);
          }
        }}>
        <span className={`task-activity-state-icon ${isRunning ? 'is-running' : 'is-stopped'}`} aria-hidden="true">
          {isRunning ? <><span className="task-activity-state-orbit" />
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeLinecap="round" strokeWidth="2.2">
              <path d="M7 15v-4m5 4V7m5 8v-5" /></svg></>
            : <svg viewBox="0 0 24 24" aria-hidden="true"><rect x="7.5" y="7.5" width="9" height="9" rx="2.2" fill="currentColor" /></svg>}
        </span>
        <span className="task-activity-card-copy"><strong>{title}</strong>
          <small>{subtitle}</small><time dateTime={updatedAt}>{formatDate(updatedAt)}</time></span>
        <span className="task-work-arrow" aria-hidden="true">›</span>
      </button>
    </article>;
  };
  const selectedTaskSummary = taskDetailTaskID ? taskPanelTasks.find((item) => item.id === taskDetailTaskID) ?? null : null;
  const selectedTaskQuery = taskDetailTaskID ? taskQueries[taskIDs.indexOf(taskDetailTaskID)] : undefined;
  const mainConversationNeedsSelection = mainConversation.data?.status === 'selection_required';
  const selectedConversationTitle = selected && !isPlaceholderConversationTitle(selected.title) ? selected.title
    : conversationTitleFromMessages(conversationMessageItems) ?? (conversationID ? conversationPreviewTitles.get(conversationID) : null)
      ?? (conversationID ? '对话' : selectedLocalDraftKey ? '新旁聊' : mainConversationNeedsSelection ? '选择主会话'
        : mainConversation.isError && !isTransientNetworkError(mainConversation.error) ? '主会话暂不可用' : '主会话');
  const showConversationWelcome = Boolean(conversationID && messages.isSuccess && displayMessages.length === 0 && pendingMessages.length === 0);
  const conversationWelcome = <div className="assistant-welcome"><span className="assistant-mark assistant-logo" aria-hidden="true" />
    {mainConversationNeedsSelection ? <><h2>选择一条旧对话</h2><p>检测到多条旧个人对话。请从左侧旁聊中选择一条设为主会话；其他记录不会被合并或删除。</p></>
      : selectedLocalDraftKey || mainConversation.data?.status === 'ready' ? <><h2>今天想聊些什么？</h2><p>告诉我你想完成的事情。</p></>
        : mainConversation.isError && !isTransientNetworkError(mainConversation.error) ? <><h2>暂时无法连接</h2><p>暂时无法确认主会话状态。重试读取不会删除已有对话。</p>
          <button type="button" className="button button-small button-secondary" aria-label="重试读取主会话" onClick={() => void mainConversation.refetch()}>重试</button></>
          : mainConversation.data?.status === 'uninitialized' ? <><h2>正在初始化主会话…</h2><p>正在确认会话状态，已有对话会保留。</p></>
            : <><h2>正在读取主会话…</h2><p>正在读取服务端会话状态。</p></>}
  </div>;
  const contextSidebarWidthLimits = useCallback(() => {
    const layout = assistantLayoutRef.current;
    const columns = layout ? getComputedStyle(layout).gridTemplateColumns.trim().split(/\s+/) : [];
    const panel = layout?.querySelector<HTMLElement>('.assistant-task-panel');
    const panelWidth = columns.length > 2 ? Number.parseFloat(columns[columns.length - 1] ?? '') || 0
      : panel && getComputedStyle(panel).position !== 'absolute' ? panel.getBoundingClientRect().width : 0;
    const maximum = Math.min(420, (layout?.getBoundingClientRect().width ?? 900) - panelWidth - 320);
    return { minimum: 170, maximum: Math.max(170, maximum) };
  }, []);
  const resizeContextSidebar = (width: number) => {
    const { minimum, maximum } = contextSidebarWidthLimits();
    setContextSidebarWidth(Math.round(Math.min(maximum, Math.max(minimum, width))));
  };
  const workspacePanelWidthLimits = useCallback(() => {
    const layout = assistantLayoutRef.current;
    const sidebarWidth = layout?.querySelector<HTMLElement>('.assistant-sidebar')?.getBoundingClientRect().width ?? contextSidebarWidth;
    const layoutWidth = layout?.getBoundingClientRect().width ?? 1200;
    const panelColumnReserved = Boolean(layout && getComputedStyle(layout).gridTemplateColumns.trim().split(/\s+/).length > 2);
    const maximum = window.innerWidth <= 1100
      ? Math.min(380, layoutWidth - sidebarWidth)
      : Math.min(900, layoutWidth - (panelColumnReserved ? sidebarWidth + 320 : 12));
    return { minimum: 350, maximum: Math.max(350, maximum) };
  }, [contextSidebarWidth]);
  const currentWorkspacePanelWidth = useCallback(() => {
    if (workspacePanelWidth !== null) return Math.round(workspacePanelWidth);
    const layout = assistantLayoutRef.current;
    const columns = layout ? getComputedStyle(layout).gridTemplateColumns.trim().split(/\s+/) : [];
    if (columns.length > 2) return Math.round(Number.parseFloat(columns[columns.length - 1] ?? '') || 560);
    const panel = layout?.querySelector<HTMLElement>('.assistant-task-panel');
    return Math.round(panel?.getBoundingClientRect().width || workspacePanelWidth || 560);
  }, [workspacePanelWidth]);
  useLayoutEffect(() => {
    const layout = assistantLayoutRef.current;
    if (!layout) return;
    const measure = () => {
      const next = {
        contextSidebarMaximum: contextSidebarWidthLimits().maximum,
        workspacePanelMaximum: workspacePanelWidthLimits().maximum,
        workspacePanelWidth: currentWorkspacePanelWidth(),
      };
      setDividerMeasurements((current) => current.contextSidebarMaximum === next.contextSidebarMaximum &&
        current.workspacePanelMaximum === next.workspacePanelMaximum && current.workspacePanelWidth === next.workspacePanelWidth
        ? current : next);
    };
    measure();
    const observer = typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(measure);
    observer?.observe(layout);
    const taskPanel = layout.querySelector<HTMLElement>('.assistant-task-panel');
    if (taskPanel) observer?.observe(taskPanel);
    window.addEventListener('resize', measure);
    return () => {
      observer?.disconnect();
      window.removeEventListener('resize', measure);
    };
  }, [contextSidebarWidthLimits, currentWorkspacePanelWidth, taskPanelOpen, taskPanelTab, workspacePanelWidth, workspacePanelWidthLimits]);
  const resizeWorkspacePanel = (width: number) => {
    const { minimum, maximum } = workspacePanelWidthLimits();
    setWorkspacePanelWidth(Math.round(Math.min(maximum, Math.max(minimum, width))));
  };
  const dividerWidth = (side: 'sidebar' | 'workspace') => side === 'sidebar' ? contextSidebarWidth : currentWorkspacePanelWidth();
  const resizeDivider = (side: 'sidebar' | 'workspace', width: number) => {
    if (side === 'sidebar') resizeContextSidebar(width);
    else resizeWorkspacePanel(width);
  };
  const beginDividerDrag = (side: 'sidebar' | 'workspace', pointerID: number, x: number) => {
    assistantDividerDrag.current = { side, pointerID, x, width: dividerWidth(side) };
  };
  const moveDividerDrag = (pointerID: number, x: number, finish = false) => {
    const drag = assistantDividerDrag.current;
    if (drag?.pointerID !== pointerID) return;
    const delta = x - drag.x;
    resizeDivider(drag.side, drag.width + (drag.side === 'sidebar' ? delta : -delta));
    if (finish) assistantDividerDrag.current = null;
  };
  const handleDividerKeyDown = (event: ReactKeyboardEvent<HTMLButtonElement>, side: 'sidebar' | 'workspace') => {
    const current = dividerWidth(side);
    if (event.key === 'ArrowLeft' || event.key === 'ArrowRight') {
      const delta = event.key === 'ArrowRight' ? 16 : -16;
      resizeDivider(side, current + (side === 'sidebar' ? delta : -delta));
      event.preventDefault();
    } else if (event.key === 'Home' || event.key === 'End') {
      const { minimum, maximum } = side === 'sidebar' ? contextSidebarWidthLimits() : workspacePanelWidthLimits();
      resizeDivider(side, event.key === 'Home' ? minimum : maximum);
      event.preventDefault();
    }
  };
  const computerSessionMatches = Boolean(computerEmbed && computerEmbed.conversationID === conversationID &&
    computerEmbed.hostID === selectedComputerHost?.id);
  const computerSession = computerSessionMatches ? computerEmbed : null;
  const computerViewportMessage = computerEmbedFailed ? computerEmbed?.error || '无法连接云端电脑。'
    : computerPanelError || (!conversationID ? '发送一条消息后连接当前会话的云端电脑。'
      : preferences.isPending ? '正在读取云端电脑设置…'
        : !selectedComputerHost ? preferredHostID && hosts.isPending ? '正在检查默认云端电脑…' : '尚未配置默认云端电脑。'
          : selectedComputerHost.state !== 'running' ? `云端电脑当前${hostStateLabel[selectedComputerHost.state] || '不可用'}。`
            : !selectedComputerTargets.includes('browser') ? '当前账号没有查看此电脑桌面的权限。'
              : computerConnecting || !computerSession ? '正在连接云端电脑…'
                : computerSession.status === 'error' ? computerSession.error || '工作台连接失败。'
                  : !computerSession.native && computerSession.status !== 'ready' ? '正在加载云端电脑工作台…' : '');
  const computerViewportConnectionError = Boolean(computerEmbedFailed || computerSession &&
    computerSession.activeTarget === 'browser' && ['disconnected', 'failed'].includes(computerSession.remoteDesktop));
  const computerWorkbenchReady = Boolean(computerSession?.status === 'ready');

  return <section className="assistant-page">
    <div ref={assistantLayoutRef} style={{ '--pc-context-sidebar-width': `${contextSidebarWidth}px`,
      ...(!outletContext ? { '--pc-workspace-panel-width': `${workspacePanelWidth ?? dividerMeasurements.workspacePanelWidth}px` } : {}) } as CSSProperties}
      onPointerMove={(event) => moveDividerDrag(event.pointerId, event.clientX)}
      onPointerUp={(event) => moveDividerDrag(event.pointerId, event.clientX, true)}
      onMouseMove={(event) => moveDividerDrag(-1, event.clientX)}
      onMouseUp={(event) => moveDividerDrag(-1, event.clientX, true)}
      className={`assistant-layout${taskPanelOpen ? ' assistant-layout-with-task-panel' : ''}${taskPanelOpen && taskPanelTab === 'computer' ? ' assistant-layout-with-computer-panel' : ''}${taskPanelOpen && taskPanelTab === 'computer' && computerControlExpanded ? ' assistant-layout-computer-expanded' : ''}`}>
      <aside id="context-sidebar" className="assistant-sidebar" aria-label="对话">
        <div className="assistant-sidebar-heading">
          <h2>Morde</h2>
          <button type="button" className="icon-button" aria-label="搜索会话" title="搜索会话" aria-expanded={conversationSearchVisible}
            onClick={() => { setConversationSearchVisible((visible) => !visible); if (conversationSearchVisible) setConversationSearch(''); }}><MordeIcon name="search" /></button>
        </div>
        <div id="conversation-history" className="assistant-history-content">
          {mainConversationID && (mainConversationItem ? <AssistantConversationRow item={mainConversationItem} isMain selected={conversationID === mainConversationItem.id}
            recent={false} onSelect={selectConversationID} />
            : renderCachedMainConversation())}
          {!mainConversationID && (mainConversation.isPending || isTransientNetworkError(mainConversation.error)) && <p className="muted" role="status">正在读取主会话…</p>}
          {!mainConversationID && mainConversation.isError && !isTransientNetworkError(mainConversation.error) && <div className="conversation-row conversation-row-main">
            <button type="button" className="conversation-item" aria-label="重试读取主会话" onClick={() => void mainConversation.refetch()}>
              <strong>主会话</strong><small>暂时无法读取 · 点击重试</small>
            </button>
          </div>}
          {!mainConversationID && mainConversation.data?.status === 'uninitialized' && <div className="conversation-row conversation-row-main" role="status">
            <div className="conversation-item"><strong>主会话</strong>
              <small>正在初始化…</small></div>
          </div>}
          {mainConversationNeedsSelection && <div className="conversation-row conversation-row-main conversation-row-main-recovery">
            <div className="conversation-item" role="status"><strong>主会话待恢复</strong>
              <small>选择旧对话设为主会话，其余记录保留。</small></div>
            <button type="button" className="button button-small button-secondary" aria-label="查看可设为主会话的旧对话"
              onClick={() => setSideConversationsOpen(true)}>查看旧对话</button>
          </div>}
          {(conversations.isPending || isTransientNetworkError(conversations.error)) && !conversations.data && <p className="muted" role="status">正在读取旁聊…</p>}
          {conversations.isError && !conversations.data && !isTransientNetworkError(conversations.error) && <p className="muted" role="status">旁聊历史暂不可用</p>}
          {conversationSearchVisible && <label className="conversation-search">
            <span className="sr-only">搜索旁聊标题</span>
            <input type="search" aria-label="搜索旁聊标题" placeholder="搜索旁聊标题" value={conversationSearch}
              onChange={(event) => { setConversationSearch(event.currentTarget.value); setVisibleSideConversationCount(5);
                if (event.currentTarget.value.trim()) setSideConversationsOpen(true); }} />
          </label>}
          <div className="conversation-group">
            <details className="conversation-group-details" open={sideConversationsOpen || mainConversationNeedsSelection} onToggle={(event) => setSideConversationsOpen(event.currentTarget.open)}>
              <summary>旁聊</summary>
              <div className="conversation-list">
                {localComposerDrafts.slice(0, visibleSideConversationCount).map(renderLocalDraftItem)}
                {sideConversations.slice(0, visibleSideConversationCount).map((item) => <AssistantConversationRow key={item.id} item={item}
                  selected={conversationID === item.id} recent={recentConversationID === item.id} previewTitle={conversationPreviewTitles.get(item.id)} onSelect={selectConversationID}
                  onMakeMain={mainConversationNeedsSelection ? makeMainConversation : undefined} makeMainPending={makeMainConversationMutation.isPending} />)}
              </div>
              {composerDrafts.isError && <FloatingError className="error-inline" role="alert">未发送草稿暂不可用：{errorDetails(composerDrafts.error).message}
                <button type="button" className="button button-small button-secondary" onClick={() => void composerDrafts.refetch()}>重试</button></FloatingError>}
              {composerDraftError && <FloatingError className="error-inline" role="alert">草稿保存状态：{composerDraftError}</FloatingError>}
              {conversations.isFetchNextPageError && !isTransientNetworkError(conversations.error) && <FloatingError className="error-inline" role="alert">{errorDetails(conversations.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void conversations.fetchNextPage()}>重试</button></FloatingError>}
              {(sideConversations.length > visibleSideConversationCount || localComposerDrafts.length > visibleSideConversationCount || conversations.hasNextPage) && <button type="button" className="button button-secondary button-small conversation-more"
                disabled={conversations.isFetchingNextPage} onClick={() => void showMoreSideConversations()}>
                {conversations.isFetchingNextPage ? '正在加载…' : '更多'}
              </button>}
              {!conversations.isError && conversations.isSuccess && !sideConversations.length && !localComposerDrafts.length && !conversations.hasNextPage && !composerDrafts.isPending && <p className="muted assistant-sidebar-empty">{conversationSearch.trim() ? '没有找到匹配标题的旁聊。' : '还没有旁聊。'}</p>}
              {composerDrafts.isPending && !localComposerDrafts.length && <p className="muted assistant-sidebar-empty" role="status">正在读取未发送草稿…</p>}
            </details>
            <button type="button" className="icon-button conversation-new" aria-label="新建旁聊" title="新建旁聊" onClick={startNew}><MordeIcon name="plus" /></button>
          </div>
          <p className="assistant-sidebar-hint">主会话保留日常交流，旁聊可用于单独讨论。</p>
        </div>
      </aside>
      <button type="button" className="assistant-context-resizer" role="separator" aria-orientation="vertical"
        aria-label="调整对话侧栏宽度" aria-valuemin={170} aria-valuemax={dividerMeasurements.contextSidebarMaximum} aria-valuenow={contextSidebarWidth}
        aria-valuetext={`${contextSidebarWidth} 像素`} title="拖动调整对话侧栏宽度"
        onPointerDown={(event) => {
          if (event.button !== 0) return;
          event.preventDefault();
          beginDividerDrag('sidebar', event.pointerId, event.clientX);
          event.currentTarget.setPointerCapture(event.pointerId);
        }}
        onPointerCancel={() => { assistantDividerDrag.current = null; }}
        onMouseDown={(event) => {
          if (event.button !== 0 || assistantDividerDrag.current) return;
          event.preventDefault();
          beginDividerDrag('sidebar', -1, event.clientX);
        }}
        onKeyDown={(event) => handleDividerKeyDown(event, 'sidebar')} />
      {taskPanelOpen && <button type="button" className="assistant-workspace-resizer" role="separator" aria-orientation="vertical"
        aria-label="调整右侧工作区宽度" aria-valuemin={350} aria-valuemax={dividerMeasurements.workspacePanelMaximum} aria-valuenow={dividerMeasurements.workspacePanelWidth}
        aria-valuetext={`${dividerMeasurements.workspacePanelWidth} 像素`} title="拖动调整右侧工作区宽度"
        onPointerDown={(event) => {
          if (event.button !== 0) return;
          event.preventDefault();
          beginDividerDrag('workspace', event.pointerId, event.clientX);
          event.currentTarget.setPointerCapture(event.pointerId);
        }}
        onPointerCancel={() => { assistantDividerDrag.current = null; }}
        onMouseDown={(event) => {
          if (event.button !== 0 || assistantDividerDrag.current) return;
          event.preventDefault();
          beginDividerDrag('workspace', -1, event.clientX);
        }}
        onKeyDown={(event) => handleDividerKeyDown(event, 'workspace')} />}
      <div className="assistant-main">
        {!taskPanelOpen && !outletContext?.focusPanelToggle && <button ref={taskPanelToggle} type="button" className="button button-secondary task-panel-toggle"
          aria-label="打开侧边栏"
          aria-expanded="false" aria-controls="assistant-task-panel"
          title="打开侧边栏"
          onClick={() => setTaskPanelOpen(true)}>
          <MordeIcon name="panel" />
        </button>}
        <header className="conversation-view-heading assistant-chat-heading-accessible sr-only">
          <h2>{conversationID && conversationID === mainConversationID ? '主会话' : selectedConversationTitle}</h2>
          <p>{conversationID && conversationID === mainConversationID ? '与 Morde 对话' : selectedLocalDraftKey || conversationID ? '旁聊' : '与 Morde 对话'}</p>
        </header>
        {streamedTasks.map((task) => <TaskStream key={task.id} task={task} conversationID={task.conversation_id} onState={setTaskEventFeed} />)}
        <div className={`conversation-view${!conversationID || showConversationWelcome ? ' conversation-view-welcome' : ''}`} aria-live="polite" ref={conversationView} onScroll={(event) => {
          const view = event.currentTarget;
          const atLatest = view.scrollHeight - view.scrollTop - view.clientHeight < 80;
          followConversation.current = atLatest;
          if (conversationScrollPositionKey) conversationScrollPositions.current.set(conversationScrollPositionKey, view.scrollTop);
          setShowJumpToLatest(!atLatest);
          if (view.scrollTop <= 12 && !messages.isFetchNextPageError) void loadOlderMessages();
        }}>
          {!conversationID ? <>{conversationWelcome}{pendingMessages.map(renderPendingMessage)}</> : <>
          {conversationContentAccessRevoked ? <div className="empty-card" role="alert">
            <strong>当前对话内容暂不可访问。</strong><p className="muted">可以重试，或从左侧切换其他会话。</p>
            <button type="button" className="button button-secondary" onClick={() => void messages.refetch()}>重试读取对话</button>
          </div> : messages.isPending && displayMessages.length === 0 ? <div className="loading-card" role="status">正在加载消息…</div>
            : messages.isError && displayMessages.length === 0 ? isTransientNetworkError(messages.error) ? <div className="loading-card" role="status">正在加载消息…</div>
              : <FloatingError className="error-card" role="alert">{errorDetails(messages.error).message}<button type="button" className="button button-secondary" onClick={() => void messages.refetch()}>重试</button></FloatingError>
              : <div className="message-list">
                {showConversationWelcome && conversationWelcome}
                {messages.isError && !isTransientNetworkError(messages.error) && <FloatingError className="error-inline" role="alert">{errorDetails(messages.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void messages.refetch()}>重试</button></FloatingError>}
                {messages.hasNextPage && <div className="conversation-history-load">
                  {messages.isFetchNextPageError && !isTransientNetworkError(messages.error) && <FloatingError className="error-inline" role="alert">更早的消息暂不可用：{errorDetails(messages.error).message}</FloatingError>}
                  <button type="button" className="button button-small button-secondary" disabled={messages.isFetchingNextPage}
                    onClick={() => void loadOlderMessages()}>{messages.isFetchingNextPage ? '正在加载更早消息…' : messages.isFetchNextPageError ? '重试加载更早消息' : '加载更早消息'}</button>
                </div>}
                {displayMessages.map((message, index) => {
                  const messageTaskID = message.task_id ?? message.related_task_id ?? '';
                  const messageTask = tasksByID.get(messageTaskID);
                  const activeClarification = messageTask?.conversation_id === conversationID &&
                    canAnswerClarification(messageTask)
                    ? messageTask.clarification_request : null;
                  const savedClarificationInteraction = messageTaskID ? clarificationInteractions[messageTaskID] : undefined;
                  const clarificationInteraction = activeClarification && savedClarificationInteraction &&
                    (savedClarificationInteraction.clarificationID !== activeClarification.id || savedClarificationInteraction.version !== activeClarification.version)
                    ? undefined : savedClarificationInteraction;
                  const clarificationDisplay = activeClarification ?? (clarificationInteraction ? {
                    id: clarificationInteraction.clarificationID,
                    version: clarificationInteraction.version,
                    question: clarificationInteraction.question,
                  } : null);
                  const clarificationDraftID = activeClarification
                    ? clarificationDraftKey(messageTaskID, activeClarification.id, activeClarification.version) : '';
                  const clarificationAnswer = clarificationDraftID ? clarificationAnswers[clarificationDraftID] ?? '' : '';
                  const fileDeliveryClarification = isFileDeliveryIntentClarification(activeClarification?.call_id);
                  const clarificationFileDeliveryIntent = fileDeliveryClarification
                    ? clarificationFileDeliveryIntents[clarificationDraftID] : undefined;
                  const showRoutingFailure = isUnlinkedRoutingFailure(message);
                  const routingRetryActorMatches = conversationMessageActorID(message) === user?.id;
                  const retryEligibilityQuery = routingRetryEligibilityByMessageID.get(message.id);
                  const canRetryRoutingFailure = showRoutingFailure && routingRetryActorMatches && routingRetryCapabilityConfirmed
                    && retryEligibilityQuery?.isSuccess === true && !retryEligibilityQuery.isFetching
                    && retryEligibilityAllowsDispatch(retryEligibilityQuery.data);
                  const retryingRoutingFailure = retryConversationMessageProcessing.isPending
                    && retryConversationMessageProcessing.variables?.messageID === message.id;
                  const routingFailureNextStepText = routingFailureNextStep(message, routingRetryActorMatches,
                    routingRetryCapabilityState, retryEligibilityQuery);
                  const showClarificationCard = message.role === 'user' && Boolean(messageTaskID) &&
                    latestUserMessageByTaskID.get(messageTaskID) === message.id && Boolean(clarificationDisplay);
                  const previous = displayMessages[index - 1];
                  const messageDate = formatDate(message.created_at).split(' ')[0];
                  const showDate = !previous || formatDate(previous.created_at).split(' ')[0] !== messageDate;
                  return <Fragment key={message.id}>
                    {showDate && <div className="conversation-time-divider"><time dateTime={message.created_at}>{messageDate}</time></div>}
                    <div className={`chat-message-row chat-message-row-${message.role}`}>
                    {message.role === 'assistant' && <span className="assistant-logo assistant-logo-message" aria-hidden="true" />}
                    <div className="chat-message-body">
                    {message.role === 'assistant' && <strong className="chat-message-author">Morde</strong>}
                    <article id={`conversation-message-${encodeURIComponent(message.id)}`} className={`chat-message chat-message-${message.role}`}
                      aria-label={message.role === 'assistant' ? 'Morde 的回复' : '你的消息'} tabIndex={0}>
                      {message.role === 'assistant' ? <ChatMarkdown content={message.content} />
                        : <UserMessageContent content={message.content} attachmentIDs={message.attachment_ids ?? []} />}
                    </article>
                    <div className="chat-message-meta">
                      <time className="chat-message-time" dateTime={message.created_at}
                        aria-label={`${message.role === 'assistant' ? '回复' : '消息'}时间：${formatDate(message.created_at)}`}>{formatDate(message.created_at)}</time>
                      <button type="button" className="chat-message-copy"
                        aria-label={message.role === 'assistant' ? '复制 Morde 回复' : '复制你的消息'}
                        title={message.role === 'assistant' ? '复制回复' : '复制消息'}
                        onClick={() => void copyConversationMessage(message.content, message.role)}><MordeIcon name="copy" /></button>
                    </div>
                    </div>
                    </div>
                    {showRoutingFailure && <div className="chat-message-row chat-message-row-assistant" role="group" aria-label="Morde 的消息处理状态">
                      <span className="assistant-logo assistant-logo-message" aria-hidden="true" />
                      <div className="chat-message-body"><strong className="chat-message-author">Morde</strong>
                        <div className="chat-message-processing-status" role="status" aria-live="polite">
                          <strong className="chat-message-processing-title">刚才没能处理你的消息</strong>
                          <p>{message.routing_failure_reason === 'message_too_long'
                            ? '消息内容或上下文超过当前处理上限，原消息已保留。'
                            : `${routingFailureText(message.routing_failure_reason)}，原消息已保留。`}</p>
                          {routingFailureNextStepText && <p className="chat-message-processing-hint">{routingFailureNextStepText}</p>}
                          {canRetryRoutingFailure && <button type="button" className="button button-secondary chat-message-processing-retry"
                            aria-label="重新处理这条消息" disabled={retryingRoutingFailure}
                            onClick={() => {
                              if (processingRetryInFlight.current.has(message.id)) return;
                              retryConversationMessageProcessing.mutate({ messageID: message.id });
                            }}><MordeIcon name="refresh" />{retryingRoutingFailure ? '正在重试…' : retryEligibilityQuery?.data.retry_warning
                              ? '核对后重试' : '再试一次'}</button>}
                          {routingRetryActorMatches && message.routing_failure_reason !== 'message_too_long'
                            && ((routingRetryCapabilityState === 'unknown' && !capabilities.isFetching)
                              || (routingRetryCapabilityConfirmed && retryEligibilityQuery?.isError && !retryEligibilityQuery.isFetching))
                            && <button type="button" className="button button-secondary chat-message-processing-retry"
                              onClick={() => {
                                if (!routingRetryCapabilityConfirmed) void capabilities.refetch();
                                else void retryEligibilityQuery?.refetch();
                              }}><MordeIcon name="refresh" />重新检查</button>}
                        </div>
                      </div>
                    </div>}
                    {message.role === 'user' && messageTaskID && latestUserMessageByTaskID.get(messageTaskID) === message.id &&
                      messageTask?.pending_request &&
                      (messageTask.pending_request.status !== 'pending' || messageTask.status === 'waiting_user') &&
                      messageTask.conversation_id === conversationID &&
                      <div className="chat-message-row chat-message-row-assistant" role="group" aria-label="Morde 的审批请求">
                        <span className="assistant-logo assistant-logo-message" aria-hidden="true" />
                        <div className="chat-message-body"><strong className="chat-message-author">Morde</strong>
                          <TaskApprovalCard approval={messageTask.pending_request} submitting={respondToApproval.isPending}
                            onDecision={(decision) => submitApprovalDecision(messageTask, decision)} />
                        </div>
                      </div>}
                    {showClarificationCard && clarificationDisplay && <div className="chat-message-row chat-message-row-assistant" role="group" aria-label="Morde 的澄清回复">
                      <span className="assistant-logo assistant-logo-message" aria-hidden="true" />
                      <div className="chat-message-body"><strong className="chat-message-author">Morde</strong>
                        <section className="chat-approval task-approval-card conversation-clarification-card" role="region" aria-label="对话中的澄清请求">
                          <header className="task-approval-card-heading">
                            <h3>需要补充信息</h3>
                            <span>{clarificationInteraction?.status === 'submitted' ? '已提交' : clarificationInteraction?.status === 'sending' ? '正在提交' : '等待回复'}</span>
                          </header>
                          <p>{safeTaskActivityText(clarificationDisplay.question, 1_000).value}</p>
                          {clarificationInteraction?.status === 'submitted' && <p role="status">{clarificationInteraction.message}</p>}
                          {clarificationInteraction?.status === 'sending' && <p role="status">正在提交补充信息…</p>}
                          {clarificationInteraction?.status === 'unknown' && <p role="status">{clarificationInteraction.message}</p>}
                          {(clarificationInteraction?.status === 'failed' || clarificationInteraction?.status === 'revoked') &&
                            <p role="alert">{clarificationInteraction.message}</p>}
                          {activeClarification && clarificationInteraction?.status !== 'submitted' &&
                            clarificationInteraction?.status !== 'sending' && clarificationInteraction?.status !== 'revoked' &&
                            <form aria-label="提交澄清回复" onSubmit={(event) => {
                              event.preventDefault();
                              void submitClarificationAnswer(messageTask!);
                            }}>
                              <label className="sr-only" htmlFor={`clarification-answer-${encodeURIComponent(messageTaskID)}`}>澄清问题答复</label>
                              <textarea id={`clarification-answer-${encodeURIComponent(messageTaskID)}`} aria-label="澄清问题答复"
                                value={clarificationAnswer} maxLength={16_384}
                                onChange={(event) => setClarificationAnswers((current) => ({ ...current, [clarificationDraftID]: event.target.value }))}
                                placeholder="在这里补充信息…" disabled={clarificationInteraction?.status === 'unknown'} />
                              {fileDeliveryClarification && <fieldset aria-label="文件交付方式">
                                <legend>文件交付方式</legend>
                                <label><input type="radio" name={`file-delivery-intent-${encodeURIComponent(messageTaskID)}`}
                                  checked={clarificationFileDeliveryIntent === 'none'}
                                  disabled={clarificationInteraction?.status === 'unknown'}
                                  onChange={() => setClarificationFileDeliveryIntents((current) => ({ ...current, [clarificationDraftID]: 'none' }))} />
                                  只需在聊天中回答</label>
                                {messageTask?.workspace_id ? <label><input type="radio" name={`file-delivery-intent-${encodeURIComponent(messageTaskID)}`}
                                  checked={clarificationFileDeliveryIntent === 'required'}
                                  disabled={clarificationInteraction?.status === 'unknown'}
                                  onChange={() => setClarificationFileDeliveryIntents((current) => ({ ...current, [clarificationDraftID]: 'required' }))} />
                                  需要交付文件</label> : <p role="note">当前任务尚未绑定 Workspace。若需要交付文件，请先绑定 Workspace，再重新提交此澄清。</p>}
                              </fieldset>}
                              <div className="chat-approval-actions">
                                <button type="submit" className="button button-primary"
                                  disabled={!clarificationAnswer.trim() || (fileDeliveryClarification && (!clarificationFileDeliveryIntent ||
                                    (clarificationFileDeliveryIntent === 'required' && !messageTask?.workspace_id)))}>
                                  {clarificationInteraction?.status === 'unknown' ? '使用相同回答重试' : '提交并继续'}
                                </button>
                              </div>
                            </form>}
                        </section>
                      </div>
                    </div>}
                  </Fragment>;
                })}
              </div>}
          {pendingMessages.map(renderPendingMessage)}
          {showConversationThinking && <div className="chat-message-row chat-message-row-assistant chat-thinking-row" role="status" aria-label="Morde 正在思考">
            <span className="assistant-logo assistant-logo-message" aria-hidden="true" />
            <div className="thinking-bubble"><div className="thinking-dots" aria-hidden="true"><span /><span /><span /></div></div>
          </div>}
          </>}
          {models.isError && !isTransientNetworkError(models.error) && <FloatingError className="error-inline" role="alert">无法读取模型：{errorDetails(models.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void models.refetch()}>重试</button></FloatingError>}
          {modelConfigurationKnown && models.data?.configuration_status === 'not_configured' && <FloatingNotice className="notice-warning">还没有配置云端模型。<Link to="/connections">配置模型</Link></FloatingNotice>}
          {modelConfigurationKnown && models.data?.configuration_status === 'configured' && !readyModels.length && <FloatingNotice className="notice-warning">模型已配置，当前没有通过检查的文字模型；消息仍可先发送。<Link to="/connections">检查模型设置</Link></FloatingNotice>}
          {outbox.storageError && <FloatingError className="error-inline" role="alert">本地消息存储不可用：{outbox.storageError}</FloatingError>}
          {variantOutbox.storageError && <FloatingError className="error-inline" role="alert">本地消息版本操作存储不可用：{variantOutbox.storageError}</FloatingError>}
          {execution.isError && !isTransientNetworkError(execution.error) && <FloatingError className="error-inline" role="alert">无法读取 AI 服务状态：{errorDetails(execution.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void execution.refetch()}>重试</button></FloatingError>}
          {cloudTasksAvailable && execution.data && user?.id === execution.data.owner_id && execution.data.state === 'disabled' && <FloatingNotice className="notice-warning">
            云端任务尚未启用；普通文字对话仍可使用。<button type="button" className="button button-small button-secondary" onClick={requestExecutionEnable}>启用云端任务</button>
          </FloatingNotice>}
          {execution.data?.state === 'stopping' && <FloatingNotice className="notice-warning">AI 服务正在停止；消息仍可先发送。</FloatingNotice>}
          {taskPanelOpen && taskPanelTab === 'computer' && hosts.isError && !isTransientNetworkError(hosts.error) && <FloatingError className="error-inline" role="alert">无法读取云端电脑：{errorDetails(hosts.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void hosts.refetch()}>重试</button></FloatingError>}
        </div>
        {showJumpToLatest && conversationID && <div className="assistant-jump-latest"><button type="button" className="button button-secondary"
          onClick={() => { followConversation.current = true; if (conversationView.current) conversationView.current.scrollTop = conversationView.current.scrollHeight; setShowJumpToLatest(false); }}>
          ↓ 回到最新消息</button></div>}
        <section className="assistant-composer" aria-label="发送对话消息">
          {conversationID && messageVariantOperationGroups.length > 0 && <details className="assistant-message-operations">
            <summary>消息版本与恢复</summary>
            <div className="assistant-message-operations-list">
              {messageVariantOperationGroups.map(({ group, groupMessages, interruptedMessages, operations }) => {
                const sourceMessage = groupMessages.find((item) => item.role === 'user');
                const operationLabel = sourceMessage?.content.replace(/\s+/gu, ' ').trim().slice(0, 72) || '对话消息';
                const selectedVariantVersion = selectedMessageVariantVersions[group.id] ?? group.latestVersion;
                const operationIsSending = operations.some((entry) => entry.operation.status === 'sending');
                return <div className="assistant-message-operations-item" key={group.id} aria-label={`消息操作：${operationLabel}`}>
                  <p>{operationLabel}</p>
                  {group.versions.size > 1 && <label className="message-variant-picker">版本
                    <select aria-label={`选择消息版本：${operationLabel}`} value={selectedVariantVersion}
                      onChange={(event) => selectMessageVariantVersion(group.id, Number(event.target.value))}>
                      {[...group.versions.values()].sort((left, right) => left.version - right.version).map((version) => <option key={version.version} value={version.version}>
                        {`v${version.version} · ${messageVariantActionLabel(version.action)}`}
                      </option>)}
                    </select>
                  </label>}
                  {interruptedMessages.map((interruptedMessage) => <button type="button" key={interruptedMessage.id}
                    className="button button-small button-secondary" disabled={operationIsSending}
                    onClick={() => void startVariantOperation(group.id, interruptedMessage.id)}>继续生成</button>)}
                  {operations.map((entry) => <div className="message-variant-operation" role="status" key={entry.operation.input.idempotencyKey}>
                    <span>{entry.operation.status === 'sending' ? '正在创建消息版本…' : entry.operation.status === 'unknown'
                      ? '操作结果待核对' : '消息版本操作待发送。'}</span>
                    {(entry.operation.status === 'unknown' || entry.operation.status === 'pending') && <button type="button" className="button button-small button-secondary"
                      disabled={operationIsSending} onClick={() => void sendVariantOperation(entry)}>核对并重试</button>}
                  </div>)}
                </div>;
              })}
            </div>
          </details>}
          <form onSubmit={submit}>
            {!composerDraftReady && <p className="muted" role="status">正在恢复此会话草稿…</p>}
            {composerDraftError && <FloatingError className="error-inline" role="alert">草稿尚未可靠保存：{composerDraftError}。当前输入仍保留在页面中。</FloatingError>}
            <label className="sr-only" htmlFor="assistant-content">消息内容</label>
            <textarea ref={composerTextarea} id="assistant-content" value={visibleComposerContent} style={{ resize: 'none' }} onChange={(event) => {
              composerEditRevision.current += 1;
              if (!conversationID) ensureLocalConversationKey();
              const value = event.target.value;
              updateComposerDraftSnapshot({ content: value });
              setContent(value);
            }} onKeyDown={(event) => {
              if (event.key !== 'Enter' || event.shiftKey || event.nativeEvent.isComposing || event.nativeEvent.keyCode === 229) return;
              event.preventDefault();
              event.currentTarget.form?.requestSubmit();
            }} disabled={!composerDraftReady} placeholder="和 Morde 聊聊，或告诉我你想完成什么…" />
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
              </div>
              <div className="assistant-selectors">
                <label className="assistant-selector"><span className="sr-only">AI 模型</span><select aria-label="选择 AI 模型"
                  value={composerDraftReady ? effectiveModelID : ''} onChange={(event) => { composerEditRevision.current += 1; updateComposerDraftSnapshot({ modelID: event.target.value }); setModelID(event.target.value); }} disabled={!composerDraftReady || models.isPending}>
                  <option value="">自动选择模型</option>{effectiveModelID && !models.data?.items.some((item) => item.id === effectiveModelID) && <option value={effectiveModelID} disabled>已选择模型，状态待核对</option>}{readyModels.map((item) => <option key={item.id} value={item.id}>{item.name} · {item.model_name}</option>)}
                  {unavailableModels.map((item) => <option key={item.id} value={item.id} disabled>{item.name} · {item.checked_at ? '文字能力未验证或不可用' : '请先检查连接'}</option>)}
                </select></label>
                {unavailableModels.length > 0 && <Link to="/connections">查看模型连接</Link>}
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
                {mcpConnectors.isError && <FloatingError className="error-inline assistant-connector-error" role="alert">工具连接暂不可用<button type="button" className="button button-small button-secondary" onClick={() => void mcpConnectors.refetch()}>重试</button></FloatingError>}
              </div>
              <div className="assistant-task-actions" role="group" aria-label="发送操作">
                <AssistantVoiceInput compact onTranscript={(transcript) => {
                  composerEditRevision.current += 1;
                  if (!conversationID) ensureLocalConversationKey();
                  const snapshot = latestComposerDraft.current;
                  const current = snapshot?.identity === composerDraftIdentity ? snapshot.draft.content : content;
                  const value = current.trim() ? `${current.trimEnd()} ${transcript}` : transcript;
                  updateComposerDraftSnapshot({ content: value });
                  setContent(value);
                }} />
                {preparingAttachmentCount > 0 && <button type="button" className="button button-secondary" onClick={cancelComposerAttachmentPreparation}>取消读取</button>}
                <button className="button button-primary assistant-send" type="submit" disabled={!composerDraftReady || savingMessage || preparingAttachmentCount > 0 || composerAttachments.some((item) => item.needsPreparation) || (!content.trim() && !composerAttachments.length)}>
                  {preparingAttachmentCount > 0 || composerAttachments.some((item) => item.needsPreparation) ? '读取附件…' : savingMessage ? '保存中…' : '发送'}
                  <svg className="assistant-send-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 19V5m-5 5 5-5 5 5" /></svg>
                </button>
              </div>
            </div>
          </form>
          <p className="assistant-composer-hint">Enter 发送 · Shift + Enter 换行</p>
        </section>
      </div>
      {taskPanelOpen && <aside id="assistant-task-panel" className="assistant-task-panel" aria-label="用户任务与文件侧边栏">
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
            {taskPanelTab !== 'computer' && <button type="button" className="icon-button" aria-label="刷新任务与文件侧边栏" onClick={() => {
              void conversationTasks.refetch();
              void recentConversationTasks.refetch();
              void accountTasks.refetch();
              void waitingAccountTasks.refetch();
              void client.invalidateQueries({ queryKey: ['task'] });
              if (taskPanelTab === 'files' && conversationID === mainConversationID) {
                void client.invalidateQueries({ queryKey: ['my-agent-workspace'] });
                void client.invalidateQueries({ queryKey: ['agent-workspace-entries'] });
              }
            }}>↻</button>}
            <button type="button" className="icon-button" aria-label="关闭侧边栏" onClick={() => {
              postComputerVisibility(false);
              computerRequestGeneration.current += 1;
              setComputerConnecting(false);
              setComputerEmbed(null);
              setComputerControlExpanded(false);
              setTaskPanelOpen(false);
              setTaskDetailStepID(null);
              setTaskDetailTaskID(null);
              setTaskPanelTab('tasks');
              window.requestAnimationFrame(() => {
                if (outletContext?.focusPanelToggle) outletContext.focusPanelToggle();
                else taskPanelToggle.current?.focus();
              });
            }}>×</button>
          </div>
        </header>
        {conversationContentAccessRevoked && taskPanelTab === 'files' && <div className="loading-card" role="alert">
          当前会话内容暂不可访问。<button type="button" className="button button-small button-secondary" onClick={() => void messages.refetch()}>重试</button>
        </div>}
        {taskPanelTab === 'tasks' && taskDetailTaskID && <div className="task-panel-detail task-panel-list">
          <div className="task-panel-detail-nav"><button type="button" className="button button-small button-secondary"
            onClick={() => {
              if (taskDetailStepID) setTaskDetailStepID(null);
              else {
                setTaskDetailTaskID(null);
                window.requestAnimationFrame(() => taskCardButtons.current.get(taskDetailTaskID)?.focus());
              }
            }}>‹ {taskDetailStepID ? '返回任务详情' : '返回任务列表'}</button></div>
          {isResourceNoLongerAccessible(selectedTaskQuery?.error) ? <p className="muted" role="status">任务详情不可访问或已删除。</p>
            : selectedTaskQuery?.isError && !isTransientNetworkError(selectedTaskQuery.error) ? <FloatingError className="error-inline task-detail-load-error" role="alert">任务详情暂不可用，当前状态未确认。
              <button type="button" className="button button-small button-secondary" onClick={() => void selectedTaskQuery.refetch()}>重试</button></FloatingError>
              : selectedTaskSummary ? renderTaskDetail(selectedTaskSummary)
              : selectedTaskQuery?.isError ? <p className="muted" role="status">正在读取任务详情…</p>
                : <p className="muted" role="status">此任务已不在当前可见列表中，请刷新任务动态。</p>}
        </div>}
        {taskPanelTab === 'tasks' && <div className="task-panel-list" hidden={Boolean(taskDetailTaskID)}>
          {capabilities.isPending && <p className="muted" role="status">正在确认账号与服务身份…</p>}
          {accountTasks.isPending && !accountTasks.data && <p className="muted" role="status">正在读取用户任务…</p>}
          {accountTasks.isError && !isTransientNetworkError(accountTasks.error) && <FloatingError className="error-inline" role="alert">用户任务暂不可用：{errorDetails(accountTasks.error).message}
            <button type="button" className="button button-small button-secondary" onClick={() => void accountTasks.refetch()}>重试</button></FloatingError>}
          {taskProgressReadError && !taskProgressNetworkReadError && <FloatingError className="error-inline" role="alert">
            部分任务进展暂不可用，显示最近一次已确认状态。<button type="button" className="button button-small button-secondary"
              onClick={() => void client.invalidateQueries({ queryKey: ['task'] })}>重试</button></FloatingError>}
          {accountTasks.isSuccess && !visibleTaskPanelTasks.length && <p className="muted">当前账号还没有任务。</p>}
          {visibleTaskPanelTasks.length > 0 && <div className="task-panel-cards">{visibleTaskPanelTasks.map(renderTaskSummary)}</div>}
          {visibleTaskPanelTasks.length < taskPanelTasks.length && <button type="button" className="task-panel-load-more"
            onClick={() => setVisibleTaskPanelCount((count) => Math.min(taskPanelTasks.length, count + 5))}>
            更多任务 <span aria-hidden="true">↓</span>
          </button>}
        </div>}
        {!conversationContentAccessRevoked && taskPanelTab === 'files' && <div className="task-panel-list task-panel-files">
          <Link className="button button-secondary button-small" to="/hosts?sync=1">管理本机项目</Link>
          {conversationID === mainConversationID && <AgentWorkspaceBrowser active={taskPanelOpen && taskPanelTab === 'files'} />}
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
                  setTaskDetailStepID(null);
                  setTaskDetailTaskID(item.taskID);
                }}>查看任务成果</button>
              </article>} />}
        </div>}
        <section className="task-panel-list task-panel-computer" aria-label="当前会话云端电脑" hidden={taskPanelTab !== 'computer'}>
          <div ref={computerFrameBounds} className={`computer-panel-frame${computerViewportConnectionError ? ' is-connection-error' : ''}`}
            data-remote-desktop={computerSession?.remoteDesktop ?? 'connecting'}>
            {computerSession && !computerSession.native && computerSession.status !== 'error' &&
              <iframe key={computerSession.nonce} ref={computerFrame} name={`morde-cloud-${computerSession.nonce}`} title="当前会话云端电脑工作台"
                src="about:blank" allow="fullscreen" onError={() => setComputerEmbed((current) => current?.nonce === computerSession.nonce
                  ? { ...current, status: 'error', error: '工作台页面加载失败。' } : current)} />}
            {computerViewportMessage && <div className={`computer-panel-message${computerViewportConnectionError ? ' is-error' : ''}`}
              role={computerViewportConnectionError ? 'alert' : 'status'} aria-live="polite">
              {computerConnecting && <span className="computer-panel-spinner" aria-hidden="true" />}
              <span>{computerViewportMessage}</span>
            </div>}
          </div>
          <div className="computer-panel-dock">
            <div className="computer-panel-actions" role="group" aria-label="云端电脑功能">
              {computerQuickActions.map((action) => {
                const ticketTarget = action.target === 'files' && !selectedComputerTargets.includes('files') && selectedComputerTargets.includes('workspace')
                  ? 'workspace' : action.target;
                const allowed = selectedComputerTargets.includes(ticketTarget);
                const disabled = !conversationID || !selectedComputerHost || selectedComputerHost.state !== 'running' || !allowed ||
                  !computerControlHeldByMe || !computerWorkbenchReady || computerSession?.controlLocked !== false || computerConnecting;
                const reason = !conversationID ? '发送一条消息后连接当前会话'
                  : !selectedComputerHost ? '尚未配置默认云端电脑'
                    : selectedComputerHost.state !== 'running' ? '云端电脑当前不可用'
                      : !allowed ? '此功能未获授权'
                        : !computerControlHeldByMe ? '接管电脑后可使用'
                          : !computerWorkbenchReady ? '工作台尚未就绪'
                            : computerSession?.controlLocked !== false ? '工作台输入仍处于锁定状态' : action.label;
                const selected = (computerSession?.activeTarget ?? 'browser') === action.target || action.target === 'files' && computerSession?.activeTarget === 'files';
                return <button key={action.icon} type="button" className={`computer-panel-action computer-panel-action-${action.icon}${selected ? ' is-active' : ''}`}
                  aria-label={action.label} aria-pressed={selected} title={reason} disabled={disabled}
                  onClick={() => void selectComputerTarget(action.target)}>
                  <ComputerActionIcon icon={action.icon} /><span>{action.label}</span>
                </button>;
              })}
            </div>
            {selectedComputerHost ? <HostControlPanel host={selectedComputerHost}
              canControl={selectedComputerHost.owned || selectedComputerHost.permissions.includes('desktop:control')}
              canReleaseAnyLease={selectedComputerHost.owned} compact actionReady={computerWorkbenchReady}
              onBeforeAction={prepareComputerControlAction} /> : <section className="host-control host-control-compact computer-panel-control"
                aria-label="云端电脑控制"><button type="button" className="button button-primary" disabled
                  title={preferredHostID && hosts.isPending ? '正在确认云端电脑' : '尚未配置默认云端电脑'}>
                {preferredHostID && hosts.isPending ? '正在确认控制权…' : '接管控制权'}
              </button></section>}
          </div>
        </section>
      </aside>}
    </div>
    {enableExecutionConfirmationOpen && execution.data && user?.id === execution.data.owner_id && execution.data.state === 'disabled' && <ConfirmDialog
      title="启用云端任务"
      description="启用后，Morde 可在你已有授权范围内运行云端任务和工具；审批与授权规则保持不变。普通文字对话现在也可继续使用。"
      confirmLabel="确认启用" busy={enableExecution.isPending} onClose={() => setEnableExecutionConfirmationOpen(false)} onConfirm={confirmExecutionEnable}>
      {enableExecution.isError && <p role="alert">{errorDetails(enableExecution.error).message}。确认结果不明时会先核对服务端状态。</p>}
    </ConfirmDialog>}
    {unknownRetryConfirmation && conversationMessageItems.some((message) => message.id === unknownRetryConfirmation.messageID)
      && <ConfirmDialog title="上一次模型调用结果未知"
        description="上一次个人模型调用没有确定结果。继续会创建新的独立尝试，可能产生重复调用；服务端不会重复派发旧调用。请确认后继续。"
        confirmLabel="确认后重试" busy={retryConversationMessageProcessing.isPending}
        onClose={() => setUnknownRetryConfirmation(null)}
        onConfirm={() => {
          const confirmation = unknownRetryConfirmation;
          setUnknownRetryConfirmation(null);
          retryConversationMessageProcessing.mutate({ messageID: confirmation.messageID, confirmedWarning: confirmation.warning });
        }} />}
  </section>;
}
