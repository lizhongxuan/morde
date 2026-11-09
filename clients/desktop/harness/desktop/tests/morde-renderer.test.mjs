import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';
import { createRequire } from 'node:module';

class Element {
  constructor(tagName = 'div') { this.tagName = tagName; }
  childNodes = [];
  hidden = false;
  value = '';
  textContent = '';
  className = '';
  disabled = false;
  checked = false;
  type = '';
  rows = 0;
  placeholder = '';
  onclick;
  onchange;
  oninput;
  onsubmit;
  append(...children) { this.childNodes.push(...children); }
  replaceChildren(...children) { this.childNodes = [...children]; }
}

async function mount(api, confirm = () => false) {
  const elements = new Map();
  const document = {
    getElementById(id) { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); },
    createElement(tagName) { return new Element(tagName); },
  };
  for (const id of ['origin', 'name', 'password', 'submit', 'web', 'account-settings']) document.getElementById(id);
  let uuid = 0;
  const source = await readFile(new URL('../resources/morde.js', import.meta.url), 'utf8');
  vm.runInNewContext(source, { window: { morde: api, confirm }, document, crypto: { randomUUID: () => `retry-${++uuid}` } }, { filename: 'morde.js' });
  await new Promise((resolve) => setImmediate(resolve));
  return elements;
}

function flatten(nodes) { return nodes.flatMap((node) => [node, ...flatten(node.childNodes ?? [])]); }
function textOf(node) { return flatten([node]).map((item) => item.textContent || '').join(' '); }
function plain(value) { return JSON.parse(JSON.stringify(value)); }

test('login UI sends credentials to the main process and retry generates no renderer key', async () => {
  const calls = [];
  const api = {
    state: async () => ({ authenticated: false, origin: 'https://portal.example', storageReady: true, mappings: [], view: 'web' }),
    login: async (...args) => { calls.push(args); if (calls.length === 1) throw new Error("Error invoking remote method 'morde:login': Error: 用户名或密码错误，请重试。"); return { authenticated: true, origin: 'https://portal.example', storageReady: true, mappings: [], view: 'web' }; },
    onState() {}, onSync() {},
  };
  const elements = await mount(api);
  elements.get('name').value = 'user';
  elements.get('password').value = 'secret';
  elements.get('login-form').onsubmit({ preventDefault() {} });
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(elements.get('message').textContent, '用户名或密码错误，请重试。');
  elements.get('login-form').onsubmit({ preventDefault() {} });
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(calls, [
    ['https://portal.example', 'user', 'secret'],
    ['https://portal.example', 'user', 'secret'],
  ]);
  assert.equal(elements.get('password').value, '');
  assert.equal(elements.get('login').hidden, true);
  assert.equal(elements.has('user-code'), false);
});

test('local synchronization appears only on request and public pages take the entered origin', async () => {
  const calls = [];
  let current = { authenticated: true, origin: 'https://portal.example', storageReady: true, mappings: [], view: 'web' };
  const api = {
    state: async () => current,
    showSync: async () => { current = { ...current, view: 'sync' }; },
    workspaces: async () => ({ items: [] }),
    publicPage: async (...args) => calls.push(args),
    onState() {}, onSync() {},
  };
  const elements = await mount(api);
  assert.equal(elements.get('sync-page').hidden, true);
  elements.get('local-sync').onclick();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(elements.get('sync-page').hidden, false);
  elements.get('origin').value = 'https://portal.example';
  elements.get('register').onclick();
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(calls[0], ['register', 'https://portal.example']);
});

test('desktop account shortcut opens Morde account settings in the authenticated Portal view', async () => {
  let accountOpens = 0;
  const current = { authenticated: true, accountName: 'user', origin: 'https://portal.example', storageReady: true, mappings: [], view: 'web' };
  const elements = await mount({
    state: async () => current,
    showAccount: async () => { accountOpens++; },
    onState() {}, onSync() {},
  });
  assert.equal(elements.get('account').textContent, 'user · https://portal.example');
  elements.get('account-settings').onclick();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(accountOpens, 1);
});

test('workspace controls keep the fixed local-node switch and expose scoped one-shot sync and handoff actions', async () => {
  const current = { authenticated: true, origin: 'https://portal.example', storageReady: true, view: 'sync', syncState: 'idle',
    workspaceSync: { nodeState: 'stopped', bindings: {} }, mappings: [{ workspaceId: 'w1234567', directoryName: 'morde-test' }] };
  const calls = [];
  const elements = await mount({
    state: async () => current,
    workspaces: async () => ({ items: [{ id: 'w1234567', name: '验收', state: 'ready' }] }),
    setWorkspaceSyncEnabled: async (...args) => { calls.push(args); return current; },
    onState() {}, onSync() {},
  });
  const controls = flatten(elements.get('workspaces').childNodes);
  const selects = controls.filter((node) => node.tagName === 'select');
  assert.equal(selects[0].value, 'bidirectional');
  assert.equal(selects[1].value, 'cloud_workspace');
  assert.ok(controls.some((node) => node.tagName === 'textarea'));
  assert.ok(controls.some((node) => node.textContent === '双向同步并核验交接检查点'));
  controls.find((node) => node.textContent === '启用本机同步').onclick();
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(calls, [['w1234567', true]]);
});

