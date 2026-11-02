export interface LegacyHistoryMessage {
  source_message_id: string;
  role: 'user' | 'assistant';
  content: string;
}

export interface LegacyHistoryConversation {
  source_conversation_id: string;
  title: string;
  messages: LegacyHistoryMessage[];
  ignored_events: number;
  skipped_attachments: number;
}

export interface LegacyHistorySelection {
  conversations: LegacyHistoryConversation[];
  skipped_files: number;
  ignored_events: number;
  skipped_attachments: number;
}

const MAX_FILE_BYTES = 64 * 1024 * 1024;
const MAX_SELECTION_BYTES = 256 * 1024 * 1024;
const MAX_EVENTS_PER_THREAD = 2000;
const MAX_MESSAGE_BYTES = 32 * 1024;
const MAX_BATCH_BYTES = 1 << 20;
const MAX_BATCH_MESSAGES = 200;
const SOURCE_ID = 'nanomuse-export-v1';
const DEVICE_ACCOUNT_ID = 'nanomuse-local-v1';

function object(value: unknown): Record<string, unknown> | null {
  return typeof value === 'object' && value !== null && !Array.isArray(value) ? value as Record<string, unknown> : null;
}

function validID(value: unknown): value is string {
  if (typeof value !== 'string' || !value.trim() || new TextEncoder().encode(value).length > 128) return false;
  return ![...value].some((character) => {
    const codePoint = character.codePointAt(0) ?? 0;
    return codePoint < 0x20 || codePoint === 0x7f;
  });
}

function basename(file: File): string {
  return file.name.split(/[\\/]/u).at(-1) ?? file.name;
}

function titleFor(value: unknown, fallback: string): string {
  if (typeof value !== 'string') return fallback;
  return Array.from(value).slice(0, 120).join('') || fallback;
}

export async function readLegacyHistorySelection(files: readonly File[]): Promise<LegacyHistorySelection> {
  const selectedBytes = files.reduce((total, file) => total + file.size, 0);
  if (selectedBytes > MAX_SELECTION_BYTES) throw new Error('单次最多选择 256 MiB 的 nanoMuse 会话文件。');

  let skippedFiles = 0;
  let ignoredEvents = 0;
  let skippedAttachments = 0;
  const titles = new Map<string, string>();
  const threads: Array<{ file: File; thread: unknown; events: unknown[] }> = [];

  for (const file of files) {
    if (file.size > MAX_FILE_BYTES) {
      skippedFiles++;
      continue;
    }
    let data: unknown;
    try {
      data = JSON.parse(await file.text()) as unknown;
    } catch {
      skippedFiles++;
      continue;
    }
    if (basename(file) === 'index.json') {
      if (!Array.isArray(data)) {
        skippedFiles++;
        continue;
      }
      for (const rawEntry of data) {
        const entry = object(rawEntry);
        if (entry && validID(entry.id) && typeof entry.title === 'string') titles.set(entry.id, titleFor(entry.title, entry.id));
      }
      continue;
    }
    const entry = object(data);
    if (!entry || !Array.isArray(entry.events)) {
      skippedFiles++;
      continue;
    }
    threads.push({ file, thread: entry.thread, events: entry.events });
  }

  const conversations: LegacyHistoryConversation[] = [];
  for (const { file, thread, events } of threads) {
    const conversationID = validID(thread) ? thread : basename(file).replace(/\.json$/iu, '');
    if (!validID(conversationID) || events.length > MAX_EVENTS_PER_THREAD) {
      skippedFiles++;
      continue;
    }
    const messages: LegacyHistoryMessage[] = [];
    const sourceIDs = new Set<string>();
    let conversationIgnoredEvents = 0;
    let conversationSkippedAttachments = 0;
    for (const rawEvent of events) {
      const event = object(rawEvent);
      if (Array.isArray(event?.files)) conversationSkippedAttachments += event.files.length;
      if (!event || (event.type !== 'user' && event.type !== 'assistant') || event.quiet === true ||
        (event.source !== undefined && event.source !== 'user') || !validID(event.id) || typeof event.text !== 'string' ||
        event.text.length === 0 || new TextEncoder().encode(event.text).length > MAX_MESSAGE_BYTES || event.text.includes('\0') || sourceIDs.has(event.id)) {
        conversationIgnoredEvents++;
        continue;
      }
      sourceIDs.add(event.id);
      messages.push({ source_message_id: event.id, role: event.type, content: event.text });
    }
    if (messages.length) {
      conversations.push({
        source_conversation_id: conversationID,
        title: titles.get(conversationID) ?? conversationID,
        messages,
        ignored_events: conversationIgnoredEvents,
        skipped_attachments: conversationSkippedAttachments,
      });
      ignoredEvents += conversationIgnoredEvents;
      skippedAttachments += conversationSkippedAttachments;
    } else {
      skippedFiles++;
      ignoredEvents += conversationIgnoredEvents;
      skippedAttachments += conversationSkippedAttachments;
    }
  }

  return { conversations, skipped_files: skippedFiles, ignored_events: ignoredEvents, skipped_attachments: skippedAttachments };
}

export function batchLegacyHistory(messages: LegacyHistoryMessage[]): LegacyHistoryMessage[][] {
  const batches: LegacyHistoryMessage[][] = [];
  let batch: LegacyHistoryMessage[] = [];
  let bytes = 0;
  for (const message of messages) {
    const messageBytes = new TextEncoder().encode(message.content).length;
    if (batch.length && (batch.length >= MAX_BATCH_MESSAGES || bytes + messageBytes > MAX_BATCH_BYTES)) {
      batches.push(batch);
      batch = [];
      bytes = 0;
    }
    batch.push(message);
    bytes += messageBytes;
  }
  if (batch.length) batches.push(batch);
  return batches;
}

export const legacyHistorySource = { source_id: SOURCE_ID, device_account_id: DEVICE_ACCOUNT_ID } as const;
