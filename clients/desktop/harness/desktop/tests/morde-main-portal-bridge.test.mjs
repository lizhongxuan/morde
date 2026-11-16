import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createRequire } from 'node:module';
import { createHash } from 'node:crypto';
import { mkdirSync, mkdtempSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const require = createRequire(import.meta.url);
process.env.MORDE_TEST_PORTAL_ASSETS = fileURLToPath(new URL('./portal-fixture', import.meta.url));
const future = '2099-01-01T00:00:00Z';
const serviceInstanceID = '12345678-1234-4234-8234-123456789abc';
const auth = {
  origin: 'https://portal.example', serviceInstanceID, accountName: 'local-test', installationId: 'i1234567',
  clientCredential: 'local-client-credential', clientExpiresAt: future,
};
const capabilities = {
  service_instance_id: serviceInstanceID, api_version: '1', hub_version: null, event_version: null,
  minimum_client_version: null, supported_features: ['password_login', 'client_installations', 'ordered_client_login', 'workspace_sync', 'project_upload'],
  default_max_request_bytes: 1048576,
};
const host = {
  id: 'h1234567', owner_id: 'u1234567', name: 'Local test computer', url: 'https://host.example/', state: 'running',
  workspaces: [{ state: 'active', workspace: { id: 'w1234567', name: 'Default Workspace', state: 'ready' } }],
};
const json = (value, status = 200) => new Response(JSON.stringify(value && typeof value === 'object' && 'web_session_credential' in value && !('web_session_host_frame_source' in value) ? { ...value, web_session_host_frame_source: 'https://*.hosts.example.test' } : value), { status, headers: { 'content-type': 'application/json' } });
const portalStateKeys = ['projectMappings', 'syncCheckpoint', 'syncRecovery', 'syncState'];
const appURL = (h, path) => `${new URL(h.views[0].webContents.url).origin}${path}`;
function assertPortalProjectState(state) {
  assert.deepEqual(Object.keys(state).sort(), portalStateKeys);
  for (const key of ['accountName', 'origin', 'installationId', 'expiresAt', 'mappings', 'workspaceSync', 'liveSyncs', 'handoff', 'profileError', 'storageReady', 'view']) {
    assert.equal(Object.hasOwn(state, key), false, `Portal state must not expose ${key}`);
  }
  const serialized = JSON.stringify(state);
  for (const privateValue of ['local-client-credential', 'portal.example', 'i1234567', 'credentials.bin']) {
    assert.equal(serialized.includes(privateValue), false, `Portal state must not expose ${privateValue}`);
  }
}

function mockFetch(calls) {
  globalThis.fetch = async (input, options = {}) => {
    const url = new URL(String(input));
    calls.push({ url: url.href, method: options.method ?? 'GET', body: options.body });
    if (url.pathname === '/api/capabilities') return json(capabilities);
    if (url.pathname === '/api/auth/client-web-session') return json({ web_session_credential: 'local-web-session', web_session_expires_at: future });
    if (url.pathname === '/api/auth/client-logout') return new Response(null, { status: 204 });
    if (url.pathname === '/api/me') return json({ id: 'u1234567' });
    if (url.pathname === '/api/me/preferences') return json({ default_host_id: 'h1234567' });
    if (url.pathname === '/api/hosts') return json({ items: [host] });
    if (url.pathname === '/api/hosts/h1234567') return json(host);
    if (url.pathname === '/api/hosts/h1234567/sync-folders') return json({ items: [] });
    return json({ error: { code: 'not_found', message: 'local mock route missing' } }, 404);
  };
}

function harness({ checkpoint } = {}) {
  const root = mkdtempSync(join(tmpdir(), 'morde-p11-portal-'));
  const intervals = new Set();
  const originalSetInterval = globalThis.setInterval;
  globalThis.setInterval = (...args) => {
    const interval = originalSetInterval(...args);
    intervals.add(interval);
    return interval;
  };
  const userData = join(root, 'Morde Client');
  mkdirSync(userData, { recursive: true });
  writeFileSync(join(userData, 'credentials.bin'), JSON.stringify({ version: 2, credentials: auth }));
  if (checkpoint) {
    const profile = createHash('sha256').update(`${auth.origin}\0${auth.serviceInstanceID}\0${auth.accountName}\0${auth.installationId}`).digest('hex');
    writeFileSync(join(userData, `sync-checkpoint-${profile}.json`), JSON.stringify(checkpoint));
  }
  const handlers = new Map(), windows = [], views = [], partitions = [], appEvents = new Map(), menus = [];
  let nextContentsID = 1;
  let portalLoaded;
  const portalReady = new Promise((resolve) => { portalLoaded = resolve; });

  class FakeContents {
    id = nextContentsID++;
    events = new Map();
    messages = [];
    url = '';
    closed = false;
    mainFrame = {};
    on(name, fn) { this.events.set(name, fn); }
    setWindowOpenHandler(fn) { this.open = fn; }
    async loadURL(url) { this.url = url; if (this.isPortal) portalLoaded(); }
    getURL() { return this.url; }
    isDestroyed() { return this.closed; }
    focus() { this.focused = true; }
    close() { this.closed = true; this.events.get('destroyed')?.(); }
    send(...args) { this.messages.push(args); }
  }

  class FakeWindow {
    constructor(options) {
      this.options = options;
      this.events = new Map();
      this.webContents = options.webContents ?? new FakeContents();
      this.contentBounds = { x: 0, y: 0, width: 1120, height: 760 };
      this.contentView = {
        attached: [], addChildView: (view) => this.contentView.attached.push(view),
        removeChildView: (view) => { this.contentView.attached = this.contentView.attached.filter((item) => item !== view); },
      };
      this.minimized = false;
      windows.push(this);
    }
    on(name, fn) { this.events.set(name, fn); }
    once(name, fn) { this.events.set(name, fn); }
    async loadFile(path) { this.path = path; }
    async loadURL(url) { this.webContents.url = url; }
    show() { this.shown = true; }
    isMinimized() { return this.minimized; }
    restore() { this.minimized = false; this.restored = true; }
    getContentBounds() { return this.contentBounds; }
    getContentSize() { return [this.contentBounds.width, this.contentBounds.height]; }
    focus() { this.focused = true; }
    isDestroyed() { return this.destroyed ?? false; }
    close() { this.destroyed = true; this.events.get('closed')?.(); }
  }

  class FakeView {
    constructor(options) { this.options = options; this.webContents = new FakeContents(); this.webContents.isPortal = views.length === 0; views.push(this); }
    setBounds(bounds) { this.bounds = bounds; }
    setBorderRadius(radius) { this.borderRadius = radius; }
    setVisible(visible) { this.visible = visible; }
  }

  const fakeSession = { fromPartition(name) {
    const partition = {
      name, cookies: { values: [], async set(value) { this.values.push(value); } },
      webRequest: {
        onCompleted(_filter, fn) { this.completed = fn; },
        onHeadersReceived(_filter, fn) { this.headersReceived = fn; },
      },
      events: new Map(), on(event, fn) { this.events.set(event, fn); },
      setPermissionRequestHandler(fn) { this.permissionRequest = fn; },
      async clearStorageData() { this.cleared = true; this.clearCount = (this.clearCount ?? 0) + 1; this.cookies.values = []; },
    };
    partitions.push(partition);
    return partition;
  } };
  const dialog = {
    showOpenDialog: async () => ({ canceled: true, filePaths: [] }),
    showMessageBox: async () => ({ response: 1 }),
  };
  const paths = { appData: root };
  const electron = {
    app: {
      setName() {}, getPath: (key) => paths[key] ?? root, setPath: (key, path) => { paths[key] = path; },
      getLocale: () => 'en', getVersion: () => '0.1.0', requestSingleInstanceLock: () => true,
      isPackaged: false, whenReady: () => Promise.resolve(), on: (name, fn) => appEvents.set(name, fn), quit() {},
    },
    BrowserWindow: FakeWindow, WebContentsView: FakeView, session: fakeSession,
    ipcMain: { handle: (channel, fn) => handlers.set(channel, fn) }, dialog,
    Menu: { buildFromTemplate: (template) => ({ template, popup: (options) => menus.push({ template, options }) }), setApplicationMenu() {} },
    safeStorage: {
      isEncryptionAvailable: () => true, getSelectedStorageBackend: () => 'test',
      encryptString: (value) => Buffer.from(value), decryptString: (value) => value.toString(),
    },
    shell: { openExternal: async () => undefined },
    Tray: class { setToolTip() {} setContextMenu() {} on() {} },
  };
  const Module = require('node:module');
  const originalLoad = Module._load;
  const entry = require.resolve('../out/morde-main.js');
  Module._load = function (request, parent, isMain) {
    if (request === 'electron') return electron;
    return originalLoad.call(this, request, parent, isMain);
  };
  delete require.cache[entry];
  try { require(entry); }
  finally { Module._load = originalLoad; }

  return {
    root, userData, handlers, windows, views, partitions, appEvents, dialog, menus,
    async ready() {
      let timeout;
      try {
        await Promise.race([portalReady, new Promise((_, reject) => {
          timeout = setTimeout(() => reject(new Error('Portal view did not load')), 5000);
        })]);
      } finally { clearTimeout(timeout); }
      await new Promise((resolve) => setImmediate(resolve));
      globalThis.setInterval = originalSetInterval;
    },
    invoke(channel, sender, args = [], senderFrame = sender.mainFrame) {
      return handlers.get(channel)({ sender, senderFrame }, ...args);
    },
    cleanup() {
      globalThis.setInterval = originalSetInterval;
      for (const interval of intervals) clearInterval(interval);
      delete require.cache[entry];
      rmSync(root, { recursive: true, force: true });
    },
  };
}

test('Portal text fields expose native editing commands in the context menu', async () => {
  const originalFetch = globalThis.fetch;
  mockFetch([]);
  const h = harness();
  try {
    await h.ready();
    const listener = h.views[0].webContents.events.get('context-menu');
    assert.equal(typeof listener, 'function');
    const params = { isEditable: true, selectionText: '', editFlags: {
      canUndo: true, canRedo: false, canCut: true, canCopy: false, canPaste: true, canSelectAll: true,
    } };
    listener({}, params);
    const menu = h.menus.at(-1);
    assert.equal(menu.options.window, h.windows[0]);
    assert.deepEqual(menu.template.map(({ role, enabled }) => [role ?? 'separator', enabled]), [
      ['undo', true], ['redo', false], ['separator', undefined], ['cut', true], ['copy', false], ['paste', true], ['selectAll', true],
    ]);
    const menuCount = h.menus.length;
    listener({}, { ...params, isEditable: false });
    assert.equal(h.menus.length, menuCount);
  } finally {
    globalThis.fetch = originalFetch;
    h.cleanup();
  }
});

test('Codex dialogs use the correct overload with or without a parent window', async () => {
  const originalFetch = globalThis.fetch;
  mockFetch([]);
  const h = harness();
  try {
    await h.ready();
    const portal = h.views[0].webContents;
    portal.url = appURL(h, '/codex');
    for (const withParent of [true, false]) {
      h.windows[0].destroyed = !withParent;
      const opened = [], confirmations = [];
      h.dialog.showOpenDialog = async (...args) => {
        opened.push(args);
        return opened.length === 1 ? { canceled: false, filePaths: [h.root] } : { canceled: true, filePaths: [] };
      };
      h.dialog.showMessageBox = async (...args) => { confirmations.push(args); return { response: 0 }; };
      await h.invoke('morde:codex-add-profile', portal);
      await h.invoke('morde:codex-authorize-profile', portal, ['profile-a', ['read', 'live']]);
      await h.invoke('morde:codex-revoke-profile', portal, ['profile-a']);
      assert.deepEqual(opened.map((args) => args.at(-1).properties), [['openDirectory'], ['openFile']]);
      for (const args of [...opened, ...confirmations]) {
        assert.equal(args.length, withParent ? 2 : 1);
        if (withParent) assert.equal(args[0], h.windows[0]);
      }
      assert.equal(confirmations.length, 2);
    }
  } finally {
    globalThis.fetch = originalFetch;
    h.cleanup();
  }
});

test('Portal bridge requires the current trusted top-level route and returns only empty-folder metadata', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  mockFetch(calls);
  const h = harness();
  const selectedRoot = mkdtempSync(join(process.cwd(), '.morde-p11-empty-project-'));
  const selectedDirectory = join(selectedRoot, 'empty-project');
  mkdirSync(selectedDirectory);
  try {
    await h.ready();
    const portal = h.views[0];
    assert.equal(portal.options.webPreferences.sandbox, true);
    assert.equal(portal.options.webPreferences.nodeIntegration, false);
    assert.equal(portal.options.webPreferences.additionalArguments.at(-1), '--morde-preload-mode=portal');
    assert.match(portal.options.webPreferences.preload, /morde-preload\.js$/);

    portal.webContents.url = appURL(h, '/hosts#host-h1234567');
    await assert.rejects(h.invoke('morde:portal-legacy-state', portal.webContents), /Invalid Portal window or route/);
    await assert.rejects(h.invoke('morde:portal-state', portal.webContents), /Invalid Portal window or route/);
    portal.webContents.url = appURL(h, '/hosts');
    await assert.rejects(h.invoke('morde:portal-project-upload-targets', portal.webContents), /Invalid Portal window or route/);
    portal.webContents.url = `${new URL(h.views[0].webContents.url).origin}/hosts?sync=1&next=/assistant`;
    await assert.rejects(h.invoke('morde:portal-state', portal.webContents), /Invalid Portal window or route/);
    portal.webContents.url = appURL(h, '/hosts/sync?host=h1234567');
    const state = await h.invoke('morde:portal-state', portal.webContents);
    assertPortalProjectState(state);
    assert.deepEqual(state, { projectMappings: [], syncState: 'idle', syncCheckpoint: null, syncRecovery: null });
    portal.webContents.url = appURL(h, '/hosts?sync=1&host=h1234567');
    assertPortalProjectState(await h.invoke('morde:portal-state', portal.webContents));
    const mainState = await h.invoke('morde:state', h.windows[0].webContents);
    assert.equal(mainState.authenticated, true);
    assert.equal(mainState.accountName, auth.accountName);
    assert.equal(mainState.origin, auth.origin);
    await assert.rejects(h.invoke('morde:portal-state', h.windows[0].webContents), /Invalid Portal window or route/);
    await assert.rejects(h.invoke('morde:portal-state', portal.webContents, [], {}), /Invalid Portal window or route/);
    portal.webContents.url = `${new URL(h.views[0].webContents.url).origin}/assistant`;
    await assert.rejects(h.invoke('morde:portal-state', portal.webContents), /Invalid Portal window or route/);
    portal.webContents.url = appURL(h, '/workspaces?host=h1234567');
    await assert.rejects(h.invoke('morde:portal-legacy-state', portal.webContents), /Invalid Portal window or route/);
    portal.webContents.url = appURL(h, '/hosts/sync?host=h1234567');

    const legacy = await h.invoke('morde:portal-legacy-state', portal.webContents);
    assert.deepEqual(legacy, { mappings: [], syncState: 'idle', checkpoint: null, nodeState: 'stopped', safetyCopiesAvailable: true });
    await assert.rejects(h.invoke('morde:portal-legacy-stop', portal.webContents, ['w1234567']), /当前没有可停止的旧版同步/);
    await assert.rejects(h.invoke('morde:portal-legacy-resume', portal.webContents, ['w1234567']), /旧版同步恢复点已变化/);
    await assert.rejects(h.invoke('morde:portal-preserved-restore', portal.webContents, ['w1234567', '../bad']), /Invalid safety-copy ID/);
    h.dialog.showMessageBox = async () => ({ response: 0 });
    assert.deepEqual(await h.invoke('morde:portal-preserved-restore', portal.webContents, ['w1234567', 'a'.repeat(32)]), { cancelled: true });

    const targets = await h.invoke('morde:portal-project-upload-targets', portal.webContents);
    assert.deepEqual(targets.map(({ hostId, workspaceId }) => ({ hostId, workspaceId })), [{ hostId: 'h1234567', workspaceId: 'w1234567' }]);
    assert.equal(calls.every((call) => new URL(call.url).origin === auth.origin), true, 'all network calls are replaced with this test’s local fetch stub');

    h.dialog.showOpenDialog = async () => ({ canceled: false, filePaths: [selectedDirectory] });
    portal.webContents.url = appURL(h, '/hosts/sync?host=h1234567');
    const selection = await h.invoke('morde:portal-project-select-directory', portal.webContents, ['h1234567', 'w1234567']);
    assert.deepEqual(selection, {
      selectionID: selection.selectionID, hostId: 'h1234567', workspaceId: 'w1234567',
      projectName: 'empty-project', directoryName: 'empty-project',
    });
    assert.equal(JSON.stringify(selection).includes(selectedDirectory), false);
    const pushedState = portal.webContents.messages.filter(([channel]) => channel === 'morde:portal-state').at(-1)?.[1];
    assertPortalProjectState(pushedState);
    assert.equal(pushedState.projectMappings.length, 1);
    assert.deepEqual(pushedState.projectMappings[0], {
      selectionID: selection.selectionID, hostId: 'h1234567', workspaceId: 'w1234567',
      projectName: 'empty-project', directoryName: 'empty-project',
    });
    assert.equal(JSON.stringify(pushedState).includes(selectedDirectory), false);
    await assert.rejects(
      h.invoke('morde:portal-project-upload', portal.webContents, ['00000000-0000-4000-8000-000000000000', 'empty-project']),
      /先选择本机项目文件夹/,
    );
    await assert.rejects(h.invoke('morde:portal-project-stop', portal.webContents, [selection.selectionID]), /当前没有可停止的项目同步/);
    portal.webContents.url = `${new URL(h.views[0].webContents.url).origin}/assistant`;
    await assert.rejects(h.invoke('morde:portal-project-stop', portal.webContents, [selection.selectionID]), /Invalid Portal window or route/);
    assert.equal(calls.some((call) => call.method === 'POST' && new URL(call.url).pathname.includes('/sync/')), false);
  } finally {
    globalThis.fetch = originalFetch;
    h.cleanup();
    rmSync(selectedRoot, { recursive: true, force: true });
  }
});