test('an enabled Workspace binding offers a pause switch and keeps directory selection disabled', async () => {
  const current = { authenticated: true, storageReady: true, view: 'sync', syncState: 'idle',
    workspaceSync: { nodeState: 'running', bindings: { w1234567: { enabled: true, state: 'active' } } },
    mappings: [{ workspaceId: 'w1234567', directoryName: 'morde-test' }] };
  const calls = [];
  const elements = await mount({
    state: async () => current,
    workspaces: async () => ({ items: [{ id: 'w1234567', name: '验收', state: 'ready' }] }),
    setWorkspaceSyncEnabled: async (...args) => { calls.push(args); return current; },
    onState() {}, onSync() {},
  });
  const controls = elements.get('workspaces').childNodes[1].childNodes;
  assert.equal(controls.find((node) => node.textContent === '选择目录').disabled, true);
  controls.find((node) => node.textContent === '暂停本机同步').onclick();
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(calls, [['w1234567', false]]);
  const syncCard = flatten(elements.get('workspaces').childNodes).find((node) => node.className === 'card sync-card');
  assert.equal(flatten([syncCard]).find((node) => node.textContent === '开始同步').disabled, true);
  assert.equal(flatten([syncCard]).find((node) => node.textContent === '双向同步并核验交接检查点').disabled, true);
});

test('Workspace sync status is fetched only on request and displays a local aggregate snapshot', async () => {
  let calls = 0;
  const current = { authenticated: true, storageReady: true, view: 'sync', syncState: 'idle',
    workspaceSync: { nodeState: 'running', bindings: { w1234567: { enabled: true, state: 'active' } } },
    mappings: [{ workspaceId: 'w1234567', directoryName: 'workspace' }] };
  const elements = await mount({
    state: async () => current,
    workspaces: async () => ({ items: [{ id: 'w1234567', name: '验收', state: 'ready' }] }),
    workspaceSyncStatus: async (id) => { calls++; assert.equal(id, 'w1234567'); return { state: 'syncing', needBytes: 4096, needFiles: 2, pullErrors: 1, observedAt: '2026-11-03T00:00:00Z' }; },
    onState() {}, onSync() {},
  });
  const controls = flatten(elements.get('workspaces').childNodes);
  const refresh = controls.find((node) => node.textContent === '刷新本机同步状态');
  assert.equal(calls, 0);
  refresh.onclick();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(calls, 1);
  assert.match(textOf(elements.get('workspaces')), /本机状态：同步中 · 待同步 2 个文件 \/ 4\.0 KB · 上次同步错误 1 项/);
  assert.match(textOf(elements.get('workspaces')), /不代表服务端已确认/);
});

test('live sync UI fails closed when the service capability is disabled', async () => {
  const calls = [];
  const elements = await mount({
    state: async () => ({ authenticated: true, storageReady: true, view: 'sync', syncState: 'idle', liveSyncCapability: 'disabled',
      workspaceSync: { nodeState: 'stopped', bindings: {} },
      liveSyncs: { w1234567: { workspaceId: 'w1234567', state: 'unavailable', capability: 'disabled', message: '能力未开放', conflicts: [], recoveryRequired: false } },
      mappings: [{ workspaceId: 'w1234567', directoryName: 'workspace' }] }),
    workspaces: async () => ({ items: [{ id: 'w1234567', name: '验收', state: 'ready' }] }),
    startLiveSync: async (...args) => calls.push(['start', ...args]),
    stopLiveSync: async (...args) => calls.push(['stop', ...args]),
    onLiveSync() {}, onState() {}, onSync() {},
  });
  const liveCard = flatten(elements.get('workspaces').childNodes).find((node) => node.tagName === 'section' && node.childNodes[0]?.textContent === '旧版实时同步（本机）');
  const start = flatten([liveCard]).find((node) => node.textContent === '启动旧版实时同步');
  assert.equal(start.disabled, true);
  assert.match(textOf(liveCard), /能力未开放/);
  start.onclick();
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(calls, []);
});

