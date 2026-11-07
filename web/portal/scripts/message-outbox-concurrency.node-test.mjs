import assert from 'node:assert/strict';
import { existsSync, readFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { chromium } from '@playwright/test';
import ts from 'typescript';

test('two browser tabs serialize the same conversation outbox in IndexedDB', async () => {
  const source = readFileSync(fileURLToPath(new URL('../src/app/message-outbox.ts', import.meta.url)), 'utf8');
  const moduleSource = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText;
  const server = createServer((request, response) => {
    if (request.url === '/outbox.js') {
      response.writeHead(200, { 'Content-Type': 'text/javascript' }).end(moduleSource);
    } else {
      response.writeHead(200, { 'Content-Type': 'text/html' }).end('<!doctype html><title>outbox test</title>');
    }
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address();
  assert.ok(address && typeof address === 'object');
  const origin = `http://127.0.0.1:${address.port}`;
  const installedChrome = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
  const executablePath = process.env.MORDE_TEST_CHROME ?? (existsSync(installedChrome) ? installedChrome : undefined);
  let browser;
  try {
    browser = await chromium.launch({ ...(executablePath ? { executablePath } : {}), headless: true,
      args: ['--disable-background-networking', '--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1'] });
    const context = await browser.newContext();
    await context.route('**/*', (route) => new URL(route.request().url()).origin === origin ? route.continue() : route.abort());
    const tabs = await Promise.all([context.newPage(), context.newPage()]);
    await Promise.all(tabs.map((tab) => tab.goto(origin)));
    const message = (id, overrides = {}) => ({ request: { client_message_id: id, content: `synthetic ${id}` },
      actorID: 'u1234567', serviceInstanceID: 'instance-001', conversationID: 'c1234567', attachments: [],
      status: 'pending', createdAt: 1, ...overrides });
    const enqueue = (tab, scope, value, predecessor) => tab.evaluate(async ({ scope, value, predecessor }) => {
      const { enqueueOutboxMessage, messageOutbox } = await import('/outbox.js');
      return enqueueOutboxMessage(messageOutbox, scope, value, predecessor);
    }, { scope, value, predecessor });

    const scope = 'browser-account-service';
    const messages = await Promise.all(Array.from({ length: 20 }, (_, index) =>
      enqueue(tabs[index % tabs.length], scope, message(`message-${index}`), 'server-previous')));
    const following = new Map(messages.map((item) => [item.request.previous_client_message_id, item.request.client_message_id]));
    assert.equal(following.size, 20);
    let current = 'server-previous';
    const visited = new Set();
    while (following.has(current)) { current = following.get(current); visited.add(current); }
    assert.equal(visited.size, 20);
    await tabs[0].evaluate(async (scope) => {
      const { messageOutbox } = await import('/outbox.js');
      await messageOutbox.update(scope, () => []);
    }, scope);
    const afterRemoval = await enqueue(tabs[1], scope, message('message-after-removal'), 'stale-server');
    assert.equal(afterRemoval.request.previous_client_message_id, current);
    const projectMessages = await Promise.all([
      enqueue(tabs[0], scope, message('project-1', { projectID: 'j1234567' })),
      enqueue(tabs[1], scope, message('project-2', { projectID: 'j1234567' })),
    ]);
    const projectFirst = projectMessages.find((item) => !item.request.previous_client_message_id);
    const projectSecond = projectMessages.find((item) => item.request.previous_client_message_id);
    assert.ok(projectFirst && projectSecond);
    assert.equal(projectSecond.request.previous_client_message_id, projectFirst.request.client_message_id);
    const otherService = await enqueue(tabs[1], 'other-service', message('other-service-message'));
    assert.equal(otherService.request.previous_client_message_id, undefined);

    const draftScope = 'draft-account-service';
    const draft = message('draft-1', { conversationID: null, conversationCreateKey: 'create-1' });
    await enqueue(tabs[0], draftScope, draft);
    await tabs[1].evaluate(async ({ scope }) => {
      const { bindOutboxConversation, messageOutbox } = await import('/outbox.js');
      await bindOutboxConversation(messageOutbox, scope, 'u1234567', 'create-1', 'c1234567');
    }, { scope: draftScope });
    const [linked, sameDraft] = await Promise.all([
      enqueue(tabs[0], draftScope, message('linked-2')),
      enqueue(tabs[1], draftScope, message('draft-3', { conversationID: null, conversationCreateKey: 'create-1' })),
    ]);
    const chain = new Map([linked, sameDraft].map((item) => [item.request.previous_client_message_id, item.request.client_message_id]));
    assert.ok(chain.has('draft-1'));
    assert.ok(chain.has(chain.get('draft-1')));

    const conflictScope = 'conflict-account-service';
    await enqueue(tabs[0], conflictScope, message('draft-conflict', { conversationID: null, conversationCreateKey: 'create-conflict' }));
    await enqueue(tabs[1], conflictScope, message('conversation-conflict'));
    await assert.rejects(tabs[0].evaluate(async ({ scope }) => {
      const { bindOutboxConversation, messageOutbox } = await import('/outbox.js');
      await bindOutboxConversation(messageOutbox, scope, 'u1234567', 'create-conflict', 'c1234567');
    }, { scope: conflictScope }), /本地消息顺序待核对/);
    const retained = await tabs[1].evaluate(async (scope) => {
      const { messageOutbox } = await import('/outbox.js');
      return (await messageOutbox.read(scope)).map((item) => item.request.client_message_id);
    }, conflictScope);
    assert.deepEqual(retained, ['draft-conflict', 'conversation-conflict']);
    await context.close();
  } finally {
    await browser?.close();
    await new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
  }
});