test('cancelling the native project upload confirmation never registers or starts a sync session', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  mockFetch(calls);
  const h = harness();
  const selectedRoot = mkdtempSync(join(process.cwd(), '.morde-u13-cancel-project-'));
  const selectedDirectory = join(selectedRoot, 'empty-project');
  mkdirSync(selectedDirectory);
  try {
    await h.ready();
    const portal = h.views[0];
    portal.webContents.url = appURL(h, '/hosts/sync?host=h1234567');
    h.dialog.showOpenDialog = async () => ({ canceled: false, filePaths: [selectedDirectory] });
    const selection = await h.invoke('morde:portal-project-select-directory', portal.webContents, ['h1234567', 'w1234567']);

    let confirmation;
    h.dialog.showMessageBox = async (_window, options) => { confirmation = options; return { response: 1 }; };
    const result = await h.invoke('morde:portal-project-upload', portal.webContents, [selection.selectionID, 'empty-project']);

    assert.deepEqual(result, { cancelled: true });
    assert.equal(confirmation.title, '单向上传项目文件夹');
    assert.deepEqual(confirmation.buttons, ['上传项目', '取消']);
    assert.match(confirmation.message, /Local test computer.*empty-project/);
    assert.match(confirmation.detail, /目标路径：~\/workspace\/project\/empty-project/);
    assert.match(confirmation.detail, /云端变化不会下载到本机/);
    assert.equal(calls.some((call) => call.method === 'POST' && new URL(call.url).pathname.endsWith('/sync-folders')), false);
    assert.equal(calls.some((call) => call.method === 'POST' && new URL(call.url).pathname.includes('/sync-sessions')), false);
    const state = await h.invoke('morde:portal-state', portal.webContents);
    assert.equal(state.projectMappings.length, 1, 'cancellation keeps the local selection for a later explicit attempt');
    assert.equal(state.syncCheckpoint, null, 'cancellation does not create a recovery point');
  } finally {
    globalThis.fetch = originalFetch;
    h.cleanup();
    rmSync(selectedRoot, { recursive: true, force: true });
  }
});