test('live sync UI keeps status and events scoped to each Workspace and requires explicit actions', async () => {
  let current = { authenticated: true, storageReady: true, view: 'sync', syncState: 'idle', liveSyncCapability: 'enabled',
    workspaceSync: { nodeState: 'stopped', bindings: {} },
    liveSyncs: {
      w1234567: { workspaceId: 'w1234567', state: 'stopped', capability: 'enabled', scope: ['src'], rescanSeconds: 45, conflicts: [], recoveryRequired: false },
      w7654321: { workspaceId: 'w7654321', state: 'running', capability: 'enabled', scope: ['docs'], rescanSeconds: 90, sessionId: 'p7654321', conflicts: [], recoveryRequired: false },
    },
    mappings: [{ workspaceId: 'w1234567', directoryName: 'alpha' }, { workspaceId: 'w7654321', directoryName: 'beta' }] };
  const calls = [];
  let onLiveSync;
  const elements = await mount({
    state: async () => current,
    workspaces: async () => ({ items: [{ id: 'w1234567', name: 'Alpha', state: 'ready' }, { id: 'w7654321', name: 'Beta', state: 'ready' }] }),
    startLiveSync: async (...args) => calls.push(['start', ...args]),
    stopLiveSync: async (...args) => calls.push(['stop', ...args]),
    onLiveSync: (handler) => { onLiveSync = handler; }, onState() {}, onSync() {},
  });
  const workspaceCard = (name) => elements.get('workspaces').childNodes.find((node) => node.className === 'card' && node.childNodes[0]?.textContent.startsWith(name));
  const alpha = workspaceCard('Alpha');
  const beta = workspaceCard('Beta');
  const alphaLive = flatten([alpha]).find((node) => node.tagName === 'section' && node.childNodes[0]?.textContent === '旧版实时同步（本机）');
  const betaLive = flatten([beta]).find((node) => node.tagName === 'section' && node.childNodes[0]?.textContent === '旧版实时同步（本机）');
  assert.equal(flatten([alphaLive]).find((node) => node.tagName === 'textarea').value, 'src');
  assert.equal(flatten([alphaLive]).find((node) => node.tagName === 'input' && node.type === 'number').value, '45');
  assert.equal(flatten([betaLive]).find((node) => node.textContent === '停止旧版实时同步').disabled, false);

  flatten([alphaLive]).find((node) => node.textContent === '启动旧版实时同步').onclick();
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(plain(calls[0]), ['start', 'w1234567', ['src'], 45, false]);

  onLiveSync({ workspaceId: 'w7654321', event: { state: 'transferring', progress: { stage: 'transfer', confirmed_bytes: 10, planned_bytes: 20, current_path: '/private/beta/secret.txt' } } });
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  assert.match(textOf(elements.get('workspaces')), /10 B \/ 20 B/);
  assert.doesNotMatch(textOf(elements.get('workspaces')), /\/private\/beta\/secret\.txt/);

  const refreshedBeta = workspaceCard('Beta');
  const refreshedBetaLive = flatten([refreshedBeta]).find((node) => node.tagName === 'section' && node.childNodes[0]?.textContent === '旧版实时同步（本机）');
  flatten([refreshedBetaLive]).find((node) => node.textContent === '停止旧版实时同步').onclick();
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(plain(calls[1]), ['stop', 'w7654321']);
});

test('live sync recovery remains manual and uses its saved scope and interval', async () => {
  const calls = [];
  const elements = await mount({
    state: async () => ({ authenticated: true, storageReady: true, view: 'sync', syncState: 'idle', liveSyncCapability: 'enabled',
      workspaceSync: { nodeState: 'stopped', bindings: {} },
      liveSyncs: { w1234567: { workspaceId: 'w1234567', state: 'persisted_uncertain', capability: 'enabled', scope: ['docs'], rescanSeconds: 60, recoveryRequired: true, conflicts: [] } },
      mappings: [{ workspaceId: 'w1234567', directoryName: 'workspace' }] }),
    workspaces: async () => ({ items: [{ id: 'w1234567', name: '验收', state: 'ready' }] }),
    startLiveSync: async (...args) => calls.push(args),
    stopLiveSync: async () => {},
    onLiveSync() {}, onState() {}, onSync() {},
  });
  const liveCard = flatten(elements.get('workspaces').childNodes).find((node) => node.tagName === 'section' && node.childNodes[0]?.textContent === '旧版实时同步（本机）');
  const controls = flatten([liveCard]);
  const start = controls.find((node) => node.textContent === '确认并恢复旧版实时同步');
  const confirmation = controls.find((node) => node.tagName === 'input' && node.type === 'checkbox');
  assert.equal(start.disabled, true);
  assert.equal(calls.length, 0, 'mounting a recovered Workspace must never resume sync automatically');
  confirmation.checked = true;
  confirmation.onchange();
  assert.equal(start.disabled, false);
  start.onclick();
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(plain(calls), [['w1234567', ['docs'], 60, true]]);
});

