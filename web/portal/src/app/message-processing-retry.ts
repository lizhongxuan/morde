import { newIdempotencyKey } from './api';

function storageKey(instanceID: string, actorID: string, projectID: string, conversationID: string, messageID: string) {
  return `morde:message-processing-retry:${instanceID}:${actorID}:${projectID}:${conversationID}:${messageID}`;
}

// Persist before dispatch so a lost response can be reconciled with the same key.
export function processingRetryKey(instanceID: string, actorID: string, projectID: string, conversationID: string, messageID: string): string {
  const name = storageKey(instanceID, actorID, projectID, conversationID, messageID);
  const existing = localStorage.getItem(name);
  if (existing) return existing;
  const key = newIdempotencyKey();
  localStorage.setItem(name, key);
  return key;
}

export function clearProcessingRetryKey(instanceID: string, actorID: string, projectID: string, conversationID: string, messageID: string) {
  localStorage.removeItem(storageKey(instanceID, actorID, projectID, conversationID, messageID));
}