test('legacy checkpoint recovery requires confirmation and the same trusted page afterward', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  mockFetch(calls);
  const h = harness({ checkpoint: { serviceInstanceID, installationId: auth.installationId, workspaceId: 'w1234567',
    directory: '/tmp/synthetic-project', dev: '1', ino: '1', mode: 'upload', scope: ['notes'], idempotencyKey: 'synthetic-checkpoint' } });
  try {
    await h.ready();
    const portal = h.views[0].webContents;
    portal.url = appURL(h, '/hosts/sync');
    assert.deepEqual((await h.invoke('morde:portal-legacy-state', portal)).checkpoint,
      { workspaceId: 'w1234567', mode: 'upload', scope: ['notes'] });
    h.dialog.showMessageBox = async () => ({ response: 0 });
    assert.deepEqual(await h.invoke('morde:portal-legacy-resume', portal, ['w1234567']), { cancelled: true });
    h.dialog.showMessageBox = async () => { portal.url = `${new URL(h.views[0].webContents.url).origin}/assistant`; return { response: 1 }; };
    await assert.rejects(h.invoke('morde:portal-legacy-resume', portal, ['w1234567']), /账号、页面或同步恢复点已变化/);
    assert.equal(calls.some((call) => call.method === 'POST' && new URL(call.url).pathname.includes('/sync/')), false);
  } finally {
    globalThis.fetch = originalFetch;
    h.cleanup();
  }
});