test('single Workspace sync passes the chosen direction and relative scope and only reports verified completion', async () => {
  let onSync, finishSync;
  let current = { authenticated: true, storageReady: true, view: 'sync', syncState: 'idle', workspaceSync: { nodeState: 'stopped', bindings: {} },
    mappings: [{ workspaceId: 'w1234567', directoryName: 'workspace' }] };
  const calls = [];
  const elements = await mount({
    state: async () => current,
    workspaces: async () => ({ items: [{ id: 'w1234567', name: '验收', state: 'ready' }] }),
    sync: (...args) => { calls.push(args); return new Promise((resolve) => { finishSync = resolve; }); },
    onState() {}, onSync: (handler) => { onSync = handler; },
  });
  const controls = flatten(elements.get('workspaces').childNodes);
  const mode = controls.find((node) => node.tagName === 'select');
  mode.value = 'upload';
  mode.onchange();
  const scope = controls.find((node) => node.tagName === 'textarea');
  scope.value = 'src\n README.md';
  scope.oninput();
  controls.find((node) => node.textContent === '开始同步').onclick();
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(plain(calls), [['w1234567', 'upload', ['src', 'README.md'], false]]);
  onSync({ state: 'running', workspaceId: 'w1234567', mode: 'upload', progress: { stage: 'transferring', confirmed_bytes: 512, planned_bytes: 1024, current_path: 'src/main.ts' } });
  await new Promise((resolve) => setImmediate(resolve));
  assert.match(textOf(elements.get('workspaces')), /512 B \/ 1\.0 KB/);
  assert.doesNotMatch(textOf(elements.get('workspaces')), /src\/main\.ts/);
  current = { ...current, syncState: 'idle' };
  finishSync({ session: { id: 'p1234567', workspace_id: 'w1234567', state: 'completed' }, conflicts: [] });
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  assert.match(elements.get('message').textContent, /上传到 Morde已完成/);
  assert.match(textOf(elements.get('workspaces')), /上传到 Morde已完成/);
});

test('unknown and conflicting results stay visibly unconfirmed and retain the recovery point', async () => {
  for (const result of [
    { session: { id: 'p1234567', workspace_id: 'w1234567', state: 'conflict' }, conflicts: [{ path: 'src/main.ts', reason: 'content_changed' }] },
    { unexpected: true },
  ]) {
    const current = { authenticated: true, storageReady: true, view: 'sync', syncState: 'idle',
      syncCheckpoint: { workspaceId: 'w1234567', mode: 'bidirectional' },
      workspaceSync: { nodeState: 'stopped', bindings: {} }, mappings: [{ workspaceId: 'w1234567', directoryName: 'workspace' }] };
    const calls = [];
    const elements = await mount({
      state: async () => current,
      workspaces: async () => ({ items: [{ id: 'w1234567', name: '验收', state: 'ready' }] }),
      sync: async (...args) => { calls.push(args); return result; },
      onState() {}, onSync() {},
    });
    const controls = flatten(elements.get('workspaces').childNodes);
    controls.find((node) => node.textContent.startsWith('按原模式')).onclick();
    await new Promise((resolve) => setImmediate(resolve));
    await new Promise((resolve) => setImmediate(resolve));
    assert.deepEqual(plain(calls), [['w1234567', 'bidirectional', [], false]]);
    assert.ok(current.syncCheckpoint, 'the recovery point must remain available');
    if (result.session) {
      assert.match(textOf(elements.get('workspaces')), /发现冲突/);
      assert.match(textOf(elements.get('workspaces')), /src\/main\.ts/);
      assert.doesNotMatch(elements.get('message').textContent, /已完成/);
    } else {
      assert.match(elements.get('message').textContent, /结果未获确认/);
      assert.doesNotMatch(textOf(elements.get('workspaces')), /双向补齐缺失文件已完成/);
    }
  }
});

test('handoff requires an explicit quiesced confirmation and a paused Workspace live binding', async () => {
  for (const enabled of [false, true]) {
    const current = { authenticated: true, storageReady: true, view: 'sync', syncState: 'idle',
      workspaceSync: { nodeState: enabled ? 'running' : 'stopped', bindings: { w1234567: { enabled, state: enabled ? 'active' : 'paused' } } },
      mappings: [{ workspaceId: 'w1234567', directoryName: 'workspace' }] };
    const calls = [];
    const elements = await mount({
      state: async () => current,
      workspaces: async () => ({ items: [{ id: 'w1234567', name: '验收', state: 'ready' }] }),
      sync: async (...args) => { calls.push(args); return { revision: 21 }; },
      onState() {}, onSync() {},
    });
    const controls = flatten(elements.get('workspaces').childNodes);
    const checkbox = controls.find((node) => node.tagName === 'input' && node.type === 'checkbox');
    const handoff = controls.find((node) => node.textContent === '双向同步并核验交接检查点');
    assert.equal(handoff.disabled, true);
    handoff.onclick();
    await new Promise((resolve) => setImmediate(resolve));
    assert.deepEqual(calls, []);
    if (enabled) assert.equal(checkbox.disabled, true);
    else assert.match(elements.get('message').textContent, /需要勾选人工停写确认/);
  }
});

