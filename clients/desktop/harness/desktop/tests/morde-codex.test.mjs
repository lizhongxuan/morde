import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  codexSnapshotItemEvents,
  codexSnapshotItemFingerprint,
  encodeJsonRpcFrame,
  parseJsonRpcFrame,
} from '../out/morde-codex.js';

test('Codex App Server JSONL frames route versionless requests, notifications, and responses safely', () => {
  assert.deepEqual(parseJsonRpcFrame('{"id":7,"method":"thread/read","params":{"threadId":"thread-1"}}'), {
    type: 'request', id: 7, method: 'thread/read', params: { threadId: 'thread-1' },
  });
  assert.deepEqual(parseJsonRpcFrame('{"method":"item/agentMessage/delta","params":{"delta":"hello"}}'), {
    type: 'notification', method: 'item/agentMessage/delta', params: { delta: 'hello' },
  });
  assert.deepEqual(parseJsonRpcFrame('{"id":1,"result":{"threadId":"thread-1"}}'), {
    type: 'response', id: 1, result: { threadId: 'thread-1' },
  });
  assert.equal(encodeJsonRpcFrame({ id: 1, method: 'initialize', params: {} }), '{"id":1,"method":"initialize","params":{}}\n');
  for (const frame of [
    '{"jsonrpc":"1.0","id":1,"result":{}}',
    '{"id":true,"method":"thread/read"}',
    '{"id":1,"method":"thread/read","result":{}}',
    '{"id":1,"result":{},"error":{"code":-1,"message":"bad"}}',
    '{"id":1,"error":{"code":"bad","message":"bad"}}',
    '{broken json',
  ]) assert.throws(() => parseJsonRpcFrame(frame));
});

test('Codex snapshots produce stable bounded visible items and distinguish an unstable edit', () => {
  const turn = {
    id: 'turn-1', status: 'completed', startedAt: 100, completedAt: 101, itemsView: 'full', items: [],
  };
  const reasoning = { id: 'reason-1', type: 'reasoning', summary: ['Short public summary'], content: ['private reasoning content'] };
  const plan = { id: 'plan-1', type: 'plan', text: 'Inspect files\nUpdate adapter' };
  const command = { id: 'command-1', type: 'commandExecution', command: 'npm run check', aggregatedOutput: 'done', exitCode: 0, status: 'completed' };
  const file = { id: 'file-1', type: 'fileChange', status: 'completed', changes: [{ path: '/Users/example/work/src/a.ts', kind: { type: 'update' }, diff: 'private patch' }] };
  const child = { id: 'child-1', type: 'subAgentActivity', agentThreadId: 'sub-thread-1', agentPath: 'child', kind: 'completed' };
  const question = { id: 'question-1', type: 'agentMessage', text: 'Need a choice', questions: [{ id: 'q-1', header: 'Pick', question: 'Which?', options: [{ label: 'A', description: 'Option A' }] }] };
  const projected = [reasoning, plan, command, file, child, question].flatMap((item) => codexSnapshotItemEvents(turn, item));
  const kinds = new Set(projected.map((event) => event.kind));
  for (const kind of ['reasoning_summary', 'plan', 'tool_call', 'tool_result', 'file_change', 'child_agent', 'assistant_message', 'unsupported']) assert.ok(kinds.has(kind), `missing ${kind}`);
  assert.doesNotMatch(JSON.stringify(projected), /private reasoning content|private patch|\/Users\/example/);

  const repeated = codexSnapshotItemEvents(turn, reasoning);
  assert.deepEqual(repeated.map((event) => event.provider_event_id), codexSnapshotItemEvents(turn, reasoning).map((event) => event.provider_event_id));
  assert.equal(codexSnapshotItemFingerprint(reasoning), codexSnapshotItemFingerprint({ ...reasoning }));
  const changed = { ...reasoning, summary: ['Changed public summary'] };
  assert.notEqual(codexSnapshotItemFingerprint(reasoning), codexSnapshotItemFingerprint(changed));
  assert.deepEqual(repeated.map((event) => event.provider_event_id), codexSnapshotItemEvents(turn, changed).map((event) => event.provider_event_id));
  assert.notDeepEqual(repeated.map((event) => event.payload), codexSnapshotItemEvents(turn, changed).map((event) => event.payload));
});