test('Portal project upload rejects a replaced empty directory before registering or uploading it', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  mockFetch(calls);
  const h = harness();
  const selectedRoot = mkdtempSync(join(process.cwd(), '.morde-p11-replaced-project-'));
  const selectedDirectory = join(selectedRoot, 'empty-project');
  const movedDirectory = join(selectedRoot, 'original-empty-project');
  mkdirSync(selectedDirectory);
  try {
    await h.ready();
    const portal = h.views[0];
    portal.webContents.url = appURL(h, '/hosts/sync?host=h1234567');
    h.dialog.showOpenDialog = async () => ({ canceled: false, filePaths: [selectedDirectory] });
    const selection = await h.invoke('morde:portal-project-select-directory', portal.webContents, ['h1234567', 'w1234567']);
    assert.equal(selection.projectName, 'empty-project');

    renameSync(selectedDirectory, movedDirectory);
    mkdirSync(selectedDirectory);
    h.dialog.showMessageBox = async () => ({ response: 0 });

    await assert.rejects(
      h.invoke('morde:portal-project-upload', portal.webContents, [selection.selectionID, 'empty-project']),
      /项目文件夹身份已变化，请重新选择/,
    );
    assert.equal(calls.some((call) => call.method === 'POST' && new URL(call.url).pathname.endsWith('/sync-folders')), false,
      'a changed local directory must be rejected before registering a remote folder');
    assert.equal(calls.some((call) => call.method === 'POST' && new URL(call.url).pathname.includes('/sync-sessions')), false,
      'a changed local directory must never start an upload session');
  } finally {
    globalThis.fetch = originalFetch;
    h.cleanup();
    rmSync(selectedRoot, { recursive: true, force: true });
  }
});