test('confirmed handoff uses the dedicated mode and only reports a verified revision', async () => {
  const calls = [];
  let current = { authenticated: true, storageReady: true, view: 'sync', syncState: 'idle',
    workspaceSync: { nodeState: 'stopped', bindings: { w1234567: { enabled: false, state: 'paused' } } },
    mappings: [{ workspaceId: 'w1234567', directoryName: 'workspace' }] };
  const elements = await mount({
    state: async () => current,
    workspaces: async () => ({ items: [{ id: 'w1234567', name: '验收', state: 'ready' }] }),
    sync: async (...args) => {
      calls.push(args);
      current = { ...current, handoff: { id: 'handoff-1', workspaceId: 'w1234567', scope: [], source: 'cloud_workspace', target: 'desktop', phase: 'verified', revision: 21 } };
      return { revision: 21 };
    },
    onState() {}, onSync() {},
  });
  const controls = flatten(elements.get('workspaces').childNodes);
  const target = controls.filter((node) => node.tagName === 'select')[1];
  target.value = 'desktop';
  target.onchange();
  const checkbox = controls.find((node) => node.tagName === 'input' && node.type === 'checkbox');
  checkbox.checked = true;
  checkbox.onchange();
  const handoff = controls.find((node) => node.textContent === '双向同步并核验交接检查点');
  assert.equal(handoff.disabled, false);
  handoff.onclick();
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(plain(calls), [['w1234567', 'handoff_checkpoint', [], true, 'desktop']]);
  assert.match(elements.get('message').textContent, /版本 21/);
  assert.match(textOf(elements.get('workspaces')), /交接检查点已核验（版本 21）/);
  assert.match(textOf(elements.get('workspaces')), /云端 Workspace → 此电脑 · 已核验 · 版本 21/);
  assert.match(textOf(elements.get('workspaces')), /不会启动远端工作进程或绑定 Host/);
});

test('checkpoint continuation uses its original Workspace and mode even when the editor differs', async () => {
  const calls = [];
  const current = { authenticated: true, storageReady: true, view: 'sync', syncState: 'idle',
    syncCheckpoint: { workspaceId: 'w1234567', mode: 'upload', scope: ['src', 'README.md'] },
    workspaceSync: { nodeState: 'stopped', bindings: {} }, mappings: [{ workspaceId: 'w1234567', directoryName: 'workspace' }] };
  const elements = await mount({
    state: async () => current,
    workspaces: async () => ({ items: [{ id: 'w1234567', name: '验收', state: 'ready' }] }),
    sync: async (...args) => { calls.push(args); return { session: { id: 'p1234567', workspace_id: 'w1234567', state: 'completed' }, conflicts: [] }; },
    onState() {}, onSync() {},
  });
  const controls = flatten(elements.get('workspaces').childNodes);
  const mode = controls.find((node) => node.tagName === 'select');
  const scope = controls.find((node) => node.tagName === 'textarea');
  assert.equal(mode.disabled, true);
  assert.equal(mode.value, 'upload');
  assert.equal(scope.value, 'src\nREADME.md');
  controls.find((node) => node.textContent.startsWith('按原模式')).onclick();
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(plain(calls), [['w1234567', 'upload', ['src', 'README.md'], false]]);
});

test('legacy live-sync checkpoints can only continue in their original mode', async () => {
  const calls = [];
  const current = { authenticated: true, storageReady: true, view: 'sync', syncState: 'idle',
    syncCheckpoint: { workspaceId: 'w1234567', mode: 'live_sync', scope: ['src'] },
    workspaceSync: { nodeState: 'stopped', bindings: {} }, mappings: [{ workspaceId: 'w1234567', directoryName: 'workspace' }] };
  const elements = await mount({
    state: async () => current,
    workspaces: async () => ({ items: [{ id: 'w1234567', name: '验收', state: 'ready' }] }),
    sync: async (...args) => { calls.push(args); return { session: { id: 'p1234567', workspace_id: 'w1234567', state: 'completed' }, conflicts: [] }; },
    onState() {}, onSync() {},
  });
  const controls = flatten(elements.get('workspaces').childNodes);
  const mode = controls.find((node) => node.tagName === 'select');
  assert.equal(mode.value, 'live_sync');
  assert.equal(mode.disabled, true);
  assert.ok(mode.childNodes.some((option) => option.value === 'live_sync'));
  controls.find((node) => node.textContent.startsWith('按原模式')).onclick();
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(plain(calls), [['w1234567', 'live_sync', ['src'], false]]);
});

