import { api, ApiError, unwrapApiResponse } from '../api/client';
import type { components } from '../api/schema';

const directMessageBytes = 32 * 1024;
const chunkBytes = 16 * 1024;
export const maxMessageBytes = 8 * 1024 * 1024;

type MessageRequest = components['schemas']['AcceptConversationMessageRequest'];
type Message = components['schemas']['ConversationMessage'];
type UploadInit = components['schemas']['ConversationMessageUploadInit'];
type UploadChunk = components['schemas']['ConversationMessageChunk'];

export function needsMessageUpload(content: string): boolean {
  return new TextEncoder().encode(content).length > directMessageBytes;
}

async function digest(bytes: Uint8Array): Promise<string> {
  const copy = new Uint8Array(bytes);
  const hash = await crypto.subtle.digest('SHA-256', copy.buffer);
  return Array.from(new Uint8Array(hash), (value) => value.toString(16).padStart(2, '0')).join('');
}

function unwrapUpload<T>(result: { data?: T; error?: unknown; response: Response }): T {
  if (result.response.status === 404 || result.response.status === 501) {
    throw new ApiError({ message: `长消息上传未完成（服务返回 ${result.response.status}），本机保留完整内容。`,
      code: 'message_upload_unavailable', status: result.response.status });
  }
  return unwrapApiResponse(result);
}

function unwrapCommit<T>(result: { data?: T; error?: unknown; response: Response }): T {
  const data = unwrapUpload(result);
  if (result.response.status !== 202) throw new Error('长消息提交结果待确认。');
  return data;
}

/** Uploads one message as UTF-8 bytes; the caller validates the returned message and preserves the outbox on failure. */
export async function uploadMessage<T extends Message>(
  target: { conversationID: string; projectID?: string }, request: MessageRequest, serviceInstanceID: string, actorID: string, checkScope: () => void,
): Promise<T> {
  const { client_message_id: messageID, content, ...metadata } = request;
  if (!serviceInstanceID || !actorID || !messageID) throw new Error('消息、账号或服务身份尚未确认。');
  const bytes = new TextEncoder().encode(content);
  if (bytes.length > maxMessageBytes) {
    throw new ApiError({ message: '消息超过 8 MiB，本机已保留完整内容。', code: 'message_too_large', status: 413 });
  }
  const count = Math.ceil(bytes.length / chunkBytes);
  const headers = { 'X-Morde-Service-Instance-ID': serviceInstanceID, 'X-Morde-Actor-ID': actorID };
  const requestHeaders = { ...headers, 'Idempotency-Key': messageID };
  const conversationPath = { id: target.conversationID, client_message_id: messageID };
  const projectPath = { id: target.projectID ?? '', conversation_id: target.conversationID, client_message_id: messageID };
  const init = async (body: UploadInit) => target.projectID
    ? unwrapUpload(await api.PUT('/api/projects/{id}/conversations/{conversation_id}/message-uploads/{client_message_id}', {
      params: { path: projectPath, header: requestHeaders }, body,
    }))
    : unwrapUpload(await api.PUT('/api/conversations/{id}/message-uploads/{client_message_id}', {
      params: { path: conversationPath, header: requestHeaders }, body,
    }));
  const progress = async () => target.projectID
    ? unwrapUpload(await api.GET('/api/projects/{id}/conversations/{conversation_id}/message-uploads/{client_message_id}', {
      params: { path: projectPath, header: headers },
    }))
    : unwrapUpload(await api.GET('/api/conversations/{id}/message-uploads/{client_message_id}', {
      params: { path: conversationPath, header: headers },
    }));
  const putChunk = async (sequence: number, body: UploadChunk) => target.projectID
    ? unwrapUpload(await api.PUT('/api/projects/{id}/conversations/{conversation_id}/message-uploads/{client_message_id}/chunks/{sequence}', {
      params: { path: { ...projectPath, sequence }, header: headers }, body,
    }))
    : unwrapUpload(await api.PUT('/api/conversations/{id}/message-uploads/{client_message_id}/chunks/{sequence}', {
      params: { path: { ...conversationPath, sequence }, header: headers }, body,
    }));
  const commit = async () => target.projectID
    ? unwrapCommit(await api.POST('/api/projects/{id}/conversations/{conversation_id}/message-uploads/{client_message_id}/commit', {
      params: { path: projectPath, header: requestHeaders },
    }))
    : unwrapCommit(await api.POST('/api/conversations/{id}/message-uploads/{client_message_id}/commit', {
      params: { path: conversationPath, header: requestHeaders },
    }));

  const fullHash = await digest(bytes);
  checkScope();
  await init({ ...metadata, total_bytes: bytes.length, total_chunks: count, sha256: fullHash });
  checkScope();
  const state = await progress();
  if (!state || typeof state !== 'object' || state.client_message_id !== messageID || state.total_bytes !== bytes.length || state.total_chunks !== count
    || (state.state !== 'uploading' && state.state !== 'committed') || !Array.isArray(state.received_sequences)
    || state.received_sequences.some((sequence) => !Number.isInteger(sequence) || sequence < 0 || sequence >= count)
    || new Set(state.received_sequences).size !== state.received_sequences.length) {
    throw new Error('长消息上传进度无效，本机已保留原文。');
  }
  if (state.state === 'committed') {
    if (!state.message) throw new Error('已提交的长消息缺少回执，请核对后重试。');
    return state.message as T;
  }
  const received = new Set(state.received_sequences);
  for (let sequence = 0; sequence < count; sequence += 1) {
    if (received.has(sequence)) continue;
    const chunk = bytes.subarray(sequence * chunkBytes, Math.min((sequence + 1) * chunkBytes, bytes.length));
    const hash = await digest(chunk);
    checkScope();
    await putChunk(sequence, { content_base64: btoa(String.fromCharCode(...chunk)), sha256: hash });
  }
  checkScope();
  const receipt = await commit();
  if (!receipt?.message) throw new Error('长消息回执无效，请核对后重试。');
  return receipt.message as T;
}