test('returnToPortalHosts is limited to the verified Host Workbench top frame', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  mockFetch(calls);
  const h = harness();
  try {
    await h.ready();
    const portal = h.views[0];
    const returnHandler = h.handlers.get('morde:return-to-portal-hosts');
    await assert.rejects(returnHandler({ sender: h.windows[0].webContents, senderFrame: h.windows[0].webContents.mainFrame }), /Invalid Host Workbench window/);
    await assert.rejects(returnHandler({ sender: portal.webContents, senderFrame: portal.webContents.mainFrame }), /Invalid Host Workbench window/);

    const popup = portal.webContents.open({ url: appURL(h, '/host-entry/h1234567') });
    assert.equal(popup.action, 'allow');
    const hostContents = new portal.webContents.constructor();
    popup.createWindow({ webContents: hostContents, webPreferences: { additionalArguments: ['--existing-arg'] } });
    const hostWindow = h.windows.at(-1);
    Object.defineProperty(hostWindow, 'webContents', {
      configurable: true,
      get() {
        if (hostWindow.destroyed) throw new Error('Object has been destroyed');
        return hostContents;
      },
    });
    assert.equal(hostWindow.options.webPreferences.sandbox, true);
    assert.equal(hostWindow.options.webPreferences.nodeIntegration, false);
    assert.match(hostWindow.options.webPreferences.preload, /morde-preload\.js$/);
    assert.deepEqual(hostWindow.options.webPreferences.additionalArguments, ['--existing-arg', '--morde-preload-mode=host-workbench']);

    const navigate = hostContents.events.get('will-navigate');
    let prevented = false;
    navigate({ preventDefault() { prevented = true; } }, 'https://host.example/__ch/auth?t=local-test-ticket');
    await new Promise((resolve) => setImmediate(resolve));
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(hostContents.url, 'https://host.example/__ch/auth?t=local-test-ticket');
    prevented = false;
    navigate({ preventDefault() { prevented = true; } }, 'https://host.example/__ch/wb/?tab=workspace');
    assert.equal(prevented, false, 'the gateway can open its standalone Workbench route');
    hostContents.url = 'https://host.example/__ch/wb/?tab=workspace';

    await assert.rejects(returnHandler({ sender: hostContents, senderFrame: {} }), /Invalid Host Workbench window/);
    hostContents.url = 'https://host.example/__ch/wb/?task_id=q1234567';
    await assert.rejects(returnHandler({ sender: hostContents, senderFrame: hostContents.mainFrame }), /not on a trusted page/);
    hostContents.url = 'https://evil.example/__ch/wb/';
    await assert.rejects(returnHandler({ sender: hostContents, senderFrame: hostContents.mainFrame }), /not on a trusted page/);

    hostContents.url = 'https://host.example/__ch/wb/';
    const mainWindow = h.windows[0];
    for (const [index, event] of ['resize', 'maximize', 'unmaximize', 'restore', 'enter-full-screen', 'leave-full-screen'].entries()) {
      const bounds = { x: 0, y: 0, width: 1440 + index * 10, height: 900 + index * 10 };
      mainWindow.contentBounds = bounds;
      mainWindow.events.get(event)();
      assert.deepEqual(portal.bounds, bounds, `WebContentsView must follow ${event}`);
    }
    assert.equal(mainWindow.options.frame, true, 'the desktop window uses native OS caption controls');

    const restoredBounds = { x: 0, y: 0, width: 1680, height: 1050 };
    mainWindow.contentBounds = restoredBounds;
    mainWindow.minimized = true;
    mainWindow.shown = false;
    assert.deepEqual(await returnHandler({ sender: hostContents, senderFrame: hostContents.mainFrame }), { returned: true });
    assert.equal(hostWindow.isDestroyed(), true);
    assert.deepEqual(portal.bounds, restoredBounds, 'returning to an already attached view refreshes its bounds');
    assert.equal(mainWindow.restored, true, 'returning from a Host Workbench restores a minimized desktop window');
    assert.equal(mainWindow.shown, true, 'returning from a Host Workbench shows a hidden desktop window');
    assert.equal(mainWindow.focused, true);
    assert.equal(mainWindow.contentView.attached.includes(portal), true);
    await assert.rejects(returnHandler({ sender: hostContents, senderFrame: hostContents.mainFrame }), /Invalid Host Workbench window/);
  } finally {
    globalThis.fetch = originalFetch;
    h.cleanup();
  }
});