test('handoff recovery pins its original target and scope while showing the durable review record', async () => {
  const calls = [];
  const current = { authenticated: true, storageReady: true, view: 'sync', syncState: 'idle',
    syncCheckpoint: { workspaceId: 'w1234567', mode: 'handoff_checkpoint', scope: ['src'], handoffTarget: 'desktop' },
    handoff: { id: 'handoff-1', workspaceId: 'w1234567', scope: ['src'], source: 'cloud_workspace', target: 'desktop', phase: 'needs_review' },
    workspaceSync: { nodeState: 'stopped', bindings: {} }, mappings: [{ workspaceId: 'w1234567', directoryName: 'workspace' }] };
  const elements = await mount({
    state: async () => current,
    workspaces: async () => ({ items: [{ id: 'w1234567', name: '验收', state: 'ready' }] }),
    sync: async (...args) => { calls.push(args); return { revision: 22 }; },
    onState() {}, onSync() {},
  });
  const controls = flatten(elements.get('workspaces').childNodes);
  const target = controls.filter((node) => node.tagName === 'select')[1];
  const scope = controls.find((node) => node.tagName === 'textarea');
  assert.equal(target.value, 'desktop');
  assert.equal(target.disabled, true);
  assert.equal(scope.value, 'src');
  assert.match(textOf(elements.get('workspaces')), /需要检查/);
  const checkbox = controls.find((node) => node.tagName === 'input' && node.type === 'checkbox');
  checkbox.checked = true;
  checkbox.onchange();
  controls.find((node) => node.textContent === '按原交接检查点恢复').onclick();
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(plain(calls), [['w1234567', 'handoff_checkpoint', ['src'], true, 'desktop']]);
});

test('discarding a sync checkpoint requires explicit confirmation', async () => {
  const confirmations = [], discards = [];
  let current = { authenticated: true, storageReady: true, view: 'sync', syncState: 'idle',
    syncCheckpoint: { workspaceId: 'w1234567', mode: 'download' },
    workspaceSync: { nodeState: 'stopped', bindings: {} }, mappings: [{ workspaceId: 'w1234567', directoryName: 'workspace' }] };
  const elements = await mount({
    state: async () => current,
    workspaces: async () => ({ items: [{ id: 'w1234567', name: '验收', state: 'ready' }] }),
    discardSync: async () => { discards.push(true); current = { ...current, syncCheckpoint: null }; },
    onState() {}, onSync() {},
  }, (prompt) => { confirmations.push(prompt); return confirmations.length > 1; });
  let discard = flatten(elements.get('workspaces').childNodes).find((node) => node.textContent === '明确放弃恢复点');
  discard.onclick();
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(discards, []);
  assert.ok(current.syncCheckpoint);
  discard = flatten(elements.get('workspaces').childNodes).find((node) => node.textContent === '明确放弃恢复点');
  discard.onclick();
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(discards, [true]);
  assert.equal(current.syncCheckpoint, null);
  assert.match(elements.get('message').textContent, /已明确放弃同步恢复点/);
  assert.equal(confirmations.length, 2);
  assert.match(confirmations[0], /不能从该检查点继续/);
});

test('secure storage failures surface before login', async () => {
  const elements = await mount({ state: async () => ({ authenticated: false, storageReady: false, mappings: [], view: 'web' }), onState() {}, onSync() {} });
  assert.match(elements.get('message').textContent, /系统安全存储不可用/);
  assert.equal(elements.get('message').className, 'error');
});

test('damaged local sync state is shown without offering sync actions', async () => {
  const warning = '本机同步恢复点无法读取，已暂停同步';
  const elements = await mount({
    state: async () => ({ authenticated: true, storageReady: true, mappings: [], view: 'sync', profileError: warning }),
    workspaces: async () => { throw new Error('must not fetch workspaces'); },
    onState() {}, onSync() {},
  });
  assert.equal(elements.get('workspaces').childNodes[0].textContent, warning);
  assert.equal(elements.get('workspaces').childNodes[0].className, 'error');
  assert.equal(elements.get('workspaces').childNodes.length, 1);
});

