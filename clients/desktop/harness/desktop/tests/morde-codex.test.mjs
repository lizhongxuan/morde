import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  codexSnapshotItemEvents,
  codexSnapshotItemFingerprint,
  codexApprovalPresentation,
  codexTurnDispatch,
  codexApprovalVersion,
  codexApprovalStatusEvent,
  codexUserInputQuestions,
  codexUserInputVersion,
  codexQuestionRequestMatches,
  codexConsumeQuestionRequest,
  codexQuestionAnswerResponse,
  codexQuestionEvent,
  codexQuestionStatusEvent,
  codexQuestionAnswerEvent,
  codexErrorNotificationEvent,
  codexUnsupportedNotificationEvent,
  codexCommandsForExitedProfile,
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
  assert.deepEqual(parseJsonRpcFrame('{"id":8,"error":{"code":-32000,"message":"active turn cannot be steered"}}'), {
    type: 'response', id: 8, error: { code: -32000, message: 'active turn cannot be steered' },
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

test('Codex follow-up dispatch uses turn/steer with the active turn precondition', () => {
  assert.deepEqual(codexTurnDispatch('thread-1', 'first', null, 'g0000001'), {
    method: 'turn/start',
    params: { threadId: 'thread-1', input: [{ type: 'text', text: 'first' }], clientUserMessageId: 'g0000001' },
  });
  assert.deepEqual(codexTurnDispatch('thread-1', 'follow-up', 'turn-9', 'g0000002'), {
    method: 'turn/steer',
    params: { threadId: 'thread-1', expectedTurnId: 'turn-9', input: [{ type: 'text', text: 'follow-up' }], clientUserMessageId: 'g0000002' },
  });
  assert.throws(() => codexTurnDispatch('thread-1', 'follow-up', '', 'g0000002'));
});

test('Codex error notifications distinguish terminal failure from retry and discard raw fields', () => {
  const failed = codexErrorNotificationEvent('thread-1', {
    turnId: 'turn-1', willRetry: false,
    error: { message: 'provider failed', additionalDetails: 'check credentials', codexErrorInfo: { private: 'raw detail' } },
  }, '2026-11-14T00:00:00.000Z');
  assert.equal(failed.willRetry, false);
  assert.equal(failed.event.kind, 'turn_failed');
  assert.deepEqual(failed.event.payload, { status: 'failed', error_code: 'codex_error', summary: 'check credentials' });
  assert.doesNotMatch(JSON.stringify(failed), /private|raw detail/);

  const retry = codexErrorNotificationEvent('thread-1', {
    turnId: 'turn-1', willRetry: true, error: { message: 'temporary disconnect' },
  }, '2026-11-14T00:00:00.000Z');
  assert.equal(retry.event.kind, 'unsupported');
  assert.equal(retry.event.payload.provider_type, 'app_server_error');
  assert.match(retry.event.payload.diagnostic, /temporary disconnect/);
});

test('Codex approvals expire across App Server epochs and unknown notifications contain no raw params', () => {
  const approval = { id: '12', method: 'item/commandExecution/requestApproval', params: { threadId: 'thread-1', turnId: 'turn-1' } };
  const firstVersion = codexApprovalVersion('process-a', approval.method, approval.params);
  const nextVersion = codexApprovalVersion('process-b', approval.method, approval.params);
  assert.notEqual(firstVersion, nextVersion);
  const stale = codexApprovalStatusEvent({ ...approval, version: firstVersion }, 'stale', 'App Server restarted');
  assert.equal(stale.kind, 'approval_status');
  assert.equal(stale.turn_id, 'turn-1');
  assert.deepEqual(stale.payload, { approval_id: '12', approval_version: firstVersion, status: 'stale', summary: 'App Server restarted' });

  const unsupported = codexUnsupportedNotificationEvent('future/notice', 'thread-1', {
    turnId: 'turn-1', itemId: 'item-1', private: { raw: 'must not escape' },
  }, '2026-11-14T00:00:00.000Z');
  assert.equal(unsupported.kind, 'unsupported');
  assert.equal(unsupported.payload.provider_type, 'app_server_notification');
  assert.equal(unsupported.payload.diagnostic, 'Morde 暂不支持 Codex App Server 通知：future/notice');
  assert.doesNotMatch(JSON.stringify(unsupported), /private|must not escape/);
});

test('Codex user-input answers preserve opaque IDs and return the exact App Server response shape', () => {
  const questions = codexUserInputQuestions([
    { id: ' choice ', header: 'Pick', question: 'Which?', options: [{ label: 'A', description: 'Option A' }] },
    { id: '__proto__', header: 'Token', question: 'Enter token', isSecret: true, options: [] },
  ]);
  assert.ok(questions);
  assert.equal(questions[0].id, ' choice ');
  assert.equal(questions[1].is_secret, true);
  const answers = Object.fromEntries([
    [' choice ', [' A\nB ']],
    ['__proto__', ['hidden-value']],
  ]);
  const response = codexQuestionAnswerResponse(questions, answers);
  assert.deepEqual(response, {
    answers: Object.fromEntries([
      [' choice ', { answers: [' A\nB '] }],
      ['__proto__', { answers: ['hidden-value'] }],
    ]),
  });
  assert.equal(Object.hasOwn(response.answers, '__proto__'), true);
  assert.equal(response.answers[' choice '].answers[0], ' A\nB ');
  for (const invalid of [
    { 'choice': ['A'], '__proto__': ['hidden-value'] },
    { ' choice ': ['A'] },
    { ' choice ': ['A'], '__proto__': ['hidden-value'], extra: ['x'] },
    { ' choice ': ['   '], '__proto__': ['hidden-value'] },
    { ' choice ': ['A\u0001'], '__proto__': ['hidden-value'] },
  ]) assert.equal(codexQuestionAnswerResponse(questions, invalid), null);
  assert.equal(codexUserInputQuestions([
    { id: 'duplicate', header: 'A', question: 'First' },
    { id: 'duplicate', header: 'B', question: 'Second' },
  ]), null);
});

test('Codex question requests bind IDs, profile, thread, grant version, and expire once', () => {
  const questions = codexUserInputQuestions([
    { id: ' id with spaces ', header: 'Pick', question: 'Choose one', options: [], isOther: true },
  ]);
  assert.ok(questions);
  const context = {
    profileKey: 'profile-a', threadId: 'thread-a', grantVersion: 7, rpcID: 12,
    turnId: 'turn-a', itemId: 'item-a', questions,
  };
  const request = { ...context, id: '12', version: codexUserInputVersion('process-a', 'request-a', context), createdAt: '2026-11-14T00:00:00.000Z' };
  assert.equal(codexQuestionRequestMatches(request, 'profile-a', 'thread-a', 7, '12', request.version), true);
  assert.equal(codexQuestionRequestMatches(request, 'profile-b', 'thread-a', 7, '12', request.version), false);
  assert.equal(codexQuestionRequestMatches(request, 'profile-a', 'thread-b', 7, '12', request.version), false);
  assert.equal(codexQuestionRequestMatches(request, 'profile-a', 'thread-a', 8, '12', request.version), false);
  assert.equal(codexQuestionRequestMatches(request, 'profile-a', 'thread-a', 7, ' 12 ', request.version), false);
  assert.equal(codexQuestionRequestMatches(request, 'profile-a', 'thread-a', 7, '12', 'v1'), false);
  assert.notEqual(codexUserInputVersion('process-b', 'request-a', context), request.version);
  assert.notEqual(codexUserInputVersion('process-a', 'request-b', context), request.version);
  assert.notEqual(codexUserInputVersion('process-a', 'request-a', { ...context, grantVersion: 8 }), request.version);

  const pending = Object.assign(Object.create(null), { '12': request });
  assert.equal(codexConsumeQuestionRequest(pending, 'profile-a', 'thread-a', 7, '12', 'b'.repeat(64)), null);
  assert.equal(Object.hasOwn(pending, '12'), true);
  assert.equal(codexConsumeQuestionRequest(pending, 'profile-a', 'thread-a', 7, '12', request.version), request);
  assert.equal(codexConsumeQuestionRequest(pending, 'profile-a', 'thread-a', 7, '12', request.version), null);
});

test('Codex question lifecycle events keep pending details, use terminal status, and redact secret answers', () => {
  const questions = codexUserInputQuestions([
    { id: 'public-id', header: 'Choice', question: 'Select', options: [], isSecret: false },
    { id: 'secret-id', header: 'Credential', question: 'Enter secret', options: [], isSecret: true },
  ]);
  assert.ok(questions);
  const context = {
    profileKey: 'profile-a', threadId: 'thread-a', grantVersion: 2, rpcID: 21,
    turnId: 'turn-a', itemId: 'item-a', questions,
  };
  const request = { ...context, id: '21', version: codexUserInputVersion('process-a', 'request-a', context), createdAt: '2026-11-14T00:00:00.000Z' };
  const pending = codexQuestionEvent(request, 'pending', '2026-11-14T00:01:00.000Z');
  assert.equal(pending.kind, 'question');
  assert.equal(pending.source, 'live');
  assert.deepEqual(pending.payload, {
    question_request_id: '21', question_version: request.version, status: 'pending', questions,
  });
  const stale = codexQuestionStatusEvent('21', request.version, 'stale', '2026-11-14T00:02:00.000Z');
  assert.deepEqual(stale.payload, { question_request_id: '21', question_version: request.version, status: 'stale' });
  assert.equal(Object.hasOwn(stale.payload, 'questions'), false);

  const answers = { 'public-id': ['Visible answer'], 'secret-id': ['do-not-show-this'] };
  const answerEvent = codexQuestionAnswerEvent(request, answers, '2026-11-14T00:03:00.000Z');
  assert.equal(answerEvent.kind, 'user_message');
  assert.match(answerEvent.payload.text, /Visible answer/);
  assert.doesNotMatch(JSON.stringify(answerEvent), /do-not-show-this/);
});

test('unexpected App Server exit selects only active commands associated with its profile', () => {
  const commandProfiles = new Map([
    ['g0000001', 'profile-a'],
    ['g0000002', 'profile-b'],
    ['g0000003', 'profile-a'],
    ['g0000004', 'profile-a'],
    ['g0000005', 'profile-a'],
    ['g0000006', 'profile-a'],
    ['g0000007', 'profile-a'],
  ]);
  const commands = {
    g0000001: { status: 'running' },
    g0000002: { status: 'waiting_approval' },
    g0000003: { status: 'completed' },
    g0000004: { status: 'dispatching' },
    g0000005: { status: 'waiting_approval' },
    g0000006: { status: 'forwarded' },
    g0000007: { status: 'failed' },
  };
  assert.deepEqual(codexCommandsForExitedProfile(commandProfiles, commands, 'profile-a'), ['g0000001', 'g0000004', 'g0000005', 'g0000006']);
});

test('Codex snapshots produce stable bounded visible items with content-versioned IDs', () => {
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
  assert.notDeepEqual(repeated.map((event) => event.provider_event_id), codexSnapshotItemEvents(turn, changed).map((event) => event.provider_event_id));
  assert.notDeepEqual(repeated.map((event) => event.payload), codexSnapshotItemEvents(turn, changed).map((event) => event.payload));
  const externalQuestion = codexSnapshotItemEvents(turn, question).find((event) => event.kind === 'unsupported');
  assert.ok(externalQuestion);
  assert.match(externalQuestion.payload.diagnostic, /不包含对应的原始 RPC handle/);
  assert.doesNotMatch(JSON.stringify(externalQuestion.payload), /question_request_id|question_version|approval_id|approval_version/);
});

test('active Codex snapshots project visible user, assistant, tool, and plan items', () => {
  const activeTurn = { id: 'active-turn', status: 'inProgress', startedAt: 100, completedAt: null, itemsView: 'full', items: [] };
  const user = { id: 'active-user', type: 'userMessage', content: [{ type: 'text', text: 'Inspect the current change.' }] };
  const assistant = { id: 'active-assistant', type: 'agentMessage', text: 'I am checking the files.' };
  const tool = { id: 'active-command', type: 'commandExecution', command: 'git status --short', aggregatedOutput: ' M src/example.ts', exitCode: null, status: 'inProgress' };
  const plan = { id: 'active-plan', type: 'plan', text: 'Inspect files\nSummarize findings' };
  const projected = [user, assistant, tool, plan].flatMap((item) => codexSnapshotItemEvents(activeTurn, item));

  assert.equal(projected.find((event) => event.item_id === user.id)?.kind, 'user_message');
  assert.equal(projected.find((event) => event.item_id === assistant.id)?.kind, 'assistant_message');
  assert.ok(projected.some((event) => event.item_id === tool.id && event.kind === 'tool_call'));
  assert.ok(projected.some((event) => event.item_id === tool.id && event.kind === 'tool_result' && event.payload.output === tool.aggregatedOutput));
  assert.equal(projected.find((event) => event.item_id === plan.id)?.kind, 'plan');
});

test('changed active assistant snapshots replace and append without losing chunks or duplicating the final snapshot', () => {
  const activeTurn = { id: 'active-turn', status: 'inProgress', startedAt: 100, completedAt: null, itemsView: 'full', items: [] };
  const firstItem = { id: 'long-assistant', type: 'agentMessage', text: 'A'.repeat(24 * 1024 + 10) };
  const changedItem = { ...firstItem, text: `${firstItem.text}B${'C'.repeat(24 * 1024 + 20)}` };
  const firstVersion = codexSnapshotItemEvents(activeTurn, firstItem);
  const changedVersion = codexSnapshotItemEvents(activeTurn, changedItem);
  const finalSnapshot = codexSnapshotItemEvents({ ...activeTurn, status: 'completed', completedAt: 101 }, changedItem);

  assert.equal(firstVersion[0].kind, 'assistant_message');
  assert.ok(firstVersion.every((event) => event.kind === 'assistant_message'));
  assert.deepEqual(firstVersion.map((event) => event.payload.segment_id), firstVersion.map((_, index) => `${firstItem.id}:${index}`));
  assert.equal(firstVersion.map((event) => event.payload.text).join(''), firstItem.text);
  assert.equal(changedVersion[0].kind, 'assistant_message');
  assert.ok(changedVersion.every((event) => event.kind === 'assistant_message'));
  assert.equal(changedVersion.map((event) => event.payload.text).join(''), changedItem.text);
  assert.notDeepEqual(firstVersion.map((event) => event.provider_event_id), changedVersion.map((event) => event.provider_event_id));
  assert.deepEqual(changedVersion.map((event) => event.provider_event_id), finalSnapshot.map((event) => event.provider_event_id));

  // Mirrors iOS snapshot grouping: retain the latest payload for each segment_id,
  // then join the numbered chunks in order.
  const latestSegments = new Map();
  for (const event of [...firstVersion, ...changedVersion]) latestSegments.set(event.payload.segment_id, event.payload.text);
  const mergedText = [...latestSegments.entries()]
    .sort(([left], [right]) => Number(left.slice(left.lastIndexOf(':') + 1)) - Number(right.slice(right.lastIndexOf(':') + 1)))
    .map(([, text]) => text).join('');
  assert.equal(mergedText, changedItem.text);
});

test('Codex approval projection preserves the action and reduces local path fields to display labels', () => {
  const command = codexApprovalPresentation('command', {
    command: 'npm run check -- --workspace packages/server',
    cwd: '/Users/alice/work/morde/server',
    reason: 'Run the server checks',
  });
  assert.equal(command.object, 'npm run check -- --workspace packages/server');
  assert.equal(command.detail, 'morde/server');
  assert.equal(command.impact, 'Run the server checks');
  assert.doesNotMatch(JSON.stringify(command), /\/Users\/alice/);

  const permissions = codexApprovalPresentation('permissions', {
    cwd: '/Users/alice/work/morde',
    reason: 'Need access to update generated files',
    permissions: {
      fileSystem: { entries: [
        { access: 'read', path: { type: 'path', path: '/Users/alice/work/morde/src' } },
        { access: 'write', path: { type: 'glob_pattern', pattern: '/Users/alice/work/morde/generated/**' } },
      ] },
      network: { enabled: true },
    },
  });
  assert.equal(permissions.object, 'work/morde');
  assert.equal(permissions.detail, 'read: morde/src; write: generated/**; network access');
  assert.doesNotMatch(JSON.stringify(permissions), /\/Users\/alice/);
});