test('task observation windows do not inherit a desktop preload or bridge mode', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  mockFetch(calls);
  const h = harness();
  try {
    await h.ready();
    const popup = h.views[0].webContents.open({ url: 'about:blank', frameName: 'morde-task-observation' });
    assert.equal(popup.action, 'allow');
    popup.createWindow({ webPreferences: { additionalArguments: ['--caller-arg'] } });
    const observationWindow = h.windows.at(-1);
    assert.equal(observationWindow.options.webPreferences.preload, undefined);
    assert.deepEqual(observationWindow.options.webPreferences.additionalArguments, []);
  } finally {
    globalThis.fetch = originalFetch;
    h.cleanup();
  }
});

test('MCP OAuth windows use a dedicated HTTPS-only popup guard', async () => {
  const originalFetch = globalThis.fetch;
  mockFetch([]);
  const h = harness();
  try {
    await h.ready();
    const portal = h.views[0].webContents;
    assert.equal(portal.open({ url: 'about:blank', frameName: '_blank' }).action, 'deny');

    const popup = portal.open({ url: 'about:blank', frameName: 'morde-mcp-oauth' });
    assert.equal(popup.action, 'allow');
    const oauthContents = popup.createWindow({ webPreferences: { preload: 'untrusted.js', additionalArguments: ['--untrusted'] } });
    const oauthWindow = h.windows.at(-1);
    assert.equal(oauthWindow.options.title, 'morde-pc · OAuth');
    assert.equal(oauthWindow.options.webPreferences.preload, undefined);
    assert.deepEqual(oauthWindow.options.webPreferences.additionalArguments, []);
    assert.equal(oauthWindow.options.webPreferences.session, h.partitions[0]);

    let prevented = false;
    const navigate = oauthContents.events.get('will-navigate');
    navigate({ preventDefault() { prevented = true; } }, 'https://oauth.example.test/authorize');
    assert.equal(prevented, false);
    prevented = false;
    navigate({ preventDefault() { prevented = true; } }, 'http://oauth.example.test/authorize');
    assert.equal(prevented, true);

    const redirect = oauthContents.events.get('will-redirect');
    prevented = false;
    redirect({ preventDefault() { prevented = true; } }, 'https://portal.example/api/mcp/oauth/callback');
    assert.equal(prevented, false);
    prevented = false;
    redirect({ preventDefault() { prevented = true; } }, 'about:blank');
    assert.equal(prevented, true);
    assert.deepEqual(oauthContents.open({ url: 'https://evil.example.test' }), { action: 'deny' });
  } finally {
    globalThis.fetch = originalFetch;
    h.cleanup();
  }
});