test('a legacy sync remains stoppable and an uncertain stop keeps its recovery warning', async () => {
  let onSync;
  let current = { authenticated: true, storageReady: true, view: 'sync', syncState: 'idle',
    syncCheckpoint: { workspaceId: 'w1234567', mode: 'live_sync' },
    mappings: [{ workspaceId: 'w1234567', directoryName: 'morde-test' }] };
  const elements = await mount({
    state: async () => current,
    workspaces: async () => ({ items: [{ id: 'w1234567', name: '验收', state: 'ready' }] }),
    stopSync: async () => { current = { ...current, syncState: 'shutdown_unknown' }; return false; },
    onState() {}, onSync: (handler) => { onSync = handler; },
  });
  onSync({ state: 'running', progress: { stage: 'watching' } });
  await new Promise((resolve) => setImmediate(resolve));
  let root = elements.get('workspaces');
  assert.equal(root.childNodes[0].textContent, '旧版同步进程仍在运行。');
  let stop = root.childNodes.find((node) => node.textContent === '停止同步');
  assert.equal(stop.disabled, false);

  stop.onclick();
  await new Promise((resolve) => setImmediate(resolve));
  root = elements.get('workspaces');
  assert.match(root.childNodes[0].childNodes[0].textContent, /尚待恢复/);
  assert.equal(elements.get('message').textContent, '同步停止未获确认；检查点恢复点已保留。');
  assert.equal(elements.get('message').className, 'error');
  const controls = root.childNodes.find((node) => node.childNodes.some((child) => child.textContent.includes('验收'))).childNodes;
  assert.equal(controls.find((node) => node.textContent === '选择目录').disabled, true);
  assert.equal(controls.find((node) => node.textContent === '启用本机同步').disabled, true);
});

test('a running sync remains stoppable while Workspace loading times out', async () => {
  let rejectWorkspaces, stopCount = 0;
  const request = new Promise((_, reject) => { rejectWorkspaces = reject; });
  const current = { authenticated: true, storageReady: true, view: 'sync', syncState: 'running', mappings: [] };
  const elements = await mount({
    state: async () => current,
    workspaces: async () => request,
    stopSync: async () => { stopCount++; return true; },
    onState() {}, onSync() {},
  });
  let root = elements.get('workspaces');
  assert.equal(root.childNodes[0].textContent, '正在读取 Workspace…');
  assert.equal(root.childNodes.find((node) => node.textContent === '停止同步').disabled, false);
  rejectWorkspaces(new Error('timeout'));
  await new Promise((resolve) => setImmediate(resolve));
  root = elements.get('workspaces');
  assert.match(root.childNodes[0].textContent, /云端 Workspace 当前不可读取/);
  root.childNodes.find((node) => node.textContent === '停止同步').onclick();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(stopCount, 1);
});

test('late Workspace response or error from a previous account cannot replace the current view', async () => {
  for (const failOld of [false, true]) {
    let onState, finishOld, rejectOld;
    const oldResponse = new Promise((resolve, reject) => { finishOld = resolve; rejectOld = reject; });
    let requests = 0;
    const elements = await mount({
      state: async () => ({ authenticated: true, origin: 'https://old.example', storageReady: true, mappings: [], view: 'sync' }),
      workspaces: async () => ++requests === 1 ? oldResponse : { items: [{ id: 'w1234567', name: '新账号目录', state: 'ready' }] },
      onState: (handler) => { onState = handler; }, onSync() {},
    });
    onState({ authenticated: true, origin: 'https://new.example', storageReady: true, mappings: [], view: 'sync' });
    await new Promise((resolve) => setImmediate(resolve));
    if (failOld) rejectOld(new Error('旧账号请求失败'));
    else finishOld({ items: [{ id: 'w7654321', name: '旧账号目录', state: 'ready' }] });
    await new Promise((resolve) => setImmediate(resolve));
    const contents = JSON.stringify(elements.get('workspaces').childNodes.map((card) => card.childNodes.map((item) => item.textContent)));
    assert.match(contents, /新账号目录/);
    assert.doesNotMatch(contents, /旧账号目录/);
    assert.doesNotMatch(elements.get('message').textContent, /旧账号请求失败/);
  }
});