test('native Workbench exchange keeps the ticket and strict cookie in an isolated same-window view', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  const nonce = 'B'.repeat(32);
  const ticket = 'A'.repeat(43);
  const exchangeTarget = `https://host.example/__ch/wb/?embed=1&embed_nonce=${nonce}&tab=browser`;
  globalThis.fetch = async (input, options = {}) => {
    const url = new URL(String(input));
    calls.push({ url, options });
    if (url.pathname === '/api/auth/client-web-session') return json({ web_session_credential: 'local-web-session', web_session_expires_at: future });
    if (url.pathname === '/api/hosts/h1234567') return json(host);
    if (url.pathname === '/api/hosts/h1234567/embed-tickets') return json({ exchange_url: 'https://host.example/__ch/embed-auth',
      ticket, embed_nonce: nonce, targets: ['browser', 'terminal'], expires_at: future }, 201);
    if (url.pathname === '/__ch/embed-auth') return new Response(null, { status: 303,
      headers: { location: exchangeTarget, 'set-cookie': 'ch_embed=opaque.signed; Path=/; Max-Age=600; HttpOnly; Secure; SameSite=Strict' } });
    return json({ error: { code: 'not_found', message: 'local mock route missing' } }, 404);
  };
  const h = harness();
  try {
    await h.ready();
    const portal = h.views[0].webContents;
    portal.url = appURL(h, '/assistant');
    const opened = await h.invoke('morde:portal-workbench-open', portal, [{ hostID: 'h1234567', conversationID: 'c1234567',
      initialTarget: 'browser', targets: ['browser', 'terminal'], shared: false }]);
    assert.deepEqual(opened, { hostID: 'h1234567', hostOrigin: 'https://host.example', conversationID: 'c1234567', nonce,
      targets: ['browser', 'terminal'] });
    assert.equal(JSON.stringify(opened).includes(ticket), false, 'the one-time ticket never returns to Portal JavaScript');
    assert.equal(calls.find((call) => call.url.pathname === '/api/hosts/h1234567/embed-tickets').options.headers.cookie, 'ch_portal=local-web-session');
    const exchange = calls.find((call) => call.url.pathname === '/__ch/embed-auth');
    assert.equal(exchange.options.headers.origin, auth.origin);
    assert.equal(exchange.options.headers.authorization, undefined);
    const workbench = h.views[1];
    const partition = h.partitions.at(-1);
    assert.equal(workbench.options.webPreferences.session, partition);
    assert.match(partition.name, /^morde-workbench-/);
    assert.equal(partition.name.startsWith('persist:'), false);
    assert.deepEqual(partition.cookies.values[0], { url: 'https://host.example', name: 'ch_embed', value: 'opaque.signed', path: '/',
      httpOnly: true, secure: true, sameSite: 'strict', expirationDate: partition.cookies.values[0].expirationDate });
    assert.equal(workbench.webContents.url, exchangeTarget);
    assert.equal(h.windows[0].contentView.attached.includes(workbench), true);
    assert.deepEqual(workbench.options.webPreferences.additionalArguments, ['--morde-preload-mode=host-workbench']);

    await assert.rejects(h.invoke('morde:portal-workbench-bounds', portal, ['wrong-nonce', { x: 0, y: 0, width: 100, height: 100 }]), /Invalid Host Workbench bounds request/);
    assert.deepEqual(await h.invoke('morde:portal-workbench-bounds', portal, [nonce, { x: 12, y: 20, width: 700, height: 500 }]), { updated: true });
    assert.deepEqual(workbench.bounds, { x: 12, y: 20, width: 700, height: 500 });
    assert.deepEqual(await h.invoke('morde:portal-workbench-command', portal, [nonce,
      { type: 'init', conversation_id: 'c1234567', visible: true }]), { sent: true });
    const ready = await h.invoke('morde:host-workbench-ready', workbench.webContents, [nonce]);
    assert.deepEqual(ready, { ready: true });
    assert.equal(workbench.webContents.messages.some(([channel, message]) => channel === 'morde:host-workbench-message' && message.type === 'init'), true);
    assert.equal(portal.messages.some(([channel, message]) => channel === 'morde:portal-workbench-message' && message.type === 'ready'), true);
    await h.invoke('morde:host-workbench-state', workbench.webContents, [{ nonce, type: 'state', conversation_id: 'c1234567', host_id: 'h1234567',
      workbench_ready: true, remote_desktop: 'connected', control_state: 'ai', control_lease_active: false, control_lease_held_by_me: false }]);
    assert.equal(portal.messages.some(([channel, message]) => channel === 'morde:portal-workbench-message' && message.type === 'state' && message.host_id === 'h1234567'), true);

    assert.deepEqual(await h.invoke('morde:portal-workbench-close', portal, [nonce]), { closed: true });
    assert.equal(h.windows[0].contentView.attached.includes(workbench), false);
    assert.equal(workbench.webContents.isDestroyed(), true);
    assert.equal(partition.cleared, true);
  } finally {
    globalThis.fetch = originalFetch;
    h.cleanup();
  }
});