test('offline Workspace loading keeps a local safety-copy section and renders untrusted paths as text', async () => {
  const maliciousPath = '<img src=x onerror=alert(1)>.txt';
  const elements = await mount({
    state: async () => ({ authenticated: true, origin: 'https://portal.example', storageReady: true, view: 'sync', syncState: 'idle',
      mappings: [{ workspaceId: 'w1234567', directoryName: 'workspace' }] }),
    workspaces: async () => { throw new Error('network unavailable'); },
    preserved: async () => [{ id: 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', workspace_id: 'w1234567', path: maliciousPath, created_at: '2026-10-31T10:00:00Z', size: 2048 }],
    onState() {}, onSync() {},
  });
  await new Promise((resolve) => setImmediate(resolve));
  const root = elements.get('workspaces');
  const all = flatten(root.childNodes);
  assert.match(all.map((node) => node.textContent).join(' '), /云端 Workspace 当前不可读取/);
  assert.ok(all.some((node) => node.textContent === maliciousPath));
  assert.ok(all.some((node) => node.textContent.includes('额外占用 2.0 KB')));
  assert.equal(all.some((node) => node.tagName === 'img'), false);
  assert.equal(elements.get('message').textContent, 'network unavailable');
});

test('cancelled restore and deletion confirmations never call the local IPC bridge', async () => {
  const restoreCalls = [], deleteCalls = [], confirmations = [];
  const copy = { id: 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', workspace_id: 'w1234567', path: 'private notes.txt', created_at: '2026-10-31T10:00:00Z', size: 2048 };
  const elements = await mount({
    state: async () => ({ authenticated: true, storageReady: true, view: 'sync', syncState: 'idle', mappings: [{ workspaceId: 'w1234567', directoryName: 'workspace' }] }),
    workspaces: async () => ({ items: [{ id: 'w1234567', name: 'Notes', state: 'ready' }] }),
    preserved: async () => [copy],
    restorePreserved: async (...args) => restoreCalls.push(args),
    deletePreserved: async (...args) => deleteCalls.push(args),
    onState() {}, onSync() {},
  }, (prompt) => { confirmations.push(prompt); return false; });
  await new Promise((resolve) => setImmediate(resolve));
  const buttons = flatten(elements.get('workspaces').childNodes).filter((node) => node.tagName === 'button');
  buttons.find((node) => node.textContent === '恢复到本机').onclick();
  buttons.find((node) => node.textContent === '删除此副本').onclick();
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(restoreCalls, []);
  assert.deepEqual(deleteCalls, []);
  assert.equal(confirmations.length, 2);
  assert.match(confirmations[0], /目标路径必须为空/);
  assert.match(confirmations[0], /归档副本仍会保留/);
  assert.match(confirmations[1], /private notes\.txt.*2\.0 KB/);
  assert.match(confirmations[1], /仅删除这份本机副本，无法恢复，不影响云端 Workspace/);
});

test('confirmed restore and delete invoke only the selected local safety-copy actions', async () => {
  const restoreCalls = [], deleteCalls = [], confirmations = [];
  const copy = { id: 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', workspace_id: 'w1234567', path: 'notes.txt', created_at: '2026-10-31T10:00:00Z', size: 19 };
  const elements = await mount({
    state: async () => ({ authenticated: true, storageReady: true, view: 'sync', syncState: 'idle', mappings: [{ workspaceId: 'w1234567', directoryName: 'workspace' }] }),
    workspaces: async () => ({ items: [{ id: 'w1234567', name: 'Notes', state: 'ready' }] }),
    preserved: async () => [copy],
    restorePreserved: async (...args) => restoreCalls.push(args),
    deletePreserved: async (...args) => deleteCalls.push(args),
    onState() {}, onSync() {},
  }, (prompt) => { confirmations.push(prompt); return true; });
  await new Promise((resolve) => setImmediate(resolve));
  let buttons = flatten(elements.get('workspaces').childNodes).filter((node) => node.tagName === 'button');
  buttons.find((node) => node.textContent === '恢复到本机').onclick();
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(restoreCalls, [['w1234567', copy.id]]);
  assert.match(elements.get('message').textContent, /已恢复本机文件/);
  buttons = flatten(elements.get('workspaces').childNodes).filter((node) => node.tagName === 'button');
  buttons.find((node) => node.textContent === '删除此副本').onclick();
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(deleteCalls, [['w1234567', copy.id]]);
  assert.match(elements.get('message').textContent, /已删除本机安全副本/);
  assert.equal(confirmations.length, 2);
});

test('preload exposes manual sync and dedicated safety-copy IPC calls', async () => {
  const require = createRequire(import.meta.url);
  const Module = require('node:module');
  const originalLoad = Module._load;
  const entry = require.resolve('../out/morde-preload.js');
  const calls = [];
  let exposed;
  Module._load = function (request, parent, isMain) {
    if (request === 'electron') return {
      contextBridge: { exposeInMainWorld: (name, value) => { assert.equal(name, 'morde'); exposed = value; } },
      ipcRenderer: { invoke: (...args) => { calls.push(args); return Promise.resolve(null); }, on() {}, off() {} },
    };
    return originalLoad.call(this, request, parent, isMain);
  };
  try {
    delete require.cache[entry];
    require(entry);
    await exposed.sync('w1234567', 'upload', ['src'], false);
    await exposed.preserved('w1234567');
    await exposed.restorePreserved('w1234567', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa');
    await exposed.deletePreserved('w1234567', 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb');
    assert.deepEqual(calls, [
      ['morde:sync', 'w1234567', 'upload', ['src'], false],
      ['morde:preserved-list', 'w1234567'],
      ['morde:preserved-restore', 'w1234567', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'],
      ['morde:preserved-delete', 'w1234567', 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'],
    ]);
  } finally {
    Module._load = originalLoad;
    delete require.cache[entry];
  }
});