test('native Workbench setup failure removes its cookie and closes the partial view', async () => {
  const originalFetch = globalThis.fetch;
  const nonce = 'B'.repeat(32);
  const ticket = 'A'.repeat(43);
  const exchangeTarget = `https://host.example/__ch/wb/?embed=1&embed_nonce=${nonce}&tab=browser`;
  globalThis.fetch = async (input) => {
    const url = new URL(String(input));
    if (url.pathname === '/api/auth/client-web-session') return json({ web_session_credential: 'local-web-session', web_session_expires_at: future });
    if (url.pathname === '/api/hosts/h1234567') return json(host);
    if (url.pathname === '/api/hosts/h1234567/embed-tickets') return json({ exchange_url: 'https://host.example/__ch/embed-auth',
      ticket, embed_nonce: nonce, targets: ['browser'], expires_at: future }, 201);
    if (url.pathname === '/__ch/embed-auth') return new Response(null, { status: 303,
      headers: { location: exchangeTarget, 'set-cookie': 'ch_embed=opaque.signed; Path=/; Max-Age=600; HttpOnly; Secure; SameSite=Strict' } });
    return json({ error: { code: 'not_found', message: 'local mock route missing' } }, 404);
  };
  const h = harness();
  try {
    await h.ready();
    const portal = h.views[0].webContents;
    portal.url = appURL(h, '/assistant');
    h.windows[0].contentView.addChildView = () => { throw new Error('view attachment failed'); };

    await assert.rejects(h.invoke('morde:portal-workbench-open', portal, [{ hostID: 'h1234567', conversationID: 'c1234567',
      initialTarget: 'browser', targets: ['browser'], shared: false }]), /view attachment failed/);

    const workbench = h.views[1];
    const partition = h.partitions.at(-1);
    assert.equal(partition.clearCount, 2, 'the isolated session is cleared again after setup fails');
    assert.deepEqual(partition.cookies.values, []);
    assert.equal(workbench.webContents.isDestroyed(), true);
  } finally {
    globalThis.fetch = originalFetch;
    h.cleanup();
  }
});
